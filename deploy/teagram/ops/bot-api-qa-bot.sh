#!/usr/bin/env bash
set -Eeuo pipefail
set +x
umask 077

readonly REPO=/opt/teagram
readonly SERVICE=teagram-teagramd-1
readonly BOT_USERNAME=teagram_qa_test_bot
readonly BOT_FIRST_NAME='Teagram QA Test'
readonly AGE_RECIPIENT=age1p5s5le72wnxu465pe2xz5a3qv8mdykwp5leezpjk0zm6p0m3e5hs36fydn
readonly PROBE_REQUESTS=24
readonly LOCK_PATH=/tmp/teagram-deploy.lock
readonly EXPECTED_ROOT_UID=0

fail() {
  printf 'Bot API QA bot operation rejected: %s\n' "$1" >&2
  return 1
}

usage() {
  printf '%s\n' 'usage: bot-api-qa-bot <create|rotate|revoke|status|probe>' >&2
}

if [[ $# != 1 ]]; then
  usage
  exit 64
fi
MODE=$1
case "$MODE" in
  create|rotate|revoke|status|probe) ;;
  *) usage; exit 64 ;;
esac

[[ ${EUID:-$(id -u)} == 0 ]] || { fail 'operation requires root'; exit 1; }
[[ ${OPS_SHA:-} =~ ^[0-9a-f]{40}$ ]] || { fail 'reviewed source SHA is invalid'; exit 1; }
if [[ ! -e "$LOCK_PATH" && ! -L "$LOCK_PATH" ]]; then
  ( set -o noclobber; : > "$LOCK_PATH" ) || {
    [[ -e "$LOCK_PATH" || -L "$LOCK_PATH" ]] || { fail 'cannot create deployment lock'; exit 1; }
  }
fi
[[ -f "$LOCK_PATH" && ! -L "$LOCK_PATH" && "$(stat -c %u -- "$LOCK_PATH")" == "$EXPECTED_ROOT_UID" ]] || {
  fail 'deployment lock is unsafe'
  exit 1
}
exec 9>>"$LOCK_PATH" || { fail 'cannot open deployment lock'; exit 1; }
flock -x 9 || { fail 'cannot acquire deployment lock'; exit 1; }
live_sha=$(git -C "$REPO" rev-parse HEAD) || { fail 'cannot inspect the live checkout'; exit 1; }
[[ "$live_sha" == "$OPS_SHA" ]] || { fail 'live checkout differs from reviewed source'; exit 1; }
image_sha=$(docker inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$SERVICE") || {
  fail 'cannot inspect the running server revision'
  exit 1
}
[[ "$image_sha" == "$OPS_SHA" ]] || { fail 'running server differs from reviewed source'; exit 1; }
if [[ "$MODE" == create || "$MODE" == rotate ]]; then
  docker exec "$SERVICE" teagramd bot encrypt --check-recipient "$AGE_RECIPIENT" >/dev/null || {
    fail 'pinned QA encryption recipient is invalid or unavailable'
    exit 1
  }
fi

lookup_bot() {
  docker exec "$SERVICE" teagramd bot lookup "$BOT_USERNAME"
}

