#!/usr/bin/env python3
from __future__ import annotations

import hashlib
import json
import os
import re
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
FROZEN_MIGRATIONS = SCRIPT_DIR / "testdata" / "release-60-66"
FROZEN_MIGRATIONS_67 = SCRIPT_DIR / "testdata" / "release-60-67"
FROZEN_MIGRATIONS_69 = SCRIPT_DIR / "testdata" / "release-60-69"
FROZEN_MIGRATIONS_70 = SCRIPT_DIR / "testdata" / "release-60-70"
GATE = SCRIPT_DIR / "qualify-rustfs-transition.sh"
GATE_PY = SCRIPT_DIR / "qualify-rustfs-transition.py"
LIVE_MIGRATION_67 = "20261008000067_secret_chat_party_date_idx.sql"
LIVE_MIGRATION_68 = "20261008000068_files_owner_ownership_backstop.sql"
LIVE_MIGRATION_69 = "20261008000069_profile_photo_gallery.sql"
LIVE_MIGRATION_70 = "20261008000070_erasure_outbox_epoch_markers.sql"
PINNED_IMAGE = (
    "mirror.gcr.io/rustfs/rustfs:1.0.1@sha256:"
    "1803faef57627e2d9c2e7d89d655d712ddded5389040054987163043fecb6a3c"
)
VERSIONS_60_62 = ["20261005000060", "20261005000061", "20261006000062"]
VERSIONS_60_66 = VERSIONS_60_62 + [
    "20261006000063",
    "20261007000064",
    "20261007000065",
    "20261007000066",
]
VERSIONS_60_65 = VERSIONS_60_66[:-1]
VERSIONS_60_67 = VERSIONS_60_66 + ["20261008000067"]
VERSIONS_60_69 = VERSIONS_60_67 + ["20261008000068", "20261008000069"]
VERSIONS_60_70 = VERSIONS_60_69 + ["20261008000070"]
SECRET_CHATS_INDEX_NAMES_60_67 = {
    "secret_chats_pkey",
    "secret_chats_admin_state_idx",
    "secret_chats_participant_state_idx",
    "secret_chats_admin_random_id_idx",
    "secret_chats_admin_date_idx",
    "secret_chats_participant_date_idx",
}
ROOT_ACCESS = "a" * 20
ROOT_SECRET = "c" * 64
APP_ACCESS = "b" * 20
APP_SECRET = "d" * 64
FILE_KEY = "02/258"
PART_KEY = "parts/aa/" + "b" * 32
TIMES = {
    "baseline": "2026-10-07T17:59:00Z",
    "provisional_references": "2026-10-07T17:59:30Z",
    "freeze_start": "2026-10-07T18:00:00Z",
    "dump": "2026-10-07T18:01:00Z",
    "references": "2026-10-07T18:02:00Z",
    "schema": "2026-10-07T18:02:30Z",
    "census": "2026-10-07T18:03:00Z",
    "frozen": "2026-10-07T18:04:00Z",
    "held": "2026-10-07T18:05:00Z",
}


def gate_namespace() -> dict[str, Any]:
    namespace: dict[str, Any] = {"__name__": "qualify_module", "__file__": str(GATE_PY)}
    exec(compile(GATE_PY.read_text(encoding="utf-8"), str(GATE_PY), "exec"), namespace)
    return namespace


def gate_constants(release_set: str = "60-66") -> dict[str, Any]:
    namespace = gate_namespace()
    release = namespace["RELEASES"][release_set]
    return {
        "schema": namespace["SCHEMA"],
        "reference_query_sha256": namespace["REFERENCE_QUERY_SHA256"],
        "active_links_query_sha256": namespace["ACTIVE_LINKS_QUERY_SHA256"],
        "atlas_sum_sha256": release["atlas_sum_sha256"],
        "migration_sha256": release["file_sha256"],
        "atlas_pins": release["atlas_pins"],
        "migration_files": release["files"],
        "revisions": release["revisions"],
        "legacy_live_schema_query_sha256": namespace["LEGACY_LIVE_SCHEMA_QUERY_SHA256"],
        "release_set": release_set,
        "minimum_migration_version": namespace["MIGRATIONS_60_62"][0],
        "live_schema_query_sha256": namespace["live_schema_query_sha256"](release_set),
        "migration_68": namespace["MIGRATION_68"],
        "migration_69": namespace["MIGRATION_69"],
        "migration_68_file": namespace["MIGRATION_68_FILE"],
        "migration_69_file": namespace["MIGRATION_69_FILE"],
        "files_index_names": namespace["FILES_INDEX_NAMES_60_69"],
        "r69_columns": namespace["R69_COLUMNS"],
        "r69_constraints": namespace["R69_CONSTRAINTS"],
        "r69_index_names": namespace["R69_INDEX_NAMES"],
        "inert_surfaces": namespace["INERT_SURFACES"],
        "inert_surfaces_query_sha256": namespace["INERT_SURFACES_QUERY_SHA256"],
        "migration_70": namespace["MIGRATION_70"],
        "migration_70_file": namespace["MIGRATION_70_FILE"],
        "r70_columns": namespace["R70_COLUMNS"],
        "r70_constraints": namespace["R70_CONSTRAINTS"],
        "r70_index_names": namespace["R70_INDEX_NAMES"],
        "r70_inert_surfaces": namespace["R70_INERT_SURFACES"],
        "r70_inert_surfaces_query_sha256": namespace["R70_INERT_SURFACES_QUERY_SHA256"],
    }


class FixtureProvenanceError(RuntimeError):
    pass


def verify_fixture_provenance(fixture_root: Path, release_set: str = "60-66") -> None:
    constants = gate_constants(release_set)
    migration_hashes = constants["migration_sha256"]
    expected_names = {"atlas.sum", *migration_hashes}

    try:
        root_info = fixture_root.lstat()
    except OSError as exc:
        raise FixtureProvenanceError("fixture_provenance_error: release fixture directory is missing or unreadable") from exc
    if not stat.S_ISDIR(root_info.st_mode):
        raise FixtureProvenanceError("fixture_provenance_error: release fixture path is not a directory")

    try:
        actual_names = {path.name for path in fixture_root.iterdir()}
    except OSError as exc:
        raise FixtureProvenanceError("fixture_provenance_error: release fixture directory is unreadable") from exc
    missing = sorted(expected_names - actual_names)
    unexpected = sorted(actual_names - expected_names)
    if missing:
        raise FixtureProvenanceError(f"fixture_provenance_error: missing release input {missing[0]}")
    if unexpected:
        raise FixtureProvenanceError(f"fixture_provenance_error: unexpected release input {unexpected[0]}")

    expected_hashes = {"atlas.sum": constants["atlas_sum_sha256"], **migration_hashes}
    for name, expected_sha256 in expected_hashes.items():
        path = fixture_root / name
        try:
            info = path.lstat()
        except OSError as exc:
            raise FixtureProvenanceError(f"fixture_provenance_error: missing or unreadable release input {name}") from exc
        if not stat.S_ISREG(info.st_mode):
            raise FixtureProvenanceError(f"fixture_provenance_error: release input {name} is not a regular file")
        try:
            data = path.read_bytes()
        except OSError as exc:
            raise FixtureProvenanceError(f"fixture_provenance_error: missing or unreadable release input {name}") from exc
        if hashlib.sha256(data).hexdigest() != expected_sha256:
            raise FixtureProvenanceError(f"fixture_provenance_error: SHA-256 mismatch for release input {name}")


def live_migrations_release() -> str | None:
    migrations_dir = PROJECT_ROOT / "migrations"
    for release_set in ("60-66", "60-67", "60-69", "60-70"):
        constants = gate_constants(release_set)
        expected_hashes = constants["migration_sha256"]
        try:
            live_names = sorted(
                path.name
                for path in migrations_dir.iterdir()
                if path.is_file()
                and re.match(r"^[0-9]{14}_.*\.sql$", path.name)
                and path.name[:14] >= constants["minimum_migration_version"]
            )
            if live_names != sorted(expected_hashes):
                continue
            if hashlib.sha256((migrations_dir / "atlas.sum").read_bytes()).hexdigest() != constants["atlas_sum_sha256"]:
                continue
            if all(
                hashlib.sha256((migrations_dir / name).read_bytes()).hexdigest() == expected_sha256
                for name, expected_sha256 in expected_hashes.items()
            ):
                return release_set
        except OSError:
            continue
    return None


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


def base_server_environment(service_name: str) -> dict[str, str]:
    trust = "proxy-v2" if service_name.startswith("telegramd-proxy") else "socket"
    return {
        "TG_BLOB_DIR": "/var/lib/telegramd-blobs",
        "TG_CLIENT_ADDR_TRUST": trust,
        "TG_REPLICA_COUNT": "1",
        "TG_RSA_KEY_FINGERPRINT": "fixture-fingerprint",
        "TG_AUTHKEY_ENC_KEY_FILE": "/run/secrets/authkey",
    }


