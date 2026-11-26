#!/usr/bin/env python3
"""Check lima demo egress from its Docker network namespace."""

from __future__ import annotations

import argparse
import ipaddress
import os
import pathlib
import re
import secrets
import socket
import ssl
import struct
import subprocess
import sys
from collections.abc import Callable


TAILNET_PEER = ("tailnet", "100.124.236.66", 22)
LIMA_DEMO_GATEWAY_TARGETS = (
    ("lima-demo-backend-gateway", "198.18.130.1", 22),
    ("lima-demo-egress-gateway", "198.18.132.1", 22),
    ("lima-demo-edge-gateway", "198.18.133.1", 22),
)
CHARLIE_TARGETS = (
    TAILNET_PEER,
    ("charlie", "192.168.64.128", 22),
    ("router", "192.168.64.1", 80),
)
LIMA_LAN_CIDRS = tuple(
    ipaddress.ip_network(cidr)
    for cidr in (
        "10.0.0.0/8",
        "172.16.0.0/12",
        "192.168.0.0/16",
        "100.64.0.0/10",
        "169.254.0.0/16",
    )
)
BLOCKED_IPV6_CIDRS = tuple(
    ipaddress.ip_network(cidr) for cidr in ("fc00::/7",)
)
DOCKER_PROJECT = "teagram-demo"
DOCKER_SERVICE = "turnstile-egress"
PUBLIC_HOST = "example.com"
DNS_SERVER = "127.0.0.11"


def fail(message: str) -> None:
    print(f"demo isolation probe failed: {message}", file=sys.stderr)
    raise SystemExit(1)


def probe_target(
    name: str,
    address: str,
    port: int,
    connect: Callable[..., socket.socket] | None = None,
) -> None:
    if connect is None:
        connect = socket.create_connection
    try:
        connection = connect((address, port), timeout=3)
    except TimeoutError:
        return
    except OSError as error:
        fail(f"{name}-probe-error:{type(error).__name__}")
    connection.close()
    fail(f"{name}-destination-reachable")


def require_host_control_target(
    name: str,
    address: str,
    port: int,
    connect: Callable[..., socket.socket] | None = None,
) -> None:
    if connect is None:
        connect = socket.create_connection
    try:
        connection = connect((address, port), timeout=3)
    except TimeoutError:
        fail(f"{name}-host-control-timeout")
    except (ConnectionRefusedError, ConnectionResetError):
        return
    except OSError as error:
        fail(f"{name}-host-control-error:{type(error).__name__}")
    connection.close()


def host_address_targets_from_ip_output(ip_output: str) -> tuple[tuple[str, str, int], ...]:
    targets: list[tuple[str, str, int]] = []
    seen: set[str] = set()
    for line in ip_output.splitlines():
        fields = line.split()
        if len(fields) < 4 or fields[2] not in ("inet", "inet6"):
            continue
        interface = fields[1].split("@", 1)[0]
        if interface == "lo" or interface.startswith(("docker", "br-", "veth")):
            continue
        try:
            address = ipaddress.ip_interface(fields[3]).ip
        except ValueError:
            continue
        networks = LIMA_LAN_CIDRS if address.version == 4 else BLOCKED_IPV6_CIDRS
        if not any(address in network for network in networks):
            continue
        value = str(address)
        if value not in seen:
            seen.add(value)
            targets.append((f"lima-host-{interface}", value, 22))
    return tuple(targets)


def lima_host_address_targets() -> tuple[tuple[str, str, int], ...]:
    output = _one_line_command(["ip", "-o", "address", "show"])
    targets = host_address_targets_from_ip_output(output)
    if not targets:
        fail("could-not-find-lima-private-or-tailnet-address")
    return targets


def lan_gateway_from_route_table(route_table: str) -> str:
    candidates: list[tuple[int, str]] = []
    for line in route_table.splitlines()[1:]:
        fields = line.split()
        if len(fields) < 8:
            continue
        interface, destination, gateway, flags, _, _, metric, _ = fields[:8]
        if destination != "00000000" or not int(flags, 16) & 0x1:
            continue
        if interface == "lo" or interface.startswith(
            ("docker", "br-", "veth", "tailscale", "wg", "tun")
        ):
            continue
        gateway_value = int(gateway, 16)
        if gateway_value == 0:
            continue
        address = str(ipaddress.IPv4Address(struct.pack("<I", gateway_value)))
        if any(ipaddress.ip_address(address) in network for network in LIMA_LAN_CIDRS):
            candidates.append((int(metric, 16), address))

    if not candidates:
        fail("could-not-find-lima-lan-gateway-on-a-private-default-route")
    candidates.sort()
    return candidates[0][1]


