#!/usr/bin/env python3
"""Inspect and publish the runner-owned durable blob-mode authority."""

from __future__ import annotations

import argparse
import datetime as dt
import fcntl
import hashlib
import json
import os
import pathlib
import posixpath
import re
import stat
import subprocess
import sys
import uuid


MODE_TARGET = "/run/telegramd/blob-mode"
BLOB_TARGET = "/var/lib/telegramd-blobs"
SCHEMA = "teagram.blob-mode/v1"
REPORT_SCHEMA = "teagram.blob-mode-report/v1"
NON_SERVING_SERVICES = {"rustfs", "rustfs-init", "migrate", "blob-migrate", "blob-restore"}
S3_FIELDS = (
    "TG_BLOB_S3_ENDPOINT",
    "TG_BLOB_S3_BUCKET",
    "TG_BLOB_S3_PREFIX",
    "TG_BLOB_S3_REGION",
    "TG_BLOB_S3_ACCESS_KEY_ID",
    "TG_BLOB_S3_SECRET_ACCESS_KEY",
    "TG_BLOB_S3_SECRET_ACCESS_KEY_FILE",
    "TG_BLOB_S3_CA_PATH",
    "TG_BLOB_S3_ALLOW_INSECURE_HTTP",
)
RECORD_FIELDS = {
    "schema", "generation", "transition_id", "supersedes", "outcome",
    "backend", "volumes", "evidence", "published_at",
}


class Reject(Exception):
    pass


def reject(reason: str) -> None:
    raise Reject(reason)


def strict_pairs(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            reject("duplicate-json-key")
        result[key] = value
    return result


def read_json(path: pathlib.Path) -> object:
    try:
        raw = path.read_bytes()
        if raw.startswith(b"\xef\xbb\xbf"):
            reject("json-bom")
        return json.loads(raw.decode("utf-8"), object_pairs_hook=strict_pairs)
    except Reject:
        raise
    except (OSError, UnicodeDecodeError, json.JSONDecodeError):
        reject("invalid-json")


def stdin_json() -> object:
    try:
        return json.load(sys.stdin, object_pairs_hook=strict_pairs)
    except Reject:
        raise
    except (UnicodeDecodeError, json.JSONDecodeError):
        reject("invalid-json")


def file_stat(path: pathlib.Path, kind: int, expected_mode: int | None = None) -> os.stat_result:
    try:
        info = path.lstat()
    except OSError:
        reject("state-unavailable")
    if stat.S_ISLNK(info.st_mode) or stat.S_IFMT(info.st_mode) != kind or info.st_uid != 0:
        reject("state-type-or-owner")
    if info.st_mode & 0o022:
        reject("state-permissions")
    if expected_mode is not None and stat.S_IMODE(info.st_mode) != expected_mode:
        reject("private-evidence-permissions")
    return info


def secure_dir(path: pathlib.Path, create: bool = False, mode: int = 0o755) -> None:
    try:
        path.lstat()
    except FileNotFoundError:
        if not create:
            reject("state-unavailable")
        path.mkdir(mode=mode)
        os.chown(path, 0, 0)
        os.chmod(path, mode)
    file_stat(path, stat.S_IFDIR)


def secure_state_parent(state_dir: pathlib.Path) -> None:
    parent = state_dir.parent
    if parent.is_symlink():
        reject("state-parent-symlink")
    created = not parent.exists()
    secure_dir(parent, create=True, mode=0o755)
    if created:
        fsync_dir(parent.parent)


def canonical_uuid(value: object) -> bool:
    return isinstance(value, str) and re.fullmatch(
        r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}", value
    ) is not None


def digest(value: object) -> bool:
    return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{64}", value) is not None


def normalize_prefix(value: object) -> str:
    if not isinstance(value, str) or not value:
        reject("backend-prefix")
    result = value if value.endswith("/") else value + "/"
    if result.startswith("/") or "//" in result or any(part in ("", ".", "..") for part in result[:-1].split("/")):
        reject("backend-prefix")
    return result


def backend_from_values(values: dict[str, str]) -> dict[str, str]:
    blob_dir = posixpath.normpath(values.get("TG_BLOB_DIR", "blobs"))
    if not blob_dir:
        reject("backend-dir")
    if any(values.get(key, "") for key in S3_FIELDS):
        endpoint = values.get("TG_BLOB_S3_ENDPOINT", "")
        bucket = values.get("TG_BLOB_S3_BUCKET", "")
        prefix = normalize_prefix(values.get("TG_BLOB_S3_PREFIX", ""))
        if not endpoint or not bucket:
            reject("backend-s3-incomplete")
        return {"kind": "s3", "endpoint": endpoint, "bucket": bucket, "prefix": prefix}
    return {"kind": "local", "dir": blob_dir}


def environment_map(raw: object, compose: bool) -> dict[str, str]:
    result: dict[str, str] = {}
    if compose and isinstance(raw, list):
        pairs = []
        for entry in raw:
            if not isinstance(entry, str) or "=" not in entry:
                reject("compose-environment")
            pairs.append(entry.split("=", 1))
    elif isinstance(raw, dict):
        pairs = list(raw.items())
    elif raw is None and compose:
        pairs = []
    else:
        reject("environment-shape")
    relevant = {"TG_BLOB_DIR", *S3_FIELDS}
    for key, value in pairs:
        if key not in relevant:
            continue
        if key in result:
            reject("duplicate-environment-key")
        if value is None:
            reject("unresolved-environment")
        if not isinstance(value, str):
            value = str(value)
        result[key] = value
    return result


