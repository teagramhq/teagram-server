#!/usr/bin/env python3
from __future__ import annotations

import hashlib
import json
import os
import re
import runpy
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from typing import Any


SCRIPT_DIR = Path(__file__).resolve().parent
PROJECT_ROOT = SCRIPT_DIR.parents[2]
GATE = SCRIPT_DIR / "qualify-rustfs-transition.py"
SCHEMA_QUERY = SCRIPT_DIR / "rustfs-schema-capture.sql"
INERT_QUERY = SCRIPT_DIR / "rustfs-inert-surfaces.sql"
R70_SCHEMA_QUERY = SCRIPT_DIR / "rustfs-r70-schema-capture.sql"
R70_INERT_QUERY = SCRIPT_DIR / "rustfs-r70-inert-surfaces.sql"
POSTGRES_CONTAINER = os.environ.get("RUSTFS_POSTGRES_CONTAINER", os.environ.get("R69_POSTGRES_CONTAINER", ""))
ATLAS_INPUT = os.environ.get("RUSTFS_ATLAS_INPUT", os.environ.get("R69_ATLAS_INPUT", ""))
RELEASE_SET = os.environ.get("RUSTFS_RELEASE_SET", "60-69")
GATE_MODULE = runpy.run_path(str(GATE), run_name="qualify_module")
GATE_REJECT = GATE_MODULE["GateReject"]
RELEASE = GATE_MODULE["RELEASES"][RELEASE_SET]
FIXTURE_TEST_MODULE = runpy.run_path(
    str(SCRIPT_DIR / "test-qualify-rustfs-transition.py"),
    run_name="qualification_fixture",
)
FIXTURE_ROOT = SCRIPT_DIR / "testdata" / f"release-{RELEASE_SET}"
LIVE_MIGRATIONS = PROJECT_ROOT / "migrations"
FIRST_RELEASE_VERSION = RELEASE["revisions"][0]
LIVE_R70_FILE = "20261008000070_erasure_outbox_epoch_markers.sql"
SYNTHETIC_FUTURE_FILE = "20261009000071_synthetic_future_qualification.sql"


def read_regular_bytes(path: Path) -> bytes:
    try:
        info = path.lstat()
    except OSError as exc:
        raise AssertionError(f"expected regular migration input is unreadable: {path.name}") from exc
    if not stat.S_ISREG(info.st_mode):
        raise AssertionError(f"migration input is not a regular file: {path.name}")

    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        fd = os.open(path, flags)
        with os.fdopen(fd, "rb") as stream:
            opened_info = os.fstat(stream.fileno())
            if not stat.S_ISREG(opened_info.st_mode):
                raise AssertionError(f"migration input is not a regular file: {path.name}")
            return stream.read()
    except OSError as exc:
        raise AssertionError(f"expected regular migration input is unreadable: {path.name}") from exc


def write_new_regular_file(path: Path, data: bytes, mode: int = 0o644) -> None:
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    try:
        fd = os.open(path, flags, mode)
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
        os.chmod(path, mode)
    except OSError as exc:
        raise AssertionError(f"could not create staged migration input: {path.name}") from exc


def replace_regular_file(path: Path, data: bytes) -> None:
    read_regular_bytes(path)
    path.unlink()
    write_new_regular_file(path, data)


def atlas_sum_migration_names(atlas_sum: bytes) -> list[str]:
    try:
        lines = atlas_sum.decode("utf-8").splitlines()
    except UnicodeDecodeError as exc:
        raise AssertionError(f"{RELEASE_SET} fixture atlas.sum is not UTF-8") from exc
    if not lines or not lines[0].startswith("h1:"):
        raise AssertionError(f"{RELEASE_SET} fixture atlas.sum header is invalid")

    names: list[str] = []
    seen: set[str] = set()
    for line in lines[1:]:
        parts = line.split(" ")
        if len(parts) != 2 or not re.fullmatch(r"h1:[A-Za-z0-9+/]{43}=", parts[1]):
            raise AssertionError(f"{RELEASE_SET} fixture atlas.sum contains an invalid migration row")
        name = parts[0]
        if not re.fullmatch(r"[0-9]{14}_[A-Za-z0-9][A-Za-z0-9._-]*\.sql", name):
            raise AssertionError(f"{RELEASE_SET} fixture atlas.sum contains an invalid migration name")
        if name in seen:
            raise AssertionError(f"{RELEASE_SET} fixture atlas.sum contains a duplicate migration name")
        seen.add(name)
        names.append(name)

    release_names = [name for name in names if name[:14] >= FIRST_RELEASE_VERSION]
    if release_names != RELEASE["files"]:
        raise AssertionError(f"{RELEASE_SET} fixture atlas.sum does not name exactly the pinned release files")
    for name in RELEASE["files"]:
        expected_pin = RELEASE["atlas_pins"].get(name)
        if expected_pin is None:
            raise AssertionError(f"{RELEASE_SET} gate has no Atlas pin for fixture input: {name}")
        row = next((line.split(" ", 1) for line in lines[1:] if line.startswith(f"{name} ")), None)
        if row != [name, expected_pin]:
            raise AssertionError(f"{RELEASE_SET} fixture atlas.sum does not match gate pin: {name}")
    return names


