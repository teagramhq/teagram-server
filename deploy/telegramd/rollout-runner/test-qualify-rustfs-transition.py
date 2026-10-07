#!/usr/bin/env python3
from __future__ import annotations

import hashlib
import json
import os
import shlex
import shutil
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path
from typing import Any


SCRIPT_DIR = Path(__file__).resolve().parent
PROJECT_ROOT = SCRIPT_DIR.parents[2]
GATE = SCRIPT_DIR / "qualify-rustfs-transition.sh"
GATE_PY = SCRIPT_DIR / "qualify-rustfs-transition.py"
PINNED_IMAGE = (
    "rustfs/rustfs:1.0.1@sha256:"
    "1803faef57627e2d9c2e7d89d655d712ddded5389040054987163043fecb6a3c"
)
VERSIONS_60_62 = ["20261005000060", "20261005000061", "20261006000062"]
VERSIONS_60_66 = VERSIONS_60_62 + [
    "20261006000063",
    "20261007000064",
    "20261007000065",
    "20261007000066",
]
ROOT_ACCESS = "a" * 20
ROOT_SECRET = "c" * 64
APP_ACCESS = "b" * 20
APP_SECRET = "d" * 64
FILE_KEY = "02/258"
PART_KEY = "parts/aa/" + "b" * 32
TIMES = {
    "baseline": "2026-10-07T17:59:00Z",
    "freeze_start": "2026-10-07T18:00:00Z",
    "dump": "2026-10-07T18:01:00Z",
    "references": "2026-10-07T18:02:00Z",
    "schema": "2026-10-07T18:02:30Z",
    "census": "2026-10-07T18:03:00Z",
    "frozen": "2026-10-07T18:04:00Z",
    "held": "2026-10-07T18:05:00Z",
}


def gate_constants() -> dict[str, str]:
    namespace: dict[str, Any] = {"__name__": "qualify_module"}
    exec(compile(GATE_PY.read_text(encoding="utf-8"), str(GATE_PY), "exec"), namespace)
    return {
        "schema": namespace["SCHEMA"],
        "reference_query_sha256": namespace["REFERENCE_QUERY_SHA256"],
        "active_links_query_sha256": namespace["ACTIVE_LINKS_QUERY_SHA256"],
    }


def dump_json(path: Path, value: Any) -> None:
    path.write_text(json.dumps(value, sort_keys=True, separators=(",", ":")), encoding="utf-8")
    path.chmod(0o600)


def dump_bytes(path: Path, value: bytes, mode: int = 0o600) -> None:
    path.write_bytes(value)
    path.chmod(mode)


def manifest(rows: list[tuple[str, int, str]]) -> bytes:
    return b"".join(f"{key}\t{size}\t{digest}\n".encode("ascii") for key, size, digest in rows)


def new_unread_mark_schema() -> dict[str, Any]:
    return {
        "columns": {
            "owner_id": {"type": "bigint", "not_null": True, "default": None},
            "peer_type": {"type": "smallint", "not_null": True, "default": None},
            "peer_id": {"type": "bigint", "not_null": True, "default": None},
            "unread": {"type": "boolean", "not_null": True, "default": None},
            "changed_at": {"type": "timestamp with time zone", "not_null": True, "default": None},
        },
        "constraint_count": 3,
        "primary_key": ["owner_id", "peer_type", "peer_id"],
        "foreign_key": {
            "columns": ["owner_id"],
            "referenced_table": "users",
            "referenced_columns": ["id"],
            "on_delete": "CASCADE",
            "validated": True,
        },
        "check": {
            "name": "user_dialog_unread_marks_peer",
            "expression": "CHECK (((peer_type >= 1) AND (peer_type <= 3) AND (peer_id > 0)))",
            "validated": True,
        },
        "changed_index": {
            "name": "user_dialog_unread_marks_changed_idx",
            "columns": ["owner_id", "changed_at", "peer_type", "peer_id"],
            "valid": True,
            "ready": True,
            "unique": False,
        },
        "explicit_index_count": 1,
    }


def named_volumes() -> dict[str, Any]:
    return {
        "pgdata": {"name": "telegram-server_pgdata"},
        "tgkey": {"name": "telegram-server_tgkey"},
        "tgblobs": {"name": "telegram-server_tgblobs"},
    }


def base_server_service(service_name: str) -> dict[str, Any]:
    trust = "proxy-v2" if service_name.startswith("telegramd-proxy") else "socket"
    return {
        "image": "telegramd:local",
        "environment": {
            "TG_BLOB_DIR": "/var/lib/telegramd-blobs",
            "TG_CLIENT_ADDR_TRUST": trust,
            "TG_REPLICA_COUNT": "1",
            "TG_RSA_KEY_FINGERPRINT": "fixture-fingerprint",
            "TG_AUTHKEY_ENC_KEY_FILE": "/run/secrets/authkey",
        },
        "ports": [{"target": 2443, "published": "2443", "host_ip": "127.0.0.1", "protocol": "tcp"}],
        "stop_grace_period": "120s",
        "volumes": [
            {"type": "volume", "source": "tgkey", "target": "/var/lib/telegramd", "read_only": False},
            {
                "type": "volume",
                "source": "tgblobs",
                "target": "/var/lib/telegramd-blobs",
                "read_only": False,
            },
        ],
        "depends_on": {"migrate": {"condition": "service_completed_successfully"}},
    }


