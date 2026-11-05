#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TMP=$(mktemp -d)
trap 'rm -rf -- "$TMP"' EXIT
OPS_SHA=ffffffffffffffffffffffffffffffffffffffff
SERVER_ID=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
SIDECAR_ID=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
CURRENT_UID=$(id -u)

fail() {
  printf 'FAIL %s\n' "$1" >&2
  exit 1
}

pass() {
  printf 'PASS %s\n' "$1"
}

make_fixture() {
  local name=$1 configured=${2:-false}
  local case_dir="$TMP/$name" repo="$TMP/$name/opt/teagram" bin="$TMP/$name/bin"
  local test_op="$TMP/$name/bot-api-qa.sh" runner="$TMP/$name/fixture-runner.sh"
  local helper="$TMP/$name/blob-transition-runner.py" lock="$TMP/$name/teagram-deploy.lock"
  mkdir -p "$repo/.state/blob-mode" "$bin"
  chmod 700 "$repo/.state/blob-mode" "$bin"
  if [[ "$configured" == true ]]; then
    printf 'TEAGRAM_TEST_SETTING=fixture\nTG_BOT_API_LISTEN_ADDR=127.0.0.1:2447\nTEAGRAM_BOT_API_HOST_IP=100.124.236.66\n' > "$repo/.env"
  else
    printf 'TEAGRAM_TEST_SETTING=fixture\n' > "$repo/.env"
  fi
  chmod 600 "$repo/.env"
  cp -- "$repo/.env" "$case_dir/before.env"
  if [[ "$configured" == true ]]; then
    printf 'TEAGRAM_TEST_SETTING=fixture-before-bot-api\n' > "$repo/.state/.env.main-2825-before-bot-api"
    chmod 600 "$repo/.state/.env.main-2825-before-bot-api"
  fi
  printf 'env_sha256=%s source_sha=%s\n' "$(sha256sum "$repo/.env" | awk '{print $1}')" "$OPS_SHA" > "$repo/.state/rollout-env"
  printf '%s\n' "$OPS_SHA" > "$case_dir/image-sha"
  chmod 600 "$repo/.state/rollout-env" "$case_dir/image-sha"
  cat > "$helper" <<PY
import json
print(json.dumps({"outcome": "s3-accepted", "env_files": ["$repo/.env", "$repo/.env.rustfs-parked"]}))
PY
  cat > "$runner" <<'SH'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'runner %s\n' "$*" >> "$MOCK_EVENTS"
case "$MOCK_RUNNER_MODE" in
  success)
    printf 'env_sha256=%s source_sha=%s\n' "$(sha256sum "$MOCK_ENV_FILE" | awk '{print $1}')" "$OPS_SHA" > "$MOCK_RECORD_FILE"
    printf '%s\n' "$OPS_SHA" > "$MOCK_IMAGE_FILE"
    ;;
  target-failed)
    printf '%s\n' "$OPS_SHA" > "$MOCK_IMAGE_FILE"
    exit 23
    ;;
  *) exit 90 ;;
esac
SH
  chmod 600 "$helper" "$runner"
  python3 - "$SCRIPT_DIR/ops/bot-api-qa.sh" "$test_op" "$repo" "$runner" "$helper" "$CURRENT_UID" "$lock" <<'PY'
from pathlib import Path
import sys

source = Path(sys.argv[1]).read_text()
replacements = {
    "readonly REPO=/opt/teagram": f"readonly REPO={sys.argv[3]}",
    "readonly RUNNER_DIR=/root/teagramd-rollout-runner": f"readonly RUNNER_DIR={sys.argv[3]}/.state",
    "readonly RUNNER=$RUNNER_DIR/rollout-runner.sh": f"readonly RUNNER={sys.argv[4]}",
    "readonly BLOB_HELPER=$RUNNER_DIR/blob-transition-runner.py": f"readonly BLOB_HELPER={sys.argv[5]}",
    "readonly EXPECTED_ROOT_UID=0": f"readonly EXPECTED_ROOT_UID={sys.argv[6]}",
    "exec 9>/tmp/teagram-deploy.lock": f"exec 9>{sys.argv[7]}",
}
for old, new in replacements.items():
    if source.count(old) != 1:
        raise SystemExit(f"fixture could not locate one source setting: {old}")
    source = source.replace(old, new)
