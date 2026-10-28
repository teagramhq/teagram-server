#!/usr/bin/env python3
"""Find or publish the checksummed RPC performance fixture release."""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from pathlib import Path
from typing import TypedDict


RELEASE_QUERY = """query($owner: String!, $name: String!, $tagName: String!) {
  repository(owner: $owner, name: $name) {
    release(tagName: $tagName) {
      databaseId
      isDraft
    }
  }
}"""


class ReleaseSummary(TypedDict):
    database_id: int
    is_draft: bool


def run_gh(args: list[str]) -> str:
    result = subprocess.run(
        ["gh", *args], check=True, capture_output=True, text=True
    )
    return result.stdout


def repository_parts() -> tuple[str, str]:
    repository = os.environ.get("GITHUB_REPOSITORY", "")
    owner, separator, name = repository.partition("/")
    if not owner or not separator or not name or "/" in name:
        raise ValueError("GITHUB_REPOSITORY must be owner/repository")
    return owner, name


def lookup_release(tag: str) -> ReleaseSummary | None:
    owner, name = repository_parts()
    response = run_gh(
        [
            "api",
            "graphql",
            "-F",
            f"owner={owner}",
            "-F",
            f"name={name}",
            "-F",
            f"tagName={tag}",
            "-f",
            f"query={RELEASE_QUERY}",
        ]
    )
    payload = json.loads(response)
    repository = payload.get("data", {}).get("repository")
    if repository is None:
        raise ValueError("GitHub did not return the release repository")
    release = repository.get("release")
    if release is None:
        return None
    database_id = release.get("databaseId")
    is_draft = release.get("isDraft")
    if not isinstance(database_id, int) or database_id <= 0:
        raise ValueError(f"release {tag} has no REST database ID")
    if not isinstance(is_draft, bool):
        raise ValueError(f"release {tag} has no draft state")
    return {"database_id": database_id, "is_draft": is_draft}


def release_asset_names(database_id: int) -> list[str]:
    owner, name = repository_parts()
    response = run_gh(
        [
            "api",
            "--paginate",
            "--jq",
            ".[].name",
            f"repos/{owner}/{name}/releases/{database_id}/assets?per_page=100",
        ]
    )
    return sorted(line for line in response.splitlines() if line)


def staged_assets(assets_dir: Path) -> list[Path]:
    if not assets_dir.is_dir():
        raise ValueError(f"release assets directory does not exist: {assets_dir}")
    entries = sorted(assets_dir.iterdir())
    if not entries or any(path.is_symlink() or not path.is_file() for path in entries):
        raise ValueError(f"release assets must be non-empty regular files: {assets_dir}")
    if "manifest.json" not in {path.name for path in entries}:
        raise ValueError("release assets are missing manifest.json")
    return entries


def publish_release(tag: str, *, target: str, assets_dir: Path) -> None:
    assets = staged_assets(assets_dir)
    release = lookup_release(tag)
    if release is not None and not release["is_draft"]:
        print(f"Release {tag} is already published; leaving it unchanged")
        return

    if release is None:
        run_gh(
            [
                "release",
                "create",
                tag,
                "--target",
                target,
                "--draft",
                "--prerelease",
                "--latest=false",
                "--title",
                tag,
                "--notes",
                (
                    "Scale performance fixture snapshot for fixture version "
                    f"{tag.removeprefix('perf-fixture-')}."
                ),
            ]
        )
        release = lookup_release(tag)
        if release is None or not release["is_draft"]:
            raise ValueError(f"GitHub did not expose the newly created draft {tag}")

    database_id = release["database_id"]
    run_gh(
        [
            "release",
            "upload",
            tag,
            *(str(path) for path in assets),
            "--clobber",
        ]
    )

    expected = sorted(path.name for path in assets)
    actual = release_asset_names(database_id)
    if actual != expected:
        raise ValueError(
            f"draft {tag} assets do not match staged files: "
            f"expected {expected}, found {actual}"
        )

    run_gh(
        [
            "release",
            "edit",
            tag,
            "--draft=false",
            "--prerelease",
            "--latest=false",
        ]
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    lookup_parser = subparsers.add_parser("lookup")
    lookup_parser.add_argument("tag")
    publish_parser = subparsers.add_parser("publish")
    publish_parser.add_argument("tag")
    publish_parser.add_argument("target")
    publish_parser.add_argument("assets", type=Path)
    args = parser.parse_args()

    try:
        if args.command == "lookup":
            release = lookup_release(args.tag)
            print(
                json.dumps(
                    {
                        "exists": release is not None,
                        "draft": release["is_draft"] if release else False,
                        "database_id": release["database_id"] if release else None,
                    }
                )
            )
        else:
            publish_release(args.tag, target=args.target, assets_dir=args.assets)
    except subprocess.CalledProcessError as error:
        detail = (error.stderr or error.stdout or "").strip()
        print(
            f"GitHub release operation failed: {detail or error}",
            file=sys.stderr,
        )
        return 1
    except (OSError, ValueError) as error:
        print(f"could not handle performance fixture release: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
