#!/usr/bin/env python3
"""Move the live single-host Compose stack to the teagram project."""

from __future__ import annotations

import argparse
import copy
import hashlib
import importlib.machinery
import importlib.util
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile
import time
from collections.abc import Sequence


SOURCE_PROJECT = "telegram-server"
TARGET_PROJECT = "teagram"
SOURCE_VOLUMES = {
    "pgdata": "telegram-server_pgdata",
    "tgkey": "telegram-server_tgkey",
    "tgblobs": "telegram-server_tgblobs",
    "rustfsdata": "telegram-server_rustfsdata",
}
SOURCE_COMPOSE_VOLUMES = ("pgdata", "tgkey", "tgblobs")
TARGET_VOLUMES = {
    "pgdata": "teagram_pgdata",
    "tgkey": "teagram_tgkey",
    "tgblobs": "teagram_tgblobs",
    "rustfsdata": "teagram_rustfsdata",
}
# This journal is Docker-managed metadata, not a data volume to migrate.
TARGET_REPORT_VOLUME = "blob_key_rotate_reports"
TARGET_REPORT_VOLUME_NAME = "teagram_blob_key_rotate_reports"
SOURCE_SERVER = "telegramd"
TARGET_SERVER = "teagramd"
TARGET_IMAGE_NAME = "teagramd:local"
PUBLIC_PROFILE_SERVICES = frozenset({"public-haproxy", "teagramd-public-1", "teagramd-public-2"})
PGDATA_MOUNT = "/var/lib/postgresql"
PGDATA_PATH = "/var/lib/postgresql/18/docker"
MEDIA_KEY_MOUNT = {
    "type": "bind",
    "source": "/etc/teagramd/media-keys",
    "target": "/run/teagramd/media-keys",
    "read_only": True,
    "bind": {"create_host_path": False},
}
SOURCE_LOCAL_BLOB_PATH = "/var/lib/telegramd-blobs"
TARGET_LOCAL_BLOB_PATH = "/var/lib/teagramd-blobs"
MOVE_BLOB_DIR_ENV = "TEAGRAMD_STACK_MOVE_BLOB_DIR"
IDENTIFIER = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
S3_BLOB_ENVIRONMENT = (
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


class MoveError(Exception):
    pass


def regular_file(path: pathlib.Path, label: str) -> pathlib.Path:
    if path.is_symlink() or not path.is_file():
        raise MoveError(f"{label} is missing or symlinked")
    return path.resolve(strict=True)


def run(
    command: Sequence[str],
    *,
    input_bytes: bytes | None = None,
    check: bool = True,
    timeout: int = 120,
    env: dict[str, str] | None = None,
    cwd: pathlib.Path | None = None,
    operation: str | None = None,
    failure_evidence: pathlib.Path | None = None,
) -> subprocess.CompletedProcess[bytes]:
    process_env = os.environ.copy()
    process_env.pop("COMPOSE_FILE", None)
    process_env.pop("COMPOSE_PROJECT_NAME", None)
    if env:
        process_env.update(env)
    try:
        result = subprocess.run(
            list(command),
            input=input_bytes,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            timeout=timeout,
            env=process_env,
            cwd=cwd,
        )
    except subprocess.TimeoutExpired as error:
        label = operation or f"{pathlib.Path(command[0]).name} command"
        timeout_diagnostic = (
            f"operation: {label}\ntimeout: {error.timeout}\n"
        ).encode()
        for name, captured in (("stdout", error.stdout), ("stderr", error.stderr)):
            if captured:
                payload = captured.encode() if isinstance(captured, str) else captured
                timeout_diagnostic += f"\n--- {name} ---\n".encode() + payload
        evidence_saved = False
        evidence_error = False
        if failure_evidence is not None:
            try:
                write_private(failure_evidence, timeout_diagnostic)
            except MoveError:
                evidence_error = True
            else:
                evidence_saved = True
        message = f"{label} timed out after {error.timeout} seconds"
        if evidence_saved:
            message += f"; diagnostic saved to {failure_evidence}"
        elif evidence_error:
            message += "; private diagnostic could not be saved"
        raise MoveError(message) from error
    except OSError as error:
        raise MoveError(f"command could not complete: {pathlib.Path(command[0]).name}") from error
    if result.returncode != 0:
        diagnostic = result.stderr or result.stdout
        saved_diagnostic = False
        if failure_evidence is not None and diagnostic:
            try:
                write_private(failure_evidence, diagnostic)
            except MoveError as error:
                if check:
                    label = operation or f"{pathlib.Path(command[0]).name} command"
                    raise MoveError(f"{label} failed with exit {result.returncode}; private diagnostic could not be saved") from error
                raise
            saved_diagnostic = True
        if check:
            label = operation or f"{pathlib.Path(command[0]).name} command"
            message = f"{label} failed with exit {result.returncode}"
            if saved_diagnostic:
                message += f"; diagnostic saved to {failure_evidence}"
            raise MoveError(message)
    return result


def output(
    command: Sequence[str],
    *,
    input_bytes: bytes | None = None,
    timeout: int = 120,
    operation: str | None = None,
    failure_evidence: pathlib.Path | None = None,
) -> str:
    result = run(
        command,
        input_bytes=input_bytes,
        timeout=timeout,
        operation=operation,
        failure_evidence=failure_evidence,
    )
    try:
        return result.stdout.decode("utf-8").strip()
    except UnicodeDecodeError as error:
        raise MoveError("command returned non-UTF-8 output") from error


def compose_args(
    project: str | None,
    project_dir: pathlib.Path,
    env_file: pathlib.Path,
    files: Sequence[pathlib.Path],
    profiles: Sequence[str] = (),
) -> list[str]:
    args = ["docker", "compose"]
    for profile in profiles:
        args.extend(("--profile", profile))
    if project is not None:
        args.extend(("--project-name", project))
    args.extend(("--project-directory", str(project_dir), "--env-file", str(env_file)))
    for file_path in files:
        args.extend(("--file", str(file_path)))
    return args


def render_compose(
    project: str | None,
    project_dir: pathlib.Path,
    env_file: pathlib.Path,
    files: Sequence[pathlib.Path],
    profiles: Sequence[str] = (),
) -> dict[str, object]:
    rendered = output([
        *compose_args(project, project_dir, env_file, files, profiles),
        "config", "--format", "json",
    ])
    try:
        config = json.loads(rendered)
    except json.JSONDecodeError as error:
        raise MoveError("Compose returned invalid rendered configuration") from error
    if not isinstance(config, dict) or not isinstance(config.get("services"), dict):
        raise MoveError("Compose configuration has an invalid shape")
    return config


def remap_server_override(
    override: pathlib.Path,
    destination_dir: pathlib.Path,
    target_server: str,
    target_project: str,
) -> pathlib.Path:
    if override.is_symlink() or not override.is_file():
        raise MoveError("Compose override is missing or symlinked")
    try:
        body = override.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError) as error:
        raise MoveError("Compose override is not readable UTF-8") from error

    if body.lstrip().startswith("{"):
        try:
            config = json.loads(body)
        except json.JSONDecodeError as error:
            raise MoveError("JSON Compose override is invalid") from error
        if not isinstance(config, dict):
            raise MoveError("JSON Compose override is invalid")
        services = config.get("services", {})
        if not isinstance(services, dict):
            raise MoveError("JSON Compose override services are invalid")
        if target_server != SOURCE_SERVER and SOURCE_SERVER in services:
            if target_server in services:
                raise MoveError("Compose override configures both old and target server names")
            services[target_server] = services.pop(SOURCE_SERVER)
        if "name" in config:
            config["name"] = target_project
        body = json.dumps(config, sort_keys=True, separators=(",", ":")) + "\n"
    else:
        lines = body.splitlines(keepends=True)
        in_services = False
        service_indent: int | None = None
        source_indexes: list[int] = []
        target_indexes: list[int] = []
        for index, line in enumerate(lines):
            if not in_services:
                if re.match(r"^services\s*:\s*(?:#.*)?(?:\r?\n)?$", line):
                    in_services = True
                    service_indent = None
                    continue
                if re.match(r"^services\s*:\s*\S", line):
                    raise MoveError("inline YAML Compose service maps cannot be remapped safely")
                if re.match(r"^name\s*:", line):
                    ending = "\r\n" if line.endswith("\r\n") else "\n" if line.endswith("\n") else ""
                    lines[index] = f"name: {target_project}{ending}"
                continue

            if not line.strip() or line.lstrip().startswith("#"):
                continue
            indentation = len(line) - len(line.lstrip(" \t"))
            if indentation == 0:
                in_services = False
                if re.match(r"^name\s*:", line):
                    ending = "\r\n" if line.endswith("\r\n") else "\n" if line.endswith("\n") else ""
                    lines[index] = f"name: {target_project}{ending}"
                continue
            if service_indent is None:
                service_indent = indentation
            if indentation != service_indent:
                continue
            key = re.match(r"^[ \t]*(?:telegramd|['\"]telegramd['\"])[ \t]*:", line)
            target_key = re.match(r"^[ \t]*(?:teagramd|['\"]teagramd['\"])[ \t]*:", line)
            if key:
                source_indexes.append(index)
            if target_key:
                target_indexes.append(index)
        if source_indexes and target_indexes:
            raise MoveError("Compose override configures both old and target server names")
        if target_server != SOURCE_SERVER:
            for index in source_indexes:
                lines[index] = re.sub(
                    r"^([ \t]*)(?:telegramd|['\"]telegramd['\"])([ \t]*:)",
                    r"\1teagramd\2",
                    lines[index],
                    count=1,
                )
        body = "".join(lines)

    output_path = destination_dir / "docker-compose.override.yml"
    write_private(output_path, body.encode("utf-8"))
    return output_path


def compose_files(value: str, checkout: pathlib.Path) -> list[pathlib.Path]:
    entries = [entry for entry in value.split(os.pathsep) if entry]
    if not entries:
        entries = ["docker-compose.yml"]
        if (checkout / "docker-compose.override.yml").is_file():
            entries.append("docker-compose.override.yml")
    files = []
    for entry in entries:
        path = pathlib.Path(entry)
        if not path.is_absolute():
            path = checkout / path
        if path.is_symlink():
            raise MoveError("Compose input is a symlink")
        path = path.resolve(strict=True)
        if not path.is_file():
            raise MoveError("Compose input is missing or not a regular file")
        files.append(path)
    return files


def stage_legacy_blob_secret(source_dir: pathlib.Path, target_dir: pathlib.Path) -> None:
    legacy_secret = source_dir / ".secrets" / "telegramd-blob-secret-key"
    if not legacy_secret.exists() and not legacy_secret.is_symlink():
        return
    if legacy_secret.is_symlink() or not legacy_secret.is_file():
        raise MoveError("legacy blob secret is missing or symlinked")

    target_secrets = target_dir / ".secrets"
    if target_secrets.is_symlink() or not target_secrets.is_dir():
        raise MoveError("target .secrets directory is missing or symlinked")
    target_secret = target_secrets / "teagramd-blob-secret-key"
    if target_secret.exists() or target_secret.is_symlink():
        if target_secret.is_symlink() or not target_secret.is_file():
            raise MoveError("target teagramd blob secret is missing or symlinked")
        return
    try:
        shutil.copy2(legacy_secret, target_secret)
    except OSError as error:
        raise MoveError("cannot stage the legacy blob secret for teagramd") from error


