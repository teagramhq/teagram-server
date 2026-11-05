#!/usr/bin/env python3
"""Fake incus and tailscale for test-bootstrap-alpha.sh.

File operations inside the fake LXC and source roots run as real commands on
rebased paths, so the bootstrap's permission and comparison checks are real.
Host, LXC service and cluster state is kept in state.json. Each mutating
action is appended to MOCK_ACTIONS so the test can assert a rerun changes
nothing.
"""
import base64
import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys

STATE_DIR = os.environ["MOCK_STATE"]
SOURCE_ROOT = os.environ["MOCK_SRC"]
ACTIONS = os.environ["MOCK_ACTIONS"]
LXC_ROOT = os.path.join(STATE_DIR, "lxc")
STATE_FILE = os.path.join(STATE_DIR, "state.json")

SOURCE = "teagram"
INSTANCE = "teagram-k3s"
NAMESPACE = "teagram"
SOURCE_CONTAINERS = [
    "teagram-postgres-1",
    "teagram-teagramd-1",
    "teagram-rustfs-1",
    "teagram-link-edge-selector-1",
    "teagram-web-cloudflared",
]
TOKEN = "tgtest-deployer-token-7f3a9c"
CA_PEM = "-----BEGIN CERTIFICATE-----\nMIIBtgtestonly\n-----END CERTIFICATE-----\n"
TAILNET_IP = "100.100.1.2"
K3S_VERSION = "v1.37.1+k3s1"
PATH_PREFIXES = ("/root/", "/etc/", "/usr/", "/run/", "/var/", "/opt/")
CA_JSONPATH = "{.data.ca\\.crt}"
TOKEN_JSONPATH = "{.data.token}"


def load_state():
    try:
        with open(STATE_FILE) as handle:
            return json.load(handle)
    except FileNotFoundError:
        return {
            "instance": None,
            "k3s_installed": False,
            "k3s_running": False,
            "ssh_enabled": False,
            "ssh_active": False,
            "serve": {"TCP": {}},
            "objects": {},
            "resource_data": {},
            "token_populated": False,
            "ca_populated": False,
            "token_wait_polls": 0,
            "ca_wait_polls": 0,
            "pod_security_enforce": "",
            "pod_security_enforce_version": "",
        }


def save_state(state):
    with open(STATE_FILE, "w") as handle:
        json.dump(state, handle)


def log_action(action):
    with open(ACTIONS, "a") as handle:
        handle.write(action + "\n")


def exit_saved(state, code):
    save_state(state)
    sys.exit(code)


def fail(message, code=1):
    print(message, file=sys.stderr)
    sys.exit(code)


def assert_no_host_secret_temp_files():
    directory = pathlib.Path(os.environ["MOCK_HOST_TMP"])
    values = [value.encode() for value in os.environ["MOCK_SECRET_VALUES"].splitlines() if value]
    markers = values + [base64.b64encode(value) for value in values]
    for path in directory.iterdir():
        if path.is_file():
            contents = path.read_bytes()
            if any(marker in contents for marker in markers):
                fail("host temporary file contains a fixture credential")


def rebase(root, args):
    return [root + arg if arg.startswith(PATH_PREFIXES) else arg for arg in args]


def run_rebased(root, args):
    os.makedirs(root, exist_ok=True)
    sys.stdout.flush()
    sys.exit(subprocess.run(rebase(root, args)).returncode)


def docker(args):
    if args[0] == "inspect":
        if "--format" not in args:
            missing = [name for name in args[1:]
                       if not os.path.isdir(os.path.join(SOURCE_ROOT, "containers", name))]
            sys.exit(1 if missing else 0)
        if "Cmd" in args[-2]:
            fail("cmd-based token lookup is not expected in this fixture")
        env_file = os.path.join(SOURCE_ROOT, "containers", args[-1], "env")
        if os.path.exists(env_file):
            with open(env_file) as handle:
                sys.stdout.write(handle.read())
        sys.exit(0)
    if args[0] == "cp":
        container, path = args[1].split(":", 1)
        relative = path.lstrip("/")
        base = os.path.join(SOURCE_ROOT, "containers", container)
        if not os.path.exists(os.path.join(base, relative)):
            fail(f"no such path in {container}: {path}")
        sys.stdout.flush()
        sys.exit(subprocess.run(["tar", "-C", base, "-cf", "-", relative]).returncode)
    fail(f"unexpected docker command: {args}")


