#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"

demo_require_root
demo_require_env
[[ "$#" -eq 2 ]] || demo_fail 'expected the active private endpoint and a unique smoke marker'

ACTIVE_SERVER=$(demo_active_server)
ACTIVE_ENDPOINT="$ACTIVE_SERVER:2443"
[[ "$1" == "$ACTIVE_ENDPOINT" ]] || demo_fail 'moderation smoke endpoint does not match the active server'
MODERATION_SMOKE_MARKER="$2"
[[ "$MODERATION_SMOKE_MARKER" =~ ^[a-f0-9]{32}$ ]] ||
  demo_fail 'moderation smoke marker must be 32 lowercase hexadecimal characters'

SEED_SCRIPT="$SCRIPT_DIR/seed-demo.sh"
SMOKE_SESSION_FILE="$DEMO_SEED_ACCOUNT_DIR/moderation-smoke.session"
MODERATION_SMOKE_CLEANUP_NEEDED=0

cleanup_moderation_smoke() {
  local rc=$? cleanup_endpoint="$ACTIVE_ENDPOINT" server_id trust
  trap - EXIT
  if ((MODERATION_SMOKE_CLEANUP_NEEDED)); then
    mapfile -t server_ids < <(demo_compose ps --status running -q "$ACTIVE_SERVER")
    if ((${#server_ids[@]} == 1)) && [[ -n "$(demo_compose ps --status running -q haproxy)" ]]; then
      server_id=${server_ids[0]}
      cleanup_endpoint=haproxy:2443
      if trust=$(docker inspect "$server_id" | jq -er '
        .[0].Config.Env[] | select(startswith("TG_CLIENT_ADDR_TRUST=")) | split("=")[1]
      '); then
        if [[ "$trust" == socket ]]; then
          cleanup_endpoint="$ACTIVE_ENDPOINT"
        fi
      fi
    fi
    if ! "$SEED_SCRIPT" --cleanup-general-moderation-smoke \
      --moderation-smoke-marker "$MODERATION_SMOKE_MARKER" --endpoint "$cleanup_endpoint"; then
      printf 'General moderation smoke cleanup failed; fixture credentials remain at %s\n' \
        "$SMOKE_SESSION_FILE" >&2
      rc=1
    fi
  fi
  exit "$rc"
}
trap cleanup_moderation_smoke EXIT

[[ -z "$(demo_compose ps --status running -q edge haproxy)" ]] ||
  demo_fail 'moderation smoke requires the public edge and HAProxy to be stopped before preparation'
mapfile -t private_ids < <(demo_seed_compose ps --status running -q "$ACTIVE_SERVER")
((${#private_ids[@]} == 1)) || demo_fail 'moderation smoke requires one running private seed server'
docker inspect "${private_ids[0]}" | jq -e '
  .[0].Config.Env as $env
  | ($env | any(. == "TG_CLIENT_ADDR_TRUST=socket"))
    and ($env | any(. == "TG_TURNSTILE_SIGNIN_ENABLED=false"))
    and ($env | any(. == "TG_DEMO_GENERAL_CHAT_ENABLED=true"))
' >/dev/null || demo_fail 'moderation smoke preparation requires the private, sign-in-disabled seed server'

MODERATION_SMOKE_CLEANUP_NEEDED=1
prepare_output=$("$SEED_SCRIPT" --prepare-general-moderation-smoke \
  --moderation-smoke-marker "$MODERATION_SMOKE_MARKER" --endpoint "$ACTIVE_ENDPOINT")
mapfile -t message_ids < <(sed -n 's/^general moderation smoke prepared message_id=//p' <<< "$prepare_output")
((${#message_ids[@]} == 1)) && [[ "${message_ids[0]}" =~ ^[1-9][0-9]*$ ]] ||
  demo_fail 'private moderation preparation did not return exactly one message ID'
MODERATION_SMOKE_MESSAGE_ID=${message_ids[0]}
((MODERATION_SMOKE_MESSAGE_ID <= 2147483647)) || demo_fail 'moderation smoke message ID exceeds the MTProto limit'
[[ -f "$SMOKE_SESSION_FILE" && ! -L "$SMOKE_SESSION_FILE" &&
  "$(stat -c '%u:%a' "$SMOKE_SESSION_FILE")" == '0:600' ]] ||
  demo_fail 'private preparation did not persist a root-only short-handle guest session'

# Recreate the active server from the production Compose profile before either
# moderation command. Both sessions were established by the private seed pass;
# the moderation flow must keep working with the configured sign-in gate value.
demo_compose up -d --wait --wait-timeout 120 "$ACTIVE_SERVER" haproxy
mapfile -t production_ids < <(demo_compose ps --status running -q "$ACTIVE_SERVER")
((${#production_ids[@]} == 1)) || demo_fail 'production moderation smoke requires one active server'
if ! PRODUCTION_TURNSTILE_SIGNIN_ENABLED=$(docker inspect "${production_ids[0]}" | jq -er '
  .[0].Config.Env
  | map(select(startswith("TG_TURNSTILE_SIGNIN_ENABLED=")) | split("=")[1])
  | if length == 1 and (.[0] == "true" or .[0] == "false") then .[0] else error("invalid sign-in Turnstile setting") end
'); then
  demo_fail 'production sign-in Turnstile setting is missing or invalid'
fi
docker inspect "${production_ids[0]}" | jq -e --arg signinEnabled "$PRODUCTION_TURNSTILE_SIGNIN_ENABLED" '
  .[0].Config.Env as $env
  | ($env | any(. == "TG_CLIENT_ADDR_TRUST=proxy-v2"))
    and ($env | any(. == ("TG_TURNSTILE_SIGNIN_ENABLED=" + $signinEnabled)))
    and ($env | any(. == "TG_DEMO_GENERAL_CHAT_ENABLED=true"))
' >/dev/null || demo_fail 'production moderation smoke requires the configured sign-in gate and PROXY-v2'
[[ -n "$(demo_compose ps --status running -q haproxy)" ]] || demo_fail 'production HAProxy is not running'
"$SEED_SCRIPT" --verify-turnstile --expect-signin-enabled "$PRODUCTION_TURNSTILE_SIGNIN_ENABLED" \
  --endpoint haproxy:2443

"$SCRIPT_DIR/moderate-general.sh" delete-message "$MODERATION_SMOKE_MESSAGE_ID"
"$SCRIPT_DIR/moderate-general.sh" ban-user ab
"$SEED_SCRIPT" --verify-general-moderation-smoke \
  --moderation-smoke-marker "$MODERATION_SMOKE_MARKER" \
  --moderation-smoke-message-id "$MODERATION_SMOKE_MESSAGE_ID" --endpoint haproxy:2443
"$SEED_SCRIPT" --cleanup-general-moderation-smoke \
  --moderation-smoke-marker "$MODERATION_SMOKE_MARKER" --endpoint haproxy:2443
MODERATION_SMOKE_CLEANUP_NEEDED=0

printf 'production moderation Compose smoke=verified message_deleted=true short_handle_ban=true\n'
