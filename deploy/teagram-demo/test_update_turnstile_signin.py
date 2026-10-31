#!/usr/bin/env python3
"""Regression tests for the demo signin environment rollout helper."""

from __future__ import annotations

import os
import pathlib
import signal
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPT_DIR))
import update_turnstile_signin  # noqa: E402


ENV_NAME = update_turnstile_signin.ENV_NAME


class UpdateTurnstileSigninTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        self.env_file = self.root / ".env"
        self.backup_dir = self.root / "backups"
        self.backup_dir.mkdir(mode=0o700)
        self.environment = (
            b"POSTGRES_PASSWORD=fixture-database-password\n"
            b"TEAGRAM_DEMO_TURNSTILE_SITE_KEY=fixture-site-key\n"
            b"TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE=/root-only/fixture-secret\n"
            b"TG_ADVERTISE_DEMO_GUEST_SIGN_UP=true\n"
        )
        self.env_file.write_bytes(self.environment)
        self.env_file.chmod(0o600)

    def update(
        self,
        target: str,
        backup_path: pathlib.Path | None = None,
        **rollout_env: str,
    ) -> dict[str, str]:
        with mock.patch.dict(os.environ, rollout_env, clear=True):
            return update_turnstile_signin.update_env_file(
                self.env_file,
                self.backup_dir,
                target,
                backup_path,
            )

    def test_addition_uses_approved_env_addition_and_preserves_signup_credentials(self) -> None:
        result = self.update(
            "true",
            ROLLOUT_RUNNER_ADDED_ENV=ENV_NAME,
            ROLLOUT_RUNNER_CHANGED_ENV=ENV_NAME,
        )

        self.assertEqual(result["mode"], "added")
        self.assertEqual(result["previous"], "false")
        self.assertEqual(result["current"], "true")
        updated = self.env_file.read_bytes()
        self.assertEqual(updated, self.environment + f"{ENV_NAME}=true\n".encode())
        self.assertEqual(self.backup_dir.joinpath(pathlib.Path(result["backup"]).name).read_bytes(), self.environment)
        self.assertEqual(self.env_file.stat().st_mode & 0o777, 0o600)

    def test_value_change_requires_runner_change_and_verified_backup(self) -> None:
        self.env_file.write_bytes(self.environment + f"{ENV_NAME}=false\n".encode())
        result = self.update(
            "true",
            ROLLOUT_RUNNER_ADDED_ENV=ENV_NAME,
            ROLLOUT_RUNNER_CHANGED_ENV=ENV_NAME,
        )

        self.assertEqual(result["mode"], "changed")
        updated = self.env_file.read_bytes()
        self.assertEqual(updated, self.environment + f"{ENV_NAME}=true\n".encode())
        self.assertEqual(pathlib.Path(result["backup"]).read_bytes(), self.environment + f"{ENV_NAME}=false\n".encode())

    def test_explicit_backup_path_is_created_before_environment_update(self) -> None:
        backup = self.backup_dir / "env.before-turnstile-reviewed-run"
        result = self.update(
            "true",
            backup_path=backup,
            ROLLOUT_RUNNER_ADDED_ENV=ENV_NAME,
            ROLLOUT_RUNNER_CHANGED_ENV=ENV_NAME,
        )

        self.assertEqual(result["backup"], str(backup))
        self.assertEqual(backup.read_bytes(), self.environment)
        self.assertEqual(backup.stat().st_mode & 0o777, 0o600)

    def test_failed_first_use_sync_is_retried_before_snapshot(self) -> None:
        backup_dir = self.root / "first-use-backups"
        backup = backup_dir / "env.before-turnstile-first-use"
        parent_metadata = self.root.stat()
        real_fsync = os.fsync
        real_link = os.link
        parent_fsyncs = 0

        def fail_first_parent_fsync(descriptor: int) -> None:
            nonlocal parent_fsyncs
            metadata = os.fstat(descriptor)
            if (
                stat.S_ISDIR(metadata.st_mode)
                and (metadata.st_dev, metadata.st_ino)
                == (parent_metadata.st_dev, parent_metadata.st_ino)
            ):
                parent_fsyncs += 1
                if parent_fsyncs == 1:
                    raise OSError("injected backup-parent directory fsync failure")
            real_fsync(descriptor)

        def require_retry_sync_before_link(*args: object, **kwargs: object) -> None:
            self.assertEqual(parent_fsyncs, 2, "retry did not fsync backup parent before snapshot publication")
            real_link(*args, **kwargs)

        with (
            mock.patch.object(update_turnstile_signin.os, "fsync", side_effect=fail_first_parent_fsync),
            mock.patch.object(update_turnstile_signin.os, "link", side_effect=require_retry_sync_before_link),
            mock.patch.dict(
                os.environ,
                {
                    "ROLLOUT_RUNNER_ADDED_ENV": ENV_NAME,
                    "ROLLOUT_RUNNER_CHANGED_ENV": ENV_NAME,
                },
                clear=True,
            ),
        ):
            with self.assertRaises(SystemExit):
                update_turnstile_signin.update_env_file(
                    self.env_file,
                    backup_dir,
                    "true",
                    backup,
                )

            self.assertEqual(parent_fsyncs, 1)
            self.assertTrue(backup_dir.is_dir())
            self.assertEqual(list(backup_dir.iterdir()), [])
            self.assertEqual(self.env_file.read_bytes(), self.environment)

            result = update_turnstile_signin.update_env_file(
                self.env_file,
                backup_dir,
                "true",
                backup,
            )

        self.assertEqual(result["backup"], str(backup))
        self.assertEqual(parent_fsyncs, 3)
        self.assertTrue(backup_dir.is_dir())
        self.assertEqual(backup.read_bytes(), self.environment)
        self.assertEqual(self.env_file.read_bytes(), self.environment + f"{ENV_NAME}=true\n".encode())

    def test_explicit_backup_path_does_not_replace_an_existing_file(self) -> None:
        backup = self.backup_dir / "env.before-turnstile-existing"
        backup.write_bytes(b"preserve this backup")
        backup.chmod(0o600)

        with self.assertRaises(SystemExit):
            self.update(
                "true",
                backup_path=backup,
                ROLLOUT_RUNNER_ADDED_ENV=ENV_NAME,
                ROLLOUT_RUNNER_CHANGED_ENV=ENV_NAME,
            )

        self.assertEqual(backup.read_bytes(), b"preserve this backup")
        self.assertEqual(self.env_file.read_bytes(), self.environment)

    def test_interrupted_backup_is_not_published_at_the_recorded_path(self) -> None:
        backup = self.backup_dir / "env.before-turnstile-interrupted-run"
        child = r'''
import os
import pathlib
import signal
import sys

sys.path.insert(0, sys.argv[1])
import update_turnstile_signin

real_fdopen = os.fdopen

class InterruptingOutput:
    def __init__(self, output):
        self.output = output

    def __enter__(self):
        self.output.__enter__()
        return self

    def __exit__(self, *args):
        return self.output.__exit__(*args)

    def write(self, content):
        self.output.write(content[:len(content) // 2])
        self.output.flush()
        os.kill(os.getpid(), signal.SIGTERM)

    def flush(self):
        self.output.flush()

    def fileno(self):
        return self.output.fileno()

def interrupting_fdopen(descriptor, mode):
    return InterruptingOutput(real_fdopen(descriptor, mode))

os.fdopen = interrupting_fdopen
os.environ["ROLLOUT_RUNNER_ADDED_ENV"] = update_turnstile_signin.ENV_NAME
os.environ["ROLLOUT_RUNNER_CHANGED_ENV"] = update_turnstile_signin.ENV_NAME
update_turnstile_signin.update_env_file(
    pathlib.Path(sys.argv[2]),
    pathlib.Path(sys.argv[3]),
    "true",
    pathlib.Path(sys.argv[4]),
)
'''
        completed = subprocess.run(
            [sys.executable, "-c", child, str(SCRIPT_DIR), str(self.env_file), str(self.backup_dir), str(backup)],
            capture_output=True,
            check=False,
            timeout=10,
        )

        self.assertEqual(completed.returncode, -signal.SIGTERM, completed.stderr.decode())
        self.assertFalse(backup.exists())
        self.assertEqual(self.env_file.read_bytes(), self.environment)

    def test_parent_directory_open_failure_restores_original_bytes(self) -> None:
        real_open = os.open

        def fail_parent_open(
            path: os.PathLike[str] | str,
            flags: int,
            *args: object,
            **kwargs: object,
        ) -> int:
            if pathlib.Path(path) == self.env_file.parent:
                raise OSError("injected parent directory open failure")
            return real_open(path, flags, *args, **kwargs)

        with mock.patch.object(update_turnstile_signin.os, "open", side_effect=fail_parent_open):
            with self.assertRaises(SystemExit):
                self.update(
                    "true",
                    ROLLOUT_RUNNER_ADDED_ENV=ENV_NAME,
                    ROLLOUT_RUNNER_CHANGED_ENV=ENV_NAME,
                )

        self.assertEqual(self.env_file.read_bytes(), self.environment)

    def test_parent_directory_fsync_failure_restores_original_bytes(self) -> None:
        real_fsync = os.fsync
        env_directory = self.env_file.parent.stat()
        failed = False
        parent_fsyncs = 0
        updated = self.environment + f"{ENV_NAME}=true\n".encode()

        def fail_directory_fsync(descriptor: int) -> None:
            nonlocal failed, parent_fsyncs
            directory_metadata = os.fstat(descriptor)
            if (
                stat.S_ISDIR(directory_metadata.st_mode)
                and (directory_metadata.st_dev, directory_metadata.st_ino)
                == (env_directory.st_dev, env_directory.st_ino)
            ):
                parent_fsyncs += 1
                if parent_fsyncs == 2:
                    failed = True
                    self.assertEqual(self.env_file.read_bytes(), updated)
                    raise OSError("injected parent directory fsync failure")
            real_fsync(descriptor)

        with mock.patch.object(update_turnstile_signin.os, "fsync", side_effect=fail_directory_fsync):
            with self.assertRaises(SystemExit):
                self.update(
                    "true",
                    ROLLOUT_RUNNER_ADDED_ENV=ENV_NAME,
                    ROLLOUT_RUNNER_CHANGED_ENV=ENV_NAME,
                )

        self.assertTrue(failed)
        self.assertEqual(parent_fsyncs, 3)
        self.assertEqual(self.env_file.read_bytes(), self.environment)

    def test_missing_flag_defaults_false_without_mutation(self) -> None:
        result = self.update("false")

        self.assertEqual(result, {"changed": "false", "current": "false", "backup": "none", "mode": "none"})
        self.assertEqual(self.env_file.read_bytes(), self.environment)
        self.assertFalse(self.backup_dir.exists() and any(self.backup_dir.iterdir()))

    def test_unapproved_setting_cannot_be_added(self) -> None:
        with self.assertRaises(SystemExit):
            self.update("true", ROLLOUT_RUNNER_ADDED_ENV="TG_REGISTRATION")
        self.assertEqual(self.env_file.read_bytes(), self.environment)

    def test_unapproved_setting_cannot_be_changed(self) -> None:
        self.env_file.write_bytes(self.environment + f"{ENV_NAME}=false\n".encode())
        with self.assertRaises(SystemExit):
            self.update("true", ROLLOUT_RUNNER_ADDED_ENV=ENV_NAME)
        self.assertEqual(self.env_file.read_bytes(), self.environment + f"{ENV_NAME}=false\n".encode())

    def test_duplicate_signin_assignments_are_rejected(self) -> None:
        duplicate = self.environment + f"{ENV_NAME}=false\n{ENV_NAME}=true\n".encode()
        self.env_file.write_bytes(duplicate)
        with self.assertRaises(SystemExit):
            self.update("false")
        self.assertEqual(self.env_file.read_bytes(), duplicate)


if __name__ == "__main__":
    unittest.main()
