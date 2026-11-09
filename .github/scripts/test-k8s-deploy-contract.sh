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
overlay_render_line="$(grep -n -m 1 -F 'for overlay in prod prod-cutover ci bootstrap bootstrap/ci' <<< "$manifest_step" | cut -d: -f1)"
cutover_overlay_check_pattern="if [[ \"\$overlay\" == prod-cutover ]]"
cutover_overlay_check_line="$(grep -n -m 1 -F "$cutover_overlay_check_pattern" <<< "$manifest_step" | cut -d: -f1)"
base_sha_else_line="$(grep -n -m 1 -F 'else' <<< "$manifest_step" | cut -d: -f1)"
base_sha_error_line="$(grep -n -m 1 -F 'K8S_MIGRATION_BASE_SHA must be a 40-character lowercase SHA' <<< "$manifest_step" | cut -d: -f1)"
base_sha_exit_line="$(grep -n -m 1 -F 'exit 1' <<< "$manifest_step" | cut -d: -f1)"
[[ "$manifest_step" == *'K8S_MIGRATION_BASE_SHA'* &&
   "$manifest_step" == *'git show'* &&
   "$manifest_step" == *"migrations deploy/k8s/base/migration-version \"\$previous_migration_version\""* &&
   "$manifest_step" == *'K8S_MIGRATION_BASE_SHA must be a 40-character lowercase SHA'* &&
   "$manifest_step" == *'>&2'* &&
   "$base_sha_else_line" =~ ^[0-9]+$ &&
   "$base_sha_error_line" =~ ^[0-9]+$ &&
   "$base_sha_exit_line" =~ ^[0-9]+$ &&
   "$base_sha_else_line" -lt "$base_sha_error_line" &&
   "$base_sha_error_line" -lt "$base_sha_exit_line" &&
   "$base_migration_check_line" =~ ^[0-9]+$ &&
   "$ci_migration_prepare_line" =~ ^[0-9]+$ &&
   "$overlay_render_line" =~ ^[0-9]+$ &&
   "$cutover_overlay_check_line" =~ ^[0-9]+$ &&
   "$base_migration_check_line" -lt "$ci_migration_prepare_line" &&
   "$ci_migration_prepare_line" -lt "$overlay_render_line" &&
   "$overlay_render_line" -lt "$cutover_overlay_check_line" ]] ||
  fail 'manifest validation must preserve migration checks and validate the cutover overlay'

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
grep --fixed-strings --quiet 'must match newest migration' "$migration_fixture/invalid-error" ||
  fail 'a changed base pin rejection must explain that it must match the newest migration'
printf '20261103000001\n' > "$migration_fixture/migration-version"
if bash .github/scripts/check-k8s-migration-version.sh \
  "$migration_fixture/migrations" "$migration_fixture/migration-version" 20261102000002 \
  >"$migration_fixture/missing-output" 2>"$migration_fixture/missing-error"; then
  fail 'the base migration version must refer to a migration in the repository'
fi
grep --fixed-strings --quiet 'does not exist in' "$migration_fixture/missing-error" ||
  fail 'a missing base pin rejection must identify that the migration does not exist'
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
cutover_teagramd_digest="$(yq -r '.images[] | select(.name == "ghcr.io/teagramhq/teagramd") | .digest' deploy/k8s/prod-cutover/kustomization.yaml)"
[[ "$cutover_teagramd_digest" =~ ^sha256:[0-9a-f]{64}$ ]] ||
  fail 'the cutover workload must use a pinned teagramd image digest'
[[ "$cutover_teagramd_digest" != "sha256:$(printf '%064d' 0)" ]] ||
  fail 'the cutover workload must not use a placeholder teagramd image digest'
web_base='https://github.com/teagramhq/teagram-web//deploy/k8s/base?ref=6947c892793562a8413d59cc741a123fe728da5d&timeout=120&submodules=false'
grep --fixed-strings --quiet -- "$web_base" deploy/k8s/prod-cutover/kustomization.yaml ||
  fail 'the cutover overlay must pin teagram-web k8s manifests to the reviewed commit'
cutover_web_digest="$(yq -r '.images[] | select(.name == "ghcr.io/teagramhq/teagram-web") | .digest' deploy/k8s/prod-cutover/kustomization.yaml)"
[[ "$cutover_web_digest" =~ ^sha256:[0-9a-f]{64}$ &&
   "$cutover_web_digest" != "sha256:$(printf '%064d' 0)" ]] ||
  fail 'the cutover web app must use a pinned image digest'
[[ "$(yq -r '.spec.selector."app.kubernetes.io/name"' deploy/k8s/prod-cutover/web-service.yaml)" == teagram-web &&
   "$(yq -r '.spec.ports[] | select(.name == "http") | .port' deploy/k8s/prod-cutover/web-service.yaml)" == 8080 &&
   "$(yq -r '.spec.ports[] | select(.name == "http") | .targetPort' deploy/k8s/prod-cutover/web-service.yaml)" == 8080 ]] ||
  fail 'the cutover web Service must address the teagram-web deployment on port 8080'
grep --fixed-strings --quiet -- 'http://web:8080' deploy/k8s/base/linkselector-deployment.yaml ||
  fail 'linkselector must resolve its web backend through the in-cluster web Service'
if grep --fixed-strings --quiet 'teagramd' deploy/k8s/base/linkselector-deployment.yaml; then
  fail 'linkselector must not forward public or tailnet requests to teagramd'