def compose_documents(candidate_root: Path) -> tuple[dict[str, Any], dict[str, Any]]:
    baseline = {
        "services": {
            "postgres": {
                "image": "postgres:16-alpine",
                "volumes": [{"type": "volume", "source": "pgdata", "target": "/var/lib/postgresql/data"}],
            },
            "migrate": {"image": "arigaio/atlas:1.2.0-alpine", "command": ["migrate", "apply"]},
            "telegramd": base_server_service("telegramd"),
            "telegramd-proxy": base_server_service("telegramd-proxy"),
        },
        "volumes": named_volumes(),
        "secrets": {},
        "networks": {"default": {"name": "telegram-server_default"}},
    }
    candidate = json.loads(json.dumps(baseline))
    candidate["services"]["rustfs"] = {
        "image": PINNED_IMAGE,
        "environment": {
            "RUSTFS_VOLUMES": "/data",
            "RUSTFS_ADDRESS": "0.0.0.0:9000",
            "RUSTFS_CONSOLE_ENABLE": "false",
            "RUSTFS_ACCESS_KEY_FILE": "/run/secrets/rustfs-root-access-key",
            "RUSTFS_SECRET_KEY_FILE": "/run/secrets/rustfs-root-secret-key",
            "RUSTFS_OBS_LOGGER_LEVEL": "warn",
        },
        "volumes": [{"type": "volume", "source": "rustfsdata", "target": "/data", "read_only": False}],
        "secrets": [
            {
                "source": "rustfs_root_access_key",
                "target": "rustfs-root-access-key",
                "uid": "10001",
                "gid": "10001",
                "mode": 0o400,
            },
            {
                "source": "rustfs_root_secret_key",
                "target": "rustfs-root-secret-key",
                "uid": "10001",
                "gid": "10001",
                "mode": 0o400,
            },
        ],
        "healthcheck": {
            "test": ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:9000/health/ready"],
            "interval": "2s",
            "timeout": "5s",
            "retries": 30,
            "start_period": "5s",
        },
        "restart": "unless-stopped",
        "logging": {"driver": "json-file", "options": {"max-size": "10m", "max-file": "3"}},
    }
    policy = os.path.realpath(candidate_root / "deploy" / "rustfs" / "telegramd-blob.json")
    candidate["services"]["rustfs-init"] = {
        "image": "telegramd-mc-init:local",
        "build": {"context": str(candidate_root), "dockerfile": "Dockerfile.mc"},
        "entrypoint": ["/bin/sh", "/usr/local/bin/rustfs-init.sh"],
        "environment": {"TG_BLOB_S3_ACCESS_KEY_ID": APP_ACCESS},
        "volumes": [
            {
                "type": "bind",
                "source": policy,
                "target": "/policy/telegramd-blob.json",
                "read_only": True,
                "bind": {"create_host_path": True},
            }
        ],
        "secrets": [
            {"source": "rustfs_root_access_key", "target": "rustfs-root-access-key"},
            {"source": "rustfs_root_secret_key", "target": "rustfs-root-secret-key"},
            {"source": "telegramd_blob_secret_key", "target": "telegramd-blob-secret-key"},
        ],
        "depends_on": {"rustfs": {"condition": "service_healthy"}},
    }
    s3_environment = {
        "TG_BLOB_S3_ENDPOINT": "http://rustfs:9000",
        "TG_BLOB_S3_BUCKET": "telegram",
        "TG_BLOB_S3_PREFIX": "telegramd/",
        "TG_BLOB_S3_REGION": "us-east-1",
        "TG_BLOB_S3_ACCESS_KEY_ID": APP_ACCESS,
        "TG_BLOB_S3_SECRET_ACCESS_KEY_FILE": "/run/secrets/telegramd-blob-secret-key",
        "TG_BLOB_S3_ALLOW_INSECURE_HTTP": "true",
    }
    for name, source, target, read_only in (
        ("blob-migrate", "/source", "/source", True),
        ("blob-restore", "/destination", "/destination", False),
    ):
        command = ["--source", "/source"] if name == "blob-migrate" else ["--direction", "s3-to-local", "--destination", "/destination"]
        candidate["services"][name] = {
            "image": "telegramd:local",
            "entrypoint": ["/usr/local/bin/blob-migrate"],
            "command": command,
            "profiles": ["migration"],
            "environment": s3_environment,
            "secrets": [{"source": "telegramd_blob_secret_key", "target": "telegramd-blob-secret-key"}],
            "volumes": [
                {"type": "volume", "source": "tgblobs", "target": target, "read_only": read_only}
            ],
            "depends_on": {"rustfs-init": {"condition": "service_completed_successfully"}},
        }
    candidate["volumes"]["rustfsdata"] = {"name": f"{candidate_root.name}_rustfsdata"}
    candidate["secrets"] = {
        "rustfs_root_access_key": {"environment": "RUSTFS_ROOT_ACCESS_KEY"},
        "rustfs_root_secret_key": {"environment": "RUSTFS_ROOT_SECRET_KEY"},
        "telegramd_blob_secret_key": {
            "file": os.path.realpath(candidate_root / ".secrets" / "telegramd-blob-secret-key")
        },
    }
    for name, service in candidate["services"].items():
        if not name.startswith("telegramd"):
            continue
        service["environment"].update(s3_environment)
        service["volumes"].append(
            {
                "type": "bind",
                "source": os.path.realpath(candidate_root / ".state" / "blob-mode"),
                "target": "/run/telegramd/blob-mode",
                "read_only": True,
                "bind": {"create_host_path": True},
            }
        )
        service["volumes"][1]["read_only"] = True
        service["secrets"] = [{"source": "telegramd_blob_secret_key", "target": "telegramd-blob-secret-key"}]
        service["depends_on"]["rustfs-init"] = {"condition": "service_completed_successfully"}
    return baseline, candidate


