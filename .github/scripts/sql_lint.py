#!/usr/bin/env python3
"""Lint every sqlc query and only newly added Atlas migration SQL."""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
from pathlib import Path


def changed_migrations(repo_root: Path, base: str, head: str) -> tuple[list[str], list[str]]:
    """Return added SQL migrations and changed historical SQL migrations."""
    result = subprocess.run(
        [
            "git",
            "diff",
            "--no-renames",
            "--name-status",
            "-z",
            f"{base}...{head}",
            "--",
            "migrations/",
        ],
        cwd=repo_root,
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )

    fields = result.stdout.split(b"\0")
    new_migrations: list[str] = []
    historical_changes: list[str] = []
    offset = 0
    while offset < len(fields) and fields[offset]:
        status = fields[offset].decode("ascii")
        offset += 1
        if offset >= len(fields):
            raise RuntimeError("git returned an incomplete migration diff")
        path = os.fsdecode(fields[offset])
        offset += 1

        if not path.endswith(".sql"):
            continue
        if status == "A":
            new_migrations.append(path)
        else:
            historical_changes.append(path)

    return sorted(new_migrations), sorted(historical_changes)


def lint_paths(repo_root: Path, sqlfluff: str, paths: list[str]) -> int:
    if not paths:
        return 0

    command = [sqlfluff, "lint", "--config", ".sqlfluff", *paths]
    print(f"Running SQLFluff on {', '.join(paths)}", flush=True)
    try:
        return subprocess.run(command, cwd=repo_root, check=False).returncode
    except OSError as error:
        print(f"Could not run SQLFluff: {error}", file=sys.stderr)
        return 1


def lint_changed_sql(repo_root: Path, base: str, head: str, sqlfluff: str) -> int:
    try:
        new_migrations, historical_changes = changed_migrations(repo_root, base, head)
    except (OSError, subprocess.CalledProcessError, RuntimeError) as error:
        detail = getattr(error, "stderr", b"")
        if isinstance(detail, bytes):
            detail = detail.decode(errors="replace").strip()
        suffix = f": {detail}" if detail else f": {error}"
        print(f"Could not select SQL migrations from the PR diff{suffix}", file=sys.stderr)
        return 1

    query_status = lint_paths(repo_root, sqlfluff, ["internal/store/queries"])
    migration_status = 0
    if new_migrations:
        migration_status = lint_paths(repo_root, sqlfluff, new_migrations)
    else:
        print("No new SQL migrations to lint", flush=True)

    if historical_changes:
        print(
            "Existing SQL migrations are immutable; add a new migration instead:",
            file=sys.stderr,
        )
        for path in historical_changes:
            print(f"  {path}", file=sys.stderr)

    if query_status != 0 or migration_status != 0 or historical_changes:
        return 1
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", required=True, help="PR base or previous push SHA")
    parser.add_argument("--head", required=True, help="PR head or current push SHA")
    parser.add_argument("--sqlfluff", default=".venv-sqlfluff/bin/sqlfluff")
    args = parser.parse_args()

    repo_root = Path(__file__).resolve().parents[2]
    return lint_changed_sql(repo_root, args.base, args.head, args.sqlfluff)


if __name__ == "__main__":
    raise SystemExit(main())
