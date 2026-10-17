#!/usr/bin/env python3
"""Regression coverage for timed-out stack-move commands."""

from __future__ import annotations

import importlib.util
import pathlib
import subprocess
import sys
import tempfile


def main() -> None:
    helper_path = pathlib.Path(__file__).with_name("stack-move-runner.py")
    spec = importlib.util.spec_from_file_location("stack_move_runner", helper_path)
    if spec is None or spec.loader is None:
        raise AssertionError("could not load stack-move runner")
    runner = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(runner)

    operation = "restart old Compose project during rollback"
    with tempfile.TemporaryDirectory(prefix="stack-move-timeout-") as temporary:
        evidence = pathlib.Path(temporary) / "rollback-source-up.stderr"
        try:
            runner.run(
                [
                    sys.executable,
                    "-c",
                    "import sys, time; "
                    "print('partial stdout', flush=True); "
                    "print('partial stderr', file=sys.stderr, flush=True); "
                    "time.sleep(3)",
                ],
                check=False,
                timeout=1,
                operation=operation,
                failure_evidence=evidence,
            )
        except runner.MoveError as error:
            message = str(error)
        else:
            raise AssertionError("timed-out command unexpectedly returned")

        if (
            operation not in message
            or "timed out" not in message
            or str(evidence) not in message
        ):
            raise AssertionError(f"timeout error omitted its operation or evidence path: {message}")
        if "partial stdout" in message or "partial stderr" in message:
            raise AssertionError("captured timeout output was exposed in the error message")
        diagnostic = evidence.read_bytes()
        for expected in (
            f"operation: {operation}".encode(),
            b"timeout: 1",
            b"--- stdout ---\npartial stdout",
            b"--- stderr ---\npartial stderr",
        ):
            if expected not in diagnostic:
                raise AssertionError(f"private timeout evidence omitted {expected!r}")
        if evidence.stat().st_mode & 0o777 != 0o600:
            raise AssertionError("timeout evidence is not private")

    print("PASS timed-out commands preserve private diagnostics and operation labels")


if __name__ == "__main__":
    main()
