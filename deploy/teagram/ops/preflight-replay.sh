#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

BASE_RUNNER=
BASE_VERIFIER=
LOCK_PATH=
LIVE_SECRETS=
MOUNTED_INPUTS=()
readonly TEMP_PREFIX=/tmp/teagram-preflight-replay.
readonly SANDBOX_UID=65534
readonly SANDBOX_PATH=/bin:/usr/bin
readonly TRUSTED_BIN_PATH=/usr/sbin:/usr/bin:/sbin:/bin

fail() {
  printf 'preflight replay rejected: %s\n' "$1" >&2
  return 1
}

unset ROLLOUT_PREFLIGHT_REPLAY ROLLOUT_PREFLIGHT_LIVE_SECRETS_DIR

usage() {
  printf '%s\n' 'usage: preflight-replay.sh <40-hex-pr-head-sha>' >&2
}

if [ "${PREFLIGHT_REPLAY_TEST_MODE:-0}" = 1 ]; then
  [ -z "${OPS_SHA:-}" ] || { fail 'test overrides are unavailable from the forced command'; exit 64; }
  EXPECTED_OWNER=$(id -u) || { fail 'cannot inspect fixture owner'; exit 1; }
  export ROLLOUT_PREFLIGHT_TEST_MODE=1
  LIVE_ROOT=${PREFLIGHT_REPLAY_TEST_LIVE_CHECKOUT:-}
  REPOSITORY_URL=${PREFLIGHT_REPLAY_TEST_REPOSITORY_URL:-}
  [[ "$LIVE_ROOT" == /tmp/* && "$REPOSITORY_URL" == /* ]] || {
    fail 'fixture paths are invalid'
    exit 64
  }
else
  [[ "${OPS_SHA:-}" =~ ^[0-9a-f]{40}$ ]] || { fail 'forced command SHA is invalid'; exit 64; }
  [ "$(id -u)" = 0 ] || { fail 'ops command requires uid 0'; exit 1; }
  EXPECTED_OWNER=0
  unset ROLLOUT_PREFLIGHT_TEST_MODE
  LIVE_ROOT=/opt/teagram
  REPOSITORY_URL=https://github.com/teagramhq/teagram-server.git
  LOCK_PATH=/tmp/teagram-deploy.lock
fi

if [ "$LIVE_ROOT" != /opt/teagram ]; then
  LIVE_ROOT=$(readlink -f -- "$LIVE_ROOT") || { fail 'live checkout is unavailable'; exit 1; }
fi
if [ "${PREFLIGHT_REPLAY_TEST_MODE:-0}" = 1 ]; then
  fixture_root=${LIVE_ROOT%/live}
  [ "$fixture_root" != "$LIVE_ROOT" ] || { fail 'fixture live checkout path is invalid'; exit 64; }
  LOCK_PATH="$fixture_root/deploy.lock"
fi
[ -d "$LIVE_ROOT" ] && [ ! -L "$LIVE_ROOT" ] || { fail 'live checkout is unavailable'; exit 1; }
[ "$(git -C "$LIVE_ROOT" rev-parse --is-inside-work-tree 2>/dev/null)" = true ] || {
  fail 'live checkout is not a Git worktree'
  exit 1
}
if [ "${PREFLIGHT_REPLAY_TEST_MODE:-0}" = 1 ]; then
  runtime_sha=$(git -C "$LIVE_ROOT" rev-parse --verify 'HEAD^{commit}' 2>/dev/null) || {
    fail 'cannot resolve the fixture rollout runtime revision'
    exit 1
  }
else
  runtime_sha=$OPS_SHA
fi
if ! [[ "$runtime_sha" =~ ^[0-9a-f]{40}$ ]] || \
   ! git -C "$LIVE_ROOT" cat-file -e "$runtime_sha^{commit}" 2>/dev/null; then
  fail 'reviewed rollout runtime commit is unavailable'
  exit 1
fi

target_sha=${1:-}
[ "$#" -eq 1 ] || { usage; exit 64; }
[[ "$target_sha" =~ ^[0-9a-f]{40}$ ]] || { fail 'PR head must be a full 40-hex commit SHA'; exit 64; }

if [ -L "$LOCK_PATH" ] || { [ -e "$LOCK_PATH" ] && [ ! -f "$LOCK_PATH" ]; }; then
  fail 'deploy lock path is unsafe'
  exit 1
fi
exec 9>"$LOCK_PATH" || { fail 'cannot open the deploy lock'; exit 1; }
[ -f "$LOCK_PATH" ] && [ ! -L "$LOCK_PATH" ] && \
  [ "$(stat -c %u -- "$LOCK_PATH")" = "$EXPECTED_OWNER" ] || {
  fail 'deploy lock owner or type is invalid'
  exit 1
}
flock -n 9 || { fail 'deploy is running; preflight replay refused'; exit 1; }

TMP_DIR=
cleanup() {
  local rc=$? suffix mounted_path can_remove=1
  trap - EXIT
  if [ -n "$TMP_DIR" ]; then
    case "$TMP_DIR" in
      "$TEMP_PREFIX"*)
        suffix=${TMP_DIR#"$TEMP_PREFIX"}
        case "$suffix" in
          ''|*/*|*[!A-Za-z0-9]*)
            printf '%s\n' 'preflight replay cleanup refused a path outside its mktemp directory' >&2
            rc=1
            ;;
          *)
            for mounted_path in "${MOUNTED_INPUTS[@]}"; do
              case "$mounted_path" in
                "$TMP_DIR/baseline/.env"|"$TMP_DIR/baseline/docker-compose.override.yml"|\
                "$TMP_DIR/baseline/.secrets"|"$TMP_DIR/target/.env"|\
                "$TMP_DIR/target/docker-compose.override.yml"|"$TMP_DIR/target/.secrets") ;;
                *)
                  printf '%s\n' 'preflight replay cleanup refused an unexpected secrets mount path' >&2
                  rc=1
                  can_remove=0
                  continue
                  ;;
              esac
              if ! PATH="$TRUSTED_BIN_PATH" umount "$mounted_path"; then
                printf 'preflight replay could not unmount its private secrets reference: %s\n' "$mounted_path" >&2
                rc=1
                can_remove=0
              fi
            done
            if [ "$can_remove" = 1 ]; then
              rm -rf -- "$TMP_DIR" || rc=1
            else
              printf 'preflight replay temporary directory retained for safe cleanup: %s\n' "$TMP_DIR" >&2
            fi
            ;;
        esac
        ;;
      *)
        printf '%s\n' 'preflight replay cleanup refused a path outside its mktemp directory' >&2
        rc=1
        ;;
    esac
  fi
  exit "$rc"
}

