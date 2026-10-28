#!/usr/bin/env python3
"""Exercise upstream identity and rollout gating for the web artifact pin."""

from __future__ import annotations

import importlib.util
import json
import pathlib
import sys
from typing import Any


SCRIPT = pathlib.Path(__file__).with_name("resolve_web_artifact_pin.py")
SPEC = importlib.util.spec_from_file_location("resolve_web_artifact_pin", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise SystemExit("could not load web artifact pin resolver")
MODULE = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


SOURCE_SHA = "1" * 40
ARTIFACT_DIGEST = "sha256:" + "2" * 64
RUN_ID = 123456789
PULL_REQUEST = 121
API_ROOT = MODULE.API_ROOT


def candidate_pin() -> dict[str, Any]:
    return {
        "schemaVersion": 1,
        "mode": "candidate",
        "pullRequest": PULL_REQUEST,
        "runId": RUN_ID,
        "sourceCommit": SOURCE_SHA,
        "artifactDigest": ARTIFACT_DIGEST,
    }


def published_pin() -> dict[str, Any]:
    return {
        "schemaVersion": 1,
        "mode": "published",
        "runId": RUN_ID,
        "sourceCommit": SOURCE_SHA,
        "artifactDigest": ARTIFACT_DIGEST,
    }


def candidate_responses() -> dict[str, Any]:
    return {
        f"{API_ROOT}/actions/runs/{RUN_ID}": {
            "id": RUN_ID,
            "path": ".github/workflows/private-artifact.yml",
            "head_sha": SOURCE_SHA,
            "status": "completed",
            "conclusion": "success",
            "event": "pull_request",
        },
        f"{API_ROOT}/pulls/{PULL_REQUEST}": {
            "state": "open",
            "merged_at": None,
            "head": {"sha": SOURCE_SHA, "repo": {"full_name": "teagramhq/teagram-web"}},
        },
        f"{API_ROOT}/actions/runs/{RUN_ID}/jobs?per_page=100": {
            "jobs": [
                {
                    "id": 987654321,
                    "name": MODULE.PR_VALIDATION_JOB,
                    "head_sha": SOURCE_SHA,
                    "status": "completed",
                    "conclusion": "success",
                }
            ]
        },
    }


def published_responses() -> dict[str, Any]:
    artifact_name = f"private-mtproto-{SOURCE_SHA}-{ARTIFACT_DIGEST.removeprefix('sha256:')}"
    return {
        f"{API_ROOT}/actions/runs/{RUN_ID}": {
            "id": RUN_ID,
            "path": ".github/workflows/private-artifact.yml",
            "head_sha": SOURCE_SHA,
            "head_branch": "master",
            "status": "completed",
            "conclusion": "success",
            "event": "push",
            "pull_requests": [],
        },
        f"{API_ROOT}/actions/runs/{RUN_ID}/artifacts?per_page=100": {
            "artifacts": [{"name": artifact_name, "expired": False}]
        },
        MODULE.LIVE_MANIFEST_URL: {
            "mode": "private",
            "sourceCommit": SOURCE_SHA,
            "artifactDigest": ARTIFACT_DIGEST,
        },
    }


def fake_fetch(responses: dict[str, Any]):
    def fetch(url: str) -> Any:
        if url not in responses:
            raise AssertionError(f"unexpected upstream request: {url}")
        return responses[url]

    return fetch


def main() -> None:
    actual_pin = MODULE.validate_pin_document(
        json.loads(MODULE.PIN_PATH.read_text(encoding="utf-8"))
    )
    if actual_pin["mode"] == "candidate" and actual_pin["pullRequest"] != PULL_REQUEST:
        raise SystemExit("checked-in candidate pin does not reference PR 121")

    validated = MODULE.verify_upstream(
        candidate_pin(),
        fetch=fake_fetch(candidate_responses()),
    )
    if validated["deployable"]:
        raise SystemExit("candidate artifact must not reach the demo deploy")
    if MODULE.resolve_for_deploy(candidate_pin(), fetch=fake_fetch({}))["deployable"]:
        raise SystemExit("deployment preflight must skip a PR candidate pin")

    published = MODULE.verify_upstream(
        published_pin(), fetch=fake_fetch(published_responses()), require_live_manifest=True
    )
    if not published["deployable"]:
        raise SystemExit("matching published master artifact should be deployable")

    MODULE.validate_release_identity(published_pin(), SOURCE_SHA, ARTIFACT_DIGEST)
    for source_commit, artifact_digest, pin, description in (
        ("3" * 40, ARTIFACT_DIGEST, published_pin(), "source commit"),
        (SOURCE_SHA, "sha256:" + "4" * 64, published_pin(), "artifact digest"),
        (SOURCE_SHA, ARTIFACT_DIGEST, candidate_pin(), "candidate mode"),
    ):
        try:
            MODULE.validate_release_identity(pin, source_commit, artifact_digest)
        except ValueError:
            pass
        else:
            raise SystemExit(f"host pin validation accepted a mismatched {description}")

    stale = candidate_responses()
    stale[f"{API_ROOT}/pulls/{PULL_REQUEST}"]["head"]["sha"] = "3" * 40
    try:
        MODULE.verify_upstream(candidate_pin(), fetch=fake_fetch(stale))
    except ValueError:
        pass
    else:
        raise SystemExit("candidate validation accepted a stale PR head")

    missing = published_responses()
    missing[f"{API_ROOT}/actions/runs/{RUN_ID}/artifacts?per_page=100"]["artifacts"] = []
    try:
        MODULE.verify_upstream(published_pin(), fetch=fake_fetch(missing))
    except ValueError:
        pass
    else:
        raise SystemExit("published validation accepted a missing upstream artifact")

    print("demo web artifact pin checks passed")


if __name__ == "__main__":
    main()
