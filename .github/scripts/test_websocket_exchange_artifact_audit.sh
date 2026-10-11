#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
source "$script_dir/websocket-exchange-artifact-audit.sh"

test_source="$script_dir/../../internal/mtproto/websocket_exchange_experiment_internal_test.go"
report_init_line="$(awk '/writeWebExchangeReport\(artifactRoot, artifactName, unknownWebExchangeReport\(\)\)/ { print NR; exit }' "$test_source")"
first_exchange_line="$(awk '/runWebExchangeGotdControl\(ctx, privateKey\)/ { print NR; exit }' "$test_source")"
if [[ -z "$report_init_line" || -z "$first_exchange_line" || "$report_init_line" -ge "$first_exchange_line" ]]; then
  printf 'sanitized report must be initialized before synthetic exchange setup\n' >&2
  exit 1
fi

tmpdir="$(mktemp -d "${TMPDIR:-/tmp}/websocket-exchange-audit-test.XXXXXX")"
original_path="$PATH"
cleanup() {
  PATH="$original_path"
  export PATH
  rm -rf -- "$tmpdir"
}
trap cleanup EXIT

mkdir -p "$tmpdir/bin" "$tmpdir/bundle"
grep_path="$(command -v grep)"
ln -s "$grep_path" "$tmpdir/bin/grep"
PATH="$tmpdir/bin"
export PATH

marker='teagram-web-authorizer-exchange-experiment-test-only'
printf 'ordinary application output\n' >"$tmpdir/bundle/app.js"
if ! websocket_exchange_marker_absent "$tmpdir/bundle" "$marker"; then
  printf 'marker-free artifact was not accepted without ripgrep\n' >&2
  exit 1
fi

printf '%s\n' "$marker" >"$tmpdir/bundle/diagnostic.js"
if websocket_exchange_marker_absent "$tmpdir/bundle" "$marker"; then
  printf 'artifact containing the test-only marker was accepted\n' >&2
  exit 1
fi

if websocket_exchange_marker_absent "$tmpdir/missing" "$marker" 2>/dev/null; then
  printf 'artifact audit error was treated as marker absence\n' >&2
  exit 1
fi

PATH="$tmpdir/no-tools"
export PATH
if websocket_exchange_marker_absent "$tmpdir/bundle/app.js" "$marker" 2>/dev/null; then
  printf 'artifact audit with a missing scanner was accepted\n' >&2
  exit 1
fi

PATH="$original_path"
export PATH
printf 'websocket exchange artifact audit tests passed\n'
