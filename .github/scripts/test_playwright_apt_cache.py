#!/usr/bin/env python3
"""Verify that a Playwright apt retry retains completed package downloads."""

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
PREPARE_CACHE = ROOT / ".github/scripts/prepare-playwright-apt-cache.sh"
INSTALL_CACHE = ROOT / ".github/scripts/install-playwright-apt-cache.sh"
INSTALL_FALLBACK = ROOT / ".github/scripts/install-playwright-apt-fallback.sh"
REBUILD_CACHE = ROOT / ".github/scripts/rebuild-playwright-apt-cache.sh"
DISCARD_CACHE = ROOT / ".github/scripts/discard-playwright-apt-cache.sh"
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
        "required_packages": json.loads(
            os.environ.get("PLAYWRIGHT_REQUIRED_PACKAGES", '["libnss3-tools"]')
        ),
        "installed_packages": [],
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
state["playwright_packages"] = sorted(
    set(state["playwright_packages"])
    | {
        Path(path).name.rsplit("_", 2)[0]
        for path in args[2:]
        if Path(path).name.startswith("libnss3-tools_")
    }
)
save()

for path in args[2:]:
    package_path = Path(path)
    match = re.match(r"^(.+)_([^_]+)_(?:all|amd64)[.]deb$", package_path.name)
    if match is None:
        print("unexpected package archive: " + package_path.name, file=sys.stderr)
        sys.exit(64)
    if package_path.read_bytes() != f"{match.group(1)}:{match.group(2)}".encode():
        state["configured"] = False
        state["deconfigured"] = True
        state["auto_deconfigured"] = True
        save()
        print("corrupt cached package: " + package_path.name, file=sys.stderr)
        sys.exit(1)
    state["installed_packages"] = sorted(
        set(state["installed_packages"]) | {match.group(1)}
    )
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
import time
from pathlib import Path

state_path = Path(os.environ["PACKAGE_STATE_FILE"])
log_path = Path(os.environ["APT_COMMAND_LOG"])
state = json.loads(state_path.read_text(encoding="utf-8"))
args = sys.argv[1:]
archive_dir_arg = next(
    (arg.split("=", 1)[1] for arg in args if arg.startswith("Dir::Cache::archives=")),
    None,
)

def save_package(package, version):
    if archive_dir_arg is None:
        print("package install did not set the archive directory", file=sys.stderr)
        sys.exit(1)
    archive_dir = Path(archive_dir_arg)
    architecture = (
        "all" if package in {"libasound2-data", "fonts-freefont-ttf"} else "amd64"
    )
    archive_dir.mkdir(parents=True, exist_ok=True)
    archive = archive_dir / f"{package}_{version}_{architecture}.deb"
    archive.write_bytes(f"{package}:{version}".encode())

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
    save_package("libasound2-data", version)
    save_package("libasound2t64", version)
    record("repair")
elif "--download-only" in args and "--reinstall" in args:
    if not state["configured"] or state["data_version"] != state["t64_version"]:
        print("cannot refresh package cache while dpkg is broken", file=sys.stderr)
        sys.exit(1)
    package_start = args.index("install") + 1
    packages = [
        arg
        for arg in args[package_start:]
        if not arg.startswith("-") and not arg.startswith("Dir::")
    ]
    if not packages or not any(arg.startswith("Dir::Cache::archives=") for arg in args):
        print("cache refresh needs packages and an archive directory", file=sys.stderr)
        sys.exit(1)
    disappeared_package = os.environ.get("DISAPPEARED_APT_PACKAGE")
    sleep_seconds = os.environ.get("APT_REBUILD_SLEEP_SECONDS")
    if sleep_seconds is not None:
        time.sleep(float(sleep_seconds))
    if state["apt_updated"] and disappeared_package in packages:
        record("download-failed")
        print(
            f"package {disappeared_package} is no longer present in refreshed indexes",
            file=sys.stderr,
        )
        sys.exit(100)
    omitted_package = os.environ.get("OMIT_APT_PACKAGE")
    for package in packages:
        if package != omitted_package:
            save_package(package, os.environ["NETWORK_PACKAGE_VERSION"])
    record("download")
