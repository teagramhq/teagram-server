#!/usr/bin/env python3
"""Rotate the demo-only QA bypass token without displaying its value."""

from __future__ import annotations

import datetime
import os
import pathlib
import re
import secrets
import stat
import sys
import tempfile


TOKEN_PATTERN = re.compile(r"[A-Za-z0-9_-]{32,256}\Z")
CONFIG_SECRET_PATTERN = re.compile(r"[A-Za-z0-9_-]+\Z")
GENERATED_PATTERN = re.compile(r"[0-9a-f]{64}\Z")
SECRET_DIR = pathlib.Path("/var/lib/teagram-demo/secrets")
TOKEN_FILE = SECRET_DIR / "turnstile-qa-bypass-token"
SITEVERIFY_FILE = SECRET_DIR / "turnstile-secret-key"
ENV_FILE = pathlib.Path("/opt/teagram-demo/deploy/teagram-demo/.env")
BACKUP_DIR = pathlib.Path("/var/backups/teagram-demo-turnstile-qa-bypass")


class RotationError(Exception):
    """A safe, non-secret error suitable for command output."""


def _read_checked_file(
    path: pathlib.Path, name: str, owner: int, group: int, mode: int
) -> tuple[bytes, os.stat_result]:
    descriptor = -1
    try:
        descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        metadata = os.fstat(descriptor)
        if (
            not stat.S_ISREG(metadata.st_mode)
            or metadata.st_uid != owner
            or metadata.st_gid != group
            or stat.S_IMODE(metadata.st_mode) != mode
        ):
            raise RotationError(f"{name}-owner-group-or-mode")
        with os.fdopen(descriptor, "rb") as stream:
            descriptor = -1
            return stream.read(), metadata
    except RotationError:
        raise
    except OSError as exc:
        raise RotationError(f"{name}-unavailable: {exc.strerror}") from exc
    finally:
        if descriptor >= 0:
            os.close(descriptor)


def _require_directory(path: pathlib.Path, owner: int, group: int, mode: int, name: str) -> None:
    try:
        metadata = path.lstat()
    except OSError as exc:
        raise RotationError(f"{name}-unavailable") from exc
    if (
        stat.S_ISLNK(metadata.st_mode)
        or not stat.S_ISDIR(metadata.st_mode)
        or metadata.st_uid != owner
        or metadata.st_gid != group
        or stat.S_IMODE(metadata.st_mode) != mode
    ):
        raise RotationError(f"{name}-owner-or-mode")


def _runtime_secret(
    path: pathlib.Path, name: str, pattern: re.Pattern[str]
) -> tuple[bytes, str, os.stat_result]:
    raw, metadata = _read_checked_file(path, name, 0, 65532, 0o440)
    try:
        value = raw.decode("ascii").strip()
    except UnicodeDecodeError as exc:
        raise RotationError(f"{name}-format") from exc
    if not pattern.fullmatch(value):
        raise RotationError(f"{name}-format")
    return raw, value, metadata


def _site_key(path: pathlib.Path) -> str:
    raw, _ = _read_checked_file(path, "demo-env", 0, 0, 0o600)
    try:
        lines = raw.decode("utf-8").splitlines()
    except UnicodeDecodeError as exc:
        raise RotationError("demo-env-format") from exc
    values = [
        line.partition("=")[2]
        for line in lines
        if line.partition("=")[0] == "TEAGRAM_DEMO_TURNSTILE_SITE_KEY"
        and "=" in line
    ]
    if len(values) != 1 or not CONFIG_SECRET_PATTERN.fullmatch(values[0]):
        raise RotationError("demo-site-key-format")
    return values[0]


def _write_all(descriptor: int, content: bytes) -> None:
    offset = 0
    while offset < len(content):
        offset += os.write(descriptor, content[offset:])


def _snapshot(path: pathlib.Path, content: bytes) -> None:
    try:
        descriptor = os.open(
            path,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0),
            0o600,
        )
    except OSError as exc:
        raise RotationError("snapshot-create-failed") from exc
    try:
        os.fchown(descriptor, 0, 0)
        os.fchmod(descriptor, 0o600)
        _write_all(descriptor, content)
        os.fsync(descriptor)
    except OSError as exc:
        os.close(descriptor)
        path.unlink(missing_ok=True)
        raise RotationError("snapshot-write-failed") from exc
    else:
        os.close(descriptor)

    try:
        copied, metadata = _read_checked_file(path, "snapshot", 0, 0, 0o600)
        if copied != content or stat.S_IMODE(metadata.st_mode) != 0o600:
            raise RotationError("snapshot-verify-failed")
    except RotationError:
        path.unlink(missing_ok=True)
        raise