def named_volumes(config: dict[str, object]) -> dict[str, str]:
    volumes = config.get("volumes")
    if not isinstance(volumes, dict):
        raise MoveError("Compose named volumes are missing")
    result = {}
    for logical, item in volumes.items():
        if not isinstance(item, dict) or not isinstance(item.get("name"), str):
            raise MoveError(f"Compose volume {logical} has no resolved name")
        result[logical] = item["name"]
    return result


def validate_source_compose_volumes(config: dict[str, object]) -> dict[str, str]:
    expected = {logical: SOURCE_VOLUMES[logical] for logical in SOURCE_COMPOSE_VOLUMES}
    actual = named_volumes(config)
    if "rustfsdata" in actual:
        expected["rustfsdata"] = SOURCE_VOLUMES["rustfsdata"]
    if actual != expected:
        raise MoveError("source Compose volume names do not match the expected local telegram-server stack")
    return expected


def database_name(config: dict[str, object]) -> str:
    services = config.get("services")
    postgres = services.get("postgres") if isinstance(services, dict) else None
    environment = postgres.get("environment") if isinstance(postgres, dict) else None
    database = environment.get("POSTGRES_DB") if isinstance(environment, dict) else None
    if not isinstance(database, str) or not IDENTIFIER.fullmatch(database):
        raise MoveError("Compose PostgreSQL database name is missing or invalid")
    return database


def server_service(config: dict[str, object], expected: str) -> dict[str, object]:
    services = config.get("services")
    service = services.get(expected) if isinstance(services, dict) else None
    if not isinstance(service, dict):
        raise MoveError(f"Compose service {expected} is missing")
    return service


def service_mounts(config: dict[str, object], service_name: str) -> list[dict[str, object]]:
    service = server_service(config, service_name)
    volumes = service.get("volumes", [])
    if not isinstance(volumes, list):
        raise MoveError(f"Compose service {service_name} mounts are invalid")
    selected = []
    for mount in volumes:
        if not isinstance(mount, dict):
            raise MoveError(f"Compose service {service_name} has an invalid mount")
        selected.append({
            "type": mount.get("type", ""),
            "source": mount.get("source", ""),
            "target": mount.get("target", ""),
            "read_only": mount.get("read_only", False),
            "bind": mount.get("bind"),
        })
    return sorted(selected, key=lambda item: (str(item["target"]), str(item["source"])))


def persistent_service_mounts(config: dict[str, object]) -> dict[str, list[dict[str, object]]]:
    services = config.get("services")
    if not isinstance(services, dict):
        raise MoveError("Compose services are missing")
    result = {}
    for service_name in sorted(services):
        mounts = [
            mount for mount in service_mounts(config, service_name)
            if mount["type"] in {"bind", "volume"}
        ]
        if mounts:
            result[service_name] = mounts
    return result


def layout(config: dict[str, object], service_name: str) -> dict[str, object]:
    name = config.get("name")
    services = config.get("services")
    if not isinstance(name, str) or not isinstance(services, dict):
        raise MoveError("Compose project name is missing")
    postgres = services.get("postgres")
    if not isinstance(postgres, dict):
        raise MoveError("Compose postgres service is missing")
    return {
        "project": name,
        "server": service_name,
        "server_mounts": service_mounts(config, service_name),
        "postgres_mounts": sorted(
            (mount for mount in postgres.get("volumes", []) if isinstance(mount, dict)),
            key=lambda item: (str(item.get("target", "")), str(item.get("source", ""))),
        ),
        "persistent_service_mounts": persistent_service_mounts(config),
        "volumes": named_volumes(config),
    }


def _has_only_one_addition(source: object, target: object, addition: dict[str, object]) -> bool:
    if not isinstance(source, list) or not isinstance(target, list):
        return False
    remaining = list(target)
    for item in source:
        try:
            remaining.remove(item)
        except ValueError:
            return False
    if remaining == [addition]:
        return True
    if (
        addition != MEDIA_KEY_MOUNT
        or len(remaining) != 1
        or not isinstance(remaining[0], dict)
    ):
        return False

    # Compose versions serialize the explicit false as either false or {}.
    normalized = dict(remaining[0])
    if normalized.get("bind") != {}:
        return False
    normalized["bind"] = {"create_host_path": False}
    return normalized == addition


def _is_approved_media_key_mount_addition(
    source: dict[str, object], target: dict[str, object],
) -> bool:
    if source.keys() != target.keys() or any(
        source.get(key) != target.get(key)
        for key in ("project", "server", "postgres_mounts", "volumes")
    ):
        return False
    if source.get("project") != TARGET_PROJECT or source.get("server") != TARGET_SERVER:
        return False
    if not _has_only_one_addition(
        source.get("server_mounts"), target.get("server_mounts"), MEDIA_KEY_MOUNT,
    ):
        return False

    source_services = source.get("persistent_service_mounts")
    target_services = target.get("persistent_service_mounts")
    if not isinstance(source_services, dict) or not isinstance(target_services, dict):
        return False
    if source_services.keys() != target_services.keys():
        return False
    for service, source_mounts in source_services.items():
        target_mounts = target_services[service]
        if service == TARGET_SERVER:
            if not _has_only_one_addition(source_mounts, target_mounts, MEDIA_KEY_MOUNT):
                return False
        elif source_mounts != target_mounts:
            return False
    return True


def layout_changed(source: dict[str, object], target: dict[str, object]) -> bool:
    if source == target:
        return False
    return not _is_approved_media_key_mount_addition(source, target)


def _is_approved_media_key_mount(mount: object) -> bool:
    if not isinstance(mount, dict):
        return False
    if mount == MEDIA_KEY_MOUNT:
        return True
    normalized = dict(mount)
    if normalized.get("bind") == {}:
        normalized["bind"] = {"create_host_path": False}
    return normalized == MEDIA_KEY_MOUNT


def _public_replica_mounts_match(
    base_mounts: object,
    replica_mounts: object,
    allow_private_media_key_omission: bool,
) -> bool:
    if not isinstance(base_mounts, list) or not isinstance(replica_mounts, list):
        return False
    if replica_mounts == base_mounts:
        return True
    if not allow_private_media_key_omission:
        return False
    media_key_mounts = [
        index for index, mount in enumerate(base_mounts)
        if _is_approved_media_key_mount(mount)
    ]
    if len(media_key_mounts) != 1:
        return False
    expected = list(base_mounts)
    expected.pop(media_key_mounts[0])
    return replica_mounts == expected


def _without_initial_public_profile_mounts(
    source: dict[str, object],
    target: dict[str, object],
    target_config: dict[str, object],
) -> dict[str, object] | None:
    source_services = source.get("persistent_service_mounts")
    target_services = target.get("persistent_service_mounts")
    configured_services = target_config.get("services")
    if not isinstance(source_services, dict) or not isinstance(target_services, dict):
        return None
    if not isinstance(configured_services, dict):
        return None

    additions = set(target_services) - set(source_services)
    if not additions:
        return target
    selected_profiles = {
        profile.strip()
        for profile in os.environ.get("COMPOSE_PROFILES", "").split(",")
        if profile.strip()
    }
    if (
        "public" not in selected_profiles
        or os.environ.get("ROLLOUT_RUNNER_PUBLIC_WEBSOCKET") != "1"
    ):
        return None
    if additions != PUBLIC_PROFILE_SERVICES:
        return None

    base_service = configured_services.get(TARGET_SERVER)
    if not isinstance(base_service, dict) or base_service.get("profiles"):
        return None
    base_environment = base_service.get("environment")
    if not isinstance(base_environment, dict):
        return None
    allow_private_media_key_omission = _local_blob_media_keys_are_cleared(base_environment)
    try:
        base_mounts = service_mounts(target_config, TARGET_SERVER)
    except MoveError:
        return None

    for name in ("teagramd-public-1", "teagramd-public-2"):
        service = configured_services.get(name)
        if not isinstance(service, dict) or service.get("profiles") != ["public"]:
            return None
        try:
            replica_mounts = service_mounts(target_config, name)
        except MoveError:
            return None
        if (
            not _public_replica_mounts_match(
                base_mounts, replica_mounts, allow_private_media_key_omission,
            )
            or not _public_replica_mounts_match(
                target_services.get(TARGET_SERVER), target_services.get(name),
                allow_private_media_key_omission,
            )
        ):
            return None

    proxy_service = configured_services.get("public-haproxy")
    if not isinstance(proxy_service, dict) or proxy_service.get("profiles") != ["public"]:
        return None
    try:
        proxy_mounts = service_mounts(target_config, "public-haproxy")
    except MoveError:
        return None
    proxy_source = proxy_mounts[0].get("source") if len(proxy_mounts) == 1 else None
    if (
        len(proxy_mounts) != 1
        or proxy_mounts[0].get("type") != "bind"
        or proxy_mounts[0].get("target") != "/usr/local/etc/haproxy/haproxy.cfg"
        or proxy_mounts[0].get("read_only") is not True
        or not isinstance(proxy_source, str)
        or not proxy_source.startswith("/")
        or not proxy_source.endswith(
            "/deploy/teagramd/haproxy-public-websocket.cfg"
        )
        or proxy_mounts[0].get("bind") not in (None, {}, {"create_host_path": False})
        or target_services.get("public-haproxy") != proxy_mounts
    ):
        return None

    # The public topology validator checks these profile services in detail.
    # Exclude only this first validated addition from the data-layout compare;
    # later changes to their mounts still fail against the live baseline.
    normalized = copy.deepcopy(target)
    normalized_services = normalized.get("persistent_service_mounts")
    if not isinstance(normalized_services, dict):
        return None
    for name in PUBLIC_PROFILE_SERVICES:
        normalized_services.pop(name, None)
    return normalized


def _local_blob_media_keys_are_cleared(environment: dict[str, object]) -> bool:
    return (
        environment.get("TG_BLOB_DIR") == TARGET_LOCAL_BLOB_PATH
        and all(environment.get(name) in (None, "") for name in S3_BLOB_ENVIRONMENT)
        and environment.get("TG_MEDIA_KEY_DIR") in (None, "")
        and environment.get("TG_MEDIA_KEY_CURRENT") in (None, "")
    )


def _media_key_environment_is_valid(environment: dict[str, object]) -> bool:
    configured_media_keys = (
        environment.get("TG_MEDIA_KEY_DIR") == "/run/teagramd/media-keys"
        and environment.get("TG_MEDIA_KEY_CURRENT") == "1"
    )
    return configured_media_keys or _local_blob_media_keys_are_cleared(environment)


