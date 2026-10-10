#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
web_root="${D1_WEB_CHECKOUT:?D1_WEB_CHECKOUT is required}"
server_source_revision=193b8c24357e22f3fc16099936e3bf187f935445
if ! git -C "$repo_root" cat-file -e "$server_source_revision^{commit}" 2>/dev/null; then
  git -C "$repo_root" fetch --quiet --no-tags origin "$server_source_revision"
fi
if ! git -C "$repo_root" diff --quiet "$server_source_revision" HEAD -- \
  internal/mtproto \
  ':(exclude)internal/mtproto/websocket_d1_test.go' \
  ':(exclude)internal/mtproto/testdata/web-client-req-pq.json' \
  ':(exclude)internal/mtproto/testdata/websocket_d1_client.test.ts'; then
  echo "D1 server source revision mismatch" >&2
  exit 1
fi

expected_web_revision=c88211e3985942343bf40dcbbcb8e8f5b4b7d364
actual_web_revision="$(git -C "$web_root" rev-parse HEAD)"
if [[ "$actual_web_revision" != "$expected_web_revision" ]]; then
  echo "D1 Web source revision mismatch" >&2
  exit 1
fi

tmpdir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/teagram-websocket-d1.XXXXXX")"
client_test="$web_root/src/tests/websocketD1Interop.test.ts"
if [[ -e "$client_test" ]]; then
  echo "D1 client test destination already exists" >&2
  rmdir "$tmpdir"
  exit 1
fi
cleanup() {
  rm -f -- "$client_test" "$tmpdir/result.json" "$tmpdir/server-response.bin"
  rmdir -- "$tmpdir"
}
trap cleanup EXIT

cp "$repo_root/internal/mtproto/testdata/websocket_d1_client.test.ts" "$client_test"
export D1_VECTOR_PATH="$repo_root/internal/mtproto/testdata/web-client-req-pq.json"
export D1_RESULT_PATH="$tmpdir/result.json"
export D1_RESPONSE_PATH="$tmpdir/server-response.bin"

go test -race -count=1 -timeout 45s -v ./internal/mtproto -run '^TestWebClientD1$'
pnpm --dir "$web_root" exec vitest run src/tests/websocketD1Interop.test.ts
