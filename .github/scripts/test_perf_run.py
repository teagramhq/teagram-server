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
printf 'fixture version %s\\n' "${TG_PERF_FIXTURE_VERSION:-}" >> "$PERF_TEST_CALLS"
exit 0
SH
    chmod +x "$output"
    ;;
  run) echo "# fake performance report" ;;
  *) exit 64 ;;
esac
"""
FAKE_SEED_GO = """#!/usr/bin/env bash
set -euo pipefail
printf 'go %s\\n' "$*" >> "$PERF_TEST_CALLS"
"""


class PerfRunTests(unittest.TestCase):
    def test_seed_rebuilds_the_message_history_index_after_fixture_writes(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            runner_temp = root / "runner-temp"
            runner_temp.mkdir()
            calls = root / "calls"
            for name, content in (
                ("atlas", FAKE_ATLAS),
                ("docker", FAKE_DOCKER),
                ("go", FAKE_SEED_GO),
            ):
                executable = bin_dir / name
                executable.write_text(content, encoding="utf-8")
                executable.chmod(0o755)

            env = {
                **os.environ,
                "PATH": f"{bin_dir}{os.pathsep}{os.environ['PATH']}",
                "PERF_CACHE": str(root / "cache"),
                "PERF_PG_CONTAINER": "perf-test-pg",
                "PERF_PG_URL": "postgres://postgres:perf@localhost:5432/",
                "PERF_TEST_CALLS": str(calls),
                "RUNNER_TEMP": str(runner_temp),
            }
            result = subprocess.run(
                ["bash", str(SCRIPT), "seed"],
                cwd=ROOT,
                env=env,
                capture_output=True,
                text=True,
                timeout=60,
                check=False,
            )

            self.assertEqual(0, result.returncode, result.stdout + result.stderr)
            lines = calls.read_text(encoding="utf-8").splitlines()
            seed = next(i for i, line in enumerate(lines) if line.startswith("go test -tags performance"))
            drops = [i for i, line in enumerate(lines) if "DROP INDEX messages_owner_peer_date_idx" in line]
            creates = [i for i, line in enumerate(lines) if "CREATE INDEX messages_owner_peer_date_idx" in line]
            analyzes = [i for i, line in enumerate(lines) if "vacuumdb -U postgres -q --analyze" in line]
            dumps = [i for i, line in enumerate(lines) if "pg_dump -U postgres -Fc -Z 3 --no-acl" in line]

            self.assertEqual(2, len(drops), lines)
            self.assertEqual(2, len(creates), lines)
            self.assertLess(max(drops), seed, lines)
            self.assertLess(seed, min(creates), lines)
            self.assertLess(max(creates), min(analyzes), lines)
            self.assertLess(max(analyzes), min(dumps), lines)

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

    def test_server_setup_change_skips_base_comparison(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            runner_temp = root / "runner-temp"
            runner_temp.mkdir()
            calls = root / "calls"
            fake_git = """#!/usr/bin/env bash
set -euo pipefail
printf 'git %s\\n' "$*" >> "$PERF_TEST_CALLS"
if [[ "${1:-}" == diff && "$*" == *"test/perf/server_test.go"* ]]; then exit 1; fi
"""
            for name, content in (
                ("atlas", FAKE_ATLAS),
                ("docker", FAKE_DOCKER),
                ("git", fake_git),
                ("go", FAKE_GO),
            ):
                executable = bin_dir / name
                executable.write_text(content, encoding="utf-8")
                executable.chmod(0o755)

            env = {
                **os.environ,
                "PATH": f"{bin_dir}{os.pathsep}{os.environ['PATH']}",
                "PERF_BASE_SHA": "base-sha",
                "PERF_CACHE": str(root / "cache"),
                "PERF_HISTORY": str(root / "history"),
                "PERF_PG_CONTAINER": "perf-test-pg",
                "PERF_PG_URL": "postgres://postgres:perf@localhost:5432/",
                "PERF_RECORD": "",
                "PERF_ROUNDS": "1",
                "PERF_TEST_CALLS": str(calls),
                "PERF_WORK": str(root / "work"),
                "FIXTURE_VERSION": "fixture-current-v2",
                "RUNNER_TEMP": str(runner_temp),
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

            self.assertEqual(0, result.returncode, result.stdout + result.stderr)
            self.assertIn("Base comparison skipped: fixture seeder changed", result.stdout)
            diff_call = next(
                line for line in calls.read_text(encoding="utf-8").splitlines()
                if line.startswith("git diff --quiet")
            )
            self.assertIn("test/perf/fixture_test.go", diff_call)
            self.assertIn("test/perf/server_test.go", diff_call)
            self.assertIn(
                "fixture version fixture-current-v2",
                calls.read_text(encoding="utf-8").splitlines(),
            )


if __name__ == "__main__":
    unittest.main()
