#!/usr/bin/env python3
"""Read-only validator for a private RustFS transition evidence bundle."""

from __future__ import annotations

import base64
import copy
import datetime as dt
import hashlib
import json
import os
import re
import shutil
import stat
import subprocess
import sys
from pathlib import Path
from typing import Any


SCHEMA = "teagram.rustfs-transition-qualification/v3"
PINNED_RUSTFS_IMAGE = (
    "mirror.gcr.io/rustfs/rustfs:1.0.1@sha256:"
    "1803faef57627e2d9c2e7d89d655d712ddded5389040054987163043fecb6a3c"
)
MIGRATION_66 = "20261007000066"
MIGRATION_66_ATLAS_HASH = "h1:o3QLcFMfrTkdKsYDmgJFEfbaTSW2Zn+pTly5jHarasc="
MIGRATION_66_FILE_SHA256 = "842cd000072a6c1aa74c0d39a62f89497703ae5a79e045f2f6259b2157aba859"
ATLAS_SUM_60_66_SHA256 = "6a0d3f862d97f18227f492c9cd7fd8d7713744967459d35448cf75b3e32ae849"
MIGRATION_67 = "20261008000067"
MIGRATION_67_FILE = "20261008000067_secret_chat_party_date_idx.sql"
MIGRATION_67_ATLAS_HASH = "h1:Lux8heOMbxuuDRHoHwo61qwT/B+Nm05v6jFNbXvz2EE="
MIGRATION_67_FILE_SHA256 = "eb94b35a5303dd6ef3d22c8d3164b13284b9c7529071800af6592ff160573388"
ATLAS_SUM_60_67_SHA256 = "b2c094461a8224de2adde980a7c510e8254d5c0fae2d1e39a0dd9125cae8e9d9"
MIGRATION_68 = "20261008000068"
MIGRATION_68_FILE = "20261008000068_files_owner_ownership_backstop.sql"
MIGRATION_68_ATLAS_HASH = "h1:wpeh1DEL6OoF7lR2m5RPeMVAC/maP9l+mXGeHsTZnXc="
MIGRATION_68_FILE_SHA256 = "e4d3aed863bc6bec9feec53c89859ace763a3832a203fbbab8a7ba777b284ae0"
MIGRATION_69 = "20261008000069"
MIGRATION_69_FILE = "20261008000069_profile_photo_gallery.sql"
MIGRATION_69_ATLAS_HASH = "h1:gX6I/YcbJQsvERf2mEbtNolprFNg8fVzx/05XGa6ESM="
MIGRATION_69_FILE_SHA256 = "4972fad76892ac89bf8529b16b9ec1679773fd9257f037c621397f59383ca490"
ATLAS_SUM_60_69_SHA256 = "c54c4c43a1941519fb5ea7a62e56b5286853496c42d4238420bcf761f9577e77"
MIGRATION_70 = "20261008000070"
MIGRATION_70_FILE = "20261008000070_erasure_outbox_epoch_markers.sql"
MIGRATION_70_ATLAS_HASH = "h1:3ZPWNySt9YWg9s+xi+aQVAFrcSkeym+fGlL4PgkIayk="
MIGRATION_70_FILE_SHA256 = "cf7bc135c5df5a539cf5b77d76136ab321fc63e6d0a787566b68b8e052779a2c"
ATLAS_SUM_60_70_SHA256 = "e16da8e46119290cac52762235a47f47efd796ab2847db56a368a36d3b2ec608"
MIGRATIONS_60_62 = [
    "20261005000060",
    "20261005000061",
    "20261006000062",
]
MIGRATIONS_60_66 = MIGRATIONS_60_62 + [
    "20261006000063",
    "20261007000064",
    "20261007000065",
    MIGRATION_66,
]
MIGRATIONS_60_67 = MIGRATIONS_60_66 + [MIGRATION_67]
MIGRATIONS_60_69 = MIGRATIONS_60_67 + [MIGRATION_68, MIGRATION_69]
MIGRATIONS_60_70 = MIGRATIONS_60_69 + [MIGRATION_70]
MIGRATION_FILES_60_66 = [
    "20261005000060_file_media_metadata.sql",
    "20261005000061_validate_file_media_metadata.sql",
    "20261006000062_trusted_message_replies.sql",
    "20261006000063_dialog_pins.sql",
    "20261007000064_cloud_drafts.sql",
    "20261007000065_poll_description_entities.sql",
    "20261007000066_dialog_unread_marks.sql",
]
MIGRATION_FILES_60_67 = MIGRATION_FILES_60_66 + [MIGRATION_67_FILE]
MIGRATION_FILES_60_69 = MIGRATION_FILES_60_67 + [MIGRATION_68_FILE, MIGRATION_69_FILE]
MIGRATION_FILES_60_70 = MIGRATION_FILES_60_69 + [MIGRATION_70_FILE]
MIGRATION_SHA256_60_66 = {
    "20261005000060_file_media_metadata.sql": "5c5ee684f5ba218c5c4d9bc8f0a29788bf9ef62a7fd640d04fbf0a7568d220af",
    "20261005000061_validate_file_media_metadata.sql": "8263920473a6f48b4d0da35e2496d8464b27e5359fe4e383b961c246654773ab",
    "20261006000062_trusted_message_replies.sql": "1ec9f2f98ea2b0ef6e47f75b8f9ae1ec0a484de21baf1392dcbabc118be3bfdb",
    "20261006000063_dialog_pins.sql": "91d35b9751cae11e17c2e00c5461df8dc00cca9809f0a227ccedc7f1ac6e3cfb",
    "20261007000064_cloud_drafts.sql": "e5b13be532a549fe9c22d9cfe39ce97399f83242d0750e9f6fb3047e359aebf0",
    "20261007000065_poll_description_entities.sql": "ec8d86a1495cd6a1ec2e262d15fca65f3e6c693d95c1f1b6e7745c8b8a533547",
    "20261007000066_dialog_unread_marks.sql": MIGRATION_66_FILE_SHA256,
}
MIGRATION_SHA256_60_67 = {
    **MIGRATION_SHA256_60_66,
    MIGRATION_67_FILE: MIGRATION_67_FILE_SHA256,
}
MIGRATION_SHA256_60_69 = {
    **MIGRATION_SHA256_60_67,
    MIGRATION_68_FILE: MIGRATION_68_FILE_SHA256,
    MIGRATION_69_FILE: MIGRATION_69_FILE_SHA256,
}
MIGRATION_SHA256_60_70 = {
    **MIGRATION_SHA256_60_69,
    MIGRATION_70_FILE: MIGRATION_70_FILE_SHA256,
}
MIGRATION_ATLAS_PINS_60_66 = {
    "20261005000060_file_media_metadata.sql": "h1:pVa0QAbrHYJKCFIAetI1233DYfejdfRsgQKvH+VYNBw=",
    "20261005000061_validate_file_media_metadata.sql": "h1:JuiEs5kWKJjML/c08w1CySFUgtQVSON5BSsMqyooL5o=",
    "20261006000062_trusted_message_replies.sql": "h1:LndEQrLWR5dJx/H3QM3FY0eNcxm0GQqig3E8FCkKeSw=",
    "20261006000063_dialog_pins.sql": "h1:KsGc/MVs78pwnV2370VaxVWPGUAeI9AMLiFWQgu906Q=",
    "20261007000064_cloud_drafts.sql": "h1:HRrwny26zZtQBWOeQUcfp5rKuwAEIsNoKyILYCZTfzY=",
    "20261007000065_poll_description_entities.sql": "h1:UagmIV9R7m4NEH629GslmqXa+rWeJIOuwnM5AVc66vU=",
    "20261007000066_dialog_unread_marks.sql": MIGRATION_66_ATLAS_HASH,
}
MIGRATION_ATLAS_PINS_60_67 = {
    **MIGRATION_ATLAS_PINS_60_66,
    MIGRATION_67_FILE: MIGRATION_67_ATLAS_HASH,
}
MIGRATION_ATLAS_PINS_60_69 = {
    **MIGRATION_ATLAS_PINS_60_67,
    MIGRATION_68_FILE: MIGRATION_68_ATLAS_HASH,
    MIGRATION_69_FILE: MIGRATION_69_ATLAS_HASH,
}
MIGRATION_ATLAS_PINS_60_70 = {
    **MIGRATION_ATLAS_PINS_60_69,
    MIGRATION_70_FILE: MIGRATION_70_ATLAS_HASH,
}
RELEASES = {
    "60-66": {
        "atlas_sum_sha256": ATLAS_SUM_60_66_SHA256,
        "revisions": MIGRATIONS_60_66,
        "files": MIGRATION_FILES_60_66,
        "file_sha256": MIGRATION_SHA256_60_66,
        "atlas_pins": MIGRATION_ATLAS_PINS_60_66,
    },
    "60-67": {
        "atlas_sum_sha256": ATLAS_SUM_60_67_SHA256,
        "revisions": MIGRATIONS_60_67,
        "files": MIGRATION_FILES_60_67,
        "file_sha256": MIGRATION_SHA256_60_67,
        "atlas_pins": MIGRATION_ATLAS_PINS_60_67,
    },
    "60-69": {
        "atlas_sum_sha256": ATLAS_SUM_60_69_SHA256,
        "revisions": MIGRATIONS_60_69,
        "files": MIGRATION_FILES_60_69,
        "file_sha256": MIGRATION_SHA256_60_69,
        "atlas_pins": MIGRATION_ATLAS_PINS_60_69,
    },
    "60-70": {
        "atlas_sum_sha256": ATLAS_SUM_60_70_SHA256,
        "revisions": MIGRATIONS_60_70,
        "files": MIGRATION_FILES_60_70,
        "file_sha256": MIGRATION_SHA256_60_70,
        "atlas_pins": MIGRATION_ATLAS_PINS_60_70,
    },
}
SECRET_CHATS_INDEX_NAMES_60_67 = {
    "secret_chats_pkey",
    "secret_chats_admin_state_idx",
    "secret_chats_participant_state_idx",
    "secret_chats_admin_random_id_idx",
    "secret_chats_admin_date_idx",
    "secret_chats_participant_date_idx",
}
FILES_INDEX_NAMES_60_69 = {
    "files_pkey",
    "files_uploader_idx",
    "files_id_uploader_id_key",
}
INERT_SURFACES = {
    "user_photos",
    "profile_photo_state",
    "profile_upload_receipt",
    "profile_delete_operation",
}
INERT_SURFACES_QUERY_SHA256 = "2d0c108eb69b0cab431f01837a649e5e7f14d33483aae677be1032d5aa32cfe3"
R70_INERT_SURFACES = INERT_SURFACES | {
    "erasure_outbox",
    "erasure_epoch",
    "erasure_epoch_completion",
}
R70_INERT_SURFACES_QUERY_SHA256 = "ade88675d3c578cc42eea5ca7250a224c05cd68a3cf814c045dec488bc60cee5"
S3_ENV = {
    "TG_BLOB_S3_ENDPOINT": "http://rustfs:9000",
    "TG_BLOB_S3_BUCKET": "telegram",
    "TG_BLOB_S3_PREFIX": "telegramd/",
    "TG_BLOB_S3_REGION": "us-east-1",
    "TG_BLOB_S3_SECRET_ACCESS_KEY_FILE": "/run/secrets/telegramd-blob-secret-key",
    "TG_BLOB_S3_ALLOW_INSECURE_HTTP": "true",
}
S3_ENV_KEYS = set(S3_ENV) | {"TG_BLOB_S3_ACCESS_KEY_ID"}
RUSTFS_ENV_KEYS = [
    "RUSTFS_ROOT_ACCESS_KEY",
    "RUSTFS_ROOT_SECRET_KEY",
    "TG_BLOB_S3_ACCESS_KEY_ID",
    "TG_BLOB_S3_SECRET_ACCESS_KEY",
]
ADDED_SERVICES = {"rustfs", "rustfs-init", "blob-migrate", "blob-restore"}
ONE_SHOT_BASELINE_SERVICES = {"migrate"}
ADDED_SECRETS = {
    "rustfs_root_access_key",
    "rustfs_root_secret_key",
    "telegramd_blob_secret_key",
}
ALLOWED_FILES = {
    "qualification.json",
    "baseline-compose.json",
    "candidate-compose.json",
    "baseline.env",
    "baseline-recovery-point.json",
    "candidate.env",
    "baseline.override.yml",
    "candidate.override.yml",
    "baseline-containers.json",
    "frozen-containers.json",
    "postgres.dump",
    "source-provisional.tsv",
    "source-frozen.tsv",
    "references-provisional.tsv",
    "active-links-provisional.tsv",
    "references.tsv",
    "active-links.tsv",
    "copy-pass-1.tsv",
    "copy-pass-2.tsv",
    "destination-census-pass-1.tsv",
    "destination-census-pass-2.tsv",
    "migrations.json",
}
PRE_COPY_FILES = ALLOWED_FILES - {
    "copy-pass-1.tsv",
    "copy-pass-2.tsv",
    "destination-census-pass-1.tsv",
    "destination-census-pass-2.tsv",
}
ALLOWED_DIRS = {"candidate-secrets"}
EXPECTED_COLUMN_SCHEMA = {
    "owner_id": ("bigint", True, None),
    "peer_type": ("smallint", True, None),
    "peer_id": ("bigint", True, None),
    "unread": ("boolean", True, None),
    "changed_at": ("timestamp with time zone", True, None),
}
EXPECTED_RUSTFS_POLICY = {
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Action": ["s3:ListBucket"],
            "Resource": ["arn:aws:s3:::telegram"],
            "Condition": {"StringLike": {"s3:prefix": ["telegramd/*"]}},
        },
        {
            "Effect": "Allow",
            "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
            "Resource": ["arn:aws:s3:::telegram/telegramd/*"],
        },
    ],
}


class GateReject(Exception):
    def __init__(self, reason: str):
        self.reason = reason


def require(condition: bool, reason: str) -> None:
    if not condition:
        raise GateReject(reason)


def no_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate key")
        result[key] = value
    return result


def no_non_json_constant(_value: str) -> None:
    raise ValueError("non-json constant")


def checked_file(path: Path, mode: int = 0o600, max_size: int | None = None) -> os.stat_result:
    try:
        info = path.lstat()
    except OSError as exc:
        raise GateReject("bundle_invalid") from exc
    require(stat.S_ISREG(info.st_mode), "bundle_invalid")
    require(info.st_uid == 0 and stat.S_IMODE(info.st_mode) == mode, "bundle_invalid")
    if max_size is not None:
        require(info.st_size <= max_size, "bundle_invalid")
    return info


def read_bytes(path: Path, max_size: int | None = None, mode: int = 0o600) -> bytes:
    info = checked_file(path, mode=mode, max_size=max_size)
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        fd = os.open(path, flags)
        with os.fdopen(fd, "rb") as stream:
            raw = stream.read() if max_size is None else stream.read(max_size + 1)
    except OSError as exc:
        raise GateReject("bundle_invalid") from exc
    if max_size is not None:
        require(len(raw) <= max_size and len(raw) == info.st_size, "bundle_invalid")
    return raw


def read_regular_bytes(path: Path, max_size: int) -> bytes:
    try:
        info = path.lstat()
    except OSError as exc:
        raise GateReject("bundle_invalid") from exc
    require(stat.S_ISREG(info.st_mode) and info.st_uid == 0 and info.st_size <= max_size, "bundle_invalid")
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        fd = os.open(path, flags)
        with os.fdopen(fd, "rb") as stream:
            raw = stream.read(max_size + 1)
    except OSError as exc:
        raise GateReject("bundle_invalid") from exc
    require(len(raw) <= max_size and len(raw) == info.st_size, "bundle_invalid")
    return raw


