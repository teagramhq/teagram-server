#!/usr/bin/env python3
"""Validate Atlas revision progress against the selected checkout's migrations."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
from typing import Any


HASH_RE = re.compile(r"h1:([A-Za-z0-9+/]{43}=)$")
MIGRATION_RE = re.compile(r"([0-9]{14})_[A-Za-z0-9][A-Za-z0-9._-]*\.sql$")
QUERY = """
SELECT version,
       hash,
       type::text,
       applied::text,
       total::text,
       (COALESCE(error, '') <> '')::text,
       (COALESCE(error_stmt, '') <> '')::text,
       (partial_hashes IS NOT NULL
        AND partial_hashes <> 'null'::jsonb
        AND partial_hashes <> '{}'::jsonb
        AND partial_hashes <> '[]'::jsonb)::text
FROM atlas_schema_revisions.atlas_schema_revisions
ORDER BY version;
"""


class GateError(Exception):
    """An expected validation failure that should be recorded in private evidence."""


def run_command(
    args: list[str],
    cwd: Path,
    timeout: int = 60,
    extra_env: dict[str, str] | None = None,
) -> subprocess.CompletedProcess[str]:
    environment = os.environ.copy()
    if extra_env:
        environment.update(extra_env)
    return subprocess.run(
        args,
        cwd=cwd,
        check=False,
        capture_output=True,
        env=environment,
        text=True,
        timeout=timeout,
    )


def parse_manifest(migrations_dir: Path) -> tuple[list[str], dict[str, str]]:
    checksum_path = migrations_dir / "atlas.sum"
    if checksum_path.is_symlink() or not checksum_path.is_file():
        raise GateError("atlas_sum_missing_or_symlinked")

    try:
        lines = checksum_path.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeError) as exc:
        raise GateError("atlas_sum_unreadable") from exc
    if not lines or not HASH_RE.fullmatch(lines[0]):
        raise GateError("atlas_sum_header_invalid")

    checksums: dict[str, str] = {}
    versions: dict[str, str] = {}
    for line in lines[1:]:
        fields = line.split(" ")
        if len(fields) != 2 or not HASH_RE.fullmatch(fields[1]):
            raise GateError("atlas_sum_entry_invalid")
        filename, checksum = fields
        match = MIGRATION_RE.fullmatch(filename)
        if not match or filename in checksums:
            raise GateError("migration_filename_or_sum_duplicate")
        version = match.group(1)
        if version in versions:
            raise GateError("migration_version_duplicate")
        checksums[filename] = checksum.removeprefix("h1:")
        versions[version] = filename

    if not checksums:
        raise GateError("migration_manifest_empty")

    migration_files: set[str] = set()
    try:
        for path in migrations_dir.iterdir():
            if path.name.endswith(".sql"):
                if path.is_symlink() or not path.is_file():
                    raise GateError("migration_input_not_regular_file")
                migration_files.add(path.name)
    except OSError as exc:
        raise GateError("migration_directory_unreadable") from exc
    if migration_files != set(checksums):
        raise GateError("migration_files_do_not_match_atlas_sum")

    ordered_versions = sorted(versions)
    if list(versions) != ordered_versions:
        raise GateError("migration_version_order_invalid")
    return ordered_versions, {version: checksums[filename] for version, filename in versions.items()}


def migration_inputs_clean(repo_root: Path) -> bool:
    result = run_command(
        [
            "git",
            "-C",
            str(repo_root),
            "status",
            "--porcelain=v1",
            "--untracked-files=all",
            "--ignored=matching",
            "--",
            "migrations/",
        ],
        repo_root,
    )
    return result.returncode == 0 and result.stdout == ""


def migration_mount_matches(repo_root: Path) -> bool:
    result = run_command(["docker", "compose", "config", "--format", "json"], repo_root)
    if result.returncode != 0:
        return False
    try:
        config = json.loads(result.stdout)
        volumes = config["services"]["migrate"]["volumes"]
        matches = [volume for volume in volumes if volume.get("target") == "/migrations"]
        if len(matches) != 1:
            return False
        mount = matches[0]
        source = Path(mount["source"]).resolve(strict=True)
        expected_source = (repo_root / "migrations").resolve(strict=True)
        return (
            mount.get("type") == "bind"
            and source == expected_source
            and mount.get("read_only") is True
        )
    except (KeyError, TypeError, ValueError, OSError):
        return False


def atlas_checksum_valid(repo_root: Path) -> bool:
    result = run_command(
        [
            "docker",
            "compose",
            "run",
            "--rm",
            "--no-deps",
            "migrate",
            "migrate",
            "validate",
            "--dir",
            "file:///migrations",
        ],
        repo_root,
    )
    return result.returncode == 0


def query_revisions(repo_root: Path, mode: str) -> tuple[list[dict[str, str]] | None, str]:
    result = run_command(
        [
            "docker",
            "compose",
            "exec",
            "-T",
            "postgres",
            "psql",
            "-X",
            "-A",
            "-t",
            "-F",
            "\t",
            "-v",
            "ON_ERROR_STOP=1",
            "-U",
            "postgres",
            "-d",
            "telegram",
            "-c",
            QUERY,
        ],
        repo_root,
        extra_env={"SCHEMA_GATE_MODE": mode},
    )
    if result.returncode != 0:
        return None, "unavailable"

    revisions: list[dict[str, str]] = []
    seen_versions: set[str] = set()
    for line in result.stdout.splitlines():
        fields = line.split("\t")
        if len(fields) != 8:
            return None, "malformed"
        version, checksum, revision_type, applied, total, error, error_stmt, partial = fields
        if (
            not version
            or not re.fullmatch(r"[0-9]{14}", version)
            or version in seen_versions
            or not checksum
            or not revision_type.isdigit()
            or not applied.isdigit()
            or not total.isdigit()
            or error not in {"true", "false"}
            or error_stmt not in {"true", "false"}
            or partial not in {"true", "false"}
        ):
            return None, "malformed"
        seen_versions.add(version)
        revisions.append(
            {
                "version": version,
                "hash": checksum,
                "type": revision_type,
                "applied": applied,
                "total": total,
                "error": error,
                "error_stmt": error_stmt,
                "partial": partial,
            }
        )
    return revisions, "available"


def revision_status(record: dict[str, str], expected_hash: str) -> str:
    failures: list[str] = []
    if record["hash"] != expected_hash:
        failures.append("hash_mismatch")
    # Atlas CLI v1.2.0 records successful applied revisions as type 2.
    if (
        record["type"] != "2"
        or record["applied"] != record["total"]
        or record["error"] != "false"
        or record["error_stmt"] != "false"
        or record["partial"] != "false"
    ):
        failures.append("partial_or_failed")
    return "+".join(failures) if failures else "pass"


def safe_version(version: str) -> str:
    return re.sub(r"[^A-Za-z0-9_.-]", "_", version)[:80] or "invalid"


def persist_evidence(evidence_dir: Path, mode: str, lines: list[str]) -> Path:
    if evidence_dir.is_symlink() or not evidence_dir.is_dir():
        raise GateError("evidence_directory_missing_or_symlinked")
    metadata = evidence_dir.stat()
    if stat.S_IMODE(metadata.st_mode) != 0o700 or metadata.st_uid != os.getuid():
        raise GateError("evidence_directory_not_private")

    result_path = evidence_dir / f"schema-result-gate-{mode}.tsv"
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    descriptor = os.open(result_path, flags, 0o600)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8", newline="\n") as output:
            output.write("\n".join(lines) + "\n")
            output.flush()
            os.fsync(output.fileno())
        directory_fd = os.open(evidence_dir, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except BaseException:
        try:
            result_path.unlink()
        except OSError:
            pass
        raise
    return result_path


def evaluate(mode: str, repo_root: Path, evidence_dir: Path) -> tuple[list[str], list[str], int]:
    lines = [f"mode={mode}"]
    failures: list[str] = []

    try:
        repo_root = repo_root.resolve(strict=True)
        migrations_dir = repo_root / "migrations"
        if migrations_dir.is_symlink() or not migrations_dir.is_dir():
            raise GateError("migration_directory_missing_or_symlinked")
        expected_versions, expected_hashes = parse_manifest(migrations_dir)
        manifest_error = ""
    except (GateError, OSError) as exc:
        expected_versions, expected_hashes = [], {}
        manifest_error = str(exc)

    clean = migration_inputs_clean(repo_root)
    lines.append(f"migration_inputs_clean={'true' if clean else 'false'}")
    if not clean:
        failures.append("migration_inputs_clean")

    mount_ok = migration_mount_matches(repo_root)
    lines.append(f"migration_mount_read_only={'true' if mount_ok else 'false'}")
    if not mount_ok:
        failures.append("migration_mount_read_only")

    atlas_ok = atlas_checksum_valid(repo_root)
    lines.append(f"atlas_checksum_valid={'true' if atlas_ok else 'false'}")
    if not atlas_ok:
        failures.append("atlas_checksum_valid")

    manifest_ok = not manifest_error
    lines.append(f"migration_manifest_valid={'true' if manifest_ok else 'false'}")
    if manifest_error:
        lines.append(f"migration_manifest_error={manifest_error}")
    if not manifest_ok:
        failures.append("migration_manifest_valid")
    lines.append(f"expected_migration_count={len(expected_versions)}")

    revisions, query_status = query_revisions(repo_root, mode)
    lines.append(f"database_revision_query={query_status}")
    if revisions is None:
        failures.append("database_revision_query")
        actual_versions: list[str] = []
    else:
        actual_versions = [record["version"] for record in revisions]
    lines.append(f"actual_revision_count={len(actual_versions)}")

    if revisions is None:
        lines.append("revision_set=unavailable")
    else:
        by_version = {record["version"]: record for record in revisions}
        for version in expected_versions:
            record = by_version.get(version)
            if record is None:
                lines.append(f"revision_{version}=missing")
                continue
            status = revision_status(record, expected_hashes[version])
            lines.append(f"revision_{version}={status}")
            if status != "pass":
                failures.append(f"revision_{version}")

        extra_versions = [version for version in actual_versions if version not in expected_hashes]
        for version in extra_versions:
            lines.append(f"revision_{safe_version(version)}=unexpected")
            failures.append(f"revision_{safe_version(version)}")

        if mode == "pre":
            ordered_prefix = (
                len(actual_versions) <= len(expected_versions)
                and actual_versions == expected_versions[: len(actual_versions)]
            )
            lines.append(f"ordered_prefix={'true' if ordered_prefix else 'false'}")
            if not ordered_prefix:
                failures.append("ordered_prefix")
            set_ok = ordered_prefix and not extra_versions
        else:
            set_ok = actual_versions == expected_versions
        lines.append(f"revision_set={'complete' if set_ok and mode == 'post' else 'prefix' if set_ok else 'reject'}")
        if not set_ok:
            failures.append("revision_set")

    unique_failures = sorted(set(failures))
    result = "pass" if not unique_failures else "reject"
    lines.append(f"failed_checks={','.join(unique_failures) if unique_failures else 'none'}")
    lines.append(f"gate_result={result}")
    return lines, unique_failures, len(actual_versions)


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Check Atlas revision progress for the selected repository checkout."
    )
    subparsers = parser.add_subparsers(dest="command", required=True)
    check_parser = subparsers.add_parser("check")
    check_parser.add_argument("mode", choices=("pre", "post"))
    check_parser.add_argument("evidence_dir", type=Path)
    check_parser.add_argument("repo_root", type=Path)
    args = parser.parse_args()

    try:
        lines, failures, revision_count = evaluate(args.mode, args.repo_root, args.evidence_dir)
        persist_evidence(args.evidence_dir, args.mode, lines)
    except (GateError, OSError, subprocess.SubprocessError) as exc:
        print(f"schema gate rejected mode={args.mode} reason={exc}", file=sys.stderr)
        return 1

    if failures:
        print(
            f"schema_gate=reject mode={args.mode} failed_checks={','.join(failures)}",
            file=sys.stderr,
        )
        return 1
    print(f"schema_gate=pass mode={args.mode} revisions={revision_count}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
