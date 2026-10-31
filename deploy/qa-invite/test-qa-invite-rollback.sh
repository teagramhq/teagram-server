#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROLLBACK=$SCRIPT_DIR/rollback.sh
TMP=$(mktemp -d)
trap 'rm -rf -- "$TMP"' EXIT

dump=$TMP/postgres.dump
events=$TMP/events
printf '%s\n' fixture > "$dump"
: > "$events"

run_fixture() {
  local running=$1 down_rc=$2 docker_rc=$3
  : > "$events"
  export RUNNING_CONTAINERS="$running" DOWN_RC="$down_rc" DOCKER_RC="$docker_rc" EVENTS="$events" DUMP="$dump" ROLLBACK="$ROLLBACK"
  # Variables intentionally expand in the fixture child Bash.
  # shellcheck disable=SC2016
  bash -Eeuo pipefail -c '
      compose() {
        printf "compose %s\n" "$*" >> "$EVENTS"
        if [[ "$1" == down && "$DOWN_RC" != 0 ]]; then
          return "$DOWN_RC"
        fi
      }
      docker() {
        printf "docker %s\n" "$*" >> "$EVENTS"
        [[ "$DOCKER_RC" == 0 ]] || return "$DOCKER_RC"
        printf "%s" "$RUNNING_CONTAINERS"
      }
      write_env() { printf "write_env %s\n" "$1" >> "$EVENTS"; }
      source "$ROLLBACK"
      rollback_failed_up "" "$DUMP"
    '
}

run_fixture running-container 1 0 2> "$TMP/running.stderr"
grep -Fq 'project containers remain running; database restore skipped' "$TMP/running.stderr" || {
  printf '%s\n' 'qa-invite rollback test: failed shutdown was not reported' >&2
  exit 1
}
if grep -Fq 'compose up -d --wait postgres' "$events"; then
  printf '%s\n' 'qa-invite rollback test: database restore ran while a project container remained running' >&2
  exit 1
fi

run_fixture '' 0 1 2> "$TMP/unverified.stderr"
grep -Fq 'could not verify that project containers stopped; database restore skipped' "$TMP/unverified.stderr" || {
  printf '%s\n' 'qa-invite rollback test: failed container verification was not reported' >&2
  exit 1
}
if grep -Fq 'compose up -d --wait postgres' "$events"; then
  printf '%s\n' 'qa-invite rollback test: database restore ran without container-state verification' >&2
  exit 1
fi

run_fixture '' 0 0 2> "$TMP/safe.stderr"
grep -Fq 'compose up -d --wait postgres' "$events" || {
  printf '%s\n' 'qa-invite rollback test: safe recovery did not restore Postgres' >&2
  exit 1
}
[[ ! -s "$TMP/safe.stderr" ]] || {
  printf '%s\n' 'qa-invite rollback test: safe database recovery reported an error' >&2
  exit 1
}

printf '%s\n' 'qa-invite rollback stops and verifies the project before database restore'
