#!/usr/bin/env python3
"""Exercise QA bypass token rotation against private temporary fixtures."""

from __future__ import annotations

import contextlib
import errno
import importlib.util
import io
import os
import pathlib
import re
import shlex
import socket
import stat
import subprocess
import sys
import tempfile
import textwrap
import threading
import unittest
from unittest import mock


SCRIPT = pathlib.Path(__file__).with_name("rotate-qa-bypass-token.py")
SPEC = importlib.util.spec_from_file_location("rotate_qa_bypass_token", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
ROTATION = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ROTATION)
CONTROL_SCRIPT = pathlib.Path(__file__).with_name("haproxy-control.py")
CONTROL_SPEC = importlib.util.spec_from_file_location("demo_haproxy_control", CONTROL_SCRIPT)
assert CONTROL_SPEC is not None and CONTROL_SPEC.loader is not None
HAPROXY_CONTROL = importlib.util.module_from_spec(CONTROL_SPEC)
CONTROL_SPEC.loader.exec_module(HAPROXY_CONTROL)


class RotationErrorTests(unittest.TestCase):
    def test_checked_file_os_errors_identify_cause_without_exposing_path(self) -> None:
        for code in (errno.EACCES, errno.ENOENT, errno.ELOOP):
            with self.subTest(code=code):
                error = OSError(code, os.strerror(code), "secret-value")
                with mock.patch.object(ROTATION.os, "open", side_effect=error):
                    with self.assertRaises(ROTATION.RotationError) as caught:
                        ROTATION._read_checked_file(pathlib.Path("unused"), "token", 0, 0, 0o600)
                self.assertIn(os.strerror(code), str(caught.exception))
                self.assertNotIn("secret-value", str(caught.exception))

    def test_snapshot_verification_failures_remove_partial_snapshot(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = pathlib.Path(temporary) / "snapshot"
            results = (
                ROTATION.RotationError("snapshot-unavailable"),
                (b"wrong", mock.Mock(st_mode=0o100600)),
            )
            for read_result in results:
                with self.subTest(read_result=read_result):
                    read_mock = {"side_effect": read_result} if isinstance(
                        read_result, Exception
                    ) else {"return_value": read_result}
                    with mock.patch.object(ROTATION.os, "fchown"), mock.patch.object(
                        ROTATION, "_read_checked_file", **read_mock
                    ):
                        with self.assertRaises(ROTATION.RotationError):
                            ROTATION._snapshot(path, b"secret-value")
                    self.assertFalse(path.exists())


@unittest.skipUnless(os.geteuid() == 0, "fixture ownership checks require root")
class RotateQABypassTokenTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="teagram-qa-bypass-rotation-")
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        self.secret_dir = self.root / "secrets"
        self.secret_dir.mkdir(mode=0o700)
        self.backup_dir = self.root / "backups" / "qa-bypass"
        self.backup_dir.parent.mkdir(mode=0o755)
        os.chown(self.root, 0, 0)
        os.chmod(self.root, 0o700)
        os.chown(self.secret_dir, 0, 0)
        os.chmod(self.secret_dir, 0o700)
        os.chown(self.backup_dir.parent, 0, 0)
        os.chmod(self.backup_dir.parent, 0o755)

        self.token_file = self.secret_dir / "turnstile-qa-bypass-token"
        self.siteverify_file = self.secret_dir / "turnstile-secret-key"
        self.env_file = self.root / ".env"
        self.old_token = "old-demo-bypass-token-that-is-long-enough"
        self.siteverify_secret = "b" * 64
        self.site_key = "demo-turnstile-site-key-that-is-long-enough"
        self._write_runtime_secret(self.token_file, self.old_token)
        self._write_runtime_secret(self.siteverify_file, self.siteverify_secret)
        self.env_file.write_text(
            f"TEAGRAM_DEMO_TURNSTILE_SITE_KEY={self.site_key}\n", encoding="utf-8"
        )
        os.chown(self.env_file, 0, 0)
        os.chmod(self.env_file, 0o600)

    @staticmethod
    def _write_runtime_secret(path: pathlib.Path, value: str) -> None:
        path.write_text(f"{value}\n", encoding="ascii")
        os.chown(path, 0, 65532)
        os.chmod(path, 0o440)

    def _rotate(self) -> tuple[pathlib.Path, int]:
        return ROTATION.rotate_token(
            self.secret_dir,
            self.token_file,
            self.siteverify_file,
            self.env_file,
            self.backup_dir,
        )

    def test_rotates_and_snapshots_without_printing_either_value(self) -> None:
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            backup, length = self._rotate()

        new_token = self.token_file.read_text(encoding="ascii").strip()
        self.assertEqual(length, 64)
        self.assertRegex(new_token, re.compile(r"[0-9a-f]{64}\Z"))
        self.assertNotEqual(new_token, self.old_token)
        self.assertEqual(backup.read_text(encoding="ascii"), f"{self.old_token}\n")
        self.assertEqual(output.getvalue(), "")
        self.assertNotIn(self.old_token, output.getvalue())
        self.assertNotIn(new_token, output.getvalue())
        backup_stat = backup.stat()
        self.assertEqual(
            (backup_stat.st_uid, backup_stat.st_gid, stat.S_IMODE(backup_stat.st_mode)),
            (0, 0, 0o600),
        )
        token_stat = self.token_file.stat()
        self.assertEqual(
            (token_stat.st_uid, token_stat.st_gid, stat.S_IMODE(token_stat.st_mode)),
            (0, 65532, 0o440),
        )

    def test_rejects_insecure_current_token_without_changing_it(self) -> None:
        os.chmod(self.token_file, 0o644)

        with self.assertRaisesRegex(ROTATION.RotationError, "qa-bypass-token-owner-group-or-mode"):
            self._rotate()

        self.assertEqual(self.token_file.read_text(encoding="ascii"), f"{self.old_token}\n")
        self.assertFalse(self.backup_dir.exists())

    def test_command_output_contains_only_snapshot_path_and_length(self) -> None:
        names = ("SECRET_DIR", "TOKEN_FILE", "SITEVERIFY_FILE", "ENV_FILE", "BACKUP_DIR")
        original_paths = {name: getattr(ROTATION, name) for name in names}
        self.addCleanup(
            lambda: [setattr(ROTATION, name, value) for name, value in original_paths.items()]
        )
        ROTATION.SECRET_DIR = self.secret_dir
        ROTATION.TOKEN_FILE = self.token_file
        ROTATION.SITEVERIFY_FILE = self.siteverify_file
        ROTATION.ENV_FILE = self.env_file
        ROTATION.BACKUP_DIR = self.backup_dir
        marker = self.root / "rotation-started"
        original_argv = sys.argv
        sys.argv = [str(SCRIPT), str(marker)]
        self.addCleanup(setattr, sys, "argv", original_argv)
        output = io.StringIO()

        with contextlib.redirect_stdout(output):
            self.assertEqual(ROTATION.main(), 0)

        new_token = self.token_file.read_text(encoding="ascii").strip()
        self.assertIn("token_length=64", output.getvalue())
        self.assertIn("snapshot=", output.getvalue())
        self.assertTrue(marker.is_file())
        self.assertEqual(marker.read_bytes(), b"")
        self.assertNotIn(self.old_token, output.getvalue())
        self.assertNotIn(new_token, output.getvalue())

    def test_rejects_reuse_of_the_siteverify_secret(self) -> None:
        original_generator = ROTATION.secrets.token_hex

        def deterministic_token(size: int) -> str:
            return "0" * (size * 2) if size == 4 else self.siteverify_secret

        ROTATION.secrets.token_hex = deterministic_token
        self.addCleanup(setattr, ROTATION.secrets, "token_hex", original_generator)

        with self.assertRaisesRegex(ROTATION.RotationError, "generated-token-invalid"):
            self._rotate()

        self.assertEqual(self.token_file.read_text(encoding="ascii"), f"{self.old_token}\n")

    def test_accepts_short_turnstile_keys_but_keeps_qa_token_length(self) -> None:
        self.site_key = "short-site-key"
        self.siteverify_secret = "short-secret"
        self._write_runtime_secret(self.siteverify_file, self.siteverify_secret)
        self.env_file.write_text(
            f"TEAGRAM_DEMO_TURNSTILE_SITE_KEY={self.site_key}\n", encoding="utf-8"
        )

        _, length = self._rotate()

        self.assertEqual(length, 64)
        self.assertRegex(
            self.token_file.read_text(encoding="ascii").strip(),
            re.compile(r"[0-9a-f]{64}\Z"),
        )

    def test_rejects_qa_token_below_minimum_length(self) -> None:
        short_token = "short-token"
        self._write_runtime_secret(self.token_file, short_token)

        with self.assertRaisesRegex(ROTATION.RotationError, "qa-bypass-token-format"):
            self._rotate()

        self.assertEqual(self.token_file.read_text(encoding="ascii"), f"{short_token}\n")
        self.assertFalse(self.backup_dir.exists())

    def test_rejects_existing_qa_token_reused_as_site_key(self) -> None:
        self.site_key = self.old_token
        self.env_file.write_text(
            f"TEAGRAM_DEMO_TURNSTILE_SITE_KEY={self.site_key}\n", encoding="utf-8"
        )

        with self.assertRaisesRegex(ROTATION.RotationError, "existing-token-reuse"):
            self._rotate()

        self.assertEqual(self.token_file.read_text(encoding="ascii"), f"{self.old_token}\n")
        self.assertFalse(self.backup_dir.exists())


