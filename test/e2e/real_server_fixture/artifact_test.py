#!/usr/bin/env python3
"""Behavior tests for the fixture-owned production artifact boundary."""

from __future__ import annotations

import base64
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("artifact.py")
WEB_REVISION = "69bd2c7dc25b6e92630d04363c8460cfd2ab000e"
ENDPOINT = "wss://telegramd.test/apiws"
FINGERPRINT = "1234567890abcdef"
# Ordinary product references the accepted private release carries in executable assets.
PRODUCT_REFERENCES = (
    "https://web.telegram.org/a/",
    "https://telegram.org/android",
    "https://t.me/botfather",
)
CSP = (
    "default-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; "
    "object-src 'none'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; "
    "img-src 'self' data: blob:; font-src 'self' data:; media-src 'self' blob:; "
    "worker-src 'self' blob:; manifest-src 'self'; connect-src 'self' " + ENDPOINT + ";"
)
# One of the trusted Telegram MTProto RSA moduli, used only to prove the
# harness rejects it from its own pinned list.
TRUSTED_MODULUS = (
    "bdf2c77d81f6afd47bd30f29ac76e55adfe70e487e5e48297e5a9055c9c07d2b"
    "93b4ed3994d3eca5098bf18d978d54f8b7c713eb10247607e69af9ef44f38e28"
    "f8b439f257a11572945cc0406fe3f37bb92b79112db69eedf2dc71584a661638"
    "ea5becb9e23585074b80d57d9f5710dd30d2da940e0ada2f1b878397dc1a72b5"
    "ce2531b6f7dd158e09c828d03450ca0ff8a174deacebcaa22dde84ef66ad370f"
    "259d18af806638012da0ca4a70baa83d9c158f3552bc9158e69bf332a45809e1"
    "c36905a5caa12348dd57941a482131be7b2355a5f4635374f3bd3ddf5ff925bf"
    "4809ee27c1e67d9120c5fe08a9de458b1b4a3c5d0a428437f2beca81f4e2d5ff"
)


def artifact_digest(directory: Path) -> str:
    digest = hashlib.sha256()
    for path in sorted(item for item in directory.rglob("*") if item.is_file()):
        relative = path.relative_to(directory).as_posix()
        if relative == "mtproto-target.json":
            continue
        content = path.read_bytes()
        digest.update(relative.encode() + b"\0" + str(len(content)).encode() + b"\0")
        digest.update(content + b"\0")
    return "sha256:" + digest.hexdigest()


def write_manifest(directory: Path, **overrides: str) -> None:
    manifest = {
        "mode": overrides.get("mode", "private"),
        "endpoint": overrides.get("endpoint", ENDPOINT),
        "fingerprint": overrides.get("fingerprint", FINGERPRINT),
        "sourceCommit": overrides.get("sourceCommit", WEB_REVISION),
        "artifactDigest": artifact_digest(directory),
    }
    (directory / "mtproto-target.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")


def make_artifact(directory: Path, **overrides: str) -> None:
    (directory / "assets").mkdir(parents=True, exist_ok=True)
    (directory / "index.html").write_text(
        '<!doctype html><html><head><meta http-equiv="Content-Security-Policy" content="'
        + CSP
        + '"></head><body><script type="module" src="/assets/app.js"></script></body></html>',
        encoding="utf-8",
    )
    (directory / "assets" / "app.js").write_text(
        f"const endpoint = '{ENDPOINT}'; const fingerprint = '{FINGERPRINT}';\n"
        "const productLinks = [" + ", ".join(f"'{link}'" for link in PRODUCT_REFERENCES) + "];\n",
        encoding="utf-8",
    )
    (directory / "assets" / "app.css").write_text("body { color: black; }\n", encoding="utf-8")
    (directory / "service-worker.js").write_text("self.addEventListener('install', () => {});\n", encoding="utf-8")
    (directory / "shared-worker.js").write_text("onconnect = () => {};\n", encoding="utf-8")
    write_manifest(directory, **overrides)


