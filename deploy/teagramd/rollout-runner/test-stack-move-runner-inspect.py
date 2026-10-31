#!/usr/bin/env python3
"""Regression coverage for live Compose selection during inspect and move."""

from __future__ import annotations

import argparse
import contextlib
import copy
import importlib.machinery
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


def load_runner(path: pathlib.Path = HELPER):
    loader = importlib.machinery.SourceFileLoader("stack_move_runner", str(path))
    spec = importlib.util.spec_from_loader("stack_move_runner", loader)
    if spec is None or spec.loader is None:
        raise AssertionError("could not load stack-move runner")
    runner = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(runner)
    return runner


class InspectComposeSelectionTest(unittest.TestCase):
    def test_public_profile_addition_accepts_local_blobs_private_override_layout(self) -> None:
        runner = load_runner()
        shared_mounts = [
            {"type": "volume", "source": "tgkey", "target": "/var/lib/teagramd", "read_only": False},
            {"type": "volume", "source": "tgblobs", "target": "/var/lib/teagramd-blobs", "read_only": True},
            {
                "type": "bind",
                "source": "/opt/teagram/.state/blob-mode",
                "target": "/run/teagramd/blob-mode",
                "read_only": True,
                "bind": {"create_host_path": False},
            },
        ]
        private_media_mount = {
            "type": "bind",
            "source": "/etc/teagramd/media-keys",
            "target": "/run/teagramd/media-keys",
            "read_only": True,
            "bind": {},
        }
        postgres_mounts = [
            {"type": "volume", "source": "pgdata", "target": "/var/lib/postgresql/18/docker", "read_only": False},
            {
                "type": "bind",
                "source": "/opt/teagram/deploy/postgres/postgresql.conf",
                "target": "/etc/postgresql/postgresql.conf",
                "read_only": True,
                "bind": {},
            },
        ]
        target_config = {
            "name": "teagram",
            "services": {
                "postgres": {"volumes": copy.deepcopy(postgres_mounts)},
                "teagramd": {
                    "environment": {
                        "TG_BLOB_DIR": "/var/lib/teagramd-blobs",
                        "TG_BLOB_S3_ENDPOINT": None,
                        "TG_MEDIA_KEY_DIR": None,
                        "TG_MEDIA_KEY_CURRENT": None,
                    },
                    "volumes": [*copy.deepcopy(shared_mounts), copy.deepcopy(private_media_mount)],
                },
                "public-haproxy": {
                    "profiles": ["public"],
                    "volumes": [{
                        "type": "bind",
                        "source": "/opt/teagram/deploy/teagramd/haproxy-public-websocket.cfg",
                        "target": "/usr/local/etc/haproxy/haproxy.cfg",
                        "read_only": True,
                        "bind": {"create_host_path": False},
                    }],
                },
                "teagramd-public-1": {
                    "profiles": ["public"],
                    "volumes": copy.deepcopy(shared_mounts),
                },
                "teagramd-public-2": {
                    "profiles": ["public"],
                    "volumes": copy.deepcopy(shared_mounts),
                },
            },
            "volumes": {
                "pgdata": {"name": "teagram_pgdata"},
                "tgkey": {"name": "teagram_tgkey"},
                "tgblobs": {"name": "teagram_tgblobs"},
            },
        }
        source_config = copy.deepcopy(target_config)
        for name in ("public-haproxy", "teagramd-public-1", "teagramd-public-2"):
            source_config["services"].pop(name)

        source = runner.layout(source_config, "teagramd")
        target = runner.layout(target_config, "teagramd")
        with mock.patch.dict(os.environ, {
            "COMPOSE_PROFILES": "public",
            "ROLLOUT_RUNNER_PUBLIC_WEBSOCKET": "1",
        }, clear=True):
            self.assertFalse(runner.ordinary_rollout_layout_changed(source, target, target_config))

            configured_media_keys = copy.deepcopy(target_config)
            configured_media_keys["services"]["teagramd"]["environment"][
                "TG_MEDIA_KEY_DIR"
            ] = "/run/teagramd/media-keys"
            configured_media_keys["services"]["teagramd"]["environment"][
                "TG_MEDIA_KEY_CURRENT"
            ] = "1"
            self.assertTrue(
                runner.ordinary_rollout_layout_changed(
                    source, runner.layout(configured_media_keys, "teagramd"), configured_media_keys,
                )
            )

            changed_project = copy.deepcopy(target_config)
            changed_project["name"] = "another-project"
            self.assertTrue(
                runner.ordinary_rollout_layout_changed(
                    source, runner.layout(changed_project, "teagramd"), changed_project,
                )
            )

            changed_persistent_mount = copy.deepcopy(target_config)
            changed_persistent_mount["services"]["teagramd"]["volumes"][0]["source"] = "other-key"
            changed_persistent_mount["services"]["teagramd"]["environment"][
                "TG_MEDIA_KEY_DIR"
            ] = "secret-env-value-sentinel"
            self.assertTrue(
                runner.ordinary_rollout_layout_changed(
                    source, runner.layout(changed_persistent_mount, "teagramd"), changed_persistent_mount,
                )
            )
            differences = runner.sanitized_layout_differences(
                source, runner.layout(changed_persistent_mount, "teagramd"), changed_persistent_mount,
            )
            self.assertIn({
                "service": "teagramd",
                "mount_target": "/var/lib/teagramd",
                "field": "source",
            }, differences)
            self.assertIn({
                "service": "teagramd",
                "mount_target": "/run/teagramd/media-keys",
                "field": "media_key_environment",
            }, differences)
            self.assertNotIn("secret-env-value-sentinel", json.dumps(differences))

            changed_volume = copy.deepcopy(target_config)
            changed_volume["volumes"]["tgkey"]["name"] = "teagram_other_tgkey"
            self.assertTrue(
                runner.ordinary_rollout_layout_changed(
                    source, runner.layout(changed_volume, "teagramd"), changed_volume,
                )
            )

    def test_public_profile_addition_reuses_existing_data_mounts(self) -> None:
        runner = load_runner()
        server_mounts = [
            {"type": "volume", "source": "tgkey", "target": "/var/lib/teagramd", "read_only": False},
            {"type": "volume", "source": "tgblobs", "target": "/var/lib/teagramd-blobs", "read_only": True},
        ]
        postgres_mounts = [
            {"type": "volume", "source": "pgdata", "target": "/var/lib/postgresql/18/docker", "read_only": False},
        ]
        source_config = {
            "name": "teagram",
            "services": {
                "postgres": {"volumes": copy.deepcopy(postgres_mounts)},
                "teagramd": {
                    "environment": {
                        "TG_MEDIA_KEY_DIR": "/run/teagramd/media-keys",
                        "TG_MEDIA_KEY_CURRENT": "1",
                    },
                    "volumes": copy.deepcopy(server_mounts),
                },
            },
            "volumes": {
                "pgdata": {"name": "teagram_pgdata"},
                "tgkey": {"name": "teagram_tgkey"},
                "tgblobs": {"name": "teagram_tgblobs"},
            },
        }
        target_config = copy.deepcopy(source_config)
        target_config["services"].update({
            "public-haproxy": {
                "profiles": ["public"],
                "volumes": [{
                    "type": "bind",
                    "source": "/opt/teagram/deploy/teagramd/haproxy-public-websocket.cfg",
                    "target": "/usr/local/etc/haproxy/haproxy.cfg",
                    "read_only": True,
                    "bind": {"create_host_path": False},
                }],
            },
            "teagramd-public-1": {
                "profiles": ["public"],
                "volumes": copy.deepcopy(server_mounts),
            },
            "teagramd-public-2": {
                "profiles": ["public"],
                "volumes": copy.deepcopy(server_mounts),
            },
        })

        source = runner.layout(source_config, "teagramd")
        target = runner.layout(target_config, "teagramd")

        with mock.patch.dict(os.environ, {
            "COMPOSE_PROFILES": "public",
            "ROLLOUT_RUNNER_PUBLIC_WEBSOCKET": "1",
        }, clear=True):
            self.assertFalse(runner.ordinary_rollout_layout_changed(source, target, target_config))

            unprofiled = copy.deepcopy(target_config)
            unprofiled["services"]["teagramd-public-1"]["profiles"] = []
            self.assertTrue(
                runner.ordinary_rollout_layout_changed(
                    source, runner.layout(unprofiled, "teagramd"), unprofiled,
                )
            )

            changed_replica_mount = copy.deepcopy(target_config)
            changed_replica_mount["services"]["teagramd-public-1"]["volumes"].append(
                {"type": "volume", "source": "newdata", "target": "/data", "read_only": False},
            )
            self.assertTrue(
                runner.ordinary_rollout_layout_changed(
                    source, runner.layout(changed_replica_mount, "teagramd"), changed_replica_mount,
                )
            )

            added_proxy_mount = copy.deepcopy(target_config)
            added_proxy_mount["services"]["public-haproxy"]["volumes"].append(
                {"type": "volume", "source": "newdata", "target": "/data", "read_only": False},
            )
            self.assertTrue(
                runner.ordinary_rollout_layout_changed(
                    source, runner.layout(added_proxy_mount, "teagramd"), added_proxy_mount,
                )
            )

            added_volume = copy.deepcopy(target_config)
            added_volume["volumes"]["newdata"] = {"name": "teagram_newdata"}
            self.assertTrue(
                runner.ordinary_rollout_layout_changed(
                    source, runner.layout(added_volume, "teagramd"), added_volume,
                )
            )

            extra_service = copy.deepcopy(target_config)
            extra_service["services"]["unexpected"] = {
                "profiles": ["public"],
                "volumes": [{"type": "volume", "source": "newdata", "target": "/data"}],
            }
            self.assertTrue(
                runner.ordinary_rollout_layout_changed(
                    source, runner.layout(extra_service, "teagramd"), extra_service,
                )
            )

        for environment in (
            {"COMPOSE_PROFILES": "public"},
            {"ROLLOUT_RUNNER_PUBLIC_WEBSOCKET": "1"},
        ):
            with mock.patch.dict(os.environ, environment, clear=True):
                self.assertTrue(runner.ordinary_rollout_layout_changed(source, target, target_config))

    def test_only_additive_read_only_media_key_mount_is_a_safe_layout_change(self) -> None:
        runner = load_runner()
        key_mount = {
            "type": "bind",
            "source": "/etc/teagramd/media-keys",
            "target": "/run/teagramd/media-keys",
            "read_only": True,
            "bind": {"create_host_path": False},
        }
        source = {
            "project": "teagram",
            "server": "teagramd",
            "server_mounts": [
                {"type": "volume", "source": "tgkey", "target": "/var/lib/teagramd", "read_only": False},
            ],
            "postgres_mounts": [
                {"type": "volume", "source": "pgdata", "target": "/var/lib/postgresql/18/docker"},
            ],
            "persistent_service_mounts": {
                "postgres": [
                    {"type": "volume", "source": "pgdata", "target": "/var/lib/postgresql/18/docker"},
                ],
                "teagramd": [
                    {"type": "volume", "source": "tgkey", "target": "/var/lib/teagramd", "read_only": False},
                ],
            },
            "volumes": {"pgdata": "teagram_pgdata", "tgkey": "teagram_tgkey"},
        }
        target = copy.deepcopy(source)
        target["server_mounts"].append(key_mount)
        target["persistent_service_mounts"]["teagramd"].append(key_mount)

        self.assertFalse(runner.layout_changed(source, target))

        normalized_bind = copy.deepcopy(target)
        normalized_bind["server_mounts"][-1]["bind"] = {}
        normalized_bind["persistent_service_mounts"]["teagramd"][-1]["bind"] = {}
        self.assertFalse(runner.layout_changed(source, normalized_bind))

        writable_key_mount = copy.deepcopy(target)
        writable_key_mount["server_mounts"][-1]["read_only"] = False
        writable_key_mount["persistent_service_mounts"]["teagramd"][-1]["read_only"] = False
        self.assertTrue(runner.layout_changed(source, writable_key_mount))

        implicit_host_path = copy.deepcopy(target)
        implicit_host_path["server_mounts"][-1]["bind"]["create_host_path"] = True
        implicit_host_path["persistent_service_mounts"]["teagramd"][-1]["bind"]["create_host_path"] = True
        self.assertTrue(runner.layout_changed(source, implicit_host_path))

        alternate_key_source = copy.deepcopy(target)
        alternate_key_source["server_mounts"][-1]["source"] = "/var/lib/teagram/media-keys"
        alternate_key_source["persistent_service_mounts"]["teagramd"][-1]["source"] = (
            "/var/lib/teagram/media-keys"
        )
        self.assertTrue(runner.layout_changed(source, alternate_key_source))

        changed_project = copy.deepcopy(target)
        changed_project["project"] = "another-project"
        self.assertTrue(runner.layout_changed(source, changed_project))

        added_data_mount = copy.deepcopy(target)
        extra_mount = {"type": "volume", "source": "newdata", "target": "/data"}
        added_data_mount["server_mounts"].append(extra_mount)
        added_data_mount["persistent_service_mounts"]["teagramd"].append(extra_mount)
        self.assertTrue(runner.layout_changed(source, added_data_mount))
        print("PASS layout preflight permits only the exact read-only media-key mount addition")

    def test_read_only_media_key_mount_is_not_a_data_layout_change(self) -> None:
        runner = load_runner()
        source = {
            "project": "teagram",
            "server": "teagramd",
            "server_mounts": [
                {"type": "volume", "source": "teagram_tgkey", "target": "/var/lib/teagramd", "read_only": False},
                {"type": "volume", "source": "teagram_tgblobs", "target": "/var/lib/teagramd-blobs", "read_only": True},
            ],
            "postgres_mounts": [
                {"type": "volume", "source": "teagram_pgdata", "target": "/var/lib/postgresql", "read_only": False},
            ],
            "persistent_service_mounts": {
                "postgres": [{"type": "volume", "source": "teagram_pgdata", "target": "/var/lib/postgresql", "read_only": False}],
                "teagramd": [
                    {"type": "volume", "source": "teagram_tgkey", "target": "/var/lib/teagramd", "read_only": False},
                    {"type": "volume", "source": "teagram_tgblobs", "target": "/var/lib/teagramd-blobs", "read_only": True},
                ],
            },
            "volumes": {"pgdata": "teagram_pgdata", "tgkey": "teagram_tgkey", "tgblobs": "teagram_tgblobs"},
        }
        media_mount = {
            "type": "bind",
            "source": "/etc/teagramd/media-keys",
            "target": "/run/teagramd/media-keys",
            "read_only": True,
            "bind": {"create_host_path": False},
        }
        target = copy.deepcopy(source)
        target["server_mounts"].append(media_mount)
        target["persistent_service_mounts"]["teagramd"].append(media_mount)
        compose_media_mount = copy.deepcopy(media_mount)
        compose_media_mount["bind"] = {}
        target_config = {
            "name": "teagram",
            "services": {
                "teagramd": {
                    "environment": {
                        "TG_MEDIA_KEY_DIR": "/run/teagramd/media-keys",
                        "TG_MEDIA_KEY_CURRENT": "1",
                    },
                    "volumes": [compose_media_mount],
                },
            },
        }

        self.assertFalse(runner.ordinary_rollout_layout_changed(source, target, target_config))
        print("PASS rollout layout accepts only the read-only media-key bind addition")

        writable_target = copy.deepcopy(target)
        writable_target["server_mounts"][-1]["read_only"] = False
        writable_target["persistent_service_mounts"]["teagramd"][-1]["read_only"] = False
        writable_config = copy.deepcopy(target_config)
        writable_config["services"]["teagramd"]["volumes"][0]["read_only"] = False
        self.assertTrue(runner.ordinary_rollout_layout_changed(source, writable_target, writable_config))

        changed_volume_target = copy.deepcopy(target)
        changed_volume_target["volumes"]["tgblobs"] = "teagram_other_tgblobs"
        self.assertTrue(runner.ordinary_rollout_layout_changed(source, changed_volume_target, target_config))
        wrong_version_config = copy.deepcopy(target_config)
        wrong_version_config["services"]["teagramd"]["environment"]["TG_MEDIA_KEY_CURRENT"] = "2"
        self.assertTrue(runner.ordinary_rollout_layout_changed(source, target, wrong_version_config))
        host_path_creating_target = copy.deepcopy(target)
        host_path_creating_target["server_mounts"][-1]["bind"]["create_host_path"] = True
        host_path_creating_target["persistent_service_mounts"]["teagramd"][-1]["bind"][
            "create_host_path"
        ] = True
        host_path_creating_config = copy.deepcopy(target_config)
        host_path_creating_config["services"]["teagramd"]["volumes"][0]["bind"]["create_host_path"] = True
        self.assertTrue(
            runner.ordinary_rollout_layout_changed(
                source, host_path_creating_target, host_path_creating_config,
            )
        )
        unexpected_bind_target = copy.deepcopy(target)
        unexpected_bind_target["server_mounts"][-1]["bind"]["propagation"] = "rprivate"
        unexpected_bind_target["persistent_service_mounts"]["teagramd"][-1]["bind"][
            "propagation"
        ] = "rprivate"
        unexpected_bind_option = copy.deepcopy(target_config)
        unexpected_bind_option["services"]["teagramd"]["volumes"][0]["bind"]["propagation"] = "rprivate"
        self.assertTrue(
            runner.ordinary_rollout_layout_changed(
                source, unexpected_bind_target, unexpected_bind_option,
            )
        )
        alternate_source_target = copy.deepcopy(target)
        alternate_source_config = copy.deepcopy(target_config)
        alternate_source_target["server_mounts"][-1]["source"] = "/var/lib/teagram/media-keys"
        alternate_source_target["persistent_service_mounts"]["teagramd"][-1]["source"] = (
            "/var/lib/teagram/media-keys"
        )
        alternate_source_config["services"]["teagramd"]["volumes"][0]["source"] = (
            "/var/lib/teagram/media-keys"
        )
        self.assertTrue(
            runner.ordinary_rollout_layout_changed(
                source, alternate_source_target, alternate_source_config,
            )
        )
        print("PASS rollout layout still rejects writable media mounts and persistent-volume changes")

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

    def test_existing_teagram_project_is_compared_with_teagramd(self) -> None:
        runner = load_runner()
        with tempfile.TemporaryDirectory(prefix="stack-move-inspect-teagram-") as temporary:
            checkout = pathlib.Path(temporary)
            default_compose = checkout / "docker-compose.yml"
            default_compose.write_text("name: teagram\n", encoding="utf-8")
            override = checkout / "docker-compose.override.yml"
            override.write_text("services:\n  teagramd:\n    ports: []\n", encoding="utf-8")
            env_file = checkout / ".env"
            env_file.write_text("POSTGRES_PASSWORD=fixture-only\n", encoding="utf-8")
            current_config = {
                "name": "teagram",
                "services": {
                    "postgres": {
                        "environment": {"POSTGRES_DB": "telegram"},
                        "volumes": [{
                            "type": "volume",
                            "source": "pgdata",
                            "target": "/var/lib/postgresql/18/docker",
                        }],
                    },
                    "teagramd": {
                        "volumes": [
                            {"type": "volume", "source": "tgkey", "target": "/var/lib/teagramd"},
                            {"type": "volume", "source": "tgblobs", "target": "/var/lib/teagramd-blobs"},
                        ],
                    },
                },
                "volumes": {
                    "pgdata": {"name": "teagram_pgdata"},
                    "tgkey": {"name": "teagram_tgkey"},
                    "tgblobs": {"name": "teagram_tgblobs"},
                    "rustfsdata": {"name": "teagram_rustfsdata"},
                },
            }
            target_sha = "1234567890abcdef1234567890abcdef12345678"

            def target_compose(
                target_checkout: pathlib.Path,
                sha: str,
                destination: pathlib.Path,
            ) -> pathlib.Path:
                self.assertEqual(target_checkout, checkout)
                self.assertEqual(sha, target_sha)
                target_path = destination / "target-docker-compose.yml"
                target_path.write_text("name: teagram\n", encoding="utf-8")
                return target_path

            def render_compose(
                _project: str | None,
                _project_dir: pathlib.Path,
                _env_file: pathlib.Path,
                files: list[pathlib.Path],
                _profiles: object = (),
            ) -> dict[str, object]:
                self.assertTrue(files)
                return current_config

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
                    "COMPOSE_FILE": os.pathsep.join((str(default_compose), str(override))),
                    "PATH": os.environ.get("PATH", os.defpath),
                },
                clear=True,
            ), mock.patch.object(runner, "render_compose", side_effect=render_compose), \
                    mock.patch.object(runner, "inspect_git_target", side_effect=target_compose), \
                    contextlib.redirect_stdout(output):
                runner.inspect(args)

            result = json.loads(output.getvalue())
            self.assertFalse(result["changed"])
            self.assertEqual(result["source"], result["target"])
            self.assertEqual(result["source"]["project"], "teagram")
            self.assertEqual(result["source"]["server"], "teagramd")
            print("PASS apply layout preflight accepts the current teagram project")

    def test_inspect_accepts_only_the_media_key_mount_transition(self) -> None:
        runner = load_runner()
        with tempfile.TemporaryDirectory(prefix="stack-move-inspect-media-key-") as temporary:
            checkout = pathlib.Path(temporary)
            default_compose = checkout / "docker-compose.yml"
            default_compose.write_text("name: teagram\n", encoding="utf-8")
            override = checkout / "docker-compose.override.yml"
            override.write_text("services:\n  teagramd:\n    ports: []\n", encoding="utf-8")
            env_file = checkout / ".env"
            env_file.write_text("POSTGRES_PASSWORD=fixture-only\n", encoding="utf-8")
            base_config = {
                "name": "teagram",
                "services": {
                    "postgres": {
                        "environment": {"POSTGRES_DB": "telegram"},
                        "volumes": [{
                            "type": "volume",
                            "source": "pgdata",
                            "target": "/var/lib/postgresql/18/docker",
                        }],
                    },
                    "teagramd": {
                        "volumes": [
                            {"type": "volume", "source": "tgkey", "target": "/var/lib/teagramd"},
                            {"type": "volume", "source": "tgblobs", "target": "/var/lib/teagramd-blobs", "read_only": True},
                        ],
                    },
                },
                "volumes": {
                    "pgdata": {"name": "teagram_pgdata"},
                    "tgkey": {"name": "teagram_tgkey"},
                    "tgblobs": {"name": "teagram_tgblobs"},
                    "rustfsdata": {"name": "teagram_rustfsdata"},
                },
            }
            media_mount = {
                "type": "bind",
                "source": "/etc/teagramd/media-keys",
                "target": "/run/teagramd/media-keys",
                "read_only": True,
                "bind": {},
            }
            target_config = copy.deepcopy(base_config)
            target_config["services"]["teagramd"]["environment"] = {
                "TG_MEDIA_KEY_DIR": "/run/teagramd/media-keys",
                "TG_MEDIA_KEY_CURRENT": "1",
            }
            target_config["services"]["teagramd"]["volumes"].append(media_mount)
            render_configs = iter((base_config, target_config, target_config))
            target_sha = "1234567890abcdef1234567890abcdef12345678"

            def target_compose(
                target_checkout: pathlib.Path,
                sha: str,
                destination: pathlib.Path,
            ) -> pathlib.Path:
                self.assertEqual(target_checkout, checkout)
                self.assertEqual(sha, target_sha)
                target_path = destination / "target-docker-compose.yml"
                target_path.write_text("name: teagram\n", encoding="utf-8")
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
                    "COMPOSE_FILE": os.pathsep.join((str(default_compose), str(override))),
                    "PATH": os.environ.get("PATH", os.defpath),
                },
                clear=True,
            ), mock.patch.object(runner, "render_compose", side_effect=lambda *_args, **_kwargs: next(render_configs)), \
                    mock.patch.object(runner, "inspect_git_target", side_effect=target_compose), \
                    contextlib.redirect_stdout(output):
                runner.inspect(args)

            result = json.loads(output.getvalue())
            self.assertFalse(result["changed"])
            self.assertEqual(result["source"]["volumes"], result["target"]["volumes"])
            self.assertNotEqual(result["source"]["server_mounts"], result["target"]["server_mounts"])
            print("PASS Compose inspect permits the reviewed read-only media-key mount addition")


