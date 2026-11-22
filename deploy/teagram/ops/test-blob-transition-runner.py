#!/usr/bin/env python3
from __future__ import annotations

import contextlib
import hashlib
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
    def test_test_environment_passes_source_volume_mountpoint_to_docker(self) -> None:
        runner = import_from_path("blob_transition_runner", RUNNER)
        with (
            patch.object(runner, "TEST_MODE", True),
            patch.object(runner, "TEST_FIXTURE_DOCKER", Path("/fixture/docker")),
            patch.dict(os.environ, {"MOCK_SOURCE_VOLUME_MOUNTPOINT": "/host/source-volume"}, clear=True),
        ):
            environment = runner.compose_environment(Path("/checkout"))

        self.assertEqual(environment["MOCK_SOURCE_VOLUME_MOUNTPOINT"], "/host/source-volume")

    def test_live_authority_rejection_keeps_its_reason(self) -> None:
        runner = import_from_path("blob_transition_runner_authority_rejection", RUNNER)
        mode = import_from_path("blob_mode_authority_rejection", BLOB_MODE_DIR / "blob-mode-state.py")
        container_id = "a" * 64
        inspected = {
            "Id": container_id,
            "Config": {
                "Labels": {
                    "com.docker.compose.project": "teagram",
                    "com.docker.compose.service": "teagramd",
                }
            },
            "State": {"Status": "running"},
        }
        with tempfile.TemporaryDirectory(prefix="live-authority-rejection.") as temporary:
            with (
                patch.object(
                    runner,
                    "run_command",
                    side_effect=[
                        f"{container_id}\tteagramd\n".encode("ascii"),
                        (json.dumps(inspected) + "\n").encode("ascii"),
                    ],
                ),
                patch.object(
                    mode,
                    "container_inventory",
                    side_effect=mode.Reject("running-backend-mismatch"),
                ),
            ):
                with self.assertRaisesRegex(runner.TransitionReject, "^running-backend-mismatch$"):
                    runner.assert_live_teagramd_matches_authority(
                        mode,
                        "teagram",
                        ["teagramd"],
                        {},
                        "/checkout/.state/blob-mode",
                        Path(temporary),
                        Path("/checkout"),
                        {},
                        "test-live-authority",
                    )

    def test_compose_authority_rejection_keeps_its_reason(self) -> None:
        runner = import_from_path("blob_transition_runner_compose_rejection", RUNNER)
        mode = import_from_path(
            "blob_mode_compose_rejection", BLOB_MODE_DIR / "blob-mode-state.py"
        )
        with patch.object(
            mode,
            "assert_compose_matches",
            side_effect=mode.Reject("render-teagramd-rustfs-network"),
        ):
            with self.assertRaisesRegex(
                runner.TransitionReject, "^render-teagramd-rustfs-network$"
            ):
                runner.assert_compose_matches_authority(
                    mode, {}, {}, "/checkout/.state/blob-mode", Path("/checkout/docker-compose.override.yml")
                )

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

    def test_rustfs_commands_load_parked_credentials_after_the_checkout_env(self) -> None:
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
            command = runner.compose_command(
                ["docker", "compose", "config"],
                {runner.BLOB_TRANSITION_COMPOSE_ENV_FILES: os.pathsep.join(env_files)},
            )

        self.assertEqual(env_files[0], str(checkout / ".env"))
        self.assertEqual(env_files[1], str(parked.resolve()))
        self.assertEqual(
            command,
            ["docker", "compose", "--env-file", env_files[0], "--env-file", env_files[1], "config"],
        )

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

    def test_r70_inert_capture_preserves_reference_coverage_rejection(self) -> None:
        runner = import_from_path("blob_transition_runner_inert_capture", RUNNER)
        with tempfile.TemporaryDirectory(prefix="r70-inert-capture.") as temporary:
            root = Path(temporary)
            bundle = root / "bundle"
            bundle.mkdir()
            (bundle / "migrations.json").write_text('{"release_set":"60-70"}\n', encoding="utf-8")
            output_dir = root / "output"
            output_dir.mkdir()
            observed = {
                name: name == "erasure_outbox"
                for name in qualifier_fixtures.gate_constants("60-70")["r70_inert_surfaces"]
            }

            def captured_query(*_args: Any, **_kwargs: Any) -> Path:
                output = output_dir / "inert-surfaces.json"
                output.write_text(json.dumps(observed), encoding="utf-8")
                return output

            with (
                patch.object(runner, "run_private_command", side_effect=captured_query),
                patch.object(runner, "replace_synced"),
            ):
                with self.assertRaises(runner.TransitionReject) as caught:
                    runner.capture_live_r70_inert_surfaces(
                        bundle, output_dir, root, {}, "fresh-recovery-r70-inert-surfaces"
                    )

        self.assertEqual(str(caught.exception), "reference_coverage")

    def test_r70_runner_rejects_nonempty_revision_state_in_baseline_and_applied_captures(self) -> None:
        if os.geteuid() != 0:
            self.skipTest("R70 live-schema fixture requires root-owned migration bundle files")
        runner = import_from_path("blob_transition_r70_revision_capture", RUNNER)
        good_observation = live_schema_observation("60-70")
        version = qualifier_fixtures.VERSIONS_60_70[-1]
        for stage, field in (
            ("baseline", "error_stmt_empty"),
            ("baseline", "partial_hashes_empty"),
            ("applied", "error_stmt_empty"),
            ("applied", "partial_hashes_empty"),
        ):
            with self.subTest(stage=stage, field=field), tempfile.TemporaryDirectory(
                prefix="r70-live-revision-capture."
            ) as temporary:
                root = Path(temporary)
                bundle, checkout, _, _ = qualifier_fixtures.write_bundle(
                    root / "fixture", release_set="60-70"
                )
                output_dir = root / "output"
                output_dir.mkdir(mode=0o700)
                query_output = root / "live-schema.json"

                query_output.write_text(
                    json.dumps(good_observation, sort_keys=True, separators=(",", ":")) + "\n",
                    encoding="utf-8",
                )
                query_output.chmod(0o600)
                with patch.object(runner, "run_private_command", return_value=query_output):
                    baseline = runner.capture_baseline_live_migration_schema(
                        bundle, output_dir, checkout, {}, "baseline-live-schema"
                    )

                bad_observation = json.loads(json.dumps(good_observation))
                bad_observation["revision_detail"][version][field] = False
                query_output.write_text(
                    json.dumps(bad_observation, sort_keys=True, separators=(",", ":")) + "\n",
                    encoding="utf-8",
                )
                query_output.chmod(0o600)
                with patch.object(runner, "run_private_command", return_value=query_output):
                    if stage == "baseline":
                        with self.assertRaises(runner.TransitionReject) as caught:
                            runner.capture_baseline_live_migration_schema(
                                bundle, output_dir, checkout, {}, "baseline-live-schema"
                            )
                        self.assertEqual(str(caught.exception), "baseline-live-schema-rejected")
                    else:
                        dump_sha256 = hashlib.sha256((bundle / "postgres.dump").read_bytes()).hexdigest()
                        with self.assertRaises(runner.TransitionReject) as caught:
                            runner.capture_live_migration_schema(
                                bundle,
                                output_dir,
                                checkout,
                                {},
                                dump_sha256,
                                "applied-live-schema",
                                baseline,
                            )
                        self.assertEqual(str(caught.exception), "live-schema-rejected")


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




