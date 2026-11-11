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

    def test_production_variable_in_perf_named_source_file_is_scanned(self):
        (self.root / "internal/config/config_perf.go").write_text(
            'package config\nimport "os"\n'
            'func load() { _ = os.Getenv("TG_RUNTIME_SETTING") }\n'
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("TG_RUNTIME_SETTING", result.stdout + result.stderr)

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
            "` stray\n[missing](docs/nope.md) and plain text\n"
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md", result.stdout + result.stderr)
        self.assertIn("docs/nope.md", result.stdout + result.stderr)

    def test_multiline_code_span_hides_markdown_links(self):
        (self.root / "README.md").write_text(
            "``example\n[missing](docs/nope.md)\ncontinued``\n"
        )

        result = self.run_checker()

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_indented_code_block_hides_markdown_links(self):
        (self.root / "README.md").write_text(
            "Example:\n\n"
            "    [missing](docs/nope.md)\n"
            "    more example\n"
        )

        result = self.run_checker()

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_indented_list_content_keeps_markdown_links_visible(self):
        (self.root / "README.md").write_text(
            "1. Install.\n\n"
            "    See [the guide](docs/gone.md).\n"
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("docs/gone.md", result.stdout + result.stderr)

    def test_lazy_list_continuation_preserves_later_indented_link(self):
        (self.root / "README.md").write_text(
            "- Configure the key first\n"
            "lazy continuation\n\n"
            "    See [the guide](docs/gone.md).\n"
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("docs/gone.md", result.stdout + result.stderr)

    def test_nested_indented_code_block_inside_list_hides_markdown_links(self):
        (self.root / "README.md").write_text(
            "1. Install.\n\n"
            "        [missing](docs/nope.md)\n"
            "        more example\n"
        )

        result = self.run_checker()

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_nested_indented_code_block_stops_at_list_content_indent(self):
        (self.root / "README.md").write_text(
            "1. Install.\n\n"
            "        [example](docs/nope.md)\n\n"
            "    See [the guide](docs/gone.md).\n"
        )

        result = self.run_checker()
        output = result.stdout + result.stderr

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("docs/gone.md", output)
        self.assertNotIn("docs/nope.md", output)

    def test_code_span_does_not_cross_table_rows(self):
        (self.root / "README.md").write_text(
            "| `x | [a](docs/gone.md) |\n"
            "| --- | --- |\n"
            "| y ` | z |\n"
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("docs/gone.md", result.stdout + result.stderr)

    def test_code_span_does_not_cross_table_rows_with_single_hyphen_delimiter(self):
        (self.root / "README.md").write_text(
            "| `x | [a](docs/gone.md) |\n"
            "|-|-|\n"
            "| y ` | z |\n"
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("docs/gone.md", result.stdout + result.stderr)

    def test_code_span_does_not_cross_table_rows_inside_nested_list(self):
        (self.root / "README.md").write_text(
            "- outer\n\n"
            "  - inner\n\n"
            "    | `x | [a](docs/gone.md) |\n"
            "    | --- | --- |\n"
            "    | y ` | z |\n"
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("docs/gone.md", result.stdout + result.stderr)

    def test_code_span_does_not_cross_table_rows_inside_blockquote(self):
        (self.root / "README.md").write_text(
            "> | `x | [a](docs/gone.md) |\n"
            "> | --- | --- |\n"
            "> | y ` | z |\n"
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("docs/gone.md", result.stdout + result.stderr)

    def test_indented_table_shaped_code_block_hides_markdown_links(self):
        (self.root / "README.md").write_text(
            "Example:\n\n"
            "    | header | guide |\n"
            "    | --- | --- |\n"
            "    | [missing](docs/nope.md) | sample |\n"
        )

        result = self.run_checker()

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_pipe_line_inside_multiline_code_span_is_not_a_table_boundary(self):
        (self.root / "README.md").write_text(
            "``example\n"
            "| [missing](docs/nope.md)\n"
            "continued``\n"
        )

        result = self.run_checker()

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_code_span_does_not_cross_blank_paragraph_boundary(self):
        (self.root / "README.md").write_text(
            "` unfinished paragraph\n\n"
            "[missing](docs/nope.md) and `valid code`\n"
        )

        result = self.run_checker()
        output = result.stdout + result.stderr

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md links to missing path docs/nope.md", output)

    def test_code_span_does_not_cross_heading_boundary(self):
        (self.root / "README.md").write_text(
            "` unfinished paragraph\n"
            "# Heading\n"
            "[missing](docs/nope.md) and `valid code`\n"
        )

        result = self.run_checker()
        output = result.stdout + result.stderr

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md links to missing path docs/nope.md", output)

    def test_code_span_does_not_cross_fenced_block_boundary(self):
        (self.root / "README.md").write_text(
            "` unfinished paragraph\n"
            "```text\nexample\n```\n"
            "[missing](docs/nope.md) and `valid code`\n"
        )

        result = self.run_checker()
        output = result.stdout + result.stderr

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md links to missing path docs/nope.md", output)

    def test_escaped_backtick_closes_code_span_before_broken_link(self):
        (self.root / "README.md").write_text(
            "`code\\` [missing](docs/nope.md) and `valid`\n"
        )

        result = self.run_checker()
        output = result.stdout + result.stderr

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md links to missing path docs/nope.md", output)

    def test_mismatched_backtick_runs_do_not_hide_broken_link(self):
        (self.root / "README.md").write_text(
            "``foo`bar`` [missing](docs/nope.md) and `code`\n"
        )

        result = self.run_checker()
        output = result.stdout + result.stderr

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md links to missing path docs/nope.md", output)

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

    def test_unclosed_nested_link_reports_raw_line_and_nested_destination(self):
        (self.root / "README.md").write_text(
            "`abcdefghijklmnopqrstuvwxyz`\n"
            "Intro.\n"
            "Details.\n"
            "```text\n"
            "A long fenced line that shifts an offset when code is removed.\n"
            "Another fenced line keeps the broken link far beyond the shortened text.\n"
            "```\n"
            "[broken `example` [nested](docs/nested-gone.md)](docs/gone.md\n"
        )

        result = self.run_checker()
        output = result.stdout + result.stderr

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md:8", output)
        self.assertIn("unclosed Markdown link", output)
        self.assertIn("[broken `example` [nested](docs/nested-gone.md)]", output)
        self.assertIn("links to missing path docs/nested-gone.md", output)

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

    def test_blank_line_before_parenthesized_title_is_rejected(self):
        (self.root / "docs/a.md").write_text("Guide.\n")
        (self.root / "README.md").write_text(
            "[x](docs/a.md\n\n(title))\n[y](docs/gone.md)\n"
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md:1", result.stdout + result.stderr)
        self.assertIn("unclosed Markdown link", result.stdout + result.stderr)
        self.assertIn("docs/gone.md", result.stdout + result.stderr)

    def test_unescaped_parenthesis_in_title_does_not_hide_following_links(self):
        (self.root / "docs/a.md").write_text("Guide.\n")
        (self.root / "README.md").write_text(
            "[x](docs/a.md (Smile :(\n[y](docs/gone.md)\nSee :)))\n"
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("docs/gone.md", result.stdout + result.stderr)

    def test_bare_destination_with_open_parenthesis_does_not_hide_links(self):
        (self.root / "README.md").write_text(
            "[x](https://example.com/a(b and [y](docs/gone.md))x)\n"
        )

        result = self.run_checker()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md", result.stdout + result.stderr)
        self.assertIn("docs/gone.md", result.stdout + result.stderr)

    def test_backslash_space_does_not_escape_bare_destination_whitespace(self):
        (self.root / "README.md").write_text(
            "[x](https://example.com/a\\ and[y](docs/nope.md))\n"
        )

        result = self.run_checker()
        output = result.stdout + result.stderr

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md links to missing path docs/nope.md", output)

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

    def test_missing_relative_unquoted_html_links_name_file_and_targets(self):
        (self.root / "docs/guide.md").write_text(
            "<a href=missing.html>Guide</a> and <img src=missing.svg>.\n"
        )

        result = self.run_checker()
        output = result.stdout + result.stderr

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("docs/guide.md links to missing path missing.html", output)
        self.assertIn("docs/guide.md links to missing path missing.svg", output)

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
