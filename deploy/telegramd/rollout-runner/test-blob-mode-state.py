#!/usr/bin/env python3
"""Credential-free unit checks for rollout blob-mode inventory gates."""

from __future__ import annotations

import importlib.util
import os
import pathlib
import tempfile
import unittest
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


if __name__ == "__main__":
    unittest.main()
