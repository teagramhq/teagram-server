#!/usr/bin/env bash
set -euo pipefail

script="${PERF_POSTGRES_SCRIPT:-.github/scripts/perf-postgres.sh}"
probe='pg_isready -h 127.0.0.1'

if ! grep -Fq "$probe" "$script"; then
  printf 'Postgres readiness must probe TCP to avoid the init-only Unix socket: %s\n' "$script" >&2
  exit 1
fi

printf 'Postgres readiness probes the serving TCP listener\n'