mount_live_input() {
  local source=$1 destination=$2 kind=$3 options source_inode mounted_inode
  [ ! -e "$destination" ] && [ ! -L "$destination" ] || {
    fail 'checkout already contains a live Compose input path'
    return 1
  }
  case "$kind" in
    file)
      [ -f "$source" ] && [ ! -L "$source" ] || { fail 'live Compose input is not a regular file'; return 1; }
      install -m 600 /dev/null "$destination" || { fail 'cannot prepare the private Compose input reference'; return 1; }
      ;;
    directory)
      [ -d "$source" ] && [ ! -L "$source" ] || { fail 'live secrets source is not a directory'; return 1; }
      mkdir -m 700 -- "$destination" || { fail 'cannot prepare the private secrets reference'; return 1; }
      ;;
    *) fail 'private Compose input type is invalid'; return 1 ;;
  esac
  PATH="$TRUSTED_BIN_PATH" mount -o bind "$source" "$destination" || {
    fail 'cannot reference the live Compose input in the private checkout'
    return 1
  }
  MOUNTED_INPUTS+=("$destination")
  PATH="$TRUSTED_BIN_PATH" mount -o remount,bind,ro "$destination" || {
    fail 'cannot make the private Compose input reference read-only'
    return 1
  }
  options=$(awk -v target="$destination" '
    $5 == target { count++; value=$6 }
    END { if (count != 1) exit 1; print value }
  ' /proc/self/mountinfo) || { fail 'cannot verify the private Compose input mount'; return 1; }
  case ",$options," in *,ro,*) ;; *) fail 'private Compose input reference is not read-only'; return 1 ;; esac
  if [ "$kind" = directory ] && [ "$(stat -c %u:%a -- "$destination")" != "$EXPECTED_OWNER:700" ]; then
    fail 'private secrets reference has invalid ownership or permissions'
    return 1
  fi
  source_inode=$(stat -c %d:%i -- "$source") || return 1
  mounted_inode=$(stat -c %d:%i -- "$destination") || return 1
  [ "$source_inode" = "$mounted_inode" ] || {
    fail 'private Compose input reference does not point to its live source'
    return 1
  }
}

stage_target_secrets() {
  local checkout=$1 secret_file secret_name target_dir="$1/.secrets"
  local had_nullglob=0
  local -a live_secret_files=()
  if [ -e "$target_dir" ] || [ -L "$target_dir" ]; then
    fail 'PR checkout already contains a secrets path'
    return 1
  fi
  mkdir -m 700 -- "$target_dir" || { fail 'cannot prepare sanitized target secrets'; return 1; }
  shopt -q nullglob && had_nullglob=1
  shopt -s nullglob
  live_secret_files=("$LIVE_SECRETS"/*)
  [ "$had_nullglob" = 1 ] || shopt -u nullglob
  for secret_file in "${live_secret_files[@]}"; do
    secret_name=${secret_file##*/}
    if ! [[ "$secret_name" =~ ^[A-Za-z0-9._-]+$ ]] || [ "$secret_name" = . ] || [ "$secret_name" = .. ] || \
       [ ! -f "$secret_file" ] || [ -L "$secret_file" ] || \
       [ "$(stat -c %u -- "$secret_file")" != "$EXPECTED_OWNER" ]; then
      fail 'live secrets directory contains an unsupported entry'
      return 1
    fi
    install -m 444 /dev/null "$target_dir/$secret_name" || {
      fail 'cannot stage a sanitized target secret source'
      return 1
    }
  done
  if [ "$(stat -c %u:%a -- "$target_dir")" != "$EXPECTED_OWNER:700" ]; then
    fail 'sanitized target secrets directory has invalid ownership or permissions'
    return 1
  fi
}

