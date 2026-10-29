#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
OPS_SCRIPT="$SCRIPT_DIR/main2612-live-compose-replay.sh"
TMP=$(mktemp -d /tmp/main2612-replay-safety.XXXXXXXX)
CLEANUP_FIXTURE=
SUCCESS_FIXTURE=
MOUNT_FIXTURE=
REJECT_FIXTURE=
SUCCESS_TOKEN=0123456789abcdef0123456789abcdef

cleanup_test() {
  local status=$?
  trap - EXIT
  if [ -n "$CLEANUP_FIXTURE" ]; then
    [ ! -d "$CLEANUP_FIXTURE/live" ] || rmdir -- "$CLEANUP_FIXTURE/live" || status=1
    [ ! -d "$CLEANUP_FIXTURE" ] || rmdir -- "$CLEANUP_FIXTURE" || status=1
  fi
  if [ -n "$SUCCESS_FIXTURE" ] && [ -d "$SUCCESS_FIXTURE" ]; then
    marker="$SUCCESS_FIXTURE/.rollout-owned-$SUCCESS_TOKEN"
    [ ! -f "$marker" ] || unlink -- "$marker" || status=1
    rmdir -- "$SUCCESS_FIXTURE" || status=1
  fi
  if [ -n "$MOUNT_FIXTURE" ] && [ -d "$MOUNT_FIXTURE" ]; then
    [ ! -f "$MOUNT_FIXTURE/live/evidence/result.json" ] || unlink -- "$MOUNT_FIXTURE/live/evidence/result.json" || status=1
    [ ! -d "$MOUNT_FIXTURE/live/evidence" ] || rmdir -- "$MOUNT_FIXTURE/live/evidence" || status=1
    [ ! -d "$MOUNT_FIXTURE/live" ] || rmdir -- "$MOUNT_FIXTURE/live" || status=1
    [ ! -f "$MOUNT_FIXTURE/.rollout-owned-$SUCCESS_TOKEN" ] || unlink -- "$MOUNT_FIXTURE/.rollout-owned-$SUCCESS_TOKEN" || status=1
    rmdir -- "$MOUNT_FIXTURE" || status=1
  fi
  if [ -n "$REJECT_FIXTURE" ] && [ -d "$REJECT_FIXTURE" ]; then
    rmdir -- "$REJECT_FIXTURE" || status=1
  fi
  for file in mock-mount mock-findmnt mock-umount mount.state mount.calls umount.calls; do
    [ ! -f "$TMP/$file" ] || unlink -- "$TMP/$file" || status=1
  done
  [ ! -f "$TMP/expected-field-results.tsv" ] || unlink -- "$TMP/expected-field-results.tsv" || status=1
  [ ! -f "$TMP/approved-field-results.tsv" ] || unlink -- "$TMP/approved-field-results.tsv" || status=1
  [ ! -f "$TMP/field-results.valid.tsv" ] || unlink -- "$TMP/field-results.valid.tsv" || status=1
  [ ! -f "$TMP/field-results.untrusted.tsv" ] || unlink -- "$TMP/field-results.untrusted.tsv" || status=1
  [ ! -f "$TMP/field-results.oversized.tsv" ] || unlink -- "$TMP/field-results.oversized.tsv" || status=1
  [ ! -f "$TMP/synthetic.env.source" ] || unlink -- "$TMP/synthetic.env.source" || status=1
  [ ! -f "$TMP/synthetic.env.output" ] || unlink -- "$TMP/synthetic.env.output" || status=1
  rmdir -- "$TMP" || status=1
  exit "$status"
}
trap cleanup_test EXIT

