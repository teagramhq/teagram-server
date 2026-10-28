#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly LIVE_CHECKOUT=/opt/teagram
OPS_SHA=${OPS_SHA:-}
CANDIDATE_SHA=
TMP=
TMP_TOKEN=
RESULT_ROWS=
BWRAP=
TIMEOUT=
REAL_DOCKER_SANDBOX_PATH=/sandbox-tools/real-docker
REAL_STAT_SANDBOX_PATH=/sandbox-tools/real-stat
unset COMPOSE_ENV_FILES

report_cleanup_failure() {
  local target=$1 status=$2 detail=$3
  if [ -n "$detail" ]; then
    printf 'live Compose replay cleanup failed for %s (exit %s): %s\n' "$target" "$status" "$detail" >&2
  else
    printf 'live Compose replay cleanup failed for %s (exit %s): owned-tree guard refused removal or recursive deletion failed without a diagnostic\n' \
      "$target" "$status" >&2
  fi
}

cleanup() {
  local status=$? cleanup_status=0 owner mode real_tmp marker child target remaining remove_status remove_error
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
              for child in candidate.git live pr baseline preflight logs sandbox trusted; do
                target="$TMP/$child"
                if [ -e "$target" ] || [ -L "$target" ]; then
                  if ! declare -F rollout_safe_remove_tree >/dev/null; then
                    report_cleanup_failure "$target" 127 'candidate cleanup helper is unavailable; guarded tree retained'
                    cleanup_status=1
                    break
                  fi
                  if remove_error=$(rollout_safe_remove_tree "$target" "$TMP" "$TMP_TOKEN" "$LIVE_CHECKOUT" 2>&1); then
                    :
                  else
                    remove_status=$?
                    report_cleanup_failure "$target" "$remove_status" "$remove_error"
                    cleanup_status=1
                  fi
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
            if ! rmdir -- "$TMP"; then
              report_cleanup_failure "$TMP" 1 'outer replay directory was not empty'
              cleanup_status=1
            fi
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
  if [ "$status" -eq 0 ] && [ "$cleanup_status" -eq 0 ] && [ -n "$RESULT_ROWS" ]; then
    printf '%s\n' "$RESULT_ROWS"
  fi
  exit "$status"
}

die() {
  printf 'live Compose replay failed: %s\n' "$1" >&2
  exit 1
}

resolve_candidate_sha() {
  case "$#" in
    0) CANDIDATE_SHA=$OPS_SHA ;;
    1) CANDIDATE_SHA=$1 ;;
    *) return 1 ;;
  esac
  [[ "$CANDIDATE_SHA" =~ ^[0-9a-f]{40}$ ]]
}

mark_replay_tree() {
  [ "$#" -eq 1 ] || return 64
  local directory=$1 token marker owner mode real_directory first_entry
  [[ "$directory" =~ ^/tmp/main2612-replay\.[A-Za-z0-9]{8}$ ]] || return 64
  [ "$(id -u)" = 0 ] || return 1
  [ -d "$directory" ] && [ ! -L "$directory" ] || return 1
  real_directory=$(realpath -e -- "$directory") || return 1
  [ "$real_directory" = "$directory" ] || return 1
  owner=$(stat -c %u -- "$directory") || return 1
  mode=$(stat -c %a -- "$directory") || return 1
  [ "$owner" = 0 ] && [ "$mode" = 700 ] || return 1
  first_entry=$(find "$directory" -mindepth 1 -maxdepth 1 -print -quit) || return 1
  [ -z "$first_entry" ] || return 1
  token=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n') || return 1
  [[ "$token" =~ ^[0-9a-f]{32}$ ]] || return 1
  marker="$directory/.rollout-owned-$token"
  (umask 077; set -o noclobber; printf '%s\n' "$token" > "$marker") || return 1
  printf '%s\n' "$token"
}

