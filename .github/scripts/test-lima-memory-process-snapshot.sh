#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=.github/scripts/lima-memory-process-snapshot.sh
source "$script_dir/lima-memory-process-snapshot.sh"

fixture="$(mktemp -d)"
runtime_request_release=
runtime_final_request_release=
runtime_final_request_started=
runtime_cgroup_stop=
runtime_stop=
sample_peak_writer_pid=
runtime_sampler_pid=
cgroup_sampler_pid=
cleanup_fixture() {
  local status=$?
  trap - EXIT
  [[ -n ${runtime_request_release:-} ]] && : >"$runtime_request_release" 2>/dev/null || true
  [[ -n ${runtime_final_request_release:-} ]] && : >"$runtime_final_request_release" 2>/dev/null || true
  [[ -n ${runtime_cgroup_stop:-} ]] && : >"$runtime_cgroup_stop" 2>/dev/null || true
  [[ -n ${runtime_stop:-} ]] && : >"$runtime_stop" 2>/dev/null || true
  for pid in "${sample_peak_writer_pid:-}" "${runtime_sampler_pid:-}" "${cgroup_sampler_pid:-}"; do
    if [[ -n $pid ]]; then
      wait "$pid" 2>/dev/null || true
    fi
  done
  rm -rf -- "$fixture"
  exit "$status"
}
trap cleanup_fixture EXIT
proc_root="$fixture/proc"
mkdir -p "$proc_root/101" "$proc_root/102" "$proc_root/103" "$proc_root/104"

cat >"$fixture/cgroup.procs" <<'EOF'
101
102
103
EOF
cat >"$proc_root/101/status" <<'EOF'
Name:   teagramd
VmRSS:  100 kB
RssAnon: 80 kB
EOF
cat >"$proc_root/102/status" <<'EOF'
Name:   photothumb
VmRSS:  70 kB
RssAnon: 60 kB
EOF
cat >"$proc_root/103/status" <<'EOF'
Name:   photothumb
VmRSS:  75 kB
RssAnon: 65 kB
EOF
cat >"$proc_root/104/status" <<'EOF'
Name:   photothumb
VmRSS:  90 kB
RssAnon: 85 kB
EOF
cat >"$proc_root/101/smaps" <<'EOF'
10000000-10002000 rw-p 00000000 00:00 0 [heap]
Size:                  8 kB
Rss:                   8 kB
Anonymous:             4 kB
20000000-20001000 r--p 00000000 00:00 0 /fixture/map.dat
Size:                  4 kB
Rss:                   4 kB
Anonymous:             0 kB
EOF

mapping_snapshot=$(lima_memory_process_mapping_snapshot "$proc_root" 101 | cut -d'|' -f1,2)
expected=$'8192|4096\n4096|0'
if [[ "$mapping_snapshot" != "$expected" ]]; then
  printf 'mapping RSS snapshot = %s, want %s\n' "$mapping_snapshot" "$expected" >&2
  exit 1
fi
curl() {
  printf '%s\n' '{"sample_unix_nano":123,"mappings":[{"rss_bytes":8192,"anonymous_bytes":4096,"mapping":"10000000-10002000 rw-p 00000000 00:00 0 [heap]"},{"rss_bytes":4096,"anonymous_bytes":0,"mapping":"20000000-20001000 r--p 00000000 00:00 0 /fixture/map.dat"}]}'
}
mapping_snapshot=$(LIMA_MEMORY_MAPPING_ENDPOINT=http://127.0.0.1:2447/debug/runtime-memory-mappings \
  lima_memory_process_mapping_snapshot "$proc_root" 101)
if [[ "$mapping_snapshot" != $'mapping_sample_unix_nano=123\n8192|4096|10000000-10002000 rw-p 00000000 00:00 0 [heap]\n4096|0|20000000-20001000 r--p 00000000 00:00 0 /fixture/map.dat' ]]; then
  printf 'endpoint mapping snapshot = %s, want fixture mappings in RSS order\n' "$mapping_snapshot" >&2
  exit 1
fi

