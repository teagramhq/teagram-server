#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
OPS_SCRIPT="$SCRIPT_DIR/main2612-live-compose-replay.sh"
TMP=$(mktemp -d /tmp/main2612-replay-safety.XXXXXXXX)
CLEANUP_FIXTURE=
SUCCESS_FIXTURE=
SUCCESS_TOKEN=0123456789abcdef0123456789abcdef

cleanup_test() {
  local status=$?
  trap - EXIT
  if [ -n "$CLEANUP_FIXTURE" ]; then
    [ ! -d "$CLEANUP_FIXTURE/live" ] || rmdir -- "$CLEANUP_FIXTURE/live" || status=1
    [ ! -d "$CLEANUP_FIXTURE" ] || rmdir -- "$CLEANUP_FIXTURE" || status=1
  fi
  if [ -n "$SUCCESS_FIXTURE" ] && [ -d "$SUCCESS_FIXTURE" ]; then
    marker="$SUCCESS_FIXTURE/.rollout-owned-$SUCCESS_TOKEN"
    [ ! -f "$marker" ] || unlink -- "$marker" || status=1
    rmdir -- "$SUCCESS_FIXTURE" || status=1
  fi
  rmdir -- "$TMP" || status=1
  exit "$status"
}
trap cleanup_test EXIT

bash -n "$OPS_SCRIPT"
grep -Fq 'umask 077' "$OPS_SCRIPT"
grep -Fq 'TMP_TOKEN=$(mark_replay_tree "$TMP")' "$OPS_SCRIPT"
grep -Fq 'chmod 600 -- "$tree/$env_name"' "$OPS_SCRIPT"
grep -Fq 'unlink -- "$env_path"' "$OPS_SCRIPT"
grep -Fq 'RESULT_ROWS=$(<"$TMP/logs/field-results.tsv")' "$OPS_SCRIPT"
if grep -Fq 'cat -- "$TMP/logs/field-results.tsv"' "$OPS_SCRIPT"; then
  printf '%s\n' 'FAIL live replay prints result rows before guarded cleanup' >&2
  exit 1
fi

if grep -Eq '(^|[[:space:]])(/bin/)?rm[[:space:]]+-rf([[:space:]]|$)' "$OPS_SCRIPT"; then
  printf '%s\n' 'FAIL live replay bypasses guarded tree cleanup' >&2
  exit 1
fi
if grep -Eq 'source[[:space:]].*rollout-(runner|verifier)\.sh' "$OPS_SCRIPT"; then
  printf '%s\n' 'FAIL live replay sources candidate runner or verifier code' >&2
  exit 1
fi
grep -Fq 'git -C "$TMP/candidate.git" fetch --quiet --no-tags --depth=1 "$CANDIDATE_REMOTE" "$CANDIDATE_SHA"' "$OPS_SCRIPT"
grep -Fq 'rollout_safe_remove_tree "$target" "$TMP" "$TMP_TOKEN" "$LIVE_CHECKOUT"' "$OPS_SCRIPT"
grep -Fq 'bash "$VERIFIER" snapshot' "$OPS_SCRIPT"
grep -Fq 'bash "$RUNNER" compare-preflight' "$OPS_SCRIPT"
grep -Fq 'com.docker.compose.project.config_files' "$OPS_SCRIPT"
if grep -Fq 'git -C "$LIVE_CHECKOUT" fetch' "$OPS_SCRIPT"; then
  printf '%s\n' 'FAIL candidate fetch writes into the live checkout' >&2
  exit 1
fi

selected_sha=1111111111111111111111111111111111111111
candidate_sha=2222222222222222222222222222222222222222
resolved=$(OPS_SHA="$selected_sha" bash -c '
  set -Eeuo pipefail
  source "$1"
  resolve_candidate_sha "$2"
  [ "$CANDIDATE_SHA" = "$2" ]
  resolve_candidate_sha
  [ "$CANDIDATE_SHA" = "$OPS_SHA" ]
  if resolve_candidate_sha invalid; then exit 20; fi
  if resolve_candidate_sha too many; then exit 21; fi
  printf "%s\n" ok
' _ "$OPS_SCRIPT" "$candidate_sha")
[ "$resolved" = ok ]
printf '%s\n' 'PASS candidate SHA argument overrides the selected main SHA; the selected SHA is the no-argument default'

CLEANUP_FIXTURE=$(mktemp -d /tmp/main2612-replay.XXXXXXXX)
mkdir -m 700 "$CLEANUP_FIXTURE/live"
cleanup_status=0
if cleanup_output=$(bash -c '
  set -Eeuo pipefail
  source "$1"
  TMP=$2
  TMP_TOKEN=0123456789abcdef0123456789abcdef
  RESULT_ROWS=$3
  realpath() { printf "%s\n" "$3"; }
  stat() {
    case "$2" in
      %u) printf "0\n" ;;
      %a) printf "700\n" ;;
      *) return 2 ;;
    esac
  }
  rollout_safe_remove_tree() {
    printf "synthetic ownership marker mismatch\n" >&2
    return 23
  }
  cleanup
' _ "$OPS_SCRIPT" "$CLEANUP_FIXTURE" $'field\tresult\ncompose\tpass' 2>&1); then
  printf '%s\n' 'FAIL live replay cleanup fixture accepted a guarded-delete failure' >&2
  exit 1
else
  cleanup_status=$?
fi
[ "$cleanup_status" -eq 1 ]
expected_cleanup_error="live Compose replay cleanup failed for $CLEANUP_FIXTURE/live (exit 23): synthetic ownership marker mismatch"
[ "$cleanup_output" = "$expected_cleanup_error" ] || {
  printf 'FAIL unexpected live replay cleanup diagnostic: %s\n' "$cleanup_output" >&2
  exit 1
}
rmdir -- "$CLEANUP_FIXTURE/live" "$CLEANUP_FIXTURE"
CLEANUP_FIXTURE=
printf '%s\n' 'PASS replay withholds passing rows and reports a guarded-delete failure'

SUCCESS_FIXTURE=$(mktemp -d /tmp/main2612-replay.XXXXXXXX)
printf '%s\n' "$SUCCESS_TOKEN" > "$SUCCESS_FIXTURE/.rollout-owned-$SUCCESS_TOKEN"
chmod 600 "$SUCCESS_FIXTURE/.rollout-owned-$SUCCESS_TOKEN"
success_rows=$(bash -c '
  set -Eeuo pipefail
  source "$1"
  TMP=$2
  TMP_TOKEN=$3
  RESULT_ROWS=$4
  realpath() { printf "%s\n" "$3"; }
  stat() {
    case "$2" in
      %u) printf "0\n" ;;
      %a)
        if [ "$4" = "$TMP" ]; then printf "700\n"; else printf "600\n"; fi
        ;;
      *) return 2 ;;
    esac
  }
  cleanup
' _ "$OPS_SCRIPT" "$SUCCESS_FIXTURE" "$SUCCESS_TOKEN" $'field\tresult\ncompose\tpass')
[ "$success_rows" = $'field\tresult\ncompose\tpass' ]
[ ! -e "$SUCCESS_FIXTURE" ]
SUCCESS_FIXTURE=
printf '%s\n' 'PASS replay prints field/result rows after successful cleanup'