def resolved_volumes(compose: dict[str, object]) -> dict[str, str | None]:
    raw = compose.get("volumes", {})
    if not isinstance(raw, dict):
        reject("compose-volumes")
    result: dict[str, str] = {}
    for key in ("tgblobs", "rustfsdata"):
        item = raw.get(key)
        if key == "rustfsdata" and item is None:
            result[key] = None
            continue
        if not isinstance(item, dict) or not isinstance(item.get("name"), str) or not item["name"]:
            reject("compose-volume-name")
        result[key] = item["name"]
    return result


def service_mounts(service: dict[str, object], mode_source: str, compose: dict[str, object]) -> tuple[list[dict[str, object]], list[dict[str, object]]]:
    raw = service.get("volumes", [])
    if not isinstance(raw, list):
        reject("compose-mounts")
    volumes = resolved_volumes(compose)
    mode_mounts: list[dict[str, object]] = []
    blob_mounts: list[dict[str, object]] = []
    for mount in raw:
        if not isinstance(mount, dict):
            reject("compose-mount")
        source = mount.get("source", "")
        target = mount.get("target", "")
        kind = mount.get("type", "")
        read_only = mount.get("read_only", False) is True
        source_is_mode_dir = False
        if kind == "bind" and isinstance(source, str) and source:
            source_path = pathlib.Path(source)
            if not source_path.is_absolute():
                source_path = pathlib.Path(mode_source).parents[1] / source_path
            source_is_mode_dir = os.path.realpath(source_path) == mode_source
        if target == MODE_TARGET or source_is_mode_dir:
            mode_mounts.append({"type": kind, "source": source, "target": target, "read_only": read_only})
        if target == BLOB_TARGET:
            if kind != "volume" or source != "tgblobs":
                reject("compose-blob-mount")
            blob_mounts.append({
                "type": kind,
                "source": volumes["tgblobs"],
                "target": target,
                "read_only": read_only,
            })
    return mode_mounts, blob_mounts


def compose_inventory(compose: object, checkout: pathlib.Path) -> dict[str, object]:
    if not isinstance(compose, dict) or not isinstance(compose.get("services"), dict):
        reject("compose-shape")
    services = compose["services"]
    mode_source = os.path.realpath(checkout / ".state" / "blob-mode")
    volume_names = resolved_volumes(compose)
    result_services = []
    for name in sorted(services):
        if not name.startswith("telegramd"):
            continue
        service = services[name]
        if not isinstance(service, dict):
            reject("compose-service")
        environment = environment_map(service.get("environment", {}), True)
        mode_mounts, blob_mounts = service_mounts(service, mode_source, compose)
        result_services.append({
            "name": name,
            "backend": backend_from_values(environment),
            "blob_mode_mounts": mode_mounts,
            "tgblobs_mounts": blob_mounts,
        })
    if not result_services:
        reject("compose-telegramd-missing")
    # Retargeted mounts from the authority directory on non-serving helpers are
    # prohibited too; scan every service without returning unrelated settings.
    for name, service in services.items():
        if name.startswith("telegramd"):
            continue
        if not isinstance(service, dict):
            reject("compose-service")
        modes, _ = service_mounts(service, mode_source, compose)
        if modes:
            reject("mode-mount-helper")
    return {"services": result_services, "volumes": volume_names, "mode_source": mode_source}


def container_backend(container: dict[str, object]) -> dict[str, str]:
    config = container.get("Config")
    if not isinstance(config, dict):
        reject("container-config")
    env = config.get("Env", [])
    if not isinstance(env, list):
        reject("container-environment")
    values: dict[str, str] = {}
    relevant = {"TG_BLOB_DIR", *S3_FIELDS}
    for item in env:
        if not isinstance(item, str) or "=" not in item:
            reject("container-environment")
        key, value = item.split("=", 1)
        if key not in relevant:
            continue
        if key in values:
            reject("duplicate-environment-key")
        values[key] = value
    return backend_from_values(values)


def container_inventory(inspected: object, checkout: pathlib.Path, allow_empty: bool = False) -> dict[str, object]:
    if isinstance(inspected, dict):
        inspected = [inspected]
    if not isinstance(inspected, list):
        reject("inspect-shape")
    expected_source = os.path.realpath(checkout / ".state" / "blob-mode")
    result = []
    for item in inspected:
        if not isinstance(item, dict):
            reject("inspect-container")
        config = item.get("Config")
        labels = config.get("Labels", {}) if isinstance(config, dict) else {}
        service = labels.get("com.docker.compose.service", "") if isinstance(labels, dict) else ""
        state = item.get("State", {})
        if not isinstance(state, dict) or state.get("Status") != "running":
            continue
        if not isinstance(service, str) or not service:
            reject("container-service")
        mounts = item.get("Mounts", [])
        if not isinstance(mounts, list):
            reject("container-mounts")
        mode_mounts = []
        blob_mounts = []
        rustfs_mounts = []
        for mount in mounts:
            if not isinstance(mount, dict):
                reject("container-mount")
            source = mount.get("Source", "")
            destination = mount.get("Destination", "")
            kind = mount.get("Type", "")
            rw_value = mount.get("RW")
            if not isinstance(source, str) or not isinstance(destination, str) or not isinstance(kind, str) or not isinstance(rw_value, bool):
                reject("container-mount")
            rw = rw_value
            source_is_mode_dir = kind == "bind" and isinstance(source, str) and os.path.realpath(source) == expected_source
            if destination == MODE_TARGET or source_is_mode_dir:
                mode_mounts.append({"type": kind, "source": source, "target": destination, "read_only": not rw})
            if destination == BLOB_TARGET:
                blob_mounts.append({"type": kind, "name": mount.get("Name", ""), "target": destination, "rw": rw})
            if destination == "/data" and service == "rustfs":
                rustfs_mounts.append({"type": kind, "name": mount.get("Name", ""), "target": destination, "rw": rw})
        is_telegramd = isinstance(service, str) and service.startswith("telegramd")
        is_helper = isinstance(service, str) and service in NON_SERVING_SERVICES
        if not is_telegramd and not is_helper:
            if mode_mounts:
                reject("mode-mount-helper")
            continue
        container_id = item.get("Id")
        if not isinstance(container_id, str) or re.fullmatch(r"[0-9a-f]{64}", container_id) is None:
            reject("container-id")
        record: dict[str, object] = {"id": container_id, "service": service, "mode_mounts": mode_mounts}
        if is_telegramd:
            record["backend"] = container_backend(item)
            record["tgblobs_mounts"] = blob_mounts
        elif service == "rustfs":
            record["rustfs_mounts"] = rustfs_mounts
        result.append(record)
    result.sort(key=lambda item: (str(item["service"]), str(item["id"])))
    if not allow_empty and not any(item["service"].startswith("telegramd") for item in result):
        reject("running-telegramd-missing")
    return {"containers": result, "mode_source": expected_source}


