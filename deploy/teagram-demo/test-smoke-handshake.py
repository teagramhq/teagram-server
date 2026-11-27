#!/usr/bin/env python3
"""Exercise smoke WebSocket dialing and Host headers against a local stub."""

from __future__ import annotations

import base64
import hashlib
import http.client
import http.server
import pathlib
import sys
import threading
import unittest
from unittest import mock


SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPT_DIR))
import smoke  # noqa: E402


class HandshakeHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self) -> None:
        self.server.seen_hosts.append(self.headers.get("Host"))
        key = self.headers.get("Sec-WebSocket-Key", "")
        accept = base64.b64encode(
            hashlib.sha1((key + smoke.WS_GUID).encode("ascii")).digest()
        ).decode("ascii")
        self.send_response(101)
        self.send_header("Upgrade", "websocket")
        self.send_header("Connection", "Upgrade")
        self.send_header("Sec-WebSocket-Accept", accept)
        self.end_headers()

    def log_message(self, _format: str, *_args: object) -> None:
        pass


class SmokeHandshakeTests(unittest.TestCase):
    def setUp(self) -> None:
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), HandshakeHandler)
        self.server.seen_hosts = []
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.targets: list[tuple[str, int]] = []
        self.real_http_connection = http.client.HTTPConnection
        self.real_https_connection = http.client.HTTPSConnection

        def local_connection(host: str, port: int, timeout: float = 5) -> object:
            self.targets.append((host, port))
            connection = self.real_http_connection("127.0.0.1", self.server.server_port, timeout=timeout)
            return connection

        self.local_connection = local_connection
        self.http_patch = mock.patch.object(smoke.http.client, "HTTPConnection", self.local_connection)
        self.https_patch = mock.patch.object(smoke.http.client, "HTTPSConnection", self.local_connection)
        self.http_patch.start()
        self.https_patch.start()

    def tearDown(self) -> None:
        self.http_patch.stop()
        self.https_patch.stop()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)

    def test_tailnet_host_is_only_the_host_header(self) -> None:
        smoke.websocket_handshake(
            self.server.server_port,
            "https://teagram-dev.tailaa4918.ts.net",
            101,
            host="teagram-dev.tailaa4918.ts.net",
        )

        self.assertEqual(self.targets, [("127.0.0.1", self.server.server_port)])
        self.assertEqual(self.server.seen_hosts, ["teagram-dev.tailaa4918.ts.net"])

    def test_local_handshake_defaults_to_public_host_header(self) -> None:
        smoke.websocket_handshake(self.server.server_port, None, 101)

        self.assertEqual(self.targets, [("127.0.0.1", self.server.server_port)])
        self.assertEqual(self.server.seen_hosts, ["demo.teagram.io"])

    def test_public_handshake_connects_to_public_host(self) -> None:
        smoke.websocket_handshake(443, "https://demo.teagram.io", 101, public=True)

        self.assertEqual(self.targets, [("demo.teagram.io", 443)])
        self.assertEqual(self.server.seen_hosts, ["demo.teagram.io"])


if __name__ == "__main__":
    unittest.main()
