#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

BASE_RUNNER=
BASE_VERIFIER=
readonly TEMP_PREFIX=/tmp/teagram-preflight-replay.
readonly SANDBOX_UID=65534
readonly SANDBOX_PATH=/bin:/usr/bin
readonly TRUSTED_BIN_PATH=/usr/sbin:/usr/bin:/sbin:/bin

fail() {
  printf 'preflight replay rejected: %s\n' "$1" >&2
  return 1
}

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
fi

if [ "$LIVE_ROOT" != /opt/teagram ]; then
  LIVE_ROOT=$(readlink -f -- "$LIVE_ROOT") || { fail 'live checkout is unavailable'; exit 1; }
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

TMP_DIR=
cleanup() {
  local rc=$? suffix
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
          *) rm -rf -- "$TMP_DIR" || rc=1 ;;
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
project_name=$(printf '%s' "$container_json" | jq -er '
  .[0].Config.Labels["com.docker.compose.project"]
  | select(type == "string" and test("^[a-z0-9][a-z0-9_-]*$"))
' 2>/dev/null) || { fail 'running teagramd Compose project label is invalid'; exit 1; }
selection=$(printf '%s' "$container_json" | jq -er '
  .[0].Config.Labels["com.docker.compose.project.config_files"]
  | select(type == "string" and length > 0)
' 2>/dev/null) || { fail 'running teagramd Compose file label is missing'; exit 1; }

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
      install -m 600 -- "$live_override" "$destination" || { fail 'cannot stage the live Compose override'; exit 1; }
    elif [ "$checkout" = "$TMP_DIR/baseline" ]; then
      install -m 600 -- "$live_file" "$destination" || { fail 'cannot stage a live Compose file'; exit 1; }
    else
      if git -C "$checkout" cat-file -e "$target_sha:$relative_file" 2>/dev/null; then
        [ -f "$destination" ] && [ ! -L "$destination" ] || {
          fail 'PR head selected Compose file is unavailable'
          exit 1
        }
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
    if [ -L "$destination" ] || { [ -e "$destination" ] && [ ! -f "$destination" ]; }; then
      fail 'live Compose input path is not a regular file'
      exit 1
    fi
  done
  install -m 600 -- "$live_env" "$checkout/.env" || { fail 'cannot stage the live environment file'; exit 1; }
  install -m 600 -- "$live_override" "$checkout/docker-compose.override.yml" || {
    fail 'cannot stage the live Compose override'
    exit 1
  }
done

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
target_runner=$(prepare_target_runner "$target_runner_source") || exit 1

for checkout in "$TMP_DIR/baseline" "$TMP_DIR/target"; do
  name=preflight
  [ "$checkout" = "$TMP_DIR/baseline" ] && name=baseline
  ROLLOUT_PREFLIGHT_TURNSTILE_SECRET_SOURCE="$LIVE_ROOT/.secrets/teagramd-turnstile" \
    bash "$BASE_VERIFIER" preflight-snapshot "$container_id" "$checkout" "$TMP_DIR" \
    "$name" "$project_name" "$compose_selection" >/dev/null || exit $?
done

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
printf '%s\n' "$comparison_rows"
[ "$runner_rc" -eq 0 ] || exit "$runner_rc"
