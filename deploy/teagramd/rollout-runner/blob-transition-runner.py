#!/usr/bin/env python3
"""Run a private RustFS cutover bundle through copy, qualification, and authority."""

from __future__ import annotations

import argparse
import contextlib
import datetime as dt
import fcntl
import hashlib
import importlib.util
import json
import os
import pathlib
import re
import stat
import subprocess
import sys
import uuid
from types import SimpleNamespace
from typing import Any


SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
QUALIFIER = SCRIPT_DIR / "qualify-rustfs-transition.sh"
MODE_HELPER = SCRIPT_DIR / "blob-mode-state.py"
TEST_MODE = os.environ.get("BLOB_TRANSITION_TEST_MODE") == "1"
TEST_FIXTURE_ROOT: pathlib.Path | None = None
TEST_FIXTURE_DOCKER: pathlib.Path | None = None
BLOB_TRANSITION_COMPOSE_ENV_FILES = "BLOB_TRANSITION_COMPOSE_ENV_FILES"
RECOVERY_BUNDLE_FILES = {
    "recovery.json",
    "frozen-containers.json",
    "postgres.dump",
    "migrations.json",
}


class TransitionReject(Exception):
    pass


def reject(reason: str) -> None:
    raise TransitionReject(reason)


def secure_directory(path: pathlib.Path, mode: int = 0o700) -> None:
    try:
        info = path.lstat()
    except OSError:
        reject("private-directory-unavailable")
    if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
        reject("private-directory-invalid")
    if info.st_uid != 0 or stat.S_IMODE(info.st_mode) != mode:
        reject("private-directory-permissions")


def secure_file(path: pathlib.Path) -> os.stat_result:
    try:
        info = path.lstat()
    except OSError:
        reject("private-evidence-unavailable")
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode) or info.st_uid != 0:
        reject("private-evidence-invalid")
    if stat.S_IMODE(info.st_mode) != 0o600:
        reject("private-evidence-permissions")
    return info


def inside(path: pathlib.Path, root: pathlib.Path) -> bool:
    try:
        path.resolve(strict=True).relative_to(root)
    except (OSError, ValueError):
        return False
    return True


def require_synthetic_fixture_scope(args: argparse.Namespace, bundle: pathlib.Path, checkout: pathlib.Path, report_root: pathlib.Path) -> None:
    global TEST_FIXTURE_ROOT, TEST_FIXTURE_DOCKER
    if not TEST_MODE:
        return
    if os.geteuid() != 0:
        reject("root-required")
    root_value = os.environ.get("BLOB_TRANSITION_TEST_FIXTURE_ROOT")
    docker_value = os.environ.get("BLOB_TRANSITION_TEST_FIXTURE_DOCKER")
    if not root_value or not docker_value:
        reject("synthetic-fixture-scope-required")
    root = pathlib.Path(root_value)
    adapter = pathlib.Path(docker_value)
    secure_directory(root)
    try:
        root = root.resolve(strict=True)
        adapter_info = adapter.lstat()
        adapter_real = adapter.resolve(strict=True)
    except OSError:
        reject("synthetic-fixture-scope-invalid")
    if (
        not adapter_real.is_relative_to(root)
        or not stat.S_ISREG(adapter_info.st_mode)
        or stat.S_ISLNK(adapter_info.st_mode)
        or adapter_info.st_uid != 0
        or stat.S_IMODE(adapter_info.st_mode) != 0o700
        or not os.access(adapter_real, os.X_OK)
        or adapter_real.name != "docker"
    ):
        reject("synthetic-fixture-scope-invalid")
    for path in (bundle, checkout, report_root, args.state_dir.absolute(), args.lock_path.absolute()):
        if not inside(path, root):
            reject("synthetic-fixture-scope-invalid")
    TEST_FIXTURE_ROOT = root
    TEST_FIXTURE_DOCKER = adapter_real


def copy_bundle(source: pathlib.Path, destination: pathlib.Path) -> None:
    """Copy a private immutable qualification input into this attempt's workspace."""
    try:
        destination.mkdir(mode=0o700)
        for item in sorted(source.iterdir(), key=lambda path: path.name):
            info = item.lstat()
            target = destination / item.name
            if stat.S_ISDIR(info.st_mode) and item.name == "candidate-secrets":
                secure_directory(item)
                target.mkdir(mode=0o700)
                for secret in sorted(item.iterdir(), key=lambda path: path.name):
                    secret_info = secret.lstat()
                    if (
                        not stat.S_ISREG(secret_info.st_mode)
                        or stat.S_ISLNK(secret_info.st_mode)
                        or secret_info.st_uid != 0
                        or stat.S_IMODE(secret_info.st_mode) not in (0o600, 0o444)
                    ):
                        reject("private-evidence-invalid")
                    copy_synced_file(secret, target / secret.name)
                    if stat.S_IMODE(secret_info.st_mode) == 0o444:
                        os.chmod(target / secret.name, 0o444)
                continue
            secure_file(item)
            copy_synced_file(item, target)
            if stat.S_IMODE(info.st_mode) == 0o444:
                os.chmod(target, 0o444)
    except TransitionReject:
        raise
    except OSError:
        reject("qualification-bundle-copy-failed")
    secure_directory(destination)


def fixture_interrupt(point: str) -> None:
    if TEST_MODE and os.environ.get("BLOB_TRANSITION_TEST_INTERRUPT_AFTER") == point:
        os._exit(86)


def write_synced(path: pathlib.Path, content: bytes) -> None:
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW
    try:
        fd = os.open(path, flags, 0o600)
        with os.fdopen(fd, "wb") as stream:
            stream.write(content)
            stream.flush()
            os.fchown(stream.fileno(), 0, 0)
            os.fchmod(stream.fileno(), 0o600)
            os.fsync(stream.fileno())
        dir_fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except FileExistsError:
        reject("evidence-already-exists")
    except OSError:
        reject("evidence-write-failed")


