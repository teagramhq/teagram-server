#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
ulimit -c 0

script_dir=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# Last release before the base-schema migrations; it seeds the fixture at its own schema head.
seed_commit=7b62b9ab1cad0c459627ad7a2eb736a40382939b
PREPARED_BUILD_FLAGS=(-trimpath -buildvcs=false -p=1)
preparation_timeout_seconds=$((45 * 60))
suite_work_deadline=0
suite_teardown_deadline=0
preparation_deadline=0

emit_preflight_failure() {
  printf 'restore readiness gate failed at stage: %s\n' "${preflight_stage:-unknown}" >&2
  printf '%s\n' '{"result":"fail","phase":"preflight","category":"preflight_failure","migration_head":"not_reached","help_getConfig":"not_run","synthetic_signin":"not_run","cleanup":"not_started","elapsed_ms":0,"exit":1}'
  exit 1
}

emit_prepared_cleanup_failure() {
  printf '%s\n' '{"result":"fail","phase":"cleanup","category":"cleanup_failure","migration_head":"not_reached","help_getConfig":"not_run","synthetic_signin":"not_run","cleanup":"unverified","elapsed_ms":0,"exit":1}'
  exit 1
}

suite_remaining_seconds() {
  local remaining=$((suite_work_deadline - SECONDS - 1))
  (( remaining > 0 )) || return 1
  printf '%s\n' "$remaining"
}

