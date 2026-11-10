#!/usr/bin/env python3
"""Probe the v1 local-direct discovery protocol over a TCP half-close."""

from __future__ import annotations

import argparse
import os
import socket
import struct


REQUEST_MAGIC = b"telegramd-key-v1"
RESPONSE_MAGIC = b"telegramd-key-r1"
NONCE_SIZE = 32
MAX_SPKI_SIZE = 4096


def probe(host: str, port: int, timeout: float = 5.0) -> int:
    nonce = os.urandom(NONCE_SIZE)
    request = REQUEST_MAGIC + nonce
    if len(request) != 48:
        raise AssertionError(f"discovery request is {len(request)} bytes, want 48")

    with socket.create_connection((host, port), timeout=timeout) as connection:
        connection.settimeout(timeout)
        connection.sendall(request)
        connection.shutdown(socket.SHUT_WR)
        response = bytearray()
        while len(response) <= len(RESPONSE_MAGIC) + NONCE_SIZE + 4 + 6 + MAX_SPKI_SIZE:
            chunk = connection.recv(4096)
            if not chunk:
                break
            response.extend(chunk)

    if len(response) < 58:
        raise RuntimeError(f"discovery returned {len(response)} bytes, want a complete response")
    if response[:16] != RESPONSE_MAGIC:
        raise RuntimeError("discovery response has the wrong magic")
    if response[16:48] != nonce:
        raise RuntimeError("discovery response nonce does not match the request")

    body_length = struct.unpack_from(">I", response, 48)[0]
    dc_id = struct.unpack_from(">i", response, 52)[0]
    spki_length = struct.unpack_from(">H", response, 56)[0]
    if not 1 <= dc_id <= 5:
        raise RuntimeError(f"discovery response DC id {dc_id} is invalid")
    if not 1 <= spki_length <= MAX_SPKI_SIZE or body_length != 6 + spki_length:
        raise RuntimeError("discovery response has invalid body or SPKI lengths")
    if len(response) != 58 + spki_length:
        raise RuntimeError(f"discovery response has {len(response)} bytes, want {58 + spki_length}")
    return len(response)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", required=True)
    parser.add_argument("--port", required=True, type=int)
    parser.add_argument("--timeout", type=float, default=5.0)
    args = parser.parse_args()
    size = probe(args.host, args.port, args.timeout)
    print(f"discovery half-close succeeded: {size} bytes from {args.host}:{args.port}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
