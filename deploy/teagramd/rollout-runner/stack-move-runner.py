#!/usr/bin/env python3
"""Move the live single-host Compose stack to the teagram project."""

from __future__ import annotations

import argparse
import hashlib
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
TARGET_VOLUMES = {
    "pgdata": "teagram_pgdata",
    "tgkey": "teagram_tgkey",
    "tgblobs": "teagram_tgblobs",
    "rustfsdata": "teagram_rustfsdata",
}
SOURCE_SERVER = "telegramd"
TARGET_SERVER = "teagramd"
TARGET_IMAGE_NAME = "teagramd:local"
PGDATA_MOUNT = "/var/lib/postgresql"
PGDATA_PATH = "/var/lib/postgresql/18/docker"
IDENTIFIER = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")


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
) -> list[str]:
    args = ["docker", "compose"]
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
) -> dict[str, object]:
    rendered = output([*compose_args(project, project_dir, env_file, files), "config", "--format", "json"])
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


def layout_changed(source: dict[str, object], target: dict[str, object]) -> bool:
    return source != target


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
    if not isinstance(postgres_image, str) or not postgres_image.endswith("postgres:16-alpine"):
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
        "AND left(schemaname, 3) <> 'pg_' ORDER BY 1;\n\\gexec\n"
    ).encode()
    return output([
        "docker", "exec", "-i", container, "psql", "-X", "-A", "-t",
        "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", database,
    ], input_bytes=query)


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


