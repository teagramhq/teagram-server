#!/usr/bin/env python3
"""Focused tests for the restore readiness gate's public safety contract."""

from __future__ import annotations

import argparse
import ast
import importlib.util
import io
import json
import os
import stat
import subprocess
import tempfile
import time
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from typing import Any
from unittest.mock import Mock, patch


SCRIPT = Path(__file__).with_name("restore-readiness.py")
SHELL_GATE = Path(__file__).with_name("test-restore-readiness.sh")
SPEC = importlib.util.spec_from_file_location("restore_readiness", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError("restore readiness module unavailable")
RESTORE_READINESS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RESTORE_READINESS)


def container_fixture() -> tuple[dict[str, Any], dict[str, Any]]:
    environment = {"LANG": "C", "PATH": "/usr/bin:/bin"}
    record = {
        "id": "a" * 64,
        "name": "restore-readiness-fixture-application",
        "image_id": "sha256:" + "b" * 64,
        "token": "c" * 32,
        "role": "application",
        "user": "10001:10001",
        "entrypoint": ["/run/app/launcher"],
        "command": [],
        "open_stdin": False,
        "environment_sha256": RESTORE_READINESS.environment_digest(environment),
        "network_mode": "none",
        "extra_hosts": [],
        "memory": 768 * 1024 * 1024,
        "pids_limit": 64,
        "shm_size": 64 * 1024 * 1024,
        "cpu_quota": 100000,
        "cpu_period": 100000,
        "mounts": [],
        "unexpected_volume_mounts": [],
        "tmpfs_options": {"/tmp": "rw,nosuid,nodev,noexec,size=64m,uid=10001,gid=10001,mode=1777"},
    }
    document = {
        "Id": record["id"],
        "Name": "/" + record["name"],
        "Image": record["image_id"],
        "Config": {
            "Labels": {
                RESTORE_READINESS.IMAGE_TOKEN_LABEL: record["token"],
                RESTORE_READINESS.IMAGE_ROLE_LABEL: record["role"],
            },
            "User": record["user"],
            "Entrypoint": record["entrypoint"],
            "Cmd": record["command"],
            "Tty": False,
            "OpenStdin": False,
            # `docker create` records these stdout/stderr attach defaults on every container.
            "AttachStdout": True,
            "AttachStderr": True,
            "AttachStdin": False,
            "Env": [f"{key}={value}" for key, value in environment.items()],
        },
        "HostConfig": {
            "NetworkMode": "none",
            "Privileged": False,
            "ReadonlyRootfs": True,
            "CapAdd": [],
            "CapDrop": ["ALL"],
            "SecurityOpt": ["no-new-privileges:true"],
            "PortBindings": None,
            "PublishAllPorts": False,
            "AutoRemove": False,
            "Devices": [],
            "Binds": [],
            "VolumesFrom": None,
            "Links": None,
            "ExtraHosts": [],
            "PidMode": "",
            "UTSMode": "",
            "UsernsMode": "",
            "IpcMode": "private",
            "CgroupnsMode": "private",
            "Runtime": "runc",
            "Sysctls": None,
            "LogConfig": {"Type": "none"},
            "RestartPolicy": {"Name": "no"},
            "Memory": record["memory"],
            "MemorySwap": record["memory"],
            "PidsLimit": record["pids_limit"],
            "ShmSize": record["shm_size"],
            "CpuQuota": record["cpu_quota"],
            "CpuPeriod": record["cpu_period"],
            "NanoCpus": 0,
            "Ulimits": [{"Name": "core", "Soft": 0, "Hard": 0}],
            "Tmpfs": record["tmpfs_options"],
        },
        "Mounts": [],
        "NetworkSettings": {"Networks": {}},
    }
    return document, record


def cleanup_deadline_drill_type(inventory_calls: list[list[str]], instances: list[Any]) -> type:
    class CleanupDeadlineDrill(RESTORE_READINESS.Drill):
        def __init__(self, _args: Any, run_dir: Path, manifest: dict[str, Any]) -> None:
            self.manifest = manifest
            self.run_dir = run_dir
            self.manifest_path = run_dir / "ownership.json"
            self.gateway = None
            self.save = lambda: None

            def cleanup_call(args: list[str], **_kwargs: Any) -> tuple[int, bytes]:
                inventory_calls.append(args)
                return 0, b""

            self.cleanup_call = Mock(side_effect=cleanup_call)
            self.cleanup_processes = lambda: True
            self.remove_build_inputs = lambda: True
            self.cleanup_owned_containers = lambda: True
            self.cleanup_owned_images = lambda: True
            self.cleanup_networks_and_volumes = lambda: True
            instances.append(self)

    return CleanupDeadlineDrill


def expired_work_cleanup_manifest(
    run_dir: Path,
    global_lock: dict[str, int],
    rollout_lock: dict[str, int],
) -> tuple[dict[str, Any], Path]:
    private_path = run_dir / "secrets" / "fixture.bin"
    private_path.parent.mkdir()
    private_path.write_bytes(b"private fixture")
    info = private_path.lstat()
    manifest = {
        "global_lock": global_lock,
        "rollout_lock": rollout_lock,
        "supervisor": {"pid": 0, "start_ticks": 0},
        "parent": {"pid": 999999, "start_ticks": 1},
        "parent_done": True,
        "work_deadline": time.monotonic() - 1,
        "cleanup_deadline": time.monotonic() + 60,
        "token": "c" * 32,
        "resource_creation_started": True,
        "baseline": {
            "docker_inventory": {"containers": {}, "volumes": {}, "networks": {}, "images": {}},
            "containers": [],
            "volumes": [],
            "networks": [],
        },
        "containers": [],
        "pending_container": None,
        "images": [],
        "networks": [],
        "files": [
            {
                "path": str(private_path),
                "device": info.st_dev,
                "inode": info.st_ino,
                "mode": stat.S_IMODE(info.st_mode),
                "uid": info.st_uid,
                "gid": info.st_gid,
            }
        ],
        "processes": [],
        "build_directory": None,
    }
    return manifest, private_path