def ordinary_rollout_layout_changed(
    source: dict[str, object],
    target: dict[str, object],
    target_config: dict[str, object],
) -> bool:
    if source == target:
        return False
    if target_config.get("name") != TARGET_PROJECT:
        return True
    services = target_config.get("services")
    service = services.get(TARGET_SERVER) if isinstance(services, dict) else None
    environment = service.get("environment") if isinstance(service, dict) else None
    volumes = service.get("volumes") if isinstance(service, dict) else None
    if not isinstance(environment, dict) or not isinstance(volumes, list):
        return True
    if not _media_key_environment_is_valid(environment):
        return True

    normalized_target = _without_initial_public_profile_mounts(source, target, target_config)
    if normalized_target is None:
        return True
    return layout_changed(source, normalized_target)


def _record_sanitized_mount_differences(
    before: object,
    after: object,
    service: str,
    differences: set[tuple[str, str | None, str]],
) -> None:
    if not isinstance(before, list) or not isinstance(after, list):
        differences.add((service, None, "mounts"))
        return

    before_by_target: dict[str | None, list[dict[str, object]]] = {}
    after_by_target: dict[str | None, list[dict[str, object]]] = {}
    for mount in before:
        if isinstance(mount, dict):
            mount_target = mount.get("target")
            safe_target = mount_target if isinstance(mount_target, str) else None
            before_by_target.setdefault(safe_target, []).append(mount)
    for mount in after:
        if isinstance(mount, dict):
            mount_target = mount.get("target")
            safe_target = mount_target if isinstance(mount_target, str) else None
            after_by_target.setdefault(safe_target, []).append(mount)

    missing = object()

    def changed_fields(
        before_value: object,
        after_value: object,
        prefix: str = "",
    ) -> set[str]:
        before_is_dict = isinstance(before_value, dict)
        after_is_dict = isinstance(after_value, dict)
        if before_is_dict or after_is_dict:
            before_fields = before_value if before_is_dict else {}
            after_fields = after_value if after_is_dict else {}
            field_names = set(before_fields) | set(after_fields)
            if not field_names:
                if (
                    before_is_dict != after_is_dict
                    or (before_value is missing) != (after_value is missing)
                ):
                    return {prefix or "mount"}
                return set()

            changed: set[str] = set()
            for field_name in sorted(field_names, key=str):
                field = str(field_name)
                field_path = f"{prefix}.{field}" if prefix else field
                changed.update(changed_fields(
                    before_fields.get(field_name, missing),
                    after_fields.get(field_name, missing),
                    field_path,
                ))
            return changed

        return {prefix or "mount"} if before_value != after_value else set()

    for mount_target in sorted(
        set(before_by_target) | set(after_by_target),
        key=lambda item: item or "",
    ):
        before_mounts = before_by_target.get(mount_target, [])
        after_mounts = after_by_target.get(mount_target, [])
        if len(before_mounts) != len(after_mounts):
            differences.add((service, mount_target, "mount"))
            continue
        before_mounts = sorted(
            before_mounts,
            key=lambda mount: json.dumps(mount, sort_keys=True, separators=(",", ":")),
        )
        after_mounts = sorted(
            after_mounts,
            key=lambda mount: json.dumps(mount, sort_keys=True, separators=(",", ":")),
        )
        for before_mount, after_mount in zip(before_mounts, after_mounts):
            for field in changed_fields(before_mount, after_mount):
                differences.add((service, mount_target, field))


def sanitized_layout_differences(
    source: dict[str, object],
    target: dict[str, object],
    target_config: dict[str, object],
) -> list[dict[str, str | None]]:
    differences: set[tuple[str, str | None, str]] = set()

    if source.get("project") != target.get("project") or target_config.get("name") != TARGET_PROJECT:
        differences.add(("<project>", None, "name"))
    if source.get("server") != target.get("server"):
        differences.add(("<layout>", None, "server"))

    source_services = source.get("persistent_service_mounts")
    target_services = target.get("persistent_service_mounts")
    if isinstance(source_services, dict) and isinstance(target_services, dict):
        for service in sorted(set(source_services) | set(target_services)):
            _record_sanitized_mount_differences(
                source_services.get(service, []),
                target_services.get(service, []),
                str(service),
                differences,
            )

    target_server = target.get("server")
    server_name = target_server if isinstance(target_server, str) else "<server>"
    _record_sanitized_mount_differences(
        source.get("server_mounts"), target.get("server_mounts"), server_name, differences,
    )
    _record_sanitized_mount_differences(
        source.get("postgres_mounts"), target.get("postgres_mounts"), "postgres", differences,
    )

    source_volumes = source.get("volumes")
    target_volumes = target.get("volumes")
    if isinstance(source_volumes, dict) and isinstance(target_volumes, dict):
        changed_volumes = {
            name for name in set(source_volumes) | set(target_volumes)
            if source_volumes.get(name) != target_volumes.get(name)
        }
        for name in changed_volumes:
            references = set()
            for layout_value in (source, target):
                service_mounts = layout_value.get("persistent_service_mounts")
                if not isinstance(service_mounts, dict):
                    continue
                for service, mounts in service_mounts.items():
                    if not isinstance(mounts, list):
                        continue
                    for mount in mounts:
                        if isinstance(mount, dict) and mount.get("source") == name:
                            mount_target = mount.get("target")
                            references.add((
                                str(service),
                                mount_target if isinstance(mount_target, str) else None,
                            ))
            if references:
                for service, mount_target in references:
                    differences.add((service, mount_target, "volume_name"))
            else:
                differences.add(("<volumes>", None, "definition"))

    services = target_config.get("services")
    service = services.get(TARGET_SERVER) if isinstance(services, dict) else None
    if not isinstance(service, dict):
        differences.add((TARGET_SERVER, None, "service"))
    else:
        environment = service.get("environment")
        if not isinstance(environment, dict):
            differences.add((TARGET_SERVER, MEDIA_KEY_MOUNT["target"], "environment"))
        elif not _media_key_environment_is_valid(environment):
            differences.add((TARGET_SERVER, MEDIA_KEY_MOUNT["target"], "media_key_environment"))
        if not isinstance(service.get("volumes"), list):
            differences.add((TARGET_SERVER, None, "volumes"))

    return [
        {"service": service, "mount_target": mount_target, "field": field}
        for service, mount_target, field in sorted(
            differences,
            key=lambda difference: (
                difference[0], difference[1] or "", difference[2],
            ),
        )
    ]


def write_private(path: pathlib.Path, data: bytes, *, exclusive: bool = True) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    if path.parent.is_symlink() or path.is_symlink():
        raise MoveError("evidence path is symlinked")
    flags = os.O_WRONLY | os.O_CREAT | os.O_CLOEXEC
    flags |= os.O_EXCL if exclusive else os.O_TRUNC
    try:
        fd = os.open(path, flags, 0o600)
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(path, 0o600)
    except OSError as error:
        raise MoveError(f"cannot write evidence file {path.name}") from error


def persist_move_blob_dir(path: pathlib.Path, blob_dir: str) -> None:
    if blob_dir != SOURCE_LOCAL_BLOB_PATH:
        raise MoveError("source blob directory is not an approved local move path")
    if path.is_symlink() or not path.is_file() or path.parent.is_symlink():
        raise MoveError("target Compose environment file is missing or symlinked")
    try:
        contents = path.read_bytes().decode("utf-8")
    except (OSError, UnicodeDecodeError) as error:
        raise MoveError("target Compose environment file is unreadable") from error
    key = MOVE_BLOB_DIR_ENV
    assignment = re.compile(rf"^\s*(?:export\s+)?{re.escape(key)}\s*=")
    matching_lines = [index for index, line in enumerate(contents.splitlines()) if assignment.match(line)]
    if len(matching_lines) > 1:
        raise MoveError("target Compose environment has duplicate blob layout selectors")
    lines = contents.splitlines(keepends=True)
    selected = f"{key}={blob_dir}"
    if matching_lines:
        index = matching_lines[0]
        ending = "\r\n" if lines[index].endswith("\r\n") else "\n" if lines[index].endswith("\n") else ""
        lines[index] = selected + ending
    else:
        if lines and not lines[-1].endswith(("\n", "\r")):
            lines[-1] += "\n"
        lines.append(selected + "\n")
    updated = "".join(lines).encode("utf-8")
    if updated == path.read_bytes():
        return

    temporary: pathlib.Path | None = None
    try:
        descriptor, name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
        temporary = pathlib.Path(name)
        with os.fdopen(descriptor, "wb") as stream:
            os.fchmod(stream.fileno(), 0o600)
            stream.write(updated)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        directory_fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except OSError as error:
        if temporary is not None:
            temporary.unlink(missing_ok=True)
        raise MoveError("target Compose environment update failed") from error


def volume_exists(name: str) -> bool:
    result = run(["docker", "volume", "inspect", name], check=False, timeout=10)
    if result.returncode == 0:
        try:
            inspected = json.loads(result.stdout)
        except json.JSONDecodeError as error:
            raise MoveError("Docker returned invalid volume inspection") from error
        if not isinstance(inspected, list) or len(inspected) != 1 or inspected[0].get("Name") != name:
            raise MoveError("Docker returned an unexpected volume")
        return True
    diagnostic = result.stderr.decode("utf-8", errors="replace")
    if "no such volume" in diagnostic.lower() and name in diagnostic:
        return False
    raise MoveError("Docker volume inspection failed")


def volume_files(image: str, volume: str) -> str:
    return output([
        "docker", "run", "--rm", "--network", "none", "--log-driver", "none", "--user", "0:0",
        "--mount", f"type=volume,source={volume},target=/volume,readonly",
        "--entrypoint", "sh", image, "-ec",
        "cd /volume && find . -type f -print0 | sort -z | xargs -0 -r sha256sum",
    ])


def volume_empty(image: str, volume: str) -> bool:
    return run([
        "docker", "run", "--rm", "--network", "none", "--log-driver", "none", "--user", "0:0",
        "--mount", f"type=volume,source={volume},target=/volume,readonly",
        "--entrypoint", "sh", image, "-ec", "test -z \"$(find /volume -mindepth 1 -print -quit)\"",
    ], check=False, timeout=30).returncode == 0


def inspect_containers(ids: Sequence[str]) -> list[dict[str, object]]:
    if not ids:
        return []
    raw = output(["docker", "inspect", *ids])
    try:
        inspected = json.loads(raw)
    except json.JSONDecodeError as error:
        raise MoveError("Docker returned invalid container inspection") from error
    if not isinstance(inspected, list) or not all(isinstance(item, dict) for item in inspected):
        raise MoveError("Docker returned an unexpected container list")
    return inspected


def project_container_ids(project: str, *, running: bool) -> list[str]:
    command = ["docker", "ps", "--no-trunc", "-q"]
    if not running:
        command = ["docker", "ps", "--no-trunc", "-aq"]
    command.extend(("--filter", f"label=com.docker.compose.project={project}"))
    result = output(command)
    ids = [item for item in result.splitlines() if item]
    if any(re.fullmatch(r"[0-9a-f]{64}", item) is None for item in ids):
        raise MoveError("Docker returned an invalid container ID")
    return ids


