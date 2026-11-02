#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
OP_SCRIPT=$SCRIPT_DIR/ops/start-server.sh
TMP=$(mktemp -d /tmp/teagram-start-server-test.XXXXXXXX)
cleanup() {
  status=$?
  trap - EXIT
  case "$TMP" in
    /tmp/teagram-start-server-test.????????)
      [[ -d "$TMP" && ! -L "$TMP" ]] && rm -rf -- "$TMP" || status=1
      ;;
    *) printf '%s\n' 'refusing to remove an unexpected fixture path' >&2; status=1 ;;
  esac
  exit "$status"
}
trap cleanup EXIT

mkdir -p "$TMP/bin"
FIXTURE_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export FIXTURE_SHA

cat > "$TMP/bin/id" <<'ID'
#!/usr/bin/env bash
[[ "$#" == 1 && "$1" == -u ]] || exit 99
printf '0\n'
ID

cat > "$TMP/bin/stat" <<'STAT'
#!/usr/bin/env bash
if [[ "$#" == 3 && "$1" == -c && "$2" == %a && "$3" == /tmp/teagram-start-server.* ]]; then
  printf '700\n'
  exit 0
fi
exec /usr/bin/stat "$@"
STAT

cat > "$TMP/bin/flock" <<'FLOCK'
#!/usr/bin/env bash
[[ "$#" == 2 && "$1" == -x && "$2" == 9 ]] || { echo "unexpected fixture flock: $*" >&2; exit 99; }
FLOCK

cat > "$TMP/bin/git" <<'GIT'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == -C && "$2" == "$FIXTURE_REPO" && "$3" == show ]] || {
  echo "unexpected fixture git call: $*" >&2
  exit 99
}
case "$4" in
  "$FIXTURE_SHA:deploy/teagramd/rollout-runner/blob-transition-runner.py"|\
  "$FIXTURE_SHA:deploy/teagramd/rollout-runner/blob-mode-state.py"|\
  "$FIXTURE_SHA:deploy/teagramd/rollout-runner/qualify-rustfs-transition.py")
    printf '# fixture helper\n'
    ;;
  *) echo "unexpected fixture git show: $4" >&2; exit 99 ;;
esac
GIT

cat > "$TMP/bin/python3" <<'PYTHON'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == */blob-transition-runner.py ]] || { echo 'unexpected fixture helper' >&2; exit 99; }
helper_dir=$(dirname "$1")
[[ -f "$helper_dir/blob-mode-state.py" && -f "$helper_dir/qualify-rustfs-transition.py" ]] || {
  echo 'reviewed blob state dependencies were not staged together' >&2
  exit 99
}
shift
[[ "$1" == deployment-env && "$2" == --checkout && "$3" == "$FIXTURE_REPO" &&
   "$4" == --state-dir && "$5" == "$FIXTURE_REPO/.state/blob-mode" &&
   "$6" == --report-root && "$7" == /root &&
   "$8" == --lock-path && "$9" == /tmp/teagram-deploy.lock ]] || {
  echo "unexpected deployment-env arguments: $*" >&2
  exit 99
}
printf '%s\n' "$FIXTURE_AUTHORITY_JSON"
PYTHON

cat > "$TMP/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == inspect ]]; then
  [[ "$2" == --format && "$#" == 4 ]] || { echo "unexpected inspect: $*" >&2; exit 99; }
  format=$3
  container=$4
  case "$container" in
    teagram-postgres-1) service=postgres; state_file=$MOCK_POSTGRES_STATE_FILE; project=${MOCK_POSTGRES_PROJECT:-teagram}; files=${MOCK_POSTGRES_FILES:-$MOCK_COMPOSE_FILES} ;;
    teagram-teagramd-1) service=teagramd; state_file=$MOCK_TEAGRAMD_STATE_FILE; project=${MOCK_TEAGRAMD_PROJECT:-teagram}; files=${MOCK_TEAGRAMD_FILES:-$MOCK_COMPOSE_FILES} ;;
    *) echo "unexpected fixture container: $container" >&2; exit 99 ;;
  esac
  case "$format" in
    '{{.State.Status}}|{{index .Config.Labels "com.docker.compose.project"}}|{{index .Config.Labels "com.docker.compose.service"}}|{{index .Config.Labels "com.docker.compose.project.config_files"}}')
      printf '%s|%s|%s|%s\n' "$(cat "$state_file")" "$project" "$service" "$files"
      ;;
    *) echo "unexpected fixture inspect format: $format" >&2; exit 99 ;;
  esac
elif [[ "$1" == compose ]]; then
  if [[ "$*" == *' start teagramd' ]]; then
    printf 'start:migrate\n' >> "$MOCK_EVENTS"
    printf 'running\n' > "$MOCK_MIGRATE_STATE_FILE"
    printf 'start:teagramd\n' >> "$MOCK_EVENTS"
    printf 'running\n' > "$MOCK_TEAGRAMD_STATE_FILE"
  else
    echo "unexpected fixture Compose command: $*" >&2
    exit 98
  fi