def prepare_atlas_input(atlas_input: Path, release_set: str = RELEASE_SET) -> None:
    global RELEASE_SET, RELEASE, FIXTURE_ROOT, FIRST_RELEASE_VERSION
    RELEASE_SET = release_set
    RELEASE = GATE_MODULE["RELEASES"][release_set]
    FIXTURE_ROOT = SCRIPT_DIR / "testdata" / f"release-{release_set}"
    FIRST_RELEASE_VERSION = RELEASE["revisions"][0]
    FIXTURE_TEST_MODULE["verify_fixture_provenance"](FIXTURE_ROOT, release_set)
    try:
        input_info = atlas_input.lstat()
    except OSError as exc:
        raise AssertionError("fresh Atlas input directory is missing or unreadable") from exc
    if not stat.S_ISDIR(input_info.st_mode) or stat.S_IMODE(input_info.st_mode) != 0o700:
        raise AssertionError("fresh Atlas input directory must be a real mode-0700 directory")
    if any(atlas_input.iterdir()):
        raise AssertionError("fresh Atlas input directory is not empty")

    atlas_sum = read_regular_bytes(FIXTURE_ROOT / "atlas.sum")
    migration_names = atlas_sum_migration_names(atlas_sum)
    earlier_names = [name for name in migration_names if name[:14] < FIRST_RELEASE_VERSION]
    expected_names = {"atlas.sum", *RELEASE["files"], *earlier_names}

    write_new_regular_file(atlas_input / "atlas.sum", atlas_sum)
    for name in earlier_names:
        write_new_regular_file(
            atlas_input / name,
            read_regular_bytes(LIVE_MIGRATIONS / name),
        )
    for name in RELEASE["files"]:
        write_new_regular_file(
            atlas_input / name,
            read_regular_bytes(FIXTURE_ROOT / name),
        )

    staged_entries = list(atlas_input.iterdir())
    actual_names = {entry.name for entry in staged_entries}
    if actual_names != expected_names:
        raise AssertionError(f"staged Atlas input names do not match the frozen {release_set} manifest")
    for entry in staged_entries:
        info = entry.lstat()
        if not stat.S_ISREG(info.st_mode):
            raise AssertionError(f"staged Atlas input is not a regular file: {entry.name}")


def copy_staged_inputs(destination: Path) -> None:
    destination.mkdir(mode=0o700)
    for source in Path(ATLAS_INPUT).iterdir():
        write_new_regular_file(destination / source.name, read_regular_bytes(source))


def write_valid_migration_bundle(bundle: Path, release_set: str = "60-69") -> None:
    bundle.mkdir(mode=0o700)
    evidence = FIXTURE_TEST_MODULE["good_migration_evidence"](release_set)
    evidence_path = bundle / "migrations.json"
    write_new_regular_file(evidence_path, json.dumps(evidence, sort_keys=True).encode("utf-8"), 0o600)


def reject_overlay_through_gate(checkout: Path, bundle: Path, expected_release_set: str) -> str:
    stage = "select_migration_release"
    try:
        release_set = GATE_MODULE["select_migration_release"](bundle)
        if release_set != expected_release_set:
            raise AssertionError(f"migration fixture selected an unexpected release: {release_set}")
        stage = "validate_migration_schema"
        GATE_MODULE["validate_migration_schema"](bundle, checkout)
    except GATE_REJECT as exc:
        if exc.reason != "schema_rejected":
            raise AssertionError(f"migration overlay used unexpected gate rejection: {exc.reason}") from exc
        return stage
    raise AssertionError("RustFS gate accepted an unapproved migration overlay")


def psql(sql: str) -> list[str]:
    result = subprocess.run(
        [
            "docker",
            "exec",
            "-i",
            POSTGRES_CONTAINER,
            "psql",
            "-X",
            "-q",
            "-A",
            "-t",
            "-v",
            "ON_ERROR_STOP=1",
            "-U",
            "postgres",
            "-d",
            os.environ.get("RUSTFS_POSTGRES_DB", "telegram"),
        ],
        input=sql,
        text=True,
        capture_output=True,
        check=False,
    )
    if result.returncode != 0:
        raise AssertionError("PostgreSQL 16 catalog capture failed")
    return [line for line in result.stdout.splitlines() if line]


def capture(query_source: Path | str, mutation: str = "") -> dict[str, Any]:
    query = (
        query_source.read_text(encoding="utf-8")
        if isinstance(query_source, Path)
        else query_source
    )
    sql = f"BEGIN;\n{mutation}\n{query}\nROLLBACK;\n" if mutation else query
    rows = psql(sql)
    if len(rows) != 1:
        raise AssertionError("PostgreSQL 16 catalog capture returned an unexpected row count")
    try:
        result = json.loads(rows[0])
    except json.JSONDecodeError as exc:
        raise AssertionError("PostgreSQL 16 catalog capture was not JSON") from exc
    if not isinstance(result, dict):
        raise AssertionError("PostgreSQL 16 catalog capture was not an object")
    return result


def schema_capture(mutation: str = "") -> dict[str, Any]:
    evidence = capture(SCHEMA_QUERY, mutation)
    if RELEASE_SET == "60-70":
        evidence.update(capture(R70_SCHEMA_QUERY, mutation))
    return evidence


def inert_capture(mutation: str = "") -> dict[str, Any]:
    return capture(R70_INERT_QUERY if RELEASE_SET == "60-70" else INERT_QUERY, mutation)


def validate_68(evidence: dict[str, Any]) -> None:
    GATE_MODULE["validate_migration_68_schema"](evidence)


def validate_69(evidence: dict[str, Any]) -> None:
    GATE_MODULE["validate_migration_69_schema"](evidence)


def validate_70(evidence: dict[str, Any]) -> None:
    GATE_MODULE["validate_migration_70_schema"](evidence)


def reject_68(label: str, mutation: str) -> None:
    evidence = schema_capture(mutation)
    try:
        validate_68(evidence)
    except GATE_REJECT:
        return
    raise AssertionError(f"PostgreSQL mutation was not rejected by migration 68 qualification: {label}")


def reject_69(label: str, mutation: str) -> None:
    reject_69_evidence(label, schema_capture(mutation))


def reject_69_evidence(label: str, evidence: dict[str, Any]) -> None:
    try:
        validate_69(evidence)
    except GATE_REJECT:
        return
    raise AssertionError(f"PostgreSQL mutation was not rejected by migration 69 qualification: {label}")


def reject_70(label: str, mutation: str) -> None:
    evidence = schema_capture(mutation)
    try:
        validate_70(evidence)
    except GATE_REJECT:
        return
    raise AssertionError(f"PostgreSQL mutation was not rejected by migration 70 qualification: {label}")


def constraint_evidence(evidence: dict[str, Any], table: str, name: str) -> dict[str, Any]:
    return evidence["migration_69_schema"]["tables"][table]["constraints"][name]


def restore_expected_index_catalog(evidence: dict[str, Any], table: str) -> None:
    indexes = GATE_MODULE["R69_INDEX_NAMES"][table]
    table_evidence = evidence["migration_69_schema"]["tables"][table]
    table_evidence["index_names"] = indexes
    table_evidence["index_validity"] = {name: True for name in indexes}