def inspect_git_target(checkout: pathlib.Path, target_sha: str, temporary: pathlib.Path) -> pathlib.Path:
    if re.fullmatch(r"[0-9a-f]{40}", target_sha) is None:
        raise MoveError("target SHA must be a full commit ID")
    compose_file = temporary / "target-docker-compose.yml"
    result = run(["git", "-C", str(checkout), "show", f"{target_sha}:docker-compose.yml"], timeout=30)
    write_private(compose_file, result.stdout)
    return compose_file


def postgres_image_matches_tag(image: object, expected_tag: str) -> bool:
    return isinstance(image, str) and image.endswith(expected_tag)


def assert_source_running(
    source_compose: Sequence[str], source_config: dict[str, object]
) -> tuple[str, str]:
    server_ids = output([*source_compose, "ps", "-q", SOURCE_SERVER]).splitlines()
    postgres_ids = output([*source_compose, "ps", "-q", "postgres"]).splitlines()
    if len(server_ids) != 1 or len(postgres_ids) != 1:
        raise MoveError("expected old telegram-server and postgres containers are not running exactly once")
    server_id = server_ids[0]
    postgres_id = postgres_ids[0]
    if re.fullmatch(r"[0-9a-f]{64}", server_id) is None or re.fullmatch(r"[0-9a-f]{64}", postgres_id) is None:
        raise MoveError("expected old telegram-server and postgres containers are not running")
    inspected = inspect_containers([server_id, postgres_id])
    services = source_config["services"]
    for item, service in zip(inspected, (SOURCE_SERVER, "postgres"), strict=True):
        config = item.get("Config")
        labels = config.get("Labels") if isinstance(config, dict) else None
        state = item.get("State")
        mounts = item.get("Mounts")
        rendered_service = services.get(service) if isinstance(services, dict) else None
        if (
            not isinstance(labels, dict)
            or labels.get("com.docker.compose.project") != SOURCE_PROJECT
            or labels.get("com.docker.compose.service") != service
            or not isinstance(state, dict)
            or state.get("Running") is not True
            or not isinstance(mounts, list)
            or not isinstance(rendered_service, dict)
        ):
            raise MoveError("running project is not the expected telegram-server stack")
        expected = rendered_service.get("volumes", [])
        if not isinstance(expected, list):
            raise MoveError("source Compose mounts are invalid")
        expected_volume_mounts = []
        for expected_mount in expected:
            if not isinstance(expected_mount, dict) or expected_mount.get("type") != "volume":
                continue
            actual_name = SOURCE_VOLUMES.get(expected_mount.get("source"))
            if not actual_name:
                raise MoveError("source Compose contains an unexpected named volume")
            expected_volume_mounts.append((
                actual_name,
                expected_mount.get("target"),
                expected_mount.get("read_only") is not True,
            ))
            matches = [
                mount for mount in mounts
                if isinstance(mount, dict)
                and mount.get("Type") == "volume"
                and mount.get("Name") == actual_name
                and mount.get("Destination") == expected_mount.get("target")
                and mount.get("RW") is (expected_mount.get("read_only") is not True)
            ]
            if len(matches) != 1:
                raise MoveError("running source volume mounts differ from the expected old stack")
        actual_volume_mounts = [
            (mount.get("Name"), mount.get("Destination"), mount.get("RW"))
            for mount in mounts
            if isinstance(mount, dict) and mount.get("Type") == "volume"
        ]
        if sorted(actual_volume_mounts) != sorted(expected_volume_mounts):
            raise MoveError("running source stack has an unexpected persistent volume mount")
    postgres = services.get("postgres") if isinstance(services, dict) else None
    postgres_image = postgres.get("image") if isinstance(postgres, dict) else None
    if not postgres_image_matches_tag(postgres_image, "postgres:16-alpine"):
        raise MoveError("source PostgreSQL must be the expected 16-alpine image")
    return server_id, postgres_id


def assert_no_external_writers(
    source_volumes: set[str],
    target_volumes: set[str],
    *,
    allowed_container_names: set[str] | None = None,
) -> None:
    allowed = allowed_container_names or set()
    ids = output(["docker", "ps", "--no-trunc", "-q"]).splitlines()
    for item in inspect_containers(ids):
        container_name = item.get("Name")
        if isinstance(container_name, str) and container_name.lstrip("/") in allowed:
            continue
        state = item.get("State")
        if not isinstance(state, dict) or state.get("Running") is not True:
            continue
        mounts = item.get("Mounts")
        config = item.get("Config")
        labels = config.get("Labels") if isinstance(config, dict) else None
        project = labels.get("com.docker.compose.project") if isinstance(labels, dict) else None
        if not isinstance(mounts, list):
            raise MoveError("running container mounts are unavailable")
        for mount in mounts:
            if not isinstance(mount, dict) or mount.get("RW") is not True:
                continue
            name = mount.get("Name")
            if name in target_volumes:
                raise MoveError(f"a running container writes target volume {name}")
            if name in source_volumes and project != SOURCE_PROJECT:
                raise MoveError("a running container outside telegram-server writes a source volume")


def check_source_volumes_are_stopped(volumes: set[str]) -> None:
    running = output(["docker", "ps", "--no-trunc", "-q"]).splitlines()
    for item in inspect_containers(running):
        mounts = item.get("Mounts")
        if not isinstance(mounts, list):
            raise MoveError("running container mounts are unavailable")
        if any(
            isinstance(mount, dict)
            and mount.get("Name") in volumes
            and mount.get("RW") is True
            for mount in mounts
        ):
            raise MoveError("a container that writes a source volume is still running")


def table_counts(container: str, database: str) -> str:
    query = (
        "SELECT format('SELECT %L, count(*) FROM %I.%I;', "
        "schemaname || '.' || tablename, schemaname, tablename) "
        "FROM pg_tables WHERE schemaname <> 'information_schema' "
        "AND left(schemaname, 3) <> 'pg_' ORDER BY 1\n\\gexec\n"
    ).encode()
    return output([
        "docker", "exec", "-i", container, "psql", "-X", "-A", "-t",
        "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", database,
    ], input_bytes=query)


def runtime_table_counts_compatible(source_counts: str, target_counts: str) -> bool:
    """Allow migrations to append Atlas revisions while preserving application rows."""
    def parse(raw: str) -> dict[str, int] | None:
        counts: dict[str, int] = {}
        for line in raw.splitlines():
            table, separator, count = line.partition("|")
            if (
                not separator
                or not table
                or "|" in count
                or not count.isascii()
                or not count.isdecimal()
                or table in counts
            ):
                return None
            counts[table] = int(count)
        return counts

    source = parse(source_counts)
    target = parse(target_counts)
    if source is None or target is None:
        return False

    atlas_revisions = "atlas_schema_revisions.atlas_schema_revisions"
    source_application_tables = source.keys() - {atlas_revisions}
    target_application_tables = target.keys() - {atlas_revisions}
    if source_application_tables != target_application_tables:
        return False
    if atlas_revisions in source and atlas_revisions not in target:
        return False
    for table in source_application_tables:
        if source[table] != target[table]:
            return False
    if atlas_revisions in source and target[atlas_revisions] < source[atlas_revisions]:
        return False
    return True


def role_rows(container: str) -> tuple[str, list[dict[str, object]]]:
    query = (
        "SELECT rolname, rolsuper, rolinherit, rolcreaterole, rolcreatedb, "
        "rolcanlogin, rolreplication, rolbypassrls, rolconnlimit, "
        "COALESCE(rolvaliduntil::text, 'infinity'), COALESCE(rolpassword, '') "
        "FROM pg_authid WHERE left(rolname, 3) <> 'pg_' ORDER BY rolname;\n"
    ).encode()
    raw = output([
        "docker", "exec", "-i", container, "psql", "-X", "-A", "-t", "-F", "|",
        "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "postgres",
    ], input_bytes=query)
    normalized = []
    role_evidence = []
    for line in raw.splitlines():
        fields = line.split("|")
        if len(fields) != 11:
            raise MoveError("PostgreSQL returned an invalid role inventory")
        normalized.append(line)
        role_evidence.append({
            "name": fields[0],
            "superuser": fields[1] == "t",
            "inherit": fields[2] == "t",
            "create_role": fields[3] == "t",
            "create_database": fields[4] == "t",
            "login": fields[5] == "t",
            "replication": fields[6] == "t",
            "bypass_rls": fields[7] == "t",
            "connection_limit": int(fields[8]),
            "valid_until": fields[9],
            "password_set": fields[10] != "",
        })
    return "\n".join(normalized), role_evidence


def restore_database(
    container: str,
    database: str,
    globals_sql: bytes,
    dump_sql: bytes,
    evidence_dir: pathlib.Path,
) -> None:
    globals_input = re.sub(rb"(?m)^CREATE ROLE (?:postgres|\"postgres\");\r?\n?", b"", globals_sql)
    assert_no_external_writers(
        set(SOURCE_VOLUMES.values()),
        set(TARGET_VOLUMES.values()),
        allowed_container_names={container},
    )
    run(
        [
            "docker", "exec", "-i", container, "psql", "-X", "-v", "ON_ERROR_STOP=1",
            "-U", "postgres", "-d", "postgres",
        ],
        input_bytes=globals_input,
        operation="PostgreSQL role restore",
        failure_evidence=evidence_dir / "restore-roles.stderr",
    )
    assert_no_external_writers(
        set(SOURCE_VOLUMES.values()),
        set(TARGET_VOLUMES.values()),
        allowed_container_names={container},
    )
    run(
        [
            "docker", "exec", "-i", container, "psql", "-X", "-v", "ON_ERROR_STOP=1",
            "-U", "postgres", "-d", database,
        ],
        input_bytes=dump_sql,
        operation="PostgreSQL database restore",
        failure_evidence=evidence_dir / "restore-database.stderr",
    )


def wait_postgres(container: str, database: str) -> None:
    for _ in range(60):
        result = run([
            "docker", "exec", container, "pg_isready", "-h", "127.0.0.1",
            "-U", "postgres", "-d", database,
        ], check=False, timeout=5)
        if result.returncode == 0:
            return
        time.sleep(1)
    raise MoveError("PostgreSQL 18 did not become ready")


def mount_volume(image: str, source: str, destination: str, *, readonly: bool = False) -> str:
    return f"type=volume,source={source},target={destination}" + (",readonly" if readonly else "")


def copy_volume(
    image: str,
    source: str,
    destination: str,
    logical_name: str,
    evidence_dir: pathlib.Path,
) -> None:
    run(
        [
            "docker", "run", "--rm", "--network", "none", "--log-driver", "none", "--user", "0:0",
            "--mount", mount_volume(image, source, "/source", readonly=True),
            "--mount", mount_volume(image, destination, "/destination"),
            "--entrypoint", "sh", image, "-ec",
            "cd /source && tar cpf - . | tar xpf - -C /destination",
        ],
        timeout=600,
        operation=f"copy {logical_name} volume",
        failure_evidence=evidence_dir / f"copy-{logical_name}.stderr",
    )


