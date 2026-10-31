#!/usr/bin/env python3
"""Verify the pinned teagram-web artifact against its upstream CI run."""

from __future__ import annotations

import argparse
import json
import pathlib
import re
import sys
import time
import urllib.error
import urllib.request
from typing import Any, Callable


API_ROOT = "https://api.github.com/repos/teagramhq/teagram-web"
LIVE_MANIFEST_URL = "https://web.teagram.io/mtproto-target.json"
PIN_PATH = pathlib.Path(__file__).with_name("web-artifact-pin.json")
PIN_REPO = "teagramhq/teagram-web"
PR_VALIDATION_JOB = "Validate private artifact on pull request"
FETCH_MAX_ATTEMPTS = 3
FETCH_RETRY_DELAYS = (1.0, 2.0)


def fetch_json(
    url: str,
    *,
    open_url: Callable[..., Any] = urllib.request.urlopen,
    sleep: Callable[[float], None] = time.sleep,
) -> Any:
    request = urllib.request.Request(
        url,
        headers={
            "Accept": "application/vnd.github+json",
            "User-Agent": "teagram-demo-release-check",
            "X-GitHub-Api-Version": "2022-11-28",
        },
    )
    last_error: Exception | None = None
    for attempt in range(FETCH_MAX_ATTEMPTS):
        try:
            with open_url(request, timeout=20) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            last_error = error
            headers = error.headers or {}
            rate_limited = error.code == 403 and (
                headers.get("Retry-After") is not None
                or headers.get("X-RateLimit-Remaining") == "0"
            )
            retryable = (
                error.code in (408, 425, 429)
                or 500 <= error.code <= 599
                or rate_limited
            )
            if not retryable:
                raise ValueError(f"upstream metadata request returned HTTP {error.code}") from error
        except (OSError, urllib.error.URLError, json.JSONDecodeError) as error:
            last_error = error

        if attempt + 1 < FETCH_MAX_ATTEMPTS:
            delay = FETCH_RETRY_DELAYS[attempt]
            if isinstance(last_error, urllib.error.HTTPError):
                retry_after = (last_error.headers or {}).get("Retry-After")
                if retry_after is not None:
                    try:
                        delay = min(max(float(retry_after), 0.0), 30.0)
                    except ValueError:
                        pass
            sleep(delay)

    raise ValueError(
        f"upstream metadata is unavailable after {FETCH_MAX_ATTEMPTS} attempts"
    ) from last_error


