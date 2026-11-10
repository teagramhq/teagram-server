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
declare -A baseline_replicas=()
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

controller_replicas() {
  local resource="$1"
  kubectl get "$resource" --namespace "$namespace" --output json |
    jq --raw-output --exit-status '.spec.replicas // 1 | numbers'
}

controller_is_stable() {
  local resource="$1" kind="$2"
  kubectl get "$resource" --namespace "$namespace" --output json |
    jq --exit-status --arg kind "$kind" '
      (.spec.replicas // 1) as $desired
      | (.metadata.generation // 0) as $generation
      | (.status.observedGeneration // 0) >= $generation
        and (.status.replicas // 0) == $desired
        and (.status.updatedReplicas // 0) == $desired
        and (.status.readyReplicas // 0) == $desired
        and (.status.availableReplicas // $desired) == $desired
        and ($kind != "statefulset" or
          ((.status.currentRevision // "") != "" and
           .status.currentRevision == .status.updateRevision))
    ' >/dev/null
}

rendered_job_name() {
  local resources="$1" prefix="$2" matches
  matches="$(grep --extended-regexp --only-matching \
    "^job(\\.batch)?/${prefix}-[a-z0-9]+$" <<< "$resources" || true)"
  if [[ -z "$matches" || "$matches" == *$'\n'* ]]; then
    printf 'deploy-release: expected exactly one rendered %s Job\n' "$prefix" >&2
    return 1
  fi
  printf '%s\n' "${matches#*/}"
}

print_job_logs() {
  local job="$1"
  kubectl logs "job/$job" --namespace "$namespace" --all-containers=true >&2 || true
}

prepare_failed_job_retry() {
  local job="$1" job_json
  if ! job_json="$(kubectl get "job/$job" --namespace "$namespace" \
    --ignore-not-found=true --output json)"; then
    printf 'deploy-release: could not inspect existing Job %s before apply\n' "$job" >&2
    return 1
  fi
  [[ -n "$job_json" ]] || return 0
  if ! jq --exit-status 'type == "object" and .kind == "Job"' \
    <<< "$job_json" >/dev/null; then
    printf 'deploy-release: invalid API response while inspecting Job %s\n' "$job" >&2
    return 1
  fi
  if ! jq --exit-status \
    'any(.status.conditions[]?; .type == "Failed" and .status == "True")' \
    <<< "$job_json" >/dev/null; then
    return 0
  fi

  printf 'deploy-release: previous Job %s failed; logs follow before retry cleanup\n' \
    "$job" >&2
  print_job_logs "$job"
  # Use the existing patch permission; the deployer does not need Job delete access.
  if ! kubectl patch "job/$job" --namespace "$namespace" --type=merge \
    --patch '{"spec":{"ttlSecondsAfterFinished":0}}'; then
    printf 'deploy-release: could not expire failed Job %s for retry\n' "$job" >&2
    return 1
  fi
  if ! kubectl wait --for=delete "job/$job" --namespace "$namespace" \
    --timeout=60s; then
    printf 'deploy-release: failed Job %s did not expire for retry\n' "$job" >&2
    return 1
  fi
  printf 'deploy-release: expired failed Job %s; the next apply can recreate it\n' \
    "$job" >&2
}

wait_for_job() {
  local job="$1" timeout_seconds="$2" deadline job_json
  deadline=$((SECONDS + timeout_seconds))
  while ((SECONDS < deadline)); do
    if ! job_json="$(kubectl get "job/$job" --namespace "$namespace" \
      --output json 2>&1)"; then
      printf 'deploy-release: could not inspect Job %s: %s\n' "$job" "$job_json" >&2
      return 1
    fi
    if ! jq --exit-status 'type == "object" and .kind == "Job"' \
      <<< "$job_json" >/dev/null; then
      printf 'deploy-release: invalid API response while waiting for Job %s\n' "$job" >&2
      return 1
    fi
    if jq --exit-status \
      'any(.status.conditions[]?; .type == "Complete" and .status == "True")' \
      <<< "$job_json" >/dev/null; then
      return 0
    fi
    if jq --exit-status \
      'any(.status.conditions[]?; .type == "Failed" and .status == "True")' \
      <<< "$job_json" >/dev/null; then
      printf 'deploy-release: Job %s failed; logs follow\n' "$job" >&2
      print_job_logs "$job"
      return 1
    fi
    sleep 5
  done
  printf 'deploy-release: timed out waiting for Job %s; logs follow\n' "$job" >&2
  print_job_logs "$job"
  return 1
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
  local baseline_count current_count
  local restored_template restored_replicas rollback_failed=0
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
      baseline_count="${baseline_replicas[$resource]:-}"
      if [[ ! "$baseline_count" =~ ^[0-9]+$ ]]; then
        printf 'rollback=unavailable resource=%s reason=no-replica-baseline\n' \
          "$resource" >&2
        rollback_failed=1
        continue
      fi
      if ! current_count="$(controller_replicas "$resource" 2>/dev/null)"; then
        printf 'rollback=failed resource=%s reason=replica-count-unreadable\n' "$resource" >&2
        rollback_failed=1
        continue
      fi
      if [[ "$current_template" == "$baseline_template" &&
        "$current_count" == "$baseline_count" ]]; then
        continue
      fi
      if [[ "$current_template" != "$baseline_template" ]]; then
        if ! kubectl rollout undo "$resource" --namespace "$namespace" \
          --to-revision="$baseline"; then
          printf 'rollback=failed resource=%s reason=undo-failed\n' "$resource" >&2
          rollback_failed=1
          continue
        fi
      fi
      if [[ "$current_count" != "$baseline_count" ]] &&
        ! kubectl patch "$resource" --namespace "$namespace" --type=merge \
          --patch "{\"spec\":{\"replicas\":$baseline_count}}"; then
        printf 'rollback=failed resource=%s reason=replica-count-restore-failed\n' \
          "$resource" >&2
        rollback_failed=1
        continue
      fi
      if [[ "$kind" == statefulset && "$current_template" != "$baseline_template" ]]; then
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
      restored_replicas="$(controller_replicas "$resource" 2>/dev/null)" || {
        printf 'rollback=failed resource=%s reason=replica-count-unreadable\n' "$resource" >&2
        rollback_failed=1
        continue
      }
      if [[ "$restored_template" != "$baseline_template" ||
        "$restored_replicas" != "$baseline_count" ]]; then
        printf 'rollback=failed resource=%s reason=spec-not-restored\n' \
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

rendered_resources="$(kubectl apply --dry-run=server --filename "$manifest" --output name)"
migration_job="$(rendered_job_name "$rendered_resources" db-migrations)"
rustfs_job="$(rendered_job_name "$rendered_resources" rustfs-init)"
for resource in "${controllers[@]}"; do
  kind="${resource%%/*}"
  if ! controller_is_stable "$resource" "$kind"; then
    printf 'deploy-release: %s is not fully stable; refusing to record a rollback baseline\n' \
      "$resource" >&2
    exit 1
  fi
  baseline_replicas["$resource"]="$(controller_replicas "$resource")"
  [[ "${baseline_replicas[$resource]}" =~ ^[0-9]+$ ]] || {
    printf 'deploy-release: no valid replica baseline for %s\n' "$resource" >&2
    exit 1
  }
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

prepare_failed_job_retry "$migration_job"
prepare_failed_job_retry "$rustfs_job"
apply_started=true
applied_resources="$(kubectl apply --filename "$manifest" --output name)"
applied_migration_job="$(rendered_job_name "$applied_resources" db-migrations)"
applied_rustfs_job="$(rendered_job_name "$applied_resources" rustfs-init)"
[[ "$applied_migration_job" == "$migration_job" && "$applied_rustfs_job" == "$rustfs_job" ]] || {
  printf 'deploy-release: applied Job names did not match the server dry-run\n' >&2
  exit 1
}
wait_for_job "$migration_job" 900
wait_for_job "$rustfs_job" 600
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
