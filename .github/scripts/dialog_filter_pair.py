#!/usr/bin/env python3
"""Validate the one-use paired dialog-filter smoke run without exposing raw output."""

from __future__ import annotations

import argparse
import json
import math
import re
import subprocess
import sys
from pathlib import Path
from typing import Any


SOURCE_SHA = "3f42239ce8391b4bf88b3dfb2fffbc34752d664e"
PACKAGE = "github.com/teagramhq/teagram-server/test/e2e"
CLEANUP_TEST = "TestSmokeFixtureRestartKeepsClientCleanupAheadOfServerStop"
PAIR_PREFIX = "PAIR_OBSERVER "
PAIR_OUTPUT = re.compile(
    r"^ +[A-Za-z0-9_.-]+\.go:[1-9][0-9]{0,5}: PAIR_OBSERVER (?P<record>\{.*\})$"
)
ASSERTION_MARKER = re.compile(r"\[assert:([^\]\r\n]+)\]")
SHA = re.compile(r"[0-9a-f]{40}\Z")
STATE_NAMES = {"connecting", "ready", "disconnected"}
EVENT_ACTIONS = {
    "start",
    "run",
    "pause",
    "cont",
    "output",
    "pass",
    "bench",
    "fail",
    "skip",
    "build-output",
    "build-fail",
}
EVENT_FIELDS = {
    "Time",
    "Action",
    "Package",
    "Test",
    "Output",
    "Elapsed",
    "FailedBuild",
    "ImportPath",
}
COMMON_RECORD_FIELDS = {
    "schema",
    "variant",
    "states",
    "restartMark",
    "readIssueSeq",
    "readReturnSeq",
    "replacementReadySeq",
    "overflow",
    "acceptCount",
    "assertions",
    "safeErrorClass",
    "outcome",
}
GATED_RECORD_FIELDS = COMMON_RECORD_FIELDS | {"authorized"}
STATE_FIELDS = {"seq", "state"}
EXISTING_DIALOG_FILTER_ASSERTIONS = {
    "dialog-filters.app-config-duplicate-enabled",
    "dialog-filters.app-config-enabled",
    "dialog-filters.app-config-enabled-type",
    "dialog-filters.app-config-fetch-error",
    "dialog-filters.app-config-object-type",
    "dialog-filters.create-error",
    "dialog-filters.created-count-tags",
    "dialog-filters.created-list-error",
    "dialog-filters.created-value",
    "dialog-filters.defaults-initialize-error",
    "dialog-filters.delete-error",
    "dialog-filters.deleted-count",
    "dialog-filters.deleted-list-error",
    "dialog-filters.edit-error",
    "dialog-filters.edited-list-error",
    "dialog-filters.edited-value",
    "dialog-filters.initial-default-all-type",
    "dialog-filters.initial-default-count",
    "dialog-filters.initial-default-order",
    "dialog-filters.other-owner-all-type",
    "dialog-filters.other-owner-count-tags",
    "dialog-filters.other-owner-list-error",
    "dialog-filters.other-owner-personal",
    "dialog-filters.other-owner-restart-count",
    "dialog-filters.other-owner-restart-personal",
    "dialog-filters.other-owner-restart-read-error",
    "dialog-filters.other-session-restart-count",
    "dialog-filters.other-session-restart-read-error",
    "dialog-filters.other-session-restart-value",
    "dialog-filters.reorder-error",
    "dialog-filters.reordered-first",
    "dialog-filters.reordered-list-error",
    "dialog-filters.repeat-default-all-type",
    "dialog-filters.repeat-default-count",
    "dialog-filters.repeat-default-order",
    "dialog-filters.repeat-default-read-error",
    "dialog-filters.suggested-count",
    "dialog-filters.suggested-list-error",
}
PAIR_ASSERTIONS = {
    "dialog-filters.pair-immediate-read",
    "dialog-filters.pair-gated-replacement-ready",
    "dialog-filters.pair-gated-auth-status",
    "dialog-filters.pair-gated-read",
    "dialog-filters.pair-observer-overflow",
    "dialog-filters.pair-observer-order",
}
ALLOWED_ASSERTIONS = EXISTING_DIALOG_FILTER_ASSERTIONS | PAIR_ASSERTIONS
REQUIRED_ASSERTIONS = {
    "immediate": {
        "dialog-filters.pair-immediate-read",
        "dialog-filters.pair-observer-overflow",
        "dialog-filters.pair-observer-order",
    },
    "gated": {
        "dialog-filters.pair-gated-replacement-ready",
        "dialog-filters.pair-observer-overflow",
        "dialog-filters.pair-observer-order",
    },
}
SAFE_ERROR_LITERALS = {
    "unexpected nil",
    "injected probe",
    "deadline",
    "canceled",
    "net closed/EOF",
}
RPC_ERROR = re.compile(r"rpc\(code=-?[0-9]{1,9},type=[A-Z0-9_]{1,64}\)\Z")
OTHER_ERROR = re.compile(r"other\(\*?[A-Za-z0-9_./\[\]-]{1,160}\)\Z")
METADATA_VALUE = re.compile(rb"[A-Za-z0-9._-]{1,128}\Z")
SUMMARY_PREFIX_FIELDS = (
    "workflow_sha",
    "source_sha",
    "observer_patch_sha",
    "arm",
    "go_exit_status",
    "selection",
    "test_smoke_dialog_filters",
    "cleanup_order_test",
    "observation_quality",
)
SUMMARY_RECORD_FIELDS = (
    "outcome",
    "states",
    "restart_mark",
    "read_issue_seq",
    "read_return_seq",
    "replacement_ready_seq",
    "overflow",
    "accept_count",
    "authorized",
    "assertions",
    "safe_error_class",
)
SUMMARY_FIELD_ORDER = SUMMARY_PREFIX_FIELDS + tuple(
    f"{variant}_{field}"
    for variant in ("immediate", "gated")
    for field in SUMMARY_RECORD_FIELDS
) + ("visible_assertions",)
SUMMARY_UNSIGNED = re.compile(r"(?:0|[1-9][0-9]{0,8})\Z")
SUMMARY_STATE = re.compile(r"([1-9][0-9]{0,8}):(connecting|ready|disconnected)\Z")


