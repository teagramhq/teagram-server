#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

report_fail() {
  printf 'target failure report: %s\n' "$1" >&2
  return 1
}

read_private_file() {
  local path=$1 expected_uid=$2 owner_mode
  [ -f "$path" ] && [ ! -L "$path" ] || { report_fail 'evidence file is missing or not regular'; return 1; }
  owner_mode=$(stat -c '%u:%a' -- "$path") || { report_fail 'cannot inspect evidence file'; return 1; }
  [ "$owner_mode" = "$expected_uid:600" ] || { report_fail 'evidence file is not owner-only mode 0600'; return 1; }
  cat -- "$path"
}

read_optional_private_file() {
  local path=$1 expected_uid=$2
  if [ -e "$path" ] || [ -L "$path" ]; then
    read_private_file "$path" "$expected_uid"
  else
    return 2
  fi
}

report_readiness() {
  local target_dir=$1 expected_uid=$2 result_path sample_path result contents token key value
  local result_role= result_state= result_exit= result_max= result_elapsed=
  result_path=$target_dir/readiness-target.result
  if contents=$(read_optional_private_file "$result_path" "$expected_uid"); then
    for token in $contents; do
      case "$token" in *=*) key=${token%%=*}; value=${token#*=} ;; *) report_fail 'readiness result is malformed'; return 1 ;; esac
      case "$key" in
        role) result_role=$value ;;
        result) result_state=$value ;;
        exit) result_exit=$value ;;
        max_seconds) result_max=$value ;;
        elapsed) result_elapsed=$value ;;
        *) report_fail 'readiness result has an unexpected field'; return 1 ;;
      esac
    done
    [ "$result_role" = target ] || { report_fail 'readiness result role is invalid'; return 1; }
    [[ "$result_state" =~ ^(ready|failed|bounded_timeout|timeout)$ ]] || { report_fail 'readiness result is invalid'; return 1; }
    [[ -z "$result_exit" || "$result_exit" =~ ^[0-9]+$ ]] || { report_fail 'readiness exit is invalid'; return 1; }
    [[ -z "$result_max" || "$result_max" =~ ^[0-9]+$ ]] || { report_fail 'readiness bound is invalid'; return 1; }
    [[ -z "$result_elapsed" || "$result_elapsed" =~ ^[0-9]+$ ]] || { report_fail 'readiness elapsed time is invalid'; return 1; }
    printf 'readiness_result=%s\n' "$result_state"
    [ -z "$result_exit" ] || printf 'readiness_exit=%s\n' "$result_exit"
    [ -z "$result_max" ] || printf 'readiness_max_seconds=%s\n' "$result_max"
    [ -z "$result_elapsed" ] || printf 'readiness_elapsed=%s\n' "$result_elapsed"
  else
    result=$?
    [ "$result" -eq 2 ] || return 1
    printf '%s\n' 'readiness_result=unavailable'
  fi

  sample_path=$target_dir/readiness-target.tsv
  if contents=$(read_optional_private_file "$sample_path" "$expected_uid"); then
    local last_line
    last_line=${contents##*$'\n'}
    local -a columns=()
    IFS=$'\t' read -r -a columns <<< "$last_line"
    [ "${#columns[@]}" -eq 14 ] || { report_fail 'readiness sample is malformed'; return 1; }
    [[ "${columns[0]}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || {
      report_fail 'readiness sample timestamp is invalid'
      return 1
    }
    printf 'readiness_sample_at=%s\n' "${columns[0]}"
    local field
    for field in "${columns[@]:1}"; do
      case "$field" in *=*) key=${field%%=*}; value=${field#*=} ;; *) report_fail 'readiness sample field is malformed'; return 1 ;; esac
      case "$key" in
        role) [ "$value" = target ] || { report_fail 'readiness sample role is invalid'; return 1; } ;;
        elapsed) [[ "$value" =~ ^[0-9]+$ ]] || { report_fail 'readiness sample elapsed time is invalid'; return 1; } ;;
        container_id) ;;
        state|exit|advertise|errors|migrate|postgres|tcp|tls|tls_http|tls_verify)
          [[ "$value" =~ ^[A-Za-z0-9._:/+-]+$ ]] || { report_fail 'readiness sample value is invalid'; return 1; }
          printf 'readiness_%s=%s\n' "$key" "$value"
          ;;
        *) report_fail 'readiness sample has an unexpected field'; return 1 ;;
      esac
    done
  else
    result=$?
    [ "$result" -eq 2 ] || return 1
    printf '%s\n' 'readiness_sample=unavailable'
  fi
}

