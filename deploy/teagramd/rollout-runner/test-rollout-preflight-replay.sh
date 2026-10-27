#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
RUNNER="$SCRIPT_DIR/rollout-runner.sh"
FIXTURE_DIR="$SCRIPT_DIR/testdata/main2612-compose-replay"
if [ "$(id -u)" != 0 ]; then
  printf '%s\n' 'run the preflight replay fixture as root to exercise production evidence checks' >&2
  exit 77
fi

TMP=$(mktemp -d /tmp/main2612-replay.XXXXXXXX)
FAILURE_TMP=$(mktemp -d /tmp/main2612-replay.XXXXXXXX)
mkdir -m 700 -- "$TMP/baseline" "$TMP/preflight" "$FAILURE_TMP/baseline" "$FAILURE_TMP/preflight"
cleanup() {
  rm -f -- \
    "$TMP/baseline/baseline.snapshot.json" \
    "$TMP/preflight/preflight.snapshot.json" \
    "$TMP/preflight/preflight-comparisons.tsv" \
    "$FAILURE_TMP/baseline/baseline.snapshot.json" \
    "$FAILURE_TMP/preflight/preflight.snapshot.json" \
    "$FAILURE_TMP/preflight/preflight-comparisons.tsv" \
    "$TMP/pass-rows.tsv" "$TMP/fail.stdout" "$TMP/fail.stderr"
  rmdir -- "$TMP/baseline" "$TMP/preflight" "$TMP" \
    "$FAILURE_TMP/baseline" "$FAILURE_TMP/preflight" "$FAILURE_TMP"
}
trap cleanup EXIT

cp -- "$FIXTURE_DIR/baseline.snapshot.json" "$TMP/baseline/baseline.snapshot.json"
cp -- "$FIXTURE_DIR/preflight.snapshot.json" "$TMP/preflight/preflight.snapshot.json"
chmod 600 -- "$TMP/baseline/baseline.snapshot.json" "$TMP/preflight/preflight.snapshot.json"
env ROLLOUT_RUNNER_EVIDENCE_ROOT="$TMP" \
  bash "$RUNNER" compare-preflight "$TMP/baseline" "$TMP/preflight" > "$TMP/pass-rows.tsv"
awk -F '\t' '
  NR == 1 { if ($1 != "field" || $2 != "result") failed = 1; next }
  NF != 2 || $2 != "pass" { failed = 1 }
  { rows++ }
  END { if (failed || rows != 12 || NR != 13) exit 1 }
' "$TMP/pass-rows.tsv"
printf '%s\n' 'PASS sanitized baseline and preflight fixture rows are field/result only and all pass'

cp -- "$FIXTURE_DIR/baseline.snapshot.json" "$FAILURE_TMP/baseline/baseline.snapshot.json"
jq '.env_sha256 = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"' \
  "$FIXTURE_DIR/preflight.snapshot.json" > "$FAILURE_TMP/preflight/preflight.snapshot.json"
chmod 600 -- "$FAILURE_TMP/baseline/baseline.snapshot.json" "$FAILURE_TMP/preflight/preflight.snapshot.json"
if env ROLLOUT_RUNNER_EVIDENCE_ROOT="$FAILURE_TMP" \
  bash "$RUNNER" compare-preflight "$FAILURE_TMP/baseline" "$FAILURE_TMP/preflight" \
  > "$TMP/fail.stdout" 2> "$TMP/fail.stderr"; then
  printf '%s\n' 'FAIL preflight fixture accepted an environment digest mismatch' >&2
  exit 1
fi
grep -q $'preflight\tenv_sha256\t.*\tfail$' "$FAILURE_TMP/preflight/preflight-comparisons.tsv"
[ ! -s "$TMP/fail.stdout" ]
printf '%s\n' 'PASS preflight fixture rejects an environment digest mismatch without printing comparison values'

if env ROLLOUT_RUNNER_EVIDENCE_ROOT=/root \
  bash "$RUNNER" compare-preflight "$FAILURE_TMP/baseline" "$FAILURE_TMP/preflight" \
  > /dev/null 2>&1; then
  printf '%s\n' 'FAIL preflight fixture accepted /root as replay evidence' >&2
  exit 1
fi
printf '%s\n' 'PASS preflight fixture rejects unguarded evidence roots'