def install_k3s(state):
    unit = os.path.join(LXC_ROOT, "etc/systemd/system/k3s.service")
    os.makedirs(os.path.dirname(unit), exist_ok=True)
    with open(unit, "w") as handle:
        handle.write(
            "ExecStart=/usr/local/bin/k3s server --disable=traefik --disable=servicelb "
            "--disable=metrics-server --tls-san=" + TAILNET_IP +
            " --write-kubeconfig-mode=0600\n")
    state["k3s_installed"] = True
    log_action("k3s-install")
    exit_saved(state, 0)


def store_manifest(text, state):
    if text.lstrip().startswith("{"):
        obj = json.loads(text)
        documents = [(obj["kind"], obj["metadata"]["name"], json.dumps(obj, sort_keys=True))]
        resource_data = obj.get("data", {})
    else:
        documents = []
        resource_data = None
        for part in re.split(r"^---$", text, flags=re.M):
            kind = re.search(r"^kind: (\S+)$", part, re.M)
            name = re.search(r"^  name: (\S+)$", part, re.M)
            if kind and name:
                documents.append((kind.group(1), name.group(1), part.strip()))
    for kind, name, document in documents:
        key = kind.lower() + "/" + name
        if os.environ.get("MOCK_FAIL_APPLY_RESOURCE") == key:
            fail("mock kubectl first diagnostic\nmock kubectl second diagnostic", 1)
        digest = hashlib.sha256(document.encode()).hexdigest()
        if state["objects"].get(key) != digest:
            state["objects"][key] = digest
            log_action("kubectl-apply " + key)
        if resource_data is not None and kind.lower() in ("configmap", "secret"):
            state.setdefault("resource_data", {})[key] = resource_data
        if key == "namespace/teagram":
            label = re.search(
                r"^\s+pod-security\.kubernetes\.io/enforce:\s*(\S+)\s*$",
                document,
                re.M,
            )
            state["pod_security_enforce"] = label.group(1) if label else ""
            version = re.search(
                r"^\s+pod-security\.kubernetes\.io/enforce-version:\s*(\S+)\s*$",
                document,
                re.M,
            )
            state["pod_security_enforce_version"] = version.group(1) if version else ""


def create_from_env(args, text):
    if len(args) < 3:
        fail("kubectl create requires a resource type and name")
    if args[1] == "secret":
        if len(args) < 4 or args[2] != "generic":
            fail('kubectl create secret requires the "generic" subcommand')
        kind, name = "secret", args[3]
    elif args[1] == "configmap":
        kind, name = "configmap", args[2]
    else:
        fail(f"unexpected kubectl create resource: {args[1]}")
    if os.environ.get("MOCK_FAIL_CREATE_RESOURCE") == kind + "/" + name:
        fail("mock kubectl create first diagnostic\nmock kubectl create second diagnostic", 1)
    values = {}
    for line in text.splitlines():
        if "=" in line:
            key, value = line.split("=", 1)
            values[key] = value
    if kind == "secret":
        obj = {"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
               "metadata": {"name": name, "namespace": NAMESPACE},
               "data": {k: base64.b64encode(v.encode()).decode() for k, v in values.items()}}
    else:
        obj = {"apiVersion": "v1", "kind": "ConfigMap",
               "metadata": {"name": name, "namespace": NAMESPACE}, "data": values}
    return obj


