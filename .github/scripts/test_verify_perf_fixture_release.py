#!/usr/bin/env python3
"""Exercise anonymous fixture release verification and retry behavior."""

from __future__ import annotations

import hashlib
import importlib.util
import io
import json
import urllib.error
import unittest
from pathlib import Path
from typing import Any
from urllib.request import Request


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / ".github/scripts/verify_perf_fixture_release.py"
SPEC = importlib.util.spec_from_file_location("verify_perf_fixture_release", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
verifier = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(verifier)


BASE_URL = (
    "https://github.com/teagramhq/teagram-server/releases/download/"
    "perf-fixture-version"
)
ASSET = b"fixture snapshot"


def manifest_for(asset: bytes = ASSET) -> bytes:
    return json.dumps(
        {
            "assets": [
                {
                    "name": "perf_1x.dump",
                    "sha256": hashlib.sha256(asset).hexdigest(),
                }
            ]
        }
    ).encode()


class PerfFixturePublicVerificationTests(unittest.TestCase):
    def make_opener(
        self, responses: list[bytes | BaseException], requests: list[Request]
    ) -> Any:
        def open_url(request: Request, *, timeout: float) -> io.BytesIO:
            self.assertEqual(timeout, 30)
            self.assertEqual(request.get_method(), "GET")
            self.assertIsNone(request.get_header("Authorization"))
            requests.append(request)
            response = responses.pop(0)
            if isinstance(response, BaseException):
                raise response
            return io.BytesIO(response)

        return open_url

    def test_downloads_manifest_and_assets_anonymously(self) -> None:
        requests: list[Request] = []
        verifier.verify_public_release(
            BASE_URL,
            opener=self.make_opener([manifest_for(), ASSET], requests),
            sleeper=lambda _: self.fail("a successful verification must not sleep"),
        )

        self.assertEqual(
            [request.full_url for request in requests],
            [f"{BASE_URL}/manifest.json", f"{BASE_URL}/perf_1x.dump"],
        )

    def test_checksum_mismatch_retries_entire_verification_with_backoff(self) -> None:
        asset = ASSET
        manifest = manifest_for(asset)
        requests: list[Request] = []
        delays: list[float] = []

        verifier.verify_public_release(
            BASE_URL,
            attempts=4,
            initial_delay_seconds=0.5,
            max_delay_seconds=1,
            opener=self.make_opener(
                [
                    manifest,
                    b"stale",
                    manifest,
                    b"stale retry",
                    manifest,
                    b"stale again",
                    manifest,
                    asset,
                ],
                requests,
            ),
            sleeper=delays.append,
        )

        self.assertEqual(delays, [0.5, 1, 1])
        self.assertEqual(len(requests), 8)
        self.assertEqual(requests[0].full_url, requests[2].full_url)
        self.assertEqual(requests[2].full_url, requests[4].full_url)
        self.assertEqual(requests[4].full_url, requests[6].full_url)

    def test_transient_download_error_retries_with_backoff(self) -> None:
        asset = ASSET
        manifest = manifest_for(asset)
        requests: list[Request] = []
        delays: list[float] = []

        verifier.verify_public_release(
            BASE_URL,
            attempts=3,
            initial_delay_seconds=0.25,
            opener=self.make_opener(
                [urllib.error.URLError("temporary"), manifest, asset], requests
            ),
            sleeper=delays.append,
        )

        self.assertEqual(delays, [0.25])
        self.assertEqual(len(requests), 3)


if __name__ == "__main__":
    unittest.main()
