#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
tmp_parent="${TMPDIR:-/tmp}"
tmp="$(mktemp -d "$tmp_parent/perf-postgres-readiness-test.XXXXXXXX")"

cleanup() {
  case "$tmp" in
    "$tmp_parent"/perf-postgres-readiness-test.*) rm -rf -- "$tmp" ;;
    *) printf 'refusing to remove unexpected test directory: %s\n' "$tmp" >&2; return 1 ;;
  esac
}
trap cleanup EXIT

mkdir -p "$tmp/bin" "$tmp/.github/scripts" "$tmp/cache"
cp "$script_dir/perf-postgres.sh" "$tmp/.github/scripts/perf-postgres.sh"
cat > "$tmp/.github/scripts/pull-base-images.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == --image && "$2" == mirror.gcr.io/postgres:18-alpine ]]
SH
cat > "$tmp/bin/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '<%s>' "$@" >> "$PERF_DOCKER_CALLS"
printf '\n' >> "$PERF_DOCKER_CALLS"
SH
chmod +x "$tmp/bin/docker"

export PERF_PG_CONTAINER=perf-pg
export PERF_CACHE="$tmp/cache"
export PERF_DOCKER_CALLS="$tmp/docker-calls"

(
  cd "$tmp"
  PATH="$tmp/bin:$PATH" bash .github/scripts/perf-postgres.sh
)

expected='<exec><perf-pg><pg_isready><-h><127.0.0.1><-U><postgres><-q>'
if ! grep -Fxq -- "$expected" "$PERF_DOCKER_CALLS"; then
  printf 'Postgres readiness must probe TCP, not the socket-only init server\n' >&2
  cat "$PERF_DOCKER_CALLS" >&2
  exit 1
fi

printf '%s\n' 'perf-postgres waits for the TCP-ready server'