def base_server_service(service_name: str) -> dict[str, Any]:
    return {
        "image": "telegramd:local",
        "environment": base_server_environment(service_name),
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


def running_container(
    identifier: str,
    service: str,
    mounts: list[dict[str, Any]],
    environment: dict[str, Any],
    ports: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    return {
        "id": identifier,
        "name": f"/{service}-1",
        "service": service,
        "running": True,
        "mounts": mounts,
        "environment": environment,
        "ports": ports or [],
    }


def baseline_inventory() -> dict[str, Any]:
    blobs = {"type": "volume", "source": "telegram-server_tgblobs", "target": "/var/lib/telegramd-blobs", "rw": True}
    keys = {"type": "volume", "source": "telegram-server_tgkey", "target": "/var/lib/telegramd", "rw": True}
    telegramd_environment = {
        **base_server_environment("telegramd"),
        "TG_SYNTHETIC_FIXTURE": "unchanged",
    }
    proxy_environment = base_server_environment("telegramd-proxy")
    telegramd_ports = [{"target": 2443, "published": "2443", "host_ip": "127.0.0.1", "protocol": "tcp"}]
    return {
        "complete": True,
        "captured_at": TIMES["baseline"],
        "containers": [
            running_container("container-main", "telegramd", [keys, blobs], telegramd_environment, telegramd_ports),
            running_container("container-proxy", "telegramd-proxy", [keys, blobs], proxy_environment, telegramd_ports),
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
                "a" * 64,
                "postgres",
                [{"type": "volume", "source": "telegram-server_pgdata", "target": "/var/lib/postgresql/data", "rw": True}],
                {},
            )
        ],
    }


def good_migration_evidence(release_set: str = "60-66") -> dict[str, Any]:
    if release_set in {"60-69", "60-70"}:
        constants = gate_constants(release_set)
        expected_index_properties = {
            "access_method": "btree",
            "indisunique": True,
            "indisprimary": False,
            "indimmediate": True,
            "indisvalid": True,
            "indisready": True,
            "indislive": True,
            "indpred": None,
            "indexprs": None,
            "indnatts": 2,
            "indnkeyatts": 2,
            "indoption": [0, 0],
        }
        gallery_tables: dict[str, Any] = {}
        for table_name, expected_columns in constants["r69_columns"].items():
            constraints = {}
            for name, expected in constants["r69_constraints"][table_name].items():
                constraints[name] = {
                    "type": expected["type"],
                    "validated": True,
                    "columns": expected.get("columns", []),
                    "referenced_table": expected.get("referenced_table"),
                    "referenced_columns": expected.get("referenced_columns"),
                    "on_delete": expected.get("on_delete"),
                    "on_update": expected.get("on_update"),
                    "match": expected.get("match"),
                    "set_null_columns": expected.get("set_null_columns", []),
                    "referenced_index": expected.get("referenced_index"),
                    "check_expression": "CHECK (true)" if expected["type"] == "c" else None,
                }
            index_names = constants["r69_index_names"][table_name]
            gallery_tables[table_name] = {
                "table": f"public.{table_name}",
                "columns": {
                    name: {"type": sql_type, "not_null": not_null, "default": default}
                    for name, (sql_type, not_null, default) in expected_columns.items()
                },
                "constraints": constraints,
                "index_names": list(index_names),
                "index_validity": {name: True for name in index_names},
            }
        migration_70_schema = None
        if release_set == "60-70":
            r70_tables: dict[str, Any] = {}
            for table_name, expected_columns in constants["r70_columns"].items():
                constraints = {}
                for name, expected in constants["r70_constraints"][table_name].items():
                    constraints[name] = {
                        "type": expected["type"],
                        "validated": True,
                        "deferrable": False,
                        "initially_deferred": False,
                        "columns": expected.get("columns", []),
                        "referenced_table": expected.get("referenced_table"),
                        "referenced_columns": expected.get("referenced_columns"),
                        "on_delete": expected.get("on_delete"),
                        "on_update": expected.get("on_update"),
                        "match": expected.get("match"),
                        "set_null_columns": expected.get("set_null_columns", []),
                        "referenced_index": expected.get("referenced_index"),
                        "check_expression": "CHECK (true)" if expected["type"] == "c" else None,
                    }
                index_names = constants["r70_index_names"][table_name]
                r70_tables[table_name] = {
                    "table": f"public.{table_name}",
                    "columns": {
                        name: {
                            "type": sql_type,
                            "not_null": True,
                            "default": None,
                            "identity": "",
                            "generated": "",
                            "sequence": False,
                        }
                        for name, sql_type in expected_columns.items()
                    },
                    "constraints": constraints,
                    "index_names": list(index_names),
                    "index_validity": {name: True for name in index_names},
                }
            migration_70_schema = {
                "tables": r70_tables,
                "inbound_foreign_keys": [],
                "user_triggers": {name: [] for name in constants["r70_columns"]},
            }

        revision_detail = {}
        for filename, version in zip(constants["migration_files"], constants["revisions"], strict=True):
            detail = {
                "applied": 1,
                "total": 1,
                "error": "",
                "hash": constants["atlas_pins"][filename],
            }
            if release_set == "60-70":
                detail.update({"error_stmt_empty": True, "partial_hashes_empty": True})
            revision_detail[version] = detail

        evidence = {
            "release_set": release_set,
            "baseline_revisions": list(constants["revisions"]),
            "revision_rows": {version: True for version in constants["revisions"]},
            "target_revisions": list(constants["revisions"]),
            "approved_revision_set_exact": True,
            "migration_66_present": True,
            "migration_67_present": True,
            "migration_68_present": True,
            "migration_69_present": True,
            "migration_66_schema": new_unread_mark_schema(),
            "migration_67_schema": {
                "table": "public.secret_chats",
                "index_validity": {name: True for name in SECRET_CHATS_INDEX_NAMES_60_67},
                "party_date_indexes": {
                    name: {"columns": columns, **{
                        "access_method": "btree",
                        "indisvalid": True,
                        "indisready": True,
                        "indislive": True,
                        "indisunique": False,
                        "indisprimary": False,
                        "indpred": None,
                        "indexprs": None,
                        "indnatts": 2,
                        "indnkeyatts": 2,
                        "indoption": [0, 0],
                    }}
                    for name, columns in {
                        "secret_chats_admin_date_idx": ["admin_id", "date"],
                        "secret_chats_participant_date_idx": ["participant_id", "date"],
                    }.items()
                },
            },
            "migration_68_schema": {
                "table": "public.files",
                "index_names": sorted(constants["files_index_names"]),
                "index_validity": {name: True for name in constants["files_index_names"]},
                "ownership_index": {
                    "columns": ["id", "uploader_id"],
                    **expected_index_properties,
                },
            },
            "migration_69_schema": {"tables": gallery_tables},
            "inert_surfaces": {
                name: False
                for name in (
                    constants["r70_inert_surfaces"]
                    if release_set == "60-70"
                    else constants["inert_surfaces"]
                )
            },
            "revision_detail": revision_detail,
        }
        if release_set == "60-70":
            evidence["migration_70_present"] = True
            evidence["migration_70_schema"] = migration_70_schema
        return evidence
    if release_set == "60-67":
        constants = gate_constants(release_set)
        expected_properties = {
            "access_method": "btree",
            "indisvalid": True,
            "indisready": True,
            "indislive": True,
            "indisunique": False,
            "indisprimary": False,
            "indpred": None,
            "indexprs": None,
            "indnatts": 2,
            "indnkeyatts": 2,
            "indoption": [0, 0],
        }
        return {
            "release_set": release_set,
            "baseline_revisions": VERSIONS_60_67,
            "revision_rows": {version: True for version in VERSIONS_60_67},
            "target_revisions": VERSIONS_60_67,
            "approved_revision_set_exact": True,
            "migration_66_present": True,
            "migration_67_present": True,
            "migration_66_schema": new_unread_mark_schema(),
            "migration_67_schema": {
                "table": "public.secret_chats",
                "index_validity": {name: True for name in SECRET_CHATS_INDEX_NAMES_60_67},
                "party_date_indexes": {
                    name: {"columns": columns, **expected_properties}
                    for name, columns in {
                        "secret_chats_admin_date_idx": ["admin_id", "date"],
                        "secret_chats_participant_date_idx": ["participant_id", "date"],
                    }.items()
                },
            },
            "revision_detail": {
                version: {
                    "applied": 1,
                    "total": 1,
                    "error": "",
                    "hash": constants["atlas_pins"][filename],
                }
                for filename, version in zip(
                    constants["migration_files"],
                    VERSIONS_60_67,
                    strict=True,
                )
            },
        }
    constants = gate_constants(release_set)
    return {
        "baseline_revisions": VERSIONS_60_62,
        "revision_rows": {version: True for version in VERSIONS_60_66},
        "target_revisions": VERSIONS_60_66,
        "approved_revision_set_exact": True,
        "migration_66_present": True,
        "migration_66_schema": new_unread_mark_schema(),
        "revision_detail": {
            version: {
                "applied": 1,
                "total": 1,
                "error": "",
                "hash": constants["atlas_pins"][filename],
            }
            for filename, version in zip(
                constants["migration_files"],
                VERSIONS_60_66,
                strict=True,
            )
        },
    }


def write_bundle(
    root: Path,
    scenario: str = "success",
    fixture_root: Path | None = None,
    release_set: str = "60-66",
) -> tuple[Path, Path, Path, str]:
    if fixture_root is None:
        fixture_root = {
            "60-67": FROZEN_MIGRATIONS_67,
            "60-69": FROZEN_MIGRATIONS_69,
            "60-70": FROZEN_MIGRATIONS_70,
        }.get(release_set, FROZEN_MIGRATIONS)
    verify_fixture_provenance(fixture_root, release_set)
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
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
    if scenario in (
        "live-migrations-overlay",
        "changed-68-migration-file",
        "changed-69-migration-file",
    ):
        migration_sources = sorted(
            path
            for path in (PROJECT_ROOT / "migrations").glob("*.sql")
            if path.name[:14] < gate_constants(release_set)["minimum_migration_version"]
        )
        migration_sources.append(FROZEN_MIGRATIONS_69 / "atlas.sum")
        migration_sources.extend(sorted(FROZEN_MIGRATIONS_69.glob("*.sql")))
    else:
        migration_sources = [fixture_root / "atlas.sum"]
        migration_sources.extend(
            path
            for path in sorted((PROJECT_ROOT / "migrations").glob("*.sql"))
            if path.name[:14] < gate_constants(release_set)["minimum_migration_version"]
        )
        migration_sources.extend(fixture_root / name for name in gate_constants(release_set)["migration_sha256"])
    for source in migration_sources:
        destination = checkout / "migrations" / source.name
        shutil.copyfile(source, destination)
        destination.chmod(0o600)
    if scenario == "real-67-file":
        source = PROJECT_ROOT / "migrations" / LIVE_MIGRATION_67
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
    elif scenario == "baseline-and-candidate-extra-mount":
        extra_mount = {"type": "bind", "source": "/etc", "target": "/etc-unapproved", "read_only": True}
        baseline_compose["services"]["telegramd"]["volumes"].append(extra_mount)
        candidate_compose["services"]["telegramd"]["volumes"].append(extra_mount)
    elif scenario == "baseline-and-candidate-extra-environment":
        baseline_compose["services"]["telegramd"]["environment"]["TG_UNAPPROVED"] = "unexpected"
        candidate_compose["services"]["telegramd"]["environment"]["TG_UNAPPROVED"] = "unexpected"
    elif scenario == "baseline-and-candidate-extra-port":
        extra_port = {"target": 8080, "published": "8080", "host_ip": "127.0.0.1", "protocol": "tcp"}
        baseline_compose["services"]["telegramd"]["ports"].append(extra_port)
        candidate_compose["services"]["telegramd"]["ports"].append(extra_port)

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
    baseline_inventory_doc = baseline_inventory()
    baseline_inventory_raw = json.dumps(baseline_inventory_doc, sort_keys=True, separators=(",", ":")).encode("utf-8")
    dump_json(bundle / "baseline-containers.json", baseline_inventory_doc)
    baseline_env_sha = hashlib.sha256(base_env).hexdigest()
    baseline_compose_sha = hashlib.sha256(
        json.dumps(baseline_compose, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    ).hexdigest()
    recovery_point = {
        "schema": "teagram.main-1387-recovery-point/v1",
        "source_issue": "MAIN-1387",
        "recovery_point_id": "MAIN-1387-fixture-recovery-point-1",
        "baseline_env_sha256": baseline_env_sha,
        "inspected_baseline_sha256": hashlib.sha256(baseline_inventory_raw).hexdigest(),
        "baseline_compose_sha256": baseline_compose_sha,
    }
    recovery_point_raw = json.dumps(recovery_point, sort_keys=True, separators=(",", ":")).encode("utf-8")
    dump_json(bundle / "baseline-recovery-point.json", recovery_point)

    source_rows = [(FILE_KEY, 5, "a" * 64), (PART_KEY, 3, "b" * 64)]
    refs = f"file\ttrue\t{FILE_KEY}\nupload_part\ttrue\t{PART_KEY}\n".encode("ascii")
    links = f"channel_messages\t258\ttrue\nmessages\t258\tfalse\n".encode("ascii")
    if release_set in {"60-69", "60-70"}:
        source_rows = [(PART_KEY, 3, "b" * 64)]
        refs = f"upload_part\ttrue\t{PART_KEY}\n".encode("ascii")
        links = b""
    dump_content = b"synthetic private postgres dump\n-- PostgreSQL database dump complete\n"
    dump_bytes(bundle / "postgres.dump", dump_content)
    for name in (
        "source-provisional.tsv", "source-frozen.tsv", "copy-pass-1.tsv", "copy-pass-2.tsv",
        "destination-census-pass-1.tsv", "destination-census-pass-2.tsv",
    ):
        dump_bytes(bundle / name, manifest(source_rows))
    if scenario == "unstored-file-absent":
        refs = (
            f"file\ttrue\t{FILE_KEY}\n"
            "file\tfalse\t03/259\n"
            f"upload_part\ttrue\t{PART_KEY}\n"
        ).encode("ascii")
    dump_bytes(bundle / "references-provisional.tsv", refs)
    dump_bytes(bundle / "active-links-provisional.tsv", links)
    dump_bytes(bundle / "references.tsv", refs)
    dump_bytes(bundle / "active-links.tsv", links)

    metadata = {
        "schema": gate_constants()["schema"],
        "source_volume": "telegram-server_tgblobs",
        "baseline_env_provenance": {
            "source_issue": "MAIN-1387",
            "recovery_point_id": "MAIN-1387-fixture-recovery-point-1",
            "recovery_point_sha256": hashlib.sha256(recovery_point_raw).hexdigest(),
            "recovery_point_env_sha256": baseline_env_sha,
            "baseline_env_sha256": baseline_env_sha,
            "inspected_baseline_sha256": hashlib.sha256(baseline_inventory_raw).hexdigest(),
            "inspected_env_sha256": baseline_env_sha,
            "baseline_compose_sha256": baseline_compose_sha,
            "inspected_at": TIMES["baseline"],
        },
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
            "provisional_references_captured_at": TIMES["provisional_references"],
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
    if release_set in {"60-69", "60-70"}:
        metadata["references"]["inert_surfaces_query_sha256"] = gate_constants(release_set)[
            "r70_inert_surfaces_query_sha256"
            if release_set == "60-70"
            else "inert_surfaces_query_sha256"
        ]
    if release_set == "60-70":
        metadata["freeze"]["schema_captured_at"] = "2026-10-07T18:00:45Z"
        metadata["freeze"]["inert_surfaces_captured_at"] = "2026-10-07T18:02:45Z"
    dump_json(bundle / "qualification.json", metadata)
    frozen = frozen_inventory()
    migrations = good_migration_evidence(release_set)
    if scenario == "r67-db-60-69":
        migrations = good_migration_evidence("60-69")
    if release_set in {"60-67", "60-69", "60-70"}:
        metadata["freeze"]["baseline_schema_captured_at"] = "2026-10-07T18:00:30Z"
        dump_json(bundle / "qualification.json", metadata)

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
    elif scenario == "missing-baseline-service":
        baseline_inventory_doc = baseline_inventory()
        baseline_inventory_doc["containers"] = [
            container for container in baseline_inventory_doc["containers"] if container["service"] != "postgres"
        ]
        dump_json(bundle / "baseline-containers.json", baseline_inventory_doc)
    elif scenario == "mismatched-blob-directory":
        baseline_inventory_doc = baseline_inventory()
        baseline_inventory_doc["containers"][1]["environment"]["TG_BLOB_DIR"] = "/var/lib/telegramd"
        dump_json(bundle / "baseline-containers.json", baseline_inventory_doc)
    elif scenario == "changed-census":
        dump_bytes(bundle / "source-frozen.tsv", manifest([(FILE_KEY, 5, "f" * 64), (PART_KEY, 3, "b" * 64)]))
    elif scenario == "different-copy-pass-2":
        dump_bytes(bundle / "copy-pass-2.tsv", manifest([(FILE_KEY, 5, "f" * 64), (PART_KEY, 3, "b" * 64)]))
    elif scenario == "different-destination-census-pass-2":
        dump_bytes(bundle / "destination-census-pass-2.tsv", manifest([(FILE_KEY, 5, "f" * 64), (PART_KEY, 3, "b" * 64)]))
    elif scenario == "changed-references-after-freeze":
        dump_bytes(bundle / "references.tsv", refs + f"file\ttrue\t04/260\n".encode("ascii"))
    elif scenario == "changed-active-links-after-freeze":
        dump_bytes(bundle / "active-links.tsv", links + b"messages\t259\tfalse\n")
    elif scenario == "missing-reference":
        empty_source = [(PART_KEY, 3, "b" * 64)]
        for name in (
            "source-provisional.tsv", "source-frozen.tsv", "copy-pass-1.tsv", "copy-pass-2.tsv",
            "destination-census-pass-1.tsv", "destination-census-pass-2.tsv",
        ):
            dump_bytes(bundle / name, manifest(empty_source))
    elif scenario == "leftover-temp":
        temporary = [("02/258.tmp", 1, "c" * 64), (PART_KEY, 3, "b" * 64)]
        for name in (
            "source-provisional.tsv", "source-frozen.tsv", "copy-pass-1.tsv", "copy-pass-2.tsv",
            "destination-census-pass-1.tsv", "destination-census-pass-2.tsv",
        ):
            dump_bytes(bundle / name, manifest(temporary))
    elif scenario == "unrelated-env-drift":
        candidate_env = candidate_env.replace(b"POSTGRES_PASSWORD=stable-value", b"POSTGRES_PASSWORD=changed-value")
        dump_bytes(bundle / "candidate.env", candidate_env)
        dump_bytes(checkout / ".env", candidate_env)
    elif scenario == "missing-baseline-provenance":
        metadata.pop("baseline_env_provenance")
        dump_json(bundle / "qualification.json", metadata)
    elif scenario == "wrong-baseline-provenance-source":
        metadata["baseline_env_provenance"]["source_issue"] = "MAIN-1399"
        dump_json(bundle / "qualification.json", metadata)
    elif scenario == "mismatched-recovery-point-env":
        metadata["baseline_env_provenance"]["recovery_point_env_sha256"] = "0" * 64
        dump_json(bundle / "qualification.json", metadata)
    elif scenario == "mismatched-inspected-env":
        metadata["baseline_env_provenance"]["inspected_env_sha256"] = "0" * 64
        dump_json(bundle / "qualification.json", metadata)
    elif scenario == "mismatched-inspected-baseline":
        metadata["baseline_env_provenance"]["inspected_baseline_sha256"] = "0" * 64
        dump_json(bundle / "qualification.json", metadata)
    elif scenario == "mismatched-baseline-compose":
        metadata["baseline_env_provenance"]["baseline_compose_sha256"] = "0" * 64
        dump_json(bundle / "qualification.json", metadata)
    elif scenario == "mismatched-recovery-point-document":
        recovery_point["source_issue"] = "MAIN-1399"
        dump_json(bundle / "baseline-recovery-point.json", recovery_point)
    elif scenario == "secret-change":
        dump_bytes(bundle / "candidate-secrets" / "telegramd-blob-secret-key", b"e" * 64, mode=0o444)
        dump_bytes(checkout / ".secrets" / "telegramd-blob-secret-key", b"e" * 64, mode=0o444)
    elif scenario == "wrong-source-identity":
        metadata["source_volume"] = ""
        dump_json(bundle / "qualification.json", metadata)
    elif scenario == "dump-outside-freeze":
        metadata["freeze"]["dump_captured_at"] = "2026-10-07T17:59:59Z"
        dump_json(bundle / "qualification.json", metadata)
    elif scenario in {"r67-baseline-capture-after-dump", "r69-baseline-capture-after-dump", "r70-baseline-capture-after-dump"}:
        metadata["freeze"]["baseline_schema_captured_at"] = "2026-10-07T18:01:30Z"
        dump_json(bundle / "qualification.json", metadata)
    elif scenario in {"r67-baseline-capture-missing", "r69-baseline-capture-missing", "r70-baseline-capture-missing"}:
        metadata["freeze"].pop("baseline_schema_captured_at")
        dump_json(bundle / "qualification.json", metadata)
    elif scenario in {"r67-applied-capture-outside-freeze", "r69-applied-capture-outside-freeze", "r70-applied-capture-outside-freeze"}:
        metadata["freeze"]["schema_captured_at"] = "2026-10-07T18:06:00Z"
        dump_json(bundle / "qualification.json", metadata)
    elif scenario == "r70-applied-capture-after-dump":
        metadata["freeze"]["schema_captured_at"] = "2026-10-07T18:01:30Z"
        dump_json(bundle / "qualification.json", metadata)
    elif scenario in {"r69-applied-capture-before-baseline", "r70-applied-capture-before-baseline"}:
        metadata["freeze"]["schema_captured_at"] = "2026-10-07T18:00:15Z"
        dump_json(bundle / "qualification.json", metadata)
    elif scenario in ("missing-66", "wrong-version-66", "extra-67"):
        if release_set == "60-67":
            if scenario == "missing-66":
                migrations["revision_rows"]["20261007000066"] = False
            elif scenario == "wrong-version-66":
                migrations["revision_rows"]["20261007000066"] = False
                migrations["revision_rows"]["20261008000068"] = True
            else:
                migrations["revision_rows"]["20261008000068"] = True
        else:
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
    elif scenario == "r67-db-60-66":
        migrations["baseline_revisions"] = VERSIONS_60_66
        migrations["revision_rows"] = {version: True for version in VERSIONS_60_66}
        migrations["target_revisions"] = VERSIONS_60_66
        migrations["approved_revision_set_exact"] = False
        migrations["migration_67_present"] = False
        migrations["revision_detail"].pop("20261008000067")
        migrations["migration_67_schema"]["index_validity"] = {
            name: True for name in SECRET_CHATS_INDEX_NAMES_60_67 if not name.endswith("_date_idx")
        }
        migrations["migration_67_schema"]["party_date_indexes"] = {}
    elif scenario == "r67-baseline-60-65":
        migrations["baseline_revisions"] = VERSIONS_60_65
    elif scenario == "r67-baseline-60-62":
        migrations["baseline_revisions"] = VERSIONS_60_62
    elif scenario == "r66-db-60-67":
        migrations["revision_rows"]["20261008000067"] = True
        migrations["target_revisions"] = VERSIONS_60_66 + ["20261008000067"]
        migrations["approved_revision_set_exact"] = False
        migrations["migration_67_present"] = True
    elif scenario == "release-set-missing":
        migrations.pop("release_set")
    elif scenario == "release-set-66":
        migrations["release_set"] = "60-66"
    elif scenario == "release-set-67":
        migrations["release_set"] = "60-67"
    elif scenario == "r67-extra-68-row":
        migrations["revision_rows"]["20261008000068"] = False
    elif scenario == "r67-extra-revision-detail":
        migrations["revision_detail"]["20261008000068"] = {
            "applied": 1,
            "total": 1,
            "error": "",
            "hash": "h1:unapproved",
        }
    elif scenario == "r67-extra-migrations-key":
        migrations["unapproved"] = True
    elif scenario == "r67-extra-schema-key":
        migrations["migration_67_schema"]["unapproved"] = True
    elif scenario.startswith("r67-index-"):
        parts = scenario.split("-")
        mutation, index_party = "-".join(parts[2:-1]), parts[-1]
        name = f"secret_chats_{index_party}_date_idx"
        schema = migrations["migration_67_schema"]
        if mutation == "missing":
            schema["index_validity"].pop(name)
            schema["party_date_indexes"].pop(name)
        elif mutation == "renamed":
            schema["index_validity"][f"{name}_renamed"] = schema["index_validity"].pop(name)
            schema["party_date_indexes"][f"{name}_renamed"] = schema["party_date_indexes"].pop(name)
        elif mutation == "invalid":
            schema["index_validity"][name] = False
            schema["party_date_indexes"][name]["indisvalid"] = False
        elif mutation == "validity-type":
            schema["party_date_indexes"][name]["indisvalid"] = 1
        elif mutation == "not-ready":
            schema["party_date_indexes"][name]["indisready"] = False
        elif mutation == "not-live":
            schema["party_date_indexes"][name]["indislive"] = False
        elif mutation == "unique":
            schema["party_date_indexes"][name]["indisunique"] = True
        elif mutation == "primary":
            schema["party_date_indexes"][name]["indisprimary"] = True
        elif mutation == "partial":
            schema["party_date_indexes"][name]["indpred"] = "(admin_id IS NOT NULL)"
        elif mutation == "expression":
            schema["party_date_indexes"][name]["indexprs"] = "(admin_id)"
        elif mutation == "wrong-method":
            schema["party_date_indexes"][name]["access_method"] = "hash"
        elif mutation == "wrong-count":
            schema["party_date_indexes"][name]["indnatts"] = 3
        elif mutation == "wrong-key-count":
            schema["party_date_indexes"][name]["indnkeyatts"] = 1
        elif mutation == "wrong-option":
            schema["party_date_indexes"][name]["indoption"] = [False, 0]
        elif mutation == "swapped":
            schema["party_date_indexes"][name]["columns"].reverse()
    elif scenario == "r67-extra-invalid-index":
        migrations["migration_67_schema"]["index_validity"]["secret_chats_unexpected_invalid_idx"] = False
    elif scenario == "r67-revision-incomplete":
        migrations["revision_detail"]["20261008000067"]["applied"] = 0
    elif scenario == "r67-revision-error":
        migrations["revision_detail"]["20261008000067"]["error"] = "migration failed"
    elif scenario == "r67-revision-hash":
        migrations["revision_detail"]["20261008000067"]["hash"] = "h1:tampered"
    elif scenario == "r69-baseline-60-68":
        migrations["baseline_revisions"] = VERSIONS_60_67
    elif scenario in {"r69-db-60-67", "r69-db-60-68"}:
        incomplete_versions = VERSIONS_60_67 if scenario.endswith("60-67") else VERSIONS_60_67 + ["20261008000068"]
        migrations["baseline_revisions"] = incomplete_versions
        migrations["revision_rows"] = {version: True for version in incomplete_versions}
        migrations["target_revisions"] = incomplete_versions
        migrations["approved_revision_set_exact"] = False
        for version in ("20261008000068", "20261008000069"):
            migrations[f"migration_{version[-2:]}_present"] = version in incomplete_versions
            if version not in incomplete_versions:
                migrations["revision_detail"].pop(version)
    elif scenario == "r69-extra-70-row":
        migrations["revision_rows"]["20261008000070"] = True
        migrations["target_revisions"].append("20261008000070")
        migrations["approved_revision_set_exact"] = False
    elif scenario == "r69-68-present-false":
        migrations["migration_68_present"] = False
    elif scenario == "r69-69-present-false":
        migrations["migration_69_present"] = False
    elif scenario == "r69-revision-68-incomplete":
        migrations["revision_detail"]["20261008000068"]["applied"] = 0
    elif scenario == "r69-revision-68-error":
        migrations["revision_detail"]["20261008000068"]["error"] = "migration failed"
    elif scenario == "r69-revision-68-hash":
        migrations["revision_detail"]["20261008000068"]["hash"] = "h1:tampered"
    elif scenario == "r69-revision-69-incomplete":
        migrations["revision_detail"]["20261008000069"]["total"] = 2
    elif scenario == "r69-revision-69-error":
        migrations["revision_detail"]["20261008000069"]["error"] = "migration failed"
    elif scenario == "r69-revision-69-hash":
        migrations["revision_detail"]["20261008000069"]["hash"] = "h1:tampered"
    elif scenario == "r69-extra-migrations-key":
        migrations["unapproved"] = True
    elif scenario == "r69-extra-schema-key":
        migrations["migration_69_schema"]["unapproved"] = True
    elif scenario == "r69-extra-schema-table":
        migrations["migration_69_schema"]["tables"]["unapproved"] = {}
    elif scenario == "r69-68-index-missing":
        migrations["migration_68_schema"]["ownership_index"] = None
    elif scenario == "r69-68-index-renamed":
        schema = migrations["migration_68_schema"]
        schema["index_names"][0] = "files_id_uploader_id_renamed"
        schema["index_validity"]["files_id_uploader_id_renamed"] = schema["index_validity"].pop(
            "files_id_uploader_id_key"
        )
    elif scenario == "r69-68-index-extra":
        schema = migrations["migration_68_schema"]
        schema["index_names"].append("files_unapproved_idx")
        schema["index_validity"]["files_unapproved_idx"] = True
    elif scenario == "r69-68-index-invalid":
        migrations["migration_68_schema"]["ownership_index"]["indisvalid"] = False
    elif scenario == "r69-68-index-not-ready":
        migrations["migration_68_schema"]["ownership_index"]["indisready"] = False
    elif scenario == "r69-68-index-not-live":
        migrations["migration_68_schema"]["ownership_index"]["indislive"] = False
    elif scenario == "r69-68-index-not-immediate":
        migrations["migration_68_schema"]["ownership_index"]["indimmediate"] = False
    elif scenario == "r69-68-index-not-unique":
        migrations["migration_68_schema"]["ownership_index"]["indisunique"] = False
    elif scenario == "r69-68-index-primary":
        migrations["migration_68_schema"]["ownership_index"]["indisprimary"] = True
    elif scenario == "r69-68-index-method":
        migrations["migration_68_schema"]["ownership_index"]["access_method"] = "hash"
    elif scenario == "r69-68-index-partial":
        migrations["migration_68_schema"]["ownership_index"]["indpred"] = "(uploader_id IS NOT NULL)"
    elif scenario == "r69-68-index-expression":
        migrations["migration_68_schema"]["ownership_index"]["indexprs"] = "(uploader_id)"
    elif scenario == "r69-68-index-attributes":
        migrations["migration_68_schema"]["ownership_index"]["indnatts"] = 3
    elif scenario == "r69-68-index-key-count":
        migrations["migration_68_schema"]["ownership_index"]["indnkeyatts"] = 1
    elif scenario == "r69-68-index-option":
        migrations["migration_68_schema"]["ownership_index"]["indoption"] = [1, 0]
    elif scenario == "r69-68-index-columns":
        migrations["migration_68_schema"]["ownership_index"]["columns"].reverse()
    elif scenario == "r69-ownership-local-order":
        migrations["migration_69_schema"]["tables"]["user_photos"]["constraints"][
            "user_photos_file_owned_by_owner"
        ]["columns"].reverse()
    elif scenario == "r69-ownership-referenced-order":
        migrations["migration_69_schema"]["tables"]["user_photos"]["constraints"][
            "user_photos_file_owned_by_owner"
        ]["referenced_columns"].reverse()
    elif scenario == "r69-ownership-action":
        migrations["migration_69_schema"]["tables"]["user_photos"]["constraints"][
            "user_photos_file_owned_by_owner"
        ]["on_delete"] = "CASCADE"
    elif scenario == "r69-ownership-conindid":
        migrations["migration_69_schema"]["tables"]["user_photos"]["constraints"][
            "user_photos_file_owned_by_owner"
        ]["referenced_index"] = "files_pkey"
    elif scenario == "r69-pointer-local-order":
        migrations["migration_69_schema"]["tables"]["profile_photo_state"]["constraints"][
            "profile_photo_state_current_is_own_gallery_entry"
        ]["columns"].reverse()
    elif scenario == "r69-pointer-referenced-order":
        migrations["migration_69_schema"]["tables"]["profile_photo_state"]["constraints"][
            "profile_photo_state_current_is_own_gallery_entry"
        ]["referenced_columns"].reverse()
    elif scenario == "r69-pointer-match":
        migrations["migration_69_schema"]["tables"]["profile_photo_state"]["constraints"][
            "profile_photo_state_current_is_own_gallery_entry"
        ]["match"] = "FULL"
    elif scenario == "r69-receipt-set-null-columns":
        migrations["migration_69_schema"]["tables"]["profile_upload_receipt"]["constraints"][
            "profile_upload_receipt_file_owned_by_owner"
        ]["set_null_columns"] = ["file_id", "user_id"]
    elif scenario == "r69-missing-ownership-fk":
        migrations["migration_69_schema"]["tables"]["user_photos"]["constraints"].pop(
            "user_photos_file_owned_by_owner"
        )
    elif scenario == "r69-extra-auth-key-fk":
        migrations["migration_69_schema"]["tables"]["profile_delete_operation"]["constraints"][
            "profile_delete_operation_auth_key_id_fkey"
        ] = {"type": "f"}
    elif scenario == "r69-primary-key-order":
        migrations["migration_69_schema"]["tables"]["profile_delete_operation"]["constraints"][
            "profile_delete_operation_pkey"
        ]["columns"].reverse()
    elif scenario == "r69-missing-unique":
        migrations["migration_69_schema"]["tables"]["user_photos"]["constraints"].pop(
            "user_photos_user_id_client_file_id_key"
        )
    elif scenario == "r69-column-type":
        migrations["migration_69_schema"]["tables"]["profile_upload_receipt"]["columns"]["part_count"][
            "type"
        ] = "bigint"
    elif scenario == "r69-column-nullability":
        migrations["migration_69_schema"]["tables"]["profile_photo_state"]["columns"]["current_file_id"][
            "not_null"
        ] = True
    elif scenario == "r69-column-default":
        migrations["migration_69_schema"]["tables"]["profile_photo_state"]["columns"]["mutation_revision"][
            "default"
        ] = "1"
    elif scenario == "r69-index-extra":
        table = migrations["migration_69_schema"]["tables"]["user_photos"]
        table["index_names"].append("user_photos_unapproved_idx")
        table["index_validity"]["user_photos_unapproved_idx"] = True
    elif scenario == "r69-index-invalid":
        migrations["migration_69_schema"]["tables"]["user_photos"]["index_validity"][
            "user_photos_file_id_key"
        ] = False
    elif scenario.startswith("r69-inert-row-"):
        table_name = scenario.removeprefix("r69-inert-row-")
        migrations["inert_surfaces"][table_name] = True
    elif scenario == "r69-inert-malformed":
        migrations["inert_surfaces"]["user_photos"] = "false"
    elif scenario == "r69-inert-query-hash":
        metadata["references"]["inert_surfaces_query_sha256"] = "0" * 64
        dump_json(bundle / "qualification.json", metadata)
    elif scenario == "r69-file-row":
        r69_source_rows = [(FILE_KEY, 5, "a" * 64), (PART_KEY, 3, "b" * 64)]
        for name in (
            "source-provisional.tsv", "source-frozen.tsv", "copy-pass-1.tsv", "copy-pass-2.tsv",
            "destination-census-pass-1.tsv", "destination-census-pass-2.tsv",
        ):
            dump_bytes(bundle / name, manifest(r69_source_rows))
        r69_references = f"file\ttrue\t{FILE_KEY}\nupload_part\ttrue\t{PART_KEY}\n".encode("ascii")
        r69_active_links = f"messages\t258\ttrue\n".encode("ascii")
        dump_bytes(bundle / "references-provisional.tsv", r69_references)
        dump_bytes(bundle / "references.tsv", r69_references)
        dump_bytes(bundle / "active-links-provisional.tsv", r69_active_links)
        dump_bytes(bundle / "active-links.tsv", r69_active_links)
    elif scenario == "r67-db-60-69":
        r69_source_rows = [(PART_KEY, 3, "b" * 64)]
        for name in (
            "source-provisional.tsv", "source-frozen.tsv", "copy-pass-1.tsv", "copy-pass-2.tsv",
            "destination-census-pass-1.tsv", "destination-census-pass-2.tsv",
        ):
            dump_bytes(bundle / name, manifest(r69_source_rows))
        r69_references = f"upload_part\ttrue\t{PART_KEY}\n".encode("ascii")
        dump_bytes(bundle / "references-provisional.tsv", r69_references)
        dump_bytes(bundle / "references.tsv", r69_references)
        dump_bytes(bundle / "active-links-provisional.tsv", b"")
        dump_bytes(bundle / "active-links.tsv", b"")
        metadata["references"]["inert_surfaces_query_sha256"] = gate_constants("60-69")[
            "inert_surfaces_query_sha256"
        ]
        dump_json(bundle / "qualification.json", metadata)
    elif scenario == "r70-baseline-60-69":
        migrations["baseline_revisions"] = VERSIONS_60_69
    elif scenario == "r70-db-60-69":
        migrations["revision_rows"].pop("20261008000070")
        migrations["target_revisions"] = list(VERSIONS_60_69)
        migrations["approved_revision_set_exact"] = False
        migrations["migration_70_present"] = False
        migrations["revision_detail"].pop("20261008000070")
    elif scenario == "r70-extra-71-row":
        migrations["revision_rows"]["20261009000071"] = True
        migrations["target_revisions"].append("20261009000071")
        migrations["approved_revision_set_exact"] = False
    elif scenario == "r70-70-present-false":
        migrations["migration_70_present"] = False
    elif scenario.startswith("r70-revision-"):
        version = "20261008000070"
        detail = migrations["revision_detail"][version]
        mutation = scenario.removeprefix("r70-revision-")
        if mutation == "error":
            detail["error"] = "migration failed"
        elif mutation == "error-stmt":
            detail["error_stmt_empty"] = False
        elif mutation == "partial-hashes":
            detail["partial_hashes_empty"] = False
        elif mutation == "incomplete":
            detail["applied"] = 0
        elif mutation == "total":
            detail["total"] = 2
        elif mutation == "hash":
            detail["hash"] = "h1:tampered"
        elif mutation == "missing":
            migrations["revision_rows"].pop(version)
            migrations["revision_detail"].pop(version)
        else:
            raise AssertionError(f"unknown R70 revision mutation: {mutation}")
    elif scenario == "r70-extra-migrations-key":
        migrations["unapproved"] = True
    elif scenario == "r70-missing-migration-70-schema":
        migrations.pop("migration_70_schema")
    elif scenario == "r70-extra-migration-70-schema-key":
        migrations["migration_70_schema"]["unapproved"] = True
    elif scenario.startswith("r70-column-type-"):
        table_name, column_name = scenario.removeprefix("r70-column-type-").split("-", 1)
        migrations["migration_70_schema"]["tables"][table_name]["columns"][column_name]["type"] = "text"
    elif scenario.startswith("r70-column-nullability-"):
        table_name, column_name = scenario.removeprefix("r70-column-nullability-").split("-", 1)
        migrations["migration_70_schema"]["tables"][table_name]["columns"][column_name]["not_null"] = False
    elif scenario.startswith("r70-column-default-"):
        table_name, column_name = scenario.removeprefix("r70-column-default-").split("-", 1)
        migrations["migration_70_schema"]["tables"][table_name]["columns"][column_name]["default"] = "1"
    elif scenario == "r70-sequence-default":
        column = migrations["migration_70_schema"]["tables"]["erasure_outbox"]["columns"]["seq"]
        column["default"] = "nextval('erasure_outbox_seq_seq'::regclass)"
    elif scenario == "r70-identity-column":
        migrations["migration_70_schema"]["tables"]["erasure_outbox"]["columns"]["seq"]["identity"] = "d"
    elif scenario == "r70-sequence-column":
        migrations["migration_70_schema"]["tables"]["erasure_outbox"]["columns"]["seq"]["sequence"] = True
    elif scenario == "r70-generated-column":
        migrations["migration_70_schema"]["tables"]["erasure_outbox"]["columns"]["seq"]["generated"] = "s"
    elif scenario == "r70-primary-key-order":
        migrations["migration_70_schema"]["tables"]["erasure_outbox"]["constraints"]["erasure_outbox_pkey"]["columns"].reverse()
    elif scenario == "r70-missing-unique":
        migrations["migration_70_schema"]["tables"]["erasure_outbox"]["constraints"].pop("erasure_outbox_operation_key_unique")
    elif scenario == "r70-extra-index":
        table = migrations["migration_70_schema"]["tables"]["erasure_epoch"]
        table["index_names"].append("erasure_epoch_unapproved_idx")
        table["index_validity"]["erasure_epoch_unapproved_idx"] = True
    elif scenario == "r70-invalid-index":
        migrations["migration_70_schema"]["tables"]["erasure_epoch"]["index_validity"]["erasure_epoch_pkey"] = False
    elif scenario.startswith("r70-completion-fk-"):
        constraint = migrations["migration_70_schema"]["tables"]["erasure_epoch_completion"]["constraints"][
            "erasure_epoch_completion_marker_exists"
        ]
        mutation = scenario.removeprefix("r70-completion-fk-")
        if mutation == "missing":
            migrations["migration_70_schema"]["tables"]["erasure_epoch_completion"]["constraints"].pop(
                "erasure_epoch_completion_marker_exists"
            )
        elif mutation == "local-order":
            constraint["columns"].reverse()
        elif mutation == "referenced-order":
            constraint["referenced_columns"].reverse()
        elif mutation == "delete-action":
            constraint["on_delete"] = "CASCADE"
        elif mutation == "update-action":
            constraint["on_update"] = "CASCADE"
        elif mutation == "match":
            constraint["match"] = "FULL"
        elif mutation == "referenced-index":
            constraint["referenced_index"] = "erasure_epoch_unapproved_idx"
        elif mutation == "unvalidated":
            constraint["validated"] = False
        elif mutation == "deferrable":
            constraint["deferrable"] = True
        elif mutation == "initially-deferred":
            constraint["initially_deferred"] = True
        else:
            raise AssertionError(f"unknown R70 completion FK mutation: {mutation}")
    elif scenario in {"r70-extra-outbox-fk", "r70-files-fk"}:
        name = "erasure_outbox_unapproved_fkey"
        referenced_table = "public.files" if scenario == "r70-files-fk" else "public.erasure_epoch"
        migrations["migration_70_schema"]["tables"]["erasure_outbox"]["constraints"][name] = {
            "type": "f",
            "validated": True,
            "columns": ["epoch"],
            "referenced_table": referenced_table,
            "referenced_columns": ["id" if scenario == "r70-files-fk" else "epoch"],
            "on_delete": "NO ACTION",
            "on_update": "NO ACTION",
            "match": "SIMPLE",
            "set_null_columns": [],
            "referenced_index": "files_pkey" if scenario == "r70-files-fk" else "erasure_epoch_pkey",
            "check_expression": None,
        }
    elif scenario == "r70-inbound-fk":
        migrations["migration_70_schema"]["inbound_foreign_keys"].append(
            {"table": "public.files", "name": "files_erasure_epoch_fkey"}
        )
    elif scenario.startswith("r70-trigger-"):
        table_name = scenario.removeprefix("r70-trigger-")
        migrations["migration_70_schema"]["user_triggers"][table_name].append("erasure_probe_trigger")
    elif scenario == "r70-extra-check":
        migrations["migration_70_schema"]["tables"]["erasure_epoch"]["constraints"]["erasure_epoch_probe_check"] = {
            "type": "c"
        }
    elif scenario == "r70-renamed-check":
        constraints = migrations["migration_70_schema"]["tables"]["erasure_epoch"]["constraints"]
        constraints["erasure_epoch_epoch_probe_check"] = constraints.pop("erasure_epoch_epoch_check")
    elif scenario.startswith("r70-inert-row-"):
        table_name = scenario.removeprefix("r70-inert-row-")
        migrations["inert_surfaces"][table_name] = True
    elif scenario == "r70-inert-malformed":
        migrations["inert_surfaces"]["erasure_outbox"] = "false"
    elif scenario == "r70-inert-missing":
        migrations["inert_surfaces"].pop("erasure_outbox")
    elif scenario == "r70-inert-extra":
        migrations["inert_surfaces"]["unapproved"] = False
    elif scenario == "r70-inert-query-hash":
        metadata["references"]["inert_surfaces_query_sha256"] = "0" * 64
        dump_json(bundle / "qualification.json", metadata)
    elif scenario == "r70-inert-query-missing":
        metadata["references"].pop("inert_surfaces_query_sha256")
        dump_json(bundle / "qualification.json", metadata)
    elif scenario == "r70-reference-extra-key":
        metadata["references"]["unapproved"] = "0" * 64
        dump_json(bundle / "qualification.json", metadata)
    elif scenario == "r70-inert-capture-missing":
        metadata["freeze"].pop("inert_surfaces_captured_at")
        dump_json(bundle / "qualification.json", metadata)
    elif scenario == "r70-inert-capture-outside-freeze":
        metadata["freeze"]["inert_surfaces_captured_at"] = "2026-10-07T18:06:00Z"
        dump_json(bundle / "qualification.json", metadata)
    elif scenario == "r70-file-row":
        file_rows = [(FILE_KEY, 5, "a" * 64), (PART_KEY, 3, "b" * 64)]
        for name in (
            "source-provisional.tsv", "source-frozen.tsv", "copy-pass-1.tsv", "copy-pass-2.tsv",
            "destination-census-pass-1.tsv", "destination-census-pass-2.tsv",
        ):
            dump_bytes(bundle / name, manifest(file_rows))
        file_refs = f"file\ttrue\t{FILE_KEY}\nupload_part\ttrue\t{PART_KEY}\n".encode("ascii")
        file_links = f"messages\t258\ttrue\n".encode("ascii")
        dump_bytes(bundle / "references-provisional.tsv", file_refs)
        dump_bytes(bundle / "references.tsv", file_refs)
        dump_bytes(bundle / "active-links-provisional.tsv", file_links)
        dump_bytes(bundle / "active-links.tsv", file_links)

    if scenario == "changed-migration-file":
        path = checkout / "migrations" / "20261007000066_dialog_unread_marks.sql"
        path.write_bytes(path.read_bytes() + b"-- unreviewed change\n")
    elif scenario == "changed-67-migration-file":
        path = checkout / "migrations" / LIVE_MIGRATION_67
        path.write_bytes(path.read_bytes() + b"-- unreviewed change\n")
    elif scenario in ("changed-68-migration-file", "changed-69-migration-file"):
        name = LIVE_MIGRATION_68 if scenario == "changed-68-migration-file" else LIVE_MIGRATION_69
        path = checkout / "migrations" / name
        path.write_bytes(path.read_bytes() + b"-- unreviewed change\n")
    elif scenario == "changed-70-migration-file":
        path = checkout / "migrations" / LIVE_MIGRATION_70
        path.write_bytes(path.read_bytes() + b"-- unreviewed change\n")
    elif scenario == "tampered-67-atlas-row":
        path = checkout / "migrations" / "atlas.sum"
        path.write_bytes(
            path.read_bytes().replace(
                b"20261008000067_secret_chat_party_date_idx.sql h1:Lux8heOMbxuuDRHoHwo61qwT/B+Nm05v6jFNbXvz2EE=",
                b"20261008000067_secret_chat_party_date_idx.sql h1:tampered",
            )
        )
    elif scenario == "extra-migration-file":
        extra = checkout / "migrations" / "20261007000067_unreviewed.sql"
        extra.write_text("SELECT 1;\n", encoding="utf-8")
        extra.chmod(0o600)
    elif scenario == "extra-68-file":
        extra = checkout / "migrations" / "20261008000068_unreviewed.sql"
        extra.write_text("SELECT 1;\n", encoding="utf-8")
        extra.chmod(0o600)
    elif scenario == "extra-70-file":
        extra = checkout / "migrations" / "20261009000070_unreviewed.sql"
        extra.write_text("SELECT 1;\n", encoding="utf-8")
        extra.chmod(0o600)
    elif scenario == "extra-71-file":
        extra = checkout / "migrations" / "20261009000071_unreviewed.sql"
        extra.write_text("SELECT 1;\n", encoding="utf-8")
        extra.chmod(0o600)
    elif scenario == "tampered-atlas-sum":
        path = checkout / "migrations" / "atlas.sum"
        release = gate_constants(release_set)
        name = release["migration_files"][-1]
        approved_row = f"{name} {release['atlas_pins'][name]}".encode("ascii")
        path.write_bytes(path.read_bytes().replace(approved_row, f"{name} h1:tampered".encode("ascii")))
    elif scenario == "overbroad-policy":
        policy_path = checkout / "deploy" / "rustfs" / "telegramd-blob.json"
        policy_path.write_text(
            policy_path.read_text(encoding="utf-8").replace("s3:DeleteObject", "s3:*"),
            encoding="utf-8",
        )
    elif scenario == "empty-reference-coverage":
        for name in (
            "source-provisional.tsv", "source-frozen.tsv", "copy-pass-1.tsv", "copy-pass-2.tsv",
            "destination-census-pass-1.tsv", "destination-census-pass-2.tsv",
        ):
            dump_bytes(bundle / name, b"")
        dump_bytes(bundle / "references-provisional.tsv", b"")
        dump_bytes(bundle / "active-links-provisional.tsv", b"")
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

    if scenario in (
        "writable-state-dir",
        "writable-blob-mode-dir",
        "unowned-state-dir",
        "unowned-blob-mode-dir",
    ):
        protected_dir = checkout / ".state"
        if scenario.endswith("blob-mode-dir"):
            protected_dir = protected_dir / "blob-mode"
        if scenario.startswith("writable-"):
            protected_dir.chmod(0o777)
        else:
            os.chown(protected_dir, 65534, 65534)

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
    def run_bundle(
        self,
        root: Path,
        scenario: str,
        expected_reason: str | None = None,
        fixture_root: Path | None = None,
        release_set: str = "60-66",
    ) -> subprocess.CompletedProcess[str]:
        bundle, checkout, mock_bin, events = write_bundle(root, scenario, fixture_root, release_set)
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
        if result.returncode == 0:
            evidence = json.loads((bundle / "migrations.json").read_text(encoding="utf-8"))
            digest = hashlib.sha256((bundle / "migrations.json").read_bytes()).hexdigest()
            expected_output = (
                "gate_result=pass"
                f" release_set={release_set}"
                f" applied_versions={','.join(evidence['target_revisions'])}"
                f" migrations_sha256={digest}"
            )
            self.assertEqual(result.stdout.strip(), expected_output)
        if expected_reason is not None:
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("gate_result=reject", result.stderr)
            self.assertIn(f"reason={expected_reason}", result.stderr)
        return result

    def run_scenario(
        self,
        scenario: str,
        expected_reason: str | None = None,
        release_set: str = "60-66",
    ) -> subprocess.CompletedProcess[str]:
        temp = tempfile.TemporaryDirectory(prefix="rustfs-transition-gate.", dir=os.environ.get("TMPDIR", "/root"))
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        if expected_reason == "schema_rejected":
            base = self.run_bundle(root / "passing-base", "success", release_set=release_set)
            self.assertEqual(
                base.returncode,
                0,
                f"schema negative {scenario} is invalid because its unmutated base was rejected: {base.stderr}",
            )
            self.assertIn("gate_result=pass", base.stdout)
        return self.run_bundle(root / "scenario", scenario, expected_reason, release_set=release_set)

    def add_r70_live_captures(self, bundle: Path, *, include_baseline: bool = True) -> None:
        migrations_path = bundle / "migrations.json"
        migrations = json.loads(migrations_path.read_text(encoding="utf-8"))
        qualification = json.loads((bundle / "qualification.json").read_text(encoding="utf-8"))
        dump_sha256 = hashlib.sha256((bundle / "postgres.dump").read_bytes()).hexdigest()
        detail_keys = ["applied", "total", "error", "hash"]
        detail_keys.extend(("error_stmt_empty", "partial_hashes_empty"))
        observation = {
            "applied_revisions": VERSIONS_60_70,
            "revision_detail": {
                version: {
                    key: detail[key]
                    for key in detail_keys
                }
                for version, detail in migrations["revision_detail"].items()
            },
            "migration_66_schema": migrations["migration_66_schema"],
            "migration_67_schema": migrations["migration_67_schema"],
        }
        capture_common = {
            "schema": "teagram.live-migration-schema/v1",
            "dump_sha256": dump_sha256,
            "query_sha256": gate_constants("60-70")["live_schema_query_sha256"],
            "observed": observation,
        }
        freeze = qualification["freeze"]
        if include_baseline:
            migrations["baseline_live_capture"] = {
                **capture_common,
                "captured_at": freeze["baseline_schema_captured_at"],
                "query_output_sha256": "1" * 64,
            }
        else:
            migrations.pop("baseline_live_capture", None)
        migrations["live_capture"] = {
            **capture_common,
            "captured_at": freeze["schema_captured_at"],
            "query_output_sha256": "2" * 64,
        }
        dump_json(migrations_path, migrations)

    def add_r70_recovery_capture(self, bundle: Path, scenario: str = "success") -> None:
        migrations_path = bundle / "migrations.json"
        migrations = json.loads(migrations_path.read_text(encoding="utf-8"))
        qualification = json.loads((bundle / "qualification.json").read_text(encoding="utf-8"))
        freeze = qualification["freeze"]
        recovery_freeze = {
            "held": True,
            "inventory_complete": True,
            "started_at": freeze["started_at"],
            "baseline_schema_captured_at": freeze["baseline_schema_captured_at"],
            "schema_captured_at": freeze["schema_captured_at"],
            "inert_surfaces_captured_at": freeze["inert_surfaces_captured_at"],
            "held_at": freeze["held_at"],
        }
        recovery = {
            "schema": "teagram.blob-recovery-qualification/v1",
            "source_volume": "telegram-server_tgblobs",
            "freeze": recovery_freeze,
            "dump": {
                "captured_at": TIMES["dump"],
                "sha256": hashlib.sha256((bundle / "postgres.dump").read_bytes()).hexdigest(),
            },
            "references": {
                "inert_surfaces_query_sha256": gate_constants("60-70")[
                    "r70_inert_surfaces_query_sha256"
                ],
            },
        }
        if scenario == "missing-inert-capture":
            recovery["freeze"].pop("inert_surfaces_captured_at")
        elif scenario == "inert-capture-outside-freeze":
            recovery["freeze"]["inert_surfaces_captured_at"] = "2026-10-07T18:06:00Z"
        elif scenario == "wrong-inert-query-digest":
            recovery["references"]["inert_surfaces_query_sha256"] = "0" * 64
        elif scenario == "applied-capture-after-dump":
            after_dump = "2026-10-07T18:01:30Z"
            recovery["freeze"]["schema_captured_at"] = after_dump
            migrations["live_capture"]["captured_at"] = after_dump
            dump_json(migrations_path, migrations)
        dump_json(bundle / "recovery.json", recovery)

    def test_approved_bundle_passes_with_aggregate_only_output(self) -> None:
        result = self.run_scenario("success")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("gate_result=pass", result.stdout)
        self.assertIn("release_set=60-66", result.stdout)
        self.assertIn("applied_versions=", result.stdout)
        self.assertIn("migrations_sha256=", result.stdout)

    def test_pre_copy_gate_accepts_complete_frozen_source_without_copy_artifacts(self) -> None:
        temp = tempfile.TemporaryDirectory(prefix="rustfs-transition-pre-copy.", dir=os.environ.get("TMPDIR", "/root"))
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        bundle, checkout, mock_bin, events = write_bundle(root)
        for name in (
            "copy-pass-1.tsv",
            "copy-pass-2.tsv",
            "destination-census-pass-1.tsv",
            "destination-census-pass-2.tsv",
        ):
            (bundle / name).unlink()
        before_bundle = tree_digest(bundle)
        environment = os.environ.copy()
        environment["PATH"] = f"{mock_bin}:{environment['PATH']}"
        environment["MOCK_EVENTS"] = events
        result = subprocess.run(
            ["bash", str(GATE), "pre-copy", str(bundle), str(checkout)],
            env=environment,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(tree_digest(bundle), before_bundle)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("gate_result=pre-copy-pass release_set=60-66", result.stdout)
        self.assertIn("applied_versions=" + ",".join(VERSIONS_60_66), result.stdout)
        self.assertIn("migrations_sha256=", result.stdout)
        self.assertNotIn("destination_manifest_sha256", result.stdout)
        event_text = Path(events).read_text(encoding="utf-8")
        self.assertNotIn(APP_ACCESS, event_text)
        self.assertNotIn(APP_SECRET, event_text)

    def test_forbidden_override_is_rejected(self) -> None:
        self.run_scenario("forbidden-override", "protected_override")

    def test_mixed_trust_writer_is_rejected_during_freeze(self) -> None:
        self.run_scenario("mixed-trust-writer", "writer_freeze_incomplete")

    def test_substituted_volume_identity_is_rejected(self) -> None:
        self.run_scenario("wrong-inspected-volume", "baseline_env_provenance")

    def test_mismatched_live_blob_directory_is_rejected(self) -> None:
        self.run_scenario("mismatched-blob-directory", "baseline_env_provenance")

    def test_missing_nonprofiled_running_baseline_service_is_rejected(self) -> None:
        self.run_scenario("missing-baseline-service", "baseline_env_provenance")

    def test_exited_unprofiled_migrate_service_is_not_required_in_steady_state(self) -> None:
        result = self.run_scenario("success")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("gate_result=pass", result.stdout)

    def test_baseline_and_candidate_extra_mount_missing_from_live_inventory_is_rejected(self) -> None:
        self.run_scenario("baseline-and-candidate-extra-mount", "source_identity")

    def test_baseline_and_candidate_extra_environment_missing_from_live_inventory_is_rejected(self) -> None:
        self.run_scenario("baseline-and-candidate-extra-environment", "source_identity")

    def test_baseline_and_candidate_extra_port_missing_from_live_inventory_is_rejected(self) -> None:
        self.run_scenario("baseline-and-candidate-extra-port", "source_identity")

    def test_state_directories_must_be_root_owned_and_not_group_or_world_writable(self) -> None:
        for scenario in (
            "writable-state-dir",
            "writable-blob-mode-dir",
            "unowned-state-dir",
            "unowned-blob-mode-dir",
        ):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "configuration_mismatch")

    def test_empty_source_identity_is_rejected(self) -> None:
        self.run_scenario("wrong-source-identity", "source_identity")

    def test_rustfs_volume_cannot_alias_a_baseline_volume(self) -> None:
        self.run_scenario("rustfsdata-alias", "source_identity")

    def test_changed_frozen_census_is_rejected(self) -> None:
        self.run_scenario("changed-census", "source_census_changed")

    def test_second_copy_pass_with_different_digest_is_rejected(self) -> None:
        self.run_scenario("different-copy-pass-2", "manifest_mismatch")

    def test_second_destination_census_with_different_digest_is_rejected(self) -> None:
        self.run_scenario("different-destination-census-pass-2", "manifest_mismatch")

    def test_reference_snapshot_change_after_freeze_is_rejected(self) -> None:
        self.run_scenario("changed-references-after-freeze", "reference_snapshot_changed")

    def test_active_link_snapshot_change_after_freeze_is_rejected(self) -> None:
        self.run_scenario("changed-active-links-after-freeze", "reference_snapshot_changed")

    def test_missing_reference_is_rejected_even_for_empty_copy_output(self) -> None:
        self.run_scenario("missing-reference", "reference_coverage")

    def test_unstored_file_without_blob_does_not_block_qualification(self) -> None:
        result = self.run_scenario("unstored-file-absent")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("gate_result=pass", result.stdout)

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

    def test_preprovision_env_provenance_is_required_and_bound(self) -> None:
        for scenario in (
            "missing-baseline-provenance",
            "wrong-baseline-provenance-source",
            "mismatched-recovery-point-env",
            "mismatched-inspected-env",
            "mismatched-inspected-baseline",
            "mismatched-baseline-compose",
            "mismatched-recovery-point-document",
        ):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "baseline_env_provenance")

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

    def test_real_live_migration_67_is_rejected(self) -> None:
        self.run_scenario("real-67-file", "schema_rejected")

    def test_current_checkout_with_unapplied_future_migrations_uses_pinned_r67_snapshot(self) -> None:
        migrations_dir = PROJECT_ROOT / "migrations"
        for version in ("20261008000068", "20261008000069"):
            self.assertTrue(any(path.name.startswith(version + "_") for path in migrations_dir.glob("*.sql")))
        result = self.run_scenario("live-migrations-overlay", release_set="60-67")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("gate_result=pass release_set=60-67", result.stdout)

    def test_unapplied_future_migration_files_must_match_their_atlas_hashes(self) -> None:
        for scenario in ("changed-68-migration-file", "changed-69-migration-file"):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "schema_rejected", release_set="60-67")

    def test_r67_keeps_the_four_existing_positive_cases(self) -> None:
        for scenario in (
            "success",
            "success",
            "unstored-file-absent",
            "inherited-compose-override",
        ):
            with self.subTest(scenario=scenario):
                result = self.run_scenario(scenario, release_set="60-67")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("release_set=60-67", result.stdout)
                self.assertIn("applied_versions=" + ",".join(VERSIONS_60_67), result.stdout)

    def test_r67_live_schema_captures_are_frozen_and_dump_bound(self) -> None:
        temp = tempfile.TemporaryDirectory(
            prefix="rustfs-transition-r67-live-schema.", dir=os.environ.get("TMPDIR", "/root")
        )
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        bundle, checkout, mock_bin, events = write_bundle(root, release_set="60-67")
        migrations_path = bundle / "migrations.json"
        migrations = json.loads(migrations_path.read_text(encoding="utf-8"))
        dump_sha256 = hashlib.sha256((bundle / "postgres.dump").read_bytes()).hexdigest()
        observation = {
            "applied_revisions": VERSIONS_60_67,
            "revision_detail": migrations["revision_detail"],
            "migration_66_schema": migrations["migration_66_schema"],
            "migration_67_schema": migrations["migration_67_schema"],
        }
        capture_common = {
            "schema": "teagram.live-migration-schema/v1",
            "dump_sha256": dump_sha256,
            "query_sha256": gate_constants("60-67")["live_schema_query_sha256"],
            "observed": observation,
        }
        migrations["baseline_live_capture"] = {
            **capture_common,
            "captured_at": "2026-10-07T18:00:30Z",
            "query_output_sha256": "1" * 64,
        }
        migrations["live_capture"] = {
            **capture_common,
            "captured_at": TIMES["schema"],
            "query_output_sha256": "2" * 64,
        }
        dump_json(migrations_path, migrations)
        environment = os.environ.copy()
        environment["PATH"] = f"{mock_bin}:{environment['PATH']}"
        environment["MOCK_EVENTS"] = events
        passing = subprocess.run(
            ["bash", str(GATE), "check", str(bundle), str(checkout)],
            env=environment,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(passing.returncode, 0, passing.stderr)

        migrations["baseline_live_capture"]["dump_sha256"] = "0" * 64
        dump_json(migrations_path, migrations)
        mismatched_dump = subprocess.run(
            ["bash", str(GATE), "check", str(bundle), str(checkout)],
            env=environment,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertNotEqual(mismatched_dump.returncode, 0)
        self.assertIn("reason=schema_rejected", mismatched_dump.stderr)

    def test_r67_live_revision_captures_reject_incomplete_error_and_hash_mismatches(self) -> None:
        temp = tempfile.TemporaryDirectory(
            prefix="rustfs-transition-r67-live-revision.", dir=os.environ.get("TMPDIR", "/root")
        )
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        bundle, checkout, mock_bin, events = write_bundle(root, release_set="60-67")
        original = json.loads((bundle / "migrations.json").read_text(encoding="utf-8"))
        dump_sha256 = hashlib.sha256((bundle / "postgres.dump").read_bytes()).hexdigest()
        observation = {
            "applied_revisions": VERSIONS_60_67,
            "revision_detail": original["revision_detail"],
            "migration_66_schema": original["migration_66_schema"],
            "migration_67_schema": original["migration_67_schema"],
        }
        capture_common = {
            "schema": "teagram.live-migration-schema/v1",
            "dump_sha256": dump_sha256,
            "query_sha256": gate_constants("60-67")["live_schema_query_sha256"],
        }
        environment = os.environ.copy()
        environment["PATH"] = f"{mock_bin}:{environment['PATH']}"
        environment["MOCK_EVENTS"] = events
        mutations = {
            "applied": 0,
            "total": 2,
            "error": "migration failed",
            "hash": "h1:stale",
        }
        for capture_name in ("baseline_live_capture", "live_capture"):
            for field, bad_value in mutations.items():
                with self.subTest(capture=capture_name, field=field):
                    evidence = json.loads(json.dumps(original))
                    evidence["baseline_live_capture"] = {
                        **capture_common,
                        "captured_at": "2026-10-07T18:00:30Z",
                        "query_output_sha256": "1" * 64,
                        "observed": json.loads(json.dumps(observation)),
                    }
                    evidence["live_capture"] = {
                        **capture_common,
                        "captured_at": TIMES["schema"],
                        "query_output_sha256": "2" * 64,
                        "observed": json.loads(json.dumps(observation)),
                    }
                    evidence[capture_name]["observed"]["revision_detail"][VERSIONS_60_67[-1]][field] = bad_value
                    dump_json(bundle / "migrations.json", evidence)
                    result = subprocess.run(
                        ["bash", str(GATE), "check", str(bundle), str(checkout)],
                        env=environment,
                        text=True,
                        capture_output=True,
                        check=False,
                    )
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("reason=schema_rejected", result.stderr)

    def test_r67_live_schema_rejects_applied_future_revisions(self) -> None:
        temp = tempfile.TemporaryDirectory(
            prefix="rustfs-transition-r67-future-revision.", dir=os.environ.get("TMPDIR", "/root")
        )
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        bundle, checkout, mock_bin, events = write_bundle(root, release_set="60-67")
        migrations_path = bundle / "migrations.json"
        migrations = json.loads(migrations_path.read_text(encoding="utf-8"))
        dump_sha256 = hashlib.sha256((bundle / "postgres.dump").read_bytes()).hexdigest()
        observation = {
            "applied_revisions": VERSIONS_60_67,
            "revision_detail": migrations["revision_detail"],
            "migration_66_schema": migrations["migration_66_schema"],
            "migration_67_schema": migrations["migration_67_schema"],
        }
        capture_common = {
            "schema": "teagram.live-migration-schema/v1",
            "dump_sha256": dump_sha256,
            "query_sha256": gate_constants("60-67")["live_schema_query_sha256"],
        }
        environment = os.environ.copy()
        environment["PATH"] = f"{mock_bin}:{environment['PATH']}"
        environment["MOCK_EVENTS"] = events

        for future_version in ("20261008000068", "20261008000069"):
            with self.subTest(future_version=future_version):
                evidence = json.loads(json.dumps(migrations))
                observed_with_future = {**observation, "applied_revisions": VERSIONS_60_67 + [future_version]}
                evidence["baseline_live_capture"] = {
                    **capture_common,
                    "captured_at": "2026-10-07T18:00:30Z",
                    "query_output_sha256": "1" * 64,
                    "observed": observed_with_future,
                }
                evidence["live_capture"] = {
                    **capture_common,
                    "captured_at": TIMES["schema"],
                    "query_output_sha256": "2" * 64,
                    "observed": observed_with_future,
                }
                dump_json(migrations_path, evidence)
                result = subprocess.run(
                    ["bash", str(GATE), "check", str(bundle), str(checkout)],
                    env=environment,
                    text=True,
                    capture_output=True,
                    check=False,
                )
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("reason=schema_rejected", result.stderr)

    def test_r67_rejects_an_incomplete_database_and_a_reverted_checkout(self) -> None:
        self.run_scenario("r67-db-60-66", "schema_rejected", release_set="60-67")
        self.run_scenario("r66-db-60-67", "schema_rejected")

    def test_r67_rejects_baseline_revisions_without_changing_applied_evidence(self) -> None:
        for scenario in ("r67-baseline-60-65", "r67-baseline-60-62"):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "schema_rejected", release_set="60-67")

    def test_revision_metadata_selects_the_pinned_release_snapshot(self) -> None:
        self.run_scenario("release-set-67", "schema_rejected")
        self.run_scenario("release-set-missing", "schema_rejected", release_set="60-67")
        self.run_scenario("release-set-66", "schema_rejected", release_set="60-67")

    def test_r67_requires_in_freeze_baseline_and_applied_captures(self) -> None:
        for scenario in (
            "r67-baseline-capture-after-dump",
            "r67-baseline-capture-missing",
            "r67-applied-capture-outside-freeze",
        ):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "schema_rejected", release_set="60-67")

    def test_r67_rejects_each_67_index_property_mutation(self) -> None:
        for party in ("admin", "participant"):
            for mutation in (
                "missing",
                "invalid",
                "validity-type",
                "not-ready",
                "not-live",
                "unique",
                "primary",
                "partial",
                "expression",
                "wrong-method",
                "wrong-count",
                "wrong-key-count",
                "wrong-option",
                "swapped",
                "renamed",
            ):
                scenario = f"r67-index-{mutation}-{party}"
                with self.subTest(scenario=scenario):
                    self.run_scenario(scenario, "schema_rejected", release_set="60-67")
        self.run_scenario("r67-extra-invalid-index", "schema_rejected", release_set="60-67")
        self.run_scenario("r67-extra-schema-key", "schema_rejected", release_set="60-67")

    def test_r67_revision_capture_is_complete_successful_and_pinned(self) -> None:
        for scenario in (
            "r67-revision-incomplete",
            "r67-revision-error",
            "r67-revision-hash",
            "r67-extra-revision-detail",
            "r67-extra-migrations-key",
            "r67-extra-68-row",
        ):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "schema_rejected", release_set="60-67")

    def test_r67_keeps_all_existing_66_schema_negatives(self) -> None:
        for scenario in (
            "missing-66",
            "wrong-version-66",
            "wrong-unread-schema",
            "changed-migration-file",
            "extra-migration-file",
            "tampered-atlas-sum",
        ):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "schema_rejected", release_set="60-67")

    def test_r67_pins_67_bytes_atlas_row_and_rejects_future_revisions(self) -> None:
        for scenario in (
            "changed-67-migration-file",
            "tampered-67-atlas-row",
            "extra-68-file",
        ):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "schema_rejected", release_set="60-67")

    def test_r69_exact_release_passes_and_pins_the_existing_queries(self) -> None:
        result = self.run_scenario("success", release_set="60-69")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("release_set=60-69", result.stdout)
        constants = gate_constants("60-69")
        self.assertEqual(
            constants["reference_query_sha256"],
            "c9963fdc620abd2656faeadb2dc7fab7e9d101013a8d810a655cf3832d374aa3",
        )
        self.assertEqual(
            constants["active_links_query_sha256"],
            "9c85d110fc08cd71b29cc26b6419a571e0676a529f3302af01c5397866c63a40",
        )
        self.assertEqual(
            constants["inert_surfaces_query_sha256"],
            "2d0c108eb69b0cab431f01837a649e5e7f14d33483aae677be1032d5aa32cfe3",
        )

    def test_qualifier_rejects_obsolete_artifact_digests(self) -> None:
        artifacts = (
            "qualify-rustfs-transition.py",
            "rustfs-schema-capture.sql",
            "rustfs-inert-surfaces.sql",
            "rustfs-r70-schema-capture.sql",
            "rustfs-r70-inert-surfaces.sql",
        )
        with tempfile.TemporaryDirectory(prefix="r69-qualifier-digest-") as temporary:
            root = Path(temporary)
            for artifact in artifacts:
                with self.subTest(artifact=artifact):
                    runtime = root / artifact
                    runtime.mkdir()
                    for name in (GATE.name, *artifacts):
                        shutil.copy2(SCRIPT_DIR / name, runtime / name)
                    with (runtime / artifact).open("ab") as changed:
                        changed.write(b"\n")

                    result = subprocess.run(
                        [
                            str(runtime / GATE.name),
                            "check",
                            str(runtime / "bundle"),
                            str(runtime / "checkout"),
                        ],
                        capture_output=True,
                        text=True,
                        check=False,
                    )
                    self.assertEqual(result.returncode, 1, result.stderr)
                    self.assertIn(
                        "gate_result=reject reason=qualifier_artifact_digest",
                        result.stderr,
                    )

    def test_qualifier_artifact_pin_matches_checked_in_files(self) -> None:
        gate_source = GATE.read_text(encoding="utf-8")
        artifact_list = re.search(
            r"(?ms)^readonly -a QUALIFIER_ARTIFACTS=\(\n(.*?)^\)", gate_source
        )
        self.assertIsNotNone(artifact_list, "qualifier artifact list is not pinned")
        artifacts = re.findall(r"(?m)^\s+([A-Za-z0-9._-]+)\s*$", artifact_list.group(1))
        self.assertTrue(artifacts, "qualifier artifact list is empty")
        digest_input = "".join(
            f"{hashlib.sha256((SCRIPT_DIR / artifact).read_bytes()).hexdigest()}  "
            f"{artifact}\n"
            for artifact in artifacts
        ).encode("ascii")
        expected_digest = hashlib.sha256(digest_input).hexdigest()
        match = re.search(
            r"(?m)^readonly APPROVED_QUALIFIER_ARTIFACT_SHA256=([0-9a-f]{64})$",
            gate_source,
        )
        self.assertIsNotNone(match, "qualifier artifact digest is not pinned")
        self.assertEqual(
            match.group(1),
            expected_digest,
            "qualifier artifact pin must match the reviewed gate and SQL files",
        )

    def test_r69_rejects_incomplete_and_cross_release_databases(self) -> None:
        self.run_scenario("r69-db-60-67", "schema_rejected", release_set="60-69")
        self.run_scenario("r69-db-60-68", "schema_rejected", release_set="60-69")
        self.run_scenario("r67-db-60-69", "schema_rejected", release_set="60-67")
        self.run_scenario("r69-baseline-60-68", "schema_rejected", release_set="60-69")

    def test_r69_requires_complete_in_freeze_baseline_and_applied_captures(self) -> None:
        for scenario in (
            "r69-baseline-capture-after-dump",
            "r69-baseline-capture-missing",
            "r69-applied-capture-outside-freeze",
            "r69-applied-capture-before-baseline",
        ):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "schema_rejected", release_set="60-69")

    def test_r69_revision_rows_and_details_are_exact_and_successful(self) -> None:
        for scenario in (
            "r69-extra-70-row",
            "r69-68-present-false",
            "r69-69-present-false",
            "r69-revision-68-incomplete",
            "r69-revision-68-error",
            "r69-revision-68-hash",
            "r69-revision-69-incomplete",
            "r69-revision-69-error",
            "r69-revision-69-hash",
            "r69-extra-migrations-key",
            "r69-extra-schema-key",
            "r69-extra-schema-table",
        ):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "schema_rejected", release_set="60-69")

    def test_r69_rejects_each_migration_68_index_mutation(self) -> None:
        for scenario in (
            "r69-68-index-missing",
            "r69-68-index-renamed",
            "r69-68-index-extra",
            "r69-68-index-invalid",
            "r69-68-index-not-ready",
            "r69-68-index-not-live",
            "r69-68-index-not-immediate",
            "r69-68-index-not-unique",
            "r69-68-index-primary",
            "r69-68-index-method",
            "r69-68-index-partial",
            "r69-68-index-expression",
            "r69-68-index-attributes",
            "r69-68-index-key-count",
            "r69-68-index-option",
            "r69-68-index-columns",
        ):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "schema_rejected", release_set="60-69")

    def test_r69_rejects_ordered_keys_actions_and_reference_catalog_mismatches(self) -> None:
        for scenario in (
            "r69-ownership-local-order",
            "r69-ownership-referenced-order",
            "r69-ownership-action",
            "r69-ownership-conindid",
            "r69-pointer-local-order",
            "r69-pointer-referenced-order",
            "r69-pointer-match",
            "r69-receipt-set-null-columns",
            "r69-missing-ownership-fk",
            "r69-extra-auth-key-fk",
            "r69-primary-key-order",
            "r69-missing-unique",
            "r69-column-type",
            "r69-column-nullability",
            "r69-column-default",
            "r69-index-extra",
            "r69-index-invalid",
        ):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "schema_rejected", release_set="60-69")

    def test_r69_rejects_any_successor_row_or_nonempty_files(self) -> None:
        for table in (
            "user_photos",
            "profile_photo_state",
            "profile_upload_receipt",
            "profile_delete_operation",
        ):
            with self.subTest(table=table):
                self.run_scenario(f"r69-inert-row-{table}", "reference_coverage", release_set="60-69")
        self.run_scenario("r69-file-row", "reference_coverage", release_set="60-69")

    def test_r69_inert_surface_capture_is_pinned_and_typed(self) -> None:
        self.run_scenario("r69-inert-query-hash", "reference_coverage", release_set="60-69")
        self.run_scenario("r69-inert-malformed", "schema_rejected", release_set="60-69")

    def test_r69_pins_each_new_file_and_rejects_a_70_or_changed_sum(self) -> None:
        for scenario in (
            "changed-68-migration-file",
            "changed-69-migration-file",
            "tampered-atlas-sum",
            "extra-70-file",
        ):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "schema_rejected", release_set="60-69")

    def test_r70_exact_release_and_query_pins(self) -> None:
        result = self.run_scenario("success", release_set="60-70")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("release_set=60-70", result.stdout)
        constants = gate_constants("60-70")
        self.assertEqual(constants["atlas_sum_sha256"], "e16da8e46119290cac52762235a47f47efd796ab2847db56a368a36d3b2ec608")
        self.assertEqual(constants["migration_sha256"][LIVE_MIGRATION_70], "cf7bc135c5df5a539cf5b77d76136ab321fc63e6d0a787566b68b8e052779a2c")
        self.assertEqual(
            constants["atlas_pins"][LIVE_MIGRATION_70],
            "h1:3ZPWNySt9YWg9s+xi+aQVAFrcSkeym+fGlL4PgkIayk=",
        )
        self.assertEqual(constants["r70_inert_surfaces_query_sha256"], hashlib.sha256(
            (SCRIPT_DIR / "rustfs-r70-inert-surfaces.sql").read_bytes()
        ).hexdigest())

    def test_r70_accepts_captured_live_schema(self) -> None:
        temp = tempfile.TemporaryDirectory(
            prefix="rustfs-transition-r70-live-capture.",
            dir=os.environ.get("TMPDIR", "/root"),
        )
        self.addCleanup(temp.cleanup)
        bundle, checkout, _, _ = write_bundle(Path(temp.name), release_set="60-70")
        self.add_r70_live_captures(bundle)
        gate = gate_namespace()

        applied = gate["validate_migration_schema"](
            bundle, checkout, require_live_capture=True
        )

        self.assertEqual(applied, VERSIONS_60_70)

    def test_r70_live_capture_requires_baseline_capture(self) -> None:
        temp = tempfile.TemporaryDirectory(
            prefix="rustfs-transition-r70-live-capture.",
            dir=os.environ.get("TMPDIR", "/root"),
        )
        self.addCleanup(temp.cleanup)
        bundle, checkout, _, _ = write_bundle(Path(temp.name), release_set="60-70")
        self.add_r70_live_captures(bundle, include_baseline=False)
        gate = gate_namespace()
        # Isolate the required baseline check from observation-shape validation.
        gate["validate_live_schema_observation"] = lambda *_args: None

        with self.assertRaises(gate["GateReject"]) as rejected:
            gate["validate_migration_schema"](
                bundle, checkout, require_live_capture=True
            )

        self.assertEqual(rejected.exception.reason, "schema_rejected")

    def test_r70_recovery_requires_fresh_pinned_inert_capture_before_dump(self) -> None:
        gate = gate_namespace()
        for scenario in (
            "success",
            "missing-inert-capture",
            "inert-capture-outside-freeze",
            "wrong-inert-query-digest",
            "applied-capture-after-dump",
        ):
            with self.subTest(scenario=scenario):
                temp = tempfile.TemporaryDirectory(
                    prefix="rustfs-transition-r70-recovery.",
                    dir=os.environ.get("TMPDIR", "/root"),
                )
                self.addCleanup(temp.cleanup)
                bundle, checkout, _, _ = write_bundle(
                    Path(temp.name), release_set="60-70"
                )
                self.add_r70_live_captures(bundle)
                self.add_r70_recovery_capture(bundle, scenario)

                if scenario == "success":
                    applied = gate["validate_migration_schema"](
                        bundle, checkout, require_live_capture=True
                    )
                    self.assertEqual(applied, VERSIONS_60_70)
                    continue

                with self.assertRaises(gate["GateReject"]) as rejected:
                    gate["validate_migration_schema"](
                        bundle, checkout, require_live_capture=True
                    )

                self.assertEqual(rejected.exception.reason, "schema_rejected")

    def test_r70_rejects_r69_baselines_and_incomplete_applied_revisions(self) -> None:
        self.run_scenario("r70-baseline-60-69", "schema_rejected", release_set="60-70")
        self.run_scenario("r70-db-60-69", "schema_rejected", release_set="60-70")
        self.run_scenario("r70-70-present-false", "schema_rejected", release_set="60-70")
        self.run_scenario("r70-extra-71-row", "schema_rejected", release_set="60-70")
        for mutation in ("error", "error-stmt", "partial-hashes", "incomplete", "total", "hash", "missing"):
            with self.subTest(mutation=mutation):
                self.run_scenario(f"r70-revision-{mutation}", "schema_rejected", release_set="60-70")

    def test_r70_pins_migration_bytes_and_rejects_successor_migrations(self) -> None:
        for scenario in (
            "changed-70-migration-file",
            "tampered-atlas-sum",
            "extra-71-file",
        ):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "schema_rejected", release_set="60-70")

    def test_r70_requires_complete_in_freeze_baseline_and_applied_captures(self) -> None:
        for scenario in (
            "r70-baseline-capture-after-dump",
            "r70-baseline-capture-missing",
            "r70-applied-capture-outside-freeze",
            "r70-applied-capture-after-dump",
            "r70-applied-capture-before-baseline",
        ):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "schema_rejected", release_set="60-70")

    def test_r70_rejects_each_column_type_nullability_and_default_change(self) -> None:
        for table, columns in gate_constants("60-70")["r70_columns"].items():
            for column in columns:
                for field in ("type", "nullability", "default"):
                    with self.subTest(table=table, column=column, field=field):
                        self.run_scenario(
                            f"r70-column-{field}-{table}-{column}",
                            "schema_rejected",
                            release_set="60-70",
                        )
        for scenario in (
            "r70-sequence-default",
            "r70-identity-column",
            "r70-generated-column",
            "r70-sequence-column",
        ):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "schema_rejected", release_set="60-70")

    def test_r70_rejects_keys_foreign_keys_indexes_checks_and_triggers(self) -> None:
        for scenario in (
            "r70-primary-key-order",
            "r70-missing-unique",
            "r70-extra-index",
            "r70-invalid-index",
            "r70-completion-fk-missing",
            "r70-completion-fk-local-order",
            "r70-completion-fk-referenced-order",
            "r70-completion-fk-delete-action",
            "r70-completion-fk-update-action",
            "r70-completion-fk-match",
            "r70-completion-fk-referenced-index",
            "r70-completion-fk-unvalidated",
            "r70-completion-fk-deferrable",
            "r70-completion-fk-initially-deferred",
            "r70-extra-outbox-fk",
            "r70-files-fk",
            "r70-inbound-fk",
            "r70-extra-check",
            "r70-renamed-check",
            "r70-trigger-erasure_outbox",
            "r70-trigger-erasure_epoch",
            "r70-trigger-erasure_epoch_completion",
            "r70-extra-migrations-key",
            "r70-missing-migration-70-schema",
            "r70-extra-migration-70-schema-key",
        ):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "schema_rejected", release_set="60-70")

    def test_r70_requires_pinned_in_freeze_seven_surface_evidence(self) -> None:
        for scenario in (
            "r70-inert-query-hash",
            "r70-inert-query-missing",
            "r70-reference-extra-key",
            "r70-inert-capture-missing",
            "r70-inert-capture-outside-freeze",
        ):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "schema_rejected", release_set="60-70")
        for scenario in ("r70-inert-malformed", "r70-inert-missing", "r70-inert-extra"):
            with self.subTest(scenario=scenario):
                self.run_scenario(scenario, "schema_rejected", release_set="60-70")
        for table in gate_constants("60-70")["r70_inert_surfaces"]:
            with self.subTest(table=table):
                self.run_scenario(f"r70-inert-row-{table}", "reference_coverage", release_set="60-70")
        self.run_scenario("r70-file-row", "reference_coverage", release_set="60-70")

    def test_fixture_provenance_fails_before_bundle_construction(self) -> None:
        temp = tempfile.TemporaryDirectory(prefix="rustfs-transition-provenance.", dir=os.environ.get("TMPDIR", "/root"))
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        fixture_roots = {
            "60-66": FROZEN_MIGRATIONS,
            "60-67": FROZEN_MIGRATIONS_67,
            "60-69": FROZEN_MIGRATIONS_69,
            "60-70": FROZEN_MIGRATIONS_70,
        }
        for release_set, source_root in fixture_roots.items():
            migration_name = sorted(gate_constants(release_set)["migration_sha256"])[0]
            for scenario in ("missing", "altered", "unexpected"):
                with self.subTest(release_set=release_set, scenario=scenario):
                    scenario_root = root / release_set / scenario
                    fixture_root = scenario_root / "fixtures"
                    shutil.copytree(source_root, fixture_root)
                    if scenario == "missing":
                        (fixture_root / migration_name).unlink()
                    elif scenario == "altered":
                        path = fixture_root / migration_name
                        path.write_bytes(path.read_bytes() + b"-- altered fixture\n")
                    else:
                        (fixture_root / "unexpected.sql").write_text("SELECT 1;\n", encoding="utf-8")
                    attempt_root = scenario_root / "attempt"
                    with self.assertRaisesRegex(FixtureProvenanceError, "fixture_provenance_error"):
                        write_bundle(attempt_root, fixture_root=fixture_root, release_set=release_set)
                    self.assertFalse(attempt_root.exists())

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
