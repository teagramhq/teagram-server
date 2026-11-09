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
    if not isinstance(discovery, dict) or not discovery.get("rsa_spki"):
        raise RuntimeError("public discovery response has no rsa_spki")


def test_websocket(host, port):
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
        "Origin: https://web.example.test\r\n"
        "CF-Connecting-IP: 192.0.2.1\r\n"
        "\r\n"
    ).encode("ascii")
    with socket.create_connection((host, port), timeout=10) as connection:
        connection.settimeout(10)
        connection.sendall(request)
        headers, pending = receive_headers(connection)
        lines = headers.split("\r\n")
        if not lines or " 101 " not in f" {lines[0]} ":
            raise RuntimeError(f"public WebSocket did not upgrade: {lines[0] if lines else 'empty response'}")
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
        test_websocket(args.host, args.port)
    except (OSError, RuntimeError, urllib.error.URLError, json.JSONDecodeError) as error:
        print(f"public connector check failed: {error}", file=sys.stderr)
        return 1
    print("public discovery and WebSocket ping/pong passed through the connector")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