rollout_safe_remove_tree() {
  [ "$#" -eq 4 ] || return 64
  local target=$1 parent=$2 token=$3 live_checkout=$4 real_parent real_target owner mode marker
  [[ "$parent" =~ ^/tmp/main2612-replay\.[A-Za-z0-9]{8}$ ]] || return 64
  [[ "$token" =~ ^[0-9a-f]{32}$ ]] || return 64
  case "$target" in
    "$parent"/candidate.git|"$parent"/live|"$parent"/pr|"$parent"/baseline|"$parent"/preflight|"$parent"/logs|"$parent"/sandbox|"$parent"/trusted) ;;
    *) return 64 ;;
  esac
  [ "$live_checkout" = "$LIVE_CHECKOUT" ] || return 64
  [ "$(id -u)" = 0 ] && [ -d "$parent" ] && [ ! -L "$parent" ] || return 1
  real_parent=$(realpath -e -- "$parent") || return 1
  [ "$real_parent" = "$parent" ] || return 1
  [ "$(stat -c %u -- "$parent")" = 0 ] && [ "$(stat -c %a -- "$parent")" = 700 ] || return 1
  marker="$parent/.rollout-owned-$token"
  [ -f "$marker" ] && [ ! -L "$marker" ] && \
    [ "$(stat -c %u -- "$marker")" = 0 ] && [ "$(stat -c %a -- "$marker")" = 600 ] && \
    [ "$(cat -- "$marker")" = "$token" ] || return 1
  [ -d "$target" ] && [ ! -L "$target" ] || return 1
  real_target=$(realpath -e -- "$target") || return 1
  [ "$real_target" = "$target" ] || return 1
  owner=$(stat -c %u -- "$target") || return 1
  mode=$(stat -c %a -- "$target") || return 1
  [ "$owner" = 0 ] && [ "$mode" = 700 ] || return 1
  find "$target" -xdev -depth -delete
}

