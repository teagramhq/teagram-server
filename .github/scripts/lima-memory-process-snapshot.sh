# shellcheck shell=bash

lima_memory_process_snapshot() {
  local proc_root="$1" cgroup_procs_file="$2" server_pid="$3"
  local pid status_file field value name rss_kib anon_kib rss_bytes anon_bytes membership_snapshot
  local server_rss_bytes=unknown server_anon_bytes=unknown worker_count=0 worker_entry
  local server_seen=false
  local -a workers=() members_before=() members_after=() incomplete_pids=()

  if [[ ! -r "$cgroup_procs_file" ]]; then
    printf '%s|unknown|unknown|0|-|incomplete|cgroup.procs\n' "$server_pid"
    return 0
  fi
  mapfile -t members_before <"$cgroup_procs_file"

  for pid in "${members_before[@]}"; do
    if [[ ! "$pid" =~ ^[1-9][0-9]*$ ]]; then
      incomplete_pids+=(unknown)
      continue
    fi
    if [[ "$pid" == "$server_pid" ]]; then
      server_seen=true
    fi
    status_file="$proc_root/$pid/status"
    if [[ ! -r "$status_file" ]]; then
      incomplete_pids+=("$pid")
      continue
    fi

    name=
    rss_kib=
    anon_kib=
    while read -r field value _; do
      case "$field" in
        Name:) name="$value" ;;
        VmRSS:) rss_kib="$value" ;;
        RssAnon:) anon_kib="$value" ;;
      esac
    done <"$status_file"

    rss_bytes=unknown
    anon_bytes=unknown
    if [[ "$rss_kib" =~ ^[0-9]+$ ]]; then
      rss_bytes=$((rss_kib * 1024))
    fi
    if [[ "$anon_kib" =~ ^[0-9]+$ ]]; then
      anon_bytes=$((anon_kib * 1024))
    fi

    if [[ "$pid" == "$server_pid" ]]; then
      if [[ "$rss_bytes" == unknown || "$anon_bytes" == unknown ]]; then
        incomplete_pids+=("$pid")
        continue
      fi
      server_rss_bytes="$rss_bytes"
      server_anon_bytes="$anon_bytes"
    elif [[ "$name" == photothumb ]]; then
      worker_count=$((worker_count + 1))
      worker_entry="$pid:$rss_bytes:$anon_bytes"
      workers+=("$worker_entry")
      if [[ "$rss_bytes" == unknown || "$anon_bytes" == unknown ]]; then
        incomplete_pids+=("$pid")
      fi
    elif [[ -z "$name" ]]; then
      incomplete_pids+=("$pid")
    fi
  done

  if ! mapfile -t members_after <"$cgroup_procs_file"; then
    incomplete_pids+=(cgroup.procs)
  else
    local before_pid after_pid found
    for before_pid in "${members_before[@]}"; do
      found=false
      for after_pid in "${members_after[@]}"; do
        if [[ "$before_pid" == "$after_pid" ]]; then
          found=true
          break
        fi
      done
      if [[ "$found" != true ]]; then
        incomplete_pids+=("$before_pid")
      fi
    done
    for after_pid in "${members_after[@]}"; do
      found=false
      for before_pid in "${members_before[@]}"; do
        if [[ "$after_pid" == "$before_pid" ]]; then
          found=true
          break
        fi
      done
      if [[ "$found" != true ]]; then
        incomplete_pids+=("$after_pid")
      fi
    done
  fi

  if [[ "$server_seen" != true ]]; then
    incomplete_pids+=("$server_pid")
  fi

  local worker_processes=-
  local IFS=,
  if (( ${#workers[@]} > 0 )); then
    worker_processes="${workers[*]}"
  fi
  membership_snapshot="${members_before[*]}"
  local attribution=complete incomplete_processes=-
  if (( ${#incomplete_pids[@]} > 0 )); then
    attribution=incomplete
    incomplete_processes=$(printf '%s\n' "${incomplete_pids[@]}" | LC_ALL=C sort -u | paste -sd, -)
  fi
  printf '%s|%s|%s|%s|%s|%s|%s|%s\n' \
    "$server_pid" "$server_rss_bytes" "$server_anon_bytes" "$worker_count" "$worker_processes" \
    "$attribution" "$incomplete_processes" "$membership_snapshot"
}

lima_memory_process_attribution_check() {
  local attempts="$1" sample_peak_bytes="$2" attribution="$3" incomplete_pid_list="$4"

  case "$attempts" in
    3|10) ;;
    *)
      printf 'unsupported Lima memory probe attempt count: %s\n' "$attempts" >&2
      return 1
      ;;
  esac
  case "$attribution" in
    complete) return 0 ;;
    incomplete) ;;
    *)
      printf 'invalid Lima memory process attribution status: %s\n' "$attribution" >&2
      return 1
      ;;
  esac

  printf 'memory probe sampled incomplete process attribution at %s bytes: pids=%s\n' \
    "$sample_peak_bytes" "$incomplete_pid_list" >&2
  [[ "$attempts" == 3 ]]
}

