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
