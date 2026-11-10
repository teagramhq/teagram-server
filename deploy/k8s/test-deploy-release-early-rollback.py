#!/usr/bin/env python3
"""Verify a changed workload is reverted when deploy fails before revisions advance."""

from __future__ import annotations

import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


CONTROLLERS = (
    "statefulset/postgres",
    "statefulset/rustfs",
    "statefulset/teagramd",
    "deployment/linklanding",
    "deployment/linkselector",
    "deployment/cloudflared",
)


def mock_kubectl() -> int:
    state_path = Path(os.environ["DEPLOY_RELEASE_TEST_STATE"])
    log_path = Path(os.environ["DEPLOY_RELEASE_TEST_LOG"])
    state = json.loads(state_path.read_text(encoding="utf-8"))
    args = sys.argv[1:]

    def record(message: str) -> None:
        with log_path.open("a", encoding="utf-8") as log:
            log.write(message + "\n")

    def save() -> None:
        state_path.write_text(json.dumps(state), encoding="utf-8")

    if not args:
        print("mock kubectl: missing command", file=sys.stderr)
        return 2

    if args[0] == "apply":
        if "--dry-run=server" in args:
            record("dry-run")
            return 0
        state["applied"] = True
        state["templates"]["statefulset/teagramd"] = state["failed_template"]
        record("apply changed=statefulset/teagramd revisions=unchanged")
        save()
        return 0

    if args[0] == "wait":
        if state["applied"] and not state["failure_observed"]:
            state["failure_observed"] = True
            revision = state["update_revisions"]["statefulset/teagramd"]
            record(
                "immediate-post-apply-failure "
                f"updateRevision={revision} controllerRevision=1"
            )
            save()
            print("mock kubectl: immediate post-apply failure", file=sys.stderr)
            return 42
        print("mock kubectl: unexpected wait", file=sys.stderr)
        return 2

    if args[0] == "get":
        if len(args) < 2:
            print("mock kubectl: missing get resource", file=sys.stderr)
            return 2
        resource = args[1]
        output = args[args.index("--output") + 1] if "--output" in args else ""
        if resource == "pods" and output == "json":
            print('{"items":[]}')
            return 0
        if resource == "controllerrevision":
            print("1")
            return 0
        if resource in state["templates"]:
            if output == "json":
                print(json.dumps({"spec": {"template": state["templates"][resource]}}))
                return 0
            if output == "jsonpath={.status.updateRevision}":
                print(state["update_revisions"][resource])
                return 0
            if output == r"jsonpath={.metadata.annotations.deployment\.kubernetes\.io/revision}":
                print("1")
                return 0
        print(f"mock kubectl: unsupported get {resource} {output}", file=sys.stderr)
        return 2

    if args[0] == "rollout" and len(args) >= 3:
        action, resource = args[1:3]
        if action == "undo":
            if resource not in state["templates"] or "--to-revision=1" not in args:
                print("mock kubectl: unexpected rollback target", file=sys.stderr)
                return 2
            state["templates"][resource] = state["baseline_templates"][resource]
            record(f"undo resource={resource} revision=1")
            save()
            return 0
        if action == "status":
            record(f"rollout-status resource={resource}")
            return 0

    if args[0] == "delete" and len(args) >= 2 and args[1] == "pod":
        return 0

    print(f"mock kubectl: unsupported command {' '.join(args)}", file=sys.stderr)
    return 2


def run_test() -> int:
    repo_root = Path(__file__).resolve().parents[2]
    release_script = repo_root / "deploy/k8s/deploy-release.sh"
    baseline_templates: dict[str, dict[str, object]] = {}
    for resource in CONTROLLERS:
        name = resource.split("/", maxsplit=1)[1]
        baseline_templates[resource] = {
            "metadata": {"labels": {"fixture": name}},
            "spec": {
                "containers": [
                    {"name": name, "image": f"example/{name}:baseline"}
                ]
            },
        }

    failed_template = copy.deepcopy(baseline_templates["statefulset/teagramd"])
    failed_template["spec"]["containers"][0]["image"] = (
        "example/teagramd:failed-release"
    )
    update_revisions = {
        resource: f"baseline-{resource.split('/', maxsplit=1)[1]}"
        for resource in CONTROLLERS
        if resource.startswith("statefulset/")
    }
    state = {
        "applied": False,
        "failure_observed": False,
        "baseline_templates": baseline_templates,
        "failed_template": failed_template,
        "templates": copy.deepcopy(baseline_templates),
        "update_revisions": update_revisions,
    }

    with tempfile.TemporaryDirectory(prefix="deploy-release-rollback-") as temp:
        temp_path = Path(temp)
        bin_path = temp_path / "bin"
        bin_path.mkdir()
        state_path = temp_path / "state.json"
        log_path = temp_path / "kubectl.log"
        state_path.write_text(json.dumps(state), encoding="utf-8")
        log_path.write_text("", encoding="utf-8")
        (temp_path / "manifest.yaml").write_text(
            "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: rollback-fixture\n",
            encoding="utf-8",
        )
        (bin_path / "kubectl").symlink_to(Path(__file__).resolve())

        env = os.environ.copy()
        env["PATH"] = f"{bin_path}{os.pathsep}{env['PATH']}"
        env["DEPLOY_RELEASE_TEST_STATE"] = str(state_path)
        env["DEPLOY_RELEASE_TEST_LOG"] = str(log_path)
        result = subprocess.run(
            ["bash", str(release_script), str(temp_path / "manifest.yaml"), "a" * 40],
            cwd=repo_root,
            env=env,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            check=False,
        )

        final_state = json.loads(state_path.read_text(encoding="utf-8"))
        log = log_path.read_text(encoding="utf-8")
        failures = []
        if result.returncode == 0:
            failures.append("deploy unexpectedly succeeded after the injected failure")
        if not final_state["failure_observed"]:
            failures.append("the injected failure did not occur immediately after apply")
        if "apply changed=statefulset/teagramd revisions=unchanged" not in log:
            failures.append("the bad template was not applied with controller revisions unchanged")
        if "immediate-post-apply-failure updateRevision=baseline-teagramd controllerRevision=1" not in log:
            failures.append("the failure did not observe the baseline StatefulSet revisions")
        if "undo resource=statefulset/teagramd revision=1" not in log:
            failures.append("the changed template was not rolled back to revision 1")
        if "rollback=verified resource=statefulset/teagramd revision=1" not in result.stdout:
            failures.append("the release runner did not verify the restored template")
        if final_state["templates"] != baseline_templates:
            failures.append("the post-failure workload templates do not match the baselines")

        if failures:
            print("Early post-apply rollback test failed:", file=sys.stderr)
            for failure in failures:
                print(f"- {failure}", file=sys.stderr)
            print("kubectl log:", file=sys.stderr)
            print(log, file=sys.stderr, end="")
            print("deploy-release output:", file=sys.stderr)
            print(result.stdout, file=sys.stderr, end="")
            return 1

    print("Early post-apply rollback test passed")
    return 0


if __name__ == "__main__":
    if "DEPLOY_RELEASE_TEST_STATE" in os.environ:
        raise SystemExit(mock_kubectl())
    raise SystemExit(run_test())
