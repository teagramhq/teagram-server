#!/usr/bin/env python3
"""Check the demo app, discovery document, edge policy, and WebSocket origin."""

from __future__ import annotations

import argparse
import base64
import hashlib
import http.client
import json
import pathlib
import re
import secrets
import sys
import urllib.error
import urllib.parse
import urllib.request


WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
DEMO_CSP = (
    "default-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; "
    "frame-src 'self' https://challenges.cloudflare.com; object-src 'none'; "
    "script-src 'self' 'wasm-unsafe-eval' https://challenges.cloudflare.com; "
    "style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; "
    "media-src 'self' blob:; worker-src 'self' blob:; manifest-src 'self'; "
    "connect-src 'self' wss://demo.teagram.io https://challenges.cloudflare.com;"
)


def fail(message: str) -> None:
    print(f"demo smoke failed: {message}", file=sys.stderr)
    raise SystemExit(1)


def read_response(url: str, *, headers: dict[str, str] | None = None) -> tuple[int, object, bytes]:
    request = urllib.request.Request(url, headers=headers or {})
    try:
        response = urllib.request.urlopen(request, timeout=5)
    except urllib.error.HTTPError as error:
        return error.code, error.headers, error.read()
    except (OSError, urllib.error.URLError) as error:
        fail(f"request to {url} failed: {error}")
    with response:
        return response.status, response.headers, response.read()


def check_security_headers(headers: object) -> None:
    csp = headers.get("Content-Security-Policy", "")
    if csp != DEMO_CSP:
        fail("Content-Security-Policy differs from the approved demo web policy")
    for name, expected in (
        ("X-Content-Type-Options", "nosniff"),
        ("X-Frame-Options", "DENY"),
        ("Referrer-Policy", "no-referrer"),
    ):
        if headers.get(name) != expected:
            fail(f"{name} returned {headers.get(name)!r}, want {expected!r}")


