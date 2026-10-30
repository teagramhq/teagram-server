#!/usr/bin/env python3
"""Regression tests for the migration lock-risk policy."""

import unittest
from pathlib import Path

from migration_lock_lint import lint_migration


FIXTURE_DIR = Path(__file__).parent / "testdata/migrations/atlas-lint"


class MigrationLockLintTest(unittest.TestCase):
    def test_rejects_non_concurrent_index_on_existing_table(self) -> None:
        sql = "CREATE INDEX users_phone_idx ON users (phone);"
        self.assertIn("CREATE INDEX", lint_migration(sql)[0])

    def test_concurrent_atlas_lint_fixture_uses_allowed_pattern(self) -> None:
        migration = FIXTURE_DIR / "additive/20260103000000_add_users_email_index.sql"
        self.assertEqual([], lint_migration(migration.read_text(encoding="utf-8")))

    def test_blocking_atlas_lint_fixture_is_rejected(self) -> None:
        migration = FIXTURE_DIR / "blocking/20260102000000_lock_users.sql"
        self.assertIn("LOCK TABLE", lint_migration(migration.read_text(encoding="utf-8"))[0])

    def test_allows_index_on_table_created_by_the_migration(self) -> None:
        sql = "CREATE TABLE new_table (id BIGINT PRIMARY KEY); CREATE INDEX new_table_id_idx ON new_table (id);"
        self.assertEqual([], lint_migration(sql))

    def test_allows_concurrent_index(self) -> None:
        sql = "CREATE INDEX CONCURRENTLY users_phone_idx ON users (phone);"
        self.assertEqual([], lint_migration(sql))

    def test_normal_additive_column_passes(self) -> None:
        sql = "ALTER TABLE users ADD COLUMN display_name TEXT NOT NULL DEFAULT '';"
        self.assertEqual([], lint_migration(sql))

    def test_lock_opt_out_requires_a_reason(self) -> None:
        sql = "-- atlas:nolint blocking\nCREATE INDEX users_phone_idx ON users (phone);"
        self.assertIn("requires a non-empty", lint_migration(sql)[0])

    def test_lock_opt_out_with_reason_passes(self) -> None:
        sql = (
            "-- atlas:nolint blocking\n"
            "-- atlas:lock-waiver: small table during the reviewed transition\n"
            "CREATE INDEX users_phone_idx ON users (phone);"
        )
        self.assertEqual([], lint_migration(sql))

    def test_ignores_lock_examples_in_comments_and_strings(self) -> None:
        sql = (
            "-- LOCK TABLE users IN SHARE MODE;\n"
            "-- CREATE INDEX example_idx ON users (phone);\n"
            "SELECT 'LOCK TABLE users IN SHARE MODE';\n"
            "SELECT 'DROP INDEX example_idx';"
        )
        self.assertEqual([], lint_migration(sql))

    def test_rejects_type_rewrites(self) -> None:
        sql = "ALTER TABLE users ALTER COLUMN phone TYPE VARCHAR(64);"
        self.assertIn("changing a column type", lint_migration(sql)[0])

    def test_fails_closed_for_procedural_ddl_in_do_blocks(self) -> None:
        sql = """DO $migration$
BEGIN
    EXECUTE 'CREATE INDEX users_phone_idx ON users (phone)';
END;
$migration$;
"""
        self.assertIn("DO blocks can hide procedural DDL", lint_migration(sql)[0])

    def test_does_not_flag_dollar_quoted_function_body(self) -> None:
        sql = """CREATE FUNCTION example() RETURNS trigger
LANGUAGE plpgsql AS $function$
BEGIN
    RETURN NEW;
END;
$function$;
"""
        self.assertEqual([], lint_migration(sql))

    def test_ignores_do_examples_in_comments_and_strings(self) -> None:
        sql = "-- DO $migration$ BEGIN EXECUTE 'CREATE INDEX'; END $migration$;\nSELECT 'DO $migration$';"
        self.assertEqual([], lint_migration(sql))


if __name__ == "__main__":
    unittest.main()
