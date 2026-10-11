#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
server_root="${D1_SERVER_SOURCE:?D1_SERVER_SOURCE is required}"
web_root="${D1_WEB_CHECKOUT:?D1_WEB_CHECKOUT is required}"
repaired_web_root="${D1_REPAIRED_WEB_CHECKOUT:?D1_REPAIRED_WEB_CHECKOUT is required}"
server_source_revision=193b8c24357e22f3fc16099936e3bf187f935445
actual_server_revision="$(git -C "$server_root" rev-parse HEAD)"
if [[ "$actual_server_revision" != "$server_source_revision" ]]; then
  echo "D1 server source revision mismatch" >&2
  exit 1
fi

expected_web_revision=c88211e3985942343bf40dcbbcb8e8f5b4b7d364
actual_web_revision="$(git -C "$web_root" rev-parse HEAD)"
if [[ "$actual_web_revision" != "$expected_web_revision" ]]; then
  echo "D1 Web source revision mismatch" >&2
  exit 1
fi

expected_repaired_web_revision=569529c1f36923c098760ef727808254dcb6a662
actual_repaired_web_revision="$(git -C "$repaired_web_root" rev-parse HEAD)"
if [[ "$actual_repaired_web_revision" != "$expected_repaired_web_revision" ]]; then
  echo "D1 repaired Web source revision mismatch" >&2
  exit 1
fi

temp_root="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
artifact_report="${D1_SANITIZED_ARTIFACT_PATH:?D1_SANITIZED_ARTIFACT_PATH is required}"
case "$artifact_report" in
  "$temp_root"/teagram-websocket-d1-*.json) ;;
  *) echo "D1 sanitized report must stay in the runner temp directory" >&2; exit 1 ;;
esac
if [[ -e "$artifact_report" || -L "$artifact_report" ]]; then
  echo "D1 sanitized report destination already exists" >&2
  exit 1
fi
tmpdir="$(mktemp -d "$temp_root/teagram-websocket-d1.XXXXXX")"
sanitized_report="$tmpdir/sanitized-report.json"
baseline_report="$tmpdir/baseline-report.json"
client_test="$web_root/src/tests/websocketD1Interop.test.ts"
repaired_client_test="$repaired_web_root/src/tests/websocketD1RepairedInterop.test.ts"
server_test="$server_root/internal/mtproto/websocket_d1_internal_test.go"
server_testdata="$server_root/internal/mtproto/testdata"
server_vector="$server_testdata/web-client-req-pq.json"

cleanup_tmpdir() {
  local status=0
  if ! rm -f -- "$tmpdir/result.json" "$tmpdir/server-response.json" "$baseline_report" "$sanitized_report"; then
    status=1
  fi
  if ! rmdir -- "$tmpdir"; then
    status=1
  fi
  return "$status"
}
trap cleanup_tmpdir EXIT

if [[ -e "$client_test" || -L "$client_test" || -e "$repaired_client_test" || -L "$repaired_client_test" || \
  -e "$server_test" || -L "$server_test" || -e "$server_vector" || -L "$server_vector" ]]; then
  echo "D1 artifact destination already exists" >&2
  exit 1
fi
testdata_existed=false
if [[ -d "$server_testdata" ]]; then
  testdata_existed=true
else
  mkdir -p -- "$server_testdata"
fi
cleanup() {
  local status=$?
  trap - EXIT
  if [[ -f "$sanitized_report" ]]; then
    if ! install -m 600 -- "$sanitized_report" "$artifact_report"; then
      status=1
    fi
  fi
  if ! rm -f -- "$client_test" "$repaired_client_test" "$server_test" "$server_vector"; then
    status=1
  fi
  if [[ "$testdata_existed" == false ]]; then
    if ! rmdir -- "$server_testdata"; then
      status=1
    fi
  fi
  if ! cleanup_tmpdir; then
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT

cp "$repo_root/internal/mtproto/testdata/websocket_d1_client.test.ts" "$client_test"
cp "$repo_root/internal/mtproto/testdata/websocket_d1_repaired_client.test.ts" "$repaired_client_test"
cp "$repo_root/internal/mtproto/websocket_d1_internal_test.go" "$server_test"
cp "$repo_root/internal/mtproto/testdata/web-client-req-pq.json" "$server_vector"
export D1_VECTOR_PATH="$repo_root/internal/mtproto/testdata/web-client-req-pq.json"
export D1_OUTPUT_DIR="$tmpdir"
export D1_RESULT_PATH="$tmpdir/result.json"
export D1_RESPONSE_PATH="$tmpdir/server-response.json"
export D1_BASELINE_REPORT_PATH="$baseline_report"
export D1_SANITIZED_REPORT_PATH="$sanitized_report"

(cd "$server_root" && TMPDIR="$temp_root" go test -race -count=1 -timeout 45s -v ./internal/mtproto -run '^TestWebClientD1$')
pnpm --dir "$web_root" exec vitest run src/tests/websocketD1Interop.test.ts
pnpm --dir "$repaired_web_root" exec vitest run src/tests/websocketD1RepairedInterop.test.ts
