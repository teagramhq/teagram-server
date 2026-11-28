#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tmp_dir=$(mktemp -d)
trap 'rm -rf -- "$tmp_dir"' EXIT

shim_dir="$tmp_dir/bin"
pool_dir="$tmp_dir/pool"
mkdir -p "$shim_dir" "$pool_dir"

cat > "$shim_dir/bash" <<'MOCK_BASH'
#!/usr/bin/bash
set -Eeuo pipefail
if [[ ${SIMULATE_WORKER_HANG:-0} == 1 &&
  ${1:-} == */run-deploy-workflow-checks.sh &&
  ${2:-} == --task &&
  ${3:-} == 11 ]]; then
  exec /usr/bin/sleep 30
fi
if [[ ${1:-} == -n && ${2:-} == .github/scripts/perf-run.sh ]]; then
  printf 'injected bash syntax failure\n' >&2
  exit 42
fi
exec /usr/bin/bash "$@"
MOCK_BASH
chmod 0755 "$shim_dir/bash"

for tool in shellcheck actionlint node; do
  cat > "$shim_dir/$tool" <<'MOCK_SUCCESS'
#!/usr/bin/bash
exit 0
MOCK_SUCCESS
  chmod 0755 "$shim_dir/$tool"
done

if DEPLOY_CHECK_POOL_DIR="$pool_dir" PATH="$shim_dir:$PATH" \
  /usr/bin/bash "$script_dir/run-deploy-workflow-checks.sh" --worker 4 --task-index 11; then
  printf 'worker reported success after an injected early command failure\n' >&2
  exit 1
fi

worker_log="$pool_dir/worker-4/output.log"
task_log="$pool_dir/worker-4/11.log"
grep -Fq 'injected bash syntax failure' "$task_log" || {
  printf 'worker task did not reach the injected syntax failure\n' >&2
  exit 1
}
grep -Fq 'FAILED: Check workflow contract script syntax' "$worker_log" || {
  printf 'worker did not report the failed check by name\n' >&2
  exit 1
}

printf 'worker failed the named check after its first command failed\n'

cat > "$shim_dir/xargs" <<'MOCK_XARGS_HANG'
#!/usr/bin/bash
set -Eeuo pipefail
while (($#)); do
  case "$1" in
    -r) shift ;;
    -n|-P) shift 2 ;;
    *) break ;;
  esac
done
cat >/dev/null
exec "$@" 4 --task-index 11
MOCK_XARGS_HANG
chmod 0755 "$shim_dir/xargs"

hang_output="$tmp_dir/pool-hang-output.log"
if TMPDIR="$tmp_dir" PATH="$shim_dir:$PATH" SIMULATE_WORKER_HANG=1 \
  timeout --signal=TERM --kill-after=1s 1s \
    /usr/bin/bash "$script_dir/run-deploy-workflow-checks.sh" > "$hang_output" 2>&1; then
  printf 'worker completed the injected hanging check before timeout\n' >&2
  exit 1
else
  hang_status=$?
fi
[[ $hang_status == 124 ]] || {
  printf 'worker hang test exited with unexpected status %s\n' "$hang_status" >&2
  exit 1
}
grep -Fq 'worker 4: start Check workflow contract script syntax' "$hang_output" || {
  printf 'pool output did not name the check before it hung\n' >&2
  exit 1
}

cat > "$shim_dir/xargs" <<'MOCK_XARGS'
#!/usr/bin/bash
set -Eeuo pipefail
: "${DEPLOY_CHECK_POOL_DIR:?}"
for worker in 1 2 3 4 5; do
  worker_dir="$DEPLOY_CHECK_POOL_DIR/worker-$worker"
  mkdir -p "$worker_dir"
  if [[ $worker == 1 ]]; then
    printf '1\n' > "$worker_dir/status"
  else
    printf '0\n' > "$worker_dir/status"
  fi
done
MOCK_XARGS
chmod 0755 "$shim_dir/xargs"

pool_output="$tmp_dir/pool-output.log"
if TMPDIR="$tmp_dir" PATH="$shim_dir:$PATH" \
  /usr/bin/bash "$script_dir/run-deploy-workflow-checks.sh" > "$pool_output" 2>&1; then
  printf 'worker pool reported success after a worker status failed\n' >&2
  exit 1
fi

grep -Fq 'FAILED:' "$pool_output" || {
  printf 'worker pool exited non-zero without reporting the failed worker\n' >&2
  exit 1
}

printf 'worker pool reported a failed worker status\n'
