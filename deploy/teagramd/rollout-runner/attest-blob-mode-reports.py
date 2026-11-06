#!/usr/bin/env python3
"""Attest a one-time loss of blob-mode transition reports."""

from __future__ import annotations

import hashlib
import importlib.util
import json
import os
import pathlib
import re
import stat
import subprocess
import sys


SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
CHECKOUT = pathlib.Path("/opt/teagram")
STATE_DIR = CHECKOUT / ".state" / "blob-mode"
REPORT_ROOT = pathlib.Path("/var/lib/teagram-blob-mode-reports")
ARCHIVE_ROOT = pathlib.Path("/var/lib/teagram-blob-mode-archives")
LOCK_PATH = pathlib.Path("/tmp/teagram-deploy.lock")
ROOT_UID = 0
TEST_MODE = False

ATTESTATION_SCHEMA = "teagram.blob-mode-report-loss-attestation/v1"
ARCHIVE_SCHEMA = "teagram.blob-mode-report-loss-archive/v1"
CENSUS_SCHEMA = "teagram.blob-mode-report-loss-census/v1"
RECOVERY_SCHEMA = "teagram.blob-mode-report-loss-recovery/v2"
LEGACY_RECOVERY_SCHEMA = "teagram.blob-mode-report-loss-recovery/v1"
ATTESTATION_PREFIX = "teagramd-blob-mode-report-loss-attestation-"
ARCHIVE_MANIFEST_NAME = "teagramd-blob-mode-report-loss-archive-manifest.json"
CENSUS_MANIFEST_NAME = "teagramd-blob-mode-report-loss-census-manifest.json"
RECOVERY_MARKER_NAME = ".teagramd-blob-mode-report-loss-attestation-in-progress.json"
RECOVERY_MARKER_TEMP_PREFIX = ".tmp-report-loss-recovery-"
ATTESTATION_TEMP_PREFIX = ".tmp-report-loss-attestation-"
REPORT_ATTESTATION_PREFIX = ATTESTATION_PREFIX
REPORT_LOSS_ARCHIVE_MANIFEST = ARCHIVE_MANIFEST_NAME
REPORT_LOSS_CENSUS_MANIFEST = CENSUS_MANIFEST_NAME

REFERENCE_QUERY = """
SELECT key || chr(9) || logical_bytes::text
FROM (
    SELECT lpad(to_hex(f.id & 255), 2, '0') || '/' || f.id::text AS key,
           f.size AS logical_bytes
    FROM files AS f
    WHERE f.stored = true
    UNION ALL
    SELECT up.blob_key AS key, up.size AS logical_bytes
    FROM upload_parts AS up
    WHERE up.blob_key <> ''
) AS refs
ORDER BY key;
"""


class AttestationReject(Exception):
    """A fixed, sanitized reason for refusing the operation."""


def reject(reason: str) -> None:
    raise AttestationReject(reason)


def import_helper(name: str, path: pathlib.Path):
    spec = importlib.util.spec_from_file_location(name, path)
    if spec is None or spec.loader is None:
        reject("runtime-helper-unavailable")
    module = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(module)
    except Exception:
        reject("runtime-helper-unavailable")
    return module


mode = import_helper("attestation_blob_mode_state", SCRIPT_DIR / "blob-mode-state.py")
transition = import_helper("attestation_blob_transition_runner", SCRIPT_DIR / "blob-transition-runner.py")


def require_directory(path: pathlib.Path, expected_mode: int | None = None, create: bool = False) -> None:
    created = False
    try:
        info = path.lstat()
    except FileNotFoundError:
        if not create:
            reject("path-unavailable")
        try:
            path.mkdir(mode=expected_mode if expected_mode is not None else 0o700)
            os.chown(path, ROOT_UID, 0)
            if expected_mode is not None:
                os.chmod(path, expected_mode)
            info = path.lstat()
            created = True
        except OSError:
            reject("path-create-failed")
    except OSError:
        reject("path-unavailable")
    if (
        stat.S_ISLNK(info.st_mode)
        or not stat.S_ISDIR(info.st_mode)
        or info.st_uid != ROOT_UID
        or info.st_mode & 0o022
        or (expected_mode is not None and stat.S_IMODE(info.st_mode) != expected_mode)
    ):
        reject("path-permissions")
    if created:
        sync_directory(path.parent)


def require_production_scope() -> None:
    if TEST_MODE:
        return
    try:
        checkout = CHECKOUT.resolve(strict=True)
        cwd = pathlib.Path.cwd().resolve(strict=True)
    except OSError:
        reject("production-path-invalid")
    if (
        os.geteuid() != 0
        or checkout != pathlib.Path("/opt/teagram")
        or cwd != checkout
        or STATE_DIR != checkout / ".state" / "blob-mode"
        or REPORT_ROOT != pathlib.Path("/var/lib/teagram-blob-mode-reports")
        or ARCHIVE_ROOT != pathlib.Path("/var/lib/teagram-blob-mode-archives")
        or LOCK_PATH != pathlib.Path("/tmp/teagram-deploy.lock")
    ):
        reject("production-path-invalid")