fi
[[ "$(yq -r '.spec.type' deploy/k8s/base/linkselector-service.yaml)" == ClusterIP &&
   "$(yq -r '.spec.ports[] | select(has("nodePort")) | .nodePort' deploy/k8s/base/linkselector-service.yaml)" == '' ]] ||
  fail 'linkselector must remain a ClusterIP-only Service'
[[ "$(yq -r 'select(.metadata.name == "teagramd") | .spec.type' deploy/k8s/base/teagramd-service.yaml)" == ClusterIP &&
   "$(yq -r 'select(.metadata.name == "teagramd") | .spec.ports[] | select(has("nodePort")) | .nodePort' deploy/k8s/base/teagramd-service.yaml)" == '' &&
   "$(yq -r 'select(.metadata.name == "teagramd") | [.spec.ports[] | [.name, .port] | join(":")] | join(",")' deploy/k8s/base/teagramd-service.yaml)" == \
     'mtproto:2443,websocket:2444,admin:2445,bot-api:2447,public-websocket:2446,public-bot-api:2449' ]] ||
  fail 'teagramd Services must stay internal and expose only the reviewed listener ports'
[[ "$(yq -r '.spec.ingress | length' deploy/k8s/base/teagramd-networkpolicy.yaml)" == 2 &&
   "$(yq -r '[.spec.ingress[].from[].podSelector.matchLabels."app.kubernetes.io/name"] | join(",")' deploy/k8s/base/teagramd-networkpolicy.yaml)" == 'tailscale-proxy,cloudflared' &&
   "$(yq -r '[.spec.ingress[0].ports[].port] | join(",")' deploy/k8s/base/teagramd-networkpolicy.yaml)" == '2443,2444,2445,2448' &&
   "$(yq -r '[.spec.ingress[1].ports[].port] | join(",")' deploy/k8s/base/teagramd-networkpolicy.yaml)" == '2446,2449' ]] ||
  fail 'the default-deny teagramd policy must isolate tailnet and public listener ports by pod identity'
if grep --fixed-strings --quiet 'ipBlock:' deploy/k8s/base/teagramd-networkpolicy.yaml; then
  fail 'teagramd ingress must be admitted by trusted pod selectors, not pod CIDRs'
fi
serve_config="$(yq -r 'select(.kind == "ConfigMap" and .metadata.name == "tailscale-proxy-serve-config") | .data."serve.json"' deploy/k8s/base/tailscale-proxy.yaml)"
jq --exit-status '
  .TCP["443"].HTTPS == true and
  .TCP["2443"].TCPForward == "teagramd:2443" and
  .TCP["2447"].TCPForward == "teagramd:2447" and
  .TCP["2447"].ProxyProtocol == 1 and
  ((.TCP | keys | sort) == ["2443", "2447", "443"]) and
  (has("AllowFunnel") | not) and
  .Web["teagram.tailaa4918.ts.net:443"].Handlers["/"] .Proxy == "http://linkselector:8081" and
  .Web["teagram.tailaa4918.ts.net:443"].Handlers["/apiws"].Proxy == "http://teagramd:2444" and
  .Web["teagram.tailaa4918.ts.net:443"].Handlers["/.well-known/teagramd/client"].Proxy == "http://teagramd:2444" and
  .Web["teagram.tailaa4918.ts.net:443"].Handlers["/.well-known/telegramd/client"].Proxy == "http://teagramd:2444" and
  .Web["teagram.tailaa4918.ts.net:443"].Handlers["/admin"].Proxy == "http://teagramd:2445"
' <<<"$serve_config" >/dev/null ||
  fail 'the Tailscale Serve config must contain only the named tailnet Service targets and ports'
[[ "$(yq -r '.spec.template.spec.containers[] | select(.name == "tailscale") | .image' deploy/k8s/base/tailscale-proxy.yaml)" == \
     'ghcr.io/tailscale/tailscale:v1.104.4@sha256:ed6480b33c6eb67c9cbd203ed42b9fb57e361986725b6856def71b7a3d9e276c' &&
   "$(yq -r '.spec.template.spec.containers[] | select(.name == "tailscale") | .env[] | select(.name == "TS_KUBE_SECRET") | .value' deploy/k8s/base/tailscale-proxy.yaml)" == teagram-tailscale-state &&
   "$(yq -r '.spec.template.spec.containers[] | select(.name == "tailscale") | .env[] | select(.name == "TS_AUTHKEY") | .valueFrom.secretKeyRef.name' deploy/k8s/base/tailscale-proxy.yaml)" == tailscale-auth &&
   "$(yq -r '.spec.template.spec.containers[] | select(.name == "tailscale") | .env[] | select(.name == "TS_AUTHKEY") | .valueFrom.secretKeyRef.key' deploy/k8s/base/tailscale-proxy.yaml)" == authkey &&
   "$(yq -r '.spec.template.spec.containers[] | select(.name == "tailscale") | .securityContext.capabilities.add // [] | length' deploy/k8s/base/tailscale-proxy.yaml)" == 0 &&
   "$(yq -r 'select(.kind == "StatefulSet" and .metadata.name == "tailscale-proxy") | .spec.template.spec.hostNetwork // false' deploy/k8s/base/tailscale-proxy.yaml)" == false ]] ||
  fail 'the proxy must use a pinned userspace image, external key, stable state Secret, and no host networking or added capabilities'
