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
mapping_cgroup_stop=
mapping_worker_stop=
mapping_worker_release=
mapping_worker_pid=
mapping_cgroup_sampler_pid=
cleanup_fixture() {
  local status=$?
  trap - EXIT
  [[ -n ${runtime_request_release:-} ]] && : >"$runtime_request_release" 2>/dev/null || true
  [[ -n ${runtime_final_request_release:-} ]] && : >"$runtime_final_request_release" 2>/dev/null || true
  [[ -n ${runtime_cgroup_stop:-} ]] && : >"$runtime_cgroup_stop" 2>/dev/null || true
  [[ -n ${runtime_stop:-} ]] && : >"$runtime_stop" 2>/dev/null || true
  [[ -n ${mapping_worker_release:-} ]] && : >"$mapping_worker_release" 2>/dev/null || true
  [[ -n ${mapping_worker_stop:-} ]] && : >"$mapping_worker_stop" 2>/dev/null || true
  [[ -n ${mapping_cgroup_stop:-} ]] && : >"$mapping_cgroup_stop" 2>/dev/null || true
  for pid in "${sample_peak_writer_pid:-}" "${runtime_sampler_pid:-}" "${cgroup_sampler_pid:-}" \
    "${mapping_worker_pid:-}" "${mapping_cgroup_sampler_pid:-}"; do
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

mapping_snapshot=$(lima_memory_process_mapping_snapshot "$proc_root" 101)
expected=$'8192|4096\n4096|0'
if [[ $(sed -n '1p' <<<"$mapping_snapshot") != mapping_sample_unix_nano=* ||
  $(sed -n '2p' <<<"$mapping_snapshot") != mapping_anon_bytes=4096 ||
  $(sed -n '3,4p' <<<"$mapping_snapshot" | cut -d'|' -f1,2) != "$expected" ]]; then
  printf 'mapping RSS snapshot = %s, want %s\n' "$mapping_snapshot" "$expected" >&2
  exit 1
fi
curl() {
  printf '%s\n' '{"sample_unix_nano":123,"total_anonymous_bytes":16384,"mappings":[{"rss_bytes":8192,"anonymous_bytes":4096,"mapping":"10000000-10002000 rw-p 00000000 00:00 0 [heap]"},{"rss_bytes":4096,"anonymous_bytes":0,"mapping":"20000000-20001000 r--p 00000000 00:00 0 /fixture/map.dat"}]}'
}
mapping_snapshot=$(LIMA_MEMORY_MAPPING_ENDPOINT=http://127.0.0.1:2447/debug/runtime-memory-mappings \
  lima_memory_process_mapping_snapshot "$proc_root" 101)
if [[ "$mapping_snapshot" != $'mapping_sample_unix_nano=123\nmapping_anon_bytes=16384\n8192|4096|10000000-10002000 rw-p 00000000 00:00 0 [heap]\n4096|0|20000000-20001000 r--p 00000000 00:00 0 /fixture/map.dat' ]]; then
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
if [[ "$mapping_snapshot_in_child" != $'mapping_sample_unix_nano=123\nmapping_anon_bytes=16384\n8192|4096|10000000-10002000 rw-p 00000000 00:00 0 [heap]\n4096|0|20000000-20001000 r--p 00000000 00:00 0 /fixture/map.dat' ]]; then
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

peak_history="$fixture/peak-history"
cat >"$peak_history" <<'EOF'
100|34578432|19959808|0|6918144|34578432|101|45355008|20041728|0|-|incomplete|102|39825408
200|27119616|19959808|0|6918144|27119616|101|45355008|20041728|0|-|complete|-|39825408
EOF
if ! declare -F lima_memory_best_cgroup_sample >/dev/null; then
  printf 'independent best cgroup sample selector is missing\n' >&2
  exit 1
fi
best_process_sample=$(lima_memory_best_cgroup_sample "$peak_history")
expected='100|34578432|19959808|0|6918144|34578432|101|45355008|20041728|0|-|incomplete|102|39825408'
if [[ "$best_process_sample" != "$expected" ]]; then
  printf 'independent process peak sample = %s, want %s\n' "$best_process_sample" "$expected" >&2
  exit 1
fi
IFS='|' read -r _ best_process_peak _ _ _ _ _ _ _ _ _ best_process_attribution best_process_incomplete_pids _ \
  <<<"$best_process_sample"
if [[ $best_process_attribution != incomplete || $best_process_peak != 34578432 ]]; then
  printf 'best process sample did not preserve the incomplete high-current row\n' >&2
  exit 1
fi
if lima_memory_process_attribution_check 10 "$best_process_peak" "$best_process_attribution" \
  "$best_process_incomplete_pids" >/dev/null 2>&1; then
  printf 'diagnostic gate accepted incomplete attribution from the independent peak sample\n' >&2
  exit 1
fi

mapping_cgroup="$fixture/mapping-cgroup"
mapping_prefix="$fixture/mapping-sample"
mapping_ready="$mapping_prefix-mappings-ready"
mapping_worker_stop="$fixture/mapping-worker-stop"
mapping_worker_error="$fixture/mapping-worker-error"
mapping_worker_request_started="$fixture/mapping-worker-request-started"
mapping_worker_release="$fixture/mapping-worker-release"
mapping_worker_calls_file="$fixture/mapping-worker-calls"
mapping_cgroup_stop="$fixture/mapping-cgroup-stop"
mkdir -p "$mapping_cgroup"
printf '101\n102\n' >"$mapping_cgroup/cgroup.procs"
printf '3145728\n' >"$mapping_cgroup/memory.peak"
printf '2000000\n' >"$mapping_cgroup/memory.current"
printf 'low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n' >"$mapping_cgroup/memory.events"
printf 'anon 1500000\nfile 100000\nkernel 500000\n' >"$mapping_cgroup/memory.stat"
printf '0\n' >"$mapping_worker_calls_file"
mkdir -p "$proc_root/105"
cat >"$proc_root/105/status" <<'EOF'
Name:   photothumb
VmRSS:  95 kB
RssAnon: 90 kB
EOF
export mapping_worker_calls_file mapping_worker_request_started mapping_worker_release
curl() {
  local mapping_worker_calls sample_unix_nano mapping_anon_bytes
  mapping_worker_calls=$(<"$mapping_worker_calls_file")
  mapping_worker_calls=$((mapping_worker_calls + 1))
  printf '%s\n' "$mapping_worker_calls" >"$mapping_worker_calls_file"
  if (( mapping_worker_calls == 2 )); then
    : >"$mapping_worker_request_started"
    while [[ ! -e "$mapping_worker_release" ]]; do
      sleep 0.01
    done
  fi
  sample_unix_nano=$(date +%s%N)
  if (( mapping_worker_calls == 1 )); then
    mapping_anon_bytes=81920
  else
    mapping_anon_bytes=819200
  fi
  printf '{"sample_unix_nano":%s,"total_anonymous_bytes":%s,"mappings":[{"rss_bytes":%s,"anonymous_bytes":%s,"mapping":"10000000-10002000 rw-p 00000000 00:00 0 [anon: Go: heap]"}]}\n' \
    "$sample_unix_nano" "$mapping_anon_bytes" "$mapping_anon_bytes" "$mapping_anon_bytes"
}
export -f curl
export LIMA_MEMORY_CAPTURE_MAPS=1
export LIMA_MEMORY_MAPPING_TYPICAL_BYTES=2000000
export LIMA_MEMORY_MAPPING_SPIKE_DELTA_BYTES=524288
export LIMA_MEMORY_MAPPING_ENDPOINT=http://127.0.0.1:2447/debug/runtime-memory-mappings
lima_memory_sample_mapping_loop "$proc_root" 101 "$mapping_prefix" \
  "$mapping_prefix-history" "$mapping_ready" "$mapping_worker_stop" \
  "$mapping_worker_error" 0.01 &
mapping_worker_pid=$!
lima_memory_sample_cgroup_loop "$mapping_cgroup" "$proc_root" 101 "$mapping_prefix" \
  "$mapping_cgroup_stop" "$mapping_worker_error" 0.01 &
mapping_cgroup_sampler_pid=$!
mapping_baseline_row=
for _ in $(seq 500); do
  if [[ -s "$mapping_prefix-history" ]]; then
    mapping_baseline_row=$(tail -n 1 "$mapping_prefix-history")
    [[ -n $mapping_baseline_row ]] && break
  fi
  sleep 0.01
done
if [[ -z $mapping_baseline_row ]]; then
  printf 'cgroup sampler did not record a pre-workload mapping baseline\n' >&2
  exit 1
fi
lima_memory_queue_mapping_request "$mapping_prefix" typical "$mapping_baseline_row"
: >"$mapping_ready"
for _ in $(seq 500); do
  if [[ -r "$mapping_prefix-mappings-typical" ]]; then
    break
  fi
  sleep 0.01
done
if [[ ! -r "$mapping_prefix-mappings-typical" ]]; then
  printf 'mapping worker did not capture the typical baseline\n' >&2
  [[ ! -r $mapping_worker_error ]] || cat "$mapping_worker_error" >&2
  [[ ! -r $mapping_prefix-history ]] || cat "$mapping_prefix-history" >&2
  exit 1
fi
cat >"$proc_root/101/status" <<'EOF'
Name:   teagramd
VmRSS:  900 kB
RssAnon: 800 kB
EOF
printf '101\n102\n103\n105\n' >"$mapping_cgroup/cgroup.procs"
printf '3100000\n' >"$mapping_cgroup/memory.current"
printf 'anon 2500000\nfile 100000\nkernel 500000\n' >"$mapping_cgroup/memory.stat"
for _ in $(seq 500); do
  if [[ -e "$mapping_worker_request_started" ]]; then
    break
  fi
  sleep 0.01
done
if [[ ! -e "$mapping_worker_request_started" ]]; then
  printf 'mapping worker did not begin its spike mapping request\n' >&2
  exit 1
fi
printf '3200000\n' >"$mapping_cgroup/memory.current"
printf '3200000\n' >"$mapping_cgroup/memory.peak"
printf 'anon 2600000\nfile 100000\nkernel 500000\n' >"$mapping_cgroup/memory.stat"
for _ in $(seq 500); do
  if grep -Eq '^([0-9]+)[|]3200000[|]' "$mapping_prefix-history"; then
    break
  fi
  sleep 0.01
done
if ! grep -Eq '^([0-9]+)[|]3200000[|]' "$mapping_prefix-history"; then
  printf '10 ms cgroup sampler paused while mapping request was blocked\n' >&2
  exit 1
fi
: >"$mapping_worker_release"
for _ in $(seq 500); do
  if [[ -r "$mapping_prefix-mappings-spike" ]]; then
    break
  fi
  sleep 0.01
done
if [[ ! -r "$mapping_prefix-mappings-spike" ]]; then
  printf 'mapping worker did not persist the spike sample after the blocked request completed\n' >&2
  exit 1
fi
: >"$mapping_cgroup_stop"
wait "$mapping_cgroup_sampler_pid"
mapping_cgroup_sampler_pid=
: >"$mapping_worker_stop"
wait "$mapping_worker_pid"
mapping_worker_pid=
if [[ -e "$mapping_worker_error" ]]; then
  cat "$mapping_worker_error" >&2
  exit 1
fi
mapping_curl_calls=$(<"$mapping_worker_calls_file")
if [[ $mapping_curl_calls != 2 ]]; then
  printf 'mapping worker requests = %s, want typical and one measured spike\n' "$mapping_curl_calls" >&2
  exit 1
fi
unset -f curl
unset LIMA_MEMORY_CAPTURE_MAPS LIMA_MEMORY_MAPPING_TYPICAL_BYTES LIMA_MEMORY_MAPPING_SPIKE_DELTA_BYTES \
  LIMA_MEMORY_MAPPING_ENDPOINT
if ! grep -Fq 'server_process_anon_bytes=81920' "$mapping_prefix-mappings-typical" ||
  ! grep -Fq 'mapping_anon_bytes=81920' "$mapping_prefix-mappings-typical" ||
  ! grep -Fq 'server_process_anon_bytes=819200' "$mapping_prefix-mappings-spike" ||
  ! grep -Fq 'mapping_anon_bytes=819200' "$mapping_prefix-mappings-spike" ||
  ! grep -Fq 'cgroup_memory_peak_bytes=3200000' "$mapping_prefix-mappings-spike" ||
  ! grep -Fq '[anon: Go: heap]' "$mapping_prefix-mappings-spike"; then
  printf 'mapping samples did not pair server anon with the Go heap mapping totals:\ntypical:\n%s\nspike:\n%s\n' \
    "$(<"$mapping_prefix-mappings-typical")" "$(<"$mapping_prefix-mappings-spike")" >&2
  exit 1
fi
for mapping_file in "$mapping_prefix-mappings-typical" "$mapping_prefix-mappings-spike"; do
  mapping_alignment=$(awk -F= '$1 == "mapping_cgroup_alignment_delta_ns" { print $2; exit }' "$mapping_file")
  if [[ ! $mapping_alignment =~ ^[0-9]+$ ]] || (( mapping_alignment > 100000000 )); then
    printf 'mapping sample did not align with a cgroup row within 100 ms: %s\n' "$mapping_file" >&2
    exit 1
  fi
done
cat >"$proc_root/101/status" <<'EOF'
Name:   teagramd
VmRSS:  100 kB
RssAnon: 80 kB
EOF

mapping_guard_typical="$fixture/guard-typical"
mapping_guard_spike="$fixture/guard-spike"
cat >"$mapping_guard_typical" <<'EOF'
mapping_cgroup_alignment_delta_ns=25000000
server_process_anon_bytes=1000000
mapping_anon_bytes=900000
EOF
unset LIMA_MEMORY_MAPPING_SPIKE_DELTA_BYTES
if ! mapping_spike_delta_bytes=$(lima_memory_mapping_spike_delta_bytes); then
  printf 'mapping spike delta default helper is unavailable\n' >&2
  exit 1
fi
if [[ $mapping_spike_delta_bytes != 262144 ]]; then
  printf 'mapping spike delta default = %s, want 262144 bytes\n' "$mapping_spike_delta_bytes" >&2
  exit 1
fi
mapping_actual_typical="$fixture/guard-actual-typical"
mapping_actual_spike="$fixture/guard-actual-spike"
cat >"$mapping_actual_typical" <<'EOF'
mapping_cgroup_alignment_delta_ns=25000000
server_process_anon_bytes=19886080
mapping_anon_bytes=19877888
EOF
cat >"$mapping_actual_spike" <<'EOF'
mapping_cgroup_alignment_delta_ns=50000000
server_process_anon_bytes=20205568
mapping_anon_bytes=20119552
EOF
if ! lima_memory_mapping_capture_check 38604800 31457280 "$mapping_spike_delta_bytes" 1048576 \
  "$mapping_actual_typical" "$mapping_actual_spike"; then
  printf 'mapping capture guard rejected the measured 319488-byte anon increase and matching mapping delta\n' >&2
  exit 1
fi
if mapping_guard_report=$(lima_memory_mapping_capture_check 2500000 1000000 500000 100000 \
  "$mapping_guard_typical" "$mapping_guard_spike" 2>&1); then
  printf 'mapping capture guard accepted a high-water spike without spike evidence\n' >&2
  exit 1
fi
expected='memory.peak reached 2500000 bytes but no spike mapping sample was captured'
if [[ "$mapping_guard_report" != "$expected" ]]; then
  printf 'mapping capture guard report = %s, want %s\n' "$mapping_guard_report" "$expected" >&2
  exit 1
fi
if ! lima_memory_mapping_capture_check 1400000 1000000 500000 100000 \
  "$mapping_guard_typical" "$mapping_guard_spike"; then
  printf 'mapping capture guard rejected a high-water value below the spike threshold\n' >&2
  exit 1
fi
cat >"$mapping_guard_spike" <<'EOF'
mapping_cgroup_alignment_delta_ns=50000000
server_process_anon_bytes=1100000
mapping_anon_bytes=1000000
EOF
if mapping_guard_report=$(lima_memory_mapping_capture_check 2500000 1000000 500000 100000 \
  "$mapping_guard_typical" "$mapping_guard_spike" 2>&1); then
  printf 'mapping capture guard accepted a nonempty spike row without the required anon increase\n' >&2
  exit 1
fi
if [[ $mapping_guard_report != *'server anon increase was 100000 bytes, want at least 500000'* ]]; then
  printf 'mapping capture guard anon report = %s, want the measured server anon delta\n' \
    "$mapping_guard_report" >&2
  exit 1
fi
cat >"$mapping_guard_spike" <<'EOF'
mapping_cgroup_alignment_delta_ns=50000000
server_process_anon_bytes=1600000
mapping_anon_bytes=1040000
EOF
if mapping_guard_report=$(lima_memory_mapping_capture_check 2500000 1000000 500000 100000 \
  "$mapping_guard_typical" "$mapping_guard_spike" 2>&1); then
  printf 'mapping capture guard accepted a server anon spike not accounted for by map anon RSS\n' >&2
  exit 1
fi
if [[ $mapping_guard_report != *'mapping anon delta was 140000 bytes for a 600000-byte server anon increase'* ]]; then
  printf 'mapping capture guard map report = %s, want measured mapping delta mismatch\n' \
    "$mapping_guard_report" >&2
  exit 1
fi
cat >"$mapping_guard_spike" <<'EOF'
mapping_cgroup_alignment_delta_ns=50000000
server_process_anon_bytes=1600000
mapping_anon_bytes=1450000
EOF
if ! lima_memory_mapping_capture_check 2500000 1000000 500000 100000 \
  "$mapping_guard_typical" "$mapping_guard_spike"; then
  printf 'mapping capture guard rejected a measured map delta matching the server anon increase\n' >&2
  exit 1
fi
sed -i 's/mapping_cgroup_alignment_delta_ns=50000000/mapping_cgroup_alignment_delta_ns=100000001/' \
  "$mapping_guard_spike"
if mapping_guard_report=$(lima_memory_mapping_capture_check 2500000 1000000 500000 100000 \
  "$mapping_guard_typical" "$mapping_guard_spike" 2>&1); then
  printf 'mapping capture guard accepted a spike sample outside the 100 ms pairing window\n' >&2
  exit 1
fi
if [[ $mapping_guard_report != 'spike mapping sample is missing a cgroup pair within 100 ms' ]]; then
  printf 'mapping capture guard alignment report = %s, want a 100 ms pairing failure\n' \
    "$mapping_guard_report" >&2
  exit 1
fi

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
