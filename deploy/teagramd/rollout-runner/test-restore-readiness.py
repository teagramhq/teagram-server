#!/usr/bin/env python3
"""Focused tests for the restore readiness gate's public safety contract."""

from __future__ import annotations

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
        self.assertIn('local_args=(--local-dump "$2")', shell)
        self.assertIn('local_args=(--local-fixture-test)', shell)
        self.assertLess(shell.index("--validate-local-dump"), shell.index("build_inputs\n"))
        workflow = Path(__file__).parents[3] / ".github" / "workflows" / "ci.yml"
        self.assertIn("test-restore-readiness.sh --local-fixture-test", workflow.read_text(encoding="utf-8"))

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

    def test_sigkill_drill_requires_injection_marker_and_signal_status(self) -> None:
        marker = b"SIGKILL_INJECTED\n"
        with patch.object(RESTORE_READINESS.os, "write", return_value=len(marker)) as write:
            RESTORE_READINESS.write_sigkill_marker(17)
        write.assert_called_once_with(17, marker)

        with patch.object(RESTORE_READINESS.os, "write", return_value=1):
            with self.assertRaises(RESTORE_READINESS.DrillFailure):
                RESTORE_READINESS.write_sigkill_marker(17)

        shell = SHELL_GATE.read_text(encoding="utf-8")
        self.assertIn('run_drill --fault-test sigkill >"$sigkill_output" 2>/dev/null &', shell)
        self.assertIn('[[ $sigkill_status -ne 137 ]]', shell)
        self.assertIn('[[ $(<"$sigkill_output") != "SIGKILL_INJECTED" ]]', shell)

    def test_docker_executable_must_be_a_trusted_system_binary(self) -> None:
        trusted = RESTORE_READINESS.ensure_trusted_system_executable("/usr/bin/true")
        self.assertEqual(trusted, str(Path("/usr/bin/true").resolve()))

        with tempfile.TemporaryDirectory(prefix="rr-path-shadow-") as temporary_directory:
            shadow = Path(temporary_directory) / "docker"
            shadow.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            shadow.chmod(0o755)
            with self.assertRaises(RESTORE_READINESS.DrillFailure):
                RESTORE_READINESS.ensure_trusted_system_executable(str(shadow))

    def test_startup_report_names_a_rejected_docker_executable(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-path-shadow-") as temporary_directory:
            shadow = Path(temporary_directory) / "docker"
            shadow.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            shadow.chmod(0o755)
            output = io.StringIO()
            error = io.StringIO()
            args = Mock(docker_bin=str(shadow), fault_test=None, work_seconds=None)
            with patch.object(RESTORE_READINESS.os, "geteuid", return_value=0):
                with redirect_stdout(output), redirect_stderr(error):
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

    def test_local_dump_metadata_and_restore_contract_are_bounded(self) -> None:
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
        for info, candidate in (
            (file_info(mode=stat.S_IFLNK | 0o600), name),
            (file_info(uid=1), name),
            (file_info(gid=1), name),
            (file_info(mode=stat.S_IFREG | 0o640), name),
            (file_info(links=2), name),
            (file_info(size=0), name),
            (file_info(size=RESTORE_READINESS.MAX_LOCAL_DUMP_BYTES + 1), name),
            (file_info(), "pgdump-pre-" + "A" * 40 + ".sql"),
        ):
            with self.subTest(info=info, name=candidate):
                self.assertFalse(RESTORE_READINESS.validate_local_dump_metadata(info, candidate))

        source = SCRIPT.read_text(encoding="utf-8")
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
        self.assertIn('role="local_restore_client",\n            user="0:0"', source)
        self.assertIn('"checker_provisioner", "local_restore_client"', source)

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
        self.assertEqual(inventory["schema_head"], RESTORE_READINESS.HEAD_SCHEMA)
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
                "auth_keys_pending_user_id_fkey",
                "auth_keys_user_id_fkey",
                "notify_settings_difference_cursors_auth_key_id_fkey",
                "srp_challenges_auth_key_id_fkey",
            },
        )
        self.assertEqual(
            {item["name"] for item in inventory["triggers"]},
            {"AA_browser_acceptance_binding_evidence", "browser_acceptance_auth_keys_reject_truncate"},
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

    def test_supervisor_bounds_delayed_cleanup_command_and_leaves_cleanup_unverified(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-cleanup-deadline-") as directory:
            lock_path = Path(directory) / "lock"
            lock_path.write_bytes(b"")
            lock_fd = RESTORE_READINESS.os.open(lock_path, RESTORE_READINESS.os.O_RDWR)
            lock_info = RESTORE_READINESS.os.fstat(lock_fd)
            manifest = {
                "global_lock": {"device": lock_info.st_dev, "inode": lock_info.st_ino},
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
            with (
                patch.object(RESTORE_READINESS, "load_manifest", return_value=manifest),
                patch.object(RESTORE_READINESS, "process_start_ticks", side_effect=start_ticks),
                patch.object(RESTORE_READINESS, "make_runtime_args", return_value=None),
                patch.object(RESTORE_READINESS, "Drill", DelayedCleanupDrill),
                patch.object(RESTORE_READINESS, "remove_finished_run", return_value=True),
            ):
                status = RESTORE_READINESS.supervise(Path(directory) / "ownership.json", lock_fd)

            self.assertEqual(status, 1)
            self.assertLess(time.monotonic() - started, 1)
            self.assertFalse(manifest["cleanup_verified"])

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

    def test_recovery_refreshes_an_expired_cleanup_deadline_after_lock(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rr-recovery-deadline-") as directory:
            lock_path = Path(directory) / "lock"
            lock_path.write_bytes(b"")
            descriptor = RESTORE_READINESS.os.open(lock_path, RESTORE_READINESS.os.O_RDWR)
            lock_info = RESTORE_READINESS.os.fstat(descriptor)
            lock_record = {"device": lock_info.st_dev, "inode": lock_info.st_ino}
            original_deadline = time.monotonic() - 1
            manifest = {"global_lock": lock_record, "cleanup_deadline": original_deadline}
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

            def remove_run(drill: Any, deadline: float) -> bool:
                observed["remove_deadline"] = deadline
                self.assertEqual(drill.manifest["cleanup_deadline"], deadline)
                return True

            started = time.monotonic()
            output = io.StringIO()
            with (
                patch.object(RESTORE_READINESS.os, "geteuid", return_value=0),
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
