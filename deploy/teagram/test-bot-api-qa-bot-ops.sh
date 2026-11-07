#!/usr/bin/env bash
set -Eeuo pipefail
ROOT=$(cd -P "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT
repo="$tmp/repo"
mkdir -p "$tmp/bin" "$repo/.state" "$tmp/ops"
export MOCK_TMP="$tmp" MOCK_REPO="$repo" MOCK_OPS_DIR="$tmp/ops"
export OPS_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export MOCK_BOT_ID=987654321
MOCK_CANARY_SECRET=$(printf 'B%.0s' {1..42})A
export MOCK_CANARY_TOKEN="${MOCK_BOT_ID}:${MOCK_CANARY_SECRET}"
export MOCK_ROOT_UID=$(id -u)

go build -o "$tmp/teagramd" ./cmd/teagramd
recipient=$(go run ./cmd/teagramd/testdata/bot-api-qa-age-fixture.go keygen "$tmp/identity")

python3 - "$ROOT/deploy/teagram/ops/bot-api-qa-bot.sh" "$tmp/ops/bot-api-qa-bot.sh" "$repo" "$recipient" "$MOCK_ROOT_UID" <<'PY'
from pathlib import Path
import sys

source = Path(sys.argv[1]).read_text()
replacements = {
    "readonly REPO=/opt/teagram": f"readonly REPO={sys.argv[3]}",
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
case "$*" in
  'inspect --format {{index .Config.Labels "org.opencontainers.image.revision"}} teagram-teagramd-1')
    printf '%s\n' "$OPS_SHA"
    ;;
  'exec teagram-teagramd-1 teagramd bot encrypt --check-recipient '*)
    "$MOCK_TMP/teagramd" bot encrypt --check-recipient "${@:7}"
    ;;
  'exec teagram-teagramd-1 teagramd bot lookup teagram_qa_test_bot')
    cat "$MOCK_REPO/.bot-status"
    ;;
  'exec teagram-teagramd-1 teagramd bot create teagram_qa_test_bot Teagram QA Test')
    printf '{"state":"found","owner":"user","id":%s,"kind":"bot","first_name":"Teagram QA Test","token_active":true}\n' "$MOCK_BOT_ID" > "$MOCK_REPO/.bot-status"
    printf '%s\n' "$MOCK_CANARY_TOKEN"
    ;;
  'exec teagram-teagramd-1 teagramd bot rotate '*)
    id=${*:6:1}
    [[ "$id" == "$MOCK_BOT_ID" ]] || exit 98
    printf '{"state":"found","owner":"user","id":%s,"kind":"bot","first_name":"Teagram QA Test","token_active":true}\n' "$MOCK_BOT_ID" > "$MOCK_REPO/.bot-status"
    printf '%s\n' "$MOCK_CANARY_TOKEN"
    ;;
  'exec teagram-teagramd-1 teagramd bot revoke '*)
    id=${*:6:1}
    [[ "$id" == "$MOCK_BOT_ID" ]] || exit 98
    printf '{"state":"found","owner":"user","id":%s,"kind":"bot","first_name":"Teagram QA Test","token_active":false}\n' "$MOCK_BOT_ID" > "$MOCK_REPO/.bot-status"
    ;;
  'exec -i teagram-teagramd-1 teagramd bot encrypt '*)
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
  if SSH_ORIGINAL_COMMAND=$command bash "$tmp/dispatcher.sh" > "$tmp/stdout" 2> "$tmp/stderr"; then
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
  local artifact
  for artifact in "$tmp/stdout" "$tmp/stderr" "$tmp/logger.log" "$tmp/docker.argv" "$tmp/curl.argv"; do
    if [[ -f "$artifact" ]] && grep -Fq "$MOCK_CANARY_TOKEN" "$artifact"; then
      printf 'canary token leaked into %s\n' "$artifact" >&2
      return 1
    fi
  done
  while IFS= read -r -d '' artifact; do
    if grep -Fq "$MOCK_CANARY_TOKEN" "$artifact"; then
      printf 'canary token leaked into operation work file %s\n' "$artifact" >&2
      return 1
    fi
  done < <(find "$repo" -type f -print0)
}

expect_status 0 "run $OPS_SHA bot-api-qa-bot create"
assert_no_token
[[ "$(head -n 1 "$tmp/stdout")" == "action=create bot_id=$MOCK_BOT_ID username=teagram_qa_test_bot" ]]
decrypted=$(tail -n +2 "$tmp/stdout" | go run ./cmd/teagramd/testdata/bot-api-qa-age-fixture.go decrypt "$tmp/identity")
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
decrypted=$(tail -n +2 "$tmp/stdout" | go run ./cmd/teagramd/testdata/bot-api-qa-age-fixture.go decrypt "$tmp/identity")
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

printf '%s\n' 'Bot API QA bot operation fixture passed'
