#!/usr/bin/env python3
"""Credential-free unit checks for rollout blob-mode inventory gates."""

from __future__ import annotations

import importlib.util
import json
import os
import pathlib
import stat
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch


HELPER = pathlib.Path(__file__).with_name("blob-mode-state.py")
SPEC = importlib.util.spec_from_file_location("blob_mode_state", HELPER)
assert SPEC is not None and SPEC.loader is not None
blob_mode = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(blob_mode)
QUALIFIER_PATH = pathlib.Path(__file__).with_name("qualify-rustfs-transition.py")
QUALIFIER_SPEC = importlib.util.spec_from_file_location("transition_qualifier_fixture", QUALIFIER_PATH)
assert QUALIFIER_SPEC is not None and QUALIFIER_SPEC.loader is not None
qualifier = importlib.util.module_from_spec(QUALIFIER_SPEC)
QUALIFIER_SPEC.loader.exec_module(qualifier)
QUALIFIER_FIXTURE_PATH = pathlib.Path(__file__).with_name("test-qualify-rustfs-transition.py")
QUALIFIER_FIXTURE_SPEC = importlib.util.spec_from_file_location("transition_qualifier_fixtures", QUALIFIER_FIXTURE_PATH)
assert QUALIFIER_FIXTURE_SPEC is not None and QUALIFIER_FIXTURE_SPEC.loader is not None
qualifier_fixtures = importlib.util.module_from_spec(QUALIFIER_FIXTURE_SPEC)
QUALIFIER_FIXTURE_SPEC.loader.exec_module(qualifier_fixtures)


