#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROLLOUT_VERIFIER_SOURCE_ONLY=1
source "$SCRIPT_DIR/rollout-verifier.sh"

TMP=$(mktemp -d)
trap 'rm -rf -- "$TMP"' EXIT
chmod 700 "$TMP"
ROLLOUT_CHECKOUT_PATH="$TMP/checkout"
mkdir -m 700 "$ROLLOUT_CHECKOUT_PATH"
ENV_FILE="$TMP/.env"
OVERRIDE_FILE="$TMP/docker-compose.override.yml"
printf 'TG_NEW_FLAG=on\n' > "$ENV_FILE"
: > "$OVERRIDE_FILE"
chmod 600 "$ENV_FILE" "$OVERRIDE_FILE"

REAL_JQ=$(command -v jq)
JQ_SECRET_ARG_MARKER="$TMP/jq-secret-arg"
export REAL_JQ JQ_SECRET_ARG_MARKER
mkdir -m 700 "$TMP/bin"
cat > "$TMP/bin/jq" <<'SH'
#!/usr/bin/env bash
set -Eeuo pipefail
real_jq=$REAL_JQ
for arg in "$@"; do
  if [[ "$arg" == *'fixture-secret-postgres-password'* || "$arg" == *'postgres://fixture:dsn-secret@db/telegram'* ]]; then
    : > "$JQ_SECRET_ARG_MARKER"
  fi
done
unset REAL_JQ JQ_SECRET_ARG_MARKER
exec "$real_jq" "$@"
SH
chmod 700 "$TMP/bin/jq"
PATH="$TMP/bin:$PATH"
export PATH

COMPOSE_JSON='{"services":{"telegramd":{"stop_grace_period":"2m0s","ports":[],"volumes":[],"network_mode":"","networks":{},"environment":{"TG_REPLICA_COUNT":"1","TG_CLIENT_ADDR_TRUST":"socket","POSTGRES_PASSWORD":"fixture-secret-postgres-password","DATABASE_URL":"postgres://fixture:dsn-secret@db/telegram"}}}}'

inspect_json() {
  local extra=$1
  jq -cn --arg extra "$extra" '
    ["TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket"] as $base
    | {
        Id:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        Image:"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
        Config:{Env:($base + (if $extra == "" then [] else [$extra] end)),StopTimeout:120},
        State:{Status:"running",ExitCode:0,StartedAt:"2026-10-16T12:00:00Z",FinishedAt:"0001-01-01T00:00:00Z"},
        HostConfig:{PortBindings:{}},Mounts:[]
      }
  '
}

target_snapshot() {
  local actual_env=$1 env_assignment=${2:-TG_NEW_FLAG=on} resolved_env_value=${3:-on} inspected compose_json
  printf '%s\n' "$env_assignment" > "$ENV_FILE"
  inspected=$(inspect_json "$actual_env")
  compose_json=$(jq -c --arg value "$resolved_env_value" \
    '.services.telegramd.environment.TG_NEW_FLAG=$value' <<< "$COMPOSE_JSON")
  ROLLOUT_ADDED_ENV=TG_NEW_FLAG snapshot_from_json \
    "$inspected" "$compose_json" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target
}

rollback_snapshot() {
  local actual_env=$1 resolved_env_value=${2:-on} inspected compose_json
  inspected=$(inspect_json "$actual_env")
  compose_json=$(jq -c --arg value "$resolved_env_value" \
    '.services.telegramd.environment.TG_NEW_FLAG=$value' <<< "$COMPOSE_JSON")
  ROLLOUT_ADDED_ENV=TG_NEW_FLAG snapshot_from_json \
    "$inspected" "$compose_json" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render rollback
}

snapshot=$(target_snapshot 'TG_NEW_FLAG=on')
[ "$(jq -r '.container_env_added' <<< "$snapshot")" = TG_NEW_FLAG ]
if [ -e "$JQ_SECRET_ARG_MARKER" ]; then
  printf '%s\n' 'FAIL resolved Compose secrets were passed in subprocess arguments' >&2
  exit 1
fi
printf '%s\n' 'PASS resolved Compose secrets stay out of subprocess arguments'
printf '%s\n' 'PASS target container receives the named .env value'

snapshot=$(target_snapshot 'TG_NEW_FLAG=quoted value' 'TG_NEW_FLAG="quoted value"' 'quoted value')
[ "$(jq -r '.container_env_added' <<< "$snapshot")" = TG_NEW_FLAG ]
printf '%s\n' 'PASS quoted .env value matches the resolved Compose environment'

snapshot=$(target_snapshot 'TG_NEW_FLAG=on-suffix' 'TG_NEW_FLAG=on' 'on-suffix')
[ "$(jq -r '.container_env_added' <<< "$snapshot")" = TG_NEW_FLAG ]
printf '%s\n' 'PASS interpolated Compose value matches the target container'

printf 'TG_NEW_FLAG=on\n' > "$ENV_FILE"
inspected=$(inspect_json 'TG_NEW_FLAG=on')
if ROLLOUT_ADDED_ENV=TG_NEW_FLAG snapshot_from_json \
  "$inspected" "$COMPOSE_JSON" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/unresolved-compose.err"; then
  printf '%s\n' 'FAIL addition absent from resolved Compose environment was accepted' >&2
  exit 1
fi
! grep -q 'TG_NEW_FLAG=on' "$TMP/unresolved-compose.err"
printf '%s\n' 'PASS addition absent from the resolved Compose environment is rejected'

if target_snapshot '' >/dev/null 2>"$TMP/missing.err"; then
  printf '%s\n' 'FAIL missing named environment addition was accepted' >&2
  exit 1
fi
! grep -q 'TG_NEW_FLAG=on' "$TMP/missing.err"
printf '%s\n' 'PASS target container missing the named .env value is rejected'

if target_snapshot 'TG_NEW_FLAG=off' >/dev/null 2>"$TMP/mismatch.err"; then
  printf '%s\n' 'FAIL mismatched named environment addition was accepted' >&2
  exit 1
fi
! grep -Eq 'TG_NEW_FLAG=(on|off)' "$TMP/mismatch.err"
printf '%s\n' 'PASS target container with a mismatched named value is rejected'

snapshot=$(rollback_snapshot '')
[ "$(jq -r '.container_env_added' <<< "$snapshot")" = none ]
printf '%s\n' 'PASS rollback snapshot permits the previous release to omit the added variable'

snapshot=$(rollback_snapshot 'TG_NEW_FLAG=on')
[ "$(jq -r '.container_env_added' <<< "$snapshot")" = TG_NEW_FLAG ]
printf '%s\n' 'PASS rollback snapshot accepts a retained addition matching resolved Compose'

if rollback_snapshot 'TG_NEW_FLAG=off' >/dev/null 2>"$TMP/rollback-mismatch.err"; then
  printf '%s\n' 'FAIL rollback snapshot accepted a retained addition that mismatches resolved Compose' >&2
  exit 1
fi
! grep -Eq 'TG_NEW_FLAG=(on|off)' "$TMP/rollback-mismatch.err"
printf '%s\n' 'PASS rollback snapshot rejects a retained addition mismatching resolved Compose'
