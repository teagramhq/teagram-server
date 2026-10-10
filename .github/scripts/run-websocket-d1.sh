#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
server_root="${D1_SERVER_SOURCE:?D1_SERVER_SOURCE is required}"
web_root="${D1_WEB_CHECKOUT:?D1_WEB_CHECKOUT is required}"
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

temp_root="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
tmpdir="$(mktemp -d "$temp_root/teagram-websocket-d1.XXXXXX")"
client_test="$web_root/src/tests/websocketD1Interop.test.ts"
server_test="$server_root/internal/mtproto/websocket_d1_internal_test.go"
server_testdata="$server_root/internal/mtproto/testdata"
server_vector="$server_testdata/web-client-req-pq.json"

cleanup_tmpdir() {
  rm -f -- "$tmpdir/result.json" "$tmpdir/server-response.bin"
  rmdir -- "$tmpdir"
}
trap cleanup_tmpdir EXIT

if [[ -e "$client_test" || -L "$client_test" || -e "$server_test" || -L "$server_test" || -e "$server_vector" || -L "$server_vector" ]]; then
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
  rm -f -- "$client_test" "$server_test" "$server_vector"
  if [[ "$testdata_existed" == false ]]; then
    rmdir -- "$server_testdata"
  fi
  cleanup_tmpdir
}
trap cleanup EXIT

cp "$repo_root/internal/mtproto/testdata/websocket_d1_client.test.ts" "$client_test"
cp "$repo_root/internal/mtproto/websocket_d1_internal_test.go" "$server_test"
cp "$repo_root/internal/mtproto/testdata/web-client-req-pq.json" "$server_vector"
export D1_VECTOR_PATH="$repo_root/internal/mtproto/testdata/web-client-req-pq.json"
export D1_OUTPUT_DIR="$tmpdir"
export D1_RESULT_PATH="$tmpdir/result.json"
export D1_RESPONSE_PATH="$tmpdir/server-response.bin"

(cd "$server_root" && TMPDIR="$temp_root" go test -race -count=1 -timeout 45s -v ./internal/mtproto -run '^TestWebClientD1$')
pnpm --dir "$web_root" exec vitest run src/tests/websocketD1Interop.test.ts
