#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=.github/scripts/lima-memory-process-snapshot.sh
source "$script_dir/lima-memory-process-snapshot.sh"

fixture="$(mktemp -d)"
trap 'rm -rf -- "$fixture"' EXIT
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
runtime_stop="$fixture/runtime-stop"
runtime_error="$fixture/runtime-error"
runtime_file="$fixture/runtime.json"
runtime_sample_current="$fixture/runtime-current"
runtime_request_started="$fixture/runtime-request-started"
runtime_request_release="$fixture/runtime-request-release"
mkdir -p "$runtime_cgroup"
printf '101\n102\n103\n' >"$runtime_cgroup/cgroup.procs"
printf '3145728\n' >"$runtime_cgroup/memory.peak"
printf 'low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n' >"$runtime_cgroup/memory.events"
printf '2500000\n' >"$runtime_cgroup/memory.current"
printf 'anon 2000000\nfile 100000\nkernel 400000\n' >"$runtime_cgroup/memory.stat"
touch "$runtime_ready"

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
  : >"$runtime_request_started"
  while [[ ! -e "$runtime_request_release" ]]; do
    sleep 0.01
  done
  printf '{"heap_live_bytes":1}\n' >"$output"
}

lima_memory_sample_cgroup_loop "$runtime_cgroup" "$proc_root" 101 "$runtime_prefix" \
  "$runtime_stop" "$runtime_error" 0.01 &
cgroup_sampler_pid=$!
lima_memory_sample_runtime_metrics_loop "$runtime_cgroup/memory.current" "$((512 * 1024))" "$runtime_ready" \
  "$runtime_file" "$runtime_sample_current" "$runtime_error" "$runtime_stop" 0.01 &
runtime_sampler_pid=$!
for attempt in $(seq 200); do
  if [[ -e "$runtime_request_started" ]]; then
    break
  fi
  sleep 0.01
done
if [[ ! -e "$runtime_request_started" ]]; then
  printf 'runtime sampler did not begin the blocking metrics request\n' >&2
  : >"$runtime_request_release"
  : >"$runtime_stop"
  wait "$cgroup_sampler_pid" 2>/dev/null || true
  wait "$runtime_sampler_pid" 2>/dev/null || true
  exit 1
fi

# A delayed runtime request must not pause 10 ms process snapshots. Capture a
# short-lived worker at the higher current, then lower current before it exits.
mkdir -p "$proc_root/105"
cat >"$proc_root/105/status" <<'EOF'
Name:   photothumb
VmRSS:  95 kB
RssAnon: 90 kB
EOF
printf '101\n102\n103\n105\n' >"$runtime_cgroup/cgroup.procs"
printf '3000000\n' >"$runtime_cgroup/memory.current"
printf 'anon 2400000\nfile 100000\nkernel 500000\n' >"$runtime_cgroup/memory.stat"
sampled_worker_count=
for attempt in $(seq 200); do
  if [[ -r "$runtime_prefix-processes" ]]; then
    IFS='|' read -r sampled_current _ _ _ sampled_worker_count _ sampled_attribution _ \
      <"$runtime_prefix-processes"
    if [[ "$sampled_current" == 3000000 && "$sampled_worker_count" == 3 &&
      "$sampled_attribution" == complete ]]; then
      break
    fi
  fi
  sleep 0.01
done
if [[ "$sampled_current" != 3000000 || "$sampled_worker_count" != 3 ||
  "$sampled_attribution" != complete ]]; then
  printf 'process sampler missed the short-lived worker while runtime metrics were blocked: %s\n' \
    "$(<"$runtime_prefix-processes" 2>/dev/null || printf missing)" >&2
  : >"$runtime_request_release"
  : >"$runtime_stop"
  wait "$cgroup_sampler_pid" 2>/dev/null || true
  wait "$runtime_sampler_pid" 2>/dev/null || true
  exit 1
fi
printf '2800000\n' >"$runtime_cgroup/memory.current"
printf '101\n102\n103\n' >"$runtime_cgroup/cgroup.procs"
rm -f -- "$proc_root/105/status"
touch "$runtime_stop" "$runtime_request_release"
wait "$cgroup_sampler_pid"
wait "$runtime_sampler_pid"
if [[ -e "$runtime_error" ]]; then
  cat "$runtime_error" >&2
  exit 1
fi
if [[ $(<"$runtime_sample_current") != 2500000 || $(<"$runtime_file") != '{"heap_live_bytes":1}' ]]; then
  printf 'runtime metrics sample = %s at %s bytes, want fixture metrics at 2500000 bytes\n' \
    "$(<"$runtime_file")" "$(<"$runtime_sample_current")" >&2
  exit 1
fi

printf 'Lima process snapshot fixture passed\n'