class BlobTransitionAuthorityTests(unittest.TestCase):
    def test_compose_environment_forwards_s3_authority_inspector(self) -> None:
        runner = import_from_path("blob_transition_runner_compose_environment", RUNNER)
        inspector = "/fixtures/s3-authority-inspect.jsonl"
        with (
            patch.object(runner, "TEST_MODE", True),
            patch.object(runner, "TEST_FIXTURE_DOCKER", Path("/fixtures/docker")),
            patch.dict(os.environ, {"MOCK_S3_AUTHORITY_INSPECT": inspector}),
        ):
            environment = runner.compose_environment(Path("/checkout"))

        self.assertEqual(environment["MOCK_S3_AUTHORITY_INSPECT"], inspector)

    def test_live_teagramd_inventory_must_match_authority(self) -> None:
        runner = import_from_path("blob_transition_runner_live_authority", RUNNER)
        mode = mode_fixtures.blob_mode
        project = "teagram"
        checkout = Path("/checkout")
        mode_source = str(checkout / ".state" / "blob-mode")
        authority = {
            "outcome": "initial-local",
            "backend": {"kind": "local", "dir": mode.BLOB_TARGET},
            "volumes": {"tgblobs": "teagram_tgblobs", "rustfsdata": None},
        }
        identifier = "a" * 64

        def inspect(backend_dir: str, volume_name: str) -> dict[str, Any]:
            return {
                "Id": identifier,
                "State": {"Status": "running"},
                "Config": {
                    "Labels": {
                        "com.docker.compose.project": project,
                        "com.docker.compose.service": "teagramd",
                    },
                    "Env": [f"TG_BLOB_DIR={backend_dir}"],
                },
                "HostConfig": {"NetworkMode": "teagram_default", "PortBindings": {}},
                "Mounts": [
                    {
                        "Type": "volume", "Name": volume_name, "Source": "",
                        "Destination": mode.BLOB_TARGET, "RW": True,
                    },
                    {
                        "Type": "bind", "Name": "", "Source": mode_source,
                        "Destination": mode.MODE_TARGET, "RW": False,
                    },
                ],
                "NetworkSettings": {"Networks": {"teagram_default": {}}, "Ports": {}},
            }

        for case, backend_dir, volume_name, expected_error in (
            ("matching", mode.BLOB_TARGET, "teagram_tgblobs", None),
            ("backend drift", "/srv/unexpected-blobs", "teagram_tgblobs", "running-backend-mismatch"),
            ("mount drift", mode.BLOB_TARGET, "teagram_other_tgblobs", "running-volume-mismatch"),
        ):
            with self.subTest(case=case):
                inspected = inspect(backend_dir, volume_name)
                with (
                    patch.object(
                        runner,
                        "run_command",
                        side_effect=[
                            f"{identifier}\tteagramd\n".encode("ascii"),
                            (json.dumps(inspected) + "\n").encode("utf-8"),
                        ],
                    ),
                    patch.object(mode, "docker_volume_exists"),
                ):
                    if expected_error is None:
                        runner.assert_live_teagramd_matches_authority(
                            mode, project, ["teagramd"], authority, mode_source,
                            Path("/output"), checkout, {}, "test-live-authority",
                        )
                    else:
                        with self.assertRaisesRegex(runner.TransitionReject, expected_error):
                            runner.assert_live_teagramd_matches_authority(
                                mode, project, ["teagramd"], authority, mode_source,
                                Path("/output"), checkout, {}, "test-live-authority",
                            )

    def test_live_s3_inventory_includes_rustfs_volume(self) -> None:
        runner = import_from_path("blob_transition_runner_live_s3_authority", RUNNER)
        mode = mode_fixtures.blob_mode
        project = "teagram"
        checkout = Path("/checkout")
        mode_source = str(checkout / ".state" / "blob-mode")
        authority = {
            "outcome": "s3-accepted",
            "backend": {
                "kind": "s3", "endpoint": "http://rustfs:9000",
                "bucket": "teagram", "prefix": "teagramd/",
            },
            "volumes": {"tgblobs": "teagram_tgblobs", "rustfsdata": "teagram_rustfsdata"},
        }
        serving = {
            "Id": "a" * 64,
            "State": {"Status": "running"},
            "Config": {
                "Labels": {
                    "com.docker.compose.project": project,
                    "com.docker.compose.service": "teagramd",
                },
                "Env": [
                    "TG_BLOB_S3_ENDPOINT=http://rustfs:9000",
                    "TG_BLOB_S3_BUCKET=teagram",
                    "TG_BLOB_S3_PREFIX=teagramd/",
                    "TG_BLOB_S3_REGION=us-east-1",
                    "TG_BLOB_S3_ACCESS_KEY_ID=fixture-access",
                    "TG_BLOB_S3_SECRET_ACCESS_KEY_FILE=/run/secrets/teagramd-blob-secret-key",
                ],
            },
            "HostConfig": {"NetworkMode": "teagram_default", "PortBindings": {}},
            "Mounts": [
                {
                    "Type": "volume", "Name": "teagram_tgblobs", "Source": "",
                    "Destination": mode.BLOB_TARGET, "RW": False,
                },
                {
                    "Type": "bind", "Name": "", "Source": mode_source,
                    "Destination": mode.MODE_TARGET, "RW": False,
                },
            ],
            "NetworkSettings": {
                "Networks": {"teagram_default": {}, "teagram_rustfs_private": {}},
                "Ports": {},
            },
        }
        rustfs = {
            "Id": "b" * 64,
            "State": {"Status": "running"},
            "Config": {
                "Labels": {
                    "com.docker.compose.project": project,
                    "com.docker.compose.service": "rustfs",
                },
                "Env": [],
            },
            "Mounts": [{
                "Type": "volume", "Name": "teagram_rustfsdata", "Source": "",
                "Destination": "/data", "RW": True,
            }],
            "HostConfig": {"NetworkMode": "teagram_rustfs_private", "PortBindings": {}},
            "NetworkSettings": {"Networks": {"teagram_rustfs_private": {}}, "Ports": {}},
        }
        inspected = (json.dumps(serving) + "\n" + json.dumps(rustfs) + "\n").encode("utf-8")
        with (
            patch.object(
                runner,
                "run_command",
                side_effect=[
                    b"a" * 64 + b"\tteagramd\n" + b"b" * 64 + b"\trustfs\n",
                    inspected,
                    json.dumps({
                        "Name": "teagram_rustfs_private",
                        "Driver": "bridge",
                        "Internal": True,
                        "EnableIPv6": False,
                        "Options": {"com.docker.network.bridge.gateway_mode_ipv4": "isolated"},
                    }).encode("utf-8"),
                ],
            ),
            patch.object(mode, "docker_volume_exists"),
        ):
            runner.assert_live_teagramd_matches_authority(
                mode, project, ["teagramd"], authority, mode_source,
                Path("/output"), checkout, {}, "test-live-s3-authority",
            )




if __name__ == "__main__":
    unittest.main()
