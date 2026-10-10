#!/usr/bin/env python3
"""Synthetic checks for the disposable paired-smoke delivery boundary."""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import dialog_filter_pair as pair


SCRIPT = Path(__file__).with_name("dialog_filter_pair.py")
REPO_ROOT = Path(__file__).resolve().parents[2]


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def git(root: Path, *args: str) -> str:
    result = subprocess.run(
        ["git", "-C", str(root), *args],
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        encoding="utf-8",
    )
    return result.stdout.strip()


def test_event(action: str, test_name: str | None = None, output: str | None = None) -> dict[str, str]:
    event = {"Action": action, "Package": pair.PACKAGE}
    if test_name is not None:
        event["Test"] = test_name
    if output is not None:
        event["Output"] = output
    return event


def observer_record(variant: str, outcome: str = "pass") -> dict[str, object]:
    assertions = ["dialog-filters.pair-observer-overflow", "dialog-filters.pair-observer-order"]
    if variant == "immediate":
        assertions.append("dialog-filters.pair-immediate-read")
        issue_sequence = 2
        return_sequence = 2
    else:
        assertions.extend(
            (
                "dialog-filters.pair-gated-replacement-ready",
                "dialog-filters.pair-gated-auth-status",
                "dialog-filters.pair-gated-read",
            )
        )
        issue_sequence = 4
        return_sequence = 4
    record: dict[str, object] = {
        "schema": 1,
        "variant": variant,
        "states": [
            {"seq": 1, "state": "ready"},
            {"seq": 2, "state": "disconnected"},
            {"seq": 3, "state": "connecting"},
            {"seq": 4, "state": "ready"},
        ],
        "restartMark": 1,
        "readIssueSeq": issue_sequence,
        "readReturnSeq": return_sequence,
        "replacementReadySeq": 4,
        "overflow": 0,
        "acceptCount": 3,
        "assertions": assertions,
        "safeErrorClass": None,
        "outcome": outcome,
    }
    if variant == "gated":
        record["authorized"] = True
    return record


def observer_output(record: dict[str, object], line: int = 500) -> str:
    encoded = json.dumps(record, separators=(",", ":"), sort_keys=True)
    return f"    smoke_test.go:{line}: PAIR_OBSERVER {encoded}\n"


class DialogFilterPairVerifierTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="dialog-filter-pair-")
        self.temp_root = Path(self.temp.name)
        self.repo = self.temp_root / "source"
        self.repo.mkdir()
        subprocess.run(["git", "init", "--quiet", str(self.repo)], check=True, capture_output=True)
        git(self.repo, "config", "user.name", "Synthetic Fixture")
        git(self.repo, "config", "user.email", "fixture@example.invalid")
        scripts = self.repo / ".github/scripts"
        scripts.mkdir(parents=True)
        (scripts / "smoke-scenarios.sh").write_bytes(
            (REPO_ROOT / ".github/scripts/smoke-scenarios.sh").read_bytes()
        )
        git(self.repo, "add", ".github/scripts/smoke-scenarios.sh")
        git(self.repo, "commit", "--quiet", "-m", "synthetic source")
        (self.repo / "test/e2e").mkdir(parents=True)
        (self.repo / "test/e2e/observer_test.go").write_text("package e2e\n", encoding="utf-8")
        git(self.repo, "add", "test/e2e/observer_test.go")
        git(self.repo, "commit", "--quiet", "-m", "synthetic observer patch")
        self.patch_sha = git(self.repo, "rev-parse", "HEAD")
        self.input_path = self.temp_root / "go-test.jsonl"
        self.summary_path = self.temp_root / "allowlisted-summary.txt"

    def tearDown(self) -> None:
        self.temp.cleanup()

    def run_verifier(
        self,
        events: list[dict[str, object]],
        *,
        arm: str = "focused",
        go_status: int = 0,
    ) -> subprocess.CompletedProcess[str]:
        self.input_path.write_text(
            "".join(json.dumps(event, separators=(",", ":")) + "\n" for event in events),
            encoding="utf-8",
        )
        return subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "verify",
                "--root",
                str(self.repo),
                "--arm",
                arm,
                "--go-status",
                str(go_status),
                "--workflow-sha",
                "a" * 40,
                "--source-sha",
                pair.SOURCE_SHA,
                "--patch-sha",
                self.patch_sha,
                "--input",
                str(self.input_path),
                "--summary",
                str(self.summary_path),
            ],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            encoding="utf-8",
        )

    def synthetic_events(self, *, sibling: bool = False, failure: bool = False) -> list[dict[str, object]]:
        events: list[dict[str, object]] = [test_event("start"), test_event("run", "TestSmoke")]
        scenarios = pair.smoke_scenarios(REPO_ROOT) if sibling else ("dialog-filters",)
        for scenario in scenarios:
            scenario_test = f"TestSmoke/{scenario}"
            events.append(test_event("run", scenario_test))
            if scenario == "dialog-filters":
                immediate_test = f"{scenario_test}/pair-immediate"
                gated_test = f"{scenario_test}/pair-gated"
                events.append(test_event("run", immediate_test))
                if failure:
                    events.append(
                        test_event(
                            "output",
                            immediate_test,
                            "    smoke_test.go:1114: [assert:dialog-filters.other-session-restart-read-error] read failed\n",
                        )
                    )
                immediate_record = observer_record("immediate", "failure" if failure else "pass")
                if failure:
                    immediate_record["safeErrorClass"] = "net closed/EOF"
                    immediate_record["assertions"].append(
                        "dialog-filters.other-session-restart-read-error"
                    )
                events.append(test_event("output", immediate_test, observer_output(immediate_record)))
                events.append(test_event("fail" if failure else "pass", immediate_test))
                events.append(test_event("run", gated_test))
                events.append(test_event("output", gated_test, observer_output(observer_record("gated"), 600)))
                events.append(test_event("pass", gated_test))
            events.append(test_event("fail" if failure and scenario == "dialog-filters" else "pass", scenario_test))
        events.append(test_event("fail" if failure else "pass", "TestSmoke"))
        events.append(test_event("run", pair.CLEANUP_TEST))
        events.append(test_event("pass", pair.CLEANUP_TEST))
        events.append(test_event("pass"))
        return events

    def expect_rejected(self, result: subprocess.CompletedProcess[str], sentinel: str | None = None) -> None:
        require(result.returncode == 1, "invalid observer input was accepted")
        require(result.stdout == "observer-rejected\n", "rejection output was not fixed")
        require(result.stderr == "", "verifier wrote stderr")
        require(self.summary_path.read_text(encoding="utf-8") == "observer-rejected\n", "rejection artifact was not fixed")
        if sentinel is not None:
            combined = result.stdout + result.stderr + self.summary_path.read_text(encoding="utf-8")
            require(sentinel not in combined, "synthetic sentinel escaped containment")

    def test_focused_accepts_omitted_sibling_scenarios_and_cleanup_test(self) -> None:
        result = self.run_verifier(self.synthetic_events())
        require(result.returncode == 0, "focused selection was rejected")
        summary = self.summary_path.read_text(encoding="utf-8")
        require("selection=focused\n" in summary, "focused selection summary missing")
        require("cleanup_order_test=pass\n" in summary, "cleanup-order test was not accounted for")
        require("dialog-filters.other-session-restart-read-error" not in summary, "unexpected assertion appeared")
        require("one-to-one" not in summary, "focused summary included a sibling")
        require(result.stderr == "", "verifier wrote stderr")

    def test_sibling_requires_every_declared_scenario(self) -> None:
        events = self.synthetic_events(sibling=True)
        events = [event for event in events if event.get("Test") not in {"TestSmoke/one-to-one"}]
        result = self.run_verifier(events, arm="sibling")
        self.expect_rejected(result)

    def test_sibling_accepts_all_scenarios_and_cleanup_test(self) -> None:
        result = self.run_verifier(self.synthetic_events(sibling=True), arm="sibling")
        require(result.returncode == 0, "complete sibling selection was rejected")
        require("selection=sibling\n" in self.summary_path.read_text(encoding="utf-8"), "sibling selection summary missing")

    def test_missing_gated_observation_is_rejected(self) -> None:
        events = self.synthetic_events()
        events = [
            event
            for event in events
            if not (event.get("Action") == "output" and "PAIR_OBSERVER" in str(event.get("Output", "")) and "\"variant\":\"gated\"" in str(event.get("Output", "")))
        ]
        result = self.run_verifier(events)
        self.expect_rejected(result)

    def test_missing_required_assertion_is_rejected(self) -> None:
        events = self.synthetic_events()
        for event in events:
            output = event.get("Output")
            if isinstance(output, str) and "PAIR_OBSERVER" in output and "\"variant\":\"gated\"" in output:
                record_text = output.split("PAIR_OBSERVER ", 1)[1].strip()
                record = json.loads(record_text)
                record["assertions"].remove("dialog-filters.pair-gated-read")
                event["Output"] = observer_output(record, 600)
        result = self.run_verifier(events)
        self.expect_rejected(result)

    def test_original_restart_assertion_is_retained_on_failure(self) -> None:
        result = self.run_verifier(self.synthetic_events(failure=True), go_status=23)
        require(result.returncode == 23, "nonzero Go exit was not preserved")
        summary = self.summary_path.read_text(encoding="utf-8")
        require("immediate_outcome=failure\n" in summary, "failure record was not extracted")
        require("immediate_safe_error_class=net closed/EOF\n" in summary, "safe error class was not extracted")
        require("dialog-filters.other-session-restart-read-error" in summary, "original assertion was hidden")
        require("PAIR_OBSERVER" not in result.stdout, "raw observer framing reached output")
        require(result.stderr == "", "verifier wrote stderr")

    def test_overflow_is_sanitized_and_marks_observation_inconclusive(self) -> None:
        events = self.synthetic_events(failure=True)
        for event in events:
            output = event.get("Output")
            if isinstance(output, str) and "PAIR_OBSERVER" in output and "\"variant\":\"immediate\"" in output:
                record = json.loads(output.split("PAIR_OBSERVER ", 1)[1].strip())
                record["overflow"] = 1
                event["Output"] = observer_output(record)
                break
        result = self.run_verifier(events, go_status=23)
        require(result.returncode == 23, "overflow changed the Go exit status")
        summary = self.summary_path.read_text(encoding="utf-8")
        require("immediate_overflow=1\n" in summary, "overflow count was not retained")
        require("observation_quality=inconclusive\n" in summary, "overflow was not marked inconclusive")

    def test_failure_record_cannot_produce_a_zero_exit(self) -> None:
        result = self.run_verifier(self.synthetic_events(failure=True), go_status=0)
        self.expect_rejected(result)

    def test_nonzero_go_exit_is_preserved_even_when_stream_is_malformed(self) -> None:
        self.input_path.write_text("malformed PRIVATE_TEST_SENTINEL\n", encoding="utf-8")
        result = subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "verify",
                "--root",
                str(self.repo),
                "--arm",
                "focused",
                "--go-status",
                "23",
                "--workflow-sha",
                "a" * 40,
                "--source-sha",
                pair.SOURCE_SHA,
                "--patch-sha",
                self.patch_sha,
                "--input",
                str(self.input_path),
                "--summary",
                str(self.summary_path),
            ],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            encoding="utf-8",
        )
        require(result.returncode == 23, "malformed output changed the Go exit status")
        require(result.stdout == "observer-rejected\n", "malformed output rejection was not fixed")
        require(result.stderr == "", "malformed output wrote stderr")
        summary = self.summary_path.read_text(encoding="utf-8")
        require(summary == "observer-rejected\n", "malformed output artifact was not fixed")
        require(
            "PRIVATE_TEST_SENTINEL" not in result.stdout + result.stderr + summary,
            "malformed output sentinel escaped containment",
        )

    def test_malformed_extra_and_duplicate_records_are_rejected_without_echo(self) -> None:
        cases: list[tuple[list[dict[str, object]], str]] = []
        malformed = self.synthetic_events()
        malformed.append(test_event("output", "TestSmoke/dialog-filters", "PAIR_OBSERVER {bad json}\n"))
        cases.append((malformed, "bad json"))

        extra_field = self.synthetic_events()
        for event in extra_field:
            output = event.get("Output")
            if isinstance(output, str) and "PAIR_OBSERVER" in output:
                record = json.loads(output.split("PAIR_OBSERVER ", 1)[1].strip())
                record["payload"] = "PRIVATE_FIELD_SENTINEL"
                event["Output"] = observer_output(record)
                break
        cases.append((extra_field, "PRIVATE_FIELD_SENTINEL"))

        duplicate = self.synthetic_events()
        duplicates = [
            event
            for event in duplicate
            if "PAIR_OBSERVER" in str(event.get("Output", ""))
            and "\"variant\":\"immediate\"" in str(event.get("Output", ""))
        ]
        duplicate.extend(duplicates)
        cases.append((duplicate, None))

        for events, sentinel in cases:
            with self.subTest(case="malformed-or-extra"):
                self.expect_rejected(self.run_verifier(events), sentinel)

    def test_unknown_state_and_workflow_command_values_are_rejected(self) -> None:
        for injected_state in ("unknown-state-sentinel", "::error::injected-sentinel"):
            events = self.synthetic_events()
            for event in events:
                output = event.get("Output")
                if isinstance(output, str) and "PAIR_OBSERVER" in output:
                    record = json.loads(output.split("PAIR_OBSERVER ", 1)[1].strip())
                    if record["variant"] == "immediate":
                        record["states"][1]["state"] = injected_state
                        event["Output"] = observer_output(record)
                        break
            self.expect_rejected(self.run_verifier(events), injected_state)

    def test_image_gate_emits_only_closed_results(self) -> None:
        focused = self.temp_root / "focused-image.txt"
        sibling = self.temp_root / "sibling-image.txt"
        focused.write_text("IMAGE_OS_CANARY\nIMAGE_VERSION_CANARY\n", encoding="ascii")
        sibling.write_text("IMAGE_OS_CANARY\nIMAGE_VERSION_CANARY\n", encoding="ascii")
        base_command = [
            sys.executable,
            str(SCRIPT),
            "image-gate",
            "--focused",
            str(focused),
            "--sibling",
            str(sibling),
            "--focused-result",
            "success",
            "--sibling-result",
            "success",
        ]
        matching = subprocess.run(base_command, capture_output=True, text=True, check=False)
        require(matching.returncode == 0 and matching.stdout == "pair-valid\n", "matching image metadata was rejected")
        require("IMAGE_" not in matching.stdout + matching.stderr, "image metadata was emitted")

        sibling.write_text("DIFFERENT_IMAGE\nIMAGE_VERSION_CANARY\n", encoding="ascii")
        mismatch = subprocess.run(base_command, capture_output=True, text=True, check=False)
        require(mismatch.returncode == 1 and mismatch.stdout == "pair-invalid-image-mismatch\n", "mismatched image metadata was accepted")
        require("IMAGE_" not in mismatch.stdout + mismatch.stderr and "DIFFERENT_IMAGE" not in mismatch.stdout + mismatch.stderr, "image value escaped mismatch gate")

        sibling.unlink()
        missing = subprocess.run(base_command, capture_output=True, text=True, check=False)
        require(missing.returncode == 1 and missing.stdout == "pair-invalid-image-missing\n", "missing metadata was not rejected")
        focused.write_text("\nIMAGE_VERSION_CANARY\n", encoding="ascii")
        empty = subprocess.run(base_command, capture_output=True, text=True, check=False)
        require(empty.returncode == 1 and empty.stdout == "pair-invalid-image-missing\n", "empty metadata was not rejected")
        failed_arm = subprocess.run(
            [*base_command[:-1], "failure"], capture_output=True, text=True, check=False
        )
        require(failed_arm.returncode == 1 and failed_arm.stdout == "pair-invalid-image-missing\n", "failed arm was accepted")
        require("IMAGE_" not in missing.stdout + missing.stderr + empty.stdout + empty.stderr + failed_arm.stdout + failed_arm.stderr, "missing metadata value was emitted")

    def test_patch_verification_requires_one_test_only_child_commit(self) -> None:
        with tempfile.TemporaryDirectory(prefix="dialog-filter-pair-patch-") as temp:
            root = Path(temp)
            remote = root / "remote.git"
            work = root / "work"
            checked_out = root / "checkout"
            subprocess.run(["git", "init", "--quiet", "--bare", str(remote)], check=True, capture_output=True)
            subprocess.run(["git", "init", "--quiet", str(work)], check=True, capture_output=True)
            git(work, "config", "user.name", "Synthetic Fixture")
            git(work, "config", "user.email", "fixture@example.invalid")
            (work / ".github/scripts").mkdir(parents=True)
            (work / ".github/scripts/smoke-scenarios.sh").write_bytes(
                (REPO_ROOT / ".github/scripts/smoke-scenarios.sh").read_bytes()
            )
            git(work, "add", ".")
            git(work, "commit", "--quiet", "-m", "synthetic base")
            base_sha = git(work, "rev-parse", "HEAD")
            git(work, "remote", "add", "origin", str(remote))
            git(work, "push", "--quiet", "origin", "HEAD:refs/heads/source")
            subprocess.run(
                ["git", "--git-dir", str(remote), "symbolic-ref", "HEAD", "refs/heads/source"],
                check=True,
                capture_output=True,
            )
            (work / "test/e2e").mkdir(parents=True)
            (work / "test/e2e/observer_test.go").write_text("package e2e\n", encoding="utf-8")
            git(work, "add", "test/e2e/observer_test.go")
            git(work, "commit", "--quiet", "-m", "synthetic observer")
            patch_sha = git(work, "rev-parse", "HEAD")
            git(work, "push", "--quiet", "origin", "HEAD:refs/heads/observer")
            subprocess.run(
                ["git", "clone", "--quiet", str(remote), str(checked_out)],
                check=True,
                capture_output=True,
            )

            original_source_sha = pair.SOURCE_SHA
            try:
                pair.SOURCE_SHA = base_sha
                require(pair.verify_patch(checked_out, base_sha, patch_sha), "valid observer patch was rejected")
            finally:
                pair.SOURCE_SHA = original_source_sha

            subprocess.run(
                ["git", "checkout", "--quiet", "-B", "invalid", base_sha],
                cwd=work,
                check=True,
                capture_output=True,
            )
            (work / "workflow.yml").write_text("name: not a test\n", encoding="utf-8")
            git(work, "add", "workflow.yml")
            git(work, "commit", "--quiet", "-m", "invalid setup change")
            invalid_patch_sha = git(work, "rev-parse", "HEAD")
            git(work, "push", "--quiet", "origin", "HEAD:refs/heads/invalid")
            subprocess.run(
                ["git", "checkout", "--quiet", "--detach", base_sha],
                cwd=checked_out,
                check=True,
                capture_output=True,
            )
            try:
                pair.SOURCE_SHA = base_sha
                require(not pair.verify_patch(checked_out, base_sha, invalid_patch_sha), "non-test patch was accepted")
            finally:
                pair.SOURCE_SHA = original_source_sha


if __name__ == "__main__":
    unittest.main(verbosity=2)
