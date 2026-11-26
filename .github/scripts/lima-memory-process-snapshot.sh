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

lima_memory_record_cgroup_sample() {
  local cgroup_dir="$1" proc_root="$2" server_pid="$3" sample_prefix="$4"
  local process_snapshot peak_bytes events current_bytes stat_snapshot stat_name stat_value
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
  if [[ -r "$sample_prefix-stat-peak" ]]; then
    observed_current=$(<"$sample_prefix-stat-peak") || return 1
    if [[ ! "$observed_current" =~ ^[0-9]+$ ]]; then
      return 1
    fi
  fi
  if (( current_bytes >= observed_current )); then
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
    printf '%s\n' "$current_bytes" >"$sample_prefix-stat-peak" || return 1
    printf '%s\n' "$current_bytes" >"$sample_prefix-current" || return 1
    printf '%s\n' "$stat_anon_bytes" >"$sample_prefix-anon" || return 1
    printf '%s\n' "$stat_file_bytes" >"$sample_prefix-file" || return 1
    printf '%s\n' "$stat_kernel_bytes" >"$sample_prefix-kernel" || return 1
    printf '%s|%s|%s|%s|%s|%s|%s|%s\n' \
      "$current_bytes" "$server_process_pid" "$server_process_rss_bytes" "$server_process_anon_bytes" \
      "$photo_worker_process_count" "$photo_worker_processes" "$attribution" "$incomplete_processes" \
      >"$sample_prefix-processes" || return 1
  fi
}
