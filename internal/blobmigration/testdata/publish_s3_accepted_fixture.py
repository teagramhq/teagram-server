#!/usr/bin/env python3
"""Create a rollout-valid s3-accepted authority for migration integration tests."""

from __future__ import annotations

import hashlib
import importlib.util
import json
import pathlib
import sys
from unittest.mock import patch


repo_root = pathlib.Path(sys.argv[1]).resolve()
fixture_root = pathlib.Path(sys.argv[2]).resolve()
endpoint = sys.argv[3]
bucket = sys.argv[4]
fixture_path = repo_root / "deploy/teagramd/rollout-runner/test-blob-mode-state.py"
spec = importlib.util.spec_from_file_location("blob_mode_state_test_fixtures", fixture_path)
if spec is None or spec.loader is None:
    raise RuntimeError("could not load blob-mode-state test fixtures")
fixtures = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixtures)
fixture = fixtures.BlobModeStateTests("test_simple_s3_transition_accepts_current_migrations_and_one_verified_copy")
fixture.setUp()
state_dir, report_root, lock_path = fixture.create_initial_authority(fixture_root)
with (
    patch.object(fixtures.blob_mode, "require_runner_lock"),
    patch.object(fixtures.blob_mode, "docker_volume_exists"),
    patch.object(fixtures.blob_mode, "file_stat", side_effect=fixture.allow_unowned_private_files),
    patch.object(fixtures.blob_mode.os, "fchown"),
    patch.object(fixtures.blob_mode.os, "chown"),
):
    fixture.publish(state_dir, report_root, lock_path, fixture_root, "s3-accepted")

# The rollout fixture uses its production-like endpoint and bucket. Retarget its
# valid report and journal record to this test's local S3 endpoint.
record_path = state_dir / "journal" / "0000000002.json"
record = json.loads(record_path.read_text(encoding="utf-8"))
report_path = report_root / f"teagramd-blob-mode-report-{record['transition_id']}.json"
report = json.loads(report_path.read_text(encoding="utf-8"))
backend = {"kind": "s3", "endpoint": endpoint, "bucket": bucket, "prefix": "teagramd/"}
record["backend"] = backend
report["backend"] = backend
report_bytes = (json.dumps(report, sort_keys=True, separators=(",", ":")) + "\n").encode()
report_path.write_bytes(report_bytes)
report_path.chmod(0o600)
record["evidence"]["report_sha256"] = hashlib.sha256(report_bytes).hexdigest()
record_bytes = (json.dumps(record, sort_keys=True, separators=(",", ":")) + "\n").encode()
record_path.write_bytes(record_bytes)
record_path.chmod(0o644)
mode_path = state_dir / "mode.json"
mode_path.write_bytes(record_bytes)
mode_path.chmod(0o644)
