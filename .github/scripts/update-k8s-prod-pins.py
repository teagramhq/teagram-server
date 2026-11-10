#!/usr/bin/env python3
"""Update the production overlay from one trusted CI digest artifact."""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import tempfile
from pathlib import Path


IMAGES = {
    "ghcr.io/teagramhq/teagramd": "teagramd",
    "ghcr.io/teagramhq/teagram-migrations": "migrations",
    "ghcr.io/teagramhq/teagramd-mc-init": "mc_init",
    "ghcr.io/teagramhq/link-edge-landing": "link_landing",
    "ghcr.io/teagramhq/link-edge-selector": "link_selector",
}


def fail(message: str) -> None:
    raise ValueError(message)


def replace_once(text: str, pattern: str, replacement: str, description: str) -> str:
    updated, count = re.subn(pattern, replacement, text, flags=re.MULTILINE)
    if count != 1:
        fail(f"expected one {description}, found {count}")
    return updated


def atomic_write(path: Path, content: str) -> None:
    fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="") as output:
            output.write(content)
        os.replace(temp_name, path)
    except BaseException:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass
        raise


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", type=Path, default=Path.cwd())
    parser.add_argument("--digest-file", required=True, type=Path)
    parser.add_argument("--expected-sha", required=True)
    args = parser.parse_args()

    root = args.root.resolve()
    digest_file = args.digest_file.resolve()
    try:
        record = json.loads(digest_file.read_text(encoding="utf-8"))
        if not isinstance(record, dict):
            fail("digest artifact must be a JSON object")
        revision = record.get("commit")
        if not isinstance(revision, str) or not re.fullmatch(r"[0-9a-f]{40}", revision):
            fail("digest artifact has an invalid commit")
        if revision != args.expected_sha:
            fail("digest artifact commit does not match the selected CI commit")
        entries = record.get("images")
        if not isinstance(entries, list) or len(entries) != len(IMAGES):
            fail("digest artifact must contain exactly five image references")

        digests: dict[str, str] = {}
        for entry in entries:
            if not isinstance(entry, dict):
                fail("digest artifact contains an invalid image entry")
            name = entry.get("name")
            reference = entry.get("reference")
            if name not in IMAGES or name in digests:
                fail(f"digest artifact contains an unexpected or duplicate image: {name}")
            match = re.fullmatch(re.escape(name) + r"@(sha256:[0-9a-f]{64})", str(reference))
            if not match or match.group(1) == "sha256:" + "0" * 64:
                fail(f"digest artifact contains an invalid reference for {name}")
            digests[name] = match.group(1)
        if set(digests) != set(IMAGES):
            fail("digest artifact does not contain the required image set")

        migration_dir = root / "migrations"
        versions = [
            match.group(1)
            for path in migration_dir.glob("*.sql")
            if (match := re.match(r"([0-9]{14})_.+\.sql$", path.name))
        ]
        if not versions:
            fail("checkout has no timestamped SQL migrations")
        migration_version = max(versions)

        kustomization = root / "deploy/k8s/prod/kustomization.yaml"
        source_revision = root / "deploy/k8s/prod/source-revision"
        content = kustomization.read_text(encoding="utf-8")
        for name in IMAGES:
            image_pattern = (
                r"(^[ \t]*- name: "
                + re.escape(name)
                + r"\n[ \t]*digest: )sha256:[0-9a-f]{64}$"
            )
            content = replace_once(
                content,
                image_pattern,
                lambda match, digest=digests[name]: match.group(1) + digest,
                f"pin for {name}",
            )
        content = replace_once(
            content,
            r"(^[ \t]*- migration-version=)[0-9]{14}$",
            lambda match: match.group(1) + migration_version,
            "required migration version",
        )
        content = replace_once(
            content,
            r"(^[ \t]*- job-name=db-migrations-)[0-9a-f]{16}$",
            lambda match: match.group(1) + digests["ghcr.io/teagramhq/teagram-migrations"][7:23],
            "migration Job name",
        )
        content = replace_once(
            content,
            r"(^[ \t]*- job-name=rustfs-init-)[0-9a-f]{16}$",
            lambda match: match.group(1) + digests["ghcr.io/teagramhq/teagramd-mc-init"][7:23],
            "RustFS initializer Job name",
        )

        atomic_write(kustomization, content)
        atomic_write(source_revision, revision + "\n")
    except (OSError, json.JSONDecodeError, ValueError) as error:
        print(f"K8s production pin update failed: {error}", file=sys.stderr)
        return 1

    outputs = {
        "sha": revision,
        **{key: digests[name] for name, key in IMAGES.items()},
    }
    for key, value in outputs.items():
        print(f"{key}={value}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
