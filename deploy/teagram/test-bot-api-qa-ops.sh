#!/usr/bin/env bash
set -Eeuo pipefail
ROOT=$(cd -P "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT
repo="$tmp/teagram"
mkdir -p "$repo/.state" "$tmp/bin"
chmod 700 "$repo/.state"
export MOCK_REPO="$repo" MOCK_TMP="$tmp"
export OPS_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export MOCK_SERVER=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
export MOCK_INNER=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
export MOCK_INGRESS=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
printf '%s\n' 'TG_BOT_API_LISTEN_ADDR=127.0.0.1:2447' > "$repo/.env"
printf 'env_sha256=%s source_sha=%s\n' "$(sha256sum "$repo/.env" | cut -d' ' -f1)" "$OPS_SHA" > "$repo/.state/rollout-env"
printf '%s\n' '["TG_BOT_API_LISTEN_ADDR=127.0.0.1:2447"]' > "$repo/.runtime-env"
: > "$repo/.runtime-relays"
: > "$tmp/runner"
cat > "$tmp/helper.py" <<'PY'
import json, os
repo = os.environ['MOCK_REPO']
print(json.dumps({'outcome': 's3-accepted', 'env_files': [repo + '/.env', repo + '/.env.rustfs-parked']}))
PY
chmod 600 "$repo/.env" "$repo/.state/rollout-env" "$tmp/runner" "$tmp/helper.py"
POSTGRES_PASSWORD=fixture TG_PUBLIC_LINK_PREFIX=https://example.invalid \
  docker compose -f "$ROOT/docker-compose.yml" --profile bot-api-qa config --format json > "$tmp/config.json"
printf '%s\n' '{"TCP":{"443":{"HTTPS":true}}}' > "$tmp/serve.json"
python3 - "$ROOT/deploy/teagram/ops/bot-api-qa.sh" "$tmp/op.sh" "$repo" "$tmp" "$(id -u)" <<'PY'
from pathlib import Path
import sys
s = Path(sys.argv[1]).read_text()
changes = {
    'readonly REPO=/opt/teagram': f'readonly REPO={sys.argv[3]}',
    'readonly RUNNER_DIR=/root/teagramd-rollout-runner': f'readonly RUNNER_DIR={sys.argv[4]}',
    'readonly RUNNER=$RUNNER_DIR/rollout-runner.sh': f'readonly RUNNER={sys.argv[4]}/runner',
    'readonly BLOB_HELPER=$RUNNER_DIR/blob-transition-runner.py': f'readonly BLOB_HELPER={sys.argv[4]}/helper.py',
    'readonly EXPECTED_ROOT_UID=0': f'readonly EXPECTED_ROOT_UID={sys.argv[5]}',
    '[[ $(id -u) == 0 &&': f'[[ $(id -u) == {sys.argv[5]} &&',
    'readonly LOCK_PATH=/tmp/teagram-deploy.lock': f'readonly LOCK_PATH={sys.argv[4]}/lock',
}
for old, new in changes.items():
    if s.count(old) != 1:
        raise SystemExit(f'fixture expected one occurrence: {old}')
    s = s.replace(old, new)
Path(sys.argv[2]).write_text(s)
PY
cat > "$tmp/bin/git" <<'SH'
#!/usr/bin/env bash
set -eu
case "$*" in
  *'rev-parse HEAD') printf '%s\n' "$OPS_SHA" ;;
  *'branch --show-current') printf '%s\n' main ;;
  *'diff --quiet '*docker-compose.yml) : ;;
  *) exit 1 ;;
esac
SH
cat > "$tmp/bin/tailscale" <<'SH'
#!/usr/bin/env bash
set -eu
case "$*" in
  'serve status --json') cat "$MOCK_TMP/serve.json" ;;
  'serve --help') printf '%s\n' --proxy-protocol ;;
  'ip -4') printf '%s\n' 100.64.1.1 ;;
  'ip -6') printf '%s\n' fd7a:115c:a1e0::1 ;;
  'serve --yes --bg --tcp=2447 --proxy-protocol=1 tcp://127.0.0.1:2447')
    jq '.TCP["2447"]={"TCPForward":"127.0.0.1:2447","ProxyProtocol":1}' "$MOCK_TMP/serve.json" > "$MOCK_TMP/next"; mv "$MOCK_TMP/next" "$MOCK_TMP/serve.json" ;;
  'serve --yes --tcp=2447 off')
    jq 'del(.TCP["2447"])' "$MOCK_TMP/serve.json" > "$MOCK_TMP/next"; mv "$MOCK_TMP/next" "$MOCK_TMP/serve.json" ;;
  *) printf 'unexpected tailscale command: %s\n' "$*" >&2; exit 1 ;;
esac
SH
cat > "$tmp/bin/ss" <<'SH'
#!/usr/bin/env bash
if [[ -f "$MOCK_REPO/.runtime-relays" ]]; then
  printf '%s\n' 'LISTEN 0 128 127.0.0.1:2447 0.0.0.0:* users:(("docker-proxy"))'