def exact_backend(record: object) -> dict[str, str]:
    if not isinstance(record, dict) or not isinstance(record.get("backend"), dict):
        reject("record-backend")
    backend = record["backend"]
    if backend.get("kind") == "local":
        if set(backend) != {"kind", "dir"} or not isinstance(backend.get("dir"), str) or not backend["dir"]:
            reject("record-backend")
        if posixpath.normpath(backend["dir"]) != backend["dir"]:
            reject("record-backend")
    elif backend.get("kind") == "s3":
        if set(backend) != {"kind", "endpoint", "bucket", "prefix"}:
            reject("record-backend")
        if not all(isinstance(backend.get(key), str) and backend[key] for key in ("endpoint", "bucket", "prefix")):
            reject("record-backend")
        if normalize_prefix(backend["prefix"]) != backend["prefix"]:
            reject("record-backend")
    else:
        reject("record-backend")
    return backend


def valid_outcome(record: object, expected_generation: int, previous_id: str | None, previous_outcome: str | None) -> dict[str, object]:
    if not isinstance(record, dict) or set(record) != RECORD_FIELDS:
        reject("record-schema")
    if (
        record.get("schema") != SCHEMA
        or type(record.get("generation")) is not int
        or record.get("generation") != expected_generation
    ):
        reject("record-sequence")
    transition = record.get("transition_id")
    if not canonical_uuid(transition):
        reject("record-transition-id")
    outcome = record.get("outcome")
    backend = exact_backend(record)
    if backend["kind"] == "local" and backend["dir"] != BLOB_TARGET:
        reject("record-local-dir")
    if (outcome == "initial-local" and expected_generation != 1) or (outcome == "s3-accepted" and backend["kind"] != "s3") or (outcome == "recovered-local" and backend["kind"] != "local"):
        reject("record-outcome")
    if expected_generation == 1:
        if outcome != "initial-local" or record.get("supersedes") is not None:
            reject("record-supersession")
    else:
        if not canonical_uuid(record.get("supersedes")) or record["supersedes"] != previous_id or outcome == "initial-local":
            reject("record-supersession")
        if outcome == "recovered-local" and previous_outcome != "s3-accepted":
            reject("record-supersession")
    volumes = record.get("volumes")
    if not isinstance(volumes, dict) or set(volumes) != {"tgblobs", "rustfsdata"}:
        reject("record-volumes")
    if not isinstance(volumes.get("tgblobs"), str) or not volumes["tgblobs"]:
        reject("record-volumes")
    if expected_generation == 1:
        if volumes.get("rustfsdata") is not None:
            reject("record-volumes")
    elif not isinstance(volumes.get("rustfsdata"), str) or not volumes["rustfsdata"]:
        reject("record-volumes")
    evidence = record.get("evidence")
    if not isinstance(evidence, dict):
        reject("record-evidence")
    if outcome == "initial-local":
        if set(evidence) != {"report_sha256"}:
            reject("record-evidence")
    elif outcome == "s3-accepted":
        expected = {"report_sha256", "source_manifest_sha256", "destination_manifest_sha256", "object_count", "byte_total", "copy_passes"}
        if set(evidence) != expected or type(evidence.get("copy_passes")) is not int or evidence.get("copy_passes") != 2:
            reject("record-evidence")
        if not all(digest(evidence.get(key)) for key in ("source_manifest_sha256", "destination_manifest_sha256")):
            reject("record-evidence")
        if not all(type(evidence.get(key)) is int and evidence[key] >= 0 for key in ("object_count", "byte_total")):
            reject("record-evidence")
    elif outcome == "recovered-local":
        expected = {"report_sha256", "s3_census_manifest_sha256", "restored_manifest_sha256", "object_count", "byte_total", "restore_passes", "retained_cutover_key_count"}
        if set(evidence) != expected or type(evidence.get("restore_passes")) is not int or evidence.get("restore_passes") != 2:
            reject("record-evidence")
        if not all(digest(evidence.get(key)) for key in ("s3_census_manifest_sha256", "restored_manifest_sha256")):
            reject("record-evidence")
        if not all(type(evidence.get(key)) is int and evidence[key] >= 0 for key in ("object_count", "byte_total", "retained_cutover_key_count")):
            reject("record-evidence")
    else:
        reject("record-outcome")
    if not digest(evidence.get("report_sha256")):
        reject("record-evidence")
    published = record.get("published_at")
    try:
        timestamp = dt.datetime.fromisoformat(published.replace("Z", "+00:00"))
    except (AttributeError, ValueError):
        reject("record-published-at")
    if timestamp.tzinfo is None or timestamp.utcoffset() != dt.timedelta(0):
        reject("record-published-at")
    return record


def report_path(report_root: pathlib.Path, transition_id: str) -> pathlib.Path:
    return report_root / f"telegramd-blob-mode-report-{transition_id}.json"


