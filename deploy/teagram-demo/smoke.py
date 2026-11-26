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
TAILNET_WEBSOCKET_ENDPOINT = "wss://teagram-dev.tailaa4918.ts.net/apiws"
DEMO_CSP = (
    "default-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; "
    "frame-src 'self' https://challenges.cloudflare.com; object-src 'none'; "
    "script-src 'self' 'wasm-unsafe-eval' https://challenges.cloudflare.com; "
    "style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; "
    "media-src 'self' blob:; worker-src 'self' blob:; manifest-src 'self'; "
    "connect-src 'self' wss://demo.teagram.io https://challenges.cloudflare.com;"
)
TAILNET_CSP = DEMO_CSP.replace(
    "wss://demo.teagram.io",
    "wss://demo.teagram.io wss://teagram-dev.tailaa4918.ts.net",
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


def check_security_headers(headers: object, *, expected_csp: str = DEMO_CSP) -> None:
    csp = headers.get("Content-Security-Policy", "")
    if csp != expected_csp:
        fail("Content-Security-Policy differs from the approved demo web policy")
    for name, expected in (
        ("X-Content-Type-Options", "nosniff"),
        ("X-Frame-Options", "DENY"),
        ("Referrer-Policy", "no-referrer"),
    ):
        if headers.get(name) != expected:
            fail(f"{name} returned {headers.get(name)!r}, want {expected!r}")


def websocket_handshake(
    port: int,
    origin: str | None,
    expected_status: int,
    expected_policy: str | None = None,
    *,
    public: bool = False,
    host: str | None = None,
    cf_connecting_ip: str | None = "192.0.2.1",
    duplicate_cf_connecting_ip: str | None = None,
    x_forwarded_for: str | None = None,
) -> None:
    key = base64.b64encode(secrets.token_bytes(16)).decode("ascii")
    host = host or "demo.teagram.io"
    connection_type = http.client.HTTPSConnection if public else http.client.HTTPConnection
    connect_host = "demo.teagram.io" if public else "127.0.0.1"
    connection = connection_type(connect_host, 443 if public else port, timeout=5)
    source = f"to wss://demo.teagram.io/apiws from {origin}" if public else f"from {origin}"
    if origin is None:
        source = f"to wss://demo.teagram.io/apiws without Origin" if public else "without Origin"
    headers = {
        "Host": host,
        "Connection": "Upgrade",
        "Upgrade": "websocket",
        "Sec-WebSocket-Key": key,
        "Sec-WebSocket-Version": "13",
    }
    if not public and cf_connecting_ip is not None:
        headers["CF-Connecting-IP"] = cf_connecting_ip
    if not public and x_forwarded_for is not None:
        headers["X-Forwarded-For"] = x_forwarded_for
    if origin is not None:
        headers["Origin"] = origin
    try:
        connection.putrequest("GET", "/apiws", skip_host=True)
        for name, value in headers.items():
            connection.putheader(name, value)
        if not public and duplicate_cf_connecting_ip is not None:
            connection.putheader("CF-Connecting-IP", duplicate_cf_connecting_ip)
        connection.endheaders()
        response = connection.getresponse()
        if response.status != expected_status:
            fail(f"WebSocket handshake {source} returned HTTP {response.status}, want {expected_status}")
        if (
            expected_policy is not None
            and response.getheader("Teagram-Client-Policy") != expected_policy
        ):
            fail(f"WebSocket handshake {source} did not return Teagram-Client-Policy: {expected_policy}")
        if expected_status == 101:
            expected_accept = base64.b64encode(
                hashlib.sha1((key + WS_GUID).encode("ascii")).digest()
            ).decode("ascii")
            if response.getheader("Sec-WebSocket-Accept") != expected_accept:
                fail("WebSocket handshake accept value does not match the request key")
    except OSError as error:
        fail(f"WebSocket handshake {source} failed: {error}")
    finally:
        connection.close()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bootstrap", action="store_true")
    parser.add_argument("--web-source-commit")
    parser.add_argument("--web-artifact-digest")
    parser.add_argument("--tailnet-port", type=int)
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
    if args.tailnet_port is not None and not 1 <= args.tailnet_port <= 65535:
        fail("tailnet-port must be between 1 and 65535")

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

    websocket_handshake(port, None, 403, "browser-only")
    websocket_handshake(port, "https://demo.teagram.io", 101)
    websocket_handshake(port, "https://web.teagram.io", 101)
    websocket_handshake(port, "https://foreign.example", 403)
    websocket_handshake(port, "https://teagram-dev.tailaa4918.ts.net", 403)
    websocket_handshake(port, "https://demo.teagram.io", 400, cf_connecting_ip=None, x_forwarded_for="192.0.2.44")
    websocket_handshake(port, "https://demo.teagram.io", 400, cf_connecting_ip="not-an-ip")
    websocket_handshake(
        port,
        "https://demo.teagram.io",
        400,
        cf_connecting_ip="192.0.2.44",
        duplicate_cf_connecting_ip="192.0.2.45",
    )
    websocket_handshake(443, None, 403, "browser-only", public=True)
    websocket_handshake(443, "https://demo.teagram.io", 101, public=True)
    websocket_handshake(443, "https://web.teagram.io", 101, public=True)
    websocket_handshake(443, "https://teagram-dev.tailaa4918.ts.net", 403, public=True)

    if args.tailnet_port is not None:
        tailnet_url = f"http://127.0.0.1:{args.tailnet_port}"
        tailnet_status, tailnet_headers, tailnet_body = read_response(tailnet_url + "/")
        if tailnet_status != 200 or tailnet_body != body:
            fail("tailnet edge root response differs from the public edge")
        check_security_headers(tailnet_headers, expected_csp=TAILNET_CSP)
        tailnet_admin_status, _, _ = read_response(tailnet_url + "/admin/")
        if tailnet_admin_status != 404:
            fail(f"tailnet /admin/ returned HTTP {tailnet_admin_status}, want 404")
        tailnet_discovery_status, _, tailnet_discovery_body = read_response(
            tailnet_url + "/.well-known/teagramd/client"
        )
        if tailnet_discovery_status != 200:
            fail(f"tailnet discovery HTTP status is {tailnet_discovery_status}")
        try:
            tailnet_discovery = json.loads(tailnet_discovery_body)
            expected_tailnet_discovery = json.loads(json.dumps(expected))
            expected_tailnet_discovery["websocket"]["endpoint"] = TAILNET_WEBSOCKET_ENDPOINT
        except (json.JSONDecodeError, KeyError, TypeError) as error:
            fail(f"tailnet discovery document is invalid: {error}")
        if tailnet_discovery != expected_tailnet_discovery:
            fail("tailnet discovery document differs from the dev-specific client config")
        if tailnet_discovery.get("websocket", {}).get("endpoint") != TAILNET_WEBSOCKET_ENDPOINT:
            fail("tailnet discovery WebSocket endpoint is incorrect")
        websocket_handshake(
            args.tailnet_port,
            "https://teagram-dev.tailaa4918.ts.net",
            101,
            host="teagram-dev.tailaa4918.ts.net",
            cf_connecting_ip="198.51.100.9",
            x_forwarded_for="203.0.113.8, 192.0.2.44",
        )
        websocket_handshake(
            args.tailnet_port,
            "https://foreign.example",
            403,
            host="teagram-dev.tailaa4918.ts.net",
            cf_connecting_ip="198.51.100.9",
            x_forwarded_for="192.0.2.44",
        )
        websocket_handshake(
            args.tailnet_port,
            "https://teagram-dev.tailaa4918.ts.net",
            400,
            host="teagram-dev.tailaa4918.ts.net",
            x_forwarded_for=None,
        )
        websocket_handshake(
            args.tailnet_port,
            "https://teagram-dev.tailaa4918.ts.net",
            400,
            host="teagram-dev.tailaa4918.ts.net",
            x_forwarded_for="not-an-ip",
        )
    print(
        "demo smoke=passed app=html config=valid discovery=cors health=200 "
        "local_websocket=101 public_websocket=101 no_origin=403 "
        "browser_only=present public_demo_origin=101 public_web_origin=101 "
        "foreign_origin=403 public_tailnet_origin=403 missing_cf_ip=400 "
        "invalid_cf_ip=400 duplicate_cf_ip=400 admin=404"
    )
    if args.tailnet_port is not None:
        print("tailnet_websocket=101 tailnet_foreign_origin=403 tailnet_missing_xff=400 tailnet_invalid_xff=400")


if __name__ == "__main__":
    main()
