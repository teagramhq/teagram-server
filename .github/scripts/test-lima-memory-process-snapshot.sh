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
expected='101|102400|81920|2|102:71680:61440,103:76800:66560|complete|-'
if [[ "$snapshot" != "$expected" ]]; then
  printf 'process snapshot = %s, want %s\n' "$snapshot" "$expected" >&2
  exit 1
fi

rm "$proc_root/104/status"
printf '101\n102\n103\n104\n' >"$fixture/cgroup.procs"
snapshot="$(lima_memory_process_snapshot "$proc_root" "$fixture/cgroup.procs" 101)"
expected='101|102400|81920|2|102:71680:61440,103:76800:66560|incomplete|104'
if [[ "$snapshot" != "$expected" ]]; then
  printf 'snapshot with an unreadable cgroup PID = %s, want %s\n' "$snapshot" "$expected" >&2
  exit 1
fi

printf '101\n102\n103\n' >"$fixture/cgroup.procs"
snapshot="$(lima_memory_process_snapshot "$proc_root" "$fixture/cgroup.procs" 999)"
expected='999|unknown|unknown|2|102:71680:61440,103:76800:66560|incomplete|999'
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

printf '101\n102\n104\n' >"$sample_cgroup/cgroup.procs"
printf '2600000\n' >"$sample_cgroup/memory.current"
lima_memory_record_cgroup_sample "$sample_cgroup" "$proc_root" 101 "$sample_prefix"
expected='2600000|101|102400|81920|1|102:81920:71680|incomplete|104'
actual="$(<"$sample_prefix-processes")"
if [[ "$actual" != "$expected" ]]; then
  printf 'incomplete process sample = %s, want %s\n' "$actual" "$expected" >&2
  exit 1
fi

printf 'Lima process snapshot fixture passed\n'
