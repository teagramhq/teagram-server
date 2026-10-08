import contextlib
import errno
import io
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import isolated_control as control


REVISION = "a" * 40
IMAGE = "sha256:" + "b" * 64
CONTAINER = "c" * 64


def valid_observation(case="baseline"):
    return {
        "record_type": "case_result",
        "case": case,
        "case_verdict": "pass",
        "reason_code": "none",
        "selector_synthetic_http": "status=200",
        "selector_root_http": "status=200",
        "direct_landing_http": "status=200",
        "selector_probe_exit": 0,
        "landing_probe_exit": 0,
        "selector_health": "healthy",
        "landing_health": "healthy",
        "web_health": "none",
        "probe_health": "none",
        "acquisition_failure": "none",
        "selector_state": "running",
        "landing_state": "running",
        "web_state": "running",
        "probe_state": "running",
    }


class EvidenceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name) / "private"
        self.writer = control.EvidenceWriter(self.directory)
        self.addCleanup(self.writer.close)

    def write_complete_control(self, include_gate=True):
        self.writer.append(
            {
                "record_type": "control_start",
                "source": control.IMAGE_SOURCE,
                "source_revision": REVISION,
            }
        )
        identity = {"record_type": "identities"}
        for service in control.SERVICES:
            identity.update(
                {
                    f"{service}_container_id": CONTAINER,
                    f"{service}_image_id": IMAGE,
                    f"{service}_image_source": control.IMAGE_SOURCE,
                    f"{service}_image_revision": REVISION,
                }
            )
        self.writer.append(identity)
        for case in control.REQUIRED_CASES:
            self.writer.append(valid_observation(case))
        if include_gate:
            self.writer.append({"record_type": "pre_cleanup_gate", "verdict": "pass"})

    def test_evidence_directory_and_file_are_private(self):
        self.assertEqual(self.directory.stat().st_mode & 0o777, 0o700)
        self.assertEqual(self.writer.path.stat().st_mode & 0o777, 0o600)

    def test_append_failure_rejects_evidence(self):
        with mock.patch.object(control.os, "write", side_effect=OSError(errno.ENOSPC, "full")):
            with self.assertRaises(control.PersistenceFailure):
                self.writer.append({"record_type": "control_start"})

    def test_sync_failure_rejects_evidence(self):
        with mock.patch.object(control.os, "fsync", side_effect=OSError(errno.EIO, "sync")):
            with self.assertRaises(control.PersistenceFailure):
                self.writer.append({"record_type": "control_start"})

    def test_invalid_permissions_reject_acceptance(self):
        self.writer.append({"record_type": "partial-control"})
        os.chmod(self.writer.path, 0o644)
        with self.assertRaises(control.PersistenceFailure):
            control.validate_evidence(self.writer.path, REVISION, "pre_cleanup")

    def test_partial_record_rejects_acceptance(self):
        self.writer.close()
        self.writer.path.write_bytes(b'{"record_type":"control_start"')
        with self.assertRaises(control.PersistenceFailure):
            control.validate_evidence(self.writer.path, REVISION, "pre_cleanup")

    def test_missing_case_record_rejects_acceptance(self):
        self.write_complete_control()
        raw = self.writer.path.read_text()
        self.writer.close()
        self.writer.path.write_text(raw.replace('"case":"landing_recovery"', '"case":"missing"'))
        with self.assertRaises(control.PersistenceFailure):
            control.validate_evidence(self.writer.path, REVISION, "pre_cleanup")

    def test_observations_validate_before_pre_cleanup_gate_record(self):
        self.write_complete_control(include_gate=False)
        control.validate_evidence(self.writer.path, REVISION, "pre_cleanup")

    def test_diagnostic_output_accepts_only_status_or_fixed_transport(self):
        self.assertEqual(control.parse_diagnostic_result("status=503\n", 0), "status=503")
        self.assertEqual(
            control.parse_diagnostic_result("transport_error=name_resolution\n", 1),
            "transport_error=name_resolution",
        )
        self.assertEqual(
            control.parse_diagnostic_result("transport_error=secret host:123\n", 1),
            "transport_error=diagnostic_failure",
        )

    def test_persistence_failure_prevents_control_acceptance(self):
        runner = control.LinkEdgeControl(self.writer)
        with mock.patch.object(self.writer, "append", side_effect=control.PersistenceFailure()):
            with self.assertRaises(control.PersistenceFailure):
                runner.record({"record_type": "control_start"})
        self.assertTrue(runner.persistence_failed)
        self.assertFalse(runner.finalize())


