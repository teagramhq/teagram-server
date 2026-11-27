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

    def test_go_build_cache_is_readable_by_prs_and_all_suites_upload_outputs(self) -> None:
        workflow = WORKFLOW.read_text(encoding="utf-8")

        test_jobs = ("test-go", "test-go-e2e", "test-go-fleet-cap")
        artifact_name = (
            "go-build-cache-${{ github.run_id }}-${{ github.run_attempt }}-${{ github.job }}"
        )
        for job_name in test_jobs:
            match = re.search(
                rf"(?ms)^  {re.escape(job_name)}:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n|\Z)",
                workflow,
            )
            self.assertIsNotNone(match, f"missing {job_name} job")
            body = match.group("body")
            with self.subTest(job=job_name):
                self.assertIn("uses: actions/cache/restore@v6.1.0", body)
                self.assertIn("name: Upload Go build cache snapshot", body)
                self.assertIn(f"name: {artifact_name}", body)
                self.assertIn("path: ~/.cache/go-build", body)
                self.assertIn("retention-days: 1", body)
                self.assertNotIn("Prune previous Go build snapshots", body)
                self.assertNotIn("Save Go build and test results", body)

        cache_job = re.search(
            r"(?ms)^  ops-go-build-cache:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n|\Z)",
            workflow,
        )
        self.assertIsNotNone(cache_job, "missing Go build cache aggregation job")
        cache_body = cache_job.group("body")
        self.assertIn("name: 'ops: go-build-cache'", cache_body)
        self.assertIn("if: ${{ always() }}", cache_body)
        eligibility = cache_body.split("- name: Check whether the complete Go cache can be saved", 1)[1]
        for job_name, env_name in (
            ("test-go", "TEST_GO_RESULT"),
            ("test-go-e2e", "TEST_GO_E2E_RESULT"),
            ("test-go-fleet-cap", "TEST_GO_FLEET_CAP_RESULT"),
        ):
            self.assertIn(f"{env_name}: ${{{{ needs.{job_name}.result }}}}", eligibility)
            self.assertIn(f'"${env_name}" == "success"', eligibility)
        self.assertIn('"$GITHUB_REF" == "refs/heads/main"', eligibility)
        self.assertIn("steps.eligibility.outputs.eligible == 'true'", cache_body)
        self.assertIn("uses: actions/cache/restore@v6.1.0", cache_body)
        self.assertIn("uses: actions/download-artifact@v8.0.2", cache_body)
        self.assertIn("pattern: go-build-cache-${{ github.run_id }}-${{ github.run_attempt }}-*", cache_body)
        self.assertIn("merge-multiple: true", cache_body)
        self.assertLess(
            cache_body.index("uses: actions/download-artifact@v8.0.2"),
            cache_body.index("- name: Prune previous Go build snapshots"),
        )
        self.assertLess(
            cache_body.index("- name: Prune previous Go build snapshots"),
            cache_body.index("- name: Save Go build and test results"),
        )

        ci_job = re.search(
            r"(?ms)^  ci:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n|\Z)",
            workflow,
        )
        self.assertIsNotNone(ci_job, "missing CI aggregate job")
        ci_body = ci_job.group("body")
        self.assertIn("      - ops-go-build-cache\n", ci_body)
        self.assertIn("GO_BUILD_CACHE_RESULT: ${{ needs.ops-go-build-cache.result }}", ci_body)
        self.assertIn('"ops-go-build-cache=$GO_BUILD_CACHE_RESULT"', ci_body)
        self.assertIn('"$GO_BUILD_CACHE_RESULT" != "success"', ci_body)

        publisher = re.search(
            r"(?ms)^  publish-k8s-images:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n|\Z)",
            workflow,
        )
        self.assertIsNotNone(publisher, "missing k8s image publisher job")
        self.assertNotIn("ops-go-build-cache", publisher.group("body"))

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
