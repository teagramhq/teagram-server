#!/usr/bin/env python3
"""Check the loopback edge, discovery document, admin denial, and WebSocket."""

from __future__ import annotations

import base64
import hashlib
import http.client
import json
import os
import pathlib
import secrets
import sys
import urllib.error
import urllib.request


BASE_URL = "http://127.0.0.1:8080"
WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


def fail(message: str) -> None:
    print(f"demo smoke failed: {message}", file=sys.stderr)
    raise SystemExit(1)


def main() -> None:
    if len(sys.argv) != 2:
        fail("usage: smoke.py <client-config.json>")
    expected = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
    request = urllib.request.Request(
        BASE_URL + "/.well-known/telegramd/client",
        headers={"Origin": "https://web.teagram.io"},
    )
    try:
        with urllib.request.urlopen(request, timeout=5) as response:
            if response.status != 200:
                fail(f"discovery HTTP status is {response.status}")
            if response.headers.get("Access-Control-Allow-Origin") != "https://web.teagram.io":
                fail("discovery CORS origin does not match the web client")
            actual = json.loads(response.read())
    except (OSError, urllib.error.URLError, json.JSONDecodeError) as error:
        fail(f"discovery request failed: {error}")
    if actual != expected:
        fail("served discovery document differs from the generated client config")
    if actual.get("websocket", {}).get("endpoint") != "wss://demo.teagram.io/apiws":
        fail("discovery WebSocket endpoint is incorrect")

    try:
        with urllib.request.urlopen(BASE_URL + "/admin/", timeout=5) as response:
            admin_status = response.status
    except urllib.error.HTTPError as error:
        admin_status = error.code
    except (OSError, urllib.error.URLError) as error:
        fail(f"admin denial request failed: {error}")
    if admin_status != 404:
        fail(f"/admin/ returned HTTP {admin_status}, want 404")

    key = base64.b64encode(secrets.token_bytes(16)).decode("ascii")
    connection = http.client.HTTPConnection("127.0.0.1", 8080, timeout=5)
    try:
        connection.request(
            "GET",
            "/apiws",
            headers={
                "Host": "demo.teagram.io",
                "Origin": "https://web.teagram.io",
                "Connection": "Upgrade",
                "Upgrade": "websocket",
                "Sec-WebSocket-Key": key,
                "Sec-WebSocket-Version": "13",
            },
        )
        response = connection.getresponse()
        expected_accept = base64.b64encode(hashlib.sha1((key + WS_GUID).encode("ascii")).digest()).decode("ascii")
        if response.status != 101:
            fail(f"WebSocket handshake returned HTTP {response.status}, want 101")
        if response.getheader("Sec-WebSocket-Accept") != expected_accept:
            fail("WebSocket handshake accept value does not match the request key")
    except OSError as error:
        fail(f"WebSocket handshake failed: {error}")
    finally:
        connection.close()
    print("demo smoke=passed discovery=cors websocket=101 admin=404")


if __name__ == "__main__":
    main()
