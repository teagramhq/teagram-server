#!/usr/bin/env python3
"""Validate the live deployment's blob authority and Compose environment."""

from __future__ import annotations

import argparse
import importlib.util
import json
import os
import pathlib
import re
import stat
import sys
from typing import Any

SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
BLOB_MODE_DIR = SCRIPT_DIR.parent.parent / "teagram-demo" / "blob-mode"
HELPER_DIR = SCRIPT_DIR if (SCRIPT_DIR / "blob-mode-state.py").is_file() else BLOB_MODE_DIR
MODE_HELPER = HELPER_DIR / "blob-mode-state.py"
TEST_MODE = os.environ.get("BLOB_TRANSITION_TEST_MODE") == "1"


class TransitionReject(Exception):
    pass


def reject(reason: str) -> None:
    raise TransitionReject(reason)


def import_mode_helper() -> Any:
    spec = importlib.util.spec_from_file_location("blob_mode_state", MODE_HELPER)
    if spec is None or spec.loader is None:
        reject("blob-mode-helper-unavailable")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


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
