#!/usr/bin/env python3
"""Inspect host TCP listeners without depending on the optional `ss` utility."""

from __future__ import annotations

import ipaddress
import os
import re
import sys
from pathlib import Path

PORT = 2447
LISTEN = "0A"
TAILSCALE_IPV4 = ipaddress.ip_network("100.64.0.0/10")
TAILSCALE_IPV6 = ipaddress.ip_network("fd7a:115c:a1e0::/48")
SOCKET_FD = re.compile(r"socket:\[(\d+)\]")


def decode_address(encoded: str, family: int) -> ipaddress.IPv4Address | ipaddress.IPv6Address:
    address_hex, port_hex = encoded.split(":", 1)
    expected_length = 8 if family == 4 else 32
    if len(address_hex) != expected_length or len(port_hex) != 4:
        raise ValueError("malformed address in proc TCP table")
    raw = bytes.fromhex(address_hex)
    if family == 4:
        return ipaddress.IPv4Address(raw[::-1])
    decoded = b"".join(raw[index : index + 4][::-1] for index in range(0, 16, 4))
    return ipaddress.IPv6Address(decoded)


def read_listeners(proc_root: Path) -> list[tuple[ipaddress.IPv4Address | ipaddress.IPv6Address, str]]:
    listeners = []
    for name, family in (("tcp", 4), ("tcp6", 6)):
        path = proc_root / "net" / name
        try:
            with path.open(encoding="ascii") as table:
                next(table, None)
                for line in table:
                    fields = line.split()
                    if len(fields) < 10:
                        raise ValueError("malformed row in proc TCP table")
                    address = decode_address(fields[1], family)
                    port = int(fields[1].split(":", 1)[1], 16)
                    if port == PORT and fields[3] == LISTEN:
                        inode = fields[9]
                        if not inode.isdecimal():
                            raise ValueError("malformed listener inode in proc TCP table")
                        listeners.append((address, inode))
        except OSError as error:
            raise ValueError(f"cannot read {path}") from error
    return listeners


def socket_owners(proc_root: Path, wanted: set[str]) -> dict[str, set[str]]:
    owners = {inode: set() for inode in wanted}
    try:
        processes = os.scandir(proc_root)
    except OSError as error:
        raise ValueError(f"cannot inspect {proc_root}: {error}") from error

    with processes:
        for process in processes:
            if not process.name.isdecimal():
                continue
            fd_path = proc_root / process.name / "fd"
            try:
                descriptors = list(os.scandir(fd_path))
            except (FileNotFoundError, ProcessLookupError):
                continue
            except OSError as error:
                raise ValueError(f"cannot inspect {fd_path}: {error}") from error

            for descriptor in descriptors:
                try:
                    target = os.readlink(descriptor.path)
                except (FileNotFoundError, ProcessLookupError):
                    continue
                except OSError as error:
                    raise ValueError(f"cannot read {descriptor.path}: {error}") from error
                match = SOCKET_FD.fullmatch(target)
                if not match or match.group(1) not in wanted:
                    continue
                try:
                    comm_path = proc_root / process.name / "comm"
                    command = comm_path.read_text(encoding="ascii").strip()
                except (FileNotFoundError, ProcessLookupError):
                    continue
                except OSError as error:
                    raise ValueError(f"cannot read {comm_path}: {error}") from error
                owners[match.group(1)].add(command)
    return owners


def inspect_listeners(proc_root: Path, tail_ipv4: str, tail_ipv6: str) -> bool:
    expected_v4 = ipaddress.ip_address(tail_ipv4)
    expected_v6 = ipaddress.ip_address(tail_ipv6)
    if expected_v4 not in TAILSCALE_IPV4 or expected_v6 not in TAILSCALE_IPV6:
        raise ValueError("Tailscale returned an address outside its expected ranges")

    listeners = read_listeners(proc_root)
    tailscale_inodes = set()
    has_loopback = False
    for address, inode in listeners:
        if address == ipaddress.ip_address("127.0.0.1"):
            has_loopback = True
        elif address == expected_v4 or address == expected_v6:
            tailscale_inodes.add(inode)
        else:
            raise ValueError(f"unexpected listener address {address}")

    owners = socket_owners(proc_root, tailscale_inodes) if tailscale_inodes else {}
    for inode in tailscale_inodes:
        if owners[inode] != {"tailscaled"}:
            raise ValueError("Tailscale address listener is not owned by tailscaled")
    return has_loopback


def main(arguments: list[str]) -> int:
    if len(arguments) != 3:
        print("usage: bot_api_qa_listeners.py <tailnet-ipv4> <tailnet-ipv6> <proc-root>", file=sys.stderr)
        return 64
    try:
        has_loopback = inspect_listeners(Path(arguments[2]), arguments[0], arguments[1])
    except (OSError, ValueError) as error:
        print(f"Bot API QA listener inspection failed: {error}", file=sys.stderr)
        return 1
    print("loopback" if has_loopback else "none")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
