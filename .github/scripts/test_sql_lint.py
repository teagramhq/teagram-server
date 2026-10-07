from __future__ import annotations

import contextlib
import io
import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from sql_lint import changed_migrations, lint_changed_sql


REPO_ROOT = Path(__file__).resolve().parents[2]


class MigrationSelectionTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tempdir = tempfile.TemporaryDirectory()
        self.addCleanup(self.tempdir.cleanup)
        self.repo = Path(self.tempdir.name) / "repo"
        self.repo.mkdir()
        self.git("init", "--quiet")
        self.git("config", "user.name", "SQL lint fixture")
        self.git("config", "user.email", "sql-lint-fixture@example.invalid")
        (self.repo / "migrations").mkdir()
        (self.repo / "internal/store/queries").mkdir(parents=True)
        (self.repo / "migrations/20260101000000_existing.sql").write_text(
            "SELECT 1;\n", encoding="utf-8"
        )
        (self.repo / "migrations/20260105000000_deleted.sql").write_text(
            "SELECT 3;\n", encoding="utf-8"
        )
        (self.repo / "internal/store/queries/query.sql").write_text(
            "SELECT 1;\n", encoding="utf-8"
        )
        self.git("add", "--all")
        self.git("commit", "--quiet", "-m", "fixture base")
        self.base = self.git("rev-parse", "HEAD").strip()
        self.sqlfluff = Path(self.tempdir.name) / "sqlfluff"
        self.write_stub_sqlfluff()
        self.capture = Path(self.tempdir.name) / "sqlfluff-calls.jsonl"
        self.environment = patch.dict(os.environ, {"SQLFLUFF_CAPTURE": str(self.capture)})
        self.environment.start()
        self.addCleanup(self.environment.stop)

    def git(self, *args: str) -> str:
        return subprocess.run(
            ["git", *args],
            cwd=self.repo,
            check=True,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        ).stdout

    def commit(self, message: str) -> str:
        self.git("add", "--all")
        self.git("commit", "--quiet", "-m", message)
        return self.git("rev-parse", "HEAD").strip()

    def write_stub_sqlfluff(self) -> None:
        self.sqlfluff.write_text(
            "#!/usr/bin/env python3\n"
            "import json, os, sys\n"
            "with open(os.environ['SQLFLUFF_CAPTURE'], 'a', encoding='utf-8') as output:\n"
            "    output.write(json.dumps(sys.argv[1:]) + '\\n')\n"
            "failed = os.environ.get('SQLFLUFF_EXIT_CODE') == '1'\n"
            "failed |= os.environ.get('SQLFLUFF_FAIL_ON_MIGRATION') == '1' and any(\n"
            "    arg.startswith('migrations/') for arg in sys.argv[1:]\n"
            ")\n"
            "raise SystemExit(1 if failed else 0)\n",
            encoding="utf-8",
        )
        self.sqlfluff.chmod(0o755)

    def calls(self) -> list[list[str]]:
        if not self.capture.exists():
            return []
        return [json.loads(line) for line in self.capture.read_text(encoding="utf-8").splitlines()]

    def run_lint(self, head: str) -> tuple[int, str]:
        stdout = io.StringIO()
        stderr = io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            result = lint_changed_sql(self.repo, self.base, head, str(self.sqlfluff))
        return result, stdout.getvalue() + stderr.getvalue()

    def test_no_new_migration_still_lints_query_sources(self) -> None:
        (self.repo / "README.md").write_text("query-only change\n", encoding="utf-8")
        head = self.commit("query-only change")

        result, output = self.run_lint(head)

        self.assertEqual(result, 0, output)
        self.assertEqual(
            self.calls(), [["lint", "--config", ".sqlfluff", "internal/store/queries"]]
        )

    def test_every_added_migration_is_linted_individually(self) -> None:
        first = "migrations/20260102000000_first.sql"
        second = "migrations/20260103000000_second.sql"
        (self.repo / first).write_text("SELECT 1;\n", encoding="utf-8")
        (self.repo / second).write_text("SELECT 2;\n", encoding="utf-8")
        (self.repo / "migrations/atlas.sum").write_text("updated checksum\n", encoding="utf-8")
        head = self.commit("add migrations")

        new, historical = changed_migrations(self.repo, self.base, head)
        result, output = self.run_lint(head)

        self.assertEqual(new, [first, second])
        self.assertEqual(historical, [])
        self.assertEqual(result, 0, output)
        self.assertEqual(
            self.calls(),
            [
                ["lint", "--config", ".sqlfluff", "internal/store/queries"],
                ["lint", "--config", ".sqlfluff", first, second],
            ],
        )

    def test_modified_and_deleted_historical_migrations_fail(self) -> None:
        modified = "migrations/20260101000000_existing.sql"
        deleted = "migrations/20260105000000_deleted.sql"
        (self.repo / modified).write_text("SELECT 2;\n", encoding="utf-8")
        (self.repo / deleted).unlink()
        head = self.commit("modify history")

        new, historical = changed_migrations(self.repo, self.base, head)
        result, output = self.run_lint(head)

        self.assertEqual(new, [])
        self.assertEqual(historical, [modified, deleted])
        self.assertNotEqual(result, 0)
        self.assertIn(modified, output)
        self.assertIn(deleted, output)
        self.assertEqual(
            self.calls(), [["lint", "--config", ".sqlfluff", "internal/store/queries"]]
        )

    def test_query_lint_error_cannot_pass(self) -> None:
        (self.repo / "README.md").write_text("query-only change\n", encoding="utf-8")
        head = self.commit("query-only change")

        with patch.dict(os.environ, {"SQLFLUFF_EXIT_CODE": "1"}):
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                result = lint_changed_sql(self.repo, self.base, head, str(self.sqlfluff))

        self.assertNotEqual(result, 0)

    def test_new_migration_lint_error_cannot_pass(self) -> None:
        migration = "migrations/20260102000000_invalid.sql"
        (self.repo / migration).write_text("SELECT 1;\n", encoding="utf-8")
        head = self.commit("add migration")

        with patch.dict(os.environ, {"SQLFLUFF_FAIL_ON_MIGRATION": "1"}):
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                result = lint_changed_sql(self.repo, self.base, head, str(self.sqlfluff))

        self.assertNotEqual(result, 0)
        self.assertEqual(len(self.calls()), 2)


class SQLFluffRuleTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        sqlfluff = os.environ.get("SQLFLUFF_BIN")
        if not sqlfluff or not Path(sqlfluff).is_file():
            raise RuntimeError("SQLFLUFF_BIN must point to the installed SQLFluff binary")
        cls.sqlfluff = sqlfluff

    def lint_fixture_change(self, migration_sql: str | None) -> int:
        with tempfile.TemporaryDirectory() as tempdir:
            repo = Path(tempdir)
            subprocess.run(["git", "init", "--quiet"], cwd=repo, check=True)
            subprocess.run(
                ["git", "config", "user.name", "SQL lint fixture"], cwd=repo, check=True
            )
            subprocess.run(
                ["git", "config", "user.email", "sql-lint-fixture@example.invalid"],
                cwd=repo,
                check=True,
            )
            (repo / "migrations").mkdir()
            (repo / "internal/store/queries").mkdir(parents=True)
            shutil.copyfile(REPO_ROOT / ".sqlfluff", repo / ".sqlfluff")
            (repo / "migrations/20260101000000_existing.sql").write_text(
                "select 1;\n", encoding="utf-8"
            )
            (repo / "internal/store/queries/query.sql").write_text(
                "SELECT 1;\n", encoding="utf-8"
            )

            def git(*args: str) -> str:
                return subprocess.run(
                    ["git", *args],
                    cwd=repo,
                    check=True,
                    text=True,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                ).stdout

            git("add", "--all")
            git("commit", "--quiet", "-m", "fixture base")
            base = git("rev-parse", "HEAD").strip()
            if migration_sql is None:
                (repo / "README.md").write_text("query-only change\n", encoding="utf-8")
            else:
                (repo / "migrations/20260102000000_new.sql").write_text(
                    migration_sql, encoding="utf-8"
                )
            git("add", "--all")
            git("commit", "--quiet", "-m", "fixture change")
            head = git("rev-parse", "HEAD").strip()
            return lint_changed_sql(repo, base, head, self.sqlfluff)

    def lint_fixture(self, sql: str) -> subprocess.CompletedProcess[str]:
        with tempfile.TemporaryDirectory() as tempdir:
            path = Path(tempdir) / "fixture.sql"
            path.write_text(sql, encoding="utf-8")
            return subprocess.run(
                [
                    self.sqlfluff,
                    "lint",
                    "--config",
                    str(REPO_ROOT / ".sqlfluff"),
                    str(path),
                ],
                check=False,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
            )

    def test_clean_sql_passes(self) -> None:
        result = self.lint_fixture("SELECT 1;\n")
        self.assertEqual(result.returncode, 0, result.stdout)

    def test_clean_new_migration_passes_and_historical_sql_is_excluded(self) -> None:
        self.assertEqual(self.lint_fixture_change("SELECT 2;\n"), 0)

    def test_no_new_migration_passes_with_actual_linter(self) -> None:
        self.assertEqual(self.lint_fixture_change(None), 0)

    def test_cp01_violation_in_new_migration_fails(self) -> None:
        self.assertNotEqual(self.lint_fixture_change("select 2;\n"), 0)

    def test_parse_error_in_new_migration_fails(self) -> None:
        self.assertNotEqual(self.lint_fixture_change("SELECT FROM;\n"), 0)

    def test_parse_error_fails(self) -> None:
        result = self.lint_fixture("SELECT FROM;\n")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("PRS", result.stdout)

    def test_capitalization_violation_fails(self) -> None:
        result = self.lint_fixture("select 1;\n")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("CP01", result.stdout)

    def test_long_line_violation_fails(self) -> None:
        result = self.lint_fixture(f"SELECT 1 AS {'a' * 130};\n")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("LT05", result.stdout)


if __name__ == "__main__":
    unittest.main()