elif args[:1] == ["install"]:
    if not state["configured"] or state["data_version"] != state["t64_version"]:
        print("cannot install while dpkg is broken", file=sys.stderr)
        sys.exit(1)
    if "libnss3-tools" not in args:
        print("expected the refreshed Playwright dependency", file=sys.stderr)
        sys.exit(1)
    package_start = args.index("install") + 1
    packages = [
        arg
        for arg in args[package_start:]
        if not arg.startswith("-") and not arg.startswith("Dir::")
    ]
    state["playwright_packages"] = sorted(
        set(state["playwright_packages"]) | set(packages)
    )
    state["installed_packages"] = sorted(
        set(state["installed_packages"]) | set(packages)
    )
    for package in packages:
        save_package(package, os.environ["NETWORK_PACKAGE_VERSION"])
    record("install")
else:
    print("unexpected apt-get arguments: " + " ".join(args), file=sys.stderr)
    sys.exit(64)

state_path.write_text(json.dumps(state), encoding="utf-8")
"""

FAKE_DPKG_DEB = """#!/usr/bin/env python3
import sys
from pathlib import Path

args = sys.argv[1:]
if len(args) != 3 or args[0] != "--field" or args[2] != "Package":
    print("unexpected dpkg-deb arguments: " + " ".join(args), file=sys.stderr)
    sys.exit(64)
parts = Path(args[1]).name.rsplit("_", 2)
if len(parts) != 3 or not parts[2].endswith(".deb"):
    print("invalid Debian archive name: " + Path(args[1]).name, file=sys.stderr)
    sys.exit(1)
