#!/usr/bin/env python3
"""Generate a move journal with the production Python authority publisher."""

from __future__ import annotations

import importlib.util
import pathlib
import sys
from types import SimpleNamespace
from unittest.mock import patch


repo_root = pathlib.Path(sys.argv[1]).resolve()
fixture_root = pathlib.Path(sys.argv[2]).resolve()
fixture_path = repo_root / "deploy/teagramd/rollout-runner/test-blob-mode-state.py"
spec = importlib.util.spec_from_file_location("blob_mode_state_test_fixtures", fixture_path)
if spec is None or spec.loader is None:
    raise RuntimeError("could not load blob-mode-state test fixtures")
fixtures = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixtures)
fixture = fixtures.BlobModeStateTests("test_local_inventory_preserves_exact_backend_and_volume_names")
fixture.setUp()
fixture.volume = "telegram-server_tgblobs"
blob_mode = fixtures.blob_mode

legacy_blob_dir = "/var/lib/telegramd-blobs"
layouts = dict(blob_mode.RUNTIME_LAYOUTS)
layouts["teagramd"] = {**layouts["teagramd"], "blobs": legacy_blob_dir}
with (
    patch.object(blob_mode, "BLOB_TARGET", legacy_blob_dir),
    patch.object(blob_mode, "BLOB_TARGETS", {legacy_blob_dir}),
    patch.object(blob_mode, "RUNTIME_LAYOUTS", layouts),
):
    state_dir, report_root, lock_path = fixture.create_initial_authority(fixture_root)

inventory_paths: dict[str, dict[str, pathlib.Path]] = {"source": {}, "target": {}}
for logical in ("tgblobs", "rustfsdata"):
    inventory_line = blob_mode.hashlib.sha256(f"moved {logical}".encode()).hexdigest()
    inventory = f"{inventory_line}  ./fixture.bin\n".encode()
    for side in ("source", "target"):
        path = fixture_root / f"{logical}.{side}.sha256"
        path.write_bytes(inventory)
        path.chmod(0o600)
        inventory_paths[side][logical] = path

with (
    patch.object(blob_mode, "require_runner_lock"),
    patch.object(blob_mode, "file_stat", side_effect=fixture.allow_unowned_private_files),
    patch.object(blob_mode.os, "fchown"),
    patch.object(blob_mode.os, "chown"),
):
    blob_mode.publish_local_stack_move(
        SimpleNamespace(
            state_dir=state_dir,
            report_root=report_root,
            lock_path=lock_path,
            source_inventories=inventory_paths["source"],
            target_inventories=inventory_paths["target"],
        )
    )