elif [[ "$1" == start ]]; then
  [[ "$#" == 2 ]] || { echo "unexpected fixture start args: $*" >&2; exit 99; }
  case "$2" in
    teagram-postgres-1)
      service=postgres
      state_file=$MOCK_POSTGRES_STATE_FILE
      failure_service=postgres
      failure_status=42
      ;;
    teagram-teagramd-1)
      service=teagramd
      state_file=$MOCK_TEAGRAMD_STATE_FILE
      failure_service=teagramd
      failure_status=43
      ;;
    *) echo "unexpected fixture container start: $2" >&2; exit 99 ;;
  esac
  printf 'start:%s\n' "$service" >> "$MOCK_EVENTS"
  [[ "${MOCK_START_FAIL_SERVICE:-}" != "$failure_service" ]] || exit "$failure_status"
  printf 'running\n' > "$state_file"
elif [[ "$1" == exec ]]; then
  [[ "$#" == 7 && "$2" == teagram-postgres-1 && "$3" == pg_isready &&
     "$4" == -U && "$5" == postgres && "$6" == -d && "$7" == teagram ]] || {
    echo "unexpected fixture exec: $*" >&2
    exit 99
  }
  printf 'readiness:postgres\n' >> "$MOCK_EVENTS"
  [[ "${MOCK_PG_READY:-1}" == 1 ]]
elif [[ "$1" == ps || "$1" == volume ]]; then
  echo "unexpected mutating or broad Docker command: $*" >&2
  exit 98
else
  echo "unexpected fixture Docker call: $*" >&2
  exit 99
fi
DOCKER

cat > "$TMP/bin/sleep" <<'SLEEP'
#!/usr/bin/env bash
[[ "$#" == 1 && ( "$1" == 5 || "$1" == 2 ) ]] || { echo "unexpected fixture sleep: $*" >&2; exit 99; }
printf 'sleep:%s\n' "$1" >> "$MOCK_EVENTS"
SLEEP

chmod 755 "$TMP/bin/"*

setup_case() {
  local name=$1 dir
  dir=$TMP/$name
  FIXTURE_REPO=$dir/repo
  MOCK_POSTGRES_STATE_FILE=$dir/postgres-state
  MOCK_TEAGRAMD_STATE_FILE=$dir/teagramd-state
  MOCK_MIGRATE_STATE_FILE=$dir/migrate-state
  MOCK_EVENTS=$dir/events
  MOCK_COMPOSE_FILES=$FIXTURE_REPO/docker-compose.yml,$FIXTURE_REPO/docker-compose.override.yml
  unset MOCK_POSTGRES_PROJECT MOCK_TEAGRAMD_PROJECT MOCK_POSTGRES_FILES MOCK_TEAGRAMD_FILES
  unset MOCK_PG_READY MOCK_START_FAIL_SERVICE
  FIXTURE_AUTHORITY_JSON=$(jq -cn --arg repo "$FIXTURE_REPO" \
    '{outcome:"s3-accepted",env_files:[$repo + "/.env",$repo + "/.env.rustfs-parked"]}')
  mkdir -p "$FIXTURE_REPO/.state/blob-mode"
  printf 'fixture compose\n' > "$FIXTURE_REPO/docker-compose.yml"
  printf 'fixture override\n' > "$FIXTURE_REPO/docker-compose.override.yml"
  printf 'POSTGRES_PASSWORD=fixture-secret-value\n' > "$FIXTURE_REPO/.env"
  printf 'RUSTFS_SECRET_KEY=fixture-parked-secret\n' > "$FIXTURE_REPO/.env.rustfs-parked"
  printf 'exited\n' > "$MOCK_POSTGRES_STATE_FILE"
  printf 'exited\n' > "$MOCK_TEAGRAMD_STATE_FILE"
  printf 'exited\n' > "$MOCK_MIGRATE_STATE_FILE"
  : > "$MOCK_EVENTS"
  sed "s|/opt/teagram|$FIXTURE_REPO|g" "$OP_SCRIPT" > "$dir/op.sh"
  chmod 755 "$dir/op.sh"
  export FIXTURE_REPO FIXTURE_AUTHORITY_JSON MOCK_POSTGRES_STATE_FILE MOCK_TEAGRAMD_STATE_FILE \
    MOCK_MIGRATE_STATE_FILE MOCK_EVENTS MOCK_COMPOSE_FILES
}

run_operation() {
  local status=0 output
  output=$(env OPS_SHA="$FIXTURE_SHA" PATH="$TMP/bin:$PATH" bash "$TMP/$(basename "$(dirname "$FIXTURE_REPO")")/op.sh" "$@" 2>&1) || status=$?
  printf '%s\n' "$status" > "$TMP/operation-status"
  printf '%s\n' "$output"
}

assert_no_event() {
  local pattern=$1 status=0
  grep -q -- "$pattern" "$MOCK_EVENTS" || status=$?
  [[ "$status" == 1 ]] || {
    echo "unexpected fixture event or unreadable event log: $pattern" >&2
    return 1
  }
}