def check_path_boundaries(state_dir: pathlib.Path, report_root: pathlib.Path, archive_path: pathlib.Path) -> None:
    try:
        state = state_dir.resolve(strict=True)
        reports = report_root.resolve(strict=True)
        archive = archive_path.resolve(strict=False)
        root = pathlib.Path("/root").resolve(strict=True)
    except OSError:
        reject("path-unavailable")
    if (
        reports.is_relative_to(root)
        or reports.is_relative_to(state)
        or state.is_relative_to(reports)
        or archive.is_relative_to(root)
        or archive.is_relative_to(state)
        or archive.is_relative_to(reports)
    ):
        reject("archive-path-invalid")


def hash_bytes(content: bytes) -> str:
    return hashlib.sha256(content).hexdigest()


def write_private_file(path: pathlib.Path, content: bytes) -> None:
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW
    try:
        fd = os.open(path, flags, 0o600)
    except FileExistsError:
        reject("attestation-output-exists")
    except OSError:
        reject("attestation-output-write")
    try:
        with os.fdopen(fd, "wb") as output:
            output.write(content)
            output.flush()
            os.fchown(output.fileno(), ROOT_UID, 0)
            os.fchmod(output.fileno(), 0o600)
            os.fsync(output.fileno())
        sync_directory(path.parent)
    except (OSError, AttestationReject) as error:
        try:
            path.unlink()
            sync_directory(path.parent)
        except (OSError, AttestationReject):
            pass
        if isinstance(error, AttestationReject):
            raise
        reject("attestation-output-write")


def sync_directory(path: pathlib.Path) -> None:
    try:
        fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    except OSError:
        reject("attestation-output-sync")


def recovery_marker_path() -> pathlib.Path:
    return ARCHIVE_ROOT / RECOVERY_MARKER_NAME


def _private_regular_info(directory_fd: int, name: str) -> os.stat_result:
    try:
        info = os.stat(name, dir_fd=directory_fd, follow_symlinks=False)
    except OSError:
        reject("attestation-recovery-failed")
    if (
        not stat.S_ISREG(info.st_mode)
        or info.st_uid != ROOT_UID
        or stat.S_IMODE(info.st_mode) != 0o600
    ):
        reject("attestation-recovery-failed")
    return info


def _read_private_at(directory_fd: int, name: str, maximum: int = 1_048_576) -> bytes:
    _private_regular_info(directory_fd, name)
    try:
        fd = os.open(name, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW, dir_fd=directory_fd)
    except OSError:
        reject("attestation-recovery-failed")
    try:
        info = os.fstat(fd)
        if (
            not stat.S_ISREG(info.st_mode)
            or info.st_uid != ROOT_UID
            or stat.S_IMODE(info.st_mode) != 0o600
            or info.st_size > maximum
        ):
            reject("attestation-recovery-failed")
        chunks = []
        total = 0
        while True:
            block = os.read(fd, min(65536, maximum + 1 - total))
            if not block:
                break
            chunks.append(block)
            total += len(block)
            if total > maximum:
                reject("attestation-recovery-failed")
        return b"".join(chunks)
    except OSError:
        reject("attestation-recovery-failed")
    finally:
        os.close(fd)


def _unlink_private_at(directory_fd: int, name: str) -> None:
    try:
        os.stat(name, dir_fd=directory_fd, follow_symlinks=False)
    except FileNotFoundError:
        return
    except OSError:
        reject("attestation-recovery-failed")
    _private_regular_info(directory_fd, name)
    try:
        os.unlink(name, dir_fd=directory_fd)
        os.fsync(directory_fd)
    except OSError:
        reject("attestation-recovery-failed")


def _remove_private_tree_at(parent_fd: int, name: str) -> None:
    try:
        info = os.stat(name, dir_fd=parent_fd, follow_symlinks=False)
    except FileNotFoundError:
        return
    except OSError:
        reject("attestation-recovery-failed")
    if stat.S_ISREG(info.st_mode):
        if info.st_uid != ROOT_UID or stat.S_IMODE(info.st_mode) != 0o600:
            reject("attestation-recovery-failed")
        try:
            os.unlink(name, dir_fd=parent_fd)
            os.fsync(parent_fd)
        except OSError:
            reject("attestation-recovery-failed")
        return
    if (
        not stat.S_ISDIR(info.st_mode)
        or info.st_uid != ROOT_UID
        or stat.S_IMODE(info.st_mode) != 0o700
    ):
        reject("attestation-recovery-failed")
    try:
        child_fd = os.open(
            name,
            os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW,
            dir_fd=parent_fd,
        )
    except OSError:
        reject("attestation-recovery-failed")
    try:
        opened = os.fstat(child_fd)
        if (opened.st_dev, opened.st_ino) != (info.st_dev, info.st_ino):
            reject("attestation-recovery-failed")
        try:
            names = sorted(os.listdir(child_fd))
        except OSError:
            reject("attestation-recovery-failed")
        for child_name in names:
            if child_name in (".", "..") or "/" in child_name or "\\" in child_name:
                reject("attestation-recovery-failed")
            _remove_private_tree_at(child_fd, child_name)
        try:
            os.fsync(child_fd)
        except OSError:
            reject("attestation-recovery-failed")
    finally:
        try:
            os.close(child_fd)
        except OSError:
            reject("attestation-recovery-failed")
    try:
        os.rmdir(name, dir_fd=parent_fd)
        os.fsync(parent_fd)
    except OSError:
        reject("attestation-recovery-failed")


