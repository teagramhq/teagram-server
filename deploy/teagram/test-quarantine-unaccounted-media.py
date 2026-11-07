#!/usr/bin/env python3
"""Fixtures for the unaccounted media quarantine operation."""

from __future__ import annotations

import datetime as dt
import hashlib
import importlib.util
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


HELPER_PATH = pathlib.Path(__file__).with_name("ops") / "quarantine_unaccounted_media.py"
SPEC = importlib.util.spec_from_file_location("quarantine_unaccounted_media", HELPER_PATH)
assert SPEC is not None and SPEC.loader is not None
MEDIA = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = MEDIA
SPEC.loader.exec_module(MEDIA)


PREFIX = "teagramd/"
NOW = dt.datetime(2026, 11, 7, tzinfo=dt.timezone.utc)
REFERENCE = MEDIA.Reference("file", "1", "01/1", 5)
POST_MIGRATION_KEY = "parts/bb/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
POST_MIGRATION_REFERENCE = MEDIA.Reference("part", POST_MIGRATION_KEY, POST_MIGRATION_KEY, 7)
REFERENCE_BYTES = b"referenced"
CANDIDATE_BYTES = {
    "02/2": b"plain-file",
    "parts/aa/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": b"plain-part",
}
MODIFIED = "2026-09-01T00:00:00Z"
# PostgreSQL COPY text output uses tabs between columns and escapes tabs inside a field.
POSTGRES_COPY_OUTPUT = b"file\t1\t01/1\t5\n"
POSTGRES_COPY_OUTPUT_WITH_NEW_UPLOAD = (
    b"file\t1\t01/1\t5\n"
    b"part\tparts/bb/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\tparts/bb/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\t7\n"
)
POSTGRES_ESCAPED_SINGLE_COLUMN_COPY_OUTPUT = b"file\\t1\\t01/1\\t5\n"


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
        self.copied: list[str] = []
        self.restored: list[str] = []
        self.removed: list[str] = []

    def copy_from(self, key: str, destination: pathlib.Path) -> None:
        self.copied.append(key)
        content = self.objects[key]
        if key == self.mismatch_key:
            content += b"corrupt"
        destination.write_bytes(content)

    def copy_to(self, source: pathlib.Path, key: str) -> None:
        self.restored.append(key)
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

    def write_envelope_inventory(self, counts: dict[str, int]) -> pathlib.Path:
        path = self.root / "fresh-envelopes.json"
        path.write_text(json.dumps({
            "classes": {name: {"count": count} for name, count in counts.items()},
            "unversioned_envelope_references": 0,
        }))
        path.chmod(0o600)
        return path

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
            self.snapshot_provider(store), store, directory, self.run_id, "2026-12-07", lambda _: None
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

    def test_live_postgres_copy_output_parses_and_matches_journal_digest(self) -> None:
        fixture = self
        store = FakeStorage(dict(CANDIDATE_BYTES), self.root / "unused")
        captured_sql: list[str] = []

        class LiveListing:
            evidence_dir = fixture.root
            run_id = fixture.run_id

            def list_jsonl(_self, destination: pathlib.Path) -> None:
                fixture.write_listing(store.objects, destination, fixture.references)

        def fake_psql(command, *, input, stdout, stderr, check):
            self.assertIn("psql", command)
            self.assertFalse(check)
            sql = input.decode("utf-8")
            captured_sql.append(sql)
            if r"SELECT kind || E'\t' || object_id" in sql:
                stdout.write(POSTGRES_ESCAPED_SINGLE_COLUMN_COPY_OUTPUT)
            else:
                self.assertIn("SELECT kind, object_id, key, logical_bytes", sql)
                stdout.write(POSTGRES_COPY_OUTPUT)
            return subprocess.CompletedProcess(command, 0)

        provider = MEDIA.LiveSnapshotProvider(
            LiveListing(),
            self.inventory_path,
            self.inventory_sha(),
            len(self.references),
            self.journal_path,
            PREFIX,
            self.root,
        )
        with mock.patch.object(MEDIA.subprocess, "run", side_effect=fake_psql):
            snapshot = provider("before")

        self.assertIn("SELECT kind, object_id, key, logical_bytes", captured_sql[0])
        self.assertEqual(snapshot.references, {REFERENCE.key: REFERENCE})
        _journal_keys, journal_sha = MEDIA.parse_journal(self.journal_path)
        self.assertEqual(snapshot.reference_sha256, journal_sha)
        self.assertEqual((self.root / "references-01.tsv").read_bytes(), POSTGRES_COPY_OUTPUT)

    def test_apply_allows_live_references_added_after_migration(self) -> None:
        fixture = self
        directory = self.qbase / f"MAIN-3441-{self.run_id}"
        store = FakeStorage(dict(CANDIDATE_BYTES), directory)
        live_references = [REFERENCE, POST_MIGRATION_REFERENCE]
        envelope_counts: list[int] = []

        class LiveListing:
            evidence_dir = fixture.root
            run_id = fixture.run_id

            def list_jsonl(_self, destination: pathlib.Path) -> None:
                fixture.write_listing(store.objects, destination, live_references)

        def fake_psql(command, *, input, stdout, stderr, check):
            self.assertIn("psql", command)
            self.assertIn("SELECT kind, object_id, key, logical_bytes", input.decode("utf-8"))
            self.assertFalse(check)
            stdout.write(POSTGRES_COPY_OUTPUT_WITH_NEW_UPLOAD)
            return subprocess.CompletedProcess(command, 0)

        provider = MEDIA.LiveSnapshotProvider(
            LiveListing(),
            self.inventory_path,
            self.inventory_sha(),
            len(self.references),
            self.journal_path,
            PREFIX,
            self.root,
        )
        with mock.patch.object(MEDIA.subprocess, "run", side_effect=fake_psql):
            final, _summary = MEDIA.apply_quarantine(
                provider,
                store,
                directory,
                self.run_id,
                "2026-12-07",
                envelope_counts.append,
            )

        _journal_keys, journal_sha = MEDIA.parse_journal(self.journal_path)
        self.assertEqual(len(final.references), 2)
        self.assertNotEqual(final.reference_sha256, journal_sha)
        self.assertEqual(envelope_counts, [2, 2, 2])
        self.assertEqual(set(store.removed), set(CANDIDATE_BYTES))

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
            MEDIA.apply_quarantine(
                self.snapshot_provider(store), store, directory, self.run_id, "2026-12-07", lambda _: None
            )
        self.assertEqual(store.removed, [])
        self.assertEqual(set(store.objects), set(CANDIDATE_BYTES))

    def test_envelope_check_failure_prevents_quarantine_copies_and_removal(self) -> None:
        directory = self.qbase / f"MAIN-3441-{self.run_id}"
        store = FakeStorage(dict(CANDIDATE_BYTES), directory)

        def reject_envelopes(_referenced_count: int) -> None:
            raise MEDIA.QuarantineError("fresh envelope inventory found invalid references")

        with self.assertRaisesRegex(MEDIA.QuarantineError, "invalid references"):
            MEDIA.apply_quarantine(
                self.snapshot_provider(store), store, directory, self.run_id, "2026-12-07", reject_envelopes
            )
        self.assertEqual(store.copied, [])
        self.assertEqual(store.removed, [])

    def test_envelope_check_failure_before_deletion_leaves_bucket_untouched(self) -> None:
        directory = self.qbase / f"MAIN-3441-{self.run_id}"
        store = FakeStorage(dict(CANDIDATE_BYTES), directory)
        calls = 0

        def reject_before_delete(_referenced_count: int) -> None:
            nonlocal calls
            calls += 1
            if calls == 2:
                raise MEDIA.QuarantineError("fresh envelope inventory found invalid references")

        with self.assertRaisesRegex(MEDIA.QuarantineError, "invalid references"):
            MEDIA.apply_quarantine(
                self.snapshot_provider(store), store, directory, self.run_id, "2026-12-07", reject_before_delete
            )
        self.assertEqual(calls, 2)
        self.assertEqual(set(store.copied), set(CANDIDATE_BYTES))
        self.assertEqual(store.removed, [])
        self.assertEqual(set(store.objects), set(CANDIDATE_BYTES))

    def test_envelope_check_failure_after_removal_leaves_manifest_recoverable(self) -> None:
        directory = self.qbase / f"MAIN-3441-{self.run_id}"
        store = FakeStorage(dict(CANDIDATE_BYTES), directory)
        calls = 0

        def reject_after_removal(_referenced_count: int) -> None:
            nonlocal calls
            calls += 1
            if calls == 3:
                raise MEDIA.QuarantineError("fresh envelope inventory found invalid references")

        with self.assertRaisesRegex(MEDIA.QuarantineError, "invalid references"):
            MEDIA.apply_quarantine(
                self.snapshot_provider(store), store, directory, self.run_id, "2026-12-07", reject_after_removal
            )
        self.assertEqual(calls, 3)
        self.assertEqual(set(store.removed), set(CANDIDATE_BYTES))
        manifest = json.loads((directory / "manifest.json").read_text())
        self.assertEqual(manifest["status"], "deleting")
        self.assertTrue(all(record["deleted"] for record in manifest["objects"]))

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
            MEDIA.apply_quarantine(changing_provider, store, directory, self.run_id, "2026-12-07", lambda _: None)
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
            self.snapshot_provider(store), store, directory, self.run_id, "2026-12-07", lambda _: None
        )

        def listing_provider() -> tuple[dict[str, MEDIA.ObjectMeta], dict[str, MEDIA.ObjectMeta]]:
            raw = {
                key: MEDIA.ObjectMeta(len(content), MODIFIED, "over-30d")
                for key, content in store.objects.items()
            }
            return raw, {}

        restored, already = MEDIA.restore_quarantine(
            listing_provider,
            store,
            directory,
            directory / "manifest.json",
            self.run_id,
            lambda: 1,
            lambda _: None,
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
                lambda: ({}, {}),
                store,
                directory,
                directory / "manifest.json",
                self.run_id,
                lambda: 1,
                lambda _: None,
            )
        self.assertEqual(store.removed, [])

    def test_restore_envelope_check_failure_prevents_copy_to(self) -> None:
        directory = self.qbase / f"MAIN-3441-{self.run_id}"
        store = FakeStorage(dict(CANDIDATE_BYTES), directory)
        MEDIA.apply_quarantine(
            self.snapshot_provider(store), store, directory, self.run_id, "2026-12-07", lambda _: None
        )
        removed_before_restore = list(store.removed)

        def reject_envelopes(_referenced_count: int) -> None:
            raise MEDIA.QuarantineError("fresh envelope inventory found invalid references")

        with self.assertRaisesRegex(MEDIA.QuarantineError, "invalid references"):
            MEDIA.restore_quarantine(
                lambda: ({}, {}),
                store,
                directory,
                directory / "manifest.json",
                self.run_id,
                lambda: 1,
                reject_envelopes,
            )
        self.assertEqual(store.restored, [])
        self.assertEqual(store.removed, removed_before_restore)

    def test_restore_envelope_check_runs_after_restore(self) -> None:
        directory = self.qbase / f"MAIN-3441-{self.run_id}"
        store = FakeStorage(dict(CANDIDATE_BYTES), directory)
        MEDIA.apply_quarantine(
            self.snapshot_provider(store), store, directory, self.run_id, "2026-12-07", lambda _: None
        )
        calls = 0

        def reject_after_restore(_referenced_count: int) -> None:
            nonlocal calls
            calls += 1
            if calls == 2:
                raise MEDIA.QuarantineError("fresh envelope inventory found invalid references")

        with self.assertRaisesRegex(MEDIA.QuarantineError, "invalid references"):
            MEDIA.restore_quarantine(
                lambda: (
                    {
                        key: MEDIA.ObjectMeta(len(content), MODIFIED, "over-30d")
                        for key, content in store.objects.items()
                    },
                    {},
                ),
                store,
                directory,
                directory / "manifest.json",
                self.run_id,
                lambda: 1,
                reject_after_restore,
            )
        self.assertEqual(calls, 2)
        self.assertEqual(set(store.restored), set(CANDIDATE_BYTES))
        manifest = json.loads((directory / "manifest.json").read_text())
        self.assertEqual(manifest["restore_status"], "restoring")

    def test_restore_uses_live_reference_count_after_migration_growth(self) -> None:
        directory = self.qbase / f"MAIN-3441-{self.run_id}"
        store = FakeStorage(dict(CANDIDATE_BYTES), directory)
        MEDIA.apply_quarantine(
            self.snapshot_provider(store), store, directory, self.run_id, "2026-12-07", lambda _: None
        )

        query_calls = 0
        envelope_counts: list[int] = []
        diagnostic_storage = MEDIA.LiveMCStorage(
            [], self.root, self.root, PREFIX, self.root, self.run_id
        )

        def fake_psql(command, *, input, stdout, stderr, check):
            self.assertIn("psql", command)
            self.assertIn("SELECT kind, object_id, key, logical_bytes", input.decode("utf-8"))
            self.assertFalse(check)
            stdout.write(POSTGRES_COPY_OUTPUT_WITH_NEW_UPLOAD)
            return subprocess.CompletedProcess(command, 0)

        def current_reference_count() -> int:
            nonlocal query_calls
            query_calls += 1
            return diagnostic_storage.query_current_reference_count(
                self.root / f"restore-references-{query_calls:02d}.tsv"
            )

        def listing_provider() -> tuple[dict[str, MEDIA.ObjectMeta], dict[str, MEDIA.ObjectMeta]]:
            raw = {
                key: MEDIA.ObjectMeta(len(content), MODIFIED, "over-30d")
                for key, content in store.objects.items()
            }
            return raw, {}

        with mock.patch.object(MEDIA.subprocess, "run", side_effect=fake_psql):
            restored, already = MEDIA.restore_quarantine(
                listing_provider,
                store,
                directory,
                directory / "manifest.json",
                self.run_id,
                current_reference_count,
                envelope_counts.append,
            )

        self.assertEqual((restored, already), (len(CANDIDATE_BYTES), 0))
        self.assertEqual(query_calls, 2)
        self.assertEqual(envelope_counts, [2, 2])
        error_paths = [
            self.root / f"media-quarantine-{self.run_id}-{index:04d}-postgres.stderr"
            for index in (1, 2)
        ]
        self.assertTrue(all(path.is_file() for path in error_paths))
        self.assertTrue(all(path.stat().st_mode & 0o777 == 0o600 for path in error_paths))
        self.assertEqual(store.objects, CANDIDATE_BYTES)

    def test_fresh_envelope_inventory_accepts_all_valid_references(self) -> None:
        path = self.write_envelope_inventory({
            "valid_envelope": 1,
            "no_valid_envelope": 0,
            "missing": 0,
            "unaccounted": 0,
        })
        MEDIA.validate_fresh_envelope_inventory(path, referenced_count=1)

    def test_fresh_envelope_inventory_rejects_preexisting_corruption(self) -> None:
        path = self.write_envelope_inventory({
            "valid_envelope": 0,
            "no_valid_envelope": 1,
            "missing": 0,
            "unaccounted": 0,
        })
        with self.assertRaisesRegex(MEDIA.QuarantineError, "found invalid or missing references") as error:
            MEDIA.validate_fresh_envelope_inventory(path, referenced_count=1)
        self.assertNotIn("01/1", str(error.exception))

    def test_fresh_envelope_inventory_rejects_reference_count_drift(self) -> None:
        path = self.write_envelope_inventory({
            "valid_envelope": 2,
            "no_valid_envelope": 0,
            "missing": 0,
            "unaccounted": 0,
        })
        with self.assertRaisesRegex(MEDIA.QuarantineError, "found invalid or missing references"):
            MEDIA.validate_fresh_envelope_inventory(path, referenced_count=1)

    def test_rustfs_alias_failure_keeps_stderr_and_stops_command(self) -> None:
        config_dir = self.root / "mc-config"
        mount_dir = self.root / "mount"
        bin_dir = self.root / "bin"
        secret_path = self.root / "blob-secret"
        config_dir.mkdir()
        mount_dir.mkdir()
        bin_dir.mkdir()
        secret_path.write_text("fixture-secret")
        fake_mc = bin_dir / "mc"
        fake_mc.write_text(
            "#!/bin/sh\n"
            "if [ \"$1\" = alias ]; then\n"
            "  printf 'fixture alias stdout\\n'\n"
            "  printf 'fixture alias rejected\\n' >&2\n"
            "  exit 23\n"
            "fi\n"
            "printf 'later command ran\\n' >&2\n"
        )
        fake_mc.chmod(0o755)
        storage = MEDIA.LiveMCStorage(
            ["docker", "compose"], config_dir, mount_dir, "teagramd/", self.root, self.run_id
        )
        command = storage._command(["ls", "local/teagram/teagramd/"])
        script = command[command.index("-ec") + 1].replace(
            "/run/secrets/teagramd-blob-secret-key", str(secret_path)
        )
        env = os.environ.copy()
        env["PATH"] = f"{bin_dir}:{env['PATH']}"
        env["TG_BLOB_S3_ACCESS_KEY_ID"] = "fixture-access-key"
        result = subprocess.run(
            ["/bin/sh", "-ec", script, "mc", "ls", "local/teagram/teagramd/"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            env=env,
            check=False,
        )
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout, "")
        self.assertIn("fixture alias rejected", result.stderr)
        self.assertNotIn("later command ran", result.stderr)


if __name__ == "__main__":
    unittest.main()
