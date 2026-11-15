#!/usr/bin/env python3
import json
import os
import pathlib
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "deploy/k8s/qa-bot-operation.sh"
RENDER = ROOT / "deploy/k8s/render-qa-bot-job.py"
TOKEN = "987654321:" + "B" * 42 + "A"
USERNAME = "teagram_qa_test_bot"


class OperationTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.path = pathlib.Path(self.temporary.name)
        self.mock = self.path / "teagramd"
        self.state = self.path / "state"
        self.calls = self.path / "calls"
        self.state.write_text('{"state":"missing"}\n')
        self.mock.write_text("""#!/usr/bin/env python3
import json, os, pathlib, sys
state = pathlib.Path(os.environ['MOCK_STATE'])
with pathlib.Path(os.environ['MOCK_CALLS']).open('a') as f:
    f.write(' '.join(sys.argv[1:]) + '\\n')
args = sys.argv[1:]
if args == ['bot', 'lookup', 'teagram_qa_test_bot']:
    print(state.read_text().strip())
elif args == ['bot', 'encrypt', '--check-recipient', os.environ['MOCK_RECIPIENT']]:
    pass
elif args == ['bot', 'create', 'teagram_qa_test_bot', 'Teagram QA Test'] or args == ['bot', 'rotate', '987654321']:
    state.write_text(json.dumps({'first_name':'Teagram QA Test', 'id':987654321, 'kind':'bot', 'owner':'user', 'state':'found', 'token_active':True}, separators=(',', ':'), sort_keys=True))
    print(os.environ.get('MOCK_TOKEN_OVERRIDE', os.environ['MOCK_TOKEN']))
elif args == ['bot', 'revoke', '987654321']:
    value = json.loads(state.read_text())
    value['token_active'] = False
    state.write_text(json.dumps(value, separators=(',', ':'), sort_keys=True))
elif args == ['bot', 'encrypt', os.environ['MOCK_RECIPIENT']]:
    token = sys.stdin.read().strip()
    if token != os.environ['MOCK_TOKEN'] or os.environ.get('MOCK_ENCRYPT_FAIL'):
        raise SystemExit(1)
    print('-----BEGIN AGE ENCRYPTED FILE-----\\nciphertext\\n-----END AGE ENCRYPTED FILE-----')
else:
    raise SystemExit(99)
""")
        self.mock.chmod(0o700)
        source = SCRIPT.read_text()
        source = source.replace("readonly TEAGRAMD=/usr/local/bin/teagramd", f"readonly TEAGRAMD={self.mock}")
        source = source.replace('[ "$(id -u)" = 0 ]', f'[ "$(id -u)" = {os.getuid()} ]')
        self.script = self.path / "operation.sh"
        self.script.write_text(source)
        self.env = os.environ | {
            "MOCK_STATE": str(self.state),
            "MOCK_CALLS": str(self.calls),
            "MOCK_TOKEN": TOKEN,
            "MOCK_RECIPIENT": "age1p5s5le72wnxu465pe2xz5a3qv8mdykwp5leezpjk0zm6p0m3e5hs36fydn",
        }

    def invoke(self, *args, extra_env=None):
        result = subprocess.run(["sh", str(self.script), *args], text=True, capture_output=True,
                                env=self.env | (extra_env or {}), check=False)
        self.assertNotIn(TOKEN, result.stdout + result.stderr)
        if self.calls.exists():
            self.assertNotIn(TOKEN, self.calls.read_text())
        return result

    def test_each_operation_and_idempotence(self):
        self.assertIn("state=missing", self.invoke("status").stdout)
        self.assertNotEqual(self.invoke("probe").returncode, 0)
        created = self.invoke("create")
        self.assertEqual(created.returncode, 0, created.stderr)
        self.assertIn("action=create bot_id=987654321", created.stdout)
        self.assertIn("BEGIN AGE ENCRYPTED FILE", created.stdout)
        self.assertIn("state=exists", self.invoke("create").stdout)
        self.assertIn("token_active=true", self.invoke("status").stdout)
        self.assertEqual(self.invoke("probe").returncode, 0)
        rotated = self.invoke("rotate")
        self.assertEqual(rotated.returncode, 0, rotated.stderr)
        self.assertIn("action=rotate", rotated.stdout)
        self.assertIn("state=revoked", self.invoke("revoke").stdout)
        revocations = self.calls.read_text().count("bot revoke")
        self.assertIn("state=revoked", self.invoke("revoke").stdout)
        self.assertEqual(self.calls.read_text().count("bot revoke"), revocations)
        self.assertNotEqual(self.invoke("probe").returncode, 0)

    def test_refuses_unpinned_names_arguments_and_non_bot_accounts(self):
        for arguments in [(), ("unknown",), ("create", "other_bot"), ("rotate", "1"), ("probe", "extra")]:
            self.assertEqual(self.invoke(*arguments).returncode, 64)
        for record in [
            {"state": "found", "owner": "channel", "id": 9},
            {"first_name": "Other", "id": 987654321, "kind": "bot", "owner": "user", "state": "found", "token_active": True},
            {"first_name": "Teagram QA Test", "id": 987654321, "kind": "human", "owner": "user", "state": "found", "token_active": True},
        ]:
            self.state.write_text(json.dumps(record, separators=(",", ":"), sort_keys=True))
            before = self.calls.read_text() if self.calls.exists() else ""
            self.assertNotEqual(self.invoke("revoke").returncode, 0)
            self.assertNotIn("bot revoke", self.calls.read_text()[len(before):])

    def test_encryption_failure_revokes_only_the_pinned_bot(self):
        result = self.invoke("create", extra_env={"MOCK_ENCRYPT_FAIL": "1"})
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")
        self.assertFalse(json.loads(self.state.read_text())["token_active"])
        self.assertIn("bot revoke 987654321", self.calls.read_text())

    def test_mismatched_returned_id_is_not_revoked(self):
        result = self.invoke("create", extra_env={"MOCK_TOKEN_OVERRIDE": "42:" + "B" * 43})
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("bot revoke", self.calls.read_text())


