#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT="$SCRIPT_DIR/ops/rollback-public-websocket.sh"
TMP_ROOT=${TMPDIR:-/tmp}
TMP=$(mktemp -d "$TMP_ROOT/rollback-public-websocket.XXXXXXXX")
chmod 700 "$TMP"
trap 'rm -rf -- "$TMP"' EXIT

BIN="$TMP/bin"
REPO="$TMP/repo"
mkdir -m 700 "$BIN" "$REPO"

DEPLOYED_SHA=0123456789abcdef0123456789abcdef01234567
BASE_ID=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
HAPROXY_ID=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
PUBLIC_ONE_ID=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
PUBLIC_TWO_ID=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
export DEPLOYED_SHA BASE_ID HAPROXY_ID PUBLIC_ONE_ID PUBLIC_TWO_ID
export MOCK_REPO="$REPO" MOCK_REMOVED="$TMP/removed" MOCK_DOCKER_CALLS="$TMP/docker-calls"
: > "$MOCK_REMOVED"
: > "$MOCK_DOCKER_CALLS"

# The test copy points at a private fixture checkout and lock file. It also
# bypasses only the production root guard; all Docker effects are mocked below.
sed \
  -e "s|^REPO=/opt/teagram$|REPO=$REPO|" \
  -e "s|^LOCK_PATH=/tmp/teagram-deploy.lock$|LOCK_PATH=$TMP/deploy.lock|" \
  -e "s|if \[\[ \"\$(id -u)\" != 0 \]\]; then|if [[ 0 != 0 ]]; then|" \
  "$SCRIPT" > "$TMP/rollback-public-websocket.sh"
chmod 700 "$TMP/rollback-public-websocket.sh"

cat > "$BIN/git" <<'GIT'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == -C && "$2" == "$MOCK_REPO" && "$3" == rev-parse && "$4" == HEAD ]] || {
  printf 'unexpected git call: %s\n' "$*" >&2
  exit 90
}
printf '%s\n' "$DEPLOYED_SHA"
GIT

cat > "$BIN/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$MOCK_DOCKER_CALLS"

if [[ "${1:-}" == image && "${2:-}" == inspect ]]; then
  [[ "${4:-}" == '{{index .Config.Labels "org.opencontainers.image.revision"}}' && \
    "${5:-}" == sha256:fixture ]] || {
    printf 'unexpected docker image inspect call: %s\n' "$*" >&2
    exit 91
  }
  printf '%s\n' "$DEPLOYED_SHA"
  exit 0
fi

if [[ "${1:-}" == inspect ]]; then
  format=${3:-}
  target=${4:-}
  case "$format" in
    '{{.Id}}') [[ "$target" == teagram-teagramd-1 ]] && printf '%s\n' "$BASE_ID" ;;
    '{{.State.Status}}') [[ "$target" == "$BASE_ID" ]] && printf '%s\n' running ;;
    '{{.Image}}') [[ "$target" == "$BASE_ID" ]] && printf '%s\n' sha256:fixture ;;
    '{{index .Config.Labels "com.docker.compose.project"}}')
      [[ "$target" == "$BASE_ID" ]] && printf '%s\n' teagram
      ;;
    *) printf 'unexpected docker inspect format: %s\n' "$format" >&2; exit 91 ;;
  esac
  exit 0
fi

if [[ "${1:-}" == ps ]]; then
  service=
  publish=
  for arg in "$@"; do
    case "$arg" in
      label=com.docker.compose.service=*) service=${arg##*=} ;;
      publish=*) publish=${arg#*=} ;;
    esac
  done

  if [[ "$publish" == 2446 ]]; then
    exit 0
  fi

  case "$service" in
    public-haproxy) container_id=$HAPROXY_ID ;;
    teagramd-public-1) container_id=$PUBLIC_ONE_ID ;;
    teagramd-public-2) container_id=$PUBLIC_TWO_ID ;;
    *) printf 'unexpected docker ps service: %s\n' "$service" >&2; exit 92 ;;
  esac
  if grep -Fxq "$container_id" "$MOCK_REMOVED"; then
    exit 0
  fi
  if [[ " $* " == *" --no-trunc "* ]]; then
    printf '%s\n' "$container_id"
  else
    printf '%s\n' "${container_id:0:12}"
  fi
  exit 0
fi

if [[ "${1:-}" == rm && "${2:-}" == --force ]]; then
  case "${3:-}" in
    "$HAPROXY_ID"|"$PUBLIC_ONE_ID"|"$PUBLIC_TWO_ID")
      printf '%s\n' "$3" >> "$MOCK_REMOVED"
      ;;
    *) printf 'unexpected docker rm target: %s\n' "${3:-}" >&2; exit 93 ;;
  esac
  exit 0
fi

printf 'unexpected docker call: %s\n' "$*" >&2
exit 94
DOCKER

chmod 700 "$BIN/git" "$BIN/docker"
export PATH="$BIN:$PATH"

output="$TMP/stdout"
error="$TMP/stderr"
status=0
OPS_SHA="$DEPLOYED_SHA" bash "$TMP/rollback-public-websocket.sh" "$DEPLOYED_SHA" >"$output" 2>"$error" || status=$?
if [[ "$status" != 0 ]]; then
  printf 'rollback fixture failed with status %s\n' "$status" >&2
  cat "$error" >&2
  exit 1
fi

grep -Fq 'removed_containers=3' "$output" || {
  printf '%s\n' 'rollback fixture did not remove all three public containers' >&2
  cat "$output" >&2
  exit 1
}
for container_id in "$HAPROXY_ID" "$PUBLIC_ONE_ID" "$PUBLIC_TWO_ID"; do
  grep -Fxq "$container_id" "$MOCK_REMOVED" || {
    printf 'rollback fixture did not remove full container ID %s\n' "$container_id" >&2
    cat "$MOCK_REMOVED" >&2
    exit 1
  }
done

service_queries=$(grep -Fc 'label=com.docker.compose.service=' "$MOCK_DOCKER_CALLS")
[[ "$service_queries" == 6 ]] || {
  printf 'expected six service container listings, got %s\n' "$service_queries" >&2
  cat "$MOCK_DOCKER_CALLS" >&2
  exit 1
}
if grep -F 'label=com.docker.compose.service=' "$MOCK_DOCKER_CALLS" | grep -vF -- '--no-trunc'; then
  printf '%s\n' 'a public service listing omitted --no-trunc' >&2
  exit 1
fi

printf '%s\n' 'public WebSocket rollback fixture passes'
