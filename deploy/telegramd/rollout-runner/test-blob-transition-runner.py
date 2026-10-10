#!/usr/bin/env python3
from __future__ import annotations

import importlib.util
import hashlib
import json
import os
import shlex
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from typing import Any
from unittest.mock import patch


SCRIPT_DIR = Path(__file__).resolve().parent
RUNNER = SCRIPT_DIR / "blob-transition-runner.py"
QUALIFIER_FIXTURES = SCRIPT_DIR / "test-qualify-rustfs-transition.py"
MODE_FIXTURES = SCRIPT_DIR / "test-blob-mode-state.py"
FILE_KEY = "02/258"
PART_KEY = "parts/aa/" + "b" * 32
MANIFEST = f"{FILE_KEY}\t5\t{'a' * 64}\n{PART_KEY}\t3\t{'b' * 64}\n"
MISMATCHED_MANIFEST = f"{FILE_KEY}\t5\t{'f' * 64}\n{PART_KEY}\t3\t{'b' * 64}\n"
LOCAL_ONLY_KEY = "03/259"
LOCAL_BEFORE = f"{FILE_KEY}\t5\t{'a' * 64}\n{LOCAL_ONLY_KEY}\t7\t{'e' * 64}\n{PART_KEY}\t3\t{'b' * 64}\n"
RESTORED_UNION = LOCAL_BEFORE