class BlobModeStateTests(unittest.TestCase):
    def setUp(self) -> None:
        self.checkout = pathlib.Path("/srv/telegram-server")
        self.mode_source = str(self.checkout / ".state" / "blob-mode")
        self.volume = "telegram-server_tgblobs"
        self.record = {
            "schema": blob_mode.SCHEMA,
            "generation": 1,
            "transition_id": "00000000-0000-4000-8000-000000000001",
            "supersedes": None,
            "outcome": "initial-local",
            "backend": {"kind": "local", "dir": blob_mode.BLOB_TARGET},
            "volumes": {"tgblobs": self.volume, "rustfsdata": None},
            "evidence": {"report_sha256": "a" * 64},
            "published_at": "2026-10-08T00:00:00Z",
        }
        self.override = pathlib.Path("/dev/null")

    def local_compose(self, *, proxy_guard: bool = True) -> dict[str, object]:
        def service(name: str, guarded: bool = True) -> dict[str, object]:
            volumes: list[dict[str, object]] = [
                {"type": "volume", "source": "tgblobs", "target": blob_mode.BLOB_TARGET, "read_only": False}
            ]
            if guarded:
                volumes.append({
                    "type": "bind",
                    "source": self.mode_source,
                    "target": blob_mode.MODE_TARGET,
                    "read_only": True,
                })
            return {
                "name": name,
                "backend": {"kind": "local", "dir": blob_mode.BLOB_TARGET},
                "blob_mode_mounts": [
                    {"type": "bind", "source": self.mode_source, "target": blob_mode.MODE_TARGET, "read_only": True}
                ] if guarded else [],
                "tgblobs_mounts": [
                    {"type": "volume", "source": self.volume, "target": blob_mode.BLOB_TARGET, "read_only": False}
                ],
            }

        return {
            "services": [service("telegramd"), service("telegramd-proxy", proxy_guard)],
            "volumes": {"tgblobs": self.volume, "rustfsdata": None},
            "mode_source": self.mode_source,
        }

    def local_containers(self, *, guarded: bool = True) -> dict[str, object]:
        return {
            "containers": [{
                "id": "a" * 64,
                "service": "telegramd",
                "backend": {"kind": "local", "dir": blob_mode.BLOB_TARGET},
                "mode_mounts": [{
                    "type": "bind", "source": self.mode_source,
                    "target": blob_mode.MODE_TARGET, "read_only": True,
                }] if guarded else [],
                "tgblobs_mounts": [{
                    "type": "volume", "name": self.volume,
                    "target": blob_mode.BLOB_TARGET, "rw": True,
                }],
            }],
            "mode_source": self.mode_source,
        }

    def assert_rejects(self, callback, reason: str) -> None:
        with self.assertRaises(blob_mode.Reject) as caught:
            callback()
        self.assertEqual(str(caught.exception), reason)

    def transition_proof(self, outcome: str) -> dict[str, object]:
        phases = blob_mode.S3_TRANSITION_PHASES if outcome == "s3-accepted" else blob_mode.RECOVERY_TRANSITION_PHASES
        phase_digests = {name: "b" * 64 for name in phases}
        if outcome == "s3-accepted":
            for name in (
                "source_provisional_sha256", "source_frozen_sha256",
                "destination_census_pass_1_sha256", "destination_census_pass_2_sha256",
            ):
                phase_digests[name] = "c" * 64
            evidence: dict[str, object] = {
                "source_manifest_sha256": "c" * 64,
                "destination_manifest_sha256": "c" * 64,
                "object_count": 2,
                "byte_total": 20,
                "copy_passes": 2,
            }
            backend = {"kind": "s3", "endpoint": "http://rustfs:9000", "bucket": "telegram", "prefix": "telegramd/"}
        else:
            phase_digests["s3_census_pass_1_sha256"] = "d" * 64
            phase_digests["s3_census_pass_2_sha256"] = "d" * 64
            phase_digests["local_census_pass_1_sha256"] = "e" * 64
            phase_digests["local_census_pass_2_sha256"] = "e" * 64
            evidence = {
                "s3_census_manifest_sha256": "d" * 64,
                "restored_manifest_sha256": "e" * 64,
                "object_count": 3,
                "byte_total": 30,
                "restore_passes": 2,
                "retained_cutover_key_count": 1,
            }
            backend = {"kind": "local", "dir": blob_mode.BLOB_TARGET}
        return {
            "schema": blob_mode.TRANSITION_PROOF_SCHEMA,
            "outcome": outcome,
            "backend": backend,
            "volumes": {"tgblobs": self.volume, "rustfsdata": "telegram-server_rustfsdata"},
            "evidence": evidence,
            "phase_digests": phase_digests,
        }

    def materialize_transition_proof(self, proof: dict[str, object], report_root: pathlib.Path) -> None:
        outcome = proof["outcome"]
        phase_digests = proof["phase_digests"]
        assert isinstance(outcome, str) and isinstance(phase_digests, dict)
        phase_dir = report_root / f"transition-proof-fixtures-{outcome}-{len(list(report_root.iterdir()))}"
        phase_dir.mkdir(mode=0o700)
        phase_files: dict[str, str] = {}
        source_manifest = b"01/1\t10\t" + b"a" * 64 + b"\n02/2\t10\t" + b"b" * 64 + b"\n"
        local_before = b"02/2\t10\t" + b"c" * 64 + b"\n03/3\t10\t" + b"d" * 64 + b"\n"
        local_final = b"01/1\t10\t" + b"a" * 64 + b"\n02/2\t10\t" + b"b" * 64 + b"\n03/3\t10\t" + b"d" * 64 + b"\n"
        retained = b"03/3\t10\t" + b"d" * 64 + b"\n"
        for name in sorted(phase_digests):
            if outcome == "s3-accepted" and name in {
                "source_provisional_sha256", "source_frozen_sha256", "copy_pass_1_sha256",
                "copy_pass_2_sha256", "destination_census_pass_1_sha256",
                "destination_census_pass_2_sha256",
            }:
                content = source_manifest
            elif outcome == "recovered-local" and name in {
                "s3_census_pass_1_sha256", "s3_census_pass_2_sha256",
                "restore_pass_1_sha256", "restore_pass_2_sha256",
            }:
                content = source_manifest
            elif outcome == "recovered-local" and name in {
                "local_census_pass_1_sha256", "local_census_pass_2_sha256",
            }:
                content = local_final
            elif outcome == "recovered-local" and name == "local_before_restore_sha256":
                content = local_before
            elif outcome == "recovered-local" and name == "retained_keys_sha256":
                content = retained
            elif outcome == "recovered-local" and name == "recovery_reference_rows_sha256":
                content = b"file\ttrue\t01/1\n"
            elif outcome == "recovered-local" and name == "recovery_active_links_sha256":
                content = b"messages\t1\tfalse\n"
            elif name == "schema_evidence_sha256":
                migration = qualifier_fixtures.good_migration_evidence()
                observed = {
                    "applied_revisions": migration["target_revisions"],
                    "revision_detail": migration["revision_detail"],
                    "migration_66_schema": migration["migration_66_schema"],
                }
                content = (
                    json.dumps({
                        **migration,
                        "live_capture": {
                            "schema": "teagram.live-migration-schema/v1",
                            "captured_at": "2026-10-08T01:04:00Z",
                            "dump_sha256": phase_digests["dump_sha256"],
                            "query_sha256": qualifier.LIVE_SCHEMA_QUERY_SHA256,
                            "query_output_sha256": "f" * 64,
                            "observed": observed,
                        }
                    }, sort_keys=True, separators=(",", ":")) + "\n"
                ).encode("utf-8")
            else:
                content = ("fixture evidence: " + name + "\n").encode()
            path = phase_dir / f"{name}.tsv"
            path.write_bytes(content)
            path.chmod(0o600)
            phase_files[name] = f"{phase_dir.name}/{path.name}"
            phase_digests[name] = blob_mode.hashlib.sha256(content).hexdigest()
        proof["phase_files"] = phase_files
        evidence = proof["evidence"]
        assert isinstance(evidence, dict)
        if outcome == "s3-accepted":
            manifest_sha = phase_digests["source_frozen_sha256"]
            evidence["source_manifest_sha256"] = manifest_sha
            evidence["destination_manifest_sha256"] = manifest_sha
        else:
            evidence["s3_census_manifest_sha256"] = phase_digests["s3_census_pass_1_sha256"]
            evidence["restored_manifest_sha256"] = phase_digests["local_census_pass_1_sha256"]
            evidence["object_count"] = 3
            evidence["byte_total"] = 30

    def create_initial_authority(self, root: pathlib.Path) -> tuple[pathlib.Path, pathlib.Path, pathlib.Path]:
        checkout = root / "checkout"
        state_dir = checkout / ".state" / "blob-mode"
        (state_dir / "journal").mkdir(parents=True)
        report_root = root / "reports"
        report_root.mkdir(mode=0o700)
        override = checkout / "docker-compose.override.yml"
        override.write_text("override: synthetic\n", encoding="utf-8")
        mode_source = os.path.realpath(state_dir)
        baseline_compose = self.local_compose()
        baseline_compose["mode_source"] = mode_source
        for service in baseline_compose["services"]:
            service["blob_mode_mounts"] = []
        guarded_compose = self.local_compose()
        guarded_compose["mode_source"] = mode_source
        for service in guarded_compose["services"]:
            for mount in service["blob_mode_mounts"]:
                mount["source"] = mode_source
        local_inventory = self.local_containers(guarded=False)
        local_inventory["mode_source"] = mode_source

        def write_json(name: str, value: object) -> pathlib.Path:
            path = root / name
            path.write_text(json.dumps(value), encoding="utf-8")
            path.chmod(0o600)
            return path

        args = SimpleNamespace(
            lock_path=root / "deploy.lock",
            state_dir=state_dir,
            report_root=report_root,
            baseline_containers=write_json("baseline-containers.json", local_inventory),
            current_containers=write_json("current-containers.json", local_inventory),
            baseline_compose=write_json("baseline-compose.json", baseline_compose),
            target_compose=write_json("target-compose.json", guarded_compose),
            override=override,
            checkout=checkout,
            target_sha="f" * 40,
            baseline_sha="9" * 40,
        )
        with (
            patch.object(blob_mode, "require_runner_lock"),
            patch.object(blob_mode, "docker_volume_exists"),
            patch.object(blob_mode.os, "fchown"),
            patch.object(blob_mode.os, "chown"),
            patch.object(blob_mode, "file_stat", side_effect=self.allow_unowned_private_files),
        ):
            blob_mode.init_local(args)
        return state_dir, report_root, args.lock_path

    def allow_unowned_private_files(self, path, kind, expected_mode=None):
        info = pathlib.Path(path).lstat()
        self.assertFalse(stat.S_ISLNK(info.st_mode))
        self.assertEqual(stat.S_IFMT(info.st_mode), kind)
        self.assertEqual(info.st_mode & 0o022, 0)
        if expected_mode is not None:
            self.assertEqual(stat.S_IMODE(info.st_mode), expected_mode)
        return info

    def publish(self, state_dir: pathlib.Path, report_root: pathlib.Path, lock_path: pathlib.Path, root: pathlib.Path, outcome: str) -> pathlib.Path:
        proof_path = root / f"{outcome}.json"
        proof = self.transition_proof(outcome)
        self.materialize_transition_proof(proof, report_root)
        proof_path.write_text(json.dumps(proof), encoding="utf-8")
        proof_path.chmod(0o600)
        args = SimpleNamespace(
            outcome=outcome,
            proof=proof_path,
            state_dir=state_dir,
            report_root=report_root,
            lock_path=lock_path,
        )
        blob_mode.publish_transition(args)
        return proof_path

    def test_local_inventory_preserves_exact_backend_and_volume_names(self) -> None:
        inventory = self.local_compose()
        blob_mode.assert_compose_matches(inventory, self.record, self.mode_source, self.override)
        with patch.object(blob_mode, "docker_volume_exists"):
            blob_mode.assert_containers_match(self.local_containers(), self.record, self.mode_source)

    def test_blob_settings_and_tgblobs_mounts_are_forbidden_in_override(self) -> None:
        cases = (
            (
                "services:\n  telegramd:\n    environment:\n      TG_BLOB_S3_ENDPOINT: \"\"\n",
                "override-blob-setting",
            ),
            (
                "services:\n  telegramd:\n    volumes:\n      - tgblobs:/var/lib/telegramd-blobs:ro\n",
                "override-tgblobs-mount",
            ),
        )
        with tempfile.TemporaryDirectory() as temporary:
            override = pathlib.Path(temporary) / "docker-compose.override.yml"
            for contents, reason in cases:
                with self.subTest(reason=reason):
                    override.write_text(contents, encoding="utf-8")
                    self.assert_rejects(
                        lambda: blob_mode.assert_compose_matches(
                            self.local_compose(), self.record, self.mode_source, override
                        ),
                        reason,
                    )

    def test_missing_proxy_guard_rejects_before_replacement(self) -> None:
        self.assert_rejects(
            lambda: blob_mode.assert_compose_matches(
                self.local_compose(proxy_guard=False), self.record, self.mode_source, self.override
            ),
            "render-mode-mount-invalid",
        )

    def test_wrong_mode_source_rejects(self) -> None:
        inventory = self.local_compose()
        inventory["services"][0]["blob_mode_mounts"][0]["source"] = "/tmp/other-mode"
        self.assert_rejects(
            lambda: blob_mode.assert_compose_matches(inventory, self.record, self.mode_source, self.override),
            "render-mode-mount-invalid",
        )

    def test_symlinked_mode_source_retarget_rejects(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            checkout = pathlib.Path(temporary) / "checkout"
            mode_dir = checkout / ".state" / "blob-mode"
            mode_dir.mkdir(parents=True)
            alias = checkout / ".state" / "current-mode"
            alias.symlink_to(mode_dir, target_is_directory=True)
            compose = {
                "services": {
                    "telegramd": {
                        "environment": {"TG_BLOB_DIR": blob_mode.BLOB_TARGET},
                        "volumes": [
                            {"type": "volume", "source": "tgblobs", "target": blob_mode.BLOB_TARGET},
                            {"type": "bind", "source": str(alias), "target": "/run/other", "read_only": True},
                        ],
                    }
                },
                "volumes": {"tgblobs": {"name": self.volume}},
            }
            inventory = blob_mode.compose_inventory(compose, checkout)
            self.assert_rejects(
                lambda: blob_mode.assert_compose_matches(
                    inventory, self.record, os.fspath(mode_dir), self.override
                ),
                "render-mode-mount-invalid",
            )

    def test_unprotected_helper_mode_mount_rejects(self) -> None:
        compose = {
            "services": {
                "telegramd": {
                    "environment": {"TG_BLOB_DIR": blob_mode.BLOB_TARGET},
                    "volumes": [{
                        "type": "bind", "source": self.mode_source,
                        "target": blob_mode.MODE_TARGET, "read_only": True,
                    }],
                },
                "rustfs-init": {
                    "volumes": [{
                        "type": "bind", "source": self.mode_source,
                        "target": blob_mode.MODE_TARGET, "read_only": True,
                    }]
                },
            },
            "volumes": {"tgblobs": {"name": self.volume}},
        }
        self.assert_rejects(
            lambda: blob_mode.compose_inventory(compose, self.checkout),
            "mode-mount-helper",
        )

    def test_running_helper_mode_mount_rejects(self) -> None:
        inspected = [{
            "Id": "b" * 64,
            "Config": {"Labels": {"com.docker.compose.service": "rustfs-init"}},
            "State": {"Status": "running"},
            "Mounts": [{
                "Type": "bind", "Source": self.mode_source,
                "Destination": blob_mode.MODE_TARGET, "RW": False,
            }],
        }]
        inventory = blob_mode.container_inventory(inspected, self.checkout, allow_empty=True)
        with patch.object(blob_mode, "docker_volume_exists"):
            self.assert_rejects(
                lambda: blob_mode.assert_containers_match(
                    inventory, self.record, self.mode_source, allow_no_telegramd=True
                ),
                "running-mode-mount-helper",
            )

    def test_guardless_running_container_requires_explicit_initial_local_exception(self) -> None:
        inventory = self.local_containers(guarded=False)
        with patch.object(blob_mode, "docker_volume_exists"):
            self.assert_rejects(
                lambda: blob_mode.assert_containers_match(inventory, self.record, self.mode_source),
                "running-mode-mount-missing",
            )
            blob_mode.assert_containers_match(
                inventory, self.record, self.mode_source, allow_unguarded_initial_local=True
            )

    def test_s3_head_never_accepts_guardless_render_or_local_backend(self) -> None:
        s3_record = dict(self.record)
        s3_record.update({
            "generation": 2,
            "transition_id": "00000000-0000-4000-8000-000000000002",
            "supersedes": self.record["transition_id"],
            "outcome": "s3-accepted",
            "backend": {"kind": "s3", "endpoint": "http://rustfs:9000", "bucket": "telegram", "prefix": "telegramd/"},
            "volumes": {"tgblobs": self.volume, "rustfsdata": "telegram-server_rustfsdata"},
        })
        self.assert_rejects(
            lambda: blob_mode.assert_compose_matches(
                self.local_compose(), s3_record, self.mode_source, self.override,
                allow_unguarded_initial_local=True,
            ),
            "render-backend-mismatch",
        )

    def test_initial_local_record_rejects_wrong_generation_type_or_backend_path(self) -> None:
        invalid_generation = dict(self.record)
        invalid_generation["generation"] = True
        self.assert_rejects(
            lambda: blob_mode.valid_outcome(invalid_generation, 1, None, None),
            "record-sequence",
        )
        wrong_backend = dict(self.record)
        wrong_backend["backend"] = {"kind": "local", "dir": "/tmp/unmounted"}
        self.assert_rejects(
            lambda: blob_mode.valid_outcome(wrong_backend, 1, None, None),
            "record-local-dir",
        )

    def test_initial_state_inspection_does_not_remove_prepublication_files(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            state_dir = pathlib.Path(temporary) / "blob-mode"
            journal = state_dir / "journal"
            journal.mkdir(parents=True)
            staged = journal / ".tmp-00000000-0000-4000-8000-000000000001"
            staged.write_text("incomplete publication", encoding="utf-8")

            with patch.object(blob_mode, "file_stat", side_effect=lambda path, _kind, expected_mode=None: path.lstat()):
                discovered = blob_mode.prepare_initial_state(state_dir)
                self.assertEqual(discovered, [staged])
                self.assertEqual(staged.read_text(encoding="utf-8"), "incomplete publication")
                blob_mode.cleanup_initial_state(state_dir, discovered)

            self.assertFalse(staged.exists())

    def test_initial_local_report_keeps_captured_inventories(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            checkout = root / "checkout"
            state_dir = checkout / ".state" / "blob-mode"
            (state_dir / "journal").mkdir(parents=True)
            report_root = root / "reports"
            report_root.mkdir(mode=0o700)
            override = checkout / "docker-compose.override.yml"
            override.write_text("override: synthetic\n", encoding="utf-8")
            mode_source = os.path.realpath(state_dir)
            backend = {"kind": "local", "dir": blob_mode.BLOB_TARGET}
            local_mount = {
                "type": "volume", "source": self.volume,
                "target": blob_mode.BLOB_TARGET, "read_only": False,
            }
            mode_mount = {
                "type": "bind", "source": mode_source,
                "target": blob_mode.MODE_TARGET, "read_only": True,
            }
            baseline_compose = {
                "services": [{
                    "name": "telegramd", "backend": backend,
                    "blob_mode_mounts": [], "tgblobs_mounts": [local_mount],
                }],
                "volumes": {"tgblobs": self.volume, "rustfsdata": None},
                "mode_source": mode_source,
            }
            target_compose = {
                "services": [{
                    "name": "telegramd", "backend": backend,
                    "blob_mode_mounts": [mode_mount], "tgblobs_mounts": [local_mount],
                }],
                "volumes": {"tgblobs": self.volume, "rustfsdata": None},
                "mode_source": mode_source,
            }
            container = {
                "id": "a" * 64, "service": "telegramd", "backend": backend,
                "mode_mounts": [],
                "tgblobs_mounts": [{
                    "type": "volume", "name": self.volume,
                    "target": blob_mode.BLOB_TARGET, "rw": True,
                }],
            }
            baseline_containers = {"containers": [container], "mode_source": mode_source}
            current_containers = {"containers": [container], "mode_source": mode_source}

            def write_json(name: str, value: object) -> pathlib.Path:
                path = root / name
                path.write_text(json.dumps(value), encoding="utf-8")
                return path

            args = SimpleNamespace(
                lock_path=root / "deploy.lock",
                state_dir=state_dir,
                report_root=report_root,
                baseline_containers=write_json("baseline-containers.json", baseline_containers),
                current_containers=write_json("current-containers.json", current_containers),
                baseline_compose=write_json("baseline-compose.json", baseline_compose),
                target_compose=write_json("target-compose.json", target_compose),
                override=override,
                checkout=checkout,
                target_sha="f" * 40,
                baseline_sha="9" * 40,
            )

            def allow_unowned_private_files(path, kind, expected_mode=None):
                info = pathlib.Path(path).lstat()
                self.assertFalse(stat.S_ISLNK(info.st_mode))
                self.assertEqual(stat.S_IFMT(info.st_mode), kind)
                self.assertEqual(info.st_mode & 0o022, 0)
                if expected_mode is not None:
                    self.assertEqual(stat.S_IMODE(info.st_mode), expected_mode)
                return info

            with (
                patch.object(blob_mode, "require_runner_lock"),
                patch.object(blob_mode, "docker_volume_exists"),
                patch.object(blob_mode.os, "fchown"),
                patch.object(blob_mode, "file_stat", side_effect=allow_unowned_private_files),
            ):
                blob_mode.init_local(args)
                record = json.loads((state_dir / "mode.json").read_text(encoding="utf-8"))
                blob_mode.validate_report(report_root, record)
                report_path = blob_mode.report_path(report_root, record["transition_id"])
                report = json.loads(report_path.read_text(encoding="utf-8"))

            self.assertEqual(report["containers"], current_containers)
            self.assertEqual(report["compose"], baseline_compose)

    def test_two_pass_transitions_publish_and_validate_private_reports(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path = self.create_initial_authority(root)
            with (
                patch.object(blob_mode, "require_runner_lock"),
                patch.object(blob_mode, "file_stat", side_effect=self.allow_unowned_private_files),
                patch.object(blob_mode.os, "fchown"),
                patch.object(blob_mode.os, "chown"),
            ):
                self.publish(state_dir, report_root, lock_path, root, "s3-accepted")
                records, head, mode_bytes = blob_mode.read_authority(state_dir, report_root)
                self.assertEqual(len(records), 2)
                self.assertEqual(records[-1]["outcome"], "s3-accepted")
                self.assertEqual(head, mode_bytes)
                report_path = blob_mode.report_path(report_root, records[-1]["transition_id"])
                self.assertEqual(stat.S_IMODE(report_path.stat().st_mode), 0o600)

                self.publish(state_dir, report_root, lock_path, root, "recovered-local")
                records, head, mode_bytes = blob_mode.read_authority(state_dir, report_root)
                self.assertEqual(len(records), 3)
                self.assertEqual(records[-1]["outcome"], "recovered-local")
                self.assertEqual(records[-1]["evidence"]["retained_cutover_key_count"], 1)
                self.assertEqual(head, mode_bytes)

    def test_transition_validation_failure_preserves_authority_tree(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path = self.create_initial_authority(root)
            state_before = {
                path.relative_to(state_dir): path.read_bytes()
                for path in state_dir.rglob("*") if path.is_file()
            }
            proof = self.transition_proof("s3-accepted")
            self.materialize_transition_proof(proof, report_root)
            changed_phase = report_root / proof["phase_files"]["copy_pass_2_sha256"]
            changed_phase.write_bytes(b"01/1\t10\t" + b"a" * 64 + b"\n02/2\t10\t" + b"e" * 64 + b"\n")
            changed_phase.chmod(0o600)
            proof["phase_digests"]["copy_pass_2_sha256"] = blob_mode.hashlib.sha256(changed_phase.read_bytes()).hexdigest()
            proof_path = root / "invalid-proof.json"
            proof_path.write_text(json.dumps(proof), encoding="utf-8")
            proof_path.chmod(0o600)
            args = SimpleNamespace(
                outcome="s3-accepted", proof=proof_path, state_dir=state_dir,
                report_root=report_root, lock_path=lock_path,
            )
            with (
                patch.object(blob_mode, "require_runner_lock"),
                patch.object(blob_mode, "file_stat", side_effect=self.allow_unowned_private_files),
                patch.object(blob_mode.os, "fchown"),
                patch.object(blob_mode.os, "chown"),
            ):
                self.assert_rejects(lambda: blob_mode.publish_transition(args), "transition-report-evidence")
            state_after = {
                path.relative_to(state_dir): path.read_bytes()
                for path in state_dir.rglob("*") if path.is_file()
            }
            self.assertEqual(state_after, state_before)

    def test_transition_rejects_schema_capture_bound_to_another_dump(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path = self.create_initial_authority(root)
            proof = self.transition_proof("s3-accepted")
            self.materialize_transition_proof(proof, report_root)
            schema_path = report_root / proof["phase_files"]["schema_evidence_sha256"]
            schema = json.loads(schema_path.read_text(encoding="utf-8"))
            schema["live_capture"]["dump_sha256"] = "0" * 64
            schema_path.write_text(json.dumps(schema), encoding="utf-8")
            schema_path.chmod(0o600)
            proof["phase_digests"]["schema_evidence_sha256"] = blob_mode.hashlib.sha256(
                schema_path.read_bytes()
            ).hexdigest()
            proof_path = root / "mismatched-schema-dump-proof.json"
            proof_path.write_text(json.dumps(proof), encoding="utf-8")
            proof_path.chmod(0o600)
            args = SimpleNamespace(
                outcome="s3-accepted", proof=proof_path, state_dir=state_dir,
                report_root=report_root, lock_path=lock_path,
            )
            with (
                patch.object(blob_mode, "require_runner_lock"),
                patch.object(blob_mode, "file_stat", side_effect=self.allow_unowned_private_files),
                patch.object(blob_mode.os, "fchown"),
                patch.object(blob_mode.os, "chown"),
            ):
                self.assert_rejects(lambda: blob_mode.publish_transition(args), "transition-report-evidence")
            self.assertEqual(json.loads((state_dir / "mode.json").read_text())["outcome"], "initial-local")

    def test_transition_rejects_r67_baseline_capture_bound_to_another_dump(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path = self.create_initial_authority(root)
            proof = self.transition_proof("s3-accepted")
            self.materialize_transition_proof(proof, report_root)
            schema_path = report_root / proof["phase_files"]["schema_evidence_sha256"]
            original = json.loads(schema_path.read_text(encoding="utf-8"))
            migration = qualifier_fixtures.good_migration_evidence("60-67")
            observed = {
                "applied_revisions": migration["target_revisions"],
                "revision_detail": migration["revision_detail"],
                "migration_66_schema": migration["migration_66_schema"],
                "migration_67_schema": migration["migration_67_schema"],
            }
            capture = {
                **original["live_capture"],
                "observed": observed,
            }
            schema = {
                **migration,
                "live_capture": capture,
                "baseline_live_capture": {
                    **capture,
                    "captured_at": "2026-10-08T01:03:00Z",
                    "dump_sha256": "0" * 64,
                    "query_output_sha256": "e" * 64,
                },
            }
            schema_path.write_text(json.dumps(schema), encoding="utf-8")
            schema_path.chmod(0o600)
            proof["phase_digests"]["schema_evidence_sha256"] = blob_mode.hashlib.sha256(
                schema_path.read_bytes()
            ).hexdigest()
            proof_path = root / "r67-baseline-schema-dump-proof.json"
            proof_path.write_text(json.dumps(proof), encoding="utf-8")
            proof_path.chmod(0o600)
            args = SimpleNamespace(
                outcome="s3-accepted", proof=proof_path, state_dir=state_dir,
                report_root=report_root, lock_path=lock_path,
            )
            with (
                patch.object(blob_mode, "require_runner_lock"),
                patch.object(blob_mode, "file_stat", side_effect=self.allow_unowned_private_files),
                patch.object(blob_mode.os, "fchown"),
                patch.object(blob_mode.os, "chown"),
            ):
                self.assert_rejects(lambda: blob_mode.publish_transition(args), "transition-report-evidence")
            self.assertEqual(json.loads((state_dir / "mode.json").read_text())["outcome"], "initial-local")

    def test_transition_rejects_reused_independent_pass_artifact(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path = self.create_initial_authority(root)
            proof = self.transition_proof("s3-accepted")
            self.materialize_transition_proof(proof, report_root)
            proof["phase_files"]["destination_census_pass_2_sha256"] = proof["phase_files"]["destination_census_pass_1_sha256"]
            proof_path = root / "reused-artifact.json"
            proof_path.write_text(json.dumps(proof), encoding="utf-8")
            proof_path.chmod(0o600)
            args = SimpleNamespace(
                outcome="s3-accepted", proof=proof_path, state_dir=state_dir,
                report_root=report_root, lock_path=lock_path,
            )
            with (
                patch.object(blob_mode, "require_runner_lock"),
                patch.object(blob_mode, "file_stat", side_effect=self.allow_unowned_private_files),
                patch.object(blob_mode.os, "fchown"),
                patch.object(blob_mode.os, "chown"),
            ):
                self.assert_rejects(lambda: blob_mode.publish_transition(args), "transition-report-evidence")
            self.assertEqual(json.loads((state_dir / "mode.json").read_text())["outcome"], "initial-local")

    def test_synced_evidence_before_journal_interruption_keeps_local_authority(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path = self.create_initial_authority(root)
            state_before = {
                path.relative_to(state_dir): path.read_bytes()
                for path in state_dir.rglob("*") if path.is_file()
            }
            proof = self.transition_proof("s3-accepted")
            self.materialize_transition_proof(proof, report_root)
            proof_path = root / "synced-before-journal.json"
            proof_path.write_text(json.dumps(proof), encoding="utf-8")
            proof_path.chmod(0o600)
            args = SimpleNamespace(
                outcome="s3-accepted", proof=proof_path, state_dir=state_dir,
                report_root=report_root, lock_path=lock_path,
            )
            with (
                patch.object(blob_mode, "require_runner_lock"),
                patch.object(blob_mode, "file_stat", side_effect=self.allow_unowned_private_files),
                patch.object(blob_mode.os, "fchown"),
                patch.object(blob_mode.os, "chown"),
                patch.object(blob_mode, "atomic_publish_state", side_effect=blob_mode.Reject("simulated-before-journal")),
            ):
                self.assert_rejects(lambda: blob_mode.publish_transition(args), "simulated-before-journal")
            state_after = {
                path.relative_to(state_dir): path.read_bytes()
                for path in state_dir.rglob("*") if path.is_file()
            }
            self.assertEqual(state_after, state_before)
            self.assertEqual(json.loads((state_dir / "mode.json").read_text())["outcome"], "initial-local")

    def test_transition_head_publication_crash_reconciles_from_matching_report(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path = self.create_initial_authority(root)
            with (
                patch.object(blob_mode, "require_runner_lock"),
                patch.object(blob_mode, "file_stat", side_effect=self.allow_unowned_private_files),
                patch.object(blob_mode.os, "fchown"),
                patch.object(blob_mode.os, "chown"),
                patch.object(blob_mode.os, "replace", side_effect=OSError("simulated head crash")),
            ):
                self.assert_rejects(
                    lambda: self.publish(state_dir, report_root, lock_path, root, "s3-accepted"),
                    "mode-publication",
                )
            journal_head = (state_dir / "journal" / "0000000002.json").read_bytes()
            self.assertNotEqual((state_dir / "mode.json").read_bytes(), journal_head)

            args = SimpleNamespace(state_dir=state_dir, report_root=report_root, lock_path=lock_path)
            with (
                patch.object(blob_mode, "require_runner_lock"),
                patch.object(blob_mode, "file_stat", side_effect=self.allow_unowned_private_files),
                patch.object(blob_mode.os, "fchown"),
                patch.object(blob_mode.os, "chown"),
            ):
                blob_mode.reconcile(args)
                records, head, mode_bytes = blob_mode.read_authority(state_dir, report_root)
            self.assertEqual(records[-1]["outcome"], "s3-accepted")
            self.assertEqual(head, journal_head)
            self.assertEqual(mode_bytes, journal_head)


if __name__ == "__main__":
    unittest.main()
