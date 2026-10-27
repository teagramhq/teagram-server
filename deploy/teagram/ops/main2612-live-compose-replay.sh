#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly LIVE_CHECKOUT=/opt/teagram
OPS_SHA=${OPS_SHA:-}
TMP=
TMP_TOKEN=
unset COMPOSE_ENV_FILES

cleanup() {
  local status=$? cleanup_status=0 owner mode real_tmp marker child target remaining
  trap - EXIT
  if [ -n "$TMP" ]; then
    case "$TMP" in
      /tmp/main2612-replay.*)
        if [[ "$TMP" =~ ^/tmp/main2612-replay\.[A-Za-z0-9]{8}$ ]] && \
           [ -d "$TMP" ] && [ ! -L "$TMP" ] && \
           real_tmp=$(realpath -e -- "$TMP") && [ "$real_tmp" = "$TMP" ] && \
           owner=$(stat -c %u -- "$TMP") && [ "$owner" = 0 ] && \
           mode=$(stat -c %a -- "$TMP") && [ "$mode" = 700 ]; then
          if [ -n "$TMP_TOKEN" ]; then
            [[ "$TMP_TOKEN" =~ ^[0-9a-f]{32}$ ]] || cleanup_status=1
            if [ "$cleanup_status" -eq 0 ]; then
              for child in live pr baseline preflight logs; do
                target="$TMP/$child"
                if [ -e "$target" ] || [ -L "$target" ]; then
                  rollout_safe_remove_tree "$target" "$TMP" "$TMP_TOKEN" "$LIVE_CHECKOUT" || cleanup_status=1
                fi
              done
            fi
            if [ "$cleanup_status" -eq 0 ]; then
              marker="$TMP/.rollout-owned-$TMP_TOKEN"
              case "$marker" in
                "$TMP"/.rollout-owned-*)
                  if [ -f "$marker" ] && [ ! -L "$marker" ] && \
                     [ "$(stat -c %u -- "$marker")" = 0 ] && \
                     [ "$(stat -c %a -- "$marker")" = 600 ] && \
                     [ "$(cat -- "$marker")" = "$TMP_TOKEN" ]; then
                    remaining=$(find "$TMP" -mindepth 1 -maxdepth 1 ! -name ".rollout-owned-$TMP_TOKEN" -print -quit) || cleanup_status=1
                    [ -z "$remaining" ] || cleanup_status=1
                    if [ "$cleanup_status" -eq 0 ]; then
                      unlink -- "$marker" || cleanup_status=1
                    fi
                  else
                    printf '%s\n' 'refusing to remove an invalid replay ownership marker' >&2
                    cleanup_status=1
                  fi
                  ;;
                *)
                  printf '%s\n' 'refusing to remove an ownership marker outside the replay tree' >&2
                  cleanup_status=1
                  ;;
              esac
            fi
          fi
          if [ "$cleanup_status" -eq 0 ]; then
            rmdir -- "$TMP" || cleanup_status=1
          fi
        else
          printf '%s\n' 'refusing to remove an unverified replay temp tree' >&2
          cleanup_status=1
        fi
        ;;
      *)
        printf '%s\n' 'refusing to remove a replay path outside its mktemp prefix' >&2
        cleanup_status=1
        ;;
    esac
  fi
  [ "$cleanup_status" -eq 0 ] || status=1
  exit "$status"
}
trap cleanup EXIT

die() {
  printf 'live Compose replay failed: %s\n' "$1" >&2
  exit 1
}

[[ "$OPS_SHA" =~ ^[0-9a-f]{40}$ ]] || die 'OPS_SHA must be a full commit ID'
live_git_root=$(git -C "$LIVE_CHECKOUT" rev-parse --show-toplevel 2>/dev/null) || die 'live checkout is unavailable'
[ "$live_git_root" = "$LIVE_CHECKOUT" ] || die 'live checkout path is not canonical'
git -C "$LIVE_CHECKOUT" cat-file -e "$OPS_SHA^{commit}" 2>/dev/null || die 'OPS_SHA is unavailable in the live checkout'
safe_remove_tree_source=$(git -C "$LIVE_CHECKOUT" show "$OPS_SHA:deploy/teagramd/rollout-runner/rollout-safe-remove-tree.sh" 2>/dev/null) || die 'OPS_SHA is missing the owned-tree cleanup helper'
source /dev/stdin <<< "$safe_remove_tree_source"
unset safe_remove_tree_source
declare -F rollout_mark_owned_dir >/dev/null && declare -F rollout_safe_remove_tree >/dev/null || die 'owned-tree cleanup helper is incomplete'

