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
ARTIFACT_PATH_COLLATION = (
    " _-,;:!?.'\"()[]{}@*/\\&#%`^+<=>|~$"
    "0123456789abcdefghijklmnopqrstuvwxyz"
)
ARTIFACT_PATH_ORDER = {character: index for index, character in enumerate(ARTIFACT_PATH_COLLATION)}


def write_archive(path: pathlib.Path, entries: dict[str, bytes]) -> None:
    with tarfile.open(path, "w:gz") as archive:
        for name, contents in entries.items():
            member = tarfile.TarInfo(name)
            member.size = len(contents)
            member.mode = 0o644
            archive.addfile(member, BytesIO(contents))


def artifact_digest(entries: dict[str, bytes]) -> str:
    digest = hashlib.sha256()

    def path_key(name: str) -> tuple[tuple[int, ...], tuple[int, ...]]:
        relative_path = name.removeprefix("./")
        primary = tuple(ARTIFACT_PATH_ORDER[character.lower()] for character in relative_path)
        case = tuple(1 if character.isupper() else 0 for character in relative_path)
        return primary, case

    for name, contents in sorted(entries.items(), key=lambda entry: path_key(entry[0])):
        relative_path = name.removeprefix("./")
        if relative_path == "mtproto-target.json":
            continue
        digest.update(f"{relative_path}\0{len(contents)}\0".encode())
        digest.update(contents)
        digest.update(b"\0")
    return "sha256:" + digest.hexdigest()


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
    entries = {
        "./index.html": b'<title>Teagram Web</title><script type="module" src="/assets/app.js"></script>',
        "./assets/app.js": b"console.log('demo');",
        "./assets/about-DysinTlO.js": b"export const about = true;",
        "./assets/AuthCard-RMGYlx7Z.js": b"export const AuthCard = true;",
        "./assets/KFOm.woff2": b"font fixture",
        "./assets/a-b.js": b"export const hyphen = true;",
        "./assets/a_b.js": b"export const underscore = true;",
    }
    artifact_digest_value = artifact_digest(entries)
    entries["./mtproto-target.json"] = json.dumps(
        {
            "mode": "private",
            "endpoint": "wss://web.example/apiws",
            "fingerprint": "c" * 16,
            "sourceCommit": SOURCE_SHA,
            "artifactDigest": artifact_digest_value,
        }
    ).encode()

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
            artifact_digest_value,
        )
        staged = run(*install_args, env=install_env)
        expected_release = f"{SOURCE_SHA}-{artifact_digest_value.removeprefix('sha256:')}"
        if staged.returncode != 0 or staged.stdout.decode().strip() != expected_release:
            raise SystemExit(f"web release extraction failed: {staged.stderr.decode(errors='replace')}")
        release_dir = releases / expected_release
        if (release_dir / "assets/app.js").read_bytes() != entries["./assets/app.js"]:
            raise SystemExit("web release extraction changed an artifact file")
        repeated = run(*install_args, env=install_env)
        if repeated.returncode != 0 or repeated.stdout.decode().strip() != expected_release:
            raise SystemExit("web release staging is not idempotent")

        altered_entries = entries.copy()
        altered_entries["./assets/app.js"] = b"console.log('unverified payload');"
        altered_archive = root / "altered-web.tar.gz"
        write_archive(altered_archive, altered_entries)
        altered_archive.chmod(0o600)
        altered_env = install_env.copy()
        altered_env["DEPLOY_WEB_ARCHIVE_SHA256"] = hashlib.sha256(
            altered_archive.read_bytes()
        ).hexdigest()
        altered = run(
            str(SCRIPT_DIR / "install-web-release.py"),
            "stage",
            str(altered_archive),
            str(releases),
            SOURCE_SHA,
            artifact_digest_value,
            env=altered_env,
        )
        if altered.returncode == 0:
            raise SystemExit("web release installer accepted changed content with a copied pinned identity")

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
            artifact_digest_value,
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
            or "TG_UPLOADS_DISABLED=true\n" not in expected_env
            or "POSTGRES_PASSWORD=" + "d" * 64 + "\n" not in expected_env
            or "fixture-secret-must-not-be-printed" in updated.stdout.decode(errors="replace")
            or "fixture-secret-must-not-be-printed" in updated.stderr.decode(errors="replace")
        ):
            raise SystemExit("demo websocket origin update did not preserve the secret environment")
        unchanged = run(str(SCRIPT_DIR / "update-websocket-origins.py"), str(env_file))
        if unchanged.returncode != 0 or env_file.read_text(encoding="utf-8") != expected_env:
            raise SystemExit("demo websocket origin update is not idempotent")

        current_origins_env = root / "demo-current-origins.env"
        current_origins_env.write_text(
            "TG_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io,https://demo.teagram.io\n"
            "TEAGRAM_DEMO_TURNSTILE_SITE_KEY=fixture-secret-must-not-be-printed\n",
            encoding="utf-8",
        )
        current_origins_env.chmod(0o600)
        migrated = run(str(SCRIPT_DIR / "update-websocket-origins.py"), str(current_origins_env))
        if migrated.returncode != 0 or "TG_UPLOADS_DISABLED=true\n" not in current_origins_env.read_text(encoding="utf-8"):
            raise SystemExit("demo env migration omitted TG_UPLOADS_DISABLED for existing origins")

        unsafe_env = expected_env.replace("TG_UPLOADS_DISABLED=true\n", "TG_UPLOADS_DISABLED=false\n")
        env_file.write_text(unsafe_env, encoding="utf-8")
        env_file.chmod(0o600)
        rejected_uploads = run(str(SCRIPT_DIR / "update-websocket-origins.py"), str(env_file))
        if rejected_uploads.returncode == 0 or env_file.read_text(encoding="utf-8") != unsafe_env:
            raise SystemExit("demo environment update accepted client uploads or rewrote the file")

    print("demo web release receipt and extraction checks passed")


if __name__ == "__main__":
    main()