LIMA_MEMORY_CAPTURE_MAPS=1
lima_memory_configure_mapping_endpoint
mapping_endpoint_in_child=$(bash -c 'printf %s "${LIMA_MEMORY_MAPPING_ENDPOINT:-}"')
if [[ "$mapping_endpoint_in_child" != http://127.0.0.1:2447/debug/runtime-memory-mappings ]]; then
  printf 'mapping endpoint in sampler child = %s, want the loopback diagnostics endpoint\n' \
    "$mapping_endpoint_in_child" >&2
  exit 1
fi
export -f lima_memory_process_mapping_snapshot
export -f curl
mapping_snapshot_in_child=$(bash -c 'lima_memory_process_mapping_snapshot "$1" 101' _ "$proc_root")
if [[ "$mapping_snapshot_in_child" != $'mapping_sample_unix_nano=123\n8192|4096|10000000-10002000 rw-p 00000000 00:00 0 [heap]\n4096|0|20000000-20001000 r--p 00000000 00:00 0 /fixture/map.dat' ]]; then
  printf 'mapping snapshot in sampler child = %s, want timestamped mapping rows\n' \
    "$mapping_snapshot_in_child" >&2
  exit 1
fi
unset -f curl
LIMA_MEMORY_CAPTURE_MAPS=0
lima_memory_configure_mapping_endpoint
if bash -c '[[ -n ${LIMA_MEMORY_MAPPING_ENDPOINT:-} ]]'; then
  printf 'mapping endpoint remained enabled with mapping capture disabled\n' >&2
  exit 1
fi

snapshot="$(lima_memory_process_snapshot "$proc_root" "$fixture/cgroup.procs" 101)"
expected='101|102400|81920|2|102:71680:61440,103:76800:66560|complete|-|101,102,103'
if [[ "$snapshot" != "$expected" ]]; then
  printf 'process snapshot = %s, want %s\n' "$snapshot" "$expected" >&2
  exit 1
fi

rm "$proc_root/104/status"
printf '101\n102\n103\n104\n' >"$fixture/cgroup.procs"
snapshot="$(lima_memory_process_snapshot "$proc_root" "$fixture/cgroup.procs" 101)"
expected='101|102400|81920|2|102:71680:61440,103:76800:66560|incomplete|104|101,102,103,104'
if [[ "$snapshot" != "$expected" ]]; then
  printf 'snapshot with an unreadable cgroup PID = %s, want %s\n' "$snapshot" "$expected" >&2
  exit 1
fi

printf '101\n102\n103\n' >"$fixture/cgroup.procs"
snapshot="$(lima_memory_process_snapshot "$proc_root" "$fixture/cgroup.procs" 999)"
expected='999|unknown|unknown|2|102:71680:61440,103:76800:66560|incomplete|999|101,102,103'
if [[ "$snapshot" != "$expected" ]]; then
  printf 'snapshot without its server PID = %s, want %s\n' "$snapshot" "$expected" >&2
  exit 1
fi

sample_cgroup="$fixture/sample-cgroup"
sample_prefix="$fixture/sample"
mkdir -p "$sample_cgroup"
if ! declare -F lima_memory_record_cgroup_sample >/dev/null; then
  printf 'per-sample cgroup recorder is missing\n' >&2
  exit 1
fi
printf '101\n102\n' >"$sample_cgroup/cgroup.procs"
printf '3145728\n' >"$sample_cgroup/memory.peak"
printf '2000000\n' >"$sample_cgroup/memory.current"
printf 'low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n' >"$sample_cgroup/memory.events"
printf 'anon 1500000\nfile 100000\nkernel 500000\n' >"$sample_cgroup/memory.stat"
lima_memory_record_cgroup_sample "$sample_cgroup" "$proc_root" 101 "$sample_prefix"

# Current can rise while the cgroup high-water mark stays unchanged. The
# selected process snapshot must belong to that higher current sample.
printf '2500000\n' >"$sample_cgroup/memory.current"
printf 'anon 2000000\nfile 100000\nkernel 400000\n' >"$sample_cgroup/memory.stat"
cat >"$proc_root/102/status" <<'EOF'
Name:   photothumb
VmRSS:  80 kB
RssAnon: 70 kB
EOF
lima_memory_record_cgroup_sample "$sample_cgroup" "$proc_root" 101 "$sample_prefix"
expected='2500000|101|102400|81920|1|102:81920:71680|complete|-'
actual="$(<"$sample_prefix-processes")"
if [[ "$actual" != "$expected" ]]; then
  printf 'process sample paired with the highest current = %s, want %s\n' "$actual" "$expected" >&2
  exit 1
fi
if [[ $(<"$sample_prefix-stat-peak") != 2500000 || $(<"$sample_prefix-current") != 2500000 ||
  $(<"$sample_prefix-anon") != 2000000 ]]; then
  printf 'cgroup sample fields do not match the selected process sample\n' >&2
  exit 1
fi

mapping_cgroup="$fixture/mapping-cgroup"
mapping_prefix="$fixture/mapping-sample"
mkdir -p "$mapping_cgroup"
printf '101\n102\n' >"$mapping_cgroup/cgroup.procs"
printf '3145728\n' >"$mapping_cgroup/memory.peak"
printf '2000000\n' >"$mapping_cgroup/memory.current"
printf 'low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n' >"$mapping_cgroup/memory.events"
printf 'anon 1500000\nfile 100000\nkernel 500000\n' >"$mapping_cgroup/memory.stat"
LIMA_MEMORY_CAPTURE_MAPS=1 LIMA_MEMORY_MAPPING_TYPICAL_BYTES=1000000 \
  LIMA_MEMORY_MAPPING_SPIKE_DELTA_BYTES=500000 \
  lima_memory_record_cgroup_sample "$mapping_cgroup" "$proc_root" 101 "$mapping_prefix"
for label in typical spike; do
  if [[ ! -r "$mapping_prefix-mappings-$label" ]] ||
    ! grep -Fq '8192|4096|10000000-' "$mapping_prefix-mappings-$label"; then
    printf 'mapping sample %s is missing the expected heap RSS row\n' "$label" >&2
    exit 1
  fi
done

peak_unattributed_bytes=$(lima_memory_peak_unattributed_bytes 44675072 39170048)
if [[ "$peak_unattributed_bytes" != 5505024 ]]; then
  printf 'memory.peak gap = %s bytes, want 5505024\n' "$peak_unattributed_bytes" >&2
  exit 1
fi

printf '101\n102\n104\n' >"$sample_cgroup/cgroup.procs"
printf '2600000\n' >"$sample_cgroup/memory.current"
lima_memory_record_cgroup_sample "$sample_cgroup" "$proc_root" 101 "$sample_prefix"
expected='2600000|101|102400|81920|1|102:81920:71680|incomplete|104'
actual="$(<"$sample_prefix-processes")"
if [[ "$actual" != "$expected" ]]; then
  printf 'incomplete process sample = %s, want %s\n' "$actual" "$expected" >&2
  exit 1
fi

race_cgroup="$fixture/race-cgroup"
race_prefix="$fixture/race"
mkdir -p "$race_cgroup"
printf '101\n' >"$race_cgroup/cgroup.procs"
printf '3145728\n' >"$race_cgroup/memory.peak"
printf 'low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n' >"$race_cgroup/memory.events"
printf '3000000\n' >"$race_cgroup/memory.current"
mkfifo "$race_cgroup/memory.stat"
(
  exec 3>"$race_cgroup/memory.stat"
  printf '101\n102\n' >"$race_cgroup/cgroup.procs"
  printf 'anon 2500000\nfile 0\nkernel 500000\n' >&3
) &
race_writer_pid=$!
lima_memory_record_cgroup_sample "$race_cgroup" "$proc_root" 101 "$race_prefix"
wait "$race_writer_pid"
expected='3000000|101|102400|81920|0|-|incomplete|102'
actual="$(<"$race_prefix-processes")"
if [[ "$actual" != "$expected" ]]; then
  printf 'membership change between process and memory samples = %s, want %s\n' "$actual" "$expected" >&2
  exit 1
fi

if normal_report="$(lima_memory_process_attribution_check 3 3000000 incomplete 102 2>&1)"; then
  normal_status=0
else
  normal_status=$?
fi
expected='memory probe sampled incomplete process attribution at 3000000 bytes: pids=102'
if [[ "$normal_status" != 0 || "$normal_report" != "$expected" ]]; then
  printf 'normal probe attribution report = %s (status %s), want %s (status 0)\n' \
    "$normal_report" "$normal_status" "$expected" >&2
  exit 1
fi

if diagnostic_report="$(lima_memory_process_attribution_check 10 3000000 incomplete 102 2>&1)"; then
  diagnostic_status=0
else
  diagnostic_status=$?
fi
if [[ "$diagnostic_status" == 0 || "$diagnostic_report" != "$expected" ]]; then
  printf 'diagnostic attribution report = %s (status %s), want %s (nonzero)\n' \
    "$diagnostic_report" "$diagnostic_status" "$expected" >&2
  exit 1
fi
if ! lima_memory_process_attribution_check 10 3000000 complete -; then
  printf 'diagnostic probe rejected complete process attribution\n' >&2
  exit 1
fi

runtime_cgroup="$fixture/runtime-cgroup"
runtime_prefix="$fixture/runtime-sample"
runtime_ready="$fixture/runtime-ready"
runtime_cgroup_stop="$fixture/runtime-cgroup-stop"
runtime_stop="$fixture/runtime-runtime-stop"
runtime_error="$fixture/runtime-error"
runtime_file="$fixture/runtime.json"
runtime_request_started="$fixture/runtime-request-started"
runtime_request_release="$fixture/runtime-request-release"
runtime_final_request_started="$fixture/runtime-final-request-started"
runtime_final_request_release="$fixture/runtime-final-request-release"
runtime_request_samples="$fixture/runtime-request-samples"
mkdir -p "$runtime_cgroup"
printf '101\n102\n103\n' >"$runtime_cgroup/cgroup.procs"
printf '3145728\n' >"$runtime_cgroup/memory.peak"
printf 'low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n' >"$runtime_cgroup/memory.events"
printf '2500000\n' >"$runtime_cgroup/memory.current"
printf 'anon 2000000\nfile 100000\nkernel 400000\n' >"$runtime_cgroup/memory.stat"
lima_memory_record_cgroup_sample "$runtime_cgroup" "$proc_root" 101 "$runtime_prefix"
# The runtime request must carry the selected cgroup sample, even if the live
# current value advances before the request finishes.
: >"$runtime_prefix-stat-peak"
(sleep 0.05; printf '2500000\n' >"$runtime_prefix-stat-peak") &
sample_peak_writer_pid=$!
printf '2600000\n' >"$runtime_cgroup/memory.current"
printf 'anon 2100000\nfile 100000\nkernel 400000\n' >"$runtime_cgroup/memory.stat"
touch "$runtime_ready"

runtime_curl_calls=0
curl() {
  local output=
  while (($#)); do
    if [[ "$1" == --output ]]; then
      output="$2"
      shift 2
    else
      shift
    fi
  done
  runtime_curl_calls=$((runtime_curl_calls + 1))
  if (( runtime_curl_calls == 1 )); then
    : >"$runtime_request_started"
    while [[ ! -e "$runtime_request_release" ]]; do
      sleep 0.01
    done
  elif (( runtime_curl_calls == 2 )); then
    : >"$runtime_final_request_started"
    while [[ ! -e "$runtime_final_request_release" ]]; do
      sleep 0.01
    done
  fi
  local sample_unix_nano
  sample_unix_nano=$(date +%s%N)
  printf '{"sample_unix_nano":%s,"heap_live_bytes":%s}\n' "$sample_unix_nano" "$runtime_curl_calls" >"$output"
  printf '%s|%s\n' "$runtime_curl_calls" "$sample_unix_nano" >>"$runtime_request_samples"
}

lima_memory_sample_runtime_metrics_loop "$runtime_prefix-stat-peak" "$runtime_prefix-history" \
  "$((512 * 1024))" "$runtime_ready" "$runtime_file" "$runtime_error" "$runtime_stop" 0.01 &
runtime_sampler_pid=$!
for _ in $(seq 200); do
  if [[ -e "$runtime_request_started" ]]; then
    break
  fi
  sleep 0.01
done
if [[ ! -e "$runtime_request_started" ]]; then
  printf 'runtime sampler did not begin the blocking metrics request\n' >&2
  : >"$runtime_request_release"
  : >"$runtime_stop"
  wait "$sample_peak_writer_pid" 2>/dev/null || true
  wait "$runtime_sampler_pid" 2>/dev/null || true
  exit 1
fi
lima_memory_sample_cgroup_loop "$runtime_cgroup" "$proc_root" 101 "$runtime_prefix" \
  "$runtime_cgroup_stop" "$runtime_error" 0.01 &
cgroup_sampler_pid=$!

# A delayed runtime request must not pause 10 ms process snapshots. Capture a
# short-lived worker at the higher current, then lower current before it exits.
mkdir -p "$proc_root/105"
cat >"$proc_root/105/status" <<'EOF'
Name:   photothumb
VmRSS:  95 kB
RssAnon: 90 kB
EOF
printf '101\n102\n103\n105\n' >"$runtime_cgroup/cgroup.procs"
printf '3100000\n' >"$runtime_cgroup/memory.current"
printf 'anon 2500000\nfile 100000\nkernel 500000\n' >"$runtime_cgroup/memory.stat"
sampled_current=
sampled_worker_count=
sampled_attribution=
for _ in $(seq 200); do
  if [[ -r "$runtime_prefix-processes" ]]; then
    sampled_current=
    sampled_worker_count=
    sampled_attribution=
    if IFS='|' read -r sampled_current _ _ _ sampled_worker_count _ sampled_attribution _ \
      <"$runtime_prefix-processes"; then
      if [[ "$sampled_current" == 3100000 && "$sampled_worker_count" == 3 &&
        "$sampled_attribution" == complete ]]; then
        break
      fi
    fi
  fi
  sleep 0.01
done
if [[ "$sampled_current" != 3100000 || "$sampled_worker_count" != 3 ||
  "$sampled_attribution" != complete ]]; then
  process_snapshot=missing
  if [[ -r "$runtime_prefix-processes" ]]; then
    process_snapshot=$(<"$runtime_prefix-processes")
  fi
  printf 'process sampler missed the short-lived worker while runtime metrics were blocked: %s\n' \
    "$process_snapshot" >&2
  exit 1
fi
for _ in $(seq 200); do
  if [[ -r "$runtime_prefix-history" ]] &&
    grep -Eq '^([0-9]+)[|]3100000[|]' "$runtime_prefix-history"; then
    break
  fi
  sleep 0.01
done
if ! grep -Eq '^([0-9]+)[|]3100000[|]' "$runtime_prefix-history"; then
  printf 'cgroup sampler did not record the high-current worker snapshot\n' >&2
  exit 1
fi
: >"$runtime_request_release"
for _ in $(seq 200); do
  if [[ -r "$runtime_file" ]]; then
    paired_sample=$(jq -er '.cgroup_sample | select(type == "string")' "$runtime_file")
    if IFS='|' read -r _ paired_current _ <<<"$paired_sample" && [[ "$paired_current" == 3100000 ]]; then
      break
    fi
  fi
  sleep 0.01
done
if [[ ! -r "$runtime_file" ]]; then
  [[ -r "$runtime_error" ]] && cat "$runtime_error" >&2
  printf 'runtime sampler did not persist its first paired sample\n' >&2
  exit 1
fi
for _ in $(seq 200); do
  if [[ -e "$runtime_final_request_started" ]]; then
    break
  fi
  sleep 0.01
done
if [[ ! -e "$runtime_final_request_started" ]]; then
  [[ -r "$runtime_error" ]] && cat "$runtime_error" >&2
  printf 'runtime sampler did not make the final aligned request\n' >&2
  exit 1
fi
printf '2800000\n' >"$runtime_cgroup/memory.current"
printf '101\n102\n103\n' >"$runtime_cgroup/cgroup.procs"
rm -f -- "$proc_root/105/status"
for _ in $(seq 200); do
  if grep -Eq '^([0-9]+)[|]2800000[|]' "$runtime_prefix-history"; then
    break
  fi
  sleep 0.01
done
: >"$runtime_final_request_release"
touch "$runtime_stop"
wait "$runtime_sampler_pid"
runtime_sampler_pid=
: >"$runtime_cgroup_stop"
wait "$cgroup_sampler_pid"
cgroup_sampler_pid=
wait "$sample_peak_writer_pid"
sample_peak_writer_pid=
final_runtime_timestamp=$(awk -F'|' '$1 == 2 { print $2 }' "$runtime_request_samples")
final_aligned_sample=$(lima_memory_nearest_cgroup_sample "$runtime_prefix-history" "$final_runtime_timestamp")
IFS='|' read -r _ final_sample_current _ <<<"$final_aligned_sample"
if [[ -e "$runtime_error" ]]; then
  cat "$runtime_error" >&2
  exit 1
fi
runtime_metrics=$(jq -c '.runtime' "$runtime_file")
paired_sample=$(jq -er '.cgroup_sample | select(type == "string")' "$runtime_file")
if [[ "$runtime_metrics" != *'"heap_live_bytes":1'* ]] ||
  ! IFS='|' read -r _ paired_current _ <<<"$paired_sample" ||
  [[ "$paired_current" != 3100000 || "$final_sample_current" != 2800000 ]]; then
  printf 'runtime flush relabeled a later cgroup sample: metrics=%s cgroup_sample=%s\n' \
    "$runtime_metrics" "$paired_sample" >&2
  exit 1
fi

printf 'Lima process snapshot fixture passed\n'