def import_from_path(name: str, path: Path) -> Any:
    spec = importlib.util.spec_from_file_location(name, path)
    if spec is None or spec.loader is None:
        raise AssertionError(f"cannot load {path.name}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


qualifier_fixtures = import_from_path("qualifier_fixtures", QUALIFIER_FIXTURES)
mode_fixtures = import_from_path("mode_fixtures", MODE_FIXTURES)


def live_schema_observation(release_set: str) -> dict[str, Any]:
    migration = qualifier_fixtures.good_migration_evidence(release_set)
    detail_keys = ["applied", "total", "error", "hash"]
    if release_set == "60-70":
        detail_keys.extend(("error_stmt_empty", "partial_hashes_empty"))
    observation = {
        "applied_revisions": migration["target_revisions"],
        "revision_detail": {
            version: {key: detail[key] for key in detail_keys}
            for version, detail in migration["revision_detail"].items()
        },
        "migration_66_schema": migration["migration_66_schema"],
    }
    if release_set in {"60-67", "60-69", "60-70"}:
        observation["migration_67_schema"] = migration["migration_67_schema"]
    return observation


class BlobTransitionEnvironmentTests(unittest.TestCase):
    def test_test_environment_passes_source_volume_mountpoint_to_docker(self) -> None:
        runner = import_from_path("blob_transition_runner", RUNNER)
        with (
            patch.object(runner, "TEST_MODE", True),
            patch.object(runner, "TEST_FIXTURE_DOCKER", Path("/fixture/docker")),
            patch.dict(os.environ, {"MOCK_SOURCE_VOLUME_MOUNTPOINT": "/host/source-volume"}, clear=True),
        ):
            environment = runner.compose_environment(Path("/checkout"))

        self.assertEqual(environment["MOCK_SOURCE_VOLUME_MOUNTPOINT"], "/host/source-volume")

    def test_r70_inert_capture_preserves_reference_coverage_rejection(self) -> None:
        runner = import_from_path("blob_transition_runner_inert_capture", RUNNER)
        with tempfile.TemporaryDirectory(prefix="r70-inert-capture.") as temporary:
            root = Path(temporary)
            bundle = root / "bundle"
            bundle.mkdir()
            (bundle / "migrations.json").write_text('{"release_set":"60-70"}\n', encoding="utf-8")
            output_dir = root / "output"
            output_dir.mkdir()
            observed = {
                name: name == "erasure_outbox"
                for name in qualifier_fixtures.gate_constants("60-70")["r70_inert_surfaces"]
            }

            def captured_query(*_args: Any, **_kwargs: Any) -> Path:
                output = output_dir / "inert-surfaces.json"
                output.write_text(json.dumps(observed), encoding="utf-8")
                return output

            with (
                patch.object(runner, "run_private_command", side_effect=captured_query),
                patch.object(runner, "replace_synced"),
            ):
                with self.assertRaises(runner.TransitionReject) as caught:
                    runner.capture_live_r70_inert_surfaces(
                        bundle, output_dir, root, {}, "fresh-recovery-r70-inert-surfaces"
                    )

        self.assertEqual(str(caught.exception), "reference_coverage")

    def test_r70_runner_rejects_nonempty_revision_state_in_baseline_and_applied_captures(self) -> None:
        runner = import_from_path("blob_transition_r70_revision_capture", RUNNER)
        good_observation = live_schema_observation("60-70")
        version = qualifier_fixtures.VERSIONS_60_70[-1]
        for stage, field in (
            ("baseline", "error_stmt_empty"),
            ("baseline", "partial_hashes_empty"),
            ("applied", "error_stmt_empty"),
            ("applied", "partial_hashes_empty"),
        ):
            with self.subTest(stage=stage, field=field), tempfile.TemporaryDirectory(
                prefix="r70-live-revision-capture."
            ) as temporary:
                root = Path(temporary)
                bundle, checkout, _, _ = qualifier_fixtures.write_bundle(
                    root / "fixture", release_set="60-70"
                )
                output_dir = root / "output"
                output_dir.mkdir(mode=0o700)
                query_output = root / "live-schema.json"

                query_output.write_text(
                    json.dumps(good_observation, sort_keys=True, separators=(",", ":")) + "\n",
                    encoding="utf-8",
                )
                query_output.chmod(0o600)
                with patch.object(runner, "run_private_command", return_value=query_output):
                    baseline = runner.capture_baseline_live_migration_schema(
                        bundle, output_dir, checkout, {}, "baseline-live-schema"
                    )

                bad_observation = json.loads(json.dumps(good_observation))
                bad_observation["revision_detail"][version][field] = False
                query_output.write_text(
                    json.dumps(bad_observation, sort_keys=True, separators=(",", ":")) + "\n",
                    encoding="utf-8",
                )
                query_output.chmod(0o600)
                with patch.object(runner, "run_private_command", return_value=query_output):
                    if stage == "baseline":
                        with self.assertRaises(runner.TransitionReject) as caught:
                            runner.capture_baseline_live_migration_schema(
                                bundle, output_dir, checkout, {}, "baseline-live-schema"
                            )
                        self.assertEqual(str(caught.exception), "baseline-live-schema-rejected")
                    else:
                        dump_sha256 = hashlib.sha256((bundle / "postgres.dump").read_bytes()).hexdigest()
                        with self.assertRaises(runner.TransitionReject) as caught:
                            runner.capture_live_migration_schema(
                                bundle,
                                output_dir,
                                checkout,
                                {},
                                dump_sha256,
                                "applied-live-schema",
                                baseline,
                            )
                        self.assertEqual(str(caught.exception), "live-schema-rejected")


class BlobTransitionEvidenceTests(unittest.TestCase):
    def schema_document(self, release_set: str = "60-66") -> tuple[Any, dict[str, Any], dict[str, str]]:
        mode = import_from_path("blob_transition_state", SCRIPT_DIR / "blob-mode-state.py")
        qualifier = import_from_path("transition_evidence_qualifier", SCRIPT_DIR / "qualify-rustfs-transition.py")
        migration = qualifier_fixtures.good_migration_evidence(release_set)
        observed = live_schema_observation(release_set)
        dump_sha256 = "d" * 64
        capture = {
            "schema": "teagram.live-migration-schema/v1",
            "captured_at": "2026-10-08T01:04:00Z",
            "dump_sha256": dump_sha256,
            "query_sha256": qualifier.live_schema_query_sha256(release_set),
            "query_output_sha256": "e" * 64,
            "observed": observed,
        }
        document = {
            **migration,
            "live_capture": capture,
        }
        if release_set in {"60-67", "60-69", "60-70"}:
            document["baseline_live_capture"] = {
                **capture,
                "captured_at": "2026-10-08T01:03:00Z",
                "query_output_sha256": "f" * 64,
            }
        return mode, document, {"dump_sha256": dump_sha256}

    def validate(self, mode: Any, document: dict[str, Any], phase_digests: dict[str, str]) -> None:
        with patch.object(mode, "read_json", return_value=document):
            mode.validate_live_schema_dump_binding(
                {"schema_evidence_sha256": Path("migrations.json")}, phase_digests
            )

    def test_publisher_accepts_complete_live_atlas_revision_evidence(self) -> None:
        mode, document, phase_digests = self.schema_document()

        self.validate(mode, document, phase_digests)

    def test_publisher_accepts_complete_r67_live_atlas_revision_evidence(self) -> None:
        mode, document, phase_digests = self.schema_document("60-67")

        self.validate(mode, document, phase_digests)

    def test_publisher_accepts_complete_r69_and_r70_live_atlas_revision_evidence(self) -> None:
        for release_set in ("60-69", "60-70"):
            with self.subTest(release_set=release_set):
                mode, document, phase_digests = self.schema_document(release_set)
                self.validate(mode, document, phase_digests)

    def test_publisher_preserves_legacy_live_query_digest_for_pre_r70_reports(self) -> None:
        mode, document, phase_digests = self.schema_document("60-69")
        for capture_name in ("baseline_live_capture", "live_capture"):
            document[capture_name]["query_sha256"] = qualifier_fixtures.gate_constants(
                "60-69"
            )["legacy_live_schema_query_sha256"]

        self.validate(mode, document, phase_digests)

    def test_publisher_rejects_legacy_live_query_digest_for_r70_reports(self) -> None:
        mode, document, phase_digests = self.schema_document("60-70")
        for capture_name in ("baseline_live_capture", "live_capture"):
            document[capture_name]["query_sha256"] = "f0458b327ab900e9d5eaa4fc4e702149520163bafbdb58a2fd3e9ab53bc7b363"

        with self.assertRaises(mode.Reject):
            self.validate(mode, document, phase_digests)

    def test_publisher_rejects_nonempty_r70_live_revision_state_for_all_rows(self) -> None:
        for version in qualifier_fixtures.VERSIONS_60_70:
            for field in ("error_stmt_empty", "partial_hashes_empty"):
                with self.subTest(version=version, field=field):
                    mode, document, phase_digests = self.schema_document("60-70")
                    for capture_name in ("baseline_live_capture", "live_capture"):
                        document[capture_name]["observed"]["revision_detail"][version][field] = False
                    with self.assertRaises(mode.Reject):
                        self.validate(mode, document, phase_digests)

    def test_publisher_rejects_invalid_r70_baseline_capture(self) -> None:
        for field, value in (
            ("dump_sha256", "0" * 64),
            ("query_sha256", "0" * 64),
            ("observed", {}),
            ("captured_at", "2026-10-08T01:05:00Z"),
        ):
            with self.subTest(field=field):
                mode, document, phase_digests = self.schema_document("60-70")
                self.validate(mode, document, phase_digests)
                document["baseline_live_capture"][field] = value
                with self.assertRaises(mode.Reject):
                    self.validate(mode, document, phase_digests)

        mode, document, phase_digests = self.schema_document("60-70")
        self.validate(mode, document, phase_digests)
        del document["baseline_live_capture"]
        with self.assertRaises(mode.Reject):
            self.validate(mode, document, phase_digests)

    def test_publisher_rejects_incomplete_or_failed_live_atlas_revision(self) -> None:
        version = qualifier_fixtures.VERSIONS_60_66[-1]
        for field, value in (
            ("applied", 0),
            ("total", 2),
            ("error", "migration failed"),
            ("hash", "h1:stale"),
        ):
            with self.subTest(field=field):
                mode, document, phase_digests = self.schema_document()
                document["live_capture"]["observed"]["revision_detail"][version][field] = value

                with self.assertRaises(mode.Reject):
                    self.validate(mode, document, phase_digests)


def write_fake_docker(
    bin_dir: Path,
    state: Path,
    events: Path,
    scenario: str,
    candidate_compose: Path,
) -> None:
    script = f"""#!/usr/bin/env bash
set -eu
printf 'docker' >> {shlex.quote(str(events))}
for arg in "$@"; do printf ' %q' "$arg" >> {shlex.quote(str(events))}; done
printf '\\n' >> {shlex.quote(str(events))}
if [ "${{1:-}}" = compose ]; then
  printf 'compose-env=%s\\n' "${{COMPOSE_FILE:-unset}}" >> {shlex.quote(str(events))}
  shift
  if [[ " $* " == *" config "* ]]; then
    if [[ "${{COMPOSE_FILE:-}}" == *docker-compose.local-blobs.yml* ]]; then
      cat "${{MOCK_LOCAL_COMPOSE_JSON:?}}"
      exit 0
    fi
    cat {shlex.quote(str(candidate_compose))}
    exit 0
  fi
  if [[ " $* " == *" pg_dump "* ]]; then cat "${{MOCK_POSTGRES_DUMP:?}}"; exit 0; fi
  if [[ " $* " == *" psql "* ]]; then
    if [[ " $* " == *"atlas_schema_revisions"* ]]; then cat "${{MOCK_LIVE_SCHEMA:?}}"
    elif [[ " $* " == *"erasure_epoch_completion"* ]]; then cat "${{MOCK_LIVE_R70_INERT_SURFACES:?}}"
    elif [[ " $* " == *"FROM files AS f"* ]]; then cat "${{MOCK_REFERENCE_BUNDLE:?}}/references.tsv"
    else cat "${{MOCK_REFERENCE_BUNDLE:?}}/active-links.tsv"; fi
    exit 0
  fi
  case "${{1:-}}" in
    up)
      if [[ " $* " == *" rustfs "* ]]; then printf '%s\\n' running > {shlex.quote(str(state / 'rustfs'))}; fi
      if [[ " $* " == *" telegramd "* ]]; then
        if [ {shlex.quote(scenario)} = post-publication-serving-not-ready ] \\
          && [[ "${{COMPOSE_FILE:-}}" != *docker-compose.local-blobs.yml* ]]; then
          exit 0
        fi
        printf '%s\\n' running > {shlex.quote(str(state / 'serving'))}
        if [[ "${{COMPOSE_FILE:-}}" == *docker-compose.local-blobs.yml* ]]; then
          printf '%s\\n' local > {shlex.quote(str(state / 'serving-backend'))}
        else
          printf '%s\\n' s3 > {shlex.quote(str(state / 'serving-backend'))}
        fi
      fi
      exit 0
      ;;
    stop)
      if [[ " $* " == *" rustfs "* ]]; then rm -f -- {shlex.quote(str(state / 'rustfs'))}; fi
      if [[ " $* " == *" telegramd "* ]]; then rm -f -- {shlex.quote(str(state / 'serving'))} {shlex.quote(str(state / 'serving-backend'))}; fi
      exit 0
      ;;
    run)
      if [[ " $* " == *" rustfs-init "* ]]; then exit 0; fi
      if [[ " $* " == *" blob-migrate "* ]]; then
        if [[ " $* " == *" local-census "* ]]; then
          cat "${{MOCK_REFERENCE_BUNDLE:?}}/source-frozen.tsv"
          exit 0
        fi
        if [[ " $* " == *" s3-census "* ]]; then printf '%s' {shlex.quote(MANIFEST)}; exit 0; fi
        count=0
        [ ! -f {shlex.quote(str(state / 'copy-count'))} ] || count=$(cat {shlex.quote(str(state / 'copy-count'))})
        count=$((count + 1))
        printf '%s\\n' "$count" > {shlex.quote(str(state / 'copy-count'))}
        if [ {shlex.quote(scenario)} = mismatched-second-copy ] && [ "$count" -eq 2 ]; then
          printf '%s' {shlex.quote(MISMATCHED_MANIFEST)}
        else
          printf '%s' {shlex.quote(MANIFEST)}
        fi
        exit 0
      fi
      if [[ " $* " == *" blob-restore "* ]]; then
        if [[ " $* " == *" local-census "* ]]; then
          if [ -f {shlex.quote(str(state / 'restore-count'))} ]; then
            printf '%s' {shlex.quote(RESTORED_UNION)}
          else
            printf '%s' {shlex.quote(LOCAL_BEFORE)}
          fi
          exit 0
        fi
        if [[ " $* " == *" s3-to-local "* ]]; then
          count=0
          [ ! -f {shlex.quote(str(state / 'restore-count'))} ] || count=$(cat {shlex.quote(str(state / 'restore-count'))})
          count=$((count + 1))
          if [ {shlex.quote(scenario)} = failed-second-restore ] && [ "$count" -eq 2 ]; then exit 91; fi
          printf '%s\\n' "$count" > {shlex.quote(str(state / 'restore-count'))}
          printf '%s' {shlex.quote(MANIFEST)}
          exit 0
        fi
      fi
      ;;
  esac
fi
if [ "${{1:-}}" = inspect ]; then
  cat "${{MOCK_FROZEN_INSPECT:?}}"
  exit 0
fi
if [ "${{1:-}}" = volume ] && [ "${{2:-}}" = inspect ]; then
  printf '%s\\n' "${{MOCK_SOURCE_VOLUME_MOUNTPOINT:?}}"
  exit 0
fi
if [ "${{1:-}}" = ps ]; then
  if [[ " $* " == *"{{.ID}}"* ]]; then
    cat "${{MOCK_FROZEN_PS:?}}"
    exit 0
  fi
  if [ "${{MOCK_WRITER_RUNNING:-0}}" = 1 ]; then printf 'postgres\\ntelegramd\\ntelegramd-proxy\\n'
  elif [ -f {shlex.quote(str(state / 'serving'))} ]; then
    if [ -f {shlex.quote(str(state / 'rustfs'))} ]; then
      printf 'postgres\\nrustfs\\ntelegramd\\ntelegramd-proxy\\n'
    else
      printf 'postgres\\ntelegramd\\ntelegramd-proxy\\n'
    fi
  elif [ -f {shlex.quote(str(state / 'rustfs'))} ]; then printf 'postgres\\nrustfs\\n'
  else printf 'postgres\\n'; fi
  exit 0
fi
printf 'unexpected docker fixture command\\n' >&2
exit 90
"""
    executable = bin_dir / "docker"
    executable.write_text(script, encoding="utf-8")
    executable.chmod(0o700)


class BlobTransitionRunnerFixtures(unittest.TestCase):
    def setUp(self) -> None:
        if os.geteuid() != 0:
            self.skipTest("transition runner fixtures require root ownership checks")
        self.temp = tempfile.TemporaryDirectory(prefix="blob-transition-runner.", dir=os.environ.get("TMPDIR", "/root"))
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.root.chmod(0o700)
        self.report_root = self.root / "reports"
        self.authority_case = mode_fixtures.BlobModeStateTests(methodName="runTest")
        self.authority_case.setUp()
        self.state_dir, authority_report_root, self.lock_path = self.authority_case.create_initial_authority(self.root)
        self.assertEqual(authority_report_root, self.report_root)
        self.lock_path.write_bytes(b"")
        self.lock_path.chmod(0o600)
        self.bundle, self.checkout, self.mock_bin, events_text = qualifier_fixtures.write_bundle(self.report_root)
        self.decoy_bin = self.root / "decoy-bin"
        self.decoy_bin.mkdir(mode=0o700)
        self.decoy_called = self.root / "decoy-called"
        decoy = self.decoy_bin / "docker"
        decoy.write_text(
            f"#!/usr/bin/env bash\nprintf called > {shlex.quote(str(self.decoy_called))}\nexit 88\n",
            encoding="utf-8",
        )
        decoy.chmod(0o700)
        self.live_dump_path = self.root / "live-postgres.dump"
        self.live_dump_path.write_bytes((self.bundle / "postgres.dump").read_bytes())
        self.live_dump_path.chmod(0o600)
        self.live_schema_path = self.root / "live-migration-schema.json"
        self.live_r70_inert_path = self.root / "live-r70-inert-surfaces.json"
        self.live_r70_inert_path.write_text(
            json.dumps(
                {
                    name: False
                    for name in qualifier_fixtures.gate_constants("60-70")["r70_inert_surfaces"]
                },
                sort_keys=True,
                separators=(",", ":"),
            ) + "\n",
            encoding="utf-8",
        )
        self.live_r70_inert_path.chmod(0o600)
        migration_evidence = qualifier_fixtures.good_migration_evidence()
        self.write_live_schema_fixture({
            "applied_revisions": migration_evidence["target_revisions"],
            "revision_detail": migration_evidence["revision_detail"],
            "migration_66_schema": migration_evidence["migration_66_schema"],
        })
        self.live_evidence = self.root / "live-evidence"
        self.live_evidence.mkdir(mode=0o700)
        for name in ("references.tsv", "active-links.tsv", "source-frozen.tsv"):
            target = self.live_evidence / name
            target.write_bytes((self.bundle / name).read_bytes())
            target.chmod(0o600)
        for name in (
            "copy-pass-1.tsv",
            "copy-pass-2.tsv",
            "destination-census-pass-1.tsv",
            "destination-census-pass-2.tsv",
        ):
            (self.bundle / name).unlink()
        self.events = Path(events_text)
        self.local_compose_json = self.mock_bin / "local-compose.json"
        (self.checkout / "docker-compose.local-blobs.yml").write_text("services: {}\n", encoding="utf-8")
        (self.checkout / "docker-compose.local-blobs.yml").chmod(0o600)
        write_fake_docker(self.mock_bin, self.root, self.events, "success", self.bundle / "candidate-compose.json")
        self.write_local_compose_fixture()
        (self.root / "serving").write_text("running\n", encoding="ascii")
        (self.root / "serving-backend").write_text("local\n", encoding="ascii")

    def run_action(
        self,
        action: str,
        scenario: str = "success",
        seed_running_writer: bool = False,
        bundle: Path | None = None,
        interrupt_after: str | None = None,
        put_decoy_first: bool = False,
        host_container_case: str | None = None,
    ) -> subprocess.CompletedProcess[str]:
        selected_bundle = bundle or self.bundle
        frozen_doc = json.loads((selected_bundle / "frozen-containers.json").read_text(encoding="utf-8"))
        candidate_compose = json.loads((self.bundle / "candidate-compose.json").read_text(encoding="utf-8"))
        project = candidate_compose["name"]
        ps_lines = []
        inspected = []
        for container in frozen_doc["containers"]:
            ps_lines.append(f"{container['id']}\t{project}\t{container['service']}\n")
            ports: dict[str, list[dict[str, str]]] = {}
            for port in container.get("ports", []):
                key = f"{port['target']}/{port.get('protocol', 'tcp')}"
                ports.setdefault(key, []).append({
                    "HostIp": port.get("host_ip", ""),
                    "HostPort": str(port.get("published", "")),
                })
            inspected.append({
                "Id": container["id"],
                "Name": container["name"],
                "State": {"Status": "running"},
                "Config": {
                    "Labels": {
                        "com.docker.compose.project": project,
                        "com.docker.compose.service": container["service"],
                    },
                    "Env": [f"{key}={value}" for key, value in container["environment"].items()],
                },
                "Mounts": [
                    {
                        "Type": mount["type"],
                        "Name": mount["source"] if mount["type"] == "volume" else "",
                        "Source": mount["source"] if mount["type"] == "bind" else "",
                        "Destination": mount["target"],
                        "RW": mount["rw"],
                    }
                    for mount in container["mounts"]
                ],
                "NetworkSettings": {"Ports": ports},
            })
        if seed_running_writer and host_container_case is None:
            host_container_case = "same-project-writer"
        if host_container_case is not None:
            host_project = "another-compose-project"
            host_service = ""
            labels: dict[str, str] = {}
            mounts = []
            if host_container_case in ("same-project-writer", "cross-project-writer"):
                host_service = "telegramd-sidecar"
                host_project = project if host_container_case == "same-project-writer" else host_project
                labels = {
                    "com.docker.compose.project": host_project,
                    "com.docker.compose.service": host_service,
                }
            elif host_container_case == "unlabelled-source-volume-writer":
                mounts = [{
                    "Type": "volume",
                    "Name": "telegram-server_tgblobs",
                    "Source": "",
                    "Destination": "/var/lib/telegramd-blobs",
                    "RW": True,
                }]
            elif host_container_case == "unlabelled-source-bind-writer":
                mounts = [{
                    "Type": "bind",
                    "Name": "",
                    "Source": "/var/lib/docker/volumes/telegram-server_tgblobs/_data",
                    "Destination": "/var/lib/telegramd-blobs",
                    "RW": True,
                }]
            else:
                raise AssertionError(f"unknown host container case: {host_container_case}")
            inspected.append({
                "Id": "d" * 64,
                "Name": "/foreign-writer",
                "State": {"Status": "running"},
                "Config": {"Labels": labels, "Env": []},
                "Mounts": mounts,
                "NetworkSettings": {"Ports": {}},
            })
            ps_lines.append(f"{'d' * 64}\t{host_project if labels else ''}\t{host_service}\n")
        frozen_ps = self.root / "frozen-ps.tsv"
        frozen_ps.write_text("".join(ps_lines), encoding="utf-8")
        frozen_ps.chmod(0o600)
        frozen_inspect = self.root / "frozen-inspect.json"
        frozen_inspect.write_text(json.dumps(inspected, sort_keys=True), encoding="utf-8")
        frozen_inspect.chmod(0o600)
        write_fake_docker(self.mock_bin, self.root, self.events, scenario, self.bundle / "candidate-compose.json")
        environment = os.environ.copy()
        if put_decoy_first:
            environment["PATH"] = f"{self.decoy_bin}:/usr/bin:/bin"
        else:
            environment["PATH"] = f"{self.mock_bin}:{environment['PATH']}"
        environment["BLOB_TRANSITION_TEST_MODE"] = "1"
        environment["BLOB_TRANSITION_TEST_FIXTURE_ROOT"] = str(self.root)
        environment["BLOB_TRANSITION_TEST_FIXTURE_DOCKER"] = str(self.mock_bin / "docker")
        environment["MOCK_REFERENCE_BUNDLE"] = str(self.live_evidence)
        environment["MOCK_LIVE_SCHEMA"] = str(self.live_schema_path)
        environment["MOCK_LIVE_R70_INERT_SURFACES"] = str(self.live_r70_inert_path)
        environment["MOCK_FROZEN_PS"] = str(frozen_ps)
        environment["MOCK_FROZEN_INSPECT"] = str(frozen_inspect)
        environment["MOCK_SOURCE_VOLUME_MOUNTPOINT"] = (
            "/var/lib/docker/volumes/telegram-server_tgblobs/_data"
        )
        if interrupt_after is not None:
            environment["BLOB_TRANSITION_TEST_INTERRUPT_AFTER"] = interrupt_after
        if self.local_compose_json.exists():
            environment["MOCK_LOCAL_COMPOSE_JSON"] = str(self.local_compose_json)
        if seed_running_writer:
            environment["MOCK_WRITER_RUNNING"] = "1"
        environment["MOCK_POSTGRES_DUMP"] = str(self.live_dump_path)
        return subprocess.run(
            [
                "python3",
                str(RUNNER),
                action,
                "--bundle",
                str(selected_bundle),
                "--checkout",
                str(self.checkout),
                "--state-dir",
                str(self.state_dir),
                "--report-root",
                str(self.report_root),
                "--lock-path",
                str(self.lock_path),
            ],
            env=environment,
            text=True,
            capture_output=True,
            check=False,
        )

    def write_live_schema_fixture(self, observation: dict[str, Any]) -> None:
        self.live_schema_path.write_text(
            json.dumps(observation, sort_keys=True, separators=(",", ":")) + "\n",
            encoding="utf-8",
        )
        self.live_schema_path.chmod(0o600)

    def run_runner(
        self,
        scenario: str = "success",
        seed_running_writer: bool = False,
        interrupt_after: str | None = None,
        put_decoy_first: bool = False,
        host_container_case: str | None = None,
    ) -> subprocess.CompletedProcess[str]:
        return self.run_action(
            "accept-s3", scenario, seed_running_writer,
            interrupt_after=interrupt_after, put_decoy_first=put_decoy_first,
            host_container_case=host_container_case,
        )

    def write_local_compose_fixture(self) -> None:
        model = json.loads((self.bundle / "candidate-compose.json").read_text(encoding="utf-8"))
        for name, service in model["services"].items():
            if not name.startswith("telegramd"):
                continue
            environment = service["environment"]
            for key in (
                "TG_BLOB_S3_ENDPOINT",
                "TG_BLOB_S3_BUCKET",
                "TG_BLOB_S3_PREFIX",
                "TG_BLOB_S3_REGION",
                "TG_BLOB_S3_ACCESS_KEY_ID",
                "TG_BLOB_S3_SECRET_ACCESS_KEY",
                "TG_BLOB_S3_SECRET_ACCESS_KEY_FILE",
                "TG_BLOB_S3_CA_PATH",
                "TG_BLOB_S3_ALLOW_INSECURE_HTTP",
            ):
                environment[key] = ""
            for mount in service["volumes"]:
                if mount.get("target") == "/var/lib/telegramd-blobs":
                    mount["read_only"] = False
        self.local_compose_json.write_text(json.dumps(model, sort_keys=True), encoding="utf-8")
        self.local_compose_json.chmod(0o600)

    def write_recovery_bundle(self, release_set: str = "60-66") -> Path:
        bundle = self.report_root / "recovery-bundle"
        bundle.mkdir(mode=0o700)
        dump = self.live_dump_path.read_bytes()
        (bundle / "postgres.dump").write_bytes(dump)
        (bundle / "postgres.dump").chmod(0o600)
        frozen = {
            "complete": True,
            "captured_at": "2026-10-08T01:04:00Z",
            "containers": [
                {
                    "service": "postgres", "id": "b" * 64, "name": "/postgres-1",
                    "running": True,
                    "mounts": [{
                        "type": "volume", "source": "telegram-server_pgdata",
                        "target": "/var/lib/postgresql/data", "rw": True,
                    }],
                    "environment": {}, "ports": [],
                },
                {
                    "service": "rustfs", "id": "c" * 64, "name": "/rustfs-1",
                    "running": True,
                    "mounts": [{
                        "type": "volume", "source": "telegram-server_rustfsdata",
                        "target": "/data", "rw": True,
                    }],
                    "environment": {}, "ports": [],
                },
            ],
        }
        migrations = (
            qualifier_fixtures.good_migration_evidence(release_set)
            if release_set != "60-66"
            else json.loads((self.bundle / "migrations.json").read_text(encoding="utf-8"))
        )
        (bundle / "frozen-containers.json").write_text(
            json.dumps(frozen, sort_keys=True, separators=(",", ":")), encoding="utf-8"
        )
        (bundle / "frozen-containers.json").chmod(0o600)
        (bundle / "migrations.json").write_text(
            json.dumps(migrations, sort_keys=True, separators=(",", ":")), encoding="utf-8"
        )
        (bundle / "migrations.json").chmod(0o600)
        recovery = {
            "schema": "teagram.blob-recovery-qualification/v1",
            "source_volume": "telegram-server_tgblobs",
            "freeze": {
                "held": True,
                "inventory_complete": True,
                "started_at": "2026-10-08T01:00:00Z",
                "held_at": "2026-10-08T01:05:00Z",
            },
            "dump": {
                "captured_at": "2026-10-08T01:03:00Z",
                "exit_status": 0,
                "completion_marker": True,
                "isolated_restore_exit_status": 0,
                "isolated_restore_network": "none",
                "sha256": hashlib.sha256(dump).hexdigest(),
            },
            "schema_evidence_sha256": hashlib.sha256((bundle / "migrations.json").read_bytes()).hexdigest(),
        }
        (bundle / "recovery.json").write_text(
            json.dumps(recovery, sort_keys=True, separators=(",", ":")), encoding="utf-8"
        )
        (bundle / "recovery.json").chmod(0o600)
        return bundle

    def configure_recovery_release_fixture(self, release_set: str) -> None:
        if release_set not in {"60-67", "60-69", "60-70"}:
            raise AssertionError(f"unsupported recovery release fixture: {release_set}")
        source_bundle, source_checkout, _, _ = qualifier_fixtures.write_bundle(
            self.report_root / f"fixture-{release_set}", release_set=release_set
        )
        if release_set == "60-70":
            for name in ("references.tsv", "active-links.tsv"):
                destination = self.live_evidence / name
                shutil.copyfile(source_bundle / name, destination)
                destination.chmod(0o600)
        migrations_dir = self.checkout / "migrations"
        for path in migrations_dir.iterdir():
            if path.name == "atlas.sum" or path.name[:14] >= "20261005000060":
                path.unlink()
        for source in (source_checkout / "migrations").iterdir():
            if source.name == "atlas.sum" or source.name[:14] >= "20261005000060":
                destination = migrations_dir / source.name
                shutil.copyfile(source, destination)
                destination.chmod(0o600)
        observation = live_schema_observation(release_set)
        self.write_live_schema_fixture(observation)

    def seed_s3_authority_and_running_stack(self, release_set: str = "60-66") -> Path:
        result = self.run_runner()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.events.write_text("", encoding="utf-8")
        self.write_local_compose_fixture()
        if release_set != "60-66":
            self.configure_recovery_release_fixture(release_set)
        return self.write_recovery_bundle(release_set)

    def state_snapshot(self) -> dict[Path, bytes]:
        return {
            path.relative_to(self.state_dir): path.read_bytes()
            for path in self.state_dir.rglob("*")
            if path.is_file()
        }

    def test_cutover_qualifies_before_copy_and_publishes_before_serving(self) -> None:
        result = self.run_runner()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("gate_result=pre-copy-pass", result.stdout)
        self.assertIn("gate_result=pass", result.stdout)
        self.assertIn("transition=accepted outcome=s3-accepted", result.stdout)
        lines = self.events.read_text(encoding="utf-8").splitlines()
        freeze = next(index for index, line in enumerate(lines) if "compose stop --timeout 120 telegramd telegramd-proxy" in line)
        source_volume_inspect = next(index for index, line in enumerate(lines) if "docker volume inspect --format" in line)
        fresh_dump = next(index for index, line in enumerate(lines) if "compose exec -T postgres pg_dump" in line)
        storage_start = next(index for index, line in enumerate(lines) if "compose up -d --wait rustfs" in line)
        first_copy = next(index for index, line in enumerate(lines) if "compose run --rm --no-deps blob-migrate --manifest stdout" in line)
        activation = next(index for index, line in enumerate(lines) if "compose up -d --no-deps telegramd telegramd-proxy" in line)
        self.assertLess(freeze, fresh_dump)
        self.assertLess(freeze, source_volume_inspect)
        self.assertLess(source_volume_inspect, fresh_dump)
        self.assertLess(fresh_dump, storage_start)
        self.assertLess(storage_start, first_copy)
        self.assertLess(first_copy, activation)
        records, _head, _mode = mode_fixtures.blob_mode.read_authority(self.state_dir, self.report_root)
        self.assertEqual(records[-1]["outcome"], "s3-accepted")
        report = mode_fixtures.blob_mode.read_json(
            mode_fixtures.blob_mode.report_path(self.report_root, records[-1]["transition_id"])
        )
        phase = report["phase_files"]["qualification_output_sha256"]
        qualification = (self.report_root / phase).read_text(encoding="ascii")
        self.assertIn("gate_result=pass", qualification)
        schema_path = self.report_root / report["phase_files"]["schema_evidence_sha256"]
        schema_evidence = json.loads(schema_path.read_text(encoding="utf-8"))
        live_capture = schema_evidence["live_capture"]
        qualifier = import_from_path("runner_qualifier", SCRIPT_DIR / "qualify-rustfs-transition.py")
        self.assertEqual(live_capture["dump_sha256"], report["phase_digests"]["dump_sha256"])
        self.assertEqual(
            live_capture["observed"]["revision_detail"],
            qualifier_fixtures.good_migration_evidence()["revision_detail"],
        )
        self.assertEqual(
            live_capture["query_sha256"],
            qualifier.live_schema_query_sha256("60-66"),
        )

    def test_post_publication_serving_failure_keeps_s3_authority_and_volumes(self) -> None:
        result = self.run_runner("post-publication-serving-not-ready")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("s3-serving-not-ready", result.stderr)
        self.assertTrue((self.root / "rustfs").is_file())
        self.assertFalse((self.root / "serving").exists())

        lines = self.events.read_text(encoding="utf-8").splitlines()
        activation = next(
            index for index, line in enumerate(lines)
            if "compose up -d --no-deps telegramd telegramd-proxy" in line
        )
        serving_check = next(
            index for index, line in enumerate(lines[activation + 1 :], start=activation + 1)
            if "compose ps --status running --services" in line
        )
        self.assertLess(activation, serving_check)
        self.assertFalse(any("compose stop rustfs" in line for line in lines))
        local_serving_starts = []
        for index, line in enumerate(lines[:-1]):
            if "compose up -d --no-deps telegramd telegramd-proxy" not in line:
                continue
            if (
                lines[index + 1].startswith("compose-env=")
                and "docker-compose.local-blobs.yml" in lines[index + 1]
            ):
                local_serving_starts.append(line)
        self.assertEqual(local_serving_starts, [])
        self.assertFalse(
            any("docker volume rm " in line or "docker volume prune" in line for line in lines)
        )

        records, head, mode_bytes = mode_fixtures.blob_mode.read_authority(self.state_dir, self.report_root)
        self.assertEqual(mode_bytes, head)
        self.assertEqual([record["outcome"] for record in records], ["initial-local", "s3-accepted"])
        snapshot = self.state_snapshot()
        self.assertEqual(head, snapshot[Path("journal/0000000002.json")])
        self.assertEqual(
            set(snapshot),
            {
                Path("mode.json"),
                Path("journal/0000000001.json"),
                Path("journal/0000000002.json"),
            },
        )
        self.assertEqual(snapshot[Path("mode.json")], head)
        candidate = json.loads((self.bundle / "candidate-compose.json").read_text(encoding="utf-8"))
        self.assertEqual(records[-1]["volumes"]["tgblobs"], "telegram-server_tgblobs")
        self.assertEqual(
            records[-1]["volumes"]["rustfsdata"],
            candidate["volumes"]["rustfsdata"]["name"],
        )

    def test_cutover_live_schema_mismatch_keeps_local_authority(self) -> None:
        before = self.state_snapshot()
        observed_schema = qualifier_fixtures.new_unread_mark_schema()
        observed_schema["columns"]["peer_id"]["type"] = "integer"
        self.write_live_schema_fixture({
            "applied_revisions": qualifier_fixtures.VERSIONS_60_66,
            "revision_detail": qualifier_fixtures.good_migration_evidence()["revision_detail"],
            "migration_66_schema": observed_schema,
        })
        result = self.run_runner()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("live-schema-rejected", result.stderr)
        self.assertEqual((self.root / "serving-backend").read_text(encoding="utf-8").strip(), "local")
        self.assertEqual(self.state_snapshot(), before)
        self.assertNotIn("compose up -d --wait rustfs", self.events.read_text(encoding="utf-8"))

    def test_cutover_incomplete_live_atlas_revision_rejects_before_rustfs(self) -> None:
        before = self.state_snapshot()
        migration_evidence = qualifier_fixtures.good_migration_evidence()
        observation = {
            "applied_revisions": migration_evidence["target_revisions"],
            "revision_detail": migration_evidence["revision_detail"],
            "migration_66_schema": migration_evidence["migration_66_schema"],
        }
        observation["revision_detail"][qualifier_fixtures.VERSIONS_60_66[-1]]["applied"] = 0
        self.write_live_schema_fixture(observation)
        result = self.run_runner()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("live-schema-rejected", result.stderr)
        self.assertEqual((self.root / "serving-backend").read_text(encoding="utf-8").strip(), "local")
        self.assertEqual(self.state_snapshot(), before)
        self.assertNotIn("compose up -d --wait rustfs", self.events.read_text(encoding="utf-8"))

    def test_second_copy_mismatch_keeps_local_authority_and_never_starts_telegramd(self) -> None:
        before = self.state_snapshot()
        result = self.run_runner("mismatched-second-copy")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("qualification-check-rejected", result.stderr)
        self.assertEqual((self.root / "serving-backend").read_text(encoding="utf-8").strip(), "local")
        self.assertEqual(self.state_snapshot(), before)
        records, _head, _mode = mode_fixtures.blob_mode.read_authority(self.state_dir, self.report_root)
        self.assertEqual(records[-1]["outcome"], "initial-local")
        self.assertFalse((self.root / "rustfs").exists())
        lines = self.events.read_text(encoding="utf-8").splitlines()
        rustfs_stops = [index for index, line in enumerate(lines) if "compose stop rustfs" in line]
        local_resume = next(
            index for index, line in enumerate(lines)
            if "compose up -d --no-deps telegramd telegramd-proxy" in line
        )
        self.assertTrue(rustfs_stops)
        self.assertLess(rustfs_stops[-1], local_resume)

    def test_interruption_after_second_copy_keeps_authority_and_retry_uses_fresh_passes(self) -> None:
        before = self.state_snapshot()
        interrupted = self.run_runner(interrupt_after="copy-pass-2")
        self.assertEqual(interrupted.returncode, 86)
        self.assertEqual(self.state_snapshot(), before)
        self.assertFalse((self.bundle / "copy-pass-1.tsv").exists())
        self.assertFalse((self.bundle / "copy-pass-2.tsv").exists())
        attempts = sorted(self.report_root.glob(".transition-*"))
        self.assertEqual(len(attempts), 1)
        first_attempt = attempts[0] / "bundle"
        self.assertTrue((first_attempt / "copy-pass-1.tsv").is_file())
        self.assertTrue((first_attempt / "copy-pass-2.tsv").is_file())

        retried = self.run_runner()
        self.assertEqual(retried.returncode, 0, retried.stderr)
        attempts = sorted(self.report_root.glob(".transition-*"))
        self.assertEqual(len(attempts), 2)
        second_attempt_dir = next(path for path in attempts if path.name != first_attempt.parent.name)
        self.assertNotEqual(first_attempt.parent.name, second_attempt_dir.name)
        second_attempt = second_attempt_dir / "bundle"
        self.assertTrue((second_attempt / "copy-pass-1.tsv").is_file())
        self.assertTrue((second_attempt / "copy-pass-2.tsv").is_file())
        self.assertEqual((self.root / "copy-count").read_text(encoding="ascii").strip(), "4")
        records, _head, _mode = mode_fixtures.blob_mode.read_authority(self.state_dir, self.report_root)
        self.assertEqual(records[-1]["outcome"], "s3-accepted")
        self.assertEqual(records[-1]["generation"], 2)

    def test_live_writer_mismatch_rejects_before_starting_rustfs(self) -> None:
        before = self.state_snapshot()
        result = self.run_runner(seed_running_writer=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("writer-freeze-not-held", result.stderr)
        lines = self.events.read_text(encoding="utf-8")
        self.assertNotIn("compose up -d --wait rustfs", lines)
        self.assertNotIn("compose run --rm --no-deps blob-migrate", lines)
        self.assertEqual(self.state_snapshot(), before)

    def test_cross_project_writer_rejects_before_fresh_database_capture(self) -> None:
        before = self.state_snapshot()
        result = self.run_runner(host_container_case="cross-project-writer")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("writer-freeze-not-held", result.stderr)
        events = self.events.read_text(encoding="utf-8")
        self.assertNotIn("compose exec -T postgres pg_dump", events)
        self.assertNotIn("compose up -d --wait rustfs", events)
        self.assertEqual(self.state_snapshot(), before)

    def test_unlabelled_writable_source_volume_rejects_before_fresh_database_capture(self) -> None:
        for case in ("unlabelled-source-volume-writer", "unlabelled-source-bind-writer"):
            with self.subTest(case=case):
                before = self.state_snapshot()
                result = self.run_runner(host_container_case=case)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("writer-freeze-not-held", result.stderr)
                events = self.events.read_text(encoding="utf-8")
                self.assertNotIn("compose exec -T postgres pg_dump", events)
                self.assertNotIn("compose up -d --wait rustfs", events)
                self.assertEqual(self.state_snapshot(), before)

    def test_stale_reference_snapshot_is_rejected_after_freeze_before_copy(self) -> None:
        before = self.state_snapshot()
        changed = (self.bundle / "references.tsv").read_bytes() + b"file\ttrue\t03/259\n"
        for name in ("references-provisional.tsv", "references.tsv"):
            path = self.bundle / name
            path.write_bytes(changed)
            path.chmod(0o600)
        result = self.run_runner()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("fresh-reference-evidence-mismatch", result.stderr)
        lines = self.events.read_text(encoding="utf-8").splitlines()
        freeze = next(index for index, line in enumerate(lines) if "compose stop --timeout 120 telegramd telegramd-proxy" in line)
        fresh_query = next(index for index, line in enumerate(lines) if "compose exec -T postgres psql" in line)
        self.assertLess(freeze, fresh_query)
        self.assertNotIn("compose up -d --wait rustfs", "\n".join(lines))
        self.assertEqual(self.state_snapshot(), before)
        records, _head, _mode = mode_fixtures.blob_mode.read_authority(self.state_dir, self.report_root)
        self.assertEqual(records[-1]["outcome"], "initial-local")

    def test_cutover_dump_provenance_mismatch_rejects_after_freeze(self) -> None:
        before = self.state_snapshot()
        dump_path = self.bundle / "postgres.dump"
        dump_path.write_bytes(b"substituted dump")
        dump_path.chmod(0o600)
        result = self.run_runner()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("fresh-database-dump-provenance-mismatch", result.stderr)
        lines = self.events.read_text(encoding="utf-8").splitlines()
        freeze = next(index for index, line in enumerate(lines) if "compose stop --timeout 120 telegramd telegramd-proxy" in line)
        resume = next(index for index, line in enumerate(lines) if "compose up -d --no-deps telegramd telegramd-proxy" in line)
        self.assertLess(freeze, resume)
        self.assertNotIn("compose up -d --wait rustfs", "\n".join(lines))
        self.assertEqual(self.state_snapshot(), before)

    def test_recovery_runs_two_restores_and_publishes_before_local_serving(self) -> None:
        bundle = self.seed_s3_authority_and_running_stack()
        result = self.run_action("recover-local", bundle=bundle)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("recovery_qualification=pass", result.stdout)
        self.assertIn("transition=accepted outcome=recovered-local", result.stdout)
        self.assertIn("retained_cutover_key_count=1", result.stdout)
        self.assertNotIn(LOCAL_ONLY_KEY, result.stdout)
        lines = self.events.read_text(encoding="utf-8").splitlines()
        freeze = next(index for index, line in enumerate(lines) if "compose stop --timeout 120 telegramd telegramd-proxy" in line)
        s3_census = next(index for index, line in enumerate(lines) if "blob-migrate --direction s3-census" in line)
        before_restore = next(index for index, line in enumerate(lines) if "blob-restore --direction local-census" in line)
        restores = [index for index, line in enumerate(lines) if "blob-restore --direction s3-to-local" in line]
        local_start = next(index for index, line in enumerate(lines) if "compose up -d --no-deps telegramd telegramd-proxy" in line)
        self.assertEqual(len(restores), 2)
        self.assertLess(freeze, s3_census)
        self.assertLess(s3_census, before_restore)
        self.assertLess(before_restore, restores[0])
        self.assertLess(restores[0], restores[1])
        self.assertLess(restores[1], local_start)
        records, _head, _mode = mode_fixtures.blob_mode.read_authority(self.state_dir, self.report_root)
        self.assertEqual(records[-1]["outcome"], "recovered-local")
        self.assertEqual(records[-1]["evidence"]["retained_cutover_key_count"], 1)
        report = mode_fixtures.blob_mode.read_json(
            mode_fixtures.blob_mode.report_path(self.report_root, records[-1]["transition_id"])
        )
        schema_evidence = json.loads(
            (self.report_root / report["phase_files"]["schema_evidence_sha256"]).read_text(encoding="utf-8")
        )
        self.assertEqual(
            schema_evidence["live_capture"]["dump_sha256"],
            report["phase_digests"]["dump_sha256"],
        )
        self.assertEqual(
            schema_evidence["live_capture"]["observed"]["revision_detail"],
            qualifier_fixtures.good_migration_evidence()["revision_detail"],
        )

    def test_r67_recovery_reports_its_validated_release_set(self) -> None:
        bundle = self.seed_s3_authority_and_running_stack(release_set="60-67")
        result = self.run_action("recover-local", bundle=bundle)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("recovery_qualification=pass", result.stdout)
        self.assertIn("migrations=60-67", result.stdout)
        self.assertNotIn("migrations=60-66", result.stdout)

    def test_r70_recovery_recaptures_schema_and_inert_evidence_before_dump(self) -> None:
        bundle = self.seed_s3_authority_and_running_stack(release_set="60-70")
        result = self.run_action("recover-local", bundle=bundle)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("recovery_qualification=pass", result.stdout)
        self.assertIn("migrations=60-70", result.stdout)
        records, _head, _mode = mode_fixtures.blob_mode.read_authority(self.state_dir, self.report_root)
        self.assertEqual(records[-1]["outcome"], "recovered-local")

        lines = self.events.read_text(encoding="utf-8").splitlines()
        schema_captures = [
            index for index, line in enumerate(lines) if "atlas_schema_revisions" in line
        ]
        inert_capture = next(
            index for index, line in enumerate(lines) if "erasure_epoch_completion" in line
        )
        dump = next(
            index
            for index, line in enumerate(lines)
            if "compose exec -T postgres pg_dump" in line
        )
        self.assertEqual(len(schema_captures), 2)
        self.assertTrue(all(index < dump for index in schema_captures))
        self.assertLess(inert_capture, dump)

        report = mode_fixtures.blob_mode.read_json(
            mode_fixtures.blob_mode.report_path(self.report_root, records[-1]["transition_id"])
        )
        schema_path = self.report_root / report["phase_files"]["schema_evidence_sha256"]
        reference_rows = (
            self.report_root / report["phase_files"]["recovery_reference_rows_sha256"]
        ).read_text(encoding="ascii")
        self.assertEqual(reference_rows, f"upload_part\ttrue\t{PART_KEY}\n")
        active_links = (
            self.report_root / report["phase_files"]["recovery_active_links_sha256"]
        ).read_bytes()
        self.assertEqual(active_links, b"")
        recovery = mode_fixtures.blob_mode.read_json(schema_path.parent / "recovery.json")
        freeze = recovery["freeze"]
        self.assertLessEqual(freeze["baseline_schema_captured_at"], freeze["schema_captured_at"])
        self.assertLessEqual(freeze["schema_captured_at"], recovery["dump"]["captured_at"])
        self.assertLessEqual(freeze["started_at"], freeze["inert_surfaces_captured_at"])
        self.assertLessEqual(freeze["inert_surfaces_captured_at"], freeze["held_at"])
        self.assertEqual(
            recovery["references"]["inert_surfaces_query_sha256"],
            qualifier_fixtures.gate_constants("60-70")["r70_inert_surfaces_query_sha256"],
        )
        migrations = mode_fixtures.blob_mode.read_json(schema_path)
        expected_revision_detail = qualifier_fixtures.good_migration_evidence("60-70")["revision_detail"]
        for capture_name in ("baseline_live_capture", "live_capture"):
            self.assertEqual(
                migrations[capture_name]["observed"]["revision_detail"],
                expected_revision_detail,
            )
        self.assertEqual(
            migrations["inert_surfaces"],
            {
                name: False
                for name in qualifier_fixtures.gate_constants("60-70")["r70_inert_surfaces"]
            },
        )

    def test_r70_recovery_rejects_nonempty_live_revision_state_before_dump(self) -> None:
        version = qualifier_fixtures.VERSIONS_60_70[-1]
        for field in ("error_stmt_empty", "partial_hashes_empty"):
            with self.subTest(field=field):
                bundle = self.seed_s3_authority_and_running_stack(release_set="60-70")
                authority_before = self.state_snapshot()
                observation = json.loads(self.live_schema_path.read_text(encoding="utf-8"))
                observation["revision_detail"][version][field] = False
                self.write_live_schema_fixture(observation)

                result = self.run_action("recover-local", bundle=bundle)

                self.assertNotEqual(result.returncode, 0)
                self.assertIn("baseline-live-schema-rejected", result.stderr)
                self.assertEqual(self.state_snapshot(), authority_before)
                lines = self.events.read_text(encoding="utf-8").splitlines()
                self.assertFalse(any("compose exec -T postgres pg_dump" in line for line in lines))
                self.assertFalse(any("blob-restore --direction s3-to-local" in line for line in lines))

    def test_r70_recovery_returns_reference_coverage_for_true_inert_surface(self) -> None:
        bundle = self.seed_s3_authority_and_running_stack(release_set="60-70")
        authority_before = self.state_snapshot()
        surfaces = json.loads(self.live_r70_inert_path.read_text(encoding="utf-8"))
        surfaces["erasure_outbox"] = True
        self.live_r70_inert_path.write_text(
            json.dumps(surfaces, sort_keys=True, separators=(",", ":")) + "\n",
            encoding="utf-8",
        )
        self.live_r70_inert_path.chmod(0o600)

        result = self.run_action("recover-local", bundle=bundle)

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("reference_coverage", result.stderr)
        self.assertNotIn("live-inert-surfaces-rejected", result.stderr)
        self.assertEqual(self.state_snapshot(), authority_before)
        lines = self.events.read_text(encoding="utf-8").splitlines()
        self.assertFalse(any("compose exec -T postgres pg_dump" in line for line in lines))
        self.assertFalse(any("blob-restore --direction s3-to-local" in line for line in lines))

    def test_interruption_after_second_restore_keeps_s3_authority_and_rejects_local_start(self) -> None:
        bundle = self.seed_s3_authority_and_running_stack()
        before = self.state_snapshot()
        interrupted = self.run_action(
            "recover-local", bundle=bundle, interrupt_after="restore-pass-2"
        )
        self.assertEqual(interrupted.returncode, 86)
        self.assertEqual(self.state_snapshot(), before)
        self.assertTrue((self.root / "rustfs").is_file())
        lines = self.events.read_text(encoding="utf-8").splitlines()
        restores = [index for index, line in enumerate(lines) if "blob-restore --direction s3-to-local" in line]
        self.assertEqual(len(restores), 2)
        self.assertNotIn(
            "compose up -d --no-deps telegramd telegramd-proxy",
            "\n".join(lines),
        )

        mode = mode_fixtures.blob_mode
        records, head, mode_bytes = mode.read_authority(self.state_dir, self.report_root)
        self.assertEqual(mode_bytes, head)
        self.assertEqual(records[-1]["outcome"], "s3-accepted")
        local_render = mode.compose_inventory(
            json.loads(self.local_compose_json.read_text(encoding="utf-8")), self.checkout
        )
        s3_render = mode.compose_inventory(
            json.loads((self.bundle / "candidate-compose.json").read_text(encoding="utf-8")),
            self.checkout,
        )
        mode.assert_compose_matches(
            s3_render,
            records[-1],
            os.path.realpath(self.checkout / ".state" / "blob-mode"),
            self.checkout / "docker-compose.override.yml",
        )
        with self.assertRaises(mode.Reject) as caught:
            mode.assert_compose_matches(
                local_render,
                records[-1],
                os.path.realpath(self.checkout / ".state" / "blob-mode"),
                self.checkout / "docker-compose.override.yml",
            )
        self.assertEqual(str(caught.exception), "render-backend-mismatch")

    def test_recovery_live_schema_mismatch_keeps_s3_authority(self) -> None:
        bundle = self.seed_s3_authority_and_running_stack()
        before = self.state_snapshot()
        self.write_live_schema_fixture({
            "applied_revisions": qualifier_fixtures.VERSIONS_60_66[:-1],
            "revision_detail": qualifier_fixtures.good_migration_evidence()["revision_detail"],
            "migration_66_schema": qualifier_fixtures.new_unread_mark_schema(),
        })
        result = self.run_action("recover-local", bundle=bundle)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("live-schema-rejected", result.stderr)
        lines = self.events.read_text(encoding="utf-8").splitlines()
        self.assertFalse(any("blob-restore --direction s3-to-local" in line for line in lines))
        self.assertEqual((self.root / "serving-backend").read_text(encoding="utf-8").strip(), "s3")
        self.assertEqual(self.state_snapshot(), before)

    def test_recovery_live_atlas_hash_mismatch_preserves_s3_authority(self) -> None:
        bundle = self.seed_s3_authority_and_running_stack()
        migration_evidence = qualifier_fixtures.good_migration_evidence()
        observation = {
            "applied_revisions": migration_evidence["target_revisions"],
            "revision_detail": migration_evidence["revision_detail"],
            "migration_66_schema": migration_evidence["migration_66_schema"],
        }
        observation["revision_detail"][qualifier_fixtures.VERSIONS_60_66[-1]]["hash"] = "h1:stale"
        self.write_live_schema_fixture(observation)
        result = self.run_action("recover-local", bundle=bundle)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("live-schema-rejected", result.stderr)
        self.assertEqual((self.root / "serving-backend").read_text(encoding="utf-8").strip(), "s3")
        records, _head, _mode = mode_fixtures.blob_mode.read_authority(self.state_dir, self.report_root)
        self.assertEqual(records[-1]["outcome"], "s3-accepted")

    def test_failed_second_restore_keeps_s3_authority_and_resumes_s3(self) -> None:
        bundle = self.seed_s3_authority_and_running_stack()
        before = self.state_snapshot()
        result = self.run_action("recover-local", "failed-second-restore", bundle=bundle)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("restore-pass-2-failed", result.stderr)
        lines = self.events.read_text(encoding="utf-8").splitlines()
        serving_starts = [line for line in lines if "compose up -d --no-deps telegramd telegramd-proxy" in line]
        self.assertEqual(len(serving_starts), 1)
        self.assertEqual((self.root / "serving-backend").read_text(encoding="utf-8").strip(), "s3")
        self.assertEqual(self.state_snapshot(), before)
        records, _head, _mode = mode_fixtures.blob_mode.read_authority(self.state_dir, self.report_root)
        self.assertEqual(records[-1]["outcome"], "s3-accepted")
        mode = mode_fixtures.blob_mode
        local_render = mode.compose_inventory(json.loads(self.local_compose_json.read_text(encoding="utf-8")), self.checkout)
        s3_render = mode.compose_inventory(
            json.loads((self.bundle / "candidate-compose.json").read_text(encoding="utf-8")), self.checkout
        )
        mode.assert_compose_matches(
            s3_render, records[-1], os.path.realpath(self.checkout / ".state" / "blob-mode"),
            self.checkout / "docker-compose.override.yml",
        )
        with self.assertRaises(mode.Reject) as caught:
            mode.assert_compose_matches(
                local_render, records[-1], os.path.realpath(self.checkout / ".state" / "blob-mode"),
                self.checkout / "docker-compose.override.yml",
            )
        self.assertEqual(str(caught.exception), "render-backend-mismatch")

    def test_recovery_dump_provenance_failure_rejects_after_freeze_and_resumes_s3(self) -> None:
        bundle = self.seed_s3_authority_and_running_stack()
        before = self.state_snapshot()
        dump_path = bundle / "postgres.dump"
        dump_path.write_bytes(b"substituted dump")
        dump_path.chmod(0o600)
        result = self.run_action("recover-local", bundle=bundle)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("recovery-dump-invalid", result.stderr)
        lines = self.events.read_text(encoding="utf-8").splitlines()
        freeze = next(index for index, line in enumerate(lines) if "compose stop --timeout 120 telegramd telegramd-proxy" in line)
        resume = next(index for index, line in enumerate(lines) if "compose up -d --no-deps telegramd telegramd-proxy" in line)
        self.assertLess(freeze, resume)
        self.assertEqual((self.root / "serving-backend").read_text(encoding="utf-8").strip(), "s3")
        self.assertEqual(self.state_snapshot(), before)

    def test_test_mode_without_private_synthetic_fixture_cannot_reach_docker(self) -> None:
        environment = os.environ.copy()
        environment["BLOB_TRANSITION_TEST_MODE"] = "1"
        environment.pop("BLOB_TRANSITION_TEST_FIXTURE_ROOT", None)
        environment.pop("BLOB_TRANSITION_TEST_FIXTURE_DOCKER", None)
        result = subprocess.run(
            [
                "python3", str(RUNNER), "accept-s3", "--bundle", str(self.bundle),
                "--checkout", str(self.checkout), "--state-dir", str(self.state_dir),
                "--report-root", str(self.report_root), "--lock-path", str(self.lock_path),
            ],
            env=environment,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("synthetic-fixture-scope-required", result.stderr)
        self.assertFalse(self.events.exists())

    def test_test_mode_pins_qualification_to_fixture_docker(self) -> None:
        result = self.run_runner(put_decoy_first=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.decoy_called.exists())
        self.assertIn("--project-directory", self.events.read_text(encoding="utf-8"))


if __name__ == "__main__":
    unittest.main()
