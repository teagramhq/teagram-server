#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TMP_ROOT=${TMPDIR:-/root}
[ -w "$TMP_ROOT" ] || TMP_ROOT=/tmp
TMP=$(mktemp -d "$TMP_ROOT/rollout-container-ids.XXXXXXXX")
chmod 700 "$TMP"
trap 'rm -rf -- "$TMP"' EXIT

BIN="$TMP/bin"
CHECKOUT="$TMP/checkout"
mkdir -m 700 "$BIN" "$CHECKOUT"
cat > "$BIN/docker" <<'SH'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$MOCK_COMMANDS"

if [ "${1:-}" = compose ]; then
  shift
  case "$*" in
    'ps -q telegramd')
      [ "${MOCK_REFERENCE_EMPTY:-0}" = 1 ] || printf '%s\n' "$MOCK_REFERENCE_ID"
      ;;
    'ps -aq telegramd')
      [ "${MOCK_REFERENCE_EMPTY:-0}" = 1 ] || printf '%s\n' "$MOCK_REFERENCE_ID"
      ;;
    'config --format json') printf '%s\n' '{"name":"fixture"}' ;;
    *) exit 90 ;;
  esac
  exit 0
fi

if [ "${1:-}" = ps ]; then
  case "$MOCK_ENUMERATION_RESULT" in
    failure) exit 23 ;;
    empty) exit 0 ;;
    invalid) printf '%s\n' "$MOCK_INVALID_ID"; exit 0 ;;
    mismatch) printf '%s\n' "$MOCK_OTHER_ID"; exit 0 ;;
    short)
      if [[ " $* " == *' --no-trunc '* ]]; then
        printf '%s\n' "$MOCK_REFERENCE_ID"
      else
        printf '%s\n' "${MOCK_REFERENCE_ID:0:12}"
      fi
      exit 0
      ;;
    *) exit 91 ;;
  esac
fi

if [ "${1:-}" = inspect ]; then
  shift
  if [ "$#" -eq 1 ]; then
    printf '{"Id":"%s","Config":{"Labels":{"com.docker.compose.project":"fixture"}}}\n' "$MOCK_REFERENCE_ID"
    exit 0
  fi
  printf '['
  separator=''
  for id in "$@"; do
    printf '%s{"Id":"%s"}' "$separator" "$id"
    separator=','
  done
  printf ']\n'
  exit 0
fi

exit 92
SH
chmod 700 "$BIN/docker"

CAPTURE_HELPER="$TMP/capture-inventory.py"
cat > "$CAPTURE_HELPER" <<'PY'
import sys

sys.stdout.write(sys.stdin.read())
PY
chmod 600 "$CAPTURE_HELPER"

export PATH="$BIN:$PATH"
export ROLLOUT_RUNNER_SOURCE_ONLY=1
export ROLLOUT_RUNNER_CHECKOUT="$CHECKOUT"
source "$SCRIPT_DIR/rollout-runner.sh"
MODE_HELPER="$CAPTURE_HELPER"
if [ "$(id -u)" != 0 ]; then
  write_immutable() {
    local path=$1 contents=$2
    [ ! -e "$path" ] && [ ! -L "$path" ] || return 1
    printf '%s\n' "$contents" > "$path"
    chmod 600 -- "$path"
  }
fi

REFERENCE_ID=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
OTHER_ID=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
INVALID_ID=not-a-container-id
export MOCK_REFERENCE_ID="$REFERENCE_ID"
export MOCK_OTHER_ID="$OTHER_ID"
export MOCK_INVALID_ID="$INVALID_ID"

run_case() {
  local name=$1 allow_empty=$2 result=$3 expected_status=$4 error=${5:-}
  local case_dir="$TMP/$name" output expected_command status
  mkdir -m 700 "$case_dir"
  : > "$case_dir/commands"
  chmod 600 "$case_dir/commands"
  output="$case_dir/inventory.json"
  export MOCK_COMMANDS="$case_dir/commands"
  export MOCK_ENUMERATION_RESULT="$result"
  export MOCK_REFERENCE_EMPTY=0
  if [ "$name" = rollback-empty-inventory ] || [ "$name" = running-reference-missing ]; then
    export MOCK_REFERENCE_EMPTY=1
  fi

  if capture_running_blob_inventory "$output" "$allow_empty" >"$case_dir/stdout" 2>"$case_dir/stderr"; then
    status=0
  else
    status=$?
  fi

  if [ "$status" != "$expected_status" ]; then
    printf 'FAIL %s: expected status %s, got %s\n' "$name" "$expected_status" "$status" >&2
    cat "$case_dir/stderr" >&2
    exit 1
  fi
  if [ -n "$error" ] && ! grep -Fq "$error" "$case_dir/stderr"; then
    printf 'FAIL %s: expected error %s\n' "$name" "$error" >&2
    cat "$case_dir/stderr" >&2
    exit 1
  fi

  if [ "$allow_empty" = 1 ]; then
    expected_command='ps --no-trunc -aq --filter label=com.docker.compose.project=fixture'
  else
    expected_command='ps --no-trunc -q --filter label=com.docker.compose.project=fixture'
  fi
  if [ "$name" = running-reference-missing ]; then
    if grep -Fq 'ps --no-trunc' "$case_dir/commands"; then
      printf 'FAIL %s: enumeration ran without a full reference container ID\n' "$name" >&2
      cat "$case_dir/commands" >&2
      exit 1
    fi
  elif ! grep -Fxq "$expected_command" "$case_dir/commands"; then
    printf 'FAIL %s: full-ID project-filtered enumeration command was not requested\n' "$name" >&2
    cat "$case_dir/commands" >&2
    exit 1
  fi

  if [ "$name" = rollback-empty-inventory ]; then
    [ "$(cat "$output")" = '[]' ] || {
      printf 'FAIL %s: empty rollback inventory was not retained\n' "$name" >&2
      exit 1
    }
  elif [ "$expected_status" = 0 ]; then
    grep -Fq "$REFERENCE_ID" "$output" || {
      printf 'FAIL %s: full enumerated identity was not captured\n' "$name" >&2
      cat "$output" >&2
      exit 1
    }
  else
    [ ! -e "$output" ] || {
      printf 'FAIL %s: rejected inventory was published\n' "$name" >&2
      exit 1
    }
  fi

  printf 'PASS %s\n' "$name"
}

run_case running-full-id 0 short 0
run_case rollback-full-id 1 short 0
run_case running-command-failure 0 failure 1 'cannot enumerate containers in the telegramd project'
run_case rollback-command-failure 1 failure 1 'cannot enumerate containers in the telegramd project'
run_case running-empty-inventory 0 empty 1 'no running containers were found in the telegramd project'
run_case running-reference-missing 0 short 1 'running telegramd container ID is unavailable'
run_case rollback-empty-inventory 1 empty 0
run_case running-invalid-id 0 invalid 1 'project container enumeration returned an invalid container ID'
run_case rollback-invalid-id 1 invalid 1 'project container enumeration returned an invalid container ID'
run_case running-reference-mismatch 0 mismatch 1 'running telegramd container is absent from project enumeration'
run_case rollback-reference-mismatch 1 mismatch 1 'running telegramd container is absent from project enumeration'
