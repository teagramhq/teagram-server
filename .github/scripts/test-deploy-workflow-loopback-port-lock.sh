#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
runner="$script_dir/run-deploy-workflow-checks.sh"
tmp_dir=$(mktemp -d)
release_file="$tmp_dir/release"
pool_pid=
trap 'cleanup' EXIT

cleanup() {
  : > "$release_file"
  [[ -z $pool_pid ]] || wait "$pool_pid" 2>/dev/null || true
  rm -rf -- "$tmp_dir"
}

wait_for_file() {
  local path=$1 attempt
  for ((attempt = 0; attempt < 500; attempt++)); do
    [[ -e $path ]] && return 0
    sleep 0.01
  done
  printf 'timed out waiting for %s\n' "$path" >&2
  return 1
}

shim_dir="$tmp_dir/bin"
mkdir -p "$shim_dir"
flock_bin=$(command -v flock)

cat > "$shim_dir/flock" <<'MOCK_FLOCK'
#!/usr/bin/bash
set -Eeuo pipefail
: "${LOCK_TEST_ATTEMPTED:?}"
: "${LOCK_TEST_ACQUIRED:?}"
: > "$LOCK_TEST_ATTEMPTED"
"$FLOCK_BIN" "$@"
: > "$LOCK_TEST_ACQUIRED"
MOCK_FLOCK
chmod 0755 "$shim_dir/flock"

cat > "$shim_dir/sudo" <<'MOCK_SUDO'
#!/usr/bin/bash
set -Eeuo pipefail
[[ ${1:-} != -n ]] || shift
exec "$@"
MOCK_SUDO
chmod 0755 "$shim_dir/sudo"

cat > "$shim_dir/bash" <<'MOCK_BASH'
#!/usr/bin/bash
set -Eeuo pipefail
case "${1:-}" in
  deploy/qa-invite/test-qa-invite-tls.sh)
    : "${LOCK_TEST_ENTERED:?}"
    : "${LOCK_TEST_RELEASE:?}"
    : > "$LOCK_TEST_ENTERED"
    while [[ ! -e $LOCK_TEST_RELEASE ]]; do
      sleep 0.01
    done
    ;;
  deploy/teagram/test-bot-api-qa-bot-ops.sh|deploy/teagram/test-bot-api-qa-optional-restore.sh)
    ;;
  deploy/teagram/test-bot-api-relays.sh)
    : "${LOCK_TEST_ENTERED:?}"
    : "${LOCK_TEST_RELEASE:?}"
    : > "$LOCK_TEST_ENTERED"
    while [[ ! -e $LOCK_TEST_RELEASE ]]; do
      sleep 0.01
    done
    ;;
  *)
    exec /usr/bin/bash "$@"
    ;;
esac
MOCK_BASH
chmod 0755 "$shim_dir/bash"

cat > "$shim_dir/xargs" <<'MOCK_XARGS'
#!/usr/bin/bash
set -Eeuo pipefail
pool_dir=${DEPLOY_CHECK_POOL_DIR:?}
runner=${LOCK_TEST_RUNNER:?}
test_dir=${LOCK_TEST_DIR:?}
worker_three_pid=
worker_five_pid=

cleanup_workers() {
  : > "$test_dir/release"
  [[ -z $worker_three_pid ]] || wait "$worker_three_pid" 2>/dev/null || true
  [[ -z $worker_five_pid ]] || wait "$worker_five_pid" 2>/dev/null || true
}
trap 'cleanup_workers' EXIT

DEPLOY_CHECK_POOL_DIR="$pool_dir" \
  LOCK_TEST_ATTEMPTED="$test_dir/worker-3-attempted" \
  LOCK_TEST_ACQUIRED="$test_dir/worker-3-acquired" \
  LOCK_TEST_ENTERED="$test_dir/worker-3-entered" \
  LOCK_TEST_RELEASE="$test_dir/release" \
  FLOCK_BIN="$FLOCK_BIN" \
  /usr/bin/bash "$runner" --worker 3 --task-index 4 &
worker_three_pid=$!

for ((attempt = 0; attempt < 500; attempt++)); do
  [[ -e $test_dir/worker-3-entered ]] && break
  sleep 0.01
done
[[ -e $test_dir/worker-3-entered ]] || exit 1

DEPLOY_CHECK_POOL_DIR="$pool_dir" \
  LOCK_TEST_ATTEMPTED="$test_dir/worker-5-attempted" \
  LOCK_TEST_ACQUIRED="$test_dir/worker-5-acquired" \
  LOCK_TEST_ENTERED="$test_dir/worker-5-entered" \
  LOCK_TEST_RELEASE="$test_dir/release" \
  FLOCK_BIN="$FLOCK_BIN" \
  /usr/bin/bash "$runner" --worker 5 --task-index 19 &
worker_five_pid=$!

wait "$worker_three_pid"
worker_three_pid=
wait "$worker_five_pid"
worker_five_pid=
for worker in 1 2 4; do
  worker_dir="$pool_dir/worker-$worker"
  mkdir -p "$worker_dir"
  printf '0\n' > "$worker_dir/status"
done
MOCK_XARGS
chmod 0755 "$shim_dir/xargs"

TMPDIR="$tmp_dir" \
  PATH="$shim_dir:$PATH" \
  LOCK_TEST_RUNNER="$runner" \
  LOCK_TEST_DIR="$tmp_dir" \
  FLOCK_BIN="$flock_bin" \
  /usr/bin/bash "$runner" > "$tmp_dir/pool.log" 2>&1 &
pool_pid=$!

wait_for_file "$tmp_dir/worker-3-entered"
wait_for_file "$tmp_dir/worker-5-attempted"
for ((attempt = 0; attempt < 100; attempt++)); do
  [[ ! -e $tmp_dir/worker-5-entered ]] || break
  sleep 0.01
done
if [[ -e $tmp_dir/worker-5-entered ]]; then
  printf 'loopback fixture lock did not exclude worker 5 while worker 3 held it\n' >&2
  exit 1
fi

: > "$release_file"
if ! wait "$pool_pid"; then
  cat "$tmp_dir/pool.log" >&2
  printf 'worker pool failed during the loopback lock test\n' >&2
  exit 1
fi
pool_pid=

[[ -e $tmp_dir/worker-5-entered ]] || {
  printf 'worker 5 did not enter the fixture after worker 3 released the lock\n' >&2
  exit 1
}

printf 'loopback fixture lock serialized workers 3 and 5 in the pool\n'
