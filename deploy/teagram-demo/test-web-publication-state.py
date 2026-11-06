#!/usr/bin/env python3
"""Exercise persistent publication ordering and interrupted deployments."""

import importlib.util
import pathlib
import tempfile


SCRIPT = pathlib.Path(__file__).with_name("web-publication-state.py")
SPEC = importlib.util.spec_from_file_location("web_publication_state", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


def identity(run_id: int, char: str) -> dict:
    return {"run_id": run_id, "source_commit": char * 40, "artifact_digest": "sha256:" + char * 64}


def rejected(fn) -> None:
    try:
        fn()
    except ValueError:
        return
    raise SystemExit("publication state accepted an unsafe transition")


def main() -> None:
    old, newer, stale = identity(100, "a"), identity(101, "b"), identity(99, "c")
    with tempfile.TemporaryDirectory() as directory:
        path = pathlib.Path(directory) / "publication.json"
        rejected(lambda: MODULE.prepare(path, old, stale, False))
        if MODULE.prepare(path, old, old, False) != "noop":
            raise SystemExit("the installed publication was not a no-op")
        rejected(lambda: MODULE.prepare(path, old, identity(100, "c"), False))
        if MODULE.prepare(path, old, newer, False) != "install":
            raise SystemExit("a newer publication was not admitted")
        if MODULE.prepare(path, old, newer, False) != "install":
            raise SystemExit("an interrupted publication could not resume")
        rejected(lambda: MODULE.prepare(path, newer, stale, True))
        if MODULE.prepare(path, newer, newer, False) != "install":
            raise SystemExit("an activated but uncommitted publication could not be recovered")
        MODULE.commit(path, newer)
        if MODULE.prepare(path, newer, newer, False) != "noop":
            raise SystemExit("a committed publication was not a no-op")
        rejected(lambda: MODULE.prepare(path, newer, stale, False))
        if MODULE.prepare(path, newer, stale, True) != "install":
            raise SystemExit("an authorized rollback was not admitted")
        MODULE.commit(path, stale)
        rejected(lambda: MODULE.prepare(path, stale, identity(100, "a"), False))
        if MODULE.prepare(path, stale, stale, False) != "noop":
            raise SystemExit("the installed rollback was not a no-op")
        path.unlink()
        rejected(lambda: MODULE.prepare(path, stale, newer, False))
    with tempfile.TemporaryDirectory() as directory:
        path = pathlib.Path(directory) / "publication.json"
        MODULE.prepare(path, old, old, False)
        path.write_text("{}", encoding="utf-8")
        rejected(lambda: MODULE.prepare(path, old, newer, False))
    with tempfile.TemporaryDirectory() as directory:
        path = pathlib.Path(directory) / "publication.json"
        MODULE.prepare(path, old, old, False)
        path.with_name(path.name + ".guard").unlink()
        rejected(lambda: MODULE.prepare(path, old, newer, False))
    print("demo web publication ordering checks passed")


if __name__ == "__main__":
    main()
