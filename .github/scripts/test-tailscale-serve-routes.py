#!/usr/bin/env python3
"""Regress Tailscale Serve mount-prefix routing in the proxy config."""

from __future__ import annotations

import json
import sys
from urllib.parse import urlsplit


HOST = "teagram.tailaa4918.ts.net:443"
EXPECTED = {
    "/": ("linkselector:8081", "/"),
    "/apiws": ("teagramd:2444", "/apiws"),
    "/apiws/session": ("teagramd:2444", "/apiws/session"),
    "/.well-known/teagramd/client": (
        "teagramd:2444",
        "/.well-known/teagramd/client",
    ),
    "/.well-known/telegramd/client": (
        "teagramd:2444",
        "/.well-known/teagramd/client",
    ),
    "/admin/login": ("teagramd:2445", "/admin/login"),
}


def route(handlers: dict[str, dict[str, str]], path: str) -> tuple[str, str]:
    mounts = [
        mount
        for mount in handlers
        if mount == "/"
        or path == mount
        or path.startswith(mount.rstrip("/") + "/")
    ]
    if not mounts:
        raise ValueError(f"no Tailscale Serve handler matches {path}")
    mount = max(mounts, key=len)
    target = urlsplit(handlers[mount]["Proxy"])

    # Tailscale strips the selected mount prefix before proxying. For an exact
    # mount request its reverse proxy sends the configured target path as-is.
    suffix = path if path == "/" else path[len(mount.rstrip("/")) :]
    if not suffix:
        backend_path = target.path
    else:
        backend_path = target.path.rstrip("/") + "/" + suffix.lstrip("/")
    return target.netloc, backend_path or "/"


def main() -> None:
    serve_config = json.load(sys.stdin)
    handlers = serve_config["Web"][HOST]["Handlers"]
    for request_path, expected in EXPECTED.items():
        actual = route(handlers, request_path)
        if actual != expected:
            raise SystemExit(
                f"Serve route {request_path} reaches {actual}, expected {expected}"
            )


if __name__ == "__main__":
    main()
