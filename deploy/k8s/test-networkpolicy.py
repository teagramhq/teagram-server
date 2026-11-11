#!/usr/bin/env python3
"""Exercise trusted tailnet ingress and denied non-proxy ingress from CI pods."""

from __future__ import annotations

import argparse
import http.client
import ipaddress
import json
import pathlib
import socket
import ssl
import sys

from probe_discovery import probe as probe_discovery


BOT_API_AUTH_BURST = 20
TAILNET_SERVICE_PORTS = (2443, 2444, 2445, 2447)
PUBLIC_SERVICE_PORTS = (2446, 2449)


def connect(host: str, port: int, timeout: float = 1.5) -> socket.socket:
    return socket.create_connection((host, port), timeout=timeout)


def require_blocked(host: str, port: int, listener: str) -> None:
    socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
    try:
        connection = connect(host, port)
    except OSError:
        return
    connection.close()
    raise RuntimeError(f"pod reached denied {listener} listener {host}:{port}")


def require_open(host: str, port: int) -> None:
    with connect(host, port) as connection:
        connection.shutdown(socket.SHUT_WR)


def probe_linkselector(host: str) -> None:
    connection = http.client.HTTPConnection(host, 8081, timeout=5)
    try:
        connection.request("GET", "/+k3s-smoke", headers={"Connection": "close"})
        response = connection.getresponse()
        body = response.read()
    finally:
        connection.close()
    if response.status != 200 or b"Open this in Teagram" not in body:
        raise RuntimeError(f"linkselector returned HTTP {response.status}")


def probe_bot_api(host: str) -> None:
    with connect(host, 2447, timeout=5) as connection:
        connection.settimeout(5)
        connection.sendall(
            b"PROXY TCP4 198.51.100.10 127.0.0.1 43000 2447\r\n"
            b"GET /botbad-token/getMe HTTP/1.1\r\n"
            b"Host: teagramd\r\n"
            b"Connection: close\r\n\r\n"
        )
        status_line = connection.makefile("rb").readline().decode("ascii", "replace")
    fields = status_line.split()
    if len(fields) < 2 or fields[1] != "401":
        raise RuntimeError(f"unauthenticated Bot API request returned {status_line.strip()!r}, want 401")


def probe_public_ingress(host: str) -> None:
    discovery = http.client.HTTPConnection(host, 2446, timeout=5)
    try:
        discovery.request(
            "GET",
            "/.well-known/teagramd/client",
            headers={
                "CF-Connecting-IP": "203.0.113.10",
                "Host": "server.example.test",
                "Connection": "close",
            },
        )
        discovery_response = discovery.getresponse()
        discovery_body = discovery_response.read()
    finally:
        discovery.close()
    if discovery_response.status != 200:
        raise RuntimeError(
            f"public discovery returned HTTP {discovery_response.status}, want 200"
        )
    try:
        document = json.loads(discovery_body)
    except json.JSONDecodeError as error:
        raise RuntimeError("public discovery returned invalid JSON") from error
    public_endpoint = document.get("websocket", {}).get("endpoint")
    if (
        document.get("version") != 1
        or public_endpoint != "wss://server.example.test/apiws"
    ):
        raise RuntimeError(
            "public discovery did not advertise wss://server.example.test/apiws"
        )

    bot_api = http.client.HTTPConnection(host, 2449, timeout=5)
    try:
        bot_api.request(
            "GET",
            "/botbad-token/getMe",
            headers={
                "CF-Connecting-IP": "203.0.113.10",
                "Host": "server.example.test",
                "Connection": "close",
            },
        )
        bot_api_response = bot_api.getresponse()
        bot_api_response.read()
    finally:
        bot_api.close()
    if bot_api_response.status != 401:
        raise RuntimeError(
            "unauthenticated public Bot API returned "
            f"HTTP {bot_api_response.status}, want 401"
        )


def public_discovery_status(host: str, client_ip: str) -> int:
    connection = http.client.HTTPConnection(host, 2446, timeout=5)
    try:
        connection.request(
            "GET",
            "/.well-known/teagramd/client",
            headers={
                "CF-Connecting-IP": client_ip,
                "Host": "server.example.test",
                "Connection": "close",
            },
        )
        response = connection.getresponse()
        response.read()
        return response.status
    finally:
        connection.close()


def probe_public_discovery_budgets(host: str) -> None:
    for request_number in range(1, 12):
        status = public_discovery_status(host, "198.51.100.10")
        expected = 200 if request_number <= 10 else 429
        if status != expected:
            raise RuntimeError(
                f"public discovery client A request {request_number} returned "
                f"HTTP {status}, want {expected}"
            )

    other_status = public_discovery_status(host, "198.51.101.10")
    if other_status != 200:
        raise RuntimeError(
            f"public discovery client B returned HTTP {other_status} after A exhausted its budget, want 200"
        )


def public_bot_api_status(connection: http.client.HTTPConnection, client_ip: str) -> int:
    connection.request(
        "GET",
        "/botbad-token/getMe",
        headers={
            "CF-Connecting-IP": client_ip,
            "Host": "server.example.test",
        },
    )
    response = connection.getresponse()
    response.read()
    return response.status


