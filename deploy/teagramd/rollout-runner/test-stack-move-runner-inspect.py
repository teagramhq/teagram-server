#!/usr/bin/env python3
"""Regression coverage for inspecting the live Compose selection."""

from __future__ import annotations

import argparse
import contextlib
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


class MoveSecretMigrationTest(unittest.TestCase):
    def test_move_preserves_source_blob_path_and_stages_legacy_secret(self) -> None:
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
            source_env.write_text("POSTGRES_PASSWORD=fixture-only\n", encoding="utf-8")
            target_env = target / ".env"
            target_env.write_text(
                "POSTGRES_PASSWORD=fixture-only\n"
                "TG_PUBLIC_LINK_PREFIX=https://example.invalid/\n",
                encoding="utf-8",
            )
            target_env.chmod(0o600)
            source_compose = source / "docker-compose.yml"
            source_compose.write_text(
                "name: telegram-server\n"
                "services:\n"
                "  postgres:\n"
                "    image: postgres:16-alpine\n"
                "    environment:\n"
                "      POSTGRES_DB: telegram\n"
                "      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?required}\n"
                "    volumes: [pgdata:/var/lib/postgresql/data]\n"
                "  telegramd:\n"
                "    image: telegramd:local\n"
                "    environment:\n"
                "      TG_BLOB_DIR: /var/lib/telegramd-blobs\n"
                "    volumes:\n"
                "      - tgkey:/var/lib/telegramd\n"
                "      - tgblobs:/var/lib/telegramd-blobs\n"
                "  rustfs:\n"
                "    image: rustfs:fixture\n"
                "    volumes: [rustfsdata:/data]\n"
                "volumes:\n"
                "  pgdata: {name: telegram-server_pgdata}\n"
                "  tgkey: {name: telegram-server_tgkey}\n"
                "  tgblobs: {name: telegram-server_tgblobs}\n"
                "  rustfsdata: {name: telegram-server_rustfsdata}\n",
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
            source_config = runner.render_compose(
                "telegram-server", source, source_env, [source_compose],
            )
            source_service = source_config["services"]["telegramd"]
            # A new deploy process has no temporary mover environment. The
            # target .env must retain the selected mount layout on its own.
            with mock.patch.dict(os.environ):
                os.environ.pop("TEAGRAMD_STACK_MOVE_BLOB_DIR", None)
                target_config = runner.render_compose(
                    "teagram", target, target_env,
                    [target_compose, target_local_overlay, target / "docker-compose.override.yml"],
                    profiles=("migration",),
                )
            target_service = target_config["services"]["teagramd"]
            self.assertEqual(
                target_service["environment"]["TG_BLOB_DIR"],
                source_service["environment"]["TG_BLOB_DIR"],
            )
            self.assertTrue(all(
                name not in target_service["environment"]
                for name in runner.S3_BLOB_ENVIRONMENT
            ))
            self.assertEqual(target_service.get("secrets", []), [])
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
            print("PASS stack move preserves the source blob path and stages the legacy target secret")

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


if __name__ == "__main__":
    unittest.main()