[[ "$(yq -r '.spec.template.spec.containers[] | select(.name == "tailscale") | .env[] | select(.name == "TS_USERSPACE") | .value' deploy/k8s/base/tailscale-proxy.yaml)" == true &&
   "$(yq -r '.spec.template.spec.containers[] | select(.name == "tailscale") | .env[] | select(.name == "TS_HOSTNAME") | .value' deploy/k8s/base/tailscale-proxy.yaml)" == teagram &&
   "$(yq -r '.spec.template.spec.containers[] | select(.name == "tailscale") | .env[] | select(.name == "TS_AUTH_ONCE") | .value' deploy/k8s/base/tailscale-proxy.yaml)" == true &&
   "$(yq -r '.spec.template.spec.containers[] | select(.name == "tailscale") | .livenessProbe // "none"' deploy/k8s/base/tailscale-proxy.yaml)" == none &&
   "$(yq -r 'select(.kind == "StatefulSet" and .metadata.name == "tailscale-proxy") | .spec.replicas' deploy/k8s/base/tailscale-proxy.yaml)" == 1 &&
   "$(yq -r 'select(.kind == "StatefulSet" and .metadata.name == "tailscale-proxy") | .spec.template.spec.serviceAccountName' deploy/k8s/base/tailscale-proxy.yaml)" == tailscale-proxy ]] ||
  fail 'the proxy must preserve the teagram node identity without auth churn or liveness restarts'
[[ "$(yq -r 'select(.kind == "Role" and .metadata.name == "tailscale-proxy-state") | .rules[0].verbs | join(",")' deploy/k8s/tailscale-proxy-access.yaml)" == 'get,update,patch' &&
   "$(yq -r 'select(.kind == "Role" and .metadata.name == "tailscale-proxy-state") | .rules[0].resourceNames | join(",")' deploy/k8s/tailscale-proxy-access.yaml)" == teagram-tailscale-state &&
   "$(yq -r 'select(.kind == "Role" and .metadata.name == "teagram-deployer-networkpolicy") | .rules[0].resources | join(",")' deploy/k8s/tailscale-proxy-access.yaml)" == networkpolicies &&
   "$(yq -r 'select(.kind == "Role" and .metadata.name == "teagram-deployer-networkpolicy") | .rules[0].verbs | join(",")' deploy/k8s/tailscale-proxy-access.yaml)" == 'get,list,watch,create,patch' ]] ||
  fail 'Tailscale state and deployer NetworkPolicy RBAC must remain resource- and namespace-scoped'
if ! grep --fixed-strings --quiet -- '--dry-run' deploy/k8s/install-tailnet-proxy-access.sh; then
  fail 'the one-time tailnet installer must provide a dry-run and exact rollback'
fi
if ! grep --fixed-strings --quiet -- '--rollback' deploy/k8s/install-tailnet-proxy-access.sh; then
  fail 'the one-time tailnet installer must provide a dry-run and exact rollback'
fi
grep --fixed-strings --quiet 'telegramd-key-v1' deploy/k8s/probe-discovery.py ||
  fail 'the shared discovery probe must send the v1 half-close request'
ingress_step="$(yq -r '.jobs."test-k8s-deploy".steps[] | select(.name == "Probe Tailscale, cloudflared, and denied in-cluster ingress") | .run' .github/workflows/ci.yml)"
[[ "$ingress_step" == *'test-service-ingress.py'* &&
   "$ingress_step" != *'kubectl port-forward'* ]] ||
  fail 'k8s-deploy must exercise ingress from in-cluster identities without port-forward'
ci_overlay="$(kubectl kustomize deploy/k8s/ci)"
ci_advertise_addr="$(yq -r 'select(.kind == "ConfigMap" and (.metadata.name | test("^teagramd-runtime-"))) | .data.TG_ADVERTISE_ADDR' <<<"$ci_overlay")"
mtproto_smoke_endpoint="$(yq -r '.spec.template.spec.containers[] | select(.name == "smoke") | .command[2]' deploy/k8s/ci/mtproto-probe-job.yaml)"
[[ "$ci_advertise_addr" == 'teagramd:2443' && "$mtproto_smoke_endpoint" == "$ci_advertise_addr" ]] ||
  fail 'the in-cluster MTProto smoke must use the endpoint advertised by the CI server'
if yq --exit-status 'select(.kind == "Job" and .metadata.name == "mtproto-smoke")' \
  <<<"$ci_overlay" >/dev/null 2>&1; then
  fail 'the MTProto smoke Job must be created after its generated client ConfigMap'
fi
# A fresh pod is admitted by the label-based NetworkPolicy only after the
# policy controller syncs it, so its first connection can be refused. The Job
# must therefore be allowed to start a replacement pod.
mtproto_smoke_backoff="$(yq -r '.spec.backoffLimit' deploy/k8s/ci/mtproto-probe-job.yaml)"
[[ "$mtproto_smoke_backoff" =~ ^[0-9]+$ && "$mtproto_smoke_backoff" -ge 3 ]] ||
  fail 'the MTProto smoke Job must retry so a new pod survives the NetworkPolicy sync delay'