def validate_report(report_root: pathlib.Path, record: dict[str, object]) -> None:
    report_file = report_path(report_root, record["transition_id"])
    file_stat(report_file, stat.S_IFREG, 0o600)
    try:
        raw = report_file.read_bytes()
    except OSError:
        reject("report-unavailable")
    if hashlib.sha256(raw).hexdigest() != record["evidence"]["report_sha256"]:
        reject("report-digest")
    report = read_json(report_file)
    if not isinstance(report, dict) or report.get("schema") != REPORT_SCHEMA:
        reject("report-schema")
    if (
        type(report.get("generation")) is not int
        or report.get("generation") != record["generation"]
        or report.get("transition_id") != record["transition_id"]
    ):
        reject("report-provenance")
    if report.get("backend") != record["backend"] or report.get("outcome") != record["outcome"]:
        reject("report-provenance")
    if record["outcome"] == "initial-local":
        if (
            report.get("inspection_kind") != "unguarded-local-baseline"
            or type(report.get("generation")) is not int
            or type(report.get("journal_entries")) is not int
            or report["journal_entries"] != 0
            or not isinstance(report.get("baseline_sha"), str)
            or re.fullmatch(r"[0-9a-f]{40}", report["baseline_sha"]) is None
            or not isinstance(report.get("target_sha"), str)
            or re.fullmatch(r"[0-9a-f]{40}", report["target_sha"]) is None
        ):
            reject("initial-report-provenance")
        containers = report.get("containers")
        compose = report.get("compose")
        if not isinstance(containers, dict) or not isinstance(compose, dict):
            reject("initial-report-inspection")
        container_items = containers.get("containers")
        if not isinstance(container_items, list):
            reject("initial-report-inspection")
        inspected_services = [
            item for item in container_items
            if isinstance(item, dict) and str(item.get("service", "")).startswith("telegramd")
        ]
        rendered_services = compose.get("services", [])
        if not inspected_services or not isinstance(rendered_services, list) or not rendered_services:
            reject("initial-report-inspection")
        for item in inspected_services:
            mounts = item.get("tgblobs_mounts", [])
            if (
                item.get("backend") != record["backend"]
                or item.get("mode_mounts")
                or not isinstance(mounts, list)
                or len(mounts) != 1
                or not isinstance(mounts[0], dict)
                or mounts[0].get("type") != "volume"
                or mounts[0].get("name") != record["volumes"]["tgblobs"]
                or mounts[0].get("rw") is not True
            ):
                reject("initial-report-inspection")
        for item in rendered_services:
            if not isinstance(item, dict):
                reject("initial-report-inspection")
            mounts = item.get("tgblobs_mounts", [])
            if (
                item.get("backend") != record["backend"]
                or item.get("blob_mode_mounts")
                or not isinstance(mounts, list)
                or len(mounts) != 1
                or not isinstance(mounts[0], dict)
                or mounts[0].get("source") != record["volumes"]["tgblobs"]
                or mounts[0].get("read_only") is not False
            ):
                reject("initial-report-inspection")
        compose_volumes = compose.get("volumes")
        if not isinstance(compose_volumes, dict) or compose_volumes.get("tgblobs") != record["volumes"]["tgblobs"]:
            reject("initial-report-inspection")


def read_authority(state_dir: pathlib.Path, report_root: pathlib.Path) -> tuple[list[dict[str, object]], bytes, bytes | None]:
    secure_dir(state_dir)
    root_temporaries: list[tuple[pathlib.Path, bytes]] = []
    try:
        root_entries = list(state_dir.iterdir())
    except OSError:
        reject("state-unavailable")
    for entry in root_entries:
        if entry.name in ("mode.json", "journal"):
            continue
        match = re.fullmatch(r"\.mode\.json\.tmp-([0-9a-f-]{36})", entry.name)
        if match is None or not canonical_uuid(match.group(1)):
            reject("state-ambiguous")
        info = file_stat(entry, stat.S_IFREG)
        if info.st_size > 4096:
            reject("mode-oversize")
        try:
            root_temporaries.append((entry, entry.read_bytes()))
        except OSError:
            reject("state-unavailable")
    journal = state_dir / "journal"
    secure_dir(journal)
    try:
        names = sorted(entry.name for entry in journal.iterdir())
    except OSError:
        reject("journal-unavailable")
    committed = []
    journal_temporaries: list[tuple[str, bytes]] = []
    for name in names:
        if name.startswith("."):
            match = re.fullmatch(r"\.tmp-([0-9a-f-]{36})", name)
            if match is None or not canonical_uuid(match.group(1)):
                reject("journal-ambiguous")
            item = journal / name
            info = file_stat(item, stat.S_IFREG)
            if info.st_size > 4096:
                reject("journal-oversize")
            try:
                journal_temporaries.append((name, item.read_bytes()))
            except OSError:
                reject("journal-unavailable")
            continue
        committed.append(name)
    if not committed or len(committed) > 1000:
        reject("journal-sequence")
    if committed != [f"{index:010d}.json" for index in range(1, len(committed) + 1)]:
        reject("journal-sequence")
    records: list[dict[str, object]] = []
    transitions: set[str] = set()
    reports: set[str] = set()
    previous_id = None
    previous_outcome = None
    head = b""
    for generation, name in enumerate(committed, start=1):
        path = journal / name
        info = file_stat(path, stat.S_IFREG)
        if info.st_size > 4096:
            reject("journal-oversize")
        record = read_json(path)
        valid = valid_outcome(record, generation, previous_id, previous_outcome)
        if valid["transition_id"] in transitions or valid["evidence"]["report_sha256"] in reports:
            reject("record-reused")
        transitions.add(valid["transition_id"])
        reports.add(valid["evidence"]["report_sha256"])
        validate_report(report_root, valid)
        raw = path.read_bytes()
        records.append(valid)
        head = raw
        previous_id = valid["transition_id"]
        previous_outcome = valid["outcome"]
    mode_path = state_dir / "mode.json"
    if mode_path.is_symlink():
        reject("mode-symlink")
    mode_bytes = None
    if mode_path.exists():
        info = file_stat(mode_path, stat.S_IFREG)
        if info.st_size > 4096:
            reject("mode-oversize")
        mode_bytes = mode_path.read_bytes()
    if mode_bytes == head and root_temporaries:
        reject("state-ambiguous")
    if root_temporaries:
        transition = records[-1]["transition_id"]
        if len(root_temporaries) != 1 or root_temporaries[0][0].name != f".mode.json.tmp-{transition}" or root_temporaries[0][1] != head:
            reject("state-ambiguous")
    if journal_temporaries:
        transition = records[-1]["transition_id"]
        if len(journal_temporaries) != 1 or journal_temporaries[0] != (f".tmp-{transition}", head):
            reject("journal-ambiguous")
    return records, head, mode_bytes


