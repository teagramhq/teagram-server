#!/usr/bin/env python3
"""Exercise trusted tailnet ingress and denied non-proxy ingress from CI pods."""

from __future__ import annotations

import argparse
import http.client
import socket
import sys

from probe_discovery import probe as probe_discovery


TAILNET_SERVICE_PORTS = (2443, 2444, 2445, 2447)


def connect(host: str, port: int, timeout: float = 1.5) -> socket.socket:
    return socket.create_connection((host, port), timeout=timeout)


def require_blocked(host: str, port: int) -> None:
    socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
    try:
        connection = connect(host, port)
    except OSError:
        return
    connection.close()
    raise RuntimeError(f"untrusted pod reached tailnet-only listener {host}:{port}")


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


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=("discovery", "linkselector", "bot-api", "check-listeners", "deny-tailnet"))
    parser.add_argument("--host", default="teagramd")
    args = parser.parse_args()

    if args.operation == "discovery":
        size = probe_discovery(args.host, 2443, timeout=5)
        print(f"discovery half-close succeeded: {size} bytes from {args.host}:2443")
    elif args.operation == "linkselector":
        probe_linkselector("linkselector")
    elif args.operation == "bot-api":
        probe_bot_api(args.host)
    elif args.operation == "check-listeners":
        for port in (2444, 2445):
            require_open(args.host, port)
    else:
        for port in TAILNET_SERVICE_PORTS:
            require_blocked(args.host, port)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as error:
        print(f"network policy probe failed: {error}", file=sys.stderr)
        raise SystemExit(1)
