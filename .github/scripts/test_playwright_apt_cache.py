#!/usr/bin/env python3
"""Verify that a Playwright apt retry retains completed package downloads."""

from __future__ import annotations

import os
import re
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
PREPARE_CACHE = ROOT / ".github/scripts/prepare-playwright-apt-cache.sh"
INSTALL_CACHE = ROOT / ".github/scripts/install-playwright-apt-cache.sh"
WORKFLOW = ROOT / ".github/workflows/ci.yml"
REAL_CLIENT_WORKFLOW = ROOT / ".github/workflows/real-client-browser-flow.yml"


FAKE_SUDO = """#!/usr/bin/env bash
set -euo pipefail
command=$1
shift
case "$command" in
  rm|mkdir) exec "/bin/$command" "$@" ;;
  chown) exit 0 ;;
  dpkg) exec dpkg "$@" ;;
  *) echo "unexpected sudo command: $command" >&2; exit 64 ;;
esac
"""

FAKE_DPKG = """#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "--configure" && "${2:-}" == "-a" ]]; then
  exit 0
fi
if [[ "${1:-}" != "--auto-deconfigure" || "${2:-}" != "-i" ]]; then
  echo "version conflict between $RUNNER_PACKAGE_VERSION and $CACHED_PACKAGE_VERSION" >&2
  exit 1
fi
case "${3:-}" in
  *"$CACHED_PACKAGE_VERSION"*_all.deb) ;;
  *) echo "unexpected cached package: ${3:-}" >&2; exit 64 ;;
esac
"""

FAKE_PNPM = """#!/usr/bin/env bash
set -euo pipefail
[[ "$*" == "exec playwright install-deps --dry-run chromium" ]] || exit 64
if [[ "${DEPENDENCIES_SATISFIED:-true}" == "true" ]]; then
  echo "All system dependencies are installed"
else
  echo "Missing system dependencies: libasound2-data"
  exit 1
fi
"""


