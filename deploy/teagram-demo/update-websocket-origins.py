#!/usr/bin/env python3
"""Add the demo origin to the prepared demo environment without touching secrets."""

from __future__ import annotations

import os
import pathlib
import stat
import sys
import tempfile


OLD_ORIGINS = "https://web.teagram.io"
NEW_ORIGINS = "https://web.teagram.io,https://demo.teagram.io"
VARIABLE = "TG_WEBSOCKET_ALLOWED_ORIGINS"


def reject(message: str) -> None:
    print(f"demo env update rejected: {message}", file=sys.stderr)
    raise SystemExit(1)


def update(path: pathlib.Path) -> None:
    try:
        metadata = path.lstat()
    except OSError:
        reject("file-unavailable")
    if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISREG(metadata.st_mode):
        reject("file-type")
    if metadata.st_uid != os.geteuid() or stat.S_IMODE(metadata.st_mode) != 0o600:
        reject("file-owner-or-mode")

    try:
        lines = path.read_text(encoding="utf-8").splitlines(keepends=True)
    except (OSError, UnicodeDecodeError):
        reject("file-read")

    matches = [index for index, line in enumerate(lines) if line.rstrip("\r\n").startswith(f"{VARIABLE}=")]
    if len(matches) != 1:
        reject("assignment-count")
    index = matches[0]
    line = lines[index]
    current = line.rstrip("\r\n").partition("=")[2]
    newline = line[len(line.rstrip("\r\n")) :]
    if current == NEW_ORIGINS:
        return
    if current != OLD_ORIGINS:
        reject("unexpected-origin-list")
    lines[index] = f"{VARIABLE}={NEW_ORIGINS}{newline}"

    temporary = None
    try:
        fd, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
        temporary = pathlib.Path(temporary_name)
        os.fchmod(fd, 0o600)
        os.fchown(fd, metadata.st_uid, metadata.st_gid)
        with os.fdopen(fd, "w", encoding="utf-8", newline="") as output:
            output.writelines(lines)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
        directory_fd = os.open(path.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except OSError:
        if temporary is not None:
            try:
                temporary.unlink()
            except OSError:
                pass
        reject("atomic-write")


def main() -> None:
    if len(sys.argv) != 2:
        reject("usage")
    update(pathlib.Path(sys.argv[1]))
    print("demo env updated: TG_WEBSOCKET_ALLOWED_ORIGINS")


if __name__ == "__main__":
    main()
