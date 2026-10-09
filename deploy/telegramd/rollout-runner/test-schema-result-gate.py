#!/usr/bin/env python3
"""Fixtures for selected-checkout Atlas migration gate decisions."""
from __future__ import annotations

import base64
from contextlib import redirect_stderr, redirect_stdout
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


SCRIPT_DIR = Path(__file__).resolve().parent
GATE = SCRIPT_DIR / "schema-result-gate.py"
PHASE_RUNNER = SCRIPT_DIR.parents[2] / ".github" / "scripts" / "schema-phase-runner.py"
PHASE_RUNNER_SPEC = importlib.util.spec_from_file_location(
    "schema_phase_runner", PHASE_RUNNER
)
if PHASE_RUNNER_SPEC is None or PHASE_RUNNER_SPEC.loader is None:
    raise RuntimeError("schema phase runner could not be loaded")
SCHEMA_PHASE_RUNNER = importlib.util.module_from_spec(PHASE_RUNNER_SPEC)
PHASE_RUNNER_SPEC.loader.exec_module(SCHEMA_PHASE_RUNNER)
VERSIONS = [
    "20261005000060",
    "20261005000061",
    "20261006000062",
    "20261006000063",
    "20261007000064",
    "20261007000065",
    "20261007000066",
    "20261008000067",
    "20261008000068",
    "20261008000069",
]