def copy_synced_file(source: pathlib.Path, destination: pathlib.Path) -> None:
    try:
        source_info = source.lstat()
    except OSError:
        reject("private-evidence-unavailable")
    if (
        not stat.S_ISREG(source_info.st_mode)
        or stat.S_ISLNK(source_info.st_mode)
        or source_info.st_uid != 0
        or stat.S_IMODE(source_info.st_mode) not in (0o600, 0o444)
    ):
        reject("private-evidence-invalid")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW
    try:
        with os.fdopen(os.open(source, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW), "rb") as source_stream:
            destination_fd = os.open(destination, flags, 0o600)
            with os.fdopen(destination_fd, "wb") as destination_stream:
                after = os.fstat(source_stream.fileno())
                if (source_info.st_dev, source_info.st_ino) != (after.st_dev, after.st_ino):
                    reject("private-evidence-changed")
                for block in iter(lambda: source_stream.read(1024 * 1024), b""):
                    destination_stream.write(block)
                destination_stream.flush()
                os.fchown(destination_stream.fileno(), 0, 0)
                os.fchmod(destination_stream.fileno(), 0o600)
                os.fsync(destination_stream.fileno())
        dir_fd = os.open(destination.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except FileExistsError:
        reject("evidence-already-exists")
    except TransitionReject:
        raise
    except OSError:
        reject("evidence-copy-failed")


def replace_synced(path: pathlib.Path, content: bytes) -> None:
    temporary = path.with_name(f".{path.name}.{uuid.uuid4()}.tmp")
    write_synced(temporary, content)
    try:
        os.replace(temporary, path)
        dir_fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except OSError:
        reject("evidence-replace-failed")


def replace_with_synced_file(path: pathlib.Path, source: pathlib.Path) -> None:
    temporary = path.with_name(f".{path.name}.{uuid.uuid4()}.tmp")
    copy_synced_file(source, temporary)
    try:
        os.replace(temporary, path)
        dir_fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except OSError:
        reject("evidence-replace-failed")


def sha256_file(path: pathlib.Path) -> str:
    secure_file(path)
    digest = hashlib.sha256()
    try:
        with path.open("rb") as stream:
            for block in iter(lambda: stream.read(1024 * 1024), b""):
                digest.update(block)
    except OSError:
        reject("private-evidence-unavailable")
    return digest.hexdigest()


def acquire_lock(path: pathlib.Path) -> None:
    if os.geteuid() != 0:
        reject("root-required")
    try:
        try:
            info = path.lstat()
        except FileNotFoundError:
            fd = os.open(
                path,
                os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW,
                0o600,
            )
            os.fchown(fd, 0, 0)
            os.fchmod(fd, 0o600)
            os.close(fd)
            info = path.lstat()
        if (
            stat.S_ISLNK(info.st_mode)
            or not stat.S_ISREG(info.st_mode)
            or info.st_uid != 0
            or stat.S_IMODE(info.st_mode) != 0o600
            or info.st_nlink != 1
        ):
            reject("shared-lock-invalid")
        fd = os.open(path, os.O_RDWR | os.O_CLOEXEC | os.O_NOFOLLOW)
        os.dup2(fd, 9, inheritable=True)
        if fd != 9:
            os.close(fd)
        fcntl.flock(9, fcntl.LOCK_EX)
        after = os.fstat(9)
        current = path.stat()
        if (after.st_dev, after.st_ino) != (current.st_dev, current.st_ino):
            reject("shared-lock-changed")
    except TransitionReject:
        raise
    except OSError:
        reject("shared-lock-unavailable")


def compose_command(args: list[str], env: dict[str, str] | None) -> list[str]:
    if args[:2] != ["docker", "compose"] or env is None:
        return args
    configured = env.get(BLOB_TRANSITION_COMPOSE_ENV_FILES)
    if not configured:
        return args
    files = configured.split(os.pathsep)
    if not files or any(not item for item in files):
        reject("compose-environment-files-invalid")
    prefix = ["docker", "compose"]
    for path in files:
        prefix.extend(("--env-file", path))
    return [*prefix, *args[2:]]


def run_command(
    args: list[str],
    output_dir: pathlib.Path,
    name: str,
    *,
    cwd: pathlib.Path | None = None,
    env: dict[str, str] | None = None,
    persist_output: bool = True,
) -> bytes:
    stdout_path = output_dir / f"{name}.stdout"
    stderr_path = output_dir / f"{name}.stderr"
    try:
        command = compose_command(args, env)
        if TEST_MODE and args and args[0] == "docker":
            if TEST_FIXTURE_DOCKER is None:
                reject("synthetic-fixture-scope-required")
            command = [str(TEST_FIXTURE_DOCKER), *command[1:]]
        result = subprocess.run(
            command, cwd=cwd, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False
        )
    except OSError:
        reject(f"{name}-unavailable")
    if persist_output:
        try:
            write_synced(stdout_path, result.stdout)
            write_synced(stderr_path, result.stderr)
        except TransitionReject:
            reject(f"{name}-evidence-write-failed")
    if result.returncode != 0:
        reject(f"{name}-failed")
    return result.stdout


def run_command_with_input_file(
    args: list[str],
    input_path: pathlib.Path,
    output_dir: pathlib.Path,
    name: str,
    *,
    cwd: pathlib.Path | None = None,
    env: dict[str, str] | None = None,
    persist_output: bool = True,
) -> bytes:
    stdout_path = output_dir / f"{name}.stdout"
    stderr_path = output_dir / f"{name}.stderr"
    command = compose_command(args, env)
    if TEST_MODE and args and args[0] == "docker":
        if TEST_FIXTURE_DOCKER is None:
            reject("synthetic-fixture-scope-required")
        command = [str(TEST_FIXTURE_DOCKER), *command[1:]]
    try:
        with input_path.open("rb") as input_stream:
            result = subprocess.run(
                command,
                cwd=cwd,
                env=env,
                stdin=input_stream,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
    except OSError:
        reject(f"{name}-unavailable")
    if persist_output:
        try:
            write_synced(stdout_path, result.stdout)
            write_synced(stderr_path, result.stderr)
        except TransitionReject:
            reject(f"{name}-evidence-write-failed")
    if result.returncode != 0:
        reject(f"{name}-failed")
    return result.stdout


def run_private_command(
    args: list[str],
    output_dir: pathlib.Path,
    name: str,
    *,
    cwd: pathlib.Path | None = None,
    env: dict[str, str] | None = None,
) -> pathlib.Path:
    stdout_path = output_dir / f"{name}.stdout"
    stderr_path = output_dir / f"{name}.stderr"
    command = compose_command(args, env)
    if TEST_MODE and args and args[0] == "docker":
        if TEST_FIXTURE_DOCKER is None:
            reject("synthetic-fixture-scope-required")
        command = [str(TEST_FIXTURE_DOCKER), *command[1:]]
    try:
        fd = os.open(
            stdout_path,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW,
            0o600,
        )
    except FileExistsError:
        reject("evidence-already-exists")
    except OSError:
        reject(f"{name}-evidence-unavailable")
    try:
        with os.fdopen(fd, "wb") as stream:
            result = subprocess.run(
                command, cwd=cwd, env=env, stdout=stream, stderr=subprocess.PIPE, check=False
            )
            stream.flush()
            os.fchown(stream.fileno(), 0, 0)
            os.fchmod(stream.fileno(), 0o600)
            os.fsync(stream.fileno())
        write_synced(stderr_path, result.stderr)
        dir_fd = os.open(output_dir, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except OSError:
        reject(f"{name}-unavailable")
    if result.returncode != 0:
        reject(f"{name}-failed")
    secure_file(stdout_path)
    return stdout_path


def files_equal(first: pathlib.Path, second: pathlib.Path) -> bool:
    secure_file(first)
    secure_file(second)
    try:
        with first.open("rb") as first_stream, second.open("rb") as second_stream:
            while True:
                left = first_stream.read(1024 * 1024)
                right = second_stream.read(1024 * 1024)
                if left != right:
                    return False
                if not left:
                    return True
    except OSError:
        reject("private-evidence-unavailable")


def run_gate(stage: str, bundle: pathlib.Path, checkout: pathlib.Path, output_dir: pathlib.Path) -> str:
    if stage not in ("pre-copy", "check"):
        reject("qualification-stage-invalid")
    stdout_path = output_dir / f"qualification-{stage}.stdout"
    stderr_path = output_dir / f"qualification-{stage}.stderr"
    try:
        result = subprocess.run(
            ["bash", str(QUALIFIER), stage, str(bundle), str(checkout)],
            cwd=checkout,
            env=compose_environment(checkout),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
    except OSError:
        reject("qualification-unavailable")
    write_synced(stdout_path, result.stdout)
    write_synced(stderr_path, result.stderr)
    if result.returncode != 0:
        reject(f"qualification-{stage}-rejected")
    try:
        output = result.stdout.decode("ascii").strip()
    except UnicodeDecodeError:
        reject("qualification-output-invalid")
    expected = "gate_result=pre-copy-pass" if stage == "pre-copy" else "gate_result=pass"
    if not output.startswith(expected + " "):
        reject("qualification-output-invalid")
    return output


def live_host_inventory(
    project: str,
    source_volume: str,
    output_dir: pathlib.Path,
    checkout: pathlib.Path,
    environment: dict[str, str],
    name: str,
) -> list[dict[str, Any]]:
    mountpoint_output = run_command(
        ["docker", "volume", "inspect", "--format", "{{.Mountpoint}}", source_volume],
        output_dir,
        f"{name}-source-volume",
        cwd=checkout,
        env=environment,
    )
    try:
        mountpoint_text = mountpoint_output.decode("utf-8").strip()
        mountpoint_path = pathlib.Path(mountpoint_text)
        if not mountpoint_path.is_absolute():
            reject("running-inventory-invalid")
        source_mountpoint = mountpoint_path.resolve(strict=False)
    except (UnicodeDecodeError, OSError):
        reject("running-inventory-invalid")
    if not mountpoint_text:
        reject("running-inventory-invalid")

    id_output = run_command(
        [
            "docker", "ps", "--no-trunc", "--format",
            '{{.ID}}\t{{.Label "com.docker.compose.project"}}\t{{.Label "com.docker.compose.service"}}',
        ],
        output_dir,
        f"{name}-ids",
        cwd=checkout,
        env=environment,
    )
    try:
        rows = [line.split("\t") for line in id_output.decode("utf-8").splitlines() if line]
    except UnicodeDecodeError:
        reject("running-inventory-invalid")
    if not rows or any(
        len(row) != 3
        or re.fullmatch(r"[0-9a-f]{64}", row[0]) is None
        for row in rows
    ):
        reject("running-inventory-invalid")
    identifiers = [row[0] for row in rows]
    if len(identifiers) != len(set(identifiers)):
        reject("running-inventory-invalid")
    inspect_output = run_command(
        ["docker", "inspect", *identifiers], output_dir, f"{name}-inspect",
        cwd=checkout,
        env=environment,
    )
    mode = import_mode_helper()
    try:
        inspected = json.loads(inspect_output, object_pairs_hook=mode.strict_pairs)
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError, mode.Reject):
        reject("running-inventory-invalid")
    if not isinstance(inspected, list) or len(inspected) != len(rows):
        reject("running-inventory-invalid")

    actual: list[dict[str, Any]] = []
    for row, item in zip(rows, inspected, strict=True):
        if not isinstance(item, dict):
            reject("running-inventory-invalid")
        config = item.get("Config")
        labels = config.get("Labels") if isinstance(config, dict) else None
        labels = labels if isinstance(labels, dict) else {}
        live_project = labels.get("com.docker.compose.project", "")
        service = labels.get("com.docker.compose.service", "")
        state = item.get("State")
        raw_mounts = item.get("Mounts")
        if (
            item.get("Id") != row[0]
            or not isinstance(live_project, str)
            or not isinstance(service, str)
            or live_project != row[1]
            or service != row[2]
            or not isinstance(state, dict)
            or state.get("Status") != "running"
            or not isinstance(raw_mounts, list)
        ):
            reject("running-inventory-invalid")
        if service.startswith("teagramd"):
            reject("writer-freeze-not-held")

        mounts: list[dict[str, Any]] = []
        for mount in raw_mounts:
            if not isinstance(mount, dict):
                reject("running-inventory-invalid")
            kind = mount.get("Type")
            source = mount.get("Name", "") if kind == "volume" else mount.get("Source", "")
            target = mount.get("Destination")
            rw = mount.get("RW")
            if (
                kind not in {"bind", "volume", "tmpfs"}
                or not isinstance(source, str)
                or not isinstance(target, str)
                or not isinstance(rw, bool)
            ):
                reject("running-inventory-invalid")
            if rw:
                if kind == "volume" and source == source_volume:
                    reject("writer-freeze-not-held")
                if kind == "bind":
                    try:
                        bind_source = pathlib.Path(source).resolve(strict=False)
                    except OSError:
                        reject("running-inventory-invalid")
                    if (
                        bind_source == source_mountpoint
                        or bind_source.is_relative_to(source_mountpoint)
                        or source_mountpoint.is_relative_to(bind_source)
                    ):
                        reject("writer-freeze-not-held")
            mounts.append({"type": kind, "source": source, "target": target, "rw": rw})

        if live_project != project:
            continue
        if not service:
            reject("frozen-inventory-invalid")
        raw_env = config.get("Env") if isinstance(config, dict) else None
        if not isinstance(raw_env, list):
            reject("frozen-inventory-invalid")
        env_values: dict[str, str] = {}
        for value in raw_env:
            if not isinstance(value, str) or "=" not in value:
                reject("frozen-inventory-invalid")
            key, setting = value.split("=", 1)
            if key in env_values:
                reject("frozen-inventory-invalid")
            env_values[key] = setting
        ports: list[dict[str, Any]] = []
        network = item.get("NetworkSettings")
        raw_ports = network.get("Ports", {}) if isinstance(network, dict) else {}
        if not isinstance(raw_ports, dict):
            reject("frozen-inventory-invalid")
        for binding, values in raw_ports.items():
            match = re.fullmatch(r"([1-9][0-9]*)/(tcp|udp|sctp)", binding)
            if match is None:
                reject("frozen-inventory-invalid")
            for value in values or []:
                if not isinstance(value, dict) or not isinstance(value.get("HostIp", ""), str) or not isinstance(value.get("HostPort", ""), str):
                    reject("frozen-inventory-invalid")
                ports.append({
                    "target": int(match.group(1)), "published": value.get("HostPort", ""),
                    "host_ip": value.get("HostIp", ""), "protocol": match.group(2),
                })
        actual.append({
            "id": item.get("Id"), "name": item.get("Name"), "service": service,
            "running": True, "mounts": mounts, "environment": env_values, "ports": ports,
        })
    return actual


def require_writers_stopped(
    project: str,
    source_volume: str,
    output_dir: pathlib.Path,
    checkout: pathlib.Path,
    environment: dict[str, str],
    name: str,
) -> set[str]:
    containers = live_host_inventory(project, source_volume, output_dir, checkout, environment, name)
    return {container["service"] for container in containers}


def inventory_projection(container: Any) -> tuple[Any, ...]:
    if not isinstance(container, dict):
        reject("frozen-inventory-invalid")
    identifier = container.get("id")
    service = container.get("service")
    name = container.get("name")
    mounts = container.get("mounts")
    environment = container.get("environment")
    ports = container.get("ports", [])
    if (
        not isinstance(identifier, str)
        or re.fullmatch(r"[0-9a-f]{64}", identifier) is None
        or not isinstance(service, str)
        or not service
        or not isinstance(name, str)
        or not isinstance(mounts, list)
        or not isinstance(environment, dict)
        or not isinstance(ports, list)
        or container.get("running") is not True
    ):
        reject("frozen-inventory-invalid")
    normalized_mounts = []
    for mount in mounts:
        if (
            not isinstance(mount, dict)
            or mount.get("type") not in {"bind", "volume", "tmpfs"}
            or not isinstance(mount.get("source"), str)
            or not isinstance(mount.get("target"), str)
            or not isinstance(mount.get("rw"), bool)
        ):
            reject("frozen-inventory-invalid")
        normalized_mounts.append((mount["type"], mount["source"], mount["target"], mount["rw"]))
    normalized_ports = []
    for port in ports:
        if (
            not isinstance(port, dict)
            or type(port.get("target")) is not int
            or not isinstance(port.get("published", ""), (str, int))
            or not isinstance(port.get("host_ip", ""), str)
            or not isinstance(port.get("protocol", "tcp"), str)
        ):
            reject("frozen-inventory-invalid")
        normalized_ports.append((
            port["target"], str(port.get("published", "")),
            port.get("host_ip", ""), port.get("protocol", "tcp"),
        ))
    if not all(isinstance(key, str) and isinstance(value, str) for key, value in environment.items()):
        reject("frozen-inventory-invalid")
    return (
        identifier, name, service, tuple(sorted(normalized_mounts)),
        tuple(sorted(environment.items())), tuple(sorted(normalized_ports)),
    )


def capture_live_frozen_inventory(
    project: str,
    source_volume: str,
    expected_path: pathlib.Path,
    output_dir: pathlib.Path,
    checkout: pathlib.Path,
    environment: dict[str, str],
    name: str,
) -> tuple[pathlib.Path, str]:
    actual = live_host_inventory(project, source_volume, output_dir, checkout, environment, name)
    mode = import_mode_helper()
    expected = mode.read_json(expected_path)
    if not isinstance(expected, dict) or expected.get("complete") is not True or not isinstance(expected.get("containers"), list):
        reject("frozen-inventory-invalid")
    if sorted(inventory_projection(item) for item in actual) != sorted(
        inventory_projection(item) for item in expected["containers"]
    ):
        reject("frozen-inventory-mismatch")
    captured_at = dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")
    path = output_dir / f"{name}.json"
    document = {"complete": True, "captured_at": captured_at, "containers": sorted(actual, key=lambda item: (item["service"], item["id"]))}
    write_synced(path, (json.dumps(document, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8"))
    return path, captured_at


def file_timestamp(path: pathlib.Path) -> str:
    try:
        timestamp = dt.datetime.fromtimestamp(path.stat().st_mtime, dt.timezone.utc)
    except OSError:
        reject("private-evidence-unavailable")
    return timestamp.isoformat(timespec="seconds").replace("+00:00", "Z")


def utc_timestamp() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def manifest_command(
    bundle: pathlib.Path,
    output_dir: pathlib.Path,
    name: str,
    direction: str | None = None,
    *,
    service: str = "blob-migrate",
    extra_args: tuple[str, ...] = (),
    artifact_dir: pathlib.Path | None = None,
    env: dict[str, str] | None = None,
    cwd: pathlib.Path | None = None,
) -> pathlib.Path:
    artifact_dir = bundle if artifact_dir is None else artifact_dir
    destination = artifact_dir / f"{name}.tsv"
    if destination.exists() or destination.is_symlink():
        reject("phase-artifact-already-exists")
    command = ["docker", "compose", "run", "--rm", "--no-deps", service]
    if direction is not None:
        command.extend(("--direction", direction))
    command.extend((*extra_args, "--manifest", "stdout"))
    output = run_command(command, output_dir, name, cwd=cwd, env=env)
    try:
        fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "wb") as stream:
            stream.write(output)
            stream.flush()
            os.fchown(stream.fileno(), 0, 0)
            os.fchmod(stream.fileno(), 0o600)
            os.fsync(stream.fileno())
        dir_fd = os.open(artifact_dir, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except FileExistsError:
        reject("phase-artifact-already-exists")
    except OSError:
        reject("phase-artifact-write-failed")
    secure_file(destination)
    return destination


def capture_fresh_database_dump(
    output_dir: pathlib.Path,
    checkout: pathlib.Path,
    environment: dict[str, str],
    expected_sha256: str,
) -> pathlib.Path:
    path = run_private_command(
        ["docker", "compose", "exec", "-T", "postgres", "pg_dump", "-U", "postgres", "teagram"],
        output_dir,
        "fresh-postgres-dump",
        cwd=checkout,
        env=environment,
    )
    try:
        with path.open("rb") as stream:
            stream.seek(max(0, path.stat().st_size - 200))
            tail = stream.read(200)
    except OSError:
        reject("fresh-database-dump-unavailable")
    if not re.search(rb"dump complete", tail, re.IGNORECASE):
        reject("fresh-database-dump-incomplete")
    if sha256_file(path) != expected_sha256:
        reject("fresh-database-dump-mismatch")
    return path


def capture_pre_cutover_database_dump(
    output_dir: pathlib.Path,
    checkout: pathlib.Path,
    environment: dict[str, str],
) -> pathlib.Path:
    path = run_private_command(
        ["docker", "compose", "exec", "-T", "postgres", "pg_dump", "-U", "postgres", "teagram"],
        output_dir,
        "pre-cutover-postgres-dump",
        cwd=checkout,
        env=environment,
    )
    try:
        with path.open("rb") as stream:
            stream.seek(max(0, path.stat().st_size - 200))
            tail = stream.read(200)
    except OSError:
        reject("pre-cutover-database-dump-unavailable")
    if not re.search(rb"dump complete", tail, re.IGNORECASE):
        reject("pre-cutover-database-dump-incomplete")
    return path


def verify_pre_cutover_database_dump(
    dump_path: pathlib.Path,
    output_dir: pathlib.Path,
    checkout: pathlib.Path,
    environment: dict[str, str],
) -> pathlib.Path:
    database = f"blob_restore_{uuid.uuid4().hex}"
    run_command(
        ["docker", "compose", "exec", "-T", "postgres", "createdb", "-U", "postgres", database],
        output_dir,
        "create-dump-restore-database",
        cwd=checkout,
        env=environment,
    )
    try:
        run_command_with_input_file(
            [
                "docker", "compose", "exec", "-T", "postgres", "psql", "-X",
                "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", database,
            ],
            dump_path,
            output_dir,
            "restore-pre-cutover-dump",
            cwd=checkout,
            env=environment,
        )
    finally:
        run_command(
            ["docker", "compose", "exec", "-T", "postgres", "dropdb", "-U", "postgres", database],
            output_dir,
            "drop-dump-restore-database",
            cwd=checkout,
            env=environment,
        )
    dump_sha256 = sha256_file(dump_path)
    result_path = output_dir / "dump-restore-result.txt"
    write_synced(
        result_path,
        f"restore=pass database={database} dump_sha256={dump_sha256}\n".encode("ascii"),
    )
    return result_path


def capture_baseline_live_migration_schema(
    bundle: pathlib.Path,
    output_dir: pathlib.Path,
    checkout: pathlib.Path,
    environment: dict[str, str],
    name: str,
) -> dict[str, Any] | None:
    qualifier = import_qualifier()
    release_set = qualifier.select_migration_release(bundle)
    if release_set not in {"60-67", "60-69", "60-70"}:
        return None
    query = qualifier.live_schema_query(release_set)
    query_output = run_private_command(
        [
            "docker", "compose", "exec", "-T", "postgres", "psql", "-X",
            "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "teagram",
            "-A", "-t", "-c", query,
        ],
        output_dir,
        name,
        cwd=checkout,
        env=environment,
    )
    try:
        raw = query_output.read_bytes()
        observation = json.loads(
            raw.decode("utf-8"),
            object_pairs_hook=qualifier.no_duplicate_keys,
            parse_constant=qualifier.no_non_json_constant,
        )
        qualifier.validate_migration_schema(bundle, checkout, live_observation=observation)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError, qualifier.GateReject):
        reject("baseline-live-schema-rejected")
    return {
        "schema": "teagram.live-migration-schema/v1",
        "captured_at": utc_timestamp(),
        "query_sha256": qualifier.live_schema_query_sha256(release_set),
        "query_output_sha256": hashlib.sha256(raw).hexdigest(),
        "observed": observation,
    }


def capture_live_migration_schema(
    bundle: pathlib.Path,
    output_dir: pathlib.Path,
    checkout: pathlib.Path,
    environment: dict[str, str],
    dump_sha256: str,
    name: str,
    baseline_capture: dict[str, Any] | None = None,
) -> str:
    qualifier = import_qualifier()
    release_set = qualifier.select_migration_release(bundle)
    query = qualifier.live_schema_query(release_set)
    query_output = run_private_command(
        [
            "docker", "compose", "exec", "-T", "postgres", "psql", "-X",
            "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "teagram",
            "-A", "-t", "-c", query,
        ],
        output_dir,
        name,
        cwd=checkout,
        env=environment,
    )
    try:
        raw = query_output.read_bytes()
        observation = json.loads(
            raw.decode("utf-8"),
            object_pairs_hook=qualifier.no_duplicate_keys,
            parse_constant=qualifier.no_non_json_constant,
        )
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError):
        reject("live-schema-evidence-invalid")
    if sha256_file(bundle / "postgres.dump") != dump_sha256:
        reject("live-schema-dump-binding-invalid")

    try:
        metadata = qualifier.read_json(bundle / "migrations.json")
        qualifier.validate_migration_schema(
            bundle,
            checkout,
            live_observation=observation,
            expected_dump_sha256=dump_sha256,
        )
    except qualifier.GateReject:
        reject("live-schema-rejected")
    if not isinstance(metadata, dict):
        reject("live-schema-evidence-invalid")
    if baseline_capture is not None:
        if baseline_capture.get("observed") != observation:
            reject("baseline-live-schema-changed")
        metadata["baseline_live_capture"] = {
            **baseline_capture,
            "dump_sha256": dump_sha256,
        }
    captured_at = utc_timestamp()
    metadata["live_capture"] = {
        "schema": "teagram.live-migration-schema/v1",
        "captured_at": captured_at,
        "dump_sha256": dump_sha256,
        "query_sha256": qualifier.live_schema_query_sha256(release_set),
        "query_output_sha256": hashlib.sha256(raw).hexdigest(),
        "observed": observation,
    }
    replace_synced(
        bundle / "migrations.json",
        (json.dumps(metadata, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8"),
    )
    return captured_at


def capture_live_r70_inert_surfaces(
    bundle: pathlib.Path,
    output_dir: pathlib.Path,
    checkout: pathlib.Path,
    environment: dict[str, str],
    name: str,
) -> str:
    qualifier = import_qualifier()
    query_path = SCRIPT_DIR / "rustfs-r70-inert-surfaces.sql"
    try:
        query_bytes = query_path.read_bytes()
        query = query_bytes.decode("utf-8")
    except (OSError, UnicodeDecodeError):
        reject("live-inert-surfaces-unavailable")
    if hashlib.sha256(query_bytes).hexdigest() != qualifier.R70_INERT_SURFACES_QUERY_SHA256:
        reject("live-inert-surfaces-rejected")
    query_output = run_private_command(
        [
            "docker", "compose", "exec", "-T", "postgres", "psql", "-X",
            "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "teagram",
            "-A", "-t", "-c", query,
        ],
        output_dir,
        name,
        cwd=checkout,
        env=environment,
    )
    try:
        raw = query_output.read_bytes()
        observed = json.loads(
            raw.decode("utf-8"),
            object_pairs_hook=qualifier.no_duplicate_keys,
            parse_constant=qualifier.no_non_json_constant,
        )
        if not isinstance(observed, dict):
            reject("live-inert-surfaces-rejected")
        qualifier.validate_inert_surfaces({"inert_surfaces": observed}, qualifier.R70_INERT_SURFACES)
        metadata = qualifier.read_json(bundle / "migrations.json")
        if not isinstance(metadata, dict):
            reject("live-inert-surfaces-rejected")
        metadata["inert_surfaces"] = observed
    except qualifier.GateReject as exc:
        reject(exc.reason)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError):
        reject("live-inert-surfaces-rejected")
    replace_synced(
        bundle / "migrations.json",
        (json.dumps(metadata, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8"),
    )
    return utc_timestamp()


def capture_fresh_cutover_evidence(
    bundle: pathlib.Path,
    output_dir: pathlib.Path,
    checkout: pathlib.Path,
    environment: dict[str, str],
    source_volume: str,
    freeze_started_at: str,
) -> dict[str, pathlib.Path]:
    try:
        candidate_compose = json.loads((bundle / "candidate-compose.json").read_text(encoding="utf-8"))
        project = candidate_compose["name"]
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, KeyError, TypeError):
        reject("compose-project-invalid")
    if not isinstance(project, str):
        reject("compose-project-invalid")
    frozen_path, _frozen_at = capture_live_frozen_inventory(
        project,
        source_volume,
        bundle / "frozen-containers.json",
        output_dir,
        checkout,
        environment,
        "fresh-frozen-inventory",
    )
    qualifier = import_qualifier()
    try:
        qualification = json.loads((bundle / "qualification.json").read_text(encoding="utf-8"))
        expected_dump = qualification["freeze"]["postgres_dump"]["sha256"]
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, KeyError, TypeError):
        reject("qualification-bundle-invalid")
    if not isinstance(expected_dump, str) or re.fullmatch(r"[0-9a-f]{64}", expected_dump) is None:
        reject("qualification-bundle-invalid")
    if sha256_file(bundle / "postgres.dump") != expected_dump:
        reject("fresh-database-dump-provenance-mismatch")
    release_set = qualifier.select_migration_release(bundle)
    baseline_schema_capture = capture_baseline_live_migration_schema(
        bundle, output_dir, checkout, environment, "fresh-cutover-baseline-migration-schema"
    )
    schema_captured_at = None
    inert_surfaces_captured_at = None
    if release_set == "60-70":
        schema_captured_at = capture_live_migration_schema(
            bundle, output_dir, checkout, environment, expected_dump,
            "fresh-cutover-migration-schema", baseline_schema_capture,
        )
        inert_surfaces_captured_at = capture_live_r70_inert_surfaces(
            bundle, output_dir, checkout, environment, "fresh-cutover-r70-inert-surfaces"
        )
    fresh_dump = capture_fresh_database_dump(output_dir, checkout, environment, expected_dump)
    replace_with_synced_file(bundle / "postgres.dump", fresh_dump)
    if schema_captured_at is None:
        schema_captured_at = capture_live_migration_schema(
            bundle, output_dir, checkout, environment, expected_dump,
            "fresh-cutover-migration-schema", baseline_schema_capture,
        )

    phase_paths: dict[str, pathlib.Path] = {"schema_evidence_sha256": bundle / "migrations.json"}
    for phase, query in (
        ("reference_rows_sha256", qualifier.REFERENCE_QUERY),
        ("active_links_sha256", qualifier.ACTIVE_LINKS_QUERY),
    ):
        evidence_path = run_private_command(
            [
                "docker", "compose", "exec", "-T", "postgres", "psql", "-X",
                "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "teagram",
                "-A", "-t", "-F", "\t", "-c", query,
            ],
            output_dir,
            f"fresh-{phase.removesuffix('_sha256')}",
            cwd=checkout,
            env=environment,
        )
        evidence_name = "references.tsv" if phase == "reference_rows_sha256" else "active-links.tsv"
        provisional_name = "references-provisional.tsv" if phase == "reference_rows_sha256" else "active-links-provisional.tsv"
        if not files_equal(evidence_path, bundle / evidence_name) or not files_equal(evidence_path, bundle / provisional_name):
            reject("fresh-reference-evidence-mismatch")
        phase_paths[phase] = evidence_path

    fresh_source = manifest_command(
        bundle,
        output_dir,
        "fresh-source-census",
        "local-census",
        service="blob-migrate",
        artifact_dir=output_dir,
        cwd=checkout,
        env=environment,
    )
    if not files_equal(fresh_source, bundle / "source-provisional.tsv") or not files_equal(fresh_source, bundle / "source-frozen.tsv"):
        reject("fresh-source-census-mismatch")
    phase_paths["source_frozen_sha256"] = fresh_source
    phase_paths["frozen_inventory_sha256"] = frozen_path
    try:
        frozen_document = json.loads(frozen_path.read_text(encoding="utf-8"))
        frozen_document["captured_at"] = max(
            file_timestamp(fresh_dump),
            schema_captured_at,
            inert_surfaces_captured_at or schema_captured_at,
            file_timestamp(phase_paths["reference_rows_sha256"]),
            file_timestamp(phase_paths["active_links_sha256"]),
            file_timestamp(fresh_source),
        )
        frozen_bytes = (json.dumps(frozen_document, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
        replace_synced(frozen_path, frozen_bytes)
        replace_synced(bundle / "frozen-containers.json", frozen_bytes)
        freeze = qualification["freeze"]
        freeze["started_at"] = freeze_started_at
        freeze["dump_captured_at"] = file_timestamp(fresh_dump)
        freeze["references_captured_at"] = max(
            file_timestamp(phase_paths["reference_rows_sha256"]),
            file_timestamp(phase_paths["active_links_sha256"]),
        )
        freeze["source_frozen_census_at"] = file_timestamp(fresh_source)
        freeze["schema_captured_at"] = schema_captured_at
        if inert_surfaces_captured_at is not None:
            freeze["inert_surfaces_captured_at"] = inert_surfaces_captured_at
        else:
            freeze.pop("inert_surfaces_captured_at", None)
        if baseline_schema_capture is not None:
            freeze["baseline_schema_captured_at"] = baseline_schema_capture["captured_at"]
        else:
            freeze.pop("baseline_schema_captured_at", None)
        freeze["postgres_dump"]["captured_at"] = file_timestamp(fresh_dump)
        freeze["held_at"] = max(utc_timestamp(), frozen_document["captured_at"])
        replace_synced(
            bundle / "qualification.json",
            (json.dumps(qualification, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8"),
        )
        try:
            qualifier.validate_migration_schema(
                bundle,
                checkout,
                expected_dump_sha256=expected_dump,
                require_live_capture=True,
            )
        except qualifier.GateReject:
            reject("live-schema-rejected")
    except (KeyError, TypeError):
        reject("qualification-bundle-invalid")
    return phase_paths


def capture_fresh_recovery_evidence(
    bundle: pathlib.Path,
    output_dir: pathlib.Path,
    checkout: pathlib.Path,
    environment: dict[str, str],
    recovery: dict[str, Any],
    project: str,
    source_volume: str,
    freeze_started_at: str,
) -> dict[str, pathlib.Path]:
    frozen_path, _frozen_at = capture_live_frozen_inventory(
        project,
        source_volume,
        bundle / "frozen-containers.json",
        output_dir,
        checkout,
        environment,
        "fresh-recovery-frozen-inventory",
    )
    expected_dump = recovery.get("dump", {}).get("sha256") if isinstance(recovery.get("dump"), dict) else None
    if not isinstance(expected_dump, str) or re.fullmatch(r"[0-9a-f]{64}", expected_dump) is None:
        reject("recovery-dump-invalid")
    if sha256_file(bundle / "postgres.dump") != expected_dump:
        reject("recovery-dump-invalid")
    qualifier = import_qualifier()
    if sha256_file(bundle / "migrations.json") != recovery.get("schema_evidence_sha256"):
        reject("recovery-schema-invalid")
    try:
        qualifier.validate_migration_schema(bundle, checkout)
    except qualifier.GateReject:
        reject("recovery-schema-invalid")
    release_set = qualifier.select_migration_release(bundle)
    baseline_schema_capture = capture_baseline_live_migration_schema(
        bundle, output_dir, checkout, environment, "fresh-recovery-baseline-migration-schema"
    )
    schema_captured_at = None
    inert_surfaces_captured_at = None
    if release_set == "60-70":
        schema_captured_at = capture_live_migration_schema(
            bundle, output_dir, checkout, environment, expected_dump,
            "fresh-recovery-migration-schema", baseline_schema_capture,
        )
        inert_surfaces_captured_at = capture_live_r70_inert_surfaces(
            bundle, output_dir, checkout, environment, "fresh-recovery-r70-inert-surfaces"
        )
    fresh_dump = capture_fresh_database_dump(output_dir, checkout, environment, expected_dump)
    replace_with_synced_file(bundle / "postgres.dump", fresh_dump)
    if schema_captured_at is None:
        schema_captured_at = capture_live_migration_schema(
            bundle, output_dir, checkout, environment, expected_dump,
            "fresh-recovery-migration-schema", baseline_schema_capture,
        )
    phase_paths: dict[str, pathlib.Path] = {
        "dump_sha256": fresh_dump,
        "frozen_inventory_sha256": frozen_path,
    }
    for phase, query, name in (
        ("recovery_reference_rows_sha256", qualifier.REFERENCE_QUERY, "references"),
        ("recovery_active_links_sha256", qualifier.ACTIVE_LINKS_QUERY, "active-links"),
    ):
        evidence_path = run_private_command(
            [
                "docker", "compose", "exec", "-T", "postgres", "psql", "-X",
                "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "teagram",
                "-A", "-t", "-F", "\t", "-c", query,
            ],
            output_dir,
            f"fresh-recovery-{name}",
            cwd=checkout,
            env=environment,
        )
        phase_paths[phase] = evidence_path
        try:
            evidence_size = evidence_path.stat().st_size
            with evidence_path.open("rb") as stream:
                stream.seek(max(0, evidence_size - 1))
                terminated = stream.read(1) == b"\n"
        except OSError:
            reject("fresh-reference-evidence-unavailable")
        empty_r70_active_links = release_set == "60-70" and name == "active-links" and evidence_size == 0
        if not terminated and not empty_r70_active_links:
            reject("fresh-reference-evidence-invalid")
    try:
        frozen_document = json.loads(frozen_path.read_text(encoding="utf-8"))
        frozen_document["captured_at"] = max(
            file_timestamp(fresh_dump),
            schema_captured_at,
            inert_surfaces_captured_at or schema_captured_at,
            file_timestamp(phase_paths["recovery_reference_rows_sha256"]),
            file_timestamp(phase_paths["recovery_active_links_sha256"]),
        )
        frozen_bytes = (json.dumps(frozen_document, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
        replace_synced(frozen_path, frozen_bytes)
        replace_synced(bundle / "frozen-containers.json", frozen_bytes)
        recovery["freeze"]["started_at"] = freeze_started_at
        recovery["freeze"]["schema_captured_at"] = schema_captured_at
        if baseline_schema_capture is not None:
            recovery["freeze"]["baseline_schema_captured_at"] = baseline_schema_capture["captured_at"]
        else:
            recovery["freeze"].pop("baseline_schema_captured_at", None)
        if inert_surfaces_captured_at is not None:
            recovery["freeze"]["inert_surfaces_captured_at"] = inert_surfaces_captured_at
            recovery["references"] = {
                "inert_surfaces_query_sha256": qualifier.R70_INERT_SURFACES_QUERY_SHA256,
            }
        else:
            recovery["freeze"].pop("inert_surfaces_captured_at", None)
            recovery.pop("references", None)
        recovery["freeze"]["held_at"] = max(utc_timestamp(), frozen_document["captured_at"])
        recovery["dump"]["captured_at"] = file_timestamp(fresh_dump)
        recovery["schema_evidence_sha256"] = sha256_file(bundle / "migrations.json")
        replace_synced(
            bundle / "recovery.json",
            (json.dumps(recovery, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8"),
        )
    except (KeyError, TypeError, AttributeError):
        reject("recovery-qualification-invalid")
    return phase_paths


def bundle_manifest(bundle: pathlib.Path, output_dir: pathlib.Path) -> pathlib.Path:
    try:
        entries = []
        for path in sorted(bundle.rglob("*"), key=lambda item: item.relative_to(bundle).as_posix()):
            info = path.lstat()
            if stat.S_ISLNK(info.st_mode):
                reject("bundle-entry-invalid")
            if stat.S_ISDIR(info.st_mode):
                if path.relative_to(bundle).as_posix() != "candidate-secrets":
                    reject("bundle-entry-invalid")
                secure_directory(path)
                continue
            if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) not in (0o600, 0o444):
                reject("bundle-entry-invalid")
            entries.append({
                "name": path.relative_to(bundle).as_posix(),
                "sha256": sha256_file(path) if stat.S_IMODE(info.st_mode) == 0o600 else _sha256_regular(path),
                "size": info.st_size,
            })
    except OSError:
        reject("bundle-unavailable")
    path = output_dir / "qualification-bundle.json"
    write_synced(path, (json.dumps(entries, sort_keys=True, separators=(",", ":")) + "\n").encode("ascii"))
    return path


def _sha256_regular(path: pathlib.Path) -> str:
    try:
        before = path.lstat()
        if not stat.S_ISREG(before.st_mode) or before.st_uid != 0 or stat.S_IMODE(before.st_mode) != 0o444:
            reject("private-evidence-invalid")
        fd = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
    except TransitionReject:
        raise
    except OSError:
        reject("private-evidence-unavailable")
    digest = hashlib.sha256()
    try:
        with os.fdopen(fd, "rb") as stream:
            after = os.fstat(stream.fileno())
            if (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino):
                reject("private-evidence-changed")
            for block in iter(lambda: stream.read(1024 * 1024), b""):
                digest.update(block)
    except OSError:
        reject("private-evidence-unavailable")
    return digest.hexdigest()


def relative_phase_paths(report_root: pathlib.Path, phase_files: dict[str, pathlib.Path]) -> dict[str, str]:
    result: dict[str, str] = {}
    for key, path in phase_files.items():
        try:
            result[key] = path.resolve(strict=True).relative_to(report_root.resolve(strict=True)).as_posix()
        except (OSError, ValueError):
            reject("phase-evidence-outside-report-root")
    return result


def import_mode_helper() -> Any:
    spec = importlib.util.spec_from_file_location("blob_mode_state", MODE_HELPER)
    if spec is None or spec.loader is None:
        reject("blob-mode-helper-unavailable")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def import_qualifier() -> Any:
    spec = importlib.util.spec_from_file_location("qualify_rustfs_transition", SCRIPT_DIR / "qualify-rustfs-transition.py")
    if spec is None or spec.loader is None:
        reject("qualification-helper-unavailable")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def compose_environment(checkout: pathlib.Path, local: bool = False) -> dict[str, str]:
    path = os.environ.get("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
    if TEST_MODE:
        if TEST_FIXTURE_DOCKER is None:
            reject("synthetic-fixture-scope-required")
        path = f"{TEST_FIXTURE_DOCKER.parent}:{path}"
    else:
        path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin"
    files = ["docker-compose.yml"]
    if (checkout / "docker-compose.override.yml").is_file():
        files.append("docker-compose.override.yml")
    if local:
        overlay = checkout / "docker-compose.local-blobs.yml"
        if not overlay.is_file() or overlay.is_symlink():
            reject("local-compose-overlay-unavailable")
        files.append("docker-compose.local-blobs.yml")
    environment = {
        "PATH": path,
        "HOME": os.environ.get("HOME", "/root"),
        "COMPOSE_FILE": ":".join(files),
    }
    if TEST_MODE:
        for key in (
            "MOCK_LOCAL_COMPOSE_JSON", "MOCK_POSTGRES_DUMP",
            "MOCK_REFERENCE_BUNDLE", "MOCK_LIVE_SCHEMA",
            "MOCK_LIVE_R70_INERT_SURFACES", "MOCK_FROZEN_PS",
            "MOCK_FROZEN_INSPECT", "MOCK_SOURCE_VOLUME_MOUNTPOINT",
            "MOCK_WRITER_RUNNING", "MOCK_LIVE_PS", "MOCK_LIVE_COMPOSE_FILES",
            "MOCK_AUTHORITY_INSPECT",
            "MOCK_DUMP_RESTORE_FAIL",
        ):
            if os.environ.get(key):
                environment[key] = os.environ[key]
    return environment


def private_root_file(path: pathlib.Path, expected_mode: int) -> bytes:
    try:
        fd = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
    except OSError:
        reject("rustfs-credentials-unavailable")
    try:
        info = os.fstat(fd)
        if (
            not stat.S_ISREG(info.st_mode)
            or info.st_uid != 0
            or stat.S_IMODE(info.st_mode) != expected_mode
            or info.st_size > 65_536
        ):
            reject("rustfs-credentials-permissions")
        with os.fdopen(fd, "rb") as stream:
            fd = -1
            return stream.read()
    except OSError:
        reject("rustfs-credentials-unavailable")
    finally:
        if fd >= 0:
            os.close(fd)


def parse_compose_env_file(path: pathlib.Path) -> dict[str, list[str]]:
    try:
        lines = private_root_file(path, 0o600).decode("utf-8").splitlines()
    except UnicodeDecodeError:
        reject("rustfs-credentials-invalid")
    values: dict[str, list[str]] = {}
    for line in lines:
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        values.setdefault(key, []).append(value)
    return values


def rustfs_compose_environment_files(checkout: pathlib.Path) -> list[str]:
    rustfs_keys = (
        "RUSTFS_ROOT_ACCESS_KEY",
        "RUSTFS_ROOT_SECRET_KEY",
        "TG_BLOB_S3_ACCESS_KEY_ID",
        "TG_BLOB_S3_SECRET_ACCESS_KEY",
    )
    required_keys = rustfs_keys
    base_env = checkout / ".env"
    base_values = parse_compose_env_file(base_env)
    if any(key in base_values for key in rustfs_keys):
        reject("rustfs-credentials-conflict")
    candidates = [
        checkout / ".env.rustfs-parked",
        checkout.parent / "telegram-server" / ".env.rustfs-parked",
        pathlib.Path("/root/.env.rustfs-parked"),
    ]
    parked = [path for path in dict.fromkeys(candidates) if path.exists() or path.is_symlink()]
    if len(parked) != 1:
        reject("rustfs-credentials-unavailable" if not parked else "rustfs-credentials-ambiguous")
    parked_path = parked[0]
    values = parse_compose_env_file(parked_path)
    if any(len(values.get(key, [])) != 1 for key in required_keys):
        reject("rustfs-credentials-invalid")
    credentials = {key: values[key][0] for key in required_keys}
    if (
        re.fullmatch(r"[0-9a-f]{20}", credentials["RUSTFS_ROOT_ACCESS_KEY"]) is None
        or re.fullmatch(r"[0-9a-f]{64}", credentials["RUSTFS_ROOT_SECRET_KEY"]) is None
        or re.fullmatch(r"[0-9a-f]{20}", credentials["TG_BLOB_S3_ACCESS_KEY_ID"]) is None
        or credentials["RUSTFS_ROOT_ACCESS_KEY"] == credentials["TG_BLOB_S3_ACCESS_KEY_ID"]
    ):
        reject("rustfs-credentials-invalid")
    secret_dir = checkout / ".secrets"
    try:
        dir_info = secret_dir.lstat()
    except OSError:
        reject("rustfs-credentials-unavailable")
    if (
        not stat.S_ISDIR(dir_info.st_mode)
        or stat.S_ISLNK(dir_info.st_mode)
        or dir_info.st_uid != 0
        or stat.S_IMODE(dir_info.st_mode) != 0o700
    ):
        reject("rustfs-credentials-permissions")
    app_secret = private_root_file(secret_dir / "teagramd-blob-secret-key", 0o444)
    try:
        app_secret_text = app_secret.decode("ascii")
    except UnicodeDecodeError:
        reject("rustfs-credentials-invalid")
    if re.fullmatch(r"[0-9a-f]{64}", app_secret_text) is None:
        reject("rustfs-credentials-invalid")
    if app_secret_text != credentials["TG_BLOB_S3_SECRET_ACCESS_KEY"]:
        reject("rustfs-credentials-mismatch")
    return [str(base_env), str(parked_path.resolve(strict=True))]


def compose_environment_for_files(
    checkout: pathlib.Path,
    files: list[str],
    *,
    include_rustfs_credentials: bool = False,
) -> dict[str, str]:
    paths = []
    for name in files:
        path = checkout / name
        try:
            info = path.lstat()
        except OSError:
            reject("compose-file-unavailable")
        if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
            reject("compose-file-invalid")
        paths.append(str(path))
    environment = compose_environment(checkout)
    environment["COMPOSE_FILE"] = ":".join(paths)
    if include_rustfs_credentials and not TEST_MODE:
        environment[BLOB_TRANSITION_COMPOSE_ENV_FILES] = os.pathsep.join(
            rustfs_compose_environment_files(checkout)
        )
    return environment


def read_live_compose_files(
    checkout: pathlib.Path,
    project: str,
    output_dir: pathlib.Path,
    environment: dict[str, str],
    expected_files: list[str] | None = None,
) -> list[str]:
    running = run_command(
        [
            "docker", "ps", "--no-trunc", "--filter",
            f"label=com.docker.compose.project={project}", "--format",
            '{{.ID}}\t{{.Label "com.docker.compose.service"}}',
        ],
        output_dir,
        "live-compose-containers",
        cwd=checkout,
        env=environment,
    )
    try:
        rows = [line.split("\t") for line in running.decode("utf-8").splitlines() if line]
    except UnicodeDecodeError:
        reject("live-compose-selection-invalid")
    if not rows or any(
        len(row) != 2
        or re.fullmatch(r"[0-9a-f]{64}", row[0]) is None
        or not row[1]
        for row in rows
    ):
        reject("live-compose-selection-invalid")
    identifiers = [row[0] for row in rows if row[1].startswith("teagramd")]
    if not identifiers or len(identifiers) != len(set(identifiers)):
        reject("live-compose-selection-invalid")
    labels = run_command(
        [
            "docker", "inspect", "--format",
            '{{index .Config.Labels "com.docker.compose.project.config_files"}}',
            *identifiers,
        ],
        output_dir,
        "live-compose-labels",
        cwd=checkout,
        env=environment,
    )
    try:
        selections = [line.split(",") for line in labels.decode("utf-8").splitlines() if line]
        expected_files = expected_files or [
            "docker-compose.yml",
            "docker-compose.local-blobs.yml",
            "docker-compose.override.yml",
        ]
        expected = [str(checkout / name) for name in expected_files]
        resolved = [[str(pathlib.Path(value).resolve(strict=True)) for value in selection] for selection in selections]
        expected_resolved = [str(pathlib.Path(value).resolve(strict=True)) for value in expected]
    except (UnicodeDecodeError, OSError):
        reject("live-compose-selection-invalid")
    if len(resolved) != len(identifiers) or any(selection != expected_resolved for selection in resolved):
        reject("live-compose-selection-invalid")
    return expected


def parse_utc(value: Any, reason: str) -> Any:
    if not isinstance(value, str):
        reject(reason)
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        reject(reason)
    if parsed.tzinfo is None or parsed.utcoffset() != dt.timedelta(0):
        reject(reason)
    return parsed


def validate_recovery_bundle(bundle: pathlib.Path, checkout: pathlib.Path, source_volume: str) -> str:
    try:
        names = {path.name for path in bundle.iterdir()}
    except OSError:
        reject("recovery-bundle-unavailable")
    if names != RECOVERY_BUNDLE_FILES:
        reject("recovery-bundle-invalid")
    for name in RECOVERY_BUNDLE_FILES:
        secure_file(bundle / name)
    mode = import_mode_helper()
    qualifier = import_qualifier()
    release_set = qualifier.select_migration_release(bundle)
    recovery = mode.read_json(bundle / "recovery.json")
    expected_recovery_keys = {"schema", "source_volume", "freeze", "dump", "schema_evidence_sha256"}
    if release_set == "60-70":
        expected_recovery_keys.add("references")
    if (
        not isinstance(recovery, dict)
        or set(recovery) != expected_recovery_keys
        or recovery.get("schema") != "teagram.blob-recovery-qualification/v1"
        or recovery.get("source_volume") != source_volume
    ):
        reject("recovery-qualification-invalid")
    freeze = recovery.get("freeze")
    expected_freeze_keys = {"held", "inventory_complete", "started_at", "schema_captured_at", "held_at"}
    if release_set in {"60-67", "60-69", "60-70"}:
        expected_freeze_keys.add("baseline_schema_captured_at")
    if release_set == "60-70":
        expected_freeze_keys.add("inert_surfaces_captured_at")
    if (
        not isinstance(freeze, dict)
        or set(freeze) != expected_freeze_keys
        or freeze.get("held") is not True
        or freeze.get("inventory_complete") is not True
    ):
        reject("recovery-freeze-invalid")
    freeze_start = parse_utc(freeze.get("started_at"), "recovery-freeze-invalid")
    schema_captured = parse_utc(freeze.get("schema_captured_at"), "recovery-freeze-invalid")
    freeze_held = parse_utc(freeze.get("held_at"), "recovery-freeze-invalid")
    if not freeze_start <= schema_captured <= freeze_held:
        reject("recovery-freeze-invalid")
    baseline_schema_captured = None
    if release_set in {"60-67", "60-69", "60-70"}:
        baseline_schema_captured = parse_utc(
            freeze.get("baseline_schema_captured_at"), "recovery-freeze-invalid"
        )
        if not freeze_start <= baseline_schema_captured <= freeze_held:
            reject("recovery-freeze-invalid")
    dump = recovery.get("dump")
    if (
        not isinstance(dump, dict)
        or set(dump)
        != {
            "captured_at",
            "exit_status",
            "completion_marker",
            "isolated_restore_exit_status",
            "isolated_restore_network",
            "sha256",
        }
        or dump.get("exit_status") != 0
        or dump.get("completion_marker") is not True
        or dump.get("isolated_restore_exit_status") != 0
        or dump.get("isolated_restore_network") != "none"
        or not isinstance(dump.get("sha256"), str)
        or re.fullmatch(r"[0-9a-f]{64}", dump["sha256"]) is None
    ):
        reject("recovery-dump-invalid")
    dump_time = parse_utc(dump.get("captured_at"), "recovery-dump-invalid")
    if not freeze_start <= dump_time <= freeze_held or sha256_file(bundle / "postgres.dump") != dump["sha256"]:
        reject("recovery-dump-invalid")
    if baseline_schema_captured is not None and baseline_schema_captured > dump_time:
        reject("recovery-schema-invalid")
    if release_set == "60-70" and schema_captured > dump_time:
        reject("recovery-schema-invalid")
    frozen = mode.read_json(bundle / "frozen-containers.json")
    if (
        not isinstance(frozen, dict)
        or frozen.get("complete") is not True
        or not isinstance(frozen.get("containers"), list)
    ):
        reject("recovery-freeze-invalid")
    frozen_time = parse_utc(frozen.get("captured_at"), "recovery-freeze-invalid")
    if not freeze_start <= frozen_time <= freeze_held:
        reject("recovery-freeze-invalid")
    frozen_services: list[str] = []
    for container in frozen["containers"]:
        service = container.get("service") if isinstance(container, dict) else None
        if not isinstance(service, str) or not service or service.startswith("teagramd"):
            reject("recovery-freeze-invalid")
        frozen_services.append(service)
    if len(frozen_services) != len(set(frozen_services)) or frozen_services.count("postgres") != 1:
        reject("recovery-freeze-invalid")
    if sha256_file(bundle / "migrations.json") != recovery.get("schema_evidence_sha256"):
        reject("recovery-schema-invalid")
    try:
        qualifier.validate_migration_schema(
            bundle,
            checkout,
            expected_dump_sha256=dump["sha256"],
            require_live_capture=True,
        )
    except qualifier.GateReject:
        reject("recovery-schema-invalid")
    return release_set


def recovery_bundle_manifest(bundle: pathlib.Path, output_dir: pathlib.Path) -> pathlib.Path:
    entries = []
    for name in sorted(RECOVERY_BUNDLE_FILES):
        path = bundle / name
        entries.append({"name": name, "sha256": sha256_file(path), "size": path.stat().st_size})
    path = output_dir / "recovery-bundle.json"
    write_synced(path, (json.dumps(entries, sort_keys=True, separators=(",", ":")) + "\n").encode("ascii"))
    return path


def write_retained_manifest(mode: Any, before: pathlib.Path, s3: pathlib.Path, output_dir: pathlib.Path) -> pathlib.Path:
    s3_keys = {key for key, _size, _digest in mode.iter_manifest(s3)}
    retained = [(key, size, digest) for key, size, digest in mode.iter_manifest(before) if key not in s3_keys]
    content = "".join(f"{key}\t{size}\t{digest}\n" for key, size, digest in retained).encode("utf-8")
    path = output_dir / "retained-cutover-keys.tsv"
    write_synced(path, content)
    return path


def build_recovery_proof(
    bundle: pathlib.Path,
    output_dir: pathlib.Path,
    report_root: pathlib.Path,
    phase_paths: dict[str, pathlib.Path],
) -> pathlib.Path:
    mode = import_mode_helper()
    all_paths = {
        "recovery_bundle_sha256": recovery_bundle_manifest(bundle, output_dir),
        "qualification_output_sha256": output_dir / "qualification-recovery.stdout",
        "frozen_inventory_sha256": bundle / "frozen-containers.json",
        "dump_sha256": bundle / "postgres.dump",
        "schema_evidence_sha256": bundle / "migrations.json",
        **phase_paths,
    }
    phase_digests = {name: sha256_file(path) for name, path in all_paths.items()}
    s3_summary = mode.manifest_summary(all_paths["s3_census_pass_1_sha256"])
    local_summary = mode.manifest_summary(all_paths["local_census_pass_1_sha256"])
    proof = {
        "schema": mode.TRANSITION_PROOF_SCHEMA,
        "outcome": "recovered-local",
        "backend": {"kind": "local", "dir": "/var/lib/teagramd-blobs"},
        "volumes": {
            "tgblobs": json.loads((output_dir / "authority-before.json").read_text(encoding="utf-8"))["volumes"]["tgblobs"],
            "rustfsdata": json.loads((output_dir / "authority-before.json").read_text(encoding="utf-8"))["volumes"]["rustfsdata"],
        },
        "evidence": {
            "s3_census_manifest_sha256": s3_summary[0],
            "restored_manifest_sha256": local_summary[0],
            "object_count": local_summary[1],
            "byte_total": local_summary[2],
            "restore_passes": 2,
            "retained_cutover_key_count": sum(1 for _ in mode.iter_manifest(all_paths["retained_keys_sha256"])),
        },
        "phase_digests": phase_digests,
        "phase_files": relative_phase_paths(report_root, all_paths),
    }
    proof_path = output_dir / "transition-proof.json"
    write_synced(proof_path, (json.dumps(proof, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8"))
    return proof_path


def build_s3_proof(
    bundle: pathlib.Path,
    output_dir: pathlib.Path,
    report_root: pathlib.Path,
    fresh_phase_paths: dict[str, pathlib.Path],
) -> pathlib.Path:
    mode = import_mode_helper()
    compose = json.loads((bundle / "candidate-compose.json").read_text(encoding="utf-8"))
    baseline = json.loads((bundle / "baseline-compose.json").read_text(encoding="utf-8"))
    candidate_volumes = compose.get("volumes")
    baseline_volumes = baseline.get("volumes")
    if not isinstance(candidate_volumes, dict) or not isinstance(baseline_volumes, dict):
        reject("compose-volumes-invalid")
    volumes: dict[str, str] = {}
    for key in ("tgblobs", "rustfsdata"):
        item = candidate_volumes.get(key)
        name = item.get("name") if isinstance(item, dict) else None
        if not isinstance(name, str) or not name:
            reject("compose-volumes-invalid")
        volumes[key] = name
    baseline_names = {
        item.get("name") for item in baseline_volumes.values() if isinstance(item, dict)
    }
    if volumes["rustfsdata"] in baseline_names or volumes["rustfsdata"] == volumes["tgblobs"]:
        reject("rustfs-volume-alias")

    manifest_path = bundle_manifest(bundle, output_dir)
    phase_paths = {
        "baseline_inventory_sha256": bundle / "baseline-containers.json",
        "candidate_compose_sha256": bundle / "candidate-compose.json",
        "qualification_bundle_sha256": manifest_path,
        "qualification_output_sha256": output_dir / "qualification-check.stdout",
        "frozen_inventory_sha256": bundle / "frozen-containers.json",
        "dump_sha256": bundle / "postgres.dump",
        "reference_rows_sha256": bundle / "references.tsv",
        "active_links_sha256": bundle / "active-links.tsv",
        "schema_evidence_sha256": bundle / "migrations.json",
        "source_provisional_sha256": bundle / "source-provisional.tsv",
        "source_frozen_sha256": bundle / "source-frozen.tsv",
        "copy_pass_1_sha256": bundle / "copy-pass-1.tsv",
        "copy_pass_2_sha256": bundle / "copy-pass-2.tsv",
        "destination_census_pass_1_sha256": bundle / "destination-census-pass-1.tsv",
        "destination_census_pass_2_sha256": bundle / "destination-census-pass-2.tsv",
    }
    phase_paths.update(fresh_phase_paths)
    phase_digests = {name: sha256_file(path) for name, path in phase_paths.items()}
    summary = mode.manifest_summary(bundle / "source-frozen.tsv")
    proof = {
        "schema": mode.TRANSITION_PROOF_SCHEMA,
        "outcome": "s3-accepted",
        "backend": {"kind": "s3", "endpoint": "http://rustfs:9000", "bucket": "teagram", "prefix": "teagramd/"},
        "volumes": volumes,
        "evidence": {
            "source_manifest_sha256": summary[0],
            "destination_manifest_sha256": summary[0],
            "object_count": summary[1],
            "byte_total": summary[2],
            "copy_passes": 2,
        },
        "phase_digests": phase_digests,
        "phase_files": relative_phase_paths(report_root, phase_paths),
    }
    proof_path = output_dir / "transition-proof.json"
    write_synced(proof_path, (json.dumps(proof, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8"))
    return proof_path


def build_simple_s3_proof(
    output_dir: pathlib.Path,
    report_root: pathlib.Path,
    previous: dict[str, Any],
) -> pathlib.Path:
    mode = import_mode_helper()
    candidate = mode.read_json(output_dir / "candidate-compose.json")
    volumes_config = candidate.get("volumes") if isinstance(candidate, dict) else None
    if not isinstance(volumes_config, dict):
        reject("compose-volumes-invalid")
    volumes: dict[str, str] = {}
    for name in ("tgblobs", "rustfsdata"):
        item = volumes_config.get(name)
        volume_name = item.get("name") if isinstance(item, dict) else None
        if not isinstance(volume_name, str) or not volume_name:
            reject("compose-volumes-invalid")
        volumes[name] = volume_name
    if (
        volumes["tgblobs"] != previous["volumes"]["tgblobs"]
        or (
            previous["volumes"]["rustfsdata"] is not None
            and volumes["rustfsdata"] != previous["volumes"]["rustfsdata"]
        )
        or volumes["tgblobs"] == volumes["rustfsdata"]
    ):
        reject("transition-volume-changed")

    phase_paths = {
        "candidate_compose_sha256": output_dir / "candidate-compose.json",
        "local_compose_sha256": output_dir / "local-compose.json",
        "frozen_inventory_sha256": output_dir / "frozen-inventory.json",
        "dump_sha256": output_dir / "pre-cutover-postgres-dump.stdout",
        "dump_restore_sha256": output_dir / "dump-restore-result.txt",
        "source_frozen_sha256": output_dir / "source-census.tsv",
        "copy_manifest_sha256": output_dir / "copy-manifest.tsv",
        "destination_census_sha256": output_dir / "destination-census.tsv",
    }
    phase_digests = {name: sha256_file(path) for name, path in phase_paths.items()}
    summary = mode.manifest_summary(phase_paths["source_frozen_sha256"])
    proof = {
        "schema": mode.SIMPLE_TRANSITION_PROOF_SCHEMA,
        "outcome": "s3-accepted",
        "backend": {
            "kind": "s3",
            "endpoint": "http://rustfs:9000",
            "bucket": "teagram",
            "prefix": "teagramd/",
        },
        "volumes": volumes,
        "evidence": {
            "source_manifest_sha256": summary[0],
            "destination_manifest_sha256": summary[0],
            "object_count": summary[1],
            "byte_total": summary[2],
            "copy_passes": 1,
        },
        "phase_digests": phase_digests,
        "phase_files": relative_phase_paths(report_root, phase_paths),
    }
    proof_path = output_dir / "transition-proof.json"
    write_synced(proof_path, (json.dumps(proof, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8"))
    return proof_path


def build_simple_recovery_proof(
    output_dir: pathlib.Path,
    report_root: pathlib.Path,
    previous: dict[str, Any],
    phase_paths: dict[str, pathlib.Path],
) -> pathlib.Path:
    mode = import_mode_helper()
    all_paths = {
        "s3_compose_sha256": output_dir / "s3-compose.json",
        "local_compose_sha256": output_dir / "local-compose.json",
        "frozen_inventory_sha256": output_dir / "frozen-inventory.json",
        "dump_sha256": output_dir / "pre-cutover-postgres-dump.stdout",
        "dump_restore_sha256": output_dir / "dump-restore-result.txt",
        **phase_paths,
    }
    phase_digests = {name: sha256_file(path) for name, path in all_paths.items()}
    s3_summary = mode.manifest_summary(all_paths["s3_census_sha256"])
    restored_summary = mode.manifest_summary(all_paths["destination_census_sha256"])
    proof = {
        "schema": mode.SIMPLE_TRANSITION_PROOF_SCHEMA,
        "outcome": "recovered-local",
        "backend": {"kind": "local", "dir": mode.BLOB_TARGET},
        "volumes": {
            "tgblobs": previous["volumes"]["tgblobs"],
            "rustfsdata": previous["volumes"]["rustfsdata"],
        },
        "evidence": {
            "s3_census_manifest_sha256": s3_summary[0],
            "restored_manifest_sha256": restored_summary[0],
            "object_count": restored_summary[1],
            "byte_total": restored_summary[2],
            "restore_passes": 1,
            "retained_cutover_key_count": sum(
                1 for _ in mode.iter_manifest(all_paths["retained_keys_sha256"])
            ),
        },
        "phase_digests": phase_digests,
        "phase_files": relative_phase_paths(report_root, all_paths),
    }
    proof_path = output_dir / "transition-proof.json"
    write_synced(proof_path, (json.dumps(proof, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8"))
    return proof_path


def read_compose_render(
    checkout: pathlib.Path,
    output_dir: pathlib.Path,
    name: str,
    environment: dict[str, str],
) -> tuple[dict[str, Any], pathlib.Path]:
    raw = run_command(
        ["docker", "compose", "config", "--format", "json"],
        output_dir,
        f"{name}-render",
        cwd=checkout,
        env=environment,
        persist_output=False,
    )
    try:
        compose = json.loads(raw, object_pairs_hook=import_mode_helper().strict_pairs)
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError):
        reject("compose-render-invalid")
    if not isinstance(compose, dict):
        reject("compose-render-invalid")
    path = output_dir / f"{name}-compose.json"
    safe_compose = json.loads(json.dumps(compose))
    services = safe_compose.get("services")
    if isinstance(services, dict):
        for service in services.values():
            if isinstance(service, dict):
                service.pop("environment", None)
    write_synced(
        path,
        (json.dumps(safe_compose, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8"),
    )
    return compose, path


def live_compose_services(compose: object, reason: str) -> list[str]:
    services = compose.get("services") if isinstance(compose, dict) else None
    if not isinstance(services, dict):
        reject(reason)
    names = sorted(name for name in services if isinstance(name, str) and name.startswith("teagramd"))
    if not names:
        reject("serving-services-missing")
    return names


def assert_live_teagramd_matches_authority(
    mode: Any,
    project: str,
    services: list[str],
    authority: dict[str, Any],
    mode_source: str,
    output_dir: pathlib.Path,
    checkout: pathlib.Path,
    environment: dict[str, str],
    name: str,
) -> None:
    running = run_command(
        [
            "docker", "ps", "--no-trunc", "--filter",
            f"label=com.docker.compose.project={project}", "--format",
            '{{.ID}}\t{{.Label "com.docker.compose.service"}}',
        ],
        output_dir,
        f"{name}-containers",
        cwd=checkout,
        env=environment,
    )
    try:
        rows = [line.split("\t") for line in running.decode("utf-8").splitlines() if line]
    except UnicodeDecodeError:
        reject("running-inventory-invalid")
    if any(
        len(row) != 2
        or re.fullmatch(r"[0-9a-f]{64}", row[0]) is None
        or not row[1]
        for row in rows
    ):
        reject("running-inventory-invalid")
    serving = [row for row in rows if mode.is_runtime_service(row[1])]
    identifiers = [row[0] for row in serving]
    if (
        not serving
        or len(identifiers) != len(set(identifiers))
        or {row[1] for row in serving} != set(services)
    ):
        reject("running-service-mismatch")
    inspected_rows = [row for row in rows if mode.is_runtime_service(row[1]) or row[1] == "rustfs"]
    inspected_ids = [row[0] for row in inspected_rows]
    if len(inspected_ids) != len(set(inspected_ids)):
        reject("running-inventory-invalid")

    inspected_output = run_command(
        ["docker", "inspect", "--format", "{{json .}}", *inspected_ids],
        output_dir,
        f"{name}-inspect",
        cwd=checkout,
        env=environment,
    )
    try:
        inspected = [
            json.loads(line, object_pairs_hook=mode.strict_pairs)
            for line in inspected_output.decode("utf-8").splitlines()
            if line
        ]
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError, mode.Reject):
        reject("running-inventory-invalid")
    if len(inspected) != len(inspected_ids):
        reject("running-inventory-invalid")
    for row, container in zip(inspected_rows, inspected, strict=True):
        config = container.get("Config") if isinstance(container, dict) else None
        labels = config.get("Labels") if isinstance(config, dict) else None
        state = container.get("State") if isinstance(container, dict) else None
        if (
            not isinstance(labels, dict)
            or labels.get("com.docker.compose.project") != project
            or labels.get("com.docker.compose.service") != row[1]
            or container.get("Id") != row[0]
            or not isinstance(state, dict)
            or state.get("Status") != "running"
        ):
            reject("running-inventory-invalid")
    try:
        inventory = mode.container_inventory(inspected, checkout)
        mode.assert_containers_match(inventory, authority, mode_source)
    except mode.Reject as error:
        reject(str(error))


def run_current_cutover(args: argparse.Namespace) -> None:
    checkout = args.checkout.absolute()
    state_dir = args.state_dir.absolute()
    report_root = args.report_root.absolute()
    lock_path = args.lock_path.absolute()
    secure_directory(report_root)
    if checkout.is_symlink():
        reject("candidate-checkout-invalid")
    try:
        checkout = checkout.resolve(strict=True)
        report_root = report_root.resolve(strict=True)
    except OSError:
        reject("transition-input-unavailable")
    if not checkout.is_dir():
        reject("candidate-checkout-invalid")
    if not TEST_MODE and (
        os.geteuid() != 0
        or checkout != pathlib.Path("/opt/teagram")
        or str(lock_path) != "/tmp/teagram-deploy.lock"
        or pathlib.Path.cwd().resolve(strict=True) != checkout
    ):
        reject("production-path-invalid")

    acquire_lock(lock_path)
    run_id = uuid.uuid4().hex
    output_dir = report_root / f".blob-cutover-{run_id}"
    try:
        output_dir.mkdir(mode=0o700)
    except OSError:
        reject("runner-evidence-directory-create-failed")
    secure_directory(output_dir)
    require_synthetic_fixture_scope(args, output_dir, checkout, report_root)

    mode = import_mode_helper()
    inspector_env = compose_environment(checkout)
    records, head, mode_bytes = mode.read_authority(state_dir, report_root)
    if (
        not records
        or mode_bytes != head
        or records[-1]["outcome"] not in ("initial-local", "recovered-local", mode.STACK_MOVE_OUTCOME)
    ):
        reject("transition-authority-not-local")
    previous = records[-1]
    source_volume = previous["volumes"]["tgblobs"]
    volume_match = re.fullmatch(r"([a-z0-9][a-z0-9_-]*)_tgblobs", source_volume)
    if volume_match is None:
        reject("transition-volume-invalid")
    project = volume_match.group(1)

    local_files = read_live_compose_files(checkout, project, output_dir, inspector_env)
    local_env = compose_environment_for_files(
        checkout,
        [
            "docker-compose.yml",
            "docker-compose.local-blobs.yml",
            "docker-compose.override.yml",
        ],
    )
    candidate_env = compose_environment_for_files(
        checkout,
        ["docker-compose.yml", "docker-compose.override.yml"],
        include_rustfs_credentials=True,
    )
    if local_files != [str(checkout / name) for name in (
        "docker-compose.yml", "docker-compose.local-blobs.yml", "docker-compose.override.yml"
    )]:
        reject("live-compose-selection-invalid")

    local_services: list[str] = []
    rustfs_may_be_running = False
    writers_may_be_stopped = False
    authority_published = False
    try:
        local_compose, local_compose_path = read_compose_render(
            checkout, output_dir, "local", local_env
        )
        mode.assert_compose_matches(
            mode.compose_inventory(local_compose, checkout),
            previous,
            os.path.realpath(checkout / ".state" / "blob-mode"),
            checkout / "docker-compose.override.yml",
        )
        local_services = live_compose_services(local_compose, "local-compose-invalid")
        if local_compose.get("name") != project:
            reject("compose-project-invalid")

        candidate_compose, candidate_compose_path = read_compose_render(
            checkout, output_dir, "candidate", candidate_env
        )
        candidate_services = live_compose_services(candidate_compose, "compose-services-invalid")
        if candidate_services != local_services or candidate_compose.get("name") != project:
            reject("serving-services-changed")
        candidate_volumes = candidate_compose.get("volumes")
        if not isinstance(candidate_volumes, dict):
            reject("compose-volumes-invalid")
        s3_record = dict(previous)
        s3_record["outcome"] = "s3-accepted"
        s3_record["backend"] = {
            "kind": "s3", "endpoint": "http://rustfs:9000", "bucket": "teagram", "prefix": "teagramd/"
        }
        s3_volumes: dict[str, str] = {}
        for name in ("tgblobs", "rustfsdata"):
            item = candidate_volumes.get(name)
            value = item.get("name") if isinstance(item, dict) else None
            if not isinstance(value, str) or not value:
                reject("compose-volumes-invalid")
            s3_volumes[name] = value
        s3_record["volumes"] = s3_volumes
        mode.assert_compose_matches(
            mode.compose_inventory(candidate_compose, checkout),
            s3_record,
            os.path.realpath(checkout / ".state" / "blob-mode"),
            checkout / "docker-compose.override.yml",
        )
        try:
            mode.validate_private_rustfs_render(candidate_compose)
        except mode.Reject:
            reject("rustfs-exposed")
        if (
            s3_volumes["tgblobs"] != source_volume
            or (previous["volumes"]["rustfsdata"] is not None
                and s3_volumes["rustfsdata"] != previous["volumes"]["rustfsdata"])
            or s3_volumes["tgblobs"] == s3_volumes["rustfsdata"]
        ):
            reject("transition-volume-changed")

        local_running = run_command(
            ["docker", "compose", "ps", "--status", "running", "--services"],
            output_dir,
            "verify-local-serving-before-freeze",
            cwd=checkout,
            env=local_env,
        )
        running_services = set(local_running.decode("utf-8").splitlines())
        if "postgres" not in running_services or not set(local_services) <= running_services:
            reject("local-serving-not-ready")
        assert_live_teagramd_matches_authority(
            mode,
            project,
            local_services,
            previous,
            os.path.realpath(checkout / ".state" / "blob-mode"),
            output_dir,
            checkout,
            local_env,
            "verify-local-authority-before-freeze",
        )
        candidate_running = run_command(
            ["docker", "compose", "ps", "--status", "running", "--services"],
            output_dir,
            "verify-project-before-transition",
            cwd=checkout,
            env=candidate_env,
        )
        if "rustfs" in candidate_running.decode("utf-8").splitlines():
            rustfs_may_be_running = True
            run_command(
                ["docker", "compose", "stop", "rustfs"], output_dir, "stop-prior-rustfs",
                cwd=checkout, env=candidate_env,
            )
            rustfs_may_be_running = False

        writers_may_be_stopped = True
        run_command(
            ["docker", "compose", "stop", "--timeout", "120", *local_services],
            output_dir,
            "freeze-local-serving",
            cwd=checkout,
            env=local_env,
        )
        frozen_containers = live_host_inventory(
            project, source_volume, output_dir, checkout, local_env, "frozen-inventory"
        )
        frozen_services = {item["service"] for item in frozen_containers}
        if "postgres" not in frozen_services or frozen_services.intersection(local_services):
            reject("writer-freeze-not-held")
        frozen_path = output_dir / "frozen-inventory.json"
        write_synced(
            frozen_path,
            (json.dumps(
                {
                    "complete": True,
                    "captured_at": utc_timestamp(),
                    "containers": sorted(frozen_containers, key=lambda item: (item["service"], item["id"])),
                },
                sort_keys=True,
                separators=(",", ":"),
            ) + "\n").encode("utf-8"),
        )

        dump_path = capture_pre_cutover_database_dump(output_dir, checkout, local_env)
        restore_path = verify_pre_cutover_database_dump(
            dump_path, output_dir, checkout, local_env
        )
        source_manifest = manifest_command(
            output_dir,
            output_dir,
            "source-census",
            "local-census",
            artifact_dir=output_dir,
            cwd=checkout,
            env=candidate_env,
        )

        rustfs_may_be_running = True
        run_command(
            ["docker", "compose", "up", "-d", "--wait", "rustfs"],
            output_dir,
            "start-rustfs",
            cwd=checkout,
            env=candidate_env,
        )
        run_command(
            ["docker", "compose", "run", "--rm", "--no-deps", "rustfs-init"],
            output_dir,
            "initialize-rustfs",
            cwd=checkout,
            env=candidate_env,
        )
        copy_manifest = manifest_command(
            output_dir,
            output_dir,
            "copy-manifest",
            artifact_dir=output_dir,
            cwd=checkout,
            env=candidate_env,
        )
        destination_manifest = manifest_command(
            output_dir,
            output_dir,
            "destination-census",
            "s3-census",
            artifact_dir=output_dir,
            cwd=checkout,
            env=candidate_env,
        )
        summary = mode.compare_manifest_summaries(
            [source_manifest, copy_manifest, destination_manifest]
        )

        # The writer freeze must still hold at the authority publication boundary.
        live_host_inventory(
            project, source_volume, output_dir, checkout, candidate_env, "writers-before-publication"
        )
        current_records, current_head, current_bytes = mode.read_authority(state_dir, report_root)
        if current_head != head or current_bytes != mode_bytes or len(current_records) != len(records):
            reject("transition-authority-changed")
        proof_path = build_simple_s3_proof(output_dir, report_root, previous)
        publication = invoke_publisher(proof_path, state_dir, report_root, lock_path)
        if not publication.startswith("blob_mode=published outcome=s3-accepted "):
            reject("authority-publication-invalid")
        authority_published = True

        run_command(
            ["docker", "compose", "up", "-d", "--no-deps", *candidate_services],
            output_dir,
            "activate-s3-serving",
            cwd=checkout,
            env=candidate_env,
        )
        running = run_command(
            ["docker", "compose", "ps", "--status", "running", "--services"],
            output_dir,
            "verify-s3-serving",
            cwd=checkout,
            env=candidate_env,
        )
        if not set(candidate_services) <= set(running.decode("utf-8").splitlines()):
            reject("s3-serving-not-ready")
        final_records, final_head, final_bytes = mode.read_authority(state_dir, report_root)
        if final_head != final_bytes or final_records[-1]["outcome"] != "s3-accepted":
            reject("s3-authority-invalid")
        result = {
            "outcome": "s3-accepted",
            "generation": final_records[-1]["generation"],
            "object_count": summary[1],
            "byte_total": summary[2],
        }
        write_synced(output_dir / "result.json", (json.dumps(result, sort_keys=True, separators=(",", ":")) + "\n").encode("ascii"))
        print(
            f"transition=accepted outcome=s3-accepted generation={final_records[-1]['generation']} "
            f"objects={summary[1]} bytes={summary[2]}"
        )
    except Exception as error:
        failure = error if isinstance(error, TransitionReject) else TransitionReject("transition-failed")
        if not authority_published:
            rustfs_stopped = True
            if rustfs_may_be_running:
                try:
                    run_command(
                        ["docker", "compose", "stop", "rustfs"], output_dir,
                        "stop-rustfs-after-rejection", cwd=checkout, env=candidate_env,
                    )
                except TransitionReject:
                    failure = TransitionReject("transition-rejected-rustfs-stop-failed")
                    rustfs_stopped = False
            if writers_may_be_stopped and rustfs_stopped:
                try:
                    run_command(
                        ["docker", "compose", "up", "-d", "--no-deps", *local_services],
                        output_dir,
                        "resume-local-serving-after-rejection",
                        cwd=checkout,
                        env=local_env,
                    )
                    running = run_command(
                        ["docker", "compose", "ps", "--status", "running", "--services"],
                        output_dir,
                        "verify-local-resumed-after-rejection",
                        cwd=checkout,
                        env=local_env,
                    )
                    if not set(local_services) <= set(running.decode("utf-8").splitlines()):
                        reject("transition-rejected-local-resume-failed")
                except TransitionReject:
                    failure = TransitionReject("transition-rejected-local-resume-failed")
        raise failure


def run_simple_recovery(args: argparse.Namespace) -> None:
    checkout = args.checkout.absolute()
    state_dir = args.state_dir.absolute()
    report_root = args.report_root.absolute()
    lock_path = args.lock_path.absolute()
    secure_directory(report_root)
    if checkout.is_symlink():
        reject("candidate-checkout-invalid")
    try:
        checkout = checkout.resolve(strict=True)
        report_root = report_root.resolve(strict=True)
    except OSError:
        reject("transition-input-unavailable")
    if not checkout.is_dir():
        reject("candidate-checkout-invalid")
    if not TEST_MODE and (
        os.geteuid() != 0
        or checkout != pathlib.Path("/opt/teagram")
        or str(lock_path) != "/tmp/teagram-deploy.lock"
        or pathlib.Path.cwd().resolve(strict=True) != checkout
    ):
        reject("production-path-invalid")

    acquire_lock(lock_path)
    output_dir = report_root / f".blob-recovery-{uuid.uuid4().hex}"
    try:
        output_dir.mkdir(mode=0o700)
    except OSError:
        reject("runner-evidence-directory-create-failed")
    secure_directory(output_dir)
    require_synthetic_fixture_scope(args, output_dir, checkout, report_root)

    mode = import_mode_helper()
    inspector_env = compose_environment(checkout)
    records, head, mode_bytes = mode.read_authority(state_dir, report_root)
    if not records or mode_bytes != head or records[-1]["outcome"] != "s3-accepted":
        reject("recovery-authority-not-s3")
    previous = records[-1]
    source_volume = previous["volumes"]["tgblobs"]
    volume_match = re.fullmatch(r"([a-z0-9][a-z0-9_-]*)_tgblobs", source_volume)
    if volume_match is None:
        reject("transition-volume-invalid")
    project = volume_match.group(1)
    s3_files = ["docker-compose.yml", "docker-compose.override.yml"]
    live_files = read_live_compose_files(
        checkout, project, output_dir, inspector_env, expected_files=s3_files
    )
    if live_files != [str(checkout / name) for name in s3_files]:
        reject("live-compose-selection-invalid")
    s3_env = compose_environment_for_files(
        checkout, s3_files, include_rustfs_credentials=True
    )
    local_files = [
        "docker-compose.yml",
        "docker-compose.local-blobs.yml",
        "docker-compose.override.yml",
    ]
    local_env = compose_environment_for_files(checkout, local_files)

    s3_services: list[str] = []
    local_services: list[str] = []
    writers_may_be_stopped = False
    authority_published = False
    try:
        s3_compose, _s3_compose_path = read_compose_render(checkout, output_dir, "s3", s3_env)
        if s3_compose.get("name") != project:
            reject("compose-project-invalid")
        s3_services = live_compose_services(s3_compose, "s3-compose-invalid")
        mode.assert_compose_matches(
            mode.compose_inventory(s3_compose, checkout),
            previous,
            os.path.realpath(checkout / ".state" / "blob-mode"),
            checkout / "docker-compose.override.yml",
        )
        try:
            mode.validate_private_rustfs_render(s3_compose)
        except mode.Reject:
            reject("rustfs-exposed")

        local_compose, _local_compose_path = read_compose_render(
            checkout, output_dir, "local", local_env
        )
        local_services = live_compose_services(local_compose, "local-compose-invalid")
        local_record = dict(previous)
        local_record["outcome"] = "recovered-local"
        local_record["backend"] = {"kind": "local", "dir": mode.BLOB_TARGET}
        mode.assert_compose_matches(
            mode.compose_inventory(local_compose, checkout),
            local_record,
            os.path.realpath(checkout / ".state" / "blob-mode"),
            checkout / "docker-compose.override.yml",
        )
        if local_compose.get("name") != project or local_services != s3_services:
            reject("serving-services-changed")

        before = run_command(
            ["docker", "compose", "ps", "--status", "running", "--services"],
            output_dir,
            "verify-s3-serving-before-recovery",
            cwd=checkout,
            env=s3_env,
        )
        running = set(before.decode("utf-8").splitlines())
        if not ({"postgres", "rustfs"} | set(s3_services)) <= running:
            reject("s3-serving-not-ready")
        assert_live_teagramd_matches_authority(
            mode,
            project,
            s3_services,
            previous,
            os.path.realpath(checkout / ".state" / "blob-mode"),
            output_dir,
            checkout,
            s3_env,
            "verify-s3-authority-before-freeze",
        )

        writers_may_be_stopped = True
        run_command(
            ["docker", "compose", "stop", "--timeout", "120", *s3_services],
            output_dir,
            "freeze-s3-serving",
            cwd=checkout,
            env=s3_env,
        )
        frozen_containers = live_host_inventory(
            project, source_volume, output_dir, checkout, s3_env, "frozen-inventory"
        )
        frozen_services = {item["service"] for item in frozen_containers}
        if "postgres" not in frozen_services or "rustfs" not in frozen_services or frozen_services.intersection(s3_services):
            reject("writer-freeze-not-held")
        write_synced(
            output_dir / "frozen-inventory.json",
            (json.dumps(
                {
                    "complete": True,
                    "captured_at": utc_timestamp(),
                    "containers": sorted(frozen_containers, key=lambda item: (item["service"], item["id"])),
                },
                sort_keys=True,
                separators=(",", ":"),
            ) + "\n").encode("utf-8"),
        )

        dump_path = capture_pre_cutover_database_dump(output_dir, checkout, s3_env)
        verify_pre_cutover_database_dump(dump_path, output_dir, checkout, s3_env)
        phase_paths = {
            "s3_census_sha256": manifest_command(
                output_dir,
                output_dir,
                "s3-census",
                "s3-census",
                service="blob-restore",
                extra_args=("--source", "/destination"),
                artifact_dir=output_dir,
                cwd=checkout,
                env=s3_env,
            ),
            "local_before_restore_sha256": manifest_command(
                output_dir,
                output_dir,
                "local-before-restore",
                "local-census",
                service="blob-restore",
                extra_args=("--source", "/destination"),
                artifact_dir=output_dir,
                cwd=checkout,
                env=s3_env,
            ),
            "restore_manifest_sha256": manifest_command(
                output_dir,
                output_dir,
                "restore-manifest",
                "s3-to-local",
                service="blob-restore",
                extra_args=("--destination", "/destination"),
                artifact_dir=output_dir,
                cwd=checkout,
                env=s3_env,
            ),
            "destination_census_sha256": manifest_command(
                output_dir,
                output_dir,
                "destination-census",
                "local-census",
                service="blob-restore",
                extra_args=("--source", "/destination"),
                artifact_dir=output_dir,
                cwd=checkout,
                env=s3_env,
            ),
        }
        phase_paths["retained_keys_sha256"] = write_retained_manifest(
            mode,
            phase_paths["local_before_restore_sha256"],
            phase_paths["s3_census_sha256"],
            output_dir,
        )
        s3_summary = mode.compare_manifest_summaries(
            [phase_paths["s3_census_sha256"], phase_paths["restore_manifest_sha256"]]
        )
        retained_count = mode.validate_restored_union(
            phase_paths["local_before_restore_sha256"],
            phase_paths["s3_census_sha256"],
            phase_paths["retained_keys_sha256"],
            phase_paths["destination_census_sha256"],
        )
        if s3_summary[1] <= 0:
            reject("recovery-empty-source")

        live_host_inventory(
            project, source_volume, output_dir, checkout, s3_env, "writers-before-recovery-publication"
        )
        current_records, current_head, current_bytes = mode.read_authority(state_dir, report_root)
        if current_head != head or current_bytes != mode_bytes or len(current_records) != len(records):
            reject("transition-authority-changed")
        proof_path = build_simple_recovery_proof(output_dir, report_root, previous, phase_paths)
        publication = invoke_publisher(
            proof_path, state_dir, report_root, lock_path, outcome="recovered-local"
        )
        if not publication.startswith("blob_mode=published outcome=recovered-local "):
            reject("authority-publication-invalid")
        authority_published = True

        run_command(
            ["docker", "compose", "up", "-d", "--no-deps", *local_services],
            output_dir,
            "activate-recovered-local-serving",
            cwd=checkout,
            env=local_env,
        )
        running = run_command(
            ["docker", "compose", "ps", "--status", "running", "--services"],
            output_dir,
            "verify-recovered-local-serving",
            cwd=checkout,
            env=local_env,
        )
        if not set(local_services) <= set(running.decode("utf-8").splitlines()):
            reject("local-serving-not-ready")
        final_records, final_head, final_bytes = mode.read_authority(state_dir, report_root)
        if final_head != final_bytes or final_records[-1]["outcome"] != "recovered-local":
            reject("local-authority-invalid")
        result = {
            "outcome": "recovered-local",
            "generation": final_records[-1]["generation"],
            "object_count": mode.manifest_summary(phase_paths["destination_census_sha256"])[1],
            "retained_cutover_key_count": retained_count,
        }
        write_synced(
            output_dir / "result.json",
            (json.dumps(result, sort_keys=True, separators=(",", ":")) + "\n").encode("ascii"),
        )
        print(
            f"transition=accepted outcome=recovered-local generation={final_records[-1]['generation']} "
            f"objects={result['object_count']} retained={retained_count}"
        )
    except Exception as error:
        failure = error if isinstance(error, TransitionReject) else TransitionReject("recovery-failed")
        if not authority_published and writers_may_be_stopped:
            try:
                run_command(
                    ["docker", "compose", "up", "-d", "--no-deps", *s3_services],
                    output_dir,
                    "resume-s3-serving-after-rejection",
                    cwd=checkout,
                    env=s3_env,
                )
                running = run_command(
                    ["docker", "compose", "ps", "--status", "running", "--services"],
                    output_dir,
                    "verify-s3-resumed-after-rejection",
                    cwd=checkout,
                    env=s3_env,
                )
                if not set(s3_services) <= set(running.decode("utf-8").splitlines()):
                    reject("recovery-rejected-s3-resume-failed")
            except TransitionReject:
                failure = TransitionReject("recovery-rejected-s3-resume-failed")
        raise failure


def invoke_publisher(
    proof_path: pathlib.Path,
    state_dir: pathlib.Path,
    report_root: pathlib.Path,
    lock_path: pathlib.Path,
    outcome: str = "s3-accepted",
) -> str:
    mode = import_mode_helper()
    output = []
    args = SimpleNamespace(
        outcome=outcome,
        proof=proof_path,
        state_dir=state_dir,
        report_root=report_root,
        lock_path=lock_path,
    )
    try:
        with contextlib.redirect_stdout(_ListWriter(output)):
            mode.publish_transition(args)
    except mode.Reject:
        reject("authority-publication-rejected")
    return "".join(output).strip()


class _ListWriter:
    def __init__(self, target: list[str]) -> None:
        self.target = target

    def write(self, value: str) -> int:
        self.target.append(value)
        return len(value)


def run_cutover(args: argparse.Namespace) -> None:
    if not TEST_MODE:
        reject("live-post-67-transition-gates-pending")
    checkout = args.checkout.absolute()
    bundle = args.bundle.absolute()
    report_root = args.report_root.absolute()
    secure_directory(report_root)
    secure_directory(bundle)
    if checkout.is_symlink():
        reject("candidate-checkout-invalid")
    try:
        checkout = checkout.resolve(strict=True)
        bundle = bundle.resolve(strict=True)
        report_root = report_root.resolve(strict=True)
    except OSError:
        reject("transition-input-unavailable")
    try:
        bundle.relative_to(report_root)
    except ValueError:
        reject("bundle-outside-report-root")
    require_synthetic_fixture_scope(args, bundle, checkout, report_root)
    if checkout.is_symlink() or not checkout.is_dir():
        reject("candidate-checkout-invalid")
    compose_env = compose_environment(checkout)
    local_env = compose_environment(checkout, local=True)
    lock_path = args.lock_path.absolute()
    if not TEST_MODE and (str(lock_path) != "/tmp/teagram-deploy.lock" or checkout != pathlib.Path("/opt/teagram")):
        reject("production-path-invalid")
    acquire_lock(lock_path)
    if not TEST_MODE:
        if pathlib.Path.cwd().resolve(strict=True) != checkout:
            reject("runner-checkout-mismatch")

    run_id = str(uuid.uuid4())
    output_dir = bundle.parent / f".transition-{run_id}"
    try:
        output_dir.mkdir(mode=0o700)
    except OSError:
        reject("runner-evidence-directory-create-failed")
    secure_directory(output_dir)

    writers_may_be_stopped = False
    rustfs_may_be_running = False
    authority_published = False
    try:
        working_bundle = output_dir / "bundle"
        copy_bundle(bundle, working_bundle)
        compose = json.loads((working_bundle / "candidate-compose.json").read_text(encoding="utf-8"))
        mode = import_mode_helper()
        records, head, mode_bytes = mode.read_authority(args.state_dir, report_root)
        if not records or mode_bytes != head or records[-1]["outcome"] not in ("initial-local", "recovered-local"):
            reject("transition-authority-not-local")
        local_render = run_command(
            ["docker", "compose", "config", "--format", "json"],
            output_dir,
            "local-compose-render",
            cwd=checkout,
            env=local_env,
        )
        try:
            local_compose = json.loads(local_render, object_pairs_hook=mode.strict_pairs)
        except (UnicodeDecodeError, json.JSONDecodeError, ValueError):
            reject("compose-render-invalid")
        if not isinstance(local_compose, dict) or not isinstance(local_compose.get("services"), dict):
            reject("compose-render-invalid")
        local_inventory = mode.compose_inventory(local_compose, checkout)
        mode.assert_compose_matches(
            local_inventory,
            records[-1],
            os.path.realpath(checkout / ".state" / "blob-mode"),
            checkout / "docker-compose.override.yml",
        )
        local_services = sorted(name for name in local_compose["services"] if name.startswith("teagramd"))
        if not local_services:
            reject("serving-services-missing")
        candidate_services = compose.get("services")
        if not isinstance(candidate_services, dict):
            reject("compose-services-invalid")
        candidate_serving_services = sorted(
            name for name in candidate_services if name.startswith("teagramd")
        )
        if candidate_serving_services != local_services:
            reject("serving-services-changed")
        current_local = run_command(
            ["docker", "compose", "ps", "--status", "running", "--services"],
            output_dir,
            "verify-local-serving-before-freeze",
            cwd=checkout,
            env=local_env,
        )
        try:
            current_local_services = set(current_local.decode("utf-8").splitlines())
        except UnicodeDecodeError:
            reject("running-inventory-invalid")
        if not set(local_services) <= current_local_services:
            run_command(
                ["docker", "compose", "up", "-d", "--no-deps", *local_services],
                output_dir,
                "resume-local-serving-before-transition",
                cwd=checkout,
                env=local_env,
            )
            current_local = run_command(
                ["docker", "compose", "ps", "--status", "running", "--services"],
                output_dir,
                "verify-local-serving-resumed-before-transition",
                cwd=checkout,
                env=local_env,
            )
            try:
                current_local_services = set(current_local.decode("utf-8").splitlines())
            except UnicodeDecodeError:
                reject("running-inventory-invalid")
            if not set(local_services) <= current_local_services:
                reject("local-serving-not-ready")
        compose_volumes = compose.get("volumes")
        if not isinstance(compose_volumes, dict):
            reject("compose-volumes-invalid")
        candidate_volume_names: dict[str, str] = {}
        for name in ("tgblobs", "rustfsdata"):
            item = compose_volumes.get(name)
            volume_name = item.get("name") if isinstance(item, dict) else None
            if not isinstance(volume_name, str) or not volume_name:
                reject("compose-volumes-invalid")
            candidate_volume_names[name] = volume_name
        previous_volumes = records[-1]["volumes"]
        if candidate_volume_names["tgblobs"] != previous_volumes["tgblobs"]:
            reject("transition-volume-changed")
        if previous_volumes["rustfsdata"] is not None and candidate_volume_names["rustfsdata"] != previous_volumes["rustfsdata"]:
            reject("transition-volume-changed")
        project = compose.get("name")
        if not isinstance(project, str) or re.fullmatch(r"[a-z0-9][a-z0-9_-]*", project) is None:
            reject("compose-project-invalid")
        existing_services = run_command(
            ["docker", "compose", "ps", "--status", "running", "--services"],
            output_dir,
            "verify-project-before-transition",
            cwd=checkout,
            env=compose_env,
        )
        try:
            existing_service_names = set(existing_services.decode("utf-8").splitlines())
        except UnicodeDecodeError:
            reject("running-inventory-invalid")
        if "rustfs" in existing_service_names:
            rustfs_may_be_running = True
            run_command(
                ["docker", "compose", "stop", "rustfs"],
                output_dir,
                "stop-prior-uncommitted-rustfs",
                cwd=checkout,
                env=compose_env,
            )
            rustfs_may_be_running = False
        writers_may_be_stopped = True
        freeze_started_at = utc_timestamp()
        run_command(
            ["docker", "compose", "stop", "--timeout", "120", *local_services],
            output_dir,
            "freeze-local-serving",
            cwd=checkout,
            env=local_env,
        )
        running_services = require_writers_stopped(
            project, candidate_volume_names["tgblobs"], output_dir, checkout, local_env, "writers-before-copy"
        )
        frozen = mode.read_json(working_bundle / "frozen-containers.json")
        frozen_services = {item["service"] for item in frozen["containers"]}
        if running_services != frozen_services:
            reject("frozen-inventory-mismatch")
        fresh_phase_paths = capture_fresh_cutover_evidence(
            working_bundle, output_dir, checkout, local_env,
            candidate_volume_names["tgblobs"], freeze_started_at,
        )
        preflight = run_gate("pre-copy", working_bundle, checkout, output_dir)
        print(preflight)
        rustfs_may_be_running = True
        run_command(["docker", "compose", "up", "-d", "--wait", "rustfs"], output_dir, "start-rustfs", cwd=checkout, env=compose_env)
        run_command(["docker", "compose", "run", "--rm", "--no-deps", "rustfs-init"], output_dir, "initialize-rustfs", cwd=checkout, env=compose_env)
        manifest_command(working_bundle, output_dir, "copy-pass-1", cwd=checkout, env=compose_env)
        manifest_command(working_bundle, output_dir, "destination-census-pass-1", "s3-census", cwd=checkout, env=compose_env)
        manifest_command(working_bundle, output_dir, "copy-pass-2", cwd=checkout, env=compose_env)
        fixture_interrupt("copy-pass-2")
        manifest_command(working_bundle, output_dir, "destination-census-pass-2", "s3-census", cwd=checkout, env=compose_env)

        verdict = run_gate("check", working_bundle, checkout, output_dir)
        print(verdict)
        require_writers_stopped(
            project, candidate_volume_names["tgblobs"], output_dir, checkout, compose_env,
            "writers-before-publication",
        )
        proof_path = build_s3_proof(working_bundle, output_dir, report_root, fresh_phase_paths)
        published = invoke_publisher(proof_path, args.state_dir, report_root, lock_path)
        if not published.startswith("blob_mode=published outcome=s3-accepted "):
            reject("authority-publication-invalid")
        authority_published = True
        serving_services = candidate_serving_services
        if not serving_services:
            reject("serving-services-missing")
        run_command(["docker", "compose", "up", "-d", "--no-deps", *serving_services], output_dir, "activate-s3-serving", cwd=checkout, env=compose_env)
        running = run_command(
            ["docker", "compose", "ps", "--status", "running", "--services"],
            output_dir,
            "verify-s3-serving",
            cwd=checkout,
            env=compose_env,
        )
        try:
            running_services = set(running.decode("utf-8").splitlines())
        except UnicodeDecodeError:
            reject("running-inventory-invalid")
        if not set(serving_services) <= running_services:
            reject("s3-serving-not-ready")
        result = {
            "outcome": "s3-accepted",
            "generation": json.loads((args.state_dir / "mode.json").read_text(encoding="utf-8"))["generation"],
            "services": len(serving_services),
            "qualification": verdict,
        }
        write_synced(output_dir / "result.json", (json.dumps(result, sort_keys=True, separators=(",", ":")) + "\n").encode("ascii"))
        print(f"transition=accepted outcome=s3-accepted services={len(serving_services)}")
    except Exception as error:
        failure = error if isinstance(error, TransitionReject) else TransitionReject("transition-failed")
        if not authority_published:
            rustfs_stopped = True
            if rustfs_may_be_running:
                try:
                    run_command(
                        ["docker", "compose", "stop", "rustfs"],
                        output_dir,
                        "stop-rustfs-after-rejection",
                        cwd=checkout,
                        env=compose_env,
                    )
                except TransitionReject:
                    failure = TransitionReject("transition-rejected-rustfs-stop-failed")
                    rustfs_stopped = False
            if writers_may_be_stopped and rustfs_stopped:
                try:
                    run_command(
                        ["docker", "compose", "up", "-d", "--no-deps", *local_services],
                        output_dir,
                        "resume-local-serving-after-rejection",
                        cwd=checkout,
                        env=local_env,
                    )
                    running = run_command(
                        ["docker", "compose", "ps", "--status", "running", "--services"],
                        output_dir,
                        "verify-local-resumed-after-rejection",
                        cwd=checkout,
                        env=local_env,
                    )
                    if not set(local_services) <= set(running.decode("utf-8").splitlines()):
                        reject("transition-rejected-local-resume-failed")
                except TransitionReject:
                    failure = TransitionReject("transition-rejected-local-resume-failed")
        raise failure


def run_recovery(args: argparse.Namespace) -> None:
    if not TEST_MODE:
        reject("live-post-67-transition-gates-pending")
    checkout = args.checkout.absolute()
    bundle = args.bundle.absolute()
    report_root = args.report_root.absolute()
    secure_directory(report_root)
    secure_directory(bundle)
    if checkout.is_symlink():
        reject("candidate-checkout-invalid")
    try:
        checkout = checkout.resolve(strict=True)
        bundle = bundle.resolve(strict=True)
        report_root = report_root.resolve(strict=True)
    except OSError:
        reject("transition-input-unavailable")
    try:
        bundle.relative_to(report_root)
    except ValueError:
        reject("bundle-outside-report-root")
    require_synthetic_fixture_scope(args, bundle, checkout, report_root)
    if not checkout.is_dir():
        reject("candidate-checkout-invalid")
    lock_path = args.lock_path.absolute()
    acquire_lock(lock_path)

    run_id = str(uuid.uuid4())
    output_dir = bundle.parent / f".recovery-{run_id}"
    try:
        output_dir.mkdir(mode=0o700)
    except OSError:
        reject("runner-evidence-directory-create-failed")
    secure_directory(output_dir)

    mode = import_mode_helper()
    s3_environment = compose_environment(checkout)
    local_environment = compose_environment(checkout, local=True)
    writers_may_be_stopped = False
    authority_published = False
    try:
        working_bundle = output_dir / "bundle"
        copy_bundle(bundle, working_bundle)
        records, head, mode_bytes = mode.read_authority(args.state_dir, report_root)
        if not records or mode_bytes != head or records[-1]["outcome"] != "s3-accepted":
            reject("recovery-authority-not-s3")
        previous = records[-1]
        write_synced(
            output_dir / "authority-before.json",
            (json.dumps(previous, sort_keys=True, separators=(",", ":")) + "\n").encode("ascii"),
        )

        source_volume = previous["volumes"]["tgblobs"]

        s3_render_bytes = run_command(
            ["docker", "compose", "config", "--format", "json"],
            output_dir,
            "s3-compose-render",
            cwd=checkout,
            env=s3_environment,
        )
        try:
            s3_compose = json.loads(s3_render_bytes, object_pairs_hook=mode.strict_pairs)
        except (UnicodeDecodeError, json.JSONDecodeError, ValueError):
            reject("compose-render-invalid")
        if not isinstance(s3_compose, dict):
            reject("compose-render-invalid")
        write_synced(output_dir / "s3-compose.json", s3_render_bytes)
        s3_inventory = mode.compose_inventory(s3_compose, checkout)
        mode.assert_compose_matches(
            s3_inventory, previous, os.path.realpath(checkout / ".state" / "blob-mode"), checkout / "docker-compose.override.yml"
        )
        project = s3_compose.get("name")
        if not isinstance(project, str) or re.fullmatch(r"[a-z0-9][a-z0-9_-]*", project) is None:
            reject("compose-project-invalid")
        services = s3_compose.get("services")
        if not isinstance(services, dict):
            reject("compose-services-invalid")
        serving_services = sorted(name for name in services if name.startswith("teagramd"))
        if not serving_services:
            reject("serving-services-missing")
        running_before_freeze = run_command(
            ["docker", "compose", "ps", "--status", "running", "--services"],
            output_dir,
            "verify-s3-serving-before-recovery",
            cwd=checkout,
            env=s3_environment,
        )
        if not (set(serving_services) | {"rustfs"}) <= set(running_before_freeze.decode("utf-8").splitlines()):
            reject("s3-serving-not-ready")

        local_render_bytes = run_command(
            ["docker", "compose", "config", "--format", "json"],
            output_dir,
            "local-compose-render",
            cwd=checkout,
            env=local_environment,
        )
        try:
            local_compose = json.loads(local_render_bytes, object_pairs_hook=mode.strict_pairs)
        except (UnicodeDecodeError, json.JSONDecodeError, ValueError):
            reject("compose-render-invalid")
        if not isinstance(local_compose, dict) or not isinstance(local_compose.get("services"), dict):
            reject("compose-render-invalid")
        write_synced(output_dir / "local-compose.json", local_render_bytes)
        local_inventory = mode.compose_inventory(local_compose, checkout)
        local_record = dict(previous)
        local_record["outcome"] = "recovered-local"
        local_record["backend"] = {"kind": "local", "dir": mode.BLOB_TARGET}
        mode.assert_compose_matches(
            local_inventory, local_record, os.path.realpath(checkout / ".state" / "blob-mode"), checkout / "docker-compose.override.yml"
        )
        local_services = sorted(
            name for name in local_compose["services"] if name.startswith("teagramd")
        )
        if local_services != serving_services:
            reject("serving-services-changed")

        writers_may_be_stopped = True
        freeze_started_at = utc_timestamp()
        run_command(
            ["docker", "compose", "stop", "--timeout", "120", *serving_services],
            output_dir,
            "freeze-s3-serving",
            cwd=checkout,
            env=s3_environment,
        )
        running_services = require_writers_stopped(
            project, source_volume, output_dir, checkout, s3_environment, "writers-after-freeze"
        )
        frozen = mode.read_json(working_bundle / "frozen-containers.json")
        frozen_services = {item["service"] for item in frozen["containers"]}
        if running_services != frozen_services:
            reject("frozen-inventory-mismatch")
        if "rustfs" not in running_services:
            reject("s3-service-not-running")
        recovery = mode.read_json(working_bundle / "recovery.json")
        if not isinstance(recovery, dict):
            reject("recovery-qualification-invalid")
        fresh_recovery_phase_paths = capture_fresh_recovery_evidence(
            working_bundle, output_dir, checkout, s3_environment, recovery,
            project, source_volume, freeze_started_at,
        )
        release_set = validate_recovery_bundle(working_bundle, checkout, source_volume)

        phase_paths: dict[str, pathlib.Path] = {}
        phase_paths["s3_compose_sha256"] = output_dir / "s3-compose.json"
        phase_paths["local_compose_sha256"] = output_dir / "local-compose.json"
        phase_paths["s3_census_pass_1_sha256"] = manifest_command(
            bundle, output_dir, "s3-census-pass-1", "s3-census", cwd=checkout, env=s3_environment, artifact_dir=output_dir
        )
        phase_paths["s3_census_pass_2_sha256"] = manifest_command(
            bundle, output_dir, "s3-census-pass-2", "s3-census", cwd=checkout, env=s3_environment, artifact_dir=output_dir
        )
        phase_paths["local_before_restore_sha256"] = manifest_command(
            bundle,
            output_dir,
            "local-before-restore",
            "local-census",
            service="blob-restore",
            extra_args=("--source", "/destination"),
            artifact_dir=output_dir,
            cwd=checkout,
            env=s3_environment,
        )
        phase_paths["restore_pass_1_sha256"] = manifest_command(
            bundle,
            output_dir,
            "restore-pass-1",
            "s3-to-local",
            service="blob-restore",
            extra_args=("--destination", "/destination"),
            artifact_dir=output_dir,
            cwd=checkout,
            env=s3_environment,
        )
        phase_paths["restore_pass_2_sha256"] = manifest_command(
            bundle,
            output_dir,
            "restore-pass-2",
            "s3-to-local",
            service="blob-restore",
            extra_args=("--destination", "/destination"),
            artifact_dir=output_dir,
            cwd=checkout,
            env=s3_environment,
        )
        fixture_interrupt("restore-pass-2")
        phase_paths["local_census_pass_1_sha256"] = manifest_command(
            bundle,
            output_dir,
            "local-census-pass-1",
            "local-census",
            service="blob-restore",
            extra_args=("--source", "/destination"),
            artifact_dir=output_dir,
            cwd=checkout,
            env=s3_environment,
        )
        phase_paths["local_census_pass_2_sha256"] = manifest_command(
            bundle,
            output_dir,
            "local-census-pass-2",
            "local-census",
            service="blob-restore",
            extra_args=("--source", "/destination"),
            artifact_dir=output_dir,
            cwd=checkout,
            env=s3_environment,
        )
        phase_paths["retained_keys_sha256"] = write_retained_manifest(
            mode, phase_paths["local_before_restore_sha256"], phase_paths["s3_census_pass_1_sha256"], output_dir
        )
        phase_paths.update(fresh_recovery_phase_paths)

        s3_summary = mode.manifest_summary(phase_paths["s3_census_pass_1_sha256"])
        retained_count = sum(1 for _ in mode.iter_manifest(phase_paths["retained_keys_sha256"]))
        qualification_output = (
            f"recovery_qualification=pass s3_objects={s3_summary[1]} s3_bytes={s3_summary[2]} "
            f"retained_cutover_key_count={retained_count} migrations={release_set}\n"
        ).encode("ascii")
        write_synced(output_dir / "qualification-recovery.stdout", qualification_output)
        print(qualification_output.decode("ascii").strip())

        require_writers_stopped(
            project, source_volume, output_dir, checkout, local_environment,
            "writers-before-recovery-publication",
        )
        proof_path = build_recovery_proof(working_bundle, output_dir, report_root, phase_paths)
        published = invoke_publisher(
            proof_path, args.state_dir, report_root, lock_path, outcome="recovered-local"
        )
        if not published.startswith("blob_mode=published outcome=recovered-local "):
            reject("authority-publication-invalid")
        authority_published = True

        run_command(
            ["docker", "compose", "up", "-d", "--no-deps", *local_services],
            output_dir,
            "activate-recovered-local-serving",
            cwd=checkout,
            env=local_environment,
        )
        running = run_command(
            ["docker", "compose", "ps", "--status", "running", "--services"],
            output_dir,
            "verify-recovered-local-serving",
            cwd=checkout,
            env=local_environment,
        )
        try:
            running_services = set(running.decode("utf-8").splitlines())
        except UnicodeDecodeError:
            reject("running-inventory-invalid")
        if not set(local_services) <= running_services:
            reject("local-serving-not-ready")
        records, _head, _mode_bytes = mode.read_authority(args.state_dir, report_root)
        print(
            "transition=accepted outcome=recovered-local "
            f"generation={records[-1]['generation']} services={len(local_services)} "
            f"retained_cutover_key_count={records[-1]['evidence']['retained_cutover_key_count']}"
        )
    except Exception as error:
        failure = error if isinstance(error, TransitionReject) else TransitionReject("transition-failed")
        if writers_may_be_stopped and not authority_published:
            try:
                run_command(
                    ["docker", "compose", "up", "-d", "--no-deps", *serving_services],
                    output_dir,
                    "resume-s3-serving-after-rejection",
                    cwd=checkout,
                    env=s3_environment,
                )
                running = run_command(
                    ["docker", "compose", "ps", "--status", "running", "--services"],
                    output_dir,
                    "verify-s3-resumed-after-rejection",
                    cwd=checkout,
                    env=s3_environment,
                )
                if not set(serving_services) <= set(running.decode("utf-8").splitlines()):
                    reject("transition-rejected-s3-resume-failed")
            except TransitionReject:
                failure = TransitionReject("transition-rejected-s3-resume-failed")
        raise failure


def main() -> int:
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="action", required=True)
    accept = commands.add_parser("accept-s3")
    accept.add_argument("--checkout", type=pathlib.Path, required=True)
    accept.add_argument("--state-dir", type=pathlib.Path, required=True)
    accept.add_argument("--report-root", type=pathlib.Path, required=True)
    accept.add_argument("--lock-path", type=pathlib.Path, default=pathlib.Path("/tmp/teagram-deploy.lock"))
    qualified = commands.add_parser("accept-s3-qualified")
    qualified.add_argument("--bundle", type=pathlib.Path, required=True)
    qualified.add_argument("--checkout", type=pathlib.Path, required=True)
    qualified.add_argument("--state-dir", type=pathlib.Path, required=True)
    qualified.add_argument("--report-root", type=pathlib.Path, required=True)
    qualified.add_argument("--lock-path", type=pathlib.Path, default=pathlib.Path("/tmp/teagram-deploy.lock"))
    recover = commands.add_parser("recover-local")
    recover.add_argument("--checkout", type=pathlib.Path, required=True)
    recover.add_argument("--state-dir", type=pathlib.Path, required=True)
    recover.add_argument("--report-root", type=pathlib.Path, required=True)
    recover.add_argument("--lock-path", type=pathlib.Path, default=pathlib.Path("/tmp/teagram-deploy.lock"))
    qualified_recover = commands.add_parser("recover-local-qualified")
    qualified_recover.add_argument("--bundle", type=pathlib.Path, required=True)
    qualified_recover.add_argument("--checkout", type=pathlib.Path, required=True)
    qualified_recover.add_argument("--state-dir", type=pathlib.Path, required=True)
    qualified_recover.add_argument("--report-root", type=pathlib.Path, required=True)
    qualified_recover.add_argument("--lock-path", type=pathlib.Path, default=pathlib.Path("/tmp/teagram-deploy.lock"))
    args = parser.parse_args()
    try:
        if args.action == "accept-s3":
            run_current_cutover(args)
        elif args.action == "accept-s3-qualified":
            run_cutover(args)
        elif args.action == "recover-local":
            run_simple_recovery(args)
        else:
            run_recovery(args)
    except TransitionReject as error:
        print(f"blob transition rejected: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    os.umask(0o077)
    raise SystemExit(main())
