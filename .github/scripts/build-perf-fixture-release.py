#!/usr/bin/env python3
"""Stage every cached performance fixture file as a checksummed release asset."""

import argparse
import hashlib
import json
import sys
from pathlib import Path


MAX_ASSET_BYTES = 1_900_000_000
CHUNK_BYTES = 1024 * 1024


def copy_and_hash(source: Path, target: Path) -> str:
    digest = hashlib.sha256()
    with source.open("rb") as src, target.open("wb") as dst:
        while chunk := src.read(CHUNK_BYTES):
            dst.write(chunk)
            digest.update(chunk)
    return digest.hexdigest()


def stage_file(source: Path, relative: Path, output: Path) -> list[dict[str, str]]:
    name = relative.as_posix().replace("/", "__")
    if source.stat().st_size <= MAX_ASSET_BYTES:
        target = output / name
        digest = copy_and_hash(source, target)
        return [{"name": name, "sha256": digest}]

    assets = []
    with source.open("rb") as src:
        part = 0
        while True:
            part_name = f"{name}.part{part:03d}"
            target = output / part_name
            digest = hashlib.sha256()
            bytes_written = 0
            with target.open("wb") as dst:
                while bytes_written < MAX_ASSET_BYTES:
                    chunk = src.read(min(CHUNK_BYTES, MAX_ASSET_BYTES - bytes_written))
                    if not chunk:
                        break
                    dst.write(chunk)
                    digest.update(chunk)
                    bytes_written += len(chunk)
            if bytes_written == 0:
                target.unlink()
                break
            assets.append({"name": part_name, "sha256": digest.hexdigest()})
            part += 1
            if bytes_written < MAX_ASSET_BYTES:
                break
    return assets


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--snapshot", type=Path, required=True)
    parser.add_argument("--metadata", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()

    try:
        metadata = json.loads(args.metadata.read_text(encoding="utf-8"))
        fixture = metadata["fixture"]
        server_commit = metadata["server_commit"]
        migration_revision = metadata["migration_revision"]
        if not fixture or not server_commit or not migration_revision:
            raise ValueError("fixture, server_commit, and migration_revision must be non-empty")
        if not args.snapshot.is_dir():
            raise ValueError(f"snapshot directory does not exist: {args.snapshot}")

        args.output.mkdir(parents=True, exist_ok=False)
        files = sorted(args.snapshot.rglob("*"))
        symlinks = [path for path in files if path.is_symlink()]
        if symlinks:
            raise ValueError(f"snapshot contains a symlink: {symlinks[0]}")
        sources = [path for path in files if path.is_file()]
        if not sources:
            raise ValueError(f"snapshot has no files: {args.snapshot}")

        names = {"manifest.json"}
        assets = []
        for source in sources:
            relative = source.relative_to(args.snapshot)
            staged = stage_file(source, relative, args.output)
            for asset in staged:
                if asset["name"] in names:
                    raise ValueError(f"asset name collision: {asset['name']}")
                names.add(asset["name"])
                assets.append(asset)

        manifest = {
            "fixture": fixture,
            "server_commit": server_commit,
            "migration_revision": migration_revision,
            "assets": assets,
        }
        (args.output / "manifest.json").write_text(
            json.dumps(manifest, indent=2) + "\n", encoding="utf-8"
        )
    except (OSError, KeyError, json.JSONDecodeError, ValueError) as error:
        print(f"could not stage performance fixture release: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
