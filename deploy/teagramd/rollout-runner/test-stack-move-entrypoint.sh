#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TMP=$(mktemp -d "${TMPDIR:-/root}/stack-move-entrypoint.XXXXXXXX")
chmod 700 "$TMP"
trap 'rm -rf -- "$TMP"' EXIT

FAILURES=()
record_failure() {
  FAILURES+=("$1")
  printf 'FAIL %s\n' "$1" >&2
}

if ! (unset ROLLOUT_RUNNER_CHECKOUT ROLLOUT_RUNNER_TARGET_CHECKOUT; \
  ROLLOUT_RUNNER_SOURCE_ONLY=1 bash -c 'source "$1"; [ "$CHECKOUT" != "$TARGET_CHECKOUT" ]' _ "$SCRIPT_DIR/rollout-runner.sh"); then
  record_failure 'production defaults must keep the source and target checkout paths distinct'
fi

CHECKOUT="$TMP/teagram"
TARGET_CHECKOUT="$TMP/teagram-target"
TARGET_DIR="$TMP/evidence"
ORIGIN="$TMP/origin.git"
BIN="$TMP/bin"
COMMANDS="$TMP/docker-commands"
CONTAINER_ID=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
mkdir -m 700 "$CHECKOUT" "$TARGET_DIR" "$BIN"

git init -q --bare "$ORIGIN"
git -C "$CHECKOUT" init -q -b main
git -C "$CHECKOUT" config user.name Fixture
git -C "$CHECKOUT" config user.email fixture@example.invalid
git -C "$CHECKOUT" remote add origin "$ORIGIN"

cat > "$CHECKOUT/docker-compose.yml" <<'YAML'
name: telegram-server
services:
  postgres:
    image: postgres:16-alpine
    volumes:
      - pgdata:/var/lib/postgresql/data
  telegramd:
    image: telegramd:local
    network_mode: bridge
    environment:
      TG_BLOB_DIR: /var/lib/telegramd-blobs
      TG_BLOB_S3_ENDPOINT: http://rustfs:9000
      TG_BLOB_S3_BUCKET: telegram
      TG_BLOB_S3_PREFIX: telegramd/
      TG_BLOB_S3_REGION: us-east-1
      TG_BLOB_S3_ACCESS_KEY_ID: fixture-access
      TG_BLOB_S3_SECRET_ACCESS_KEY: fixture-secret
    volumes:
      - tgkey:/var/lib/telegramd
      - tgblobs:/var/lib/telegramd-blobs:ro
volumes:
  pgdata:
    name: telegram-server_pgdata
  tgkey:
    name: telegram-server_tgkey
  tgblobs:
    name: telegram-server_tgblobs
  rustfsdata:
    name: telegram-server_rustfsdata
YAML
cat > "$CHECKOUT/docker-compose.override.yml" <<'YAML'
services:
  telegramd:
    ports: []
YAML
printf '%s\n' 'POSTGRES_PASSWORD=fixture-only' > "$CHECKOUT/.env"
chmod 600 "$CHECKOUT/.env"
git -C "$CHECKOUT" add docker-compose.yml docker-compose.override.yml .env
git -C "$CHECKOUT" commit -q -m 'legacy stack baseline'
BASELINE_SHA=$(git -C "$CHECKOUT" rev-parse HEAD)
git -C "$CHECKOUT" push -q origin main
printf '%s\n' target > "$CHECKOUT/target-marker"
git -C "$CHECKOUT" add target-marker
git -C "$CHECKOUT" commit -q -m 'reviewed target'
TARGET_SHA=$(git -C "$CHECKOUT" rev-parse HEAD)
git -C "$CHECKOUT" push -q origin main
git -C "$CHECKOUT" reset -q --hard "$BASELINE_SHA"
git -C "$CHECKOUT" fetch -q origin