def _install_token(
    path: pathlib.Path,
    secret_dir: pathlib.Path,
    token: str,
    rotation_marker: pathlib.Path | None,
) -> None:
    descriptor = -1
    temporary_path: pathlib.Path | None = None
    try:
        descriptor, temporary_name = tempfile.mkstemp(
            prefix=".turnstile-qa-bypass-token.", dir=secret_dir
        )
        temporary_path = pathlib.Path(temporary_name)
        os.fchown(descriptor, 0, 65532)
        os.fchmod(descriptor, 0o440)
        _write_all(descriptor, f"{token}\n".encode("ascii"))
        os.fsync(descriptor)
        os.close(descriptor)
        descriptor = -1
        if rotation_marker is not None:
            marker_fd = os.open(
                rotation_marker,
                os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0),
                0o600,
            )
            try:
                os.fchown(marker_fd, 0, 0)
                os.fchmod(marker_fd, 0o600)
                os.fsync(marker_fd)
            finally:
                os.close(marker_fd)
        os.replace(temporary_path, path)
        directory_fd = os.open(secret_dir, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except OSError as exc:
        raise RotationError("token-install-failed") from exc
    finally:
        if descriptor >= 0:
            os.close(descriptor)
        if temporary_path is not None:
            temporary_path.unlink(missing_ok=True)


def rotate_token(
    secret_dir: pathlib.Path,
    token_file: pathlib.Path,
    siteverify_file: pathlib.Path,
    env_file: pathlib.Path,
    backup_dir: pathlib.Path,
    rotation_marker: pathlib.Path | None = None,
) -> tuple[pathlib.Path, int]:
    _require_directory(secret_dir, 0, 0, 0o700, "demo-secret-directory")
    old_raw, old_token, original_metadata = _runtime_secret(
        token_file, "qa-bypass-token", TOKEN_PATTERN
    )
    _, siteverify_secret, _ = _runtime_secret(
        siteverify_file, "siteverify-secret", CONFIG_SECRET_PATTERN
    )
    site_key = _site_key(env_file)
    if old_token in {site_key, siteverify_secret}:
        raise RotationError("existing-token-reuse")

    try:
        backup_parent_metadata = backup_dir.parent.lstat()
    except OSError as exc:
        raise RotationError("snapshot-parent-unavailable") from exc
    if (
        stat.S_ISLNK(backup_parent_metadata.st_mode)
        or not stat.S_ISDIR(backup_parent_metadata.st_mode)
        or backup_parent_metadata.st_uid != 0
        or backup_parent_metadata.st_gid != 0
        or stat.S_IMODE(backup_parent_metadata.st_mode) & 0o022
    ):
        raise RotationError("snapshot-parent-owner-or-mode")
    try:
        backup_dir.mkdir(mode=0o700)
    except FileExistsError:
        pass
    except OSError as exc:
        raise RotationError("snapshot-directory-create-failed") from exc
    _require_directory(backup_dir, 0, 0, 0o700, "snapshot-directory")

    timestamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    snapshot_path: pathlib.Path | None = None
    for _ in range(5):
        candidate = backup_dir / f"turnstile-qa-bypass-token.{timestamp}.{secrets.token_hex(4)}"
        try:
            _snapshot(candidate, old_raw)
        except RotationError as exc:
            if str(exc) == "snapshot-create-failed" and candidate.exists():
                continue
            raise
        snapshot_path = candidate
        break
    if snapshot_path is None:
        raise RotationError("snapshot-create-failed")

    try:
        current_raw, current_metadata = _read_checked_file(
            token_file, "qa-bypass-token", 0, 65532, 0o440
        )
    except RotationError as exc:
        raise RotationError("qa-bypass-token-changed") from exc
    if (
        current_metadata.st_dev != original_metadata.st_dev
        or current_metadata.st_ino != original_metadata.st_ino
        or current_raw != old_raw
    ):
        raise RotationError("qa-bypass-token-changed")

    token = secrets.token_hex(32)
    if (
        not GENERATED_PATTERN.fullmatch(token)
        or token == old_token
        or token == site_key
        or token == siteverify_secret
    ):
        raise RotationError("generated-token-invalid")
    _install_token(token_file, secret_dir, token, rotation_marker)
    installed_raw, installed_token, _ = _runtime_secret(
        token_file, "qa-bypass-token", TOKEN_PATTERN
    )
    if installed_token != token or installed_raw != f"{token}\n".encode("ascii"):
        raise RotationError("installed-token-verify-failed")
    return snapshot_path, len(token)


def main() -> int:
    if os.geteuid() != 0:
        print("demo QA bypass rotation: root is required", file=sys.stderr)
        return 1
    if len(sys.argv) != 2:
        print("demo QA bypass rotation: rotation marker path is required", file=sys.stderr)
        return 1
    try:
        snapshot_path, token_length = rotate_token(
            SECRET_DIR,
            TOKEN_FILE,
            SITEVERIFY_FILE,
            ENV_FILE,
            BACKUP_DIR,
            pathlib.Path(sys.argv[1]),
        )
    except RotationError as exc:
        print(f"demo QA bypass rotation: {exc}", file=sys.stderr)
        return 1
    print(f"rotation=pass token_length={token_length} snapshot={snapshot_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