def docker_volume_exists(name: str) -> None:
    try:
        result = subprocess.run(["docker", "volume", "inspect", name], check=True, capture_output=True, text=True, timeout=5)
        inspected = json.loads(result.stdout, object_pairs_hook=strict_pairs)
    except (OSError, subprocess.SubprocessError, json.JSONDecodeError):
        reject("volume-inspect")
    if not isinstance(inspected, list) or len(inspected) != 1 or not isinstance(inspected[0], dict) or inspected[0].get("Name") != name:
        reject("volume-name")


def expected_mount_read_only(record: dict[str, object]) -> bool:
    return record["outcome"] == "s3-accepted"


def assert_override_has_no_blob_overrides(override_path: pathlib.Path) -> None:
    try:
        override = override_path.read_text(encoding="utf-8")
    except OSError:
        reject("override-unavailable")
    if re.search(r"blob-mode|/run/telegramd/blob-mode", override, re.IGNORECASE):
        reject("override-mode-mount")
    if re.search(r"\bTG_BLOB_[A-Z0-9_]*\b", override):
        reject("override-blob-setting")
    if re.search(r"\btgblobs\b", override, re.IGNORECASE):
        reject("override-tgblobs-mount")


def assert_compose_matches(
    inventory: dict[str, object],
    record: dict[str, object],
    mode_source: str,
    override_path: pathlib.Path,
    allow_unguarded_initial_local: bool = False,
) -> None:
    assert_override_has_no_blob_overrides(override_path)
    services = inventory.get("services")
    if not isinstance(services, list) or not services:
        reject("compose-telegramd-missing")
    names = {service.get("name") for service in services if isinstance(service, dict)}
    expected_backend = record["backend"]
    expected_read_only = expected_mount_read_only(record)
    for service in services:
        if not isinstance(service, dict) or service.get("backend") != expected_backend:
            reject("render-backend-mismatch")
        mode_mounts = service.get("blob_mode_mounts", [])
        if allow_unguarded_initial_local and record["outcome"] == "initial-local" and not mode_mounts:
            pass
        elif mode_mounts != [{"type": "bind", "source": mode_source, "target": MODE_TARGET, "read_only": True}]:
            reject("render-mode-mount-invalid")
        blob_mounts = service.get("tgblobs_mounts", [])
        if len(blob_mounts) != 1:
            reject("render-tgblobs-mount")
        blob_mount = blob_mounts[0]
        if blob_mount.get("source") != record["volumes"]["tgblobs"] or blob_mount.get("read_only") != expected_read_only:
            reject("render-volume-mismatch")
    volumes = inventory.get("volumes")
    if not isinstance(volumes, dict) or volumes.get("tgblobs") != record["volumes"]["tgblobs"]:
        reject("render-volume-mismatch")
    if record["volumes"]["rustfsdata"] is not None and volumes.get("rustfsdata") != record["volumes"]["rustfsdata"]:
        reject("render-rustfs-volume-mismatch")
    # Every configured telegramd replica is guarded; unrelated services must
    # not inherit the mount (checked by compose_inventory).
    if not names:
        reject("compose-telegramd-missing")


def assert_containers_match(
    inventory: dict[str, object],
    record: dict[str, object],
    mode_source: str,
    allow_no_telegramd: bool = False,
    allow_unguarded_initial_local: bool = False,
) -> None:
    containers = inventory.get("containers")
    if not isinstance(containers, list):
        reject("container-inventory")
    telegramd = [item for item in containers if isinstance(item, dict) and str(item.get("service", "")).startswith("telegramd")]
    rustfs = [item for item in containers if isinstance(item, dict) and item.get("service") == "rustfs"]
    if any(item.get("mode_mounts") for item in containers if isinstance(item, dict) and not str(item.get("service", "")).startswith("telegramd")):
        reject("running-mode-mount-helper")
    if not telegramd and not allow_no_telegramd:
        reject("running-telegramd-missing")
    read_only = expected_mount_read_only(record)
    names = set()
    for container in telegramd:
        if container.get("backend") != record["backend"]:
            reject("running-backend-mismatch")
        mounts = container.get("tgblobs_mounts", [])
        if len(mounts) != 1:
            reject("running-tgblobs-mount")
        mount = mounts[0]
        if mount.get("type") != "volume" or mount.get("name") != record["volumes"]["tgblobs"] or mount.get("rw") == read_only:
            reject("running-volume-mismatch")
        names.add(container["service"])
        mode_mounts = container.get("mode_mounts", [])
        if mode_mounts:
            if mode_mounts != [{"type": "bind", "source": mode_source, "target": MODE_TARGET, "read_only": True}]:
                reject("running-mode-mount-invalid")
        elif not (allow_unguarded_initial_local and record["outcome"] == "initial-local"):
            reject("running-mode-mount-missing")
    if record["volumes"]["rustfsdata"] is not None:
        docker_volume_exists(record["volumes"]["rustfsdata"])
        if any(mount.get("name") != record["volumes"]["rustfsdata"] for container in rustfs for mount in container.get("rustfs_mounts", [])):
            reject("running-rustfs-volume-mismatch")
        if record["outcome"] == "s3-accepted" and not any(
            mount.get("type") == "volume" and mount.get("name") == record["volumes"]["rustfsdata"] and mount.get("rw") is True
            for container in rustfs for mount in container.get("rustfs_mounts", [])
        ):
            reject("running-rustfs-mount-missing")
    docker_volume_exists(record["volumes"]["tgblobs"])


