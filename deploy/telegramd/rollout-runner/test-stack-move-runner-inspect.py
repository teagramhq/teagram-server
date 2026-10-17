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

    def test_non_pinned_selection_renders_target_sha_compose(self) -> None:
        runner = load_runner()
        with tempfile.TemporaryDirectory(prefix="stack-move-inspect-target-") as temporary:
            checkout = pathlib.Path(temporary)
            default_compose = checkout / "docker-compose.yml"
            default_compose.write_text(
                "name: telegram-server\n"
                "services:\n"
                "  postgres:\n"
                "    image: postgres:16-alpine\n"
                "    environment:\n"
                "      POSTGRES_DB: telegram\n"
                "      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?required}\n"
                "    volumes:\n"
                "      - pgdata:/var/lib/postgresql/data\n"
                "  telegramd:\n"
                "    image: telegramd:local\n"
                "    volumes:\n"
                "      - tgkey:/var/lib/telegramd\n"
                "volumes:\n"
                "  pgdata:\n"
                "    name: telegram-server_pgdata\n"
                "  tgkey:\n"
                "    name: telegram-server_tgkey\n",
                encoding="utf-8",
            )
            local_blobs = checkout / "docker-compose.local-blobs.yml"
            local_blobs.write_text(
                "services:\n"
                "  telegramd:\n"
                "    environment:\n"
                "      TG_BLOB_DIR: /var/lib/telegramd-blobs\n"
                "    volumes:\n"
                "      - tgblobs:/var/lib/telegramd-blobs\n"
                "volumes:\n"
                "  tgblobs:\n",
                encoding="utf-8",
            )
            override = checkout / "docker-compose.override.yml"
            override.write_text(
                "services:\n  telegramd:\n    ports: [\"127.0.0.1:2443:2443\"]\n",
                encoding="utf-8",
            )
            env_file = checkout / ".env"
            env_file.write_text("POSTGRES_PASSWORD=fixture-only\n", encoding="utf-8")
            target_compose_body = (
                "name: teagram\n"
                "services:\n"
                "  postgres:\n"
                "    image: postgres:16-alpine\n"
                "    environment:\n"
                "      POSTGRES_DB: telegram\n"
                "      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?required}\n"
                "    volumes:\n"
                "      - pgdata:/var/lib/postgresql/data\n"
                "  telegramd:\n"
                "    image: telegramd:local\n"
                "    volumes:\n"
                "      - tgkey:/var/lib/telegramd\n"
                "volumes:\n"
                "  pgdata:\n"
                "    name: teagram_pgdata\n"
                "  tgkey:\n"
                "    name: teagram_tgkey\n"
                "  tgblobs:\n"
                "    name: teagram_tgblobs\n"
            )
            target_sha = "1234567890abcdef1234567890abcdef12345678"
            render_selections: list[list[pathlib.Path]] = []
            original_output = runner.output

            def compose_output(command: list[str], **_kwargs: object) -> str:
                files = [
                    pathlib.Path(command[index + 1]).resolve()
                    for index, value in enumerate(command[:-1])
                    if value == "--file"
                ]
                render_selections.append(files)
                return original_output(command, **_kwargs)

            def target_compose(
                target_checkout: pathlib.Path,
                sha: str,
                destination: pathlib.Path,
            ) -> pathlib.Path:
                self.assertEqual(target_checkout, checkout)
                self.assertEqual(sha, target_sha)
                target_path = destination / "target-docker-compose.yml"
                target_path.write_text(target_compose_body, encoding="utf-8")
                return target_path

            args = argparse.Namespace(
                checkout=checkout,
                target_sha=target_sha,
                env_file=env_file,
                override=override,
            )
            output = io.StringIO()
            with mock.patch.dict(
                os.environ,
                {
                    "COMPOSE_FILE": os.pathsep.join((
                        str(default_compose), str(local_blobs), str(override),
                    )),
                    "PATH": os.environ.get("PATH", os.defpath),
                },
                clear=True,
            ), mock.patch.object(runner, "output", side_effect=compose_output), \
                    mock.patch.object(runner, "inspect_git_target", side_effect=target_compose) as inspect_target, \
                    contextlib.redirect_stdout(output):
                self.assertFalse(any(name.startswith("TG_BLOB_S3_") for name in os.environ))
                runner.inspect(args)

            result = json.loads(output.getvalue())
            inspect_target.assert_called_once()
            self.assertTrue(result["changed"])
            self.assertEqual(result["source"]["project"], "telegram-server")
            self.assertEqual(result["target"]["project"], "teagram")
            self.assertEqual(result["source"]["volumes"]["pgdata"], "telegram-server_pgdata")
            self.assertEqual(result["target"]["volumes"]["pgdata"], "teagram_pgdata")
            self.assertEqual(render_selections[0], [
                default_compose.resolve(), local_blobs.resolve(), override.resolve(),
            ])
            self.assertEqual(render_selections[1][1], local_blobs.resolve())
            self.assertEqual(render_selections[2][1], local_blobs.resolve())
            self.assertEqual(render_selections[1][0].name, "target-docker-compose.yml")
            self.assertEqual(render_selections[2][0].name, "target-docker-compose.yml")
            print("PASS non-pinned Compose selection renders the target SHA layout")


if __name__ == "__main__":
    unittest.main()
