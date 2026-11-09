#!/usr/bin/env python3
"""Exercise live teagram-web publication resolution and rollout gating."""

from __future__ import annotations

import base64
import hashlib
import importlib.util
import io
import json
import pathlib
import sys
from typing import Any
from unittest.mock import patch
from urllib.error import HTTPError, URLError


SCRIPT = pathlib.Path(__file__).with_name("resolve_web_artifact_pin.py")
SPEC = importlib.util.spec_from_file_location("resolve_web_artifact_pin", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise SystemExit("could not load web publication resolver")
MODULE = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


OLD_SOURCE_SHA = "1" * 40
OLD_ARTIFACT_DIGEST = "sha256:" + "2" * 64
SOURCE_SHA = "3" * 40
ARTIFACT_DIGEST = "sha256:" + "4" * 64
RUN_ID = 123456789
API_ROOT = MODULE.API_ROOT
RUNS_URL = MODULE.PUBLICATION_RUNS_URL.format(source_commit=SOURCE_SHA)


def published_responses() -> dict[str, Any]:
    artifact_name = f"private-mtproto-{SOURCE_SHA}-{ARTIFACT_DIGEST.removeprefix('sha256:')}"
    return {
        MODULE.LIVE_MANIFEST_URL: {
            "mode": "private",
            "sourceCommit": SOURCE_SHA,
            "artifactDigest": ARTIFACT_DIGEST,
        },
        RUNS_URL: {
            "workflow_runs": [
                {
                    "id": RUN_ID,
                    "run_number": 23,
                    "path": ".github/workflows/private-artifact.yml",
                    "head_sha": SOURCE_SHA,
                    "head_branch": "master",
                    "event": "push",
                    "status": "completed",
                    "conclusion": "success",
                    "pull_requests": [],
                }
            ]
        },
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
    }


def fake_fetch(responses: dict[str, Any]):
    def fetch(url: str) -> Any:
        if url not in responses:
            raise AssertionError(f"unexpected upstream request: {url}")
        return responses[url]

    return fetch


def check_fetch_retries_transient_errors() -> None:
    url = f"{API_ROOT}/actions/runs/{RUN_ID}"
    rate_limit_headers = {"Retry-After": "0"}
    with (
        patch.object(
            MODULE.HTTP_OPENER,
            "open",
            side_effect=[
                URLError("temporary connection reset"),
                HTTPError(url, 429, "Too Many Requests", rate_limit_headers, io.BytesIO()),
                io.BytesIO(b'{"ok":true}'),
            ],
        ) as open_url,
        patch.object(MODULE.time, "sleep") as sleep,
    ):
        if MODULE.fetch_json(url) != {"ok": True}:
            raise SystemExit("transient upstream rate limit did not recover")
        if open_url.call_count != 3:
            raise SystemExit("transient upstream errors were not retried")
        if [call.args[0] for call in sleep.call_args_list] != [1.0, 0.0]:
            raise SystemExit("upstream retry delays were not respected")


def check_fetch_uses_github_token_when_configured() -> None:
    url = f"{API_ROOT}/actions/runs/{RUN_ID}"
    with (
        patch.dict(MODULE.os.environ, {"GITHUB_TOKEN": "ghs_fixture_token"}),
        patch.object(
            MODULE.HTTP_OPENER, "open", return_value=io.BytesIO(b'{"ok":true}')
        ) as open_url,
    ):
        if MODULE.fetch_json(url) != {"ok": True}:
            raise SystemExit("authenticated upstream request did not return its response")
        request = open_url.call_args.args[0]
        if request.get_header("Authorization") != "Bearer ghs_fixture_token":
            raise SystemExit("configured GitHub token was not sent as a bearer token")

    with (
        patch.dict(MODULE.os.environ, {"GITHUB_TOKEN": "ghs_fixture_token"}),
        patch.object(
            MODULE.HTTP_OPENER, "open", return_value=io.BytesIO(b'{"ok":true}')
        ) as open_url,
    ):
        MODULE.fetch_json(MODULE.LIVE_MANIFEST_URL)
        request = open_url.call_args.args[0]
        if request.get_header("Authorization") is not None:
            raise SystemExit("GitHub token was sent to the live web origin")


def check_fetch_retries_secondary_rate_limit_reset() -> None:
    url = f"{API_ROOT}/actions/runs/{RUN_ID}"
    headers = {"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1005"}
    with (
        patch.object(
            MODULE.HTTP_OPENER,
            "open",
            side_effect=[
                HTTPError(url, 403, "Forbidden", headers, io.BytesIO()),
                io.BytesIO(b'{"ok":true}'),
            ],
        ) as open_url,
        patch.object(MODULE.time, "time", return_value=1000.0),
        patch.object(MODULE.time, "sleep") as sleep,
    ):
        if MODULE.fetch_json(url) != {"ok": True}:
            raise SystemExit("GitHub secondary rate limit did not recover")
        if open_url.call_count != 2:
            raise SystemExit("GitHub secondary rate limit was not retried")
        if [call.args[0] for call in sleep.call_args_list] != [5.0]:
            raise SystemExit("X-RateLimit-Reset delay was not respected")


def check_fetch_aborts_excessive_secondary_rate_limit_delay() -> None:
    url = f"{API_ROOT}/actions/runs/{RUN_ID}"
    headers = {"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1031"}
    with (
        patch.object(
            MODULE.HTTP_OPENER,
            "open",
            side_effect=HTTPError(url, 403, "Forbidden", headers, io.BytesIO()),
        ) as open_url,
        patch.object(MODULE.time, "time", return_value=1000.0),
        patch.object(MODULE.time, "sleep") as sleep,
    ):
        try:
            MODULE.fetch_json(url)
        except ValueError as error:
            if "retry requested after 31s" not in str(error):
                raise SystemExit("excessive rate-limit delay must explain why retry stopped")
        else:
            raise SystemExit("rate-limit delay above MAX_RETRY_DELAY_SECONDS was retried")
        if open_url.call_count != 1 or sleep.called:
            raise SystemExit("excessive rate-limit delay retried before it was safe")


def check_fetch_exhausts_secondary_rate_limit_attempts() -> None:
    url = f"{API_ROOT}/actions/runs/{RUN_ID}"
    headers = {"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1000"}
    errors = [
        HTTPError(url, 403, "Forbidden", headers, io.BytesIO())
        for _ in range(MODULE.MAX_FETCH_ATTEMPTS)
    ]
    with (
        patch.object(MODULE.HTTP_OPENER, "open", side_effect=errors) as open_url,
        patch.object(MODULE.time, "time", return_value=1000.0),
        patch.object(MODULE.time, "sleep") as sleep,
    ):
        try:
            MODULE.fetch_json(url)
        except ValueError as error:
            if f"after {MODULE.MAX_FETCH_ATTEMPTS} attempts" not in str(error):
                raise SystemExit("exhausted retries must report the attempt count")
        else:
            raise SystemExit("secondary rate-limit retry exhaustion unexpectedly passed")
        if open_url.call_count != MODULE.MAX_FETCH_ATTEMPTS:
            raise SystemExit("secondary rate-limit retry exhaustion used the wrong attempt count")
        if [call.args[0] for call in sleep.call_args_list] != [0.0, 0.0]:
            raise SystemExit("secondary rate-limit retries did not stop after the final attempt")


def check_fetch_errors_are_actionable() -> None:
    url = f"{API_ROOT}/actions/runs/{RUN_ID}"
    with patch.object(
        MODULE.HTTP_OPENER,
        "open",
        side_effect=HTTPError(url, 404, "Not Found", {}, io.BytesIO()),
    ) as open_url:
        try:
            MODULE.fetch_json(url)
        except ValueError as error:
            if url not in str(error) or "HTTP 404 Not Found" not in str(error):
                raise SystemExit("upstream HTTP failures must identify URL and status")
        else:
            raise SystemExit("upstream 404 unexpectedly passed")
        if open_url.call_count != 1:
            raise SystemExit("permanent upstream HTTP failures must not be retried")


def check_untrusted_redirect_is_rejected() -> None:
    handler_type = getattr(
        MODULE, "PublicationRedirectHandler", MODULE.urllib.request.HTTPRedirectHandler
    )
    handler = handler_type()
    request = MODULE.urllib.request.Request(MODULE.LIVE_MANIFEST_URL)
    for target in (
        "http://127.0.0.1:8080/",
        "https://attacker.example/",
        "http://api.github.com/",
        "https://api.github.com.attacker.example/",
        "https://user@api.github.com/",
        "https://api.github.com:8443/",
    ):
        try:
            handler.redirect_request(request, None, 302, "Found", {}, target)
        except ValueError:
            continue
        raise SystemExit(f"resolver followed an unsafe redirect: {target}")
    for target in ("https://api.github.com/test", "https://web.teagram.io/test"):
        if handler.redirect_request(request, None, 302, "Found", {}, target) is None:
            raise SystemExit(f"resolver rejected an allowed HTTPS redirect: {target}")
    authenticated_request = MODULE.urllib.request.Request(
        f"{API_ROOT}/actions/runs/{RUN_ID}",
        headers={"Authorization": "Bearer ghs_fixture_token"},
    )
    redirected = handler.redirect_request(
        authenticated_request, None, 302, "Found", {}, "https://web.teagram.io/test"
    )
    if redirected is not None and redirected.get_header("Authorization") is not None:
        raise SystemExit("resolver forwarded GitHub credentials across hosts")
    with patch.object(MODULE.HTTP_OPENER, "open") as open_url:
        try:
            MODULE.fetch_json("https://attacker.example/")
        except ValueError:
            pass
        else:
            raise SystemExit("resolver accepted an unsafe initial URL")
        if open_url.called:
            raise SystemExit("resolver requested an unsafe initial URL")


def check_oversized_response_is_rejected() -> None:
    payload = b'{"value":"' + b'a' * (1024 * 1024) + b'"}'
    with patch.object(MODULE.HTTP_OPENER, "open", return_value=io.BytesIO(payload)):
        try:
            MODULE.fetch_json(MODULE.LIVE_MANIFEST_URL)
        except ValueError:
            pass
        else:
            raise SystemExit("resolver accepted a JSON response larger than 1 MiB")
    limit_payload = b'"' + b'a' * (MODULE.MAX_JSON_BYTES - 2) + b'"'
    with patch.object(MODULE.HTTP_OPENER, "open", return_value=io.BytesIO(limit_payload)):
        if len(MODULE.fetch_json(MODULE.LIVE_MANIFEST_URL)) != MODULE.MAX_JSON_BYTES - 2:
            raise SystemExit("resolver rejected JSON at the size limit")
    class FragmentedResponse(io.BytesIO):
        def read(self, size=-1):
            return super().read(min(size, 4))

    fragmented = FragmentedResponse(b"null" + b" " * MODULE.MAX_JSON_BYTES)
    with patch.object(MODULE.HTTP_OPENER, "open", return_value=fragmented):
        try:
            MODULE.fetch_json(MODULE.LIVE_MANIFEST_URL)
        except ValueError:
            return
    raise SystemExit("resolver accepted an oversized fragmented JSON response")


def check_reviewed_target_and_key() -> None:
    key = (SCRIPT.parent / "testdata/reviewed-web-public.txt").read_bytes()
    target = {
        "MTPROTO_TARGET_MODE": "private",
        "MTPROTO_PRIVATE_ENDPOINT": "wss://teagram.tailaa4918.ts.net/apiws",
        "MTPROTO_PRIVATE_RSA_PUBLIC_KEY_FILE": "ci/private-mtproto-server-public.pem",
        "publicKeySha256": hashlib.sha256(key).hexdigest(),
        "fingerprint": "fbb62871f07fae2a",
    }

    def file_response(content: bytes) -> dict[str, Any]:
        return {"type": "file", "encoding": "base64", "size": len(content),
                "content": base64.b64encode(content).decode("ascii")}

    target_url = f"{API_ROOT}/contents/ci/private-mtproto-target.json?ref={SOURCE_SHA}"
    key_url = f"{API_ROOT}/contents/ci/private-mtproto-server-public.pem?ref={SOURCE_SHA}"
    responses = {target_url: file_response(json.dumps(target).encode()), key_url: file_response(key)}
    reviewed = MODULE.fetch_reviewed_target(SOURCE_SHA, fetch=fake_fetch(responses))
    if reviewed != (target["MTPROTO_PRIVATE_ENDPOINT"], target["fingerprint"]):
        raise SystemExit("reviewed endpoint and key fingerprint were not verified")
    for field, value in (("fingerprint", "d" * 16), ("publicKeySha256", "e" * 64),
                         ("MTPROTO_PRIVATE_ENDPOINT", "http://web.example/apiws"),
                         ("MTPROTO_PRIVATE_RSA_PUBLIC_KEY_FILE", "../outside.pem")):
        modified = dict(target)
        modified[field] = value
        bad = dict(responses)
        bad[target_url] = file_response(json.dumps(modified).encode())
        try:
            MODULE.fetch_reviewed_target(SOURCE_SHA, fetch=fake_fetch(bad))
        except ValueError:
            continue
        raise SystemExit(f"resolver accepted an invalid reviewed {field}")
    bad_key = b"not a public key"
    modified = dict(target, publicKeySha256=hashlib.sha256(bad_key).hexdigest())
    bad = {target_url: file_response(json.dumps(modified).encode()), key_url: file_response(bad_key)}
    try:
        MODULE.fetch_reviewed_target(SOURCE_SHA, fetch=fake_fetch(bad))
    except ValueError:
        return
    raise SystemExit("resolver accepted a non-RSA key with a matching file hash")


def check_new_live_publication_resolves_without_pin_edit() -> None:
    pin_path = pathlib.Path(__file__).with_name("web-artifact-pin.json")
    if pin_path.exists():
        raise SystemExit("live publication resolution must not depend on a checked-in pin")

    resolved = MODULE.resolve_live_publication(fetch=fake_fetch(published_responses()))
    if (
        resolved["sourceCommit"] != SOURCE_SHA
        or resolved["artifactDigest"] != ARTIFACT_DIGEST
        or resolved["runId"] != RUN_ID
        or not resolved["deployable"]
    ):
        raise SystemExit("a newer live publication did not resolve to its verified artifact")

    MODULE.validate_release_identity(
        SOURCE_SHA,
        ARTIFACT_DIGEST,
        fetch=fake_fetch(published_responses()),
    )
    try:
        MODULE.validate_release_identity(
            OLD_SOURCE_SHA,
            OLD_ARTIFACT_DIGEST,
            fetch=fake_fetch(published_responses()),
        )
    except ValueError:
        pass
    else:
        raise SystemExit("host identity check accepted the stale web publication")


def check_pinned_release_identity_survives_concurrent_publication() -> None:
    responses = published_responses()
    responses[MODULE.LIVE_MANIFEST_URL] = {
        "mode": "private",
        "sourceCommit": "5" * 40,
        "artifactDigest": "sha256:" + "6" * 64,
    }
    key = (SCRIPT.parent / "testdata/reviewed-web-public.txt").read_bytes()
    target = {
        "MTPROTO_TARGET_MODE": "private",
        "MTPROTO_PRIVATE_ENDPOINT": "wss://teagram.tailaa4918.ts.net/apiws",
        "MTPROTO_PRIVATE_RSA_PUBLIC_KEY_FILE": "ci/private-mtproto-server-public.pem",
        "publicKeySha256": hashlib.sha256(key).hexdigest(),
        "fingerprint": "fbb62871f07fae2a",
    }

    def file_response(content: bytes) -> dict[str, Any]:
        return {"type": "file", "encoding": "base64", "size": len(content),
                "content": base64.b64encode(content).decode("ascii")}

    target_url = f"{API_ROOT}/contents/ci/private-mtproto-target.json?ref={SOURCE_SHA}"
    key_url = f"{API_ROOT}/contents/ci/private-mtproto-server-public.pem?ref={SOURCE_SHA}"
    responses[target_url] = file_response(json.dumps(target).encode())
    responses[key_url] = file_response(key)
    pinned = MODULE.validate_pinned_release_identity(
        SOURCE_SHA,
        ARTIFACT_DIGEST,
        fetch=fake_fetch(responses),
    )
    if (
        pinned["sourceCommit"] != SOURCE_SHA
        or pinned["artifactDigest"] != ARTIFACT_DIGEST
        or pinned["endpoint"] != target["MTPROTO_PRIVATE_ENDPOINT"]
        or pinned["fingerprint"] != target["fingerprint"]
    ):
        raise SystemExit("verified publication snapshot changed when the live publication advanced")


def check_existing_publication_baseline() -> None:
    responses = published_responses()
    responses[MODULE.LIVE_MANIFEST_URL] = {
        "mode": "private", "sourceCommit": "5" * 40, "artifactDigest": "sha256:" + "6" * 64,
    }
    existing = MODULE.resolve_published_identity(SOURCE_SHA, ARTIFACT_DIGEST, fetch=fake_fetch(responses))
    if existing["runId"] != RUN_ID or existing["deployable"]:
        raise SystemExit("the installed publication did not resolve its own verified run")


def check_rollback_requires_explicit_dispatch() -> None:
    url = f"https://api.github.com/repos/teagramhq/teagram-server/actions/runs/{RUN_ID}"
    publication_run_id = 987654321
    run = {
        "id": RUN_ID, "event": "workflow_dispatch", "status": "in_progress",
        "head_branch": "main", "head_sha": SOURCE_SHA,
        "name": "Deploy demo.", "path": ".github/workflows/deploy-demo.yml",
        "display_title": f"Deploy demo. [MAIN-3333] [web-rollback={publication_run_id}]",
        "repository": {"full_name": "teagramhq/teagram-server"},
    }
    jobs_url = url + "/jobs?per_page=100"
    jobs = {"jobs": [{"name": "check: server-deploy-result", "status": "completed", "conclusion": "success"}]}
    MODULE.verify_rollback_authorization(RUN_ID, SOURCE_SHA, publication_run_id, fetch=fake_fetch({url: run, jobs_url: jobs}))
    for field, value in (("event", "workflow_run"), ("status", "completed"),
                         ("head_sha", "4" * 40), ("path", ".github/workflows/ci.yml"),
                         ("head_branch", "other"), ("id", RUN_ID + 1),
                         ("name", "Other"), ("repository", {"full_name": "attacker/server"}),
                         ("display_title", "Deploy demo. [MAIN-3333]")):
        bad = dict(run)
        bad[field] = value
        try:
            MODULE.verify_rollback_authorization(RUN_ID, SOURCE_SHA, publication_run_id, fetch=fake_fetch({url: bad, jobs_url: jobs}))
        except ValueError:
            continue
        raise SystemExit(f"rollback accepted invalid {field}")
    bad_jobs = {"jobs": [{"name": "check: server-deploy-result", "status": "completed", "conclusion": "failure"}]}
    try:
        MODULE.verify_rollback_authorization(RUN_ID, SOURCE_SHA, publication_run_id, fetch=fake_fetch({url: run, jobs_url: bad_jobs}))
    except ValueError:
        pass
    else:
        raise SystemExit("rollback accepted a dispatch whose issue validation failed")
    try:
        MODULE.verify_rollback_authorization(RUN_ID, SOURCE_SHA, publication_run_id + 1, fetch=fake_fetch({url: run, jobs_url: jobs}))
    except ValueError:
        return
    raise SystemExit("rollback accepted a dispatch for a different publication run")


def check_live_publication_fails_closed() -> None:
    malformed = published_responses()
    malformed[MODULE.LIVE_MANIFEST_URL] = {
        "mode": "private",
        "sourceCommit": "not-a-full-sha",
        "artifactDigest": ARTIFACT_DIGEST,
    }
    try:
        MODULE.resolve_live_publication(fetch=fake_fetch(malformed))
    except ValueError:
        pass
    else:
        raise SystemExit("resolver accepted a live manifest without a full source SHA")

    wrong_run = published_responses()
    wrong_run[f"{API_ROOT}/actions/runs/{RUN_ID}"]["event"] = "pull_request"
    try:
        MODULE.resolve_live_publication(fetch=fake_fetch(wrong_run))
    except ValueError:
        pass
    else:
        raise SystemExit("resolver accepted an artifact from a pull-request run")

    missing = published_responses()
    missing[f"{API_ROOT}/actions/runs/{RUN_ID}/artifacts?per_page=100"]["artifacts"] = []
    try:
        MODULE.resolve_live_publication(fetch=fake_fetch(missing))
    except ValueError:
        pass
    else:
        raise SystemExit("resolver accepted a live publication without its matching artifact")

    malformed_artifacts = published_responses()
    malformed_artifacts[
        f"{API_ROOT}/actions/runs/{RUN_ID}/artifacts?per_page=100"
    ]["artifacts"] = None
    try:
        MODULE.resolve_live_publication(fetch=fake_fetch(malformed_artifacts))
    except ValueError:
        pass
    else:
        raise SystemExit("resolver accepted malformed publication artifact metadata")

    changing = published_responses()
    changed_manifest = {
        "mode": "private",
        "sourceCommit": "5" * 40,
        "artifactDigest": "sha256:" + "6" * 64,
    }
    manifest_reads = 0

    def changing_fetch(url: str) -> Any:
        nonlocal manifest_reads
        if url == MODULE.LIVE_MANIFEST_URL:
            manifest_reads += 1
            if manifest_reads == 2:
                return changed_manifest
        return changing[url]

    try:
        MODULE.resolve_live_publication(fetch=changing_fetch)
    except ValueError as error:
        if "changed while resolving" not in str(error):
            raise SystemExit("manifest change during resolution failed for the wrong reason")
    else:
        raise SystemExit("resolver accepted a publication that changed during validation")


def main() -> None:
    check_fetch_retries_transient_errors()
    check_fetch_uses_github_token_when_configured()
    check_fetch_retries_secondary_rate_limit_reset()
    check_fetch_aborts_excessive_secondary_rate_limit_delay()
    check_fetch_exhausts_secondary_rate_limit_attempts()
    check_fetch_errors_are_actionable()
    check_untrusted_redirect_is_rejected()
    check_oversized_response_is_rejected()
    check_reviewed_target_and_key()
    check_new_live_publication_resolves_without_pin_edit()
    check_pinned_release_identity_survives_concurrent_publication()
    check_existing_publication_baseline()
    check_rollback_requires_explicit_dispatch()
    check_live_publication_fails_closed()
    print("demo live web publication checks passed")


if __name__ == "__main__":
    main()