Path(sys.argv[2]).write_text(source)
PY
  chmod 700 "$test_op"
  cat > "$bin/git" <<'SH'
#!/usr/bin/env bash
set -eu
if [[ " $* " == *' branch --show-current '* ]]; then
  printf '%s\n' main
  exit 0
fi
exit 91
SH
  cat > "$bin/docker" <<'SH'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'docker %s\n' "$*" >> "$MOCK_EVENTS"
listener=$(sed -n 's/^TG_BOT_API_LISTEN_ADDR=//p' "$MOCK_ENV_FILE")
host_ip=$(sed -n 's/^TEAGRAM_BOT_API_HOST_IP=//p' "$MOCK_ENV_FILE")
if [[ "$1" == inspect && "$2" == --format ]]; then
  case "$3" in
    *org.opencontainers.image.revision*) cat "$MOCK_IMAGE_FILE" ;;
    *project.config_files*) printf '%s/docker-compose.yml,%s/docker-compose.override.yml\n' "$MOCK_REPO" "$MOCK_REPO" ;;
    '{{.State.Status}}') printf '%s\n' running ;;
    *Config.Env*) [[ -n "$listener" ]] && printf 'TG_BOT_API_LISTEN_ADDR=%s\n' "$listener" ;;
    '{{.Id}}') printf '%s\n' "$MOCK_SERVER_ID" ;;
    *) exit 92 ;;
  esac
  exit 0
fi
if [[ "$1" == inspect && "$2" == teagram-teagramd-1 ]]; then
  jq -cn --arg host_ip "$host_ip" '[{HostConfig:{PortBindings:{"2448/tcp":[{HostIp:$host_ip,HostPort:"2447"}]}}}]'
  exit 0
fi
if [[ "$1" == inspect && "$2" == "$MOCK_SIDECAR_ID" ]]; then
  jq -cn --arg server_id "$MOCK_SERVER_ID" --arg listener "$listener" '[{
    Id:"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    State:{Status:"running",Health:{Status:"healthy"}},
    HostConfig:{NetworkMode:("container:" + $server_id),ReadonlyRootfs:true,PortBindings:{}},
    Config:{Labels:{"com.docker.compose.service":"bot-api-forwarder"},Image:"teagramd:local",Entrypoint:["/usr/local/bin/bot-api-forwarder"],Env:["TG_BOT_API_LISTEN_ADDR=" + $listener]},
    Mounts:[]
  }]'
  exit 0
fi
if [[ "$1" == ps && "$2" == --quiet ]]; then
  if [[ " $* " == *' --no-trunc '* ]]; then
    printf '%s\n' "$MOCK_SIDECAR_ID"
  else
    printf 'bbbbbbbbbbbb\n'
  fi
  exit 0
fi
if [[ "$1" == compose && "$2" == config && "$3" == --format && "$4" == json ]]; then
  jq -cn --arg listener "$listener" '{services:{teagramd:{environment:{TG_BOT_API_LISTEN_ADDR:$listener}}}}'
  exit 0
fi
if [[ "$1" == exec && " $* " == *' bot create '* ]]; then
  printf '%s\n' "$MOCK_CREATE_TOKEN"
  exit 0
fi
if [[ "$1" == exec && " $* " == *' bot rotate '* ]]; then
  printf '%s\n' "$MOCK_ROTATE_TOKEN"
  exit 0
fi
printf 'unexpected docker fixture call\n' >&2
exit 93
SH
  cat > "$bin/ss" <<'SH'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "$*" == "-Hltn sport = :2447" ]] || exit 94
printf '%s\n' "$MOCK_SS_OUTPUT"
SH
  cat > "$bin/curl" <<'SH'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "$*" == *https://api.ipify.org* ]] || exit 95