def _read_recovery_marker(archive_root_fd: int) -> tuple[str, str]:
    try:
        content = _read_private_at(archive_root_fd, RECOVERY_MARKER_NAME, maximum=4096)
        document = json.loads(content.decode("ascii"))
    except (AttestationReject, UnicodeDecodeError, json.JSONDecodeError):
        reject("attestation-recovery-marker-invalid")
    phase = "publishing"
    if (
        not isinstance(document, dict)
        or not isinstance(document.get("head_bytes_sha256"), str)
        or re.fullmatch(r"[0-9a-f]{64}", document["head_bytes_sha256"]) is None
    ):
        reject("attestation-recovery-marker-invalid")
    if document.get("schema") == LEGACY_RECOVERY_SCHEMA:
        if set(document) != {"schema", "head_bytes_sha256"}:
            reject("attestation-recovery-marker-invalid")
    elif document.get("schema") == RECOVERY_SCHEMA:
        if set(document) != {"schema", "head_bytes_sha256", "phase"}:
            reject("attestation-recovery-marker-invalid")
        phase_value = document.get("phase")
        if not isinstance(phase_value, str) or phase_value not in ("publishing", "committed"):
            reject("attestation-recovery-marker-invalid")
        phase = phase_value
    else:
        reject("attestation-recovery-marker-invalid")
    return document["head_bytes_sha256"], phase


def _remove_recovery_marker_temporaries(archive_root_fd: int) -> None:
    try:
        names = sorted(os.listdir(archive_root_fd))
    except OSError:
        reject("attestation-recovery-failed")
    for name in names:
        if re.fullmatch(re.escape(RECOVERY_MARKER_TEMP_PREFIX) + r"[0-9a-f]{16}", name):
            _unlink_private_at(archive_root_fd, name)


def _remove_attestation_temporaries(report_root_fd: int) -> None:
    try:
        names = sorted(os.listdir(report_root_fd))
    except OSError:
        reject("attestation-recovery-failed")
    for name in names:
        if re.fullmatch(re.escape(ATTESTATION_TEMP_PREFIX) + r"[0-9a-f]{16}", name):
            _unlink_private_at(report_root_fd, name)


def _remove_incomplete_publication(head_sha256: str, archive_root_fd: int) -> None:
    archive_name = f"attestation-{head_sha256}"
    _remove_private_tree_at(archive_root_fd, archive_name)
    try:
        report_fd = os.open(
            REPORT_ROOT,
            os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW,
        )
    except OSError:
        reject("attestation-recovery-failed")
    try:
        for name in (ARCHIVE_MANIFEST_NAME, CENSUS_MANIFEST_NAME):
            _unlink_private_at(report_fd, name)
        _remove_attestation_temporaries(report_fd)
        try:
            names = sorted(os.listdir(report_fd))
        except OSError:
            reject("attestation-recovery-failed")
        for name in names:
            match = re.fullmatch(re.escape(ATTESTATION_PREFIX) + r"([0-9a-f]{64})\.json", name)
            if match is None:
                continue
            content = _read_private_at(report_fd, name)
            try:
                document = json.loads(content.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError):
                reject("attestation-recovery-failed")
            if (
                not isinstance(document, dict)
                or document.get("schema") != ATTESTATION_SCHEMA
                or document.get("head_bytes_sha256") != head_sha256
                or hash_bytes(content) != match.group(1)
            ):
                reject("attestation-recovery-failed")
            _unlink_private_at(report_fd, name)
    finally:
        try:
            os.close(report_fd)
        except OSError:
            reject("attestation-recovery-failed")


def _remove_recovery_marker(archive_root_fd: int) -> None:
    _unlink_private_at(archive_root_fd, RECOVERY_MARKER_NAME)


def _recover_incomplete_publication() -> bool:
    try:
        archive_root_info = ARCHIVE_ROOT.lstat()
    except FileNotFoundError:
        return False
    except OSError:
        reject("archive-path-invalid")
    if (
        not stat.S_ISDIR(archive_root_info.st_mode)
        or stat.S_ISLNK(archive_root_info.st_mode)
        or archive_root_info.st_uid != ROOT_UID
        or stat.S_IMODE(archive_root_info.st_mode) != 0o700
    ):
        reject("path-permissions")
    try:
        archive_root_fd = os.open(
            ARCHIVE_ROOT,
            os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW,
        )
    except OSError:
        reject("archive-path-invalid")
    try:
        _remove_recovery_marker_temporaries(archive_root_fd)
        try:
            os.stat(RECOVERY_MARKER_NAME, dir_fd=archive_root_fd, follow_symlinks=False)
        except FileNotFoundError:
            return False
        except OSError:
            reject("attestation-recovery-marker-invalid")
        head_sha256, phase = _read_recovery_marker(archive_root_fd)
        if phase == "committed":
            try:
                report_fd = os.open(
                    REPORT_ROOT,
                    os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW,
                )
            except OSError:
                reject("attestation-recovery-failed")
            try:
                _remove_attestation_temporaries(report_fd)
            finally:
                try:
                    os.close(report_fd)
                except OSError:
                    reject("attestation-recovery-failed")
            _remove_recovery_marker(archive_root_fd)
            return False
        try:
            mode.current_authority(STATE_DIR, REPORT_ROOT)
        except mode.Reject:
            _remove_incomplete_publication(head_sha256, archive_root_fd)
            _remove_recovery_marker(archive_root_fd)
            return False
        try:
            report_fd = os.open(
                REPORT_ROOT,
                os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW,
            )
        except OSError:
            reject("attestation-recovery-failed")
        try:
            _remove_attestation_temporaries(report_fd)
        finally:
            os.close(report_fd)
        _remove_recovery_marker(archive_root_fd)
        return True
    finally:
        try:
            os.close(archive_root_fd)
        except OSError:
            reject("attestation-recovery-failed")


