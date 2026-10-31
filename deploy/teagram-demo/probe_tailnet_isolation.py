#!/usr/bin/env python3
"""Require the demo LXC's known private-network TCP probes to time out."""

from __future__ import annotations

import socket
import sys
from collections.abc import Callable


TARGETS = (
    ("tailnet", "100.124.236.66", 22),
    ("charlie", "192.168.64.128", 22),
    ("router", "192.168.64.1", 80),
)


def fail(message: str) -> None:
    print(f"demo isolation probe failed: {message}", file=sys.stderr)
    raise SystemExit(1)


def probe_target(
    name: str,
    address: str,
    port: int,
    connect: Callable[..., socket.socket] | None = None,
) -> None:
    if connect is None:
        connect = socket.create_connection
    try:
        connection = connect((address, port), timeout=3)
    except TimeoutError:
        return
    except OSError as error:
        fail(f"{name}-probe-error:{type(error).__name__}")
    connection.close()
    fail(f"{name}-destination-reachable")


def main() -> None:
    for name, address, port in TARGETS:
        probe_target(name, address, port)
    print("tailnet_isolation_probe=pass tailnet=timeout charlie=timeout router=timeout")


if __name__ == "__main__":
    main()