setup_case ordered-start
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 0 ]]
[[ "$output" == *'postgres=ready teagramd=running compose_selection=verified'* ]]
[[ "$output" != *'fixture-secret-value'* && "$output" != *'fixture-parked-secret'* ]]
[[ "$(cat "$MOCK_EVENTS")" == $'start:postgres\nsleep:5\nreadiness:postgres\nstart:teagramd' ]]
[[ "$(cat "$MOCK_MIGRATE_STATE_FILE")" == exited ]]
printf '%s\n' 'PASS starts Postgres before only the existing teagramd container, without starting migration dependencies'

setup_case already-running
printf 'running\n' > "$MOCK_POSTGRES_STATE_FILE"
printf 'running\n' > "$MOCK_TEAGRAMD_STATE_FILE"
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 0 ]]
[[ "$(cat "$MOCK_EVENTS")" == $'sleep:5\nreadiness:postgres' ]]
printf '%s\n' 'PASS repeated recovery is idempotent for running containers'

setup_case wrong-teagramd-project
MOCK_TEAGRAMD_PROJECT=other
export MOCK_TEAGRAMD_PROJECT
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'teagramd container is not in the teagram Compose project'* ]]
assert_no_event '^start:'
printf '%s\n' 'PASS rejects teagramd from a different Compose project before starting services'

setup_case paused-teagramd
printf 'paused\n' > "$MOCK_TEAGRAMD_STATE_FILE"
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'existing teagram-teagramd-1 container state is not restartable'* ]]
assert_no_event '^start:'
printf '%s\n' 'PASS rejects a paused teagramd container before starting services'

setup_case mismatched-teagramd
MOCK_TEAGRAMD_FILES="$FIXTURE_REPO/docker-compose.yml,$FIXTURE_REPO/docker-compose.local-blobs.yml"
export MOCK_TEAGRAMD_FILES
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'live Compose selection is not the reviewed teagram S3 stack'* ]]
assert_no_event '^start:'
printf '%s\n' 'PASS rejects teagramd from a different Compose selection before starting services'

setup_case unapproved-compose
MOCK_COMPOSE_FILES="$FIXTURE_REPO/docker-compose.yml,$FIXTURE_REPO/docker-compose.override.yml,$FIXTURE_REPO/docker-compose.local-blobs.yml"
export MOCK_COMPOSE_FILES
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'live Compose selection is not the reviewed teagram S3 stack'* ]]
assert_no_event '^start:'
printf '%s\n' 'PASS rejects a changed live Compose selection before starting services'

setup_case mismatched-postgres
MOCK_POSTGRES_FILES="$FIXTURE_REPO/docker-compose.yml,$FIXTURE_REPO/docker-compose.local-blobs.yml"
export MOCK_POSTGRES_FILES
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'server containers do not share the inspected Compose selection'* ]]
assert_no_event '^start:'
printf '%s\n' 'PASS rejects a Postgres container created from a different Compose selection'

setup_case invalid-environment
FIXTURE_AUTHORITY_JSON=$(jq -cn --arg repo "$FIXTURE_REPO" \
  '{outcome:"s3-accepted",env_files:[$repo + "/.env","/tmp/unexpected-env"]}')
export FIXTURE_AUTHORITY_JSON
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'live S3 environment selection includes an unapproved credential path'* ]]
assert_no_event '^start:'
printf '%s\n' 'PASS rejects an unapproved parked credential path before starting services'

setup_case rejected-authority
FIXTURE_AUTHORITY_JSON=$(jq -cn '{outcome:"transition-rejected",env_files:[]}')
export FIXTURE_AUTHORITY_JSON
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'live S3 environment selection is invalid'* ]]
assert_no_event '^start:'
printf '%s\n' 'PASS rejects a non-accepted S3 authority result before starting services'

setup_case postgres-not-ready
MOCK_PG_READY=0
export MOCK_PG_READY
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'Postgres did not become ready within 60 seconds'* ]]
assert_no_event '^start:teagramd$'
printf '%s\n' 'PASS leaves teagramd stopped when Postgres never becomes ready'

setup_case postgres-start-fails
MOCK_START_FAIL_SERVICE=postgres
export MOCK_START_FAIL_SERVICE
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'could not start the existing postgres container'* ]]
assert_no_event '^start:teagramd$'
printf '%s\n' 'PASS does not start teagramd when Postgres startup fails'

setup_case app-start-fails
MOCK_START_FAIL_SERVICE=teagramd
export MOCK_START_FAIL_SERVICE
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'could not start the existing teagramd container'* ]]
printf '%s\n' 'PASS reports a teagramd startup failure after Postgres is ready'

setup_case bad-arguments
output=$(run_operation unexpected)
[[ "$(cat "$TMP/operation-status")" == 64 ]]
[[ "$output" == *'usage: start-server.sh'* ]]
assert_no_event '^start:'
printf '%s\n' 'PASS rejects caller-supplied operation arguments'
