#!/usr/bin/env python3
"""Exercise the guarded alpha runtime-prefix update against a fake k3s API."""

from __future__ import annotations

import json
import os
import pathlib
import subprocess
import tempfile


ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "deploy/k8s/update-public-link-prefix.sh"
OLD_PREFIX = "https://teagram.tailaa4918.ts.net/"
PUBLIC_PREFIX = "https://web.teagram.io/"


INCUS_MOCK = r'''#!/usr/bin/env python3
import copy
import json
import os
import pathlib
import sys

args = sys.argv[1:]
if args[:2] == ["info", "teagram-k3s"]:
    raise SystemExit(0)
if args[:2] == ["config", "get"] and len(args) == 4:
    if args[2:] == ["teagram-k3s", "user.teagram-k3s-bootstrap"]:
        print("MAIN-3278")
        raise SystemExit(0)
    raise SystemExit(2)
if len(args) < 6 or args[:2] != ["exec", "--mode=non-interactive"]:
    raise SystemExit(2)
try:
    separator = args.index("--")
except ValueError:
    raise SystemExit(2)
command = args[separator + 1:]
if command[:2] != ["k3s", "kubectl"]:
    raise SystemExit(2)
command = command[2:]
if command[:2] == ["get", "configmap"]:
    state_path = pathlib.Path(os.environ["UPDATE_PREFIX_STATE"])
    sys.stdout.write(state_path.read_text())
    raise SystemExit(0)
if command[:2] == ["get", "statefulset"]:
    state_path = pathlib.Path(os.environ["UPDATE_PREFIX_STATE"])
    state = json.loads(state_path.read_text())
    print(json.dumps({
        "spec": {
            "replicas": 1,
            "template": {"spec": {"containers": [{"name": "teagramd", "env": [
                {"name": "TG_PUBLIC_LINK_PREFIX", "valueFrom": {"configMapKeyRef": {
                    "name": "teagramd-runtime", "key": "TG_PUBLIC_LINK_PREFIX"}}},
                {"name": "TG_PREVIOUS_PUBLIC_LINK_PREFIXES", "valueFrom": {
                    "configMapKeyRef": {"name": "teagramd-runtime",
                                         "key": "TG_PREVIOUS_PUBLIC_LINK_PREFIXES",
                                         "optional": True}}},
            ]}]}}
        },
        "status": {"readyReplicas": state.get("_test", {}).get("readyReplicas", 1)},
    }))
    raise SystemExit(0)
if command[:2] == ["patch", "configmap"]:
    try:
        patch_text = command[command.index("--patch") + 1]
        patch = json.loads(patch_text)
    except (ValueError, IndexError, json.JSONDecodeError):
        raise SystemExit(2)
    state_path = pathlib.Path(os.environ["UPDATE_PREFIX_STATE"])
    state = json.loads(state_path.read_text())
    if patch.get("metadata", {}).get("resourceVersion") != state["metadata"]["resourceVersion"]:
        print("resource version conflict", file=sys.stderr)
        raise SystemExit(1)
    updated = copy.deepcopy(state)
    for key, value in patch["data"].items():
        if value is None:
            updated["data"].pop(key, None)
        else:
            updated["data"][key] = value
    if "--dry-run=server" not in command:
        updated["metadata"]["resourceVersion"] = str(int(state["metadata"]["resourceVersion"]) + 1)
        state_path.write_text(json.dumps(updated))
    print(json.dumps(updated))
    raise SystemExit(0)
if command[:2] == ["rollout", "restart"]:
    state_path = pathlib.Path(os.environ["UPDATE_PREFIX_STATE"])
    state = json.loads(state_path.read_text())
    state.setdefault("_test", {})["readyReplicas"] = 1
    state_path.write_text(json.dumps(state))
    print("statefulset restarted")
    raise SystemExit(0)
if command[:2] == ["rollout", "status"]:
    state_path = pathlib.Path(os.environ["UPDATE_PREFIX_STATE"])
    state = json.loads(state_path.read_text())
    mock_state = state.setdefault("_test", {})
    if mock_state.pop("fail_next_rollout", False):
        mock_state["readyReplicas"] = 0
        state_path.write_text(json.dumps(state))
        print("timed out waiting for statefulset", file=sys.stderr)
        raise SystemExit(1)
    if mock_state.get("readyReplicas", 1) != 1:
        print("statefulset has no ready replicas", file=sys.stderr)
        raise SystemExit(1)
    print("statefulset successfully rolled out")
    raise SystemExit(0)
raise SystemExit(2)
'''


def initial_state(prefix: str = OLD_PREFIX, previous: str | None = None) -> dict:
    data = {
        "TG_PUBLIC_LINK_PREFIX": prefix,
        "TG_ADVERTISE_ADDR": "teagramd:2443",
        "TG_MEDIA_KEY_CURRENT": "1",
        "KEEP_ME": "unrelated-setting",
    }
    if previous is not None:
        data["TG_PREVIOUS_PUBLIC_LINK_PREFIXES"] = previous
    return {"metadata": {"resourceVersion": "531"}, "data": data,
            "_test": {"readyReplicas": 1}}


def expect_success(command: list[str], env: dict[str, str], expected: str) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(command, cwd=ROOT, env=env, text=True, capture_output=True, check=False)
    if result.returncode != 0 or expected not in result.stdout:
        raise AssertionError(
            f"command {command[-1]} failed: exit={result.returncode}\n"
            f"stdout={result.stdout}\nstderr={result.stderr}"
        )
    return result


