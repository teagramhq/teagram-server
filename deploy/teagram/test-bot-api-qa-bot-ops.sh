#!/usr/bin/env bash
set -Eeuo pipefail
ROOT=$(cd -P "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
tmp=$(mktemp -d)
cleanup() {
  : > "$tmp/release-encrypt-a"
  for process in "${rotate_a_pid:-}" "${rotate_b_pid:-}"; do
    [[ -n "$process" ]] || continue
    wait "$process" 2>/dev/null || true
  done
  rm -rf -- "$tmp"
}
trap cleanup EXIT
repo="$tmp/repo"
mkdir -p "$tmp/bin" "$repo/.state" "$tmp/ops"
export MOCK_TMP="$tmp" MOCK_REPO="$repo" MOCK_OPS_DIR="$tmp/ops"
export OPS_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export MOCK_BOT_ID=987654321
MOCK_CANARY_SECRET=$(printf 'B%.0s' {1..42})A
export MOCK_CANARY_TOKEN="${MOCK_BOT_ID}:${MOCK_CANARY_SECRET}"
MOCK_CANARY_SECRET_B=$(printf 'C%.0s' {1..42})Q
export MOCK_CANARY_TOKEN_B="${MOCK_BOT_ID}:${MOCK_CANARY_SECRET_B}"
MOCK_ROOT_UID=$(id -u)
export MOCK_ROOT_UID
MOCK_REAL_FLOCK=$(command -v flock)
export MOCK_REAL_FLOCK

go build -o "$tmp/teagramd" ./cmd/teagramd
key_material=$(go run ./cmd/teagramd/testdata/bot-api-qa-age-fixture.go keygen)
recipient=${key_material%%$'\n'*}
identity=${key_material#*$'\n'}
unset key_material

python3 - "$ROOT/deploy/teagram/ops/bot-api-qa-bot.sh" "$tmp/ops/bot-api-qa-bot.sh" "$repo" "$recipient" "$MOCK_ROOT_UID" "$tmp/bot-api-qa.lock" <<'PY'
from pathlib import Path
import sys

source = Path(sys.argv[1]).read_text()
replacements = {
    "readonly REPO=/opt/teagram": f"readonly REPO={sys.argv[3]}",
    "readonly LOCK_PATH=/tmp/teagram-deploy.lock": f"readonly LOCK_PATH={sys.argv[6]}",
    "readonly EXPECTED_ROOT_UID=0": f"readonly EXPECTED_ROOT_UID={sys.argv[5]}",
    "readonly AGE_RECIPIENT=age1p5s5le72wnxu465pe2xz5a3qv8mdykwp5leezpjk0zm6p0m3e5hs36fydn":
        f"readonly AGE_RECIPIENT={sys.argv[4]}",
    "[[ ${EUID:-$(id -u)} == 0 ]]": f"[[ ${'{'}EUID:-$(id -u){'}'} == {sys.argv[5]} ]]",
}
for old, new in replacements.items():
    if source.count(old) != 1:
        raise SystemExit(f"fixture expected one occurrence: {old}")
    source = source.replace(old, new)
Path(sys.argv[2]).write_text(source)
PY
chmod 700 "$tmp/ops/bot-api-qa-bot.sh"
sed "s|REPO=/opt/teagram|REPO=$repo|" "$ROOT/deploy/teagram/teagram-ops.sh" > "$tmp/dispatcher.sh"
chmod 700 "$tmp/dispatcher.sh"

cat > "$repo/.bot-status" <<'JSON'
{"state":"missing"}
JSON

cat > "$tmp/bin/git" <<'GIT'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "$1" == -C && "$2" == "$MOCK_REPO" ]] || { printf 'unexpected git args: %s\n' "$*" >&2; exit 99; }
shift 2
case "$1" in
  rev-parse) [[ "${2:-}" == HEAD ]] && printf '%s\n' "$OPS_SHA" ;;
  fetch) [[ "$*" == 'fetch -q origin main' ]] ;;
  merge-base) [[ "$*" == "merge-base --is-ancestor $OPS_SHA origin/main" ]] ;;
  cat-file) [[ "$*" == "cat-file -e $OPS_SHA:deploy/teagram/ops/bot-api-qa-bot.sh" ]] ;;
  show)
    [[ "${2:-}" == "$OPS_SHA:deploy/teagram/ops/bot-api-qa-bot.sh" ]] || exit 99
    cat "$MOCK_OPS_DIR/bot-api-qa-bot.sh"
    ;;
  *) printf 'unexpected git operation: %s\n' "$*" >&2; exit 99 ;;
