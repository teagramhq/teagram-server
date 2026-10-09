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

    def test_local_inventory_preserves_exact_backend_and_volume_names(self) -> None:
        inventory = self.local_compose()
        blob_mode.assert_compose_matches(inventory, self.record, self.mode_source, self.override)
        with patch.object(blob_mode, "docker_volume_exists"):
            blob_mode.assert_containers_match(self.local_containers(), self.record, self.mode_source)

    def test_initial_local_inventory_compares_live_and_target_mounts_exposure_and_environment(self) -> None:
        mode_mount = {
            "type": "bind", "source": self.mode_source,
            "target": blob_mode.MODE_TARGET, "read_only": True,
        }
        desired_environment = {
            **blob_mode.IMAGE_ENV_DEFAULTS,
            "TG_BLOB_DIR": blob_mode.BLOB_TARGET,
            "TG_SYNTHETIC_FLAG": "fixture",
            **blob_mode.INITIAL_LOCAL_GUARD_ENV,
        }
        compose = {
            "services": {
                "telegramd": {
                    "environment": {
                        "TG_BLOB_DIR": blob_mode.BLOB_TARGET,
                        "TG_SYNTHETIC_FLAG": "fixture",
                        **blob_mode.INITIAL_LOCAL_GUARD_ENV,
                    },
                    "ports": [{
                        "target": 2443, "published": "2443", "host_ip": "127.0.0.1",
                        "protocol": "tcp", "mode": "host",
                    }],
                    "volumes": [
                        {"type": "volume", "source": "tgkey", "target": blob_mode.KEY_TARGET, "read_only": False},
                        {"type": "volume", "source": "tgblobs", "target": blob_mode.BLOB_TARGET, "read_only": False},
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
        }
        rendered = blob_mode.compose_inventory(compose, pathlib.Path("/srv/telegram-server"))
        self.assertEqual(rendered["services"][0]["tg_environment_sha256"], blob_mode.telegramd_environment_sha256(desired_environment))
        inspected = [{
            "Id": "a" * 64,
            "Config": {
                "Env": [f"{key}={value}" for key, value in desired_environment.items() if key not in blob_mode.INITIAL_LOCAL_GUARD_ENV],
                "Labels": {"com.docker.compose.service": "telegramd"},
            },
            "State": {"Status": "running"},
            "HostConfig": {"PortBindings": {"2443/tcp": [{"HostIp": "127.0.0.1", "HostPort": "2443"}]}},
            "Mounts": [
                {"Type": "volume", "Name": "fixture_tgkey", "Source": "/synthetic/key", "Destination": blob_mode.KEY_TARGET, "RW": True},
                {"Type": "volume", "Name": self.volume, "Source": "/synthetic/blobs", "Destination": blob_mode.BLOB_TARGET, "RW": True},
            ],
        }, {
            "Id": "b" * 64,
            "Config": {"Env": [], "Labels": {"com.docker.compose.service": "postgres"}},
            "State": {"Status": "running"},
            "HostConfig": {"PortBindings": {}},
            "Mounts": [{"Type": "volume", "Name": "fixture_pgdata", "Source": "/synthetic/pgdata", "Destination": blob_mode.PGDATA_TARGET, "RW": True}],
        }]
        baseline = blob_mode.container_inventory(inspected, pathlib.Path("/srv/telegram-server"))
        self.assertEqual(baseline["containers"][0]["pgdata_mounts"], rendered["postgres_mounts"])
        with patch.object(blob_mode, "docker_volume_exists"):
            blob_mode.assert_compose_matches_initial(
                baseline, rendered, {"kind": "local", "dir": blob_mode.BLOB_TARGET},
                self.volume, self.mode_source,
            )

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
                    "name": "telegramd", "backend": backend,
                    "blob_mode_mounts": [mode_mount], "tgblobs_mounts": [local_mount],
                    "ports": [],
                    "tgkey_mounts": [{"type": "volume", "source": "fixture_tgkey", "target": blob_mode.KEY_TARGET, "read_only": False}],
                    "tg_environment_sha256": blob_mode.telegramd_environment_sha256({
                        **blob_mode.IMAGE_ENV_DEFAULTS, "TG_BLOB_DIR": blob_mode.BLOB_TARGET,
                    }),
                    "initial_local_guard_environment": blob_mode.INITIAL_LOCAL_GUARD_ENV,
                }],
                "volumes": {"tgblobs": self.volume, "rustfsdata": None, "tgkey": "fixture_tgkey", "pgdata": "fixture_pgdata"},
                "postgres_mounts": [{"type": "volume", "source": "fixture_pgdata", "target": blob_mode.PGDATA_TARGET, "read_only": False}],
                "mode_source": mode_source,
            }
            container = {
                "id": "a" * 64, "service": "telegramd", "backend": backend,
                "mode_mounts": [],
                "tgblobs_mounts": [{
                    "type": "volume", "name": self.volume,
                    "target": blob_mode.BLOB_TARGET, "rw": True,
                }],
                "ports": [],
                "tgkey_mounts": [{"type": "volume", "source": "fixture_tgkey", "target": blob_mode.KEY_TARGET, "read_only": False}],
                "tg_environment_sha256": blob_mode.telegramd_environment_sha256({
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
                blob_mode.validate_report(report_root, record)
                report_path = blob_mode.report_path(report_root, record["transition_id"])
                report = json.loads(report_path.read_text(encoding="utf-8"))

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
            "id": "a" * 64, "service": "telegramd", "backend": backend,
            "mode_mounts": [],
            "tgblobs_mounts": [{
                "type": "volume", "name": self.volume,
                "target": blob_mode.BLOB_TARGET, "rw": True,
            }],
            "ports": [],
            "tgkey_mounts": [{"type": "volume", "source": "fixture_tgkey", "target": blob_mode.KEY_TARGET, "read_only": False}],
            "tg_environment_sha256": blob_mode.telegramd_environment_sha256({
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
                "name": "telegramd", "backend": backend,
                "blob_mode_mounts": [target_mount], "tgblobs_mounts": [local_mount],
                "ports": [],
                "tgkey_mounts": [{"type": "volume", "source": "fixture_tgkey", "target": blob_mode.KEY_TARGET, "read_only": False}],
                "tg_environment_sha256": blob_mode.telegramd_environment_sha256({
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
                containers, containers, target, target, pathlib.Path("/srv/telegram-server"),
                "f" * 40, "9" * 40, "a" * 64, self.override,
            )

        cases = [
            ("guarded baseline", lambda c, t: c["containers"][0].update(mode_mounts=[target_mount]), "baseline-already-guarded"),
            ("writable target authority", lambda c, t: t["services"][0]["blob_mode_mounts"][0].update(read_only=False), "initial-render-mode-mount"),
            ("wrong target authority source", lambda c, t: t["services"][0]["blob_mode_mounts"][0].update(source="/tmp/other"), "initial-render-mode-mount"),
            ("wrong local volume", lambda c, t: t["services"][0]["tgblobs_mounts"][0].update(source="unexpected_tgblobs"), "initial-render-volume"),
            ("wrong pgdata target", lambda c, t: t["volumes"].update(pgdata="unexpected_pgdata") or t["postgres_mounts"][0].update(source="unexpected_pgdata"), "initial-render-pgdata-mount"),
            ("changed target exposure", lambda c, t: t["services"][0].update(ports=[{"target": "2443", "published": "2443", "host_ip": "0.0.0.0", "protocol": "tcp", "mode": "host"}]), "initial-render-exposure"),
            ("changed key volume", lambda c, t: t["services"][0]["tgkey_mounts"][0].update(source="unexpected_tgkey"), "initial-render-key-mount"),
            ("changed application environment", lambda c, t: t["services"][0].update(tg_environment_sha256="f" * 64), "initial-render-environment"),
            ("unapproved replica count", lambda c, t: t["services"][0]["initial_local_guard_environment"].update(TG_REPLICA_COUNT="2"), "initial-render-guard-environment"),
            ("S3 baseline", lambda c, t: c["containers"][0].update(backend={"kind": "s3", "endpoint": "https://objects.invalid", "bucket": "fixture", "prefix": "fixture/"}), "initial-baseline-not-local"),
            ("conflicting target replica", lambda c, t: t["services"].append({"name": "telegramd-replica", "backend": backend, "blob_mode_mounts": [target_mount], "tgblobs_mounts": [local_mount]}), "running-service-not-rendered"),
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
            "id": "a" * 64, "service": "telegramd", "backend": backend,
            "mode_mounts": [],
            "tgblobs_mounts": [{
                "type": "volume", "name": self.volume,
                "target": blob_mode.BLOB_TARGET, "rw": True,
            }],
            "ports": [],
            "tgkey_mounts": [{"type": "volume", "source": "fixture_tgkey", "target": blob_mode.KEY_TARGET, "read_only": False}],
            "tg_environment_sha256": blob_mode.telegramd_environment_sha256({
                **blob_mode.IMAGE_ENV_DEFAULTS, "TG_BLOB_DIR": blob_mode.BLOB_TARGET,
            }),
        }, {
            "id": "b" * 64, "service": "postgres", "mode_mounts": [],
            "pgdata_mounts": [{"type": "volume", "source": "fixture_pgdata", "target": blob_mode.PGDATA_TARGET, "read_only": False}],
        }], "mode_source": self.mode_source}
        pgdata_mount = {"type": "volume", "source": "fixture_pgdata", "target": blob_mode.PGDATA_TARGET, "read_only": False}
        preflight_target = {
            "services": [{"name": "telegramd", "backend": backend,
                           "blob_mode_mounts": [target_mount], "tgblobs_mounts": [local_mount],
                           "ports": [],
                           "tgkey_mounts": [{"type": "volume", "source": "fixture_tgkey", "target": blob_mode.KEY_TARGET, "read_only": False}],
                           "tg_environment_sha256": blob_mode.telegramd_environment_sha256({
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
                pathlib.Path("/srv/telegram-server"), "f" * 40, "9" * 40,
                "a" * 64, self.override,
            )
        self.assertEqual(str(caught.exception), "target-compose-changed")


if __name__ == "__main__":
    unittest.main()