report_schema_gate() {
  local target_dir=$1 expected_uid=$2 mode=$3 path contents line key value result
  path=$target_dir/schema-result-gate-$mode.tsv
  if contents=$(read_optional_private_file "$path" "$expected_uid"); then
    :
  else
    result=$?
    [ "$result" -eq 2 ] || return 1
    printf 'schema_%s=unavailable\n' "$mode"
    return 0
  fi
  while IFS= read -r line; do
    case "$line" in *=*) key=${line%%=*}; value=${line#*=} ;; *) report_fail 'schema gate result is malformed'; return 1 ;; esac
    case "$key" in
      gate_result)
        [[ "$value" =~ ^(pass|reject)$ ]] || { report_fail 'schema gate outcome is invalid'; return 1; }
        printf 'schema_%s_gate_result=%s\n' "$mode" "$value"
        ;;
      failed_checks)
        [[ "$value" =~ ^(none|[A-Za-z0-9_.+-]+(,[A-Za-z0-9_.+-]+)*)$ ]] || {
          report_fail 'schema gate failure list is invalid'
          return 1
        }
        printf 'schema_%s_failed_checks=%s\n' "$mode" "$value"
        ;;
      database_revision_query|revision_set)
        [[ "$value" =~ ^[A-Za-z0-9_.+-]+$ ]] || { report_fail 'schema gate status is invalid'; return 1; }
        printf 'schema_%s_%s=%s\n' "$mode" "$key" "$value"
        ;;
      expected_migration_count|actual_revision_count)
        [[ "$value" =~ ^[0-9]+$ ]] || { report_fail 'schema gate count is invalid'; return 1; }
        printf 'schema_%s_%s=%s\n' "$mode" "$key" "$value"
        ;;
      mode) [ "$value" = "$mode" ] || { report_fail 'schema gate mode is invalid'; return 1; } ;;
      *) ;;
    esac
  done <<< "$contents"
}

report_target_failure() {
  local evidence_root=$1 prefix=$2 expected_uid=$3 owner_mode target_dir marker token key value candidate dir_component
  [[ "$prefix" =~ ^[0-9a-f]{7,40}$ ]] || { report_fail 'target SHA prefix must be 7 to 40 lowercase hex characters'; return 64; }
  [ -d "$evidence_root" ] && [ ! -L "$evidence_root" ] || { report_fail 'evidence root is missing or symlinked'; return 1; }
  owner_mode=$(stat -c '%u:%a' -- "$evidence_root") || { report_fail 'cannot inspect evidence root'; return 1; }
  [ "$owner_mode" = "$expected_uid:700" ] || { report_fail 'evidence root is not owner-only mode 0700'; return 1; }

  local -a matches=()
  shopt -s nullglob
  for candidate in "$evidence_root"/main1238-*.target; do
    dir_component=${candidate##*/main1238-}
    dir_component=${dir_component%%-*}
    if [[ "$dir_component" == "$prefix"* || "$prefix" == "$dir_component"* ]]; then
      matches+=("$candidate")
    fi
  done
  shopt -u nullglob
  [ "${#matches[@]}" -eq 1 ] || { report_fail 'target evidence must match exactly one report directory'; return 1; }
  target_dir=${matches[0]}
  dir_component=${target_dir##*/main1238-}
  dir_component=${dir_component%%-*}
  [ -d "$target_dir" ] && [ ! -L "$target_dir" ] || { report_fail 'target evidence is not a regular directory'; return 1; }
  owner_mode=$(stat -c '%u:%a' -- "$target_dir") || { report_fail 'cannot inspect target evidence directory'; return 1; }
  [ "$owner_mode" = "$expected_uid:700" ] || { report_fail 'target evidence directory is not owner-only mode 0700'; return 1; }

  marker=$(read_private_file "$target_dir/target-failure.txt" "$expected_uid") || return 1
  declare -A marker_fields=()
  for token in $marker; do
    case "$token" in *=*) key=${token%%=*}; value=${token#*=} ;; *) report_fail 'failure marker is malformed'; return 1 ;; esac
    case "$key" in captured_at_utc|target_sha|source_sha|reason|target_snapshot) ;; *) report_fail 'failure marker has an unexpected field'; return 1 ;; esac
    [[ -z "${marker_fields[$key]+present}" ]] || { report_fail 'failure marker contains a duplicate field'; return 1; }
    marker_fields[$key]=$value
  done
  for key in captured_at_utc target_sha source_sha reason target_snapshot; do
    [ -n "${marker_fields[$key]:-}" ] || { report_fail 'failure marker is incomplete'; return 1; }
  done
  [[ "${marker_fields[captured_at_utc]}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || {
    report_fail 'failure marker timestamp is invalid'
    return 1
  }
  [[ "${marker_fields[target_sha]}" =~ ^[0-9a-f]{40}$ && "${marker_fields[source_sha]}" =~ ^[0-9a-f]{40}$ ]] || {
    report_fail 'failure marker commit IDs are invalid'
    return 1
  }
  [[ "${marker_fields[target_sha]}" == "$prefix"* ]] || { report_fail 'failure marker does not match the requested target'; return 1; }
  [[ "${marker_fields[target_sha]}" == "$dir_component"* ]] || { report_fail 'failure marker does not match its directory'; return 1; }
  [[ "${marker_fields[reason]}" =~ ^[a-z0-9_]+$ ]] || { report_fail 'failure marker gate is invalid'; return 1; }
  [[ "${marker_fields[target_snapshot]}" == unavailable || "${marker_fields[target_snapshot]}" =~ ^[0-9a-f]{64}$ ]] || {
    report_fail 'failure marker snapshot digest is invalid'
    return 1
  }

  printf 'target_sha=%s\nsource_sha=%s\ncaptured_at_utc=%s\ntarget_failure_gate=%s\n' \
    "${marker_fields[target_sha]}" "${marker_fields[source_sha]}" \
    "${marker_fields[captured_at_utc]}" "${marker_fields[reason]}"
  report_readiness "$target_dir" "$expected_uid" || return 1
  report_schema_gate "$target_dir" "$expected_uid" pre || return 1
  report_schema_gate "$target_dir" "$expected_uid" post || return 1
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  [ "$#" -eq 1 ] || { printf '%s\n' 'usage: target-failure-report <target-sha-prefix>' >&2; exit 64; }
  report_target_failure /root "$1" "$(id -u)"
fi