mtproto_smoke_step="$(yq -r '.jobs."test-k8s-deploy".steps[] | select(.name == "Smoke the MTProto Service through the in-cluster proxy path") | .run' .github/workflows/ci.yml)"
if [[ "$mtproto_smoke_step" != *'kubectl create configmap mtproto-probe-client'*'kubectl apply --dry-run=server --namespace=teagram'*'--filename=deploy/k8s/ci/mtproto-probe-job.yaml'*'kubectl apply --namespace=teagram'*'--filename=deploy/k8s/ci/mtproto-probe-job.yaml'* ]]; then
  fail 'k8s-deploy must create the MTProto client ConfigMap before starting its smoke Job'
fi
if yq -r '.jobs."test-k8s-deploy".steps[].run // ""' .github/workflows/ci.yml |
  grep --fixed-strings --quiet 'kubectl port-forward'; then
  fail 'k8s-deploy must not use port-forward for any smoke test'
fi
grep --fixed-strings --quiet 'PROXY TCP4' deploy/k8s/test-networkpolicy.py ||
  fail 'the tailnet Bot API check must send a PROXY v1 header'
if ! grep --fixed-strings --quiet 'status_line' deploy/k8s/test-networkpolicy.py; then
  fail 'the Bot API probe must exercise an unauthenticated request and inspect its response'
fi
if ! grep --fixed-strings --quiet '401' deploy/k8s/test-networkpolicy.py; then
  fail 'the Bot API probe must exercise an unauthenticated request and inspect its response'
fi
[[ "$(yq -r 'select(.kind == "StatefulSet" and .metadata.name == "tailscale-proxy") | .spec.replicas' deploy/k8s/ci/tailscale-proxy-ci.yaml)" == 0 ]] ||
  fail 'the disposable k3s lane must not authenticate a production Tailscale device'
cutover_plan="$(deploy/k8s/cutover-alpha.sh --dry-run)"
[[ "$cutover_plan" == *'source teagram LXC was deleted'* &&
   "$cutover_plan" != *'Serve'* && "$cutover_plan" != *'port-forward'* ]] ||
  fail 'the retired cutover dry-run must not describe host-side ingress changes'
if deploy/k8s/cutover-alpha.sh --apply >/dev/null 2>&1; then
  fail 'the retired cutover script must refuse production apply'
fi
python3 -c 'import ast, pathlib; [ast.parse(pathlib.Path(p).read_text()) for p in ["deploy/k8s/probe-discovery.py", "deploy/k8s/test-networkpolicy.py", "deploy/k8s/test-service-ingress.py"]]' ||
  fail 'the in-cluster ingress probe scripts must parse as Python'
grep --fixed-strings --quiet 'telegramd-key-v1' deploy/k8s/probe-discovery.py ||
  fail 'the shared discovery probe must send the v1 half-close request'
if [[ "$(yq -r '.jobs."test-k8s-deploy".permissions.packages // "none"' .github/workflows/ci.yml)" != none ]]; then
  fail 'the k3s deployment lane must not require GHCR package credentials'
fi
k8s_deploy_run="$(yq -r '.jobs."test-k8s-deploy".steps[] | .run // ""' .github/workflows/ci.yml)"
[[ "$k8s_deploy_run" == *'kubectl create namespace teagram'* ]] ||
  fail 'the cutover fixture namespace must stay unlabeled so Pod Security does not mask its capability check'
if grep --extended-regexp --quiet 'docker login ghcr.io|ghcr-pull|GITHUB_TOKEN' <<< "$k8s_deploy_run"; then
  fail 'the k3s deployment lane must pull public images without registry credentials'
fi
for overlay in prod prod-cutover bootstrap bootstrap/ci ci; do
  rendered_overlay="$(kubectl kustomize "deploy/k8s/$overlay")" ||
    fail "could not render $overlay while checking public image pulls"
  if grep --fixed-strings --quiet -- 'imagePullSecrets:' <<< "$rendered_overlay"; then
    fail "$overlay must not require a registry pull Secret"
  fi
done
for overlay in prod prod-cutover; do
  if ! kubectl kustomize "deploy/k8s/$overlay" |
    yq eval-all -o=json '[select(.kind == "StatefulSet" and .metadata.name == "teagramd") |
      .spec.template.spec.containers[] | select(.name == "teagramd") | .env[] |
      select(.name == "TG_WEBSOCKET_LISTEN_ADDR" or .name == "TG_ADMIN_LISTEN_ADDR" or
             .name == "TG_ADMIN_TOKEN_HASH" or .name == "TG_ADMIN_ORIGIN")]' - |
    jq -e 'length == 4 and (map({key: .name, value: .}) | from_entries |
      .TG_WEBSOCKET_LISTEN_ADDR.value == ":2444" and
      .TG_ADMIN_LISTEN_ADDR.value == ":2445" and
      .TG_ADMIN_TOKEN_HASH.valueFrom.secretKeyRef == {name: "teagramd-admin", key: "TG_ADMIN_TOKEN_HASH"} and
      .TG_ADMIN_ORIGIN.valueFrom.secretKeyRef == {name: "teagramd-admin", key: "TG_ADMIN_ORIGIN", optional: true})' >/dev/null; then
    fail "$overlay must preserve the WebSocket and admin listeners and Secret references"
  fi