class MoveSecretMigrationTest(unittest.TestCase):
    def test_move_uses_live_compose_and_stages_legacy_secret(self) -> None:
        runner = load_runner()
        repository = HELPER.parents[3]
        with tempfile.TemporaryDirectory(prefix="stack-move-secret-") as temporary:
            root = pathlib.Path(temporary)
            source = root / "source"
            target = root / "target"
            evidence = root / "evidence"
            source.mkdir(mode=0o700)
            target.mkdir(mode=0o700)
            evidence.mkdir(mode=0o700)
            source_env = source / ".env"
            source_env.write_text(
                "POSTGRES_PASSWORD=fixture-only\n"
                "TG_PUBLIC_LINK_PREFIX=https://example.invalid/\n",
                encoding="utf-8",
            )
            target_env = target / ".env"
            target_env.write_text(
                "POSTGRES_PASSWORD=fixture-only\n"
                "TG_PUBLIC_LINK_PREFIX=https://example.invalid/\n",
                encoding="utf-8",
            )
            target_env.chmod(0o600)
            source_compose = source / ".rollout-compose.local-cce00d1.yml"
            shutil.copyfile(LOCAL_COMPOSE, source_compose)
            (source / "docker-compose.yml").write_text(
                # Default S3 Compose must not be used for the live local stack.
                "services:\n"
                "  rustfs-init:\n"
                "    environment:\n"
                "      TG_BLOB_S3_ACCESS_KEY_ID: ${TG_BLOB_S3_ACCESS_KEY_ID:?required}\n",
                encoding="utf-8",
            )
            (source / "docker-compose.override.yml").write_text(
                "name: telegram-server\nservices:\n  telegramd:\n    ports: []\n",
                encoding="utf-8",
            )
            legacy_secret_dir = source / ".secrets"
            legacy_secret_dir.mkdir(mode=0o700)
            legacy_secret = legacy_secret_dir / "telegramd-blob-secret-key"
            legacy_secret.write_bytes(b"fixture legacy blob key\n")
            legacy_secret.chmod(0o400)
            shutil.copytree(legacy_secret_dir, target / ".secrets", copy_function=shutil.copy2)
            target_compose = target / "docker-compose.yml"
            target_local_overlay = target / "docker-compose.local-blobs.yml"
            shutil.copyfile(repository / "docker-compose.yml", target_compose)
            shutil.copyfile(repository / "docker-compose.local-blobs.yml", target_local_overlay)
            target_postgres_config = target / "deploy" / "postgres" / "postgresql.conf"
            target_postgres_config.parent.mkdir(parents=True)
            shutil.copyfile(
                repository / "deploy" / "postgres" / "postgresql.conf",
                target_postgres_config,
            )
            target_compose_selection = os.pathsep.join(
                (str(target_compose), str(target_local_overlay)),
            )

            args = argparse.Namespace(
                source_dir=source,
                target_dir=target,
                evidence_dir=evidence,
                env_file=source_env,
                target_env_file=target_env,
                override=source / "docker-compose.override.yml",
                target_sha="1234567890abcdef1234567890abcdef12345678",
                source_compose=str(source_compose),
                target_compose=target_compose_selection,
            )

            class ReachedSourceRunningCheck(Exception):
                pass

            with mock.patch.dict(os.environ, {
                "COMPOSE_FILE": "",
                "TEAGRAMD_STACK_MOVE_BLOB_DIR": "/host-selected-blob-path",
            }, clear=False), \
                    mock.patch.object(runner, "project_is_running", return_value=False), \
                    mock.patch.object(
                        runner,
                        "assert_source_running",
                        side_effect=ReachedSourceRunningCheck,
                    ):
                with self.assertRaises(ReachedSourceRunningCheck):
                    runner.move(args)
                self.assertEqual(
                    os.environ["TEAGRAMD_STACK_MOVE_BLOB_DIR"],
                    "/host-selected-blob-path",
                )

            staged_secret = target / ".secrets/teagramd-blob-secret-key"
            self.assertEqual(staged_secret.read_bytes(), b"fixture legacy blob key\n")
            self.assertEqual(staged_secret.stat().st_mode & 0o777, 0o400)
            self.assertIn(
                "TEAGRAMD_STACK_MOVE_BLOB_DIR=/var/lib/telegramd-blobs",
                target_env.read_text(encoding="utf-8").splitlines(),
            )
            self.assertEqual(target_env.stat().st_mode & 0o777, 0o600)
            with mock.patch.dict(os.environ, {
                "TEAGRAM_TURNSTILE_SITE_KEY": "",
                "TEAGRAM_TURNSTILE_SECRET_KEY_FILE": "",
                "TG_TURNSTILE_SIGNIN_ENABLED": "",
            }, clear=False):
                source_config = runner.render_compose(
                    "telegram-server", source, source_env, [source_compose],
                )
                # A new deploy process has no temporary mover environment. The
                # target .env must retain the selected mount layout on its own.
                with mock.patch.dict(os.environ):
                    os.environ.pop("TEAGRAMD_STACK_MOVE_BLOB_DIR", None)
                    target_config = runner.render_compose(
                        "teagram", target, target_env,
                        [target_compose, target_local_overlay, target / "docker-compose.override.yml"],
                        profiles=("migration",),
                    )
            self.assertEqual(
                runner.validate_source_compose_volumes(source_config),
                {
                    "pgdata": "telegram-server_pgdata",
                    "tgkey": "telegram-server_tgkey",
                    "tgblobs": "telegram-server_tgblobs",
                },
            )
            self.assertNotIn("rustfsdata", runner.named_volumes(source_config))
            source_service = source_config["services"]["telegramd"]
            target_service = target_config["services"]["teagramd"]
            self.assertEqual(
                target_service["environment"]["TG_BLOB_DIR"],
                source_service["environment"]["TG_BLOB_DIR"],
            )
            self.assertTrue(all(
                name not in target_service["environment"]
                for name in runner.S3_BLOB_ENVIRONMENT
            ))
            self.assertEqual(
                target_service.get("secrets", []),
                [{"source": "teagramd_turnstile", "target": "teagramd-turnstile"}],
            )
            self.assertEqual(
                target_service["environment"]["TG_TURNSTILE_SITE_KEY"],
                "",
            )
            self.assertEqual(
                target_service["environment"]["TG_TURNSTILE_SECRET_KEY_FILE"],
                "",
            )
            self.assertEqual(
                target_service["environment"]["TG_TURNSTILE_SIGNIN_ENABLED"],
                "false",
            )
            self.assertEqual(
                target_config["secrets"]["teagramd_turnstile"]["file"],
                "/dev/null",
            )
            self.assertTrue(any(
                mount.get("source") == "tgblobs"
                and mount.get("target") == source_service["environment"]["TG_BLOB_DIR"]
                and mount.get("read_only", False) is False
                for mount in target_service["volumes"]
            ))
            self.assertTrue(any(
                mount.get("source") == "tgblobs"
                and mount.get("target") == "/var/lib/teagramd-blobs"
                and mount.get("read_only", False) is True
                for mount in target_service["volumes"]
            ))
            runner.inspect_target_contract(
                target_config, source_service["environment"]["TG_BLOB_DIR"],
            )
            unsafe_report_volume = copy.deepcopy(target_config)
            unsafe_report_volume["volumes"][runner.TARGET_REPORT_VOLUME]["driver_opts"] = {
                "type": "none", "o": "bind", "device": "/root",
            }
            with self.assertRaisesRegex(runner.MoveError, "Docker-managed local storage"):
                runner.inspect_target_contract(
                    unsafe_report_volume, source_service["environment"]["TG_BLOB_DIR"],
                )
            unreviewed_volume = copy.deepcopy(target_config)
            unreviewed_volume["volumes"]["unreviewed"] = {"name": "teagram_unreviewed"}
            with self.assertRaisesRegex(runner.MoveError, "named volumes do not match"):
                runner.inspect_target_contract(
                    unreviewed_volume, source_service["environment"]["TG_BLOB_DIR"],
                )
            self.assertEqual(
                pathlib.Path(target_config["secrets"]["teagramd_blob_secret_key"]["file"]),
                staged_secret.resolve(),
            )
            self.assertTrue(
                any(
                    secret.get("source") == "teagramd_blob_secret_key"
                    and secret.get("target") == "teagramd-blob-secret-key"
                    for secret in target_config["services"]["rustfs-init"]["secrets"]
                ),
                "rustfs-init does not mount the staged Teagram blob key",
            )
            print("PASS stack move uses live local Compose and stages the legacy target secret")

    def test_s3_authority_is_rejected_by_the_mover_contract(self) -> None:
        runner = load_runner()
        source_config = {
            "services": {
                "telegramd": {
                    "environment": {
                        "TG_BLOB_DIR": "/var/lib/telegramd-blobs",
                        "TG_BLOB_S3_ENDPOINT": "http://rustfs:9000",
                    },
                    "volumes": [{
                        "type": "volume",
                        "source": "tgblobs",
                        "target": "/var/lib/telegramd-blobs",
                        "read_only": True,
                    }],
                },
            },
        }
        with self.assertRaisesRegex(runner.MoveError, "source blob mode must not select S3 authority"):
            runner.require_local_source_blob_lane(source_config)
        print("PASS stack mover rejects S3 authority before touching the source stack")