bash -n "$OPS_SCRIPT"
grep -Fq 'umask 077' "$OPS_SCRIPT"
grep -Fq "TMP_TOKEN=\$(mark_replay_tree \"\$TMP\")" "$OPS_SCRIPT"
grep -Fq "write_synthetic_env \"\$env_source\" \"\$tree/\$env_name\"" "$OPS_SCRIPT"
grep -Fq "unlink -- \"\$env_path\"" "$OPS_SCRIPT"
grep -Fq "validate_result_rows \"\$TMP/logs/field-results.tsv\"" "$OPS_SCRIPT"
grep -Fq "mount_replay_tmpfs \"\$TMP\" || die" "$OPS_SCRIPT"
grep -Fq 'ulimit -t 60' "$OPS_SCRIPT"
grep -Fq 'ulimit -v 1048576' "$OPS_SCRIPT"
grep -Fq 'ulimit -f 4096' "$OPS_SCRIPT"
grep -Fq -- '--property=MemoryMax=1G' "$OPS_SCRIPT"
grep -Fq -- '--property=CPUQuota=100%' "$OPS_SCRIPT"
grep -Fq -- '--property=TasksMax=64' "$OPS_SCRIPT"
if grep -Fq "cat -- \"\$TMP/logs/field-results.tsv\"" "$OPS_SCRIPT"; then
  printf '%s\n' 'FAIL live replay prints result rows before guarded cleanup' >&2
  exit 1
fi

if grep -Eq '(^|[[:space:]])(/bin/)?rm[[:space:]]+-rf([[:space:]]|$)' "$OPS_SCRIPT"; then
  printf '%s\n' 'FAIL live replay bypasses guarded tree cleanup' >&2
  exit 1
fi
if grep -Eq 'source[[:space:]].*rollout-(runner|verifier)\.sh' "$OPS_SCRIPT"; then
  printf '%s\n' 'FAIL live replay sources candidate runner or verifier code' >&2
  exit 1
fi
grep -Fq "git -C \"\$TMP/candidate.git\" fetch --quiet --no-tags --depth=1 \"\$CANDIDATE_REMOTE\" \"\$CANDIDATE_SHA\"" "$OPS_SCRIPT"
grep -Fq "rollout_safe_remove_tree \"\$target\" \"\$TMP\" \"\$TMP_TOKEN\" \"\$LIVE_CHECKOUT\"" "$OPS_SCRIPT"
grep -Fq "run_candidate_sandbox \"\$tree\" /usr/bin/env" "$OPS_SCRIPT"
grep -Fq "bash \"\$tree/deploy/teagramd/rollout-runner/rollout-verifier.sh\" snapshot" "$OPS_SCRIPT"
grep -Fq 'run_candidate_sandbox /workspace/pr /usr/bin/env' "$OPS_SCRIPT"
grep -Fq 'compare-preflight /root/baseline /root/preflight' "$OPS_SCRIPT"
grep -Fq 'require_replay_sandbox' "$OPS_SCRIPT"
grep -Fq -- '--unshare-net' "$OPS_SCRIPT"
grep -Fq 'stage_synthetic_secrets' "$OPS_SCRIPT"
if grep -Fq 'source /dev/stdin' "$OPS_SCRIPT" || grep -Fq 'safe_remove_tree_source=' "$OPS_SCRIPT"; then
  printf '%s\n' 'FAIL live replay executes candidate-controlled cleanup code on the host' >&2
  exit 1
fi

cat > "$TMP/mock-mount" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[ "$#" -eq 6 ] && [ "$1" = -t ] && [ "$2" = tmpfs ] && [ "$3" = -o ] && \
  [ "$4" = size=134217728,nr_inodes=8192,mode=0700,nodev,nosuid ] && \
  [ "$5" = tmpfs ] || exit 64
printf '%s\n' "$*" >> "$MOCK_MOUNT_CALLS"
printf '%s\n' "$6" > "$MOCK_MOUNT_STATE"
EOF
cat > "$TMP/mock-findmnt" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[ "$#" -eq 5 ] && [ "$1" = -n ] && [ "$2" = -o ] && [ "$4" = --target ] || exit 64
[ -f "$MOCK_MOUNT_STATE" ] && [ "$(<"$MOCK_MOUNT_STATE")" = "$5" ] || exit 1
case "$3" in
  FSTYPE) printf '%s\n' "${MOCK_FINDMNT_FSTYPE:-tmpfs}" ;;
  TARGET) printf '%s\n' "${MOCK_FINDMNT_TARGET:-$5}" ;;
  OPTIONS) printf '%s\n' "${MOCK_FINDMNT_OPTIONS:-}" ;;
  *) exit 64 ;;
