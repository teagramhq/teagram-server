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
  if [[ "$arg" == *'fixture-secret-postgres-password'* || "$arg" == *'postgres://fixture:dsn-secret@db/teagram'* ]]; then
    : > "$JQ_SECRET_ARG_MARKER"
  fi
done
unset REAL_JQ JQ_SECRET_ARG_MARKER
exec "$real_jq" "$@"
SH
chmod 700 "$TMP/bin/jq"
PATH="$TMP/bin:$PATH"
export PATH

COMPOSE_JSON='{"services":{"teagramd":{"stop_grace_period":"2m0s","ports":[],"volumes":[],"network_mode":"","networks":{},"environment":{"TG_REPLICA_COUNT":"1","TG_CLIENT_ADDR_TRUST":"socket","POSTGRES_PASSWORD":"fixture-secret-postgres-password","DATABASE_URL":"postgres://fixture:dsn-secret@db/teagram"}}}}'

NETWORK_ALLOWLIST_BASE=$(jq -cn '
  {
    name:"fixture",
    networks:{default:{name:"fixture_default",ipam:{}}},
    services:{postgres:{},migrate:{},teagramd:{}}
  }
')
DEFAULT_NETWORK_PROJECTION=$(compose_network_projection "$NETWORK_ALLOWLIST_BASE")
[ "$(compose_network_profile "$NETWORK_ALLOWLIST_BASE")" = default-only ]
for service in postgres migrate; do
  host_network_compose=$(jq -c --arg service "$service" \
    '.services[$service].network_mode="host"' <<< "$NETWORK_ALLOWLIST_BASE")
  host_network_projection=$(compose_network_projection "$host_network_compose")
  if [ "$(compose_network_profile "$host_network_compose")" != other ] || \
     [ "$host_network_projection" = "$DEFAULT_NETWORK_PROJECTION" ] || \
     ! jq -e --arg service "$service" '.services[$service].network_mode == "host"' \
       <<< "$host_network_projection" >/dev/null; then
    printf 'FAIL %s host network mode must be projected and rejected by the allowlist\n' "$service" >&2
    exit 1
  fi
  printf 'PASS %s host network mode is projected and rejected by the allowlist\n' "$service"
done

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
    '.services.teagramd.environment.TG_NEW_FLAG=$value' <<< "$COMPOSE_JSON")
  ROLLOUT_ADDED_ENV=TG_NEW_FLAG snapshot_from_json \
    "$inspected" "$compose_json" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target
}

rollback_snapshot() {
  local actual_env=$1 resolved_env_value=${2:-on} inspected compose_json
  inspected=$(inspect_json "$actual_env")
  compose_json=$(jq -c --arg value "$resolved_env_value" \
    '.services.teagramd.environment.TG_NEW_FLAG=$value' <<< "$COMPOSE_JSON")
  ROLLOUT_ADDED_ENV=TG_NEW_FLAG snapshot_from_json \
    "$inspected" "$compose_json" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render rollback
}

registration_snapshot() {
  local actual=$1 env_value=$2 compose_value=$3 name=$4 inspected compose_json
  printf 'TG_REGISTRATION=%s\n' "$env_value" > "$ENV_FILE"
  inspected=$(inspect_json "$actual")
  compose_json=$(jq -c --arg value "$compose_value" \
    '.services.teagramd.environment.TG_REGISTRATION=$value' <<< "$COMPOSE_JSON")
  ROLLOUT_CHANGED_ENV=TG_REGISTRATION snapshot_from_json \
    "$inspected" "$compose_json" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render "$name"
}

