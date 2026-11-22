#!/usr/bin/env python3
from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import os
import shutil
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from typing import Any
from unittest.mock import patch

SCRIPT_DIR = Path(__file__).resolve().parent
RUNNER = SCRIPT_DIR / "blob-transition-runner.py"
BLOB_MODE_DIR = SCRIPT_DIR.parent.parent / "teagram-demo" / "blob-mode"
QUALIFIER_FIXTURES = BLOB_MODE_DIR / "test-qualify-rustfs-transition.py"
MODE_FIXTURES = BLOB_MODE_DIR / "test-blob-mode-state.py"

def import_from_path(name: str, path: Path) -> Any:
    spec = importlib.util.spec_from_file_location(name, path)
    if spec is None or spec.loader is None:
        raise AssertionError(f"cannot load {path.name}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module

qualifier_fixtures = import_from_path("qualifier_fixtures", QUALIFIER_FIXTURES)
mode_fixtures = import_from_path("mode_fixtures", MODE_FIXTURES)

def live_schema_observation(release_set: str) -> dict[str, Any]:
    migration = qualifier_fixtures.good_migration_evidence(release_set)
    detail_keys = ["applied", "total", "error", "hash"]
    if release_set == "60-70":
        detail_keys.extend(("error_stmt_empty", "partial_hashes_empty"))
    observation = {
        "applied_revisions": migration["target_revisions"],
        "revision_detail": {
            version: {key: detail[key] for key in detail_keys}
            for version, detail in migration["revision_detail"].items()
        },
        "migration_66_schema": migration["migration_66_schema"],
    }
    if release_set in {"60-67", "60-69", "60-70"}:
        observation["migration_67_schema"] = migration["migration_67_schema"]
    return observation

class BlobTransitionEnvironmentTests(unittest.TestCase):

    def test_deployment_environment_requires_s3_authority(self) -> None:
        runner = import_from_path("blob_transition_runner_deploy_environment", RUNNER)
        mode = SimpleNamespace(
            Reject=RuntimeError,
            require_runner_lock=lambda _path: None,
            current_deployment_authority=lambda _state, _reports: (
                {"outcome": "s3-accepted", "backend": {"kind": "s3"}},
                2,
            ),
        )
        with tempfile.TemporaryDirectory(prefix="deploy-s3-environment.") as temporary:
            checkout = Path(temporary) / "teagram"
            checkout.mkdir()
            report_root = Path(temporary) / "reports"
            report_root.mkdir()
            env_files = [
                str(checkout / ".env"),
                str(Path(temporary) / "telegram-server/.env.rustfs-parked"),
            ]
            output = io.StringIO()
            errors = io.StringIO()
            with (
                patch.object(runner, "TEST_MODE", True),
                patch.object(runner, "import_mode_helper", return_value=mode),
                patch.object(runner, "rustfs_compose_environment_files", return_value=env_files),
                contextlib.redirect_stdout(output),
                contextlib.redirect_stderr(errors),
            ):
                runner.deployment_environment(
                    SimpleNamespace(
                        checkout=checkout,
                        state_dir=checkout / ".state/blob-mode",
                        report_root=report_root,
                        lock_path=Path(temporary) / "deploy.lock",
                    )
                )
            self.assertEqual(
                json.loads(output.getvalue()),
                {"outcome": "s3-accepted", "env_files": env_files},
            )
            self.assertEqual(
                errors.getvalue(),
                "blob transition deployment accepted 2 journal records without reports\n",
            )

            mode.current_deployment_authority = lambda _state, _reports: (
                {
                    "outcome": "initial-local",
                    "backend": {"kind": "local", "dir": "/var/lib/teagramd-blobs"},
                },
                0,
            )
            with (
                patch.object(runner, "TEST_MODE", True),
                patch.object(runner, "import_mode_helper", return_value=mode),
                patch.object(runner, "rustfs_compose_environment_files") as credentials,
            ):
                with self.assertRaisesRegex(runner.TransitionReject, "deploy-s3-authority-invalid"):
                    runner.deployment_environment(
                        SimpleNamespace(
                            checkout=checkout,
                            state_dir=checkout / ".state/blob-mode",
                            report_root=report_root,
                            lock_path=Path(temporary) / "deploy.lock",
                        )
                    )
                credentials.assert_not_called()

    def test_deployment_environment_accepts_missing_reports_and_logs_count(self) -> None:
        runner = import_from_path("blob_transition_runner_missing_reports", RUNNER)
        fixtures = mode_fixtures.BlobModeStateTests()
        fixtures.setUp()
        with tempfile.TemporaryDirectory(prefix="deploy-s3-missing-reports.") as temporary:
            root = Path(temporary)
            state_dir, report_root, lock_path = fixtures.create_initial_authority(root)
            with (
                patch.object(mode_fixtures.blob_mode, "require_runner_lock"),
                patch.object(mode_fixtures.blob_mode, "file_stat", side_effect=fixtures.allow_unowned_private_files),
                patch.object(mode_fixtures.blob_mode.os, "fchown"),
                patch.object(mode_fixtures.blob_mode.os, "chown"),
            ):
                fixtures.publish(state_dir, report_root, lock_path, root, "s3-accepted")
                records, _head, _mode_bytes = mode_fixtures.blob_mode.read_authority(state_dir, report_root)
                shutil.rmtree(report_root)

            env_files = [
                str(root / "checkout" / ".env"),
                str(root / "telegram-server" / ".env.rustfs-parked"),
            ]
            output = io.StringIO()
            errors = io.StringIO()
            with (
                patch.object(runner, "TEST_MODE", True),
                patch.object(runner, "import_mode_helper", return_value=mode_fixtures.blob_mode),
                patch.object(runner, "rustfs_compose_environment_files", return_value=env_files),
                patch.object(mode_fixtures.blob_mode, "require_runner_lock"),
                patch.object(
                    mode_fixtures.blob_mode,
                    "file_stat",
                    side_effect=fixtures.allow_unowned_private_files,
                ),
                contextlib.redirect_stdout(output),
                contextlib.redirect_stderr(errors),
            ):
                runner.deployment_environment(
                    SimpleNamespace(
                        checkout=root / "checkout",
                        state_dir=state_dir,
                        report_root=report_root,
                        lock_path=lock_path,
                    )
                )

            self.assertEqual(
                json.loads(output.getvalue()),
                {"outcome": "s3-accepted", "env_files": env_files},
            )
            self.assertEqual(
                errors.getvalue(),
                f"blob transition deployment accepted {len(records)} journal records without reports\n",
            )

    def test_rustfs_environment_files_include_parked_credentials(self) -> None:
        if os.geteuid() != 0:
            self.skipTest("RustFS environment fixture requires root ownership checks")
        runner = import_from_path("blob_transition_runner_parked_env", RUNNER)
        with tempfile.TemporaryDirectory(prefix="rustfs-parked-env.") as temporary:
            root = Path(temporary)
            checkout = root / "opt" / "teagram"
            parked_checkout = root / "opt" / "telegram-server"
            checkout.mkdir(parents=True)
            parked_checkout.mkdir(parents=True)
            (checkout / ".env").write_text("POSTGRES_PASSWORD=fixture\n", encoding="ascii")
            (checkout / ".env").chmod(0o600)
            values = {
                "RUSTFS_ROOT_ACCESS_KEY": "a" * 20,
                "RUSTFS_ROOT_SECRET_KEY": "c" * 64,
                "TG_BLOB_S3_ACCESS_KEY_ID": "b" * 20,
                "TG_BLOB_S3_SECRET_ACCESS_KEY": "d" * 64,
            }
            parked = parked_checkout / ".env.rustfs-parked"
            parked_values = values
            parked.write_text(
                "".join(f"{key}={value}\n" for key, value in parked_values.items()),
                encoding="ascii",
            )
            parked.chmod(0o600)
            secret_dir = checkout / ".secrets"
            secret_dir.mkdir(mode=0o700)
            secret_file = secret_dir / "teagramd-blob-secret-key"
            secret_file.write_text(values["TG_BLOB_S3_SECRET_ACCESS_KEY"], encoding="ascii")
            secret_file.chmod(0o444)

            env_files = runner.rustfs_compose_environment_files(checkout)
        self.assertEqual(env_files[0], str(checkout / ".env"))
        self.assertEqual(env_files[1], str(parked.resolve()))

    def test_rustfs_credentials_reject_mismatched_existing_blob_secret(self) -> None:
        if os.geteuid() != 0:
            self.skipTest("RustFS environment fixture requires root ownership checks")
        runner = import_from_path("blob_transition_runner_mismatched_parked_env", RUNNER)
        with tempfile.TemporaryDirectory(prefix="rustfs-parked-env-mismatch.") as temporary:
            root = Path(temporary)
            checkout = root / "opt" / "teagram"
            parked_checkout = root / "opt" / "telegram-server"
            checkout.mkdir(parents=True)
            parked_checkout.mkdir(parents=True)
            (checkout / ".env").write_text("POSTGRES_PASSWORD=fixture\n", encoding="ascii")
            (checkout / ".env").chmod(0o600)
            parked = parked_checkout / ".env.rustfs-parked"
            parked.write_text(
                "RUSTFS_ROOT_ACCESS_KEY=" + "a" * 20 + "\n"
                "RUSTFS_ROOT_SECRET_KEY=" + "c" * 64 + "\n"
                "TG_BLOB_S3_ACCESS_KEY_ID=" + "b" * 20 + "\n"
                "TG_BLOB_S3_SECRET_ACCESS_KEY=" + "d" * 64 + "\n",
                encoding="ascii",
            )
            parked.chmod(0o600)
            secret_dir = checkout / ".secrets"
            secret_dir.mkdir(mode=0o700)
            secret_file = secret_dir / "teagramd-blob-secret-key"
            secret_file.write_text("e" * 64, encoding="ascii")
            secret_file.chmod(0o444)

            with self.assertRaisesRegex(runner.TransitionReject, "rustfs-credentials-mismatch"):
                runner.rustfs_compose_environment_files(checkout)

class BlobTransitionEvidenceTests(unittest.TestCase):
    def schema_document(self, release_set: str = "60-66") -> tuple[Any, dict[str, Any], dict[str, str]]:
        mode = import_from_path("blob_transition_state", BLOB_MODE_DIR / "blob-mode-state.py")
        qualifier = import_from_path("transition_evidence_qualifier", BLOB_MODE_DIR / "qualify-rustfs-transition.py")
        migration = qualifier_fixtures.good_migration_evidence(release_set)
        observed = live_schema_observation(release_set)
        dump_sha256 = "d" * 64
        capture = {
            "schema": "teagram.live-migration-schema/v1",
            "captured_at": "2026-10-08T01:04:00Z",
            "dump_sha256": dump_sha256,
            "query_sha256": qualifier.live_schema_query_sha256(release_set),
            "query_output_sha256": "e" * 64,
            "observed": observed,
        }
        document = {
            **migration,
            "live_capture": capture,
        }
        if release_set in {"60-67", "60-69", "60-70"}:
            document["baseline_live_capture"] = {
                **capture,
                "captured_at": "2026-10-08T01:03:00Z",
                "query_output_sha256": "f" * 64,
            }
        return mode, document, {"dump_sha256": dump_sha256}

    def validate(self, mode: Any, document: dict[str, Any], phase_digests: dict[str, str]) -> None:
        with patch.object(mode, "read_json", return_value=document):
            mode.validate_live_schema_dump_binding(
                {"schema_evidence_sha256": Path("migrations.json")}, phase_digests
            )

    def test_publisher_accepts_complete_live_atlas_revision_evidence(self) -> None:
        mode, document, phase_digests = self.schema_document()

        self.validate(mode, document, phase_digests)

    def test_publisher_accepts_complete_r67_live_atlas_revision_evidence(self) -> None:
        mode, document, phase_digests = self.schema_document("60-67")

        self.validate(mode, document, phase_digests)

    def test_publisher_accepts_complete_r69_and_r70_live_atlas_revision_evidence(self) -> None:
        for release_set in ("60-69", "60-70"):
            with self.subTest(release_set=release_set):
                mode, document, phase_digests = self.schema_document(release_set)
                self.validate(mode, document, phase_digests)

    def test_publisher_preserves_legacy_live_query_digest_for_pre_r70_reports(self) -> None:
        mode, document, phase_digests = self.schema_document("60-69")
        for capture_name in ("baseline_live_capture", "live_capture"):
            document[capture_name]["query_sha256"] = qualifier_fixtures.gate_constants(
                "60-69"
            )["legacy_live_schema_query_sha256"]

        self.validate(mode, document, phase_digests)

    def test_publisher_rejects_legacy_live_query_digest_for_r70_reports(self) -> None:
        mode, document, phase_digests = self.schema_document("60-70")
        for capture_name in ("baseline_live_capture", "live_capture"):
            document[capture_name]["query_sha256"] = "f0458b327ab900e9d5eaa4fc4e702149520163bafbdb58a2fd3e9ab53bc7b363"

        with self.assertRaises(mode.Reject):
            self.validate(mode, document, phase_digests)

    def test_publisher_rejects_nonempty_r70_live_revision_state_for_all_rows(self) -> None:
        for version in qualifier_fixtures.VERSIONS_60_70:
            for field in ("error_stmt_empty", "partial_hashes_empty"):
                with self.subTest(version=version, field=field):
                    mode, document, phase_digests = self.schema_document("60-70")
                    for capture_name in ("baseline_live_capture", "live_capture"):
                        document[capture_name]["observed"]["revision_detail"][version][field] = False
                    with self.assertRaises(mode.Reject):
                        self.validate(mode, document, phase_digests)

    def test_publisher_rejects_invalid_r70_baseline_capture(self) -> None:
        for field, value in (
            ("dump_sha256", "0" * 64),
            ("query_sha256", "0" * 64),
            ("observed", {}),
            ("captured_at", "2026-10-08T01:05:00Z"),
        ):
            with self.subTest(field=field):
                mode, document, phase_digests = self.schema_document("60-70")
                self.validate(mode, document, phase_digests)
                document["baseline_live_capture"][field] = value
                with self.assertRaises(mode.Reject):
                    self.validate(mode, document, phase_digests)

        mode, document, phase_digests = self.schema_document("60-70")
        self.validate(mode, document, phase_digests)
        del document["baseline_live_capture"]
        with self.assertRaises(mode.Reject):
            self.validate(mode, document, phase_digests)

    def test_publisher_rejects_incomplete_or_failed_live_atlas_revision(self) -> None:
        version = qualifier_fixtures.VERSIONS_60_66[-1]
        for field, value in (
            ("applied", 0),
            ("total", 2),
            ("error", "migration failed"),
            ("hash", "h1:stale"),
        ):
            with self.subTest(field=field):
                mode, document, phase_digests = self.schema_document()
                document["live_capture"]["observed"]["revision_detail"][version][field] = value

                with self.assertRaises(mode.Reject):
                    self.validate(mode, document, phase_digests)

if __name__ == "__main__":
    unittest.main()