class SchemaGateFixtures(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="schema-gate-fixtures-")
        self.root = Path(self.temp.name)
        self.repo = self.root / "checkout"
        self.migrations = self.repo / "migrations"
        self.migrations.mkdir(parents=True)
        self.evidence = self.root / "evidence"
        self.evidence.mkdir(mode=0o700)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.write_command("git", self.git_mock())
        self.write_command("docker", self.docker_mock())
        for index, version in enumerate(VERSIONS, start=60):
            self.write_migration(version, f"CREATE TABLE gate_fixture_{index} (id BIGINT PRIMARY KEY);\n")
        self.hash_migrations()

    def tearDown(self) -> None:
        self.temp.cleanup()

    def write_command(self, name: str, body: str) -> None:
        path = self.bin / name
        path.write_text(body, encoding="utf-8")
        path.chmod(0o700)

    @staticmethod
    def git_mock() -> str:
        return """#!/usr/bin/env python3
import os
import sys
if sys.argv[1:] and sys.argv[-1] == "migrations/":
    sys.stdout.write(os.environ.get("MOCK_GIT_STATUS", ""))
    raise SystemExit(int(os.environ.get("MOCK_GIT_STATUS_EXIT", "0")))
raise SystemExit(90)
"""

    @staticmethod
    def docker_mock() -> str:
        return """#!/usr/bin/env python3
import json
import os
import sys
args = sys.argv[1:]
if args[:3] == ["compose", "config", "--format"]:
    source = os.environ.get("MOCK_MOUNT_SOURCE", os.environ["MOCK_MIGRATIONS_DIR"])
    read_only = os.environ.get("MOCK_MOUNT_READ_ONLY", "true") == "true"
    print(json.dumps({"services": {"migrate": {"volumes": [
        {"type": "bind", "source": source, "target": "/migrations", "read_only": read_only}
    ]}}}))
    raise SystemExit(0)
if "validate" in args:
    raise SystemExit(int(os.environ.get("MOCK_ATLAS_VALIDATE_STATUS", "0")))
if "psql" in args:
    status = int(os.environ.get("MOCK_DB_QUERY_STATUS", "0"))
    if status:
        raise SystemExit(status)
    rows = os.environ.get("MOCK_DB_ROWS_FILE")
    if rows:
        sys.stdout.write(open(rows, encoding="utf-8").read())
    raise SystemExit(0)
raise SystemExit(91)
"""

    def write_migration(self, version: str, sql: str) -> None:
        path = self.migrations / f"{version}_fixture.sql"
        path.write_text(sql, encoding="utf-8")

    def hash_migrations(self) -> None:
        atlas = shutil.which("atlas")
        if not atlas:
            self.fail("Atlas CLI must be installed to generate valid fixture checksums")
        result = subprocess.run(
            [atlas, "migrate", "hash", "--dir", f"file://{self.migrations}"],
            cwd=self.repo,
            capture_output=True,
            text=True,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)

    def manifest(self) -> dict[str, str]:
        result: dict[str, str] = {}
        for line in (self.migrations / "atlas.sum").read_text(encoding="utf-8").splitlines()[1:]:
            filename, checksum = line.split(" ")
            result[filename[:14]] = checksum.removeprefix("h1:")
        return result

    def rows(
        self,
        versions: list[str] | None = None,
        overrides: dict[str, tuple[str, str, str, str, str, str]] | None = None,
        extra: list[str] | None = None,
    ) -> str:
        manifest = self.manifest()
        selected = versions if versions is not None else list(manifest)
        states = overrides or {}
        records: list[str] = []
        for version in selected:
            checksum = manifest.get(version, base64.b64encode(b"x" * 32).decode("ascii"))
            revision_type, applied, total, error, error_stmt, partial = states.get(
                version, ("2", "1", "1", "false", "false", "false")
            )
            records.append(
                "\t".join(
                    [version, checksum, revision_type, applied, total, error, error_stmt, partial]
                )
            )
        for version in extra or []:
            records.append(
                "\t".join(
                    [version, base64.b64encode(b"x" * 32).decode("ascii"),
                     "2", "1", "1", "false", "false", "false"]
                )
            )
        return "".join(f"{record}\n" for record in sorted(records))

    def run_gate(
        self,
        mode: str,
        rows: str,
        *,
        git_status: str = "",
        atlas_status: int = 0,
        db_status: int = 0,
        mount_source: str | None = None,
        mount_read_only: bool = True,
    ) -> subprocess.CompletedProcess[str]:
        rows_file = self.root / "database-rows.tsv"
        rows_file.write_text(rows, encoding="utf-8")
        env = os.environ.copy()
        env.update(
            {
                "PATH": f"{self.bin}{os.pathsep}{env['PATH']}",
                "MOCK_GIT_STATUS": git_status,
                "MOCK_ATLAS_VALIDATE_STATUS": str(atlas_status),
                "MOCK_DB_QUERY_STATUS": str(db_status),
                "MOCK_DB_ROWS_FILE": str(rows_file),
                "MOCK_MIGRATIONS_DIR": str(self.migrations),
                "MOCK_MOUNT_SOURCE": mount_source or str(self.migrations),
                "MOCK_MOUNT_READ_ONLY": str(mount_read_only).lower(),
            }
        )
        return subprocess.run(
            [sys.executable, str(GATE), "check", mode, str(self.evidence), str(self.repo)],
            cwd=self.repo,
            env=env,
            capture_output=True,
            text=True,
            check=False,
        )

    def evidence_text(self, mode: str) -> str:
        return (self.evidence / f"schema-result-gate-{mode}.tsv").read_text(encoding="utf-8")

    def test_post_deploy_accepts_complete_migrations_60_through_69(self) -> None:
        result = self.run_gate("post", self.rows())
        self.assertEqual(result.returncode, 0, result.stderr)
        evidence = self.evidence_text("post")
        self.assertIn("revision_set=complete", evidence)
        self.assertIn("revision_20261008000069=pass", evidence)

    def test_pre_deploy_accepts_a_complete_ordered_prefix(self) -> None:
        result = self.run_gate("pre", self.rows(VERSIONS[:7]))
        self.assertEqual(result.returncode, 0, result.stderr)
        evidence = self.evidence_text("pre")
        self.assertIn("ordered_prefix=true", evidence)
        self.assertIn("revision_set=prefix", evidence)

    def test_additive_migration_passes_after_checksum_and_revision_are_added(self) -> None:
        additive_version = "20261009000070"
        self.write_migration(
            additive_version,
            "CREATE TABLE gate_fixture_additive (id BIGINT PRIMARY KEY);\n",
        )
        self.hash_migrations()
        all_versions = VERSIONS + [additive_version]
        result = self.run_gate("post", self.rows(all_versions))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f"revision_{additive_version}=pass", self.evidence_text("post"))

    def test_pre_deploy_rejects_a_gap_in_the_applied_prefix(self) -> None:
        gap_versions = VERSIONS[:3] + VERSIONS[4:6]
        result = self.run_gate("pre", self.rows(gap_versions))
        self.assertNotEqual(result.returncode, 0)
        evidence = self.evidence_text("pre")
        self.assertIn("ordered_prefix=false", evidence)
        self.assertIn("revision_set=reject", evidence)

    def test_post_deploy_rejects_missing_expected_revision(self) -> None:
        result = self.run_gate("post", self.rows(VERSIONS[:-1]))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("revision_20261008000069=missing", self.evidence_text("post"))

    def test_rejects_unexpected_database_revision(self) -> None:
        result = self.run_gate("post", self.rows(extra=["20261009000070"]))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("revision_20261009000070=unexpected", self.evidence_text("post"))

    def test_rejects_revision_hash_mismatch(self) -> None:
        rows = self.rows(overrides={VERSIONS[-1]: (
            "2", "1", "1", "false", "false", "false"
        )})
        lines = rows.splitlines()
        fields = lines[-1].split("\t")
        fields[1] = base64.b64encode(b"wrong checksum".ljust(32, b"!")).decode("ascii")
        lines[-1] = "\t".join(fields)
        result = self.run_gate("post", "\n".join(lines) + "\n")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("revision_20261008000069=hash_mismatch", self.evidence_text("post"))

    def test_rejects_version_69_failed_record_even_when_version_set_is_complete(self) -> None:
        result = self.run_gate(
            "post",
            self.rows(overrides={VERSIONS[-1]: (
                "2", "1", "1", "true", "true", "false"
            )}),
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("revision_20261008000069=partial_or_failed", self.evidence_text("post"))

    def test_rejects_partial_nontransactional_revision_without_repair(self) -> None:
        result = self.run_gate(
            "post",
            self.rows(overrides={VERSIONS[-1]: (
                "1", "1", "2", "true", "true", "true"
            )}),
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("revision_20261008000069=partial_or_failed", self.evidence_text("post"))

    def test_rejects_dirty_migration_inputs(self) -> None:
        result = self.run_gate(
            "post",
            self.rows(),
            git_status=" M migrations/20261008000069_fixture.sql\n",
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("migration_inputs_clean=false", self.evidence_text("post"))

    def test_rejects_migration_mount_that_is_writable_or_from_another_path(self) -> None:
        for index, options in enumerate(
            [
                {"mount_read_only": False},
                {"mount_source": str(self.root / "other-migrations")},
            ]
        ):
            with self.subTest(index=index):
                self.evidence = self.root / f"evidence-{index}"
                self.evidence.mkdir(mode=0o700)
                result = self.run_gate("post", self.rows(), **options)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("migration_mount_read_only=false", self.evidence_text("post"))

    def test_rejects_atlas_checksum_validation_failure(self) -> None:
        result = self.run_gate("post", self.rows(), atlas_status=1)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("atlas_checksum_valid=false", self.evidence_text("post"))

    def test_rejects_file_changed_without_updated_atlas_checksum(self) -> None:
        path = self.migrations / f"{VERSIONS[-1]}_fixture.sql"
        path.write_text(path.read_text(encoding="utf-8") + "SELECT 1;\n", encoding="utf-8")
        result = self.run_gate("post", self.rows(), atlas_status=1)
        self.assertNotEqual(result.returncode, 0)
        evidence = self.evidence_text("post")
        self.assertIn("atlas_checksum_valid=false", evidence)

    def test_rejects_unavailable_database_evidence(self) -> None:
        result = self.run_gate("post", "", db_status=1)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("database_revision_query=unavailable", self.evidence_text("post"))


class SchemaPhaseFixtures(unittest.TestCase):
    checkout_sha = "b" * 40
    canary = "PHASE-CANARY-93bd7"
    synthetic_password = "synthetic-ci-password-71fd"
    synthetic_admin_token = "synthetic-admin-token-2a4c"

    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="schema-phase-fixtures-")
        self.root = Path(self.temp.name)
        self.workspace = self.root / "workspace"
        self.workspace.mkdir()
        self.runner_temp = self.root / "runner-temp"
        self.runner_temp.mkdir(mode=0o700)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.command_log = self.root / "commands.jsonl"
        self.write_command("docker", self.child_command("MOCK_DOCKER_STATUS"))
        self.write_command("bash", self.child_command("MOCK_GATE_STATUS"))
        self.write_command("git", self.git_command())

    def tearDown(self) -> None:
        self.temp.cleanup()

    def write_command(self, name: str, body: str) -> None:
        path = self.bin / name
        path.write_text(body, encoding="utf-8")
        path.chmod(0o700)

    def child_command(self, status_variable: str) -> str:
        return f"""#!/usr/bin/env python3
import json
import os
import sys
args = sys.argv[1:]
with open(os.environ["MOCK_PHASE_COMMAND_LOG"], "a", encoding="utf-8") as output:
    output.write(json.dumps({{"program": os.path.basename(sys.argv[0]), "args": args}}) + "\\n")
payload = " ".join([
    os.environ["MOCK_PHASE_CANARY"],
    os.environ["POSTGRES_PASSWORD"],
    os.environ["MOCK_ADMIN_TOKEN"],
    "::error::forged phase annotation ::stop-commands::forged",
])
print(payload)
print(payload, file=sys.stderr)
raise SystemExit(int(os.environ.get("{status_variable}", "0")))
"""

    @staticmethod
    def git_command() -> str:
        return """#!/usr/bin/env python3
import os
import sys
if sys.argv[1:] == ["rev-parse", "HEAD"]:
    print(os.environ.get("MOCK_GIT_SHA", ""))
    raise SystemExit(0)
raise SystemExit(90)
"""

    def env(self, **overrides: str) -> dict[str, str]:
        environment = os.environ.copy()
        environment.update(
            {
                "PATH": f"{self.bin}{os.pathsep}{environment['PATH']}",
                "GITHUB_WORKSPACE": str(self.workspace),
                "RUNNER_TEMP": str(self.runner_temp),
                "POSTGRES_PASSWORD": self.synthetic_password,
                "MOCK_ADMIN_TOKEN": self.synthetic_admin_token,
                "MOCK_PHASE_CANARY": self.canary,
                "MOCK_PHASE_COMMAND_LOG": str(self.command_log),
                "MOCK_GIT_SHA": self.checkout_sha,
                "MOCK_DOCKER_STATUS": "0",
                "MOCK_GATE_STATUS": "0",
            }
        )
        environment.update(overrides)
        return environment

    def run_phase(
        self,
        phase: str,
        *,
        docker_status: int = 0,
        gate_status: int = 0,
    ) -> subprocess.CompletedProcess[str]:
        self.command_log.write_text("", encoding="utf-8")
        environment = self.env(
            MOCK_DOCKER_STATUS=str(docker_status),
            MOCK_GATE_STATUS=str(gate_status),
        )
        return subprocess.run(
            [sys.executable, str(PHASE_RUNNER), phase],
            cwd=self.workspace,
            env=environment,
            capture_output=True,
            text=True,
            check=False,
        )

    def command_records(self) -> list[dict[str, object]]:
        return [
            json.loads(line)
            for line in self.command_log.read_text(encoding="utf-8").splitlines()
        ]

    def test_each_phase_reports_only_its_fixed_phase_and_preserves_status(self) -> None:
        cases = [
            ("service-start", 4, 0, ["compose", "up", "-d", "--wait", "postgres"]),
            (
                "migrate-apply",
                3,
                0,
                [
                    "compose",
                    "run",
                    "--rm",
                    "--no-deps",
                    "migrate",
                    "migrate",
                    "apply",
                    "--dir",
                    "file:///migrations",
                    "--url",
                    f"postgres://postgres:{self.synthetic_password}@postgres:5432/telegram?sslmode=disable",
                ],
            ),
            (
                "pre-check",
                0,
                5,
                [
                    "deploy/telegramd/rollout-runner/schema-result-gate.sh",
                    "check",
                    "pre",
                    str(self.runner_temp / "schema-gate-evidence"),
                    str(self.workspace),
                ],
            ),
            (
                "post-check",
                0,
                6,
                [
                    "deploy/telegramd/rollout-runner/schema-result-gate.sh",
                    "check",
                    "post",
                    str(self.runner_temp / "schema-gate-evidence"),
                    str(self.workspace),
                ],
            ),
        ]

        for phase, docker_status, gate_status, expected_args in cases:
            with self.subTest(phase=phase):
                result = self.run_phase(
                    phase,
                    docker_status=docker_status,
                    gate_status=gate_status,
                )
                status = docker_status or gate_status
                self.assertEqual(result.returncode, status)
                self.assertEqual(
                    result.stdout,
                    f"::error::PostgreSQL Atlas schema gate failed "
                    f"(category: phase-failure; phase: {phase}; exit: {status}; "
                    f"checked-out commit: {self.checkout_sha}; details redacted)\n",
                )
                self.assertEqual(result.stderr, "")
                for secret in (
                    self.canary,
                    self.synthetic_password,
                    self.synthetic_admin_token,
                ):
                    self.assertNotIn(secret, result.stdout + result.stderr)
                self.assertEqual(result.stdout.count("::error::"), 1)
                self.assertNotIn("::stop-commands::", result.stdout)
                records = self.command_records()
                expected_program = (
                    "docker"
                    if phase in {"service-start", "migrate-apply"}
                    else "bash"
                )
                self.assertEqual(
                    records,
                    [{"program": expected_program, "args": expected_args}],
                )

        self.assertEqual(
            stat.S_IMODE((self.runner_temp / "schema-gate-evidence").stat().st_mode),
            0o700,
        )

    def test_failed_evidence_directory_creation_is_pre_check_and_stops_gate_command(
        self,
    ) -> None:
        (self.runner_temp / "schema-gate-evidence").mkdir(mode=0o700)
        result = self.run_phase("pre-check", gate_status=5)
        self.assertEqual(result.returncode, 1)
        self.assertIn("phase: pre-check; exit: 1;", result.stdout)
        self.assertEqual(result.stdout.count("::error::"), 1)
        self.assertNotIn(self.canary, result.stdout + result.stderr)
        self.assertEqual(self.command_records(), [])

    def test_missing_ambiguous_and_unknown_phase_evidence_is_unavailable(self) -> None:
        cases = [
            [],
            ["service-start", "migrate-apply"],
            ["unknown-phase"],
            [self.canary, "pre-check"],
            ["x" * 1_000_001],
        ]
        for evidence in cases:
            with self.subTest(evidence=evidence):
                annotation = SCHEMA_PHASE_RUNNER.format_annotation(
                    evidence, 3, self.checkout_sha
                )
                self.assertIsNotNone(annotation)
                self.assertIn("phase: unavailable; exit: 3;", annotation or "")
                for secret in (
                    self.canary,
                    self.synthetic_password,
                    self.synthetic_admin_token,
                ):
                    self.assertNotIn(secret, annotation or "")
                self.assertNotIn("assertion", annotation or "")

    def test_invalid_commit_is_redacted_and_success_has_no_failure_annotation(self) -> None:
        annotation = SCHEMA_PHASE_RUNNER.format_annotation(
            ["post-check"], 7, f"{self.checkout_sha}{self.canary}"
        )
        self.assertIn("phase: post-check; exit: 7;", annotation or "")
        self.assertIn("checked-out commit: unavailable", annotation or "")
        self.assertIsNone(
            SCHEMA_PHASE_RUNNER.format_annotation(
                ["service-start"], 0, self.checkout_sha
            )
        )

    def test_reporter_error_falls_back_without_changing_command_status(self) -> None:
        output = io.StringIO()
        errors = io.StringIO()
        with (
            mock.patch.object(SCHEMA_PHASE_RUNNER, "run_command", return_value=7),
            mock.patch.object(
                SCHEMA_PHASE_RUNNER,
                "format_annotation",
                side_effect=RuntimeError(self.canary),
            ),
            redirect_stdout(output),
            redirect_stderr(errors),
        ):
            status = SCHEMA_PHASE_RUNNER.run_phase("service-start", self.env())

        self.assertEqual(status, 7)
        self.assertEqual(
            output.getvalue(),
            "::error::PostgreSQL Atlas schema gate failed "
            "(category: phase-failure; phase: unavailable; exit: 7; "
            "checked-out commit: unavailable; details redacted)\n",
        )
        self.assertEqual(errors.getvalue(), "")
        self.assertNotIn(self.canary, output.getvalue() + errors.getvalue())

    def test_reporter_crash_does_not_change_command_status_or_expose_error(self) -> None:
        output = io.StringIO()
        errors = io.StringIO()
        with (
            mock.patch.object(SCHEMA_PHASE_RUNNER, "run_command", return_value=9),
            mock.patch.object(
                SCHEMA_PHASE_RUNNER,
                "emit_failure",
                side_effect=RuntimeError(self.canary),
            ),
            redirect_stdout(output),
            redirect_stderr(errors),
        ):
            status = SCHEMA_PHASE_RUNNER.run_phase("service-start", self.env())

        self.assertEqual(status, 9)
        self.assertEqual(
            output.getvalue(),
            "::error::PostgreSQL Atlas schema gate failed "
            "(category: phase-failure; phase: unavailable; exit: 9; "
            "checked-out commit: unavailable; details redacted)\n",
        )
        self.assertEqual(errors.getvalue(), "")
        self.assertNotIn(self.canary, output.getvalue() + errors.getvalue())

    def test_workflow_keeps_phase_order_and_always_runs_teardown(self) -> None:
        workflow = (PHASE_RUNNER.parents[1] / "workflows" / "ci.yml").read_text(
            encoding="utf-8"
        )
        positions = [
            workflow.index(f"      - name: {phase}\n")
            for phase in ("service-start", "migrate-apply", "pre-check", "post-check")
        ]
        teardown = workflow.index("      - name: Tear down schema gate database\n")
        self.assertEqual(positions, sorted(positions))
        self.assertGreater(teardown, positions[-1])
        self.assertIn("        if: always()\n", workflow[teardown : teardown + 160])


if __name__ == "__main__":
    unittest.main(verbosity=2)
