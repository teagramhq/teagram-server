#!/usr/bin/env python3
"""Validate and stage a CI-verified teagram-web private artifact."""

from __future__ import annotations

import hashlib
import json
import os
import pathlib
import re
import shutil
import stat
import sys
import tarfile
import tempfile


MAX_ARCHIVE_BYTES = 128 * 1024 * 1024
MAX_RELEASE_BYTES = 256 * 1024 * 1024
MAX_MEMBERS = 20_000
# Mirrors the upstream private-artifact publisher's default JavaScript localeCompare ordering.
ARTIFACT_PATH_COLLATION = (
    " _-,;:!?.'\"()[]{}@*/\\&#%`^+<=>|~$"
    "0123456789abcdefghijklmnopqrstuvwxyz"
)
ARTIFACT_PATH_ORDER = {character: index for index, character in enumerate(ARTIFACT_PATH_COLLATION)}


def reject(message: str) -> None:
    print(f"demo web release rejected: {message}", file=sys.stderr)
    raise SystemExit(1)


def verify_directory(path: pathlib.Path, *, mode: int | None = None) -> None:
    try:
        metadata = path.lstat()
    except OSError:
        reject("release-directory-unavailable")
    if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISDIR(metadata.st_mode) or metadata.st_uid != os.geteuid():
        reject("release-directory-owner-or-type")
    if mode is not None and stat.S_IMODE(metadata.st_mode) != mode:
        reject("release-directory-mode")


def verify_archive(path: pathlib.Path, expected_sha256: str) -> None:
    if not re.fullmatch(r"[0-9a-f]{64}", expected_sha256):
        reject("archive-checksum-format")
    try:
        metadata = path.lstat()
    except OSError:
        reject("archive-unavailable")
    if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISREG(metadata.st_mode) or metadata.st_uid != os.geteuid():
        reject("archive-owner-or-type")
    if metadata.st_size <= 0 or metadata.st_size > MAX_ARCHIVE_BYTES or stat.S_IMODE(metadata.st_mode) != 0o600:
        reject("archive-size-or-mode")
    digest = hashlib.sha256()
    with path.open("rb") as archive:
        for chunk in iter(lambda: archive.read(1024 * 1024), b""):
            digest.update(chunk)
    if digest.hexdigest() != expected_sha256:
        reject("archive-checksum-mismatch")


def normalized_member_name(name: str) -> pathlib.PurePosixPath | None:
    if "\\" in name or "\x00" in name:
        reject("archive-path-shape")
    while name.startswith("./"):
        name = name[2:]
    if name in ("", "."):
        return None
    path = pathlib.PurePosixPath(name)
    if path.is_absolute() or any(part in ("", ".", "..") for part in path.parts):
        reject("archive-path-traversal")
    return path


def verify_tree(directory: pathlib.Path) -> None:
    for current, child_directories, child_files in os.walk(directory, followlinks=False):
        current_path = pathlib.Path(current)
        current_metadata = current_path.lstat()
        if (
            stat.S_ISLNK(current_metadata.st_mode)
            or not stat.S_ISDIR(current_metadata.st_mode)
            or current_metadata.st_uid != os.geteuid()
            or stat.S_IMODE(current_metadata.st_mode) != 0o755
        ):
            reject("release-directory-owner-mode-or-type")
        for child in child_directories:
            child_metadata = (current_path / child).lstat()
            if stat.S_ISLNK(child_metadata.st_mode) or not stat.S_ISDIR(child_metadata.st_mode):
                reject("release-directory-type")
        for child in child_files:
            child_metadata = (current_path / child).lstat()
            if (
                stat.S_ISLNK(child_metadata.st_mode)
                or not stat.S_ISREG(child_metadata.st_mode)
                or child_metadata.st_uid != os.geteuid()
                or stat.S_IMODE(child_metadata.st_mode) != 0o644
            ):
                reject("release-file-owner-mode-or-type")


def extract_archive(archive_path: pathlib.Path, destination: pathlib.Path) -> None:
    seen_files: set[str] = set()
    total_bytes = 0
    member_count = 0
    try:
        archive = tarfile.open(archive_path, mode="r:gz")
    except (OSError, tarfile.TarError):
        reject("archive-format")

    with archive:
        for member in archive:
            member_count += 1
            if member_count > MAX_MEMBERS:
                reject("archive-member-count")
            relative = normalized_member_name(member.name)
            if relative is None:
                if member.isdir():
                    continue
                reject("archive-root-entry")
            output_path = destination.joinpath(*relative.parts)
            if member.isdir():
                if member.size != 0:
                    reject("archive-directory-size")
                output_path.mkdir(mode=0o755, parents=True, exist_ok=True)
                continue
            if not member.isfile() or member.size < 0:
                reject("archive-link-or-special-file")
            key = relative.as_posix()
            if key in seen_files:
                reject("archive-duplicate-file")
            seen_files.add(key)
            total_bytes += member.size
            if total_bytes > MAX_RELEASE_BYTES:
                reject("release-too-large")
            output_path.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
            source = archive.extractfile(member)
            if source is None:
                reject("archive-file-unreadable")
            try:
                with source, output_path.open("xb") as output:
                    shutil.copyfileobj(source, output, length=1024 * 1024)
                    output.flush()
                    os.fsync(output.fileno())
                output_path.chmod(0o644)
            except OSError:
                reject("release-file-write")


