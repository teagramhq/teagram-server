#!/usr/bin/env python3
"""Credential-free unit checks for rollout blob-mode inventory gates."""

from __future__ import annotations

import importlib.util
import json
import os
import pathlib
import shutil
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
DEFAULT_NETWORK = {"mode": "networks", "networks": ["teagram-server_default"]}


class BlobModeStateTests(unittest.TestCase):
    def setUp(self) -> None:
        self.checkout = pathlib.Path("/srv/teagram-server")
        self.mode_source = str(self.checkout / ".state" / "blob-mode")
        self.volume = "teagram-server_tgblobs"
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

    def test_private_rustfs_network_name_comes_from_rustfs_volume(self) -> None:
        record = {
            "volumes": {
                "tgblobs": "teagram-server_tgblobs",
                "rustfsdata": "candidate-checkout_rustfsdata",
            },
        }

        self.assertEqual(
            blob_mode.rustfs_private_network_name(record),
            "candidate-checkout_rustfs_private",
        )

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
            "services": [service("teagramd"), service("teagramd-proxy", proxy_guard)],
            "volumes": {"tgblobs": self.volume, "rustfsdata": None},
            "mode_source": self.mode_source,
        }

    def local_containers(self, *, guarded: bool = True) -> dict[str, object]:
        return {
            "containers": [{
                "id": "a" * 64,
                "service": "teagramd",
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
            backend = {"kind": "s3", "endpoint": "http://rustfs:9000", "bucket": "teagram", "prefix": "teagramd/"}
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
            "volumes": {"tgblobs": self.volume, "rustfsdata": "teagram-server_rustfsdata"},
            "evidence": evidence,
            "phase_digests": phase_digests,
        }

    def materialize_transition_proof(
        self, proof: dict[str, object], report_root: pathlib.Path, release_set: str = "60-66"
    ) -> None:
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
                content = (
                    b"upload_part\ttrue\t01/1\n"
                    if release_set == "60-70"
                    else b"file\ttrue\t01/1\n"
                )
            elif outcome == "recovered-local" and name == "recovery_active_links_sha256":
                content = b"" if release_set == "60-70" else b"messages\t1\tfalse\n"
            elif name == "schema_evidence_sha256":
                migration = qualifier_fixtures.good_migration_evidence(release_set)
                detail_keys = ["applied", "total", "error", "hash"]
                if release_set == "60-70":
                    detail_keys.extend(("error_stmt_empty", "partial_hashes_empty"))
                observed = {
                    "applied_revisions": migration["target_revisions"],
                    "revision_detail": {
                        version: {
                            key: detail[key]
                            for key in detail_keys
                        }
                        for version, detail in migration["revision_detail"].items()
                    },
                    "migration_66_schema": migration["migration_66_schema"],
                }
                if release_set in {"60-67", "60-69", "60-70"}:
                    observed["migration_67_schema"] = migration["migration_67_schema"]
                capture = {
                    "schema": "teagram.live-migration-schema/v1",
                    "captured_at": "2026-10-08T01:04:00Z",
                    "dump_sha256": phase_digests["dump_sha256"],
                    "query_sha256": qualifier.live_schema_query_sha256(release_set),
                    "query_output_sha256": "f" * 64,
                    "observed": observed,
                }
                schema_document = {**migration, "live_capture": capture}
                if release_set in {"60-67", "60-69", "60-70"}:
                    schema_document["baseline_live_capture"] = {
                        **capture,
                        "captured_at": "2026-10-08T01:03:00Z",
                        "query_output_sha256": "e" * 64,
                    }
                content = (
                    json.dumps(schema_document, sort_keys=True, separators=(",", ":")) + "\n"
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
        checkout.mkdir()
        report_root = root / "reports"
        report_root.mkdir(mode=0o700)
        override = checkout / "docker-compose.override.yml"
        override.write_text("override: synthetic\n", encoding="utf-8")
        mode_source = os.path.realpath(state_dir)
        backend = {"kind": "local", "dir": blob_mode.BLOB_TARGET}
        mode_mount = {
            "type": "bind", "source": mode_source,
            "target": blob_mode.MODE_TARGET, "read_only": True,
        }
        local_mount = {
            "type": "volume", "source": self.volume,
            "target": blob_mode.BLOB_TARGET, "read_only": False,
        }
        key_mount = {
            "type": "volume", "source": "fixture_tgkey",
            "target": blob_mode.KEY_TARGET, "read_only": False,
        }
        pgdata_mount = {
            "type": "volume", "source": "fixture_pgdata",
            "target": blob_mode.PGDATA_TARGET, "read_only": False,
        }
        environment_sha256 = blob_mode.teagramd_environment_sha256({
            **blob_mode.IMAGE_ENV_DEFAULTS, "TG_BLOB_DIR": blob_mode.BLOB_TARGET,
        })
        baseline = {
            "containers": [
                {
                    "id": "a" * 64,
                    "service": "teagramd",
                    "backend": backend,
                    "mode_mounts": [],
                    "tgblobs_mounts": [{
                        "type": "volume", "name": self.volume,
                        "target": blob_mode.BLOB_TARGET, "rw": True,
                    }],
                    "ports": [],
                    "network": DEFAULT_NETWORK,
                    "tgkey_mounts": [{**key_mount}],
                    "tg_environment_sha256": environment_sha256,
                },
                {
                    "id": "b" * 64,
                    "service": "postgres",
                    "mode_mounts": [],
                    "pgdata_mounts": [{**pgdata_mount}],
                },
            ],
            "mode_source": mode_source,
        }
        target_compose = {
            "services": [{
                "name": "teagramd",
                "backend": backend,
                "blob_mode_mounts": [mode_mount],
                "tgblobs_mounts": [local_mount],
                "ports": [],
                "network": DEFAULT_NETWORK,
                "tgkey_mounts": [{**key_mount}],
                "tg_environment_sha256": environment_sha256,
                "initial_local_guard_environment": blob_mode.INITIAL_LOCAL_GUARD_ENV,
            }],
            "volumes": {
                "tgblobs": self.volume,
                "rustfsdata": None,
                "tgkey": "fixture_tgkey",
                "pgdata": "fixture_pgdata",
            },
            "postgres_mounts": [pgdata_mount],
            "mode_source": mode_source,
        }

        def write_json(name: str, value: object) -> pathlib.Path:
            path = root / name
            path.write_text(json.dumps(value), encoding="utf-8")
            path.chmod(0o600)
            return path

        baseline_path = write_json("baseline-containers.json", baseline)
        target_path = write_json("target-compose.json", target_compose)
        args = SimpleNamespace(
            lock_path=root / "deploy.lock",
            state_dir=state_dir,
            report_root=report_root,
            baseline_containers=baseline_path,
            current_containers=write_json("current-containers.json", baseline),
            preflight_target_compose=write_json("preflight-target-compose.json", target_compose),
            target_compose=target_path,
            target_artifact_sha256="a" * 64,
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

    def publish(
        self,
        state_dir: pathlib.Path,
        report_root: pathlib.Path,
        lock_path: pathlib.Path,
        root: pathlib.Path,
        outcome: str,
        release_set: str = "60-66",
    ) -> pathlib.Path:
        proof_path = root / f"{outcome}.json"
        proof = self.transition_proof(outcome)
        self.materialize_transition_proof(proof, report_root, release_set)
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

    def simple_s3_proof_file(
        self,
        root: pathlib.Path,
        report_root: pathlib.Path,
        *,
        exposed: bool = False,
        external_network: bool = False,
        mismatched_destination: bool = False,
    ) -> pathlib.Path:
        phase_dir = report_root / f"simple-transition-proof-{len(list(report_root.iterdir()))}"
        phase_dir.mkdir(mode=0o700)
        manifest = b"01/1\t10\t" + b"a" * 64 + b"\n"
        destination = b"01/1\t10\t" + b"b" * 64 + b"\n" if mismatched_destination else manifest
        dump = b"-- PostgreSQL database dump complete\n"
        dump_sha256 = blob_mode.hashlib.sha256(dump).hexdigest()
        candidate_compose = {
            "name": "teagram",
            "services": {
                "teagramd": {
                    "networks": {"default": None, "rustfs_private": None},
                },
                "rustfs": {
                    "ports": [{"target": 9000, "published": "9000"}] if exposed else [],
                    "networks": (
                        {"rustfs_private": None, "edge": None}
                        if external_network else {"rustfs_private": None}
                    ),
                },
            },
            "networks": {
                "default": {"name": "teagram_default"},
                "rustfs_private": {
                    "name": "teagram_rustfs_private",
                    "driver": "bridge",
                    "internal": True,
                    "enable_ipv6": False,
                    "driver_opts": {"com.docker.network.bridge.gateway_mode_ipv4": "isolated"},
                },
                **({"edge": {"name": "edge", "external": True}} if external_network else {}),
            },
        }
        contents = {
            "candidate_compose_sha256": json.dumps(
                candidate_compose,
                sort_keys=True,
                separators=(",", ":"),
            ).encode("utf-8"),
            "local_compose_sha256": b"local compose\n",
            "frozen_inventory_sha256": b"writer freeze verified\n",
            "dump_sha256": dump,
            "dump_restore_sha256": (
                f"restore=pass database=blob_restore_{'a' * 32} dump_sha256={dump_sha256}\n"
            ).encode("ascii"),
            "source_frozen_sha256": manifest,
            "copy_manifest_sha256": manifest,
            "destination_census_sha256": destination,
        }
        phase_files: dict[str, str] = {}
        phase_digests: dict[str, str] = {}
        for name, content in contents.items():
            path = phase_dir / f"{name}.artifact"
            path.write_bytes(content)
            path.chmod(0o600)
            phase_files[name] = f"{phase_dir.name}/{path.name}"
            phase_digests[name] = blob_mode.hashlib.sha256(content).hexdigest()
        summary = blob_mode.manifest_summary(phase_dir / "source_frozen_sha256.artifact")
        proof = {
            "schema": blob_mode.SIMPLE_TRANSITION_PROOF_SCHEMA,
            "outcome": "s3-accepted",
            "backend": {"kind": "s3", "endpoint": "http://rustfs:9000", "bucket": "teagram", "prefix": "teagramd/"},
            "volumes": {"tgblobs": self.volume, "rustfsdata": "teagram-server_rustfsdata"},
            "evidence": {
                "source_manifest_sha256": summary[0],
                "destination_manifest_sha256": summary[0],
                "object_count": summary[1],
                "byte_total": summary[2],
                "copy_passes": 1,
            },
            "phase_digests": phase_digests,
            "phase_files": phase_files,
        }
        proof_path = root / "simple-s3-proof.json"
        proof_path.write_text(json.dumps(proof), encoding="utf-8")
        proof_path.chmod(0o600)
        return proof_path

    def test_local_inventory_preserves_exact_backend_and_volume_names(self) -> None:
        inventory = self.local_compose()
        blob_mode.assert_compose_matches(inventory, self.record, self.mode_source, self.override)
        with patch.object(blob_mode, "docker_volume_exists"):
            blob_mode.assert_containers_match(self.local_containers(), self.record, self.mode_source)

    def test_moved_local_authority_validates_compose_and_container_inventories(self) -> None:
        legacy_blob_dir = "/var/lib/telegramd-blobs"
        target_volume = "teagram_tgblobs"
        self.volume = "telegram-server_tgblobs"
        layouts = dict(blob_mode.RUNTIME_LAYOUTS)
        layouts["teagramd"] = {**layouts["teagramd"], "blobs": legacy_blob_dir}
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            with (
                patch.object(blob_mode, "BLOB_TARGET", legacy_blob_dir),
                patch.object(blob_mode, "BLOB_TARGETS", {legacy_blob_dir}),
                patch.object(blob_mode, "RUNTIME_LAYOUTS", layouts),
            ):
                state_dir, report_root, _lock_path = self.create_initial_authority(root)

            inventory_paths: dict[str, dict[str, pathlib.Path]] = {"source": {}, "target": {}}
            for logical in ("tgblobs", "rustfsdata"):
                inventory_line = blob_mode.hashlib.sha256(f"moved {logical}".encode()).hexdigest()
                inventory = f"{inventory_line}  ./fixture.bin\n".encode()
                for side in ("source", "target"):
                    path = root / f"{logical}.{side}.sha256"
                    path.write_bytes(inventory)
                    path.chmod(0o600)
                    inventory_paths[side][logical] = path
            with (
                patch.object(blob_mode, "require_runner_lock"),
                patch.object(blob_mode, "file_stat", side_effect=self.allow_unowned_private_files),
                patch.object(blob_mode.os, "fchown"),
                patch.object(blob_mode.os, "chown"),
            ):
                moved_record = blob_mode.publish_local_stack_move(
                    SimpleNamespace(
                        state_dir=state_dir,
                        report_root=report_root,
                        lock_path=root / "deploy.lock",
                        source_inventories=inventory_paths["source"],
                        target_inventories=inventory_paths["target"],
                    ),
                )
            self.assertEqual(moved_record["volumes"]["tgblobs"], target_volume)
            self.assertEqual(moved_record["outcome"], "stack-moved-local")
            self.volume = target_volume

            checkout = state_dir.parents[1]
            mode_source = os.path.realpath(state_dir)
            s3_environment = {name: "" for name in blob_mode.S3_FIELDS}
            compose_config = {
                "services": {
                    "teagramd": {
                        "environment": {"TG_BLOB_DIR": legacy_blob_dir, **s3_environment},
                        "ports": [],
                        "volumes": [
                            {
                                "type": "volume", "source": "tgblobs",
                                "target": blob_mode.BLOB_TARGET, "read_only": True,
                            },
                            {
                                "type": "volume", "source": "tgblobs",
                                "target": legacy_blob_dir, "read_only": False,
                            },
                            {
                                "type": "bind", "source": mode_source,
                                "target": blob_mode.MODE_TARGET, "read_only": True,
                            },
                        ],
                    },
                },
                "volumes": {
                    "tgblobs": {"name": target_volume},
                    "tgkey": {"name": "teagram_tgkey"},
                    "rustfsdata": {"name": "teagram_rustfsdata"},
                },
                "networks": {"default": {"name": "teagram_default"}},
            }
            target_compose = blob_mode.compose_inventory(compose_config, checkout)
            inspected = [{
                "Id": "c" * 64,
                "Config": {
                    "Env": [
                        f"TG_BLOB_DIR={legacy_blob_dir}",
                        *(f"{name}=" for name in blob_mode.S3_FIELDS),
                        f"TG_RSA_KEY_PATH={blob_mode.KEY_TARGET}/server_key.pem",
                    ],
                    "Labels": {"com.docker.compose.service": "teagramd"},
                },
                "State": {"Status": "running"},
                "HostConfig": {"PortBindings": {}, "NetworkMode": "teagram_default"},
                "NetworkSettings": {"Networks": {"teagram_default": {}}},
                "Mounts": [
                    {
                        "Type": "volume", "Name": target_volume,
                        "Source": "/synthetic/tgblobs", "Destination": blob_mode.BLOB_TARGET,
                        "RW": False,
                    },
                    {
                        "Type": "volume", "Name": target_volume,
                        "Source": "/synthetic/tgblobs", "Destination": legacy_blob_dir,
                        "RW": True,
                    },
                    {
                        "Type": "bind", "Source": mode_source,
                        "Destination": blob_mode.MODE_TARGET, "RW": False,
                    },
                ],
            }]
            target_containers = blob_mode.container_inventory(inspected, checkout)

            def write_inventory(name: str, value: object) -> pathlib.Path:
                path = root / name
                path.write_text(json.dumps(value), encoding="utf-8")
                path.chmod(0o600)
                return path

            args = SimpleNamespace(
                state_dir=state_dir,
                report_root=report_root,
                containers=write_inventory("containers.json", target_containers),
                compose=write_inventory("compose.json", target_compose),
                override=checkout / "docker-compose.override.yml",
                checkout=checkout,
                allow_unguarded_initial_local=False,
                allow_unguarded_initial_local_containers=False,
            )
            with (
                patch.object(blob_mode, "docker_volume_exists"),
                patch.object(blob_mode, "file_stat", side_effect=self.allow_unowned_private_files),
            ):
                record = blob_mode.validate_runtime(args)

            self.assertEqual(record["backend"], {"kind": "local", "dir": legacy_blob_dir})
            self.assertEqual(record["volumes"]["tgblobs"], target_volume)
            self.assertEqual(record["outcome"], "stack-moved-local")
            self.assertEqual(target_compose["services"][0]["backend"], record["backend"])
            self.assertEqual(
                target_containers["containers"][0]["tgblobs_mounts"],
                [
                    {
                        "type": "volume", "name": self.volume,
                        "target": blob_mode.BLOB_TARGET, "rw": False,
                    },
                    {
                        "type": "volume", "name": self.volume,
                        "target": legacy_blob_dir, "rw": True,
                    },
                ],
            )

    def test_stack_move_keeps_source_authority_when_volume_inventories_differ(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            self.volume = "telegram-server_tgblobs"
            layouts = dict(blob_mode.RUNTIME_LAYOUTS)
            layouts["teagramd"] = {**layouts["teagramd"], "blobs": blob_mode.MOVED_BLOB_TARGET}
            with (
                patch.object(blob_mode, "BLOB_TARGET", blob_mode.MOVED_BLOB_TARGET),
                patch.object(blob_mode, "BLOB_TARGETS", {blob_mode.MOVED_BLOB_TARGET}),
                patch.object(blob_mode, "RUNTIME_LAYOUTS", layouts),
            ):
                state_dir, report_root, lock_path = self.create_initial_authority(root)

            source_inventories: dict[str, pathlib.Path] = {}
            target_inventories: dict[str, pathlib.Path] = {}
            for logical in ("tgblobs", "rustfsdata"):
                for side, content in (
                    ("source", b"a" * 64 + b"  ./fixture.bin\n"),
                    ("target", b"b" * 64 + b"  ./fixture.bin\n"),
                ):
                    path = root / f"{logical}.{side}.sha256"
                    path.write_bytes(content)
                    path.chmod(0o600)
                    (source_inventories if side == "source" else target_inventories)[logical] = path

            args = SimpleNamespace(
                state_dir=state_dir,
                report_root=report_root,
                lock_path=lock_path,
                source_inventories=source_inventories,
                target_inventories=target_inventories,
            )
            with (
                patch.object(blob_mode, "require_runner_lock"),
                patch.object(blob_mode, "file_stat", side_effect=self.allow_unowned_private_files),
                patch.object(blob_mode.os, "fchown"),
                patch.object(blob_mode.os, "chown"),
            ):
                self.assert_rejects(
                    lambda: blob_mode.publish_local_stack_move(args),
                    "stack-move-inventory-mismatch",
                )
                records, _head, mode_bytes = blob_mode.read_authority(state_dir, report_root)

            self.assertEqual(len(records), 1)
            self.assertEqual(records[0]["volumes"]["tgblobs"], "telegram-server_tgblobs")
            self.assertEqual(mode_bytes, _head)

    def test_initial_local_inventory_compares_live_and_target_mounts_exposure_and_environment(self) -> None:
        self.assert_initial_local_inventory(
            "teagramd", blob_mode.MODE_TARGET, blob_mode.BLOB_TARGET, blob_mode.KEY_TARGET,
        )

    def test_initial_local_environment_ignores_only_disabled_public_websocket_defaults(self) -> None:
        baseline = {
            "TG_RSA_KEY_PATH": blob_mode.IMAGE_ENV_DEFAULTS["TG_RSA_KEY_PATH"],
            "TG_BLOB_DIR": blob_mode.BLOB_TARGET,
            "TG_SYNTHETIC_FLAG": "fixture",
        }
        with_empty_advertise = {**baseline, "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR": ""}
        self.assertEqual(
            blob_mode.teagramd_environment_sha256(baseline),
            blob_mode.teagramd_environment_sha256(with_empty_advertise),
        )
        with_public_advertise = {
            **baseline,
            "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR": "server.teagram.io:443",
        }
        self.assertNotEqual(
            blob_mode.teagramd_environment_sha256(baseline),
            blob_mode.teagramd_environment_sha256(with_public_advertise),
        )
        with_disabled_defaults = {**baseline, **blob_mode.INITIAL_LOCAL_DISABLED_ENV}
        self.assertEqual(
            blob_mode.teagramd_environment_sha256(baseline),
            blob_mode.teagramd_environment_sha256(with_disabled_defaults),
        )

        active_values = {
            "TG_PUBLIC_WEBSOCKET_LISTEN_ADDR": "0.0.0.0:2444",
            "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR": "server.teagram.io:443",
            "TG_PUBLIC_REGISTRATION": "open",
            "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS": "https://example.invalid",
        }
        for key, disabled_value in blob_mode.INITIAL_LOCAL_DISABLED_ENV.items():
            with self.subTest(key=key):
                active = {**baseline, key: active_values[key]}
                self.assertNotEqual(
                    blob_mode.teagramd_environment_sha256(baseline),
                    blob_mode.teagramd_environment_sha256(active),
                )
                non_default = {**baseline, key: disabled_value + "changed"}
                self.assertNotEqual(
                    blob_mode.teagramd_environment_sha256(baseline),
                    blob_mode.teagramd_environment_sha256(non_default),
                )

    def test_initial_local_logging_profile_is_checked_outside_blob_authority_hash(self) -> None:
        baseline = {
            "TG_RSA_KEY_PATH": blob_mode.IMAGE_ENV_DEFAULTS["TG_RSA_KEY_PATH"],
            "TG_BLOB_DIR": blob_mode.BLOB_TARGET,
            "TG_SYNTHETIC_FLAG": "fixture",
        }
        with_code_logging = {**baseline, "TG_LOG_LOGIN_CODES": "true"}
        without_code_logging = {**baseline, "TG_LOG_LOGIN_CODES": "false"}
        self.assertEqual(
            blob_mode.teagramd_environment_sha256(with_code_logging),
            blob_mode.teagramd_environment_sha256(without_code_logging),
        )
        with_unrelated_change = {**without_code_logging, "TG_SYNTHETIC_FLAG": "changed"}
        self.assertNotEqual(
            blob_mode.teagramd_environment_sha256(without_code_logging),
            blob_mode.teagramd_environment_sha256(with_unrelated_change),
        )

    def test_initial_local_inventory_accepts_live_telegramd_layout(self) -> None:
        # The live stack keeps its historical names until the stack move.
        self.assert_initial_local_inventory(
            "telegramd", "/run/telegramd/blob-mode", "/var/lib/telegramd-blobs", "/var/lib/telegramd",
        )

    def assert_initial_local_inventory(self, service: str, mode_target: str, blob_target: str, key_target: str) -> None:
        mode_mount = {
            "type": "bind", "source": self.mode_source,
            "target": mode_target, "read_only": True,
        }
        desired_environment = {
            "TG_RSA_KEY_PATH": f"{key_target}/server_key.pem",
            "TG_BLOB_DIR": blob_target,
            "TG_SYNTHETIC_FLAG": "fixture",
            **blob_mode.INITIAL_LOCAL_GUARD_ENV,
        }
        compose = {
            "services": {
                service: {
                    "environment": {
                        "TG_BLOB_DIR": blob_target,
                        "TG_SYNTHETIC_FLAG": "fixture",
                        **blob_mode.INITIAL_LOCAL_GUARD_ENV,
                    },
                    "ports": [{
                        "target": 2443, "published": "2443", "host_ip": "127.0.0.1",
                        "protocol": "tcp", "mode": "ingress",
                    }],
                    "volumes": [
                        {"type": "volume", "source": "tgkey", "target": key_target, "read_only": False},
                        {"type": "volume", "source": "tgblobs", "target": blob_target, "read_only": False},
                        mode_mount,
                    ],
                },
                "postgres": {
                    "volumes": [{"type": "volume", "source": "pgdata", "target": blob_mode.PGDATA_TARGET, "read_only": False}],
                },
            },
            "volumes": {
                "tgblobs": {"name": self.volume},
                "tgkey": {"name": "fixture_tgkey"},
                "pgdata": {"name": "fixture_pgdata"},
            },
            "networks": {"default": {"name": "teagram-server_default"}},
        }
        rendered = blob_mode.compose_inventory(compose, pathlib.Path("/srv/teagram-server"))
        self.assertEqual(rendered["services"][0]["tg_environment_sha256"], blob_mode.teagramd_environment_sha256(desired_environment))
        inspected = [{
            "Id": "a" * 64,
            "Config": {
                "Env": [f"{key}={value}" for key, value in desired_environment.items() if key not in blob_mode.INITIAL_LOCAL_GUARD_ENV],
                "Labels": {"com.docker.compose.service": service},
            },
            "State": {"Status": "running"},
            "HostConfig": {
                "PortBindings": {"2443/tcp": [{"HostIp": "127.0.0.1", "HostPort": "2443"}]},
                "NetworkMode": "teagram-server_default",
            },
            "NetworkSettings": {"Networks": {"teagram-server_default": {}}},
            "Mounts": [
                {"Type": "volume", "Name": "fixture_tgkey", "Source": "/synthetic/key", "Destination": key_target, "RW": True},
                {"Type": "volume", "Name": self.volume, "Source": "/synthetic/blobs", "Destination": blob_target, "RW": True},
            ],
        }, {
            "Id": "b" * 64,
            "Config": {"Env": [], "Labels": {"com.docker.compose.service": "postgres"}},
            "State": {"Status": "running"},
            "HostConfig": {"PortBindings": {}},
            "Mounts": [{"Type": "volume", "Name": "fixture_pgdata", "Source": "/synthetic/pgdata", "Destination": blob_mode.PGDATA_TARGET, "RW": True}],
        }]
        baseline = blob_mode.container_inventory(inspected, pathlib.Path("/srv/teagram-server"))
        self.assertEqual(baseline["containers"][0]["pgdata_mounts"], rendered["postgres_mounts"])
        live_teagramd = next(item for item in baseline["containers"] if item["service"] == service)
        self.assertEqual(
            rendered["services"][0]["ports"],
            [{"target": "2443", "published": "2443", "host_ip": "127.0.0.1", "protocol": "tcp"}],
        )
        self.assertEqual(rendered["services"][0]["ports"], live_teagramd["ports"])
        self.assertEqual(rendered["services"][0]["network"], DEFAULT_NETWORK)
        self.assertEqual(rendered["services"][0]["network"], live_teagramd["network"])
        with patch.object(blob_mode, "docker_volume_exists"):
            blob_mode.assert_compose_matches_initial(
                baseline, rendered, {"kind": "local", "dir": blob_target},
                self.volume, self.mode_source,
            )

        # PostgreSQL 18 stores PGDATA below /var/lib/postgresql and needs the
        # volume mounted at that root. Keep recognizing the PostgreSQL 16
        # /var/lib/postgresql/data layout used by existing deployments too.
        compose_18 = json.loads(json.dumps(compose))
        compose_18["services"]["postgres"]["volumes"][0]["target"] = "/var/lib/postgresql"
        rendered_18 = blob_mode.compose_inventory(compose_18, pathlib.Path("/srv/teagram-server"))
        inspected_18 = json.loads(json.dumps(inspected))
        inspected_18[1]["Mounts"][0]["Destination"] = "/var/lib/postgresql"
        baseline_18 = blob_mode.container_inventory(inspected_18, pathlib.Path("/srv/teagram-server"))
        self.assertEqual(baseline_18["containers"][0]["pgdata_mounts"], rendered_18["postgres_mounts"])
        with patch.object(blob_mode, "docker_volume_exists"):
            blob_mode.assert_compose_matches_initial(
                baseline_18, rendered_18, {"kind": "local", "dir": blob_target},
                self.volume, self.mode_source,
            )

    def test_network_inventory_records_mode_and_attached_networks(self) -> None:
        compose = {
            "services": {
                "teagramd": {"networks": {"default": None, "edge": None}},
                "teagramd-host": {"network_mode": "host"},
                "teagramd-implicit": {},
            },
            "networks": {
                "default": {"name": "teagram-server_default"},
                "edge": {"name": "edge", "external": True},
            },
        }
        services = compose["services"]
        self.assertEqual(
            blob_mode.compose_network(services["teagramd"], compose),
            {"mode": "networks", "networks": ["edge", "teagram-server_default"]},
        )
        self.assertEqual(blob_mode.compose_network(services["teagramd-host"], compose), {"mode": "host", "networks": []})
        self.assertEqual(blob_mode.compose_network(services["teagramd-implicit"], compose), DEFAULT_NETWORK)
        self.assert_rejects(
            lambda: blob_mode.compose_network({"networks": {"undeclared": None}}, compose),
            "compose-network-name",
        )
        self.assertEqual(
            blob_mode.container_network({
                "HostConfig": {"NetworkMode": "teagram-server_default"},
                "NetworkSettings": {"Networks": {"teagram-server_default": {}, "edge": {}}},
            }),
            {"mode": "networks", "networks": ["edge", "teagram-server_default"]},
        )
        self.assertEqual(
            blob_mode.container_network({
                "HostConfig": {"NetworkMode": "host"},
                "NetworkSettings": {"Networks": {"host": {}}},
            }),
            {"mode": "host", "networks": []},
        )
        for name, container in (
            ("missing settings", {"HostConfig": {"NetworkMode": "teagram-server_default"}}),
            ("primary not attached", {"HostConfig": {"NetworkMode": "teagram-server_default"}, "NetworkSettings": {"Networks": {"edge": {}}}}),
            ("built-in mode with extra network", {"HostConfig": {"NetworkMode": "bridge"}, "NetworkSettings": {"Networks": {"bridge": {}, "edge": {}}}}),
        ):
            with self.subTest(name=name):
                self.assert_rejects(lambda: blob_mode.container_network(container), "container-network")

    def test_blob_settings_and_tgblobs_mounts_are_forbidden_in_override(self) -> None:
        cases = (
            (
                "services:\n  teagramd:\n    environment:\n      TG_BLOB_S3_ENDPOINT: \"\"\n",
                "override-blob-setting",
            ),
            (
                "services:\n  teagramd:\n    volumes:\n      - tgblobs:/var/lib/teagramd-blobs:ro\n",
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

    def test_encryption_migration_profile_has_only_the_exact_writable_authority_mount(self) -> None:
        checkout = pathlib.Path(tempfile.mkdtemp())
        authority_source = checkout / ".state" / "blob-mode"
        authority_source.mkdir(parents=True)
        shared_environment = {
            "TG_BLOB_DIR": blob_mode.BLOB_TARGET,
            "TG_BLOB_S3_ENDPOINT": "https://objects.example.test",
            "TG_BLOB_S3_BUCKET": "teagram",
            "TG_BLOB_S3_PREFIX": "teagramd/",
            "TG_POSTGRES_DSN": "postgres://postgres:secret@postgres:5432/teagram",
            "TG_AUTHKEY_ENC_KEY_FILE": "/var/lib/teagramd/enc_key.hex",
            "TG_MEDIA_KEY_DIR": "/run/teagramd/media-keys",
            "TG_MEDIA_KEY_CURRENT": "1",
        }
        service = {
            "profiles": ["migration"],
            "entrypoint": ["/usr/local/bin/blob-encrypt-migrate"],
            "user": "0:0",
            "read_only": True,
            "environment": dict(shared_environment),
            "networks": {"default": None, "rustfs_private": None},
            "volumes": [{
                "type": "bind", "source": str(authority_source),
                "target": blob_mode.MODE_TARGET,
            }, {
                "type": "bind", "source": "/root", "target": "/run/teagramd/reports",
            }, {
                "type": "volume", "source": "tgkey", "target": "/var/lib/teagramd", "read_only": True,
            }, {
                "type": "bind", "source": "/etc/teagramd/media-keys",
                "target": "/run/teagramd/media-keys", "read_only": True,
            }],
        }
        compose = {
            "services": {
                "teagramd": {
                    "environment": dict(shared_environment),
                    "volumes": [
                        {"type": "volume", "source": "tgblobs", "target": blob_mode.BLOB_TARGET, "read_only": True},
                        {"type": "bind", "source": str(authority_source), "target": blob_mode.MODE_TARGET, "read_only": True},
                    ],
                    "networks": {"default": None, "rustfs_private": None},
                },
                "blob-encrypt-migrate": service,
            },
            "volumes": {
                "tgblobs": {"name": "teagram_tgblobs"},
                "rustfsdata": {"name": "teagram_rustfsdata"},
                "tgkey": {"name": "teagram_tgkey"},
            },
            "networks": {
                "default": {"name": "teagram_default"},
                "rustfs_private": {"name": "teagram_rustfs_private"},
            },
        }
        inventory = blob_mode.compose_inventory(compose, checkout)
        self.assertEqual(inventory["services"][0]["backend"]["kind"], "s3")

        for name, mutation in (
            ("not profile-gated", lambda item: item.pop("profiles")),
            ("read-only authority", lambda item: item["volumes"][0].update(read_only=True)),
            ("wrong authority target", lambda item: item["volumes"][0].update(target="/tmp/blob-mode")),
        ):
            with self.subTest(name=name):
                candidate = json.loads(json.dumps(compose))
                mutation(candidate["services"]["blob-encrypt-migrate"])
                self.assert_rejects(
                    lambda: blob_mode.compose_inventory(candidate, checkout),
                    "mode-mount-helper",
                )

    def test_key_rotation_profile_has_only_a_read_only_authority_mount(self) -> None:
        checkout = pathlib.Path(tempfile.mkdtemp())
        authority_source = checkout / ".state" / "blob-mode"
        authority_source.mkdir(parents=True)
        shared_environment = {
            "TG_BLOB_DIR": blob_mode.BLOB_TARGET,
            "TG_BLOB_S3_ENDPOINT": "https://objects.example.test",
            "TG_BLOB_S3_BUCKET": "teagram",
            "TG_BLOB_S3_PREFIX": "teagramd/",
            "TG_POSTGRES_DSN": "postgres://postgres:secret@postgres:5432/teagram",
            "TG_AUTHKEY_ENC_KEY_FILE": "/var/lib/teagramd/enc_key.hex",
            "TG_MEDIA_KEY_DIR": "/run/teagramd/media-keys",
            "TG_MEDIA_KEY_CURRENT": "2",
        }
        service = {
            "profiles": ["migration"],
            "entrypoint": ["/usr/local/bin/blob-key-rotate"],
            "user": "0:0",
            "read_only": True,
            "environment": dict(shared_environment),
            "networks": {"default": None, "rustfs_private": None},
            "volumes": [{
                "type": "bind", "source": str(authority_source),
                "target": blob_mode.MODE_TARGET, "read_only": True,
            }, {
                "type": "volume", "source": blob_mode.KEY_ROTATION_REPORT_VOLUME,
                "target": blob_mode.REPORT_TARGET,
            }, {
                "type": "volume", "source": "tgkey", "target": "/var/lib/teagramd", "read_only": True,
            }, {
                "type": "bind", "source": "/etc/teagramd/media-keys",
                "target": "/run/teagramd/media-keys", "read_only": True,
            }],
        }
        compose = {
            "services": {
                "teagramd": {
                    "environment": dict(shared_environment),
                    "volumes": [
                        {"type": "volume", "source": "tgblobs", "target": blob_mode.BLOB_TARGET, "read_only": True},
                        {"type": "bind", "source": str(authority_source), "target": blob_mode.MODE_TARGET, "read_only": True},
                    ],
                    "networks": {"default": None, "rustfs_private": None},
                },
                "blob-key-rotate": service,
            },
            "volumes": {
                "tgblobs": {"name": "teagram_tgblobs"},
                "rustfsdata": {"name": "teagram_rustfsdata"},
                "tgkey": {"name": "teagram_tgkey"},
                blob_mode.KEY_ROTATION_REPORT_VOLUME: {
                    "name": blob_mode.KEY_ROTATION_REPORT_VOLUME_NAME,
                },
            },
            "networks": {
                "default": {"name": "teagram_default"},
                "rustfs_private": {"name": "teagram_rustfs_private"},
            },
        }
        inventory = blob_mode.compose_inventory(compose, checkout)
        self.assertEqual([item["name"] for item in inventory["services"]], ["teagramd"])

        for name, mutation in (
            ("not profile-gated", lambda item: item.pop("profiles")),
            ("missing authority", lambda item: item["volumes"].pop(0)),
            ("writable authority", lambda item: item["volumes"][0].update(read_only=False)),
            ("wrong entrypoint", lambda item: item.update(entrypoint=["/bin/sh"])),
            ("host-wide reports bind", lambda item: item["volumes"][1].update(
                type="bind", source="/root",
            )),
            ("unbounded writable mount", lambda item: item["volumes"].append({
                "type": "bind", "source": "/tmp", "target": "/tmp",
            })),
        ):
            with self.subTest(name=name):
                candidate = json.loads(json.dumps(compose))
                mutation(candidate["services"]["blob-key-rotate"])
                self.assert_rejects(
                    lambda: blob_mode.compose_inventory(candidate, checkout),
                    "mode-mount-helper",
                )

        candidate = json.loads(json.dumps(compose))
        candidate["volumes"][blob_mode.KEY_ROTATION_REPORT_VOLUME]["driver_opts"] = {
            "type": "none", "o": "bind", "device": "/root",
        }
        self.assert_rejects(
            lambda: blob_mode.compose_inventory(candidate, checkout),
            "mode-mount-helper",
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
                    "teagramd": {
                        "environment": {"TG_BLOB_DIR": blob_mode.BLOB_TARGET},
                        "volumes": [
                            {"type": "volume", "source": "tgblobs", "target": blob_mode.BLOB_TARGET},
                            {"type": "bind", "source": str(alias), "target": "/run/other", "read_only": True},
                        ],
                    }
                },
                "volumes": {"tgblobs": {"name": self.volume}},
                "networks": {"default": {"name": "teagram-server_default"}},
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
                "teagramd": {
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
            "networks": {"default": {"name": "teagram-server_default"}},
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
                    inventory, self.record, self.mode_source, allow_no_teagramd=True
                ),
                "running-mode-mount-helper",
            )

    def test_running_key_rotation_helper_mode_mount_rejects(self) -> None:
        inspected = [{
            "Id": "c" * 64,
            "Config": {"Labels": {"com.docker.compose.service": "blob-key-rotate"}},
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
                    inventory, self.record, self.mode_source, allow_no_teagramd=True
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
            "backend": {"kind": "s3", "endpoint": "http://rustfs:9000", "bucket": "teagram", "prefix": "teagramd/"},
            "volumes": {"tgblobs": self.volume, "rustfsdata": "teagram-server_rustfsdata"},
        })
        self.assert_rejects(
            lambda: blob_mode.assert_compose_matches(
                self.local_compose(), s3_record, self.mode_source, self.override,
                allow_unguarded_initial_local=True,
            ),
            "render-backend-mismatch",
        )

    def test_local_render_allows_omitted_inactive_rustfs_volume(self) -> None:
        local_record = dict(self.record)
        local_record["volumes"] = {
            **self.record["volumes"],
            "rustfsdata": "teagram-server_rustfsdata",
        }
        inventory = self.local_compose()
        inventory["volumes"].pop("rustfsdata")

        blob_mode.assert_compose_matches(
            inventory, local_record, self.mode_source, self.override
        )

    def test_local_render_rejects_mismatched_rustfs_volume(self) -> None:
        local_record = dict(self.record)
        local_record["volumes"] = {
            **self.record["volumes"],
            "rustfsdata": "teagram-server_rustfsdata",
        }
        inventory = self.local_compose()
        inventory["volumes"]["rustfsdata"] = "teagram-server_other_rustfsdata"

        self.assert_rejects(
            lambda: blob_mode.assert_compose_matches(
                inventory, local_record, self.mode_source, self.override
            ),
            "render-rustfs-volume-mismatch",
        )

    def test_s3_render_still_requires_its_rustfs_volume(self) -> None:
        s3_backend = {
            "kind": "s3",
            "endpoint": "http://rustfs:9000",
            "bucket": "teagram",
            "prefix": "teagramd/",
        }
        s3_record = dict(self.record)
        s3_record.update({
            "outcome": "s3-accepted",
            "backend": s3_backend,
            "volumes": {
                "tgblobs": self.volume,
                "rustfsdata": "teagram-server_rustfsdata",
            },
        })
        inventory = self.local_compose()
        inventory["volumes"].pop("rustfsdata")
        for service in inventory["services"]:
            service["backend"] = s3_backend
            service["network"] = {
                "mode": "networks",
                "networks": ["teagram-server_default", "teagram-server_rustfs_private"],
            }
            for mount in service["tgblobs_mounts"]:
                mount["read_only"] = True

        self.assert_rejects(
            lambda: blob_mode.assert_compose_matches(
                inventory, s3_record, self.mode_source, self.override
            ),
            "render-rustfs-volume-mismatch",
        )

    def test_private_rustfs_render_requires_an_isolated_project_network(self) -> None:
        compose = {
            "name": "teagram",
            "services": {
                "rustfs": {"networks": {"rustfs_private": None}},
                "teagramd": {"networks": {"default": None, "rustfs_private": None}},
            },
            "networks": {
                "default": {"name": "teagram_default"},
                "rustfs_private": {
                    "name": "teagram_rustfs_private",
                    "driver": "bridge",
                    "internal": True,
                    "enable_ipv6": False,
                    "driver_opts": {
                        "com.docker.network.bridge.gateway_mode_ipv4": "isolated",
                    },
                },
            },
        }
        blob_mode.validate_private_rustfs_render(compose)

        invalid_renders = []
        plain_bridge = json.loads(json.dumps(compose))
        plain_bridge["networks"]["rustfs_private"]["driver_opts"][
            "com.docker.network.bridge.gateway_mode_ipv4"
        ] = "nat"
        invalid_renders.append(plain_bridge)
        external = json.loads(json.dumps(compose))
        external["networks"]["rustfs_private"]["external"] = True
        invalid_renders.append(external)
        extra_network = json.loads(json.dumps(compose))
        extra_network["services"]["rustfs"]["networks"]["edge"] = None
        extra_network["networks"]["edge"] = {"name": "edge", "external": True}
        invalid_renders.append(extra_network)

        for candidate in invalid_renders:
            with self.subTest(networks=candidate["services"]["rustfs"]["networks"]):
                self.assert_rejects(
                    lambda candidate=candidate: blob_mode.validate_private_rustfs_render(candidate),
                    "transition-rustfs-exposed",
                )

        app_missing_private_network = json.loads(json.dumps(compose))
        app_missing_private_network["services"]["teagramd"]["networks"].pop("rustfs_private")
        self.assert_rejects(
            lambda: blob_mode.validate_private_rustfs_render(app_missing_private_network),
            "transition-teagramd-rustfs-network",
        )

    def test_running_s3_teagramd_requires_the_private_rustfs_network(self) -> None:
        record = {
            "outcome": "s3-accepted",
            "backend": {
                "kind": "s3", "endpoint": "http://rustfs:9000",
                "bucket": "teagram", "prefix": "teagramd/",
            },
            "volumes": {"tgblobs": "teagram_tgblobs", "rustfsdata": "teagram_rustfsdata"},
        }
        network = {"mode": "networks", "networks": ["teagram_default", "teagram_rustfs_private"]}
        inventory = {
            "containers": [
                {
                    "service": "teagramd",
                    "backend": record["backend"],
                    "mode_mounts": blob_mode.guarded_mode_mounts("teagramd", self.mode_source),
                    "tgblobs_mounts": [{
                        "type": "volume", "name": "teagram_tgblobs",
                        "target": blob_mode.BLOB_TARGET, "rw": False,
                    }],
                    "network": network,
                },
                {
                    "service": "rustfs",
                    "mode_mounts": [],
                    "rustfs_mounts": [{
                        "type": "volume", "name": "teagram_rustfsdata",
                        "target": "/data", "rw": True,
                    }],
                    "ports": [],
                    "network": {"mode": "networks", "networks": ["teagram_rustfs_private"]},
                },
            ],
        }
        with patch.object(blob_mode, "docker_volume_exists"):
            blob_mode.assert_containers_match(inventory, record, self.mode_source)

            inventory["containers"][0]["network"] = {
                "mode": "networks", "networks": ["teagram_default"],
            }
            self.assert_rejects(
                lambda: blob_mode.assert_containers_match(inventory, record, self.mode_source),
                "running-teagramd-rustfs-network",
            )

    def test_running_s3_rustfs_rejects_ports_extra_networks_and_unisolated_networks(self) -> None:
        record = {
            "outcome": "s3-accepted",
            "volumes": {"tgblobs": "teagram_tgblobs", "rustfsdata": "teagram_rustfsdata"},
        }
        rustfs = {
            "ports": [],
            "network": {"mode": "networks", "networks": ["teagram_rustfs_private"]},
            "rustfs_mounts": [{
                "type": "volume", "name": "teagram_rustfsdata", "target": "/data", "rw": True,
            }],
        }
        blob_mode.assert_rustfs_containers_match([rustfs], record)

        exposed = json.loads(json.dumps(rustfs))
        exposed["ports"] = [{"target": "9000", "published": "9000", "host_ip": "0.0.0.0", "protocol": "tcp"}]
        self.assert_rejects(
            lambda: blob_mode.assert_rustfs_containers_match([exposed], record),
            "running-rustfs-exposed",
        )
        attached_elsewhere = json.loads(json.dumps(rustfs))
        attached_elsewhere["network"]["networks"].append("untrusted_bridge")
        self.assert_rejects(
            lambda: blob_mode.assert_rustfs_containers_match([attached_elsewhere], record),
            "running-rustfs-network",
        )
        self.assert_rejects(
            lambda: blob_mode.validate_private_rustfs_network({
                "Name": "teagram_rustfs_private",
                "Driver": "bridge",
                "Internal": True,
                "EnableIPv6": False,
                "Options": {"com.docker.network.bridge.gateway_mode_ipv4": "nat"},
            }, "teagram_rustfs_private"),
            "running-rustfs-network",
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

    def test_existing_authority_directory_is_rejected_without_mutation(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            state_dir = pathlib.Path(temporary) / "blob-mode"
            state_dir.mkdir()
            authority = state_dir / "mode.json"
            authority.write_text("published authority", encoding="utf-8")

            with patch.object(blob_mode, "file_stat", side_effect=lambda path, _kind, expected_mode=None: path.lstat()):
                with self.assertRaises(blob_mode.Reject) as caught:
                    blob_mode.prepare_initial_state(state_dir)

            self.assertEqual(str(caught.exception), "state-already-exists")
            self.assertEqual(authority.read_text(encoding="utf-8"), "published authority")

    def test_initial_local_report_keeps_captured_inventories(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            checkout = root / "checkout"
            state_dir = checkout / ".state" / "blob-mode"
            checkout.mkdir()
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
            target_compose = {
                "services": [{
                    "name": "teagramd", "backend": backend,
                    "blob_mode_mounts": [mode_mount], "tgblobs_mounts": [local_mount],
                    "ports": [],
                    "network": DEFAULT_NETWORK,
                    "tgkey_mounts": [{"type": "volume", "source": "fixture_tgkey", "target": blob_mode.KEY_TARGET, "read_only": False}],
                    "tg_environment_sha256": blob_mode.teagramd_environment_sha256({
                        **blob_mode.IMAGE_ENV_DEFAULTS, "TG_BLOB_DIR": blob_mode.BLOB_TARGET,
                    }),
                    "initial_local_guard_environment": blob_mode.INITIAL_LOCAL_GUARD_ENV,
                }],
                "volumes": {"tgblobs": self.volume, "rustfsdata": None, "tgkey": "fixture_tgkey", "pgdata": "fixture_pgdata"},
                "postgres_mounts": [{"type": "volume", "source": "fixture_pgdata", "target": blob_mode.PGDATA_TARGET, "read_only": False}],
                "mode_source": mode_source,
            }
            container = {
                "id": "a" * 64, "service": "teagramd", "backend": backend,
                "mode_mounts": [],
                "tgblobs_mounts": [{
                    "type": "volume", "name": self.volume,
                    "target": blob_mode.BLOB_TARGET, "rw": True,
                }],
                "ports": [],
                "network": DEFAULT_NETWORK,
                "tgkey_mounts": [{"type": "volume", "source": "fixture_tgkey", "target": blob_mode.KEY_TARGET, "read_only": False}],
                "tg_environment_sha256": blob_mode.teagramd_environment_sha256({
                    **blob_mode.IMAGE_ENV_DEFAULTS, "TG_BLOB_DIR": blob_mode.BLOB_TARGET,
                }),
            }
            postgres = {
                "id": "b" * 64, "service": "postgres", "mode_mounts": [],
                "pgdata_mounts": [{"type": "volume", "source": "fixture_pgdata", "target": blob_mode.PGDATA_TARGET, "read_only": False}],
            }
            baseline_containers = {"containers": [container, postgres], "mode_source": mode_source}
            current_containers = {"containers": [container, postgres], "mode_source": mode_source}

            def write_json(name: str, value: object) -> pathlib.Path:
                path = root / name
                path.write_text(json.dumps(value), encoding="utf-8")
                path.chmod(0o600)
                return path

            baseline_path = write_json("baseline-containers.json", baseline_containers)
            preflight_target_path = write_json("preflight-target-compose.json", target_compose)
            target_path = write_json("target-compose.json", target_compose)
            args = SimpleNamespace(
                lock_path=root / "deploy.lock",
                state_dir=state_dir,
                report_root=report_root,
                baseline_containers=baseline_path,
                current_containers=write_json("current-containers.json", current_containers),
                preflight_target_compose=preflight_target_path,
                target_compose=target_path,
                target_artifact_sha256="a" * 64,
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
                patch.object(blob_mode.os, "chown"),
                patch.object(blob_mode, "file_stat", side_effect=allow_unowned_private_files),
            ):
                blob_mode.init_local(args)
                record = json.loads((state_dir / "mode.json").read_text(encoding="utf-8"))
                report_path = blob_mode.report_path(report_root, record["transition_id"])
                legacy_report_path = report_root / f"telegramd-blob-mode-report-{record['transition_id']}.json"
                report_path.rename(legacy_report_path)
                records, head, mode_bytes = blob_mode.read_authority(state_dir, report_root)
                report = json.loads(legacy_report_path.read_text(encoding="utf-8"))

            self.assertEqual(records, [record])
            self.assertEqual(head, (state_dir / "journal/0000000001.json").read_bytes())
            self.assertEqual(mode_bytes, head)
            self.assertEqual(report["inspection_kind"], "unguarded-local-baseline-to-pinned-target")
            self.assertEqual(report["baseline"]["source"], "running-unguarded-containers")
            self.assertEqual(report["baseline"]["containers"], baseline_containers)
            self.assertEqual(report["target"]["source"], "pinned-target-compose")
            self.assertEqual(report["target"]["artifact_sha256"], "a" * 64)
            self.assertEqual(report["target"]["compose"], target_compose)
            self.assertEqual(report["target"]["compose"]["services"][0]["blob_mode_mounts"], [mode_mount])

    def test_initial_local_transition_rejects_drift_from_passing_fixture(self) -> None:
        mode_source = self.mode_source
        backend = {"kind": "local", "dir": blob_mode.BLOB_TARGET}
        target_mount = {
            "type": "bind", "source": mode_source,
            "target": blob_mode.MODE_TARGET, "read_only": True,
        }
        local_mount = {
            "type": "volume", "source": self.volume,
            "target": blob_mode.BLOB_TARGET, "read_only": False,
        }
        running = {
            "id": "a" * 64, "service": "teagramd", "backend": backend,
            "mode_mounts": [],
            "tgblobs_mounts": [{
                "type": "volume", "name": self.volume,
                "target": blob_mode.BLOB_TARGET, "rw": True,
            }],
            "ports": [],
            "network": DEFAULT_NETWORK,
            "tgkey_mounts": [{"type": "volume", "source": "fixture_tgkey", "target": blob_mode.KEY_TARGET, "read_only": False}],
            "tg_environment_sha256": blob_mode.teagramd_environment_sha256({
                **blob_mode.IMAGE_ENV_DEFAULTS, "TG_BLOB_DIR": blob_mode.BLOB_TARGET,
            }),
        }
        pgdata_mount = {"type": "volume", "source": "fixture_pgdata", "target": blob_mode.PGDATA_TARGET, "read_only": False}
        postgres = {
            "id": "b" * 64, "service": "postgres", "mode_mounts": [],
            "pgdata_mounts": [pgdata_mount],
        }
        baseline = {"containers": [running, postgres], "mode_source": mode_source}
        valid_target = {
            "services": [{
                "name": "teagramd", "backend": backend,
                "blob_mode_mounts": [target_mount], "tgblobs_mounts": [local_mount],
                "ports": [],
                "network": DEFAULT_NETWORK,
                "tgkey_mounts": [{"type": "volume", "source": "fixture_tgkey", "target": blob_mode.KEY_TARGET, "read_only": False}],
                "tg_environment_sha256": blob_mode.teagramd_environment_sha256({
                    **blob_mode.IMAGE_ENV_DEFAULTS, "TG_BLOB_DIR": blob_mode.BLOB_TARGET,
                }),
                "initial_local_guard_environment": blob_mode.INITIAL_LOCAL_GUARD_ENV,
            }],
            "volumes": {"tgblobs": self.volume, "rustfsdata": None, "tgkey": "fixture_tgkey", "pgdata": "fixture_pgdata"},
            "postgres_mounts": [pgdata_mount],
            "mode_source": mode_source,
        }

        def validate(containers: dict[str, object], target: dict[str, object]) -> None:
            blob_mode.validate_initial_local_transition(
                containers, containers, target, target, pathlib.Path("/srv/teagram-server"),
                "f" * 40, "9" * 40, "a" * 64, self.override,
            )

        for network in (
            DEFAULT_NETWORK,
            {"mode": "networks", "networks": ["edge", "teagram-server_default"]},
            {"mode": "host", "networks": []},
        ):
            for replica_first in (True, False):
                with self.subTest(replica_network=network, replica_first=replica_first):
                    containers = json.loads(json.dumps(baseline))
                    replica = json.loads(json.dumps(running))
                    replica.update(id="c" * 64, network=network)
                    containers["containers"].insert(0 if replica_first else 1, replica)
                    self.assert_rejects(
                        lambda: validate(containers, valid_target),
                        "baseline-inspection-invalid",
                    )

        cases = [
            ("guarded baseline", lambda c, t: c["containers"][0].update(mode_mounts=[target_mount]), "baseline-already-guarded"),
            ("writable target authority", lambda c, t: t["services"][0]["blob_mode_mounts"][0].update(read_only=False), "initial-render-mode-mount"),
            ("wrong target authority source", lambda c, t: t["services"][0]["blob_mode_mounts"][0].update(source="/tmp/other"), "initial-render-mode-mount"),
            ("wrong local volume", lambda c, t: t["services"][0]["tgblobs_mounts"][0].update(source="unexpected_tgblobs"), "initial-render-volume"),
            ("wrong pgdata target", lambda c, t: t["volumes"].update(pgdata="unexpected_pgdata") or t["postgres_mounts"][0].update(source="unexpected_pgdata"), "initial-render-pgdata-mount"),
            ("changed target exposure", lambda c, t: t["services"][0].update(ports=[{"target": "2443", "published": "2443", "host_ip": "0.0.0.0", "protocol": "tcp", "mode": "host"}]), "initial-render-exposure"),
            ("extra target network", lambda c, t: t["services"][0]["network"].update(networks=["edge", "teagram-server_default"]), "initial-render-network"),
            ("host network target", lambda c, t: t["services"][0].update(network={"mode": "host", "networks": []}), "initial-render-network"),
            ("missing target network", lambda c, t: t["services"][0].pop("network"), "initial-render-network"),
            ("changed key volume", lambda c, t: t["services"][0]["tgkey_mounts"][0].update(source="unexpected_tgkey"), "initial-render-key-mount"),
            ("changed application environment", lambda c, t: t["services"][0].update(tg_environment_sha256="f" * 64), "initial-render-environment"),
            ("unapproved replica count", lambda c, t: t["services"][0]["initial_local_guard_environment"].update(TG_REPLICA_COUNT="2"), "initial-render-guard-environment"),
            ("S3 baseline", lambda c, t: c["containers"][0].update(backend={"kind": "s3", "endpoint": "https://objects.invalid", "bucket": "fixture", "prefix": "fixture/"}), "initial-baseline-not-local"),
            ("conflicting target replica", lambda c, t: t["services"].append({"name": "teagramd-replica", "backend": backend, "blob_mode_mounts": [target_mount], "tgblobs_mounts": [local_mount]}), "running-service-not-rendered"),
        ]
        for name, mutate, reason in cases:
            with self.subTest(name=name):
                containers = json.loads(json.dumps(baseline))
                target = json.loads(json.dumps(valid_target))
                mutate(containers, target)
                with self.assertRaises(blob_mode.Reject) as caught:
                    validate(containers, target)
                self.assertEqual(str(caught.exception), reason)

    def test_initial_local_transition_rejects_render_change_after_preflight(self) -> None:
        backend = {"kind": "local", "dir": blob_mode.BLOB_TARGET}
        target_mount = {
            "type": "bind", "source": self.mode_source,
            "target": blob_mode.MODE_TARGET, "read_only": True,
        }
        local_mount = {
            "type": "volume", "source": self.volume,
            "target": blob_mode.BLOB_TARGET, "read_only": False,
        }
        baseline = {"containers": [{
            "id": "a" * 64, "service": "teagramd", "backend": backend,
            "mode_mounts": [],
            "tgblobs_mounts": [{
                "type": "volume", "name": self.volume,
                "target": blob_mode.BLOB_TARGET, "rw": True,
            }],
            "ports": [],
            "network": DEFAULT_NETWORK,
            "tgkey_mounts": [{"type": "volume", "source": "fixture_tgkey", "target": blob_mode.KEY_TARGET, "read_only": False}],
            "tg_environment_sha256": blob_mode.teagramd_environment_sha256({
                **blob_mode.IMAGE_ENV_DEFAULTS, "TG_BLOB_DIR": blob_mode.BLOB_TARGET,
            }),
        }, {
            "id": "b" * 64, "service": "postgres", "mode_mounts": [],
            "pgdata_mounts": [{"type": "volume", "source": "fixture_pgdata", "target": blob_mode.PGDATA_TARGET, "read_only": False}],
        }], "mode_source": self.mode_source}
        pgdata_mount = {"type": "volume", "source": "fixture_pgdata", "target": blob_mode.PGDATA_TARGET, "read_only": False}
        preflight_target = {
            "services": [{"name": "teagramd", "backend": backend,
                           "blob_mode_mounts": [target_mount], "tgblobs_mounts": [local_mount],
                           "ports": [],
                           "network": DEFAULT_NETWORK,
                           "tgkey_mounts": [{"type": "volume", "source": "fixture_tgkey", "target": blob_mode.KEY_TARGET, "read_only": False}],
                           "tg_environment_sha256": blob_mode.teagramd_environment_sha256({
                               **blob_mode.IMAGE_ENV_DEFAULTS, "TG_BLOB_DIR": blob_mode.BLOB_TARGET,
                           }),
                           "initial_local_guard_environment": blob_mode.INITIAL_LOCAL_GUARD_ENV}],
            "volumes": {"tgblobs": self.volume, "rustfsdata": None, "tgkey": "fixture_tgkey", "pgdata": "fixture_pgdata"},
            "postgres_mounts": [pgdata_mount],
            "mode_source": self.mode_source,
        }
        changed_target = json.loads(json.dumps(preflight_target))
        changed_target["services"][0]["blob_mode_mounts"][0]["source"] = "/tmp/other"
        with self.assertRaises(blob_mode.Reject) as caught:
            blob_mode.validate_initial_local_transition(
                baseline, baseline, preflight_target, changed_target,
                pathlib.Path("/srv/teagram-server"), "f" * 40, "9" * 40,
                "a" * 64, self.override,
            )
        self.assertEqual(str(caught.exception), "target-compose-changed")

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

    def test_deployment_runtime_validation_accepts_missing_reports(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path = self.create_initial_authority(root)
            args = SimpleNamespace(
                state_dir=state_dir,
                report_root=report_root,
                containers=root / "containers.json",
                compose=root / "compose.json",
                checkout=root,
                override=root / "docker-compose.override.yml",
                allow_unguarded_initial_local=False,
                allow_unguarded_initial_local_containers=False,
            )
            args.containers.write_text('{"containers":[]}', encoding="utf-8")
            args.containers.chmod(0o600)
            args.compose.write_text('{"services":[]}', encoding="utf-8")
            args.compose.chmod(0o600)
            with (
                patch.object(blob_mode, "require_runner_lock"),
                patch.object(blob_mode, "file_stat", side_effect=self.allow_unowned_private_files),
                patch.object(blob_mode.os, "fchown"),
                patch.object(blob_mode.os, "chown"),
            ):
                self.publish(state_dir, report_root, lock_path, root, "s3-accepted")
                shutil.rmtree(report_root)

                self.assert_rejects(
                    lambda: blob_mode.validate_runtime(args),
                    "state-unavailable",
                )
                with (
                    patch.object(blob_mode, "assert_containers_match"),
                    patch.object(blob_mode, "assert_compose_matches"),
                ):
                    record = blob_mode.validate_runtime(args, allow_missing_reports=True)

            self.assertEqual(record["outcome"], "s3-accepted")

    def test_deployment_authority_accepts_missing_reports_but_normal_reads_reject(self) -> None:
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
                records, _head, _mode_bytes = blob_mode.read_authority(state_dir, report_root)
                shutil.rmtree(report_root)
                report_root.mkdir(mode=0o700)

                record, missing_report_count = blob_mode.current_deployment_authority(state_dir, report_root)

                self.assertEqual(record["outcome"], "s3-accepted")
                self.assertEqual(missing_report_count, len(records))
                shutil.rmtree(report_root)
                record, missing_report_count = blob_mode.current_deployment_authority(state_dir, report_root)
                self.assertEqual(record["outcome"], "s3-accepted")
                self.assertEqual(missing_report_count, len(records))
                self.assert_rejects(lambda: blob_mode.read_authority(state_dir, report_root), "state-unavailable")
                self.assert_rejects(
                    lambda: blob_mode.publish_transition(SimpleNamespace(
                        outcome="s3-accepted",
                        proof=root / "unused-proof.json",
                        state_dir=state_dir,
                        report_root=report_root,
                        lock_path=lock_path,
                    )),
                    "state-unavailable",
                )

    def test_deployment_authority_rejects_present_mismatched_report(self) -> None:
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
                records, _head, _mode_bytes = blob_mode.read_authority(state_dir, report_root)
                report = blob_mode.report_path(report_root, records[-1]["transition_id"])
                report.write_bytes(b"{}")

                self.assert_rejects(
                    lambda: blob_mode.current_deployment_authority(state_dir, report_root),
                    "report-digest",
                )

    def test_deployment_authority_rejects_missing_journal(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, _lock_path = self.create_initial_authority(root)
            shutil.rmtree(state_dir / "journal")

            with patch.object(blob_mode, "file_stat", side_effect=self.allow_unowned_private_files):
                self.assert_rejects(
                    lambda: blob_mode.current_deployment_authority(state_dir, report_root),
                    "state-unavailable",
                )

    def test_simple_s3_transition_accepts_current_migrations_and_one_verified_copy(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path = self.create_initial_authority(root)
            proof_path = self.simple_s3_proof_file(root, report_root)
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
                blob_mode.publish_transition(args)
                records, head, mode_bytes = blob_mode.read_authority(state_dir, report_root)
            self.assertEqual(records[-1]["outcome"], "s3-accepted")
            self.assertEqual(records[-1]["evidence"]["copy_passes"], 1)
            self.assertEqual(head, mode_bytes)

    def test_encrypted_authority_accepts_only_a_verified_inventory_after_s3(self) -> None:
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
                records, _, _ = blob_mode.read_authority(state_dir, report_root)
                previous = records[-1]
                transition_id = str(blob_mode.uuid.uuid4())
                inventory = {
                    "schema": blob_mode.ENCRYPTED_INVENTORY_SCHEMA,
                    "encrypted": {"classes": {
                        "valid_envelope": {"count": 3, "logical_bytes": 4096},
                        "no_valid_envelope": {"count": 0},
                        "missing": {"count": 0},
                    }},
                    "plaintext": {"classes": {}},
                }
                inventory_bytes = json.dumps(inventory, sort_keys=True, separators=(",", ":")).encode("utf-8")
                inventory_sha = blob_mode.hashlib.sha256(inventory_bytes).hexdigest()
                inventory_name = f"teagramd-blob-encryption-inventory-{transition_id}.json"
                inventory_path = report_root / inventory_name
                inventory_path.write_bytes(inventory_bytes)
                inventory_path.chmod(0o600)
                report_evidence = {
                    "encrypted_inventory_sha256": inventory_sha,
                    "object_count": 3,
                    "byte_total": 4096,
                    "key_version": 4,
                    "writes": "drained",
                }
                report = {
                    "schema": blob_mode.ENCRYPTED_REPORT_SCHEMA,
                    "generation": 3,
                    "transition_id": transition_id,
                    "outcome": "encrypted",
                    "backend": previous["backend"],
                    "volumes": previous["volumes"],
                    "evidence": report_evidence,
                    "inventory_report": inventory_name,
                }
                report_bytes = json.dumps(report, sort_keys=True, separators=(",", ":")).encode("utf-8")
                report_path = blob_mode.report_path(report_root, transition_id)
                report_path.write_bytes(report_bytes)
                report_path.chmod(0o600)
                record = {
                    "schema": blob_mode.SCHEMA,
                    "generation": 3,
                    "transition_id": transition_id,
                    "supersedes": previous["transition_id"],
                    "outcome": "encrypted",
                    "backend": previous["backend"],
                    "volumes": previous["volumes"],
                    "evidence": {
                        **report_evidence,
                        "report_sha256": blob_mode.hashlib.sha256(report_bytes).hexdigest(),
                    },
                    "published_at": "2026-10-24T00:00:00Z",
                }
                valid = blob_mode.valid_outcome(record, 3, previous["transition_id"], "s3-accepted")
                blob_mode.validate_report(report_root, valid, previous)
                bad_inventory = dict(inventory)
                bad_inventory["encrypted"] = {"classes": {
                    "valid_envelope": {"count": 2, "logical_bytes": 4096},
                    "no_valid_envelope": {"count": 0},
                    "missing": {"count": 0},
                }}
                inventory_path.write_text(json.dumps(bad_inventory), encoding="utf-8")
                with self.assertRaises(blob_mode.Reject):
                    blob_mode.validate_report(report_root, valid, previous)
                inventory_path.write_bytes(inventory_bytes)
                self.assertTrue(blob_mode.expected_mount_read_only(valid))
                mode_source = os.path.realpath(state_dir)
                runtime = {
                    "id": "c" * 64,
                    "service": "teagramd",
                    "backend": valid["backend"],
                    "mode_mounts": blob_mode.guarded_mode_mounts("teagramd", mode_source),
                    "tgblobs_mounts": [{
                        "type": "volume", "name": valid["volumes"]["tgblobs"],
                        "target": blob_mode.BLOB_TARGET, "rw": False,
                    }],
                    "network": {
                        "mode": "networks",
                        "networks": ["teagram_default", blob_mode.rustfs_private_network_name(valid)],
                    },
                }
                rustfs = {
                    "id": "d" * 64,
                    "service": "rustfs",
                    "ports": [],
                    "network": {
                        "mode": "networks",
                        "networks": [blob_mode.rustfs_private_network_name(valid)],
                    },
                    "rustfs_mounts": [{
                        "type": "volume", "name": valid["volumes"]["rustfsdata"],
                        "target": "/data", "rw": True,
                    }],
                }
                with patch.object(blob_mode, "docker_volume_exists"):
                    blob_mode.assert_containers_match(
                        {"containers": [runtime, rustfs]}, valid, mode_source
                    )
                record_bytes = json.dumps(record, sort_keys=True, separators=(",", ":")).encode("utf-8")
                blob_mode.atomic_publish_state(state_dir, record_bytes, transition_id, 3)
                final, head, mode_bytes = blob_mode.read_authority(state_dir, report_root)
            self.assertEqual(final[-1]["outcome"], "encrypted")
            self.assertEqual(final[-1]["evidence"]["key_version"], 4)
            self.assertEqual(head, mode_bytes)

    def test_simple_s3_transition_rejects_published_rustfs_port(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path = self.create_initial_authority(root)
            proof_path = self.simple_s3_proof_file(root, report_root, exposed=True)
            args = SimpleNamespace(
                outcome="s3-accepted", proof=proof_path, state_dir=state_dir,
                report_root=report_root, lock_path=lock_path,
            )
            state_before = {
                path.relative_to(state_dir): path.read_bytes()
                for path in state_dir.rglob("*") if path.is_file()
            }
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

    def test_simple_s3_transition_rejects_external_rustfs_network(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path = self.create_initial_authority(root)
            proof_path = self.simple_s3_proof_file(root, report_root, external_network=True)
            args = SimpleNamespace(
                outcome="s3-accepted", proof=proof_path, state_dir=state_dir,
                report_root=report_root, lock_path=lock_path,
            )
            state_before = {
                path.relative_to(state_dir): path.read_bytes()
                for path in state_dir.rglob("*") if path.is_file()
            }
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

    def test_simple_s3_transition_rejects_object_manifest_mismatch(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path = self.create_initial_authority(root)
            proof_path = self.simple_s3_proof_file(root, report_root, mismatched_destination=True)
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

    def test_r70_recovery_publisher_rejects_nonempty_files(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path = self.create_initial_authority(root)
            with (
                patch.object(blob_mode, "require_runner_lock"),
                patch.object(blob_mode, "file_stat", side_effect=self.allow_unowned_private_files),
                patch.object(blob_mode.os, "fchown"),
                patch.object(blob_mode.os, "chown"),
            ):
                self.publish(state_dir, report_root, lock_path, root, "s3-accepted", "60-70")
                proof = self.transition_proof("recovered-local")
                self.materialize_transition_proof(proof, report_root, "60-70")
                references_name = "recovery_reference_rows_sha256"
                references_path = report_root / proof["phase_files"][references_name]
                references_path.write_bytes(b"file\ttrue\t01/1\n")
                references_path.chmod(0o600)
                proof["phase_digests"][references_name] = blob_mode.hashlib.sha256(
                    references_path.read_bytes()
                ).hexdigest()
                proof_path = root / "recovered-local-r70.json"
                proof_path.write_text(json.dumps(proof), encoding="utf-8")
                proof_path.chmod(0o600)
                args = SimpleNamespace(
                    outcome="recovered-local",
                    proof=proof_path,
                    state_dir=state_dir,
                    report_root=report_root,
                    lock_path=lock_path,
                )

                self.assert_rejects(
                    lambda: blob_mode.publish_transition(args), "transition-reference-coverage"
                )
                records, head, mode_bytes = blob_mode.read_authority(state_dir, report_root)
                self.assertEqual(len(records), 2)
                self.assertEqual(records[-1]["outcome"], "s3-accepted")
                self.assertEqual(head, mode_bytes)

    def test_r70_recovery_publisher_accepts_empty_files(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            state_dir, report_root, lock_path = self.create_initial_authority(root)
            with (
                patch.object(blob_mode, "require_runner_lock"),
                patch.object(blob_mode, "file_stat", side_effect=self.allow_unowned_private_files),
                patch.object(blob_mode.os, "fchown"),
                patch.object(blob_mode.os, "chown"),
            ):
                self.publish(state_dir, report_root, lock_path, root, "s3-accepted", "60-70")
                self.publish(state_dir, report_root, lock_path, root, "recovered-local", "60-70")
                records, head, mode_bytes = blob_mode.read_authority(state_dir, report_root)
                self.assertEqual(len(records), 3)
                self.assertEqual(records[-1]["outcome"], "recovered-local")
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
            interrupted_record = json.loads(journal_head)
            report = blob_mode.report_path(report_root, interrupted_record["transition_id"])
            legacy_report = report_root / (
                f"telegramd-blob-mode-report-{interrupted_record['transition_id']}.json"
            )
            report.rename(legacy_report)

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
