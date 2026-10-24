#!/usr/bin/env python3
"""Track teagramd Docker log byte deltas without reading log contents."""

from __future__ import annotations

import argparse
import datetime as dt
import fcntl
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
from typing import Any


CONTAINER_NAME = "teagram-teagramd-1"
DEFAULT_STATE_DIR = Path("/var/lib/teagramd-log-volume")
MAX_RETAINED_DAYS = 30
MAX_SAMPLE_AGE = dt.timedelta(minutes=20)
MIDNIGHT_GRACE = dt.timedelta(seconds=30)
HOST_FREE_SPACE_RESERVE_BYTES = 64 * 1024**3
SCHEMA_VERSION = 1


class SourceUnavailable(Exception):
    """The Docker log metadata could not be sampled."""


def format_time(value: dt.datetime) -> str:
    return value.astimezone(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def parse_time(value: str) -> dt.datetime:
    parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("timestamp has no UTC offset")
    return parsed.astimezone(dt.timezone.utc)


def day_start(day: dt.date) -> dt.datetime:
    return dt.datetime.combine(day, dt.time.min, tzinfo=dt.timezone.utc)


def new_day(day: dt.date, *, start: str | None = None, end: str | None = None,
            gap: bool = False, byte_count: int | None = 0) -> dict[str, Any]:
    return {
        "utc_date": day.isoformat(),
        "bytes": byte_count,
        "coverage_start": start,
        "coverage_end": end,
        "gap": gap,
    }


def new_state() -> dict[str, Any]:
    return {
        "schema_version": SCHEMA_VERSION,
        "collection_start": None,
        "days": [],
        "current_day": None,
        "last_sample_at": None,
        "last_container_created_at": None,
        "last_size_bytes": None,
        "replacement_pending_at": None,
    }


def validate_state(state: Any) -> dict[str, Any]:
    if not isinstance(state, dict) or state.get("schema_version") != SCHEMA_VERSION:
        raise ValueError("accounting state has an unsupported format")
    if not isinstance(state.get("days"), list) or len(state["days"]) > MAX_RETAINED_DAYS:
        raise ValueError("accounting state has an invalid daily history")
    current = state.get("current_day")
    if current is not None and not isinstance(current, dict):
        raise ValueError("accounting state has an invalid current day")
    last_size = state.get("last_size_bytes")
    if last_size is not None and (not isinstance(last_size, int) or last_size < 0):
        raise ValueError("accounting state has an invalid byte counter")
    for timestamp in (
        state.get("collection_start"),
        state.get("last_sample_at"),
        state.get("replacement_pending_at"),
    ):
        if timestamp is not None:
            parse_time(timestamp)
    created = state.get("last_container_created_at")
    if created is not None:
        parse_time(created)
    return state


def read_state(state_dir: Path) -> dict[str, Any]:
    path = state_dir / "state.json"
    try:
        flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
        descriptor = os.open(path, flags)
    except FileNotFoundError:
        return new_state()
    with os.fdopen(descriptor, "r", encoding="utf-8") as state_file:
        return validate_state(json.load(state_file))


def write_state(state_dir: Path, state: dict[str, Any]) -> None:
    state_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    if state_dir.is_symlink() or not state_dir.is_dir():
        raise ValueError("accounting state directory is unsafe")
    os.chmod(state_dir, 0o700)
    temporary = state_dir / ".state.json.tmp"
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0), 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as state_file:
        json.dump(state, state_file, separators=(",", ":"), sort_keys=True)
        state_file.write("\n")
        state_file.flush()
        os.fsync(state_file.fileno())
    os.replace(temporary, state_dir / "state.json")
    os.chmod(state_dir / "state.json", 0o600)
    directory_fd = os.open(state_dir, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)


def read_snapshot() -> tuple[str, int]:
    try:
        result = subprocess.run(
            [
                "docker", "inspect", "--format",
                "{{.Created}}|{{.LogPath}}|{{.HostConfig.LogConfig.Type}}|{{json .HostConfig.LogConfig.Config}}",
                CONTAINER_NAME,
            ],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
        )
    except OSError:
        raise SourceUnavailable("log_source_unavailable") from None
    if result.returncode != 0:
        raise SourceUnavailable("log_source_unavailable")
    fields = result.stdout.strip().split("|", 3)
    if len(fields) != 4 or not fields[0] or not fields[1]:
        raise SourceUnavailable("log_source_unavailable")
    if fields[2] != "json-file" or fields[3] not in ("{}", "null"):
        raise SourceUnavailable("log_source_unavailable")
    try:
        parse_time(fields[0])
        metadata = os.stat(fields[1], follow_symlinks=False)
    except (OSError, ValueError):
        raise SourceUnavailable("log_source_unavailable") from None
    if not stat.S_ISREG(metadata.st_mode) or metadata.st_size < 0:
        raise SourceUnavailable("log_source_unavailable")
    return fields[0], metadata.st_size


def append_day(state: dict[str, Any], row: dict[str, Any]) -> None:
    state["days"].append(row)
    state["days"] = state["days"][-MAX_RETAINED_DAYS:]


