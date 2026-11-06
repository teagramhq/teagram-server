#!/usr/bin/env python3
import errno
import importlib.util
import io
import ipaddress
import os
import tempfile
import unittest
from contextlib import redirect_stderr
from pathlib import Path
from unittest.mock import patch


SCRIPT = Path(__file__).with_name("ops") / "bot_api_qa_listeners.py"
SPEC = importlib.util.spec_from_file_location("bot_api_qa_listeners", SCRIPT)
LISTENERS = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
SPEC.loader.exec_module(LISTENERS)


def encode_address(address: str) -> str:
    parsed = ipaddress.ip_address(address)
    packed = parsed.packed
    if parsed.version == 4:
        packed = packed[::-1]
    else:
        packed = b"".join(packed[index : index + 4][::-1] for index in range(0, 16, 4))
    return f"{packed.hex().upper()}:098F"


def tcp_row(address: str, inode: int) -> str:
    return f"0: {encode_address(address)} 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 {inode}"


class ListenerInspectionTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.proc = Path(self.temporary.name)
        (self.proc / "net").mkdir()
        self.write_tables([], [])

    def tearDown(self):
        self.temporary.cleanup()

    def write_tables(self, ipv4_rows, ipv6_rows):
        header = "sl local_address rem_address st tx_queue:rx_queue tr:tm->when retrnsmt uid timeout inode\n"
        (self.proc / "net" / "tcp").write_text(header + "".join(f"{row}\n" for row in ipv4_rows))
        (self.proc / "net" / "tcp6").write_text(header + "".join(f"{row}\n" for row in ipv6_rows))

    def add_process(self, pid: int, command: str, *inodes: int):
        process = self.proc / str(pid)
        (process / "fd").mkdir(parents=True)
        (process / "comm").write_text(command + "\n")
        for index, inode in enumerate(inodes):
            os.symlink(f"socket:[{inode}]", process / "fd" / str(index))

    def inspect(self):
        return LISTENERS.inspect_listeners(self.proc, "100.64.1.1", "fd7a:115c:a1e0::1")

    def inspect_cli(self):
        stderr = io.StringIO()
        with redirect_stderr(stderr):
            status = LISTENERS.main(["100.64.1.1", "fd7a:115c:a1e0::1", str(self.proc)])
        return status, stderr.getvalue()

    def test_accepts_loopback_and_tailscale_listeners_owned_by_tailscaled(self):
        self.write_tables(
            [tcp_row("127.0.0.1", 1001), tcp_row("100.64.1.1", 1002)],
            [tcp_row("fd7a:115c:a1e0::1", 1003)],
        )
        self.add_process(50, "tailscaled", 1002, 1003)
        self.assertTrue(self.inspect())

    def test_ignores_processes_that_exit_during_owner_scan(self):
        self.write_tables([tcp_row("127.0.0.1", 1001), tcp_row("100.64.1.1", 1002)], [])
        self.add_process(50, "tailscaled", 1002)
        (self.proc / "49").mkdir()
        exited_process = self.proc / "51"
        (exited_process / "fd").mkdir(parents=True)
        os.symlink("socket:[1002]", exited_process / "fd" / "3")

        self.assertTrue(self.inspect())

    def test_ignores_esrch_for_exiting_pids_when_a_tailscaled_owner_survives(self):
        self.write_tables([tcp_row("127.0.0.1", 1001), tcp_row("100.64.1.1", 1002)], [])
        self.add_process(50, "tailscaled", 1002)
        self.add_process(51, "tailscaled", 1002)
        process = self.proc / "51"
        failures = (
            ("fd scan", LISTENERS.os, "scandir", process / "fd"),
            ("fd readlink", LISTENERS.os, "readlink", process / "fd" / "0"),
            ("comm read", Path, "read_text", process / "comm"),
        )

        for stage, owner, method, failed_path in failures:
            original = getattr(owner, method)

            def raise_esrch(path, *args, target=failed_path, delegate=original, **kwargs):
                if Path(path) == target:
                    raise ProcessLookupError(errno.ESRCH, "process exited", str(path))
                return delegate(path, *args, **kwargs)

            with self.subTest(stage=stage):
                with patch.object(owner, method, side_effect=raise_esrch, autospec=True):
                    self.assertTrue(self.inspect())

    def test_reports_fd_directory_errors_with_path_and_cause(self):
        self.write_tables([tcp_row("100.64.1.1", 1002)], [])
        fd_path = self.proc / "50" / "fd"
        fd_path.parent.mkdir()
        fd_path.write_text("not a directory")

        status, stderr = self.inspect_cli()

        self.assertEqual(1, status)
        self.assertIn(str(fd_path), stderr)
        self.assertIn("[Errno", stderr)

    def test_reports_fd_readlink_errors_with_path_and_cause(self):
        self.write_tables([tcp_row("100.64.1.1", 1002)], [])
        fd_path = self.proc / "50" / "fd"
        fd_path.mkdir(parents=True)
        descriptor_path = fd_path / "3"
        descriptor_path.write_text("not a symlink")

        status, stderr = self.inspect_cli()

        self.assertEqual(1, status)
        self.assertIn(str(descriptor_path), stderr)
        self.assertIn("[Errno", stderr)

    def test_reports_process_name_errors_with_path_and_cause(self):
        self.write_tables([tcp_row("100.64.1.1", 1002)], [])
        process = self.proc / "50"
        (process / "fd").mkdir(parents=True)
        os.symlink("socket:[1002]", process / "fd" / "3")
        comm_path = process / "comm"
        comm_path.mkdir()

        status, stderr = self.inspect_cli()

        self.assertEqual(1, status)
        self.assertIn(str(comm_path), stderr)
        self.assertIn("[Errno", stderr)

    def test_accepts_no_listener(self):
        self.assertFalse(self.inspect())

    def test_rejects_wildcard_lan_and_public_listeners(self):
        for address in ("0.0.0.0", "192.168.1.20", "203.0.113.8"):
            with self.subTest(address=address):
                self.write_tables([tcp_row(address, 1001)], [])
                with self.assertRaisesRegex(ValueError, "unexpected listener address"):
                    self.inspect()

    def test_rejects_tailscale_address_owned_by_another_process(self):
        self.write_tables([tcp_row("100.64.1.1", 1002)], [])
        self.add_process(50, "not-tailscaled", 1002)
        with self.assertRaisesRegex(ValueError, "not owned by tailscaled"):
            self.inspect()

    def test_rejects_tailscale_address_with_no_resolvable_process(self):
        self.write_tables([tcp_row("100.64.1.1", 1002)], [])
        with self.assertRaisesRegex(ValueError, "not owned by tailscaled"):
            self.inspect()

    def test_rejects_tailnet_address_outside_expected_interface(self):
        self.write_tables([tcp_row("100.64.1.2", 1002)], [])
        with self.assertRaisesRegex(ValueError, "unexpected listener address"):
            self.inspect()


if __name__ == "__main__":
    unittest.main()
