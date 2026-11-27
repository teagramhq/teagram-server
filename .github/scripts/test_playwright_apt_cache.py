#!/usr/bin/env python3
"""Verify that a Playwright apt retry retains completed package downloads."""

from __future__ import annotations

import json
import os
import re
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
PREPARE_CACHE = ROOT / ".github/scripts/prepare-playwright-apt-cache.sh"
INSTALL_CACHE = ROOT / ".github/scripts/install-playwright-apt-cache.sh"
INSTALL_FALLBACK = ROOT / ".github/scripts/install-playwright-apt-fallback.sh"
WORKFLOW = ROOT / ".github/workflows/ci.yml"
REAL_CLIENT_WORKFLOW = ROOT / ".github/workflows/real-client-browser-flow.yml"
PLAYWRIGHT_PACKAGE = "libnss3-tools"


FAKE_SUDO = """#!/usr/bin/env bash
set -euo pipefail
command=$1
shift
case "$command" in
  rm|mkdir) exec "/bin/$command" "$@" ;;
  chown) exit 0 ;;
  dpkg|apt-get) exec "$command" "$@" ;;
  *) echo "unexpected sudo command: $command" >&2; exit 64 ;;
esac
"""

FAKE_DPKG = """#!/usr/bin/env python3
import json
import os
import re
import sys
from pathlib import Path

state_path = Path(os.environ["PACKAGE_STATE_FILE"])
if state_path.exists():
    state = json.loads(state_path.read_text(encoding="utf-8"))
else:
    runner_version = os.environ["RUNNER_PACKAGE_VERSION"]
    installed = os.environ["PLAYWRIGHT_PACKAGE_INSTALLED"] == "true"
    state = {
        "runner_version": runner_version,
        "data_version": runner_version,
        "t64_version": runner_version,
        "configured": True,
        "deconfigured": False,
        "auto_deconfigured": False,
        "playwright_packages": ["libnss3-tools"] if installed else [],
        "apt_updated": False,
    }

def save():
    state_path.write_text(json.dumps(state), encoding="utf-8")

args = sys.argv[1:]
if args == ["--configure", "-a"]:
    state["configured"] = state["data_version"] == state["t64_version"]
    state["deconfigured"] = not state["configured"]
    save()
    if not state["configured"]:
        print(
            "libasound2-data and libasound2t64 versions do not match",
            file=sys.stderr,
        )
        sys.exit(1)
    sys.exit(0)

if args[:2] != ["--auto-deconfigure", "-i"]:
    print("unexpected dpkg arguments: " + " ".join(args), file=sys.stderr)
    sys.exit(64)

versions = {}
for package in ("libasound2-data", "libasound2t64"):
    pattern = re.compile(r"^" + re.escape(package) + r"_(.+)_(?:all|amd64)[.]deb$")
    matches = [pattern.match(Path(path).name) for path in args[2:]]
    match = next((candidate for candidate in matches if candidate), None)
    if match is None:
        print("cached set is missing " + package, file=sys.stderr)
        sys.exit(64)
    versions[package] = match.group(1)

state["transition"] = (
    "upgrade"
    if versions["libasound2-data"] > state["runner_version"]
    else "downgrade"
    if versions["libasound2-data"] < state["runner_version"]
    else "same"
)
state["deconfigured"] = any(
    version != state["runner_version"] for version in versions.values()
)
state["auto_deconfigured"] = state["deconfigured"]
state["configured"] = not state["deconfigured"]
state["data_version"] = versions["libasound2-data"]
state["t64_version"] = versions["libasound2t64"]
save()
if state["data_version"] != state["t64_version"]:
    print(
        "dependency conflict: libasound2t64 requires matching libasound2-data",
        file=sys.stderr,
    )
    sys.exit(1)
"""

