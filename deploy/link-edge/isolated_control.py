#!/usr/bin/env python3
"""Run the credential-free link-edge failure control in a private Compose project."""

from __future__ import annotations

import argparse
import concurrent.futures
import errno
import json
import os
import re
import stat
import subprocess
import sys
import tempfile
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Callable


ROOT = Path(__file__).resolve().parents[2]
FIXTURE = ROOT / "deploy/link-edge/control-compose.yaml"
IDENTITY_GATE = ROOT / "deploy/link-edge/container-identity-gate.sh"
IMAGE_SOURCE = "https://github.com/teagramhq/teagram-server"
SERVICES = ("selector", "linklanding", "web", "probe")
REQUIRED_CASES = (
    "baseline",
    "web_stopped",
    "web_recovery",
    "landing_unavailable",
    "landing_recovery",
)
TRANSPORT_CATEGORIES = {
    "connection_refused",
    "name_resolution",
    "timeout",
    "connect_failure",
    "diagnostic_failure",
}
HEALTH_STATES = {"none", "starting", "healthy", "unhealthy", "unknown"}
CONTAINER_STATES = {
    "created",
    "restarting",
    "running",
    "paused",
    "exited",
    "dead",
    "unknown",
}
OBSERVATION_FIELDS = (
    "selector_synthetic_http",
    "selector_root_http",
    "direct_landing_http",
    "selector_probe_exit",
    "landing_probe_exit",
    "selector_health",
    "landing_health",
    "web_health",
    "probe_health",
    "selector_state",
    "landing_state",
    "web_state",
    "probe_state",
)
HTTP_STATUS = re.compile(r"status=([1-5][0-9][0-9])\Z")
HTTP_TRANSPORT = re.compile(r"transport_error=([a-z_]+)\Z")
FULL_ID = re.compile(r"[0-9a-f]{64}\Z")
IMAGE_ID = re.compile(r"sha256:[0-9a-f]{64}\Z")
REVISION = re.compile(r"[0-9a-f]{40}\Z")


class ControlFailure(Exception):
    """A fixed, safe-to-report control failure code."""

    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


class PersistenceFailure(ControlFailure):
    def __init__(self):
        super().__init__("persistence_failure")


def utc_now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _mode(path: Path, wanted: int, want_directory: bool) -> None:
    try:
        info = path.lstat()
    except OSError as exc:
        raise PersistenceFailure() from exc
    if path.is_symlink() or info.st_uid != os.geteuid():
        raise PersistenceFailure()
    if want_directory and not stat.S_ISDIR(info.st_mode):
        raise PersistenceFailure()
    if not want_directory and not stat.S_ISREG(info.st_mode):
        raise PersistenceFailure()
    if stat.S_IMODE(info.st_mode) != wanted:
        raise PersistenceFailure()


