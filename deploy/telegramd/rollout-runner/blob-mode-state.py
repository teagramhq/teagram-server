#!/usr/bin/env python3
"""Inspect and publish the runner-owned durable blob-mode authority."""

from __future__ import annotations

import argparse
import datetime as dt
import fcntl
import hashlib
import importlib.util
import json
import os
import pathlib
import posixpath
import re
import stat
import subprocess
import sys
import uuid
from collections.abc import Iterator


MODE_TARGET = "/run/telegramd/blob-mode"
BLOB_TARGET = "/var/lib/telegramd-blobs"
KEY_TARGET = "/var/lib/telegramd"
PGDATA_TARGET = "/var/lib/postgresql/data"
SCHEMA = "teagram.blob-mode/v1"
REPORT_SCHEMA = "teagram.blob-mode-report/v1"
TRANSITION_REPORT_SCHEMA = "teagram.blob-mode-report/v2"
TRANSITION_PROOF_SCHEMA = "teagram.blob-transition-proof/v2"
NON_SERVING_SERVICES = {"rustfs", "rustfs-init", "migrate", "blob-migrate", "blob-restore"}
DATABASE_SERVICES = {"postgres"}
INITIAL_LOCAL_GUARD_ENV = {"TG_REPLICA_COUNT": "1", "TG_CLIENT_ADDR_TRUST": "socket"}
IMAGE_ENV_DEFAULTS = {"TG_RSA_KEY_PATH": "/var/lib/telegramd/server_key.pem"}
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
S3_TRANSITION_PHASES = {
    "baseline_inventory_sha256",
    "candidate_compose_sha256",
    "qualification_bundle_sha256",
    "qualification_output_sha256",
    "frozen_inventory_sha256",
    "dump_sha256",
    "reference_rows_sha256",
    "active_links_sha256",
    "schema_evidence_sha256",
    "source_provisional_sha256",
    "source_frozen_sha256",
    "copy_pass_1_sha256",
    "copy_pass_2_sha256",
    "destination_census_pass_1_sha256",
    "destination_census_pass_2_sha256",
}
RECOVERY_TRANSITION_PHASES_V1 = {
    "frozen_inventory_sha256",
    "dump_sha256",
    "schema_evidence_sha256",
    "s3_census_pass_1_sha256",
    "s3_census_pass_2_sha256",
    "local_before_restore_sha256",
    "restore_pass_1_sha256",
    "restore_pass_2_sha256",
    "local_census_pass_1_sha256",
    "local_census_pass_2_sha256",
    "retained_keys_sha256",
}
RECOVERY_TRANSITION_PHASES = RECOVERY_TRANSITION_PHASES_V1 | {
    "recovery_bundle_sha256",
    "qualification_output_sha256",
    "s3_compose_sha256",
    "local_compose_sha256",
    "recovery_reference_rows_sha256",
    "recovery_active_links_sha256",
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


def read_private_json(path: pathlib.Path) -> object:
    file_stat(path, stat.S_IFREG, 0o600)
    return read_json(path)


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
    values = raw_environment_map(raw, compose)
    relevant = {"TG_BLOB_DIR", *S3_FIELDS}
    return {key: value for key, value in values.items() if key in relevant}


def raw_environment_map(raw: object, compose: bool) -> dict[str, str]:
    result: dict[str, str] = {}
    if isinstance(raw, list):
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
    for key, value in pairs:
        if not isinstance(key, str):
            reject("environment-shape")
        if not key.startswith("TG_"):
            continue
        if key in result:
            reject("duplicate-environment-key")
        if value is None:
            reject("unresolved-environment")
        if not isinstance(value, str):
            value = str(value)
        result[key] = value
    return result


def telegramd_environment_sha256(values: dict[str, str]) -> str:
    # These settings intentionally change as part of the initial guarded rollout.
    comparable = {
        key: value for key, value in values.items()
        if key not in INITIAL_LOCAL_GUARD_ENV
    }
    encoded = json.dumps(comparable, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def compose_ports(service: dict[str, object]) -> list[dict[str, str]]:
    raw = service.get("ports", [])
    if not isinstance(raw, list):
        reject("compose-ports")
    result = []
    for port in raw:
        if not isinstance(port, dict):
            reject("compose-port")
        target = port.get("target")
        if type(target) is not int or target < 1 or target > 65535:
            reject("compose-port")
        published = port.get("published", "")
        host_ip = port.get("host_ip", "")
        protocol = port.get("protocol", "tcp")
        mode = port.get("mode", "")
        if not isinstance(published, (str, int)) or not isinstance(host_ip, str) or not isinstance(protocol, str) or not isinstance(mode, str):
            reject("compose-port")
        # Compose reports ingress for short-syntax ports, while inspect only
        # reports the effective host bindings. Compare their shared fields.
        result.append({
            "target": str(target), "published": str(published),
            "host_ip": host_ip, "protocol": protocol,
        })
    return sorted(result, key=lambda item: (item["host_ip"], item["published"], item["target"], item["protocol"]))


def container_ports(container: dict[str, object]) -> list[dict[str, str]]:
    host_config = container.get("HostConfig", {})
    bindings = host_config.get("PortBindings", {}) if isinstance(host_config, dict) else None
    if bindings is None:
        bindings = {}
    if not isinstance(bindings, dict):
        reject("container-ports")
    result = []
    for key, entries in bindings.items():
        if not isinstance(key, str) or "/" not in key:
            reject("container-ports")
        target, protocol = key.rsplit("/", 1)
        if not target.isdigit() or not 1 <= int(target) <= 65535 or not isinstance(protocol, str):
            reject("container-ports")
        if entries is None:
            entries = []
        if not isinstance(entries, list):
            reject("container-ports")
        for entry in entries:
            if not isinstance(entry, dict):
                reject("container-ports")
            host_ip = entry.get("HostIp", "")
            host_port = entry.get("HostPort", "")
            if not isinstance(host_ip, str) or not isinstance(host_port, str):
                reject("container-ports")
            result.append({
                "target": target, "published": host_port,
                "host_ip": host_ip, "protocol": protocol,
            })
    return sorted(result, key=lambda item: (item["host_ip"], item["published"], item["target"], item["protocol"]))


def compose_named_mounts(
    service: dict[str, object], compose: dict[str, object], volume_key: str, target_root: str,
) -> list[dict[str, object]]:
    raw = service.get("volumes", [])
    if not isinstance(raw, list):
        reject("compose-mounts")
    volumes = resolved_volumes(compose)
    result = []
    for mount in raw:
        if not isinstance(mount, dict):
            reject("compose-mount")
        target = mount.get("target", "")
        if not isinstance(target, str):
            reject("compose-mount")
        if target == target_root or target.startswith(target_root + "/"):
            source = mount.get("source", "")
            kind = mount.get("type", "")
            if kind != "volume" or source != volume_key or volumes.get(volume_key) is None:
                reject("compose-named-mount")
            result.append({
                "type": kind, "source": volumes[volume_key], "target": target,
                "read_only": mount.get("read_only", False) is True,
            })
    return sorted(result, key=lambda item: (str(item["target"]), str(item["source"])))


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
    for key in ("tgkey", "pgdata"):
        if key not in raw:
            continue
        item = raw[key]
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
        environment_values = raw_environment_map(service.get("environment", {}), True)
        effective_environment = dict(IMAGE_ENV_DEFAULTS)
        effective_environment.update(environment_values)
        environment = {key: value for key, value in environment_values.items() if key in {"TG_BLOB_DIR", *S3_FIELDS}}
        mode_mounts, blob_mounts = service_mounts(service, mode_source, compose)
        result_services.append({
            "name": name,
            "backend": backend_from_values(environment),
            "blob_mode_mounts": mode_mounts,
            "tgblobs_mounts": blob_mounts,
            "ports": compose_ports(service),
            "tgkey_mounts": compose_named_mounts(service, compose, "tgkey", KEY_TARGET),
            "tg_environment_sha256": telegramd_environment_sha256(effective_environment),
            "initial_local_guard_environment": {
                key: environment_values.get(key) for key in INITIAL_LOCAL_GUARD_ENV
            },
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
    postgres_mounts: list[dict[str, object]] = []
    postgres = services.get("postgres")
    if postgres is not None:
        if not isinstance(postgres, dict):
            reject("compose-service")
        postgres_mounts = compose_named_mounts(postgres, compose, "pgdata", PGDATA_TARGET)
    return {
        "services": result_services, "volumes": volume_names,
        "postgres_mounts": postgres_mounts, "mode_source": mode_source,
    }


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


def container_tg_environment(container: dict[str, object]) -> dict[str, str]:
    config = container.get("Config")
    if not isinstance(config, dict):
        reject("container-config")
    return raw_environment_map(config.get("Env", []), False)


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
        key_mounts = []
        pgdata_mounts = []
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
            if destination == KEY_TARGET or destination.startswith(KEY_TARGET + "/"):
                key_mounts.append({"type": kind, "source": mount.get("Name", ""), "target": destination, "read_only": not rw})
            if destination == PGDATA_TARGET or destination.startswith(PGDATA_TARGET + "/"):
                pgdata_mounts.append({"type": kind, "source": mount.get("Name", ""), "target": destination, "read_only": not rw})
        is_telegramd = isinstance(service, str) and service.startswith("telegramd")
        is_helper = isinstance(service, str) and (service in NON_SERVING_SERVICES or service in DATABASE_SERVICES)
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
            record["ports"] = container_ports(item)
            record["tgkey_mounts"] = key_mounts
            record["tg_environment_sha256"] = telegramd_environment_sha256(container_tg_environment(item))
        elif service == "rustfs":
            record["rustfs_mounts"] = rustfs_mounts
        elif service == "postgres":
            record["pgdata_mounts"] = pgdata_mounts
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


def private_phase_path(report_root: pathlib.Path, relative: object) -> pathlib.Path:
    if not isinstance(relative, str):
        reject("transition-proof-invalid")
    relative_path = pathlib.PurePosixPath(relative)
    if relative_path.is_absolute() or ".." in relative_path.parts or not relative_path.parts or "\\" in relative:
        reject("transition-proof-invalid")
    phase_path = report_root.joinpath(*relative_path.parts)
    parent = phase_path.parent
    try:
        resolved_root = report_root.resolve(strict=True)
        resolved_parent = parent.resolve(strict=True)
    except OSError:
        reject("transition-proof-invalid")
    if not resolved_parent.is_relative_to(resolved_root):
        reject("transition-proof-invalid")
    current = parent
    while current != report_root:
        secure_dir(current)
        current = current.parent
    file_stat(phase_path, stat.S_IFREG, 0o600)
    return phase_path


def hash_phase_file(path: pathlib.Path, sync: bool = False) -> str:
    info = file_stat(path, stat.S_IFREG, 0o600)
    hasher = hashlib.sha256()
    try:
        fd = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
        try:
            after = os.fstat(fd)
            if (info.st_dev, info.st_ino) != (after.st_dev, after.st_ino):
                reject("transition-proof-evidence-changed")
            with os.fdopen(fd, "rb", closefd=False) as stream:
                for block in iter(lambda: stream.read(1024 * 1024), b""):
                    hasher.update(block)
            if sync:
                os.fsync(fd)
        finally:
            os.close(fd)
    except Reject:
        raise
    except OSError:
        reject("transition-proof-evidence-sync" if sync else "transition-report-evidence")
    return hasher.hexdigest()


def iter_manifest(path: pathlib.Path) -> Iterator[tuple[str, int, str]]:
    previous = ""
    try:
        with path.open("rb") as stream:
            for raw_line in stream:
                key, size, content_sha = parse_manifest_line(raw_line)
                if previous and previous >= key:
                    reject("transition-manifest-invalid")
                previous = key
                yield key, size, content_sha
    except Reject:
        raise
    except (OSError, ValueError):
        reject("transition-manifest-invalid")


def manifest_summary(path: pathlib.Path) -> tuple[str, int, int]:
    hasher = hashlib.sha256()
    objects = 0
    total_bytes = 0
    previous = ""
    try:
        with path.open("rb") as stream:
            for raw_line in stream:
                hasher.update(raw_line)
                key, size, _content_sha = parse_manifest_line(raw_line)
                if previous and previous >= key:
                    reject("transition-manifest-invalid")
                previous = key
                objects += 1
                total_bytes += size
                if total_bytes > 9_223_372_036_854_775_807:
                    reject("transition-manifest-invalid")
    except Reject:
        raise
    except OSError:
        reject("transition-manifest-invalid")
    return hasher.hexdigest(), objects, total_bytes


def parse_manifest_line(raw_line: bytes) -> tuple[str, int, str]:
    if not raw_line.endswith(b"\n") or b"\r" in raw_line:
        reject("transition-manifest-invalid")
    try:
        fields = raw_line[:-1].decode("utf-8").split("\t")
    except UnicodeDecodeError:
        reject("transition-manifest-invalid")
    if len(fields) != 3:
        reject("transition-manifest-invalid")
    key, size_text, content_sha = fields
    if (
        not key
        or key.startswith("/")
        or "\\" in key
        or any(part in ("", ".", "..") for part in key.split("/"))
        or any(part.startswith(".tmp") or part.endswith(".tmp") for part in key.split("/"))
        or any(not re.fullmatch(r"[A-Za-z0-9._-]+", part) for part in key.split("/"))
        or re.fullmatch(r"0|[1-9][0-9]*", size_text) is None
        or not digest(content_sha)
    ):
        reject("transition-manifest-invalid")
    size = int(size_text)
    if size > 9_223_372_036_854_775_807:
        reject("transition-manifest-invalid")
    return key, size, content_sha


def validate_phase_files(
    report_root: pathlib.Path,
    phase_digests: dict[str, object],
    phase_files: dict[str, object],
    sync: bool = False,
) -> dict[str, pathlib.Path]:
    if set(phase_files) != set(phase_digests):
        reject("transition-report-evidence")
    result: dict[str, pathlib.Path] = {}
    paths_seen: set[pathlib.Path] = set()
    inodes_seen: set[tuple[int, int]] = set()
    for name, expected in phase_digests.items():
        path = private_phase_path(report_root, phase_files[name])
        info = path.lstat()
        identity = (info.st_dev, info.st_ino)
        if path in paths_seen or identity in inodes_seen:
            reject("transition-report-evidence")
        paths_seen.add(path)
        inodes_seen.add(identity)
        if hash_phase_file(path, sync=sync) != expected:
            reject("transition-proof-evidence-digest" if sync else "transition-report-evidence")
        if sync:
            current = path.parent
            while current != report_root:
                fsync_dir(current)
                current = current.parent
            fsync_dir(report_root)
        result[name] = path
    return result


def compare_manifest_summaries(paths: list[pathlib.Path]) -> tuple[str, int, int]:
    summaries = [manifest_summary(path) for path in paths]
    if not summaries or any(summary != summaries[0] for summary in summaries[1:]):
        reject("transition-report-evidence")
    return summaries[0]


def validate_live_schema_dump_binding(paths: dict[str, pathlib.Path], phase_digests: dict[str, object]) -> None:
    schema_document = read_json(paths["schema_evidence_sha256"])
    capture = schema_document.get("live_capture") if isinstance(schema_document, dict) else None
    qualifier_path = pathlib.Path(__file__).with_name("qualify-rustfs-transition.py")
    spec = importlib.util.spec_from_file_location("transition_schema_query", qualifier_path)
    if spec is None or spec.loader is None:
        reject("transition-report-evidence")
    qualifier = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(qualifier)
    except (ImportError, OSError, ValueError):
        reject("transition-report-evidence")
    migrations = schema_document if isinstance(schema_document, dict) else None
    release_set = migrations.get("release_set", "60-66") if migrations is not None else None
    if (
        not isinstance(capture, dict)
        or set(capture) != {"schema", "captured_at", "dump_sha256", "query_sha256", "query_output_sha256", "observed"}
        or capture.get("schema") != "teagram.live-migration-schema/v1"
        or capture.get("dump_sha256") != phase_digests.get("dump_sha256")
        or capture.get("query_sha256") != qualifier.LIVE_SCHEMA_QUERY_SHA256
        or not isinstance(capture.get("query_output_sha256"), str)
        or re.fullmatch(r"[0-9a-f]{64}", capture["query_output_sha256"]) is None
        or not isinstance(capture.get("captured_at"), str)
        or not isinstance(capture.get("observed"), dict)
        or release_set not in ("60-66", "60-67")
    ):
        reject("transition-report-evidence")
    try:
        qualifier.validate_live_schema_observation(migrations, capture["observed"], release_set)
    except qualifier.GateReject:
        reject("transition-report-evidence")
    baseline_capture = schema_document.get("baseline_live_capture") if isinstance(schema_document, dict) else None
    if release_set == "60-67":
        if (
            not isinstance(baseline_capture, dict)
            or set(baseline_capture) != {"schema", "captured_at", "dump_sha256", "query_sha256", "query_output_sha256", "observed"}
            or baseline_capture.get("schema") != "teagram.live-migration-schema/v1"
            or baseline_capture.get("dump_sha256") != phase_digests.get("dump_sha256")
            or baseline_capture.get("query_sha256") != qualifier.LIVE_SCHEMA_QUERY_SHA256
            or not isinstance(baseline_capture.get("query_output_sha256"), str)
            or re.fullmatch(r"[0-9a-f]{64}", baseline_capture["query_output_sha256"]) is None
            or baseline_capture.get("observed") != capture.get("observed")
            or not isinstance(baseline_capture.get("captured_at"), str)
        ):
            reject("transition-report-evidence")
    elif baseline_capture is not None:
        reject("transition-report-evidence")
    try:
        captured_at = dt.datetime.fromisoformat(capture["captured_at"].replace("Z", "+00:00"))
    except ValueError:
        reject("transition-report-evidence")
    if captured_at.tzinfo is None or captured_at.utcoffset() != dt.timedelta(0):
        reject("transition-report-evidence")
    if release_set == "60-67":
        try:
            baseline_captured_at = dt.datetime.fromisoformat(
                baseline_capture["captured_at"].replace("Z", "+00:00")
            )
        except ValueError:
            reject("transition-report-evidence")
        if (
            baseline_captured_at.tzinfo is None
            or baseline_captured_at.utcoffset() != dt.timedelta(0)
            or baseline_captured_at > captured_at
        ):
            reject("transition-report-evidence")


def validate_restored_union(
    local_before: pathlib.Path,
    s3_source: pathlib.Path,
    retained: pathlib.Path,
    local_after: pathlib.Path,
) -> int:
    before = iter(iter_manifest(local_before))
    source = iter(iter_manifest(s3_source))
    retained_rows = iter(iter_manifest(retained))
    after = iter(iter_manifest(local_after))
    before_row = next(before, None)
    source_row = next(source, None)
    retained_row = next(retained_rows, None)
    after_row = next(after, None)
    retained_count = 0

    while before_row is not None or source_row is not None:
        if source_row is None or (before_row is not None and before_row[0] < source_row[0]):
            expected = before_row
            before_row = next(before, None)
            if retained_row != expected:
                reject("transition-local-only-not-preserved")
            retained_count += 1
            retained_row = next(retained_rows, None)
        elif before_row is None or source_row[0] < before_row[0]:
            expected = source_row
            source_row = next(source, None)
        else:
            expected = source_row
            before_row = next(before, None)
            source_row = next(source, None)
        if after_row != expected:
            reject("transition-restored-manifest-mismatch")
        after_row = next(after, None)

    if retained_row is not None or after_row is not None:
        reject("transition-restored-manifest-mismatch")
    return retained_count


def validate_transition_artifacts(
    report_root: pathlib.Path,
    outcome: str,
    evidence: dict[str, object],
    phase_digests: dict[str, object],
    phase_files: dict[str, object],
    sync: bool = False,
) -> None:
    paths = validate_phase_files(report_root, phase_digests, phase_files, sync=sync)
    validate_live_schema_dump_binding(paths, phase_digests)
    if outcome == "s3-accepted":
        manifest_names = (
            "source_provisional_sha256", "source_frozen_sha256", "copy_pass_1_sha256",
            "copy_pass_2_sha256", "destination_census_pass_1_sha256", "destination_census_pass_2_sha256",
        )
        manifest_sha, objects, total_bytes = compare_manifest_summaries([paths[name] for name in manifest_names])
        if (
            manifest_sha != evidence.get("source_manifest_sha256")
            or manifest_sha != evidence.get("destination_manifest_sha256")
            or type(evidence.get("object_count")) is not int
            or evidence.get("object_count") != objects
            or type(evidence.get("byte_total")) is not int
            or evidence.get("byte_total") != total_bytes
        ):
            reject("transition-report-evidence")
        return

    s3_names = (
        "s3_census_pass_1_sha256", "s3_census_pass_2_sha256",
        "restore_pass_1_sha256", "restore_pass_2_sha256",
    )
    s3_sha, _s3_objects, _s3_bytes = compare_manifest_summaries([paths[name] for name in s3_names])
    local_names = ("local_census_pass_1_sha256", "local_census_pass_2_sha256")
    local_sha, local_objects, local_bytes = compare_manifest_summaries([paths[name] for name in local_names])
    before_sha, _before_objects, _before_bytes = manifest_summary(paths["local_before_restore_sha256"])
    retained_sha, _retained_objects, _retained_bytes = manifest_summary(paths["retained_keys_sha256"])
    retained_count = validate_restored_union(
        paths["local_before_restore_sha256"],
        paths["s3_census_pass_1_sha256"],
        paths["retained_keys_sha256"],
        paths["local_census_pass_1_sha256"],
    )
    if (
        s3_sha != evidence.get("s3_census_manifest_sha256")
        or local_sha != evidence.get("restored_manifest_sha256")
        or local_objects != evidence.get("object_count")
        or local_bytes != evidence.get("byte_total")
        or retained_count != evidence.get("retained_cutover_key_count")
        or retained_sha != phase_digests.get("retained_keys_sha256")
        or before_sha != phase_digests.get("local_before_restore_sha256")
    ):
        reject("transition-report-evidence")
    qualifier_path = pathlib.Path(__file__).with_name("qualify-rustfs-transition.py")
    spec = importlib.util.spec_from_file_location("transition_qualifier", qualifier_path)
    if spec is None or spec.loader is None:
        reject("transition-reference-coverage")
    qualifier = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(qualifier)
        files, _reference_keys, required_keys = qualifier.parse_references(paths["recovery_reference_rows_sha256"])
        qualifier.validate_active_links(paths["recovery_active_links_sha256"], files)
        s3_rows, _s3_digest, _s3_bytes = qualifier.read_manifest(paths["s3_census_pass_1_sha256"])
    except (OSError, ValueError, qualifier.GateReject):
        reject("transition-reference-coverage")
    if not required_keys or not required_keys <= {row[0] for row in s3_rows}:
        reject("transition-reference-coverage")


def validate_report(report_root: pathlib.Path, record: dict[str, object]) -> None:
    secure_dir(report_root)
    report_file = report_path(report_root, record["transition_id"])
    file_stat(report_file, stat.S_IFREG, 0o600)
    try:
        raw = report_file.read_bytes()
    except OSError:
        reject("report-unavailable")
    if hashlib.sha256(raw).hexdigest() != record["evidence"]["report_sha256"]:
        reject("report-digest")
    report = read_json(report_file)
    if not isinstance(report, dict) or report.get("schema") not in (REPORT_SCHEMA, TRANSITION_REPORT_SCHEMA):
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
        if report.get("schema") != REPORT_SCHEMA:
            reject("report-schema")
        inspection_kind = report.get("inspection_kind")
        if (
            inspection_kind not in ("unguarded-local-baseline", "unguarded-local-baseline-to-pinned-target")
            or type(report.get("generation")) is not int
            or type(report.get("journal_entries")) is not int
            or report["journal_entries"] != 0
            or not isinstance(report.get("baseline_sha"), str)
            or re.fullmatch(r"[0-9a-f]{40}", report["baseline_sha"]) is None
            or not isinstance(report.get("target_sha"), str)
            or re.fullmatch(r"[0-9a-f]{40}", report["target_sha"]) is None
        ):
            reject("initial-report-provenance")
        if inspection_kind == "unguarded-local-baseline":
            # Read reports emitted by the first rollout-runner version. New
            # reports below keep the live baseline and guarded target render
            # under separate, explicitly named provenance fields.
            containers = report.get("containers")
            compose = report.get("compose")
            expected_mode_mount = []
        else:
            baseline = report.get("baseline")
            target = report.get("target")
            if (
                not isinstance(baseline, dict)
                or baseline.get("source") != "running-unguarded-containers"
                or not isinstance(target, dict)
                or target.get("source") != "pinned-target-compose"
                or not digest(target.get("artifact_sha256"))
            ):
                reject("initial-report-inspection")
            containers = baseline.get("containers")
            compose = target.get("compose")
            mode_source = compose.get("mode_source") if isinstance(compose, dict) else None
            if (
                not isinstance(containers, dict)
                or not isinstance(compose, dict)
                or not isinstance(mode_source, str)
                or containers.get("mode_source") != mode_source
            ):
                reject("initial-report-inspection")
            expected_mode_mount = [{
                "type": "bind", "source": mode_source,
                "target": MODE_TARGET, "read_only": True,
            }]
        if not isinstance(containers, dict) or not isinstance(compose, dict):
            reject("initial-report-inspection")
        container_items = containers.get("containers")
        rendered_services = compose.get("services")
        if (
            not isinstance(container_items, list)
            or not isinstance(rendered_services, list)
            or not rendered_services
        ):
            reject("initial-report-inspection")
        inspected_services = [
            item for item in container_items
            if isinstance(item, dict) and str(item.get("service", "")).startswith("telegramd")
        ]
        if not inspected_services:
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
                or item.get("blob_mode_mounts") != expected_mode_mount
                or not isinstance(mounts, list)
                or len(mounts) != 1
                or not isinstance(mounts[0], dict)
                or mounts[0].get("source") != record["volumes"]["tgblobs"]
                or mounts[0].get("read_only") is not False
            ):
                reject("initial-report-inspection")
        compose_volumes = compose.get("volumes")
        if (
            not isinstance(compose_volumes, dict)
            or compose_volumes.get("tgblobs") != record["volumes"]["tgblobs"]
            or (inspection_kind != "unguarded-local-baseline" and compose_volumes.get("rustfsdata") is not None)
        ):
            reject("initial-report-inspection")
    else:
        if report.get("schema") == TRANSITION_REPORT_SCHEMA:
            expected_phases = S3_TRANSITION_PHASES if record["outcome"] == "s3-accepted" else RECOVERY_TRANSITION_PHASES
        elif record["outcome"] == "s3-accepted":
            expected_phases = S3_TRANSITION_PHASES
        else:
            expected_phases = RECOVERY_TRANSITION_PHASES_V1
        expected_evidence = dict(record["evidence"])
        expected_evidence.pop("report_sha256")
        phases = report.get("phase_digests")
        phase_files = report.get("phase_files")
        if (
            set(report) != {"schema", "generation", "transition_id", "outcome", "backend", "volumes", "evidence", "phase_digests", "phase_files"}
            or report.get("volumes") != record["volumes"]
            or report.get("evidence") != expected_evidence
            or not isinstance(phases, dict)
            or set(phases) != expected_phases
            or not all(digest(value) for value in phases.values())
            or not isinstance(phase_files, dict)
        ):
            reject("transition-report-evidence")
        validate_transition_artifacts(report_root, record["outcome"], expected_evidence, phases, phase_files)
        if record["outcome"] == "s3-accepted":
            if (
                phases.get("copy_pass_1_sha256") != phases.get("copy_pass_2_sha256")
                or phases.get("source_provisional_sha256") != phases.get("source_frozen_sha256")
                or phases.get("source_frozen_sha256") != phases.get("destination_census_pass_1_sha256")
                or phases.get("destination_census_pass_1_sha256") != phases.get("destination_census_pass_2_sha256")
                or phases.get("source_frozen_sha256") != record["evidence"]["source_manifest_sha256"]
                or record["evidence"]["source_manifest_sha256"] != record["evidence"]["destination_manifest_sha256"]
            ):
                reject("transition-report-evidence")
        elif (
            phases.get("s3_census_pass_1_sha256") != phases.get("s3_census_pass_2_sha256")
            or phases.get("s3_census_pass_1_sha256") != record["evidence"]["s3_census_manifest_sha256"]
            or phases.get("restore_pass_1_sha256") != phases.get("restore_pass_2_sha256")
            or phases.get("local_census_pass_1_sha256") != phases.get("local_census_pass_2_sha256")
            or phases.get("local_census_pass_1_sha256") != record["evidence"]["restored_manifest_sha256"]
        ):
            reject("transition-report-evidence")


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


def atomic_publish_state(state_dir: pathlib.Path, record_bytes: bytes, transition_id: str, generation: int = 1) -> None:
    state_created = not state_dir.exists()
    secure_dir(state_dir, create=True, mode=0o755)
    if state_created:
        fsync_dir(state_dir.parent)
    journal = state_dir / "journal"
    journal_created = not journal.exists()
    secure_dir(journal, create=True, mode=0o755)
    if journal_created:
        fsync_dir(state_dir)
    entry = journal / f"{generation:010d}.json"
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


def assert_compose_matches_initial(
    baseline_containers: dict[str, object],
    target: dict[str, object],
    backend: dict[str, str],
    tgblobs_name: str,
    mode_source: str,
) -> None:
    running_items = baseline_containers.get("containers")
    target_services = target.get("services")
    if not isinstance(running_items, list) or not isinstance(target_services, list) or not target_services:
        reject("compose-telegramd-missing")
    running_names = {
        item.get("service") for item in running_items
        if isinstance(item, dict) and str(item.get("service", "")).startswith("telegramd")
    }
    target_names = {item.get("name") for item in target_services if isinstance(item, dict)}
    if not running_names or running_names != target_names:
        reject("running-service-not-rendered")
    running_by_name = {
        item.get("service"): item for item in running_items
        if isinstance(item, dict) and str(item.get("service", "")).startswith("telegramd")
    }
    expected_mode_mount = [{
        "type": "bind", "source": mode_source,
        "target": MODE_TARGET, "read_only": True,
    }]
    for service in target_services:
        if not isinstance(service, dict) or service.get("backend") != backend:
            reject("initial-render-backend")
        running = running_by_name.get(service.get("name"))
        if not isinstance(running, dict):
            reject("running-service-not-rendered")
        if service.get("blob_mode_mounts") != expected_mode_mount:
            reject("initial-render-mode-mount")
        mounts = service.get("tgblobs_mounts", [])
        if (
            not isinstance(mounts, list)
            or len(mounts) != 1
            or not isinstance(mounts[0], dict)
            or mounts[0].get("source") != tgblobs_name
            or mounts[0].get("read_only") is not False
        ):
            reject("initial-render-volume")
        if service.get("ports") != running.get("ports"):
            reject("initial-render-exposure")
        key_mounts = service.get("tgkey_mounts")
        if (
            not isinstance(key_mounts, list)
            or len(key_mounts) != 1
            or not isinstance(key_mounts[0], dict)
            or not isinstance(running.get("tgkey_mounts"), list)
            or len(running["tgkey_mounts"]) != 1
            or not isinstance(running["tgkey_mounts"][0], dict)
            or key_mounts[0].get("type") != "volume"
            or key_mounts[0].get("target") != KEY_TARGET
            or key_mounts[0].get("read_only") is not False
            or key_mounts != running.get("tgkey_mounts")
        ):
            reject("initial-render-key-mount")
        if service.get("tg_environment_sha256") != running.get("tg_environment_sha256"):
            reject("initial-render-environment")
        if service.get("name") == "telegramd" and service.get("initial_local_guard_environment") != INITIAL_LOCAL_GUARD_ENV:
            reject("initial-render-guard-environment")
    volumes = target.get("volumes")
    if (
        not isinstance(volumes, dict)
        or volumes.get("tgblobs") != tgblobs_name
        or volumes.get("rustfsdata") is not None
        or target.get("mode_source") != mode_source
    ):
        reject("initial-render-volume")
    postgres_items = [
        item for item in running_items
        if isinstance(item, dict) and item.get("service") == "postgres"
    ]
    target_pgdata = target.get("postgres_mounts")
    if len(postgres_items) != 1 or not isinstance(target_pgdata, list) or len(target_pgdata) != 1:
        reject("initial-render-pgdata-mount")
    live_pgdata = postgres_items[0].get("pgdata_mounts")
    if (
        not isinstance(live_pgdata, list)
        or len(live_pgdata) != 1
        or not isinstance(target_pgdata[0], dict)
        or not isinstance(live_pgdata[0], dict)
        or target_pgdata != live_pgdata
        or target_pgdata[0].get("type") != "volume"
        or target_pgdata[0].get("source") != volumes.get("pgdata")
        or target_pgdata[0].get("target") != PGDATA_TARGET
        or target_pgdata[0].get("read_only") is not False
    ):
        reject("initial-render-pgdata-mount")
    docker_volume_exists(tgblobs_name)


def validate_initial_local_transition(
    baseline: object,
    current: object,
    preflight_target: object,
    target: object,
    checkout: pathlib.Path,
    target_sha: str,
    baseline_sha: str,
    target_artifact_sha256: str,
    override: pathlib.Path,
) -> tuple[dict[str, str], str]:
    if not all(isinstance(item, dict) for item in (baseline, current, preflight_target, target)):
        reject("inventory-shape")
    if (
        re.fullmatch(r"[0-9a-f]{40}", target_sha) is None
        or re.fullmatch(r"[0-9a-f]{40}", baseline_sha) is None
    ):
        reject("revision-id")
    if not digest(target_artifact_sha256):
        reject("target-artifact-digest")
    source = os.path.realpath(checkout / ".state" / "blob-mode")
    baseline_inventory = baseline
    current_inventory = current
    baseline_items = baseline_inventory.get("containers")
    current_items = current_inventory.get("containers")
    if (
        not isinstance(baseline_items, list)
        or not isinstance(current_items, list)
        or any(not isinstance(item, dict) for item in (*baseline_items, *current_items))
        or baseline_inventory.get("mode_source") != source
        or current_inventory.get("mode_source") != source
    ):
        reject("baseline-inspection-invalid")
    if baseline_inventory != current_inventory:
        reject("baseline-container-changed")
    running = [item for item in baseline_items if str(item.get("service", "")).startswith("telegramd")]
    if not running:
        reject("running-telegramd-missing")
    if any(
        not isinstance(item, dict)
        or (not str(item.get("service", "")).startswith("telegramd") and item.get("mode_mounts"))
        for item in baseline_items
    ):
        reject("mode-mount-helper")
    services = [item.get("service") for item in running]
    container_ids = [item.get("id") for item in running]
    if len(set(services)) != len(services) or len(set(container_ids)) != len(container_ids):
        reject("baseline-inspection-invalid")

    initial_backend: dict[str, str] | None = None
    tgblobs_name: str | None = None
    for container in running:
        backend = container.get("backend")
        if not isinstance(backend, dict) or backend.get("kind") != "local":
            reject("initial-baseline-not-local")
        if initial_backend is None:
            initial_backend = backend
        elif backend != initial_backend:
            reject("baseline-backend-mismatch")
        mounts = container.get("tgblobs_mounts", [])
        if (
            not isinstance(mounts, list)
            or len(mounts) != 1
            or not isinstance(mounts[0], dict)
            or mounts[0].get("type") != "volume"
            or mounts[0].get("target") != BLOB_TARGET
            or mounts[0].get("rw") is not True
        ):
            reject("baseline-tgblobs-mount")
        if tgblobs_name is None:
            tgblobs_name = mounts[0].get("name")
        if not isinstance(tgblobs_name, str) or not tgblobs_name or mounts[0].get("name") != tgblobs_name:
            reject("baseline-volume-mismatch")
        if container.get("mode_mounts"):
            reject("baseline-already-guarded")
    if not isinstance(initial_backend, dict) or initial_backend.get("dir") != BLOB_TARGET:
        reject("initial-backend-dir")
    if preflight_target != target:
        reject("target-compose-changed")
    assert_compose_matches_initial(baseline_inventory, target, initial_backend, tgblobs_name, source)
    assert_override_has_no_blob_overrides(override)
    return {"kind": "local", "dir": initial_backend["dir"]}, tgblobs_name


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


def validate_state_parent(state_dir: pathlib.Path) -> None:
    if state_dir.parent.is_symlink():
        reject("state-parent-symlink")
    if state_dir.parent.exists():
        file_stat(state_dir.parent, stat.S_IFDIR)


def prepare_initial_transition(args: argparse.Namespace) -> tuple[dict[str, str], str, dict[str, object], dict[str, object], dict[str, object]]:
    state_dir = args.state_dir
    validate_state_parent(state_dir)
    prepare_initial_state(state_dir)
    baseline = read_private_json(args.baseline_containers)
    current_path = getattr(args, "current_containers", args.baseline_containers)
    current = read_private_json(current_path)
    preflight_target = read_private_json(args.preflight_target_compose)
    target = read_private_json(getattr(args, "target_compose", args.preflight_target_compose))
    backend, tgblobs_name = validate_initial_local_transition(
        baseline,
        current,
        preflight_target,
        target,
        args.checkout,
        args.target_sha,
        args.baseline_sha,
        args.target_artifact_sha256,
        args.override,
    )
    return backend, tgblobs_name, baseline, target, current


def preflight_initial_local(args: argparse.Namespace) -> None:
    require_runner_lock(args.lock_path)
    backend, tgblobs_name, _, target, _ = prepare_initial_transition(args)
    print(
        "blob_mode=preflight outcome=initial-local "
        f"backend={backend['kind']} tgblobs={tgblobs_name} "
        f"target_services={len(target['services'])}"
    )


def init_local(args: argparse.Namespace) -> None:
    require_runner_lock(args.lock_path)
    state_dir = args.state_dir
    validate_state_parent(state_dir)
    initial_temporaries = prepare_initial_state(state_dir)
    backend, tgblobs_name, baseline, target, _ = prepare_initial_transition(args)
    target_sha = args.target_sha
    baseline_sha = args.baseline_sha
    transition_id = str(uuid.uuid4())
    report = {
        "schema": REPORT_SCHEMA,
        "generation": 1,
        "transition_id": transition_id,
        "outcome": "initial-local",
        "backend": backend,
        "inspection_kind": "unguarded-local-baseline-to-pinned-target",
        "baseline_sha": baseline_sha,
        "target_sha": target_sha,
        "journal_entries": 0,
        "baseline": {
            "source": "running-unguarded-containers",
            "containers": baseline,
        },
        "target": {
            "source": "pinned-target-compose",
            "artifact_sha256": args.target_artifact_sha256,
            "compose": target,
        },
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


def publish_transition(args: argparse.Namespace) -> None:
    require_runner_lock(args.lock_path)
    if args.outcome not in ("s3-accepted", "recovered-local"):
        reject("transition-outcome")
    state_dir = args.state_dir
    report_root = args.report_root
    if state_dir.parent.is_symlink() or report_root.is_symlink():
        reject("state-symlink")
    secure_state_parent(state_dir)
    records, head, mode_bytes = read_authority(state_dir, report_root)
    if mode_bytes != head:
        reject("transition-authority-stale")
    previous = records[-1]
    if args.outcome == "s3-accepted" and previous["outcome"] not in ("initial-local", "recovered-local"):
        reject("transition-predecessor")
    if args.outcome == "recovered-local" and previous["outcome"] != "s3-accepted":
        reject("transition-predecessor")

    proof_path = args.proof
    file_stat(proof_path, stat.S_IFREG, 0o600)
    if proof_path.stat().st_size > 1_048_576:
        reject("transition-proof-oversize")
    proof = read_json(proof_path)
    if (
        not isinstance(proof, dict)
        or set(proof) != {"schema", "outcome", "backend", "volumes", "evidence", "phase_digests", "phase_files"}
        or proof.get("schema") != TRANSITION_PROOF_SCHEMA
        or proof.get("outcome") != args.outcome
    ):
        reject("transition-proof-invalid")
    expected_phases = S3_TRANSITION_PHASES if args.outcome == "s3-accepted" else RECOVERY_TRANSITION_PHASES
    phases = proof.get("phase_digests")
    phase_files = proof.get("phase_files")
    if (
        not isinstance(phases, dict)
        or set(phases) != expected_phases
        or not all(digest(value) for value in phases.values())
        or not isinstance(phase_files, dict)
        or set(phase_files) != expected_phases
    ):
        reject("transition-proof-invalid")
    backend = proof.get("backend")
    volumes = proof.get("volumes")
    evidence = proof.get("evidence")
    if not isinstance(backend, dict) or not isinstance(volumes, dict) or not isinstance(evidence, dict):
        reject("transition-proof-invalid")
    secure_dir(report_root, create=True, mode=0o700)
    validate_transition_artifacts(report_root, args.outcome, evidence, phases, phase_files, sync=True)
    if set(volumes) != {"tgblobs", "rustfsdata"} or not all(isinstance(volumes.get(key), str) and volumes[key] for key in volumes):
        reject("transition-proof-invalid")
    if volumes["tgblobs"] != previous["volumes"]["tgblobs"]:
        reject("transition-volume-changed")
    prior_rustfsdata = previous["volumes"]["rustfsdata"]
    if prior_rustfsdata is not None and volumes["rustfsdata"] != prior_rustfsdata:
        reject("transition-volume-changed")
    if args.outcome == "s3-accepted":
        if set(backend) != {"kind", "endpoint", "bucket", "prefix"} or backend != {
            "kind": "s3", "endpoint": "http://rustfs:9000", "bucket": "telegram", "prefix": "telegramd/"
        }:
            reject("transition-backend")
        expected = {"source_manifest_sha256", "destination_manifest_sha256", "object_count", "byte_total", "copy_passes"}
        if set(evidence) != expected:
            reject("transition-proof-invalid")
    else:
        if set(backend) != {"kind", "dir"} or backend != {"kind": "local", "dir": BLOB_TARGET}:
            reject("transition-backend")
        expected = {
            "s3_census_manifest_sha256", "restored_manifest_sha256", "object_count", "byte_total",
            "restore_passes", "retained_cutover_key_count",
        }
        if set(evidence) != expected:
            reject("transition-proof-invalid")

    generation = len(records) + 1
    transition_id = str(uuid.uuid4())
    report = {
        "schema": TRANSITION_REPORT_SCHEMA,
        "generation": generation,
        "transition_id": transition_id,
        "outcome": args.outcome,
        "backend": backend,
        "volumes": volumes,
        "evidence": evidence,
        "phase_digests": phases,
        "phase_files": phase_files,
    }
    report_bytes = (json.dumps(report, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
    report_file = report_path(report_root, transition_id)
    write_synced(report_file, report_bytes, 0o600)
    fsync_dir(report_root)
    report_sha = hashlib.sha256(report_bytes).hexdigest()
    record = {
        "schema": SCHEMA,
        "generation": generation,
        "transition_id": transition_id,
        "supersedes": previous["transition_id"],
        "outcome": args.outcome,
        "backend": backend,
        "volumes": volumes,
        "evidence": {**evidence, "report_sha256": report_sha},
        "published_at": dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
    }
    valid_outcome(record, generation, previous["transition_id"], previous["outcome"])
    record_bytes = (json.dumps(record, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
    if len(record_bytes) > 4096:
        reject("record-oversize")
    validate_report(report_root, record)
    atomic_publish_state(state_dir, record_bytes, transition_id, generation)
    published_evidence = record["evidence"]
    print(
        f"blob_mode=published outcome={args.outcome} generation={generation} "
        f"object_count={published_evidence['object_count']} "
        f"byte_total={published_evidence['byte_total']} report_sha256={report_sha}"
        + (f" retained_cutover_key_count={published_evidence['retained_cutover_key_count']}" if args.outcome == "recovered-local" else "")
    )


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
    def add_initial_local_arguments(command: argparse.ArgumentParser, include_current: bool) -> None:
        command.add_argument("--state-dir", type=pathlib.Path, required=True)
        command.add_argument("--baseline-containers", type=pathlib.Path, required=True)
        if include_current:
            command.add_argument("--current-containers", type=pathlib.Path, required=True)
            command.add_argument("--target-compose", type=pathlib.Path, required=True)
        command.add_argument("--preflight-target-compose", type=pathlib.Path, required=True)
        command.add_argument("--target-artifact-sha256", required=True)
        command.add_argument("--override", type=pathlib.Path, required=True)
        command.add_argument("--checkout", type=pathlib.Path, required=True)
        command.add_argument("--target-sha", required=True)
        command.add_argument("--baseline-sha", required=True)
        command.add_argument("--lock-path", type=pathlib.Path, required=True)

    preflight = commands.add_parser("preflight-initial-local")
    add_initial_local_arguments(preflight, include_current=False)
    initial = commands.add_parser("initialize-local")
    initial.add_argument("--report-root", type=pathlib.Path, required=True)
    add_initial_local_arguments(initial, include_current=True)
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
            record = validate_runtime(args, args.allow_empty_containers)
            print(f"blob_mode=valid outcome={record['outcome']} generation={record['generation']} transition_id={record['transition_id']}")
        elif args.command == "preflight-initial-local":
            preflight_initial_local(args)
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
