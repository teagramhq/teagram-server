#!/usr/bin/env python3
"""Control or health-check the private HAProxy runtime socket."""

from __future__ import annotations

import argparse
import csv
import io
import pathlib
import socket
import sys
import time


SOCKET_PATH = pathlib.Path(".state/teagram-demo/haproxy-run/admin.sock")
SLOTS = {"teagramd", "teagramd-next"}
BACKENDS = {
    "teagramd": "teagramd",
    "teagramd-next": "teagramd-next",
}
GOOD_CHECKS = {"L4OK", "L7OK", "L6OK", "L57OK"}


def fail(message: str) -> None:
    print(f"teagram-demo HAProxy control: {message}", file=sys.stderr)
    raise SystemExit(1)


def exchange(socket_path: pathlib.Path, command: str) -> str:
    client = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    client.settimeout(5)
    try:
        client.connect(str(socket_path))
        client.sendall(command.encode("ascii") + b"\n")
        client.shutdown(socket.SHUT_WR)
        chunks = []
        while True:
            chunk = client.recv(4096)
            if not chunk:
                break
            chunks.append(chunk)
        response = b"".join(chunks).decode("utf-8", errors="replace")
    except OSError as error:
        fail(f"runtime socket command failed ({command.split()[0]}): {error}")
    finally:
        client.close()
    if not response or "Unknown command" in response or "Permission denied" in response:
        fail(f"HAProxy rejected runtime command: {command.split()[0]}")
    return response


def server_row(socket_path: pathlib.Path, backend: str, server: str) -> dict[str, str] | None:
    report = exchange(socket_path, "show stat").removeprefix("# ")
    rows = csv.DictReader(io.StringIO(report))
    for row in rows:
        if row.get("pxname") == backend and row.get("svname") == server:
            return row
    return None


def wait_healthy(socket_path: pathlib.Path, slot: str, timeout: int) -> None:
    servers = (
        ("teagramd", slot),
        ("teagramd_mtproto", f"{slot}-mtproto"),
    )
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if all(
            (row := server_row(socket_path, backend, server)) is not None
            and row.get("status") == "UP"
            and row.get("check_status") in GOOD_CHECKS
            for backend, server in servers
        ):
            return
        time.sleep(1)
    fail(f"{slot} did not pass both HAProxy server checks")


def slot_command(socket_path: pathlib.Path, action: str, slot: str) -> None:
    if slot not in SLOTS:
        fail("invalid service slot")
    state = "ready" if action == "enable" else "drain"
    exchange(socket_path, f"set server teagramd/{slot} state {state}")
    exchange(socket_path, f"set server teagramd_mtproto/{slot}-mtproto state {state}")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--socket", type=pathlib.Path, default=SOCKET_PATH)
    subparsers = parser.add_subparsers(dest="action", required=True)
    for action in ("enable", "drain"):
        subparser = subparsers.add_parser(action)
        subparser.add_argument("slot", choices=sorted(SLOTS))
    wait = subparsers.add_parser("wait-healthy")
    wait.add_argument("slot", choices=sorted(SLOTS))
    wait.add_argument("--timeout", type=int, default=60)
    args = parser.parse_args()
    if args.action == "wait-healthy":
        wait_healthy(args.socket, args.slot, args.timeout)
        print(f"HAProxy server checks=passed slot={args.slot}")
        return
    slot_command(args.socket, args.action, args.slot)
    print(f"HAProxy server action={args.action} slot={args.slot}")


if __name__ == "__main__":
    main()
