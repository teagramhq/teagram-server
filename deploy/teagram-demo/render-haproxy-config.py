#!/usr/bin/env python3
"""Render the persisted HAProxy slot selection for the demo stack."""

from __future__ import annotations

import os
import pathlib
import sys
import tempfile


def fail(message: str) -> None:
    print(f"teagram-demo HAProxy config: {message}", file=sys.stderr)
    raise SystemExit(1)


def main() -> None:
    if len(sys.argv) != 4:
        fail("usage: render-haproxy-config.py <active-service> <template> <destination>")
    active, template_arg, destination_arg = sys.argv[1:]
    if active not in {"teagramd", "teagramd-next"}:
        fail("invalid active service")

    template = pathlib.Path(template_arg)
    destination = pathlib.Path(destination_arg)
    try:
        contents = template.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError):
        fail("template is unavailable")

    markers = {"@TEAGRAMD_DISABLED@", "@TEAGRAMD_NEXT_DISABLED@"}
    if any(contents.count(marker) != 2 for marker in markers):
        fail("template markers are invalid")
    contents = contents.replace(
        "@TEAGRAMD_DISABLED@", "" if active == "teagramd" else " disabled"
    )
    contents = contents.replace(
        "@TEAGRAMD_NEXT_DISABLED@", "" if active == "teagramd-next" else " disabled"
    )

    destination.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
    if destination.parent.is_symlink() or not destination.parent.is_dir():
        fail("destination directory must be a real directory")
    if destination.is_symlink():
        fail("destination must not be a symlink")
    try:
        with tempfile.NamedTemporaryFile(
            mode="w",
            encoding="utf-8",
            dir=destination.parent,
            prefix=f".{destination.name}.",
            delete=False,
        ) as stream:
            temporary = pathlib.Path(stream.name)
            os.fchmod(stream.fileno(), 0o644)
            stream.write(contents)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, destination)
        if os.geteuid() == 0:
            os.chown(destination, 0, 0)
    except OSError:
        try:
            temporary.unlink(missing_ok=True)
        except (UnboundLocalError, OSError):
            pass
        fail("could not write the active configuration")


if __name__ == "__main__":
    main()