class InvalidInput(Exception):
    """An input failed validation; its contents must not be reported."""


class SafeArgumentParser(argparse.ArgumentParser):
    def error(self, _message: str) -> None:
        raise InvalidInput

    def exit(self, status: int = 0, _message: str | None = None) -> None:
        if status != 0:
            raise InvalidInput
        raise InvalidInput


def strict_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise InvalidInput
        result[key] = value
    return result


def reject_json_constant(_value: str) -> None:
    raise InvalidInput


def parse_json(value: str) -> Any:
    return json.loads(
        value,
        object_pairs_hook=strict_object,
        parse_constant=reject_json_constant,
    )


def valid_int(value: Any, *, minimum: int = 0, maximum: int = 999_999_999) -> bool:
    return type(value) is int and minimum <= value <= maximum


def run_git(root: Path, *args: str) -> subprocess.CompletedProcess[bytes]:
    return subprocess.run(
        ["git", "-C", str(root), *args],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )


def clean_git_tree(root: Path) -> bool:
    result = run_git(root, "status", "--porcelain", "--untracked-files=all")
    return result.returncode == 0 and not result.stdout


def verify_patch(source: Path, source_sha: str, patch_sha: str) -> bool:
    if source_sha != SOURCE_SHA or SHA.fullmatch(patch_sha) is None:
        return False
    head = run_git(source, "rev-parse", "--verify", "HEAD^{commit}")
    if head.returncode != 0 or head.stdout.decode("ascii", "strict").strip() != source_sha:
        return False

    fetched = run_git(source, "fetch", "--quiet", "--no-tags", "origin", patch_sha)
    if fetched.returncode != 0:
        return False

    parents = run_git(source, "rev-list", "--parents", "-n", "1", patch_sha)
    if parents.returncode != 0:
        return False
    parent_fields = parents.stdout.decode("ascii", "strict").split()
    if parent_fields != [patch_sha, source_sha]:
        return False

    changed = run_git(
        source,
        "diff-tree",
        "--no-commit-id",
        "--name-only",
        "-r",
        "-z",
        patch_sha,
    )
    if changed.returncode != 0:
        return False
    try:
        paths = [item.decode("utf-8", "strict") for item in changed.stdout.split(b"\0") if item]
    except UnicodeDecodeError:
        return False
    if not paths or any(not path.startswith("test/e2e/") for path in paths):
        return False

    checked_out = run_git(source, "checkout", "--quiet", "--detach", patch_sha)
    if checked_out.returncode != 0:
        return False
    checked_head = run_git(source, "rev-parse", "--verify", "HEAD^{commit}")
    return (
        checked_head.returncode == 0
        and checked_head.stdout.decode("ascii", "strict").strip() == patch_sha
        and clean_git_tree(source)
    )