lima_memory_write_sample_file() {
  local sample_file="$1" sample_value="$2" sample_tmp="$1.tmp"
  printf '%s\n' "$sample_value" >"$sample_tmp" || return 1
  mv -- "$sample_tmp" "$sample_file"
}

lima_memory_configure_mapping_endpoint() {
  LIMA_MEMORY_MAPPING_ENDPOINT=''
  if [[ ${LIMA_MEMORY_CAPTURE_MAPS:-0} == 1 ]]; then
    LIMA_MEMORY_MAPPING_ENDPOINT=http://127.0.0.1:2447/debug/runtime-memory-mappings
  fi
  export LIMA_MEMORY_MAPPING_ENDPOINT
}

lima_memory_process_mapping_snapshot() {
  local proc_root="$1" server_pid="$2" mapping_json
  if [[ -n ${LIMA_MEMORY_MAPPING_ENDPOINT:-} ]]; then
    mapping_json=$(curl --noproxy '*' --fail --connect-timeout 1 --max-time 2 --silent \
      "$LIMA_MEMORY_MAPPING_ENDPOINT") || return 1
    jq -r '"mapping_sample_unix_nano=\(.sample_unix_nano)", (.mappings[] | "\(.rss_bytes)|\(.anonymous_bytes)|\(.mapping)")' <<<"$mapping_json" |
      sed -n '1,21p'
    return
  fi
  awk '
    /^[[:xdigit:]]+-[[:xdigit:]]+[[:space:]]/ {
      if (mapping != "") print rss_bytes "|" anonymous_bytes "|" mapping
      mapping = $0
      rss_bytes = 0
      anonymous_bytes = 0
      next
    }
    $1 == "Rss:" { rss_bytes = $2 * 1024 }
    $1 == "Anonymous:" { anonymous_bytes = $2 * 1024 }
    END {
      if (mapping != "") print rss_bytes "|" anonymous_bytes "|" mapping
    }
  ' "$proc_root/$server_pid/smaps" | LC_ALL=C sort -t'|' -k1,1nr | sed -n '1,20p'
}

lima_memory_write_mapping_sample() {
  local proc_root="$1" server_pid="$2" sample_prefix="$3" label="$4"
  local sampled_at_unix_nano="$5" current_bytes="$6" mapping_tmp mapping_snapshot
  mapping_tmp="$sample_prefix-mappings-$label.tmp"
  mapping_snapshot=$(lima_memory_process_mapping_snapshot "$proc_root" "$server_pid") || return 1
  {
    printf 'cgroup_sample_unix_nano=%s cgroup_current_bytes=%s server_pid=%s\n' \
      "$sampled_at_unix_nano" "$current_bytes" "$server_pid"
    printf '%s\n' "$mapping_snapshot"
  } >"$mapping_tmp" || return 1
  mv -- "$mapping_tmp" "$sample_prefix-mappings-$label"
}

lima_memory_mapping_capture_check() {
  local memory_peak_bytes="$1" mapping_typical_bytes="$2" mapping_spike_delta_bytes="$3"
  local typical_file="$4" spike_file="$5"
  if [[ ! $memory_peak_bytes =~ ^[0-9]+$ || ! $mapping_typical_bytes =~ ^[0-9]+$ ||
    ! $mapping_spike_delta_bytes =~ ^[0-9]+$ ]]; then
    printf 'mapping capture thresholds must be nonnegative integers\n' >&2
    return 1
  fi
  if (( memory_peak_bytes >= mapping_typical_bytes )) && [[ ! -s $typical_file ]]; then
    printf 'memory.peak reached %s bytes but no typical mapping sample was captured\n' \
      "$memory_peak_bytes" >&2
    return 1
  fi
  if (( memory_peak_bytes >= mapping_typical_bytes + mapping_spike_delta_bytes )) &&
    [[ ! -s $spike_file ]]; then
    printf 'memory.peak reached %s bytes but no spike mapping sample was captured\n' \
      "$memory_peak_bytes" >&2
    return 1
  fi
}