class EvidenceWriter:
    """Append complete JSONL records and fsync each record before it is gated."""

    def __init__(self, directory: Path):
        self.directory = directory
        self.path = directory / "control-evidence.jsonl"
        self.fd: int | None = None
        try:
            directory.parent.mkdir(parents=True, exist_ok=True)
            directory.mkdir(mode=0o700)
            self.dir_fd = os.open(directory, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
            flags = os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_EXCL
            flags |= getattr(os, "O_NOFOLLOW", 0)
            self.fd = os.open(self.path, flags, 0o600)
            os.fchmod(self.fd, 0o600)
            _mode(directory, 0o700, True)
            _mode(self.path, 0o600, False)
            os.fsync(self.fd)
            os.fsync(self.dir_fd)
        except (OSError, PersistenceFailure) as exc:
            closed = self.close()
            if not closed:
                raise PersistenceFailure() from exc
            if isinstance(exc, PersistenceFailure):
                raise
            raise PersistenceFailure() from exc

    def close(self) -> bool:
        succeeded = True
        for name in ("fd", "dir_fd"):
            descriptor = getattr(self, name, None)
            if descriptor is not None:
                try:
                    os.close(descriptor)
                except OSError:
                    succeeded = False
                setattr(self, name, None)
        return succeeded

    def check_permissions(self) -> None:
        _mode(self.directory, 0o700, True)
        _mode(self.path, 0o600, False)

    def append(self, record: dict[str, object]) -> None:
        if self.fd is None:
            raise PersistenceFailure()
        self.check_permissions()
        try:
            complete_record = {"timestamp_utc": utc_now(), **record}
            data = (json.dumps(complete_record, sort_keys=True, separators=(",", ":")) + "\n").encode()
            view = memoryview(data)
            while view:
                written = os.write(self.fd, view)
                if written <= 0:
                    raise OSError(errno.EIO, "short evidence write")
                view = view[written:]
            os.fsync(self.fd)
            self.check_permissions()
        except (OSError, PersistenceFailure) as exc:
            if isinstance(exc, PersistenceFailure):
                raise
            raise PersistenceFailure() from exc


def _valid_http_value(value: object) -> bool:
    if not isinstance(value, str):
        return False
    status = HTTP_STATUS.fullmatch(value)
    if status:
        return 100 <= int(status.group(1)) <= 599
    transport = HTTP_TRANSPORT.fullmatch(value)
    return bool(transport and transport.group(1) in TRANSPORT_CATEGORIES)


def _valid_probe_exit(value: object) -> bool:
    return (isinstance(value, int) and 0 <= value <= 255) or value in {
        "unavailable",
        "timeout",
        "probe_runtime_error",
    }


def _read_records(path: Path) -> list[dict[str, object]]:
    _mode(path.parent, 0o700, True)
    _mode(path, 0o600, False)
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise PersistenceFailure() from exc
    if not raw or not raw.endswith(b"\n"):
        raise PersistenceFailure()
    records: list[dict[str, object]] = []
    try:
        for line in raw.splitlines():
            record = json.loads(line)
            if not isinstance(record, dict):
                raise ValueError
            if not re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z", str(record.get("timestamp_utc", ""))):
                raise ValueError
            records.append(record)
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
        raise PersistenceFailure() from exc
    return records


def validate_evidence(path: Path, expected_revision: str, stage: str) -> list[dict[str, object]]:
    """Reject incomplete, malformed, unprivate, or non-durable control evidence."""
    records = _read_records(path)
    if stage not in {"pre_cleanup", "pre_final", "complete"}:
        raise PersistenceFailure()
    starts = [r for r in records if r.get("record_type") == "control_start"]
    identities = [r for r in records if r.get("record_type") == "identities"]
    results = [r for r in records if r.get("record_type") == "case_result"]
    if len(starts) != 1 or len(identities) != 1:
        raise PersistenceFailure()
    if starts[0].get("source") != IMAGE_SOURCE or starts[0].get("source_revision") != expected_revision:
        raise PersistenceFailure()
    identity = identities[0]
    for service in SERVICES:
        if not FULL_ID.fullmatch(str(identity.get(f"{service}_container_id", ""))):
            raise PersistenceFailure()
        if not IMAGE_ID.fullmatch(str(identity.get(f"{service}_image_id", ""))):
            raise PersistenceFailure()
        if identity.get(f"{service}_image_source") != IMAGE_SOURCE:
            raise PersistenceFailure()
        if identity.get(f"{service}_image_revision") != expected_revision:
            raise PersistenceFailure()
    for record in records:
        if record.get("record_type") in {"observation", "case_result"}:
            if not all(field in record for field in OBSERVATION_FIELDS):
                raise PersistenceFailure()
            if not all(_valid_http_value(record.get(field)) for field in OBSERVATION_FIELDS[:3]):
                raise PersistenceFailure()
            if not all(_valid_probe_exit(record.get(field)) for field in OBSERVATION_FIELDS[3:5]):
                raise PersistenceFailure()
            if not all(record.get(field) in HEALTH_STATES for field in OBSERVATION_FIELDS[5:9]):
                raise PersistenceFailure()
            if not all(record.get(field) in CONTAINER_STATES for field in OBSERVATION_FIELDS[9:]):
                raise PersistenceFailure()
    result_by_case: dict[str, dict[str, object]] = {}
    for result in results:
        case = result.get("case")
        if case not in REQUIRED_CASES or result.get("case_verdict") not in {"pass", "fail"}:
            raise PersistenceFailure()
        result_by_case[str(case)] = result
    if stage in {"pre_cleanup", "pre_final", "complete"}:
        if set(result_by_case) != set(REQUIRED_CASES):
            raise PersistenceFailure()
        if any(result_by_case[case].get("case_verdict") != "pass" for case in REQUIRED_CASES):
            raise PersistenceFailure()
    if stage in {"pre_final", "complete"} and not any(
        r.get("record_type") == "pre_cleanup_gate" and r.get("verdict") == "pass"
        for r in records
    ):
        raise PersistenceFailure()
    if stage in {"pre_final", "complete"} and not any(
        r.get("record_type") == "cleanup_result" and r.get("verdict") == "pass"
        for r in records
    ):
        raise PersistenceFailure()
    if stage == "complete" and not any(
        r.get("record_type") == "control_result" and r.get("verdict") == "pass"
        for r in records
    ):
        raise PersistenceFailure()
    return records


def parse_diagnostic_result(output: str, exit_code: int) -> str:
    """Keep only one numeric status or one fixed transport category."""
    lines = output.splitlines()
    if len(lines) != 1 or not _valid_http_value(lines[0]):
        return "transport_error=diagnostic_failure"
    if HTTP_STATUS.fullmatch(lines[0]) and exit_code == 0:
        return lines[0]
    category = HTTP_TRANSPORT.fullmatch(lines[0])
    if category and exit_code != 0 and category.group(1) in TRANSPORT_CATEGORIES:
        return lines[0]
    return "transport_error=diagnostic_failure"


def wait_for_observation(
    observe: Callable[[], dict[str, object]],
    ready: Callable[[dict[str, object]], bool],
    timeout: float,
    clock: Callable[[], float] = time.monotonic,
    pause: Callable[[float], None] = time.sleep,
) -> tuple[bool, dict[str, object], float]:
    """Poll to a fixed deadline and return the last complete per-leg observation."""
    start = clock()
    deadline = start + timeout
    last: dict[str, object] = {}
    while True:
        last = observe()
        elapsed = clock() - start
        if elapsed > timeout:
            return False, last, elapsed
        if ready(last):
            return True, last, elapsed
        remaining = deadline - clock()
        if remaining <= 0:
            return False, last, max(0.0, clock() - start)
        pause(min(1.0, remaining))


def run_with_cleanup(work: Callable[[], object], cleanup: Callable[[], bool]) -> object:
    """Always clean the private project, preserving a work failure as failure."""
    result: object = None
    work_error: BaseException | None = None
    try:
        result = work()
    except BaseException as exc:
        work_error = exc
    cleanup_error: BaseException | None = None
    try:
        cleanup_ok = cleanup()
    except BaseException as exc:
        cleanup_ok = False
        cleanup_error = exc
    if work_error is not None:
        raise work_error
    if cleanup_error is not None:
        raise ControlFailure("cleanup_failed") from cleanup_error
    if cleanup_ok is not True:
        raise ControlFailure("cleanup_failed")
    return result


class LinkEdgeControl:
    def __init__(self, evidence: EvidenceWriter):
        self.evidence = evidence
        self.project = f"link-edge-control-{uuid.uuid4().hex[:10]}"
        self.env = os.environ.copy()
        self.env.update(
            {
                "EDGE_IMAGE_SOURCE": IMAGE_SOURCE,
                "EDGE_IMAGE_REVISION": "",
                "LINK_EDGE_CONTROL_PROJECT": self.project,
            }
        )
        self.revision = ""
        self.container_ids: dict[str, str] = {}
        self.identity_recorded = False
        self.stack_maybe_started = False
        self.persistence_failed = False
        self.failure_recorded = False
        self.failure_code: str | None = None
        self.failure_phase = "startup"
        self.cleanup_ok = False
        self.pre_cleanup_ok = False
        self.attempts: dict[str, int] = {}

    def record(self, record: dict[str, object]) -> None:
        try:
            self.evidence.append({"timestamp_utc": utc_now(), **record})
        except PersistenceFailure:
            self.persistence_failed = True
            self.failure_code = "persistence_failure"
            raise

    def command(
        self,
        args: list[str],
        reason: str,
        timeout: float = 15.0,
        *,
        allow_failure: bool = False,
    ) -> subprocess.CompletedProcess[str]:
        try:
            result = subprocess.run(
                args,
                cwd=ROOT,
                env=self.env,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                timeout=timeout,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired) as exc:
            if allow_failure:
                return subprocess.CompletedProcess(args, 127, "", "")
            raise ControlFailure(reason) from exc
        if result.returncode and not allow_failure:
            raise ControlFailure(reason)
        return result

    def compose(
        self,
        *args: str,
        reason: str,
        timeout: float = 60.0,
        allow_failure: bool = False,
    ) -> subprocess.CompletedProcess[str]:
        return self.command(
            [
                "docker",
                "compose",
                "--env-file",
                "/dev/null",
                "--project-directory",
                str(FIXTURE.parent),
                "--file",
                str(FIXTURE),
                "--project-name",
                self.project,
                *args,
            ],
            reason,
            timeout,
            allow_failure=allow_failure,
        )

    def capture(self, args: list[str], reason: str, timeout: float = 10.0) -> str:
        result = self.command(args, reason, timeout)
        return result.stdout.strip()

    def preflight(self) -> None:
        self.failure_phase = "preflight"
        self.revision = self.capture(["git", "rev-parse", "HEAD"], "source_revision_unavailable")
        if not REVISION.fullmatch(self.revision):
            raise ControlFailure("source_revision_invalid")
        self.env["EDGE_IMAGE_REVISION"] = self.revision
        self.record(
            {
                "record_type": "control_start",
                "project": self.project,
                "source": IMAGE_SOURCE,
                "source_revision": self.revision,
                "linkprobe_timeout_seconds": 3,
                "diagnostic_http_timeout_seconds": 4,
                "unhealthy_transition_bound_seconds": 45,
                "recovery_bound_seconds": 15,
                "evidence_directory_mode": "0700",
                "evidence_file_mode": "0600",
            }
        )
        # The image context contains only these paths; their bytes must match the revision label.
        source_paths = [
            "go.mod",
            "go.sum",
            "deploy/link-edge/Dockerfile",
            "cmd/linklanding",
            "cmd/linkselector",
            "cmd/linkprobe",
            "internal/linklanding",
            "internal/linkselector",
        ]
        changed = self.command(
            ["git", "diff", "--quiet", "HEAD", "--", *source_paths],
            "source_tree_mismatch",
            allow_failure=True,
        )
        untracked = self.capture(
            ["git", "ls-files", "--others", "--exclude-standard", "--", *source_paths],
            "source_tree_mismatch",
        )
        if changed.returncode != 0 or untracked:
            raise ControlFailure("source_tree_mismatch")
        self.record(
            {
                "record_type": "source_revision",
                "source": IMAGE_SOURCE,
                "source_revision": self.revision,
                "fixture": FIXTURE.relative_to(ROOT).as_posix(),
            }
        )
        projection = self.compose("config", "--format", "json", reason="compose_config_failed")
        try:
            config = json.loads(projection.stdout)
        except json.JSONDecodeError as exc:
            raise ControlFailure("compose_config_invalid") from exc
        self.validate_projection(config)
        self.record({"record_type": "compose_config", "verdict": "pass"})

    def validate_projection(self, config: dict[str, object]) -> None:
        services = config.get("services")
        networks = config.get("networks")
        if not isinstance(services, dict) or set(services) != {"linklanding", "selector", "web", "probe"}:
            raise ControlFailure("compose_policy_failed")
        if not isinstance(networks, dict) or set(networks) != {"web-edge", "private-edge", "landing-publish"}:
            raise ControlFailure("compose_policy_failed")
        for service in services.values():
            if not isinstance(service, dict):
                raise ControlFailure("compose_policy_failed")
            if (
                service.get("read_only") is not True
                or service.get("cap_drop") != ["ALL"]
                or "no-new-privileges:true" not in service.get("security_opt", [])
                or service.get("user") != "65532:65532"
                or service.get("privileged") is True
                or service.get("volumes")
                or service.get("secrets")
                or service.get("env_file")
                or service.get("environment")
            ):
                raise ControlFailure("compose_policy_failed")
        if any(service.get("ports") for service in services.values()):
            raise ControlFailure("compose_policy_failed")
        expected_networks = {
            "linklanding": {"private-edge", "landing-publish"},
            "selector": {"web-edge", "private-edge"},
            "web": {"web-edge"},
            "probe": {"private-edge"},
        }
        for name, names in expected_networks.items():
            if set(services[name].get("networks", {})) != names:
                raise ControlFailure("compose_policy_failed")
        if services["selector"].get("depends_on", {}).get("linklanding", {}).get("condition") != "service_healthy":
            raise ControlFailure("compose_policy_failed")
        if services["probe"].get("depends_on", {}).get("selector", {}).get("condition") != "service_started":
            raise ControlFailure("compose_policy_failed")
        if (
            services["probe"].get("entrypoint") != ["/usr/local/bin/linkprobe"]
            or services["probe"].get("command") != ["hold"]
        ):
            raise ControlFailure("compose_policy_failed")
        if services["selector"].get("healthcheck", {}).get("test") != [
            "CMD",
            "/usr/local/bin/linkprobe",
            "selector",
        ]:
            raise ControlFailure("compose_policy_failed")
        if services["linklanding"].get("healthcheck", {}).get("test") != [
            "CMD",
            "/usr/local/bin/linkprobe",
            "landing",
        ]:
            raise ControlFailure("compose_policy_failed")
        if networks["web-edge"].get("internal") is not True or networks["private-edge"].get("internal") is not True:
            raise ControlFailure("compose_policy_failed")
        landing_publish = networks["landing-publish"]
        if (
            landing_publish.get("internal", False) is not False
            or landing_publish.get("driver") != "bridge"
            or landing_publish.get("driver_opts", {}).get(
                "com.docker.network.bridge.enable_ip_masquerade"
            )
            != "false"
        ):
            raise ControlFailure("compose_policy_failed")

    def build_and_start(self) -> None:
        self.failure_phase = "image_build"
        self.compose("build", reason="image_build_failed", timeout=600)
        self.record({"record_type": "image_build", "verdict": "pass"})
        self.failure_phase = "compose_up"
        self.stack_maybe_started = True
        self.compose("up", "--detach", "--no-build", reason="compose_up_failed", timeout=90)
        self.record({"record_type": "compose_up", "verdict": "pass"})
        self.capture_identities()

    def capture_identities(self) -> None:
        values: dict[str, object] = {"record_type": "identities", "source": IMAGE_SOURCE}
        for service in SERVICES:
            reference = self.compose("ps", "--quiet", service, reason="container_identity_unavailable").stdout.strip()
            if not re.fullmatch(r"[0-9a-f]{12,64}", reference):
                raise ControlFailure("container_identity_unavailable")
            full_id = self.capture(["bash", str(IDENTITY_GATE), "id", reference], "container_identity_invalid")
            if not FULL_ID.fullmatch(full_id):
                raise ControlFailure("container_identity_invalid")
            image_id = self.capture(
                ["docker", "inspect", "--format", "{{.Image}}", full_id],
                "image_identity_unavailable",
            )
            source = self.capture(
                ["docker", "image", "inspect", "--format", '{{index .Config.Labels "org.opencontainers.image.source"}}', image_id],
                "image_provenance_unavailable",
            )
            revision = self.capture(
                ["docker", "image", "inspect", "--format", '{{index .Config.Labels "org.opencontainers.image.revision"}}', image_id],
                "image_provenance_unavailable",
            )
            if not IMAGE_ID.fullmatch(image_id) or source != IMAGE_SOURCE or revision != self.revision:
                raise ControlFailure("image_provenance_mismatch")
            self.container_ids[service] = full_id
            values[f"{service}_container_id"] = full_id
            values[f"{service}_image_id"] = image_id
            values[f"{service}_image_source"] = source
            values[f"{service}_image_revision"] = revision
        self.record(values)
        self.identity_recorded = True

    def inspect_value(self, container_id: str, template: str) -> str:
        result = self.command(
            ["docker", "inspect", "--format", template, container_id],
            "observation_unavailable",
            timeout=5,
            allow_failure=True,
        )
        if result.returncode:
            return "unknown"
        return result.stdout.strip()

    def probe_exit(self, container_id: str | None, mode: str) -> int | str:
        if not container_id:
            return "unavailable"
        try:
            result = subprocess.run(
                ["docker", "exec", container_id, "/usr/local/bin/linkprobe", mode],
                cwd=ROOT,
                env=self.env,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                timeout=4,
                check=False,
            )
        except subprocess.TimeoutExpired:
            return "timeout"
        except OSError:
            return "probe_runtime_error"
        if result.returncode < 0:
            return "probe_runtime_error"
        if result.returncode > 255:
            return "probe_runtime_error"
        return result.returncode

    def diagnostic(self, target: str) -> str:
        result = self.compose(
            "exec",
            "--no-TTY",
            "probe",
            "/usr/local/bin/linkprobe",
            "diagnostic",
            target,
            reason="diagnostic_failed",
            timeout=5,
            allow_failure=True,
        )
        return parse_diagnostic_result(result.stdout, result.returncode)

    def observe(self, case: str, attempt: int) -> dict[str, object]:
        selector_id = self.container_ids.get("selector")
        landing_id = self.container_ids.get("linklanding")
        web_id = self.container_ids.get("web")
        probe_id = self.container_ids.get("probe")
        operations: dict[str, tuple[Callable[[], object], object]] = {
            "selector_synthetic_http": (lambda: self.diagnostic("selector-synthetic"), "transport_error=diagnostic_failure"),
            "selector_root_http": (lambda: self.diagnostic("selector-root"), "transport_error=diagnostic_failure"),
            "direct_landing_http": (lambda: self.diagnostic("direct-landing"), "transport_error=diagnostic_failure"),
            "selector_probe_exit": (lambda: self.probe_exit(selector_id, "selector"), "probe_runtime_error"),
            "landing_probe_exit": (lambda: self.probe_exit(landing_id, "landing"), "probe_runtime_error"),
            "selector_health": (
                lambda: self.inspect_value(selector_id or "", "{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}"),
                "unknown",
            ),
            "landing_health": (
                lambda: self.inspect_value(landing_id or "", "{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}"),
                "unknown",
            ),
            "web_health": (
                lambda: self.inspect_value(web_id or "", "{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}"),
                "unknown",
            ),
            "probe_health": (
                lambda: self.inspect_value(probe_id or "", "{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}"),
                "unknown",
            ),
            "selector_state": (
                lambda: self.inspect_value(selector_id or "", "{{.State.Status}}"),
                "unknown",
            ),
            "landing_state": (
                lambda: self.inspect_value(landing_id or "", "{{.State.Status}}"),
                "unknown",
            ),
            "web_state": (
                lambda: self.inspect_value(web_id or "", "{{.State.Status}}"),
                "unknown",
            ),
            "probe_state": (
                lambda: self.inspect_value(probe_id or "", "{{.State.Status}}"),
                "unknown",
            ),
        }
        values: dict[str, object] = {}
        with concurrent.futures.ThreadPoolExecutor(max_workers=len(operations)) as pool:
            futures = {key: pool.submit(operation) for key, (operation, _) in operations.items()}
        for key, future in futures.items():
                fallback = operations[key][1]
                try:
                    value = future.result(timeout=7)
                except BaseException:
                    value = fallback
                if key.endswith("_health") and value not in HEALTH_STATES:
                    value = "unknown"
                if key.endswith("_state") and value not in CONTAINER_STATES:
                    value = "unknown"
                if key.endswith("_http") and not _valid_http_value(value):
                    value = "transport_error=diagnostic_failure"
                if key.endswith("_probe_exit") and not _valid_probe_exit(value):
                    value = "probe_runtime_error"
                values[key] = value
        if values["selector_state"] != "running":
            values["selector_probe_exit"] = "unavailable"
        if values["landing_state"] != "running":
            values["landing_probe_exit"] = "unavailable"
        return {
            "record_type": "observation",
            "case": case,
            "attempt": attempt,
            "phase_elapsed_seconds": 0.0,
            **values,
        }

    def await_case(
        self,
        case: str,
        timeout: int,
        ready: Callable[[dict[str, object]], bool],
    ) -> tuple[dict[str, object], bool, str]:
        start = time.monotonic()
        self.attempts[case] = 0

        def sample() -> dict[str, object]:
            self.attempts[case] += 1
            observation = self.observe(case, self.attempts[case])
            observation["phase_elapsed_seconds"] = round(time.monotonic() - start, 2)
            self.record(observation)
            return observation

        ready_ok, last, elapsed = wait_for_observation(sample, ready, timeout)
        if elapsed > timeout:
            return last, False, "timeout"
        return last, ready_ok, "none" if ready_ok else "timeout"

    def case_result(self, case: str, observation: dict[str, object], passed: bool, reason: str) -> None:
        self.record(
            {
                **observation,
                "record_type": "case_result",
                "case": case,
                "case_verdict": "pass" if passed else "fail",
                "reason_code": reason,
            }
        )

    @staticmethod
    def healthy_ready(observation: dict[str, object]) -> bool:
        return (
            observation["selector_synthetic_http"] == "status=200"
            and observation["selector_root_http"] == "status=200"
            and observation["direct_landing_http"] == "status=200"
            and observation["selector_probe_exit"] == 0
            and observation["landing_probe_exit"] == 0
            and observation["selector_health"] == "healthy"
            and observation["landing_health"] == "healthy"
            and observation["selector_state"] == "running"
            and observation["landing_state"] == "running"
            and observation["web_state"] == "running"
            and observation["probe_state"] == "running"
        )

    @staticmethod
    def web_stopped_ready(observation: dict[str, object]) -> bool:
        return (
            observation["selector_synthetic_http"] == "status=200"
            and observation["selector_root_http"] == "status=502"
            and observation["direct_landing_http"] == "status=200"
            and isinstance(observation["selector_probe_exit"], int)
            and observation["selector_probe_exit"] != 0
            and observation["landing_probe_exit"] == 0
            and observation["selector_health"] == "unhealthy"
            and observation["landing_health"] == "healthy"
            and observation["selector_state"] == "running"
            and observation["landing_state"] == "running"
            and observation["web_state"] == "exited"
            and observation["probe_state"] == "running"
        )

    @staticmethod
    def landing_unavailable_ready(observation: dict[str, object]) -> bool:
        direct = str(observation["direct_landing_http"])
        category = HTTP_TRANSPORT.fullmatch(direct)
        return (
            observation["selector_synthetic_http"] == "status=503"
            and observation["selector_root_http"] == "status=200"
            and bool(category and category.group(1) in TRANSPORT_CATEGORIES - {"diagnostic_failure"})
            and isinstance(observation["selector_probe_exit"], int)
            and observation["selector_probe_exit"] != 0
            and observation["selector_health"] == "unhealthy"
            and observation["selector_state"] == "running"
            and observation["landing_state"] == "exited"
            and observation["web_state"] == "running"
            and observation["probe_state"] == "running"
        )

    def run_case(self, case: str, timeout: int, ready: Callable[[dict[str, object]], bool]) -> None:
        observation, passed, reason = self.await_case(case, timeout, ready)
        if not passed and reason == "none":
            reason = "assertion_failed"
        self.case_result(case, observation, passed, reason)
        if not passed:
            raise ControlFailure(f"{case}_failed")

    def gate_identities(self) -> None:
        if not self.identity_recorded:
            raise ControlFailure("container_identity_unavailable")
        for service in SERVICES:
            reference = self.compose("ps", "--quiet", service, reason="container_identity_unavailable").stdout.strip()
            self.command(
                ["bash", str(IDENTITY_GATE), "compare", self.container_ids[service], reference],
                "container_identity_changed",
            )
        self.record({"record_type": "identity_preservation_gate", "verdict": "pass"})

    def pre_cleanup_gate(self) -> None:
        self.gate_identities()
        validate_evidence(self.evidence.path, self.revision, "pre_cleanup")
        self.record({"record_type": "pre_cleanup_gate", "verdict": "pass"})
        self.pre_cleanup_ok = True

    def _persist_failure_before_cleanup(self, code: str) -> None:
        if self.failure_recorded or self.persistence_failed:
            return
        prior_results = [
            r for r in self._records_best_effort() if r.get("record_type") == "case_result"
        ]
        if self.stack_maybe_started and not (
            prior_results and prior_results[-1].get("case_verdict") == "fail"
        ):
            observation = self.observe("unexpected_failure", 1)
            self.record(observation)
            self.record(
                {
                    **observation,
                    "record_type": "case_result",
                    "case": "unexpected_failure",
                    "case_verdict": "fail",
                    "reason_code": code,
                }
            )
        self.record(
            {
                "record_type": "control_failure",
                "phase": self.failure_phase,
                "reason_code": code,
                "verdict": "fail",
            }
        )
        self.failure_recorded = True

    def _records_best_effort(self) -> list[dict[str, object]]:
        try:
            return _read_records(self.evidence.path)
        except PersistenceFailure:
            self.persistence_failed = True
            return []

    def work(self) -> None:
        try:
            self.preflight()
            self.failure_phase = "image_build"
            self.build_and_start()
            self.failure_phase = "baseline"
            self.run_case("baseline", 45, self.healthy_ready)
            self.failure_phase = "web_stop"
            self.compose("stop", "--timeout", "1", "web", reason="web_stop_failed")
            self.run_case("web_stopped", 45, self.web_stopped_ready)
            self.failure_phase = "web_recovery"
            self.compose("start", "web", reason="web_start_failed")
            self.run_case("web_recovery", 15, self.healthy_ready)
            self.failure_phase = "landing_stop"
            self.compose("stop", "--timeout", "1", "linklanding", reason="landing_stop_failed")
            self.run_case("landing_unavailable", 45, self.landing_unavailable_ready)
            self.failure_phase = "landing_recovery"
            self.compose("start", "linklanding", reason="landing_start_failed")
            self.run_case("landing_recovery", 15, self.healthy_ready)
            self.failure_phase = "identity_gate"
            self.pre_cleanup_gate()
        except PersistenceFailure:
            self.persistence_failed = True
            self.failure_code = "persistence_failure"
            raise
        except ControlFailure as exc:
            self.failure_code = exc.code
            self._persist_failure_before_cleanup(exc.code)
            raise
        except Exception as exc:
            self.failure_code = "unexpected_failure"
            self._persist_failure_before_cleanup("unexpected_failure")
            raise ControlFailure("unexpected_failure") from exc

    def cleanup(self) -> bool:
        self.failure_phase = "cleanup"
        if not self.persistence_failed:
            try:
                self.record({"record_type": "cleanup_start"})
            except PersistenceFailure:
                self.persistence_failed = True
        down = self.compose(
            "down",
            "--remove-orphans",
            "--rmi",
            "local",
            "--timeout",
            "3",
            reason="cleanup_failed",
            timeout=45,
            allow_failure=True,
        )
        down_ok = down.returncode == 0
        if not down_ok:
            containers = self.filtered_ids("docker", "ps", "--all", "--quiet", "--filter", f"label=com.docker.compose.project={self.project}")
            if containers:
                self.command(["docker", "rm", "--force", *containers], "cleanup_failed", timeout=30, allow_failure=True)
            networks = self.filtered_ids("docker", "network", "ls", "--quiet", "--filter", f"label=com.docker.compose.project={self.project}")
            if networks:
                self.command(["docker", "network", "rm", *networks], "cleanup_failed", timeout=30, allow_failure=True)
        for image in (f"{self.project}-linkselector:control", f"{self.project}-linklanding:control"):
            self.command(["docker", "image", "rm", image], "cleanup_failed", timeout=15, allow_failure=True)
        containers = self.filtered_ids("docker", "ps", "--all", "--quiet", "--filter", f"label=com.docker.compose.project={self.project}")
        networks = self.filtered_ids("docker", "network", "ls", "--quiet", "--filter", f"label=com.docker.compose.project={self.project}")
        volumes = self.filtered_ids("docker", "volume", "ls", "--quiet", "--filter", f"label=com.docker.compose.project={self.project}")
        images_left = any(
            self.command(["docker", "image", "inspect", image], "cleanup_failed", allow_failure=True).returncode == 0
            for image in (f"{self.project}-linkselector:control", f"{self.project}-linklanding:control")
        )
        cleanup_ok = down_ok and not containers and not networks and not volumes and not images_left
        self.cleanup_ok = cleanup_ok
        if not self.persistence_failed:
            try:
                self.record(
                    {
                        "record_type": "cleanup_result",
                        "verdict": "pass" if cleanup_ok else "fail",
                        "containers_remaining": len(containers),
                        "networks_remaining": len(networks),
                        "volumes_remaining": len(volumes),
                        "fixture_images_remaining": images_left,
                    }
                )
            except PersistenceFailure:
                self.persistence_failed = True
        return cleanup_ok and not self.persistence_failed

    def filtered_ids(self, *args: str) -> list[str]:
        result = self.command(list(args), "cleanup_failed", timeout=10, allow_failure=True)
        if result.returncode:
            return ["inspection_failed"]
        return [line for line in result.stdout.splitlines() if line.strip()]

    def finalize(self) -> bool:
        if self.persistence_failed:
            return False
        verdict = self.failure_code is None and self.pre_cleanup_ok and self.cleanup_ok
        if verdict:
            try:
                validate_evidence(self.evidence.path, self.revision, "pre_final")
            except PersistenceFailure:
                verdict = False
                self.failure_code = "evidence_validation_failed"
        record = {
            "record_type": "control_result",
            "verdict": "pass" if verdict else "fail",
            "reason_code": "none" if verdict else (self.failure_code or "control_failed"),
        }
        try:
            self.record(record)
            if verdict:
                validate_evidence(self.evidence.path, self.revision, "complete")
        except PersistenceFailure:
            self.persistence_failed = True
            return False
        return verdict

    def execute(self) -> tuple[bool, str]:
        try:
            run_with_cleanup(self.work, self.cleanup)
        except ControlFailure as exc:
            if self.failure_code is None:
                self.failure_code = exc.code
        except BaseException:
            self.failure_code = "unexpected_failure"
        result = self.finalize()
        if self.persistence_failed:
            return False, "persistence_failure"
        return result, "none" if result else (self.failure_code or "control_failed")


def default_evidence_directory() -> Path:
    parent = Path(os.environ.get("RUNNER_TEMP", tempfile.gettempdir()))
    return parent / f"link-edge-control-{datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')}-{uuid.uuid4().hex[:8]}"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--evidence-dir",
        type=Path,
        default=default_evidence_directory(),
        help="new private directory for durable JSONL evidence (default: a unique temporary path)",
    )
    args = parser.parse_args(argv)
    try:
        evidence = EvidenceWriter(args.evidence_dir)
    except PersistenceFailure:
        print("control=fail reason=persistence_failure", file=sys.stderr)
        return 1
    runner = LinkEdgeControl(evidence)
    try:
        passed, reason = runner.execute()
    except BaseException:
        passed, reason = False, "unexpected_failure"
    if not evidence.close():
        passed, reason = False, "persistence_failure"
    if passed:
        print(f"control=pass evidence={evidence.path}")
        return 0
    print(f"control=fail reason={reason} evidence={evidence.path}", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
