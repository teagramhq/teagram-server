#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -P "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
# shellcheck source=deploy/teagram/ops/bot-api-qa.sh
source "$ROOT/deploy/teagram/ops/bot-api-qa.sh"

tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT
calls="$tmp/docker-calls"
server_container=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
mock_server_id=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
mock_inner_id=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
mock_ingress_id=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
orphan_inner_id=
server_env='[]'

docker() {
  printf '%s\n' "$*" >> "$calls"
  case "$*" in
    "inspect --format {{json .Config.Env}} $server_container") printf '%s\n' "$server_env" ;;
    "inspect --format {{.Id}} $server_container") printf '%s\n' "$mock_server_id" ;;
    "compose --profile bot-api-qa ps --all -q bot-api-inner")
      [[ -z "$orphan_inner_id" ]] || printf '%s\n' "$orphan_inner_id"
      ;;
    "compose --profile bot-api-qa ps --all -q bot-api-ingress") ;;
    "compose --profile bot-api-qa up -d --no-deps --force-recreate --wait --wait-timeout 60 bot-api-inner") ;;
    "compose --profile bot-api-qa up -d --no-deps --wait --wait-timeout 60 bot-api-ingress") ;;
    "compose --profile bot-api-qa ps -q bot-api-inner") printf '%s\n' "$mock_inner_id" ;;
    "compose --profile bot-api-qa ps -q bot-api-ingress") printf '%s\n' "$mock_ingress_id" ;;
    "inspect --format {{.State.Health.Status}} $mock_inner_id"|"inspect --format {{.State.Health.Status}} $mock_ingress_id") printf '%s\n' healthy ;;
    "inspect --format {{.HostConfig.NetworkMode}} $mock_inner_id") printf 'container:%s\n' "$mock_server_id" ;;
    *) printf 'unexpected docker call: %s\n' "$*" >&2; return 1 ;;
  esac
}

restore_bot_api_inner optional "$server_container" docker compose
if grep -Fq ' up ' "$calls"; then
  printf '%s\n' 'optional restore started relays without a Bot API listener' >&2
  exit 1
fi

: > "$calls"
orphan_inner_id=$mock_inner_id
if restore_bot_api_inner optional "$server_container" docker compose; then
  printf '%s\n' 'optional restore accepted existing relays without a Bot API listener' >&2
  exit 1
fi
grep -Fq 'compose --profile bot-api-qa ps --all -q bot-api-inner' "$calls"
if grep -Fq ' up ' "$calls"; then
  printf '%s\n' 'optional restore started orphan relays without a Bot API listener' >&2
  exit 1
fi

: > "$calls"
orphan_inner_id=
server_env='["TG_BOT_API_LISTEN_ADDR=127.0.0.1:2447"]'
restore_bot_api_inner optional "$server_container" docker compose
grep -Fq 'compose --profile bot-api-qa up -d --no-deps --force-recreate --wait --wait-timeout 60 bot-api-inner' "$calls"
grep -Fq 'compose --profile bot-api-qa up -d --no-deps --wait --wait-timeout 60 bot-api-ingress' "$calls"
printf '%s\n' 'optional Bot API relay restoration verified'