def validate_runtime(args: argparse.Namespace, allow_no_telegramd: bool = False) -> dict[str, object]:
    records, head, mode_bytes = read_authority(args.state_dir, args.report_root)
    if mode_bytes != head:
        reject("mode-head-mismatch")
    record = records[-1]
    containers = read_json(args.containers)
    compose = read_json(args.compose)
    if not isinstance(containers, dict) or not isinstance(compose, dict):
        reject("inventory-shape")
    mode_source = os.path.realpath(args.checkout / ".state" / "blob-mode")
    assert_containers_match(
        containers,
        record,
        mode_source,
        allow_no_telegramd,
        args.allow_unguarded_initial_local_containers,
    )
    assert_compose_matches(compose, record, mode_source, args.override, args.allow_unguarded_initial_local)
    running_names = {item.get("service") for item in containers["containers"] if str(item.get("service", "")).startswith("telegramd")}
    compose_names = {item.get("name") for item in compose["services"]}
    if not running_names.issubset(compose_names):
        reject("running-service-not-rendered")
    return record


def write_synced(path: pathlib.Path, data: bytes, mode: int, exclusive: bool = True) -> None:
    flags = os.O_WRONLY | os.O_CREAT | os.O_CLOEXEC
    flags |= os.O_EXCL if exclusive else os.O_TRUNC
    try:
        fd = os.open(path, flags, mode)
        with os.fdopen(fd, "wb") as output:
            output.write(data)
            output.flush()
            os.fchown(output.fileno(), 0, 0)
            os.fchmod(output.fileno(), mode)
            os.fsync(output.fileno())
    except OSError:
        reject("publication-write")


def fsync_dir(path: pathlib.Path) -> None:
    try:
        fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    except OSError:
        reject("publication-sync")


