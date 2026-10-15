#!/usr/bin/env python3
"""Turn the Claude review log into a PR comment body and a verdict output.

Usage: claude_review_verdict.py <execution-file> <body-file>

Writes verdict=blocking|clean|none to $GITHUB_OUTPUT and always exits 0: only
an explicit BLOCKING verdict may fail the check, and a later step does that.
"""

from __future__ import annotations

import json
import os
import sys
from pathlib import Path

VERDICTS = {
    "CLAUDE_REVIEW: BLOCKING": "blocking",
    "CLAUDE_REVIEW: CLEAN": "clean",
}


def final_text(execution_file: str) -> str:
    """Return the review text of a successful run, or "" for anything else."""
    if not execution_file:
        return ""
    try:
        messages = json.loads(Path(execution_file).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return ""
    if not isinstance(messages, list):
        return ""
    for message in reversed(messages):
        if isinstance(message, dict) and message.get("type") == "result":
            if message.get("subtype") != "success" or message.get("is_error"):
                return ""
            result = message.get("result")
            return result if isinstance(result, str) else ""
    return ""


def verdict_of(text: str) -> str:
    lines = [line.strip() for line in text.splitlines() if line.strip()]
    if not lines:
        return "none"
    return VERDICTS.get(lines[-1].strip("`*_ "), "none")


def main(argv: list[str]) -> int:
    if len(argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    text = final_text(argv[1])
    verdict = verdict_of(text)
    body = Path(argv[2])
    if verdict == "none":
        print("::warning::Claude review gave no verdict (timeout, auth or quota error, or a malformed reply); not blocking.")
        if text:
            print(text)
    else:
        head = os.environ.get("HEAD_SHA", "")[:12]
        title = f"**Claude review** of `{head}`" if head else "**Claude review**"
        body.write_text(f"{title}\n\n{text.strip()}\n", encoding="utf-8")
    output = os.environ.get("GITHUB_OUTPUT")
    if output:
        with open(output, "a", encoding="utf-8") as handle:
            handle.write(f"verdict={verdict}\n")
    else:
        print(f"verdict={verdict}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