stage_target_local_compose() {
  local checkout=$1 destination=$2 relative_file=$3 runner_source=$4
  local suffix source_file source_prefix line source_pin=0 runtime_pin=0
  local approved_content_sha source_content_sha approved_source source_source actual_content_sha candidate_source
  [[ "$relative_file" =~ ^\.rollout-compose\.local-([0-9a-f]{7})\.yml$ ]] || return 1
  suffix=${BASH_REMATCH[1]}
  source_file="deploy/teagramd/rollout-runner/local-compose-$suffix.yml"
  if [ -e "$destination" ] || [ -L "$destination" ]; then
    fail 'PR head generated local Compose path already exists outside its tracked source'
    return 1
  fi
  while IFS= read -r line; do
    case "$line" in
      "readonly "*"_LOCAL_COMPOSE_SOURCE_FILE=$source_file")
        source_prefix=${line#readonly }
        source_prefix=${source_prefix%_LOCAL_COMPOSE_SOURCE_FILE=$source_file}
        if [[ "$source_prefix" =~ ^[A-Z][A-Z0-9_]*$ ]]; then
          source_pin=1
          if grep -Fqx "readonly ${source_prefix}_LOCAL_COMPOSE_RUNTIME_FILE=$relative_file" "$runner_source"; then
            runtime_pin=1
          fi
        fi
        ;;
    esac
  done < "$runner_source"
  if [ "$source_pin" = 1 ] && [ "$runtime_pin" = 1 ]; then
    if ! git -C "$checkout" cat-file -e "$target_sha:$source_file" 2>/dev/null || \
       [ ! -f "$checkout/$source_file" ] || [ -L "$checkout/$source_file" ]; then
      fail 'PR head is missing the pinned local Compose source'
      return 1
    fi
  else
    approved_content_sha=$(sed -nE 's/^readonly APPROVED_LOCAL_COMPOSE_CONTENT_SHA=([0-9a-f]{64})$/\1/p' "$runner_source")
    source_content_sha=$(sed -nE 's/^readonly SOURCE_LOCAL_COMPOSE_CONTENT_SHA=([0-9a-f]{64})$/\1/p' "$runner_source")
    approved_source=$(sed -nE 's/^readonly DF9_LOCAL_COMPOSE_SOURCE_FILE=(deploy\/teagramd\/rollout-runner\/local-compose-[0-9a-f]{7}\.yml)$/\1/p' "$runner_source")
    source_source=$(sed -nE 's/^readonly CCE_LOCAL_COMPOSE_SOURCE_FILE=(deploy\/teagramd\/rollout-runner\/local-compose-[0-9a-f]{7}\.yml)$/\1/p' "$runner_source")
    if [ "$(grep -Fc 'case "$artifact_content_sha" in' "$runner_source")" != 1 ] || \
       [ "$(grep -Fc 'PINNED_LOCAL_COMPOSE_RUNTIME_FILE=${first_entry##*/}' "$runner_source")" != 1 ] || \
       [ "$(grep -Fc '"$APPROVED_LOCAL_COMPOSE_CONTENT_SHA")' "$runner_source")" -lt 1 ] || \
       [ "$(grep -Fc 'PINNED_LOCAL_COMPOSE_SOURCE_FILE=$DF9_LOCAL_COMPOSE_SOURCE_FILE' "$runner_source")" -lt 1 ] || \
       [ "$(grep -Fc '"$SOURCE_LOCAL_COMPOSE_CONTENT_SHA")' "$runner_source")" -lt 1 ] || \
       [ "$(grep -Fc 'PINNED_LOCAL_COMPOSE_SOURCE_FILE=$CCE_LOCAL_COMPOSE_SOURCE_FILE' "$runner_source")" -lt 1 ] || \
       ! [[ "$approved_content_sha" =~ ^[0-9a-f]{64}$ && "$source_content_sha" =~ ^[0-9a-f]{64}$ && \
            "$approved_source" =~ ^deploy/teagramd/rollout-runner/local-compose-[0-9a-f]{7}\.yml$ && \
            "$source_source" =~ ^deploy/teagramd/rollout-runner/local-compose-[0-9a-f]{7}\.yml$ ]]; then
      fail 'PR head rollout runner does not pin the selected local Compose source'
      return 1
    fi
    source_file=
    for candidate_source in "$approved_source" "$source_source"; do
      if ! git -C "$checkout" cat-file -e "$target_sha:$candidate_source" 2>/dev/null || \
         [ ! -f "$checkout/$candidate_source" ] || [ -L "$checkout/$candidate_source" ]; then
        continue
      fi
      actual_content_sha=$(awk '!/^[[:space:]]*#/' "$checkout/$candidate_source" | sha256sum) || return 1
      actual_content_sha=${actual_content_sha%% *}
      if [ "$candidate_source" = "$approved_source" ] && [ "$actual_content_sha" = "$approved_content_sha" ]; then
        source_file=$candidate_source
        break
      elif [ "$candidate_source" = "$source_source" ] && [ "$actual_content_sha" = "$source_content_sha" ]; then
        source_file=$candidate_source
        break
      fi
    done
    [ -n "$source_file" ] || {
      fail 'PR head has no tracked local Compose source matching its carried-forward pin'
      return 1
    }
  fi
  [ -n "$source_file" ] || {
    fail 'PR head rollout runner does not pin the selected local Compose source'
    return 1
  }
  install -m 600 -- "$checkout/$source_file" "$destination" || {
    fail 'cannot stage the PR head pinned local Compose source'
    return 1
  }
}

unmount_live_inputs() {
  local mounted_path failed=0
  local -a remaining=()
  for mounted_path in "${MOUNTED_INPUTS[@]}"; do
    case "$mounted_path" in
      "$TMP_DIR/baseline/.env"|"$TMP_DIR/baseline/docker-compose.override.yml"|\
      "$TMP_DIR/baseline/.secrets"|"$TMP_DIR/target/.env"|\
      "$TMP_DIR/target/docker-compose.override.yml"|"$TMP_DIR/target/.secrets") ;;
      *)
        fail 'refusing to unmount an unexpected private Compose input path'
        remaining+=("$mounted_path")
        failed=1
        continue
        ;;
    esac
    if ! PATH="$TRUSTED_BIN_PATH" umount "$mounted_path"; then
      fail 'cannot release a private Compose input reference'
      remaining+=("$mounted_path")
      failed=1
    fi
  done
  MOUNTED_INPUTS=("${remaining[@]}")
  [ "$failed" = 0 ]
}

