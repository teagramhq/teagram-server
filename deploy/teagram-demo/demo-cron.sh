#!/usr/bin/env bash
# Root crontab helpers for the demo installers. Sourced only.
#
# Each job is one crontab line tagged with a marker comment. Installing a job
# replaces only its own line, so reinstalling keeps exactly one entry and the
# rest of the root crontab is left as it was.

DEMO_CRON_REPO=/opt/teagram-demo
DEMO_CRON_PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

demo_cron_fail() {
  printf 'teagram-demo: %s\n' "$1" >&2
  return 1
}

demo_cron_require_daemon() {
  command -v crontab >/dev/null 2>&1 || { demo_cron_fail 'crontab is not installed'; return 1; }
  command -v pgrep >/dev/null 2>&1 || { demo_cron_fail 'pgrep is required to check the cron daemon'; return 1; }
  if pgrep -x crond >/dev/null 2>&1 || pgrep -x cron >/dev/null 2>&1; then
    return 0
  fi
  demo_cron_fail 'the cron daemon (crond) is not running; start it and rerun the installer'
}

demo_cron_require_utc() {
  [[ "$(date +%z)" == +0000 ]] || demo_cron_fail 'the host clock must be UTC for the demo cron schedule'
}

demo_cron_install_entry() {
  local tag=$1 schedule=$2 script=$3 log=$4
  local marker="# teagram-demo:$tag"
  local job="cd $DEMO_CRON_REPO && env PATH=$DEMO_CRON_PATH $script >>$log 2>&1 $marker"
  local existing kept

  existing=$(crontab -l 2>/dev/null) || existing=''
  kept=$(printf '%s\n' "$existing" | grep -Fv -- "$marker") || true
  {
    [[ -z "$kept" ]] || printf '%s\n' "$kept"
    printf '%s %s\n' "$schedule" "$job"
  } | crontab - || { demo_cron_fail "could not write the root crontab for $tag"; return 1; }
  [[ "$(crontab -l 2>/dev/null | grep -Fc -- "$marker")" == 1 ]] ||
    { demo_cron_fail "root crontab does not hold exactly one $tag entry"; return 1; }
}
