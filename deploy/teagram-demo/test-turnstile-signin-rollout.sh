#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
ROLLBACK_HELPER=$SCRIPT_DIR/ops/turnstile-signin-rollback.sh
TMP_DIR=$(mktemp -d)
trap 'rm -rf -- "$TMP_DIR"' EXIT

for signal in HUP INT TERM; do
  case_dir=$TMP_DIR/$signal
  mkdir -m 0700 "$case_dir"
  env_file=$case_dir/.env
  backup_file=$case_dir/env.before-turnstile
  active_file=$case_dir/active-flag
  rollback_log=$case_dir/rollouts
  original_env=$'POSTGRES_PASSWORD=fixture\nTG_TURNSTILE_SIGNIN_ENABLED=false\n'
  target_env=$'POSTGRES_PASSWORD=fixture\nTG_TURNSTILE_SIGNIN_ENABLED=true\n'
  printf '%s' "$original_env" > "$backup_file"
  printf '%s' "$target_env" > "$env_file"
  printf '%s\n' true > "$active_file"
  chmod 0600 "$backup_file" "$env_file"

  set +e
  bash -c '
    source "$1"
    ENV_FILE=$2
    ENV_BACKUP=$3
    expected_flag=false
    ROLLOUT_COMMITTED=0
    ROLLBACK_CONFIRMED=0
    TMP_DIR=
    DEMO_IMAGE_REPOSITORY=fixture/demo
    OPS_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    ACTIVE_FILE=$4
    ROLLBACK_LOG=$5
    active_flag() { cat "$ACTIVE_FILE"; }
    demo_active_image_ref() { printf "%s@sha256:%064d\n" "$DEMO_IMAGE_REPOSITORY" 0; }
    run_rollout() {
      local digest=$1
      sed -n "s/^TG_TURNSTILE_SIGNIN_ENABLED=//p" "$ENV_FILE" > "$ACTIVE_FILE"
      printf "%s\n" "$digest" >> "$ROLLBACK_LOG"
    }
    install_turnstile_traps
    kill -s "$6" "$BASHPID"
  ' _ "$ROLLBACK_HELPER" "$env_file" "$backup_file" "$active_file" "$rollback_log" "$signal" \
    >"$case_dir/stdout" 2>"$case_dir/stderr"
  status=$?
  set -e

  case "$signal:$status" in
    HUP:129|INT:130|TERM:143) ;;
    *) printf 'unexpected status for %s: %s\n' "$signal" "$status" >&2; exit 1 ;;
  esac
  cmp -s "$backup_file" "$env_file" || {
    printf 'environment file was not restored after %s\n' "$signal" >&2
    exit 1
  }
  [[ "$(cat "$active_file")" == false ]] || {
    printf 'active container was not reconciled after %s\n' "$signal" >&2
    exit 1
  }
  [[ "$(wc -l < "$rollback_log")" -eq 1 ]] || {
    printf 'runtime rollback was not executed once after %s\n' "$signal" >&2
    exit 1
  }
  expected_rollback_digest=$(printf 'sha256:%064d' 0)
  [[ "$(cat "$rollback_log")" == "$expected_rollback_digest" ]] || {
    printf 'runtime rollback used the wrong image digest after %s\n' "$signal" >&2
    exit 1
  }
  grep -Fq 'rollback=confirmed effective_flag=false' "$case_dir/stderr" || {
    printf 'rollback confirmation was missing after %s\n' "$signal" >&2
    exit 1
  }
done

trap - EXIT
rm -rf -- "$TMP_DIR"