def roll_to_day(state: dict[str, Any], target_day: dt.date, *, gap: bool) -> None:
    current = state.get("current_day")
    if current is None:
        state["current_day"] = new_day(target_day, gap=gap, byte_count=None if gap else 0)
        return
    current_day = dt.date.fromisoformat(current["utc_date"])
    if target_day <= current_day:
        if gap:
            current["gap"] = True
            if current["bytes"] == 0:
                current["bytes"] = None
        return

    if gap:
        current["gap"] = True
        if current["bytes"] == 0:
            current["bytes"] = None
    append_day(state, current)
    next_day = current_day + dt.timedelta(days=1)
    while next_day < target_day:
        append_day(state, new_day(next_day, gap=True, byte_count=None))
        next_day += dt.timedelta(days=1)
    state["current_day"] = new_day(target_day, gap=gap, byte_count=None if gap else 0)


def mark_source_gap(state: dict[str, Any], at: dt.datetime, reason: str) -> str:
    roll_to_day(state, at.date(), gap=True)
    current = state["current_day"]
    current["gap"] = True
    if current["bytes"] == 0:
        current["bytes"] = None
    return reason


def record_reading(state: dict[str, Any], created_at: str, size_bytes: int,
                   at: dt.datetime, event: str) -> str | None:
    timestamp = format_time(at)
    last_sample = state.get("last_sample_at")
    current = state.get("current_day")
    if current is None:
        current = new_day(at.date(), start=timestamp, end=timestamp, byte_count=0)
        state["current_day"] = current
    if last_sample is None:
        if dt.date.fromisoformat(current["utc_date"]) < at.date():
            roll_to_day(state, at.date(), gap=True)
            current = state["current_day"]
        if current["bytes"] is None:
            if state.get("replacement_pending_at"):
                current["bytes"] = size_bytes
            elif not current["gap"]:
                current["bytes"] = 0
        if current["coverage_start"] is None:
            current["coverage_start"] = timestamp
        current["coverage_end"] = timestamp
        state["collection_start"] = state["collection_start"] or timestamp
        state["last_sample_at"] = timestamp
        state["last_container_created_at"] = created_at
        state["last_size_bytes"] = size_bytes
        if event == "before-replacement":
            state["replacement_pending_at"] = timestamp
        elif event == "after-replacement":
            state["replacement_pending_at"] = None
        elif state.get("replacement_pending_at"):
            state["replacement_pending_at"] = None
        return "sample_gap" if current["gap"] else None

    previous_time = parse_time(last_sample)
    if at < previous_time:
        return mark_source_gap(state, at, "clock_regressed")

    reason = None
    if at - previous_time > MAX_SAMPLE_AGE:
        reason = "sample_missed"

    previous_created = state.get("last_container_created_at")
    previous_size = state.get("last_size_bytes")
    if previous_created is None or previous_size is None:
        delta = size_bytes
        reason = reason or "baseline_missing"
    elif created_at != previous_created:
        delta = size_bytes
        if not (event == "after-replacement" and state.get("replacement_pending_at")):
            reason = reason or "replacement_capture_missing"
    elif size_bytes < previous_size:
        delta = size_bytes
        reason = reason or "counter_regressed"
    else:
        delta = size_bytes - previous_size

    previous_day = previous_time.date()
    current_day = dt.date.fromisoformat(current["utc_date"])
    if at.date() != current_day:
        boundary = day_start(at.date())
        exact_boundary = (
            previous_day == current_day
            and current_day + dt.timedelta(days=1) == at.date()
            and previous_time < boundary
            and at - boundary <= MIDNIGHT_GRACE
            and reason is None
        )
        if exact_boundary:
            if current["bytes"] is not None:
                current["bytes"] += delta
            current["coverage_end"] = timestamp
            append_day(state, current)
            current = new_day(at.date(), start=format_time(boundary), end=timestamp, byte_count=0)
            state["current_day"] = current
        else:
            current["gap"] = True
            if current["bytes"] == 0:
                current["bytes"] = None
            append_day(state, current)
            day = current_day + dt.timedelta(days=1)
            while day < at.date():
                append_day(state, new_day(day, gap=True, byte_count=None))
                day += dt.timedelta(days=1)
            current = new_day(at.date(), gap=True, byte_count=None)
            state["current_day"] = current
            reason = reason or "utc_boundary_gap"
    else:
        if current["bytes"] is None:
            if not current["gap"]:
                current["bytes"] = 0
            current["coverage_start"] = timestamp
        if current["bytes"] is not None:
            current["bytes"] += delta
        current["coverage_end"] = timestamp
        if reason is not None:
            current["gap"] = True
            if current["bytes"] == 0:
                current["bytes"] = None

    if reason is not None and current["utc_date"] == at.date().isoformat():
        current["gap"] = True
        if current["bytes"] == 0:
            current["bytes"] = None

    if created_at != previous_created and state.get("replacement_pending_at"):
        state["replacement_pending_at"] = None
    elif event == "after-replacement":
        state["replacement_pending_at"] = None
    elif event == "before-replacement":
        state["replacement_pending_at"] = timestamp

    state["last_sample_at"] = timestamp
    state["last_container_created_at"] = created_at
    state["last_size_bytes"] = size_bytes
    return reason