def smoke_scenarios(root: Path) -> tuple[str, ...]:
    script = (root / ".github/scripts/smoke-scenarios.sh").read_text(encoding="utf-8")
    match = re.search(r"(?ms)^SMOKE_SCENARIOS=\(\s*(.*?)\s*\)", script)
    if match is None:
        raise InvalidInput
    scenarios = tuple(re.findall(r"[a-z0-9-]+", match.group(1)))
    if not scenarios or len(set(scenarios)) != len(scenarios):
        raise InvalidInput
    if "dialog-filters" not in scenarios:
        raise InvalidInput
    return scenarios


def validate_event(event: Any) -> tuple[str, str, str] | None:
    if not isinstance(event, dict) or not set(event).issubset(EVENT_FIELDS):
        raise InvalidInput
    action = event.get("Action")
    if not isinstance(action, str) or action not in EVENT_ACTIONS:
        raise InvalidInput
    for field in ("Time", "Test", "Output", "FailedBuild", "ImportPath"):
        if field in event and not isinstance(event[field], str):
            raise InvalidInput
    if "Elapsed" in event and (
        type(event["Elapsed"]) not in {int, float}
        or not math.isfinite(event["Elapsed"])
        or event["Elapsed"] < 0
    ):
        raise InvalidInput

    if action in {"build-output", "build-fail"}:
        if "Package" in event and event["Package"] != PACKAGE:
            raise InvalidInput
        if "ImportPath" in event and event["ImportPath"] != PACKAGE:
            raise InvalidInput
        return None

    if event.get("Package") != PACKAGE:
        raise InvalidInput
    return action, event.get("Test", ""), event.get("Output", "")


def parse_safe_error(value: Any) -> str | None:
    if value is None:
        return None
    if not isinstance(value, str):
        raise InvalidInput
    if value in SAFE_ERROR_LITERALS or RPC_ERROR.fullmatch(value) or OTHER_ERROR.fullmatch(value):
        return value
    raise InvalidInput


def replacement_ready(states: list[dict[str, Any]], restart_mark: int) -> int | None:
    saw_disconnected = False
    for item in states:
        if item["seq"] > restart_mark and item["state"] == "disconnected":
            saw_disconnected = True
        elif saw_disconnected and item["state"] == "ready":
            return item["seq"]
    return None


