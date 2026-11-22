#!/usr/bin/env python3
"""Validate the live deployment's blob authority and Compose environment."""

from __future__ import annotations

import argparse
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
from typing import Any

SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
BLOB_MODE_DIR = SCRIPT_DIR.parent.parent / "teagram-demo" / "blob-mode"
HELPER_DIR = SCRIPT_DIR if (SCRIPT_DIR / "blob-mode-state.py").is_file() else BLOB_MODE_DIR
MODE_HELPER = HELPER_DIR / "blob-mode-state.py"
TEST_MODE = os.environ.get("BLOB_TRANSITION_TEST_MODE") == "1"
TEST_FIXTURE_DOCKER: pathlib.Path | None = None
BLOB_TRANSITION_COMPOSE_ENV_FILES = "BLOB_TRANSITION_COMPOSE_ENV_FILES"


class TransitionReject(Exception):
    pass


def reject(reason: str) -> None:
    raise TransitionReject(reason)


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


def utc_timestamp() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


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
    query_path = HELPER_DIR / "rustfs-r70-inert-surfaces.sql"
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


def import_mode_helper() -> Any:
    spec = importlib.util.spec_from_file_location("blob_mode_state", MODE_HELPER)
    if spec is None or spec.loader is None:
        reject("blob-mode-helper-unavailable")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def import_qualifier() -> Any:
    spec = importlib.util.spec_from_file_location("qualify_rustfs_transition", HELPER_DIR / "qualify-rustfs-transition.py")
    if spec is None or spec.loader is None:
        reject("qualification-helper-unavailable")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def compose_environment(checkout: pathlib.Path) -> dict[str, str]:
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
            "MOCK_AUTHORITY_INSPECT", "MOCK_S3_AUTHORITY_INSPECT",
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


def assert_compose_matches_authority(
    mode: Any,
    inventory: dict[str, Any],
    authority: dict[str, Any],
    mode_source: str,
    override_path: pathlib.Path,
) -> None:
    try:
        mode.assert_compose_matches(inventory, authority, mode_source, override_path)
    except mode.Reject as error:
        reject(str(error))


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
    if authority.get("outcome") in ("s3-accepted", "encrypted"):
        assert_live_rustfs_network(mode, authority, output_dir, checkout, environment, name)


def assert_live_rustfs_network(
    mode: Any,
    authority: dict[str, Any],
    output_dir: pathlib.Path,
    checkout: pathlib.Path,
    environment: dict[str, str],
    name: str,
) -> None:
    expected_name = mode.rustfs_private_network_name(authority)
    raw = run_command(
        ["docker", "network", "inspect", "--format", "{{json .}}", expected_name],
        output_dir,
        f"{name}-rustfs-network",
        cwd=checkout,
        env=environment,
    )
    try:
        network = json.loads(raw, object_pairs_hook=mode.strict_pairs)
        if isinstance(network, list):
            if len(network) != 1:
                reject("running-rustfs-network")
            network = network[0]
        mode.validate_private_rustfs_network(network, expected_name)
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError, mode.Reject) as error:
        if isinstance(error, mode.Reject):
            reject(str(error))
        reject("running-rustfs-network")


class _ListWriter:
    def __init__(self, target: list[str]) -> None:
        self.target = target

    def write(self, value: str) -> int:
        self.target.append(value)
        return len(value)


def deployment_environment(args: argparse.Namespace) -> None:
    checkout = args.checkout.absolute()
    state_dir = args.state_dir.absolute()
    report_root = args.report_root.absolute()
    lock_path = args.lock_path.absolute()
    if checkout.is_symlink() or report_root.is_symlink():
        reject("transition-input-invalid")
    try:
        checkout = checkout.resolve(strict=True)
        report_root = report_root.resolve(strict=False)
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

    mode = import_mode_helper()
    try:
        mode.require_runner_lock(lock_path)
        record, missing_report_count = mode.current_deployment_authority(state_dir, report_root)
    except mode.Reject as error:
        reject(str(error))
    backend = record.get("backend")
    if (
        record.get("outcome") not in ("s3-accepted", "encrypted")
        or not isinstance(backend, dict)
        or backend.get("kind") != "s3"
    ):
        reject("deploy-s3-authority-invalid")
    environment_files = rustfs_compose_environment_files(checkout)
    if environment_files[0] != str(checkout / ".env"):
        reject("rustfs-credentials-invalid")
    if missing_report_count:
        print(
            f"blob transition deployment accepted {missing_report_count} journal records without reports",
            file=sys.stderr,
        )
    print(json.dumps(
        {"outcome": "s3-accepted", "env_files": environment_files},
        sort_keys=True,
        separators=(",", ":"),
    ))


def main() -> int:
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="action", required=True)
    deployment_env = commands.add_parser("deployment-env")
    deployment_env.add_argument("--checkout", type=pathlib.Path, required=True)
    deployment_env.add_argument("--state-dir", type=pathlib.Path, required=True)
    deployment_env.add_argument("--report-root", type=pathlib.Path, required=True)
    deployment_env.add_argument(
        "--lock-path", type=pathlib.Path, default=pathlib.Path("/tmp/teagram-deploy.lock")
    )
    args = parser.parse_args()
    try:
        deployment_environment(args)
    except TransitionReject as error:
        print(f"blob transition rejected: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    os.umask(0o077)
    raise SystemExit(main())
