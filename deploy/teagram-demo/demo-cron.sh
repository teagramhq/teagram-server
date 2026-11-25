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

demo_cron_read_is_absent() {
  local error_file=$1 message
  message=$(<"$error_file")
  case "$message" in
    'no crontab for root'|\
    "crontab: can't open 'root': No such file or directory"|\
    "crontab: can't open '/var/spool/cron/crontabs/root': No such file or directory"|\
    "crontab: can't open '/var/spool/cron/root': No such file or directory")
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

demo_cron_read_current() {
  local allow_absent=$1 tmp_dir
  tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/teagram-demo-crontab.XXXXXXXX") || {
    demo_cron_fail 'could not prepare a private temporary directory to read the root crontab'
    return 1
  }

  if crontab -l >"$tmp_dir/current" 2>"$tmp_dir/error"; then
    if ! cat "$tmp_dir/current"; then
      rm -rf -- "$tmp_dir"
      demo_cron_fail 'could not read the root crontab output'
      return 1
    fi
  elif [[ "$allow_absent" == true ]] && demo_cron_read_is_absent "$tmp_dir/error"; then
    :
  else
    if ! cat "$tmp_dir/error" >&2; then
      if ! rm -rf -- "$tmp_dir"; then
        demo_cron_fail 'could not remove the temporary root crontab read files'
        return 1
      fi
      demo_cron_fail 'could not report the root crontab read error'
      return 1
    fi
    if ! rm -rf -- "$tmp_dir"; then
      demo_cron_fail 'could not remove the temporary root crontab read files'
      return 1
    fi
    demo_cron_fail 'could not read the root crontab; leaving it unchanged'
    return 1
  fi

  if ! rm -rf -- "$tmp_dir"; then
    demo_cron_fail 'could not remove the temporary root crontab read files'
    return 1
  fi
  return 0
}

demo_cron_install_entry() {
  local tag=$1 schedule=$2 script=$3 log=$4
  local marker="# teagram-demo:$tag"
  local job="cd $DEMO_CRON_REPO && env PATH=$DEMO_CRON_PATH $script >>$log 2>&1 $marker"
  local existing kept verified marker_count

  existing=$(demo_cron_read_current true) || return 1
  kept=$(printf '%s\n' "$existing" | grep -Fv -- "$marker") || true
  {
    [[ -z "$kept" ]] || printf '%s\n' "$kept"
    printf '%s %s\n' "$schedule" "$job"
  } | crontab - || { demo_cron_fail "could not write the root crontab for $tag"; return 1; }

  verified=$(demo_cron_read_current false) || return 1
  marker_count=$(printf '%s\n' "$verified" | grep -Fc -- "$marker") || marker_count=0
  [[ "$marker_count" == 1 ]] ||
    { demo_cron_fail "root crontab does not hold exactly one $tag entry"; return 1; }
}
