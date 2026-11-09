#!/usr/bin/env bash
set -Eeuo pipefail

if (($# != 1)); then
  printf 'usage: %s <teagramd-image-reference>\n' "$0" >&2
  exit 2
fi

image="$1"
if [[ ! "$image" =~ ^ghcr\.io/teagramhq/teagramd(@sha256:[0-9a-f]{64}|:ci)$ ]]; then
  printf 'expiring-summary maintenance: pass the verified teagramd image\n' >&2
  exit 2
fi

namespace=teagram
job_timeout_seconds=900
repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
template="$repo_root/deploy/k8s/initialize-channel-post-expiring-summaries-job.yaml"
image_placeholder='image: ghcr.io/teagramhq/teagramd:unreleased'
placeholder_count="$(grep --fixed-strings --count "$image_placeholder" "$template")"
[[ "$placeholder_count" == 1 ]] || {
  printf 'expiring-summary maintenance: expected one image placeholder in %s\n' "$template" >&2
  exit 1
}

tmp_dir="$(mktemp -d "${TMPDIR:-${RUNNER_TEMP:-/tmp}}/expiring-summary.XXXXXX")"
cleanup() {
  local status=$?
  trap - EXIT
  rm -rf -- "$tmp_dir"
  exit "$status"
}
trap cleanup EXIT
manifest="$tmp_dir/job.yaml"
sed "s|$image_placeholder|image: $image|" "$template" > "$manifest"
if grep --fixed-strings --quiet "$image_placeholder" "$manifest"; then
  printf 'expiring-summary maintenance: image placeholder was not replaced\n' >&2
  exit 1
fi

wait_for_job() {
  local job="$1" deadline=$((SECONDS + job_timeout_seconds)) conditions
  while ((SECONDS < deadline)); do
    if ! conditions="$(kubectl get "job/$job" --namespace "$namespace" \
      --output 'jsonpath={range .status.conditions[*]}{.type}={.status}{"\n"}{end}' 2>&1)"; then
      printf 'expiring-summary maintenance: could not inspect job %s: %s\n' \
        "$job" "$conditions" >&2
      return 3
    fi
    if grep --fixed-strings --line-regexp --quiet 'Complete=True' <<< "$conditions"; then
      return 0
    fi
    if grep --fixed-strings --line-regexp --quiet 'Failed=True' <<< "$conditions"; then
      return 1
    fi
    sleep 5
  done
  return 2
}

stop_running_job() {
  local job="$1" active deadline=$((SECONDS + 60))
  if ! kubectl patch "job/$job" --namespace "$namespace" --type=merge \
    --patch '{"spec":{"suspend":true}}' >/dev/null; then
    printf 'expiring-summary maintenance: could not suspend timed-out job %s\n' "$job" >&2
    return 1
  fi
  while ((SECONDS < deadline)); do
    if ! active="$(kubectl get "job/$job" --namespace "$namespace" \
      --output 'jsonpath={.status.active}' 2>&1)"; then
      printf 'expiring-summary maintenance: could not verify job %s stopped: %s\n' \
        "$job" "$active" >&2
      return 1
    fi
    if [[ -z "$active" || "$active" == 0 ]]; then
      return 0
    fi
    sleep 5
  done
  printf 'expiring-summary maintenance: job %s still has active pods after suspension\n' \
    "$job" >&2
  return 1
}

batch=0
remaining=''
previous_remaining=''
while [[ "$remaining" != 0 ]]; do
  ((batch += 1))
  if ! job_name="$(kubectl create --filename "$manifest" \
    --output 'jsonpath={.metadata.name}')"; then
    printf 'expiring-summary maintenance: could not create batch job\n' >&2
    exit 1
  fi
  if [[ ! "$job_name" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]; then
    printf 'expiring-summary maintenance: Kubernetes returned an invalid job name\n' >&2
    exit 1
  fi
  printf 'expiring-summary maintenance=batch-start batch=%s job=%s\n' "$batch" "$job_name"

  wait_status=0
  if wait_for_job "$job_name"; then
    wait_status=0
  else
    wait_status=$?
  fi
  if ((wait_status != 0)); then
    if ((wait_status == 2 || wait_status == 3)); then
      stop_running_job "$job_name" || true
    fi
    kubectl logs "job/$job_name" --namespace "$namespace" --all-containers=true || true
    kubectl describe "job/$job_name" --namespace "$namespace" || true
    if ((wait_status == 1)); then
      printf 'expiring-summary maintenance: batch job %s failed\n' "$job_name" >&2
    else
      printf 'expiring-summary maintenance: batch job %s did not finish successfully\n' \
        "$job_name" >&2
    fi
    exit 1
  fi

  if ! output="$(kubectl logs "job/$job_name" --namespace "$namespace" \
    --all-containers=true)"; then
    printf 'expiring-summary maintenance: could not read batch job %s logs\n' "$job_name" >&2
    exit 1
  fi
  printf '%s\n' "$output"
  mapfile -t results < <(sed -nE \
    's/^Channel post expiring summaries initialized: ([0-9]+) channels, ([0-9]+) remaining$/\1 \2/p' \
    <<< "$output")
  if ((${#results[@]} != 1)); then
    printf 'expiring-summary maintenance: job %s did not report exactly one remaining count\n' \
      "$job_name" >&2
    exit 1
  fi
  read -r initialized remaining <<< "${results[0]}"
  if [[ ! "$initialized" =~ ^[0-9]+$ || ! "$remaining" =~ ^[0-9]+$ ]]; then
    printf 'expiring-summary maintenance: job %s reported invalid counts\n' "$job_name" >&2
    exit 1
  fi
  if ((remaining > 0 && initialized == 0)); then
    printf 'expiring-summary maintenance: %s channels remain but this batch initialized none\n' \
      "$remaining" >&2
    exit 1
  fi
  if [[ -n "$previous_remaining" ]] && ((remaining > 0 && remaining >= previous_remaining)); then
    printf 'expiring-summary maintenance: pending channels did not decrease (%s remain)\n' \
      "$remaining" >&2
    exit 1
  fi
  previous_remaining="$remaining"
  printf 'expiring-summary maintenance=batch-complete batch=%s initialized=%s remaining=%s\n' \
    "$batch" "$initialized" "$remaining"
done

printf 'expiring-summary maintenance=verified remaining=0 batches=%s\n' "$batch"
