#!/usr/bin/env python3
from __future__ import annotations

import json
import shutil
import subprocess
import tempfile
from pathlib import Path


REPO = Path(__file__).resolve().parents[2]
UPDATER = REPO / ".github/scripts/update-k8s-prod-pins.py"
REVISION = "0123456789abcdef0123456789abcdef01234567"
IMAGES = [
    "ghcr.io/teagramhq/teagramd",
    "ghcr.io/teagramhq/teagram-migrations",
    "ghcr.io/teagramhq/teagramd-mc-init",
    "ghcr.io/teagramhq/link-edge-landing",
    "ghcr.io/teagramhq/link-edge-selector",
]


def run(root: Path, artifact: Path, expected_sha: str = REVISION) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [
            "python3",
            str(UPDATER),
            "--root",
            str(root),
            "--digest-file",
            str(artifact),
            "--expected-sha",
            expected_sha,
        ],
        check=False,
        capture_output=True,
        text=True,
    )


def main() -> None:
    with tempfile.TemporaryDirectory(prefix="k8s-prod-pin-test-") as temp:
        root = Path(temp)
        (root / "deploy/k8s/prod").mkdir(parents=True)
        shutil.copyfile(REPO / "deploy/k8s/prod/kustomization.yaml", root / "deploy/k8s/prod/kustomization.yaml")
        shutil.copyfile(REPO / "deploy/k8s/prod/source-revision", root / "deploy/k8s/prod/source-revision")
        (root / "migrations").mkdir()
        (root / "migrations/20261108000001_create_users.sql").write_text("-- first\n", encoding="utf-8")
        (root / "migrations/20261108000002_add_index.sql").write_text("-- latest\n", encoding="utf-8")

        artifact = root / "digests.json"
        test_digests = {name: f"{index:02x}" * 32 for index, name in enumerate(IMAGES, start=1)}
        entries = [
            {"name": name, "reference": f"{name}@sha256:{test_digests[name]}"}
            for name in IMAGES
        ]
        artifact.write_text(json.dumps({"commit": REVISION, "images": entries}), encoding="utf-8")
        result = run(root, artifact)
        if result.returncode != 0:
            raise SystemExit(result.stderr)
        if "sha=" + REVISION not in result.stdout:
            raise SystemExit("updater did not report the selected commit")

        rendered_source = (root / "deploy/k8s/prod/source-revision").read_text(encoding="utf-8")
        rendered = (root / "deploy/k8s/prod/kustomization.yaml").read_text(encoding="utf-8")
        if rendered_source != REVISION + "\n":
            raise SystemExit("updater did not set the source revision")
        for name in IMAGES:
            if f"- name: {name}\n    digest: sha256:{test_digests[name]}" not in rendered:
                raise SystemExit(f"updater did not set the digest for {name}")
        if "migration-version=20261108000002" not in rendered:
            raise SystemExit("updater did not select the newest SQL migration")
        if "job-name=db-migrations-0202020202020202" not in rendered:
            raise SystemExit("updater did not derive the migration Job name")
        if "job-name=rustfs-init-0303030303030303" not in rendered:
            raise SystemExit("updater did not derive the RustFS Job name")

        before = rendered
        artifact.write_text(
            json.dumps({"commit": REVISION, "images": entries[:-1]}), encoding="utf-8"
        )
        result = run(root, artifact)
        if result.returncode == 0:
            raise SystemExit("updater accepted an incomplete digest artifact")
        if (root / "deploy/k8s/prod/kustomization.yaml").read_text(encoding="utf-8") != before:
            raise SystemExit("invalid artifact partially changed production pins")
        artifact.write_text(json.dumps({"commit": "f" * 40, "images": entries}), encoding="utf-8")
        result = run(root, artifact)
        if result.returncode == 0:
            raise SystemExit("updater accepted digest metadata for another commit")
        if (root / "deploy/k8s/prod/kustomization.yaml").read_text(encoding="utf-8") != before:
            raise SystemExit("mismatched commit partially changed production pins")


if __name__ == "__main__":
    main()
