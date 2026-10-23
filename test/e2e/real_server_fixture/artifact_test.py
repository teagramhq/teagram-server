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
WEB_REVISION = "e947f119ba90d9d87b198c9d7a113cceb1395318"
AUTH_PROGRESS_WEB_REVISION = "569529c1f36923c098760ef727808254dcb6a662"
ENDPOINT = "wss://teagramd.test/apiws"
FINGERPRINT = "1234567890abcdef"
DISCOVERY_CONNECT_SRC = "connect-src 'self' https: wss:"
# Ordinary product references the accepted private release carries in executable assets.
PRODUCT_REFERENCES = (
    "https://web.telegram.org/a/",
    "https://telegram.org/android",
    "https://t.me/botfather",
)
CSP = (
    "default-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; "
    "frame-src 'self' https://challenges.cloudflare.com; object-src 'none'; "
    "script-src 'self' 'wasm-unsafe-eval' https://challenges.cloudflare.com; style-src 'self' 'unsafe-inline'; "
    "img-src 'self' data: blob:; font-src 'self' data:; media-src 'self' blob:; "
    "worker-src 'self' blob:; manifest-src 'self'; " + DISCOVERY_CONNECT_SRC + ";"
)
AUTH_PROGRESS_CSP = (
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
        content = path.read_bytes()
        digest.update(relative.encode() + b"\0" + str(len(content)).encode() + b"\0")
        digest.update(content + b"\0")
    return "sha256:" + digest.hexdigest()


def producer_digest(directory: Path) -> str:
    script = """
import {createHash} from 'node:crypto';
import {readdirSync, readFileSync} from 'node:fs';
import {relative, resolve, sep} from 'node:path';
const root = resolve(process.argv[1]);
const files = [];
const visit = (current) => {
  for(const entry of readdirSync(current, {withFileTypes: true})) {
    const file = resolve(current, entry.name);
    if(entry.isDirectory()) visit(file);
    else if(entry.isFile()) files.push(file);
  }
};
visit(root);
files.sort((left, right) => relative(root, left).split(sep).join('/').localeCompare(relative(root, right).split(sep).join('/')));
const hash = createHash('sha256');
for(const file of files) {
  const path = relative(root, file).split(sep).join('/');
  if(path === 'mtproto-target.json') continue;
  const contents = readFileSync(file);
  hash.update(path + '\\0' + contents.length + '\\0');
  hash.update(contents);
  hash.update('\\0');
}
console.log('sha256:' + hash.digest('hex'));
"""
    result = subprocess.run(
        ["node", "--input-type=module", "-e", script, str(directory)],
        capture_output=True,
        text=True,
        check=True,
    )
    return result.stdout.strip()


def write_manifest(directory: Path, **overrides: str) -> None:
    manifest = {
        "mode": overrides.get("mode", "private"),
        "endpoint": overrides.get("endpoint", ENDPOINT),
        "fingerprint": overrides.get("fingerprint", FINGERPRINT),
        "sourceCommit": overrides.get("sourceCommit", WEB_REVISION),
        "artifactDigest": producer_digest(directory),
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


def stage(
    source: Path,
    destination: Path,
    secrets: Path,
    build: Path,
    repository: Path,
    web_revision: str = WEB_REVISION,
) -> subprocess.CompletedProcess[str]:
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
            web_revision,
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

    def test_accepts_discovery_ready_csp_and_stages_immutable_bytes(self) -> None:
        expected_index = hashlib.sha256((self.source / "index.html").read_bytes()).hexdigest()
        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)
        self.assertEqual(result.returncode, 0, result.stderr)
        report = json.loads(result.stdout)
        self.assertEqual(report["status"], "passed")
        self.assertEqual(report["artifactDigest"], artifact_digest(self.destination))
        manifest = json.loads((self.destination / "mtproto-target.json").read_text())
        self.assertEqual(report["manifestArtifactDigest"], manifest["artifactDigest"])
        self.assertEqual(manifest["artifactDigest"], producer_digest(self.destination))
        self.assertNotEqual(report["artifactDigest"], manifest["artifactDigest"])
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

    def test_rejects_unreviewed_csp_network_sources(self) -> None:
        index_path = self.source / "index.html"
        original = index_path.read_text(encoding="utf-8")
        for extra_source in ("http:", "*", "https://unexpected.invalid"):
            with self.subTest(extra_source=extra_source):
                index_path.write_text(
                    original.replace(
                        DISCOVERY_CONNECT_SRC,
                        f"{DISCOVERY_CONNECT_SRC} {extra_source}",
                    ),
                    encoding="utf-8",
                )
                write_manifest(self.source)

                result = stage(self.source, self.destination, self.secrets, self.build, self.repository)

                self.assertNotEqual(result.returncode, 0)
                self.assertIn("privateCSP", result.stderr)
                self.assertFalse(self.destination.exists())

    def test_accepts_legacy_exact_private_csp_for_current_revision(self) -> None:
        index_path = self.source / "index.html"
        legacy_csp = CSP.replace(DISCOVERY_CONNECT_SRC, f"connect-src 'self' {ENDPOINT}")
        index_path.write_text(
            index_path.read_text(encoding="utf-8").replace(CSP, legacy_csp),
            encoding="utf-8",
        )
        write_manifest(self.source)

        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(json.loads(result.stdout)["checks"]["privateCSP"])

    def test_rejects_multiple_private_csp_meta_elements(self) -> None:
        index_path = self.source / "index.html"
        original = index_path.read_text(encoding="utf-8")
        duplicate = '<meta http-equiv="Content-Security-Policy" content="' + CSP + '">'
        index_path.write_text(original.replace("</head>", duplicate + "</head>"), encoding="utf-8")
        write_manifest(self.source)

        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("privateCSP", result.stderr)
        self.assertFalse(self.destination.exists())

    def test_rejects_changed_non_connect_csp_directive(self) -> None:
        index_path = self.source / "index.html"
        index_path.write_text(
            index_path.read_text(encoding="utf-8").replace("default-src 'self'", "default-src *"),
            encoding="utf-8",
        )
        write_manifest(self.source)

        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("privateCSP", result.stderr)
        self.assertFalse(self.destination.exists())

    def test_accepts_exact_historical_auth_progress_csp(self) -> None:
        index_path = self.source / "index.html"
        index_path.write_text(
            index_path.read_text(encoding="utf-8").replace(CSP, AUTH_PROGRESS_CSP),
            encoding="utf-8",
        )
        write_manifest(self.source, sourceCommit=AUTH_PROGRESS_WEB_REVISION)

        result = stage(
            self.source,
            self.destination,
            self.secrets,
            self.build,
            self.repository,
            web_revision=AUTH_PROGRESS_WEB_REVISION,
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(json.loads(result.stdout)["checks"]["privateCSP"])

    def test_rejects_historical_auth_progress_csp_for_current_revision(self) -> None:
        index_path = self.source / "index.html"
        index_path.write_text(
            index_path.read_text(encoding="utf-8").replace(CSP, AUTH_PROGRESS_CSP),
            encoding="utf-8",
        )
        write_manifest(self.source)

        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("privateCSP", result.stderr)
        self.assertFalse(self.destination.exists())

    def test_frozen_digest_changes_when_target_manifest_bytes_change(self) -> None:
        original = self.root / "frozen-original"
        changed = self.root / "frozen-manifest-changed"
        shutil.copytree(self.source, original)
        shutil.copytree(self.source, changed)
        manifest_path = changed / "mtproto-target.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["fingerprint"] = "fedcba0987654321"
        manifest_path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")

        for directory in (original, changed):
            for path in directory.rglob("*"):
                path.chmod(0o555 if path.is_dir() else 0o444)
            directory.chmod(0o555)

        try:
            digests = []
            for directory in (original, changed):
                result = subprocess.run(
                    [sys.executable, str(SCRIPT), "digest", "--directory", str(directory)],
                    capture_output=True,
                    text=True,
                    encoding="utf-8",
                    check=False,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                digests.append(json.loads(result.stdout)["artifactDigest"])
            self.assertNotEqual(digests[0], digests[1])
        finally:
            for directory in (original, changed):
                for current, subdirectories, names in os.walk(directory, topdown=False):
                    current_path = Path(current)
                    for name in names:
                        (current_path / name).chmod(0o600)
                    for name in subdirectories:
                        (current_path / name).chmod(0o700)
                    current_path.chmod(0o700)

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
            "synthetic-password-a teagram.tailaa4918.ts.net "
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

    def test_rejects_producer_digest_that_disagrees_with_canonical_frozen_digest(self) -> None:
        manifest_path = self.source / "mtproto-target.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["artifactDigest"] = "sha256:" + "0" * 64
        manifest_path.write_text(json.dumps(manifest) + "\n", encoding="utf-8")

        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("artifactDigest", result.stderr)
        self.assertFalse(self.destination.exists())

    def test_accepts_producer_digest_for_case_collated_relative_paths(self) -> None:
        (self.source / "a.js").write_text("lowercase path\n", encoding="utf-8")
        (self.source / "B.js").write_text("uppercase path\n", encoding="utf-8")
        manifest_path = self.source / "mtproto-target.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["artifactDigest"] = producer_digest(self.source)
        manifest_path.write_text(json.dumps(manifest) + "\n", encoding="utf-8")

        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)

        self.assertEqual(result.returncode, 0, result.stderr)
        report = json.loads(result.stdout)
        self.assertEqual(report["artifactDigest"], artifact_digest(self.destination))
        staged_manifest = json.loads((self.destination / "mtproto-target.json").read_text())
        self.assertEqual(report["manifestArtifactDigest"], staged_manifest["artifactDigest"])
        self.assertEqual(staged_manifest["artifactDigest"], producer_digest(self.destination))

    def test_rejects_bundle_and_manifest_built_for_another_experiment_key(self) -> None:
        other_fingerprint = "fedcba0987654321"
        app_path = self.source / "assets" / "app.js"
        app_path.write_text(
            app_path.read_text(encoding="utf-8").replace(FINGERPRINT, other_fingerprint), encoding="utf-8"
        )
        write_manifest(self.source, fingerprint=other_fingerprint)

        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("manifestFingerprint", result.stderr)
        self.assertIn("privateTargetInBundle", result.stderr)
        self.assertFalse(self.destination.exists())

    def test_rejects_wrong_bundle_key_even_when_manifest_matches_this_experiment(self) -> None:
        other_fingerprint = "fedcba0987654321"
        app_path = self.source / "assets" / "app.js"
        app_path.write_text(
            app_path.read_text(encoding="utf-8").replace(FINGERPRINT, other_fingerprint), encoding="utf-8"
        )
        write_manifest(self.source, fingerprint=FINGERPRINT)

        result = stage(self.source, self.destination, self.secrets, self.build, self.repository)

        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("manifestFingerprint", result.stderr)
        self.assertIn("privateTargetInBundle", result.stderr)
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
            "const fallback = 'ws://teagramd.test/apiws';\n",
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
