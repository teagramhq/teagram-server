#!/usr/bin/env python3
"""Require the real-server fixture tests to run and pass in Go's JSON stream."""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Iterable


PACKAGE = "github.com/teagramhq/teagram-server/test/e2e"
FIXTURE_TEST_PREFIX = "TestRealServerFixture"
REQUIRED_TESTS = frozenset(
    {
        "TestRealServerFixtureUsesCIAMD64BrowserImage",
        "TestRealServerFixtureValidatesDockerEndpointBeforeDaemonAccess",
        "TestRealServerFixtureValidatesDockerEndpointBeforeDaemonAccess/DOCKER_HOST",
        "TestRealServerFixtureValidatesDockerEndpointBeforeDaemonAccess/"
        "DOCKER_CONTEXT_overrides_DOCKER_HOST",
        "TestRealServerFixtureValidatesDockerEndpointBeforeDaemonAccess/saved_active_context",
        "TestRealServerFixtureValidatesDockerEndpointBeforeDaemonAccess/workspace-local_daemon_is_pinned",
        "TestRealServerFixtureRejectsLiveEndpointBeforeMutation",
        "TestRealServerFixtureRejectsUnapprovedWebRevisionBeforeMutation",
        "TestRealServerFixtureRejectsServerRevisionMismatchBeforeMutation",
        "TestRealServerFixtureRejectsResourceCollisionBeforeMutation",
        "TestRealServerFixtureContextOutlivesCleanup",
        "TestRealServerFixtureContextOutlivesCleanup/fixture_cleanup",
        "TestRealServerFixturePreservesLaunchOrder",
        "TestRealServerFixture",
        "TestRealServerFixtureStartupFailureCleanup",
        "TestRealServerFixtureReadinessTimeoutCleanup",
        "TestRealServerFixtureCleanupFailureIsNonzero",
        "TestRealServerFixtureRejectsTargetKeyMismatch",
        "TestRealServerFixtureCommandRunsCleanupOnDeadline",
        "TestRealServerFixtureCommandRunsCleanupWhileChildActive",
    }
)
JSON_ACTIONS = frozenset(
    {"start", "run", "pause", "cont", "output", "pass", "bench", "fail", "skip"}
)
TERMINAL_ACTIONS = frozenset({"pass", "fail", "skip"})


class GateError(ValueError):
    """The Go test result stream does not prove complete fixture coverage."""


def verify_events(lines: Iterable[str]) -> None:
    events: list[dict[str, object]] = []
    for line_number, line in enumerate(lines, start=1):
        if not line.strip():
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError as error:
            raise GateError(f"line {line_number} is not valid JSON") from error
        if not isinstance(event, dict):
            raise GateError(f"line {line_number} is not a JSON object")
        if event.get("Package") != PACKAGE:
            raise GateError(f"line {line_number} belongs to an unexpected package")
        action = event.get("Action")
        if not isinstance(action, str) or action not in JSON_ACTIONS:
            raise GateError(f"line {line_number} has an unsupported Go test action")
        test_name = event.get("Test")
        if test_name is not None and not isinstance(test_name, str):
            raise GateError(f"line {line_number} has an invalid test name")
        events.append(event)

    if not events:
        raise GateError("Go test produced no JSON events")

    if events[-1].get("Test") or events[-1].get("Action") != "pass":
        raise GateError("the final Go test JSON event is not a package pass")
    package_terminals = [
        event["Action"]
        for event in events
        if not event.get("Test") and event.get("Action") in TERMINAL_ACTIONS
    ]
    if not package_terminals or package_terminals[-1] != "pass":
        raise GateError("the e2e package did not report a final passing result")

    fixture_events = [
        event
        for event in events
        if isinstance(event.get("Test"), str)
        and event["Test"].startswith(FIXTURE_TEST_PREFIX)
    ]
    observed_tests = {event["Test"] for event in fixture_events}
    unexpected = observed_tests - REQUIRED_TESTS
    if unexpected:
        raise GateError("unlisted fixture tests: " + ", ".join(sorted(unexpected)))

    started = {
        event["Test"] for event in fixture_events if event.get("Action") == "run"
    }
    missing = REQUIRED_TESTS - observed_tests
    if missing:
        raise GateError("missing fixture tests: " + ", ".join(sorted(missing)))
    missing_runs = REQUIRED_TESTS - started
    if missing_runs:
        raise GateError("fixture tests did not start: " + ", ".join(sorted(missing_runs)))

    skipped = {
        event["Test"]
        for event in fixture_events
        if event.get("Action") == "skip" and isinstance(event.get("Test"), str)
    }
    if skipped:
        raise GateError("fixture tests were skipped: " + ", ".join(sorted(skipped)))
    failed = {
        event["Test"]
        for event in fixture_events
        if event.get("Action") == "fail" and isinstance(event.get("Test"), str)
    }
    if failed:
        raise GateError("fixture tests failed: " + ", ".join(sorted(failed)))

    passed = {
        event["Test"]
        for event in fixture_events
        if event.get("Action") == "pass" and isinstance(event.get("Test"), str)
    }
    missing_passes = REQUIRED_TESTS - passed
    if missing_passes:
        raise GateError(
            "fixture tests did not report pass: " + ", ".join(sorted(missing_passes))
        )


def main() -> int:
    if len(sys.argv) != 2:
        print(f"usage: {Path(sys.argv[0]).name} <go-test-json-file>", file=sys.stderr)
        return 2
    try:
        with Path(sys.argv[1]).open(encoding="utf-8") as result_file:
            verify_events(result_file)
    except (OSError, GateError) as error:
        print(f"real-server fixture gate rejected test results: {error}", file=sys.stderr)
        return 1
    print("real-server fixture gate verified all required tests", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
