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
README = ROOT / "deploy/k8s/README.md"
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
    if os.environ.get("UPDATE_PREFIX_FAIL_INFO") == "1":
        print("mock Incus socket permission denied", file=sys.stderr)
        raise SystemExit(1)
    raise SystemExit(0)
if args[:2] == ["config", "get"] and len(args) == 4:
    if args[2:] == ["teagram-k3s", "user.teagram-k3s-bootstrap"]:
        if os.environ.get("UPDATE_PREFIX_FAIL_MARKER") == "1":
            print("mock Incus config read failed", file=sys.stderr)
            raise SystemExit(1)
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
        fail_after_patch = updated.get("_test", {}).pop("fail_after_patch", False)
        state_path.write_text(json.dumps(updated))
        if fail_after_patch:
            print("mock patch response timed out after commit", file=sys.stderr)
            raise SystemExit(1)
    print(json.dumps(updated))
    raise SystemExit(0)
if command[:2] == ["rollout", "restart"]:
    state_path = pathlib.Path(os.environ["UPDATE_PREFIX_STATE"])
    state = json.loads(state_path.read_text())
    mock_state = state.setdefault("_test", {})
    mock_state["restartCount"] = mock_state.get("restartCount", 0) + 1
    if mock_state.pop("fail_next_restart", False):
        mock_state["readyReplicas"] = 0
        state_path.write_text(json.dumps(state))
        print("mock rollout restart failed", file=sys.stderr)
        raise SystemExit(1)
    mock_state["readyReplicas"] = 1
    state_path.write_text(json.dumps(state))
    print("statefulset restarted")
    raise SystemExit(0)
