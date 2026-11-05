#!/usr/bin/env python3
"""Resolve the current teagram-web publication against its live manifest."""

from __future__ import annotations

import argparse
import base64
import binascii
import hashlib
import json
import math
import pathlib
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
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
MAX_JSON_BYTES = 1024 * 1024
ALLOWED_HOSTS = {"api.github.com", "web.teagram.io"}


def validate_request_url(url: str) -> None:
    try:
        parsed = urllib.parse.urlsplit(url)
        valid = (
            parsed.scheme == "https"
            and parsed.hostname in ALLOWED_HOSTS
            and parsed.port in (None, 443)
            and parsed.username is None
            and parsed.password is None
        )
    except ValueError as error:
        raise ValueError("invalid publication metadata URL") from error
    if not valid:
        raise ValueError("publication metadata URL is not an allowed HTTPS endpoint")


class PublicationRedirectHandler(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, headers, new_url):
        validate_request_url(new_url)
        return super().redirect_request(request, response, code, message, headers, new_url)


HTTP_OPENER = urllib.request.build_opener(PublicationRedirectHandler())


def fetch_json(url: str) -> Any:
    validate_request_url(url)
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
            with HTTP_OPENER.open(request, timeout=20) as response:
                body = bytearray()
                while len(body) <= MAX_JSON_BYTES:
                    chunk = response.read(min(65536, MAX_JSON_BYTES + 1 - len(body)))
                    if not chunk:
                        break
                    body.extend(chunk)
                if len(body) > MAX_JSON_BYTES:
                    raise ValueError("publication metadata response exceeds 1 MiB")
                return json.loads(body)
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


def fetch_repo_file(path: str, source_commit: str, *, fetch: Callable[[str], Any]) -> bytes:
    parts = pathlib.PurePosixPath(path)
    if (
        not re.fullmatch(r"[A-Za-z0-9._/-]+", path)
        or parts.is_absolute()
        or any(part in (".", "..") for part in path.split("/"))
        or "//" in path
    ):
        raise ValueError("reviewed key path is invalid")
    obj = _require_object(fetch(f"{API_ROOT}/contents/{path}?ref={source_commit}"), "reviewed file")
    content = obj.get("content")
    if obj.get("type") != "file" or obj.get("encoding") != "base64" or not isinstance(content, str):
        raise ValueError("reviewed file response is invalid")
    try:
        decoded = base64.b64decode("".join(content.split()), validate=True)
    except (binascii.Error, ValueError) as error:
        raise ValueError("reviewed file encoding is invalid") from error
    if type(obj.get("size")) is not int or obj["size"] != len(decoded) or len(decoded) > 16 * 1024:
        raise ValueError("reviewed file size is invalid")
    return decoded


