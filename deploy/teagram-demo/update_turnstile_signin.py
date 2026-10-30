#!/usr/bin/env python3
"""Safely add or change only the demo signin Turnstile flag."""

from __future__ import annotations

import argparse
import hashlib
import os
import pathlib
import stat
import sys
import tempfile


ENV_NAME = "TG_TURNSTILE_SIGNIN_ENABLED"


def fail(message: str) -> None:
    print(f"demo Turnstile rollout rejected: {message}", file=sys.stderr)
    raise SystemExit(1)


def require_file(path: pathlib.Path, expected_mode: int) -> os.stat_result:
    try:
        metadata = path.lstat()
    except OSError:
        fail("environment-file-unavailable")
    if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISREG(metadata.st_mode):
        fail("environment-file-type")
    if metadata.st_uid != os.geteuid() or stat.S_IMODE(metadata.st_mode) != expected_mode:
        fail("environment-file-owner-or-mode")
    return metadata


def require_directory(path: pathlib.Path, expected_mode: int | None = None) -> os.stat_result:
    try:
        metadata = path.lstat()
    except OSError:
        fail("backup-directory-unavailable")
    if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISDIR(metadata.st_mode):
        fail("backup-directory-type")
    if metadata.st_uid != os.geteuid():
        fail("backup-directory-owner")
    if expected_mode is not None and stat.S_IMODE(metadata.st_mode) != expected_mode:
        fail("backup-directory-mode")
    if stat.S_IMODE(metadata.st_mode) & 0o022:
        fail("backup-directory-writable-by-others")
    return metadata


def fsync_directory(path: pathlib.Path) -> None:
    descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def atomic_write(
    path: pathlib.Path,
    content: bytes,
    metadata: os.stat_result,
    mode: int,
    original_content: bytes,
) -> None:
    def replace_file(contents: bytes) -> None:
        descriptor = -1
        temporary: pathlib.Path | None = None
        try:
            descriptor, name = tempfile.mkstemp(prefix=f".{path.name}.turnstile-", dir=path.parent)
            temporary = pathlib.Path(name)
            os.fchown(descriptor, metadata.st_uid, metadata.st_gid)
            os.fchmod(descriptor, mode)
            with os.fdopen(descriptor, "wb") as output:
                descriptor = -1
                output.write(contents)
                output.flush()
                os.fsync(output.fileno())
            os.replace(temporary, path)
            temporary = None
        finally:
            if descriptor >= 0:
                os.close(descriptor)
            if temporary is not None and temporary.exists():
                temporary.unlink()

    def sync_parent() -> None:
        fsync_directory(path.parent)

    replaced = False
    try:
        replace_file(content)
        replaced = True
        sync_parent()
    except OSError:
        if replaced:
            try:
                replace_file(original_content)
                sync_parent()
            except OSError:
                fail("environment-file-update-and-rollback")
        fail("environment-file-update")


def backup_file(
    directory: pathlib.Path,
    content: bytes,
    metadata: os.stat_result,
    backup_path: pathlib.Path | None = None,
) -> pathlib.Path:
    require_directory(directory.parent)
    if not directory.exists() and not directory.is_symlink():
        try:
            directory.mkdir(mode=0o700)
        except OSError:
            fail("backup-directory-create")
    try:
        fsync_directory(directory.parent)
    except OSError:
        fail("backup-directory-sync")
    require_directory(directory, 0o700)
    if backup_path is not None and backup_path.parent != directory:
        fail("environment-backup-outside-directory")
    descriptor = -1
    temporary: pathlib.Path | None = None
    backup: pathlib.Path | None = None
    published = False
    complete = False
    try:
        descriptor, name = tempfile.mkstemp(prefix=".env.before-turnstile-", dir=directory)
        temporary = pathlib.Path(name)
        if backup_path is not None:
            backup = backup_path
        else:
            backup = directory / temporary.name[1:]
        os.fchown(descriptor, metadata.st_uid, metadata.st_gid)
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "wb") as output:
            descriptor = -1
            output.write(content)
            output.flush()
            os.fsync(output.fileno())
        saved_metadata = require_file(temporary, 0o600)
        if saved_metadata.st_uid != metadata.st_uid or temporary.read_bytes() != content:
            fail("environment-backup-verification")
        os.link(temporary, backup, follow_symlinks=False)
        published = True
        temporary.unlink()
        temporary = None
        fsync_directory(directory)
        saved_metadata = require_file(backup, 0o600)
        if saved_metadata.st_uid != metadata.st_uid or backup.read_bytes() != content:
            fail("environment-backup-verification")
        complete = True
        return backup
    except OSError:
        fail("environment-backup-write")
    finally:
        if descriptor >= 0:
            os.close(descriptor)
        if temporary is not None:
            try:
                temporary.unlink(missing_ok=True)
            except OSError:
                fail("environment-backup-cleanup")
        if published and not complete and backup is not None:
            try:
                backup.unlink(missing_ok=True)
            except OSError:
                fail("environment-backup-cleanup")