prepare_target_runner() {
  local source=$1 output="$TMP_DIR/rollout-runner-replay.sh" helper="$TMP_DIR/runner-replay-functions.sh" dispatch_marker
  dispatch_marker="compare_resolved_preflight_command \"\${@:2}\""
  if grep -Fq 'ROLLOUT_PREFLIGHT_REPLAY_SANDBOX' "$source" && \
     grep -Fq "$dispatch_marker" "$source"; then
    printf '%s\n' "$source"
    return 0
  fi
  sed -n \
    -e '/^check_private_dir() {$/,/^}/p' \
    -e '/^write_immutable() {$/,/^}/p' \
    -e '/^compare_resolved_preflight_command() {$/,/^}/p' \
    "$BASE_RUNNER" > "$helper" || return 1
  if [ ! -s "$helper" ] || \
     ! grep -Fq 'check_private_dir() {' "$helper" || \
     ! grep -Fq 'write_immutable() {' "$helper" || \
     ! grep -Fq 'compare_resolved_preflight_command() {' "$helper"; then
    fail 'reviewed preflight compatibility functions are unavailable'
    return 1
  fi
  if ! awk -v helper_file="$helper" '
    /^main\(\) \{/ && !inserted {
      while ((getline line < helper_file) > 0) print line
      close(helper_file)
      print
      print "  if [ \"$#\" -ge 2 ] && [ \"$#\" -le 3 ] && [ \"$1\" = compare-resolved-preflight ]; then"
      print "    compare_resolved_preflight_command \"${@:2}\""
      print "    return $?"
      print "  fi"
      inserted=1
      next
    }
    { print }
    END { if (!inserted) exit 1 }
  ' "$source" > "$output"; then
    fail 'cannot prepare the PR-head preflight comparator'
    return 1
  fi
  chmod 700 "$output" || return 1
  printf '%s\n' "$output"
}

copy_sandbox_binary() {
  local name=$1 destination=$2 source dependencies line dep target
  source=$(PATH="$TRUSTED_BIN_PATH" command -v "$name") || {
    fail "sandbox runtime tool is unavailable: $name"
    return 1
  }
  case "$source" in /*) ;; *) fail "sandbox runtime path is invalid: $name"; return 1 ;; esac
  [ -f "$source" ] || { fail "sandbox runtime tool is not a regular file: $name"; return 1; }
  mkdir -p -- "$SANDBOX_ROOT$(dirname -- "$destination")" || return 1
  chmod 755 "$SANDBOX_ROOT$(dirname -- "$destination")" || return 1
  install -m 755 -- "$source" "$SANDBOX_ROOT$destination" || return 1
  dependencies=$(PATH="$TRUSTED_BIN_PATH" ldd "$source" 2>&1) || {
    case "$dependencies" in
      *'not a dynamic executable'*|*'statically linked'*) return 0 ;;
      *) fail "cannot inspect sandbox runtime dependencies: $name"; return 1 ;;
    esac
  }
  while IFS= read -r line; do
    dep=
    for target in $line; do
      case "$target" in /*) dep=$target; break ;; esac
    done
    case "$line" in *'not found'*) fail "sandbox runtime dependency is unavailable: $name"; return 1 ;; esac
    [ -n "$dep" ] || continue
    [ -f "$dep" ] || { fail "sandbox runtime dependency is not a regular file: $name"; return 1; }
    if [ ! -e "$SANDBOX_ROOT$dep" ]; then
      mkdir -p -- "$SANDBOX_ROOT$(dirname -- "$dep")" || return 1
      chmod 755 "$SANDBOX_ROOT$(dirname -- "$dep")" || return 1
      install -m 755 -- "$dep" "$SANDBOX_ROOT$dep" || return 1
    fi
  done <<< "$dependencies"
}

prepare_sandbox() {
  local command path
  local -a commands=(bash jq id readlink stat dirname basename awk sha256sum date mktemp chmod rm ln sync mkdir)
  SANDBOX_ROOT="$TMP_DIR/sandbox"
  SANDBOX_INPUT="$SANDBOX_ROOT/tmp/replay-input"
  SANDBOX_OUTPUT="$SANDBOX_ROOT/tmp/replay-output"
  SANDBOX_RUNNER=/workspace/deploy/teagramd/rollout-runner/rollout-runner.sh
  SANDBOX_VERIFIER=/workspace/deploy/teagramd/rollout-runner/rollout-verifier.sh
  SANDBOX_EVIDENCE=/tmp/replay-input
  SANDBOX_RESULTS=/tmp/replay-output
  readonly SANDBOX_ROOT SANDBOX_INPUT SANDBOX_OUTPUT SANDBOX_RUNNER SANDBOX_VERIFIER SANDBOX_EVIDENCE SANDBOX_RESULTS

  if ! PATH="$TRUSTED_BIN_PATH" command -v chroot >/dev/null 2>&1; then
    fail 'unprivileged chroot sandbox is unavailable'
    return 1
  elif PATH="$TRUSTED_BIN_PATH" chroot --help 2>&1 | grep -Fq -- '--userspec'; then
    CHROOT_KIND=coreutils
    CHROOT_BIN=$(PATH="$TRUSTED_BIN_PATH" command -v chroot)
  elif PATH="$TRUSTED_BIN_PATH" busybox chroot --help 2>&1 | grep -Fq -- '-u USER'; then
    CHROOT_KIND=busybox
    CHROOT_BIN=$(PATH="$TRUSTED_BIN_PATH" command -v busybox)
  else
    fail 'chroot does not support dropping sandbox privileges'
    return 1
  fi
  HOST_ENV=$(PATH="$TRUSTED_BIN_PATH" command -v env) || { fail 'sandbox launcher is unavailable'; return 1; }
  HOST_TIMEOUT=$(PATH="$TRUSTED_BIN_PATH" command -v timeout) || { fail 'sandbox timeout is unavailable'; return 1; }
  HOST_UNSHARE=$(PATH="$TRUSTED_BIN_PATH" command -v unshare) || {
    fail 'network-isolated sandbox launcher is unavailable'
    return 1
  }
  readonly CHROOT_KIND CHROOT_BIN HOST_ENV HOST_TIMEOUT HOST_UNSHARE

  install -d -m 755 "$SANDBOX_ROOT" "$SANDBOX_ROOT/etc" "$SANDBOX_ROOT/dev" \
    "$SANDBOX_ROOT/workspace/deploy/teagramd/rollout-runner"
  install -d -m 1777 "$SANDBOX_ROOT/tmp"
  install -d -m 755 "$SANDBOX_INPUT"
  install -d -m 700 "$SANDBOX_OUTPUT"
  printf 'nobody:x:%s:%s:nobody:/:/sbin/nologin\n' "$SANDBOX_UID" "$SANDBOX_UID" > "$SANDBOX_ROOT/etc/passwd"
  printf 'nobody:x:%s:\n' "$SANDBOX_UID" > "$SANDBOX_ROOT/etc/group"
  chmod 644 "$SANDBOX_ROOT/etc/passwd" "$SANDBOX_ROOT/etc/group"
  install -m 666 /dev/null "$SANDBOX_ROOT/dev/null"

  for command in "${commands[@]}"; do
    case "$command" in
      bash) path=/bin/bash ;;
      jq) path=/usr/bin/jq ;;
      *) path=$(PATH="$TRUSTED_BIN_PATH" command -v "$command") || { fail "sandbox runtime tool is unavailable: $command"; return 1; } ;;
    esac
    copy_sandbox_binary "$command" "$path" || return 1
  done
  chown 0:0 "$SANDBOX_ROOT" "$SANDBOX_ROOT/tmp" "$SANDBOX_INPUT" || return 1
  chown "$SANDBOX_UID:$SANDBOX_UID" "$SANDBOX_OUTPUT" || return 1
  PATH="$TRUSTED_BIN_PATH" find "$SANDBOX_ROOT" -type d ! -path "$SANDBOX_OUTPUT" \
    -exec chmod 755 {} + || return 1
  chmod 755 "$SANDBOX_ROOT" "$SANDBOX_ROOT/etc" "$SANDBOX_ROOT/dev" \
    "$SANDBOX_ROOT/workspace" "$SANDBOX_ROOT/workspace/deploy" \
    "$SANDBOX_ROOT/workspace/deploy/teagramd" \
    "$SANDBOX_ROOT/workspace/deploy/teagramd/rollout-runner"
  chmod 1777 "$SANDBOX_ROOT/tmp"
  chmod 755 "$SANDBOX_INPUT"
  chmod 700 "$SANDBOX_OUTPUT"
  install -m 444 "$TMP_DIR/baseline.snapshot.json" "$SANDBOX_INPUT/baseline.snapshot.json"
  install -m 444 "$TMP_DIR/preflight.snapshot.json" "$SANDBOX_INPUT/preflight.snapshot.json"
  chown 0:0 "$SANDBOX_INPUT/baseline.snapshot.json" "$SANDBOX_INPUT/preflight.snapshot.json"
  install -m 555 "$target_runner" "$SANDBOX_ROOT$SANDBOX_RUNNER"
  install -m 555 "$target_verifier_source" "$SANDBOX_ROOT$SANDBOX_VERIFIER"
  [ "$(stat -c %u:%a -- "$SANDBOX_ROOT")" = '0:755' ] && \
    [ "$(stat -c %u:%a -- "$SANDBOX_INPUT")" = '0:755' ] && \
    [ "$(stat -c %u:%a -- "$SANDBOX_OUTPUT")" = "$SANDBOX_UID:700" ] || {
    fail 'preflight replay sandbox permissions are invalid'
    return 1
  }
}

run_in_sandbox() {
  local command=$1 rc=0 descriptor fd
  shift
  (
    ulimit -c 0
    ulimit -f 2048
    ulimit -n 64
    ulimit -t 30
    ulimit -v 262144
    ulimit -u 64
    for descriptor in /proc/"$BASHPID"/fd/*; do
      fd=${descriptor##*/}
      [[ "$fd" =~ ^[0-9]+$ ]] || continue
      case "$fd" in 0|1|2) continue ;; esac
      eval "exec ${fd}>&-" 2>/dev/null || :
    done
    if [ "$CHROOT_KIND" = coreutils ]; then
      exec "$HOST_TIMEOUT" -k 5 45 "$HOST_UNSHARE" -n -- \
        "$HOST_ENV" -i PATH="$SANDBOX_PATH" HOME=/nonexistent LANG=C LC_ALL=C TZ=UTC \
        ROLLOUT_PREFLIGHT_REPLAY_SANDBOX=1 \
        ROLLOUT_VERIFIER_SOURCE_ONLY=1 \
        PREFLIGHT_REPLAY_SANDBOX_EVIDENCE_DIR="$SANDBOX_RESULTS" \
        "$CHROOT_BIN" --groups="$SANDBOX_UID" --userspec="$SANDBOX_UID:$SANDBOX_UID" \
        "$SANDBOX_ROOT" /bin/bash "$command" "$@"
    fi
    exec "$HOST_TIMEOUT" -k 5 45 "$HOST_UNSHARE" -n -- \
      "$HOST_ENV" -i PATH="$SANDBOX_PATH" HOME=/nonexistent LANG=C LC_ALL=C TZ=UTC \
      ROLLOUT_PREFLIGHT_REPLAY_SANDBOX=1 \
      ROLLOUT_VERIFIER_SOURCE_ONLY=1 \
      PREFLIGHT_REPLAY_SANDBOX_EVIDENCE_DIR="$SANDBOX_RESULTS" \
      "$CHROOT_BIN" chroot -u "$SANDBOX_UID:$SANDBOX_UID" "$SANDBOX_ROOT" \
      /bin/bash "$command" "$@"
  ) </dev/null >/dev/null 2>&1 || rc=$?
  return "$rc"
}

