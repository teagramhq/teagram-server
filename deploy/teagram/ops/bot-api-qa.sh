#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

restore_bot_api_inner() {
  local mode=$1 server_container=$2 expected_listener=127.0.0.1:2447
  local environment listener inner_id ingress_id server_id inner_health inner_network ingress_health
  shift 2
  [[ "$mode" == optional || "$mode" == required ]] || return 1
  [[ "$#" -gt 0 ]] || return 1

  environment=$(docker inspect --format '{{json .Config.Env}}' "$server_container") || {
    printf '%s\n' 'cannot inspect the server environment for Bot API relay recovery' >&2
    return 1
  }
  listener=$(jq -r '
    if type != "array" or any(.[]; type != "string") then error("invalid container environment")
    else [ .[] | select(startswith("TG_BOT_API_LISTEN_ADDR=")) ] as $settings
      | if ($settings | length) > 1 then error("duplicate Bot API listener setting")
        elif ($settings | length) == 1 then ($settings[0] | sub("^TG_BOT_API_LISTEN_ADDR="; ""))
        else "" end
    end
  ' <<< "$environment") || {
    printf '%s\n' 'cannot read the Bot API listener setting for relay recovery' >&2
    return 1
  }
  if [[ "$mode" == optional && -z "$listener" ]]; then
    inner_id=$("$@" --profile bot-api-qa ps --all -q bot-api-inner) || return 1
    ingress_id=$("$@" --profile bot-api-qa ps --all -q bot-api-ingress) || return 1
    [[ -z "$inner_id" && -z "$ingress_id" ]] && return 0
  fi
  [[ "$listener" == "$expected_listener" ]] || {
    printf '%s\n' 'Bot API relay recovery requires the reviewed loopback listener' >&2
    return 1
  }

  if ! "$@" --profile bot-api-qa up -d --no-deps --force-recreate --wait --wait-timeout 60 bot-api-inner </dev/null; then
    printf '%s\n' 'Bot API inner relay did not become healthy after recreation' >&2
    return 1
  fi
  if ! "$@" --profile bot-api-qa up -d --no-deps --wait --wait-timeout 60 bot-api-ingress </dev/null; then
    printf '%s\n' 'Bot API ingress relay did not become healthy after teagramd startup' >&2
    return 1
  fi

  server_id=$(docker inspect --format '{{.Id}}' "$server_container") || return 1
  inner_id=$("$@" --profile bot-api-qa ps -q bot-api-inner) || return 1
  ingress_id=$("$@" --profile bot-api-qa ps -q bot-api-ingress) || return 1
  [[ "$server_id" =~ ^[0-9a-f]{64}$ && "$inner_id" =~ ^[0-9a-f]{64}$ &&
     "$ingress_id" =~ ^[0-9a-f]{64}$ ]] || {
    printf '%s\n' 'Bot API relay container identities are invalid after recovery' >&2
    return 1
  }
  inner_health=$(docker inspect --format '{{.State.Health.Status}}' "$inner_id") || return 1
  inner_network=$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$inner_id") || return 1
  ingress_health=$(docker inspect --format '{{.State.Health.Status}}' "$ingress_id") || return 1
  [[ "$inner_health" == healthy && "$inner_network" == "container:$server_id" &&
     "$ingress_health" == healthy ]] || {
    printf '%s\n' 'Bot API relays are not healthy in the current teagramd network namespace' >&2
    return 1
  }
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  printf '%s\n' 'bot-api-qa.sh is a sourced relay helper, not an operation' >&2
  exit 64
fi
