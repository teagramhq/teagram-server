#!/usr/bin/env python3
"""Run pull-base-images.sh against a fake docker that fails on demand."""

from __future__ import annotations

import os
import re
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / ".github/scripts/pull-base-images.sh"
REGISTRY_ERROR = (
    "failed to copy: httpReadSeeker: failed open: could not fetch content "
    "descriptor sha256:a613 from remote: not found"
)
FAKE_DOCKER = """#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  pull)
    printf '%s\\n' "$2" >> "$FAKE_DOCKER_DIR/calls"
    fail_image="${FAKE_DOCKER_FAIL_IMAGE:-}"
    if [ -z "$fail_image" ] || [ "$2" = "$fail_image" ]; then
      failures=$(grep -cxF "$2" "$FAKE_DOCKER_DIR/calls")
      if [ "$failures" -le "$FAKE_DOCKER_FAILURES" ]; then
        echo "Error response from daemon: $FAKE_DOCKER_ERROR" >&2
        exit 1
      fi
    fi
    echo "pulled $2"
    ;;
  tag)
    printf 'tag %s %s\\n' "$2" "$3" >> "$FAKE_DOCKER_DIR/calls"
    ;;
  *)
    echo "unexpected docker $*" >&2
    exit 64
    ;;
esac
"""
FAKE_POSTGRES_DOCKER = """#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  pull) exit 0 ;;
  run) echo fake-container-id ;;
  exec)
    if [ "$3" = pg_isready ]; then
      if [ "${4:-}" = -h ] && [ "${5:-}" = 127.0.0.1 ]; then
        printf 'tcp\\n' >> "$FAKE_DOCKER_DIR/probes"
        [ "$(wc -l < "$FAKE_DOCKER_DIR/probes")" -ge 3 ]
      else
        printf 'socket\\n' >> "$FAKE_DOCKER_DIR/probes"
        exit 0
      fi
    elif [ "$3" = psql ] && [[ "$*" == *"CREATE ROLE browser_acceptance_probe NOLOGIN"* ]]; then
      [ "$(wc -l < "$FAKE_DOCKER_DIR/probes")" -ge 3 ] || {
        echo "role created before TCP readiness" >&2
        exit 64
      }
      printf 'role\\n' >> "$FAKE_DOCKER_DIR/probes"
      printf '%s\\n' "$*" >> "$FAKE_DOCKER_DIR/sql"
    else
      echo "unexpected docker $*" >&2
      exit 64
    fi
    ;;
  *) echo "unexpected docker $*" >&2; exit 64 ;;
esac
"""
DOCKERFILE = """\
FROM --platform=$BUILDPLATFORM mirror.gcr.io/library/golang:1.27 AS build
FROM mirror.gcr.io/library/golang:1.27 AS probe-build
FROM build AS tools
FROM gcr.io/distroless/static-debian12:nonroot
"""
IMAGES = [
    "gcr.io/distroless/static-debian12:nonroot",
    "mirror.gcr.io/library/golang:1.27",
]