esac
EOF
cat > "$TMP/mock-umount" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[ "$#" -eq 2 ] && [ "$1" = -- ] || exit 64
[ -f "$MOCK_MOUNT_STATE" ] && [ "$(<"$MOCK_MOUNT_STATE")" = "$2" ] || exit 1
printf '%s\n' "$2" >> "$MOCK_UMOUNT_CALLS"
unlink -- "$MOCK_MOUNT_STATE"
EOF
chmod 700 "$TMP/mock-mount" "$TMP/mock-findmnt" "$TMP/mock-umount"

MOUNT_FIXTURE=$(mktemp -d /tmp/main2612-replay.XXXXXXXX)
success_rows=$(MOCK_MOUNT_STATE="$TMP/mount.state" MOCK_MOUNT_CALLS="$TMP/mount.calls" \
  MOCK_UMOUNT_CALLS="$TMP/umount.calls" \
  MOCK_FINDMNT_OPTIONS='rw,nosuid,nodev,mode=700,nr_inodes=8192,size=131072k' \
  bash -c '
  set -Eeuo pipefail
  source "$1"
  TMP=$2
  MOUNT_BIN=$3
  FINDMNT_BIN=$4
  UMOUNT_BIN=$5
  TMP_TOKEN=$6
  id() { [ "$1" = -u ] || return 2; printf "0\n"; }
  stat() {
    case "$2" in
      %u) printf "0\n" ;;
      %a)
        case "$4" in
          "$TMP"|"$TMP/live"|"$TMP/live/evidence") printf "700\n" ;;
          "$TMP/.rollout-owned-$TMP_TOKEN") printf "600\n" ;;
          *) return 2 ;;
        esac
        ;;
      *) return 2 ;;
    esac
  }
  mount_replay_tmpfs "$TMP"
  [ "$TMP_MOUNTED" -eq 1 ]
  printf "%s\n" "$TMP_TOKEN" > "$TMP/.rollout-owned-$TMP_TOKEN"
  chmod 600 "$TMP/.rollout-owned-$TMP_TOKEN"
  mkdir -m 700 "$TMP/live"
  mkdir -m 700 "$TMP/live/evidence"
  printf "%s\n" synthetic-mounted-evidence > "$TMP/live/evidence/result.json"
  chmod 600 "$TMP/live/evidence/result.json"
  RESULT_ROWS=$7
  cleanup
' _ "$OPS_SCRIPT" "$MOUNT_FIXTURE" "$TMP/mock-mount" "$TMP/mock-findmnt" "$TMP/mock-umount" \
  "$SUCCESS_TOKEN" $'field\tresult\ncompose\tpass')
[ "$success_rows" = $'field\tresult\ncompose\tpass' ]
[ ! -e "$MOUNT_FIXTURE" ]
grep -Fqx -- "-t tmpfs -o size=134217728,nr_inodes=8192,mode=0700,nodev,nosuid tmpfs $MOUNT_FIXTURE" "$TMP/mount.calls"
grep -Fqx "$MOUNT_FIXTURE" "$TMP/umount.calls"
MOUNT_FIXTURE=
printf '%s\n' 'PASS replay mounts verified bounded tmpfs and unmounts it during guarded cleanup'

