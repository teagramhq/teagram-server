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
            'package config\nimport "os"\n'
            'func load() { _ = os.Getenv("TG_NEW_SETTING") }\n'
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("TG_NEW_SETTING", result.stdout + result.stderr)

    def test_package_level_config_variable_is_named(self):
        (self.root / "internal/config/config.go").write_text(
            'package config\nimport "os"\n'
            'var debugLevel = os.Getenv("TG_PACKAGE_ONLY")\n'
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("TG_PACKAGE_ONLY", result.stdout + result.stderr)

    def test_aliased_os_import_config_variable_is_named(self):
        (self.root / "internal/config/config.go").write_text(
            'package config\nimport envos "os"\n'
            'func load() { _ = envos.Getenv("TG_ALIASED_ONLY") }\n'
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("TG_ALIASED_ONLY", result.stdout + result.stderr)

    def test_unused_config_variable_is_named(self):
        (self.root / "internal/config/config.go").write_text(
            'package config\nimport ("errors"; "os")\n'
            'func load() { _ = os.Getenv("TG_CURRENT_SETTING"); '
            '_ = errors.New("TG_REMOVED_SETTING") }\n'
        )
        (self.root / "docs/configuration.md").write_text(
            "| Variable | Default | Notes |\n|---|---|---|\n"
            "| `TG_REMOVED_SETTING` | `false` | old option |\n"
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("TG_REMOVED_SETTING", result.stdout + result.stderr)

    def test_helper_parameter_config_variable_is_named(self):
        (self.root / "internal/config/config.go").write_text(
            'package config\nimport "os"\n'
            'func read(key string) string { return os.Getenv(key) }\n'
            'func load() { _ = read("TG_HELPER_ONLY") }\n'
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("TG_HELPER_ONLY", result.stdout + result.stderr)

    def test_method_call_config_variable_is_named(self):
        (self.root / "internal/config/config.go").write_text(
            'package config\nimport "os"\n'
            'func read(key string) string { return os.Getenv(key) }\n'
            'type loader struct{}\n'
            'func (loader) load() string { return read("TG_HELPER_ONLY") }\n'
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("TG_HELPER_ONLY", result.stdout + result.stderr)

    def test_closure_parameter_config_variable_is_named(self):
        (self.root / "internal/config/config.go").write_text(
            'package config\nimport "os"\n'
            'func load() { read := func(key string) string { return os.Getenv(key) }; '
            '_ = read("TG_CLOSURE_ONLY") }\n'
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("TG_CLOSURE_ONLY", result.stdout + result.stderr)

    def test_range_table_config_variable_is_named(self):
        (self.root / "internal/config/config.go").write_text(
            'package config\nimport "os"\n'
            'func load() { for _, setting := range []struct { name string }{'
            '{name: "TG_TABLE_ONLY"}, {"TG_TABLE_POSITIONAL"}} {'
            '_ = os.Getenv(setting.name) } }\n'
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("TG_TABLE_ONLY", result.stdout + result.stderr)
        self.assertIn("TG_TABLE_POSITIONAL", result.stdout + result.stderr)

    def test_unresolved_environment_key_reports_source_location(self):
        (self.root / "internal/config/config.go").write_text(
            'package config\nimport "os"\n'
            'func load(key string) { _ = os.Getenv(key) }\n'
            'func other() {}\n'
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("config.go:3", result.stdout + result.stderr)
        self.assertIn("cannot resolve environment key", result.stdout + result.stderr)

    def test_environment_key_depth_limit_reports_source_location(self):
        declarations = ['package config', 'import "os"', 'const key0 = "TG_DEEP_SETTING"']
        declarations.extend(f"const key{index} = key{index - 1}" for index in range(1, 20))
        declarations.append("func load() { _ = os.Getenv(key19) }")
        declarations.append("func other() {}")
        (self.root / "internal/config/config.go").write_text("\n".join(declarations) + "\n")

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("config.go:", result.stdout + result.stderr)
        self.assertIn("resolution exceeded depth", result.stdout + result.stderr)

    def test_missing_relative_markdown_target_names_file_and_target(self):
        (self.root / "README.md").write_text("[missing](docs/nope.md)\n")

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md", result.stdout + result.stderr)
        self.assertIn("docs/nope.md", result.stdout + result.stderr)

    def test_broken_markdown_link_after_stray_backtick_is_checked(self):
        (self.root / "README.md").write_text(
            "` stray\n[missing](docs/nope.md) and `valid code`\n"
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md", result.stdout + result.stderr)
        self.assertIn("docs/nope.md", result.stdout + result.stderr)

    def test_broken_markdown_link_wrapping_external_badge_is_checked(self):
        (self.root / "README.md").write_text(
            "[![guide](https://example.com/badge.svg)](docs/nope.md)\n"
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md", result.stdout + result.stderr)
        self.assertIn("docs/nope.md", result.stdout + result.stderr)

    def test_link_title_with_angle_bracket_does_not_hide_destination(self):
        (self.root / "README.md").write_text('[x](docs/nope.md "a<b")\n')

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md", result.stdout + result.stderr)
        self.assertIn("docs/nope.md", result.stdout + result.stderr)

    def test_link_title_with_angle_bracket_does_not_hide_following_links(self):
        (self.root / "docs/a.md").write_text("Guide.\n")
        (self.root / "README.md").write_text(
            '[x](docs/a.md "a<b")\n[y](docs/gone.md)\n<br> done)\n'
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md", result.stdout + result.stderr)
        self.assertIn("docs/gone.md", result.stdout + result.stderr)

    def test_unbalanced_parenthesis_in_title_does_not_hide_following_links(self):
        (self.root / "docs/a.md").write_text("Guide.\n")
        (self.root / "README.md").write_text(
            '[x](docs/a.md "Smile :(")\n[y](docs/gone.md)\nSee :)\n'
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md", result.stdout + result.stderr)
        self.assertIn("docs/gone.md", result.stdout + result.stderr)

    def test_wrapped_link_title_does_not_hide_missing_destination(self):
        (self.root / "README.md").write_text('[x](docs/gone.md "a\ntitle")\n')

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md", result.stdout + result.stderr)
        self.assertIn("docs/gone.md", result.stdout + result.stderr)

    def test_unclosed_inline_link_reports_source_line(self):
        (self.root / "README.md").write_text("Intro.\n[broken](docs/gone.md\n")

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md:2", result.stdout + result.stderr)
        self.assertIn("unclosed Markdown link", result.stdout + result.stderr)

    def test_link_title_stops_at_blank_line(self):
        (self.root / "README.md").write_text('[broken](docs/gone.md "a\n\ncontinued")\n')

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md:1", result.stdout + result.stderr)
        self.assertIn("unclosed Markdown link", result.stdout + result.stderr)

    def test_parenthesized_link_title_stops_at_blank_line(self):
        (self.root / "docs/a.md").write_text("Guide.\n")
        (self.root / "README.md").write_text(
            "[x](docs/a.md (Smile :(\n\n[y](docs/gone.md)\nSee :))\n"
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md:1", result.stdout + result.stderr)
        self.assertIn("docs/gone.md", result.stdout + result.stderr)

    def test_angle_bracket_markdown_destination_is_checked(self):
        (self.root / "README.md").write_text("[x](<docs/nope.md>)\n")

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("docs/nope.md", result.stdout + result.stderr)

    def test_escaped_parentheses_in_markdown_destination_resolve(self):
        (self.root / "docs/guide(quick).md").write_text("Guide.\n")
        (self.root / "README.md").write_text("[guide](docs/guide\\(quick\\).md)\n")

        result = self.run_checker()

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_missing_relative_html_asset_names_file_and_target(self):
        (self.root / "docs/guide.md").write_text('<img src="missing.svg">\n')

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("docs/guide.md", result.stdout + result.stderr)
        self.assertIn("missing.svg", result.stdout + result.stderr)

    def test_test_only_config_variable_and_external_links_are_ignored(self):
        (self.root / "internal/config/config.go").write_text(
            'package config\nimport "os"\n'
            'func load() { _ = os.Getenv("TG_CURRENT_SETTING"); '
            '_ = os.Getenv("TG_FIXTURE_ONLY"); _ = os.Getenv("TG_PERF_ONLY") }\n'
        )
        (self.root / "internal/config/config_test.go").write_text(
            'package config\nfunc test() { _ = os.Getenv("TG_TEST_ONLY") }\n'
        )
        (self.root / "cmd/teagramd/serve_fixture_auth_progress.go").write_text(
            'package main\nvar _ = "TG_FIXTURE_ONLY"\n'
        )
        (self.root / "internal/linklanding/landing.go").write_text(
            'package linklanding\nimport "os"\n'
            'const web = "TG_INVITE_WEB_SIGNUP_URL"\n'
            'const desktop = "TG_INVITE_DESKTOP_URL"\n'
            'func load() { _ = os.Getenv(web); _ = os.Getenv(desktop) }\n'
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
