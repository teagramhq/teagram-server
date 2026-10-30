#!/usr/bin/env python3
"""Regression tests for Atlas lint's baseline transaction replay."""

import subprocess
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("prepare_atlas_lint_history.py")


class PrepareAtlasLintHistoryTest(unittest.TestCase):
    def test_wraps_only_baseline_top_level_lock_migrations(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            migrations = Path(directory)
            baseline = migrations / "20260913000039_server_administration.sql"
            baseline.write_text(
                "CREATE TABLE users (id BIGINT);\n"
                "LOCK TABLE users IN SHARE MODE;\n"
                "INSERT INTO users VALUES (1);\n",
                encoding="utf-8",
            )
            changed = migrations / "20261030000001_new_schema.sql"
            changed_contents = "ALTER TABLE users ADD COLUMN name TEXT;\n"
            changed.write_text(changed_contents, encoding="utf-8")

            result = subprocess.run(
                [
                    "python3",
                    str(SCRIPT),
                    "--migrations-dir",
                    str(migrations),
                    "--changed-file",
                    f"migrations/{changed.name}",
                ],
                check=False,
                capture_output=True,
                text=True,
            )

            self.assertEqual(0, result.returncode, result.stderr)
            self.assertEqual(
                "BEGIN;\n"
                "CREATE TABLE users (id BIGINT);\n"
                "LOCK TABLE users IN SHARE MODE;\n"
                "INSERT INTO users VALUES (1);\n"
                "\nCOMMIT;\n",
                baseline.read_text(encoding="utf-8"),
            )
            self.assertEqual(changed_contents, changed.read_text(encoding="utf-8"))

    def test_ignores_lock_statements_inside_function_bodies_and_nontransactional_files(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            migrations = Path(directory)
            function = migrations / "20260913000039_function.sql"
            function_contents = (
                "CREATE FUNCTION lock_users() RETURNS VOID LANGUAGE plpgsql AS $function$\n"
                "BEGIN\n"
                "    LOCK TABLE users IN SHARE MODE;\n"
                "END;\n"
                "$function$;\n"
            )
            function.write_text(function_contents, encoding="utf-8")
            nontransactional = migrations / "20260913000040_nontransactional.sql"
            nontransactional_contents = (
                "-- atlas:txmode none\n"
                "LOCK TABLE users IN SHARE MODE;\n"
            )
            nontransactional.write_text(nontransactional_contents, encoding="utf-8")

            result = subprocess.run(
                ["python3", str(SCRIPT), "--migrations-dir", str(migrations)],
                check=False,
                capture_output=True,
                text=True,
            )

            self.assertEqual(0, result.returncode, result.stderr)
            self.assertEqual(function_contents, function.read_text(encoding="utf-8"))
            self.assertEqual(nontransactional_contents, nontransactional.read_text(encoding="utf-8"))


if __name__ == "__main__":
    unittest.main()