preparation_remaining_seconds() {
  local remaining=$((preparation_deadline - SECONDS - 1))
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

run_preparation_command() {
  local remaining
  remaining=$(preparation_remaining_seconds) || return 124
  /usr/bin/timeout --signal=TERM --kill-after=15s "${remaining}s" "$@"
}

run_preparation_cleanup_command() {
  /usr/bin/timeout --signal=TERM --kill-after=15s 60s "$@"
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

local_mode=""
local_args=()
prepared_dir=""
teardown_dir=""
prepare_only=0
while (($#)); do
  case "$1" in
    --local-dump)
      [[ -z $local_mode && $# -ge 2 && -n $2 ]] || { emit_preflight_failure; }
      local_mode="local-dump"
      local_args=(--local-dump "$2")
      shift 2
      ;;
    --local-fixture-test)
      [[ -z $local_mode ]] || { emit_preflight_failure; }
      local_mode="local-fixture-test"
      local_args=(--local-fixture-test)
      shift
      ;;
    --prepared-dir)
      [[ -z $prepared_dir && $# -ge 2 && -n $2 ]] || { emit_preflight_failure; }
      prepared_dir=$2
      shift 2
      ;;
    --prepare-only)
      (( prepare_only == 0 )) || { emit_preflight_failure; }
      prepare_only=1
      shift
      ;;
    --teardown-prepared)
      [[ -z $teardown_dir && $# -ge 2 && -n $2 ]] || { emit_preflight_failure; }
      teardown_dir=$2
      shift 2
      ;;
    *)
      emit_preflight_failure
      ;;
  esac
done
if (( prepare_only == 1 )) && [[ -n $local_mode || -n $prepared_dir || -n $teardown_dir ]]; then
  emit_preflight_failure
fi
if [[ -n $teardown_dir ]] && [[ -n $local_mode || -n $prepared_dir || $prepare_only -eq 1 ]]; then
  emit_preflight_failure
fi

preflight_stage="validate runner prerequisites"
if [[ $(id -u) -eq 0 ]]; then
  printf '%s\n' '{"result":"fail","phase":"preflight","category":"preflight_failure","migration_head":"not_reached","help_getConfig":"not_run","synthetic_signin":"not_run","cleanup":"not_started","elapsed_ms":0,"exit":1}'
  exit 1
fi
build_uid=$(id -u)
if [[ -n $teardown_dir ]]; then
  if ! command -v sudo >/dev/null 2>&1 ||
    ! sudo -n -- env -i \
      HOME=/root \
      LANG=C \
      LC_ALL=C \
      PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
      PYTHONDONTWRITEBYTECODE=1 \
      /usr/bin/python3 "$script_dir/restore-readiness.py" \
      --teardown-prepared "$teardown_dir" --build-uid "$build_uid"; then
    emit_preflight_failure
  fi
  [[ ! -e $teardown_dir && ! -L $teardown_dir ]] || emit_preflight_failure
  exit 0
fi

if [[ $local_mode == "local-dump" ]]; then
  if ! command -v sudo >/dev/null 2>&1; then
    emit_preflight_failure
  fi
  if ! sudo -n -- env -i \
    HOME=/root \
    LANG=C \
    LC_ALL=C \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    PYTHONDONTWRITEBYTECODE=1 \
    /usr/bin/python3 "$script_dir/restore-readiness.py" \
    --validate-local-dump "${local_args[1]}"; then
    exit 1
  fi
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
  GOMAXPROCS=1
)
preflight_stage="check required tools and sudo"
if ! sudo -n true >/dev/null 2>&1 || [[ ! -x /usr/bin/docker ]]; then
  emit_preflight_failure
fi
docker_bin=/usr/bin/docker
build_parent=/tmp
build_dir=$prepared_dir
sigkill_output=""
seed_source=""
inventory_baseline=""
keep_prepared=0

cleanup_prepared_inputs() {
  [[ -n $build_dir ]] || return 0
  if ! run_preparation_cleanup_command sudo -n -- env -i \
    HOME=/root \
    LANG=C \
    LC_ALL=C \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    PYTHONDONTWRITEBYTECODE=1 \
    /usr/bin/python3 "$script_dir/restore-readiness.py" \
    --teardown-prepared "$build_dir" --build-uid "$build_uid"; then
    return 1
  fi
  [[ ! -e $build_dir && ! -L $build_dir ]] || return 1
  build_dir=""
}

preserve_prepared_on_unverified_cleanup() {
  if ! printf '%s' "$1" | /usr/bin/python3 -c '
import json, sys
try:
    report = json.load(sys.stdin)
except (OSError, ValueError):
    raise SystemExit(1)
if not isinstance(report, dict) or report.get("cleanup") not in {"verified", "not_started"}:
    raise SystemExit(1)
'; then
    keep_prepared=1
  fi
}

cleanup_prepared_after_failed_drill() {
  preserve_prepared_on_unverified_cleanup "$1"
  if (( keep_prepared == 0 )) && ! cleanup_prepared_inputs; then
    emit_prepared_cleanup_failure
  fi
}

cleanup_build() {
  status=$?
  trap - EXIT
  if [[ -n $sigkill_output ]] && ! run_preparation_cleanup_command /usr/bin/rm -f -- "$sigkill_output" 2>/dev/null; then
    status=1
  fi
  if [[ -n $seed_source ]] && ! run_preparation_cleanup_command /usr/bin/rm -rf -- "$seed_source" 2>/dev/null; then
    status=1
  fi
  if [[ -n $inventory_baseline ]] && ! run_preparation_cleanup_command /usr/bin/rm -f -- "$inventory_baseline" 2>/dev/null; then
    status=1
  fi
  if (( keep_prepared == 0 )); then
    if ! cleanup_prepared_inputs; then status=1; fi
  elif [[ -n $build_dir ]]; then
    printf 'restore readiness prepared runtime retained for recovery: %s\n' "$build_dir" >&2
  fi
  exit "$status"
}
trap cleanup_build EXIT

build_inputs() {
  preparation_deadline=$((SECONDS + preparation_timeout_seconds))
  preparation_started=$SECONDS
  preflight_stage="validate clean source tree"
  if ! run_preparation_command /usr/bin/python3 -B "$script_dir/restore-readiness.py" --validate-source; then
    emit_preflight_failure
  fi
  if [[ -z $build_dir ]]; then
    preflight_stage="create prepared runtime directory"
    if ! build_dir=$(mktemp -d "$build_parent/teagram-restore-readiness.XXXXXXXX" 2>/dev/null); then
      emit_preflight_failure
    fi
    if ! chmod 700 "$build_dir" 2>/dev/null; then emit_preflight_failure; fi
  fi
  preflight_stage="read pinned Go toolchain"
  if ! command -v go >/dev/null 2>&1 || ! atlas_source=$(command -v atlas) ||
    ! go_binary=$(command -v go) ||
    ! go_version=$(run_preparation_command "${build_env[@]}" go version 2>/dev/null) ||
    ! go_root=$(run_preparation_command "${build_env[@]}" go env GOROOT 2>/dev/null) ||
    ! go_arch=$(run_preparation_command "${build_env[@]}" go env GOARCH 2>/dev/null); then
    emit_preflight_failure
  fi
  preflight_stage="copy Atlas CLI"
  atlas_bin="$build_dir/atlas"
  if ! run_preparation_command /usr/bin/cp -- "$atlas_source" "$atlas_bin" 2>/dev/null ||
    ! run_preparation_command /usr/bin/chmod 500 "$atlas_bin" 2>/dev/null; then
    emit_preflight_failure
  fi
  preflight_stage="build pinned runtime binaries"
  if ! run_preparation_command "${build_env[@]}" go build "${PREPARED_BUILD_FLAGS[@]}" -o "$build_dir/teagramd" ./cmd/teagramd >/dev/null 2>&1 ||
    ! run_preparation_command "${build_env[@]}" go build "${PREPARED_BUILD_FLAGS[@]}" -o "$build_dir/restore-readiness-launcher" ./deploy/teagramd/rollout-runner/restore-readiness-launcher >/dev/null 2>&1 ||
    ! run_preparation_command "${build_env[@]}" go build "${PREPARED_BUILD_FLAGS[@]}" -o "$build_dir/restore-readiness-seed" ./deploy/teagramd/rollout-runner/restore-readiness-seed >/dev/null 2>&1 ||
    ! run_preparation_command "${build_env[@]}" go build "${PREPARED_BUILD_FLAGS[@]}" -o "$build_dir/restore-readiness-seed-shim" ./deploy/teagramd/rollout-runner/restore-readiness-seed-shim >/dev/null 2>&1 ||
    ! run_preparation_command "${build_env[@]}" go build "${PREPARED_BUILD_FLAGS[@]}" -o "$build_dir/restore-readiness-provisioner" ./deploy/teagramd/rollout-runner/restore-readiness-provisioner >/dev/null 2>&1 ||
    ! run_preparation_command "${build_env[@]}" go build "${PREPARED_BUILD_FLAGS[@]}" -o "$build_dir/restore-readiness-probe" ./deploy/teagramd/rollout-runner/restore-readiness-probe >/dev/null 2>&1 ||
    ! run_preparation_command "${build_env[@]}" go build "${PREPARED_BUILD_FLAGS[@]}" -o "$build_dir/restore-drill-checker" ./deploy/teagramd/rollout-runner/restore-readiness-checker >/dev/null 2>&1; then
    emit_preflight_failure
  fi
  preflight_stage="create historical seed directory"
  if ! seed_source=$(mktemp -d "$build_parent/teagram-restore-seed.XXXXXXXX" 2>/dev/null); then
    emit_preflight_failure
  fi
  preflight_stage="build pinned historical seed"
  if ! run_preparation_command git archive "$seed_commit" 2>/dev/null | run_preparation_command tar -x -C "$seed_source" 2>/dev/null ||
    ! (cd "$seed_source" && run_preparation_command "${build_env[@]}" go build "${PREPARED_BUILD_FLAGS[@]}" -o "$build_dir/teagramd-seed" ./cmd/teagramd >/dev/null 2>&1); then
    emit_preflight_failure
  fi
  preflight_stage="remove historical seed source"
  if ! run_preparation_cleanup_command /usr/bin/rm -rf -- "$seed_source" || [[ -e $seed_source || -L $seed_source ]]; then
    emit_preflight_failure
  fi
  seed_source=""
  preflight_stage="attest prepared runtime"
  if ! run_preparation_command sudo -n -- env -i \
    HOME=/root \
    LANG=C \
    LC_ALL=C \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    PYTHONDONTWRITEBYTECODE=1 \
    /usr/bin/python3 "$script_dir/restore-readiness.py" \
    --attest-prepared "$build_dir" \
    --build-uid "$build_uid" \
    --go-version "$go_version" \
    --go-root "$go_root" \
    --go-binary "$go_binary" \
    --go-arch "$go_arch"; then
    emit_preflight_failure
  fi
}

validate_prepared_runtime() {
  if (( preparation_deadline == 0 )); then
    preparation_deadline=$((SECONDS + preparation_timeout_seconds))
  fi
  preflight_stage="verify prepared runtime attestation"
  if ! run_preparation_command sudo -n -- env -i \
    HOME=/root \
    LANG=C \
    LC_ALL=C \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    PYTHONDONTWRITEBYTECODE=1 \
    /usr/bin/python3 "$script_dir/restore-readiness.py" \
    --validate-prepared "$build_dir"; then
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
    --prepared-dir "$build_dir" \
    --work-seconds "$work_seconds" \
    "$@"
}

run_sigkill_recovery_drill() {
  local -a drill_args=("$@")
  local attempt recovered remaining_runs sigkill_status original_keep_prepared sigkill_marker
  preflight_stage="run SIGKILL recovery drill"
  original_keep_prepared=$keep_prepared
  keep_prepared=1
  if ! sigkill_output=$(run_suite_command /usr/bin/mktemp "$build_parent/teagram-restore-sigkill.XXXXXXXX" 2>/dev/null); then
    emit_preflight_failure
  fi
  run_drill "${drill_args[@]}" >"$sigkill_output" 2>/dev/null &
  recovery_pid=$!
  if wait "$recovery_pid"; then
    sigkill_status=0
  else
    sigkill_status=$?
  fi
  sigkill_marker=missing
  if [[ $(<"$sigkill_output") == "SIGKILL_INJECTED" ]]; then
    sigkill_marker=present
  elif [[ -s $sigkill_output ]]; then
    sigkill_marker=other
  fi
  if [[ $sigkill_status -ne 137 ]] || [[ $sigkill_marker != present ]]; then
    printf 'restore readiness SIGKILL drill returned status=%s marker=%s\n' \
      "$sigkill_status" "$sigkill_marker" >&2
    preserve_prepared_on_unverified_cleanup "$(<"$sigkill_output")"
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
  if ! run_teardown_command sudo -n -- env -i \
      HOME=/root \
      LANG=C \
      LC_ALL=C \
      PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
      PYTHONDONTWRITEBYTECODE=1 \
      /usr/bin/python3 -B "$script_dir/test-restore-readiness-inventory.py" compare "$inventory_baseline" ||
    ! run_teardown_command sudo -n -- env -i \
      HOME=/root \
      LANG=C \
      LC_ALL=C \
      PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
      PYTHONDONTWRITEBYTECODE=1 \
      /usr/bin/python3 "$script_dir/restore-readiness.py" \
      --validate-prepared "$build_dir" ||
    ! run_teardown_command /usr/bin/python3 -B -c '
import runpy, sys
if runpy.run_path(sys.argv[1])["docker_cli_pids"]():
    raise SystemExit(1)
' "$script_dir/restore-readiness.py"; then
    emit_preflight_failure
  fi
  keep_prepared=$original_keep_prepared
}

if [[ -z $build_dir ]]; then
  build_inputs
else
  preparation_started=$SECONDS
  preparation_deadline=$((SECONDS + preparation_timeout_seconds))
fi
validate_prepared_runtime
if (( prepare_only == 1 )); then
  keep_prepared=1
  prepared_elapsed_ms=$(((SECONDS - preparation_started) * 1000))
  printf '{"result":"prepared","artifact_count":9,"prepared_dir":"%s","elapsed_ms":%d}\n' "$build_dir" "$prepared_elapsed_ms"
  exit 0
fi
suite_work_deadline=$((SECONDS + 30 * 60))
suite_teardown_deadline=$((suite_work_deadline + 4 * 60))
preflight_stage="save baseline Docker resource inventory"
if ! inventory_baseline=$(run_suite_command /usr/bin/mktemp "$build_parent/restore-readiness-docker-inventory.XXXXXXXX" 2>/dev/null) ||
  ! run_suite_command sudo -n -- env -i \
    HOME=/root \
    LANG=C \
    LC_ALL=C \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    PYTHONDONTWRITEBYTECODE=1 \
    /usr/bin/python3 -B "$script_dir/test-restore-readiness-inventory.py" snapshot >"$inventory_baseline"; then
  emit_preflight_failure
fi

if [[ -n $local_mode ]]; then
  preflight_stage="run local-dump restore drill"
  if ! local_report=$(run_drill "${local_args[@]}"); then
    cleanup_prepared_after_failed_drill "$local_report"
    printf '%s\n' "$local_report"
    exit 1
  fi
  if ! printf '%s' "$local_report" | /usr/bin/python3 -c '
import json, sys
report = json.load(sys.stdin)
if not (report.get("result") == "pass" and report.get("input_mode") == "local" and report.get("cleanup") == "verified" and report.get("auth_keys_cleared") == "pass" and report.get("exit") == 0):
    raise SystemExit(1)
'; then
    emit_preflight_failure
  fi
  if [[ $local_mode == "local-fixture-test" ]]; then
    preflight_stage="run local auth-key signal fault drill"
    if auth_clear_signal_report=$(run_drill --local-fixture-test --fault-test auth-clear-signal); then
      printf 'restore readiness local signal fault unexpectedly passed: %s\n' "$auth_clear_signal_report" >&2
      emit_preflight_failure
    fi
    preserve_prepared_on_unverified_cleanup "$auth_clear_signal_report"
    if ! printf '%s' "$auth_clear_signal_report" | /usr/bin/python3 -c '
import json, sys
report = json.load(sys.stdin)
if not (report.get("result") == "fail" and report.get("phase") == "auth_key_clearing" and report.get("category") == "interrupted" and report.get("cleanup") == "verified" and "input_mode" not in report and report.get("exit") == 1):
    raise SystemExit(1)
'; then
      printf 'restore readiness local signal fault returned unexpected report: %s\n' "$auth_clear_signal_report" >&2
      emit_preflight_failure
    fi
    run_sigkill_recovery_drill --local-fixture-test --fault-test sigkill
  fi
  if ! cleanup_prepared_inputs; then emit_prepared_cleanup_failure; fi
  printf '%s\n' "$local_report"
  if [[ $local_mode == "local-fixture-test" ]]; then printf '%s\n' "$auth_clear_signal_report"; fi
  exit 0
fi

preflight_stage="run primary synthetic restore drill"
if ! success_report=$(run_drill); then
  cleanup_prepared_after_failed_drill "$success_report"
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

preflight_stage="run malformed SQL fault drill"
if malformed_report=$(run_drill --fault-test malformed-sql 2>/dev/null); then
  emit_preflight_failure
fi
preserve_prepared_on_unverified_cleanup "$malformed_report"
if ! printf '%s' "$malformed_report" | /usr/bin/python3 -c '
import json, sys
report = json.load(sys.stdin)
if not (report.get("result") == "fail" and report.get("phase") == "restore" and report.get("category") == "restore_failure" and report.get("cleanup") == "verified" and report.get("exit") == 1):
    raise SystemExit(1)
'; then
  printf 'restore readiness malformed SQL fault returned unexpected report: %s\n' "$malformed_report" >&2
  emit_preflight_failure
fi

preflight_stage="run auth key failure drill"
if auth_clear_failure_report=$(run_drill --fault-test auth-clear-failure); then
  emit_preflight_failure
fi
preserve_prepared_on_unverified_cleanup "$auth_clear_failure_report"
if ! printf '%s' "$auth_clear_failure_report" | /usr/bin/python3 -c '
import json, sys
report = json.load(sys.stdin)
if not (report.get("result") == "fail" and report.get("phase") == "auth_key_clearing" and report.get("category") == "auth_key_clear_failure" and report.get("cleanup") == "verified" and report.get("exit") == 1):
    raise SystemExit(1)
'; then
  printf 'restore readiness auth-key failure drill returned unexpected report: %s\n' "$auth_clear_failure_report" >&2
  emit_preflight_failure
fi

preflight_stage="run auth key signal drill"
if auth_clear_signal_report=$(run_drill --fault-test auth-clear-signal); then
  printf 'restore readiness signal fault unexpectedly passed: %s\n' "$auth_clear_signal_report" >&2
  emit_preflight_failure
fi
preserve_prepared_on_unverified_cleanup "$auth_clear_signal_report"
if ! printf '%s' "$auth_clear_signal_report" | /usr/bin/python3 -c '
import json, sys
report = json.load(sys.stdin)
if not (report.get("result") == "fail" and report.get("phase") == "auth_key_clearing" and report.get("category") == "interrupted" and report.get("cleanup") == "verified" and report.get("exit") == 1):
    raise SystemExit(1)
'; then
  printf 'restore readiness signal fault returned unexpected report: %s\n' "$auth_clear_signal_report" >&2
  emit_preflight_failure
fi

run_sigkill_recovery_drill --fault-test sigkill

if ! cleanup_prepared_inputs; then emit_prepared_cleanup_failure; fi
printf '%s\n' "$success_report"