def _publish_recovery_marker(head_sha256: str) -> None:
    require_directory(ARCHIVE_ROOT, expected_mode=0o700, create=True)
    marker_path = recovery_marker_path()
    temporary = ARCHIVE_ROOT / f"{RECOVERY_MARKER_TEMP_PREFIX}{os.urandom(8).hex()}"
    content = (
        json.dumps(
            {"schema": RECOVERY_SCHEMA, "head_bytes_sha256": head_sha256, "phase": "publishing"},
            sort_keys=True,
            separators=(",", ":"),
        )
        + "\n"
    ).encode("ascii")
    write_private_file(temporary, content)
    try:
        os.link(temporary, marker_path, follow_symlinks=False)
        sync_directory(ARCHIVE_ROOT)
        temporary.unlink()
        sync_directory(ARCHIVE_ROOT)
    except FileExistsError:
        reject("attestation-recovery-marker-exists")
    except OSError:
        reject("attestation-recovery-marker-publish")


def _mark_recovery_marker_committed(head_sha256: str) -> None:
    try:
        archive_root_fd = os.open(
            ARCHIVE_ROOT,
            os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW,
        )
    except OSError:
        reject("attestation-recovery-failed")
    temporary = ARCHIVE_ROOT / f"{RECOVERY_MARKER_TEMP_PREFIX}{os.urandom(8).hex()}"
    marker_path = recovery_marker_path()
    try:
        marker_head_sha256, phase = _read_recovery_marker(archive_root_fd)
        if marker_head_sha256 != head_sha256 or phase != "publishing":
            reject("attestation-recovery-marker-invalid")
        content = (
            json.dumps(
                {"schema": RECOVERY_SCHEMA, "head_bytes_sha256": head_sha256, "phase": "committed"},
                sort_keys=True,
                separators=(",", ":"),
            )
            + "\n"
        ).encode("ascii")
        write_private_file(temporary, content)
        try:
            os.replace(temporary, marker_path)
            sync_directory(ARCHIVE_ROOT)
        except OSError:
            reject("attestation-recovery-marker-publish")
    finally:
        try:
            if temporary.exists():
                temporary.unlink()
        except OSError:
            reject("attestation-recovery-marker-publish")
        try:
            os.close(archive_root_fd)
        except OSError:
            reject("attestation-recovery-failed")


def _rollback_incomplete_publication(head_sha256: str) -> None:
    try:
        archive_root_info = ARCHIVE_ROOT.lstat()
    except FileNotFoundError:
        return
    except OSError:
        reject("attestation-recovery-failed")
    if (
        not stat.S_ISDIR(archive_root_info.st_mode)
        or stat.S_ISLNK(archive_root_info.st_mode)
        or archive_root_info.st_uid != ROOT_UID
        or stat.S_IMODE(archive_root_info.st_mode) != 0o700
    ):
        reject("attestation-recovery-failed")
    try:
        archive_root_fd = os.open(
            ARCHIVE_ROOT,
            os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW,
        )
    except OSError:
        reject("attestation-recovery-failed")
    try:
        try:
            os.stat(RECOVERY_MARKER_NAME, dir_fd=archive_root_fd, follow_symlinks=False)
        except FileNotFoundError:
            _remove_recovery_marker_temporaries(archive_root_fd)
            return
        except OSError:
            reject("attestation-recovery-failed")
        marker_head_sha256, phase = _read_recovery_marker(archive_root_fd)
        if marker_head_sha256 != head_sha256:
            reject("attestation-recovery-marker-invalid")
        if phase == "committed":
            _remove_recovery_marker_temporaries(archive_root_fd)
            return
        _remove_incomplete_publication(head_sha256, archive_root_fd)
        _remove_recovery_marker(archive_root_fd)
        _remove_recovery_marker_temporaries(archive_root_fd)
    finally:
        try:
            os.close(archive_root_fd)
        except OSError:
            reject("attestation-recovery-failed")