FAKE_APT_GET = """#!/usr/bin/env python3
import json
import os
import sys
from pathlib import Path

state_path = Path(os.environ["PACKAGE_STATE_FILE"])
log_path = Path(os.environ["APT_COMMAND_LOG"])
state = json.loads(state_path.read_text(encoding="utf-8"))
args = sys.argv[1:]

def record(event):
    with log_path.open("a", encoding="utf-8") as log:
        log.write(event + "\\n")

if args == ["update"]:
    state["apt_updated"] = True
    record("update")
elif args[:4] == ["-f", "install", "-y", "--no-install-recommends"]:
    if not state["apt_updated"]:
        print("apt-get -f install ran before update", file=sys.stderr)
        sys.exit(1)
    if not any(arg.startswith("Dir::Cache::archives=") for arg in args):
        print("repair packages must be saved to the apt cache", file=sys.stderr)
        sys.exit(1)
    version = os.environ["NETWORK_PACKAGE_VERSION"]
    state["data_version"] = version
    state["t64_version"] = version
    state["configured"] = True
    state["deconfigured"] = False
    record("repair")
elif args[:1] == ["install"]:
    if not state["configured"] or state["data_version"] != state["t64_version"]:
        print("cannot install while dpkg is broken", file=sys.stderr)
        sys.exit(1)
    if "libnss3-tools" not in args:
        print("expected the refreshed Playwright dependency", file=sys.stderr)
        sys.exit(1)
    state["playwright_packages"] = sorted(
        set(state["playwright_packages"]) | {"libnss3-tools"}
    )
    record("install")
else:
    print("unexpected apt-get arguments: " + " ".join(args), file=sys.stderr)
    sys.exit(64)

state_path.write_text(json.dumps(state), encoding="utf-8")
"""

FAKE_PNPM = """#!/usr/bin/env python3
import json
import os
import sys
from pathlib import Path

if sys.argv[1:] != ["exec", "playwright", "install-deps", "--dry-run", "chromium"]:
    print("unexpected pnpm arguments: " + " ".join(sys.argv[1:]), file=sys.stderr)
    sys.exit(64)

state = json.loads(Path(os.environ["PACKAGE_STATE_FILE"]).read_text(encoding="utf-8"))
if not state["configured"] or state["data_version"] != state["t64_version"]:
    print("Missing system dependencies:")
    print("  libasound2-data")
    sys.exit(1)
if "libnss3-tools" not in state["playwright_packages"]:
    print("Missing system dependencies:")
    print("  libnss3-tools")
    sys.exit(1)
print("All system dependencies are installed")
"""


