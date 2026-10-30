#!/usr/bin/env python3
"""Preserve migration file transactions while Atlas replays unchanged history."""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path


_NON_SQL = re.compile(
    r"'(?:''|\\.|[^'])*'"
    r"|\$(?P<tag>[a-z_][a-z_0-9]*|)\$.*?\$(?P=tag)\$"
    r"|--[^\r\n]*"
    r"|/\*.*?\*/",
    re.IGNORECASE | re.DOTALL,
)


def _mask_comments_and_literals(sql: str) -> str:
    def blank(match: re.Match[str]) -> str:
        return "".join("\n" if char == "\n" else " " for char in match.group(0))

    return _NON_SQL.sub(blank, sql)


def _has_top_level_lock(sql: str) -> bool:
    statements = _mask_comments_and_literals(sql).split(";")
    return any(re.match(r"^\s*LOCK\s+TABLE\b", statement, re.IGNORECASE) for statement in statements)


def _has_txmode_none(sql: str) -> bool:
    return re.search(r"(?im)^\s*--\s*atlas:txmode\s+none\b", sql) is not None


def _has_explicit_transaction(sql: str) -> bool:
    statements = _mask_comments_and_literals(sql).split(";")
    return any(
        re.match(r"^\s*(?:BEGIN|COMMIT|ROLLBACK)(?:\s+WORK)?\s*$", statement, re.IGNORECASE)
        for statement in statements
    )


def prepare_history(migrations_dir: Path, changed_files: set[str]) -> list[Path]:
    """Wrap unchanged default-mode files with top-level locks for Atlas replay."""
    changed_names = {Path(name).name for name in changed_files}
    wrapped: list[Path] = []
    for path in sorted(migrations_dir.glob("*.sql")):
        if path.name in changed_names:
            continue

        sql = path.read_text(encoding="utf-8")
        if _has_txmode_none(sql) or not _has_top_level_lock(sql):
            continue
        if _has_explicit_transaction(sql):
            raise ValueError(f"cannot add replay transaction around explicit transaction in {path.name}")

        path.write_text(f"BEGIN;\n{sql.rstrip()}\n\nCOMMIT;\n", encoding="utf-8")
        wrapped.append(path)
    return wrapped


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--migrations-dir", required=True, type=Path)
    parser.add_argument("--changed-file", action="append", default=[])
    args = parser.parse_args()

    try:
        wrapped = prepare_history(args.migrations_dir, set(args.changed_file))
    except (OSError, ValueError) as error:
        print(error, file=sys.stderr)
        return 1

    if wrapped:
        names = ", ".join(path.name for path in wrapped)
        print(f"Atlas lint history: preserved file transactions for {names}")
    else:
        print("Atlas lint history: no unchanged top-level table locks to wrap")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