for rejected_options in \
  'rw,nosuid,nodev,mode=700,nr_inodes=8192,size=262144k' \
  'rw,nosuid,nodev,mode=700,nr_inodes=16384,size=131072k' \
  'rw,nosuid,nodev,mode=755,nr_inodes=8192,size=131072k' \
  'rw,nosuid,mode=700,nr_inodes=8192,size=131072k' \
  'rw,nodev,mode=700,nr_inodes=8192,size=131072k'; do
  REJECT_FIXTURE=$(mktemp -d /tmp/main2612-replay.XXXXXXXX)
  rejected=$(MOCK_MOUNT_STATE="$TMP/mount.state" MOCK_MOUNT_CALLS="$TMP/mount.calls" \
    MOCK_UMOUNT_CALLS="$TMP/umount.calls" MOCK_FINDMNT_OPTIONS="$rejected_options" \
    bash -c '
    set -Eeuo pipefail
    source "$1"
    TMP=$2
    MOUNT_BIN=$3
    FINDMNT_BIN=$4
    UMOUNT_BIN=$5
    id() { [ "$1" = -u ] || return 2; printf "0\n"; }
    stat() {
      case "$2" in
        %u) printf "0\n" ;;
        %a) [ "$4" = "$TMP" ] && printf "700\n" ;;
        *) return 2 ;;
      esac
    }
    if mount_replay_tmpfs "$TMP"; then
      printf "%s\n" "FAIL replay accepted invalid tmpfs options: $MOCK_FINDMNT_OPTIONS" >&2
      exit 1
    fi
    [ "$TMP_MOUNTED" -eq 1 ]
    unmount_replay_tmpfs
    [ "$TMP_MOUNTED" -eq 0 ]
    rmdir -- "$TMP"
    printf "%s\n" rejected
  ' _ "$OPS_SCRIPT" "$REJECT_FIXTURE" "$TMP/mock-mount" "$TMP/mock-findmnt" "$TMP/mock-umount")
  [ "$rejected" = rejected ]
  [ ! -e "$REJECT_FIXTURE" ]
done
REJECT_FIXTURE=
printf '%s\n' 'PASS replay rejects oversized, over-inode, permissive-mode, and missing nodev/nosuid tmpfs options and unmounts rejected mounts'

EXPECTED_ROWS="$TMP/expected-field-results.tsv"
printf 'field\tresult\nconfig_sha256\tpass\nenv_sha256\tpass\noverride_sha256\tpass\nstop_grace_period\tpass\ncompose_logging_policy\tpass\ncompose_log_login_codes\tpass\nmedia_key_source\tpass\nmedia_key_current\tpass\ncompose_public_ws_2446\tpass\nexposure_public_ws_2446\tpass\ncompose_replica_count\tpass\ncompose_client_addr_trust\tpass\n' \
  > "$TMP/approved-field-results.tsv"
bash -c '
  set -Eeuo pipefail
  source "$1"
  EXPECTED_RESULT_ROWS=$2
  write_expected_result_rows