done
[[ "$(yq -r '.spec.template.spec.containers[] | select(.name == "teagramd") | .env[] | select(.name == "TG_MEDIA_KEY_CURRENT") | .valueFrom.configMapKeyRef.name' deploy/k8s/base/teagramd-statefulset.yaml)" == teagramd-runtime &&
   "$(yq -r '.spec.template.spec.containers[] | select(.name == "teagramd") | .env[] | select(.name == "TG_MEDIA_KEY_CURRENT") | .valueFrom.configMapKeyRef.key' deploy/k8s/base/teagramd-statefulset.yaml)" == TG_MEDIA_KEY_CURRENT ]] ||
  fail 'the workload must read its active media encryption key version from the runtime ConfigMap'
grep --fixed-strings --quiet "TG_MEDIA_KEY_CURRENT=\$TG_MEDIA_KEY_CURRENT" deploy/k8s/bootstrap-alpha.sh ||
  fail 'bootstrap must preserve the live media encryption key version in teagramd-runtime'
# shellcheck disable=SC2016
runtime_configmap_row=$(grep --fixed-strings '`teagramd-runtime` ConfigMap' deploy/k8s/README.md)
[[ "$runtime_configmap_row" == *TG_MEDIA_KEY_CURRENT* ]] ||
  fail 'the documented teagramd-runtime ConfigMap keys must include TG_MEDIA_KEY_CURRENT'
if grep --fixed-strings --quiet 'local-blob-migration' deploy/k8s/prod-cutover/kustomization.yaml; then
  fail 'the cutover must preserve encrypted S3 objects directly, without a local-blob migration Job'
fi
[[ "$(yq -r '.spec.containers[] | select(.name == "client") | .env[] | select(.name == "TG_BLOB_S3_ACCESS_KEY_ID") | .valueFrom.secretKeyRef.name' deploy/k8s/test-cutover-s3-client.yaml)" == teagramd-blob-credentials &&
   "$(yq -r '.spec.containers[] | select(.name == "client") | .env[] | select(.name == "TG_BLOB_S3_SECRET_ACCESS_KEY") | .valueFrom.secretKeyRef.name' deploy/k8s/test-cutover-s3-client.yaml)" == teagramd-blob-credentials ]] ||
  fail 'the disposable cutover S3 client must obtain credentials from the namespace Secret'
[[ "$(yq -r '.spec.containers[] | select(.name == "client") | .image' deploy/k8s/test-cutover-s3-client.yaml)" == ghcr.io/teagramhq/teagramd-mc-init:ci ]] ||
  fail 'the cutover test S3 client must use the revision-labelled mc image built in CI'
[[ "$(yq -r '.spec.securityContext.runAsUser' deploy/k8s/test-cutover-blob-mode-seed.yaml)" == 0 &&
   "$(yq -r '.spec.volumes[] | select(.name == "blob-mode") | .persistentVolumeClaim.claimName' deploy/k8s/test-cutover-blob-mode-seed.yaml)" == blob-mode-teagramd-0 ]] ||
  fail 'the disposable blob-mode fixture must use a root-owned seed pod and the source authority PVC'
grep --fixed-strings --quiet 'CUTOVER_TEST_CORRUPT_S3_OBJECT' deploy/k8s/test-cutover-alpha-k3s.sh ||
  fail 'the cutover integration must fail closed on a per-object S3 checksum mismatch'
grep --fixed-strings --quiet 'check_source_media_keys' deploy/k8s/cutover-alpha.sh ||
  fail 'the cutover runner must verify that the target encrypted-media key set matches the source'
grep --fixed-strings --quiet 'check_source_blob_credentials' deploy/k8s/cutover-alpha.sh ||
  fail 'the cutover runner must verify that the target S3 credentials match the source'
grep --fixed-strings --quiet 'preserve_media_key_version' deploy/k8s/cutover-alpha.sh ||
  fail 'the cutover runner must preserve the source media encryption key version'
grep --fixed-strings --quiet 'source and target S3 object counts or byte totals do not match' deploy/k8s/cutover-alpha.sh ||
  fail 'the cutover runner must compare source and target S3 object counts and byte totals before switching routes'
grep --fixed-strings --quiet 'source and target S3 per-object SHA-256 checksums do not match' deploy/k8s/cutover-alpha.sh ||
  fail 'the cutover runner must reject same-size S3 object corruption by SHA-256'
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
cutover_key_reader=deploy/k8s/test-cutover-key-reader.yaml
[[ "$(yq -r '.spec.containers[] | select(.name == "reader") | .image' "$cutover_key_reader")" == \
  'mirror.gcr.io/library/busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec' ]] ||
  fail 'the cutover key reader must use the pinned BusyBox image, not the distroless workload image'
[[ "$(yq -r '.spec.securityContext.runAsUser' "$cutover_key_reader")" == 65532 &&
   "$(yq -r '.spec.volumes[] | select(.name == "key-data") | .persistentVolumeClaim.claimName' "$cutover_key_reader")" == 'key-data-teagramd-0' &&
   "$(yq -r '.spec.containers[] | select(.name == "reader") | .volumeMounts[] | select(.name == "key-data") | .readOnly' "$cutover_key_reader")" == true &&
   "$(yq -r '.spec.volumes[] | select(.name == "legacy-blobs") | .persistentVolumeClaim.claimName' "$cutover_key_reader")" == 'legacy-blobs-teagramd-0' &&
   "$(yq -r '.spec.containers[] | select(.name == "reader") | .volumeMounts[] | select(.name == "legacy-blobs") | .readOnly' "$cutover_key_reader")" == true &&
   "$(yq -r '.spec.volumes[] | select(.name == "blob-mode") | .persistentVolumeClaim.claimName' "$cutover_key_reader")" == 'blob-mode-teagramd-0' &&
   "$(yq -r '.spec.containers[] | select(.name == "reader") | .volumeMounts[] | select(.name == "blob-mode") | .readOnly' "$cutover_key_reader")" == true &&
   "$(yq -r '.spec.volumes[] | select(.name == "media-keys") | .secret.secretName' "$cutover_key_reader")" == 'teagramd-media-keys' &&
   "$(yq -r '.spec.containers[] | select(.name == "reader") | .volumeMounts[] | select(.name == "media-keys") | .readOnly' "$cutover_key_reader")" == true ]] ||
  fail 'the cutover helper must mount source identity, blob-mode and local blob PVCs read-only'