class PlaywrightAptCacheTests(unittest.TestCase):
    def _fake_package_environment(
        self,
        root: Path,
        runner_version: str,
        *,
        playwright_package_installed: bool = True,
        network_version: str = "1.2.15.3-1ubuntu1.2",
    ) -> tuple[dict[str, str], Path, Path]:
        fake_bin = root / "bin"
        fake_bin.mkdir()
        state_path = root / "package-state.json"
        apt_log = root / "apt-commands.txt"
        for name, contents in (
            ("sudo", FAKE_SUDO),
            ("dpkg", FAKE_DPKG),
            ("apt-get", FAKE_APT_GET),
            ("pnpm", FAKE_PNPM),
        ):
            executable = fake_bin / name
            executable.write_text(contents, encoding="utf-8")
            executable.chmod(0o755)

        env = {
            **os.environ,
            "PATH": f"{fake_bin}{os.pathsep}{os.environ['PATH']}",
            "PACKAGE_STATE_FILE": str(state_path),
            "APT_COMMAND_LOG": str(apt_log),
            "RUNNER_PACKAGE_VERSION": runner_version,
            "NETWORK_PACKAGE_VERSION": network_version,
            "PLAYWRIGHT_PACKAGE_INSTALLED": str(playwright_package_installed).lower(),
        }
        return env, state_path, apt_log

    @staticmethod
    def _write_cached_alsa_packages(
        cache: Path, data_version: str, t64_version: str
    ) -> None:
        cache.mkdir(parents=True, exist_ok=True)
        (cache / f"libasound2-data_{data_version}_all.deb").touch()
        (cache / f"libasound2t64_{t64_version}_amd64.deb").touch()

    def test_cached_install_handles_runner_package_version_drift_offline(self) -> None:
        cases = (
            ("1.2.15.3-1ubuntu1.1", "1.2.15.3-1ubuntu1.2", "upgrade"),
            ("1.2.15.3-1ubuntu1.2", "1.2.15.3-1ubuntu1.1", "downgrade"),
        )
        for runner_version, cached_version, transition in cases:
            with self.subTest(
                runner_version=runner_version,
                cached_version=cached_version,
                transition=transition,
            ):
                with tempfile.TemporaryDirectory(
                    prefix="playwright-apt-install-"
                ) as temporary:
                    root = Path(temporary)
                    cache = root / ".apt-cache"
                    self._write_cached_alsa_packages(
                        cache, cached_version, cached_version
                    )
                    env, state_path, apt_log = self._fake_package_environment(
                        root, runner_version
                    )
                    output = root / "github-output"
                    output.write_text("", encoding="utf-8")
                    result = subprocess.run(
                        ["bash", str(INSTALL_CACHE), str(cache)],
                        env={**env, "GITHUB_OUTPUT": str(output)},
                        capture_output=True,
                        text=True,
                        check=False,
                    )

                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(
                        output.read_text(encoding="utf-8"), "installed=true\n"
                    )
                    state = json.loads(state_path.read_text(encoding="utf-8"))
                    self.assertEqual(state["data_version"], cached_version)
                    self.assertEqual(state["t64_version"], cached_version)
                    self.assertEqual(state["runner_version"], runner_version)
                    self.assertEqual(state["transition"], transition)
                    self.assertTrue(state["configured"])
                    self.assertFalse(state["deconfigured"])
                    self.assertTrue(state["auto_deconfigured"])
                    self.assertFalse(
                        apt_log.exists(), "the warm path must stay offline"
                    )

    def test_broken_cached_packages_are_repaired_and_verified_by_network_fallback(
        self,
    ) -> None:
        runner_version = "1.2.15.3-1ubuntu1.1"
        cached_data_version = "1.2.15.3-1ubuntu1.2"
        network_version = "1.2.15.3-1ubuntu1.3"
        with tempfile.TemporaryDirectory(
            prefix="playwright-apt-fallback-"
        ) as temporary:
            root = Path(temporary)
            cache = root / ".apt-cache"
            self._write_cached_alsa_packages(
                cache, cached_data_version, runner_version
            )
            env, state_path, apt_log = self._fake_package_environment(
                root,
                runner_version,
                playwright_package_installed=False,
                network_version=network_version,
            )
            output = root / "github-output"
            output.write_text("", encoding="utf-8")

            cached_install = subprocess.run(
                ["bash", str(INSTALL_CACHE), str(cache)],
                env={**env, "GITHUB_OUTPUT": str(output)},
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(cached_install.returncode, 0, cached_install.stderr)
            self.assertEqual(output.read_text(encoding="utf-8"), "installed=false\n")
            broken_state = json.loads(state_path.read_text(encoding="utf-8"))
            self.assertEqual(broken_state["data_version"], cached_data_version)
            self.assertEqual(broken_state["t64_version"], runner_version)
            self.assertFalse(broken_state["configured"])
            self.assertTrue(broken_state["auto_deconfigured"])

            fallback = subprocess.run(
                ["bash", str(INSTALL_FALLBACK), str(cache)],
                cwd=root,
                env=env,
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(fallback.returncode, 0, fallback.stderr)

            verification = subprocess.run(
                ["pnpm", "exec", "playwright", "install-deps", "--dry-run", "chromium"],
                cwd=root,
                env=env,
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(
                verification.returncode,
                0,
                verification.stdout + verification.stderr,
            )
            self.assertIn("All system dependencies are installed", verification.stdout)

            repaired_state = json.loads(state_path.read_text(encoding="utf-8"))
            self.assertEqual(repaired_state["data_version"], network_version)
            self.assertEqual(repaired_state["t64_version"], network_version)
            self.assertTrue(repaired_state["configured"])
            self.assertIn(PLAYWRIGHT_PACKAGE, repaired_state["playwright_packages"])
            self.assertEqual(
                apt_log.read_text(encoding="utf-8"), "update\nrepair\ninstall\n"
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
        self.assertIn(
            "bash .github/scripts/install-playwright-apt-fallback.sh .apt-cache",
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
        self.assertIn(
            "bash .github/scripts/install-playwright-apt-fallback.sh .apt-cache",
            step.group("body"),
        )
        fallback_script = INSTALL_FALLBACK.read_text(encoding="utf-8")
        self.assertIn("sudo apt-get update", fallback_script)
        self.assertIn(
            "sudo apt-get -f install -y --no-install-recommends",
            fallback_script,
        )
        self.assertIn(
            "sudo apt-get install -y --no-install-recommends", fallback_script
        )

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
        fallback_script = INSTALL_FALLBACK.read_text(encoding="utf-8")
        self.assertIn(
            'prepare-playwright-apt-cache.sh" "$apt_cache_dir"',
            fallback_script,
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
        self.assertIn("if: steps.apt-cache.outputs.cache-hit == 'true'", workflow)
        self.assertIn(
            "bash ../.github/scripts/install-playwright-apt-cache.sh ../.apt-cache",
            workflow,
        )
        self.assertIn(
            "bash ../.github/scripts/install-playwright-apt-fallback.sh ../.apt-cache",
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