cat > "$TMP/source-compose.json" <<'JSON'
{
  "name": "telegram-server",
  "services": {
    "postgres": {
      "image": "postgres:16-alpine",
      "volumes": [{"type": "volume", "source": "pgdata", "target": "/var/lib/postgresql/data"}]
    },
    "telegramd": {
      "image": "telegramd:local",
      "network_mode": "bridge",
      "environment": {
        "TG_BLOB_DIR": "/var/lib/telegramd-blobs",
        "TG_BLOB_S3_ENDPOINT": "http://rustfs:9000",
        "TG_BLOB_S3_BUCKET": "telegram",
        "TG_BLOB_S3_PREFIX": "telegramd/",
        "TG_BLOB_S3_REGION": "us-east-1",
        "TG_BLOB_S3_ACCESS_KEY_ID": "fixture-access",
        "TG_BLOB_S3_SECRET_ACCESS_KEY": "fixture-secret"
      },
      "volumes": [
        {"type": "volume", "source": "tgkey", "target": "/var/lib/telegramd"},
        {"type": "volume", "source": "tgblobs", "target": "/var/lib/telegramd-blobs", "read_only": true}
      ]
    }
  },
  "volumes": {
    "pgdata": {"name": "telegram-server_pgdata"},
    "tgkey": {"name": "telegram-server_tgkey"},
    "tgblobs": {"name": "telegram-server_tgblobs"},
    "rustfsdata": {"name": "telegram-server_rustfsdata"}
  }
}
JSON
cat > "$TMP/source-container.json" <<JSON
[
  {
    "Id": "$CONTAINER_ID",
    "Config": {
      "Labels": {
        "com.docker.compose.project": "telegram-server",
        "com.docker.compose.service": "telegramd"
      },
      "Env": [
        "TG_BLOB_DIR=/var/lib/telegramd-blobs",
        "TG_BLOB_S3_ENDPOINT=http://rustfs:9000",
        "TG_BLOB_S3_BUCKET=telegram",
        "TG_BLOB_S3_PREFIX=telegramd/",
        "TG_BLOB_S3_REGION=us-east-1",
        "TG_BLOB_S3_ACCESS_KEY_ID=fixture-access",
        "TG_BLOB_S3_SECRET_ACCESS_KEY=fixture-secret"
      ]
    },
    "State": {"Status": "running"},
    "HostConfig": {"PortBindings": {}, "NetworkMode": "bridge"},
    "NetworkSettings": {"Networks": {"bridge": {}}},
    "Mounts": [
      {"Source": "telegram-server_tgkey", "Destination": "/var/lib/telegramd", "Type": "volume", "Name": "telegram-server_tgkey", "RW": true},
      {"Source": "telegram-server_tgblobs", "Destination": "/var/lib/telegramd-blobs", "Type": "volume", "Name": "telegram-server_tgblobs", "RW": true}
    ]
  }
]
JSON
cat > "$BIN/docker" <<'SH'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$MOCK_DOCKER_COMMANDS"
if [ "${1:-}" = compose ]; then
  shift
  case "$*" in
    'config --format json') cat "$MOCK_SOURCE_COMPOSE_JSON" ;;
    'ps -q telegramd') printf '%s\n' "$MOCK_CONTAINER_ID" ;;
    'ps -q teagramd') exit 91 ;;
    *) exit 92 ;;
  esac
elif [ "${1:-}" = ps ] && [ "${2:-}" = --no-trunc ] && [ "${3:-}" = -q ]; then
  printf '%s\n' "$MOCK_CONTAINER_ID"
elif [ "${1:-}" = inspect ]; then
  cat "$MOCK_SOURCE_CONTAINER_JSON"
else
  exit 93
fi
SH
chmod 700 "$BIN/docker"

export ROLLOUT_RUNNER_SOURCE_ONLY=1
export ROLLOUT_RUNNER_TEST_MODE=1
export ROLLOUT_RUNNER_CHECKOUT="$CHECKOUT"
export ROLLOUT_RUNNER_TARGET_CHECKOUT="$TARGET_CHECKOUT"
export ROLLOUT_RUNNER_EVIDENCE_ROOT=/root
export ROLLOUT_RUNNER_SOURCE_LOCK_PATH="$TMP/source.lock"
export ROLLOUT_RUNNER_TARGET_LOCK_PATH="$TMP/target.lock"
export ROLLOUT_PINNED_EXECUTION=1
export ROLLOUT_RUNNER_RUN_ID=main1975-stack-move-fixture
export ROLLOUT_RUNNER_TARGET_DIR="$TARGET_DIR"
export COMPOSE_FILE=docker-compose.yml
export MOCK_DOCKER_COMMANDS="$COMMANDS"
export MOCK_SOURCE_COMPOSE_JSON="$TMP/source-compose.json"
export MOCK_SOURCE_CONTAINER_JSON="$TMP/source-container.json"
MOCK_CONTAINER_ID=$CONTAINER_ID
export MOCK_CONTAINER_ID
PATH="$BIN:$PATH"
export PATH