grep --fixed-strings --quiet 'pod/cutover-key-reader -c reader --' deploy/k8s/test-cutover-alpha-incus.sh ||
  fail 'the actual cutover runner test adapter must read source identity from the BusyBox helper pod'
grep --fixed-strings --quiet "kubectl exec --namespace \"\$SOURCE_NAMESPACE\" postgres-0 -c postgres --" \
  deploy/k8s/test-cutover-alpha-incus.sh ||
  fail 'the actual cutover runner test adapter must read the source migration version from disposable PostgreSQL'
grep --fixed-strings --quiet '/run/teagramd/blob-mode/.' deploy/k8s/test-cutover-alpha-incus.sh ||
  fail 'the actual cutover runner test adapter must read blob-mode authority from the source PVC'
fence_step=$(sed -n '/^fence_source_writes() {/,/^}/p' deploy/k8s/cutover-alpha.sh)
stop_container_step=$(sed -n '/^stop_running_compose_container() {/,/^}/p' deploy/k8s/cutover-alpha.sh)
fence_verification=$(sed -n '/^verify_source_write_fence() {/,/^}/p' deploy/k8s/cutover-alpha.sh)
# shellcheck disable=SC2016
[[ "$fence_step" == *'alpha_for_each_line "$running_ids" stop_running_compose_container'* &&
   "$stop_container_step" == *'[[ $service == postgres || $service == rustfs ]] && return 0'* &&
   "$fence_verification" == *'alpha_for_each_line "$(<"$COMPOSE_STOPPED_FILE")" verify_stopped_compose_container'* &&
   "$fence_verification" == *'check_source_container teagram-rustfs-1'* ]] ||
  fail 'the source write fence must preserve and verify the RustFS Compose service'
grep --fixed-strings --quiet 'teagram-rustfs-1|rustfs)' deploy/k8s/test-cutover-alpha-incus.sh ||
  fail 'the disposable source adapter must model the RustFS Compose container'
grep --fixed-strings --quiet 'teagramd postgres rustfs linkselector linklanding' deploy/k8s/test-cutover-alpha-incus.sh ||
  fail 'the disposable source adapter must report RustFS among running Compose services'
grep --fixed-strings --quiet 'cutover-alpha.sh --apply --test-mode' deploy/k8s/test-cutover-alpha-k3s.sh ||
  fail 'the k3s cutover test must execute the actual apply runner in its disposable namespace'
grep --fixed-strings --quiet 'CUTOVER_TEST_SOURCE_DOCKER_HELPER=true' deploy/k8s/test-cutover-alpha-k3s.sh ||
  fail 'the k3s cutover test must exercise the source Docker helper path'
grep --fixed-strings --quiet \
  'for docker_event in pull run-migrations run-helper network-connect rm; do' \
  deploy/k8s/test-cutover-alpha-k3s.sh ||
  fail 'the k3s cutover test must verify image pulls, both Docker runs, network attachment, and cleanup'
grep --fixed-strings --quiet 'cutover-alpha.sh --rollback --test-mode' deploy/k8s/test-cutover-alpha-k3s.sh ||
  fail 'the k3s cutover test must exercise the actual rollback runner'
grep --fixed-strings --quiet 'CUTOVER_FAIL_OLD_CONNECTOR_CHECK=true' deploy/k8s/test-cutover-alpha-k3s.sh ||
  fail 'the k3s cutover test must exercise failure handling after the first live route'
grep --fixed-strings --quiet 'test-cutover-blob-seed.yaml' deploy/k8s/test-cutover-alpha-k3s.sh ||
  fail 'the k3s cutover test must seed source blobs for the actual transfer and migration'
grep --fixed-strings --quiet 'smoke_upload_preserved_media' deploy/k8s/test-cutover-alpha-k3s.sh ||
  fail 'the k3s cutover test must upload an encrypted media object before cutover'
grep --fixed-strings --quiet 'smoke_download_preserved_media' deploy/k8s/test-cutover-alpha-k3s.sh ||
  fail 'the k3s cutover test must download and compare the preserved media after cutover'
grep --fixed-strings --quiet 'CUTOVER_TEST_CORRUPT_S3_OBJECT=true' deploy/k8s/test-cutover-alpha-k3s.sh ||
  fail 'the k3s cutover test must overwrite an object with same-length corrupt data'
grep --fixed-strings --quiet 'source and target S3 per-object SHA-256 checksums do not match' \
  deploy/k8s/test-cutover-alpha-k3s.sh ||
  fail 'the k3s cutover test must assert a per-object checksum mismatch'
