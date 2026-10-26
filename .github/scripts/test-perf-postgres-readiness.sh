#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

cat >"$tmp/docker" <<'DOCKER'
#!/usr/bin/bash
set -euo pipefail
printf '%s\n' "$*" >>"${DOCKER_LOG:?}"
if [[ "${1:-}" == run ]]; then
  printf '%s\n' fake-container-id
  exit 0
fi
if [[ "${1:-}" == exec && "${3:-}" == pg_isready ]]; then
  if [[ " $* " == *" -h 127.0.0.1 "* ]]; then
    probe_count=0
    if [[ -f "${DOCKER_STATE:?}" ]]; then
      read -r probe_count <"$DOCKER_STATE"
    fi
    probe_count=$((probe_count + 1))
    printf '%s\n' "$probe_count" >"$DOCKER_STATE"
    if ((probe_count > 1)); then
      exit 0
    fi
    exit 1
  fi
  # The image's temporary init server can accept a socket probe before the
  # final server is ready for the TCP connection used by the performance gate.
  exit 0
fi
printf 'unexpected docker call: %s\n' "$*" >&2
exit 1
DOCKER

cat >"$tmp/bash" <<'BASH'
#!/usr/bin/bash
if [[ "${1:-}" == .github/scripts/pull-base-images.sh ]]; then
  exit 0
fi
exec /usr/bin/bash "$@"
BASH

chmod +x "$tmp/docker" "$tmp/bash"
docker_log="$tmp/docker.log"
PATH="$tmp:$PATH" DOCKER_LOG="$docker_log" DOCKER_STATE="$tmp/tcp-probes" PERF_PG_CONTAINER=perf-postgres-readiness-test \
  PERF_CACHE="$tmp/cache" /usr/bin/bash "$repo_root/.github/scripts/perf-postgres.sh" >/dev/null

tcp_probe=false
while IFS= read -r call; do
  if [[ "$call" == "exec perf-postgres-readiness-test pg_isready -h 127.0.0.1 -U postgres -q" ]]; then
    tcp_probe=true
    break
  fi
done <"$docker_log"

if [[ "$tcp_probe" != true ]]; then
  printf '%s\n' 'Postgres readiness must probe TCP, not the temporary init socket' >&2
  exit 1
fi