public_setting_snapshot() {
  local setting=$1 value=$2 actual=$3 name=$4 added=${5:-} changed=${6:-} inspected compose_json
  printf '%s=%s\n' "$setting" "$value" > "$ENV_FILE"
  inspected=$(inspect_json "$actual")
  compose_json=$(jq -c --arg setting "$setting" --arg value "$value" \
    '.services.teagramd.environment[$setting]=$value' <<< "$COMPOSE_JSON")
  ROLLOUT_ADDED_ENV="$added" ROLLOUT_CHANGED_ENV="$changed" snapshot_from_json \
    "$inspected" "$compose_json" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render "$name"
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

baseline=$(registration_snapshot 'TG_REGISTRATION=closed' open open baseline)
target=$(registration_snapshot 'TG_REGISTRATION=open' open open target)
[ "$(jq -r '.container_env_changed' <<< "$baseline")" = none ]
[ "$(jq -r '.container_env_changed' <<< "$target")" = TG_REGISTRATION ]
[ "$(jq -r '.container_env_sha256' <<< "$baseline")" = "$(jq -r '.container_env_sha256' <<< "$target")" ]
printf '%s\n' 'PASS target records the named registration value while preserving other environment equivalence'

rollback=$(registration_snapshot 'TG_REGISTRATION=open' open open rollback)
[ "$(jq -r '.container_env_changed' <<< "$rollback")" = TG_REGISTRATION ]
printf '%s\n' 'PASS rollback snapshot verifies and records a retained registration value'

if registration_snapshot 'TG_REGISTRATION=closed' open open target \
  >/dev/null 2>"$TMP/registration-container-mismatch.err"; then
  printf '%s\n' 'FAIL target accepted a registration value that differs from the requested .env' >&2
  exit 1
fi
! grep -Eq 'TG_REGISTRATION=(open|closed)' "$TMP/registration-container-mismatch.err"
printf '%s\n' 'PASS target rejects a registration value that differs from the requested .env'

if registration_snapshot 'TG_REGISTRATION=open' open closed target \
  >/dev/null 2>"$TMP/registration-compose-mismatch.err"; then
  printf '%s\n' 'FAIL target accepted a registration value that differs from resolved Compose' >&2
  exit 1
fi
! grep -Eq 'TG_REGISTRATION=(open|closed)' "$TMP/registration-compose-mismatch.err"
printf '%s\n' 'PASS target rejects a registration value that differs from resolved Compose'

legacy_inspect=$(inspect_json '')
legacy_snapshot=$(snapshot_from_json \
  "$legacy_inspect" "$COMPOSE_JSON" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline)
public_ws_compose=$(jq -c '
  .services.teagramd.environment += {
    TG_PUBLIC_WEBSOCKET_LISTEN_ADDR:"",
    TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR:"",
    TG_PUBLIC_REGISTRATION:"closed",
    TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS:"",
    TG_WELCOME_CHANNEL:""
  }
' <<< "$COMPOSE_JSON")
public_ws_inspect=$(printf '%s' "$legacy_inspect" | jq -c '
  .Config.Env += [
    "TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=",
    "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR=",
    "TG_PUBLIC_REGISTRATION=closed",
    "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS=",
    "TG_WELCOME_CHANNEL="
  ]
')
public_ws_snapshot=$(snapshot_from_json \
  "$public_ws_inspect" "$public_ws_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.config_sha256' <<< "$legacy_snapshot")" = "$(jq -r '.config_sha256' <<< "$public_ws_snapshot")" ]
[ "$(jq -r '.container_env_sha256' <<< "$legacy_snapshot")" = "$(jq -r '.container_env_sha256' <<< "$public_ws_snapshot")" ]
printf '%s\n' 'PASS safe public defaults preserve resolved config and container environment equivalence'

for setting in TG_PUBLIC_WEBSOCKET_LISTEN_ADDR TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR TG_PUBLIC_REGISTRATION TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS TG_WELCOME_CHANNEL; do
  case "$setting" in
    TG_PUBLIC_REGISTRATION) unsafe_value=invite ;;
    TG_WELCOME_CHANNEL) unsafe_value=welcome ;;
    TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR) unsafe_value=server.example.com:443 ;;
    *) unsafe_value=https://public.example ;;
  esac
  unsafe_compose=$(jq -c --arg setting "$setting" --arg value "$unsafe_value" \
    '.services.teagramd.environment[$setting]=$value' <<< "$public_ws_compose")
  if snapshot_from_json "$public_ws_inspect" "$unsafe_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
    >/dev/null 2>"$TMP/$setting-compose.err"; then
    printf 'FAIL unsafe resolved Compose setting was accepted: %s\n' "$setting" >&2
    exit 1
  fi
  printf 'PASS unsafe resolved Compose setting is rejected: %s\n' "$setting"

  unsafe_inspect=$(printf '%s' "$public_ws_inspect" | jq -c --arg setting "$setting" --arg value "$unsafe_value" '
    .Config.Env |= (map(select((startswith($setting + "=")) | not)) + [$setting + "=" + $value])
  ')
  if snapshot_from_json "$unsafe_inspect" "$public_ws_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
    >/dev/null 2>"$TMP/$setting-container.err"; then
    printf 'FAIL unsafe container setting was accepted: %s\n' "$setting" >&2
    exit 1
  fi
  printf 'PASS unsafe container setting is rejected: %s\n' "$setting"
done

for setting_value in \
  TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=0.0.0.0:2444 \
  TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR=server.example.com:443 \
  TG_PUBLIC_REGISTRATION=invite \
  TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io; do
  setting=${setting_value%%=*}
  value=${setting_value#*=}
  named_public_snapshot=$(public_setting_snapshot "$setting" "$value" "$setting=$value" target "$setting")
  [ "$(jq -r '.container_env_added' <<< "$named_public_snapshot")" = "$setting" ]
  printf 'PASS named public setting reaches the resolved Compose and target container: %s\n' "$setting"
done

setting=TG_PUBLIC_WEBSOCKET_LISTEN_ADDR
value=0.0.0.0:2444
printf '%s=%s\n' "$setting" "$value" > "$ENV_FILE"
named_public_compose=$(jq -c --arg setting "$setting" --arg value "$value" \
  '.services.teagramd.environment[$setting]=$value' <<< "$public_ws_compose")
named_public_preflight=$(ROLLOUT_ADDED_ENV="$setting" snapshot_from_json \
  "$legacy_inspect" "$named_public_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render preflight)
[ "$(jq -r '.container_env_added' <<< "$named_public_preflight")" = none ]
printf '%s\n' 'PASS named public setting is admitted during preflight before replacement'

printf 'TG_PUBLIC_WEBSOCKET_LISTEN_ADDR="0.0.0.0:2444"\n' > "$ENV_FILE"
public_listen_quoted_compose=$(jq -c '.services.teagramd.environment.TG_PUBLIC_WEBSOCKET_LISTEN_ADDR="0.0.0.0:2444"' <<< "$public_ws_compose")
public_listen_quoted_inspect=$(printf '%s' "$legacy_inspect" | jq -c \
  '.Config.Env += ["TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=0.0.0.0:2444"]')
named_public_quoted_target=$(ROLLOUT_ADDED_ENV=TG_PUBLIC_WEBSOCKET_LISTEN_ADDR snapshot_from_json \
  "$public_listen_quoted_inspect" "$public_listen_quoted_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.container_env_added' <<< "$named_public_quoted_target")" = TG_PUBLIC_WEBSOCKET_LISTEN_ADDR ]
printf '%s\n' 'PASS quoted public .env value reaches the exact resolved Compose and target value'

printf 'TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=0.0.0.0:2444\n' > "$ENV_FILE"
public_listen_mismatched_compose=$(jq -c '.services.teagramd.environment.TG_PUBLIC_WEBSOCKET_LISTEN_ADDR="127.0.0.1:2444"' <<< "$public_ws_compose")
public_listen_mismatched_inspect=$(printf '%s' "$legacy_inspect" | jq -c \
  '.Config.Env += ["TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=127.0.0.1:2444"]')
if ROLLOUT_ADDED_ENV=TG_PUBLIC_WEBSOCKET_LISTEN_ADDR snapshot_from_json \
  "$public_listen_mismatched_inspect" "$public_listen_mismatched_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/public-listen-mismatched-source.err"; then
  printf '%s\n' 'FAIL public setting whose resolved value differs from .env was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS public addition rejects a resolved value that differs from its .env assignment'

printf 'TG_WELCOME_CHANNEL=welcome\n' > "$ENV_FILE"
welcome_active_compose=$(jq -c '.services.teagramd.environment.TG_WELCOME_CHANNEL="welcome"' <<< "$public_ws_compose")
welcome_active_inspect=$(printf '%s' "$public_ws_inspect" | jq -c '
  .Config.Env |= (map(select(startswith("TG_WELCOME_CHANNEL=") | not)) + ["TG_WELCOME_CHANNEL=welcome"])
')
if ROLLOUT_ADDED_ENV=TG_WELCOME_CHANNEL snapshot_from_json \
  "$welcome_active_inspect" "$welcome_active_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/welcome-active-named.err"; then
  printf '%s\n' 'FAIL named TG_WELCOME_CHANNEL activation was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS TG_WELCOME_CHANNEL remains fail-closed when named'

printf 'TG_PUBLIC_REGISTRATION=open\n' > "$ENV_FILE"
public_registration_open_compose=$(jq -c '.services.teagramd.environment.TG_PUBLIC_REGISTRATION="open"' <<< "$public_ws_compose")
public_registration_open_inspect=$(printf '%s' "$public_ws_inspect" | jq -c '
  .Config.Env |= (map(select(startswith("TG_PUBLIC_REGISTRATION=") | not)) + ["TG_PUBLIC_REGISTRATION=open"])
')
if ROLLOUT_ADDED_ENV=TG_PUBLIC_REGISTRATION snapshot_from_json \
  "$public_registration_open_inspect" "$public_registration_open_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/public-registration-open.err"; then
  printf '%s\n' 'FAIL named TG_PUBLIC_REGISTRATION=open was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS TG_PUBLIC_REGISTRATION=open is rejected even when named'

printf 'TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=127.0.0.1:2444\n' > "$ENV_FILE"
public_listen_baseline_inspect=$(printf '%s' "$legacy_inspect" | jq -c '.Config.Env += ["TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=0.0.0.0:2444"]')
public_listen_changed_compose=$(jq -c '.services.teagramd.environment.TG_PUBLIC_WEBSOCKET_LISTEN_ADDR="127.0.0.1:2444"' <<< "$public_ws_compose")
if snapshot_from_json "$public_listen_baseline_inspect" "$public_listen_changed_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render preflight \
  >/dev/null 2>"$TMP/public-listen-changed-unnamed.err"; then
  printf '%s\n' 'FAIL changed public WebSocket setting was accepted without the change path' >&2
  exit 1
fi
public_listen_change_preflight=$(ROLLOUT_CHANGED_ENV=TG_PUBLIC_WEBSOCKET_LISTEN_ADDR snapshot_from_json \
  "$public_listen_baseline_inspect" "$public_listen_changed_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render preflight)
public_listen_change_target_inspect=$(printf '%s' "$legacy_inspect" | jq -c \
  '.Config.Env += ["TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=127.0.0.1:2444"]')
public_listen_change_target=$(ROLLOUT_CHANGED_ENV=TG_PUBLIC_WEBSOCKET_LISTEN_ADDR snapshot_from_json \
  "$public_listen_change_target_inspect" "$public_listen_changed_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.container_env_changed' <<< "$public_listen_change_preflight")" = none ]
[ "$(jq -r '.container_env_changed' <<< "$public_listen_change_target")" = TG_PUBLIC_WEBSOCKET_LISTEN_ADDR ]
printf '%s\n' 'PASS changed public WebSocket value requires and accepts the named change path'

printf 'TG_PUBLIC_WEBSOCKET_LISTEN_ADDR="127.0.0.1:2444"\n' > "$ENV_FILE"
public_listen_quoted_change_target=$(ROLLOUT_CHANGED_ENV=TG_PUBLIC_WEBSOCKET_LISTEN_ADDR snapshot_from_json \
  "$public_listen_change_target_inspect" "$public_listen_changed_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.container_env_changed' <<< "$public_listen_quoted_change_target")" = TG_PUBLIC_WEBSOCKET_LISTEN_ADDR ]
printf '%s\n' 'PASS quoted changed public WebSocket value matches the target container'

printf 'TG_PUBLIC_REGISTRATION="closed"\n' > "$ENV_FILE"
public_registration_closed_compose=$(jq -c '.services.teagramd.environment.TG_PUBLIC_REGISTRATION="closed"' <<< "$public_ws_compose")
public_registration_closed_inspect=$(printf '%s' "$legacy_inspect" | jq -c \
  '.Config.Env += ["TG_PUBLIC_REGISTRATION=closed"]')
public_registration_quoted_change_target=$(ROLLOUT_CHANGED_ENV=TG_PUBLIC_REGISTRATION snapshot_from_json \
  "$public_registration_closed_inspect" "$public_registration_closed_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.container_env_changed' <<< "$public_registration_quoted_change_target")" = TG_PUBLIC_REGISTRATION ]
printf '%s\n' 'PASS quoted TG_PUBLIC_REGISTRATION change matches the target container'

printf '%s\n' \
  'TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=0.0.0.0:2444' \
  'TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR=server.example.com:443' \
  'TG_PUBLIC_REGISTRATION=invite' \
  'TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io' > "$ENV_FILE"
public_retained_compose=$(jq -c '
  .services.teagramd.environment += {
    TG_PUBLIC_WEBSOCKET_LISTEN_ADDR:"0.0.0.0:2444",
    TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR:"server.example.com:443",
    TG_PUBLIC_REGISTRATION:"invite",
    TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS:"https://web.teagram.io"
  }
' <<< "$public_ws_compose")
public_retained_inspect=$(printf '%s' "$legacy_inspect" | jq -c '
  .Config.Env += [
    "TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=0.0.0.0:2444",
    "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR=server.example.com:443",
    "TG_PUBLIC_REGISTRATION=invite",
    "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io"
  ]
')
public_retained_baseline=$(snapshot_from_json \
  "$public_retained_inspect" "$public_retained_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline)
public_retained_target=$(snapshot_from_json \
  "$public_retained_inspect" "$public_retained_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.container_env_sha256' <<< "$public_retained_baseline")" = "$(jq -r '.container_env_sha256' <<< "$public_retained_target")" ]
printf '%s\n' 'PASS previously added public settings are retained on a later unnamed rollout'

turnstile_legacy_inspect=$(inspect_json '')
turnstile_legacy_snapshot=$(snapshot_from_json \
  "$turnstile_legacy_inspect" "$COMPOSE_JSON" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline)
turnstile_disabled_compose=$(jq -c '
  .services.teagramd.environment += {
    TG_TURNSTILE_SITE_KEY:"",
    TG_TURNSTILE_SECRET_KEY_FILE:"",
    TG_TURNSTILE_SIGNIN_ENABLED:"false"
  }
  | .services.teagramd.secrets = [{
      source:"teagramd_turnstile",
      target:"teagramd-turnstile"
    }]
  | .secrets.teagramd_turnstile = {file:"/dev/null"}
' <<< "$COMPOSE_JSON")
turnstile_disabled_inspect=$(printf '%s' "$turnstile_legacy_inspect" | jq -c '
  .Config.Env += [
    "TG_TURNSTILE_SITE_KEY=",
    "TG_TURNSTILE_SECRET_KEY_FILE=",
    "TG_TURNSTILE_SIGNIN_ENABLED=false"
  ]
  | .Mounts += [{
      Type:"bind",
      Source:"/dev/null",
      Destination:"/run/secrets/teagramd-turnstile",
      Mode:"ro",
      RW:false,
      Propagation:"rprivate"
    }]
')
turnstile_disabled_snapshot=$(snapshot_from_json \
  "$turnstile_disabled_inspect" "$turnstile_disabled_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.config_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.config_sha256' <<< "$turnstile_disabled_snapshot")" ]
[ "$(jq -r '.mounts_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.mounts_sha256' <<< "$turnstile_disabled_snapshot")" ]
[ "$(jq -r '.container_env_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.container_env_sha256' <<< "$turnstile_disabled_snapshot")" ]
printf '%s\n' 'PASS disabled Turnstile defaults and its exact read-only secret mount preserve rollout equivalence'

turnstile_local_compose=$(jq -c '
  .services.teagramd.environment += {
    TG_TURNSTILE_SITE_KEY:"",
    TG_TURNSTILE_SECRET_KEY_FILE:"",
    TG_TURNSTILE_SIGNIN_ENABLED:"false"
  }
  | .services.teagramd.secrets = []
  | .secrets |= del(.teagramd_turnstile)
  | if (.secrets | length) == 0 then del(.secrets) else . end
' <<< "$turnstile_disabled_compose")
turnstile_local_inspect=$(printf '%s' "$turnstile_legacy_inspect" | jq -c '
  .Config.Env += [
    "TG_TURNSTILE_SITE_KEY=",
    "TG_TURNSTILE_SECRET_KEY_FILE=",
    "TG_TURNSTILE_SIGNIN_ENABLED=false"
  ]
')
turnstile_local_snapshot=$(snapshot_from_json \
  "$turnstile_local_inspect" "$turnstile_local_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.config_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.config_sha256' <<< "$turnstile_local_snapshot")" ]
[ "$(jq -r '.mounts_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.mounts_sha256' <<< "$turnstile_local_snapshot")" ]
[ "$(jq -r '.container_env_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.container_env_sha256' <<< "$turnstile_local_snapshot")" ]
printf '%s\n' 'PASS disabled Turnstile defaults tolerate the local blob overlay resetting service secrets'

busybox_readlink() {
  [ "$1" = -f ] || { printf 'readlink: unrecognized option: %s\n' "$1" >&2; return 1; }
  command readlink -f "$2"
}
readlink() { busybox_readlink "$@"; }
verify_turnstile_compose_configuration "$turnstile_disabled_compose"
unset -f readlink busybox_readlink
printf '%s\n' 'PASS Turnstile path validation supports BusyBox readlink'

turnstile_secret_dir="$ROLLOUT_CHECKOUT_PATH/.secrets"
mkdir -m 700 "$turnstile_secret_dir"
turnstile_secret_source="$turnstile_secret_dir/teagramd-turnstile"
: > "$turnstile_secret_source"
chmod 444 "$turnstile_secret_source"
turnstile_compose=$(jq -c --arg source "$turnstile_secret_source" '
  .services.teagramd.environment += {
    TG_TURNSTILE_SITE_KEY:"fixture-site-key",
    TG_TURNSTILE_SECRET_KEY_FILE:"/run/secrets/teagramd-turnstile",
    TG_TURNSTILE_SIGNIN_ENABLED:"false"
  }
  | .services.teagramd.secrets = [{
      source:"teagramd_turnstile",
      target:"teagramd-turnstile"
    }]
  | .secrets.teagramd_turnstile = {file:$source}
' <<< "$COMPOSE_JSON")
turnstile_enabled_local_compose=$(jq -c '
  .services.teagramd.secrets = []
  | .secrets |= del(.teagramd_turnstile)
  | if (.secrets | length) == 0 then del(.secrets) else . end
' <<< "$turnstile_compose")
if verify_turnstile_compose_configuration "$turnstile_enabled_local_compose" \
  >/dev/null 2>"$TMP/turnstile-enabled-local.err"; then
  printf '%s\n' 'FAIL local blob overlay hid an enabled Turnstile secret' >&2
  exit 1
fi
printf '%s\n' 'PASS local blob overlay cannot hide an enabled Turnstile secret'

turnstile_inspect=$(printf '%s' "$turnstile_legacy_inspect" | jq -c --arg source "$turnstile_secret_source" '
  .Config.Env += [
    "TG_TURNSTILE_SITE_KEY=fixture-site-key",
    "TG_TURNSTILE_SECRET_KEY_FILE=/run/secrets/teagramd-turnstile",
    "TG_TURNSTILE_SIGNIN_ENABLED=false"
  ]
  | .Mounts += [{
      Type:"bind",
      Source:$source,
      Destination:"/run/secrets/teagramd-turnstile",
      Mode:"ro",
      RW:false,
      Propagation:"rprivate"
    }]
')
turnstile_enabled_snapshot=$(snapshot_from_json \
  "$turnstile_inspect" "$turnstile_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.config_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.config_sha256' <<< "$turnstile_enabled_snapshot")" ]
[ "$(jq -r '.mounts_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.mounts_sha256' <<< "$turnstile_enabled_snapshot")" ]
printf '%s\n' 'PASS mapped Turnstile credentials use a read-only file mount without changing rollout equivalence'

turnstile_writable_inspect=$(printf '%s' "$turnstile_inspect" | jq -c '.Mounts[0].RW=true')
if snapshot_from_json "$turnstile_writable_inspect" "$turnstile_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/turnstile-writable-mount.err"; then
  printf '%s\n' 'FAIL writable Turnstile secret mount was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS writable Turnstile secret mount is rejected'

turnstile_unsafe_source_compose=$(printf '%s' "$turnstile_compose" | jq -c '.secrets.teagramd_turnstile.file="/etc/passwd"')
turnstile_unsafe_source_inspect=$(printf '%s' "$turnstile_inspect" | jq -c '.Mounts[0].Source="/etc/passwd"')
if snapshot_from_json "$turnstile_unsafe_source_inspect" "$turnstile_unsafe_source_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/turnstile-unsafe-source.err"; then
  printf '%s\n' 'FAIL unapproved Turnstile secret source was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS unapproved Turnstile secret source is rejected'

turnstile_secret_env_inspect=$(printf '%s' "$turnstile_inspect" | jq -c '.Config.Env += ["TG_TURNSTILE_SECRET_KEY=fixture-secret"]')
if snapshot_from_json "$turnstile_secret_env_inspect" "$turnstile_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/turnstile-secret-env.err"; then
  printf '%s\n' 'FAIL Turnstile secret in container environment was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS Turnstile secret in container environment is rejected'

printf 'TEAGRAM_TURNSTILE_SITE_KEY=fixture-site-key\nTEAGRAM_TURNSTILE_SECRET_KEY_FILE=%s\n' \
  "$turnstile_secret_source" > "$ENV_FILE"
turnstile_alias_compose=$(jq -c --arg source "$turnstile_secret_source" '
  .services.teagramd.environment += {
    TG_TURNSTILE_SITE_KEY:"fixture-site-key",
    TG_TURNSTILE_SECRET_KEY_FILE:"/run/secrets/teagramd-turnstile",
    TG_TURNSTILE_SIGNIN_ENABLED:"false"
  }
  | .services.teagramd.secrets = [{source:"teagramd_turnstile",target:"teagramd-turnstile"}]
  | .secrets.teagramd_turnstile = {file:$source}
' <<< "$COMPOSE_JSON")
turnstile_alias_inspect=$(printf '%s' "$turnstile_legacy_inspect" | jq -c --arg source "$turnstile_secret_source" '
  .Config.Env += [
    "TG_TURNSTILE_SITE_KEY=fixture-site-key",
    "TG_TURNSTILE_SECRET_KEY_FILE=/run/secrets/teagramd-turnstile",
    "TG_TURNSTILE_SIGNIN_ENABLED=false"
  ]
  | .Mounts += [{
      Type:"bind",
      Source:$source,
      Destination:"/run/secrets/teagramd-turnstile",
      Mode:"ro",
      RW:false,
      Propagation:"rprivate"
    }]
')
turnstile_alias_snapshot=$(ROLLOUT_ADDED_ENV=TEAGRAM_TURNSTILE_SITE_KEY,TEAGRAM_TURNSTILE_SECRET_KEY_FILE \
  snapshot_from_json "$turnstile_alias_inspect" "$turnstile_alias_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.config_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.config_sha256' <<< "$turnstile_alias_snapshot")" ]
[ "$(jq -r '.mounts_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.mounts_sha256' <<< "$turnstile_alias_snapshot")" ]
[ "$(jq -r '.container_env_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.container_env_sha256' <<< "$turnstile_alias_snapshot")" ]
[ "$(jq -r '.container_env_added' <<< "$turnstile_alias_snapshot")" = "TEAGRAM_TURNSTILE_SECRET_KEY_FILE,TEAGRAM_TURNSTILE_SITE_KEY" ]
printf '%s\n' 'PASS named TEAGRAM host additions verify their mapped TG container settings'

turnstile_signin_snapshot() {
  local value=$1 name=$2 inspected compose
  printf 'TEAGRAM_TURNSTILE_SITE_KEY=fixture-site-key\nTEAGRAM_TURNSTILE_SECRET_KEY_FILE=%s\nTG_TURNSTILE_SIGNIN_ENABLED=%s\n' \
    "$turnstile_secret_source" "$value" > "$ENV_FILE"
  inspected=$(printf '%s' "$turnstile_alias_inspect" | jq -c --arg value "$value" '
    .Config.Env |= map(if startswith("TG_TURNSTILE_SIGNIN_ENABLED=") then "TG_TURNSTILE_SIGNIN_ENABLED=" + $value else . end)
  ')
  compose=$(printf '%s' "$turnstile_alias_compose" | jq -c --arg value "$value" \
    '.services.teagramd.environment.TG_TURNSTILE_SIGNIN_ENABLED=$value')
  ROLLOUT_CHANGED_ENV=TG_TURNSTILE_SIGNIN_ENABLED \
    snapshot_from_json "$inspected" "$compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render "$name"
}

turnstile_signin_off=$(turnstile_signin_snapshot false baseline)
turnstile_signin_on=$(turnstile_signin_snapshot true target)
[ "$(jq -r '.container_env_changed' <<< "$turnstile_signin_off")" = none ]
[ "$(jq -r '.container_env_changed' <<< "$turnstile_signin_on")" = TG_TURNSTILE_SIGNIN_ENABLED ]
[ "$(jq -r '.container_env_sha256' <<< "$turnstile_signin_off")" = "$(jq -r '.container_env_sha256' <<< "$turnstile_signin_on")" ]
printf '%s\n' 'PASS named Turnstile sign-in changes accept only boolean values and preserve other environment equivalence'

printf 'TG_TURNSTILE_SIGNIN_ENABLED=false\n' > "$ENV_FILE"
turnstile_signin_old_inspect=$(printf '%s' "$turnstile_alias_inspect" | jq -c '
  .Config.Env |= map(if startswith("TG_TURNSTILE_SIGNIN_ENABLED=") then "TG_TURNSTILE_SIGNIN_ENABLED=true" else . end)
')
ROLLOUT_CHANGED_ENV=TG_TURNSTILE_SIGNIN_ENABLED \
  snapshot_from_json "$turnstile_signin_old_inspect" "$turnstile_alias_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline >/dev/null
if ROLLOUT_CHANGED_ENV=TG_TURNSTILE_SIGNIN_ENABLED \
  snapshot_from_json "$turnstile_signin_old_inspect" "$turnstile_alias_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/turnstile-signin-old-target.err"; then
  printf '%s\n' 'FAIL target kept the previous Turnstile sign-in value' >&2
  exit 1
fi
if snapshot_from_json "$turnstile_signin_old_inspect" "$turnstile_alias_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline \
  >/dev/null 2>"$TMP/turnstile-signin-unnamed.err"; then
  printf '%s\n' 'FAIL baseline accepted an unnamed Turnstile sign-in difference' >&2
  exit 1
fi
printf '%s\n' 'PASS baseline may hold the previous value of a named Turnstile sign-in change; target and unnamed differences may not'

printf 'TEAGRAM_TURNSTILE_SITE_KEY=fixture-site-key\nTEAGRAM_TURNSTILE_SECRET_KEY_FILE=%s\n' \
  "$turnstile_secret_source" > "$ENV_FILE"
turnstile_enable_baseline=$(ROLLOUT_ADDED_ENV=TEAGRAM_TURNSTILE_SITE_KEY,TEAGRAM_TURNSTILE_SECRET_KEY_FILE \
  snapshot_from_json "$turnstile_disabled_inspect" "$turnstile_alias_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline)
[ "$(jq -r '.container_env_added' <<< "$turnstile_enable_baseline")" = none ]
[ "$(jq -r '.container_env_sha256' <<< "$turnstile_enable_baseline")" = "$(jq -r '.container_env_sha256' <<< "$turnstile_alias_snapshot")" ]
if ROLLOUT_ADDED_ENV=TEAGRAM_TURNSTILE_SITE_KEY,TEAGRAM_TURNSTILE_SECRET_KEY_FILE \
  snapshot_from_json "$turnstile_disabled_inspect" "$turnstile_alias_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/turnstile-enable-target.err"; then
  printf '%s\n' 'FAIL target kept the disabled Turnstile defaults' >&2
  exit 1
fi
printf 'TG_TURNSTILE_SIGNIN_ENABLED=false\n' > "$ENV_FILE"
turnstile_signin_add_baseline=$(ROLLOUT_ADDED_ENV=TG_TURNSTILE_SIGNIN_ENABLED \
  snapshot_from_json "$turnstile_disabled_inspect" "$turnstile_disabled_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline)
[ "$(jq -r '.container_env_added' <<< "$turnstile_signin_add_baseline")" = none ]
printf '%s\n' 'PASS live disabled Turnstile defaults count as unset when the deploy adds Turnstile settings'
