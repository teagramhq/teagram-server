#!/usr/bin/env python3
"""Exercise the publication gate around the actual isolated rollout script."""

import hashlib
import json
import os
import pathlib
import runpy
import shutil
import subprocess
import sys
import tempfile


SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
FIXTURES = runpy.run_path(str(SCRIPT_DIR / "test-web-release.py"))
ENDPOINT = "wss://web.example/apiws"
FINGERPRINT = "c" * 16


def archive(root, char):
    source = char * 40
    entries = {"./index.html": b'<title>Teagram Web</title><script type="module"></script>',
               "./app.js": f"console.log('{char}')".encode()}
    digest = FIXTURES["artifact_digest"](entries)
    entries["./mtproto-target.json"] = json.dumps({
        "mode": "private", "endpoint": ENDPOINT, "fingerprint": FINGERPRINT,
        "sourceCommit": source, "artifactDigest": digest,
    }).encode()
    path = root / f"{char}.tar.gz"
    FIXTURES["write_archive"](path, entries)
    path.chmod(0o600)
    return source, digest, path, hashlib.sha256(path.read_bytes()).hexdigest()


def main():
    with tempfile.TemporaryDirectory() as directory:
        root = pathlib.Path(directory)
        scripts = root / "scripts"
        scripts.mkdir()
        bin_dir = root / "bin"
        bin_dir.mkdir()
        python = bin_dir / "python3"
        python.write_text('''#!/usr/bin/env bash
if [[ "${1##*/}" == web-publication-state.py && "${2:-}" == commit && "${TEST_COMMIT_FAILURE:-}" == true ]]; then
  exit 1
fi
exec "$TEST_SYSTEM_PYTHON" "$@"
''', encoding="utf-8")
        python.chmod(0o755)
        for name in ("publish-web-release.sh", "install-web-release.py", "web-publication-state.py"):
            shutil.copyfile(SCRIPT_DIR / name, scripts / name)
        (scripts / "demo-common.sh").write_text('''
DEMO_STATE_DIR="$TEST_ROOT/state"
DEMO_WEB_ROOT="$TEST_ROOT/web"
DEMO_CLIENT_CONFIG_DIR="$DEMO_WEB_ROOT"
DEMO_WEB_RELEASES_DIR="$DEMO_WEB_ROOT/releases"
DEMO_WEB_CURRENT_LINK="$DEMO_WEB_ROOT/current"
DEMO_CLIENT_CONFIG="$TEST_ROOT/client"
demo_fail() { printf '%s\\n' "$1" >&2; return 1; }
demo_require_root() { :; }
demo_require_env() { :; }
demo_prepare_client_config_dir() { mkdir -p "$DEMO_STATE_DIR" "$DEMO_WEB_RELEASES_DIR"; }
demo_active_server() { echo teagramd; }
demo_active_image_ref() { echo image; }
demo_reconcile_web_release() {
  if [[ -L "$DEMO_WEB_CURRENT_LINK" ]]; then
    target=$(readlink "$DEMO_WEB_CURRENT_LINK"); name=${target##*/}
    printf '%s %s\\n' "${name:0:40}" "${name:41}"
  else printf '\\n'; fi
}
demo_require_proxy_configuration_current() { :; }
demo_write_active_release() { printf '%s\\n' "$*" >> "$TEST_ROOT/active-writes"; }
''', encoding="utf-8")
        (scripts / "resolve_web_artifact_pin.py").write_text(
            'import json, os\nprint(json.dumps({"runId": int(os.environ["TEST_BASELINE_RUN"])}))\n',
            encoding="utf-8",
        )
        (scripts / "smoke.py").write_text(
            'import os, pathlib, sys\n'
            'pathlib.Path(os.environ["TEST_ROOT"], "smokes").open("a").write("smoke\\n")\n'
            'sys.exit(1 if os.environ.get("TEST_SMOKE_FAILURE") == "true" else 0)\n',
            encoding="utf-8",
        )
        old = archive(root, "a")
        newer = archive(root, "b")
        stale = archive(root, "d")
        state_dir = root / "state"
        releases = root / "web/releases"
        state_dir.mkdir(mode=0o755)
        releases.mkdir(parents=True, mode=0o755)
        old_name = f"{old[0]}-{old[1][7:]}"
        old_release = releases / old_name
        old_release.mkdir()
        (old_release / "index.html").write_bytes(b'<title>Teagram Web</title><script type="module"></script>')
        (old_release / "app.js").write_bytes(b"console.log('a')")
        (old_release / "mtproto-target.json").write_text(json.dumps({
            "mode": "private", "endpoint": ENDPOINT, "fingerprint": FINGERPRINT,
            "sourceCommit": old[0], "artifactDigest": old[1],
        }))
        current = root / "web/current"
        current.symlink_to("releases/" + old_name)

        def deploy(item, run_id, *, rollback=False, fail_smoke=False, fail_commit=False):
            source, digest, path, checksum = item
            env = dict(os.environ, TEST_ROOT=str(root), TEST_BASELINE_RUN="100",
                       PATH=f"{bin_dir}{os.pathsep}{os.environ['PATH']}", TEST_SYSTEM_PYTHON=sys.executable,
                       DEPLOY_WEB_SOURCE_SHA=source, DEPLOY_WEB_ARTIFACT_DIGEST=digest,
                       DEPLOY_WEB_ARCHIVE=str(path), DEPLOY_WEB_ARCHIVE_SHA256=checksum,
                       DEPLOY_WEB_EXPECTED_ENDPOINT=ENDPOINT, DEPLOY_WEB_EXPECTED_FINGERPRINT=FINGERPRINT,
                       DEPLOY_WEB_PUBLICATION_RUN_ID=str(run_id),
                       DEPLOY_WEB_ROLLBACK_ALLOWED="true" if rollback else "false",
                       TEST_SMOKE_FAILURE="true" if fail_smoke else "false",
                       TEST_COMMIT_FAILURE="true" if fail_commit else "false")
            return subprocess.run(["bash", str(scripts / "publish-web-release.sh")],
                                  env=env, capture_output=True, text=True)

        if deploy(stale, 99).returncode == 0 or os.readlink(current) != "releases/" + old_name:
            raise SystemExit("rollout accepted an older publication without authorization")
        result = deploy(newer, 101)
        if result.returncode != 0:
            raise SystemExit(f"new publication failed: {result.stderr}")
        new_name = f"{newer[0]}-{newer[1][7:]}"
        if os.readlink(current) != "releases/" + new_name:
            raise SystemExit("new publication was not activated")
        smoke_count = (root / "smokes").read_text().count("smoke")
        if deploy(newer, 101).returncode != 0 or (root / "smokes").read_text().count("smoke") != smoke_count:
            raise SystemExit("the installed publication was not a no-op")
        if deploy(stale, 99).returncode == 0:
            raise SystemExit("rollout accepted a replay after recording a newer run")
        if deploy(stale, 99, rollback=True).returncode != 0:
            raise SystemExit("authorized rollback failed")
        stale_name = f"{stale[0]}-{stale[1][7:]}"
        if os.readlink(current) != "releases/" + stale_name:
            raise SystemExit("authorized rollback was not activated")
        failed = deploy(old, 102, fail_smoke=True)
        if failed.returncode == 0 or os.readlink(current) != "releases/" + stale_name:
            raise SystemExit("failed rollout did not restore the prior web release")
        record = json.loads((state_dir / "web-publication.json").read_text())
        if record["installed"]["run_id"] != 99 or record["highest"]["run_id"] != 102:
            raise SystemExit("failed rollout lost its installed identity or reserved watermark")
        if deploy(newer, 101).returncode == 0:
            raise SystemExit("a failed newer attempt allowed a publication replay")
        if deploy(old, 102).returncode != 0:
            raise SystemExit("the interrupted newer publication could not be retried")
        last = archive(root, "e")
        failed_commit = deploy(last, 103, fail_commit=True)
        if failed_commit.returncode == 0 or os.readlink(current) != "releases/" + old_name:
            raise SystemExit("failed publication state commit did not restore the prior web release")
        if (root / "active-writes").read_text().splitlines()[-1] != f"teagramd image {old[0]} {old[1]}":
            raise SystemExit("failed publication state commit did not restore active metadata")
        record = json.loads((state_dir / "web-publication.json").read_text())
        if record["installed"]["run_id"] != 102 or record["highest"]["run_id"] != 103:
            raise SystemExit("failed state commit lost the installed run or reserved watermark")
    print("demo web publication rollout checks passed")


if __name__ == "__main__":
    main()
