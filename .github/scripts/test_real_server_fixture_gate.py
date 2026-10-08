#!/usr/bin/env python3
"""Regression tests for the real-server fixture JSON gate."""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


PACKAGE = "github.com/teagramhq/teagram-server/test/e2e"
GATE = Path(__file__).with_name("real_server_fixture_gate.py")
REQUIRED_TESTS = (
    "TestRealServerFixtureUsesCIAMD64BrowserImage",
    "TestRealServerFixtureValidatesDockerEndpointBeforeDaemonAccess",
    "TestRealServerFixtureValidatesDockerEndpointBeforeDaemonAccess/DOCKER_HOST",
    "TestRealServerFixtureValidatesDockerEndpointBeforeDaemonAccess/"
    "DOCKER_CONTEXT_overrides_DOCKER_HOST",
    "TestRealServerFixtureValidatesDockerEndpointBeforeDaemonAccess/saved_active_context",
    "TestRealServerFixtureValidatesDockerEndpointBeforeDaemonAccess/workspace-local_daemon_is_pinned",
    "TestRealServerFixtureRejectsLiveEndpointBeforeMutation",
    "TestRealServerFixtureRejectsMovingWebRevisionBeforeMutation",
    "TestRealServerFixtureAcceptsImmutableHistoricalRevisionPairBeforeMutation",
    "TestRealServerFixtureRejectsResourceCollisionBeforeMutation",
    "TestRealServerFixtureContextOutlivesCleanup",
    "TestRealServerFixtureContextOutlivesCleanup/fixture_cleanup",
    "TestRealServerFixturePreservesLaunchOrder",
    "TestRealServerFixture",
    "TestRealServerFixture/ArtifactAttachment",
    "TestRealServerFixtureArtifactAttachmentFailureCleanup",
    "TestRealServerFixtureArtifactProductHostAttemptFailsRun",
    "TestRealServerFixtureArtifactWorkerStartupAttemptFailsRun",
    "TestRealServerFixtureArtifactControlledProbeURLAttemptFailsRun",
    "TestRealServerFixtureAcceptsProductionWebArtifact",
    "TestRealServerFixtureHistoricalWebArtifactShapeFailsStagingAudit",
    "TestRealServerFixtureAttemptsHistoricalRevisionPairAndFailsAtArtifactAudit",
    "TestRealServerFixtureCancellationDuringAttachmentCleanup",
    "TestRealServerFixtureStartupFailureCleanup",
    "TestRealServerFixtureReadinessTimeoutCleanup",
    "TestRealServerFixtureCleanupFailureIsNonzero",
    "TestRealServerFixtureRejectsTargetKeyMismatch",
    "TestRealServerFixtureCommandRunsCleanupOnDeadline",
    "TestRealServerFixtureCommandRunsCleanupWhileChildActive",
)


def passing_events() -> list[dict[str, str]]:
    events = [{"Package": PACKAGE, "Action": "start"}]
    for test_name in REQUIRED_TESTS:
        events.append({"Package": PACKAGE, "Action": "run", "Test": test_name})
        events.append({"Package": PACKAGE, "Action": "pass", "Test": test_name})
    events.append({"Package": PACKAGE, "Action": "pass"})
    return events


def run_gate(events: list[dict[str, str]]) -> subprocess.CompletedProcess[str]:
    with tempfile.TemporaryDirectory(prefix="fixture-gate-") as temp_dir:
        result_path = Path(temp_dir) / "go-test.json"
        result_path.write_text(
            "".join(json.dumps(event) + "\n" for event in events), encoding="utf-8"
        )
        return subprocess.run(
            [sys.executable, str(GATE), str(result_path)],
            check=False,
            capture_output=True,
            text=True,
            encoding="utf-8",
        )


class RealServerFixtureGateTests(unittest.TestCase):
    def test_artifact_boundary_suite(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]
        artifact_tests = repo_root / "test" / "e2e" / "real_server_fixture" / "artifact_test.py"
        result = subprocess.run(
            [sys.executable, str(artifact_tests)],
            check=False,
            capture_output=True,
            text=True,
            encoding="utf-8",
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_accepts_every_fixture_case_when_all_pass(self) -> None:
        result = run_gate(passing_events())
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_rejects_missing_fixture_case(self) -> None:
        events = [
            event
            for event in passing_events()
            if event.get("Test") != "TestRealServerFixtureRejectsTargetKeyMismatch"
        ]
        result = run_gate(events)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing", result.stderr.lower())

    def test_rejects_skipped_fixture_case(self) -> None:
        events = passing_events()
        for event in events:
            if event.get("Test") == "TestRealServerFixtureReadinessTimeoutCleanup" and event["Action"] == "pass":
                event["Action"] = "skip"
        result = run_gate(events)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("skip", result.stderr.lower())

    def test_accepts_two_invocations_merged_into_one_stream(self) -> None:
        # The negative-control pair case runs in its own go test window and its
        # stream is appended to the suite's, so the gate verifies the merged
        # stream instead of assuming a single invocation.
        result = run_gate(passing_events() + passing_events())
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_rejects_zero_selected_tests(self) -> None:
        result = run_gate(
            [
                {"Package": PACKAGE, "Action": "start"},
                {"Package": PACKAGE, "Action": "pass"},
            ]
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing", result.stderr.lower())


if __name__ == "__main__":
    unittest.main()
