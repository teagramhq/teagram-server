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
client_key_test="$web_root/src/tests/websocketExchangeKeySelection.test.ts"
client_lifecycle="$web_root/src/tests/websocketExchangeCaseLifecycle.mjs"
client_lifecycle_types="$web_root/src/tests/websocketExchangeCaseLifecycle.d.mts"
source_test="$repo_root/internal/mtproto/testdata/websocketExchangeExperiment.test.ts"
source_key_test="$repo_root/internal/mtproto/testdata/websocketExchangeKeySelection.test.ts"
source_lifecycle="$repo_root/internal/mtproto/testdata/websocketExchangeCaseLifecycle.mjs"
source_lifecycle_types="$repo_root/internal/mtproto/testdata/websocketExchangeCaseLifecycle.d.mts"
source_lifecycle_test="$repo_root/internal/mtproto/testdata/websocketExchangeCaseLifecycle.test.mjs"
generated_lang_version="$web_root/src/langPackLocalVersion.ts"
generated_lang_version_before=false
copied_client_test=false
copied_client_key_test=false
copied_client_lifecycle=false
copied_client_lifecycle_types=false
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
  if [[ "$copied_client_key_test" == true ]] && ! rm -f -- "$client_key_test"; then
    status=1
  fi
  if [[ "$copied_client_lifecycle" == true ]] && ! rm -f -- "$client_lifecycle"; then
    status=1
  fi
  if [[ "$copied_client_lifecycle_types" == true ]] && ! rm -f -- "$client_lifecycle_types"; then
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

if [[ -e "$client_test" || -L "$client_test" || -e "$client_key_test" || -L "$client_key_test" ]]; then
  echo "synthetic Web exchange test destination already exists" >&2
  exit 1
fi
if [[ -e "$client_lifecycle" || -L "$client_lifecycle" || -e "$client_lifecycle_types" || -L "$client_lifecycle_types" ]]; then
  echo "synthetic Web lifecycle test destination already exists" >&2
  exit 1
fi
cp -- "$source_test" "$client_test"
copied_client_test=true
cp -- "$source_key_test" "$client_key_test"
copied_client_key_test=true
cp -- "$source_lifecycle" "$client_lifecycle"
copied_client_lifecycle=true
cp -- "$source_lifecycle_types" "$client_lifecycle_types"
copied_client_lifecycle_types=true
node --test "$source_lifecycle_test" >/dev/null

pnpm --dir "$web_root" exec tsc --noEmit --pretty false
# Vite defines the shipped RSA manager's private-key branch at compile time.
# Keep this synthetic configuration in scope for the later Vitest child too.
export MTPROTO_TARGET_MODE=private
export MTPROTO_PRIVATE_ENDPOINT=wss://private.example.test:2443/apiws
export MTPROTO_PRIVATE_RSA_PUBLIC_KEY_FILE="$web_root/scripts/fixtures/private-mtproto-public.pem"
if ! pnpm --dir "$web_root" exec vitest run --pool=threads --maxWorkers=1 src/tests/websocketExchangeKeySelection.test.ts >/dev/null 2>&1; then
  echo "synthetic Web key selection regression failed" >&2
  exit 1
fi
pnpm --dir "$web_root" exec vite build --outDir "$web_build" --emptyOutDir
markers=(
  'teagram-web-authorizer-exchange-experiment-test-only'
  'synthetic_pre_request_failure'
  'synthetic_client_output_failure'
  'synthetic_server_dh_boundary'
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
  websocket_exchange_print_server_dh_boundary "$test_output"
  websocket_exchange_print_pre_request_markers "$test_output"
  websocket_exchange_print_client_output_failure "$test_output"
  failure_line="$(sed -nE 's/.*websocket_exchange_experiment_internal_test\.go:([0-9]+):.*/\1/p' "$test_output" | sed -n '1p')"
  if [[ "$failure_line" =~ ^[0-9]+$ ]]; then
    echo "synthetic_test_failure_line=$failure_line" >&2
  fi
  echo "synthetic Web exchange verification failed; inspect the sanitized report artifact" >&2
  exit 1
fi
websocket_exchange_print_server_dh_boundary "$test_output"
echo "focused_verification=passed"
