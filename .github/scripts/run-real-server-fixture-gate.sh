#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/../.." && pwd)"
cd "$repo_root"

python3 "$script_dir/test_real_server_fixture_gate.py"

json_file=$(mktemp "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/real-server-fixture-tests.XXXXXX")
trap 'rm -f -- "$json_file"' EXIT

status=0
go test -race -count=1 -timeout 15m -json -run '^TestRealServerFixture' ./test/e2e >"$json_file" 2>&1 || status=$?
if [[ "$status" -ne 0 ]]; then
  cat "$json_file" >&2
  exit "$status"
fi

python3 "$script_dir/real_server_fixture_gate.py" "$json_file"
