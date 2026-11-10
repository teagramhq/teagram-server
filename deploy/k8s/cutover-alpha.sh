#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: cutover-alpha.sh --dry-run | --apply | --rollback

  --dry-run  Confirm the retired Compose cutover performs no actions.
  --apply    Refuse the retired cutover path.
  --rollback Refuse the retired cutover path; no LXC edge remains to restore.
EOF
}

if [[ $# -ne 1 ]]; then
  usage >&2
  exit 64
fi

case "$1" in
  --dry-run)
    cat <<'EOF'
The source teagram LXC was deleted. The retired Compose cutover performs no actions.
Use the in-cluster Tailscale proxy procedure in deploy/k8s/README.md for tailnet access.
EOF
    ;;
  --apply|--rollback)
    printf 'k3s cutover: production cutover is retired; use the in-cluster ingress procedure in deploy/k8s/README.md\n' >&2
    exit 1
    ;;
  --help|-h) usage ;;
  *) usage >&2; exit 64 ;;
esac