def inspect_target_contract(config: dict[str, object]) -> None:
    if config.get("name") != TARGET_PROJECT:
        raise MoveError("target Compose project must be teagram")
    if named_volumes(config) != TARGET_VOLUMES:
        raise MoveError("target Compose named volumes do not match the reviewed teagram names")
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
    if not isinstance(image, str) or not image.endswith("postgres:18-alpine"):
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
    for logical, path, read_only in (
        ("tgkey", "/var/lib/teagramd", False),
        ("tgblobs", "/var/lib/teagramd-blobs", True),
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


def require_s3_source_blob_lane(config: dict[str, object]) -> None:
    service = server_service(config, SOURCE_SERVER)
    environment = service.get("environment")
    if not isinstance(environment, dict) or any(
        not isinstance(environment.get(name), str) or not environment[name]
        for name in (
            "TG_BLOB_S3_ENDPOINT",
            "TG_BLOB_S3_BUCKET",
            "TG_BLOB_S3_PREFIX",
            "TG_BLOB_S3_REGION",
            "TG_BLOB_S3_ACCESS_KEY_ID",
        )
    ):
        raise MoveError("source blob mode must be s3-accepted before moving to the teagram stack")
    server_mounts = service.get("volumes")
    if not isinstance(server_mounts, list) or len([
        mount for mount in server_mounts
        if isinstance(mount, dict)
        and mount.get("type") == "volume"
        and mount.get("source") == "tgblobs"
        and mount.get("target") == "/var/lib/telegramd-blobs"
        and mount.get("read_only", False) is True
    ]) != 1:
        raise MoveError("source blob mode must be s3-accepted before moving to the teagram stack")
    services = config.get("services")
    rustfs = services.get("rustfs") if isinstance(services, dict) else None
    rustfs_mounts = rustfs.get("volumes") if isinstance(rustfs, dict) else None
    if not isinstance(rustfs_mounts, list) or len([
        mount for mount in rustfs_mounts
        if isinstance(mount, dict)
        and mount.get("type") == "volume"
        and mount.get("source") == "rustfsdata"
        and mount.get("target") == "/data"
        and mount.get("read_only", False) is False
    ]) != 1:
        raise MoveError("source blob mode must be s3-accepted before moving to the teagram stack")


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


def move(args: argparse.Namespace) -> None:
    source_dir = args.source_dir.resolve(strict=True)
    target_dir = args.target_dir.resolve(strict=True)
    evidence_dir = args.evidence_dir.resolve(strict=True)
    env_file = regular_file(args.env_file, "source Compose environment file")
    target_env_file = regular_file(args.target_env_file, "target Compose environment file")
    override = regular_file(args.override, "Compose override")
    if re.fullmatch(r"[0-9a-f]{40}", args.target_sha) is None:
        raise MoveError("target SHA must be a full commit ID")
    stage_legacy_blob_secret(source_dir, target_dir)
    source_files = compose_files(args.source_compose, source_dir)
    source_layout_files = compose_files("", source_dir)
    target_files = compose_files(args.target_compose, target_dir)
    source_compose = compose_args(SOURCE_PROJECT, source_dir, env_file, source_files)
    source_stopped = False
    source_pgdata_inventory = ""
    evidence_path = evidence_dir / "stack-move-evidence.json"
    if evidence_path.exists() or evidence_path.is_symlink():
        raise MoveError("stack move evidence already exists")
    try:
        source_config = render_compose(SOURCE_PROJECT, source_dir, env_file, source_files)
        source_layout_config = render_compose(SOURCE_PROJECT, source_dir, env_file, source_layout_files)
        target_override = remap_server_override(
            override, target_dir, TARGET_SERVER, TARGET_PROJECT,
        )
        target_files = [*target_files, target_override]
        target_compose = compose_args(TARGET_PROJECT, target_dir, target_env_file, target_files)
        target_config = render_compose(TARGET_PROJECT, target_dir, target_env_file, target_files)
        source_layout = layout(source_layout_config, SOURCE_SERVER)
        target_layout = layout(target_config, TARGET_SERVER)
        if source_layout["project"] != SOURCE_PROJECT:
            raise MoveError("source Compose project is not telegram-server")
        if not layout_changed(source_layout, target_layout):
            raise MoveError("project move was requested but the Compose layout did not change")
        if named_volumes(source_layout_config) != SOURCE_VOLUMES:
            raise MoveError("source Compose volume names do not match the expected telegram-server stack")
        inspect_target_contract(target_config)
        require_s3_source_blob_lane(source_config)
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
        for name in source_volume_names:
            if not volume_exists(name):
                raise MoveError(f"expected old source volume {name} is missing")
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

        target_server_ids = output([*target_compose, "ps", "-q", TARGET_SERVER]).splitlines()
        target_postgres_ids = output([*target_compose, "ps", "-q", "postgres"]).splitlines()
        target_rustfs_ids = output([*target_compose, "ps", "-q", "rustfs"]).splitlines()
        if len(target_server_ids) != 1 or len(target_postgres_ids) != 1 or len(target_rustfs_ids) != 1:
            raise MoveError("teagramd, PostgreSQL, and rustfs must each run as one container")
        target_server_id = target_server_ids[0] if target_server_ids else ""
        target_postgres_id = target_postgres_ids[0] if target_postgres_ids else ""
        target_rustfs_id = target_rustfs_ids[0] if target_rustfs_ids else ""
        target_ids = (target_server_id, target_postgres_id, target_rustfs_id)
        if any(re.fullmatch(r"[0-9a-f]{64}", item) is None for item in target_ids):
            raise MoveError("target teagramd, postgres, or rustfs service did not remain running")
        wait_postgres(target_postgres_id, target_database)
        target_inspected = inspect_containers(target_ids)
        for item, service in zip(target_inspected, (TARGET_SERVER, "postgres", "rustfs"), strict=True):
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
            (TARGET_VOLUMES["tgblobs"], "/var/lib/teagramd-blobs", False),
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
        if source_counts != target_counts:
            raise MoveError("PostgreSQL 18 row counts changed after its runtime container was recreated")
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
        # RustFS writes internal metadata when it starts. Its data was already
        # byte-verified immediately after the stopped-volume copy; compare the
        # application-owned key and blob volumes after service startup.
        for logical in ("tgkey", "tgblobs"):
            if volume_files(source_postgres_image, TARGET_VOLUMES[logical]) != volume_files(source_postgres_image, SOURCE_VOLUMES[logical]):
                raise MoveError(f"running teagram changed the copied {logical} inventory")
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
            "table_row_counts_equal": True,
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
            or (isinstance(target_postgres_image_initial, str) and target_postgres_image_initial.endswith("postgres:18-alpine"))
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
            or (isinstance(postgres_image, str) and postgres_image.endswith("postgres:18-alpine"))
            or (isinstance(postgres_environment, dict) and postgres_environment.get("PGDATA") == PGDATA_PATH)
        ):
            target_project = TARGET_PROJECT
        if not isinstance(target_project, str) or not target_project:
            raise MoveError("target Compose project name is missing")
        target["name"] = target_project
        source_layout = layout(source, SOURCE_SERVER)
        target_server_name = TARGET_SERVER if TARGET_SERVER in target_services else SOURCE_SERVER
        target_layout = layout(target, target_server_name)
        print(json.dumps({
            "changed": layout_changed(source_layout, target_layout),
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
