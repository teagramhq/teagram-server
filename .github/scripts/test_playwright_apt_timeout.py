#!/usr/bin/env python3
"""Keep cold Playwright apt downloads within their CI timeout budgets."""

from __future__ import annotations

import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github/workflows/ci.yml"


class PlaywrightAptTimeoutTests(unittest.TestCase):
    def test_e2e_admin_budget_allows_both_cold_apt_attempts(self) -> None:
        workflow = WORKFLOW.read_text(encoding="utf-8")
        job_match = re.search(
            r"(?ms)^  test-e2e-admin:\n(.*?)(?=^  [a-z0-9-]+:|\Z)", workflow
        )
        self.assertIsNotNone(job_match, "e2e-admin job is missing")
        job = job_match.group(1)

        step_match = re.search(
            r"(?ms)^      - name: Install the system dependencies from the network\n"
            r"(.*?)(?=^      - name:|\Z)",
            job,
        )
        self.assertIsNotNone(step_match, "network apt install step is missing")
        step = step_match.group(1)

        job_timeout = re.search(r"(?m)^    timeout-minutes: (\d+)\s*$", job)
        step_timeout = re.search(r"(?m)^        timeout-minutes: (\d+)\s*$", step)
        attempt_timeout = re.search(
            r"(?m)^\s+setsid timeout --signal=KILL (\d+)m bash -c attempt_install &\s*$",
            step,
        )
        attempt_count = re.search(r"(?m)^\s+for attempt in 1 (\d+); do\s*$", step)
        for match, message in (
            (job_timeout, "e2e-admin job timeout is missing"),
            (step_timeout, "network apt install step timeout is missing"),
            (attempt_timeout, "bounded apt attempt timeout is missing"),
            (attempt_count, "apt retry count is missing"),
        ):
            self.assertIsNotNone(match, message)

        job_minutes = int(job_timeout.group(1))
        step_minutes = int(step_timeout.group(1))
        attempt_minutes = int(attempt_timeout.group(1))
        attempts = int(attempt_count.group(1))
        self.assertGreaterEqual(attempt_minutes, 8)
        self.assertGreater(step_minutes, attempt_minutes * attempts)
        self.assertGreater(job_minutes, step_minutes)


if __name__ == "__main__":
    unittest.main()