def capture_compose(checkout: Path, compose_file: str) -> dict[str, Any]:
    docker = shutil.which("docker")
    if docker is None:
        raise AssertionError("Docker Compose is required to capture the resolved Compose fixture")
    environment = {
        "HOME": str(Path.home()),
        "PATH": f"{Path(docker).resolve().parent}{os.pathsep}{os.defpath}",
    }
    command = [
        docker,
        "compose",
        "--project-directory",
        str(checkout),
        "--env-file",
        ".env",
        "-f",
        compose_file,
        "-f",
        "docker-compose.override.yml",
        "--profile",
        "*",
        "config",
        "--format",
        "json",
    ]
    result = subprocess.run(
        command,
        cwd=checkout,
        env=environment,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        raise AssertionError("Docker Compose could not resolve the fixture model")
    try:
        resolved = json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        raise AssertionError("Docker Compose returned invalid fixture JSON") from exc
    if not isinstance(resolved, dict):
        raise AssertionError("Docker Compose returned a non-object fixture")
    return resolved


def add_unapproved_compose_service(checkout: Path) -> bytes:
    compose_path = checkout / "docker-compose.yml"
    model = json.loads(compose_path.read_text(encoding="utf-8"))
    model["services"]["unapproved"] = {
        "image": "busybox",
        "volumes": [{"type": "bind", "source": "/", "target": "/host", "read_only": True}],
    }
    changed = json.dumps(model, sort_keys=True, separators=(",", ":")).encode("utf-8")
    dump_bytes(compose_path, changed, mode=0o644)
    return changed


def running_container(identifier: str, service: str, mounts: list[dict[str, Any]], environment: dict[str, Any]) -> dict[str, Any]:
    return {
        "id": identifier,
        "name": f"/{service}-1",
        "service": service,
        "running": True,
        "mounts": mounts,
        "environment": environment,
    }


def baseline_inventory() -> dict[str, Any]:
    blobs = {"type": "volume", "source": "telegram-server_tgblobs", "target": "/var/lib/telegramd-blobs", "rw": True}
    keys = {"type": "volume", "source": "telegram-server_tgkey", "target": "/var/lib/telegramd", "rw": True}
    env = {"TG_BLOB_DIR": "/var/lib/telegramd-blobs", "TG_BLOB_S3_ENDPOINT": ""}
    return {
        "complete": True,
        "captured_at": TIMES["baseline"],
        "containers": [
            running_container("container-main", "telegramd", [keys, blobs], env),
            running_container("container-proxy", "telegramd-proxy", [keys, blobs], env),
            running_container(
                "container-postgres",
                "postgres",
                [{"type": "volume", "source": "telegram-server_pgdata", "target": "/var/lib/postgresql/data", "rw": True}],
                {},
            ),
        ],
    }


def frozen_inventory() -> dict[str, Any]:
    return {
        "complete": True,
        "captured_at": TIMES["frozen"],
        "containers": [
            running_container(
                "container-postgres-frozen",
                "postgres",
                [{"type": "volume", "source": "telegram-server_pgdata", "target": "/var/lib/postgresql/data", "rw": True}],
                {},
            )
        ],
    }


def good_migration_evidence() -> dict[str, Any]:
    return {
        "baseline_revisions": VERSIONS_60_62,
        "revision_rows": {version: True for version in VERSIONS_60_66},
        "target_revisions": VERSIONS_60_66,
        "approved_revision_set_exact": True,
        "migration_66_present": True,
        "migration_66_schema": new_unread_mark_schema(),
    }


def write_bundle(root: Path, scenario: str = "success") -> tuple[Path, Path, Path, str]:
    bundle = root / "bundle"
    checkout = root / "candidate-checkout"
    mock_bin = root / "mock-bin"
    bundle.mkdir(mode=0o700)
    checkout.mkdir(mode=0o700)
    mock_bin.mkdir(mode=0o700)
    (checkout / "migrations").mkdir(mode=0o700)
    (checkout / ".secrets").mkdir(mode=0o700)
    (checkout / ".state" / "blob-mode").mkdir(mode=0o700, parents=True)
    (checkout / "deploy" / "rustfs").mkdir(mode=0o700, parents=True)
    (bundle / "candidate-secrets").mkdir(mode=0o700)
    shutil.copyfile(
        PROJECT_ROOT / "deploy" / "rustfs" / "telegramd-blob.json",
        checkout / "deploy" / "rustfs" / "telegramd-blob.json",
    )
    shutil.copyfile(PROJECT_ROOT / "migrations" / "atlas.sum", checkout / "migrations" / "atlas.sum")
    (checkout / "migrations" / "atlas.sum").chmod(0o600)
    for source in (PROJECT_ROOT / "migrations").glob("*.sql"):
        destination = checkout / "migrations" / source.name
        shutil.copyfile(source, destination)
        destination.chmod(0o600)

    base_env = b"POSTGRES_PASSWORD=stable-value\nTG_RSA_KEY_FINGERPRINT=stable-fingerprint\n"
    candidate_env = base_env + (
        f"RUSTFS_ROOT_ACCESS_KEY={ROOT_ACCESS}\n"
        f"RUSTFS_ROOT_SECRET_KEY={ROOT_SECRET}\n"
        f"TG_BLOB_S3_ACCESS_KEY_ID={APP_ACCESS}\n"
        f"TG_BLOB_S3_SECRET_ACCESS_KEY={APP_SECRET}\n"
    ).encode("ascii")
    dump_bytes(bundle / "baseline.env", base_env)
    dump_bytes(bundle / "candidate.env", candidate_env)
    dump_bytes(checkout / ".env", candidate_env)
    override = b"services:\n  telegramd:\n    environment:\n      TG_SYNTHETIC_FIXTURE: unchanged\n"
    dump_bytes(bundle / "baseline.override.yml", override)
    dump_bytes(bundle / "candidate.override.yml", override)
    dump_bytes(checkout / "docker-compose.override.yml", override)
    dump_bytes(checkout / ".secrets" / "telegramd-blob-secret-key", APP_SECRET.encode("ascii"), mode=0o444)
    dump_bytes(bundle / "candidate-secrets" / "telegramd-blob-secret-key", APP_SECRET.encode("ascii"), mode=0o444)
    secret_dir = checkout / ".secrets"
    secret_dir.chmod(0o700)

    baseline_compose, candidate_compose = compose_documents(checkout)
    if scenario == "proxy-missing-mode":
        candidate_compose["services"]["telegramd-proxy"]["volumes"] = [
            volume
            for volume in candidate_compose["services"]["telegramd-proxy"]["volumes"]
            if volume.get("target") != "/run/telegramd/blob-mode"
        ]
    elif scenario == "unexpected-mount":
        candidate_compose["services"]["telegramd"]["volumes"].append(
            {"type": "bind", "source": "/etc", "target": "/etc", "read_only": True}
        )
    elif scenario == "other-service-mode-mount":
        candidate_compose["services"]["postgres"]["volumes"].append(
            {
                "type": "bind",
                "source": os.path.realpath(checkout / ".state" / "blob-mode"),
                "target": "/run/telegramd/blob-mode",
                "read_only": True,
            }
        )
    elif scenario == "extra-migration-env":
        candidate_compose["services"]["blob-migrate"]["environment"]["UNAPPROVED_SETTING"] = "value"
    elif scenario == "unapproved-digest":
        candidate_compose["services"]["rustfs"]["image"] = "rustfs/rustfs:latest"
    elif scenario == "rustfsdata-alias":
        candidate_compose["volumes"]["rustfsdata"] = {"name": "telegram-server_tgblobs"}

    for service in candidate_compose["services"].values():
        environment = service.get("environment")
        if isinstance(environment, dict) and "TG_BLOB_S3_ACCESS_KEY_ID" in environment:
            environment["TG_BLOB_S3_ACCESS_KEY_ID"] = "${TG_BLOB_S3_ACCESS_KEY_ID}"

    dump_json(checkout / "baseline-compose-source.json", baseline_compose)
    compose_input = json.dumps(candidate_compose, sort_keys=True, separators=(",", ":")).encode("utf-8")
    dump_bytes(checkout / "docker-compose.yml", compose_input, mode=0o644)
    baseline_compose = capture_compose(checkout, "baseline-compose-source.json")
    candidate_compose = capture_compose(checkout, "docker-compose.yml")
    dump_json(bundle / "baseline-compose.json", baseline_compose)
    dump_json(bundle / "candidate-compose.json", candidate_compose)

    source_rows = [(FILE_KEY, 5, "a" * 64), (PART_KEY, 3, "b" * 64)]
    dump_content = b"synthetic private postgres dump"
    dump_bytes(bundle / "postgres.dump", dump_content)
    for name in ("source-provisional.tsv", "source-frozen.tsv", "copy-pass-1.tsv", "copy-pass-2.tsv", "destination-census.tsv"):
        dump_bytes(bundle / name, manifest(source_rows))
    refs = f"file\ttrue\t{FILE_KEY}\nupload_part\ttrue\t{PART_KEY}\n".encode("ascii")
    if scenario == "unstored-file-absent":
        refs = (
            f"file\ttrue\t{FILE_KEY}\n"
            "file\tfalse\t03/259\n"
            f"upload_part\ttrue\t{PART_KEY}\n"
        ).encode("ascii")
    links = f"channel_messages\t258\ttrue\nmessages\t258\tfalse\n".encode("ascii")
    dump_bytes(bundle / "references.tsv", refs)
    dump_bytes(bundle / "active-links.tsv", links)

    metadata = {
        "schema": gate_constants()["schema"],
        "source_volume": "telegram-server_tgblobs",
        "candidate_compose_binding": {
            "snapshot_sha256": hashlib.sha256(
                json.dumps(candidate_compose, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
            ).hexdigest(),
            "inputs_sha256": {
                ".env": hashlib.sha256(candidate_env).hexdigest(),
                "docker-compose.override.yml": hashlib.sha256(override).hexdigest(),
                "docker-compose.yml": hashlib.sha256(compose_input).hexdigest(),
            },
        },
        "freeze": {
            "held": True,
            "inventory_complete": True,
            "started_at": TIMES["freeze_start"],
            "held_at": TIMES["held"],
            "dump_captured_at": TIMES["dump"],
            "source_frozen_census_at": TIMES["census"],
            "references_captured_at": TIMES["references"],
            "schema_captured_at": TIMES["schema"],
            "postgres_dump": {
                "exit_status": 0,
                "completion_marker": True,
                "isolated_restore_exit_status": 0,
                "isolated_restore_network": "none",
                "sha256": hashlib.sha256(dump_content).hexdigest(),
            },
        },
        "references": {
            "candidate_query_sha256": gate_constants()["reference_query_sha256"],
            "active_links_query_sha256": gate_constants()["active_links_query_sha256"],
        },
    }
    dump_json(bundle / "qualification.json", metadata)
    dump_json(bundle / "baseline-containers.json", baseline_inventory())
    frozen = frozen_inventory()
    migrations = good_migration_evidence()

    if scenario == "forbidden-override":
        forbidden = override + b"  TG_BLOB_S3_ENDPOINT: http://unexpected\n"
        dump_bytes(bundle / "candidate.override.yml", forbidden)
        dump_bytes(checkout / "docker-compose.override.yml", forbidden)
    elif scenario == "mixed-trust-writer":
        proxy = running_container(
            "writer-proxy",
            "telegramd-proxy",
            [{"type": "volume", "source": "telegram-server_tgblobs", "target": "/var/lib/telegramd-blobs", "rw": True}],
            {"TG_BLOB_S3_ACCESS_KEY_ID": APP_ACCESS},
        )
        frozen["containers"].append(proxy)
    elif scenario == "wrong-inspected-volume":
        baseline_inventory_doc = baseline_inventory()
        baseline_inventory_doc["containers"][1]["mounts"][1]["source"] = "substituted-volume"
        dump_json(bundle / "baseline-containers.json", baseline_inventory_doc)
    elif scenario == "mismatched-blob-directory":
        baseline_inventory_doc = baseline_inventory()
        baseline_inventory_doc["containers"][1]["environment"]["TG_BLOB_DIR"] = "/var/lib/telegramd"
        dump_json(bundle / "baseline-containers.json", baseline_inventory_doc)
    elif scenario == "changed-census":
        dump_bytes(bundle / "source-frozen.tsv", manifest([(FILE_KEY, 5, "f" * 64), (PART_KEY, 3, "b" * 64)]))
    elif scenario == "missing-reference":
        empty_source = [(PART_KEY, 3, "b" * 64)]
        for name in ("source-provisional.tsv", "source-frozen.tsv", "copy-pass-1.tsv", "copy-pass-2.tsv", "destination-census.tsv"):
            dump_bytes(bundle / name, manifest(empty_source))
    elif scenario == "leftover-temp":
        temporary = [("02/258.tmp", 1, "c" * 64), (PART_KEY, 3, "b" * 64)]
        for name in ("source-provisional.tsv", "source-frozen.tsv", "copy-pass-1.tsv", "copy-pass-2.tsv", "destination-census.tsv"):
            dump_bytes(bundle / name, manifest(temporary))
    elif scenario == "unrelated-env-drift":
        candidate_env = candidate_env.replace(b"POSTGRES_PASSWORD=stable-value", b"POSTGRES_PASSWORD=changed-value")
        dump_bytes(bundle / "candidate.env", candidate_env)
        dump_bytes(checkout / ".env", candidate_env)
    elif scenario == "secret-change":
        dump_bytes(bundle / "candidate-secrets" / "telegramd-blob-secret-key", b"e" * 64, mode=0o444)
        dump_bytes(checkout / ".secrets" / "telegramd-blob-secret-key", b"e" * 64, mode=0o444)
    elif scenario == "wrong-source-identity":
        metadata["source_volume"] = ""
        dump_json(bundle / "qualification.json", metadata)
    elif scenario == "dump-outside-freeze":
        metadata["freeze"]["dump_captured_at"] = "2026-10-07T17:59:59Z"
        dump_json(bundle / "qualification.json", metadata)
    elif scenario in ("missing-66", "wrong-version-66", "extra-67"):
        if scenario == "missing-66":
            migrations["revision_rows"]["20261007000066"] = False
            migrations["target_revisions"] = VERSIONS_60_66[:-1]
        elif scenario == "wrong-version-66":
            migrations["revision_rows"]["20261007000066"] = False
            migrations["revision_rows"]["20261007000067"] = True
            migrations["target_revisions"] = VERSIONS_60_66[:-1] + ["20261007000067"]
        else:
            migrations["revision_rows"]["20261007000067"] = True
            migrations["target_revisions"] = VERSIONS_60_66 + ["20261007000067"]
        migrations["approved_revision_set_exact"] = False
        migrations["migration_66_present"] = False if scenario != "extra-67" else True
    elif scenario == "wrong-unread-schema":
        migrations["migration_66_schema"]["columns"]["peer_id"]["type"] = "integer"

    if scenario == "changed-migration-file":
        path = checkout / "migrations" / "20261007000066_dialog_unread_marks.sql"
        path.write_bytes(path.read_bytes() + b"-- unreviewed change\n")
    elif scenario == "extra-migration-file":
        extra = checkout / "migrations" / "20261007000067_unreviewed.sql"
        extra.write_text("SELECT 1;\n", encoding="utf-8")
        extra.chmod(0o600)
    elif scenario == "tampered-atlas-sum":
        path = checkout / "migrations" / "atlas.sum"
        path.write_bytes(path.read_bytes() + b"\n")
    elif scenario == "overbroad-policy":
        policy_path = checkout / "deploy" / "rustfs" / "telegramd-blob.json"
        policy_path.write_text(
            policy_path.read_text(encoding="utf-8").replace("s3:DeleteObject", "s3:*"),
            encoding="utf-8",
        )
    elif scenario == "empty-reference-coverage":
        for name in ("source-provisional.tsv", "source-frozen.tsv", "copy-pass-1.tsv", "copy-pass-2.tsv", "destination-census.tsv"):
            dump_bytes(bundle / name, b"")
        dump_bytes(bundle / "references.tsv", b"")
        dump_bytes(bundle / "active-links.tsv", b"")
    elif scenario == "stale-compose-input":
        add_unapproved_compose_service(checkout)
    elif scenario == "stale-compose-snapshot":
        candidate_compose["services"]["postgres"]["volumes"].append(
            {"type": "bind", "source": "/", "target": "/host", "read_only": True}
        )
        dump_json(bundle / "candidate-compose.json", candidate_compose)
    elif scenario == "compose-resolution-mismatch":
        changed_input = add_unapproved_compose_service(checkout)
        metadata["candidate_compose_binding"]["inputs_sha256"]["docker-compose.yml"] = hashlib.sha256(
            changed_input
        ).hexdigest()
        dump_json(bundle / "qualification.json", metadata)

    dump_json(bundle / "frozen-containers.json", frozen)
    dump_json(bundle / "migrations.json", migrations)

    events = root / "mock-events.log"
    real_docker = shutil.which("docker")
    if real_docker is None:
        raise AssertionError("Docker is required to execute the Compose gate fixture")
    docker = mock_bin / "docker"
    docker.write_text(
        "#!/bin/sh\n"
        f"events={shlex.quote(str(events))}\n"
        f"real_docker={shlex.quote(str(Path(real_docker).resolve()))}\n"
        "key=${TG_BLOB_S3_ACCESS_KEY_ID-unset}\n"
        "printf 'docker %s TG_BLOB_S3_ACCESS_KEY_ID=%s\\n' \"$*\" \"$key\" >> \"$events\"\n"
        "exec \"$real_docker\" \"$@\"\n",
        encoding="utf-8",
    )
    docker.chmod(0o700)
    git = mock_bin / "git"
    git.write_text(f"#!/bin/sh\nprintf 'git %s\\n' \"$*\" >> {shlex.quote(str(events))}\nexit 99\n", encoding="utf-8")
    git.chmod(0o700)
    return bundle, checkout, mock_bin, str(events)


def tree_digest(root: Path) -> str:
    digest = hashlib.sha256()
    for path in sorted(root.rglob("*")):
        relative = path.relative_to(root).as_posix()
        info = path.lstat()
        digest.update(relative.encode("utf-8") + b"\0")
        digest.update(str(stat.S_IMODE(info.st_mode)).encode("ascii") + b"\0")
        if stat.S_ISREG(info.st_mode):
            digest.update(path.read_bytes())
        elif stat.S_ISLNK(info.st_mode):
            digest.update(os.readlink(path).encode("utf-8"))
    return digest.hexdigest()


class QualificationFixtures(unittest.TestCase):
    def run_scenario(self, scenario: str, expected_reason: str | None = None) -> subprocess.CompletedProcess[str]:
        temp = tempfile.TemporaryDirectory(prefix="rustfs-transition-gate.", dir=os.environ.get("TMPDIR", "/root"))
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        bundle, checkout, mock_bin, events = write_bundle(root, scenario)
        if scenario == "success":
            captured = json.loads((bundle / "candidate-compose.json").read_text(encoding="utf-8"))
            baseline = json.loads((bundle / "baseline-compose.json").read_text(encoding="utf-8"))
            self.assertEqual(captured["name"], baseline["name"])
            self.assertNotIn("secrets", baseline)
            self.assertNotIn("read_only", baseline["services"]["telegramd"]["volumes"][1])
            self.assertEqual(
                set(captured["services"]) - set(baseline["services"]),
                {"rustfs", "rustfs-init", "blob-migrate", "blob-restore"},
            )
            self.assertEqual(
                captured["services"]["rustfs"]["secrets"][0]["mode"],
                "0400",
            )
            self.assertEqual(
                captured["secrets"]["rustfs_root_access_key"]["name"],
                f"{captured['name']}_rustfs_root_access_key",
            )
            self.assertEqual(
                captured["services"]["rustfs-init"]["depends_on"]["rustfs"],
                {"condition": "service_healthy", "required": True},
            )
            self.assertEqual(
                captured["services"]["telegramd"]["depends_on"]["rustfs-init"],
                {"condition": "service_completed_successfully", "required": True},
            )
        before_bundle = tree_digest(bundle)
        before_checkout = tree_digest(checkout)
        environment = os.environ.copy()
        environment["PATH"] = f"{mock_bin}:{environment['PATH']}"
        environment["MOCK_EVENTS"] = events
        if scenario == "inherited-compose-override":
            environment["TG_BLOB_S3_ACCESS_KEY_ID"] = "inherited-value"
        result = subprocess.run(
            ["bash", str(GATE), "check", str(bundle), str(checkout)],
            env=environment,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(tree_digest(bundle), before_bundle, "qualification changed its private input tree")
        self.assertEqual(tree_digest(checkout), before_checkout, "qualification changed its candidate checkout")
        event_text = Path(events).read_text(encoding="utf-8") if Path(events).exists() else ""
        self.assertNotIn("git ", event_text, "qualification called Git")
        for line in event_text.splitlines():
            self.assertIn("docker compose", line)
            self.assertIn("--env-file .env", line)
            self.assertIn("--project-directory", line)
            self.assertIn("-f docker-compose.yml", line)
            self.assertIn("-f docker-compose.override.yml", line)
            self.assertIn("--profile *", line)
            self.assertIn("config --format json", line)
            self.assertIn("TG_BLOB_S3_ACCESS_KEY_ID=unset", line)
        if expected_reason is None:
            self.assertIn("docker compose", event_text)
        self.assertNotIn(FILE_KEY, result.stdout + result.stderr)
        self.assertNotIn(PART_KEY, result.stdout + result.stderr)
        self.assertNotIn(ROOT_ACCESS, result.stdout + result.stderr)
        self.assertNotIn(ROOT_SECRET, result.stdout + result.stderr)
        self.assertNotIn(APP_ACCESS, result.stdout + result.stderr)
        self.assertNotIn(APP_SECRET, result.stdout + result.stderr)
        if expected_reason is not None:
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("gate_result=reject", result.stderr)
            self.assertIn(f"reason={expected_reason}", result.stderr)
        return result

    def test_approved_bundle_passes_with_aggregate_only_output(self) -> None:
        result = self.run_scenario("success")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("gate_result=pass", result.stdout)
        self.assertIn("objects=2 bytes=8 references=2 active_links=1", result.stdout)
        self.assertIn("compose_inputs_sha256=", result.stdout)

    def test_forbidden_override_is_rejected(self) -> None:
        self.run_scenario("forbidden-override", "protected_override")

    def test_mixed_trust_writer_is_rejected_during_freeze(self) -> None:
        self.run_scenario("mixed-trust-writer", "writer_freeze_incomplete")

    def test_substituted_volume_identity_is_rejected(self) -> None:
        self.run_scenario("wrong-inspected-volume", "source_identity")

    def test_mismatched_live_blob_directory_is_rejected(self) -> None:
        self.run_scenario("mismatched-blob-directory", "source_identity")

    def test_empty_source_identity_is_rejected(self) -> None:
        self.run_scenario("wrong-source-identity", "source_identity")

    def test_rustfs_volume_cannot_alias_a_baseline_volume(self) -> None:
        self.run_scenario("rustfsdata-alias", "source_identity")

    def test_changed_frozen_census_is_rejected(self) -> None:
        self.run_scenario("changed-census", "source_census_changed")

    def test_missing_reference_is_rejected_even_for_empty_copy_output(self) -> None:
        self.run_scenario("missing-reference", "reference_coverage")

    def test_unstored_file_without_blob_does_not_block_qualification(self) -> None:
        result = self.run_scenario("unstored-file-absent")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("gate_result=pass", result.stdout)
        self.assertIn("references=3", result.stdout)

    def test_proxy_without_exact_mount_is_rejected(self) -> None:
        self.run_scenario("proxy-missing-mode", "configuration_mismatch")

    def test_unexpected_mount_is_rejected(self) -> None:
        self.run_scenario("unexpected-mount", "configuration_mismatch")

    def test_blob_mode_mount_on_other_service_is_rejected(self) -> None:
        self.run_scenario("other-service-mode-mount", "configuration_mismatch")

    def test_extra_migration_service_environment_is_rejected(self) -> None:
        self.run_scenario("extra-migration-env", "configuration_mismatch")

    def test_unpinned_rustfs_digest_is_rejected(self) -> None:
        self.run_scenario("unapproved-digest", "configuration_mismatch")

    def test_stale_compose_input_is_rejected(self) -> None:
        self.run_scenario("stale-compose-input", "compose_binding_mismatch")

    def test_stale_compose_snapshot_is_rejected(self) -> None:
        self.run_scenario("stale-compose-snapshot", "compose_binding_mismatch")

    def test_current_compose_resolution_must_match_self_reported_hashes(self) -> None:
        self.run_scenario("compose-resolution-mismatch", "compose_binding_mismatch")

    def test_inherited_compose_override_is_cleared_before_resolution(self) -> None:
        result = self.run_scenario("inherited-compose-override")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("gate_result=pass", result.stdout)

    def test_unrelated_env_drift_is_rejected(self) -> None:
        self.run_scenario("unrelated-env-drift", "env_drift")

    def test_changed_app_secret_is_rejected(self) -> None:
        self.run_scenario("secret-change", "secret_mismatch")

    def test_leftover_write_temp_is_rejected(self) -> None:
        self.run_scenario("leftover-temp", "manifest_invalid")

    def test_dump_outside_freeze_is_rejected(self) -> None:
        self.run_scenario("dump-outside-freeze", "dump_invalid")

    def test_missing_migration_66_is_rejected(self) -> None:
        self.run_scenario("missing-66", "schema_rejected")

    def test_wrong_version_in_place_of_66_is_rejected(self) -> None:
        self.run_scenario("wrong-version-66", "schema_rejected")

    def test_extra_migration_67_is_rejected(self) -> None:
        self.run_scenario("extra-67", "schema_rejected")

    def test_wrong_unread_mark_schema_is_rejected(self) -> None:
        self.run_scenario("wrong-unread-schema", "schema_rejected")

    def test_changed_migration_66_file_is_rejected(self) -> None:
        self.run_scenario("changed-migration-file", "schema_rejected")

    def test_extra_migration_file_is_rejected(self) -> None:
        self.run_scenario("extra-migration-file", "schema_rejected")

    def test_tampered_atlas_sum_is_rejected(self) -> None:
        self.run_scenario("tampered-atlas-sum", "schema_rejected")

    def test_overbroad_rustfs_policy_is_rejected(self) -> None:
        self.run_scenario("overbroad-policy", "configuration_mismatch")

    def test_empty_manifests_are_not_reference_coverage(self) -> None:
        self.run_scenario("empty-reference-coverage", "reference_coverage")


if __name__ == "__main__":
    if os.geteuid() != 0:
        raise SystemExit("run RustFS qualification fixtures as root")
    suite = unittest.defaultTestLoader.loadTestsFromTestCase(QualificationFixtures)
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    print(f"fixture_summary=passed:{result.testsRun - len(result.failures) - len(result.errors)} failed:{len(result.failures) + len(result.errors)}")
    raise SystemExit(0 if result.wasSuccessful() else 1)
