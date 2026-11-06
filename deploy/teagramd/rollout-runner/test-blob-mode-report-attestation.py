#!/usr/bin/env python3
"""Fixtures for the one-shot blob-mode report-loss attestation."""

from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import os
import pathlib
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch


SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
OPERATION = SCRIPT_DIR / "attest-blob-mode-reports.py"
MODE_FIXTURES = SCRIPT_DIR / "test-blob-mode-state.py"
FIXTURE_SPEC = importlib.util.spec_from_file_location("report_attestation_mode_fixtures", MODE_FIXTURES)
assert FIXTURE_SPEC is not None and FIXTURE_SPEC.loader is not None
mode_fixtures = importlib.util.module_from_spec(FIXTURE_SPEC)
FIXTURE_SPEC.loader.exec_module(mode_fixtures)

S3_MANIFEST = f"01/1\t5\t{'a' * 64}\n".encode("ascii")
REFERENCE_ROWS = b"01/1\t5\n"


def import_operation(test: unittest.TestCase):
    if not OPERATION.is_file():
        test.fail("one-shot report attestation operation is missing")
    spec = importlib.util.spec_from_file_location("blob_mode_report_attestation", OPERATION)
    if spec is None or spec.loader is None:
        test.fail("cannot load the one-shot report attestation operation")
    operation = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(operation)
    return operation


def snapshot_files(root: pathlib.Path) -> dict[str, bytes]:
    result = {}
    if not root.exists():
        return result
    for path in sorted(root.rglob("*")):
        if path.is_file() and not path.is_symlink():
            result[str(path.relative_to(root))] = path.read_bytes()
    return result


class BlobModeReportAttestationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.operation = import_operation(self)
        self.fixture = mode_fixtures.BlobModeStateTests("runTest")
        self.fixture.setUp()

    def make_authority(
        self,
        root: pathlib.Path,
        *,
        missing_reports: int = 5,
        final_outcome: str = "s3-accepted",
    ) -> tuple[pathlib.Path, pathlib.Path, pathlib.Path, list[dict[str, object]]]:
        state_dir, report_root, lock_path = self.fixture.create_initial_authority(root)
        outcomes = ["s3-accepted", "recovered-local", "s3-accepted", "recovered-local", "s3-accepted"]
        if final_outcome == "recovered-local":
            outcomes.append("recovered-local")
        with (
            patch.object(mode_fixtures.blob_mode, "require_runner_lock"),
            patch.object(mode_fixtures.blob_mode, "file_stat", side_effect=self.fixture.allow_unowned_private_files),
            patch.object(mode_fixtures.blob_mode.os, "fchown"),
            patch.object(mode_fixtures.blob_mode.os, "chown"),
        ):
            for outcome in outcomes:
                self.fixture.publish(state_dir, report_root, lock_path, root, outcome)
            records, _head, _mode_bytes = mode_fixtures.blob_mode.read_authority(state_dir, report_root)
            for record in records[:missing_reports]:
                mode_fixtures.blob_mode.report_read_path(report_root, record["transition_id"]).unlink()
        return state_dir, report_root, lock_path, records

    def operation_context(
        self,
        root: pathlib.Path,
        state_dir: pathlib.Path,
        report_root: pathlib.Path,
        lock_path: pathlib.Path,
        census: tuple[bytes, bytes] = (S3_MANIFEST, REFERENCE_ROWS),
    ):
        operation = self.operation
        archive_root = root / "archives"
        return (
            patch.object(operation, "TEST_MODE", True),
            patch.object(operation, "ROOT_UID", os.geteuid()),
            patch.object(operation, "CHECKOUT", state_dir.parents[1]),
            patch.object(operation, "STATE_DIR", state_dir),
            patch.object(operation, "REPORT_ROOT", report_root),
            patch.object(operation, "ARCHIVE_ROOT", archive_root),
            patch.object(operation, "LOCK_PATH", lock_path),
            patch.object(operation.transition, "acquire_lock"),
            patch.object(operation.mode, "require_runner_lock"),
            patch.object(operation.mode, "file_stat", side_effect=self.fixture.allow_unowned_private_files),
            patch.object(operation.mode.os, "fchown"),
            patch.object(operation.mode.os, "chown"),
            patch.object(operation, "capture_live_census", return_value=census),
        )

    def test_live_census_accepts_legacy_flat_part_key(self) -> None:
        key = "parts/deadbeef000000000000000000000000"
        s3_manifest = f"{key}\t7\t{'b' * 64}\n".encode("ascii")
        reference_rows = f"{key}\t7\n".encode("ascii")

        census, object_count, byte_total = self.operation.validate_live_census(s3_manifest, reference_rows)

        document = json.loads(census)
        self.assertEqual(object_count, 1)
        self.assertEqual(byte_total, 7)
        self.assertEqual(document["references"], [{"key": key, "logical_bytes": 7}])

    def test_five_missing_reports_are_attested_and_encrypt_preflight_accepts_them(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path, records = self.make_authority(root)
            old_authority = snapshot_files(state_dir)
            old_reports = snapshot_files(report_root)
            patches = self.operation_context(root, state_dir, report_root, lock_path)
            with contextlib.ExitStack() as stack:
                for item in patches:
                    stack.enter_context(item)
                result = self.operation.run_attestation()

                authority = self.operation.mode.current_authority(state_dir, report_root)
                self.assertEqual(authority["transition_id"], records[-1]["transition_id"])
                self.assertEqual(snapshot_files(state_dir), old_authority)
                current_reports = snapshot_files(report_root)
                self.assertTrue(all(current_reports[name] == content for name, content in old_reports.items()))
                self.assertEqual(result["missing_report_count"], 5)
                self.assertEqual(result["object_count"], 1)
                self.assertEqual(result["byte_total"], 5)

                archive_dirs = list((root / "archives").iterdir())
                self.assertEqual(len(archive_dirs), 1)
                archive = archive_dirs[0]
                self.assertEqual((archive / "authority" / "mode.json").read_bytes(), old_authority["mode.json"])
                self.assertEqual(
                    snapshot_files(archive / "authority" / "journal"),
                    {name.removeprefix("journal/"): value for name, value in old_authority.items() if name.startswith("journal/")},
                )
                self.assertEqual(snapshot_files(archive / "reports"), old_reports)
                self.assertEqual(stat_mode(archive), 0o700)
                self.assertEqual(stat_mode(report_root / self.operation.mode.REPORT_LOSS_CENSUS_MANIFEST), 0o600)

                checkout = state_dir.parents[1]
                (checkout / ".env").write_text("fixture environment\n", encoding="utf-8")
                (checkout / ".env.rustfs-parked").write_text(
                    "RUSTFS_ROOT_ACCESS_KEY=" + "a" * 20 + "\n"
                    "RUSTFS_ROOT_SECRET_KEY=" + "b" * 64 + "\n"
                    "TG_BLOB_S3_ACCESS_KEY_ID=" + "c" * 20 + "\n"
                    "TG_BLOB_S3_SECRET_ACCESS_KEY=" + "d" * 64 + "\n",
                    encoding="utf-8",
                )
                os.chmod(checkout / ".env", 0o600)
                os.chmod(checkout / ".env.rustfs-parked", 0o600)
                secret_dir = checkout / ".secrets"
                secret_dir.mkdir(mode=0o700)
                os.chmod(secret_dir, 0o700)
                (secret_dir / "teagramd-blob-secret-key").write_text("d" * 64, encoding="ascii")
                os.chmod(secret_dir / "teagramd-blob-secret-key", 0o444)

                transition = self.operation.transition
                authority_helper = transition.import_mode_helper()
                args = [
                    "blob-transition-runner.py",
                    "deployment-env",
                    "--checkout", str(checkout),
                    "--state-dir", str(state_dir),
                    "--report-root", str(report_root),
                    "--lock-path", str(lock_path),
                ]
                credential_fixture = contextlib.nullcontext()
                if os.geteuid() != 0:
                    credential_fixture = patch.object(
                        transition,
                        "rustfs_compose_environment_files",
                        return_value=[str(checkout / ".env"), str(checkout / ".env.rustfs-parked")],
                    )
                with (
                    patch.object(transition, "TEST_MODE", True),
                    patch.object(transition, "import_mode_helper", return_value=authority_helper),
                    patch.object(authority_helper, "require_runner_lock"),
                    patch.object(
                        authority_helper,
                        "file_stat",
                        side_effect=self.fixture.allow_unowned_private_files,
                    ),
                    credential_fixture,
                    patch.object(sys, "argv", args),
                    contextlib.redirect_stdout(io.StringIO()) as output,
                ):
                    self.assertEqual(transition.main(), 0)
                self.assertEqual(json.loads(output.getvalue())["outcome"], "s3-accepted")

    def test_retry_cleans_publication_after_manifest_write_failure(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path, _records = self.make_authority(root)
            patches = self.operation_context(root, state_dir, report_root, lock_path)
            original_write = self.operation.write_private_file

            def fail_after_census_write(path: pathlib.Path, content: bytes) -> None:
                original_write(path, content)
                if path.name == self.operation.CENSUS_MANIFEST_NAME:
                    raise self.operation.AttestationReject("attestation-output-sync")

            with contextlib.ExitStack() as stack:
                for item in patches:
                    stack.enter_context(item)
                with patch.object(self.operation, "write_private_file", side_effect=fail_after_census_write):
                    with self.assertRaises(self.operation.AttestationReject):
                        self.operation.run_attestation()

                self.assertFalse((report_root / self.operation.ARCHIVE_MANIFEST_NAME).exists())
                self.assertFalse((report_root / self.operation.CENSUS_MANIFEST_NAME).exists())
                self.assertEqual(list((root / "archives").iterdir()), [])
                self.assertFalse(self.operation.recovery_marker_path().exists())

                result = self.operation.run_attestation()
                self.assertEqual(result["missing_report_count"], 5)

    def test_retry_cleans_publication_after_post_verification_failure(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path, _records = self.make_authority(root)
            patches = self.operation_context(root, state_dir, report_root, lock_path)
            original_authority = self.operation.mode.current_authority
            calls = 0

            def fail_after_publication(*args, **kwargs):
                nonlocal calls
                calls += 1
                if calls == 2:
                    raise self.operation.mode.Reject("report-unavailable")
                return original_authority(*args, **kwargs)

            with contextlib.ExitStack() as stack:
                for item in patches:
                    stack.enter_context(item)
                with patch.object(self.operation.mode, "current_authority", side_effect=fail_after_publication):
                    with self.assertRaises(self.operation.AttestationReject):
                        self.operation.run_attestation()

                self.assertFalse((report_root / self.operation.ARCHIVE_MANIFEST_NAME).exists())
                self.assertFalse((report_root / self.operation.CENSUS_MANIFEST_NAME).exists())
                self.assertEqual(list((root / "archives").iterdir()), [])
                self.assertFalse(self.operation.recovery_marker_path().exists())

                result = self.operation.run_attestation()
                self.assertEqual(result["missing_report_count"], 5)

    def test_retry_recovers_stale_publication_marker(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path, _records = self.make_authority(root)
            patches = self.operation_context(root, state_dir, report_root, lock_path)
            original_authority = self.operation.mode.current_authority
            calls = 0

            def invalidate_post_publication(*args, **kwargs):
                nonlocal calls
                calls += 1
                if calls == 2:
                    (report_root / self.operation.CENSUS_MANIFEST_NAME).write_bytes(b"damaged\n")
                return original_authority(*args, **kwargs)

            with contextlib.ExitStack() as stack:
                for item in patches:
                    stack.enter_context(item)
                with (
                    patch.object(self.operation.mode, "current_authority", side_effect=invalidate_post_publication),
                    patch.object(
                        self.operation,
                        "_rollback_incomplete_publication",
                        side_effect=self.operation.AttestationReject("simulated-process-interruption"),
                    ),
                ):
                    with self.assertRaises(self.operation.AttestationReject):
                        self.operation.run_attestation()

                self.assertTrue(self.operation.recovery_marker_path().exists())
                self.assertTrue((report_root / self.operation.ARCHIVE_MANIFEST_NAME).exists())
                result = self.operation.run_attestation()
                self.assertEqual(result["missing_report_count"], 5)
                self.assertFalse(self.operation.recovery_marker_path().exists())

    def test_live_census_uses_compose_config_s3_census_and_database_select_only(self) -> None:
        calls: list[list[str]] = []
        environment = {"TG_BLOB_S3_SECRET_ACCESS_KEY": "never-print-this"}
        record = {
            "backend": {
                "kind": "s3",
                "endpoint": "https://s3.example.test",
                "bucket": "teagram",
                "prefix": "blobs/",
            },
        }
        rendered = {
            "services": {
                "blob-migrate": {
                    "profiles": ["migration"],
                    "environment": {},
                },
                "blob-encrypt-migrate": {
                    "volumes": [{
                        "type": "bind",
                        "source": str(self.operation.REPORT_ROOT),
                        "target": self.operation.mode.REPORT_TARGET,
                    }],
                },
            },
        }
        expected = [
            ["docker", "compose", "--profile", "migration", "config", "--format", "json"],
            [
                "docker", "compose", "--profile", "migration", "run", "--rm", "--no-deps",
                "blob-migrate", "--direction", "s3-census", "--manifest", "stdout",
            ],
            [
                "docker", "compose", "--profile", "migration", "exec", "-T", "postgres", "psql",
                "-X", "-qAt", "-F", "\t", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "teagram",
                "-c", self.operation.REFERENCE_QUERY,
            ],
        ]
        def run_compose(command, _env):
            calls.append(command)
            if "config" in command:
                return json.dumps(rendered).encode()
            if "s3-census" in command:
                return S3_MANIFEST
            return REFERENCE_ROWS

        with (
            patch.object(
                self.operation.transition,
                "rustfs_compose_environment_files",
                return_value=[
                    str(self.operation.CHECKOUT / ".env"),
                    str(self.operation.CHECKOUT / ".env.rustfs-parked"),
                ],
            ),
            patch.object(
                self.operation.transition,
                "compose_environment_for_files",
                return_value=environment,
            ) as compose_environment,
            patch.object(self.operation.mode, "compose_inventory", return_value=object()) as compose_inventory,
            patch.object(self.operation.mode, "assert_compose_matches") as assert_compose_matches,
            patch.object(self.operation.mode, "backend_from_values", return_value=record["backend"]),
            patch.object(self.operation, "run_compose", side_effect=run_compose),
        ):
            s3_manifest, reference_rows = self.operation.capture_live_census(record)

        self.assertEqual(calls, expected)
        self.assertEqual((s3_manifest, reference_rows), (S3_MANIFEST, REFERENCE_ROWS))
        compose_environment.assert_called_once_with(
            self.operation.CHECKOUT,
            ["docker-compose.yml", "docker-compose.override.yml"],
            include_rustfs_credentials=True,
        )
        compose_inventory.assert_called_once_with(rendered, self.operation.CHECKOUT)
        assert_compose_matches.assert_called_once()
        self.assertFalse(any(command in ("restart", "up", "stop") for call in calls for command in call))
        self.assertNotIn("never-print-this", repr(calls))

    def test_live_census_command_failure_does_not_echo_secret_output(self) -> None:
        secret = "fixture-secret-must-not-be-printed"
        with patch.object(
            self.operation.subprocess,
            "run",
            return_value=SimpleNamespace(returncode=1, stdout=secret.encode(), stderr=secret.encode()),
        ) as run:
            with self.assertRaises(self.operation.AttestationReject) as caught:
                self.operation.run_compose(["docker", "compose", "config"], {"TG_SECRET": secret})

        self.assertEqual(str(caught.exception), "live-census-rejected")
        self.assertNotIn(secret, str(caught.exception))
        self.assertTrue(run.call_args.kwargs["stdout"] == self.operation.subprocess.PIPE)
        self.assertTrue(run.call_args.kwargs["stderr"] == self.operation.subprocess.PIPE)

    def test_refuses_journal_head_mismatch_without_writing_evidence(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path, _records = self.make_authority(root)
            mode_path = state_dir / "mode.json"
            mode_path.write_bytes(b"{}\n")
            before_state = snapshot_files(state_dir)
            before_reports = snapshot_files(report_root)
            patches = self.operation_context(root, state_dir, report_root, lock_path)
            with contextlib.ExitStack() as stack:
                for item in patches:
                    stack.enter_context(item)
                with self.assertRaises(self.operation.AttestationReject) as caught:
                    self.operation.run_attestation()
            self.assertEqual(str(caught.exception), "mode-head-mismatch")
            self.assertEqual(snapshot_files(state_dir), before_state)
            self.assertEqual(snapshot_files(report_root), before_reports)
            self.assertFalse((root / "archives").exists())

    def test_refuses_non_s3_head_without_writing_evidence(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path, _records = self.make_authority(
                root, missing_reports=5, final_outcome="recovered-local"
            )
            before_state = snapshot_files(state_dir)
            before_reports = snapshot_files(report_root)
            patches = self.operation_context(root, state_dir, report_root, lock_path)
            with contextlib.ExitStack() as stack:
                for item in patches:
                    stack.enter_context(item)
                with self.assertRaises(self.operation.AttestationReject) as caught:
                    self.operation.run_attestation()
            self.assertEqual(str(caught.exception), "head-not-s3-accepted")
            self.assertEqual(snapshot_files(state_dir), before_state)
            self.assertEqual(snapshot_files(report_root), before_reports)
            self.assertFalse((root / "archives").exists())

    def test_refuses_missing_or_size_mismatched_referenced_s3_object_before_writing(self) -> None:
        cases = (
            (b"02/2\t5\n", b"01/1\t5\t" + b"a" * 64 + b"\n", "census-reference-missing"),
            (b"01/1\t5\n", b"01/1\t6\t" + b"a" * 64 + b"\n", "census-reference-size-mismatch"),
        )
        for reference_rows, s3_manifest, reason in cases:
            with self.subTest(reason=reason), tempfile.TemporaryDirectory() as temporary:
                root = pathlib.Path(temporary)
                state_dir, report_root, lock_path, _records = self.make_authority(root)
                before_state = snapshot_files(state_dir)
                before_reports = snapshot_files(report_root)
                patches = self.operation_context(root, state_dir, report_root, lock_path, (s3_manifest, reference_rows))
                with contextlib.ExitStack() as stack:
                    for item in patches:
                        stack.enter_context(item)
                    with self.assertRaises(self.operation.AttestationReject) as caught:
                        self.operation.run_attestation()
                self.assertEqual(str(caught.exception), reason)
                self.assertEqual(snapshot_files(state_dir), before_state)
                self.assertEqual(snapshot_files(report_root), before_reports)
                self.assertFalse((root / "archives").exists())

    def test_refuses_already_valid_authority_and_non_report_strict_failure(self) -> None:
        for corrupt_report in (False, True):
            with self.subTest(corrupt_report=corrupt_report), tempfile.TemporaryDirectory() as temporary:
                root = pathlib.Path(temporary)
                state_dir, report_root, lock_path, records = self.make_authority(root, missing_reports=0)
                expected = "report-digest" if corrupt_report else "authority-already-valid"
                if corrupt_report:
                    report = self.operation.mode.report_read_path(report_root, records[0]["transition_id"])
                    report.write_bytes(b"{}"); report.chmod(0o600)
                before_state = snapshot_files(state_dir)
                before_reports = snapshot_files(report_root)
                patches = self.operation_context(root, state_dir, report_root, lock_path)
                with contextlib.ExitStack() as stack:
                    for item in patches:
                        stack.enter_context(item)
                    with self.assertRaises(self.operation.AttestationReject) as caught:
                        self.operation.run_attestation()
                self.assertEqual(str(caught.exception), expected)
                self.assertEqual(snapshot_files(state_dir), before_state)
                self.assertEqual(snapshot_files(report_root), before_reports)
                self.assertFalse((root / "archives").exists())

    def test_attestation_outputs_and_wrapper_never_expose_secrets_or_restart_services(self) -> None:
        wrapper = SCRIPT_DIR.parents[1] / "teagram" / "ops" / "attest-blob-mode-reports.sh"
        self.assertTrue(wrapper.is_file(), "forced-command wrapper is missing")
        source = wrapper.read_text(encoding="utf-8")
        self.assertNotIn("docker restart", source)
        self.assertNotIn("compose up", source)
        self.assertNotIn("rustfs_secret", source.lower())


def stat_mode(path: pathlib.Path) -> int:
    return path.stat().st_mode & 0o777


if __name__ == "__main__":
    unittest.main()
