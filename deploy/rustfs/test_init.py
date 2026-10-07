"""Exercise the Compose initializer's shell argument boundaries without S3."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]


class InitTest(unittest.TestCase):
    def run_init(self, existing=False, fail_policy=False):
        with tempfile.TemporaryDirectory(dir=ROOT) as directory:
            work = Path(directory)
            secrets = work / "secrets"
            secrets.mkdir()
            for name, value in {
                "rustfs-root-access-key": "root-access",
                "rustfs-root-secret-key": "root-secret",
                "telegramd-blob-secret-key": "dummy-app-secret",
            }.items():
                (secrets / name).write_text(value)
            mc = work / "mc"
            mc.write_text('''#!/bin/sh
set -eu
case "$1 $2 $3" in
  "admin user info") exit "$EXISTING" ;;
  "admin user add")
    [ "$#" = 6 ] && [ "$5" = app-access ] && [ "$6" = dummy-app-secret ]
    touch /work/created ;;
  "admin policy create") [ "$FAIL_POLICY" = 0 ] ;;
  "admin policy attach") touch /work/attached ;;
esac
''')
            mc.chmod(0o755)
            env = dict(os.environ, POSTGRES_PASSWORD="test",
                       TG_PUBLIC_LINK_PREFIX="https://example.test",
                       TG_BLOB_S3_ACCESS_KEY_ID="app-access")
            config = json.loads(subprocess.check_output(
                ["docker", "compose", "config", "--format", "json"],
                cwd=ROOT, env=env, stderr=subprocess.DEVNULL))
            service = config["services"]["rustfs-init"]
            args = ["docker", "run", "--rm", "-v", f"{work}:/work",
                    "-v", f"{secrets}:/run/secrets:ro",
                    "-v", f"{mc}:/usr/local/bin/mc:ro",
                    "-e", "TG_BLOB_S3_ACCESS_KEY_ID=app-access",
                    "-e", f"EXISTING={0 if existing else 1}",
                    "-e", f"FAIL_POLICY={1 if fail_policy else 0}"]
            script = ROOT / "deploy/rustfs/init.sh"
            if script.exists():
                args += ["-v", f"{script}:/usr/local/bin/rustfs-init.sh:ro"]
            args += ["--entrypoint", service["entrypoint"][0], "alpine:3.22"]
            # Config serialization escapes dollars for another Compose read;
            # the container receives one dollar at runtime.
            args += [arg.replace("$$", "$") for arg in
                     service["entrypoint"][1:] + (service.get("command") or [])]
            result = subprocess.run(args, capture_output=True, text=True)
            # Do not echo captured stderr: the regression executes a secret as
            # a command, and shell diagnostics would expose it in CI logs.
            self.assertTrue("dummy-app-secret" not in result.stdout + result.stderr,
                            "initializer leaked the application secret")
            return result.returncode, (work / "created").exists(), (work / "attached").exists()

    def test_new_user_receives_secret_in_same_command(self):
        self.assertEqual(self.run_init(), (0, True, True))

    def test_existing_user_is_not_recreated(self):
        self.assertEqual(self.run_init(existing=True), (0, False, True))

    def test_policy_failure_stops_initialization(self):
        code, created, attached = self.run_init(fail_policy=True)
        self.assertNotEqual(code, 0)
        self.assertFalse(created)
        self.assertFalse(attached)


if __name__ == "__main__":
    unittest.main()