print(parts[0])
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
missing_packages = sorted(
    set(state.get("required_packages", ["libnss3-tools"]))
    - set(state["playwright_packages"])
)
if missing_packages:
    print("Missing system dependencies:")
    for package in missing_packages:
        print("  " + package)
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
            ("dpkg-deb", FAKE_DPKG_DEB),
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
        (cache / f"libasound2-data_{data_version}_all.deb").write_bytes(
            f"libasound2-data:{data_version}".encode()
        )
        (cache / f"libasound2t64_{t64_version}_amd64.deb").write_bytes(
            f"libasound2t64:{t64_version}".encode()
        )

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
            font_archive = cache / "fonts-freefont-ttf_1.0_all.deb"
            font_archive.write_bytes(b"fonts-freefont-ttf:1.0")
            corrupt_archive = cache / f"libasound2-data_{cached_data_version}_all.deb"
            corrupt_archive.write_bytes(b"superseded corrupt archive")
            env, state_path, apt_log = self._fake_package_environment(
                root,
                runner_version,
                playwright_package_installed=False,
                network_version=network_version,
            )
            output = root / "github-output"
            output.write_text("", encoding="utf-8")
            (root / "apt-packages.txt").write_text(
                "fonts-freefont-ttf\nlibnss3-tools\n", encoding="utf-8"
            )

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
            self.assertIn("fonts-freefont-ttf", broken_state["installed_packages"])

            discard = subprocess.run(
                ["bash", str(DISCARD_CACHE), str(cache)],
                env=env,
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(discard.returncode, 0, discard.stderr)
            self.assertEqual(list(cache.glob("*.deb")), [])

            fallback = subprocess.run(
                ["bash", str(INSTALL_FALLBACK), str(cache), "apt-packages.txt"],
                cwd=root,
                env=env,
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(fallback.returncode, 0, fallback.stderr)
            self.assertIn("All system dependencies are installed", fallback.stdout)

            rebuild_output = root / "rebuild-output"
            rebuild_output.write_text("", encoding="utf-8")
            rebuild = subprocess.run(
                ["bash", str(REBUILD_CACHE), str(cache), "apt-packages.txt"],
                cwd=root,
                env={**env, "GITHUB_OUTPUT": str(rebuild_output)},
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(rebuild.returncode, 0, rebuild.stderr)
            self.assertEqual(
                rebuild_output.read_text(encoding="utf-8"),
                "cache_ready=true\n",
            )

            self.assertFalse(corrupt_archive.exists())
            self.assertEqual(
                {path.name for path in cache.glob("*.deb")},
                {
                    f"libasound2-data_{network_version}_all.deb",
                    f"libasound2t64_{network_version}_amd64.deb",
                    f"fonts-freefont-ttf_{network_version}_all.deb",
                    f"{PLAYWRIGHT_PACKAGE}_{network_version}_amd64.deb",
                },
            )

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
                apt_log.read_text(encoding="utf-8"),
                "update\nrepair\ninstall\ndownload\n",
            )

            fresh_root = root / "fresh-runner"
            fresh_root.mkdir()
            fresh_cache = fresh_root / ".apt-cache"
            shutil.copytree(cache, fresh_cache)
            fresh_env, fresh_state_path, fresh_apt_log = self._fake_package_environment(
                fresh_root,
                runner_version,
                playwright_package_installed=False,
                network_version=network_version,
            )
            fresh_output = fresh_root / "github-output"
            fresh_output.write_text("", encoding="utf-8")
            restored = subprocess.run(
                ["bash", str(INSTALL_CACHE), str(fresh_cache)],
                env={**fresh_env, "GITHUB_OUTPUT": str(fresh_output)},
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(restored.returncode, 0, restored.stderr)
            self.assertEqual(
                fresh_output.read_text(encoding="utf-8"), "installed=true\n"
            )
            fresh_state = json.loads(fresh_state_path.read_text(encoding="utf-8"))
            self.assertEqual(fresh_state["data_version"], network_version)
            self.assertEqual(fresh_state["t64_version"], network_version)
            self.assertIn(PLAYWRIGHT_PACKAGE, fresh_state["playwright_packages"])
            self.assertIn("fonts-freefont-ttf", fresh_state["installed_packages"])
            self.assertTrue(fresh_state["configured"])
            self.assertFalse(
                fresh_apt_log.exists(), "restoring the repaired cache must stay offline"
            )

    def test_disappeared_package_skips_cache_publish_after_runner_verifies(self) -> None:
        with tempfile.TemporaryDirectory(
            prefix="playwright-apt-disappeared-package-"
        ) as temporary:
            root = Path(temporary)
            cache = root / ".apt-cache"
            output = root / "github-output"
            output.write_text("", encoding="utf-8")
            package_list = root / "apt-packages.txt"
            package_list.write_text(
                "retired-transitive-dep\nlibnss3-tools\n", encoding="utf-8"
            )
            env, state_path, apt_log = self._fake_package_environment(
                root,
                "1.2.15.3-1ubuntu1.1",
                playwright_package_installed=False,
            )
            state_path.write_text(
                json.dumps(
                    {
                        "runner_version": "1.2.15.3-1ubuntu1.1",
                        "data_version": "1.2.15.3-1ubuntu1.1",
                        "t64_version": "1.2.15.3-1ubuntu1.1",
                        "configured": True,
                        "deconfigured": False,
                        "auto_deconfigured": False,
                        "playwright_packages": [],
                        "required_packages": [
                            "replacement-transitive-dep",
                            "libnss3-tools",
                        ],
                        "installed_packages": [],
                        "apt_updated": False,
                    }
                ),
                encoding="utf-8",
            )
            env.update(
                {
                    "DISAPPEARED_APT_PACKAGE": "retired-transitive-dep",
                    "GITHUB_OUTPUT": str(output),
                    "PLAYWRIGHT_REQUIRED_PACKAGES": json.dumps(
                        ["replacement-transitive-dep", "libnss3-tools"]
                    ),
                }
            )

            fallback = subprocess.run(
                ["bash", str(INSTALL_FALLBACK), str(cache), str(package_list)],
                cwd=root,
                env=env,
                capture_output=True,
                text=True,
                check=False,
            )

            self.assertEqual(fallback.returncode, 0, fallback.stderr)
            self.assertIn("All system dependencies are installed", fallback.stdout)
            self.assertEqual(
                apt_log.read_text(encoding="utf-8"), "update\nrepair\ninstall\n"
            )

            rebuild = subprocess.run(
                ["bash", str(REBUILD_CACHE), str(cache), str(package_list)],
                cwd=root,
                env=env,
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(rebuild.returncode, 0, rebuild.stderr)
            self.assertIn("::warning::APT cache rebuild failed", rebuild.stdout)
            self.assertIn("skipping cache publish", rebuild.stdout)
            self.assertEqual(
                output.read_text(encoding="utf-8"), "cache_ready=false\n"
            )
            state = json.loads(state_path.read_text(encoding="utf-8"))
            self.assertTrue(state["configured"])
            self.assertEqual(
                set(state["required_packages"]), set(state["playwright_packages"])
            )
            self.assertEqual(
                apt_log.read_text(encoding="utf-8"),
                "update\nrepair\ninstall\ndownload-failed\n",
            )
            self.assertFalse(
                any(
                    path.name.startswith("retired-transitive-dep_")
                    for path in cache.glob("*.deb")
                )
            )

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

    def test_cache_rebuild_skips_publish_when_apt_omits_a_package(self) -> None:
        with tempfile.TemporaryDirectory(
            prefix="playwright-apt-omitted-package-"
        ) as temporary:
            root = Path(temporary)
            cache = root / ".apt-cache"
            cache.mkdir()
            output = root / "github-output"
            output.write_text("", encoding="utf-8")
            package_list = root / "apt-packages.txt"
            package_list.write_text(
                "fonts-freefont-ttf\nlibnss3-tools\n", encoding="utf-8"
            )
            env, state_path, apt_log = self._fake_package_environment(
                root, "1.2.15.3-1ubuntu1.2"
            )
            state_path.write_text(
                json.dumps(
                    {
                        "runner_version": "1.2.15.3-1ubuntu1.2",
                        "data_version": "1.2.15.3-1ubuntu1.2",
                        "t64_version": "1.2.15.3-1ubuntu1.2",
                        "configured": True,
                        "deconfigured": False,
                        "auto_deconfigured": False,
                        "playwright_packages": ["fonts-freefont-ttf", "libnss3-tools"],
                        "required_packages": ["fonts-freefont-ttf", "libnss3-tools"],
                        "installed_packages": [],
                        "apt_updated": True,
                    }
                ),
                encoding="utf-8",
            )
            env.update(
                {
                    "GITHUB_OUTPUT": str(output),
                    "OMIT_APT_PACKAGE": "fonts-freefont-ttf",
                }
            )

            rebuild = subprocess.run(
                ["bash", str(REBUILD_CACHE), str(cache), str(package_list)],
                cwd=root,
                env=env,
                capture_output=True,
                text=True,
                check=False,
            )

            self.assertEqual(rebuild.returncode, 0, rebuild.stderr)
            self.assertIn("APT cache is missing fonts-freefont-ttf", rebuild.stdout)
            self.assertEqual(
                output.read_text(encoding="utf-8"), "cache_ready=false\n"
            )
            self.assertEqual(apt_log.read_text(encoding="utf-8"), "download\n")
            self.assertFalse(list(cache.glob("fonts-freefont-ttf_*.deb")))

    def test_cache_rebuild_timeout_does_not_prevent_playwright_verification(self) -> None:
        with tempfile.TemporaryDirectory(
            prefix="playwright-apt-rebuild-timeout-"
        ) as temporary:
            root = Path(temporary)
            cache = root / ".apt-cache"
            output = root / "github-output"
            output.write_text("", encoding="utf-8")
            package_list = root / "apt-packages.txt"
            package_list.write_text("libnss3-tools\n", encoding="utf-8")
            env, state_path, _ = self._fake_package_environment(
                root,
                "1.2.15.3-1ubuntu1.1",
                playwright_package_installed=False,
            )
            state_path.write_text(
                json.dumps(
                    {
                        "runner_version": "1.2.15.3-1ubuntu1.1",
                        "data_version": "1.2.15.3-1ubuntu1.1",
                        "t64_version": "1.2.15.3-1ubuntu1.1",
                        "configured": True,
                        "deconfigured": False,
                        "auto_deconfigured": False,
                        "playwright_packages": [],
                        "required_packages": ["libnss3-tools"],
                        "installed_packages": [],
                        "apt_updated": False,
                    }
                ),
                encoding="utf-8",
            )

            fallback = subprocess.run(
                ["bash", str(INSTALL_FALLBACK), str(cache), str(package_list)],
                cwd=root,
                env=env,
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(fallback.returncode, 0, fallback.stderr)
            self.assertIn("All system dependencies are installed", fallback.stdout)

            rebuild_env = {
                **env,
                "GITHUB_OUTPUT": str(output),
                "APT_REBUILD_SLEEP_SECONDS": "5",
            }
            timed_out_rebuild = subprocess.run(
                [
                    "timeout",
                    "--signal=TERM",
                    "--kill-after=1s",
                    "0.2s",
                    "bash",
                    str(REBUILD_CACHE),
                    str(cache),
                    str(package_list),
                ],
                cwd=root,
                env=rebuild_env,
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(timed_out_rebuild.returncode, 124)

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
            self.assertEqual(output.read_text(encoding="utf-8"), "")

    def test_failed_cache_restore_saves_repaired_packages_under_a_fresh_key(
        self,
    ) -> None:
        workflows = (
            (
                WORKFLOW,
                "Cache the Playwright apt dependencies",
                "Install the system dependencies from cache",
                "Rebuild the Playwright apt cache",
                "Save the Playwright apt dependencies",
                "Verify the Playwright system dependencies",
                "playwright-apt-${{ steps.playwright-deps.outputs.image }}-"
                "${{ hashFiles('apt-packages.txt') }}",
            ),
            (
                REAL_CLIENT_WORKFLOW,
                "Cache Chromium apt dependencies",
                "Install Chromium system dependencies from cache",
                "Rebuild Chromium apt cache",
                "Save Chromium apt dependencies",
                "Verify Chromium system dependencies",
                "playwright-apt-${{ steps.playwright-deps.outputs.image }}-"
                "${{ hashFiles('web-source/apt-packages.txt') }}",
            ),
        )
        for (
            path,
            cache_name,
            install_name,
            rebuild_name,
            save_name,
            verify_name,
            key_prefix,
        ) in workflows:
            with self.subTest(workflow=path.name):
                workflow = path.read_text(encoding="utf-8")
                steps = {}
                for name in (
                    cache_name,
                    install_name,
                    rebuild_name,
                    save_name,
                    verify_name,
                ):
                    step = re.search(
                        rf"(?ms)^      - name: {re.escape(name)}\n"
                        r"(?P<body>.*?)(?=^      - name:|\Z)",
                        workflow,
                    )
                    self.assertIsNotNone(step, f"{name} step is missing")
                    steps[name] = step.group("body")

                cache = steps[cache_name]
                self.assertIn("uses: actions/cache/restore@v6.1.0", cache)
                self.assertIn(
                    f"key: {key_prefix}-${{{{ github.run_id }}}}-"
                    "${{ github.run_attempt }}",
                    cache,
                )
                self.assertIn("restore-keys: |", cache)
                self.assertIn(key_prefix + "-\n", cache)
                self.assertIn(key_prefix + "\n", cache)
                self.assertIn(
                    "if: steps.apt-cache.outputs.cache-matched-key != ''",
                    steps[install_name],
                )
                self.assertIn(
                    "steps.apt-cache.outputs.cache-matched-key == ''",
                    steps[save_name],
                )
                self.assertIn(
                    "steps.apt-cached-install.outputs.installed == 'false'",
                    steps[save_name],
                )
                self.assertIn(
                    "steps.apt-cache-rebuild.outputs.cache_ready == 'true'",
                    steps[save_name],
                )
                self.assertIn(
                    "key: ${{ steps.apt-cache.outputs.cache-primary-key }}",
                    steps[save_name],
                )
                self.assertIn(
                    "steps.apt-cached-install.outputs.installed == 'false'",
                    steps[save_name],
                )
                discard_step = re.search(
                    r"(?ms)^      - name: Discard the rejected apt cache\n"
                    r"(?P<body>.*?)(?=^      - name:|\Z)",
                    workflow,
                )
                self.assertIsNotNone(discard_step, "rejected cache cleanup is missing")
                self.assertIn(
                    "steps.apt-cached-install.outputs.installed == 'false'",
                    discard_step.group("body"),
                )
                self.assertIn(
                    "discard-playwright-apt-cache.sh", discard_step.group("body")
                )
                network_name = install_name.replace(
                    "from cache", "from the network"
                )
                self.assertLess(
                    workflow.index("- name: Discard the rejected apt cache"),
                    workflow.index(f"- name: {network_name}"),
                )
                rebuild = steps[rebuild_name]
                self.assertIn("id: apt-cache-rebuild", rebuild)
                self.assertIn("continue-on-error: true", rebuild)
                self.assertIn("timeout-minutes: 5", rebuild)
                self.assertIn("rebuild-playwright-apt-cache.sh", rebuild)
                self.assertLess(
                    workflow.index(f"- name: {network_name}"),
                    workflow.index(f"- name: {rebuild_name}"),
                )
                self.assertLess(
                    workflow.index(f"- name: {rebuild_name}"),
                    workflow.index(f"- name: {save_name}"),
                )
                self.assertLess(
                    workflow.index(f"- name: {save_name}"),
                    workflow.index(f"- name: {verify_name}"),
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
            "if: steps.apt-cache.outputs.cache-matched-key != ''",
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
            "bash .github/scripts/install-playwright-apt-fallback.sh .apt-cache apt-packages.txt",
            network_install.group("body"),
        )
        self.assertNotIn("GH_TOKEN", body)
        self.assertNotIn("gh cache delete", body)

    def test_discard_rejected_cache_removes_debs_but_keeps_cache_directory(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory(prefix="playwright-apt-discard-") as temporary:
            root = Path(temporary)
            cache = root / ".apt-cache"
            cache.mkdir()
            (cache / "bad.deb").write_bytes(b"corrupt")
            env, _, _ = self._fake_package_environment(root, "1.2.3")

            result = subprocess.run(
                ["bash", str(DISCARD_CACHE), str(cache)],
                env=env,
                capture_output=True,
                text=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue(cache.is_dir())
            self.assertEqual(list(cache.glob("*.deb")), [])

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
            "bash .github/scripts/install-playwright-apt-fallback.sh .apt-cache apt-packages.txt",
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
        verification_command = "pnpm exec playwright install-deps --dry-run chromium"
        self.assertGreater(
            fallback_script.rindex(verification_command),
            fallback_script.index("sudo apt-get install -y --no-install-recommends"),
        )
        self.assertNotIn("--download-only --reinstall", fallback_script)
        rebuild_script = REBUILD_CACHE.read_text(encoding="utf-8")
        self.assertIn("--download-only --reinstall", rebuild_script)
        self.assertIn("dpkg-deb --field", rebuild_script)
        self.assertIn('echo "cache_ready=true"', rebuild_script)

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
        self.assertIn(
            "if: steps.apt-cache.outputs.cache-matched-key != ''", workflow
        )
        self.assertIn(
            "bash ../.github/scripts/install-playwright-apt-cache.sh ../.apt-cache",
            workflow,
        )
        self.assertIn(
            "bash ../.github/scripts/install-playwright-apt-fallback.sh ../.apt-cache apt-packages.txt",
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
