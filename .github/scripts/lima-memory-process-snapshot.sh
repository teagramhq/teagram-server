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

lima_memory_best_cgroup_sample() {
  local history_file="$1"
  awk -F'|' '
    NF == 14 && $1 ~ /^[0-9]+$/ && $2 ~ /^[0-9]+$/ {
      if (!found || $2 >= best_current) {
        found = 1
        best_current = $2
        best_row = $0
      }
    }
    END {
      if (found) print best_row
    }
  ' "$history_file"
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
  local proc_root="$1" server_pid="$2" mapping_json mapping_rows mapping_anon_bytes sampled_at_unix_nano server_anon_bytes
  if [[ -n ${LIMA_MEMORY_MAPPING_ENDPOINT:-} ]]; then
    mapping_json=$(curl --noproxy '*' --fail --connect-timeout 1 --max-time 2 --silent \
      "$LIMA_MEMORY_MAPPING_ENDPOINT") || return 1
    jq -er '
      select(.sample_unix_nano | type == "number" and . > 0) |
      "mapping_sample_unix_nano=\(.sample_unix_nano)",
      "mapping_anon_bytes=\(.total_anonymous_bytes)",
      "server_process_anon_bytes=\(.process_anon_bytes)",
      (.mappings | sort_by(-.rss_bytes) | .[:20][] | "\(.rss_bytes)|\(.anonymous_bytes)|\(.mapping)")
    ' <<<"$mapping_json"
    return
  fi
  mapping_anon_bytes=$(awk '$1 == "Anonymous:" { total += $2 * 1024 } END { print total + 0 }' \
    "$proc_root/$server_pid/smaps") || return 1
  mapping_rows=$(awk '
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
  ' "$proc_root/$server_pid/smaps" | LC_ALL=C sort -t'|' -k1,1nr | sed -n '1,20p') || return 1
  server_anon_bytes=$(awk '$1 == "RssAnon:" { print $2 * 1024; found = 1; exit } END { if (!found) exit 1 }' \
    "$proc_root/$server_pid/status") || return 1
  sampled_at_unix_nano=$(date +%s%N) || return 1
  printf 'mapping_sample_unix_nano=%s\nmapping_anon_bytes=%s\nserver_process_anon_bytes=%s\n' \
    "$sampled_at_unix_nano" "$mapping_anon_bytes" "$server_anon_bytes"
  [[ -z $mapping_rows ]] || printf '%s\n' "$mapping_rows"
}

lima_memory_write_mapping_sample() {
  local proc_root="$1" server_pid="$2" sample_prefix="$3" label="$4" history_file="$5"
  local mapping_tmp mapping_snapshot mapping_timestamp mapping_anon_bytes mapping_server_anon_bytes cgroup_sample
  local cgroup_timestamp cgroup_current anon_bytes file_bytes kernel_bytes
  local sampled_server_pid server_rss_bytes server_anon_bytes worker_count worker_processes attribution incomplete_pids
  local cgroup_memory_peak alignment_delta
  mapping_tmp="$sample_prefix-mappings-$label.tmp"
  mapping_snapshot=$(lima_memory_process_mapping_snapshot "$proc_root" "$server_pid") || return 1
  mapping_timestamp=$(awk -F= '$1 == "mapping_sample_unix_nano" { print $2; exit }' <<<"$mapping_snapshot")
  mapping_anon_bytes=$(awk -F= '$1 == "mapping_anon_bytes" { print $2; exit }' <<<"$mapping_snapshot")
  mapping_server_anon_bytes=$(awk -F= '$1 == "server_process_anon_bytes" { print $2; exit }' <<<"$mapping_snapshot")
  if [[ ! $mapping_timestamp =~ ^[1-9][0-9]*$ || ! $mapping_anon_bytes =~ ^[0-9]+$ ||
    ! $mapping_server_anon_bytes =~ ^[0-9]+$ ]]; then
    return 1
  fi
  cgroup_sample=
  for _ in {1..20}; do
    cgroup_sample=$(lima_memory_nearest_cgroup_sample "$history_file" "$mapping_timestamp") || return 1
    [[ -n $cgroup_sample ]] && break
    sleep 0.005
  done
  if [[ -z $cgroup_sample ]]; then
    return 1
  fi
  IFS='|' read -r cgroup_timestamp cgroup_current anon_bytes file_bytes kernel_bytes _ \
    sampled_server_pid server_rss_bytes server_anon_bytes worker_count worker_processes attribution \
    incomplete_pids cgroup_memory_peak <<<"$cgroup_sample"
  if [[ ! $cgroup_timestamp =~ ^[1-9][0-9]*$ || ! $cgroup_current =~ ^[0-9]+$ ||
    ! $server_anon_bytes =~ ^[0-9]+$ || ! $cgroup_memory_peak =~ ^[0-9]+$ ]]; then
    return 1
  fi
  alignment_delta=$((cgroup_timestamp - mapping_timestamp))
  (( alignment_delta < 0 )) && alignment_delta=$((-alignment_delta))
  {
    printf 'label=%s\n' "$label"
    printf 'mapping_sample_unix_nano=%s\n' "$mapping_timestamp"
    printf 'mapping_anon_bytes=%s\n' "$mapping_anon_bytes"
    printf 'cgroup_sample_unix_nano=%s\n' "$cgroup_timestamp"
    printf 'mapping_cgroup_alignment_delta_ns=%s\n' "$alignment_delta"
    printf 'cgroup_current_bytes=%s\n' "$cgroup_current"
    printf 'cgroup_memory_peak_bytes=%s\n' "$cgroup_memory_peak"
    printf 'memory_stat_anon_bytes=%s\n' "$anon_bytes"
    printf 'memory_stat_file_bytes=%s\n' "$file_bytes"
    printf 'memory_stat_kernel_bytes=%s\n' "$kernel_bytes"
    printf 'server_pid=%s\n' "$sampled_server_pid"
    printf 'server_process_rss_bytes=%s\n' "$server_rss_bytes"
    printf 'server_process_anon_bytes=%s\n' "$mapping_server_anon_bytes"
    printf 'cgroup_server_process_anon_bytes=%s\n' "$server_anon_bytes"
    printf 'photo_worker_process_count=%s\n' "$worker_count"
    printf 'photo_worker_processes=%s\n' "$worker_processes"
    printf 'process_attribution=%s\n' "$attribution"
    printf 'process_incomplete_pids=%s\n' "$incomplete_pids"
    printf '%s\n' "$mapping_snapshot" | sed '1,3d'
  } >"$mapping_tmp" || return 1
  mv -- "$mapping_tmp" "$sample_prefix-mappings-$label"
}

lima_memory_queue_mapping_request() {
  local sample_prefix="$1" label="$2" sample_row="$3"
  local request_file="$sample_prefix-mappings-$label-request"
  [[ -e "$request_file" || -e "$sample_prefix-mappings-$label" ]] && return 0
  lima_memory_write_sample_file "$request_file" "$sample_row"
}

lima_memory_capture_typical_mapping_baseline() {
  local sample_prefix="$1" history_file="$2" websocket_ready_file="$3" mapping_ready_file="$4"
  local websocket_ready_timestamp= sample_row=

  for _ in {1..500}; do
    if [[ -r "$websocket_ready_file" ]]; then
      websocket_ready_timestamp=$(<"$websocket_ready_file") || return 1
      [[ $websocket_ready_timestamp =~ ^[1-9][0-9]*$ ]] && break
    fi
    sleep 0.01
  done
  if [[ ! $websocket_ready_timestamp =~ ^[1-9][0-9]*$ ]]; then
    printf 'mapping diagnostics did not observe WebSocket readiness\n' >&2
    return 1
  fi

  sample_row=
  for _ in {1..500}; do
    if [[ -r "$history_file" ]]; then
      sample_row=$(awk -F'|' -v ready="$websocket_ready_timestamp" \
        'NF == 14 && $1 ~ /^[0-9]+$/ && $1 >= ready { print; exit }' "$history_file") || return 1
      [[ -n $sample_row ]] && break
    fi
    sleep 0.01
  done
  if [[ -z $sample_row ]]; then
    printf 'mapping diagnostics did not record a cgroup/process sample after WebSocket readiness\n' >&2
    return 1
  fi
  lima_memory_queue_mapping_request "$sample_prefix" typical "$sample_row" || return 1
  : >"$mapping_ready_file"
}

lima_memory_sample_mapping_loop() {
  local proc_root="$1" server_pid="$2" sample_prefix="$3" history_file="$4"
  local ready_file="$5" stop_file="$6" error_file="$7" interval_seconds="${8:-0.01}"
  local label request_file done_file pending
  while true; do
    if [[ -e "$ready_file" ]]; then
      for label in typical spike; do
        request_file="$sample_prefix-mappings-$label-request"
        done_file="$sample_prefix-mappings-$label-done"
        if [[ -r "$request_file" && ! -e "$done_file" ]]; then
          if ! lima_memory_write_mapping_sample "$proc_root" "$server_pid" "$sample_prefix" \
            "$label" "$history_file"; then
            rm -f -- "$sample_prefix-mappings-$label.tmp"
            printf 'failed to capture a timestamp-aligned %s mapping sample\n' \
              "$label" >"$error_file"
            return 1
          fi
          : >"$done_file"
        fi
      done
    fi
    if [[ -e "$stop_file" ]]; then
      pending=false
      for label in typical spike; do
        request_file="$sample_prefix-mappings-$label-request"
        done_file="$sample_prefix-mappings-$label-done"
        if [[ -r "$request_file" && ! -e "$done_file" ]]; then
          pending=true
        fi
      done
      [[ $pending == false ]] && return 0
    fi
    sleep "$interval_seconds"
  done
}

lima_memory_mapping_spike_delta_bytes() {
  local mapping_spike_delta_bytes=${LIMA_MEMORY_MAPPING_SPIKE_DELTA_BYTES:-262144}
  if [[ ! $mapping_spike_delta_bytes =~ ^[0-9]+$ ]]; then
    printf 'mapping spike delta must be a nonnegative integer\n' >&2
    return 1
  fi
  printf '%s\n' "$mapping_spike_delta_bytes"
}

lima_memory_mapping_capture_check() {
  local memory_peak_bytes="$1" mapping_typical_bytes="$2" mapping_spike_delta_bytes="$3"
  local mapping_anon_tolerance_bytes="$4" typical_file="$5" spike_file="$6"
  local typical_server_anon spike_server_anon typical_mapping_anon spike_mapping_anon
  local typical_alignment_delta spike_alignment_delta
  local server_anon_delta mapping_anon_delta anon_delta_difference
  if [[ ! $memory_peak_bytes =~ ^[0-9]+$ || ! $mapping_typical_bytes =~ ^[0-9]+$ ||
    ! $mapping_spike_delta_bytes =~ ^[0-9]+$ || ! $mapping_anon_tolerance_bytes =~ ^[0-9]+$ ]]; then
    printf 'mapping capture thresholds must be nonnegative integers\n' >&2
    return 1
  fi
  if (( mapping_anon_tolerance_bytes > 131072 )); then
    mapping_anon_tolerance_bytes=131072
  fi
  if (( memory_peak_bytes >= mapping_typical_bytes )) && [[ ! -s $typical_file ]]; then
    printf 'memory.peak reached %s bytes but no typical mapping sample was captured\n' \
      "$memory_peak_bytes" >&2
    return 1
  fi
  if [[ -s $typical_file ]]; then
    typical_alignment_delta=$(awk -F= '$1 == "mapping_cgroup_alignment_delta_ns" { print $2; exit }' "$typical_file")
    if [[ ! $typical_alignment_delta =~ ^[0-9]+$ ]] || (( typical_alignment_delta > 100000000 )); then
      printf 'typical mapping sample is missing a cgroup pair within 100 ms\n' >&2
      return 1
    fi
  fi
  if (( memory_peak_bytes >= mapping_typical_bytes + mapping_spike_delta_bytes )) &&
    [[ ! -s $spike_file ]]; then
    printf 'memory.peak reached %s bytes but no spike mapping sample was captured\n' \
      "$memory_peak_bytes" >&2
    return 1
  fi
  if (( memory_peak_bytes >= mapping_typical_bytes + mapping_spike_delta_bytes )); then
    spike_alignment_delta=$(awk -F= '$1 == "mapping_cgroup_alignment_delta_ns" { print $2; exit }' "$spike_file")
    if [[ ! $spike_alignment_delta =~ ^[0-9]+$ ]] || (( spike_alignment_delta > 100000000 )); then
      printf 'spike mapping sample is missing a cgroup pair within 100 ms\n' >&2
      return 1
    fi
    typical_server_anon=$(awk -F= '$1 == "server_process_anon_bytes" { print $2; exit }' "$typical_file")
    spike_server_anon=$(awk -F= '$1 == "server_process_anon_bytes" { print $2; exit }' "$spike_file")
    typical_mapping_anon=$(awk -F= '$1 == "mapping_anon_bytes" { print $2; exit }' "$typical_file")
    spike_mapping_anon=$(awk -F= '$1 == "mapping_anon_bytes" { print $2; exit }' "$spike_file")
    if [[ ! $typical_server_anon =~ ^[0-9]+$ || ! $spike_server_anon =~ ^[0-9]+$ ||
      ! $typical_mapping_anon =~ ^[0-9]+$ || ! $spike_mapping_anon =~ ^[0-9]+$ ]]; then
      printf 'typical and spike mapping samples must include server_process_anon_bytes and mapping_anon_bytes\n' >&2
      return 1
    fi
    server_anon_delta=$((spike_server_anon - typical_server_anon))
    if (( server_anon_delta < mapping_spike_delta_bytes )); then
      printf 'mapping spike server anon increase was %s bytes, want at least %s\n' \
        "$server_anon_delta" "$mapping_spike_delta_bytes" >&2
      return 1
    fi
    mapping_anon_delta=$((spike_mapping_anon - typical_mapping_anon))
    anon_delta_difference=$((server_anon_delta - mapping_anon_delta))
    (( anon_delta_difference < 0 )) && anon_delta_difference=$((-anon_delta_difference))
    if (( anon_delta_difference > mapping_anon_tolerance_bytes )); then
      printf 'mapping anon delta was %s bytes for a %s-byte server anon increase (tolerance %s)\n' \
        "$mapping_anon_delta" "$server_anon_delta" "$mapping_anon_tolerance_bytes" >&2
      return 1
    fi
  fi
}

lima_memory_record_cgroup_sample() {
  local cgroup_dir="$1" proc_root="$2" server_pid="$3" sample_prefix="$4"
  local process_snapshot peak_bytes events current_bytes stat_snapshot stat_name stat_value sampled_at_unix_nano
  local stat_anon_bytes=unknown stat_file_bytes=unknown stat_kernel_bytes=unknown
  local observed_current=-1 server_process_pid server_process_rss_bytes server_process_anon_bytes
  local photo_worker_process_count photo_worker_processes attribution incomplete_processes membership_snapshot
  local before_pid after_pid found mapping_typical_bytes mapping_spike_delta_bytes mapping_typical_preexisting
  local mapping_typical_server_anon request_row
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
  printf '%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s\n' \
    "$sampled_at_unix_nano" "$current_bytes" "$stat_anon_bytes" "$stat_file_bytes" "$stat_kernel_bytes" \
    "$current_bytes" "$server_process_pid" "$server_process_rss_bytes" "$server_process_anon_bytes" \
    "$photo_worker_process_count" "$photo_worker_processes" "$attribution" "$incomplete_processes" "$peak_bytes" \
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

  if [[ ${LIMA_MEMORY_CAPTURE_MAPS:-0} == 1 && -e "$sample_prefix-mappings-ready" ]]; then
    mapping_typical_bytes=${LIMA_MEMORY_MAPPING_TYPICAL_BYTES:-31457280}
    mapping_spike_delta_bytes=$(lima_memory_mapping_spike_delta_bytes) || return 1
    mapping_typical_preexisting=false
    if [[ -s "$sample_prefix-mappings-typical" ]]; then
      mapping_typical_preexisting=true
    fi
    if [[ ! -e "$sample_prefix-mappings-typical-request" ]] && (( current_bytes >= mapping_typical_bytes )); then
      request_row=$(tail -n 1 "$sample_prefix-history") || return 1
      lima_memory_queue_mapping_request "$sample_prefix" typical "$request_row" || return 1
    fi
    if [[ $mapping_typical_preexisting == true && ! -e "$sample_prefix-mappings-spike-request" &&
      ! -e "$sample_prefix-mappings-spike" ]]; then
      mapping_typical_server_anon=$(awk -F= '$1 == "server_process_anon_bytes" { print $2; exit }' \
        "$sample_prefix-mappings-typical")
      if [[ $mapping_typical_server_anon =~ ^[0-9]+$ && $server_process_anon_bytes =~ ^[0-9]+$ ]] &&
        (( server_process_anon_bytes >= mapping_typical_server_anon + mapping_spike_delta_bytes )); then
        request_row=$(tail -n 1 "$sample_prefix-history") || return 1
        lima_memory_queue_mapping_request "$sample_prefix" spike "$request_row" || return 1
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
    NF == 14 && $1 ~ /^[0-9]+$/ {
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
