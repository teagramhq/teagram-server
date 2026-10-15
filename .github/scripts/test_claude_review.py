#!/usr/bin/env python3
"""Pin the Claude review workflow's safety contract and its verdict parsing."""

from __future__ import annotations

import json
import os
import re
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github/workflows/claude-review.yml"
VERDICT = ROOT / ".github/scripts/claude_review_verdict.py"
SAME_REPO = "github.event.pull_request.head.repo.full_name == github.repository"
ENABLED = "vars.CLAUDE_REVIEW_ENABLED == 'true'"
ACTION_PIN = re.compile(r"^\s*(?:-\s+)?uses:\s*anthropics/claude-code-action@[0-9a-f]{40}(\s|$)")


def block(lines: list[str], key: str) -> list[str]:
    """Return the lines nested under the first `key:` line."""
    for index, line in enumerate(lines):
        if line.strip() == f"{key}:" or line.strip().startswith(f"{key}: "):
            indent = len(line) - len(line.lstrip())
            nested = []
            for child in lines[index + 1:]:
                if child.strip() and len(child) - len(child.lstrip()) <= indent:
                    break
                nested.append(child)
            return nested
    return []


def contract_errors(text: str) -> list[str]:
    lines = [line for line in text.splitlines() if not line.lstrip().startswith("#")]
    errors = []
    if "pull_request_target" in text:
        errors.append("must not use pull_request_target")
    trigger = block(block(lines, "on"), "pull_request")
    if not trigger:
        errors.append("must trigger on pull_request")
    if not any(line.strip().startswith("- ") for line in block(trigger, "paths")):
        errors.append("pull_request trigger must keep a paths filter")
    conditions = [line for line in lines if line.strip().startswith("if:")]
    if not any(SAME_REPO in line and "!github.event.pull_request.draft" in line for line in conditions):
        errors.append("job must skip fork and draft pull requests")
    if not any(line.strip().startswith(f"if: {ENABLED} && ") for line in conditions):
        errors.append("job must be gated on vars.CLAUDE_REVIEW_ENABLED")
    uses = [line for line in lines if "anthropics/claude-code-action@" in line]
    if not uses or not all(ACTION_PIN.match(line) for line in uses):
        errors.append("claude-code-action must be pinned by full commit SHA")
    for index, line in enumerate(lines):
        if line.strip().startswith("permissions:"):
            inline = line.split(":", 1)[1].strip()
            entries = [inline] if inline else [child.strip() for child in block(lines[index:], "permissions")]
            for entry in entries:
                if entry and "write" in entry and entry.replace(" ", "") != "pull-requests:write":
                    errors.append(f"write permission beyond pull-requests: {entry}")
    return errors


def result_log(text: str, subtype: str = "success") -> list[dict]:
    return [
        {"type": "system", "subtype": "init"},
        {"type": "result", "subtype": subtype, "is_error": subtype != "success", "result": text},
    ]


class WorkflowContractTest(unittest.TestCase):
    def setUp(self) -> None:
        self.text = WORKFLOW.read_text(encoding="utf-8")

    def assert_breaks(self, mutated: str) -> None:
        self.assertNotEqual(mutated, self.text, "mutation did not apply")
        self.assertTrue(contract_errors(mutated))

    def test_workflow_meets_contract(self) -> None:
        self.assertEqual(contract_errors(self.text), [])

    def test_rejects_pull_request_target(self) -> None:
        self.assert_breaks(self.text.replace("  pull_request:\n", "  pull_request_target:\n", 1))

    def test_rejects_missing_same_repo_condition(self) -> None:
        self.assert_breaks(self.text.replace(SAME_REPO + " && ", ""))

    def test_rejects_missing_enabled_switch(self) -> None:
        self.assert_breaks(self.text.replace(ENABLED + " && ", ""))
        self.assert_breaks(self.text.replace(ENABLED, "vars.CLAUDE_REVIEW_ENABLED != 'false'"))

    def test_rejects_missing_path_filter(self) -> None:
        self.assert_breaks(re.sub(r"    paths:\n(      - .*\n)+", "", self.text))

    def test_rejects_unpinned_action(self) -> None:
        self.assert_breaks(re.sub(r"claude-code-action@[0-9a-f]{40}", "claude-code-action@v1", self.text))

    def test_rejects_extra_write_permission(self) -> None:
        self.assert_breaks(self.text.replace("  contents: read\n", "  contents: write\n", 1))
        self.assert_breaks(self.text.replace("  pull-requests: write\n", "  pull-requests: write\n  id-token: write\n", 1))
        self.assert_breaks(self.text.replace("    runs-on:", "    permissions: write-all\n    runs-on:", 1))


class VerdictTest(unittest.TestCase):
    def run_verdict(self, log: object | None) -> tuple[str, str, str]:
        with tempfile.TemporaryDirectory() as tmp:
            execution = Path(tmp) / "execution.json"
            if log is not None:
                execution.write_text(json.dumps(log), encoding="utf-8")
            body = Path(tmp) / "body.md"
            output = Path(tmp) / "output"
            env = dict(os.environ, GITHUB_OUTPUT=str(output), HEAD_SHA="0123456789abcdef")
            completed = subprocess.run(
                ["python3", str(VERDICT), str(execution) if log is not None else "", str(body)],
                env=env, capture_output=True, text=True, check=False,
            )
            self.assertEqual(completed.returncode, 0, completed.stderr)
            verdict = output.read_text(encoding="utf-8").strip().removeprefix("verdict=")
            text = body.read_text(encoding="utf-8") if body.exists() else ""
            return verdict, text, completed.stdout

    def test_blocking(self) -> None:
        verdict, body, _ = self.run_verdict(result_log("`a.go:3` drops the key.\n\nCLAUDE_REVIEW: BLOCKING\n"))
        self.assertEqual(verdict, "blocking")
        self.assertIn("`a.go:3` drops the key.", body)
        self.assertIn("`0123456789ab`", body)

    def test_clean(self) -> None:
        verdict, body, _ = self.run_verdict(result_log("Looks right.\n**CLAUDE_REVIEW: CLEAN**"))
        self.assertEqual(verdict, "clean")
        self.assertIn("Looks right.", body)

    def test_verdict_must_be_last_line(self) -> None:
        verdict, body, out = self.run_verdict(result_log("CLAUDE_REVIEW: BLOCKING\nMore text."))
        self.assertEqual(verdict, "none")
        self.assertEqual(body, "")
        self.assertIn("::warning::", out)

    def test_neutral_without_a_usable_result(self) -> None:
        for log in (None, "not a list", [], result_log("CLAUDE_REVIEW: BLOCKING", "error_max_turns")):
            with self.subTest(log=log):
                verdict, body, out = self.run_verdict(log)
                self.assertEqual(verdict, "none")
                self.assertEqual(body, "")
                self.assertIn("::warning::", out)


if __name__ == "__main__":
    unittest.main()
