#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
source "$repo_root/.github/scripts/websocket-exchange-artifact-audit.sh"
web_root="${WEB_EXCHANGE_WEB_ROOT:?WEB_EXCHANGE_WEB_ROOT is required}"
expected_web_revision=52c3fd9d6f004346b4dcfcddc75e92030867b17a
server_source_revision=bfe00c6165d0bded84340e449542a03ec6a934a8
actual_web_revision="$(git -C "$web_root" rev-parse HEAD)"
if [[ "$actual_web_revision" != "$expected_web_revision" ]]; then
  echo "synthetic Web exchange source revision mismatch" >&2
  exit 1
fi
if ! git -C "$repo_root" merge-base --is-ancestor "$server_source_revision" HEAD; then
  echo "synthetic server exchange source revision mismatch" >&2
  exit 1
fi
pr_head_revision="${WEB_EXCHANGE_PR_HEAD_SHA:-}"
if [[ -n "$pr_head_revision" ]] && ! git -C "$repo_root" diff --quiet "$server_source_revision" "$pr_head_revision" -- \
  go.mod internal/mtproto/exchange.go internal/mtproto/exchange_diagnostics.go \
  internal/mtproto/server.go internal/mtproto/websocket.go; then
  echo "pull request changes the pinned synthetic server exchange runtime" >&2
  exit 1
fi

temp_root="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
artifact_report="${WEB_EXCHANGE_SANITIZED_ARTIFACT_PATH:?WEB_EXCHANGE_SANITIZED_ARTIFACT_PATH is required}"
case "$artifact_report" in
  "$temp_root"/teagram-websocket-exchange-*.json) ;;
  *) echo "synthetic Web exchange report must stay in the runner temp directory" >&2; exit 1 ;;
esac
if [[ -e "$artifact_report" || -L "$artifact_report" ]]; then
  echo "synthetic Web exchange report destination already exists" >&2
  exit 1
fi

tmpdir="$(mktemp -d "$temp_root/teagram-websocket-exchange.XXXXXX")"
sanitized_report="$tmpdir/sanitized-report.json"
audit_binary="$tmpdir/server-audit"
test_binary="$tmpdir/websocket-exchange.test"
test_output="$tmpdir/websocket-exchange-test-output.log"
web_build="$tmpdir/web-build"
server_source_root="$tmpdir/server-source"
client_test="$web_root/src/tests/websocketExchangeExperiment.test.ts"
source_test="$repo_root/internal/mtproto/testdata/websocketExchangeExperiment.test.ts"
generated_lang_version="$web_root/src/langPackLocalVersion.ts"
generated_lang_version_before=false
copied_client_test=false
if [[ -e "$generated_lang_version" || -L "$generated_lang_version" ]]; then
  generated_lang_version_before=true
fi

cleanup() {
  local status=$?
  trap - EXIT
  if [[ -f "$sanitized_report" ]]; then
    if ! install -m 600 -- "$sanitized_report" "$artifact_report"; then
      status=1
    fi
  fi
  if [[ "$copied_client_test" == true ]] && ! rm -f -- "$client_test"; then
    status=1
  fi
  if ! rm -f -- "$audit_binary" "$test_binary" "$test_output" "$sanitized_report"; then
    status=1
  fi
  if [[ "$generated_lang_version_before" == false ]] && ! rm -f -- "$generated_lang_version"; then
    status=1
  fi
  if [[ -d "$web_build" ]] && ! rm -rf -- "$web_build"; then
    status=1
  fi
  if [[ -d "$server_source_root" ]] && ! rm -rf -- "$server_source_root"; then
    status=1
  fi
  if ! rmdir -- "$tmpdir"; then
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT

if ! mkdir -- "$server_source_root"; then
  echo "pinned synthetic server source directory could not be created" >&2
  exit 1
fi
if ! git -C "$repo_root" archive --format=tar "$server_source_revision" | tar -xf - -C "$server_source_root"; then
  echo "pinned synthetic server source could not be materialized" >&2
  exit 1
fi
cp -- "$repo_root/internal/mtproto/websocket_exchange_experiment_internal_test.go" \
  "$server_source_root/internal/mtproto/websocket_exchange_experiment_internal_test.go"
echo "synthetic_server_source=$server_source_revision"
if [[ -n "${WEB_EXCHANGE_PR_BASE_SHA:-}" ]]; then
  echo "pull_request_base=${WEB_EXCHANGE_PR_BASE_SHA}"
fi

if [[ -e "$client_test" || -L "$client_test" ]]; then
  echo "synthetic Web exchange test destination already exists" >&2
  exit 1
fi
cp -- "$source_test" "$client_test"
copied_client_test=true

pnpm --dir "$web_root" exec tsc --noEmit --pretty false
MTPROTO_TARGET_MODE=private \
MTPROTO_PRIVATE_ENDPOINT=wss://private.example.test:2443/apiws \
MTPROTO_PRIVATE_RSA_PUBLIC_KEY_FILE="$web_root/scripts/fixtures/private-mtproto-public.pem" \
  pnpm --dir "$web_root" exec vite build --outDir "$web_build" --emptyOutDir
markers=(
  'teagram-web-authorizer-exchange-experiment-test-only'
  'synthetic_pre_request_failure'
)
for marker in "${markers[@]}"; do
  if ! websocket_exchange_marker_absent "$web_build" "$marker"; then
    echo "test-only exchange diagnostic marker audit failed for the Web production artifact" >&2
    exit 1
  fi
done

go -C "$server_source_root" build -o "$audit_binary" ./cmd/telegramd
for marker in "${markers[@]}"; do
  if ! websocket_exchange_marker_absent "$audit_binary" "$marker"; then
    echo "test-only exchange diagnostic marker audit failed for the server binary" >&2
    exit 1
  fi
done
echo "artifact_audit=passed test_only_marker_absent=true"

if ! go -C "$server_source_root" test -race -c -o "$test_binary" ./internal/mtproto; then
  echo "synthetic Web exchange test package did not compile" >&2
  exit 1
fi

if ! env \
  WEB_EXCHANGE_CI_ONLY=1 \
  WEB_EXCHANGE_WEB_ROOT="$web_root" \
  WEB_EXCHANGE_SANITIZED_ARTIFACT_PATH="$sanitized_report" \
  TMPDIR="$temp_root" \
  "$test_binary" -test.run '^TestWebClientExchangeExperiment$' -test.count=1 -test.timeout=120s >"$test_output" 2>&1; then
  websocket_exchange_print_pre_request_markers "$test_output"
  failure_line="$(sed -nE 's/.*websocket_exchange_experiment_internal_test\.go:([0-9]+):.*/\1/p' "$test_output" | sed -n '1p')"
  if [[ "$failure_line" =~ ^[0-9]+$ ]]; then
    echo "synthetic_test_failure_line=$failure_line" >&2
  fi
  echo "synthetic Web exchange verification failed; inspect the sanitized report artifact" >&2
  exit 1
fi
echo "focused_verification=passed"
