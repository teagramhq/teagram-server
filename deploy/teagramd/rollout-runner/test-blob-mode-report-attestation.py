#!/usr/bin/env python3
"""Fixtures for the one-shot blob-mode report-loss attestation."""

from __future__ import annotations

import contextlib
import importlib.util
import json
import os
import pathlib
import shlex
import stat
import subprocess
import sys
import tempfile
import textwrap
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
REFERENCE_ROWS = b"file\t1\t\t5\n"


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


def git_blob_oid(content: bytes) -> str:
    result = subprocess.run(
        ["git", "hash-object", "--stdin"],
        input=content,
        stdout=subprocess.PIPE,
        check=True,
    )
    return result.stdout.decode("ascii").strip()


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

    def run_encrypt_media_preflight(
        self,
        root: pathlib.Path,
        checkout: pathlib.Path,
        state_dir: pathlib.Path,
        report_root: pathlib.Path,
        lock_path: pathlib.Path,
    ) -> None:
        preflight_root = root / "encrypt-media-preflight"
        preflight_root.mkdir(mode=0o700)
        runtime_dir = preflight_root / "runtime"
        runtime_dir.mkdir(mode=0o700)
        bin_dir = preflight_root / "bin"
        bin_dir.mkdir(mode=0o700)
        key_dir = preflight_root / "media-keys"
        key_dir.mkdir(mode=0o700)
        evidence_root = preflight_root / "evidence"
        evidence_root.mkdir(mode=0o700)

        actual_common = SCRIPT_DIR.parents[2] / "deploy" / "teagram" / "ops" / "blob-encryption-common.inc"
        common_source = actual_common.read_text(encoding="utf-8")
        replacements = {
            "readonly REPO=/opt/teagram": f"readonly REPO={shlex.quote(str(checkout))}",
            "readonly LOCK_FILE=/tmp/teagram-deploy.lock": f"readonly LOCK_FILE={shlex.quote(str(lock_path))}",
            "readonly EXPECTED_ROOT_UID=0": f"readonly EXPECTED_ROOT_UID={os.geteuid()}",
            "readonly KEY_DIR=/etc/teagramd/media-keys": f"readonly KEY_DIR={shlex.quote(str(key_dir))}",
            "readonly EVIDENCE_ROOT=/root/teagram-media-encryption": f"readonly EVIDENCE_ROOT={shlex.quote(str(evidence_root))}",
            "/opt/teagram/.state/blob-mode": str(state_dir),
        }
        for old, new in replacements.items():
            self.assertIn(old, common_source)
            common_source = common_source.replace(old, new)
        common_path = preflight_root / "blob-encryption-common.inc"
        common_path.write_text(common_source, encoding="utf-8")
        common_path.chmod(0o600)
        (checkout / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
        (checkout / "docker-compose.override.yml").write_text("services: {}\n", encoding="utf-8")

        migration_mounts = [
            {"type": "bind", "source": str(key_dir), "target": "/run/teagramd/media-keys", "read_only": True},
            {"type": "bind", "source": str(report_root), "target": "/run/teagramd/reports"},
            {"type": "bind", "source": str(state_dir), "target": "/run/teagramd/blob-mode"},
        ]
        compose = {
            "services": {
                "blob-encrypt-migrate": {
                    "image": "teagramd:local",
                    "entrypoint": ["/usr/local/bin/blob-encrypt-migrate"],
                    "user": "0:0",
                    "read_only": True,
                    "profiles": ["migration"],
                    "volumes": migration_mounts,
                    "networks": {"default": {}, "rustfs_private": {}},
                },
                "teagramd": {
                    "image": "teagramd:local",
                    "environment": {
                        "TG_MEDIA_KEY_CURRENT": "1",
                        "TG_MEDIA_KEY_DIR": "/run/teagramd/media-keys",
                    },
                    "volumes": [migration_mounts[0]],
                },
            },
        }
        compose_path = preflight_root / "compose.json"
        compose_path.write_text(json.dumps(compose), encoding="utf-8")
        (runtime_dir / "blob-transition-runner.py").write_bytes(
            (SCRIPT_DIR / "blob-transition-runner.py").read_bytes()
        )
        (runtime_dir / "blob-transition-runner.py").chmod(0o600)
        (runtime_dir / "blob-mode-state.py").write_bytes(
            (SCRIPT_DIR / "blob-mode-state.py").read_bytes()
        )
        (runtime_dir / "blob-mode-state.py").chmod(0o600)

        docker_stub = bin_dir / "docker"
        docker_stub.write_text(
            "#!/bin/sh\nset -eu\n[ \"$1\" = compose ] || exit 64\ncat -- \"$BLOB_TEST_COMPOSE_JSON\"\n",
            encoding="utf-8",
        )
        docker_stub.chmod(0o755)
        python_stub = bin_dir / "python3"
        python_stub.write_text(
            textwrap.dedent(
                f"""\
                #!{sys.executable}
                import importlib.util
                import os
                import pathlib
                import stat
                import sys

                runner_path = pathlib.Path(sys.argv[1])
                mode_path = pathlib.Path(os.environ["BLOB_TEST_MODE_HELPER"])

                def load(name, path):
                    spec = importlib.util.spec_from_file_location(name, path)
                    module = importlib.util.module_from_spec(spec)
                    sys.modules[name] = module
                    spec.loader.exec_module(module)
                    return module

                mode = load("encrypt_preflight_fixture_mode", mode_path)
                runner = load("encrypt_preflight_fixture_runner", runner_path)

                def fixture_file_stat(path, kind, expected_mode=None):
                    info = pathlib.Path(path).lstat()
                    if stat.S_ISLNK(info.st_mode) or stat.S_IFMT(info.st_mode) != kind:
                        raise mode.Reject("fixture-state-type")
                    if info.st_mode & 0o022:
                        raise mode.Reject("fixture-state-permissions")
                    if expected_mode is not None and stat.S_IMODE(info.st_mode) != expected_mode:
                        raise mode.Reject("fixture-state-permissions")
                    return info

                mode.file_stat = fixture_file_stat
                mode.require_runner_lock = lambda _path: None
                runner.import_mode_helper = lambda: mode
                runner.rustfs_compose_environment_files = lambda checkout: [
                    str(checkout / ".env"), str(checkout / ".env.rustfs-parked")
                ]
                runner.TEST_MODE = True
                sys.argv = [str(runner_path), *sys.argv[2:]]
                raise SystemExit(runner.main())
                """
            ),
            encoding="utf-8",
        )
        python_stub.chmod(0o755)

        environment = os.environ.copy()
        environment.update(
            {
                "BLOB_TEST_COMMON": str(common_path),
                "BLOB_TEST_COMPOSE_JSON": str(compose_path),
                "BLOB_TEST_MODE_HELPER": str(SCRIPT_DIR / "blob-mode-state.py"),
                "BLOB_TRANSITION_TEST_MODE": "1",
                "OPS_TEMP_DIR": str(runtime_dir),
                "PATH": f"{bin_dir}:{environment.get('PATH', '')}",
                "PYTHONDONTWRITEBYTECODE": "1",
                "RUN_ID": "attested-fixture",
                "TEAGRAM_BLOB_MODE_REPORT_ROOT": str(report_root),
            }
        )
        result = subprocess.run(
            [
                "bash",
                "-c",
                'set -Eeuo pipefail\nsource "$BLOB_TEST_COMMON"\nconfigure_compose\nprintf "authority_preflight=pass\\n"',
            ],
            cwd=checkout,
            env=environment,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "authority_preflight=pass")

    def test_live_census_accepts_legacy_flat_part_key(self) -> None:
        key = "parts/deadbeef000000000000000000000000"
        s3_manifest = f"{key}\t7\t{'b' * 64}\n".encode("ascii")
        reference_rows = f"part\t0\t{key}\t7\n".encode("ascii")

        census, object_count, byte_total = self.operation.validate_live_census(s3_manifest, reference_rows)

        document = json.loads(census)
        self.assertEqual(object_count, 1)
        self.assertEqual(byte_total, 7)
        self.assertEqual(document["references"], [{"key": key, "logical_bytes": 7}])

    def test_live_census_accepts_sharded_part_key_with_independent_shard(self) -> None:
        part_suffix = "0123456789abcdef" + "0123456789abcdef"[:14]
        key = f"parts/ab/{part_suffix}"
        s3_manifest = f"{key}\t7\t{'b' * 64}\n".encode("ascii")
        reference_rows = f"part\t0\t{key}\t7\n".encode("ascii")

        census, object_count, byte_total = self.operation.validate_live_census(
            s3_manifest, reference_rows
        )

        document = json.loads(census)
        self.assertEqual(object_count, 1)
        self.assertEqual(byte_total, 7)
        self.assertEqual(document["references"], [{"key": key, "logical_bytes": 7}])

        short_key = f"parts/ab/{part_suffix[:-1]}"
        with self.assertRaises(self.operation.AttestationReject) as caught:
            self.operation.validate_live_census(
                f"{short_key}\t7\t{'b' * 64}\n".encode("ascii"),
                f"{short_key}\t7\n".encode("ascii"),
            )
        self.assertEqual(str(caught.exception), "census-manifest-invalid")

    def test_malformed_live_census_rows_refuse_before_creating_archive(self) -> None:
        valid_hash = "a" * 64
        malformed = [
            (
                "unsorted S3 rows",
                f"02/2\t5\t{valid_hash}\n01/1\t5\t{valid_hash}\n".encode("ascii"),
                b"",
                "census-manifest-invalid",
            ),
            (
                "duplicate S3 rows",
                f"01/1\t5\t{valid_hash}\n01/1\t5\t{valid_hash}\n".encode("ascii"),
                b"",
                "census-manifest-invalid",
            ),
            ("malformed S3 row", b"01/1\t5\n", b"", "census-manifest-invalid"),
            (
                "duplicate reference rows",
                S3_MANIFEST,
                b"file\t1\t\t5\nfile\t1\t\t5\n",
                "census-references-invalid",
            ),
            (
                "malformed reference row",
                S3_MANIFEST,
                b"file\t1\t\t5\textra\n",
                "census-references-invalid",
            ),
        ]
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path, _records = self.make_authority(root)
            before_state = snapshot_files(state_dir)
            before_reports = snapshot_files(report_root)

            for name, s3_manifest, reference_rows, expected_reason in malformed:
                with self.subTest(name=name):
                    patches = self.operation_context(
                        root,
                        state_dir,
                        report_root,
                        lock_path,
                        census=(s3_manifest, reference_rows),
                    )
                    with contextlib.ExitStack() as stack:
                        for item in patches:
                            stack.enter_context(item)
                        with self.assertRaises(self.operation.AttestationReject) as caught:
                            self.operation.run_attestation()
                    self.assertEqual(str(caught.exception), expected_reason)
                    self.assertEqual(snapshot_files(state_dir), before_state)
                    self.assertEqual(snapshot_files(report_root), before_reports)
                    self.assertFalse((root / "archives").exists())

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

                self.run_encrypt_media_preflight(root, checkout, state_dir, report_root, lock_path)

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

    def test_committed_publication_survives_later_authority_failure(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path, _records = self.make_authority(root)
            patches = self.operation_context(root, state_dir, report_root, lock_path)
            with contextlib.ExitStack() as stack:
                for item in patches:
                    stack.enter_context(item)
                with patch.object(
                    self.operation,
                    "_remove_recovery_marker",
                    side_effect=self.operation.AttestationReject("simulated-process-interruption"),
                ):
                    with self.assertRaises(self.operation.AttestationReject):
                        self.operation.run_attestation()

                marker = json.loads(self.operation.recovery_marker_path().read_text(encoding="ascii"))
                self.assertEqual(marker["phase"], "committed")
                archive_path = next((root / "archives").glob("attestation-*"))
                archive_before = snapshot_files(archive_path)
                published_names = [
                    self.operation.ARCHIVE_MANIFEST_NAME,
                    self.operation.CENSUS_MANIFEST_NAME,
                    *[
                        path.name
                        for path in report_root.glob(f"{self.operation.ATTESTATION_PREFIX}*.json")
                    ],
                ]
                published_before = {name: (report_root / name).read_bytes() for name in published_names}

                with (
                    patch.object(mode_fixtures.blob_mode, "require_runner_lock"),
                    patch.object(
                        mode_fixtures.blob_mode,
                        "file_stat",
                        side_effect=self.fixture.allow_unowned_private_files,
                    ),
                    patch.object(mode_fixtures.blob_mode.os, "fchown"),
                    patch.object(mode_fixtures.blob_mode.os, "chown"),
                ):
                    self.fixture.publish(state_dir, report_root, lock_path, root, "recovered-local")
                    records, _head, _mode_bytes = mode_fixtures.blob_mode.read_authority(
                        state_dir, report_root
                    )
                    mode_fixtures.blob_mode.report_read_path(
                        report_root, records[-1]["transition_id"]
                    ).unlink()

                with self.assertRaises(self.operation.AttestationReject):
                    self.operation.run_attestation()

                self.assertEqual(snapshot_files(archive_path), archive_before)
                self.assertEqual(
                    {name: (report_root / name).read_bytes() for name in published_names},
                    published_before,
                )
                self.assertFalse(self.operation.recovery_marker_path().exists())

    def test_published_attestation_survives_failure_before_marker_commit(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path, _records = self.make_authority(root)
            patches = self.operation_context(root, state_dir, report_root, lock_path)
            with contextlib.ExitStack() as stack:
                for item in patches:
                    stack.enter_context(item)
                with (
                    patch.object(
                        self.operation,
                        "_mark_recovery_marker_committed",
                        side_effect=self.operation.AttestationReject("simulated-process-interruption"),
                    ),
                    patch.object(
                        self.operation,
                        "_rollback_incomplete_publication",
                        side_effect=self.operation.AttestationReject("simulated-process-interruption"),
                    ),
                ):
                    with self.assertRaises(self.operation.AttestationReject):
                        self.operation.run_attestation()

                marker = json.loads(self.operation.recovery_marker_path().read_text(encoding="ascii"))
                self.assertEqual(marker["phase"], "publishing")
                archive_path = next((root / "archives").glob("attestation-*"))
                archive_before = snapshot_files(archive_path)
                published_names = [
                    self.operation.ARCHIVE_MANIFEST_NAME,
                    self.operation.CENSUS_MANIFEST_NAME,
                    *[
                        path.name
                        for path in report_root.glob(f"{self.operation.ATTESTATION_PREFIX}*.json")
                    ],
                ]
                published_before = {name: (report_root / name).read_bytes() for name in published_names}

                with (
                    patch.object(mode_fixtures.blob_mode, "require_runner_lock"),
                    patch.object(
                        mode_fixtures.blob_mode,
                        "file_stat",
                        side_effect=self.fixture.allow_unowned_private_files,
                    ),
                    patch.object(mode_fixtures.blob_mode.os, "fchown"),
                    patch.object(mode_fixtures.blob_mode.os, "chown"),
                ):
                    self.fixture.publish(state_dir, report_root, lock_path, root, "recovered-local")
                    records, _head, _mode_bytes = mode_fixtures.blob_mode.read_authority(
                        state_dir, report_root
                    )
                    mode_fixtures.blob_mode.report_read_path(
                        report_root, records[-1]["transition_id"]
                    ).unlink()

                with self.assertRaises(self.operation.AttestationReject):
                    self.operation.run_attestation()

                self.assertEqual(snapshot_files(archive_path), archive_before)
                self.assertEqual(
                    {name: (report_root / name).read_bytes() for name in published_names},
                    published_before,
                )
                self.assertFalse(self.operation.recovery_marker_path().exists())

    def test_damaged_manifest_does_not_delete_possible_published_evidence(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path, _records = self.make_authority(root)
            patches = self.operation_context(root, state_dir, report_root, lock_path)
            with contextlib.ExitStack() as stack:
                for item in patches:
                    stack.enter_context(item)
                with (
                    patch.object(
                        self.operation,
                        "_mark_recovery_marker_committed",
                        side_effect=self.operation.AttestationReject("simulated-process-interruption"),
                    ),
                    patch.object(
                        self.operation,
                        "_rollback_incomplete_publication",
                        side_effect=self.operation.AttestationReject("simulated-process-interruption"),
                    ),
                ):
                    with self.assertRaises(self.operation.AttestationReject):
                        self.operation.run_attestation()

                archive_path = next((root / "archives").glob("attestation-*"))
                archive_before = snapshot_files(archive_path)
                published_names = [
                    self.operation.ARCHIVE_MANIFEST_NAME,
                    self.operation.CENSUS_MANIFEST_NAME,
                    *[
                        path.name
                        for path in report_root.glob(f"{self.operation.ATTESTATION_PREFIX}*.json")
                    ],
                ]

                with (
                    patch.object(mode_fixtures.blob_mode, "require_runner_lock"),
                    patch.object(
                        mode_fixtures.blob_mode,
                        "file_stat",
                        side_effect=self.fixture.allow_unowned_private_files,
                    ),
                    patch.object(mode_fixtures.blob_mode.os, "fchown"),
                    patch.object(mode_fixtures.blob_mode.os, "chown"),
                ):
                    self.fixture.publish(state_dir, report_root, lock_path, root, "recovered-local")
                    records, _head, _mode_bytes = mode_fixtures.blob_mode.read_authority(
                        state_dir, report_root
                    )
                    mode_fixtures.blob_mode.report_read_path(
                        report_root, records[-1]["transition_id"]
                    ).unlink()

                census_path = report_root / self.operation.CENSUS_MANIFEST_NAME
                census_path.write_bytes(b"damaged\n")
                published_before = {
                    name: (report_root / name).read_bytes() for name in published_names
                }

                with self.assertRaises(self.operation.AttestationReject) as raised:
                    self.operation.run_attestation()

                self.assertIn("report-attestation-evidence", str(raised.exception))
                self.assertEqual(snapshot_files(archive_path), archive_before)
                self.assertEqual(
                    {name: (report_root / name).read_bytes() for name in published_names},
                    published_before,
                )
                self.assertTrue(self.operation.recovery_marker_path().exists())

    def test_live_census_uses_compose_config_s3_census_and_database_select_only(self) -> None:
        calls: list[list[str]] = []
        reasons: list[str] = []
        environment = {"TG_BLOB_S3_SECRET_ACCESS_KEY": "never-print-this"}
        checkout = SCRIPT_DIR.parents[2]
        sql_source = (checkout / "internal/store/queries/uploads.sql").read_text(encoding="utf-8")
        query_start = sql_source.index("SELECT 'file'::text AS kind")
        query_end = sql_source.index(";", query_start) + 1
        canonical_query = sql_source[query_start:query_end]
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
                "--single-transaction", "-c", f"SET TRANSACTION READ ONLY;\n{canonical_query}",
            ],
        ]
        def run_compose(command, _env, *, reason):
            calls.append(command)
            reasons.append(reason)
            if "config" in command:
                return json.dumps(rendered).encode()
            if "s3-census" in command:
                return S3_MANIFEST
            return REFERENCE_ROWS

        with tempfile.TemporaryDirectory() as temporary:
            staged_dir = pathlib.Path(temporary)
            staged_query = staged_dir / "uploads.sql"
            staged_blob_oid = staged_dir / "uploads.sql.git-blob"
            sql_bytes = (checkout / "internal/store/queries/uploads.sql").read_bytes()
            staged_query.write_bytes(sql_bytes)
            staged_blob_oid.write_text(git_blob_oid(sql_bytes) + "\n", encoding="ascii")
            with (
                patch.object(
                    self.operation.transition,
                    "rustfs_compose_environment_files",
                    return_value=[
                        str(checkout / ".env"),
                        str(checkout / ".env.rustfs-parked"),
                    ],
                ),
                patch.object(
                    self.operation.transition,
                    "compose_environment_for_files",
                    return_value=environment,
                ) as compose_environment,
                patch.object(self.operation, "CHECKOUT", checkout),
                patch.object(self.operation, "REFERENCE_QUERY_SOURCE", staged_query),
                patch.object(self.operation, "REFERENCE_QUERY_BLOB_OID", staged_blob_oid),
                patch.object(self.operation.mode, "compose_inventory", return_value=object()) as compose_inventory,
                patch.object(self.operation.mode, "assert_compose_matches") as assert_compose_matches,
                patch.object(self.operation.mode, "backend_from_values", return_value=record["backend"]),
                patch.object(self.operation, "run_compose", side_effect=run_compose),
            ):
                s3_manifest, reference_rows = self.operation.capture_live_census(record)

        self.assertEqual(calls, expected)
        self.assertEqual(
            reasons,
            ["live-compose-config", "live-s3-census", "live-reference-query"],
        )
        self.assertEqual((s3_manifest, reference_rows), (S3_MANIFEST, REFERENCE_ROWS))
        compose_environment.assert_called_once_with(
            checkout,
            ["docker-compose.yml", "docker-compose.override.yml"],
            include_rustfs_credentials=True,
        )
        compose_inventory.assert_called_once_with(rendered, checkout)
        assert_compose_matches.assert_called_once()
        self.assertFalse(any(command in ("restart", "up", "stop") for call in calls for command in call))
        self.assertNotIn("never-print-this", repr(calls))

    def test_reference_query_refuses_staged_bytes_that_do_not_match_reviewed_git_blob(self) -> None:
        checkout = SCRIPT_DIR.parents[2]
        sql_bytes = (checkout / "internal/store/queries/uploads.sql").read_bytes()
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            source = root / "uploads.sql"
            blob_oid_path = root / "uploads.sql.git-blob"
            source.write_bytes(sql_bytes)
            blob_oid_path.write_text(git_blob_oid(sql_bytes) + "\n", encoding="ascii")
            with (
                patch.object(self.operation, "REFERENCE_QUERY_SOURCE", source),
                patch.object(self.operation, "REFERENCE_QUERY_BLOB_OID", blob_oid_path),
            ):
                query = self.operation.blob_inventory_references_query()
                self.assertIn("FROM upload_parts up", query)

                source.write_bytes(sql_bytes.replace(b"up.size AS logical_bytes", b"0 AS logical_bytes"))
                with self.assertRaises(self.operation.AttestationReject) as caught:
                    self.operation.blob_inventory_references_query()

        self.assertEqual(str(caught.exception), "live-reference-query-source-invalid")

    def test_live_census_command_failure_does_not_echo_secret_output(self) -> None:
        secret = "fixture-secret-must-not-be-printed"
        for reason in ("live-compose-config", "live-s3-census", "live-reference-query"):
            with self.subTest(reason=reason):
                with patch.object(
                    self.operation.subprocess,
                    "run",
                    return_value=SimpleNamespace(
                        returncode=1, stdout=secret.encode(), stderr=secret.encode()
                    ),
                ) as run:
                    with self.assertRaises(self.operation.AttestationReject) as caught:
                        self.operation.run_compose(
                            ["docker", "compose", "config"],
                            {"TG_SECRET": secret},
                            reason=reason,
                        )

                self.assertEqual(str(caught.exception), f"{reason}-rejected")
                self.assertNotIn(secret, str(caught.exception))
                self.assertTrue(run.call_args.kwargs["stdout"] == self.operation.subprocess.PIPE)
                self.assertTrue(run.call_args.kwargs["stderr"] == self.operation.subprocess.PIPE)

    def test_census_programming_errors_are_not_reported_as_live_system_rejections(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path, _records = self.make_authority(root)
            before_state = snapshot_files(state_dir)
            before_reports = snapshot_files(report_root)
            patches = self.operation_context(root, state_dir, report_root, lock_path)
            with contextlib.ExitStack() as stack:
                for item in patches:
                    stack.enter_context(item)
                with patch.object(
                    self.operation,
                    "capture_live_census",
                    side_effect=RuntimeError("programming error"),
                ):
                    with self.assertRaisesRegex(RuntimeError, "programming error"):
                        self.operation.run_attestation()

            self.assertEqual(snapshot_files(state_dir), before_state)
            self.assertEqual(snapshot_files(report_root), before_reports)
            self.assertFalse((root / "archives").exists())

    def test_lock_refusal_writes_nothing(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path, _records = self.make_authority(root)
            before_state = snapshot_files(state_dir)
            before_reports = snapshot_files(report_root)
            patches = self.operation_context(root, state_dir, report_root, lock_path)
            with contextlib.ExitStack() as stack:
                for item in patches:
                    stack.enter_context(item)
                with patch.object(
                    self.operation.transition,
                    "acquire_lock",
                    side_effect=self.operation.transition.TransitionReject("deployment-lock-held"),
                ):
                    with self.assertRaises(self.operation.AttestationReject) as caught:
                        self.operation.run_attestation()

            self.assertEqual(str(caught.exception), "deployment-lock-held")
            self.assertEqual(snapshot_files(state_dir), before_state)
            self.assertEqual(snapshot_files(report_root), before_reports)
            self.assertFalse((root / "archives").exists())

    def test_write_private_file_names_file_when_cleanup_unlink_fails(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = pathlib.Path(temporary) / "attestation-manifest.tmp"
            root_uid = os.geteuid()
            original_unlink = pathlib.Path.unlink

            def fail_target_unlink(target, *args, **kwargs):
                if target == path:
                    raise PermissionError("injected unlink failure")
                return original_unlink(target, *args, **kwargs)

            with (
                patch.object(self.operation, "ROOT_UID", root_uid),
                patch.object(
                    self.operation,
                    "sync_directory",
                    side_effect=self.operation.AttestationReject("attestation-output-sync"),
                ),
                patch.object(pathlib.Path, "unlink", new=fail_target_unlink),
            ):
                with self.assertRaises(self.operation.AttestationReject) as caught:
                    self.operation.write_private_file(path, b"payload\n")

            self.assertEqual(
                str(caught.exception),
                f"attestation-output-cleanup-failed:{path.name}",
            )
            self.assertTrue(path.exists())

    def test_journal_head_read_failure_has_journal_reason(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path, _records = self.make_authority(root)
            patches = self.operation_context(root, state_dir, report_root, lock_path)
            original_iterdir = pathlib.Path.iterdir

            def fail_journal_iteration(path):
                if path == state_dir / "journal":
                    raise OSError("injected journal read failure")
                return original_iterdir(path)

            with contextlib.ExitStack() as stack:
                for item in patches:
                    stack.enter_context(item)
                with patch.object(pathlib.Path, "iterdir", new=fail_journal_iteration):
                    with self.assertRaises(self.operation.AttestationReject) as caught:
                        self.operation._journal_head_matches("a" * 64)

            self.assertEqual(str(caught.exception), "journal-unavailable")

    def test_archive_copy_failure_names_relative_entry(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            source = root / "source"
            destination = root / "destination"
            source.mkdir(mode=0o700)
            destination.mkdir(mode=0o700)
            source_file = source / "report.json"
            source_file.write_bytes(b"report\n")
            source_file.chmod(0o600)
            source_fd = os.open(source, os.O_RDONLY | os.O_DIRECTORY)
            destination_fd = os.open(destination, os.O_RDONLY | os.O_DIRECTORY)
            try:
                with (
                    patch.object(self.operation, "ROOT_UID", os.geteuid()),
                    patch.object(self.operation.os, "write", side_effect=OSError("injected write failure")),
                ):
                    with self.assertRaises(self.operation.AttestationReject) as caught:
                        self.operation.copy_tree_contents(
                            source_fd, destination_fd, "reports", []
                        )
            finally:
                os.close(source_fd)
                os.close(destination_fd)

            self.assertEqual(
                str(caught.exception),
                "archive-copy-failed:reports/report.json",
            )

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
            (
                b"file\t2\t\t5\n",
                b"01/1\t5\t" + b"a" * 64 + b"\n",
                "census-reference-missing",
            ),
            (
                b"file\t1\t\t6\n",
                b"01/1\t5\t" + b"a" * 64 + b"\n",
                "census-reference-size-mismatch",
            ),
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
