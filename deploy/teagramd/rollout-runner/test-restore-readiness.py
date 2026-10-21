#!/usr/bin/env python3
"""Focused tests for the synthetic restore gate's public safety contract."""

from __future__ import annotations

import importlib.util
import io
import json
import os
import tempfile
import time
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from typing import Any
from unittest.mock import patch


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


class RestoreReadinessContractTests(unittest.TestCase):
    def test_suite_work_budget_caps_each_run_and_is_shared_by_shell_gate(self) -> None:
        self.assertEqual(RESTORE_READINESS.bounded_work_seconds(None, None), 30 * 60)
        self.assertEqual(RESTORE_READINESS.bounded_work_seconds(None, 300), 300)
        self.assertEqual(RESTORE_READINESS.bounded_work_seconds("malformed-sql", None), 8 * 60)
        self.assertEqual(RESTORE_READINESS.bounded_work_seconds("sigkill", 9 * 60), 8 * 60)
        with self.assertRaises(ValueError):
            RESTORE_READINESS.bounded_work_seconds(None, 0)

        shell = SHELL_GATE.read_text(encoding="utf-8")
        self.assertIn("suite_work_deadline=$((SECONDS + 30 * 60))", shell)
        self.assertIn("suite_teardown_deadline=$((suite_work_deadline + 4 * 60))", shell)
        run_drill = shell[shell.index("run_drill() {"):shell.index("\nbuild_inputs\n", shell.index("run_drill() {"))]
        self.assertIn("work_seconds=$(suite_remaining_seconds)", run_drill)
        self.assertIn('--work-seconds "$work_seconds"', run_drill)
        self.assertIn('run_suite_command "${build_env[@]}" go build', shell)
        self.assertIn("run_teardown_command sudo -n -- /usr/bin/find", shell)

    def test_docker_executable_must_be_a_trusted_system_binary(self) -> None:
        trusted = RESTORE_READINESS.ensure_trusted_system_executable("/usr/bin/true")
        self.assertEqual(trusted, str(Path("/usr/bin/true").resolve()))

        with tempfile.TemporaryDirectory(prefix="rr-path-shadow-") as temporary_directory:
            shadow = Path(temporary_directory) / "docker"
            shadow.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            shadow.chmod(0o755)
            with self.assertRaises(RESTORE_READINESS.DrillFailure):
                RESTORE_READINESS.ensure_trusted_system_executable(str(shadow))

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
        )
        self.assertEqual(
            passed,
            {
                "result": "pass",
                "migration_head": RESTORE_READINESS.HEAD_SCHEMA,
                "help_getConfig": "pass",
                "synthetic_signin": "pass",
                "cleanup": "verified",
                "elapsed_ms": 123,
                "exit": 0,
            },
        )
        cleanup_failure = RESTORE_READINESS.report_document(
            "application_assertions",
            "assertions_passed",
            "unverified",
            123,
            migration_head=RESTORE_READINESS.HEAD_SCHEMA,
            help_get_config="pass",
            synthetic_signin="pass",
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
                migration_head=RESTORE_READINESS.BASE_SCHEMA,
                help_get_config="pass",
                synthetic_signin="pass",
            )

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
        )
        self.assertTrue(RESTORE_READINESS.validate_container_inspect(document, created))
        self.assertNotIn("--cpus", created_args)

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

    def test_cleanup_listing_and_removal_failures_never_report_absence(self) -> None:
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {"containers": [], "pending_container": None, "baseline": {"containers": []}}
        drill.cleanup_container_inventory = lambda: (_ for _ in ()).throw(
            RESTORE_READINESS.DrillFailure("cleanup", "cleanup_failure")
        )
        self.assertFalse(drill.cleanup_owned_containers())

        document, record = container_fixture()
        drill = object.__new__(RESTORE_READINESS.Drill)
        drill.manifest = {"containers": [record], "pending_container": None, "baseline": {"containers": []}}
        drill.cleanup_container_inventory = lambda: [(record["id"], record["name"])]
        drill.cleanup_recover_pending_container = lambda _containers: True
        drill.cleanup_container_inspect = lambda _identifier: document
        drill.cleanup_call = lambda args, **_kwargs: (1, b"") if args[0] == "rm" else (0, b"")
        self.assertFalse(drill.cleanup_owned_containers())

    def test_supervisor_recovers_owned_resources_after_parent_disappears(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-supervisor-") as directory:
            lock_path = Path(directory) / "lock"
            lock_path.write_bytes(b"")
            lock_fd = RESTORE_READINESS.os.open(lock_path, RESTORE_READINESS.os.O_RDWR)
            lock_info = RESTORE_READINESS.os.fstat(lock_fd)
            manifest = {
                "global_lock": {"device": lock_info.st_dev, "inode": lock_info.st_ino},
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
                self.assertEqual(RESTORE_READINESS.supervise(Path(directory) / "ownership.json", lock_fd), 0)

    def test_disposable_dns_fixture_answers_only_its_query(self) -> None:
        query = RESTORE_READINESS.dns_fixture_query()
        response = RESTORE_READINESS.dns_fixture_response(query)
        self.assertIsNotNone(response)
        assert response is not None
        self.assertTrue(RESTORE_READINESS.dns_fixture_valid_response(query, response))
        self.assertIsNone(RESTORE_READINESS.dns_fixture_response(b"bad"))

    def test_recovery_failure_suppresses_unexpected_diagnostics(self) -> None:
        output = io.StringIO()
        with (
            patch.object(RESTORE_READINESS.os, "geteuid", return_value=0),
            patch.object(RESTORE_READINESS, "load_manifest", side_effect=RuntimeError("private-marker")),
            redirect_stdout(output),
        ):
            status = RESTORE_READINESS.recover(Path("/run/restore-readiness-private/ownership.json"))
        self.assertEqual(status, 1)
        report = json.loads(output.getvalue())
        self.assertEqual(report["result"], "fail")
        self.assertEqual(report["cleanup"], "unverified")
        self.assertNotIn("private-marker", output.getvalue())


if __name__ == "__main__":
    unittest.main()
