#!/usr/bin/env python3
"""Regression tests for perf-run.sh base comparison handling."""

from __future__ import annotations

import os
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / ".github/scripts/perf-run.sh"
FAKE_ATLAS = """#!/usr/bin/env bash
set -euo pipefail
printf 'atlas %s\\n' "$*" >> "$PERF_TEST_CALLS"
"""
FAKE_DOCKER = """#!/usr/bin/env bash
set -euo pipefail
printf 'docker %s\\n' "$*" >> "$PERF_TEST_CALLS"
"""
FAKE_GO = """#!/usr/bin/env bash
set -euo pipefail
printf 'go %s\\n' "$*" >> "$PERF_TEST_CALLS"
case "${1:-}" in
  test)
    output=
    while (($#)); do
      if [[ "$1" == -o ]]; then
        output=$2
        shift 2
      else
        shift
      fi
    done
    [[ -n "$output" ]]
    cat > "$output" <<'SH'
#!/usr/bin/env bash
exit 0
SH
    chmod +x "$output"
    ;;
  run) ;;
  *) exit 64 ;;
esac
"""


class PerfRunTests(unittest.TestCase):
    def test_invalid_base_sha_fails_before_measurement(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls = root / "calls"
            for name, content in (
                ("atlas", FAKE_ATLAS),
                ("docker", FAKE_DOCKER),
                ("go", FAKE_GO),
            ):
                executable = bin_dir / name
                executable.write_text(content, encoding="utf-8")
                executable.chmod(0o755)

            base_sha = f"invalid-perf-base-{os.getpid()}"
            env = {
                **os.environ,
                "PATH": f"{bin_dir}{os.pathsep}{os.environ['PATH']}",
                "PERF_BASE_SHA": base_sha,
                "PERF_CACHE": str(root / "cache"),
                "PERF_HISTORY": str(root / "history"),
                "PERF_PG_CONTAINER": "perf-test-pg",
                "PERF_PG_URL": "postgres://postgres:perf@localhost:5432/",
                "PERF_RECORD": "",
                "PERF_ROUNDS": "1",
                "PERF_TEST_CALLS": str(calls),
                "PERF_WORK": str(root / "work"),
                "GITHUB_ACTIONS": "true",
            }
            env.pop("GITHUB_STEP_SUMMARY", None)
            result = subprocess.run(
                ["bash", str(SCRIPT), "measure"],
                cwd=ROOT,
                env=env,
                capture_output=True,
                text=True,
                timeout=60,
                check=False,
            )

            self.assertNotEqual(0, result.returncode, result.stdout)
            self.assertIn(
                f"Could not compare fixture seeder with base {base_sha}",
                result.stdout + result.stderr,
            )
            self.assertFalse(calls.exists(), "measurement started with an invalid base SHA")


if __name__ == "__main__":
    unittest.main()