def stage(source: Path, destination: Path, secrets: Path, build: Path, repository: Path) -> subprocess.CompletedProcess[str]:
    if destination.exists():
        for current, directories, names in os.walk(destination, topdown=True):
            os.chmod(current, 0o700)
            for name in directories:
                os.chmod(os.path.join(current, name), 0o700)
        shutil.rmtree(destination)
    return subprocess.run(
        [
            sys.executable,
            str(SCRIPT),
            "stage",
            "--source",
            str(source),
            "--destination",
            str(destination),
            "--secret-dir",
            str(secrets),
            "--build-dir",
            str(build),
            "--repo-root",
            str(repository),
            "--endpoint",
            ENDPOINT,
            "--fingerprint",
            FINGERPRINT,
            "--web-revision",
            WEB_REVISION,
        ],
        capture_output=True,
        text=True,
        check=False,
    )


class ArtifactBoundaryTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="fixture-artifact-test-")
        self.root = Path(self.temp.name)
        self.source = self.root / "caller-bundle"
        self.secrets = self.root / "secrets"
        self.build = self.root / "build"
        self.repository = self.root / "server"
        for path in (self.source, self.secrets, self.build, self.repository):
            path.mkdir(mode=0o700)
        self.destination = self.build / "owned-stage"
        (self.secrets / "a-password").write_bytes(b"synthetic-password-a")
        (self.secrets / "b-password").write_bytes(b"synthetic-password-b")
        (self.secrets / "authkey.hex").write_bytes(b"ab" * 32)
        (self.secrets / "server-key.pem").write_bytes(b"synthetic-server-private-key")
        (self.secrets / "tls.key").write_bytes(b"synthetic-tls-private-key")
        for secret_path in self.secrets.iterdir():
            secret_path.chmod(0o400)
        make_artifact(self.source)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_stages_immutable_bytes_and_reports_independent_audit_results(self) -> None:
        expected_index = hashlib.sha256((self.source / "index.html").read_bytes()).hexdigest()
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertEqual(result.returncode, 0, result.stderr)
        report = json.loads(result.stdout)
        self.assertEqual(report["status"], "passed")
        self.assertEqual(report["artifactDigest"], json.loads((self.destination / "mtproto-target.json").read_text())["artifactDigest"])
        self.assertEqual(report["indexSHA256"], expected_index)
        self.assertEqual(
            set(report["checks"]),
            {
                "manifestMode",
                "manifestEndpoint",
                "manifestFingerprint",
                "manifestSourceCommit",
                "artifactDigest",
                "privateCSP",
                "privateTargetInBundle",
                "safeFileTypesAndPermissions",
                "routeCollisions",
            },
        )
        self.assertTrue(all(report["checks"].values()))
        self.assertEqual(
            [[item["file"], item["reference"]] for item in report["productReferences"]],
            [["assets/app.js", link] for link in PRODUCT_REFERENCES],
        )
        self.assertEqual(report["productReferenceCount"], len(PRODUCT_REFERENCES))
        (self.source / "index.html").write_text("caller changed after attach", encoding="utf-8")
        self.assertEqual(hashlib.sha256((self.destination / "index.html").read_bytes()).hexdigest(), expected_index)

    def test_rejects_source_nested_in_fixture_secret_directory(self) -> None:
        result = stage(self.secrets, self.destination, self.secrets, self.build, self.repository)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("artifact source overlaps a protected fixture path", result.stderr)
        self.assertFalse(self.destination.exists())

    def test_rejects_symlink_entries_without_following_them(self) -> None:
        (self.source / "leak.txt").symlink_to(self.secrets / "a-password")
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("symlink", result.stderr.lower())
        self.assertFalse(self.destination.exists())

    def test_rejects_hardlinks_and_unsafe_permissions(self) -> None:
        os.link(self.source / "assets" / "app.js", self.source / "linked.js")
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("hard link", result.stderr.lower())
        self.assertFalse(self.destination.exists())

        (self.source / "linked.js").unlink()
        (self.source / "assets" / "app.css").chmod(0o666)
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unsafe file permissions", result.stderr.lower())
        self.assertFalse(self.destination.exists())

        (self.source / "assets" / "app.css").chmod(0o200)
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not readable", result.stderr.lower())
        self.assertFalse(self.destination.exists())

    def test_rejects_special_files_and_artifact_limits(self) -> None:
        os.mkfifo(self.source / "pipe")
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unsupported file type", result.stderr.lower())
        self.assertFalse(self.destination.exists())

        (self.source / "pipe").unlink()
        with (self.source / "oversized.bin").open("wb") as output:
            output.truncate(128 * 1024 * 1024 + 1)
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("total byte limit", result.stderr.lower())
        self.assertFalse(self.destination.exists())

        (self.source / "oversized.bin").unlink()
        for index in range(5000 - 6 + 1):
            (self.source / f"extra-{index}").touch()
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("file count limit", result.stderr.lower())
        self.assertFalse(self.destination.exists())

    def test_rejects_control_character_paths_and_excessive_depth(self) -> None:
        (self.source / "unsafe\nname.js").write_text("unsafe", encoding="utf-8")
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unsafe path", result.stderr.lower())
        self.assertFalse(self.destination.exists())

        (self.source / "unsafe\nname.js").unlink()
        current = self.source
        for index in range(65):
            current = current / f"level-{index}"
            current.mkdir()
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("directory depth limit", result.stderr.lower())
        self.assertFalse(self.destination.exists())

    def test_rejects_wrong_build_revision_and_run_target(self) -> None:
        for revision in (
            "84961bf77003a1bdb582d1096d988f1d304e3d1f",
            "09373cc2713d31e93664c38a4fd0335ea37a5f01",
        ):
            with self.subTest(source_commit=revision):
                write_manifest(self.source, sourceCommit=revision)
                result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("manifestSourceCommit", result.stderr)
                self.assertFalse(self.destination.exists())

    def test_rejects_run_secrets_and_production_destinations_in_any_file(self) -> None:
        (self.source / "assets" / "app.css").write_text(
            "/* "
            "synthetic-password-a telegram-server.tailaa4918.ts.net "
            "synthetic-server-private-key synthetic-tls-private-key "
            "fbb62871f07fae2a wss://other.example.test/apiws "
            + base64.b64encode(bytes.fromhex("ab" * 32)).decode("ascii")
            + " */",
            encoding="utf-8",
        )
        write_manifest(self.source)
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("productionIdentifiers", result.stderr)
        self.assertIn("runSecrets", result.stderr)
        self.assertIn("alternateWebSocketRoutes", result.stderr)
        self.assertFalse(self.destination.exists())

    def test_rejects_upper_case_alternate_websocket_scheme(self) -> None:
        (self.source / "assets" / "app.js").write_text(
            'new WebSocket("WSS://alt.example/apiws");\n', encoding="utf-8"
        )
        write_manifest(self.source)
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("alternateWebSocketRoutes", result.stderr)
        self.assertFalse(self.destination.exists())

    def test_rejects_manifest_digest_that_does_not_match_staged_bytes(self) -> None:
        (self.source / "assets" / "app.css").write_text("body { color: rebeccapurple; }\n", encoding="utf-8")
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("artifactDigest", result.stderr)
        self.assertFalse(self.destination.exists())

    def test_permits_product_references_and_records_their_staged_locations(self) -> None:
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertEqual(result.returncode, 0, result.stderr)
        report = json.loads(result.stdout)
        self.assertNotIn("officialTelegramDestinations", report["checks"])
        recorded = {(item["file"], item["reference"]) for item in report["productReferences"]}
        self.assertTrue({("assets/app.js", link) for link in PRODUCT_REFERENCES} <= recorded)

    def test_rejects_datacenter_addresses_in_any_staged_file(self) -> None:
        for payload, category in (
            ("149.154.167.51", "officialDcIpRanges"),
            ("2001:67c:4e8:f002::a", "officialDcIpv6Prefixes"),
            ("2001:b28:f23d::5", "officialDcIpv6Prefixes"),
        ):
            with self.subTest(payload=payload):
                (self.source / "assets" / "app.css").write_text(f"/* {payload} */\n", encoding="utf-8")
                write_manifest(self.source)
                result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(category, result.stderr)
                self.assertIn("officialDcIpRanges", result.stderr)
                self.assertFalse(self.destination.exists())

    def test_rejects_official_mtproto_routes_and_dc_hosts(self) -> None:
        cases = (
            ("wss://kws1-1.web.telegram.org/apiws", "officialMtprotoRoutes"),
            ("https://web.telegram.org/apiw1", "officialMtprotoRoutes"),
            ("https://web.telegram.org/apiw_test1", "officialMtprotoRoutes"),
            ("https://pluto1.web.telegram.org:443", "officialDcHosts"),
            ("`wss://${host}.web.telegram.org/`", "officialMtprotoDynamicRoutes"),
            ("'wss://' + host + '/apiws'", "officialMtprotoDynamicRoutes"),
        )
        for payload, category in cases:
            with self.subTest(payload=payload):
                (self.source / "assets" / "app.js").write_text(
                    f"const endpoint = '{ENDPOINT}'; const fingerprint = '{FINGERPRINT}';\n"
                    f"const route = {payload!r};\n",
                    encoding="utf-8",
                )
                write_manifest(self.source)
                result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(category, result.stderr)
                self.assertFalse(self.destination.exists())

    def test_rejects_cleartext_websocket_routes(self) -> None:
        (self.source / "assets" / "app.js").write_text(
            f"const endpoint = '{ENDPOINT}'; const fingerprint = '{FINGERPRINT}';\n"
            "const fallback = 'ws://telegramd.test/apiws';\n",
            encoding="utf-8",
        )
        write_manifest(self.source)
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("cleartextWebSocketRoutes", result.stderr)
        self.assertFalse(self.destination.exists())

    def test_rejects_trusted_telegram_rsa_fingerprints_and_moduli(self) -> None:
        cases = (
            "c3b42b026ce86b21",
            "0BC35F3509F7B7A5",
            TRUSTED_MODULUS,
            base64.b64encode(bytes.fromhex(TRUSTED_MODULUS)).decode("ascii"),
        )
        for payload in cases:
            with self.subTest(payload=payload[:24]):
                (self.source / "assets" / "app.js").write_text(
                    f"const endpoint = '{ENDPOINT}'; const fingerprint = '{FINGERPRINT}';\n"
                    f"const key = '{payload}';\n",
                    encoding="utf-8",
                )
                write_manifest(self.source)
                result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("trustedRsaKeyMaterial", result.stderr)
                self.assertFalse(self.destination.exists())

    def test_rejects_private_key_blocks_and_fixture_route_collisions(self) -> None:
        (self.source / "assets" / "app.css").write_text(
            "-----BEGIN RSA PRIVATE KEY-----\nZm9v\n-----END RSA PRIVATE KEY-----\n", encoding="utf-8"
        )
        write_manifest(self.source)
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("privateKeyBlocks", result.stderr)
        self.assertFalse(self.destination.exists())

        (self.source / "assets" / "app.css").write_text("body { color: black; }\n", encoding="utf-8")
        (self.source / "healthz").write_text("not a bundle route", encoding="utf-8")
        write_manifest(self.source)
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("routeCollisions", result.stderr)
        self.assertFalse(self.destination.exists())

        (self.source / "healthz").unlink()
        (self.source / "_fixture_probe").mkdir()
        (self.source / "_fixture_probe" / "index.html").write_text("reserved", encoding="utf-8")
        write_manifest(self.source)
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("routeCollisions", result.stderr)
        self.assertFalse(self.destination.exists())


if __name__ == "__main__":
    unittest.main()