def verify_manifest(directory: pathlib.Path, source_commit: str, artifact_digest: str) -> None:
    manifest_path = directory / "mtproto-target.json"
    try:
        metadata = manifest_path.lstat()
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError):
        reject("artifact-manifest-unavailable")
    if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISREG(metadata.st_mode):
        reject("artifact-manifest-type")
    if not isinstance(manifest, dict) or set(manifest) != {
        "mode", "endpoint", "fingerprint", "sourceCommit", "artifactDigest"
    }:
        reject("artifact-manifest-fields")
    if (
        manifest.get("mode") != "private"
        or manifest.get("sourceCommit") != source_commit
        or manifest.get("artifactDigest") != artifact_digest
        or not isinstance(manifest.get("endpoint"), str)
        or not re.fullmatch(r"wss://[^/\s]+(?:/[^\s]*)?", manifest["endpoint"])
        or not re.fullmatch(r"[0-9a-f]{16}", str(manifest.get("fingerprint", "")))
    ):
        reject("artifact-manifest-identity")
    index_path = directory / "index.html"
    try:
        index_metadata = index_path.lstat()
        index = index_path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError):
        reject("artifact-index-unavailable")
    if stat.S_ISLNK(index_metadata.st_mode) or not stat.S_ISREG(index_metadata.st_mode):
        reject("artifact-index-type")
    if "<title>Teagram Web</title>" not in index or "type=\"module\"" not in index:
        reject("artifact-index-shell")

    def artifact_path_key(path: pathlib.Path) -> tuple[tuple[int, ...], tuple[int, ...]]:
        relative_path = path.relative_to(directory).as_posix()
        if any(
            not character.isascii() or character.lower() not in ARTIFACT_PATH_ORDER
            for character in relative_path
        ):
            reject("artifact-path-character")
        primary = tuple(ARTIFACT_PATH_ORDER[character.lower()] for character in relative_path)
        case = tuple(1 if character.isupper() else 0 for character in relative_path)
        return primary, case

    digest = hashlib.sha256()
    files = sorted(
        (
            pathlib.Path(current) / name
            for current, _, names in os.walk(directory, followlinks=False)
            for name in names
        ),
        key=artifact_path_key,
    )
    for path in files:
        relative_path = path.relative_to(directory).as_posix()
        if relative_path == "mtproto-target.json":
            continue
        contents = path.read_bytes()
        digest.update(f"{relative_path}\0{len(contents)}\0".encode())
        digest.update(contents)
        digest.update(b"\0")
    if f"sha256:{digest.hexdigest()}" != artifact_digest:
        reject("artifact-content-digest-mismatch")


def stage(
    archive_path: pathlib.Path,
    releases_dir: pathlib.Path,
    source_commit: str,
    artifact_digest: str,
    archive_sha256: str,
) -> pathlib.Path:
    if not re.fullmatch(r"[0-9a-f]{40}", source_commit):
        reject("source-commit-format")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", artifact_digest):
        reject("artifact-digest-format")
    verify_directory(releases_dir, mode=0o755)
    verify_archive(archive_path, archive_sha256)
    temporary = pathlib.Path(tempfile.mkdtemp(prefix=".web-stage-", dir=releases_dir))
    temporary.chmod(0o755)
    try:
        extract_archive(archive_path, temporary)
        for directory, _, _ in os.walk(temporary):
            pathlib.Path(directory).chmod(0o755)
        verify_tree(temporary)
        verify_manifest(temporary, source_commit, artifact_digest)
        release_name = f"{source_commit}-{artifact_digest.removeprefix('sha256:')}"
        destination = releases_dir / release_name
        if destination.exists() or destination.is_symlink():
            verify_directory(destination, mode=0o755)
            verify_tree(destination)
            verify_manifest(destination, source_commit, artifact_digest)
            shutil.rmtree(temporary)
            return destination
        os.replace(temporary, destination)
        directory_fd = os.open(releases_dir, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
        return destination
    except BaseException:
        if temporary.exists() and not temporary.is_symlink():
            shutil.rmtree(temporary, ignore_errors=True)
        raise


def main() -> None:
    if len(sys.argv) != 6 or sys.argv[1] != "stage":
        reject("usage")
    destination = stage(
        pathlib.Path(sys.argv[2]),
        pathlib.Path(sys.argv[3]),
        sys.argv[4],
        sys.argv[5],
        os.environ.get("DEPLOY_WEB_ARCHIVE_SHA256", ""),
    )
    print(destination.name)


if __name__ == "__main__":
    main()
