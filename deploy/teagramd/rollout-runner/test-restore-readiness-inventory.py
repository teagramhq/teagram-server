#!/usr/bin/env python3
"""Snapshot the Docker metadata used by the restore-readiness isolation gate."""

from __future__ import annotations

import argparse
import importlib.util
import json
import os
import stat
import subprocess
import sys
from pathlib import Path
from typing import Any


SCRIPT = Path(__file__).with_name("restore-readiness.py")
DOCKER = "/usr/bin/docker"
SPEC = importlib.util.spec_from_file_location("restore_readiness_inventory", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError("restore-readiness module unavailable")
RESTORE_READINESS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RESTORE_READINESS)
DOCKER = RESTORE_READINESS.ensure_trusted_system_executable(DOCKER)
MAX_SNAPSHOT = RESTORE_READINESS.MAX_FOREIGN_INVENTORY


def docker_call(arguments: list[str]) -> bytes:
    try:
        result = subprocess.run(
            [DOCKER, *arguments],
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            env={
                "HOME": "/root" if os.geteuid() == 0 else os.environ.get("HOME", ""),
                "LANG": "C",
                "LC_ALL": "C",
                "PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
            },
            timeout=20,
            check=False,
        )
    except (OSError, subprocess.SubprocessError) as exc:
        raise RuntimeError("Docker inventory command failed") from exc
    if result.returncode != 0 or len(result.stdout) > MAX_SNAPSHOT:
        raise RuntimeError("Docker inventory command failed")
    return result.stdout


def read_snapshot(path: Path) -> dict[str, Any]:
    try:
        info = path.lstat()
        if (
            not stat.S_ISREG(info.st_mode)
            or info.st_nlink != 1
            or info.st_mode & 0o077
            or info.st_size > MAX_SNAPSHOT
        ):
            raise ValueError("invalid inventory snapshot")
        descriptor = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
        try:
            raw = os.read(descriptor, MAX_SNAPSHOT + 1)
        finally:
            os.close(descriptor)
        if len(raw) > MAX_SNAPSHOT:
            raise ValueError("inventory snapshot too large")
        parsed = json.loads(raw)
        if not isinstance(parsed, dict) or set(parsed) != {"containers", "volumes", "networks", "images"}:
            raise ValueError("invalid inventory snapshot")
        return parsed
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError("invalid inventory snapshot") from exc


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=("snapshot", "compare", "source-image"))
    parser.add_argument("path", nargs="?", type=Path)
    arguments = parser.parse_args()
    if arguments.action == "source-image":
        if arguments.path is not None:
            parser.error("source-image does not accept a path")
        print(RESTORE_READINESS.SOURCE_IMAGE)
        return 0
    if arguments.action == "compare" and arguments.path is None:
        parser.error("compare requires a snapshot path")
    if arguments.action == "snapshot" and arguments.path is not None:
        parser.error("snapshot writes to stdout")
    try:
        current = RESTORE_READINESS.collect_docker_resource_inventory(docker_call)
        if arguments.action == "snapshot":
            print(json.dumps(current, sort_keys=True, separators=(",", ":")))
            return 0
        if current != read_snapshot(arguments.path):
            print("restore readiness foreign Docker inventory changed", file=sys.stderr)
            return 1
    except (RuntimeError, ValueError, TypeError, KeyError, RESTORE_READINESS.DrillFailure):
        print("restore readiness Docker inventory verification failed", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
