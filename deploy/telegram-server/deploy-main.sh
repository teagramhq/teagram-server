#!/usr/bin/env bash
# Deploys origin/main to the telegram-server LXC through the rollout runner.
# Runs as root on the LXC: from the Deploy. workflow's forced-command key, or by
# hand with `ssh telegram-server 'bash -s' < deploy/telegram-server/deploy-main.sh`.
# Stdin may be this script, so every docker compose call reads /dev/null.
set -eu
exec 9>/tmp/telegram-server-deploy.lock; flock -x 9   # wait out a running deploy; released before the runner takes the same lock
cd /opt/telegram-server
git fetch -q origin
PREV=$(git rev-parse HEAD); SHA=$(git rev-parse origin/main); S=${SHA:0:7}; TS=$(date -u +%Y%m%dT%H%M%SZ)
C=telegram-server-telegramd-1
LIVE_CF=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' $C | tr , :)
case "$LIVE_CF" in *.rollout-compose.local-*:*/docker-compose.override.yml) ;; *) echo "live telegramd not on a pinned local compose ($LIVE_CF); refusing"; exit 1;; esac
export COMPOSE_FILE="$LIVE_CF"   # carry forward the pinned local-blob files live runs from
IMG=$(docker inspect --format '{{.Image}}' $C)
if [ "$PREV" = "$SHA" ] && grep -qs "source_sha=$SHA image_id=$IMG " /root/main1238-*.build/built-image.identity; then
  echo "already at $SHA with runner provenance"; exit 0
fi
echo "deploying ${PREV:0:7} -> $S"; git log --oneline HEAD..origin/main
git diff --name-only HEAD origin/main -- migrations
umask 077
docker compose exec -T postgres pg_dump -U postgres telegram > /root/pgdump-pre-$S-$TS.sql </dev/null
tail -c 200 /root/pgdump-pre-$S-$TS.sql | grep -q "dump complete"
echo "dump /root/pgdump-pre-$S-$TS.sql"
mkdir -m 700 -p /root/telegramd-rollout-runner
for f in rollout-runner.sh rollout-verifier.sh schema-result-gate.sh schema-result-gate.py blob-mode-state.py; do
  git show "$SHA:deploy/telegramd/rollout-runner/$f" > /root/telegramd-rollout-runner/$f; chmod 600 /root/telegramd-rollout-runner/$f
done
flock -u 9; exec 9>&-
rc=0
ROLLOUT_RUNNER_SOURCE_SHA=$SHA bash /root/telegramd-rollout-runner/rollout-runner.sh apply "$SHA" "$PREV" \
  </dev/null >/root/rollout-$S-$TS.log 2>&1 || rc=$?
tail -5 /root/rollout-$S-$TS.log; echo "runner exit $rc"; [ $rc = 0 ]
IMG=$(docker inspect --format '{{.Image}}' $C)
[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' $IMG)" = "$SHA" ]
grep -ls "source_sha=$SHA image_id=$IMG " /root/main1238-*.build/built-image.identity
PIN=$(grep -E '^TG_RSA_KEY_FINGERPRINT=-?[0-9]+$' .env)
docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' $C | grep -qx "$PIN"; echo "pin ok: $PIN"
docker compose ps -a </dev/null
LOGS=$(docker compose logs --since 3m telegramd </dev/null)
printf '%s\n' "$LOGS" | grep -E "advertise=|level=ERROR" || true
if ! printf '%s\n' "$LOGS" | grep -q "advertise=telegram-server.tailaa4918.ts.net:2443"; then
  echo "telegramd did not log its tailnet advertise address"; exit 1
fi
if printf '%s\n' "$LOGS" | grep -q "level=ERROR"; then
  echo "telegramd logged level=ERROR after the rollout"; exit 1
fi
echo "deployed $SHA"
