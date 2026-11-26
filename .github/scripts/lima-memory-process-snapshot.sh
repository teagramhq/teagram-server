# shellcheck shell=bash

lima_memory_process_snapshot() {
  local proc_root="$1" cgroup_procs_file="$2" server_pid="$3"
  local pid status_file field value name rss_kib anon_kib rss_bytes anon_bytes
  local server_rss_bytes=unknown server_anon_bytes=unknown worker_count=0 worker_entry
  local -a workers=()

  while IFS= read -r pid; do
    [[ "$pid" =~ ^[1-9][0-9]*$ ]] || continue
    status_file="$proc_root/$pid/status"
    [[ -r "$status_file" ]] || continue

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
        return 1
      fi
      server_rss_bytes="$rss_bytes"
      server_anon_bytes="$anon_bytes"
    elif [[ "$name" == photothumb ]]; then
      if [[ "$rss_bytes" == unknown || "$anon_bytes" == unknown ]]; then
        return 1
      fi
      worker_count=$((worker_count + 1))
      worker_entry="$pid:$rss_bytes:$anon_bytes"
      workers+=("$worker_entry")
    fi
  done <"$cgroup_procs_file"

  if [[ "$server_rss_bytes" == unknown || "$server_anon_bytes" == unknown ]]; then
    return 1
  fi

  local worker_processes=-
  if (( ${#workers[@]} > 0 )); then
    local IFS=,
    worker_processes="${workers[*]}"
  fi
  printf '%s|%s|%s|%s|%s\n' "$server_pid" "$server_rss_bytes" "$server_anon_bytes" "$worker_count" "$worker_processes"
}
