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
create_identity_pvc_wait="$(grep -F -A 1 'kubectl wait --for=create pvc/key-data-teagramd-0' <<< "$bootstrap_step")"
bound_identity_pvc_wait="$(grep -F -A 1 'kubectl wait --for=jsonpath=' <<< "$bootstrap_step")"
create_identity_pvc_wait_line="$(grep -n -m 1 -F 'kubectl wait --for=create pvc/key-data-teagramd-0' <<< "$bootstrap_step" | cut -d: -f1)"
bound_identity_pvc_wait_line="$(grep -n -m 1 -F 'kubectl wait --for=jsonpath=' <<< "$bootstrap_step" | cut -d: -f1)"
[[ -n "$bootstrap_step" && "$bootstrap_step" != null &&
   "$bootstrap_step" == *'kubectl apply -k deploy/k8s/bootstrap/ci'* &&
   "$bootstrap_step" == *'kubectl wait --for=condition=complete job/teagramd-identity-bootstrap'* &&
   "$bootstrap_step" == *'kubectl scale statefulset/teagramd --replicas=0'* &&
   "$bootstrap_step" == *'kubectl scale statefulset/teagramd --replicas=1'* ]] ||
  fail 'the deployment test must run the separately applied first-install identity bootstrap Job'
[[ "$create_identity_pvc_wait" == *'--namespace teagram --timeout=180s'* &&
   "$bound_identity_pvc_wait" == *'--namespace teagram --timeout=180s'* &&
   "$create_identity_pvc_wait_line" =~ ^[0-9]+$ &&
   "$bound_identity_pvc_wait_line" =~ ^[0-9]+$ &&
   "$create_identity_pvc_wait_line" -lt "$bound_identity_pvc_wait_line" ]] ||
  fail 'the deployment test must wait for the identity PVC to be created before waiting for it to bind'

bootstrap_job=deploy/k8s/bootstrap/ci/identity-job.yaml
[[ "$(yq -r '.spec.template.spec.containers[] | select(.name == "bootstrap-identity") | .command | join(" ")' "$bootstrap_job")" == '/usr/local/bin/teagramd bootstrap-identity' ]] ||
  fail 'the bootstrap Job must use the explicit bootstrap-identity command'
[[ "$(yq -r '.spec.template.spec.volumes[] | select(.name == "key-data") | .persistentVolumeClaim.claimName' "$bootstrap_job")" == 'key-data-teagramd-0' ]] ||
  fail 'the bootstrap Job must mount the primary server key PVC'
prod_teagramd_digest="$(yq -r '.images[] | select(.name == "ghcr.io/teagramhq/teagramd") | .digest' deploy/k8s/prod/kustomization.yaml)"
bootstrap_teagramd_digest="$(yq -r '.images[] | select(.name == "ghcr.io/teagramhq/teagramd") | .digest' deploy/k8s/bootstrap/kustomization.yaml)"
[[ -n "$prod_teagramd_digest" && "$prod_teagramd_digest" == "$bootstrap_teagramd_digest" ]] ||
  fail 'the production bootstrap Job must use the same pinned teagramd digest as the workload'
cutover_teagramd_digest="$(yq -r '.images[] | select(.name == "ghcr.io/teagramhq/teagramd") | .digest' deploy/k8s/prod-cutover/kustomization.yaml)"
[[ "$cutover_teagramd_digest" =~ ^sha256:[0-9a-f]{64}$ ]] ||
  fail 'the cutover workload must use a pinned teagramd image digest'
[[ "$cutover_teagramd_digest" != "sha256:$(printf '%064d' 0)" ]] ||
  fail 'the cutover workload must not use a placeholder teagramd image digest'
[[ "$(yq -r '.spec.suspend' deploy/k8s/prod-cutover/local-blob-migration-job.yaml)" == true ]] ||
  fail 'the cutover blob migration Job must stay suspended until the source PVC is populated'
[[ "$(yq -r '.spec.template.spec.containers[] | select(.name == "migrate") | .args | join(" ")' deploy/k8s/prod-cutover/local-blob-migration-job.yaml)" == *'--manifest discard --encrypt'* ]] ||
  fail 'the cutover blob migration must encrypt the private manifest and migrated media'
cutover_seed_job=deploy/k8s/test-cutover-account-job.yaml
[[ "$(yq -r '.spec.template.spec.containers[] | select(.name == "create-account") | .command | join(" ")' "$cutover_seed_job")" == '/usr/local/bin/teagramd' ]] ||
  fail 'the cutover account seed Job must invoke teagramd directly for its distroless image'
[[ "$(yq -r '.spec.template.spec.containers[] | select(.name == "create-account") | .args | join(" ")' "$cutover_seed_job")" == 'admin create-user --username cutover_smoke' ]] ||
  fail 'the cutover account seed Job must create the disposable smoke account'
[[ "$(yq -r '.spec.template.spec.containers[] | select(.name == "create-account") | .stdin' "$cutover_seed_job")" == true &&
   "$(yq -r '.spec.template.spec.containers[] | select(.name == "create-account") | .stdinOnce' "$cutover_seed_job")" == true ]] ||
  fail 'the cutover account seed Job must accept the password through one stdin attachment'
[[ "$(yq -r '.spec.template.spec.containers[] | select(.name == "create-account") | .securityContext.capabilities.add // [] | join(",")' "$cutover_seed_job")" == 'DAC_READ_SEARCH' ]] ||
  fail 'the cutover account seed Job must grant only DAC_READ_SEARCH to read the runtime-owned key as root'
[[ "$(yq -r '.spec.template.spec.containers[] | select(.name == "create-account") | .securityContext.capabilities.drop // [] | join(",")' "$cutover_seed_job")" == 'ALL' ]] ||
  fail 'the cutover account seed Job must drop every other capability'
[[ "$(yq -r '.spec.template.spec.containers[] | select(.name == "create-account") | .securityContext.runAsUser' "$cutover_seed_job")" == 0 ]] ||
  fail 'the cutover account seed Job must run as root for the guarded admin command'
for pvc in key-data-teagramd-0 legacy-blobs-teagramd-0 blob-mode-teagramd-0; do
  yq -e "select(.kind == \"PersistentVolumeClaim\" and .metadata.name == \"$pvc\")" \
    deploy/k8s/prod-cutover/persistentvolumeclaims.yaml >/dev/null 2>&1 ||
    fail "the cutover overlay is missing PVC $pvc"
done

statefulset=deploy/k8s/base/teagramd-statefulset.yaml
for probe in startupProbe readinessProbe livenessProbe; do
  probe_port="$(yq -r ".spec.template.spec.containers[] | select(.name == \"teagramd\") | .${probe}.tcpSocket.port" "$statefulset")"
  [[ "$probe_port" == mtproto ]] ||
    fail "the main teagramd container must have a $probe that checks its MTProto listener"
done
identity_guard_status=0
grep --recursive --extended-regexp --quiet \
  'ensure-identity|ensure-server-identity' deploy/k8s || identity_guard_status=$?
require_no_match_status "$identity_guard_status" 'the K8s identity command guard'

public_workload_guard_status=0
yq -e '.resources[] | select(. == "teagramd-public-deployment.yaml")' \
  deploy/k8s/base/kustomization.yaml >/dev/null 2>&1 || public_workload_guard_status=$?
require_no_match_status "$public_workload_guard_status" 'the K8s public WebSocket workload guard'

printf 'K8s deployment contract passed\n'
