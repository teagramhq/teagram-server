"""Exercise RustFS credential bootstrap stability and process boundaries."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "deploy/bootstrap-rustfs-secrets.sh"
TEMPLATE = ROOT / ".env.example"
CREDENTIALS = (
    "RUSTFS_ROOT_ACCESS_KEY",
    "RUSTFS_ROOT_SECRET_KEY",
    "TG_BLOB_S3_ACCESS_KEY_ID",
    "TG_BLOB_S3_SECRET_ACCESS_KEY",
)


class BootstrapTest(unittest.TestCase):
    def make_workdir(self):
        temporary = tempfile.TemporaryDirectory(dir=ROOT)
        work = Path(temporary.name)
        env_file = work / ".env"
        shutil.copyfile(TEMPLATE, env_file)
        return temporary, work, env_file

    def run_bootstrap(self, env_file, extra_env=None):
        env = dict(os.environ)
        if extra_env:
            env.update(extra_env)
        return subprocess.run(
            [str(SCRIPT), str(env_file)],
            cwd=env_file.parent,
            env=env,
            capture_output=True,
            text=True,
        )

    def read_credentials(self, env_file):
        found = {}
        for line in env_file.read_text().splitlines():
            for name in CREDENTIALS:
                if line.startswith(f"{name}="):
                    found[name] = line[len(name) + 1 :]
        return found

    def test_generated_credentials_remain_stable_on_rerun(self):
        temporary, work, env_file = self.make_workdir()
        with temporary:
            first = self.run_bootstrap(env_file)
            self.assertEqual(first.returncode, 0, "first bootstrap failed")
            initial_env = env_file.read_bytes()
            initial_secret = (work / ".secrets/telegramd-blob-secret-key").read_bytes()
            credentials = self.read_credentials(env_file)

            self.assertEqual(set(credentials), set(CREDENTIALS))
            self.assertEqual(len(credentials["RUSTFS_ROOT_ACCESS_KEY"]), 20)
            self.assertEqual(len(credentials["RUSTFS_ROOT_SECRET_KEY"]), 64)
            self.assertEqual(len(credentials["TG_BLOB_S3_ACCESS_KEY_ID"]), 20)
            self.assertEqual(len(credentials["TG_BLOB_S3_SECRET_ACCESS_KEY"]), 64)
            self.assertTrue(
                initial_secret.decode() == credentials["TG_BLOB_S3_SECRET_ACCESS_KEY"],
                "app secret file does not match the generated credential",
            )

            second = self.run_bootstrap(env_file)
            self.assertEqual(second.returncode, 0, "repeat bootstrap failed")
            self.assertTrue(
                env_file.read_bytes() == initial_env,
                "repeat bootstrap changed the generated credentials",
            )
            self.assertTrue(
                (work / ".secrets/telegramd-blob-secret-key").read_bytes() == initial_secret,
                "repeat bootstrap changed the app secret file",
            )
            self.assertEqual(env_file.stat().st_mode & 0o777, 0o600)
            self.assertEqual((work / ".secrets").stat().st_mode & 0o777, 0o700)
            self.assertEqual(
                (work / ".secrets/telegramd-blob-secret-key").stat().st_mode & 0o777,
                0o444,
            )

    def test_partial_credentials_are_refused_without_rewriting_env(self):
        temporary, work, env_file = self.make_workdir()
        with temporary:
            contents = env_file.read_text().replace(
                "RUSTFS_ROOT_ACCESS_KEY=\n",
                "RUSTFS_ROOT_ACCESS_KEY=0123456789abcdef0123\n",
                1,
            )
            env_file.write_text(contents)
            before = env_file.read_bytes()

            result = self.run_bootstrap(env_file)

            self.assertNotEqual(result.returncode, 0)
            self.assertTrue(
                env_file.read_bytes() == before,
                "partial credentials should not modify .env",
            )
            self.assertFalse((work / ".secrets").exists())
            self.assertNotIn("0123456789abcdef0123", result.stdout + result.stderr)

    def test_generated_credentials_are_not_passed_to_awk_argv(self):
        temporary, work, env_file = self.make_workdir()
        with temporary:
            awk_path = shutil.which("awk")
            self.assertIsNotNone(awk_path, "awk is required by the bootstrap")
            bin_dir = work / "bin"
            bin_dir.mkdir()
            argv_log = work / "awk-argv.log"
            wrapper = bin_dir / "awk"
            wrapper.write_text(
                "#!/bin/sh\n"
                "for argument do printf '%s\\n' \"$argument\" >>\"$AWK_ARGV_LOG\"; done\n"
                "exec \"$REAL_AWK\" \"$@\"\n"
            )
            wrapper.chmod(0o755)

            result = self.run_bootstrap(
                env_file,
                {
                    "PATH": f"{bin_dir}:{os.environ['PATH']}",
                    "REAL_AWK": awk_path,
                    "AWK_ARGV_LOG": str(argv_log),
                },
            )

            self.assertEqual(result.returncode, 0, "bootstrap failed")
            argv = argv_log.read_text()
            credential_values = self.read_credentials(env_file).values()
            self.assertFalse(
                any(value in argv for value in credential_values),
                "bootstrap passed a credential to a child-process argument",
            )
            self.assertFalse(
                any(value in result.stdout + result.stderr for value in credential_values),
                "bootstrap printed a credential",
            )


if __name__ == "__main__":
    unittest.main()
