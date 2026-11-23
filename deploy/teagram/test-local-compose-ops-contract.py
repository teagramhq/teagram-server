#!/usr/bin/env python3
"""Keep live operator container identities aligned with the local Compose profile."""

from __future__ import annotations

import json
import os
import re
import subprocess
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]


def operator_constant(path: Path, name: str) -> str:
    if path.suffix == ".py":
        pattern = re.compile(rf"^{re.escape(name)}\s*=\s*['\"]([^'\"]+)['\"]$", re.MULTILINE)
    else:
        pattern = re.compile(rf"^readonly {re.escape(name)}=([^\s]+)$", re.MULTILINE)
    match = pattern.search(path.read_text(encoding="utf-8"))
    if match is None:
        raise AssertionError(f"{path.relative_to(ROOT)} is missing operator constant {name}")
    return match.group(1)


def assert_service_identity(
    config: dict[str, object],
    service_name: str,
    container_names: list[tuple[str, str]],
    service_names: list[tuple[str, str]],
) -> None:
    services = config.get("services")
    if not isinstance(services, dict) or service_name not in services:
        raise AssertionError(f"local Compose config is missing service {service_name}")
    service = services[service_name]
    if not isinstance(service, dict):
        raise AssertionError(f"local Compose service {service_name} is malformed")

    project = config.get("name")
    if not isinstance(project, str) or not project:
        raise AssertionError("local Compose config has no project name")
    deploy = service.get("deploy")
    replicas = deploy.get("replicas", 1) if isinstance(deploy, dict) else 1
    if replicas != 1:
        raise AssertionError(f"operator identity assumes one {service_name} replica, found {replicas}")
    expected_container = service.get("container_name") or f"{project}-{service_name}-1"

    for relative_path, constant in container_names:
        actual = operator_constant(ROOT / relative_path, constant)
        if actual != expected_container:
            raise AssertionError(
                f"{relative_path} {constant}={actual!r}, but Compose renders {expected_container!r}"
            )
    for relative_path, constant in service_names:
        actual = operator_constant(ROOT / relative_path, constant)
        if actual != service_name:
            raise AssertionError(
                f"{relative_path} {constant}={actual!r}, but Compose defines {service_name!r}"
            )


def render_local_config(project_name: str) -> dict[str, object]:
    env = os.environ.copy()
    env["POSTGRES_PASSWORD"] = "fixture-only"
    env["TG_PUBLIC_LINK_PREFIX"] = "https://web.teagram.io"
    rendered = subprocess.run(
        [
            "docker",
            "compose",
            "--project-directory",
            str(ROOT),
            "--project-name",
            project_name,
            "--file",
            str(ROOT / "docker-compose.yml"),
            "--profile",
            "local",
            "config",
            "--format",
            "json",
        ],
        cwd=ROOT,
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )
    if rendered.returncode != 0:
        raise SystemExit(f"could not render local Compose config: {rendered.stderr.strip()}")
    return json.loads(rendered.stdout)


def main() -> None:
    config = render_local_config("teagram")
    custom_config = render_local_config("custom-local")
    shell_ops = [
        "deploy/teagram/ops/start-server.sh",
        "deploy/teagram/ops/blob-encryption-common.inc",
        "deploy/teagram/ops/qa-password-timestamps.sh",
    ]
    assert_service_identity(
        config,
        "local-postgres",
        [(path, "POSTGRES_CONTAINER") for path in shell_ops]
        + [("deploy/teagram/ops/quarantine_unaccounted_media.py", "POSTGRES_CONTAINER")],
        [
            ("deploy/teagram/ops/start-server.sh", "EXPECTED_POSTGRES_SERVICE"),
            ("deploy/teagram/ops/blob-encryption-common.inc", "POSTGRES_SERVICE"),
            ("deploy/teagram/ops/qa-password-timestamps.sh", "POSTGRES_SERVICE"),
        ],
    )
    assert_service_identity(
        config,
        "local-teagramd",
        [(path, "SERVER_CONTAINER") for path in shell_ops],
        [
            ("deploy/teagram/ops/start-server.sh", "EXPECTED_SERVER_SERVICE"),
            ("deploy/teagram/ops/blob-encryption-common.inc", "SERVER_SERVICE"),
            ("deploy/teagram/ops/qa-password-timestamps.sh", "SERVER_SERVICE"),
        ],
    )
    default_blobs = config.get("volumes", {}).get("local_tgblobs", {}).get("name")
    custom_blobs = custom_config.get("volumes", {}).get("local_tgblobs", {}).get("name")
    if default_blobs != "teagram_tgblobs":
        raise AssertionError(f"default local blob volume changed from teagram_tgblobs: {default_blobs!r}")
    if custom_blobs != "custom-local_tgblobs":
        raise AssertionError(f"custom local project shares the wrong blob volume: {custom_blobs!r}")
    print("local Compose service and operator identities match")


if __name__ == "__main__":
    main()
