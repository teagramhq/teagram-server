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
public_replica_count() {
  awk -F= '
    $1 == "TG_REPLICA_COUNT" { count++; value=$2 }
    END {
      if (count == 0) print "unset"
      else if (count > 1) print "duplicate"
      else print value
    }
  ' "$LIVE_CHECKOUT/.env"
}
public_route_services_running() {
  local service id state health
  for service in public-haproxy teagramd-public-1 teagramd-public-2; do
    id=$(docker_compose ps -q "$service" </dev/null 2>/dev/null) || return 1
    [[ "$id" =~ ^[0-9a-f]{12,64}$ ]] || return 1
    state=$(docker inspect --format '{{.State.Status}}' "$id" 2>/dev/null) || return 1
    [ "$state" = running ] || return 1
    if [[ "$service" == public-haproxy ]]; then
      health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}' "$id" 2>/dev/null) || return 1
      [ "$health" = healthy ] || return 1
    fi
  done
}
stage_public_replica_count() {
  local env_file="$LIVE_CHECKOUT/.env" previous_env temp count
  [[ ! -L "$env_file" && -f "$env_file" ]] || { echo 'public WebSocket rollout requires a regular .env file' >&2; return 1; }
  if [[ -z ${DEPLOY_DIR:-} ]]; then
    [[ "$(stat -c '%u:%a' -- "$env_file")" == 0:600 ]] || {
      echo 'public WebSocket rollout requires a root-owned mode-0600 .env file' >&2
      return 1
    }
  fi
  [ -z "$(tail -c 1 -- "$env_file")" ] || { echo '.env must end with a newline before changing replica count' >&2; return 1; }
  count=$(public_replica_count) || return 1
  case "$count" in
    3) return 0 ;;
    1|unset) ;;
    *)
      echo "public WebSocket rollout requires an unset, 1, or 3 TG_REPLICA_COUNT value (found $count)" >&2
      return 1
      ;;
  esac
  previous_env="$R/teagramd-env-before-${S}-${TS}"
  [[ ! -e "$previous_env" && ! -L "$previous_env" ]] || { echo 'replica-count backup path already exists' >&2; return 1; }
  cp -p -- "$env_file" "$previous_env" && chmod 600 -- "$previous_env" || {
    echo 'cannot protect the pre-change .env for replica-count rollback' >&2
    return 1
  }
  temp=$(mktemp "$LIVE_CHECKOUT/.env.public-count.XXXXXXXX") || return 1
  if [[ "$count" == 1 ]] && ! awk '
      BEGIN { count=0; invalid=0 }
      /^TG_REPLICA_COUNT=/ {
        count++
        if ($0 != "TG_REPLICA_COUNT=1") invalid=1
        print "TG_REPLICA_COUNT=3"
        next
      }
      { print }
      END { if (count != 1 || invalid) exit 1 }
    ' "$env_file" > "$temp"; then
    rm -f -- "$temp"
    echo 'only a literal TG_REPLICA_COUNT=1 value may be raised for the public replicas' >&2
    return 1
  elif [[ "$count" == unset ]] && ! {
    cat -- "$env_file" && printf '%s\n' 'TG_REPLICA_COUNT=3'
  } > "$temp"; then
    rm -f -- "$temp"
    echo 'cannot stage TG_REPLICA_COUNT=3 as an approved environment addition' >&2
    return 1
  fi
  if ! chmod 600 -- "$temp" || ! sync || ! mv -f -- "$temp" "$env_file" || ! sync; then
    rm -f -- "$temp"
    echo 'cannot atomically stage TG_REPLICA_COUNT=3' >&2
    return 1
  fi
  export ROLLOUT_RUNNER_PREVIOUS_ENV_FILE="$previous_env"
  if [[ "$count" == unset ]]; then
    export ROLLOUT_RUNNER_ADDED_ENV=TG_REPLICA_COUNT
    printf 'staged TG_REPLICA_COUNT=3 as a named environment addition through the rollout runner\n'
  else
    export ROLLOUT_RUNNER_CHANGED_ENV=TG_REPLICA_COUNT
    printf 'staged TG_REPLICA_COUNT=3 through the rollout runner approved environment change path\n'
  fi
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
if [ "$PREV" = "$SHA" ] && [ "$(public_replica_count)" = 3 ] && \
   public_route_services_running && grep -qs "source_sha=$SHA image_id=$IMG " "$R"/main1238-*.build/built-image.identity; then
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
stage_public_replica_count
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