def kubectl(args, state):
    if args[0].startswith("--as="):
        args = args[1:]
    if args[0] == "apply":
        log_action("kubectl-apply-attempt")
        manifest = sys.stdin.read()
        if not manifest.strip():
            fail("no objects passed to apply")
        assert_no_host_secret_temp_files()
        store_manifest(manifest, state)
        exit_saved(state, 0)
    if args[0] == "create" and "--dry-run=server" in args:
        pod = sys.stdin.read()
        if (state["pod_security_enforce"] == "baseline" and
                "privileged: true" in pod and "hostPath:" in pod):
            log_action("podsecurity-deny")
            fail(
                'Error from server (Forbidden): pods "teagram-deployer-escape-check" '
                'violates PodSecurity "baseline:v1.37": privileged container, hostPath volumes',
                1,
            )
        print("pod/teagram-deployer-escape-check created (server dry run)")
        sys.exit(0)
    if args[0] == "create":
        print(json.dumps(create_from_env(args, sys.stdin.read())))
        sys.exit(0)
    if args[:2] == ["auth", "can-i"]:
        verb, resource = args[2:4]
        namespace = args[args.index("--namespace") + 1] if "--namespace" in args else ""
        group = ""
        if "." in resource:
            resource, group = resource.split(".", 1)
        subresource = (
            args[args.index("--subresource") + 1] if "--subresource" in args else ""
        )
        if resource == "namespaces":
            print(
                "Warning: resource 'namespaces' is not namespace scoped",
                file=sys.stderr,
            )
        allowed = False
        if namespace == NAMESPACE:
            if group == "apps" and resource in ("deployments", "statefulsets"):
                allowed = verb in {"create", "get", "list", "patch", "watch"}
            elif group == "apps" and resource in ("replicasets", "controllerrevisions"):
                allowed = verb in {"get", "list"}
            elif group == "batch" and resource == "jobs":
                allowed = verb in {"create", "get", "list", "patch", "watch"}
            elif group == "" and resource in ("configmaps", "services"):
                allowed = verb in {"create", "get", "list", "patch", "watch"}
            elif group == "" and resource in ("persistentvolumeclaims", "pods", "events"):
                allowed = verb in {"get", "list", "watch"}
            elif group == "" and resource == "pods" and subresource == "log":
                allowed = verb == "get"
        answer = "yes" if allowed else "no"
        print(answer)
        sys.exit(0 if answer == "yes" else 1)
    if args[0] == "get" and args[1] == "namespace":
        print(json.dumps({
            "apiVersion": "v1",
            "kind": "Namespace",
            "metadata": {"name": NAMESPACE, "labels": {
                "pod-security.kubernetes.io/enforce": state["pod_security_enforce"],
                "pod-security.kubernetes.io/enforce-version": state["pod_security_enforce_version"],
            }},
        }))
        sys.exit(0)
    if args[0] == "get" and args[1] == "nodes":
        print("teagram-k3s Ready control-plane 1m v1.37.1+k3s1")
        sys.exit(0)
    if args[0] == "get" and args[1] == "pods":
        print("coredns-0 1/1 Running 0 1m")
        sys.exit(0)
    if args[0] == "get" and args[1] == "secret":
        name = args[2]
        if "secret/" + name not in state["objects"]:
            fail(f'secrets "{name}" not found', 1)
        data = {}
        if name == "teagram-deployer-token":
            if state["token_populated"]:
                data["token"] = base64.b64encode(TOKEN.encode()).decode()
            if state["ca_populated"]:
                data["ca.crt"] = base64.b64encode(CA_PEM.encode()).decode()
        print(json.dumps({"apiVersion": "v1", "kind": "Secret",
                          "metadata": {"name": name}, "data": data}))
        sys.exit(0)
    if args[0] == "wait":
        target = "secret/teagram-deployer-token"
        condition = next(arg for arg in args if arg.startswith("--for="))[len("--for="):]
        if condition == "create":
            sys.exit(0 if target in state["objects"] else 1)
        if condition.startswith("jsonpath="):
            jsonpath = condition[len("jsonpath="):]
            # The token controller populates each key only after several wait polls.
            if jsonpath == TOKEN_JSONPATH:
                for _ in range(3):
                    state["token_wait_polls"] += 1
                state["token_populated"] = True
            elif jsonpath == CA_JSONPATH:
                for _ in range(3):
                    state["ca_wait_polls"] += 1
                state["ca_populated"] = True
            else:
                fail(f"unexpected jsonpath wait: {jsonpath}")
            exit_saved(state, 0 if target in state["objects"] else 1)
        sys.exit(0)
    fail(f"unexpected kubectl command: {args}")


def k3s(args):
    state = load_state()
    if args[0] == "kubectl":
        kubectl(args[1:], state)
    fail(f"unexpected k3s command: {args}")