read_status() {
  local json=$1
  local state
  state=$(jq -er 'select(type == "object") | .state | select(. == "missing" or . == "found")' <<< "$json") || {
    fail 'bot lookup returned an invalid result'
    return 1
  }
  if [[ "$state" == missing ]]; then
    BOT_STATE=missing
    BOT_ID=
    BOT_ACTIVE=false
    return 0
  fi

  local owner kind first_name id active
  owner=$(jq -er '.owner | select(type == "string")' <<< "$json") || { fail 'bot lookup omitted its owner'; return 1; }
  id=$(jq -er '.id | select(type == "number" and floor == . and . > 0)' <<< "$json") || {
    fail 'bot lookup returned an invalid id'
    return 1
  }
  kind=$(jq -er '.kind // "" | select(type == "string")' <<< "$json") || { fail 'bot lookup omitted its account kind'; return 1; }
  first_name=$(jq -er '.first_name // "" | select(type == "string")' <<< "$json") || { fail 'bot lookup omitted its first name'; return 1; }
  active=$(jq -er 'if (.token_active | type) == "boolean" then (.token_active | tostring) else error("invalid token state") end' <<< "$json") || {
    fail 'bot lookup omitted its token state'
    return 1
  }
  [[ "$owner" == user && "$kind" == bot && "$first_name" == "$BOT_FIRST_NAME" ]] || {
    fail 'pinned username does not resolve to the expected QA bot'
    return 1
  }
  BOT_STATE=found
  BOT_ID=$id
  BOT_ACTIVE=$active
}

status_json=$(lookup_bot) || { fail 'cannot resolve the pinned QA username'; exit 1; }
read_status "$status_json" || exit 1

if [[ "$MODE" == probe ]]; then
  [[ "$BOT_STATE" == found ]] || { fail 'QA bot does not exist'; exit 1; }
  tailnet_ipv4=$(tailscale ip -4) || { fail 'cannot inspect the host tailnet address'; exit 1; }
  python3 - "$tailnet_ipv4" <<'PY' || { fail 'host tailnet address is outside the approved range'; exit 1; }
import ipaddress
import sys

address = ipaddress.ip_address(sys.argv[1])
if address not in ipaddress.ip_network("100.64.0.0/10"):
    raise SystemExit(1)
PY
  probe_token="${BOT_ID}:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
  seen_429=false
  for ((request = 0; request < PROBE_REQUESTS; request++)); do
    if status=$(printf 'url = "http://%s:2447/bot%s/getMe"\n' "$tailnet_ipv4" "$probe_token" |
      curl -q --silent --output /dev/null --write-out '%{http_code}' --connect-timeout 1 --max-time 2 --config - 2>/dev/null); then
      :
    else
      status=000
    fi
    [[ "$status" =~ ^[0-9]{3}$ ]] || status=000
    printf '%s\n' "$status"
    if [[ "$status" == 429 ]]; then
      seen_429=true
    fi
  done
  [[ "$seen_429" == true ]] || { fail 'tailnet burst did not hit the peer auth limit'; exit 1; }
  exit 0
fi

case "$MODE" in
  status)
    if [[ "$BOT_STATE" == missing ]]; then
      printf 'state=missing username=%s\n' "$BOT_USERNAME"
    else
      printf 'state=found bot_id=%s username=%s token_active=%s\n' "$BOT_ID" "$BOT_USERNAME" "$BOT_ACTIVE"
    fi
    exit 0
    ;;
  create)
    if [[ "$BOT_STATE" == found ]]; then
      printf 'state=exists bot_id=%s username=%s token_active=%s\n' "$BOT_ID" "$BOT_USERNAME" "$BOT_ACTIVE"
      exit 0
    fi
    ;;
  rotate)
    [[ "$BOT_STATE" == found ]] || { fail 'QA bot does not exist'; exit 1; }
    ;;
  revoke)
    if [[ "$BOT_STATE" == missing ]]; then
      printf 'state=missing username=%s action=revoke\n' "$BOT_USERNAME"
      exit 0
    fi
    if [[ "$BOT_ACTIVE" == false ]]; then
      printf 'state=revoked bot_id=%s username=%s action=revoke\n' "$BOT_ID" "$BOT_USERNAME"
      exit 0
    fi
    if ! docker exec "$SERVICE" teagramd bot revoke "$BOT_ID" >/dev/null; then
      fail 'cannot revoke the QA bot token'
      exit 1
    fi
    printf 'state=revoked bot_id=%s username=%s action=revoke\n' "$BOT_ID" "$BOT_USERNAME"
    exit 0
    ;;
esac

