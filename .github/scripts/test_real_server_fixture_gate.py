#!/usr/bin/env python3
"""Regression tests for the real-server fixture JSON gate."""

from __future__ import annotations

import fnmatch
import json
import os
import re
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


PACKAGE = "github.com/teagramhq/teagram-server/test/e2e"
GATE = Path(__file__).with_name("real_server_fixture_gate.py")
REQUIRED_TESTS = (
    "TestRealServerFixtureUsesCIPinnedBrowserImagesByDockerArchitecture",
    "TestRealServerFixtureInstallsPhotoWorkerFromServerRevision",
    "TestRealServerFixtureUsesUniqueGitWorktreeRegistrationName",
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
    "TestRealServerFixtureArtifactMissingSameOriginResponseFailsAttach",
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
    "TestRealServerFixtureAuthProgressBuildIsTaggedAndProcessScoped",
    "TestRealServerFixtureAuthProgressControlOutcomesAreClosed",
    "TestRealServerFixtureAuthProgressMapsRejectedReferenceBindingToMalformed",
    "TestRealServerFixtureAuthProgressModeIsExplicit",
    "TestRealServerFixtureAuthProgressPairedCleanupRequiresBothLegsAndKeyRemoval",
    "TestRealServerFixtureAuthProgressPreservesPendingAtRunnerExit",
    "TestRealServerFixtureAuthProgressRejectsChangedArtifactBytesWithinOrBetweenLegs",
    "TestRealServerFixtureAuthProgressRejectsHistoricalDigestBinding",
    "TestRealServerFixtureAuthProgressRejectsImpossibleArithmetic",
    "TestRealServerFixtureAuthProgressRejectsImpossibleArithmetic/negative_pending",
    "TestRealServerFixtureAuthProgressRejectsImpossibleArithmetic/settled_exceeds_started",
    "TestRealServerFixtureAuthProgressRejectsImpossibleArithmetic/settled_total_mismatch",
    "TestRealServerFixtureAuthProgressRejectsMalformedReports",
    "TestRealServerFixtureAuthProgressRejectsMalformedReports/duplicate_key",
    "TestRealServerFixtureAuthProgressRejectsMalformedReports/extra_key",
    "TestRealServerFixtureAuthProgressRejectsMalformedReports/missing_nested_field",
    "TestRealServerFixtureAuthProgressRejectsMalformedReports/non_enum_boundary",
    "TestRealServerFixtureAuthProgressRejectsMalformedReports/non_integer",
    "TestRealServerFixtureAuthProgressRejectsMalformedReports/null_integer",
    "TestRealServerFixtureAuthProgressRejectsMalformedReports/out_of_range",
    "TestRealServerFixtureAuthProgressRejectsMalformedReports/trailing_data",
    "TestRealServerFixtureAuthProgressRequiresASetOncePreBaselineBinding",
    "TestRealServerFixtureAuthProgressStrictSchemaAndDeltas",
    "TestRealServerFixtureAuthProgressUnavailableReasonsAreClosedAndDeterministic",
    "TestRealServerFixtureAuthProgressUnavailableReasonsAreClosedAndDeterministic/cancellation_wins_combined_failures",
    "TestRealServerFixtureAuthProgressUnavailableReasonsAreClosedAndDeterministic/cancelled",
    "TestRealServerFixtureAuthProgressUnavailableReasonsAreClosedAndDeterministic/combined_exporter_failures",
    "TestRealServerFixtureAuthProgressUnavailableReasonsAreClosedAndDeterministic/counter_decreases",
    "TestRealServerFixtureAuthProgressUnavailableReasonsAreClosedAndDeterministic/counter_overflow",
    "TestRealServerFixtureAuthProgressUnavailableReasonsAreClosedAndDeterministic/counter_reset",
    "TestRealServerFixtureAuthProgressUnavailableReasonsAreClosedAndDeterministic/drop_at_first_boundary",
    "TestRealServerFixtureAuthProgressUnavailableReasonsAreClosedAndDeterministic/exporter_error_at_final_drain",
    "TestRealServerFixtureAuthProgressUnavailableReasonsAreClosedAndDeterministic/exporter_outstanding_drains",
    "TestRealServerFixtureAuthProgressUnavailableReasonsAreClosedAndDeterministic/exporter_panic",
    "TestRealServerFixtureAuthProgressUnavailableReasonsAreClosedAndDeterministic/final_export_remains_queued",
    "TestRealServerFixtureAuthProgressUnavailableReasonsAreClosedAndDeterministic/missing_runner_boundary",
    "TestRealServerFixtureAuthProgressUnavailableReasonsAreClosedAndDeterministic/report_incomplete",
    "TestRealServerFixtureAuthProgressUnavailableReasonsAreClosedAndDeterministic/setup_request_pending_at_baseline",
    "TestRealServerFixtureAuthProgressUnavailableReasonsAreClosedAndDeterministic/teardown_failure",
    "TestRealServerFixtureAuthProgressBrowserReportStrictSchema",
    "TestRealServerFixtureAuthProgressBrowserReportRejectsMalformedOrForeignOutput",
    "TestRealServerFixtureAuthProgressBrowserObservationPreservesMeasuredOutcomes",
    "TestRealServerFixtureAuthProgressBrowserControlsStayBeforeBaseline",
    "TestRealServerFixtureAuthProgressBrowserObservation",
    "TestRealServerFixtureTaggedParityAndPositiveControls",
)
UPGRADE_GRID_TEST = "TestRealServerFixtureAuthProgressBrowserReportRequiresExactUpgradeGrid"
RUNNER_BINDING_TEST = "TestRealServerFixtureAuthProgressBrowserReportRunnerCheckoutBinding"
UPGRADE_GRID_CASES = (
    UPGRADE_GRID_TEST,
    "TestRealServerFixtureAuthProgressBrowserReportRejectsLegacyRunnerProvenance",
    RUNNER_BINDING_TEST,
    RUNNER_BINDING_TEST + "/097794ab18ef853ca09c3491b9df4e9a702d0ca6",
    RUNNER_BINDING_TEST + "/877aeffca544728d415946d8e7ccb4aeae12df28",
    UPGRADE_GRID_TEST + "/missing_grid",
    UPGRADE_GRID_TEST + "/case_variant_grid",
    UPGRADE_GRID_TEST + "/runner_supplied_residual",
    UPGRADE_GRID_TEST + "/null_grid",
    UPGRADE_GRID_TEST + "/extra_context_bob",
    UPGRADE_GRID_TEST + "/extra_context_Alice",
    UPGRADE_GRID_TEST + "/extra_context_unattributed",
    UPGRADE_GRID_TEST + "/extra_phase",
) + tuple(
    f"{UPGRADE_GRID_TEST}/{mutation}_{context}"
    for mutation in ("missing", "case_variant")
    for context in ("alice", "control", "other")
) + tuple(
    f"{UPGRADE_GRID_TEST}/{mutation}_{context}/{phase}"
    for mutation in ("missing", "case_variant")
    for context in ("alice", "control", "other")
    for phase in ("pre_submit", "post_submit", "unknown")
) + tuple(
    f"{UPGRADE_GRID_TEST}/invalid_{context}/{phase}{suffix}"
    for context in ("alice", "control", "other")
    for phase in ("pre_submit", "post_submit", "unknown")
    for suffix in ("", "#01", "#02", "#03", "#04")
)
REQUIRED_TESTS += UPGRADE_GRID_CASES
WORKER_MAP_TEST = "TestRealServerFixtureAuthProgressBrowserReportWorkerMapKeysAreClosed"
WORKER_MAP_CASES = (
    WORKER_MAP_TEST,
    "TestRealServerFixtureAuthProgressBrowserReportRetainsPartialFixedWorkerMaps",
) + tuple(
    f"{WORKER_MAP_TEST}/{case}"
    for case in (
        "extra_context", "renamed_context", "case_variant_context",
        "extra_control", "renamed_control", "case_variant_control",
    )
)
REQUIRED_TESTS += WORKER_MAP_CASES
OPTIONAL_RUNNER_TEST = "TestRealServerFixtureAuthProgressBrowserObservation"
RUNNER_CHECKOUT_ENV = "TEAGRAM_FIXTURE_AUTH_PROGRESS_RUNNER_CHECKOUT"


