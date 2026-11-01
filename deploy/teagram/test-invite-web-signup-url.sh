#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

[[ "$(id -u)" == 0 ]] || {
  printf '%s\n' 'run invite signup URL fixtures as root' >&2
  exit 1
}

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SOURCE_SCRIPT="$SCRIPT_DIR/ops/invite-web-signup-url.sh"
REAL_MV=$(command -v mv)
REAL_SYNC=$(command -v sync)
SOURCE_SHA=0123456789abcdef0123456789abcdef01234567
FIXTURE_GROUP=65534
TMP_PREFIX="${TMPDIR:-/tmp}/invite-web-signup-url-test."
TMP=$(mktemp -d "${TMP_PREFIX}"XXXXXXXX)
chmod 700 "$TMP"

fail() {
  printf 'invite-web-signup-url fixture: %s\n' "$1" >&2
  exit 1
}

cleanup() {
  local status=$?
  trap - EXIT
  case "$TMP" in
    "$TMP_PREFIX"????????)
      [[ ! -L "$TMP" && "$(stat -c '%u:%a' -- "$TMP" 2>/dev/null)" == 0:700 ]] || {
        printf '%s\n' 'fixture cleanup refused an unsafe temporary directory' >&2
        exit 1
      }
      rm -rf -- "$TMP" || status=1
      ;;
    *)
      printf '%s\n' 'fixture cleanup refused an unexpected temporary path' >&2
      status=1
      ;;
  esac
  exit "$status"
}
trap cleanup EXIT

[[ "$(stat -c '%u:%a' -- "$TMP")" == 0:700 ]] || fail 'temporary directory is not root-owned and private'
[[ -f "$SOURCE_SCRIPT" && ! -L "$SOURCE_SCRIPT" ]] || fail 'production script is not a regular file'

FIXTURE_ROOT=
FIXTURE_SCRIPT=
FIXTURE_BIN="$TMP/no-wrappers"
mkdir -m 700 "$FIXTURE_BIN"

make_fixture() {
  local name=$1 env_body=$2 env_mode=${3:-640} env_group=${4:-$FIXTURE_GROUP}
  local record_sha=${5:-} case_dir
  case_dir="$TMP/$name"
  FIXTURE_ROOT="$case_dir/root"
  FIXTURE_SCRIPT="$case_dir/invite-web-signup-url.sh"
  mkdir -p "$FIXTURE_ROOT/.state"
  printf '%s' "$env_body" > "$FIXTURE_ROOT/.env"
  chgrp "$env_group" "$FIXTURE_ROOT/.env"
  chmod "$env_mode" "$FIXTURE_ROOT/.env"
  if [[ -z "$record_sha" ]]; then
    record_sha=$(sha256sum -- "$FIXTURE_ROOT/.env")
    record_sha=${record_sha%% *}
  fi
  printf 'env_sha256=%s source_sha=%s\n' "$record_sha" "$SOURCE_SHA" > "$FIXTURE_ROOT/.state/rollout-env"
  chmod 600 "$FIXTURE_ROOT/.state/rollout-env"
  cp -- "$FIXTURE_ROOT/.env" "$case_dir/env.expected"
  cp -- "$FIXTURE_ROOT/.state/rollout-env" "$case_dir/record.expected"
  sed \
    -e "s|^readonly ROOT=/opt/teagram$|readonly ROOT=$FIXTURE_ROOT|" \
    -e "s|/tmp/teagram-deploy.lock|$case_dir/deploy.lock|" \
    "$SOURCE_SCRIPT" > "$FIXTURE_SCRIPT"
  if grep -Eq '^readonly ROOT=/opt/teagram$|/tmp/teagram-deploy\.lock' "$FIXTURE_SCRIPT"; then
    fail "$name fixture copy still contains a production path"
  fi
  chmod 700 "$FIXTURE_SCRIPT"
}

run_fixture() {
  PATH="$FIXTURE_BIN:$PATH" "$FIXTURE_SCRIPT" "$@"
}