printf '%s\n' 8.8.8.8
SH
  chmod 700 "$bin"/*
  printf '%s\n' "$repo" > "$case_dir/repo"
  printf '%s\n' "$test_op" > "$case_dir/op"
  printf '%s\n' "$bin" > "$case_dir/bin-path"
}

run_op() {
  local name=$1 mode=$2
  local case_dir="$TMP/$name" repo bin test_op
  repo=$(cat "$case_dir/repo")
  bin=$(cat "$case_dir/bin-path")
  test_op=$(cat "$case_dir/op")
  set +e
  PATH="$bin:$PATH" \
    MOCK_ENV_FILE="$repo/.env" MOCK_RECORD_FILE="$repo/.state/rollout-env" \
    MOCK_IMAGE_FILE="$case_dir/image-sha" MOCK_REPO="$repo" \
    MOCK_EVENTS="$case_dir/events" MOCK_SERVER_ID="$SERVER_ID" MOCK_SIDECAR_ID="$SIDECAR_ID" \
    MOCK_RUNNER_MODE="${MOCK_RUNNER_MODE:-success}" MOCK_CREATE_TOKEN="${MOCK_CREATE_TOKEN:-}" \
    MOCK_ROTATE_TOKEN="${MOCK_ROTATE_TOKEN:-}" MOCK_SS_OUTPUT="${MOCK_SS_OUTPUT:-LISTEN 0 4096 100.124.236.66:2447 0.0.0.0:*}" \
    OPS_SHA="$OPS_SHA" bash "$test_op" "$mode" > "$case_dir/stdout" 2> "$case_dir/stderr"
  local status=$?
  set -e
  printf '%s' "$status"
}

make_fixture apply-success
MOCK_RUNNER_MODE=success
status=$(run_op apply-success apply)
repo=$(cat "$TMP/apply-success/repo")
if [[ "$status" == 0 ]] && grep -q 'configuration=applied source_sha=' "$TMP/apply-success/stdout" && \
  grep -Fxq 'TG_BOT_API_LISTEN_ADDR=127.0.0.1:2447' "$repo/.env" && \
  grep -Fxq 'TEAGRAM_BOT_API_HOST_IP=100.124.236.66' "$repo/.env" && \
  grep -q '^runner apply ' "$TMP/apply-success/events" && \
  cmp -s "$TMP/apply-success/before.env" "$repo/.state/.env.main-2825-before-bot-api"; then
  pass 'apply uses the named runner path and verifies the resulting configuration'
else
  cat "$TMP/apply-success/stderr" >&2
  fail 'apply success fixture'
fi

make_fixture apply-restore
MOCK_RUNNER_MODE=target-failed
status=$(run_op apply-restore apply)
repo=$(cat "$TMP/apply-restore/repo")
if [[ "$status" == 23 ]] && grep -q 'prior environment restored' "$TMP/apply-restore/stderr" && \
  ! grep -q 'TG_BOT_API_LISTEN_ADDR' "$repo/.env" && \
  [[ "$(cat "$repo/.state/rollout-env")" == "env_sha256=$(sha256sum "$repo/.env" | awk '{print $1}') source_sha=$OPS_SHA" ]]; then
  pass 'failed target rollout restores the verified prior environment'
else
  printf 'restore_status=%s\nrestore_env=%s\nrestore_record=%s\n' "$status" \
    "$(cat "$repo/.env")" "$(cat "$repo/.state/rollout-env")" >&2
  cat "$TMP/apply-restore/stderr" >&2
  fail 'apply restore fixture'
fi

make_fixture provision-success true
MOCK_CREATE_TOKEN="42:$(python3 -c 'import base64; print(base64.urlsafe_b64encode(bytes(range(32))).decode().rstrip("="))')"
MOCK_ROTATE_TOKEN="42:$(python3 -c 'import base64; print(base64.urlsafe_b64encode(bytes(range(1, 33))).decode().rstrip("="))')"
status=$(run_op provision-success provision)
repo=$(cat "$TMP/provision-success/repo")
if [[ "$status" == 0 ]] && grep -Fxq 'bot_id=42' "$TMP/provision-success/stdout" && \
  grep -Fxq "revoked_token=$MOCK_CREATE_TOKEN" "$TMP/provision-success/stdout" && \
  grep -Fxq "active_token=$MOCK_ROTATE_TOKEN" "$TMP/provision-success/stdout" && \
  grep -Fxq 42 "$repo/.state/bot-api-qa-id" && \
  [[ "$(grep -c 'bot create teagram_test_bot' "$TMP/provision-success/events")" == 1 ]] && \
  [[ "$(grep -c 'bot rotate 42' "$TMP/provision-success/events")" == 1 ]] && \
  [[ "$(grep -n 'bot create teagram_test_bot' "$TMP/provision-success/events" | cut -d: -f1)" -lt "$(grep -n 'bot rotate 42' "$TMP/provision-success/events" | cut -d: -f1)" ]]; then
  pass 'provision creates once, activates by rotation, and returns only validated tokens'
else
  cat "$TMP/provision-success/stderr" >&2
  fail 'provision sequence fixture'
fi

make_fixture provision-invalid-create true
MOCK_CREATE_TOKEN='42:invalid'
MOCK_ROTATE_TOKEN=''
status=$(run_op provision-invalid-create provision)
repo=$(cat "$TMP/provision-invalid-create/repo")
if [[ "$status" != 0 ]] && grep -q 'bot creation returned an invalid token' "$TMP/provision-invalid-create/stderr" && \
  [[ ! -e "$repo/.state/bot-api-qa-id" ]] && [[ ! -s "$TMP/provision-invalid-create/stdout" ]]; then
  pass 'provision rejects an invalid create token without persisting identity'
else
  cat "$TMP/provision-invalid-create/stderr" >&2
  fail 'invalid bot creation token fixture'
fi

make_fixture provision-invalid-activation true
MOCK_CREATE_TOKEN="42:$(python3 -c 'import base64; print(base64.urlsafe_b64encode(bytes(range(32))).decode().rstrip("="))')"
MOCK_ROTATE_TOKEN='42:invalid'
status=$(run_op provision-invalid-activation provision)
repo=$(cat "$TMP/provision-invalid-activation/repo")
if [[ "$status" != 0 ]] && grep -q 'bot activation returned an invalid token' "$TMP/provision-invalid-activation/stderr" && \
  grep -Fxq 42 "$repo/.state/bot-api-qa-id" && [[ ! -s "$TMP/provision-invalid-activation/stdout" ]]; then
  pass 'provision rejects an invalid activation token after persisting only the bot ID'
else
  cat "$TMP/provision-invalid-activation/stderr" >&2
  fail 'invalid bot activation token fixture'
fi

make_fixture verify-host-listeners true
MOCK_SS_OUTPUT=$'LISTEN 0 4096 127.0.0.1:2447 0.0.0.0:*\nLISTEN 0 4096 100.124.236.66:2447 0.0.0.0:*'
status=$(run_op verify-host-listeners verify)
if [[ "$status" == 0 ]] && grep -Fq 'listeners=verified' "$TMP/verify-host-listeners/stdout" && \
  grep -Fxq 'public_probe_ip=8.8.8.8' "$TMP/verify-host-listeners/stdout"; then
  pass 'verify checks host listeners and the tailnet-only binding'
else
  cat "$TMP/verify-host-listeners/stderr" >&2
  fail 'host listener verification fixture'
fi

make_fixture verify-lan-listener true
MOCK_SS_OUTPUT=$'LISTEN 0 4096 100.124.236.66:2447 0.0.0.0:*\nLISTEN 0 4096 192.168.1.25:2447 0.0.0.0:*'
status=$(run_op verify-lan-listener verify)
if [[ "$status" != 0 ]] && grep -q 'Bot API port has a public or LAN host listener' "$TMP/verify-lan-listener/stderr" && \
  ! grep -q 'listeners=verified' "$TMP/verify-lan-listener/stdout"; then
  pass 'verify rejects a LAN-bound Bot API listener on the host'
else
  cat "$TMP/verify-lan-listener/stderr" >&2
  fail 'LAN listener refusal fixture'
fi
