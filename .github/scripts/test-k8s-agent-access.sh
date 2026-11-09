#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

fail() {
  printf 'K8s agent access test: %s\n' "$1" >&2
  exit 1
}

namespace=teagram
agent=system:serviceaccount:teagram:teagram-agent

kubectl apply --dry-run=server --filename deploy/k8s/agent-access.yaml >/dev/null
kubectl apply --filename deploy/k8s/deployer-access.yaml >/dev/null
kubectl apply --filename deploy/k8s/agent-access.yaml >/dev/null
kubectl apply --dry-run=server --filename deploy/k8s/agent-pod-delete-policy.yaml >/dev/null
kubectl apply --filename deploy/k8s/agent-pod-delete-policy.yaml >/dev/null

bash deploy/k8s/check-agent-rbac.sh kubectl
[[ "$(kubectl get serviceaccount teagram-agent --namespace "$namespace" \
  --output jsonpath='{.automountServiceAccountToken}')" == false ]] ||
  fail 'the agent ServiceAccount must disable token automount'
[[ -z "$(kubectl get secret teagram-agent-token --namespace "$namespace" \
  --ignore-not-found --output name)" ]] ||
  fail 'the agent must not receive a service-account-token Secret'

kubectl wait --for=create pod/postgres-0 --namespace "$namespace" --timeout=180s >/dev/null
kubectl wait --for=create pods --selector=app.kubernetes.io/name=linkselector \
  --namespace "$namespace" --timeout=180s >/dev/null
linkselector_pod=$(kubectl get pods --selector=app.kubernetes.io/name=linkselector \
  --namespace "$namespace" --output jsonpath='{.items[0].metadata.name}')
[[ -n $linkselector_pod ]] || fail 'the linkselector pod was not created'

kubectl --as="$agent" delete pod "$linkselector_pod" --dry-run=server \
  --namespace "$namespace" >/dev/null

if output=$(kubectl --as="$agent" delete pod postgres-0 --dry-run=server \
  --namespace "$namespace" 2>&1); then
  fail 'the agent admission policy accepted deletion of postgres-0'
fi
grep --fixed-strings --quiet 'teagram-agent may delete only controller-owned' <<< "$output" || {
  printf '%s\n' "$output" >&2
  fail 'postgres-0 was denied for a reason other than the agent admission policy'
}

# The username condition keeps the policy out of deployer and administrator requests.
if output=$(kubectl --as=system:serviceaccount:teagram:teagram-deployer \
  delete pod postgres-0 --dry-run=server --namespace "$namespace" 2>&1); then
  fail 'the deployer unexpectedly gained pod-delete access'
fi
grep --fixed-strings --quiet 'cannot delete resource "pods"' <<< "$output" || {
  printf '%s\n' "$output" >&2
  fail 'the deployer request was denied by the agent admission policy instead of RBAC'
}
if grep --fixed-strings --quiet 'teagram-agent may delete only' <<< "$output"; then
  fail 'the agent admission policy affected a deployer request'
fi
kubectl delete pod postgres-0 --dry-run=server --namespace "$namespace" >/dev/null
kubectl get pod "$linkselector_pod" postgres-0 --namespace "$namespace" >/dev/null

printf 'agent pod-delete admission passed: linkselector allowed, postgres denied, admin unaffected\n'
