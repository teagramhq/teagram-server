#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TMP=$(mktemp -d "${TMPDIR:-/tmp}/stack-move-entrypoint.XXXXXXXX")
chmod 700 "$TMP"
trap 'rm -rf -- "$TMP"' EXIT

FAILURES=()
record_failure() {
  FAILURES+=("$1")
  printf 'FAIL %s\n' "$1" >&2
}

has_line() {
  local expected=$1 file=$2 line
  while IFS= read -r line || [ -n "$line" ]; do
    [ "$line" = "$expected" ] && return 0
  done <"$file"
  return 1
}

has_text() {
  local expected=$1 file=$2 line
  while IFS= read -r line || [ -n "$line" ]; do
    [[ "$line" == *"$expected"* ]] && return 0
  done <"$file"
  return 1
}

if ! (unset ROLLOUT_RUNNER_CHECKOUT ROLLOUT_RUNNER_TARGET_CHECKOUT \
    ROLLOUT_RUNNER_ENV_FILE ROLLOUT_RUNNER_OVERRIDE_FILE ROLLOUT_RUNNER_SOURCE_LOCK_PATH \
    ROLLOUT_RUNNER_TEST_MODE; \
  ROLLOUT_RUNNER_SOURCE_ONLY=1 bash -c '
    source "$1"
    configure_action_paths move-stack
    [ "$CHECKOUT" = "/opt/$PROJECT_MOVE_SOURCE" ] &&
      [ "$TARGET_CHECKOUT" = /opt/teagram ] &&
      [ "$ENV_FILE" = "$CHECKOUT/.env" ] &&
      [ "$OVERRIDE_FILE" = "$CHECKOUT/docker-compose.override.yml" ] &&
      [ "$SOURCE_LOCK_PATH" = /tmp/telegram-server-deploy.lock ]
  ' _ "$SCRIPT_DIR/rollout-runner.sh"); then
  record_failure 'move-stack production defaults must select the legacy source, Teagram target, and baseline deploy lock'
fi

CHECKOUT="$TMP/teagram"
TARGET_CHECKOUT="$TMP/teagram-target"
TARGET_DIR="$TMP/evidence"
RUNTIME_DIR="$TMP/runtime"
ORIGIN="$TMP/origin.git"
BIN="$TMP/bin"
COMMANDS="$TMP/docker-commands"
CONTAINER_ID=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
mkdir -m 700 "$CHECKOUT" "$TARGET_DIR" "$RUNTIME_DIR" "$BIN"

for runtime_file in rollout-runner.sh rollout-verifier.sh schema-result-gate.sh schema-result-gate.py blob-mode-state.py qualify-rustfs-transition.py stack-move-runner.py; do
  cp "$SCRIPT_DIR/$runtime_file" "$RUNTIME_DIR/$runtime_file"
done
cp "$RUNTIME_DIR/rollout-verifier.sh" "$RUNTIME_DIR/rollout-verifier.pinned"
cp "$RUNTIME_DIR/schema-result-gate.sh" "$RUNTIME_DIR/schema-result-gate.pinned"
cp "$RUNTIME_DIR/blob-mode-state.py" "$RUNTIME_DIR/blob-mode-state.pinned"
cp "$RUNTIME_DIR/stack-move-runner.py" "$RUNTIME_DIR/stack-move-runner.pinned"
chmod 600 "$RUNTIME_DIR"/*

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
    volumes:
      - tgkey:/var/lib/telegramd
      - tgblobs:/var/lib/telegramd-blobs
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
cp "$SCRIPT_DIR/../../../docker-compose.local-blobs.yml" "$CHECKOUT/docker-compose.local-blobs.yml"
mkdir -p "$CHECKOUT/deploy/postgres"
cp "$SCRIPT_DIR/../../../deploy/postgres/postgresql.conf" \
  "$CHECKOUT/deploy/postgres/postgresql.conf"
printf '%s\n' '.env' 'docker-compose.override.yml' > "$CHECKOUT/.gitignore"
git -C "$CHECKOUT" add docker-compose.yml docker-compose.local-blobs.yml \
  deploy/postgres/postgresql.conf .gitignore
git -C "$CHECKOUT" commit -q -m 'legacy stack baseline'
BASELINE_SHA=$(git -C "$CHECKOUT" rev-parse HEAD)
git -C "$CHECKOUT" push -q origin main
printf '%s\n' target > "$CHECKOUT/target-marker"
git -C "$CHECKOUT" add target-marker
git -C "$CHECKOUT" commit -q -m 'reviewed target'
TARGET_SHA=$(git -C "$CHECKOUT" rev-parse HEAD)
if [ "$(git -C "$CHECKOUT" ls-tree -r "$TARGET_SHA" -- deploy/postgres/postgresql.conf | awk '{print $1}')" != 100644 ]; then
  record_failure 'target checkout fixture must track postgresql.conf with mode 100644'
fi
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
        "TG_BLOB_DIR": "/var/lib/telegramd-blobs"
      },
      "volumes": [
        {"type": "volume", "source": "tgkey", "target": "/var/lib/telegramd"},
        {"type": "volume", "source": "tgblobs", "target": "/var/lib/telegramd-blobs", "read_only": false}
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
        "TG_BLOB_DIR=/var/lib/telegramd-blobs"
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
export ROLLOUT_RUNNER_BASELINE_DIR="$RUNTIME_DIR"
export ROLLOUT_RUNNER_TEST_SOURCE_DIR="$RUNTIME_DIR"
export ROLLOUT_RUNNER_TEST_CHECKOUT="$CHECKOUT"
export ROLLOUT_RUNNER_SOURCE_LOCK_PATH="$TMP/source.lock"
export ROLLOUT_RUNNER_TARGET_LOCK_PATH="$TMP/target.lock"
export ROLLOUT_PINNED_EXECUTION=1
export ROLLOUT_RUNNER_RUN_ID=main1975-stack-move-fixture
export ROLLOUT_RUNNER_TARGET_DIR="$TARGET_DIR"
export COMPOSE_FILE="$CHECKOUT/docker-compose.yml:$CHECKOUT/docker-compose.override.yml"
export MOCK_DOCKER_COMMANDS="$COMMANDS"
export MOCK_SOURCE_COMPOSE_JSON="$TMP/source-compose.json"
export MOCK_SOURCE_CONTAINER_JSON="$TMP/source-container.json"
MOCK_CONTAINER_ID=$CONTAINER_ID
export MOCK_CONTAINER_ID
PATH="$BIN:$PATH"
export PATH

source "$RUNTIME_DIR/rollout-runner.sh"
cd "$CHECKOUT"
verify_approved_gates() { return 0; }
verify_runtime_sources() { return 0; }
require_clean_build_checkout() { return 0; }
MOCK_BLOB_VALIDATION='blob_mode=valid outcome=s3-accepted generation=1 transition_id=fixture'
validate_blob_authority() { printf '%s\n' "$MOCK_BLOB_VALIDATION"; }
git() {
  if [ "${1:-}" = -C ] && [ "${3:-}" = worktree ] && [ "${4:-}" = add ]; then
    printf '%s\n' "$*" > "$TMP/worktree-add-command"
    if [ "${MOCK_CREATE_TARGET:-0}" = 1 ]; then
      : > "$TMP/target-worktree-created"
      command git "$@"
      return $?
    fi
    return 88
  fi
  command git "$@"
}
python3() {
  if [ "${1:-}" = "$STACK_MOVE_HELPER" ] && [ "${2:-}" = move ]; then
    printf '%s\n' "$@" > "$TMP/stack-move-helper-args"
    return 0
  fi
  command python3 "$@"
}
exec 8>"$SOURCE_LOCK_PATH"
exec 9>"$TARGET_LOCK_PATH"

SEPARATE_TARGET_CHECKOUT=$ROLLOUT_RUNNER_TARGET_CHECKOUT
ROLLOUT_RUNNER_TARGET_CHECKOUT=$CHECKOUT
if main move-stack "$TARGET_SHA" "$BASELINE_SHA" >"$TMP/same-path.stdout" 2>"$TMP/same-path.stderr"; then
  SAME_PATH_STATUS=0
else
  SAME_PATH_STATUS=$?
fi
ROLLOUT_RUNNER_TARGET_CHECKOUT=$SEPARATE_TARGET_CHECKOUT
if [ "$SAME_PATH_STATUS" -ne 1 ] || ! has_text 'target checkout path must differ from the source checkout' "$TMP/same-path.stderr"; then
  record_failure 'move-stack entrypoint must reject a target checkout that aliases the source'
  cat "$TMP/same-path.stderr" >&2
fi

rm -f "$TMP/worktree-add-command"
: > "$COMMANDS"
if main move-stack "$TARGET_SHA" "$BASELINE_SHA" >"$TMP/s3-stdout" 2>"$TMP/s3-stderr"; then
  S3_STATUS=0
else
  S3_STATUS=$?
fi
if [ "$S3_STATUS" -ne 1 ] || ! has_text 'MAIN-1977 bucket-move work' "$TMP/s3-stderr" || \
    ! has_text 'source stack was not stopped' "$TMP/s3-stderr" || \
    [ -e "$TMP/worktree-add-command" ] || has_text 'compose down' "$COMMANDS"; then
  record_failure 'move-stack must reject S3-authorized blob state before creating a target or stopping the source stack'
  cat "$TMP/s3-stderr" >&2
fi

MOCK_BLOB_VALIDATION='blob_mode=valid outcome=initial-local generation=1 transition_id=fixture'
rm -f -- "$TARGET_DIR/stack-move-preflight-blob-compose.json" \
  "$TARGET_DIR/stack-move-preflight-blob-containers.json"
: > "$COMMANDS"
if main move-stack "$TARGET_SHA" "$BASELINE_SHA" >"$TMP/stdout" 2>"$TMP/stderr"; then
  ENTRYPOINT_STATUS=0
else
  ENTRYPOINT_STATUS=$?
fi
if [ "$ENTRYPOINT_STATUS" -ne 1 ] || ! has_text 'cannot create target checkout worktree' "$TMP/stderr"; then
  record_failure 'move-stack entrypoint did not pass local source preflight to target worktree creation'
  cat "$TMP/stderr" >&2
fi
if ! has_line 'compose ps -q telegramd' "$COMMANDS" || has_line 'compose ps -q teagramd' "$COMMANDS"; then
  record_failure 'move-stack source inventory must query the legacy telegramd service'
fi
if ! jq -e '.services | any(.name == "telegramd")' "$TARGET_DIR/stack-move-preflight-blob-compose.json" >/dev/null 2>&1; then
  record_failure 'move-stack fixture did not capture the legacy Compose service'
fi
if ! jq -e '.containers | any(.service == "telegramd")' "$TARGET_DIR/stack-move-preflight-blob-containers.json" >/dev/null 2>&1; then
  record_failure 'move-stack fixture did not capture the legacy running service'
fi

rm -f -- "$TARGET_DIR/stack-move-preflight-blob-compose.json" \
  "$TARGET_DIR/stack-move-preflight-blob-containers.json"
: > "$COMMANDS"
rm -f "$TMP/target-worktree-created"
MOCK_CREATE_TARGET=1
if main move-stack "$TARGET_SHA" "$BASELINE_SHA" >"$TMP/target-compose.stdout" 2>"$TMP/target-compose.stderr"; then
  TARGET_COMPOSE_STATUS=0
else
  TARGET_COMPOSE_STATUS=$?
fi
MOCK_CREATE_TARGET=0
if [ "$TARGET_COMPOSE_STATUS" -ne 0 ] || \
    ! has_line 'docker-compose.yml:docker-compose.local-blobs.yml' "$TMP/stack-move-helper-args" || \
    ! has_line '--blob-mode-helper' "$TMP/stack-move-helper-args" || \
    ! has_line "$RUNTIME_DIR/blob-mode-state.pinned" "$TMP/stack-move-helper-args"; then
  record_failure 'move-stack did not pass the Compose overlay and verified pinned blob-mode helper to the mover'
  cat "$TMP/target-compose.stderr" >&2
fi
if [ -e "$TMP/target-worktree-created" ]; then
  if [ ! -f "$TARGET_CHECKOUT/deploy/postgres/postgresql.conf" ]; then
    record_failure 'target checkout fixture is missing tracked postgresql.conf'
  elif [ "$(stat -c '%a' -- "$TARGET_CHECKOUT/deploy/postgres/postgresql.conf")" != 644 ]; then
    record_failure 'target worktree must check out tracked postgresql.conf as mode 0644, not 0600'
  fi
  if [ ! -f "$TARGET_CHECKOUT/.env" ]; then
    record_failure 'copied target environment file is missing'
  elif [ "$(stat -c '%a' -- "$TARGET_CHECKOUT/.env")" != 600 ]; then
    record_failure 'copied target environment file must remain private at mode 0600'
  fi
fi
rm -rf -- "$TARGET_CHECKOUT"

cat > "$TMP/lock-probe.sh" <<'SH'
#!/usr/bin/env bash
set -Eeuo pipefail
exec 8>&- 9>&-
source "$PROBE_RUNNER"
verify_approved_gates() { return 0; }
verify_runtime_sources() { return 0; }
require_clean_build_checkout() { return 0; }
create_phase_dirs() {
  : > "$LOCK_PROBE_REACHED"
  return 1
}
cd "$CHECKOUT"
main move-stack "$1" "$2"
SH
chmod 700 "$TMP/lock-probe.sh"
export PROBE_RUNNER="$RUNTIME_DIR/rollout-runner.sh"
export LOCK_PROBE_REACHED="$TMP/lock-probe-reached"
LOCK_SOURCE="$TMP/telegram-server-deploy.lock"
mkfifo "$TMP/lock-ready" "$TMP/lock-release"
(
  exec 7>"$LOCK_SOURCE"
  flock -x 7
  printf 'ready\n' > "$TMP/lock-ready"
  IFS= read -r _ < "$TMP/lock-release"
) &
LOCK_HOLDER_PID=$!
if ! IFS= read -r -t 10 _ < "$TMP/lock-ready"; then
  record_failure 'could not acquire the baseline telegram-server deploy lock for the mutual-exclusion test'
  printf 'release\n' > "$TMP/lock-release"
  wait "$LOCK_HOLDER_PID" || true
else
  rm -f "$LOCK_PROBE_REACHED"
  if timeout 1s env ROLLOUT_PINNED_EXECUTION=0 ROLLOUT_RUNNER_SOURCE_LOCK_PATH="$LOCK_SOURCE" \
      bash "$TMP/lock-probe.sh" "$TARGET_SHA" "$BASELINE_SHA" >"$TMP/lock-held.stdout" 2>"$TMP/lock-held.stderr"; then
    LOCK_HELD_STATUS=0
  else
    LOCK_HELD_STATUS=$?
  fi
  if [ "$LOCK_HELD_STATUS" -ne 124 ] || [ -e "$LOCK_PROBE_REACHED" ]; then
    record_failure 'move-stack entrypoint did not block on the baseline telegram-server deploy lock'
    cat "$TMP/lock-held.stderr" >&2
  fi
  printf 'release\n' > "$TMP/lock-release"
  wait "$LOCK_HOLDER_PID"
  rm -f "$LOCK_PROBE_REACHED"
  if env ROLLOUT_PINNED_EXECUTION=0 ROLLOUT_RUNNER_SOURCE_LOCK_PATH="$LOCK_SOURCE" \
      bash "$TMP/lock-probe.sh" "$TARGET_SHA" "$BASELINE_SHA" >"$TMP/lock-free.stdout" 2>"$TMP/lock-free.stderr"; then
    LOCK_FREE_STATUS=0
  else
    LOCK_FREE_STATUS=$?
  fi
  if [ "$LOCK_FREE_STATUS" -ne 1 ] || [ ! -e "$LOCK_PROBE_REACHED" ]; then
    record_failure 'move-stack entrypoint did not proceed after the baseline telegram-server deploy lock was released'
    cat "$TMP/lock-free.stderr" >&2
  fi
fi
if [ "${#FAILURES[@]}" -gt 0 ]; then
  exit 1
fi
printf '%s\n' 'PASS production move-stack entrypoint validates paths, serializes with the legacy deploy lock, and rejects S3 blob authority'
