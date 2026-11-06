#!/usr/bin/env python3
"""Persist the demo web publication watermark under the deploy lock."""

import argparse
import json
import os
import pathlib
import re
import stat
import tempfile

MARKER = b"web-publication-state-v1\n"


def valid_identity(value):
    return (
        isinstance(value, dict)
        and set(value) == {"run_id", "source_commit", "artifact_digest"}
        and type(value["run_id"]) is int and 0 < value["run_id"] < 2**63
        and isinstance(value["source_commit"], str)
        and re.fullmatch(r"[0-9a-f]{40}", value["source_commit"]) is not None
        and isinstance(value["artifact_digest"], str)
        and re.fullmatch(r"sha256:[0-9a-f]{64}", value["artifact_digest"]) is not None
    )


def same_release(left, right):
    return left is not None and right is not None and all(
        left.get(key) == right.get(key) for key in ("source_commit", "artifact_digest")
    )


def check_directory(path):
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o022:
        raise ValueError("publication state directory is unsafe")


def load(path):
    check_directory(path.parent)
    marker = path.with_name(path.name + ".guard")
    try:
        marker_info = marker.lstat()
    except FileNotFoundError:
        marker_info = None
    if marker_info is not None and (
        not stat.S_ISREG(marker_info.st_mode) or marker_info.st_uid != os.geteuid()
        or stat.S_IMODE(marker_info.st_mode) != 0o600 or marker_info.st_size != len(MARKER)
        or marker.read_bytes() != MARKER
    ):
        raise ValueError("publication state guard is unsafe")
    try:
        info = path.lstat()
    except FileNotFoundError:
        if marker_info is not None:
            raise ValueError("publication state is missing after initialization")
        return None
    if marker_info is None:
        raise ValueError("publication state guard is missing")
    if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid()
        or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > 4096):
        raise ValueError("publication state file is unsafe")
    try:
        state = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        raise ValueError("publication state is invalid") from error
    if (not isinstance(state, dict) or set(state) != {"version", "highest", "installed", "pending"}
        or type(state["version"]) is not int or state["version"] != 1):
        raise ValueError("publication state fields are invalid")
    for field in ("highest", "installed", "pending"):
        if state[field] is not None and not valid_identity(state[field]):
            raise ValueError("publication state identity is invalid")
    if any(state[field] is not None for field in ("installed", "pending")) and state["highest"] is None:
        raise ValueError("publication state watermark is missing")
    if state["highest"] is not None and any(
        state[field] is not None and state[field]["run_id"] > state["highest"]["run_id"]
        for field in ("installed", "pending")
    ):
        raise ValueError("publication state watermark is invalid")
    identities = [state[field] for field in ("highest", "installed", "pending") if state[field] is not None]
    for left in identities:
        if any(right["run_id"] == left["run_id"] and right != left for right in identities):
            raise ValueError("publication state reuses a run ID for different content")
    return state


def save(path, state):
    check_directory(path.parent)
    marker = path.with_name(path.name + ".guard")
    if not marker.exists():
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
        with os.fdopen(os.open(marker, flags, 0o600), "wb") as output:
            os.fchmod(output.fileno(), 0o600)
            output.write(MARKER)
            output.flush()
            os.fsync(output.fileno())
        marker_directory = os.open(path.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(marker_directory)
        finally:
            os.close(marker_directory)
    descriptor, name = tempfile.mkstemp(prefix=".web-publication-", dir=path.parent)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as output:
            json.dump(state, output, separators=(",", ":"))
            output.flush()
            os.fsync(output.fileno())
        os.replace(name, path)
        fd = os.open(path.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def prepare(path, current, candidate, allow_rollback):
    if not valid_identity(candidate) or type(allow_rollback) is not bool:
        raise ValueError("candidate publication is invalid")
    state = load(path)
    if state is None:
        if current is not None and not valid_identity(current):
            raise ValueError("existing publication needs a verified baseline run")
        state = {"version": 1, "highest": current, "installed": current, "pending": None}
    installed, pending, highest = state["installed"], state["pending"], state["highest"]
    if current is None:
        if installed is not None:
            raise ValueError("installed publication is missing")
    elif not same_release(current, installed) and not same_release(current, pending):
        raise ValueError("current release differs from publication state")
    if pending is not None and same_release(current, pending) and not same_release(current, installed) and candidate != pending:
        raise ValueError("interrupted publication must be recovered first")
    for known in (highest, installed, pending):
        if known is not None and known["run_id"] == candidate["run_id"] and known != candidate:
            raise ValueError("publication run identity changed")
    if installed == candidate and same_release(current, installed):
        state["pending"] = None
        save(path, state)
        return "noop"
    if highest is not None and candidate["run_id"] < highest["run_id"] and not allow_rollback:
        raise ValueError("older web publication requires an authorized rollback")
    if highest is None or candidate["run_id"] > highest["run_id"]:
        state["highest"] = candidate
    state["pending"] = candidate
    save(path, state)
    return "install"


def commit(path, candidate):
    state = load(path)
    if state is None or state["pending"] != candidate:
        raise ValueError("publication was not prepared")
    state["installed"] = candidate
    state["pending"] = None
    save(path, state)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("operation", choices=("prepare", "commit"))
    parser.add_argument("state", type=pathlib.Path)
    parser.add_argument("run_id", type=int)
    parser.add_argument("source_commit")
    parser.add_argument("artifact_digest")
    parser.add_argument("--current-source", default="")
    parser.add_argument("--current-digest", default="")
    parser.add_argument("--baseline-run-id", type=int, default=0)
    parser.add_argument("--allow-rollback", action="store_true")
    args = parser.parse_args()
    candidate = {"run_id": args.run_id, "source_commit": args.source_commit, "artifact_digest": args.artifact_digest}
    try:
        if args.operation == "commit":
            commit(args.state, candidate)
        else:
            current = None
            if args.current_source or args.current_digest:
                current = {"run_id": args.baseline_run_id, "source_commit": args.current_source,
                           "artifact_digest": args.current_digest}
            print(prepare(args.state, current, candidate, args.allow_rollback))
    except (OSError, ValueError) as error:
        parser.exit(1, f"demo web publication rejected: {error}\n")


if __name__ == "__main__":
    main()
