import errno
import os
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


if __name__ == "__main__":
    unittest.main()
