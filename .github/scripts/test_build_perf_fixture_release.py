#!/usr/bin/env python3
"""Exercise performance fixture release asset staging and manifest checksums."""

from __future__ import annotations

import contextlib
import hashlib
import importlib.util
import io
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / ".github/scripts/build-perf-fixture-release.py"
SPEC = importlib.util.spec_from_file_location("build_perf_fixture_release", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
builder = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(builder)


class BuildPerfFixtureReleaseTests(unittest.TestCase):
    def setUp(self) -> None:
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.snapshot = self.root / "snapshot"
        self.snapshot.mkdir()
        self.metadata = self.root / "metadata.json"
        self.output = self.root / "assets"
        self.write_metadata()

    def write_metadata(self, **updates: str) -> None:
        metadata = {
            "fixture": "fixture-sha",
            "server_commit": "0123456789abcdef0123456789abcdef01234567",
            "migration_revision": "20261026000001",
        }
        metadata.update(updates)
        self.metadata.write_text(json.dumps(metadata), encoding="utf-8")

    def run_builder(self) -> tuple[int, str]:
        stderr = io.StringIO()
        argv = [
            str(SCRIPT),
            "--snapshot",
            str(self.snapshot),
            "--metadata",
            str(self.metadata),
            "--output",
            str(self.output),
        ]
        with patch.object(sys, "argv", argv), contextlib.redirect_stderr(stderr):
            status = builder.main()
        return status, stderr.getvalue()

    def test_manifest_inventories_nested_assets_and_checksums(self) -> None:
        expected_content = {
            "perf_1x.dump": b"database snapshot",
            "nested__perf_100x.dump": b"larger database snapshot",
        }
        for name, content in (
            ("perf_1x.dump", expected_content["perf_1x.dump"]),
            ("nested/perf_100x.dump", expected_content["nested__perf_100x.dump"]),
        ):
            path = self.snapshot / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(content)

        status, error = self.run_builder()

        self.assertEqual(status, 0, error)
        manifest = json.loads((self.output / "manifest.json").read_text())
        self.assertEqual(manifest["fixture"], "fixture-sha")
        self.assertEqual(
            manifest["server_commit"],
            "0123456789abcdef0123456789abcdef01234567",
        )
        self.assertEqual(manifest["migration_revision"], "20261026000001")
        self.assertEqual(
            [asset["name"] for asset in manifest["assets"]],
            ["nested__perf_100x.dump", "perf_1x.dump"],
        )
        for asset in manifest["assets"]:
            content = (self.output / asset["name"]).read_bytes()
            self.assertEqual(asset["sha256"], hashlib.sha256(content).hexdigest())
            self.assertEqual(content, expected_content[asset["name"]])

    def test_seed_metadata_sidecar_is_not_published_as_snapshot_asset(self) -> None:
        (self.snapshot / "perf_1x.dump").write_bytes(b"database snapshot")
        self.metadata = self.snapshot / ".fixture-metadata.json"
        self.write_metadata()

        status, error = self.run_builder()

        self.assertEqual(status, 0, error)
        manifest = json.loads((self.output / "manifest.json").read_text())
        self.assertEqual([asset["name"] for asset in manifest["assets"]], ["perf_1x.dump"])

    def test_large_asset_is_chunked_without_losing_bytes(self) -> None:
        content = b"0123456789"
        (self.snapshot / "large.dump").write_bytes(content)

        with patch.object(builder, "MAX_ASSET_BYTES", 4), patch.object(
            builder, "CHUNK_BYTES", 2
        ):
            status, error = self.run_builder()

        self.assertEqual(status, 0, error)
        manifest = json.loads((self.output / "manifest.json").read_text())
        self.assertEqual(
            [asset["name"] for asset in manifest["assets"]],
            ["large.dump.part000", "large.dump.part001", "large.dump.part002"],
        )
        parts = []
        for asset in manifest["assets"]:
            part = (self.output / asset["name"]).read_bytes()
            self.assertLessEqual(len(part), 4)
            self.assertEqual(asset["sha256"], hashlib.sha256(part).hexdigest())
            parts.append(part)
        self.assertEqual(b"".join(parts), content)

    def test_flattened_asset_name_collision_is_rejected(self) -> None:
        (self.snapshot / "a").mkdir()
        (self.snapshot / "a" / "b.dump").write_bytes(b"nested")
        (self.snapshot / "a__b.dump").write_bytes(b"flat")

        status, error = self.run_builder()

        self.assertEqual(status, 1)
        self.assertIn("asset name collision: a__b.dump", error)

    def test_manifest_filename_cannot_be_overwritten_by_snapshot_asset(self) -> None:
        (self.snapshot / "manifest.json").write_bytes(b"not the release manifest")

        status, error = self.run_builder()

        self.assertEqual(status, 1)
        self.assertIn("asset name collision: manifest.json", error)

    def test_symlinked_snapshot_content_is_rejected(self) -> None:
        target = self.root / "outside.dump"
        target.write_bytes(b"outside snapshot")
        (self.snapshot / "linked.dump").symlink_to(target)

        status, error = self.run_builder()

        self.assertEqual(status, 1)
        self.assertIn("snapshot contains a symlink", error)

    def test_symlinked_seed_metadata_is_rejected(self) -> None:
        target = self.root / "outside-metadata.json"
        target.write_text("{}", encoding="utf-8")
        self.metadata = self.snapshot / ".fixture-metadata.json"
        self.metadata.symlink_to(target)

        status, error = self.run_builder()

        self.assertEqual(status, 1)
        self.assertIn("metadata file is a symlink", error)

    def test_missing_seed_commit_is_rejected(self) -> None:
        (self.snapshot / "perf_1x.dump").write_bytes(b"snapshot")
        self.write_metadata(server_commit="")

        status, error = self.run_builder()

        self.assertEqual(status, 1)
        self.assertIn("server_commit", error)
        self.assertIn("must be non-empty", error)


if __name__ == "__main__":
    unittest.main()
