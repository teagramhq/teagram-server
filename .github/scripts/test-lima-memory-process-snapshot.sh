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
expected='101|102400|81920|2|102:71680:61440,103:76800:66560'
if [[ "$snapshot" != "$expected" ]]; then
  printf 'process snapshot = %s, want %s\n' "$snapshot" "$expected" >&2
  exit 1
fi
if lima_memory_process_snapshot "$proc_root" "$fixture/cgroup.procs" 999 >/dev/null 2>&1; then
  printf 'process snapshot accepted a missing server PID\n' >&2
  exit 1
fi

printf 'Lima process snapshot fixture passed\n'