def validate_record(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict) or "variant" not in value:
        raise InvalidInput
    variant = value["variant"]
    if variant not in {"immediate", "gated"}:
        raise InvalidInput
    expected_fields = GATED_RECORD_FIELDS if variant == "gated" else COMMON_RECORD_FIELDS
    if set(value) != expected_fields or type(value["schema"]) is not int or value["schema"] != 1:
        raise InvalidInput

    states_value = value["states"]
    if not isinstance(states_value, list) or not states_value:
        raise InvalidInput
    states: list[dict[str, Any]] = []
    previous_sequence = 0
    for item in states_value:
        if not isinstance(item, dict) or set(item) != STATE_FIELDS:
            raise InvalidInput
        sequence = item["seq"]
        state = item["state"]
        if not valid_int(sequence, minimum=1) or sequence <= previous_sequence:
            raise InvalidInput
        if not isinstance(state, str) or state not in STATE_NAMES:
            raise InvalidInput
        states.append({"seq": sequence, "state": state})
        previous_sequence = sequence

    restart_mark = value["restartMark"]
    overflow = value["overflow"]
    accept_count = value["acceptCount"]
    if not valid_int(restart_mark) or not valid_int(overflow) or not valid_int(accept_count):
        raise InvalidInput
    if overflow == 0 and restart_mark > states[-1]["seq"]:
        raise InvalidInput

    issue_sequence = value["readIssueSeq"]
    return_sequence = value["readReturnSeq"]
    ready_sequence = value["replacementReadySeq"]
    for sequence in (issue_sequence, return_sequence, ready_sequence):
        if sequence is not None and not valid_int(sequence, minimum=1):
            raise InvalidInput
    if (issue_sequence is None) != (return_sequence is None):
        raise InvalidInput
    if issue_sequence is not None and issue_sequence > return_sequence:
        raise InvalidInput
    if ready_sequence != replacement_ready(states, restart_mark):
        raise InvalidInput

    authorized = value.get("authorized")
    if variant == "immediate":
        if issue_sequence is None:
            raise InvalidInput
    elif type(authorized) is not bool:
        raise InvalidInput
    if variant == "gated":
        if authorized is True:
            if ready_sequence is None or issue_sequence is None or issue_sequence < ready_sequence:
                raise InvalidInput
        elif issue_sequence is not None or return_sequence is not None:
            raise InvalidInput

    assertions = value["assertions"]
    if not isinstance(assertions, list) or not assertions:
        raise InvalidInput
    if any(not isinstance(item, str) or item not in ALLOWED_ASSERTIONS for item in assertions):
        raise InvalidInput
    if len(assertions) != len(set(assertions)):
        raise InvalidInput
    if not REQUIRED_ASSERTIONS[variant].issubset(assertions):
        raise InvalidInput
    if variant == "gated" and ready_sequence is not None and "dialog-filters.pair-gated-auth-status" not in assertions:
        raise InvalidInput
    if variant == "gated" and authorized is True and "dialog-filters.pair-gated-read" not in assertions:
        raise InvalidInput

    outcome = value["outcome"]
    if outcome not in {"pass", "failure"}:
        raise InvalidInput
    if outcome == "pass" and (overflow != 0 or (variant == "gated" and authorized is not True)):
        raise InvalidInput
    safe_error = parse_safe_error(value["safeErrorClass"])
    return {
        "variant": variant,
        "states": states,
        "restart_mark": restart_mark,
        "read_issue_seq": issue_sequence,
        "read_return_seq": return_sequence,
        "replacement_ready_seq": ready_sequence,
        "overflow": overflow,
        "accept_count": accept_count,
        "authorized": authorized,
        "assertions": assertions,
        "safe_error_class": safe_error,
        "outcome": outcome,
    }


def selection_state(
    root: Path, arm: str, events: list[tuple[str, str, str]]
) -> tuple[dict[str, str], set[str]]:
    scenarios = smoke_scenarios(root)
    runs: dict[str, int] = {}
    terminal: dict[str, str] = {}
    for action, test_name, _output in events:
        if action == "run" and test_name:
            runs[test_name] = runs.get(test_name, 0) + 1
            if runs[test_name] > 1:
                raise InvalidInput
        elif action in {"pass", "fail", "skip"} and test_name:
            if test_name in terminal:
                raise InvalidInput
            terminal[test_name] = action

    for test_name in runs:
        if test_name not in terminal:
            raise InvalidInput
    if "TestSmoke" not in runs or terminal.get("TestSmoke") not in {"pass", "fail"}:
        raise InvalidInput
    required = {"TestSmoke", "TestSmoke/dialog-filters"}
    if arm == "sibling":
        required.update(f"TestSmoke/{scenario}" for scenario in scenarios)
    if not required.issubset(runs):
        raise InvalidInput
    if any(terminal.get(test_name) == "skip" for test_name in required):
        raise InvalidInput

    scenario_set = set(scenarios)
    observed_test_names = {test_name for _action, test_name, _output in events if test_name}
    for test_name in observed_test_names:
        if test_name == "TestSmoke":
            continue
        if test_name == CLEANUP_TEST:
            continue
        if test_name.startswith("TestSmoke/"):
            suffix = test_name[len("TestSmoke/") :]
            scenario, _separator, _nested = suffix.partition("/")
            if scenario not in scenario_set:
                raise InvalidInput
            if arm == "focused" and scenario != "dialog-filters":
                raise InvalidInput
            continue
        raise InvalidInput
    if set(terminal) - set(runs):
        raise InvalidInput

    cleanup = terminal.get(CLEANUP_TEST)
    summary = {
        "selection": arm,
        "dialog_filters": terminal["TestSmoke/dialog-filters"],
        "cleanup_order": cleanup or "not-selected",
    }
    return summary, set(runs)


