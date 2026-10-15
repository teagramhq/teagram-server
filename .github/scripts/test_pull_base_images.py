#!/usr/bin/env python3
"""Run pull-base-images.sh against a fake docker that fails on demand."""

from __future__ import annotations

import os
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
[ "$1" = pull ] || { echo "unexpected docker $*" >&2; exit 64; }
printf '%s\\n' "$2" >> "$FAKE_DOCKER_DIR/calls"
failures=$(grep -cxF "$2" "$FAKE_DOCKER_DIR/calls")
if [ "$failures" -le "$FAKE_DOCKER_FAILURES" ]; then
  echo "Error response from daemon: $FAKE_DOCKER_ERROR" >&2
  exit 1
fi
echo "pulled $2"
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

    def test_persistent_failure_fails_with_the_registry_error(self) -> None:
        result = self.run_script(failures=4)
        self.assertNotEqual(0, result.returncode)
        self.assertEqual([IMAGES[0]] * 4, self.calls())
        self.assertEqual(4, result.stderr.count(REGISTRY_ERROR))
        self.assertIn(
            f"::error::pull of {IMAGES[0]} failed after 4 attempts", result.stdout
        )

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


if __name__ == "__main__":
    unittest.main()
