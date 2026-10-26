#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
readonly REPO_ROOT=$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel 2>/dev/null || true)
readonly BASE_RUNNER="$REPO_ROOT/deploy/teagramd/rollout-runner/rollout-runner.sh"
readonly BASE_VERIFIER="$REPO_ROOT/deploy/teagramd/rollout-runner/rollout-verifier.sh"
readonly TEMP_PREFIX=/tmp/teagram-preflight-replay.

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

[ -n "$REPO_ROOT" ] || { fail 'rollout runtime checkout is unavailable'; exit 1; }
[ -f "$BASE_RUNNER" ] && [ ! -L "$BASE_RUNNER" ] && [ -f "$BASE_VERIFIER" ] && [ ! -L "$BASE_VERIFIER" ] || {
  fail 'reviewed rollout runtime is unavailable'
  exit 1
}
if [ "$LIVE_ROOT" != /opt/teagram ]; then
  LIVE_ROOT=$(readlink -f -- "$LIVE_ROOT") || { fail 'live checkout is unavailable'; exit 1; }
fi
[ -d "$LIVE_ROOT" ] && [ ! -L "$LIVE_ROOT" ] || { fail 'live checkout is unavailable'; exit 1; }
[ "$(git -C "$LIVE_ROOT" rev-parse --is-inside-work-tree 2>/dev/null)" = true ] || {
  fail 'live checkout is not a Git worktree'
  exit 1
}

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
  local source=$1 output="$TMP_DIR/rollout-runner-replay.sh" helper="$TMP_DIR/runner-replay-function.sh"
  if [ "${PREFLIGHT_REPLAY_TEST_MODE:-0}" != 1 ] || \
     [ "${PREFLIGHT_REPLAY_TEST_FORCE_COMPAT:-0}" != 1 ]; then
    if grep -Fq 'compare_resolved_preflight_command "$2"' "$source"; then
      printf '%s\n' "$source"
      return 0
    fi
  fi
  sed -n '/^compare_resolved_preflight_command() {$/,/^}/p' "$BASE_RUNNER" > "$helper" || return 1
  [ -s "$helper" ] || { fail 'reviewed preflight comparison command is unavailable'; return 1; }
  if ! awk -v helper_file="$helper" '
    /^main\(\) \{/ && !inserted {
      while ((getline line < helper_file) > 0) print line
      close(helper_file)
      print
      print "  if [ \"$#\" -eq 2 ] && [ \"$1\" = compare-resolved-preflight ]; then"
      print "    compare_resolved_preflight_command \"$2\""
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

prepare_target_verifier() {
  local source=$1 output="$TMP_DIR/rollout-verifier-replay.sh" helper="$TMP_DIR/verifier-replay-functions.sh"
  if [ "${PREFLIGHT_REPLAY_TEST_MODE:-0}" != 1 ] || \
     [ "${PREFLIGHT_REPLAY_TEST_FORCE_COMPAT:-0}" != 1 ]; then
    if grep -Fq 'preflight_snapshot_command "$@"' "$source"; then
      printf '%s\n' "$source"
      return 0
    fi
  fi
  awk '
    /^secure_replay_evidence_dir\(\) \{$/ { copying=1 }
    copying { print }
    copying && /^}$/ { closed++ ; if (closed == 2) exit }
    END { if (closed != 2) exit 1 }
  ' "$BASE_VERIFIER" > "$helper" || return 1
  [ -s "$helper" ] || { fail 'reviewed preflight snapshot command is unavailable'; return 1; }
  if ! awk -v helper_file="$helper" '
    /^main\(\) \{/ && !functions_inserted {
      while ((getline line < helper_file) > 0) print line
      close(helper_file)
      print
      functions_inserted=1
      next
    }
    /^    snapshot\)/ && functions_inserted && !command_inserted {
      print "    preflight-snapshot)"
      print "      [ \"$#\" -eq 6 ] || return 64"
      print "      preflight_snapshot_command \"$@\" ;;"
      command_inserted=1
    }
    { print }
    END { if (!functions_inserted || !command_inserted) exit 1 }
  ' "$source" > "$output"; then
    fail 'cannot prepare the PR-head snapshot verifier'
    return 1
  fi
  chmod 700 "$output" || return 1
  printf '%s\n' "$output"
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
        install -m 600 -- "$live_file" "$destination" || { fail 'cannot stage the live Compose artifact'; exit 1; }
      fi
    fi
  done
  if [ -n "$compose_selection" ]; then compose_selection+=,; fi
  compose_selection+="$relative_file"
done

for checkout in "$TMP_DIR/baseline" "$TMP_DIR/target"; do
  for name in .env docker-compose.override.yml; do
    destination="$checkout/$name"
    [ ! -L "$destination" ] && { [ ! -e "$destination" ] || [ -f "$destination" ]; } || {
      fail 'live Compose input path is not a regular file'
      exit 1
    }
  done
  install -m 600 -- "$live_env" "$checkout/.env" || { fail 'cannot stage the live environment file'; exit 1; }
  install -m 600 -- "$live_override" "$checkout/docker-compose.override.yml" || {
    fail 'cannot stage the live Compose override'
    exit 1
  }
done

target_runner_source="$TMP_DIR/target/deploy/teagramd/rollout-runner/rollout-runner.sh"
target_verifier_source="$TMP_DIR/target/deploy/teagramd/rollout-runner/rollout-verifier.sh"
[ -f "$target_runner_source" ] && [ ! -L "$target_runner_source" ] && \
  [ -f "$target_verifier_source" ] && [ ! -L "$target_verifier_source" ] || {
  fail 'PR head rollout runtime is unavailable'
  exit 1
}
target_runner=$(prepare_target_runner "$target_runner_source") || exit 1
target_verifier=$(prepare_target_verifier "$target_verifier_source") || exit 1

for checkout in "$TMP_DIR/baseline" "$TMP_DIR/target"; do
  name=preflight
  [ "$checkout" = "$TMP_DIR/baseline" ] && name=baseline
  bash "$target_verifier" preflight-snapshot "$container_id" "$checkout" "$TMP_DIR" \
    "$name" "$project_name" "$compose_selection" >/dev/null || exit $?
done

bash "$target_runner" compare-resolved-preflight "$TMP_DIR"