def validate_stream(path: Path, root: Path, arm: str) -> tuple[dict[str, Any], set[str]]:
    events: list[tuple[str, str, str]] = []
    records: dict[str, dict[str, Any]] = {}
    visible_assertions: set[str] = set()
    with path.open("r", encoding="utf-8", errors="strict") as stream:
        for raw_line in stream:
            if not raw_line.strip():
                continue
            event = validate_event(parse_json(raw_line))
            if event is None:
                continue
            action, test_name, output = event
            events.append(event)
            if action != "output":
                continue
            for line in output.splitlines():
                for marker in ASSERTION_MARKER.findall(line):
                    if marker.startswith("dialog-filters."):
                        if marker not in ALLOWED_ASSERTIONS:
                            raise InvalidInput
                        visible_assertions.add(marker)
                if PAIR_PREFIX not in line:
                    continue
                match = PAIR_OUTPUT.fullmatch(line)
                if match is None:
                    raise InvalidInput
                if test_name != "TestSmoke/dialog-filters" and not test_name.startswith(
                    "TestSmoke/dialog-filters/"
                ):
                    raise InvalidInput
                record = validate_record(parse_json(match.group("record")))
                variant = record["variant"]
                if variant in records:
                    raise InvalidInput
                records[variant] = record

    if set(records) != {"immediate", "gated"}:
        raise InvalidInput
    selection, _selected = selection_state(root, arm, events)
    assertions = set(visible_assertions)
    for record in records.values():
        assertions.update(record["assertions"])
    selection["records"] = records
    selection["assertions"] = assertions
    return selection, assertions


def verify(args: argparse.Namespace) -> int:
    if args.arm not in {"focused", "sibling"} or not valid_int(args.go_status, maximum=255):
        raise InvalidInput
    if any(SHA.fullmatch(value) is None for value in (args.workflow_sha, args.source_sha, args.patch_sha)):
        raise InvalidInput
    if args.source_sha != SOURCE_SHA:
        raise InvalidInput
    root = Path(args.root)
    head = run_git(root, "rev-parse", "--verify", "HEAD^{commit}")
    if head.returncode != 0 or head.stdout.decode("ascii", "strict").strip() != args.patch_sha:
        raise InvalidInput
    if not clean_git_tree(root):
        raise InvalidInput

    selection, _assertions = validate_stream(Path(args.input), root, args.arm)
    records = selection.pop("records")
    assertions = selection.pop("assertions")
    if args.go_status == 0 and any(record["outcome"] == "failure" for record in records.values()):
        raise InvalidInput
    observation_quality = (
        "inconclusive"
        if any(record["overflow"] > 0 for record in records.values())
        or records["gated"]["replacement_ready_seq"] is None
        else "complete"
    )
    lines = [
        f"workflow_sha={args.workflow_sha}",
        f"source_sha={args.source_sha}",
        f"observer_patch_sha={args.patch_sha}",
        f"arm={args.arm}",
        f"go_exit_status={args.go_status}",
        f"selection={selection['selection']}",
        f"test_smoke_dialog_filters={selection['dialog_filters']}",
        f"cleanup_order_test={selection['cleanup_order']}",
        f"observation_quality={observation_quality}",
    ]
    for variant in ("immediate", "gated"):
        record = records[variant]
        lines.extend(
            (
                f"{variant}_outcome={record['outcome']}",
                f"{variant}_states="
                + ",".join(f"{item['seq']}:{item['state']}" for item in record["states"]),
                f"{variant}_restart_mark={record['restart_mark']}",
                f"{variant}_read_issue_seq={record['read_issue_seq'] if record['read_issue_seq'] is not None else 'none'}",
                f"{variant}_read_return_seq={record['read_return_seq'] if record['read_return_seq'] is not None else 'none'}",
                f"{variant}_replacement_ready_seq={record['replacement_ready_seq'] if record['replacement_ready_seq'] is not None else 'none'}",
                f"{variant}_overflow={record['overflow']}",
                f"{variant}_accept_count={record['accept_count']}",
                f"{variant}_authorized={str(record['authorized']).lower() if record['authorized'] is not None else 'not-checked'}",
                f"{variant}_assertions=" + ",".join(sorted(record["assertions"])),
                f"{variant}_safe_error_class={record['safe_error_class'] or 'none'}",
            )
        )
    lines.append("visible_assertions=" + ",".join(sorted(assertions)))
    summary = "\n".join(lines) + "\n"
    Path(args.summary).write_text(summary, encoding="utf-8")
    sys.stdout.write(summary)
    return args.go_status


