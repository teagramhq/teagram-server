#!/usr/bin/env python3
"""Exercise checksum receipt and safe extraction of demo web releases."""

from __future__ import annotations

import hashlib
import json
import os
import pathlib
import subprocess
import sys
import tarfile
import tempfile
from io import BytesIO


SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
SOURCE_SHA = "a" * 40
ARTIFACT_DIGEST = "sha256:" + "b" * 64


def write_archive(path: pathlib.Path, entries: dict[str, bytes]) -> None:
    with tarfile.open(path, "w:gz") as archive:
        for name, contents in entries.items():
            member = tarfile.TarInfo(name)
            member.size = len(contents)
            member.mode = 0o644
            archive.addfile(member, BytesIO(contents))


def run(*args: str, input_bytes: bytes | None = None, env: dict[str, str] | None = None) -> subprocess.CompletedProcess[bytes]:
    return subprocess.run(
        [sys.executable, *args],
        check=False,
        input=input_bytes,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
    )


def main() -> None:
    manifest = json.dumps(
        {
            "mode": "private",
            "endpoint": "wss://web.example/apiws",
            "fingerprint": "c" * 16,
            "sourceCommit": SOURCE_SHA,
            "artifactDigest": ARTIFACT_DIGEST,
        }
    ).encode()
    entries = {
        "./index.html": b'<title>Teagram Web</title><script type="module" src="/assets/app.js"></script>',
        "./mtproto-target.json": manifest,
        "./assets/app.js": b"console.log('demo');",
    }

    with tempfile.TemporaryDirectory(prefix="demo-web-release-test-") as temporary:
        root = pathlib.Path(temporary)
        incoming = root / "incoming"
        incoming.mkdir(mode=0o700)
        archive_path = root / "web.tar.gz"
        write_archive(archive_path, entries)
        archive_bytes = archive_path.read_bytes()
        archive_sha256 = hashlib.sha256(archive_bytes).hexdigest()
        received_path = incoming / "received.tar.gz"
        received = run(
            str(SCRIPT_DIR / "receive-web-artifact.py"),
            str(received_path),
            archive_sha256,
            input_bytes=archive_bytes,
        )
        if received.returncode != 0 or received_path.read_bytes() != archive_bytes:
            raise SystemExit(f"web archive receipt failed: {received.stderr.decode(errors='replace')}")
        if received_path.stat().st_mode & 0o777 != 0o600:
            raise SystemExit("web archive receipt did not create a mode 0600 file")

        releases = root / "releases"
        releases.mkdir(mode=0o755)
        install_env = os.environ.copy()
        install_env["DEPLOY_WEB_ARCHIVE_SHA256"] = archive_sha256
        install_args = (
            str(SCRIPT_DIR / "install-web-release.py"),
            "stage",
            str(received_path),
            str(releases),
            SOURCE_SHA,
            ARTIFACT_DIGEST,
        )
        staged = run(*install_args, env=install_env)
        expected_release = f"{SOURCE_SHA}-{ARTIFACT_DIGEST.removeprefix('sha256:')}"
        if staged.returncode != 0 or staged.stdout.decode().strip() != expected_release:
            raise SystemExit(f"web release extraction failed: {staged.stderr.decode(errors='replace')}")
        release_dir = releases / expected_release
        if (release_dir / "assets/app.js").read_bytes() != entries["./assets/app.js"]:
            raise SystemExit("web release extraction changed an artifact file")
        repeated = run(*install_args, env=install_env)
        if repeated.returncode != 0 or repeated.stdout.decode().strip() != expected_release:
            raise SystemExit("web release staging is not idempotent")

        bad_archive = root / "bad.tar.gz"
        write_archive(bad_archive, {"../escape.txt": b"no"})
        bad_env = install_env.copy()
        bad_env["DEPLOY_WEB_ARCHIVE_SHA256"] = hashlib.sha256(bad_archive.read_bytes()).hexdigest()
        rejected = run(
            str(SCRIPT_DIR / "install-web-release.py"),
            "stage",
            str(bad_archive),
            str(releases),
            SOURCE_SHA,
            ARTIFACT_DIGEST,
            env=bad_env,
        )
        if rejected.returncode == 0 or (root / "escape.txt").exists():
            raise SystemExit("web release installer accepted a traversal path")

        tampered = bytearray(archive_bytes)
        tampered[-1] ^= 1
        rejected_transfer_path = incoming / "tampered.tar.gz"
        rejected_transfer = run(
            str(SCRIPT_DIR / "receive-web-artifact.py"),
            str(rejected_transfer_path),
            archive_sha256,
            input_bytes=bytes(tampered),
        )
        if rejected_transfer.returncode == 0 or rejected_transfer_path.exists():
            raise SystemExit("web archive receiver accepted a checksum mismatch")

        env_file = root / "demo.env"
        env_file.write_text(
            "POSTGRES_PASSWORD=" + "d" * 64 + "\n"
            "TG_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io\n"
            "TEAGRAM_DEMO_TURNSTILE_SITE_KEY=fixture-secret-must-not-be-printed\n",
            encoding="utf-8",
        )
        env_file.chmod(0o600)
        updated = run(str(SCRIPT_DIR / "update-websocket-origins.py"), str(env_file))
        expected_env = env_file.read_text(encoding="utf-8")
        if (
            updated.returncode != 0
            or "TG_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io,https://demo.teagram.io\n" not in expected_env
            or "POSTGRES_PASSWORD=" + "d" * 64 + "\n" not in expected_env
            or "fixture-secret-must-not-be-printed" in updated.stdout.decode(errors="replace")
            or "fixture-secret-must-not-be-printed" in updated.stderr.decode(errors="replace")
        ):
            raise SystemExit("demo websocket origin update did not preserve the secret environment")
        unchanged = run(str(SCRIPT_DIR / "update-websocket-origins.py"), str(env_file))
        if unchanged.returncode != 0 or env_file.read_text(encoding="utf-8") != expected_env:
            raise SystemExit("demo websocket origin update is not idempotent")

    print("demo web release receipt and extraction checks passed")


if __name__ == "__main__":
    main()