source "$SCRIPT_DIR/rollout-runner.sh"
MODE_HELPER="$SCRIPT_DIR/blob-mode-state.py"
STACK_MOVE_HELPER="$SCRIPT_DIR/stack-move-runner.py"
require_runtime() { return 0; }
check_private_dir() {
  [ -d "$1" ] && [ ! -L "$1" ] && [ "$(stat -c %a -- "$1")" = 700 ]
}
write_immutable() {
  [ ! -e "$1" ] && [ ! -L "$1" ] || return 1
  printf '%s\n' "$2" > "$1"
  chmod 600 -- "$1"
}
verify_approved_gates() { return 0; }
verify_runtime_sources() { return 0; }
require_clean_build_checkout() { return 0; }
validate_blob_authority() { printf '%s\n' 'blob_mode=valid outcome=s3-accepted generation=1 transition_id=fixture'; }
git() {
  if [ "${1:-}" = -C ] && [ "${3:-}" = worktree ] && [ "${4:-}" = add ]; then
    printf '%s\n' "$*" > "$TMP/worktree-add-command"
    return 88
  fi
  command git "$@"
}
exec 8>"$SOURCE_LOCK_PATH"
exec 9>"$TARGET_LOCK_PATH"

SEPARATE_TARGET_CHECKOUT=$TARGET_CHECKOUT
TARGET_CHECKOUT=$CHECKOUT
if main move-stack "$TARGET_SHA" "$BASELINE_SHA" >"$TMP/same-path.stdout" 2>"$TMP/same-path.stderr"; then
  SAME_PATH_STATUS=0
else
  SAME_PATH_STATUS=$?
fi
TARGET_CHECKOUT=$SEPARATE_TARGET_CHECKOUT
if [ "$SAME_PATH_STATUS" -ne 1 ] || ! rg -Fq 'target checkout path must differ from the source checkout' "$TMP/same-path.stderr"; then
  record_failure 'move-stack entrypoint must reject a target checkout that aliases the source'
  cat "$TMP/same-path.stderr" >&2
fi

if main move-stack "$TARGET_SHA" "$BASELINE_SHA" >"$TMP/stdout" 2>"$TMP/stderr"; then
  ENTRYPOINT_STATUS=0
else
  ENTRYPOINT_STATUS=$?
fi
if [ "$ENTRYPOINT_STATUS" -ne 1 ] || ! rg -Fq 'cannot create target checkout worktree' "$TMP/stderr"; then
  record_failure 'move-stack entrypoint did not pass source preflight to target worktree creation'
  cat "$TMP/stderr" >&2
fi
if ! rg -Fxq 'compose ps -q telegramd' "$COMMANDS" || rg -Fxq 'compose ps -q teagramd' "$COMMANDS"; then
  record_failure 'move-stack source inventory must query the legacy telegramd service'
fi
if ! jq -e '.services | any(.name == "telegramd")' "$TARGET_DIR/stack-move-preflight-blob-compose.json" >/dev/null 2>&1; then
  record_failure 'move-stack fixture did not capture the legacy Compose service'
fi
if ! jq -e '.containers | any(.service == "telegramd")' "$TARGET_DIR/stack-move-preflight-blob-containers.json" >/dev/null 2>&1; then
  record_failure 'move-stack fixture did not capture the legacy running service'
fi
if [ "${#FAILURES[@]}" -gt 0 ]; then
  exit 1
fi
printf '%s\n' 'PASS production move-stack entrypoint preflights the legacy telegramd source into a separate target checkout'