def _reject_existing_publication_outputs(archive_path: pathlib.Path) -> None:
    for name in (ARCHIVE_MANIFEST_NAME, CENSUS_MANIFEST_NAME):
        try:
            (REPORT_ROOT / name).lstat()
        except FileNotFoundError:
            pass
        except OSError:
            reject("attestation-output-exists")
        else:
            reject("attestation-output-exists")
    try:
        archive_path.lstat()
    except FileNotFoundError:
        pass
    except OSError:
        reject("archive-already-exists")
    else:
        reject("archive-already-exists")
    try:
        report_fd = os.open(
            REPORT_ROOT,
            os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW,
        )
    except OSError:
        reject("path-unavailable")
    try:
        names = os.listdir(report_fd)
    except OSError:
        reject("path-unavailable")
    finally:
        os.close(report_fd)
    if any(
        name.startswith(ATTESTATION_PREFIX)
        or name.startswith(ATTESTATION_TEMP_PREFIX)
        for name in names
    ):
        reject("attestation-output-exists")


def valid_blob_key(key: str) -> bool:
    file_match = re.fullmatch(r"([0-9a-f]{2})/([1-9][0-9]*)", key)
    if file_match is not None:
        try:
            file_id = int(file_match.group(2))
        except ValueError:
            return False
        return file_id <= 9_223_372_036_854_775_807 and file_match.group(1) == f"{file_id & 0xff:02x}"
    if re.fullmatch(r"parts/[0-9a-f]{32}", key) is not None:
        return True
    part_match = re.fullmatch(r"parts/([0-9a-f]{2})/([0-9a-f]{30})", key)
    return part_match is not None and part_match.group(1) == part_match.group(2)[-2:]


def parse_size(value: str, reason: str) -> int:
    if re.fullmatch(r"0|[1-9][0-9]*", value) is None:
        reject(reason)
    result = int(value)
    if result > 9_223_372_036_854_775_807:
        reject(reason)
    return result


def parse_s3_manifest(content: bytes) -> tuple[list[dict[str, object]], int]:
    try:
        text = content.decode("ascii")
    except UnicodeDecodeError:
        reject("census-manifest-invalid")
    if text and not text.endswith("\n"):
        reject("census-manifest-invalid")
    objects = []
    previous_key = ""
    byte_total = 0
    for line in text.splitlines():
        columns = line.split("\t")
        if len(columns) != 3:
            reject("census-manifest-invalid")
        key, size_text, checksum = columns
        if (
            not valid_blob_key(key)
            or key <= previous_key
            or re.fullmatch(r"[0-9a-f]{64}", checksum) is None
        ):
            reject("census-manifest-invalid")
        size = parse_size(size_text, "census-manifest-invalid")
        if byte_total > 9_223_372_036_854_775_807 - size:
            reject("census-manifest-invalid")
        byte_total += size
        objects.append({"key": key, "size": size, "sha256": checksum})
        previous_key = key
    return objects, byte_total


def parse_references(content: bytes) -> list[dict[str, object]]:
    try:
        text = content.decode("ascii")
    except UnicodeDecodeError:
        reject("census-references-invalid")
    if text and not text.endswith("\n"):
        reject("census-references-invalid")
    references = []
    seen: set[str] = set()
    for line in text.splitlines():
        columns = line.split("\t")
        if len(columns) != 2:
            reject("census-references-invalid")
        key, size_text = columns
        if not valid_blob_key(key) or key in seen:
            reject("census-references-invalid")
        seen.add(key)
        references.append({"key": key, "logical_bytes": parse_size(size_text, "census-references-invalid")})
    references.sort(key=lambda item: str(item["key"]))
    return references