class MoveRuntimeProfileTest(unittest.TestCase):
    def test_migration_profile_keeps_rustfs_data_without_runtime_service(self) -> None:
        runner = load_runner()
        config = {"services": {"rustfs": {"profiles": ["migration"]}}}

        services, stable_volumes = runner.target_runtime_expectations(config, ())

        self.assertEqual(services, (runner.TARGET_SERVER, "postgres"))
        self.assertEqual(stable_volumes, ("tgkey", "tgblobs", "rustfsdata"))
        print("PASS local move preserves the RustFS data volume without requiring migration services")

    def test_enabled_rustfs_profile_keeps_existing_runtime_checks(self) -> None:
        runner = load_runner()
        config = {"services": {"rustfs": {"profiles": ["migration"]}}}

        services, stable_volumes = runner.target_runtime_expectations(config, ("migration",))

        self.assertEqual(services, (runner.TARGET_SERVER, "postgres", "rustfs"))
        self.assertEqual(stable_volumes, ("tgkey", "tgblobs"))
        print("PASS explicitly enabled migration profile retains RustFS runtime checks")

    def test_inactive_rustfs_volume_must_match_its_copied_inventory(self) -> None:
        runner = load_runner()
        inventories = {
            "rustfsdata": {
                "target_sha256": runner.hashlib.sha256(b"copied inventory").hexdigest(),
            },
        }

        with mock.patch.object(runner, "volume_files", return_value="changed inventory"):
            with self.assertRaisesRegex(runner.MoveError, "copied rustfsdata inventory"):
                runner.verify_runtime_volume_inventories(
                    "postgres:fixture",
                    runner.SOURCE_VOLUMES,
                    runner.TARGET_VOLUMES,
                    inventories,
                    ("rustfsdata",),
                )
        print("PASS inactive RustFS data volume is checked against its copied inventory")


