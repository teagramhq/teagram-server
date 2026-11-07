#!/usr/bin/env python3
"""Fixtures for the unaccounted media quarantine operation."""

from __future__ import annotations

import datetime as dt
import hashlib
import importlib.util
import json
import pathlib
import sys
import tempfile
import unittest


HELPER_PATH = pathlib.Path(__file__).with_name("ops") / "quarantine_unaccounted_media.py"
SPEC = importlib.util.spec_from_file_location("quarantine_unaccounted_media", HELPER_PATH)
assert SPEC is not None and SPEC.loader is not None
MEDIA = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = MEDIA
SPEC.loader.exec_module(MEDIA)


PREFIX = "teagramd/"
NOW = dt.datetime(2026, 11, 7, tzinfo=dt.timezone.utc)
REFERENCE = MEDIA.Reference("file", "1", "01/1", 5)
REFERENCE_BYTES = b"referenced"
CANDIDATE_BYTES = {
    "02/2": b"plain-file",
    "parts/aa/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": b"plain-part",
}
MODIFIED = "2026-09-01T00:00:00Z"


def digest_references(references: list[MEDIA.Reference]) -> str:
    digest = hashlib.sha256()
    for reference in sorted(references, key=lambda item: item.key):
        digest.update(
            f"{reference.kind}\t{reference.object_id}\t{reference.key}\t{reference.logical_bytes}\n".encode()
        )
    return digest.hexdigest()


class FakeStorage:
    def __init__(self, objects: dict[str, bytes], quarantine_dir: pathlib.Path, mismatch_key: str | None = None):
        self.objects = objects
        self.quarantine_dir = quarantine_dir
        self.mismatch_key = mismatch_key
        self.removed: list[str] = []

    def copy_from(self, key: str, destination: pathlib.Path) -> None:
        content = self.objects[key]
        if key == self.mismatch_key:
            content += b"corrupt"
        destination.write_bytes(content)

    def copy_to(self, source: pathlib.Path, key: str) -> None:
        self.objects[key] = source.read_bytes()

    def hash_remote(self, key: str) -> str:
        return hashlib.sha256(self.objects[key]).hexdigest()

    def remove(self, key: str) -> None:
        # Deletion is reached only after every copy and manifest entry exists.
        manifest = json.loads((self.quarantine_dir / "manifest.json").read_text())
        self.assert_manifest_complete_before_delete(manifest)
        self.removed.append(key)
        del self.objects[key]

    @staticmethod
    def assert_manifest_complete_before_delete(manifest: dict[str, object]) -> None:
        assert manifest["status"] == "deleting"
        records = manifest["objects"]
        assert isinstance(records, list) and len(records) == len(CANDIDATE_BYTES)
        assert all(record["sha256"] for record in records)


