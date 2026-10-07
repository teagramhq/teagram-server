"""Exercise the migration cutover's fail-closed marker and startup behavior."""

import os
from pathlib import Path
import tempfile
import subprocess
import unittest


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "deploy/telegram-server/migrate-local-blobs.sh"
README = ROOT / "deploy/telegram-server/README.md"
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

    def run_documented_rollback(
        self,
        work,
        restore_status=0,
        up_status=0,
        restore_output=SUMMARY,
        first_log_output="msg=listening",
        later_log_output="msg=listening",
        first_running_output="rollback-cid",
        later_running_output="rollback-cid",
    ):
        rollback = README.read_text().split("## Rollback\n", 1)[1]
        commands = rollback.split("```sh\n", 1)[1].split("\n```", 1)[0]
        commands = commands.replace("cd /opt/telegram-server", 'cd "$TEST_WORKDIR"')
        bin_dir = work / "bin"
        bin_dir.mkdir()
        calls = work / "docker-calls.log"
        log_state = work / "rollback-log-state"
        running_state = work / "rollback-running-state"
        docker = bin_dir / "docker"
        docker.write_text(
            "#!/bin/sh\n"
            "set -eu\n"
            "printf '%s\\n' \"$*\" >>\"$DOCKER_CALLS\"\n"
            "case \"$*\" in\n"
            "  *'compose stop telegramd'*) exit 0 ;;\n"
            "  *'compose run --rm --no-deps blob-restore'*) printf '%s\\n' \"$RESTORE_OUTPUT\"; exit \"$RESTORE_STATUS\" ;;\n"
            "  *'compose -f docker-compose.yml -f docker-compose.local-blobs.yml up -d --no-deps telegramd'*) exit \"$DOCKER_UP_STATUS\" ;;\n"
            "  *'compose -f docker-compose.yml -f docker-compose.local-blobs.yml ps --all --quiet telegramd'*) printf 'rollback-cid\\n' ;;\n"
            "  'inspect --format {{.State.StartedAt}} rollback-cid') printf '2026-10-07T16:00:00.000000000Z\\n' ;;\n"
            "  *'logs --since '*telegramd*)\n"
            "    if [ -e \"$ROLLBACK_LOG_STATE\" ]; then printf '%s\\n' \"$LATER_LOG_OUTPUT\"; else : >\"$ROLLBACK_LOG_STATE\"; printf '%s\\n' \"$FIRST_LOG_OUTPUT\"; fi ;;\n"
            "  *'compose -f docker-compose.yml -f docker-compose.local-blobs.yml ps --status running --quiet telegramd'*)\n"
            "    if [ -e \"$ROLLBACK_RUNNING_STATE\" ]; then printf '%s\\n' \"$LATER_RUNNING_OUTPUT\"; else : >\"$ROLLBACK_RUNNING_STATE\"; printf '%s\\n' \"$FIRST_RUNNING_OUTPUT\"; fi ;;\n"
            "  *'compose ps -a'*) exit 0 ;;\n"
            "  *) exit 90 ;;\n"
            "esac\n"
        )
        docker.chmod(0o755)
        sleep = bin_dir / "sleep"
        sleep.write_text("#!/bin/sh\nexit 0\n")
        sleep.chmod(0o755)
        env = dict(
            os.environ,
            PATH=f"{bin_dir}:{os.environ['PATH']}",
            DOCKER_CALLS=str(calls),
            RESTORE_OUTPUT=restore_output,
            RESTORE_STATUS=str(restore_status),
            DOCKER_UP_STATUS=str(up_status),
            ROLLBACK_LOG_STATE=str(log_state),
            FIRST_LOG_OUTPUT=first_log_output,
            LATER_LOG_OUTPUT=later_log_output,
            ROLLBACK_RUNNING_STATE=str(running_state),
            FIRST_RUNNING_OUTPUT=first_running_output,
            LATER_RUNNING_OUTPUT=later_running_output,
            TEST_WORKDIR=str(work),
        )
        result = subprocess.run(
            ["/bin/sh", "-c", commands, "rollback", str(work / "restore.jsonl")],
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

    def test_restore_failure_preserves_migration_marker_and_does_not_switch_backend(self):
        with tempfile.TemporaryDirectory(dir=ROOT) as temporary:
            work = Path(temporary)
            marker = work / ".state/blob-migration-complete"
            marker.parent.mkdir()
            marker.write_text("verified migration\n")

            result, calls = self.run_documented_rollback(work, restore_status=17)

            self.assertNotEqual(result.returncode, 0)
            self.assertTrue(marker.exists())
            self.assertEqual(len(calls.read_text().splitlines()), 2)
            self.assertIn("compose run --rm --no-deps blob-restore", calls.read_text().splitlines()[1])

    def test_invalid_restore_report_does_not_switch_backend(self):
        with tempfile.TemporaryDirectory(dir=ROOT) as temporary:
            work = Path(temporary)
            marker = work / ".state/blob-migration-complete"
            marker.parent.mkdir()
            marker.write_text("verified migration\n")

            result, calls = self.run_documented_rollback(work, restore_output='{"type":"summary"}')

            self.assertNotEqual(result.returncode, 0)
            self.assertTrue(marker.exists())
            self.assertEqual(len(calls.read_text().splitlines()), 2)

    def test_rollback_startup_failure_preserves_migration_marker(self):
        with tempfile.TemporaryDirectory(dir=ROOT) as temporary:
            work = Path(temporary)
            marker = work / ".state/blob-migration-complete"
            marker.parent.mkdir()
            marker.write_text("verified migration\n")

            result, calls = self.run_documented_rollback(work, up_status=1)

            self.assertNotEqual(result.returncode, 0)
            self.assertTrue(marker.exists())
            self.assertEqual(len(calls.read_text().splitlines()), 3)

    def test_rollback_waits_for_listening_and_rechecks_running(self):
        with tempfile.TemporaryDirectory(dir=ROOT) as temporary:
            work = Path(temporary)
            marker = work / ".state/blob-migration-complete"
            marker.parent.mkdir()
            marker.write_text("verified migration\n")

            result, calls = self.run_documented_rollback(
                work,
                first_log_output="",
                later_log_output="msg=listening",
                first_running_output="rollback-cid",
                later_running_output="",
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertTrue(marker.exists())
            calls = calls.read_text().splitlines()
            readiness_checks = [i for i, call in enumerate(calls) if "logs --since " in call and "5m" not in call]
            running_checks = [i for i, call in enumerate(calls) if "ps --status running --quiet telegramd" in call]
            self.assertEqual(len(readiness_checks), 2)
            self.assertEqual(len(running_checks), 2)
            self.assertLess(readiness_checks[0], running_checks[0])
            self.assertLess(readiness_checks[1], running_checks[1])

    def test_rollback_exit_before_listening_preserves_migration_marker(self):
        with tempfile.TemporaryDirectory(dir=ROOT) as temporary:
            work = Path(temporary)
            marker = work / ".state/blob-migration-complete"
            marker.parent.mkdir()
            marker.write_text("verified migration\n")

            result, calls = self.run_documented_rollback(
                work,
                first_log_output="",
                later_log_output="",
                first_running_output="rollback-cid",
                later_running_output="",
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertTrue(marker.exists())
            self.assertIn("exited before reporting msg=listening", result.stderr)
            calls = calls.read_text().splitlines()
            readiness_checks = [i for i, call in enumerate(calls) if "logs --since " in call and "5m" not in call]
            running_checks = [i for i, call in enumerate(calls) if "ps --status running --quiet telegramd" in call]
            self.assertEqual(len(readiness_checks), 2)
            self.assertEqual(len(running_checks), 2)
            self.assertLess(readiness_checks[0], running_checks[0])
            self.assertLess(readiness_checks[1], running_checks[1])

    def test_rollback_success_removes_migration_marker(self):
        with tempfile.TemporaryDirectory(dir=ROOT) as temporary:
            work = Path(temporary)
            marker = work / ".state/blob-migration-complete"
            marker.parent.mkdir()
            marker.write_text("verified migration\n")

            result, calls = self.run_documented_rollback(work, up_status=0)

            self.assertEqual(result.returncode, 0, "rollback failed")
            self.assertFalse(marker.exists())
            calls = calls.read_text().splitlines()
            self.assertEqual(len(calls), 9)
            self.assertIn("compose stop telegramd", calls[0])
            self.assertIn("compose run --rm --no-deps blob-restore", calls[1])
            self.assertIn(" up -d --no-deps telegramd", calls[2])
            self.assertIn("ps --all --quiet telegramd", calls[3])
            self.assertIn("inspect --format {{.State.StartedAt}} rollback-cid", calls[4])
            self.assertIn("logs --since 2026-10-07T16:00:00.000000000Z telegramd", calls[5])
            self.assertIn("ps --status running --quiet telegramd", calls[6])


if __name__ == "__main__":
    unittest.main()