class OptionalSourceRustFSVolumeTest(unittest.TestCase):
    def test_compose_may_omit_or_pin_the_historical_rustfs_volume(self) -> None:
        runner = load_runner()
        volumes = {
            logical: {"name": name}
            for logical, name in runner.SOURCE_VOLUMES.items()
        }
        config = {"volumes": volumes}

        self.assertEqual(runner.validate_source_compose_volumes(config), runner.SOURCE_VOLUMES)
        volumes["rustfsdata"] = {"name": "unexpected_rustfs_volume"}
        with self.assertRaisesRegex(runner.MoveError, "source Compose volume names"):
            runner.validate_source_compose_volumes(config)
        print("PASS source Compose validates declared volumes and permits only the pinned optional RustFS volume")

    def test_absent_rustfs_volume_is_staged_only_without_authoritative_data(self) -> None:
        runner = load_runner()
        name = runner.SOURCE_VOLUMES["rustfsdata"]
        with mock.patch.object(runner, "volume_exists", return_value=False), \
                mock.patch.object(runner, "output", return_value=name) as docker_output:
            runner.ensure_source_rustfsdata_volume(None)
            docker_output.assert_called_once_with(["docker", "volume", "create", name], timeout=30)
            with self.assertRaisesRegex(runner.MoveError, "authoritative source RustFS volume is missing"):
                runner.ensure_source_rustfsdata_volume(name)
        print("PASS absent local RustFS volume is staged empty only when authority permits it")

    def test_source_authority_can_record_no_rustfs_volume(self) -> None:
        runner = load_runner()
        authority = mock.Mock()
        authority.Reject = RuntimeError
        authority.read_authority.return_value = (
            [{"volumes": {"rustfsdata": None}}], b"head", b"head",
        )
        with mock.patch.object(runner, "load_blob_mode_helper", return_value=authority):
            result = runner.source_rustfsdata_authority(
                pathlib.Path("/state"), pathlib.Path("/reports"), pathlib.Path("/helper"),
            )
        self.assertIsNone(result)
        print("PASS move reads the durable RustFS volume authority before creating an empty source volume")


