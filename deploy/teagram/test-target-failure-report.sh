#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/teagram/ops/target-failure-report.sh
source "$SCRIPT_DIR/ops/target-failure-report.sh"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/target-failure-report.XXXXXXXX")
chmod 700 "$TMP"
cleanup() {
  status=$?
  trap - EXIT
  case "$TMP" in
    "${TMPDIR:-/tmp}"/target-failure-report.????????) rm -rf -- "$TMP" || status=1 ;;
    *) printf '%s\n' 'refusing to remove an unexpected fixture path' >&2; status=1 ;;
  esac
  exit "$status"
}
trap cleanup EXIT

root=$TMP/evidence
mkdir -m 700 "$root"
target_sha=abc1234defab0123456789abcdef0123456789ab
source_sha=9876543210abcdef9876543210abcdef98765432
target_dir=$root/main1238-abc1234defab-20261027T124539Z.target
mkdir -m 700 "$target_dir"
printf 'captured_at_utc=2026-10-27T12:45:39Z target_sha=%s source_sha=%s reason=target_readiness_rejected_exit_1 target_snapshot=%064d\n' \
  "$target_sha" "$source_sha" 0 > "$target_dir/target-failure.txt"
printf 'role=target result=failed exit=1 max_seconds=120\n' > "$target_dir/readiness-target.result"
printf '2026-10-27T12:45:39Z\trole=target\telapsed=10\tcontainer_id=private-container-id\tstate=exited\texit=1\tadvertise=0\terrors=1\tmigrate=exited/0\tpostgres=healthy\ttcp=connected\ttls=failed\ttls_http=000\ttls_verify=1\n' \
  > "$target_dir/readiness-target.tsv"
printf 'mode=post\nfailed_checks=revision_20261026000001\ngate_result=reject\n' > "$target_dir/schema-result-gate-post.tsv"
printf '{"password":"fixture-secret-must-not-appear"}\n' > "$target_dir/target.snapshot.json"
chmod 600 "$target_dir"/*

output=$(report_target_failure "$root" abc1234 "$(id -u)")
grep -Fxq "target_sha=$target_sha" <<< "$output"
grep -Fxq 'target_failure_gate=target_readiness_rejected_exit_1' <<< "$output"
grep -Fxq 'readiness_result=failed' <<< "$output"
grep -Fxq 'readiness_state=exited' <<< "$output"
grep -Fxq 'readiness_errors=1' <<< "$output"
grep -Fxq 'schema_post_failed_checks=revision_20261026000001' <<< "$output"
grep -Fxq 'schema_post_gate_result=reject' <<< "$output"
if grep -Eq 'private-container-id|fixture-secret-must-not-appear|target\.snapshot' <<< "$output"; then
  printf '%s\n' 'failure report exposed private evidence' >&2
  exit 1
fi
if report_target_failure "$root" ffffffff "$(id -u)" >/dev/null 2>&1; then
  printf '%s\n' 'failure report accepted an unknown target' >&2
  exit 1
fi
printf '%s\n' 'target failure report fixture passed'
