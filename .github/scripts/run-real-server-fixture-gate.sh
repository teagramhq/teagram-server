#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/../.." && pwd)"
cd "$repo_root"
source "$script_dir/smoke-diagnostics.sh"

python3 "$script_dir/test_real_server_fixture_gate.py"

json_file=$(mktemp "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/real-server-fixture-tests.XXXXXX")
prewarm_provenance_dir=""
cleanup() {
  rm -f -- "$json_file"
  if [[ -n "$prewarm_provenance_dir" ]]; then
    rm -rf -- "$prewarm_provenance_dir"
  fi
}
trap cleanup EXIT
prewarm_provenance_dir=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/e2e-prewarm-provenance.XXXXXX")
export TEAGRAM_E2E_PREWARM_PROVENANCE_DIR="$prewarm_provenance_dir"

status=0
go test -race -count=1 -timeout 15m -json -run '^TestRealServerFixture' ./test/e2e >"$json_file" 2>&1 || status=$?

command_token=$(smoke_generate_command_token 2>/dev/null) || command_token=""
if [[ "$command_token" =~ ^[0-9a-f]{64}$ ]]; then
  passthrough_status=0
  smoke_emit_raw_output "$json_file" "$command_token" || passthrough_status=$?
fi

if [[ "$status" -ne 0 ]]; then
  report_smoke_failure_diagnostics "$status" real-server-fixtures "$json_file" || true
  exit "$status"
fi

if ! python3 "$script_dir/real_server_fixture_gate.py" "$json_file"; then
  checked_out_commit=$(git rev-parse HEAD 2>/dev/null) || checked_out_commit="unavailable"
  if [[ ! "$checked_out_commit" =~ ^[0-9a-f]{40}$ ]]; then
    checked_out_commit="unavailable"
  fi
  echo "::error::E2E fixture lane did not report every required real-server fixture test (checked-out commit: $checked_out_commit)"
  exit 1
fi
