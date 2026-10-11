#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
web_root="${WEB_PERMANENT_WEB_ROOT:?WEB_PERMANENT_WEB_ROOT is required}"
server_source_revision=3dc485ef1cee45d77ae8c10428d35d83ee38cac8
web_source_revision=f4e1f7d251165e7211a900977f31fe865e4144cc
actual_web_revision="$(git -C "$web_root" rev-parse HEAD)"
if [[ "$actual_web_revision" != "$web_source_revision" ]]; then
  echo "pinned Web source revision mismatch" >&2
  exit 1
fi
if ! git -C "$repo_root" merge-base --is-ancestor "$server_source_revision" HEAD; then
  echo "pinned server source revision mismatch" >&2
  exit 1
fi
if ! git -C "$repo_root" diff --quiet "$server_source_revision" HEAD -- \
  go.mod go.sum internal/mtproto/exchange.go internal/mtproto/exchange_diagnostics.go \
  internal/mtproto/registry.go internal/mtproto/server.go internal/mtproto/websocket.go; then
  echo "pull request changes the pinned server exchange runtime" >&2
  exit 1
fi

temp_root="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
tmpdir="$(mktemp -d "$temp_root/teagram-web-single-exchange.XXXXXX")"
server_source_root="$tmpdir/server-source"
web_build="$tmpdir/web-build"
server_binary="$tmpdir/telegramd-audit"
web_test="$web_root/src/tests/websocketPermanentExchange.test.ts"
server_test="$server_source_root/internal/mtproto/websocket_permanent_exchange_internal_test.go"
web_test_copied=false
server_test_copied=false

cleanup() {
  local status=$?
  trap - EXIT
  if [[ "$web_test_copied" == true ]] && ! rm -f -- "$web_test"; then
    status=1
  fi
  if [[ "$server_test_copied" == true ]] && ! rm -f -- "$server_test"; then
    status=1
  fi
  if [[ -d "$web_build" ]] && ! rm -rf -- "$web_build"; then
    status=1
  fi
  if [[ -d "$server_source_root" ]] && ! rm -rf -- "$server_source_root"; then
    status=1
  fi
  if [[ -e "$server_binary" || -L "$server_binary" ]] && ! rm -f -- "$server_binary"; then
    status=1
  fi
  if ! rmdir -- "$tmpdir"; then
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT

if [[ -e "$web_test" || -L "$web_test" ]]; then
  echo "Web test destination already exists" >&2
  exit 1
fi
if [[ -e "$server_test" || -L "$server_test" ]]; then
  echo "server test destination already exists" >&2
  exit 1
fi

mkdir -- "$server_source_root"
if ! git -C "$repo_root" archive --format=tar "$server_source_revision" | tar -xf - -C "$server_source_root"; then
  echo "pinned server source could not be materialized" >&2
  exit 1
fi
cp -- "$repo_root/internal/mtproto/websocket_permanent_exchange_internal_test.go" \
  "$server_test"
server_test_copied=true
cp -- "$repo_root/internal/mtproto/testdata/websocketPermanentExchange.test.ts" \
  "$web_test"
web_test_copied=true

(
  cd "$web_root"
  MTPROTO_TARGET_MODE=private \
    MTPROTO_PRIVATE_ENDPOINT=wss://private.example.test:2443/apiws \
    MTPROTO_PRIVATE_RSA_PUBLIC_KEY_FILE="$web_root/scripts/fixtures/private-mtproto-public.pem" \
    pnpm exec tsc --noEmit --pretty false
  MTPROTO_TARGET_MODE=private \
    MTPROTO_PRIVATE_ENDPOINT=wss://private.example.test:2443/apiws \
    MTPROTO_PRIVATE_RSA_PUBLIC_KEY_FILE="$web_root/scripts/fixtures/private-mtproto-public.pem" \
    pnpm exec vite build --outDir "$web_build" --emptyOutDir
)

web_marker=teagram-web-single-permanent-exchange-web-test-only
if grep --recursive --text --quiet --fixed-strings -- "$web_marker" "$web_build"; then
  echo "Web production artifact contains its test-only marker" >&2
  exit 1
else
  status=$?
  if [[ "$status" -ne 1 ]]; then
    echo "Web production artifact audit could not scan the build" >&2
    exit "$status"
  fi
fi

go -C "$server_source_root" build -o "$server_binary" ./cmd/telegramd
server_marker=teagram-web-single-permanent-exchange-server-test-only
if grep --text --quiet --fixed-strings -- "$server_marker" "$server_binary"; then
  echo "server production binary contains its test-only marker" >&2
  exit 1
else
  status=$?
  if [[ "$status" -ne 1 ]]; then
    echo "server production binary audit could not scan the build" >&2
    exit "$status"
  fi
fi
echo "artifact_audit=passed web_test_marker_absent=true server_test_marker_absent=true"

MTPROTO_TARGET_MODE=private \
  MTPROTO_PRIVATE_ENDPOINT=wss://private.example.test:2443/apiws \
  MTPROTO_PRIVATE_RSA_PUBLIC_KEY_FILE="$web_root/scripts/fixtures/private-mtproto-public.pem" \
  WEB_PERMANENT_CI_ONLY=1 \
  WEB_PERMANENT_WEB_ROOT="$web_root" \
  TMPDIR="$temp_root" \
  go -C "$server_source_root" test -race -count=1 -timeout=70s -v ./internal/mtproto \
  -run '^TestWebClientPermanentExchange$'
