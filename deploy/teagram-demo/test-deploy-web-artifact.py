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
    print("deploy-time web artifact selection passed")


if __name__ == "__main__":
    main()
