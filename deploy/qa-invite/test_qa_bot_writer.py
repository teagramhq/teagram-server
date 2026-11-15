#!/usr/bin/env python3
import os
import pathlib
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer


ROOT = pathlib.Path(__file__).resolve().parents[2]
WRITER = ROOT / "deploy/qa-invite/qa-account-writer.sh"
TOKEN = "987654321:" + "B" * 42 + "A"


class WriterTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.path = pathlib.Path(self.temporary.name)
        self.bin = self.path / "bin"
        self.bin.mkdir()
        self.identity = self.path / "identity"
        self.identity.write_text("test identity\n")
        self.identity.chmod(0o600)
        self.token_file = self.path / "token"
        age = self.path / "age"
        age.write_text("""#!/bin/sh
cat >/dev/null
[ "${MOCK_DECRYPT_FAIL:-}" != 1 ] || exit 1
if [ "$SSH_ORIGINAL_COMMAND" = bot-health ]; then
    printf '%s' "${MOCK_HEALTH_OVERRIDE-1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA}"
    exit 0
fi
printf '%s' "$MOCK_TOKEN"
""")
        age.chmod(0o700)
        probe = self.bin / "python3"
        probe.write_text("""#!/bin/sh
cat >/dev/null
[ "${MOCK_PROBE_FAIL:-}" != 1 ] || exit 1
printf '%s\\n' 'getMe=200 username=teagram_qa_test_bot'
""")
        probe.chmod(0o700)
        source = WRITER.read_text()
        replacements = {
            "/home/qa/.qa-accounts-invite": str(self.path / "accounts"),
            "/home/qa/.telegram-bot-token.XXXXXXXX": str(self.path / "token.XXXXXXXX"),
            "/home/qa/.telegram-bot-token": str(self.token_file),
            "/home/qa/.config/age/keys.txt": str(self.identity),
            "/home/qa/.local/bin/age": str(age),
            '[[ "$(id -un)" == qa && "$(id -u)" -eq 1000 ]]': f'[[ "$(id -u)" -eq {os.getuid()} ]]',
            "'1000:700'": f"'{os.getuid()}:700'",
            "'1000:600'": f"'{os.getuid()}:600'",
            'install -m 0600 -o qa -g qa /dev/null "$BOT_LOCK_FILE"': 'install -m 0600 /dev/null "$BOT_LOCK_FILE"',
        }
        for old, new in replacements.items():
            self.assertIn(old, source)
            source = source.replace(old, new)
        self.writer = self.path / "writer"
        self.writer.write_text(source)
        self.env = os.environ | {"PATH": f"{self.bin}:{os.environ['PATH']}", "MOCK_TOKEN": TOKEN}

    def invoke(self, command, extra_env=None):
        result = subprocess.run(["bash", str(self.writer)], input="encrypted fixture\n", text=True, capture_output=True,
                                env=self.env | {"SSH_ORIGINAL_COMMAND": command} | (extra_env or {}), check=False)
        self.assertNotIn(TOKEN, result.stdout + result.stderr)
        return result

    def test_store_and_probe_only_the_pinned_bot(self):
        self.assertEqual(self.invoke("bot-health").stdout.strip(), "qa-bot-writer=ready")
        result = self.invoke("bot-store")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "qa-bot-token=stored")
        self.assertEqual(self.token_file.read_text(), TOKEN)
        self.assertEqual(self.token_file.stat().st_mode & 0o777, 0o600)
        self.assertEqual(self.invoke("bot-probe").stdout.strip(), "getMe=200 username=teagram_qa_test_bot")
        self.assertNotEqual(self.invoke("bot-store other_bot").returncode, 0)
        self.assertNotEqual(self.invoke("bot-probe extra").returncode, 0)

    def test_failed_decrypt_or_probe_preserves_old_token(self):
        self.token_file.write_text("previous\n")
        self.token_file.chmod(0o600)
        for failure in ("MOCK_DECRYPT_FAIL", "MOCK_PROBE_FAIL"):
            self.assertNotEqual(self.invoke("bot-store", {failure: "1"}).returncode, 0)
            self.assertEqual(self.token_file.read_text(), "previous\n")
            self.assertEqual(list(self.path.glob("token.????????")), [])

    def test_refuses_unsafe_token_or_identity(self):
        target = self.path / "target"
        target.write_text("sentinel")
        self.token_file.symlink_to(target)
        self.assertNotEqual(self.invoke("bot-store").returncode, 0)
        self.assertEqual(target.read_text(), "sentinel")
        self.token_file.unlink()
        self.identity.chmod(0o644)
        self.assertNotEqual(self.invoke("bot-health").returncode, 0)

    def test_rejects_invalid_decrypted_tokens_without_replacing_the_existing_file(self):
        self.token_file.write_text("previous\n")
        self.token_file.chmod(0o600)
        for payload in ("9" * 21 + ":" + "A" * 43, TOKEN + "\n", "0:" + "A" * 43):
            with self.subTest(length=len(payload)):
                result = self.invoke("bot-store", {"MOCK_TOKEN": payload})
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.token_file.read_text(), "previous\n")
                self.assertEqual(list(self.path.glob("token.????????")), [])

    def test_health_rejects_the_wrong_decrypted_challenge(self):
        result = self.invoke("bot-health", {"MOCK_HEALTH_OVERRIDE": "wrong"})
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("qa-bot-writer=ready", result.stdout)

    def test_real_probe_checks_status_and_username_without_printing_token(self):
        class Handler(BaseHTTPRequestHandler):
            status = 200
            body = b'{"ok":true,"result":{"username":"teagram_qa_test_bot"}}'

            def do_GET(self):
                self.send_response(self.status)
                self.end_headers()
                self.wfile.write(self.body)

            def log_message(self, *_args):
                pass

        server = HTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        self.addCleanup(thread.join)
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        self.token_file.write_text(TOKEN)
        embedded = WRITER.read_text().split('  python3 - "$1" <<\'PY\'\n', 1)[1].split('\nPY\n', 1)[0]
        embedded = embedded.replace("http://teagram.tailaa4918.ts.net:2447", f"http://127.0.0.1:{server.server_port}")
        for status, body, expected, diagnostic in [
            (200, b'{"ok":true,"result":{"username":"teagram_qa_test_bot"}}', 0, ""),
            (200, b'{"ok":true,"result":{"username":"another_bot"}}', 1, "ValueError"),
            (401, b'{}', 1, "HTTP 401"),
        ]:
            Handler.status, Handler.body = status, body
            result = subprocess.run([sys.executable, "-c", embedded, str(self.token_file)],
                                    text=True, capture_output=True, check=False)
            self.assertEqual(result.returncode, expected)
            self.assertIn(diagnostic, result.stderr)
            self.assertNotIn(TOKEN, result.stdout + result.stderr)
            self.assertNotIn("/bot" + TOKEN, result.stdout + result.stderr)

        injected_failure = """import urllib.error, urllib.request
class FailingOpener:
    def open(self, url, timeout):
        raise urllib.error.URLError(TimeoutError(url))
urllib.request.build_opener = lambda *_: FailingOpener()
"""
        result = subprocess.run([sys.executable, "-c", injected_failure + embedded, str(self.token_file)],
                                text=True, capture_output=True, check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("TimeoutError", result.stderr)
        self.assertNotIn(TOKEN, result.stdout + result.stderr)
        self.assertNotIn("http://", result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
