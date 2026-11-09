import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


CHECKER = Path(__file__).with_name("check_documentation.py")


class DocumentationCheckTests(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.root = Path(self.tempdir.name)
        (self.root / "internal/config").mkdir(parents=True)
        (self.root / "internal/linklanding").mkdir(parents=True)
        (self.root / "cmd/teagramd").mkdir(parents=True)
        (self.root / "docs").mkdir()
        (self.root / "README.md").write_text("Documentation.\n")
        (self.root / "ROADMAP.md").write_text("Roadmap.\n")
        (self.root / "docs/configuration.md").write_text(
            "| Variable | Default | Notes |\n|---|---|---|\n"
        )

    def tearDown(self):
        self.tempdir.cleanup()

    def run_checker(self):
        return subprocess.run(
            [sys.executable, str(CHECKER), "--root", str(self.root)],
            capture_output=True,
            text=True,
            check=False,
        )

    def test_missing_config_variable_is_named(self):
        (self.root / "internal/config/config.go").write_text(
            'package config\nfunc load() { _ = os.Getenv("TG_NEW_SETTING") }\n'
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("TG_NEW_SETTING", result.stdout + result.stderr)

    def test_unused_config_variable_is_named(self):
        (self.root / "internal/config/config.go").write_text(
            'package config\nfunc load() { _ = os.Getenv("TG_CURRENT_SETTING") }\n'
        )
        (self.root / "docs/configuration.md").write_text(
            "| Variable | Default | Notes |\n|---|---|---|\n"
            "| `TG_REMOVED_SETTING` | `false` | old option |\n"
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("TG_REMOVED_SETTING", result.stdout + result.stderr)

    def test_missing_relative_markdown_target_names_file_and_target(self):
        (self.root / "README.md").write_text("[missing](docs/nope.md)\n")

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md", result.stdout + result.stderr)
        self.assertIn("docs/nope.md", result.stdout + result.stderr)

    def test_missing_relative_html_asset_names_file_and_target(self):
        (self.root / "docs/guide.md").write_text('<img src="missing.svg">\n')

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("docs/guide.md", result.stdout + result.stderr)
        self.assertIn("missing.svg", result.stdout + result.stderr)

    def test_test_only_config_variable_and_external_links_are_ignored(self):
        (self.root / "internal/config/config.go").write_text(
            'package config\nfunc load() { _ = os.Getenv("TG_CURRENT_SETTING"); '
            '_ = os.Getenv("TG_FIXTURE_ONLY"); _ = os.Getenv("TG_PERF_ONLY") }\n'
        )
        (self.root / "internal/config/config_test.go").write_text(
            'package config\nfunc test() { _ = os.Getenv("TG_TEST_ONLY") }\n'
        )
        (self.root / "cmd/teagramd/serve_fixture_auth_progress.go").write_text(
            'package main\nvar _ = "TG_FIXTURE_ONLY"\n'
        )
        (self.root / "internal/linklanding/landing.go").write_text(
            'package linklanding\nconst web = "TG_INVITE_WEB_SIGNUP_URL"\n'
            'const desktop = "TG_INVITE_DESKTOP_URL"\n'
        )
        (self.root / "docs/configuration.md").write_text(
            "| Variable | Default | Notes |\n|---|---|---|\n"
            "| `TG_CURRENT_SETTING` | `false` | current option |\n"
            "| `TG_INVITE_WEB_SIGNUP_URL` | *(unset)* | web invite link |\n"
            "| `TG_INVITE_DESKTOP_URL` | *(unset)* | desktop invite link |\n"
        )
        (self.root / "README.md").write_text(
            "[valid](docs/configuration.md) and [external](https://example.com/x).\n"
        )

        result = self.run_checker()

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
