#!/usr/bin/env python3
"""Validate a Bot API token and emit the QA account-writer record."""

import base64
import binascii
import re
import sys


TOKEN_PATTERN = re.compile(r"([1-9][0-9]{0,18}):([A-Za-z0-9_-]{43})\Z")


def build_qa_bot_record(token: str) -> str:
    match = TOKEN_PATTERN.fullmatch(token)
    if not match:
        raise ValueError("invalid Bot API token")

    bot_id, encoded_secret = match.groups()
    secret = base64.b64decode(encoded_secret + "=", altchars=b"-_", validate=True)
    if len(secret) != 32 or base64.urlsafe_b64encode(secret).decode("ascii").rstrip("=") != encoded_secret:
        raise ValueError("invalid Bot API token")

    return f"bot_{bot_id}\t{secret.hex()}\n"


def main() -> int:
    token = sys.stdin.read()
    if token.endswith("\n"):
        token = token[:-1]
    try:
        record = build_qa_bot_record(token)
    except (ValueError, binascii.Error):
        print("invalid Bot API token", file=sys.stderr)
        return 1
    sys.stdout.write(record)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