require_replay_sandbox() {
  local bwrap_path timeout_path
  bwrap_path=$(command -v bwrap) || die 'bubblewrap is required for candidate replay; no live inputs were read'
  bwrap_path=$(realpath -e -- "$bwrap_path") || die 'bubblewrap path is not canonical; no live inputs were read'
  case "$bwrap_path" in /usr/bin/*|/bin/*) ;; *) die 'bubblewrap is outside the system binary paths; no live inputs were read' ;; esac
  [ -x "$bwrap_path" ] || die 'bubblewrap is not executable; no live inputs were read'
  timeout_path=$(command -v timeout) || die 'timeout is required to bound candidate replay; no live inputs were read'
  timeout_path=$(realpath -e -- "$timeout_path") || die 'timeout path is not canonical; no live inputs were read'
  case "$timeout_path" in /usr/bin/*|/bin/*) ;; *) die 'timeout is outside the system binary paths; no live inputs were read' ;; esac
  if ! "$bwrap_path" --unshare-user --uid 0 --gid 0 --unshare-net --unshare-pid \
    --unshare-ipc --unshare-uts --cap-drop ALL --disable-userns --die-with-parent \
    --new-session --ro-bind /usr /usr --ro-bind-try /bin /bin \
    --ro-bind-try /lib /lib --ro-bind-try /lib64 /lib64 \
    --proc /proc --dev /dev --tmpfs /tmp -- /usr/bin/true >/dev/null 2>&1; then
    die 'bubblewrap user, network, and process namespaces are unavailable; no live inputs were read'
  fi
  BWRAP=$bwrap_path
  TIMEOUT=$timeout_path
}

run_candidate_sandbox() {
  [ "$#" -ge 2 ] || return 64
  local workdir=$1
  shift
  (
  ulimit -u 128 || exit 1
  exec 9>&-
  "$TIMEOUT" --signal=KILL 180 "$BWRAP" --unshare-user --uid 0 --gid 0 --unshare-net --unshare-pid \
    --unshare-ipc --unshare-uts --cap-drop ALL --disable-userns --die-with-parent \
    --new-session --clearenv \
    --ro-bind /usr /usr --ro-bind-try /bin /bin \
    --ro-bind-try /lib /lib --ro-bind-try /lib64 /lib64 \
    --dir /etc --size 16777216 --tmpfs /etc --dir /root --size 67108864 --tmpfs /root \
    --dir /root/baseline --dir /root/preflight \
    --dir /home --size 16777216 --tmpfs /home --dir /opt --size 16777216 --tmpfs /opt \
    --dir /var --size 16777216 --tmpfs /var --dir /run --size 16777216 --tmpfs /run \
    --size 67108864 --tmpfs /tmp \
    --proc /proc --dev /dev --dir /workspace --dir /workspace/live --dir /workspace/pr \
    --dir /workspace/fixtures --dir /workspace/media-keys --dir /sandbox-tools \
    --ro-bind "$TMP/live" /workspace/live --ro-bind "$TMP/pr" /workspace/pr \
    --ro-bind "$TMP/sandbox/bin" /sandbox-tools \
    --ro-bind "$TMP/sandbox/fixtures" /workspace/fixtures \
    --bind "$TMP/baseline" /root/baseline --bind "$TMP/preflight" /root/preflight \
    --bind "$TMP/sandbox/media-keys" /workspace/media-keys \
    --bind "$TMP/sandbox/lock" /tmp/teagram-deploy.lock \
    --setenv HOME /root --setenv PATH /sandbox-tools:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    --setenv LANG C.UTF-8 --setenv TZ UTC \
    --setenv REPLAY_CONTAINER_ID "$container_id" \
    --setenv REPLAY_REAL_DOCKER "$REAL_DOCKER_SANDBOX_PATH" \
    --setenv REPLAY_STAT_PATH "$REAL_STAT_SANDBOX_PATH" \
    --chdir "$workdir" "$@"
  )
}

make_sandbox_tools() {
  local docker_path stat_path
  docker_path=$(command -v docker) || die 'docker CLI is unavailable for isolated Compose rendering'
  docker_path=$(realpath -e -- "$docker_path") || die 'docker CLI path is not canonical'
  case "$docker_path" in /usr/bin/*|/usr/local/bin/*|/bin/*) ;; *) die 'docker CLI is outside the system binary paths required by the replay sandbox' ;; esac
  stat_path=$(command -v stat) || die 'stat is unavailable for isolated replay'
  stat_path=$(realpath -e -- "$stat_path") || die 'stat path is not canonical'
  case "$stat_path" in /usr/bin/*|/bin/*) ;; *) die 'stat is outside the system binary paths required by the replay sandbox' ;; esac
  cp -- "$docker_path" "$TMP/sandbox/bin/real-docker" || die 'cannot stage the Docker CLI for isolated Compose rendering'
  cp -- "$stat_path" "$TMP/sandbox/bin/real-stat" || die 'cannot stage stat for isolated replay'
  cat > "$TMP/sandbox/bin/docker" <<'EOF'
#!/bin/sh
set -eu
if [ "${1:-}" = inspect ]; then
  [ "$#" -eq 2 ] && [ "$2" = "$REPLAY_CONTAINER_ID" ] || exit 64
  cat "${REPLAY_INSPECT_PATH:-/workspace/fixtures/inspect.json}"
  exit
fi
if [ "${1:-}" = compose ]; then
  shift
  for argument do
    if [ "$argument" = config ]; then
      exec "$REPLAY_REAL_DOCKER" compose "$@"
    fi
  done
fi
printf '%s\n' 'candidate replay blocked a Docker operation' >&2
exit 64
EOF
  cat > "$TMP/sandbox/bin/stat" <<'EOF'
#!/bin/sh
set -eu
if [ "${1:-}" = -c ] && [ "$#" -eq 3 ]; then
  case "$2" in
    %u:%g)
      case "$3" in
        /workspace/media-keys|/workspace/media-keys/1.key) printf '%s\n' 65532:65532; exit 0 ;;
      esac
      ;;
  esac
fi
exec "$REPLAY_STAT_PATH" "$@"
EOF
  chmod 500 "$TMP/sandbox/bin/docker" "$TMP/sandbox/bin/stat" || die 'cannot protect isolated replay shims'
  chmod 500 "$TMP/sandbox/bin/real-docker" "$TMP/sandbox/bin/real-stat" || die 'cannot protect isolated system tools'
}

synthetic_value_for_env() {
  local name=$1 value=$2 tree=$3 nonempty=true
  case "$value" in "\"\""|"''") value= ;; esac
  [ -n "$value" ] || nonempty=false
  case "$name" in
    POSTGRES_PASSWORD) printf '%s' replay-postgres-password ;;
    TG_PUBLIC_REGISTRATION) case "$value" in invite|closed) printf '%s' "$value" ;; *) printf '%s' closed ;; esac ;;
    TG_PUBLIC_WEBSOCKET_LISTEN_ADDR) [ "$nonempty" = true ] && printf '%s' replay-listener || : ;;
    TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR) [ "$nonempty" = true ] && printf '%s' replay.invalid || : ;;
    TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS) [ "$nonempty" = true ] && printf '%s' https://replay.invalid || : ;;
    TG_WELCOME_CHANNEL) : ;;
    TG_LOG_LOGIN_CODES|TG_TURNSTILE_SIGNIN_ENABLED) case "$value" in true|false) printf '%s' "$value" ;; *) printf '%s' false ;; esac ;;
    TG_REPLICA_COUNT) printf '%s' 1 ;;
    TG_CLIENT_ADDR_TRUST) printf '%s' socket ;;
    TG_MEDIA_KEY_CURRENT) printf '%s' 1 ;;
    TG_MEDIA_KEY_DIR) printf '%s' /run/teagramd/media-keys ;;
    TEAGRAM_MEDIA_KEY_SOURCE_DIR) printf '%s' /workspace/media-keys ;;
    TEAGRAM_TURNSTILE_SITE_KEY) [ "$nonempty" = true ] && printf '%s' replay-site-key || : ;;
    TEAGRAM_TURNSTILE_SECRET_KEY_FILE) [ "$nonempty" = true ] && printf '%s/.secrets/teagramd-turnstile' "$tree" || : ;;
    TG_BLOB_S3_ACCESS_KEY_ID) [ "$nonempty" = true ] && printf '%s' 22222222222222222222 || : ;;
    TG_BLOB_S3_SECRET_ACCESS_KEY) [ "$nonempty" = true ] && printf '%s' bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb || : ;;
    RUSTFS_ROOT_ACCESS_KEY) printf '%s' 11111111111111111111 ;;
    RUSTFS_ROOT_SECRET_KEY) printf '%s' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
    *) [ "$nonempty" = true ] && printf 'replay-%s' "$name" || : ;;
  esac
}

write_synthetic_env() {
  [ "$#" -eq 3 ] || return 64
  local source=$1 destination=$2 tree=$3 line name value synthetic
  local -A seen=()
  : > "$destination" || return 1
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in ''|\#*|[[:space:]]\#*) continue ;; esac
    if [[ "$line" =~ ^[[:space:]]*(export[[:space:]]+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$ ]]; then
      name=${BASH_REMATCH[2]}
      value=${BASH_REMATCH[3]}
    else
      return 1
    fi
    [ -z "${seen[$name]+x}" ] || return 1
    seen[$name]=1
    synthetic=$(synthetic_value_for_env "$name" "$value" "$tree") || return 1
    printf '%s=%s\n' "$name" "$synthetic" >> "$destination" || return 1
  done < "$source"
  chmod 600 "$destination"
}

write_synthetic_authority() {
  [ "$#" -eq 1 ] || return 64
  local checkout=$1 state_dir first second
  state_dir="$checkout/.state/blob-mode"
  first="$state_dir/journal/0000000001.json"
  second="$state_dir/journal/0000000002.json"
  mkdir -p "$state_dir/journal" || return 1
  cat > "$first" <<'EOF'
{"backend":{"dir":"/var/lib/teagramd-blobs","kind":"local"},"evidence":{"report_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"generation":1,"outcome":"initial-local","published_at":"2026-10-01T00:00:00Z","schema":"teagram.blob-mode/v1","supersedes":null,"transition_id":"00000000-0000-4000-8000-000000000001","volumes":{"rustfsdata":null,"tgblobs":"teagram_tgblobs"}}
EOF
  cat > "$second" <<'EOF'
{"backend":{"bucket":"teagram","endpoint":"http://rustfs:9000","kind":"s3","prefix":"teagramd/"},"evidence":{"byte_total":1,"copy_passes":2,"destination_manifest_sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","object_count":1,"report_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","source_manifest_sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"},"generation":2,"outcome":"s3-accepted","published_at":"2026-10-02T00:00:00Z","schema":"teagram.blob-mode/v1","supersedes":"00000000-0000-4000-8000-000000000001","transition_id":"00000000-0000-4000-8000-000000000002","volumes":{"rustfsdata":"teagram_rustfsdata","tgblobs":"teagram_tgblobs"}}
EOF
  chmod 644 "$first" "$second" || return 1
  cp -- "$second" "$state_dir/mode.json" || return 1
  chmod 644 "$state_dir/mode.json" || return 1
  chmod 755 "$state_dir" "$state_dir/journal"
}

stage_synthetic_secrets() {
  [ "$#" -eq 1 ] || return 64
  local tree=$1 secret_dir
  secret_dir="$tree/.secrets"
  mkdir -m 700 "$secret_dir" || return 1
  printf '%s\n' bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
    > "$secret_dir/teagramd-blob-secret-key" || return 1
  printf '%s\n' replay-turnstile-secret \
    > "$secret_dir/teagramd-turnstile" || return 1
  chmod 444 "$secret_dir/teagramd-blob-secret-key" "$secret_dir/teagramd-turnstile" || return 1
  chmod 700 "$secret_dir"
}

if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then
  return 0
fi
trap cleanup EXIT

require_replay_sandbox

resolve_candidate_sha "$@" || die 'supply one full lowercase candidate commit ID (or use the selected main commit)'
live_git_root=$(git -C "$LIVE_CHECKOUT" rev-parse --show-toplevel 2>/dev/null) || die 'live checkout is unavailable'
[ "$live_git_root" = "$LIVE_CHECKOUT" ] || die 'live checkout path is not canonical'

TMP=$(mktemp -d /tmp/main2612-replay.XXXXXXXX) || die 'cannot create the replay temp tree'
[[ "$TMP" =~ ^/tmp/main2612-replay\.[A-Za-z0-9]{8}$ ]] || die 'mktemp returned an unexpected path'
[ "$(stat -c %u -- "$TMP")" = 0 ] && [ "$(stat -c %a -- "$TMP")" = 700 ] || die 'replay temp tree is not root-owned mode 0700'
TMP_TOKEN=$(mark_replay_tree "$TMP") || die 'cannot mark the replay temp tree as owned'
mkdir -m 700 -- "$TMP/live" "$TMP/pr" "$TMP/baseline" "$TMP/preflight" "$TMP/logs" "$TMP/sandbox" "$TMP/trusted"
mkdir -p -- "$TMP/pr/deploy/teagramd/rollout-runner"
mkdir -m 700 -- "$TMP/sandbox/bin" "$TMP/sandbox/fixtures" "$TMP/sandbox/media-keys"
: > "$TMP/sandbox/lock"
chmod 600 "$TMP/sandbox/lock"
printf '%s\n' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa > "$TMP/sandbox/media-keys/1.key"
chmod 700 "$TMP/sandbox/media-keys"
chmod 600 "$TMP/sandbox/media-keys/1.key"
chown 65532:65532 "$TMP/sandbox/media-keys" "$TMP/sandbox/media-keys/1.key" || die 'cannot prepare the synthetic media-key ownership fixture'
make_sandbox_tools

CANDIDATE_REMOTE=$(git -C "$LIVE_CHECKOUT" remote get-url origin 2>"$TMP/logs/origin.stderr") || die 'cannot read the live checkout origin'
mkdir -m 700 -- "$TMP/candidate.git"
env GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 \
  git init --bare --quiet "$TMP/candidate.git" >"$TMP/logs/candidate-init.log" 2>&1 || die 'cannot initialize the private candidate repository'
if ! env GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 \
  git -C "$TMP/candidate.git" fetch --quiet --no-tags --depth=1 "$CANDIDATE_REMOTE" "$CANDIDATE_SHA" \
    >"$TMP/logs/candidate-fetch.log" 2>&1; then
  die 'candidate commit is unavailable from the live checkout origin'
fi
fetched_sha=$(git -C "$TMP/candidate.git" rev-parse --verify 'FETCH_HEAD^{commit}' 2>"$TMP/logs/candidate-revision.stderr") || die 'cannot verify the fetched candidate commit'
[ "$fetched_sha" = "$CANDIDATE_SHA" ] || die 'the fetched candidate commit does not match the requested SHA'
git -C "$TMP/candidate.git" cat-file -e "$CANDIDATE_SHA^{commit}" 2>/dev/null || die 'candidate commit is unavailable in the private repository'
declare -F rollout_safe_remove_tree >/dev/null || die 'owned-tree cleanup helper is incomplete'

for runtime_file in rollout-runner.sh rollout-verifier.sh blob-mode-state.py blob-transition-runner.py; do
  git -C "$TMP/candidate.git" show "$CANDIDATE_SHA:deploy/teagramd/rollout-runner/$runtime_file" \
    > "$TMP/pr/deploy/teagramd/rollout-runner/$runtime_file" \
    2>"$TMP/logs/$runtime_file.stderr" || die "candidate commit is missing $runtime_file"
  chmod 600 -- "$TMP/pr/deploy/teagramd/rollout-runner/$runtime_file" || die "cannot protect $runtime_file"
done

exec 9>/tmp/teagram-deploy.lock
flock -x 9 || die 'cannot acquire the deployment read lock'

LIVE_SHA=$(git -C "$LIVE_CHECKOUT" rev-parse --verify HEAD 2>"$TMP/logs/live-revision.stderr") || die 'cannot read the live checkout revision'
[[ "$LIVE_SHA" =~ ^[0-9a-f]{40}$ ]] || die 'live checkout revision is invalid'
mkdir -p -- "$TMP/trusted/deploy/teagramd/rollout-runner"
for trusted_file in blob-mode-state.py blob-transition-runner.py; do
  git -C "$LIVE_CHECKOUT" show "$LIVE_SHA:deploy/teagramd/rollout-runner/$trusted_file" \
    > "$TMP/trusted/deploy/teagramd/rollout-runner/$trusted_file" \
    2>"$TMP/logs/trusted-$trusted_file.stderr" || die "live commit is missing trusted $trusted_file"
  chmod 600 -- "$TMP/trusted/deploy/teagramd/rollout-runner/$trusted_file" || die "cannot protect trusted $trusted_file"
done

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
    git -C "$TMP/candidate.git" show "$CANDIDATE_SHA:$relative" > "$TMP/pr/$relative" 2>"$TMP/logs/pr-compose.stderr" || die 'candidate commit does not contain a selected Compose file'
    chmod 600 -- "$TMP/live/$relative" "$TMP/pr/$relative" || die 'cannot protect staged Compose files'
  fi
done

selected_env_sources=("$LIVE_CHECKOUT/.env")
if [ "${#selected_rel[@]}" -eq 2 ] && \
   [ "${selected_rel[0]}" = docker-compose.yml ] && \
   [ "${selected_rel[1]}" = docker-compose.override.yml ]; then
  previous_directory=$PWD
  cd "$LIVE_CHECKOUT" || die 'cannot enter the live checkout for authority validation'
  if ! deployment_env_json=$(python3 "$TMP/trusted/deploy/teagramd/rollout-runner/blob-transition-runner.py" \
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
# Keep the verifier's .env paths valid, but replace every value with a synthetic
# value before any unmerged program can see the replay tree.
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
  for tree_name in live pr; do
    tree="$TMP/$tree_name"
    if ! write_synthetic_env "$env_source" "$tree/$env_name" "/workspace/$tree_name"; then
      die 'cannot create a synthetic replay environment file'
    fi
  done
done

for tree in "$TMP/live" "$TMP/pr"; do
  stage_synthetic_secrets "$tree" || die 'cannot stage synthetic replay secret files'
done

if [ "${#selected_env_names[@]}" -eq 2 ]; then
  write_synthetic_authority "$TMP/live" || die 'cannot stage synthetic replay authority state'
  candidate_deployment_env=$(run_candidate_sandbox /workspace/live /usr/bin/env BLOB_TRANSITION_TEST_MODE=1 bash -c '
    exec 9>/tmp/teagram-deploy.lock
    flock -x 9
    exec python3 /workspace/pr/deploy/teagramd/rollout-runner/blob-transition-runner.py \
      deployment-env --checkout /workspace/live --state-dir /workspace/live/.state/blob-mode \
      --report-root /root/reports --lock-path /tmp/teagram-deploy.lock
  ' 2>"$TMP/logs/candidate-deployment-env.stderr") || die 'candidate transition environment validation failed in the replay sandbox'
  jq -e '.outcome == "s3-accepted" and .env_files == ["/workspace/live/.env", "/workspace/live/.env.rustfs-parked"]' \
    <<< "$candidate_deployment_env" >/dev/null || die 'candidate transition environment output escaped the synthetic replay inputs'
fi

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

write_inspect_fixture() {
  [ "$#" -eq 1 ] || return 64
  local checkout_path="/workspace/$1"
  docker inspect "$container_id" 2>"$TMP/logs/docker-sanitized-inspect.stderr" | jq -cer \
    --arg checkout "$checkout_path" '
    def replay_value($name; $value):
      if $value == "" then ""
      elif $name == "TG_PUBLIC_REGISTRATION" then (if $value == "invite" then "invite" else "closed" end)
      elif $name == "TG_PUBLIC_WEBSOCKET_LISTEN_ADDR" then "replay-listener"
      elif $name == "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR" then "replay.invalid"
      elif $name == "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS" then "https://replay.invalid"
      elif $name == "TG_WELCOME_CHANNEL" then ""
      elif $name == "TG_LOG_LOGIN_CODES" then (if $value == "true" or $value == "false" then $value else "false" end)
      elif $name == "TG_REPLICA_COUNT" then (if ($value | test("^[0-9]{1,3}$")) then $value else "1" end)
      elif $name == "TG_CLIENT_ADDR_TRUST" then "socket"
      elif $name == "TG_TURNSTILE_SITE_KEY" then "replay-site-key"
      elif $name == "TG_TURNSTILE_SECRET_KEY_FILE" then "/run/secrets/teagramd-turnstile"
      elif $name == "TG_TURNSTILE_SIGNIN_ENABLED" then (if $value == "true" or $value == "false" then $value else "false" end)
      elif $name == "TG_MEDIA_KEY_DIR" then "/run/teagramd/media-keys"
      elif $name == "TG_MEDIA_KEY_CURRENT" then "1"
      elif $name == "TG_BLOB_S3_ACCESS_KEY_ID" then "22222222222222222222"
      elif $name == "TG_BLOB_S3_SECRET_ACCESS_KEY" then "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
      elif $name == "TG_BLOB_S3_SECRET_ACCESS_KEY_FILE" then "/run/secrets/teagramd-blob-secret-key"
      else "replay-" + $name end;
    def redacted_env:
      map(
        if type != "string" or (index("=") == null) then . else
          . as $entry
          | ($entry | index("=")) as $separator
          | $entry[:$separator] as $name
          | $entry[($separator + 1):] as $value
          | $name + "=" + replay_value($name; $value)
        end
      );
    .[0] as $container
    | [{
        Id:$container.Id,
        Image:$container.Image,
        State:{Status:$container.State.Status,ExitCode:$container.State.ExitCode,
          StartedAt:$container.State.StartedAt,FinishedAt:$container.State.FinishedAt},
        Config:{Env:($container.Config.Env // [] | redacted_env),StopTimeout:$container.Config.StopTimeout},
        HostConfig:{
          LogConfig:{Type:($container.HostConfig.LogConfig.Type // ""),Config:(
            ($container.HostConfig.LogConfig.Config // {})
            | with_entries(select(.key | IN("syslog-address","syslog-facility","syslog-format","tag")))
          )},
          PortBindings:($container.HostConfig.PortBindings // {})
        },
        Mounts:[
          ($container.Mounts // []) | to_entries[] as $entry | $entry.value as $mount
          | {
              Type:($mount.Type // ""),Name:($mount.Name // ""),
              Source:(
                if $mount.Destination == "/run/teagramd/blob-mode" then $checkout + "/.state/blob-mode"
                elif $mount.Destination == "/run/secrets/teagramd-turnstile" and $mount.Source != "/dev/null" then $checkout + "/.secrets/teagramd-turnstile"
                elif $mount.Destination == "/run/teagramd/media-keys" then "/workspace/media-keys"
                elif $mount.Type == "bind" then "/workspace/mounts/" + ($entry.key | tostring)
                else ($mount.Source // "") end
              ),
              Destination:($mount.Destination // ""),Mode:($mount.Mode // ""),
              RW:($mount.RW // false),Propagation:($mount.Propagation // "")
            }
        ]
      }]
    '
}

readonly VERIFIER="$TMP/pr/deploy/teagramd/rollout-runner/rollout-verifier.sh"
readonly RUNNER="$TMP/pr/deploy/teagramd/rollout-runner/rollout-runner.sh"
[ -f "$VERIFIER" ] && [ ! -L "$VERIFIER" ] || die 'candidate verifier is missing or symlinked'
[ -f "$RUNNER" ] && [ ! -L "$RUNNER" ] || die 'candidate runner is missing or symlinked'
[ -f "$RUNNER" ] && [ ! -L "$RUNNER" ] || die 'candidate runner is missing or symlinked'

run_snapshot() {
  local tree_name=$1 name=$2
  local tree="/workspace/$tree_name" evidence="/root/$name" compose_files compose_env_files log="$TMP/logs/$name-snapshot.log"
  compose_files=$(compose_files_for_tree "$tree")
  compose_env_files=$(compose_env_files_for_tree "$tree")
  write_inspect_fixture "$tree_name" > "$TMP/sandbox/fixtures/$tree_name.inspect.json" || die 'cannot create a redacted container inspection fixture'
  chmod 600 "$TMP/sandbox/fixtures/$tree_name.inspect.json"
  if ! run_candidate_sandbox "$tree" /usr/bin/env \
    ROLLOUT_CHECKOUT_PATH="$tree" ROLLOUT_COMPOSE_PROJECT_DIRECTORY=/workspace/live \
    ROLLOUT_COMPOSE_ENV_FILES="$compose_env_files" COMPOSE_ENV_FILES="$compose_env_files" \
    ROLLOUT_COMPOSE_SERVICE=teagramd COMPOSE_PROJECT_NAME=teagram COMPOSE_FILE="$compose_files" \
    REPLAY_INSPECT_PATH="/workspace/fixtures/$tree_name.inspect.json" \
    bash "$tree/deploy/teagramd/rollout-runner/rollout-verifier.sh" snapshot "$container_id" "$evidence" "$name" \
      "$tree/.env" "$tree/docker-compose.override.yml" selected-compose-render >"$log" 2>&1; then
    die 'runner verifier rejected a replay snapshot'
  fi
}

run_snapshot live baseline
run_snapshot pr preflight

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

if ! run_candidate_sandbox /workspace/pr /usr/bin/env \
  ROLLOUT_RUNNER_EVIDENCE_ROOT=/root \
  bash /workspace/pr/deploy/teagramd/rollout-runner/rollout-runner.sh \
    compare-preflight /root/baseline /root/preflight \
  >"$TMP/logs/field-results.tsv" 2>"$TMP/logs/compare-preflight.stderr"; then
  die 'runner rejected the resolved Compose preflight'
fi
awk -F '\t' '
  NR == 1 { if ($1 != "field" || $2 != "result") failed = 1; next }
  NF != 2 || $2 != "pass" { failed = 1 }
  END { if (failed || NR < 2) exit 1 }
' "$TMP/logs/field-results.tsv" || die 'runner emitted invalid preflight result rows'
RESULT_ROWS=$(<"$TMP/logs/field-results.tsv")
