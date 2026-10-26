#!/usr/bin/env python3
"""Verify performance fixture restores provision cluster-global ACL roles."""

from __future__ import annotations

import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
PERF_RUN = ROOT / ".github/scripts/perf-run.sh"
ROLE_SQL = (
    "CREATE ROLE browser_acceptance_probe LOGIN NOSUPERUSER NOCREATEDB "
    "NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS"
)
FAKE_DOCKER = """#!/usr/bin/env python3
import json
import os
import sys

with open(os.environ["FAKE_DOCKER_LOG"], "a", encoding="utf-8") as log:
    json.dump(sys.argv[1:], log)
    log.write("\\n")
"""


class PerfRunRestoreTests(unittest.TestCase):
    def test_restore_creates_probe_role_before_restoring_database_acls(self) -> None:
        with tempfile.TemporaryDirectory(prefix="perf-run-restore-") as temporary:
            root = Path(temporary)
            fake_bin = root / "bin"
            fake_bin.mkdir()
            docker = fake_bin / "docker"
            docker.write_text(FAKE_DOCKER, encoding="utf-8")
            docker.chmod(0o755)
            log = root / "docker.jsonl"

            result = subprocess.run(
                ["bash", str(PERF_RUN), "restore"],
                env={
                    **os.environ,
                    "PATH": f"{fake_bin}{os.pathsep}{os.environ['PATH']}",
                    "FAKE_DOCKER_LOG": str(log),
                    "PERF_PG_CONTAINER": "perf-pg",
                    "PERF_PG_URL": "postgres://postgres:perf@localhost:5432/",
                    "PERF_CACHE": str(root / "cache"),
                    "PERF_HISTORY": str(root / "history"),
                    "RUNNER_TEMP": str(root),
                },
                capture_output=True,
                text=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            docker_calls = [
                json.loads(line) for line in log.read_text(encoding="utf-8").splitlines()
            ]
            role_calls = [
                index for index, call in enumerate(docker_calls) if ROLE_SQL in call
            ]
            restore_calls = [
                index
                for index, call in enumerate(docker_calls)
                if len(call) > 2 and call[2] == "pg_restore"
            ]

            self.assertEqual(len(role_calls), 1, docker_calls)
            self.assertEqual(len(restore_calls), 2, docker_calls)
            self.assertLess(role_calls[0], restore_calls[0], docker_calls)


if __name__ == "__main__":
    unittest.main(verbosity=2)
