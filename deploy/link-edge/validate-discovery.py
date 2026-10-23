#!/usr/bin/env python3
"""Validate the public discovery document used by the link-edge deploy gate."""

import base64
import binascii
import ipaddress
import json
import re
import subprocess
import sys
import tempfile
from pathlib import Path
from urllib.parse import urlsplit

MAX_DC_ID = (1 << 31) - 1
MAX_SPKI_LENGTH = 4096
MAX_SPKI_BASE64_LENGTH = 4 * ((MAX_SPKI_LENGTH + 2) // 3)
OPENSSL = "/usr/bin/openssl"


class InvalidDiscovery(ValueError):
    pass


def validate_host(host):
    if not host or any(char in host for char in "\r\n\t /?#@%"):
        raise InvalidDiscovery("endpoint host is invalid")

    try:
        address = ipaddress.ip_address(host)
    except ValueError:
        try:
            host_length = len(host.encode("ascii"))
        except UnicodeEncodeError as error:
            raise InvalidDiscovery("endpoint DNS name must be ASCII") from error
        if host_length > 253:
            raise InvalidDiscovery("endpoint DNS name exceeds 253 octets")
        for label in host.split("."):
            if (
                not 1 <= len(label) <= 63
                or label[0] == "-"
                or label[-1] == "-"
                or re.fullmatch(r"[A-Za-z0-9-]+", label) is None
            ):
                raise InvalidDiscovery("endpoint DNS labels are invalid")
    else:
        if address.is_unspecified or address.is_multicast:
            raise InvalidDiscovery("endpoint IP must not be unspecified or multicast")


def split_host_port(endpoint):
    if not isinstance(endpoint, str) or not endpoint or endpoint.strip() != endpoint:
        raise InvalidDiscovery("MTProto endpoint must be host:port")

    if endpoint.startswith("["):
        closing_bracket = endpoint.find("]")
        if closing_bracket < 0 or endpoint[closing_bracket + 1 : closing_bracket + 2] != ":":
            raise InvalidDiscovery("MTProto endpoint must be host:port")
        host = endpoint[1:closing_bracket]
        port_text = endpoint[closing_bracket + 2 :]
        if "[" in host or "]" in port_text:
            raise InvalidDiscovery("MTProto endpoint must be host:port")
    else:
        if endpoint.count(":") != 1:
            raise InvalidDiscovery("MTProto endpoint must be host:port")
        host, port_text = endpoint.split(":", 1)

    if not port_text or not all("0" <= char <= "9" for char in port_text):
        raise InvalidDiscovery("MTProto endpoint port must be an integer")
    try:
        port = int(port_text)
    except ValueError as error:
        raise InvalidDiscovery("MTProto endpoint port must be an integer") from error
    if not 1 <= port <= 65535:
        raise InvalidDiscovery("MTProto endpoint port must be between 1 and 65535")

    validate_host(host)
    return host


def validate_websocket_endpoint(endpoint):
    if not isinstance(endpoint, str) or not endpoint.startswith("wss://"):
        raise InvalidDiscovery("WebSocket endpoint must use wss://")
    try:
        parsed = urlsplit(endpoint)
    except ValueError as error:
        raise InvalidDiscovery("WebSocket endpoint is malformed") from error

    if (
        parsed.scheme != "wss"
        or not parsed.netloc
        or parsed.username is not None
        or parsed.password is not None
        or parsed.path != "/apiws"
        or parsed.query
        or parsed.fragment
        or "?" in endpoint
        or "#" in endpoint
    ):
        raise InvalidDiscovery(
            "WebSocket endpoint must be wss://host/apiws without a port, query, or fragment"
        )

    if parsed.netloc.startswith("["):
        if not parsed.netloc.endswith("]"):
            raise InvalidDiscovery("WebSocket endpoint must use canonical host form")
        host = parsed.netloc[1:-1]
        try:
            address = ipaddress.ip_address(host)
        except ValueError as error:
            raise InvalidDiscovery("WebSocket endpoint IPv6 host is invalid") from error
        if not isinstance(address, ipaddress.IPv6Address):
            raise InvalidDiscovery("WebSocket endpoint bracketed host must be IPv6")
        validate_host(host)
    else:
        if ":" in parsed.netloc or "[" in parsed.netloc or "]" in parsed.netloc:
            raise InvalidDiscovery("WebSocket endpoint must not include a port")
        host = parsed.netloc
        validate_host(host)

    if endpoint != f"wss://{parsed.netloc}/apiws":
        raise InvalidDiscovery("WebSocket endpoint must use canonical host form")


def validate_spki(encoded):
    if not isinstance(encoded, str) or not encoded or len(encoded) > MAX_SPKI_BASE64_LENGTH:
        raise InvalidDiscovery("SPKI must be standard padded base64 DER")
    try:
        der = base64.b64decode(encoded, validate=True)
    except (binascii.Error, ValueError) as error:
        raise InvalidDiscovery("SPKI must be standard padded base64 DER") from error
    if base64.b64encode(der).decode("ascii") != encoded or not 1 <= len(der) <= MAX_SPKI_LENGTH:
        raise InvalidDiscovery("SPKI must be canonical base64 DER of at most 4096 bytes")

    with tempfile.TemporaryDirectory(prefix="teagram-discovery-") as temp_dir:
        der_path = Path(temp_dir) / "spki.der"
        der_path.write_bytes(der)
        try:
            result = subprocess.run(
                [
                    OPENSSL,
                    "rsa",
                    "-pubin",
                    "-inform",
                    "DER",
                    "-in",
                    str(der_path),
                    "-noout",
                    "-text",
                ],
                check=False,
                capture_output=True,
                text=True,
                timeout=10,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise InvalidDiscovery("could not validate SPKI with OpenSSL") from error
        if result.returncode != 0:
            raise InvalidDiscovery("SPKI is not an RSA SubjectPublicKeyInfo")

        key_size = re.search(r"^Public-Key: \((\d+) bit\)$", result.stdout, re.MULTILINE)
        exponent_match = re.search(r"^Exponent: (\d+) \(", result.stdout, re.MULTILINE)
        if key_size is None or int(key_size.group(1)) != 2048 or exponent_match is None:
            raise InvalidDiscovery("SPKI must contain a 2048-bit RSA key")

        exponent = int(exponent_match.group(1))
        if exponent < 3 or exponent % 2 == 0 or exponent > MAX_DC_ID:
            raise InvalidDiscovery("RSA public exponent is invalid")

        modulus_hex = []
        reading_modulus = False
        for line in result.stdout.splitlines():
            if line.strip() == "Modulus:":
                reading_modulus = True
                continue
            if reading_modulus and line.startswith("Exponent:"):
                break
            if reading_modulus:
                chunk = line.strip().replace(":", "")
                if chunk:
                    modulus_hex.append(chunk)
        try:
            modulus = int("".join(modulus_hex), 16)
        except ValueError as error:
            raise InvalidDiscovery("SPKI RSA modulus is malformed") from error
        if modulus.bit_length() != 2048 or modulus % 2 == 0:
            raise InvalidDiscovery("SPKI RSA modulus must be positive, odd, and 2048-bit")

        try:
            canonical = subprocess.run(
                [
                    OPENSSL,
                    "pkey",
                    "-pubin",
                    "-inform",
                    "DER",
                    "-in",
                    str(der_path),
                    "-pubout",
                    "-outform",
                    "DER",
                ],
                check=False,
                capture_output=True,
                timeout=10,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise InvalidDiscovery("could not canonicalize SPKI with OpenSSL") from error
        if canonical.returncode != 0 or canonical.stdout != der:
            raise InvalidDiscovery("SPKI must use canonical DER encoding")


def validate_document(document):
    if (
        not isinstance(document, dict)
        or type(document.get("version")) is not int
        or document["version"] != 1
    ):
        raise InvalidDiscovery("document version must be 1")

    mtproto = document.get("mtproto")
    websocket = document.get("websocket")
    if not isinstance(mtproto, dict) or not isinstance(websocket, dict):
        raise InvalidDiscovery("document must contain MTProto and WebSocket objects")

    endpoint = mtproto.get("endpoint")
    dc_id = mtproto.get("dc_id")
    spki = mtproto.get("rsa_spki")
    websocket_endpoint = websocket.get("endpoint")
    if type(dc_id) is not int or not 1 <= dc_id <= MAX_DC_ID:
        raise InvalidDiscovery("DC id must be a positive int32")

    split_host_port(endpoint)
    validate_websocket_endpoint(websocket_endpoint)
    validate_spki(spki)


def main():
    try:
        document = json.load(sys.stdin)
        validate_document(document)
    except (ValueError, UnicodeDecodeError, OSError, RecursionError) as error:
        print(f"invalid discovery document: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