class PinnedMoveAuthorityPublicationTest(unittest.TestCase):
    def test_publishes_move_authority_from_pinned_runtime_layout(self) -> None:
        with tempfile.TemporaryDirectory(prefix="stack-move-pinned-authority-") as temporary:
            root = pathlib.Path(temporary)
            runtime = root / "runtime"
            runtime.mkdir()
            pinned_runner = runtime / "stack-move-runner.pinned"
            pinned_helper = runtime / "blob-mode-state.pinned"
            shutil.copyfile(HELPER, pinned_runner)
            shutil.copyfile(HELPER.with_name("blob-mode-state.py"), pinned_helper)
            qualifier_helper = runtime / "qualify-rustfs-transition.py"
            shutil.copyfile(HELPER.with_name(qualifier_helper.name), qualifier_helper)
            runner = load_runner(pinned_runner)

            fixtures_path = HELPER.with_name("test-blob-mode-state.py")
            fixtures_spec = importlib.util.spec_from_file_location(
                "stack_move_blob_mode_fixtures", fixtures_path,
            )
            if fixtures_spec is None or fixtures_spec.loader is None:
                self.fail("could not load blob-mode-state test fixtures")
            fixtures = importlib.util.module_from_spec(fixtures_spec)
            fixtures_spec.loader.exec_module(fixtures)
            fixture = fixtures.BlobModeStateTests(
                "test_local_inventory_preserves_exact_backend_and_volume_names",
            )
            fixture.setUp()
            fixture.volume = "telegram-server_tgblobs"
            make_transition_proof = fixture.transition_proof

            def legacy_volume_transition_proof(outcome: str) -> dict[str, object]:
                proof = make_transition_proof(outcome)
                volumes = proof["volumes"]
                assert isinstance(volumes, dict)
                volumes["rustfsdata"] = "telegram-server_rustfsdata"
                return proof

            fixture.transition_proof = legacy_volume_transition_proof
            blob_mode = fixtures.blob_mode

            legacy_blob_dir = "/var/lib/telegramd-blobs"
            layouts = dict(blob_mode.RUNTIME_LAYOUTS)
            layouts["teagramd"] = {**layouts["teagramd"], "blobs": legacy_blob_dir}
            with (
                mock.patch.object(blob_mode, "BLOB_TARGET", legacy_blob_dir),
                mock.patch.object(blob_mode, "BLOB_TARGETS", {legacy_blob_dir}),
                mock.patch.object(blob_mode, "RUNTIME_LAYOUTS", layouts),
            ):
                state_dir, report_root, lock_path = fixture.create_initial_authority(root)
                with (
                    mock.patch.object(blob_mode, "require_runner_lock"),
                    mock.patch.object(blob_mode, "file_stat", side_effect=fixture.allow_unowned_private_files),
                    mock.patch.object(blob_mode.os, "fchown"),
                    mock.patch.object(blob_mode.os, "chown"),
                ):
                    fixture.publish(state_dir, report_root, lock_path, root, "s3-accepted")
                    fixture.publish(state_dir, report_root, lock_path, root, "recovered-local")

            evidence_dir = root / "evidence"
            evidence_dir.mkdir(mode=0o700)
            for logical in ("tgblobs", "rustfsdata"):
                inventory_line = blob_mode.hashlib.sha256(f"moved {logical}".encode()).hexdigest()
                inventory = f"{inventory_line}  ./fixture.bin\n".encode()
                for side in ("source", "target"):
                    path = evidence_dir / f"{logical}.{side}.sha256"
                    path.write_bytes(inventory)
                    path.chmod(0o600)

            original_load = runner.load_blob_mode_helper
            with contextlib.ExitStack() as patches:
                def load_test_helper(path: pathlib.Path):
                    helper = original_load(path)
                    patches.enter_context(mock.patch.object(helper, "require_runner_lock"))
                    patches.enter_context(mock.patch.object(
                        helper, "file_stat", side_effect=fixture.allow_unowned_private_files,
                    ))
                    patches.enter_context(mock.patch.object(helper.os, "fchown"))
                    patches.enter_context(mock.patch.object(helper.os, "chown"))
                    return helper

                with mock.patch.object(runner, "load_blob_mode_helper", side_effect=load_test_helper):
                    record = runner.publish_moved_blob_authority(
                        state_dir, report_root, lock_path, evidence_dir, pinned_helper,
                    )

            self.assertEqual(record["outcome"], "stack-moved-local")
            self.assertEqual(record["volumes"]["tgblobs"], "teagram_tgblobs")
            self.assertTrue((state_dir / "journal" / "0000000002.json").is_file())


