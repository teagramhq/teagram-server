import json
from pathlib import Path
import re
import subprocess
import unittest
from unittest import mock

import prune_go_build_cache


WORKFLOW = Path(__file__).parents[1] / "workflows" / "ci.yml"


class ActionsCachePolicyTests(unittest.TestCase):
    def test_buildkit_cache_writes_are_main_only(self) -> None:
        workflow = WORKFLOW.read_text(encoding="utf-8")
        cache_to_values = re.findall(r"(?m)^\s*cache-to:\s*(.+)$", workflow)

        self.assertEqual(len(cache_to_values), 4)
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
        self.assertIn(
            "      - name: Prune previous Go build snapshots\n"
            "        if: ${{ success() && github.ref == 'refs/heads/main' }}",
            workflow,
        )
        self.assertLess(
            workflow.index("- name: Prune previous Go build snapshots"),
            workflow.index("- name: Save Go build and test results"),
        )

    def test_repeated_main_saves_keep_one_go_build_snapshot(self) -> None:
        caches = [
            {"id": 10, "key": "go-build-Linux-old-hash-1-1", "ref": "refs/heads/main"},
            {"id": 11, "key": "go-build-Linux-pr-hash-2-1", "ref": "refs/pull/905/merge"},
            {"id": 12, "key": "sticker-bundle-v1", "ref": "refs/heads/main"},
            {"id": 13, "key": "teagram-fixture-v1", "ref": "refs/heads/main"},
            {"id": 14, "key": "perf-fixture-v1", "ref": "refs/heads/main"},
        ]

        def fake_gh(args: list[str], **kwargs: object) -> subprocess.CompletedProcess[str]:
            if args[1] == "api" and "--paginate" in args:
                return subprocess.CompletedProcess(
                    args, 0, json.dumps([{"actions_caches": caches}]), ""
                )
            if args[1] == "api" and "DELETE" in args:
                cache_id = int(args[-1].rsplit("/", 1)[1])
                caches[:] = [cache for cache in caches if cache["id"] != cache_id]
                return subprocess.CompletedProcess(args, 0, "", "")
            self.fail(f"unexpected gh command: {args}")

        environment = {
            "GITHUB_REPOSITORY": "teagramhq/teagram-server",
            "GITHUB_REF": "refs/heads/main",
            "RUNNER_OS": "Linux",
        }
        with mock.patch.dict("os.environ", environment), mock.patch.object(
            prune_go_build_cache.subprocess, "run", side_effect=fake_gh
        ):
            for run_number in range(1, 5):
                current_key = f"go-build-Linux-hash-{run_number}-{run_number}"
                with mock.patch.dict("os.environ", {"GO_BUILD_CACHE_KEY": current_key}):
                    prune_go_build_cache.main()
                caches.append(
                    {
                        "id": 100 + run_number,
                        "key": current_key,
                        "ref": "refs/heads/main",
                    }
                )

                main_go_keys = [
                    cache["key"]
                    for cache in caches
                    if cache["ref"] == "refs/heads/main"
                    and cache["key"].startswith("go-build-Linux-")
                ]
                self.assertEqual(main_go_keys, [current_key])
                cache_keys = [cache["key"] for cache in caches]
                self.assertIn("go-build-Linux-pr-hash-2-1", cache_keys)
                self.assertIn("sticker-bundle-v1", cache_keys)
                self.assertIn("teagram-fixture-v1", cache_keys)
                self.assertIn("perf-fixture-v1", cache_keys)


if __name__ == "__main__":
    unittest.main()
