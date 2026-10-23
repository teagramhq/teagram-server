#!/bin/bash
# Read-only teagramd log access (forced command of the teagram-qa-logs key).
# The client sends one UTC timestamp; this prints at most the last 5000 lines.
set -euo pipefail
T=${SSH_ORIGINAL_COMMAND:-}
case "$T" in
  [0-9][0-9][0-9][0-9]-[0-1][0-9]-[0-3][0-9]T[0-2][0-9]:[0-5][0-9]:[0-5][0-9]Z) ;;
  *) logger -t teagram-logs "refused invalid request client=${SSH_CLIENT:-}"; echo "teagram-logs: send one UTC time like 2026-10-17T00:00:00Z" >&2; exit 64 ;;
esac
since_epoch=$(date -u -d "$T" +%s 2>/dev/null) || {
  echo 'teagram-logs: timestamp is not a valid UTC date' >&2
  exit 64
}
current_year=$(date -u +%Y)
now_epoch=$(date -u +%s)
logger -t teagram-logs "since=$T client=${SSH_CLIENT:-}"

{
  if [ -f /root/teagramd-legacy-logs.log ]; then
    cat /root/teagramd-legacy-logs.log
  fi
  rotation=99
  while [ "$rotation" -ge 0 ]; do
    if [ -f "/var/log/messages.$rotation" ]; then
      cat "/var/log/messages.$rotation"
    fi
    rotation=$((rotation - 1))
  done
  if [ -f /var/log/messages ]; then
    cat /var/log/messages
  fi
} | TZ=UTC awk -v since="$since_epoch" -v year="$current_year" -v now="$now_epoch" '
  BEGIN {
    split("Jan Feb Mar Apr May Jun Jul Aug Sep Oct Nov Dec", month_names, " ")
    for (i = 1; i <= 12; i++) month_number[month_names[i]] = i
  }
  function timestamp(y, mon, day, clock, fields) {
    split(clock, fields, ":")
    return mktime(sprintf("%04d %02d %02d %02d %02d %02d", y, mon, day, fields[1], fields[2], fields[3]))
  }
  function keep(epoch, line) {
    if (epoch >= since) print line
  }
  {
    # Legacy json-file logs are snapshotted by the rollout runner with their
    # Docker RFC3339 timestamps intact.
    if (substr($0, 5, 1) == "-" && substr($0, 8, 1) == "-" && substr($0, 11, 1) == "T") {
      date_part = substr($0, 1, 10)
      clock_part = substr($0, 12, 8)
      split(date_part, date_fields, "-")
      epoch = timestamp(date_fields[1], date_fields[2], date_fields[3], clock_part)
      keep(epoch, $0)
      next
    }

    # BusyBox syslog writes RFC3164 timestamps without a year. The host is UTC;
    # retained files can cross New Year, so treat timestamps far in the future
    # as belonging to the previous year.
    if (!($1 in month_number) || $2 !~ /^[0-9]+$/ || $3 !~ /^[0-9][0-9]:[0-9][0-9]:[0-9][0-9]$/) next
    tag_found = 0
    for (field = 4; field <= 5 && field <= NF; field++) {
      tag = $field
      sub(/:$/, "", tag)
      sub(/\[[^]]*\]$/, "", tag)
      if (tag == "teagramd") {
        tag_found = 1
        break
      }
    }
    if (!tag_found) next
    epoch = timestamp(year, month_number[$1], $2, $3)
    if (epoch > now + 86400) epoch = timestamp(year - 1, month_number[$1], $2, $3)
    keep(epoch, $0)
  }
' | tail -n 5000