grep --fixed-strings --quiet 'CUTOVER_TEST_PINNED_REVISION=0000000000000000000000000000000000000000' deploy/k8s/test-cutover-alpha-k3s.sh ||
  fail 'the k3s cutover test must prove revision mismatch fails before the write fence'
[[ "$(tr -d '\r\n' < deploy/k8s/prod/source-revision)" =~ ^[0-9a-f]{40}$ ]] ||
  fail 'the production cutover source revision must be a full Git SHA'
cutover_blob_seed=deploy/k8s/test-cutover-blob-seed.yaml
[[ "$(yq -r '.spec.containers[] | select(.name == "seed") | .image' "$cutover_blob_seed")" == \
  'mirror.gcr.io/library/busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec' ]] ||
  fail 'the cutover blob seed must use the pinned BusyBox image'
bot_api_inner="$(yq -r '.spec.template.spec.containers[] | select(.name == "bot-api-inner") | .args | join(" ")' deploy/k8s/base/teagramd-statefulset.yaml)"
compose_bot_api_inner="$(yq -r '.services["bot-api-inner"].command | join(" ")' docker-compose.yml)"
bot_api_ingress="$(yq -r '.spec.template.spec.containers[] | select(.name == "bot-api-ingress") | .args | join(" ")' deploy/k8s/base/teagramd-statefulset.yaml)"
compose_bot_api_ingress="$(yq -r '.services["bot-api-ingress"].command | join(" ")' docker-compose.yml)"
[[ "$bot_api_inner" == "$compose_bot_api_inner" && "$bot_api_ingress" == "$compose_bot_api_ingress" &&
   "$(yq -r '.spec.template.spec.containers[] | select(.name == "teagramd") | .env[] | select(.name == "TG_BOT_API_LISTEN_ADDR") | .value' deploy/k8s/base/teagramd-statefulset.yaml)" == '127.0.0.1:2447' &&
   "$(yq -r '.spec.ports[] | select(.name == "bot-api") | .targetPort' deploy/k8s/base/teagramd-service.yaml)" == bot-api-ingress ]] ||
  fail 'the k3s Bot API relays must preserve the Compose listener and socket relay behavior'
grep --fixed-strings --quiet -- 'socat -T2 -u /dev/null TCP:127.0.0.1:2447' deploy/k8s/base/teagramd-statefulset.yaml ||
  fail 'the Bot API inner relay must be ready only after its socket and application listener are reachable'
grep --fixed-strings --quiet -- 'socat -T2 -u /dev/null TCP:127.0.0.1:2448' deploy/k8s/base/teagramd-statefulset.yaml ||
  fail 'the Bot API ingress relay must verify its local TCP listener before serving traffic'
grep --fixed-strings --quiet -- 'smoke_bot_api' deploy/k8s/test-cutover-alpha-k3s.sh ||
  fail 'the throwaway cutover integration must send a Bot API request through the k3s relay'
grep --fixed-strings --quiet -- 'ProxyProtocol": 1' deploy/k8s/test-cutover-alpha-k3s.sh ||
  fail 'the throwaway cutover integration must verify the Bot API PROXY protocol route'
for pvc in key-data-teagramd-0 legacy-blobs-teagramd-0 blob-mode-teagramd-0; do
  yq -e "select(.kind == \"PersistentVolumeClaim\" and .metadata.name == \"$pvc\")" \
    deploy/k8s/prod-cutover/persistentvolumeclaims.yaml >/dev/null 2>&1 ||
    fail "the cutover overlay is missing PVC $pvc"
done

cutover_volume_importer=deploy/k8s/prod-cutover/volume-importer.yaml
[[ "$(yq -r '.spec.containers[] | select(.name == "importer") | .securityContext.capabilities.add | join(",")' "$cutover_volume_importer")" == 'CHOWN,FOWNER' ]] ||
  fail 'the cutover importer must retain only the capabilities needed to prepare target PVC ownership'
script=deploy/k8s/cutover-alpha.sh
grep --fixed-strings --quiet 'tar -xopf - -C /target/legacy-blobs' "$script" ||
  fail "$script must preserve blob modes without restoring source ownership"

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

agent_role=deploy/k8s/agent-access.yaml
[[ "$(yq -r '.kind' "$agent_role" | sed '/^---$/d' | paste -sd, -)" == 'ServiceAccount,Role,RoleBinding' ]] ||
  fail 'agent access must use only a ServiceAccount, namespace Role, and RoleBinding'
[[ "$(yq -r 'select(.kind == "ServiceAccount" and .metadata.name == "teagram-agent") | .automountServiceAccountToken' "$agent_role")" == false ]] ||
  fail 'the agent ServiceAccount must disable token automount'
[[ "$(yq -r 'select(.kind == "RoleBinding" and .metadata.name == "teagram-agent") | .metadata.namespace' "$agent_role")" == teagram &&
   "$(yq -r 'select(.kind == "RoleBinding" and .metadata.name == "teagram-agent") | .roleRef.kind' "$agent_role")" == Role &&
   "$(yq -r 'select(.kind == "RoleBinding" and .metadata.name == "teagram-agent") | .subjects[0].name' "$agent_role")" == teagram-agent ]] ||
  fail 'the agent RoleBinding must bind only the agent ServiceAccount in teagram'
