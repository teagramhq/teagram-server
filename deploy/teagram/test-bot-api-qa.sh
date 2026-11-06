#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -P "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
config=$(POSTGRES_PASSWORD=fixture TG_PUBLIC_LINK_PREFIX=https://example.invalid \
  docker compose -f "$ROOT/docker-compose.yml" --profile bot-api-qa config --format json)

validate() {
  jq -e '
    .services as $services
    | [ $services | to_entries[] | select(any(.value.volumes[]?; .source == "bot_api_socket")) | .key ] | sort == ["bot-api-ingress", "bot-api-inner"]
  ' <<< "$1" >/dev/null && jq -e '
    .services as $services
    | [ $services | to_entries[] | select(.value.networks.bot_api_ingress? != null or (.value.networks // {} | has("bot_api_ingress"))) | .key ] == ["bot-api-ingress"]
    and $services["bot-api-ingress"].ports == [{"mode":"ingress","host_ip":"127.0.0.1","target":2448,"published":"2447","protocol":"tcp"}]
    and $services["bot-api-inner"].network_mode == "service:teagramd"
    and ($services["bot-api-inner"].ports // [] | length) == 0
    and $services["bot-api-inner"].command == ["-d", "-d", "-T120", "UNIX-LISTEN:/run/bot-api/relay.sock,fork,unlink-early,mode=0600,max-children=128", "TCP:127.0.0.1:2447"]
    and $services["bot-api-ingress"].command == ["-d", "-d", "-T120", "TCP-LISTEN:2448,bind=0.0.0.0,reuseaddr,fork,max-children=128", "UNIX-CONNECT:/run/bot-api/relay.sock"]
    and (.networks.bot_api_ingress.internal // false) == false
  ' <<< "$1" >/dev/null
}

validate "$config"
for unsafe in \
  '.services.teagramd.volumes += [{"source":"bot_api_socket","target":"/run/bot-api"}]' \
  '.services.teagramd.networks.bot_api_ingress = {}' \
  '.services["bot-api-ingress"].ports[0].host_ip = "0.0.0.0"' \
  '.services["bot-api-inner"].command[3] = "TCP-LISTEN:2448,fork"'; do
  bad=$(jq "$unsafe" <<< "$config")
  if validate "$bad"; then
    printf 'unsafe Bot API relay configuration was accepted: %s\n' "$unsafe" >&2
    exit 1
  fi
done
printf '%s\n' 'Bot API relay isolation verified'

# shellcheck source=deploy/teagram/ops/bot-api-qa.sh
source "$ROOT/deploy/teagram/ops/bot-api-qa.sh"
validate_relay_config "$config"
for mutation in \
  '.services["bot-api-inner"].ports = [{"target":2448,"published":"2447","host_ip":"0.0.0.0"}]' \
  '.services["bot-api-ingress"].image = "untrusted:latest"' \
  '.services.rustfs.network_mode = "container:unrelated"' \
  '.services.teagramd.volumes += [{"source":"bot_api_socket","target":"/run/bot-api"}]'; do
  bad=$(jq "$mutation" <<< "$config")
  if validate_relay_config "$bad"; then
    printf 'unsafe operation preflight configuration accepted: %s\n' "$mutation" >&2
    exit 1
  fi
done
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT
mock_state="$tmp/serve.json"
marker="$tmp/original"
changes="$tmp/changes"
: > "$changes"
tailscale() {
  case "$*" in
    'ip -4') printf '%s\n' 100.64.1.1 ;;
    'ip -6') printf '%s\n' fd7a:115c:a1e0::1 ;;
    'serve status --json') cat "$mock_state" ;;
    'serve --yes --bg --tcp=2447 --proxy-protocol=1 tcp://127.0.0.1:2447')
      jq '.TCP["2447"] = {"TCPForward":"127.0.0.1:2447", "ProxyProtocol":1}' "$mock_state" > "$tmp/next"
      mv "$tmp/next" "$mock_state"
      printf '%s\n' apply >> "$changes" ;;
    'serve --yes --tcp=2447 off')
      jq 'del(.TCP["2447"])' "$mock_state" > "$tmp/next"
      mv "$tmp/next" "$mock_state"
      printf '%s\n' off >> "$changes" ;;
    *) printf 'unexpected tailscale call: %s\n' "$*" >&2; return 1 ;;
  esac
}
printf '%s\n' '{"TCP":{"443":{"HTTPS":true}},"Web":{"example:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8081"}}}}}' > "$mock_state"
original=$(jq -cS . "$mock_state")
serve_apply "$marker"
serve_verify
serve_apply "$marker"
[[ $(wc -l < "$changes") == 1 ]]
serve_rollback "$marker"
[[ $(jq -cS . "$mock_state") == "$original" ]]
[[ $(wc -l < "$changes") == 2 ]]
printf '%s\n' '{"TCP":{"2447":{"TCPForward":"127.0.0.1:9999"}}}' > "$mock_state"
if serve_apply "$marker" 2>/dev/null; then
  printf '%s\n' 'foreign Serve mapping was accepted' >&2
  exit 1
fi
[[ ! -e "$marker" && $(wc -l < "$changes") == 2 ]]
printf '%s\n' '{"TCP":{"2447":{"TCPForward":"127.0.0.1:2447"}}}' > "$mock_state"
if serve_apply "$marker" 2>/dev/null; then
  printf '%s\n' 'Serve mapping without PROXY protocol was accepted' >&2
  exit 1
fi
if serve_verify 2>/dev/null; then
  printf '%s\n' 'Serve verification accepted a mapping without PROXY protocol' >&2
  exit 1