def parse_args(argv: list[str]) -> argparse.Namespace:
    if not argv:
        raise InvalidInput
    parser = SafeArgumentParser(add_help=False)
    subparsers = parser.add_subparsers(dest="command", required=True, parser_class=SafeArgumentParser)

    verify_parser = subparsers.add_parser("verify", add_help=False)
    verify_parser.add_argument("--root", required=True)
    verify_parser.add_argument("--arm", required=True)
    verify_parser.add_argument("--go-status", required=True, type=int)
    verify_parser.add_argument("--workflow-sha", required=True)
    verify_parser.add_argument("--source-sha", required=True)
    verify_parser.add_argument("--patch-sha", required=True)
    verify_parser.add_argument("--input", required=True)
    verify_parser.add_argument("--summary", required=True)

    patch_parser = subparsers.add_parser("verify-patch", add_help=False)
    patch_parser.add_argument("--source", required=True)
    patch_parser.add_argument("--source-sha", required=True)
    patch_parser.add_argument("--patch-sha", required=True)

    gate_parser = subparsers.add_parser("image-gate", add_help=False)
    gate_parser.add_argument("--focused", required=True)
    gate_parser.add_argument("--sibling", required=True)

    summary_parser = subparsers.add_parser("summary-gate", add_help=False)
    summary_parser.add_argument("--focused", required=True)
    summary_parser.add_argument("--sibling", required=True)
    summary_parser.add_argument("--workflow-sha", required=True)
    summary_parser.add_argument("--source-sha", required=True)
    summary_parser.add_argument("--patch-sha", required=True)
    return parser.parse_args(argv)


def read_metadata(path: str) -> tuple[bytes, bytes] | None:
    try:
        raw = Path(path).read_bytes()
    except OSError:
        return None
    if raw.count(b"\n") != 2 or not raw.endswith(b"\n"):
        return None
    first, second, _empty = raw.split(b"\n")
    if METADATA_VALUE.fullmatch(first) is None or METADATA_VALUE.fullmatch(second) is None:
        return None
    return first, second


def image_gate(args: argparse.Namespace) -> int:
    focused = read_metadata(args.focused)
    sibling = read_metadata(args.sibling)
    if focused is None or sibling is None:
        print("pair-invalid-image-missing")
        return 1
    if focused != sibling:
        print("pair-invalid-image-mismatch")
        return 1
    print("pair-valid")
    return 0


def summary_int(value: str, *, minimum: int = 0) -> int | None:
    if SUMMARY_UNSIGNED.fullmatch(value) is None:
        return None
    parsed = int(value)
    if parsed < minimum or parsed > 999_999_999:
        return None
    return parsed


def valid_summary_states(value: str) -> bool:
    encoded_states = value.split(",")
    if not encoded_states or any(not item for item in encoded_states):
        return False
    previous = 0
    for encoded in encoded_states:
        match = SUMMARY_STATE.fullmatch(encoded)
        if match is None:
            return False
        sequence = int(match.group(1))
        if sequence <= previous:
            return False
        previous = sequence
    return True


def valid_summary_assertions(value: str, *, required: set[str] | None = None) -> bool:
    assertions = value.split(",") if value else []
    return (
        assertions == sorted(set(assertions))
        and all(assertion in ALLOWED_ASSERTIONS for assertion in assertions)
        and (required is None or required.issubset(assertions))
    )


