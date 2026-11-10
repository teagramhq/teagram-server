#!/usr/bin/env python3
"""Verify workload and replica rollback across post-apply release failures."""

from __future__ import annotations

import copy
import json
import os
from pathlib import Path
import shutil
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


def record(message: str) -> None:
    with Path(os.environ["DEPLOY_RELEASE_TEST_LOG"]).open(
        "a", encoding="utf-8"
    ) as log:
        log.write(message + "\n")


def mock_kubectl() -> int:
    state_path = Path(os.environ["DEPLOY_RELEASE_TEST_STATE"])
    state = json.loads(state_path.read_text(encoding="utf-8"))
    args = sys.argv[1:]

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
        for resource, template in state["failed_templates"].items():
            state["templates"][resource] = template
        for resource, replicas in state["failed_replicas"].items():
            state["replicas"][resource] = replicas
        changed = sorted(
            set(state["failed_templates"]) | set(state["failed_replicas"])
        )
        record(f"apply changed={','.join(changed)} revisions=unchanged")
        save()
        if "--output" in args and args[args.index("--output") + 1] == "name":
            print("job.batch/db-migrations-newfixture")
            print("job.batch/rustfs-init-newfixture")
        return 0

    if args[0] == "wait":
        if state["failure_phase"] == "wait" and state["applied"] and not state["failure_observed"]:
            state["failure_observed"] = True
            resource = next(iter(state["failed_templates"]))
            record(f"immediate-post-apply-failure resource={resource} revisions=unchanged")
            save()
            print("mock kubectl: immediate post-apply failure", file=sys.stderr)
            return 42
        job = next((arg for arg in args if arg.startswith("job/")), "")
        if job not in {"job/db-migrations-newfixture", "job/rustfs-init-newfixture"}:
            print(f"mock kubectl: wait did not target a rendered Job: {args}", file=sys.stderr)
            return 2
        record(f"wait exact-job={job}")
        return 0

    if args[0] == "get":
        if len(args) < 2:
            print("mock kubectl: missing get resource", file=sys.stderr)
            return 2
        resource = args[1]
        if resource in {"deployment", "statefulset"} and len(args) >= 3:
            resource = f"{resource}/{args[2]}"
        output = args[args.index("--output") + 1] if "--output" in args else ""
        if resource == "pods" and output == "json":
            print('{"items":[]}')
            return 0
        if resource == "controllerrevision":
            print("1")
            return 0
        if resource in state["templates"]:
            if output == "json":
                if state["unstable_resource"] == resource:
                    observed_generation = 0
                else:
                    observed_generation = 2
                replicas = state["replicas"][resource]
                obj = {
                    "metadata": {"generation": 2},
                    "spec": {
                        "replicas": replicas,
                        "template": state["templates"][resource],
                    },
                    "status": {
                        "observedGeneration": observed_generation,
                        "replicas": replicas,
                        "updatedReplicas": replicas,
                        "readyReplicas": replicas,
                        "availableReplicas": replicas,
                        "currentRevision": state["update_revisions"].get(resource, ""),
                        "updateRevision": state["update_revisions"].get(resource, ""),
                    },
                }
                print(json.dumps(obj))
                return 0
            if output == "jsonpath={.spec.replicas}":
                print(state["replicas"][resource])
                return 0
            if output == "jsonpath={.status.readyReplicas}":
                print(state["replicas"][resource])
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

    if args[0] == "patch" and len(args) >= 2:
        resource = args[1]
        patch = json.loads(args[args.index("--patch") + 1])
        state["replicas"][resource] = patch["spec"]["replicas"]
        record(f"patch resource={resource} replicas={state['replicas'][resource]}")
        save()
        return 0

    if args[0] == "delete" and len(args) >= 2 and args[1] == "pod":
        return 0

    print(f"mock kubectl: unsupported command {' '.join(args)}", file=sys.stderr)
    return 2


