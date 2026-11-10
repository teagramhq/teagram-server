#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

plan=$(deploy/k8s/cutover-alpha.sh --dry-run)
[[ $plan == *'source teagram LXC was deleted'* && $plan == *'performs no actions'* ]] || {
  printf 'retired cutover dry-run did not explain that it is disabled\n' >&2
  exit 1
}
if [[ $plan == *'Tailscale Serve'* || $plan == *'port-forward'* || $plan == *'incus exec'* ]]; then
  printf 'retired cutover dry-run still describes a host route change\n' >&2
  exit 1
fi

if output=$(deploy/k8s/cutover-alpha.sh --apply 2>&1); then
  printf 'retired production cutover unexpectedly succeeded\n' >&2
  exit 1
fi
[[ $output == *'production cutover is retired'* &&
   $output == *'in-cluster ingress procedure'* ]] || {
  printf 'retired cutover did not refuse production changes with the new runbook location\n' >&2
  exit 1
}

printf 'retired alpha cutover guard passed\n'