class RenderTest(unittest.TestCase):
    def test_manifest_and_argument_boundaries(self):
        image = "ghcr.io/teagramhq/teagramd@sha256:" + "a" * 64
        for operation in ("create", "rotate", "revoke", "status", "probe"):
            result = subprocess.run(["python3", str(RENDER), image, operation], text=True, capture_output=True, check=True)
            job = json.loads(result.stdout)
            pod = job["spec"]["template"]["spec"]
            container = pod["containers"][0]
            self.assertEqual(job["metadata"]["name"], "qa-bot-operation")
            self.assertEqual(job["spec"]["backoffLimit"], 0)
            self.assertFalse(pod["automountServiceAccountToken"])
            self.assertEqual(container["image"], image)
            self.assertEqual(container["command"][-1], operation)
            self.assertEqual(container["command"][-3], SCRIPT.read_text())
            self.assertEqual(container["securityContext"]["capabilities"]["drop"], ["ALL"])
            self.assertTrue(container["securityContext"]["readOnlyRootFilesystem"])
            self.assertEqual(container["securityContext"]["runAsGroup"], 65532)
            self.assertTrue(next(v for v in container["volumeMounts"] if v["name"] == "key-copy")["readOnly"])
            self.assertEqual(next(v for v in pod["volumes"] if v["name"] == "key-copy")["emptyDir"]["medium"], "Memory")
            self.assertTrue(next(v for v in pod["initContainers"][1]["volumeMounts"] if v["name"] == "key-data")["readOnly"])
        for args in [(image, "other"), ("other:ci", "create"), (image, "create", "other_bot")]:
            self.assertNotEqual(subprocess.run(["python3", str(RENDER), *args], capture_output=True, check=False).returncode, 0)


if __name__ == "__main__":
    unittest.main()
