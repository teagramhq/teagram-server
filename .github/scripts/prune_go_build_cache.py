import json
import os
import subprocess


MAIN_REF = "refs/heads/main"


def stale_cache_ids(
    caches: list[dict[str, object]], current_key: str, runner_os: str, ref: str
) -> list[int]:
    if ref != MAIN_REF:
        return []

    prefix = f"go-build-{runner_os}-"
    return [
        int(cache["id"])
        for cache in caches
        if cache.get("ref") == MAIN_REF
        and isinstance(cache.get("key"), str)
        and cache["key"].startswith(prefix)
        and cache["key"] != current_key
    ]


def main() -> int:
    repository = os.environ["GITHUB_REPOSITORY"]
    ref = os.environ["GITHUB_REF"]
    runner_os = os.environ["RUNNER_OS"]
    current_key = os.environ["GO_BUILD_CACHE_KEY"]
    prefix = f"go-build-{runner_os}-"

    if ref != MAIN_REF:
        return 0
    if not current_key.startswith(prefix):
        raise ValueError(f"unexpected Go build cache key: {current_key}")

    response = subprocess.run(
        [
            "gh",
            "api",
            "--paginate",
            "--slurp",
            "--method",
            "GET",
            f"repos/{repository}/actions/caches",
            "-f",
            f"ref={ref}",
            "-f",
            f"key={prefix}",
            "-F",
            "per_page=100",
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    pages = json.loads(response.stdout)
    caches = [cache for page in pages for cache in page.get("actions_caches", [])]

    for cache_id in stale_cache_ids(caches, current_key, runner_os, ref):
        subprocess.run(
            [
                "gh",
                "api",
                "--method",
                "DELETE",
                f"repos/{repository}/actions/caches/{cache_id}",
            ],
            check=True,
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