def lima_lan_gateway() -> str:
    try:
        route_table = pathlib.Path("/proc/net/route").read_text(encoding="ascii")
    except OSError as error:
        fail(f"could-not-read-lima-routes:{type(error).__name__}")
    return lan_gateway_from_route_table(route_table)


def _one_line_command(command: list[str]) -> str:
    try:
        result = subprocess.run(command, check=True, capture_output=True, text=True)
    except subprocess.CalledProcessError as error:
        diagnostic = (error.stderr or "").strip().splitlines()
        detail = f":{diagnostic[0]}" if diagnostic else ""
        fail(f"command-failed:{pathlib.Path(command[0]).name}{detail}")
    except OSError as error:
        fail(f"command-failed:{pathlib.Path(command[0]).name}:{type(error).__name__}")
    return result.stdout.strip()


def lima_probe_container_pid() -> int:
    container_ids = _one_line_command(
        [
            "docker",
            "ps",
            "--quiet",
            "--filter",
            f"label=com.docker.compose.project={DOCKER_PROJECT}",
            "--filter",
            f"label=com.docker.compose.service={DOCKER_SERVICE}",
        ]
    ).splitlines()
    if len(container_ids) != 1:
        fail("expected-one-running-turnstile-egress-container")
    pid_text = _one_line_command(
        ["docker", "inspect", "--format", "{{.State.Pid}}", container_ids[0]]
    )
    try:
        pid = int(pid_text)
    except ValueError:
        fail("docker-returned-an-invalid-container-pid")
    if pid <= 1 or not pathlib.Path(f"/proc/{pid}/ns/net").exists():
        fail("turnstile-egress-network-namespace-is-not-running")
    return pid


def _skip_dns_name(packet: bytes, offset: int) -> int:
    while offset < len(packet):
        length = packet[offset]
        if length & 0xC0 == 0xC0:
            if offset + 1 >= len(packet):
                fail("malformed-dns-name")
            return offset + 2
        offset += 1
        if length == 0:
            return offset
        if length > 63 or offset + length > len(packet):
            fail("malformed-dns-name")
        offset += length
    fail("malformed-dns-name")


def dns_query_a(hostname: str, dns_server: str = DNS_SERVER) -> str:
    query_id = secrets.randbits(16)
    labels = hostname.rstrip(".").encode("ascii").split(b".")
    question = b"".join(bytes((len(label),)) + label for label in labels) + b"\0"
    packet = struct.pack("!HHHHHH", query_id, 0x0100, 1, 0, 0, 0) + question
    packet += struct.pack("!HH", 1, 1)

    try:
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as dns_socket:
            dns_socket.settimeout(5)
            dns_socket.sendto(packet, (dns_server, 53))
            response, _ = dns_socket.recvfrom(4096)
    except OSError as error:
        fail(f"dns-lookup-error:{type(error).__name__}")
    if len(response) < 12:
        fail("dns-response-too-short")
    response_id, flags, question_count, answer_count = struct.unpack("!HHHH", response[:8])
    if response_id != query_id or flags & 0x8000 == 0 or flags & 0x000F != 0:
        fail("dns-lookup-failed")

    offset = 12
    for _ in range(question_count):
        offset = _skip_dns_name(response, offset) + 4
    for _ in range(answer_count):
        offset = _skip_dns_name(response, offset)
        if offset + 10 > len(response):
            fail("malformed-dns-answer")
        record_type, record_class, _, data_length = struct.unpack(
            "!HHIH", response[offset : offset + 10]
        )
        offset += 10
        record = response[offset : offset + data_length]
        if len(record) != data_length:
            fail("malformed-dns-answer")
        offset += data_length
        if record_type == 1 and record_class == 1 and data_length == 4:
            return socket.inet_ntoa(record)
    fail("dns-answer-has-no-ipv4-address")