assert_digest_record() {
  local env_sha record
  env_sha=$(sha256sum -- "$FIXTURE_ROOT/.env")
  env_sha=${env_sha%% *}
  record=$(cat -- "$FIXTURE_ROOT/.state/rollout-env")
  [[ "$record" == "env_sha256=$env_sha source_sha=$SOURCE_SHA" ]] || \
    fail 'rollout record does not match the .env digest and source SHA'
  [[ "$(stat -c '%u:%a' -- "$FIXTURE_ROOT/.state/rollout-env")" == 0:600 ]] || \
    fail 'rollout record lost its root-owned mode 0600 metadata'
}

assert_env_metadata() {
  local expected_mode=$1 expected_group=$2 actual
  actual=$(stat -c '%u:%a:%g' -- "$FIXTURE_ROOT/.env")
  [[ "$actual" == "0:$expected_mode:$expected_group" ]] || \
    fail ".env metadata changed: expected 0:$expected_mode:$expected_group, got $actual"
}

assert_no_recovery_directory() {
  local recovery_dir
  recovery_dir=$(find "$FIXTURE_ROOT/.state" -mindepth 1 -maxdepth 1 -type d \
    -name '.invite-web-signup-url.*' -print -quit)
  [[ -z "$recovery_dir" ]] || fail "operation left recovery files under $recovery_dir"
}

expect_guard_refusal() {
  local name=$1 body=$2 record_sha=${3:-} status=0
  make_fixture "$name" "$body" 640 "$FIXTURE_GROUP" "$record_sha"
  run_fixture --check > "$TMP/$name/output" 2> "$TMP/$name/error" || status=$?
  [[ "$status" != 0 ]] || fail "$name guard unexpectedly succeeded"
  cmp -s -- "$FIXTURE_ROOT/.env" "$TMP/$name/env.expected" || fail "$name guard changed .env"
  cmp -s -- "$FIXTURE_ROOT/.state/rollout-env" "$TMP/$name/record.expected" || \
    fail "$name guard changed the rollout record"
}

base_env=$'TG_REPLICA_COUNT=1\nTG_PUBLIC_URL=https://api.teagram.io\n'
target_env=$'TG_REPLICA_COUNT=1\nTG_PUBLIC_URL=https://api.teagram.io\nTG_INVITE_WEB_SIGNUP_URL=https://web.teagram.io/\n'
make_fixture apply "$base_env"
before_env=$(sha256sum -- "$FIXTURE_ROOT/.env")
before_env=${before_env%% *}
output=$(run_fixture --check)
[[ "$output" == 'preflight=would_apply key=TG_INVITE_WEB_SIGNUP_URL value=https://web.teagram.io/ desktop=unset' ]] || \
  fail 'preflight did not report the expected apply action'
[[ "$(sha256sum -- "$FIXTURE_ROOT/.env" | cut -d' ' -f1)" == "$before_env" ]] || \
  fail 'preflight changed .env'
output=$(run_fixture)
[[ "$output" == 'env_update=added key=TG_INVITE_WEB_SIGNUP_URL value=https://web.teagram.io/ desktop=unset' ]] || \
  fail 'apply did not report the expected update'
printf '%s' "$target_env" > "$TMP/apply/env.expected"
cmp -s -- "$FIXTURE_ROOT/.env" "$TMP/apply/env.expected" || fail 'apply changed unrelated .env entries'
assert_env_metadata 640 "$FIXTURE_GROUP"
assert_digest_record
after_apply=$(sha256sum -- "$FIXTURE_ROOT/.env")
after_apply=${after_apply%% *}
output=$(run_fixture --check)
[[ "$output" == 'preflight=already_applied key=TG_INVITE_WEB_SIGNUP_URL value=https://web.teagram.io/ desktop=unset' ]] || \
  fail 'preflight did not recognize the applied target'
output=$(run_fixture)
[[ "$output" == 'env_update=already_applied key=TG_INVITE_WEB_SIGNUP_URL value=https://web.teagram.io/ desktop=unset' ]] || \
  fail 'repeat apply was not idempotent'
[[ "$(sha256sum -- "$FIXTURE_ROOT/.env" | cut -d' ' -f1)" == "$after_apply" ]] || \
  fail 'repeat apply changed .env'
assert_digest_record
assert_no_recovery_directory

