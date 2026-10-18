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
    "deploy/teagramd/mixed-trust-probe.Dockerfile",
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
    "deploy/teagramd/rollout-runner/schema-gate-ci-compose.yml",
)
ACTIVE_PULL_SCRIPTS = (
    ".github/workflows/ci.yml",
    ".github/scripts/run-busybox-phase-check.sh",
    ".github/scripts/smoke-mixed-trust-compose.sh",
    "deploy/teagramd/rollout-runner/README.md",
    "deploy/teagramd/rollout-runner/qualify-rustfs-transition.py",
    "deploy/teagramd/rollout-runner/test-rustfs-schema-postgres.sh",
    "deploy/teagramd/rollout-runner/test-isolated-restore-postgres.sh",
    "deploy/teagramd/rollout-runner/test-stack-move-runner.sh",
    "test/e2e/real_server_fixture/start.sh",
)
DIRECT_HUB_IMAGE = re.compile(
    r"(?<![A-Za-z0-9._/-])(?:"
    r"golang:1\.27|golang:1\.26|golang:1\.24\.8-alpine|alpine:3\.22|"
    r"node:24-alpine3\.22@sha256:[0-9a-f]{64}|node@sha256:[0-9a-f]{64}|"
    r"postgres:16-alpine|postgres:18-alpine|arigaio/atlas:1\.2\.0-alpine|"
    r"rustfs/rustfs:1\.0\.1@sha256:[0-9a-f]{64}|"
    r"haproxy:3\.2-alpine|busybox:1\.37(?:\.0)?)"
)
EXTERNAL_REGISTRIES = (
    "gcr.io/",
    "ghcr.io/",
    "mcr.microsoft.com/",
    "mirror.gcr.io/",
    "public.ecr.aws/",
    "quay.io/",
)
RUSTFS_QUALIFICATION_PULL_SCRIPTS = (
    "deploy/telegramd/rollout-runner/test-rustfs-schema-postgres.sh",
    "deploy/telegramd/rollout-runner/test-isolated-restore-postgres.sh",
    "deploy/telegramd/rollout-runner/test-stack-move-runner.sh",
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

    def test_rustfs_qualification_pulls_avoid_docker_hub(self) -> None:
        for relative_path in RUSTFS_QUALIFICATION_PULL_SCRIPTS:
            with self.subTest(path=relative_path):
                content = (ROOT / relative_path).read_text(encoding="utf-8")
                self.assertNotIn(
                    "docker.io/",
                    content,
                    f"{relative_path} pulls directly from Docker Hub",
                )
                self.assertNotIn(
                    "mirror.gcr.io/",
                    content,
                    f"{relative_path} pulls from the Docker Hub mirror",
                )
                self.assertNotIn(
                    "public.ecr.aws/",
                    content,
                    f"{relative_path} pulls from ECR Public, which exceeded its data limit in CI",
                )

        workflow = (ROOT / ".github/workflows/ci.yml").read_text(encoding="utf-8")
        job = re.search(
            r"(?ms)^  rustfs-transition-qualification:\n(.*?)(?=^  [a-z0-9-]+:|\Z)",
            workflow,
        )
        self.assertIsNotNone(job, "RustFS qualification job is missing")
        self.assertIn("uses: ariga/setup-atlas@v0.3", job.group(1))
        self.assertIn("version: v1.2.0", job.group(1))

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
        postgres_schema = (
            ROOT / "deploy/telegramd/rollout-runner/test-rustfs-schema-postgres.sh"
        ).read_text(encoding="utf-8")
        isolated_restore = (
            ROOT / "deploy/telegramd/rollout-runner/test-isolated-restore-postgres.sh"
        ).read_text(encoding="utf-8")
        stack_move = (
            ROOT / "deploy/telegramd/rollout-runner/test-stack-move-runner.sh"
        ).read_text(encoding="utf-8")
        self.assertIn(
            "ghcr.io/immich-app/postgres:16-vectorchord1.1.1@sha256:"
            "0446995e2b4713ae45d25de7c81c745a63180491018ded84cf13ef4d3c0c5cc9",
            postgres_schema,
        )
        self.assertIn(
            "ghcr.io/immich-app/postgres:18-vectorchord1.1.1@sha256:"
            "a20e21841b21b9c009daf572c844ffedfaef75fce51287bbf080edf171ef0834",
            isolated_restore,
        )
        self.assertIn(
            "ghcr.io/spacelift-io/aws-cli-alpine:2.36.48@sha256:"
            "7cda53b1da87e5c6789f653d14e8a04f92f39ee539d0288ab5407d112cb922f7",
            stack_move,
        )


if __name__ == "__main__":
    unittest.main()