esac
GIT

cat > "$tmp/bin/logger" <<'LOGGER'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >> "$MOCK_TMP/logger.log"
LOGGER

cat > "$tmp/bin/flock" <<'FLOCK'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ ${MOCK_RUN_LABEL:-} == B ]]; then
  : > "$MOCK_TMP/flock-b-entered"
fi
exec "$MOCK_REAL_FLOCK" "$@"
FLOCK

cat > "$tmp/bin/tailscale" <<'TAILSCALE'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "$*" == 'ip -4' ]] || exit 99
printf '%s\n' 100.64.1.1
TAILSCALE

cat > "$tmp/bin/curl" <<'CURL'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >> "$MOCK_TMP/curl.argv"
config=$(cat)
[[ "$config" == *'100.64.1.1:2447/bot987654321:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA/getMe'* ]] || exit 98
count=0
if [[ -f "$MOCK_TMP/curl.count" ]]; then
  count=$(cat "$MOCK_TMP/curl.count")
fi
count=$((count + 1))
printf '%s\n' "$count" > "$MOCK_TMP/curl.count"
if (( count <= 20 )); then printf '401'; else printf '429'; fi
CURL

cat > "$tmp/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >> "$MOCK_TMP/docker.argv"
record_event() {
  case "${MOCK_RUN_LABEL:-}" in
    A|B) printf '%s %s\n' "$1" "$MOCK_RUN_LABEL" >> "$MOCK_TMP/events" ;;
  esac
}
case "$*" in
  'inspect --format {{index .Config.Labels "org.opencontainers.image.revision"}} teagram-teagramd-1')
    printf '%s\n' "$OPS_SHA"
    ;;
  'exec teagram-teagramd-1 teagramd bot encrypt --check-recipient '*)
    "$MOCK_TMP/teagramd" bot encrypt --check-recipient "${@:7}"
    ;;
  'exec teagram-teagramd-1 teagramd bot lookup teagram_qa_test_bot')
    record_event lookup
    if [[ ${MOCK_RUN_LABEL:-} == B ]]; then
      : > "$MOCK_TMP/lookup-b-entered"
    fi
    cat "$MOCK_REPO/.bot-status"
    ;;
  'exec teagram-teagramd-1 teagramd bot create teagram_qa_test_bot Teagram QA Test')
    printf '{"state":"found","owner":"user","id":%s,"kind":"bot","first_name":"Teagram QA Test","token_active":true}\n' "$MOCK_BOT_ID" > "$MOCK_REPO/.bot-status"
    printf '%s\n' "$MOCK_CANARY_TOKEN"
    ;;
  'exec teagram-teagramd-1 teagramd bot rotate '*)
    id=${*:6:1}
    [[ "$id" == "$MOCK_BOT_ID" ]] || exit 98
    record_event rotate
    printf '{"state":"found","owner":"user","id":%s,"kind":"bot","first_name":"Teagram QA Test","token_active":true}\n' "$MOCK_BOT_ID" > "$MOCK_REPO/.bot-status"
    printf '%s\n' "${MOCK_ROTATE_TOKEN:-$MOCK_CANARY_TOKEN}"
    ;;
  'exec teagram-teagramd-1 teagramd bot revoke '*)
    id=${*:6:1}
    [[ "$id" == "$MOCK_BOT_ID" ]] || exit 98
    record_event revoke
    printf '{"state":"found","owner":"user","id":%s,"kind":"bot","first_name":"Teagram QA Test","token_active":false}\n' "$MOCK_BOT_ID" > "$MOCK_REPO/.bot-status"
    ;;
  'exec -i teagram-teagramd-1 teagramd bot encrypt '*)
    if [[ ${MOCK_FAIL_ENCRYPT:-} == 1 ]]; then
      cat >/dev/null
      : > "$MOCK_TMP/encrypt-a-entered"
      while [[ ! -e "$MOCK_TMP/release-encrypt-a" ]]; do sleep 0.01; done
      exit 77
    fi
    recipient=${*:7:1}
    "$MOCK_TMP/teagramd" bot encrypt "$recipient"
    ;;
  *) printf 'unexpected docker args: %s\n' "$*" >&2; exit 99 ;;
esac
DOCKER
chmod 755 "$tmp/bin/"*
export PATH="$tmp/bin:$PATH"

