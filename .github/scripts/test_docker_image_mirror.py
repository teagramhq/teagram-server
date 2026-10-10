#!/usr/bin/env python3
"""Keep CI Docker Hub pulls routed through the public Google mirror."""

from __future__ import annotations

import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
ACTIVE_DOCKERFILES = (
    "Dockerfile",
    "Dockerfile.mc",
    "Dockerfile.minio-test",
    "deploy/browser-acceptance/Dockerfile",
    "deploy/link-edge/Dockerfile",
    "deploy/telegramd/mixed-trust-probe.Dockerfile",
    "test/e2e/real_server_fixture/Dockerfile",
)
ACTIVE_IMAGE_CONFIGS = (
    ".github/workflows/ci.yml",
    "docker-compose.yml",
    "docker-compose.mixed-trust.yml",
    "docker-compose.mixed-trust.probe.yml",
    "deploy/browser-acceptance/compose.yaml",
    "deploy/link-edge/compose.yaml",
    "deploy/link-edge/control-compose.yaml",
    "deploy/telegramd/rollout-runner/schema-gate-ci-compose.yml",
)
ACTIVE_PULL_SCRIPTS = (
    ".github/workflows/ci.yml",
    ".github/scripts/run-busybox-phase-check.sh",
    ".github/scripts/smoke-mixed-trust-compose.sh",
    "deploy/telegramd/rollout-runner/README.md",
    "deploy/telegramd/rollout-runner/qualify-rustfs-transition.py",
    "deploy/telegramd/rollout-runner/test-rustfs-schema-postgres.sh",
    "test/e2e/real_server_fixture/start.sh",
)
DIRECT_HUB_IMAGE = re.compile(
    r"(?<![A-Za-z0-9._/-])(?:"
    r"golang:1\.27|golang:1\.26|golang:1\.24\.8-alpine|alpine:3\.22|"
    r"node:24-alpine3\.22@sha256:[0-9a-f]{64}|node@sha256:[0-9a-f]{64}|"
    r"postgres:16-alpine|arigaio/atlas:1\.2\.0-alpine|"
    r"rustfs/rustfs:1\.0\.1@sha256:[0-9a-f]{64}|"
    r"haproxy:3\.2-alpine|busybox:1\.37)"
)
EXTERNAL_REGISTRIES = (
    "gcr.io/",
    "ghcr.io/",
    "mcr.microsoft.com/",
    "mirror.gcr.io/",
    "quay.io/",
)


def is_unqualified_hub_image(image: str) -> bool:
    if image.startswith("${") or image in {"scratch"}:
        return False
    if image.startswith(EXTERNAL_REGISTRIES):
        return False
    if image.endswith((":local", ":control", ":ci")):
        return False
    registry = image.split("/", maxsplit=1)[0]
    return "." not in registry and ":" not in registry


class DockerImageMirrorTests(unittest.TestCase):
    def test_testcontainers_prefix_uses_mirror_without_trailing_slash(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text(encoding="utf-8")
        match = re.search(
            r"(?m)^  TESTCONTAINERS_HUB_IMAGE_NAME_PREFIX:\s*(\S+)\s*$", workflow
        )
        self.assertIsNotNone(match, "CI has no Testcontainers registry prefix")
        self.assertEqual("mirror.gcr.io", match.group(1))

    def test_dockerfile_hub_bases_use_the_mirror(self) -> None:
        for relative_path in ACTIVE_DOCKERFILES:
            with self.subTest(path=relative_path):
                dockerfile = (ROOT / relative_path).read_text(encoding="utf-8")
                for line in dockerfile.splitlines():
                    fields = line.strip().split()
                    if len(fields) < 2 or fields[0] != "FROM":
                        continue
                    image = fields[2] if fields[1].startswith("--") else fields[1]
                    self.assertFalse(
                        is_unqualified_hub_image(image),
                        f"{relative_path} pulls {image} directly from Docker Hub",
                    )

    def test_active_compose_and_service_images_use_the_mirror(self) -> None:
        for relative_path in ACTIVE_IMAGE_CONFIGS:
            with self.subTest(path=relative_path):
                config = (ROOT / relative_path).read_text(encoding="utf-8")
                for line in config.splitlines():
                    match = re.match(r"^\s*image:\s*([^\s#]+)", line)
                    if match is None:
                        continue
                    image = match.group(1).strip("\"'")
                    self.assertFalse(
                        is_unqualified_hub_image(image),
                        f"{relative_path} pulls {image} directly from Docker Hub",
                    )

    def test_active_pull_scripts_have_no_unqualified_hub_images(self) -> None:
        for relative_path in ACTIVE_PULL_SCRIPTS:
            with self.subTest(path=relative_path):
                content = (ROOT / relative_path).read_text(encoding="utf-8")
                match = DIRECT_HUB_IMAGE.search(content)
                if match is not None:
                    self.fail(
                        f"{relative_path} pulls {match.group(0)} directly from Docker Hub"
                    )

    def test_pinned_images_keep_their_digests(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text(encoding="utf-8")
        busybox_runner = (
            ROOT / ".github/scripts/run-busybox-phase-check.sh"
        ).read_text(encoding="utf-8")
        node_dockerfile = (
            ROOT / "test/e2e/real_server_fixture/Dockerfile"
        ).read_text(encoding="utf-8")
        compose = (ROOT / "docker-compose.yml").read_text(encoding="utf-8")
        self.assertIn(
            r'^mirror\\.gcr\\.io/rustfs/rustfs:1\\.0\\.1@sha256:[0-9a-f]{64}$',
            workflow,
        )
        self.assertIn(
            "mirror.gcr.io/library/node:24-alpine3.22@sha256:"
            "191c9f0080fcbbc6547a85dc0ff7988072214a355aabdc1d2ec55a7dae5eea8a",
            busybox_runner,
        )
        self.assertIn(
            "mirror.gcr.io/library/node@sha256:"
            "5711a0d445a1af54af9589066c646df387d1831a608226f4cd694fc59e745059",
            node_dockerfile,
        )
        self.assertIn(
            "mirror.gcr.io/rustfs/rustfs:1.0.1@sha256:"
            "1803faef57627e2d9c2e7d89d655d712ddded5389040054987163043fecb6a3c",
            compose,
        )


if __name__ == "__main__":
    unittest.main()
