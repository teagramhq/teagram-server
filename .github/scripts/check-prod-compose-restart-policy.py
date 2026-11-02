#!/usr/bin/env python3
"""Require restart policies for every long-running production Compose service."""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path
from typing import Any


# These commands intentionally run once and exit. Adding another one-shot
# service requires explicitly classifying it here before the contract passes.
ONE_SHOT_SERVICES = frozenset(
    {
        "migrate",
        "rustfs-init",
        "blob-migrate",
        "blob-restore",
        "blob-encrypt-migrate",
        "blob-key-rotate",
    }
)
REQUIRED_SERVICES = frozenset({"postgres", "rustfs", "teagramd"})
ON_FAILURE_RETRIES = re.compile(r"on-failure:[1-9][0-9]*\Z")


def valid_restart_policy(value: Any) -> bool:
    return isinstance(value, str) and (
        value in {"always", "unless-stopped", "on-failure"}
        or ON_FAILURE_RETRIES.fullmatch(value) is not None
    )


def validate_compose(compose: Any) -> list[str]:
    if not isinstance(compose, dict) or not isinstance(compose.get("services"), dict):
        return ["resolved Compose config must contain a services object"]

    services = compose["services"]
    errors: list[str] = []
    missing_required = sorted(REQUIRED_SERVICES - services.keys())
    if missing_required:
        errors.append(f"missing required production services: {', '.join(missing_required)}")

    missing_one_shots = sorted(ONE_SHOT_SERVICES - services.keys())
    if missing_one_shots:
        errors.append(f"missing one-shot service classifications: {', '.join(missing_one_shots)}")

    for name, service in sorted(services.items()):
        if not isinstance(service, dict):
            errors.append(f"service {name!r} must resolve to an object")
            continue

        policy = service.get("restart", "no")
        if name in ONE_SHOT_SERVICES:
            if policy is not None and policy != "no":
                errors.append(f"one-shot service {name!r} must not restart (got {policy!r})")
        elif not valid_restart_policy(policy):
            errors.append(f"long-running service {name!r} needs a restart policy (got {policy!r})")

    return errors


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print(f"usage: {Path(argv[0]).name} RESOLVED_COMPOSE_JSON", file=sys.stderr)
        return 2

    path = Path(argv[1])
    try:
        with path.open(encoding="utf-8") as compose_file:
            compose = json.load(compose_file)
    except (OSError, json.JSONDecodeError) as error:
        print(f"cannot read resolved Compose config {path}: {error}", file=sys.stderr)
        return 2

    errors = validate_compose(compose)
    if errors:
        for error in errors:
            print(error, file=sys.stderr)
        return 1

    long_running_count = sum(name not in ONE_SHOT_SERVICES for name in compose["services"])
    print(
        f"PASS restart policies cover {long_running_count} long-running services; "
        f"{len(ONE_SHOT_SERVICES)} one-shot services remain excluded"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
