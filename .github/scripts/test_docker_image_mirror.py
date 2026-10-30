#!/usr/bin/env python3
"""Keep CI Docker Hub pulls routed through the public Google mirror."""

from __future__ import annotations

import os
import re
import subprocess
import tempfile
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
    ".github/scripts/mirror_rustfs_qualification_images.sh",
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
    r"postgres:1[68]-alpine|arigaio/atlas:1\.2\.0-alpine|"
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
    "deploy/teagramd/rollout-runner/test-rustfs-schema-postgres.sh",
    "deploy/teagramd/rollout-runner/test-isolated-restore-postgres.sh",
    "deploy/teagramd/rollout-runner/test-stack-move-runner.sh",
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
    def test_ci_server_images_use_the_validated_sticker_bundle_context(self) -> None:
        dockerfile = (ROOT / "Dockerfile").read_text(encoding="utf-8")
        workflow = (ROOT / ".github/workflows/ci.yml").read_text(encoding="utf-8")

        self.assertRegex(
            dockerfile,
            r"(?m)^ARG STICKER_BUNDLE_SOURCE=sticker-bundle-generator$",
        )
        self.assertRegex(
            dockerfile,
            r"(?m)^FROM \$\{STICKER_BUNDLE_SOURCE\} AS sticker-bundle-source$",
        )
        build_stage = re.search(r"(?ms)^FROM .* AS build\n(.*?)(?=^FROM )", dockerfile)
        self.assertIsNotNone(build_stage, "Go build stage is missing")
        self.assertNotIn("COPY --from=sticker-bundle-source", build_stage.group(1))
        self.assertIn("RUN install -d -m 0755 /rootfs/var /rootfs/var/lib", build_stage.group(1))

        image_stage = re.search(
            r"(?ms)^FROM build AS server-image-build\n(.*?)(?=^FROM )", dockerfile
        )
        self.assertIsNotNone(image_stage, "validated server image stage is missing")
        self.assertIn(
            "COPY --from=sticker-bundle-source /sticker-bundle/ /sticker-bundle/",
            image_stage.group(1),
        )
        self.assertIn("RUN /stickerbundle-check /sticker-bundle", image_stage.group(1))
        self.assertIn(
            "COPY --from=server-image-build /sticker-bundle/ "
            "/usr/share/teagram/sticker-bundle/",
            dockerfile,
        )

        context_steps = [
            step
            for step in re.split(r"(?m)^      - ", workflow)
            if "uses: docker/build-push-action@v7.4.0" in step
            and "build-contexts: sticker-bundle=build" in step
        ]
        self.assertEqual(4, len(context_steps))
        for step in context_steps:
            with self.subTest(step=step.splitlines()[0]):
                self.assertIn("STICKER_BUNDLE_SOURCE=sticker-bundle", step)

        photo_job = re.search(
            r"(?ms)^  test-photo-worker:\n(.*?)(?=^  [a-z0-9-]+:|\Z)",
            workflow,
        )
        self.assertIsNotNone(photo_job, "Photo worker runtime job is missing")
        job = photo_job.group(1)
        self.assertRegex(
            job,
            r"(?m)^    needs:\n      - ops-prioritize-main\n      - check-sticker-license$",
        )
        self.assertIn("needs.ops-prioritize-main.result == 'success'", job)
        self.assertIn("needs.ops-prioritize-main.result == 'skipped'", job)
        self.assertIn("github.event_name == 'pull_request'", job)
        self.assertIn("needs.check-sticker-license.result == 'success'", job)
        self.assertIn("actions: read", job)
        self.assertIn("path: build/sticker-bundle", job)
        self.assertIn("uses: docker/setup-buildx-action@v4.4.1", job)
        self.assertIn("docker buildx build", job)
        self.assertIn("--build-context sticker-bundle=build", job)
        self.assertIn("--build-arg STICKER_BUNDLE_SOURCE=sticker-bundle", job)

    def test_testcontainers_prefix_uses_mirror_without_trailing_slash(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text(encoding="utf-8")
        match = re.search(
            r"(?m)^  TESTCONTAINERS_HUB_IMAGE_NAME_PREFIX:\s*(\S+)\s*$", workflow
        )
        self.assertIsNotNone(match, "CI has no Testcontainers registry prefix")
        self.assertEqual("mirror.gcr.io", match.group(1))

    def test_compose_e2e_reuses_the_loaded_testcontainers_postgres_image(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text(encoding="utf-8")
        compose_job = re.search(
            r"(?ms)^  test-compose:\n(.*?)(?=^  [a-z0-9-]+:|\Z)", workflow
        )
        self.assertIsNotNone(compose_job, "Compose job is missing")
        job = compose_job.group(1)
        alias = (
            "docker image tag mirror.gcr.io/library/postgres:18-alpine "
            "mirror.gcr.io/postgres:18-alpine"
        )
        e2e = (
            "go test -race -count=1 -timeout 15m "
            "./test/e2e -run '^TestMediaRoundTrip$'"
        )
        self.assertLess(job.index(alias), job.index(e2e))

    def test_compose_rustfs_lane_uses_digest_pinned_ghcr_image(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text(encoding="utf-8")
        compose_job = re.search(
            r"(?ms)^  test-compose:\n(.*?)(?=^  [a-z0-9-]+:|\Z)", workflow
        )
        self.assertIsNotNone(compose_job, "Compose job is missing")
        job = compose_job.group(1)
        image = (
            "ghcr.io/rustfs/rustfs@sha256:"
            "1803faef57627e2d9c2e7d89d655d712ddded5389040054987163043fecb6a3c"
        )
        self.assertIn('docker pull "$rustfs_image"', job)
        self.assertIn(f"image: {image}", job)
        self.assertIn(
            "export COMPOSE_FILE=docker-compose.yml:docker-compose.local-blobs.yml:"
            "$RUNNER_TEMP/rustfs-ci-image.yml",
            job,
        )
        self.assertLess(
            job.index('docker pull "$rustfs_image"'),
            job.index("compose up -d postgres migrate teagramd rustfs rustfs-init"),
        )

    def test_compose_lane_uses_the_pinned_ghcr_busybox_mirror(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text(encoding="utf-8")
        compose_job = re.search(
            r"(?ms)^  test-compose:\n(.*?)(?=^  [a-z0-9-]+:|\Z)", workflow
        )
        self.assertIsNotNone(compose_job, "Compose job is missing")
        job = compose_job.group(1)
        image = (
            "ghcr.io/teagramhq/teagram-server-busybox:1.37@sha256:"
            "bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
        )
        self.assertRegex(
            job,
            r"(?m)^    needs:\n      - ops-prioritize-main\n      - mirror-rustfs-qualification-images\n      - check-sticker-license$",
        )
        self.assertIn("needs.ops-prioritize-main.result == 'success'", job)
        self.assertIn("needs.ops-prioritize-main.result == 'skipped'", job)
        self.assertIn("github.event_name == 'pull_request'", job)
        self.assertIn("needs.check-sticker-license.result == 'success'", job)
        self.assertIn("packages: read", job)
        self.assertIn("docker login ghcr.io", job)
        self.assertIn(f"BUSYBOX_IMAGE: {image}", job)
        self.assertIn('docker pull "$BUSYBOX_IMAGE"', job)
        self.assertIn('docker run --rm -v "$1":/blobs "$BUSYBOX_IMAGE"', job)
        self.assertNotIn("mirror.gcr.io/library/busybox:1.37", job)

    def test_dockerfile_hub_bases_use_the_mirror(self) -> None:
        for relative_path in ACTIVE_DOCKERFILES:
            with self.subTest(path=relative_path):
                dockerfile = (ROOT / relative_path).read_text(encoding="utf-8")
                stages: set[str] = set()
                for line in dockerfile.splitlines():
                    fields = line.strip().split()
                    if len(fields) < 2 or fields[0] != "FROM":
                        continue
                    image = fields[2] if fields[1].startswith("--") else fields[1]
                    if image not in stages:
                        self.assertFalse(
                            is_unqualified_hub_image(image),
                            f"{relative_path} pulls {image} directly from Docker Hub",
                        )
                    if len(fields) >= 4 and fields[-2].lower() == "as":
                        stages.add(fields[-1])

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

    def test_rustfs_schema_fixture_waits_for_postgres_tcp(self) -> None:
        script = (
            ROOT / "deploy/teagramd/rollout-runner/test-rustfs-schema-postgres.sh"
        ).read_text(encoding="utf-8")
        self.assertIn(
            "--health-cmd 'pg_isready -h 127.0.0.1 -U postgres -d teagram'",
            script,
        )

    def test_rpc_performance_postgres_waits_for_final_tcp_server(self) -> None:
        script = (ROOT / ".github/scripts/perf-postgres.sh").read_text(
            encoding="utf-8"
        )
        self.assertIn(
            'pg_isready -h 127.0.0.1 -p 5432 -U postgres -q',
            script,
        )

    def test_rustfs_fixture_mirror_is_digest_pinned_and_precedes_qualification(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text(encoding="utf-8")
        mirror_job = re.search(
            r"(?ms)^  mirror-rustfs-qualification-images:\n(.*?)(?=^  [a-z0-9-]+:|\Z)",
            workflow,
        )
        qualification_job = re.search(
            r"(?ms)^  rustfs-transition-qualification:\n(.*?)(?=^  [a-z0-9-]+:|\Z)",
            workflow,
        )
        self.assertIsNotNone(mirror_job, "RustFS fixture mirror job is missing")
        self.assertIsNotNone(qualification_job, "RustFS qualification job is missing")
        self.assertIn("packages: write", mirror_job.group(1))
        self.assertIn("packages: read", qualification_job.group(1))
        self.assertIn(
            "needs:\n      - ops-prioritize-main\n      - mirror-rustfs-qualification-images",
            qualification_job.group(1),
        )
        self.assertIn("needs.ops-prioritize-main.result == 'success'", qualification_job.group(1))
        self.assertIn("needs.ops-prioritize-main.result == 'skipped'", qualification_job.group(1))
        self.assertIn("github.event_name == 'pull_request'", qualification_job.group(1))
        self.assertIn("docker login ghcr.io", qualification_job.group(1))

        mirror_script = (
            ROOT / ".github/scripts/mirror_rustfs_qualification_images.sh"
        ).read_text(encoding="utf-8")
        self.assertIn("skopeo copy --all --preserve-digests", mirror_script)
        for image in (
            "mirror.gcr.io/library/postgres@sha256:"
            "721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea",
            "ghcr.io/teagramhq/teagram-server-postgres:16-alpine",
            "mirror.gcr.io/library/postgres@sha256:"
            "77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873",
            "ghcr.io/teagramhq/teagram-server-postgres:18-alpine",
            "mirror.gcr.io/library/busybox@sha256:"
            "bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e",
            "ghcr.io/teagramhq/teagram-server-busybox:1.37",
        ):
            self.assertIn(image, mirror_script)

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
            "docker.io/library/node:24-alpine3.22@sha256:"
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
            ROOT / "deploy/teagramd/rollout-runner/test-rustfs-schema-postgres.sh"
        ).read_text(encoding="utf-8")
        isolated_restore = (
            ROOT / "deploy/teagramd/rollout-runner/test-isolated-restore-postgres.sh"
        ).read_text(encoding="utf-8")
        stack_move = (
            ROOT / "deploy/teagramd/rollout-runner/test-stack-move-runner.sh"
        ).read_text(encoding="utf-8")
        postgres16 = "ghcr.io/teagramhq/teagram-server-postgres:16-alpine"
        postgres16_digest = (
            "sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea"
        )
        postgres18 = "ghcr.io/teagramhq/teagram-server-postgres:18-alpine"
        postgres18_digest = (
            "sha256:77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873"
        )
        busybox = "ghcr.io/teagramhq/teagram-server-busybox:1.37"
        busybox_digest = (
            "sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
        )
        self.assertIn(
            postgres18,
            postgres_schema,
        )
        self.assertIn(f'POSTGRES_IMAGE_DIGEST={postgres18_digest}', postgres_schema)
        self.assertIn('docker pull "$POSTGRES_IMAGE@$POSTGRES_IMAGE_DIGEST"', postgres_schema)
        self.assertIn(
            'docker image tag "$POSTGRES_IMAGE_ID" "$POSTGRES_IMAGE"',
            postgres_schema,
        )
        self.assertIn(
            postgres16,
            isolated_restore,
        )
        self.assertIn(f'SOURCE_POSTGRES_IMAGE_DIGEST={postgres16_digest}', isolated_restore)
        self.assertIn(f'RESTORE_POSTGRES_IMAGE_DIGEST={postgres18_digest}', isolated_restore)
        self.assertIn(
            'docker pull "$SOURCE_POSTGRES_IMAGE@$SOURCE_POSTGRES_IMAGE_DIGEST"',
            isolated_restore,
        )
        self.assertIn(
            'docker pull "$RESTORE_POSTGRES_IMAGE@$RESTORE_POSTGRES_IMAGE_DIGEST"',
            isolated_restore,
        )
        self.assertIn(
            'docker image tag "$SOURCE_POSTGRES_IMAGE_ID" "$SOURCE_POSTGRES_IMAGE"',
            isolated_restore,
        )
        self.assertIn(
            'docker image tag "$RESTORE_POSTGRES_IMAGE_ID" "$RESTORE_POSTGRES_IMAGE"',
            isolated_restore,
        )
        self.assertIn(postgres16, stack_move)
        self.assertIn(postgres18, isolated_restore)
        self.assertIn(postgres18, stack_move)
        self.assertIn(busybox, stack_move)
        self.assertIn(f'SOURCE_POSTGRES_IMAGE_DIGEST={postgres16_digest}', stack_move)
        self.assertIn(f'TARGET_POSTGRES_IMAGE_DIGEST={postgres18_digest}', stack_move)
        self.assertIn(f'BUSYBOX_IMAGE_DIGEST={busybox_digest}', stack_move)
        self.assertIn(
            'docker pull "$SOURCE_POSTGRES_IMAGE@$SOURCE_POSTGRES_IMAGE_DIGEST"',
            stack_move,
        )
        self.assertIn(
            'docker pull "$TARGET_POSTGRES_IMAGE@$TARGET_POSTGRES_IMAGE_DIGEST"',
            stack_move,
        )
        self.assertIn('docker pull "$BUSYBOX_IMAGE@$BUSYBOX_IMAGE_DIGEST"', stack_move)
        self.assertIn("command -v aws >/dev/null", stack_move)
        self.assertIn(
            'docker image tag "$BUSYBOX_IMAGE_ID" "$BUSYBOX_IMAGE"', stack_move
        )
        self.assertIn(
            'docker image tag "$SOURCE_POSTGRES_IMAGE_ID" "$SOURCE_POSTGRES_IMAGE"',
            stack_move,
        )
        self.assertIn(
            'docker image tag "$TARGET_POSTGRES_IMAGE_ID" "$TARGET_POSTGRES_IMAGE"',
            stack_move,
        )

    def test_busybox_phase_check_falls_back_when_mirror_pull_fails(self) -> None:
        script = ROOT / ".github/scripts/run-busybox-phase-check.sh"
        mirror_image = (
            "mirror.gcr.io/library/node:24-alpine3.22@sha256:"
            "191c9f0080fcbbc6547a85dc0ff7988072214a355aabdc1d2ec55a7dae5eea8a"
        )
        hub_image = (
            "docker.io/library/node:24-alpine3.22@sha256:"
            "191c9f0080fcbbc6547a85dc0ff7988072214a355aabdc1d2ec55a7dae5eea8a"
        )
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            bin_dir = temp / "bin"
            runner_temp = temp / "runner"
            workspace = temp / "workspace"
            bin_dir.mkdir()
            runner_temp.mkdir()
            workspace.mkdir()
            calls = temp / "docker-calls"
            fake_docker = bin_dir / "docker"
            fake_docker.write_text(
                """#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  pull)
    printf 'pull %s\\n' "$2" >> "$FAKE_DOCKER_CALLS"
    [[ "$2" != "$FAKE_DOCKER_FAIL_IMAGE" ]]
    ;;
  run)
    printf 'run %s\\n' "${9}" >> "$FAKE_DOCKER_CALLS"
    ;;
  *) exit 64 ;;
esac
""",
                encoding="utf-8",
            )
            fake_docker.chmod(0o755)
            result = subprocess.run(
                ["bash", str(script), "compose-path"],
                env={
                    **os.environ,
                    "PATH": f"{bin_dir}{os.pathsep}{os.environ['PATH']}",
                    "RUNNER_TEMP": str(runner_temp),
                    "GITHUB_WORKSPACE": str(workspace),
                    "FAKE_DOCKER_CALLS": str(calls),
                    "FAKE_DOCKER_FAIL_IMAGE": mirror_image,
                },
                capture_output=True,
                text=True,
                timeout=60,
                check=False,
            )

            self.assertEqual(0, result.returncode, result.stdout + result.stderr)
            self.assertEqual(
                [f"pull {mirror_image}", f"pull {hub_image}", f"run {hub_image}"],
                calls.read_text(encoding="utf-8").splitlines(),
            )
            self.assertIn("pinned image mirror pull failed", result.stdout)


class PerformancePostgresReadinessTests(unittest.TestCase):
    def test_readiness_probe_waits_for_postgres_tcp_listener(self) -> None:
        script = (ROOT / ".github/scripts/perf-postgres.sh").read_text(
            encoding="utf-8"
        )
        self.assertIn(
            'docker exec "$PERF_PG_CONTAINER" pg_isready -h 127.0.0.1 -p 5432 -U postgres -q',
            script,
        )


class PerformanceFixtureRestoreTests(unittest.TestCase):
    def test_restore_omits_acls_that_reference_cluster_roles(self) -> None:
        script = (ROOT / ".github/scripts/perf-run.sh").read_text(encoding="utf-8")
        restore = re.search(r"(?ms)^restore\(\) \{\n(.*?)(?=^\})", script)
        self.assertIsNotNone(restore, "Performance fixture restore function is missing")
        self.assertIn("--no-acl", restore.group(1))


if __name__ == "__main__":
    unittest.main()