class PostgresImageReferenceTest(unittest.TestCase):
    def test_exact_alpine_tags_are_required_for_source_and_target(self) -> None:
        runner = load_runner()
        self.assertTrue(
            runner.postgres_image_matches_tag(
                "ghcr.io/teagramhq/teagram-server-postgres:16-alpine",
                "postgres:16-alpine",
            )
        )
        self.assertTrue(
            runner.postgres_image_matches_tag(
                "ghcr.io/teagramhq/teagram-server-postgres:18-alpine",
                "postgres:18-alpine",
            )
        )
        self.assertFalse(
            runner.postgres_image_matches_tag(
                "ghcr.io/immich-app/postgres:16-vectorchord1.1.1",
                "postgres:16-alpine",
            )
        )
        self.assertFalse(
            runner.postgres_image_matches_tag(
                "ghcr.io/teagramhq/teagram-server-postgres:18-alpine",
                "postgres:16-alpine",
            )
        )
        self.assertFalse(runner.postgres_image_matches_tag(None, "postgres:16-alpine"))
        print("PASS PostgreSQL image preflight accepts only the required Alpine tags")


class RuntimeTableCountsTest(unittest.TestCase):
    def test_table_counts_query_prints_only_count_rows(self) -> None:
        runner = load_runner()
        with mock.patch.object(runner, "output", return_value="") as output:
            runner.table_counts("postgres-id", "teagram")

        query = output.call_args.kwargs["input_bytes"].decode()
        generator, gexec = query.rstrip("\n").rsplit("\n", 1)
        self.assertEqual(gexec, "\\gexec")
        # A terminated generator runs once on its own and prints the generated
        # SELECT statements ahead of the count rows that \gexec produces.
        self.assertFalse(generator.rstrip().endswith(";"))
        print("PASS table counts buffer the generator query for \\gexec only")

    def test_allows_unchanged_counts_when_atlas_ledger_is_absent(self) -> None:
        runner = load_runner()
        counts = "public.stack_move_probe|2\n"

        self.assertTrue(runner.runtime_table_counts_compatible(counts, counts))
        self.assertFalse(
            runner.runtime_table_counts_compatible(
                counts,
                "public.stack_move_probe|3\n",
            )
        )

    def test_allows_a_database_without_tables(self) -> None:
        runner = load_runner()

        self.assertTrue(runner.runtime_table_counts_compatible("", ""))
        self.assertTrue(
            runner.runtime_table_counts_compatible(
                "", "atlas_schema_revisions.atlas_schema_revisions|72\n"
            )
        )
        self.assertFalse(
            runner.runtime_table_counts_compatible("", "public.stack_move_probe|0\n")
        )
        self.assertFalse(
            runner.runtime_table_counts_compatible("public.stack_move_probe|2\n", "")
        )

    def test_allows_target_migrations_to_create_the_atlas_ledger(self) -> None:
        runner = load_runner()
        source = "public.stack_move_probe|2\n"
        target = source + "atlas_schema_revisions.atlas_schema_revisions|72\n"

        self.assertTrue(runner.runtime_table_counts_compatible(source, target))

    def test_rejects_target_application_table_addition(self) -> None:
        runner = load_runner()
        source = "public.stack_move_probe|2\n"
        target = (
            source
            + "atlas_schema_revisions.atlas_schema_revisions|72\n"
            + "public.unexpected|0\n"
        )

        self.assertFalse(runner.runtime_table_counts_compatible(source, target))

    def test_rejects_atlas_ledger_removal(self) -> None:
        runner = load_runner()
        source = (
            "public.stack_move_probe|2\n"
            "atlas_schema_revisions.atlas_schema_revisions|72\n"
        )

        self.assertFalse(
            runner.runtime_table_counts_compatible(source, "public.stack_move_probe|2\n")
        )

    def test_allows_only_atlas_revision_growth_after_runtime_migrations(self) -> None:
        runner = load_runner()
        source = (
            "auth_keys|629\n"
            "atlas_schema_revisions.atlas_schema_revisions|98\n"
            "users|26\n"
        )
        target = (
            "auth_keys|629\n"
            "atlas_schema_revisions.atlas_schema_revisions|100\n"
            "users|26\n"
        )

        self.assertTrue(runner.runtime_table_counts_compatible(source, source))
        self.assertTrue(runner.runtime_table_counts_compatible(source, target))

    def test_rejects_application_changes_and_atlas_revision_decreases(self) -> None:
        runner = load_runner()
        source = (
            "auth_keys|629\n"
            "atlas_schema_revisions.atlas_schema_revisions|98\n"
            "users|26\n"
        )
        application_change = source.replace("auth_keys|629", "auth_keys|630").replace(
            "atlas_schema_revisions.atlas_schema_revisions|98",
            "atlas_schema_revisions.atlas_schema_revisions|100",
        )
        revision_decrease = source.replace(
            "atlas_schema_revisions.atlas_schema_revisions|98",
            "atlas_schema_revisions.atlas_schema_revisions|97",
        )

        self.assertFalse(runner.runtime_table_counts_compatible(source, application_change))
        self.assertFalse(runner.runtime_table_counts_compatible(source, revision_decrease))

    def test_rejects_missing_or_malformed_table_counts(self) -> None:
        runner = load_runner()
        source = "auth_keys|629\natlas_schema_revisions.atlas_schema_revisions|98\n"

        self.assertFalse(runner.runtime_table_counts_compatible(source, "auth_keys|629\n"))
        self.assertFalse(
            runner.runtime_table_counts_compatible(source, "auth_keys|many\natlas_schema_revisions.atlas_schema_revisions|98\n")
        )


if __name__ == "__main__":
    unittest.main()