def require_runner_lock(path: pathlib.Path) -> None:
    if os.geteuid() != 0:
        reject("root-required")
    try:
        descriptor = os.fstat(9)
        lock_stat = path.stat()
        target = os.readlink("/proc/self/fd/9")
        if (descriptor.st_dev, descriptor.st_ino) != (lock_stat.st_dev, lock_stat.st_ino) or target != str(path):
            reject("shared-lock-missing")
        fcntl.flock(9, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except Reject:
        raise
    except OSError:
        reject("shared-lock-missing")


def atomic_publish_state(state_dir: pathlib.Path, record_bytes: bytes, transition_id: str) -> None:
    state_created = not state_dir.exists()
    secure_dir(state_dir, create=True, mode=0o755)
    if state_created:
        fsync_dir(state_dir.parent)
    journal = state_dir / "journal"
    journal_created = not journal.exists()
    secure_dir(journal, create=True, mode=0o755)
    if journal_created:
        fsync_dir(state_dir)
    entry = journal / "0000000001.json"
    temp = journal / f".tmp-{transition_id}"
    write_synced(temp, record_bytes, 0o644)
    try:
        os.link(temp, entry)
    except FileExistsError:
        reject("journal-generation-exists")
    except OSError:
        reject("journal-publication")
    fsync_dir(journal)
    try:
        temp.unlink()
    except OSError:
        reject("journal-temp-cleanup")
    fsync_dir(journal)

    mode_path = state_dir / "mode.json"
    mode_temp = state_dir / f".mode.json.tmp-{transition_id}"
    write_synced(mode_temp, record_bytes, 0o644)
    try:
        os.replace(mode_temp, mode_path)
    except OSError:
        reject("mode-publication")
    fsync_dir(state_dir)


def prepare_initial_state(state_dir: pathlib.Path) -> list[pathlib.Path]:
    if state_dir.is_symlink():
        reject("state-symlink")
    if not state_dir.exists():
        return []
    file_stat(state_dir, stat.S_IFDIR)
    try:
        entries = list(state_dir.iterdir())
    except OSError:
        reject("state-unavailable")
    mode_path = state_dir / "mode.json"
    if mode_path.exists() or mode_path.is_symlink():
        reject("state-already-exists")
    unexpected = [entry for entry in entries if entry.name != "journal"]
    if unexpected:
        reject("state-already-exists")
    journal = state_dir / "journal"
    if journal not in entries:
        return []
    secure_dir(journal)
    try:
        journal_entries = list(journal.iterdir())
    except OSError:
        reject("journal-unavailable")
    for entry in journal_entries:
        match = re.fullmatch(r"\.tmp-([0-9a-f-]{36})", entry.name)
        if match is None or not canonical_uuid(match.group(1)):
            reject("state-already-exists")
        file_stat(entry, stat.S_IFREG)
    return sorted(journal_entries, key=lambda entry: entry.name)


def cleanup_initial_state(state_dir: pathlib.Path, expected_temporaries: list[pathlib.Path]) -> None:
    current_temporaries = prepare_initial_state(state_dir)
    if current_temporaries != expected_temporaries:
        reject("state-changed")
    for entry in current_temporaries:
        try:
            entry.unlink()
        except OSError:
            reject("state-cleanup")
    if current_temporaries:
        fsync_dir(state_dir / "journal")
    if state_dir.exists():
        fsync_dir(state_dir)
        fsync_dir(state_dir.parent)


def init_local(args: argparse.Namespace) -> None:
    require_runner_lock(args.lock_path)
    state_dir = args.state_dir
    if state_dir.parent.is_symlink():
        reject("state-parent-symlink")
    if state_dir.parent.exists():
        file_stat(state_dir.parent, stat.S_IFDIR)
    initial_temporaries = prepare_initial_state(state_dir)
    if args.baseline_containers.exists() is False or args.current_containers.exists() is False:
        reject("baseline-inspection-missing")
    baseline = read_json(args.baseline_containers)
    current = read_json(args.current_containers)
    baseline_compose_inventory = read_json(args.baseline_compose)
    target_compose_inventory = read_json(args.target_compose)
    if not all(
        isinstance(item, dict)
        for item in (baseline, current, baseline_compose_inventory, target_compose_inventory)
    ):
        reject("inventory-shape")
    checkout = args.checkout
    source = os.path.realpath(checkout / ".state" / "blob-mode")
    baseline_items = baseline.get("containers")
    current_items = current.get("containers")
    if (
        not isinstance(baseline_items, list)
        or not isinstance(current_items, list)
        or any(not isinstance(item, dict) for item in (*baseline_items, *current_items))
    ):
        reject("baseline-inspection-invalid")
    base_telegramd = [item for item in baseline_items if str(item.get("service", "")).startswith("telegramd")]
    current_telegramd = [item for item in current_items if str(item.get("service", "")).startswith("telegramd")]
    if not base_telegramd or not current_telegramd:
        reject("running-telegramd-missing")
    base_records = {(item.get("service"), item.get("id")): item for item in base_telegramd}
    current_records = {(item.get("service"), item.get("id")): item for item in current_telegramd}
    if len(base_records) != len(base_telegramd) or len(current_records) != len(current_telegramd) or base_records != current_records:
        reject("baseline-container-changed")
    initial_backend = None
    tgblobs_name = None
    for container in current_telegramd:
        backend = container.get("backend")
        if not isinstance(backend, dict) or backend.get("kind") != "local":
            reject("initial-baseline-not-local")
        if initial_backend is None:
            initial_backend = backend
        if backend != initial_backend:
            reject("baseline-backend-mismatch")
        mounts = container.get("tgblobs_mounts", [])
        if len(mounts) != 1 or mounts[0].get("type") != "volume" or mounts[0].get("rw") is not True:
            reject("baseline-tgblobs-mount")
        if tgblobs_name is None:
            tgblobs_name = mounts[0].get("name")
        if not tgblobs_name or mounts[0].get("name") != tgblobs_name:
            reject("baseline-volume-mismatch")
        if container.get("mode_mounts"):
            reject("baseline-already-guarded")
    if not isinstance(initial_backend, dict) or initial_backend.get("dir") != BLOB_TARGET:
        reject("initial-backend-dir")

    assert_compose_matches_initial(
        baseline_compose_inventory, target_compose_inventory, initial_backend, tgblobs_name, source
    )
    running_names = {item.get("service") for item in current_telegramd}
    target_names = {item.get("name") for item in target_compose_inventory.get("services", [])}
    if not running_names.issubset(target_names):
        reject("running-service-not-rendered")
    assert_override_has_no_blob_overrides(args.override)
    # Bind the report to the current live inventory and named reviewed target.
    target_sha = args.target_sha
    baseline_sha = args.baseline_sha
    if not re.fullmatch(r"[0-9a-f]{40}", target_sha) or not re.fullmatch(r"[0-9a-f]{40}", baseline_sha):
        reject("revision-id")
    transition_id = str(uuid.uuid4())
    backend = {"kind": "local", "dir": initial_backend["dir"]}
    report = {
        "schema": REPORT_SCHEMA,
        "generation": 1,
        "transition_id": transition_id,
        "outcome": "initial-local",
        "backend": backend,
        "inspection_kind": "unguarded-local-baseline",
        "baseline_sha": baseline_sha,
        "target_sha": target_sha,
        "journal_entries": 0,
        "containers": current,
        "compose": baseline_compose_inventory,
    }
    report_bytes = (json.dumps(report, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
    report_root = args.report_root
    secure_dir(report_root, create=True, mode=0o700)
    report_file = report_path(report_root, transition_id)
    write_synced(report_file, report_bytes, 0o600)
    fsync_dir(report_root)
    report_sha = hashlib.sha256(report_bytes).hexdigest()
    record = {
        "schema": SCHEMA,
        "generation": 1,
        "transition_id": transition_id,
        "supersedes": None,
        "outcome": "initial-local",
        "backend": backend,
        "volumes": {"tgblobs": tgblobs_name, "rustfsdata": None},
        "evidence": {"report_sha256": report_sha},
        "published_at": dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
    }
    record_bytes = (json.dumps(record, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
    if len(record_bytes) > 4096:
        reject("record-oversize")
    cleanup_initial_state(state_dir, initial_temporaries)
    secure_state_parent(state_dir)
    atomic_publish_state(state_dir, record_bytes, transition_id)
    print(f"blob_mode=initialized outcome=initial-local generation=1 transition_id={transition_id}")


def assert_compose_matches_initial(baseline: dict[str, object], target: dict[str, object], backend: dict[str, str], tgblobs_name: str, mode_source: str) -> None:
    old_services = baseline.get("services")
    new_services = target.get("services")
    if not isinstance(old_services, list) or not isinstance(new_services, list):
        reject("compose-services")
    for services, guarded in ((old_services, False), (new_services, True)):
        if not services:
            reject("compose-telegramd-missing")
        for service in services:
            if not isinstance(service, dict) or service.get("backend") != backend:
                reject("initial-render-backend")
            mounts = service.get("tgblobs_mounts", [])
            if len(mounts) != 1 or mounts[0].get("source") != tgblobs_name or mounts[0].get("read_only") is not False:
                reject("initial-render-volume")
            mode_mounts = service.get("blob_mode_mounts", [])
            if guarded:
                if mode_mounts != [{"type": "bind", "source": mode_source, "target": MODE_TARGET, "read_only": True}]:
                    reject("initial-render-mode-mount")
            elif mode_mounts:
                reject("baseline-already-guarded")
    if baseline.get("volumes", {}).get("tgblobs") != tgblobs_name or target.get("volumes", {}).get("tgblobs") != tgblobs_name:
        reject("initial-render-volume")
    docker_volume_exists(tgblobs_name)


def reconcile(args: argparse.Namespace) -> None:
    require_runner_lock(args.lock_path)
    records, head, mode_bytes = read_authority(args.state_dir, args.report_root)
    if mode_bytes == head:
        fsync_dir(args.state_dir)
        print("blob_mode=reconciled result=already-current")
        return
    prior_bytes = {
        pathlib.Path(args.state_dir / "journal" / f"{index:010d}.json").read_bytes()
        for index in range(1, len(records))
    }
    if mode_bytes is not None and mode_bytes not in prior_bytes:
        reject("reconcile-mode-ambiguous")
    record = records[-1]
    if record["outcome"] != "initial-local":
        reject("transition-reconcile-unavailable")
    validate_report(args.report_root, record)
    sync_report(args.report_root, record)
    transition_id = record["transition_id"]
    mode_temp = args.state_dir / f".mode.json.tmp-{transition_id}"
    if mode_temp.exists():
        file_stat(mode_temp, stat.S_IFREG)
        if mode_temp.read_bytes() != head:
            reject("reconcile-temp-mismatch")
        try:
            fd = os.open(mode_temp, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
        except OSError:
            reject("publication-sync")
    else:
        write_synced(mode_temp, head, 0o644)
    try:
        os.replace(mode_temp, args.state_dir / "mode.json")
    except OSError:
        reject("mode-publication")
    fsync_dir(args.state_dir)
    print(f"blob_mode=reconciled generation={record['generation']} transition_id={transition_id}")


def sync_report(report_root: pathlib.Path, record: dict[str, object]) -> None:
    path = report_path(report_root, record["transition_id"])
    before = file_stat(path, stat.S_IFREG, 0o600)
    try:
        fd = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
        try:
            after = os.fstat(fd)
            if (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino):
                reject("report-changed")
            os.fsync(fd)
        finally:
            os.close(fd)
    except Reject:
        raise
    except OSError:
        reject("report-sync")
    fsync_dir(report_root)


def main() -> int:
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    compose = commands.add_parser("compose")
    compose.add_argument("--checkout", type=pathlib.Path, required=True)
    containers = commands.add_parser("containers")
    containers.add_argument("--checkout", type=pathlib.Path, required=True)
    containers.add_argument("--allow-empty", action="store_true")
    validate = commands.add_parser("validate")
    validate.add_argument("--state-dir", type=pathlib.Path, required=True)
    validate.add_argument("--report-root", type=pathlib.Path, required=True)
    validate.add_argument("--containers", type=pathlib.Path, required=True)
    validate.add_argument("--compose", type=pathlib.Path, required=True)
    validate.add_argument("--override", type=pathlib.Path, required=True)
    validate.add_argument("--checkout", type=pathlib.Path, required=True)
    validate.add_argument("--allow-empty-containers", action="store_true")
    validate.add_argument("--allow-unguarded-initial-local", action="store_true")
    validate.add_argument("--allow-unguarded-initial-local-containers", action="store_true")
    initial = commands.add_parser("initialize-local")
    initial.add_argument("--state-dir", type=pathlib.Path, required=True)
    initial.add_argument("--report-root", type=pathlib.Path, required=True)
    initial.add_argument("--baseline-containers", type=pathlib.Path, required=True)
    initial.add_argument("--current-containers", type=pathlib.Path, required=True)
    initial.add_argument("--baseline-compose", type=pathlib.Path, required=True)
    initial.add_argument("--target-compose", type=pathlib.Path, required=True)
    initial.add_argument("--override", type=pathlib.Path, required=True)
    initial.add_argument("--checkout", type=pathlib.Path, required=True)
    initial.add_argument("--target-sha", required=True)
    initial.add_argument("--baseline-sha", required=True)
    initial.add_argument("--lock-path", type=pathlib.Path, required=True)
    recovery = commands.add_parser("reconcile")
    recovery.add_argument("--state-dir", type=pathlib.Path, required=True)
    recovery.add_argument("--report-root", type=pathlib.Path, required=True)
    recovery.add_argument("--lock-path", type=pathlib.Path, required=True)
    args = parser.parse_args()
    try:
        if args.command == "compose":
            print(json.dumps(compose_inventory(stdin_json(), args.checkout), sort_keys=True, separators=(",", ":")))
        elif args.command == "containers":
            print(json.dumps(container_inventory(stdin_json(), args.checkout, args.allow_empty), sort_keys=True, separators=(",", ":")))
        elif args.command == "validate":
            record = validate_runtime(args)
            print(f"blob_mode=valid outcome={record['outcome']} generation={record['generation']} transition_id={record['transition_id']}")
        elif args.command == "initialize-local":
            init_local(args)
        else:
            reconcile(args)
    except Reject as error:
        print(f"blob authority rejected: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
