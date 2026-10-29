#!/usr/bin/env python3
"""Receive a bounded, checksum-pinned web release over the forced SSH stream."""

from __future__ import annotations

import hashlib
import os
import pathlib
import re
import stat
import sys


MAX_ARCHIVE_BYTES = 128 * 1024 * 1024
CHUNK_SIZE = 1024 * 1024


def reject(message: str) -> None:
    print(f"demo web upload rejected: {message}", file=sys.stderr)
    raise SystemExit(1)


def receive(path: pathlib.Path, expected_sha256: str) -> None:
    if not re.fullmatch(r"[0-9a-f]{64}", expected_sha256):
        reject("archive-checksum-format")
    try:
        parent_metadata = path.parent.lstat()
    except OSError:
        reject("archive-directory-unavailable")
    if (
        stat.S_ISLNK(parent_metadata.st_mode)
        or not stat.S_ISDIR(parent_metadata.st_mode)
        or parent_metadata.st_uid != os.geteuid()
        or stat.S_IMODE(parent_metadata.st_mode) != 0o700
    ):
        reject("archive-directory-owner-or-mode")

    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags, 0o600)
    except OSError:
        reject("archive-file-create")

    digest = hashlib.sha256()
    size = 0
    try:
        with os.fdopen(descriptor, "wb") as archive:
            while True:
                chunk = sys.stdin.buffer.read(CHUNK_SIZE)
                if not chunk:
                    break
                size += len(chunk)
                if size > MAX_ARCHIVE_BYTES:
                    reject("archive-too-large")
                digest.update(chunk)
                archive.write(chunk)
            archive.flush()
            os.fsync(archive.fileno())
        if size == 0:
            reject("archive-empty")
        if digest.hexdigest() != expected_sha256:
            reject("archive-checksum-mismatch")
        directory_fd = os.open(path.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except BaseException:
        try:
            path.unlink()
        except OSError:
            pass
        raise
    print("demo web archive received")


def main() -> None:
    if len(sys.argv) != 3:
        reject("usage")
    receive(pathlib.Path(sys.argv[1]), sys.argv[2])


if __name__ == "__main__":
    main()