invoke() {
  local command=$1
  if SSH_ORIGINAL_COMMAND=$command MOCK_RUN_LABEL=serial bash "$tmp/dispatcher.sh" > "$tmp/stdout" 2> "$tmp/stderr"; then
    INVOKE_STATUS=0
  else
    INVOKE_STATUS=$?
  fi
}

expect_status() {
  local expected=$1 command=$2
  invoke "$command"
  if [[ "$INVOKE_STATUS" != "$expected" ]]; then
    printf 'command %q exited %s, want %s\n' "$command" "$INVOKE_STATUS" "$expected" >&2
    cat "$tmp/stderr" >&2
    return 1
  fi
}

assert_no_token() {
  local artifact token
  for token in "$MOCK_CANARY_TOKEN" "$MOCK_CANARY_TOKEN_B"; do
    for artifact in "$tmp/stdout" "$tmp/stderr" "$tmp/logger.log" "$tmp/docker.argv" \
      "$tmp/curl.argv" "$tmp/rotate-a.stdout" "$tmp/rotate-a.stderr" \
      "$tmp/rotate-b.stdout" "$tmp/rotate-b.stderr"; do
      if [[ -f "$artifact" ]] && grep -Fq "$token" "$artifact"; then
        printf 'canary token leaked into %s\n' "$artifact" >&2
        return 1
      fi
    done
    while IFS= read -r -d '' artifact; do
      if grep -Fq "$token" "$artifact"; then
        printf 'canary token leaked into operation work file %s\n' "$artifact" >&2
        return 1
      fi
    done < <(find "$repo" -type f -print0)
  done
}

wait_for_file() {
  local path=$1 attempt
  for ((attempt = 0; attempt < 200; attempt++)); do
    [[ -e "$path" ]] && return 0
    sleep 0.01
  done
  printf 'timed out waiting for fixture event %s\n' "$path" >&2
  return 1
}

expect_status 0 "run $OPS_SHA bot-api-qa-bot create"
assert_no_token
[[ "$(head -n 1 "$tmp/stdout")" == "action=create bot_id=$MOCK_BOT_ID username=teagram_qa_test_bot" ]]
decrypted=$(tail -n +2 "$tmp/stdout" | TEST_AGE_IDENTITY="$identity" go run ./cmd/teagramd/testdata/bot-api-qa-age-fixture.go decrypt)
[[ "$decrypted" == "$MOCK_CANARY_TOKEN" ]]

expect_status 0 "run $OPS_SHA bot-api-qa-bot create"
assert_no_token
[[ "$(cat "$tmp/stdout")" == "state=exists bot_id=$MOCK_BOT_ID username=teagram_qa_test_bot token_active=true" ]]

expect_status 0 "run $OPS_SHA bot-api-qa-bot status"
assert_no_token
[[ "$(cat "$tmp/stdout")" == "state=found bot_id=$MOCK_BOT_ID username=teagram_qa_test_bot token_active=true" ]]

expect_status 0 "run $OPS_SHA bot-api-qa-bot revoke"
assert_no_token
[[ "$(cat "$tmp/stdout")" == "state=revoked bot_id=$MOCK_BOT_ID username=teagram_qa_test_bot action=revoke" ]]
revokes_before=$(grep -c 'bot revoke' "$tmp/docker.argv" || true)
expect_status 0 "run $OPS_SHA bot-api-qa-bot revoke"
assert_no_token
[[ "$(cat "$tmp/stdout")" == "state=revoked bot_id=$MOCK_BOT_ID username=teagram_qa_test_bot action=revoke" ]]
[[ "$(grep -c 'bot revoke' "$tmp/docker.argv" || true)" == "$revokes_before" ]]

expect_status 0 "run $OPS_SHA bot-api-qa-bot rotate"
assert_no_token
[[ "$(head -n 1 "$tmp/stdout")" == "action=rotate bot_id=$MOCK_BOT_ID username=teagram_qa_test_bot" ]]
decrypted=$(tail -n +2 "$tmp/stdout" | TEST_AGE_IDENTITY="$identity" go run ./cmd/teagramd/testdata/bot-api-qa-age-fixture.go decrypt)
[[ "$decrypted" == "$MOCK_CANARY_TOKEN" ]]

expect_status 0 "run $OPS_SHA bot-api-qa-bot probe"
assert_no_token
[[ "$(wc -l < "$tmp/stdout")" == 24 ]]
[[ "$(grep -c '^401$' "$tmp/stdout")" == 20 && "$(grep -c '^429$' "$tmp/stdout")" == 4 ]]
if grep -Evq '^(401|429)$' "$tmp/stdout"; then
  printf '%s\n' 'probe output included a non-status value' >&2
  exit 1