class PullBaseImagesTests(unittest.TestCase):
    def setUp(self) -> None:
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.dir = Path(temp.name)
        bin_dir = self.dir / "bin"
        bin_dir.mkdir()
        docker = bin_dir / "docker"
        docker.write_text(FAKE_DOCKER, encoding="utf-8")
        docker.chmod(0o755)
        (self.dir / "calls").touch()
        self.dockerfile = self.dir / "Dockerfile"
        self.dockerfile.write_text(DOCKERFILE, encoding="utf-8")
        self.env = {
            **os.environ,
            "PATH": f"{bin_dir}{os.pathsep}{os.environ['PATH']}",
            "FAKE_DOCKER_DIR": str(self.dir),
            "FAKE_DOCKER_ERROR": REGISTRY_ERROR,
            "PULL_RETRY_DELAY_SECONDS": "0",
        }

    def run_script(
        self, failures: int, *dockerfiles: Path
    ) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            ["bash", str(SCRIPT), *map(str, dockerfiles or (self.dockerfile,))],
            env={**self.env, "FAKE_DOCKER_FAILURES": str(failures)},
            capture_output=True,
            text=True,
            timeout=60,
            check=False,
        )

    def run_image(
        self, failures: int, image: str, fail_image: str = ""
    ) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            ["bash", str(SCRIPT), "--image", image],
            env={
                **self.env,
                "FAKE_DOCKER_FAILURES": str(failures),
                "FAKE_DOCKER_FAIL_IMAGE": fail_image,
            },
            capture_output=True,
            text=True,
            timeout=60,
            check=False,
        )

    def calls(self) -> list[str]:
        return (self.dir / "calls").read_text(encoding="utf-8").splitlines()

    def test_pulls_each_external_base_image_once(self) -> None:
        result = self.run_script(failures=0)
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual(IMAGES, self.calls())
        self.assertNotIn("::warning::", result.stdout)

    def test_transient_failures_recover(self) -> None:
        result = self.run_script(failures=3)
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        self.assertEqual([image for image in IMAGES for _ in range(4)], self.calls())
        self.assertEqual(3 * len(IMAGES), result.stderr.count(REGISTRY_ERROR))
        for image in IMAGES:
            for attempt in (1, 2, 3):
                self.assertIn(
                    f"::warning::pull of {image} failed (attempt {attempt}); retrying",
                    result.stdout,
                )

    def test_explicit_image_pull_retries_transient_failures(self) -> None:
        image = "mirror.gcr.io/postgres:18-alpine"
        result = self.run_image(failures=2, image=image)
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        self.assertEqual([image] * 3, self.calls())
        self.assertEqual(2, result.stderr.count(REGISTRY_ERROR))
        for attempt in (1, 2):
            self.assertIn(
                f"::warning::pull of {image} failed (attempt {attempt}); retrying",
                result.stdout,
            )

    def test_broken_mirror_falls_back_to_docker_hub_and_tags_image(self) -> None:
        cases = (
            ("mirror.gcr.io/library/postgres:18-alpine", "postgres:18-alpine"),
            (
                "mirror.gcr.io/arigaio/atlas:1.2.0-alpine",
                "arigaio/atlas:1.2.0-alpine",
            ),
        )
        for image, fallback in cases:
            with self.subTest(image=image):
                (self.dir / "calls").write_text("", encoding="utf-8")
                result = self.run_image(
                    failures=4, image=image, fail_image=image
                )
                self.assertEqual(0, result.returncode, result.stdout + result.stderr)
                self.assertEqual(
                    [image] * 4 + [fallback, f"tag {fallback} {image}"],
                    self.calls(),
                )
                self.assertIn(
                    f"::warning::mirror pull of {image} failed; trying Docker Hub source {fallback}",
                    result.stdout,
                )

    def test_pinned_mirror_image_does_not_use_invalid_tag_fallback(self) -> None:
        image = "mirror.gcr.io/library/node@sha256:5711"
        result = self.run_image(failures=4, image=image, fail_image=image)
        self.assertNotEqual(0, result.returncode)
        self.assertEqual([image] * 4, self.calls())
        self.assertIn(f"::error::pull of {image} failed after 4 attempts", result.stdout)

    def test_testcontainers_jobs_preload_postgres_with_retry(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text(encoding="utf-8")
        command = re.compile(
            r"run: (?:>- )?bash \.github/scripts/pull-base-images\.sh "
            r"--image mirror\.gcr\.io/postgres:18-alpine"
        )
        for job_name in ("test-go", "test-smoke", "test-e2e-admin", "test-real-server-fixtures"):
            with self.subTest(job=job_name):
                job = re.search(
                    rf"(?ms)^  {re.escape(job_name)}:\n(.*?)(?=^  [a-z0-9-]+:|\Z)",
                    workflow,
                )
                self.assertIsNotNone(job, f"{job_name} job is missing")
                self.assertRegex(" ".join(job.group(1).split()), command)

    def test_ci_does_not_reference_deleted_minio_test_dockerfile(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text(encoding="utf-8")
        self.assertNotIn("Dockerfile.minio-test", workflow)
        self.assertNotIn("Build MinIO image for tests", workflow)

    def test_compose_job_preloads_images_through_the_fallback_helper(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text(encoding="utf-8")
        job = re.search(
            r"(?ms)^  test-compose:\n(.*?)(?=^  [a-z0-9-]+:|\Z)", workflow
        )
        self.assertIsNotNone(job, "compose job is missing")
        body = job.group(1)
        self.assertIn("- name: Preload Compose service images", body)
        for image in (
            "mirror.gcr.io/library/postgres:18-alpine",
            "mirror.gcr.io/arigaio/atlas:1.2.0-alpine",
        ):
            with self.subTest(image=image):
                self.assertIn(f"--image {image}", body)
        self.assertLess(
            body.index("Preload Compose service images"),
            body.index("docker compose"),
        )

    def test_persistent_failure_fails_with_the_registry_error(self) -> None:
        result = self.run_script(failures=4)
        self.assertNotEqual(0, result.returncode)
        self.assertEqual([IMAGES[0]] * 4, self.calls())
        self.assertEqual(4, result.stderr.count(REGISTRY_ERROR))
        self.assertIn(
            f"::error::pull of {IMAGES[0]} failed after 4 attempts", result.stdout
        )

    def test_real_client_workflow_pulls_fixture_bases_before_starting(self) -> None:
        workflow = (
            ROOT / ".github/workflows/real-client-browser-flow.yml"
        ).read_text(encoding="utf-8")
        pull_step = (
            "run: bash .github/scripts/pull-base-images.sh "
            "--image mirror.gcr.io/library/postgres:18-alpine "
            "--image mirror.gcr.io/arigaio/atlas:1.2.0-alpine "
            "test/e2e/real_server_fixture/Dockerfile"
        )
        fixture_step = (
            "run: node test/e2e/real_server_fixture/"
            "run_real_client_browser_flow.mjs"
        )

        self.assertIn(pull_step, workflow)
        self.assertLess(workflow.index(pull_step), workflow.index(fixture_step))

    def test_dockerfile_without_base_images_fails(self) -> None:
        self.dockerfile.write_text("RUN true\n", encoding="utf-8")
        result = self.run_script(failures=0)
        self.assertNotEqual(0, result.returncode)
        self.assertEqual([], self.calls())

    def test_pulls_base_images_of_every_dockerfile_once(self) -> None:
        other = self.dir / "Dockerfile.other"
        other.write_text(
            "FROM mirror.gcr.io/library/golang:1.27 AS other\n"
            "FROM build\n"
            "FROM mirror.gcr.io/library/alpine:3.22\n",
            encoding="utf-8",
        )
        result = self.run_script(0, self.dockerfile, other)
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual(
            sorted([*IMAGES, "build", "mirror.gcr.io/library/alpine:3.22"]),
            self.calls(),
        )

    def test_skips_bases_named_by_build_arguments(self) -> None:
        self.dockerfile.write_text(
            "ARG PLAYWRIGHT_IMAGE\n"
            "FROM mirror.gcr.io/library/node@sha256:5711 AS node\n"
            "FROM ${PLAYWRIGHT_IMAGE}\n",
            encoding="utf-8",
        )
        result = self.run_script(failures=0)
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual(["mirror.gcr.io/library/node@sha256:5711"], self.calls())

    def test_missing_dockerfile_fails_before_pulling(self) -> None:
        missing = self.dir / "Dockerfile.missing"
        result = self.run_script(0, self.dockerfile, missing)
        self.assertNotEqual(0, result.returncode)
        self.assertIn(f"::error::{missing} not found", result.stdout)
        self.assertEqual([], self.calls())

    def test_perf_postgres_waits_for_tcp_listener(self) -> None:
        docker = self.dir / "bin/docker"
        docker.write_text(FAKE_POSTGRES_DOCKER, encoding="utf-8")
        docker.chmod(0o755)
        (self.dir / "probes").touch()
        sleep = self.dir / "bin/sleep"
        sleep.write_text("#!/usr/bin/env bash\nexit 0\n", encoding="utf-8")
        sleep.chmod(0o755)
        cache = self.dir / "perf-cache"
        result = subprocess.run(
            ["bash", str(ROOT / ".github/scripts/perf-postgres.sh")],
            env={
                **self.env,
                "PERF_PG_CONTAINER": "perf-pg",
                "PERF_CACHE": str(cache),
            },
            capture_output=True,
            text=True,
            timeout=10,
            check=False,
        )
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        self.assertEqual(
            ["tcp", "tcp", "tcp", "role"],
            (self.dir / "probes").read_text(encoding="utf-8").splitlines(),
        )
        sql = (self.dir / "sql").read_text(encoding="utf-8").splitlines()
        self.assertEqual(1, len(sql))
        self.assertIn(
            "CREATE ROLE browser_acceptance_probe NOLOGIN NOSUPERUSER NOCREATEDB "
            "NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS",
            sql[0],
        )


if __name__ == "__main__":
    unittest.main()
