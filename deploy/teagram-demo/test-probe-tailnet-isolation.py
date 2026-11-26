#!/usr/bin/env python3
"""Tests for the lima and teagram-dev private-network isolation probes."""

from __future__ import annotations

import pathlib
import socket
import struct
import subprocess
import sys
import unittest
from unittest import mock


SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPT_DIR))
import probe_tailnet_isolation  # noqa: E402


class ProbeTailnetIsolationTests(unittest.TestCase):
    def test_demo_bridge_gateways_are_isolation_targets(self) -> None:
        self.assertEqual(
            probe_tailnet_isolation.LIMA_DEMO_GATEWAY_TARGETS,
            (
                ("lima-demo-backend-gateway", "198.18.130.1", 22),
                ("lima-demo-egress-gateway", "198.18.132.1", 22),
                ("lima-demo-edge-gateway", "198.18.133.1", 22),
            ),
        )

    def test_lima_targets_tailnet_peer_and_discovered_lan_gateway(self) -> None:
        host_targets = (
            ("lima-host-eth0", "10.0.0.2", 22),
            ("lima-host-tailscale0", "100.100.0.3", 22),
        )
        with (
            mock.patch.object(
                probe_tailnet_isolation.socket,
                "create_connection",
                side_effect=TimeoutError,
            ) as connect,
            mock.patch.object(
                probe_tailnet_isolation, "dns_query_a", return_value="93.184.216.34"
            ) as dns,
            mock.patch.object(probe_tailnet_isolation, "public_https_request") as https,
        ):
            probe_tailnet_isolation.probe_lima_inside_network("10.0.0.1", host_targets)

        self.assertEqual(
            [call.args[0] for call in connect.call_args_list],
            [
                ("100.124.236.66", 22),
                ("10.0.0.1", 22),
                *(
                    (address, port)
                    for _, address, port in probe_tailnet_isolation.LIMA_DEMO_GATEWAY_TARGETS
                ),
                ("10.0.0.2", 22),
                ("100.100.0.3", 22),
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
                "require_host_control_target",
            ) as host_control,
            mock.patch.object(
                probe_tailnet_isolation,
                "lima_host_address_targets",
                return_value=(("lima-host-eth0", "10.0.0.2", 22),),
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
        self.assertEqual(run.call_args.kwargs["env"]["TEAGRAM_DEMO_HOST_ADDRESSES"], "10.0.0.2")
        self.assertEqual(
            [call.args for call in host_control.call_args_list],
            [
                probe_tailnet_isolation.TAILNET_PEER,
                ("lima-lan-gateway", "10.0.0.1", 22),
                *probe_tailnet_isolation.LIMA_DEMO_GATEWAY_TARGETS,
            ],
        )

    def test_lan_gateway_uses_the_host_private_default_route(self) -> None:
        routes = """Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT
eth0 00000000 0100000A 0003 0 0 00000064 00000000 0 0 0
docker0 00000000 0200000A 0003 0 0 00000001 00000000 0 0 0
tailscale0 00000000 01006464 0003 0 0 00000000 00000000 0 0 0
"""

        self.assertEqual(
            probe_tailnet_isolation.lan_gateway_from_route_table(routes), "10.0.0.1"
        )

    def test_host_address_targets_include_private_and_tailnet_but_skip_docker(self) -> None:
        output = """1: lo inet 127.0.0.1/8 scope host lo
2: eth0 inet 10.0.0.2/24 brd 10.0.0.255 scope global eth0
3: tailscale0 inet 100.100.0.3/32 scope global tailscale0
4: docker0 inet 172.17.0.1/16 brd 172.17.255.255 scope global docker0
5: tailscale0 inet6 fd7a:115c:a1e0::1234/48 scope global
"""

        self.assertEqual(
            probe_tailnet_isolation.host_address_targets_from_ip_output(output),
            (
                ("lima-host-eth0", "10.0.0.2", 22),
                ("lima-host-tailscale0", "100.100.0.3", 22),
                ("lima-host-tailscale0", "fd7a:115c:a1e0::1234", 22),
            ),
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

    def test_host_control_timeout_fails_instead_of_proving_isolation(self) -> None:
        targets = (
            probe_tailnet_isolation.TAILNET_PEER,
            ("lima-lan-gateway", "10.0.0.1", 22),
            *probe_tailnet_isolation.LIMA_DEMO_GATEWAY_TARGETS,
        )
        for target in targets:
            with (
                self.subTest(target=target),
                mock.patch.object(
                    probe_tailnet_isolation.socket,
                    "create_connection",
                    side_effect=TimeoutError,
                ),
                self.assertRaises(SystemExit),
            ):
                probe_tailnet_isolation.require_host_control_target(*target)

    def test_host_control_accept_and_reset_prove_target_is_online(self) -> None:
        connection = mock.Mock()
        with mock.patch.object(
            probe_tailnet_isolation.socket,
            "create_connection",
            return_value=connection,
        ):
            probe_tailnet_isolation.require_host_control_target(
                *probe_tailnet_isolation.TAILNET_PEER
            )
        connection.close.assert_called_once_with()

        with mock.patch.object(
            probe_tailnet_isolation.socket,
            "create_connection",
            side_effect=ConnectionResetError,
        ):
            probe_tailnet_isolation.require_host_control_target(
                *probe_tailnet_isolation.TAILNET_PEER
            )

    def test_one_line_command_includes_first_stderr_line(self) -> None:
        error = subprocess.CalledProcessError(
            1, ["docker", "ps"], stderr="permission denied\nsecond line"
        )
        with (
            mock.patch.object(
                probe_tailnet_isolation.subprocess, "run", side_effect=error
            ),
            mock.patch.object(probe_tailnet_isolation, "fail", side_effect=SystemExit) as fail,
            self.assertRaises(SystemExit),
        ):
            probe_tailnet_isolation._one_line_command(["docker", "ps"])

        fail.assert_called_once_with("command-failed:docker:permission denied")

    def test_lima_probe_container_requires_exactly_one_container(self) -> None:
        for output in ("", "first\nsecond\n"):
            with (
                self.subTest(output=output),
                mock.patch.object(
                    probe_tailnet_isolation,
                    "_one_line_command",
                    return_value=output,
                ) as command,
                self.assertRaises(SystemExit),
            ):
                probe_tailnet_isolation.lima_probe_container_pid()
            self.assertEqual(
                command.call_args.args[0],
                [
                    "docker",
                    "ps",
                    "--quiet",
                    "--filter",
                    "label=com.docker.compose.project=teagram-demo",
                    "--filter",
                    "label=com.docker.compose.service=turnstile-egress",
                ],
            )

        with (
            mock.patch.object(
                probe_tailnet_isolation,
                "_one_line_command",
                side_effect=["container-id\n", "4321"],
            ) as command,
            mock.patch.object(probe_tailnet_isolation.pathlib.Path, "exists", return_value=True),
        ):
            self.assertEqual(probe_tailnet_isolation.lima_probe_container_pid(), 4321)
        self.assertEqual(command.call_count, 2)
        self.assertEqual(command.call_args_list[1].args[0][-1], "container-id")

    def test_public_https_requires_success_status_and_default_tls_verification(self) -> None:
        class FakeConnection:
            def __init__(self, status_line: bytes) -> None:
                self.status_line = status_line

            def __enter__(self) -> "FakeConnection":
                return self

            def __exit__(self, *args: object) -> None:
                return None

            def settimeout(self, timeout: float) -> None:
                self.timeout = timeout

            def sendall(self, request: bytes) -> None:
                self.request = request

            def makefile(self, mode: str):
                return mock.Mock(readline=mock.Mock(return_value=self.status_line))

        class FakeTLSContext:
            check_hostname = True
            verify_mode = probe_tailnet_isolation.ssl.CERT_REQUIRED

            def __init__(self, connection: FakeConnection) -> None:
                self.connection = connection

            def wrap_socket(self, raw_socket: object, *, server_hostname: str) -> FakeConnection:
                self.raw_socket = raw_socket
                self.server_hostname = server_hostname
                return self.connection

        for status_line, should_fail in (
            (b"HTTP/1.1 503 Service Unavailable\r\n", True),
            (b"HTTP/1.1 200 OK\r\n", False),
        ):
            with self.subTest(status_line=status_line):
                raw_socket = mock.Mock()
                connection = FakeConnection(status_line)
                context = FakeTLSContext(connection)
                with (
                    mock.patch.object(
                        probe_tailnet_isolation.socket,
                        "create_connection",
                        return_value=raw_socket,
                    ),
                    mock.patch.object(
                        probe_tailnet_isolation.ssl,
                        "create_default_context",
                        return_value=context,
                    ) as default_context,
                ):
                    if should_fail:
                        with self.assertRaises(SystemExit):
                            probe_tailnet_isolation.public_https_request(
                                "example.com", "93.184.216.34"
                            )
                    else:
                        probe_tailnet_isolation.public_https_request(
                            "example.com", "93.184.216.34"
                        )

                default_context.assert_called_once_with()
                self.assertTrue(context.check_hostname)
                self.assertEqual(context.verify_mode, probe_tailnet_isolation.ssl.CERT_REQUIRED)
                self.assertIs(context.raw_socket, raw_socket)
                self.assertEqual(context.server_hostname, "example.com")

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