agent_policy=deploy/k8s/agent-pod-delete-policy.yaml
[[ "$(yq -r 'select(.kind == "ValidatingAdmissionPolicy") | .spec.failurePolicy' "$agent_policy")" == Fail &&
   "$(yq -r 'select(.kind == "ValidatingAdmissionPolicyBinding") | .spec.policyName' "$agent_policy")" == teagram-agent-pod-delete ]] ||
  fail 'the agent pod-delete policy must fail closed and have a binding'
[[ "$(yq -r 'select(.kind == "ValidatingAdmissionPolicy") | .spec.matchConditions[0].expression' "$agent_policy")" == *'system:serviceaccount:teagram:teagram-agent'* &&
   "$(yq -r 'select(.kind == "ValidatingAdmissionPolicy") | .spec.validations[0].expression' "$agent_policy")" == *'ReplicaSet'* &&
   "$(yq -r 'select(.kind == "ValidatingAdmissionPolicy") | .spec.validations[0].expression' "$agent_policy")" == *'StatefulSet'* &&
   "$(yq -r 'select(.kind == "ValidatingAdmissionPolicy") | .spec.validations[0].expression' "$agent_policy")" == *'teagramd'* &&
   "$(yq -r 'select(.kind == "ValidatingAdmissionPolicy") | .spec.validations[0].expression' "$agent_policy")" == *'linkselector'* &&
   "$(yq -r 'select(.kind == "ValidatingAdmissionPolicy") | .spec.validations[0].expression' "$agent_policy")" == *'linklanding'* &&
   "$(yq -r 'select(.kind == "ValidatingAdmissionPolicy") | .spec.validations[0].expression' "$agent_policy")" == *'cloudflared'* ]] ||
  fail 'the agent admission policy must scope identity and controller-owned workload names'
agent_rollback_block="$(sed -n '/^To roll back only agent access after bootstrap/,/^```$/p' deploy/k8s/README.md)"
case "$agent_rollback_block" in
  *'set -euo pipefail'*'serviceaccount teagram-agent'*'rolebinding teagram-agent'*'validatingadmissionpolicybinding teagram-agent-pod-delete'*'validatingadmissionpolicy teagram-agent-pod-delete'*'role teagram-agent'*) ;;
  *) fail 'agent rollback must revoke its ServiceAccount and RoleBinding before removing admission protection' ;;
esac
wildcard_access_status=0
yq -e 'select(.kind == "Role" and .metadata.name == "teagram-agent")
  | .rules[]
  | select((.verbs | contains(["*"])) or
      (.resources | contains(["*"]) or contains(["pods/*"]) or contains(["services/*"])))' \
  "$agent_role" >/dev/null 2>&1 || wildcard_access_status=$?
require_no_match_status "$wildcard_access_status" 'wildcard agent Role access'
if ! python3 - "$agent_role" <<'PY'
import json
import subprocess
import sys

rules = subprocess.run(
    ["yq", "-o=json", 'select(.kind == "Role" and .metadata.name == "teagram-agent") | .rules', sys.argv[1]],
    check=True,
    capture_output=True,
    text=True,
).stdout
expected = [
    {"apiGroups": [""], "resources": ["pods", "pods/log", "events", "services", "endpoints"], "verbs": ["get", "list", "watch"]},
    {"apiGroups": ["apps"], "resources": ["statefulsets", "deployments", "replicasets"], "verbs": ["get", "list", "watch"]},
    {"apiGroups": ["apps"], "resources": ["controllerrevisions"], "verbs": ["get", "list"]},
    {"apiGroups": ["batch"], "resources": ["jobs", "cronjobs"], "verbs": ["get", "list", "watch"]},
    {"apiGroups": [""], "resources": ["pods"], "verbs": ["delete"]},
]
if json.loads(rules) != expected:
    raise SystemExit(1)
PY
then
  fail 'the agent Role must contain exactly the approved resources and verbs'
fi
grep --fixed-strings --quiet 'log_parameter_max_length = 0' deploy/k8s/base/postgresql.conf ||
  fail 'PostgreSQL statement logs must not include bind parameter values'
[[ -f deploy/k8s/agent-access-threat-model.md ]] ||
  fail 'the agent access threat model must be committed with the Kubernetes manifests'

deployer_boundary_step="$(yq -r '.jobs."check-k8s-manifests".steps[] | select(.name == "Verify deployer RBAC and Pod Security boundaries") | .run' .github/workflows/ci.yml)"
[[ "$deployer_boundary_step" == *'kubectl apply --filename deploy/k8s/base/namespace.yaml'* ]] ||
  fail 'the deployer boundary check must apply the namespace Pod Security labels before testing pod admission'

agent_access_step="$(yq -r '.jobs."test-k8s-deploy".steps[] | select(.name == "Verify agent RBAC and pod-delete admission") | .run' .github/workflows/ci.yml)"
[[ "$agent_access_step" == *'test-k8s-agent-access.sh'* ]] ||
  fail 'test: k8s-deploy must exercise agent RBAC and pod-delete admission'
grep --fixed-strings --quiet "kubectl --as=\"\$agent\" delete pod \"\$linkselector_pod\" --dry-run=server" \
  .github/scripts/test-k8s-agent-access.sh ||
  fail 'the agent admission test must server-side dry-run an allowed linkselector delete'
grep --fixed-strings --quiet "kubectl --as=\"\$agent\" delete pod postgres-0 --dry-run=server" \
  .github/scripts/test-k8s-agent-access.sh ||
  fail 'the agent admission test must server-side dry-run a denied postgres delete'

printf 'K8s deployment contract passed\n'
