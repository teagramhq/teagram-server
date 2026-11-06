#!/usr/bin/env python3
import importlib.util
import ipaddress
import os
import tempfile
import unittest
from pathlib import Path


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

    def test_accepts_loopback_and_tailscale_listeners_owned_by_tailscaled(self):
        self.write_tables(
            [tcp_row("127.0.0.1", 1001), tcp_row("100.64.1.1", 1002)],
            [tcp_row("fd7a:115c:a1e0::1", 1003)],
        )
        self.add_process(50, "tailscaled", 1002, 1003)
        self.assertTrue(self.inspect())

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
        self.add_process(50, "python", 1002)
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