def read_summary(
    path: str,
    *,
    arm: str,
    workflow_sha: str,
    source_sha: str,
    patch_sha: str,
) -> bool:
    try:
        raw = Path(path).read_bytes()
    except OSError:
        return False
    if not raw or len(raw) > 65_536 or b"\r" in raw or not raw.endswith(b"\n"):
        return False
    try:
        lines = raw.decode("ascii", "strict").splitlines()
    except UnicodeDecodeError:
        return False
    if len(lines) != len(SUMMARY_FIELD_ORDER):
        return False

    values: dict[str, str] = {}
    for expected_field, line in zip(SUMMARY_FIELD_ORDER, lines, strict=True):
        field, separator, value = line.partition("=")
        if separator != "=" or field != expected_field or "=" in value:
            return False
        values[field] = value

    if any(SHA.fullmatch(value) is None for value in (workflow_sha, source_sha, patch_sha)):
        return False
    if (
        values["workflow_sha"] != workflow_sha
        or values["source_sha"] != source_sha
        or values["observer_patch_sha"] != patch_sha
        or values["arm"] != arm
        or values["selection"] != arm
    ):
        return False
    go_status = values["go_exit_status"]
    if re.fullmatch(r"(?:0|[1-9][0-9]{0,2})", go_status) is None or int(go_status) > 255:
        return False
    if values["test_smoke_dialog_filters"] not in {"pass", "fail"}:
        return False
    if values["cleanup_order_test"] not in {"pass", "fail", "not-selected"}:
        return False
    if values["observation_quality"] not in {"complete", "inconclusive"}:
        return False

    has_failure = values["test_smoke_dialog_filters"] == "fail" or values["cleanup_order_test"] == "fail"
    for variant in ("immediate", "gated"):
        prefix = f"{variant}_"
        if values[prefix + "outcome"] not in {"pass", "failure"}:
            return False
        has_failure = has_failure or values[prefix + "outcome"] == "failure"
        if not valid_summary_states(values[prefix + "states"]):
            return False
        if summary_int(values[prefix + "restart_mark"]) is None:
            return False
        issue_sequence = values[prefix + "read_issue_seq"]
        return_sequence = values[prefix + "read_return_seq"]
        for sequence in (issue_sequence, return_sequence, values[prefix + "replacement_ready_seq"]):
            if sequence != "none" and summary_int(sequence, minimum=1) is None:
                return False
        if (issue_sequence == "none") != (return_sequence == "none"):
            return False
        if issue_sequence != "none" and int(issue_sequence) > int(return_sequence):
            return False
        if summary_int(values[prefix + "overflow"]) is None:
            return False
        if summary_int(values[prefix + "accept_count"]) is None:
            return False
        authorized = values[prefix + "authorized"]
        if variant == "immediate":
            if issue_sequence == "none" or authorized != "not-checked":
                return False
        elif authorized not in {"true", "false"}:
            return False
        if not valid_summary_assertions(
            values[prefix + "assertions"], required=REQUIRED_ASSERTIONS[variant]
        ):
            return False
        safe_error = values[prefix + "safe_error_class"]
        if safe_error != "none" and parse_safe_error(safe_error) != safe_error:
            return False

    if not valid_summary_assertions(values["visible_assertions"]):
        return False
    return not has_failure or int(go_status) != 0


def summary_gate(args: argparse.Namespace) -> int:
    if args.source_sha != SOURCE_SHA or any(
        SHA.fullmatch(value) is None
        for value in (args.workflow_sha, args.source_sha, args.patch_sha)
    ):
        print("pair-invalid-summary")
        return 1
    if not read_summary(
        args.focused,
        arm="focused",
        workflow_sha=args.workflow_sha,
        source_sha=args.source_sha,
        patch_sha=args.patch_sha,
    ) or not read_summary(
        args.sibling,
        arm="sibling",
        workflow_sha=args.workflow_sha,
        source_sha=args.source_sha,
        patch_sha=args.patch_sha,
    ):
        print("pair-invalid-summary")
        return 1
    print("pair-valid-summaries")
    return 0


def fallback_status(argv: list[str]) -> int:
    for index, value in enumerate(argv):
        if value == "--go-status" and index + 1 < len(argv):
            candidate = argv[index + 1]
            if candidate.isdecimal():
                status = int(candidate)
                if 0 <= status <= 255:
                    return status if status != 0 else 1
    return 1


def write_rejected_summary(argv: list[str]) -> None:
    for index, value in enumerate(argv):
        if value == "--summary" and index + 1 < len(argv):
            try:
                Path(argv[index + 1]).write_text("observer-rejected\n", encoding="utf-8")
            except OSError:
                pass
            return


def main(argv: list[str] | None = None) -> int:
    arguments = list(sys.argv[1:] if argv is None else argv)
    command = arguments[0] if arguments else ""
    try:
        args = parse_args(arguments)
        if args.command == "verify":
            return verify(args)
        if args.command == "verify-patch":
            if verify_patch(Path(args.source), args.source_sha, args.patch_sha):
                return 0
            print("activation-rejected")
            return 1
        if args.command == "image-gate":
            return image_gate(args)
        if args.command == "summary-gate":
            return summary_gate(args)
        raise InvalidInput
    except BaseException:
        if command == "image-gate":
            print("pair-invalid-image-missing")
            return 1
        if command == "summary-gate":
            print("pair-invalid-summary")
            return 1
        if command == "verify-patch":
            print("activation-rejected")
            return 1
        write_rejected_summary(arguments)
        print("observer-rejected")
        return fallback_status(arguments)


if __name__ == "__main__":
    raise SystemExit(main())
