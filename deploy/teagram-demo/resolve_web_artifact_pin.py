#!/usr/bin/env python3
"""Resolve the current teagram-web publication against its live manifest."""

from __future__ import annotations

import argparse
import json
import math
import pathlib
import re
import sys
import time
import urllib.error
import urllib.request
from typing import Any, Callable


API_ROOT = "https://api.github.com/repos/teagramhq/teagram-web"
PUBLICATION_RUNS_URL = (
    f"{API_ROOT}/actions/workflows/private-artifact.yml/runs"
    "?branch=master&event=push&head_sha={source_commit}&status=success&per_page=100"
)
LIVE_MANIFEST_URL = "https://web.teagram.io/mtproto-target.json"
MAX_FETCH_ATTEMPTS = 3
MAX_RETRY_DELAY_SECONDS = 30
RETRYABLE_HTTP_CODES = {429, 500, 502, 503, 504}


def fetch_json(url: str) -> Any:
    request = urllib.request.Request(
        url,
        headers={
            "Accept": "application/vnd.github+json",
            "User-Agent": "teagram-demo-release-check",
            "X-GitHub-Api-Version": "2022-11-28",
        },
    )
    for attempt in range(1, MAX_FETCH_ATTEMPTS + 1):
        try:
            with urllib.request.urlopen(request, timeout=20) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            last_error: Exception = error
            retry_after = error.headers.get("Retry-After") if error.headers is not None else None
            rate_limited = error.code == 403 and (
                retry_after is not None
                or (
                    error.headers is not None
                    and error.headers.get("X-RateLimit-Remaining") == "0"
                )
            )
            if error.code not in RETRYABLE_HTTP_CODES and not rate_limited:
                raise ValueError(
                    f"upstream metadata request failed for {url}: "
                    f"HTTP {error.code} {error.reason}"
                ) from error
            if retry_after is not None:
                try:
                    parsed_delay = float(retry_after)
                    delay = max(0.0, parsed_delay) if math.isfinite(parsed_delay) else 1.0
                except ValueError:
                    delay = float(2 ** (attempt - 1))
            elif rate_limited and error.headers is not None:
                reset = error.headers.get("X-RateLimit-Reset")
                try:
                    reset_at = float(reset) if reset is not None else time.time() + 1
                    delay = max(0.0, reset_at - time.time()) if math.isfinite(reset_at) else 1.0
                except ValueError:
                    delay = 1.0
            else:
                delay = float(2 ** (attempt - 1))
            detail = f"HTTP {error.code} {error.reason}"
        except (OSError, urllib.error.URLError, json.JSONDecodeError) as error:
            last_error = error
            delay = float(2 ** (attempt - 1))
            detail = str(error)

        if attempt == MAX_FETCH_ATTEMPTS:
            raise ValueError(
                f"upstream metadata request failed for {url} after {attempt} attempts: {detail}"
            ) from last_error
        if delay > MAX_RETRY_DELAY_SECONDS:
            raise ValueError(
                f"upstream metadata request failed for {url}: {detail}; "
                f"retry requested after {delay:.0f}s"
            ) from last_error
        print(
            f"Retrying upstream metadata request for {url} after transient error "
            f"({attempt}/{MAX_FETCH_ATTEMPTS}): {detail}",
            file=sys.stderr,
        )
        time.sleep(delay)

    raise AssertionError("unreachable")