' _ "$OPS_SCRIPT" "$EXPECTED_ROWS"
cmp -s -- "$TMP/approved-field-results.tsv" "$EXPECTED_ROWS"
cp -- "$EXPECTED_ROWS" "$TMP/field-results.valid.tsv"
chmod 600 -- "$TMP/field-results.valid.tsv"
valid_rows=$(bash -c '
  set -Eeuo pipefail
  source "$1"
  EXPECTED_RESULT_ROWS=$2
  validate_result_rows "$3"
  printf "%s" "$RESULT_ROWS"
' _ "$OPS_SCRIPT" "$EXPECTED_ROWS" "$TMP/field-results.valid.tsv")
[ "$valid_rows" = "$(<"$TMP/approved-field-results.tsv")" ]
{
  head -n 1 "$TMP/approved-field-results.tsv"
  printf 'operator_setting=synthetic-fixture\tpass\n'
  tail -n +3 "$TMP/approved-field-results.tsv"
} > "$TMP/field-results.untrusted.tsv"
chmod 600 -- "$TMP/field-results.untrusted.tsv"
if bash -c '
  set -Eeuo pipefail
  source "$1"
  EXPECTED_RESULT_ROWS=$2
  validate_result_rows "$3"
' _ "$OPS_SCRIPT" "$EXPECTED_ROWS" "$TMP/field-results.untrusted.tsv"; then
  printf '%s\n' 'FAIL host row validator accepted a candidate-controlled field' >&2
  exit 1
fi
head -c 1025 /dev/zero > "$TMP/field-results.oversized.tsv"
chmod 600 -- "$TMP/field-results.oversized.tsv"
if bash -c '
  set -Eeuo pipefail
  source "$1"
  EXPECTED_RESULT_ROWS=$2
  validate_result_rows "$3"
' _ "$OPS_SCRIPT" "$EXPECTED_ROWS" "$TMP/field-results.oversized.tsv"; then
  printf '%s\n' 'FAIL host row validator accepted oversized candidate output' >&2
  exit 1
fi
printf '%s\n' 'PASS host accepts only bounded, exact preflight result rows'
cat > "$TMP/synthetic.env.source" <<'EOF'
POSTGRES_PASSWORD=raw-postgres-fixture-value
TEAGRAM_TURNSTILE_SITE_KEY=raw-site-fixture-value
TEAGRAM_TURNSTILE_SECRET_KEY_FILE=/root/real-turnstile-secret
TG_BLOB_S3_SECRET_ACCESS_KEY=raw-s3-fixture-value
EOF
bash -c '
  set -Eeuo pipefail
  source "$1"
  write_synthetic_env "$2" "$3" /workspace/live
' _ "$OPS_SCRIPT" "$TMP/synthetic.env.source" "$TMP/synthetic.env.output"
if grep -Fq 'raw-' "$TMP/synthetic.env.output" || grep -Fq '/root/real-turnstile-secret' "$TMP/synthetic.env.output"; then
  printf '%s\n' 'FAIL synthetic replay environment retained a source value' >&2
  exit 1
fi
grep -Fqx 'POSTGRES_PASSWORD=replay-postgres-password' "$TMP/synthetic.env.output"
grep -Fqx 'TEAGRAM_TURNSTILE_SITE_KEY=replay-site-key' "$TMP/synthetic.env.output"
grep -Fqx 'TEAGRAM_TURNSTILE_SECRET_KEY_FILE=/workspace/live/.secrets/teagramd-turnstile' "$TMP/synthetic.env.output"
grep -Fqx 'TG_BLOB_S3_SECRET_ACCESS_KEY=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' "$TMP/synthetic.env.output"
printf '%s\n' 'PASS replay uses synthetic environment and secret values'
unlink -- "$TMP/synthetic.env.source"
unlink -- "$TMP/synthetic.env.output"
grep -Fq 'com.docker.compose.project.config_files' "$OPS_SCRIPT"
if grep -Fq "git -C \"\$LIVE_CHECKOUT\" fetch" "$OPS_SCRIPT"; then
  printf '%s\n' 'FAIL candidate fetch writes into the live checkout' >&2
  exit 1
fi

selected_sha=1111111111111111111111111111111111111111
candidate_sha=2222222222222222222222222222222222222222
resolved=$(OPS_SHA="$selected_sha" bash -c '
  set -Eeuo pipefail
  source "$1"
  resolve_candidate_sha "$2"
  [ "$CANDIDATE_SHA" = "$2" ]
  resolve_candidate_sha
  [ "$CANDIDATE_SHA" = "$OPS_SHA" ]
  if resolve_candidate_sha invalid; then exit 20; fi
  if resolve_candidate_sha too many; then exit 21; fi
  printf "%s\n" ok
' _ "$OPS_SCRIPT" "$candidate_sha")
[ "$resolved" = ok ]
printf '%s\n' 'PASS candidate SHA argument overrides the selected main SHA; the selected SHA is the no-argument default'

CLEANUP_FIXTURE=$(mktemp -d /tmp/main2612-replay.XXXXXXXX)
mkdir -m 700 "$CLEANUP_FIXTURE/live"
printf '%s\n' 0123456789abcdef0123456789abcdef > "$CLEANUP_FIXTURE/.rollout-owned-0123456789abcdef0123456789abcdef"
chmod 600 "$CLEANUP_FIXTURE/.rollout-owned-0123456789abcdef0123456789abcdef"
cleanup_status=0
if cleanup_output=$(bash -c '
  set -Eeuo pipefail
  source "$1"
  TMP=$2
  TMP_TOKEN=0123456789abcdef0123456789abcdef
  RESULT_ROWS=$3
  realpath() { printf "%s\n" "$3"; }
  stat() {
    case "$2" in
      %u) printf "0\n" ;;
      %a) printf "700\n" ;;
      *) return 2 ;;
    esac
  }
  rollout_safe_remove_tree() {
    printf "synthetic ownership marker mismatch\n" >&2
    return 23
  }
  TMP_MOUNTED=1
  unmount_replay_tmpfs() {
    unlink -- "$TMP/.rollout-owned-$TMP_TOKEN"
    rmdir -- "$TMP/live"
    TMP_MOUNTED=0
  }
  cleanup
