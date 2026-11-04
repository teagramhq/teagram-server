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

manifest_step="$(yq -r '.jobs."check-k8s-manifests".steps[] | select(.name == "Render and validate production and CI overlays") | .run' .github/workflows/ci.yml)"
base_migration_check_line="$(grep -n -m 1 -F '.github/scripts/check-k8s-migration-version.sh' <<< "$manifest_step" | cut -d: -f1)"
ci_migration_prepare_line="$(grep -n -m 1 -F '.github/scripts/set-k8s-ci-migration-version.sh migrations deploy/k8s/ci' <<< "$manifest_step" | cut -d: -f1)"
overlay_render_line="$(grep -n -m 1 -F 'for overlay in prod ci bootstrap bootstrap/ci' <<< "$manifest_step" | cut -d: -f1)"
[[ "$manifest_step" == *'K8S_MIGRATION_BASE_SHA'* &&
   "$manifest_step" == *'git show'* &&
   "$manifest_step" == *"migrations deploy/k8s/base/migration-version \"\$previous_migration_version\""* &&
   "$base_migration_check_line" =~ ^[0-9]+$ &&
   "$ci_migration_prepare_line" =~ ^[0-9]+$ &&
   "$overlay_render_line" =~ ^[0-9]+$ &&
   "$base_migration_check_line" -lt "$ci_migration_prepare_line" &&
   "$ci_migration_prepare_line" -lt "$overlay_render_line" ]] ||
  fail 'manifest validation must accept unchanged base pins and render CI against the newest migration'

migration_fixture="$(mktemp -d)"
trap 'rm -rf "$migration_fixture"' EXIT
mkdir -p "$migration_fixture/migrations"
printf 'fixture\n' > "$migration_fixture/migrations/20261101000001_older.sql"
printf 'fixture\n' > "$migration_fixture/migrations/20261102000002_existing.sql"
printf 'fixture\n' > "$migration_fixture/migrations/20261104000001_new.sql"
printf '20261102000002\n' > "$migration_fixture/migration-version"
bash .github/scripts/check-k8s-migration-version.sh \
  "$migration_fixture/migrations" "$migration_fixture/migration-version" 20261102000002 >/dev/null ||
  fail 'a new migration must not require an immediate base migration-version edit'

printf '20261101000001\n' > "$migration_fixture/migration-version"
if bash .github/scripts/check-k8s-migration-version.sh \
  "$migration_fixture/migrations" "$migration_fixture/migration-version" 20261102000002 \
  >"$migration_fixture/invalid-output" 2>"$migration_fixture/invalid-error"; then
  fail 'a changed base migration version must match the newest migration'
fi
printf '20261103000001\n' > "$migration_fixture/migration-version"
if bash .github/scripts/check-k8s-migration-version.sh \
  "$migration_fixture/migrations" "$migration_fixture/migration-version" 20261102000002 \
  >"$migration_fixture/missing-output" 2>"$migration_fixture/missing-error"; then
  fail 'the base migration version must refer to a migration in the repository'
fi
printf '20261104000001\n' > "$migration_fixture/migration-version"
bash .github/scripts/check-k8s-migration-version.sh \
  "$migration_fixture/migrations" "$migration_fixture/migration-version" 20261102000002 >/dev/null ||
  fail 'an intentional base migration-version update to the newest migration must pass'

mkdir -p "$migration_fixture/ci"
cat > "$migration_fixture/ci/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
configMapGenerator:
  - name: required-migration-version
    behavior: replace
    literals:
      - migration-version=20261102000002
YAML
bash .github/scripts/set-k8s-ci-migration-version.sh \
  "$migration_fixture/migrations" "$migration_fixture/ci" >/dev/null ||
  fail 'the CI overlay must be configured to wait for the newest migration'
[[ "$(yq -r '.configMapGenerator[] | select(.name == "required-migration-version").literals[0]' \
  "$migration_fixture/ci/kustomization.yaml")" == 'migration-version=20261104000001' ]] ||
  fail 'the CI overlay must require the newest migration version'

apply_ci_step="$(yq -r '.jobs."test-k8s-deploy".steps[] | select(.name == "Apply the rendered CI overlay to k3s") | .run' .github/workflows/ci.yml)"
prepare_ci_line="$(grep -n -m 1 -F '.github/scripts/set-k8s-ci-migration-version.sh migrations deploy/k8s/ci' <<< "$apply_ci_step" | cut -d: -f1)"
render_ci_line="$(grep -n -m 1 -F 'kubectl kustomize deploy/k8s/ci' <<< "$apply_ci_step" | cut -d: -f1)"
dry_run_ci_line="$(grep -n -m 1 -F "kubectl apply --dry-run=server --filename \"\$RUNNER_TEMP/k8s-ci.yaml\"" <<< "$apply_ci_step" | cut -d: -f1)"
apply_ci_line="$(grep -n -m 1 -F "kubectl apply --filename \"\$RUNNER_TEMP/k8s-ci.yaml\"" <<< "$apply_ci_step" | cut -d: -f1)"
[[ "$prepare_ci_line" =~ ^[0-9]+$ &&
   "$render_ci_line" =~ ^[0-9]+$ &&
   "$dry_run_ci_line" =~ ^[0-9]+$ &&
   "$apply_ci_line" =~ ^[0-9]+$ &&
   "$prepare_ci_line" -lt "$render_ci_line" &&
   "$render_ci_line" -lt "$dry_run_ci_line" &&
   "$dry_run_ci_line" -lt "$apply_ci_line" ]] ||
  fail 'the CI migration version must be updated before rendering and applying the overlay'

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
