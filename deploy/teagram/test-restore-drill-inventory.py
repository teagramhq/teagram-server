#!/usr/bin/env python3
from __future__ import annotations

import datetime as dt
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from deploy.teagram.ops.restore_drill_inventory import metadata_inventory, size_class


UTC = dt.timezone.utc


class RestoreDrillInventoryTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def add_dump(
        self,
        name: str,
        *,
        size: int = 3,
        mtime: dt.datetime = dt.datetime(2026, 10, 28, 0, 0, tzinfo=UTC),
        mode: int = 0o600,
    ) -> Path:
        path = self.root / name
        path.write_bytes(b"x" * size)
        path.chmod(mode)
        timestamp = mtime.timestamp()
        os.utime(path, (timestamp, timestamp))
        return path

    def inventory(self, *, now: dt.datetime | None = None) -> dict[str, object]:
        with patch(
            "deploy.teagram.ops.restore_drill_inventory.safe_dump_directory",
            return_value=True,
        ):
            return metadata_inventory(
                self.root,
                expected_uid=os.getuid(),
                now_epoch=int((now or dt.datetime(2026, 10, 28, 4, 0, tzinfo=UTC)).timestamp()),
            )

    def test_selects_by_filename_timestamp_then_mtime(self) -> None:
        newer_by_name = self.add_dump(
            "pgdump-pre-aaaaaaa-20261028T020000Z.sql",
            mtime=dt.datetime(2026, 10, 28, 1, 0, tzinfo=UTC),
        )
        self.add_dump(
            "pgdump-pre-bbbbbbb-20261028T010000Z.sql",
            mtime=dt.datetime(2026, 10, 28, 3, 0, tzinfo=UTC),
        )

        result = self.inventory()

        self.assertEqual(result["selected"]["name"], newer_by_name.name)
        self.assertTrue(result["selected"]["metadata_ok"])
        self.assertEqual(result["selected"]["size_class"], "0-1MiB")

    def test_uses_mtime_to_break_equal_filename_timestamps(self) -> None:
        self.add_dump(
            "pgdump-pre-aaaaaaa-20261028T020000Z.sql",
            mtime=dt.datetime(2026, 10, 28, 1, 0, tzinfo=UTC),
        )
        newest = self.add_dump(
            "pgdump-pre-bbbbbbb-20261028T020000Z.sql",
            mtime=dt.datetime(2026, 10, 28, 2, 0, tzinfo=UTC),
        )

        result = self.inventory()

        self.assertEqual(result["selected"]["name"], newest.name)

    def test_newest_invalid_dump_is_refused_without_fallback(self) -> None:
        self.add_dump("pgdump-pre-aaaaaaa-20261028T010000Z.sql")
        self.add_dump(
            "pgdump-pre-bbbbbbb-20261028T020000Z.sql",
            mode=0o644,
        )

        result = self.inventory()

        self.assertEqual(
            result["selected"]["name"], "pgdump-pre-bbbbbbb-20261028T020000Z.sql"
        )
        self.assertFalse(result["selected"]["metadata_ok"])
        self.assertEqual(result["selected"]["refusal_reasons"], ["mode"])

    def test_symlinked_newest_dump_is_refused(self) -> None:
        self.add_dump("pgdump-pre-aaaaaaa-20261028T010000Z.sql")
        (self.root / "pgdump-pre-bbbbbbb-20261028T020000Z.sql").symlink_to(
            "pgdump-pre-aaaaaaa-20261028T010000Z.sql"
        )

        result = self.inventory()

        self.assertIn("regular_file", result["selected"]["refusal_reasons"])

    def test_inventory_marks_prune_candidates_after_ten(self) -> None:
        for day in range(1, 13):
            self.add_dump(f"pgdump-pre-aaaaaaa-202610{day:02d}T000000Z.sql")

        rows = self.inventory()["inventory"]

        self.assertEqual(len(rows), 12)
        self.assertEqual(sum(row["prune_if_keep_newest_10"] for row in rows), 2)
        self.assertFalse(rows[0]["prune_if_keep_newest_10"])
        self.assertTrue(rows[-1]["prune_if_keep_newest_10"])

    def test_untimestamped_runner_dump_is_not_a_deploy_input(self) -> None:
        self.add_dump("pgdump-pre-aaaaaaa.sql")

        result = self.inventory()

        self.assertIsNone(result["selected"])

    def test_size_classes(self) -> None:
        mib = 1024 * 1024
        self.assertEqual(size_class(0), "0-1MiB")
        self.assertEqual(size_class(mib), "0-1MiB")
        self.assertEqual(size_class(3 * mib), "1-4MiB")
        self.assertEqual(size_class(512 * mib + 1), "over-512MiB")


if __name__ == "__main__":
    unittest.main()