class RestoreReadinessContractTests(unittest.TestCase):
    def test_fixture_generation_failure_names_the_failed_check(self) -> None:
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.synthetic_auth_key_ids = ()
        drill.sql = lambda *_args, **_kwargs: "unexpected fixture state"

        with self.assertRaises(RESTORE_READINESS.DrillFailure) as failure:
            drill.seed_source_auth_key_fixture({})

        self.assertEqual(failure.exception.phase, "fixture_generation")
        self.assertEqual(
            getattr(failure.exception, "fixture_check", None),
            "source_auth_key_fixture",
        )

    def test_fixture_check_in_report_is_allowlisted_and_sanitized(self) -> None:
        report = RESTORE_READINESS.report_document(
            "fixture_generation",
            "fixture_failure",
            "verified",
            1,
            fixture_check="source_auth_key_fixture",
        )

        self.assertEqual(report["fixture_check"], "source_auth_key_fixture")
        self.assertNotIn("/", json.dumps(report))
        with self.assertRaises(ValueError):
            RESTORE_READINESS.report_document(
                "fixture_generation",
                "fixture_failure",
                "verified",
                1,
                fixture_check="/tmp/private-fixture.env",
            )

    def test_run_primary_waits_for_supervisor_owned_manifest_registration(self) -> None:
        tree = ast.parse(SCRIPT.read_text(encoding="utf-8"))
        run_primary = next(
            node
            for node in ast.walk(tree)
            if isinstance(node, ast.FunctionDef) and node.name == "run_primary"
        )
        calls = [
            node
            for node in ast.walk(run_primary)
            if isinstance(node, ast.Call)
            and isinstance(node.func, ast.Name)
            and node.func.id == "wait_for_supervisor_registration"
        ]

        self.assertEqual(len(calls), 1, "the supervisor must publish its own manifest record before parent writes")
        publish_line = next(
            node.lineno
            for node in ast.walk(run_primary)
            if isinstance(node, ast.Call)
            and isinstance(node.func, ast.Attribute)
            and node.func.attr == "Popen"
        )
        wait_line = calls[0].lineno
        layout_line = next(
            node.lineno
            for node in ast.walk(run_primary)
            if isinstance(node, ast.Call)
            and isinstance(node.func, ast.Attribute)
            and node.func.attr == "create_private_layout"
        )
        self.assertLess(publish_line, wait_line)
        self.assertLess(wait_line, layout_line)
        self.assertFalse(
            any(
                isinstance(node, ast.Call)
                and isinstance(node.func, ast.Attribute)
                and node.func.attr == "save"
                and publish_line < node.lineno < wait_line
                for node in ast.walk(run_primary)
            ),
            "the parent must not save the manifest while the supervisor registers",
        )

    def test_wait_for_supervisor_registration_reads_the_child_record(self) -> None:
        supervisor = Mock(pid=123, poll=Mock(return_value=None))
        initial = {"supervisor": {"pid": 0, "start_ticks": 0}}
        registered = {"supervisor": {"pid": 123, "start_ticks": 77}}

        with (
            patch.object(RESTORE_READINESS, "load_manifest", side_effect=(initial, registered)),
            patch.object(RESTORE_READINESS.time, "sleep"),
        ):
            manifest = RESTORE_READINESS.wait_for_supervisor_registration(
                Path("/run/restore-readiness-test/ownership.json"),
                supervisor,
                77,
            )

        self.assertIs(manifest, registered)
        self.assertEqual(supervisor.poll.call_count, 2)

    def test_suite_work_budget_caps_each_run_and_is_shared_by_shell_gate(self) -> None:
        self.assertEqual(RESTORE_READINESS.bounded_work_seconds(None, None), 30 * 60)
        self.assertEqual(RESTORE_READINESS.bounded_work_seconds(None, 300), 300)
        self.assertEqual(RESTORE_READINESS.bounded_work_seconds("malformed-sql", None), 8 * 60)
        self.assertEqual(RESTORE_READINESS.bounded_work_seconds("sigkill", 9 * 60), 8 * 60)
        self.assertEqual(
            RESTORE_READINESS.bounded_work_seconds("sigkill", 20 * 60, local_fixture_test=True),
            20 * 60,
        )
        with self.assertRaises(ValueError):
            RESTORE_READINESS.bounded_work_seconds(None, 0)

        shell = SHELL_GATE.read_text(encoding="utf-8")
        self.assertIn("suite_work_deadline=0", shell)
        self.assertIn("suite_work_deadline=$((SECONDS + 30 * 60))", shell)
        self.assertIn("suite_teardown_deadline=$((suite_work_deadline + 4 * 60))", shell)
        run_drill = shell[shell.index("run_drill() {"):shell.index("\nrun_sigkill_recovery_drill", shell.index("run_drill() {"))]
        self.assertIn("work_seconds=$(suite_remaining_seconds)", run_drill)
        self.assertIn('--prepared-dir "$build_dir"', run_drill)
        self.assertIn('--work-seconds "$work_seconds"', run_drill)
        self.assertIn('run_preparation_command "${build_env[@]}" go build', shell)
        self.assertIn("preparation_timeout_seconds=$((45 * 60))", shell)
        self.assertIn("PREPARED_BUILD_FLAGS=(-trimpath -buildvcs=false -p=1)", shell)
        self.assertLess(shell.index("validate_prepared_runtime\n"), shell.index("suite_work_deadline=$((SECONDS + 30 * 60))"))
        self.assertIn("run_teardown_command sudo -n -- /usr/bin/find", shell)
        self.assertIn('local_args=(--local-dump "$2")', shell)
        self.assertIn('local_args=(--local-fixture-test)', shell)
        self.assertIn('time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())', SCRIPT.read_text(encoding="utf-8"))
        self.assertLess(shell.index("--validate-local-dump"), shell.index("build_inputs\n"))
        workflow = Path(__file__).parents[3] / ".github" / "workflows" / "ci.yml"
        workflow_text = workflow.read_text(encoding="utf-8")
        self.assertIn("test-restore-readiness-foreign-fixture.sh", workflow_text)
        self.assertIn("test-restore-readiness-foreign-fixture.sh --local-fixture-test", workflow_text)
        self.assertIn("--local-fixture-test --fault-test auth-clear-signal", shell)
        self.assertIn("--local-fixture-test --fault-test sigkill", shell)
        self.assertIn("--attest-prepared", shell)
        self.assertIn("--teardown-prepared", shell)
        self.assertLess(shell.index("--validate-source"), shell.index("build pinned runtime binaries"))

    def test_source_identity_compares_head_blobs_without_running_fsmonitor(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "repo"
            root.mkdir()
            marker = Path(directory) / "fsmonitor-ran"
            git_environment = {
                key: value for key, value in os.environ.items() if not key.startswith("GIT_")
            }
            git_environment.update({
                "GIT_CONFIG_NOSYSTEM": "1",
                "GIT_CONFIG_GLOBAL": os.devnull,
                "GIT_AUTHOR_NAME": "Restore Readiness Test",
                "GIT_AUTHOR_EMAIL": "restore-readiness-test@example.invalid",
                "GIT_COMMITTER_NAME": "Restore Readiness Test",
                "GIT_COMMITTER_EMAIL": "restore-readiness-test@example.invalid",
            })

            def git(*arguments: str) -> None:
                subprocess.run(
                    ["/usr/bin/git", "-C", str(root), *arguments],
                    check=True,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    env=git_environment,
                    timeout=10,
                )

            git("init", "--quiet")
            (root / "nested").mkdir()
            (root / "nested" / "tracked.go").write_bytes(b"package tracked\n")
            (root / ".gitignore").write_text("ignored.txt\n", encoding="utf-8")
            git("add", "nested/tracked.go", ".gitignore")
            git("commit", "--quiet", "-m", "test fixture")

            fsmonitor = Path(directory) / "fsmonitor"
            fsmonitor.write_text(
                f"#!/bin/sh\nprintf 'called\\n' > {marker}\nprintf 'token\\n'\n",
                encoding="utf-8",
            )
            fsmonitor.chmod(0o755)
            git("config", "core.fsmonitor", str(fsmonitor))

            with patch.object(RESTORE_READINESS, "ROOT", root):
                RESTORE_READINESS.source_identity()
                self.assertFalse(marker.exists(), "privileged source inspection ran checkout fsmonitor")

                git("update-index", "--assume-unchanged", "nested/tracked.go")
                (root / "nested" / "tracked.go").write_bytes(b"package tampered\n")
                with self.assertRaisesRegex(ValueError, "source worktree is dirty"):
                    RESTORE_READINESS.source_identity()

                git("update-index", "--no-assume-unchanged", "nested/tracked.go")
                (root / "nested" / "tracked.go").write_bytes(b"package tracked\n")
                (root / "untracked.txt").write_text("untracked\n", encoding="utf-8")
                with self.assertRaisesRegex(ValueError, "source worktree is dirty"):
                    RESTORE_READINESS.source_identity()
                (root / "untracked.txt").unlink()
                (root / "ignored.txt").write_text("ignored\n", encoding="utf-8")
                with self.assertRaisesRegex(ValueError, "source worktree is dirty"):
                    RESTORE_READINESS.source_identity()

    def test_unverified_drill_cleanup_keeps_prepared_runtime_for_recovery(self) -> None:
        shell = SHELL_GATE.read_text(encoding="utf-8")

        def function_source(name: str) -> str:
            start = shell.index(f"{name}() {{")
            end = shell.index("\n}\n", start) + 2
            return shell[start:end]

        helpers = "\n".join(
            function_source(name)
            for name in (
                "preserve_prepared_on_unverified_cleanup",
                "cleanup_prepared_after_failed_drill",
            )
        )
        harness = "\n".join(
            (
                helpers,
                "keep_prepared=0",
                "cleanup_called=0",
                "cleanup_failed=0",
                "cleanup_prepared_inputs() { cleanup_called=1; return 0; }",
                "emit_prepared_cleanup_failure() { cleanup_failed=1; }",
                'cleanup_prepared_after_failed_drill "$REPORT"',
                'printf "%s %s %s\\n" "$keep_prepared" "$cleanup_called" "$cleanup_failed"',
            )
        )
        self.assertIn(
            "if (( keep_prepared == 0 )); then\n    if ! cleanup_prepared_inputs; then status=1; fi",
            shell,
        )
        for cleanup, expected in (("unverified", "1 0 0\n"), ("verified", "0 1 0\n")):
            with self.subTest(cleanup=cleanup):
                environment = os.environ.copy()
                environment["REPORT"] = json.dumps({"cleanup": cleanup})
                result = subprocess.run(
                    ["/usr/bin/bash", "-c", harness],
                    check=True,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    env=environment,
                    text=True,
                )
                self.assertEqual(result.stdout, expected)

    def test_missing_prepared_runtime_refuses_before_docker_or_run_lock(self) -> None:
        path = Path("/tmp") / ("teagram-restore-readiness." + os.urandom(4).hex())
        arguments = argparse.Namespace(
            prepared_dir=path,
            docker_bin="/usr/bin/true",
            local_dump=None,
            local_fixture_test=False,
            fault_test=None,
            work_seconds=None,
        )
        output = io.StringIO()
        with (
            patch.object(RESTORE_READINESS.os, "geteuid", return_value=0),
            patch.object(RESTORE_READINESS, "ensure_trusted_system_executable") as trusted_docker,
            patch.object(RESTORE_READINESS, "open_rollout_lock") as rollout_lock,
            redirect_stdout(output),
        ):
            status = RESTORE_READINESS.run_primary(arguments)

        self.assertEqual(status, 1)
        report = json.loads(output.getvalue())
        self.assertEqual(report["phase"], "preflight")
        self.assertEqual(report["category"], "preflight_failure")
        self.assertEqual(report["cleanup"], "not_started")
        trusted_docker.assert_not_called()
        rollout_lock.assert_not_called()

    def test_failed_supervisor_spawn_names_the_cleanup_failure_stage(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-supervisor-spawn-") as directory:
            run_dir = Path(directory) / "run"
            run_dir.mkdir(mode=0o700)
            rollout_lock_path = Path(directory) / "rollout-lock"
            global_lock_path = Path(directory) / "global-lock"
            rollout_lock_path.touch()
            global_lock_path.touch()
            rollout_lock_fd = os.open(rollout_lock_path, os.O_RDONLY)
            global_lock_fd = os.open(global_lock_path, os.O_RDONLY)

            prepared = {
                "atlas_bin": "/tmp/teagram-restore-readiness.abcdefgh/atlas",
                "build_dir": "/tmp/teagram-restore-readiness.abcdefgh",
                "prepared_uid": 1000,
                "go_arch": "amd64",
                "path": "/tmp/teagram-restore-readiness.abcdefgh",
                "device": 1,
                "inode": 2,
                "attestation_sha256": "a" * 64,
            }

            class SpawnFailureDrill:
                def __init__(self, _args: Any, current_run_dir: Path, manifest: dict[str, Any]) -> None:
                    self.manifest = manifest
                    self.run_dir = current_run_dir
                    self.manifest_path = current_run_dir / "ownership.json"
                    self.phase = "preflight"
                    self.fixture_check = "fixture_setup"
                    self.gateway = None
                    self.checker_result = {"help_getConfig": "not_run", "synthetic_signin": "not_run"}
                    self.migration_head = "not_reached"
                    self.auth_keys_cleared = False
                    self.input_mode = "synthetic"

                def save(self) -> None:
                    return None

            arguments = argparse.Namespace(
                prepared_dir=Path(prepared["path"]),
                docker_bin="/usr/bin/docker",
                local_dump=None,
                local_fixture_test=False,
                fault_test=None,
                work_seconds=None,
            )
            stdout = io.StringIO()
            stderr = io.StringIO()
            try:
                with (
                    patch.object(RESTORE_READINESS.os, "geteuid", return_value=0),
                    patch.object(RESTORE_READINESS, "verify_prepared_runtime", return_value=prepared),
                    patch.object(RESTORE_READINESS, "ensure_trusted_system_executable", return_value="/usr/bin/docker"),
                    patch.object(
                        RESTORE_READINESS,
                        "open_rollout_lock",
                        return_value=(rollout_lock_fd, {"device": 1, "inode": 2}),
                    ),
                    patch.object(
                        RESTORE_READINESS,
                        "open_global_lock",
                        return_value=(global_lock_fd, {"device": 3, "inode": 4}),
                    ),
                    patch.object(RESTORE_READINESS.tempfile, "mkdtemp", return_value=str(run_dir)),
                    patch.object(RESTORE_READINESS, "process_start_ticks", return_value=77),
                    patch.object(RESTORE_READINESS, "Drill", SpawnFailureDrill),
                    patch.object(RESTORE_READINESS.subprocess, "Popen", side_effect=OSError("spawn failed")),
                    patch.object(RESTORE_READINESS, "load_manifest", return_value={}),
                    patch.object(RESTORE_READINESS, "foreign_inventory_failure", return_value=None),
                    redirect_stdout(stdout),
                    redirect_stderr(stderr),
                ):
                    status = RESTORE_READINESS.run_primary(arguments)
            finally:
                for descriptor in (rollout_lock_fd, global_lock_fd):
                    try:
                        os.close(descriptor)
                    except OSError:
                        pass

        self.assertEqual(status, 1)
        self.assertIn(
            "restore readiness cleanup failed: operation=supervisor_setup "
            "error=supervisor_failed stage=spawn_supervisor",
            stderr.getvalue(),
        )

    def test_early_preparation_failure_can_remove_empty_owned_runtime_directory(self) -> None:
        path = Path("/tmp") / ("teagram-restore-readiness." + os.urandom(4).hex())
        path.mkdir(mode=0o700)
        lock_path = Path("/tmp") / ("teagram-restore-readiness-test-lock." + os.urandom(4).hex())
        lock_fd = os.open(lock_path, os.O_RDWR | os.O_CREAT | os.O_EXCL, 0o600)
        shell = SHELL_GATE.read_text(encoding="utf-8")
        build_inputs = shell[shell.index("build_inputs() {"):shell.index("\nvalidate_prepared_runtime() {")]

        try:
            with (
                patch.object(RESTORE_READINESS.os, "geteuid", return_value=0),
                patch.object(RESTORE_READINESS, "open_global_lock", return_value=(lock_fd, {})),
            ):
                self.assertTrue(RESTORE_READINESS.teardown_prepared_runtime(path, os.getuid()))
            self.assertFalse(path.exists())
            self.assertLess(shell.index("trap cleanup_build EXIT"), shell.index("build_inputs\n"))
            self.assertLess(
                build_inputs.index('mktemp -d "$build_parent'),
                build_inputs.index('preflight_stage="read pinned Go toolchain"'),
            )
        finally:
            try:
                os.close(lock_fd)
            except OSError:
                pass
            lock_path.unlink(missing_ok=True)
            if path.exists():
                path.rmdir()

    def test_deploy_rollout_conflict_refuses_before_restore_lock(self) -> None:
        prepared_path = Path("/tmp/teagram-restore-readiness.abcdefgh")
        prepared = {
            "path": str(prepared_path),
            "device": 1,
            "inode": 2,
            "attestation_sha256": "a" * 64,
            "prepared_uid": 1000,
            "go_arch": "amd64",
            "atlas_bin": str(prepared_path / "atlas"),
            "build_dir": str(prepared_path),
        }
        arguments = argparse.Namespace(
            prepared_dir=prepared_path,
            docker_bin="/usr/bin/true",
            local_dump=None,
            local_fixture_test=False,
            fault_test=None,
            work_seconds=None,
        )
        output = io.StringIO()
        with (
            patch.object(RESTORE_READINESS.os, "geteuid", return_value=0),
            patch.object(RESTORE_READINESS, "verify_prepared_runtime", return_value=prepared),
            patch.object(RESTORE_READINESS, "ensure_trusted_system_executable", return_value="/usr/bin/true"),
            patch.object(RESTORE_READINESS, "open_rollout_lock", return_value=None),
            patch.object(RESTORE_READINESS, "open_global_lock") as restore_lock,
            redirect_stdout(output),
        ):
            status = RESTORE_READINESS.run_primary(arguments)

        self.assertEqual(status, 1)
        report = json.loads(output.getvalue())
        self.assertEqual(report["phase"], "preflight")
        self.assertEqual(report["category"], "rollout_in_progress")
        self.assertEqual(report["cleanup"], "not_started")
        restore_lock.assert_not_called()

    def test_rollout_shared_lock_is_nonblocking_and_excludes_exclusive_rollouts(self) -> None:
        lock_path = Path("/tmp") / ("teagram-deploy-test." + os.urandom(4).hex())
        exclusive_fd = os.open(lock_path, os.O_RDWR | os.O_CREAT | os.O_EXCL, 0o600)
        os.fchmod(exclusive_fd, 0o600)
        shared: tuple[int, dict[str, Any]] | None = None
        try:
            RESTORE_READINESS.fcntl.flock(exclusive_fd, RESTORE_READINESS.fcntl.LOCK_EX | RESTORE_READINESS.fcntl.LOCK_NB)
            self.assertIsNone(
                RESTORE_READINESS.open_rollout_lock(lock_path, expected_uid=os.geteuid())
            )
            RESTORE_READINESS.fcntl.flock(exclusive_fd, RESTORE_READINESS.fcntl.LOCK_UN)
            shared = RESTORE_READINESS.open_rollout_lock(lock_path, expected_uid=os.geteuid())
            self.assertIsNotNone(shared)
            with self.assertRaises(BlockingIOError):
                RESTORE_READINESS.fcntl.flock(
                    exclusive_fd,
                    RESTORE_READINESS.fcntl.LOCK_EX | RESTORE_READINESS.fcntl.LOCK_NB,
                )
        finally:
            if shared is not None:
                os.close(shared[0])
            os.close(exclusive_fd)
            lock_path.unlink(missing_ok=True)

    def test_prepared_runtime_attestation_checks_source_toolchain_images_and_artifact_hashes(self) -> None:
        prepared_path = Path("/tmp") / ("teagram-restore-readiness." + os.urandom(4).hex())
        prepared_path.mkdir(mode=0o700)
        os.chmod(prepared_path, 0o700)
        try:
            with tempfile.TemporaryDirectory(prefix="rr-toolchain-") as toolchain_directory:
                toolchain_root = Path(toolchain_directory)
                go_binary = toolchain_root / "bin" / "go"
                go_binary.parent.mkdir()
                go_binary.write_bytes(b"pinned-go-binary")
                go_binary.chmod(0o500)
                artifacts: dict[str, dict[str, Any]] = {}
                for name in sorted(RESTORE_READINESS.PREPARED_ARTIFACTS):
                    item = prepared_path / name
                    item.write_bytes((name + "-payload").encode())
                    item.chmod(0o500)
                    digest, size = RESTORE_READINESS.sha256_file(item)
                    artifacts[name] = {"sha256": digest, "size": size}

                directory_info = prepared_path.stat()
                source = {"revision": "a" * 40, "tree_sha256": "b" * 64}
                attestation = {
                    "schema_version": 1,
                    "prepared_uid": os.getuid(),
                    "directory": {"device": directory_info.st_dev, "inode": directory_info.st_ino},
                    "source": source,
                    "toolchain": {
                        "version": RESTORE_READINESS.minimum_go_version(),
                        "goos": "linux",
                        "goarch": "amd64",
                        "root": str(toolchain_root),
                        "root_sha256": RESTORE_READINESS.sha256_directory(toolchain_root),
                        "binary": str(go_binary),
                        "binary_sha256": RESTORE_READINESS.sha256_file(go_binary)[0],
                    },
                    "images": {
                        "source": RESTORE_READINESS.SOURCE_IMAGE,
                        "restore": RESTORE_READINESS.RESTORE_IMAGE,
                    },
                    "seed_commit": RESTORE_READINESS.SEED_COMMIT,
                    "build_flags": RESTORE_READINESS.PREPARED_BUILD_FLAGS,
                    "build_environment": {
                        "CGO_ENABLED": "0",
                        "GOENV": "off",
                        "GOMAXPROCS": "1",
                        "GOPROXY": "off",
                        "GOSUMDB": "off",
                        "GOTOOLCHAIN": "local",
                    },
                    "artifacts": artifacts,
                }
                attestation_path = prepared_path / RESTORE_READINESS.PREPARED_ATTESTATION
                attestation_path.write_text(json.dumps(attestation), encoding="utf-8")
                attestation_path.chmod(0o400)
                prepared_path.chmod(0o500)

                with patch.object(RESTORE_READINESS, "source_identity", return_value=(source["revision"], source["tree_sha256"])):
                    verified = RESTORE_READINESS.verify_prepared_runtime(
                        prepared_path,
                        expected_owner=os.getuid(),
                    )
                    self.assertEqual(verified["go_arch"], "amd64")
                    self.assertEqual(verified["prepared_uid"], os.getuid())

                    prepared_path.chmod(0o700)
                    attestation_path.chmod(0o600)
                    attestation["images"]["source"] = "mirror.gcr.io/library/postgres:mutable"
                    attestation_path.write_text(json.dumps(attestation), encoding="utf-8")
                    attestation_path.chmod(0o400)
                    prepared_path.chmod(0o500)
                    with self.assertRaisesRegex(ValueError, "prepared runtime pins mismatch"):
                        RESTORE_READINESS.verify_prepared_runtime(prepared_path, expected_owner=os.getuid())

                    prepared_path.chmod(0o700)
                    attestation_path.chmod(0o600)
                    attestation["images"] = {
                        "source": RESTORE_READINESS.SOURCE_IMAGE,
                        "restore": RESTORE_READINESS.RESTORE_IMAGE,
                    }
                    attestation_path.write_text(json.dumps(attestation), encoding="utf-8")
                    attestation_path.chmod(0o400)
                    (prepared_path / "teagramd").chmod(0o600)
                    (prepared_path / "teagramd").write_bytes(b"tampered-binary")
                    (prepared_path / "teagramd").chmod(0o500)
                    prepared_path.chmod(0o500)
                    with self.assertRaisesRegex(ValueError, "prepared runtime artifact mismatch"):
                        RESTORE_READINESS.verify_prepared_runtime(prepared_path, expected_owner=os.getuid())
        finally:
            prepared_path.chmod(0o700)
            for item in prepared_path.iterdir():
                item.unlink()
            prepared_path.rmdir()

    def test_local_dump_uses_latest_head_and_pinned_fixture_schema(self) -> None:
        versions = RESTORE_READINESS.repo_migration_versions()
        self.assertTrue(versions)
        self.assertEqual(RESTORE_READINESS.HEAD_SCHEMA, versions[-1])
        self.assertEqual(RESTORE_READINESS.LOCAL_FIXTURE_SCHEMA, "20261024000002")
        self.assertIn(RESTORE_READINESS.LOCAL_FIXTURE_SCHEMA, versions)
        self.assertGreater(RESTORE_READINESS.LOCAL_FIXTURE_SCHEMA, RESTORE_READINESS.BASE_SCHEMA)
        self.assertLess(RESTORE_READINESS.LOCAL_FIXTURE_SCHEMA, RESTORE_READINESS.HEAD_SCHEMA)
        self.assertGreater(RESTORE_READINESS.HEAD_SCHEMA, "20261021000004")
        self.assertEqual(
            RESTORE_READINESS.resolve_local_dump_migration_head(RESTORE_READINESS.HEAD_SCHEMA),
            RESTORE_READINESS.HEAD_SCHEMA,
        )
        with self.assertRaises(ValueError):
            RESTORE_READINESS.resolve_local_dump_migration_head("20990101000000")

    def test_local_fault_injection_is_limited_to_fixture_signal_and_sigkill(self) -> None:
        runtime_args = [
            "--docker-bin", "/usr/bin/docker",
            "--prepared-dir", "/tmp/teagram-restore-readiness.abcdefgh",
        ]
        for fault in ("auth-clear-signal", "sigkill"):
            with self.subTest(fault=fault), patch.object(
                RESTORE_READINESS.sys,
                "argv",
                [str(SCRIPT), *runtime_args, "--local-fixture-test", "--fault-test", fault],
            ):
                parsed = RESTORE_READINESS.parse_args()
            self.assertTrue(parsed.local_fixture_test)
            self.assertEqual(parsed.fault_test, fault)

        invalid_local_inputs = (
            [*runtime_args, "--local-fixture-test", "--fault-test", "malformed-sql"],
            [*runtime_args, "--local-fixture-test", "--fault-test", "auth-clear-failure"],
            [*runtime_args, "--local-dump", "/root/pgdump-pre-abcdef0-20261022T211333Z.sql", "--fault-test", "sigkill"],
        )
        for arguments in invalid_local_inputs:
            with self.subTest(arguments=arguments), patch.object(
                RESTORE_READINESS.sys,
                "argv",
                [str(SCRIPT), *arguments],
            ), redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                RESTORE_READINESS.parse_args()

    def test_local_dump_preflight_is_a_single_metadata_only_mode(self) -> None:
        arguments = [
            "--validate-local-dump",
            "/opt/teagram/backups/pgdump-pre-" + "a" * 40 + ".sql",
        ]
        with patch.object(RESTORE_READINESS.sys, "argv", [str(SCRIPT), *arguments]):
            parsed = RESTORE_READINESS.parse_args()
        self.assertEqual(parsed.validate_local_dump, Path(arguments[1]))
        self.assertIsNone(parsed.local_dump)
        self.assertFalse(parsed.local_fixture_test)

        for invalid in (
            [*arguments, "extra.sql"],
            [*arguments, "--local-dump", arguments[1]],
            [*arguments, "--docker-bin", "/usr/bin/docker"],
        ):
            with self.subTest(invalid=invalid), patch.object(
                RESTORE_READINESS.sys, "argv", [str(SCRIPT), *invalid]
            ), redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                RESTORE_READINESS.parse_args()

    def test_local_dump_fixture_shadows_postgres_18_declared_volume(self) -> None:
        tree = ast.parse(SCRIPT.read_text(encoding="utf-8"))
        fixture_method = next(
            node
            for node in ast.walk(tree)
            if isinstance(node, ast.FunctionDef) and node.name == "create_local_dump_fixture"
        )
        self.assertTrue(
            any(
                isinstance(node, ast.Call)
                and isinstance(node.func, ast.Name)
                and node.func.id == "local_unenrolled_auth_key_fixture_sql"
                for node in ast.walk(fixture_method)
            )
        )
        self.assertIn("UNENROLLED_BOUND_AUTH_KEY_COUNT_SQL", ast.unparse(fixture_method))
        image_container_call = next(
            node
            for node in ast.walk(fixture_method)
            if isinstance(node, ast.Call)
            and isinstance(node.func, ast.Attribute)
            and node.func.attr == "image_container"
        )
        tmpfs_options = next(
            keyword.value for keyword in image_container_call.keywords if keyword.arg == "tmpfs_options"
        )
        tmpfs_paths = ast.literal_eval(tmpfs_options)

        self.assertIn("/var/lib/postgresql", tmpfs_paths)
        self.assertNotIn("/var/lib/postgresql/data", tmpfs_paths)

    def test_local_fixture_source_migration_allows_added_users_and_auth_keys(self) -> None:
        self.assertEqual(RESTORE_READINESS.source_fixture_counts(False), (1, 3))
        self.assertEqual(RESTORE_READINESS.source_fixture_counts(True), (3, 5))

        tree = ast.parse(SCRIPT.read_text(encoding="utf-8"))
        run_primary = next(
            node
            for node in ast.walk(tree)
            if isinstance(node, ast.FunctionDef) and node.name == "run_primary"
        )
        count_calls = sorted(
            (node.lineno, ast.unparse(node.args[0]))
            for node in ast.walk(run_primary)
            if isinstance(node, ast.Call)
            and isinstance(node.func, ast.Name)
            and node.func.id == "source_fixture_counts"
        )
        fixture_call_line = next(
            node.lineno
            for node in ast.walk(run_primary)
            if isinstance(node, ast.Call)
            and isinstance(node.func, ast.Attribute)
            and node.func.attr == "create_local_dump_fixture"
        )
        self.assertEqual([argument for _, argument in count_calls], ["False", "args.local_fixture_test"])
        self.assertLess(count_calls[0][0], fixture_call_line)
        self.assertGreater(count_calls[1][0], fixture_call_line)

    def test_local_fixture_retains_atlas_environment_for_pending_migration(self) -> None:
        tree = ast.parse(SCRIPT.read_text(encoding="utf-8"))
        run_primary = next(
            node
            for node in ast.walk(tree)
            if isinstance(node, ast.FunctionDef) and node.name == "run_primary"
        )
        fixture_migration = next(
            node
            for node in ast.walk(run_primary)
            if isinstance(node, ast.Call)
            and isinstance(node.func, ast.Attribute)
            and node.func.attr == "migrate_database"
            and len(node.args) >= 3
            and isinstance(node.args[2], ast.Name)
            and node.args[2].id == "LOCAL_FIXTURE_SCHEMA"
        )
        self.assertIn(
            "keep_env=True",
            [f"{keyword.arg}={ast.unparse(keyword.value)}" for keyword in fixture_migration.keywords],
        )

    def test_postgres_restore_role_is_not_the_pg18_bootstrap_superuser(self) -> None:
        role = RESTORE_READINESS.RESTORE_BOOTSTRAP_ROLE
        self.assertNotEqual(role, "postgres")
        self.assertEqual(
            RESTORE_READINESS.bootstrap_postgres_role_sql("a" * 64),
            "CREATE ROLE postgres WITH LOGIN SUPERUSER CREATEDB CREATEROLE "
            "REPLICATION BYPASSRLS PASSWORD '" + "a" * 64 + "';\n"
            "ALTER DATABASE teagram OWNER TO postgres;",
        )
        with self.assertRaises(ValueError):
            RESTORE_READINESS.bootstrap_postgres_role_sql("a' SUPERUSER; --")

        tree = ast.parse(SCRIPT.read_text(encoding="utf-8"))
        create_database = next(
            node
            for node in ast.walk(tree)
            if isinstance(node, ast.FunctionDef) and node.name == "create_database"
        )
        create_database_source = ast.unparse(create_database)
        self.assertIn("database_user=RESTORE_BOOTSTRAP_ROLE", create_database_source)
        self.assertIn("self.sql(record, role_sql, self.phase", create_database_source)

        prepare_materials = next(
            node
            for node in ast.walk(tree)
            if isinstance(node, ast.FunctionDef) and node.name == "prepare_materials"
        )
        self.assertEqual(ast.unparse(prepare_materials).count("'POSTGRES_USER': RESTORE_BOOTSTRAP_ROLE"), 2)

    def test_fixture_role_inventory_excludes_cluster_bootstrap_roles(self) -> None:
        self.assertIn(
            "rolname NOT IN ('postgres', '" + RESTORE_READINESS.RESTORE_BOOTSTRAP_ROLE + "')",
            RESTORE_READINESS.FIXTURE_STATE_SQL,
        )

    def test_local_inventory_ignores_pg16_to_pg18_catalog_drift(self) -> None:
        inventory = RESTORE_READINESS.LOCAL_OBJECT_INVENTORY_SQL
        self.assertIn("FROM aclexplode(c.relacl) AS acl WHERE acl.grantee <> c.relowner", inventory)
        self.assertIn(
            "FROM aclexplode(relation.relacl) AS acl WHERE acl.grantee <> relation.relowner",
            RESTORE_READINESS.EXTENSION_STATE_SQL,
        )
        self.assertIn("d.objid IN (t.typrelid, (SELECT element.typrelid FROM pg_type AS element WHERE element.oid = t.typelem))", inventory)
        self.assertIn(
            "routine.proname || '(' || oidvectortypes(routine.proargtypes) || ')'",
            RESTORE_READINESS.EXTENSION_STATE_SQL,
        )
        self.assertIn("FROM pg_rewrite AS rewrite", inventory)
        self.assertIn("rewrite.rulename <> '_RETURN'", inventory)
        self.assertIn("pg_get_ruledef(rewrite.oid, true)", inventory)
        hostile_fixtures = next(
            node
            for node in ast.walk(ast.parse(SCRIPT.read_text(encoding="utf-8")))
            if isinstance(node, ast.FunctionDef) and node.name == "run_local_hostile_fixture_tests"
        )
        self.assertIn("CREATE RULE restore_readiness_unexpected_rule", ast.unparse(hostile_fixtures))
        self.assertIn("pg_catalog.pg_reload_conf()", ast.unparse(hostile_fixtures))

    def test_sigkill_drill_requires_injection_marker_and_signal_status(self) -> None:
        marker = b"SIGKILL_INJECTED\n"
        with patch.object(RESTORE_READINESS.os, "write", return_value=len(marker)) as write:
            RESTORE_READINESS.write_sigkill_marker(17)
        write.assert_called_once_with(17, marker)

        with patch.object(RESTORE_READINESS.os, "write", return_value=1):
            with self.assertRaises(RESTORE_READINESS.DrillFailure):
                RESTORE_READINESS.write_sigkill_marker(17)

        shell = SHELL_GATE.read_text(encoding="utf-8")
        self.assertIn("run_sigkill_recovery_drill --fault-test sigkill", shell)
        self.assertIn('[[ $(<"$sigkill_output") == "SIGKILL_INJECTED" ]]', shell)
        self.assertIn(
            'if [[ $sigkill_status -ne 137 ]] || [[ $sigkill_marker != present ]]; then',
            shell,
        )

    def test_docker_executable_must_be_a_trusted_system_binary(self) -> None:
        trusted = RESTORE_READINESS.ensure_trusted_system_executable("/usr/bin/true")
        self.assertEqual(trusted, str(Path("/usr/bin/true").resolve()))

        with tempfile.TemporaryDirectory(prefix="rr-path-shadow-") as temporary_directory:
            shadow = Path(temporary_directory) / "docker"
            shadow.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            shadow.chmod(0o755)
            with self.assertRaises(RESTORE_READINESS.DrillFailure):
                RESTORE_READINESS.ensure_trusted_system_executable(str(shadow))

    def test_preflight_docker_command_failure_reports_command_and_stderr(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-docker-inventory-") as temporary_directory:
            executable = Path(temporary_directory) / "docker"
            executable.write_text(
                "#!/bin/sh\nprintf 'Docker daemon is still starting\\n' >&2\nexit 17\n",
                encoding="utf-8",
            )
            executable.chmod(0o755)
            drill = object.__new__(RESTORE_READINESS.Drill)
            drill.executable = str(executable)
            drill.environment = {"PATH": "/usr/bin:/bin"}
            drill.manifest = {"work_deadline": time.monotonic() + 10, "processes": []}
            drill.save = lambda: None
            error = io.StringIO()

            with redirect_stderr(error):
                with self.assertRaises(RESTORE_READINESS.DrillFailure) as failure:
                    drill.call(
                        ["pull", "mirror.gcr.io/library/postgres@sha256:" + "a" * 64],
                        phase="preflight",
                        category="preflight_failure",
                    )

        self.assertEqual(failure.exception.category, "preflight_failure")
        diagnostic = error.getvalue()
        self.assertIn("restore readiness preflight Docker command failed:", diagnostic)
        self.assertIn(f'command=["{executable}","pull","mirror.gcr.io/library/postgres@sha256:', diagnostic)
        self.assertIn("outcome=exit 17", diagnostic)
        self.assertIn('stderr="Docker daemon is still starting\\n"', diagnostic)
        self.assertEqual(drill.manifest["processes"], [])

    def test_preload_image_validation_failure_reports_reference_check_and_value(self) -> None:
        reference = "mirror.gcr.io/library/postgres@sha256:" + "a" * 64
        valid_image = {
            "RepoDigests": [reference],
            "Os": "linux",
            "Architecture": "amd64",
            "Id": "sha256:" + "b" * 64,
        }
        failures = (
            ("RepoDigests", {**valid_image, "RepoDigests": ["unexpected"]}, ["unexpected"]),
            (
                "Os/Architecture",
                {**valid_image, "Architecture": "arm64"},
                {"Os": "linux", "Architecture": "arm64"},
            ),
            ("Id", {**valid_image, "Id": "unexpected"}, "unexpected"),
        )

        for check, document, observed in failures:
            with self.subTest(check=check):
                drill = object.__new__(RESTORE_READINESS.Drill)
                drill.args = Mock(go_arch="amd64")
                drill.call = Mock()
                drill.image_inspect = Mock(return_value=document)
                error = io.StringIO()

                with redirect_stderr(error):
                    with self.assertRaises(RESTORE_READINESS.DrillFailure) as failure:
                        drill.preload_image(reference)

                self.assertEqual(failure.exception.category, "preflight_failure")
                diagnostic = error.getvalue()
                self.assertIn(f"reference={json.dumps(reference)}", diagnostic)
                self.assertIn(f"check={check}", diagnostic)
                self.assertIn(f"observed={json.dumps(observed, separators=(',', ':'))}", diagnostic)

    def test_preload_sets_specific_preflight_stage_for_each_image(self) -> None:
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.images = {}
        stages: list[str] = []

        def preload_image(reference: str) -> str:
            stages.append(drill.preflight_step)
            return reference

        drill.preload_image = preload_image
        drill.preload()

        self.assertEqual(
            stages,
            ["preload source database image", "preload restore database image"],
        )
        drill.run_dir = Path(".")
        with patch.object(
            RESTORE_READINESS,
            "create_image_build_contexts",
            side_effect=RuntimeError("stop after stage update"),
        ):
            with self.assertRaisesRegex(RuntimeError, "stop after stage update"):
                drill.build_images()
        self.assertEqual(drill.preflight_step, "build drill images")

    def test_startup_report_names_a_rejected_docker_executable(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-path-shadow-") as temporary_directory:
            shadow = Path(temporary_directory) / "docker"
            shadow.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            shadow.chmod(0o755)
            output = io.StringIO()
            error = io.StringIO()
            args = Mock(docker_bin=str(shadow), fault_test=None, work_seconds=None)
            with patch.object(RESTORE_READINESS.os, "geteuid", return_value=0):
                with (
                    patch.object(
                        RESTORE_READINESS,
                        "verify_prepared_runtime",
                        return_value={
                            "atlas_bin": "/tmp/teagram-restore-readiness.abcdefgh/atlas",
                            "build_dir": "/tmp/teagram-restore-readiness.abcdefgh",
                            "prepared_uid": 1000,
                            "go_arch": "amd64",
                            "path": "/tmp/teagram-restore-readiness.abcdefgh",
                            "device": 1,
                            "inode": 2,
                            "attestation_sha256": "a" * 64,
                        },
                    ),
                    redirect_stdout(output),
                    redirect_stderr(error),
                ):
                    status = RESTORE_READINESS.run_primary(args)

        self.assertEqual(status, 1)
        self.assertEqual(json.loads(output.getvalue())["category"], "preflight_failure")
        self.assertIn("preflight check failed: trusted Docker executable", error.getvalue())

    def test_attached_timeouts_terminate_and_reap_child(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-attach-timeout-") as temporary_directory:
            executable = Path(temporary_directory) / "docker"
            executable.write_text("#!/bin/sh\nexec /usr/bin/sleep 30\n", encoding="utf-8")
            executable.chmod(0o755)
            real_popen = RESTORE_READINESS.subprocess.Popen
            for method_name in ("attached", "attached_status"):
                with self.subTest(method=method_name):
                    drill = object.__new__(RESTORE_READINESS.Drill)
                    drill.executable = str(executable)
                    drill.environment = {"PATH": "/usr/bin:/bin"}
                    drill.manifest = {"work_deadline": time.monotonic() + 30, "processes": []}
                    drill.save = lambda: None
                    drill.attest_container = lambda *_args: {}
                    children: list[Any] = []

                    def spawn(*args: Any, **kwargs: Any) -> Any:
                        child = real_popen(*args, **kwargs)
                        children.append(child)
                        return child

                    with patch.object(RESTORE_READINESS.subprocess, "Popen", side_effect=spawn):
                        with self.assertRaises(RESTORE_READINESS.DrillFailure) as failure:
                            getattr(drill, method_name)({"id": "fixture"}, "restore", "restore_failure", timeout=1)

                    self.assertEqual(failure.exception.category, "timeout")
                    self.assertEqual(len(children), 1)
                    self.assertIsNotNone(children[0].returncode)
                    self.assertEqual(drill.manifest["processes"], [])

    def test_malformed_sql_fixture_is_generated_internally(self) -> None:
        statement = RESTORE_READINESS.malformed_sql_fixture("a" * 32)
        self.assertEqual(statement, 'SELEC "restore_readiness_' + "a" * 32 + '";')
        with self.assertRaises(ValueError):
            RESTORE_READINESS.malformed_sql_fixture("external input")

    def test_malformed_sql_stream_rejects_unexpected_success_and_reports_failure(self) -> None:
        token = "a" * 32
        fixture = RESTORE_READINESS.malformed_sql_fixture(token)
        for consumer_status, expected_category in ((0, "schema_failure"), (1, "restore_failure")):
            with self.subTest(consumer_status=consumer_status), tempfile.TemporaryDirectory(
                prefix="rr-malformed-restore-"
            ) as temporary_directory:
                capture = Path(temporary_directory) / "restore-input.sql"
                executable = Path(temporary_directory) / "docker-fixture"
                executable.write_text(
                    "#!/usr/bin/python3\n"
                    "import pathlib, sys\n"
                    "payload = sys.stdin.buffer.read()\n"
                    f"pathlib.Path({str(capture)!r}).write_bytes(payload)\n"
                    f"raise SystemExit({consumer_status} if payload == {fixture.encode()!r} else 0)\n",
                    encoding="utf-8",
                )
                executable.chmod(0o755)

                drill = object.__new__(RESTORE_READINESS.Drill)
                drill.executable = str(executable)
                drill.environment = {"PATH": "/usr/bin:/bin"}
                drill.manifest = {"work_deadline": time.monotonic() + 5, "processes": []}
                drill.save = lambda: None
                drill.attest_container = lambda *_args: {}
                drill.sockets = {"target": Path(temporary_directory)}
                drill.files = {"target_tool_env": Path(temporary_directory) / "target-env"}
                client_specs: list[dict[str, Any]] = []

                def create_client(_image: str, **kwargs: Any) -> dict[str, str]:
                    client_specs.append(kwargs)
                    return {"id": "target"}

                drill.image_container = create_client

                with self.assertRaises(RESTORE_READINESS.DrillFailure) as failure:
                    drill.restore_fixture({"id": "source"}, {"id": "database"}, malformed_sql=fixture)

                self.assertEqual(failure.exception.phase, "restore")
                self.assertEqual(failure.exception.category, expected_category)
                self.assertEqual(capture.read_bytes(), fixture.encode())
                self.assertEqual(len(client_specs), 1)
                self.assertEqual(client_specs[0]["entrypoint"], "psql")
                self.assertEqual(client_specs[0]["command_args"], ["--no-psqlrc", "--set=ON_ERROR_STOP=1", "--dbname=teagram"])
                self.assertEqual(drill.manifest["processes"], [])
                report = RESTORE_READINESS.report_document("restore", failure.exception.category, "verified", 1)
                self.assertEqual(report["result"], "fail")
                self.assertEqual(report["exit"], 1)
                self.assertEqual(report["cleanup"], "verified")
                self.assertNotIn(token, json.dumps(report))

    def test_image_build_contexts_create_the_missing_build_directory(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            work_directory = Path(temporary_directory) / "work"
            work_directory.mkdir(mode=0o700)
            build_root = work_directory / "build"

            app_context, tools_context = RESTORE_READINESS.create_image_build_contexts(build_root)

            self.assertEqual(app_context, build_root / "app")
            self.assertEqual(tools_context, build_root / "tools")
            self.assertTrue(app_context.is_dir())
            self.assertTrue(tools_context.is_dir())
            self.assertEqual(app_context.stat().st_mode & 0o777, 0o700)
            self.assertEqual(tools_context.stat().st_mode & 0o777, 0o700)

    def test_postgres_fixture_images_are_public_and_digest_pinned(self) -> None:
        for image in (RESTORE_READINESS.SOURCE_IMAGE, RESTORE_READINESS.RESTORE_IMAGE):
            with self.subTest(image=image):
                self.assertRegex(image, r"^mirror\.gcr\.io/library/postgres@sha256:[0-9a-f]{64}$")
        self.assertNotEqual(RESTORE_READINESS.SOURCE_IMAGE, RESTORE_READINESS.RESTORE_IMAGE)

    def test_ambient_environment_rejects_production_and_proxy_configuration(self) -> None:
        self.assertTrue(RESTORE_READINESS.validate_ambient_environment({"HOME": "/home/runner", "LANG": "C"}))
        for environment in (
            {"DATABASE_URL": "postgres://example"},
            {"COMPOSE_FILE": "compose.yml"},
            {"TG_POSTGRES_DSN": "postgres://example"},
            {"HTTP_PROXY": "http://proxy.invalid"},
            {"AWS_PROFILE": "production"},
        ):
            with self.subTest(environment=tuple(environment)):
                self.assertFalse(RESTORE_READINESS.validate_ambient_environment(environment))

    def test_environment_inventory_rejects_duplicate_names(self) -> None:
        self.assertEqual(RESTORE_READINESS.env_map(["A=one", "B=two"]), {"A": "one", "B": "two"})
        with self.assertRaises(ValueError):
            RESTORE_READINESS.env_map(["A=one", "A=two"])
        with self.assertRaises(ValueError):
            RESTORE_READINESS.env_map(["not-an-environment-entry"])

    def test_report_success_requires_all_assertions_and_cleanup(self) -> None:
        passed = RESTORE_READINESS.report_document(
            "application_assertions",
            "assertions_passed",
            "verified",
            123,
            migration_head=RESTORE_READINESS.HEAD_SCHEMA,
            help_get_config="pass",
            synthetic_signin="pass",
            auth_keys_cleared="pass",
        )
        self.assertEqual(
            passed,
            {
                "result": "pass",
                "migration_head": RESTORE_READINESS.HEAD_SCHEMA,
                "help_getConfig": "pass",
                "synthetic_signin": "pass",
                "auth_keys_cleared": "pass",
                "cleanup": "verified",
                "elapsed_ms": 123,
                "exit": 0,
            },
        )
        local_passed = RESTORE_READINESS.report_document(
            "application_assertions",
            "assertions_passed",
            "verified",
            123,
            migration_head=RESTORE_READINESS.HEAD_SCHEMA,
            help_get_config="pass",
            synthetic_signin="pass",
            auth_keys_cleared="pass",
            input_mode="local",
        )
        self.assertEqual(local_passed, dict(passed, input_mode="local"))
        cleanup_failure = RESTORE_READINESS.report_document(
            "application_assertions",
            "assertions_passed",
            "unverified",
            123,
            migration_head=RESTORE_READINESS.HEAD_SCHEMA,
            help_get_config="pass",
            synthetic_signin="pass",
            auth_keys_cleared="pass",
        )
        self.assertEqual(cleanup_failure["result"], "fail")
        self.assertEqual(cleanup_failure["exit"], 1)
        self.assertEqual(cleanup_failure["cleanup"], "unverified")
        with self.assertRaises(ValueError):
            RESTORE_READINESS.report_document(
                "application_assertions",
                "assertions_passed",
                "verified",
                123,
                migration_head=RESTORE_READINESS.HEAD_SCHEMA,
                help_get_config="pass",
                synthetic_signin="pass",
            )
        with self.assertRaises(ValueError):
            RESTORE_READINESS.report_document(
                "application_assertions",
                "assertions_passed",
                "verified",
                123,
                migration_head=RESTORE_READINESS.BASE_SCHEMA,
                help_get_config="pass",
                synthetic_signin="pass",
                auth_keys_cleared="pass",
            )

    def test_docker_resource_inventory_records_isolation_metadata_only(self) -> None:
        container_id = "a" * 64
        image_id = "sha256:" + "b" * 64
        network_id = "c" * 64
        container_name = "foreign-postgres"
        network_name = "foreign-network"
        volume_name = "foreign-volume"
        labels = {"restore-readiness.fixture": "foreign"}
        mounts = [
            {
                "Type": "volume",
                "Name": volume_name,
                "Source": "/var/lib/docker/volumes/foreign-volume/_data",
                "Destination": "/foreign",
                "Driver": "local",
                "Mode": "z",
                "RW": True,
                "Propagation": "rprivate",
            },
            {
                "Type": "bind",
                "Source": "/srv/foreign-config",
                "Destination": "/etc/foreign-config",
                "Mode": "ro",
                "RW": False,
                "Propagation": "rprivate",
            },
        ]
        port_bindings = {"5432/tcp": [{"HostIp": "127.0.0.1", "HostPort": "49153"}]}
        attachments = {
            network_name: {
                "NetworkID": network_id,
                "EndpointID": "d" * 64,
                "Gateway": "172.25.0.1",
                "IPAddress": "172.25.0.2",
                "MacAddress": "02:42:ac:19:00:02",
            }
        }
        container_fields = [
            container_id,
            "/" + container_name,
            image_id,
            "postgres:18",
            json.dumps(labels),
            "running",
            "true",
            "false",
            "false",
            "false",
            "0",
            "2026-10-25T17:00:00.000000000Z",
            "0001-01-01T00:00:00Z",
            "none",
            json.dumps(attachments),
            json.dumps(mounts),
            json.dumps({"5432/tcp": {}}),
            json.dumps(port_bindings),
            "false",
            network_name,
            "null",
            "null",
            "null",
            "null",
            "false",
            "null",
            "null",
            json.dumps(["no-new-privileges:true"]),
            "false",
            "null",
            "",
            "private",
            "",
            "",
            json.dumps({"Name": "no", "MaximumRetryCount": 0}),
        ]
        volume_fields = [
            volume_name,
            "local",
            "local",
            "/var/lib/docker/volumes/foreign-volume/_data",
            "2026-10-25T17:00:00Z",
            json.dumps(labels),
            "{}",
        ]
        network_fields = [
            network_id,
            network_name,
            "bridge",
            "local",
            "false",
            "true",
            "false",
            "false",
            "false",
            "null",
            json.dumps({"Driver": "default", "Options": None, "Config": [{"Subnet": "172.25.0.0/16"}]}),
            "null",
            json.dumps(labels),
            json.dumps({container_id: {"Name": container_name, "EndpointID": "d" * 64}}),
        ]

        def call(arguments: list[str]) -> bytes:
            if arguments[:3] == ["ps", "--all", "--no-trunc"]:
                return f"{container_id}\n".encode()
            if arguments[0] == "inspect" and arguments[1:3] == ["--type", "container"]:
                return ("\n".join(container_fields) + "\n").encode()
            if arguments[:2] == ["volume", "ls"]:
                return f"{volume_name}\n".encode()
            if arguments[:2] == ["volume", "inspect"]:
                return ("\n".join(volume_fields) + "\n").encode()
            if arguments[:2] == ["image", "ls"]:
                self.assertEqual(
                    arguments,
                    [
                        "image",
                        "ls",
                        "--all",
                        "--quiet",
                        "--no-trunc",
                        "--filter",
                        f"label={RESTORE_READINESS.IMAGE_TOKEN_LABEL}",
                    ],
                )
                return f"{image_id}\n".encode()
            if arguments[:2] == ["network", "ls"]:
                return f"{network_id}\t{network_name}\n".encode()
            if arguments[:2] == ["network", "inspect"]:
                return ("\n".join(network_fields) + "\n").encode()
            raise AssertionError(f"unexpected Docker inventory command: {arguments[0]}")

        inventory = RESTORE_READINESS.collect_docker_resource_inventory(call)
        container = inventory["containers"][container_id]
        self.assertEqual(container["name"], container_name)
        self.assertEqual(container["labels"], labels)
        self.assertEqual(container["attachments"], attachments)
        expected_mounts = sorted(mounts, key=lambda item: json.dumps(item, sort_keys=True, separators=(",", ":")))
        self.assertEqual(container["mounts"], expected_mounts)
        self.assertEqual(container["published_ports"], port_bindings)
        self.assertEqual(container["state"]["running"], "true")
        self.assertEqual(inventory["volumes"][volume_name]["labels"], labels)
        self.assertEqual(inventory["networks"][network_id]["attachments"][container_id]["Name"], container_name)
        self.assertEqual(inventory["images"], {image_id: {"id": image_id}})
        container_fields[15] = json.dumps(list(reversed(mounts)))
        self.assertEqual(RESTORE_READINESS.collect_docker_resource_inventory(call), inventory)
        RESTORE_READINESS.require_no_docker_identity_collisions(inventory, "9" * 32, "preflight")
        self.assertNotIn(".Config.Env", RESTORE_READINESS.CONTAINER_INVENTORY_TEMPLATE)
        self.assertNotIn(".State.Health.Log", RESTORE_READINESS.CONTAINER_INVENTORY_TEMPLATE)
        for path in (
            "State.Health",
            "Config.Labels",
            "Config.ExposedPorts",
            "NetworkSettings.Networks",
            "HostConfig.PortBindings",
            "HostConfig.Binds",
            "HostConfig.VolumesFrom",
            "HostConfig.Links",
            "HostConfig.ExtraHosts",
            "HostConfig.CapAdd",
            "HostConfig.CapDrop",
            "HostConfig.SecurityOpt",
            "HostConfig.Devices",
        ):
            parent, key = path.split(".")
            self.assertIn(f'(index .{parent} "{key}")', RESTORE_READINESS.CONTAINER_INVENTORY_TEMPLATE)
        with self.assertRaises(ValueError):
            RESTORE_READINESS._inventory_labels('{"unexpected":[]}')

    def test_mounted_sources_includes_foreign_container_mounts(self) -> None:
        foreign_id = "e" * 64
        foreign_mount = {
            "Type": "volume",
            "Source": "/var/lib/docker/volumes/foreign-volume/_data",
            "Destination": "/foreign",
        }
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {
            "resource_creation_started": True,
            "containers": [],
            "pending_container": None,
        }
        drill.run_dir = Path("/run/restore-readiness.1234abcd")
        drill.docker_resource_inventory = Mock(
            return_value={
                "containers": {
                    foreign_id: {
                        "name": "foreign-postgres",
                        "mounts": [foreign_mount],
                    }
                }
            }
        )

        self.assertEqual(drill.mounted_sources(), {foreign_mount["Source"]})

    def test_mounted_sources_refuses_foreign_mount_of_private_run_directory(self) -> None:
        foreign_id = "e" * 64
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {
            "resource_creation_started": True,
            "containers": [],
            "pending_container": None,
        }
        drill.run_dir = Path("/run/restore-readiness.1234abcd")
        drill.docker_resource_inventory = Mock(
            return_value={
                "containers": {
                    foreign_id: {
                        "name": "foreign-host-runtime",
                        "mounts": [
                            {
                                "Type": "bind",
                                "Source": "/run",
                                "Destination": "/host-run",
                            }
                        ],
                    }
                }
            }
        )

        self.assertIsNone(drill.mounted_sources())

    def test_docker_identity_collision_fixtures_refuse_with_fixed_category(self) -> None:
        token = "a" * 32
        project = "restore-readiness-" + token[:16]
        fixtures = (
            (
                "name",
                {"containers": {"id": {"name": project + "-application", "labels": {}}}, "volumes": {}, "networks": {}},
            ),
            (
                "token label",
                {"containers": {}, "volumes": {"volume": {"name": "volume", "labels": {"owner": token}}}, "networks": {}},
            ),
            (
                "Compose project",
                {"containers": {}, "volumes": {}, "networks": {"id": {"name": "network", "labels": {"com.docker.compose.project": project}}}},
            ),
        )
        for name, inventory in fixtures:
            with self.subTest(name=name), self.assertRaises(RESTORE_READINESS.DrillFailure) as failure:
                RESTORE_READINESS.require_no_docker_identity_collisions(inventory, token, "preflight")
            self.assertEqual(failure.exception.category, "isolation_refusal")

    def test_foreign_inventory_change_fails_with_fixed_isolation_category(self) -> None:
        foreign_id = "e" * 64
        second_id = "d" * 64
        network_id = "f" * 64
        baseline = {
            "containers": {foreign_id: {"name": "foreign", "state": {"running": "true"}}},
            "volumes": {"foreign-volume": {"labels": {}}},
            "networks": {network_id: {"name": "foreign-network", "attachments": {}}},
            "images": {},
        }
        changed_state = json.loads(json.dumps(baseline))
        changed_state["containers"][foreign_id]["state"]["running"] = "false"
        added_container = json.loads(json.dumps(baseline))
        added_container["containers"][second_id] = {"name": "new-foreign", "state": {"running": "true"}}
        removed_volume = json.loads(json.dumps(baseline))
        removed_volume["volumes"].clear()
        changed_network = json.loads(json.dumps(baseline))
        changed_network["networks"][network_id]["attachments"] = {foreign_id: {"IPAddress": "172.25.0.2"}}
        added_image = json.loads(json.dumps(baseline))
        added_image["images"]["sha256:" + "b" * 64] = {"id": "sha256:" + "b" * 64}

        for name, current, summary in (
            ("changed state", changed_state, "containers(added=0,removed=0,fields=state)"),
            ("added container", added_container, "containers(added=1,removed=0,fields=none)"),
            ("removed volume", removed_volume, "volumes(added=0,removed=1,fields=none)"),
            ("changed network attachment", changed_network, "networks(added=0,removed=0,fields=attachments)"),
            ("added token-labeled image", added_image, "images(added=1,removed=0,fields=none)"),
        ):
            with self.subTest(change=name):
                drill = object.__new__(RESTORE_READINESS.Drill)
                drill.manifest = {
                    "baseline": {"docker_inventory": baseline},
                    "resource_creation_started": True,
                    "containers": [],
                    "pending_container": None,
                    "networks": [],
                }
                drill.docker_resource_inventory = Mock(return_value=current)
                diagnostic = io.StringIO()
                with redirect_stderr(diagnostic):
                    self.assertFalse(drill.verify_foreign_inventory())
                self.assertTrue(drill.manifest["foreign_inventory_changed"])
                self.assertEqual(
                    diagnostic.getvalue().strip(),
                    f"restore readiness foreign Docker inventory changed: {summary}",
                )
                self.assertEqual(drill.manifest["foreign_inventory_diagnostic"], f"changed:{summary}")
                self.assertIsNotNone(
                    RESTORE_READINESS.FOREIGN_INVENTORY_DIAGNOSTIC.fullmatch(f"changed:{summary}")
                )
                failure = RESTORE_READINESS.foreign_inventory_failure(drill.manifest)
                self.assertIsNotNone(failure)
                self.assertEqual(failure.phase, "cleanup")
                self.assertEqual(failure.category, "isolation_refusal")
                report = RESTORE_READINESS.report_document("cleanup", failure.category, "unverified", 0)
                self.assertEqual(report["category"], "isolation_refusal")

    def test_cleanup_resources_persists_safe_failed_step_names(self) -> None:
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {"resource_creation_started": False, "cleanup_deadline": 1.0}
        drill.gateway = None
        drill.cleanup_budget_available = Mock(return_value=True)
        drill.cleanup_processes = Mock(return_value=False)
        drill.remove_build_inputs = Mock(return_value=True)
        drill.remove_unmounted_files = Mock(return_value=True)
        drill.verify_foreign_inventory = Mock(return_value=True)
        drill.save = Mock()
        diagnostic = io.StringIO()

        with redirect_stderr(diagnostic):
            self.assertFalse(drill.cleanup_resources())

        self.assertEqual(drill.manifest["cleanup_failures"], ["processes"])
        self.assertEqual(diagnostic.getvalue().strip(), "restore readiness cleanup failed: processes")

    def test_foreign_inventory_comparison_excludes_only_manifest_owned_containers(self) -> None:
        owned_id = "1" * 64
        owned = {"name": "restore-readiness-owned", "labels": {}}
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {
            "baseline": {"docker_inventory": {"containers": {}, "volumes": {}, "networks": {}, "images": {}}},
            "resource_creation_started": True,
            "containers": [{"id": owned_id}],
            "pending_container": None,
            "networks": [],
        }
        drill.docker_resource_inventory = Mock(
            return_value={"containers": {owned_id: owned}, "volumes": {}, "networks": {}, "images": {}}
        )
        self.assertTrue(drill.verify_foreign_inventory())

    def test_cleanup_inventory_checks_cleanup_command_status(self) -> None:
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {
            "work_deadline": time.monotonic() - 1,
            "cleanup_deadline": time.monotonic() + 60,
            "token": "c" * 32,
        }
        drill.cleanup_call = Mock(return_value=(1, b""))

        with self.assertRaises(RESTORE_READINESS.DrillFailure) as failure:
            drill.docker_resource_inventory("cleanup")

        self.assertEqual(failure.exception.phase, "cleanup")
        self.assertEqual(failure.exception.category, "cleanup_failure")
        self.assertEqual(drill.cleanup_call.call_count, 1)

    def test_unattached_volume_between_container_snapshots_stays_foreign(self) -> None:
        owned_id = "1" * 64
        race_volume = "foreign-created-during-container-create"
        baseline = {"containers": {}, "volumes": {}, "networks": {}, "images": {}}
        current = {
            "containers": {owned_id: {"name": "restore-readiness-owned", "mounts": []}},
            "volumes": {race_volume: {"name": race_volume}},
            "networks": {},
            "images": {},
        }
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {
            "baseline": {"docker_inventory": baseline},
            "resource_creation_started": True,
            "containers": [
                {
                    "id": owned_id,
                    "name": "restore-readiness-owned",
                    # Old manifests recorded every volume created during the snapshot window.
                    "unexpected_volumes": [race_volume],
                    "unexpected_volume_mounts": [],
                }
            ],
            "pending_container": None,
            "networks": [],
        }
        drill.docker_resource_inventory = Mock(return_value=current)

        self.assertFalse(drill.verify_foreign_inventory())
        self.assertTrue(drill.manifest["foreign_inventory_changed"])

        drill.manifest["baseline"] = {"volumes": [], "networks": []}
        drill.manifest["cleanup_deadline"] = time.monotonic() + 10
        drill.cleanup_volume_inventory = Mock(return_value=[race_volume])
        calls: list[list[str]] = []

        def cleanup_call(args: list[str], **_kwargs: Any) -> tuple[int, bytes]:
            calls.append(args)
            return (0, b"")

        drill.cleanup_call = cleanup_call
        drill.cleanup_budget_available = Mock(return_value=True)

        self.assertFalse(drill.cleanup_networks_and_volumes())
        self.assertNotIn(["volume", "rm", race_volume], calls)

    def test_pending_container_recovery_does_not_claim_unattached_race_volume(self) -> None:
        document, record = container_fixture()
        pending = dict(record)
        pending.update(
            {
                "id": "",
                "volumes_before": [],
                "containers_before": [],
                "unexpected_volumes": [],
                "unexpected_volume_mounts": [],
                "unexpected_volume_records": [],
            }
        )
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {"pending_container": pending, "containers": []}
        drill.save = lambda: None
        drill.cleanup_volume_inventory = lambda: ["foreign-created-during-container-create"]
        drill.cleanup_container_inspect = lambda _identifier: document

        self.assertTrue(drill.cleanup_recover_pending_container([(record["id"], record["name"])]))
        self.assertEqual(drill.manifest["containers"][0]["unexpected_volumes"], [])
        self.assertEqual(drill.manifest["containers"][0]["unexpected_volume_records"], [])

    def test_local_dump_metadata_is_bounded(self) -> None:
        name = "pgdump-pre-" + "a" * 40 + ".sql"

        def file_info(
            *,
            mode: int = stat.S_IFREG | 0o600,
            uid: int = 0,
            gid: int = 0,
            links: int = 1,
            size: int = 4096,
        ) -> os.stat_result:
            return os.stat_result((mode, 1, 2, links, uid, gid, size, 0, 0, 0))

        self.assertTrue(RESTORE_READINESS.validate_local_dump_metadata(file_info(), name))
        deploy_name = "pgdump-pre-abcdef0-20261022T211333Z.sql"
        self.assertTrue(RESTORE_READINESS.validate_local_dump_metadata(file_info(), deploy_name))
        for info, candidate in (
            (file_info(mode=stat.S_IFLNK | 0o600), name),
            (file_info(uid=1), name),
            (file_info(gid=1), name),
            (file_info(mode=stat.S_IFREG | 0o640), name),
            (file_info(links=2), name),
            (file_info(size=0), name),
            (file_info(size=RESTORE_READINESS.MAX_LOCAL_DUMP_BYTES + 1), name),
            (file_info(), "pgdump-pre-" + "A" * 40 + ".sql"),
            (file_info(), "pgdump-pre-abcdef0-20261022T21133Z.sql"),
        ):
            with self.subTest(info=info, name=candidate):
                self.assertFalse(RESTORE_READINESS.validate_local_dump_metadata(info, candidate))

    def test_local_dump_walk_rejects_an_untrusted_root_directory_fd(self) -> None:
        name = "pgdump-pre-" + "a" * 40 + ".sql"
        path = Path("/tmp") / name
        cases = (
            (stat.S_IFDIR | 0o755, 1000, 0),
            (stat.S_IFDIR | 0o755, 0, 1000),
            (stat.S_IFDIR | 0o775, 0, 0),
            (stat.S_IFDIR | 0o757, 0, 0),
            (stat.S_IFREG | 0o755, 0, 0),
        )
        for mode, uid, gid in cases:
            info = os.stat_result((mode, 1, 2, 1, uid, gid, 0, 0, 0, 0))
            with (
                self.subTest(mode=oct(mode), uid=uid, gid=gid),
                patch.object(RESTORE_READINESS.os, "open", return_value=41) as open_directory,
                patch.object(RESTORE_READINESS.os, "fstat", return_value=info),
                patch.object(RESTORE_READINESS.os, "close") as close_directory,
                self.assertRaises(ValueError),
            ):
                RESTORE_READINESS.open_local_dump(path)

            open_directory.assert_called_once()
            self.assertEqual(open_directory.call_args.args[0], "/")
            close_directory.assert_called_once_with(41)

    def test_local_inventory_and_restore_contracts_are_bounded(self) -> None:
        inventory = RESTORE_READINESS.LOCAL_OBJECT_INVENTORY_SQL
        self.assertIn("LEFT JOIN pg_sequence AS seq ON seq.seqrelid = c.oid", inventory)
        for parameter in (
            "seqtypid",
            "seqstart",
            "seqincrement",
            "seqmax",
            "seqmin",
            "seqcache",
            "seqcycle",
        ):
            with self.subTest(parameter=parameter):
                self.assertIn(f"seq.{parameter}", inventory)

        source = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("ALTER SEQUENCE public.users_id_seq INCREMENT BY", source)
        self.assertIn("seqincrement::text FROM pg_sequence", source)

        self.assertIn("self.mount(self.local_dump_path, \"/run/local-dump/input.sql\", writable=False)", source)
        self.assertIn("--single-transaction --file=-", source)
        self.assertIn("\\\\restrict", source)
        self.assertIn("-- PostgreSQL database dump complete", source)
        self.assertIn("printf 'SELECT 1 / 0;\\n'", source)
        self.assertIn('entrypoint="/bin/sh"', source)
        self.assertIn("LIMIT {MAX_LOCAL_AUTH_KEYS}", source)
        self.assertIn("IF deleted_keys <> original_keys", source)
        self.assertIn("def validate_local_dump_argument", source)
        self.assertIn("LOCAL_OBJECT_INVENTORY_SQL", source)
        self.assertIn("COMMENT ON SCHEMA public IS", source)
        self.assertIn("obj_description(namespace.oid, 'pg_namespace')", source)
        self.assertIn('"synthetic_local_dump_fixture"}', source)
        self.assertIn("            LOCAL_DUMP_LEAK_MARKER,", source)
        self.assertIn('role="local_restore_client",\n            user="0:70"', source)
        self.assertIn('"local_restore_client": "0:70"', source)
        self.assertIn("MIGRATION_HEAD_RELATION_SAFE_SQL", source)
        self.assertIn("database_user=LOCAL_ATTESTATION_ROLE", source)
        self.assertIn("pinned_search_path=True", source)
        self.assertIn("restore_readiness_copy_to_program", source)
        self.assertIn("ALTER ROLE postgres SET search_path = public, pg_catalog", source)
        self.assertIn("CREATE FUNCTION public.md5(text)", source)

        tree = ast.parse(source)
        restore_method = next(
            node
            for node in ast.walk(tree)
            if isinstance(node, ast.FunctionDef) and node.name == "restore_local_dump"
        )
        assignment = next(
            node
            for node in restore_method.body
            if isinstance(node, ast.Assign)
            and any(isinstance(target, ast.Name) and target.id == "restore_script" for target in node.targets)
        )
        restore_script = ast.literal_eval(assignment.value)
        awk_program = restore_script.split("  if awk '\n", 1)[1].split('\n  \' "$dump_path"; then', 1)[0]
        valid_pg16_dump = (
            "SET statement_timeout = 0;\n"
            "--\n-- PostgreSQL database dump complete\n--\n\n"
        )
        valid_pg18_dump = (
            "\\restrict sourcekey\nSET statement_timeout = 0;\n"
            "--\n-- PostgreSQL database dump complete\n--\n\n"
            "\\unrestrict sourcekey\n\n"
        )
        hostile_dump = valid_pg18_dump.replace(
            "--\n-- PostgreSQL database dump complete",
            "\\unrestrict injectedkey\n--\n-- PostgreSQL database dump complete",
        )
        for dump, expected_status in (
            (valid_pg16_dump, 0),
            (valid_pg18_dump, 0),
            (hostile_dump, 42),
        ):
            with self.subTest(expected_status=expected_status):
                filtered = subprocess.run(
                    ["awk", awk_program, "-"],
                    input=dump,
                    text=True,
                    capture_output=True,
                    check=False,
                )
                self.assertEqual(filtered.returncode, expected_status)
                self.assertNotIn("\\unrestrict", filtered.stdout)

        # Blank lines are SQL content: function bodies and empty-string COPY rows must reach psql unchanged.
        body = "CREATE FUNCTION public.f() RETURNS integer LANGUAGE plpgsql AS $$\nBEGIN\n\n  RETURN 1;\nEND\n$$;\n"
        copy = "COPY public.t (note) FROM stdin;\n\nkept\n\\.\n"
        dump_with_blank_lines = valid_pg18_dump.replace(
            "SET statement_timeout = 0;\n",
            "SET statement_timeout = 0;\n" + body + copy,
        )
        filtered = subprocess.run(
            ["awk", awk_program, "-"],
            input=dump_with_blank_lines,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(filtered.returncode, 0)
        self.assertIn("SET statement_timeout = 0;\n" + body + copy, filtered.stdout)

    def test_local_schema_snapshot_uses_pinned_non_superuser_queries(self) -> None:
        target = {"id": "a" * 64}
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.local_admin_role = None

        def query(_database: dict[str, Any], statement: str, _phase: str, **_kwargs: Any) -> str:
            if statement == RESTORE_READINESS.LOCAL_OBJECT_INVENTORY_SQL:
                return "1|" + "a" * 32
            if statement == RESTORE_READINESS.ROLE_STATE_SQL:
                return json.dumps({"roles": [], "memberships": [], "settings": []})
            if statement in (RESTORE_READINESS.DATABASE_STATE_SQL, RESTORE_READINESS.EXTENSION_STATE_SQL):
                return "{}"
            raise AssertionError(f"unexpected local snapshot query: {statement}")

        drill.sql = Mock(side_effect=query)
        drill.local_schema_snapshot(target, "schema_validation")
        self.assertEqual(drill.sql.call_count, 4)
        for call in drill.sql.call_args_list:
            self.assertEqual(call.kwargs["database_user"], RESTORE_READINESS.LOCAL_ATTESTATION_ROLE)
            self.assertTrue(call.kwargs["pinned_search_path"])

    def test_local_schema_state_ignores_only_the_live_restore_admin_role(self) -> None:
        # The admin role exists on the target until promotion, which now runs after every local check.
        source = {"id": "a" * 64}
        target = {"id": "b" * 64}
        head = RESTORE_READINESS.HEAD_SCHEMA
        for target_roles, passes in (
            ("browser_acceptance_probe,restore_admin_0123456789abcdef", True),
            ("browser_acceptance_probe,restore_admin_0123456789abcdef,restore_readiness_hostile", False),
        ):
            with self.subTest(target_roles=target_roles):
                drill = object.__new__(RESTORE_READINESS.Drill)
                drill.local_admin_role = "restore_admin_0123456789abcdef"
                drill.local_schema_snapshot = Mock(return_value=("same",))
                drill.verify_database_settings = Mock()

                def query(database: dict[str, Any], statement: str, _phase: str, **_kwargs: Any) -> str:
                    if statement == RESTORE_READINESS.MIGRATION_HEAD_SQL:
                        return head
                    if statement == RESTORE_READINESS.FIXTURE_STATE_SQL:
                        roles = target_roles if database is target else "browser_acceptance_probe"
                        return f"5|3|{roles}|pg_stat_statements,plpgsql|0|0|0|0|0"
                    raise AssertionError(f"unexpected query: {statement}")

                drill.sql = Mock(side_effect=query)
                if passes:
                    drill.verify_local_schema_state(source, target, "schema_validation", expected_head=head)
                else:
                    with self.assertRaises(RESTORE_READINESS.DrillFailure):
                        drill.verify_local_schema_state(source, target, "schema_validation", expected_head=head)

    def test_server_settings_are_checked_only_after_promotion(self) -> None:
        # SHOW of server settings needs superuser; postgres stays demoted until every local check passed.
        target = {"id": "b" * 64}
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.databases = {"target": target}
        drill.local_admin_role = "restore_admin_0123456789abcdef"
        drill.target_db_password = "a" * 64
        events: list[str] = []

        def query(_database: dict[str, Any], statement: str, _phase: str, **kwargs: Any) -> str:
            if statement.startswith("SELECT rolsuper"):
                return "true|true|true"
            if "SUPERUSER CREATEDB" in statement:
                events.append("promote")
            if statement.startswith("DROP ROLE"):
                events.append("drop_admin")
            return ""

        drill.sql = Mock(side_effect=query)
        drill.verify_database_settings = Mock(side_effect=lambda *_args: events.append("settings"))
        drill.promote_local_restore_owner(target)
        self.assertEqual(events, ["promote", "drop_admin", "settings"])

        drill.local_admin_role = "restore_admin_0123456789abcdef"
        drill.verify_database_settings.reset_mock()
        drill.local_schema_snapshot = Mock(return_value=("same",))
        drill.sql = Mock(side_effect=lambda _db, statement, _phase, **_kw: (
            RESTORE_READINESS.HEAD_SCHEMA if statement == RESTORE_READINESS.MIGRATION_HEAD_SQL
            else "5|3|browser_acceptance_probe|pg_stat_statements,plpgsql|0|0|0|0|0"
        ))
        drill.verify_local_schema_state({"id": "a" * 64}, target, "schema_validation", expected_head=RESTORE_READINESS.HEAD_SCHEMA)
        drill.verify_database_settings.assert_not_called()

    def test_local_hostile_refusals_compare_the_normalized_snapshot(self) -> None:
        target = {"id": "a" * 64}
        baseline = ((1, "a" * 32), "normalized-roles", "database-state", "extension-state")
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.args = Mock(local_fixture_test=True)
        drill.databases = {"target": target}
        drill.local_object_inventory = Mock(return_value=baseline[0])
        drill.sql = Mock(
            side_effect=lambda _database, statement, _phase, **_kwargs: (
                "raw-roles" if statement == RESTORE_READINESS.ROLE_STATE_SQL else "{}"
            )
        )

        class ComparedCurrentSnapshot(Exception):
            pass

        snapshot_calls = 0

        def snapshot(_database: dict[str, Any], _phase: str) -> tuple[Any, ...]:
            nonlocal snapshot_calls
            snapshot_calls += 1
            if snapshot_calls == 3:
                raise ComparedCurrentSnapshot
            return baseline

        drill.local_schema_snapshot = Mock(side_effect=snapshot)
        drill.run_local_fixture_sql = Mock(return_value=(1, b""))

        with self.assertRaises(ComparedCurrentSnapshot):
            drill.run_local_hostile_fixture_tests({"id": "b" * 64}, target, RESTORE_READINESS.HEAD_SCHEMA)

        self.assertEqual(snapshot_calls, 3)

    def test_local_migration_head_is_guarded_by_catalog_shape_before_read(self) -> None:
        target = {"id": "b" * 64, "role": "target_postgres"}
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.databases = {"target": target}

        with tempfile.TemporaryDirectory() as temporary_directory:
            migration_dir = Path(temporary_directory) / "migrations"
            migration_dir.mkdir()
            (migration_dir / f"{RESTORE_READINESS.HEAD_SCHEMA}_fixture.sql").write_text("-- fixture\n")
            drill.run_dir = Path(temporary_directory)
            calls: list[tuple[str, dict[str, Any]]] = []
            outputs = iter(("false", "safe", "reviewed-shape", "reviewed-shape", RESTORE_READINESS.HEAD_SCHEMA))

            def query(_database: dict[str, Any], statement: str, _phase: str, **kwargs: Any) -> str:
                calls.append((statement, kwargs))
                return next(outputs)

            drill.sql = Mock(side_effect=query)
            self.assertEqual(
                drill.local_dump_migration_head({"id": "c" * 64}, target, "schema_validation"),
                RESTORE_READINESS.HEAD_SCHEMA,
            )
            self.assertEqual(
                [statement for statement, _ in calls],
                [
                    "SELECT rolsuper::text FROM pg_catalog.pg_roles WHERE rolname = CURRENT_USER;",
                    RESTORE_READINESS.MIGRATION_HEAD_RELATION_SAFE_SQL,
                    RESTORE_READINESS.MIGRATION_HEAD_RELATION_SHAPE_SQL,
                    RESTORE_READINESS.MIGRATION_HEAD_RELATION_SHAPE_SQL,
                    RESTORE_READINESS.MIGRATION_HEAD_SQL,
                ],
            )
            self.assertTrue(all(kwargs["pinned_search_path"] for _, kwargs in calls))
            self.assertEqual(calls[1][1]["database_user"], RESTORE_READINESS.LOCAL_ATTESTATION_ROLE)
            self.assertEqual(calls[-1][1]["database_user"], "postgres")

            drill.sql = Mock(side_effect=("false", "unsafe"))
            with self.assertRaises(RESTORE_READINESS.DrillFailure) as unsafe_relation:
                drill.local_dump_migration_head({"id": "c" * 64}, target, "schema_validation")
            self.assertEqual(unsafe_relation.exception.category, "schema_failure")
            self.assertEqual(drill.sql.call_count, 2)

    def test_local_success_report_adds_only_its_mode_marker(self) -> None:
        report = RESTORE_READINESS.report_document(
            "application_assertions",
            "assertions_passed",
            "verified",
            3,
            migration_head=RESTORE_READINESS.HEAD_SCHEMA,
            help_get_config="pass",
            synthetic_signin="pass",
            auth_keys_cleared="pass",
            input_mode="local",
        )
        self.assertEqual(set(report) - {"input_mode"}, {
            "result",
            "migration_head",
            "help_getConfig",
            "synthetic_signin",
            "auth_keys_cleared",
            "cleanup",
            "elapsed_ms",
            "exit",
        })
        self.assertEqual(report["input_mode"], "local")

    def test_auth_key_inventory_and_bounded_fixture_transactions(self) -> None:
        inventory = RESTORE_READINESS.load_auth_key_inventory()
        self.assertEqual(inventory["base_commit"], "bdb89970bbd40fb57fdb89f3aa8fafb78925ce93")
        self.assertEqual(inventory["schema_head"], "20261021000004")
        self.assertIn(inventory["schema_head"], RESTORE_READINESS.repo_migration_versions())
        self.assertEqual(
            inventory["trigger_tables"],
            [
                "public.auth_keys",
                "public.browser_acceptance_binding_evidence",
                "public.notify_settings_difference_cursors",
                "public.srp_challenges",
            ],
        )
        for table in inventory["trigger_tables"]:
            self.assertIn(f"'{table}'::regclass", RESTORE_READINESS.AUTH_KEY_INVENTORY_SQL)
        self.assertIn(
            "tg.tgrelid IN (SELECT relation FROM clearing_relations)",
            RESTORE_READINESS.AUTH_KEY_INVENTORY_SQL,
        )
        self.assertEqual(
            {item["name"] for item in inventory["foreign_keys"]},
            {
                "auth_keys_pending_user_human_fk",
                "auth_keys_pending_user_id_fkey",
                "auth_keys_user_human_fk",
                "auth_keys_user_id_fkey",
                "notify_settings_difference_cursors_auth_key_id_fkey",
                "srp_challenges_auth_key_id_fkey",
            },
        )
        self.assertIn(
            {
                "name": "auth_keys_pending_user_human_fk",
                "table": "public.auth_keys",
                "columns": ["pending_user_id", "pending_user_kind"],
                "references": "public.users",
                "referenced_columns": ["id", "kind"],
                "on_delete": "NO ACTION",
                "on_update": "NO ACTION",
                "match": "SIMPLE",
                "deferrable": False,
                "initially_deferred": False,
                "validated": True,
            },
            inventory["foreign_keys"],
        )
        self.assertIn(
            {
                "name": "auth_keys_user_human_fk",
                "table": "public.auth_keys",
                "columns": ["user_id", "user_kind"],
                "references": "public.users",
                "referenced_columns": ["id", "kind"],
                "on_delete": "NO ACTION",
                "on_update": "NO ACTION",
                "match": "SIMPLE",
                "deferrable": False,
                "initially_deferred": False,
                "validated": True,
            },
            inventory["foreign_keys"],
        )
        self.assertEqual(
            {item["name"] for item in inventory["triggers"]},
            {
                "AA_browser_acceptance_binding_evidence",
                "auth_keys_mark_ever_bound",
                "browser_acceptance_auth_keys_reject_truncate",
            },
        )
        self.assertIn(
            {
                "name": "auth_keys_mark_ever_bound",
                "table": "public.auth_keys",
                "timing": "BEFORE",
                "level": "ROW",
                "events": ["INSERT", "UPDATE"],
                "enabled": "ORIGIN",
                "function": "public.auth_keys_mark_ever_bound",
            },
            inventory["triggers"],
        )
        self.assertEqual(
            RESTORE_READINESS.validate_synthetic_auth_key_ids((-101, -202, -303)),
            (-101, -202, -303),
        )
        for invalid in ((-1, -1, -2), (1, -2, -3), (-1, -2, "-3"), (-1, -2), (-(1 << 63), -2, -3)):
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                RESTORE_READINESS.validate_synthetic_auth_key_ids(invalid)

        fixture_sql = RESTORE_READINESS.synthetic_auth_key_fixture_sql((-101, -202, -303))
        self.assertIn("public.browser_acceptance_enroll_binding_evidence", fixture_sql)
        self.assertIn("INSERT INTO public.srp_challenges", fixture_sql)
        self.assertIn("INSERT INTO public.notify_settings_difference_cursors", fixture_sql)
        self.assertIn("decode(repeat('a5', 256), 'hex')", fixture_sql)
        self.assertNotIn("SELECT key_value", fixture_sql)

        local_fixture_sql = RESTORE_READINESS.local_unenrolled_auth_key_fixture_sql((-404, -505), "a" * 32)
        self.assertIn("INSERT INTO public.users", local_fixture_sql)
        self.assertIn("INSERT INTO public.auth_keys", local_fixture_sql)
        self.assertIn("ever_bound_at, pending_user_id", local_fixture_sql)
        self.assertIn("clock_timestamp()", local_fixture_sql)
        self.assertNotIn("browser_acceptance_enroll_binding_evidence", local_fixture_sql)
        self.assertIn(
            "JOIN public.browser_acceptance_binding_evidence AS evidence\n  ON evidence.user_id = auth_key.user_id",
            RESTORE_READINESS.AUTH_KEY_EVIDENCE_BOUND_COUNT_SQL,
        )
        self.assertIn(
            "LEFT JOIN public.browser_acceptance_binding_evidence AS evidence",
            RESTORE_READINESS.UNENROLLED_BOUND_AUTH_KEY_COUNT_SQL,
        )
        with self.assertRaises(ValueError):
            RESTORE_READINESS.local_unenrolled_auth_key_fixture_sql((-404, -404), "a" * 32)

        local_transaction = RESTORE_READINESS.local_auth_key_clear_transaction_sql()
        self.assertIn("INTO evidence_bound_keys", local_transaction)
        self.assertIn("original_removals + evidence_bound_keys", local_transaction)
        self.assertIn(
            "JOIN public.browser_acceptance_binding_evidence AS evidence",
            local_transaction,
        )
        local_interrupted = RESTORE_READINESS.local_auth_key_clear_transaction_sql(wait_for_signal=True)
        self.assertLess(
            local_interrupted.index("\\echo RESTORE_READINESS_AUTH_KEY_DELETE_APPLIED"),
            local_interrupted.index("pg_sleep(30)"),
        )
        self.assertLess(local_interrupted.index("pg_sleep(30)"), local_interrupted.index("COMMIT"))

        transaction = RESTORE_READINESS.auth_key_clear_transaction_sql((-101, -202, -303))
        self.assertIn("LOCK TABLE public.auth_keys, public.srp_challenges", transaction)
        self.assertLess(transaction.index("auth-key clearing precondition mismatch"), transaction.index("DELETE FROM public.auth_keys"))
        self.assertIn("DELETE FROM public.auth_keys WHERE id IN (-101, -202, -303)", transaction)
        self.assertIn("SELECT count(*) INTO remaining_auth_keys FROM public.auth_keys", transaction)
        self.assertLess(transaction.index("remaining_auth_keys <> 0"), transaction.index("COMMIT"))
        self.assertNotIn("TRUNCATE", transaction.upper())
        self.assertNotIn("CASCADE", transaction.upper())
        self.assertNotIn("DISABLE TRIGGER", transaction.upper())

        failed = RESTORE_READINESS.auth_key_clear_transaction_sql((-101, -202, -303), inject_failure=True)
        self.assertLess(failed.index("DELETE FROM public.auth_keys"), failed.index("SELECT 1 / 0"))
        self.assertLess(failed.index("DELETE FROM public.auth_keys"), failed.index("\\echo RESTORE_READINESS_AUTH_KEY_DELETE_APPLIED"))
        self.assertLess(failed.index("remaining_auth_keys <> 0"), failed.index("\\echo RESTORE_READINESS_AUTH_KEY_DELETE_APPLIED"))
        self.assertLess(failed.index("SELECT 1 / 0"), failed.index("COMMIT"))
        interrupted = RESTORE_READINESS.auth_key_clear_transaction_sql((-101, -202, -303), wait_for_signal=True)
        self.assertLess(interrupted.index("DELETE FROM public.auth_keys"), interrupted.index("pg_sleep(30)"))
        self.assertLess(interrupted.index("\\echo RESTORE_READINESS_AUTH_KEY_DELETE_APPLIED"), interrupted.index("pg_sleep(30)"))
        self.assertLess(interrupted.index("pg_sleep(30)"), interrupted.index("COMMIT"))

    def test_auth_key_clear_fault_results_require_acknowledged_rollback(self) -> None:
        before = ["3", "2", "3", "1", "2", "0"]
        ack = RESTORE_READINESS.AUTH_KEY_CLEAR_ACK + b"\n"
        expected_error = b"ERROR:  division by zero\n"

        self.assertEqual(
            RESTORE_READINESS.auth_key_clear_failure_diagnostics(
                before,
                before,
                client_status=3,
                container_status=3,
                stdout=ack,
                stderr=expected_error,
            ),
            {
                "acknowledged": True,
                "client_status": 3,
                "container_status": 3,
                "rollback_preserved": True,
                "division_by_zero_reported": True,
            },
        )
        self.assertEqual(
            RESTORE_READINESS.auth_key_clear_failure_diagnostics(
                before,
                before,
                client_status=3,
                container_status=3,
                stdout=ack,
                stderr=b"ERROR: unrelated SQL failure\n",
            )["division_by_zero_reported"],
            False,
        )
        shell = SHELL_GATE.read_text(encoding="utf-8")
        self.assertIn(
            "auth_clear_failure_report=$(run_drill --fault-test auth-clear-failure)",
            shell,
        )
        self.assertNotIn(
            "auth_clear_failure_report=$(run_drill --fault-test auth-clear-failure 2>/dev/null)",
            shell,
        )
        self.assertIn(
            "restore readiness auth-key failure drill returned unexpected report:",
            shell,
        )

        self.assertEqual(
            RESTORE_READINESS.auth_key_clear_fault_category(
                "failure",
                before,
                before,
                client_status=3,
                container_status=3,
                stdout=ack,
                stderr=expected_error,
            ),
            "auth_key_clear_failure",
        )
        for result in (
            RESTORE_READINESS.auth_key_clear_fault_category(
                "failure",
                before,
                ["0", "0", "0", "1", "2", "2"],
                client_status=3,
                container_status=3,
                stdout=ack,
                stderr=expected_error,
            ),
            RESTORE_READINESS.auth_key_clear_fault_category(
                "failure",
                before,
                ["0", "0", "0", "1", "2", "2"],
                client_status=0,
                container_status=0,
                stdout=ack,
            ),
            RESTORE_READINESS.auth_key_clear_fault_category(
                "failure",
                before,
                before,
                client_status=3,
                container_status=3,
                stdout=ack,
                stderr=b"ERROR: unrelated SQL failure\n",
            ),
            RESTORE_READINESS.auth_key_clear_fault_category(
                "failure",
                before,
                before,
                client_status=3,
                container_status=3,
                stderr=expected_error,
            ),
        ):
            self.assertEqual(result, "fault_test_failure")

        self.assertEqual(
            RESTORE_READINESS.auth_key_clear_fault_category(
                "signal", before, before, interrupt_acknowledged=True
            ),
            "interrupted",
        )
        self.assertEqual(
            RESTORE_READINESS.auth_key_clear_fault_category(
                "signal", before, before, interrupt_acknowledged=False
            ),
            "fault_test_failure",
        )
        self.assertEqual(
            RESTORE_READINESS.auth_key_clear_fault_category(
                "signal", before, ["0", "0", "0", "1", "2", "2"], interrupt_acknowledged=True
            ),
            "fault_test_failure",
        )

    def test_attached_interrupt_is_sent_only_after_transaction_acknowledgment(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            fake_docker = Path(temporary_directory) / "docker"
            fake_docker.write_text(
                "#!/usr/bin/python3\n"
                "import os, sys, time\n"
                "sys.stdin.buffer.read()\n"
                "if os.environ.get('ACK_MODE') == 'ack':\n"
                f"    os.write(1, {RESTORE_READINESS.AUTH_KEY_CLEAR_ACK!r} + b'\\n')\n"
                "time.sleep(10)\n",
                encoding="utf-8",
            )
            fake_docker.chmod(0o700)

            for ack_mode, expected_ack in (("missing", False), ("ack", True)):
                drill = object.__new__(RESTORE_READINESS.Drill)
                drill.executable = str(fake_docker)
                drill.environment = {"PATH": "/usr/bin:/bin", "ACK_MODE": ack_mode}
                drill.manifest = {"work_deadline": time.monotonic() + 10, "processes": []}
                drill.attest_container = Mock(return_value={"State": {"Running": True}})
                drill._track_child = lambda _process: {}
                drill._untrack_child = Mock()

                parent_pid = RESTORE_READINESS.os.getpid()
                real_kill = RESTORE_READINESS.os.kill
                parent_signals: list[tuple[int, int]] = []

                def record_parent_signal(pid: int, signum: int) -> None:
                    if pid == parent_pid:
                        parent_signals.append((pid, signum))
                    else:
                        real_kill(pid, signum)

                with patch.object(RESTORE_READINESS.os, "kill", side_effect=record_parent_signal):
                    with self.assertRaises(RESTORE_READINESS.DrillFailure) as timeout_failure:
                        drill.attached(
                            {"id": "f" * 64},
                            "auth_key_clearing",
                            "fault_test_failure",
                            input_bytes=b"transaction",
                            interrupt_signal=RESTORE_READINESS.signal.SIGTERM,
                            interrupt_ack=RESTORE_READINESS.AUTH_KEY_CLEAR_ACK,
                            timeout=1,
                        )
                self.assertEqual(timeout_failure.exception.category, "timeout")
                self.assertEqual(drill.last_attached_interrupt_acknowledged, expected_ack)
                if expected_ack:
                    self.assertEqual(
                        parent_signals,
                        [(parent_pid, RESTORE_READINESS.signal.SIGTERM)],
                    )
                else:
                    self.assertEqual(parent_signals, [])

    def test_auth_key_clear_refuses_wrong_target_and_missing_marker_before_sql(self) -> None:
        target = {"id": "a" * 64, "role": "target_postgres", "removed": False}
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.input_mode = "synthetic"
        drill.phase = "auth_key_clearing"
        drill.databases = {"target": target}
        drill.manifest = {"containers": [target], "processes": [], "files": []}
        drill.post_restore_marker = None
        drill.auth_keys_cleared = False
        drill.synthetic_auth_key_ids = (-101, -202, -303)
        drill.sql = Mock()
        drill.auth_key_clear_client = Mock()

        with tempfile.TemporaryDirectory() as temporary_directory:
            drill.run_dir = Path(temporary_directory)
            with self.assertRaises(RESTORE_READINESS.DrillFailure) as missing_marker:
                drill.clear_auth_keys(target)
            self.assertEqual(missing_marker.exception.phase, "auth_key_clearing")
            self.assertEqual(missing_marker.exception.category, "isolation_refusal")
            drill.sql.assert_not_called()
            drill.auth_key_clear_client.assert_not_called()

        with self.assertRaises(RESTORE_READINESS.DrillFailure) as wrong_target:
            drill.clear_auth_keys({**target})
        self.assertEqual(wrong_target.exception.category, "isolation_refusal")
        drill.sql.assert_not_called()
        drill.auth_key_clear_client.assert_not_called()

    def test_dependent_table_trigger_refuses_auth_key_clear_before_write(self) -> None:
        expected_inventory = RESTORE_READINESS.load_auth_key_inventory()
        for table in expected_inventory["trigger_tables"]:
            if table == "public.auth_keys":
                continue
            with self.subTest(table=table):
                actual_inventory = json.loads(json.dumps(expected_inventory))
                actual_inventory["triggers"].append(
                    {"name": "unexpected_dependent_trigger", "table": table}
                )
                target = {"id": "a" * 64, "role": "target_postgres", "removed": False}
                marker = {
                    "input_mode": "synthetic",
                    "target_container_id": target["id"],
                    "migration_head": RESTORE_READINESS.HEAD_SCHEMA,
                    "database_identity": "fixture-db",
                    "table_counts": ["fixture-counts"],
                }
                drill = object.__new__(RESTORE_READINESS.Drill)
                drill.input_mode = "synthetic"
                drill.phase = "auth_key_clearing"
                drill.databases = {"target": target}
                drill.manifest = {"containers": [target], "processes": []}
                drill.post_restore_marker = marker
                drill.auth_keys_cleared = False
                drill.synthetic_auth_key_ids = (-101, -202, -303)
                drill.run_dir = Path("/unused")
                drill.private_read = Mock(return_value=json.dumps(marker).encode())
                drill.attest_container = Mock(return_value={"State": {"Running": True}})
                drill.table_counts = Mock(return_value=([], marker["table_counts"]))
                drill.auth_key_clear_client = Mock()

                def read_sql(_target: Any, statement: str, _phase: str, **_kwargs: Any) -> str:
                    if statement == "SELECT current_database();":
                        return "teagram"
                    if statement == RESTORE_READINESS.MIGRATION_HEAD_SQL:
                        return RESTORE_READINESS.HEAD_SCHEMA
                    if statement == RESTORE_READINESS.DATABASE_IDENTITY_SQL:
                        return marker["database_identity"]
                    if statement == RESTORE_READINESS.AUTH_KEY_INVENTORY_SQL:
                        return json.dumps(actual_inventory)
                    raise AssertionError(f"unexpected query before inventory refusal: {statement}")

                drill.sql = Mock(side_effect=read_sql)

                with self.assertRaises(RESTORE_READINESS.DrillFailure) as failure:
                    drill.clear_auth_keys(target)

                self.assertEqual(failure.exception.phase, "auth_key_clearing")
                self.assertEqual(failure.exception.category, "schema_failure")
                drill.auth_key_clear_client.assert_not_called()
                self.assertTrue(
                    all(
                        call.args[1].lstrip().upper().startswith(("SELECT", "WITH"))
                        for call in drill.sql.call_args_list
                    )
                )

    def test_interrupted_auth_key_client_is_cancelled_not_terminated(self) -> None:
        # psql is PID 1 in the clearer, so it ignores SIGTERM; only SIGINT (query cancel) or SIGKILL stops it.
        record = {"id": "d" * 64, "removed": False}
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.save = Mock()
        running = [True]
        calls: list[list[str]] = []

        def cleanup_call(args: list[str], *, timeout: float = 30, input_bytes: bytes | None = None):
            calls.append(args)
            if args[0] == "kill":
                if args[1] in {"--signal=SIGINT", "--signal=SIGKILL"}:
                    running[0] = False
                return 0, b""
            if args[0] == "wait":
                return (1, b"") if running[0] else (0, b"3\n")
            if args[0] == "inspect":
                return 0, b"true\n" if running[0] else b"false\n"
            return 0, b""

        drill.cleanup_call = cleanup_call
        drill.remove_interrupted_auth_key_client(record)

        self.assertEqual(calls[0], ["kill", "--signal=SIGINT", record["id"]])
        self.assertNotIn(["kill", "--signal=SIGTERM", record["id"]], calls)
        self.assertNotIn(["kill", "--signal=SIGKILL", record["id"]], calls)
        self.assertTrue(record["removed"])

    def test_interrupted_auth_key_client_escalates_to_sigkill_when_cancel_is_ignored(self) -> None:
        record = {"id": "e" * 64, "removed": False}
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.save = Mock()
        running = [True]
        calls: list[list[str]] = []

        def cleanup_call(args: list[str], *, timeout: float = 30, input_bytes: bytes | None = None):
            calls.append(args)
            if args[0] == "kill":
                if args[1] == "--signal=SIGKILL":
                    running[0] = False
                return 0, b""
            if args[0] == "wait":
                return (1, b"") if running[0] else (0, b"137\n")
            if args[0] == "inspect":
                return 0, b"true\n" if running[0] else b"false\n"
            return 0, b""

        drill.cleanup_call = cleanup_call
        drill.remove_interrupted_auth_key_client(record)

        self.assertIn(["kill", "--signal=SIGKILL", record["id"]], calls)
        self.assertLess(
            calls.index(["kill", "--signal=SIGINT", record["id"]]),
            calls.index(["kill", "--signal=SIGKILL", record["id"]]),
        )
        self.assertTrue(record["removed"])

    def test_failure_report_is_allowlisted_and_does_not_include_raw_errors(self) -> None:
        report = RESTORE_READINESS.report_document(
            "restore",
            "restore_failure",
            "verified",
            7,
            migration_head=RESTORE_READINESS.BASE_SCHEMA,
        )
        self.assertEqual(
            set(report),
            {
                "result",
                "phase",
                "category",
                "migration_head",
                "help_getConfig",
                "synthetic_signin",
                "cleanup",
                "elapsed_ms",
                "exit",
            },
        )
        self.assertEqual(report["category"], "restore_failure")
        self.assertNotIn("raw", str(report))
        later_migration_failure = RESTORE_READINESS.report_document(
            "schema_validation",
            "schema_failure",
            "verified",
            7,
            migration_head="20261022000002",
        )
        self.assertEqual(later_migration_failure["migration_head"], "20261022000002")
        with self.assertRaises(ValueError):
            RESTORE_READINESS.report_document(
                "schema_validation", "schema_failure", "verified", 7, migration_head="20990101000000"
            )
        with self.assertRaises(ValueError):
            RESTORE_READINESS.report_document("restore", "raw-database-error", "verified", 7)

    def test_container_attestation_accepts_confined_configuration(self) -> None:
        document, record = container_fixture()
        self.assertTrue(RESTORE_READINESS.validate_container_inspect(document, record))

    def test_container_attestation_rejects_root_access_and_drift(self) -> None:
        document, record = container_fixture()
        document["Config"]["User"] = "0:0"
        self.assertFalse(RESTORE_READINESS.validate_container_inspect(document, record))

        document, record = container_fixture()
        document["HostConfig"]["LogConfig"] = {"Type": "json-file"}
        self.assertFalse(RESTORE_READINESS.validate_container_inspect(document, record))

        document, record = container_fixture()
        document["NetworkSettings"]["Networks"] = {"unexpected": {"IPAddress": "172.20.0.2"}}
        self.assertFalse(RESTORE_READINESS.validate_container_inspect(document, record))

        document, record = container_fixture()
        document["Mounts"] = [{"Type": "volume", "Name": "unexpected", "Source": "/var/lib/docker", "Destination": "/data", "RW": True}]
        self.assertFalse(RESTORE_READINESS.validate_container_inspect(document, record))

    def test_container_attestation_rejects_cpu_and_stdin_drift(self) -> None:
        document, record = container_fixture()
        document["HostConfig"].update({"NanoCpus": 1_000_000_000, "CpuQuota": 0, "CpuPeriod": 0})
        self.assertFalse(RESTORE_READINESS.validate_container_inspect(document, record))

        document, record = container_fixture()
        document["Config"]["AttachStdin"] = True
        self.assertFalse(RESTORE_READINESS.validate_container_inspect(document, record))

    def test_created_container_passes_its_own_attestation(self) -> None:
        document, record = container_fixture()
        document["HostConfig"]["ShmSize"] = 16 * 1024 * 1024
        drill = RESTORE_READINESS.Drill.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {"token": record["token"], "containers": []}
        drill.save = lambda: None
        drill.list_containers = lambda phase: []
        drill.list_volumes = lambda phase: set()
        drill.list_networks = lambda phase: {"none"}
        drill.inspect_container = lambda identifier, phase: created_document
        created_args: list[str] = []

        def create(args: list[str], **_kwargs: Any) -> bytes:
            created_args.extend(args)
            # Docker stores the requested CPU flags verbatim, so derive the inspect view from them.
            host = document["HostConfig"]
            host["CpuQuota"] = int(args[args.index("--cpu-quota") + 1]) if "--cpu-quota" in args else 0
            host["CpuPeriod"] = int(args[args.index("--cpu-period") + 1]) if "--cpu-period" in args else 0
            host["NanoCpus"] = int(float(args[args.index("--cpus") + 1]) * 1e9) if "--cpus" in args else 0
            return (record["id"] + "\n").encode()

        created_document = document
        drill.call = create
        created = drill.create_container(
            role=record["role"],
            image_id=record["image_id"],
            name=record["name"],
            user=record["user"],
            memory=record["memory"],
            pids_limit=record["pids_limit"],
            mounts=[],
            tmpfs_options=record["tmpfs_options"],
            environment={"LANG": "C", "PATH": "/usr/bin:/bin"},
            explicit_environment={},
            command_args=[],
            expected_entrypoint=record["entrypoint"],
            expected_command=record["command"],
            phase="fixture_generation",
            fixture_check=None,
        )
        self.assertTrue(RESTORE_READINESS.validate_container_inspect(document, created))
        self.assertNotIn("--cpus", created_args)

    def test_target_container_creation_failure_keeps_restore_phase_and_drops_stale_fixture_check(self) -> None:
        drill = RESTORE_READINESS.Drill.__new__(RESTORE_READINESS.Drill)
        drill.phase = "restore"
        drill.fixture_check = "source_auth_key_fixture"
        drill.images = {}
        drill.manifest = {"token": "c" * 32, "containers": []}
        drill.image_environment = lambda _image: ({}, {})
        drill.save = lambda: None
        drill.list_containers = lambda _phase: []
        drill.list_volumes = lambda _phase: set()
        drill.list_networks = lambda _phase: set()

        def fail_create(_args: list[str], *, phase: str, category: str, **kwargs: Any) -> bytes:
            raise RESTORE_READINESS.DrillFailure(
                phase,
                category,
                fixture_check=kwargs.get("fixture_check"),
            )

        drill.call = fail_create

        with self.assertRaises(RESTORE_READINESS.DrillFailure) as failure:
            drill.image_container(
                "sha256:" + "b" * 64,
                role="target_client",
                user="65532:70",
                memory=RESTORE_READINESS.CLIENT_MEMORY,
                pids_limit=96,
                mounts=[],
                tmpfs_options={},
            )

        self.assertEqual(failure.exception.phase, "restore")
        self.assertIsNone(failure.exception.fixture_check)

    def test_created_container_does_not_claim_unattached_race_volume(self) -> None:
        document, record = container_fixture()
        drill = RESTORE_READINESS.Drill.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {"token": record["token"], "containers": []}
        drill.save = lambda: None
        drill.list_containers = lambda _phase: []
        volume_snapshots = iter((set(), {"foreign-created-during-container-create"}))
        drill.list_volumes = lambda _phase: next(volume_snapshots)
        drill.list_networks = lambda _phase: {"none"}
        drill.call = lambda _args, **_kwargs: (record["id"] + "\n").encode()
        drill.inspect_container = lambda _identifier, _phase: document

        with self.assertRaises(RESTORE_READINESS.DrillFailure) as failure:
            drill.create_container(
                role=record["role"],
                image_id=record["image_id"],
                name=record["name"],
                user=record["user"],
                memory=record["memory"],
                pids_limit=record["pids_limit"],
                mounts=[],
                tmpfs_options=record["tmpfs_options"],
                environment={"LANG": "C", "PATH": "/usr/bin:/bin"},
                explicit_environment={},
                command_args=[],
                expected_entrypoint=record["entrypoint"],
                expected_command=record["command"],
                phase="fixture_generation",
                fixture_check=None,
            )

        self.assertEqual(failure.exception.category, "isolation_refusal")
        self.assertEqual(drill.manifest["containers"][0]["unexpected_volumes"], [])
        self.assertEqual(drill.manifest["containers"][0]["unexpected_volume_records"], [])

    def test_finished_run_removal_accepts_directories_already_cleaned(self) -> None:
        class RootOwnedPath(type(Path())):
            def lstat(self) -> os.stat_result:  # type: ignore[override]
                info = super().lstat()
                return os.stat_result((info.st_mode, info.st_ino, info.st_dev, info.st_nlink, 0, 0, info.st_size, 0, 0, 0))

        for layout in ("emptied", "removed", "leftover"):
            with self.subTest(layout=layout), tempfile.TemporaryDirectory() as temporary_directory:
                run_dir = Path(temporary_directory) / "run"
                run_dir.mkdir(mode=0o700)
                manifest_path = RootOwnedPath(run_dir / "ownership.json")
                manifest_path.write_text("{}", encoding="utf-8")
                manifest_path.chmod(0o600)
                if layout != "removed":
                    for name in ("work", "invocation", "seed", "secrets", "migrations", "sockets/source", "sockets/target"):
                        (run_dir / name).mkdir(parents=True)
                if layout == "leftover":
                    (run_dir / "secrets" / "left").write_bytes(b"x")
                drill = RESTORE_READINESS.Drill.__new__(RESTORE_READINESS.Drill)
                drill.run_dir = run_dir
                drill.manifest_path = manifest_path
                drill.manifest = {"cleanup_verified": True}

                removed = RESTORE_READINESS.remove_finished_run(drill)

                self.assertEqual(removed, layout != "leftover")
                self.assertEqual(run_dir.exists(), layout == "leftover")

    def test_only_one_shot_provisioners_may_be_root_with_secret_mounts(self) -> None:
        document, record = container_fixture()
        provisioner_mounts = [
            {"type": "bind", "source": f"/run/secrets/{name}", "destination": f"/run/secrets/{name}", "rw": False}
            for name in ("postgres.dsn", "authkey.hex", "username", "password")
        ]
        record.update(
            {
                "role": "checker_provisioner",
                "user": "0:0",
                "entrypoint": ["/run/app/restore-readiness-provisioner"],
                "mounts": provisioner_mounts,
                "network_mode": "container:" + "d" * 64,
            }
        )
        document["Config"]["User"] = "0:0"
        document["Config"]["Entrypoint"] = record["entrypoint"]
        document["Config"]["Labels"][RESTORE_READINESS.IMAGE_ROLE_LABEL] = "checker_provisioner"
        document["HostConfig"]["NetworkMode"] = record["network_mode"]
        document["Mounts"] = [
            {"Type": "bind", "Source": item["source"], "Destination": item["destination"], "RW": False}
            for item in provisioner_mounts
        ]
        self.assertTrue(RESTORE_READINESS.validate_container_inspect(document, record))

        extra_document = json.loads(json.dumps(document))
        extra_record = json.loads(json.dumps(record))
        extra_mount = {"type": "bind", "source": "/run/postgres", "destination": "/run/postgres", "rw": True}
        extra_record["mounts"].append(extra_mount)
        extra_document["Mounts"].append(
            {"Type": "bind", "Source": "/run/postgres", "Destination": "/run/postgres", "RW": True}
        )
        self.assertFalse(RESTORE_READINESS.validate_container_inspect(extra_document, extra_record))

        wrong_network_document = json.loads(json.dumps(document))
        wrong_network_record = json.loads(json.dumps(record))
        wrong_network_document["HostConfig"]["NetworkMode"] = "none"
        wrong_network_record["network_mode"] = "none"
        self.assertFalse(RESTORE_READINESS.validate_container_inspect(wrong_network_document, wrong_network_record))

        seed_document, seed_record = container_fixture()
        seed_record.update(
            {
                "role": "source_seed",
                "user": "0:0",
                "entrypoint": ["/run/app/restore-readiness-seed-shim"],
                "mounts": provisioner_mounts,
                "network_mode": "container:" + "e" * 64,
                "tmpfs_options": {
                    "/run/postgres": "rw,nosuid,nodev,noexec,size=8m,uid=0,gid=0,mode=0700",
                    "/tmp": "rw,nosuid,nodev,noexec,size=64m,uid=0,gid=0,mode=1777",
                },
            }
        )
        seed_document["Config"]["User"] = "0:0"
        seed_document["Config"]["Entrypoint"] = seed_record["entrypoint"]
        seed_document["Config"]["Labels"][RESTORE_READINESS.IMAGE_ROLE_LABEL] = "source_seed"
        seed_document["HostConfig"]["NetworkMode"] = seed_record["network_mode"]
        seed_document["HostConfig"]["Tmpfs"] = seed_record["tmpfs_options"]
        seed_document["Mounts"] = [
            {"Type": "bind", "Source": item["source"], "Destination": item["destination"], "RW": False}
            for item in provisioner_mounts
        ]
        self.assertTrue(RESTORE_READINESS.validate_container_inspect(seed_document, seed_record))

    def test_local_restore_client_is_root_in_the_socket_group(self) -> None:
        # Root owns the 0600 dump; the scratch socket directory is 70:70 0770 and no capability overrides it.
        document, record = container_fixture()
        mounts = [
            {"type": "bind", "source": "/run/dump.sql", "destination": "/run/local-dump/input.sql", "rw": False},
            {"type": "bind", "source": "/run/sockets/target", "destination": "/run/postgres", "rw": True},
        ]
        record.update(
            {
                "role": "local_restore_client",
                "user": "0:70",
                "entrypoint": ["/bin/sh"],
                "mounts": mounts,
                "network_mode": "container:" + "d" * 64,
            }
        )
        document["Config"]["User"] = "0:70"
        document["Config"]["Entrypoint"] = record["entrypoint"]
        document["Config"]["Labels"][RESTORE_READINESS.IMAGE_ROLE_LABEL] = "local_restore_client"
        document["HostConfig"]["NetworkMode"] = record["network_mode"]
        document["Mounts"] = [
            {"Type": "bind", "Source": item["source"], "Destination": item["destination"], "RW": item["rw"]}
            for item in mounts
        ]
        self.assertTrue(RESTORE_READINESS.validate_container_inspect(document, record))

        for user in ("0:0", "65532:70"):
            wrong_document = json.loads(json.dumps(document))
            wrong_record = json.loads(json.dumps(record))
            wrong_document["Config"]["User"] = user
            wrong_record["user"] = user
            self.assertFalse(RESTORE_READINESS.validate_container_inspect(wrong_document, wrong_record))

    def test_application_checker_must_stay_non_root(self) -> None:
        document, record = container_fixture()
        record.update({"role": "checker", "user": "0:0"})
        document["Config"]["User"] = "0:0"
        document["Config"]["Labels"][RESTORE_READINESS.IMAGE_ROLE_LABEL] = "checker"
        self.assertFalse(RESTORE_READINESS.validate_container_inspect(document, record))

    def test_provisioner_result_contract_rejects_ambiguous_or_mismatched_identity(self) -> None:
        valid = b'{"schema_version":1,"category":"provisioned","user_id":42,"username":"fixture_0123456789abcdef"}\n'
        category, identity = RESTORE_READINESS.parse_provisioner_result(valid, 0, "fixture_0123456789abcdef")
        self.assertEqual(category, "provisioned")
        self.assertEqual(identity, {"schema_version": 1, "user_id": 42, "username": "fixture_0123456789abcdef"})

        collision = b'{"schema_version":1,"category":"identity_collision"}\n'
        self.assertEqual(
            RESTORE_READINESS.parse_provisioner_result(collision, 1, "fixture_0123456789abcdef"),
            ("identity_collision", None),
        )
        for stdout, status in (
            (b'{"schema_version":1,"schema_version":1,"category":"provisioned","user_id":42,"username":"fixture_0123456789abcdef"}', 0),
            (b'{"schema_version":1,"category":"provisioned","user_id":true,"username":"fixture_0123456789abcdef"}', 0),
            (b'{"schema_version":1,"category":"provisioned","user_id":42,"username":"other_fixture"}', 0),
            (b'{"schema_version":1,"category":"identity_collision"}', 0),
        ):
            with self.subTest(stdout=stdout, status=status):
                self.assertEqual(
                    RESTORE_READINESS.parse_provisioner_result(stdout, status, "fixture_0123456789abcdef"),
                    ("provisioning_failure", None),
                )

    def test_restored_target_mismatch_fails_closed(self) -> None:
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.phase = "schema_validation"
        drill.local_admin_role = None
        outputs = iter(("0|2||pg_stat_statements,plpgsql|0|0|0|0|0",))
        drill.sql = lambda _database, _statement, _phase: next(outputs)
        with self.assertRaises(RESTORE_READINESS.DrillFailure) as failure:
            drill.verify_schema_state({}, 1, "", "schema_validation")
        self.assertEqual(failure.exception.category, "schema_failure")

    def test_signin_failure_startup_failure_and_output_leaks_cannot_pass(self) -> None:
        passed = {"category": "assertions_passed", "help_getConfig": "pass", "synthetic_signin": "pass"}
        self.assertTrue(RESTORE_READINESS.checker_passes(passed, 0))
        wrong_signin = dict(passed, synthetic_signin="fail")
        self.assertFalse(RESTORE_READINESS.checker_passes(wrong_signin, 0))
        self.assertFalse(RESTORE_READINESS.checker_passes(passed, 1))

        with self.assertRaises(RESTORE_READINESS.DrillFailure) as failure:
            RESTORE_READINESS.require_container_running({"State": {"Running": False}}, "daemon_startup", "startup_failure")
        self.assertEqual(failure.exception.category, "startup_failure")

        marker = b"private-synthetic-password-marker"
        self.assertTrue(RESTORE_READINESS.contains_leak_marker(marker, b"{}", [marker]))
        self.assertTrue(RESTORE_READINESS.contains_leak_marker(b"{}", marker, [marker]))
        self.assertFalse(RESTORE_READINESS.contains_leak_marker(b"{}", b"{}", [marker]))

    def test_interrupts_are_recorded_without_losing_the_original_failure(self) -> None:
        for signum in (RESTORE_READINESS.signal.SIGINT, RESTORE_READINESS.signal.SIGTERM):
            drill = type("DrillState", (), {"phase": "restore", "interrupted": None})()
            with self.subTest(signum=signum), self.assertRaises(RESTORE_READINESS.DrillFailure) as failure:
                RESTORE_READINESS.record_drill_signal(drill, signum, None, False)
            self.assertEqual(failure.exception.category, "interrupted")
            self.assertEqual(drill.interrupted, str(signum))
            original = RESTORE_READINESS.DrillFailure("restore", "restore_failure")
            self.assertIs(RESTORE_READINESS.record_drill_signal(drill, signum, original, True), original)

    def test_pending_container_is_recovered_only_when_immutable_identity_matches(self) -> None:
        document, record = container_fixture()
        pending = dict(record)
        pending.update(
            {
                "id": "",
                "volumes_before": [],
                "containers_before": [],
                "unexpected_volumes": [],
                "unexpected_volume_mounts": [],
                "unexpected_volume_records": [],
            }
        )
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {"pending_container": pending, "containers": []}
        drill.save = lambda: None
        drill.cleanup_volume_inventory = lambda: []
        drill.cleanup_container_inventory = lambda: [(record["id"], record["name"])]
        drill.cleanup_container_inspect = lambda _identifier: document
        self.assertTrue(drill.cleanup_recover_pending_container([(record["id"], record["name"])]))
        self.assertEqual(drill.manifest["pending_container"], None)
        self.assertEqual(drill.manifest["containers"][0]["id"], record["id"])

        wrong_document = json.loads(json.dumps(document))
        wrong_document["Id"] = "f" * 64
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {"pending_container": pending, "containers": []}
        drill.save = lambda: None
        drill.cleanup_volume_inventory = lambda: []
        drill.cleanup_container_inspect = lambda _identifier: wrong_document
        self.assertFalse(drill.cleanup_recover_pending_container([(record["id"], record["name"])]))

    def test_cleanup_volume_inventory_accepts_named_foreign_volumes(self) -> None:
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.cleanup_call = Mock(return_value=(0, b"restore-readiness-foreign-fixture\n"))
        self.assertEqual(drill.cleanup_volume_inventory(), ["restore-readiness-foreign-fixture"])

    def test_cleanup_listing_and_removal_failures_never_report_absence(self) -> None:
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {"containers": [], "pending_container": None, "baseline": {"containers": []}}
        drill.cleanup_container_inventory = lambda: (_ for _ in ()).throw(
            RESTORE_READINESS.DrillFailure("cleanup", "cleanup_failure")
        )
        self.assertFalse(drill.cleanup_owned_containers())

        document, record = container_fixture()
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {
            "containers": [record],
            "pending_container": None,
            "baseline": {"containers": []},
            "cleanup_deadline": time.monotonic() + 10,
        }
        drill.cleanup_container_inventory = lambda: [(record["id"], record["name"])]
        drill.cleanup_recover_pending_container = lambda _containers: True
        drill.cleanup_container_inspect = lambda _identifier: document
        drill.cleanup_call = lambda args, **_kwargs: (1, b"") if args[0] == "rm" else (0, b"")
        self.assertFalse(drill.cleanup_owned_containers())

    def test_cleanup_process_scan_ignores_docker_cli_that_exited_before_stat(self) -> None:
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {"cleanup_deadline": time.monotonic() + 10}
        drill.run_dir = Path("/tmp/teagram-restore-readiness.gone")

        with (
            patch.object(RESTORE_READINESS, "docker_cli_pids", return_value=[999_999]),
            patch.object(RESTORE_READINESS, "process_start_ticks", return_value=None),
            patch.object(RESTORE_READINESS.os, "kill", side_effect=ProcessLookupError),
        ):
            self.assertTrue(drill.cleanup_processes())

    def test_cleanup_process_scan_keeps_live_unreadable_docker_cli_unverified(self) -> None:
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {"cleanup_deadline": time.monotonic() + 10}
        drill.run_dir = Path("/tmp/teagram-restore-readiness.unreadable")

        with (
            patch.object(RESTORE_READINESS, "docker_cli_pids", return_value=[999_999]),
            patch.object(RESTORE_READINESS, "process_start_ticks", return_value=None),
            patch.object(RESTORE_READINESS.os, "kill", return_value=None),
        ):
            self.assertFalse(drill.cleanup_processes())

    def test_cleanup_process_scan_keeps_live_unreadable_manifest_process_unverified(self) -> None:
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {
            "cleanup_deadline": time.monotonic() + 10,
            "processes": [{"pid": 999_999, "start_ticks": 1}],
        }
        drill.run_dir = Path("/tmp/teagram-restore-readiness.tracked-unreadable")

        with (
            patch.object(RESTORE_READINESS, "docker_cli_pids", return_value=[]),
            patch.object(RESTORE_READINESS, "process_start_ticks", return_value=None),
            patch.object(RESTORE_READINESS.os, "kill", return_value=None),
        ):
            self.assertFalse(drill.cleanup_processes())

    def test_cleanup_process_scan_ignores_gone_manifest_process(self) -> None:
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {
            "cleanup_deadline": time.monotonic() + 10,
            "processes": [{"pid": 999_999, "start_ticks": 1}],
        }
        drill.run_dir = Path("/tmp/teagram-restore-readiness.tracked-gone")

        with (
            patch.object(RESTORE_READINESS, "docker_cli_pids", return_value=[]),
            patch.object(RESTORE_READINESS, "process_start_ticks", return_value=None),
            patch.object(RESTORE_READINESS.os, "kill", side_effect=ProcessLookupError),
        ):
            self.assertTrue(drill.cleanup_processes())

    def test_supervisor_retries_a_transient_parent_manifest_read_failure(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-supervisor-parent-read-") as directory:
            lock_path = Path(directory) / "lock"
            lock_path.write_bytes(b"")
            lock_fd = RESTORE_READINESS.os.open(lock_path, RESTORE_READINESS.os.O_RDWR)
            lock_info = RESTORE_READINESS.os.fstat(lock_fd)
            rollout_path = Path(directory) / "rollout-lock"
            rollout_path.write_bytes(b"")
            rollout_fd = RESTORE_READINESS.os.open(rollout_path, RESTORE_READINESS.os.O_RDWR)
            rollout_info = RESTORE_READINESS.os.fstat(rollout_fd)

            def manifest(parent_done: bool, *, registered: bool = False) -> dict[str, Any]:
                return {
                    "global_lock": {"device": lock_info.st_dev, "inode": lock_info.st_ino},
                    "rollout_lock": {"device": rollout_info.st_dev, "inode": rollout_info.st_ino},
                    "supervisor": (
                        {"pid": os.getpid(), "start_ticks": 77}
                        if registered
                        else {"pid": 0, "start_ticks": 0}
                    ),
                    "parent": {"pid": 999999, "start_ticks": 1},
                    "parent_done": parent_done,
                    "work_deadline": RESTORE_READINESS.time.monotonic() + 60,
                    "cleanup_deadline": RESTORE_READINESS.time.monotonic() + 60,
                }

            loads = iter(
                (
                    manifest(False),
                    manifest(False),
                    RESTORE_READINESS.DrillFailure("cleanup", "cleanup_failure"),
                    manifest(True, registered=True),
                    manifest(True, registered=True),
                )
            )
            output = io.StringIO()

            def load_then_fail(_path: Path) -> dict[str, Any]:
                value = next(loads)
                if isinstance(value, Exception):
                    raise value
                return value

            class RecoveryDrill:
                def __init__(self, _args: Any, _run_dir: Path, current_manifest: dict[str, Any]) -> None:
                    self.manifest = current_manifest
                    self.cleanup_failure = None
                    self.cleanup_active = False
                    self.active_cleanup_operation = None

                def save(self) -> None:
                    return None

                def cleanup_resources(self) -> bool:
                    return True

            with (
                patch.object(RESTORE_READINESS, "load_manifest", side_effect=load_then_fail),
                patch.object(
                    RESTORE_READINESS,
                    "process_start_ticks",
                    side_effect=lambda pid: 77 if pid == RESTORE_READINESS.os.getpid() else 1,
                ),
                patch.object(RESTORE_READINESS, "make_runtime_args", return_value=None),
                patch.object(RESTORE_READINESS, "Drill", RecoveryDrill),
                patch.object(RESTORE_READINESS, "remove_finished_run", return_value=True),
                redirect_stdout(output),
            ):
                status = RESTORE_READINESS.supervise(Path(directory) / "ownership.json", lock_fd, rollout_fd)

            self.assertEqual(status, 0)
            self.assertEqual(output.getvalue(), "")

    def test_supervisor_recovers_owned_resources_after_parent_disappears(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-supervisor-") as directory:
            lock_path = Path(directory) / "lock"
            lock_path.write_bytes(b"")
            lock_fd = RESTORE_READINESS.os.open(lock_path, RESTORE_READINESS.os.O_RDWR)
            lock_info = RESTORE_READINESS.os.fstat(lock_fd)
            rollout_path = Path(directory) / "rollout-lock"
            rollout_path.write_bytes(b"")
            rollout_fd = RESTORE_READINESS.os.open(rollout_path, RESTORE_READINESS.os.O_RDWR)
            rollout_info = RESTORE_READINESS.os.fstat(rollout_fd)
            manifest = {
                "global_lock": {"device": lock_info.st_dev, "inode": lock_info.st_ino},
                "rollout_lock": {"device": rollout_info.st_dev, "inode": rollout_info.st_ino},
                "supervisor": {"pid": 0, "start_ticks": 0},
                "parent": {"pid": 999999, "start_ticks": 1},
                "parent_done": False,
                "cleanup_deadline": RESTORE_READINESS.time.monotonic() + 60,
            }

            class RecoveryDrill:
                def __init__(self, _args: Any, _run_dir: Path, recovered_manifest: dict[str, Any]) -> None:
                    self.manifest = recovered_manifest

                def save(self) -> None:
                    return None

                def cleanup_resources(self) -> bool:
                    return True

            with (
                patch.object(RESTORE_READINESS, "load_manifest", return_value=manifest),
                patch.object(RESTORE_READINESS, "process_start_ticks", side_effect=lambda pid: 77 if pid == RESTORE_READINESS.os.getpid() else None),
                patch.object(RESTORE_READINESS, "make_runtime_args", return_value=None),
                patch.object(RESTORE_READINESS, "Drill", RecoveryDrill),
                patch.object(RESTORE_READINESS, "remove_finished_run", return_value=True),
            ):
                self.assertEqual(RESTORE_READINESS.supervise(Path(directory) / "ownership.json", lock_fd, rollout_fd), 0)

    def test_supervisor_cleanup_uses_cleanup_budget_after_work_deadline(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-supervisor-expired-work-") as directory:
            run_dir = Path(directory)
            lock_path = run_dir / "lock"
            lock_path.write_bytes(b"")
            lock_fd = RESTORE_READINESS.os.open(lock_path, RESTORE_READINESS.os.O_RDWR)
            lock_info = RESTORE_READINESS.os.fstat(lock_fd)
            rollout_path = run_dir / "rollout-lock"
            rollout_path.write_bytes(b"")
            rollout_fd = RESTORE_READINESS.os.open(rollout_path, RESTORE_READINESS.os.O_RDWR)
            rollout_info = RESTORE_READINESS.os.fstat(rollout_fd)
            global_lock = {"device": lock_info.st_dev, "inode": lock_info.st_ino}
            rollout_lock = {"device": rollout_info.st_dev, "inode": rollout_info.st_ino}
            manifest, private_path = expired_work_cleanup_manifest(run_dir, global_lock, rollout_lock)
            inventory_calls: list[list[str]] = []
            instances: list[Any] = []

            with (
                patch.object(RESTORE_READINESS, "load_manifest", return_value=manifest),
                patch.object(
                    RESTORE_READINESS,
                    "process_start_ticks",
                    side_effect=lambda pid: 77 if pid == RESTORE_READINESS.os.getpid() else None,
                ),
                patch.object(RESTORE_READINESS, "make_runtime_args", return_value=None),
                patch.object(
                    RESTORE_READINESS,
                    "Drill",
                    cleanup_deadline_drill_type(inventory_calls, instances),
                ),
                patch.object(RESTORE_READINESS, "remove_finished_run", return_value=True),
            ):
                status = RESTORE_READINESS.supervise(
                    run_dir / "ownership.json", lock_fd, rollout_fd
                )

            self.assertEqual(status, 0)
            self.assertFalse(private_path.exists())
            self.assertEqual(len(inventory_calls), 8)
            self.assertEqual(instances[-1].cleanup_call.call_count, 8)

    def test_recovery_accepts_legacy_baseline_without_image_inventory(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-recover-legacy-inventory-") as directory:
            run_dir = Path(directory)
            lock_path = run_dir / "lock"
            lock_path.write_bytes(b"")
            lock_fd = RESTORE_READINESS.os.open(lock_path, RESTORE_READINESS.os.O_RDWR)
            lock_info = RESTORE_READINESS.os.fstat(lock_fd)
            rollout_path = run_dir / "rollout-lock"
            rollout_path.write_bytes(b"")
            rollout_fd = RESTORE_READINESS.os.open(rollout_path, RESTORE_READINESS.os.O_RDWR)
            rollout_info = RESTORE_READINESS.os.fstat(rollout_fd)
            global_lock = {"device": lock_info.st_dev, "inode": lock_info.st_ino}
            rollout_lock = {
                "path": str(RESTORE_READINESS.ROLLOUT_LOCK_PATH),
                "device": rollout_info.st_dev,
                "inode": rollout_info.st_ino,
            }
            manifest, _private_path = expired_work_cleanup_manifest(run_dir, global_lock, rollout_lock)
            token = "c" * 32
            other_run_image = "sha256:" + "d" * 64
            manifest["token"] = token
            manifest["baseline"]["docker_inventory"].pop("images")
            verify_foreign_inventory = RESTORE_READINESS.Drill.verify_foreign_inventory

            def docker_call(arguments: list[str]) -> bytes:
                if arguments[:3] == ["ps", "--all", "--no-trunc"]:
                    return b""
                if arguments[:2] == ["volume", "ls"]:
                    return b""
                if arguments[:2] == ["image", "ls"]:
                    if arguments[-1] == f"label={RESTORE_READINESS.IMAGE_TOKEN_LABEL}={token}":
                        return b""
                    if arguments[-1] == f"label={RESTORE_READINESS.IMAGE_TOKEN_LABEL}":
                        return f"{other_run_image}\n".encode()
                    raise AssertionError(f"unexpected image inventory filter: {arguments[-1]}")
                if arguments[:2] == ["network", "ls"]:
                    return b""
                raise AssertionError(f"unexpected Docker inventory command: {arguments[0]}")

            class RecoveryDrill:
                def __init__(self, _args: Any, _run_dir: Path, recovery_manifest: dict[str, Any]) -> None:
                    self.manifest = recovery_manifest

                def save(self) -> None:
                    return None

                def docker_resource_inventory(self, _phase: str) -> dict[str, Any]:
                    return RESTORE_READINESS.collect_docker_resource_inventory(docker_call, self.manifest["token"])

                def cleanup_resources(self) -> bool:
                    return verify_foreign_inventory(self)

            output = io.StringIO()
            with (
                patch.object(RESTORE_READINESS.os, "geteuid", return_value=0),
                patch.object(RESTORE_READINESS, "open_rollout_lock", return_value=(rollout_fd, rollout_lock)),
                patch.object(RESTORE_READINESS, "load_manifest", return_value=manifest),
                patch.object(RESTORE_READINESS, "open_global_lock", return_value=(lock_fd, global_lock)),
                patch.object(RESTORE_READINESS, "make_runtime_args", return_value=None),
                patch.object(RESTORE_READINESS, "Drill", RecoveryDrill),
                patch.object(RESTORE_READINESS, "remove_finished_run", return_value=True),
                redirect_stdout(output),
            ):
                status = RESTORE_READINESS.recover(run_dir / "ownership.json")

            self.assertEqual(status, 1)
            self.assertEqual(manifest["baseline"]["docker_inventory"]["images"], {})
            report = json.loads(output.getvalue())
            self.assertEqual(report["cleanup"], "verified")

    def test_recovery_uses_cleanup_budget_after_work_deadline(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-recover-expired-work-") as directory:
            run_dir = Path(directory)
            lock_path = run_dir / "lock"
            lock_path.write_bytes(b"")
            lock_fd = RESTORE_READINESS.os.open(lock_path, RESTORE_READINESS.os.O_RDWR)
            lock_info = RESTORE_READINESS.os.fstat(lock_fd)
            rollout_path = run_dir / "rollout-lock"
            rollout_path.write_bytes(b"")
            rollout_fd = RESTORE_READINESS.os.open(rollout_path, RESTORE_READINESS.os.O_RDWR)
            rollout_info = RESTORE_READINESS.os.fstat(rollout_fd)
            global_lock = {"device": lock_info.st_dev, "inode": lock_info.st_ino}
            rollout_lock = {
                "path": str(RESTORE_READINESS.ROLLOUT_LOCK_PATH),
                "device": rollout_info.st_dev,
                "inode": rollout_info.st_ino,
            }
            manifest, private_path = expired_work_cleanup_manifest(run_dir, global_lock, rollout_lock)
            inventory_calls: list[list[str]] = []
            instances: list[Any] = []
            output = io.StringIO()

            with (
                patch.object(RESTORE_READINESS.os, "geteuid", return_value=0),
                patch.object(RESTORE_READINESS, "open_rollout_lock", return_value=(rollout_fd, rollout_lock)),
                patch.object(RESTORE_READINESS, "load_manifest", return_value=manifest),
                patch.object(RESTORE_READINESS, "open_global_lock", return_value=(lock_fd, global_lock)),
                patch.object(RESTORE_READINESS, "make_runtime_args", return_value=None),
                patch.object(
                    RESTORE_READINESS,
                    "Drill",
                    cleanup_deadline_drill_type(inventory_calls, instances),
                ),
                patch.object(RESTORE_READINESS, "remove_finished_run", return_value=True),
                redirect_stdout(output),
            ):
                status = RESTORE_READINESS.recover(run_dir / "ownership.json")

            self.assertEqual(status, 1)
            self.assertFalse(private_path.exists())
            self.assertLess(manifest["work_deadline"], time.monotonic())
            self.assertGreater(manifest["cleanup_deadline"], time.monotonic())
            self.assertEqual(len(inventory_calls), 8)
            self.assertEqual(instances[-1].cleanup_call.call_count, 8)
            report = json.loads(output.getvalue())
            self.assertEqual(report["cleanup"], "verified")

    def test_supervisor_reports_allowlisted_cleanup_operation_and_error(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-supervisor-cleanup-failure-") as directory:
            lock_path = Path(directory) / "lock"
            lock_path.write_bytes(b"")
            lock_fd = RESTORE_READINESS.os.open(lock_path, RESTORE_READINESS.os.O_RDWR)
            lock_info = RESTORE_READINESS.os.fstat(lock_fd)
            rollout_path = Path(directory) / "rollout-lock"
            rollout_path.write_bytes(b"")
            rollout_fd = RESTORE_READINESS.os.open(rollout_path, RESTORE_READINESS.os.O_RDWR)
            rollout_info = RESTORE_READINESS.os.fstat(rollout_fd)
            manifest = {
                "global_lock": {"device": lock_info.st_dev, "inode": lock_info.st_ino},
                "rollout_lock": {"device": rollout_info.st_dev, "inode": rollout_info.st_ino},
                "supervisor": {"pid": 0, "start_ticks": 0},
                "parent": {"pid": 999999, "start_ticks": 1},
                "parent_done": True,
                "cleanup_deadline": RESTORE_READINESS.time.monotonic() + 60,
            }

            class FailedCleanupDrill:
                def __init__(self, _args: Any, _run_dir: Path, recovered_manifest: dict[str, Any]) -> None:
                    self.manifest = recovered_manifest
                    self.cleanup_failure = {
                        "operation": "docker_container_remove",
                        "error": "command_failed",
                        "exit_status": 1,
                    }

                def cleanup_resources(self) -> bool:
                    return False

                def save(self) -> None:
                    return None

            output = io.StringIO()
            with (
                patch.object(RESTORE_READINESS, "load_manifest", return_value=manifest),
                patch.object(RESTORE_READINESS, "process_start_ticks", side_effect=lambda pid: 77 if pid == RESTORE_READINESS.os.getpid() else None),
                patch.object(RESTORE_READINESS, "make_runtime_args", return_value=None),
                patch.object(RESTORE_READINESS, "Drill", FailedCleanupDrill),
                redirect_stdout(output),
            ):
                status = RESTORE_READINESS.supervise(Path(directory) / "ownership.json", lock_fd, rollout_fd)

            self.assertEqual(status, 1)
            diagnostic = json.loads(output.getvalue())
            self.assertEqual(
                diagnostic,
                {
                    "operation": "docker_container_remove",
                    "error": "command_failed",
                    "exit_status": 1,
                },
            )
            self.assertNotIn("container-id", output.getvalue())

    def test_supervisor_setup_failure_reports_the_safe_cleanup_stage(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-supervisor-setup-stage-") as directory:
            lock_path = Path(directory) / "lock"
            lock_path.write_bytes(b"")
            lock_fd = RESTORE_READINESS.os.open(lock_path, RESTORE_READINESS.os.O_RDWR)
            lock_info = RESTORE_READINESS.os.fstat(lock_fd)
            rollout_path = Path(directory) / "rollout-lock"
            rollout_path.write_bytes(b"")
            rollout_fd = RESTORE_READINESS.os.open(rollout_path, RESTORE_READINESS.os.O_RDWR)
            rollout_info = RESTORE_READINESS.os.fstat(rollout_fd)
            manifest = {
                "global_lock": {"device": lock_info.st_dev, "inode": lock_info.st_ino},
                "rollout_lock": {"device": rollout_info.st_dev, "inode": rollout_info.st_ino},
                "supervisor": {"pid": 0, "start_ticks": 0},
                "parent": {"pid": 999999, "start_ticks": 1},
                "parent_done": True,
                "cleanup_deadline": RESTORE_READINESS.time.monotonic() + 60,
            }
            loads = iter((manifest, manifest, manifest))
            output = io.StringIO()

            class RegisteredDrill:
                def __init__(self, _args: Any, _run_dir: Path, current_manifest: dict[str, Any]) -> None:
                    self.manifest = current_manifest
                    self.cleanup_active = False
                    self.cleanup_failure = None
                    self.active_cleanup_operation = None

                def save(self) -> None:
                    return None

            def load_then_fail(_path: Path) -> dict[str, Any]:
                try:
                    return next(loads)
                except StopIteration as exc:
                    raise RESTORE_READINESS.DrillFailure("cleanup", "cleanup_failure") from exc

            with (
                patch.object(RESTORE_READINESS, "load_manifest", side_effect=load_then_fail),
                patch.object(
                    RESTORE_READINESS,
                    "process_start_ticks",
                    side_effect=(77, None),
                ),
                patch.object(RESTORE_READINESS, "make_runtime_args", return_value=None),
                patch.object(RESTORE_READINESS, "Drill", RegisteredDrill),
                redirect_stdout(output),
            ):
                status = RESTORE_READINESS.supervise(Path(directory) / "ownership.json", lock_fd, rollout_fd)

            self.assertEqual(status, 1)
            diagnostic = json.loads(output.getvalue())
            self.assertEqual(diagnostic["operation"], "supervisor_setup")
            self.assertEqual(diagnostic["error"], "supervisor_failed")
            self.assertEqual(diagnostic["stage"], "load_final_manifest")
            self.assertNotIn(directory, output.getvalue())
            self.assertEqual(
                RESTORE_READINESS.parse_cleanup_failure_output(output.getvalue().encode()),
                "restore readiness cleanup failed: operation=supervisor_setup "
                "error=supervisor_failed stage=load_final_manifest",
            )
            with self.assertRaises(ValueError):
                RESTORE_READINESS.cleanup_failure_document(
                    "supervisor_setup",
                    "supervisor_failed",
                    stage=directory,
                )

    def test_supervisor_reports_malformed_later_container_inventory(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-supervisor-invalid-inventory-") as directory:
            lock_path = Path(directory) / "lock"
            lock_path.write_bytes(b"")
            lock_fd = RESTORE_READINESS.os.open(lock_path, RESTORE_READINESS.os.O_RDWR)
            lock_info = RESTORE_READINESS.os.fstat(lock_fd)
            rollout_path = Path(directory) / "rollout-lock"
            rollout_path.write_bytes(b"")
            rollout_fd = RESTORE_READINESS.os.open(rollout_path, RESTORE_READINESS.os.O_RDWR)
            rollout_info = RESTORE_READINESS.os.fstat(rollout_fd)
            container_id = "a" * 64
            manifest = {
                "global_lock": {"device": lock_info.st_dev, "inode": lock_info.st_ino},
                "rollout_lock": {"device": rollout_info.st_dev, "inode": rollout_info.st_ino},
                "supervisor": {"pid": 0, "start_ticks": 0},
                "parent": {"pid": 999999, "start_ticks": 1},
                "parent_done": True,
                "cleanup_deadline": RESTORE_READINESS.time.monotonic() + 60,
                "resource_creation_started": True,
                "containers": [{"id": container_id, "name": "owned"}],
                "pending_container": None,
            }
            inventories = iter((f"{container_id} owned\n".encode(), b"malformed inventory\n"))

            class MalformedInventoryDrill(RESTORE_READINESS.Drill):
                def __init__(self, _args: Any, _run_dir: Path, recovered_manifest: dict[str, Any]) -> None:
                    self.manifest = recovered_manifest
                    self.cleanup_active = False
                    self.cleanup_failure = None
                    self.active_cleanup_operation = None
                    self.gateway = None
                    self.save = lambda: None
                    self.cleanup_budget_available = lambda: True
                    self.cleanup_processes = lambda: True
                    self.remove_build_inputs = lambda: True
                    self.cleanup_call = lambda *_args, **_kwargs: (0, next(inventories))

            output = io.StringIO()
            with (
                patch.object(RESTORE_READINESS, "load_manifest", return_value=manifest),
                patch.object(RESTORE_READINESS, "process_start_ticks", side_effect=lambda pid: 77 if pid == RESTORE_READINESS.os.getpid() else None),
                patch.object(RESTORE_READINESS, "make_runtime_args", return_value=None),
                patch.object(RESTORE_READINESS, "Drill", MalformedInventoryDrill),
                redirect_stdout(output),
            ):
                status = RESTORE_READINESS.supervise(Path(directory) / "ownership.json", lock_fd, rollout_fd)

            self.assertEqual(status, 1)
            self.assertEqual(
                json.loads(output.getvalue()),
                {"operation": "docker_container_inventory", "error": "invalid_output"},
            )
            self.assertNotIn(container_id, output.getvalue())
            self.assertNotIn("owned", output.getvalue())

    def test_malformed_cleanup_volume_and_image_inventories_record_diagnostics(self) -> None:
        cases = (
            ("cleanup_volume_inventory", "docker_volume_inventory"),
            ("cleanup_image_inventory", "docker_image_inventory"),
        )
        for method_name, operation in cases:
            with self.subTest(method=method_name):
                drill = object.__new__(RESTORE_READINESS.Drill)
                drill.cleanup_failure = None
                drill.cleanup_active = True
                drill.cleanup_call = Mock(return_value=(0, b"malformed inventory\n"))

                with self.assertRaises(RESTORE_READINESS.DrillFailure):
                    getattr(drill, method_name)()

                self.assertEqual(
                    drill.cleanup_failure,
                    {"operation": operation, "error": "invalid_output"},
                )

    def test_supervisor_retains_active_cleanup_operation_for_other_exceptions(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-supervisor-cleanup-exception-") as directory:
            lock_path = Path(directory) / "lock"
            lock_path.write_bytes(b"")
            lock_fd = RESTORE_READINESS.os.open(lock_path, RESTORE_READINESS.os.O_RDWR)
            lock_info = RESTORE_READINESS.os.fstat(lock_fd)
            rollout_path = Path(directory) / "rollout-lock"
            rollout_path.write_bytes(b"")
            rollout_fd = RESTORE_READINESS.os.open(rollout_path, RESTORE_READINESS.os.O_RDWR)
            rollout_info = RESTORE_READINESS.os.fstat(rollout_fd)
            manifest = {
                "global_lock": {"device": lock_info.st_dev, "inode": lock_info.st_ino},
                "rollout_lock": {"device": rollout_info.st_dev, "inode": rollout_info.st_ino},
                "supervisor": {"pid": 0, "start_ticks": 0},
                "parent": {"pid": 999999, "start_ticks": 1},
                "parent_done": True,
                "cleanup_deadline": RESTORE_READINESS.time.monotonic() + 60,
            }

            class UnexpectedCleanupDrill(RESTORE_READINESS.Drill):
                def __init__(self, _args: Any, _run_dir: Path, recovered_manifest: dict[str, Any]) -> None:
                    self.manifest = recovered_manifest
                    self.cleanup_active = False
                    self.cleanup_failure = None
                    self.active_cleanup_operation = None
                    self.gateway = None
                    self.save = lambda: None
                    self.cleanup_budget_available = lambda: True

                    def fail_cleanup_processes() -> bool:
                        raise ValueError("private cleanup detail")

                    self.cleanup_processes = fail_cleanup_processes

            output = io.StringIO()
            with (
                patch.object(RESTORE_READINESS, "load_manifest", return_value=manifest),
                patch.object(RESTORE_READINESS, "process_start_ticks", side_effect=lambda pid: 77 if pid == RESTORE_READINESS.os.getpid() else None),
                patch.object(RESTORE_READINESS, "make_runtime_args", return_value=None),
                patch.object(RESTORE_READINESS, "Drill", UnexpectedCleanupDrill),
                redirect_stdout(output),
            ):
                status = RESTORE_READINESS.supervise(Path(directory) / "ownership.json", lock_fd, rollout_fd)

            self.assertEqual(status, 1)
            self.assertEqual(
                json.loads(output.getvalue()),
                {"operation": "cleanup_processes", "error": "supervisor_failed"},
            )
            self.assertNotIn("private cleanup detail", output.getvalue())

    def test_cleanup_command_failure_diagnostic_omits_command_output_and_identifiers(self) -> None:
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.executable = "/usr/bin/docker"
        drill.cleanup_active = True
        drill.cleanup_failure = None
        drill._run_child = lambda *_args, **_kwargs: (1, b"private-container-id daemon error")

        status, output = RESTORE_READINESS.Docker.cleanup_call(
            drill,
            ["rm", "private-container-id"],
        )

        self.assertEqual(status, 1)
        self.assertEqual(output, b"private-container-id daemon error")
        self.assertEqual(
            drill.cleanup_failure,
            {
                "operation": "docker_container_remove",
                "error": "command_failed",
                "exit_status": 1,
            },
        )
        message = RESTORE_READINESS.cleanup_failure_log_message(drill.cleanup_failure)
        self.assertEqual(
            message,
            "restore readiness cleanup failed: operation=docker_container_remove "
            "error=command_failed exit_status=1",
        )
        self.assertEqual(
            RESTORE_READINESS.parse_cleanup_failure_output(
                b'{"operation":"docker_container_remove","error":"command_failed","exit_status":1}\n'
            ),
            message,
        )
        self.assertNotIn("private-container-id", message)
        self.assertIsNone(
            RESTORE_READINESS.parse_cleanup_failure_output(
                b'{"operation":"docker_container_remove","error":"private-container-id"}\n'
            )
        )

    def test_supervisor_bounds_delayed_cleanup_command_and_leaves_cleanup_unverified(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-cleanup-deadline-") as directory:
            lock_path = Path(directory) / "lock"
            lock_path.write_bytes(b"")
            lock_fd = RESTORE_READINESS.os.open(lock_path, RESTORE_READINESS.os.O_RDWR)
            lock_info = RESTORE_READINESS.os.fstat(lock_fd)
            rollout_path = Path(directory) / "rollout-lock"
            rollout_path.write_bytes(b"")
            rollout_fd = RESTORE_READINESS.os.open(rollout_path, RESTORE_READINESS.os.O_RDWR)
            rollout_info = RESTORE_READINESS.os.fstat(rollout_fd)
            manifest = {
                "global_lock": {"device": lock_info.st_dev, "inode": lock_info.st_ino},
                "rollout_lock": {"device": rollout_info.st_dev, "inode": rollout_info.st_ino},
                "supervisor": {"pid": 0, "start_ticks": 0},
                "parent": {"pid": 999999, "start_ticks": 1},
                "parent_done": True,
                "cleanup_deadline": time.monotonic() + 1.5,
                "processes": [],
            }

            class DelayedCleanupDrill(RESTORE_READINESS.Drill):
                def __init__(self, _args: Any, _run_dir: Path, recovered_manifest: dict[str, Any]) -> None:
                    self.manifest = recovered_manifest
                    self.executable = "/usr/bin/sleep"
                    self.environment = {"PATH": "/usr/bin:/bin"}
                    self.cleanup_active = True
                    self.cleanup_failure = None
                    self.save = lambda: None

                def cleanup_resources(self) -> bool:
                    try:
                        self.cleanup_call(["2"], timeout=2)
                    except RESTORE_READINESS.DrillFailure:
                        self.manifest["cleanup_verified"] = False
                        return False
                    self.manifest["cleanup_verified"] = True
                    return True

            process_start_ticks = RESTORE_READINESS.process_start_ticks

            def start_ticks(pid: int) -> int | None:
                if pid == 999999:
                    return None
                return process_start_ticks(pid)

            started = time.monotonic()
            output = io.StringIO()
            with (
                patch.object(RESTORE_READINESS, "load_manifest", return_value=manifest),
                patch.object(RESTORE_READINESS, "process_start_ticks", side_effect=start_ticks),
                patch.object(RESTORE_READINESS, "make_runtime_args", return_value=None),
                patch.object(RESTORE_READINESS, "Drill", DelayedCleanupDrill),
                patch.object(RESTORE_READINESS, "remove_finished_run", return_value=True),
                redirect_stdout(output),
            ):
                status = RESTORE_READINESS.supervise(Path(directory) / "ownership.json", lock_fd, rollout_fd)

            self.assertEqual(status, 1)
            self.assertLess(time.monotonic() - started, 1)
            self.assertFalse(manifest["cleanup_verified"])
            self.assertEqual(
                json.loads(output.getvalue()),
                {"operation": "cleanup_resources", "error": "command_timeout"},
            )

    def test_disposable_dns_fixture_answers_only_its_query(self) -> None:
        query = RESTORE_READINESS.dns_fixture_query()
        response = RESTORE_READINESS.dns_fixture_response(query)
        self.assertIsNotNone(response)
        assert response is not None
        self.assertTrue(RESTORE_READINESS.dns_fixture_valid_response(query, response))
        self.assertIsNone(RESTORE_READINESS.dns_fixture_response(b"bad"))

    def test_recovery_failure_suppresses_unexpected_diagnostics(self) -> None:
        output = io.StringIO()
        rollout_fd, rollout_write_fd = os.pipe()
        rollout_record = {
            "path": str(RESTORE_READINESS.ROLLOUT_LOCK_PATH),
            "device": 1,
            "inode": 2,
        }
        with (
            patch.object(RESTORE_READINESS.os, "geteuid", return_value=0),
            patch.object(RESTORE_READINESS, "open_rollout_lock", return_value=(rollout_fd, rollout_record)),
            patch.object(RESTORE_READINESS, "load_manifest", side_effect=RuntimeError("private-marker")),
            redirect_stdout(output),
        ):
            status = RESTORE_READINESS.recover(Path("/run/restore-readiness-private/ownership.json"))
        os.close(rollout_write_fd)
        self.assertEqual(status, 1)
        report = json.loads(output.getvalue())
        self.assertEqual(report["result"], "fail")
        self.assertEqual(report["cleanup"], "unverified")
        self.assertNotIn("private-marker", output.getvalue())

    def test_recovery_refreshes_an_expired_cleanup_deadline_after_lock(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-recovery-deadline-") as directory:
            lock_path = Path(directory) / "lock"
            lock_path.write_bytes(b"")
            descriptor = RESTORE_READINESS.os.open(lock_path, RESTORE_READINESS.os.O_RDWR)
            lock_info = RESTORE_READINESS.os.fstat(descriptor)
            lock_record = {"device": lock_info.st_dev, "inode": lock_info.st_ino}
            rollout_path = Path(directory) / "rollout-lock"
            rollout_path.write_bytes(b"")
            rollout_descriptor = RESTORE_READINESS.os.open(rollout_path, RESTORE_READINESS.os.O_RDWR)
            rollout_info = RESTORE_READINESS.os.fstat(rollout_descriptor)
            rollout_record = {
                "path": str(RESTORE_READINESS.ROLLOUT_LOCK_PATH),
                "device": rollout_info.st_dev,
                "inode": rollout_info.st_ino,
            }
            original_deadline = time.monotonic() - 1
            manifest = {
                "global_lock": lock_record,
                "rollout_lock": rollout_record,
                "cleanup_deadline": original_deadline,
            }
            observed: dict[str, float] = {}

            class RecoveryDrill:
                def __init__(self, _args: Any, _run_dir: Path, recovered_manifest: dict[str, Any]) -> None:
                    self.manifest = recovered_manifest

                def save(self) -> None:
                    observed["saved_deadline"] = self.manifest["cleanup_deadline"]

                def cleanup_resources(self) -> bool:
                    observed["cleanup_deadline"] = self.manifest["cleanup_deadline"]
                    return True

            def acquire_lock() -> tuple[int, dict[str, int]]:
                self.assertEqual(manifest["cleanup_deadline"], original_deadline)
                return descriptor, lock_record

            def acquire_rollout_lock() -> tuple[int, dict[str, int]]:
                return rollout_descriptor, rollout_record

            def remove_run(drill: Any, deadline: float) -> bool:
                observed["remove_deadline"] = deadline
                self.assertEqual(drill.manifest["cleanup_deadline"], deadline)
                return True

            started = time.monotonic()
            output = io.StringIO()
            with (
                patch.object(RESTORE_READINESS.os, "geteuid", return_value=0),
                patch.object(RESTORE_READINESS, "open_rollout_lock", side_effect=acquire_rollout_lock),
                patch.object(RESTORE_READINESS, "load_manifest", return_value=manifest),
                patch.object(RESTORE_READINESS, "open_global_lock", side_effect=acquire_lock),
                patch.object(RESTORE_READINESS, "make_runtime_args", return_value=None),
                patch.object(RESTORE_READINESS, "Drill", RecoveryDrill),
                patch.object(RESTORE_READINESS, "remove_finished_run", side_effect=remove_run),
                redirect_stdout(output),
            ):
                status = RESTORE_READINESS.recover(Path(directory) / "ownership.json")

            refreshed_deadline = manifest["cleanup_deadline"]
            self.assertEqual(status, 1)
            self.assertGreater(refreshed_deadline, time.monotonic())
            self.assertGreater(refreshed_deadline, original_deadline)
            self.assertLessEqual(refreshed_deadline, started + RESTORE_READINESS.CLEANUP_SECONDS + 0.1)
            self.assertEqual(observed, {
                "saved_deadline": refreshed_deadline,
                "cleanup_deadline": refreshed_deadline,
                "remove_deadline": refreshed_deadline,
            })
            report = json.loads(output.getvalue())
            self.assertEqual(report["cleanup"], "verified")


if __name__ == "__main__":
    unittest.main()
