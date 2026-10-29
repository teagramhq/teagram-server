#!/usr/bin/env python3
"""Regression tests for the migration lock-risk policy."""

import unittest

from migration_lock_lint import lint_migration


class MigrationLockLintTest(unittest.TestCase):
    def test_rejects_non_concurrent_index_on_existing_table(self) -> None:
        sql = "CREATE INDEX users_phone_idx ON users (phone);"
        self.assertIn("CREATE INDEX", lint_migration(sql)[0])

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
        sql = "-- CREATE INDEX example_idx ON users (phone);\nSELECT 'DROP INDEX example_idx';"
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
