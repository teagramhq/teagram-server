#!/usr/bin/env python3
"""Reject common PostgreSQL migration patterns that can hold long table locks."""

from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys
from pathlib import Path


_NON_SQL = re.compile(
    r"'(?:''|\\.|[^'])*'"
    r"|\$(?P<tag>[a-z_][a-z_0-9]*|)\$.*?\$(?P=tag)\$"
    r"|--[^\r\n]*"
    r"|/\*.*?\*/",
    re.IGNORECASE | re.DOTALL,
)
_CREATE_TABLE = re.compile(
    r"\bCREATE\s+(?:UNLOGGED\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"
    r'(?P<table>(?:"[^"]+"|[a-z_][\w$]*)(?:\s*\.\s*(?:"[^"]+"|[a-z_][\w$]*))?)',
    re.IGNORECASE,
)
_ALTER_TABLE = re.compile(
    r"^\s*ALTER\s+TABLE\s+(?:ONLY\s+)?"
    r'(?P<table>(?:"[^"]+"|[a-z_][\w$]*)(?:\s*\.\s*(?:"[^"]+"|[a-z_][\w$]*))?)',
    re.IGNORECASE,
)
_CREATE_INDEX = re.compile(
    r"^\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+"
    r"(?P<concurrently>CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?[^\s]+\s+"
    r"ON\s+(?:ONLY\s+)?"
    r'(?P<table>(?:"[^"]+"|[a-z_][\w$]*)(?:\s*\.\s*(?:"[^"]+"|[a-z_][\w$]*))?)',
    re.IGNORECASE,
)
_DROP_INDEX = re.compile(r"^\s*DROP\s+INDEX\s+(?P<concurrently>CONCURRENTLY\s+)?", re.IGNORECASE)
_LOCK_WAIVER = re.compile(r"(?im)^\s*--\s*atlas:lock-waiver:\s*(\S.*)$")


def _mask_comments_and_literals(sql: str) -> str:
    def blank(match: re.Match[str]) -> str:
        return "".join("\n" if char == "\n" else " " for char in match.group(0))

    return _NON_SQL.sub(blank, sql)


def _identifier(value: str) -> str:
    parts = re.findall(r'"([^"]+)"|([a-z_][\w$]*)', value, re.IGNORECASE)
    return ".".join(quoted or unquoted.lower() for quoted, unquoted in parts)


def _has_nolint(sql: str, analyzer: str) -> bool:
    for line in sql.splitlines():
        match = re.match(r"\s*--\s*atlas:nolint\s+(.+)$", line, re.IGNORECASE)
        if match and analyzer.lower() in {name.lower() for name in match.group(1).split()}:
            return True
    return False


def _created_tables(statements: list[str]) -> set[str]:
    tables: set[str] = set()
    for statement in statements:
        match = _CREATE_TABLE.search(statement)
        if match:
            tables.add(_identifier(match.group("table")))
    return tables


def _statement_risks(statement: str, created_tables: set[str]) -> list[str]:
    risks: list[str] = []
    if re.match(r"^\s*DO\b", statement, re.IGNORECASE):
        risks.append("DO blocks can hide procedural DDL whose lock risk this lint cannot inspect")

    index = _CREATE_INDEX.search(statement)
    if index and not index.group("concurrently"):
        if _identifier(index.group("table")) not in created_tables:
            risks.append("CREATE INDEX without CONCURRENTLY can block writes on an existing table")

    drop_index = _DROP_INDEX.search(statement)
    if drop_index and not drop_index.group("concurrently"):
        risks.append("DROP INDEX without CONCURRENTLY can block table access")

    alter = _ALTER_TABLE.search(statement)
    if not alter:
        return risks

    if _identifier(alter.group("table")) in created_tables:
        return risks

    if re.search(r"\bALTER\s+(?:COLUMN\s+)?[^;]*\b(?:SET\s+DATA\s+)?TYPE\b", statement, re.IGNORECASE):
        risks.append("changing a column type can rewrite and lock the table")
    if re.search(r"\bSET\s+NOT\s+NULL\b", statement, re.IGNORECASE):
        risks.append("setting NOT NULL scans and locks the existing table")
    if re.search(r"\bSET\s+(?:UNLOGGED|LOGGED)\b", statement, re.IGNORECASE):
        risks.append("changing table logging mode rewrites the table")
    if re.search(
        r"\bADD\s+(?:CONSTRAINT\b.*?\s+)?(?:PRIMARY\s+KEY|UNIQUE|EXCLUDE)\b",
        statement,
        re.IGNORECASE | re.DOTALL,
    ) and not re.search(r"\bUSING\s+INDEX\b", statement, re.IGNORECASE):
        risks.append("adding a primary, unique, or exclusion constraint scans and locks the table")
    if re.search(
        r"\bADD\s+(?:CONSTRAINT\b.*?\s+)?(?:CHECK|FOREIGN\s+KEY)\b",
        statement,
        re.IGNORECASE | re.DOTALL,
    ) and not re.search(r"\bNOT\s+VALID\b", statement, re.IGNORECASE):
        risks.append("validating a new CHECK or FOREIGN KEY constraint scans and locks the table")
    if re.search(
        r"\bADD\s+(?:COLUMN\s+)?[^;]*\bDEFAULT\s+[^;]*"
        r"\b(?:clock_timestamp|random|nextval|setval|gen_random_uuid|uuid_generate_v4)\s*\(",
        statement,
        re.IGNORECASE,
    ):
        risks.append("adding a column with a volatile default can rewrite the table")

    return risks


def lint_migration(sql: str, created_tables: set[str] | None = None) -> list[str]:
    statements = [statement for statement in _mask_comments_and_literals(sql).split(";") if statement.strip()]
    created_tables = created_tables if created_tables is not None else _created_tables(statements)
    risks = [risk for statement in statements for risk in _statement_risks(statement, created_tables)]

    if risks and _has_nolint(sql, "blocking"):
        if not _LOCK_WAIVER.search(sql):
            return ["blocking opt-out requires a non-empty `-- atlas:lock-waiver: <reason>` comment"]
        return []
    return risks


def changed_migrations(base: str) -> list[Path]:
    result = subprocess.run(
        ["git", "diff", "--name-only", "--diff-filter=AMR", f"{base}...HEAD", "--", "migrations"],
        check=True,
        capture_output=True,
        text=True,
    )
    return [Path(name) for name in result.stdout.splitlines() if name.endswith(".sql")]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", default=None, help="Git base ref used to select migration changes")
    args = parser.parse_args()
    base = args.base or os.environ.get("MIGRATIONS_BASE_REF")
    if not base:
        parser.error("--base or MIGRATIONS_BASE_REF is required")

    files = changed_migrations(base)
    if not files:
        print("Migration lock lint: no changed SQL migrations")
        return 0

    contents = {path: path.read_text(encoding="utf-8") for path in files}
    statements_by_file = {
        path: [statement for statement in _mask_comments_and_literals(sql).split(";") if statement.strip()]
        for path, sql in contents.items()
    }
    new_tables = set().union(*(_created_tables(statements) for statements in statements_by_file.values()))
    findings: list[str] = []
    for path, sql in contents.items():
        for risk in lint_migration(sql, new_tables):
            findings.append(
                f"{path}: {risk}; add `-- atlas:nolint blocking` and "
                "`-- atlas:lock-waiver: <reason>` only for a reviewed exception"
            )

    if findings:
        print("\n".join(findings), file=sys.stderr)
        return 1

    print(f"Migration lock lint: checked {len(files)} changed SQL migration(s)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