ACTION=$MODE
EXPECTED_ID=$BOT_ID
if [[ "$MODE" == create ]]; then
  token=$(docker exec "$SERVICE" teagramd bot create "$BOT_USERNAME" "$BOT_FIRST_NAME") || {
    fail 'cannot create the QA bot'
    exit 1
  }
else
  token=$(docker exec "$SERVICE" teagramd bot rotate "$BOT_ID") || {
    fail 'cannot rotate the QA bot token'
    exit 1
  }
fi

if [[ "$token" =~ ^([1-9][0-9]*):[A-Za-z0-9_-]{43}$ ]]; then
  TOKEN_ID=${BASH_REMATCH[1]}
else
  unset token
  fail 'bot command returned an invalid token'
  exit 1
fi
if [[ "$MODE" == rotate && "$TOKEN_ID" != "$EXPECTED_ID" ]]; then
  unset token
  fail 'rotation returned a different bot id'
  exit 1
fi

encrypted=
if ! encrypted=$(printf '%s\n' "$token" | docker exec -i "$SERVICE" teagramd bot encrypt "$AGE_RECIPIENT"); then
  unset token
  if ! docker exec "$SERVICE" teagramd bot revoke "$TOKEN_ID" >/dev/null 2>&1; then
    fail "encryption failed and token revocation also failed for bot id $TOKEN_ID"
    exit 1
  fi
  fail "token encryption failed and bot id $TOKEN_ID was revoked"
  exit 1
fi
unset token
if [[ "$encrypted" != *'-----BEGIN AGE ENCRYPTED FILE-----'* || "$encrypted" != *'-----END AGE ENCRYPTED FILE-----'* ]]; then
  unset encrypted
  if ! docker exec "$SERVICE" teagramd bot revoke "$TOKEN_ID" >/dev/null 2>&1; then
    fail "encrypted output was invalid and token revocation also failed for bot id $TOKEN_ID"
    exit 1
  fi
  fail "encrypted output was invalid and bot id $TOKEN_ID was revoked"
  exit 1
fi

status_json=$(lookup_bot) || {
  unset encrypted
  if ! docker exec "$SERVICE" teagramd bot revoke "$TOKEN_ID" >/dev/null 2>&1; then
    fail "cannot verify the new token and revocation failed for bot id $TOKEN_ID"
    exit 1
  fi
  fail "cannot verify the new token; bot id $TOKEN_ID was revoked"
  exit 1
}
read_status "$status_json" || {
  unset encrypted
  if ! docker exec "$SERVICE" teagramd bot revoke "$TOKEN_ID" >/dev/null 2>&1; then
    fail "cannot verify the new token and revocation failed for bot id $TOKEN_ID"
    exit 1
  fi
  fail "cannot verify the new token; bot id $TOKEN_ID was revoked"
  exit 1
}
if [[ "$BOT_STATE" != found || "$BOT_ID" != "$TOKEN_ID" || "$BOT_ACTIVE" != true ]]; then
  unset encrypted
  if ! docker exec "$SERVICE" teagramd bot revoke "$TOKEN_ID" >/dev/null 2>&1; then
    fail "new token did not match the QA bot and revocation failed for bot id $TOKEN_ID"
    exit 1
  fi
  fail "new token did not match the QA bot; bot id $TOKEN_ID was revoked"
  exit 1
fi

if ! { printf 'action=%s bot_id=%s username=%s\n' "$ACTION" "$TOKEN_ID" "$BOT_USERNAME" && printf '%s\n' "$encrypted"; }; then
  unset encrypted
  if ! docker exec "$SERVICE" teagramd bot revoke "$TOKEN_ID" >/dev/null 2>&1; then
    fail "ciphertext output failed and token revocation also failed for bot id $TOKEN_ID"
    exit 1
  fi
  fail "ciphertext output failed; bot id $TOKEN_ID was revoked"
  exit 1
fi
unset encrypted
