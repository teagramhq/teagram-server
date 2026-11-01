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
UPLOADS_DISABLED_VARIABLE = "TG_UPLOADS_DISABLED"


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
    changed = False
    if current == NEW_ORIGINS:
        pass
    elif current == OLD_ORIGINS:
        lines[index] = f"{VARIABLE}={NEW_ORIGINS}{newline}"
        changed = True
    else:
        reject("unexpected-origin-list")

    uploads_matches = [
        index
        for index, line in enumerate(lines)
        if line.rstrip("\r\n").startswith(f"{UPLOADS_DISABLED_VARIABLE}=")
    ]
    if len(uploads_matches) > 1:
        reject("uploads-disabled-assignment-count")
    if uploads_matches:
        uploads_value = lines[uploads_matches[0]].rstrip("\r\n").partition("=")[2]
        if uploads_value != "true":
            reject("uploads-must-remain-disabled")
    else:
        if lines and not lines[-1].endswith(("\n", "\r")):
            lines[-1] = lines[-1] + "\n"
        lines.append(f"{UPLOADS_DISABLED_VARIABLE}=true\n")
        changed = True

    if not changed:
        return

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
    print("demo env updated: TG_WEBSOCKET_ALLOWED_ORIGINS, TG_UPLOADS_DISABLED")


if __name__ == "__main__":
    main()
