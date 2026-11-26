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

printf 'Lima process snapshot fixture passed\n'
