#!/usr/bin/env python3
"""Regression coverage for inspecting the live Compose selection."""

from __future__ import annotations

import argparse
import contextlib
import importlib.util
import io
import json
import os
import pathlib
import shutil
import tempfile
import unittest
from unittest import mock


HELPER = pathlib.Path(__file__).with_name("stack-move-runner.py")
LOCAL_COMPOSE = pathlib.Path(__file__).with_name("local-compose-cce00d1.yml")


def load_runner():
    spec = importlib.util.spec_from_file_location("stack_move_runner", HELPER)
    if spec is None or spec.loader is None:
        raise AssertionError("could not load stack-move runner")
    runner = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(runner)
    return runner


class InspectComposeSelectionTest(unittest.TestCase):
    def test_pinned_local_compose_is_used_for_both_layouts_without_s3_env(self) -> None:
        runner = load_runner()
        with tempfile.TemporaryDirectory(prefix="stack-move-inspect-") as temporary:
            checkout = pathlib.Path(temporary)
            local_compose = checkout / ".rollout-compose.local-cce00d1.yml"
            shutil.copyfile(LOCAL_COMPOSE, local_compose)
            override = checkout / "docker-compose.override.yml"
            override.write_text("services:\n  telegramd:\n    ports: []\n", encoding="utf-8")
            env_file = checkout / ".env"
            env_file.write_text(
                "POSTGRES_PASSWORD=fixture-only\nTG_PUBLIC_LINK_PREFIX=https://example.invalid/\n",
                encoding="utf-8",
            )
            default_compose = checkout / "docker-compose.yml"
            default_compose.write_text(
                "services:\n"
                "  blob-migrate:\n"
                "    environment:\n"
                "      TG_BLOB_S3_ACCESS_KEY_ID: ${TG_BLOB_S3_ACCESS_KEY_ID:?required}\n",
                encoding="utf-8",
            )

            render_selections: list[list[pathlib.Path]] = []
            original_output = runner.output

            def compose_output(command: list[str], **_kwargs: object) -> str:
                files = [
                    pathlib.Path(command[index + 1]).resolve()
                    for index, value in enumerate(command[:-1])
                    if value == "--file"
                ]
                render_selections.append(files)
                if default_compose.resolve() in files:
                    raise runner.MoveError(
                        "required variable TG_BLOB_S3_ACCESS_KEY_ID is missing"
                    )
                return original_output(command, **_kwargs)

            args = argparse.Namespace(
                checkout=checkout,
                target_sha="1234567890abcdef1234567890abcdef12345678",
                env_file=env_file,
                override=override,
            )
            output = io.StringIO()
            with mock.patch.dict(
                os.environ,
                {
                    "COMPOSE_FILE": os.pathsep.join((str(local_compose), str(override))),
                    "PATH": os.environ.get("PATH", os.defpath),
                },
                clear=True,
            ), mock.patch.object(runner, "output", side_effect=compose_output), \
                    mock.patch.object(
                        runner,
                        "inspect_git_target",
                        side_effect=AssertionError("live pinned selection must not use default Compose"),
                    ), contextlib.redirect_stdout(output):
                self.assertFalse(any(name.startswith("TG_BLOB_S3_") for name in os.environ))
                self.assertNotIn("TG_BLOB_S3_", local_compose.read_text(encoding="utf-8"))
                runner.inspect(args)

            result = json.loads(output.getvalue())
            self.assertFalse(result["changed"])
            self.assertEqual(result["source_database"], "telegram")
            self.assertEqual(result["target_database"], "telegram")
            self.assertEqual(render_selections[0], [local_compose.resolve(), override.resolve()])
            self.assertEqual(render_selections[1], [local_compose.resolve()])
            self.assertEqual(render_selections[2][0], local_compose.resolve())
            self.assertEqual(render_selections[2][1].name, "docker-compose.override.yml")
            self.assertNotEqual(render_selections[2][1], override.resolve())
            print("PASS apply layout preflight uses the live local-blob selection without S3 credentials")


if __name__ == "__main__":
    unittest.main()