def lxc(cmd):
    state = load_state()
    if cmd[0] == "bash" and cmd[1:2] == ["-s"]:
        sys.stdin.read()
        sys.exit(0)
    if (cmd[0] == "bash" and cmd[1:2] == ["-c"] and
            'manifest=$(k3s kubectl "$@")' in cmd[2]):
        sys.stdout.flush()
        sys.stderr.flush()
        sys.exit(subprocess.run(["bash", "-c", *cmd[2:]]).returncode)
    if cmd[0] == "bash" and cmd[1:2] == ["-c"] and "command -v k3s" in cmd[2]:
        sys.exit(0 if state["k3s_installed"] else 1)
    if cmd[0] == "bash" and cmd[1:2] == ["-c"] and "install.sh" in cmd[2]:
        install_k3s(state)
    if cmd[0] == "command":
        fail("command -v must be invoked through a shell")
    if cmd[0] == "k3s" and cmd[1] == "--version":
        log_action("k3s-version-check")
        print(f"k3s version {K3S_VERSION} (test)")
        sys.exit(0)
    if cmd[0] == "k3s" and cmd[1] == "kubectl":
        kubectl(cmd[2:], state)
    if cmd[0] == "systemctl":
        action = cmd[1:]
        if action == ["enable", "--now", "k3s"]:
            if not state["k3s_running"]:
                log_action("systemctl enable --now k3s")
                state["k3s_running"] = True
            exit_saved(state, 0)
        if action == ["enable", "ssh"]:
            log_action("systemctl enable ssh")
            state["ssh_enabled"] = True
            exit_saved(state, 0)
        if action == ["is-enabled", "--quiet", "ssh"]:
            sys.exit(0 if state["ssh_enabled"] else 1)
        if action in (["restart", "ssh"], ["start", "ssh"]):
            log_action("systemctl " + " ".join(action))
            state["ssh_active"] = True
            exit_saved(state, 0)
        if action == ["is-active", "--quiet", "ssh"]:
            sys.exit(0 if state["ssh_active"] else 1)
        fail(f"unexpected systemctl command: {action}")
    if cmd[0] == "sysctl":
        log_action("sysctl")
        sys.exit(0)
    if cmd[0] == "sshd":
        sys.exit(0)
    if cmd[0] == "ss":
        print("State Recv-Q Send-Q Local Address:Port Peer Address:Port")
        print("LISTEN 0 4096 *:6443 *:*")
        sys.exit(0)
    if cmd[0] == "install" and "-d" in cmd:
        log_action("mkdir " + cmd[-1])
    elif cmd[0] == "install" and "/dev/stdin" in cmd:
        log_action("file-write " + cmd[-1])
    run_rebased(LXC_ROOT, cmd)


def incus_exec(args):
    args = [arg for arg in args if not arg.startswith("--mode")]
    separator = args.index("--")
    name = args[separator - 1]
    cmd = args[separator + 1:]
    if name == SOURCE:
        if cmd[0] == "docker":
            docker(cmd[1:])
        run_rebased(SOURCE_ROOT, cmd)
    if name == INSTANCE:
        lxc(cmd)
    fail(f"unknown instance: {name}")


def incus(args):
    state = load_state()
    if args[0] == "info":
        exists = args[1] == SOURCE or (args[1] == INSTANCE and state["instance"] is not None)
        sys.exit(0 if exists else 1)
    if args[0] == "init":
        config = {}
        for index, arg in enumerate(args):
            if arg == "--config":
                key, value = args[index + 1].split("=", 1)
                config[key] = value
        state["instance"] = {"config": config, "devices": {}, "state": "STOPPED"}
        log_action("init")
        exit_saved(state, 0)
    if args[0] == "config" and args[1] == "device":
        instance = state["instance"]
        if args[2] == "override":
            instance["devices"].setdefault(args[4], {}).update(dict(a.split("=", 1) for a in args[5:]))
            log_action("device-override " + args[4])
            exit_saved(state, 0)
        if args[2] == "add":
            device = {"type": args[5]}
            device.update(dict(a.split("=", 1) for a in args[6:]))
            instance["devices"][args[4]] = device
            log_action("device-add " + args[4])
            exit_saved(state, 0)
        if args[2] == "get":
            print(instance["devices"].get(args[4], {}).get(args[5], ""))
            sys.exit(0)
        if args[2] == "show":
            for device_name, props in instance["devices"].items():
                print(device_name + ":")
                for key, value in props.items():
                    print(f"  {key}: {value}")
            sys.exit(0)
    if args[0] == "config" and args[1] == "get":
        print(state["instance"]["config"].get(args[3], ""))
        sys.exit(0)
    if args[0] == "list":
        print(state["instance"]["state"])
        sys.exit(0)
    if args[0] == "start":
        state["instance"]["state"] = "RUNNING"
        log_action("start")
        exit_saved(state, 0)
    if args[0] == "exec":
        incus_exec(args[1:])
    fail(f"unexpected incus command: {args}")


def tailscale(args):
    state = load_state()
    if args == ["ip", "-4"]:
        print(TAILNET_IP)
        sys.exit(0)
    if args == ["serve", "status", "--json"]:
        print(json.dumps({"TCP": state["serve"]["TCP"], "AllowFunnel": {}}))
        sys.exit(0)
    if args[0] == "serve" and "--tcp=" in args[-2]:
        port = args[-2].split("=", 1)[1]
        target = args[-1]
        state["serve"]["TCP"][port] = {"TCPForward": target[len("tcp://"):]}
        log_action("serve " + port)
        exit_saved(state, 0)
    fail(f"unexpected tailscale command: {args}")


def main():
    tool, args = sys.argv[1], sys.argv[2:]
    if tool == "incus":
        incus(args)
    if tool == "tailscale":
        tailscale(args)
    if tool == "k3s":
        k3s(args)
    fail(f"unknown mock tool: {tool}")


if __name__ == "__main__":
    main()
