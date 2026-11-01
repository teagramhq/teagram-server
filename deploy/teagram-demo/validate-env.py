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
    "TG_REGISTRATION": "open",
    "TG_LOG_LOGIN_CODES": "false",
    "TG_MAX_FILE_BYTES": "16777216",
    "TG_MAX_USER_STORAGE_BYTES": "268435456",
    "TG_UPLOADS_DISABLED": "true",
    "TEAGRAM_DEMO_BLOB_PATH": "/srv/teagram-demo-blobs/data",
    "TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE": "/var/lib/teagram-demo/secrets/turnstile-secret-key",
}
OPTIONAL_VALUES = {
    "TG_ADVERTISE_DEMO_GUEST_SIGN_UP": {"true"},
    "TG_TURNSTILE_SIGNIN_ENABLED": {"false", "true"},
}
QA_BYPASS_TOKEN_PATH = pathlib.Path("/var/lib/teagram-demo/secrets/turnstile-qa-bypass-token")


def reject(message: str) -> None:
    print(f"demo env rejected: {message}", file=sys.stderr)
    raise SystemExit(1)


def read_runtime_secret(path: pathlib.Path, name: str, pattern: str, format_error: str) -> str:
    try:
        secret_dir_metadata = path.parent.lstat()
        secret_metadata = path.lstat()
        secret_key = path.read_text(encoding="utf-8").strip()
    except (OSError, UnicodeDecodeError):
        reject("turnstile-secret-file-unavailable")
    if (
        stat.S_ISLNK(secret_dir_metadata.st_mode)
        or not stat.S_ISDIR(secret_dir_metadata.st_mode)
        or secret_dir_metadata.st_uid != 0
        or stat.S_IMODE(secret_dir_metadata.st_mode) != 0o700
    ):
        reject(f"{name}-directory-owner-or-mode")
    if (
        stat.S_ISLNK(secret_metadata.st_mode)
        or not stat.S_ISREG(secret_metadata.st_mode)
        or secret_metadata.st_uid != 0
        or secret_metadata.st_gid != 65532
        or stat.S_IMODE(secret_metadata.st_mode) != 0o440
    ):
        reject(f"{name}-file-owner-group-or-mode")
    if not re.fullmatch(pattern, secret_key):
        reject(format_error)
    return secret_key


def read_turnstile_secret(path: pathlib.Path) -> str:
    return read_runtime_secret(
        path,
        "turnstile-secret",
        r"[A-Za-z0-9_-]+",
        "turnstile-secret-key-format",
    )


def read_qa_bypass_token(path: pathlib.Path) -> str:
    return read_runtime_secret(
        path,
        "turnstile-qa-bypass-token",
        r"[A-Za-z0-9_-]{32,256}",
        "turnstile-qa-bypass-token-format",
    )


def validate_values(values: dict[str, str]) -> None:
    expected_names = set(EXPECTED) | {
        "TG_WEBSOCKET_ALLOWED_ORIGINS",
        "POSTGRES_PASSWORD", "TG_AUTHKEY_ENC_KEY",
        "TEAGRAM_DEMO_TURNSTILE_SITE_KEY",
    }
    optional_names = set(OPTIONAL_VALUES)
    if not expected_names.issubset(values) or set(values) - expected_names - optional_names:
        reject("variable-set")
    if not re.fullmatch(r"[0-9a-f]{64}", values["POSTGRES_PASSWORD"]):
        reject("postgres-password-format")
    if not re.fullmatch(r"[0-9a-f]{64}", values["TG_AUTHKEY_ENC_KEY"]):
        reject("authkey-encryption-key-format")
    if values["POSTGRES_PASSWORD"] == values["TG_AUTHKEY_ENC_KEY"]:
        reject("secret-reuse")
    site_key = values["TEAGRAM_DEMO_TURNSTILE_SITE_KEY"]
    if not re.fullmatch(r"[A-Za-z0-9_-]+", site_key):
        reject("turnstile-site-key-format")
    secret_path_value = values["TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE"]
    if secret_path_value != EXPECTED["TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE"]:
        reject("turnstile-secret-file-path")
    secret_path = pathlib.Path(secret_path_value)
    secret_key = read_turnstile_secret(secret_path)
    qa_bypass_token = read_qa_bypass_token(QA_BYPASS_TOKEN_PATH)
    if site_key == secret_key:
        reject("turnstile-key-reuse")
    if qa_bypass_token in {site_key, secret_key}:
        reject("turnstile-qa-bypass-token-reuse")
    if any(values[name] != value for name, value in EXPECTED.items()):
        reject("fixed-demo-settings")
    if values["TG_WEBSOCKET_ALLOWED_ORIGINS"] != "https://web.teagram.io,https://demo.teagram.io":
        reject("fixed-demo-settings")
    if any(values[name] not in allowed for name, allowed in OPTIONAL_VALUES.items() if name in values):
        reject("fixed-demo-settings")
    if os.path.realpath(values["TEAGRAM_DEMO_BLOB_PATH"]) != values["TEAGRAM_DEMO_BLOB_PATH"]:
        reject("blob-path-not-canonical")
    print("demo env=valid")


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

    validate_values(values)


if __name__ == "__main__":
    main()
