#!/usr/bin/env python3
"""Tests for the lima and teagram-dev private-network isolation probes."""

from __future__ import annotations

import pathlib
import socket
import struct
import sys
import unittest
from unittest import mock


SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPT_DIR))
import probe_tailnet_isolation  # noqa: E402


class ProbeTailnetIsolationTests(unittest.TestCase):
    def test_lima_targets_tailnet_peer_and_discovered_lan_gateway(self) -> None:
        with (
            mock.patch.object(probe_tailnet_isolation, "probe_target") as probe,
            mock.patch.object(
                probe_tailnet_isolation, "dns_query_a", return_value="93.184.216.34"
            ) as dns,
            mock.patch.object(probe_tailnet_isolation, "public_https_request") as https,
        ):
            probe_tailnet_isolation.probe_lima_inside_network("10.0.0.1")

        self.assertEqual(
            [call.args[:3] for call in probe.call_args_list],
            [
                probe_tailnet_isolation.TAILNET_PEER,
                ("lima-lan-gateway", "10.0.0.1", 22),
            ],
        )
        dns.assert_called_once_with("example.com")
        https.assert_called_once_with("example.com", "93.184.216.34")

    def test_lima_probe_enters_the_running_egress_network_namespace(self) -> None:
        with (
            mock.patch.object(probe_tailnet_isolation.os, "geteuid", return_value=0),
            mock.patch.object(
                probe_tailnet_isolation, "lima_lan_gateway", return_value="10.0.0.1"
            ),
            mock.patch.object(
                probe_tailnet_isolation,
                "lima_probe_container_pid",
                return_value=4321,
            ),
            mock.patch.object(
                probe_tailnet_isolation.subprocess,
                "run",
                return_value=mock.Mock(returncode=0),
            ) as run,
        ):
            probe_tailnet_isolation.run_lima_probe_from_host()

        command = run.call_args.args[0]
        self.assertEqual(command[:5], ["nsenter", "--target", "4321", "--net", "--"])
        self.assertEqual(command[-1], "--inside")
        self.assertEqual(run.call_args.kwargs["env"]["TEAGRAM_DEMO_LAN_GATEWAY"], "10.0.0.1")

    def test_lan_gateway_uses_the_host_private_default_route(self) -> None:
        routes = """Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT
eth0 00000000 0100000A 0003 0 0 00000064 00000000 0 0 0
docker0 00000000 010000C6 0003 0 0 00000001 00000000 0 0 0
tailscale0 00000000 01000064 0003 0 0 00000000 00000000 0 0 0
"""

        self.assertEqual(
            probe_tailnet_isolation.lan_gateway_from_route_table(routes), "10.0.0.1"
        )

    def test_timeout_is_the_only_success_condition(self) -> None:
        with mock.patch.object(
            probe_tailnet_isolation.socket,
            "create_connection",
            side_effect=TimeoutError,
        ):
            probe_tailnet_isolation.probe_target("tailnet", "100.124.236.66", 22)

    def test_reachable_destination_fails(self) -> None:
        connection = mock.Mock()
        with mock.patch.object(
            probe_tailnet_isolation.socket,
            "create_connection",
            return_value=connection,
        ), self.assertRaises(SystemExit):
            probe_tailnet_isolation.probe_target("tailnet", "100.124.236.66", 22)
        connection.close.assert_called_once_with()

    def test_unreachable_route_error_is_not_reported_as_a_timeout(self) -> None:
        with mock.patch.object(
            probe_tailnet_isolation.socket,
            "create_connection",
            side_effect=OSError("network unreachable"),
        ), self.assertRaises(SystemExit):
            probe_tailnet_isolation.probe_target("tailnet", "100.124.236.66", 22)

    def test_dns_lookup_uses_docker_embedded_dns(self) -> None:
        fake_socket = mock.MagicMock()
        fake_socket.__enter__.return_value = fake_socket
        fake_socket.recvfrom.return_value = (
            struct.pack("!HHHHHH", 0x1234, 0x8180, 1, 1, 0, 0)
            + b"\x07example\x03com\0"
            + struct.pack("!HH", 1, 1)
            + b"\xc0\x0c"
            + struct.pack("!HHIH", 1, 1, 30, 4)
            + socket.inet_aton("93.184.216.34"),
            ("127.0.0.11", 53),
        )
        with (
            mock.patch.object(probe_tailnet_isolation.secrets, "randbits", return_value=0x1234),
            mock.patch.object(
                probe_tailnet_isolation.socket, "socket", return_value=fake_socket
            ),
        ):
            result = probe_tailnet_isolation.dns_query_a("example.com")

        self.assertEqual(result, "93.184.216.34")
        fake_socket.sendto.assert_called_once()
        self.assertEqual(fake_socket.sendto.call_args.args[1], ("127.0.0.11", 53))

    def test_charlie_mode_keeps_its_existing_private_targets(self) -> None:
        with (
            mock.patch.object(probe_tailnet_isolation, "probe_target") as probe,
            mock.patch.object(sys, "argv", ["probe_tailnet_isolation.py", "--charlie"]),
        ):
            probe_tailnet_isolation.main()

        self.assertEqual(
            [call.args[:3] for call in probe.call_args_list],
            list(probe_tailnet_isolation.CHARLIE_TARGETS),
        )

    def test_turnstile_rollout_keeps_using_charlie_mode(self) -> None:
        rollout = (SCRIPT_DIR / "ops" / "turnstile-signin-rollout.sh").read_text(
            encoding="utf-8"
        )

        self.assertIn('probe_tailnet_isolation.py" --charlie', rollout)


if __name__ == "__main__":
    unittest.main()
