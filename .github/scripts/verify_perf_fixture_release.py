#!/usr/bin/env python3
"""Verify a published RPC performance fixture using anonymous downloads."""

from __future__ import annotations

import argparse
import hashlib
import http.client
import json
import re
import sys
import time
import urllib.parse
import urllib.request
from typing import BinaryIO, Callable


REQUEST_TIMEOUT_SECONDS = 30
DEFAULT_ATTEMPTS = 5
DEFAULT_INITIAL_DELAY_SECONDS = 1.0
DEFAULT_MAX_DELAY_SECONDS = 8.0
CHUNK_BYTES = 1024 * 1024
OpenURL = Callable[..., BinaryIO]


def asset_checksums(manifest_bytes: bytes) -> list[tuple[str, str]]:
    try:
        manifest = json.loads(manifest_bytes)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise ValueError("release manifest is not valid JSON") from error
    if not isinstance(manifest, dict):
        raise ValueError("release manifest must be an object")
    assets = manifest.get("assets")
    if not isinstance(assets, list) or not assets:
        raise ValueError("release manifest must list at least one asset")

    checksums: list[tuple[str, str]] = []
    names: set[str] = set()
    for asset in assets:
        if not isinstance(asset, dict):
            raise ValueError("release manifest asset entries must be objects")
        name = asset.get("name")
        checksum = asset.get("sha256")
        if (
            not isinstance(name, str)
            or not name
            or name in {".", ".."}
            or "/" in name
            or "\\" in name
        ):
            raise ValueError("release manifest contains an unsafe asset name")
        if name in names:
            raise ValueError(f"release manifest contains duplicate asset {name}")
        if not isinstance(checksum, str) or re.fullmatch(r"[0-9a-f]{64}", checksum) is None:
            raise ValueError(f"release manifest has an invalid SHA-256 for {name}")
        names.add(name)
        checksums.append((name, checksum))
    return checksums


def download_bytes(url: str, opener: OpenURL) -> bytes:
    request = urllib.request.Request(
        url,
        headers={"User-Agent": "teagram-perf-fixture-verifier"},
        method="GET",
    )
    with opener(request, timeout=REQUEST_TIMEOUT_SECONDS) as response:
        return response.read()


def download_sha256(url: str, opener: OpenURL) -> str:
    request = urllib.request.Request(
        url,
        headers={"User-Agent": "teagram-perf-fixture-verifier"},
        method="GET",
    )
    digest = hashlib.sha256()
    with opener(request, timeout=REQUEST_TIMEOUT_SECONDS) as response:
        while chunk := response.read(CHUNK_BYTES):
            digest.update(chunk)
    return digest.hexdigest()


def verify_once(release_url: str, opener: OpenURL) -> None:
    manifest_url = f"{release_url.rstrip('/')}/manifest.json"
    checksums = asset_checksums(download_bytes(manifest_url, opener))
    base_url = release_url.rstrip("/")
    for name, expected in checksums:
        encoded_name = urllib.parse.quote(name, safe="")
        actual = download_sha256(f"{base_url}/{encoded_name}", opener)
        if actual != expected:
            raise ValueError(f"SHA-256 mismatch for public release asset {name}")


def verify_public_release(
    release_url: str,
    *,
    attempts: int = DEFAULT_ATTEMPTS,
    initial_delay_seconds: float = DEFAULT_INITIAL_DELAY_SECONDS,
    max_delay_seconds: float = DEFAULT_MAX_DELAY_SECONDS,
    opener: OpenURL | None = None,
    sleeper: Callable[[float], None] | None = None,
) -> None:
    parsed_url = urllib.parse.urlsplit(release_url)
    if parsed_url.scheme != "https" or not parsed_url.netloc:
        raise ValueError("release URL must be an absolute HTTPS URL")
    if attempts < 1 or initial_delay_seconds < 0 or max_delay_seconds < 0:
        raise ValueError("retry attempts must be positive and delays non-negative")

    open_url = opener or urllib.request.urlopen
    sleep = sleeper or time.sleep
    delay = initial_delay_seconds
    for attempt in range(1, attempts + 1):
        try:
            verify_once(release_url, open_url)
            return
        except (http.client.HTTPException, OSError, ValueError) as error:
            if attempt == attempts:
                raise RuntimeError(
                    f"anonymous fixture release verification failed after "
                    f"{attempts} attempts: {error}"
                ) from error
            print(
                f"Anonymous fixture release verification attempt {attempt}/"
                f"{attempts} failed: {error}; retrying in {delay:g}s",
                file=sys.stderr,
            )
            sleep(delay)
            delay = min(delay * 2, max_delay_seconds)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("release_url")
    args = parser.parse_args()
    try:
        verify_public_release(args.release_url)
    except (OSError, RuntimeError, ValueError) as error:
        print(f"could not verify public fixture release: {error}", file=sys.stderr)
        return 1
    print("Anonymous fixture release downloads and SHA-256 checks passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