def public_key_fingerprint(pem: bytes) -> str:
    try:
        result = subprocess.run(
            ["openssl", "rsa", "-pubin", "-RSAPublicKey_out", "-outform", "DER"],
            input=pem, capture_output=True, timeout=10, check=True,
        )
    except (OSError, subprocess.SubprocessError) as error:
        raise ValueError("reviewed RSA public key is invalid") from error

    def der_value(data: bytes, offset: int, expected_tag: int) -> tuple[bytes, int]:
        if offset + 2 > len(data) or data[offset] != expected_tag:
            raise ValueError("reviewed RSA public key DER is invalid")
        length = data[offset + 1]
        offset += 2
        if length & 0x80:
            count = length & 0x7f
            if count == 0 or count > 4 or offset + count > len(data) or data[offset] == 0:
                raise ValueError("reviewed RSA public key DER length is invalid")
            length = int.from_bytes(data[offset:offset + count], "big")
            offset += count
            if length < 128:
                raise ValueError("reviewed RSA public key DER length is not canonical")
        if offset + length > len(data):
            raise ValueError("reviewed RSA public key DER is truncated")
        return data[offset:offset + length], offset + length

    sequence, end = der_value(result.stdout, 0, 0x30)
    if end != len(result.stdout):
        raise ValueError("reviewed RSA public key DER has trailing data")
    modulus_bytes, end = der_value(sequence, 0, 0x02)
    exponent_bytes, end = der_value(sequence, end, 0x02)
    if end != len(sequence) or any(
        not value or value[0] & 0x80 or (len(value) > 1 and value[0] == 0 and value[1] < 0x80)
        for value in (modulus_bytes, exponent_bytes)
    ):
        raise ValueError("reviewed RSA public key integers are invalid")
    modulus = int.from_bytes(modulus_bytes, "big")
    exponent = int.from_bytes(exponent_bytes, "big")
    if modulus.bit_length() != 2048 or modulus % 2 == 0 or exponent != 65537:
        raise ValueError("reviewed RSA public key parameters are invalid")

    def tl_bytes(value: int) -> bytes:
        data = value.to_bytes((value.bit_length() + 7) // 8, "big")
        prefix = bytes([len(data)]) if len(data) < 254 else b"\xfe" + len(data).to_bytes(3, "little")
        encoded = prefix + data
        return encoded + b"\0" * (-len(encoded) % 4)

    # Telegram defines the RSA fingerprint using the last eight SHA-1 bytes.
    digest = hashlib.sha1(tl_bytes(modulus) + tl_bytes(exponent)).digest()
    return f"{int.from_bytes(digest[-8:], 'little'):016x}"


def fetch_reviewed_target(
    source_commit: str, *, fetch: Callable[[str], Any] = fetch_json
) -> tuple[str, str]:
    if not re.fullmatch(r"[0-9a-f]{40}", source_commit):
        raise ValueError("reviewed target source commit is invalid")
    raw = fetch_repo_file("ci/private-mtproto-target.json", source_commit, fetch=fetch)
    target = _require_object(json.loads(raw), "reviewed target")
    if set(target) != {
        "MTPROTO_TARGET_MODE", "MTPROTO_PRIVATE_ENDPOINT", "MTPROTO_PRIVATE_RSA_PUBLIC_KEY_FILE",
        "publicKeySha256", "fingerprint",
    }:
        raise ValueError("reviewed target fields are invalid")
    endpoint = target["MTPROTO_PRIVATE_ENDPOINT"]
    fingerprint = target["fingerprint"]
    key_path = target["MTPROTO_PRIVATE_RSA_PUBLIC_KEY_FILE"]
    if (target["MTPROTO_TARGET_MODE"] != "private" or not isinstance(endpoint, str)
        or not re.fullmatch(r"wss://[^/\s]+(?:/[^\s]*)?", endpoint)
        or not isinstance(fingerprint, str) or not re.fullmatch(r"[0-9a-f]{16}", fingerprint)
        or not isinstance(key_path, str)):
        raise ValueError("reviewed target identity is invalid")
    key = fetch_repo_file(key_path, source_commit, fetch=fetch)
    if hashlib.sha256(key).hexdigest() != target["publicKeySha256"]:
        raise ValueError("reviewed public key digest does not match")
    if public_key_fingerprint(key) != fingerprint:
        raise ValueError("reviewed public key fingerprint does not match")
    return endpoint, fingerprint


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
        "deployable": False,
    }


def resolve_published_identity(
    source_commit: str, artifact_digest: str, *, fetch: Callable[[str], Any] = fetch_json
) -> dict[str, Any]:
    if not re.fullmatch(r"[0-9a-f]{40}", source_commit):
        raise ValueError("publication source commit is invalid")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", artifact_digest):
        raise ValueError("publication artifact digest is invalid")
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

        return resolved

    reason = f": {last_error}" if last_error is not None else ""
    raise ValueError(f"live web publication has no verified master artifact{reason}") from last_error


def resolve_live_publication(
    *, fetch: Callable[[str], Any] = fetch_json
) -> dict[str, Any]:
    source_commit, artifact_digest = _validate_live_manifest(fetch(LIVE_MANIFEST_URL))
    result = resolve_published_identity(source_commit, artifact_digest, fetch=fetch)
    current_commit, current_digest = _validate_live_manifest(fetch(LIVE_MANIFEST_URL))
    if current_commit != source_commit or current_digest != artifact_digest:
        raise ValueError("live web publication changed while resolving its artifact")
    result["deployable"] = True
    return result


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


