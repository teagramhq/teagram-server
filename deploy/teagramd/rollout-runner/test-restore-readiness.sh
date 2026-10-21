#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
ulimit -c 0

script_dir=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# Last release before the base-schema migrations; it seeds the fixture at its own schema head.
seed_commit=7b62b9ab1cad0c459627ad7a2eb736a40382939b
suite_work_deadline=$((SECONDS + 30 * 60))
suite_teardown_deadline=$((suite_work_deadline + 4 * 60))

emit_preflight_failure() {
  printf 'restore readiness gate failed at stage: %s\n' "${preflight_stage:-unknown}" >&2
  printf '%s\n' '{"result":"fail","phase":"preflight","category":"preflight_failure","migration_head":"not_reached","help_getConfig":"not_run","synthetic_signin":"not_run","cleanup":"not_started","elapsed_ms":0,"exit":1}'
  exit 1
}

suite_remaining_seconds() {
  local remaining=$((suite_work_deadline - SECONDS - 1))
  (( remaining > 0 )) || return 1
  printf '%s\n' "$remaining"
}

suite_remaining_teardown_seconds() {
  local remaining=$((suite_teardown_deadline - SECONDS - 1))
  (( remaining > 0 )) || return 1
  printf '%s\n' "$remaining"
}

run_suite_command() {
  local remaining
  remaining=$(suite_remaining_seconds) || return 124
  /usr/bin/timeout --signal=TERM --kill-after=15s "${remaining}s" "$@"
}

run_teardown_command() {
  local remaining
  remaining=$(suite_remaining_teardown_seconds) || return 124
  /usr/bin/timeout --signal=TERM --kill-after=15s "${remaining}s" "$@"
}

