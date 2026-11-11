#!/usr/bin/env bash
set -euo pipefail

readonly INSTANCE=teagram-k3s
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

  --dry-run  Validate access manifests and show the API egress allowlist.
  --apply    Install the proxy ServiceAccount/RBAC and create its empty state Secret once.
  --rollback Stop the proxy and remove proxy access; preserve Tailscale state and deployer access.

Run on alpha. Kubernetes administration uses `incus exec teagram-k3s -- k3s kubectl`;
the script does not create or require a cluster-admin kubeconfig. It never creates
the external tailscale-auth Secret and never reads or prints Secret contents.
The API egress policy is restricted to the current Kubernetes API endpoint IPs.
EOF
}

kubectl() {
  local -a args=("$@")
  local index filename
  for ((index = 0; index < ${#args[@]}; index++)); do
    case "${args[index]}" in
      --filename|-f)
        ((index + 1 < ${#args[@]})) || fail 'kubectl filename flag has no value'
        filename=${args[index + 1]}
        [[ $filename != - ]] || break
        [[ -r $filename ]] || fail "kubectl input file is not readable: $filename"
        args[index + 1]=-
        incus exec --mode=non-interactive "$INSTANCE" -- k3s kubectl "${args[@]}" < "$filename"
        return
        ;;
      --filename=*|-f=*)
        filename=${args[index]#*=}
        [[ $filename != - ]] || break
        [[ -r $filename ]] || fail "kubectl input file is not readable: $filename"
        args[index]=--filename=-
        incus exec --mode=non-interactive "$INSTANCE" -- k3s kubectl "${args[@]}" < "$filename"
        return
        ;;
    esac
  done
  incus exec --mode=non-interactive "$INSTANCE" -- k3s kubectl "${args[@]}"
}

[[ $# -eq 1 ]] || { usage >&2; exit 64; }
case "$1" in
  --dry-run|--apply|--rollback) mode=$1 ;;
  --help|-h) usage; exit 0 ;;
  *) usage >&2; exit 64 ;;
esac

command -v incus >/dev/null 2>&1 || fail 'incus is required'
command -v python3 >/dev/null 2>&1 || fail 'python3 is required'
[[ -f "$ACCESS_MANIFEST" ]] || fail "missing reviewed manifest: $ACCESS_MANIFEST"
readonly API_EGRESS_RENDERER="$SCRIPT_DIR/render-tailnet-proxy-api-egress.py"
[[ -f "$API_EGRESS_RENDERER" ]] || fail "missing reviewed renderer: $API_EGRESS_RENDERER"
incus info "$INSTANCE" >/dev/null || fail "Incus instance $INSTANCE is unavailable"
context=$(kubectl config current-context)
[[ -n "$context" ]] || fail 'kubectl has no current context'
printf 'Target context: %s\n' "$context"
kubectl get namespace "$NAMESPACE" >/dev/null || fail "namespace $NAMESPACE is unavailable"

api_egress_manifest() {
  kubectl get endpoints kubernetes --namespace default --output=json |
    python3 "$API_EGRESS_RENDERER"
}

case "$mode" in
  --dry-run)
    kubectl apply --dry-run=server --filename "$ACCESS_MANIFEST"
    api_policy=$(api_egress_manifest) || fail 'could not render the Kubernetes API egress policy'
    printf '%s\n' "$api_policy"
    printf '%s\n' "$api_policy" | kubectl apply --dry-run=server --filename=-
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
    api_policy=$(api_egress_manifest) || fail 'could not render the Kubernetes API egress policy'
    printf '%s\n' "$api_policy" | kubectl apply --dry-run=server --filename=- >/dev/null
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
    printf '%s\n' "$api_policy" | kubectl apply --filename=-
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
    kubectl delete rolebinding tailscale-proxy-state \
      --namespace "$NAMESPACE" --ignore-not-found=true
    kubectl delete role tailscale-proxy-state \
      --namespace "$NAMESPACE" --ignore-not-found=true
    kubectl delete networkpolicy tailscale-proxy-api-egress \
      --namespace "$NAMESPACE" --ignore-not-found=true
    kubectl delete serviceaccount tailscale-proxy --namespace "$NAMESPACE" \
      --ignore-not-found=true
    printf 'Proxy access is removed. Tailscale Secrets and deployer NetworkPolicy access were preserved.\n'
    ;;
esac