def probe_public_bot_api_budget(host: str) -> None:
    with http.client.HTTPConnection(host, 2449, timeout=5) as client_a:
        for request_number in range(1, BOT_API_AUTH_BURST + 2):
            status = public_bot_api_status(client_a, "198.51.100.10")
            expected = 401 if request_number <= BOT_API_AUTH_BURST else 429
            if status != expected:
                raise RuntimeError(
                    f"public Bot API client A request {request_number} returned "
                    f"HTTP {status}, want {expected}"
                )

    with http.client.HTTPConnection(host, 2449, timeout=5) as client_b:
        other_status = public_bot_api_status(client_b, "198.51.101.10")
    if other_status != 401:
        raise RuntimeError(
            "public Bot API client B returned "
            f"HTTP {other_status} after client A exhausted its auth budget, want 401 (not 429)"
        )


def probe_untrusted_public_header(host: str, trusted_proxy_ip: str) -> None:
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as route:
        route.connect((host, 2446))
        peer = ipaddress.ip_address(route.getsockname()[0])
    trusted_proxy = ipaddress.ip_address(trusted_proxy_ip)
    if peer == trusted_proxy:
        raise RuntimeError(
            f"untrusted probe source {peer} is the trusted cloudflared address {trusted_proxy}"
        )

    for request_number in range(1, 12):
        forwarded_ip = f"198.51.{request_number}.10"
        status = public_discovery_status(host, forwarded_ip)
        expected = 200 if request_number <= 10 else 429
        if status != expected:
            raise RuntimeError(
                f"untrusted source {peer} request {request_number} with "
                f"CF-Connecting-IP {forwarded_ip} returned HTTP {status}, want {expected}; "
                "the untrusted header may have been credited"
            )


def probe_tailnet_discovery_budget(host: str) -> None:
    for request_number in range(1, 51):
        connection = http.client.HTTPConnection(host, 2444, timeout=5)
        try:
            connection.request(
                "GET",
                "/.well-known/teagramd/client",
                headers={"Host": "teagramd", "Connection": "close"},
            )
            response = connection.getresponse()
            response.read()
        finally:
            connection.close()
        if response.status != 200:
            raise RuntimeError(
                f"tailnet discovery request {request_number} returned HTTP {response.status}, want 200"
            )


def probe_kubernetes_api() -> None:
    service_account_dir = pathlib.Path(
        "/var/run/secrets/kubernetes.io/serviceaccount"
    )
    token = (service_account_dir / "token").read_text(encoding="utf-8").strip()
    if not token:
        raise RuntimeError("Kubernetes API probe has no service-account token")
    connection = http.client.HTTPSConnection(
        "kubernetes.default.svc",
        443,
        timeout=5,
        context=ssl.create_default_context(
            cafile=str(service_account_dir / "ca.crt")
        ),
    )
    try:
        connection.request(
            "GET",
            "/version",
            headers={
                "Authorization": f"Bearer {token}",
                "Connection": "close",
            },
        )
        response = connection.getresponse()
        body = response.read()
    finally:
        connection.close()
    if response.status != 200:
        raise RuntimeError(
            f"Kubernetes API /version returned HTTP {response.status}, want 200"
        )
    try:
        version = json.loads(body)
    except json.JSONDecodeError as error:
        raise RuntimeError("Kubernetes API /version returned invalid JSON") from error
    if not isinstance(version, dict) or not version.get("gitVersion"):
        raise RuntimeError("Kubernetes API /version response has no gitVersion")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "operation",
        choices=(
            "discovery",
            "linkselector",
            "bot-api",
            "public-ingress",
            "public-discovery-budget",
            "public-bot-api-budget",
            "untrusted-public-header",
            "tailnet-discovery-budget",
            "kube-api",
            "check-listeners",
            "deny-tailnet",
            "deny-public",
            "deny-egress",
        ),
    )
    parser.add_argument("--host", default="teagramd")
    parser.add_argument("--trusted-proxy-ip")
    args = parser.parse_args()

    if args.operation == "discovery":
        size = probe_discovery(args.host, 2443, timeout=5)
        print(f"discovery half-close succeeded: {size} bytes from {args.host}:2443")
    elif args.operation == "linkselector":
        probe_linkselector("linkselector")
    elif args.operation == "bot-api":
        probe_bot_api(args.host)
    elif args.operation == "public-ingress":
        probe_public_ingress(args.host)
    elif args.operation == "public-discovery-budget":
        probe_public_discovery_budgets(args.host)
    elif args.operation == "public-bot-api-budget":
        probe_public_bot_api_budget(args.host)
    elif args.operation == "untrusted-public-header":
        if not args.trusted_proxy_ip:
            raise RuntimeError("untrusted-public-header requires --trusted-proxy-ip")
        probe_untrusted_public_header(args.host, args.trusted_proxy_ip)
    elif args.operation == "tailnet-discovery-budget":
        probe_tailnet_discovery_budget(args.host)
    elif args.operation == "kube-api":
        probe_kubernetes_api()
    elif args.operation == "check-listeners":
        for port in (2444, 2445):
            require_open(args.host, port)
    else:
        if args.operation == "deny-tailnet":
            for port in TAILNET_SERVICE_PORTS:
                require_blocked(args.host, port, "tailnet-only")
        elif args.operation == "deny-public":
            for port in PUBLIC_SERVICE_PORTS:
                require_blocked(args.host, port, "public")
        else:
            require_blocked("postgres", 5432, "unapproved egress")
            require_blocked("rustfs", 9000, "unapproved egress")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as error:
        print(f"network policy probe failed: {error}", file=sys.stderr)
        raise SystemExit(1)