def verify_rollback_authorization(
    run_id: int, server_sha: str, publication_run_id: int, *, fetch: Callable[[str], Any] = fetch_json
) -> None:
    if (type(run_id) is not int or run_id < 1 or type(publication_run_id) is not int
        or publication_run_id < 1 or not re.fullmatch(r"[0-9a-f]{40}", server_sha)):
        raise ValueError("rollback request identity is invalid")
    run = _require_object(
        fetch(f"https://api.github.com/repos/teagramhq/teagram-server/actions/runs/{run_id}"),
        "rollback workflow run",
    )
    repository = _require_object(run.get("repository"), "rollback repository")
    title = run.get("display_title")
    if (
        run.get("id") != run_id or run.get("event") != "workflow_dispatch"
        or run.get("status") != "in_progress" or run.get("head_branch") != "main"
        or run.get("head_sha") != server_sha or run.get("name") != "Deploy demo."
        or run.get("path") != ".github/workflows/deploy-demo.yml"
        or repository.get("full_name") != "teagramhq/teagram-server"
        or not isinstance(title, str)
        or not re.fullmatch(rf"Deploy demo\. \[MAIN-[0-9]+\] \[web-rollback={publication_run_id}\]", title)
    ):
        raise ValueError("web rollback requires the explicit main workflow dispatch")
    jobs = _require_object(
        fetch(f"https://api.github.com/repos/teagramhq/teagram-server/actions/runs/{run_id}/jobs?per_page=100"),
        "rollback workflow jobs",
    ).get("jobs")
    if not isinstance(jobs, list):
        raise ValueError("rollback workflow jobs are invalid")
    checks = [job for job in jobs if isinstance(job, dict) and job.get("name") == "check: server-deploy-result"]
    if len(checks) != 1 or checks[0].get("status") != "completed" or checks[0].get("conclusion") != "success":
        raise ValueError("rollback dispatch did not pass issue and main-ref validation")


def main() -> None:
    parser = argparse.ArgumentParser()
    group = parser.add_mutually_exclusive_group()
    group.add_argument(
        "--verify-release-identity",
        nargs=2,
        metavar=("SOURCE_COMMIT", "ARTIFACT_DIGEST"),
    )
    group.add_argument(
        "--verify-published-identity",
        nargs=2,
        metavar=("SOURCE_COMMIT", "ARTIFACT_DIGEST"),
    )
    group.add_argument("--verify-rollback-request", nargs=3, metavar=("RUN_ID", "SERVER_SHA", "PUBLICATION_RUN_ID"))
    args = parser.parse_args()
    try:
        if args.verify_rollback_request:
            run_id, server_sha, publication_run_id = args.verify_rollback_request
            if not re.fullmatch(r"[1-9][0-9]*", run_id) or not re.fullmatch(r"[1-9][0-9]*", publication_run_id):
                raise ValueError("rollback run ID is invalid")
            verify_rollback_authorization(int(run_id), server_sha, int(publication_run_id))
            print("web rollback dispatch authorized")
            return
        if args.verify_published_identity:
            print(json.dumps(resolve_published_identity(*args.verify_published_identity), separators=(",", ":")))
            return
        if args.verify_release_identity:
            result = validate_release_identity(*args.verify_release_identity)
            endpoint, fingerprint = fetch_reviewed_target(result["sourceCommit"])
            if _validate_live_manifest(fetch_json(LIVE_MANIFEST_URL)) != (result["sourceCommit"], result["artifactDigest"]):
                raise ValueError("live web publication changed while verifying its target")
            result.update(endpoint=endpoint, fingerprint=fingerprint)
            print(json.dumps(result, separators=(",", ":")))
            return
        print(json.dumps(resolve_live_publication(), separators=(",", ":")))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError) as error:
        print(f"demo web publication rejected: {error}", file=sys.stderr)
        raise SystemExit(1) from error


if __name__ == "__main__":
    main()