TMP=$(mktemp -d /tmp/main2612-replay.XXXXXXXX) || die 'cannot create the replay temp tree'
[[ "$TMP" =~ ^/tmp/main2612-replay\.[A-Za-z0-9]{8}$ ]] || die 'mktemp returned an unexpected path'
[ "$(stat -c %u -- "$TMP")" = 0 ] && [ "$(stat -c %a -- "$TMP")" = 700 ] || die 'replay temp tree is not root-owned mode 0700'
TMP_TOKEN=$(rollout_mark_owned_dir "$TMP") || die 'cannot mark the replay temp tree as owned'
mkdir -m 700 -- "$TMP/live" "$TMP/pr" "$TMP/baseline" "$TMP/preflight" "$TMP/logs"
mkdir -m 700 -p -- "$TMP/pr/deploy/teagramd/rollout-runner"
for runtime_file in rollout-runner.sh rollout-verifier.sh rollout-safe-remove-tree.sh blob-mode-state.py blob-transition-runner.py; do
  git -C "$LIVE_CHECKOUT" show "$OPS_SHA:deploy/teagramd/rollout-runner/$runtime_file" \
    > "$TMP/pr/deploy/teagramd/rollout-runner/$runtime_file" \
    2>"$TMP/logs/$runtime_file.stderr" || die "OPS_SHA is missing $runtime_file"
  chmod 600 -- "$TMP/pr/deploy/teagramd/rollout-runner/$runtime_file" || die "cannot protect $runtime_file"
done

exec 9>/tmp/teagram-deploy.lock
flock -x 9 || die 'cannot acquire the deployment read lock'

LIVE_SHA=$(git -C "$LIVE_CHECKOUT" rev-parse --verify HEAD 2>"$TMP/logs/live-revision.stderr") || die 'cannot read the live checkout revision'
[[ "$LIVE_SHA" =~ ^[0-9a-f]{40}$ ]] || die 'live checkout revision is invalid'

container_ids=$(docker ps --no-trunc -q \
  --filter label=com.docker.compose.project=teagram \
  --filter label=com.docker.compose.service=teagramd \
  2>"$TMP/logs/docker-ps.stderr") || die 'cannot enumerate live teagramd containers'
[ -n "$container_ids" ] || die 'no running teagramd containers were found'
mapfile -t containers <<< "$container_ids"

container_id=
live_compose=
for candidate_id in "${containers[@]}"; do
  [[ "$candidate_id" =~ ^[0-9a-f]{64}$ ]] || die 'a live teagramd container ID is invalid'
  actual_id=$(docker inspect --format '{{.Id}}' "$candidate_id" 2>>"$TMP/logs/docker-inspect.stderr") || die 'cannot inspect a live teagramd container'
  [ "$actual_id" = "$candidate_id" ] || die 'a live teagramd container ID changed during inspection'
  [ "$(docker inspect --format '{{.State.Running}}' "$candidate_id" 2>>"$TMP/logs/docker-inspect.stderr")" = true ] || die 'a teagramd container is not running'
  selected=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' \
    "$candidate_id" 2>>"$TMP/logs/docker-inspect.stderr") || die 'cannot read the live Compose selection'
  [ -n "$selected" ] || die 'a live teagramd container has no Compose selection label'
  if [ -z "$live_compose" ]; then
    live_compose=$selected
    container_id=$actual_id
  elif [ "$selected" != "$live_compose" ]; then
    die 'live teagramd containers have different Compose selections'
  fi
done

