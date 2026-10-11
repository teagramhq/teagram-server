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

markers=(
  'teagram-web-authorizer-exchange-experiment-test-only'
  'synthetic_pre_request_failure'
  'synthetic_client_output_failure'
)
printf 'ordinary application output\n' >"$tmpdir/bundle/app.js"
for marker in "${markers[@]}"; do
  if ! websocket_exchange_marker_absent "$tmpdir/bundle" "$marker"; then
    printf 'marker-free artifact was not accepted without ripgrep\n' >&2
    exit 1
  fi

  printf '%s\n' "$marker" >"$tmpdir/bundle/diagnostic.js"
  if websocket_exchange_marker_absent "$tmpdir/bundle" "$marker"; then
    printf 'artifact containing the test-only marker was accepted\n' >&2
    exit 1
  fi
done

printf '%s\n' \
  'websocket_exchange_experiment_internal_test.go:7: synthetic_pre_request_failure case=A category=transport_selection' \
  'websocket_exchange_experiment_internal_test.go:8: synthetic_pre_request_failure case=B category=raw-error-value' \
  >"$tmpdir/marker-output.log"
actual="$(websocket_exchange_print_pre_request_markers "$tmpdir/marker-output.log")"
expected="$(printf '%s\n' \
  'synthetic_pre_request_failure case=A category=transport_selection' \
  'synthetic_pre_request_failure case=B category=unknown')"
if [[ "$actual" != "$expected" ]]; then
  printf 'pre-request marker output was not sanitized\n' >&2
  exit 1
fi

printf '%s\n' \
  'websocket_exchange_experiment_internal_test.go:9: synthetic_client_output_failure category=file_read' \
  >"$tmpdir/client-output-marker.log"
actual="$(websocket_exchange_print_client_output_failure "$tmpdir/client-output-marker.log")"
expected='synthetic_client_output_failure category=file_read'
if [[ "$actual" != "$expected" ]]; then
  printf 'client output marker category was not preserved\n' >&2
  exit 1
fi

printf '%s\n' \
  'websocket_exchange_experiment_internal_test.go:9: synthetic_client_output_failure category=raw-error-value' \
  >"$tmpdir/client-output-marker.log"
actual="$(websocket_exchange_print_client_output_failure "$tmpdir/client-output-marker.log")"
expected='synthetic_client_output_failure category=unknown'
if [[ "$actual" != "$expected" ]]; then
  printf 'malformed client output marker was not failed closed\n' >&2
  exit 1
fi

printf '%s\n' \
  'websocket_exchange_experiment_internal_test.go:9: synthetic_client_output_failure category=file_read' \
  'websocket_exchange_experiment_internal_test.go:10: synthetic_client_output_failure category=json_decode' \
  >"$tmpdir/client-output-marker.log"
actual="$(websocket_exchange_print_client_output_failure "$tmpdir/client-output-marker.log")"
expected='synthetic_client_output_failure category=unknown'
if [[ "$actual" != "$expected" ]]; then
  printf 'duplicate client output markers were not collapsed to unknown\n' >&2
  exit 1
fi

printf '%s\n' \
  'websocket_exchange_experiment_internal_test.go:9: synthetic_client_output_failure category=json_decode' \
  >"$tmpdir/client-output-marker.log"
actual="$(websocket_exchange_print_client_output_failure "$tmpdir/client-output-marker.log")"
expected='synthetic_client_output_failure category=json_decode'
if [[ "$actual" != "$expected" ]]; then
  printf 'client output marker category was not preserved\n' >&2
  exit 1
fi

for marker in "${markers[@]}"; do
  if websocket_exchange_marker_absent "$tmpdir/missing" "$marker" 2>/dev/null; then
    printf 'artifact audit error was treated as marker absence\n' >&2
    exit 1
  fi
done

PATH="$tmpdir/no-tools"
export PATH
for marker in "${markers[@]}"; do
  if websocket_exchange_marker_absent "$tmpdir/bundle/app.js" "$marker" 2>/dev/null; then
    printf 'artifact audit with a missing scanner was accepted\n' >&2
    exit 1
  fi
done

PATH="$original_path"
export PATH
printf 'websocket exchange artifact audit tests passed\n'