fi
[[ ! -e "$marker" && $(wc -l < "$changes") == 2 ]]
printf '%s\n' '{"TCP":{"2447":{"TCPForward":"127.0.0.1:2447","ProxyProtocol":1}}}' > "$mock_state"
serve_apply "$marker"
serve_rollback "$marker"
[[ $(serve_state) == owned && $(wc -l < "$changes") == 2 ]]
printf '%s\n' '{"Foreground":{"session":{"TCP":{"2447":{"TCPForward":"127.0.0.1:2447","ProxyProtocol":1}}}}}' > "$mock_state"
if serve_apply "$marker" 2>/dev/null; then
  printf '%s\n' 'foreground mapping was accepted' >&2; exit 1
fi
printf '%s\n' '{"AllowFunnel":{"example:2447":true}}' > "$mock_state"
if serve_apply "$marker" 2>/dev/null; then
  printf '%s\n' 'Funnel mapping was accepted' >&2; exit 1
fi
printf '%s\n' '{}' > "$mock_state"
serve_apply "$marker"
printf '%s\n' '{"TCP":{"2447":{"TCPForward":"127.0.0.1:9999"}}}' > "$mock_state"
if serve_rollback "$marker" 2>/dev/null; then
  printf '%s\n' 'rollback overwrote a foreign mapping' >&2; exit 1
fi
[[ -f "$marker" && $(jq -r '.TCP["2447"].TCPForward' "$mock_state") == 127.0.0.1:9999 ]]
rm -- "$marker"
printf '%s\n' 'Serve apply, re-apply, conflict refusal and rollback verified'
ss() { cat "$tmp/listeners"; }
printf '%s\n' 'LISTEN 0 128 127.0.0.1:2447 0.0.0.0:* users:(("docker-proxy"))' > "$tmp/listeners"
printf '%s\n' 'LISTEN 0 128 100.64.1.1:2447 0.0.0.0:* users:(("tailscaled"))' >> "$tmp/listeners"
verify_host_listeners
printf '%s\n' 'LISTEN 0 128 0.0.0.0:2447 0.0.0.0:* users:(("docker-proxy"))' >> "$tmp/listeners"
if verify_host_listeners; then
  printf '%s\n' 'wildcard host listener was accepted' >&2; exit 1
fi

(
  export ROLLOUT_RUNNER_SOURCE_ONLY=1
  # shellcheck disable=SC1091
  source "$ROOT/deploy/teagramd/rollout-runner/rollout-runner.sh"
  # Called by reconcile_bot_api_relays from the sourced runner.
  # shellcheck disable=SC2329
  current_service_id() { printf '%s\n' fixture; }
  # shellcheck disable=SC2329
  rollout_compose_service() { printf '%s\n' teagramd; }
  calls="$tmp/runner-calls"
  : > "$calls"
  mock_listener=127.0.0.1:2447
  runner_config=$config
  # shellcheck disable=SC2329
  docker() {
    case "$*" in
      'inspect fixture') jq -nc --arg value "$mock_listener" '[{Config:{Env:[("TG_BOT_API_LISTEN_ADDR=" + $value)]}}]' ;;
      'compose --profile bot-api-qa config --format json') printf '%s\n' "$runner_config" ;;
      'compose --profile bot-api-qa up -d --no-deps --force-recreate --wait --wait-timeout 120 bot-api-inner bot-api-ingress') printf '%s\n' recreated >> "$calls" ;;
      *) printf 'unexpected docker call: %s\n' "$*" >&2; return 1 ;;
    esac
  }
  reconcile_bot_api_relays
  [[ $(wc -l < "$calls") == 1 ]]
  runner_config=$(jq '.services.teagramd.networks.bot_api_ingress = {}' <<< "$config")
  if reconcile_bot_api_relays 2>/dev/null; then
    printf '%s\n' 'rollout accepted an unsafe relay network' >&2
    exit 1
  fi
  [[ $(wc -l < "$calls") == 1 ]]
  runner_config=$config
  mock_listener=100.64.0.1:2447
  if reconcile_bot_api_relays 2>/dev/null; then
    printf '%s\n' 'rollout accepted a non-loopback Bot API listener' >&2
    exit 1
  fi
  [[ $(wc -l < "$calls") == 1 ]]
  mock_listener=
  reconcile_bot_api_relays
  [[ $(wc -l < "$calls") == 1 ]]
)
printf '%s\n' 'rollout relay recreation and isolation guard verified'

(
  export ROLLOUT_VERIFIER_SOURCE_ONLY=1
  # shellcheck disable=SC1091
  source "$ROOT/deploy/teagramd/rollout-runner/rollout-verifier.sh"
  printf '%s\n' 'TG_BOT_API_LISTEN_ADDR=127.0.0.1:2447' > "$tmp/env"
  chmod 600 "$tmp/env"
  inspect='{"Config":{"Env":["TG_BOT_API_LISTEN_ADDR=127.0.0.1:2447"]}}'
  target='{"services":{"teagramd":{"environment":{"TG_BOT_API_LISTEN_ADDR":"127.0.0.1:2447"}}}}'
  verify_added_container_environment "$inspect" "$target" "$tmp/env" '["TG_BOT_API_LISTEN_ADDR"]'
  printf '%s\n' 'TG_BOT_API_LISTEN_ADDR=100.64.0.1:2447' > "$tmp/env"
  if verify_added_container_environment "$inspect" "$target" "$tmp/env" '["TG_BOT_API_LISTEN_ADDR"]' 2>/dev/null; then
    printf '%s\n' 'rollout verifier accepted a non-loopback listener setting' >&2
    exit 1
  fi
)
printf '%s\n' 'rollout listener addition validated'