def flag_lines(content: str) -> list[str]:
    return [
        line for line in content.splitlines(keepends=True)
        if line.rstrip("\r\n").startswith(f"{ENV_NAME}=")
    ]


def content_without_flag(content: str) -> str:
    return "".join(
        line for line in content.splitlines(keepends=True)
        if not line.rstrip("\r\n").startswith(f"{ENV_NAME}=")
    )


def update_env_file(
    env_file: pathlib.Path,
    backup_dir: pathlib.Path,
    target: str,
    backup_path: pathlib.Path | None = None,
) -> dict[str, str]:
    if target not in {"false", "true"}:
        fail("target-must-be-boolean")
    metadata = require_file(env_file, 0o600)
    try:
        original_bytes = env_file.read_bytes()
        original = original_bytes.decode("utf-8")
    except (OSError, UnicodeDecodeError):
        fail("environment-file-read")
    if not original_bytes.endswith(b"\n"):
        fail("environment-file-must-end-with-newline")

    assignments = flag_lines(original)
    if len(assignments) > 1:
        fail("duplicate-signin-flag")
    if assignments:
        current = assignments[0].rstrip("\r\n").partition("=")[2]
        if current not in {"false", "true"}:
            fail("signin-flag-must-be-boolean")
    else:
        current = "false"
    if current == target:
        return {"changed": "false", "current": current, "backup": "none", "mode": "none"}

    mode = "changed" if assignments else "added"
    if mode == "added":
        if target != "true" or os.environ.get("ROLLOUT_RUNNER_ADDED_ENV") != ENV_NAME:
            fail("new-signin-flag-requires-approved-environment-addition")
        updated = original + f"{ENV_NAME}={target}\n"
    else:
        if os.environ.get("ROLLOUT_RUNNER_CHANGED_ENV") != ENV_NAME:
            fail("existing-signin-flag-requires-approved-environment-change")
        updated = original.replace(assignments[0], f"{ENV_NAME}={target}\n", 1)

    if content_without_flag(original) != content_without_flag(updated):
        fail("environment-change-touched-an-unapproved-setting")

    backup = backup_file(backup_dir, original_bytes, metadata, backup_path)
    if mode == "changed":
        previous_file = os.environ.get("ROLLOUT_RUNNER_PREVIOUS_ENV_FILE")
        if previous_file and previous_file != str(backup):
            fail("previous-environment-file-does-not-match-this-rollout")
        os.environ["ROLLOUT_RUNNER_PREVIOUS_ENV_FILE"] = str(backup)
        previous_metadata = require_file(backup, 0o600)
        if backup.read_bytes() != original_bytes:
            fail("previous-environment-file-verification")

    atomic_write(env_file, updated.encode("utf-8"), metadata, 0o600, original_bytes)
    saved = env_file.read_bytes().decode("utf-8")
    if (
        content_without_flag(saved) != content_without_flag(original)
        or len(flag_lines(saved)) != 1
        or flag_lines(saved)[0].rstrip("\r\n") != f"{ENV_NAME}={target}"
    ):
        atomic_write(env_file, original_bytes, metadata, 0o600, original_bytes)
        fail("environment-change-verification")

    return {
        "changed": "true",
        "current": target,
        "previous": current,
        "backup": str(backup),
        "mode": mode,
        "env_sha256": hashlib.sha256(env_file.read_bytes()).hexdigest(),
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--env-file", required=True, type=pathlib.Path)
    parser.add_argument("--backup-dir", required=True, type=pathlib.Path)
    parser.add_argument("--backup-path", type=pathlib.Path)
    parser.add_argument("--target", required=True, choices=("false", "true"))
    args = parser.parse_args()
    result = update_env_file(args.env_file, args.backup_dir, args.target, args.backup_path)
    fields = [f"changed={result['changed']}", f"effective_flag={result['current']}"]
    if result["changed"] == "true":
        fields.extend([
            f"mode={result['mode']}",
            f"previous_flag={result['previous']}",
            f"backup={result['backup']}",
            f"env_sha256={result['env_sha256']}",
        ])
    else:
        fields.append("backup=none")
    print(" ".join(fields))


if __name__ == "__main__":
    main()
