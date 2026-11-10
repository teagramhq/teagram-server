#!/usr/bin/env bash
set -euo pipefail

if (($# == 0)); then
  printf 'usage: %s <kubectl-command> [args...]: kubectl-command auth can-i ...\n' "$0" >&2
  exit 64
fi

kubectl_command=("$@")
agent=system:serviceaccount:teagram:teagram-agent
namespace=teagram
temp_error=$(mktemp)
trap 'rm -f -- "$temp_error"' EXIT

expect_can_i() {
  local expected=$1 verb=$2 resource=$3 answer status
  shift 3
  if answer=$("${kubectl_command[@]}" auth can-i "$verb" "$resource" \
    --as="$agent" "$@" 2>"$temp_error"); then
    status=0
  else
    status=$?
  fi
  case "$expected:$status:$answer" in
    yes:0:yes|no:1:no) ;;
    *)
      printf 'unexpected agent authorization: %s %s => %s:%s\n' \
        "$verb" "$resource" "$status" "$answer" >&2
      if [[ -s $temp_error ]]; then
        cat "$temp_error" >&2
      fi
      return 1
      ;;
  esac
}

readonly -a read_verbs=(get list watch)
readonly -a all_verbs=(get list watch create update patch delete deletecollection '*')

for resource in pods events services endpoints; do
  for verb in "${read_verbs[@]}"; do
    expect_can_i yes "$verb" "$resource" --namespace "$namespace"
  done
done
for verb in "${read_verbs[@]}"; do
  expect_can_i yes "$verb" pods --subresource log --namespace "$namespace"
done
for resource in deployments.apps statefulsets.apps replicasets.apps jobs.batch cronjobs.batch; do
  for verb in "${read_verbs[@]}"; do
    expect_can_i yes "$verb" "$resource" --namespace "$namespace"
  done
done
for verb in get list; do
  expect_can_i yes "$verb" controllerrevisions.apps --namespace "$namespace"
done
expect_can_i yes delete pods --namespace "$namespace"

for resource in pods events services endpoints deployments.apps statefulsets.apps \
  replicasets.apps controllerrevisions.apps jobs.batch cronjobs.batch secrets \
  configmaps persistentvolumeclaims roles.rbac.authorization.k8s.io \
  rolebindings.rbac.authorization.k8s.io serviceaccounts; do
  for verb in create patch update; do
    expect_can_i no "$verb" "$resource" --namespace "$namespace"
  done
done
for verb in create patch update; do
  expect_can_i no "$verb" pods --subresource log --namespace "$namespace"
done

for verb in "${all_verbs[@]}"; do
  expect_can_i no "$verb" secrets --namespace "$namespace"
  for subresource in exec attach portforward eviction proxy; do
    expect_can_i no "$verb" pods --subresource "$subresource" --namespace "$namespace"
  done
  expect_can_i no "$verb" services --subresource proxy --namespace "$namespace"
done
expect_can_i no deletecollection pods --namespace "$namespace"
expect_can_i no '*' '*' --namespace "$namespace"
expect_can_i no '*' '*'
for resource in namespaces nodes persistentvolumes storageclasses \
  clusterroles.rbac.authorization.k8s.io clusterrolebindings.rbac.authorization.k8s.io; do
  for verb in "${all_verbs[@]}"; do
    expect_can_i no "$verb" "$resource"
  done
done
for other_namespace in default kube-system; do
  expect_can_i no '*' '*' --namespace "$other_namespace"
  for resource in pods secrets deployments.apps; do
    for verb in "${all_verbs[@]}"; do
      expect_can_i no "$verb" "$resource" --namespace "$other_namespace"
    done
  done
done

printf 'agent RBAC matrix passed: read-only namespace access and pod delete only\n'