class RustFSCatalogQualification(unittest.TestCase):
    def test_live_transition_query_validates_atlas_revision_hashes(self) -> None:
        observation = capture(GATE_MODULE["live_schema_query"](RELEASE_SET))
        metadata = FIXTURE_TEST_MODULE["good_migration_evidence"](RELEASE_SET)

        for version, expected in metadata["revision_detail"].items():
            with self.subTest(version=version):
                self.assertEqual(observation["revision_detail"][version]["hash"], expected["hash"])
                expected_keys = {"applied", "total", "error", "hash"}
                if RELEASE_SET == "60-70":
                    expected_keys.update({"error_stmt_empty", "partial_hashes_empty"})
                    self.assertIs(observation["revision_detail"][version]["error_stmt_empty"], True)
                    self.assertIs(observation["revision_detail"][version]["partial_hashes_empty"], True)
                self.assertEqual(set(observation["revision_detail"][version]), expected_keys)

        GATE_MODULE["validate_live_schema_observation"](metadata, observation, RELEASE_SET)

    @unittest.skipUnless(RELEASE_SET == "60-70", "R70-only live Atlas revision mutations")
    def test_r70_live_transition_query_rejects_nonempty_revision_failure_fields(self) -> None:
        metadata = FIXTURE_TEST_MODULE["good_migration_evidence"]("60-70")
        first_version = RELEASE["revisions"][0]
        last_version = RELEASE["revisions"][-1]
        mutations = {
            "error_stmt": (
                "UPDATE atlas_schema_revisions.atlas_schema_revisions "
                "SET error_stmt = 'r70 live probe' "
                f"WHERE version >= '{first_version}' AND version <= '{last_version}';"
            ),
            "partial_hashes": (
                "UPDATE atlas_schema_revisions.atlas_schema_revisions "
                "SET partial_hashes = '{\"probe\":true}'::jsonb "
                f"WHERE version >= '{first_version}' AND version <= '{last_version}';"
            ),
        }
        for field, mutation in mutations.items():
            with self.subTest(field=field):
                observation = capture(GATE_MODULE["live_schema_query"]("60-70"), mutation)
                self.assertEqual(set(observation["revision_detail"]), set(RELEASE["revisions"]))
                field_key = f"{field}_empty"
                self.assertTrue(
                    all(detail[field_key] is False for detail in observation["revision_detail"].values())
                )
                with self.assertRaises(GATE_REJECT):
                    GATE_MODULE["validate_live_schema_observation"](
                        metadata, observation, "60-70"
                    )

    def test_actual_atlas_directory_and_postgres_catalog_pass(self) -> None:
        self.assertTrue(ATLAS_INPUT, "RUSTFS_ATLAS_INPUT is required")
        migrations = Path(ATLAS_INPUT)
        self.assertEqual(stat.S_IMODE(migrations.lstat().st_mode), 0o700)
        staged_atlas_sum = read_regular_bytes(migrations / "atlas.sum")
        atlas_sum_names = atlas_sum_migration_names(staged_atlas_sum)
        expected_names = {
            "atlas.sum",
            *RELEASE["files"],
            *(name for name in atlas_sum_names if name[:14] < FIRST_RELEASE_VERSION),
        }
        actual_names = {path.name for path in migrations.iterdir()}
        self.assertEqual(actual_names, expected_names)
        for path in migrations.iterdir():
            self.assertTrue(stat.S_ISREG(path.lstat().st_mode), path.name)

        expected_names = set(RELEASE["files"])
        actual_release_names = {
            path.name
            for path in migrations.iterdir()
            if re.fullmatch(r"[0-9]{14}_.*\.sql", path.name)
            and path.name[:14] >= FIRST_RELEASE_VERSION
        }
        self.assertEqual(actual_release_names, expected_names)
        self.assertEqual(hashlib.sha256(staged_atlas_sum).hexdigest(), RELEASE["atlas_sum_sha256"])
        for name, digest in RELEASE["file_sha256"].items():
            self.assertEqual(hashlib.sha256(read_regular_bytes(migrations / name)).hexdigest(), digest)

        final_version = RELEASE["revisions"][-1]
        self.assertEqual(
            psql("SELECT COALESCE(MAX(version), '') FROM atlas_schema_revisions.atlas_schema_revisions;\n"),
            [final_version],
        )
        self.assertEqual(
            psql(
                "SELECT COUNT(*)::text FROM atlas_schema_revisions.atlas_schema_revisions "
                f"WHERE version > '{final_version}';\n"
            ),
            ["0"],
        )
        revision_rows = psql(
            "SELECT concat_ws(E'\\t', version, hash, type::text, applied::text, total::text, "
            "(COALESCE(error, '') = '')::text, (COALESCE(error_stmt, '') = '')::text, "
            "(NOT (partial_hashes IS NOT NULL AND partial_hashes <> 'null'::jsonb "
            "AND partial_hashes <> '{}'::jsonb AND partial_hashes <> '[]'::jsonb))::text) "
            "FROM atlas_schema_revisions.atlas_schema_revisions "
            f"WHERE version >= '{FIRST_RELEASE_VERSION}' ORDER BY version;\n"
        )
        self.assertEqual(len(revision_rows), len(RELEASE["revisions"]))
        for row, version, filename in zip(
            revision_rows,
            RELEASE["revisions"],
            RELEASE["files"],
            strict=True,
        ):
            fields = row.split("\t")
            self.assertEqual(len(fields), 8)
            self.assertEqual(fields[:3], [version, RELEASE["atlas_pins"][filename].removeprefix("h1:"), "2"])
            self.assertGreater(int(fields[3]), 0)
            self.assertEqual(fields[3], fields[4])
            self.assertEqual(fields[5:], ["true", "true", "true"])

        evidence = schema_capture()
        validate_68(evidence)
        validate_69(evidence)
        if RELEASE_SET == "60-70":
            validate_70(evidence)
        inert = inert_capture()
        expected_surfaces = GATE_MODULE[
            "R70_INERT_SURFACES" if RELEASE_SET == "60-70" else "INERT_SURFACES"
        ]
        self.assertEqual(inert, {name: False for name in expected_surfaces})
        GATE_MODULE["validate_inert_surfaces"]({"inert_surfaces": inert}, expected_surfaces)
        self.assertEqual(psql("SELECT (NOT EXISTS (SELECT 1 FROM public.files))::text;\n"), ["true"])
        print(f"postgres16_baseline=passed release_set={RELEASE_SET} public_files=empty")

    def assert_fixed_overlay_rejected(self, label: str, overlay: str) -> None:
        with tempfile.TemporaryDirectory(prefix="r69-migration-overlay-") as temporary_root:
            root = Path(temporary_root)
            checkout = root / "checkout"
            checkout.mkdir(mode=0o700)
            migrations = checkout / "migrations"
            copy_staged_inputs(migrations)
            bundle = root / "bundle"
            write_valid_migration_bundle(bundle, "60-69")

            if overlay == "real-live-r70-and-live-atlas-sum":
                write_new_regular_file(
                    migrations / LIVE_R70_FILE,
                    read_regular_bytes(LIVE_MIGRATIONS / LIVE_R70_FILE),
                )
                replace_regular_file(
                    migrations / "atlas.sum",
                    read_regular_bytes(LIVE_MIGRATIONS / "atlas.sum"),
                )
            elif overlay == "synthetic-future-with-fixture-atlas-sum":
                write_new_regular_file(
                    migrations / SYNTHETIC_FUTURE_FILE,
                    b"SELECT 1;\n",
                )
            elif overlay == "fixture-files-with-live-atlas-sum":
                replace_regular_file(
                    migrations / "atlas.sum",
                    read_regular_bytes(LIVE_MIGRATIONS / "atlas.sum"),
                )
            else:
                raise AssertionError(f"unknown fixed migration overlay: {overlay}")

            rejection_stage = reject_overlay_through_gate(checkout, bundle, "60-69")
            print(
                f"overlay_negative={label} result=GateReject(schema_rejected) "
                f"via={rejection_stage}"
            )

    def test_fixed_real_live_r70_and_live_atlas_sum_overlay_rejects(self) -> None:
        self.assert_fixed_overlay_rejected(
            "real-live-r70-plus-live-atlas-sum",
            "real-live-r70-and-live-atlas-sum",
        )

    def test_fixed_synthetic_future_revision_with_fixture_atlas_sum_rejects(self) -> None:
        self.assert_fixed_overlay_rejected(
            "synthetic-future-plus-fixture-atlas-sum",
            "synthetic-future-with-fixture-atlas-sum",
        )

    def test_fixed_fixture_files_with_live_atlas_sum_rejects(self) -> None:
        self.assert_fixed_overlay_rejected(
            "fixture-files-plus-live-atlas-sum",
            "fixture-files-with-live-atlas-sum",
        )

    def test_ordered_owner_and_pointer_keys_and_foreign_key_actions_reject(self) -> None:
        mutations = {
            "owner local key order": """
ALTER TABLE public.user_photos DROP CONSTRAINT user_photos_file_owned_by_owner;
ALTER TABLE public.user_photos ADD CONSTRAINT user_photos_file_owned_by_owner
    FOREIGN KEY (user_id, file_id) REFERENCES public.files (id, uploader_id) ON DELETE RESTRICT;
""",
            "owner referenced key order": """
ALTER TABLE public.user_photos DROP CONSTRAINT user_photos_file_owned_by_owner;
CREATE UNIQUE INDEX files_owner_order_probe_idx ON public.files (uploader_id, id);
ALTER TABLE public.user_photos ADD CONSTRAINT user_photos_file_owned_by_owner
    FOREIGN KEY (file_id, user_id) REFERENCES public.files (uploader_id, id) ON DELETE RESTRICT;
""",
            "owner delete action": """
ALTER TABLE public.user_photos DROP CONSTRAINT user_photos_file_owned_by_owner;
ALTER TABLE public.user_photos ADD CONSTRAINT user_photos_file_owned_by_owner
    FOREIGN KEY (file_id, user_id) REFERENCES public.files (id, uploader_id) ON DELETE CASCADE;
""",
            "owner update action": """
ALTER TABLE public.user_photos DROP CONSTRAINT user_photos_file_owned_by_owner;
ALTER TABLE public.user_photos ADD CONSTRAINT user_photos_file_owned_by_owner
    FOREIGN KEY (file_id, user_id) REFERENCES public.files (id, uploader_id)
    ON DELETE RESTRICT ON UPDATE CASCADE;
""",
            "owner match type": """
ALTER TABLE public.user_photos DROP CONSTRAINT user_photos_file_owned_by_owner;
ALTER TABLE public.user_photos ADD CONSTRAINT user_photos_file_owned_by_owner
    FOREIGN KEY (file_id, user_id) REFERENCES public.files (id, uploader_id)
    MATCH FULL ON DELETE RESTRICT;
""",
            "owner conindid": """
ALTER TABLE public.user_photos DROP CONSTRAINT user_photos_file_owned_by_owner;
ALTER TABLE public.profile_upload_receipt DROP CONSTRAINT profile_upload_receipt_file_owned_by_owner;
DROP INDEX public.files_id_uploader_id_key;
CREATE UNIQUE INDEX files_owner_conindid_probe_idx ON public.files (id, uploader_id);
ALTER TABLE public.user_photos ADD CONSTRAINT user_photos_file_owned_by_owner
    FOREIGN KEY (file_id, user_id) REFERENCES public.files (id, uploader_id) ON DELETE RESTRICT;
ALTER TABLE public.profile_upload_receipt ADD CONSTRAINT profile_upload_receipt_file_owned_by_owner
    FOREIGN KEY (file_id, user_id) REFERENCES public.files (id, uploader_id) ON DELETE SET NULL (file_id);
""",
            "pointer local key order": """
ALTER TABLE public.profile_photo_state DROP CONSTRAINT profile_photo_state_current_is_own_gallery_entry;
ALTER TABLE public.profile_photo_state ADD CONSTRAINT profile_photo_state_current_is_own_gallery_entry
    FOREIGN KEY (current_file_id, user_id) REFERENCES public.user_photos (user_id, file_id) ON DELETE RESTRICT;
""",
            "pointer referenced key order": """
ALTER TABLE public.profile_photo_state DROP CONSTRAINT profile_photo_state_current_is_own_gallery_entry;
CREATE UNIQUE INDEX user_photos_pointer_order_probe_idx ON public.user_photos (file_id, user_id);
ALTER TABLE public.profile_photo_state ADD CONSTRAINT profile_photo_state_current_is_own_gallery_entry
    FOREIGN KEY (user_id, current_file_id) REFERENCES public.user_photos (file_id, user_id) ON DELETE RESTRICT;
""",
            "pointer delete action": """
ALTER TABLE public.profile_photo_state DROP CONSTRAINT profile_photo_state_current_is_own_gallery_entry;
ALTER TABLE public.profile_photo_state ADD CONSTRAINT profile_photo_state_current_is_own_gallery_entry
    FOREIGN KEY (user_id, current_file_id) REFERENCES public.user_photos (user_id, file_id) ON DELETE CASCADE;
""",
            "pointer update action": """
ALTER TABLE public.profile_photo_state DROP CONSTRAINT profile_photo_state_current_is_own_gallery_entry;
ALTER TABLE public.profile_photo_state ADD CONSTRAINT profile_photo_state_current_is_own_gallery_entry
    FOREIGN KEY (user_id, current_file_id) REFERENCES public.user_photos (user_id, file_id)
    ON DELETE RESTRICT ON UPDATE CASCADE;
""",
            "pointer match type": """
ALTER TABLE public.profile_photo_state DROP CONSTRAINT profile_photo_state_current_is_own_gallery_entry;
ALTER TABLE public.profile_photo_state ADD CONSTRAINT profile_photo_state_current_is_own_gallery_entry
    FOREIGN KEY (user_id, current_file_id) REFERENCES public.user_photos (user_id, file_id)
    MATCH FULL ON DELETE RESTRICT;
""",
            "receipt local key order": """
ALTER TABLE public.profile_upload_receipt DROP CONSTRAINT profile_upload_receipt_file_owned_by_owner;
ALTER TABLE public.profile_upload_receipt ADD CONSTRAINT profile_upload_receipt_file_owned_by_owner
    FOREIGN KEY (user_id, file_id) REFERENCES public.files (id, uploader_id)
    ON DELETE SET NULL (file_id);
""",
            "receipt update action": """
ALTER TABLE public.profile_upload_receipt DROP CONSTRAINT profile_upload_receipt_file_owned_by_owner;
ALTER TABLE public.profile_upload_receipt ADD CONSTRAINT profile_upload_receipt_file_owned_by_owner
    FOREIGN KEY (file_id, user_id) REFERENCES public.files (id, uploader_id)
    ON DELETE SET NULL (file_id) ON UPDATE CASCADE;
""",
            "receipt match type": """
ALTER TABLE public.profile_upload_receipt DROP CONSTRAINT profile_upload_receipt_file_owned_by_owner;
ALTER TABLE public.profile_upload_receipt ADD CONSTRAINT profile_upload_receipt_file_owned_by_owner
    FOREIGN KEY (file_id, user_id) REFERENCES public.files (id, uploader_id)
    MATCH FULL ON DELETE SET NULL (file_id);
""",
            "receipt SET NULL column list": """
ALTER TABLE public.profile_upload_receipt DROP CONSTRAINT profile_upload_receipt_file_owned_by_owner;
ALTER TABLE public.profile_upload_receipt ADD CONSTRAINT profile_upload_receipt_file_owned_by_owner
    FOREIGN KEY (file_id, user_id) REFERENCES public.files (id, uploader_id) ON DELETE SET NULL;
""",
        }
        for label, mutation in mutations.items():
            with self.subTest(mutation=label):
                reject_69(label, mutation)

    def test_receipt_referenced_key_order_rejects_independently(self) -> None:
        evidence = schema_capture("""
ALTER TABLE public.profile_upload_receipt DROP CONSTRAINT profile_upload_receipt_file_owned_by_owner;
CREATE UNIQUE INDEX files_receipt_referenced_order_probe_idx ON public.files (uploader_id, id);
ALTER TABLE public.profile_upload_receipt ADD CONSTRAINT profile_upload_receipt_file_owned_by_owner
    FOREIGN KEY (file_id, user_id) REFERENCES public.files (uploader_id, id)
    ON DELETE SET NULL (file_id);
""")
        receipt_fk = constraint_evidence(
            evidence,
            "profile_upload_receipt",
            "profile_upload_receipt_file_owned_by_owner",
        )
        self.assertEqual(receipt_fk["columns"], ["file_id", "user_id"])
        self.assertEqual(receipt_fk["referenced_columns"], ["uploader_id", "id"])
        receipt_fk["referenced_index"] = "files_id_uploader_id_key"
        reject_69_evidence("receipt referenced key order", evidence)

    def test_receipt_delete_action_rejects_independently(self) -> None:
        evidence = schema_capture("""
ALTER TABLE public.profile_upload_receipt DROP CONSTRAINT profile_upload_receipt_file_owned_by_owner;
ALTER TABLE public.profile_upload_receipt ADD CONSTRAINT profile_upload_receipt_file_owned_by_owner
    FOREIGN KEY (file_id, user_id) REFERENCES public.files (id, uploader_id) ON DELETE CASCADE;
""")
        receipt_fk = constraint_evidence(
            evidence,
            "profile_upload_receipt",
            "profile_upload_receipt_file_owned_by_owner",
        )
        self.assertEqual(receipt_fk["on_delete"], "CASCADE")
        self.assertEqual(receipt_fk["set_null_columns"], [])
        receipt_fk["set_null_columns"] = ["file_id"]
        reject_69_evidence("receipt delete action", evidence)

    def test_receipt_conindid_rejects_independently(self) -> None:
        evidence = schema_capture("""
ALTER TABLE public.user_photos DROP CONSTRAINT user_photos_file_owned_by_owner;
ALTER TABLE public.profile_upload_receipt DROP CONSTRAINT profile_upload_receipt_file_owned_by_owner;
DROP INDEX public.files_id_uploader_id_key;
CREATE UNIQUE INDEX files_owner_conindid_probe_idx ON public.files (id, uploader_id);
ALTER TABLE public.user_photos ADD CONSTRAINT user_photos_file_owned_by_owner
    FOREIGN KEY (file_id, user_id) REFERENCES public.files (id, uploader_id) ON DELETE RESTRICT;
ALTER TABLE public.profile_upload_receipt ADD CONSTRAINT profile_upload_receipt_file_owned_by_owner
    FOREIGN KEY (file_id, user_id) REFERENCES public.files (id, uploader_id) ON DELETE SET NULL (file_id);
CREATE UNIQUE INDEX files_id_uploader_id_key ON public.files (id, uploader_id);
""")
        receipt_fk = constraint_evidence(
            evidence,
            "profile_upload_receipt",
            "profile_upload_receipt_file_owned_by_owner",
        )
        self.assertEqual(receipt_fk["referenced_index"], "files_owner_conindid_probe_idx")
        owner_fk = constraint_evidence(
            evidence,
            "user_photos",
            "user_photos_file_owned_by_owner",
        )
        self.assertEqual(owner_fk["referenced_index"], "files_owner_conindid_probe_idx")
        owner_fk["referenced_index"] = "files_id_uploader_id_key"
        reject_69_evidence("receipt conindid", evidence)

    def test_pointer_conindid_rejects_independently(self) -> None:
        evidence = schema_capture("""
ALTER TABLE public.profile_photo_state DROP CONSTRAINT profile_photo_state_current_is_own_gallery_entry;
ALTER TABLE public.user_photos DROP CONSTRAINT user_photos_pkey;
CREATE UNIQUE INDEX user_photos_pointer_conindid_probe_idx ON public.user_photos (user_id, file_id);
ALTER TABLE public.profile_photo_state ADD CONSTRAINT profile_photo_state_current_is_own_gallery_entry
    FOREIGN KEY (user_id, current_file_id) REFERENCES public.user_photos (user_id, file_id) ON DELETE RESTRICT;
ALTER TABLE public.user_photos ADD CONSTRAINT user_photos_pkey PRIMARY KEY (user_id, file_id);
""")
        pointer_fk = constraint_evidence(
            evidence,
            "profile_photo_state",
            "profile_photo_state_current_is_own_gallery_entry",
        )
        self.assertEqual(pointer_fk["referenced_index"], "user_photos_pointer_conindid_probe_idx")
        restore_expected_index_catalog(evidence, "user_photos")
        reject_69_evidence("pointer conindid", evidence)

    def test_constraint_names_keys_columns_and_index_sets_reject(self) -> None:
        mutations = {
            "missing ownership FK": "ALTER TABLE public.user_photos DROP CONSTRAINT user_photos_file_owned_by_owner;",
            "extra auth key FK": "ALTER TABLE public.profile_delete_operation ADD CONSTRAINT profile_delete_operation_auth_key_probe_fkey FOREIGN KEY (auth_key_id) REFERENCES public.users (id);",
            "extra historical file FK": "ALTER TABLE public.profile_delete_operation ADD CONSTRAINT profile_delete_operation_target_file_probe_fkey FOREIGN KEY (target_file_id) REFERENCES public.files (id) ON DELETE SET NULL;",
            "unvalidated FK": "ALTER TABLE public.profile_delete_operation ADD CONSTRAINT profile_delete_operation_probe_fkey FOREIGN KEY (auth_key_id) REFERENCES public.users (id) NOT VALID;",
            "reordered primary key": "ALTER TABLE public.profile_delete_operation DROP CONSTRAINT profile_delete_operation_pkey; ALTER TABLE public.profile_delete_operation ADD CONSTRAINT profile_delete_operation_pkey PRIMARY KEY (auth_key_id, user_id, session_id, msg_id);",
            "reordered unique key": "ALTER TABLE public.user_photos DROP CONSTRAINT user_photos_user_id_client_file_id_key; ALTER TABLE public.user_photos ADD CONSTRAINT user_photos_user_id_client_file_id_key UNIQUE (client_file_id, user_id);",
            "missing unique key": "ALTER TABLE public.user_photos DROP CONSTRAINT user_photos_file_id_key;",
            "wrong column type": "ALTER TABLE public.profile_upload_receipt ALTER COLUMN part_count TYPE BIGINT;",
            "wrong nullability": "ALTER TABLE public.profile_photo_state ALTER COLUMN current_file_id SET NOT NULL;",
            "wrong default": "ALTER TABLE public.profile_photo_state ALTER COLUMN mutation_revision SET DEFAULT 1;",
            "extra column": "ALTER TABLE public.profile_delete_operation ADD COLUMN unapproved BIGINT;",
            "extra table index": "CREATE INDEX user_photos_unapproved_probe_idx ON public.user_photos (created_at);",
        }
        for label, mutation in mutations.items():
            with self.subTest(mutation=label):
                reject_69(label, mutation)

    def test_migration_68_index_metadata_mutations_reject(self) -> None:
        mutations = {
            "non-unique": "ALTER TABLE public.user_photos DROP CONSTRAINT user_photos_file_owned_by_owner; ALTER TABLE public.profile_upload_receipt DROP CONSTRAINT profile_upload_receipt_file_owned_by_owner; DROP INDEX public.files_id_uploader_id_key; CREATE INDEX files_id_uploader_id_key ON public.files (id, uploader_id);",
            "reordered columns": "ALTER TABLE public.user_photos DROP CONSTRAINT user_photos_file_owned_by_owner; ALTER TABLE public.profile_upload_receipt DROP CONSTRAINT profile_upload_receipt_file_owned_by_owner; DROP INDEX public.files_id_uploader_id_key; CREATE UNIQUE INDEX files_id_uploader_id_key ON public.files (uploader_id, id);",
            "partial": "ALTER TABLE public.user_photos DROP CONSTRAINT user_photos_file_owned_by_owner; ALTER TABLE public.profile_upload_receipt DROP CONSTRAINT profile_upload_receipt_file_owned_by_owner; DROP INDEX public.files_id_uploader_id_key; CREATE UNIQUE INDEX files_id_uploader_id_key ON public.files (id, uploader_id) WHERE uploader_id IS NOT NULL;",
            "expression": "ALTER TABLE public.user_photos DROP CONSTRAINT user_photos_file_owned_by_owner; ALTER TABLE public.profile_upload_receipt DROP CONSTRAINT profile_upload_receipt_file_owned_by_owner; DROP INDEX public.files_id_uploader_id_key; CREATE UNIQUE INDEX files_id_uploader_id_key ON public.files (id, (uploader_id + 0));",
            "extra index": "CREATE INDEX files_unapproved_probe_idx ON public.files (size);",
            "renamed index": "ALTER TABLE public.user_photos DROP CONSTRAINT user_photos_file_owned_by_owner; ALTER TABLE public.profile_upload_receipt DROP CONSTRAINT profile_upload_receipt_file_owned_by_owner; ALTER INDEX public.files_id_uploader_id_key RENAME TO files_id_uploader_id_probe_idx;",
        }
        for label, mutation in mutations.items():
            with self.subTest(mutation=label):
                reject_68(label, mutation)

    def test_inert_surface_query_detects_each_table_row(self) -> None:
        setup = """
INSERT INTO public.users (id, phone) VALUES (899999, 'rustfs-schema-probe');
INSERT INTO public.files (id, uploader_id, access_hash, size, mime_type, file_name)
VALUES (899999, 899999, 1, 1, 'application/octet-stream', 'probe');
"""
        row_mutations = {
            "user_photos": setup + "INSERT INTO public.user_photos (user_id, file_id, client_file_id) VALUES (899999, 899999, 899998);",
            "profile_photo_state": setup + "INSERT INTO public.profile_photo_state (user_id) VALUES (899999);",
            "profile_upload_receipt": setup + "INSERT INTO public.profile_upload_receipt (user_id, client_file_id, state, request_size, part_count, payload_digest, media_mode) VALUES (899999, 899998, 0, 0, 0, decode(repeat('00', 32), 'hex'), 'photo');",
            "profile_delete_operation": setup + "INSERT INTO public.profile_delete_operation (user_id, auth_key_id, session_id, msg_id, operation_key) VALUES (899999, 1, 1, 1, decode(repeat('00', 16), 'hex'));",
        }
        if RELEASE_SET == "60-70":
            row_mutations.update(
                {
                    "erasure_outbox": "INSERT INTO public.erasure_outbox (operation_key, epoch, stream_id, seq, kind, record) VALUES (decode(repeat('00', 16), 'hex'), 1, decode(repeat('11', 16), 'hex'), 1, 1, decode('01', 'hex'));",
                    "erasure_epoch": "INSERT INTO public.erasure_epoch (epoch, lineage_id) VALUES (1, decode(repeat('11', 16), 'hex'));",
                    "erasure_epoch_completion": "INSERT INTO public.erasure_epoch (epoch, lineage_id) VALUES (1, decode(repeat('11', 16), 'hex')); INSERT INTO public.erasure_epoch_completion (epoch, lineage_id) VALUES (1, decode(repeat('11', 16), 'hex'));",
                }
            )
        expected_surfaces = GATE_MODULE[
            "R70_INERT_SURFACES" if RELEASE_SET == "60-70" else "INERT_SURFACES"
        ]
        for table_name, mutation in row_mutations.items():
            with self.subTest(table=table_name):
                result = inert_capture(mutation)
                self.assertIs(result[table_name], True)
                expected_nonempty = {table_name}
                if table_name == "erasure_epoch_completion":
                    expected_nonempty.add("erasure_epoch")
                self.assertEqual(
                    {name for name, present in result.items() if present},
                    expected_nonempty,
                )
                with self.assertRaisesRegex(GATE_REJECT, "reference_coverage"):
                    GATE_MODULE["validate_inert_surfaces"]({"inert_surfaces": result}, expected_surfaces)

    def test_r70_schema_sql_mutations_reject(self) -> None:
        self.assertEqual(RELEASE_SET, "60-70")
        mutations = {
            "operation key type": "ALTER TABLE public.erasure_outbox ALTER COLUMN operation_key TYPE text USING encode(operation_key, 'hex');",
            "nullability": "ALTER TABLE public.erasure_outbox ALTER COLUMN kind DROP NOT NULL;",
            "default": "ALTER TABLE public.erasure_outbox ALTER COLUMN seq SET DEFAULT 1;",
            "sequence default": "CREATE SEQUENCE public.r70_seq_probe; ALTER TABLE public.erasure_outbox ALTER COLUMN seq SET DEFAULT nextval('public.r70_seq_probe');",
            "identity": "ALTER TABLE public.erasure_outbox ALTER COLUMN seq ADD GENERATED BY DEFAULT AS IDENTITY;",
            "sequence ownership": "CREATE SEQUENCE public.r70_seq_probe; ALTER SEQUENCE public.r70_seq_probe OWNED BY public.erasure_outbox.seq;",
            "primary key order": "ALTER TABLE public.erasure_outbox DROP CONSTRAINT erasure_outbox_pkey; ALTER TABLE public.erasure_outbox ADD CONSTRAINT erasure_outbox_pkey PRIMARY KEY (stream_id, epoch, seq);",
            "unique missing": "ALTER TABLE public.erasure_outbox DROP CONSTRAINT erasure_outbox_operation_key_unique;",
            "completion fk missing": "ALTER TABLE public.erasure_epoch_completion DROP CONSTRAINT erasure_epoch_completion_marker_exists;",
            "completion fk keys swapped": "ALTER TABLE public.erasure_epoch_completion DROP CONSTRAINT erasure_epoch_completion_marker_exists; CREATE UNIQUE INDEX r70_epoch_swapped_key ON public.erasure_epoch (lineage_id, epoch); ALTER TABLE public.erasure_epoch_completion ADD CONSTRAINT erasure_epoch_completion_marker_exists FOREIGN KEY (lineage_id, epoch) REFERENCES public.erasure_epoch (lineage_id, epoch) ON DELETE RESTRICT;",
            "completion delete cascade": "ALTER TABLE public.erasure_epoch_completion DROP CONSTRAINT erasure_epoch_completion_marker_exists; ALTER TABLE public.erasure_epoch_completion ADD CONSTRAINT erasure_epoch_completion_marker_exists FOREIGN KEY (epoch, lineage_id) REFERENCES public.erasure_epoch (epoch, lineage_id) ON DELETE CASCADE;",
            "completion delete no action": "ALTER TABLE public.erasure_epoch_completion DROP CONSTRAINT erasure_epoch_completion_marker_exists; ALTER TABLE public.erasure_epoch_completion ADD CONSTRAINT erasure_epoch_completion_marker_exists FOREIGN KEY (epoch, lineage_id) REFERENCES public.erasure_epoch (epoch, lineage_id) ON DELETE NO ACTION;",
            "completion update action": "ALTER TABLE public.erasure_epoch_completion DROP CONSTRAINT erasure_epoch_completion_marker_exists; ALTER TABLE public.erasure_epoch_completion ADD CONSTRAINT erasure_epoch_completion_marker_exists FOREIGN KEY (epoch, lineage_id) REFERENCES public.erasure_epoch (epoch, lineage_id) ON DELETE RESTRICT ON UPDATE CASCADE;",
            "completion match type": "ALTER TABLE public.erasure_epoch_completion DROP CONSTRAINT erasure_epoch_completion_marker_exists; ALTER TABLE public.erasure_epoch_completion ADD CONSTRAINT erasure_epoch_completion_marker_exists FOREIGN KEY (epoch, lineage_id) REFERENCES public.erasure_epoch (epoch, lineage_id) MATCH FULL ON DELETE RESTRICT;",
            "completion deferrable": "ALTER TABLE public.erasure_epoch_completion DROP CONSTRAINT erasure_epoch_completion_marker_exists; ALTER TABLE public.erasure_epoch_completion ADD CONSTRAINT erasure_epoch_completion_marker_exists FOREIGN KEY (epoch, lineage_id) REFERENCES public.erasure_epoch (epoch, lineage_id) ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED;",
            "completion referenced index": "ALTER TABLE public.erasure_epoch_completion DROP CONSTRAINT erasure_epoch_completion_marker_exists; ALTER TABLE public.erasure_epoch DROP CONSTRAINT erasure_epoch_pkey; CREATE UNIQUE INDEX r70_epoch_reference_probe_key ON public.erasure_epoch (epoch, lineage_id); ALTER TABLE public.erasure_epoch_completion ADD CONSTRAINT erasure_epoch_completion_marker_exists FOREIGN KEY (epoch, lineage_id) REFERENCES public.erasure_epoch (epoch, lineage_id) ON DELETE RESTRICT; ALTER TABLE public.erasure_epoch ADD CONSTRAINT erasure_epoch_pkey PRIMARY KEY (epoch, lineage_id);",
            "outbox epoch fk": "ALTER TABLE public.erasure_outbox ADD CONSTRAINT erasure_outbox_epoch_probe_fkey FOREIGN KEY (epoch, stream_id) REFERENCES public.erasure_epoch (epoch, lineage_id);",
            "files fk": "ALTER TABLE public.erasure_outbox ADD CONSTRAINT erasure_outbox_files_probe_fkey FOREIGN KEY (epoch) REFERENCES public.files (id);",
            "inbound fk": "ALTER TABLE public.files ADD COLUMN erasure_epoch BIGINT; ALTER TABLE public.files ADD COLUMN erasure_lineage BYTEA; ALTER TABLE public.files ADD CONSTRAINT files_erasure_epoch_probe_fkey FOREIGN KEY (erasure_epoch, erasure_lineage) REFERENCES public.erasure_epoch (epoch, lineage_id);",
            "check added": "ALTER TABLE public.erasure_epoch ADD CONSTRAINT erasure_epoch_probe_check CHECK (epoch >= 1);",
            "check renamed": "ALTER TABLE public.erasure_epoch DROP CONSTRAINT erasure_epoch_epoch_check; ALTER TABLE public.erasure_epoch ADD CONSTRAINT erasure_epoch_epoch_probe_check CHECK (epoch >= 1);",
        }
        for table_name in GATE_MODULE["R70_COLUMNS"]:
            mutations[f"trigger on {table_name}"] = (
                "CREATE FUNCTION public.r70_probe_trigger() RETURNS trigger LANGUAGE plpgsql AS "
                "$$ BEGIN RETURN NEW; END $$; "
                f"CREATE TRIGGER r70_probe_trigger BEFORE INSERT ON public.{table_name} "
                "FOR EACH ROW EXECUTE FUNCTION public.r70_probe_trigger();"
            )
        for label, mutation in mutations.items():
            with self.subTest(mutation=label):
                reject_70(label, mutation)