lima_memory_record_cgroup_sample() {
  local cgroup_dir="$1" proc_root="$2" server_pid="$3" sample_prefix="$4"
  local process_snapshot peak_bytes events current_bytes stat_snapshot stat_name stat_value sampled_at_unix_nano
  local stat_anon_bytes=unknown stat_file_bytes=unknown stat_kernel_bytes=unknown
  local observed_current=-1 server_process_pid server_process_rss_bytes server_process_anon_bytes
  local photo_worker_process_count photo_worker_processes attribution incomplete_processes membership_snapshot
  local before_pid after_pid found
  local -a members_before=() members_after=() membership_changes=() incomplete_pids=()

  process_snapshot=$(lima_memory_process_snapshot "$proc_root" "$cgroup_dir/cgroup.procs" "$server_pid") || return 1
  peak_bytes=$(<"$cgroup_dir/memory.peak") || return 1
  events=$(<"$cgroup_dir/memory.events") || return 1
  current_bytes=$(<"$cgroup_dir/memory.current") || return 1
  stat_snapshot=$(<"$cgroup_dir/memory.stat") || return 1
  if [[ ! "$peak_bytes" =~ ^[0-9]+$ || ! "$current_bytes" =~ ^[0-9]+$ ]]; then
    return 1
  fi

  while read -r stat_name stat_value; do
    case "$stat_name" in
      anon) stat_anon_bytes="$stat_value" ;;
      file) stat_file_bytes="$stat_value" ;;
      kernel) stat_kernel_bytes="$stat_value" ;;
    esac
  done <<<"$stat_snapshot"
  printf '%s\n' "$peak_bytes" >"$sample_prefix-peak" || return 1
  printf '%s\n' "$events" >"$sample_prefix-events" || return 1
  IFS='|' read -r server_process_pid server_process_rss_bytes server_process_anon_bytes \
    photo_worker_process_count photo_worker_processes attribution incomplete_processes membership_snapshot \
    <<<"$process_snapshot"
  if [[ -n "$membership_snapshot" ]]; then
    IFS=, read -r -a members_before <<<"$membership_snapshot"
  fi
  if ! mapfile -t members_after <"$cgroup_dir/cgroup.procs"; then
    membership_changes+=(cgroup.procs)
  else
    for before_pid in "${members_before[@]}"; do
      found=false
      for after_pid in "${members_after[@]}"; do
        if [[ "$before_pid" == "$after_pid" ]]; then
          found=true
          break
        fi
      done
      if [[ "$found" != true ]]; then
        membership_changes+=("$before_pid")
      fi
    done
    for after_pid in "${members_after[@]}"; do
      found=false
      for before_pid in "${members_before[@]}"; do
        if [[ "$after_pid" == "$before_pid" ]]; then
          found=true
          break
        fi
      done
      if [[ "$found" != true ]]; then
        membership_changes+=("$after_pid")
      fi
    done
  fi
  if (( ${#membership_changes[@]} > 0 )); then
    if [[ "$incomplete_processes" != - ]]; then
      IFS=, read -r -a incomplete_pids <<<"$incomplete_processes"
    fi
    incomplete_pids+=("${membership_changes[@]}")
    attribution=incomplete
    incomplete_processes=$(printf '%s\n' "${incomplete_pids[@]}" | LC_ALL=C sort -u | paste -sd, -)
  fi
  sampled_at_unix_nano=$(date +%s%N) || return 1
  printf '%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s\n' \
    "$sampled_at_unix_nano" "$current_bytes" "$stat_anon_bytes" "$stat_file_bytes" "$stat_kernel_bytes" \
    "$current_bytes" "$server_process_pid" "$server_process_rss_bytes" "$server_process_anon_bytes" \
    "$photo_worker_process_count" "$photo_worker_processes" "$attribution" "$incomplete_processes" \
    >>"$sample_prefix-history" || return 1

  if [[ -r "$sample_prefix-stat-peak" ]]; then
    observed_current=$(<"$sample_prefix-stat-peak") || return 1
    if [[ ! "$observed_current" =~ ^[0-9]+$ ]]; then
      return 1
    fi
  fi
  if (( current_bytes >= observed_current )); then
    printf '%s\n' "$current_bytes" >"$sample_prefix-current" || return 1
    printf '%s\n' "$stat_anon_bytes" >"$sample_prefix-anon" || return 1
    printf '%s\n' "$stat_file_bytes" >"$sample_prefix-file" || return 1
    printf '%s\n' "$stat_kernel_bytes" >"$sample_prefix-kernel" || return 1
    printf '%s|%s|%s|%s|%s|%s|%s|%s\n' \
      "$current_bytes" "$server_process_pid" "$server_process_rss_bytes" "$server_process_anon_bytes" \
      "$photo_worker_process_count" "$photo_worker_processes" "$attribution" "$incomplete_processes" \
      >"$sample_prefix-processes" || return 1
    lima_memory_write_sample_file "$sample_prefix-stat-peak" "$current_bytes" || return 1
  fi

  if [[ ${LIMA_MEMORY_CAPTURE_MAPS:-0} == 1 ]]; then
    local mapping_typical_bytes=${LIMA_MEMORY_MAPPING_TYPICAL_BYTES:-31457280}
    local mapping_spike_delta_bytes=${LIMA_MEMORY_MAPPING_SPIKE_DELTA_BYTES:-524288}
    local mapping_spike_current=0
    local mapping_typical_preexisting=false
    if [[ -s "$sample_prefix-mappings-typical" ]]; then
      mapping_typical_preexisting=true
    fi
    if [[ -r "$sample_prefix-mappings-spike-current" ]]; then
      mapping_spike_current=$(<"$sample_prefix-mappings-spike-current") || return 1
      [[ $mapping_spike_current =~ ^[0-9]+$ ]] || return 1
    fi
    if [[ ! -e "$sample_prefix-mappings-typical" ]] && (( current_bytes >= mapping_typical_bytes )); then
      if lima_memory_write_mapping_sample "$proc_root" "$server_pid" "$sample_prefix" typical \
        "$sampled_at_unix_nano" "$current_bytes"; then
        :
      else
        rm -f -- "$sample_prefix-mappings-typical.tmp"
      fi
    fi
    if [[ $mapping_typical_preexisting == true ]] && (( current_bytes >= mapping_typical_bytes + mapping_spike_delta_bytes &&
      ( mapping_spike_current == 0 || current_bytes >= mapping_spike_current + 1048576 ) )); then
      if lima_memory_write_mapping_sample "$proc_root" "$server_pid" "$sample_prefix" spike \
        "$sampled_at_unix_nano" "$current_bytes"; then
        lima_memory_write_sample_file "$sample_prefix-mappings-spike-current" "$current_bytes" || return 1
      else
        rm -f -- "$sample_prefix-mappings-spike.tmp"
      fi
    fi
  fi
}

lima_memory_sample_cgroup_loop() {
  local cgroup_dir="$1" proc_root="$2" server_pid="$3" sample_prefix="$4"
  local stop_file="$5" error_file="$6" interval_seconds="${7:-0.01}"

  while [[ ! -e "$stop_file" && -r "$cgroup_dir/memory.peak" && -r "$cgroup_dir/memory.events" ]]; do
    if ! lima_memory_record_cgroup_sample "$cgroup_dir" "$proc_root" "$server_pid" "$sample_prefix"; then
      printf 'failed to capture a complete cgroup sample\n' >"$error_file"
      return 1
    fi
    sleep "$interval_seconds"
  done
}

lima_memory_nearest_cgroup_sample() {
  local history_file="$1" target_timestamp="$2" maximum_delta_ns="${3:-100000000}"
  awk -F'|' -v target="$target_timestamp" -v maximum_delta="$maximum_delta_ns" '
    NF == 13 && $1 ~ /^[0-9]+$/ {
      delta = $1 - target
      if (delta < 0) delta = -delta
      if (!found || delta < best_delta) {
        found = 1
        best_delta = delta
        best_row = $0
      }
    }
    END {
      if (found && best_delta <= maximum_delta) print best_row
    }
  ' "$history_file"
}

lima_memory_sample_runtime_metrics_loop() {
  local sampled_peak_file="$1" sample_history_file="$2" sample_interval_bytes="$3" ready_file="$4"
  local runtime_file="$5" error_file="$6" stop_file="$7"
  local interval_seconds="${8:-0.01}" last_runtime_sample_current=0 best_runtime_sample_current=0
  local sampled_current runtime_sample_timestamp sample_row sample_timestamp sample_current runtime_sample_tmp runtime_bundle_tmp
  local has_runtime_sample=false

  while true; do
    if [[ ! -e "$ready_file" ]]; then
      if [[ -e "$stop_file" ]]; then
        return 0
      fi
      sleep "$interval_seconds"
      continue
    fi
    if [[ ! -r "$sampled_peak_file" ]] ||
      ! sampled_current=$(<"$sampled_peak_file") || [[ ! "$sampled_current" =~ ^[0-9]+$ ]]; then
      if [[ -e "$stop_file" ]]; then
        printf 'failed to read sampled cgroup current peak before Go runtime memory metrics request\n' >"$error_file"
        return 1
      fi
      sleep "$interval_seconds"
      continue
    fi
    if (( sampled_current >= last_runtime_sample_current + sample_interval_bytes )) ||
      [[ -e "$stop_file" && $sampled_current != "$last_runtime_sample_current" ]]; then
      runtime_sample_tmp="$runtime_file.tmp"
      if curl --noproxy '*' --fail --connect-timeout 1 --max-time 2 --silent --show-error \
        --output "$runtime_sample_tmp" http://127.0.0.1:2447/debug/runtime-memory &&
        [[ -s "$runtime_sample_tmp" ]]; then
        if ! runtime_sample_timestamp=$(jq -er '.sample_unix_nano | select(type == "number" and . > 0)' \
          "$runtime_sample_tmp") || [[ ! "$runtime_sample_timestamp" =~ ^[0-9]+$ ]]; then
          rm -f -- "$runtime_sample_tmp"
          printf 'Go runtime memory metrics did not include a valid sample timestamp\n' >"$error_file"
          return 1
        fi
        sample_row=
        for _ in {1..20}; do
          sample_row=$(lima_memory_nearest_cgroup_sample "$sample_history_file" "$runtime_sample_timestamp")
          if [[ -n "$sample_row" ]]; then
            break
          fi
          sleep 0.005
        done
        if [[ -z "$sample_row" ]]; then
          rm -f -- "$runtime_sample_tmp"
          printf 'no timestamped cgroup/process sample within 100 ms of Go runtime sample %s\n' \
            "$runtime_sample_timestamp" >"$error_file"
          return 1
        fi
        IFS='|' read -r sample_timestamp sample_current _ <<<"$sample_row"
        if [[ ! "$sample_current" =~ ^[0-9]+$ || ! "$sample_timestamp" =~ ^[0-9]+$ ]]; then
          rm -f -- "$runtime_sample_tmp"
          printf 'nearest cgroup/process sample has invalid timestamp or current\n' >"$error_file"
          return 1
        fi
        if [[ "$has_runtime_sample" != true ]] || (( sample_current > best_runtime_sample_current )); then
          runtime_bundle_tmp="$runtime_file.bundle.tmp"
          if ! jq -c --arg cgroup_sample "$sample_row" \
            '{runtime: ., cgroup_sample: $cgroup_sample}' "$runtime_sample_tmp" >"$runtime_bundle_tmp"; then
            rm -f -- "$runtime_sample_tmp"
            rm -f -- "$runtime_bundle_tmp"
            printf 'failed to pair Go runtime metrics with the timestamped cgroup/process sample\n' >"$error_file"
            return 1
          fi
          if ! mv -- "$runtime_bundle_tmp" "$runtime_file"; then
            rm -f -- "$runtime_sample_tmp" "$runtime_bundle_tmp"
            printf 'failed to persist paired Go runtime and cgroup/process metrics\n' >"$error_file"
            return 1
          fi
          rm -f -- "$runtime_sample_tmp"
          best_runtime_sample_current=$sample_current
          has_runtime_sample=true
        else
          rm -f -- "$runtime_sample_tmp"
        fi
        last_runtime_sample_current=$sampled_current
      else
        rm -f -- "$runtime_sample_tmp"
        printf 'failed to capture Go runtime memory metrics at %s cgroup bytes\n' \
          "$sampled_current" >"$error_file"
        return 1
      fi
    fi
    if [[ -e "$stop_file" ]]; then
      if [[ "$sampled_current" == "$last_runtime_sample_current" ]]; then
        return 0
      fi
    fi
    sleep "$interval_seconds"
  done
}

lima_memory_peak_unattributed_bytes() {
  local peak_bytes="$1" sampled_current_bytes="$2"
  if [[ ! "$peak_bytes" =~ ^[0-9]+$ || ! "$sampled_current_bytes" =~ ^[0-9]+$ ]] ||
    (( peak_bytes < sampled_current_bytes )); then
    printf 'invalid memory peak/sample values: peak=%s sample=%s\n' \
      "$peak_bytes" "$sampled_current_bytes" >&2
    return 1
  fi
  printf '%s\n' "$((peak_bytes - sampled_current_bytes))"
}