def get_image_id(container: str) -> str:
    image = output(["docker", "inspect", "--format", "{{.Image}}", container])
    if re.fullmatch(r"sha256:[0-9a-f]{64}", image) is None:
        raise MoveError("source PostgreSQL image identity is invalid")
    return image


def evidence(path: pathlib.Path, values: dict[str, object]) -> None:
    write_private(path, (json.dumps(values, sort_keys=True, separators=(",", ":")) + "\n").encode())


def require_local_blob_lane(config: dict[str, object], service_name: str, blob_path: str, label: str) -> None:
    service = server_service(config, service_name)
    environment = service.get("environment")
    if not isinstance(environment, dict) or environment.get("TG_BLOB_DIR") != blob_path:
        raise MoveError(f"{label} blob mode must use its local blob directory")
    if any(environment.get(name) not in (None, "") for name in S3_BLOB_ENVIRONMENT):
        raise MoveError(f"{label} blob mode must not select S3 authority")
    mounts = service.get("volumes")
    if not isinstance(mounts, list) or len([
        mount for mount in mounts
        if isinstance(mount, dict)
        and mount.get("type") == "volume"
        and mount.get("source") == "tgblobs"
        and mount.get("target") == blob_path
        and mount.get("read_only", False) is False
    ]) != 1:
        raise MoveError(f"{label} blob mode must use a writable tgblobs volume")


def inspect_target_contract(
    config: dict[str, object],
    blob_path: str = TARGET_LOCAL_BLOB_PATH,
) -> None:
    if config.get("name") != TARGET_PROJECT:
        raise MoveError("target Compose project must be teagram")
    target_volumes = named_volumes(config)
    report_volume_name = target_volumes.pop(TARGET_REPORT_VOLUME, None)
    if (
        target_volumes != TARGET_VOLUMES
        or report_volume_name not in (None, TARGET_REPORT_VOLUME_NAME)
    ):
        raise MoveError("target Compose named volumes do not match the reviewed teagram names")
    if report_volume_name is not None:
        declared_volumes = config.get("volumes")
        report_volume = (
            declared_volumes.get(TARGET_REPORT_VOLUME)
            if isinstance(declared_volumes, dict)
            else None
        )
        if (
            not isinstance(report_volume, dict)
            or report_volume.get("driver") != "local"
            or report_volume.get("external") not in (None, False)
            or report_volume.get("driver_opts") not in (None, {})
        ):
            raise MoveError("target rotation reports volume must use Docker-managed local storage")
    service = server_service(config, TARGET_SERVER)
    if service.get("image") != TARGET_IMAGE_NAME:
        raise MoveError("target server image must be teagramd:local")
    if not isinstance(service.get("build"), dict):
        raise MoveError("target teagramd must be built from the target checkout")
    services = config.get("services")
    postgres = services.get("postgres") if isinstance(services, dict) else None
    if not isinstance(postgres, dict):
        raise MoveError("target PostgreSQL service is missing")
    image = postgres.get("image")
    if not postgres_image_matches_tag(image, "postgres:18-alpine"):
        raise MoveError("target PostgreSQL image must be postgres:18-alpine")
    environment = postgres.get("environment")
    if not isinstance(environment, dict) or environment.get("PGDATA") != PGDATA_PATH:
        raise MoveError("target PostgreSQL must use its PostgreSQL 18 data path")
    if environment.get("POSTGRES_USER", "postgres") != "postgres":
        raise MoveError("target PostgreSQL bootstrap user must remain postgres")
    if not isinstance(environment.get("POSTGRES_PASSWORD"), str) or not environment["POSTGRES_PASSWORD"]:
        raise MoveError("target PostgreSQL password must be resolved from its private environment")
    if environment.get("POSTGRES_INITDB_WALDIR"):
        raise MoveError("target PostgreSQL WAL directory must remain inside the named data volume")
    mount_list = postgres.get("volumes")
    if not isinstance(mount_list, list) or len([
        mount for mount in mount_list
        if isinstance(mount, dict)
        and mount.get("source") == "pgdata"
        and mount.get("target") == PGDATA_MOUNT
        and mount.get("type") == "volume"
        and mount.get("read_only", False) is False
    ]) != 1:
        raise MoveError("PostgreSQL 18 must mount teagram_pgdata at /var/lib/postgresql")
    if len([mount for mount in mount_list if isinstance(mount, dict) and mount.get("type") == "volume"]) != 1:
        raise MoveError("PostgreSQL 18 must use only its teagram_pgdata persistent volume")
    if any(
        isinstance(mount, dict)
        and isinstance(mount.get("target"), str)
        and mount["target"].startswith(PGDATA_PATH)
        for mount in mount_list
    ):
        raise MoveError("target PostgreSQL must not use an anonymous mount under PGDATA")
    require_local_blob_lane(config, TARGET_SERVER, blob_path, "target")
    for logical, path, read_only in (
        ("tgkey", "/var/lib/teagramd", False),
        ("tgblobs", blob_path, False),
    ):
        server_volumes = service.get("volumes")
        if not isinstance(server_volumes, list):
            raise MoveError("target teagramd mounts are invalid")
        matches = [
            mount for mount in server_volumes
            if isinstance(mount, dict)
            and mount.get("type") == "volume"
            and mount.get("source") == logical
            and mount.get("target") == path
            and mount.get("read_only", False) is read_only
        ]
        if len(matches) != 1:
            raise MoveError(f"target teagramd must mount teagram_{logical} at {path}")
    if any(
        isinstance(mount, dict)
        and mount.get("type") == "volume"
        and mount.get("source") not in {"tgkey", "tgblobs"}
        for mount in service.get("volumes", [])
    ):
        raise MoveError("target teagramd has an unexpected persistent volume")
    rustfs = services.get("rustfs") if isinstance(services, dict) else None
    rustfs_volumes = rustfs.get("volumes") if isinstance(rustfs, dict) else None
    if not isinstance(rustfs_volumes, list) or len([
        mount for mount in rustfs_volumes
        if isinstance(mount, dict)
        and mount.get("type") == "volume"
        and mount.get("source") == "rustfsdata"
        and mount.get("target") == "/data"
        and mount.get("read_only", False) is False
    ]) != 1:
        raise MoveError("target rustfs must mount teagram_rustfsdata at /data")
    if len([mount for mount in rustfs_volumes if isinstance(mount, dict) and mount.get("type") == "volume"]) != 1:
        raise MoveError("target rustfs must use only its teagram_rustfsdata persistent volume")


def target_runtime_expectations(
    config: dict[str, object],
    selected_profiles: Sequence[str],
) -> tuple[tuple[str, ...], tuple[str, ...]]:
    services = config.get("services")
    rustfs = services.get("rustfs") if isinstance(services, dict) else None
    if not isinstance(rustfs, dict):
        raise MoveError("target rustfs service is missing")
    profiles = rustfs.get("profiles", [])
    if not isinstance(profiles, list) or not all(
        isinstance(profile, str) and profile for profile in profiles
    ):
        raise MoveError("target rustfs profiles are invalid")
    if profiles and not set(profiles).intersection(selected_profiles):
        return (TARGET_SERVER, "postgres"), ("tgkey", "tgblobs", "rustfsdata")
    return (TARGET_SERVER, "postgres", "rustfs"), ("tgkey", "tgblobs")


def require_local_source_blob_lane(config: dict[str, object]) -> str:
    require_local_blob_lane(config, SOURCE_SERVER, SOURCE_LOCAL_BLOB_PATH, "source")
    return SOURCE_LOCAL_BLOB_PATH


def verify_target_readiness(
    target_dir: pathlib.Path,
    target_server_id: str,
    evidence_dir: pathlib.Path,
) -> None:
    verifier = regular_file(
        target_dir / "deploy/teagramd/rollout-runner/rollout-verifier.sh",
        "target rollout readiness verifier",
    )
    run(
        ["bash", str(verifier), "ready", "target", target_server_id, str(evidence_dir)],
        timeout=130,
        env={"ROLLOUT_CHECKOUT_PATH": str(target_dir), "ROLLOUT_READY_SECONDS": "120"},
        cwd=target_dir,
        operation="target teagramd readiness check",
        failure_evidence=evidence_dir / "target-readiness.stderr",
    )


def postgres_bootstrap_environment(config: dict[str, object], database: str) -> dict[str, str]:
    services = config.get("services")
    postgres = services.get("postgres") if isinstance(services, dict) else None
    environment = postgres.get("environment") if isinstance(postgres, dict) else None
    if not isinstance(environment, dict):
        raise MoveError("target PostgreSQL environment is missing")
    selected = {"PGDATA": PGDATA_PATH, "POSTGRES_DB": database, "POSTGRES_USER": "postgres"}
    for name in (
        "POSTGRES_PASSWORD",
        "POSTGRES_INITDB_ARGS",
        "POSTGRES_HOST_AUTH_METHOD",
    ):
        if name in environment:
            value = environment[name]
            if not isinstance(value, str) or not value:
                raise MoveError(f"target PostgreSQL {name} is unresolved or empty")
            selected[name] = value
    if "POSTGRES_PASSWORD" not in selected:
        raise MoveError("target PostgreSQL password is missing")
    return selected


def start_pg18_restore(
    image: str,
    volume: str,
    database: str,
    startup_environment: dict[str, str],
    globals_sql: bytes,
    dump_sql: bytes,
    source_counts: str,
    source_roles: str,
    evidence_dir: pathlib.Path,
) -> dict[str, object]:
    stamp = f"{os.getpid()}-{time.time_ns() % 1000000000:09d}"
    container = f"main1974-move-restore-{stamp}"
    startup_args: list[str] = []
    for name in startup_environment:
        startup_args.extend(("--env", name))
    assert_no_external_writers(set(SOURCE_VOLUMES.values()), set(TARGET_VOLUMES.values()))
    run(
        [
            "docker", "run", "--detach", "--name", container,
            "--network", "none", "--log-driver", "none",
            "--mount", mount_volume(image, volume, PGDATA_MOUNT),
            *startup_args, image,
        ],
        timeout=120,
        env=startup_environment,
        operation="start isolated PostgreSQL 18 restore",
        failure_evidence=evidence_dir / "restore-container-start.stderr",
    )
    try:
        wait_postgres(container, database)
        restore_database(container, database, globals_sql, dump_sql, evidence_dir)
        target_counts = table_counts(container, database)
        if source_counts != target_counts:
            raise MoveError("PostgreSQL 18 table row counts differ from PostgreSQL 16")
        target_roles, target_role_evidence = role_rows(container)
        if source_roles != target_roles:
            raise MoveError("PostgreSQL role attributes or passwords differ after restore")
        write_private(evidence_dir / "table-row-counts.source.tsv", (source_counts + "\n").encode())
        write_private(evidence_dir / "table-row-counts.target.tsv", (target_counts + "\n").encode())
        write_private(
            evidence_dir / "roles.target-restore.json",
            (json.dumps(target_role_evidence, sort_keys=True, separators=(",", ":")) + "\n").encode(),
        )
        return {"row_counts_equal": True, "roles_equal": True, "roles": target_role_evidence}
    finally:
        run(["docker", "rm", "-f", container], check=False, timeout=30)
        present = run(["docker", "inspect", "--type", "container", container], check=False, timeout=10)
        if present.returncode == 0:
            raise MoveError("isolated PostgreSQL restore container remains after cleanup")