if __name__ == "__main__":
    if len(sys.argv) in {3, 4} and sys.argv[1] == "--prepare-atlas-input":
        release_set = sys.argv[3] if len(sys.argv) == 4 else "60-69"
        prepare_atlas_input(Path(sys.argv[2]), release_set)
        print(f"atlas_input=verified frozen_release={release_set} mode=0700")
        raise SystemExit(0)
    if os.geteuid() != 0:
        raise SystemExit("run RustFS PostgreSQL qualification as root")
    if not POSTGRES_CONTAINER:
        raise SystemExit("RUSTFS_POSTGRES_CONTAINER is required")
    if not ATLAS_INPUT:
        raise SystemExit("RUSTFS_ATLAS_INPUT is required")
    if RELEASE_SET not in {"60-69", "60-70"}:
        raise SystemExit("RUSTFS_RELEASE_SET must be 60-69 or 60-70")

    test_names = unittest.defaultTestLoader.getTestCaseNames(RustFSCatalogQualification)
    baseline_name = "test_actual_atlas_directory_and_postgres_catalog_pass"
    if baseline_name not in test_names:
        raise SystemExit(f"passing {RELEASE_SET} PostgreSQL baseline test is missing")
    suite = unittest.TestSuite([RustFSCatalogQualification(baseline_name)])
    suite.addTests(
        RustFSCatalogQualification(name)
        for name in test_names
        if name != baseline_name
        and not (RELEASE_SET == "60-70" and name.startswith("test_fixed_"))
        and not (RELEASE_SET == "60-70" and name == "test_live_transition_query_validates_atlas_revision_hashes")
        and not (RELEASE_SET == "60-69" and name.startswith("test_r70_"))
    )
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    print(
        f"qualification_scope=success proves frozen {RELEASE_SET} inputs; "
        "schema_at_head=not_qualified; rollout_approval=not_granted"
    )
    print(
        "postgres16_schema_summary="
        f"passed:{result.testsRun - len(result.failures) - len(result.errors)} "
        f"failed:{len(result.failures) + len(result.errors)}"
    )
    raise SystemExit(0 if result.wasSuccessful() else 1)
