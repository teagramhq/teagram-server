from __future__ import annotations

import contextlib
import hashlib
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from textwrap import dedent
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

    def test_precommit_formats_staged_sql_and_preserves_worktree_edits(self) -> None:
        with tempfile.TemporaryDirectory() as tempdir:
            repo = Path(tempdir) / "repo"
            repo.mkdir()
            for relative in (".githooks", "bin", "internal/store/queries", "migrations"):
                (repo / relative).mkdir(parents=True, exist_ok=True)

            def git(*args: str) -> str:
                return subprocess.run(
                    ["git", *args],
                    cwd=repo,
                    check=True,
                    text=True,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                ).stdout

            git("init", "--quiet")
            git("config", "user.name", "SQL hook fixture")
            git("config", "user.email", "sql-hook-fixture@example.invalid")
            empty_hooks = repo / "empty-hooks"
            empty_hooks.mkdir()
            git("config", "core.hooksPath", str(empty_hooks))
            shutil.copyfile(REPO_ROOT / ".githooks/pre-commit", repo / ".githooks/pre-commit")
            shutil.copymode(REPO_ROOT / ".githooks/pre-commit", repo / ".githooks/pre-commit")
            shutil.copyfile(REPO_ROOT / ".githooks/commit-msg", repo / ".githooks/commit-msg")
            shutil.copymode(REPO_ROOT / ".githooks/commit-msg", repo / ".githooks/commit-msg")
            shutil.copyfile(REPO_ROOT / ".sqlfluff", repo / ".sqlfluff")
            shutil.copyfile(REPO_ROOT / "Makefile", repo / "Makefile")
            (repo / ".golangci.yml").write_text("fixture: true\n", encoding="utf-8")

            sqlc = repo / "bin/sqlc"
            sqlc.write_text(
                "#!/usr/bin/env python3\n"
                "from pathlib import Path\n"
                "source = Path('internal/store/queries/source.sql').read_text(encoding='utf-8')\n"
                "output = Path('internal/store/db/source.sql.go')\n"
                "output.parent.mkdir(parents=True, exist_ok=True)\n"
                "output.write_text(source, encoding='utf-8')\n",
                encoding="utf-8",
            )
            sqlc.chmod(0o755)

            venv_bin = repo / ".venv-sqlfluff/bin"
            venv_bin.mkdir(parents=True)
            (venv_bin / "sqlfluff").symlink_to(Path(self.sqlfluff).resolve())
            (venv_bin / "python").symlink_to(Path(sys.executable).resolve())

            example_go = repo / "internal/example/example.go"
            example_go.parent.mkdir(parents=True)
            source_sql = repo / "internal/store/queries/source.sql"
            source_sql.write_text("SELECT 0;\n", encoding="utf-8")
            format_sql = repo / "internal/store/queries/format.sql"
            format_sql.write_text("SELECT 0;\n", encoding="utf-8")
            migration = repo / "migrations/20261031000000_hook_test.sql"
            migration.write_text("CREATE TABLE hook_test (id integer);\n", encoding="utf-8")
            example_go.write_text("package example\n", encoding="utf-8")
            git("add", "--all")
            git("commit", "--quiet", "-m", "chore(test): create hook fixture")
            git("config", "core.hooksPath", str(repo / ".githooks"))

            example_go.write_text("package example\n// format_me\n// autofix_me\n", encoding="utf-8")
            source_sql.write_text("SELECT 2;\n", encoding="utf-8")
            format_sql.write_text("select 1;\n", encoding="utf-8")
            migration.write_text("CREATE TABLE hook_test (id bigint);\n", encoding="utf-8")
            git("add", "internal/example/example.go", "internal/store/queries", "migrations")
            source_sql.write_text("SELECT 2;\n-- unstaged query note\n", encoding="utf-8")
            example_go.write_text(
                "package example\n// format_me\n// autofix_me\n// unstaged go note\n",
                encoding="utf-8",
            )
            migration.write_text(
                "CREATE TABLE hook_test (id integer);\n-- unstaged migration note\n",
                encoding="utf-8",
            )

            tools = Path(tempdir) / "tools"
            tools.mkdir()
            (tools / "golangci-lint").write_text(
                dedent(
                    """\
                    #!/usr/bin/env python3
                    import pathlib
                    import sys

                    if sys.argv[1] == "fmt":
                        sys.stdout.write(sys.stdin.read().replace("format_me", "format_fixed"))
                    elif sys.argv[1] == "run":
                        path = pathlib.Path("example.go")
                        path.write_text(path.read_text().replace("autofix_me", "autofix_fixed"))
                    else:
                        raise SystemExit(2)
                    """
                ),
                encoding="utf-8",
            )
            (tools / "golangci-lint").chmod(0o755)
            (tools / "atlas").write_text(
                dedent(
                    """\
                    #!/usr/bin/env python3
                    import hashlib
                    from pathlib import Path

                    lines = []
                    for path in sorted(Path("migrations").glob("*.sql")):
                        digest = hashlib.sha256(path.read_bytes()).hexdigest()
                        lines.append(f"{digest} {path.as_posix()}")
                    Path("migrations/atlas.sum").write_text("\\n".join(lines) + "\\n")
                    """
                ),
                encoding="utf-8",
            )
            (tools / "atlas").chmod(0o755)

            environment = os.environ.copy()
            environment["PATH"] = f"{tools}{os.pathsep}{environment['PATH']}"
            commit = subprocess.run(
                ["git", "commit", "--quiet", "-m", "chore(test): run staged hooks"],
                cwd=repo,
                env=environment,
                check=False,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
            )
            self.assertEqual(commit.returncode, 0, commit.stdout)

            def committed(path: str) -> str:
                return git("show", f"HEAD:{path}")

            formatted_sql = committed("internal/store/queries/format.sql")
            self.assertIn("SELECT 1;", formatted_sql, commit.stdout)
            self.assertNotIn("select 1;", formatted_sql, commit.stdout)
            self.assertNotIn("unstaged query note", committed("internal/store/queries/source.sql"))
            self.assertIn("unstaged query note", source_sql.read_text(encoding="utf-8"))

            staged_go = committed("internal/example/example.go")
            self.assertIn("format_fixed", staged_go)
            self.assertIn("autofix_fixed", staged_go)
            self.assertNotIn("unstaged go note", staged_go)
            self.assertIn("unstaged go note", example_go.read_text(encoding="utf-8"))

            generated = committed("internal/store/db/source.sql.go")
            staged_source = committed("internal/store/queries/source.sql")
            self.assertEqual(generated, staged_source)
            self.assertNotIn("unstaged query note", generated)

            staged_migration_path = "migrations/20261031000000_hook_test.sql"
            staged_migration = committed(staged_migration_path)
            expected_hash = hashlib.sha256(staged_migration.encode()).hexdigest()
            self.assertIn(
                f"{expected_hash} {staged_migration_path}", committed("migrations/atlas.sum")
            )
            self.assertIn("unstaged migration note", migration.read_text(encoding="utf-8"))

            lint = subprocess.run(
                ["make", "lint-sql"],
                cwd=repo,
                check=False,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
            )
            self.assertEqual(lint.returncode, 0, lint.stdout)


if __name__ == "__main__":
    unittest.main()