make_fixture rollback "$target_env"
printf '%s' "$base_env" > "$TMP/rollback/env.expected"
output=$(run_fixture --rollback)
[[ "$output" == 'rollback=removed key=TG_INVITE_WEB_SIGNUP_URL desktop=unset' ]] || \
  fail 'rollback did not report the expected removal'
cmp -s -- "$FIXTURE_ROOT/.env" "$TMP/rollback/env.expected" || fail 'rollback changed unrelated .env entries'
assert_env_metadata 640 "$FIXTURE_GROUP"
assert_digest_record
assert_no_recovery_directory

expect_guard_refusal different-target \
  $'TG_REPLICA_COUNT=1\nTG_INVITE_WEB_SIGNUP_URL=https://wrong.example/\n'
expect_guard_refusal duplicate-target \
  $'TG_REPLICA_COUNT=1\nTG_INVITE_WEB_SIGNUP_URL=https://web.teagram.io/\nTG_INVITE_WEB_SIGNUP_URL=https://web.teagram.io/\n'
expect_guard_refusal desktop-target \
  $'TG_REPLICA_COUNT=1\nTG_INVITE_DESKTOP_URL=https://desktop.example/\n'
expect_guard_refusal stale-record "$base_env" \
  0000000000000000000000000000000000000000000000000000000000000000

install_record_mv_failure() {
  FIXTURE_BIN="$TMP/fail-record-mv-bin"
  mkdir -m 700 "$FIXTURE_BIN"
  cat > "$FIXTURE_BIN/mv" <<'MV'
#!/usr/bin/env bash
set -euo pipefail
args=("$@")
count=${#args[@]}
if (( count >= 2 )); then
  source=${args[count - 2]}
  destination=${args[count - 1]}
  if [[ "$source" == */record.new && "$destination" == "$FIXTURE_ROOT/.state/rollout-env" ]]; then
    exit 91
  fi
fi
exec "$REAL_MV" "$@"
MV
  chmod 700 "$FIXTURE_BIN/mv"
}

install_second_sync_failure() {
  FIXTURE_BIN="$TMP/fail-second-sync-bin"
  FIXTURE_SYNC_COUNT_FILE="$TMP/sync-count"
  mkdir -m 700 "$FIXTURE_BIN"
  cat > "$FIXTURE_BIN/sync" <<'SYNC'
#!/usr/bin/env bash
set -euo pipefail
count=0
if [[ -f "$FIXTURE_SYNC_COUNT_FILE" ]]; then
  read -r count < "$FIXTURE_SYNC_COUNT_FILE"
fi
count=$((count + 1))
printf '%s\n' "$count" > "$FIXTURE_SYNC_COUNT_FILE"
if [[ "$count" == 2 ]]; then
  exit 92
fi
exec "$REAL_SYNC" "$@"
SYNC
  chmod 700 "$FIXTURE_BIN/sync"
}

run_expected_failure() {
  local name=$1 status=0
  REAL_MV="$REAL_MV" REAL_SYNC="$REAL_SYNC" \
    FIXTURE_ROOT="$FIXTURE_ROOT" FIXTURE_SYNC_COUNT_FILE="${FIXTURE_SYNC_COUNT_FILE:-$TMP/unused-sync-count}" \
    PATH="$FIXTURE_BIN:$PATH" "$FIXTURE_SCRIPT" > "$TMP/$name/output" 2> "$TMP/$name/error" || status=$?
  [[ "$status" != 0 ]] || fail "$name injected failure unexpectedly succeeded"
  cmp -s -- "$FIXTURE_ROOT/.env" "$TMP/$name/env.expected" || \
    fail "$name did not restore the original .env"
  cmp -s -- "$FIXTURE_ROOT/.state/rollout-env" "$TMP/$name/record.expected" || \
    fail "$name did not restore the original rollout record"
  assert_env_metadata 640 "$FIXTURE_GROUP"
  assert_digest_record
  assert_no_recovery_directory
}

make_fixture partial-record-install "$base_env"
install_record_mv_failure
run_expected_failure partial-record-install

make_fixture partial-post-sync "$base_env"
install_second_sync_failure
run_expected_failure partial-post-sync

printf '%s\n' 'invite signup URL apply, guard, rollback, digest, and recovery fixtures pass'
