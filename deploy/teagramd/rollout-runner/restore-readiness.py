#!/usr/bin/env python3
"""Run the synthetic-only PostgreSQL 16 to 18 application restore gate."""

from __future__ import annotations

import argparse
import fcntl
import hashlib
import ipaddress
import json
import os
import re
import secrets
import select
import shutil
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import threading
import time
from pathlib import Path
from typing import Any, Callable


# Use the public, digest-preserving mirror because this gate refuses ambient registry credentials.
SOURCE_IMAGE = "mirror.gcr.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea"
RESTORE_IMAGE = "mirror.gcr.io/library/postgres@sha256:77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873"
ROOT = Path(__file__).resolve().parents[3]
SEED_COMMIT = "7b62b9ab1cad0c459627ad7a2eb736a40382939b"
PREPARED_DIRECTORY_PREFIX = "teagram-restore-readiness."
PREPARED_ATTESTATION = "prepared.json"
PREPARED_ARTIFACTS = {
    "atlas",
    "teagramd",
    "teagramd-seed",
    "restore-readiness-launcher",
    "restore-readiness-seed",
    "restore-readiness-seed-shim",
    "restore-readiness-provisioner",
    "restore-drill-checker",
    "restore-readiness-probe",
}
PREPARED_BUILD_FLAGS = ["-trimpath", "-buildvcs=false", "-p=1"]
ROLLOUT_LOCK_PATH = Path("/tmp/teagram-deploy.lock")
BASE_SCHEMA = "20261021000003"
# The base migrations landed with the head store check, so the fixture is seeded by the
# last release before them and then migrated to BASE_SCHEMA.
SEED_SCHEMA = "20261021000002"


def repo_migration_versions(migration_dir: Path | None = None) -> tuple[str, ...]:
    directory = migration_dir or ROOT / "migrations"
    try:
        entries = sorted(directory.glob("*.sql"))
    except OSError as exc:
        raise ValueError("repository migrations are unavailable") from exc
    versions: list[str] = []
    for entry in entries:
        if entry.is_symlink() or not entry.is_file():
            raise ValueError("invalid repository migration")
        match = re.fullmatch(r"([0-9]{14})_[A-Za-z0-9_-]+\.sql", entry.name)
        if match is None:
            raise ValueError("invalid repository migration")
        versions.append(match.group(1))
    if not versions or len(versions) != len(set(versions)):
        raise ValueError("invalid repository migration set")
    return tuple(versions)


HEAD_SCHEMA = repo_migration_versions()[-1]
# Keep the dump fixture on a stable pre-sweep-index schema. New migrations stay
# in the post-restore upgrade path instead of changing fixture generation.
LOCAL_FIXTURE_SCHEMA = "20261024000002"
if not BASE_SCHEMA < LOCAL_FIXTURE_SCHEMA < HEAD_SCHEMA:
    raise ValueError("local fixture needs a repo migration strictly between the base and head")
AUTH_KEY_INVENTORY_PATH = Path(__file__).with_name("auth-key-clearing-inventory.json")
AUTH_KEY_FIXTURE_COUNT = 3
LOCAL_UNENROLLED_AUTH_KEY_FIXTURE_COUNT = 2
AUTH_KEY_FIXTURE_STATE = ["3", "2", "3", "1", "2", "0"]
AUTH_KEY_CLEARED_STATE = ["0", "0", "0", "1", "2", "2"]
AUTH_KEY_CLEAR_ACK = b"RESTORE_READINESS_AUTH_KEY_DELETE_APPLIED"
# Roles the repository migrations create; the restore imports no globals, so the target has none.
MIGRATION_ROLES = "browser_acceptance_probe"
LOCAL_ATTESTATION_ROLE = "browser_acceptance_probe"
WORK_SECONDS = 30 * 60
FAULT_WORK_SECONDS = 8 * 60
CLEANUP_SECONDS = 4 * 60
CLEANUP_REAP_SECONDS = 1
CLEANUP_FINALIZE_SECONDS = 0.25
SUPERVISOR_SECONDS = WORK_SECONDS + CLEANUP_SECONDS
SAFE_PHASES = {
    "preflight",
    "fixture_generation",
    "source_migrations",
    "restore",
    "schema_validation",
    "pending_migration",
    "auth_key_clearing",
    "daemon_startup",
    "application_assertions",
    "cleanup",
}
SAFE_CATEGORIES = {
    "preflight_failure",
    "isolation_refusal",
    "fixture_failure",
    "migration_failure",
    "restore_failure",
    "schema_failure",
    "startup_failure",
    "identity_collision",
    "provisioning_failure",
    "auth_failure",
    "assertion_failure",
    "auth_key_clear_failure",
    "fault_test_failure",
    "timeout",
    "interrupted",
    "cleanup_failure",
    "assertions_passed",
    "rollout_in_progress",
}
SAFE_FIXTURE_CHECKS = {
    "build_images",
    "create_source_database",
    "fixture_setup",
    "prepare_materials",
    "seed_source_fixture",
    "source_auth_key_fixture",
    "create_local_dump_fixture",
    "run_local_fixture_sql",
    "run_local_hostile_fixture_tests",
    "write_sigkill_marker",
}
CLEANUP_FAILURE_STEPS = frozenset(
    {"gateway", "processes", "build-inputs", "files", "foreign-inventory", "containers", "images", "networks-and-volumes"}
)
FOREIGN_INVENTORY_SUMMARY_ITEM = r"(?:[a-z-]+\(invalid-shape\)|(?:containers|volumes|networks|images)\(added=[0-9]+,removed=[0-9]+,fields=(?:none|[a-z_,]+)\))"
FOREIGN_INVENTORY_DIAGNOSTIC = re.compile(
    rf"(?:unverified:(?:DrillFailure|KeyError|TypeError|ValueError|JSONDecodeError|UnicodeDecodeError)|"
    rf"changed:(?:unknown|{FOREIGN_INVENTORY_SUMMARY_ITEM}(?:;{FOREIGN_INVENTORY_SUMMARY_ITEM})*))"
)
SAFE_CLEANUP_OPERATIONS = {
    "build_input_removal",
    "cleanup_deadline",
    "cleanup_processes",
    "cleanup_resources",
    "docker_container_inspect",
    "docker_container_inventory",
    "docker_container_remove",
    "docker_container_stop",
    "docker_image_inspect",
    "docker_image_inventory",
    "docker_image_remove",
    "docker_image_use_check",
    "docker_network_inventory",
    "docker_volume_inspect",
    "docker_volume_inventory",
    "docker_volume_remove",
    "docker_volume_use_check",
    "gateway_shutdown",
    "network_volume_cleanup",
    "owned_container_cleanup",
    "owned_image_cleanup",
    "run_directory_finalization",
    "run_file_removal",
    "supervisor_setup",
}
SAFE_CLEANUP_ERRORS = {
    "command_failed",
    "command_timeout",
    "deadline_expired",
    "invalid_output",
    "supervisor_failed",
    "verification_failed",
}
SAFE_SUPERVISOR_STAGES = frozenset(
    {
        "check_cleanup_deadline",
        "cleanup_resources",
        "build_runtime_args",
        "finalize_run_directory",
        "initialize_cleanup_drill",
        "load_final_manifest",
        "load_initial_manifest",
        "register_supervisor",
        "spawn_supervisor",
        "verify_lock_descriptors",
        "verify_supervisor_identity",
        "wait_for_parent",
    }
)
IMAGE_TOKEN_LABEL = "com.teagram.restore-readiness.token"
IMAGE_ROLE_LABEL = "com.teagram.restore-readiness.role"
COMPOSE_PROJECT_LABEL = "com.docker.compose.project"
APP_MEMORY = 768 * 1024 * 1024
PG_MEMORY = 1024 * 1024 * 1024
CLIENT_MEMORY = 512 * 1024 * 1024
MAX_CAPTURE = 2 * 1024 * 1024
MAX_FOREIGN_INVENTORY = 1024 * 1024
MAX_ARCHIVE = 256 * 1024 * 1024
MAX_LOCAL_DUMP_BYTES = 512 * 1024 * 1024
MAX_LOCAL_AUTH_KEYS = 100_000
LOCAL_DUMP_NAME = re.compile(r"^pgdump-pre-(?:[0-9a-f]{7,40}|[0-9a-f]{7}-[0-9]{8}T[0-9]{6}Z)\.sql$")
LOCAL_DUMP_LEAK_MARKER = b"RESTORE_READINESS_LOCAL_DUMP_LEAK_MARKER"
RESTORE_BOOTSTRAP_ROLE = "restore_bootstrap"

CONTAINER_INVENTORY_TEMPLATE = r'''{{.Id}}{{"\n"}}{{.Name}}{{"\n"}}{{.Image}}{{"\n"}}{{index .Config "Image"}}{{"\n"}}{{json (index .Config "Labels")}}{{"\n"}}{{index .State "Status"}}{{"\n"}}{{index .State "Running"}}{{"\n"}}{{index .State "Paused"}}{{"\n"}}{{index .State "Restarting"}}{{"\n"}}{{index .State "OOMKilled"}}{{"\n"}}{{index .State "ExitCode"}}{{"\n"}}{{index .State "StartedAt"}}{{"\n"}}{{index .State "FinishedAt"}}{{"\n"}}{{with (index .State "Health")}}{{.Status}}{{else}}none{{end}}{{"\n"}}{{json (index .NetworkSettings "Networks")}}{{"\n"}}{{json .Mounts}}{{"\n"}}{{json (index .Config "ExposedPorts")}}{{"\n"}}{{json (index .HostConfig "PortBindings")}}{{"\n"}}{{index .HostConfig "PublishAllPorts"}}{{"\n"}}{{index .HostConfig "NetworkMode"}}{{"\n"}}{{json (index .HostConfig "Binds")}}{{"\n"}}{{json (index .HostConfig "VolumesFrom")}}{{"\n"}}{{json (index .HostConfig "Links")}}{{"\n"}}{{json (index .HostConfig "ExtraHosts")}}{{"\n"}}{{index .HostConfig "Privileged"}}{{"\n"}}{{json (index .HostConfig "CapAdd")}}{{"\n"}}{{json (index .HostConfig "CapDrop")}}{{"\n"}}{{json (index .HostConfig "SecurityOpt")}}{{"\n"}}{{index .HostConfig "ReadonlyRootfs"}}{{"\n"}}{{json (index .HostConfig "Devices")}}{{"\n"}}{{index .HostConfig "PidMode"}}{{"\n"}}{{index .HostConfig "IpcMode"}}{{"\n"}}{{index .HostConfig "UTSMode"}}{{"\n"}}{{index .HostConfig "UsernsMode"}}{{"\n"}}{{json (index .HostConfig "RestartPolicy")}}'''
NETWORK_INVENTORY_TEMPLATE = r'''{{.Id}}{{"\n"}}{{.Name}}{{"\n"}}{{.Driver}}{{"\n"}}{{.Scope}}{{"\n"}}{{.Internal}}{{"\n"}}{{.Attachable}}{{"\n"}}{{.Ingress}}{{"\n"}}{{.ConfigOnly}}{{"\n"}}{{.EnableIPv6}}{{"\n"}}{{json .ConfigFrom}}{{"\n"}}{{json .IPAM}}{{"\n"}}{{json .Options}}{{"\n"}}{{json .Labels}}{{"\n"}}{{json .Containers}}'''
VOLUME_INVENTORY_TEMPLATE = r'''{{.Name}}{{"\n"}}{{.Driver}}{{"\n"}}{{.Scope}}{{"\n"}}{{.Mountpoint}}{{"\n"}}{{.CreatedAt}}{{"\n"}}{{json .Labels}}{{"\n"}}{{json .Options}}'''


def _inventory_fields(output: bytes, expected: int) -> list[str]:
    fields = output.decode("utf-8", "strict").splitlines()
    if len(fields) != expected:
        raise ValueError("invalid Docker inventory response")
    return fields


def _inventory_json(value: str, expected_type: type | tuple[type, ...]) -> Any:
    parsed = json.loads(value)
    if parsed is not None and not isinstance(parsed, expected_type):
        raise ValueError("invalid Docker inventory field")
    return parsed


def _inventory_labels(value: str) -> dict[str, str] | None:
    labels = _inventory_json(value, dict)
    if labels is not None and any(not isinstance(key, str) or not isinstance(label, str) for key, label in labels.items()):
        raise ValueError("invalid Docker inventory labels")
    return labels


def _inventory_mounts(value: str) -> list[dict[str, Any]]:
    mounts = _inventory_json(value, list)
    if mounts is None:
        return []
    if any(not isinstance(mount, dict) for mount in mounts):
        raise ValueError("invalid Docker mount inventory")
    return sorted(mounts, key=lambda mount: json.dumps(mount, sort_keys=True, separators=(",", ":")))


def collect_docker_resource_inventory(
    call: Callable[[list[str]], bytes], image_token: str | None = None
) -> dict[str, Any]:
    """Read only Docker metadata needed to prove foreign-resource isolation."""
    image_label_filter = f"label={IMAGE_TOKEN_LABEL}"
    if image_token is not None:
        if not re.fullmatch(r"[0-9a-f]{32}", image_token):
            raise ValueError("invalid Docker image token")
        image_label_filter += f"={image_token}"
    containers: dict[str, dict[str, Any]] = {}
    container_ids = [
        item for item in call(["ps", "--all", "--no-trunc", "--format", "{{.ID}}"])
        .decode("utf-8", "strict")
        .splitlines()
        if item
    ]
    for identifier in container_ids:
        if not re.fullmatch(r"[0-9a-f]{64}", identifier) or identifier in containers:
            raise ValueError("invalid Docker container identity")
        fields = _inventory_fields(
            call(["inspect", "--type", "container", "--format", CONTAINER_INVENTORY_TEMPLATE, identifier]),
            35,
        )
        if (
            fields[0] != identifier
            or not fields[1].startswith("/")
            or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", fields[1][1:])
        ):
            raise ValueError("Docker container identity changed during inventory")
        labels = _inventory_labels(fields[4])
        attachments = _inventory_json(fields[14], dict)
        mounts = _inventory_mounts(fields[15])
        exposed_ports = _inventory_json(fields[16], dict)
        port_bindings = _inventory_json(fields[17], dict)
        containers[identifier] = {
            "name": fields[1].lstrip("/"),
            "image_id": fields[2],
            "image_reference": fields[3],
            "labels": labels,
            "state": {
                "status": fields[5],
                "running": fields[6],
                "paused": fields[7],
                "restarting": fields[8],
                "oom_killed": fields[9],
                "exit_code": fields[10],
                "started_at": fields[11],
                "finished_at": fields[12],
                "health": fields[13],
            },
            "attachments": attachments,
            "mounts": mounts,
            "exposed_ports": exposed_ports,
            "published_ports": port_bindings,
            "publish_all_ports": fields[18],
            "network_mode": fields[19],
            "binds": _inventory_json(fields[20], list),
            "volumes_from": _inventory_json(fields[21], list),
            "links": _inventory_json(fields[22], list),
            "extra_hosts": _inventory_json(fields[23], list),
            "privileged": fields[24],
            "cap_add": _inventory_json(fields[25], list),
            "cap_drop": _inventory_json(fields[26], list),
            "security_options": _inventory_json(fields[27], list),
            "readonly_rootfs": fields[28],
            "devices": _inventory_json(fields[29], list),
            "pid_mode": fields[30],
            "ipc_mode": fields[31],
            "uts_mode": fields[32],
            "userns_mode": fields[33],
            "restart_policy": _inventory_json(fields[34], dict),
        }

    volumes: dict[str, dict[str, Any]] = {}
    volume_names = [
        item for item in call(["volume", "ls", "--quiet"]).decode("utf-8", "strict").splitlines() if item
    ]
    for name in volume_names:
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", name) or name in volumes:
            raise ValueError("invalid Docker volume identity")
        fields = _inventory_fields(
            call(["volume", "inspect", "--format", VOLUME_INVENTORY_TEMPLATE, name]),
            7,
        )
        if fields[0] != name:
            raise ValueError("Docker volume identity changed during inventory")
        labels = _inventory_labels(fields[5])
        options = _inventory_json(fields[6], dict)
        if not fields[3].startswith("/"):
            raise ValueError("incomplete Docker volume inventory")
        volumes[name] = {
            "name": name,
            "driver": fields[1],
            "scope": fields[2],
            "mountpoint": fields[3],
            "created_at": fields[4],
            "labels": labels,
            "options": options,
        }

    image_ids = [
        item
        for item in call(
            [
                "image",
                "ls",
                "--all",
                "--quiet",
                "--no-trunc",
                "--filter",
                image_label_filter,
            ]
        )
        .decode("utf-8", "strict")
        .splitlines()
        if item
    ]
    images: dict[str, dict[str, str]] = {}
    for identifier in image_ids:
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", identifier):
            raise ValueError("invalid Docker image identity")
        images[identifier] = {"id": identifier}

    networks: dict[str, dict[str, Any]] = {}
    listed_networks = call(["network", "ls", "--no-trunc", "--format", '{{.ID}}{{"\\t"}}{{.Name}}'])
    for line in listed_networks.decode("utf-8", "strict").splitlines():
        identity, separator, name = line.partition("\t")
        if (
            not separator
            or not re.fullmatch(r"[0-9a-f]{64}", identity)
            or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", name)
            or identity in networks
        ):
            raise ValueError("invalid Docker network identity")
        fields = _inventory_fields(
            call(["network", "inspect", "--format", NETWORK_INVENTORY_TEMPLATE, identity]),
            14,
        )
        if fields[0] != identity or fields[1] != name:
            raise ValueError("Docker network identity changed during inventory")
        labels = _inventory_labels(fields[12])
        containers_attached = _inventory_json(fields[13], dict)
        networks[identity] = {
            "name": name,
            "driver": fields[2],
            "scope": fields[3],
            "internal": fields[4],
            "attachable": fields[5],
            "ingress": fields[6],
            "config_only": fields[7],
            "ipv6_enabled": fields[8],
            "config_from": _inventory_json(fields[9], dict),
            "ipam": _inventory_json(fields[10], dict),
            "options": _inventory_json(fields[11], dict),
            "labels": labels,
            "attachments": containers_attached,
        }

    return {"containers": containers, "volumes": volumes, "networks": networks, "images": images}


def require_no_docker_identity_collisions(inventory: dict[str, Any], token: str, phase: str) -> None:
    if not re.fullmatch(r"[0-9a-f]{32}", token):
        raise DrillFailure(phase, "isolation_refusal")
    name_prefix = f"restore-readiness-{token[:16]}"
    project_name = name_prefix
    for kind in ("containers", "volumes", "networks"):
        for item in inventory.get(kind, {}).values():
            name = item.get("name", "")
            labels = item.get("labels") or {}
            if (
                name == name_prefix
                or name.startswith(f"{name_prefix}-")
                or name.startswith(f"{name_prefix}_")
                or any(value == token for value in labels.values())
                or labels.get(IMAGE_TOKEN_LABEL) == token
                or labels.get(COMPOSE_PROJECT_LABEL) in {token, project_name}
            ):
                raise DrillFailure(phase, "isolation_refusal")


def foreign_resource_inventory(
    inventory: dict[str, Any],
    *,
    owned_container_ids: set[str],
    owned_volume_names: set[str],
    owned_network_ids: set[str],
) -> dict[str, Any]:
    return {
        "containers": {
            identifier: record
            for identifier, record in inventory["containers"].items()
            if identifier not in owned_container_ids
        },
        "volumes": {
            name: record for name, record in inventory["volumes"].items() if name not in owned_volume_names
        },
        "networks": {
            identifier: record
            for identifier, record in inventory["networks"].items()
            if identifier not in owned_network_ids
        },
        "images": inventory["images"],
    }


def manifest_owned_volume_names(
    records: list[dict[str, Any]],
    inventory: dict[str, Any] | None = None,
) -> set[str]:
    """Return volumes proven attached to an exact manifest-owned container."""
    owned: set[str] = set()
    for record in records:
        identifier = record.get("id")
        name = record.get("name")
        if not isinstance(identifier, str) or not re.fullmatch(r"[0-9a-f]{64}", identifier):
            continue
        if not isinstance(name, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", name):
            continue
        claimed = record.get("unexpected_volumes")
        if not isinstance(claimed, list):
            continue
        mounts = record.get("unexpected_volume_mounts")
        if not isinstance(mounts, list):
            continue
        if inventory is not None:
            container = inventory.get("containers", {}).get(identifier)
            if not isinstance(container, dict) or container.get("name") != name:
                continue
            actual_mounts = container.get("mounts")
            if not isinstance(actual_mounts, list):
                continue
        else:
            actual_mounts = None
        for mount in mounts:
            if not isinstance(mount, dict) or mount.get("type") != "volume":
                continue
            volume_name = mount.get("name")
            source = mount.get("source")
            destination = mount.get("destination")
            rw = mount.get("rw")
            if (
                not isinstance(volume_name, str)
                or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", volume_name)
                or volume_name not in claimed
                or not isinstance(source, str)
                or not source.startswith("/")
                or not isinstance(destination, str)
                or not destination.startswith("/")
                or not isinstance(rw, bool)
            ):
                continue
            if actual_mounts is not None:
                matching_mounts = [
                    actual
                    for actual in actual_mounts
                    if isinstance(actual, dict)
                    and actual.get("Type") == "volume"
                    and actual.get("Name") == volume_name
                    and actual.get("Source") == source
                    and actual.get("Destination") == destination
                    and actual.get("RW") is rw
                ]
                if len(matching_mounts) != 1:
                    continue
            owned.add(volume_name)
    return owned


def foreign_inventory_change_summary(baseline: dict[str, Any], current: dict[str, Any]) -> str:
    changes: list[str] = []
    for kind in ("containers", "volumes", "networks", "images"):
        before = baseline.get(kind)
        after = current.get(kind)
        if not isinstance(before, dict) or not isinstance(after, dict):
            return f"{kind}(invalid-shape)"
        added = after.keys() - before.keys()
        removed = before.keys() - after.keys()
        fields: set[str] = set()
        for identity in before.keys() & after.keys():
            old_record = before[identity]
            new_record = after[identity]
            if not isinstance(old_record, dict) or not isinstance(new_record, dict):
                fields.add("record")
                continue
            fields.update(
                field
                for field in old_record.keys() | new_record.keys()
                if old_record.get(field) != new_record.get(field)
            )
        if added or removed or fields:
            changed_fields = ",".join(sorted(fields)) or "none"
            changes.append(f"{kind}(added={len(added)},removed={len(removed)},fields={changed_fields})")
    return ";".join(changes) or "unknown"


def foreign_inventory_failure(manifest: dict[str, Any]) -> DrillFailure | None:
    if manifest.get("foreign_inventory_changed"):
        return DrillFailure("cleanup", "isolation_refusal")
    return None


def bounded_work_seconds(
    fault_test: str | None,
    requested: int | None,
    *,
    local_fixture_test: bool = False,
) -> int:
    """Keep each run inside its normal or fault-test work budget."""
    if requested is not None and requested < 1:
        raise ValueError("work budget must be positive")
    limit = FAULT_WORK_SECONDS if fault_test and not local_fixture_test else WORK_SECONDS
    return limit if requested is None else min(limit, requested)


def prepared_directory_path(value: Path) -> Path:
    path = Path(value)
    if (
        not path.is_absolute()
        or path.parent != Path("/tmp")
        or not re.fullmatch(r"teagram-restore-readiness\.[A-Za-z0-9]{8}", path.name)
    ):
        raise ValueError("invalid prepared runtime path")
    parent = path.parent.lstat()
    if (
        not stat.S_ISDIR(parent.st_mode)
        or parent.st_uid != 0
        or stat.S_IMODE(parent.st_mode) != 0o1777
    ):
        raise ValueError("invalid prepared runtime parent")
    return path


def sha256_open_file(descriptor: int, *, limit: int = 2 * 1024 * 1024 * 1024) -> tuple[str, int]:
    digest = hashlib.sha256()
    size = 0
    os.lseek(descriptor, 0, os.SEEK_SET)
    while True:
        chunk = os.read(descriptor, 1024 * 1024)
        if not chunk:
            break
        size += len(chunk)
        if size > limit:
            raise ValueError("prepared input exceeds size limit")
        digest.update(chunk)
    return digest.hexdigest(), size


def sha256_file(path: Path) -> tuple[str, int]:
    descriptor = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
    try:
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
            raise ValueError("invalid prepared file")
        return sha256_open_file(descriptor)
    finally:
        os.close(descriptor)


def sha256_directory(path: Path, *, max_entries: int = 200_000) -> str:
    root = Path(path)
    root_info = root.lstat()
    if not stat.S_ISDIR(root_info.st_mode) or root.is_symlink():
        raise ValueError("invalid toolchain directory")
    digest = hashlib.sha256()
    entries_seen = 0
    bytes_seen = 0
    for current, directories, files in os.walk(root, topdown=True, followlinks=False):
        directories.sort()
        files.sort()
        current_path = Path(current)
        for name in list(directories):
            item = current_path / name
            info = item.lstat()
            relative = item.relative_to(root).as_posix().encode("utf-8", "strict")
            if stat.S_ISLNK(info.st_mode):
                target = os.readlink(item).encode("utf-8", "strict")
                record = b"L\0" + relative + b"\0" + target + b"\0"
                directories.remove(name)
            elif stat.S_ISDIR(info.st_mode):
                record = b"D\0" + relative + b"\0" + oct(stat.S_IMODE(info.st_mode)).encode() + b"\0"
            else:
                raise ValueError("invalid toolchain entry")
            digest.update(record)
            entries_seen += 1
        for name in files:
            item = current_path / name
            info = item.lstat()
            relative = item.relative_to(root).as_posix().encode("utf-8", "strict")
            if stat.S_ISLNK(info.st_mode):
                record = b"L\0" + relative + b"\0" + os.readlink(item).encode("utf-8", "strict") + b"\0"
            elif stat.S_ISREG(info.st_mode):
                file_hash, size = sha256_file(item)
                bytes_seen += size
                if bytes_seen > 2 * 1024 * 1024 * 1024:
                    raise ValueError("toolchain exceeds size limit")
                record = (
                    b"F\0"
                    + relative
                    + b"\0"
                    + oct(stat.S_IMODE(info.st_mode)).encode()
                    + b"\0"
                    + str(size).encode()
                    + b"\0"
                    + file_hash.encode()
                    + b"\0"
                )
            else:
                raise ValueError("invalid toolchain entry")
            digest.update(record)
            entries_seen += 1
            if entries_seen > max_entries:
                raise ValueError("toolchain has too many entries")
    return digest.hexdigest()


def source_worktree_paths() -> set[bytes]:
    paths: set[bytes] = set()

    def walk_error(error: OSError) -> None:
        raise error

    for directory, child_directories, child_files in os.walk(
        ROOT, topdown=True, followlinks=False, onerror=walk_error
    ):
        parent = Path(directory)
        if parent == ROOT:
            if ".git" in child_directories:
                child_directories.remove(".git")
            if ".git" in child_files:
                child_files.remove(".git")
        for name in tuple(child_directories):
            item = parent / name
            info = item.lstat()
            if stat.S_ISLNK(info.st_mode):
                paths.add(os.fsencode(item.relative_to(ROOT).as_posix()))
                child_directories.remove(name)
            elif not stat.S_ISDIR(info.st_mode):
                raise ValueError("invalid source directory")
        for name in child_files:
            paths.add(os.fsencode((parent / name).relative_to(ROOT).as_posix()))
    return paths


def open_source_parent(raw_path: bytes) -> tuple[int, bytes]:
    parts = raw_path.split(b"/")
    if not raw_path or any(part in (b"", b".", b"..") for part in parts):
        raise ValueError("invalid source path")
    directory_fd = os.open(ROOT, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW)
    try:
        for part in parts[:-1]:
            next_fd = os.open(
                part,
                os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW,
                dir_fd=directory_fd,
            )
            os.close(directory_fd)
            directory_fd = next_fd
        return directory_fd, parts[-1]
    except BaseException:
        os.close(directory_fd)
        raise


def source_identity() -> tuple[str, str]:
    environment = {
        "GIT_CONFIG_GLOBAL": os.devnull,
        "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_NO_LAZY_FETCH": "1",
        "GIT_NO_REPLACE_OBJECTS": "1",
        "GIT_OPTIONAL_LOCKS": "0",
        "LANG": "C",
        "LC_ALL": "C",
        "PATH": "/usr/bin:/bin",
    }
    git = [
        "/usr/bin/git",
        "-c",
        "core.fsmonitor=false",
        "-c",
        f"safe.directory={ROOT}",
        "-C",
        str(ROOT),
    ]

    def git_output(*arguments: str) -> bytes:
        return subprocess.run(
            [*git, *arguments],
            check=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            env=environment,
            timeout=30,
        ).stdout

    revision = git_output("rev-parse", "--verify", "--end-of-options", "HEAD^{commit}").decode(
        "ascii", "strict"
    ).strip()
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("invalid source revision")
    tree = git_output("ls-tree", "-r", "-z", "--full-tree", revision)
    entries: list[tuple[bytes, bytes, bytes]] = []
    for record in tree.split(b"\0"):
        if not record:
            continue
        header, separator, raw_path = record.partition(b"\t")
        fields = header.split(b" ")
        if (
            not separator
            or len(fields) != 3
            or fields[0] not in (b"100644", b"100755", b"120000")
            or fields[1] != b"blob"
            or not re.fullmatch(rb"[0-9a-f]{40}", fields[2])
            or not raw_path
        ):
            raise ValueError("invalid source tree entry")
        entries.append((raw_path, fields[0], fields[2]))
    entries.sort(key=lambda entry: entry[0])
    if len({path for path, _, _ in entries}) != len(entries):
        raise ValueError("duplicate source tree path")
    expected_paths = {path for path, _, _ in entries}
    if source_worktree_paths() != expected_paths:
        raise ValueError("source worktree is dirty")

    digest = hashlib.sha256()
    total_bytes = 0
    for raw_path, mode, object_id in entries:
        parent_fd, leaf = open_source_parent(raw_path)
        file_fd: int | None = None
        try:
            info = os.stat(leaf, dir_fd=parent_fd, follow_symlinks=False)
            is_symlink = mode == b"120000"
            if is_symlink:
                if not stat.S_ISLNK(info.st_mode):
                    raise ValueError("source worktree is dirty")
                link_bytes = os.fsencode(os.readlink(leaf, dir_fd=parent_fd))
                if len(link_bytes) != info.st_size:
                    raise ValueError("source worktree is dirty")
                size = len(link_bytes)
            else:
                if (
                    not stat.S_ISREG(info.st_mode)
                    or bool(info.st_mode & 0o111) != (mode == b"100755")
                    or info.st_nlink != 1
                ):
                    raise ValueError("source worktree is dirty")
                size = info.st_size
                file_fd = os.open(
                    leaf,
                    os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK,
                    dir_fd=parent_fd,
                )
                opened_info = os.fstat(file_fd)
                if (
                    not stat.S_ISREG(opened_info.st_mode)
                    or opened_info.st_nlink != 1
                    or (opened_info.st_dev, opened_info.st_ino) != (info.st_dev, info.st_ino)
                ):
                    raise ValueError("source worktree is dirty")
            if size < 0 or total_bytes + size > 2 * 1024 * 1024 * 1024:
                raise ValueError("source tree exceeds size limit")
            total_bytes += size

            git_hash = hashlib.sha1(usedforsecurity=False)
            git_hash.update(b"blob " + str(size).encode() + b"\0")
            file_hash = hashlib.sha256()
            if is_symlink:
                git_hash.update(link_bytes)
                file_hash.update(link_bytes)
                after_info = os.stat(leaf, dir_fd=parent_fd, follow_symlinks=False)
                if (after_info.st_dev, after_info.st_ino) != (info.st_dev, info.st_ino):
                    raise ValueError("source worktree is dirty")
            else:
                read_size = 0
                while True:
                    chunk = os.read(file_fd, 1024 * 1024)
                    if not chunk:
                        break
                    read_size += len(chunk)
                    if read_size > size:
                        raise ValueError("source worktree is dirty")
                    git_hash.update(chunk)
                    file_hash.update(chunk)
                after_info = os.fstat(file_fd)
                if (
                    read_size != size
                    or (
                        after_info.st_dev,
                        after_info.st_ino,
                        after_info.st_size,
                        after_info.st_mode,
                        after_info.st_mtime_ns,
                        after_info.st_ctime_ns,
                    )
                    != (
                        opened_info.st_dev,
                        opened_info.st_ino,
                        opened_info.st_size,
                        opened_info.st_mode,
                        opened_info.st_mtime_ns,
                        opened_info.st_ctime_ns,
                    )
                ):
                    raise ValueError("source worktree is dirty")
            if git_hash.hexdigest().encode() != object_id:
                raise ValueError("source worktree is dirty")
            digest.update(
                (b"L" if is_symlink else b"F")
                + b"\0"
                + raw_path
                + b"\0"
                + mode
                + b"\0"
                + str(size).encode()
                + b"\0"
                + file_hash.hexdigest().encode()
                + b"\0"
            )
        finally:
            if file_fd is not None:
                os.close(file_fd)
            os.close(parent_fd)
    if git_output("rev-parse", "--verify", "--end-of-options", "HEAD^{commit}").decode(
        "ascii", "strict"
    ).strip() != revision or source_worktree_paths() != expected_paths:
        raise ValueError("source worktree is dirty")
    return revision, digest.hexdigest()


def parse_go_version(value: str) -> tuple[str, str]:
    match = re.fullmatch(r"go version (go[0-9]+\.[0-9]+(?:\.[0-9]+)?) linux/(amd64|arm64)", value)
    if match is None:
        raise ValueError("invalid Go toolchain version")
    return match.group(1), match.group(2)


def minimum_go_version() -> str:
    go_mod = ROOT / "go.mod"
    for line in go_mod.read_text(encoding="utf-8").splitlines():
        match = re.fullmatch(r"go ([0-9]+\.[0-9]+(?:\.[0-9]+)?)", line.strip())
        if match is not None:
            return "go" + match.group(1)
    raise ValueError("go.mod toolchain pin missing")


def finalize_prepared_runtime(
    directory: Path,
    *,
    build_uid: int,
    go_version: str,
    go_root: Path,
    go_binary: Path,
    go_arch: str,
) -> dict[str, Any]:
    if os.geteuid() != 0 or build_uid <= 0:
        raise ValueError("invalid prepared runtime authority")
    path = prepared_directory_path(directory)
    info = path.lstat()
    if path.is_symlink() or not stat.S_ISDIR(info.st_mode) or info.st_uid != build_uid or stat.S_IMODE(info.st_mode) != 0o700:
        raise ValueError("invalid prepared runtime directory")
    expected_entries = PREPARED_ARTIFACTS
    if {entry.name for entry in path.iterdir()} != expected_entries:
        raise ValueError("prepared runtime contents mismatch")
    toolchain_version, version_arch = parse_go_version(go_version)
    if version_arch != go_arch or go_arch not in {"amd64", "arm64"}:
        raise ValueError("prepared runtime architecture mismatch")
    if toolchain_version != minimum_go_version():
        raise ValueError("Go toolchain does not match go.mod")
    resolved_root = go_root.resolve(strict=True)
    resolved_binary = go_binary.resolve(strict=True)
    if not resolved_binary.is_relative_to(resolved_root):
        raise ValueError("Go binary is outside GOROOT")
    root_info = resolved_root.lstat()
    binary_info = resolved_binary.lstat()
    if not stat.S_ISDIR(root_info.st_mode) or not stat.S_ISREG(binary_info.st_mode) or not binary_info.st_mode & 0o111:
        raise ValueError("invalid Go toolchain paths")
    artifacts: dict[str, dict[str, Any]] = {}
    open_files: list[int] = []
    directory_fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW)
    try:
        directory_info = os.fstat(directory_fd)
        if directory_info.st_dev != info.st_dev or directory_info.st_ino != info.st_ino:
            raise ValueError("prepared runtime directory changed")
        for name in sorted(PREPARED_ARTIFACTS):
            descriptor = os.open(name, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW, dir_fd=directory_fd)
            open_files.append(descriptor)
            artifact_info = os.fstat(descriptor)
            if (
                not stat.S_ISREG(artifact_info.st_mode)
                or artifact_info.st_uid != build_uid
                or artifact_info.st_nlink != 1
                or artifact_info.st_mode & 0o022
                or not artifact_info.st_mode & 0o111
                or artifact_info.st_size <= 0
            ):
                raise ValueError("invalid prepared runtime artifact")
            file_hash, size = sha256_open_file(descriptor)
            artifacts[name] = {"sha256": file_hash, "size": size}
        revision, source_digest = source_identity()
        attestation = {
            "schema_version": 1,
            "prepared_uid": build_uid,
            "directory": {"device": directory_info.st_dev, "inode": directory_info.st_ino},
            "source": {"revision": revision, "tree_sha256": source_digest},
            "toolchain": {
                "version": toolchain_version,
                "goos": "linux",
                "goarch": go_arch,
                "root": str(resolved_root),
                "root_sha256": sha256_directory(resolved_root),
                "binary": str(resolved_binary),
                "binary_sha256": sha256_file(resolved_binary)[0],
            },
            "images": {"source": SOURCE_IMAGE, "restore": RESTORE_IMAGE},
            "seed_commit": SEED_COMMIT,
            "build_flags": PREPARED_BUILD_FLAGS,
            "build_environment": {
                "CGO_ENABLED": "0",
                "GOENV": "off",
                "GOMAXPROCS": "1",
                "GOPROXY": "off",
                "GOSUMDB": "off",
                "GOTOOLCHAIN": "local",
            },
            "artifacts": artifacts,
        }
        encoded = (json.dumps(attestation, separators=(",", ":"), sort_keys=True) + "\n").encode("utf-8")
        descriptor = os.open(
            PREPARED_ATTESTATION,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW,
            0o400,
            dir_fd=directory_fd,
        )
        try:
            os.fchown(descriptor, 0, 0)
            os.fchmod(descriptor, 0o400)
            view = memoryview(encoded)
            while view:
                written = os.write(descriptor, view)
                if written <= 0:
                    raise OSError("short prepared runtime attestation write")
                view = view[written:]
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
        for file_descriptor in open_files:
            os.fchown(file_descriptor, 0, 0)
            os.fchmod(file_descriptor, 0o500)
        os.fchown(directory_fd, 0, 0)
        os.fchmod(directory_fd, 0o500)
        os.fsync(directory_fd)
        final_info = path.lstat()
        if final_info.st_dev != directory_info.st_dev or final_info.st_ino != directory_info.st_ino or final_info.st_uid != 0:
            raise ValueError("prepared runtime directory changed")
        return attestation
    finally:
        for descriptor in open_files:
            os.close(descriptor)
        os.close(directory_fd)


def verify_prepared_runtime(directory: Path, *, expected_owner: int = 0) -> dict[str, Any]:
    path = prepared_directory_path(directory)
    directory_info = path.lstat()
    if path.is_symlink() or not stat.S_ISDIR(directory_info.st_mode) or directory_info.st_uid != expected_owner or stat.S_IMODE(directory_info.st_mode) != 0o500:
        raise ValueError("prepared runtime is missing or has unsafe ownership")
    descriptor = os.open(path / PREPARED_ATTESTATION, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
    try:
        info = os.fstat(descriptor)
        if (
            not stat.S_ISREG(info.st_mode)
            or info.st_uid != expected_owner
            or stat.S_IMODE(info.st_mode) != 0o400
            or info.st_nlink != 1
            or info.st_size > 1024 * 1024
        ):
            raise ValueError("invalid prepared runtime attestation")
        data = os.read(descriptor, 1024 * 1024 + 1)
    finally:
        os.close(descriptor)
    if len(data) > 1024 * 1024:
        raise ValueError("prepared runtime attestation is too large")
    attestation = json.loads(data, object_pairs_hook=unique_json_object)
    expected_top = {
        "schema_version",
        "prepared_uid",
        "directory",
        "source",
        "toolchain",
        "images",
        "seed_commit",
        "build_flags",
        "build_environment",
        "artifacts",
    }
    if not isinstance(attestation, dict) or set(attestation) != expected_top or attestation.get("schema_version") != 1:
        raise ValueError("invalid prepared runtime attestation fields")
    if (
        attestation["directory"] != {"device": directory_info.st_dev, "inode": directory_info.st_ino}
        or not isinstance(attestation["prepared_uid"], int)
        or attestation["prepared_uid"] <= 0
        or attestation["images"] != {"source": SOURCE_IMAGE, "restore": RESTORE_IMAGE}
        or attestation["seed_commit"] != SEED_COMMIT
        or attestation["build_flags"] != PREPARED_BUILD_FLAGS
        or attestation["build_environment"]
        != {
            "CGO_ENABLED": "0",
            "GOENV": "off",
            "GOMAXPROCS": "1",
            "GOPROXY": "off",
            "GOSUMDB": "off",
            "GOTOOLCHAIN": "local",
        }
    ):
        raise ValueError("prepared runtime pins mismatch")
    source_revision, source_digest = source_identity()
    source = attestation["source"]
    if source != {"revision": source_revision, "tree_sha256": source_digest}:
        raise ValueError("prepared runtime source mismatch")
    toolchain = attestation["toolchain"]
    if not isinstance(toolchain, dict) or set(toolchain) != {
        "version", "goos", "goarch", "root", "root_sha256", "binary", "binary_sha256"
    }:
        raise ValueError("invalid prepared runtime toolchain")
    version = "go version " + toolchain.get("version", "") + " linux/" + toolchain.get("goarch", "")
    version_value, go_arch = parse_go_version(version)
    root = Path(toolchain["root"])
    binary = Path(toolchain["binary"])
    if (
        toolchain.get("goos") != "linux"
        or go_arch not in {"amd64", "arm64"}
        or version_value != minimum_go_version()
        or not root.is_absolute()
        or not binary.is_absolute()
        or binary.resolve(strict=True) != binary
        or not binary.is_relative_to(root)
        or sha256_directory(root) != toolchain["root_sha256"]
        or sha256_file(binary)[0] != toolchain["binary_sha256"]
    ):
        raise ValueError("prepared runtime toolchain mismatch")
    artifacts = attestation["artifacts"]
    if not isinstance(artifacts, dict) or set(artifacts) != PREPARED_ARTIFACTS:
        raise ValueError("prepared runtime artifact list mismatch")
    if {item.name for item in path.iterdir()} != PREPARED_ARTIFACTS | {PREPARED_ATTESTATION}:
        raise ValueError("prepared runtime contents mismatch")
    for name in sorted(PREPARED_ARTIFACTS):
        item = path / name
        item_info = item.lstat()
        expected = artifacts[name]
        if (
            not isinstance(expected, dict)
            or set(expected) != {"sha256", "size"}
            or not stat.S_ISREG(item_info.st_mode)
            or item_info.st_uid != expected_owner
            or stat.S_IMODE(item_info.st_mode) != 0o500
            or item_info.st_nlink != 1
            or not item_info.st_mode & 0o111
            or sha256_file(item) != (expected["sha256"], expected["size"])
        ):
            raise ValueError("prepared runtime artifact mismatch")
    manifest_hash = hashlib.sha256(data).hexdigest()
    return {
        "path": str(path),
        "device": directory_info.st_dev,
        "inode": directory_info.st_ino,
        "attestation_sha256": manifest_hash,
        "prepared_uid": attestation["prepared_uid"],
        "go_arch": go_arch,
        "atlas_bin": str(path / "atlas"),
        "build_dir": str(path),
    }


def resolve_local_dump_migration_head(version: str, migration_dir: Path | None = None) -> str:
    if not re.fullmatch(r"[0-9]{14}", version):
        raise ValueError("invalid local dump migration head")
    if version < BASE_SCHEMA or version not in repo_migration_versions(migration_dir):
        raise ValueError("unsupported local dump migration head")
    return version


MIGRATION_HEAD_SQL = "SELECT COALESCE(pg_catalog.max(version), '') FROM atlas_schema_revisions.atlas_schema_revisions;"
MIGRATION_HEAD_RELATION_SAFE_SQL = r"""
SELECT CASE WHEN relation.relkind = 'r'
  AND NOT relation.relrowsecurity
  AND NOT relation.relforcerowsecurity
  AND pg_catalog.pg_get_userbyid(relation.relowner) = 'postgres'
  AND EXISTS (
    SELECT 1 FROM pg_catalog.pg_attribute AS attribute
    JOIN pg_catalog.pg_type AS data_type ON data_type.oid = attribute.atttypid
    JOIN pg_catalog.pg_namespace AS type_namespace ON type_namespace.oid = data_type.typnamespace
    WHERE attribute.attrelid = relation.oid
      AND attribute.attname = 'version'
      AND type_namespace.nspname = 'pg_catalog'
      AND data_type.typname IN ('text', 'varchar')
      AND attribute.attnum > 0
      AND NOT attribute.attisdropped
      AND attribute.attgenerated = ''
      AND attribute.attidentity = ''
  )
  AND NOT EXISTS (
    SELECT 1 FROM pg_catalog.pg_policy AS policy
    WHERE policy.polrelid = relation.oid
  )
  AND NOT EXISTS (
    SELECT 1 FROM pg_catalog.pg_rewrite AS rewrite
    WHERE rewrite.ev_class = relation.oid
  )
  AND NOT EXISTS (
    SELECT 1 FROM pg_catalog.pg_trigger AS trigger_row
    WHERE trigger_row.tgrelid = relation.oid AND NOT trigger_row.tgisinternal
  )
  THEN 'safe' ELSE 'unsafe' END
FROM pg_catalog.pg_class AS relation
JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
WHERE namespace.nspname = 'atlas_schema_revisions'
  AND relation.relname = 'atlas_schema_revisions';
"""
MIGRATION_HEAD_RELATION_SHAPE_SQL = r"""
SELECT jsonb_build_object(
  'kind', relation.relkind,
  'owner', pg_catalog.pg_get_userbyid(relation.relowner),
  'persistence', relation.relpersistence,
  'columns', COALESCE((
    SELECT jsonb_agg(jsonb_build_object(
      'name', attribute.attname,
      'type', pg_catalog.format_type(attribute.atttypid, attribute.atttypmod),
      'not_null', attribute.attnotnull,
      'identity', attribute.attidentity,
      'generated', attribute.attgenerated,
      'default', COALESCE(pg_catalog.pg_get_expr(default_value.adbin, default_value.adrelid), '')
    ) ORDER BY attribute.attnum)
    FROM pg_catalog.pg_attribute AS attribute
    LEFT JOIN pg_catalog.pg_attrdef AS default_value
      ON default_value.adrelid = attribute.attrelid AND default_value.adnum = attribute.attnum
    WHERE attribute.attrelid = relation.oid
      AND attribute.attnum > 0
      AND NOT attribute.attisdropped
  ), '[]'::jsonb),
  'constraints', COALESCE((
    SELECT jsonb_agg(pg_catalog.pg_get_constraintdef(constraint_row.oid, true) ORDER BY constraint_row.conname)
    FROM pg_catalog.pg_constraint AS constraint_row
    WHERE constraint_row.conrelid = relation.oid AND constraint_row.contype <> 'n'
  ), '[]'::jsonb),
  'rules', COALESCE((
    SELECT jsonb_agg(rewrite.rulename ORDER BY rewrite.rulename)
    FROM pg_catalog.pg_rewrite AS rewrite WHERE rewrite.ev_class = relation.oid
  ), '[]'::jsonb),
  'policies', COALESCE((
    SELECT jsonb_agg(policy.polname ORDER BY policy.polname)
    FROM pg_catalog.pg_policy AS policy WHERE policy.polrelid = relation.oid
  ), '[]'::jsonb),
  'triggers', COALESCE((
    SELECT jsonb_agg(pg_catalog.pg_get_triggerdef(trigger_row.oid, true) ORDER BY trigger_row.tgname)
    FROM pg_catalog.pg_trigger AS trigger_row
    WHERE trigger_row.tgrelid = relation.oid AND NOT trigger_row.tgisinternal
  ), '[]'::jsonb)
)::text
FROM pg_catalog.pg_class AS relation
JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
WHERE namespace.nspname = 'atlas_schema_revisions'
  AND relation.relname = 'atlas_schema_revisions';
"""
DATABASE_IDENTITY_SQL = r"""
SELECT current_database() || '|' || current_setting('server_version_num') || '|' ||
       (SELECT oid::text FROM pg_database WHERE datname = current_database()) || '|' ||
       (SELECT system_identifier::text FROM pg_control_system());
"""
AUTH_KEY_INVENTORY_SQL = r"""
WITH auth_keys AS (
  SELECT 'public.auth_keys'::regclass AS relation
), clearing_relations AS (
  SELECT relation FROM auth_keys
  UNION SELECT 'public.srp_challenges'::regclass
  UNION SELECT 'public.notify_settings_difference_cursors'::regclass
  UNION SELECT 'public.browser_acceptance_binding_evidence'::regclass
), foreign_keys AS (
  SELECT con.conname AS name,
         jsonb_build_object(
           'name', con.conname,
           'table', format('%I.%I', child_ns.nspname, child.relname),
           'columns', (
             SELECT jsonb_agg(attribute.attname ORDER BY key_column.ordinality)
             FROM unnest(con.conkey) WITH ORDINALITY AS key_column(attnum, ordinality)
             JOIN pg_attribute AS attribute
               ON attribute.attrelid = child.oid AND attribute.attnum = key_column.attnum
           ),
           'references', format('%I.%I', parent_ns.nspname, parent.relname),
           'referenced_columns', (
             SELECT jsonb_agg(attribute.attname ORDER BY key_column.ordinality)
             FROM unnest(con.confkey) WITH ORDINALITY AS key_column(attnum, ordinality)
             JOIN pg_attribute AS attribute
               ON attribute.attrelid = parent.oid AND attribute.attnum = key_column.attnum
           ),
           'on_delete', CASE con.confdeltype
             WHEN 'a' THEN 'NO ACTION' WHEN 'r' THEN 'RESTRICT' WHEN 'c' THEN 'CASCADE'
             WHEN 'n' THEN 'SET NULL' WHEN 'd' THEN 'SET DEFAULT' END,
           'on_update', CASE con.confupdtype
             WHEN 'a' THEN 'NO ACTION' WHEN 'r' THEN 'RESTRICT' WHEN 'c' THEN 'CASCADE'
             WHEN 'n' THEN 'SET NULL' WHEN 'd' THEN 'SET DEFAULT' END,
           'match', CASE con.confmatchtype
             WHEN 's' THEN 'SIMPLE' WHEN 'f' THEN 'FULL' WHEN 'p' THEN 'PARTIAL' END,
           'deferrable', con.condeferrable,
           'initially_deferred', con.condeferred,
           'validated', con.convalidated
         ) AS entry
  FROM pg_constraint AS con
  JOIN pg_class AS child ON child.oid = con.conrelid
  JOIN pg_namespace AS child_ns ON child_ns.oid = child.relnamespace
  JOIN pg_class AS parent ON parent.oid = con.confrelid
  JOIN pg_namespace AS parent_ns ON parent_ns.oid = parent.relnamespace
  WHERE con.contype = 'f'
    AND (child.oid = (SELECT relation FROM auth_keys)
      OR parent.oid = (SELECT relation FROM auth_keys))
), triggers AS (
  SELECT tg.tgname AS name,
         jsonb_build_object(
           'name', tg.tgname,
           'table', format('%I.%I', table_ns.nspname, relation.relname),
           'timing', CASE WHEN (tg.tgtype & 2) <> 0 THEN 'BEFORE'
                          WHEN (tg.tgtype & 64) <> 0 THEN 'INSTEAD OF'
                          ELSE 'AFTER' END,
           'level', CASE WHEN (tg.tgtype & 1) <> 0 THEN 'ROW' ELSE 'STATEMENT' END,
           'events', array_remove(ARRAY[
             CASE WHEN (tg.tgtype & 4) <> 0 THEN 'INSERT' END,
             CASE WHEN (tg.tgtype & 8) <> 0 THEN 'DELETE' END,
             CASE WHEN (tg.tgtype & 16) <> 0 THEN 'UPDATE' END,
             CASE WHEN (tg.tgtype & 32) <> 0 THEN 'TRUNCATE' END
           ]::text[], NULL),
           'enabled', CASE tg.tgenabled WHEN 'A' THEN 'ALWAYS'
                                        WHEN 'R' THEN 'REPLICA'
                                        WHEN 'O' THEN 'ORIGIN' ELSE 'DISABLED' END,
           'function', function_ns.nspname || '.' || proc.proname
         ) AS entry
  FROM pg_trigger AS tg
  JOIN pg_class AS relation ON relation.oid = tg.tgrelid
  JOIN pg_namespace AS table_ns ON table_ns.oid = relation.relnamespace
  JOIN pg_proc AS proc ON proc.oid = tg.tgfoid
  JOIN pg_namespace AS function_ns ON function_ns.oid = proc.pronamespace
  WHERE tg.tgrelid IN (SELECT relation FROM clearing_relations)
    AND NOT tg.tgisinternal
)
SELECT jsonb_build_object(
  'trigger_tables', COALESCE((
    SELECT jsonb_agg(
      format('%I.%I', table_ns.nspname, relation.relname)
      ORDER BY table_ns.nspname, relation.relname
    )
    FROM clearing_relations
    JOIN pg_class AS relation ON relation.oid = clearing_relations.relation
    JOIN pg_namespace AS table_ns ON table_ns.oid = relation.relnamespace
  ), '[]'::jsonb),
  'foreign_keys', COALESCE((SELECT jsonb_agg(entry ORDER BY name) FROM foreign_keys), '[]'::jsonb),
  'triggers', COALESCE((SELECT jsonb_agg(entry ORDER BY name) FROM triggers), '[]'::jsonb)
)::text;
"""
AUTH_KEY_STATE_SQL = r"""
SELECT
  (SELECT count(*) FROM public.auth_keys)::text || '|' ||
  (SELECT count(*) FROM public.srp_challenges)::text || '|' ||
  (SELECT count(*) FROM public.notify_settings_difference_cursors)::text || '|' ||
  (SELECT count(*) FROM public.browser_acceptance_binding_evidence)::text || '|' ||
  (SELECT COALESCE(sum(additions), 0) FROM public.browser_acceptance_binding_evidence)::text || '|' ||
  (SELECT COALESCE(sum(removals), 0) FROM public.browser_acceptance_binding_evidence)::text;
"""
AUTH_KEY_COUNT_SQL = "SELECT count(*)::text FROM public.auth_keys;"
AUTH_KEY_EVIDENCE_BOUND_COUNT_SQL = """
SELECT count(*)::text
FROM public.auth_keys AS auth_key
JOIN public.browser_acceptance_binding_evidence AS evidence
  ON evidence.user_id = auth_key.user_id;
"""
UNENROLLED_BOUND_AUTH_KEY_COUNT_SQL = """
SELECT count(*)::text
FROM public.auth_keys AS auth_key
LEFT JOIN public.browser_acceptance_binding_evidence AS evidence
  ON evidence.user_id = auth_key.user_id
WHERE auth_key.user_id IS NOT NULL AND evidence.user_id IS NULL;
"""


def local_dump_identity(info: os.stat_result) -> dict[str, int]:
    return {
        "device": info.st_dev,
        "inode": info.st_ino,
        "mode": stat.S_IMODE(info.st_mode),
        "uid": info.st_uid,
        "gid": info.st_gid,
        "links": info.st_nlink,
        "size": info.st_size,
        "mtime_ns": info.st_mtime_ns,
        "ctime_ns": info.st_ctime_ns,
    }


def bootstrap_postgres_role_sql(password: str) -> str:
    if not re.fullmatch(r"[0-9a-f]{64}", password):
        raise ValueError("invalid scratch database password")
    return (
        "CREATE ROLE postgres WITH LOGIN SUPERUSER CREATEDB CREATEROLE "
        f"REPLICATION BYPASSRLS PASSWORD '{password}';\n"
        "ALTER DATABASE teagram OWNER TO postgres;"
    )


def validate_local_dump_metadata(info: os.stat_result, name: str) -> bool:
    return (
        bool(LOCAL_DUMP_NAME.fullmatch(name))
        and stat.S_ISREG(info.st_mode)
        and info.st_uid == 0
        and info.st_gid == 0
        and stat.S_IMODE(info.st_mode) == 0o600
        and info.st_nlink == 1
        and 0 < info.st_size <= MAX_LOCAL_DUMP_BYTES
    )


def validate_local_dump_parent(info: os.stat_result) -> bool:
    return (
        stat.S_ISDIR(info.st_mode)
        and info.st_uid == 0
        and info.st_gid == 0
        and (info.st_mode & 0o022) == 0
    )


def open_local_dump(path: Path) -> tuple[int, dict[str, int]]:
    """Open a protected dump without following path components or reading its bytes."""
    if not path.is_absolute() or str(path) != os.path.normpath(str(path)) or not LOCAL_DUMP_NAME.fullmatch(path.name):
        raise ValueError("invalid local dump path")

    directory_fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
    descriptor = -1
    try:
        if not validate_local_dump_parent(os.fstat(directory_fd)):
            raise ValueError("invalid local dump parent")

        for component in path.parts[1:-1]:
            if component in {"", ".", ".."}:
                raise ValueError("invalid local dump path")
            next_fd = os.open(
                component,
                os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW,
                dir_fd=directory_fd,
            )
            parent_info = os.fstat(next_fd)
            if not validate_local_dump_parent(parent_info):
                os.close(next_fd)
                raise ValueError("invalid local dump parent")
            os.close(directory_fd)
            directory_fd = next_fd

        descriptor = os.open(
            path.name,
            os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK,
            dir_fd=directory_fd,
        )
        info = os.fstat(descriptor)
        path_info = os.stat(path.name, dir_fd=directory_fd, follow_symlinks=False)
        if not validate_local_dump_metadata(info, path.name) or local_dump_identity(info) != local_dump_identity(path_info):
            raise ValueError("invalid local dump metadata")
        return descriptor, local_dump_identity(info)
    except BaseException:
        if descriptor >= 0:
            os.close(descriptor)
        raise
    finally:
        os.close(directory_fd)


def malformed_sql_fixture(token: str) -> str:
    if not re.fullmatch(r"[0-9a-f]{32}", token):
        raise ValueError("invalid fixture token")
    return f'SELEC "restore_readiness_{token}";'


def validate_synthetic_auth_key_ids(auth_key_ids: list[int] | tuple[int, ...]) -> tuple[int, ...]:
    if (
        len(auth_key_ids) != AUTH_KEY_FIXTURE_COUNT
        or any(
            type(identifier) is not int
            or identifier >= 0
            or identifier <= -(1 << 63)
            for identifier in auth_key_ids
        )
        or len(set(auth_key_ids)) != AUTH_KEY_FIXTURE_COUNT
    ):
        raise ValueError("invalid synthetic auth-key identifiers")
    return tuple(auth_key_ids)


def load_auth_key_inventory() -> dict[str, Any]:
    try:
        inventory = json.loads(AUTH_KEY_INVENTORY_PATH.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise ValueError("auth-key inventory is unavailable") from exc
    if (
        not isinstance(inventory, dict)
        or inventory.get("schema_head") not in repo_migration_versions()
        or inventory.get("schema_head") < BASE_SCHEMA
        or not isinstance(inventory.get("foreign_keys"), list)
        or not isinstance(inventory.get("triggers"), list)
        or not isinstance(inventory.get("trigger_tables"), list)
    ):
        raise ValueError("auth-key inventory is invalid")
    return inventory


def synthetic_auth_key_fixture_sql(auth_key_ids: list[int] | tuple[int, ...]) -> str:
    first, second, pending = validate_synthetic_auth_key_ids(auth_key_ids)
    srp_first = abs(first)
    srp_second = abs(second)
    values = ",\n    ".join(
        (
            f"({first}, decode(repeat('a5', 256), 'hex'), fixture_user_id, NULL)",
            f"({second}, decode(repeat('5a', 256), 'hex'), fixture_user_id, NULL)",
            f"({pending}, decode(repeat('3c', 256), 'hex'), NULL, fixture_user_id)",
        )
    )
    return f"""
DO $auth_key_fixture$
DECLARE
  fixture_user_id BIGINT;
BEGIN
  SELECT id INTO STRICT fixture_user_id FROM public.users;
  IF (SELECT count(*) FROM public.auth_keys) <> 0 THEN
    RAISE EXCEPTION 'unexpected auth-key fixture state';
  END IF;
  PERFORM public.browser_acceptance_enroll_binding_evidence(fixture_user_id);
  INSERT INTO public.auth_keys (id, key_value, user_id, pending_user_id)
  VALUES
    {values};
  INSERT INTO public.srp_challenges (srp_id, auth_key_id, user_id, b_secret, b_public, expires_at)
  VALUES
    ({srp_first}, {first}, fixture_user_id, decode(repeat('11', 32), 'hex'), decode(repeat('22', 32), 'hex'), now() + interval '1 hour'),
    ({srp_second}, {second}, fixture_user_id, decode(repeat('33', 32), 'hex'), decode(repeat('44', 32), 'hex'), now() + interval '1 hour');
  INSERT INTO public.notify_settings_difference_cursors (
    auth_key_id, user_id, cutoff, resume_date, secret_chat_resume_at,
    last_changed_at, last_scope, last_peer_type, last_peer_id,
    state_pts, state_qts, state_date, state_seq
  )
  SELECT auth_key.id, fixture_user_id, now(), 0, now(), now(), 0, 0, 0, 0, 0, 0, 0
  FROM public.auth_keys AS auth_key
  WHERE auth_key.id IN ({first}, {second}, {pending});
END
$auth_key_fixture$;
"""


def local_unenrolled_auth_key_fixture_sql(auth_key_ids: list[int] | tuple[int, ...], token: str) -> str:
    if (
        len(auth_key_ids) != LOCAL_UNENROLLED_AUTH_KEY_FIXTURE_COUNT
        or any(
            isinstance(identifier, bool)
            or not isinstance(identifier, int)
            or identifier >= 0
            or identifier <= -(1 << 63)
            for identifier in auth_key_ids
        )
        or len(set(auth_key_ids)) != LOCAL_UNENROLLED_AUTH_KEY_FIXTURE_COUNT
        or not re.fullmatch(r"[0-9a-f]{32}", token)
    ):
        raise ValueError("invalid local auth-key fixture")
    first, second = auth_key_ids
    return f"""
DO $local_unenrolled_auth_key_fixture$
DECLARE
  first_unenrolled_user_id BIGINT;
  second_unenrolled_user_id BIGINT;
BEGIN
  IF (SELECT count(*) FROM public.auth_keys) <> {AUTH_KEY_FIXTURE_COUNT}
     OR (SELECT count(*) FROM public.browser_acceptance_binding_evidence) <> 1 THEN
    RAISE EXCEPTION 'unexpected local auth-key fixture state';
  END IF;
  INSERT INTO public.users (phone, first_name)
  VALUES ('restore-readiness-{token}-unenrolled-a', 'Unenrolled fixture user A')
  RETURNING id INTO STRICT first_unenrolled_user_id;
  INSERT INTO public.users (phone, first_name)
  VALUES ('restore-readiness-{token}-unenrolled-b', 'Unenrolled fixture user B')
  RETURNING id INTO STRICT second_unenrolled_user_id;
  INSERT INTO public.auth_keys (id, key_value, user_id, ever_bound_at, pending_user_id)
  VALUES
    ({first}, decode(repeat('c3', 256), 'hex'), first_unenrolled_user_id, clock_timestamp(), NULL),
    ({second}, decode(repeat('d4', 256), 'hex'), second_unenrolled_user_id, clock_timestamp(), NULL);
END
$local_unenrolled_auth_key_fixture$;
"""


def source_fixture_counts(local_fixture_test: bool) -> tuple[int, int]:
    extra_count = LOCAL_UNENROLLED_AUTH_KEY_FIXTURE_COUNT if local_fixture_test else 0
    return 1 + extra_count, AUTH_KEY_FIXTURE_COUNT + extra_count


def auth_key_clear_transaction_sql(
    auth_key_ids: list[int] | tuple[int, ...], *, inject_failure: bool = False, wait_for_signal: bool = False
) -> str:
    identifiers = validate_synthetic_auth_key_ids(auth_key_ids)
    expected_fixture_state = ", ".join(AUTH_KEY_FIXTURE_STATE)
    statements = [
        "BEGIN;",
        "LOCK TABLE public.auth_keys, public.srp_challenges, "
        "public.notify_settings_difference_cursors, public.browser_acceptance_binding_evidence "
        "IN ACCESS EXCLUSIVE MODE;",
        f"""DO $auth_key_preclear$
DECLARE
  auth_key_count BIGINT;
  srp_challenge_count BIGINT;
  cursor_count BIGINT;
  evidence_count BIGINT;
  addition_count BIGINT;
  removal_count BIGINT;
BEGIN
  SELECT count(*) INTO auth_key_count FROM public.auth_keys;
  SELECT count(*) INTO srp_challenge_count FROM public.srp_challenges;
  SELECT count(*) INTO cursor_count FROM public.notify_settings_difference_cursors;
  SELECT count(*) INTO evidence_count FROM public.browser_acceptance_binding_evidence;
  SELECT COALESCE(sum(additions), 0) INTO addition_count FROM public.browser_acceptance_binding_evidence;
  SELECT COALESCE(sum(removals), 0) INTO removal_count FROM public.browser_acceptance_binding_evidence;
  IF (auth_key_count, srp_challenge_count, cursor_count, evidence_count, addition_count, removal_count)
     IS DISTINCT FROM ({expected_fixture_state}) THEN
    RAISE EXCEPTION 'auth-key clearing precondition mismatch';
  END IF;
END
$auth_key_preclear$;""",
        f"DELETE FROM public.auth_keys WHERE id IN ({', '.join(str(identifier) for identifier in identifiers)});",
        """DO $auth_key_clear$
DECLARE
  remaining_auth_keys BIGINT;
BEGIN
  SELECT count(*) INTO remaining_auth_keys FROM public.auth_keys;
  IF remaining_auth_keys <> 0 THEN
    RAISE EXCEPTION 'auth-key clearing count mismatch';
  END IF;
END
$auth_key_clear$;""",
    ]
    if inject_failure or wait_for_signal:
        statements.append(r"\echo RESTORE_READINESS_AUTH_KEY_DELETE_APPLIED")
    if inject_failure:
        statements.append("SELECT 1 / 0;")
    if wait_for_signal:
        statements.append("SELECT pg_sleep(30);")
    statements.append("COMMIT;")
    return "\n".join(statements) + "\n"


def local_auth_key_clear_transaction_sql(*, wait_for_signal: bool = False) -> str:
    wait = (
        f"\\echo {AUTH_KEY_CLEAR_ACK.decode('ascii')}\nSELECT pg_sleep(30);\n"
        if wait_for_signal
        else ""
    )
    return f"""
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '120s';
LOCK TABLE public.auth_keys, public.srp_challenges,
  public.notify_settings_difference_cursors, public.browser_acceptance_binding_evidence
  IN ACCESS EXCLUSIVE MODE;
DO $local_auth_key_clear$
DECLARE
  original_keys BIGINT;
  evidence_bound_keys BIGINT;
  original_evidence BIGINT;
  original_additions BIGINT;
  original_removals BIGINT;
  deleted_keys BIGINT;
BEGIN
  SELECT count(*) INTO original_keys FROM public.auth_keys;
  SELECT count(*) INTO evidence_bound_keys
    FROM public.auth_keys AS auth_key
    JOIN public.browser_acceptance_binding_evidence AS evidence
      ON evidence.user_id = auth_key.user_id;
  IF original_keys > {MAX_LOCAL_AUTH_KEYS} THEN
    RAISE EXCEPTION 'auth-key clearing bound exceeded';
  END IF;
  SELECT count(*), COALESCE(sum(additions), 0), COALESCE(sum(removals), 0)
    INTO original_evidence, original_additions, original_removals
    FROM public.browser_acceptance_binding_evidence;
  DELETE FROM public.auth_keys
    WHERE id IN (
      SELECT auth_key.id FROM public.auth_keys AS auth_key
      ORDER BY auth_key.id LIMIT {MAX_LOCAL_AUTH_KEYS}
    );
  GET DIAGNOSTICS deleted_keys = ROW_COUNT;
  IF deleted_keys <> original_keys
     OR (SELECT count(*) FROM public.auth_keys) <> 0
     OR (SELECT count(*) FROM public.srp_challenges) <> 0
     OR (SELECT count(*) FROM public.notify_settings_difference_cursors) <> 0
     OR (SELECT count(*) FROM public.browser_acceptance_binding_evidence) <> original_evidence
     OR (SELECT COALESCE(sum(additions), 0) FROM public.browser_acceptance_binding_evidence) <> original_additions
     OR (SELECT COALESCE(sum(removals), 0) FROM public.browser_acceptance_binding_evidence) <> original_removals + evidence_bound_keys THEN
    RAISE EXCEPTION 'auth-key clearing aggregate mismatch';
  END IF;
END
$local_auth_key_clear$;
{wait}COMMIT;
""".lstrip()


def auth_key_clear_failure_diagnostics(
    before: list[str],
    after: list[str],
    *,
    client_status: int | None = None,
    container_status: int | None = None,
    stdout: bytes = b"",
    stderr: bytes = b"",
) -> dict[str, bool | int | None]:
    expected_error = re.search(
        rb"(?:^|\n)ERROR:[ \t]+division by zero(?:\r?\n|$)", stdout + stderr
    )
    return {
        "acknowledged": AUTH_KEY_CLEAR_ACK in stdout,
        "client_status": client_status,
        "container_status": container_status,
        "rollback_preserved": before == after,
        "division_by_zero_reported": expected_error is not None,
    }


def auth_key_clear_fault_category(
    fault_kind: str,
    before: list[str],
    after: list[str],
    *,
    client_status: int | None = None,
    container_status: int | None = None,
    stdout: bytes = b"",
    stderr: bytes = b"",
    interrupt_acknowledged: bool = False,
) -> str:
    if before != after:
        return "fault_test_failure"
    if fault_kind == "signal":
        return "interrupted" if interrupt_acknowledged else "fault_test_failure"
    if fault_kind != "failure":
        return "fault_test_failure"
    diagnostics = auth_key_clear_failure_diagnostics(
        before,
        after,
        client_status=client_status,
        container_status=container_status,
        stdout=stdout,
        stderr=stderr,
    )
    if (
        diagnostics["acknowledged"]
        and diagnostics["client_status"] == 3
        and diagnostics["container_status"] == 3
        and diagnostics["division_by_zero_reported"]
    ):
        return "auth_key_clear_failure"
    return "fault_test_failure"


FIXTURE_STATE_SQL = """
SELECT
  (SELECT count(*) FROM public.auth_keys)::text || '|' ||
  (SELECT count(*) FROM public.users)::text || '|' ||
  (SELECT COALESCE(string_agg(rolname, ',' ORDER BY rolname), '')
   FROM pg_roles WHERE rolname NOT IN ('postgres', 'restore_bootstrap') AND left(rolname, 3) <> 'pg_') || '|' ||
  (SELECT COALESCE(string_agg(extname, ',' ORDER BY extname), '') FROM pg_extension) || '|' ||
  (SELECT count(*) FROM pg_event_trigger)::text || '|' ||
  (SELECT count(*) FROM pg_foreign_server)::text || '|' ||
  (SELECT count(*) FROM pg_publication)::text || '|' ||
  (SELECT count(*) FROM pg_subscription)::text || '|' ||
  (SELECT count(*) FROM pg_db_role_setting
   WHERE setdatabase = (SELECT oid FROM pg_database WHERE datname = current_database()))::text;
"""
ROLE_STATE_SQL = r"""
SELECT jsonb_build_object(
  'roles', COALESCE((
    SELECT jsonb_agg(jsonb_build_object(
      'name', role.rolname,
      'superuser', role.rolsuper,
      'inherit', role.rolinherit,
      'create_role', role.rolcreaterole,
      'create_database', role.rolcreatedb,
      'login', role.rolcanlogin,
      'replication', role.rolreplication,
      'bypass_rls', role.rolbypassrls,
      'connection_limit', role.rolconnlimit,
      'valid_until', role.rolvaliduntil,
      'settings', role.rolconfig
    ) ORDER BY role.rolname)
    FROM pg_roles AS role
    WHERE role.rolname NOT LIKE 'pg_%'
  ), '[]'::jsonb),
  'memberships', COALESCE((
    SELECT jsonb_agg(jsonb_build_object(
      'role', granted.rolname,
      'member', member.rolname,
      'grantor', grantor.rolname,
      'admin', membership.admin_option,
      'inherit', membership.inherit_option,
      'set', membership.set_option
    ) ORDER BY granted.rolname, member.rolname, grantor.rolname)
    FROM pg_auth_members AS membership
    JOIN pg_roles AS granted ON granted.oid = membership.roleid
    JOIN pg_roles AS member ON member.oid = membership.member
    JOIN pg_roles AS grantor ON grantor.oid = membership.grantor
    WHERE granted.rolname NOT LIKE 'pg_%'
      AND member.rolname NOT LIKE 'pg_%'
      AND grantor.rolname NOT LIKE 'pg_%'
  ), '[]'::jsonb),
  'settings', COALESCE((
    SELECT jsonb_agg(jsonb_build_object(
      'database', COALESCE(database.datname, ''),
      'role', COALESCE(role.rolname, ''),
      'values', setting.setconfig
    ) ORDER BY COALESCE(database.datname, ''), COALESCE(role.rolname, ''))
    FROM pg_db_role_setting AS setting
    LEFT JOIN pg_database AS database ON database.oid = setting.setdatabase
    LEFT JOIN pg_roles AS role ON role.oid = setting.setrole
    WHERE setting.setrole = 0 OR role.rolname NOT LIKE 'pg_%'
  ), '[]'::jsonb)
)::text;
"""
DATABASE_STATE_SQL = r"""
SELECT jsonb_build_object(
  'owner', pg_get_userbyid(database.datdba),
  'acl', COALESCE(database.datacl::text, ''),
  'connection_limit', database.datconnlimit,
  'allow_connections', database.datallowconn,
  'settings', (
    SELECT setting.setconfig
    FROM pg_db_role_setting AS setting
    WHERE setting.setdatabase = database.oid AND setting.setrole = 0
  )
)::text
FROM pg_database AS database
WHERE database.datname = current_database();
"""
EXTENSION_STATE_SQL = r"""
SELECT jsonb_build_object(
  'extensions', COALESCE((
    SELECT jsonb_agg(jsonb_build_object(
      'name', extension.extname,
      'schema', namespace.nspname,
      'owner', pg_get_userbyid(extension.extowner)
    ) ORDER BY extension.extname)
    FROM pg_extension AS extension
    JOIN pg_namespace AS namespace ON namespace.oid = extension.extnamespace
  ), '[]'::jsonb),
  'statistics_relations', COALESCE((
    SELECT jsonb_agg(jsonb_build_object(
      'name', relation.relname,
      'owner', pg_get_userbyid(relation.relowner),
      'acl', COALESCE((SELECT string_agg(entry, ',' ORDER BY entry) FROM (SELECT CASE WHEN acl.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(acl.grantee) END || '=' || acl.privilege_type || CASE WHEN acl.is_grantable THEN '*' ELSE '' END || '/' || pg_get_userbyid(acl.grantor) AS entry FROM aclexplode(relation.relacl) AS acl WHERE acl.grantee <> relation.relowner) AS entries), '')
    ) ORDER BY relation.relname)
    FROM pg_class AS relation
    JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
    WHERE namespace.nspname = 'public'
      AND relation.relname IN ('pg_stat_statements', 'pg_stat_statements_info')
  ), '[]'::jsonb),
  'statistics_functions', COALESCE((
    SELECT jsonb_agg(jsonb_build_object(
      'name', routine.proname || '(' || oidvectortypes(routine.proargtypes) || ')',
      'owner', pg_get_userbyid(routine.proowner),
      'acl', COALESCE(routine.proacl::text, '')
    ) ORDER BY routine.proname, pg_get_function_identity_arguments(routine.oid))
    FROM pg_proc AS routine
    JOIN pg_namespace AS namespace ON namespace.oid = routine.pronamespace
    WHERE namespace.nspname = 'public'
      AND routine.proname IN ('pg_stat_statements', 'pg_stat_statements_info')
  ), '[]'::jsonb)
)::text;
"""
LOCAL_OBJECT_INVENTORY_SQL = r"""
WITH objects AS (
  SELECT 'schema'::text AS kind,
         namespace.nspname || '|' || pg_get_userbyid(namespace.nspowner) || '|' ||
         COALESCE(namespace.nspacl::text, '') || '|' ||
         COALESCE(obj_description(namespace.oid, 'pg_namespace'), '') AS detail
  FROM pg_namespace AS namespace
  WHERE namespace.nspname NOT IN ('pg_catalog', 'information_schema')
    AND namespace.nspname NOT LIKE 'pg_toast%'
    AND namespace.nspname NOT LIKE 'pg_temp_%'
    AND namespace.nspname NOT LIKE 'pg_toast_temp_%'
  UNION ALL
  SELECT 'type', n.nspname || '.' || t.typname || '|' || t.typtype::text || '|' ||
         t.typcategory::text || '|' || pg_get_userbyid(t.typowner) || '|' ||
         COALESCE(t.typacl::text, '') || '|' || COALESCE((
           SELECT string_agg(enum.enumlabel, ',' ORDER BY enum.enumsortorder)
           FROM pg_enum AS enum
           WHERE enum.enumtypid = t.oid
         ), '')
  FROM pg_type AS t
  JOIN pg_namespace AS n ON n.oid = t.typnamespace
  WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
    AND n.nspname NOT LIKE 'pg_toast%'
    AND n.nspname NOT LIKE 'pg_temp_%'
    AND n.nspname NOT LIKE 'pg_toast_temp_%'
    AND NOT EXISTS (
      SELECT 1 FROM pg_depend AS d
      WHERE d.classid = 'pg_type'::regclass AND d.objid = t.oid
        AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
    )
    -- Row types of extension relations, and their arrays, are only extension members on some majors.
    AND NOT EXISTS (
      SELECT 1 FROM pg_depend AS d
      WHERE d.classid = 'pg_class'::regclass
        AND d.objid IN (t.typrelid, (SELECT element.typrelid FROM pg_type AS element WHERE element.oid = t.typelem))
        AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
    )
  UNION ALL
  SELECT 'default_acl', COALESCE(n.nspname, '') || '|' || pg_get_userbyid(a.defaclrole) || '|' ||
         a.defaclobjtype::text || '|' || COALESCE(a.defaclacl::text, '')
  FROM pg_default_acl AS a
  LEFT JOIN pg_namespace AS n ON n.oid = a.defaclnamespace
  WHERE n.oid IS NULL OR (n.nspname NOT IN ('pg_catalog', 'information_schema')
    AND n.nspname NOT LIKE 'pg_toast%'
    AND n.nspname NOT LIKE 'pg_temp_%'
    AND n.nspname NOT LIKE 'pg_toast_temp_%')
  UNION ALL
  SELECT 'relation'::text AS kind,
         n.nspname || '.' || c.relname || '|' || c.relkind::text || '|' ||
         pg_get_userbyid(c.relowner) || '|' ||
         -- The owner's own privileges differ across majors and pg_dump omits them; every other grant compares.
         COALESCE((SELECT string_agg(entry, ',' ORDER BY entry) FROM (SELECT CASE WHEN acl.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(acl.grantee) END || '=' || acl.privilege_type || CASE WHEN acl.is_grantable THEN '*' ELSE '' END || '/' || pg_get_userbyid(acl.grantor) AS entry FROM aclexplode(c.relacl) AS acl WHERE acl.grantee <> c.relowner) AS entries), '') || '|' ||
         COALESCE(array_to_string(c.reloptions, ','), '') || '|' || c.relpersistence::text || '|' ||
         c.relrowsecurity::text || '|' || c.relforcerowsecurity::text || '|' ||
         CASE WHEN c.relkind = 'S' THEN
           COALESCE(format_type(seq.seqtypid, NULL) || '|' || seq.seqstart::text || '|' ||
                    seq.seqincrement::text || '|' || seq.seqmax::text || '|' ||
                    seq.seqmin::text || '|' || seq.seqcache::text || '|' ||
                    seq.seqcycle::text, 'missing')
         ELSE '' END || '|' ||
         CASE WHEN c.relkind IN ('v', 'm') THEN pg_get_viewdef(c.oid, true) ELSE '' END AS detail
  FROM pg_class AS c
  JOIN pg_namespace AS n ON n.oid = c.relnamespace
  LEFT JOIN pg_sequence AS seq ON seq.seqrelid = c.oid
  WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
    AND n.nspname NOT LIKE 'pg_toast%'
    AND n.nspname NOT LIKE 'pg_temp_%'
    AND n.nspname NOT LIKE 'pg_toast_temp_%'
    AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')
    AND NOT EXISTS (
      SELECT 1 FROM pg_depend AS d
      WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid
        AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
    )
  UNION ALL
  SELECT 'column', n.nspname || '.' || c.relname || '.' || a.attname || '|' ||
         format_type(a.atttypid, a.atttypmod) || '|' || a.attnotnull || '|' ||
         a.attidentity::text || '|' || a.attgenerated::text || '|' || COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
  FROM pg_attribute AS a
  JOIN pg_class AS c ON c.oid = a.attrelid
  JOIN pg_namespace AS n ON n.oid = c.relnamespace
  LEFT JOIN pg_attrdef AS d ON d.adrelid = c.oid AND d.adnum = a.attnum
  WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
    AND n.nspname NOT LIKE 'pg_toast%'
    AND n.nspname NOT LIKE 'pg_temp_%'
    AND n.nspname NOT LIKE 'pg_toast_temp_%'
    AND a.attnum > 0 AND NOT a.attisdropped
    AND NOT EXISTS (
      SELECT 1 FROM pg_depend AS e
      WHERE e.classid = 'pg_class'::regclass AND e.objid = c.oid
        AND e.refclassid = 'pg_extension'::regclass AND e.deptype = 'e'
    )
  UNION ALL
  SELECT 'index', n.nspname || '.' || idx.relname || '|' ||
         i.indisvalid || '|' || i.indisready || '|' || pg_get_indexdef(i.indexrelid)
  FROM pg_index AS i
  JOIN pg_class AS idx ON idx.oid = i.indexrelid
  JOIN pg_namespace AS n ON n.oid = idx.relnamespace
  WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
    AND n.nspname NOT LIKE 'pg_toast%'
    AND n.nspname NOT LIKE 'pg_temp_%'
    AND n.nspname NOT LIKE 'pg_toast_temp_%'
    AND NOT EXISTS (
      SELECT 1 FROM pg_depend AS d
      WHERE d.classid = 'pg_class'::regclass AND d.objid = idx.oid
        AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
    )
  UNION ALL
  SELECT 'constraint', n.nspname || '.' || c.relname || '.' || con.conname || '|' ||
         con.contype::text || '|' || con.convalidated || '|' || pg_get_constraintdef(con.oid, true)
  FROM pg_constraint AS con
  JOIN pg_class AS c ON c.oid = con.conrelid
  JOIN pg_namespace AS n ON n.oid = c.relnamespace
  -- PG18 also catalogs NOT NULL as constraints; column rows already compare nullability.
  WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
    AND n.nspname NOT LIKE 'pg_toast%'
    AND n.nspname NOT LIKE 'pg_temp_%'
    AND n.nspname NOT LIKE 'pg_toast_temp_%'
    AND con.contype <> 'n'
    AND NOT EXISTS (
      SELECT 1 FROM pg_depend AS d
      WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid
        AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
    )
  UNION ALL
  SELECT 'function', n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')|' ||
         pg_get_userbyid(p.proowner) || '|' || p.prosecdef || '|' || p.provolatile::text || '|' ||
         p.proparallel::text || '|' || p.proleakproof || '|' || p.proisstrict || '|' ||
         COALESCE(array_to_string(p.proconfig, ','), '') || '|' || COALESCE(p.proacl::text, '') || '|' ||
         pg_get_functiondef(p.oid)
  FROM pg_proc AS p
  JOIN pg_namespace AS n ON n.oid = p.pronamespace
  WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
    AND n.nspname NOT LIKE 'pg_toast%'
    AND n.nspname NOT LIKE 'pg_temp_%'
    AND n.nspname NOT LIKE 'pg_toast_temp_%'
    AND NOT EXISTS (
      SELECT 1 FROM pg_depend AS d
      WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid
        AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
    )
  UNION ALL
  SELECT 'trigger', n.nspname || '.' || c.relname || '.' || t.tgname || '|' ||
         t.tgenabled::text || '|' || pg_get_triggerdef(t.oid, true)
  FROM pg_trigger AS t
  JOIN pg_class AS c ON c.oid = t.tgrelid
  JOIN pg_namespace AS n ON n.oid = c.relnamespace
  WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
    AND n.nspname NOT LIKE 'pg_toast%'
    AND n.nspname NOT LIKE 'pg_temp_%'
    AND n.nspname NOT LIKE 'pg_toast_temp_%'
    AND NOT t.tgisinternal
  UNION ALL
  SELECT 'rule', n.nspname || '.' || c.relname || '.' || rewrite.rulename || '|' ||
         rewrite.ev_enabled::text || '|' || pg_get_ruledef(rewrite.oid, true)
  FROM pg_rewrite AS rewrite
  JOIN pg_class AS c ON c.oid = rewrite.ev_class
  JOIN pg_namespace AS n ON n.oid = c.relnamespace
  WHERE rewrite.rulename <> '_RETURN'
    AND n.nspname NOT IN ('pg_catalog', 'information_schema')
    AND n.nspname NOT LIKE 'pg_toast%'
    AND n.nspname NOT LIKE 'pg_temp_%'
    AND n.nspname NOT LIKE 'pg_toast_temp_%'
  UNION ALL
  SELECT 'policy', n.nspname || '.' || c.relname || '.' || p.polname || '|' ||
         p.polcmd::text || '|' || p.polpermissive::text || '|' ||
         COALESCE(array_to_string(ARRAY(
           SELECT CASE WHEN role_oid = 0 THEN 'PUBLIC'
                       ELSE COALESCE((SELECT rolname FROM pg_roles WHERE oid = role_oid), role_oid::text)
                  END
           FROM unnest(p.polroles) AS policy_role(role_oid)
           ORDER BY 1
         ), ','), '') || '|' ||
         COALESCE(pg_get_expr(p.polqual, p.polrelid), '') || '|' ||
         COALESCE(pg_get_expr(p.polwithcheck, p.polrelid), '')
  FROM pg_policy AS p
  JOIN pg_class AS c ON c.oid = p.polrelid
  JOIN pg_namespace AS n ON n.oid = c.relnamespace
  WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
    AND n.nspname NOT LIKE 'pg_toast%'
    AND n.nspname NOT LIKE 'pg_temp_%'
    AND n.nspname NOT LIKE 'pg_toast_temp_%'
  UNION ALL
  SELECT 'language', language.lanname || '|' || language.lanpltrusted::text || '|' ||
         pg_get_userbyid(language.lanowner) || '|' || COALESCE(language.lanacl::text, '')
  FROM pg_language AS language
  WHERE language.lanname NOT IN ('internal', 'c', 'sql', 'plpgsql')
)
SELECT count(*)::text || '|' || md5(COALESCE(string_agg(kind || '|' || detail, E'\n' ORDER BY kind, detail), ''))
FROM objects;
"""
OBJECT_INVENTORY_SQL = r"""
WITH objects AS (
  SELECT 'relation'::text AS kind,
         n.nspname || '.' || c.relname || '|' || c.relkind::text || '|' ||
         pg_get_userbyid(c.relowner) || '|' || COALESCE(array_to_string(c.reloptions, ','), '') AS detail
  FROM pg_class AS c
  JOIN pg_namespace AS n ON n.oid = c.relnamespace
  WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')
    AND NOT EXISTS (
      SELECT 1 FROM pg_depend AS d
      WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid
        AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
    )
  UNION ALL
  SELECT 'column', n.nspname || '.' || c.relname || '.' || a.attname || '|' ||
         format_type(a.atttypid, a.atttypmod) || '|' || a.attnotnull || '|' ||
         a.attidentity::text || '|' || a.attgenerated::text || '|' || COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
  FROM pg_attribute AS a
  JOIN pg_class AS c ON c.oid = a.attrelid
  JOIN pg_namespace AS n ON n.oid = c.relnamespace
  LEFT JOIN pg_attrdef AS d ON d.adrelid = c.oid AND d.adnum = a.attnum
  WHERE n.nspname = 'public' AND a.attnum > 0 AND NOT a.attisdropped
    AND NOT EXISTS (
      SELECT 1 FROM pg_depend AS e
      WHERE e.classid = 'pg_class'::regclass AND e.objid = c.oid
        AND e.refclassid = 'pg_extension'::regclass AND e.deptype = 'e'
    )
  UNION ALL
  SELECT 'index', n.nspname || '.' || idx.relname || '|' ||
         i.indisvalid || '|' || i.indisready || '|' || pg_get_indexdef(i.indexrelid)
  FROM pg_index AS i
  JOIN pg_class AS idx ON idx.oid = i.indexrelid
  JOIN pg_namespace AS n ON n.oid = idx.relnamespace
  WHERE n.nspname = 'public'
    AND NOT EXISTS (
      SELECT 1 FROM pg_depend AS d
      WHERE d.classid = 'pg_class'::regclass AND d.objid = idx.oid
        AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
    )
  UNION ALL
  SELECT 'constraint', n.nspname || '.' || c.relname || '.' || con.conname || '|' ||
         con.contype::text || '|' || con.convalidated || '|' || pg_get_constraintdef(con.oid, true)
  FROM pg_constraint AS con
  JOIN pg_class AS c ON c.oid = con.conrelid
  JOIN pg_namespace AS n ON n.oid = c.relnamespace
  -- PG18 also catalogs NOT NULL as constraints; column rows already compare nullability.
  WHERE n.nspname = 'public' AND con.contype <> 'n'
    AND NOT EXISTS (
      SELECT 1 FROM pg_depend AS d
      WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid
        AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
    )
  UNION ALL
  SELECT 'function', n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')|' ||
         pg_get_userbyid(p.proowner) || '|' || p.prosecdef || '|' || p.provolatile::text || '|' ||
         p.proparallel::text || '|' || p.proleakproof || '|' || p.proisstrict || '|' ||
         COALESCE(array_to_string(p.proconfig, ','), '') || '|' || pg_get_functiondef(p.oid)
  FROM pg_proc AS p
  JOIN pg_namespace AS n ON n.oid = p.pronamespace
  WHERE n.nspname = 'public'
    AND NOT EXISTS (
      SELECT 1 FROM pg_depend AS d
      WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid
        AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
    )
  UNION ALL
  SELECT 'trigger', n.nspname || '.' || c.relname || '.' || t.tgname || '|' ||
         t.tgenabled::text || '|' || pg_get_triggerdef(t.oid, true)
  FROM pg_trigger AS t
  JOIN pg_class AS c ON c.oid = t.tgrelid
  JOIN pg_namespace AS n ON n.oid = c.relnamespace
  WHERE n.nspname = 'public' AND NOT t.tgisinternal
)
SELECT count(*)::text || '|' || md5(COALESCE(string_agg(kind || '|' || detail, E'\n' ORDER BY kind, detail), ''))
FROM objects;
"""
TABLE_COUNTS_SQL = r"""
WITH counted AS (
  SELECT c.relname,
         (xpath('/table/row/count/text()', query_to_xml(
           format('SELECT count(*) AS count FROM %I.%I', n.nspname, c.relname), false, true, ''
         )))[1]::text::bigint AS row_count
  FROM pg_class AS c
  JOIN pg_namespace AS n ON n.oid = c.relnamespace
  WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')
    AND NOT EXISTS (
      SELECT 1 FROM pg_depend AS d
      WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid
        AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
    )
)
SELECT count(*)::text || '|' || md5(COALESCE(string_agg(relname || '=' || row_count::text, E'\n' ORDER BY relname), ''))
FROM counted;
"""
INDEX_VALID_SQL = """
SELECT CASE WHEN i.indisvalid AND i.indisready
  AND pg_get_indexdef(i.indexrelid) =
    'CREATE INDEX profile_upload_receipt_file_id_idx ON public.profile_upload_receipt USING btree (file_id) WHERE (file_id IS NOT NULL)'
  THEN 'valid' ELSE 'invalid' END
FROM pg_index AS i
JOIN pg_class AS index_table ON index_table.oid = i.indexrelid
JOIN pg_namespace AS index_schema ON index_schema.oid = index_table.relnamespace
WHERE index_schema.nspname = 'public' AND index_table.relname = 'profile_upload_receipt_file_id_idx';
"""


class DrillFailure(Exception):
    def __init__(self, phase: str, category: str, *, fixture_check: str | None = None) -> None:
        if phase not in SAFE_PHASES or category not in SAFE_CATEGORIES:
            raise ValueError("unsafe report value")
        if fixture_check is not None and (
            phase != "fixture_generation" or fixture_check not in SAFE_FIXTURE_CHECKS
        ):
            raise ValueError("unsafe fixture check")
        super().__init__(category)
        self.phase = phase
        self.category = category
        self.fixture_check = fixture_check


def write_sigkill_marker(fd: int) -> None:
    marker = b"SIGKILL_INJECTED\n"
    try:
        written = os.write(fd, marker)
    except OSError as exc:
        raise DrillFailure(
            "fixture_generation",
            "fixture_failure",
            fixture_check="write_sigkill_marker",
        ) from exc
    if written != len(marker):
        raise DrillFailure(
            "fixture_generation",
            "fixture_failure",
            fixture_check="write_sigkill_marker",
        )


def validate_ambient_environment(environment: dict[str, str]) -> bool:
    exact = {
        "DOCKER_HOST",
        "DOCKER_CONTEXT",
        "DOCKER_TLS_VERIFY",
        "DOCKER_CERT_PATH",
        "DOCKER_CONFIG",
        "PGHOST",
        "PGPORT",
        "PGUSER",
        "PGPASSWORD",
        "PGPASSFILE",
        "PGDATABASE",
        "DATABASE_URL",
        "GITHUB_TOKEN",
        "GH_TOKEN",
        "KUBECONFIG",
        "ATLAS_URL",
        "BUILDKIT_HOST",
        "DOCKER_AUTH_CONFIG",
        "HTTP_PROXY",
        "HTTPS_PROXY",
        "ALL_PROXY",
        "NO_PROXY",
        "SSH_AUTH_SOCK",
        "BASH_ENV",
        "ENV",
    }
    prefixes = (
        "COMPOSE_",
        "TG_",
        "POSTGRES_",
        "PGSERVICE",
        "PGOPTIONS",
        "AWS_",
        "S3_",
        "TURNSTILE_",
        "RUSTFS_",
        "AZURE_",
        "GOOGLE_",
        "BUILDX_",
        "GIT_CONFIG_",
        "TESTCONTAINERS_",
    )
    return not any(name in exact or name.startswith(prefixes) for name in environment)


def environment_digest(environment: dict[str, str]) -> str:
    encoded = "\0".join(f"{key}={environment[key]}" for key in sorted(environment)).encode()
    return hashlib.sha256(encoded).hexdigest()


def env_map(entries: list[str] | None) -> dict[str, str]:
    result: dict[str, str] = {}
    for entry in entries or []:
        key, separator, value = entry.partition("=")
        if not separator or not key or key in result:
            raise ValueError("invalid environment inventory")
        result[key] = value
    return result


def validate_container_inspect(document: dict[str, Any], record: dict[str, Any]) -> bool:
    if document.get("Id") != record.get("id"):
        return False
    if document.get("Name", "").lstrip("/") != record.get("name"):
        return False
    if document.get("Image") != record.get("image_id"):
        return False
    config = document.get("Config") or {}
    labels = config.get("Labels") or {}
    if labels.get(IMAGE_TOKEN_LABEL) != record.get("token") or labels.get(IMAGE_ROLE_LABEL) != record.get("role"):
        return False
    if config.get("User", "") != record.get("user", ""):
        return False
    # The local restore client reads the root-owned dump and reaches the scratch socket through group 70.
    root_roles = {"source_seed": "0:0", "checker_provisioner": "0:0", "local_restore_client": "0:70"}
    if record.get("role") not in root_roles and config.get("User", "").startswith("0"):
        return False
    if record.get("role") in root_roles and config.get("User", "") != root_roles[record["role"]]:
        return False
    if record.get("role") in {
        "application",
        "probe",
        "egress_probe",
        "readiness_probe",
        "migration",
        "source_migration",
        "seed_migration",
        "target_migration",
        "restore_client",
        "source_client",
        "target_client",
        "source_postgres",
        "target_postgres",
    } and config.get("User", "").startswith("0"):
        return False
    if (config.get("Entrypoint") or []) != (record.get("entrypoint") or []):
        return False
    if (config.get("Cmd") or []) != (record.get("command") or []):
        return False
    if config.get("Tty") or bool(config.get("OpenStdin")) != bool(record.get("open_stdin")):
        return False
    # `docker create` always records stdout/stderr attach defaults; only stdin follows the request.
    if bool(config.get("AttachStdin")) != bool(record.get("open_stdin")):
        return False
    try:
        actual_env = env_map(config.get("Env"))
    except ValueError:
        return False
    if environment_digest(actual_env) != record.get("environment_sha256"):
        return False
    host = document.get("HostConfig") or {}
    if host.get("NetworkMode") != record.get("network_mode"):
        return False
    if host.get("Privileged") or not host.get("ReadonlyRootfs"):
        return False
    if host.get("CapAdd") or set(host.get("CapDrop") or []) != {"ALL"}:
        return False
    if "no-new-privileges:true" not in (host.get("SecurityOpt") or []):
        return False
    if host.get("PortBindings") not in (None, {}):
        return False
    if host.get("PublishAllPorts") or host.get("AutoRemove"):
        return False
    if host.get("Devices") not in (None, []):
        return False
    if host.get("Binds") not in (None, []):
        return False
    if host.get("VolumesFrom") or host.get("Links"):
        return False
    if (host.get("ExtraHosts") or []) != (record.get("extra_hosts") or []):
        return False
    if host.get("PidMode") or host.get("UTSMode") or host.get("UsernsMode"):
        return False
    if host.get("IpcMode") != "private" or host.get("CgroupnsMode") != "private" or host.get("Runtime") != "runc":
        return False
    if host.get("Sysctls") not in (None, {}):
        return False
    if (host.get("LogConfig") or {}).get("Type") != "none":
        return False
    if (host.get("RestartPolicy") or {}).get("Name", "no") != "no":
        return False
    if int(host.get("Memory") or 0) != record.get("memory"):
        return False
    if int(host.get("MemorySwap") or 0) != record.get("memory"):
        return False
    if int(host.get("PidsLimit") or 0) != record.get("pids_limit"):
        return False
    if int(host.get("ShmSize") or 0) != record.get("shm_size"):
        return False
    if int(host.get("CpuQuota") or 0) != record.get("cpu_quota"):
        return False
    if int(host.get("CpuPeriod") or 0) != record.get("cpu_period"):
        return False
    if int(host.get("NanoCpus") or 0) != 0:
        return False
    core_limits = [
        item
        for item in (host.get("Ulimits") or [])
        if item.get("Name") == "core"
    ]
    if len(core_limits) != 1 or core_limits[0].get("Soft") != 0 or core_limits[0].get("Hard") != 0:
        return False
    actual_mounts = []
    for mount in document.get("Mounts") or []:
        mount_type = mount.get("Type")
        if mount_type == "tmpfs":
            continue
        if mount_type not in {"bind", "volume"}:
            return False
        item = {
            "type": mount_type,
            "destination": mount.get("Destination"),
            "rw": bool(mount.get("RW")),
        }
        if mount_type == "bind":
            item["source"] = mount.get("Source")
        else:
            item["name"] = mount.get("Name")
            item["source"] = mount.get("Source")
        actual_mounts.append(item)
    expected_mounts = sorted(
        list(record.get("mounts") or []) + list(record.get("unexpected_volume_mounts") or []),
        key=lambda item: item["destination"],
    )
    if sorted(actual_mounts, key=lambda item: item["destination"]) != expected_mounts:
        return False
    role = record.get("role")
    mounts_by_destination = {item["destination"]: item for item in actual_mounts}
    if role == "source_seed":
        if not record.get("network_mode", "").startswith("container:") or set(mounts_by_destination) != {
            "/run/secrets/postgres.dsn",
            "/run/secrets/authkey.hex",
            "/run/secrets/username",
            "/run/secrets/password",
        } or (record.get("entrypoint") or []) != ["/run/app/restore-readiness-seed-shim"]:
            return False
        if any(mount.get("type") != "bind" or mount.get("rw") for mount in mounts_by_destination.values()):
            return False
    elif role == "checker_provisioner":
        if not record.get("network_mode", "").startswith("container:") or set(mounts_by_destination) != {
            "/run/secrets/postgres.dsn",
            "/run/secrets/authkey.hex",
            "/run/secrets/username",
            "/run/secrets/password",
        } or any(mount.get("type") != "bind" or mount.get("rw") for mount in mounts_by_destination.values()):
            return False
        if (record.get("entrypoint") or []) != ["/run/app/restore-readiness-provisioner"]:
            return False
    elif role == "local_restore_client":
        dump_mount = mounts_by_destination.get("/run/local-dump/input.sql")
        socket_mount = mounts_by_destination.get("/run/postgres")
        if (
            not record.get("network_mode", "").startswith("container:")
            or set(mounts_by_destination) != {"/run/local-dump/input.sql", "/run/postgres"}
            or dump_mount is None
            or dump_mount.get("type") != "bind"
            or dump_mount.get("rw")
            or socket_mount is None
            or socket_mount.get("type") != "bind"
            or not socket_mount.get("rw")
            or record.get("entrypoint") != ["/bin/sh"]
        ):
            return False
    elif role == "source_local_dump_fixture":
        output_mount = mounts_by_destination.get("/run/local-dump/output.sql")
        socket_mount = mounts_by_destination.get("/run/postgres")
        if (
            not record.get("network_mode", "").startswith("container:")
            or set(mounts_by_destination) != {"/run/local-dump/output.sql", "/run/postgres"}
            or output_mount is None
            or output_mount.get("type") != "bind"
            or not output_mount.get("rw")
            or socket_mount is None
            or socket_mount.get("type") != "bind"
            or not socket_mount.get("rw")
            or record.get("entrypoint") != ["pg_dump"]
        ):
            return False
    elif role == "local_fixture_attack_client":
        socket_mount = mounts_by_destination.get("/run/postgres")
        if (
            not record.get("network_mode", "").startswith("container:")
            or set(mounts_by_destination) != {"/run/postgres"}
            or socket_mount is None
            or socket_mount.get("type") != "bind"
            or not socket_mount.get("rw")
            or record.get("entrypoint") != ["psql"]
        ):
            return False
    tmpfs = host.get("Tmpfs") or {}
    if tmpfs != (record.get("tmpfs_options") or {}):
        return False
    network_attachments = (document.get("NetworkSettings") or {}).get("Networks") or {}
    if any(name != "none" for name in network_attachments):
        return False
    for network in network_attachments.values():
        if network.get("IPAddress") or network.get("GlobalIPv6Address"):
            return False
    return True


def report_document(
    phase: str,
    category: str,
    cleanup: str,
    elapsed_ms: int,
    *,
    migration_head: str = "not_reached",
    help_get_config: str = "not_run",
    synthetic_signin: str = "not_run",
    auth_keys_cleared: str = "not_run",
    input_mode: str = "synthetic",
    fixture_check: str | None = None,
) -> dict[str, Any]:
    if phase not in SAFE_PHASES | {"preflight"} or category not in SAFE_CATEGORIES:
        raise ValueError("invalid report value")
    if cleanup not in {"verified", "unverified", "not_started"}:
        raise ValueError("invalid cleanup value")
    if migration_head != "not_reached" and migration_head not in repo_migration_versions():
        raise ValueError("invalid migration head")
    if help_get_config not in {"pass", "fail", "not_run"}:
        raise ValueError("invalid config assertion")
    if synthetic_signin not in {"pass", "fail", "not_run"}:
        raise ValueError("invalid sign-in assertion")
    if auth_keys_cleared not in {"pass", "not_run"}:
        raise ValueError("invalid auth-key clearing assertion")
    if input_mode not in {"synthetic", "local"}:
        raise ValueError("invalid restore input mode")
    if phase == "fixture_generation" and fixture_check not in SAFE_FIXTURE_CHECKS:
        raise ValueError("missing fixture check")
    if fixture_check is not None and (
        phase != "fixture_generation" or fixture_check not in SAFE_FIXTURE_CHECKS
    ):
        raise ValueError("invalid fixture check")
    success = category == "assertions_passed" and cleanup == "verified"
    if success:
        if (
            migration_head != HEAD_SCHEMA
            or help_get_config != "pass"
            or synthetic_signin != "pass"
            or auth_keys_cleared != "pass"
        ):
            raise ValueError("incomplete success report")
        result = {
            "result": "pass",
            "migration_head": migration_head,
            "help_getConfig": help_get_config,
            "synthetic_signin": synthetic_signin,
            "auth_keys_cleared": auth_keys_cleared,
            "cleanup": cleanup,
            "elapsed_ms": max(0, int(elapsed_ms)),
            "exit": 0,
        }
        if input_mode == "local":
            result["input_mode"] = input_mode
        return result
    result = {
        "result": "fail",
        "phase": phase,
        "category": category,
        "migration_head": migration_head,
        "help_getConfig": help_get_config,
        "synthetic_signin": synthetic_signin,
        "cleanup": cleanup,
        "elapsed_ms": max(0, int(elapsed_ms)),
        "exit": 1,
    }
    if fixture_check is not None:
        result["fixture_check"] = fixture_check
    return result


class GatewayFixture:
    """A local TCP/DNS target used to prove that a no-network namespace cannot egress."""

    def __init__(self, gateway: str) -> None:
        address = ipaddress.ip_address(gateway)
        if address.version != 4 or address.is_loopback or address.is_unspecified:
            raise DrillFailure("preflight", "isolation_refusal")
        self.gateway = str(address)
        self.stop_event = threading.Event()
        self.lock = threading.Lock()
        self.hits = {"tcp": 0, "dns": 0, "ipv6": 0, "errors": 0}
        self.sockets: list[socket.socket] = []
        self.threads: list[threading.Thread] = []
        try:
            tcp = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            tcp.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            tcp.bind((self.gateway, 0))
            self.port = tcp.getsockname()[1]
            tcp.listen(16)
            tcp.settimeout(0.2)
            self.sockets.append(tcp)

            dns = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            dns.bind((self.gateway, self.port))
            dns.settimeout(0.2)
            self.sockets.append(dns)

            ipv6 = socket.socket(socket.AF_INET6, socket.SOCK_STREAM)
            ipv6.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            ipv6.bind(("::1", self.port))
            ipv6.listen(16)
            ipv6.settimeout(0.2)
            self.sockets.append(ipv6)
        except OSError as exc:
            self.close()
            raise DrillFailure("preflight", "isolation_refusal") from exc

        for sock, name, handler in (
            (tcp, "tcp", self._serve_tcp),
            (dns, "dns", self._serve_dns),
            (ipv6, "ipv6", self._serve_tcp),
        ):
            thread = threading.Thread(target=handler, args=(sock, name), daemon=True)
            thread.start()
            self.threads.append(thread)
        try:
            self._self_test()
        except BaseException:
            self.close()
            raise
        self.baseline = self.snapshot()

    def snapshot(self) -> dict[str, int]:
        with self.lock:
            return dict(self.hits)

    def _increment(self, name: str) -> None:
        with self.lock:
            self.hits[name] += 1

    def _record_error(self) -> None:
        with self.lock:
            self.hits["errors"] += 1

    def _serve_tcp(self, listener: socket.socket, name: str) -> None:
        while not self.stop_event.is_set():
            try:
                client, _ = listener.accept()
            except TimeoutError:
                continue
            except OSError:
                return
            self._increment(name)
            try:
                client.sendall(b"restore-readiness-fixture\n")
            except OSError:
                self._record_error()
            finally:
                try:
                    client.close()
                except OSError:
                    self._record_error()

    def _serve_dns(self, server: socket.socket, name: str) -> None:
        while not self.stop_event.is_set():
            try:
                query, peer = server.recvfrom(512)
            except TimeoutError:
                continue
            except OSError:
                return
            self._increment(name)
            response = dns_fixture_response(query)
            if response is None:
                continue
            try:
                server.sendto(response, peer)
            except OSError:
                self._record_error()

    def _self_test(self) -> None:
        try:
            with socket.create_connection((self.gateway, self.port), timeout=1) as connection:
                if connection.recv(64) != b"restore-readiness-fixture\n":
                    raise DrillFailure("preflight", "isolation_refusal")
            with socket.create_connection(("::1", self.port), timeout=1) as connection:
                if connection.recv(64) != b"restore-readiness-fixture\n":
                    raise DrillFailure("preflight", "isolation_refusal")
            query = dns_fixture_query()
            with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as client:
                client.settimeout(1)
                client.sendto(query, (self.gateway, self.port))
                response, _ = client.recvfrom(512)
                if not dns_fixture_valid_response(query, response):
                    raise DrillFailure("preflight", "isolation_refusal")
        except (OSError, DrillFailure) as exc:
            raise DrillFailure("preflight", "isolation_refusal") from exc
        hits = self.snapshot()
        if any(hits[name] < 1 for name in ("tcp", "dns", "ipv6")) or hits["errors"] != 0:
            raise DrillFailure("preflight", "isolation_refusal")

    def close(self, deadline: float | None = None) -> bool:
        self.stop_event.set()
        clean = True
        for sock in self.sockets:
            try:
                sock.close()
            except OSError:
                clean = False
        for thread in self.threads:
            timeout = 1 if deadline is None else max(0, min(1, deadline - time.monotonic() - CLEANUP_FINALIZE_SECONDS))
            thread.join(timeout=timeout)
            if thread.is_alive():
                clean = False
        return clean


def dns_fixture_query() -> bytes:
    query = bytearray(b"\x72\x64\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00")
    for label in (b"restore-readiness-fixture", b"invalid"):
        query.append(len(label))
        query.extend(label)
    query.extend(b"\x00\x00\x01\x00\x01")
    return bytes(query)


def dns_fixture_response(query: bytes) -> bytes | None:
    if len(query) < 17 or int.from_bytes(query[4:6], "big") != 1:
        return None
    question_end = 12
    while question_end < len(query) and query[question_end] != 0:
        label_length = query[question_end]
        if label_length > 63 or question_end + label_length + 1 >= len(query):
            return None
        question_end += label_length + 1
    question_end += 5
    if question_end > len(query):
        return None
    header = query[:2] + b"\x81\x80" + query[4:6] + b"\x00\x01\x00\x00\x00\x00"
    answer = b"\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x00\x00\x04\xc0\x00\x02\x35"
    return header + query[12:question_end] + answer


def dns_fixture_valid_response(query: bytes, response: bytes) -> bool:
    return (
        len(response) >= 12
        and response[:2] == query[:2]
        and int.from_bytes(response[2:4], "big") & 0x8000 != 0
        and int.from_bytes(response[6:8], "big") == 1
    )


def ensure_trusted_system_executable(executable: str) -> str:
    try:
        candidate = Path(executable)
        if not candidate.is_absolute():
            raise ValueError("executable path is not absolute")
        resolved = candidate.resolve(strict=True)
        info = resolved.lstat()
        if (
            not stat.S_ISREG(info.st_mode)
            or info.st_uid != 0
            or not info.st_mode & 0o111
            or info.st_mode & 0o022
        ):
            raise ValueError("executable is not a trusted system file")
        for parent in resolved.parents:
            parent_info = parent.lstat()
            if (
                not stat.S_ISDIR(parent_info.st_mode)
                or parent_info.st_uid != 0
                or parent_info.st_mode & (stat.S_IWGRP | stat.S_IWOTH)
            ):
                raise ValueError("executable has a writable parent")
        return str(resolved)
    except (OSError, RuntimeError, ValueError) as exc:
        raise DrillFailure("preflight", "preflight_failure") from exc


class Docker:
    def __init__(self, executable: str, manifest_path: Path, manifest: dict[str, Any]) -> None:
        self.executable = ensure_trusted_system_executable(executable)
        self.manifest_path = manifest_path
        self.manifest = manifest
        self.environment = {
            "HOME": "/root",
            "LANG": "C",
            "LC_ALL": "C",
            "PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
        }

    def _track_child(self, process: subprocess.Popen[bytes]) -> dict[str, int]:
        ticks = process_start_ticks(process.pid)
        if ticks is None:
            process.kill()
            process.wait()
            raise DrillFailure(getattr(self, "phase", "cleanup"), "cleanup_failure")
        record = {"pid": process.pid, "start_ticks": ticks}
        self.manifest.setdefault("processes", []).append(record)
        saver = getattr(self, "save", None)
        try:
            if saver is not None:
                saver()
        except BaseException:
            process.kill()
            process.wait()
            self.manifest["processes"].remove(record)
            raise
        return record

    def _untrack_child(self, record: dict[str, int]) -> None:
        processes = self.manifest.setdefault("processes", [])
        self.manifest["processes"] = [item for item in processes if item != record]
        saver = getattr(self, "save", None)
        if saver is not None:
            saver()

    def cleanup_remaining(self) -> float:
        deadline = self.manifest.get("cleanup_deadline")
        if not isinstance(deadline, (int, float)):
            raise DrillFailure("cleanup", "cleanup_failure")
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise DrillFailure("cleanup", "cleanup_failure")
        return remaining

    def cleanup_budget_available(self) -> bool:
        try:
            return self.cleanup_remaining() > CLEANUP_REAP_SECONDS + CLEANUP_FINALIZE_SECONDS
        except DrillFailure:
            return False

    @staticmethod
    def _terminate_and_reap(process: subprocess.Popen[bytes]) -> bool:
        if process.poll() is None:
            try:
                process.terminate()
            except ProcessLookupError:
                pass
            except OSError:
                return False
        try:
            process.communicate(timeout=2)
        except subprocess.TimeoutExpired:
            try:
                process.kill()
            except ProcessLookupError:
                pass
            except OSError:
                return False
            try:
                process.communicate(timeout=2)
            except subprocess.TimeoutExpired:
                return False
        except OSError:
            return False
        return process.poll() is not None

    def _run_child(
        self,
        args: list[str],
        *,
        timeout: float,
        stdin: Any = subprocess.DEVNULL,
        stdout: Any = subprocess.PIPE,
        input_bytes: bytes | None = None,
        environment: dict[str, str] | None = None,
        phase: str,
        category: str,
        capture_stderr: bool = False,
    ) -> tuple[int, bytes]:
        cleanup_deadline: float | None = None
        if phase == "cleanup":
            timeout = min(timeout, self.cleanup_remaining() - CLEANUP_REAP_SECONDS - CLEANUP_FINALIZE_SECONDS)
            if timeout <= 0:
                raise DrillFailure("cleanup", "cleanup_failure")
            cleanup_deadline = float(self.manifest["cleanup_deadline"])
        try:
            process = subprocess.Popen(
                args,
                stdin=subprocess.PIPE if input_bytes is not None else stdin,
                stdout=stdout,
                stderr=subprocess.PIPE if capture_stderr else subprocess.DEVNULL,
                env=environment or self.environment,
                close_fds=True,
            )
        except OSError as exc:
            if capture_stderr:
                self.report_command_failure(
                    args,
                    f"could not start ({type(exc).__name__}: {exc})",
                    b"",
                )
            raise DrillFailure(phase, category) from exc
        try:
            tracked = self._track_child(process)
        except BaseException:
            process.kill()
            process.wait()
            raise
        try:
            try:
                if cleanup_deadline is not None:
                    timeout = min(
                        timeout,
                        self.cleanup_remaining() - CLEANUP_REAP_SECONDS - CLEANUP_FINALIZE_SECONDS,
                    )
                    if timeout <= 0:
                        raise DrillFailure("cleanup", "cleanup_failure")
                output, error_output = process.communicate(input_bytes, timeout=timeout)
            except subprocess.TimeoutExpired as exc:
                try:
                    process.kill()
                except ProcessLookupError:
                    pass
                if cleanup_deadline is None:
                    _, error_output = process.communicate()
                    if capture_stderr:
                        self.report_command_failure(args, "timed out", error_output or exc.stderr or b"")
                    raise DrillFailure(phase, "timeout") from exc
                remaining = max(0.0, cleanup_deadline - time.monotonic() - CLEANUP_FINALIZE_SECONDS)
                if remaining:
                    try:
                        process.wait(timeout=min(CLEANUP_REAP_SECONDS, remaining))
                    except subprocess.TimeoutExpired:
                        pass
                raise DrillFailure("cleanup", "cleanup_failure") from exc
            if capture_stderr and process.returncode != 0:
                self.report_command_failure(args, f"exit {process.returncode}", error_output or b"")
            return process.returncode, output or b""
        finally:
            if process.poll() is None:
                try:
                    process.kill()
                except ProcessLookupError:
                    pass
                if cleanup_deadline is None:
                    process.communicate()
                else:
                    remaining = max(0.0, cleanup_deadline - time.monotonic() - CLEANUP_FINALIZE_SECONDS)
                    if remaining:
                        try:
                            process.wait(timeout=min(CLEANUP_REAP_SECONDS, remaining))
                        except subprocess.TimeoutExpired:
                            pass
            if cleanup_deadline is not None:
                for stream in (process.stdin, process.stdout, process.stderr):
                    if stream is not None:
                        stream.close()
            if process.poll() is not None:
                self._untrack_child(tracked)

    def report_command_failure(self, args: list[str], outcome: str, stderr: bytes) -> None:
        command = json.dumps(args, separators=(",", ":"))
        detail = json.dumps(stderr.decode("utf-8", "replace"), ensure_ascii=True)
        print(
            f"restore readiness preflight Docker command failed: command={command} outcome={outcome} stderr={detail}",
            file=sys.stderr,
        )

    def call(
        self,
        args: list[str],
        *,
        phase: str,
        category: str,
        timeout: float = 30,
        input_bytes: bytes | None = None,
        allow_interrupted: bool = False,
    ) -> bytes:
        if time.monotonic() >= self.manifest["work_deadline"]:
            raise DrillFailure(phase, "timeout")
        child_timeout = min(timeout, max(1, self.manifest["work_deadline"] - time.monotonic()))
        status, output = self._run_child(
            [self.executable, *args],
            timeout=child_timeout,
            input_bytes=input_bytes,
            phase=phase,
            category=category,
            capture_stderr=phase == "preflight",
        )
        if getattr(self, "interrupted", None) and not allow_interrupted:
            raise DrillFailure(phase, "interrupted")
        if len(output) > MAX_CAPTURE:
            raise DrillFailure(phase, category)
        if status != 0:
            raise DrillFailure(phase, category)
        return output

    def call_status(
        self,
        args: list[str],
        *,
        phase: str,
        category: str,
        timeout: float = 30,
        input_bytes: bytes | None = None,
    ) -> tuple[int, bytes]:
        if time.monotonic() >= self.manifest["work_deadline"]:
            raise DrillFailure(phase, "timeout")
        child_timeout = min(timeout, max(1, self.manifest["work_deadline"] - time.monotonic()))
        status, output = self._run_child(
            [self.executable, *args],
            timeout=child_timeout,
            input_bytes=input_bytes,
            phase=phase,
            category=category,
        )
        if getattr(self, "interrupted", None):
            raise DrillFailure(phase, "interrupted")
        if len(output) > MAX_CAPTURE:
            raise DrillFailure(phase, category)
        return status, output

    def cleanup_call(
        self,
        args: list[str],
        *,
        timeout: float = 30,
        input_bytes: bytes | None = None,
    ) -> tuple[int, bytes]:
        active = getattr(self, "cleanup_active", False)
        operation = docker_cleanup_operation(args)
        if active:
            self.active_cleanup_operation = operation
        record_failure = getattr(self, "record_cleanup_failure", None)
        try:
            status, output = self._run_child(
                [self.executable, *args],
                timeout=timeout,
                input_bytes=input_bytes,
                phase="cleanup",
                category="cleanup_failure",
            )
        except DrillFailure as exc:
            if active and record_failure is not None:
                error = "command_timeout" if isinstance(exc.__cause__, subprocess.TimeoutExpired) else "command_failed"
                record_failure(operation, error)
            raise
        if len(output) > MAX_CAPTURE:
            if active and record_failure is not None:
                record_failure(operation, "invalid_output")
            raise DrillFailure("cleanup", "cleanup_failure")
        if status != 0 and active and record_failure is not None:
            record_failure(operation, "command_failed", status)
        return status, output


def private_file(path: Path, content: bytes, mode: int, owner: tuple[int, int] = (0, 0)) -> None:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW, mode)
    try:
        os.fchown(descriptor, owner[0], owner[1])
        os.fchmod(descriptor, mode)
        view = memoryview(content)
        while view:
            written = os.write(descriptor, view)
            if written <= 0:
                raise OSError("short write")
            view = view[written:]
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def read_one_line(path: Path, max_bytes: int = 256) -> bytes:
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_nlink != 1 or info.st_size > max_bytes:
        raise DrillFailure("fixture_generation", "fixture_failure")
    descriptor = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
    try:
        result = bytearray()
        while len(result) <= max_bytes:
            chunk = os.read(descriptor, max_bytes + 1 - len(result))
            if not chunk:
                break
            result.extend(chunk)
        if len(result) > max_bytes:
            raise DrillFailure("fixture_generation", "fixture_failure")
        return bytes(result)
    finally:
        os.close(descriptor)


def ensure_executable_file(path: Path) -> None:
    try:
        info = path.lstat()
    except OSError as exc:
        raise DrillFailure("fixture_generation", "fixture_failure") from exc
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or not info.st_mode & 0o111 or info.st_size <= 0:
        raise DrillFailure("fixture_generation", "fixture_failure")


def parse_env_file(data: bytes) -> list[tuple[str, str]]:
    try:
        text = data.decode("ascii", "strict")
    except UnicodeDecodeError as exc:
        raise DrillFailure("fixture_generation", "fixture_failure") from exc
    result: list[tuple[str, str]] = []
    seen: set[str] = set()
    for line in text.splitlines():
        if not line or line.startswith("#") or "=" not in line:
            raise DrillFailure("fixture_generation", "fixture_failure")
        key, value = line.split("=", 1)
        if not re.fullmatch(r"[A-Z][A-Z0-9_]*", key) or key in seen or any(char in value for char in "\r\n\x00"):
            raise DrillFailure("fixture_generation", "fixture_failure")
        result.append((key, value))
        seen.add(key)
    if not result:
        raise DrillFailure("fixture_generation", "fixture_failure")
    return result


def database_dsn(password: str) -> str:
    if not re.fullmatch(r"[0-9a-f]{64}", password):
        raise DrillFailure("fixture_generation", "fixture_failure")
    return f"postgres://postgres:{password}@/teagram?host=/run/postgres&sslmode=disable"


def database_tcp_dsn(password: str) -> str:
    if not re.fullmatch(r"[0-9a-f]{64}", password):
        raise DrillFailure("fixture_generation", "fixture_failure")
    return f"postgres://postgres:{password}@127.0.0.1:5432/teagram?sslmode=disable"


def process_start_ticks(pid: int) -> int | None:
    try:
        data = Path(f"/proc/{pid}/stat").read_text(encoding="ascii")
    except FileNotFoundError:
        return None
    except OSError:
        return None
    closing = data.rfind(")")
    if closing < 0:
        return None
    fields = data[closing + 2 :].split()
    if len(fields) <= 19 or not fields[19].isdigit():
        return None
    return int(fields[19])


def process_is_alive(pid: int) -> bool | None:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    except OSError:
        return None
    return True


def docker_cli_pids() -> list[int]:
    pids: list[int] = []
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit():
            continue
        try:
            if os.path.basename(os.readlink(entry / "exe")) == "docker":
                pids.append(int(entry.name))
        except FileNotFoundError:
            continue
        except OSError:
            continue
    return sorted(pids)


def unique_json_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result


def validate_cleanup_failure_document(value: Any) -> dict[str, Any] | None:
    if type(value) is not dict or not {"operation", "error"} <= set(value):
        return None
    if set(value) - {"operation", "error", "exit_status", "stage"}:
        return None
    if "stage" in value and value["stage"] not in SAFE_SUPERVISOR_STAGES:
        return None
    operation = value.get("operation")
    error = value.get("error")
    if (
        not isinstance(operation, str)
        or not isinstance(error, str)
        or operation not in SAFE_CLEANUP_OPERATIONS
        or error not in SAFE_CLEANUP_ERRORS
    ):
        return None
    result: dict[str, Any] = {"operation": operation, "error": error}
    if "exit_status" in value:
        status = value["exit_status"]
        if type(status) is not int or status == 0 or not -255 <= status <= 255:
            return None
        result["exit_status"] = status
    if "stage" in value:
        result["stage"] = value["stage"]
    return result


def cleanup_failure_document(
    operation: str,
    error: str,
    exit_status: int | None = None,
    *,
    stage: str | None = None,
) -> dict[str, Any]:
    value: dict[str, Any] = {"operation": operation, "error": error}
    if exit_status is not None:
        value["exit_status"] = exit_status
    if stage is not None:
        value["stage"] = stage
    result = validate_cleanup_failure_document(value)
    if result is None:
        raise ValueError("invalid cleanup failure diagnostic")
    return result


def cleanup_failure_log_message(value: Any) -> str | None:
    diagnostic = validate_cleanup_failure_document(value)
    if diagnostic is None:
        return None
    message = (
        "restore readiness cleanup failed: "
        f"operation={diagnostic['operation']} error={diagnostic['error']}"
    )
    if "stage" in diagnostic:
        message += f" stage={diagnostic['stage']}"
    if "exit_status" in diagnostic:
        message += f" exit_status={diagnostic['exit_status']}"
    return message


def parse_cleanup_failure_output(output: bytes) -> str | None:
    if not output or len(output) > 1024 or not output.endswith(b"\n") or output.count(b"\n") != 1:
        return None
    try:
        value = json.loads(output[:-1], object_pairs_hook=unique_json_object)
    except (UnicodeDecodeError, ValueError, json.JSONDecodeError):
        return None
    return cleanup_failure_log_message(value)


def emit_cleanup_failure(value: Any) -> None:
    diagnostic = validate_cleanup_failure_document(value)
    if diagnostic is None:
        diagnostic = cleanup_failure_document("cleanup_resources", "verification_failed")
    print(json.dumps(diagnostic, separators=(",", ":"), sort_keys=True), flush=True)


def docker_cleanup_operation(args: list[str]) -> str:
    if not args:
        return "cleanup_resources"
    if args[0] == "ps":
        if "--filter" in args:
            filter_index = args.index("--filter") + 1
            if filter_index < len(args):
                value = args[filter_index]
                if value.startswith("ancestor="):
                    return "docker_image_use_check"
                if value.startswith("volume="):
                    return "docker_volume_use_check"
        return "docker_container_inventory"
    if args[0] == "volume" and len(args) > 1:
        return {
            "inspect": "docker_volume_inspect",
            "ls": "docker_volume_inventory",
            "rm": "docker_volume_remove",
        }.get(args[1], "cleanup_resources")
    if args[0] == "image" and len(args) > 1:
        return {
            "inspect": "docker_image_inspect",
            "ls": "docker_image_inventory",
            "rm": "docker_image_remove",
        }.get(args[1], "cleanup_resources")
    if args[0] == "network" and len(args) > 1 and args[1] == "ls":
        return "docker_network_inventory"
    if args[0] == "inspect":
        return "docker_container_inspect"
    if args[0] == "stop":
        return "docker_container_stop"
    if args[0] == "rm":
        return "docker_container_remove"
    return "cleanup_resources"


def parse_provisioner_result(stdout: bytes, exit_code: int, expected_username: str) -> tuple[str, dict[str, Any] | None]:
    try:
        result = json.loads(stdout, object_pairs_hook=unique_json_object)
    except (json.JSONDecodeError, UnicodeDecodeError, ValueError):
        return "provisioning_failure", None
    if not isinstance(result, dict) or type(result.get("schema_version")) is not int or result["schema_version"] != 1:
        return "provisioning_failure", None
    category = result.get("category")
    if category in {"identity_collision", "timeout", "provisioning_failure"}:
        if set(result) == {"schema_version", "category"} and exit_code != 0:
            return category, None
        return "provisioning_failure", None
    if (
        set(result) != {"schema_version", "category", "user_id", "username"}
        or category != "provisioned"
        or exit_code != 0
        or type(result.get("user_id")) is not int
        or result["user_id"] <= 0
        or result.get("username") != expected_username
    ):
        return "provisioning_failure", None
    return "provisioned", {
        "schema_version": 1,
        "user_id": result["user_id"],
        "username": expected_username,
    }


def checker_passes(result: dict[str, Any], exit_code: int) -> bool:
    return (
        result.get("category") == "assertions_passed"
        and result.get("help_getConfig") == "pass"
        and result.get("synthetic_signin") == "pass"
        and exit_code == 0
    )


def contains_leak_marker(stdout: bytes, artifact: bytes, markers: list[bytes]) -> bool:
    return any(marker and (marker in stdout or marker in artifact) for marker in markers)


def require_container_running(document: dict[str, Any], phase: str, category: str) -> None:
    if not (document.get("State") or {}).get("Running"):
        raise DrillFailure(phase, category)


def record_drill_signal(drill: Any, signum: int, previous: DrillFailure | None, parent_done: bool) -> DrillFailure:
    failure = previous or DrillFailure(drill.phase, "interrupted")
    if drill.interrupted is None:
        drill.interrupted = str(signum)
    if not parent_done:
        raise failure
    return failure


def load_manifest(path: Path) -> dict[str, Any]:
    try:
        info = path.lstat()
        parent_info = path.parent.lstat()
        if (
            os.geteuid() != 0
            or path.name != "ownership.json"
            or path.is_symlink()
            or not stat.S_ISREG(info.st_mode)
            or info.st_uid != 0
            or stat.S_IMODE(info.st_mode) != 0o600
            or info.st_nlink != 1
            or info.st_size > 2 * 1024 * 1024
            or path.parent.is_symlink()
            or not stat.S_ISDIR(parent_info.st_mode)
            or parent_info.st_uid != 0
            or stat.S_IMODE(parent_info.st_mode) != 0o700
            or path.parent.parent != Path("/run")
            or not path.parent.name.startswith("restore-readiness-")
        ):
            raise ValueError("invalid ownership file")
        descriptor = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
        try:
            data = os.read(descriptor, 2 * 1024 * 1024 + 1)
        finally:
            os.close(descriptor)
        if len(data) > 2 * 1024 * 1024:
            raise ValueError("ownership file too large")
        manifest = json.loads(data, object_pairs_hook=unique_json_object)
    except (OSError, ValueError, json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise DrillFailure("cleanup", "cleanup_failure") from exc
    if (
        not isinstance(manifest, dict)
        or manifest.get("schema_version") != 1
        or manifest.get("run_dir") != str(path.parent)
        or not re.fullmatch(r"[0-9a-f]{32}", manifest.get("token", ""))
        or not isinstance(manifest.get("containers"), list)
        or not isinstance(manifest.get("images"), list)
        or not isinstance(manifest.get("files"), list)
        or not isinstance(manifest.get("processes"), list)
        or not isinstance(manifest.get("parent"), dict)
        or not isinstance(manifest.get("supervisor"), dict)
        or not isinstance(manifest.get("runtime"), dict)
        or not isinstance(manifest.get("global_lock"), dict)
        or not isinstance(manifest.get("rollout_lock"), dict)
        or not isinstance(manifest.get("prepared_runtime"), dict)
        or not isinstance(manifest.get("work_deadline"), (float, int))
        or not isinstance(manifest.get("cleanup_deadline"), (float, int))
        or not isinstance(manifest.get("cleanup_failures", []), list)
        or any(step not in CLEANUP_FAILURE_STEPS for step in manifest.get("cleanup_failures", []))
        or (
            manifest.get("foreign_inventory_diagnostic") is not None
            and (
                not isinstance(manifest.get("foreign_inventory_diagnostic"), str)
                or FOREIGN_INVENTORY_DIAGNOSTIC.fullmatch(manifest["foreign_inventory_diagnostic"]) is None
            )
        )
    ):
        raise DrillFailure("cleanup", "cleanup_failure")
    lock_record = manifest["global_lock"]
    try:
        lock_path = Path(lock_record["path"])
        lock_info = lock_path.lstat()
        if (
            str(lock_path) != "/run/restore-readiness.lock"
            or lock_path.is_symlink()
            or not stat.S_ISREG(lock_info.st_mode)
            or lock_info.st_uid != 0
            or stat.S_IMODE(lock_info.st_mode) != 0o600
            or lock_info.st_nlink != 1
            or lock_info.st_dev != lock_record["device"]
            or lock_info.st_ino != lock_record["inode"]
        ):
            raise ValueError("invalid global lock")
    except (KeyError, OSError, TypeError, ValueError) as exc:
        raise DrillFailure("cleanup", "cleanup_failure") from exc
    rollout_record = manifest["rollout_lock"]
    try:
        rollout_path = Path(rollout_record["path"])
        rollout_info = rollout_path.lstat()
        if (
            str(rollout_path) != str(ROLLOUT_LOCK_PATH)
            or rollout_path.is_symlink()
            or not stat.S_ISREG(rollout_info.st_mode)
            or rollout_info.st_uid != 0
            or stat.S_IMODE(rollout_info.st_mode) != 0o600
            or rollout_info.st_nlink != 1
            or rollout_info.st_dev != rollout_record["device"]
            or rollout_info.st_ino != rollout_record["inode"]
        ):
            raise ValueError("invalid rollout lock")
    except (KeyError, OSError, TypeError, ValueError) as exc:
        raise DrillFailure("cleanup", "cleanup_failure") from exc
    prepared_runtime = manifest["prepared_runtime"]
    try:
        prepared_path = prepared_directory_path(Path(prepared_runtime["path"]))
        prepared_info = prepared_path.lstat()
        attestation_path = prepared_path / PREPARED_ATTESTATION
        attestation_info = attestation_path.lstat()
        attestation_fd = os.open(attestation_path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
        try:
            attestation_data = os.read(attestation_fd, 1024 * 1024 + 1)
        finally:
            os.close(attestation_fd)
        if (
            set(prepared_runtime) != {"path", "device", "inode", "attestation_sha256", "prepared_uid", "go_arch"}
            or not stat.S_ISDIR(prepared_info.st_mode)
            or prepared_info.st_uid != 0
            or stat.S_IMODE(prepared_info.st_mode) != 0o500
            or prepared_info.st_dev != prepared_runtime["device"]
            or prepared_info.st_ino != prepared_runtime["inode"]
            or not stat.S_ISREG(attestation_info.st_mode)
            or attestation_info.st_uid != 0
            or stat.S_IMODE(attestation_info.st_mode) != 0o400
            or len(attestation_data) > 1024 * 1024
            or hashlib.sha256(attestation_data).hexdigest() != prepared_runtime["attestation_sha256"]
            or not isinstance(prepared_runtime["prepared_uid"], int)
            or prepared_runtime["prepared_uid"] <= 0
            or prepared_runtime["go_arch"] not in {"amd64", "arm64"}
        ):
            raise ValueError("invalid prepared runtime record")
    except (KeyError, OSError, TypeError, ValueError) as exc:
        raise DrillFailure("cleanup", "cleanup_failure") from exc
    for item in manifest["processes"]:
        if (
            not isinstance(item, dict)
            or set(item) != {"pid", "start_ticks"}
            or not isinstance(item["pid"], int)
            or not isinstance(item["start_ticks"], int)
            or item["pid"] <= 1
            or item["start_ticks"] <= 0
        ):
            raise DrillFailure("cleanup", "cleanup_failure")
    runtime = manifest["runtime"]
    parent_record = manifest["parent"]
    supervisor_record = manifest["supervisor"]
    build_directory = manifest.get("build_directory")
    if (
        not all(
            isinstance(runtime.get(key), str) and runtime[key]
            for key in ("docker_bin", "atlas_bin", "build_dir", "script")
        )
        or not isinstance(runtime.get("build_uid"), int)
        or runtime.get("go_arch") not in {"amd64", "arm64"}
        or not isinstance(parent_record.get("pid"), int)
        or not isinstance(parent_record.get("start_ticks"), int)
        or parent_record["pid"] <= 1
        or parent_record["start_ticks"] <= 0
        or not isinstance(supervisor_record.get("pid"), int)
        or not isinstance(supervisor_record.get("start_ticks"), int)
        or supervisor_record["pid"] < 0
        or supervisor_record["start_ticks"] < 0
        or not isinstance(manifest.get("resource_creation_started"), bool)
        or (build_directory is not None and not isinstance(build_directory, dict))
    ):
        raise DrillFailure("cleanup", "cleanup_failure")
    if build_directory is not None:
        if (
            set(build_directory) != {"path", "device", "inode", "mode", "uid", "gid"}
            or build_directory.get("path") != runtime.get("build_dir")
            or any(not isinstance(build_directory.get(key), int) for key in ("device", "inode", "mode", "uid", "gid"))
        ):
            raise DrillFailure("cleanup", "cleanup_failure")
    seen_files: set[str] = set()
    for item in manifest["files"]:
        if not isinstance(item, dict) or not isinstance(item.get("path"), str) or item["path"] in seen_files:
            raise DrillFailure("cleanup", "cleanup_failure")
        seen_files.add(item["path"])
        file_path = Path(item["path"])
        if not file_path.is_absolute():
            raise DrillFailure("cleanup", "cleanup_failure")
        try:
            file_path.relative_to(path.parent)
            in_run_dir = True
        except ValueError:
            in_run_dir = False
        if not in_run_dir and not (
            build_directory is not None
            and item.get("purpose", "").startswith("build_input_")
            and file_path.parent == Path(build_directory["path"])
        ):
            raise DrillFailure("cleanup", "cleanup_failure")
    return manifest


def create_image_build_contexts(build_root: Path) -> tuple[Path, Path]:
    build_root.mkdir(mode=0o700)
    app_context = build_root / "app"
    tools_context = build_root / "tools"
    app_context.mkdir(mode=0o700)
    tools_context.mkdir(mode=0o700)
    return app_context, tools_context


class Drill(Docker):
    def __init__(self, args: argparse.Namespace, run_dir: Path, manifest: dict[str, Any]) -> None:
        self.args = args
        self.run_dir = run_dir
        self.manifest = manifest
        self.prepared_runtime = getattr(args, "prepared_runtime", manifest.get("prepared_runtime"))
        self.input_mode = "local" if getattr(args, "local_dump", None) is not None or getattr(args, "local_fixture_test", False) else "synthetic"
        self.local_dump_path: Path | None = getattr(args, "local_dump", None)
        self.local_dump_fd: int | None = getattr(args, "local_dump_fd", None)
        self.local_dump_info: dict[str, int] | None = getattr(args, "local_dump_identity", None)
        self.local_admin_role: str | None = None
        self.target_db_password: str | None = None
        self.manifest_path = run_dir / "ownership.json"
        super().__init__(args.docker_bin, self.manifest_path, manifest)
        self.phase = "preflight"
        self.preflight_step = "initialize private run"
        self.started = time.monotonic()
        self.gateway: GatewayFixture | None = None
        self.images: dict[str, dict[str, Any]] = {}
        self.files: dict[str, Path] = {}
        self.sockets: dict[str, Path] = {}
        self.databases: dict[str, dict[str, Any]] = {}
        self.checker_result = {"help_getConfig": "not_run", "synthetic_signin": "not_run"}
        self.migration_head = "not_reached"
        self.synthetic_auth_key_ids: tuple[int, ...] = ()
        self.post_restore_marker: dict[str, Any] | None = None
        self.auth_keys_cleared = False
        self.supervisor: subprocess.Popen[bytes] | None = None
        self.interrupted: str | None = None
        self.fixture_check = "fixture_setup"
        self.cleanup_active = False
        self.cleanup_failure: dict[str, Any] | None = None
        self.active_cleanup_operation: str | None = None

    def record_cleanup_failure(
        self,
        operation: str,
        error: str,
        exit_status: int | None = None,
    ) -> None:
        if getattr(self, "cleanup_failure", None) is None:
            self.cleanup_failure = cleanup_failure_document(operation, error, exit_status)

    def save(self) -> None:
        temporary = self.manifest_path.with_suffix(".tmp")
        try:
            temporary_info = temporary.lstat()
        except FileNotFoundError:
            temporary_info = None
        if temporary_info is not None and (
            not stat.S_ISREG(temporary_info.st_mode)
            or temporary_info.st_uid != 0
            or stat.S_IMODE(temporary_info.st_mode) != 0o600
            or temporary_info.st_nlink != 1
        ):
            raise DrillFailure(self.phase, "cleanup_failure")
        descriptor = os.open(
            temporary,
            os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_CLOEXEC | os.O_NOFOLLOW,
            0o600,
        )
        os.fchown(descriptor, 0, 0)
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            json.dump(self.manifest, stream, separators=(",", ":"), sort_keys=True)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, self.manifest_path)
        directory_fd = os.open(self.manifest_path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)

    def create_private_layout(self) -> None:
        os.chmod(self.run_dir, 0o700)
        self.manifest.setdefault("files", [])
        for name in ("work", "sockets", "invocation", "seed", "secrets", "migrations"):
            directory = self.run_dir / name
            directory.mkdir(mode=0o700)
            os.chmod(directory, 0o700)
        for name in ("source", "target"):
            path = self.run_dir / "sockets" / name
            path.mkdir(mode=0o770)
            os.chown(path, 70, 70)
            os.chmod(path, 0o770)
            self.sockets[name] = path
        migration_source = ROOT / "migrations"
        if migration_source.is_symlink() or not migration_source.is_dir():
            raise DrillFailure("preflight", "preflight_failure")
        for item in migration_source.iterdir():
            if item.is_symlink() or not item.is_file():
                raise DrillFailure("preflight", "preflight_failure")
            target = self.run_dir / "migrations" / item.name
            shutil.copyfile(item, target, follow_symlinks=False)
            os.chown(target, 0, 70)
            os.chmod(target, 0o440)
            self.register_file(target, "repository_migration_fixture")
        os.chown(self.run_dir / "migrations", 0, 70)
        os.chmod(self.run_dir / "migrations", 0o550)
        if not (self.run_dir / "migrations" / "atlas.sum").is_file():
            raise DrillFailure("preflight", "preflight_failure")

    def preflight(self) -> tuple[str, str]:
        self.phase = "preflight"
        self.preflight_step = "privileges and environment"
        if os.geteuid() != 0 or not validate_ambient_environment(dict(os.environ)):
            raise DrillFailure(self.phase, "preflight_failure")
        self.preflight_step = "compiled build inputs"
        self.validate_build_inputs()
        self.preflight_step = "Docker CLI process inventory"
        baseline_docker_pids = docker_cli_pids()
        self.preflight_step = "/run tmpfs"
        fs_type = subprocess.run(
            ["stat", "-f", "-c", "%T", "/run"],
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            env={"PATH": "/usr/bin:/bin", "LC_ALL": "C"},
            timeout=5,
            check=False,
        )
        if fs_type.returncode != 0 or fs_type.stdout.decode("ascii", "ignore").strip() != "tmpfs":
            raise DrillFailure(self.phase, "preflight_failure")
        self.preflight_step = "root Docker configuration"
        config_path = Path("/root/.docker/config.json")
        if config_path.exists():
            try:
                config = json.loads(config_path.read_text(encoding="utf-8"))
            except (OSError, json.JSONDecodeError) as exc:
                raise DrillFailure(self.phase, "isolation_refusal") from exc
            if any(config.get(key) for key in ("auths", "credsStore", "credHelpers")):
                raise DrillFailure(self.phase, "isolation_refusal")
        self.preflight_step = "default Docker context"
        context = self.call(["context", "show"], phase=self.phase, category="isolation_refusal").decode().strip()
        if context != "default":
            raise DrillFailure(self.phase, "isolation_refusal")
        self.preflight_step = "Docker daemon security options"
        security = self.call(
            ["info", "--format", "{{json .SecurityOptions}}"],
            phase=self.phase,
            category="isolation_refusal",
        )
        try:
            security_options = json.loads(security)
        except json.JSONDecodeError as exc:
            raise DrillFailure(self.phase, "isolation_refusal") from exc
        if not isinstance(security_options, list) or any(
            not isinstance(item, str) or "rootless" in item.lower() or "userns" in item.lower()
            for item in security_options
        ):
            raise DrillFailure(self.phase, "isolation_refusal")
        self.preflight_step = "default Docker endpoint"
        endpoint = self.call(
            ["context", "inspect", "default", "--format", "{{json .Endpoints.docker}}"],
            phase=self.phase,
            category="isolation_refusal",
        )
        try:
            endpoint_value = json.loads(endpoint)
        except json.JSONDecodeError as exc:
            raise DrillFailure(self.phase, "isolation_refusal") from exc
        if not isinstance(endpoint_value, dict) or endpoint_value.get("Host") != "unix:///var/run/docker.sock":
            raise DrillFailure(self.phase, "isolation_refusal")
        self.preflight_step = "Docker bridge gateway"
        gateway = self.call(
            ["network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}"],
            phase=self.phase,
            category="isolation_refusal",
        ).decode("ascii", "strict").strip()
        try:
            ipaddress.IPv4Address(gateway)
        except ipaddress.AddressValueError as exc:
            raise DrillFailure(self.phase, "isolation_refusal") from exc
        self.preflight_step = "baseline Docker resource inventory"
        docker_inventory = self.docker_resource_inventory(self.phase)
        inventory_size = len(json.dumps(docker_inventory, sort_keys=True, separators=(",", ":")).encode("utf-8"))
        if inventory_size > MAX_FOREIGN_INVENTORY:
            raise DrillFailure(self.phase, "isolation_refusal")
        self.manifest["baseline"] = {
            "containers": sorted(
                [identifier, record["name"]]
                for identifier, record in docker_inventory["containers"].items()
            ),
            "volumes": sorted(docker_inventory["volumes"]),
            "networks": sorted(record["name"] for record in docker_inventory["networks"].values()),
            "docker_processes": baseline_docker_pids,
            "docker_inventory": docker_inventory,
        }
        self.preflight_step = "save baseline Docker inventory"
        try:
            self.save()
        except DrillFailure as exc:
            print(
                f"restore readiness baseline Docker inventory save failed: category={exc.category}",
                file=sys.stderr,
            )
            raise
        except (OSError, TypeError, ValueError) as exc:
            print(
                "restore readiness baseline Docker inventory save failed: "
                f"{type(exc).__name__}: {exc}",
                file=sys.stderr,
            )
            raise DrillFailure(self.phase, "preflight_failure") from exc
        self.preflight_step = "check Docker resource ownership identities"
        require_no_docker_identity_collisions(docker_inventory, self.manifest["token"], self.phase)
        project_name = f"restore-readiness-{self.manifest['token'][:16]}"
        for label in (IMAGE_TOKEN_LABEL, COMPOSE_PROJECT_LABEL):
            for value in {self.manifest["token"], project_name}:
                found = self.call(
                    ["image", "ls", "--quiet", "--no-trunc", "--filter", f"label={label}={value}"],
                    phase=self.phase,
                    category="isolation_refusal",
                )
                if found.strip():
                    raise DrillFailure(self.phase, "isolation_refusal")
        expected_tags = {
            f"local/restore-readiness-app:{self.manifest['token']}",
            f"local/restore-readiness-tools:{self.manifest['token']}",
        }
        if expected_tags & {name for _, name in self.list_images(self.phase)}:
            raise DrillFailure(self.phase, "isolation_refusal")
        return gateway, self.args.go_arch

    def validate_build_inputs(self) -> None:
        directory = Path(self.args.build_dir)
        try:
            info = directory.lstat()
        except OSError as exc:
            raise DrillFailure(self.phase, "preflight_failure") from exc
        if (
            directory.is_symlink()
            or not stat.S_ISDIR(info.st_mode)
            or info.st_uid != 0
            or stat.S_IMODE(info.st_mode) != 0o500
            or self.prepared_runtime is None
            or info.st_dev != self.prepared_runtime["device"]
            or info.st_ino != self.prepared_runtime["inode"]
        ):
            raise DrillFailure(self.phase, "preflight_failure")
        expected = PREPARED_ARTIFACTS | {PREPARED_ATTESTATION}
        if Path(self.args.atlas_bin) != directory / "atlas":
            raise DrillFailure(self.phase, "preflight_failure")
        try:
            if {item.name for item in directory.iterdir()} != expected:
                raise DrillFailure(self.phase, "preflight_failure")
        except OSError as exc:
            raise DrillFailure(self.phase, "preflight_failure") from exc
        for name in expected:
            try:
                item = (directory / name).lstat()
            except OSError as exc:
                raise DrillFailure(self.phase, "preflight_failure") from exc
            if (
                not stat.S_ISREG(item.st_mode)
                or item.st_uid != 0
                or item.st_nlink != 1
                or stat.S_IMODE(item.st_mode) != (0o400 if name == PREPARED_ATTESTATION else 0o500)
                or (name != PREPARED_ATTESTATION and not item.st_mode & 0o111)
                or (name != PREPARED_ATTESTATION and item.st_size <= 0)
            ):
                raise DrillFailure(self.phase, "preflight_failure")
        self.manifest["prepared_runtime"] = self.prepared_runtime
        self.save()

    def verify_foreign_inventory(self, phase: str = "cleanup") -> bool:
        baseline = self.manifest.get("baseline", {}).get("docker_inventory")
        if not isinstance(baseline, dict):
            if self.manifest.get("resource_creation_started"):
                self.manifest["foreign_inventory_unverified"] = True
                return False
            return True
        try:
            current = self.docker_resource_inventory(phase)
            owned_container_ids = {
                record.get("id") for record in self.manifest.get("containers", []) if record.get("id")
            }
            pending = self.manifest.get("pending_container") or {}
            if pending.get("id"):
                owned_container_ids.add(pending["id"])
            volume_records = self.manifest.get("containers", []) + ([pending] if pending else [])
            owned_volume_names = manifest_owned_volume_names(volume_records, current)
            owned_volume_names.intersection_update(current.get("volumes", {}))
            owned_volume_names.difference_update(baseline.get("volumes", {}))
            owned_network_ids = {
                record.get("id") for record in self.manifest.get("networks", []) if record.get("id")
            }
            foreign = foreign_resource_inventory(
                current,
                owned_container_ids=owned_container_ids,
                owned_volume_names=owned_volume_names,
                owned_network_ids=owned_network_ids,
            )
        except (DrillFailure, KeyError, TypeError, ValueError) as exc:
            self.manifest["foreign_inventory_unverified"] = True
            self.manifest["foreign_inventory_diagnostic"] = f"unverified:{type(exc).__name__}"
            print(
                f"restore readiness foreign Docker inventory could not be verified during {phase}: "
                f"{type(exc).__name__}",
                file=sys.stderr,
            )
            return False
        if foreign != baseline:
            self.manifest["foreign_inventory_changed"] = True
            summary = foreign_inventory_change_summary(baseline, foreign)
            self.manifest["foreign_inventory_diagnostic"] = f"changed:{summary}"
            print(
                "restore readiness foreign Docker inventory changed: "
                + summary,
                file=sys.stderr,
            )
            return False
        return True

    def preload(self) -> None:
        self.preflight_step = "preload source database image"
        self.images["source"] = {"id": self.preload_image(SOURCE_IMAGE), "reference": SOURCE_IMAGE}
        self.preflight_step = "preload restore database image"
        self.images["target"] = {"id": self.preload_image(RESTORE_IMAGE), "reference": RESTORE_IMAGE}

    def image_environment(self, image_id: str) -> tuple[dict[str, str], dict[str, Any]]:
        document = self.image_inspect(image_id, self.phase)
        config = document.get("Config") or {}
        try:
            return env_map(config.get("Env")), config
        except ValueError as exc:
            raise DrillFailure(self.phase, "isolation_refusal") from exc

    def build_images(self) -> None:
        self.preflight_step = "build drill images"
        self.phase = "fixture_generation"
        self.fixture_check = "build_images"
        build_root = self.run_dir / "work" / "build"
        app_context, tools_context = create_image_build_contexts(build_root)
        artifacts = {
            "teagramd": Path(self.args.build_dir) / "teagramd",
            "teagramd_seed": Path(self.args.build_dir) / "teagramd-seed",
            "launcher": Path(self.args.build_dir) / "restore-readiness-launcher",
            "seed": Path(self.args.build_dir) / "restore-readiness-seed",
            "seed_shim": Path(self.args.build_dir) / "restore-readiness-seed-shim",
            "provisioner": Path(self.args.build_dir) / "restore-readiness-provisioner",
            "checker": Path(self.args.build_dir) / "restore-drill-checker",
            "probe": Path(self.args.build_dir) / "restore-readiness-probe",
        }
        for name in ("teagramd", "launcher"):
            source = artifacts[name]
            ensure_executable_file(source)
            target = app_context / name
            shutil.copyfile(source, target, follow_symlinks=False)
            os.chmod(target, 0o555)
            self.register_file(target, f"app_image_input_{name}")
        for name in ("teagramd", "teagramd_seed", "seed", "seed_shim", "provisioner", "checker", "probe"):
            source = artifacts[name]
            ensure_executable_file(source)
            target = tools_context / name
            shutil.copyfile(source, target, follow_symlinks=False)
            os.chmod(target, 0o555)
            self.register_file(target, f"tools_image_input_{name}")
        atlas_binary = Path(self.args.atlas_bin)
        ensure_executable_file(atlas_binary)
        shutil.copyfile(atlas_binary, tools_context / "atlas", follow_symlinks=False)
        os.chmod(tools_context / "atlas", 0o555)
        self.register_file(tools_context / "atlas", "tools_image_input_atlas")
        empty = app_context / "empty"
        private_file(empty, b"", 0o444)
        self.register_file(empty, "app_image_input_empty")
        private_file(tools_context / "empty", b"", 0o444)
        self.register_file(tools_context / "empty", "tools_image_input_empty")
        # Atlas reads the DSN from the protected env file only through a project config.
        private_file(tools_context / "atlas.hcl", b'env "drill" {\n  url = getenv("DATABASE_URL")\n}\n', 0o444)
        self.register_file(tools_context / "atlas.hcl", "tools_image_input_atlas_config")
        (app_context / "Dockerfile").write_text(
            "FROM scratch\n"
            "COPY teagramd /run/app/teagramd\n"
            "COPY launcher /run/app/restore-readiness-launcher\n"
            "COPY empty /run/postgres/.keep\n"
            "COPY empty /run/invocation/.keep\n"
            "COPY empty /run/secrets/.keep\n"
            "COPY empty /run/blobs/.keep\n"
            "USER 10001:70\n"
            "ENTRYPOINT [\"/run/app/restore-readiness-launcher\"]\n",
            encoding="utf-8",
        )
        (tools_context / "Dockerfile").write_text(
            "FROM scratch\n"
            "COPY teagramd /run/app/teagramd\n"
            "COPY teagramd_seed /run/app/teagramd-seed\n"
            "COPY seed /run/app/restore-readiness-seed\n"
            "COPY seed_shim /run/app/restore-readiness-seed-shim\n"
            "COPY provisioner /run/app/restore-readiness-provisioner\n"
            "COPY checker /run/app/restore-drill-checker\n"
            "COPY probe /run/app/restore-readiness-probe\n"
            "COPY atlas /run/app/atlas\n"
            "COPY atlas.hcl /run/app/atlas.hcl\n"
            "COPY empty /run/postgres/.keep\n"
            "COPY empty /run/invocation/.keep\n"
            "COPY empty /run/secrets/.keep\n"
            "USER 65532:65532\n"
            "ENTRYPOINT [\"/run/app/restore-readiness-probe\"]\n",
            encoding="utf-8",
        )
        for context in (app_context, tools_context):
            os.chmod(context / "Dockerfile", 0o400)
            self.register_file(context / "Dockerfile", "scratch_image_build_instruction")
        self.images["app"] = self.build_image(
            "app",
            app_context,
            f"local/restore-readiness-app:{self.manifest['token']}",
            "10001:70",
        )
        self.images["tools"] = self.build_image(
            "tools",
            tools_context,
            f"local/restore-readiness-tools:{self.manifest['token']}",
            "65532:65532",
        )
        atlas_version = self.image_container(
            "tools",
            role="atlas_version",
            user="65532:65532",
            memory=CLIENT_MEMORY,
            pids_limit=32,
            mounts=[],
            tmpfs_options={},
            command_args=["version"],
            entrypoint="/run/app/atlas",
        )
        output = self.utility(atlas_version, "fixture_generation", "fixture_failure")
        if output.decode("utf-8", "ignore").splitlines()[:1] != ["atlas version v1.2.0"]:
            raise DrillFailure("fixture_generation", "fixture_failure")

    def inspect_container(self, identifier: str, phase: str) -> dict[str, Any]:
        output = self.call(["inspect", "--type", "container", identifier], phase=phase, category="isolation_refusal")
        try:
            document = json.loads(output)
            if not isinstance(document, list) or len(document) != 1 or not isinstance(document[0], dict):
                raise ValueError("invalid inspect response")
            return document[0]
        except (json.JSONDecodeError, ValueError) as exc:
            raise DrillFailure(phase, "isolation_refusal") from exc

    def list_containers(self, phase: str) -> list[tuple[str, str]]:
        output = self.call(
            ["ps", "--all", "--no-trunc", "--format", "{{.ID}} {{.Names}}"],
            phase=phase,
            category="cleanup_failure",
        )
        result: list[tuple[str, str]] = []
        for line in output.decode("utf-8", "strict").splitlines():
            parts = line.split(" ", 1)
            if len(parts) != 2:
                raise DrillFailure(phase, "cleanup_failure")
            result.append((parts[0], parts[1]))
        return result

    def list_volumes(self, phase: str) -> set[str]:
        output = self.call(["volume", "ls", "--quiet"], phase=phase, category="cleanup_failure")
        return {line for line in output.decode("utf-8", "strict").splitlines() if line}

    def list_networks(self, phase: str) -> set[str]:
        output = self.call(
            ["network", "ls", "--format", "{{.Name}}"],
            phase=phase,
            category="isolation_refusal" if phase != "cleanup" else "cleanup_failure",
        )
        return {line for line in output.decode("utf-8", "strict").splitlines() if line}

    def docker_resource_inventory(self, phase: str) -> dict[str, Any]:
        def invoke(args: list[str]) -> bytes:
            if phase == "cleanup":
                status, output = self.cleanup_call(args, timeout=20)
                if status != 0:
                    raise DrillFailure("cleanup", "cleanup_failure")
                return output
            return self.call(args, phase=phase, category="isolation_refusal", timeout=20)

        try:
            return collect_docker_resource_inventory(invoke, self.manifest["token"])
        except (KeyError, TypeError, UnicodeDecodeError, ValueError) as exc:
            raise DrillFailure(phase, "isolation_refusal") from exc

    def _record_by_name(self, name: str) -> dict[str, Any] | None:
        return next((item for item in self.manifest["containers"] if item["name"] == name), None)

    def _image_by_tag(self, tag: str) -> dict[str, Any] | None:
        return next((item for item in self.manifest["images"] if item["tag"] == tag), None)

    def attest_container(self, record: dict[str, Any], phase: str) -> dict[str, Any]:
        if not record.get("id") or not re.fullmatch(r"[0-9a-f]{64}", record["id"]):
            raise DrillFailure(phase, "isolation_refusal")
        document = self.inspect_container(record["id"], phase)
        if not validate_container_inspect(document, record):
            raise DrillFailure(phase, "isolation_refusal")
        return document

    def create_container(
        self,
        *,
        role: str,
        image_id: str,
        name: str,
        user: str,
        memory: int,
        pids_limit: int,
        mounts: list[dict[str, Any]],
        tmpfs_options: dict[str, str],
        environment: dict[str, str],
        explicit_environment: dict[str, str],
        command_args: list[str],
        expected_entrypoint: list[str],
        expected_command: list[str],
        network_mode: str = "none",
        extra_hosts: list[str] | None = None,
        entrypoint: str | None = None,
        interactive: bool = False,
        env_file: Path | None = None,
        phase: str,
        fixture_check: str | None,
    ) -> dict[str, Any]:
        owned_container_ids = {
            record.get("id") for record in self.manifest.get("containers", []) if record.get("id")
        }
        if network_mode != "none":
            if not network_mode.startswith("container:") or network_mode.removeprefix("container:") not in owned_container_ids:
                raise DrillFailure(phase, "isolation_refusal", fixture_check=fixture_check)
        if any(mount.get("type") != "bind" for mount in mounts):
            raise DrillFailure(phase, "isolation_refusal", fixture_check=fixture_check)
        existing = self.list_containers(phase)
        if any(found_name == name for _, found_name in existing):
            raise DrillFailure(phase, "isolation_refusal", fixture_check=fixture_check)
        baseline_volumes = self.list_volumes(phase)
        baseline_networks = self.list_networks(phase)
        record = {
            "name": name,
            "role": role,
            "token": self.manifest["token"],
            "id": "",
            "image_id": image_id,
            "user": user,
            "network_mode": network_mode,
            "extra_hosts": sorted(extra_hosts or []),
            "memory": memory,
            "pids_limit": pids_limit,
            "shm_size": 16 * 1024 * 1024,
            "cpu_quota": 100000,
            "cpu_period": 100000,
            "mounts": mounts,
            "tmpfs": sorted(tmpfs_options),
            "open_stdin": interactive,
            "environment_sha256": environment_digest(environment),
            "entrypoint": expected_entrypoint,
            "command": expected_command,
            "tmpfs_options": tmpfs_options,
            "volumes_before": sorted(baseline_volumes),
            "networks_before": sorted(baseline_networks),
            "containers_before": [
                {"id": identifier, "name": found_name} for identifier, found_name in existing
            ],
            "unexpected_volumes": [],
            "unexpected_volume_mounts": [],
        }
        self.manifest["pending_container"] = record
        self.save()
        args = [
            "create",
            "--name",
            name,
            "--label",
            f"{IMAGE_TOKEN_LABEL}={self.manifest['token']}",
            "--label",
            f"{IMAGE_ROLE_LABEL}={role}",
            "--network",
            network_mode,
            "--read-only",
            "--cap-drop",
            "ALL",
            "--security-opt",
            "no-new-privileges:true",
            "--ipc",
            "private",
            "--cgroupns",
            "private",
            "--runtime",
            "runc",
            "--shm-size",
            "16m",
            "--pids-limit",
            str(pids_limit),
            "--memory",
            str(memory),
            "--memory-swap",
            str(memory),
            "--cpu-period",
            "100000",
            "--cpu-quota",
            "100000",
            "--ulimit",
            "core=0:0",
            "--log-driver",
            "none",
            "--user",
            user,
        ]
        for destination, options in sorted(tmpfs_options.items()):
            args.extend(["--tmpfs", f"{destination}:{options}"])
        for item in sorted(extra_hosts or []):
            args.extend(["--add-host", item])
        for mount in mounts:
            if mount["type"] != "bind":
                continue
            spec = f"type=bind,source={mount['source']},target={mount['destination']}"
            if not mount["rw"]:
                spec += ",readonly"
            args.extend(["--mount", spec])
        if env_file is not None:
            args.extend(["--env-file", str(env_file)])
        for key, value in sorted(explicit_environment.items()):
            args.extend(["--env", f"{key}={value}"])
        if interactive:
            args.append("--interactive")
        if entrypoint:
            args.extend(["--entrypoint", entrypoint])
        args.append(image_id)
        args.extend(command_args)
        try:
            output = self.call(args, phase=phase, category="fixture_failure")
        except DrillFailure as exc:
            if exc.fixture_check is None and fixture_check is not None:
                exc.fixture_check = fixture_check
            raise
        identifier = output.decode("ascii", "strict").strip()
        if not re.fullmatch(r"[0-9a-f]{64}", identifier):
            raise DrillFailure(phase, "fixture_failure", fixture_check=fixture_check)
        record["id"] = identifier
        self.manifest["containers"].append(record)
        self.manifest["pending_container"] = None
        self.save()
        document = self.inspect_container(record["id"], phase)
        volumes_after = self.list_volumes(phase)
        baseline = set(record["volumes_before"])
        created_volumes = volumes_after - baseline
        observed_volume_mounts = sorted(
            (
                {
                    "type": "volume",
                    "name": mount.get("Name"),
                    "source": mount.get("Source"),
                    "destination": mount.get("Destination"),
                    "rw": bool(mount.get("RW")),
                }
                for mount in document.get("Mounts", [])
                if mount.get("Type") == "volume"
            ),
            key=lambda item: item["destination"],
        )
        attached_created_volumes = {
            mount["name"] for mount in observed_volume_mounts if mount["name"] in created_volumes
        }
        record["unexpected_volumes"] = sorted(attached_created_volumes)
        record["unexpected_volume_mounts"] = [
            mount for mount in observed_volume_mounts if mount["name"] in attached_created_volumes
        ]
        record["unexpected_volume_records"] = [
            volume for name in record["unexpected_volumes"]
            if (volume := self.inspect_volume(name, phase)) is not None
        ]
        networks_after = self.list_networks(phase)
        self.save()
        actual_volume_mounts = [mount for mount in document.get("Mounts", []) if mount.get("Type") == "volume"]
        if (
            not validate_container_inspect(document, record)
            or actual_volume_mounts
            or created_volumes
            or len(record["unexpected_volume_records"]) != len(attached_created_volumes)
            or networks_after != baseline_networks
        ):
            raise DrillFailure(phase, "isolation_refusal", fixture_check=fixture_check)
        return record

    def start_container(self, record: dict[str, Any], phase: str, category: str) -> None:
        self.attest_container(record, phase)
        self.call(["start", record["id"]], phase=phase, category=category)
        self.attest_container(record, phase)

    def exec_container(self, record: dict[str, Any], args: list[str], phase: str, category: str) -> bytes:
        self.attest_container(record, phase)
        return self.call(["exec", record["id"], *args], phase=phase, category=category)

    def copy_to_container(self, record: dict[str, Any], source: Path, target: str, phase: str) -> None:
        self.attest_container(record, phase)
        self.call(["cp", "--archive", str(source), f"{record['id']}:{target}"], phase=phase, category="fixture_failure")

    def attached(
        self,
        record: dict[str, Any],
        phase: str,
        category: str,
        stdin: Any = None,
        input_bytes: bytes | None = None,
        interrupt_signal: int | None = None,
        interrupt_ack: bytes | None = None,
        accept_nonzero: bool = False,
        capture_stderr: bool = False,
        timeout: float = 600,
        max_bytes: int = MAX_CAPTURE,
    ) -> bytes:
        self.attest_container(record, phase)
        self.last_attached_status: int | None = None
        self.last_attached_stderr = b""
        self.last_attached_interrupt_acknowledged = False
        process: subprocess.Popen[bytes] | None = None
        tracked: dict[str, int] | None = None
        interrupt_finished = threading.Event()
        interrupt_thread: threading.Thread | None = None
        stdout_capture: Any = None
        stderr_capture: Any = None
        try:
            if interrupt_signal is None and interrupt_ack is not None:
                raise DrillFailure(phase, "preflight_failure")
            if interrupt_signal is not None:
                if interrupt_signal not in {signal.SIGINT, signal.SIGTERM} or not interrupt_ack:
                    raise DrillFailure(phase, "preflight_failure")
                stdout_capture = tempfile.TemporaryFile()
                stderr_capture = tempfile.TemporaryFile()
            process = subprocess.Popen(
                [self.executable, "start", "--attach", "--interactive", record["id"]],
                stdin=subprocess.PIPE if input_bytes is not None else stdin if stdin is not None else subprocess.DEVNULL,
                stdout=stdout_capture if stdout_capture is not None else subprocess.PIPE,
                stderr=(
                    stderr_capture
                    if stderr_capture is not None
                    else subprocess.PIPE if capture_stderr else subprocess.DEVNULL
                ),
                env=self.environment,
            )
            tracked = self._track_child(process)
            if interrupt_signal is not None:
                def interrupt_after_ack() -> None:
                    assert stdout_capture is not None and interrupt_ack is not None
                    while not interrupt_finished.wait(0.02):
                        try:
                            output = os.pread(stdout_capture.fileno(), max_bytes + 1, 0)
                        except OSError:
                            return
                        if interrupt_ack in output:
                            self.last_attached_interrupt_acknowledged = True
                            try:
                                os.kill(os.getpid(), interrupt_signal)
                            except OSError:
                                return
                            return
                        if process is not None and process.poll() is not None:
                            return

                interrupt_thread = threading.Thread(target=interrupt_after_ack, daemon=True)
                interrupt_thread.start()
            try:
                output, error = process.communicate(
                    input=input_bytes,
                    timeout=max(1, min(timeout, self.manifest["work_deadline"] - time.monotonic()))
                )
            except subprocess.TimeoutExpired as exc:
                if not self._terminate_and_reap(process):
                    raise DrillFailure("cleanup", "cleanup_failure") from exc
                raise DrillFailure(phase, "timeout") from exc
            if stdout_capture is not None:
                output = os.pread(stdout_capture.fileno(), max_bytes + 1, 0)
                error = os.pread(stderr_capture.fileno(), max_bytes + 1, 0) if stderr_capture is not None else b""
            output = output or b""
            error = error or b""
            self.last_attached_status = int(process.returncode or 0)
            self.last_attached_stderr = error
            if (process.returncode != 0 and not accept_nonzero) or len(output) > max_bytes or len(error) > max_bytes:
                raise DrillFailure(phase, category)
            return output
        except subprocess.TimeoutExpired as exc:
            raise DrillFailure(phase, "timeout") from exc
        except OSError as exc:
            raise DrillFailure(phase, category) from exc
        finally:
            interrupt_finished.set()
            if interrupt_thread is not None:
                interrupt_thread.join(timeout=1)
            if process is not None:
                reaped = process.poll() is not None or self._terminate_and_reap(process)
                if tracked is not None and reaped:
                    self._untrack_child(tracked)
            if stdout_capture is not None:
                stdout_capture.close()
            if stderr_capture is not None:
                stderr_capture.close()

    def attached_status(
        self,
        record: dict[str, Any],
        phase: str,
        category: str,
        timeout: float = 180,
        max_bytes: int = MAX_CAPTURE,
    ) -> tuple[int, bytes]:
        self.attest_container(record, phase)
        process: subprocess.Popen[bytes] | None = None
        tracked: dict[str, int] | None = None
        try:
            process = subprocess.Popen(
                [self.executable, "start", "--attach", "--interactive", record["id"]],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                env=self.environment,
            )
            tracked = self._track_child(process)
            try:
                output, _ = process.communicate(
                    timeout=max(1, min(timeout, self.manifest["work_deadline"] - time.monotonic()))
                )
            except subprocess.TimeoutExpired as exc:
                if not self._terminate_and_reap(process):
                    raise DrillFailure("cleanup", "cleanup_failure") from exc
                raise DrillFailure(phase, "timeout") from exc
            if len(output) > max_bytes:
                raise DrillFailure(phase, category)
            return int(process.returncode or 0), output
        except subprocess.TimeoutExpired as exc:
            raise DrillFailure(phase, "timeout") from exc
        except OSError as exc:
            raise DrillFailure(phase, category) from exc
        finally:
            if process is not None:
                reaped = process.poll() is not None or self._terminate_and_reap(process)
                if tracked is not None and reaped:
                    self._untrack_child(tracked)

    def stream_containers(
        self,
        producer: dict[str, Any] | None,
        consumer: dict[str, Any],
        *,
        phase: str,
        category: str,
        max_bytes: int = 256 * 1024 * 1024,
        consumer_input: bytes | None = None,
    ) -> None:
        if (producer is None) == (consumer_input is None):
            raise DrillFailure(phase, category)
        if producer is not None:
            self.attest_container(producer, phase)
        self.attest_container(consumer, phase)
        source: subprocess.Popen[bytes] | None = None
        target: subprocess.Popen[bytes] | None = None
        source_record: dict[str, int] | None = None
        target_record: dict[str, int] | None = None
        transferred = 0
        try:
            if producer is not None:
                source = subprocess.Popen(
                    [self.executable, "start", "--attach", "--interactive", producer["id"]],
                    stdin=subprocess.DEVNULL,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.DEVNULL,
                    env=self.environment,
                )
                source_record = self._track_child(source)
            target = subprocess.Popen(
                [self.executable, "start", "--attach", "--interactive", consumer["id"]],
                stdin=subprocess.PIPE,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                env=self.environment,
            )
            target_record = self._track_child(target)
            if target.stdin is None:
                raise DrillFailure(phase, category)
            if consumer_input is not None:
                if len(consumer_input) > max_bytes:
                    raise DrillFailure(phase, category)
                target.stdin.write(consumer_input)
                target.stdin.close()
                remaining = self.manifest["work_deadline"] - time.monotonic()
                if remaining <= 0:
                    raise DrillFailure(phase, "timeout")
                if target.wait(timeout=min(600, remaining)) != 0:
                    raise DrillFailure(phase, category)
                return
            if source is None or source.stdout is None:
                raise DrillFailure(phase, category)
            while True:
                if time.monotonic() >= self.manifest["work_deadline"] or self.interrupted:
                    raise DrillFailure(phase, "timeout" if not self.interrupted else "interrupted")
                readable, _, _ = select.select(
                    [source.stdout.fileno()], [], [], min(1, self.manifest["work_deadline"] - time.monotonic())
                )
                if not readable:
                    continue
                chunk = os.read(source.stdout.fileno(), 64 * 1024)
                if not chunk:
                    break
                transferred += len(chunk)
                if transferred > max_bytes:
                    raise DrillFailure(phase, category)
                target.stdin.write(chunk)
            target.stdin.close()
            remaining = max(1, self.manifest["work_deadline"] - time.monotonic())
            source_status = source.wait(timeout=min(60, remaining))
            remaining = self.manifest["work_deadline"] - time.monotonic()
            if remaining <= 0:
                raise DrillFailure(phase, "timeout")
            target_status = target.wait(timeout=min(600, remaining))
            if source_status != 0 or target_status != 0:
                raise DrillFailure(phase, category)
        except subprocess.TimeoutExpired as exc:
            raise DrillFailure(phase, "timeout") from exc
        except (BrokenPipeError, OSError) as exc:
            raise DrillFailure(phase, category) from exc
        finally:
            if source is not None:
                if source.stdout is not None:
                    source.stdout.close()
                if source.poll() is None:
                    source.terminate()
                    try:
                        source.wait(timeout=2)
                    except subprocess.TimeoutExpired:
                        source.kill()
                        source.wait()
            if target is not None:
                if target.stdin is not None and not target.stdin.closed:
                    target.stdin.close()
                if target.poll() is None:
                    target.terminate()
                    try:
                        target.wait(timeout=2)
                    except subprocess.TimeoutExpired:
                        target.kill()
                        target.wait()
            if source_record is not None:
                self._untrack_child(source_record)
            if target_record is not None:
                self._untrack_child(target_record)

    def list_images(self, phase: str) -> list[tuple[str, str]]:
        output = self.call(
            ["image", "ls", "--no-trunc", "--format", "{{.ID}} {{.Repository}}:{{.Tag}}"],
            phase=phase,
            category="cleanup_failure",
        )
        result: list[tuple[str, str]] = []
        for line in output.decode("utf-8", "strict").splitlines():
            parts = line.split(" ", 1)
            if len(parts) != 2:
                raise DrillFailure(phase, "cleanup_failure")
            result.append((parts[0], parts[1]))
        return result

    def image_inspect(self, identifier: str, phase: str) -> dict[str, Any]:
        output = self.call(["image", "inspect", identifier], phase=phase, category="isolation_refusal")
        try:
            documents = json.loads(output)
            if not isinstance(documents, list) or len(documents) != 1 or not isinstance(documents[0], dict):
                raise ValueError("invalid image response")
            return documents[0]
        except (json.JSONDecodeError, ValueError) as exc:
            raise DrillFailure(phase, "isolation_refusal") from exc

    def preload_image(self, reference: str) -> str:
        self.call(["pull", reference], phase="preflight", category="preflight_failure", timeout=180)
        document = self.image_inspect(reference, "preflight")
        if reference not in (document.get("RepoDigests") or []):
            print(
                "restore readiness preflight image validation failed: "
                f"reference={json.dumps(reference)} check=RepoDigests "
                f"observed={json.dumps(document.get('RepoDigests'), separators=(',', ':'))}",
                file=sys.stderr,
            )
            raise DrillFailure("preflight", "preflight_failure")
        if document.get("Os") != "linux" or document.get("Architecture") != self.args.go_arch:
            observed_platform = {"Os": document.get("Os"), "Architecture": document.get("Architecture")}
            print(
                "restore readiness preflight image validation failed: "
                f"reference={json.dumps(reference)} check=Os/Architecture "
                f"observed={json.dumps(observed_platform, separators=(',', ':'))}",
                file=sys.stderr,
            )
            raise DrillFailure("preflight", "preflight_failure")
        identifier = document.get("Id", "")
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", identifier):
            print(
                "restore readiness preflight image validation failed: "
                f"reference={json.dumps(reference)} check=Id "
                f"observed={json.dumps(identifier, separators=(',', ':'))}",
                file=sys.stderr,
            )
            raise DrillFailure("preflight", "preflight_failure")
        return identifier

    def build_image(self, role: str, context: Path, tag: str, expected_user: str) -> dict[str, Any]:
        existing_tags = {name for _, name in self.list_images("preflight")}
        if tag in existing_tags:
            raise DrillFailure("preflight", "isolation_refusal")
        self.manifest["pending_image"] = {
            "role": role,
            "tag": tag,
            "token": self.manifest["token"],
            "user": expected_user,
            "before": sorted(existing_tags),
        }
        self.save()
        dockerfile = context / "Dockerfile"
        output = self.call(
            [
                "build",
                "--quiet",
                "--network",
                "none",
                "--pull=false",
                "--label",
                f"{IMAGE_TOKEN_LABEL}={self.manifest['token']}",
                "--label",
                f"{IMAGE_ROLE_LABEL}={role}",
                "--tag",
                tag,
                "--file",
                str(dockerfile),
                str(context),
            ],
            phase="fixture_generation",
            category="fixture_failure",
            timeout=180,
        )
        identifier = output.decode("ascii", "strict").strip()
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", identifier):
            raise DrillFailure("fixture_generation", "fixture_failure")
        record = {"role": role, "tag": tag, "token": self.manifest["token"], "id": identifier, "user": expected_user}
        self.manifest["images"].append(record)
        self.manifest["pending_image"] = None
        self.save()
        self.attest_image(record, "fixture_generation")
        return record

    def attest_image(self, record: dict[str, Any], phase: str) -> dict[str, Any]:
        document = self.image_inspect(record["id"], phase)
        config = document.get("Config") or {}
        labels = config.get("Labels") or {}
        if (
            document.get("Id") != record["id"]
            or document.get("Os") != "linux"
            or document.get("Architecture") != self.args.go_arch
            or (document.get("RepoTags") or []) != [record["tag"]]
            or labels.get(IMAGE_TOKEN_LABEL) != record["token"]
            or labels.get(IMAGE_ROLE_LABEL) != record["role"]
            or config.get("User", "") != record["user"]
            or config.get("Volumes") not in (None, {})
        ):
            raise DrillFailure(phase, "isolation_refusal")
        return document

    def private_write(
        self,
        path: Path,
        content: bytes,
        *,
        mode: int,
        owner: tuple[int, int] = (0, 0),
        purpose: str,
    ) -> Path:
        private_file(path, content, mode, owner)
        self.register_file(path, purpose)
        return path

    def register_file(self, path: Path, purpose: str) -> None:
        info = path.lstat()
        self.manifest["files"].append(
            {
                "path": str(path),
                "purpose": purpose,
                "device": info.st_dev,
                "inode": info.st_ino,
                "mode": stat.S_IMODE(info.st_mode),
                "uid": info.st_uid,
                "gid": info.st_gid,
                "size": info.st_size,
            }
        )
        self.save()

    def private_read(self, path: Path, *, max_bytes: int = 8192) -> bytes:
        record = next((item for item in self.manifest.get("files", []) if item["path"] == str(path)), None)
        if record is None:
            raise DrillFailure(self.phase, "isolation_refusal")
        info = path.lstat()
        if (
            not stat.S_ISREG(info.st_mode)
            or info.st_dev != record["device"]
            or info.st_ino != record["inode"]
            or stat.S_IMODE(info.st_mode) != record["mode"]
            or info.st_uid != record["uid"]
            or info.st_gid != record["gid"]
            or info.st_nlink != 1
            or info.st_size > max_bytes
        ):
            raise DrillFailure(self.phase, "isolation_refusal")
        descriptor = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
        try:
            data = bytearray()
            while len(data) <= max_bytes:
                chunk = os.read(descriptor, min(4096, max_bytes + 1 - len(data)))
                if not chunk:
                    break
                data.extend(chunk)
            if len(data) > max_bytes:
                raise DrillFailure(self.phase, "fixture_failure")
            return bytes(data)
        finally:
            os.close(descriptor)

    def truncate_private(self, path: Path) -> None:
        record = next((item for item in self.manifest.get("files", []) if item["path"] == str(path)), None)
        if record is None:
            raise DrillFailure(self.phase, "isolation_refusal")
        info = path.lstat()
        if (
            not stat.S_ISREG(info.st_mode)
            or info.st_dev != record["device"]
            or info.st_ino != record["inode"]
            or info.st_uid != record["uid"]
            or info.st_gid != record["gid"]
            or info.st_nlink != 1
        ):
            raise DrillFailure(self.phase, "isolation_refusal")
        descriptor = os.open(path, os.O_WRONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
        try:
            os.ftruncate(descriptor, 0)
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
        record["size"] = 0
        self.save()

    def delete_private_file(self, path: Path) -> None:
        record = next((item for item in self.manifest.get("files", []) if item["path"] == str(path)), None)
        if record is None:
            return
        try:
            info = path.lstat()
        except FileNotFoundError:
            self.manifest["files"].remove(record)
            self.save()
            return
        except OSError as exc:
            raise DrillFailure(self.phase, "cleanup_failure") from exc
        if (
            not stat.S_ISREG(info.st_mode)
            or info.st_dev != record["device"]
            or info.st_ino != record["inode"]
            or stat.S_IMODE(info.st_mode) != record["mode"]
            or info.st_uid != record["uid"]
            or info.st_gid != record["gid"]
            or info.st_nlink != 1
        ):
            raise DrillFailure(self.phase, "isolation_refusal")
        try:
            os.unlink(path)
        except OSError as exc:
            raise DrillFailure(self.phase, "cleanup_failure") from exc
        self.manifest["files"].remove(record)
        self.save()

    def image_container(
        self,
        image: str,
        *,
        role: str,
        user: str,
        memory: int,
        pids_limit: int,
        mounts: list[dict[str, Any]],
        tmpfs_options: dict[str, str],
        command_args: list[str] | None = None,
        entrypoint: str | None = None,
        network_mode: str = "none",
        extra_hosts: list[str] | None = None,
        explicit_environment: dict[str, str] | None = None,
        env_file: Path | None = None,
        interactive: bool = False,
    ) -> dict[str, Any]:
        phase = self.phase
        fixture_check = self.fixture_check if phase == "fixture_generation" else None
        image_id = self.images[image]["id"] if image in self.images else image
        base_environment, image_config = self.image_environment(image_id)
        environment = dict(base_environment)
        if env_file is not None:
            for key, value in parse_env_file(self.private_read(env_file)):
                environment[key] = value
        explicit = explicit_environment or {}
        environment.update(explicit)
        effective_entrypoint = [entrypoint] if entrypoint else (image_config.get("Entrypoint") or [])
        effective_command = list(command_args) if command_args is not None else (image_config.get("Cmd") or [])
        return self.create_container(
            role=role,
            image_id=image_id,
            name=f"restore-readiness-{self.manifest['token'][:16]}-{role}",
            user=user,
            memory=memory,
            pids_limit=pids_limit,
            mounts=mounts,
            tmpfs_options=tmpfs_options,
            environment=environment,
            explicit_environment=explicit,
            command_args=effective_command if command_args is None else command_args,
            expected_entrypoint=effective_entrypoint,
            expected_command=effective_command,
            network_mode=network_mode,
            extra_hosts=extra_hosts,
            entrypoint=entrypoint,
            interactive=interactive,
            env_file=env_file,
            phase=phase,
            fixture_check=fixture_check,
        )

    def mount(self, source: Path, destination: str, *, writable: bool) -> dict[str, Any]:
        info = source.lstat()
        if source.is_symlink() or not (stat.S_ISREG(info.st_mode) or stat.S_ISDIR(info.st_mode)):
            raise DrillFailure(self.phase, "isolation_refusal")
        resolved = source.resolve(strict=True)
        if resolved != source:
            raise DrillFailure(self.phase, "isolation_refusal")
        return {
            "type": "bind",
            "source": str(source),
            "destination": destination,
            "rw": writable,
        }

    def utility(self, record: dict[str, Any], phase: str, category: str, timeout: float = 600) -> bytes:
        output = self.attached(record, phase, category, timeout=timeout)
        document = self.attest_container(record, phase)
        state = document.get("State") or {}
        if state.get("Running") or int(state.get("ExitCode", -1)) != 0:
            raise DrillFailure(phase, category)
        self.discard_container(record, phase, category)
        return output

    def discard_container(self, record: dict[str, Any], phase: str, category: str) -> None:
        document = self.attest_container(record, phase)
        if (document.get("State") or {}).get("Running"):
            raise DrillFailure(phase, category)
        self.call(["rm", record["id"]], phase=phase, category=category)
        remaining = self.list_containers(phase)
        if any(identifier == record["id"] or name == record["name"] for identifier, name in remaining):
            raise DrillFailure(phase, category)
        record["removed"] = True
        self.save()

    def sql(
        self,
        database: dict[str, Any],
        statement: str,
        phase: str,
        *,
        allow_interrupted: bool = False,
        database_user: str = "postgres",
        pinned_search_path: bool = False,
    ) -> str:
        if not re.fullmatch(r"[a-z][a-z0-9_]{0,63}", database_user):
            raise DrillFailure(phase, "isolation_refusal")
        command = ["exec", "--interactive", "--user", "70:70"]
        if pinned_search_path:
            command.extend(("--env", "PGOPTIONS=-c search_path=pg_catalog"))
        command.extend(
            (
                database["id"],
                "psql",
                "-X",
                "-qAt",
                "-v",
                "ON_ERROR_STOP=1",
                "-F",
                "|",
                "-U",
                database_user,
                "-d",
                "teagram",
            )
        )
        output = self.call(
            command,
            phase=phase,
            category="schema_failure",
            input_bytes=statement.encode("utf-8"),
            timeout=60,
            allow_interrupted=allow_interrupted,
        )
        try:
            return output.decode("ascii", "strict").strip()
        except UnicodeDecodeError as exc:
            raise DrillFailure(phase, "schema_failure") from exc

    def wait_database(self, database: dict[str, Any], phase: str) -> None:
        deadline = min(self.manifest["work_deadline"], time.monotonic() + 120)
        while time.monotonic() < deadline:
            document = self.attest_container(database, phase)
            if not (document.get("State") or {}).get("Running"):
                raise DrillFailure(phase, "fixture_failure")
            status, _ = self.call_status(
                [
                    "exec",
                    "--user",
                    "70:70",
                    database["id"],
                    "pg_isready",
                    # Only the final server listens on TCP; the entrypoint's init server shares the socket.
                    "-h",
                    "127.0.0.1",
                    "-U",
                    RESTORE_BOOTSTRAP_ROLE,
                    "-d",
                    "teagram",
                ],
                phase=phase,
                category="fixture_failure",
                timeout=10,
            )
            if status == 0:
                return
            if self.interrupted:
                raise DrillFailure(phase, "interrupted")
            time.sleep(0.5)
        raise DrillFailure(phase, "timeout")

    def create_database(self, name: str) -> dict[str, Any]:
        self.phase = "fixture_generation" if name == "source" else "restore"
        if self.phase == "fixture_generation":
            self.fixture_check = "create_source_database"
        image = self.images[name]
        secret = self.files[f"{name}_init_password"]
        env_path = self.files[f"{name}_database_env"]
        socket_path = self.sockets[name]
        env_values = {
            "PGDATA": "/var/lib/postgresql/18/docker",
            "POSTGRES_DB": "teagram",
            "POSTGRES_USER": RESTORE_BOOTSTRAP_ROLE,
            "POSTGRES_PASSWORD_FILE": "/run/secrets/init-password",
            "POSTGRES_HOST_AUTH_METHOD": "scram-sha-256",
        }
        env_content = ("\n".join(f"{key}={value}" for key, value in env_values.items()) + "\n").encode()
        if not env_path.exists():
            env_path = self.private_write(
                env_path,
                env_content,
                mode=0o600,
                purpose=f"{name}_database_env",
            )
        secret_mount = self.mount(secret, "/run/secrets/init-password", writable=False)
        socket_mount = self.mount(socket_path, "/run/postgres", writable=True)
        mounts = [secret_mount, socket_mount]
        listen_address = "127.0.0.1"
        # The image entrypoint initialises through the default socket directory, kept private on tmpfs.
        tmpfs_options = {
            "/run/postgresql": "rw,nosuid,nodev,noexec,size=1m,uid=70,gid=70,mode=0700",
            "/tmp": "rw,nosuid,nodev,noexec,size=64m,uid=70,gid=70,mode=1777",
            "/var/lib/postgresql": "rw,nosuid,nodev,noexec,size=1200m,uid=70,gid=70,mode=0700",
        }
        if name == "source":
            # PG16 declares its data directory as a volume; PG18 refuses a mount there.
            tmpfs_options["/var/lib/postgresql/data"] = "rw,nosuid,nodev,noexec,size=16m,uid=70,gid=70,mode=0700"
        record = self.image_container(
            name,
            role=f"{name}_postgres",
            user="70:70",
            memory=PG_MEMORY,
            pids_limit=128,
            mounts=mounts,
            tmpfs_options=tmpfs_options,
            command_args=[
                "postgres",
                "-c",
                f"listen_addresses={listen_address}",
                "-c",
                "unix_socket_directories=/run/postgres,/run/postgresql",
                "-c",
                "max_connections=20",
                "-c",
                "shared_buffers=128MB",
                "-c",
                "work_mem=8MB",
                "-c",
                "maintenance_work_mem=64MB",
                "-c",
                "shared_preload_libraries=pg_stat_statements",
                "-c",
                "pg_stat_statements.track=none",
                "-c",
                "log_statement=none",
                "-c",
                "log_min_error_statement=panic",
                "-c",
                "log_min_messages=panic",
                "-c",
                "logging_collector=off",
                "-c",
                "archive_mode=off",
                "-c",
                "ssl=off",
            ],
            env_file=env_path,
        )
        self.databases[name] = record
        self.start_container(record, self.phase, "fixture_failure")
        self.wait_database(record, self.phase)
        try:
            bootstrap_password = self.private_read(secret, max_bytes=128).decode("ascii").strip()
            role_sql = bootstrap_postgres_role_sql(bootstrap_password)
        except (UnicodeDecodeError, ValueError) as exc:
            raise DrillFailure(self.phase, "fixture_failure") from exc
        self.sql(record, role_sql, self.phase, database_user=RESTORE_BOOTSTRAP_ROLE)
        self.truncate_private(secret)
        if self.private_read(secret) != b"":
            raise DrillFailure(self.phase, "fixture_failure")
        self.delete_private_file(secret)
        self.delete_private_file(env_path)
        self.verify_database_settings(record, self.phase)
        return record

    def verify_database_settings(self, database: dict[str, Any], phase: str) -> None:
        settings = (
            ("listen_addresses", "127.0.0.1"),
            ("unix_socket_directories", "/run/postgres,/run/postgresql"),
            ("shared_preload_libraries", "pg_stat_statements"),
            ("pg_stat_statements.track", "none"),
            ("log_statement", "none"),
            ("log_min_error_statement", "panic"),
            ("log_min_messages", "panic"),
            ("logging_collector", "off"),
            ("archive_mode", "off"),
            ("ssl", "off"),
        )
        for name, expected in settings:
            if self.sql(database, f"SHOW {name};", phase) != expected:
                raise DrillFailure(phase, "isolation_refusal")

    def migrate_database(
        self,
        name: str,
        database: dict[str, Any],
        version: str,
        phase: str,
        *,
        role: str | None = None,
        keep_env: bool = False,
    ) -> None:
        self.phase = phase
        env_file = self.files[f"{name}_atlas_env"]
        socket_mount = self.mount(self.sockets[name], "/run/postgres", writable=True)
        migration_mount = self.mount(self.run_dir / "migrations", "/migrations", writable=False)
        record = self.image_container(
            "tools",
            role=role or f"{name}_migration",
            user="65532:70",
            memory=CLIENT_MEMORY,
            pids_limit=96,
            mounts=[socket_mount, migration_mount],
            tmpfs_options={"/tmp": "rw,nosuid,nodev,noexec,size=128m,uid=70,gid=70,mode=1777"},
            command_args=[
                "migrate",
                "apply",
                "--dir",
                "file:///migrations",
                "--config",
                "file:///run/app/atlas.hcl",
                "--env",
                "drill",
                "--to-version",
                version,
            ],
            entrypoint="/run/app/atlas",
            network_mode=f"container:{database['id']}",
            env_file=env_file,
            interactive=True,
        )
        self.utility(record, phase, "migration_failure", timeout=900)
        if not keep_env:
            self.delete_private_file(env_file)

    def fixture_state(
        self,
        database: dict[str, Any],
        phase: str,
        *,
        pinned_search_path: bool = False,
    ) -> list[str]:
        if pinned_search_path:
            value = self.sql(database, FIXTURE_STATE_SQL, phase, pinned_search_path=True)
        else:
            value = self.sql(database, FIXTURE_STATE_SQL, phase)
        parts = value.split("|")
        if len(parts) != 9 or not all(part.isdigit() for part in (parts[0], parts[1], *parts[4:])):
            raise DrillFailure(phase, "schema_failure")
        # The harness's own restore admin role lives on the target until promotion.
        local_admin = self.local_admin_role
        if local_admin is not None:
            parts[2] = ",".join(role for role in parts[2].split(",") if role != local_admin)
        return parts

    def object_inventory(self, database: dict[str, Any], phase: str) -> tuple[int, str]:
        value = self.sql(database, OBJECT_INVENTORY_SQL, phase)
        parts = value.split("|")
        if len(parts) != 2 or not parts[0].isdigit() or not re.fullmatch(r"[0-9a-f]{32}", parts[1]):
            raise DrillFailure(phase, "schema_failure")
        return int(parts[0]), parts[1]

    def local_object_inventory(self, database: dict[str, Any], phase: str) -> tuple[int, str]:
        value = self.sql(
            database,
            LOCAL_OBJECT_INVENTORY_SQL,
            phase,
            database_user=LOCAL_ATTESTATION_ROLE,
            pinned_search_path=True,
        )
        parts = value.split("|")
        if len(parts) != 2 or not parts[0].isdigit() or not re.fullmatch(r"[0-9a-f]{32}", parts[1]):
            raise DrillFailure(phase, "schema_failure")
        return int(parts[0]), parts[1]

    def local_schema_snapshot(self, database: dict[str, Any], phase: str) -> tuple[Any, ...]:
        return (
            self.local_object_inventory(database, phase),
            self.local_role_state(database, phase),
            self.sql(
                database,
                DATABASE_STATE_SQL,
                phase,
                database_user=LOCAL_ATTESTATION_ROLE,
                pinned_search_path=True,
            ),
            self.sql(
                database,
                EXTENSION_STATE_SQL,
                phase,
                database_user=LOCAL_ATTESTATION_ROLE,
                pinned_search_path=True,
            ),
        )

    def local_role_state(self, database: dict[str, Any], phase: str) -> str:
        try:
            state = json.loads(
                self.sql(
                    database,
                    ROLE_STATE_SQL,
                    phase,
                    database_user=LOCAL_ATTESTATION_ROLE,
                    pinned_search_path=True,
                )
            )
        except (json.JSONDecodeError, TypeError) as exc:
            raise DrillFailure(phase, "schema_failure") from exc
        if not isinstance(state, dict) or not all(isinstance(state.get(key), list) for key in ("roles", "memberships", "settings")):
            raise DrillFailure(phase, "schema_failure")
        local_admin = self.local_admin_role
        state["roles"] = [role for role in state["roles"] if role.get("name") != local_admin]
        for role in state["roles"]:
            if role.get("name") == "postgres":
                role.update(
                    superuser=False,
                    create_role=False,
                    create_database=False,
                    replication=False,
                    bypass_rls=False,
                )
        state["memberships"] = [
            membership
            for membership in state["memberships"]
            if membership.get("member") != local_admin
            and membership.get("role") != local_admin
            and membership.get("grantor") != local_admin
        ]
        state["settings"] = [setting for setting in state["settings"] if setting.get("role") != local_admin]
        return json.dumps(state, separators=(",", ":"), sort_keys=True)

    def local_dump_migration_head(
        self,
        source: dict[str, Any],
        target: dict[str, Any],
        phase: str,
    ) -> str:
        if target is not self.databases.get("target"):
            raise DrillFailure(phase, "isolation_refusal")
        if self.sql(
            target,
            "SELECT rolsuper::text FROM pg_catalog.pg_roles WHERE rolname = CURRENT_USER;",
            phase,
            database_user="postgres",
            pinned_search_path=True,
        ) != "false":
            raise DrillFailure(phase, "schema_failure")
        safe = self.sql(
            target,
            MIGRATION_HEAD_RELATION_SAFE_SQL,
            phase,
            database_user=LOCAL_ATTESTATION_ROLE,
            pinned_search_path=True,
        )
        if safe != "safe":
            raise DrillFailure(phase, "schema_failure")
        target_shape = self.sql(
            target,
            MIGRATION_HEAD_RELATION_SHAPE_SQL,
            phase,
            database_user=LOCAL_ATTESTATION_ROLE,
            pinned_search_path=True,
        )
        source_shape = self.sql(
            source,
            MIGRATION_HEAD_RELATION_SHAPE_SQL,
            phase,
            database_user=LOCAL_ATTESTATION_ROLE,
            pinned_search_path=True,
        )
        if not target_shape or target_shape != source_shape:
            raise DrillFailure(phase, "schema_failure")
        version = self.sql(
            target,
            MIGRATION_HEAD_SQL,
            phase,
            database_user="postgres",
            pinned_search_path=True,
        )
        try:
            return resolve_local_dump_migration_head(version, self.run_dir / "migrations")
        except ValueError as exc:
            raise DrillFailure(phase, "schema_failure") from exc

    def table_counts(self, database: dict[str, Any], phase: str) -> tuple[int, str]:
        value = self.sql(database, TABLE_COUNTS_SQL, phase)
        parts = value.split("|")
        if len(parts) != 2 or not parts[0].isdigit() or not re.fullmatch(r"[0-9a-f]{32}", parts[1]):
            raise DrillFailure(phase, "schema_failure")
        return int(parts[0]), parts[1]

    def verify_schema_state(
        self,
        database: dict[str, Any],
        expected_users: int,
        expected_roles: str,
        phase: str,
        expected_head: str = BASE_SCHEMA,
        expected_auth_keys: int = 0,
    ) -> None:
        state = self.fixture_state(database, phase)
        if state != [str(expected_auth_keys), str(expected_users), expected_roles, "pg_stat_statements,plpgsql", "0", "0", "0", "0", "0"]:
            raise DrillFailure(phase, "schema_failure")
        head = self.sql(database, MIGRATION_HEAD_SQL, phase)
        if head != expected_head:
            raise DrillFailure(phase, "schema_failure")

    def seed_source_fixture(self, database: dict[str, Any]) -> None:
        self.phase = "fixture_generation"
        self.fixture_check = "seed_source_fixture"
        mounts = [
            self.mount(self.files["source_dsn"], "/run/secrets/postgres.dsn", writable=False),
            self.mount(self.files["seed_authkey"], "/run/secrets/authkey.hex", writable=False),
            self.mount(self.files["seed_username"], "/run/secrets/username", writable=False),
            self.mount(self.files["seed_password"], "/run/secrets/password", writable=False),
        ]
        record = self.image_container(
            "tools",
            role="source_seed",
            user="0:0",
            memory=CLIENT_MEMORY,
            pids_limit=64,
            mounts=mounts,
            tmpfs_options={
                "/run/postgres": "rw,nosuid,nodev,noexec,size=8m,uid=0,gid=0,mode=0700",
                "/tmp": "rw,nosuid,nodev,noexec,size=64m,uid=0,gid=0,mode=1777",
            },
            entrypoint="/run/app/restore-readiness-seed-shim",
            network_mode=f"container:{database['id']}",
        )
        self.utility(record, self.phase, "fixture_failure")
        self.verify_schema_state(database, 1, MIGRATION_ROLES, self.phase, SEED_SCHEMA)
        for key in ("source_dsn", "seed_authkey", "seed_username", "seed_password"):
            self.delete_private_file(self.files[key])

    def seed_source_auth_key_fixture(self, database: dict[str, Any]) -> None:
        self.phase = "fixture_generation"
        self.fixture_check = "source_auth_key_fixture"
        generated_ids: list[int] = []
        while len(generated_ids) < AUTH_KEY_FIXTURE_COUNT:
            candidate = -(secrets.randbelow(9_000_000_000_000_000_000) + 1)
            if candidate not in generated_ids:
                generated_ids.append(candidate)
        self.synthetic_auth_key_ids = validate_synthetic_auth_key_ids(generated_ids)
        self.sql(database, synthetic_auth_key_fixture_sql(self.synthetic_auth_key_ids), self.phase)
        if self.sql(database, AUTH_KEY_STATE_SQL, self.phase).split("|") != AUTH_KEY_FIXTURE_STATE:
            raise DrillFailure(
                self.phase,
                "fixture_failure",
                fixture_check="source_auth_key_fixture",
            )

    def create_egress_probe(self, gateway: str) -> None:
        self.phase = "preflight"
        if self.gateway is None:
            self.gateway = GatewayFixture(gateway)
        record = self.image_container(
            "tools",
            role="egress_probe",
            user="65532:65532",
            memory=CLIENT_MEMORY,
            pids_limit=32,
            mounts=[],
            tmpfs_options={},
            command_args=[gateway, str(self.gateway.port)],
            extra_hosts=[
                f"restore-ipv4.fixture.invalid:{gateway}",
                f"restore-metadata.fixture.invalid:{gateway}",
            ],
            interactive=True,
        )
        output = self.utility(record, self.phase, "isolation_refusal")
        try:
            result = json.loads(output)
        except json.JSONDecodeError as exc:
            raise DrillFailure(self.phase, "isolation_refusal") from exc
        expected = {
            "host_gateway_denied": True,
            "metadata_fixture_denied": True,
            "external_dns_denied": True,
            "ipv4_fixture_denied": True,
            "ipv6_fixture_denied": True,
            "loopback_only": True,
        }
        if result != expected or self.gateway.snapshot() != self.gateway.baseline:
            raise DrillFailure(self.phase, "isolation_refusal")

    def create_restore_clients(
        self,
        source: dict[str, Any],
        target: dict[str, Any],
        *,
        malformed_sql: str | None = None,
    ) -> tuple[dict[str, Any] | None, dict[str, Any]]:
        self.phase = "restore"
        source_client = None
        if malformed_sql is None:
            source_client = self.image_container(
                "source",
                role="source_client",
                user="65532:70",
                memory=CLIENT_MEMORY,
                pids_limit=96,
                mounts=[self.mount(self.sockets["source"], "/run/postgres", writable=True)],
                # Shadow the image's declared volume so no anonymous volume is created.
                tmpfs_options={
                    "/tmp": "rw,nosuid,nodev,noexec,size=128m,uid=70,gid=70,mode=1777",
                    "/var/lib/postgresql/data": "rw,nosuid,nodev,noexec,size=1m,uid=70,gid=70,mode=0700",
                },
                command_args=[
                    "--format=custom",
                    "--no-owner",
                    "--no-acl",
                    "--no-publications",
                    "--no-subscriptions",
                    "--dbname=teagram",
                ],
                entrypoint="pg_dump",
                network_mode=f"container:{source['id']}",
                env_file=self.files["source_tool_env"],
                interactive=True,
            )
        target_client = self.image_container(
            "target",
            role="target_client",
            user="65532:70",
            memory=CLIENT_MEMORY,
            pids_limit=96,
            mounts=[self.mount(self.sockets["target"], "/run/postgres", writable=True)],
            tmpfs_options={
                "/tmp": "rw,nosuid,nodev,noexec,size=128m,uid=70,gid=70,mode=1777",
                "/var/lib/postgresql": "rw,nosuid,nodev,noexec,size=1m,uid=70,gid=70,mode=0700",
            },
            command_args=(
                ["--no-psqlrc", "--set=ON_ERROR_STOP=1", "--dbname=teagram"]
                if malformed_sql is not None
                else ["--exit-on-error", "--no-owner", "--no-acl", "--no-privileges", "--dbname=teagram"]
            ),
            entrypoint="psql" if malformed_sql is not None else "pg_restore",
            network_mode=f"container:{target['id']}",
            env_file=self.files["target_tool_env"],
            interactive=True,
        )
        return source_client, target_client

    def restore_fixture(
        self,
        source: dict[str, Any],
        target: dict[str, Any],
        *,
        malformed_sql: str | None = None,
    ) -> None:
        self.phase = "restore"
        source_client, target_client = self.create_restore_clients(source, target, malformed_sql=malformed_sql)
        self.stream_containers(
            source_client,
            target_client,
            phase=self.phase,
            category="restore_failure",
            max_bytes=MAX_ARCHIVE,
            consumer_input=malformed_sql.encode("utf-8") if malformed_sql is not None else None,
        )
        if malformed_sql is not None:
            # A successful consumer means the malformed stream was accepted.
            # Keep this distinct from a rejected restore so the shell gate cannot
            # mistake swallowed stream errors for the expected drill result.
            raise DrillFailure(self.phase, "schema_failure")
        if source_client is None:
            raise DrillFailure(self.phase, "restore_failure")
        source_state = self.attest_container(source_client, self.phase).get("State") or {}
        target_state = self.attest_container(target_client, self.phase).get("State") or {}
        if source_state.get("Running") or target_state.get("Running") or int(source_state.get("ExitCode", -1)) != 0 or int(target_state.get("ExitCode", -1)) != 0:
            raise DrillFailure(self.phase, "restore_failure")
        self.discard_container(source_client, self.phase, "restore_failure")
        self.discard_container(target_client, self.phase, "restore_failure")
        self.delete_private_file(self.files["source_tool_env"])

        self.phase = "schema_validation"
        self.verify_schema_state(target, 1, "", self.phase, expected_auth_keys=AUTH_KEY_FIXTURE_COUNT)
        source_inventory = self.object_inventory(source, self.phase)
        target_inventory = self.object_inventory(target, self.phase)
        source_rows = self.table_counts(source, self.phase)
        target_rows = self.table_counts(target, self.phase)
        if (
            source_inventory != target_inventory
            or source_inventory[0] < 1
            or source_rows != target_rows
            or source_rows[0] < 1
        ):
            raise DrillFailure(self.phase, "schema_failure")

    def verify_local_dump_unchanged(self) -> None:
        phase = "restore"
        if self.local_dump_path is None or self.local_dump_fd is None or self.local_dump_info is None:
            raise DrillFailure(phase, "isolation_refusal")
        try:
            descriptor_info = os.fstat(self.local_dump_fd)
            path_info = os.stat(self.local_dump_path, follow_symlinks=False)
        except OSError as exc:
            raise DrillFailure(phase, "isolation_refusal") from exc
        if (
            local_dump_identity(descriptor_info) != self.local_dump_info
            or local_dump_identity(path_info) != self.local_dump_info
            or not validate_local_dump_metadata(descriptor_info, self.local_dump_path.name)
        ):
            raise DrillFailure(phase, "isolation_refusal")

    def create_local_dump_fixture(self, source: dict[str, Any]) -> None:
        self.phase = "fixture_generation"
        self.fixture_check = "create_local_dump_fixture"
        if not self.args.local_fixture_test or self.local_dump_path is not None:
            raise DrillFailure(self.phase, "isolation_refusal")
        marker = LOCAL_DUMP_LEAK_MARKER.decode("ascii")
        token = self.manifest["token"]
        used_ids = set(self.synthetic_auth_key_ids)
        unenrolled_ids: list[int] = []
        while len(unenrolled_ids) < LOCAL_UNENROLLED_AUTH_KEY_FIXTURE_COUNT:
            candidate = -(secrets.randbelow(9_000_000_000_000_000_000) + 1)
            if candidate not in used_ids:
                used_ids.add(candidate)
                unenrolled_ids.append(candidate)
        self.sql(source, local_unenrolled_auth_key_fixture_sql(unenrolled_ids, token), self.phase)
        if (
            self.sql(source, AUTH_KEY_STATE_SQL, self.phase).split("|") != ["5", "2", "3", "1", "2", "0"]
            or self.sql(source, UNENROLLED_BOUND_AUTH_KEY_COUNT_SQL, self.phase)
            != str(LOCAL_UNENROLLED_AUTH_KEY_FIXTURE_COUNT)
        ):
            raise DrillFailure(self.phase, "fixture_failure")
        self.sql(source, f"COMMENT ON SCHEMA public IS '{marker}';", self.phase)
        fixture_timestamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
        fixture_path = self.run_dir / "work" / f"pgdump-pre-{token[:7]}-{fixture_timestamp}.sql"
        self.private_write(
            fixture_path,
            b"",
            mode=0o600,
            owner=(65532, 70),
            purpose="synthetic_local_dump_fixture",
        )
        client = self.image_container(
            "target",
            role="source_local_dump_fixture",
            user="65532:70",
            memory=CLIENT_MEMORY,
            pids_limit=64,
            mounts=[
                self.mount(self.sockets["source"], "/run/postgres", writable=True),
                self.mount(fixture_path, "/run/local-dump/output.sql", writable=True),
            ],
            tmpfs_options={
                "/tmp": "rw,nosuid,nodev,noexec,size=64m,uid=70,gid=70,mode=1777",
                "/var/lib/postgresql": "rw,nosuid,nodev,noexec,size=1m,uid=70,gid=70,mode=0700",
            },
            command_args=[
                "--format=plain",
                "--no-publications",
                "--no-subscriptions",
                "--file=/run/local-dump/output.sql",
                "--dbname=teagram",
            ],
            entrypoint="pg_dump",
            network_mode=f"container:{source['id']}",
            env_file=self.files["source_tool_env"],
            interactive=True,
        )
        dump_output = self.utility(client, self.phase, "fixture_failure", timeout=120)
        if LOCAL_DUMP_LEAK_MARKER in dump_output:
            raise DrillFailure(self.phase, "fixture_failure")
        fixture_info = fixture_path.lstat()
        if (
            not stat.S_ISREG(fixture_info.st_mode)
            or fixture_info.st_uid != 65532
            or fixture_info.st_gid != 70
            or stat.S_IMODE(fixture_info.st_mode) != 0o600
            or fixture_info.st_nlink != 1
            or not 0 < fixture_info.st_size <= 16 * 1024 * 1024
        ):
            raise DrillFailure(self.phase, "fixture_failure")
        os.chown(fixture_path, 0, 0, follow_symlinks=False)
        os.chmod(fixture_path, 0o600, follow_symlinks=False)
        fixture_info = fixture_path.lstat()
        if not stat.S_ISREG(fixture_info.st_mode) or fixture_info.st_nlink != 1 or fixture_info.st_size > 16 * 1024 * 1024:
            raise DrillFailure(self.phase, "fixture_failure")
        fixture_record = next(item for item in self.manifest["files"] if item["path"] == str(fixture_path))
        fixture_record.update(uid=0, gid=0, mode=0o600, size=fixture_info.st_size)
        self.save()
        try:
            self.local_dump_fd, self.local_dump_info = open_local_dump(fixture_path)
        except (OSError, ValueError) as exc:
            raise DrillFailure(self.phase, "fixture_failure") from exc
        self.local_dump_path = fixture_path

    def run_local_fixture_sql(self, target: dict[str, Any], statement: str) -> tuple[int, bytes]:
        self.fixture_check = "run_local_fixture_sql"
        return_phase = self.phase
        self.phase = "restore"
        if not self.args.local_fixture_test or target is not self.databases.get("target"):
            raise DrillFailure(self.phase, "isolation_refusal")
        restrict_key = secrets.token_hex(16)
        client = self.image_container(
            "target",
            role="local_fixture_attack_client",
            user="65532:70",
            memory=CLIENT_MEMORY,
            pids_limit=64,
            mounts=[self.mount(self.sockets["target"], "/run/postgres", writable=True)],
            tmpfs_options={
                "/tmp": "rw,nosuid,nodev,noexec,size=64m,uid=70,gid=70,mode=1777",
                "/var/lib/postgresql": "rw,nosuid,nodev,noexec,size=1m,uid=70,gid=70,mode=0700",
            },
            command_args=["-X", "-qAt", "-v", "ON_ERROR_STOP=1", "--single-transaction", "--file=-", "-U", "postgres", "-d", "teagram"],
            entrypoint="psql",
            network_mode=f"container:{target['id']}",
            env_file=self.files["target_tool_env"],
            interactive=True,
        )
        payload = f"\\restrict {restrict_key}\n{statement.rstrip()}\n".encode()
        output = self.attached(
            client,
            self.phase,
            "fixture_failure",
            input_bytes=payload,
            accept_nonzero=True,
            timeout=60,
        )
        document = self.attest_container(client, self.phase)
        state = document.get("State") or {}
        if state.get("Running"):
            raise DrillFailure(self.phase, "fixture_failure")
        status = int(state.get("ExitCode", -1))
        self.discard_container(client, self.phase, "fixture_failure")
        self.phase = return_phase
        return status, output

    def run_local_hostile_fixture_tests(
        self,
        source: dict[str, Any],
        target: dict[str, Any],
        expected_head: str,
    ) -> None:
        self.fixture_check = "run_local_hostile_fixture_tests"
        if not self.args.local_fixture_test:
            return
        self.phase = "schema_validation"
        baseline = self.local_schema_snapshot(target, self.phase)
        if baseline != self.local_schema_snapshot(source, self.phase):
            raise DrillFailure(self.phase, "schema_failure")
        marker = LOCAL_DUMP_LEAK_MARKER.decode("ascii")
        refused_inputs = (
            "CREATE ROLE restore_readiness_hostile SUPERUSER;",
            "ALTER ROLE postgres WITH SUPERUSER;",
            "COPY (SELECT 1) TO PROGRAM 'touch /tmp/restore-readiness-local-copy-to';",
            "COPY public.auth_keys FROM PROGRAM 'touch /tmp/restore-readiness-local-copy-from';",
            "DROP EXTENSION pg_stat_statements; CREATE EXTENSION pg_stat_statements;",
            "CREATE LANGUAGE restore_readiness_untrusted HANDLER pg_catalog.plpgsql_call_handler;",
            f"\\! printf '{marker}'",
        )
        for statement in refused_inputs:
            status, output = self.run_local_fixture_sql(target, statement)
            current = self.local_schema_snapshot(target, self.phase)
            if status == 0 or marker.encode() in output or current != baseline:
                raise DrillFailure(self.phase, "schema_failure")

        status, _ = self.run_local_fixture_sql(target, "ALTER DATABASE teagram SET work_mem = '9MB';")
        if status != 0 or self.sql(target, DATABASE_STATE_SQL, self.phase) == baseline[2]:
            raise DrillFailure(self.phase, "schema_failure")
        status, _ = self.run_local_fixture_sql(target, "ALTER DATABASE teagram RESET work_mem;")
        if status != 0 or self.sql(target, DATABASE_STATE_SQL, self.phase) != baseline[2]:
            raise DrillFailure(self.phase, "schema_failure")

        sequence_increment = self.sql(
            target,
            "SELECT seqincrement::text FROM pg_sequence "
            "WHERE seqrelid = 'public.users_id_seq'::regclass;",
            self.phase,
        )
        if not re.fullmatch(r"-?[1-9][0-9]*", sequence_increment):
            raise DrillFailure(self.phase, "schema_failure")
        test_increment = "19" if sequence_increment == "17" else "17"
        status, _ = self.run_local_fixture_sql(
            target,
            f"ALTER SEQUENCE public.users_id_seq INCREMENT BY {test_increment};",
        )
        if status != 0 or self.local_object_inventory(target, self.phase) == baseline[0]:
            raise DrillFailure(self.phase, "schema_failure")
        status, _ = self.run_local_fixture_sql(
            target,
            f"ALTER SEQUENCE public.users_id_seq INCREMENT BY {sequence_increment};",
        )
        if status != 0 or self.local_object_inventory(target, self.phase) != baseline[0]:
            raise DrillFailure(self.phase, "schema_failure")

        status, _ = self.run_local_fixture_sql(
            target,
            "CREATE FUNCTION public.restore_readiness_unexpected() RETURNS trigger "
            "LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$; "
            "CREATE TRIGGER restore_readiness_unexpected BEFORE INSERT ON public.users "
            "FOR EACH ROW EXECUTE FUNCTION public.restore_readiness_unexpected();",
        )
        if status != 0 or self.local_object_inventory(target, self.phase) == baseline[0]:
            raise DrillFailure(self.phase, "schema_failure")
        status, _ = self.run_local_fixture_sql(
            target,
            "DROP TRIGGER restore_readiness_unexpected ON public.users; "
            "DROP FUNCTION public.restore_readiness_unexpected();",
        )
        if status != 0:
            raise DrillFailure(self.phase, "schema_failure")
        status, _ = self.run_local_fixture_sql(
            target,
            "CREATE RULE restore_readiness_unexpected_rule AS ON INSERT TO public.users "
            "DO ALSO SELECT pg_catalog.pg_reload_conf();",
        )
        if status != 0 or self.local_object_inventory(target, self.phase) == baseline[0]:
            raise DrillFailure(self.phase, "schema_failure")
        try:
            self.verify_local_schema_state(source, target, self.phase, expected_head=expected_head)
        except DrillFailure as exc:
            if exc.phase != self.phase or exc.category != "schema_failure":
                raise
        else:
            raise DrillFailure(self.phase, "schema_failure")
        status, _ = self.run_local_fixture_sql(
            target,
            "DROP RULE restore_readiness_unexpected_rule ON public.users;",
        )
        if status != 0 or self.local_schema_snapshot(target, self.phase) != baseline:
            raise DrillFailure(self.phase, "schema_failure")
        current = (
            *self.local_schema_snapshot(target, self.phase),
        )
        if current != baseline:
            raise DrillFailure(self.phase, "schema_failure")

        program_marker = "/tmp/restore-readiness-local-copy-to"
        status, _ = self.run_local_fixture_sql(
            target,
            "ALTER TABLE atlas_schema_revisions.atlas_schema_revisions "
            "RENAME TO restore_readiness_saved_revisions; "
            "CREATE FUNCTION public.restore_readiness_copy_to_program() RETURNS text "
            "LANGUAGE plpgsql AS $$ BEGIN "
            "EXECUTE 'COPY (SELECT 1) TO PROGRAM ''touch /tmp/restore-readiness-local-copy-to'''; "
            f"RETURN '{expected_head}'; END $$; "
            "CREATE VIEW atlas_schema_revisions.atlas_schema_revisions AS "
            "SELECT public.restore_readiness_copy_to_program() AS version;",
        )
        if status != 0:
            raise DrillFailure(self.phase, "schema_failure")
        try:
            self.verify_local_schema_state(source, target, self.phase, expected_head=expected_head)
        except DrillFailure as exc:
            if exc.phase != self.phase or exc.category != "schema_failure":
                raise
        else:
            raise DrillFailure(self.phase, "schema_failure")
        self.call(
            ["exec", "--user", "70:70", target["id"], "test", "!", "-e", program_marker],
            phase=self.phase,
            category="schema_failure",
        )
        status, _ = self.run_local_fixture_sql(
            target,
            "DROP VIEW atlas_schema_revisions.atlas_schema_revisions; "
            "DROP FUNCTION public.restore_readiness_copy_to_program(); "
            "ALTER TABLE atlas_schema_revisions.restore_readiness_saved_revisions "
            "RENAME TO atlas_schema_revisions;",
        )
        if status != 0:
            raise DrillFailure(self.phase, "schema_failure")

        status, _ = self.run_local_fixture_sql(
            target,
            "ALTER ROLE postgres SET search_path = public, pg_catalog; "
            "CREATE FUNCTION public.md5(text) RETURNS text LANGUAGE plpgsql IMMUTABLE "
            "AS $$ BEGIN RAISE EXCEPTION 'shadow md5 executed'; END $$;",
        )
        if status != 0:
            raise DrillFailure(self.phase, "schema_failure")
        try:
            self.verify_local_schema_state(source, target, self.phase, expected_head=expected_head)
        except DrillFailure as exc:
            if exc.phase != self.phase or exc.category != "schema_failure":
                raise
        else:
            raise DrillFailure(self.phase, "schema_failure")
        self.call(
            ["exec", "--user", "70:70", target["id"], "test", "!", "-e", program_marker],
            phase=self.phase,
            category="schema_failure",
        )
        status, _ = self.run_local_fixture_sql(
            target,
            "ALTER ROLE postgres RESET search_path; DROP FUNCTION public.md5(text);",
        )
        if status != 0:
            raise DrillFailure(self.phase, "schema_failure")
        if self.local_schema_snapshot(target, self.phase) != baseline:
            raise DrillFailure(self.phase, "schema_failure")

    def prepare_local_restore_target(self, target: dict[str, Any]) -> None:
        self.phase = "restore"
        if target is not self.databases.get("target") or self.local_dump_path is None:
            raise DrillFailure(self.phase, "isolation_refusal")
        self.local_admin_role = f"restore_admin_{self.manifest['token'][:16]}"
        self.sql(
            target,
            "\n".join(
                (
                    "CREATE EXTENSION IF NOT EXISTS pg_stat_statements;",
                    "REVOKE ALL ON TABLE public.pg_stat_statements, public.pg_stat_statements_info FROM PUBLIC;",
                    "REVOKE EXECUTE ON FUNCTION public.pg_stat_statements(boolean), public.pg_stat_statements_info() FROM PUBLIC;",
                    "GRANT SELECT ON TABLE public.pg_stat_statements, public.pg_stat_statements_info TO pg_read_all_stats;",
                    "GRANT EXECUTE ON FUNCTION public.pg_stat_statements(boolean), public.pg_stat_statements_info() TO pg_read_all_stats;",
                    "CREATE ROLE browser_acceptance_probe LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;",
                    f"CREATE ROLE {self.local_admin_role} SUPERUSER LOGIN;",
                    "ALTER DATABASE postgres OWNER TO browser_acceptance_probe;",
                    "REVOKE CONNECT ON DATABASE postgres FROM PUBLIC, postgres;",
                    "ALTER DATABASE template1 OWNER TO browser_acceptance_probe;",
                    "REVOKE CONNECT ON DATABASE template1 FROM PUBLIC, postgres;",
                    "ALTER ROLE postgres NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;",
                )
            ),
            self.phase,
        )
        if self.sql(target, "SELECT rolsuper::text || '|' || rolcreaterole::text || '|' || rolcreatedb::text FROM pg_roles WHERE rolname = 'postgres';", self.phase) != "false|false|false":
            raise DrillFailure(self.phase, "isolation_refusal")

    def restore_local_dump(self, target: dict[str, Any]) -> None:
        self.phase = "restore"
        if target is not self.databases.get("target"):
            raise DrillFailure(self.phase, "isolation_refusal")
        self.verify_local_dump_unchanged()
        dump_mount = self.mount(self.local_dump_path, "/run/local-dump/input.sql", writable=False)
        restrict_key = secrets.token_hex(16)
        restore_script = r'''set -eu
restrict_key=$1
dump_path=$2
{
  printf '\\restrict %s\n' "$restrict_key"
  if awk '
    {
      if (!sql_started && source_key == "" && substr($0, 1, 10) == "\\restrict ") {
        source_key = substr($0, 11)
        next
      }
      if (dump_complete) {
        if ($0 ~ /^[[:space:]]*$/ || $0 ~ /^[[:space:]]*--/) next
        if (source_key != "" && !source_unrestrict && $0 == "\\unrestrict " source_key) {
          source_unrestrict = 1
          next
        }
        invalid_meta = 1
        next
      }
      if ($0 ~ /^\\restrict([[:space:]]|$)/ || $0 ~ /^\\unrestrict([[:space:]]|$)/) {
        invalid_meta = 1
        next
      }
      if ($0 !~ /^[[:space:]]*$/ && $0 !~ /^[[:space:]]*--/) sql_started = 1
      if (has_pending) print pending
      pending = $0
      has_pending = 1
      if ($0 == "-- PostgreSQL database dump complete") dump_complete = 1
    }
    END {
      if (invalid_meta || !dump_complete) exit 42
      if (source_key != "" && (!source_unrestrict || pending != "-- PostgreSQL database dump complete")) exit 42
      if (source_key == "" && (source_unrestrict || pending != "-- PostgreSQL database dump complete")) exit 42
    }
  ' "$dump_path"; then
    :
  else
    printf 'SELECT 1 / 0;\n'
  fi
} | psql -X -qAt -v ON_ERROR_STOP=1 --single-transaction --file=- -U postgres -d teagram >/dev/null 2>&1
'''
        client = self.image_container(
            "target",
            role="local_restore_client",
            user="0:70",
            memory=CLIENT_MEMORY,
            pids_limit=96,
            mounts=[
                self.mount(self.sockets["target"], "/run/postgres", writable=True),
                dump_mount,
            ],
            tmpfs_options={
                "/tmp": "rw,nosuid,nodev,noexec,size=128m,uid=70,gid=70,mode=1777",
                "/var/lib/postgresql": "rw,nosuid,nodev,noexec,size=1m,uid=70,gid=70,mode=0700",
            },
            command_args=["-eu", "-c", restore_script, "restore-readiness", restrict_key, "/run/local-dump/input.sql"],
            entrypoint="/bin/sh",
            network_mode=f"container:{target['id']}",
            env_file=self.files["target_tool_env"],
            interactive=True,
        )
        self.attest_container(client, self.phase)
        self.verify_local_dump_unchanged()
        restore_output = self.utility(client, self.phase, "restore_failure", timeout=900)
        if LOCAL_DUMP_LEAK_MARKER in restore_output:
            raise DrillFailure(self.phase, "restore_failure")
        self.verify_local_dump_unchanged()

    def verify_local_schema_state(
        self,
        source: dict[str, Any],
        target: dict[str, Any],
        phase: str,
        expected_head: str,
    ) -> None:
        target_snapshot = self.local_schema_snapshot(target, phase)
        source_snapshot = self.local_schema_snapshot(source, phase)
        if (
            target_snapshot != source_snapshot
            or self.sql(target, MIGRATION_HEAD_SQL, phase, pinned_search_path=True) != expected_head
            or self.sql(source, MIGRATION_HEAD_SQL, phase, pinned_search_path=True) != expected_head
            or self.fixture_state(target, phase, pinned_search_path=True)[2:]
            != self.fixture_state(source, phase, pinned_search_path=True)[2:]
        ):
            raise DrillFailure(phase, "schema_failure")

    def promote_local_restore_owner(self, target: dict[str, Any]) -> None:
        self.phase = "schema_validation"
        if target is not self.databases.get("target") or self.local_admin_role is None or self.target_db_password is None:
            raise DrillFailure(self.phase, "isolation_refusal")
        self.sql(
            target,
            "\n".join(
                (
                    "ALTER ROLE browser_acceptance_probe LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;",
                    "ALTER ROLE postgres WITH PASSWORD '" + self.target_db_password + "' SUPERUSER CREATEDB CREATEROLE REPLICATION BYPASSRLS;",
                )
            ),
            self.phase,
            database_user=self.local_admin_role,
        )
        if self.sql(target, "SELECT rolsuper::text || '|' || rolcreaterole::text || '|' || rolcreatedb::text FROM pg_roles WHERE rolname = 'postgres';", self.phase) != "true|true|true":
            raise DrillFailure(self.phase, "schema_failure")
        self.sql(target, f"DROP ROLE {self.local_admin_role};", self.phase)
        self.local_admin_role = None
        # Server settings are readable only by a superuser, so they are rechecked once postgres is promoted.
        self.verify_database_settings(target, self.phase)

    def apply_pending_migration(self, target: dict[str, Any]) -> None:
        self.migrate_database("target", target, HEAD_SCHEMA, "pending_migration")
        self.migration_head = self.sql(
            target,
            MIGRATION_HEAD_SQL,
            "pending_migration",
            pinned_search_path=self.input_mode == "local",
        )
        if self.migration_head != HEAD_SCHEMA or self.sql(
            target,
            INDEX_VALID_SQL,
            "pending_migration",
            pinned_search_path=self.input_mode == "local",
        ) != "valid":
            raise DrillFailure("pending_migration", "schema_failure")

    def auth_key_state(self, database: dict[str, Any], phase: str, *, allow_interrupted: bool = False) -> list[str]:
        state = self.sql(
            database,
            AUTH_KEY_STATE_SQL,
            phase,
            allow_interrupted=allow_interrupted,
            pinned_search_path=self.input_mode == "local",
        ).split("|")
        if len(state) != 6 or any(not value.isdigit() for value in state):
            raise DrillFailure(phase, "schema_failure")
        return state

    def verify_auth_key_inventory(self, database: dict[str, Any], phase: str) -> None:
        try:
            expected = load_auth_key_inventory()
            actual = json.loads(
                self.sql(
                    database,
                    AUTH_KEY_INVENTORY_SQL,
                    phase,
                    pinned_search_path=self.input_mode == "local",
                )
            )
        except (ValueError, json.JSONDecodeError) as exc:
            raise DrillFailure(phase, "schema_failure") from exc
        expected_inventory = {key: expected[key] for key in ("trigger_tables", "foreign_keys", "triggers")}
        if actual != expected_inventory:
            raise DrillFailure(phase, "schema_failure")

    @staticmethod
    def auth_key_clear_marker_path(run_dir: Path) -> Path:
        return run_dir / "work" / "post-restore-marker.json"

    def write_post_restore_marker(self, target: dict[str, Any]) -> None:
        self.phase = "schema_validation"
        if target is not self.databases.get("target"):
            raise DrillFailure(self.phase, "isolation_refusal")
        document = self.attest_container(target, self.phase)
        require_container_running(document, self.phase, "schema_failure")
        if self.sql(
            target,
            MIGRATION_HEAD_SQL,
            self.phase,
            pinned_search_path=self.input_mode == "local",
        ) != HEAD_SCHEMA:
            raise DrillFailure(self.phase, "schema_failure")
        self.verify_auth_key_inventory(target, self.phase)
        auth_key_state = self.auth_key_state(target, self.phase)
        if self.input_mode == "synthetic":
            if auth_key_state != AUTH_KEY_FIXTURE_STATE:
                raise DrillFailure(self.phase, "schema_failure")
            marker = {
                "schema_version": 1,
                "input_mode": self.input_mode,
                "target_container_id": target["id"],
                "database_identity": self.sql(
                    target,
                    DATABASE_IDENTITY_SQL,
                    self.phase,
                    pinned_search_path=self.input_mode == "local",
                ),
                "migration_head": HEAD_SCHEMA,
                "table_counts": self.table_counts(target, self.phase)[1],
                "auth_key_state": auth_key_state,
            }
        else:
            try:
                auth_key_count = int(auth_key_state[0])
                evidence_bound_key_count = int(
                    self.sql(target, AUTH_KEY_EVIDENCE_BOUND_COUNT_SQL, self.phase, pinned_search_path=True)
                )
            except ValueError as exc:
                raise DrillFailure(self.phase, "schema_failure") from exc
            if auth_key_count > MAX_LOCAL_AUTH_KEYS or evidence_bound_key_count > auth_key_count:
                raise DrillFailure(self.phase, "schema_failure")
            marker = {
                "schema_version": 1,
                "input_mode": self.input_mode,
                "target_container_id": target["id"],
                "database_identity": self.sql(
                    target,
                    DATABASE_IDENTITY_SQL,
                    self.phase,
                    pinned_search_path=self.input_mode == "local",
                ),
                "migration_head": HEAD_SCHEMA,
                "auth_key_state": auth_key_state,
                "auth_key_evidence_bound_count": evidence_bound_key_count,
            }
        encoded = (json.dumps(marker, separators=(",", ":"), sort_keys=True) + "\n").encode()
        self.private_write(
            self.auth_key_clear_marker_path(self.run_dir),
            encoded,
            mode=0o400,
            purpose="post_restore_marker",
        )
        self.post_restore_marker = marker

    def validate_auth_key_clear_target(self, target: dict[str, Any]) -> dict[str, Any]:
        phase = "auth_key_clearing"
        if (
            target is not self.databases.get("target")
            or target.get("role") != "target_postgres"
            or target.get("removed")
            or self.manifest.get("processes")
            or self.auth_keys_cleared
        ):
            raise DrillFailure(phase, "isolation_refusal")
        owned = next(
            (record for record in self.manifest.get("containers", []) if record.get("id") == target.get("id")),
            None,
        )
        if owned is not target or any(
            not record.get("removed") and record.get("role") not in {"source_postgres", "target_postgres"}
            for record in self.manifest.get("containers", [])
        ):
            raise DrillFailure(phase, "isolation_refusal")
        marker_path = self.auth_key_clear_marker_path(self.run_dir)
        try:
            raw_marker = self.private_read(marker_path, max_bytes=4096)
            marker = json.loads(raw_marker, object_pairs_hook=unique_json_object)
        except (DrillFailure, OSError, ValueError, json.JSONDecodeError) as exc:
            raise DrillFailure(phase, "isolation_refusal") from exc
        if (
            self.post_restore_marker is None
            or marker != self.post_restore_marker
            or marker.get("target_container_id") != target.get("id")
            or marker.get("migration_head") != HEAD_SCHEMA
            or marker.get("input_mode") != self.input_mode
        ):
            raise DrillFailure(phase, "isolation_refusal")
        document = self.attest_container(target, phase)
        require_container_running(document, phase, "isolation_refusal")
        if (
            self.sql(
                target,
                "SELECT current_database();",
                phase,
                pinned_search_path=self.input_mode == "local",
            ) != "teagram"
            or self.sql(
                target,
                MIGRATION_HEAD_SQL,
                phase,
                pinned_search_path=self.input_mode == "local",
            ) != marker["migration_head"]
            or self.sql(
                target,
                DATABASE_IDENTITY_SQL,
                phase,
                pinned_search_path=self.input_mode == "local",
            ) != marker["database_identity"]
        ):
            raise DrillFailure(phase, "isolation_refusal")
        self.verify_auth_key_inventory(target, phase)
        state = self.auth_key_state(target, phase)
        if self.input_mode == "synthetic":
            if self.table_counts(target, phase)[1] != marker["table_counts"] or state != AUTH_KEY_FIXTURE_STATE:
                raise DrillFailure(phase, "schema_failure")
        else:
            try:
                evidence_bound_key_count = int(
                    self.sql(target, AUTH_KEY_EVIDENCE_BOUND_COUNT_SQL, phase, pinned_search_path=True)
                )
            except ValueError as exc:
                raise DrillFailure(phase, "schema_failure") from exc
            if (
                state != marker["auth_key_state"]
                or int(state[0]) > MAX_LOCAL_AUTH_KEYS
                or evidence_bound_key_count != marker["auth_key_evidence_bound_count"]
            ):
                raise DrillFailure(phase, "schema_failure")
        if self.sql(
            target,
            "SELECT count(*)::text FROM pg_stat_activity "
            "WHERE datname = current_database() AND backend_type = 'client backend' "
            "AND pid <> pg_backend_pid();",
            phase,
            pinned_search_path=self.input_mode == "local",
        ) != "0":
            raise DrillFailure(phase, "isolation_refusal")
        return marker

    def auth_key_clear_client(self, target: dict[str, Any]) -> dict[str, Any]:
        return self.image_container(
            "target",
            role="auth_key_clearer",
            user="65532:70",
            memory=CLIENT_MEMORY,
            pids_limit=64,
            mounts=[self.mount(self.sockets["target"], "/run/postgres", writable=True)],
            tmpfs_options={
                "/tmp": "rw,nosuid,nodev,noexec,size=64m,uid=70,gid=70,mode=1777",
                "/var/lib/postgresql": "rw,nosuid,nodev,noexec,size=1m,uid=70,gid=70,mode=0700",
            },
            command_args=["-X", "-qAt", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "teagram"],
            entrypoint="psql",
            network_mode=f"container:{target['id']}",
            env_file=self.files["target_tool_env"],
            interactive=True,
        )

    def remove_interrupted_auth_key_client(self, record: dict[str, Any]) -> None:
        def cleanup_call(args: list[str], timeout: int) -> tuple[int, bytes]:
            try:
                return self.cleanup_call(args, timeout=timeout)
            except DrillFailure:
                print(f"restore readiness signal cleanup failed during Docker {args[0]}", file=sys.stderr)
                raise

        # psql runs as PID 1, which ignores SIGTERM; SIGINT cancels the open transaction so it rolls back.
        kill_status, _ = cleanup_call(["kill", "--signal=SIGINT", record["id"]], 10)
        wait_status, wait_output = cleanup_call(["wait", record["id"]], 15)
        if wait_status != 0:
            kill_status, _ = cleanup_call(["kill", "--signal=SIGKILL", record["id"]], 10)
            wait_status, wait_output = cleanup_call(["wait", record["id"]], 15)
        inspect_status, inspect_output = cleanup_call(
            ["inspect", "--format", "{{.State.Running}}", record["id"]], 10
        )
        wait_exit_valid = wait_output.strip().isdigit()
        stopped = inspect_output.strip() == b"false"
        if (
            kill_status != 0
            and wait_status == 0
            and inspect_status == 0
            and stopped
        ):
            kill_status = 0
        if (
            kill_status != 0
            or wait_status != 0
            or not wait_exit_valid
            or inspect_status != 0
            or not stopped
        ):
            print(
                "restore readiness signal cleanup failed while stopping Docker client "
                f"(kill_status={kill_status}, wait_status={wait_status}, "
                f"wait_exit_valid={wait_exit_valid}, inspect_status={inspect_status}, stopped={stopped})",
                file=sys.stderr,
            )
            raise DrillFailure("cleanup", "cleanup_failure")
        remove_status, _ = cleanup_call(["rm", record["id"]], 10)
        if remove_status != 0:
            print("restore readiness signal cleanup failed during Docker rm", file=sys.stderr)
            raise DrillFailure("cleanup", "cleanup_failure")
        list_status, output = cleanup_call(["ps", "--all", "--quiet", "--no-trunc"], 10)
        if list_status != 0 or record["id"].encode() in output.splitlines():
            print(
                "restore readiness signal cleanup failed verifying Docker client removal "
                f"(list_status={list_status}, still_listed={record['id'].encode() in output.splitlines()})",
                file=sys.stderr,
            )
            raise DrillFailure("cleanup", "cleanup_failure")
        record["removed"] = True
        self.save()

    def clear_auth_keys(
        self,
        target: dict[str, Any],
        *,
        inject_failure: bool = False,
        interrupt_signal: int | None = None,
    ) -> None:
        phase = "auth_key_clearing"
        self.phase = phase
        if self.input_mode == "synthetic":
            try:
                self.synthetic_auth_key_ids = validate_synthetic_auth_key_ids(self.synthetic_auth_key_ids)
            except (TypeError, ValueError) as exc:
                raise DrillFailure(phase, "isolation_refusal") from exc
        self.validate_auth_key_clear_target(target)
        before = self.auth_key_state(target, phase)
        if self.input_mode == "synthetic" and before != AUTH_KEY_FIXTURE_STATE:
            raise DrillFailure(phase, "schema_failure")
        if self.input_mode == "local":
            try:
                auth_key_count = int(before[0])
                evidence_bound_key_count = int(
                    self.sql(target, AUTH_KEY_EVIDENCE_BOUND_COUNT_SQL, phase, pinned_search_path=True)
                )
            except ValueError as exc:
                raise DrillFailure(phase, "schema_failure") from exc
            marker = self.post_restore_marker or {}
            if (
                auth_key_count > MAX_LOCAL_AUTH_KEYS
                or evidence_bound_key_count > auth_key_count
                or before != marker.get("auth_key_state")
                or evidence_bound_key_count != marker.get("auth_key_evidence_bound_count")
            ):
                raise DrillFailure(phase, "schema_failure")
        client = self.auth_key_clear_client(target)
        try:
            if self.input_mode == "local":
                if inject_failure:
                    raise DrillFailure(phase, "fault_test_failure")
                transaction = local_auth_key_clear_transaction_sql(
                    wait_for_signal=interrupt_signal is not None
                ).encode()
            else:
                transaction = auth_key_clear_transaction_sql(
                    self.synthetic_auth_key_ids,
                    inject_failure=inject_failure,
                    wait_for_signal=interrupt_signal is not None,
                ).encode()
            fault_kind = "failure" if inject_failure else "signal" if interrupt_signal is not None else None
            failure_category = "fault_test_failure" if fault_kind is not None else "auth_key_clear_failure"
            output = self.attached(
                client,
                phase,
                failure_category,
                input_bytes=transaction,
                interrupt_signal=interrupt_signal,
                interrupt_ack=AUTH_KEY_CLEAR_ACK if interrupt_signal is not None else None,
                accept_nonzero=inject_failure,
                capture_stderr=inject_failure,
                timeout=60,
            )
        except DrillFailure as exc:
            if interrupt_signal is not None and exc.phase == phase and exc.category == "interrupted":
                acknowledged = self.last_attached_interrupt_acknowledged
                self.remove_interrupted_auth_key_client(client)
                after = self.auth_key_state(target, phase, allow_interrupted=True)
                result_category = auth_key_clear_fault_category(
                    "signal",
                    before,
                    after,
                    interrupt_acknowledged=acknowledged,
                )
                raise DrillFailure(phase, result_category) from exc
            raise

        document = self.attest_container(client, phase)
        state = document.get("State") or {}
        container_status = int(state.get("ExitCode", -1))
        if fault_kind is not None:
            if state.get("Running"):
                raise DrillFailure(phase, "fault_test_failure")
            self.discard_container(client, phase, failure_category)
            after = self.auth_key_state(target, phase)
            result_category = auth_key_clear_fault_category(
                fault_kind,
                before,
                after,
                client_status=self.last_attached_status,
                container_status=container_status,
                stdout=output,
                stderr=self.last_attached_stderr,
                interrupt_acknowledged=self.last_attached_interrupt_acknowledged,
            )
            if fault_kind == "failure" and result_category == "fault_test_failure":
                diagnostics = auth_key_clear_failure_diagnostics(
                    before,
                    after,
                    client_status=self.last_attached_status,
                    container_status=container_status,
                    stdout=output,
                    stderr=self.last_attached_stderr,
                )
                print(
                    "restore readiness auth-key injected failure diagnostics: "
                    + json.dumps(diagnostics, separators=(",", ":"), sort_keys=True),
                    file=sys.stderr,
                )
            raise DrillFailure(phase, result_category)
        if state.get("Running") or container_status != 0:
            raise DrillFailure(phase, "auth_key_clear_failure")
        self.discard_container(client, phase, "auth_key_clear_failure")
        if self.input_mode == "synthetic":
            expected_state = AUTH_KEY_CLEARED_STATE
        else:
            expected_state = [
                "0",
                "0",
                "0",
                before[3],
                before[4],
                str(int(before[5]) + evidence_bound_key_count),
            ]
        if output or self.auth_key_state(target, phase) != expected_state:
            raise DrillFailure(phase, "auth_key_clear_failure")
        self.auth_keys_cleared = True

    def start_application(self, target: dict[str, Any]) -> dict[str, Any]:
        self.phase = "daemon_startup"
        if not self.auth_keys_cleared or target is not self.databases.get("target"):
            raise DrillFailure(self.phase, "startup_failure")
        mounts = [
            self.mount(self.sockets["target"], "/run/postgres", writable=True),
            self.mount(self.files["target_dsn_app"], "/run/secrets/postgres.dsn", writable=False),
            self.mount(self.files["app_authkey"], "/run/secrets/authkey.hex", writable=False),
            self.mount(self.files["server_key"], "/run/secrets/server-key.pem", writable=False),
        ]
        environment = {
            "TG_POSTGRES_DSN_FILE": "/run/secrets/postgres.dsn",
            "TG_AUTHKEY_ENC_KEY_FILE": "/run/secrets/authkey.hex",
            "TG_RSA_KEY_PATH": "/run/secrets/server-key.pem",
            "TG_LISTEN_ADDR": "127.0.0.1:2443",
            "TG_ADVERTISE_ADDR": "127.0.0.1:2443",
            "TG_WEBSOCKET_LISTEN_ADDR": "127.0.0.1:2444",
            "TG_REGISTRATION": "closed",
            "TG_PUBLIC_REGISTRATION": "closed",
            "TG_LOG_LOGIN_CODES": "false",
            "TG_PUBLIC_LINK_PREFIX": "https://restore.invalid/",
            "TG_BLOB_DIR": "/run/blobs",
            "TG_MEDIA_ERASURE_DESTRUCTIVE": "false",
            "TG_MEDIA_ERASURE_INTERVAL_MIN": "0s",
            "TG_MEDIA_ERASURE_INTERVAL_MAX": "0s",
            "TG_MEDIA_ERASURE_REPORT_INTERVAL": "0s",
            "TG_BLOB_SCAN_REPORT_INTERVAL": "0s",
        }
        app = self.image_container(
            "app",
            role="application",
            user="10001:70",
            memory=APP_MEMORY,
            pids_limit=128,
            mounts=mounts,
            tmpfs_options={
                "/run/blobs": "rw,nosuid,nodev,noexec,size=64m,uid=10001,gid=70,mode=0700",
                "/tmp": "rw,nosuid,nodev,noexec,size=32m,uid=10001,gid=70,mode=1777",
            },
            explicit_environment=environment,
        )
        self.attest_container(app, self.phase)
        self.attest_container(target, self.phase)
        if self.sql(target, AUTH_KEY_COUNT_SQL, self.phase) != "0":
            raise DrillFailure(self.phase, "startup_failure")
        self.call(["start", app["id"]], phase=self.phase, category="startup_failure")
        document = self.attest_container(app, self.phase)
        require_container_running(document, self.phase, "startup_failure")

        ready = self.image_container(
            "tools",
            role="readiness_probe",
            user="65532:65532",
            memory=CLIENT_MEMORY,
            pids_limit=32,
            mounts=[],
            tmpfs_options={},
            command_args=["--wait-loopback", "2444"],
            network_mode=f"container:{app['id']}",
            interactive=True,
        )
        output = self.utility(ready, self.phase, "startup_failure", timeout=120)
        if output != b'{"loopback_ready":true}\n':
            raise DrillFailure(self.phase, "startup_failure")
        document = self.attest_container(app, self.phase)
        require_container_running(document, self.phase, "startup_failure")
        return app

    def write_checker_invocation(self) -> None:
        invocation = {
            "schema_version": 1,
            "mode": "synthetic-only",
            "endpoint": "ws://127.0.0.1:2444/apiws",
            "scratch_target_owned": True,
            "network_namespace_isolated": True,
            "daemon_identity_pinned": True,
            "registration_closed": True,
        }
        if (self.run_dir / "invocation" / "result.json").exists():
            raise DrillFailure("application_assertions", "isolation_refusal")
        encoded = (json.dumps(invocation, separators=(",", ":"), sort_keys=True) + "\n").encode()
        self.private_write(
            self.run_dir / "invocation" / "invocation.json",
            encoded,
            mode=0o600,
            purpose="checker_invocation",
        )

    def provision_checker_identity(self) -> None:
        self.phase = "application_assertions"
        secret_mounts = [
            self.mount(self.files["target_dsn_provisioner"], "/run/secrets/postgres.dsn", writable=False),
            self.mount(self.files["checker_authkey"], "/run/secrets/authkey.hex", writable=False),
            self.mount(self.files["checker_username"], "/run/secrets/username", writable=False),
            self.mount(self.files["checker_password"], "/run/secrets/password", writable=False),
        ]
        provisioner = self.image_container(
            "tools",
            role="checker_provisioner",
            user="0:0",
            memory=CLIENT_MEMORY,
            pids_limit=64,
            mounts=secret_mounts,
            tmpfs_options={},
            entrypoint="/run/app/restore-readiness-provisioner",
            network_mode=f"container:{self.databases['target']['id']}",
            interactive=True,
        )
        self.attest_container(provisioner, self.phase)
        cli_status, output = self.attached_status(provisioner, self.phase, "provisioning_failure")
        state = self.attest_container(provisioner, self.phase).get("State") or {}
        status = int(state.get("ExitCode", -1))
        if state.get("Running") or status < 0 or cli_status not in {0, status}:
            raise DrillFailure(self.phase, "provisioning_failure")
        self.discard_container(provisioner, self.phase, "provisioning_failure")
        expected_username = self.private_read(self.files["checker_username"], max_bytes=64).strip().decode("ascii", "strict")
        category, identity = parse_provisioner_result(output, status, expected_username)
        if identity is None:
            raise DrillFailure(self.phase, category)
        self.private_write(
            self.run_dir / "invocation" / "provisioned.json",
            (json.dumps(identity, separators=(",", ":"), sort_keys=True) + "\n").encode(),
            mode=0o400,
            purpose="checker_provisioned_identity",
        )
        self.delete_private_file(self.files["target_dsn_provisioner"])
        self.delete_private_file(self.files["checker_authkey"])

    def prepare_checker_inputs(self) -> None:
        directory = self.run_dir / "invocation"
        expected_files = (
            directory / "invocation.json",
            directory / "username",
            directory / "password",
            directory / "server.pub.pem",
            directory / "provisioned.json",
        )
        if {item.name for item in directory.iterdir()} != {item.name for item in expected_files}:
            raise DrillFailure(self.phase, "isolation_refusal")
        for path in expected_files:
            record = next((item for item in self.manifest.get("files", []) if item.get("path") == str(path)), None)
            if record is None:
                raise DrillFailure(self.phase, "isolation_refusal")
            info = path.lstat()
            if (
                not stat.S_ISREG(info.st_mode)
                or info.st_dev != record["device"]
                or info.st_ino != record["inode"]
                or stat.S_IMODE(info.st_mode) != record["mode"]
                or info.st_uid != record["uid"]
                or info.st_gid != record["gid"]
                or info.st_nlink != 1
            ):
                raise DrillFailure(self.phase, "isolation_refusal")
            try:
                os.chown(path, 65532, 65532, follow_symlinks=False)
            except OSError as exc:
                raise DrillFailure(self.phase, "isolation_refusal") from exc
            record["uid"] = 65532
            record["gid"] = 65532
            self.save()
        try:
            os.chown(directory, 65532, 65532, follow_symlinks=False)
            os.chmod(directory, 0o700)
        except OSError as exc:
            raise DrillFailure(self.phase, "isolation_refusal") from exc

    def verify_checker_output(self, stdout: bytes, checker_status: int) -> None:
        phase = "application_assertions"
        try:
            result = json.loads(stdout, object_pairs_hook=unique_json_object)
        except (json.JSONDecodeError, UnicodeDecodeError, ValueError) as exc:
            raise DrillFailure(phase, "assertion_failure") from exc
        required = {"schema_version", "phase", "category", "help_getConfig", "synthetic_signin"}
        if not isinstance(result, dict) or set(result) != required or type(result.get("schema_version")) is not int or result["schema_version"] != 1:
            raise DrillFailure(phase, "assertion_failure")
        if result.get("phase") not in {"invocation", "help_get_config", "provisioning", "synthetic_signin", "application_assertions", "checker_cleanup", "result_artifact"}:
            raise DrillFailure(phase, "assertion_failure")
        categories = {
            "assertions_passed",
            "invocation_invalid",
            "identity_collision",
            "auth_failure",
            "key_mismatch",
            "configuration_invalid",
            "identity_mismatch",
            "connection_failure",
            "provisioning_failure",
            "timeout",
            "checker_cleanup_failure",
            "artifact_failure",
        }
        if result.get("category") not in categories or result.get("help_getConfig") not in {"pass", "fail", "not_run"} or result.get("synthetic_signin") not in {"pass", "fail", "not_run"}:
            raise DrillFailure(phase, "assertion_failure")
        self.checker_result = {
            "help_getConfig": result["help_getConfig"],
            "synthetic_signin": result["synthetic_signin"],
        }
        artifact = self.run_dir / "invocation" / "result.json"
        try:
            info = artifact.lstat()
        except OSError as exc:
            raise DrillFailure(phase, "assertion_failure") from exc
        if (
            not stat.S_ISREG(info.st_mode)
            or stat.S_IMODE(info.st_mode) != 0o600
            or info.st_uid != 65532
            or info.st_nlink != 1
            or info.st_size > 4096
        ):
            raise DrillFailure(phase, "assertion_failure")
        self.register_file(artifact, "checker_result")
        artifact_bytes = self.private_read(artifact, max_bytes=4096)
        try:
            artifact_result = json.loads(artifact_bytes, object_pairs_hook=unique_json_object)
        except (json.JSONDecodeError, ValueError) as exc:
            raise DrillFailure(phase, "assertion_failure") from exc
        if artifact_result != result:
            raise DrillFailure(phase, "assertion_failure")
        leaks = [
            self.manifest["token"].encode(),
            b"restore.invalid",
            b"127.0.0.1:2444",
            str(self.run_dir).encode(),
            LOCAL_DUMP_LEAK_MARKER,
        ]
        for record in self.manifest.get("containers", []):
            leaks.extend((record["name"].encode(), record.get("id", "").encode()))
        for file_record in self.manifest.get("files", []):
            if file_record.get("purpose") in {"checker_result", "synthetic_local_dump_fixture"}:
                continue
            path = Path(file_record["path"])
            if path.exists() and file_record.get("size", 0) > 7:
                content = self.private_read(path, max_bytes=max(2 * 1024 * 1024, file_record["size"]))
                marker = content.strip()
                if len(marker) >= 8:
                    leaks.append(marker)
        if contains_leak_marker(stdout, artifact_bytes, leaks):
            raise DrillFailure(phase, "assertion_failure")
        if checker_passes(result, checker_status):
            return
        if result["category"] == "identity_collision":
            raise DrillFailure(phase, "identity_collision")
        if result["category"] == "auth_failure":
            raise DrillFailure(phase, "auth_failure")
        raise DrillFailure(phase, "assertion_failure")

    def run_application_checker(self, app: dict[str, Any]) -> None:
        self.phase = "application_assertions"
        self.write_checker_invocation()
        self.provision_checker_identity()
        self.prepare_checker_inputs()
        mounts = [
            self.mount(self.run_dir / "invocation", "/run/invocation", writable=True),
        ]
        checker = self.image_container(
            "tools",
            role="checker",
            user="65532:65532",
            memory=APP_MEMORY,
            pids_limit=128,
            mounts=mounts,
            tmpfs_options={"/tmp": "rw,nosuid,nodev,noexec,size=64m,uid=65532,gid=65532,mode=1777"},
            entrypoint="/run/app/restore-drill-checker",
            network_mode=f"container:{app['id']}",
            explicit_environment={"TEAGRAM_RESTORE_DRILL_INVOCATION_FILE": "/run/invocation/invocation.json"},
            interactive=True,
        )
        self.attest_container(checker, self.phase)
        cli_status, output = self.attached_status(checker, self.phase, "assertion_failure")
        checker_state = self.attest_container(checker, self.phase).get("State") or {}
        checker_status = int(checker_state.get("ExitCode", -1))
        if checker_state.get("Running") or checker_status < 0 or cli_status not in {0, checker_status}:
            raise DrillFailure(self.phase, "assertion_failure")
        self.verify_checker_output(output, checker_status)
        self.discard_container(checker, self.phase, "assertion_failure")
        for key in (
            "checker_username",
            "checker_password",
            "server_public",
        ):
            self.delete_private_file(self.files[key])
        self.delete_private_file(self.run_dir / "invocation" / "invocation.json")
        self.delete_private_file(self.run_dir / "invocation" / "provisioned.json")
        self.delete_private_file(self.run_dir / "invocation" / "result.json")

    def cleanup_container_inventory(self) -> list[tuple[str, str]]:
        status, output = self.cleanup_call(
            ["ps", "--all", "--no-trunc", "--format", "{{.ID}} {{.Names}}"],
            timeout=20,
        )
        if status != 0:
            raise DrillFailure("cleanup", "cleanup_failure")
        containers: list[tuple[str, str]] = []
        try:
            for line in output.decode("utf-8", "strict").splitlines():
                fields = line.split(" ", 1)
                if len(fields) != 2 or not re.fullmatch(r"[0-9a-f]{64}", fields[0]):
                    raise ValueError("invalid container listing")
                containers.append((fields[0], fields[1]))
        except (UnicodeDecodeError, ValueError) as exc:
            self.record_cleanup_failure("docker_container_inventory", "invalid_output")
            raise DrillFailure("cleanup", "cleanup_failure") from exc
        return containers

    def cleanup_volume_inventory(self) -> list[str]:
        status, output = self.cleanup_call(["volume", "ls", "--quiet"], timeout=20)
        if status != 0:
            raise DrillFailure("cleanup", "cleanup_failure")
        try:
            volumes = output.decode("utf-8", "strict").splitlines()
            if any(not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", name) for name in volumes):
                raise ValueError("invalid volume listing")
        except (UnicodeDecodeError, ValueError) as exc:
            self.record_cleanup_failure("docker_volume_inventory", "invalid_output")
            raise DrillFailure("cleanup", "cleanup_failure") from exc
        return sorted(name for name in volumes if name)

    def inspect_volume(self, name: str, phase: str) -> dict[str, Any] | None:
        try:
            if phase == "cleanup":
                status, output = self.cleanup_call(["volume", "inspect", name], timeout=20)
                if status != 0:
                    return None
            else:
                output = self.call(["volume", "inspect", name], phase=phase, category="cleanup_failure")
            documents = json.loads(output)
        except (DrillFailure, json.JSONDecodeError):
            return None
        if not isinstance(documents, list) or len(documents) != 1 or not isinstance(documents[0], dict):
            return None
        document = documents[0]
        if (
            document.get("Name") != name
            or document.get("Driver") != "local"
            or document.get("Scope") != "local"
            or document.get("Labels") not in (None, {})
            or document.get("Options") not in (None, {})
            or not isinstance(document.get("Mountpoint"), str)
            or not document["Mountpoint"].startswith("/")
            or not isinstance(document.get("CreatedAt"), str)
        ):
            return None
        return {
            "name": name,
            "driver": document["Driver"],
            "scope": document["Scope"],
            "labels": document.get("Labels") or {},
            "options": document.get("Options") or {},
            "mountpoint": document["Mountpoint"],
            "created_at": document["CreatedAt"],
        }

    def cleanup_container_inspect(self, identifier: str) -> dict[str, Any] | None:
        status, output = self.cleanup_call(
            ["inspect", "--type", "container", identifier],
            timeout=20,
        )
        if status != 0:
            return None
        try:
            document = json.loads(output)
            if not isinstance(document, list) or len(document) != 1 or not isinstance(document[0], dict):
                return None
            return document[0]
        except (json.JSONDecodeError, UnicodeDecodeError):
            return None

    def cleanup_image_inventory(self) -> list[tuple[str, str]]:
        status, output = self.cleanup_call(
            ["image", "ls", "--all", "--no-trunc", "--format", "{{.ID}} {{.Repository}}:{{.Tag}}"],
            timeout=30,
        )
        if status != 0:
            raise DrillFailure("cleanup", "cleanup_failure")
        result: list[tuple[str, str]] = []
        try:
            for line in output.decode("utf-8", "strict").splitlines():
                fields = line.split(" ", 1)
                if len(fields) != 2 or not re.fullmatch(r"sha256:[0-9a-f]{64}", fields[0]):
                    raise ValueError("invalid image listing")
                result.append((fields[0], fields[1]))
        except (UnicodeDecodeError, ValueError) as exc:
            self.record_cleanup_failure("docker_image_inventory", "invalid_output")
            raise DrillFailure("cleanup", "cleanup_failure") from exc
        return result

    def cleanup_image_inspect(self, identifier: str) -> dict[str, Any] | None:
        status, output = self.cleanup_call(["image", "inspect", identifier], timeout=20)
        if status != 0:
            return None
        try:
            document = json.loads(output)
            if not isinstance(document, list) or len(document) != 1 or not isinstance(document[0], dict):
                return None
            return document[0]
        except (json.JSONDecodeError, UnicodeDecodeError):
            return None

    def cleanup_recover_pending_container(self, containers: list[tuple[str, str]]) -> bool:
        pending = self.manifest.get("pending_container")
        if not pending:
            return True
        if any(item.get("name") == pending.get("name") for item in pending.get("containers_before", [])):
            return False
        matches = [(identifier, name) for identifier, name in containers if name == pending.get("name")]
        if not matches:
            self.manifest["pending_container"] = None
            self.save()
            return True
        if len(matches) != 1:
            return False
        identifier, _ = matches[0]
        document = self.cleanup_container_inspect(identifier)
        if document is None:
            return False
        recovered = dict(pending)
        recovered["id"] = identifier
        try:
            current_volumes = set(self.cleanup_volume_inventory())
        except DrillFailure:
            return False
        before_volumes = set(recovered.get("volumes_before", []))
        created_volumes = current_volumes - before_volumes
        observed_volume_mounts = sorted(
            (
                {
                    "type": "volume",
                    "name": mount.get("Name"),
                    "source": mount.get("Source"),
                    "destination": mount.get("Destination"),
                    "rw": bool(mount.get("RW")),
                }
                for mount in document.get("Mounts", [])
                if mount.get("Type") == "volume"
            ),
            key=lambda item: item["destination"],
        )
        attached_created_volumes = {
            mount["name"] for mount in observed_volume_mounts if mount["name"] in created_volumes
        }
        recovered["unexpected_volumes"] = sorted(attached_created_volumes)
        recovered["unexpected_volume_mounts"] = [
            mount for mount in observed_volume_mounts if mount["name"] in attached_created_volumes
        ]
        recovered["unexpected_volume_records"] = [
            volume for name in recovered["unexpected_volumes"]
            if (volume := self.inspect_volume(name, "cleanup")) is not None
        ]
        if set(recovered["unexpected_volumes"]) != attached_created_volumes:
            return False
        if len(recovered["unexpected_volume_records"]) != len(attached_created_volumes):
            return False
        if not validate_container_inspect(document, recovered):
            return False
        self.manifest["containers"].append(recovered)
        self.manifest["pending_container"] = None
        self.save()
        return True

    def cleanup_recover_pending_image(self, images: list[tuple[str, str]]) -> bool:
        pending = self.manifest.get("pending_image")
        if not pending:
            return True
        if pending.get("tag") in pending.get("before", []):
            return False
        matches = [(identifier, tag) for identifier, tag in images if tag == pending.get("tag")]
        if not matches:
            self.manifest["pending_image"] = None
            self.save()
            return True
        if len(matches) != 1:
            return False
        identifier, tag = matches[0]
        document = self.cleanup_image_inspect(identifier)
        if document is None:
            return False
        config = document.get("Config") or {}
        labels = config.get("Labels") or {}
        if (
            document.get("Id") != identifier
            or (document.get("RepoTags") or []) != [tag]
            or labels.get(IMAGE_TOKEN_LABEL) != pending.get("token")
            or labels.get(IMAGE_ROLE_LABEL) != pending.get("role")
            or config.get("User", "") != pending.get("user", "")
            or config.get("Volumes") not in (None, {})
        ):
            return False
        recovered = {
            "role": pending["role"],
            "tag": tag,
            "token": pending["token"],
            "id": identifier,
            "user": pending["user"],
        }
        self.manifest["images"].append(recovered)
        self.manifest["pending_image"] = None
        self.save()
        return True

    def cleanup_owned_containers(self) -> bool:
        try:
            containers = self.cleanup_container_inventory()
        except DrillFailure:
            return False
        if not self.cleanup_recover_pending_container(containers):
            return False
        clean = True
        for record in list(self.manifest.get("containers", [])):
            if not self.cleanup_budget_available():
                return False
            if not re.fullmatch(r"[0-9a-f]{64}", record.get("id", "")):
                clean = False
                continue
            current = self.cleanup_container_inventory()
            by_id = {identifier: name for identifier, name in current}
            matching_name = [identifier for identifier, name in current if name == record["name"]]
            if record["id"] not in by_id:
                if matching_name:
                    clean = False
                continue
            if by_id[record["id"]] != record["name"]:
                clean = False
                continue
            document = self.cleanup_container_inspect(record["id"])
            if document is None or not validate_container_inspect(document, record):
                clean = False
                continue
            if (document.get("State") or {}).get("Running"):
                status, _ = self.cleanup_call(["stop", "--time", "10", record["id"]], timeout=20)
                if status != 0:
                    clean = False
                    continue
                document = self.cleanup_container_inspect(record["id"])
                if document is None or not validate_container_inspect(document, record):
                    clean = False
                    continue
            # Re-attest the immutable ID, configuration, and attachments immediately before removal.
            document = self.cleanup_container_inspect(record["id"])
            if document is None or not validate_container_inspect(document, record):
                clean = False
                continue
            status, _ = self.cleanup_call(["rm", record["id"]], timeout=30)
            if status != 0:
                clean = False
                continue
            after = self.cleanup_container_inventory()
            if any(identifier == record["id"] or name == record["name"] for identifier, name in after):
                clean = False
        try:
            remaining = self.cleanup_container_inventory()
        except DrillFailure:
            return False
        own_ids = {record.get("id") for record in self.manifest.get("containers", [])}
        own_names = {record.get("name") for record in self.manifest.get("containers", [])}
        if any(identifier in own_ids or name in own_names for identifier, name in remaining):
            clean = False
        baseline = self.manifest.get("baseline", {}).get("containers", [])
        if sorted([list(item) for item in remaining]) != sorted(baseline):
            clean = False
        return clean

    def cleanup_owned_images(self) -> bool:
        try:
            images = self.cleanup_image_inventory()
        except DrillFailure:
            return False
        if not self.cleanup_recover_pending_image(images):
            return False
        clean = True
        for record in list(self.manifest.get("images", [])):
            if not self.cleanup_budget_available():
                return False
            identifier = record.get("id", "")
            if not re.fullmatch(r"sha256:[0-9a-f]{64}", identifier):
                clean = False
                continue
            current = self.cleanup_image_inventory()
            if not any(image_id == identifier for image_id, _ in current):
                continue
            document = self.cleanup_image_inspect(identifier)
            if document is None:
                clean = False
                continue
            config = document.get("Config") or {}
            labels = config.get("Labels") or {}
            if (
                document.get("Id") != identifier
                or (document.get("RepoTags") or []) != [record["tag"]]
                or labels.get(IMAGE_TOKEN_LABEL) != record["token"]
                or labels.get(IMAGE_ROLE_LABEL) != record["role"]
                or config.get("User", "") != record["user"]
                or config.get("Volumes") not in (None, {})
            ):
                clean = False
                continue
            status, in_use = self.cleanup_call(
                ["ps", "--all", "--no-trunc", "--filter", f"ancestor={identifier}", "--format", "{{.ID}}"],
                timeout=20,
            )
            if status != 0 or in_use.strip():
                clean = False
                continue
            # Re-attest the tagged image immediately before removing only this unique tag.
            document = self.cleanup_image_inspect(identifier)
            if document is None or (document.get("RepoTags") or []) != [record["tag"]]:
                clean = False
                continue
            config = document.get("Config") or {}
            labels = config.get("Labels") or {}
            if labels.get(IMAGE_TOKEN_LABEL) != record["token"] or labels.get(IMAGE_ROLE_LABEL) != record["role"]:
                clean = False
                continue
            status, _ = self.cleanup_call(["image", "rm", record["tag"]], timeout=30)
            if status != 0:
                clean = False
                continue
            after = self.cleanup_image_inventory()
            if any(image_id == identifier or tag == record["tag"] for image_id, tag in after):
                clean = False
        return clean

    def cleanup_networks_and_volumes(self) -> bool:
        baseline = self.manifest.get("baseline", {})
        clean = True
        try:
            volumes = self.cleanup_volume_inventory()
        except DrillFailure:
            return False
        pending = self.manifest.get("pending_container") or {}
        volume_records = self.manifest.get("containers", []) + ([pending] if pending else [])
        expected_created = manifest_owned_volume_names(volume_records)
        baseline_volumes = set(baseline.get("volumes", []))
        created_now = set(volumes) - baseline_volumes
        if created_now != expected_created:
            clean = False
        for name in sorted(expected_created & created_now):
            if not self.cleanup_budget_available():
                return False
            volume_record = next(
                (
                    item
                    for record in self.manifest.get("containers", []) + ([pending] if pending else [])
                    for item in record.get("unexpected_volume_records", [])
                    if item.get("name") == name
                ),
                None,
            )
            actual = self.inspect_volume(name, "cleanup")
            if volume_record is None or actual != volume_record:
                clean = False
                continue
            status, output = self.cleanup_call(
                ["ps", "--all", "--no-trunc", "--filter", f"volume={name}", "--format", "{{.ID}}"],
                timeout=20,
            )
            if status != 0 or output.strip():
                clean = False
                continue
            if self.inspect_volume(name, "cleanup") != volume_record:
                clean = False
                continue
            status, _ = self.cleanup_call(["volume", "rm", name], timeout=30)
            if status != 0:
                clean = False
                continue
            try:
                if name in self.cleanup_volume_inventory():
                    clean = False
            except DrillFailure:
                clean = False
        try:
            remaining_volumes = self.cleanup_volume_inventory()
        except DrillFailure:
            return False
        if remaining_volumes != baseline.get("volumes", []):
            clean = False
        status, output = self.cleanup_call(["network", "ls", "--format", "{{.Name}}"], timeout=20)
        if status != 0:
            clean = False
        else:
            try:
                networks = sorted(line for line in output.decode("utf-8", "strict").splitlines() if line)
            except UnicodeDecodeError:
                return False
            if networks != baseline.get("networks", []):
                clean = False
        return clean

    def owned_processes(self) -> tuple[list[tuple[int, int]], bool]:
        owned: list[tuple[int, int]] = []
        unknown = False
        supervisor_pid = int(self.manifest.get("supervisor", {}).get("pid", 0) or 0)
        parent_pid = int(self.manifest.get("parent", {}).get("pid", 0) or 0)
        for record in self.manifest.get("processes", []):
            pid = record.get("pid")
            ticks = record.get("start_ticks")
            if not isinstance(pid, int) or not isinstance(ticks, int) or pid <= 1 or ticks <= 0:
                unknown = True
                continue
            if pid in {os.getpid(), supervisor_pid, parent_pid}:
                unknown = True
                continue
            current_ticks = process_start_ticks(pid)
            if current_ticks == ticks:
                owned.append((pid, ticks))
            elif current_ticks is None and process_is_alive(pid) is not False:
                unknown = True

        markers = [self.manifest.get("token", "").encode(), str(self.run_dir).encode()]
        markers.extend(record.get("id", "").encode() for record in self.manifest.get("containers", []))
        pending = self.manifest.get("pending_container") or {}
        if pending.get("name"):
            markers.append(pending["name"].encode())
        markers.extend(record.get("tag", "").encode() for record in self.manifest.get("images", []))
        pending_image = self.manifest.get("pending_image") or {}
        if pending_image.get("tag"):
            markers.append(pending_image["tag"].encode())
        known = set(owned)
        supervisor_pid = int(self.manifest.get("supervisor", {}).get("pid", 0) or 0)
        parent_pid = int(self.manifest.get("parent", {}).get("pid", 0) or 0)
        for pid in docker_cli_pids():
            if pid in {os.getpid(), supervisor_pid, parent_pid}:
                continue
            ticks = process_start_ticks(pid)
            if ticks is None:
                if process_is_alive(pid) is not False:
                    unknown = True
                continue
            if (pid, ticks) in known:
                continue
            try:
                command = Path(f"/proc/{pid}/cmdline").read_bytes()
            except FileNotFoundError:
                continue
            except OSError:
                unknown = True
                continue
            if any(marker and marker in command for marker in markers):
                owned.append((pid, ticks))
        return sorted(set(owned)), unknown

    def cleanup_processes(self) -> bool:
        owned, unknown = self.owned_processes()
        for pid, start_ticks in owned:
            if process_start_ticks(pid) != start_ticks:
                continue
            try:
                os.kill(pid, signal.SIGTERM)
            except ProcessLookupError:
                continue
            except PermissionError:
                unknown = True
                continue
        deadline = min(
            time.monotonic() + 5,
            self.manifest["cleanup_deadline"] - CLEANUP_FINALIZE_SECONDS,
        )
        while time.monotonic() < deadline:
            alive = [(pid, ticks) for pid, ticks in owned if process_start_ticks(pid) == ticks]
            if not alive:
                break
            time.sleep(min(0.1, deadline - time.monotonic()))
        for pid, start_ticks in owned:
            if process_start_ticks(pid) != start_ticks:
                continue
            try:
                os.kill(pid, signal.SIGKILL)
            except ProcessLookupError:
                continue
            except PermissionError:
                unknown = True
        alive = any(process_start_ticks(pid) == ticks for pid, ticks in owned)
        return not unknown and not alive

    def mounted_sources(self) -> set[str] | None:
        if not self.manifest.get("resource_creation_started"):
            if self.manifest.get("containers") or self.manifest.get("images"):
                return None
            return set()
        try:
            containers = self.docker_resource_inventory("cleanup")["containers"]
        except (DrillFailure, KeyError, TypeError, ValueError):
            return None
        if not isinstance(containers, dict):
            return None
        known = {record.get("id"): record.get("name") for record in self.manifest.get("containers", [])}
        pending = self.manifest.get("pending_container") or {}
        if pending.get("name"):
            known[pending.get("id") or ""] = pending["name"]
        known_names = set(known.values())
        try:
            run_dir = self.run_dir.resolve()
        except (OSError, RuntimeError):
            return None
        retained: set[str] = set()
        for identifier, record in containers.items():
            if not isinstance(record, dict):
                return None
            name = record.get("name")
            if identifier in known and name != known[identifier]:
                return None
            if identifier not in known and name in known_names:
                return None
            mounts = record.get("mounts")
            if not isinstance(mounts, list):
                return None
            for mount in mounts:
                if not isinstance(mount, dict):
                    return None
                source = mount.get("Source")
                if source is not None and not isinstance(source, str):
                    return None
                if source:
                    source_path = Path(source)
                    if not source_path.is_absolute():
                        return None
                    try:
                        source_path = source_path.resolve()
                    except (OSError, RuntimeError):
                        return None
                    # Keep unrelated workloads, but refuse mounts that expose or retain this private run tree.
                    if source_path == run_dir or source_path in run_dir.parents or run_dir in source_path.parents:
                        return None
                    retained.add(str(source_path))
        return {path for path in retained if path}

    def remove_unmounted_files(self, *, keep_all: bool) -> bool:
        if not self.cleanup_budget_available():
            return False
        retained = self.mounted_sources()
        if retained is None:
            keep_all = True
            retained = set()
        if keep_all:
            return False
        clean = True
        root_device = self.run_dir.stat().st_dev
        retained_paths = {Path(item) for item in retained}
        for record in self.manifest.get("files", []):
            if not self.cleanup_budget_available():
                return False
            path = Path(record["path"])
            if any(path == mount or mount in path.parents for mount in retained_paths):
                continue
            try:
                info = path.lstat()
            except FileNotFoundError:
                continue
            except OSError:
                clean = False
                continue
            if (
                not stat.S_ISREG(info.st_mode)
                or info.st_dev != record["device"]
                or info.st_ino != record["inode"]
                or stat.S_IMODE(info.st_mode) != record["mode"]
                or info.st_uid != record["uid"]
                or info.st_gid != record["gid"]
                or info.st_nlink != 1
            ):
                clean = False
                continue
            try:
                os.unlink(path)
            except OSError:
                clean = False
        preserve = {self.manifest_path, self.manifest_path.with_suffix(".tmp")}
        for current, directories, files in os.walk(self.run_dir, topdown=False, followlinks=False):
            if not self.cleanup_budget_available():
                return False
            current_path = Path(current)
            for name in files:
                candidate = current_path / name
                if candidate in preserve or any(candidate == item or item in candidate.parents for item in retained_paths):
                    continue
                try:
                    info = candidate.lstat()
                    if stat.S_ISREG(info.st_mode) and info.st_dev == root_device and info.st_nlink == 1:
                        os.unlink(candidate)
                    elif stat.S_ISLNK(info.st_mode):
                        clean = False
                        os.unlink(candidate)
                    else:
                        clean = False
                except FileNotFoundError:
                    continue
                except OSError:
                    clean = False
            for name in directories:
                candidate = current_path / name
                if any(candidate == item or item in candidate.parents for item in retained_paths):
                    continue
                try:
                    info = candidate.lstat()
                    if stat.S_ISLNK(info.st_mode):
                        clean = False
                        os.unlink(candidate)
                    elif stat.S_ISDIR(info.st_mode) and info.st_dev == root_device:
                        candidate.rmdir()
                    else:
                        clean = False
                except FileNotFoundError:
                    continue
                except OSError:
                    clean = False
        for record in self.manifest.get("files", []):
            path = Path(record["path"])
            if any(path == item or item in path.parents for item in retained_paths):
                continue
            try:
                path.lstat()
                clean = False
            except FileNotFoundError:
                pass
            except OSError:
                clean = False
        return clean

    def remove_build_inputs(self) -> bool:
        if not self.cleanup_budget_available():
            return False
        record = self.manifest.get("build_directory")
        if record is None:
            return True
        path = Path(record.get("path", ""))
        clean = True
        for item in self.manifest.get("files", []):
            if not self.cleanup_budget_available():
                return False
            if not item.get("purpose", "").startswith("build_input_"):
                continue
            file_path = Path(item["path"])
            try:
                info = file_path.lstat()
            except FileNotFoundError:
                continue
            except OSError:
                clean = False
                continue
            if (
                not stat.S_ISREG(info.st_mode)
                or info.st_dev != item["device"]
                or info.st_ino != item["inode"]
                or stat.S_IMODE(info.st_mode) != item["mode"]
                or info.st_uid != item["uid"]
                or info.st_gid != item["gid"]
                or info.st_nlink != 1
            ):
                clean = False
                continue
            try:
                file_path.unlink()
            except OSError:
                clean = False
        try:
            directory_info = path.lstat()
            if (
                not stat.S_ISDIR(directory_info.st_mode)
                or directory_info.st_dev != record["device"]
                or directory_info.st_ino != record["inode"]
                or stat.S_IMODE(directory_info.st_mode) != record["mode"]
                or directory_info.st_uid != record["uid"]
                or directory_info.st_gid != record["gid"]
            ):
                return False
            if any(path.iterdir()):
                return False
            path.rmdir()
        except FileNotFoundError:
            pass
        except OSError:
            clean = False
        if path.exists():
            clean = False
        if clean:
            self.manifest["build_directory"] = None
        self.save()
        return clean

    def cleanup_resources(self) -> bool:
        self.cleanup_active = True
        self.active_cleanup_operation = "cleanup_resources"
        if not self.cleanup_budget_available():
            self.record_cleanup_failure("cleanup_deadline", "deadline_expired")
            self.manifest["cleanup_verified"] = False
            self.save()
            return False
        clean = True
        failed_steps: list[str] = []

        def record_step(name: str, outcome: bool, operation: str) -> bool:
            if not outcome:
                failed_steps.append(name)
                self.record_cleanup_failure(operation, "verification_failed")
            return outcome

        if self.gateway is not None:
            self.active_cleanup_operation = "gateway_shutdown"
            gateway_clean = self.gateway.close(self.manifest["cleanup_deadline"])
            clean = record_step("gateway", gateway_clean, "gateway_shutdown") and clean
            self.gateway = None
        if not self.cleanup_budget_available():
            self.record_cleanup_failure("cleanup_deadline", "deadline_expired")
            self.manifest["cleanup_verified"] = False
            self.save()
            return False
        self.active_cleanup_operation = "cleanup_processes"
        processes_clean = self.cleanup_processes()
        clean = record_step("processes", processes_clean, "cleanup_processes") and clean
        if not self.cleanup_budget_available():
            self.record_cleanup_failure("cleanup_deadline", "deadline_expired")
            self.manifest["cleanup_verified"] = False
            self.save()
            return False
        self.active_cleanup_operation = "build_input_removal"
        build_inputs_clean = self.remove_build_inputs()
        clean = record_step("build-inputs", build_inputs_clean, "build_input_removal") and clean
        if not self.cleanup_budget_available():
            self.record_cleanup_failure("cleanup_deadline", "deadline_expired")
            self.manifest["cleanup_verified"] = False
            self.save()
            return False
        if not self.manifest.get("resource_creation_started"):
            self.active_cleanup_operation = "run_file_removal"
            files_clean = self.remove_unmounted_files(keep_all=False)
            clean = record_step("files", files_clean, "run_file_removal") and clean
            self.active_cleanup_operation = "cleanup_resources"
            inventory_clean = self.verify_foreign_inventory()
            clean = record_step("foreign-inventory", inventory_clean, "cleanup_resources") and clean
            self.manifest["cleanup_verified"] = bool(clean)
            self.manifest["cleanup_failures"] = failed_steps
            if not clean and self.cleanup_failure is None:
                self.record_cleanup_failure("cleanup_resources", "verification_failed")
            self.save()
            if failed_steps:
                print("restore readiness cleanup failed: " + ",".join(failed_steps), file=sys.stderr)
            return clean
        self.active_cleanup_operation = "owned_container_cleanup"
        containers_clean = self.cleanup_owned_containers()
        clean = record_step("containers", containers_clean, "owned_container_cleanup") and clean
        if not self.cleanup_budget_available():
            self.record_cleanup_failure("cleanup_deadline", "deadline_expired")
            self.manifest["cleanup_verified"] = False
            self.save()
            return False
        self.active_cleanup_operation = "owned_image_cleanup"
        images_clean = self.cleanup_owned_images()
        clean = record_step("images", images_clean, "owned_image_cleanup") and clean
        if not self.cleanup_budget_available():
            self.record_cleanup_failure("cleanup_deadline", "deadline_expired")
            self.manifest["cleanup_verified"] = False
            self.save()
            return False
        self.active_cleanup_operation = "network_volume_cleanup"
        network_volumes_clean = self.cleanup_networks_and_volumes()
        clean = record_step("networks-and-volumes", network_volumes_clean, "network_volume_cleanup") and clean
        if not self.cleanup_budget_available():
            self.record_cleanup_failure("cleanup_deadline", "deadline_expired")
            self.manifest["cleanup_verified"] = False
            self.save()
            return False
        self.active_cleanup_operation = "run_file_removal"
        files_clean = self.remove_unmounted_files(keep_all=not containers_clean)
        clean = record_step("files", files_clean, "run_file_removal") and clean
        self.active_cleanup_operation = "cleanup_resources"
        inventory_clean = self.verify_foreign_inventory()
        clean = record_step("foreign-inventory", inventory_clean, "cleanup_resources") and clean
        self.manifest["cleanup_verified"] = bool(clean)
        self.manifest["cleanup_failures"] = failed_steps
        if not clean and self.cleanup_failure is None:
            self.record_cleanup_failure("cleanup_resources", "verification_failed")
        self.save()
        if failed_steps:
            print("restore readiness cleanup failed: " + ",".join(failed_steps), file=sys.stderr)
        return clean

    def local_command(self, args: list[str], phase: str, timeout: float = 30) -> bytes:
        status, output = self._run_child(
            args,
            timeout=timeout,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            environment={"HOME": "/root", "LANG": "C", "LC_ALL": "C", "PATH": "/usr/bin:/bin"},
            phase=phase,
            category="fixture_failure",
        )
        if status != 0 or len(output) > MAX_CAPTURE:
            raise DrillFailure(phase, "fixture_failure")
        return output

    def generate_rsa_key(self) -> tuple[Path, Path]:
        if self.phase == "fixture_generation":
            self.fixture_check = "prepare_materials"
        key_path = self.run_dir / "secrets" / "server-key.pem"
        try:
            status, _ = self._run_child(
                [
                    "/usr/bin/openssl",
                    "genrsa",
                    # The server loads only PKCS#1 keys.
                    "-traditional",
                    "-out",
                    str(key_path),
                    "2048",
                ],
                timeout=20,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                environment={"HOME": "/root", "LANG": "C", "LC_ALL": "C", "PATH": "/usr/bin:/bin"},
                phase=self.phase,
                category="fixture_failure",
            )
            if status != 0:
                raise DrillFailure(self.phase, "fixture_failure")
        except BaseException:
            try:
                key_path.unlink()
            except FileNotFoundError:
                pass
            raise
        os.chown(key_path, 10001, 70)
        os.chmod(key_path, 0o400)
        if key_path.stat().st_size > 8192:
            raise DrillFailure(self.phase, "fixture_failure")
        self.register_file(key_path, "server_rsa_private_key")
        public_key = self.local_command(
            ["/usr/bin/openssl", "pkey", "-in", str(key_path), "-pubout"],
            self.phase,
            timeout=20,
        )
        if b"BEGIN PUBLIC KEY" not in public_key or len(public_key) > 4096:
            raise DrillFailure(self.phase, "fixture_failure")
        public_path = self.private_write(
            self.run_dir / "invocation" / "server.pub.pem",
            public_key,
            mode=0o400,
            purpose="server_rsa_public_key",
        )
        return key_path, public_path

    def prepare_materials(self) -> None:
        self.phase = "fixture_generation"
        self.fixture_check = "prepare_materials"
        source_db_password = secrets.token_hex(32)
        target_db_password = secrets.token_hex(32)
        self.target_db_password = target_db_password
        source_dsn = database_dsn(source_db_password)
        target_dsn = database_dsn(target_db_password)
        auth_key = secrets.token_hex(32)
        seed_username = f"fixture_{secrets.token_hex(8)}"
        seed_password = secrets.token_hex(32)
        checker_username = f"fixture_{secrets.token_hex(8)}"
        while checker_username == seed_username:
            checker_username = f"fixture_{secrets.token_hex(8)}"
        checker_password = secrets.token_hex(32)

        self.files = {
            "source_init_password": self.run_dir / "secrets" / "source-init-password",
            "target_init_password": self.run_dir / "secrets" / "target-init-password",
            "source_database_env": self.run_dir / "secrets" / "source-postgres.env",
            "target_database_env": self.run_dir / "secrets" / "target-postgres.env",
            "source_tool_env": self.run_dir / "secrets" / "source-client.env",
            "target_tool_env": self.run_dir / "secrets" / "target-client.env",
            "source_atlas_env": self.run_dir / "secrets" / "source-atlas.env",
            "target_atlas_env": self.run_dir / "secrets" / "target-atlas.env",
            "source_dsn": self.run_dir / "seed" / "postgres.dsn",
            "target_dsn_app": self.run_dir / "secrets" / "app-postgres.dsn",
            "target_dsn_provisioner": self.run_dir / "secrets" / "checker-provisioner.dsn",
            "seed_authkey": self.run_dir / "seed" / "authkey.hex",
            "app_authkey": self.run_dir / "secrets" / "app-authkey.hex",
            "checker_authkey": self.run_dir / "invocation" / "authkey.hex",
            "seed_username": self.run_dir / "seed" / "username",
            "seed_password": self.run_dir / "seed" / "password",
            "checker_username": self.run_dir / "invocation" / "username",
            "checker_password": self.run_dir / "invocation" / "password",
            "server_key": self.run_dir / "secrets" / "server-key.pem",
            "server_public": self.run_dir / "invocation" / "server.pub.pem",
        }
        self.private_write(
            self.files["source_init_password"],
            (source_db_password + "\n").encode(),
            mode=0o400,
            owner=(70, 70),
            purpose="source_postgres_init_secret",
        )
        self.private_write(
            self.files["target_init_password"],
            (target_db_password + "\n").encode(),
            mode=0o400,
            owner=(70, 70),
            purpose="target_postgres_init_secret",
        )
        self.private_write(self.files["source_dsn"], (source_dsn + "\n").encode(), mode=0o400, purpose="source_dsn")
        self.private_write(
            self.files["target_dsn_provisioner"],
            (database_tcp_dsn(target_db_password) + "\n").encode(),
            mode=0o400,
            purpose="target_dsn_provisioner",
        )
        self.private_write(
            self.files["target_dsn_app"],
            (target_dsn + "\n").encode(),
            mode=0o400,
            owner=(10001, 70),
            purpose="application_postgres_dsn",
        )
        self.private_write(self.files["seed_authkey"], (auth_key + "\n").encode(), mode=0o400, purpose="seed_authkey")
        self.private_write(
            self.files["app_authkey"],
            (auth_key + "\n").encode(),
            mode=0o400,
            owner=(10001, 70),
            purpose="application_authkey",
        )
        self.private_write(
            self.files["checker_authkey"],
            (auth_key + "\n").encode(),
            mode=0o400,
            purpose="checker_authkey",
        )
        for name, value in (
            ("seed_username", seed_username),
            ("seed_password", seed_password),
            ("checker_username", checker_username),
            ("checker_password", checker_password),
        ):
            mode = 0o400
            content = value if name == "seed_password" else value + "\n"
            self.private_write(self.files[name], content.encode(), mode=mode, purpose=name)

        source_pg_env = {
            "PGHOST": "/run/postgres",
            "PGUSER": "postgres",
            "PGDATABASE": "teagram",
            "PGPASSWORD": source_db_password,
        }
        target_pg_env = {
            "PGHOST": "/run/postgres",
            "PGUSER": "postgres",
            "PGDATABASE": "teagram",
            "PGPASSWORD": target_db_password,
        }
        for name, values in (
            ("source_tool_env", source_pg_env),
            ("target_tool_env", target_pg_env),
            ("source_database_env", {
                "PGDATA": "/var/lib/postgresql/18/docker",
                "POSTGRES_DB": "teagram",
                "POSTGRES_USER": RESTORE_BOOTSTRAP_ROLE,
                "POSTGRES_PASSWORD_FILE": "/run/secrets/init-password",
                "POSTGRES_HOST_AUTH_METHOD": "scram-sha-256",
            }),
            ("target_database_env", {
                "PGDATA": "/var/lib/postgresql/18/docker",
                "POSTGRES_DB": "teagram",
                "POSTGRES_USER": RESTORE_BOOTSTRAP_ROLE,
                "POSTGRES_PASSWORD_FILE": "/run/secrets/init-password",
                "POSTGRES_HOST_AUTH_METHOD": "scram-sha-256",
            }),
            ("source_atlas_env", {"DATABASE_URL": source_dsn}),
            ("target_atlas_env", {"DATABASE_URL": target_dsn}),
        ):
            content = ("\n".join(f"{key}={value}" for key, value in values.items()) + "\n").encode()
            self.private_write(self.files[name], content, mode=0o600, purpose=name)
        server_key, server_public = self.generate_rsa_key()
        if server_key != self.files["server_key"]:
            raise DrillFailure(self.phase, "fixture_failure")
        if server_public != self.files["server_public"]:
            raise DrillFailure(self.phase, "fixture_failure")


def open_global_lock() -> tuple[int, dict[str, Any]] | None:
    path = Path("/run/restore-readiness.lock")
    try:
        descriptor = os.open(
            path,
            os.O_RDWR | os.O_CREAT | os.O_CLOEXEC | os.O_NOFOLLOW,
            0o600,
        )
        os.fchown(descriptor, 0, 0)
        os.fchmod(descriptor, 0o600)
        info = os.fstat(descriptor)
        path_info = path.lstat()
        if (
            not stat.S_ISREG(info.st_mode)
            or info.st_uid != 0
            or stat.S_IMODE(info.st_mode) != 0o600
            or info.st_nlink != 1
            or path_info.st_dev != info.st_dev
            or path_info.st_ino != info.st_ino
        ):
            os.close(descriptor)
            return None
        fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return descriptor, {"path": str(path), "device": info.st_dev, "inode": info.st_ino}
    except (OSError, BlockingIOError):
        try:
            os.close(descriptor)
        except (UnboundLocalError, OSError):
            pass
        return None


def open_rollout_lock(
    path: Path = ROLLOUT_LOCK_PATH,
    *,
    expected_uid: int = 0,
) -> tuple[int, dict[str, Any]] | None:
    descriptor: int | None = None
    try:
        path = Path(path)
        if path.parent != Path("/tmp") or expected_uid < 0:
            return None
        descriptor = os.open(
            path,
            os.O_RDWR | os.O_CREAT | os.O_CLOEXEC | os.O_NOFOLLOW,
            0o600,
        )
        info = os.fstat(descriptor)
        path_info = path.lstat()
        parent_info = path.parent.lstat()
        if (
            not stat.S_ISDIR(parent_info.st_mode)
            or parent_info.st_uid != 0
            or stat.S_IMODE(parent_info.st_mode) != 0o1777
            or not stat.S_ISREG(info.st_mode)
            or info.st_uid != expected_uid
            or stat.S_IMODE(info.st_mode) != 0o600
            or info.st_nlink != 1
            or path_info.st_dev != info.st_dev
            or path_info.st_ino != info.st_ino
        ):
            os.close(descriptor)
            return None
        fcntl.flock(descriptor, fcntl.LOCK_SH | fcntl.LOCK_NB)
        path_info = path.lstat()
        if path_info.st_dev != info.st_dev or path_info.st_ino != info.st_ino:
            os.close(descriptor)
            return None
        return descriptor, {"path": str(path), "device": info.st_dev, "inode": info.st_ino}
    except (OSError, BlockingIOError):
        if descriptor is not None:
            try:
                os.close(descriptor)
            except OSError:
                pass
        return None


def teardown_prepared_runtime(directory: Path, build_uid: int) -> bool:
    if os.geteuid() != 0 or build_uid <= 0:
        return False
    try:
        path = prepared_directory_path(directory)
        try:
            directory_info = path.lstat()
        except FileNotFoundError:
            return True
        if (
            path.is_symlink()
            or not stat.S_ISDIR(directory_info.st_mode)
            or directory_info.st_uid not in {0, build_uid}
            or stat.S_IMODE(directory_info.st_mode) not in {0o500, 0o700}
        ):
            return False
        lock = open_global_lock()
        if lock is None:
            return False
        lock_fd, _ = lock
        parent_fd: int | None = None
        try:
            parent_fd = os.open("/tmp", os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW)
            directory_fd = os.open(path.name, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW, dir_fd=parent_fd)
            try:
                opened_info = os.fstat(directory_fd)
                if opened_info.st_dev != directory_info.st_dev or opened_info.st_ino != directory_info.st_ino:
                    return False
                entries = set(os.listdir(directory_fd))
                if not entries <= PREPARED_ARTIFACTS | {PREPARED_ATTESTATION}:
                    return False
                for name in sorted(entries):
                    descriptor = os.open(name, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW, dir_fd=directory_fd)
                    try:
                        info = os.fstat(descriptor)
                        if (
                            not stat.S_ISREG(info.st_mode)
                            or info.st_uid not in {0, build_uid}
                            or info.st_nlink != 1
                            or info.st_mode & 0o022
                            or info.st_size > 2 * 1024 * 1024 * 1024
                        ):
                            return False
                    finally:
                        os.close(descriptor)
                    current = os.stat(name, dir_fd=directory_fd, follow_symlinks=False)
                    if current.st_dev != info.st_dev or current.st_ino != info.st_ino:
                        return False
                    os.unlink(name, dir_fd=directory_fd)
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
            os.rmdir(path.name, dir_fd=parent_fd)
            os.fsync(parent_fd)
            try:
                path.lstat()
                return False
            except FileNotFoundError:
                return True
        finally:
            if parent_fd is not None:
                os.close(parent_fd)
            os.close(lock_fd)
    except (OSError, ValueError, TypeError):
        return False


def make_runtime_args(manifest: dict[str, Any]) -> argparse.Namespace:
    runtime = manifest["runtime"]
    return argparse.Namespace(
        docker_bin=runtime["docker_bin"],
        atlas_bin=runtime["atlas_bin"],
        build_dir=runtime["build_dir"],
        build_uid=runtime["build_uid"],
        go_arch=runtime["go_arch"],
        prepared_runtime=manifest["prepared_runtime"],
    )


def remove_finished_run(drill: Drill, deadline: float | None = None) -> bool:
    if not drill.manifest.get("cleanup_verified"):
        return False

    def within_budget() -> bool:
        return deadline is None or time.monotonic() < deadline - CLEANUP_FINALIZE_SECONDS

    run_dir = drill.run_dir
    manifest_path = drill.manifest_path
    temporary = manifest_path.with_suffix(".tmp")
    expected_directories = {
        "work",
        "sockets",
        "invocation",
        "seed",
        "secrets",
        "migrations",
    }
    try:
        if not within_budget():
            return False
        root_entries = {entry.name for entry in run_dir.iterdir()}
        # Verified file cleanup may already have removed the emptied directories.
        if "ownership.json" not in root_entries or not root_entries <= expected_directories | {
            "ownership.json",
            "ownership.tmp",
        }:
            return False
        for current, directories, files in os.walk(run_dir, topdown=True, followlinks=False):
            if not within_budget():
                return False
            current_path = Path(current)
            if current_path == run_dir:
                continue
            if files:
                return False
            if current_path == run_dir / "sockets":
                if not set(directories) <= {"source", "target"}:
                    return False
            elif directories:
                return False
        if temporary.exists():
            if not within_budget():
                return False
            info = temporary.lstat()
            if (
                not stat.S_ISREG(info.st_mode)
                or info.st_uid != 0
                or stat.S_IMODE(info.st_mode) != 0o600
                or info.st_nlink != 1
                or info.st_size > 2 * 1024 * 1024
            ):
                return False
            temporary.unlink()
        for name in ("sockets/target", "sockets/source", "migrations", "secrets", "seed", "invocation", "sockets", "work"):
            if not within_budget():
                return False
            if (run_dir / name).exists():
                (run_dir / name).rmdir()
        if not within_budget():
            return False
        info = manifest_path.lstat()
        if (
            not stat.S_ISREG(info.st_mode)
            or info.st_uid != 0
            or stat.S_IMODE(info.st_mode) != 0o600
            or info.st_nlink != 1
        ):
            return False
        manifest_path.unlink()
        run_dir.rmdir()
        return not run_dir.exists()
    except (FileNotFoundError, NotADirectoryError, OSError):
        return False


def wait_for_supervisor_registration(
    manifest_path: Path,
    supervisor: subprocess.Popen[bytes],
    supervisor_ticks: int,
) -> dict[str, Any]:
    wait_until = time.monotonic() + 10
    while time.monotonic() < wait_until:
        if supervisor.poll() is not None:
            raise DrillFailure("preflight", "preflight_failure")
        try:
            manifest = load_manifest(manifest_path)
        except DrillFailure as exc:
            raise DrillFailure("preflight", "preflight_failure") from exc
        if manifest["supervisor"] == {
            "pid": supervisor.pid,
            "start_ticks": supervisor_ticks,
        }:
            return manifest
        time.sleep(0.1)
    raise DrillFailure("preflight", "preflight_failure")


def cleanup_supervisor_failure(drill: Drill | None, stage: str | None = None) -> dict[str, Any]:
    if drill is not None:
        recorded = getattr(drill, "cleanup_failure", None)
        if recorded is not None:
            return recorded
        if getattr(drill, "cleanup_active", False):
            operation = getattr(drill, "active_cleanup_operation", None)
            if operation not in SAFE_CLEANUP_OPERATIONS:
                operation = "cleanup_resources"
            return cleanup_failure_document(operation, "supervisor_failed")
    failure = cleanup_failure_document("supervisor_setup", "supervisor_failed")
    if stage is not None:
        failure = {**failure, "stage": stage}
        diagnostic = validate_cleanup_failure_document(failure)
        if diagnostic is None:
            raise ValueError("invalid supervisor failure stage")
        return diagnostic
    return failure


def supervise(manifest_path: Path, lock_fd: int, rollout_lock_fd: int) -> int:
    drill: Drill | None = None
    stage = "load_initial_manifest"
    try:
        manifest = load_manifest(manifest_path)
        stage = "verify_lock_descriptors"
        lock_info = os.fstat(lock_fd)
        rollout_info = os.fstat(rollout_lock_fd)
        if (
            lock_info.st_dev != manifest["global_lock"]["device"]
            or lock_info.st_ino != manifest["global_lock"]["inode"]
            or rollout_info.st_dev != manifest["rollout_lock"]["device"]
            or rollout_info.st_ino != manifest["rollout_lock"]["inode"]
        ):
            emit_cleanup_failure(
                cleanup_failure_document(
                    "supervisor_setup", "verification_failed", stage=stage
                )
            )
            return 1
        supervisor_pid = os.getpid()
        stage = "verify_supervisor_identity"
        supervisor_ticks = process_start_ticks(supervisor_pid)
        if supervisor_ticks is None:
            emit_cleanup_failure(
                cleanup_failure_document(
                    "supervisor_setup", "verification_failed", stage=stage
                )
            )
            return 1
        wait_until = time.monotonic() + 10
        stage = "register_supervisor"
        while time.monotonic() < wait_until:
            manifest = load_manifest(manifest_path)
            record = manifest["supervisor"]
            if record.get("pid") == 0 and record.get("start_ticks") == 0:
                manifest["supervisor"] = {"pid": supervisor_pid, "start_ticks": supervisor_ticks}
                drill = Drill(make_runtime_args(manifest), manifest_path.parent, manifest)
                drill.save()
                break
            if record.get("pid") == supervisor_pid and record.get("start_ticks") == supervisor_ticks:
                break
            time.sleep(0.1)
        else:
            emit_cleanup_failure(
                cleanup_failure_document(
                    "supervisor_setup", "verification_failed", stage=stage
                )
            )
            return 1

        parent = manifest["parent"]
        stage = "wait_for_parent"
        while True:
            try:
                manifest = load_manifest(manifest_path)
            except DrillFailure:
                if time.monotonic() < manifest["work_deadline"]:
                    # Keep the last validated snapshot while the parent is active.
                    time.sleep(0.2)
                    continue
            parent_alive = process_start_ticks(parent["pid"]) == parent["start_ticks"]
            if manifest.get("parent_done") or not parent_alive:
                break
            if time.monotonic() >= manifest["work_deadline"]:
                try:
                    os.kill(parent["pid"], signal.SIGTERM)
                except ProcessLookupError:
                    pass
                term_deadline = time.monotonic() + 15
                while time.monotonic() < term_deadline:
                    if process_start_ticks(parent["pid"]) != parent["start_ticks"]:
                        break
                    time.sleep(0.1)
                if process_start_ticks(parent["pid"]) == parent["start_ticks"]:
                    try:
                        os.kill(parent["pid"], signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    kill_deadline = time.monotonic() + 5
                    while time.monotonic() < kill_deadline:
                        if process_start_ticks(parent["pid"]) != parent["start_ticks"]:
                            break
                        time.sleep(0.1)
                break
            time.sleep(0.2)

        stage = "load_final_manifest"
        manifest = load_manifest(manifest_path)
        stage = "check_cleanup_deadline"
        if time.monotonic() >= manifest["cleanup_deadline"]:
            emit_cleanup_failure(
                cleanup_failure_document(
                    "cleanup_deadline", "deadline_expired", stage=stage
                )
            )
            return 1
        stage = "build_runtime_args"
        runtime_args = make_runtime_args(manifest)
        stage = "initialize_cleanup_drill"
        drill = Drill(runtime_args, manifest_path.parent, manifest)
        stage = "cleanup_resources"
        clean = drill.cleanup_resources()
        if not clean or time.monotonic() >= manifest["cleanup_deadline"]:
            if time.monotonic() >= manifest["cleanup_deadline"]:
                drill.record_cleanup_failure("cleanup_deadline", "deadline_expired")
            drill.manifest["cleanup_verified"] = False
            drill.save()
            emit_cleanup_failure(
                getattr(drill, "cleanup_failure", None)
                or cleanup_failure_document("cleanup_resources", "verification_failed")
            )
            return 1
        stage = "finalize_run_directory"
        drill.active_cleanup_operation = "run_directory_finalization"
        if not remove_finished_run(drill, manifest["cleanup_deadline"]):
            drill.record_cleanup_failure("run_directory_finalization", "verification_failed")
            drill.manifest["cleanup_verified"] = False
            try:
                drill.save()
            except OSError:
                pass
            emit_cleanup_failure(getattr(drill, "cleanup_failure", None))
            return 1
        return 0
    except DrillFailure:
        emit_cleanup_failure(cleanup_supervisor_failure(drill, stage))
        return 1
    except (OSError, ValueError, TypeError, KeyError, json.JSONDecodeError):
        emit_cleanup_failure(cleanup_supervisor_failure(drill, stage))
        return 1
    finally:
        try:
            os.close(lock_fd)
        except OSError:
            pass
        try:
            os.close(rollout_lock_fd)
        except OSError:
            pass


def recover(manifest_path: Path) -> int:
    if os.geteuid() != 0:
        print(json.dumps(report_document("cleanup", "cleanup_failure", "not_started", 0), separators=(",", ":")))
        return 1
    descriptor: int | None = None
    rollout_descriptor: int | None = None
    foreign_inventory_changed = False
    try:
        rollout_lock = open_rollout_lock()
        if rollout_lock is None:
            raise ValueError("deployment rollout is active")
        rollout_descriptor, rollout_record = rollout_lock
        manifest = load_manifest(manifest_path)
        baseline = manifest.get("baseline")
        if isinstance(baseline, dict):
            inventory = baseline.get("docker_inventory")
            if (
                isinstance(inventory, dict)
                and set(inventory) == {"containers", "volumes", "networks"}
            ):
                # Earlier schema-v1 runs rejected token-labeled images before saving this inventory.
                inventory["images"] = {}
        lock = open_global_lock()
        if lock is not None:
            descriptor, _ = lock
            if lock[1] == manifest["global_lock"] and rollout_record == manifest["rollout_lock"]:
                manifest["cleanup_deadline"] = time.monotonic() + CLEANUP_SECONDS
                drill = Drill(make_runtime_args(manifest), manifest_path.parent, manifest)
                drill.save()
                clean = drill.cleanup_resources()
                foreign_inventory_changed = bool(drill.manifest.get("foreign_inventory_changed"))
                if clean:
                    if remove_finished_run(drill, drill.manifest["cleanup_deadline"]):
                        print(json.dumps(report_document("cleanup", "cleanup_failure", "verified", 0), separators=(",", ":")))
                        return 1
                    drill.manifest["cleanup_verified"] = False
                    try:
                        drill.save()
                    except OSError:
                        pass
    except BaseException:
        pass
    finally:
        if descriptor is not None:
            try:
                os.close(descriptor)
            except OSError:
                pass
        if rollout_descriptor is not None:
            try:
                os.close(rollout_descriptor)
            except OSError:
                pass
    category = "isolation_refusal" if foreign_inventory_changed else "cleanup_failure"
    print(json.dumps(report_document("cleanup", category, "unverified", 0), separators=(",", ":")))
    return 1


def validate_local_dump_argument(path: Path) -> int:
    if os.geteuid() != 0:
        print("restore readiness preflight check failed: root privileges", file=sys.stderr)
        print(json.dumps(report_document("preflight", "preflight_failure", "not_started", 0), separators=(",", ":")))
        return 1
    try:
        descriptor, _ = open_local_dump(path)
        os.close(descriptor)
    except (OSError, ValueError):
        print("restore readiness preflight check failed: local dump metadata", file=sys.stderr)
        print(json.dumps(report_document("preflight", "isolation_refusal", "not_started", 0), separators=(",", ":")))
        return 1
    return 0


def run_primary(args: argparse.Namespace) -> int:
    if os.geteuid() != 0:
        print("restore readiness preflight check failed: root privileges", file=sys.stderr)
        print(json.dumps(report_document("preflight", "preflight_failure", "not_started", 0), separators=(",", ":")))
        return 1
    try:
        prepared = verify_prepared_runtime(args.prepared_dir)
    except (OSError, ValueError, TypeError, KeyError, json.JSONDecodeError, subprocess.SubprocessError):
        print("restore readiness preflight check failed: prepared runtime", file=sys.stderr)
        print(json.dumps(report_document("preflight", "preflight_failure", "not_started", 0), separators=(",", ":")))
        return 1
    args.atlas_bin = prepared["atlas_bin"]
    args.build_dir = prepared["build_dir"]
    args.build_uid = prepared["prepared_uid"]
    args.go_arch = prepared["go_arch"]
    args.prepared_runtime = {
        key: prepared[key]
        for key in ("path", "device", "inode", "attestation_sha256", "prepared_uid", "go_arch")
    }
    try:
        args.docker_bin = ensure_trusted_system_executable(args.docker_bin)
    except DrillFailure:
        print("restore readiness preflight check failed: trusted Docker executable", file=sys.stderr)
        print(json.dumps(report_document("preflight", "preflight_failure", "not_started", 0), separators=(",", ":")))
        return 1
    try:
        work_seconds = bounded_work_seconds(
            args.fault_test,
            args.work_seconds,
            local_fixture_test=args.local_fixture_test,
        )
    except ValueError:
        print("restore readiness preflight check failed: work budget", file=sys.stderr)
        print(json.dumps(report_document("preflight", "preflight_failure", "not_started", 0), separators=(",", ":")))
        return 1
    local_dump_fd: int | None = None
    if args.local_dump is not None:
        try:
            local_dump_fd, args.local_dump_identity = open_local_dump(args.local_dump)
            args.local_dump_fd = local_dump_fd
        except (OSError, ValueError):
            print("restore readiness preflight check failed: local dump metadata", file=sys.stderr)
            print(json.dumps(report_document("preflight", "isolation_refusal", "not_started", 0), separators=(",", ":")))
            return 1

    def close_local_dump() -> bool:
        nonlocal local_dump_fd
        if local_dump_fd is None:
            return True
        try:
            os.close(local_dump_fd)
        except OSError:
            return False
        local_dump_fd = None
        return True

    rollout_lock = open_rollout_lock()
    if rollout_lock is None:
        close_local_dump()
        print("restore readiness preflight check failed: deploy rollout lock", file=sys.stderr)
        print(json.dumps(report_document("preflight", "rollout_in_progress", "not_started", 0), separators=(",", ":")))
        return 1
    rollout_lock_fd, rollout_lock_record = rollout_lock
    lock = open_global_lock()
    if lock is None:
        os.close(rollout_lock_fd)
        close_local_dump()
        print("restore readiness preflight check failed: global lock", file=sys.stderr)
        print(json.dumps(report_document("preflight", "isolation_refusal", "not_started", 0), separators=(",", ":")))
        return 1
    lock_fd, lock_record = lock
    token = secrets.token_hex(16)
    try:
        run_dir = Path(tempfile.mkdtemp(prefix=f"restore-readiness-{token[:12]}-", dir="/run"))
        os.chmod(run_dir, 0o700)
    except OSError:
        os.close(lock_fd)
        os.close(rollout_lock_fd)
        close_local_dump()
        print("restore readiness preflight check failed: private /run directory", file=sys.stderr)
        print(json.dumps(report_document("preflight", "preflight_failure", "not_started", 0), separators=(",", ":")))
        return 1
    started = time.monotonic()
    parent_ticks = process_start_ticks(os.getpid())
    if parent_ticks is None:
        os.close(lock_fd)
        os.close(rollout_lock_fd)
        close_local_dump()
        print("restore readiness preflight check failed: parent process identity", file=sys.stderr)
        print(json.dumps(report_document("preflight", "preflight_failure", "not_started", 0), separators=(",", ":")))
        return 1
    runtime = {
        "docker_bin": args.docker_bin,
        "atlas_bin": args.atlas_bin,
        "build_dir": args.build_dir,
        "build_uid": args.build_uid,
        "go_arch": args.go_arch,
        "script": str(Path(__file__).resolve()),
    }
    manifest: dict[str, Any] = {
        "schema_version": 1,
        "token": token,
        "run_dir": str(run_dir),
        "started": started,
        "work_deadline": started + work_seconds,
        "cleanup_deadline": started + work_seconds + CLEANUP_SECONDS,
        "parent": {"pid": os.getpid(), "start_ticks": parent_ticks},
        "supervisor": {"pid": 0, "start_ticks": 0},
        "global_lock": lock_record,
        "rollout_lock": rollout_lock_record,
        "prepared_runtime": args.prepared_runtime,
        "runtime": runtime,
        "baseline": {
            "containers": [],
            "volumes": [],
            "networks": [],
            "docker_processes": [],
        },
        "containers": [],
        "images": [],
        "files": [],
        "processes": [],
        "build_directory": None,
        "pending_container": None,
        "pending_image": None,
        "resource_creation_started": False,
        "parent_done": False,
        "cleanup_verified": False,
        "cleanup_failures": [],
    }
    drill = Drill(args, run_dir, manifest)
    drill.save()
    supervisor: subprocess.Popen[bytes] | None = None
    failure: DrillFailure | None = None
    signal_failure: DrillFailure | None = None
    started_ms = time.monotonic()

    def on_signal(signum: int, _frame: Any) -> None:
        nonlocal signal_failure
        try:
            signal_failure = record_drill_signal(drill, signum, signal_failure, manifest.get("parent_done", False))
        except DrillFailure as exc:
            signal_failure = exc
            raise

    previous_handlers = {
        sig: signal.signal(sig, on_signal)
        for sig in (signal.SIGINT, signal.SIGTERM)
    }
    try:
        supervisor = subprocess.Popen(
            [
                sys.executable,
                str(Path(__file__).resolve()),
                "--supervise",
                str(drill.manifest_path),
                "--global-lock-fd",
                str(lock_fd),
                "--rollout-lock-fd",
                str(rollout_lock_fd),
            ],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            env={"HOME": "/root", "LANG": "C", "LC_ALL": "C", "PATH": "/usr/bin:/bin"},
            close_fds=True,
            pass_fds=(lock_fd, rollout_lock_fd),
            start_new_session=True,
        )
        supervisor_ticks = process_start_ticks(supervisor.pid)
        if supervisor_ticks is None:
            raise DrillFailure("preflight", "preflight_failure")
        manifest = wait_for_supervisor_registration(
            drill.manifest_path,
            supervisor,
            supervisor_ticks,
        )
        drill.manifest = manifest
        drill.preflight_step = "private run layout"
        drill.create_private_layout()
        gateway, _ = drill.preflight()
        drill.manifest["resource_creation_started"] = True
        drill.save()
        drill.preload()
        drill.build_images()
        drill.preflight_step = "verify no-network egress confinement"
        drill.create_egress_probe(gateway)
        if drill.gateway is None or not drill.gateway.close():
            drill.gateway = None
            raise DrillFailure("preflight", "isolation_refusal")
        drill.gateway = None
        drill.prepare_materials()
        source = drill.create_database("source")
        if args.fault_test == "sigkill" and not args.local_fixture_test:
            write_sigkill_marker(sys.stdout.fileno())
            os.kill(os.getpid(), signal.SIGKILL)
        drill.migrate_database(
            "source", source, SEED_SCHEMA, "source_migrations", role="seed_migration", keep_env=True
        )
        drill.verify_schema_state(source, 0, MIGRATION_ROLES, "source_migrations", SEED_SCHEMA)
        drill.seed_source_fixture(source)
        drill.migrate_database(
            "source",
            source,
            BASE_SCHEMA,
            "source_migrations",
            keep_env=drill.input_mode == "local",
        )
        drill.verify_schema_state(source, 1, MIGRATION_ROLES, "source_migrations")
        drill.seed_source_auth_key_fixture(source)
        drill.verify_schema_state(
            source,
            1,
            MIGRATION_ROLES,
            "source_migrations",
            expected_auth_keys=AUTH_KEY_FIXTURE_COUNT,
        )
        if args.local_fixture_test:
            drill.migrate_database(
                "source",
                source,
                LOCAL_FIXTURE_SCHEMA,
                "source_migrations",
                keep_env=True,
            )
            expected_users, expected_auth_keys = source_fixture_counts(False)
            drill.verify_schema_state(
                source,
                expected_users,
                MIGRATION_ROLES,
                "source_migrations",
                expected_head=LOCAL_FIXTURE_SCHEMA,
                expected_auth_keys=expected_auth_keys,
            )
        target = drill.create_database("target")
        if drill.input_mode == "local":
            if args.local_fixture_test:
                drill.create_local_dump_fixture(source)
            drill.delete_private_file(drill.files["source_tool_env"])
            drill.prepare_local_restore_target(target)
            drill.restore_local_dump(target)
            if args.fault_test == "sigkill":
                write_sigkill_marker(sys.stdout.fileno())
                os.kill(os.getpid(), signal.SIGKILL)
            dump_head = drill.local_dump_migration_head(source, target, "schema_validation")
            try:
                source_head = resolve_local_dump_migration_head(
                    drill.sql(source, MIGRATION_HEAD_SQL, "schema_validation", pinned_search_path=True),
                    drill.run_dir / "migrations",
                )
            except ValueError as exc:
                raise DrillFailure("schema_validation", "schema_failure") from exc
            if source_head > dump_head:
                raise DrillFailure("schema_validation", "schema_failure")
            if source_head != dump_head:
                drill.migrate_database(
                    "source",
                    source,
                    dump_head,
                    "source_migrations",
                    keep_env=dump_head != HEAD_SCHEMA,
                )
            expected_users, expected_auth_keys = source_fixture_counts(args.local_fixture_test)
            drill.verify_schema_state(
                source,
                expected_users,
                MIGRATION_ROLES,
                "source_migrations",
                expected_head=dump_head,
                expected_auth_keys=expected_auth_keys,
            )
            drill.verify_local_schema_state(source, target, "schema_validation", expected_head=dump_head)
            drill.run_local_hostile_fixture_tests(source, target, dump_head)
            if dump_head != HEAD_SCHEMA:
                drill.migrate_database("source", source, HEAD_SCHEMA, "source_migrations")
                drill.verify_schema_state(
                    source,
                    expected_users,
                    MIGRATION_ROLES,
                    "source_migrations",
                    expected_head=HEAD_SCHEMA,
                    expected_auth_keys=expected_auth_keys,
                )
                drill.apply_pending_migration(target)
            drill.verify_local_schema_state(source, target, "schema_validation", expected_head=HEAD_SCHEMA)
            drill.migration_head = HEAD_SCHEMA
            drill.promote_local_restore_owner(target)
        elif args.fault_test == "malformed-sql":
            drill.restore_fixture(source, target, malformed_sql=malformed_sql_fixture(token))
        else:
            drill.restore_fixture(source, target)
        if drill.input_mode == "synthetic":
            drill.apply_pending_migration(target)
        if args.fault_test == "malformed-sql":
            raise DrillFailure("auth_key_clearing", "schema_failure")
        drill.write_post_restore_marker(target)
        if args.fault_test == "auth-clear-failure":
            drill.clear_auth_keys(target, inject_failure=True)
        elif args.fault_test == "auth-clear-signal":
            for signum in (signal.SIGINT, signal.SIGTERM):
                try:
                    drill.clear_auth_keys(target, interrupt_signal=signum)
                except DrillFailure as exc:
                    if exc.phase != "auth_key_clearing" or exc.category != "interrupted" or drill.interrupted != str(signum):
                        raise
                    drill.interrupted = None
                    signal_failure = None
                else:
                    raise DrillFailure("auth_key_clearing", "auth_key_clear_failure")
            raise DrillFailure("auth_key_clearing", "interrupted")
        else:
            drill.clear_auth_keys(target)
            drill.delete_private_file(drill.files["target_tool_env"])
        app = drill.start_application(target)
        drill.run_application_checker(app)
    except DrillFailure as exc:
        failure = exc
        if exc.phase == "fixture_generation" and exc.fixture_check is None:
            check = drill.fixture_check
            exc.fixture_check = check if check in SAFE_FIXTURE_CHECKS else "fixture_setup"
        if exc.phase == "preflight":
            print(f"restore readiness preflight check failed: {drill.preflight_step}", file=sys.stderr)
    except BaseException:
        fixture_check = drill.fixture_check if drill.phase == "fixture_generation" else None
        failure = DrillFailure(drill.phase, "fixture_failure", fixture_check=fixture_check)
    finally:
        manifest["parent_done"] = True
        if drill.gateway is not None:
            if not drill.gateway.close() and failure is None:
                failure = DrillFailure("cleanup", "cleanup_failure")
            drill.gateway = None

    if failure is not None:
        manifest["failure_phase"] = failure.phase
        manifest["failure_category"] = failure.category
    manifest["parent_done"] = True
    try:
        drill.save()
    except OSError:
        if failure is None:
            fixture_check = drill.fixture_check if drill.phase == "fixture_generation" else None
            failure = DrillFailure(drill.phase, "fixture_failure", fixture_check=fixture_check)
    try:
        os.close(lock_fd)
    except OSError:
        if failure is None:
            failure = DrillFailure("cleanup", "cleanup_failure")
    try:
        os.close(rollout_lock_fd)
    except OSError:
        if failure is None:
            failure = DrillFailure("cleanup", "cleanup_failure")

    supervisor_succeeded = False
    supervisor_output = b""
    if supervisor is not None:
        remaining = max(1, manifest["cleanup_deadline"] - time.monotonic() + 10)
        try:
            supervisor.wait(timeout=remaining)
            supervisor_succeeded = supervisor.returncode == 0
        except DrillFailure as exc:
            failure = failure or exc
            try:
                supervisor.wait(timeout=max(1, manifest["cleanup_deadline"] - time.monotonic() + 10))
                supervisor_succeeded = supervisor.returncode == 0
            except subprocess.TimeoutExpired:
                failure = failure or DrillFailure("cleanup", "cleanup_failure")
        except subprocess.TimeoutExpired:
            failure = failure or DrillFailure("cleanup", "cleanup_failure")
        if supervisor.poll() is not None:
            try:
                supervisor_output, _ = supervisor.communicate()
            except (OSError, ValueError):
                supervisor_output = b""
    else:
        failure = failure or DrillFailure("cleanup", "cleanup_failure")

    if signal_failure is not None:
        failure = failure or signal_failure
    for sig, previous in previous_handlers.items():
        signal.signal(sig, previous)
    if not close_local_dump() and failure is None:
        failure = DrillFailure("cleanup", "cleanup_failure")

    foreign_inventory_changed = False
    if run_dir.exists():
        try:
            final_manifest = load_manifest(drill.manifest_path)
            cleanup_failures = final_manifest.get("cleanup_failures", [])
            if cleanup_failures:
                print("restore readiness cleanup failed: " + ",".join(cleanup_failures), file=sys.stderr)
            inventory_diagnostic = final_manifest.get("foreign_inventory_diagnostic")
            if isinstance(inventory_diagnostic, str):
                if inventory_diagnostic.startswith("changed:"):
                    print(
                        "restore readiness foreign Docker inventory "
                        + inventory_diagnostic.replace(":", " ", 1),
                        file=sys.stderr,
                    )
                elif inventory_diagnostic.startswith("unverified:"):
                    print(
                        "restore readiness foreign Docker inventory could not be verified: "
                        + inventory_diagnostic.removeprefix("unverified:"),
                        file=sys.stderr,
                    )
            foreign_failure = foreign_inventory_failure(final_manifest)
            foreign_inventory_changed = foreign_failure is not None
        except DrillFailure:
            pass
    if foreign_inventory_changed:
        failure = foreign_failure

    cleanup_verified = supervisor_succeeded and not run_dir.exists()
    category = failure.category if failure is not None else "assertions_passed"
    phase = failure.phase if failure is not None else "application_assertions"
    cleanup = "verified" if cleanup_verified else "unverified"
    if cleanup == "unverified":
        diagnostic = parse_cleanup_failure_output(supervisor_output)
        if diagnostic is None:
            if supervisor_succeeded:
                fallback = cleanup_failure_document("run_directory_finalization", "verification_failed")
            elif supervisor is None:
                fallback = cleanup_failure_document(
                    "supervisor_setup",
                    "supervisor_failed",
                    stage="spawn_supervisor",
                )
            else:
                status = supervisor.returncode
                fallback = cleanup_failure_document(
                    "cleanup_resources",
                    "supervisor_failed",
                    status if status not in (None, 0) and -255 <= status <= 255 else None,
                )
            diagnostic = cleanup_failure_log_message(fallback)
        if diagnostic is not None:
            print(diagnostic, file=sys.stderr)
    if failure is None and not cleanup_verified:
        category = "cleanup_failure"
        phase = "cleanup"
    report = report_document(
        phase,
        category,
        cleanup,
        int((time.monotonic() - started_ms) * 1000),
        migration_head=drill.migration_head,
        help_get_config=drill.checker_result["help_getConfig"],
        synthetic_signin=drill.checker_result["synthetic_signin"],
        auth_keys_cleared="pass" if drill.auth_keys_cleared else "not_run",
        input_mode=drill.input_mode,
        fixture_check=failure.fixture_check if failure is not None else None,
    )
    print(json.dumps(report, separators=(",", ":"), sort_keys=True))
    return report["exit"]


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(add_help=True)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--supervise", type=Path)
    mode.add_argument("--recover", type=Path)
    mode.add_argument("--validate-local-dump", type=Path)
    mode.add_argument("--validate-source", action="store_true")
    mode.add_argument("--validate-prepared", type=Path)
    mode.add_argument("--attest-prepared", type=Path)
    mode.add_argument("--teardown-prepared", type=Path)
    parser.add_argument("--global-lock-fd", type=int)
    parser.add_argument("--rollout-lock-fd", type=int)
    parser.add_argument("--docker-bin")
    parser.add_argument("--atlas-bin")
    parser.add_argument("--build-dir")
    parser.add_argument("--build-uid", type=int)
    parser.add_argument("--go-arch")
    parser.add_argument("--go-version")
    parser.add_argument("--go-root", type=Path)
    parser.add_argument("--go-binary", type=Path)
    parser.add_argument("--prepared-dir", type=Path)
    parser.add_argument(
        "--fault-test",
        choices=("malformed-sql", "auth-clear-failure", "auth-clear-signal", "sigkill"),
    )
    parser.add_argument("--work-seconds", type=int)
    local_mode = parser.add_mutually_exclusive_group()
    local_mode.add_argument("--local-dump", type=Path)
    local_mode.add_argument("--local-fixture-test", action="store_true")
    args = parser.parse_args()
    if args.work_seconds is not None and args.work_seconds < 1:
        parser.error("work budget must be positive")
    if args.supervise is not None:
        if (
            args.global_lock_fd is None
            or args.global_lock_fd < 0
            or args.rollout_lock_fd is None
            or args.rollout_lock_fd < 0
            or args.local_dump is not None
            or args.local_fixture_test
            or args.fault_test is not None
        ):
            parser.error("invalid supervisor descriptor")
        return args
    if args.attest_prepared is not None:
        if (
            args.build_uid is None
            or args.build_uid <= 0
            or args.go_version is None
            or args.go_root is None
            or args.go_binary is None
            or args.go_arch is None
            or any(
                (
                    args.global_lock_fd is not None,
                    args.rollout_lock_fd is not None,
                    args.docker_bin is not None,
                    args.atlas_bin is not None,
                    args.build_dir is not None,
                    args.prepared_dir is not None,
                    args.validate_prepared is not None,
                    args.fault_test is not None,
                    args.work_seconds is not None,
                    args.local_dump is not None,
                    args.local_fixture_test,
                )
            )
        ):
            parser.error("invalid prepared runtime attestation input")
        return args
    if args.validate_prepared is not None:
        if any(
            (
                args.global_lock_fd is not None,
                args.rollout_lock_fd is not None,
                args.docker_bin is not None,
                args.atlas_bin is not None,
                args.build_dir is not None,
                args.build_uid is not None,
                args.go_arch is not None,
                args.go_version is not None,
                args.go_root is not None,
                args.go_binary is not None,
                args.prepared_dir is not None,
                args.fault_test is not None,
                args.work_seconds is not None,
                args.local_dump is not None,
                args.local_fixture_test,
            )
        ):
            parser.error("invalid prepared runtime validation input")
        return args
    if args.teardown_prepared is not None:
        if (
            args.build_uid is None
            or args.build_uid <= 0
            or args.go_version is not None
            or args.go_root is not None
            or args.go_binary is not None
            or args.go_arch is not None
            or args.prepared_dir is not None
            or args.validate_prepared is not None
            or args.global_lock_fd is not None
            or args.rollout_lock_fd is not None
            or args.docker_bin is not None
            or args.atlas_bin is not None
            or args.build_dir is not None
            or args.fault_test is not None
            or args.work_seconds is not None
            or args.local_dump is not None
            or args.local_fixture_test
        ):
            parser.error("invalid prepared runtime teardown input")
        return args
    if args.validate_local_dump is not None:
        if any(
            (
                args.global_lock_fd is not None,
                args.rollout_lock_fd is not None,
                args.docker_bin is not None,
                args.atlas_bin is not None,
                args.build_dir is not None,
                args.build_uid is not None,
                args.go_arch is not None,
                args.go_version is not None,
                args.go_root is not None,
                args.go_binary is not None,
                args.prepared_dir is not None,
                args.validate_prepared is not None,
                args.fault_test is not None,
                args.work_seconds is not None,
                args.local_dump is not None,
                args.local_fixture_test,
            )
        ):
            parser.error("invalid local dump validation input")
        return args
    if args.validate_source:
        if any(
            (
                args.global_lock_fd is not None,
                args.rollout_lock_fd is not None,
                args.docker_bin is not None,
                args.atlas_bin is not None,
                args.build_dir is not None,
                args.build_uid is not None,
                args.go_arch is not None,
                args.go_version is not None,
                args.go_root is not None,
                args.go_binary is not None,
                args.prepared_dir is not None,
                args.fault_test is not None,
                args.work_seconds is not None,
                args.local_dump is not None,
                args.local_fixture_test,
            )
        ):
            parser.error("invalid source validation input")
        return args
    if args.recover is not None:
        if (
            args.local_dump is not None
            or args.local_fixture_test
            or args.fault_test is not None
            or args.prepared_dir is not None
            or args.validate_prepared is not None
        ):
            parser.error("invalid recovery input")
        return args
    if args.local_dump is not None and args.fault_test is not None:
        parser.error("local dump mode cannot use fault injection")
    if args.local_fixture_test and args.fault_test not in (None, "auth-clear-signal", "sigkill"):
        parser.error("unsupported local fixture fault injection")
    if (
        not all((args.docker_bin, args.prepared_dir))
        or any((args.atlas_bin, args.build_dir, args.build_uid, args.go_arch, args.go_version, args.go_root, args.go_binary))
    ):
        parser.error("missing confined drill input")
    return args


def main() -> int:
    args = parse_args()
    if args.supervise is not None:
        return supervise(args.supervise, args.global_lock_fd, args.rollout_lock_fd)
    if args.recover is not None:
        return recover(args.recover)
    if args.validate_local_dump is not None:
        return validate_local_dump_argument(args.validate_local_dump)
    if args.validate_source:
        try:
            source_identity()
            return 0
        except (OSError, ValueError, TypeError, subprocess.SubprocessError):
            print("restore readiness preflight check failed: source tree", file=sys.stderr)
            return 1
    if args.validate_prepared is not None:
        try:
            verify_prepared_runtime(args.validate_prepared)
            return 0
        except (OSError, ValueError, TypeError, KeyError, json.JSONDecodeError, subprocess.SubprocessError):
            print("restore readiness preflight check failed: prepared runtime", file=sys.stderr)
            return 1
    if args.attest_prepared is not None:
        try:
            finalize_prepared_runtime(
                args.attest_prepared,
                build_uid=args.build_uid,
                go_version=args.go_version,
                go_root=args.go_root,
                go_binary=args.go_binary,
                go_arch=args.go_arch,
            )
            return 0
        except (OSError, ValueError, TypeError, subprocess.SubprocessError):
            print("restore readiness preflight check failed: prepared runtime attestation", file=sys.stderr)
            return 1
    if args.teardown_prepared is not None:
        if teardown_prepared_runtime(args.teardown_prepared, args.build_uid):
            return 0
        print("restore readiness preflight check failed: prepared runtime teardown", file=sys.stderr)
        return 1
    return run_primary(args)


if __name__ == "__main__":
    raise SystemExit(main())

