#!/bin/sh
# Forced command for the Multica agent key on teagram-k3s. Keep all operations
# scoped to the teagram namespace; agents never receive an interactive root shell.
set -eu
set -f

COMMAND=${SSH_ORIGINAL_COMMAND:-}
# Split the fixed menu command into arguments; glob expansion is disabled above.
# shellcheck disable=SC2086
set -- $COMMAND
VERB=${1:-help}
[ $# -eq 0 ] || shift

usage() {
  cat >&2 <<'EOF'
teagram-ops verbs (namespace: teagram):
  status
  logs <pod>
  restart <teagramd|postgres|rustfs|linklanding|linkselector|cloudflared>
  rollout history <teagramd|postgres|rustfs|linklanding|linkselector|cloudflared>
EOF
}

workload_resource() {
  case "$1" in
    teagramd) printf '%s\n' statefulset/teagramd ;;
    postgres) printf '%s\n' statefulset/postgres ;;
    rustfs) printf '%s\n' statefulset/rustfs ;;
    linklanding|linkselector|cloudflared) printf 'deployment/%s\n' "$1" ;;
    *) return 1 ;;
  esac
}

case "$VERB" in
  status)
    [ $# -eq 0 ] || { usage; exit 64; }
    exec k3s kubectl get pods,deployments,statefulsets,jobs,services,persistentvolumeclaims \
      --namespace teagram --output wide
    ;;
  logs)
    [ $# -eq 1 ] || { usage; exit 64; }
    case "$1" in *[!a-z0-9.-]*|''|-*) usage; exit 64 ;; esac
    exec k3s kubectl logs --namespace teagram --all-containers=true \
      --prefix=true --tail=200 "$1"
    ;;
  restart)
    [ $# -eq 1 ] || { usage; exit 64; }
    resource=$(workload_resource "$1") || { usage; exit 64; }
    exec k3s kubectl rollout restart "$resource" --namespace teagram
    ;;
  rollout)
    [ $# -eq 2 ] && [ "$1" = history ] || { usage; exit 64; }
    resource=$(workload_resource "$2") || { usage; exit 64; }
    exec k3s kubectl rollout history "$resource" --namespace teagram
    ;;
  help|-h|--help) usage ;;
  *) usage; exit 64 ;;
esac