class HaproxyControlTests(unittest.TestCase):
    def test_shutdown_sessions_covers_http_and_mtproto_backends(self) -> None:
        with tempfile.TemporaryDirectory(prefix="teagram-haproxy-shutdown-") as temporary:
            socket_path = pathlib.Path(temporary) / "admin.sock"
            admin_socket = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            admin_socket.bind(str(socket_path))
            admin_socket.listen(2)
            admin_socket.settimeout(3)
            commands: list[str] = []
            errors: list[OSError] = []

            def serve_commands() -> None:
                try:
                    for _ in range(2):
                        client, _ = admin_socket.accept()
                        with client:
                            commands.append(client.recv(1024).decode("ascii").strip())
                            client.sendall(b"\n")
                except OSError as error:
                    errors.append(error)
                finally:
                    admin_socket.close()

            thread = threading.Thread(target=serve_commands)
            thread.start()
            try:
                HAPROXY_CONTROL.slot_command(
                    socket_path, "shutdown-sessions", "teagramd-next"
                )
            finally:
                thread.join(timeout=4)

            self.assertFalse(thread.is_alive())
            self.assertEqual(errors, [])
            self.assertEqual(
                commands,
                [
                    "shutdown sessions server teagramd/teagramd-next",
                    "shutdown sessions server teagramd_mtproto/teagramd-next-mtproto",
                ],
            )

    def test_shutdown_sessions_rejects_unknown_server_reply(self) -> None:
        with tempfile.TemporaryDirectory(prefix="teagram-haproxy-error-") as temporary:
            socket_path = pathlib.Path(temporary) / "admin.sock"
            admin_socket = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            admin_socket.bind(str(socket_path))
            admin_socket.listen(1)
            admin_socket.settimeout(3)

            def serve_error() -> None:
                try:
                    client, _ = admin_socket.accept()
                    with client:
                        client.recv(1024)
                        client.sendall(b"No such server.\n\n")
                finally:
                    admin_socket.close()

            thread = threading.Thread(target=serve_error)
            thread.start()
            error_output = io.StringIO()
            try:
                with contextlib.redirect_stderr(error_output):
                    with self.assertRaises(SystemExit):
                        HAPROXY_CONTROL.slot_command(socket_path, "shutdown-sessions", "teagramd-next")
            finally:
                thread.join(timeout=4)
            self.assertFalse(thread.is_alive())
            self.assertIn("HAProxy rejected runtime command", error_output.getvalue())

    def test_wait_healthy_rejects_drained_status_without_opt_in(self) -> None:
        clock = iter((0.0, 0.0, 2.0))
        with mock.patch.object(HAPROXY_CONTROL, "server_row", return_value={
            "status": "DRAIN",
            "check_status": "L7OK",
        }), contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):
                HAPROXY_CONTROL.wait_healthy(
                    pathlib.Path("unused.sock"), "teagramd-next", 1,
                    monotonic=lambda: next(clock), sleep=lambda _seconds: None,
                )

    def test_wait_healthy_accepts_healthy_drained_server_rows(self) -> None:
        with tempfile.TemporaryDirectory(prefix="teagram-haproxy-drain-") as temporary:
            socket_path = pathlib.Path(temporary) / "admin.sock"
            admin_socket = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            admin_socket.bind(str(socket_path))
            admin_socket.listen(2)
            admin_socket.settimeout(3)
            errors: list[OSError] = []
            stats = (
                b"# pxname,svname,status,check_status\n"
                b"teagramd,teagramd-next,DRAIN,L7OK\n"
                b"teagramd_mtproto,teagramd-next-mtproto,DRAIN,L7OK\n"
            )

            def serve_stats() -> None:
                try:
                    for _ in range(2):
                        client, _ = admin_socket.accept()
                        with client:
                            client.recv(1024)
                            client.sendall(stats)
                except OSError as error:
                    errors.append(error)
                finally:
                    admin_socket.close()

            thread = threading.Thread(target=serve_stats)
            thread.start()
            original_argv = sys.argv
            sys.argv = [
                str(CONTROL_SCRIPT),
                "--socket",
                str(socket_path),
                "wait-healthy",
                "teagramd-next",
                "--timeout",
                "2",
                "--allow-drained",
            ]
            output = io.StringIO()
            try:
                with contextlib.redirect_stdout(output):
                    HAPROXY_CONTROL.main()
            finally:
                sys.argv = original_argv
                thread.join(timeout=4)

            self.assertFalse(thread.is_alive())
            self.assertEqual(errors, [])
            self.assertIn("HAProxy server checks=passed slot=teagramd-next", output.getvalue())


class RotateQABypassOperationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="teagram-qa-bypass-operation-")
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        self.repo = self.root / "demo"
        self.repo.mkdir()
        (self.repo / ".git").mkdir()
        (self.repo / ".state" / "teagram-demo").mkdir(parents=True)
        demo_dir = self.repo / "deploy" / "teagram-demo"
        (demo_dir / "nginx-config").mkdir(parents=True)
        tracked_files = (
            "deploy/teagram-demo/demo-common.sh",
            "deploy/teagram-demo/compose.yaml",
            "deploy/teagram-demo/haproxy.cfg",
            "deploy/teagram-demo/render-haproxy-config.py",
            "deploy/teagram-demo/nginx-config/nginx.conf",
            "deploy/teagram-demo/validate-env.py",
            "deploy/teagram-demo/haproxy-control.py",
            "deploy/teagram-demo/rotate-qa-bypass-token.py",
        )
        for relative_path in tracked_files:
            path = self.repo / relative_path
            path.parent.mkdir(parents=True, exist_ok=True)
            path.touch()
        (demo_dir / ".env").touch()

        operation_path = pathlib.Path(__file__).with_name("ops") / "rotate-turnstile-qa-bypass-token.sh"
        operation = operation_path.read_text(encoding="utf-8")
        repo_assignment = "readonly REPO=/opt/teagram-demo"
        self.assertEqual(operation.count(repo_assignment), 1)
        operation = operation.replace(
            repo_assignment, f"readonly REPO={shlex.quote(str(self.repo))}"
        )
        root_guard = '[[ "$EUID" -eq 0 ]] || fail \'operation must run as root\''
        self.assertEqual(operation.count(root_guard), 1)
        operation = operation.replace(root_guard, ": # bypassed only in this private fixture copy")
        self.operation = self.root / "rotate-operation.sh"
        self.operation.write_text(operation, encoding="utf-8")
        self.operation.chmod(0o700)

        self.fake_bin = self.root / "bin"
        self.fake_bin.mkdir()
        self.calls = self.root / "calls"
        self.current_container = self.root / "current-container"
        self.current_container.write_text("old-container\n", encoding="ascii")
        self.haproxy_state = self.root / "haproxy-state"
        self.haproxy_state.write_text("UP\n", encoding="ascii")
        self.token_file = self.root / "installed-token"
        self.old_token = "compromised-demo-token-fixture"
        self.new_token = "f" * 64
        self.token_file.write_text(f"{self.old_token}\n", encoding="ascii")
        self.rotation_ran = self.root / "rotation-ran"

        self._write_executable(
            "git",
            r"""#!/usr/bin/env bash
set -euo pipefail
if [[ ${1:-} == --no-optional-locks ]]; then shift; fi
if [[ ${1:-} == -C ]]; then shift 2; fi
case ${1:-} in
  merge-base)
    [[ ${FIXTURE_FAIL_MERGE_BASE:-0} != 1 ]] || exit 1
    exit 0
    ;;
  diff) exit 0 ;;
  rev-parse) printf '%040d\n' 1 ;;
  hash-object)
    case ${2:-} in
      */demo-common.sh)
        [[ ${FIXTURE_SOURCE_DRIFT:-0} != 1 ]] || { printf '%040d\n' 2; exit 0; }
        ;;
      */rotate-qa-bypass-token.py)
        [[ ${FIXTURE_ROTATOR_IDENTITY_MISMATCH:-0} != 1 ]] || { printf '%040d\n' 2; exit 0; }
        ;;
    esac
    printf '%040d\n' 1
    ;;
  show) printf 'fixture rotator\n' ;;
  *) exit 90 ;;
esac
""",
        )
        self._write_executable(
            "python3",
            r"""#!/usr/bin/env bash
set -euo pipefail
case ${1##*/} in
  validate-env.py) exit 0 ;;
  rotate-qa-bypass-token.py)
    : > "$2"
    chmod 0600 "$2"
    printf '%s\n' "$TEST_NEW_TOKEN" > "$TEST_TOKEN_FILE"
    : > "$TEST_ROTATION_RAN"
    [[ ${FIXTURE_FAIL_ROTATOR:-0} != 1 ]] || exit 1
    printf '%s\n' 'rotation=pass token_length=64 snapshot=/var/backups/teagram-demo-turnstile-qa-bypass/turnstile-qa-bypass-token.fixture.abcdef01'
    ;;
  *) exit 91 ;;
esac
""",
        )
        self._write_executable(
            "docker",
            r"""#!/usr/bin/env bash
set -euo pipefail
log() { printf '%s\n' "$*" >> "$TEST_CALLS"; }
case ${1:-} in
  image) printf '%s\n' 'sha256:fixture-image' ;;
  inspect)
    case ${3:-} in
      '{{.Image}}') printf '%s\n' 'sha256:fixture-image' ;;
      '{{json .Config.Env}}') printf '%s\n' '["TG_TURNSTILE_QA_BYPASS_TOKEN_FILE=/run/secrets/teagram_demo_turnstile_qa_bypass"]' ;;
      *) exit 92 ;;
    esac
    ;;
  ps) cat "$TEST_CURRENT_CONTAINER" ;;
  stop)
    log docker-stop
    [[ ${FIXTURE_FAIL_DOCKER_STOP:-0} != 1 ]] || exit 1
    : > "$TEST_CURRENT_CONTAINER"
    ;;
  kill)
    log docker-kill
    [[ ${FIXTURE_FAIL_DOCKER_KILL:-0} != 1 ]] || exit 1
    : > "$TEST_CURRENT_CONTAINER"
    ;;
  *) exit 93 ;;
esac
""",
        )
        self._write_executable(
            "common.sh",
            textwrap.dedent(
                r"""#!/usr/bin/env bash
DEMO_ENV_FILE="$REPO/deploy/teagram-demo/.env"
fixture_log() { printf '%s\n' "$*" >> "$TEST_CALLS"; }
demo_active_server() { printf '%s\n' teagramd-next; }
demo_inactive_server() { printf '%s\n' teagramd; }
demo_active_image_ref() { printf '%s\n' demo:fixture; }
demo_render_config() {
  cat > "$1" <<'JSON'
{"services":{"teagramd":{"environment":{"TG_TURNSTILE_QA_BYPASS_TOKEN_FILE":"/run/secrets/teagram_demo_turnstile_qa_bypass"}},"teagramd-next":{"environment":{"TG_TURNSTILE_QA_BYPASS_TOKEN_FILE":"/run/secrets/teagram_demo_turnstile_qa_bypass"}}},"secrets":{"teagram_demo_turnstile_qa_bypass":{"file":"/var/lib/teagram-demo/secrets/turnstile-qa-bypass-token"}}}
JSON
}
demo_compose() {
  case ${1:-} in
    ps)
      if [[ ${2:-} == -q && ${3:-} == teagramd-next ]]; then
        cat "$TEST_CURRENT_CONTAINER"
      elif [[ ${2:-} == --status ]]; then
        cat "$TEST_CURRENT_CONTAINER"
      fi
      ;;
    up)
      fixture_log compose-up
      [[ ${FIXTURE_FAIL_UP:-0} != 1 ]] || return 1
      printf '%s\n' new-container > "$TEST_CURRENT_CONTAINER"
      ;;
    stop)
      fixture_log compose-stop
      [[ ${FIXTURE_FAIL_COMPOSE_STOP:-0} != 1 ]] || return 1
      : > "$TEST_CURRENT_CONTAINER"
      ;;
    *) return 94 ;;
  esac
}
demo_haproxy_control() {
  fixture_log "haproxy-$*"
  case ${1:-} in
    wait-healthy)
      [[ ${FIXTURE_FAIL_HAPROXY:-0} != 1 ]] || return 1
      if [[ $(cat "$TEST_HAPROXY_STATE") == DRAIN && " $* " != *" --allow-drained "* ]]; then
        return 1
      fi
      ;;
    drain)
      [[ ${FIXTURE_FAIL_DRAIN:-0} != 1 ]] || return 1
      printf '%s\n' DRAIN > "$TEST_HAPROXY_STATE"
      ;;
    shutdown-sessions)
      [[ ${FIXTURE_FAIL_SHUTDOWN_SESSIONS:-0} != 1 ]] || return 1
      ;;
    enable) printf '%s\n' UP > "$TEST_HAPROXY_STATE" ;;
    *) return 95 ;;
  esac
  return 0
}
"""
            ),
        )
        (demo_dir / "demo-common.sh").write_text(
            f"source {shlex.quote(str(self.fake_bin / 'common.sh'))}\n", encoding="utf-8"
        )

    def _write_executable(self, name: str, content: str) -> None:
        path = self.fake_bin / name
        path.write_text(content, encoding="utf-8")
        path.chmod(0o700)

    def _run_operation(self, **flags: str) -> subprocess.CompletedProcess[str]:
        environment = os.environ.copy()
        environment.update(
            {
                "PATH": f"{self.fake_bin}{os.pathsep}{environment['PATH']}",
                "OPS_SHA": "a" * 40,
                "OPS_NAME": "rotate-turnstile-qa-bypass-token",
                "TEST_CALLS": str(self.calls),
                "TEST_CURRENT_CONTAINER": str(self.current_container),
                "TEST_HAPROXY_STATE": str(self.haproxy_state),
                "TEST_TOKEN_FILE": str(self.token_file),
                "TEST_NEW_TOKEN": self.new_token,
                "TEST_ROTATION_RAN": str(self.rotation_ran),
            }
        )
        environment.update(flags)
        return subprocess.run(
            ["/bin/bash", str(self.operation)],
            check=False,
            capture_output=True,
            text=True,
            env=environment,
            timeout=20,
        )

    def _assert_rotated_without_leaking(self, result: subprocess.CompletedProcess[str]) -> None:
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(self.rotation_ran.is_file())
        self.assertEqual(self.token_file.read_text(encoding="ascii"), f"{self.new_token}\n")
        self.assertNotIn(self.old_token, result.stdout + result.stderr)
        self.assertNotIn(self.new_token, result.stdout + result.stderr)

    def _assert_rejected_before_rotation(self, result: subprocess.CompletedProcess[str]) -> None:
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.rotation_ran.exists())
        self.assertEqual(self.token_file.read_text(encoding="ascii"), f"{self.old_token}\n")
        calls = self.calls.read_text(encoding="utf-8") if self.calls.exists() else ""
        self.assertNotIn("compose-up", calls)

    def test_source_drift_rejects_before_rotation_or_compose_up(self) -> None:
        result = self._run_operation(FIXTURE_SOURCE_DRIFT="1")

        self._assert_rejected_before_rotation(result)

    def test_origin_main_guard_rejects_before_rotation_or_compose_up(self) -> None:
        result = self._run_operation(FIXTURE_FAIL_MERGE_BASE="1")

        self._assert_rejected_before_rotation(result)

    def test_rotator_identity_guard_rejects_before_rotation_or_compose_up(self) -> None:
        result = self._run_operation(FIXTURE_ROTATOR_IDENTITY_MISMATCH="1")

        self._assert_rejected_before_rotation(result)

    def test_successful_recreation_reports_the_replaced_container(self) -> None:
        result = self._run_operation()

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.token_file.read_text(encoding="ascii"), f"{self.new_token}\n")
        self.assertIn("old_container_replaced=true", result.stdout)
        calls = self.calls.read_text(encoding="utf-8")
        self.assertIn("compose-up", calls)
        self.assertIn("haproxy-enable teagramd-next", calls)
        self.assertIn("--allow-drained", calls)
        self.assertNotIn("compose-stop", calls)
        self.assertEqual(self.current_container.read_text(encoding="ascii"), "new-container\n")
        self.assertNotIn(self.old_token, result.stdout + result.stderr)
        self.assertNotIn(self.new_token, result.stdout + result.stderr)

    def test_rotator_failure_after_install_stops_without_starting_server(self) -> None:
        result = self._run_operation(FIXTURE_FAIL_ROTATOR="1")

        self._assert_rotated_without_leaking(result)
        calls = self.calls.read_text(encoding="utf-8").splitlines()
        self.assertIn("compose-stop", calls)
        self.assertNotIn("compose-up", calls)
        self.assertIn("server stopped", result.stderr)
        self.assertEqual(self.current_container.read_text(encoding="ascii"), "")

    def test_compose_startup_readiness_failure_stops_old_container(self) -> None:
        result = self._run_operation(FIXTURE_FAIL_UP="1")

        self._assert_rotated_without_leaking(result)
        self.assertIn("compose-stop", self.calls.read_text(encoding="utf-8"))
        self.assertIn("server stopped", result.stderr)
        self.assertEqual(self.current_container.read_text(encoding="ascii"), "")

    def test_haproxy_readiness_failure_stops_server_after_token_install(self) -> None:
        result = self._run_operation(FIXTURE_FAIL_HAPROXY="1")

        self._assert_rotated_without_leaking(result)
        self.assertIn("compose-stop", self.calls.read_text(encoding="utf-8"))
        self.assertIn("server stopped", result.stderr)
        self.assertEqual(self.current_container.read_text(encoding="ascii"), "")

    def test_compose_stop_failure_falls_back_to_docker_stop(self) -> None:
        result = self._run_operation(
            FIXTURE_FAIL_HAPROXY="1", FIXTURE_FAIL_COMPOSE_STOP="1"
        )

        self._assert_rotated_without_leaking(result)
        self.assertIn("docker-stop", self.calls.read_text(encoding="utf-8"))
        self.assertIn("server stopped", result.stderr)
        self.assertEqual(self.current_container.read_text(encoding="ascii"), "")

    def test_docker_stop_failure_falls_back_to_kill(self) -> None:
        result = self._run_operation(
            FIXTURE_FAIL_HAPROXY="1",
            FIXTURE_FAIL_COMPOSE_STOP="1",
            FIXTURE_FAIL_DOCKER_STOP="1",
        )

        self._assert_rotated_without_leaking(result)
        calls = self.calls.read_text(encoding="utf-8")
        self.assertIn("docker-stop", calls)
        self.assertIn("docker-kill", calls)
        self.assertIn("server stopped", result.stderr)
        self.assertEqual(self.current_container.read_text(encoding="ascii"), "")

    def test_stop_failures_drain_old_server_and_shutdown_sessions(self) -> None:
        result = self._run_operation(
            FIXTURE_FAIL_UP="1",
            FIXTURE_FAIL_COMPOSE_STOP="1",
            FIXTURE_FAIL_DOCKER_STOP="1",
            FIXTURE_FAIL_DOCKER_KILL="1",
        )

        self._assert_rotated_without_leaking(result)
        calls = self.calls.read_text(encoding="utf-8").splitlines()
        self.assertIn("haproxy-drain teagramd-next", calls)
        self.assertIn("haproxy-shutdown-sessions teagramd-next", calls)
        self.assertLess(
            calls.index("haproxy-drain teagramd-next"),
            calls.index("haproxy-shutdown-sessions teagramd-next"),
        )
        self.assertIn("server drained and existing sessions shut down", result.stderr)
        self.assertIn("old token may still be live", result.stderr)
        self.assertEqual(self.current_container.read_text(encoding="ascii"), "old-container\n")
        self.assertEqual(self.haproxy_state.read_text(encoding="ascii"), "DRAIN\n")

    def test_drain_with_failed_session_shutdown_warns_old_token_may_be_live(self) -> None:
        result = self._run_operation(
            FIXTURE_FAIL_UP="1",
            FIXTURE_FAIL_COMPOSE_STOP="1",
            FIXTURE_FAIL_DOCKER_STOP="1",
            FIXTURE_FAIL_DOCKER_KILL="1",
            FIXTURE_FAIL_SHUTDOWN_SESSIONS="1",
        )

        self._assert_rotated_without_leaking(result)
        self.assertIn("haproxy-shutdown-sessions teagramd-next", self.calls.read_text(encoding="utf-8"))
        self.assertIn("existing sessions could not be shut down", result.stderr)
        self.assertIn("old token may still be live", result.stderr)
        self.assertEqual(self.current_container.read_text(encoding="ascii"), "old-container\n")

    def test_rerun_recovers_a_drained_healthy_slot_before_enabling_it(self) -> None:
        first = self._run_operation(
            FIXTURE_FAIL_UP="1",
            FIXTURE_FAIL_COMPOSE_STOP="1",
            FIXTURE_FAIL_DOCKER_STOP="1",
            FIXTURE_FAIL_DOCKER_KILL="1",
        )
        self._assert_rotated_without_leaking(first)
        self.assertEqual(self.haproxy_state.read_text(encoding="ascii"), "DRAIN\n")
        self.calls.write_text("", encoding="utf-8")

        retry = self._run_operation()

        self.assertEqual(retry.returncode, 0, retry.stderr)
        self.assertEqual(self.haproxy_state.read_text(encoding="ascii"), "UP\n")
        calls = self.calls.read_text(encoding="utf-8").splitlines()
        health_check = next(i for i, call in enumerate(calls) if call.startswith("haproxy-wait-healthy"))
        enable = next(i for i, call in enumerate(calls) if call == "haproxy-enable teagramd-next")
        self.assertIn("--allow-drained", calls[health_check])
        self.assertLess(health_check, enable)

    def test_failed_stop_and_drain_are_reported_as_unconfirmed(self) -> None:
        result = self._run_operation(
            FIXTURE_FAIL_UP="1",
            FIXTURE_FAIL_COMPOSE_STOP="1",
            FIXTURE_FAIL_DOCKER_STOP="1",
            FIXTURE_FAIL_DOCKER_KILL="1",
            FIXTURE_FAIL_DRAIN="1",
        )

        self._assert_rotated_without_leaking(result)
        self.assertIn("server stop and HAProxy drain could not be confirmed", result.stderr)
        self.assertIn("old token may still be live", result.stderr)
        self.assertEqual(self.current_container.read_text(encoding="ascii"), "old-container\n")


if __name__ == "__main__":
    unittest.main()
