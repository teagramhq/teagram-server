"""Exercise the migration cutover's fail-closed marker and startup behavior."""

import os
from pathlib import Path
import tempfile
import subprocess
import unittest


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "deploy/telegram-server/migrate-local-blobs.sh"
MANIFEST = "a" * 64
SUMMARY = (
    '{"objects":1,"bytes":4,"source_manifest_sha256":"'
    + MANIFEST
    + '","destination_manifest_sha256":"'
    + MANIFEST
    + '","type":"summary"}'
)


class MigrationCutoverTest(unittest.TestCase):
    def run_cutover(self, work, report, migration_output, migration_status=0, jq_available=True):
        bin_dir = work / "bin"
        bin_dir.mkdir(exist_ok=True)
        calls = work / "docker-calls.log"
        docker = bin_dir / "docker"
        docker.write_text(
            "#!/bin/sh\n"
            "set -eu\n"
            "printf '%s\\n' \"$*\" >>\"$DOCKER_CALLS\"\n"
            "case \"$1 $2\" in\n"
            "  'compose stop') exit 0 ;;\n"
            "  'compose run') printf '%s\\n' \"$MIGRATION_OUTPUT\"; exit \"$MIGRATION_STATUS\" ;;\n"
            "  'compose up') exit 0 ;;\n"
            "  *) exit 90 ;;\n"
            "esac\n"
        )
        docker.chmod(0o755)
        path = f"{bin_dir}:{os.environ['PATH']}" if jq_available else str(bin_dir)
        env = dict(
            os.environ,
            PATH=path,
            DOCKER_CALLS=str(calls),
            MIGRATION_OUTPUT=migration_output,
            MIGRATION_STATUS=str(migration_status),
        )
        result = subprocess.run(
            [str(SCRIPT), str(report)],
            cwd=work,
            env=env,
            capture_output=True,
            text=True,
        )
        return result, calls

    def test_missing_jq_fails_before_stopping_telegramd(self):
        with tempfile.TemporaryDirectory(dir=ROOT) as temporary:
            work = Path(temporary)
            report = work / "migration.jsonl"
            result, calls = self.run_cutover(
                work,
                report,
                SUMMARY,
                jq_available=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(calls.exists())
            self.assertFalse((work / ".state/blob-migration-complete").exists())

    def test_failed_migration_does_not_write_marker_or_start_telegramd(self):
        with tempfile.TemporaryDirectory(dir=ROOT) as temporary:
            work = Path(temporary)
            report = work / "migration.jsonl"
            result, calls = self.run_cutover(
                work,
                report,
                '{"type":"object","key":"partial"}',
                migration_status=17,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertFalse((work / ".state/blob-migration-complete").exists())
            self.assertEqual(len(calls.read_text().splitlines()), 2)
            self.assertIn("compose stop telegramd", calls.read_text().splitlines()[0])
            self.assertNotIn("compose up", calls.read_text())

    def test_invalid_summary_does_not_write_marker_or_start_telegramd(self):
        with tempfile.TemporaryDirectory(dir=ROOT) as temporary:
            work = Path(temporary)
            report = work / "migration.jsonl"
            result, calls = self.run_cutover(work, report, '{"type":"object"}')

            self.assertNotEqual(result.returncode, 0)
            self.assertFalse((work / ".state/blob-migration-complete").exists())
            self.assertEqual(len(calls.read_text().splitlines()), 2)
            self.assertIn("compose stop telegramd", calls.read_text().splitlines()[0])
            self.assertNotIn("compose up", calls.read_text())

    def test_mismatched_manifests_do_not_write_marker_or_start_telegramd(self):
        with tempfile.TemporaryDirectory(dir=ROOT) as temporary:
            work = Path(temporary)
            report = work / "migration.jsonl"
            mismatched = SUMMARY.replace(
                '"destination_manifest_sha256":"' + MANIFEST,
                '"destination_manifest_sha256":"' + "b" * 64,
            )
            result, calls = self.run_cutover(work, report, mismatched)

            self.assertNotEqual(result.returncode, 0)
            self.assertFalse((work / ".state/blob-migration-complete").exists())
            self.assertEqual(len(calls.read_text().splitlines()), 2)
            self.assertIn("compose stop telegramd", calls.read_text().splitlines()[0])
            self.assertNotIn("compose up", calls.read_text())

    def test_verified_summary_is_marked_before_startup(self):
        with tempfile.TemporaryDirectory(dir=ROOT) as temporary:
            work = Path(temporary)
            report = work / "migration.jsonl"
            result, calls = self.run_cutover(
                work,
                report,
                '{"type":"object"}\n' + SUMMARY,
            )

            self.assertEqual(result.returncode, 0, "verified migration failed")
            marker = work / ".state/blob-migration-complete"
            self.assertEqual(marker.read_text(), SUMMARY + "\n")
            self.assertEqual(marker.stat().st_mode & 0o777, 0o600)
            self.assertEqual((work / ".state").stat().st_mode & 0o777, 0o700)
            calls = calls.read_text().splitlines()
            self.assertEqual(len(calls), 3)
            self.assertIn("compose stop telegramd", calls[0])
            self.assertIn("compose run", calls[1])
            self.assertIn("compose up -d telegramd", calls[2])


if __name__ == "__main__":
    unittest.main()