def validate_pin_document(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ValueError("pin must be an object")
    mode = value.get("mode")
    fields = (
        {"schemaVersion", "mode", "pullRequest", "runId", "sourceCommit", "artifactDigest"}
        if mode == "candidate"
        else {"schemaVersion", "mode", "runId", "sourceCommit", "artifactDigest"}
        if mode == "published"
        else set()
    )
    if not fields or set(value) != fields:
        raise ValueError("pin fields or mode are invalid")
    if type(value["schemaVersion"]) is not int or value["schemaVersion"] != 1:
        raise ValueError("pin schema version is invalid")
    source_commit = value.get("sourceCommit")
    artifact_digest = value.get("artifactDigest")
    run_id = value.get("runId")
    if not isinstance(source_commit, str) or not re.fullmatch(r"[0-9a-f]{40}", source_commit):
        raise ValueError("source commit must be a full lowercase SHA")
    if not isinstance(artifact_digest, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", artifact_digest):
        raise ValueError("artifact digest must be a full lowercase sha256")
    if type(run_id) is not int or run_id < 1:
        raise ValueError("upstream run ID is invalid")
    if mode == "candidate" and (type(value["pullRequest"]) is not int or value["pullRequest"] != 121):
        raise ValueError("candidate pin must reference teagram-web PR 121")
    return dict(value)


def _require_object(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ValueError(f"{label} response is invalid")
    return value


def verify_upstream(
    pin: dict[str, Any],
    *,
    fetch: Callable[[str], Any] = fetch_json,
    require_live_manifest: bool = False,
) -> dict[str, Any]:
    source_commit = pin["sourceCommit"]
    artifact_digest = pin["artifactDigest"]
    run_id = pin["runId"]
    run = _require_object(fetch(f"{API_ROOT}/actions/runs/{run_id}"), "workflow run")
    if (
        run.get("id") != run_id
        or run.get("path") != ".github/workflows/private-artifact.yml"
        or run.get("head_sha") != source_commit
        or run.get("status") != "completed"
        or run.get("conclusion") != "success"
    ):
        raise ValueError("pinned upstream run does not verify this completed artifact")

    if pin["mode"] == "candidate":
        if run.get("event") != "pull_request":
            raise ValueError("candidate pin run is not a pull-request validation")
        pull_request = _require_object(
            fetch(f"{API_ROOT}/pulls/{pin['pullRequest']}"), "pull request"
        )
        head = _require_object(pull_request.get("head"), "pull-request head")
        head_repo = _require_object(head.get("repo"), "pull-request head repository")
        merged = pull_request.get("merged_at") is not None
        if (
            pull_request.get("state") not in ("open", "closed")
            or (pull_request.get("state") == "closed" and not merged)
            or head.get("sha") != source_commit
            or head_repo.get("full_name") != PIN_REPO
        ):
            raise ValueError("candidate commit is no longer the reviewed PR 121 head")
        jobs = _require_object(
            fetch(f"{API_ROOT}/actions/runs/{run_id}/jobs?per_page=100"), "workflow jobs"
        )
        job_records = jobs.get("jobs")
        if not isinstance(job_records, list):
            raise ValueError("PR 121 validation jobs are invalid")
        validation_jobs = [
            job
            for job in job_records
            if isinstance(job, dict)
            and job.get("name") == PR_VALIDATION_JOB
            and job.get("head_sha") == source_commit
            and job.get("status") == "completed"
            and job.get("conclusion") == "success"
        ]
        if len(validation_jobs) != 1:
            raise ValueError("PR 121 has no unique successful private-artifact validation job")
        deployable = False
    else:
        if (
            run.get("event") != "push"
            or run.get("head_branch") != "master"
            or run.get("pull_requests")
        ):
            raise ValueError("published pin run is not a master publication")
        artifact_hex = artifact_digest.removeprefix("sha256:")
        artifact_name = f"private-mtproto-{source_commit}-{artifact_hex}"
        artifacts = _require_object(
            fetch(f"{API_ROOT}/actions/runs/{run_id}/artifacts?per_page=100"),
            "publication artifacts",
        )
        if not any(
            isinstance(artifact, dict)
            and artifact.get("name") == artifact_name
            and artifact.get("expired") is False
            for artifact in artifacts.get("artifacts", [])
        ):
            raise ValueError("published pin has no available matching upstream artifact")
        deployable = False
        if require_live_manifest:
            manifest = _require_object(fetch(LIVE_MANIFEST_URL), "live web manifest")
            if (
                manifest.get("mode") != "private"
                or manifest.get("sourceCommit") != source_commit
                or manifest.get("artifactDigest") != artifact_digest
            ):
                raise ValueError("live web publication does not match the pinned artifact")
            deployable = True

    return {
        "schemaVersion": 1,
        "mode": pin["mode"],
        "sourceCommit": source_commit,
        "artifactDigest": artifact_digest,
        "runId": run_id,
        "deployable": deployable,
    }


def resolve_for_deploy(
    pin: dict[str, Any], *, fetch: Callable[[str], Any] = fetch_json
) -> dict[str, Any]:
    if pin["mode"] == "candidate":
        return {
            "schemaVersion": 1,
            "mode": pin["mode"],
            "sourceCommit": pin["sourceCommit"],
            "artifactDigest": pin["artifactDigest"],
            "runId": pin["runId"],
            "deployable": False,
        }
    return verify_upstream(pin, fetch=fetch, require_live_manifest=True)


def validate_release_identity(
    pin: dict[str, Any], source_commit: str, artifact_digest: str
) -> dict[str, Any]:
    validated_pin = validate_pin_document(pin)
    if not re.fullmatch(r"[0-9a-f]{40}", source_commit):
        raise ValueError("web release source commit is invalid")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", artifact_digest):
        raise ValueError("web release artifact digest is invalid")
    if validated_pin["mode"] != "published":
        raise ValueError("only a published web pin can be deployed")
    if (
        validated_pin["sourceCommit"] != source_commit
        or validated_pin["artifactDigest"] != artifact_digest
    ):
        raise ValueError("web release identity does not match the checked-in pin")
    return validated_pin


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--require-live-manifest", action="store_true")
    parser.add_argument(
        "--verify-release-identity",
        nargs=2,
        metavar=("SOURCE_COMMIT", "ARTIFACT_DIGEST"),
    )
    args = parser.parse_args()
    try:
        pin = validate_pin_document(json.loads(PIN_PATH.read_text(encoding="utf-8")))
        if args.verify_release_identity:
            validate_release_identity(pin, *args.verify_release_identity)
            print("demo web release identity matches the published pin")
            return
        result = (
            resolve_for_deploy(pin)
            if args.require_live_manifest
            else verify_upstream(pin)
        )
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError) as error:
        print(f"demo web pin rejected: {error}", file=sys.stderr)
        raise SystemExit(1) from error
    print(json.dumps(result, separators=(",", ":")))


if __name__ == "__main__":
    main()
