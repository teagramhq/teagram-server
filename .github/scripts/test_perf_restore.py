#!/usr/bin/env python3
"""Ensure fixture restore ignores ACLs for cluster-local database roles."""

from __future__ import annotations

import os
import re
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / ".github/scripts/perf-run.sh"
FAKE_DOCKER = r"""#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == exec && "$3" == pg_restore ]]; then
  printf '%s\n' "$*" >> "$FAKE_DOCKER_CALLS"
  if [[ " $* " != *" --no-acl "* ]]; then
    echo 'pg_restore: error: role "browser_acceptance_probe" does not exist' >&2
    exit 1
  fi
fi
"""


class PerfRestoreTests(unittest.TestCase):
    def test_pr_comparison_base_matches_the_checked_out_merge_parent(self) -> None:
        workflow = (ROOT / ".github/workflows/perf.yml").read_text(
            encoding="utf-8"
        )
        step = re.search(
            r"(?ms)^      - name: Resolve PR comparison base\n(.*?)(?=^      - name:|\Z)",
            workflow,
        )

        self.assertIsNotNone(step, "PR base resolution step is missing")
        self.assertIn("if: github.event_name == 'pull_request'", step.group(1))
        self.assertIn(
            'echo "PERF_BASE_SHA=$(git rev-parse HEAD^1)" >> "$GITHUB_ENV"',
            step.group(1),
        )
        self.assertLess(
            workflow.index("- uses: actions/checkout@v7.0.1"),
            workflow.index("name: Resolve PR comparison base"),
        )
        self.assertLess(
            workflow.index("name: Resolve PR comparison base"),
            workflow.index("name: Restore the snapshot"),
        )

    def test_fixture_restore_ignores_grants_to_cluster_local_roles(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            temp = Path(temporary)
            bin_dir = temp / "bin"
            bin_dir.mkdir()
            docker = bin_dir / "docker"
            docker.write_text(FAKE_DOCKER, encoding="utf-8")
            docker.chmod(0o755)
            calls = temp / "restore-calls"
            env = {
                **os.environ,
                "PATH": f"{bin_dir}{os.pathsep}{os.environ['PATH']}",
                "FAKE_DOCKER_CALLS": str(calls),
                "PERF_PG_CONTAINER": "perf-pg",
                "PERF_PG_URL": "postgres://postgres:perf@localhost:5432/",
                "PERF_CACHE": str(temp / "cache"),
                "PERF_WORK": str(temp / "work"),
            }

            result = subprocess.run(
                ["bash", str(SCRIPT), "restore"],
                env=env,
                capture_output=True,
                text=True,
                timeout=30,
                check=False,
            )

            self.assertEqual(0, result.returncode, result.stdout + result.stderr)
            restore_calls = calls.read_text(encoding="utf-8").splitlines()
            self.assertEqual(2, len(restore_calls))
            for command in restore_calls:
                with self.subTest(command=command):
                    self.assertIn("--no-acl", command.split())


if __name__ == "__main__":
    unittest.main()
