#!/usr/bin/env python3
"""Verify that a Playwright apt retry retains completed package downloads."""

from __future__ import annotations

import os
import re
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
PREPARE_CACHE = ROOT / ".github/scripts/prepare-playwright-apt-cache.sh"
WORKFLOW = ROOT / ".github/workflows/ci.yml"


FAKE_SUDO = """#!/usr/bin/env bash
set -euo pipefail
command=$1
shift
case "$command" in
  rm|mkdir) exec "/bin/$command" "$@" ;;
  chown) exit 0 ;;
  *) echo "unexpected sudo command: $command" >&2; exit 64 ;;
esac
"""


class PlaywrightAptCacheTests(unittest.TestCase):
    def test_retry_cleanup_keeps_complete_packages_and_clears_partial_downloads(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory(prefix="playwright-apt-cache-") as temporary:
            root = Path(temporary)
            cache = root / ".apt-cache"
            partial = cache / "partial"
            partial.mkdir(parents=True)
            (cache / "fonts.deb").write_bytes(b"complete package")
            (partial / "interrupted.deb").write_bytes(b"partial package")

            fake_bin = root / "bin"
            fake_bin.mkdir()
            fake_sudo = fake_bin / "sudo"
            fake_sudo.write_text(FAKE_SUDO, encoding="utf-8")
            fake_sudo.chmod(0o755)

            result = subprocess.run(
                ["bash", str(PREPARE_CACHE), str(cache)],
                env={
                    **os.environ,
                    "PATH": f"{fake_bin}{os.pathsep}{os.environ['PATH']}",
                },
                capture_output=True,
                text=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((cache / "fonts.deb").read_bytes(), b"complete package")
            self.assertEqual(list(partial.iterdir()), [])

    def test_network_retry_uses_the_tested_cache_preparation_script(self) -> None:
        workflow = WORKFLOW.read_text(encoding="utf-8")
        self.assertIn(
            "bash .github/scripts/prepare-playwright-apt-cache.sh .apt-cache",
            workflow,
        )

    def test_network_install_retry_budget_covers_slow_archive_mirrors(self) -> None:
        workflow = WORKFLOW.read_text(encoding="utf-8")
        job = re.search(
            r"(?ms)^  test-e2e-admin:\n(.*?)(?=^  [a-z0-9-]+:|\Z)", workflow
        )
        self.assertIsNotNone(job, "test-e2e-admin job is missing")
        step = re.search(
            r"(?ms)^      - name: Install the system dependencies from the network\n(.*?)(?=^      - name:|\Z)",
            job.group(1),
        )
        self.assertIsNotNone(step, "network dependency install step is missing")
        self.assertIn("timeout-minutes: 20", step.group(1))
        self.assertIn("timeout --signal=KILL 9m", step.group(1))
        self.assertIn("timeout-minutes: 35", job.group(1))


if __name__ == "__main__":
    unittest.main(verbosity=2)
