#!/usr/bin/env python3
"""The deploy run must use the web publication rebuilt in that run."""

import os
import pathlib
import subprocess
import tempfile


ROOT = pathlib.Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github/workflows/deploy-demo.yml"


def main() -> None:
    download = subprocess.check_output(
        ["yq", "-r", '.jobs."deploy-demo".steps[] | select(.id == "demo-web-artifact") | .run', str(WORKFLOW)],
        text=True,
    )
    gate = subprocess.check_output(
        ["yq", "-r", '.jobs."deploy-demo".steps[] | select(.id == "web-gate") | .run', str(WORKFLOW)],
        text=True,
    )
    publish = subprocess.check_output(
        ["yq", "-r", '.jobs."deploy-demo".steps[] | select(.name == "Publish the verified web artifact on the demo origin") | .run', str(WORKFLOW)],
        text=True,
    )
    with tempfile.TemporaryDirectory() as directory:
        temp = pathlib.Path(directory)
        bin_dir = temp / "bin"
        bin_dir.mkdir()
        gh = bin_dir / "gh"
        gh.write_text(
            """#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == run && "$2" == download && "$3" == "$GITHUB_RUN_ID" ]]
[[ "$4" == --repo && "$6" == --name && "$7" == "$WEB_ARTIFACT_NAME" && "$8" == --dir ]]
archive="$9/demo-web-$WEB_SOURCE_SHA.tar.gz"
printf 'new publication' > "$archive"
if [[ "${BAD_CHECKSUM:-}" == true ]]; then
  printf '%064d\n' 0 > "$archive.sha256"
else
  sha256sum "$archive" | awk '{print $1}' > "$archive.sha256"
fi
""",
            encoding="utf-8",
        )
        gh.chmod(0o755)
        env = dict(os.environ)
        # Server CI used run 100; a newer web publication was built in run 200.
        # A failed-only rerun can reuse the artifact from the first attempt.
        env.update(
            PATH=f"{bin_dir}{os.pathsep}{env['PATH']}",
            RUNNER_TEMP=str(temp),
            GITHUB_OUTPUT=str(temp / "output"),
            GITHUB_REPOSITORY="teagramhq/teagram-server",
            GITHUB_RUN_ID="200",
            GITHUB_RUN_ATTEMPT="2",
            CI_RUN_ID="100",
            WEB_SOURCE_SHA="b" * 40,
            WEB_ARTIFACT_NAME="demo-web-release-" + "b" * 40 + "-1",
        )
        subprocess.run(["bash", "-eo", "pipefail", "-c", download], env=env, check=True)
        output = (temp / "output").read_text(encoding="utf-8")
        if f"demo-web-{'b' * 40}.tar.gz" not in output or "archive_sha256=" not in output:
            raise SystemExit("the newer deploy-time publication was not downloaded and verified")
        env["BAD_CHECKSUM"] = "true"
        if subprocess.run(["bash", "-eo", "pipefail", "-c", download], env=env).returncode == 0:
            raise SystemExit("the deploy accepted a mismatched archive checksum")
        for raw_result, job_result, enabled in (
            ("success", "success", "true"),
            ("failure", "success", "false"),
            ("success", "failure", "false"),
            ("", "success", "false"),
        ):
            gate_output = temp / "gate-output"
            gate_output.write_text("", encoding="utf-8")
            env.update(
                GITHUB_OUTPUT=str(gate_output),
                DEMO_WEB_BUILD_RESULT=raw_result,
                DEMO_WEB_JOB_RESULT=job_result,
            )
            subprocess.run(["bash", "-eo", "pipefail", "-c", gate], env=env, check=True, capture_output=True)
            if gate_output.read_text(encoding="utf-8").strip() != f"enabled={enabled}":
                raise SystemExit("the web gate accepted an unsuccessful build")
        ssh = bin_dir / "ssh"
        ssh.write_text('#!/usr/bin/env bash\nprintf "%s\\n" "$@" > "$TEST_SSH_ARGS"\ncat > /dev/null\n', encoding="utf-8")
        ssh.chmod(0o755)
        archive = temp / "demo-web-artifact" / f"demo-web-{'b' * 40}.tar.gz"
        ssh_args = temp / "ssh-args"
        env.update(DEPLOY_SHA="a" * 40, WEB_ARTIFACT_DIGEST="sha256:" + "c" * 64,
                   WEB_ARCHIVE_SHA256="d" * 64, WEB_ARCHIVE=str(archive),
                   DEMO_DEPLOY_HOST="demo.example", DEMO_DEPLOY_PORT="2223",
                   WEB_PUBLICATION_RUN_ID="300", TEST_SSH_ARGS=str(ssh_args))
        for event, requested, suffix in (("workflow_run", "", "0"),
                                         ("workflow_dispatch", "300", "200")):
            env.update(GITHUB_EVENT_NAME=event, WEB_ROLLBACK_REQUESTED=requested)
            subprocess.run(["bash", "-eo", "pipefail", "-c", publish], env=env, check=True)
            if not ssh_args.read_text().splitlines()[-1].endswith(" " + suffix):
                raise SystemExit("rollback was not bound to the explicit dispatch run")
        ssh_args.unlink()
        env.update(GITHUB_EVENT_NAME="workflow_run", WEB_ROLLBACK_REQUESTED="300")
        if subprocess.run(["bash", "-eo", "pipefail", "-c", publish], env=env).returncode == 0 or ssh_args.exists():
            raise SystemExit("an automatic deploy was allowed to request rollback")
        env.update(GITHUB_EVENT_NAME="workflow_dispatch", WEB_ROLLBACK_REQUESTED="301")
        if subprocess.run(["bash", "-eo", "pipefail", "-c", publish], env=env).returncode == 0 or ssh_args.exists():
            raise SystemExit("a dispatch was allowed to roll back a different publication")
    print("deploy-time web artifact selection passed")


if __name__ == "__main__":
    main()
