#!/usr/bin/env python3
"""Fake Incus and k3s for test-bootstrap-alpha.sh.

File operations inside the fake k3s LXC root run as real commands on rebased
paths, so permission and comparison checks are real. The deleted application
LXC is unavailable. Host, LXC service and cluster state is kept in state.json.
Each mutating action is appended to MOCK_ACTIONS so the test can assert a
rerun changes nothing.
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
ACTIONS = os.environ["MOCK_ACTIONS"]
LXC_ROOT = os.path.join(STATE_DIR, "lxc")
STATE_FILE = os.path.join(STATE_DIR, "state.json")

INSTANCE = "teagram-k3s"
NAMESPACE = "teagram"
TOKEN = "tgtest-deployer-token-7f3a9c"
CA_PEM = "-----BEGIN CERTIFICATE-----\nMIIBtgtestonly\n-----END CERTIFICATE-----\n"
TAILNET_HOST = "teagram.tailaa4918.ts.net"
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
            "api_tls_san": False,
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


def install_k3s(state, command):
    unit = os.path.join(LXC_ROOT, "etc/systemd/system/k3s.service")
    os.makedirs(os.path.dirname(unit), exist_ok=True)
    match = re.search(r"INSTALL_K3S_EXEC='([^']*)'", command)
    if not match:
        fail("k3s install omitted INSTALL_K3S_EXEC")
    with open(unit, "w") as handle:
        handle.write("ExecStart=/usr/local/bin/k3s " + match.group(1) + "\n")
    state["k3s_installed"] = True
    log_action("k3s-install-skip-download" if "INSTALL_K3S_SKIP_DOWNLOAD=true" in command else "k3s-install")
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


def deployer_role_allows(verb, group, resource):
    """Evaluate the deployer Role rules from deployer-access.yaml like RBAC does."""
    with open(os.environ["MOCK_DEPLOYER_RULES"]) as handle:
        rules = json.load(handle)
    for rule in rules:
        if (verb in rule.get("verbs", []) or "*" in rule.get("verbs", [])) and (
                group in rule.get("apiGroups", []) or "*" in rule.get("apiGroups", [])) and (
                resource in rule.get("resources", []) or "*" in rule.get("resources", [])):
            return True
    return False


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
        env_file = next((arg.split("=", 1)[1] for arg in args
                         if arg.startswith("--from-env-file=")), None)
        if env_file is None:
            text = sys.stdin.read()
        else:
            expected_env_dir = pathlib.Path(LXC_ROOT, "run").resolve()
            if pathlib.Path(env_file).resolve().parent != expected_env_dir:
                fail("temporary env file was created outside the LXC sandbox")
            try:
                with open(env_file) as handle:
                    text = handle.read()
            except OSError:
                fail("kubectl could not read the temporary env file")
        print(json.dumps(create_from_env(args, text)))
        sys.exit(0)
    if args[:2] == ["auth", "can-i"]:
        verb, resource = args[2:4]
        namespace = args[args.index("--namespace") + 1] if "--namespace" in args else ""
        subject = next(
            (arg.split("=", 1)[1] for arg in args if arg.startswith("--as=")), ""
        )
        group = ""
        if "." in resource:
            resource, group = resource.split(".", 1)
        if "--subresource" in args:
            subresource = args[args.index("--subresource") + 1]
        else:
            subresource = next(
                (arg.split("=", 1)[1] for arg in args
                 if arg.startswith("--subresource=")),
                "",
            )
        if resource == "namespaces":
            print(
                "Warning: resource 'namespaces' is not namespace scoped",
                file=sys.stderr,
            )
        allowed = False
        if (namespace == NAMESPACE and
                subject == "system:serviceaccount:teagram:teagram-deployer"):
            allowed = deployer_role_allows(
                verb, group, resource + ("/" + subresource if subresource else ""))
        elif (namespace == NAMESPACE and
              subject == "system:serviceaccount:teagram:teagram-agent"):
            if group == "" and resource == "pods" and subresource == "log":
                allowed = verb in {"get", "list", "watch"}
            elif (group == "" and
                  resource in ("pods", "events", "services", "endpoints") and
                  not subresource):
                allowed = verb in {"get", "list", "watch"}
                if resource == "pods" and verb == "delete":
                    allowed = True
            elif (group == "apps" and
                  resource in ("statefulsets", "deployments", "replicasets")):
                allowed = verb in {"get", "list", "watch"}
            elif group == "apps" and resource == "controllerrevisions":
                allowed = verb in {"get", "list"}
            elif group == "batch" and resource in ("jobs", "cronjobs"):
                allowed = verb in {"get", "list", "watch"}
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
    if any("/dev/stdin" in arg for arg in cmd):
        fail("open /dev/stdin: permission denied in unprivileged LXC")
    if cmd[0] == "bash" and cmd[1:2] == ["-s"]:
        sys.stdin.read()
        sys.exit(0)
    if (cmd[0] == "bash" and cmd[1:2] == ["-c"] and
            "--from-env-file=$env_file" in cmd[2]):
        script = cmd[2].replace(
            "/run/teagram-k3s-env.",
            os.path.join(LXC_ROOT, "run/teagram-k3s-env."),
        )
        sys.stdout.flush()
        sys.stderr.flush()
        sys.exit(subprocess.run(["bash", "-c", script, *cmd[3:]]).returncode)
    if (cmd[0] == "sh" and cmd[1:2] == ["-c"] and
            'temporary=$(mktemp "${destination}.XXXXXX")' in cmd[2]):
        destination, mode = cmd[-2:]
        target = os.path.join(LXC_ROOT, destination.lstrip("/"))
        os.makedirs(os.path.dirname(target), exist_ok=True)
        temporary = target + ".test-transfer"
        with open(temporary, "wb") as handle:
            handle.write(sys.stdin.buffer.read())
        os.chmod(temporary, int(mode, 8))
        os.replace(temporary, target)
        log_action("file-write " + destination)
        sys.exit(0)
    if cmd[0] == "bash" and cmd[1:2] == ["-c"] and "command -v k3s" in cmd[2]:
        sys.exit(0 if state["k3s_installed"] else 1)
    if cmd[0] == "bash" and cmd[1:2] == ["-c"] and "install.sh" in cmd[2]:
        install_k3s(state, cmd[2])
    if cmd[0] == "command":
        fail("command -v must be invoked through a shell")
    if cmd[0] == "k3s" and cmd[1] == "--version":
        log_action("k3s-version-check")
        print(f"k3s version {K3S_VERSION} (test)")
        sys.exit(0)
    if cmd[0] == "k3s" and cmd[1] == "kubectl":
        kubectl(cmd[2:], state)
    if cmd[:3] == ["k3s", "certificate", "rotate"]:
        if cmd[3:] != ["--service", "k3s-server"]:
            fail(f"unexpected k3s certificate rotation: {cmd}")
        unit = pathlib.Path(LXC_ROOT, "etc/systemd/system/k3s.service")
        state["api_tls_san"] = TAILNET_HOST in unit.read_text()
        log_action("certificate-rotate-k3s-server")
        sys.exit(0)
    if cmd[:2] == ["openssl", "s_client"]:
        if ("-servername" not in cmd or
                cmd[cmd.index("-servername") + 1] != TAILNET_HOST or
                "-verify_hostname" not in cmd or
                cmd[cmd.index("-verify_hostname") + 1] != TAILNET_HOST or
                "-verify" not in cmd or
                "-verify_return_error" not in cmd or
                "-CAfile" not in cmd):
            fail(f"API TLS verification is not pinned to the tailnet endpoint and CA: {cmd}")
        log_action("api-listener-tls-verify")
        if not state["api_tls_san"]:
            print("verify error:num=62:Hostname mismatch", file=sys.stderr)
            sys.exit(1)
        print("Verify return code: 0 (ok)")
        sys.exit(0)
    if cmd[0] == "systemctl":
        action = cmd[1:]
        if action == ["enable", "--now", "k3s"]:
            if not state["k3s_running"]:
                log_action("systemctl enable --now k3s")
                state["k3s_running"] = True
                unit = pathlib.Path(LXC_ROOT, "etc/systemd/system/k3s.service")
                state["api_tls_san"] = TAILNET_HOST in unit.read_text()
            exit_saved(state, 0)
        if action == ["daemon-reload"]:
            log_action("systemctl daemon-reload")
            sys.exit(0)
        if action == ["stop", "k3s"]:
            log_action("systemctl stop k3s")
            state["k3s_running"] = False
            exit_saved(state, 0)
        if action in (["start", "k3s"], ["restart", "k3s"]):
            log_action("systemctl " + " ".join(action))
            state["k3s_running"] = True
            unit = pathlib.Path(LXC_ROOT, "etc/systemd/system/k3s.service")
            state["api_tls_san"] = TAILNET_HOST in unit.read_text()
            exit_saved(state, 0)
        fail(f"unexpected systemctl command: {action}")
    if cmd[0] == "sysctl":
        log_action("sysctl")
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
    if name == INSTANCE:
        lxc(cmd)
    fail(f"unknown instance: {name}")


def incus(args):
    state = load_state()
    if args[0] == "info":
        exists = args[1] == INSTANCE and state["instance"] is not None
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


def main():
    tool, args = sys.argv[1], sys.argv[2:]
    if tool == "incus":
        incus(args)
    if tool == "k3s":
        k3s(args)
    fail(f"unknown mock tool: {tool}")


if __name__ == "__main__":
    main()
