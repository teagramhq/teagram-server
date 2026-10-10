#!/usr/bin/env python3
"""Run one Atlas schema-gate phase and report only its fixed phase and status."""
from __future__ import annotations

import os
from pathlib import Path
import re
import subprocess
import sys
from typing import Sequence


LANE = "PostgreSQL Atlas schema gate"
PHASES = frozenset(
    {"service-start", "migrate-apply", "pre-check", "post-check"}
)
CHECKOUT_SHA_RE = re.compile(r"[0-9a-f]{40}")
GATE_SCRIPT = "deploy/telegramd/rollout-runner/schema-result-gate.sh"


def shell_status(returncode: int) -> int:
    if returncode < 0:
        return (128 + abs(returncode)) & 0xFF
    return returncode & 0xFF


def run_command(
    args: list[str],
    *,
    cwd: Path,
    env: dict[str, str],
) -> int:
    try:
        # Runtime output is untrusted; the fixed annotation is the only report.
        result = subprocess.run(
            args,
            cwd=cwd,
            env=env,
            check=False,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
    except FileNotFoundError:
        return 127
    except OSError:
        return 126
    return shell_status(result.returncode)


def checked_out_sha(cwd: Path, env: dict[str, str]) -> str:
    try:
        result = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            cwd=cwd,
            env=env,
            check=False,
            capture_output=True,
            text=True,
            timeout=5,
        )
    except (OSError, subprocess.SubprocessError):
        return "unavailable"

    if result.returncode != 0:
        return "unavailable"

    value = result.stdout[:-1] if result.stdout.endswith("\n") else result.stdout
    return value if CHECKOUT_SHA_RE.fullmatch(value) else "unavailable"


def valid_status(status: object) -> bool:
    return type(status) is int and 1 <= status <= 255


def format_annotation(
    phase_evidence: Sequence[str] | None,
    status: object,
    commit: object,
) -> str | None:
    if not valid_status(status):
        return None

    phase = "unavailable"
    if (
        isinstance(phase_evidence, (list, tuple))
        and len(phase_evidence) == 1
        and isinstance(phase_evidence[0], str)
        and phase_evidence[0] in PHASES
    ):
        phase = phase_evidence[0]

    checked_out_commit = (
        commit
        if isinstance(commit, str) and CHECKOUT_SHA_RE.fullmatch(commit)
        else "unavailable"
    )
    return (
        f"::error::{LANE} failed (category: phase-failure; phase: {phase}; "
        f"exit: {status}; checked-out commit: {checked_out_commit}; details redacted)"
    )


def fallback_annotation(status: object) -> str | None:
    if not valid_status(status):
        return None
    return (
        f"::error::{LANE} failed (category: phase-failure; phase: unavailable; "
        f"exit: {status}; checked-out commit: unavailable; details redacted)"
    )


def emit_failure(
    phase_evidence: Sequence[str] | None,
    status: int,
    commit: str,
) -> None:
    try:
        message = format_annotation(phase_evidence, status, commit)
    except Exception:
        message = fallback_annotation(status)

    if message is None:
        return

    try:
        sys.stdout.write(message + "\n")
        sys.stdout.flush()
    except Exception:
        return


def emit_fallback(status: int) -> None:
    message = fallback_annotation(status)
    if message is None:
        return
    try:
        sys.stdout.write(message + "\n")
        sys.stdout.flush()
    except Exception:
        return


def run_phase(phase: str, env: dict[str, str] | None = None) -> int:
    if phase not in PHASES:
        return 2

    environment = dict(os.environ if env is None else env)
    workspace = Path(environment.get("GITHUB_WORKSPACE", os.getcwd()))
    status = 0
    try:
        if phase == "service-start":
            status = run_command(
                ["docker", "compose", "up", "-d", "--wait", "postgres"],
                cwd=workspace,
                env=environment,
            )
        elif phase == "migrate-apply":
            password = environment["POSTGRES_PASSWORD"]
            status = run_command(
                [
                    "docker",
                    "compose",
                    "run",
                    "--rm",
                    "--no-deps",
                    "migrate",
                    "migrate",
                    "apply",
                    "--dir",
                    "file:///migrations",
                    "--url",
                    f"postgres://postgres:{password}@postgres:5432/telegram?sslmode=disable",
                ],
                cwd=workspace,
                env=environment,
            )
        elif phase in {"pre-check", "post-check"}:
            evidence_dir = Path(environment["RUNNER_TEMP"]) / "schema-gate-evidence"
            if phase == "pre-check":
                status = run_command(
                    ["mkdir", "-m", "700", str(evidence_dir)],
                    cwd=workspace,
                    env=environment,
                )
            if status == 0:
                mode = "pre" if phase == "pre-check" else "post"
                status = run_command(
                    [
                        "bash",
                        GATE_SCRIPT,
                        "check",
                        mode,
                        str(evidence_dir),
                        str(workspace),
                    ],
                    cwd=workspace,
                    env=environment,
                )
    except (KeyError, TypeError, ValueError):
        status = 1

    if status != 0:
        try:
            commit = checked_out_sha(workspace, environment)
        except Exception:
            commit = "unavailable"
        try:
            emit_failure([phase], status, commit)
        except Exception:
            emit_fallback(status)
    return status


def main(argv: list[str] | None = None) -> int:
    arguments = sys.argv[1:] if argv is None else argv
    if len(arguments) != 1:
        return 2
    return run_phase(arguments[0])


if __name__ == "__main__":
    raise SystemExit(main())