def public_https_request(hostname: str, address: str) -> None:
    try:
        raw_socket = socket.create_connection((address, 443), timeout=5)
        with ssl.create_default_context().wrap_socket(
            raw_socket, server_hostname=hostname
        ) as connection:
            connection.settimeout(5)
            connection.sendall(
                f"GET / HTTP/1.1\r\nHost: {hostname}\r\nConnection: close\r\n\r\n".encode(
                    "ascii"
                )
            )
            status_line = connection.makefile("rb").readline(4096).decode(
                "ascii", errors="replace"
            )
    except (OSError, ssl.SSLError) as error:
        fail(f"public-https-error:{type(error).__name__}")
    match = re.match(r"^HTTP/\d\.\d ([0-9]{3})(?: |$)", status_line)
    if match is None or not 200 <= int(match.group(1)) < 400:
        fail("public-https-request-failed")


def probe_lima_inside_network(
    lan_gateway: str,
    host_addresses: tuple[tuple[str, str, int], ...] = (),
) -> None:
    targets = (
        TAILNET_PEER,
        ("lima-lan-gateway", lan_gateway, 22),
        *LIMA_DEMO_GATEWAY_TARGETS,
        *host_addresses,
    )
    for name, address, port in targets:
        probe_target(name, address, port)
    public_address = dns_query_a(PUBLIC_HOST)
    public_https_request(PUBLIC_HOST, public_address)
    print(
        "tailnet_isolation_probe=pass tailnet=timeout lan_gateway=timeout "
        "demo_gateways=timeout lima_addresses=timeout dns=pass https=pass"
    )


def probe_charlie_host() -> None:
    for name, address, port in CHARLIE_TARGETS:
        probe_target(name, address, port)
    print("tailnet_isolation_probe=pass tailnet=timeout charlie=timeout router=timeout")


def run_lima_probe_from_host() -> None:
    if os.geteuid() != 0:
        fail("lima-probe-must-run-as-root-to-enter-the-Docker-network-namespace")
    require_host_control_target(*TAILNET_PEER)
    lan_gateway = lima_lan_gateway()
    require_host_control_target("lima-lan-gateway", lan_gateway, 22)
    for target in LIMA_DEMO_GATEWAY_TARGETS:
        require_host_control_target(*target)
    host_targets = lima_host_address_targets()
    pid = lima_probe_container_pid()
    script_path = pathlib.Path(__file__).resolve()
    environment = os.environ.copy()
    environment["TEAGRAM_DEMO_LAN_GATEWAY"] = lan_gateway
    environment["TEAGRAM_DEMO_HOST_ADDRESSES"] = " ".join(
        address for _, address, _ in host_targets
    )
    try:
        result = subprocess.run(
            [
                "nsenter",
                "--target",
                str(pid),
                "--net",
                "--",
                sys.executable,
                str(script_path),
                "--inside",
            ],
            check=False,
            env=environment,
        )
    except OSError as error:
        fail(f"could-not-enter-demo-network-namespace:{type(error).__name__}")
    if result.returncode != 0:
        raise SystemExit(result.returncode)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument(
        "--charlie",
        action="store_true",
        help="keep the existing teagram-dev host-network checks",
    )
    mode.add_argument(
        "--inside",
        action="store_true",
        help=argparse.SUPPRESS,
    )
    args = parser.parse_args()

    if args.charlie:
        probe_charlie_host()
    elif args.inside:
        gateway = os.environ.get("TEAGRAM_DEMO_LAN_GATEWAY", "")
        try:
            parsed_gateway = ipaddress.ip_address(gateway)
        except ValueError:
            fail("TEAGRAM_DEMO_LAN_GATEWAY-must-be-an-IPv4-address")
        if parsed_gateway.version != 4 or not any(
            parsed_gateway in network for network in LIMA_LAN_CIDRS
        ):
            fail("TEAGRAM_DEMO_LAN_GATEWAY-must-be-in-a-blocked-private-range")
        addresses = os.environ.get("TEAGRAM_DEMO_HOST_ADDRESSES", "").split()
        if not addresses:
            fail("TEAGRAM_DEMO_HOST_ADDRESSES-must-include-Lima-private-or-tailnet-addresses")
        host_targets = tuple(
            (f"lima-host-{index}", address, 22)
            for index, address in enumerate(addresses, start=1)
        )
        probe_lima_inside_network(str(parsed_gateway), host_targets)
    else:
        run_lima_probe_from_host()


if __name__ == "__main__":
    main()
