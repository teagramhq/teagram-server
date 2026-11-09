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
declare -A baseline_statefulset_hashes=()
declare -A baseline_templates=()
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

controller_template() {
  local resource="$1"
  kubectl get "$resource" --namespace "$namespace" --output json |
    jq --compact-output --sort-keys --exit-status \
      '.spec.template | select(type == "object")'
}

wait_for_statefulset_revision() {
  local resource="$1" expected_revision="$2" revision deadline=$((SECONDS + 60))
  while ((SECONDS < deadline)); do
    if ! revision="$(kubectl get "$resource" --namespace "$namespace" \
      --output 'jsonpath={.status.updateRevision}')"; then
      return 1
    fi
    if [[ "$revision" == "$expected_revision" ]]; then
      return 0
    fi
    sleep 1
  done
  printf 'rollback=failed resource=%s reason=statefulset-revision-not-restored expected=%s actual=%s\n' \
    "$resource" "$expected_revision" "${revision:-unknown}" >&2
  return 1
}

delete_stale_statefulset_pods() {
  local resource="$1" expected_revision="$2" name pods_json stale_pods pod
  name="${resource#statefulset/}"
  if ! pods_json="$(kubectl get pods --namespace "$namespace" --output json)"; then
    printf 'rollback=failed resource=%s reason=pods-unreadable\n' "$resource" >&2
    return 1
  fi
  if ! stale_pods="$(jq -r --arg name "$name" --arg revision "$expected_revision" '
    .items[]?
    | select(any(.metadata.ownerReferences[]?; .kind == "StatefulSet" and .name == $name))
    | select(.metadata.labels["controller-revision-hash"] != $revision)
    | .metadata.name
  ' <<< "$pods_json")"; then
    printf 'rollback=failed resource=%s reason=pods-unparseable\n' "$resource" >&2
    return 1
  fi
  [[ -n "$stale_pods" ]] || return 0
  while IFS= read -r pod; do
    [[ -n "$pod" ]] || continue
    printf 'rollback=replacing-stale-pod resource=%s pod=%s revision=%s\n' \
      "$resource" "$pod" "$expected_revision" >&2
    if ! kubectl delete pod "$pod" --namespace "$namespace" \
      --ignore-not-found=true --wait=true --timeout="$rollout_timeout"; then
      printf 'rollback=failed resource=%s pod=%s reason=stale-pod-delete-failed\n' \
        "$resource" "$pod" >&2
      return 1
    fi
  done <<< "$stale_pods"
}

rollback() {
  local status=$? resource kind baseline baseline_template current_template
  local restored_template rollback_failed=0
  trap - EXIT
  if ((status != 0)) && [[ "$apply_started" == true ]]; then
    printf 'deploy=failed sha=%s; restoring previous workload revisions\n' "$revision" >&2
    for resource in "${controllers[@]}"; do
      kind="${resource%%/*}"
      baseline="${baseline_revisions[$resource]:-}"
      baseline_template="${baseline_templates[$resource]:-}"
      if [[ -z "$baseline" ]]; then
        printf 'rollback=unavailable resource=%s reason=no-baseline\n' "$resource" >&2
        rollback_failed=1
        continue
      fi
      if [[ -z "$baseline_template" ]]; then
        printf 'rollback=unavailable resource=%s reason=no-template-baseline\n' \
          "$resource" >&2
        rollback_failed=1
        continue
      fi
      if ! current_template="$(controller_template "$resource" 2>/dev/null)"; then
        printf 'rollback=failed resource=%s reason=template-unreadable\n' "$resource" >&2
        rollback_failed=1
        continue
      fi
      if [[ "$current_template" == "$baseline_template" ]]; then
        continue
      fi
      if ! kubectl rollout undo "$resource" --namespace "$namespace" \
        --to-revision="$baseline"; then
        printf 'rollback=failed resource=%s reason=undo-failed\n' "$resource" >&2
        rollback_failed=1
        continue
      fi
      if [[ "$kind" == statefulset ]]; then
        local baseline_hash="${baseline_statefulset_hashes[$resource]:-}"
        if [[ -z "$baseline_hash" ]] ||
          ! wait_for_statefulset_revision "$resource" "$baseline_hash" ||
          ! delete_stale_statefulset_pods "$resource" "$baseline_hash"; then
          printf 'rollback=failed resource=%s reason=statefulset-pod-repair-failed\n' \
            "$resource" >&2
          rollback_failed=1
          continue
        fi
      fi
      if ! kubectl rollout status "$resource" --namespace "$namespace" \
        --timeout="$rollout_timeout"; then
        printf 'rollback=failed resource=%s reason=rollout-not-ready\n' "$resource" >&2
        rollback_failed=1
        continue
      fi
      if ! restored_template="$(controller_template "$resource" 2>/dev/null)"; then
        printf 'rollback=failed resource=%s reason=restored-template-unreadable\n' \
          "$resource" >&2
        rollback_failed=1
        continue
      fi
      if [[ "$restored_template" != "$baseline_template" ]]; then
        printf 'rollback=failed resource=%s reason=template-not-restored\n' \
          "$resource" >&2
        rollback_failed=1
        continue
      fi
      printf 'rollback=verified resource=%s revision=%s\n' "$resource" "$baseline" >&2
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
  if [[ "$kind" == statefulset ]]; then
    baseline_hash="$(kubectl get "$resource" --namespace "$namespace" \
      --output 'jsonpath={.status.updateRevision}')"
    [[ -n "$baseline_hash" ]] || {
      printf 'deploy-release: no stable prior revision for %s\n' "$resource" >&2
      exit 1
    }
    baseline_statefulset_hashes["$resource"]="$baseline_hash"
  fi
  if ! baseline_template="$(controller_template "$resource")"; then
    printf 'deploy-release: no stable prior template for %s\n' "$resource" >&2
    exit 1
  fi
  baseline_templates["$resource"]="$baseline_template"
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
bash "$(dirname -- "${BASH_SOURCE[0]}")/initialize-channel-post-expiring-summaries.sh" \
  "${TEAGRAMD_IMAGE:?TEAGRAMD_IMAGE must name the tested server image}"
printf 'rollout=verified sha=%s\n' "$revision"