def project_is_running(project: str) -> bool:
    return bool(project_container_ids(project, running=True))


def rollback(
    source_compose: Sequence[str],
    target_compose: Sequence[str],
    source_config: dict[str, object],
    source_stopped: bool,
    evidence_dir: pathlib.Path,
) -> tuple[bool, list[str]]:
    diagnostics: list[str] = []
    if not source_stopped:
        return True, diagnostics
    target_down_evidence = evidence_dir / "rollback-target-down.stderr"
    target_down = run(
        [*target_compose, "down", "--remove-orphans"],
        check=False,
        timeout=180,
        operation="stop target Compose stack during rollback",
        failure_evidence=target_down_evidence,
    )
    if target_down.returncode != 0:
        detail = f"stop target Compose stack during rollback failed with exit {target_down.returncode}"
        if target_down_evidence.exists():
            detail += f"; diagnostic saved to {target_down_evidence}"
        diagnostics.append(detail)
    if target_down.returncode != 0 and project_is_running(TARGET_PROJECT):
        return False, diagnostics
    source_up_evidence = evidence_dir / "rollback-source-up.stderr"
    result = run(
        [*source_compose, "up", "-d"],
        check=False,
        timeout=180,
        operation="restart old Compose project during rollback",
        failure_evidence=source_up_evidence,
    )
    if result.returncode != 0:
        detail = f"restart old Compose project during rollback failed with exit {result.returncode}"
        if source_up_evidence.exists():
            detail += f"; diagnostic saved to {source_up_evidence}"
        diagnostics.append(detail)
        return False, diagnostics
    try:
        assert_source_running(source_compose, source_config)
    except (MoveError, KeyError, TypeError) as error:
        diagnostics.append(f"verify old Compose project after rollback failed: {error}")
        return False, diagnostics
    return True, diagnostics


def asset_inventories(
    image: str,
    source_volumes: dict[str, str],
    target_volumes: dict[str, str],
    evidence_dir: pathlib.Path,
) -> dict[str, dict[str, str]]:
    inventories: dict[str, dict[str, str]] = {}
    for logical in ("tgkey", "tgblobs", "rustfsdata"):
        check_source_volumes_are_stopped(set(source_volumes.values()))
        before = volume_files(image, source_volumes[logical])
        assert_no_external_writers(set(source_volumes.values()), set(target_volumes.values()))
        copy_volume(image, source_volumes[logical], target_volumes[logical], logical, evidence_dir)
        assert_no_external_writers(set(source_volumes.values()), set(target_volumes.values()))
        check_source_volumes_are_stopped(set(source_volumes.values()))
        after = volume_files(image, target_volumes[logical])
        if before != after:
            raise MoveError(f"copied {logical} volume file checksums do not match")
        if before != volume_files(image, source_volumes[logical]):
            raise MoveError(f"source {logical} volume changed during the copy")
        write_private(evidence_dir / f"{logical}.source.sha256", (before + "\n").encode())
        write_private(evidence_dir / f"{logical}.target.sha256", (after + "\n").encode())
        inventories[logical] = {
            "source_sha256": hashlib.sha256(before.encode()).hexdigest(),
            "target_sha256": hashlib.sha256(after.encode()).hexdigest(),
            "files": str(len([line for line in after.splitlines() if line])),
        }
    return inventories


def verify_runtime_volume_inventories(
    image: str,
    source_volumes: dict[str, str],
    target_volumes: dict[str, str],
    inventories: dict[str, dict[str, str]],
    stable_volumes: Sequence[str],
) -> None:
    for logical in stable_volumes:
        target_inventory = volume_files(image, target_volumes[logical])
        if hashlib.sha256(target_inventory.encode()).hexdigest() != inventories[logical]["target_sha256"]:
            raise MoveError(f"running teagram changed the copied {logical} inventory")
        if logical in ("tgkey", "tgblobs") and target_inventory != volume_files(
            image, source_volumes[logical],
        ):
            raise MoveError(f"running teagram changed the copied {logical} inventory")


def load_blob_mode_helper(helper_path: pathlib.Path):
    helper = regular_file(helper_path, "pinned blob-mode helper")
    loader = importlib.machinery.SourceFileLoader("stack_move_blob_mode_state", str(helper))
    spec = importlib.util.spec_from_loader("stack_move_blob_mode_state", loader)
    if spec is None or spec.loader is None:
        raise MoveError("blob authority helper is unavailable for the stack move")
    blob_mode = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(blob_mode)
    return blob_mode


def source_rustfsdata_authority(
    state_dir: pathlib.Path | None,
    report_root: pathlib.Path | None,
    helper_path: pathlib.Path | None,
) -> str | None:
    if state_dir is None:
        return None
    if report_root is None or helper_path is None:
        raise MoveError("durable blob authority paths are incomplete")
    blob_mode = load_blob_mode_helper(helper_path)
    try:
        records, head, mode_bytes = blob_mode.read_authority(state_dir, report_root)
    except (blob_mode.Reject, OSError, ValueError) as error:
        raise MoveError("source blob mode authority is unavailable") from error
    if mode_bytes != head or not records:
        raise MoveError("source blob mode authority is stale")
    volumes = records[-1].get("volumes")
    if not isinstance(volumes, dict):
        raise MoveError("source blob mode volume authority is invalid")
    rustfsdata = volumes.get("rustfsdata")
    if rustfsdata not in (None, SOURCE_VOLUMES["rustfsdata"]):
        raise MoveError("source blob mode RustFS volume authority is invalid")
    return rustfsdata


def ensure_source_rustfsdata_volume(authoritative_volume: str | None) -> None:
    name = SOURCE_VOLUMES["rustfsdata"]
    if volume_exists(name):
        return
    if authoritative_volume is not None:
        raise MoveError("authoritative source RustFS volume is missing")
    created = output(["docker", "volume", "create", name], timeout=30)
    if created != name:
        raise MoveError("Docker created an unexpected source RustFS volume")


def publish_moved_blob_authority(
    state_dir: pathlib.Path,
    report_root: pathlib.Path,
    lock_path: pathlib.Path,
    evidence_dir: pathlib.Path,
    blob_mode_helper: pathlib.Path,
) -> dict[str, object]:
    blob_mode = load_blob_mode_helper(blob_mode_helper)
    try:
        return blob_mode.publish_local_stack_move(argparse.Namespace(
            state_dir=state_dir,
            report_root=report_root,
            lock_path=lock_path,
            source_inventories={
                logical: evidence_dir / f"{logical}.source.sha256"
                for logical in ("tgblobs", "rustfsdata")
            },
            target_inventories={
                logical: evidence_dir / f"{logical}.target.sha256"
                for logical in ("tgblobs", "rustfsdata")
            },
        ))
    except blob_mode.Reject as error:
        raise MoveError(f"durable blob authority volume rebind failed: {error}") from error
    except (OSError, ValueError) as error:
        raise MoveError("durable blob authority volume rebind failed") from error