fi

docker_calls_before=$(wc -l < "$tmp/docker.argv")
expect_status 64 "run $OPS_SHA bot-api-qa-bot create extra"
assert_no_token
[[ "$(wc -l < "$tmp/docker.argv")" == "$docker_calls_before" ]]
expect_status 64 "run $OPS_SHA bot-api-qa-bot unknown"
assert_no_token
[[ "$(wc -l < "$tmp/docker.argv")" == "$docker_calls_before" ]]

printf '{"state":"found","owner":"user","id":%s,"kind":"human","first_name":"Teagram QA Test","token_active":false}\n' "$MOCK_BOT_ID" > "$repo/.bot-status"
creates_before=$(grep -c 'bot create' "$tmp/docker.argv" || true)
expect_status 1 "run $OPS_SHA bot-api-qa-bot create"
assert_no_token
if [[ "$(grep -c 'bot create' "$tmp/docker.argv" || true)" != "$creates_before" ]]; then
  printf '%s\n' 'operation attempted to create over a non-bot username' >&2
  exit 1
fi

printf '{"state":"found","owner":"user","id":%s,"kind":"bot","first_name":"Teagram QA Test","token_active":true}\n' "$MOCK_BOT_ID" > "$repo/.bot-status"
: > "$tmp/events"
(
  if SSH_ORIGINAL_COMMAND="run $OPS_SHA bot-api-qa-bot rotate" MOCK_RUN_LABEL=A \
    MOCK_ROTATE_TOKEN="$MOCK_CANARY_TOKEN" MOCK_FAIL_ENCRYPT=1 \
    bash "$tmp/dispatcher.sh" > "$tmp/rotate-a.stdout" 2> "$tmp/rotate-a.stderr"; then
    printf '0\n'
  else
    printf '%s\n' "$?"
  fi
) > "$tmp/rotate-a.status" &
rotate_a_pid=$!
wait_for_file "$tmp/encrypt-a-entered"
(
  if SSH_ORIGINAL_COMMAND="run $OPS_SHA bot-api-qa-bot rotate" MOCK_RUN_LABEL=B \
    MOCK_ROTATE_TOKEN="$MOCK_CANARY_TOKEN_B" \
    bash "$tmp/dispatcher.sh" > "$tmp/rotate-b.stdout" 2> "$tmp/rotate-b.stderr"; then
    printf '0\n'
  else
    printf '%s\n' "$?"
  fi
) > "$tmp/rotate-b.status" &
rotate_b_pid=$!
wait_for_file "$tmp/flock-b-entered"
for ((attempt = 0; attempt < 20; attempt++)); do
  if [[ -e "$tmp/lookup-b-entered" ]]; then
    printf '%s\n' 'overlapping rotation entered lookup while rotation A held the lock' >&2
    exit 1
  fi
  sleep 0.01
done
: > "$tmp/release-encrypt-a"
wait "$rotate_a_pid"
wait "$rotate_b_pid"
[[ "$(cat "$tmp/rotate-a.status")" == 1 && ! -s "$tmp/rotate-a.stdout" ]]
if [[ "$(cat "$tmp/rotate-b.status")" != 0 ]]; then
  printf 'overlapping rotation B exited %s\n' "$(cat "$tmp/rotate-b.status")" >&2
  cat "$tmp/rotate-b.stderr" >&2
  exit 1
fi
[[ "$(head -n 1 "$tmp/rotate-b.stdout")" == "action=rotate bot_id=$MOCK_BOT_ID username=teagram_qa_test_bot" ]]
decrypted=$(tail -n +2 "$tmp/rotate-b.stdout" | TEST_AGE_IDENTITY="$identity" go run ./cmd/teagramd/testdata/bot-api-qa-age-fixture.go decrypt)
[[ "$decrypted" == "$MOCK_CANARY_TOKEN_B" ]]
jq -e '.token_active == true' "$repo/.bot-status" >/dev/null
expected_events=$(printf 'lookup A\nrotate A\nrevoke A\nlookup B\nrotate B\nlookup B')
actual_events=$(grep -E '^(lookup|rotate|revoke) [AB]$' "$tmp/events")
[[ "$actual_events" == "$expected_events" ]]
assert_no_token
unset identity

printf '%s\n' 'Bot API QA bot operation fixture passed'
