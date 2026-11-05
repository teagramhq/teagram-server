#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
export ROLLOUT_VERIFIER_SOURCE_ONLY=1
# shellcheck source=deploy/teagramd/rollout-runner/rollout-verifier.sh
source "$SCRIPT_DIR/../teagramd/rollout-runner/rollout-verifier.sh"

TMP=$(mktemp -d)
trap 'rm -rf -- "$TMP"' EXIT
chmod 700 "$TMP"
ENV_FILE="$TMP/.env"
printf 'TG_BOT_API_LISTEN_ADDR=127.0.0.1:2447\nTEAGRAM_BOT_API_HOST_IP=100.124.236.66\n' > "$ENV_FILE"
chmod 600 "$ENV_FILE"

compose_json=$(jq -cn '
  {
    services: {
      teagramd: {
        image: "teagramd:local",
        environment: {TG_BOT_API_LISTEN_ADDR:"127.0.0.1:2447"},
        networks: {default:{}, rustfs_private:{}},
        ports: [
          {target:2443,published:"2443",host_ip:"127.0.0.1",protocol:"tcp"},
          {target:2448,published:"2447",host_ip:"100.124.236.66",protocol:"tcp",mode:"ingress"}
        ]
      },
      "bot-api-forwarder": {
        image: "teagramd:local",
        entrypoint: ["/usr/local/bin/bot-api-forwarder"],
        network_mode: "service:teagramd",
        depends_on: {teagramd:{condition:"service_started",required:true}},
        restart: "unless-stopped",
        read_only: true,
        logging: {
          driver:"syslog",
          options:{
            "syslog-address":"unixgram:///dev/log",
            "syslog-facility":"daemon",
            "syslog-format":"rfc5424",
            tag:"teagramd-bot-api-forwarder"
          }
        },
        healthcheck: {
          test:["CMD","/usr/local/bin/bot-api-forwarder","-healthcheck"],
          interval:"5s", timeout:"2s", retries:12, start_period:"2s"
        }
      }
    }
  }
')
inspect_json=$(jq -cn '
  {
    Config:{Env:["TG_BOT_API_LISTEN_ADDR=127.0.0.1:2447"]},
    HostConfig:{PortBindings:{"2448/tcp":[{HostIp:"100.124.236.66",HostPort:"2447"}]}}
  }
')

verify_configuration() {
  verify_added_container_environment \
    "$1" "$2" "$3" \
    '["TG_BOT_API_LISTEN_ADDR","TEAGRAM_BOT_API_HOST_IP"]'
  verify_bot_api_configuration \
    "$1" "$2" "$3" \
    '["TG_BOT_API_LISTEN_ADDR","TEAGRAM_BOT_API_HOST_IP"]' \
    "$4" selected-compose-render
}

verify_configuration "$inspect_json" "$compose_json" "$ENV_FILE" target
printf '%s\n' 'PASS reviewed loopback listener and tailnet-only forwarder configuration'

bad_env_file="$TMP/bad.env"
printf 'TG_BOT_API_LISTEN_ADDR=127.0.0.1:2447\nTEAGRAM_BOT_API_HOST_IP=127.0.0.1\n' > "$bad_env_file"
chmod 600 "$bad_env_file"
if verify_added_container_environment \
  "$inspect_json" "$compose_json" "$bad_env_file" \
  '["TG_BOT_API_LISTEN_ADDR","TEAGRAM_BOT_API_HOST_IP"]' >/dev/null 2>&1; then
  printf '%s\n' 'FAIL non-tailnet published host IP was accepted as a named addition' >&2
  exit 1
fi
printf '%s\n' 'PASS named host-IP addition is restricted to the teagram tailnet address'

bad_public_compose=$(jq -c '
  .services.teagramd.ports[1].host_ip="0.0.0.0"
' <<< "$compose_json")
bad_public_inspect=$(jq -c '
  .HostConfig.PortBindings["2448/tcp"][0].HostIp="0.0.0.0"
' <<< "$inspect_json")
if verify_configuration "$bad_public_inspect" "$bad_public_compose" "$ENV_FILE" target >/dev/null 2>&1; then
  printf '%s\n' 'FAIL public Bot API bind was accepted' >&2
  exit 1
fi

bad_sidecar=$(jq -c '
  .services["bot-api-forwarder"].environment={EXTRA:"reachable"}
' <<< "$compose_json")
if verify_configuration "$inspect_json" "$bad_sidecar" "$ENV_FILE" target >/dev/null 2>&1; then
  printf '%s\n' 'FAIL sidecar environment expansion was accepted' >&2
  exit 1
fi

bad_namespace=$(jq -c '
  .services["bot-api-forwarder"].network_mode="host"
' <<< "$compose_json")
if verify_configuration "$inspect_json" "$bad_namespace" "$ENV_FILE" target >/dev/null 2>&1; then
  printf '%s\n' 'FAIL sidecar namespace change was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS public binds, sidecar environment expansion, and namespace changes are rejected'

baseline_env=$(jq -cn '
  {
    services: {
      teagramd: {
        image:"teagramd:local",
        environment:{TG_BOT_API_LISTEN_ADDR:""},
        networks:{default:{},rustfs_private:{}},
        ports:[{target:2448,published:"2447",host_ip:"127.0.0.1",protocol:"tcp"}]
      },
      "bot-api-forwarder": {
        image:"teagramd:local",
        entrypoint:["/usr/local/bin/bot-api-forwarder"],
        network_mode:"service:teagramd",
        depends_on:{teagramd:{condition:"service_started",required:true}},
        restart:"unless-stopped",
        read_only:true,
        logging:{driver:"syslog",options:{
          "syslog-address":"unixgram:///dev/log",
          "syslog-facility":"daemon",
          "syslog-format":"rfc5424",
          tag:"teagramd-bot-api-forwarder"
        }},
        healthcheck:{
          test:["CMD","/usr/local/bin/bot-api-forwarder","-healthcheck"],
          interval:"5s",timeout:"2s",retries:12,start_period:"2s"
        }
      }
    }
  }
')
baseline_inspect=$(jq -cn '{Config:{Env:[]},HostConfig:{PortBindings:{}}}')
baseline_env_file="$TMP/baseline.env"
: > "$baseline_env_file"
chmod 600 "$baseline_env_file"
verify_bot_api_configuration "$baseline_inspect" "$baseline_env" "$baseline_env_file" '[]' baseline selected-compose-render
printf '%s\n' 'PASS first rollout baseline remains verifiable before Bot API activation'