def _require_object(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ValueError(f"{label} response is invalid")
    return value


def _validate_live_manifest(value: Any) -> tuple[str, str]:
    manifest = _require_object(value, "live web manifest")
    source_commit = manifest.get("sourceCommit")
    artifact_digest = manifest.get("artifactDigest")
    if manifest.get("mode") != "private":
        raise ValueError("live web manifest is not a private publication")
    if not isinstance(source_commit, str) or not re.fullmatch(r"[0-9a-f]{40}", source_commit):
        raise ValueError("live web manifest source commit is invalid")
    if not isinstance(artifact_digest, str) or not re.fullmatch(
        r"sha256:[0-9a-f]{64}", artifact_digest
    ):
        raise ValueError("live web manifest artifact digest is invalid")
    return source_commit, artifact_digest


def verify_published_run(
    run_id: int,
    source_commit: str,
    artifact_digest: str,
    *,
    fetch: Callable[[str], Any] = fetch_json,
) -> dict[str, Any]:
    run = _require_object(fetch(f"{API_ROOT}/actions/runs/{run_id}"), "workflow run")
    if (
        run.get("id") != run_id
        or run.get("path") != ".github/workflows/private-artifact.yml"
        or run.get("head_sha") != source_commit
        or run.get("head_branch") != "master"
        or run.get("status") != "completed"
        or run.get("conclusion") != "success"
        or run.get("event") != "push"
        or run.get("pull_requests")
    ):
        raise ValueError("upstream run is not a successful master publication for the live commit")

    artifact_hex = artifact_digest.removeprefix("sha256:")
    artifact_name = f"private-mtproto-{source_commit}-{artifact_hex}"
    artifacts = _require_object(
        fetch(f"{API_ROOT}/actions/runs/{run_id}/artifacts?per_page=100"),
        "publication artifacts",
    )
    artifact_records = artifacts.get("artifacts")
    if not isinstance(artifact_records, list):
        raise ValueError("publication artifacts are invalid")
    if not any(
        isinstance(artifact, dict)
        and artifact.get("name") == artifact_name
        and artifact.get("expired") is False
        for artifact in artifact_records
    ):
        raise ValueError("live web publication has no available matching upstream artifact")

    return {
        "schemaVersion": 1,
        "mode": "published",
        "sourceCommit": source_commit,
        "artifactDigest": artifact_digest,
        "runId": run_id,
        "deployable": True,
    }


def resolve_live_publication(
    *, fetch: Callable[[str], Any] = fetch_json
) -> dict[str, Any]:
    source_commit, artifact_digest = _validate_live_manifest(fetch(LIVE_MANIFEST_URL))
    runs_url = PUBLICATION_RUNS_URL.format(source_commit=source_commit)
    runs = _require_object(fetch(runs_url), "master publication runs")
    records = runs.get("workflow_runs")
    if not isinstance(records, list):
        raise ValueError("master publication runs are invalid")

    candidates = sorted(
        (
            record["run_number"],
            record["id"],
        )
        for record in records
        if isinstance(record, dict)
        and record.get("head_sha") == source_commit
        and type(record.get("run_number")) is int
        and record.get("run_number", 0) > 0
        and type(record.get("id")) is int
        and record.get("id", 0) > 0
    )
    candidates.reverse()
    if not candidates:
        raise ValueError("live web publication has no successful master publication run")

    last_error: ValueError | None = None
    for _, run_id in candidates:
        try:
            resolved = verify_published_run(
                run_id,
                source_commit,
                artifact_digest,
                fetch=fetch,
            )
        except ValueError as error:
            if str(error).startswith("upstream metadata request failed"):
                raise
            last_error = error
            continue

        current_commit, current_digest = _validate_live_manifest(fetch(LIVE_MANIFEST_URL))
        if current_commit != source_commit or current_digest != artifact_digest:
            raise ValueError("live web publication changed while resolving its artifact")
        return resolved

    reason = f": {last_error}" if last_error is not None else ""
    raise ValueError(f"live web publication has no verified master artifact{reason}") from last_error


def validate_release_identity(
    source_commit: str,
    artifact_digest: str,
    *,
    fetch: Callable[[str], Any] = fetch_json,
) -> dict[str, Any]:
    if not re.fullmatch(r"[0-9a-f]{40}", source_commit):
        raise ValueError("web release source commit is invalid")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", artifact_digest):
        raise ValueError("web release artifact digest is invalid")
    resolved = resolve_live_publication(fetch=fetch)
    if (
        resolved["sourceCommit"] != source_commit
        or resolved["artifactDigest"] != artifact_digest
    ):
        raise ValueError("web release identity does not match the live publication")
    return resolved


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--verify-release-identity",
        nargs=2,
        metavar=("SOURCE_COMMIT", "ARTIFACT_DIGEST"),
    )
    args = parser.parse_args()
    try:
        if args.verify_release_identity:
            validate_release_identity(*args.verify_release_identity)
            print("demo web release identity matches the live verified publication")
            return
        print(json.dumps(resolve_live_publication(), separators=(",", ":")))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError) as error:
        print(f"demo web publication rejected: {error}", file=sys.stderr)
        raise SystemExit(1) from error


if __name__ == "__main__":
    main()