if [[ $# -eq 2 && $1 == "--recover" ]]; then
  exec sudo -n -- env -i \
    HOME=/root \
    LANG=C \
    LC_ALL=C \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    PYTHONDONTWRITEBYTECODE=1 \
    /usr/bin/python3 "$script_dir/restore-readiness.py" --recover "$2"
fi

preflight_stage="validate runner prerequisites"
if [[ $# -ne 0 || $(id -u) -eq 0 ]]; then
  printf '%s\n' '{"result":"fail","phase":"preflight","category":"preflight_failure","migration_head":"not_reached","help_getConfig":"not_run","synthetic_signin":"not_run","cleanup":"not_started","elapsed_ms":0,"exit":1}'
  exit 1
fi

build_uid=$(id -u)
build_env=(
  env -i
  "HOME=$HOME"
  "PATH=$PATH"
  GOENV=off
  GOTOOLCHAIN=local
  GOPROXY=off
  GOSUMDB=off
  CGO_ENABLED=0
)
preflight_stage="check required tools and sudo"
if ! sudo -n true >/dev/null 2>&1 || ! command -v go >/dev/null 2>&1 ||
  [[ ! -x /usr/bin/docker ]] || ! atlas_source=$(command -v atlas); then
  emit_preflight_failure
fi
preflight_stage="read Go target architecture"
if ! go_arch=$(run_suite_command "${build_env[@]}" go env GOARCH 2>/dev/null); then
  emit_preflight_failure
fi
docker_bin=/usr/bin/docker
build_parent=${TMPDIR:-/tmp}
preflight_stage="create build directory"
if ! build_dir=$(mktemp -d "$build_parent/teagram-restore-readiness.XXXXXXXX" 2>/dev/null); then
  emit_preflight_failure
fi
sigkill_output=""

cleanup_build() {
  status=$?
  trap - EXIT
  if [[ -n $sigkill_output ]] && ! run_teardown_command /usr/bin/rm -f -- "$sigkill_output" 2>/dev/null; then
    status=1
  fi
  if ! run_teardown_command /usr/bin/rm -rf -- "$build_dir" 2>/dev/null; then
    status=1
  fi
  exit "$status"
}
trap cleanup_build EXIT

preflight_stage="secure build directory"
if ! chmod 700 "$build_dir" 2>/dev/null; then
  emit_preflight_failure
fi
atlas_bin="$build_dir/atlas"
build_inputs() {
  preflight_stage="build pinned runtime inputs"
  if ! suite_remaining_seconds >/dev/null; then
    emit_preflight_failure
  fi
  if [[ -n ${build_dir_initialized:-} ]]; then
    preflight_stage="recreate runtime input directory"
    if ! mkdir -m 700 -- "$build_dir" 2>/dev/null; then
      emit_preflight_failure
    fi
  else
    build_dir_initialized=1
  fi
  preflight_stage="copy Atlas CLI"
  if ! run_suite_command /usr/bin/cp -- "$atlas_source" "$atlas_bin" 2>/dev/null ||
    ! run_suite_command /usr/bin/chmod 500 "$atlas_bin" 2>/dev/null; then
    emit_preflight_failure
  fi
  preflight_stage="build pinned runtime binaries"
  if ! run_suite_command "${build_env[@]}" go build -trimpath -buildvcs=false -o "$build_dir/teagramd" ./cmd/teagramd >/dev/null 2>&1 ||
    ! run_suite_command "${build_env[@]}" go build -trimpath -buildvcs=false -o "$build_dir/restore-readiness-launcher" ./deploy/teagramd/rollout-runner/restore-readiness-launcher >/dev/null 2>&1 ||
    ! run_suite_command "${build_env[@]}" go build -trimpath -buildvcs=false -o "$build_dir/restore-readiness-seed" ./deploy/teagramd/rollout-runner/restore-readiness-seed >/dev/null 2>&1 ||
    ! run_suite_command "${build_env[@]}" go build -trimpath -buildvcs=false -o "$build_dir/restore-readiness-seed-shim" ./deploy/teagramd/rollout-runner/restore-readiness-seed-shim >/dev/null 2>&1 ||
    ! run_suite_command "${build_env[@]}" go build -trimpath -buildvcs=false -o "$build_dir/restore-readiness-provisioner" ./deploy/teagramd/rollout-runner/restore-readiness-provisioner >/dev/null 2>&1 ||
    ! run_suite_command "${build_env[@]}" go build -trimpath -buildvcs=false -o "$build_dir/restore-readiness-probe" ./deploy/teagramd/rollout-runner/restore-readiness-probe >/dev/null 2>&1 ||
    ! run_suite_command "${build_env[@]}" go build -trimpath -buildvcs=false -o "$build_dir/restore-drill-checker" ./deploy/teagramd/rollout-runner/restore-readiness-checker >/dev/null 2>&1; then
    emit_preflight_failure
  fi
  preflight_stage="create historical seed directory"
  if ! seed_source=$(mktemp -d "$build_parent/teagram-restore-seed.XXXXXXXX" 2>/dev/null); then
    emit_preflight_failure
  fi
  preflight_stage="build pinned historical seed"
  if ! run_suite_command git archive "$seed_commit" 2>/dev/null | run_suite_command tar -x -C "$seed_source" 2>/dev/null ||
    ! (cd "$seed_source" && run_suite_command "${build_env[@]}" go build -trimpath -buildvcs=false -o "$build_dir/teagramd-seed" ./cmd/teagramd >/dev/null 2>&1); then
    run_teardown_command /usr/bin/rm -rf -- "$seed_source" 2>/dev/null || true
    emit_preflight_failure
  fi
  preflight_stage="remove historical seed source"
  if ! run_teardown_command /usr/bin/rm -rf -- "$seed_source"; then
    emit_preflight_failure
  fi
}

run_drill() {
  local work_seconds
  work_seconds=$(suite_remaining_seconds) || return 124
  sudo -n -- env -i \
    HOME=/root \
    LANG=C \
    LC_ALL=C \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    PYTHONDONTWRITEBYTECODE=1 \
    /usr/bin/python3 "$script_dir/restore-readiness.py" \
    --docker-bin "$docker_bin" \
    --atlas-bin "$atlas_bin" \
    --build-dir "$build_dir" \
    --build-uid "$build_uid" \
    --go-arch "$go_arch" \
    --work-seconds "$work_seconds" \
    "$@"
}

build_inputs
preflight_stage="run primary synthetic restore drill"
if ! success_report=$(run_drill); then
  printf '%s\n' "$success_report"
  exit 1
fi
if ! printf '%s' "$success_report" | /usr/bin/python3 -c '
import json, sys
report = json.load(sys.stdin)
if not (report.get("result") == "pass" and report.get("cleanup") == "verified" and report.get("auth_keys_cleared") == "pass" and report.get("exit") == 0):
    raise SystemExit(1)
'; then
  emit_preflight_failure
fi

build_inputs
preflight_stage="run malformed SQL fault drill"
if malformed_report=$(run_drill --fault-test malformed-sql 2>/dev/null); then
  emit_preflight_failure
fi
if ! printf '%s' "$malformed_report" | /usr/bin/python3 -c '
import json, sys
report = json.load(sys.stdin)
if not (report.get("result") == "fail" and report.get("phase") == "restore" and report.get("category") == "restore_failure" and report.get("cleanup") == "verified" and report.get("exit") == 1):
    raise SystemExit(1)
'; then
  emit_preflight_failure
fi

build_inputs
preflight_stage="run auth key failure drill"
if auth_clear_failure_report=$(run_drill --fault-test auth-clear-failure 2>/dev/null); then
  emit_preflight_failure
fi
if ! printf '%s' "$auth_clear_failure_report" | /usr/bin/python3 -c '
import json, sys
report = json.load(sys.stdin)
if not (report.get("result") == "fail" and report.get("phase") == "auth_key_clearing" and report.get("category") == "auth_key_clear_failure" and report.get("cleanup") == "verified" and report.get("exit") == 1):
    raise SystemExit(1)
'; then
  emit_preflight_failure
fi

build_inputs
preflight_stage="run auth key signal drill"
if auth_clear_signal_report=$(run_drill --fault-test auth-clear-signal 2>/dev/null); then
  emit_preflight_failure
fi
if ! printf '%s' "$auth_clear_signal_report" | /usr/bin/python3 -c '
import json, sys
report = json.load(sys.stdin)
if not (report.get("result") == "fail" and report.get("phase") == "auth_key_clearing" and report.get("category") == "interrupted" and report.get("cleanup") == "verified" and report.get("exit") == 1):
    raise SystemExit(1)
'; then
  emit_preflight_failure
fi

build_inputs
preflight_stage="run SIGKILL recovery drill"
if ! sigkill_output=$(run_suite_command /usr/bin/mktemp "$build_parent/teagram-restore-sigkill.XXXXXXXX" 2>/dev/null); then
  emit_preflight_failure
fi
run_drill --fault-test sigkill >"$sigkill_output" 2>/dev/null &
recovery_pid=$!
sigkill_status=0
if wait "$recovery_pid"; then
  emit_preflight_failure
else
  sigkill_status=$?
fi
if [[ $sigkill_status -ne 137 ]] || [[ $(<"$sigkill_output") != "SIGKILL_INJECTED" ]]; then
  emit_preflight_failure
fi
recovered=0
for ((attempt = 0; attempt < 240; attempt++)); do
  if ! remaining_runs=$(run_teardown_command sudo -n -- /usr/bin/find /run -mindepth 1 -maxdepth 1 -type d -name 'restore-readiness-*' -print 2>/dev/null); then
    emit_preflight_failure
  fi
  if [[ -z $remaining_runs ]]; then
    recovered=1
    break
  fi
  if ! run_teardown_command /usr/bin/sleep 1; then
    emit_preflight_failure
  fi
done
if [[ $recovered -ne 1 ]]; then
  emit_preflight_failure
fi
if ! containers=$(run_teardown_command "$docker_bin" ps --all --quiet --no-trunc 2>/dev/null) ||
  ! volumes=$(run_teardown_command "$docker_bin" volume ls --quiet 2>/dev/null) ||
  ! networks=$(run_teardown_command "$docker_bin" network ls --format '{{.Name}}' 2>/dev/null) ||
  ! owned_images=$(run_teardown_command "$docker_bin" image ls --all --quiet --no-trunc --filter 'label=com.teagram.restore-readiness.token' 2>/dev/null); then
  emit_preflight_failure
fi
if [[ -n $containers || -n $volumes || -n $owned_images ]] ||
  ! printf '%s\n' "$networks" | /usr/bin/python3 -c '
import sys
networks = sorted(line.strip() for line in sys.stdin if line.strip())
if networks != ["bridge", "host", "none"]:
    raise SystemExit(1)
' ||
  [[ -n $(/usr/bin/find "$build_dir" -mindepth 1 -print -quit 2>/dev/null) ]] ||
  ! run_teardown_command /usr/bin/python3 -B -c '
import runpy, sys
if runpy.run_path(sys.argv[1])["docker_cli_pids"]():
    raise SystemExit(1)
' "$script_dir/restore-readiness.py"; then
  emit_preflight_failure
fi

printf '%s\n' "$success_report"