def move(args: argparse.Namespace) -> None:
    source_dir = args.source_dir.resolve(strict=True)
    target_dir = args.target_dir.resolve(strict=True)
    evidence_dir = args.evidence_dir.resolve(strict=True)
    env_file = regular_file(args.env_file, "source Compose environment file")
    target_env_file = regular_file(args.target_env_file, "target Compose environment file")
    override = regular_file(args.override, "Compose override")
    state_dir_arg = getattr(args, "state_dir", None)
    report_root_arg = getattr(args, "report_root", None)
    lock_path_arg = getattr(args, "lock_path", None)
    blob_mode_helper_arg = getattr(args, "blob_mode_helper", None)
    if state_dir_arg is None and report_root_arg is None and lock_path_arg is None:
        if (source_dir / ".state" / "blob-mode").exists():
            raise MoveError("durable blob authority paths are required when moving an initialized source")
        state_dir = report_root = lock_path = None
        blob_mode_helper = None
    elif state_dir_arg is None or report_root_arg is None or lock_path_arg is None:
        raise MoveError("durable blob authority paths must be supplied together")
    else:
        state_dir = state_dir_arg.resolve(strict=True)
        expected_state_dir = target_dir / ".state" / "blob-mode"
        if state_dir != expected_state_dir.resolve(strict=True):
            raise MoveError("target blob authority must come from the copied target checkout state")
        report_root = report_root_arg.resolve(strict=True)
        lock_path = regular_file(lock_path_arg, "shared deploy lock")
        if blob_mode_helper_arg is None:
            raise MoveError("pinned blob-mode helper path is required for durable authority publication")
        blob_mode_helper = regular_file(blob_mode_helper_arg, "pinned blob-mode helper")
    if re.fullmatch(r"[0-9a-f]{40}", args.target_sha) is None:
        raise MoveError("target SHA must be a full commit ID")
    source_files = compose_files(args.source_compose, source_dir)
    target_files = compose_files(args.target_compose, target_dir)
    source_compose = compose_args(SOURCE_PROJECT, source_dir, env_file, source_files)
    source_stopped = False
    source_pgdata_inventory = ""
    move_blob_dir_was_set = MOVE_BLOB_DIR_ENV in os.environ
    previous_move_blob_dir = os.environ.get(MOVE_BLOB_DIR_ENV)
    evidence_path = evidence_dir / "stack-move-evidence.json"
    if evidence_path.exists() or evidence_path.is_symlink():
        raise MoveError("stack move evidence already exists")
    try:
        source_config = render_compose(SOURCE_PROJECT, source_dir, env_file, source_files)
        source_blob_path = require_local_source_blob_lane(source_config)
        rustfsdata_authority = source_rustfsdata_authority(
            state_dir, report_root, blob_mode_helper,
        )
        persist_move_blob_dir(target_env_file, source_blob_path)
        os.environ[MOVE_BLOB_DIR_ENV] = source_blob_path
        stage_legacy_blob_secret(source_dir, target_dir)
        target_override = remap_server_override(
            override, target_dir, TARGET_SERVER, TARGET_PROJECT,
        )
        target_files = [*target_files, target_override]
        target_compose = compose_args(TARGET_PROJECT, target_dir, target_env_file, target_files)
        target_config = render_compose(
            TARGET_PROJECT, target_dir, target_env_file, target_files,
            profiles=("migration",),
        )
        selected_profiles = tuple(
            profile.strip()
            for profile in os.environ.get("COMPOSE_PROFILES", "").split(",")
            if profile.strip()
        )
        runtime_services, runtime_stable_volumes = target_runtime_expectations(
            target_config, selected_profiles,
        )
        source_layout = layout(source_config, SOURCE_SERVER)
        target_layout = layout(target_config, TARGET_SERVER)
        if source_layout["project"] != SOURCE_PROJECT:
            raise MoveError("source Compose project is not telegram-server")
        if not layout_changed(source_layout, target_layout):
            raise MoveError("project move was requested but the Compose layout did not change")
        validate_source_compose_volumes(source_config)
        inspect_target_contract(target_config, source_blob_path)
        source_database = database_name(source_config)
        target_database = database_name(target_config)
        for path in (env_file, target_env_file):
            if not path.is_file() or path.is_symlink():
                raise MoveError("Compose environment file is missing or symlinked")
        if project_is_running(TARGET_PROJECT):
            raise MoveError("target teagram project is already running")

        _, source_postgres_id = assert_source_running(source_compose, source_config)
        source_postgres_image = get_image_id(source_postgres_id)
        source_volume_names = set(SOURCE_VOLUMES.values())
        for logical in SOURCE_COMPOSE_VOLUMES:
            name = SOURCE_VOLUMES[logical]
            if not volume_exists(name):
                raise MoveError(f"expected old source volume {name} is missing")
        if (
            not volume_exists(SOURCE_VOLUMES["rustfsdata"])
            and rustfsdata_authority == SOURCE_VOLUMES["rustfsdata"]
        ):
            raise MoveError("authoritative source RustFS volume is missing")
        assert_no_external_writers(source_volume_names, set(TARGET_VOLUMES.values()))
        target_postgres_image = target_config["services"]["postgres"]["image"]
        run(
            ["docker", "pull", target_postgres_image],
            timeout=600,
            operation="pull target PostgreSQL image",
            failure_evidence=evidence_dir / "postgres-image-pull.stderr",
        )
        for name in TARGET_VOLUMES.values():
            if volume_exists(name) and not volume_empty(source_postgres_image, name):
                raise MoveError(f"target volume {name} already contains data")

        run(
            [*target_compose, "build", "-q", TARGET_SERVER],
            timeout=1800,
            env={"TEAGRAMD_SOURCE_REVISION": args.target_sha},
            operation="build target teagramd image",
            failure_evidence=evidence_dir / "teagramd-build.stderr",
        )
        target_image_id = output(
            ["docker", "image", "inspect", TARGET_IMAGE_NAME, "--format", "{{.Id}}"],
            operation="inspect target teagramd image identity",
            failure_evidence=evidence_dir / "teagramd-image-inspect.stderr",
        )
        target_revision = output(
            [
                "docker", "image", "inspect", TARGET_IMAGE_NAME,
                "--format", '{{ index .Config.Labels "org.opencontainers.image.revision" }}',
            ],
            operation="inspect target teagramd revision",
            failure_evidence=evidence_dir / "teagramd-revision-inspect.stderr",
        )
        if re.fullmatch(r"sha256:[0-9a-f]{64}", target_image_id) is None:
            raise MoveError("built teagramd image identity is invalid")
        if target_revision != args.target_sha:
            raise MoveError("built teagramd image revision does not match the target SHA")
        write_private(
            evidence_dir / "target-image.identity",
            (f"target_sha={args.target_sha} image_id={target_image_id} revision={target_revision}\n").encode(),
        )

        # Stop every writer except Postgres so the dump and role list have one
        # stable source. Postgres itself stops before any volume copy begins.
        source_stopped = True
        non_database_services = [name for name in source_config["services"] if name != "postgres"]
        if non_database_services:
            run(
                [*source_compose, "stop", *non_database_services],
                timeout=180,
                operation="stop old non-PostgreSQL services",
                failure_evidence=evidence_dir / "source-services-stop.stderr",
            )
        # The pinned local lane does not declare RustFS. Preserve its historical
        # volume when present; when authority says it never existed, stage an
        # empty legacy volume so the move evidence still covers both backends.
        ensure_source_rustfsdata_volume(rustfsdata_authority)
        check_source_volumes_are_stopped({
            SOURCE_VOLUMES["tgkey"], SOURCE_VOLUMES["tgblobs"], SOURCE_VOLUMES["rustfsdata"],
        })

        dump_result = run(
            ["docker", "exec", source_postgres_id, "pg_dump", "-U", "postgres", source_database],
            timeout=600,
            operation="PostgreSQL database dump",
            failure_evidence=evidence_dir / "pgdump.stderr",
        )
        dump_sql = dump_result.stdout
        if not dump_sql or b"dump complete" not in dump_sql[-512:].lower():
            raise MoveError("PostgreSQL 16 dump did not complete")
        globals_result = run(
            ["docker", "exec", source_postgres_id, "pg_dumpall", "--globals-only", "-U", "postgres"],
            timeout=300,
            operation="PostgreSQL role globals dump",
            failure_evidence=evidence_dir / "pgdumpall.stderr",
        )
        globals_sql = globals_result.stdout
        if not globals_sql:
            raise MoveError("PostgreSQL role globals dump is empty")
        source_counts = table_counts(source_postgres_id, source_database)
        source_roles, source_role_evidence = role_rows(source_postgres_id)
        write_private(evidence_dir / f"pgdump-pre-{source_database}.sql", dump_sql)
        write_private(evidence_dir / "pgglobals.sql", globals_sql)
        dump_sha = hashlib.sha256(dump_sql).hexdigest()
        globals_sha = hashlib.sha256(globals_sql).hexdigest()

        run(
            [*source_compose, "stop", "postgres"],
            timeout=180,
            operation="stop old PostgreSQL service",
            failure_evidence=evidence_dir / "source-postgres-stop.stderr",
        )
        check_source_volumes_are_stopped(source_volume_names)
        source_pgdata_inventory = volume_files(source_postgres_image, SOURCE_VOLUMES["pgdata"])
        write_private(
            evidence_dir / "pgdata.source-stopped.sha256",
            (source_pgdata_inventory + "\n").encode(),
        )
        for name in TARGET_VOLUMES.values():
            if not volume_exists(name):
                actual = output(["docker", "volume", "create", name], timeout=30)
                if actual != name:
                    raise MoveError("Docker created a target volume with an unexpected name")
            if not volume_empty(source_postgres_image, name):
                raise MoveError(f"target volume {name} is not empty")

        assert_no_external_writers(source_volume_names, set(TARGET_VOLUMES.values()))
        inventories = asset_inventories(source_postgres_image, SOURCE_VOLUMES, TARGET_VOLUMES, evidence_dir)
        postgres_result = start_pg18_restore(
            target_postgres_image,
            TARGET_VOLUMES["pgdata"],
            target_database,
            postgres_bootstrap_environment(target_config, target_database),
            globals_sql,
            dump_sql,
            source_counts,
            source_roles,
            evidence_dir,
        )
        run(
            [*target_compose, "up", "-d"],
            timeout=900,
            operation="start target Compose stack",
            failure_evidence=evidence_dir / "target-compose-up.stderr",
        )

        target_service_ids: dict[str, str] = {}
        for service in runtime_services:
            service_ids = output([*target_compose, "ps", "-q", service]).splitlines()
            if len(service_ids) != 1:
                raise MoveError(f"{', '.join(runtime_services)} must each run as one container")
            service_id = service_ids[0]
            if re.fullmatch(r"[0-9a-f]{64}", service_id) is None:
                raise MoveError(f"target {service} service did not remain running")
            target_service_ids[service] = service_id
        target_server_id = target_service_ids[TARGET_SERVER]
        target_postgres_id = target_service_ids["postgres"]
        wait_postgres(target_postgres_id, target_database)
        target_inspected = inspect_containers(tuple(target_service_ids.values()))
        for item, service in zip(target_inspected, runtime_services, strict=True):
            config = item.get("Config")
            labels = config.get("Labels") if isinstance(config, dict) else None
            state = item.get("State")
            if (
                not isinstance(labels, dict)
                or labels.get("com.docker.compose.project") != TARGET_PROJECT
                or labels.get("com.docker.compose.service") != service
                or not isinstance(state, dict)
                or state.get("Running") is not True
            ):
                raise MoveError(f"target {service} container did not start in project teagram")
        if target_inspected[0].get("Image") != target_image_id:
            raise MoveError("running teagramd image differs from the target build identity")
        target_server_mounts = target_inspected[0].get("Mounts")
        if not isinstance(target_server_mounts, list):
            raise MoveError("running teagramd mount inspection is unavailable")
        for volume, destination, writable in (
            (TARGET_VOLUMES["tgkey"], "/var/lib/teagramd", True),
            (TARGET_VOLUMES["tgblobs"], source_blob_path, True),
        ):
            matches = [
                mount for mount in target_server_mounts
                if isinstance(mount, dict)
                and mount.get("Type") == "volume"
                and mount.get("Name") == volume
                and mount.get("Destination") == destination
                and mount.get("RW") is writable
            ]
            if len(matches) != 1:
                raise MoveError(f"running teagramd is not using the expected {volume} mount")
        postgres_mounts = target_inspected[1].get("Mounts")
        if not isinstance(postgres_mounts, list) or not any(
            isinstance(mount, dict)
            and mount.get("Name") == TARGET_VOLUMES["pgdata"]
            and mount.get("Destination") == PGDATA_MOUNT
            and mount.get("RW") is True
            for mount in postgres_mounts
        ):
            raise MoveError("running PostgreSQL 18 is not using teagram_pgdata at /var/lib/postgresql")
        if any(
            isinstance(mount, dict)
            and isinstance(mount.get("Destination"), str)
            and mount["Destination"].startswith(PGDATA_PATH)
            for mount in postgres_mounts
        ):
            raise MoveError("running PostgreSQL 18 uses an anonymous mount under PGDATA")
        if "rustfs" in runtime_services:
            rustfs_mounts = target_inspected[2].get("Mounts")
            if not isinstance(rustfs_mounts, list) or len([
                mount for mount in rustfs_mounts
                if isinstance(mount, dict)
                and mount.get("Type") == "volume"
                and mount.get("Name") == TARGET_VOLUMES["rustfsdata"]
                and mount.get("Destination") == "/data"
                and mount.get("RW") is True
            ]) != 1:
                raise MoveError("running rustfs is not using teagram_rustfsdata at /data")
        target_counts = table_counts(target_postgres_id, target_database)
        if not runtime_table_counts_compatible(source_counts, target_counts):
            raise MoveError(
                "PostgreSQL 18 application row counts changed after its runtime container was recreated"
            )
        target_roles, target_role_evidence = role_rows(target_postgres_id)
        if source_roles != target_roles:
            raise MoveError("PostgreSQL roles changed after the runtime container was recreated")
        write_private(evidence_dir / "table-row-counts.target-runtime.tsv", (target_counts + "\n").encode())
        write_private(
            evidence_dir / "roles.source.json",
            (json.dumps(source_role_evidence, sort_keys=True, separators=(",", ":")) + "\n").encode(),
        )
        write_private(
            evidence_dir / "roles.target-runtime.json",
            (json.dumps(target_role_evidence, sort_keys=True, separators=(",", ":")) + "\n").encode(),
        )
        # RustFS writes internal metadata when its profile is active. Keep its
        # copied volume byte-stable when the migration service is inactive.
        verify_runtime_volume_inventories(
            source_postgres_image, SOURCE_VOLUMES, TARGET_VOLUMES,
            inventories, runtime_stable_volumes,
        )
        check_source_volumes_are_stopped(source_volume_names)
        retained_pgdata_inventory = volume_files(source_postgres_image, SOURCE_VOLUMES["pgdata"])
        if source_pgdata_inventory != retained_pgdata_inventory:
            raise MoveError("source telegram-server_pgdata changed during the move")
        write_private(
            evidence_dir / "pgdata.source-retained.sha256",
            (retained_pgdata_inventory + "\n").encode(),
        )
        for name in SOURCE_VOLUMES.values():
            if not volume_exists(name):
                raise MoveError(f"rollback volume {name} is missing")
        for name in TARGET_VOLUMES.values():
            if not volume_exists(name):
                raise MoveError(f"target persistent volume {name} is missing")

        verify_target_readiness(target_dir, target_server_id, evidence_dir)

        authority_record = None
        if state_dir is not None and report_root is not None and lock_path is not None:
            assert blob_mode_helper is not None
            authority_record = publish_moved_blob_authority(
                state_dir, report_root, lock_path, evidence_dir, blob_mode_helper,
            )

        evidence(evidence_path, {
            "result": "verified",
            "source_project": SOURCE_PROJECT,
            "target_project": TARGET_PROJECT,
            "target_server_container": target_server_id,
            "target_sha": args.target_sha,
            "target_server_image_id": target_image_id,
            "target_server_image_revision": target_revision,
            "source_database": source_database,
            "target_database": target_database,
            "source_postgres_image_id": source_postgres_image,
            "target_postgres_image": target_postgres_image,
            "dump_sha256": dump_sha,
            "globals_sha256": globals_sha,
            "runtime_row_counts_allow_only_atlas_revision_growth": True,
            "roles_equal": True,
            "roles": target_role_evidence,
            "volumes": {
                "source": SOURCE_VOLUMES,
                "target": TARGET_VOLUMES,
                "inventories": inventories,
                "source_pgdata_unchanged": True,
                "source_pgdata_files_sha256": hashlib.sha256(source_pgdata_inventory.encode()).hexdigest(),
                "old_volumes_retained": True,
            },
            "blob_authority": ({
                "outcome": authority_record["outcome"],
                "generation": authority_record["generation"],
                "transition_id": authority_record["transition_id"],
            } if authority_record is not None else None),
            "postgres_named_volume_mount": PGDATA_MOUNT,
            "isolated_restore": postgres_result,
        })
        print(
            f"stack_move=verified source_project={SOURCE_PROJECT} target_project={TARGET_PROJECT} "
            f"source_database={source_database} target_database={target_database} "
            f"evidence={evidence_path} old_volumes=retained"
        )
    except BaseException as error:
        if source_stopped:
            rollback_diagnostics: list[str] = []
            try:
                restored, rollback_diagnostics = rollback(
                    source_compose, target_compose, source_config, True, evidence_dir,
                )
            except Exception as rollback_error:
                restored = False
                rollback_diagnostics.append(f"rollback raised {type(rollback_error).__name__}: {rollback_error}")
            rollback_result = "old-project-restored" if restored else "old-project-restart-failed"
            message = str(error) if isinstance(error, MoveError) else "unexpected failure"
            evidence_failed = False
            if not evidence_path.exists():
                try:
                    failure_evidence: dict[str, object] = {
                        "result": "failed",
                        "reason": message,
                        "rollback": rollback_result,
                        "source_project": SOURCE_PROJECT,
                        "target_project": TARGET_PROJECT,
                        "source_volumes_retained": True,
                    }
                    if rollback_diagnostics:
                        failure_evidence["rollback_diagnostics"] = rollback_diagnostics
                    evidence(evidence_path, failure_evidence)
                except Exception:
                    evidence_failed = True
            print(f"stack_move=failed reason={message} rollback={rollback_result}", file=sys.stderr)
            for diagnostic in rollback_diagnostics:
                print(f"stack_move=rollback-diagnostic {diagnostic}", file=sys.stderr)
            if not restored:
                raise MoveError("move failed and old project restart failed") from error
            if evidence_failed:
                raise MoveError("move failed; old project restored but failure evidence could not be written") from error
        raise
    finally:
        if move_blob_dir_was_set:
            os.environ[MOVE_BLOB_DIR_ENV] = previous_move_blob_dir or ""
        else:
            os.environ.pop(MOVE_BLOB_DIR_ENV, None)