IFS=',' read -r -a selected_paths <<< "$live_compose"
[ "${#selected_paths[@]}" -ge 2 ] && [ "${#selected_paths[@]}" -le 3 ] || die 'live Compose selection has an unexpected number of files'
selected_rel=()
for selected_path in "${selected_paths[@]}"; do
  case "$selected_path" in
    "$LIVE_CHECKOUT"/*) relative=${selected_path#"$LIVE_CHECKOUT"/} ;;
    *) die 'live Compose selection points outside the deployment checkout' ;;
  esac
  case "$relative" in
    docker-compose.yml|docker-compose.local-blobs.yml|docker-compose.override.yml) ;;
    .rollout-compose.local-*.yml) [[ "$relative" =~ ^\.rollout-compose\.local-[A-Za-z0-9._-]+\.yml$ ]] || die 'local Compose artifact name is invalid' ;;
    *) die 'live Compose selection contains an unapproved file' ;;
  esac
  for prior in "${selected_rel[@]}"; do
    [ "$prior" != "$relative" ] || die 'live Compose selection contains a duplicate file'
  done
  selected_rel+=("$relative")
done

last_index=$((${#selected_rel[@]} - 1))
[ "${selected_rel[$last_index]}" = docker-compose.override.yml ] || die 'live Compose selection must end with the existing override'
case "${selected_rel[0]}" in
  docker-compose.yml)
    if [ "${#selected_rel[@]}" -eq 3 ]; then
      [ "${selected_rel[1]}" = docker-compose.local-blobs.yml ] || die 'tracked Compose selection has an unexpected order'
    elif [ "${#selected_rel[@]}" -ne 2 ]; then
      die 'tracked Compose selection has an unexpected shape'
    fi
    ;;
  .rollout-compose.local-*.yml)
    [ "${#selected_rel[@]}" -eq 2 ] || die 'local Compose selection has an unexpected shape'
    ;;
  *) die 'live Compose selection does not begin with an approved base file' ;;
esac

[[ "$container_id" =~ ^[0-9a-f]{64}$ ]] || die 'no valid live container ID was selected'
[ -f "$LIVE_CHECKOUT/.env" ] && [ ! -L "$LIVE_CHECKOUT/.env" ] || die 'live environment file is missing or symlinked'
[ -f "$LIVE_CHECKOUT/docker-compose.override.yml" ] && [ ! -L "$LIVE_CHECKOUT/docker-compose.override.yml" ] || die 'live Compose override is missing or symlinked'
[ "$(realpath -e -- "$LIVE_CHECKOUT/.env")" = "$LIVE_CHECKOUT/.env" ] || die 'live environment file path is not canonical'
[ "$(realpath -e -- "$LIVE_CHECKOUT/docker-compose.override.yml")" = "$LIVE_CHECKOUT/docker-compose.override.yml" ] || die 'live override path is not canonical'
[ "$(stat -c %u -- "$LIVE_CHECKOUT/.env")" = 0 ] && [ "$(stat -c %a -- "$LIVE_CHECKOUT/.env")" = 600 ] || die 'live environment file is not root-owned mode 0600'

for relative in "${selected_rel[@]}"; do
  if [ "$relative" = docker-compose.override.yml ]; then
    for tree in "$TMP/live" "$TMP/pr"; do
      cp -- "$LIVE_CHECKOUT/$relative" "$tree/$relative" || die 'cannot stage the live Compose override'
      chmod 600 -- "$tree/$relative" || die 'cannot protect the staged Compose override'
    done
  elif [[ "$relative" == .rollout-compose.local-*.yml ]]; then
    [ -f "$LIVE_CHECKOUT/$relative" ] && [ ! -L "$LIVE_CHECKOUT/$relative" ] || die 'live pinned Compose artifact is missing or symlinked'
    [ "$(realpath -e -- "$LIVE_CHECKOUT/$relative")" = "$LIVE_CHECKOUT/$relative" ] || die 'live pinned Compose artifact path is not canonical'
    for tree in "$TMP/live" "$TMP/pr"; do
      cp -- "$LIVE_CHECKOUT/$relative" "$tree/$relative" || die 'cannot stage the live pinned Compose artifact'
      chmod 600 -- "$tree/$relative" || die 'cannot protect the staged pinned Compose artifact'
    done
  else
    git -C "$LIVE_CHECKOUT" diff --quiet "$LIVE_SHA" -- "$relative" || die 'a selected tracked live Compose file is dirty'
    git -C "$LIVE_CHECKOUT" show "$LIVE_SHA:$relative" > "$TMP/live/$relative" 2>"$TMP/logs/live-compose.stderr" || die 'cannot stage a live committed Compose file'
    git -C "$LIVE_CHECKOUT" show "$OPS_SHA:$relative" > "$TMP/pr/$relative" 2>"$TMP/logs/pr-compose.stderr" || die 'OPS_SHA does not contain a selected Compose file'
    chmod 600 -- "$TMP/live/$relative" "$TMP/pr/$relative" || die 'cannot protect staged Compose files'
  fi
done

selected_env_sources=("$LIVE_CHECKOUT/.env")
if [ "${#selected_rel[@]}" -eq 2 ] && \
   [ "${selected_rel[0]}" = docker-compose.yml ] && \
   [ "${selected_rel[1]}" = docker-compose.override.yml ]; then
  previous_directory=$PWD
  cd "$LIVE_CHECKOUT" || die 'cannot enter the live checkout for authority validation'
  if ! deployment_env_json=$(python3 "$TMP/pr/deploy/teagramd/rollout-runner/blob-transition-runner.py" \
    deployment-env --checkout "$LIVE_CHECKOUT" --state-dir "$LIVE_CHECKOUT/.state/blob-mode" \
    --report-root /root --lock-path /tmp/teagram-deploy.lock \
    2>"$TMP/logs/deployment-env.stderr"); then
    cd "$previous_directory" || die 'cannot restore the replay working directory'
    die 'cannot validate the live S3 environment file selection'
  fi
  cd "$previous_directory" || die 'cannot restore the replay working directory'
  env_file_lines=$(jq -er '
    select(.outcome == "s3-accepted") |
    .env_files |
    if type == "array" and length == 2 and all(.[]; type == "string" and length > 0)
    then .[] else error("invalid environment file list") end
  ' <<< "$deployment_env_json") || die 'live S3 environment file list is invalid'
  mapfile -t selected_env_sources <<< "$env_file_lines"
  [ "${#selected_env_sources[@]}" -eq 2 ] && \
    [ "${selected_env_sources[0]}" = "$LIVE_CHECKOUT/.env" ] || die 'live S3 environment file list has an unexpected shape'
  case "${selected_env_sources[1]}" in
    "$LIVE_CHECKOUT/.env.rustfs-parked"|/opt/telegram-server/.env.rustfs-parked|/root/.env.rustfs-parked) ;;
    *) die 'live S3 environment file list contains an unapproved path' ;;
  esac
fi

selected_env_names=()
# The verifier enforces each tree's own .env path; keep these credential copies
# inside the private replay tree and unlink them after both snapshots.
for index in "${!selected_env_sources[@]}"; do
  env_source=${selected_env_sources[$index]}
  if [ "$index" -eq 0 ]; then
    env_name=.env
  else
    env_name=.env.rustfs-parked
  fi
  [ -f "$env_source" ] && [ ! -L "$env_source" ] || die 'live environment input is missing or symlinked'
  [ "$(realpath -e -- "$env_source")" = "$env_source" ] || die 'live environment input path is not canonical'
  [ "$(stat -c %u -- "$env_source")" = 0 ] && [ "$(stat -c %a -- "$env_source")" = 600 ] || die 'live environment input is not root-owned mode 0600'
  selected_env_names+=("$env_name")
  for tree in "$TMP/live" "$TMP/pr"; do
    cp -- "$env_source" "$tree/$env_name" || die 'cannot stage the live environment input'
    chmod 600 -- "$tree/$env_name" || die 'cannot protect the staged environment input'
  done
done

for tree in "$TMP/live" "$TMP/pr"; do
  cp -- "$LIVE_CHECKOUT/docker-compose.override.yml" "$tree/docker-compose.override.yml" || die 'cannot stage the live Compose override'
  chmod 600 -- "$tree/docker-compose.override.yml" || die 'cannot protect the staged Compose override'
done

compose_files_for_tree() {
  local tree=$1 relative result=
  for relative in "${selected_rel[@]}"; do
    if [ -n "$result" ]; then result="$result:"; fi
    result+="$tree/$relative"
  done
  printf '%s' "$result"
}

compose_env_files_for_tree() {
  local tree=$1 env_name result=
  for env_name in "${selected_env_names[@]}"; do
    if [ -n "$result" ]; then result+=,; fi
    result+="$tree/$env_name"
  done
  printf '%s' "$result"
}

readonly VERIFIER="$TMP/pr/deploy/teagramd/rollout-runner/rollout-verifier.sh"
readonly RUNNER="$TMP/pr/deploy/teagramd/rollout-runner/rollout-runner.sh"
[ -f "$VERIFIER" ] && [ ! -L "$VERIFIER" ] || die 'OPS_SHA verifier is missing or symlinked'
[ -f "$RUNNER" ] && [ ! -L "$RUNNER" ] || die 'OPS_SHA runner is missing or symlinked'

run_snapshot() {
  local tree=$1 evidence=$2 name=$3 compose_files=$4 compose_env_files=$5 previous_directory=$PWD
  local log="$TMP/logs/$name-snapshot.log"
  cd "$tree" || die 'cannot enter a Compose replay tree'
  if ! env ROLLOUT_CHECKOUT_PATH="$tree" ROLLOUT_COMPOSE_PROJECT_DIRECTORY="$LIVE_CHECKOUT" \
    ROLLOUT_COMPOSE_ENV_FILES="$compose_env_files" ROLLOUT_COMPOSE_SERVICE=teagramd \
    COMPOSE_PROJECT_NAME=teagram COMPOSE_FILE="$compose_files" \
    bash "$VERIFIER" snapshot "$container_id" "$evidence" "$name" \
      "$tree/.env" "$tree/docker-compose.override.yml" selected-compose-render >"$log" 2>&1; then
    cd "$previous_directory" || die 'cannot restore the replay working directory'
    die 'runner verifier rejected a replay snapshot'
  fi
  cd "$previous_directory" || die 'cannot restore the replay working directory'
}

run_snapshot "$TMP/live" "$TMP/baseline" baseline \
  "$(compose_files_for_tree "$TMP/live")" "$(compose_env_files_for_tree "$TMP/live")"
run_snapshot "$TMP/pr" "$TMP/preflight" preflight \
  "$(compose_files_for_tree "$TMP/pr")" "$(compose_env_files_for_tree "$TMP/pr")"

# The verifier requires each checkout's own .env path. Remove those private
# copies once both snapshots have reduced them to environment fingerprints.
for tree in "$TMP/live" "$TMP/pr"; do
  for env_name in "${selected_env_names[@]}"; do
    env_path="$tree/$env_name"
    case "$env_path" in
      "$TMP/live/.env"|"$TMP/live/.env.rustfs-parked"|"$TMP/pr/.env"|"$TMP/pr/.env.rustfs-parked") ;;
      *) die 'staged environment cleanup path is outside the replay trees' ;;
    esac
    [ -f "$env_path" ] && [ ! -L "$env_path" ] && \
      [ "$(stat -c %u -- "$env_path")" = 0 ] && [ "$(stat -c %a -- "$env_path")" = 600 ] || \
      die 'staged environment input is not a private regular file'
    unlink -- "$env_path" || die 'cannot remove a staged environment input'
  done
done

if ! env ROLLOUT_RUNNER_EVIDENCE_ROOT="$TMP" \
  bash "$RUNNER" compare-preflight "$TMP/baseline" "$TMP/preflight" \
  >"$TMP/logs/field-results.tsv" 2>"$TMP/logs/compare-preflight.stderr"; then
  die 'runner rejected the resolved Compose preflight'
fi
awk -F '\t' '
  NR == 1 { if ($1 != "field" || $2 != "result") failed = 1; next }
  NF != 2 || $2 != "pass" { failed = 1 }
  END { if (failed || NR < 2) exit 1 }
' "$TMP/logs/field-results.tsv" || die 'runner emitted invalid preflight result rows'
cat -- "$TMP/logs/field-results.tsv"
