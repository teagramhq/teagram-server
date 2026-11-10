#!/usr/bin/env bash
set -euo pipefail

readonly NAMESPACE=teagram
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
readonly SCRIPT_DIR
readonly ACCESS_MANIFEST="$SCRIPT_DIR/tailscale-proxy-access.yaml"

fail() {
  printf 'tailnet proxy install: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: install-tailnet-proxy-access.sh --dry-run | --apply | --rollback

  --dry-run  Validate the namespace-scoped resources and print the target context.
  --apply    Install the proxy ServiceAccount/RBAC and create its empty state Secret once.
  --rollback Stop the proxy and remove the installed ServiceAccount/RBAC; preserve state.

Use only with the reviewed teagram k3s admin kubeconfig. This script never creates
the external tailscale-auth Secret and never reads or prints Secret contents.
EOF
}

[[ $# -eq 1 ]] || { usage >&2; exit 64; }
case "$1" in
  --dry-run|--apply|--rollback) mode=$1 ;;
  --help|-h) usage; exit 0 ;;
  *) usage >&2; exit 64 ;;
esac

command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required'
[[ -f "$ACCESS_MANIFEST" ]] || fail "missing reviewed manifest: $ACCESS_MANIFEST"
context=$(kubectl config current-context)
[[ -n "$context" ]] || fail 'kubectl has no current context'
printf 'Target context: %s\n' "$context"
kubectl get namespace "$NAMESPACE" >/dev/null || fail "namespace $NAMESPACE is unavailable"

case "$mode" in
  --dry-run)
    kubectl apply --dry-run=server --filename "$ACCESS_MANIFEST"
    if ! kubectl get secret teagram-tailscale-state --namespace "$NAMESPACE" \
      --ignore-not-found --output=name | grep --quiet .; then
      cat <<'YAML' | kubectl create --dry-run=server --filename=- --output=name
apiVersion: v1
kind: Secret
metadata:
  name: teagram-tailscale-state
  namespace: teagram
type: Opaque
YAML
    fi
    printf 'No resources were changed. Apply with --apply after tailscale-auth exists.\n'
    ;;
  --apply)
    kubectl get secret tailscale-auth --namespace "$NAMESPACE" \
      --output=jsonpath='{.data.authkey}' | grep --quiet . ||
      fail 'external Secret teagram/tailscale-auth must contain a non-empty authkey'
    kubectl apply --dry-run=server --filename "$ACCESS_MANIFEST" >/dev/null
    state_secret=$(kubectl get secret teagram-tailscale-state --namespace "$NAMESPACE" \
      --ignore-not-found --output=name)
    if [[ -z "$state_secret" ]]; then
      cat <<'YAML' | kubectl create --filename=-
apiVersion: v1
kind: Secret
metadata:
  name: teagram-tailscale-state
  namespace: teagram
type: Opaque
YAML
    fi
    kubectl apply --filename "$ACCESS_MANIFEST"
    ;;
  --rollback)
    if ! proxy_statefulset=$(kubectl get statefulset tailscale-proxy --namespace "$NAMESPACE" \
      --ignore-not-found --output=name); then
      fail 'could not determine whether StatefulSet tailscale-proxy exists'
    fi
    if [[ -n "$proxy_statefulset" ]]; then
      kubectl scale statefulset/tailscale-proxy --replicas=0 --namespace "$NAMESPACE"
      kubectl wait --for=delete pod/tailscale-proxy-0 --namespace "$NAMESPACE" --timeout=180s
    fi
    if ! proxy_pods=$(kubectl get pods --namespace "$NAMESPACE" \
      --selector=app.kubernetes.io/name=tailscale-proxy --output=name); then
      fail 'could not determine whether tailnet proxy pods remain'
    fi
    [[ -z "$proxy_pods" ]] || fail 'tailnet proxy pods remain; refusing to remove access'
    kubectl delete rolebinding tailscale-proxy-state teagram-deployer-networkpolicy \
      --namespace "$NAMESPACE" --ignore-not-found=true
    kubectl delete role tailscale-proxy-state teagram-deployer-networkpolicy \
      --namespace "$NAMESPACE" --ignore-not-found=true
    kubectl delete serviceaccount tailscale-proxy --namespace "$NAMESPACE" \
      --ignore-not-found=true
    printf 'Proxy access is removed. Both Tailscale Secrets were preserved for recovery.\n'
    ;;
esac