def inspect(args: argparse.Namespace) -> None:
    checkout = args.checkout.resolve(strict=True)
    env_file = regular_file(args.env_file, "Compose environment file")
    override = regular_file(args.override, "Compose override")
    if re.fullmatch(r"[0-9a-f]{40}", args.target_sha) is None:
        raise MoveError("target SHA must be a full commit ID")
    compose_selection = os.environ.get("COMPOSE_FILE", "")
    source_files = compose_files(compose_selection, checkout)
    pinned_local_selection = (
        bool(compose_selection)
        and source_files[0].name.startswith(".rollout-compose.local-")
        and source_files[0].name.endswith(".yml")
    )
    with tempfile.TemporaryDirectory(prefix="main1974-compose-") as temporary_name:
        temporary = pathlib.Path(temporary_name)
        if pinned_local_selection:
            target_compose_files = list(source_files)
        else:
            default_compose = (checkout / "docker-compose.yml").resolve()
            if default_compose not in source_files:
                raise MoveError("live Compose selection does not include docker-compose.yml")
            target_compose = inspect_git_target(checkout, args.target_sha, temporary)
            target_compose_files = [
                target_compose if path == default_compose else path
                for path in source_files
            ]
        target_base_files = [path for path in target_compose_files if path != override]
        if not target_base_files:
            raise MoveError("live Compose selection has no base file")
        source = render_compose(None, checkout, env_file, source_files)
        target_initial = render_compose(None, checkout, env_file, target_base_files)
        target_services = target_initial.get("services")
        target_server = TARGET_SERVER if isinstance(target_services, dict) and TARGET_SERVER in target_services else SOURCE_SERVER
        target_postgres_initial = target_services.get("postgres") if isinstance(target_services, dict) else None
        target_postgres_image_initial = target_postgres_initial.get("image") if isinstance(target_postgres_initial, dict) else None
        target_postgres_environment_initial = target_postgres_initial.get("environment") if isinstance(target_postgres_initial, dict) else None
        target_volumes_initial = target_initial.get("volumes")
        moved_volume_names_initial = isinstance(target_volumes_initial, dict) and all(
            isinstance(target_volumes_initial.get(logical), dict)
            and target_volumes_initial[logical].get("name") == name
            for logical, name in TARGET_VOLUMES.items()
        )
        target_project_initial = target_initial.get("name")
        if (
            target_server == TARGET_SERVER
            or moved_volume_names_initial
            or postgres_image_matches_tag(target_postgres_image_initial, "postgres:18-alpine")
            or (
                isinstance(target_postgres_environment_initial, dict)
                and target_postgres_environment_initial.get("PGDATA") == PGDATA_PATH
            )
        ):
            target_project_initial = TARGET_PROJECT
        if not isinstance(target_project_initial, str) or not target_project_initial:
            raise MoveError("target Compose project name is missing")
        target_files = list(target_compose_files)
        if override in target_compose_files:
            mapped_override = remap_server_override(
                override, temporary, target_server, target_project_initial,
            )
            target_files = [mapped_override if path == override else path for path in target_compose_files]
        target = render_compose(None, checkout, env_file, target_files)
        target_services = target.get("services")
        target_postgres = target_services.get("postgres") if isinstance(target_services, dict) else None
        target_volumes = target.get("volumes")
        if not isinstance(target_postgres, dict) or not isinstance(target_volumes, dict):
            raise MoveError("target Compose configuration is incomplete")
        postgres_image = target_postgres.get("image")
        postgres_environment = target_postgres.get("environment")
        target_project = target.get("name")
        moved_volume_names = all(
            isinstance(target_volumes.get(logical), dict)
            and target_volumes[logical].get("name") == name
            for logical, name in TARGET_VOLUMES.items()
        )
        if (
            TARGET_SERVER in target_services
            or moved_volume_names
            or postgres_image_matches_tag(postgres_image, "postgres:18-alpine")
            or (isinstance(postgres_environment, dict) and postgres_environment.get("PGDATA") == PGDATA_PATH)
        ):
            target_project = TARGET_PROJECT
        if not isinstance(target_project, str) or not target_project:
            raise MoveError("target Compose project name is missing")
        target["name"] = target_project
        source_services = source.get("services")
        source_server_name = (
            TARGET_SERVER
            if isinstance(source_services, dict) and TARGET_SERVER in source_services
            else SOURCE_SERVER
        )
        source_layout = layout(source, source_server_name)
        target_server_name = TARGET_SERVER if TARGET_SERVER in target_services else SOURCE_SERVER
        target_layout = layout(target, target_server_name)
        changed = ordinary_rollout_layout_changed(source_layout, target_layout, target)
        print(json.dumps({
            "changed": changed,
            "differences": sanitized_layout_differences(
                source_layout, target_layout, target,
            ) if changed else [],
            "source": source_layout,
            "target": target_layout,
            "source_database": database_name(source),
            "target_database": database_name(target),
        }, sort_keys=True, separators=(",", ":")))


def main() -> int:
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    inspect_command = commands.add_parser("inspect")
    inspect_command.add_argument("--checkout", type=pathlib.Path, required=True)
    inspect_command.add_argument("--target-sha", required=True)
    inspect_command.add_argument("--env-file", type=pathlib.Path, required=True)
    inspect_command.add_argument("--override", type=pathlib.Path, required=True)
    move_command = commands.add_parser("move")
    move_command.add_argument("--source-dir", type=pathlib.Path, required=True)
    move_command.add_argument("--target-dir", type=pathlib.Path, required=True)
    move_command.add_argument("--source-compose", default="")
    move_command.add_argument("--target-compose", default="")
    move_command.add_argument("--override", type=pathlib.Path, required=True)
    move_command.add_argument("--target-sha", required=True)
    move_command.add_argument("--env-file", type=pathlib.Path, required=True)
    move_command.add_argument("--target-env-file", type=pathlib.Path, required=True)
    move_command.add_argument("--evidence-dir", type=pathlib.Path, required=True)
    move_command.add_argument("--state-dir", type=pathlib.Path)
    move_command.add_argument("--report-root", type=pathlib.Path)
    move_command.add_argument("--lock-path", type=pathlib.Path)
    move_command.add_argument("--blob-mode-helper", type=pathlib.Path)
    args = parser.parse_args()
    try:
        if args.command == "inspect":
            inspect(args)
        else:
            move(args)
    except (MoveError, KeyError, TypeError, ValueError) as error:
        print(f"stack move rejected: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