' _ "$OPS_SCRIPT" "$CLEANUP_FIXTURE" $'field\tresult\ncompose\tpass' 2>&1); then
  printf '%s\n' 'FAIL live replay cleanup fixture accepted a guarded-delete failure' >&2
  exit 1
else
  cleanup_status=$?
fi
[ "$cleanup_status" -eq 1 ]
expected_cleanup_error="live Compose replay cleanup failed for $CLEANUP_FIXTURE/live (exit 23): synthetic ownership marker mismatch"
[ "$cleanup_output" = "$expected_cleanup_error" ] || {
  printf 'FAIL unexpected live replay cleanup diagnostic: %s\n' "$cleanup_output" >&2
  exit 1
}
[ ! -e "$CLEANUP_FIXTURE" ]
CLEANUP_FIXTURE=
printf '%s\n' 'PASS replay withholds passing rows and reports a guarded-delete failure'

SUCCESS_FIXTURE=$(mktemp -d /tmp/main2612-replay.XXXXXXXX)
mkdir -m 700 "$SUCCESS_FIXTURE/live"
mkdir -m 700 "$SUCCESS_FIXTURE/live/evidence"
printf '%s\n' synthetic-owned-evidence > "$SUCCESS_FIXTURE/live/evidence/result.json"
chmod 600 "$SUCCESS_FIXTURE/live/evidence/result.json"
printf '%s\n' "$SUCCESS_TOKEN" > "$SUCCESS_FIXTURE/.rollout-owned-$SUCCESS_TOKEN"
chmod 600 "$SUCCESS_FIXTURE/.rollout-owned-$SUCCESS_TOKEN"
success_rows=$(bash -c '
  set -Eeuo pipefail
  source "$1"
  TMP=$2
  TMP_TOKEN=$3
  RESULT_ROWS=$4
  realpath() { printf "%s\n" "$3"; }
  stat() {
    case "$2" in
      %u) printf "0\n" ;;
      %a)
        if [ "$4" = "$TMP" ] || [ "$4" = "$TMP/live" ]; then printf "700\n"; else printf "600\n"; fi
        ;;
      *) return 2 ;;
    esac
  }
  rollout_safe_remove_tree() {
    [ "$#" -eq 4 ] && [ "$1" = "$TMP/live" ] && [ "$2" = "$TMP" ] && \
      [ "$3" = "$TMP_TOKEN" ] && [ "$4" = "$LIVE_CHECKOUT" ] || return 64
    [ -f "$1/evidence/result.json" ] || return 1
    find "$1" -xdev -depth -delete
  }
  cleanup
' _ "$OPS_SCRIPT" "$SUCCESS_FIXTURE" "$SUCCESS_TOKEN" $'field\tresult\ncompose\tpass')
[ "$success_rows" = $'field\tresult\ncompose\tpass' ]
[ ! -e "$SUCCESS_FIXTURE/live/evidence/result.json" ]
[ ! -e "$SUCCESS_FIXTURE/live" ]
[ ! -e "$SUCCESS_FIXTURE" ]
SUCCESS_FIXTURE=
printf '%s\n' 'PASS replay prints field/result rows after successful cleanup'
