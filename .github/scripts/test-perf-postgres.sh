#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
tmp=$(mktemp -d)
cleanup() {
  rm -rf -- "$tmp"
}
trap cleanup EXIT

mkdir -p "$tmp/bin" "$tmp/cache"
printf '0\n' > "$tmp/tcp-probes"
cat > "$tmp/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  pull|run) exit 0 ;;
  exec)
    shift
    [[ "${1:-}" == "$PERF_PG_CONTAINER" ]] || { echo "unexpected docker exec: $*" >&2; exit 99; }
    shift
    [[ "${1:-}" == pg_isready ]] || { echo "unexpected docker command: $*" >&2; exit 99; }
    shift
    if [[ " $* " != *' -h 127.0.0.1 '* ]]; then
      # The official image's temporary init server accepts socket probes.
      exit 0
    fi
    probes=$(<"$PERF_TCP_PROBES")
    probes=$((probes + 1))
    printf '%s\n' "$probes" > "$PERF_TCP_PROBES"
    [[ "$probes" != 1 ]]
    ;;
  *) echo "unexpected docker command: $*" >&2; exit 99 ;;
esac
DOCKER
cat > "$tmp/bin/sleep" <<'SLEEP'
#!/usr/bin/env bash
exit 0
SLEEP
chmod 755 "$tmp/bin/docker" "$tmp/bin/sleep"

export PATH="$tmp/bin:$PATH"
export PERF_PG_CONTAINER=perf-pg PERF_CACHE="$tmp/cache" PERF_TCP_PROBES="$tmp/tcp-probes"
bash "$script_dir/perf-postgres.sh"

tcp_probes=$(<"$PERF_TCP_PROBES")
if [[ "$tcp_probes" != 2 ]]; then
  printf 'expected TCP readiness to wait through the socket-only startup phase, got %s TCP probes\n' \
    "$tcp_probes" >&2
  exit 1
fi
printf '%s\n' 'performance Postgres startup readiness cases pass'
