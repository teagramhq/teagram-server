#!/usr/bin/env bash
# Deploys DEPLOY_SHA, a commit on origin/main whose CI passed, to the
# teagram-server LXC through the rollout runner. Runs as root on the LXC: from
# the Deploy. workflow's forced-command key, or by hand with
# `ssh teagram-server 'DEPLOY_SHA=<full sha> bash -s' < deploy/teagram/deploy-main.sh`.
# Stdin may be this script, so every docker compose call reads /dev/null.
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
C=telegram-server-telegramd-1
LIVE_CHECKOUT=$(pwd -P)
LIVE_CF=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' $C | tr , :)
case "$LIVE_CF" in
  "$LIVE_CHECKOUT"/.rollout-compose.local-*.yml:"$LIVE_CHECKOUT"/docker-compose.override.yml) ;;
  "$LIVE_CHECKOUT"/docker-compose.yml:"$LIVE_CHECKOUT"/docker-compose.local-blobs.yml:"$LIVE_CHECKOUT"/docker-compose.override.yml) ;;
  *) echo "live teagramd not on an approved local compose ($LIVE_CF); refusing"; exit 1 ;;
esac
export COMPOSE_FILE="$LIVE_CF"   # carry forward the pinned local-blob files live runs from
IMG=$(docker inspect --format '{{.Image}}' $C)
if [ "$PREV" = "$SHA" ] && grep -qs "source_sha=$SHA image_id=$IMG " "$R"/main1238-*.build/built-image.identity; then
  echo "already at $SHA with runner provenance"; exit 0
fi
echo "deploying ${PREV:0:7} -> $S"; git log --oneline "HEAD..$SHA"
git diff --name-only HEAD "$SHA" -- migrations
umask 077
docker compose exec -T postgres pg_dump -U postgres teagram > "$R/pgdump-pre-$S-$TS.sql" </dev/null
tail -c 200 "$R/pgdump-pre-$S-$TS.sql" | grep -q "dump complete"
echo "dump $R/pgdump-pre-$S-$TS.sql"
# shellcheck disable=SC2174 # only the runner directory is new
mkdir -m 700 -p "$R/teagramd-rollout-runner"
for f in rollout-runner.sh rollout-verifier.sh schema-result-gate.sh schema-result-gate.py blob-mode-state.py qualify-rustfs-transition.py stack-move-runner.py; do
  git show "$SHA:deploy/teagramd/rollout-runner/$f" > "$R/teagramd-rollout-runner/$f"; chmod 600 "$R/teagramd-rollout-runner/$f"
done
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
docker compose ps -a </dev/null
LOGS=$(docker compose logs --since 3m teagramd </dev/null)
printf '%s\n' "$LOGS" | grep -E "advertise=|level=ERROR" || true
if ! printf '%s\n' "$LOGS" | grep -q "advertise=teagram.tailaa4918.ts.net:2443"; then
  echo "teagramd did not log its tailnet advertise address"; exit 1
fi
if printf '%s\n' "$LOGS" | grep -q "level=ERROR"; then
  echo "teagramd logged level=ERROR after the rollout"; exit 1
fi
echo "deployed $SHA"
