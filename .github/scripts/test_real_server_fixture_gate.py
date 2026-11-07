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
import textwrap
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
    def test_qa_invite_uses_resolved_live_publication(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]
        workflow = (repo_root / ".github/workflows/real-client-browser-flow.yml").read_text(encoding="utf-8")
        self.assertIn("QA_WEB_SOURCE_SHA: ${{ steps.qa-web-pin.outputs.source_commit }}", workflow)
        self.assertIn("QA_WEB_ARTIFACT_DIGEST: ${{ steps.qa-web-pin.outputs.artifact_digest }}", workflow)
        script = (repo_root / "deploy/qa-invite/test-qa-invite-browser-flow.sh").read_text(encoding="utf-8")
        start = script.index('[[ -n "$WEB_ROOT"')
        end = script.index("\nPUBLIC_TARGET=", start)
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "dist-private").mkdir()
            git = root / "git"
            git.write_text('#!/bin/sh\nprintf \'%s\\n\' "$FAKE_SHA"\n', encoding="utf-8")
            git.chmod(0o755)
            python = root / "python3"
            python.write_text(
                '#!/bin/sh\n[ "$2" = --verify-release-identity ] && '
                '[ "$3" = "$QA_WEB_SOURCE_SHA" ] && [ "$4" = "$QA_WEB_ARTIFACT_DIGEST" ] || exit 1\n'
                'printf \'%s\\n\' \'{"endpoint":"wss://web.example/apiws","fingerprint":"cccccccccccccccc"}\'\n',
                encoding="utf-8",
            )
            python.chmod(0o755)
            sha = "a" * 40
            env = {**os.environ, "PATH": f"{root}:{os.environ['PATH']}", "FAKE_SHA": sha,
                   "WEB_ROOT": temp, "WEB_REVISION": sha, "PUBLIC_WEB_ROOT": temp,
                   "QA_WEB_SOURCE_SHA": sha, "QA_WEB_ARTIFACT_DIGEST": "sha256:" + "b" * 64,
                   "REPO_ROOT": str(repo_root)}
            command = ["bash", "-e", "-c", 'fail() { echo "$*" >&2; exit 1; }\n' + script[start:end]]
            result = subprocess.run(command, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            for name, value in (("QA_WEB_SOURCE_SHA", "bad"), ("QA_WEB_ARTIFACT_DIGEST", "bad")):
                with self.subTest(name=name):
                    invalid = {**env, name: value}
                    result = subprocess.run(command, env=invalid, capture_output=True, text=True)
                    self.assertNotEqual(result.returncode, 0)

    def test_real_client_resolves_live_web_without_checked_in_pin(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]
        workflow = (repo_root / ".github/workflows/real-client-browser-flow.yml").read_text(encoding="utf-8")
        step = re.search(
            r"(?m)^        id: qa-web-pin\n        env:\n          GITHUB_TOKEN: \$\{\{ github\.token \}\}\n"
            r"        run: \|\n(?P<script>(?:          [^\n]*\n)+)",
            workflow,
        )
        self.assertIsNotNone(step)
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            resolver = root / "python3"
            resolver.write_text(
                '#!/bin/sh\n[ "$#" = 1 ] || exit 1\n'
                '[ "$1" = deploy/teagram-demo/resolve_web_artifact_pin.py ] || exit 1\n'
                'printf \'%s\\n\' "$PUBLICATION"\n', encoding="utf-8"
            )
            resolver.chmod(0o755)
            output = root / "outputs"
            publication = {"sourceCommit": "a" * 40, "artifactDigest": "sha256:" + "b" * 64, "deployable": True}
            env = {**os.environ, "PATH": f"{root}:{os.environ['PATH']}", "GITHUB_OUTPUT": str(output), "PUBLICATION": json.dumps(publication)}
            command = ["bash", "-e", "-c", textwrap.dedent(step.group("script"))]
            result = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(output.read_text(), f"source_commit={publication['sourceCommit']}\nartifact_digest={publication['artifactDigest']}\n")
            for invalid in ({**publication, "deployable": False}, {**publication, "sourceCommit": "bad"}):
                with self.subTest(publication=invalid):
                    env["PUBLICATION"] = json.dumps(invalid)
                    result = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
                    self.assertNotEqual(result.returncode, 0)

    def test_real_client_browser_checkout_uses_declared_revision(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]
        workflow = (
            repo_root / ".github" / "workflows" / "real-client-browser-flow.yml"
        ).read_text(encoding="utf-8")
        self.assertRegex(
            workflow,
            r"(?ms)^      REAL_CLIENT_WEB_REVISION: [0-9a-f]{40}$.*?"
            r"^      - name: Check out pinned web companion\n"
            r"        uses: [^\n]+\n"
            r"        with:\n"
            r"          repository: teagramhq/teagram-web\n"
            r"          ref: \$\{\{ env\.REAL_CLIENT_WEB_REVISION \}\}$",
        )

    def test_ci_checkouts_do_not_persist_write_scoped_tokens(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]
        workflow = (repo_root / ".github" / "workflows" / "ci.yml").read_text(encoding="utf-8")
        checkout = r"(?m)^      - uses: actions/checkout@\S+\n        with:\n          persist-credentials: false$"
        for job_name in (
            "check-sticker-license",
            "test-go",
            "test-e2e-admin",
            "test-real-server-fixtures",
            "build-server-image",
            "test-compose",
        ):
            with self.subTest(job=job_name):
                job = re.search(
                    rf"(?ms)^  {re.escape(job_name)}:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n|\Z)",
                    workflow,
                )
                self.assertIsNotNone(job, f"CI workflow has no {job_name} job")
                self.assertRegex(job.group("body"), checkout)

    def test_admin_e2e_job_is_read_only_and_keeps_cache_fallback(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]
        workflow = (repo_root / ".github" / "workflows" / "ci.yml").read_text(encoding="utf-8")
        job = re.search(
            r"(?ms)^  test-e2e-admin:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n|\Z)",
            workflow,
        )
        self.assertIsNotNone(job, "CI workflow has no test-e2e-admin job")
        permissions = re.search(
            r"(?ms)^    permissions:\n(?P<body>.*?)(?=^    (?:env|steps):)",
            job.group("body"),
        )
        self.assertIsNotNone(permissions, "test-e2e-admin permissions are missing")
        self.assertEqual(
            [line.strip() for line in permissions.group("body").splitlines() if line.strip()],
            ["contents: read"],
        )
        cached_install = re.search(
            r"(?ms)^      - name: Install the system dependencies from cache\n"
            r"(?P<body>.*?)(?=^      - name:|\Z)",
            job.group("body"),
        )
        self.assertIsNotNone(cached_install, "cached dependency install step is missing")
        self.assertIn("falling back to the network install", cached_install.group("body"))
        self.assertNotIn("gh cache delete", cached_install.group("body"))
        self.assertNotIn("GH_TOKEN", cached_install.group("body"))

    def test_required_ci_check_gates_all_required_lanes(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]
        workflow = (repo_root / ".github" / "workflows" / "ci.yml").read_text(encoding="utf-8")
        jobs_block = re.search(r"(?ms)^jobs:\n(?P<body>.*)\Z", workflow)
        ci_job = re.search(r"(?ms)^  ci:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n)", workflow)
        self.assertIsNotNone(jobs_block, "CI workflow has no jobs")
        self.assertIsNotNone(ci_job, "CI workflow has no required ci check")
        body = ci_job.group("body")
        self.assertRegex(body, r"(?m)^    name: ci$")
        self.assertRegex(body, r"(?m)^    if: \$\{\{ always\(\) \}\}$")
        self.assertNotIn("ops-prioritize-main", body)

        # The demo publisher and demo pin are advisory. K8s image publication
        # gates production because the prod overlay pins those images.
        excluded_jobs = {
            "ci",
            "publish-demo-image",
            "check-demo-web-pin",
            "mirror-rustfs-qualification-images",
            "restore-readiness",
            "rustfs-transition-qualification",
        }
        workflow_jobs = set(
            re.findall(r"(?m)^  ([A-Za-z0-9_-]+):\n", jobs_block.group("body"))
        )
        expected_lanes = workflow_jobs - excluded_jobs
        matrix_names = {
            "build-browser-acceptance-image": "build: browser-acceptance-image (${{ matrix.arch }})",
            "test-photo-worker": "test: photo-worker (${{ matrix.arch }})",
        }
        retired_jobs = {
            "mirror-rustfs-qualification-images",
            "restore-readiness",
            "rustfs-transition-qualification",
        }
        for job_id in sorted(workflow_jobs - {"ci", *retired_jobs}):
            job = re.search(
                rf"(?ms)^  {re.escape(job_id)}:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n|\Z)",
                workflow,
            )
            self.assertIsNotNone(job, f"CI workflow has no {job_id} job")
            name = re.search(r"(?m)^    name: ['\"]?(.*?)['\"]?$", job.group("body"))
            self.assertIsNotNone(name, f"CI job {job_id} has no explicit display name")
            if job_id in matrix_names:
                expected_name = matrix_names[job_id]
            else:
                stage, _, subject = job_id.partition("-")
                expected_name = f"{stage}: {subject}"
            self.assertEqual(name.group(1), expected_name, f"CI job {job_id} violates stage naming")

        needs = re.search(
            r"(?ms)^    needs:\n(?P<entries>(?:      - [A-Za-z0-9_-]+\n)+)", body
        )
        self.assertIsNotNone(needs, "CI aggregate has no explicit needs list")
        needed_lanes = re.findall(r"(?m)^      - ([A-Za-z0-9_-]+)$", needs.group("entries"))
        expected_dependencies = expected_lanes
        self.assertEqual(
            needed_lanes[:5],
            [
                "check-security",
                "test-go",
                "check-sticker-license",
                "test-photo-worker",
                "test-real-server-fixtures",
            ],
        )
        self.assertEqual(len(needed_lanes), len(set(needed_lanes)), "CI aggregate repeats a lane")
        self.assertEqual(
            set(needed_lanes), expected_dependencies, "CI aggregate must need every retained PR lane"
        )

        result_env_entries = re.findall(
            r"(?m)^          ([A-Z][A-Z0-9_]*_RESULT): \$\{\{ needs\.([A-Za-z0-9_-]+)\.result \}\}$",
            body,
        )
        result_envs = {lane: env_name for env_name, lane in result_env_entries}
        self.assertEqual(len(result_env_entries), len(result_envs), "CI aggregate repeats a result mapping")
        self.assertEqual(
            set(result_envs), expected_dependencies, "CI aggregate must read every lane result"
        )

        lane_results = re.search(
            r"(?ms)^          lane_results=\(\n(?P<entries>.*?)^          \)", body
        )
        self.assertIsNotNone(lane_results, "CI aggregate has no per-lane result output")
        result_output_entries = re.findall(
            r'(?m)^[ \t]*"([A-Za-z0-9_-]+)=\$([A-Z][A-Z0-9_]*_RESULT)"$',
            lane_results.group("entries"),
        )
        result_output = {lane: env_name for lane, env_name in result_output_entries}
        self.assertEqual(len(result_output_entries), len(result_output), "CI aggregate repeats a result output")
        self.assertEqual(result_output, result_envs, "CI aggregate must report every lane result")

        result_guard = re.search(
            r'(?ms)^          if \[\[ (?P<condition>'
            r'"\$CHECK_SECURITY_RESULT".*?)\]\]; then$',
            body,
        )
        self.assertIsNotNone(result_guard, "CI aggregate has no failing result guard")
        expected_guard = " || ".join(
            (
                f'"${result_envs[lane]}" != "$expected_publish_result"'
                if lane == "publish-k8s-images"
                else f'"${result_envs[lane]}" != "success"'
            )
            for lane in needed_lanes
        )
        self.assertEqual(
            " ".join(result_guard.group("condition").split()),
            expected_guard,
            "CI aggregate must fail unless every retained PR lane passes",
        )

        run_script = re.search(r"(?ms)^        run: \|\n(?P<script>.*)\Z", body)
        self.assertIsNotNone(run_script, "CI aggregate has no executable result gate")
        shell_script = textwrap.dedent(run_script.group("script"))
        environment = os.environ.copy()
        environment.update(
            {
                env_name: "success" for env_name in result_envs.values()
            }
        )
        environment.update(
            {
                "GITHUB_EVENT_NAME": "pull_request",
                "GITHUB_REPOSITORY": "teagramhq/teagram-server",
                "PULL_REQUEST_HEAD_REPO": "teagramhq/teagram-server",
            }
        )
        success = subprocess.run(
            ["bash", "-e", "-c", shell_script],
            capture_output=True,
            check=False,
            env=environment,
            text=True,
        )
        self.assertEqual(success.returncode, 0, success.stdout + success.stderr)

        workflow_triggers = re.search(r"(?ms)^on:\n(?P<body>.*?)^permissions:", workflow)
        self.assertIsNotNone(workflow_triggers, "CI workflow has no event triggers")
        self.assertRegex(
            workflow_triggers.group("body"),
            r"(?m)^  merge_group:\n    types: \[checks_requested\]$",
        )
        merge_group_environment = environment.copy()
        merge_group_environment.update(
            {
                "GITHUB_EVENT_NAME": "merge_group",
                "PULL_REQUEST_HEAD_REPO": "",
                result_envs["publish-k8s-images"]: "skipped",
            }
        )
        merge_group_success = subprocess.run(
            ["bash", "-e", "-c", shell_script],
            capture_output=True,
            check=False,
            env=merge_group_environment,
            text=True,
        )
        self.assertEqual(
            merge_group_success.returncode,
            0,
            merge_group_success.stdout + merge_group_success.stderr,
        )
        merge_group_publisher_ran = merge_group_environment.copy()
        merge_group_publisher_ran[result_envs["publish-k8s-images"]] = "success"
        merge_group_publisher = subprocess.run(
            ["bash", "-e", "-c", shell_script],
            capture_output=True,
            check=False,
            env=merge_group_publisher_ran,
            text=True,
        )
        self.assertNotEqual(
            merge_group_publisher.returncode,
            0,
            "merge-group aggregate returned success when image publication ran",
        )
        merge_group_environment[result_envs["test-go"]] = "skipped"
        merge_group_failure = subprocess.run(
            ["bash", "-e", "-c", shell_script],
            capture_output=True,
            check=False,
            env=merge_group_environment,
            text=True,
        )
        self.assertNotEqual(
            merge_group_failure.returncode,
            0,
            "merge-group aggregate returned success when a required lane was skipped",
        )

        failed_publisher_environment = environment.copy()
        failed_publisher_environment[result_envs["publish-k8s-images"]] = "failure"
        failed_publisher = subprocess.run(
            ["bash", "-e", "-c", shell_script],
            capture_output=True,
            check=False,
            env=failed_publisher_environment,
            text=True,
        )
        self.assertNotEqual(
            failed_publisher.returncode,
            0,
            "CI aggregate returned success when the image publisher failed: "
            + failed_publisher.stdout
            + failed_publisher.stderr,
        )

        fork_environment = environment.copy()
        fork_environment["PULL_REQUEST_HEAD_REPO"] = "fork/teagram-server"
        fork_environment[result_envs["publish-k8s-images"]] = "skipped"
        fork_success = subprocess.run(
            ["bash", "-e", "-c", shell_script],
            capture_output=True,
            check=False,
            env=fork_environment,
            text=True,
        )
        self.assertEqual(fork_success.returncode, 0, fork_success.stdout + fork_success.stderr)

        failed_lane = needed_lanes[0]
        environment[result_envs[failed_lane]] = "failure"
        failure = subprocess.run(
            ["bash", "-e", "-c", shell_script],
            capture_output=True,
            check=False,
            env=environment,
            text=True,
        )
        self.assertNotEqual(
            failure.returncode,
            0,
            "CI aggregate returned success when a required lane failed: "
            + failure.stdout
            + failure.stderr,
        )

    def test_e2e_selectors_partition_fixture_tests_from_remaining_suite(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]
        scripts = repo_root / ".github" / "scripts"
        remaining = (scripts / "run-e2e-diagnostics.sh").read_text(encoding="utf-8")
        fixtures = (scripts / "run-real-server-fixture-gate.sh").read_text(encoding="utf-8")
        self.assertIn("fixture_test_prefix='^TestRealServerFixture'", remaining)
        self.assertIn(
            'go test -race -count=1 -timeout 30m -json -skip "$fixture_test_prefix" "$SMOKE_E2E_PACKAGE"',
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
            r"(?ms)^  test-real-server-fixtures:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n)",
            workflow,
        )
        self.assertIsNotNone(fixture_job, "CI workflow has no test-real-server-fixtures job")
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
            r"(?ms)^  test-real-server-fixtures:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n)",
            workflow,
        )
        self.assertIsNotNone(fixture_job, "CI workflow has no test-real-server-fixtures job")
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
