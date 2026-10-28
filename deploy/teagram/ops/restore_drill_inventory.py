#!/usr/bin/env python3
"""Collect metadata for the protected pre-deploy dump inventory."""

from __future__ import annotations

import datetime as dt
import json
import os
import re
import stat
import sys
import time
from pathlib import Path
from typing import Any

MAX_DUMP_BYTES = 512 * 1024 * 1024
NAME_PATTERN = re.compile(
    r"^pgdump-pre-(?P<sha>[0-9a-f]{7,40})(?:-(?P<timestamp>[0-9]{8}T[0-9]{6}Z))?\.sql$"
)


def size_class(size_bytes: int) -> str:
    mebibyte = 1024 * 1024
    lower = 0
    for upper in (1, 4, 16, 64, 256, 512):
        if size_bytes <= upper * mebibyte:
            return f"{lower}-{upper}MiB"
        lower = upper
    return "over-512MiB"


def safe_dump_directory(directory: Path, expected_uid: int) -> bool:
    current = directory
    while True:
        try:
            info = current.lstat()
        except OSError:
            return False
        if (
            not stat.S_ISDIR(info.st_mode)
            or stat.S_ISLNK(info.st_mode)
            or info.st_uid != expected_uid
            or info.st_mode & 0o022
        ):
            return False
        if current == current.parent:
            return True
        current = current.parent


def metadata_inventory(
    directory: Path, *, expected_uid: int = 0, now_epoch: int | None = None
) -> dict[str, Any]:
    """Inspect names and stat metadata only; never open a dump."""
    now = int(time.time()) if now_epoch is None else now_epoch
    parent_safe = safe_dump_directory(directory, expected_uid)
    rows: list[dict[str, Any]] = []

    try:
        entries = sorted(
            (entry for entry in os.scandir(directory) if entry.name.startswith("pgdump-pre-") and entry.name.endswith(".sql")),
            key=lambda entry: entry.name,
        )
    except OSError:
        raise ValueError("dump inventory unavailable") from None

    for entry in entries:
        try:
            info = entry.stat(follow_symlinks=False)
        except OSError:
            raise ValueError("dump metadata unavailable") from None
        match = NAME_PATTERN.fullmatch(entry.name)
        filename_timestamp = match.group("timestamp") if match else None
        timestamp_epoch: int | None = None
        if filename_timestamp is not None:
            try:
                timestamp_epoch = int(
                    dt.datetime.strptime(filename_timestamp, "%Y%m%dT%H%M%SZ")
                    .replace(tzinfo=dt.timezone.utc)
                    .timestamp()
                )
            except ValueError:
                filename_timestamp = None

        mtime_epoch = int(info.st_mtime)
        sort_epoch = timestamp_epoch if timestamp_epoch is not None else mtime_epoch
        mode = stat.S_IMODE(info.st_mode)
        rows.append(
            {
                "name": entry.name,
                "size_bytes": info.st_size,
                "mtime_epoch": mtime_epoch,
                "mtime_utc": dt.datetime.fromtimestamp(
                    mtime_epoch, tz=dt.timezone.utc
                ).strftime("%Y-%m-%dT%H:%M:%SZ"),
                "filename_timestamp": filename_timestamp,
                "sort_epoch": sort_epoch,
                "regular": stat.S_ISREG(info.st_mode),
                "symlink": stat.S_ISLNK(info.st_mode),
                "owner_uid": info.st_uid,
                "mode": f"{mode:04o}",
                "links": info.st_nlink,
            }
        )

    rows.sort(
        key=lambda row: (row["sort_epoch"], row["mtime_epoch"], row["name"]),
        reverse=True,
    )
    for index, row in enumerate(rows):
        row["keep_if_newest_10"] = index < 10

    deploy_dumps = [row for row in rows if row["filename_timestamp"] is not None]
    selected = deploy_dumps[0] if deploy_dumps else None
    reasons: list[str] = []
    if selected is None:
        reasons.append("no_deploy_dump")
    else:
        if not parent_safe:
            reasons.append("parent_directory")
        if not selected["regular"] or selected["symlink"]:
            reasons.append("regular_file")
        if selected["owner_uid"] != expected_uid:
            reasons.append("owner")
        if selected["mode"] != "0600":
            reasons.append("mode")
        if selected["links"] != 1:
            reasons.append("links")
        if selected["size_bytes"] <= 0:
            reasons.append("empty")
        if selected["size_bytes"] > MAX_DUMP_BYTES:
            reasons.append("size_over_512_mib")
        if selected["mtime_epoch"] > now:
            reasons.append("future_mtime")

    selected_result = None
    if selected is not None:
        selected_result = {
            "name": selected["name"],
            "size_bytes": selected["size_bytes"],
            "size_class": size_class(selected["size_bytes"]),
            "mtime_utc": selected["mtime_utc"],
            "age_seconds": max(0, now - selected["mtime_epoch"]),
            "metadata_ok": not reasons,
            "refusal_reasons": reasons,
        }

    return {
        "selected": selected_result,
        "inventory": [
            {
                "name": row["name"],
                "size_bytes": row["size_bytes"],
                "mtime_utc": row["mtime_utc"],
                "prune_if_keep_newest_10": not row["keep_if_newest_10"],
            }
            for row in rows
        ],
    }


def main() -> int:
    if len(sys.argv) != 2 or sys.argv[1] != "/root":
        print("restore-drill-inventory: invalid inventory root", file=sys.stderr)
        return 64
    try:
        result = metadata_inventory(Path(sys.argv[1]))
    except ValueError as error:
        print(f"restore-drill-inventory: {error}", file=sys.stderr)
        return 1
    print(json.dumps(result, ensure_ascii=True, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