class TimingAndCleanupTests(unittest.TestCase):
    def test_compose_projection_accepts_omitted_default_false(self):
        secure_service = {
            "read_only": True,
            "cap_drop": ["ALL"],
            "security_opt": ["no-new-privileges:true"],
            "user": "65532:65532",
        }
        config = {
            "services": {
                "linklanding": {
                    **secure_service,
                    "networks": {"private-edge": None, "landing-publish": None},
                    "healthcheck": {"test": ["CMD", "/usr/local/bin/linkprobe", "landing"]},
                },
                "selector": {
                    **secure_service,
                    "networks": {"web-edge": None, "private-edge": None},
                    "depends_on": {"linklanding": {"condition": "service_healthy"}},
                    "healthcheck": {"test": ["CMD", "/usr/local/bin/linkprobe", "selector"]},
                },
                "web": {**secure_service, "networks": {"web-edge": None}},
                "probe": {
                    **secure_service,
                    "networks": {"private-edge": None},
                    "depends_on": {"selector": {"condition": "service_started"}},
                    "entrypoint": ["/usr/local/bin/linkprobe"],
                    "command": ["hold"],
                },
            },
            "networks": {
                "web-edge": {"internal": True},
                "private-edge": {"internal": True},
                # Compose omits internal=false from its normalized projection.
                "landing-publish": {
                    "driver": "bridge",
                    "driver_opts": {"com.docker.network.bridge.enable_ip_masquerade": "false"},
                },
            },
        }
        runner = object.__new__(control.LinkEdgeControl)
        runner.validate_projection(config)

    def test_timeout_returns_last_complete_observation_at_bound(self):
        now = [0.0]
        observations = []

        def clock():
            return now[0]

        def pause(seconds):
            now[0] += seconds

        def observe():
            value = {"selector_synthetic_http": "status=200", "selector_root_http": "status=502"}
            observations.append(value)
            return value

        ready, last, elapsed = control.wait_for_observation(
            observe,
            lambda value: value["selector_root_http"] == "status=200",
            15,
            clock=clock,
            pause=pause,
        )
        self.assertFalse(ready)
        self.assertGreaterEqual(elapsed, 15)
        self.assertEqual(last, observations[-1])
        self.assertGreaterEqual(len(observations), 2)

    def test_cleanup_runs_after_success_and_failure(self):
        events = []

        self.assertEqual(
            control.run_with_cleanup(lambda: events.append("work") or "ok", lambda: events.append("cleanup") or True),
            "ok",
        )
        self.assertEqual(events, ["work", "cleanup"])

        events.clear()

        def fail_work():
            events.append("work")
            raise control.ControlFailure("case_failed")

        with self.assertRaisesRegex(control.ControlFailure, "case_failed"):
            control.run_with_cleanup(fail_work, lambda: events.append("cleanup") or True)
        self.assertEqual(events, ["work", "cleanup"])

    def test_cleanup_failure_cannot_turn_failed_work_into_success(self):
        def fail_work():
            raise control.ControlFailure("case_failed")

        with self.assertRaisesRegex(control.ControlFailure, "case_failed"):
            control.run_with_cleanup(fail_work, lambda: False)


class ReadinessFailureTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.writer = control.EvidenceWriter(Path(self.temp.name) / "private")
        self.addCleanup(self.writer.close)

    def test_web_stopped_requires_linkprobe_failure_exit_one(self):
        observation = valid_observation("web_stopped")
        observation.update(
            {
                "selector_root_http": "status=502",
                "selector_probe_exit": 1,
                "selector_health": "unhealthy",
                "web_state": "exited",
            }
        )
        self.assertTrue(control.LinkEdgeControl.web_stopped_ready(observation))
        for exit_code in (126, 127):
            with self.subTest(exit_code=exit_code):
                observation["selector_probe_exit"] = exit_code
                self.assertFalse(control.LinkEdgeControl.web_stopped_ready(observation))

    def test_landing_unavailable_requires_linkprobe_failure_exit_one(self):
        observation = valid_observation("landing_unavailable")
        observation.update(
            {
                "selector_synthetic_http": "status=503",
                "direct_landing_http": "transport_error=connection_refused",
                "selector_probe_exit": 1,
                "selector_health": "unhealthy",
                "landing_state": "exited",
            }
        )
        self.assertTrue(control.LinkEdgeControl.landing_unavailable_ready(observation))
        for exit_code in (126, 127):
            with self.subTest(exit_code=exit_code):
                observation["selector_probe_exit"] = exit_code
                self.assertFalse(control.LinkEdgeControl.landing_unavailable_ready(observation))

    def test_healthy_readiness_rejects_unknown_web_or_probe_health(self):
        for field in ("web_health", "probe_health"):
            with self.subTest(field=field):
                observation = valid_observation()
                observation[field] = "unknown"
                self.assertFalse(control.LinkEdgeControl.healthy_ready(observation))

    def test_acquisition_fault_fails_before_a_later_healthy_sample(self):
        runner = control.LinkEdgeControl(self.writer)
        runner.container_ids = {
            "selector": "s" * 64,
            "linklanding": "l" * 64,
            "web": "w" * 64,
            "probe": "p" * 64,
        }
        web_health_reads = 0

        def inspect_value(container_id, template):
            nonlocal web_health_reads
            if template.endswith("Status}}{{else}}none{{end}}"):
                if container_id == runner.container_ids["web"]:
                    web_health_reads += 1
                    return "unknown" if web_health_reads == 1 else "none"
                if container_id == runner.container_ids["probe"]:
                    return "none"
                return "healthy"
            return "running"

        with (
            mock.patch.object(runner, "diagnostic", return_value="status=200"),
            mock.patch.object(runner, "probe_exit", return_value=0),
            mock.patch.object(runner, "inspect_value", side_effect=inspect_value),
        ):
            with self.assertRaisesRegex(control.ControlFailure, "baseline_failed"):
                runner.run_case("baseline", 45, runner.healthy_ready)

        records = control._read_records(self.writer.path)
        observations = [r for r in records if r.get("record_type") == "observation"]
        failures = [r for r in records if r.get("record_type") == "case_result"]
        self.assertEqual(web_health_reads, 1)
        self.assertEqual(len(observations), 1)
        self.assertEqual(observations[0]["web_health"], "unknown")
        self.assertEqual(observations[0]["acquisition_failure"], "observation_unavailable")
        self.assertEqual(failures[0]["case_verdict"], "fail")
        self.assertEqual(failures[0]["reason_code"], "observation_unavailable")

    def test_running_probe_exec_failure_is_an_acquisition_failure(self):
        runner = control.LinkEdgeControl(self.writer)
        runner.container_ids = {
            "selector": "s" * 64,
            "linklanding": "l" * 64,
            "web": "w" * 64,
            "probe": "p" * 64,
        }

        def docker_exec(args, **_kwargs):
            exit_code = 127 if args[-1] == "selector" else 0
            return subprocess.CompletedProcess(args, exit_code, "", "")

        def inspect_value(_container_id, template):
            if ".State.Health" in template:
                return "healthy"
            return "running"

        with (
            mock.patch.object(runner, "diagnostic", return_value="status=200"),
            mock.patch.object(runner, "inspect_value", side_effect=inspect_value),
            mock.patch.object(control.subprocess, "run", side_effect=docker_exec),
        ):
            observation = runner.observe("web_stopped", 1)

        self.assertEqual(observation["selector_probe_exit"], "probe_runtime_error")
        self.assertEqual(observation["acquisition_failure"], "observation_unavailable")


class RunnerFaultTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)

    def run_fixture(self, mode, *, fail_observation_append=False, cap_case_timeout=False):
        fixture = RunnerRuntime(mode)

        # Keep production case handling, observation, cleanup, and finalization; provide
        # deterministic source and Docker fixtures for the runner's external boundary.
        class FixtureControl(control.LinkEdgeControl):
            def __init__(self, evidence):
                super().__init__(evidence)
                fixture.runner = self

            def preflight(self):
                self.revision = REVISION
                self.record(
                    {
                        "record_type": "control_start",
                        "project": self.project,
                        "source": control.IMAGE_SOURCE,
                        "source_revision": REVISION,
                    }
                )

            def build_and_start(self):
                self.stack_maybe_started = True
                names = {"selector": "a", "linklanding": "b", "web": "c", "probe": "d"}
                images = {"selector": "e", "linklanding": "f", "web": "1", "probe": "2"}
                self.container_ids = {name: letter * 64 for name, letter in names.items()}
                identity = {"record_type": "identities"}
                for service in control.SERVICES:
                    identity.update(
                        {
                            f"{service}_container_id": self.container_ids[service],
                            f"{service}_image_id": "sha256:" + images[service] * 64,
                            f"{service}_image_source": control.IMAGE_SOURCE,
                            f"{service}_image_revision": REVISION,
                        }
                    )
                self.record(identity)
                self.identity_recorded = True
                fixture.containers[:] = list(self.container_ids.values())
                fixture.networks[:] = ["fixture-network"]

            def diagnostic(self, target):
                if fixture.mode == "probe_restarting":
                    return "transport_error=diagnostic_failure"
                if fixture.mode == "timeout" and target == "selector-root":
                    return "status=502"
                return "status=200"

            def probe_exit(self, _container_id, mode):
                if fixture.mode == "timeout" and mode == "selector":
                    return 1
                return 0

            def inspect_value(self, container_id, template):
                service = next(
                    name for name, value in self.container_ids.items() if value == container_id
                )
                if ".State.Health" not in template:
                    if fixture.mode == "probe_restarting" and service == "probe":
                        return "restarting"
                    return "running"
                if service in {"web", "probe"}:
                    return "none"
                if fixture.mode == "timeout" and service == "selector":
                    return "unhealthy"
                return "healthy"

            def compose(self, *args, reason, timeout=60, allow_failure=False):
                if args and args[0] == "down":
                    fixture.containers.clear()
                    fixture.networks.clear()
                    fixture.volumes.clear()
                return subprocess.CompletedProcess(args, 0, "", "")

            def command(self, args, reason, timeout=15.0, *, allow_failure=False):
                if args[:2] == ["docker", "ps"]:
                    return subprocess.CompletedProcess(args, 0, "\n".join(fixture.containers), "")
                if args[:3] == ["docker", "network", "ls"]:
                    return subprocess.CompletedProcess(args, 0, "\n".join(fixture.networks), "")
                if args[:3] == ["docker", "volume", "ls"]:
                    return subprocess.CompletedProcess(args, 0, "\n".join(fixture.volumes), "")
                if args[:3] == ["docker", "image", "inspect"]:
                    return subprocess.CompletedProcess(args, 1, "", "")
                return subprocess.CompletedProcess(args, 0, "", "")

        evidence_directory = Path(self.temp.name) / f"{mode}-evidence"
        real_writer = control.EvidenceWriter

        def writer_factory(directory):
            writer = real_writer(directory)
            if fail_observation_append:
                append = writer.append

                def fail_observation(record):
                    if record.get("record_type") == "observation":
                        raise control.PersistenceFailure()
                    append(record)

                writer.append = fail_observation
            return writer

        real_wait = control.wait_for_observation

        def immediate_timeout(observe, ready, _timeout):
            return real_wait(observe, ready, 0.0)

        stdout = io.StringIO()
        stderr = io.StringIO()
        with contextlib.ExitStack() as stack:
            stack.enter_context(mock.patch.object(control, "LinkEdgeControl", FixtureControl))
            stack.enter_context(mock.patch.object(control, "EvidenceWriter", side_effect=writer_factory))
            if cap_case_timeout:
                stack.enter_context(
                    mock.patch.object(
                        control,
                        "wait_for_observation",
                        side_effect=immediate_timeout,
                    )
                )
            with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                exit_code = control.main(["--evidence-dir", str(evidence_directory)])
        return (
            exit_code,
            evidence_directory / "control-evidence.jsonl",
            fixture,
            stderr.getvalue(),
        )

    def test_versioned_runner_persists_timeout_and_cleans_fixture(self):
        exit_code, path, fixture, stderr = self.run_fixture("timeout", cap_case_timeout=True)
        records = control._read_records(path)
        failed_case = next(r for r in records if r.get("record_type") == "case_result")
        cleanup = next(r for r in records if r.get("record_type") == "cleanup_result")
        result = next(r for r in records if r.get("record_type") == "control_result")

        self.assertEqual(exit_code, 1)
        self.assertIn("reason=baseline_failed", stderr)
        self.assertEqual(failed_case["case_verdict"], "fail")
        self.assertEqual(failed_case["reason_code"], "timeout")
        self.assertTrue(all(field in failed_case for field in control.OBSERVATION_FIELDS))
        self.assertEqual(cleanup["verdict"], "pass")
        self.assertEqual(cleanup["containers_remaining"], 0)
        self.assertEqual(cleanup["networks_remaining"], 0)
        self.assertEqual(result["verdict"], "fail")
        self.assertEqual(fixture.containers, [])
        self.assertEqual(fixture.networks, [])

    def test_versioned_runner_rejects_unavailable_probe_and_cleans_fixture(self):
        exit_code, path, fixture, stderr = self.run_fixture(
            "probe_restarting", cap_case_timeout=True
        )
        records = control._read_records(path)
        observation = next(r for r in records if r.get("record_type") == "observation")
        failed_case = next(r for r in records if r.get("record_type") == "case_result")
        cleanup = next(r for r in records if r.get("record_type") == "cleanup_result")
        result = next(r for r in records if r.get("record_type") == "control_result")

        self.assertEqual(exit_code, 1)
        self.assertIn("reason=baseline_failed", stderr)
        self.assertEqual(observation["probe_state"], "restarting")
        self.assertEqual(observation["acquisition_failure"], "observation_unavailable")
        self.assertEqual(failed_case["case_verdict"], "fail")
        self.assertEqual(failed_case["reason_code"], "observation_unavailable")
        self.assertEqual(fixture.runner.attempts["baseline"], 1)
        self.assertTrue(
            all(
                observation[field] == "transport_error=diagnostic_failure"
                for field in (
                    "selector_synthetic_http",
                    "selector_root_http",
                    "direct_landing_http",
                )
            )
        )
        self.assertEqual(cleanup["verdict"], "pass")
        self.assertEqual(cleanup["containers_remaining"], 0)
        self.assertEqual(cleanup["networks_remaining"], 0)
        self.assertEqual(result["verdict"], "fail")
        self.assertEqual(fixture.containers, [])
        self.assertEqual(fixture.networks, [])

    def test_versioned_runner_persistence_failure_exits_and_cleans_fixture(self):
        exit_code, path, fixture, stderr = self.run_fixture(
            "healthy", fail_observation_append=True
        )
        records = control._read_records(path)

        self.assertEqual(exit_code, 1)
        self.assertIn("reason=persistence_failure", stderr)
        self.assertTrue(any(r.get("record_type") == "identities" for r in records))
        self.assertFalse(any(r.get("record_type") == "control_result" for r in records))
        self.assertEqual(fixture.containers, [])
        self.assertEqual(fixture.networks, [])
        self.assertTrue(fixture.runner.cleanup_ok)


class RunnerRuntime:
    def __init__(self, mode):
        self.mode = mode
        self.containers = []
        self.networks = []
        self.volumes = []
        self.runner = None


if __name__ == "__main__":
    unittest.main()