def record_unavailable(state: dict[str, Any], at: dt.datetime, event: str) -> str:
    reason = "log_source_unavailable"
    mark_source_gap(state, at, reason)
    state["collection_start"] = state["collection_start"] or format_time(at)
    if event == "before-replacement":
        state["replacement_pending_at"] = format_time(at)
    return reason


def complete_day(row: dict[str, Any]) -> bool:
    if row.get("gap") or not isinstance(row.get("bytes"), int):
        return False
    day = dt.date.fromisoformat(row["utc_date"])
    start = row.get("coverage_start")
    end = row.get("coverage_end")
    return bool(start and end and parse_time(start) <= day_start(day) and
                parse_time(end) >= day_start(day + dt.timedelta(days=1)))


def summarize(state: dict[str, Any], report_time: dt.datetime | None = None) -> dict[str, Any]:
    rows = list(state["days"])
    if state.get("current_day") is not None:
        rows.append(state["current_day"])
    rows = rows[-MAX_RETAINED_DAYS - 1:]
    for row in rows:
        row["complete_utc_day"] = complete_day(row)

    complete = [row for row in rows if row["complete_utc_day"]]
    peaks: list[tuple[int, str, str]] = []
    for index in range(len(rows) - 6):
        window = rows[index:index + 7]
        dates = [dt.date.fromisoformat(row["utc_date"]) for row in window]
        if all(row["complete_utc_day"] for row in window) and all(
            dates[position] + dt.timedelta(days=1) == dates[position + 1]
            for position in range(6)
        ):
            peaks.append((sum(row["bytes"] for row in window), window[0]["utc_date"], window[-1]["utc_date"]))
    peak = max(peaks, default=None, key=lambda item: item[0])
    disk = os.statvfs("/")
    free_bytes = disk.f_bavail * disk.f_frsize
    ready = len(complete) >= 14 and peak is not None
    return {
        "start_time": state.get("collection_start"),
        "end_time": format_time(report_time or dt.datetime.now(dt.timezone.utc)),
        "complete_utc_days": len(complete),
        "days": rows,
        "coverage_gaps": [row["utc_date"] for row in rows if row.get("gap")],
        "maximum_rolling_seven_day_bytes": peak[0] if peak else None,
        "peak_window_start": peak[1] if peak else None,
        "peak_window_end": peak[2] if peak else None,
        "recommended_quota_bytes": peak[0] * 2 if ready else None,
        "host_free_bytes_at_report": free_bytes,
        "host_free_space_reserve_bytes": HOST_FREE_SPACE_RESERVE_BYTES,
        "available_for_logs_after_reserve_bytes": max(0, free_bytes - HOST_FREE_SPACE_RESERVE_BYTES),
        "recommendation_fits_current_free_space": (
            peak[0] * 2 <= max(0, free_bytes - HOST_FREE_SPACE_RESERVE_BYTES) if ready else None
        ),
        "ready_for_capacity_decision": ready,
    }


def log_gap(at: dt.datetime, reason: str) -> None:
    message = f"at={format_time(at)} gap=1 reason={reason}"
    subprocess.run(
        ["logger", "-p", "daemon.warning", "-t", "teagramd-log-volume", message],
        check=False,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("sample", "before-replacement", "after-replacement", "report"))
    parser.add_argument("--state-dir", type=Path, default=DEFAULT_STATE_DIR)
    args = parser.parse_args()
    if args.state_dir.exists() and (args.state_dir.is_symlink() or not args.state_dir.is_dir()):
        raise ValueError("accounting state directory is unsafe")
    args.state_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    if args.state_dir.is_symlink():
        raise ValueError("accounting state directory is unsafe")
    os.chmod(args.state_dir, 0o700)

    lock_path = args.state_dir / "collector.lock"
    lock_fd = os.open(lock_path, os.O_CREAT | os.O_RDWR | getattr(os, "O_NOFOLLOW", 0), 0o600)
    with os.fdopen(lock_fd, "r+") as lock_file:
        fcntl.flock(lock_file, fcntl.LOCK_EX)
        state = read_state(args.state_dir)
        if args.action == "report":
            print(json.dumps(summarize(state), separators=(",", ":"), sort_keys=True))
            return 0

        at = dt.datetime.now(dt.timezone.utc)
        event = args.action
        before_day = state.get("current_day")
        before_gap = bool(before_day and before_day.get("gap"))
        before_date = before_day.get("utc_date") if before_day else None
        try:
            created_at, size_bytes = read_snapshot()
            reason = record_reading(state, created_at, size_bytes, at, event)
        except SourceUnavailable:
            reason = record_unavailable(state, at, event)
        write_state(args.state_dir, state)
        current_day = state.get("current_day")
        if reason and current_day and (current_day.get("utc_date") != before_date or not before_gap):
            log_gap(at, reason)
        return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError, json.JSONDecodeError) as error:
        print(f"log-volume accounting failed: {type(error).__name__}", file=sys.stderr)
        raise SystemExit(1)
