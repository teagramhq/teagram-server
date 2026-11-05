#!/usr/bin/env python3
"""Exercise QA bypass token rotation against private temporary fixtures."""

from __future__ import annotations

import contextlib
import importlib.util
import io
import os
import pathlib
import re
import stat
import sys
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("rotate-qa-bypass-token.py")
SPEC = importlib.util.spec_from_file_location("rotate_qa_bypass_token", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
ROTATION = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ROTATION)


@unittest.skipUnless(os.geteuid() == 0, "fixture ownership checks require root")
class RotateQABypassTokenTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="teagram-qa-bypass-rotation-")
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        self.secret_dir = self.root / "secrets"
        self.secret_dir.mkdir(mode=0o700)
        self.backup_dir = self.root / "backups" / "qa-bypass"
        self.backup_dir.parent.mkdir(mode=0o755)
        os.chown(self.root, 0, 0)
        os.chmod(self.root, 0o700)
        os.chown(self.secret_dir, 0, 0)
        os.chmod(self.secret_dir, 0o700)
        os.chown(self.backup_dir.parent, 0, 0)
        os.chmod(self.backup_dir.parent, 0o755)

        self.token_file = self.secret_dir / "turnstile-qa-bypass-token"
        self.siteverify_file = self.secret_dir / "turnstile-secret-key"
        self.env_file = self.root / ".env"
        self.old_token = "old-demo-bypass-token-that-is-long-enough"
        self.siteverify_secret = "b" * 64
        self.site_key = "demo-turnstile-site-key-that-is-long-enough"
        self._write_runtime_secret(self.token_file, self.old_token)
        self._write_runtime_secret(self.siteverify_file, self.siteverify_secret)
        self.env_file.write_text(
            f"TEAGRAM_DEMO_TURNSTILE_SITE_KEY={self.site_key}\n", encoding="utf-8"
        )
        os.chown(self.env_file, 0, 0)
        os.chmod(self.env_file, 0o600)

    @staticmethod
    def _write_runtime_secret(path: pathlib.Path, value: str) -> None:
        path.write_text(f"{value}\n", encoding="ascii")
        os.chown(path, 0, 65532)
        os.chmod(path, 0o440)

    def _rotate(self) -> tuple[pathlib.Path, int]:
        return ROTATION.rotate_token(
            self.secret_dir,
            self.token_file,
            self.siteverify_file,
            self.env_file,
            self.backup_dir,
        )

    def test_rotates_and_snapshots_without_printing_either_value(self) -> None:
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            backup, length = self._rotate()

        new_token = self.token_file.read_text(encoding="ascii").strip()
        self.assertEqual(length, 64)
        self.assertRegex(new_token, re.compile(r"[0-9a-f]{64}\Z"))
        self.assertNotEqual(new_token, self.old_token)
        self.assertEqual(backup.read_text(encoding="ascii"), f"{self.old_token}\n")
        self.assertEqual(output.getvalue(), "")
        self.assertNotIn(self.old_token, output.getvalue())
        self.assertNotIn(new_token, output.getvalue())
        backup_stat = backup.stat()
        self.assertEqual(
            (backup_stat.st_uid, backup_stat.st_gid, stat.S_IMODE(backup_stat.st_mode)),
            (0, 0, 0o600),
        )
        token_stat = self.token_file.stat()
        self.assertEqual(
            (token_stat.st_uid, token_stat.st_gid, stat.S_IMODE(token_stat.st_mode)),
            (0, 65532, 0o440),
        )

    def test_rejects_insecure_current_token_without_changing_it(self) -> None:
        os.chmod(self.token_file, 0o644)

        with self.assertRaisesRegex(ROTATION.RotationError, "qa-bypass-token-owner-group-or-mode"):
            self._rotate()

        self.assertEqual(self.token_file.read_text(encoding="ascii"), f"{self.old_token}\n")
        self.assertFalse(self.backup_dir.exists())

    def test_command_output_contains_only_snapshot_path_and_length(self) -> None:
        names = ("SECRET_DIR", "TOKEN_FILE", "SITEVERIFY_FILE", "ENV_FILE", "BACKUP_DIR")
        original_paths = {name: getattr(ROTATION, name) for name in names}
        self.addCleanup(
            lambda: [setattr(ROTATION, name, value) for name, value in original_paths.items()]
        )
        ROTATION.SECRET_DIR = self.secret_dir
        ROTATION.TOKEN_FILE = self.token_file
        ROTATION.SITEVERIFY_FILE = self.siteverify_file
        ROTATION.ENV_FILE = self.env_file
        ROTATION.BACKUP_DIR = self.backup_dir
        marker = self.root / "rotation-started"
        original_argv = sys.argv
        sys.argv = [str(SCRIPT), str(marker)]
        self.addCleanup(setattr, sys, "argv", original_argv)
        output = io.StringIO()

        with contextlib.redirect_stdout(output):
            self.assertEqual(ROTATION.main(), 0)

        new_token = self.token_file.read_text(encoding="ascii").strip()
        self.assertIn("token_length=64", output.getvalue())
        self.assertIn("snapshot=", output.getvalue())
        self.assertTrue(marker.is_file())
        self.assertEqual(marker.read_bytes(), b"")
        self.assertNotIn(self.old_token, output.getvalue())
        self.assertNotIn(new_token, output.getvalue())

    def test_rejects_reuse_of_the_siteverify_secret(self) -> None:
        original_generator = ROTATION.secrets.token_hex

        def deterministic_token(size: int) -> str:
            return "0" * (size * 2) if size == 4 else self.siteverify_secret

        ROTATION.secrets.token_hex = deterministic_token
        self.addCleanup(setattr, ROTATION.secrets, "token_hex", original_generator)

        with self.assertRaisesRegex(ROTATION.RotationError, "generated-token-invalid"):
            self._rotate()

        self.assertEqual(self.token_file.read_text(encoding="ascii"), f"{self.old_token}\n")


if __name__ == "__main__":
    unittest.main()