def passing_events() -> list[dict[str, str]]:
    events = [{"Package": PACKAGE, "Action": "start"}]
    for test_name in REQUIRED_TESTS:
        events.append({"Package": PACKAGE, "Action": "run", "Test": test_name})
        events.append({"Package": PACKAGE, "Action": "pass", "Test": test_name})
    events.append({"Package": PACKAGE, "Action": "pass"})
    return events


def run_gate(
    events: list[dict[str, str]], runner_checkout: str | None = None
) -> subprocess.CompletedProcess[str]:
    with tempfile.TemporaryDirectory(prefix="fixture-gate-") as temp_dir:
        result_path = Path(temp_dir) / "go-test.json"
        result_path.write_text(
            "".join(json.dumps(event) + "\n" for event in events), encoding="utf-8"
        )
        environment = os.environ.copy()
        if runner_checkout is None:
            environment.pop(RUNNER_CHECKOUT_ENV, None)
        else:
            environment[RUNNER_CHECKOUT_ENV] = runner_checkout
        return subprocess.run(
            [sys.executable, str(GATE), str(result_path)],
            check=False,
            capture_output=True,
            text=True,
            encoding="utf-8",
            env=environment,
        )


class RealServerFixtureGateTests(unittest.TestCase):
    def test_ci_checkouts_do_not_persist_write_scoped_tokens(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]
        workflow = (repo_root / ".github" / "workflows" / "ci.yml").read_text(encoding="utf-8")
        ci_job = re.search(r"(?ms)^  ci-main:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n)", workflow)
        fixture_job = re.search(
            r"(?ms)^  real-server-fixtures:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n)",
            workflow,
        )
        self.assertIsNotNone(ci_job, "CI workflow has no ci-main job")
        self.assertIsNotNone(fixture_job, "CI workflow has no real-server-fixtures job")
        self.assertRegex(
            ci_job.group("body"),
            r"(?m)^      - uses: actions/checkout@\S+\n        with:\n          persist-credentials: false$",
        )
        self.assertRegex(
            fixture_job.group("body"),
            r"(?m)^      - uses: actions/checkout@\S+\n        with:\n          persist-credentials: false$",
        )

    def test_required_ci_check_gates_all_required_lanes(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]
        workflow = (repo_root / ".github" / "workflows" / "ci.yml").read_text(encoding="utf-8")
        ci_job = re.search(r"(?ms)^  ci:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n)", workflow)
        self.assertIsNotNone(ci_job, "CI workflow has no required ci check")
        body = ci_job.group("body")
        self.assertRegex(body, r"(?m)^    name: ci$")
        self.assertRegex(body, r"(?m)^    if: \$\{\{ always\(\) \}\}$")
        self.assertRegex(
            body,
            r"(?ms)^    needs:\n      - prioritize-main\n      - ci-main\n      - sticker-bundle\n      - photothumb-runtime\n      - real-server-fixtures\n",
        )
        self.assertIn("${{ needs.ci-main.result }}", body)
        self.assertIn("${{ needs.sticker-bundle.result }}", body)
        self.assertIn("${{ needs.photothumb-runtime.result }}", body)
        self.assertIn("${{ needs.real-server-fixtures.result }}", body)
        self.assertIn('"$STICKER_BUNDLE_RESULT" != "success"', body)
        self.assertIn('"$PHOTOTHUMB_RUNTIME_RESULT" != "success"', body)

    def test_e2e_selectors_partition_fixture_tests_from_remaining_suite(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]
        scripts = repo_root / ".github" / "scripts"
        remaining = (scripts / "run-e2e-diagnostics.sh").read_text(encoding="utf-8")
        fixtures = (scripts / "run-real-server-fixture-gate.sh").read_text(encoding="utf-8")
        self.assertIn("fixture_test_prefix='^TestRealServerFixture'", remaining)
        self.assertIn(
            'go test -race -count=1 -timeout 20m -json -skip "$fixture_test_prefix" "$SMOKE_E2E_PACKAGE"',
            remaining,
        )
        self.assertIn(
            'go test -race -count=1 -timeout 25m -json -run \'^TestRealServerFixture\' ./test/e2e',
            fixtures,
        )
        self.assertIn(
            'and all($events[]; ((.Test // "") | startswith("TestRealServerFixture") | not))',
            remaining,
        )
        self.assertNotIn("real_server_fixture_gate.py", remaining)
        self.assertIn('real_server_fixture_gate.py" "$json_file"', fixtures)
        self.assertIn(
            'report_smoke_failure_diagnostics "$status" real-server-fixture "$json_file" || true',
            fixtures,
        )
        self.assertIn('exit "$status"', fixtures)
        self.assertNotIn('cat "$json_file"', fixtures)

    def test_fixture_lane_downloads_modules_before_json_capture(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]
        workflow = (repo_root / ".github" / "workflows" / "ci.yml").read_text(
            encoding="utf-8"
        )
        fixture_job = re.search(
            r"(?ms)^  real-server-fixtures:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n)",
            workflow,
        )
        self.assertIsNotNone(fixture_job, "CI workflow has no real-server-fixtures job")
        body = fixture_job.group("body")
        download_step = body.find("- name: Download Go modules")
        fixture_test_step = body.find(
            "run: bash .github/scripts/run-real-server-fixture-gate.sh"
        )
        self.assertGreaterEqual(download_step, 0, "fixture lane does not download Go modules")
        self.assertGreater(
            fixture_test_step, download_step, "module download must precede JSON capture"
        )
        self.assertIn("run: go mod download", body[download_step:fixture_test_step])

    def test_fixture_failure_diagnostics_match_database_container_name(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]
        workflow = (repo_root / ".github" / "workflows" / "ci.yml").read_text(
            encoding="utf-8"
        )
        start_script = (
            repo_root / "test" / "e2e" / "real_server_fixture" / "start.sh"
        ).read_text(encoding="utf-8")
        fixture_job = re.search(
            r"(?ms)^  real-server-fixtures:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n)",
            workflow,
        )
        self.assertIsNotNone(fixture_job, "CI workflow has no real-server-fixtures job")
        self.assertIn('PREFIX="teagram-fixture-$RUN_ID"', start_script)
        self.assertIn('DATABASE="$PREFIX"database', start_script)
        diagnostics = fixture_job.group("body").split(
            "- name: Fixture Postgres container state on failure", maxsplit=1
        )[1]
        pattern = re.search(r"(?m)^\s+([^\s)]+database)\) printf", diagnostics)
        self.assertIsNotNone(pattern, "fixture diagnostics do not select database containers")
        container_name = f"teagram-fixture-{'a' * 32}database"
        self.assertTrue(fnmatch.fnmatchcase(container_name, pattern.group(1)))

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

    def test_requires_auth_progress_and_parity_cases(self) -> None:
        cases = (
            "TestRealServerFixtureAuthProgressStrictSchemaAndDeltas",
            "TestRealServerFixtureAuthProgressRejectsMalformedReports/duplicate_key",
            "TestRealServerFixtureTaggedParityAndPositiveControls",
        )
        for test_name in cases:
            for action in ("missing", "skip", "fail"):
                with self.subTest(test=test_name, action=action):
                    events = passing_events()
                    if action == "missing":
                        events = [event for event in events if event.get("Test") != test_name]
                    else:
                        for event in events:
                            if event.get("Test") == test_name and event["Action"] == "pass":
                                event["Action"] = action
                    result = run_gate(events)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(action if action != "fail" else "failed", result.stderr)
                    self.assertNotIn("unlisted", result.stderr)

    def test_requires_upgrade_grid_and_runner_binding_cases(self) -> None:
        for test_name in UPGRADE_GRID_CASES + WORKER_MAP_CASES:
            for action in ("missing", "skip", "fail"):
                with self.subTest(test=test_name, action=action):
                    events = passing_events()
                    if action == "missing":
                        events = [event for event in events if event.get("Test") != test_name]
                    else:
                        for event in events:
                            if event.get("Test") == test_name and event["Action"] == "pass":
                                event["Action"] = action
                    result = run_gate(events)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(action if action != "fail" else "failed", result.stderr)
                    self.assertNotIn("unlisted", result.stderr)

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

    def test_allows_opt_in_runner_case_to_skip_without_runner_checkout(self) -> None:
        events = passing_events()
        for event in events:
            if event.get("Test") == OPTIONAL_RUNNER_TEST and event["Action"] == "pass":
                event["Action"] = "skip"
        result = run_gate(events)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_requires_opt_in_runner_case_when_checkout_is_configured(self) -> None:
        events = passing_events()
        for event in events:
            if event.get("Test") == OPTIONAL_RUNNER_TEST and event["Action"] == "pass":
                event["Action"] = "skip"
        result = run_gate(events, "/configured/runner/checkout")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("skip", result.stderr.lower())

    def test_rejects_failed_fixture_case(self) -> None:
        events = passing_events()
        for event in events:
            if event.get("Test") == "TestRealServerFixtureReadinessTimeoutCleanup" and event["Action"] == "pass":
                event["Action"] = "fail"
        result = run_gate(events)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("failed", result.stderr.lower())

    def test_rejects_incomplete_package_result(self) -> None:
        result = run_gate(passing_events()[:-1])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("final", result.stderr.lower())

    def test_rejects_unlisted_fixture_case(self) -> None:
        events = passing_events()
        events[1:1] = [
            {"Package": PACKAGE, "Action": "run", "Test": "TestRealServerFixtureNewCase"},
            {"Package": PACKAGE, "Action": "pass", "Test": "TestRealServerFixtureNewCase"},
        ]
        result = run_gate(events)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unlisted", result.stderr.lower())

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
