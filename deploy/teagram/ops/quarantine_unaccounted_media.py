#!/usr/bin/env python3
"""Quarantine exact unaccounted plaintext objects from a verified inventory."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import stat
import subprocess
import sys
import uuid
from dataclasses import dataclass
from typing import Callable, Iterable, Protocol


INVENTORY_SCHEMA = "teagram.blob-encryption-inventory/v1"
PROGRESS_SCHEMA = "teagram.blob-encryption-progress/v1"
MANIFEST_SCHEMA = "teagram.media-quarantine/v1"
POSTGRES_CONTAINER = "teagram-postgres-1"
MC_SERVICE = "rustfs-init"
ENVELOPE_INVENTORY_SERVICE = "blob-key-rotate"
ENVELOPE_INVENTORY_BINARY = "/usr/local/bin/blob-inventory"
S3_ALIAS = "local"
S3_BUCKET = "teagram"
REFERENCE_INVENTORY_SQL = """COPY (
  SELECT kind, object_id, key, logical_bytes
  FROM (
    SELECT 'file'::text AS kind, f.id::text AS object_id,
      lpad(to_hex((f.id & 255)::integer), 2, '0') || '/' || f.id::text AS key,
      f.size::bigint AS logical_bytes
    FROM files f WHERE f.stored = true
    UNION ALL
    SELECT 'part'::text AS kind, up.blob_key AS object_id, up.blob_key AS key,
      up.size::bigint AS logical_bytes
    FROM upload_parts up WHERE up.blob_key <> ''
  ) refs
  ORDER BY key
) TO STDOUT;
"""


class QuarantineError(Exception):
    """A safe-to-print operation error."""


@dataclass(frozen=True)
class ObjectMeta:
    size: int
    modified: str
    age_bucket: str


@dataclass(frozen=True)
class Candidate:
    key: str
    kind: str
    object_id: str
    size: int
    modified: str
    category: str
    age_bucket: str


@dataclass(frozen=True)
class Reference:
    kind: str
    object_id: str
    key: str
    logical_bytes: int


@dataclass(frozen=True)
class InventoryContract:
    candidates: dict[str, tuple[str, str]]
    candidate_physical_bytes: int
    valid_envelope_count: int
    no_valid_envelope_count: int
    missing_count: int
    encrypted_unaccounted_count: int
    inventory_sha256: str


@dataclass(frozen=True)
class Snapshot:
    contract: InventoryContract
    raw_objects: dict[str, ObjectMeta]
    encrypted_objects: dict[str, ObjectMeta]
    references: dict[str, Reference]
    journal_keys: frozenset[str]
    reference_sha256: str
    candidates: dict[str, Candidate]


class Storage(Protocol):
    def copy_from(self, key: str, destination: pathlib.Path) -> None: ...

    def copy_to(self, source: pathlib.Path, key: str) -> None: ...

    def hash_remote(self, key: str) -> str: ...

    def remove(self, key: str) -> None: ...


def fail(message: str) -> None:
    raise QuarantineError(message)


def validate_key(key: object) -> str:
    if not isinstance(key, str) or not key or len(key.encode("utf-8")) > 1024:
        fail("inventory contains an invalid object key")
    allowed = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-/"
    if any(character not in allowed for character in key):
        fail("inventory contains an unsupported object key")
    if any(segment in ("", ".", "..") for segment in key.split("/")):
        fail("inventory contains an unsafe object key")
    return key


def private_file(path: pathlib.Path) -> None:
    try:
        info = path.lstat()
    except OSError:
        fail("private operation evidence is unavailable")
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) != 0o600:
        fail("private operation evidence has unsafe permissions")


def private_directory(path: pathlib.Path) -> None:
    try:
        info = path.lstat()
    except OSError:
        fail("quarantine directory is unavailable")
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) != 0o700:
        fail("quarantine directory has unsafe permissions")


def read_json(path: pathlib.Path) -> object:
    private_file(path)
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError):
        fail("private operation JSON is invalid")


def validate_fresh_envelope_inventory(path: pathlib.Path, referenced_count: int) -> None:
    if not isinstance(referenced_count, int) or isinstance(referenced_count, bool) or referenced_count < 0:
        fail("current database reference count is invalid")
    root = read_json(path)
    if not isinstance(root, dict):
        fail("fresh encrypted envelope inventory is invalid")
    classes = root.get("classes")
    if not isinstance(classes, dict):
        fail("fresh encrypted envelope inventory is incomplete")

    def class_count(name: str) -> int:
        value = classes.get(name)
        count = value.get("count") if isinstance(value, dict) else None
        if not isinstance(count, int) or isinstance(count, bool) or count < 0:
            fail("fresh encrypted envelope inventory counts are invalid")
        return count

    valid = class_count("valid_envelope")
    invalid = class_count("no_valid_envelope")
    missing = class_count("missing")
    unaccounted = class_count("unaccounted")
    unversioned = root.get("unversioned_envelope_references")
    if not isinstance(unversioned, int) or isinstance(unversioned, bool) or unversioned < 0:
        fail("fresh encrypted envelope inventory counts are invalid")
    if valid != referenced_count or invalid != 0 or missing != 0 or unaccounted != 0 or unversioned != 0:
        fail("fresh encrypted envelope inventory found invalid or missing references")


def load_contract(inventory_path: pathlib.Path, expected_sha256: str, referenced_count: int) -> InventoryContract:
    private_file(inventory_path)
    try:
        content = inventory_path.read_bytes()
    except OSError:
        fail("latest encrypted inventory is unavailable")
    actual_sha256 = hashlib.sha256(content).hexdigest()
    if actual_sha256 != expected_sha256 or not re.fullmatch(r"[0-9a-f]{64}", expected_sha256):
        fail("latest encrypted inventory no longer matches authority")
    try:
        root = json.loads(content)
    except (UnicodeError, json.JSONDecodeError):
        fail("latest encrypted inventory is invalid")
    if not isinstance(root, dict) or root.get("schema") != INVENTORY_SCHEMA:
        fail("latest encrypted inventory schema is invalid")

    plaintext = root.get("plaintext")
    encrypted = root.get("encrypted")
    if not isinstance(plaintext, dict) or not isinstance(encrypted, dict):
        fail("latest encrypted inventory is incomplete")
    plaintext_classes = plaintext.get("classes")
    encrypted_classes = encrypted.get("classes")
    if not isinstance(plaintext_classes, dict) or not isinstance(encrypted_classes, dict):
        fail("latest encrypted inventory classes are invalid")

    stray_class = plaintext_classes.get("unaccounted")
    if not isinstance(stray_class, dict):
        fail("latest inventory has no unaccounted plaintext class")
    items = stray_class.get("ids")
    count = stray_class.get("count")
    physical_bytes = stray_class.get("physical_bytes")
    if not isinstance(items, list) or not isinstance(count, int) or count < 0 or count != len(items):
        fail("latest inventory unaccounted count does not match its keys")
    if not isinstance(physical_bytes, int) or physical_bytes < 0:
        fail("latest inventory unaccounted byte total is invalid")
    candidates: dict[str, tuple[str, str]] = {}
    for item in items:
        if not isinstance(item, dict):
            fail("latest inventory contains an invalid object record")
        key = validate_key(item.get("key"))
        kind = item.get("kind")
        object_id = item.get("id")
        if kind not in ("file", "part", "unknown") or not isinstance(object_id, str) or not object_id:
            fail("latest inventory contains an invalid object identity")
        if key == "encrypted" or key.startswith("encrypted/") or key in candidates:
            fail("latest inventory contains a duplicate or encrypted plaintext key")
        candidates[key] = (kind, object_id)
    if count == 0:
        fail("latest inventory contains no unaccounted plaintext objects")

    if not isinstance(referenced_count, int) or referenced_count < 0:
        fail("encrypted authority reference count is invalid")

    def class_count(name: str) -> int:
        value = encrypted_classes.get(name)
        if not isinstance(value, dict) or not isinstance(value.get("count"), int) or value["count"] < 0:
            fail("encrypted inventory count is invalid")
        return value["count"]

    valid_envelope_count = class_count("valid_envelope")
    no_valid_envelope_count = class_count("no_valid_envelope")
    missing_count = class_count("missing")
    encrypted_unaccounted_count = class_count("unaccounted")
    if valid_envelope_count != referenced_count or no_valid_envelope_count != 0 or missing_count != 0:
        fail("encrypted inventory does not verify every referenced object")

    return InventoryContract(
        candidates=candidates,
        candidate_physical_bytes=physical_bytes,
        valid_envelope_count=valid_envelope_count,
        no_valid_envelope_count=no_valid_envelope_count,
        missing_count=missing_count,
        encrypted_unaccounted_count=encrypted_unaccounted_count,
        inventory_sha256=expected_sha256,
    )


def age_bucket(value: str, now: dt.datetime) -> str:
    try:
        modified = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except (ValueError, AttributeError):
        fail("object listing has an invalid modification time")
    if modified.tzinfo is None:
        fail("object listing has a timezone-free modification time")
    age_seconds = (now - modified.astimezone(dt.timezone.utc)).total_seconds()
    if age_seconds < 0:
        return "future"
    if age_seconds < 86400:
        return "under-1d"
    if age_seconds < 7 * 86400:
        return "1-7d"
    if age_seconds < 30 * 86400:
        return "7-30d"
    return "over-30d"


def parse_listing(path: pathlib.Path, prefix: str, now: dt.datetime) -> tuple[dict[str, ObjectMeta], dict[str, ObjectMeta]]:
    raw: dict[str, ObjectMeta] = {}
    encrypted: dict[str, ObjectMeta] = {}
    try:
        with path.open("r", encoding="utf-8") as stream:
            for line in stream:
                try:
                    row = json.loads(line)
                except json.JSONDecodeError:
                    fail("RustFS object listing is invalid")
                if not isinstance(row, dict) or row.get("status") != "success":
                    fail("RustFS object listing did not complete")
                row_type = row.get("type")
                if row_type == "folder":
                    continue
                if row_type != "file":
                    fail("RustFS object listing has an unknown entry type")
                listed_key = row.get("key")
                if not isinstance(listed_key, str):
                    fail("RustFS object listing has no key")
                logical_key = listed_key[len(prefix):] if prefix and listed_key.startswith(prefix) else listed_key
                logical_key = validate_key(logical_key)
                size = row.get("size")
                modified = row.get("lastModified")
                if not isinstance(size, int) or size < 0 or not isinstance(modified, str):
                    fail("RustFS object listing metadata is invalid")
                metadata = ObjectMeta(size=size, modified=modified, age_bucket=age_bucket(modified, now))
                if logical_key == "encrypted" or logical_key.startswith("encrypted/"):
                    if logical_key == "encrypted":
                        continue
                    base_key = validate_key(logical_key[len("encrypted/"):])
                    if base_key in encrypted:
                        fail("RustFS object listing contains a duplicate encrypted key")
                    encrypted[base_key] = metadata
                else:
                    if logical_key in raw:
                        fail("RustFS object listing contains a duplicate plaintext key")
                    raw[logical_key] = metadata
    except OSError:
        fail("RustFS object listing is unavailable")
    return raw, encrypted


def parse_references(path: pathlib.Path) -> tuple[dict[str, Reference], str]:
    references: dict[str, Reference] = {}
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeError):
        fail("current database references are unavailable")
    ordered: list[Reference] = []
    for line in lines:
        fields = line.split("\t")
        if len(fields) != 4:
            fail("current database reference row is invalid")
        kind, object_id, raw_key, raw_size = fields
        key = validate_key(raw_key)
        try:
            logical_bytes = int(raw_size)
        except ValueError:
            fail("current database reference size is invalid")
        if kind not in ("file", "part") or not object_id or logical_bytes < 0 or key in references:
            fail("current database references are inconsistent")
        reference = Reference(kind, object_id, key, logical_bytes)
        references[key] = reference
        ordered.append(reference)
    digest = hashlib.sha256()
    for reference in sorted(ordered, key=lambda item: item.key):
        digest.update(
            f"{reference.kind}\t{reference.object_id}\t{reference.key}\t{reference.logical_bytes}\n".encode("utf-8")
        )
    return references, digest.hexdigest()


def parse_journal(path: pathlib.Path) -> tuple[frozenset[str], str]:
    private_file(path)
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
        records = [json.loads(line) for line in lines]
    except (OSError, UnicodeError, json.JSONDecodeError):
        fail("migration progress journal is invalid")
    if not records:
        fail("migration progress journal is empty")
    header = records[0]
    if not isinstance(header, dict) or header.get("type") != "header" or \
       header.get("schema") != PROGRESS_SCHEMA or header.get("writes") != "stopped":
        fail("migration progress journal header is invalid")
    reference_sha = header.get("references_sha256")
    if not isinstance(reference_sha, str) or not re.fullmatch(r"[0-9a-f]{64}", reference_sha):
        fail("migration progress journal reference digest is invalid")
    keys: set[str] = set()
    for record in records[1:]:
        if not isinstance(record, dict) or record.get("type") != "object":
            fail("migration progress journal record is invalid")
        keys.add(validate_key(record.get("key")))
    return frozenset(keys), reference_sha


def make_snapshot(
    inventory_path: pathlib.Path,
    inventory_sha256: str,
    referenced_count: int,
    listing_path: pathlib.Path,
    reference_path: pathlib.Path,
    journal_path: pathlib.Path,
    prefix: str,
    mode: str,
    now: dt.datetime | None = None,
) -> Snapshot:
    if mode not in ("before", "after"):
        fail("inventory phase is invalid")
    now = now or dt.datetime.now(dt.timezone.utc)
    contract = load_contract(inventory_path, inventory_sha256, referenced_count)
    raw_objects, encrypted_objects = parse_listing(listing_path, prefix, now)
    references, reference_sha256 = parse_references(reference_path)
    journal_keys, _journal_reference_sha = parse_journal(journal_path)
    if set(encrypted_objects) != set(references):
        fail("encrypted object inventory differs from database references")
    if contract.encrypted_unaccounted_count != 0:
        fail("encrypted inventory contains unaccounted objects")

    candidate_keys = set(contract.candidates)
    if candidate_keys.intersection(references):
        fail("an inventory object became referenced by the database")
    if candidate_keys.intersection(journal_keys):
        fail("an inventory object is present in the migration journal")
    actual_unaccounted = set(raw_objects).difference(references)
    if mode == "before" and actual_unaccounted != candidate_keys:
        fail("live unaccounted object set differs from the latest inventory")
    if mode == "after" and actual_unaccounted:
        fail("post-quarantine inventory still has unaccounted objects")

    candidates: dict[str, Candidate] = {}
    if mode == "before":
        for key, (kind, object_id) in contract.candidates.items():
            object_metadata = raw_objects.get(key)
            if object_metadata is None:
                fail("an inventory object is missing from RustFS")
            category = {"file": "file", "part": "upload-part", "unknown": "other"}[kind]
            candidates[key] = Candidate(
                key=key,
                kind=kind,
                object_id=object_id,
                size=object_metadata.size,
                modified=object_metadata.modified,
                category=category,
                age_bucket=object_metadata.age_bucket,
            )
        if sum(candidate.size for candidate in candidates.values()) != contract.candidate_physical_bytes:
            fail("live unaccounted byte total differs from the latest inventory")
    elif candidate_keys.intersection(raw_objects):
        fail("post-quarantine inventory still contains a quarantined object")
    return Snapshot(
        contract=contract,
        raw_objects=raw_objects,
        encrypted_objects=encrypted_objects,
        references=references,
        journal_keys=journal_keys,
        reference_sha256=reference_sha256,
        candidates=candidates,
    )


def distribution(candidates: Iterable[Candidate]) -> str:
    counts: dict[str, int] = {}
    for candidate in candidates:
        label = f"{candidate.category}/{candidate.age_bucket}"
        counts[label] = counts.get(label, 0) + 1
    return ",".join(f"{label}:{counts[label]}" for label in sorted(counts)) or "none"


def summarize(snapshot: Snapshot) -> str:
    count = len(snapshot.candidates)
    total_bytes = sum(candidate.size for candidate in snapshot.candidates.values())
    return f"objects={count} bytes={total_bytes} category_age={distribution(snapshot.candidates.values())}"


def ensure_private_directory(path: pathlib.Path) -> None:
    try:
        path.mkdir(mode=0o700)
    except FileExistsError:
        pass
    except OSError:
        fail("cannot create the private quarantine directory")
    private_directory(path)


def atomic_manifest(path: pathlib.Path, data: dict[str, object]) -> None:
    private_directory(path.parent)
    temp = path.parent / f".manifest-{os.getpid()}-{uuid.uuid4().hex}.tmp"
    raw = (json.dumps(data, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(temp, flags, 0o600)
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(raw)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp, path)
        parent_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(parent_fd)
        finally:
            os.close(parent_fd)
    except OSError:
        try:
            temp.unlink()
        except OSError:
            pass
        fail("cannot write the private quarantine manifest")
    private_file(path)


def file_digest(path: pathlib.Path) -> tuple[int, str]:
    try:
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) != 0o600:
            fail("quarantine copy has unsafe permissions")
        digest = hashlib.sha256()
        with path.open("rb") as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                digest.update(chunk)
    except OSError:
        fail("quarantine copy is unavailable")
    return info.st_size, digest.hexdigest()


def manifest_base(run_id: str, inventory_sha256: str, delete_after: str) -> dict[str, object]:
    return {
        "schema": MANIFEST_SCHEMA,
        "run_id": run_id,
        "inventory_sha256": inventory_sha256,
        "created_at_utc": dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
        "delete_after_utc": delete_after,
        "status": "copying",
        "objects": [],
    }


def candidate_manifest_record(candidate: Candidate, filename: str, digest: str) -> dict[str, object]:
    return {
        "key": candidate.key,
        "kind": candidate.kind,
        "category": candidate.category,
        "age_bucket": candidate.age_bucket,
        "size": candidate.size,
        "last_modified": candidate.modified,
        "filename": filename,
        "sha256": digest,
        "deleted": False,
    }


def apply_quarantine(
    snapshot_provider: Callable[[str], Snapshot],
    storage: Storage,
    quarantine_dir: pathlib.Path,
    run_id: str,
    delete_after: str,
    validate_envelopes: Callable[[int], None],
) -> tuple[Snapshot, str]:
    first = snapshot_provider("before")
    validate_envelopes(len(first.references))
    ensure_private_directory(quarantine_dir)
    set_mount = getattr(storage, "set_mount", None)
    if callable(set_mount):
        set_mount(quarantine_dir)
    manifest = manifest_base(run_id, first.contract.inventory_sha256, delete_after)
    manifest_path = quarantine_dir / "manifest.json"
    if manifest_path.exists() or manifest_path.is_symlink():
        fail("quarantine manifest already exists")
    atomic_manifest(manifest_path, manifest)

    for index, candidate in enumerate(sorted(first.candidates.values(), key=lambda item: item.key), start=1):
        filename = f"object-{index:04d}"
        destination = quarantine_dir / filename
        if destination.exists() or destination.is_symlink():
            fail("quarantine copy path already exists")
        storage.copy_from(candidate.key, destination)
        try:
            os.chmod(destination, 0o600)
        except OSError:
            fail("cannot protect quarantine copy")
        local_size, local_sha256 = file_digest(destination)
        remote_sha256 = storage.hash_remote(candidate.key)
        if local_size != candidate.size or local_sha256 != remote_sha256:
            fail("quarantine copy verification mismatch; bucket objects were left untouched")
        records = manifest["objects"]
        assert isinstance(records, list)
        records.append(candidate_manifest_record(candidate, filename, local_sha256))
        atomic_manifest(manifest_path, manifest)

    second = snapshot_provider("before")
    first_identity = {
        key: (item.kind, item.object_id, item.size, item.modified)
        for key, item in first.candidates.items()
    }
    second_identity = {
        key: (item.kind, item.object_id, item.size, item.modified)
        for key, item in second.candidates.items()
    }
    first_encrypted_identity = {
        key: (item.size, item.modified) for key, item in first.encrypted_objects.items()
    }
    second_encrypted_identity = {
        key: (item.size, item.modified) for key, item in second.encrypted_objects.items()
    }
    if first.reference_sha256 != second.reference_sha256 or first_identity != second_identity or \
       first_encrypted_identity != second_encrypted_identity:
        fail("media inventory changed during quarantine; bucket objects were left untouched")
    records = manifest["objects"]
    assert isinstance(records, list)
    for candidate, record in zip(
        sorted(first.candidates.values(), key=lambda item: item.key), records, strict=True
    ):
        if storage.hash_remote(candidate.key) != record["sha256"]:
            fail("RustFS object changed after quarantine copy; bucket objects were left untouched")

    validate_envelopes(len(second.references))
    manifest["status"] = "deleting"
    manifest["delete_started_at_utc"] = dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")
    atomic_manifest(manifest_path, manifest)
    for candidate, record in zip(
        sorted(first.candidates.values(), key=lambda item: item.key), records, strict=True
    ):
        storage.remove(candidate.key)
        record["deleted"] = True
        record["deleted_at_utc"] = dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")
        atomic_manifest(manifest_path, manifest)

    final = snapshot_provider("after")
    validate_envelopes(len(final.references))
    final_encrypted_identity = {
        key: (item.size, item.modified) for key, item in final.encrypted_objects.items()
    }
    if first.reference_sha256 != final.reference_sha256 or \
       first_encrypted_identity != final_encrypted_identity:
        fail("encrypted reference inventory changed during quarantine; restore copies from the quarantine manifest")
    manifest["status"] = "complete"
    manifest["completed_at_utc"] = dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")
    atomic_manifest(manifest_path, manifest)
    return final, summarize(first)


def load_manifest(path: pathlib.Path, expected_run_id: str) -> dict[str, object]:
    value = read_json(path)
    if not isinstance(value, dict) or value.get("schema") != MANIFEST_SCHEMA or value.get("run_id") != expected_run_id:
        fail("quarantine manifest is invalid")
    if value.get("status") not in ("deleting", "complete"):
        fail("quarantine copies are incomplete; no deletions were started")
    records = value.get("objects")
    if not isinstance(records, list) or not records:
        fail("quarantine manifest has no verified objects")
    seen_keys: set[str] = set()
    for record in records:
        if not isinstance(record, dict):
            fail("quarantine manifest object record is invalid")
        key = validate_key(record.get("key"))
        filename = record.get("filename")
        size = record.get("size")
        digest = record.get("sha256")
        if key in seen_keys or key == "encrypted" or key.startswith("encrypted/"):
            fail("quarantine manifest contains an invalid key set")
        if not isinstance(filename, str) or not re.fullmatch(r"object-[0-9]{4,}", filename):
            fail("quarantine manifest filename is invalid")
        if not isinstance(size, int) or size < 0 or not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest):
            fail("quarantine manifest checksum is invalid")
        seen_keys.add(key)
    return value


def restore_quarantine(
    listing_provider: Callable[[], tuple[dict[str, ObjectMeta], dict[str, ObjectMeta]]],
    storage: Storage,
    quarantine_dir: pathlib.Path,
    manifest_path: pathlib.Path,
    run_id: str,
    reference_count_provider: Callable[[], int],
    validate_envelopes: Callable[[int], None],
) -> tuple[int, int]:
    private_directory(quarantine_dir)
    manifest = load_manifest(manifest_path, run_id)
    records = manifest["objects"]
    assert isinstance(records, list)
    raw_objects, _encrypted_objects = listing_provider()
    pending: list[tuple[dict[str, object], pathlib.Path]] = []
    already_present = 0
    for record in records:
        assert isinstance(record, dict)
        key = str(record["key"])
        local_path = quarantine_dir / str(record["filename"])
        size, digest = file_digest(local_path)
        if size != record["size"] or digest != record["sha256"]:
            fail("quarantine copy no longer matches its manifest")
        remote = raw_objects.get(key)
        if remote is None:
            pending.append((record, local_path))
            continue
        if remote.size != record["size"] or storage.hash_remote(key) != record["sha256"]:
            fail("existing RustFS object differs from the quarantine copy")
        already_present += 1

    validate_envelopes(reference_count_provider())
    manifest["restore_status"] = "restoring"
    atomic_manifest(manifest_path, manifest)
    for record, local_path in pending:
        key = str(record["key"])
        storage.copy_to(local_path, key)
        if storage.hash_remote(key) != record["sha256"]:
            fail("restored RustFS object did not match its quarantine copy")
        record["restored"] = True
        atomic_manifest(manifest_path, manifest)

    raw_after, _encrypted_after = listing_provider()
    for record in records:
        assert isinstance(record, dict)
        key = str(record["key"])
        remote = raw_after.get(key)
        if remote is None or remote.size != record["size"] or storage.hash_remote(key) != record["sha256"]:
            fail("restored quarantine inventory did not verify")
    validate_envelopes(reference_count_provider())
    manifest["restore_status"] = "complete"
    manifest["restored_at_utc"] = dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")
    atomic_manifest(manifest_path, manifest)
    return len(pending), already_present


def load_compose_argv(path: pathlib.Path) -> list[str]:
    try:
        raw = path.read_bytes()
    except OSError:
        fail("live Compose command is unavailable")
    values = [value.decode("utf-8") for value in raw.split(b"\0") if value]
    if not values or values[0] != "docker" or values[1:3] != ["compose", "--project-directory"]:
        fail("live Compose command is invalid")
    return values


class LiveMCStorage:
    def __init__(self, compose: list[str], config_dir: pathlib.Path, mount_dir: pathlib.Path, prefix: str, evidence_dir: pathlib.Path, run_id: str):
        self.compose = compose
        self.config_dir = config_dir
        self.mount_dir = mount_dir
        self.prefix = prefix
        self.evidence_dir = evidence_dir
        self.run_id = run_id
        self.calls = 0
        self.envelope_inventory_calls = 0
        self.remote_root = f"{S3_ALIAS}/{S3_BUCKET}/{prefix}"

    def set_mount(self, path: pathlib.Path) -> None:
        self.mount_dir = path

    def validate_envelopes(self, work_dir: pathlib.Path, referenced_count: int) -> None:
        self.envelope_inventory_calls += 1
        report_path = work_dir / f"envelope-inventory-{self.envelope_inventory_calls:02d}.json"
        error_path = self._error_path("envelope-inventory")
        command = self.compose + [
            "--profile", "migration", "run", "--rm", "--no-deps", "-T",
            "--entrypoint", ENVELOPE_INVENTORY_BINARY, ENVELOPE_INVENTORY_SERVICE,
        ]
        try:
            with report_path.open("xb") as output, error_path.open("wb") as errors:
                result = subprocess.run(command, stdin=subprocess.DEVNULL, stdout=output, stderr=errors, check=False)
        except OSError:
            fail("cannot run the fresh encrypted envelope inventory")
        if result.returncode != 0:
            fail("fresh encrypted envelope inventory failed; private diagnostics were retained")
        try:
            os.chmod(report_path, 0o600)
        except OSError:
            fail("cannot protect the fresh encrypted envelope inventory")
        validate_fresh_envelope_inventory(report_path, referenced_count)

    def remote_key(self, key: str) -> str:
        return f"{self.remote_root}{key}"

    def _command(self, args: list[str]) -> list[str]:
        if not self.config_dir.is_dir() or self.config_dir.is_symlink():
            fail("private RustFS client configuration is unavailable")
        if not self.mount_dir.is_dir() or self.mount_dir.is_symlink():
            fail("private RustFS operation mount is unavailable")
        script = (
            "umask 077; "
            "mc alias set local http://rustfs:9000 \"$TG_BLOB_S3_ACCESS_KEY_ID\" "
            "\"$(cat /run/secrets/teagramd-blob-secret-key)\" >/dev/null || exit 1; "
            "exec mc \"$@\""
        )
        return self.compose + [
            "run", "--rm", "--no-deps", "-T",
            "--volume", f"{self.config_dir}:/root/.mc",
            "--volume", f"{self.mount_dir}:/quarantine",
            "--entrypoint", "/bin/sh", MC_SERVICE,
            "-ec", script, "mc", *args,
        ]

    def _error_path(self, operation: str) -> pathlib.Path:
        self.calls += 1
        path = self.evidence_dir / f"media-quarantine-{self.run_id}-{self.calls:04d}-{operation}.stderr"
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
        if hasattr(os, "O_NOFOLLOW"):
            flags |= os.O_NOFOLLOW
        try:
            descriptor = os.open(path, flags, 0o600)
            os.close(descriptor)
        except OSError:
            fail("cannot preserve private RustFS diagnostics")
        return path

    def list_jsonl(self, destination: pathlib.Path) -> None:
        error_path = self._error_path("list")
        command = self._command(["--json", "ls", "--recursive", self.remote_root])
        try:
            with destination.open("xb") as output, error_path.open("wb") as errors:
                result = subprocess.run(command, stdin=subprocess.DEVNULL, stdout=output, stderr=errors, check=False)
        except OSError:
            fail("cannot run the reviewed RustFS inventory client")
        if result.returncode != 0:
            fail("RustFS object inventory failed; private diagnostics were retained")

    def hash_remote(self, key: str) -> str:
        error_path = self._error_path("read")
        command = self._command(["cat", self.remote_key(key)])
        try:
            with error_path.open("wb") as errors:
                process = subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=errors)
                assert process.stdout is not None
                digest = hashlib.sha256()
                for chunk in iter(lambda: process.stdout.read(1024 * 1024), b""):
                    digest.update(chunk)
                result = process.wait()
        except OSError:
            fail("cannot read the RustFS object for verification")
        if result != 0:
            fail("RustFS object read failed; private diagnostics were retained")
        return digest.hexdigest()

    def copy_from(self, key: str, destination: pathlib.Path) -> None:
        error_path = self._error_path("copy")
        command = self._command(["cp", self.remote_key(key), f"/quarantine/{destination.name}"])
        try:
            with error_path.open("wb") as errors:
                result = subprocess.run(command, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=errors, check=False)
        except OSError:
            fail("cannot copy the RustFS object into quarantine")
        if result.returncode != 0:
            fail("RustFS object quarantine copy failed; private diagnostics were retained")

    def copy_to(self, source: pathlib.Path, key: str) -> None:
        error_path = self._error_path("restore")
        command = self._command(["cp", f"/quarantine/{source.name}", self.remote_key(key)])
        try:
            with error_path.open("wb") as errors:
                result = subprocess.run(command, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=errors, check=False)
        except OSError:
            fail("cannot restore the quarantine copy to RustFS")
        if result.returncode != 0:
            fail("RustFS quarantine restore failed; private diagnostics were retained")

    def remove(self, key: str) -> None:
        error_path = self._error_path("remove")
        command = self._command(["rm", "--force", self.remote_key(key)])
        try:
            with error_path.open("wb") as errors:
                result = subprocess.run(command, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=errors, check=False)
        except OSError:
            fail("cannot remove the verified RustFS object")
        if result.returncode != 0:
            fail("RustFS object removal failed; private diagnostics were retained")


class LiveSnapshotProvider:
    def __init__(self, storage: LiveMCStorage, inventory: pathlib.Path, inventory_sha: str, referenced_count: int, journal: pathlib.Path, prefix: str, work_dir: pathlib.Path):
        self.storage = storage
        self.inventory = inventory
        self.inventory_sha = inventory_sha
        self.referenced_count = referenced_count
        self.journal = journal
        self.prefix = prefix
        self.work_dir = work_dir
        self.calls = 0

    def __call__(self, mode: str) -> Snapshot:
        self.calls += 1
        listing_path = self.work_dir / f"objects-{self.calls:02d}.jsonl"
        self.storage.list_jsonl(listing_path)
        query_path = self.work_dir / f"references-{self.calls:02d}.tsv"
        error_path = self.storage.evidence_dir / f"media-quarantine-{self.storage.run_id}-{self.calls:04d}-postgres.stderr"
        write_live_reference_inventory(query_path, error_path)
        snapshot = make_snapshot(
            self.inventory,
            self.inventory_sha,
            self.referenced_count,
            listing_path,
            query_path,
            self.journal,
            self.prefix,
            mode,
        )
        return snapshot


def write_live_reference_inventory(query_path: pathlib.Path, error_path: pathlib.Path) -> None:
    command = [
        "docker", "exec", "-i", "-u", "postgres", POSTGRES_CONTAINER,
        "psql", "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1",
        "-U", "postgres", "-d", "teagram",
    ]
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(error_path, flags, 0o600)
        with os.fdopen(descriptor, "wb") as errors, query_path.open("xb") as output:
            result = subprocess.run(
                command,
                input=REFERENCE_INVENTORY_SQL.encode("utf-8"),
                stdout=output,
                stderr=errors,
                check=False,
            )
    except OSError:
        fail("cannot query current media references")
    if result.returncode != 0:
        fail("current media reference inventory failed; private diagnostics were retained")
    try:
        os.chmod(query_path, 0o600)
    except OSError:
        fail("cannot protect current database references")


def query_live_reference_count(query_path: pathlib.Path, error_path: pathlib.Path) -> int:
    write_live_reference_inventory(query_path, error_path)
    references, _reference_sha256 = parse_references(query_path)
    return len(references)


def validate_restore_run_id(run_id: str) -> None:
    if not re.fullmatch(r"[0-9]{8}T[0-9]{6}Z-[0-9]+-[0-9a-f]{7}", run_id):
        fail("quarantine run ID is invalid")


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument("--mode", choices=("check", "apply", "restore"), required=True)
    parser.add_argument("--compose-argv", type=pathlib.Path, required=True)
    parser.add_argument("--inventory", type=pathlib.Path)
    parser.add_argument("--inventory-sha256")
    parser.add_argument("--referenced-count", type=int)
    parser.add_argument("--journal", type=pathlib.Path)
    parser.add_argument("--prefix", default="")
    parser.add_argument("--work-dir", type=pathlib.Path, required=True)
    parser.add_argument("--evidence-dir", type=pathlib.Path, required=True)
    parser.add_argument("--quarantine-base", type=pathlib.Path, required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--diagnostic-id")
    parser.add_argument("--delete-after")
    parser.add_argument("--mount-dir", type=pathlib.Path, required=True)
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    try:
        args = parse_args(argv or sys.argv[1:])
        if not args.work_dir.is_dir() or args.work_dir.is_symlink():
            fail("private operation work directory is unavailable")
        if not args.evidence_dir.is_dir() or args.evidence_dir.is_symlink():
            fail("private evidence directory is unavailable")
        config_dir = args.work_dir / "mc-config"
        config_dir.mkdir(mode=0o700, exist_ok=True)
        private_directory(config_dir)
        compose = load_compose_argv(args.compose_argv)

        if args.mode == "restore":
            validate_restore_run_id(args.run_id)
            quarantine_dir = args.quarantine_base / f"MAIN-3441-{args.run_id}"
            private_directory(quarantine_dir)
            storage = LiveMCStorage(
                compose,
                config_dir,
                quarantine_dir,
                args.prefix,
                args.evidence_dir,
                args.diagnostic_id or args.run_id,
            )
            manifest_path = quarantine_dir / "manifest.json"
            private_file(manifest_path)
            listing_path = args.work_dir / "restore-listing.jsonl"
            reference_queries = 0

            def listing_provider() -> tuple[dict[str, ObjectMeta], dict[str, ObjectMeta]]:
                if listing_path.exists():
                    listing_path.unlink()
                storage.set_mount(args.mount_dir)
                storage.list_jsonl(listing_path)
                return parse_listing(listing_path, args.prefix, dt.datetime.now(dt.timezone.utc))

            def current_reference_count() -> int:
                nonlocal reference_queries
                reference_queries += 1
                query_path = args.work_dir / f"restore-references-{reference_queries:02d}.tsv"
                error_path = storage._error_path("postgres")
                return query_live_reference_count(query_path, error_path)

            validate_restore_envelopes = lambda count: storage.validate_envelopes(args.work_dir, count)
            restored, already = restore_quarantine(
                listing_provider,
                storage,
                quarantine_dir,
                manifest_path,
                args.run_id,
                current_reference_count,
                validate_restore_envelopes,
            )
            print(f"restore=pass objects={restored + already} restored={restored} already_present={already} envelope_validation=pass quarantine_path={quarantine_dir}")
            return 0

        if args.inventory is None or args.inventory_sha256 is None or args.referenced_count is None or args.journal is None:
            fail("inventory operation inputs are incomplete")
        if not re.fullmatch(r"[0-9]{8}T[0-9]{6}Z-[0-9]+-[0-9a-f]{7}", args.run_id):
            fail("quarantine run ID is invalid")
        temporary_mount = args.mount_dir
        storage = LiveMCStorage(
            compose,
            config_dir,
            temporary_mount,
            args.prefix,
            args.evidence_dir,
            args.diagnostic_id or args.run_id,
        )
        provider = LiveSnapshotProvider(
            storage,
            args.inventory,
            args.inventory_sha256,
            args.referenced_count,
            args.journal,
            args.prefix,
            args.work_dir,
        )
        if args.mode == "check":
            snapshot = provider("before")
            storage.validate_envelopes(args.work_dir, len(snapshot.references))
            print(f"preflight=pass {summarize(snapshot)} envelope_validation=pass authority=encrypted service=running")
            return 0

        if args.delete_after is None:
            fail("quarantine retention date is missing")
        quarantine_dir = args.quarantine_base / f"MAIN-3441-{args.run_id}"
        validate_apply_envelopes = lambda count: storage.validate_envelopes(args.work_dir, count)
        final, summary = apply_quarantine(
            provider,
            storage,
            quarantine_dir,
            args.run_id,
            args.delete_after,
            validate_apply_envelopes,
        )
        print(
            f"quarantine=pass {summary} quarantine_path={quarantine_dir} delete_after_utc={args.delete_after} "
            f"post_inventory=pass unaccounted=0 referenced_without_valid_envelope=0 authority=encrypted service=running"
        )
        return 0
    except QuarantineError as error:
        print(f"quarantine-unaccounted-media: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
