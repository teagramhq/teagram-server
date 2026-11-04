#!/usr/bin/env python3
"""Keep every Atlas pin in the repository on one version."""

from __future__ import annotations

import re
import subprocess
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
# Rollout artifacts are sha256-pinned by their own tests and stay on the
# version they were qualified with until they are regenerated.
FROZEN_ROLLOUT_ARTIFACTS = re.compile(
    r"^deploy/teagramd/rollout-runner/(?:(?:initial-)?local-compose-[0-9a-f]+\.yml"
    r"|test-local-compose-[0-9a-f]+\.sh"
    r"|test-qualify-rustfs-transition\.py)$"
)
IMAGE_PIN = re.compile(r"arigaio/atlas:(\d+\.\d+\.\d+)-alpine")
ESCAPED_IMAGE_PIN = re.compile(r"arigaio/atlas:(\d+\\\.\d+\\\.\d+)-alpine")
LINT_PIN = re.compile(r"ATLAS_VERSION=v?(\d+\.\d+\.\d+)")
BINARY_PIN = re.compile(r"atlas version v(\d+\.\d+\.\d+)")
SETUP_ATLAS = re.compile(r"ariga/setup-atlas@")
SETUP_VERSION = re.compile(r"^\s*version:\s*v?(\d+\.\d+\.\d+)\s*$")
EXPECTED_VERSION = "1.3.0"
REQUIRED_PIN_FILES = (
    "docker-compose.yml",
    "deploy/k8s/Dockerfile.migrations",
    "deploy/teagram-demo/compose.yaml",
    "deploy/qa-invite/compose.yaml",
    "deploy/teagramd/rollout-runner/schema-gate-ci-compose.yml",
    ".github/workflows/ci.yml",
    ".github/workflows/perf.yml",
)


def tracked_files() -> list[str]:
    output = subprocess.run(
        ["git", "ls-files", "-z"],
        cwd=ROOT,
        check=True,
        capture_output=True,
    ).stdout
    return [path for path in output.decode("utf-8").split("\0") if path]


def collect_pins() -> list[tuple[str, str, str]]:
    """Return (path, version, source) for every Atlas version pin in the repo."""
    pins: list[tuple[str, str, str]] = []
    for relative_path in tracked_files():
        if FROZEN_ROLLOUT_ARTIFACTS.match(relative_path):
            continue
        if relative_path.startswith(".github/scripts/test_atlas_version_pins"):
            continue
        path = ROOT / relative_path
        if not path.is_file():
            continue
        try:
            text = path.read_text(encoding="utf-8")
        except UnicodeDecodeError:
            continue
        for pattern, source in (
            (IMAGE_PIN, "image"),
            (ESCAPED_IMAGE_PIN, "image"),
            (LINT_PIN, "lint"),
            (BINARY_PIN, "binary"),
        ):
            for match in pattern.finditer(text):
                version = match.group(1).replace(r"\.", ".")
                pins.append((relative_path, version, source))
        if not relative_path.startswith(".github/workflows/"):
            continue
        lines = text.splitlines()
        for index, line in enumerate(lines):
            if not SETUP_ATLAS.search(line):
                continue
            for follow in lines[index + 1 : index + 4]:
                version = SETUP_VERSION.match(follow)
                if version is not None:
                    pins.append((relative_path, version.group(1), "setup-atlas"))
                    break
            else:
                pins.append((relative_path, "missing", "setup-atlas"))
    return pins


class AtlasVersionPinTests(unittest.TestCase):
    def test_every_atlas_pin_uses_one_version(self) -> None:
        pins = collect_pins()
        mismatched = [pin for pin in pins if pin[1] != EXPECTED_VERSION]
        self.assertEqual(
            [],
            mismatched,
            "Atlas pins must all name "
            f"{EXPECTED_VERSION}: {mismatched}",
        )

    def test_scans_escaped_mirror_image_pin(self) -> None:
        self.assertIn(
            (".github/scripts/test_docker_image_mirror.py", EXPECTED_VERSION, "image"),
            collect_pins(),
        )

    def test_scan_covers_compose_dockerfile_and_ci_pins(self) -> None:
        pinned_files = {path for path, _, _ in collect_pins()}
        for relative_path in REQUIRED_PIN_FILES:
            with self.subTest(path=relative_path):
                self.assertIn(relative_path, pinned_files)

    def test_setup_atlas_steps_are_all_pinned(self) -> None:
        missing = [pin for pin in collect_pins() if pin[1] == "missing"]
        self.assertEqual([], missing, "every ariga/setup-atlas step needs a version")


if __name__ == "__main__":
    unittest.main()