fi
SH
cat > "$tmp/bin/docker" <<'SH'
#!/usr/bin/env bash
set -eu
image='mirror.gcr.io/alpine/socat:1.8.0.3@sha256:beb4a68d9e4fe6b0f21ea774a0fde6c31f580dde6368939ed70100c5385b015e'
case "$*" in
  'inspect --format '*org.opencontainers*) printf '%s\n' "$OPS_SHA" ;;
  'inspect --format '*com.docker.compose.project.config_files*) printf '%s/docker-compose.yml,%s/docker-compose.override.yml\n' "$MOCK_REPO" "$MOCK_REPO" ;;
  'inspect --format {{json .Config.Env}} teagram-teagramd-1') cat "$MOCK_REPO/.runtime-env" ;;
  'inspect --format {{range .Config.Env}}{{println .}}{{end}} teagram-teagramd-1') jq -r '.[]' "$MOCK_REPO/.runtime-env" ;;
  'inspect --format {{.Id}} '*) printf '%s\n' "$MOCK_SERVER" ;;
  'compose --profile bot-api-qa config --format json') cat "$MOCK_TMP/config.json" ;;
  'compose --profile bot-api-qa ps -q bot-api-inner')
    if [[ -f "$MOCK_REPO/.runtime-relays" ]]; then printf '%s\n' "$MOCK_INNER"; fi ;;
  'compose --profile bot-api-qa ps -q bot-api-ingress')
    if [[ -f "$MOCK_REPO/.runtime-relays" ]]; then printf '%s\n' "$MOCK_INGRESS"; fi ;;
  'compose --profile bot-api-qa rm --stop --force bot-api-inner bot-api-ingress') rm -f "$MOCK_REPO/.runtime-relays" ;;
  'compose --profile bot-api-qa up '*) : > "$MOCK_REPO/.runtime-relays" ;;
  'compose up -d --no-deps --force-recreate --wait --wait-timeout 60 teagramd')
    listener=$(awk -F= '$1 == "TG_BOT_API_LISTEN_ADDR" { value=substr($0, index($0, "=") + 1) } END { print value }' "$MOCK_REPO/.env")
    jq -nc --arg listener "$listener" '["TG_BOT_API_LISTEN_ADDR=" + $listener]' > "$MOCK_REPO/.runtime-env" ;;
  'ps --all --quiet --no-trunc --filter volume=teagram_bot_api_socket') printf '%s\n%s\n' "$MOCK_INNER" "$MOCK_INGRESS" ;;
  'ps --all --quiet --no-trunc') printf '%s\n%s\n%s\n' "$MOCK_SERVER" "$MOCK_INNER" "$MOCK_INGRESS" ;;
  'network inspect teagram_bot_api_ingress --format {{.Id}}') printf '%s\n' network ;;
  'network inspect network') jq -nc --arg id "$MOCK_INGRESS" '[{Internal:false,Containers:{($id):{}}}]' ;;
  inspect*)
    jq -nc --arg inner "$MOCK_INNER" --arg ingress "$MOCK_INGRESS" --arg server "$MOCK_SERVER" --arg image "$image" --args '
      [ $ARGS.positional[] as $id | if $id == $inner then
        {Id:$inner,State:{Running:true,Health:{Status:"healthy"}},Config:{Entrypoint:["socat"],Image:$image,Cmd:["-d","-d","-T120","UNIX-LISTEN:/run/bot-api/relay.sock,fork,unlink-early,mode=0600,max-children=128","TCP:127.0.0.1:2447"]},HostConfig:{NetworkMode:("container:"+$server),PortBindings:{},ReadonlyRootfs:true},Mounts:[{Destination:"/run/bot-api",Name:"teagram_bot_api_socket"}]}
      elif $id == $ingress then
        {Id:$ingress,State:{Running:true,Health:{Status:"healthy"}},Config:{Entrypoint:["socat"],Image:$image,Cmd:["-d","-d","-T120","TCP-LISTEN:2448,bind=0.0.0.0,reuseaddr,fork,max-children=128","UNIX-CONNECT:/run/bot-api/relay.sock"]},HostConfig:{NetworkMode:"teagram_bot_api_ingress",ReadonlyRootfs:true,PortBindings:{"2448/tcp":[{HostIp:"127.0.0.1",HostPort:"2447"}]}},Mounts:[{Destination:"/run/bot-api",Name:"teagram_bot_api_socket"}],NetworkSettings:{Networks:{teagram_bot_api_ingress:{}},Ports:{"2448/tcp":[{HostIp:"127.0.0.1",HostPort:"2447"}]}}}
      else {Id:$server,HostConfig:{NetworkMode:"teagram_default"}} end ]
    ' "${@:2}" ;;
  *) printf 'unexpected docker command: %s\n' "$*" >&2; exit 1 ;;