class QuarantineFixtures(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temp.name)
        self.inventory_path = self.root / "inventory.json"
        self.journal_path = self.root / "progress.jsonl"
        self.references_path = self.root / "references.tsv"
        self.qbase = self.root / "quarantine"
        self.qbase.mkdir(mode=0o700)
        self.run_id = "20261107T120000Z-1234-abcdef0"
        self.references = [REFERENCE]
        self.report = self.make_report()
        self.write_inventory()
        self.write_references_and_journal()

    def tearDown(self) -> None:
        self.temp.cleanup()

    def make_report(self) -> dict[str, object]:
        candidates = [
            {"kind": "file", "id": "2", "key": "02/2"},
            {"kind": "part", "id": "parts/aa/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "key": "parts/aa/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
        ]
        physical_bytes = sum(len(CANDIDATE_BYTES[item["key"]]) for item in candidates)
        return {
            "schema": MEDIA.INVENTORY_SCHEMA,
            "encrypted": {
                "classes": {
                    "valid_envelope": {"count": 1},
                    "no_valid_envelope": {"count": 0},
                    "missing": {"count": 0},
                    "unaccounted": {"count": 0},
                }
            },
            "plaintext": {
                "classes": {
                    "unaccounted": {"count": len(candidates), "physical_bytes": physical_bytes, "ids": candidates}
                }
            },
        }

    def write_inventory(self) -> str:
        self.inventory_path.write_text(json.dumps(self.report, separators=(",", ":")))
        self.inventory_path.chmod(0o600)
        return hashlib.sha256(self.inventory_path.read_bytes()).hexdigest()

    def write_references_and_journal(self) -> None:
        refs = sorted(self.references, key=lambda item: item.key)
        self.references_path.write_text("".join(
            f"{ref.kind}\t{ref.object_id}\t{ref.key}\t{ref.logical_bytes}\n" for ref in refs
        ))
        self.references_path.chmod(0o600)
        reference_sha = digest_references(refs)
        self.journal_path.write_text(
            json.dumps({"type": "header", "schema": MEDIA.PROGRESS_SCHEMA, "writes": "stopped", "references_sha256": reference_sha})
            + "\n"
            + json.dumps({"type": "object", "key": REFERENCE.key})
            + "\n"
        )
        self.journal_path.chmod(0o600)

    def inventory_sha(self) -> str:
        return hashlib.sha256(self.inventory_path.read_bytes()).hexdigest()

    def write_listing(
        self,
        objects: dict[str, bytes],
        listing_path: pathlib.Path,
        references: list[MEDIA.Reference],
    ) -> None:
        rows = [("encrypted/" + reference.key, REFERENCE_BYTES) for reference in references]
        rows.extend(objects.items())
        with listing_path.open("w", encoding="utf-8") as stream:
            for key, content in rows:
                stream.write(json.dumps({
                    "status": "success",
                    "type": "file",
                    "key": PREFIX + key,
                    "size": len(content),
                    "lastModified": MODIFIED,
                }) + "\n")
        listing_path.chmod(0o600)

    def snapshot_provider(self, store: FakeStorage, extra_refs: list[MEDIA.Reference] | None = None):
        call = 0

        def provide(mode: str) -> MEDIA.Snapshot:
            nonlocal call
            call += 1
            listing_path = self.root / f"listing-{call}.jsonl"
            refs_path = self.root / f"refs-{call}.tsv"
            refs = self.references if extra_refs is None else extra_refs
            self.write_listing(store.objects, listing_path, refs)
            refs_path.write_text("".join(
                f"{ref.kind}\t{ref.object_id}\t{ref.key}\t{ref.logical_bytes}\n"
                for ref in sorted(refs, key=lambda item: item.key)
            ))
            refs_path.chmod(0o600)
            return MEDIA.make_snapshot(
                self.inventory_path,
                self.inventory_sha(),
                len(refs),
                listing_path,
                refs_path,
                self.journal_path,
                PREFIX,
                mode,
                NOW,
            )

        return provide

    def test_success_quarantines_before_removing_and_reports_zero_unaccounted(self) -> None:
        objects = dict(CANDIDATE_BYTES)
        store = FakeStorage(objects, self.qbase / f"MAIN-3441-{self.run_id}")
        directory = self.qbase / f"MAIN-3441-{self.run_id}"
        final, summary = MEDIA.apply_quarantine(
            self.snapshot_provider(store), store, directory, self.run_id, "2026-12-07"
        )
        self.assertEqual(set(store.removed), set(CANDIDATE_BYTES))
        self.assertEqual(final.candidates, {})
        self.assertIn(f"objects={len(CANDIDATE_BYTES)}", summary)
        self.assertNotIn("02/2", summary)
        self.assertNotIn("parts/aa/", summary)
        self.assertEqual(set(objects), set())
        self.assertEqual(directory.stat().st_mode & 0o777, 0o700)
        manifest_path = directory / "manifest.json"
        self.assertEqual(manifest_path.stat().st_mode & 0o777, 0o600)
        manifest = json.loads(manifest_path.read_text())
        self.assertEqual(manifest["status"], "complete")
        self.assertEqual(manifest["delete_after_utc"], "2026-12-07")

    def test_refuses_when_candidate_became_referenced(self) -> None:
        candidate = MEDIA.Reference("file", "2", "02/2", len(CANDIDATE_BYTES["02/2"]))
        refs = [candidate]
        self.references = refs
        self.write_references_and_journal()
        store = FakeStorage(dict(CANDIDATE_BYTES), self.qbase / "unused")
        provider = self.snapshot_provider(store)
        with self.assertRaisesRegex(MEDIA.QuarantineError, "referenced by the database"):
            provider("before")
        self.assertEqual(store.removed, [])

    def test_refuses_when_live_unaccounted_set_differs_from_inventory(self) -> None:
        objects = dict(CANDIDATE_BYTES)
        objects["06/6"] = b"unexpected"
        store = FakeStorage(objects, self.qbase / "unused")
        with self.assertRaisesRegex(MEDIA.QuarantineError, "differs from the latest inventory"):
            self.snapshot_provider(store)("before")
        self.assertEqual(store.removed, [])

    def test_copy_verification_mismatch_leaves_bucket_untouched(self) -> None:
        directory = self.qbase / f"MAIN-3441-{self.run_id}"
        store = FakeStorage(dict(CANDIDATE_BYTES), directory, mismatch_key="02/2")
        with self.assertRaisesRegex(MEDIA.QuarantineError, "bucket objects were left untouched"):
            MEDIA.apply_quarantine(self.snapshot_provider(store), store, directory, self.run_id, "2026-12-07")
        self.assertEqual(store.removed, [])
        self.assertEqual(set(store.objects), set(CANDIDATE_BYTES))

    def test_refuses_inventory_change_after_copy_before_deletion(self) -> None:
        directory = self.qbase / f"MAIN-3441-{self.run_id}"
        store = FakeStorage(dict(CANDIDATE_BYTES), directory)
        base_provider = self.snapshot_provider(store)
        calls = 0

        def changing_provider(mode: str) -> MEDIA.Snapshot:
            nonlocal calls
            calls += 1
            if calls == 2:
                store.objects["06/6"] = b"arrived-during-copy"
            return base_provider(mode)

        with self.assertRaisesRegex(MEDIA.QuarantineError, "differs from the latest inventory"):
            MEDIA.apply_quarantine(changing_provider, store, directory, self.run_id, "2026-12-07")
        self.assertEqual(store.removed, [])
        self.assertEqual(set(store.objects), set(CANDIDATE_BYTES) | {"06/6"})

    def test_refuses_candidates_in_migration_journal(self) -> None:
        self.journal_path.write_text(
            self.journal_path.read_text() + json.dumps({"type": "object", "key": "02/2"}) + "\n"
        )
        self.journal_path.chmod(0o600)
        store = FakeStorage(dict(CANDIDATE_BYTES), self.qbase / "unused")
        with self.assertRaisesRegex(MEDIA.QuarantineError, "present in the migration journal"):
            self.snapshot_provider(store)("before")
        self.assertEqual(store.removed, [])

    def test_restore_returns_verified_copies_to_rustfs(self) -> None:
        directory = self.qbase / f"MAIN-3441-{self.run_id}"
        store = FakeStorage(dict(CANDIDATE_BYTES), directory)
        MEDIA.apply_quarantine(
            self.snapshot_provider(store), store, directory, self.run_id, "2026-12-07"
        )

        def listing_provider() -> tuple[dict[str, MEDIA.ObjectMeta], dict[str, MEDIA.ObjectMeta]]:
            raw = {
                key: MEDIA.ObjectMeta(len(content), MODIFIED, "over-30d")
                for key, content in store.objects.items()
            }
            return raw, {}

        restored, already = MEDIA.restore_quarantine(
            listing_provider, store, directory, directory / "manifest.json", self.run_id
        )
        self.assertEqual((restored, already), (len(CANDIDATE_BYTES), 0))
        self.assertEqual(store.objects, CANDIDATE_BYTES)

    def test_restore_refuses_before_all_copies_are_verified(self) -> None:
        directory = self.qbase / f"MAIN-3441-{self.run_id}"
        directory.mkdir(mode=0o700)
        manifest = MEDIA.manifest_base(self.run_id, self.inventory_sha(), "2026-12-07")
        manifest["objects"] = [{"key": "02/2", "filename": "object-0001", "size": 10, "sha256": "0" * 64}]
        MEDIA.atomic_manifest(directory / "manifest.json", manifest)
        store = FakeStorage(dict(CANDIDATE_BYTES), directory)
        with self.assertRaisesRegex(MEDIA.QuarantineError, "copies are incomplete"):
            MEDIA.restore_quarantine(
                lambda: ({}, {}), store, directory, directory / "manifest.json", self.run_id
            )
        self.assertEqual(store.removed, [])


if __name__ == "__main__":
    unittest.main()
