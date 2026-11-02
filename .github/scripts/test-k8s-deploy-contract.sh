#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

fail() {
  printf 'K8s deployment contract: %s\n' "$1" >&2
  exit 1
}

require_no_match_status() {
  local status="$1"
  local description="$2"
  case "$status" in
    0) fail "$description unexpectedly matched" ;;
    1) return 0 ;;
    *) fail "$description check failed with status $status" ;;
  esac
}

# A missing command or parse error must not count as proof that a resource is absent.
if (require_no_match_status 127 'rg') >/dev/null 2>&1; then
  fail 'rg command errors must not pass an absence check'
fi
if (require_no_match_status 2 'yq') >/dev/null 2>&1; then
  fail 'yq parse errors must not pass an absence check'
fi

check_step="$(yq -r '.jobs."test-k8s-deploy".steps[] | select(.name == "Check RustFS initializer secret mounts") | .run' .github/workflows/ci.yml)"
[[ -n "$check_step" && "$check_step" != null ]] ||
  fail 'the RustFS secret mount check must remain in the deployment test'
[[ "$check_step" == *'kubectl wait --for=create jobs'*'kubectl get jobs'* ]] ||
  fail 'the RustFS Job must be awaited before its spec is inspected'
[[ "$check_step" == *'--selector=app.kubernetes.io/name=rustfs-init'* &&
   "$check_step" == *'--timeout=180s'* ]] ||
  fail 'the RustFS Job creation wait must target the initializer with a bounded timeout'

bootstrap_step="$(yq -r '.jobs."test-k8s-deploy".steps[] | select(.name == "Bootstrap the server identity explicitly") | .run' .github/workflows/ci.yml)"
[[ -n "$bootstrap_step" && "$bootstrap_step" != null &&
   "$bootstrap_step" == *'kubectl apply -k deploy/k8s/bootstrap/ci'* &&
   "$bootstrap_step" == *'kubectl wait --for=condition=complete job/teagramd-identity-bootstrap'* &&
   "$bootstrap_step" == *'kubectl scale statefulset/teagramd --replicas=0'* &&
   "$bootstrap_step" == *'kubectl scale statefulset/teagramd --replicas=1'* ]] ||
  fail 'the deployment test must run the separately applied first-install identity bootstrap Job'

bootstrap_job=deploy/k8s/bootstrap/ci/identity-job.yaml
[[ "$(yq -r '.spec.template.spec.containers[] | select(.name == "bootstrap-identity") | .command | join(" ")' "$bootstrap_job")" == '/usr/local/bin/teagramd bootstrap-identity' ]] ||
  fail 'the bootstrap Job must use the explicit bootstrap-identity command'
[[ "$(yq -r '.spec.template.spec.volumes[] | select(.name == "key-data") | .persistentVolumeClaim.claimName' "$bootstrap_job")" == 'key-data-teagramd-0' ]] ||
  fail 'the bootstrap Job must mount the primary server key PVC'
prod_teagramd_digest="$(yq -r '.images[] | select(.name == "ghcr.io/teagramhq/teagramd") | .digest' deploy/k8s/prod/kustomization.yaml)"
bootstrap_teagramd_digest="$(yq -r '.images[] | select(.name == "ghcr.io/teagramhq/teagramd") | .digest' deploy/k8s/bootstrap/kustomization.yaml)"
[[ -n "$prod_teagramd_digest" && "$prod_teagramd_digest" == "$bootstrap_teagramd_digest" ]] ||
  fail 'the production bootstrap Job must use the same pinned teagramd digest as the workload'

statefulset=deploy/k8s/base/teagramd-statefulset.yaml
for probe in startupProbe readinessProbe livenessProbe; do
  probe_port="$(yq -r ".spec.template.spec.containers[] | select(.name == \"teagramd\") | .${probe}.tcpSocket.port" "$statefulset")"
  [[ "$probe_port" == mtproto ]] ||
    fail "the main teagramd container must have a $probe that checks its MTProto listener"
done
identity_guard_status=0
rg --quiet 'ensure-identity|ensure-server-identity' deploy/k8s || identity_guard_status=$?
require_no_match_status "$identity_guard_status" 'the K8s identity command guard'

public_workload_guard_status=0
yq -e '.resources[] | select(. == "teagramd-public-deployment.yaml")' \
  deploy/k8s/base/kustomization.yaml >/dev/null 2>&1 || public_workload_guard_status=$?
require_no_match_status "$public_workload_guard_status" 'the K8s public WebSocket workload guard'

printf 'K8s deployment contract passed\n'
