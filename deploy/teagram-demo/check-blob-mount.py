#!/usr/bin/env python3
"""Require the demo blob path to live on the dedicated 4-GiB mount."""

from __future__ import annotations

import os
import pathlib
import stat
import sys


GIB = 1024**3
MIN_BYTES = 3 * GIB
MAX_BYTES = 4 * GIB + 64 * 1024**2


def reject(reason: str) -> None:
    print(f"demo blob mount rejected: {reason}", file=sys.stderr)
    raise SystemExit(1)


def main() -> None:
    if len(sys.argv) != 2:
        reject("usage")
    path = pathlib.Path(sys.argv[1])
    try:
        info = path.lstat()
        real = pathlib.Path(os.path.realpath(path))
    except OSError:
        reject("path-unavailable")
    if not path.is_absolute() or stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        reject("path-type")
    if real != path:
        reject("path-not-canonical")

    device = info.st_dev
    mountpoint = path
    while mountpoint != mountpoint.parent:
        parent = mountpoint.parent
        try:
            parent_device = parent.stat().st_dev
        except OSError:
            reject("mount-ancestor-unavailable")
        if parent_device != device:
            break
        mountpoint = parent
    if mountpoint == pathlib.Path("/") or not os.path.ismount(mountpoint):
        reject("not-on-dedicated-mount")

    try:
        filesystem = os.statvfs(path)
    except OSError:
        reject("filesystem-unavailable")
    total_bytes = filesystem.f_blocks * filesystem.f_frsize
    if not MIN_BYTES <= total_bytes <= MAX_BYTES:
        reject("filesystem-capacity")
    print(f"demo blob mount=valid total_bytes={total_bytes}")


if __name__ == "__main__":
    main()