def websocket_handshake(port: int, origin: str, expected_status: int) -> None:
    key = base64.b64encode(secrets.token_bytes(16)).decode("ascii")
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
    try:
        connection.request(
            "GET",
            "/apiws",
            headers={
                "Host": "demo.teagram.io",
                "Origin": origin,
                "CF-Connecting-IP": "192.0.2.1",
                "Connection": "Upgrade",
                "Upgrade": "websocket",
                "Sec-WebSocket-Key": key,
                "Sec-WebSocket-Version": "13",
            },
        )
        response = connection.getresponse()
        if response.status != expected_status:
            fail(f"WebSocket handshake from {origin} returned HTTP {response.status}, want {expected_status}")
        if expected_status == 101:
            expected_accept = base64.b64encode(
                hashlib.sha1((key + WS_GUID).encode("ascii")).digest()
            ).decode("ascii")
            if response.getheader("Sec-WebSocket-Accept") != expected_accept:
                fail("WebSocket handshake accept value does not match the request key")
    except OSError as error:
        fail(f"WebSocket handshake from {origin} failed: {error}")
    finally:
        connection.close()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bootstrap", action="store_true")
    parser.add_argument("--web-source-commit")
    parser.add_argument("--web-artifact-digest")
    parser.add_argument("client_config")
    parser.add_argument("loopback_port", nargs="?", type=int, default=8080)
    args = parser.parse_args()
    if bool(args.web_source_commit) != bool(args.web_artifact_digest):
        fail("web source commit and artifact digest must be supplied together")
    if args.bootstrap and (args.web_source_commit or args.web_artifact_digest):
        fail("bootstrap mode cannot assert a web artifact identity")
    if args.web_source_commit and not re.fullmatch(r"[0-9a-f]{40}", args.web_source_commit):
        fail("web source commit must be a full 40-hex commit")
    if args.web_artifact_digest and not re.fullmatch(r"sha256:[0-9a-f]{64}", args.web_artifact_digest):
        fail("web artifact digest must be sha256")
    port = args.loopback_port
    if not 1 <= port <= 65535:
        fail("loopback-port must be between 1 and 65535")

    base_url = f"http://127.0.0.1:{port}"
    try:
        expected = json.loads(pathlib.Path(args.client_config).read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        fail(f"client config is invalid: {error}")

    status, headers, body = read_response(base_url + "/")
    if status != 200:
        fail(f"root HTTP status is {status}")
    check_security_headers(headers)
    content_type = headers.get("Content-Type", "")
    if args.bootstrap:
        if content_type.split(";", 1)[0] != "text/plain" or body != b"Teagram demo server":
            fail("bootstrap root response does not match the demo status page")
    else:
        if content_type.split(";", 1)[0] != "text/html":
            fail(f"root Content-Type is {content_type!r}, want text/html")
        html = body.decode("utf-8", errors="replace")
        if "<title>Teagram Web</title>" not in html or "type=\"module\"" not in html:
            fail("root response does not contain the Teagram Web app shell")

        config_status, config_headers, config_body = read_response(base_url + "/teagram-config.json")
        if config_status != 200 or config_headers.get("Content-Type", "").split(";", 1)[0] != "application/json":
            fail("/teagram-config.json did not return application/json")
        if config_body != b'{"server":"demo.teagram.io","autoGuest":true}':
            fail("/teagram-config.json differs from the approved demo config")

        manifest_status, manifest_headers, manifest_body = read_response(base_url + "/mtproto-target.json")
        if manifest_status != 200 or manifest_headers.get("Content-Type", "").split(";", 1)[0] != "application/json":
            fail("/mtproto-target.json did not return application/json")
        try:
            manifest = json.loads(manifest_body)
        except json.JSONDecodeError as error:
            fail(f"/mtproto-target.json is invalid JSON: {error}")
        if (
            not isinstance(manifest, dict)
            or set(manifest) != {"mode", "endpoint", "fingerprint", "sourceCommit", "artifactDigest"}
            or manifest.get("mode") != "private"
            or not isinstance(manifest.get("endpoint"), str)
            or not manifest["endpoint"].startswith("wss://")
            or not re.fullmatch(r"[0-9a-f]{16}", str(manifest.get("fingerprint", "")))
            or not re.fullmatch(r"[0-9a-f]{40}", str(manifest.get("sourceCommit", "")))
            or not re.fullmatch(r"sha256:[0-9a-f]{64}", str(manifest.get("artifactDigest", "")))
        ):
            fail("/mtproto-target.json has an invalid private artifact identity")
        if args.web_source_commit and manifest.get("sourceCommit") != args.web_source_commit:
            fail("/mtproto-target.json source commit differs from the requested web release")
        if args.web_artifact_digest and manifest.get("artifactDigest") != args.web_artifact_digest:
            fail("/mtproto-target.json artifact digest differs from the requested web release")

        route_status, _, route_body = read_response(base_url + "/demo-smoke-route")
        if route_status != 200 or route_body != body:
            fail("SPA fallback did not serve index.html for a client route")

        asset_match = re.search(r"(?:src|href)=\"([^\"]+\.(?:m?js|css)(?:\?[^\"]*)?)\"", html)
        if asset_match is None:
            fail("root app shell does not reference a JavaScript or CSS asset")
        asset_url = urllib.parse.urljoin(base_url + "/", asset_match.group(1))
        if urllib.parse.urlparse(asset_url).netloc != f"127.0.0.1:{port}":
            fail("root app shell references an asset outside the demo origin")
        asset_status, asset_headers, _ = read_response(asset_url)
        asset_type = asset_headers.get("Content-Type", "")
        if asset_status != 200 or not ("javascript" in asset_type or "text/css" in asset_type):
            fail("the referenced app asset was not served with its static content type")

    request = urllib.request.Request(
        base_url + "/.well-known/teagramd/client",
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

    health_status, _, _ = read_response(base_url + "/healthz")
    if health_status != 200:
        fail(f"upstream health HTTP status is {health_status}")

    admin_status, _, _ = read_response(base_url + "/admin/")
    if admin_status != 404:
        fail(f"/admin/ returned HTTP {admin_status}, want 404")

    websocket_handshake(port, "https://demo.teagram.io", 101)
    websocket_handshake(port, "https://foreign.example", 403)
    print("demo smoke=passed app=html config=valid discovery=cors health=200 websocket=101 foreign_origin=403 admin=404")


if __name__ == "__main__":
    main()
