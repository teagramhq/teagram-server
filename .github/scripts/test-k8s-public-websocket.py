#!/usr/bin/env python3
"""Exercise the public WebSocket listener through the k3s connector path."""
import argparse
import base64
import hashlib
import json
import os
import socket
import struct
import sys
import urllib.error
import urllib.request

WEBSOCKET_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


def validate_discovery_document(discovery):
    mtproto = discovery.get("mtproto") if isinstance(discovery, dict) else None
    rsa_spki = mtproto.get("rsa_spki") if isinstance(mtproto, dict) else None
    if not isinstance(rsa_spki, str) or not rsa_spki:
        raise RuntimeError("public discovery response has no mtproto.rsa_spki")
    websocket = discovery.get("websocket") if isinstance(discovery, dict) else None
    endpoint = websocket.get("endpoint") if isinstance(websocket, dict) else None
    if endpoint != "wss://server.example.test/apiws":
        raise RuntimeError("public discovery has the wrong WebSocket endpoint")


def receive_headers(connection):
    response = bytearray()
    while b"\r\n\r\n" not in response:
        chunk = connection.recv(4096)
        if not chunk:
            raise RuntimeError("connector closed before the WebSocket handshake completed")
        response.extend(chunk)
        if len(response) > 65536:
            raise RuntimeError("WebSocket handshake response headers are too large")
    header_bytes, _, remainder = response.partition(b"\r\n\r\n")
    return header_bytes.decode("latin-1"), remainder


def receive_frame(connection, pending=b""):
    frame = bytearray(pending)

    def take(length):
        while len(frame) < length:
            chunk = connection.recv(max(4096, length - len(frame)))
            if not chunk:
                raise RuntimeError("connector closed before the WebSocket control frame completed")
            frame.extend(chunk)
        value = bytes(frame[:length])
        del frame[:length]
        return value

    first, second = take(2)
    length = second & 0x7F
    if length == 126:
        length = struct.unpack("!H", take(2))[0]
    elif length == 127:
        length = struct.unpack("!Q", take(8))[0]
    masked = bool(second & 0x80)
    mask = take(4) if masked else b""
    payload = take(length)
    if masked:
        payload = bytes(value ^ mask[index % 4] for index, value in enumerate(payload))
    return first & 0x0F, payload


def test_discovery(host, port):
    request = urllib.request.Request(
        f"http://{host}:{port}/.well-known/teagramd/client",
        headers={
            "Host": "server.example.test",
            "CF-Connecting-IP": "192.0.2.1",
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            if response.status != 200:
                raise RuntimeError(f"public discovery returned HTTP {response.status}")
            discovery = json.loads(response.read())
    except urllib.error.HTTPError as error:
        raise RuntimeError(f"public discovery returned HTTP {error.code}") from error
    validate_discovery_document(discovery)


def test_websocket(host, port, origin, expected_status):
    key = base64.b64encode(os.urandom(16)).decode("ascii")
    expected_accept = base64.b64encode(
        hashlib.sha1((key + WEBSOCKET_GUID).encode("ascii")).digest()
    ).decode("ascii")
    request = (
        "GET /apiws HTTP/1.1\r\n"
        "Host: server.example.test\r\n"
        "Upgrade: websocket\r\n"
        "Connection: Upgrade\r\n"
        f"Sec-WebSocket-Key: {key}\r\n"
        "Sec-WebSocket-Version: 13\r\n"
        f"Origin: {origin}\r\n"
        "CF-Connecting-IP: 192.0.2.1\r\n"
        "\r\n"
    ).encode("ascii")
    with socket.create_connection((host, port), timeout=10) as connection:
        connection.settimeout(10)
        connection.sendall(request)
        headers, pending = receive_headers(connection)
        lines = headers.split("\r\n")
        status_parts = lines[0].split() if lines else []
        if len(status_parts) < 2:
            raise RuntimeError("public WebSocket returned an invalid HTTP status line")
        try:
            status = int(status_parts[1])
        except ValueError as error:
            raise RuntimeError("public WebSocket returned a non-numeric HTTP status") from error
        if status != expected_status:
            raise RuntimeError(
                f"public WebSocket returned HTTP {status} for {origin}; want {expected_status}"
            )
        if status == 403:
            return
        response_headers = {}
        for line in lines[1:]:
            name, separator, value = line.partition(":")
            if separator:
                response_headers[name.strip().lower()] = value.strip()
        if response_headers.get("sec-websocket-accept") != expected_accept:
            raise RuntimeError("public WebSocket returned an invalid handshake accept key")

        payload = b"k3s"
        mask = os.urandom(4)
        masked = bytes(value ^ mask[index % 4] for index, value in enumerate(payload))
        connection.sendall(bytes((0x89, 0x80 | len(payload))) + mask + masked)
        opcode, pong = receive_frame(connection, pending)
        if opcode != 0xA or pong != payload:
            raise RuntimeError("public WebSocket session did not answer the ping with a matching pong")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, required=True)
    args = parser.parse_args()
    try:
        test_discovery(args.host, args.port)
        test_websocket(args.host, args.port, "https://web.example.test", 101)
        test_websocket(args.host, args.port, "https://foreign.example.test", 403)
    except (OSError, RuntimeError, urllib.error.URLError, json.JSONDecodeError) as error:
        print(f"public connector check failed: {error}", file=sys.stderr)
        return 1
    print("public discovery, allowed WebSocket session, and foreign-origin rejection passed through the connector")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