def read_json(path: Path, max_size: int = 16 * 1024 * 1024) -> Any:
    try:
        return json.loads(
            read_bytes(path, max_size).decode("utf-8"),
            object_pairs_hook=no_duplicate_keys,
            parse_constant=no_non_json_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
        raise GateReject("bundle_invalid") from exc


def sha256_bytes(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def sha256_file(path: Path) -> str:
    checked_file(path)
    digest = hashlib.sha256()
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        fd = os.open(path, flags)
        with os.fdopen(fd, "rb") as stream:
            for block in iter(lambda: stream.read(1024 * 1024), b""):
                digest.update(block)
    except OSError as exc:
        raise GateReject("bundle_invalid") from exc
    return digest.hexdigest()


def canonical_json_sha256(value: Any) -> str:
    encoded = json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    return sha256_bytes(encoded)


def require_bundle_dir(bundle: Path, pre_copy: bool = False) -> None:
    try:
        info = bundle.lstat()
    except OSError as exc:
        raise GateReject("bundle_invalid") from exc
    require(stat.S_ISDIR(info.st_mode) and not stat.S_ISLNK(info.st_mode), "bundle_invalid")
    require(info.st_uid == 0 and stat.S_IMODE(info.st_mode) == 0o700, "bundle_invalid")
    try:
        names = {entry.name for entry in bundle.iterdir()}
    except OSError as exc:
        raise GateReject("bundle_invalid") from exc
    allowed_files = PRE_COPY_FILES if pre_copy else ALLOWED_FILES
    require(names == allowed_files | ALLOWED_DIRS, "bundle_invalid")
    for name in allowed_files:
        checked_file(bundle / name, max_size=1_073_741_824 if name == "postgres.dump" else 256 * 1024 * 1024)
    secret_dir = bundle / "candidate-secrets"
    try:
        info = secret_dir.lstat()
        secret_names = {entry.name for entry in secret_dir.iterdir()}
    except OSError as exc:
        raise GateReject("bundle_invalid") from exc
    require(stat.S_ISDIR(info.st_mode) and info.st_uid == 0, "bundle_invalid")
    require(stat.S_IMODE(info.st_mode) == 0o700 and secret_names == {"telegramd-blob-secret-key"}, "bundle_invalid")
    checked_file(secret_dir / "telegramd-blob-secret-key", mode=0o444, max_size=4096)


def parse_time(value: Any) -> dt.datetime:
    require(isinstance(value, str), "evidence_invalid")
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise GateReject("evidence_invalid") from exc
    require(parsed.tzinfo is not None and parsed.utcoffset() == dt.timedelta(0), "evidence_invalid")
    return parsed


def parse_env(raw: bytes) -> dict[str, str]:
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise GateReject("env_drift") from exc
    require("\r" not in text and "\x00" not in text, "env_drift")
    result: dict[str, str] = {}
    for line in text.splitlines():
        if not line or line.lstrip().startswith("#"):
            continue
        key, separator, value = line.partition("=")
        require(bool(separator) and re.fullmatch(r"[A-Z_][A-Z0-9_]*", key) is not None, "env_drift")
        require(key not in result, "env_drift")
        result[key] = value
    return result


def validate_env(bundle: Path) -> dict[str, str]:
    baseline_raw = read_bytes(bundle / "baseline.env", 1024 * 1024)
    candidate_raw = read_bytes(bundle / "candidate.env", 1024 * 1024)
    require(baseline_raw.endswith(b"\n") and candidate_raw.startswith(baseline_raw), "env_drift")
    baseline = parse_env(baseline_raw)
    candidate = parse_env(candidate_raw)
    require(not any(key in baseline for key in RUSTFS_ENV_KEYS), "env_drift")
    suffix = candidate_raw[len(baseline_raw) :]
    values = {key: candidate.get(key) for key in RUSTFS_ENV_KEYS}
    require(all(isinstance(value, str) and value for value in values.values()), "secret_mismatch")
    expected_suffix = b"".join(f"{key}={values[key]}\n".encode("ascii") for key in RUSTFS_ENV_KEYS)
    require(suffix == expected_suffix, "env_drift")
    require(set(candidate) == set(baseline) | set(RUSTFS_ENV_KEYS), "env_drift")
    require(all(candidate[key] == value for key, value in baseline.items()), "env_drift")

    root_access = values["RUSTFS_ROOT_ACCESS_KEY"]
    root_secret = values["RUSTFS_ROOT_SECRET_KEY"]
    app_access = values["TG_BLOB_S3_ACCESS_KEY_ID"]
    app_secret = values["TG_BLOB_S3_SECRET_ACCESS_KEY"]
    require(re.fullmatch(r"[0-9a-f]{20}", root_access) is not None, "secret_mismatch")
    require(re.fullmatch(r"[0-9a-f]{64}", root_secret) is not None, "secret_mismatch")
    require(re.fullmatch(r"[0-9a-f]{20}", app_access) is not None, "secret_mismatch")
    require(re.fullmatch(r"[0-9a-f]{64}", app_secret) is not None, "secret_mismatch")
    require(root_access != app_access, "secret_mismatch")
    secret_file = read_bytes(bundle / "candidate-secrets" / "telegramd-blob-secret-key", 4096, mode=0o444)
    require(secret_file == app_secret.encode("ascii"), "secret_mismatch")
    return values


def validate_baseline_env_provenance(bundle: Path, qualification: dict[str, Any], baseline_compose: Any) -> None:
    provenance = qualification.get("baseline_env_provenance")
    require(
        isinstance(provenance, dict)
        and set(provenance)
        == {
            "source_issue",
            "recovery_point_id",
            "recovery_point_sha256",
            "recovery_point_env_sha256",
            "baseline_env_sha256",
            "inspected_baseline_sha256",
            "inspected_env_sha256",
            "baseline_compose_sha256",
            "inspected_at",
        },
        "baseline_env_provenance",
    )
    require(provenance.get("source_issue") == "MAIN-1387", "baseline_env_provenance")
    recovery_point_id = provenance.get("recovery_point_id")
    require(
        isinstance(recovery_point_id, str)
        and 1 <= len(recovery_point_id) <= 256
        and not any(ord(char) < 0x20 or ord(char) == 0x7F for char in recovery_point_id),
        "baseline_env_provenance",
    )
    raw = read_bytes(bundle / "baseline.env", 1024 * 1024)
    env_sha = sha256_bytes(raw)
    baseline_inventory = bundle / "baseline-containers.json"
    inventory_sha = sha256_file(baseline_inventory)
    compose_sha = canonical_json_sha256(baseline_compose)
    recovery_point = read_json(bundle / "baseline-recovery-point.json")
    require(
        isinstance(recovery_point, dict)
        and set(recovery_point)
        == {
            "schema",
            "source_issue",
            "recovery_point_id",
            "baseline_env_sha256",
            "inspected_baseline_sha256",
            "baseline_compose_sha256",
        }
        and recovery_point.get("schema") == "teagram.main-1387-recovery-point/v1"
        and recovery_point.get("source_issue") == "MAIN-1387"
        and recovery_point.get("recovery_point_id") == recovery_point_id
        and recovery_point.get("baseline_env_sha256") == env_sha
        and recovery_point.get("inspected_baseline_sha256") == inventory_sha
        and recovery_point.get("baseline_compose_sha256") == compose_sha
        and provenance.get("recovery_point_sha256") == sha256_file(bundle / "baseline-recovery-point.json"),
        "baseline_env_provenance",
    )
    require(
        re.fullmatch(r"[0-9a-f]{64}", str(provenance.get("recovery_point_sha256", ""))) is not None
        and provenance.get("recovery_point_env_sha256") == env_sha
        and provenance.get("baseline_env_sha256") == env_sha
        and provenance.get("inspected_env_sha256") == env_sha,
        "baseline_env_provenance",
    )
    require(
        provenance.get("inspected_baseline_sha256") == inventory_sha
        and provenance.get("baseline_compose_sha256") == compose_sha,
        "baseline_env_provenance",
    )
    inspected_at = parse_time(provenance.get("inspected_at"))
    baseline_document, baseline_captured_at = read_inventory(baseline_inventory)
    require(
        inspected_at <= baseline_captured_at
        and baseline_document.get("complete") is True,
        "baseline_env_provenance",
    )


def validate_candidate_files(bundle: Path, candidate_root: Path, secret_values: dict[str, str]) -> None:
    env_path = candidate_root / ".env"
    override_path = candidate_root / "docker-compose.override.yml"
    secret_dir = candidate_root / ".secrets"
    secret_path = secret_dir / "telegramd-blob-secret-key"
    try:
        env_info = env_path.lstat()
        override_info = override_path.lstat()
        secret_dir_info = secret_dir.lstat()
        secret_info = secret_path.lstat()
        secret_names = {entry.name for entry in secret_dir.iterdir()}
    except OSError as exc:
        raise GateReject("secret_mismatch") from exc
    require(
        stat.S_ISREG(env_info.st_mode)
        and env_info.st_uid == 0
        and stat.S_IMODE(env_info.st_mode) == 0o600,
        "env_drift",
    )
    require(
        stat.S_ISREG(override_info.st_mode)
        and not stat.S_ISLNK(override_info.st_mode)
        and override_info.st_uid == 0
        and not stat.S_IMODE(override_info.st_mode) & 0o022,
        "protected_override",
    )
    require(stat.S_ISDIR(secret_dir_info.st_mode) and secret_dir_info.st_uid == 0, "secret_mismatch")
    require(
        stat.S_IMODE(secret_dir_info.st_mode) == 0o700
        and secret_names == {"telegramd-blob-secret-key"},
        "secret_mismatch",
    )
    require(stat.S_ISREG(secret_info.st_mode) and secret_info.st_uid == 0, "secret_mismatch")
    require(stat.S_IMODE(secret_info.st_mode) == 0o444, "secret_mismatch")
    require(
        read_bytes(env_path, 1024 * 1024) == read_bytes(bundle / "candidate.env", 1024 * 1024),
        "env_drift",
    )
    require(
        read_regular_bytes(override_path, 4 * 1024 * 1024)
        == read_bytes(bundle / "candidate.override.yml", 4 * 1024 * 1024),
        "protected_override",
    )
    require(
        read_bytes(secret_path, 4096, mode=0o444) == secret_values["TG_BLOB_S3_SECRET_ACCESS_KEY"].encode("ascii"),
        "secret_mismatch",
    )


def validate_candidate_compose_binding(
    qualification: dict[str, Any], candidate_root: Path, candidate: Any
) -> dict[str, str]:
    binding = qualification.get("candidate_compose_binding")
    require(isinstance(binding, dict) and set(binding) == {"snapshot_sha256", "inputs_sha256"}, "compose_binding_mismatch")
    require(binding.get("snapshot_sha256") == canonical_json_sha256(candidate), "compose_binding_mismatch")

    inputs = binding.get("inputs_sha256")
    input_names = {".env", "docker-compose.override.yml", "docker-compose.yml"}
    require(isinstance(inputs, dict) and set(inputs) == input_names, "compose_binding_mismatch")
    require(
        all(isinstance(digest, str) and re.fullmatch(r"[0-9a-f]{64}", digest) for digest in inputs.values()),
        "compose_binding_mismatch",
    )

    compose_path = candidate_root / "docker-compose.yml"
    try:
        compose_info = compose_path.lstat()
    except OSError as exc:
        raise GateReject("compose_binding_mismatch") from exc
    require(
        stat.S_ISREG(compose_info.st_mode)
        and compose_info.st_uid == 0
        and not stat.S_IMODE(compose_info.st_mode) & 0o022,
        "compose_binding_mismatch",
    )
    current_inputs = {
        ".env": sha256_bytes(read_bytes(candidate_root / ".env", 1024 * 1024)),
        "docker-compose.override.yml": sha256_bytes(
            read_regular_bytes(candidate_root / "docker-compose.override.yml", 4 * 1024 * 1024)
        ),
        "docker-compose.yml": sha256_bytes(read_regular_bytes(compose_path, 4 * 1024 * 1024)),
    }
    require(inputs == current_inputs, "compose_binding_mismatch")
    return current_inputs


def resolve_candidate_compose(candidate_root: Path) -> Any:
    executable = shutil.which("docker")
    require(executable is not None, "compose_resolution_failed")
    try:
        docker_path = Path(executable).resolve(strict=True)
        docker_info = docker_path.lstat()
    except OSError as exc:
        raise GateReject("compose_resolution_failed") from exc
    require(not docker_path.is_relative_to(candidate_root), "compose_resolution_failed")
    require(
        stat.S_ISREG(docker_info.st_mode)
        and docker_info.st_uid == 0
        and not stat.S_IMODE(docker_info.st_mode) & 0o022
        and os.access(docker_path, os.X_OK),
        "compose_resolution_failed",
    )
    for directory in docker_path.parents:
        try:
            directory_info = directory.lstat()
        except OSError as exc:
            raise GateReject("compose_resolution_failed") from exc
        require(
            stat.S_ISDIR(directory_info.st_mode)
            and directory_info.st_uid == 0
            and not stat.S_IMODE(directory_info.st_mode) & 0o022,
            "compose_resolution_failed",
        )

    command = [
        str(docker_path),
        "compose",
        "--project-directory",
        str(candidate_root),
        "--env-file",
        ".env",
        "-f",
        "docker-compose.yml",
        "-f",
        "docker-compose.override.yml",
        "--profile",
        "*",
        "config",
        "--format",
        "json",
    ]
    try:
        result = subprocess.run(
            command,
            cwd=candidate_root,
            env={"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin"},
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            timeout=60,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise GateReject("compose_resolution_failed") from exc
    require(result.returncode == 0 and len(result.stdout) <= 16 * 1024 * 1024, "compose_resolution_failed")
    try:
        return json.loads(
            result.stdout.decode("utf-8"),
            object_pairs_hook=no_duplicate_keys,
            parse_constant=no_non_json_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
        raise GateReject("compose_resolution_failed") from exc


def validate_override(bundle: Path) -> None:
    baseline = read_bytes(bundle / "baseline.override.yml", 4 * 1024 * 1024)
    candidate = read_bytes(bundle / "candidate.override.yml", 4 * 1024 * 1024)
    require(baseline == candidate, "protected_override")
    for raw in (baseline, candidate):
        try:
            text = raw.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise GateReject("protected_override") from exc
        for line in text.splitlines():
            active = line.split("#", 1)[0]
            require(re.search(r"\bTG_BLOB_[A-Z0-9_]*\b", active) is None, "protected_override")
            require(re.search(r"\btgblobs\b", active, re.IGNORECASE) is None, "protected_override")


def compose_volume_names(document: Any) -> dict[str, str]:
    require(isinstance(document, dict), "configuration_mismatch")
    volumes = document.get("volumes")
    require(isinstance(volumes, dict), "configuration_mismatch")
    result: dict[str, str] = {}
    for logical_name, spec in volumes.items():
        require(isinstance(spec, dict), "configuration_mismatch")
        name = spec.get("name")
        require(isinstance(name, str) and name != "", "source_identity")
        result[logical_name] = name
    return result


def service_environment(service: Any) -> dict[str, Any]:
    require(isinstance(service, dict), "configuration_mismatch")
    environment = service.get("environment", {})
    require(isinstance(environment, dict), "configuration_mismatch")
    return environment


def volumes_for(service: Any) -> list[dict[str, Any]]:
    require(isinstance(service, dict), "configuration_mismatch")
    volumes = service.get("volumes", [])
    require(isinstance(volumes, list) and all(isinstance(volume, dict) for volume in volumes), "configuration_mismatch")
    return volumes


def normalized_compose_mounts(service: Any, volume_names: dict[str, str]) -> list[tuple[str, str, str, bool]]:
    result: list[tuple[str, str, str, bool]] = []
    for mount in volumes_for(service):
        mount_type = mount.get("type")
        source = mount.get("source", "")
        target = mount.get("target")
        read_only = mount.get("read_only", False)
        require(
            isinstance(mount_type, str)
            and mount_type in {"bind", "volume", "tmpfs"}
            and isinstance(source, str)
            and isinstance(target, str)
            and target.startswith("/")
            and isinstance(read_only, bool),
            "source_identity",
        )
        if mount_type == "volume":
            require(source in volume_names, "source_identity")
            source = volume_names[source]
        elif mount_type == "bind":
            require(source.startswith("/"), "source_identity")
            source = os.path.realpath(source)
        else:
            require(source == "", "source_identity")
        result.append((mount_type, source, target, not read_only))
    return sorted(result)


def normalized_inventory_mounts(container: Any) -> list[tuple[str, str, str, bool]]:
    mounts = container.get("mounts")
    require(isinstance(mounts, list), "source_identity")
    result: list[tuple[str, str, str, bool]] = []
    for mount in mounts:
        require(isinstance(mount, dict), "source_identity")
        mount_type = mount.get("type")
        source = mount.get("source")
        target = mount.get("target")
        rw = mount.get("rw")
        require(
            isinstance(mount_type, str)
            and mount_type in {"bind", "volume", "tmpfs"}
            and isinstance(source, str)
            and isinstance(target, str)
            and target.startswith("/")
            and isinstance(rw, bool),
            "source_identity",
        )
        if mount_type == "bind":
            require(source.startswith("/"), "source_identity")
            source = os.path.realpath(source)
        result.append((mount_type, source, target, rw))
    return sorted(result)


def normalized_compose_environment(service: Any) -> dict[str, str]:
    environment = service_environment(service)
    result: dict[str, str] = {}
    for key, value in environment.items():
        require(isinstance(key, str) and key and (value is None or isinstance(value, str)), "source_identity")
        if value is not None:
            result[key] = value
    return result


def normalized_ports(ports: Any) -> list[tuple[int, str, str, str]]:
    require(isinstance(ports, list), "source_identity")
    result: list[tuple[int, str, str, str]] = []
    for port in ports:
        require(isinstance(port, dict), "source_identity")
        target = port.get("target")
        published = port.get("published", "")
        host_ip = port.get("host_ip", "")
        protocol = port.get("protocol", "tcp")
        require(
            isinstance(target, int)
            and not isinstance(target, bool)
            and target > 0
            and isinstance(published, (str, int))
            and not isinstance(published, bool)
            and isinstance(host_ip, str)
            and isinstance(protocol, str)
            and protocol in {"tcp", "udp", "sctp"},
            "source_identity",
        )
        result.append((target, str(published), host_ip, protocol))
    return sorted(result)


def count_target(volumes: list[dict[str, Any]], target: str) -> list[dict[str, Any]]:
    return [volume for volume in volumes if volume.get("target") == target]


def check_secret_mount(service: Any) -> None:
    secrets = service.get("secrets", [])
    require(isinstance(secrets, list), "configuration_mismatch")
    matching = [item for item in secrets if isinstance(item, dict) and item.get("target") == "telegramd-blob-secret-key"]
    require(len(matching) == 1, "configuration_mismatch")
    require(matching[0] == {"source": "telegramd_blob_secret_key", "target": "telegramd-blob-secret-key"}, "configuration_mismatch")


def check_blob_mode_mount(service: Any, expected_source: str) -> None:
    matches = count_target(volumes_for(service), "/run/telegramd/blob-mode")
    require(len(matches) == 1, "configuration_mismatch")
    mount = matches[0]
    require(mount.get("type") == "bind", "configuration_mismatch")
    require(mount.get("source") == expected_source and mount.get("read_only") is True, "configuration_mismatch")
    allowed_fields = {"type", "source", "target", "read_only", "bind"}
    require(set(mount) <= allowed_fields, "configuration_mismatch")
    bind = mount.get("bind", {})
    require(isinstance(bind, dict), "configuration_mismatch")
    require(set(bind) <= {"create_host_path"}, "configuration_mismatch")
    if "create_host_path" in bind:
        require(bind["create_host_path"] is True, "configuration_mismatch")


def secret_sources(service: Any) -> set[str]:
    secrets = service.get("secrets", [])
    require(isinstance(secrets, list) and all(isinstance(item, dict) for item in secrets), "configuration_mismatch")
    return {item.get("source") for item in secrets}


def check_secret_mounts(service: Any, expected: list[dict[str, Any]]) -> None:
    actual = service.get("secrets", [])
    require(isinstance(actual, list) and actual == expected, "secret_mismatch")


def check_private_network_service(service: Any) -> None:
    require(service.get("network_mode") in (None, ""), "configuration_mismatch")
    networks = service.get("networks", {"default": {}})
    require(
        isinstance(networks, dict)
        and set(networks) == {"default"}
        and networks["default"] in (None, {}),
        "configuration_mismatch",
    )


def check_normalized_dependencies(service: Any) -> None:
    dependencies = service.get("depends_on", {})
    require(isinstance(dependencies, dict), "configuration_mismatch")
    for name, dependency in dependencies.items():
        require(isinstance(name, str) and name and isinstance(dependency, dict), "configuration_mismatch")
        require(
            set(dependency) == {"condition", "required"}
            and isinstance(dependency.get("condition"), str)
            and dependency["condition"]
            in {"service_started", "service_healthy", "service_completed_successfully"}
            and dependency.get("required") is True,
            "configuration_mismatch",
        )


def check_s3_environment(service: Any, access_key: str) -> None:
    environment = service_environment(service)
    expected = {**S3_ENV, "TG_BLOB_S3_ACCESS_KEY_ID": access_key}
    require(set(environment) == set(expected), "configuration_mismatch")
    require(all(environment.get(key) == value for key, value in expected.items()), "configuration_mismatch")
    require("TG_BLOB_S3_SECRET_ACCESS_KEY" not in environment, "secret_mismatch")


def check_rustfs_services(
    candidate: dict[str, Any], volume_names: dict[str, str], secret_values: dict[str, str], candidate_root: Path
) -> None:
    services = candidate.get("services")
    require(isinstance(services, dict), "configuration_mismatch")
    require(set(services) >= ADDED_SERVICES, "configuration_mismatch")

    rustfs = services["rustfs"]
    require(isinstance(rustfs, dict), "configuration_mismatch")
    require(
        set(rustfs)
        <= {
            "image",
            "command",
            "entrypoint",
            "environment",
            "secrets",
            "volumes",
            "healthcheck",
            "restart",
            "logging",
            "networks",
            "ports",
        },
        "configuration_mismatch",
    )
    require(rustfs.get("command") is None and rustfs.get("entrypoint") is None, "configuration_mismatch")
    check_private_network_service(rustfs)
    require(rustfs.get("image") == PINNED_RUSTFS_IMAGE, "configuration_mismatch")
    require(rustfs.get("ports", []) == [], "configuration_mismatch")
    rustfs_env = service_environment(rustfs)
    require(
        rustfs_env
        == {
            "RUSTFS_VOLUMES": "/data",
            "RUSTFS_ADDRESS": "0.0.0.0:9000",
            "RUSTFS_CONSOLE_ENABLE": "false",
            "RUSTFS_ACCESS_KEY_FILE": "/run/secrets/rustfs-root-access-key",
            "RUSTFS_SECRET_KEY_FILE": "/run/secrets/rustfs-root-secret-key",
            "RUSTFS_OBS_LOGGER_LEVEL": "warn",
        },
        "configuration_mismatch",
    )
    rustfs_volumes = volumes_for(rustfs)
    require(
        len(rustfs_volumes) == 1
        and rustfs_volumes[0].get("type") == "volume"
        and rustfs_volumes[0].get("source") == "rustfsdata"
        and rustfs_volumes[0].get("target") == "/data"
        and rustfs_volumes[0].get("read_only", False) is False,
        "configuration_mismatch",
    )
    require(set(rustfs_volumes[0]) <= {"type", "source", "target", "read_only"}, "configuration_mismatch")
    require(
        secret_sources(rustfs) == {"rustfs_root_access_key", "rustfs_root_secret_key"},
        "configuration_mismatch",
    )
    root_secret_mounts = [
        {
            "source": "rustfs_root_access_key",
            "target": "rustfs-root-access-key",
            "uid": "10001",
            "gid": "10001",
            "mode": "0400",
        },
        {
            "source": "rustfs_root_secret_key",
            "target": "rustfs-root-secret-key",
            "uid": "10001",
            "gid": "10001",
            "mode": "0400",
        },
    ]
    actual_root_secrets = rustfs.get("secrets", [])
    require(isinstance(actual_root_secrets, list) and len(actual_root_secrets) == 2, "secret_mismatch")
    for actual, expected in zip(actual_root_secrets, root_secret_mounts):
        require(
            isinstance(actual, dict)
            and set(actual) == set(expected)
            and all(actual.get(key) == value for key, value in expected.items()),
            "secret_mismatch",
        )
    require(
        rustfs.get("healthcheck")
        == {
            "test": ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:9000/health/ready"],
            "interval": "2s",
            "timeout": "5s",
            "retries": 30,
            "start_period": "5s",
        },
        "configuration_mismatch",
    )
    require(rustfs.get("restart") == "unless-stopped", "configuration_mismatch")
    require(
        rustfs.get("logging")
        == {"driver": "json-file", "options": {"max-size": "10m", "max-file": "3"}},
        "configuration_mismatch",
    )

    rustfs_init = services["rustfs-init"]
    require(isinstance(rustfs_init, dict), "configuration_mismatch")
    require(
        set(rustfs_init)
        <= {
            "image",
            "build",
            "command",
            "entrypoint",
            "environment",
            "secrets",
            "volumes",
            "depends_on",
            "networks",
            "ports",
        },
        "configuration_mismatch",
    )
    require(rustfs_init.get("command") is None, "configuration_mismatch")
    check_private_network_service(rustfs_init)
    require(rustfs_init.get("ports", []) == [], "configuration_mismatch")
    require(rustfs_init.get("image") == "telegramd-mc-init:local", "configuration_mismatch")
    require(
        rustfs_init.get("build") == {"context": str(candidate_root), "dockerfile": "Dockerfile.mc"},
        "configuration_mismatch",
    )
    require(rustfs_init.get("entrypoint") == ["/bin/sh", "/usr/local/bin/rustfs-init.sh"], "configuration_mismatch")
    init_dependencies = rustfs_init.get("depends_on", {})
    require(
        isinstance(init_dependencies, dict)
        and init_dependencies == {"rustfs": {"condition": "service_healthy", "required": True}},
        "configuration_mismatch",
    )
    check_secret_mounts(
        rustfs_init,
        [
            {"source": "rustfs_root_access_key", "target": "rustfs-root-access-key"},
            {"source": "rustfs_root_secret_key", "target": "rustfs-root-secret-key"},
            {"source": "telegramd_blob_secret_key", "target": "telegramd-blob-secret-key"},
        ],
    )
    expected_policy = os.path.realpath(candidate_root / "deploy" / "rustfs" / "telegramd-blob.json")
    policy_path = candidate_root / "deploy" / "rustfs" / "telegramd-blob.json"
    try:
        policy = json.loads(
            read_regular_bytes(policy_path, 1024 * 1024).decode("utf-8"),
            object_pairs_hook=no_duplicate_keys,
            parse_constant=no_non_json_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
        raise GateReject("configuration_mismatch") from exc
    require(policy == EXPECTED_RUSTFS_POLICY, "configuration_mismatch")
    init_volumes = volumes_for(rustfs_init)
    require(
        len(init_volumes) == 1
        and init_volumes[0].get("type") == "bind"
        and init_volumes[0].get("source") == expected_policy
        and init_volumes[0].get("target") == "/policy/telegramd-blob.json"
        and init_volumes[0].get("read_only") is True,
        "configuration_mismatch",
    )
    require(set(init_volumes[0]) <= {"type", "source", "target", "read_only", "bind"}, "configuration_mismatch")
    policy_bind = init_volumes[0].get("bind", {})
    require(
        isinstance(policy_bind, dict)
        and set(policy_bind) <= {"create_host_path"}
        and policy_bind.get("create_host_path", True) is True,
        "configuration_mismatch",
    )
    init_environment = service_environment(rustfs_init)
    require(
        set(init_environment) == {"TG_BLOB_S3_ACCESS_KEY_ID"}
        and init_environment.get("TG_BLOB_S3_ACCESS_KEY_ID") == secret_values["TG_BLOB_S3_ACCESS_KEY_ID"],
        "secret_mismatch",
    )

    for service_name in ("blob-migrate", "blob-restore"):
        service = services[service_name]
        require(isinstance(service, dict), "configuration_mismatch")
        require(
            set(service)
            <= {
                "image",
                "entrypoint",
                "command",
                "environment",
                "secrets",
                "volumes",
                "profiles",
                "depends_on",
                "networks",
                "ports",
            },
            "configuration_mismatch",
        )
        check_private_network_service(service)
        require(service.get("profiles") == ["migration"] and service.get("ports", []) == [], "configuration_mismatch")
        require(service.get("image") == "telegramd:local", "configuration_mismatch")
        require(service.get("entrypoint") == ["/usr/local/bin/blob-migrate"], "configuration_mismatch")
        expected_command = (
            ["--source", "/source"]
            if service_name == "blob-migrate"
            else ["--direction", "s3-to-local", "--destination", "/destination"]
        )
        require(service.get("command") == expected_command, "configuration_mismatch")
        check_s3_environment(service, secret_values["TG_BLOB_S3_ACCESS_KEY_ID"])
        check_secret_mounts(
            service,
            [{"source": "telegramd_blob_secret_key", "target": "telegramd-blob-secret-key"}],
        )
        copy_mounts = volumes_for(service)
        mount_target = "/source" if service_name == "blob-migrate" else "/destination"
        require(
            len(copy_mounts) == 1
            and copy_mounts[0].get("type") == "volume"
            and copy_mounts[0].get("source") == "tgblobs"
            and copy_mounts[0].get("target") == mount_target
            and copy_mounts[0].get("read_only", False) is (service_name == "blob-migrate"),
            "configuration_mismatch",
        )
        require(set(copy_mounts[0]) <= {"type", "source", "target", "read_only"}, "configuration_mismatch")
        require(not count_target(copy_mounts, "/run/telegramd/blob-mode"), "configuration_mismatch")
        dependencies = service.get("depends_on", {})
        require(
            isinstance(dependencies, dict)
            and dependencies.get("rustfs-init")
            == {"condition": "service_completed_successfully", "required": True},
            "configuration_mismatch",
        )


def check_candidate_services(
    baseline: dict[str, Any], candidate: dict[str, Any], secret_values: dict[str, str], candidate_root: Path
) -> tuple[dict[str, str], str]:
    base_services = baseline.get("services")
    target_services = candidate.get("services")
    require(isinstance(base_services, dict) and isinstance(target_services, dict), "configuration_mismatch")
    project_name = candidate.get("name")
    require(
        isinstance(project_name, str) and project_name and baseline.get("name") == project_name,
        "configuration_mismatch",
    )
    for service in (*base_services.values(), *target_services.values()):
        require(isinstance(service, dict), "configuration_mismatch")
        check_normalized_dependencies(service)
    require(set(target_services) - set(base_services) == ADDED_SERVICES, "configuration_mismatch")
    require(set(base_services) - set(target_services) == set(), "configuration_mismatch")

    base_volumes = compose_volume_names(baseline)
    target_volumes = compose_volume_names(candidate)
    require(set(base_volumes) == {"pgdata", "tgkey", "tgblobs"}, "configuration_mismatch")
    require(set(target_volumes) == set(base_volumes) | {"rustfsdata"}, "configuration_mismatch")
    for logical_name in base_volumes:
        require(target_volumes[logical_name] == base_volumes[logical_name], "source_identity")
    require(
        target_volumes["rustfsdata"] == f"{project_name}_rustfsdata"
        and target_volumes["rustfsdata"] not in base_volumes.values(),
        "source_identity",
    )
    require(candidate["volumes"]["rustfsdata"] == {"name": target_volumes["rustfsdata"]}, "source_identity")
    check_rustfs_services(candidate, target_volumes, secret_values, candidate_root)
    for service_name, service in target_services.items():
        if not service_name.startswith("telegramd"):
            require(not count_target(volumes_for(service), "/run/telegramd/blob-mode"), "configuration_mismatch")

    base_secrets = baseline.get("secrets", {})
    target_secrets = candidate.get("secrets", {})
    require(isinstance(base_secrets, dict) and isinstance(target_secrets, dict), "configuration_mismatch")
    require(set(target_secrets) - set(base_secrets) == ADDED_SECRETS, "configuration_mismatch")
    require(set(base_secrets) - set(target_secrets) == set(), "configuration_mismatch")
    expected_secret_source = os.path.realpath(candidate_root / ".secrets" / "telegramd-blob-secret-key")
    require(
        target_secrets.get("telegramd_blob_secret_key")
        == {
            "file": expected_secret_source,
            "name": f"{project_name}_telegramd_blob_secret_key",
        },
        "secret_mismatch",
    )
    require(
        target_secrets.get("rustfs_root_access_key")
        == {
            "environment": "RUSTFS_ROOT_ACCESS_KEY",
            "name": f"{project_name}_rustfs_root_access_key",
        }
        and target_secrets.get("rustfs_root_secret_key")
        == {
            "environment": "RUSTFS_ROOT_SECRET_KEY",
            "name": f"{project_name}_rustfs_root_secret_key",
        },
        "secret_mismatch",
    )

    state_dir = candidate_root / ".state"
    mode_dir = state_dir / "blob-mode"
    for path in (state_dir, mode_dir):
        try:
            info = path.lstat()
        except OSError as exc:
            raise GateReject("configuration_mismatch") from exc
        require(
            stat.S_ISDIR(info.st_mode)
            and not stat.S_ISLNK(info.st_mode)
            and info.st_uid == 0
            and (stat.S_IMODE(info.st_mode) & 0o022) == 0,
            "configuration_mismatch",
        )
    expected_mode_source = os.path.realpath(mode_dir)
    normalized_base = copy.deepcopy(baseline)
    normalized_target = copy.deepcopy(candidate)
    base_services_copy = normalized_base["services"]
    target_services_copy = normalized_target["services"]

    for service_name in sorted(base_services):
        if not service_name.startswith("telegramd"):
            continue
        before = base_services_copy[service_name]
        after = target_services_copy[service_name]
        before_env = service_environment(before)
        after_env = service_environment(after)
        require(all(before_env.get(key) in (None, "") for key in S3_ENV_KEYS), "configuration_mismatch")
        for key, expected in S3_ENV.items():
            require(after_env.get(key) == expected, "configuration_mismatch")
        require(after_env.get("TG_BLOB_S3_ACCESS_KEY_ID") == secret_values["TG_BLOB_S3_ACCESS_KEY_ID"], "secret_mismatch")
        require("TG_BLOB_S3_SECRET_ACCESS_KEY" not in after_env, "secret_mismatch")
        require(
            {key for key in after_env if key.startswith("TG_BLOB_S3_")} == S3_ENV_KEYS,
            "configuration_mismatch",
        )
        check_secret_mount(after)
        check_blob_mode_mount(after, expected_mode_source)

        before_volumes = volumes_for(before)
        after_volumes = volumes_for(after)
        before_tgblobs = count_target(before_volumes, "/var/lib/telegramd-blobs")
        after_tgblobs = count_target(after_volumes, "/var/lib/telegramd-blobs")
        require(len(before_tgblobs) == 1 and len(after_tgblobs) == 1, "source_identity")
        require(
            before_tgblobs[0].get("type") == "volume"
            and before_tgblobs[0].get("source") == "tgblobs"
            and before_tgblobs[0].get("read_only", False) is False,
            "source_identity",
        )
        require(
            after_tgblobs[0].get("type") == "volume"
            and after_tgblobs[0].get("source") == "tgblobs"
            and after_tgblobs[0].get("read_only") is True,
            "source_identity",
        )

        for key, expected in (("TG_REPLICA_COUNT", "1"),):
            require(after_env.get(key) == expected and before_env.get(key) in (None, expected), "configuration_mismatch")
            before_env.pop(key, None)
            after_env.pop(key, None)
        if service_name == "telegramd":
            require(
                after_env.get("TG_CLIENT_ADDR_TRUST") == "socket"
                and before_env.get("TG_CLIENT_ADDR_TRUST") in (None, "socket"),
                "configuration_mismatch",
            )
            before_env.pop("TG_CLIENT_ADDR_TRUST", None)
            after_env.pop("TG_CLIENT_ADDR_TRUST", None)
        for key in S3_ENV_KEYS:
            before_env.pop(key, None)
            after_env.pop(key, None)
        if not before_env:
            before.pop("environment", None)
        if not after_env:
            after.pop("environment", None)

        after["volumes"] = [
            volume for volume in after_volumes if volume.get("target") != "/run/telegramd/blob-mode"
        ]
        normalized_blob = next(
            volume for volume in after["volumes"] if volume.get("target") == "/var/lib/telegramd-blobs"
        )
        normalized_blob.pop("read_only", None)
        if not before.get("volumes"):
            before.pop("volumes", None)
        if not after["volumes"]:
            after.pop("volumes", None)

        after["secrets"] = [
            item for item in after.get("secrets", []) if item.get("target") != "telegramd-blob-secret-key"
        ]
        if not before.get("secrets"):
            before.pop("secrets", None)
        if not after["secrets"]:
            after.pop("secrets", None)

        dependencies = after.get("depends_on", {})
        require(isinstance(dependencies, dict), "configuration_mismatch")
        rustfs_dependency = dependencies.get("rustfs-init")
        require(
            rustfs_dependency == {"condition": "service_completed_successfully", "required": True},
            "configuration_mismatch",
        )
        dependencies.pop("rustfs-init")
        if not dependencies:
            after.pop("depends_on", None)

    for service_name in ADDED_SERVICES:
        target_services_copy.pop(service_name)
    normalized_target["volumes"].pop("rustfsdata")
    for secret_name in ADDED_SECRETS:
        normalized_target["secrets"].pop(secret_name)
    if not normalized_target["secrets"]:
        normalized_target.pop("secrets")

    require(normalized_target == normalized_base, "configuration_mismatch")
    return target_volumes, expected_mode_source


def read_inventory(path: Path) -> tuple[dict[str, Any], dt.datetime]:
    document = read_json(path)
    require(isinstance(document, dict) and document.get("complete") is True, "writer_freeze_incomplete")
    captured_at = parse_time(document.get("captured_at"))
    containers = document.get("containers")
    require(isinstance(containers, list), "writer_freeze_incomplete")
    identifiers: set[str] = set()
    for container in containers:
        require(isinstance(container, dict), "writer_freeze_incomplete")
        identifier = container.get("id")
        service = container.get("service")
        mounts = container.get("mounts")
        environment = container.get("environment")
        require(isinstance(identifier, str) and identifier and identifier not in identifiers, "writer_freeze_incomplete")
        identifiers.add(identifier)
        require(isinstance(service, str) and isinstance(mounts, list) and isinstance(environment, dict), "writer_freeze_incomplete")
        require(container.get("running") is True, "writer_freeze_incomplete")
        require(
            all(isinstance(key, str) and isinstance(value, str) for key, value in environment.items()),
            "writer_freeze_incomplete",
        )
        for mount in mounts:
            require(
                isinstance(mount, dict)
                and isinstance(mount.get("type"), str)
                and mount.get("type") in {"bind", "volume", "tmpfs"}
                and isinstance(mount.get("source"), str)
                and isinstance(mount.get("target"), str)
                and mount.get("target", "").startswith("/")
                and isinstance(mount.get("rw"), bool),
                "writer_freeze_incomplete",
            )
        if "ports" in container:
            ports = container["ports"]
            require(isinstance(ports, list), "writer_freeze_incomplete")
            for port in ports:
                require(
                    isinstance(port, dict)
                    and isinstance(port.get("target"), int)
                    and not isinstance(port.get("target"), bool)
                    and port.get("target") > 0
                    and isinstance(port.get("published", ""), (str, int))
                    and not isinstance(port.get("published", ""), bool)
                    and isinstance(port.get("host_ip", ""), str)
                    and isinstance(port.get("protocol", "tcp"), str),
                    "writer_freeze_incomplete",
                )
    return document, captured_at


def validate_baseline_containers(
    document: dict[str, Any],
    baseline_compose: dict[str, Any],
    baseline_volumes: dict[str, str],
    source_volume: str,
) -> None:
    compose_services = baseline_compose.get("services")
    require(isinstance(compose_services, dict), "source_identity")
    require(source_volume == baseline_volumes.get("tgblobs"), "source_identity")
    require(
        all(isinstance(name, str) and isinstance(service, dict) for name, service in compose_services.items()),
        "source_identity",
    )
    for service in compose_services.values():
        profiles = service.get("profiles", [])
        require(isinstance(profiles, list) and all(isinstance(profile, str) for profile in profiles), "source_identity")

    containers_by_service: dict[str, list[dict[str, Any]]] = {}
    for container in document["containers"]:
        service_name = container["service"]
        require(service_name in compose_services, "source_identity")
        containers_by_service.setdefault(service_name, []).append(container)
        service = compose_services[service_name]
        require(
            normalized_inventory_mounts(container) == normalized_compose_mounts(service, baseline_volumes),
            "source_identity",
        )
        require(
            container["environment"] == normalized_compose_environment(service),
            "source_identity",
        )
        require(
            "ports" in container
            and normalized_ports(container["ports"]) == normalized_ports(service.get("ports", [])),
            "source_identity",
        )
    for service_name, service in compose_services.items():
        if not service.get("profiles", []) and service_name not in ONE_SHOT_BASELINE_SERVICES:
            require(bool(containers_by_service.get(service_name)), "source_identity")

    expected_blob_dirs: dict[str, str] = {}
    for service_name, service in compose_services.items():
        if not service_name.startswith("telegramd"):
            continue
        environment = service_environment(service)
        blob_dir = environment.get("TG_BLOB_DIR")
        require(
            isinstance(blob_dir, str)
            and "\x00" not in blob_dir
            and os.path.isabs(blob_dir)
            and os.path.normpath(blob_dir) == blob_dir,
            "source_identity",
        )
        blob_mounts = [
            mount
            for mount in volumes_for(service)
            if mount.get("target") == blob_dir
        ]
        require(
            len(blob_mounts) == 1
            and blob_mounts[0].get("type") == "volume"
            and blob_mounts[0].get("source") == "tgblobs",
            "source_identity",
        )
        expected_blob_dirs[service_name] = blob_dir

    services = document["containers"]
    replicas = [container for container in services if container["service"].startswith("telegramd")]
    require(bool(replicas), "source_identity")
    for container in replicas:
        service_name = container["service"]
        require(service_name in expected_blob_dirs, "source_identity")
        blob_dir = expected_blob_dirs[service_name]
        mounts = container["mounts"]
        blob_mounts = [mount for mount in mounts if mount["target"] == blob_dir]
        key_mounts = [mount for mount in mounts if mount["target"] == "/var/lib/telegramd"]
        require(
            len(blob_mounts) == 1
            and blob_mounts[0]["type"] == "volume"
            and blob_mounts[0]["source"] == source_volume == baseline_volumes["tgblobs"]
            and blob_mounts[0]["rw"] is True,
            "source_identity",
        )
        require(
            len(key_mounts) == 1
            and key_mounts[0]["type"] == "volume"
            and key_mounts[0]["source"] == baseline_volumes["tgkey"]
            and key_mounts[0]["rw"] is True,
            "source_identity",
        )
        environment = container["environment"]
        require(environment.get("TG_BLOB_S3_ENDPOINT", "") in (None, ""), "source_identity")
        require(environment.get("TG_BLOB_DIR") == blob_dir, "source_identity")
        require(not any("/run/telegramd/blob-mode" == mount["target"] for mount in mounts), "source_identity")
    postgres = [container for container in services if container["service"] == "postgres"]
    require(len(postgres) == 1, "source_identity")
    pg_mounts = [mount for mount in postgres[0]["mounts"] if mount["target"] == "/var/lib/postgresql/data"]
    require(
        len(pg_mounts) == 1
        and pg_mounts[0]["type"] == "volume"
        and pg_mounts[0]["source"] == baseline_volumes["pgdata"],
        "source_identity",
    )


def validate_frozen_containers(
    document: dict[str, Any], source_volume: str, app_access_key: str, postgres_volume: str
) -> None:
    require(document.get("complete") is True, "writer_freeze_incomplete")
    postgres = [container for container in document["containers"] if container["service"] == "postgres"]
    require(len(postgres) == 1, "writer_freeze_incomplete")
    pg_mounts = [mount for mount in postgres[0]["mounts"] if mount["target"] == "/var/lib/postgresql/data"]
    require(
        len(pg_mounts) == 1
        and pg_mounts[0]["type"] == "volume"
        and pg_mounts[0]["source"] == postgres_volume
        and pg_mounts[0]["rw"] is True,
        "writer_freeze_incomplete",
    )
    for container in document["containers"]:
        service = container["service"]
        require(not service.startswith("telegramd"), "writer_freeze_incomplete")
        environment = container["environment"]
        if app_access_key in environment.values():
            require(service in {"rustfs", "blob-migrate"}, "writer_freeze_incomplete")
        for mount in container["mounts"]:
            if mount["source"] == source_volume:
                require(service == "blob-migrate" and mount["rw"] is False, "writer_freeze_incomplete")
            if mount["target"] == "/run/telegramd/blob-mode":
                require(service.startswith("telegramd"), "writer_freeze_incomplete")


def validate_freeze(
    bundle: Path, qualification: dict[str, Any], source_volume: str, app_access_key: str, release_set: str
) -> None:
    freeze = qualification.get("freeze")
    require(isinstance(freeze, dict), "writer_freeze_incomplete")
    require(
        freeze.get("held") is True and freeze.get("inventory_complete") is True,
        "writer_freeze_incomplete",
    )
    started = parse_time(freeze.get("started_at"))
    held_at = parse_time(freeze.get("held_at"))
    require(started <= held_at, "writer_freeze_incomplete")

    baseline_doc, baseline_at = read_inventory(bundle / "baseline-containers.json")
    frozen_doc, frozen_at = read_inventory(bundle / "frozen-containers.json")
    require(baseline_at <= started and started <= frozen_at <= held_at, "writer_freeze_incomplete")
    baseline_compose = read_json(bundle / "baseline-compose.json")
    baseline_volumes = compose_volume_names(baseline_compose)
    validate_baseline_containers(baseline_doc, baseline_compose, baseline_volumes, source_volume)
    validate_frozen_containers(frozen_doc, source_volume, app_access_key, baseline_volumes["pgdata"])

    dump_at = parse_time(freeze.get("dump_captured_at"))
    frozen_census_at = parse_time(freeze.get("source_frozen_census_at"))
    provisional_reference_at = parse_time(freeze.get("provisional_references_captured_at"))
    reference_at = parse_time(freeze.get("references_captured_at"))
    try:
        schema_at = parse_time(freeze.get("schema_captured_at"))
    except GateReject as exc:
        if release_set in {"60-67", "60-69", "60-70"}:
            raise GateReject("schema_rejected") from exc
        raise
    require(started <= dump_at <= frozen_census_at <= held_at, "dump_invalid")
    require(
        baseline_at <= provisional_reference_at <= started <= reference_at <= held_at,
        "dump_invalid",
    )
    if release_set in {"60-67", "60-69", "60-70"}:
        try:
            baseline_schema_at = parse_time(freeze.get("baseline_schema_captured_at"))
        except GateReject as exc:
            raise GateReject("schema_rejected") from exc
        require(started <= baseline_schema_at <= dump_at <= held_at, "schema_rejected")
        require(baseline_schema_at <= schema_at <= held_at, "schema_rejected")
    if release_set == "60-70":
        try:
            inert_surfaces_at = parse_time(freeze.get("inert_surfaces_captured_at"))
        except GateReject as exc:
            raise GateReject("schema_rejected") from exc
        require(started <= inert_surfaces_at <= held_at, "schema_rejected")
    else:
        if release_set not in {"60-67", "60-69"}:
            require(started <= schema_at <= held_at, "dump_invalid")
    require(frozen_census_at <= frozen_at, "writer_freeze_incomplete")

    dump = freeze.get("postgres_dump")
    require(isinstance(dump, dict), "dump_invalid")
    require(
        dump.get("exit_status") == 0
        and dump.get("completion_marker") is True
        and dump.get("isolated_restore_exit_status") == 0
        and dump.get("isolated_restore_network") == "none",
        "dump_invalid",
    )
    expected_sha = dump.get("sha256")
    require(isinstance(expected_sha, str) and re.fullmatch(r"[0-9a-f]{64}", expected_sha), "dump_invalid")
    require(sha256_file(bundle / "postgres.dump") == expected_sha, "dump_invalid")


def safe_key(key: str) -> bool:
    if not key or key.startswith("/") or "\\" in key or "\x00" in key or re.search(r"[\x00-\x20\x7f]", key):
        return False
    if any(part in ("", ".", "..") for part in key.split("/")):
        return False
    if any(part.startswith(".tmp") or part.endswith(".tmp") for part in key.split("/")):
        return False
    return True


def read_manifest(path: Path) -> tuple[list[tuple[str, int, str]], str, int]:
    rows: list[tuple[str, int, str]] = []
    digest = hashlib.sha256()
    total = 0
    previous = ""
    try:
        with path.open("rb") as stream:
            for raw_line in stream:
                digest.update(raw_line)
                require(raw_line.endswith(b"\n") and b"\r" not in raw_line, "manifest_invalid")
                try:
                    line = raw_line[:-1].decode("utf-8")
                except UnicodeDecodeError as exc:
                    raise GateReject("manifest_invalid") from exc
                parts = line.split("\t")
                require(len(parts) == 3, "manifest_invalid")
                key, size_text, content_sha = parts
                require(safe_key(key), "manifest_invalid")
                require(re.fullmatch(r"0|[1-9][0-9]*", size_text) is not None, "manifest_invalid")
                require(re.fullmatch(r"[0-9a-f]{64}", content_sha) is not None, "manifest_invalid")
                require(not rows or previous < key, "manifest_invalid")
                size = int(size_text)
                rows.append((key, size, content_sha))
                previous = key
                total += size
    except OSError as exc:
        raise GateReject("bundle_invalid") from exc
    return rows, digest.hexdigest(), total


def file_id_from_key(key: str) -> int:
    match = re.fullmatch(r"([0-9a-f]{2})/([1-9][0-9]*)", key)
    require(match is not None, "reference_coverage")
    file_id = int(match.group(2))
    require(int(match.group(1), 16) == file_id % 256, "reference_coverage")
    return file_id


def parse_references(path: Path) -> tuple[dict[int, tuple[bool, str]], set[str], set[str]]:
    files: dict[int, tuple[bool, str]] = {}
    seen_keys: set[str] = set()
    reference_keys: set[str] = set()
    required_keys: set[str] = set()
    previous: tuple[str, str] | None = None
    try:
        with path.open("rb") as stream:
            for raw_line in stream:
                require(raw_line.endswith(b"\n") and b"\r" not in raw_line, "reference_coverage")
                try:
                    fields = raw_line[:-1].decode("utf-8").split("\t")
                except UnicodeDecodeError as exc:
                    raise GateReject("reference_coverage") from exc
                require(len(fields) == 3, "reference_coverage")
                kind, stored_text, key = fields
                require(kind in ("file", "upload_part"), "reference_coverage")
                require(stored_text in ("true", "false"), "reference_coverage")
                require(safe_key(key), "reference_coverage")
                current = (key, kind)
                require(previous is None or previous < current, "reference_coverage")
                require(key not in seen_keys, "reference_coverage")
                seen_keys.add(key)
                reference_keys.add(key)
                previous = current
                if kind == "file":
                    require(stored_text in ("true", "false"), "reference_coverage")
                    file_id = file_id_from_key(key)
                    require(file_id not in files, "reference_coverage")
                    stored = stored_text == "true"
                    files[file_id] = (stored, key)
                    if stored:
                        required_keys.add(key)
                else:
                    require(stored_text == "true", "reference_coverage")
                    required_keys.add(key)
    except OSError as exc:
        raise GateReject("bundle_invalid") from exc
    return files, reference_keys, required_keys


def validate_active_links(path: Path, files: dict[int, tuple[bool, str]]) -> int:
    active = 0
    previous: tuple[str, str] | None = None
    try:
        with path.open("rb") as stream:
            for raw_line in stream:
                require(raw_line.endswith(b"\n") and b"\r" not in raw_line, "reference_coverage")
                try:
                    fields = raw_line[:-1].decode("utf-8").split("\t")
                except UnicodeDecodeError as exc:
                    raise GateReject("reference_coverage") from exc
                require(len(fields) == 3, "reference_coverage")
                table, file_id_text, deleted_text = fields
                require(table in ("messages", "channel_messages"), "reference_coverage")
                require(re.fullmatch(r"[1-9][0-9]*", file_id_text) is not None, "reference_coverage")
                require(deleted_text in ("true", "false"), "reference_coverage")
                current = (table, file_id_text)
                require(previous is None or previous <= current, "reference_coverage")
                previous = current
                file_id = int(file_id_text)
                if deleted_text == "true":
                    continue
                active += 1
                file_row = files.get(file_id)
                require(file_row is not None and file_row[0] is True, "reference_coverage")
    except OSError as exc:
        raise GateReject("bundle_invalid") from exc
    return active


def validate_manifests(
    bundle: Path,
    qualification: dict[str, Any],
    source_volume: str,
    release_set: str,
    pre_copy: bool = False,
) -> dict[str, Any]:
    require(qualification.get("source_volume") == source_volume and source_volume != "", "source_identity")
    source_rows, source_sha, source_bytes = read_manifest(bundle / "source-frozen.tsv")
    provisional_rows, _, _ = read_manifest(bundle / "source-provisional.tsv")
    require(provisional_rows == source_rows, "source_census_changed")
    metadata = qualification.get("references")
    require(isinstance(metadata, dict), "reference_coverage")
    if release_set == "60-70":
        require(
            set(metadata)
            == {"candidate_query_sha256", "active_links_query_sha256", "inert_surfaces_query_sha256"}
            and metadata.get("inert_surfaces_query_sha256") == R70_INERT_SURFACES_QUERY_SHA256,
            "schema_rejected",
        )
    elif release_set == "60-69":
        require(
            set(metadata)
            == {"candidate_query_sha256", "active_links_query_sha256", "inert_surfaces_query_sha256"}
            and metadata.get("inert_surfaces_query_sha256") == INERT_SURFACES_QUERY_SHA256,
            "reference_coverage",
        )
    require(metadata.get("candidate_query_sha256") == REFERENCE_QUERY_SHA256, "reference_coverage")
    require(metadata.get("active_links_query_sha256") == ACTIVE_LINKS_QUERY_SHA256, "reference_coverage")
    require(
        read_bytes(bundle / "references-provisional.tsv") == read_bytes(bundle / "references.tsv")
        and read_bytes(bundle / "active-links-provisional.tsv") == read_bytes(bundle / "active-links.tsv"),
        "reference_snapshot_changed",
    )
    files, reference_keys, required_keys = parse_references(bundle / "references.tsv")
    if release_set in {"60-69", "60-70"}:
        require(not files, "reference_coverage")
    active_count = validate_active_links(bundle / "active-links.tsv", files)
    require(bool(source_rows) and bool(required_keys), "reference_coverage")
    source_keys = {row[0] for row in source_rows}
    require(required_keys <= source_keys, "reference_coverage")
    results = {
        "source_count": len(source_rows),
        "source_bytes": source_bytes,
        "source_sha": source_sha,
        "reference_count": len(reference_keys),
        "active_link_count": active_count,
    }
    if pre_copy:
        return results

    destination_one, destination_sha_one, destination_bytes_one = read_manifest(bundle / "destination-census-pass-1.tsv")
    destination_two, destination_sha_two, destination_bytes_two = read_manifest(bundle / "destination-census-pass-2.tsv")
    pass_one, _, pass_one_bytes = read_manifest(bundle / "copy-pass-1.tsv")
    pass_two, _, pass_two_bytes = read_manifest(bundle / "copy-pass-2.tsv")
    require(source_rows == destination_one == destination_two == pass_one == pass_two, "manifest_mismatch")
    require(
        len(source_rows) == len(destination_one) == len(destination_two) == len(pass_one) == len(pass_two)
        and source_bytes == destination_bytes_one == destination_bytes_two == pass_one_bytes == pass_two_bytes
        and source_sha == destination_sha_one == destination_sha_two,
        "manifest_mismatch",
    )
    results.update(
        {
            "destination_sha": destination_sha_two,
            "destination_pass_1_sha": destination_sha_one,
            "destination_bytes": destination_bytes_two,
        }
    )
    return results


REFERENCE_QUERY = """SELECT 'file'::text AS ref_kind,
       f.stored::text AS stored,
       lpad(to_hex((f.id % 256)::integer), 2, '0') || '/' || f.id::text AS blob_key
FROM files AS f
UNION ALL
SELECT 'upload_part'::text, TRUE::text, up.blob_key
FROM upload_parts AS up
ORDER BY blob_key, ref_kind;
"""
ACTIVE_LINKS_QUERY = """SELECT 'messages'::text AS source, file_id::text, deleted::text
FROM messages
WHERE file_id <> 0
UNION ALL
SELECT 'channel_messages'::text, file_id::text, deleted::text
FROM channel_messages
WHERE file_id IS NOT NULL AND file_id <> 0
ORDER BY source, file_id;
"""
REFERENCE_QUERY_SHA256 = hashlib.sha256(REFERENCE_QUERY.encode("ascii")).hexdigest()
ACTIVE_LINKS_QUERY_SHA256 = hashlib.sha256(ACTIVE_LINKS_QUERY.encode("ascii")).hexdigest()
LIVE_SCHEMA_QUERY = """WITH target AS (
  SELECT to_regclass('public.user_dialog_unread_marks') AS relation_oid
), column_schema AS (
  SELECT COALESCE(jsonb_object_agg(
    column_info.column_name,
    jsonb_build_object(
      'type', column_info.data_type,
      'not_null', column_info.is_nullable = 'NO',
      'default', column_info.column_default
    )
  ), '{}'::jsonb) AS columns
  FROM information_schema.columns AS column_info
  WHERE column_info.table_schema = 'public'
    AND column_info.table_name = 'user_dialog_unread_marks'
), primary_key AS (
  SELECT COALESCE((
    SELECT ARRAY(
      SELECT attribute.attname::text
      FROM unnest(constraint_info.conkey) WITH ORDINALITY AS key_column(attnum, ordinal)
      JOIN pg_attribute AS attribute
        ON attribute.attrelid = constraint_info.conrelid
       AND attribute.attnum = key_column.attnum
      ORDER BY key_column.ordinal
    )
    FROM pg_constraint AS constraint_info, target
    WHERE constraint_info.conrelid = target.relation_oid
      AND constraint_info.contype = 'p'
  ), ARRAY[]::text[]) AS columns
), foreign_key AS (
  SELECT (
    SELECT jsonb_build_object(
      'columns', ARRAY(
        SELECT attribute.attname::text
        FROM unnest(constraint_info.conkey) WITH ORDINALITY AS key_column(attnum, ordinal)
        JOIN pg_attribute AS attribute
          ON attribute.attrelid = constraint_info.conrelid
         AND attribute.attnum = key_column.attnum
        ORDER BY key_column.ordinal
      ),
      'referenced_table', referenced_relation.relname,
      'referenced_columns', ARRAY(
        SELECT attribute.attname::text
        FROM unnest(constraint_info.confkey) WITH ORDINALITY AS key_column(attnum, ordinal)
        JOIN pg_attribute AS attribute
          ON attribute.attrelid = constraint_info.confrelid
         AND attribute.attnum = key_column.attnum
        ORDER BY key_column.ordinal
      ),
      'on_delete', CASE constraint_info.confdeltype
        WHEN 'a' THEN 'NO ACTION'
        WHEN 'r' THEN 'RESTRICT'
        WHEN 'c' THEN 'CASCADE'
        WHEN 'n' THEN 'SET NULL'
        WHEN 'd' THEN 'SET DEFAULT'
      END,
      'validated', constraint_info.convalidated
    )
    FROM pg_constraint AS constraint_info
    JOIN pg_class AS referenced_relation ON referenced_relation.oid = constraint_info.confrelid
    CROSS JOIN target
    WHERE constraint_info.conrelid = target.relation_oid
      AND constraint_info.contype = 'f'
  ) AS value
), check_constraint AS (
  SELECT (
    SELECT jsonb_build_object(
      'name', constraint_info.conname,
      'expression', pg_get_constraintdef(constraint_info.oid),
      'validated', constraint_info.convalidated
    )
    FROM pg_constraint AS constraint_info, target
    WHERE constraint_info.conrelid = target.relation_oid
      AND constraint_info.contype = 'c'
  ) AS value
), changed_index AS (
  SELECT (
    SELECT jsonb_build_object(
      'name', index_relation.relname,
      'columns', ARRAY(
        SELECT attribute.attname::text
        FROM unnest(index_info.indkey) WITH ORDINALITY AS index_column(attnum, ordinal)
        JOIN pg_attribute AS attribute
          ON attribute.attrelid = index_info.indrelid
         AND attribute.attnum = index_column.attnum
        WHERE index_column.ordinal <= index_info.indnkeyatts
        ORDER BY index_column.ordinal
      ),
      'valid', index_info.indisvalid,
      'ready', index_info.indisready,
      'unique', index_info.indisunique
    )
    FROM pg_index AS index_info
    JOIN pg_class AS index_relation ON index_relation.oid = index_info.indexrelid
    CROSS JOIN target
    WHERE index_info.indrelid = target.relation_oid
      AND index_relation.relname = 'user_dialog_unread_marks_changed_idx'
  ) AS value
), secret_chats AS (
  SELECT to_regclass('public.secret_chats') AS relation_oid
), expected_secret_chat_indexes AS (
  SELECT index_name
  FROM (VALUES
    ('secret_chats_pkey'),
    ('secret_chats_admin_state_idx'),
    ('secret_chats_participant_state_idx'),
    ('secret_chats_admin_random_id_idx'),
    ('secret_chats_admin_date_idx'),
    ('secret_chats_participant_date_idx')
  ) AS expected(index_name)
), secret_chat_indexes AS (
  SELECT expected.index_name,
         index_info.*,
         index_relation.relname,
         access_method.amname
  FROM expected_secret_chat_indexes AS expected
  LEFT JOIN pg_class AS index_relation
    ON index_relation.relname = expected.index_name
   AND index_relation.relnamespace = 'public'::regnamespace
  LEFT JOIN pg_index AS index_info
    ON index_info.indexrelid = index_relation.oid
   AND index_info.indrelid = (SELECT relation_oid FROM secret_chats)
  LEFT JOIN pg_am AS access_method ON access_method.oid = index_relation.relam
), migration_67_schema AS (
  SELECT jsonb_build_object(
    'table', CASE WHEN secret_chats.relation_oid IS NULL THEN NULL ELSE 'public.secret_chats' END,
    'index_validity', (
      SELECT COALESCE(jsonb_object_agg(index_name, indisvalid), '{}'::jsonb)
      FROM secret_chat_indexes
    ),
    'party_date_indexes', (
      SELECT COALESCE(jsonb_object_agg(
        index_name,
        jsonb_build_object(
          'columns', ARRAY(
            SELECT attribute.attname::text
            FROM unnest(index_info.indkey) WITH ORDINALITY AS key_column(attnum, ordinal)
            JOIN pg_attribute AS attribute
              ON attribute.attrelid = index_info.indrelid
             AND attribute.attnum = key_column.attnum
            WHERE key_column.ordinal <= index_info.indnkeyatts
            ORDER BY key_column.ordinal
          ),
          'access_method', amname,
          'indisvalid', indisvalid,
          'indisready', indisready,
          'indislive', indislive,
          'indisunique', indisunique,
          'indisprimary', indisprimary,
          'indpred', pg_get_expr(indpred, indrelid),
          'indexprs', pg_get_expr(indexprs, indrelid),
          'indnatts', indnatts,
          'indnkeyatts', indnkeyatts,
          'indoption', ARRAY(
            SELECT option_value
            FROM unnest(index_info.indoption) WITH ORDINALITY AS option_column(option_value, ordinal)
            ORDER BY option_column.ordinal
          )
        )
      ), '{}'::jsonb)
      FROM secret_chat_indexes AS index_info
      WHERE index_name IN ('secret_chats_admin_date_idx', 'secret_chats_participant_date_idx')
    )
  ) AS value
  FROM secret_chats
), revision_set AS (
  SELECT
    COALESCE(ARRAY_AGG(version::text ORDER BY version::text), ARRAY[]::text[]) AS versions,
    COALESCE(JSONB_OBJECT_AGG(
      version::text,
      JSONB_BUILD_OBJECT(
        'applied', applied,
        'total', total,
        'error', COALESCE(error, ''),
        'hash', 'h1:' || hash
      )
    ), '{}'::jsonb) AS details
  FROM atlas_schema_revisions.atlas_schema_revisions
  WHERE version::text >= '20261005000060'
)
SELECT jsonb_build_object(
  'applied_revisions', revision_set.versions,
  'revision_detail', revision_set.details,
  'migration_66_schema', jsonb_build_object(
    'columns', column_schema.columns,
    'constraint_count', (SELECT count(*) FROM pg_constraint, target WHERE conrelid = target.relation_oid),
    'primary_key', primary_key.columns,
    'foreign_key', foreign_key.value,
    'check', check_constraint.value,
    'changed_index', changed_index.value,
    'explicit_index_count', (
      SELECT count(*)
      FROM pg_index AS index_info
      LEFT JOIN pg_constraint AS constraint_info ON constraint_info.conindid = index_info.indexrelid
      CROSS JOIN target
      WHERE index_info.indrelid = target.relation_oid
        AND constraint_info.oid IS NULL
    )
  )
) || CASE
  WHEN '20261008000067' = ANY(revision_set.versions)
    THEN jsonb_build_object('migration_67_schema', migration_67_schema.value)
  ELSE '{}'::jsonb
END
FROM column_schema, primary_key, foreign_key, check_constraint, changed_index, migration_67_schema, revision_set;
"""
LIVE_SCHEMA_QUERY_SHA256 = hashlib.sha256(LIVE_SCHEMA_QUERY.encode("ascii")).hexdigest()


def normalize_check_expression(expression: Any) -> str:
    require(isinstance(expression, str), "schema_rejected")
    compact = re.sub(r"[\s()]", "", expression.lower())
    if compact.startswith("check"):
        compact = compact[len("check") :]
    allowed = {
        "peer_typebetween1and3andpeer_id>0",
        "peer_type>=1andpeer_type<=3andpeer_id>0",
    }
    require(compact in allowed, "schema_rejected")
    return compact


def validate_live_schema_observation(
    metadata: dict[str, Any], observation: Any, release_set: str
) -> None:
    expected_keys = {"applied_revisions", "revision_detail", "migration_66_schema"}
    if release_set in {"60-67", "60-69"}:
        expected_keys.add("migration_67_schema")
    require(isinstance(observation, dict) and set(observation) == expected_keys, "schema_rejected")
    applied = observation.get("applied_revisions")
    require(
        isinstance(applied, list)
        and all(isinstance(version, str) for version in applied)
        and applied == sorted(set(applied)),
        "schema_rejected",
    )
    require(applied == metadata.get("target_revisions"), "schema_rejected")
    revision_detail = observation.get("revision_detail")
    release = RELEASES[release_set]
    filename_by_version = {name[:14]: name for name in release["files"]}
    require(
        isinstance(revision_detail, dict)
        and set(revision_detail) == set(applied)
        and set(filename_by_version) == set(applied),
        "schema_rejected",
    )
    for version in applied:
        detail = revision_detail.get(version)
        require(
            isinstance(detail, dict)
            and set(detail) == {"applied", "total", "error", "hash"}
            and type(detail.get("applied")) is int
            and type(detail.get("total")) is int
            and detail["applied"] > 0
            and detail["applied"] == detail["total"]
            and detail.get("error") == ""
            and detail.get("hash") == release["atlas_pins"][filename_by_version[version]],
            "schema_rejected",
        )
    observed_schema = observation.get("migration_66_schema")
    expected_schema = metadata.get("migration_66_schema")
    require(isinstance(observed_schema, dict) and isinstance(expected_schema, dict), "schema_rejected")
    require(set(observed_schema) == set(expected_schema), "schema_rejected")
    observed_schema = copy.deepcopy(observed_schema)
    expected_schema = copy.deepcopy(expected_schema)
    observed_check = observed_schema.get("check")
    expected_check = expected_schema.get("check")
    require(isinstance(observed_check, dict) and isinstance(expected_check, dict), "schema_rejected")
    observed_check["expression"] = normalize_check_expression(observed_check.get("expression"))
    expected_check["expression"] = normalize_check_expression(expected_check.get("expression"))
    require(observed_schema == expected_schema, "schema_rejected")
    if release_set in {"60-67", "60-69"}:
        observed_67 = observation.get("migration_67_schema")
        expected_67 = metadata.get("migration_67_schema")
        require(isinstance(observed_67, dict) and isinstance(expected_67, dict), "schema_rejected")
        require(observed_67 == expected_67, "schema_rejected")


def validate_migration_schema(
    bundle: Path,
    checkout: Path,
    *,
    live_observation: Any = None,
    expected_dump_sha256: str | None = None,
    require_live_capture: bool = False,
) -> list[str]:
    release_set = select_migration_release(bundle)
    applied = validate_approved_migration_schema(bundle, checkout, release_set)
    metadata = read_json(bundle / "migrations.json")
    require(isinstance(metadata, dict), "schema_rejected")
    if live_observation is not None:
        validate_live_schema_observation(metadata, live_observation, release_set)

    live_capture = metadata.get("live_capture")
    baseline_capture = metadata.get("baseline_live_capture")
    require(not require_live_capture or isinstance(live_capture, dict), "schema_rejected")
    require(
        not require_live_capture or release_set not in {"60-67", "60-69"} or isinstance(baseline_capture, dict),
        "schema_rejected",
    )
    if live_capture is None:
        require(baseline_capture is None, "schema_rejected")
        return applied
    require(
        isinstance(live_capture, dict)
        and set(live_capture) == {
            "schema", "captured_at", "dump_sha256", "query_sha256",
            "query_output_sha256", "observed",
        }
        and live_capture.get("schema") == "teagram.live-migration-schema/v1"
        and live_capture.get("query_sha256") == LIVE_SCHEMA_QUERY_SHA256
        and isinstance(live_capture.get("query_output_sha256"), str)
        and re.fullmatch(r"[0-9a-f]{64}", live_capture["query_output_sha256"]) is not None,
        "schema_rejected",
    )
    captured_at = parse_time(live_capture.get("captured_at"))
    dump_sha = live_capture.get("dump_sha256")
    actual_dump_sha = sha256_file(bundle / "postgres.dump")
    require(
        isinstance(dump_sha, str)
        and re.fullmatch(r"[0-9a-f]{64}", dump_sha) is not None
        and dump_sha == actual_dump_sha
        and (expected_dump_sha256 is None or dump_sha == expected_dump_sha256),
        "schema_rejected",
    )
    validate_live_schema_observation(metadata, live_capture.get("observed"), release_set)
    if baseline_capture is not None:
        require(
            isinstance(baseline_capture, dict)
            and set(baseline_capture) == {
                "schema", "captured_at", "dump_sha256", "query_sha256",
                "query_output_sha256", "observed",
            }
            and baseline_capture.get("schema") == "teagram.live-migration-schema/v1"
            and baseline_capture.get("query_sha256") == LIVE_SCHEMA_QUERY_SHA256
            and isinstance(baseline_capture.get("query_output_sha256"), str)
            and re.fullmatch(r"[0-9a-f]{64}", baseline_capture["query_output_sha256"]) is not None
            and baseline_capture.get("dump_sha256") == dump_sha
            and baseline_capture.get("observed") == live_capture.get("observed"),
            "schema_rejected",
        )
        baseline_captured_at = parse_time(baseline_capture.get("captured_at"))
        require(baseline_captured_at <= captured_at, "schema_rejected")

    qualification_path = bundle / "qualification.json"
    recovery_path = bundle / "recovery.json"
    try:
        if recovery_path.is_file():
            recovery = read_json(recovery_path)
            freeze = recovery.get("freeze") if isinstance(recovery, dict) else None
            dump_value = recovery.get("dump", {}).get("captured_at") if isinstance(recovery, dict) else None
        elif qualification_path.is_file():
            qualification = read_json(qualification_path)
            freeze = qualification.get("freeze") if isinstance(qualification, dict) else None
            dump_value = freeze.get("dump_captured_at") if isinstance(freeze, dict) else None
        else:
            freeze = None
            dump_value = None
        captured_value = freeze.get("schema_captured_at") if isinstance(freeze, dict) else None
        baseline_captured_value = (
            freeze.get("baseline_schema_captured_at") if isinstance(freeze, dict) else None
        )
        freeze_window = (
            parse_time(freeze.get("started_at")),
            parse_time(freeze.get("held_at")),
        ) if isinstance(freeze, dict) else None
    except (AttributeError, TypeError):
        raise GateReject("schema_rejected")
    require(captured_value == live_capture.get("captured_at"), "schema_rejected")
    require(
        freeze_window is not None
        and freeze_window[0] <= captured_at <= freeze_window[1],
        "schema_rejected",
    )
    if baseline_capture is not None:
        baseline_captured_at = parse_time(baseline_capture.get("captured_at"))
        require(
            baseline_captured_value == baseline_capture.get("captured_at")
            and freeze_window[0] <= baseline_captured_at <= parse_time(dump_value)
            and baseline_captured_at <= captured_at,
            "schema_rejected",
        )
    return applied

def select_migration_release(bundle: Path) -> str:
    metadata = read_json(bundle / "migrations.json")
    require(isinstance(metadata, dict), "schema_rejected")
    target_revisions = metadata.get("target_revisions")
    release_set = next(
        (name for name, release in RELEASES.items() if target_revisions == release["revisions"]),
        None,
    )
    require(release_set is not None, "schema_rejected")
    return release_set


def parse_migration_atlas_sum(atlas_bytes: bytes) -> list[tuple[str, str]]:
    try:
        lines = atlas_bytes.decode("utf-8").splitlines()
    except UnicodeDecodeError as exc:
        raise GateReject("schema_rejected") from exc
    require(bool(lines) and re.fullmatch(r"h1:[A-Za-z0-9+/]{43}=", lines[0]) is not None, "schema_rejected")
    rows: list[tuple[str, str]] = []
    names: set[str] = set()
    previous_name = ""
    for line in lines[1:]:
        fields = line.split(" ")
        require(
            len(fields) == 2
            and re.fullmatch(r"[0-9]{14}_.+\.sql", fields[0]) is not None
            and re.fullmatch(r"h1:[A-Za-z0-9+/]{43}=", fields[1]) is not None
            and fields[0] not in names,
            "schema_rejected",
        )
        require(fields[0] > previous_name, "schema_rejected")
        previous_name = fields[0]
        names.add(fields[0])
        rows.append((fields[0], fields[1]))
    aggregate = hashlib.sha256()
    for name, digest in rows:
        aggregate.update(name.encode("utf-8"))
        aggregate.update(digest.removeprefix("h1:").encode("ascii"))
    require(lines[0] == "h1:" + base64.b64encode(aggregate.digest()).decode("ascii"), "schema_rejected")
    return rows


def read_pinned_snapshot_bytes(path: Path, max_size: int) -> bytes:
    try:
        info = path.lstat()
    except OSError as exc:
        raise GateReject("schema_rejected") from exc
    require(
        stat.S_ISREG(info.st_mode)
        and (stat.S_IMODE(info.st_mode) & 0o022) == 0
        and info.st_size <= max_size,
        "schema_rejected",
    )
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        fd = os.open(path, flags)
        with os.fdopen(fd, "rb") as stream:
            raw = stream.read(max_size + 1)
    except OSError as exc:
        raise GateReject("schema_rejected") from exc
    require(len(raw) <= max_size and len(raw) == info.st_size, "schema_rejected")
    return raw


def validate_pinned_migration_release(release_set: str) -> None:
    release = RELEASES[release_set]
    snapshot = Path(__file__).parent / "testdata" / f"release-{release_set}"
    try:
        info = snapshot.lstat()
    except OSError as exc:
        raise GateReject("schema_rejected") from exc
    require(
        stat.S_ISDIR(info.st_mode)
        and (stat.S_IMODE(info.st_mode) & 0o022) == 0,
        "schema_rejected",
    )
    try:
        names = {path.name for path in snapshot.iterdir()}
    except OSError as exc:
        raise GateReject("schema_rejected") from exc
    require(names == {"atlas.sum", *release["files"]}, "schema_rejected")
    atlas_bytes = read_pinned_snapshot_bytes(snapshot / "atlas.sum", 16 * 1024 * 1024)
    require(sha256_bytes(atlas_bytes) == release["atlas_sum_sha256"], "schema_rejected")
    snapshot_rows = parse_migration_atlas_sum(atlas_bytes)
    minimum = MIGRATIONS_60_62[0]
    selected_rows = [row for row in snapshot_rows if row[0][:14] >= minimum]
    expected_rows = [(name, release["atlas_pins"][name]) for name in release["files"]]
    require(selected_rows == expected_rows, "schema_rejected")
    for name, expected_sha in release["file_sha256"].items():
        require(
            sha256_bytes(read_pinned_snapshot_bytes(snapshot / name, 4 * 1024 * 1024)) == expected_sha,
            "schema_rejected",
        )


def validate_migration_66_schema(metadata: dict[str, Any]) -> None:
    schema = metadata.get("migration_66_schema")
    require(isinstance(schema, dict), "schema_rejected")
    require(
        set(schema)
        == {"columns", "constraint_count", "primary_key", "foreign_key", "check", "changed_index", "explicit_index_count"},
        "schema_rejected",
    )
    columns = schema.get("columns")
    require(isinstance(columns, dict) and set(columns) == set(EXPECTED_COLUMN_SCHEMA), "schema_rejected")
    for name, (expected_type, expected_not_null, expected_default) in EXPECTED_COLUMN_SCHEMA.items():
        column = columns.get(name)
        require(isinstance(column, dict) and set(column) == {"type", "not_null", "default"}, "schema_rejected")
        actual_type = column.get("type")
        if name == "changed_at" and actual_type == "timestamptz":
            actual_type = "timestamp with time zone"
        require(
            actual_type == expected_type
            and column.get("not_null") is expected_not_null
            and column.get("default") == expected_default,
            "schema_rejected",
        )
    require(schema.get("constraint_count") == 3 and schema.get("explicit_index_count") == 1, "schema_rejected")
    require(schema.get("primary_key") == ["owner_id", "peer_type", "peer_id"], "schema_rejected")
    foreign_key = schema.get("foreign_key")
    require(
        isinstance(foreign_key, dict)
        and foreign_key
        == {
            "columns": ["owner_id"],
            "referenced_table": "users",
            "referenced_columns": ["id"],
            "on_delete": "CASCADE",
            "validated": True,
        },
        "schema_rejected",
    )
    check = schema.get("check")
    require(isinstance(check, dict), "schema_rejected")
    require(check.get("name") == "user_dialog_unread_marks_peer" and check.get("validated") is True, "schema_rejected")
    normalize_check_expression(check.get("expression"))
    index = schema.get("changed_index")
    require(
        isinstance(index, dict)
        and index
        == {
            "name": "user_dialog_unread_marks_changed_idx",
            "columns": ["owner_id", "changed_at", "peer_type", "peer_id"],
            "valid": True,
            "ready": True,
            "unique": False,
        },
        "schema_rejected",
    )


def validate_migration_67_schema(metadata: dict[str, Any]) -> None:
    schema = metadata.get("migration_67_schema")
    require(
        isinstance(schema, dict) and set(schema) == {"table", "index_validity", "party_date_indexes"},
        "schema_rejected",
    )
    require(schema.get("table") == "public.secret_chats", "schema_rejected")
    index_validity = schema.get("index_validity")
    require(
        isinstance(index_validity, dict) and set(index_validity) == SECRET_CHATS_INDEX_NAMES_60_67,
        "schema_rejected",
    )
    require(all(valid is True for valid in index_validity.values()), "schema_rejected")

    expected_date_indexes = {
        "secret_chats_admin_date_idx": ["admin_id", "date"],
        "secret_chats_participant_date_idx": ["participant_id", "date"],
    }
    date_indexes = schema.get("party_date_indexes")
    require(
        isinstance(date_indexes, dict) and set(date_indexes) == set(expected_date_indexes),
        "schema_rejected",
    )
    expected_properties = {
        "access_method": "btree",
        "indisvalid": True,
        "indisready": True,
        "indislive": True,
        "indisunique": False,
        "indisprimary": False,
        "indpred": None,
        "indexprs": None,
        "indnatts": 2,
        "indnkeyatts": 2,
        "indoption": [0, 0],
    }
    boolean_properties = {"indisvalid", "indisready", "indislive", "indisunique", "indisprimary"}
    for name, columns in expected_date_indexes.items():
        index = date_indexes.get(name)
        require(
            isinstance(index, dict)
            and set(index) == {"columns", *expected_properties}
            and index.get("columns") == columns
            and all(index.get(key) is expected_properties[key] for key in boolean_properties)
            and type(index.get("indnatts")) is int
            and type(index.get("indnkeyatts")) is int
            and index.get("indnatts") == 2
            and index.get("indnkeyatts") == 2
            and isinstance(index.get("indoption"), list)
            and len(index["indoption"]) == 2
            and all(type(option) is int and option == 0 for option in index["indoption"])
            and index.get("access_method") == "btree"
            and index.get("indpred") is None
            and index.get("indexprs") is None,
            "schema_rejected",
        )


def validate_migration_68_schema(metadata: dict[str, Any]) -> None:
    schema = metadata.get("migration_68_schema")
    require(
        isinstance(schema, dict)
        and set(schema) == {"table", "index_names", "index_validity", "ownership_index"}
        and schema.get("table") == "public.files",
        "schema_rejected",
    )
    require(schema.get("index_names") == sorted(FILES_INDEX_NAMES_60_69), "schema_rejected")
    index_validity = schema.get("index_validity")
    require(
        isinstance(index_validity, dict)
        and set(index_validity) == FILES_INDEX_NAMES_60_69
        and all(valid is True for valid in index_validity.values()),
        "schema_rejected",
    )
    index = schema.get("ownership_index")
    expected_properties = {
        "access_method": "btree",
        "indisunique": True,
        "indisprimary": False,
        "indimmediate": True,
        "indisvalid": True,
        "indisready": True,
        "indislive": True,
        "indpred": None,
        "indexprs": None,
        "indnatts": 2,
        "indnkeyatts": 2,
        "indoption": [0, 0],
    }
    boolean_properties = {
        "indisunique",
        "indisprimary",
        "indimmediate",
        "indisvalid",
        "indisready",
        "indislive",
    }
    require(
        isinstance(index, dict)
        and set(index) == {"columns", *expected_properties}
        and index.get("columns") == ["id", "uploader_id"]
        and all(index.get(key) is expected_properties[key] for key in boolean_properties)
        and type(index.get("indnatts")) is int
        and type(index.get("indnkeyatts")) is int
        and index.get("indnatts") == 2
        and index.get("indnkeyatts") == 2
        and index.get("indoption") == [0, 0]
        and all(type(option) is int for option in index["indoption"])
        and index.get("access_method") == "btree"
        and index.get("indpred") is None
        and index.get("indexprs") is None,
        "schema_rejected",
    )


R69_CONSTRAINT_FIELDS = {
    "type",
    "validated",
    "columns",
    "referenced_table",
    "referenced_columns",
    "on_delete",
    "on_update",
    "match",
    "set_null_columns",
    "referenced_index",
    "check_expression",
}
R69_CONSTRAINTS = {
    "user_photos": {
        "user_photos_pkey": {"type": "p", "columns": ["user_id", "file_id"]},
        "user_photos_file_id_key": {"type": "u", "columns": ["file_id"]},
        "user_photos_user_id_client_file_id_key": {"type": "u", "columns": ["user_id", "client_file_id"]},
        "user_photos_user_id_fkey": {
            "type": "f",
            "columns": ["user_id"],
            "referenced_table": "public.users",
            "referenced_columns": ["id"],
            "on_delete": "CASCADE",
            "on_update": "NO ACTION",
            "match": "SIMPLE",
            "set_null_columns": [],
            "referenced_index": "users_pkey",
        },
        "user_photos_file_owned_by_owner": {
            "type": "f",
            "columns": ["file_id", "user_id"],
            "referenced_table": "public.files",
            "referenced_columns": ["id", "uploader_id"],
            "on_delete": "RESTRICT",
            "on_update": "NO ACTION",
            "match": "SIMPLE",
            "set_null_columns": [],
            "referenced_index": "files_id_uploader_id_key",
        },
    },
    "profile_photo_state": {
        "profile_photo_state_pkey": {"type": "p", "columns": ["user_id"]},
        "profile_photo_state_mutation_revision_check": {"type": "c"},
        "profile_photo_state_user_id_fkey": {
            "type": "f",
            "columns": ["user_id"],
            "referenced_table": "public.users",
            "referenced_columns": ["id"],
            "on_delete": "CASCADE",
            "on_update": "NO ACTION",
            "match": "SIMPLE",
            "set_null_columns": [],
            "referenced_index": "users_pkey",
        },
        "profile_photo_state_current_is_own_gallery_entry": {
            "type": "f",
            "columns": ["user_id", "current_file_id"],
            "referenced_table": "public.user_photos",
            "referenced_columns": ["user_id", "file_id"],
            "on_delete": "RESTRICT",
            "on_update": "NO ACTION",
            "match": "SIMPLE",
            "set_null_columns": [],
            "referenced_index": "user_photos_pkey",
        },
    },
    "profile_upload_receipt": {
        "profile_upload_receipt_pkey": {"type": "p", "columns": ["user_id", "client_file_id"]},
        "profile_upload_receipt_state_check": {"type": "c"},
        "profile_upload_receipt_request_size_check": {"type": "c"},
        "profile_upload_receipt_part_count_check": {"type": "c"},
        "profile_upload_receipt_payload_digest_check": {"type": "c"},
        "profile_upload_receipt_media_mode_check": {"type": "c"},
        "profile_upload_receipt_user_id_fkey": {
            "type": "f",
            "columns": ["user_id"],
            "referenced_table": "public.users",
            "referenced_columns": ["id"],
            "on_delete": "CASCADE",
            "on_update": "NO ACTION",
            "match": "SIMPLE",
            "set_null_columns": [],
            "referenced_index": "users_pkey",
        },
        "profile_upload_receipt_file_owned_by_owner": {
            "type": "f",
            "columns": ["file_id", "user_id"],
            "referenced_table": "public.files",
            "referenced_columns": ["id", "uploader_id"],
            "on_delete": "SET NULL",
            "on_update": "NO ACTION",
            "match": "SIMPLE",
            "set_null_columns": ["file_id"],
            "referenced_index": "files_id_uploader_id_key",
        },
    },
    "profile_delete_operation": {
        "profile_delete_operation_pkey": {"type": "p", "columns": ["user_id", "auth_key_id", "session_id", "msg_id"]},
        "profile_delete_operation_operation_key_key": {"type": "u", "columns": ["operation_key"]},
        "profile_delete_operation_operation_key_check": {"type": "c"},
        "profile_delete_operation_clear_revision_check": {"type": "c"},
        "profile_delete_operation_user_id_fkey": {
            "type": "f",
            "columns": ["user_id"],
            "referenced_table": "public.users",
            "referenced_columns": ["id"],
            "on_delete": "CASCADE",
            "on_update": "NO ACTION",
            "match": "SIMPLE",
            "set_null_columns": [],
            "referenced_index": "users_pkey",
        },
    },
}
R69_COLUMNS = {
    "user_photos": {
        "user_id": ("bigint", True, None),
        "file_id": ("bigint", True, None),
        "client_file_id": ("bigint", True, None),
        "created_at": ("timestamp with time zone", True, "now()"),
    },
    "profile_photo_state": {
        "user_id": ("bigint", True, None),
        "current_file_id": ("bigint", False, None),
        "mutation_revision": ("bigint", True, "0"),
    },
    "profile_upload_receipt": {
        "user_id": ("bigint", True, None),
        "client_file_id": ("bigint", True, None),
        "file_id": ("bigint", False, None),
        "state": ("smallint", True, None),
        "request_size": ("bigint", True, None),
        "part_count": ("integer", True, None),
        "payload_digest": ("bytea", True, None),
        "media_mode": ("text", True, None),
        "created_at": ("timestamp with time zone", True, "now()"),
        "updated_at": ("timestamp with time zone", True, "now()"),
    },
    "profile_delete_operation": {
        "user_id": ("bigint", True, None),
        "auth_key_id": ("bigint", True, None),
        "session_id": ("bigint", True, None),
        "msg_id": ("bigint", True, None),
        "operation_key": ("bytea", True, None),
        "target_file_id": ("bigint", False, None),
        "client_file_id": ("bigint", False, None),
        "clear_revision": ("bigint", False, None),
        "created_at": ("timestamp with time zone", True, "now()"),
    },
}
R69_INDEX_NAMES = {
    "user_photos": ["user_photos_file_id_key", "user_photos_pkey", "user_photos_user_id_client_file_id_key"],
    "profile_photo_state": ["profile_photo_state_pkey"],
    "profile_upload_receipt": ["profile_upload_receipt_pkey"],
    "profile_delete_operation": ["profile_delete_operation_operation_key_key", "profile_delete_operation_pkey"],
}


def validate_migration_69_schema(metadata: dict[str, Any]) -> None:
    schema = metadata.get("migration_69_schema")
    require(isinstance(schema, dict) and set(schema) == {"tables"}, "schema_rejected")
    tables = schema.get("tables")
    require(isinstance(tables, dict) and set(tables) == set(R69_COLUMNS), "schema_rejected")
    for table_name, expected_columns in R69_COLUMNS.items():
        table = tables.get(table_name)
        require(
            isinstance(table, dict)
            and set(table) == {"table", "columns", "constraints", "index_names", "index_validity"}
            and table.get("table") == f"public.{table_name}",
            "schema_rejected",
        )
        columns = table.get("columns")
        require(isinstance(columns, dict) and set(columns) == set(expected_columns), "schema_rejected")
        for column_name, (expected_type, expected_not_null, expected_default) in expected_columns.items():
            column = columns.get(column_name)
            require(
                isinstance(column, dict)
                and set(column) == {"type", "not_null", "default"}
                and column.get("type") == expected_type
                and column.get("not_null") is expected_not_null
                and column.get("default") == expected_default,
                "schema_rejected",
            )

        constraints = table.get("constraints")
        expected_constraints = R69_CONSTRAINTS[table_name]
        require(
            isinstance(constraints, dict) and set(constraints) == set(expected_constraints),
            "schema_rejected",
        )
        for name, expected in expected_constraints.items():
            constraint = constraints.get(name)
            require(
                isinstance(constraint, dict)
                and set(constraint) == R69_CONSTRAINT_FIELDS
                and constraint.get("type") == expected["type"]
                and constraint.get("validated") is True,
                "schema_rejected",
            )
            if expected["type"] in {"p", "u", "f"}:
                require(constraint.get("columns") == expected["columns"], "schema_rejected")
            else:
                require(
                    isinstance(constraint.get("columns"), list)
                    and all(isinstance(column, str) for column in constraint["columns"])
                    and isinstance(constraint.get("check_expression"), str),
                    "schema_rejected",
                )
            for field in (
                "referenced_table",
                "referenced_columns",
                "on_delete",
                "on_update",
                "match",
                "referenced_index",
            ):
                if field not in expected:
                    require(constraint.get(field) is None, "schema_rejected")
            if expected["type"] != "f":
                require(constraint.get("set_null_columns") == [], "schema_rejected")
            for field, expected_value in expected.items():
                require(constraint.get(field) == expected_value, "schema_rejected")
            if expected["type"] != "c":
                require(constraint.get("check_expression") is None, "schema_rejected")

        expected_index_names = R69_INDEX_NAMES[table_name]
        require(table.get("index_names") == expected_index_names, "schema_rejected")
        index_validity = table.get("index_validity")
        require(
            isinstance(index_validity, dict)
            and set(index_validity) == set(expected_index_names)
            and all(valid is True for valid in index_validity.values()),
            "schema_rejected",
        )


R70_CONSTRAINT_FIELDS = R69_CONSTRAINT_FIELDS | {"deferrable", "initially_deferred"}
R70_CONSTRAINTS = {
    "erasure_outbox": {
        "erasure_outbox_operation_key_check": {"type": "c"},
        "erasure_outbox_epoch_check": {"type": "c"},
        "erasure_outbox_stream_id_check": {"type": "c"},
        "erasure_outbox_seq_check": {"type": "c"},
        "erasure_outbox_kind_check": {"type": "c"},
        "erasure_outbox_record_check": {"type": "c"},
        "erasure_outbox_pkey": {"type": "p", "columns": ["epoch", "stream_id", "seq"]},
        "erasure_outbox_operation_key_unique": {"type": "u", "columns": ["operation_key"]},
    },
    "erasure_epoch": {
        "erasure_epoch_epoch_check": {"type": "c"},
        "erasure_epoch_lineage_id_check": {"type": "c"},
        "erasure_epoch_pkey": {"type": "p", "columns": ["epoch", "lineage_id"]},
    },
    "erasure_epoch_completion": {
        "erasure_epoch_completion_epoch_check": {"type": "c"},
        "erasure_epoch_completion_lineage_id_check": {"type": "c"},
        "erasure_epoch_completion_pkey": {"type": "p", "columns": ["epoch", "lineage_id"]},
        "erasure_epoch_completion_marker_exists": {
            "type": "f",
            "columns": ["epoch", "lineage_id"],
            "referenced_table": "public.erasure_epoch",
            "referenced_columns": ["epoch", "lineage_id"],
            "on_delete": "RESTRICT",
            "on_update": "NO ACTION",
            "match": "SIMPLE",
            "set_null_columns": [],
            "referenced_index": "erasure_epoch_pkey",
        },
    },
}
R70_COLUMNS = {
    "erasure_outbox": {
        "operation_key": "bytea",
        "epoch": "bigint",
        "stream_id": "bytea",
        "seq": "bigint",
        "kind": "smallint",
        "record": "bytea",
    },
    "erasure_epoch": {"epoch": "bigint", "lineage_id": "bytea"},
    "erasure_epoch_completion": {"epoch": "bigint", "lineage_id": "bytea"},
}
R70_INDEX_NAMES = {
    "erasure_outbox": ["erasure_outbox_operation_key_unique", "erasure_outbox_pkey"],
    "erasure_epoch": ["erasure_epoch_pkey"],
    "erasure_epoch_completion": ["erasure_epoch_completion_pkey"],
}


def validate_migration_70_schema(metadata: dict[str, Any]) -> None:
    schema = metadata.get("migration_70_schema")
    require(
        isinstance(schema, dict)
        and set(schema) == {"tables", "inbound_foreign_keys", "user_triggers"},
        "schema_rejected",
    )
    tables = schema.get("tables")
    require(isinstance(tables, dict) and set(tables) == set(R70_COLUMNS), "schema_rejected")
    for table_name, expected_columns in R70_COLUMNS.items():
        table = tables.get(table_name)
        require(
            isinstance(table, dict)
            and set(table) == {"table", "columns", "constraints", "index_names", "index_validity"}
            and table.get("table") == f"public.{table_name}",
            "schema_rejected",
        )
        columns = table.get("columns")
        require(isinstance(columns, dict) and set(columns) == set(expected_columns), "schema_rejected")
        for column_name, expected_type in expected_columns.items():
            column = columns.get(column_name)
            require(
                isinstance(column, dict)
                and set(column) == {"type", "not_null", "default", "identity", "generated", "sequence"}
                and column.get("type") == expected_type
                and column.get("not_null") is True
                and column.get("default") is None
                and column.get("identity") == ""
                and column.get("generated") == ""
                and column.get("sequence") is False,
                "schema_rejected",
            )

        constraints = table.get("constraints")
        expected_constraints = R70_CONSTRAINTS[table_name]
        require(
            isinstance(constraints, dict) and set(constraints) == set(expected_constraints),
            "schema_rejected",
        )
        for name, expected in expected_constraints.items():
            constraint = constraints.get(name)
            require(
                isinstance(constraint, dict)
                and set(constraint) == R70_CONSTRAINT_FIELDS
                and constraint.get("type") == expected["type"]
                and constraint.get("validated") is True,
                "schema_rejected",
            )
            require(
                constraint.get("deferrable") is False
                and constraint.get("initially_deferred") is False,
                "schema_rejected",
            )
            if expected["type"] in {"p", "u", "f"}:
                require(constraint.get("columns") == expected["columns"], "schema_rejected")
            else:
                require(
                    isinstance(constraint.get("columns"), list)
                    and all(isinstance(column, str) for column in constraint["columns"])
                    and isinstance(constraint.get("check_expression"), str),
                    "schema_rejected",
                )
            for field in (
                "referenced_table",
                "referenced_columns",
                "on_delete",
                "on_update",
                "match",
                "referenced_index",
            ):
                if field not in expected:
                    require(constraint.get(field) is None, "schema_rejected")
            if expected["type"] != "f":
                require(constraint.get("set_null_columns") == [], "schema_rejected")
            for field, expected_value in expected.items():
                require(constraint.get(field) == expected_value, "schema_rejected")
            if expected["type"] != "c":
                require(constraint.get("check_expression") is None, "schema_rejected")

        expected_index_names = R70_INDEX_NAMES[table_name]
        require(table.get("index_names") == expected_index_names, "schema_rejected")
        index_validity = table.get("index_validity")
        require(
            isinstance(index_validity, dict)
            and set(index_validity) == set(expected_index_names)
            and all(valid is True for valid in index_validity.values()),
            "schema_rejected",
        )

    inbound_foreign_keys = schema.get("inbound_foreign_keys")
    require(isinstance(inbound_foreign_keys, list) and not inbound_foreign_keys, "schema_rejected")
    user_triggers = schema.get("user_triggers")
    require(
        isinstance(user_triggers, dict)
        and set(user_triggers) == set(R70_COLUMNS)
        and all(isinstance(triggers, list) and not triggers for triggers in user_triggers.values()),
        "schema_rejected",
    )


def validate_inert_surfaces(metadata: dict[str, Any], expected_surfaces: set[str] = INERT_SURFACES) -> None:
    surfaces = metadata.get("inert_surfaces")
    require(
        isinstance(surfaces, dict)
        and set(surfaces) == expected_surfaces
        and all(type(value) is bool for value in surfaces.values()),
        "schema_rejected",
    )
    require(all(value is False for value in surfaces.values()), "reference_coverage")


def validate_approved_migration_schema(bundle: Path, checkout: Path, release_set: str) -> list[str]:
    metadata = read_json(bundle / "migrations.json")
    require(isinstance(metadata, dict), "schema_rejected")
    require(select_migration_release(bundle) == release_set, "schema_rejected")
    release = RELEASES[release_set]
    if release_set in {"60-67", "60-69", "60-70"}:
        expected_keys = {
            "release_set",
            "baseline_revisions",
            "revision_rows",
            "target_revisions",
            "approved_revision_set_exact",
            "migration_66_present",
            "migration_66_schema",
            "revision_detail",
        }
        if release_set in {"60-67", "60-69", "60-70"}:
            expected_keys.update({"migration_67_present", "migration_67_schema"})
        if release_set in {"60-69", "60-70"}:
            expected_keys.update(
                {
                    "migration_68_present",
                    "migration_68_schema",
                    "migration_69_present",
                    "migration_69_schema",
                    "inert_surfaces",
                }
            )
        if release_set == "60-70":
            expected_keys.update({"migration_70_present", "migration_70_schema"})
        require(
            set(metadata) in (
                expected_keys,
                expected_keys | {"live_capture"},
                expected_keys | {"live_capture", "baseline_live_capture"},
            ),
            "schema_rejected",
        )
        require(metadata.get("release_set") == release_set, "schema_rejected")
        require(metadata.get("baseline_revisions") == release["revisions"], "schema_rejected")
    else:
        require(metadata.get("release_set", "60-66") == "60-66", "schema_rejected")
        require(metadata.get("baseline_revisions") == MIGRATIONS_60_62, "schema_rejected")

    revision_rows = metadata.get("revision_rows")
    require(isinstance(revision_rows, dict), "schema_rejected")
    require(all(isinstance(version, str) and isinstance(present, bool) for version, present in revision_rows.items()), "schema_rejected")
    if release_set in {"60-67", "60-69", "60-70"}:
        require(set(revision_rows) == set(release["revisions"]), "schema_rejected")
        require(all(revision_rows[version] is True for version in release["revisions"]), "schema_rejected")
        applied = release["revisions"]
    else:
        applied = sorted(version for version, present in revision_rows.items() if present and version >= MIGRATIONS_60_66[0])
    require(metadata.get("target_revisions") == applied, "schema_rejected")
    exact = applied == release["revisions"]
    require(metadata.get("approved_revision_set_exact") is exact, "schema_rejected")
    require(metadata.get("migration_66_present") is revision_rows.get(MIGRATION_66, False), "schema_rejected")
    require(exact and metadata.get("migration_66_present") is True, "schema_rejected")
    if release_set in {"60-67", "60-69", "60-70"}:
        require(metadata.get("migration_67_present") is revision_rows.get(MIGRATION_67, False), "schema_rejected")
        require(metadata.get("migration_67_present") is True, "schema_rejected")
        revision_detail = metadata.get("revision_detail")
        revision_detail_keys = {"applied", "total", "error", "hash"}
        if release_set == "60-70":
            revision_detail_keys.update({"error_stmt_empty", "partial_hashes_empty"})
        require(
            isinstance(revision_detail, dict) and set(revision_detail) == set(release["revisions"]),
            "schema_rejected",
        )
        for filename, version in zip(release["files"], release["revisions"], strict=True):
            detail = revision_detail.get(version)
            require(
                isinstance(detail, dict)
                and set(detail) == revision_detail_keys
                and isinstance(detail.get("applied"), int)
                and not isinstance(detail.get("applied"), bool)
                and detail["applied"] > 0
                and isinstance(detail.get("total"), int)
                and not isinstance(detail.get("total"), bool)
                and detail["applied"] == detail["total"]
                and detail.get("error") == ""
                and detail.get("hash") == release["atlas_pins"][filename],
                "schema_rejected",
            )
            if release_set == "60-70":
                require(
                    detail.get("error_stmt_empty") is True
                    and detail.get("partial_hashes_empty") is True,
                    "schema_rejected",
                )
        if release_set in {"60-69", "60-70"}:
            require(metadata.get("migration_68_present") is revision_rows.get(MIGRATION_68, False), "schema_rejected")
            require(metadata.get("migration_69_present") is revision_rows.get(MIGRATION_69, False), "schema_rejected")
            require(metadata.get("migration_68_present") is True and metadata.get("migration_69_present") is True, "schema_rejected")
        if release_set == "60-70":
            require(metadata.get("migration_70_present") is revision_rows.get(MIGRATION_70, False), "schema_rejected")
            require(metadata.get("migration_70_present") is True, "schema_rejected")

    validate_pinned_migration_release(release_set)
    migrations_dir = checkout / "migrations"
    try:
        migrations_info = migrations_dir.lstat()
    except OSError as exc:
        raise GateReject("schema_rejected") from exc
    require(
        stat.S_ISDIR(migrations_info.st_mode)
        and not stat.S_ISLNK(migrations_info.st_mode),
        "schema_rejected",
    )
    try:
        migration_paths = list(migrations_dir.iterdir())
    except OSError as exc:
        raise GateReject("schema_rejected") from exc
    atlas_bytes = read_regular_bytes(migrations_dir / "atlas.sum", 16 * 1024 * 1024)
    if release_set in {"60-69", "60-70"}:
        require(sha256_bytes(atlas_bytes) == release["atlas_sum_sha256"], "schema_rejected")
    atlas_rows = parse_migration_atlas_sum(atlas_bytes)
    atlas_names = {name for name, _digest in atlas_rows}
    physical_names: set[str] = set()
    for path in migration_paths:
        if path.name == "atlas.sum":
            continue
        require(re.fullmatch(r"[0-9]{14}_.+\.sql", path.name) is not None, "schema_rejected")
        try:
            path_info = path.lstat()
        except OSError as exc:
            raise GateReject("schema_rejected") from exc
        require(stat.S_ISREG(path_info.st_mode) and path_info.st_uid == 0, "schema_rejected")
        physical_names.add(path.name)
    require(physical_names == atlas_names, "schema_rejected")
    atlas_hash = hashlib.sha256()
    for name, expected_digest in atlas_rows:
        atlas_hash.update(name.encode("utf-8"))
        migration_bytes = read_regular_bytes(migrations_dir / name, 16 * 1024 * 1024)
        atlas_hash.update(migration_bytes)
        actual_digest = "h1:" + base64.b64encode(atlas_hash.digest()).decode("ascii")
        require(actual_digest == expected_digest, "schema_rejected")
    minimum_version = MIGRATIONS_60_62[0]
    maximum_version = release["revisions"][-1]
    selected_rows = [
        row for row in atlas_rows
        if minimum_version <= row[0][:14] <= maximum_version
    ]
    expected_atlas_rows = [(name, release["atlas_pins"][name]) for name in release["files"]]
    require(selected_rows == expected_atlas_rows, "schema_rejected")
    if release_set == "60-69":
        post_baseline_rows = [row for row in atlas_rows if row[0][:14] >= minimum_version]
        require(post_baseline_rows == expected_atlas_rows, "schema_rejected")
        require(
            {name for name in physical_names if name[:14] >= minimum_version}
            == set(release["files"]),
            "schema_rejected",
        )
    for name, expected_sha in release["file_sha256"].items():
        actual_sha = sha256_bytes(read_regular_bytes(migrations_dir / name, 4 * 1024 * 1024))
        require(actual_sha == expected_sha, "schema_rejected")

    validate_migration_66_schema(metadata)
    if release_set in {"60-67", "60-69", "60-70"}:
        validate_migration_67_schema(metadata)
    if release_set in {"60-69", "60-70"}:
        query_digest = INERT_SURFACES_QUERY_SHA256
        query_name = "rustfs-inert-surfaces.sql"
        surfaces = INERT_SURFACES
        if release_set == "60-70":
            query_digest = R70_INERT_SURFACES_QUERY_SHA256
            query_name = "rustfs-r70-inert-surfaces.sql"
            surfaces = R70_INERT_SURFACES
        query_path = Path(__file__).with_name(query_name)
        try:
            query_hash = sha256_bytes(query_path.read_bytes())
        except OSError as exc:
            raise GateReject("schema_rejected") from exc
        require(query_hash == query_digest, "schema_rejected")
        validate_migration_68_schema(metadata)
        validate_migration_69_schema(metadata)
        if release_set == "60-70":
            validate_migration_70_schema(metadata)
        validate_inert_surfaces(metadata, surfaces)
    return applied


def qualify(bundle: Path, checkout: Path, pre_copy: bool = False) -> dict[str, Any]:
    require_bundle_dir(bundle, pre_copy=pre_copy)
    try:
        checkout_info = checkout.lstat()
    except OSError as exc:
        raise GateReject("bundle_invalid") from exc
    require(stat.S_ISDIR(checkout_info.st_mode) and not stat.S_ISLNK(checkout_info.st_mode), "bundle_invalid")
    candidate_root = checkout.resolve(strict=True)

    qualification = read_json(bundle / "qualification.json")
    require(isinstance(qualification, dict) and qualification.get("schema") == SCHEMA, "bundle_invalid")
    secret_values = validate_env(bundle)
    validate_override(bundle)
    validate_candidate_files(bundle, candidate_root, secret_values)

    baseline = read_json(bundle / "baseline-compose.json")
    candidate = read_json(bundle / "candidate-compose.json")
    validate_baseline_env_provenance(bundle, qualification, baseline)
    compose_inputs = validate_candidate_compose_binding(qualification, candidate_root, candidate)
    resolved_candidate = resolve_candidate_compose(candidate_root)
    require(
        canonical_json_sha256(resolved_candidate) == canonical_json_sha256(candidate),
        "compose_binding_mismatch",
    )
    target_volumes, _ = check_candidate_services(baseline, candidate, secret_values, candidate_root)
    require(target_volumes["tgblobs"] == qualification.get("source_volume"), "source_identity")

    release_set = select_migration_release(bundle)
    validate_freeze(
        bundle,
        qualification,
        target_volumes["tgblobs"],
        secret_values["TG_BLOB_S3_ACCESS_KEY_ID"],
        release_set,
    )
    validate_manifests(
        bundle,
        qualification,
        target_volumes["tgblobs"],
        release_set,
        pre_copy=pre_copy,
    )
    applied_revisions = validate_migration_schema(bundle, candidate_root)
    return {
        "release_set": release_set,
        "applied_revisions": applied_revisions,
        "migrations_sha256": sha256_file(bundle / "migrations.json"),
    }


def main(argv: list[str]) -> int:
    if len(argv) != 4 or argv[1] not in ("check", "pre-copy"):
        print("usage: qualify-rustfs-transition.sh check|pre-copy PRIVATE_BUNDLE_DIR CANDIDATE_CHECKOUT", file=sys.stderr)
        return 64
    try:
        pre_copy = argv[1] == "pre-copy"
        result = qualify(Path(argv[2]), Path(argv[3]), pre_copy=pre_copy)
    except GateReject as exc:
        print(f"gate_result=reject reason={exc.reason}", file=sys.stderr)
        return 1
    except Exception:
        print("gate_result=reject reason=evidence_invalid", file=sys.stderr)
        return 1
    print(
        f"gate_result={'pre-copy-pass' if pre_copy else 'pass'}"
        f" release_set={result['release_set']}"
        f" applied_versions={','.join(result['applied_revisions'])}"
        f" migrations_sha256={result['migrations_sha256']}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