if ! TMP_DIR=$(mktemp -d "${TEMP_PREFIX}XXXXXXXXXX"); then
  fail 'cannot create a private replay directory'
  exit 1
fi
trap cleanup EXIT
case "$TMP_DIR" in
  "$TEMP_PREFIX"*) ;;
  *) fail 'temporary directory is outside the approved prefix'; exit 1 ;;
esac
temp_suffix=${TMP_DIR#"$TEMP_PREFIX"}
case "$temp_suffix" in
  ''|*/*|*[!A-Za-z0-9]*) fail 'temporary directory is not a direct mktemp child'; exit 1 ;;
esac
[ "$(readlink -f -- "$TMP_DIR")" = "$TMP_DIR" ] && \
  [ "$(stat -c %u:%a -- "$TMP_DIR")" = "$EXPECTED_OWNER:700" ] || {
  fail 'temporary directory is not private'
  exit 1
}
mkdir -m 700 "$TMP_DIR/docker-config"
export DOCKER_CONFIG="$TMP_DIR/docker-config"

BASE_RUNNER="$TMP_DIR/rollout-runner.sh"
BASE_VERIFIER="$TMP_DIR/rollout-verifier.sh"
if ! git -C "$LIVE_ROOT" show \
  "$runtime_sha:deploy/teagramd/rollout-runner/rollout-runner.sh" > "$BASE_RUNNER" 2>/dev/null; then
  fail 'cannot extract the reviewed rollout runner'
  exit 1
fi
if ! git -C "$LIVE_ROOT" show \
  "$runtime_sha:deploy/teagramd/rollout-runner/rollout-verifier.sh" > "$BASE_VERIFIER" 2>/dev/null; then
  fail 'cannot extract the reviewed rollout verifier'
  exit 1
fi
chmod 700 "$BASE_RUNNER" "$BASE_VERIFIER" || { fail 'cannot secure the reviewed rollout runtime'; exit 1; }
[ -f "$BASE_RUNNER" ] && [ ! -L "$BASE_RUNNER" ] && [ -s "$BASE_RUNNER" ] && \
  [ -f "$BASE_VERIFIER" ] && [ ! -L "$BASE_VERIFIER" ] && [ -s "$BASE_VERIFIER" ] || {
  fail 'reviewed rollout runtime is unavailable'
  exit 1
}
readonly BASE_RUNNER BASE_VERIFIER

live_sha=$(git -C "$LIVE_ROOT" rev-parse --verify 'HEAD^{commit}' 2>/dev/null) || {
  fail 'cannot resolve the live checkout revision'
  exit 1
}
[[ "$live_sha" =~ ^[0-9a-f]{40}$ ]] || { fail 'live checkout revision is invalid'; exit 1; }
git -C "$LIVE_ROOT" diff --quiet HEAD -- || { fail 'live tracked files are modified'; exit 1; }

LIVE_SECRETS="$LIVE_ROOT/.secrets"
[ -d "$LIVE_SECRETS" ] && [ ! -L "$LIVE_SECRETS" ] && \
  [ "$(readlink -f -- "$LIVE_SECRETS")" = "$LIVE_SECRETS" ] && \
  [ "$(stat -c %u:%a -- "$LIVE_SECRETS")" = "$EXPECTED_OWNER:700" ] || {
  fail 'live secrets directory is unavailable or unsafe'
  exit 1
}
readonly LIVE_SECRETS

if ! git clone --quiet --local --no-hardlinks "$LIVE_ROOT" "$TMP_DIR/baseline" 2>/dev/null; then
  fail 'cannot prepare the live baseline checkout'
  exit 1
fi
chmod 700 "$TMP_DIR/baseline"
mkdir -m 700 "$TMP_DIR/target"
if ! git -C "$TMP_DIR/target" init -q 2>/dev/null || \
   ! git -C "$TMP_DIR/target" remote add origin "$REPOSITORY_URL" 2>/dev/null || \
   ! git -C "$TMP_DIR/target" fetch --quiet --no-tags --depth=1 origin "$target_sha" 2>/dev/null || \
   ! git -C "$TMP_DIR/target" checkout --quiet --detach FETCH_HEAD 2>/dev/null; then
  fail 'cannot fetch the requested PR head'
  exit 1
fi
chmod 700 "$TMP_DIR/target"
resolved_sha=$(git -C "$TMP_DIR/target" rev-parse --verify 'HEAD^{commit}' 2>/dev/null) || {
  fail 'cannot resolve the fetched PR head'
  exit 1
}
[ "$resolved_sha" = "$target_sha" ] || { fail 'fetched commit does not match the requested PR head'; exit 1; }

live_env="$LIVE_ROOT/.env"
live_override="$LIVE_ROOT/docker-compose.override.yml"
for file in "$live_env" "$live_override"; do
  [ -f "$file" ] && [ ! -L "$file" ] && [ "$(stat -c %u -- "$file")" = "$EXPECTED_OWNER" ] || {
    fail 'live Compose inputs are unavailable'
    exit 1
  }
  [ "$(readlink -f -- "$file")" = "$file" ] || { fail 'live Compose inputs are symlinked'; exit 1; }
done
[ "$(stat -c %a -- "$live_env")" = 600 ] || { fail 'live environment file is not mode 0600'; exit 1; }

container_json=$(docker inspect teagram-teagramd-1 2>/dev/null) || {
  fail 'cannot inspect the running teagramd container'
  exit 1
}
container_id=$(printf '%s' "$container_json" | jq -er '
  if type == "array" and length == 1 then .[0] else error("container inspection is ambiguous") end
  | select(.State.Status == "running")
  | .Id | select(type == "string" and test("^[0-9a-f]{64}$"))
' 2>/dev/null) || { fail 'running teagramd container identity is invalid'; exit 1; }
deployed_sha=$(printf '%s' "$container_json" | jq -er '
  .[0].Config.Labels["org.opencontainers.image.revision"]
  | select(type == "string" and test("^[0-9a-f]{40}$"))
' 2>/dev/null) || { fail 'running teagramd deployed revision is invalid'; exit 1; }
git -C "$LIVE_ROOT" cat-file -e "$deployed_sha^{commit}" 2>/dev/null || {
  fail 'running teagramd deployed revision is unavailable in the live checkout'
  exit 1
}
if ! git -C "$TMP_DIR/baseline" checkout --quiet --detach "$deployed_sha" 2>/dev/null; then
  fail 'cannot prepare the deployed revision baseline checkout'
  exit 1
fi
project_name=$(printf '%s' "$container_json" | jq -er '
  .[0].Config.Labels["com.docker.compose.project"]
  | select(type == "string" and test("^[a-z0-9][a-z0-9_-]*$"))
' 2>/dev/null) || { fail 'running teagramd Compose project label is invalid'; exit 1; }
selection=$(printf '%s' "$container_json" | jq -er '
  .[0].Config.Labels["com.docker.compose.project.config_files"]
  | select(type == "string" and length > 0)
' 2>/dev/null) || { fail 'running teagramd Compose file label is missing'; exit 1; }

target_runner_source="$TMP_DIR/target/deploy/teagramd/rollout-runner/rollout-runner.sh"
target_verifier_source="$TMP_DIR/target/deploy/teagramd/rollout-runner/rollout-verifier.sh"
[ -d "$TMP_DIR/target/deploy" ] && [ ! -L "$TMP_DIR/target/deploy" ] && \
  [ -d "$TMP_DIR/target/deploy/teagramd" ] && [ ! -L "$TMP_DIR/target/deploy/teagramd" ] && \
  [ -d "$TMP_DIR/target/deploy/teagramd/rollout-runner" ] && \
  [ ! -L "$TMP_DIR/target/deploy/teagramd/rollout-runner" ] || {
  fail 'PR head rollout runtime path is symlinked'
  exit 1
}
[ -f "$target_runner_source" ] && [ ! -L "$target_runner_source" ] && \
  [ -f "$target_verifier_source" ] && [ ! -L "$target_verifier_source" ] || {
  fail 'PR head rollout runtime is unavailable'
  exit 1
}

IFS=, read -r -a live_files <<< "$selection"
[ "${#live_files[@]}" -gt 0 ] || { fail 'running Compose selection is empty'; exit 1; }
compose_selection=
for live_file in "${live_files[@]}"; do
  case "$live_file" in
    "$LIVE_ROOT"/*) relative_file=${live_file#"$LIVE_ROOT"/} ;;
    *) fail 'running Compose selection leaves the live checkout'; exit 1 ;;
  esac
  [[ "$relative_file" =~ ^[A-Za-z0-9._-]+$ ]] && [ "$relative_file" != .. ] && \
    [[ "$relative_file" != .*..* ]] || { fail 'running Compose selection is invalid'; exit 1; }
  [ -f "$live_file" ] && [ ! -L "$live_file" ] && \
    [ "$(readlink -f -- "$live_file")" = "$live_file" ] || {
    fail 'selected live Compose file is unavailable or symlinked'
    exit 1
  }
  for checkout in "$TMP_DIR/baseline" "$TMP_DIR/target"; do
    destination="$checkout/$relative_file"
    if [ -L "$destination" ] || { [ -e "$destination" ] && [ ! -f "$destination" ]; }; then
      fail 'selected Compose path is not a regular file'
      exit 1
    fi
    if [ "$relative_file" = docker-compose.override.yml ]; then
      continue
    elif [ "$checkout" = "$TMP_DIR/baseline" ]; then
      if git -C "$checkout" cat-file -e "$deployed_sha:$relative_file" 2>/dev/null; then
        [ -f "$destination" ] && [ ! -L "$destination" ] || {
          fail 'deployed revision selected Compose file is unavailable'
          exit 1
        }
      elif [[ "$relative_file" =~ ^\.rollout-compose\.local-[0-9a-f]{7}\.yml$ ]]; then
        install -m 600 -- "$live_file" "$destination" || { fail 'cannot stage the deployed local Compose artifact'; exit 1; }
      else
        fail 'deployed revision is missing a selected Compose file'
        exit 1
      fi
    else
      if git -C "$checkout" cat-file -e "$target_sha:$relative_file" 2>/dev/null; then
        [ -f "$destination" ] && [ ! -L "$destination" ] || {
          fail 'PR head selected Compose file is unavailable'
          exit 1
        }
      elif [[ "$relative_file" =~ ^\.rollout-compose\.local-[0-9a-f]{7}\.yml$ ]]; then
        stage_target_local_compose "$checkout" "$destination" "$relative_file" "$target_runner_source" || exit 1
      else
        fail 'PR head is missing a selected Compose file'
        exit 1
      fi
    fi
  done
  if [ -n "$compose_selection" ]; then compose_selection+=,; fi
  compose_selection+="$relative_file"
done

for checkout in "$TMP_DIR/baseline" "$TMP_DIR/target"; do
  for name in .env docker-compose.override.yml; do
    destination="$checkout/$name"
    if [ -e "$destination" ] || [ -L "$destination" ]; then
      fail 'checkout already contains a live Compose input path'
      exit 1
    fi
  done
  mount_live_input "$live_env" "$checkout/.env" file || exit 1
  mount_live_input "$live_override" "$checkout/docker-compose.override.yml" file || exit 1
done
stage_target_secrets "$TMP_DIR/target" || exit 1
mount_live_input "$LIVE_SECRETS" "$TMP_DIR/baseline/.secrets" directory || exit 1

target_runner=$(prepare_target_runner "$target_runner_source") || exit 1

for checkout in "$TMP_DIR/baseline" "$TMP_DIR/target"; do
  name=preflight
  [ "$checkout" = "$TMP_DIR/baseline" ] && name=baseline
  ROLLOUT_PREFLIGHT_REPLAY=1 ROLLOUT_PREFLIGHT_LIVE_SECRETS_DIR="$LIVE_SECRETS" \
    bash "$BASE_VERIFIER" preflight-snapshot "$container_id" "$checkout" "$TMP_DIR" \
    "$name" "$project_name" "$compose_selection" >/dev/null || exit $?
done
unmount_live_inputs || exit 1

for checkout in "$TMP_DIR/baseline" "$TMP_DIR/target"; do
  rm -f -- "$checkout/.env" "$checkout/docker-compose.override.yml" || {
    fail 'cannot remove private Compose inputs from replay checkouts'
    exit 1
  }
done

prepare_sandbox || exit 1
if ! run_in_sandbox "$SANDBOX_VERIFIER"; then
  fail 'PR-head verifier could not load in the unprivileged sandbox'
  exit 1
fi

if [ "$target_runner" != "$target_runner_source" ]; then
  # Legacy comparators read the target render from TARGET_DIR rather than the input directory.
  install -m 444 "$SANDBOX_INPUT/preflight.snapshot.json" \
    "$SANDBOX_OUTPUT/preflight.snapshot.json" || {
    fail 'cannot stage the legacy preflight snapshot path'
    exit 1
  }
fi

runner_rc=0
if run_in_sandbox "$SANDBOX_RUNNER" compare-resolved-preflight "$SANDBOX_EVIDENCE" "$SANDBOX_RESULTS"; then
  :
else
  runner_rc=$?
fi

comparison_file="$SANDBOX_OUTPUT/preflight-comparisons.tsv"
[ -f "$comparison_file" ] && [ ! -L "$comparison_file" ] && \
  [ "$(stat -c %u:%a -- "$comparison_file")" = "$SANDBOX_UID:600" ] && \
  [ "$(stat -c %s -- "$comparison_file")" -le 65536 ] || {
  fail 'sandbox comparison evidence is unavailable or malformed'
  exit 1
}
# The timestamp is private metadata; preserve exact TSV fields without printing values.
if ! comparison_rows=$(awk -F '\t' '
  BEGIN {
    count = split("config_sha256 env_sha256 override_sha256 stop_grace_period compose_logging_policy compose_log_login_codes media_key_source media_key_current compose_public_ws_2446 exposure_public_ws_2446 compose_replica_count compose_client_addr_trust", fields, " ")
    for (i = 1; i <= count; i++) allowed[fields[i]] = 1
  }
  # write_immutable creates an empty TSV with one newline before rows append.
  NR == 1 && NF == 0 { next }
  {
    if (NF != 6 || $2 != "preflight" || !($3 in allowed) || $6 !~ /^(pass|fail)$/ || seen[$3]++) {
      invalid = 1
      exit 1
    }
    comparison_count++
    printf "%s\t%s\n", $3, $6
  }
  END { if (invalid || comparison_count != 12) exit 1 }
' "$comparison_file"); then
  fail 'sandbox comparison evidence is malformed or incomplete'
  exit 1
fi
if ! env_addition_names=$(jq -nr --slurpfile baseline "$TMP_DIR/baseline.snapshot.json" \
    --slurpfile target "$TMP_DIR/preflight.snapshot.json" --rawfile env_content "$live_env" '
  def protected_name:
    startswith("TG_RSA_KEY_") or startswith("TG_AUTHKEY_ENC_KEY") or
    startswith("TG_POSTGRES_") or startswith("POSTGRES_") or
    startswith("TG_BLOB_") or startswith("RUSTFS_") or
    startswith("TG_REPLICA_") or . == "TG_CLIENT_ADDR_TRUST" or
    startswith("COMPOSE_") or startswith("DOCKER_");
  def runtime_name($name):
    if $name == "TEAGRAM_TURNSTILE_SITE_KEY" then "TG_TURNSTILE_SITE_KEY"
    elif $name == "TEAGRAM_TURNSTILE_SECRET_KEY_FILE" then "TG_TURNSTILE_SECRET_KEY_FILE"
    else $name end;
  def live_env_names:
    $env_content | split("\n") | map(select(test("^[A-Z][A-Z0-9_]*=")))
    | map(split("=")[0]) | unique;
  ($baseline[0].compose_env_names) as $baseline_names
  | ($target[0].compose_env_names) as $target_names
  | ($baseline[0].container_env_names) as $container_names
  | live_env_names as $existing
  | if ($baseline_names | type) != "array" or ($target_names | type) != "array" or
      ($container_names | type) != "array" then
      error("resolved Compose environment names are missing")
    else . end
  | ($target_names - $baseline_names)
  | map(select(type == "string" and test("^[A-Z][A-Z0-9_]*$")))
  | map(. as $name | select(($name | protected_name) | not))
  | map(. as $name | select(($existing | index($name)) == null))
  | map(. as $name | select(($container_names | index(runtime_name($name))) == null))
  | unique | sort | .[]
'); then
  fail 'cannot derive allowlisted environment additions from the resolved snapshots'
  exit 1
fi
printf '%s\n' "$comparison_rows"
if [ -n "$env_addition_names" ]; then
  while IFS= read -r env_name; do
    printf 'env_addition\t%s\n' "$env_name"
  done <<< "$env_addition_names"
else
  printf 'env_addition\tnone\n'
fi
[ "$runner_rc" -eq 0 ] || exit "$runner_rc"
