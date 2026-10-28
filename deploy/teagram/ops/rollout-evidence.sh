#!/usr/bin/env bash
set -euo pipefail
umask 077

ROOT=/root

if [[ "$#" != 0 ]]; then
  printf '%s\n' 'usage: rollout-evidence.sh' >&2
  exit 64
fi

private_dir() {
  local path=$1
  [[ -d "$path" && ! -L "$path" ]] || return 1
  [[ "$(stat -c '%u:%a' -- "$path")" == 0:700 ]]
}

private_file() {
  local path=$1
  [[ -f "$path" && ! -L "$path" ]] || return 1
  [[ "$(stat -c '%u:%a' -- "$path")" == 0:600 ]]
}

if ! private_dir "$ROOT"; then
  printf '%s\n' 'rollout-evidence: /root is not a root-owned mode-0700 directory' >&2
  exit 1
fi

latest_dir=
latest_stamp=
latest_target_sha=
latest_source_sha=
latest_captured_at=
latest_failed_gate=
for dir in "$ROOT"/main1238-*.target; do
  [[ -e "$dir" || -L "$dir" ]] || continue
  if [[ -L "$dir" || ! -d "$dir" ]]; then
    printf '%s\n' 'rollout-evidence: invalid target evidence directory' >&2
    exit 1
  fi
  if ! private_dir "$dir"; then
    printf '%s\n' 'rollout-evidence: target evidence directory failed ownership or mode checks' >&2
    exit 1
  fi

  name=${dir##*/}
  if [[ "$name" =~ ^main1238-([0-9a-f]{12})-([0-9]{8}T[0-9]{6}Z)\.target$ ]]; then
    short_sha=${BASH_REMATCH[1]}
    stamp=${BASH_REMATCH[2]}
  else
    continue
  fi
  failure_file="$dir/target-failure.txt"
  [[ -e "$failure_file" || -L "$failure_file" ]] || continue
  if ! private_file "$failure_file"; then
    printf '%s\n' 'rollout-evidence: target failure evidence failed ownership or mode checks' >&2
    exit 1
  fi
  marker=
  if ! IFS= read -r marker < "$failure_file"; then
    printf '%s\n' 'rollout-evidence: target failure evidence is unreadable' >&2
    exit 1
  fi
  marker_target_sha=
  marker_source_sha=
  marker_captured_at=
  marker_failed_gate=
  marker_fields=()
  read -r -a marker_fields <<< "$marker"
  for field in "${marker_fields[@]}"; do
    case "$field" in
      captured_at_utc=*) marker_captured_at=${field#*=} ;;
      target_sha=*) marker_target_sha=${field#*=} ;;
      source_sha=*) marker_source_sha=${field#*=} ;;
      reason=*) marker_failed_gate=${field#*=} ;;
    esac
  done
  if ! [[ "$marker_captured_at" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ &&
      "$marker_target_sha" =~ ^[0-9a-f]{40}$ && "$marker_source_sha" =~ ^[0-9a-f]{40}$ &&
      "$marker_failed_gate" =~ ^[a-z0-9_]+$ && "$marker_target_sha" == "$short_sha"* ]]; then
    printf '%s\n' 'rollout-evidence: target failure marker failed validation' >&2
    exit 1
  fi
  if [[ -z "$latest_stamp" || "$stamp" > "$latest_stamp" ]]; then
    latest_dir=$dir
    latest_stamp=$stamp
    latest_target_sha=$marker_target_sha
    latest_source_sha=$marker_source_sha
    latest_captured_at=$marker_captured_at
    latest_failed_gate=$marker_failed_gate
  fi
done

if [[ -z "$latest_dir" ]]; then
  printf '%s\n' 'rollout_evidence=none'
  exit 0
fi

deploy_stamp=${latest_stamp//-/}
deploy_stamp=${deploy_stamp//:/}
latest_log=
latest_log_stamp=
for log in "$ROOT"/rollout-"${latest_target_sha:0:7}"-*.log; do
  [[ -e "$log" || -L "$log" ]] || continue
  if [[ -L "$log" || ! -f "$log" ]]; then
    printf '%s\n' 'rollout-evidence: invalid runner log file' >&2
    exit 1
  fi
  log_name=${log##*/}
  if [[ "$log_name" =~ ^rollout-[0-9a-f]{7}-([0-9]{8}T[0-9]{6}Z)\.log$ ]]; then
    log_stamp=${BASH_REMATCH[1]}
  else
    continue
  fi
  if [[ "$log_stamp" > "$deploy_stamp" ]]; then
    continue
  fi
  if [[ -z "$latest_log_stamp" || "$log_stamp" > "$latest_log_stamp" ]]; then
    latest_log=$log
    latest_log_stamp=$log_stamp
  fi
done

printf 'rollout_failure target_sha=%s source_sha=%s failed_gate=%s\n' \
  "$latest_target_sha" "$latest_source_sha" "$latest_failed_gate"
printf 'evidence_captured_at_utc=%s\n' "$latest_captured_at"

readiness_file="$latest_dir/readiness-target.tsv"
if [[ -e "$readiness_file" || -L "$readiness_file" ]]; then
  if ! private_file "$readiness_file"; then
    printf '%s\n' 'rollout-evidence: target readiness evidence failed ownership or mode checks' >&2
    exit 1
  fi
  printf '%s\n' 'readiness_target_tail_begin'
  readiness_tail=$(tail -n 25 -- "$readiness_file") || {
    printf '%s\n' 'rollout-evidence: could not read target readiness evidence' >&2
    exit 1
  }
  while IFS= read -r line; do
    redacted_line=$(printf '%s\n' "$line" | sed -E \
      's/(container_id=)[0-9a-f]{64}/\1[REDACTED]/g')
    printf '%s\n' "$redacted_line"
  done <<< "$readiness_tail"
  printf '%s\n' 'readiness_target_tail_end'
fi

if [[ -z "$latest_log" ]]; then
  printf '%s\n' 'runner_log=unavailable'
  exit 1
fi
if ! private_file "$latest_log"; then
  printf '%s\n' 'rollout-evidence: runner log failed ownership or mode checks' >&2
  exit 1
fi

printf 'runner_log=%s\n' "${latest_log##*/}"
printf '%s\n' 'runner_log_tail_begin'
tail_output=$(tail -n 80 -- "$latest_log") || {
  printf '%s\n' 'rollout-evidence: could not read runner log tail' >&2
  exit 1
}
while IFS= read -r line; do
  if printf '%s\n' "$line" | grep -Eiq \
    '(password|passwd|passphrase|pwd|token|secret|credential|private[_-]?key|api[_-]?key|access[_-]?key|database[_-]?url|dsn|authorization|[[:alnum:]_]*key"?[[:space:]]*[:=])'; then
    printf '%s\n' '[REDACTED sensitive runner log line]'
    continue
  fi
  redacted_line=$(printf '%s\n' "$line" | sed -E \
    -e 's#(://)[^/@[:space:]]+@#\1[REDACTED]@#g' \
    -e 's#([Bb]earer[[:space:]]+)[^[:space:],;]+#\1[REDACTED]#g')
  printf '%s\n' "$redacted_line"
done <<< "$tail_output"
printf '%s\n' 'runner_log_tail_end'
