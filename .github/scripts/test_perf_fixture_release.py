#!/usr/bin/env python3
"""Exercise first publication and recovery of interrupted fixture uploads."""

from __future__ import annotations

import importlib.util
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from typing import Any, TypedDict
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / ".github/scripts/perf_fixture_release.py"
SPEC = importlib.util.spec_from_file_location("perf_fixture_release", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
publisher = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(publisher)


class FakeGitHub:
    def __init__(self, *, fail_upload_once: bool = False) -> None:
        self.releases: dict[str, FakeRelease] = {}
        self.create_count = 0
        self.asset_lookups: list[str] = []
        self.fail_upload_once = fail_upload_once

    def run(
        self, command: list[str], **kwargs: Any
    ) -> subprocess.CompletedProcess[str]:
        args = command[1:]
        stdout = ""

        if args[:2] == ["api", "graphql"]:
            tag = next(
                arg.removeprefix("tagName=")
                for arg in args
                if arg.startswith("tagName=")
            )
            release = self.releases.get(tag)
            value = None if release is None else {
                "databaseId": release["database_id"],
                "isDraft": release["is_draft"],
            }
            stdout = json.dumps({"data": {"repository": {"release": value}}})
        elif args[:2] == ["release", "create"]:
            tag = args[2]
            self.create_count += 1
            self.releases[tag] = {
                "database_id": 7,
                "is_draft": True,
                "assets": set(),
            }
        elif args[:2] == ["api", "--paginate"]:
            path = next(arg for arg in args if arg.startswith("repos/"))
            release_id = path.split("/")[4]
            self.asset_lookups.append(release_id)
            release = next(
                release for release in self.releases.values()
                if str(release["database_id"]) == release_id
            )
            stdout = "".join(f"{name}\n" for name in sorted(release["assets"]))
        elif args[:2] == ["release", "upload"]:
            tag = args[2]
            release = self.releases[tag]
            files = [arg for arg in args[3:] if arg != "--clobber"]
            for index, path in enumerate(files):
                release["assets"].add(Path(path).name)
                if self.fail_upload_once and index == 0:
                    self.fail_upload_once = False
                    raise subprocess.CalledProcessError(
                        1, command, stderr="upload interrupted"
                    )
        elif args[:2] == ["release", "edit"]:
            tag = args[2]
            self.releases[tag]["is_draft"] = False
        else:
            raise AssertionError(f"unexpected gh invocation: {args}")

        return subprocess.CompletedProcess(command, 0, stdout=stdout, stderr="")


class FakeRelease(TypedDict):
    database_id: int
    is_draft: bool
    assets: set[str]


class PerfFixtureReleaseTests(unittest.TestCase):
    def setUp(self) -> None:
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.assets = self.root / "assets"
        self.assets.mkdir()
        for name, content in (("manifest.json", b"{}\n"), ("perf_1x.dump", b"dump")):
            (self.assets / name).write_bytes(content)
        self.github = FakeGitHub()
        self.env = patch.dict(
            os.environ,
            {
                "GITHUB_REPOSITORY": "teagramhq/teagram-server",
                "GITHUB_SHA": "0123456789abcdef0123456789abcdef01234567",
            },
        )
        self.env.start()
        self.addCleanup(self.env.stop)

    def run_publisher(self, github: FakeGitHub | None = None) -> None:
        with patch.object(subprocess, "run", side_effect=(github or self.github).run):
            publisher.publish_release(
                "perf-fixture-version",
                target=os.environ["GITHUB_SHA"],
                assets_dir=self.assets,
            )

    def test_first_publication_resolves_new_draft_and_verifies_assets_by_id(self) -> None:
        self.run_publisher()

        release = self.github.releases["perf-fixture-version"]
        self.assertEqual(self.github.create_count, 1)
        self.assertFalse(release["is_draft"])
        self.assertEqual(release["assets"], {"manifest.json", "perf_1x.dump"})
        self.assertEqual(self.github.asset_lookups, ["7"])

    def test_retry_after_interrupted_upload_resumes_the_same_draft(self) -> None:
        self.github.fail_upload_once = True

        with patch.object(subprocess, "run", side_effect=self.github.run):
            with self.assertRaises(subprocess.CalledProcessError):
                publisher.publish_release(
                    "perf-fixture-version",
                    target=os.environ["GITHUB_SHA"],
                    assets_dir=self.assets,
                )

            self.assertTrue(self.github.releases["perf-fixture-version"]["is_draft"])
            publisher.publish_release(
                "perf-fixture-version",
                target=os.environ["GITHUB_SHA"],
                assets_dir=self.assets,
            )

        release = self.github.releases["perf-fixture-version"]
        self.assertEqual(self.github.create_count, 1)
        self.assertFalse(release["is_draft"])
        self.assertEqual(release["assets"], {"manifest.json", "perf_1x.dump"})
        self.assertEqual(self.github.asset_lookups, ["7"])

    def test_lookup_reports_an_existing_draft(self) -> None:
        self.github.releases["perf-fixture-version"] = {
            "database_id": 7,
            "is_draft": True,
            "assets": set(),
        }

        with patch.object(subprocess, "run", side_effect=self.github.run):
            release = publisher.lookup_release("perf-fixture-version")

        self.assertEqual(release, {"database_id": 7, "is_draft": True})

    def test_published_release_is_left_unchanged(self) -> None:
        release: FakeRelease = {
            "database_id": 7,
            "is_draft": False,
            "assets": {"published.dump"},
        }
        self.github.releases["perf-fixture-version"] = release

        self.run_publisher()

        self.assertEqual(self.github.create_count, 0)
        self.assertEqual(release["assets"], {"published.dump"})
        self.assertEqual(self.github.asset_lookups, [])


if __name__ == "__main__":
    unittest.main()