class PlaywrightAptCacheTests(unittest.TestCase):
    def test_cached_install_handles_version_drift_offline_and_rejects_bad_cache(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory(prefix="playwright-apt-install-") as temporary:
            root = Path(temporary)
            cache = root / ".apt-cache"
            cache.mkdir()
            fake_bin = root / "bin"
            fake_bin.mkdir()
            output = root / "github-output"
            fake_sudo = fake_bin / "sudo"
            fake_sudo.write_text(FAKE_SUDO, encoding="utf-8")
            fake_sudo.chmod(0o755)
            fake_dpkg = fake_bin / "dpkg"
            fake_dpkg.write_text(FAKE_DPKG, encoding="utf-8")
            fake_dpkg.chmod(0o755)
            fake_pnpm = fake_bin / "pnpm"
            fake_pnpm.write_text(FAKE_PNPM, encoding="utf-8")
            fake_pnpm.chmod(0o755)

            cases = (
                ("1.2.15.3-1ubuntu1.1", "1.2.15.3-1ubuntu1.2", "true", "true"),
                ("1.2.15.3-1ubuntu1.2", "1.2.15.3-1ubuntu1.1", "true", "true"),
                ("1.2.15.3-1ubuntu1.1", "1.2.15.3-1ubuntu1.2", "false", "false"),
            )
            for runner_version, cached_version, satisfied, installed in cases:
                with self.subTest(
                    runner_version=runner_version,
                    cached_version=cached_version,
                    satisfied=satisfied,
                ):
                    for package in cache.glob("*.deb"):
                        package.unlink()
                    cached_package = cache / (
                        f"libasound2-data_{cached_version}_all.deb"
                    )
                    cached_package.touch()
                    output.write_text("", encoding="utf-8")
                    result = subprocess.run(
                        ["bash", str(INSTALL_CACHE), str(cache)],
                        env={
                            **os.environ,
                            "PATH": f"{fake_bin}{os.pathsep}{os.environ['PATH']}",
                            "GITHUB_OUTPUT": str(output),
                            "RUNNER_PACKAGE_VERSION": runner_version,
                            "CACHED_PACKAGE_VERSION": cached_version,
                            "DEPENDENCIES_SATISFIED": satisfied,
                        },
                        capture_output=True,
                        text=True,
                        check=False,
                    )

                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(
                        output.read_text(encoding="utf-8"),
                        f"installed={installed}\n",
                    )

    def test_e2e_admin_apt_cache_is_image_scoped_and_warm_path_stays_offline(
        self,
    ) -> None:
        workflow = WORKFLOW.read_text(encoding="utf-8")
        job = re.search(
            r"(?ms)^  test-e2e-admin:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n|\Z)",
            workflow,
        )
        self.assertIsNotNone(job, "e2e-admin job is missing")
        body = job.group("body")

        cache = re.search(
            r"(?ms)^      - name: Cache the Playwright apt dependencies\n"
            r"(?P<body>.*?)(?=^      - name:|\Z)",
            body,
        )
        self.assertIsNotNone(cache, "Playwright apt cache step is missing")
        self.assertIn("path: .apt-cache/*.deb", cache.group("body"))
        self.assertIn("steps.playwright-deps.outputs.image", cache.group("body"))
        self.assertIn("hashFiles('apt-packages.txt')", cache.group("body"))

        cached_install = re.search(
            r"(?ms)^      - name: Install the system dependencies from cache\n"
            r"(?P<body>.*?)(?=^      - name:|\Z)",
            body,
        )
        self.assertIsNotNone(cached_install, "cached dependency install step is missing")
        self.assertIn(
            "if: steps.apt-cache.outputs.cache-hit == 'true'",
            cached_install.group("body"),
        )
        self.assertIn(
            "bash .github/scripts/install-playwright-apt-cache.sh .apt-cache",
            cached_install.group("body"),
        )
        install_script = INSTALL_CACHE.read_text(encoding="utf-8")
        self.assertIn("sudo dpkg --configure -a", install_script)
        self.assertNotIn("apt-get", install_script)

        network_install = re.search(
            r"(?ms)^      - name: Install the system dependencies from the network\n"
            r"(?P<body>.*?)(?=^      - name:|\Z)",
            body,
        )
        self.assertIsNotNone(network_install, "network dependency install step is missing")
        self.assertIn(
            "steps.apt-cached-install.outputs.installed != 'true'",
            network_install.group("body"),
        )
        self.assertNotIn("GH_TOKEN", body)
        self.assertNotIn("gh cache delete", body)

    def test_network_fallback_repairs_dpkg_state_before_installing_dependencies(
        self,
    ) -> None:
        workflow = WORKFLOW.read_text(encoding="utf-8")
        job = re.search(
            r"(?ms)^  test-e2e-admin:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n|\Z)",
            workflow,
        )
        self.assertIsNotNone(job, "e2e-admin job is missing")
        step = re.search(
            r"(?ms)^      - name: Install the system dependencies from the network\n"
            r"(?P<body>.*?)(?=^      - name:|\Z)",
            job.group("body"),
        )
        self.assertIsNotNone(step, "network dependency install step is missing")
        body = step.group("body")
        update = body.index("sudo apt-get update")
        repair = body.index("sudo apt-get -f install -y --no-install-recommends")
        install = body.index("sudo apt-get install -y --no-install-recommends")
        self.assertLess(update, repair)
        self.assertLess(repair, install)

    def test_retry_cleanup_keeps_complete_packages_and_clears_partial_downloads(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory(prefix="playwright-apt-cache-") as temporary:
            root = Path(temporary)
            cache = root / ".apt-cache"
            partial = cache / "partial"
            partial.mkdir(parents=True)
            (cache / "fonts.deb").write_bytes(b"complete package")
            (partial / "interrupted.deb").write_bytes(b"partial package")

            fake_bin = root / "bin"
            fake_bin.mkdir()
            fake_sudo = fake_bin / "sudo"
            fake_sudo.write_text(FAKE_SUDO, encoding="utf-8")
            fake_sudo.chmod(0o755)

            result = subprocess.run(
                ["bash", str(PREPARE_CACHE), str(cache)],
                env={
                    **os.environ,
                    "PATH": f"{fake_bin}{os.pathsep}{os.environ['PATH']}",
                },
                capture_output=True,
                text=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((cache / "fonts.deb").read_bytes(), b"complete package")
            self.assertEqual(list(partial.iterdir()), [])

    def test_network_retry_uses_the_tested_cache_preparation_script(self) -> None:
        workflow = WORKFLOW.read_text(encoding="utf-8")
        self.assertIn(
            "bash .github/scripts/prepare-playwright-apt-cache.sh .apt-cache",
            workflow,
        )

    def test_network_install_retry_budget_covers_slow_archive_mirrors(self) -> None:
        workflow = WORKFLOW.read_text(encoding="utf-8")
        job = re.search(
            r"(?ms)^  test-e2e-admin:\n(.*?)(?=^  [a-z0-9-]+:|\Z)", workflow
        )
        self.assertIsNotNone(job, "test-e2e-admin job is missing")
        step = re.search(
            r"(?ms)^      - name: Install the system dependencies from the network\n(.*?)(?=^      - name:|\Z)",
            job.group(1),
        )
        self.assertIsNotNone(step, "network dependency install step is missing")
        self.assertIn("timeout-minutes: 20", step.group(1))
        self.assertIn("timeout --signal=KILL 9m", step.group(1))
        self.assertIn("timeout-minutes: 35", job.group(1))

    def test_real_client_browser_flow_uses_the_shared_apt_cache(self) -> None:
        workflow = REAL_CLIENT_WORKFLOW.read_text(encoding="utf-8")
        ci_workflow = WORKFLOW.read_text(encoding="utf-8")
        self.assertIn("path: .apt-cache/*.deb", workflow)
        self.assertIn(
            "key: playwright-apt-${{ steps.playwright-deps.outputs.image }}-"
            "${{ hashFiles('web-source/apt-packages.txt') }}",
            workflow,
        )
        self.assertIn(
            "key: playwright-apt-${{ steps.playwright-deps.outputs.image }}-"
            "${{ hashFiles('apt-packages.txt') }}",
            ci_workflow,
        )
        self.assertIn(
            "bash ../.github/scripts/prepare-playwright-apt-cache.sh ../.apt-cache",
            workflow,
        )
        self.assertIn("if: steps.apt-cache.outputs.cache-hit == 'true'", workflow)
        self.assertIn(
            "if sudo dpkg --auto-deconfigure -i ../.apt-cache/*.deb",
            workflow,
        )
        self.assertIn(
            "steps.apt-cached-install.outputs.installed != 'true'", workflow
        )
        self.assertIn("timeout --signal=KILL 9m bash -c attempt_install", workflow)
        self.assertIn("timeout-minutes: 20", workflow)
        self.assertIn("timeout-minutes: 45", workflow)
        self.assertIn("elapsed >= 2700", workflow)
        self.assertIn("command -v certutil", workflow)
        self.assertIn("sudo apt-get install -y libnss3-tools", workflow)
        self.assertIn(
            "run: corepack pnpm exec playwright install chromium", workflow
        )
        self.assertNotIn("playwright install --with-deps chromium", workflow)
        self.assertIn("playwright install-deps --dry-run chromium", workflow)


if __name__ == "__main__":
    unittest.main(verbosity=2)