def validate_live_census(
    s3_manifest: bytes,
    reference_rows: bytes,
) -> tuple[bytes, int, int]:
    objects, byte_total = parse_s3_manifest(s3_manifest)
    references = parse_references(reference_rows)
    by_key = {str(item["key"]): item for item in objects}
    for reference in references:
        key = str(reference["key"])
        item = by_key.get(key)
        if item is None:
            reject("census-reference-missing")
        if item["size"] != reference["logical_bytes"]:
            reject("census-reference-size-mismatch")
    document = {
        "schema": CENSUS_SCHEMA,
        "object_count": len(objects),
        "byte_total": byte_total,
        "objects": objects,
        "references": references,
    }
    content = (json.dumps(document, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
    return content, len(objects), byte_total


def run_compose(command: list[str], environment: dict[str, str]) -> bytes:
    try:
        full_command = transition.compose_command(command, environment)
        result = subprocess.run(
            full_command,
            cwd=CHECKOUT,
            env=environment,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
    except OSError:
        reject("live-census-unavailable")
    if result.returncode != 0:
        reject("live-census-rejected")
    return result.stdout


def validate_composed_authority(record: dict[str, object], environment: dict[str, str]) -> None:
    raw = run_compose(
        ["docker", "compose", "--profile", "migration", "config", "--format", "json"],
        environment,
    )
    try:
        rendered = json.loads(raw.decode("utf-8"), object_pairs_hook=mode.strict_pairs)
    except (UnicodeDecodeError, json.JSONDecodeError, mode.Reject):
        reject("live-compose-invalid")
    try:
        inventory = mode.compose_inventory(rendered, CHECKOUT)
        mode.assert_compose_matches(
            inventory,
            record,
            os.path.realpath(STATE_DIR),
            CHECKOUT / "docker-compose.override.yml",
        )
    except mode.Reject as error:
        reject(str(error))
    services = rendered.get("services") if isinstance(rendered, dict) else None
    migration = services.get("blob-migrate") if isinstance(services, dict) else None
    profiles = migration.get("profiles") if isinstance(migration, dict) else None
    if not isinstance(profiles, list) or "migration" not in profiles:
        reject("live-census-service-invalid")
    try:
        migration_backend = mode.backend_from_values(
            mode.raw_environment_map(migration.get("environment", {}), True)
        )
    except mode.Reject as error:
        reject(str(error))
    if migration_backend != record.get("backend"):
        reject("live-census-backend-mismatch")
    encrypt_migration = services.get("blob-encrypt-migrate") if isinstance(services, dict) else None
    raw_volumes = encrypt_migration.get("volumes", []) if isinstance(encrypt_migration, dict) else []
    report_mounts = [
        mount for mount in raw_volumes
        if isinstance(mount, dict) and mount.get("target") == mode.REPORT_TARGET
    ] if isinstance(raw_volumes, list) else []
    if (
        len(report_mounts) != 1
        or report_mounts[0].get("type") != "bind"
        or report_mounts[0].get("source") != str(REPORT_ROOT)
    ):
        reject("live-report-root-mismatch")


def capture_live_census(record: dict[str, object]) -> tuple[bytes, bytes]:
    try:
        env_files = transition.rustfs_compose_environment_files(CHECKOUT)
        if len(env_files) != 2 or env_files[0] != str(CHECKOUT / ".env"):
            reject("live-census-credentials-invalid")
        environment = transition.compose_environment_for_files(
            CHECKOUT,
            ["docker-compose.yml", "docker-compose.override.yml"],
            include_rustfs_credentials=True,
        )
    except transition.TransitionReject as error:
        reject(str(error))
    validate_composed_authority(record, environment)
    s3_manifest = run_compose(
        [
            "docker", "compose", "--profile", "migration", "run", "--rm", "--no-deps",
            "blob-migrate", "--direction", "s3-census", "--manifest", "stdout",
        ],
        environment,
    )
    reference_rows = run_compose(
        [
            "docker", "compose", "--profile", "migration", "exec", "-T", "postgres", "psql",
            "-X", "-qAt", "-F", "\t", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "teagram",
            "-c", REFERENCE_QUERY,
        ],
        environment,
    )
    return s3_manifest, reference_rows


def private_source_stat(info: os.stat_result, kind: int) -> None:
    if (
        stat.S_IFMT(info.st_mode) != kind
        or info.st_uid != ROOT_UID
        or info.st_mode & 0o022
    ):
        reject("archive-source-unsafe")


def close_descriptors(*descriptors: int) -> bool:
    failed = False
    for descriptor in descriptors:
        if descriptor < 0:
            continue
        try:
            os.close(descriptor)
        except OSError:
            failed = True
    return not failed


def hash_open_file(directory_fd: int, name: str) -> tuple[str, int]:
    flags = os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW
    try:
        fd = os.open(name, flags, dir_fd=directory_fd)
    except OSError:
        reject("archive-copy-failed")
    digest = hashlib.sha256()
    size = 0
    try:
        info = os.fstat(fd)
        private_source_stat(info, stat.S_IFREG)
        while True:
            block = os.read(fd, 1024 * 1024)
            if not block:
                break
            digest.update(block)
            size += len(block)
    except OSError:
        reject("archive-copy-failed")
    finally:
        os.close(fd)
    return digest.hexdigest(), size


def copy_tree_contents(
    source_fd: int,
    destination_fd: int,
    relative_root: str,
    entries: list[dict[str, object]],
) -> None:
    try:
        names = sorted(os.listdir(source_fd))
    except OSError:
        reject("archive-source-unavailable")
    for name in names:
        if name in (".", "..") or "/" in name or "\\" in name:
            reject("archive-source-unsafe")
        try:
            source_info = os.stat(name, dir_fd=source_fd, follow_symlinks=False)
        except OSError:
            reject("archive-source-unavailable")
        relative = f"{relative_root}/{name}"
        if stat.S_ISDIR(source_info.st_mode):
            private_source_stat(source_info, stat.S_IFDIR)
            source_child = -1
            destination_child = -1
            try:
                os.mkdir(name, mode=0o700, dir_fd=destination_fd)
                source_child = os.open(
                    name, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW, dir_fd=source_fd
                )
                destination_child = os.open(
                    name, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW, dir_fd=destination_fd
                )
                os.fchown(destination_child, ROOT_UID, 0)
                os.fchmod(destination_child, 0o700)
            except OSError:
                close_descriptors(source_child, destination_child)
                reject("archive-copy-failed")
            try:
                if (os.fstat(source_child).st_dev, os.fstat(source_child).st_ino) != (source_info.st_dev, source_info.st_ino):
                    reject("archive-source-changed")
                copy_tree_contents(source_child, destination_child, relative, entries)
                os.fsync(destination_child)
            finally:
                if not close_descriptors(source_child, destination_child):
                    reject("archive-copy-failed")
        elif stat.S_ISREG(source_info.st_mode):
            private_source_stat(source_info, stat.S_IFREG)
            source_file = -1
            destination_file = -1
            try:
                source_file = os.open(
                    name, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW, dir_fd=source_fd
                )
                source_open_info = os.fstat(source_file)
                if (source_open_info.st_dev, source_open_info.st_ino) != (source_info.st_dev, source_info.st_ino):
                    reject("archive-source-changed")
                destination_file = os.open(
                    name,
                    os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW,
                    0o600,
                    dir_fd=destination_fd,
                )
            except AttestationReject:
                close_descriptors(source_file, destination_file)
                raise
            except OSError:
                close_descriptors(source_file, destination_file)
                reject("archive-copy-failed")
            digest = hashlib.sha256()
            size = 0
            try:
                while block := os.read(source_file, 1024 * 1024):
                    digest.update(block)
                    size += len(block)
                    remaining = memoryview(block)
                    while remaining:
                        written = os.write(destination_file, remaining)
                        if written <= 0:
                            reject("archive-copy-failed")
                        remaining = remaining[written:]
                os.fchown(destination_file, ROOT_UID, 0)
                os.fchmod(destination_file, 0o600)
                os.fsync(destination_file)
            except OSError:
                reject("archive-copy-failed")
            finally:
                if not close_descriptors(source_file, destination_file):
                    reject("archive-copy-failed")
            try:
                source_after = os.stat(name, dir_fd=source_fd, follow_symlinks=False)
                if (
                    source_after.st_dev != source_info.st_dev
                    or source_after.st_ino != source_info.st_ino
                    or source_after.st_size != source_info.st_size
                    or source_after.st_mtime_ns != source_info.st_mtime_ns
                    or source_after.st_ctime_ns != source_info.st_ctime_ns
                ):
                    reject("archive-source-changed")
                source_hash, source_size = hash_open_file(source_fd, name)
                destination_hash, destination_size = hash_open_file(destination_fd, name)
                if (
                    source_hash != digest.hexdigest()
                    or destination_hash != source_hash
                    or source_size != size
                    or destination_size != size
                ):
                    reject("archive-copy-mismatch")
                entries.append({"path": relative, "size": size, "sha256": destination_hash})
            except OSError:
                reject("archive-copy-failed")
        else:
            reject("archive-source-unsafe")
    try:
        os.fsync(destination_fd)
    except OSError:
        reject("archive-copy-sync")


def copy_authority_tree(source: pathlib.Path, destination: pathlib.Path, relative_root: str, entries: list[dict[str, object]]) -> None:
    source_fd = -1
    destination_fd = -1
    try:
        info = source.lstat()
        private_source_stat(info, stat.S_IFDIR)
        source_fd = os.open(source, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW)
        destination.mkdir(mode=0o700)
        destination_fd = os.open(destination, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW)
        os.fchown(destination_fd, ROOT_UID, 0)
        os.fchmod(destination_fd, 0o700)
    except OSError:
        close_descriptors(source_fd, destination_fd)
        reject("archive-copy-failed")
    try:
        if (os.fstat(source_fd).st_dev, os.fstat(source_fd).st_ino) != (info.st_dev, info.st_ino):
            reject("archive-source-changed")
        copy_tree_contents(source_fd, destination_fd, relative_root, entries)
        os.fsync(destination_fd)
    finally:
        if not close_descriptors(source_fd, destination_fd):
            reject("archive-copy-failed")


def create_archive(
    archive_path: pathlib.Path,
    state_dir: pathlib.Path,
    report_root: pathlib.Path,
) -> tuple[bytes, str]:
    require_directory(ARCHIVE_ROOT, expected_mode=0o700, create=True)
    try:
        archive_path.mkdir(mode=0o700)
        os.chown(archive_path, ROOT_UID, 0)
        os.chmod(archive_path, 0o700)
    except FileExistsError:
        reject("archive-already-exists")
    except OSError:
        reject("archive-create-failed")
    sync_directory(ARCHIVE_ROOT)
    entries: list[dict[str, object]] = []
    copy_authority_tree(state_dir, archive_path / "authority", "authority", entries)
    copy_authority_tree(report_root, archive_path / "reports", "reports", entries)
    document = {"schema": ARCHIVE_SCHEMA, "files": sorted(entries, key=lambda item: str(item["path"]))}
    content = (json.dumps(document, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
    write_private_file(archive_path / "archive-manifest.json", content)
    sync_directory(archive_path)
    return content, hash_bytes(content)


def publish_attestation(report_root: pathlib.Path, document: dict[str, object]) -> tuple[str, str]:
    content = (json.dumps(document, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
    attestation_sha256 = hash_bytes(content)
    destination = report_root / f"{ATTESTATION_PREFIX}{attestation_sha256}.json"
    temporary = report_root / f".tmp-report-loss-attestation-{os.urandom(8).hex()}"
    write_private_file(temporary, content)
    try:
        os.link(temporary, destination, follow_symlinks=False)
        sync_directory(report_root)
        temporary.unlink()
        sync_directory(report_root)
    except FileExistsError:
        reject("attestation-output-exists")
    except OSError:
        reject("attestation-output-publish")
    return str(destination), attestation_sha256


def run_attestation() -> dict[str, object]:
    require_production_scope()
    try:
        transition.acquire_lock(LOCK_PATH)
        mode.require_runner_lock(LOCK_PATH)
    except transition.TransitionReject as error:
        reject(str(error))
    except mode.Reject as error:
        reject(str(error))

    require_directory(STATE_DIR)
    require_directory(REPORT_ROOT, expected_mode=0o700)
    if _recover_incomplete_publication():
        reject("authority-already-valid")
    try:
        mode.current_authority(STATE_DIR, REPORT_ROOT)
    except mode.Reject as error:
        if str(error) != "report-unavailable":
            reject(str(error))
    else:
        reject("authority-already-valid")

    try:
        deployment_head, missing_count = mode.current_deployment_authority(STATE_DIR, REPORT_ROOT)
        records, head_bytes, mode_bytes, parsed_missing_count = mode._read_authority(
            STATE_DIR, REPORT_ROOT, allow_missing_reports=True
        )
    except mode.Reject as error:
        reject(str(error))
    if (
        deployment_head != records[-1]
        or head_bytes != mode_bytes
        or missing_count != parsed_missing_count
        or missing_count < 1
    ):
        reject("deployment-authority-invalid")
    if deployment_head.get("outcome") != "s3-accepted":
        reject("head-not-s3-accepted")
    backend = deployment_head.get("backend")
    if not isinstance(backend, dict) or backend.get("kind") != "s3":
        reject("head-backend-not-s3")

    missing_ids = []
    for index, record in enumerate(records):
        previous = records[index - 1] if index > 0 else None
        if not mode.validate_report(REPORT_ROOT, record, previous, allow_missing=True):
            missing_ids.append(record["transition_id"])
    if len(missing_ids) != missing_count or not missing_ids:
        reject("missing-report-set-changed")

    head_sha256 = hash_bytes(head_bytes)
    archive_path = ARCHIVE_ROOT / f"attestation-{head_sha256}"
    check_path_boundaries(STATE_DIR, REPORT_ROOT, archive_path)
    _reject_existing_publication_outputs(archive_path)

    try:
        s3_manifest, reference_rows = capture_live_census(deployment_head)
    except AttestationReject:
        raise
    except Exception:
        reject("live-census-rejected")
    census_content, object_count, byte_total = validate_live_census(s3_manifest, reference_rows)
    census_sha256 = hash_bytes(census_content)

    try:
        _publish_recovery_marker(head_sha256)
        archive_content, archive_sha256 = create_archive(archive_path, STATE_DIR, REPORT_ROOT)
        write_private_file(REPORT_ROOT / ARCHIVE_MANIFEST_NAME, archive_content)
        write_private_file(REPORT_ROOT / CENSUS_MANIFEST_NAME, census_content)
        journal_prefix_sha256 = hash_bytes(b"".join(
            (STATE_DIR / "journal" / f"{generation:010d}.json").read_bytes()
            for generation in range(1, len(records) + 1)
        ))
        document = {
            "schema": ATTESTATION_SCHEMA,
            "head_generation": len(records),
            "head_transition_id": deployment_head["transition_id"],
            "head_bytes_sha256": head_sha256,
            "journal_prefix_sha256": journal_prefix_sha256,
            "missing_transition_ids": sorted(missing_ids),
            "reason": "MAIN-2610",
            "archive_manifest_sha256": archive_sha256,
            "census_manifest_sha256": census_sha256,
            "object_count": object_count,
            "byte_total": byte_total,
        }
        attestation_path, attestation_sha256 = publish_attestation(REPORT_ROOT, document)
        try:
            mode.current_authority(STATE_DIR, REPORT_ROOT)
        except mode.Reject as error:
            reject(f"post-attestation-{error}")
        _mark_recovery_marker_committed(head_sha256)
    except Exception:
        _rollback_incomplete_publication(head_sha256)
        raise

    archive_root_fd = os.open(
        ARCHIVE_ROOT,
        os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW,
    )
    try:
        _remove_recovery_marker(archive_root_fd)
    finally:
        os.close(archive_root_fd)
    return {
        "attestation_path": attestation_path,
        "attestation_sha256": attestation_sha256,
        "head_transition_id": deployment_head["transition_id"],
        "journal_prefix_sha256": journal_prefix_sha256,
        "missing_report_count": missing_count,
        "object_count": object_count,
        "byte_total": byte_total,
        "archive_path": str(archive_path),
        "archive_manifest_sha256": archive_sha256,
        "census_manifest_sha256": census_sha256,
    }


def main() -> int:
    if len(sys.argv) != 1:
        print("usage: attest-blob-mode-reports.py", file=sys.stderr)
        return 64
    try:
        result = run_attestation()
    except AttestationReject as error:
        print(f"blob report attestation rejected: {error}", file=sys.stderr)
        return 1
    print(
        "attestation=pass "
        f"head_transition_id={result['head_transition_id']} "
        f"journal_prefix_sha256={result['journal_prefix_sha256']} "
        f"missing_reports={result['missing_report_count']} "
        f"objects={result['object_count']} bytes={result['byte_total']} "
        f"archive={result['archive_path']} "
        f"archive_manifest_sha256={result['archive_manifest_sha256']} "
        f"census_manifest_sha256={result['census_manifest_sha256']} "
        f"attestation_sha256={result['attestation_sha256']}"
    )
    return 0


if __name__ == "__main__":
    os.umask(0o077)
    raise SystemExit(main())