def mock_external_command() -> int:
    command = Path(sys.argv[0]).name
    if command == "curl":
        args = sys.argv[1:]
        url = next((arg for arg in args if arg.startswith(("http://", "https://"))), "")
        destination = args[args.index("--output") + 1]
        if url == "https://invalid.example/client":
            record("smoke-discovery-failed")
            print("mock curl: invalid discovery URL", file=sys.stderr)
            return 22
        content = (
            '{"mtproto":{"rsa_spki":"fixture"}}'
            if ".well-known/" in url
            else "Open this in Teagram"
        )
        Path(destination).write_text(content, encoding="utf-8")
        return 0
    if command == "docker":
        return 0
    print(f"unexpected external command: {command}", file=sys.stderr)
    return 2


def fixture_templates() -> dict[str, dict[str, object]]:
    templates: dict[str, dict[str, object]] = {}
    for resource in CONTROLLERS:
        name = resource.split("/", maxsplit=1)[1]
        templates[resource] = {
            "metadata": {"labels": {"fixture": name}},
            "spec": {"containers": [{"name": name, "image": f"example/{name}:baseline"}]},
        }
    return templates


def run_case(case: str, failed_resource: str | None, failure_phase: str,
             replica_change: int | None = None, unstable_resource: str | None = None) -> list[str]:
    repo_root = Path(__file__).resolve().parents[2]
    release_script = repo_root / "deploy/k8s/deploy-release.sh"
    baseline_templates = fixture_templates()
    failed_templates: dict[str, dict[str, object]] = {}
    failed_replicas: dict[str, int] = {}
    if failed_resource and replica_change is None:
        changed = copy.deepcopy(baseline_templates[failed_resource])
        changed["spec"]["containers"][0]["image"] = "example/failed-release"
        failed_templates[failed_resource] = changed
    if replica_change is not None:
        failed_replicas["deployment/cloudflared"] = replica_change
    update_revisions = {
        resource: f"baseline-{resource.split('/', maxsplit=1)[1]}"
        for resource in CONTROLLERS
        if resource.startswith("statefulset/")
    }
    state = {
        "applied": False,
        "failure_observed": False,
        "failure_phase": failure_phase,
        "baseline_templates": baseline_templates,
        "failed_templates": failed_templates,
        "templates": copy.deepcopy(baseline_templates),
        "baseline_replicas": {resource: 1 for resource in CONTROLLERS},
        "failed_replicas": failed_replicas,
        "replicas": {resource: 1 for resource in CONTROLLERS},
        "update_revisions": update_revisions,
        "unstable_resource": unstable_resource,
    }

    with tempfile.TemporaryDirectory(prefix=f"deploy-release-{case}-") as temp:
        temp_path = Path(temp)
        bin_path = temp_path / "bin"
        script_dir = temp_path / "deploy" / "k8s"
        runner_temp = temp_path / "runner"
        bin_path.mkdir()
        script_dir.mkdir(parents=True)
        runner_temp.mkdir()
        state_path = temp_path / "state.json"
        log_path = temp_path / "test.log"
        state_path.write_text(json.dumps(state), encoding="utf-8")
        log_path.write_text("", encoding="utf-8")
        manifest_path = temp_path / "manifest.yaml"
        manifest_path.write_text(
            "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: rollback-fixture\n",
            encoding="utf-8",
        )
        shutil.copyfile(release_script, script_dir / "deploy-release.sh")
        shutil.copyfile(repo_root / "deploy/k8s/smoke-production.sh", script_dir / "smoke-production.sh")
        (script_dir / "initialize-channel-post-expiring-summaries.sh").write_text(
            "#!/usr/bin/env bash\n"
            "if [[ ${DEPLOY_RELEASE_TEST_CASE:-} == maintenance ]]; then\n"
            "  printf 'maintenance-failed\\n' >> \"$DEPLOY_RELEASE_TEST_LOG\"\n"
            "  exit 42\n"
            "fi\n"
            "exit 0\n",
            encoding="utf-8",
        )
        for command in ("kubectl", "curl", "docker"):
            mock_command = bin_path / command
            shutil.copyfile(Path(__file__).resolve(), mock_command)
            mock_command.chmod(0o755)

        env = os.environ.copy()
        env.update(
            {
                "PATH": f"{bin_path}{os.pathsep}{env['PATH']}",
                "DEPLOY_RELEASE_TEST_STATE": str(state_path),
                "DEPLOY_RELEASE_TEST_LOG": str(log_path),
                "DEPLOY_RELEASE_TEST_CASE": case,
                "RUNNER_TEMP": str(runner_temp),
                "TEAGRAMD_IMAGE": "ghcr.io/teagramhq/teagramd:ci",
                "MT_PROTO_ENDPOINT": "127.0.0.1:12443",
                "DISCOVERY_URL": (
                    "https://invalid.example/client"
                    if case == "smoke"
                    else "https://fixture.example/.well-known/client"
                ),
                "LINK_SMOKE_URL": "https://fixture.example/+k3s-smoke",
            }
        )
        result = subprocess.run(
            ["bash", str(script_dir / "deploy-release.sh"), str(manifest_path), "a" * 40],
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
        if failure_phase == "wait" and not final_state["failure_observed"]:
            failures.append("the early post-apply failure was not injected")
        if failure_phase == "unstable" and final_state["applied"]:
            failures.append("an unstable baseline was applied")
        if "apply changed=" in log and "revisions=unchanged" not in log:
            failures.append("the injected spec changed after controller status advanced")
        if case == "smoke" and "smoke-discovery-failed" not in log:
            failures.append("the smoke check did not fail on the bad discovery URL")
        if case == "maintenance" and "maintenance-failed" not in log:
            failures.append("the maintenance command failure was not injected")

        expected_rollbacks = set(failed_templates) | set(failed_replicas)
        for resource in expected_rollbacks:
            marker = f"rollback=verified resource={resource} revision=1"
            if marker not in result.stdout:
                failures.append(f"rollback was not verified for {resource}")
        if final_state["templates"] != baseline_templates:
            failures.append("post-failure workload templates do not match their baselines")
        if final_state["replicas"] != state["baseline_replicas"]:
            failures.append("post-failure replica counts do not match their baselines")
        if failure_phase not in {"unstable", "wait"}:
            for name in ("db-migrations-newfixture", "rustfs-init-newfixture"):
                if f"wait exact-job=job/{name}" not in log:
                    failures.append(f"deploy did not wait for exact rendered Job {name}")

        if failures:
            print(f"Deploy release rollback test failed ({case}):", file=sys.stderr)
            for failure in failures:
                print(f"- {failure}", file=sys.stderr)
            print("kubectl and smoke log:", file=sys.stderr)
            print(log, file=sys.stderr, end="")
            print("deploy-release output:", file=sys.stderr)
            print(result.stdout, file=sys.stderr, end="")
            return failures

    return []


def main() -> int:
    cases = (
        ("early-statefulset", "statefulset/teagramd", "wait", None, None),
        ("early-deployment", "deployment/linkselector", "wait", None, None),
        ("smoke", "deployment/linkselector", "smoke", None, None),
        ("maintenance", "deployment/linkselector", "maintenance", None, None),
        ("cloudflared-count", None, "cloudflared", 2, None),
        ("unstable-baseline", None, "unstable", None, "deployment/linkselector"),
    )
    failures: list[str] = []
    for case in cases:
        failures.extend(run_case(*case))
    if failures:
        return 1
    print("Deploy release rollback tests passed (6 failure paths)")
    return 0


if __name__ == "__main__":
    if "DEPLOY_RELEASE_TEST_STATE" in os.environ:
        command = Path(sys.argv[0]).name
        raise SystemExit(mock_kubectl() if command == "kubectl" else mock_external_command())
    raise SystemExit(main())
