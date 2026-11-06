#!/usr/bin/env bash
set -Eeuo pipefail
ROOT=$(cd -P "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
tmp=$(mktemp -d)
project="botapi$(basename "$tmp" | tr -cd '[:alnum:]' | tr '[:upper:]' '[:lower:]')"
compose=(docker compose -p "$project" -f "$tmp/fixture.json")
cleanup() {
  local status=$?
  trap - EXIT
  if [[ -f "$tmp/fixture.json" ]]; then
    "${compose[@]}" down --volumes >/dev/null || status=1
  fi
  rm -rf -- "$tmp" || status=1
  exit "$status"
}
trap cleanup EXIT

published=$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
POSTGRES_PASSWORD=fixture TG_PUBLIC_LINK_PREFIX=https://example.invalid \
  docker compose -f "$ROOT/docker-compose.yml" --profile bot-api-qa config --format json |
  jq --arg name "$project" --arg port "$published" '
    .services as $s
    | {name:$name, services:{
        teagramd:{image:$s["bot-api-inner"].image,
          command:["TCP-LISTEN:2447,bind=127.0.0.1,fork","EXEC:/bin/cat"],networks:{default:null}},
        "bot-api-inner":($s["bot-api-inner"] | del(.profiles)),
        "bot-api-ingress":($s["bot-api-ingress"] | del(.profiles) | .ports[0].published=$port)
      }, networks:{default:{},bot_api_ingress:{driver:"bridge",enable_ipv6:false}},volumes:{bot_api_socket:{}}}
  ' > "$tmp/fixture.json"
image=$(jq -r '.services["bot-api-ingress"].image' "$tmp/fixture.json")
"${compose[@]}" up -d --wait --wait-timeout 60
probe() {
  local port result ingress
  ingress=$("${compose[@]}" ps -q bot-api-ingress)
  port=$(docker inspect "$ingress" | jq -er '.[0].NetworkSettings.Ports["2448/tcp"] | select(length == 1 and .[0].HostIp == "127.0.0.1") | .[0].HostPort')
  [[ "$port" =~ ^[0-9]+$ ]] || return 1
  result=$(docker run --rm --network host --entrypoint /bin/sh "$image" -c \
    'printf "PROXY TCP4 100.64.0.1 100.64.0.2 1 2\r\n" | socat -T2 - "TCP:127.0.0.1:$1"' sh "$port")
  [[ "$result" == $'PROXY TCP4 100.64.0.1 100.64.0.2 1 2\r' ]]
}
probe
server_id=$("${compose[@]}" ps -q teagramd)
ingress_id=$("${compose[@]}" ps -q bot-api-ingress)
server_ip=$(docker inspect "$server_id" | jq -er --arg net "${project}_default" '.[0].NetworkSettings.Networks[$net].IPAddress')
ingress_ip=$(docker inspect "$ingress_id" | jq -er --arg net "${project}_bot_api_ingress" '.[0].NetworkSettings.Networks[$net].IPAddress')
server_gateway=$(docker inspect "$server_id" | jq -er --arg net "${project}_default" '.[0].NetworkSettings.Networks[$net].Gateway | select(length > 0)')
docker run --rm --network "${project}_default" --entrypoint /bin/sh "$image" -c 'exit 0'
for target in "$server_ip:2447" "$ingress_ip:2448" "$server_gateway:$published"; do
  if docker run --rm --network "${project}_default" --entrypoint /bin/sh "$image" -c \
    'socat -T2 -u /dev/null "TCP:$1,connect-timeout=3"' sh "$target" >/dev/null 2>&1; then
    printf 'unexpected cross-network Bot API access to %s\n' "$target" >&2
    exit 1
  fi
done
"${compose[@]}" up -d --no-deps --force-recreate teagramd
"${compose[@]}" up -d --no-deps --force-recreate --wait --wait-timeout 60 bot-api-inner bot-api-ingress
probe
printf '%s\n' 'Bot API relays preserve PROXY bytes across server replacement'
