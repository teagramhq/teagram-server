#!/usr/bin/env python3
from __future__ import annotations

import hashlib
import json
import os
import runpy
import subprocess
import unittest
from pathlib import Path
from typing import Any


SCRIPT_DIR = Path(__file__).resolve().parent
PROJECT_ROOT = SCRIPT_DIR.parents[2]
GATE = SCRIPT_DIR / "qualify-rustfs-transition.py"
SCHEMA_QUERY = SCRIPT_DIR / "rustfs-schema-capture.sql"
INERT_QUERY = SCRIPT_DIR / "rustfs-inert-surfaces.sql"
POSTGRES_CONTAINER = os.environ.get("R69_POSTGRES_CONTAINER", "")
GATE_MODULE = runpy.run_path(str(GATE), run_name="qualify_module")
GATE_REJECT = GATE_MODULE["GateReject"]
RELEASE = GATE_MODULE["RELEASES"]["60-69"]


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
            "telegram",
        ],
        input=sql,
        text=True,
        capture_output=True,
        check=False,
    )
    if result.returncode != 0:
        raise AssertionError("PostgreSQL 16 catalog capture failed")
    return [line for line in result.stdout.splitlines() if line]


def capture(query_path: Path, mutation: str = "") -> dict[str, Any]:
    query = query_path.read_text(encoding="utf-8")
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
    return capture(SCHEMA_QUERY, mutation)


def inert_capture(mutation: str = "") -> dict[str, Any]:
    return capture(INERT_QUERY, mutation)


def validate_68(evidence: dict[str, Any]) -> None:
    GATE_MODULE["validate_migration_68_schema"](evidence)


def validate_69(evidence: dict[str, Any]) -> None:
    GATE_MODULE["validate_migration_69_schema"](evidence)


def reject_68(label: str, mutation: str) -> None:
    evidence = schema_capture(mutation)
    try:
        validate_68(evidence)
    except GATE_REJECT:
        return
    raise AssertionError(f"PostgreSQL mutation was not rejected by migration 68 qualification: {label}")


def reject_69(label: str, mutation: str) -> None:
    evidence = schema_capture(mutation)
    try:
        validate_69(evidence)
    except GATE_REJECT:
        return
    raise AssertionError(f"PostgreSQL mutation was not rejected by migration 69 qualification: {label}")


class RustFSCatalogQualification(unittest.TestCase):
    def test_actual_atlas_directory_and_postgres_catalog_pass(self) -> None:
        migrations = PROJECT_ROOT / "migrations"
        expected_names = set(RELEASE["files"])
        actual_names = {
            path.name
            for path in migrations.iterdir()
            if path.is_file() and path.name[:14] >= "20261005000060" and path.suffix == ".sql"
        }
        self.assertEqual(actual_names, expected_names)
        atlas_bytes = (migrations / "atlas.sum").read_bytes()
        self.assertEqual(hashlib.sha256(atlas_bytes).hexdigest(), RELEASE["atlas_sum_sha256"])
        for name, digest in RELEASE["file_sha256"].items():
            self.assertEqual(hashlib.sha256((migrations / name).read_bytes()).hexdigest(), digest)

        evidence = schema_capture()
        validate_68(evidence)
        validate_69(evidence)
        inert = inert_capture()
        self.assertEqual(
            inert,
            {
                "user_photos": False,
                "profile_photo_state": False,
                "profile_upload_receipt": False,
                "profile_delete_operation": False,
            },
        )
        GATE_MODULE["validate_inert_surfaces"]({"inert_surfaces": inert})
        self.assertEqual(psql("SELECT (NOT EXISTS (SELECT 1 FROM public.files))::text;\n"), ["true"])

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
            "receipt SET NULL column list": """
ALTER TABLE public.profile_upload_receipt DROP CONSTRAINT profile_upload_receipt_file_owned_by_owner;
ALTER TABLE public.profile_upload_receipt ADD CONSTRAINT profile_upload_receipt_file_owned_by_owner
    FOREIGN KEY (file_id, user_id) REFERENCES public.files (id, uploader_id) ON DELETE SET NULL;
""",
        }
        for label, mutation in mutations.items():
            with self.subTest(mutation=label):
                reject_69(label, mutation)

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
        for table_name, mutation in row_mutations.items():
            with self.subTest(table=table_name):
                result = inert_capture(mutation)
                self.assertIs(result[table_name], True)
                self.assertEqual({name for name, present in result.items() if present}, {table_name})
                with self.assertRaisesRegex(GATE_REJECT, "reference_coverage"):
                    GATE_MODULE["validate_inert_surfaces"]({"inert_surfaces": result})


if __name__ == "__main__":
    if not POSTGRES_CONTAINER:
        raise SystemExit("R69_POSTGRES_CONTAINER is required")
    suite = unittest.defaultTestLoader.loadTestsFromTestCase(RustFSCatalogQualification)
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    print(
        "postgres16_schema_summary="
        f"passed:{result.testsRun - len(result.failures) - len(result.errors)} "
        f"failed:{len(result.failures) + len(result.errors)}"
    )
    raise SystemExit(0 if result.wasSuccessful() else 1)