def expect_refusal(command: list[str], env: dict[str, str], expected: str) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(command, cwd=ROOT, env=env, text=True, capture_output=True, check=False)
    if result.returncode == 0 or expected not in result.stderr:
        raise AssertionError(
            f"command {command[-1]} should refuse: exit={result.returncode}\n"
            f"stdout={result.stdout}\nstderr={result.stderr}"
        )
    return result


def run() -> None:
    with tempfile.TemporaryDirectory(prefix="public-link-prefix-test-") as temporary:
        temp_path = pathlib.Path(temporary)
        fake_bin = temp_path / "bin"
        fake_bin.mkdir()
        for name, body in {
            "incus": INCUS_MOCK,
            "id": '#!/bin/sh\n[ "$1" = "-u" ] && { printf "0"; exit 0; }\nexit 2\n',
            "hostname": '#!/bin/sh\n[ "$1" = "-s" ] && { printf "alpha\\n"; exit 0; }\nexit 2\n',
        }.items():
            path = fake_bin / name
            path.write_text(body)
            path.chmod(0o755)

        state_path = temp_path / "configmap.json"
        state_path.write_text(json.dumps(initial_state()))
        env = os.environ.copy()
        env["PATH"] = f"{fake_bin}:{env['PATH']}"
        env["UPDATE_PREFIX_STATE"] = str(state_path)
        command = ["bash", str(SCRIPT)]

        before = json.loads(state_path.read_text())
        expect_success(command + ["--dry-run"], env, "dry_run=accepted")
        if json.loads(state_path.read_text()) != before:
            raise AssertionError("server dry-run changed the ConfigMap fixture")

        expect_success(command + ["--apply"], env, "rollout=verified")
        applied = json.loads(state_path.read_text())["data"]
        if applied.get("TG_PUBLIC_LINK_PREFIX") != PUBLIC_PREFIX:
            raise AssertionError("apply did not set the public link prefix")
        if applied.get("TG_PREVIOUS_PUBLIC_LINK_PREFIXES") != OLD_PREFIX:
            raise AssertionError("apply did not preserve the former prefix for revocation")
        for key in ("TG_ADVERTISE_ADDR", "TG_MEDIA_KEY_CURRENT", "KEEP_ME"):
            if applied.get(key) != before["data"].get(key):
                raise AssertionError(f"apply changed unrelated ConfigMap key {key}")

        expect_success(command + ["--apply"], env, "rollout=verified")
        expect_success(command + ["--rollback"], env, "rollout=verified")
        rolled_back = json.loads(state_path.read_text())["data"]
        if rolled_back.get("TG_PUBLIC_LINK_PREFIX") != OLD_PREFIX:
            raise AssertionError("rollback did not restore the original prefix")
        if "TG_PREVIOUS_PUBLIC_LINK_PREFIXES" in rolled_back:
            raise AssertionError("rollback did not remove its optional previous-prefix key")
        for key in ("TG_ADVERTISE_ADDR", "TG_MEDIA_KEY_CURRENT", "KEEP_ME"):
            if rolled_back.get(key) != before["data"].get(key):
                raise AssertionError(f"rollback changed unrelated ConfigMap key {key}")
        expect_success(command + ["--rollback"], env, "rollback already applied")

        state_path.write_text(json.dumps(initial_state(prefix="https://other.example/")))
        unexpected = json.loads(state_path.read_text())
        expect_refusal(command + ["--apply"], env, "refusing to overwrite")
        if json.loads(state_path.read_text()) != unexpected:
            raise AssertionError("refused apply changed the ConfigMap fixture")

        state_path.write_text(json.dumps(initial_state(previous="https://older.example/")))
        unexpected = json.loads(state_path.read_text())
        expect_refusal(command + ["--apply"], env, "refusing to overwrite")
        if json.loads(state_path.read_text()) != unexpected:
            raise AssertionError("apply with an unexpected previous prefix changed the fixture")

        unready_state = initial_state()
        unready_state["_test"]["readyReplicas"] = 0
        state_path.write_text(json.dumps(unready_state))
        expect_refusal(command + ["--dry-run"], env, "must be ready")
        expect_refusal(command + ["--apply"], env, "must be ready")
        if json.loads(state_path.read_text()) != unready_state:
            raise AssertionError("forward operation changed the ConfigMap while teagramd was unready")

        failed_rollout_state = initial_state()
        failed_rollout_state["_test"]["fail_next_rollout"] = True
        state_path.write_text(json.dumps(failed_rollout_state))
        expect_refusal(command + ["--apply"], env, "rollout did not become ready")
        failed_apply = json.loads(state_path.read_text())
        if failed_apply["_test"].get("readyReplicas") != 0:
            raise AssertionError("failed apply did not leave the workload unready in the fixture")
        if failed_apply["data"].get("TG_PUBLIC_LINK_PREFIX") != PUBLIC_PREFIX:
            raise AssertionError("failed apply did not leave the attempted prefix in the fixture")

        expect_success(command + ["--rollback"], env, "rollout=verified")
        recovered = json.loads(state_path.read_text())
        if recovered["data"].get("TG_PUBLIC_LINK_PREFIX") != OLD_PREFIX:
            raise AssertionError("rollback did not restore the old prefix after a failed rollout")
        if "TG_PREVIOUS_PUBLIC_LINK_PREFIXES" in recovered["data"]:
            raise AssertionError("rollback did not remove the previous-prefix key after a failed rollout")


if __name__ == "__main__":
    run()
    print("public link prefix update tests passed")
