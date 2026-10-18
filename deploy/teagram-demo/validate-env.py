#!/usr/bin/env python3
"""Validate the root-owned, fixed demo runtime configuration."""

from __future__ import annotations

import os
import pathlib
import re
import stat
import sys


EXPECTED = {
    "TG_PUBLIC_LINK_PREFIX": "https://demo.teagram.io/",
    "TG_ADVERTISE_ADDR": "demo.teagram.io:443",
    "TG_WEBSOCKET_LISTEN_ADDR": ":2446",
    "TG_WEBSOCKET_ALLOWED_ORIGINS": "https://web.teagram.io",
    "TG_REGISTRATION": "open",
    "TG_LOG_LOGIN_CODES": "false",
    "TG_MAX_FILE_BYTES": "16777216",
    "TG_MAX_USER_STORAGE_BYTES": "268435456",
    "TEAGRAM_DEMO_BLOB_PATH": "/srv/teagram-demo-blobs/data",
}


def reject(message: str) -> None:
    print(f"demo env rejected: {message}", file=sys.stderr)
    raise SystemExit(1)


def main() -> None:
    if len(sys.argv) != 2:
        reject("usage")
    path = pathlib.Path(sys.argv[1])
    try:
        metadata = path.lstat()
    except OSError:
        reject("file-unavailable")
    if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISREG(metadata.st_mode):
        reject("file-type")
    if metadata.st_uid != 0 or stat.S_IMODE(metadata.st_mode) != 0o600:
        reject("file-owner-or-mode")

    values: dict[str, str] = {}
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeDecodeError):
        reject("file-read")
    for line in lines:
        if not line or line.startswith("#"):
            continue
        name, separator, value = line.partition("=")
        if not separator or not re.fullmatch(r"[A-Z][A-Z0-9_]*", name) or name in values:
            reject("assignment-shape")
        values[name] = value

    expected_names = set(EXPECTED) | {
        "POSTGRES_PASSWORD", "TG_AUTHKEY_ENC_KEY",
        "TEAGRAM_DEMO_TURNSTILE_SITE_KEY", "TEAGRAM_DEMO_TURNSTILE_SECRET_KEY",
    }
    if set(values) != expected_names:
        reject("variable-set")
    if not re.fullmatch(r"[0-9a-f]{64}", values["POSTGRES_PASSWORD"]):
        reject("postgres-password-format")
    if not re.fullmatch(r"[0-9a-f]{64}", values["TG_AUTHKEY_ENC_KEY"]):
        reject("authkey-encryption-key-format")
    if values["POSTGRES_PASSWORD"] == values["TG_AUTHKEY_ENC_KEY"]:
        reject("secret-reuse")
    site_key = values["TEAGRAM_DEMO_TURNSTILE_SITE_KEY"]
    secret_key = values["TEAGRAM_DEMO_TURNSTILE_SECRET_KEY"]
    if not re.fullmatch(r"[A-Za-z0-9_-]+", site_key) or not re.fullmatch(r"[A-Za-z0-9_-]+", secret_key):
        reject("turnstile-key-format")
    if site_key == secret_key:
        reject("turnstile-key-reuse")
    if any(values[name] != value for name, value in EXPECTED.items()):
        reject("fixed-demo-settings")
    if os.path.realpath(values["TEAGRAM_DEMO_BLOB_PATH"]) != values["TEAGRAM_DEMO_BLOB_PATH"]:
        reject("blob-path-not-canonical")
    print("demo env=valid")


if __name__ == "__main__":
    main()
