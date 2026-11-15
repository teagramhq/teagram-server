#!/usr/bin/env python3
"""Exercise the workflow guard with real kubectl against a local API fixture."""

import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit


ROOT = pathlib.Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github/workflows/qa-bot.yml"


class FakeKubeAPI(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self):
        super().__init__(("127.0.0.1", 0), KubeHandler)
        self.settled = False
        self.requests = []
        self.release = threading.Event()

    def statefulset(self):
        revision = "new" if self.settled else "old"
        return {
            "apiVersion": "apps/v1", "kind": "StatefulSet",
            "metadata": {"name": "teagramd", "namespace": "teagram", "uid": "fixture",
                         "resourceVersion": "7", "generation": 2},
            "spec": {"serviceName": "teagramd", "replicas": 1,
                     "updateStrategy": {"type": "RollingUpdate"},
                     "selector": {"matchLabels": {"app": "teagramd"}},
                     "template": {"metadata": {"labels": {"app": "teagramd"}},
                                  "spec": {"containers": [{"name": "teagramd", "image": "example.invalid/test"}]}}},
            "status": {"observedGeneration": 2 if self.settled else 1,
                       "replicas": 1, "readyReplicas": 1, "currentReplicas": 1,
                       "updatedReplicas": 1 if self.settled else 0,
                       "currentRevision": revision, "updateRevision": "new"},
        }


class KubeHandler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def send_json(self, payload):
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        url = urlsplit(self.path)
        self.server.requests.append(self.path)
        if url.path == "/api":
            self.send_json({"kind": "APIVersions", "apiVersion": "v1", "versions": ["v1"]})
        elif url.path == "/apis":
            version = {"groupVersion": "apps/v1", "version": "v1"}
            self.send_json({"kind": "APIGroupList", "apiVersion": "v1",
                            "groups": [{"name": "apps", "versions": [version], "preferredVersion": version}]})
        elif url.path in ("/api/v1", "/apis/apps/v1"):
            group_version = "v1" if url.path == "/api/v1" else "apps/v1"
            resources = [] if group_version == "v1" else [{"name": "statefulsets", "singularName": "statefulset",
                        "namespaced": True, "kind": "StatefulSet", "verbs": ["get", "list", "watch"]}]
            self.send_json({"kind": "APIResourceList", "apiVersion": "v1", "groupVersion": group_version,
                            "resources": resources})
        elif url.path == "/apis/apps/v1/namespaces/teagram/statefulsets/teagramd":
            self.send_json(self.server.statefulset())
        elif url.path == "/apis/apps/v1/namespaces/teagram/statefulsets":
            if parse_qs(url.query).get("watch") == ["true"]:
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Connection", "close")
                self.end_headers()
                event = {"type": "ADDED", "object": self.server.statefulset()}
                bookmark = {"type": "BOOKMARK", "object": {"apiVersion": "apps/v1", "kind": "StatefulSet",
                            "metadata": {"resourceVersion": "7", "annotations": {"k8s.io/initial-events-end": "true"}}}}
                try:
                    self.wfile.write((json.dumps(event) + "\n" + json.dumps(bookmark) + "\n").encode())
                    self.wfile.flush()
                    self.server.release.wait(15)
                except (BrokenPipeError, ConnectionResetError):
                    pass
            else:
                self.send_json({"apiVersion": "apps/v1", "kind": "StatefulSetList",
                                "metadata": {"resourceVersion": "7"}, "items": [self.server.statefulset()]})
        else:
            self.send_error(404)


class RolloutGuardTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.real_kubectl = shutil.which("kubectl")
        if not cls.real_kubectl:
            raise RuntimeError("pinned kubectl is required")
        version = subprocess.run([cls.real_kubectl, "version", "--client", "-o", "json"],
                                 text=True, capture_output=True, check=True)
        if json.loads(version.stdout)["clientVersion"]["gitVersion"] != "v1.37.1":
            raise RuntimeError("QA rollout regression requires kubectl v1.37.1")
        cls.guard = subprocess.run(["yq", "-r", '.jobs.ops-qa-bot.steps[] | select(.name == "Check production rollout is settled") | .run', str(WORKFLOW)],
                                   text=True, capture_output=True, check=True).stdout
        if not cls.guard.strip():
            raise RuntimeError("QA rollout guard is missing")
        cls.server = FakeKubeAPI()
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.release.set()
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join()

    def invoke_guard(self, settled):
        self.server.settled = settled
        self.server.requests.clear()
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            config = {"apiVersion": "v1", "kind": "Config", "current-context": "fixture",
                      "clusters": [{"name": "fixture", "cluster": {"server": f"http://127.0.0.1:{self.server.server_port}"}}],
                      "contexts": [{"name": "fixture", "context": {"cluster": "fixture", "user": "fixture"}}],
                      "users": [{"name": "fixture", "user": {}}]}
            (root / "config").write_text(json.dumps(config))
            wrapper = root / "kubectl"
            wrapper.write_text('#!/bin/sh\nexec "$TEST_KUBECTL" --cache-dir "$TEST_KUBE_CACHE" "$@"\n')
            wrapper.chmod(0o700)
            env = os.environ | {"PATH": f"{root}:{os.environ['PATH']}", "KUBECONFIG": str(root / "config"),
                                "TEST_KUBECTL": self.real_kubectl, "TEST_KUBE_CACHE": str(root / "cache"),
                                "NO_PROXY": "127.0.0.1,localhost", "no_proxy": "127.0.0.1,localhost"}
            return subprocess.run(["bash", "-c", self.guard], text=True, capture_output=True,
                                  env=env, timeout=20, check=False)

    def test_ready_old_pod_with_unobserved_change_is_rejected(self):
        result = self.invoke_guard(False)
        detail = f"stdout={result.stdout!r} stderr={result.stderr!r} requests={self.server.requests!r}"
        self.assertNotEqual(result.returncode, 0, detail)
        self.assertIn("Waiting for statefulset spec update to be observed", result.stdout, detail)
        self.assertIn("production teagramd rollout is not settled", result.stderr, detail)

    def test_settled_rollout_passes(self):
        result = self.invoke_guard(True)
        detail = f"stdout={result.stdout!r} stderr={result.stderr!r} requests={self.server.requests!r}"
        self.assertEqual(result.returncode, 0, detail)
        self.assertIn("statefulset rolling update complete", result.stdout, detail)


if __name__ == "__main__":
    unittest.main()