esac
SH
chmod 755 "$tmp/bin/"*
export PATH="$tmp/bin:$PATH"
bash "$tmp/op.sh" check
bash "$tmp/op.sh" apply
bash "$tmp/op.sh" apply
bash "$tmp/op.sh" verify
bash "$tmp/op.sh" rollback
jq -e '.TCP == {"443":{"HTTPS":true}}' "$tmp/serve.json" >/dev/null
: > "$repo/.env"
printf 'env_sha256=%s source_sha=%s\n' "$(sha256sum "$repo/.env" | cut -d' ' -f1)" "$OPS_SHA" > "$repo/.state/rollout-env"
printf '%s\n' '["TG_BOT_API_LISTEN_ADDR="]' > "$repo/.runtime-env"
rm -f "$repo/.runtime-relays"
cat > "$tmp/runner" <<'SH'
#!/usr/bin/env bash
set -eu
[[ "$*" == "apply $OPS_SHA $OPS_SHA" && "$ROLLOUT_RUNNER_ADDED_ENV" == TG_BOT_API_LISTEN_ADDR ]]
printf 'env_sha256=%s source_sha=%s\n' "$(sha256sum "$MOCK_REPO/.env" | cut -d' ' -f1)" "$OPS_SHA" > "$MOCK_REPO/.state/rollout-env"
jq -nc '["TG_BOT_API_LISTEN_ADDR=127.0.0.1:2447"]' > "$MOCK_REPO/.runtime-env"
SH
chmod 600 "$tmp/runner"
bash "$tmp/op.sh" apply
grep -Fxq 'TG_BOT_API_LISTEN_ADDR=127.0.0.1:2447' "$repo/.env"
[[ -f "$repo/.state/.env.main-3359-before-bot-api" ]]
bash "$tmp/op.sh" verify
bash "$tmp/op.sh" rollback
: > "$repo/.env"
printf 'env_sha256=%s source_sha=%s\n' "$(sha256sum "$repo/.env" | cut -d' ' -f1)" "$OPS_SHA" > "$repo/.state/rollout-env"
printf '%s\n' '["TG_BOT_API_LISTEN_ADDR="]' > "$repo/.runtime-env"
rm -f "$repo/.runtime-relays"
cat > "$tmp/runner" <<'SH'
#!/usr/bin/env bash
set -eu
[[ "$*" == "apply $OPS_SHA $OPS_SHA" && "$ROLLOUT_RUNNER_ADDED_ENV" == TG_BOT_API_LISTEN_ADDR ]]
jq -nc '["TG_BOT_API_LISTEN_ADDR=127.0.0.1:2447"]' > "$MOCK_REPO/.runtime-env"
: > "$MOCK_REPO/.runtime-relays"
printf '%s\n' 'fixture runner failed after replacing the server' >&2
exit 23
SH
chmod 600 "$tmp/runner"
failure_status=0
bash "$tmp/op.sh" apply > "$tmp/after-replacement.stdout" 2> "$tmp/after-replacement.stderr" || failure_status=$?
[[ "$failure_status" == 23 ]]
[[ ! -s "$repo/.env" ]]
[[ "$(cat "$repo/.state/rollout-env")" == "env_sha256=$(sha256sum "$repo/.env" | cut -d' ' -f1) source_sha=$OPS_SHA" ]]
jq -e '. == ["TG_BOT_API_LISTEN_ADDR="]' "$repo/.runtime-env" >/dev/null
[[ ! -e "$repo/.runtime-relays" ]]
failure_log=$(find "$repo/.state" -maxdepth 1 -type f -name 'bot-api-qa-runner-failure.*' -print -quit)
[[ -n "$failure_log" && "$(stat -c '%u:%a' "$failure_log")" == "$(id -u):600" ]]
grep -Fxq 'fixture runner failed after replacing the server' "$failure_log"
grep -q 'previous environment and server restored' "$tmp/after-replacement.stderr"
: > "$repo/.env"
printf 'env_sha256=%s source_sha=%s\n' "$(sha256sum "$repo/.env" | cut -d' ' -f1)" "$OPS_SHA" > "$repo/.state/rollout-env"
printf '%s\n' '["TG_BOT_API_LISTEN_ADDR="]' > "$repo/.runtime-env"
printf '%s\n' '#!/usr/bin/env bash' 'exit 23' > "$tmp/runner"
chmod 600 "$tmp/runner"
if bash "$tmp/op.sh" apply > "$tmp/before-replacement.stdout" 2> "$tmp/before-replacement.stderr"; then
  printf '%s\n' 'failed runner was accepted' >&2; exit 1
fi
[[ ! -s "$repo/.env" ]]
jq -e '.TCP == {"443":{"HTTPS":true}}' "$tmp/serve.json" >/dev/null
printf '%s\n' '{"TCP":{"2447":{"TCPForward":"127.0.0.1:9999"}}}' > "$tmp/serve.json"
if bash "$tmp/op.sh" apply 2>/dev/null; then
  printf '%s\n' 'operation accepted a foreign mapping' >&2
  exit 1
fi
printf '%s\n' 'Bot API operation fixture passed'
