from pathlib import Path
import re
import unittest


WORKFLOW = Path(__file__).parents[1] / "workflows" / "ci.yml"


class ActionsCachePolicyTests(unittest.TestCase):
    def test_buildkit_cache_writes_are_main_only(self) -> None:
        workflow = WORKFLOW.read_text(encoding="utf-8")
        cache_to_values = re.findall(r"(?m)^\s*cache-to:\s*(.+)$", workflow)

        self.assertEqual(len(cache_to_values), 5)
        for value in cache_to_values:
            with self.subTest(value=value):
                self.assertIn("github.ref == 'refs/heads/main'", value)
                self.assertIn("|| ''", value)

    def test_go_build_cache_is_readable_by_prs_but_saved_only_on_main(self) -> None:
        workflow = WORKFLOW.read_text(encoding="utf-8")

        self.assertIn(
            "      - name: Restore Go build and test results\n"
            "        uses: actions/cache/restore@v6.1.0",
            workflow,
        )
        self.assertIn(
            "      - name: Save Go build and test results\n"
            "        if: ${{ success() && github.ref == 'refs/heads/main' }}\n"
            "        uses: actions/cache/save@v6.1.0",
            workflow,
        )


if __name__ == "__main__":
    unittest.main()
