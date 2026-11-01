from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
ZERO_SHA = "0" * 40


class PrePushHookTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tempdir = tempfile.TemporaryDirectory()
        self.addCleanup(self.tempdir.cleanup)
        self.root = Path(self.tempdir.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        self.capture = self.root / "calls.log"
        self.tools = self.root / "tools"
        self.tools.mkdir()

        self.git("init", "--quiet")
        self.git("checkout", "--quiet", "-b", "main")
        self.git("config", "user.name", "Git hooks fixture")
        self.git("config", "user.email", "git-hooks-fixture@example.invalid")

        hooks = self.repo / ".githooks"
        hooks.mkdir()
        shutil.copy2(REPO_ROOT / ".githooks/pre-push", hooks / "pre-push")
        self.git("config", "core.hooksPath", ".githooks")

        for path in (
            "internal/example",
            "internal/store/queries",
            "migrations",
            ".venv-sqlfluff/bin",
        ):
            (self.repo / path).mkdir(parents=True, exist_ok=True)

        (self.repo / ".golangci.yml").write_text("fixture: true\n", encoding="utf-8")
        (self.repo / ".sqlfluff").write_text("[sqlfluff]\n", encoding="utf-8")
        (self.repo / "internal/example/example.go").write_text(
            "package example\n", encoding="utf-8"
        )
        (self.repo / "internal/store/queries/query.sql").write_text(
            "SELECT 1;\n", encoding="utf-8"
        )
        (self.repo / "migrations/20260101000000_base.sql").write_text(
            "SELECT 1;\n", encoding="utf-8"
        )

        self.write_tool("golangci-lint")
        self.write_tool("make")
        self.write_tool("python3")
        self.write_tool("docker")
        self.write_tool("atlas")
        self.write_tool("bash")
        self.write_tool("gitleaks")
        self.write_tool("sqlfluff")
        self.write_tool("python")
        self.write_tool("actionlint")
        sqlfluff_dir = self.repo / ".venv-sqlfluff/bin"
        (sqlfluff_dir / "sqlfluff").symlink_to(self.tools / "sqlfluff")
        (sqlfluff_dir / "python").symlink_to(self.tools / "python")

        self.git("add", "--all")
        self.git("commit", "--quiet", "-m", "chore(test): create base")

        self.git("checkout", "--quiet", "-b", "feature")
        (self.repo / "internal/example/example.go").write_text(
            "package example\n\nfunc Changed() {}\n", encoding="utf-8"
        )
        (self.repo / "internal/store/queries/query.sql").write_text(
            "SELECT 2;\n", encoding="utf-8"
        )
        (self.repo / "migrations/20261031000000_change.sql").write_text(
            "CREATE TABLE changed (id integer);\n", encoding="utf-8"
        )
        self.git("add", "--all")
        self.git("commit", "--quiet", "-m", "chore(test): update checked paths")
        self.feature_sha = self.git("rev-parse", "HEAD").strip()

    def git(self, *args: str) -> str:
        return subprocess.run(
            ["git", *args],
            cwd=self.repo,
            check=True,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        ).stdout

    def write_tool(self, name: str) -> None:
        path = self.tools / name
        path.write_text(
            "#!/bin/sh\n"
            'tool="${0##*/}"\n'
            'printf \'%s\\t%s\\t%s\\n\' "$tool" "$PWD" "$*" >> "$HOOK_CAPTURE"\n'
            '[ "${HOOK_FAIL:-}" != "$tool" ] || exit 23\n',
            encoding="utf-8",
        )
        path.chmod(0o755)

    def run_hook(
        self,
        *,
        local_sha: str | None = None,
        remote_sha: str = ZERO_SHA,
        ref_name: str = "feature",
        fail_tool: str = "",
    ) -> subprocess.CompletedProcess[str]:
        self.capture.unlink(missing_ok=True)
        environment = os.environ.copy()
        environment["PATH"] = f"{self.tools}{os.pathsep}{environment['PATH']}"
        environment["HOOK_CAPTURE"] = str(self.capture)
        environment["GIT_HOOK_BASE_REF"] = "main"
        if fail_tool:
            environment["HOOK_FAIL"] = fail_tool
        else:
            environment.pop("HOOK_FAIL", None)
        pushed_sha = local_sha or self.feature_sha
        ref_update = (
            f"refs/heads/{ref_name} {pushed_sha} refs/heads/{ref_name} {remote_sha}\n"
        )
        return subprocess.run(
            ["/bin/bash", str(self.repo / ".githooks/pre-push")],
            cwd=self.repo,
            env=environment,
            input=ref_update,
            check=False,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
        )

    def calls(self) -> list[tuple[str, str, str]]:
        if not self.capture.exists():
            return []
        return [
            tuple(line.split("\t", 2))
            for line in self.capture.read_text(encoding="utf-8").splitlines()
        ]

    def test_changed_go_sql_and_migration_paths_run_their_checks(self) -> None:
        result = self.run_hook()

        self.assertEqual(result.returncode, 0, result.stdout)
        calls = self.calls()
        self.assertTrue(
            any(
                tool == "golangci-lint"
                and "run" in args
                and "--new-from-rev=main" in args
                for tool, _, args in calls
            ),
            calls,
        )
        self.assertTrue(
            any(tool == "make" and args == "lint-sql" for tool, _, args in calls), calls
        )
        self.assertTrue(
            any(
                tool == "python"
                and ".github/scripts/sql_lint.py" in args
                and "--base main" in args
                and f"--head {self.feature_sha}" in args
                for tool, _, args in calls
            ),
            calls,
        )
        self.assertTrue(
            any(
                tool == "bash" and args == ".github/scripts/test-migrations.sh"
                for tool, _, args in calls
            ),
            calls,
        )
        self.assertTrue(
            any(
                tool == "gitleaks" and f"--log-opts=main..{self.feature_sha}" in args
                for tool, _, args in calls
            ),
            calls,
        )

    def test_docs_only_ref_skips_path_specific_checks(self) -> None:
        self.git("checkout", "--quiet", "main")
        self.git("checkout", "--quiet", "-b", "docs-only")
        (self.repo / "README.md").write_text("docs only\n", encoding="utf-8")
        self.git("add", "README.md")
        self.git("commit", "--quiet", "-m", "docs(test): update docs")
        docs_sha = self.git("rev-parse", "HEAD").strip()

        result = self.run_hook(local_sha=docs_sha, ref_name="docs-only")

        self.assertEqual(result.returncode, 0, result.stdout)
        calls = self.calls()
        self.assertEqual([tool for tool, _, _ in calls], ["gitleaks"], calls)
        self.assertIn(f"--log-opts=main..{docs_sha}", calls[0][2])

    def test_workflow_changes_run_actionlint_with_queue_schema_ignore(self) -> None:
        workflow = self.repo / ".github/workflows/ci.yml"
        workflow.parent.mkdir(parents=True)
        workflow.write_text("name: CI\non: pull_request\n", encoding="utf-8")
        self.git("add", ".github/workflows/ci.yml")
        self.git("commit", "--quiet", "-m", "ci(test): change workflow")

        result = self.run_hook(local_sha=self.git("rev-parse", "HEAD").strip())

        self.assertEqual(result.returncode, 0, result.stdout)
        actionlint_calls = [args for tool, _, args in self.calls() if tool == "actionlint"]
        self.assertEqual(len(actionlint_calls), 1, self.calls())
        self.assertIn(
            'unexpected key "queue" for "concurrency" section', actionlint_calls[0]
        )

    def test_deleted_ref_skips_checks_and_existing_ref_scans_branch_commits(self) -> None:
        deleted = self.run_hook(local_sha=ZERO_SHA, remote_sha=self.feature_sha)

        self.assertEqual(deleted.returncode, 0, deleted.stdout)
        self.assertEqual(self.calls(), [])

        (self.repo / "README.md").write_text("incremental push\n", encoding="utf-8")
        self.git("add", "README.md")
        self.git("commit", "--quiet", "-m", "docs(test): add incremental note")
        next_sha = self.git("rev-parse", "HEAD").strip()

        updated = self.run_hook(local_sha=next_sha, remote_sha=self.feature_sha)

        self.assertEqual(updated.returncode, 0, updated.stdout)
        gitleaks_calls = [args for tool, _, args in self.calls() if tool == "gitleaks"]
        self.assertEqual(len(gitleaks_calls), 1, self.calls())
        self.assertIn(f"--log-opts=main..{next_sha}", gitleaks_calls[0])

    def test_merge_of_base_skips_base_commits_and_scans_branch_side(self) -> None:
        (self.repo / "branch-secret.txt").write_text(
            "branch-only test fixture\n", encoding="utf-8"
        )
        self.git("add", "branch-secret.txt")
        self.git("commit", "--quiet", "-m", "test: add branch-only fixture")
        branch_secret_sha = self.git("rev-parse", "HEAD").strip()

        self.git("checkout", "--quiet", "main")
        (self.repo / "base-fixture.txt").write_text(
            "accepted historical fixture\n", encoding="utf-8"
        )
        self.git("add", "base-fixture.txt")
        self.git("commit", "--quiet", "-m", "test: add accepted base fixture")
        base_fixture_sha = self.git("rev-parse", "HEAD").strip()

        self.git("checkout", "--quiet", "feature")
        self.git("merge", "--quiet", "--no-edit", "main")
        merge_sha = self.git("rev-parse", "HEAD").strip()

        result = self.run_hook(local_sha=merge_sha, remote_sha=self.feature_sha)

        self.assertEqual(result.returncode, 0, result.stdout)
        gitleaks_calls = [args for tool, _, args in self.calls() if tool == "gitleaks"]
        self.assertEqual(len(gitleaks_calls), 1, self.calls())
        log_opts = next(
            arg for arg in gitleaks_calls[0].split() if arg.startswith("--log-opts=")
        )
        self.assertEqual(log_opts, f"--log-opts=main..{merge_sha}")
        scanned_commits = set(
            self.git("rev-list", log_opts.removeprefix("--log-opts=")).splitlines()
        )
        self.assertIn(branch_secret_sha, scanned_commits)
        self.assertNotIn(base_fixture_sha, scanned_commits)

    def test_failing_checks_block_the_push(self) -> None:
        for tool in ("golangci-lint", "make", "python", "bash", "gitleaks"):
            with self.subTest(tool=tool):
                result = self.run_hook(fail_tool=tool)
                self.assertNotEqual(result.returncode, 0, f"{tool} failure was ignored")
                self.assertTrue(
                    any(call_tool == tool for call_tool, _, _ in self.calls()),
                    f"{tool} was not invoked before the hook returned: {result.stdout}",
                )


if __name__ == "__main__":
    unittest.main()
