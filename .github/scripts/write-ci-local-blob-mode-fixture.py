#!/usr/bin/env python3
"""Create an isolated local authority fixture for the Compose boot smoke."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import pathlib
import stat
import sys
import uuid


SCHEMA = "teagram.blob-mode/v1"
REPORT_SCHEMA = "teagram.blob-mode-report/v1"
BLOB_DIR = "/var/lib/telegramd-blobs"


def fail(message: str) -> None:
    raise SystemExit(f"CI blob-mode fixture rejected: {message}")


def sync_directory(path: pathlib.Path) -> None:
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def write_file(path: pathlib.Path, content: bytes, mode: int) -> None:
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC, mode)
    with os.fdopen(fd, "wb") as output:
        os.fchown(output.fileno(), 0, 0)
        os.fchmod(output.fileno(), mode)
        output.write(content)
        output.flush()
        os.fsync(output.fileno())


def main() -> int:
    if os.geteuid() != 0 or os.environ.get("CI") != "true":
        fail("this synthetic fixture is restricted to the root-owned CI job")
    parser = argparse.ArgumentParser()
    parser.add_argument("--checkout", type=pathlib.Path, required=True)
    parser.add_argument("--compose-config", type=pathlib.Path, required=True)
    args = parser.parse_args()
    try:
        compose = json.loads(args.compose_config.read_text(encoding="utf-8"))
        volume = compose["volumes"]["tgblobs"]["name"]
    except (OSError, KeyError, TypeError, json.JSONDecodeError):
        fail("resolved local Compose volume is unavailable")
    if not isinstance(volume, str) or not volume:
        fail("resolved local Compose volume name is invalid")

    state = args.checkout / ".state"
    mode_dir = state / "blob-mode"
    journal = mode_dir / "journal"
    for path in (state, mode_dir):
        if path.exists() or path.is_symlink():
            info = path.lstat()
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
                fail("existing CI state directory is not root-owned and private")
    if mode_dir.exists() and list(mode_dir.iterdir()):
        fail("refusing to replace existing blob-mode state")
    if not state.exists():
        state.mkdir(mode=0o755)
        os.chown(state, 0, 0)
        os.chmod(state, 0o755)
        sync_directory(args.checkout)
    if not mode_dir.exists():
        mode_dir.mkdir(mode=0o755)
        os.chown(mode_dir, 0, 0)
        os.chmod(mode_dir, 0o755)
        sync_directory(state)
    journal.mkdir(mode=0o755)
    os.chown(journal, 0, 0)
    os.chmod(journal, 0o755)
    sync_directory(mode_dir)

    transition = str(uuid.uuid4())
    backend = {"kind": "local", "dir": BLOB_DIR}
    report = {
        "schema": REPORT_SCHEMA,
        "generation": 1,
        "transition_id": transition,
        "outcome": "initial-local",
        "backend": backend,
        "fixture": "CI-only local Compose startup",
    }
    report_bytes = (json.dumps(report, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
    report_path = pathlib.Path("/root") / f"ci-telegramd-blob-mode-report-{transition}.json"
    write_file(report_path, report_bytes, 0o600)
    sync_directory(report_path.parent)

    record = {
        "schema": SCHEMA,
        "generation": 1,
        "transition_id": transition,
        "supersedes": None,
        "outcome": "initial-local",
        "backend": backend,
        "volumes": {"tgblobs": volume, "rustfsdata": None},
        "evidence": {"report_sha256": hashlib.sha256(report_bytes).hexdigest()},
        "published_at": dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
    }
    record_bytes = (json.dumps(record, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
    temp = journal / f".tmp-{transition}"
    write_file(temp, record_bytes, 0o644)
    os.link(temp, journal / "0000000001.json")
    sync_directory(journal)
    temp.unlink()
    mode_temp = mode_dir / f".mode.json.tmp-{transition}"
    write_file(mode_temp, record_bytes, 0o644)
    os.replace(mode_temp, mode_dir / "mode.json")
    sync_directory(mode_dir)
    return 0


if __name__ == "__main__":
    sys.exit(main())
