#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
tmp_parent="${TMPDIR:-/tmp}"
tmp="$(mktemp -d "$tmp_parent/perf-run-restore-test.XXXXXXXX")"

cleanup() {
  case "$tmp" in
    "$tmp_parent"/perf-run-restore-test.*) rm -rf -- "$tmp" ;;
    *) printf 'refusing to remove unexpected test directory: %s\n' "$tmp" >&2; return 1 ;;
  esac
}
trap cleanup EXIT

mkdir -p "$tmp/bin" "$tmp/work"
cat > "$tmp/bin/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '<%s>' "$@" >> "$PERF_DOCKER_CALLS"
printf '\n' >> "$PERF_DOCKER_CALLS"
SH
chmod +x "$tmp/bin/docker"

export PATH="$tmp/bin:$PATH"
export PERF_PG_CONTAINER=perf-pg
export PERF_PG_URL=postgres://postgres:perf@localhost:5432/
export PERF_CACHE=/cache
export PERF_WORK="$tmp/work"
export PERF_DOCKER_CALLS="$tmp/docker-calls"

bash "$script_dir/perf-run.sh" restore

for fixture in 1x 100x; do
  expected="<exec><perf-pg><pg_restore><-U><postgres><-d><perf_$fixture><-j><4><--no-owner><--no-acl></cache/perf_$fixture.dump>"
  if ! grep -Fxq -- "$expected" "$PERF_DOCKER_CALLS"; then
    printf 'fixture restore must skip archived ACLs for %s\n' "$fixture" >&2
    cat "$PERF_DOCKER_CALLS" >&2
    exit 1
  fi
done

printf '%s\n' 'perf-run fixture restore skips archived ACLs'
