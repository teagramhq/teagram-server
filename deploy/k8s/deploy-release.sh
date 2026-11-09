#!/usr/bin/env bash
set -Eeuo pipefail

if (($# != 2)); then
  printf 'usage: %s <rendered-manifest> <source-sha>\n' "$0" >&2
  exit 2
fi

manifest="$1"
revision="$2"
if [[ ! -s "$manifest" || ! "$revision" =~ ^[0-9a-f]{40}$ ]]; then
  printf 'deploy-release: pass a manifest and full lowercase source SHA\n' >&2
  exit 2
fi

namespace=teagram
rollout_timeout="${ROLLOUT_TIMEOUT:-300s}"
apply_started=false
declare -A baseline_revisions=()
readonly controllers=(
  statefulset/postgres
  statefulset/rustfs
  statefulset/teagramd
  deployment/linklanding
  deployment/linkselector
  deployment/cloudflared
)

controller_revision() {
  local resource="$1" kind="$2" revision_name
  if [[ "$kind" == statefulset ]]; then
    revision_name="$(kubectl get "$resource" --namespace "$namespace" \
      --output 'jsonpath={.status.updateRevision}')"
    [[ -n "$revision_name" ]] || return 1
    kubectl get controllerrevision "$revision_name" --namespace "$namespace" \
      --output 'jsonpath={.revision}'
  else
    kubectl get "$resource" --namespace "$namespace" \
      --output 'jsonpath={.metadata.annotations.deployment\.kubernetes\.io/revision}'
  fi
}

rollback() {
  local status=$? resource kind baseline current rollback_failed=0
  trap - EXIT
  if ((status != 0)) && [[ "$apply_started" == true ]]; then
    printf 'deploy=failed sha=%s; restoring previous workload revisions\n' "$revision" >&2
    for resource in "${controllers[@]}"; do
      kind="${resource%%/*}"
      baseline="${baseline_revisions[$resource]:-}"
      if [[ -z "$baseline" ]]; then
        printf 'rollback=unavailable resource=%s reason=no-baseline\n' "$resource" >&2
        rollback_failed=1
        continue
      fi
      if ! current="$(controller_revision "$resource" "$kind" 2>/dev/null)"; then
        printf 'rollback=failed resource=%s reason=revision-unreadable\n' "$resource" >&2
        rollback_failed=1
        continue
      fi
      if [[ "$current" == "$baseline" ]]; then
        continue
      fi
      if ! kubectl rollout undo "$resource" --namespace "$namespace" \
        --to-revision="$baseline"; then
        printf 'rollback=failed resource=%s reason=undo-failed\n' "$resource" >&2
        rollback_failed=1
        continue
      fi
      if ! kubectl rollout status "$resource" --namespace "$namespace" \
        --timeout="$rollout_timeout"; then
        printf 'rollback=failed resource=%s reason=rollout-not-ready\n' "$resource" >&2
        rollback_failed=1
      else
        printf 'rollback=verified resource=%s revision=%s\n' "$resource" "$baseline" >&2
      fi
    done
    if ((rollback_failed != 0)); then
      printf '::error::automatic rollback did not restore every changed controller\n' >&2
    fi
  fi
  exit "$status"
}
trap rollback EXIT
trap 'printf "deploy-release failed at line %s: %s\\n" "$LINENO" "$BASH_COMMAND" >&2' ERR

kubectl apply --dry-run=server --filename "$manifest"
for resource in "${controllers[@]}"; do
  kind="${resource%%/*}"
  baseline="$(controller_revision "$resource" "$kind")"
  [[ "$baseline" =~ ^[1-9][0-9]*$ ]] || {
    printf 'deploy-release: no stable prior revision for %s\n' "$resource" >&2
    exit 1
  }
  baseline_revisions["$resource"]="$baseline"
done

apply_started=true
kubectl apply --filename "$manifest"
kubectl wait --for=condition=complete jobs \
  --selector=app.kubernetes.io/name=db-migrations \
  --namespace "$namespace" --timeout=900s
kubectl wait --for=condition=complete jobs \
  --selector=app.kubernetes.io/name=rustfs-init \
  --namespace "$namespace" --timeout=600s
for resource in "${controllers[@]}"; do
  kubectl rollout status "$resource" --namespace "$namespace" \
    --timeout="$rollout_timeout"
done
cloudflared_replicas="$(kubectl get deployment cloudflared --namespace "$namespace" \
  --output 'jsonpath={.spec.replicas}')"
cloudflared_ready="$(kubectl get deployment cloudflared --namespace "$namespace" \
  --output 'jsonpath={.status.readyReplicas}')"
if [[ "$cloudflared_replicas" != 1 || "$cloudflared_ready" != 1 ]]; then
  printf 'deploy-release: expected exactly one ready k3s tunnel connector (replicas=%s ready=%s)\n' \
    "${cloudflared_replicas:-0}" "${cloudflared_ready:-0}" >&2
  exit 1
fi
bash "$(dirname -- "${BASH_SOURCE[0]}")/smoke-production.sh"
printf 'rollout=verified sha=%s\n' "$revision"
