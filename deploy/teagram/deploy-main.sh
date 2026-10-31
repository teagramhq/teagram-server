#!/usr/bin/env bash
# Deploys DEPLOY_SHA, a commit on origin/main whose CI passed, to the
# teagram-server LXC through the rollout runner. Runs as root on the LXC: from
# the Deploy. workflow's forced-command key, or by hand with
# `ssh teagram-server 'DEPLOY_SHA=<full sha> bash -s' < deploy/teagram/deploy-main.sh`.
# Stdin may be this script, so every docker compose call reads /dev/null. The
# same tested commit refreshes the independent link selector after teagramd.
# DEPLOY_DIR and DEPLOY_ROOT exist for the fixture test only.
set -eu

normalize_tracked_file_modes() {
  local entry metadata mode path desired current links parent changed=0 complete=0
  while IFS= read -r -d '' entry; do
    if [[ "$entry" == __TRACKED_MODE_SCAN_COMPLETE__ ]]; then
      complete=1
      break
    fi
    metadata=${entry%%$'\t'*}
    path=${entry#*$'\t'}
    mode=${metadata%% *}
    case "$mode" in
      100644) desired=644 ;;
      100755) desired=755 ;;
      *) continue ;;
    esac

    parent=$path
    while [[ "$parent" == */* ]]; do
      parent=${parent%/*}
      if [[ -L "$parent" ]]; then
        printf 'tracked file has a symlink parent: %q\n' "$path" >&2
        return 1
      fi
    done
    if [[ -L "$path" || ! -f "$path" ]]; then
      printf 'tracked file is missing or not regular: %q\n' "$path" >&2
      return 1
    fi
    links=$(stat -c '%h' -- "$path") || return 1
    if [[ "$links" != 1 ]]; then
      printf 'tracked file shares an inode; refusing to change mode: %q\n' "$path" >&2
      return 1
    fi
    current=$(stat -c '%a' -- "$path") || return 1
    if [[ "$current" != "$desired" ]]; then
      chmod "$desired" -- "$path" || return 1
      changed=$((changed + 1))
    fi
    current=$(stat -c '%a' -- "$path") || return 1
    if [[ "$current" != "$desired" ]]; then
      printf 'tracked file mode mismatch after chmod: %q expected=%s actual=%s\n' \
        "$path" "$desired" "$current" >&2
      return 1
    fi
  done < <(git ls-tree -rz --full-tree HEAD && printf '__TRACKED_MODE_SCAN_COMPLETE__\0')

  if [[ "$complete" != 1 ]]; then
    echo 'cannot enumerate tracked file modes from HEAD' >&2
    return 1
  fi
  printf 'tracked file modes: normalized=%s mismatches=0\n' "$changed"
}

install_teagram_logs_command() {
  local sha=$1 target=/usr/local/sbin/teagram-logs
  local target_dir temp backup

  if [[ -n ${DEPLOY_DIR:-} ]]; then
    target="$R/usr/local/sbin/teagram-logs"
  fi

  # Older deployable commits predate the tracked log reader.
  if ! git cat-file -e "$sha:deploy/teagram/teagram-logs.sh" 2>/dev/null; then
    return 0
  fi

  target_dir=${target%/*}
  if [[ "$target_dir" == "$target" || ! -d "$target_dir" ]]; then
    printf 'teagram-logs target directory is missing: %s\n' "$target_dir" >&2
    return 1
  fi
  if [[ -L "$target" || ( -e "$target" && ! -f "$target" ) ]]; then
    printf 'teagram-logs target is not a regular file: %s\n' "$target" >&2
    return 1
  fi

  temp=$(mktemp "$target_dir/.teagram-logs.XXXXXX") || return 1
  if ! git show "$sha:deploy/teagram/teagram-logs.sh" > "$temp"; then
    rm -f "$temp"
    return 1
  fi
  if ! bash -n "$temp"; then
    rm -f "$temp"
    printf 'teagram-logs script from %s failed bash -n\n' "$sha" >&2
    return 1
  fi
  if ! chmod 0755 "$temp"; then
    rm -f "$temp"
    return 1
  fi

  if [[ -f "$target" ]] && ! cmp -s "$temp" "$target"; then
    backup="$R/teagram-logs-before-${sha:0:7}"
    if [[ ! -e "$backup" ]]; then
      if ! cp -p "$target" "$backup"; then
        rm -f "$temp"
        return 1
      fi
      chmod 0600 "$backup"
      printf 'saved previous teagram-logs command to %s\n' "$backup"
    fi
  fi

  if ! mv -f "$temp" "$target"; then
    rm -f "$temp"
    return 1
  fi
  printf 'installed teagram-logs command from %s\n' "${sha:0:7}"
}

install_teagram_ops_command() {
  local sha=$1 target=/usr/local/sbin/teagram-ops
  local target_dir temp backup

  if [[ -n ${DEPLOY_DIR:-} ]]; then
    target="$R/usr/local/sbin/teagram-ops"
  fi

  # Older deployable commits predate the reviewed operations runner.
  if ! git cat-file -e "$sha:deploy/teagram/teagram-ops.sh" 2>/dev/null; then
    return 0
  fi

  target_dir=${target%/*}
  if [[ "$target_dir" == "$target" || ! -d "$target_dir" ]]; then
    printf 'teagram-ops target directory is missing: %s\n' "$target_dir" >&2
    return 1
  fi
  if [[ -L "$target" || ( -e "$target" && ! -f "$target" ) ]]; then
    printf 'teagram-ops target is not a regular file: %s\n' "$target" >&2
    return 1
  fi

  temp=$(mktemp "$target_dir/.teagram-ops.XXXXXX") || return 1
  if ! git show "$sha:deploy/teagram/teagram-ops.sh" > "$temp"; then
    rm -f "$temp"
    return 1
  fi
  if ! sh -n "$temp"; then
    rm -f "$temp"
    printf 'teagram-ops script from %s failed sh -n\n' "$sha" >&2
    return 1
  fi
  if ! chmod 0700 "$temp"; then
    rm -f "$temp"
    return 1
  fi

  if [[ -f "$target" ]] && ! cmp -s "$temp" "$target"; then
    backup="$R/teagram-ops-before-${sha:0:7}"
    if [[ ! -e "$backup" ]]; then
      if ! cp -p "$target" "$backup"; then
        rm -f "$temp"
        return 1
      fi
      chmod 0600 "$backup"
      printf 'saved previous teagram-ops command to %s\n' "$backup"
    fi
  fi

  if ! mv -f "$temp" "$target"; then
    rm -f "$temp"
    return 1
  fi
  printf 'installed teagram-ops command from %s\n' "${sha:0:7}"
}

restore_previous_teagram_syslog_config() {
  local backup=$1 target=$2
  if ! cp -p "$backup" "$target" || ! chmod 0644 "$target"; then
    printf 'could not restore the previous syslog configuration from %s\n' "$backup" >&2
    return 1
  fi
  if ! rc-service syslog restart; then
    printf 'previous syslog configuration was restored but syslog did not restart\n' >&2
    return 1
  fi
  if ! rc-service syslog status >/dev/null; then
    printf 'previous syslog configuration was restored but syslog is not running\n' >&2
    return 1
  fi
}

install_teagram_syslog_config() {
  local sha=$1 target=/etc/conf.d/syslog
  local target_dir temp backup changed=0

  if [[ -n ${DEPLOY_DIR:-} ]]; then
    target="$R/etc/conf.d/syslog"
  fi

  # Older deployable commits predate persistent teagramd logging.
  if ! git cat-file -e "$sha:deploy/teagram/syslog.conf" 2>/dev/null; then
    return 0
  fi

  target_dir=${target%/*}
  if [[ -L "$target" || ! -f "$target" ]]; then
    printf 'syslog configuration is missing, symlinked, or not a regular file: %s\n' "$target" >&2
    return 1
  fi

  temp=$(mktemp "$target_dir/.teagram-syslog.XXXXXX") || return 1
  if ! git show "$sha:deploy/teagram/syslog.conf" > "$temp"; then
    rm -f "$temp"
    return 1
  fi
  if ! bash -n "$temp"; then
    rm -f "$temp"
    printf 'syslog configuration from %s failed bash -n\n' "$sha" >&2
    return 1
  fi

  if ! cmp -s "$temp" "$target"; then
    if [[ "$(<"$target")" != 'SYSLOGD_OPTS="-t"' ]]; then
      rm -f "$temp"
      printf 'syslog configuration drifted; refusing to replace %s\n' "$target" >&2
      return 1
    fi

    backup="$R/teagram-syslog-before-${sha:0:7}"
    if [[ ! -e "$backup" ]]; then
      if ! cp -p "$target" "$backup"; then
        rm -f "$temp"
        return 1
      fi
      chmod 0600 "$backup"
      printf 'saved previous syslog configuration to %s\n' "$backup"
    fi

    if ! chmod 0644 "$temp" || ! mv -f "$temp" "$target"; then
      rm -f "$temp"
      return 1
    fi
    changed=1

    if [[ -z ${DEPLOY_DIR:-} ]] && ! rc-service syslog restart; then
      restore_previous_teagram_syslog_config "$backup" "$target" || return 1
      printf 'syslog restart failed; restored previous configuration from %s\n' "$backup" >&2
      return 1
    fi
  else
    rm -f "$temp"
  fi

  if [[ -z ${DEPLOY_DIR:-} ]]; then
    if ! rc-service syslog status >/dev/null; then
      if [[ "$changed" == 1 ]]; then
        restore_previous_teagram_syslog_config "$backup" "$target" || return 1
      fi
      printf 'syslog service is not running after configuration update\n' >&2
      return 1
    fi
    if [[ ! -S /dev/log ]]; then
      if [[ "$changed" == 1 ]]; then
        restore_previous_teagram_syslog_config "$backup" "$target" || return 1
      fi
      printf 'syslog socket is missing: /dev/log\n' >&2
      return 1
    fi
    if ! ps -o args | awk '$0 ~ /[s]yslogd/ && $0 ~ /-s 524288/ && $0 ~ /-b 15/ { active=1 } END { exit !active }'; then
      if [[ "$changed" == 1 ]]; then
        restore_previous_teagram_syslog_config "$backup" "$target" || return 1
      fi
      printf 'syslog is not running with the configured rotation cap\n' >&2
      return 1
    fi
  fi

  printf 'installed teagram syslog rotation config from %s\n' "${sha:0:7}"
}

load_link_edge_invite_targets() {
  local line='' configured_web_target='' configured_desktop_target=''
  if [[ -r "$LIVE_CHECKOUT/.env" ]]; then
    while IFS= read -r line || [[ -n "$line" ]]; do
      case "$line" in
        TG_INVITE_WEB_SIGNUP_URL=*) configured_web_target=${line#*=} ;;
        TG_INVITE_DESKTOP_URL=*) configured_desktop_target=${line#*=} ;;
      esac
    done < "$LIVE_CHECKOUT/.env"
  fi
  TG_INVITE_WEB_SIGNUP_URL=${TG_INVITE_WEB_SIGNUP_URL:-$configured_web_target}
  if [[ -z "$TG_INVITE_WEB_SIGNUP_URL" ]]; then
    echo 'TG_INVITE_WEB_SIGNUP_URL is missing from the deployment environment and .env' >&2
    return 1
  fi
  TG_INVITE_DESKTOP_URL=${TG_INVITE_DESKTOP_URL:-$configured_desktop_target}
  export TG_INVITE_WEB_SIGNUP_URL TG_INVITE_DESKTOP_URL
}

SHA=${DEPLOY_SHA:-}
case "$SHA" in *[!0-9a-f]*|"") echo "DEPLOY_SHA must be a full 40-hex commit SHA"; exit 1;; esac
[ ${#SHA} = 40 ] || { echo "DEPLOY_SHA must be a full 40-hex commit SHA"; exit 1; }
R=${DEPLOY_ROOT:-/root}
exec 9>/tmp/teagram-deploy.lock; flock -x 9   # wait out a running deploy; released before the runner takes the same lock
cd "${DEPLOY_DIR:-/opt/teagram}"
normalize_tracked_file_modes || { echo "cannot normalize tracked checkout modes"; exit 1; }
git fetch -q origin
if ! git merge-base --is-ancestor "$SHA" origin/main 2>/dev/null; then
  echo "$SHA is not on origin/main; refusing"; exit 1
fi
PREV=$(git rev-parse HEAD); S=${SHA:0:7}; TS=$(date -u +%Y%m%dT%H%M%SZ)
if [ "$PREV" != "$SHA" ] && git merge-base --is-ancestor "$SHA" "$PREV"; then
  echo "live is newer ($PREV), skipped $SHA"; exit 0
fi
C=teagram-teagramd-1
LIVE_CHECKOUT=$(pwd -P)
load_link_edge_invite_targets || exit 1
unset COMPOSE_ENV_FILES
# Stage the candidate's validator before preflight. The selected SHA is on
# origin/main, and the same files are used by the rollout after the backup.
# shellcheck disable=SC2174 # only the runner directory is new
mkdir -m 700 -p "$R/teagramd-rollout-runner"
for f in rollout-runner.sh rollout-verifier.sh schema-result-gate.sh schema-result-gate.py blob-mode-state.py blob-transition-runner.py qualify-rustfs-transition.py stack-move-runner.py public-websocket-check.py; do
  git show "$SHA:deploy/teagramd/rollout-runner/$f" > "$R/teagramd-rollout-runner/$f"; chmod 600 "$R/teagramd-rollout-runner/$f"
done
LIVE_CF=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' $C | tr , :)
S3_ENV_FILES=()
case "$LIVE_CF" in
  "$LIVE_CHECKOUT"/.rollout-compose.local-*.yml:"$LIVE_CHECKOUT"/docker-compose.override.yml) ;;
  "$LIVE_CHECKOUT"/docker-compose.yml:"$LIVE_CHECKOUT"/docker-compose.local-blobs.yml:"$LIVE_CHECKOUT"/docker-compose.override.yml) ;;
  "$LIVE_CHECKOUT"/docker-compose.yml:"$LIVE_CHECKOUT"/docker-compose.override.yml)
    if ! authority_json=$(python3 "$R/teagramd-rollout-runner/blob-transition-runner.py" deployment-env \
      --checkout "$LIVE_CHECKOUT" --state-dir "$LIVE_CHECKOUT/.state/blob-mode" \
      --report-root "$R" --lock-path /tmp/teagram-deploy.lock); then
      echo 'live S3 Compose has no valid s3-accepted authority or credentials; refusing' >&2
      exit 1
    fi
    if ! env_file_lines=$(jq -er '
      select(.outcome == "s3-accepted") |
      .env_files |
      if type == "array" and length == 2 and all(.[]; type == "string" and length > 0)
      then .[] else error("invalid environment file list") end
    ' <<< "$authority_json"); then
      echo 'live S3 Compose validator returned an invalid environment file list; refusing' >&2
      exit 1
    fi
    mapfile -t S3_ENV_FILES <<< "$env_file_lines"
    if [[ "${#S3_ENV_FILES[@]}" != 2 || "${S3_ENV_FILES[0]}" != "$LIVE_CHECKOUT/.env" ]]; then
      echo 'live S3 Compose validator returned unexpected environment files; refusing' >&2
      exit 1
    fi
    legacy_parked="$(dirname "$LIVE_CHECKOUT")/telegram-server/.env.rustfs-parked"
    case "${S3_ENV_FILES[1]}" in
      "$LIVE_CHECKOUT/.env.rustfs-parked"|"$legacy_parked"|"$R/.env.rustfs-parked") ;;
      *) echo 'live S3 Compose validator returned an unexpected parked credential path; refusing' >&2; exit 1 ;;
    esac
    export COMPOSE_ENV_FILES="${S3_ENV_FILES[0]},${S3_ENV_FILES[1]}"
    ;;
  *) echo "live teagramd not on an approved local compose ($LIVE_CF); refusing"; exit 1 ;;
esac
export COMPOSE_FILE="$LIVE_CF"   # carry forward the pinned local-blob files live runs from
export COMPOSE_PROFILES=public
export ROLLOUT_RUNNER_PUBLIC_WEBSOCKET=1
unset ROLLOUT_RUNNER_ADDED_ENV ROLLOUT_RUNNER_CHANGED_ENV ROLLOUT_RUNNER_PREVIOUS_ENV_FILE
docker_compose() {
  if [[ "${#S3_ENV_FILES[@]}" == 2 ]]; then
    docker compose --env-file "${S3_ENV_FILES[0]}" --env-file "${S3_ENV_FILES[1]}" "$@"
  else
    docker compose "$@"
  fi
}
# Compose resolves an extended service before applying a later override file.
# Carry the resolved base advertise address through interpolation so the public
# replicas receive the same value as the base service.
resolve_public_advertise_addr() {
  local rendered advertise_addr
  rendered=$(docker_compose --profile public config --format json </dev/null) || {
    echo 'cannot render public Compose config to resolve the base advertise address' >&2
    return 1
  }
  advertise_addr=$(jq -er '
    (.services.teagramd.environment.TG_ADVERTISE_ADDR // "") |
    if type == "string" then . else error("TG_ADVERTISE_ADDR must be a string") end
  ' <<< "$rendered") || {
    echo 'cannot read the base advertise address from rendered Compose config' >&2
    return 1
  }
  if [[ "$advertise_addr" == *$'\n'* || "$advertise_addr" == *$'\r'* ]]; then
    echo 'base advertise address contains a line break' >&2
    return 1
  fi
  export TG_ADVERTISE_ADDR="$advertise_addr"
}
resolve_public_advertise_addr || exit 1
# The same-SHA shortcut must prove that both the rendered public boundary and
# the running containers still match the reviewed configuration.
PUBLIC_RENDERED_COMPOSE=
public_route_topology_is_exact() {
  PUBLIC_RENDERED_COMPOSE=$(docker_compose --profile public config --format json </dev/null) || return 1
  printf '%s' "$PUBLIC_RENDERED_COMPOSE" | python3 "$R/teagramd-rollout-runner/public-websocket-check.py" \
    --haproxy-config "$LIVE_CHECKOUT/deploy/teagramd/haproxy-public-websocket.cfg" >/dev/null
}
public_base_listener_budget_is_single() {
  printf '%s' "$PUBLIC_RENDERED_COMPOSE" | jq -e \
    '.services.teagramd.environment.TG_REPLICA_COUNT == "1"' >/dev/null || return 1
  docker inspect --format '{{json .Config.Env}}' "$C" 2>/dev/null | jq -e '
    if type != "array" then false else
      [.[] | select(type == "string" and startswith("TG_REPLICA_COUNT="))] as $counts
      | if ($counts | length) == 0 then true
        elif ($counts | length) == 1 then $counts[0] == "TG_REPLICA_COUNT=1"
        else false end
    end
  ' >/dev/null
}
public_route_services_running() {
  local service id inspect_json state health ports haproxy_config
  haproxy_config="$LIVE_CHECKOUT/deploy/teagramd/haproxy-public-websocket.cfg"
  for service in public-haproxy teagramd-public-1 teagramd-public-2; do
    id=$(docker_compose ps -q "$service" </dev/null 2>/dev/null) || return 1
    [[ "$id" =~ ^[0-9a-f]{12,64}$ ]] || return 1
    inspect_json=$(docker inspect "$id" 2>/dev/null) || return 1
    state=$(printf '%s' "$inspect_json" | jq -er '.[0].State.Status') || return 1
    [ "$state" = running ] || return 1
    if [[ "$service" == public-haproxy ]]; then
      health=$(printf '%s' "$inspect_json" | jq -er '.[0].State.Health.Status // "missing"') || return 1
      [ "$health" = healthy ] || return 1
      ports=$(printf '%s' "$inspect_json" | jq -ce '
        [.[0].NetworkSettings.Ports // {} | to_entries[] | .key as $port | .value[]? | {port:$port,host_ip:.HostIp,host_port:.HostPort}] | sort_by(.port)
      ') || return 1
      [ "$ports" = '[{"port":"2446/tcp","host_ip":"127.0.0.1","host_port":"2446"}]' ] || return 1
      ports=$(printf '%s' "$inspect_json" | jq -ce '
        [.[0].HostConfig.PortBindings // {} | to_entries[] | .key as $port | .value[]? | {port:$port,host_ip:.HostIp,host_port:.HostPort}] | sort_by(.port)
      ') || return 1
      [ "$ports" = '[{"port":"2446/tcp","host_ip":"127.0.0.1","host_port":"2446"}]' ] || return 1
      if ! printf '%s' "$inspect_json" | jq -e --arg source "$haproxy_config" '
        [.[0].Mounts[]? | select(.Destination == "/usr/local/etc/haproxy/haproxy.cfg")] as $mounts |
        ($mounts | length) == 1 and
        $mounts[0].Type == "bind" and
        $mounts[0].Source == $source and
        $mounts[0].RW == false
      ' >/dev/null; then
        return 1
      fi
      if ! docker exec "$id" cat /usr/local/etc/haproxy/haproxy.cfg 2>/dev/null | cmp -s - "$haproxy_config"; then
        return 1
      fi
    else
      if ! { printf '%s\n' "$inspect_json" "$PUBLIC_RENDERED_COMPOSE"; } | jq -se --arg service "$service" '
        .[0] as $inspect | .[1] as $compose |
        $inspect[0].Config.Env as $env |
        $compose.services[$service].environment as $expected |
        all([
          "TG_CLIENT_ADDR_TRUST", "TG_CLIENT_ADDR_PROXY_CIDRS", "TG_REPLICA_COUNT",
          "TG_REPLICA_ID", "TG_WEBSOCKET_LISTEN_ADDR", "TG_WEBSOCKET_ALLOWED_ORIGINS",
          "TG_PUBLIC_WEBSOCKET_LISTEN_ADDR", "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR",
          "TG_PUBLIC_REGISTRATION", "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS",
          "TG_TURNSTILE_SITE_KEY", "TG_TURNSTILE_SECRET_KEY_FILE", "TG_TURNSTILE_SIGNIN_ENABLED"
        ][]; . as $key |
          ($expected | has($key)) and
          ([ $env[] | select(type == "string" and startswith($key + "=")) ] | length) == 1 and
          ([ $env[] | select(type == "string" and startswith($key + "=")) ][0] == ($key + "=" + ($expected[$key] | tostring)))
        ) and
        ([ $inspect[0].HostConfig.PortBindings // {} | to_entries[] | select(.value != null and .value != []) ] | length) == 0 and
        ([ $inspect[0].NetworkSettings.Ports // {} | to_entries[] | select(.value != null and .value != []) ] | length) == 0
      ' >/dev/null; then
        return 1
      fi
    fi
  done
}
install_teagram_syslog_config "$SHA"
run_mtproto_smoke() {
  local image_id=$1
  docker run --rm \
    --network host \
    --read-only \
    --cap-drop ALL \
    --security-opt no-new-privileges \
    --user 65532:65532 \
    --entrypoint /usr/local/bin/mtproto-smoke \
    "$image_id"
}
IMG=$(docker inspect --format '{{.Image}}' $C)
if [ "$PREV" = "$SHA" ] && public_route_topology_is_exact && public_base_listener_budget_is_single && public_route_services_running && \
   grep -qs "source_sha=$SHA image_id=$IMG " "$R"/main1238-*.build/built-image.identity; then
  # The mounted bytes do not prove the config HAProxy loaded at startup. Recreate
  # it from the validated mount before accepting the same-SHA shortcut.
  if ! docker_compose up -d --no-deps --force-recreate --wait --wait-timeout 120 public-haproxy </dev/null; then
    echo 'could not recreate public HAProxy from the reviewed config; refusing same-SHA shortcut' >&2
    exit 1
  fi
  if ! public_route_services_running; then
    echo 'public HAProxy failed post-recreate validation; refusing same-SHA shortcut' >&2
    exit 1
  fi
  umask 077
  install_teagram_logs_command "$SHA"
  bash "$LIVE_CHECKOUT/deploy/link-edge/deploy-main.sh" "$SHA"
  run_mtproto_smoke "$IMG"
  install_teagram_ops_command "$SHA"
  echo "already at $SHA with runner provenance"; exit 0
fi
echo "deploying ${PREV:0:7} -> $S"; git log --oneline "HEAD..$SHA"
git diff --name-only HEAD "$SHA" -- migrations
umask 077
docker_compose exec -T postgres pg_dump -U postgres teagram > "$R/pgdump-pre-$S-$TS.sql" </dev/null
tail -c 200 "$R/pgdump-pre-$S-$TS.sql" | grep -q "dump complete"
echo "dump $R/pgdump-pre-$S-$TS.sql"
flock -u 9; exec 9>&-
rc=0
ROLLOUT_RUNNER_SOURCE_SHA=$SHA bash "$R/teagramd-rollout-runner/rollout-runner.sh" apply "$SHA" "$PREV" \
  </dev/null >"$R/rollout-$S-$TS.log" 2>&1 || rc=$?
if ! normalize_tracked_file_modes; then
  echo 'cannot normalize tracked checkout modes after rollout' >&2
  [ "$rc" != 0 ] || rc=1
fi
tail -5 "$R/rollout-$S-$TS.log"; echo "runner exit $rc"; [ $rc = 0 ]
IMG=$(docker inspect --format '{{.Image}}' $C)
[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$IMG")" = "$SHA" ]
grep -ls "source_sha=$SHA image_id=$IMG " "$R"/main1238-*.build/built-image.identity
PIN=$(grep -E '^TG_RSA_KEY_FINGERPRINT=-?[0-9]+$' .env)
docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' $C | grep -qx "$PIN"; echo "pin ok: $PIN"
docker_compose ps -a </dev/null
LOGS=$(docker_compose logs --since 3m teagramd </dev/null)
printf '%s\n' "$LOGS" | grep -E "advertise=|level=ERROR" || true
if ! printf '%s\n' "$LOGS" | grep -q "advertise=teagram.tailaa4918.ts.net:2443"; then
  echo "teagramd did not log its tailnet advertise address"; exit 1
fi
if printf '%s\n' "$LOGS" | grep -q "level=ERROR"; then
  echo "teagramd logged level=ERROR after the rollout"; exit 1
fi
install_teagram_logs_command "$SHA"
bash "$LIVE_CHECKOUT/deploy/link-edge/deploy-main.sh" "$SHA"
run_mtproto_smoke "$IMG"
install_teagram_ops_command "$SHA"
echo "deployed $SHA"
