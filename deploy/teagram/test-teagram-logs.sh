#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/teagram-logs.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
cat > "$tmp/bin/logger" <<'LOGGER'
#!/bin/sh
exit 0
LOGGER
chmod 755 "$tmp/bin/logger"

# The forced command has fixed production paths. Rewrite those paths only in
# this test copy so fixtures cannot read or write host logs.
sed \
  -e "s|/root/teagramd-legacy-logs.log|$tmp/legacy.log|g" \
  -e "s|/var/log/messages|$tmp/messages|g" \
  "$script" > "$tmp/teagram-logs.sh"
chmod 755 "$tmp/teagram-logs.sh"
export PATH="$tmp/bin:$PATH"

since=$(date -u -d '15 minutes ago' +%Y-%m-%dT%H:%M:%SZ)
legacy_time=$(date -u -d '10 minutes ago' +%Y-%m-%dT%H:%M:%S.123456789Z)
old_syslog_time=$(date -u -d '2 hours ago' '+%b %d %H:%M:%S')
recent_syslog_time=$(date -u -d '5 minutes ago' '+%b %d %H:%M:%S')
current_syslog_time=$(date -u '+%b %d %H:%M:%S')
cat > "$tmp/legacy.log" <<EOF
# teagramd-container-id=old-container
$legacy_time pre-rollout-line
EOF
cat > "$tmp/messages.1" <<EOF
$old_syslog_time teagram teagramd: too-old-line
EOF
cat > "$tmp/messages.0" <<EOF
$recent_syslog_time teagram teagramd: after-rollout-line
$recent_syslog_time teagram sshd: unrelated teagramd: hidden-line
EOF
cat > "$tmp/messages" <<EOF
$current_syslog_time teagram teagramd: current-line
EOF

output=$(SSH_ORIGINAL_COMMAND="$since" "$tmp/teagram-logs.sh")
[[ "$(printf '%s\n' "$output" | wc -l | tr -d ' ')" == 3 ]]
grep -Fq 'pre-rollout-line' <<< "$output"
grep -Fq 'after-rollout-line' <<< "$output"
grep -Fq 'current-line' <<< "$output"
[[ "$output" != *too-old-line* ]]
[[ "$output" != *hidden-line* ]]
[[ "$(printf '%s\n' "$output" | sed -n '1p')" == *pre-rollout-line* ]]
[[ "$(printf '%s\n' "$output" | sed -n '2p')" == *after-rollout-line* ]]
[[ "$(printf '%s\n' "$output" | sed -n '3p')" == *current-line* ]]

rm -f "$tmp/legacy.log" "$tmp/messages" "$tmp/messages."*
awk -v timestamp="$current_syslog_time" 'BEGIN {
  for (line = 1; line <= 5002; line++) {
    printf "%s teagram teagramd: line-%s\n", timestamp, line
  }
}' > "$tmp/messages"
output=$(SSH_ORIGINAL_COMMAND="$(date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%SZ)" "$tmp/teagram-logs.sh")
[[ "$(printf '%s\n' "$output" | wc -l | tr -d ' ')" == 5000 ]]
[[ "$output" != *line-1$'\n'* ]]
[[ "$output" == *line-3$'\n'* ]]
[[ "$output" == *line-5002 ]]

status=0
SSH_ORIGINAL_COMMAND='docker ps' "$tmp/teagram-logs.sh" >"$tmp/invalid.out" 2>"$tmp/invalid.err" || status=$?
[[ "$status" == 64 ]]
[[ ! -s "$tmp/invalid.out" ]]
grep -Fq 'teagram-logs: send one UTC time' "$tmp/invalid.err"

status=0
SSH_ORIGINAL_COMMAND='2026-02-31T12:00:00Z' "$tmp/teagram-logs.sh" >"$tmp/invalid-date.out" 2>"$tmp/invalid-date.err" || status=$?
[[ "$status" == 64 ]]
grep -Fq 'timestamp is not a valid UTC date' "$tmp/invalid-date.err"

printf '%s\n' 'teagram-logs fixture cases pass'