if command[:2] == ["rollout", "status"]:
    state_path = pathlib.Path(os.environ["UPDATE_PREFIX_STATE"])
    state = json.loads(state_path.read_text())
    mock_state = state.setdefault("_test", {})
    mock_state["statusCount"] = mock_state.get("statusCount", 0) + 1
    if mock_state.pop("fail_next_rollout", False):
        mock_state["readyReplicas"] = 0
        state_path.write_text(json.dumps(state))
        print("timed out waiting for statefulset", file=sys.stderr)
        raise SystemExit(1)
    if mock_state.get("readyReplicas", 1) != 1:
        state_path.write_text(json.dumps(state))
        print("statefulset has no ready replicas", file=sys.stderr)
        raise SystemExit(1)
    state_path.write_text(json.dumps(state))
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
            "_test": {"readyReplicas": 1, "restartCount": 0, "statusCount": 0}}


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
    readme = README.read_text()
    for mode in ("--dry-run", "--apply", "--rollback"):
        command_text = f"sudo deploy/k8s/update-public-link-prefix.sh {mode}"
        if command_text not in readme:
            raise AssertionError(f"the k8s deployment runbook does not document {mode}")
    for statement in (
        "Only `--dry-run` and `--apply` require a ready workload.",
        "`--rollback` does not require readiness",
        "failed rollout leaves the workload unready",
    ):
        if statement not in readme:
            raise AssertionError(f"the k8s deployment runbook is missing: {statement}")

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

        expect_success(command + ["--help"], env,
                       "--rollback Restore the tailnet prefix and retain the public prefix for")
        info_error_env = env.copy()
        info_error_env["UPDATE_PREFIX_FAIL_INFO"] = "1"
        info_error = subprocess.run(command + ["--dry-run"], cwd=ROOT, env=info_error_env,
                                    text=True, capture_output=True, check=False)
        if info_error.returncode == 0 or \
                "mock Incus socket permission denied" not in info_error.stderr:
            raise AssertionError(
                "a failed Incus info check did not surface its original error: "
                f"exit={info_error.returncode}\nstderr={info_error.stderr}"
            )

        marker_error_env = env.copy()
        marker_error_env["UPDATE_PREFIX_FAIL_MARKER"] = "1"
        marker_error = subprocess.run(command + ["--dry-run"], cwd=ROOT, env=marker_error_env,
                                      text=True, capture_output=True, check=False)
        if marker_error.returncode == 0 or \
                "mock Incus config read failed" not in marker_error.stderr:
            raise AssertionError(
                "a failed Incus marker read did not surface its original error: "
                f"exit={marker_error.returncode}\nstderr={marker_error.stderr}"
            )
        if "bootstrap marker does not match" in marker_error.stderr:
            raise AssertionError("a failed Incus marker read was misreported as a marker mismatch")

        before = json.loads(state_path.read_text())
        expect_success(command + ["--dry-run"], env, "dry_run=accepted")
        if json.loads(state_path.read_text()) != before:
            raise AssertionError("server dry-run changed the ConfigMap fixture")
        if json.loads(state_path.read_text())["_test"]["restartCount"] != 0:
            raise AssertionError("dry-run restarted teagramd")

        expect_success(command + ["--apply"], env, "rollout=verified")
        applied = json.loads(state_path.read_text())["data"]
        if applied.get("TG_PUBLIC_LINK_PREFIX") != PUBLIC_PREFIX:
            raise AssertionError("apply did not set the public link prefix")
        if applied.get("TG_PREVIOUS_PUBLIC_LINK_PREFIXES") != OLD_PREFIX:
            raise AssertionError("apply did not preserve the former prefix for revocation")
        if json.loads(state_path.read_text())["_test"]["restartCount"] != 1:
            raise AssertionError("apply did not restart teagramd exactly once")
        for key in ("TG_ADVERTISE_ADDR", "TG_MEDIA_KEY_CURRENT", "KEEP_ME"):
            if applied.get(key) != before["data"].get(key):
                raise AssertionError(f"apply changed unrelated ConfigMap key {key}")

        expect_success(command + ["--apply"], env, "rollout=verified")
        if json.loads(state_path.read_text())["_test"]["restartCount"] != 2:
            raise AssertionError("repeated apply did not restart teagramd exactly once")
        expect_success(command + ["--rollback"], env, "rollout=verified")
        rolled_back_state = json.loads(state_path.read_text())
        rolled_back = rolled_back_state["data"]
        if rolled_back.get("TG_PUBLIC_LINK_PREFIX") != OLD_PREFIX:
            raise AssertionError("rollback did not restore the original prefix")
        if rolled_back.get("TG_PREVIOUS_PUBLIC_LINK_PREFIXES") != PUBLIC_PREFIX:
            raise AssertionError("rollback did not retain the public prefix for invite revocation")
        if rolled_back_state["_test"]["restartCount"] != 3:
            raise AssertionError("rollback did not restart teagramd exactly once")
        for key in ("TG_ADVERTISE_ADDR", "TG_MEDIA_KEY_CURRENT", "KEEP_ME"):
            if rolled_back.get(key) != before["data"].get(key):
                raise AssertionError(f"rollback changed unrelated ConfigMap key {key}")
        expect_success(command + ["--rollback"], env, "rollback already applied")
        idempotent_rollback = json.loads(state_path.read_text())
        if idempotent_rollback["_test"]["restartCount"] != 4:
            raise AssertionError("idempotent rollback did not restart teagramd exactly once")
        if idempotent_rollback["_test"]["statusCount"] != 4:
            raise AssertionError("idempotent rollback did not verify rollout status")
        if idempotent_rollback["_test"]["readyReplicas"] != 1:
            raise AssertionError("idempotent rollback did not verify workload readiness")

        expect_success(command + ["--apply"], env, "rollout=verified")
        reapplied = json.loads(state_path.read_text())
        if reapplied["data"].get("TG_PREVIOUS_PUBLIC_LINK_PREFIXES") != OLD_PREFIX:
            raise AssertionError("reapply did not retain the tailnet prefix for revocation")
        if reapplied["_test"]["restartCount"] != 5:
            raise AssertionError("reapply did not restart teagramd exactly once")
        expect_success(command + ["--rollback"], env, "rollout=verified")
        rolled_back_again = json.loads(state_path.read_text())
        if rolled_back_again["data"].get("TG_PREVIOUS_PUBLIC_LINK_PREFIXES") != PUBLIC_PREFIX:
            raise AssertionError("rollback after reapply dropped the public prefix")
        if rolled_back_again["_test"]["restartCount"] != 6:
            raise AssertionError("rollback after reapply did not restart teagramd exactly once")
        if rolled_back_again["_test"]["statusCount"] != 6:
            raise AssertionError("rollback after reapply did not verify rollout status")

        state_path.write_text(json.dumps(initial_state(prefix="https://other.example/")))
        unexpected = json.loads(state_path.read_text())
        refusal = expect_refusal(command + ["--apply"], env, "refusing to overwrite")
        if "current_prefix=https://other.example/" not in refusal.stderr or \
                "previous_prefix=absent" not in refusal.stderr:
            raise AssertionError("refusal did not report the observed current and previous prefixes")
        if json.loads(state_path.read_text()) != unexpected:
            raise AssertionError("refused apply changed the ConfigMap fixture")

        state_path.write_text(json.dumps(initial_state(previous="https://older.example/")))
        unexpected = json.loads(state_path.read_text())
        refusal = expect_refusal(command + ["--apply"], env, "refusing to overwrite")
        if "current_prefix=https://teagram.tailaa4918.ts.net/" not in refusal.stderr or \
                "previous_prefix=https://older.example/" not in refusal.stderr:
            raise AssertionError("refusal did not report the observed prefix values")
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
        if failed_apply["_test"]["restartCount"] != 1:
            raise AssertionError("failed apply did not restart teagramd exactly once")

        expect_success(command + ["--rollback"], env, "rollout=verified")
        recovered = json.loads(state_path.read_text())
        if recovered["data"].get("TG_PUBLIC_LINK_PREFIX") != OLD_PREFIX:
            raise AssertionError("rollback did not restore the old prefix after a failed rollout")
        if recovered["data"].get("TG_PREVIOUS_PUBLIC_LINK_PREFIXES") != PUBLIC_PREFIX:
            raise AssertionError("rollback after a failed rollout dropped public invite revocation")
        if recovered["_test"]["restartCount"] != 2:
            raise AssertionError("rollback after a failed rollout did not restart teagramd exactly once")

        patch_timeout_state = initial_state()
        patch_timeout_state["_test"]["fail_after_patch"] = True
        state_path.write_text(json.dumps(patch_timeout_state))
        patch_timeout = expect_refusal(
            command + ["--apply"], env,
            "inspect the live state, then rerun --apply or --rollback",
        )
        if "could not apply the runtime ConfigMap patch" not in patch_timeout.stderr:
            raise AssertionError("ambiguous patch failure did not explain the failed apply")
        patch_timeout_result = json.loads(state_path.read_text())
        if patch_timeout_result["data"].get("TG_PUBLIC_LINK_PREFIX") != PUBLIC_PREFIX or \
                patch_timeout_result["data"].get("TG_PREVIOUS_PUBLIC_LINK_PREFIXES") != OLD_PREFIX:
            raise AssertionError("patch timeout fixture did not preserve the committed ConfigMap state")

        rollback_status_failure = initial_state()
        state_path.write_text(json.dumps(rollback_status_failure))
        expect_success(command + ["--apply"], env, "rollout=verified")
        rollback_status_failure = json.loads(state_path.read_text())
        rollback_status_failure["_test"]["fail_next_rollout"] = True
        state_path.write_text(json.dumps(rollback_status_failure))
        expect_refusal(command + ["--rollback"], env, "rollout did not become ready")
        failed_rollback_status = json.loads(state_path.read_text())
        if failed_rollback_status["data"].get("TG_PUBLIC_LINK_PREFIX") != OLD_PREFIX or \
                failed_rollback_status["data"].get("TG_PREVIOUS_PUBLIC_LINK_PREFIXES") != PUBLIC_PREFIX:
            raise AssertionError("failed rollback status did not leave the reviewed rollback values")
        if failed_rollback_status["_test"]["readyReplicas"] != 0:
            raise AssertionError("failed rollback status did not leave the workload unready")
        if failed_rollback_status["_test"]["restartCount"] != 2:
            raise AssertionError("failed rollback status did not count the attempted restart")
        if failed_rollback_status["_test"]["statusCount"] != 2:
            raise AssertionError("failed rollback did not record its rollout status check")
        expect_success(command + ["--rollback"], env, "rollback already applied rollout=verified")
        recovered_rollback_status = json.loads(state_path.read_text())
        if recovered_rollback_status["_test"]["readyReplicas"] != 1 or \
                recovered_rollback_status["_test"]["restartCount"] != 3 or \
                recovered_rollback_status["_test"]["statusCount"] != 3:
            raise AssertionError("retry of an already-applied rollback did not restart and verify readiness")

        rollback_restart_failure = initial_state()
        state_path.write_text(json.dumps(rollback_restart_failure))
        expect_success(command + ["--apply"], env, "rollout=verified")
        rollback_restart_failure = json.loads(state_path.read_text())
        rollback_restart_failure["_test"]["fail_next_restart"] = True
        state_path.write_text(json.dumps(rollback_restart_failure))
        expect_refusal(command + ["--rollback"], env, "restart could not be started")
        failed_rollback_restart = json.loads(state_path.read_text())
        if failed_rollback_restart["data"].get("TG_PUBLIC_LINK_PREFIX") != OLD_PREFIX or \
                failed_rollback_restart["data"].get("TG_PREVIOUS_PUBLIC_LINK_PREFIXES") != PUBLIC_PREFIX:
            raise AssertionError("failed rollback restart did not leave the reviewed rollback values")
        if failed_rollback_restart["_test"]["readyReplicas"] != 0:
            raise AssertionError("failed rollback restart did not leave the workload unready")
        expect_success(command + ["--rollback"], env, "rollback already applied rollout=verified")
        recovered_rollback_restart = json.loads(state_path.read_text())
        if recovered_rollback_restart["_test"]["readyReplicas"] != 1 or \
                recovered_rollback_restart["_test"]["restartCount"] != 3 or \
                recovered_rollback_restart["_test"]["statusCount"] != 2:
            raise AssertionError("retry of a failed rollback restart did not recover the workload")


if __name__ == "__main__":
    run()
    print("public link prefix update tests passed")
