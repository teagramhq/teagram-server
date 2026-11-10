#!/usr/bin/env bash
# shellcheck disable=SC2016 # Single-quoted strings match literal shell text.
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
overlay_render_line="$(grep -n -m 1 -F 'for overlay in prod ci bootstrap bootstrap/ci backup-bootstrap' <<< "$manifest_step" | cut -d: -f1)"
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
   "$base_migration_check_line" -lt "$ci_migration_prepare_line" &&
   "$ci_migration_prepare_line" -lt "$overlay_render_line" ]] ||
  fail 'manifest validation must preserve migration checks before rendering the active overlays'

namespace_apply_line="$(grep -n -m 1 -F 'kubectl apply --filename deploy/k8s/base/namespace.yaml' <<< "$manifest_step" | cut -d: -f1)"
maintenance_server_dry_run_line="$(grep -n -m 1 -F 'kubectl create --dry-run=server' <<< "$manifest_step" | cut -d: -f1)"
[[ "$namespace_apply_line" =~ ^[0-9]+$ &&
   "$maintenance_server_dry_run_line" =~ ^[0-9]+$ &&
   "$namespace_apply_line" -lt "$maintenance_server_dry_run_line" ]] ||
  fail 'the manifest check must create its namespace before server-dry-running the maintenance Job'

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
probe_serviceaccount_line="$(grep -n -m 1 -F 'kubectl apply --filename deploy/k8s/ci/tailscale-proxy-probe-serviceaccount.yaml' <<< "$apply_ci_step" | cut -d: -f1)"
dry_run_ci_line="$(grep -n -m 1 -F "kubectl apply --dry-run=server --filename \"\$RUNNER_TEMP/k8s-ci.yaml\"" <<< "$apply_ci_step" | cut -d: -f1)"
apply_ci_line="$(grep -n -m 1 -F "kubectl apply --filename \"\$RUNNER_TEMP/k8s-ci.yaml\"" <<< "$apply_ci_step" | cut -d: -f1)"
api_egress_ci_line="$(grep -n -m 1 -F 'python3 deploy/k8s/render-tailnet-proxy-api-egress.py' <<< "$apply_ci_step" | cut -d: -f1)"
[[ "$prepare_ci_line" =~ ^[0-9]+$ &&
   "$render_ci_line" =~ ^[0-9]+$ &&
   "$probe_serviceaccount_line" =~ ^[0-9]+$ &&
   "$dry_run_ci_line" =~ ^[0-9]+$ &&
   "$apply_ci_line" =~ ^[0-9]+$ &&
   "$api_egress_ci_line" =~ ^[0-9]+$ &&
   "$prepare_ci_line" -lt "$render_ci_line" &&
   "$render_ci_line" -lt "$probe_serviceaccount_line" &&
   "$probe_serviceaccount_line" -lt "$dry_run_ci_line" &&
   "$dry_run_ci_line" -lt "$apply_ci_line" &&
   "$apply_ci_line" -lt "$api_egress_ci_line" ]] ||
  fail 'CI must apply the generated API egress policy after the rendered probe pod exists'
[[ "$(yq -r '.kind' deploy/k8s/ci/tailscale-proxy-probe-serviceaccount.yaml)" == ServiceAccount &&
   "$(yq -r '.metadata.name' deploy/k8s/ci/tailscale-proxy-probe-serviceaccount.yaml)" == tailscale-proxy &&
   "$(yq -r '.automountServiceAccountToken' deploy/k8s/ci/tailscale-proxy-probe-serviceaccount.yaml)" == true ]] ||
  fail 'the CI policy probe must use a dedicated ServiceAccount with its token mounted'

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
grep --fixed-strings --quiet -- 'http://web:8080' deploy/k8s/base/linkselector-deployment.yaml ||
  fail 'linkselector must route public web traffic to the in-cluster web Service'
if grep --extended-regexp --quiet -- 'server-upstream|http://teagramd' deploy/k8s/base/linkselector-deployment.yaml; then
  fail 'linkselector must not forward any public or tailnet path to teagramd'
fi
[[ "$(yq -r '.spec.type' deploy/k8s/base/linkselector-service.yaml)" == ClusterIP &&
   "$(yq -r '.spec.selector."app.kubernetes.io/name"' deploy/k8s/base/linkselector-service.yaml)" == linkselector &&
   "$(yq -r '.spec.selector.matchLabels."app.kubernetes.io/name"' deploy/k8s/base/linkselector-deployment.yaml)" == linkselector &&
   "$(yq -r '.spec.selector.matchLabels."app.kubernetes.io/component" // "none"' deploy/k8s/base/linkselector-deployment.yaml)" == none &&
   "$(yq -r '.spec.ports[] | select(has("nodePort")) | .nodePort' deploy/k8s/base/linkselector-service.yaml)" == '' ]] ||
  fail 'linkselector must remain a ClusterIP-only Service'
[[ "$(yq -r 'select(.metadata.name == "teagramd") | .spec.type' deploy/k8s/base/teagramd-service.yaml)" == ClusterIP &&
   "$(yq -r 'select(.metadata.name == "teagramd") | .spec.ports[] | select(has("nodePort")) | .nodePort' deploy/k8s/base/teagramd-service.yaml)" == '' &&
   "$(yq -r 'select(.metadata.name == "teagramd") | [.spec.ports[] | [.name, .port] | join(":")] | join(",")' deploy/k8s/base/teagramd-service.yaml)" == \
     'mtproto:2443,websocket:2444,admin:2445,public-ws:2446,bot-api:2447,public-bot-api:2449' ]] ||
  fail 'teagramd Services must stay internal and expose only the reviewed listener ports'
[[ "$(yq -r '.spec.ingress | length' deploy/k8s/base/teagramd-networkpolicy.yaml)" == 2 &&
   "$(yq -r '[.spec.ingress[].from[].podSelector.matchLabels."app.kubernetes.io/name"] | join(",")' deploy/k8s/base/teagramd-networkpolicy.yaml)" == 'tailscale-proxy,cloudflared' &&
   "$(yq -r '[.spec.ingress[0].ports[].port] | join(",")' deploy/k8s/base/teagramd-networkpolicy.yaml)" == '2443,2444,2445,2448' &&
   "$(yq -r '[.spec.ingress[1].ports[].port] | join(",")' deploy/k8s/base/teagramd-networkpolicy.yaml)" == '2446,2449' ]] ||
  fail 'the default-deny teagramd policy must isolate tailnet and public listener ports by pod identity'
if grep --fixed-strings --quiet 'ipBlock:' deploy/k8s/base/teagramd-networkpolicy.yaml; then
  fail 'teagramd ingress must be admitted by trusted pod selectors, not pod CIDRs'
fi
[[ "$(yq -r '.spec.podSelector.matchLabels."app.kubernetes.io/name"' deploy/k8s/base/tailscale-proxy-egress.yaml)" == tailscale-proxy &&
   "$(yq -r '.spec.policyTypes | join(",")' deploy/k8s/base/tailscale-proxy-egress.yaml)" == Egress &&
   "$(yq -r '[.spec.egress[0].ports[].port] | join(",")' deploy/k8s/base/tailscale-proxy-egress.yaml)" == '2443,2444,2445,2448' &&
   "$(yq -r '.spec.egress[1].ports[0].port' deploy/k8s/base/tailscale-proxy-egress.yaml)" == 8081 &&
   "$(yq -r '.spec.egress[1].to[0].podSelector.matchLabels."app.kubernetes.io/name"' deploy/k8s/base/tailscale-proxy-egress.yaml)" == linkselector &&
   "$(yq -r '[.spec.egress[2].ports[].port] | join(",")' deploy/k8s/base/tailscale-proxy-egress.yaml)" == '53,53' &&
   "$(yq -r '[.spec.egress[3].ports[].port] | join(",")' deploy/k8s/base/tailscale-proxy-egress.yaml)" == '443,3478,41641' &&
   "$(yq -r '.spec.egress[3].to[0].ipBlock.except | join(",")' deploy/k8s/base/tailscale-proxy-egress.yaml)" == '10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,169.254.0.0/16,127.0.0.0/8' &&
   "$(yq -r '.spec.egress | length' deploy/k8s/base/tailscale-proxy-egress.yaml)" == 4 &&
   "$(yq -r '[.spec.egress[].ports[].port] | map(select(. == 6443)) | length' deploy/k8s/base/tailscale-proxy-egress.yaml)" == 0 ]] ||
  fail 'the Tailscale proxy egress policy must limit cluster destinations and keep private and tailnet ranges blocked'
python3 - <<'PY' || fail 'the API egress renderer must allow only ready endpoint IPv4 /32 addresses'
import json
import subprocess

fixture = {
    "subsets": [
        {
            "addresses": [{"ip": "10.20.30.41"}, {"ip": "10.20.30.40"}],
            "notReadyAddresses": [{"ip": "10.20.30.42"}],
        }
    ]
}
result = subprocess.run(
    ["python3", "deploy/k8s/render-tailnet-proxy-api-egress.py"],
    input=json.dumps(fixture),
    capture_output=True,
    text=True,
    check=True,
)
for cidr in ("10.20.30.40/32", "10.20.30.41/32"):
    if f"cidr: {cidr}" not in result.stdout:
        raise SystemExit(f"missing API endpoint allowlist entry {cidr}")
if "0.0.0.0/0" in result.stdout or "port: 6443" not in result.stdout:
    raise SystemExit("API egress policy has an unexpected address or port")
if "10.20.30.42" in result.stdout:
    raise SystemExit("API egress policy included an endpoint that is not Ready")
try:
    subprocess.run(
        ["python3", "deploy/k8s/render-tailnet-proxy-api-egress.py"],
        input=json.dumps({"subsets": []}),
        capture_output=True,
        text=True,
        check=True,
    )
except subprocess.CalledProcessError:
    pass
else:
    raise SystemExit("renderer accepted an API Service without ready endpoints")
PY
serve_config="$(yq -r 'select(.kind == "ConfigMap" and .metadata.name == "tailscale-proxy-serve-config") | .data."serve.json"' deploy/k8s/base/tailscale-proxy.yaml)"
jq --exit-status '
  .TCP["443"].HTTPS == true and
  .TCP["2447"].TCPForward == "teagramd:2447" and
  .TCP["2447"].ProxyProtocol == 1 and
  ((.TCP | keys | sort) == ["2447", "443"]) and
  (has("AllowFunnel") | not) and
  .Web["teagram.tailaa4918.ts.net:443"].Handlers["/"] .Proxy == "http://linkselector:8081" and
  .Web["teagram.tailaa4918.ts.net:443"].Handlers["/apiws"].Proxy == "http://teagramd:2444/apiws" and
  .Web["teagram.tailaa4918.ts.net:443"].Handlers["/.well-known/teagramd/client"].Proxy == "http://teagramd:2444/.well-known/teagramd/client" and
  .Web["teagram.tailaa4918.ts.net:443"].Handlers["/.well-known/telegramd/client"].Proxy == "http://teagramd:2444/.well-known/teagramd/client" and
  .Web["teagram.tailaa4918.ts.net:443"].Handlers["/admin"].Proxy == "http://teagramd:2445/admin"
' <<<"$serve_config" >/dev/null ||
  fail 'the Tailscale Serve config must contain only the named tailnet Service targets and ports'
python3 .github/scripts/test-tailscale-serve-routes.py <<<"$serve_config" ||
  fail 'Tailscale Serve mount prefixes must resolve to the expected teagramd backend paths'
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
[[ "$ci_advertise_addr" == 'teagramd:2443' && "$mtproto_smoke_endpoint" == "127.0.0.1:${ci_advertise_addr##*:}" ]] ||
  fail 'the in-cluster MTProto smoke must reach the advertised port through the loopback forwarder'
production_forwarder_image="$(yq -r '.spec.template.spec.containers[] | select(.name == "mtproto-forwarder") | .image' deploy/k8s/base/tailscale-proxy.yaml)"
ci_forwarder_image="$(yq -r '.spec.template.spec.initContainers[] | select(.name == "mtproto-forwarder") | .image' deploy/k8s/ci/mtproto-probe-job.yaml)"
production_forwarder_command="$(yq -r '.spec.template.spec.containers[] | select(.name == "mtproto-forwarder") | .command | join(" ")' deploy/k8s/base/tailscale-proxy.yaml)"
ci_forwarder_command="$(yq -r '.spec.template.spec.initContainers[] | select(.name == "mtproto-forwarder") | .command | join(" ")' deploy/k8s/ci/mtproto-probe-job.yaml)"
production_forwarder_args="$(yq -r '.spec.template.spec.containers[] | select(.name == "mtproto-forwarder") | .args | join(" ")' deploy/k8s/base/tailscale-proxy.yaml)"
ci_forwarder_args="$(yq -r '.spec.template.spec.initContainers[] | select(.name == "mtproto-forwarder") | .args | join(" ")' deploy/k8s/ci/mtproto-probe-job.yaml)"
[[ "$production_forwarder_image" == "$ci_forwarder_image" &&
   "$production_forwarder_image" == 'mirror.gcr.io/alpine/socat:1.8.0.3@sha256:beb4a68d9e4fe6b0f21ea774a0fde6c31f580dde6368939ed70100c5385b015e' &&
   "$production_forwarder_command" == "$ci_forwarder_command" &&
   "$production_forwarder_command" == socat &&
   "$production_forwarder_args" == "$ci_forwarder_args" &&
   "$production_forwarder_args" == '-d -d TCP-LISTEN:2443,bind=127.0.0.1,reuseaddr,fork TCP:teagramd:2443' &&
   "$(yq -r '.spec.template.spec.initContainers[] | select(.name == "mtproto-forwarder") | .restartPolicy' deploy/k8s/ci/mtproto-probe-job.yaml)" == Always ]] ||
  fail 'the MTProto CI job must use the same half-close forwarder as the production proxy'
discovery_probe_command="$(yq -r '.spec.template.spec.containers[] | select(.name == "discovery") | .command | join(" ")' deploy/k8s/ci/mtproto-probe-job.yaml)"
[[ "$discovery_probe_command" == 'python3 /tests/probe_discovery.py --host 127.0.0.1 --port 2443' &&
   "$(yq -r '.spec.template.spec.containers[] | select(.name == "discovery") | .volumeMounts[] | select(.name == "ingress-probes") | .mountPath' deploy/k8s/ci/mtproto-probe-job.yaml)" == /tests &&
   "$(yq -r '.spec.template.spec.volumes[] | select(.name == "ingress-probes") | .configMap.name' deploy/k8s/ci/mtproto-probe-job.yaml)" == k8s-ingress-probes ]] ||
  fail 'the MTProto CI job must run the 48-byte half-close discovery probe through the loopback forwarder'
ci_public_advertise_addr="$(yq -r 'select(.kind == "StatefulSet" and .metadata.name == "teagramd") | .spec.template.spec.containers[] | select(.name == "teagramd") | .env[] | select(.name == "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR") | .value' <<<"$ci_overlay")"
ci_public_registration="$(yq -r 'select(.kind == "StatefulSet" and .metadata.name == "teagramd") | .spec.template.spec.containers[] | select(.name == "teagramd") | .env[] | select(.name == "TG_PUBLIC_REGISTRATION") | .value' <<<"$ci_overlay")"
ci_public_origin="$(yq -r 'select(.kind == "StatefulSet" and .metadata.name == "teagramd") | .spec.template.spec.containers[] | select(.name == "teagramd") | .env[] | select(.name == "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS") | .value' <<<"$ci_overlay")"
[[ "$ci_public_advertise_addr" == 'server.example.test:443' &&
   "$ci_public_registration" == closed &&
   "$ci_public_origin" == 'https://web.example.test' ]] ||
  fail 'the CI overlay must use synthetic public discovery, registration, and origin settings'
# mtproto-smoke refuses a target port that differs from the advertised one, so
# every forwarded MTProto probe must use the advertised port locally.
if grep --fixed-strings --quiet '12443' .github/workflows/ci.yml deploy/k8s/test-postgres-backup-restore.sh; then
  fail 'forwarded MTProto probes must use the CI-advertised port 2443, not 12443'
fi
if yq --exit-status 'select(.kind == "Job" and .metadata.name == "mtproto-smoke")' \
  <<<"$ci_overlay" >/dev/null 2>&1; then
  fail 'the MTProto smoke Job must be created after its generated client ConfigMap'
fi
# The first connection from a new pod can be refused while the NetworkPolicy
# controller syncs its identity. Retry in the same pod so the source IP stays
# stable while that policy update propagates.
mtproto_smoke_restart_policy="$(yq -r '.spec.template.spec.restartPolicy' deploy/k8s/ci/mtproto-probe-job.yaml)"
[[ "$mtproto_smoke_restart_policy" == OnFailure ]] ||
  fail 'the MTProto smoke Job must retry in the same pod while NetworkPolicy syncs'
mtproto_smoke_backoff="$(yq -r '.spec.backoffLimit' deploy/k8s/ci/mtproto-probe-job.yaml)"
[[ "$mtproto_smoke_backoff" =~ ^[0-9]+$ && "$mtproto_smoke_backoff" -ge 3 ]] ||
  fail 'the MTProto smoke Job must allow enough retries for NetworkPolicy sync'
mtproto_smoke_step="$(yq -r '.jobs."test-k8s-deploy".steps[] | select(.name == "Smoke the MTProto Service through the in-cluster proxy path") | .run' .github/workflows/ci.yml)"
if [[ "$mtproto_smoke_step" != *'kubectl create configmap mtproto-probe-client'*'kubectl apply --dry-run=server --namespace=teagram'*'--filename=deploy/k8s/ci/mtproto-probe-job.yaml'*'kubectl apply --namespace=teagram'*'--filename=deploy/k8s/ci/mtproto-probe-job.yaml'* ]]; then
  fail 'k8s-deploy must create the MTProto client ConfigMap before starting its smoke Job'
fi
public_websocket_step="$(yq -r '.jobs."test-k8s-deploy".steps[] | select(.name == "Test public WebSocket and discovery through the CI connector") | .run' .github/workflows/ci.yml)"
[[ "$public_websocket_step" == *'test-k8s-public-websocket.py'* &&
   "$public_websocket_step" == *'--container=ingress-probe'* &&
   "$public_websocket_step" == *'--host 127.0.0.1 --port 8080'* &&
   "$public_websocket_step" != *'kubectl port-forward'* ]] ||
  fail 'the public WebSocket smoke must run inside the connector pod without port-forward'
grep --fixed-strings --quiet 'TCP:teagramd:2446' deploy/k8s/ci/cloudflared-ci.yaml ||
  fail 'the disposable public connector must reach the public WebSocket Service directly'
grep --fixed-strings --quiet 'name: ingress-probe' deploy/k8s/ci/cloudflared-ci.yaml ||
  fail 'the connector pod must include the Python ingress probe sidecar'
grep --fixed-strings --quiet 'exec_probe(cloudflared, "public-ingress", "ingress-probe")' \
  deploy/k8s/test-service-ingress.py ||
  fail 'cloudflared public listener probes must execute from the connector pod identity'
python3 .github/scripts/test_k8s_public_websocket_contract.py ||
  fail 'the public WebSocket connector smoke must validate its discovery contract'
grep --fixed-strings --quiet 'PROXY TCP4' deploy/k8s/test-networkpolicy.py ||
  fail 'the tailnet Bot API check must send a PROXY v1 header'
grep --fixed-strings --quiet 'deny-public' deploy/k8s/test-networkpolicy.py ||
  fail 'the policy probe must reject non-cloudflared access to both public listeners'
grep --fixed-strings --quiet 'exec_probe(cloudflared, "public-ingress", "ingress-probe")' deploy/k8s/test-service-ingress.py ||
  fail 'the cloudflared identity must prove access to both public listeners'
grep --fixed-strings --quiet 'deny-egress' deploy/k8s/test-networkpolicy.py ||
  fail 'the tailnet proxy policy probe must reject egress to PostgreSQL and RustFS'
grep --fixed-strings --quiet 'kubernetes.default.svc' deploy/k8s/test-networkpolicy.py ||
  fail 'the proxy-labelled policy probe must verify Kubernetes API access'
grep --fixed-strings --quiet '"Authorization": f"Bearer {token}"' deploy/k8s/test-networkpolicy.py ||
  fail 'the Kubernetes API probe must authenticate with its projected ServiceAccount token'
grep --fixed-strings --quiet 'ca.crt' deploy/k8s/test-networkpolicy.py ||
  fail 'the Kubernetes API probe must use the mounted service-account CA'
grep --fixed-strings --quiet 'ssl.create_default_context' deploy/k8s/test-networkpolicy.py ||
  fail 'the Kubernetes API probe must verify the API server certificate'
if grep --fixed-strings --quiet '_create_unverified_context' deploy/k8s/test-networkpolicy.py; then
  fail 'the Kubernetes API probe must not send its bearer token to an unverified endpoint'
fi
grep --fixed-strings --quiet 'exec_probe(TAILNET_PROBE, "kube-api")' deploy/k8s/test-service-ingress.py ||
  fail 'the proxy-labelled CI pod must test Kubernetes API access'
grep --fixed-strings --quiet 'render-tailnet-proxy-api-egress.py' .github/workflows/ci.yml ||
  fail 'CI must install the exact API endpoint egress allowlist before probing it'
grep --fixed-strings --quiet 'serviceAccountName: tailscale-proxy' deploy/k8s/ci/tailnet-policy-probes.yaml ||
  fail 'the Kubernetes API probe must use the proxy ServiceAccount identity'
grep --fixed-strings --quiet 'bot-api-public-ci-stand-in' deploy/k8s/ci/public-ingress-ci.yaml ||
  fail 'the loopback public Bot API relay must be labelled as a CI-only stand-in'
grep --fixed-strings --quiet 'MAIN-3600 replaces it' deploy/k8s/ci/public-ingress-ci.yaml ||
  fail 'the CI Bot API stand-in must identify its replacement issue'
[[ "$(yq eval-all -r '[.metadata.labels."app.kubernetes.io/name"] | join(",")' deploy/k8s/ci/tailnet-policy-probes.yaml)" == 'tailscale-proxy,linkselector,network-policy-probe' ]] ||
  fail 'the in-cluster probes must model tailnet, linkselector, and untrusted source identities'
ci_linkselector_overlay="$(kubectl kustomize deploy/k8s/ci)"
ci_linkselector_selector="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "linkselector") | .spec.selector.matchLabels' <<<"$ci_linkselector_overlay")"
[[ "$ci_linkselector_selector" == *'app.kubernetes.io/name: linkselector'* &&
   "$ci_linkselector_selector" == *'app.kubernetes.io/component: server'* &&
   "$(yq -r 'select(.kind == "Service" and .metadata.name == "linkselector") | .spec.selector."app.kubernetes.io/component"' <<<"$ci_linkselector_overlay")" == server &&
   "$(yq -r 'select(.kind == "Pod" and .metadata.name == "linkselector-policy-probe") | .metadata.labels."app.kubernetes.io/component"' deploy/k8s/ci/tailnet-policy-probes.yaml)" == ci-probe ]] ||
  fail 'the CI linkselector probe must keep its identity separate from the real Deployment and Service'
# linkselector exits on SIGTERM without draining, so a terminating pod must stay
# in service long enough for Service endpoints to drop it, or a rollout refuses
# connections for the pod's last moments.
linkselector_prestop="$(yq -r '.spec.template.spec.containers[] | select(.name == "linkselector") | .lifecycle.preStop.sleep.seconds // 0' deploy/k8s/base/linkselector-deployment.yaml)"
linkselector_grace="$(yq -r '.spec.template.spec.terminationGracePeriodSeconds // 30' deploy/k8s/base/linkselector-deployment.yaml)"
[[ "$linkselector_prestop" =~ ^[0-9]+$ && "$linkselector_prestop" -ge 5 &&
   "$linkselector_grace" =~ ^[0-9]+$ && "$linkselector_grace" -gt "$linkselector_prestop" ]] ||
  fail 'linkselector must sleep in preStop (at least 5 s, below its termination grace) so rollouts do not refuse connections'
grep --fixed-strings --quiet 'LINKSELECTOR_PROBE' deploy/k8s/test-service-ingress.py ||
  fail 'the linkselector-labelled pod must exercise both denied listener groups'
grep --fixed-strings --quiet 'old_uid' deploy/k8s/test-service-ingress.py ||
  fail 'the teagramd rollout probe must require a changed pod UID'
grep --fixed-strings --quiet 'observedGeneration' deploy/k8s/test-service-ingress.py ||
  fail 'the teagramd rollout probe must wait for the current StatefulSet generation'
grep --fixed-strings --quiet 'availableReplicas' deploy/k8s/test-service-ingress.py ||
  fail 'the linkselector rollout probe must wait for all replicas to be available'
grep --fixed-strings --quiet 'error.stderr' deploy/k8s/test-service-ingress.py ||
  fail 'the rollout probe must include kubectl stderr in failure messages'
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
if deploy/k8s/cutover-alpha.sh --apply --test-mode >/dev/null 2>&1; then
  fail 'the retired cutover script must not expose an apply path in test mode'
fi
python3 -c 'import ast, pathlib; [ast.parse(pathlib.Path(p).read_text()) for p in [".github/scripts/test-tailscale-serve-routes.py", "deploy/k8s/probe-discovery.py", "deploy/k8s/render-tailnet-proxy-api-egress.py", "deploy/k8s/test-networkpolicy.py", "deploy/k8s/test-service-ingress.py"]]' ||
  fail 'the in-cluster ingress probe scripts must parse as Python'
grep --fixed-strings --quiet 'telegramd-key-v1' deploy/k8s/probe-discovery.py ||
  fail 'the shared discovery probe must send the v1 half-close request'
if [[ "$(yq -r '.jobs."test-k8s-deploy".permissions.packages // "none"' .github/workflows/ci.yml)" != none ]]; then
  fail 'the k3s deployment lane must not require GHCR package credentials'
fi
k8s_deploy_run="$(yq -r '.jobs."test-k8s-deploy".steps[] | .run // ""' .github/workflows/ci.yml)"
[[ "$k8s_deploy_run" == *'kubectl create namespace teagram'* ]] ||
  fail 'the disposable k3s deployment lane must create its test namespace'
if grep --extended-regexp --quiet 'docker login ghcr.io|ghcr-pull|GITHUB_TOKEN' <<< "$k8s_deploy_run"; then
  fail 'the k3s deployment lane must pull public images without registry credentials'
fi
for overlay in prod bootstrap bootstrap/ci ci; do
  rendered_overlay="$(kubectl kustomize "deploy/k8s/$overlay")" ||
    fail "could not render $overlay while checking public image pulls"
  if grep --fixed-strings --quiet -- 'imagePullSecrets:' <<< "$rendered_overlay"; then
    fail "$overlay must not require a registry pull Secret"
  fi
done
if ! kubectl kustomize deploy/k8s/prod |
  yq eval-all -o=json '[select(.kind == "StatefulSet" and .metadata.name == "teagramd") |
    .spec.template.spec.containers[] | select(.name == "teagramd") | .env[] |
    select(.name == "TG_WEBSOCKET_LISTEN_ADDR" or .name == "TG_ADMIN_LISTEN_ADDR" or
           .name == "TG_ADMIN_TOKEN_HASH" or .name == "TG_ADMIN_ORIGIN")]' - |
  jq -e 'length == 4 and (map({key: .name, value: .}) | from_entries |
    .TG_WEBSOCKET_LISTEN_ADDR.value == ":2444" and
    .TG_ADMIN_LISTEN_ADDR.value == ":2445" and
    .TG_ADMIN_TOKEN_HASH.valueFrom.secretKeyRef == {name: "teagramd-admin", key: "TG_ADMIN_TOKEN_HASH"} and
    .TG_ADMIN_ORIGIN.valueFrom.secretKeyRef == {name: "teagramd-admin", key: "TG_ADMIN_ORIGIN", optional: true})' >/dev/null; then
  fail 'prod must preserve the WebSocket and admin listeners and Secret references'
fi
[[ "$(yq -r '.spec.template.spec.containers[] | select(.name == "teagramd") | .env[] | select(.name == "TG_MEDIA_KEY_CURRENT") | .valueFrom.configMapKeyRef.name' deploy/k8s/base/teagramd-statefulset.yaml)" == teagramd-runtime &&
   "$(yq -r '.spec.template.spec.containers[] | select(.name == "teagramd") | .env[] | select(.name == "TG_MEDIA_KEY_CURRENT") | .valueFrom.configMapKeyRef.key' deploy/k8s/base/teagramd-statefulset.yaml)" == TG_MEDIA_KEY_CURRENT ]] ||
  fail 'the workload must read its active media encryption key version from the runtime ConfigMap'
if grep --extended-regexp --quiet 'SOURCE_INSTANCE|docker (inspect|cp)|incus (info|exec) teagram([[:space:]]|$)' \
  deploy/k8s/bootstrap-alpha.sh; then
  fail 'the alpha bootstrap must not read from the deleted application LXC'
fi
# shellcheck disable=SC2016
runtime_configmap_row=$(grep --fixed-strings '`teagramd-runtime` ConfigMap' deploy/k8s/README.md)
[[ "$runtime_configmap_row" == *TG_MEDIA_KEY_CURRENT* &&
   "$runtime_configmap_row" == *TG_PUBLIC_LINK_PREFIX* &&
   "$runtime_configmap_row" == *TG_ADVERTISE_ADDR* &&
   "$runtime_configmap_row" != *TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR* &&
   "$runtime_configmap_row" != *TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS* ]] ||
  fail 'the documented runtime ConfigMap keys must exclude production listener values committed in the overlay'
# shellcheck disable=SC2016
grep --fixed-strings --quiet '`teagramd-turnstile` Secret' deploy/k8s/README.md ||
  fail 'the external Turnstile Secret must be documented without its value'
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

install_fixture="$migration_fixture/tailnet-proxy-install"
mkdir -p "$install_fixture/bin"
cat > "$install_fixture/bin/kubectl" <<'KUBECTL'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$KUBECTL_CALLS"
case "${1:-} ${2:-}" in
  'config current-context') printf 'install-test\n' ;;
  'get namespace') ;;
  'get secret')
    case "${3:-}" in
      tailscale-auth)
        if [[ "$TAILNET_INSTALL_CASE" != empty-key ]]; then
          printf 'dGVzdC1rZXk=\n'
        fi
        ;;
      teagram-tailscale-state)
        if [[ "$TAILNET_INSTALL_CASE" == present ]]; then
          printf 'secret/teagram-tailscale-state\n'
        fi
        ;;
      *) printf 'unexpected Secret lookup: %s\n' "$*" >&2; exit 64 ;;
    esac
    ;;
  'get endpoints')
    printf '{"subsets":[{"addresses":[{"ip":"10.20.30.40"},{"ip":"10.20.30.41"}]}]}\n'
    ;;
  'apply --dry-run=server')
    if [[ " $* " == *' --filename=- '* ]]; then
      cat >/dev/null
    fi
    ;;
  'create --dry-run=server')
    cat >/dev/null
    printf 'secret/teagram-tailscale-state\n'
    ;;
  'create --filename=-')
    cat > "$INSTALL_CREATED_MANIFEST"
    grep --fixed-strings --quiet 'name: teagram-tailscale-state' "$INSTALL_CREATED_MANIFEST"
    ;;
  'apply --filename=-')
    cat > "$INSTALL_POLICY_MANIFEST"
    ;;
  'apply --filename') ;;
  *) printf 'unexpected kubectl call: %s\n' "$*" >&2; exit 64 ;;
esac
KUBECTL
chmod 0700 "$install_fixture/bin/kubectl"

run_tailnet_proxy_apply() {
  local scenario="$1"
  env "PATH=$install_fixture/bin:$PATH" \
    "TAILNET_INSTALL_CASE=$scenario" \
    "KUBECTL_CALLS=$install_fixture/$scenario.calls" \
    "INSTALL_CREATED_MANIFEST=$install_fixture/$scenario.manifest" \
    "INSTALL_POLICY_MANIFEST=$install_fixture/$scenario.api-policy" \
    bash deploy/k8s/install-tailnet-proxy-access.sh --apply
}

if ! env "PATH=$install_fixture/bin:$PATH" \
  "TAILNET_INSTALL_CASE=absent" \
  "KUBECTL_CALLS=$install_fixture/dry-run.calls" \
  "INSTALL_CREATED_MANIFEST=$install_fixture/dry-run.manifest" \
  "INSTALL_POLICY_MANIFEST=$install_fixture/dry-run.api-policy" \
  bash deploy/k8s/install-tailnet-proxy-access.sh --dry-run \
  >"$install_fixture/dry-run.log" 2>&1; then
  fail 'the installer dry-run must validate the exact API endpoint egress policy'
fi
grep --fixed-strings --quiet 'apply --dry-run=server --filename=-' "$install_fixture/dry-run.calls" ||
  fail 'the installer dry-run must server-validate the generated API egress policy'
grep --fixed-strings --quiet 'cidr: 10.20.30.40/32' "$install_fixture/dry-run.log" ||
  fail 'the installer dry-run must show the exact API egress destination'
if grep --extended-regexp --quiet '^(apply|create) --filename=-($| )' "$install_fixture/dry-run.calls"; then
  fail 'the installer dry-run must not write the generated policy or state Secret'
fi

if ! run_tailnet_proxy_apply absent >"$install_fixture/absent.log" 2>&1; then
  fail 'the installer must create the state Secret when it is absent'
fi
grep --fixed-strings --quiet 'create --filename=-' "$install_fixture/absent.calls" ||
  fail 'the installer did not create the missing state Secret'
grep --fixed-strings --quiet 'name: teagram-tailscale-state' "$install_fixture/absent.manifest" ||
  fail 'the installer created an unexpected state Secret manifest'
grep --fixed-strings --quiet 'name: tailscale-proxy-api-egress' "$install_fixture/absent.api-policy" ||
  fail 'the installer did not apply the API egress policy'
grep --fixed-strings --quiet 'cidr: 10.20.30.40/32' "$install_fixture/absent.api-policy" ||
  fail 'the API egress policy did not restrict the first endpoint to a /32'
grep --fixed-strings --quiet 'cidr: 10.20.30.41/32' "$install_fixture/absent.api-policy" ||
  fail 'the API egress policy did not include the second ready endpoint'

if ! run_tailnet_proxy_apply present >"$install_fixture/present.log" 2>&1; then
  fail 'the installer must apply proxy access when the state Secret already exists'
fi
if grep --fixed-strings --quiet 'create --filename=-' "$install_fixture/present.calls"; then
  fail 'the installer must leave an existing Tailscale state Secret untouched'
fi
grep --fixed-strings --quiet 'apply --filename=-' "$install_fixture/present.calls" ||
  fail 'the installer must refresh the API egress policy on rerun'

if run_tailnet_proxy_apply empty-key >"$install_fixture/empty-key.log" 2>&1; then
  fail 'the installer must reject an empty tailscale-auth authkey'
fi
grep --fixed-strings --quiet 'must contain a non-empty authkey' "$install_fixture/empty-key.log" ||
  fail 'the installer must explain that the external authkey is empty'
if grep --extended-regexp --quiet 'apply --filename|create --filename' "$install_fixture/empty-key.calls"; then
  fail 'the installer changed resources after finding an empty authkey'
fi

rollback_fixture="$migration_fixture/tailnet-proxy-rollback"
mkdir -p "$rollback_fixture/bin"
cat > "$rollback_fixture/bin/kubectl" <<'KUBECTL'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$KUBECTL_CALLS"
case "${1:-} ${2:-}" in
  'config current-context')
    printf 'rollback-test\n'
    ;;
  'get namespace')
    ;;
  'get statefulset')
    if [[ " $* " != *' --ignore-not-found '* ]]; then
      printf 'StatefulSet lookup must ignore only NotFound\n' >&2
      exit 64
    fi
    case "$TAILNET_ROLLBACK_CASE" in
      api-error)
        printf 'simulated Kubernetes API timeout\n' >&2
        exit 1
        ;;
      absent|orphan-pod)
        ;;
      running|wait-error|pods-error|pods-remain)
        printf 'statefulset.apps/tailscale-proxy\n'
        ;;
      *)
        printf 'unknown test case: %s\n' "$TAILNET_ROLLBACK_CASE" >&2
        exit 64
        ;;
    esac
    ;;
  'scale statefulset/tailscale-proxy')
    ;;
  'wait --for=delete')
    if [[ "$TAILNET_ROLLBACK_CASE" == wait-error ]]; then
      printf 'simulated pod deletion timeout\n' >&2
      exit 1
    fi
    ;;
  'get pods')
    if [[ "$TAILNET_ROLLBACK_CASE" == pods-error ]]; then
      printf 'simulated Kubernetes API timeout while listing pods\n' >&2
      exit 1
    fi
    if [[ "$TAILNET_ROLLBACK_CASE" == pods-remain || "$TAILNET_ROLLBACK_CASE" == orphan-pod ]]; then
      printf 'pod/tailscale-proxy-0\n'
    fi
    ;;
  'delete rolebinding'|'delete role'|'delete networkpolicy'|'delete serviceaccount')
    ;;
  *)
    printf 'unexpected kubectl call: %s\n' "$*" >&2
    exit 64
    ;;
esac
KUBECTL
chmod 0700 "$rollback_fixture/bin/kubectl"

run_tailnet_proxy_rollback() {
  local scenario="$1"
  env "PATH=$rollback_fixture/bin:$PATH" \
    "TAILNET_ROLLBACK_CASE=$scenario" \
    "KUBECTL_CALLS=$rollback_fixture/$scenario.calls" \
    bash deploy/k8s/install-tailnet-proxy-access.sh --rollback
}

assert_no_rollback_access_removal() {
  local calls_file="$1"
  if grep --extended-regexp --quiet '^delete (rolebinding|role|networkpolicy|serviceaccount) ' "$calls_file"; then
    fail 'the rollback must keep RBAC in place until the proxy is confirmed stopped'
  fi
}

if run_tailnet_proxy_rollback api-error >"$rollback_fixture/api-error.log" 2>&1; then
  fail 'the rollback accepted a failed StatefulSet lookup'
fi
grep --fixed-strings --quiet 'could not determine whether StatefulSet tailscale-proxy exists' \
  "$rollback_fixture/api-error.log" ||
  fail 'the rollback must report an ambiguous StatefulSet lookup'
assert_no_rollback_access_removal "$rollback_fixture/api-error.calls"
if grep --fixed-strings --quiet 'scale statefulset/tailscale-proxy' "$rollback_fixture/api-error.calls"; then
  fail 'the rollback must not claim it stopped the proxy after an API lookup failure'
fi

if ! run_tailnet_proxy_rollback absent >"$rollback_fixture/absent.log" 2>&1; then
  fail 'the rollback must allow NotFound when no proxy pods remain'
fi
if grep --fixed-strings --quiet 'scale statefulset/tailscale-proxy' "$rollback_fixture/absent.calls"; then
  fail 'the rollback must not scale a StatefulSet that is absent'
fi
grep --fixed-strings --quiet 'delete rolebinding' "$rollback_fixture/absent.calls" ||
  fail 'the rollback must remove access after confirming no proxy pods remain'
grep --fixed-strings --quiet 'delete networkpolicy tailscale-proxy-api-egress' "$rollback_fixture/absent.calls" ||
  fail 'the rollback must remove its API egress policy after confirming no proxy pods remain'

for unsafe_case in wait-error pods-error pods-remain orphan-pod; do
  if run_tailnet_proxy_rollback "$unsafe_case" >"$rollback_fixture/$unsafe_case.log" 2>&1; then
    fail "the rollback accepted an unconfirmed proxy stop: $unsafe_case"
  fi
  assert_no_rollback_access_removal "$rollback_fixture/$unsafe_case.calls"
done

if ! run_tailnet_proxy_rollback running >"$rollback_fixture/running.log" 2>&1; then
  fail 'the rollback must complete after the proxy pod is deleted'
fi
grep --fixed-strings --quiet 'scale statefulset/tailscale-proxy' "$rollback_fixture/running.calls" ||
  fail 'the rollback must scale an existing proxy StatefulSet to zero'
grep --fixed-strings --quiet 'wait --for=delete pod/tailscale-proxy-0' "$rollback_fixture/running.calls" ||
  fail 'the rollback must wait for the proxy pod to stop before removing access'
grep --fixed-strings --quiet 'delete rolebinding' "$rollback_fixture/running.calls" ||
  fail 'the rollback must remove access after confirming the proxy pod stopped'
grep --fixed-strings --quiet 'delete networkpolicy tailscale-proxy-api-egress' "$rollback_fixture/running.calls" ||
  fail 'the rollback must remove its API egress policy after confirming the proxy pod stopped'
if grep --extended-regexp --quiet '^delete (rolebinding|role) .*teagram-deployer-networkpolicy' \
  "$rollback_fixture/running.calls"; then
  fail 'the rollback must preserve Deploy. access to NetworkPolicies'
fi

backup_cronjob=deploy/k8s/base/postgres-backup/cronjob.yaml
backup_script=deploy/k8s/base/postgres-backup/backup.sh
backup_pvc=deploy/k8s/base/postgres-backup/pvc.yaml
[[ "$(yq -r '.spec.schedule' "$backup_cronjob")" == '17 3 * * *' &&
   "$(yq -r '.spec.timeZone' "$backup_cronjob")" == 'Etc/UTC' &&
   "$(yq -r '.spec.suspend' "$backup_cronjob")" == true &&
   "$(yq -r '.spec.concurrencyPolicy' "$backup_cronjob")" == Forbid &&
   "$(yq -r '.spec.successfulJobsHistoryLimit' "$backup_cronjob")" == 7 &&
   "$(yq -r '.spec.failedJobsHistoryLimit' "$backup_cronjob")" == 3 ]] ||
  fail 'the base backup CronJob must be suspended, UTC scheduled, serialized, and retain bounded history'
[[ "$(yq -r '.spec.resources.requests.storage' "$backup_pvc")" == 20Gi &&
   "$(yq -r '.spec.storageClassName' "$backup_pvc")" == local-path &&
   "$(yq -r '.spec.accessModes[0]' "$backup_pvc")" == ReadWriteOnce ]] ||
  fail 'the backup PVC must use the selected single-node local storage policy'
[[ "$(yq -r '.spec.jobTemplate.spec.template.spec.containers[] | select(.name == "backup") | .image' "$backup_cronjob")" == \
   'mirror.gcr.io/library/postgres:18-alpine@sha256:3b0ef735388b2cb097fc29b8bb421d86d57d0ddbe7aa1fbd6493e06d65028503' &&
   "$(yq -r '.spec.jobTemplate.spec.template.spec.containers[] | select(.name == "backup") | .securityContext.runAsUser' "$backup_cronjob")" == 70 &&
   "$(yq -r '.spec.jobTemplate.spec.template.spec.containers[] | select(.name == "backup") | .securityContext.readOnlyRootFilesystem' "$backup_cronjob")" == true ]] ||
  fail 'the backup Job must use its pinned PostgreSQL image with a read-only, non-root container'
for required_backup_line in \
  'pg_dump --format=custom --no-owner --no-acl' \
  'pg_restore --list' \
  'backup=verified path=%s bytes=%s sha256=%s' \
  'find "$backup_directory" -maxdepth 1 -type f' \
  'prune_backups'; do
  grep --fixed-strings --quiet -- "$required_backup_line" "$backup_script" ||
    fail 'the backup command must verify each dump and prune files to seven'
done
grep --fixed-strings --quiet 'postgres-backup' deploy/k8s/base/kustomization.yaml ||
  fail 'the production and CI base must include the backup resources'
grep --fixed-strings --quiet 'suspend: false' deploy/k8s/prod/postgres-backup-cronjob.yaml ||
  fail 'the production overlay must activate nightly backups after cutover'
bootstrap_backup_suspend="$(kubectl kustomize deploy/k8s/backup-bootstrap |
  yq eval-all -r 'select(.kind == "CronJob" and .metadata.name == "postgres-backup") | .spec.suspend' -)"
[[ "$bootstrap_backup_suspend" == false ]] ||
  fail 'the pre-deploy overlay must leave nightly backups active if deployment fails'
ci_backup_suspend="$(kubectl kustomize deploy/k8s/ci |
  yq eval-all -r 'select(.kind == "CronJob" and .metadata.name == "postgres-backup") | .spec.suspend' -)"
[[ "$ci_backup_suspend" == true ]] ||
  fail 'the CI overlay must keep nightly backups suspended'
grep --fixed-strings --quiet '../base/postgres-backup' deploy/k8s/backup-bootstrap/kustomization.yaml ||
  fail 'the pre-deploy overlay must provision the backup resources before taking its dump'
for rbac_line in \
  'resources: ["cronjobs", "jobs"]' \
  'resources: ["persistentvolumeclaims"]' \
  'verbs: ["create", "patch"]'; do
  grep --fixed-strings --quiet -- "$rbac_line" deploy/k8s/deployer-access.yaml ||
    fail 'the restricted deployer Role must manage backup Jobs and their PVC'
done
grep --fixed-strings --quiet 'expect_deployer_can_i yes create cronjobs.batch' deploy/k8s/bootstrap-alpha.sh ||
  fail 'alpha bootstrap must verify backup CronJob permissions'
grep --fixed-strings --quiet 'expect_deployer_can_i yes patch jobs.batch' deploy/k8s/bootstrap-alpha.sh ||
  fail 'alpha bootstrap must verify Job patch permission for failed migration retries'
grep --fixed-strings --quiet 'expect_deployer_can_i yes get jobs.batch' deploy/k8s/bootstrap-alpha.sh ||
  fail 'alpha bootstrap must verify Job inspection permission for deploy status checks'
grep --fixed-strings --quiet 'expect_deployer_can_i yes create persistentvolumeclaims' deploy/k8s/bootstrap-alpha.sh ||
  fail 'alpha bootstrap must verify backup PVC permissions'
grep --fixed-strings --quiet 'expect_deployer_can_i yes delete pods --namespace teagram' deploy/k8s/bootstrap-alpha.sh ||
  fail 'alpha bootstrap must verify the pod deletion permission needed for StatefulSet rollback'
grep --fixed-strings --quiet 'resources: ["pods"]' deploy/k8s/deployer-access.yaml ||
  fail 'the deployer Role must scope recovery deletion to namespace Pods'
grep --fixed-strings --quiet 'verbs: ["delete"]' deploy/k8s/deployer-access.yaml ||
  fail 'the deployer Role must allow deleting stuck StatefulSet Pods during rollback'
rbac_step="$(yq -r '.jobs."check-k8s-manifests".steps[] | select(.name == "Verify deployer RBAC and Pod Security boundaries") | .run' .github/workflows/ci.yml)"
[[ "$rbac_step" == *'expect_deployer_can_i yes delete pods --namespace teagram'* ]] ||
  fail 'the k3s integration must verify the deployer can delete recovery Pods'
[[ "$rbac_step" == *'expect_deployer_can_i yes patch jobs.batch --namespace teagram'* ]] ||
  fail 'the k3s integration must verify the deployer can expire a failed Job for retry'
[[ "$rbac_step" == *'expect_deployer_can_i yes get jobs.batch --namespace teagram'* ]] ||
  fail 'the k3s integration must verify the deployer can inspect failed Jobs'

release_script=deploy/k8s/deploy-release.sh
release_dry_run_line="$(grep -n -m 1 -F 'kubectl apply --dry-run=server --filename "$manifest"' "$release_script" | cut -d: -f1)"
release_apply_line="$(grep -n -m 1 -F 'kubectl apply --filename "$manifest"' "$release_script" | cut -d: -f1)"
release_marker_line="$(grep -n -m 1 -F 'rollout=verified sha=%s' "$release_script" | cut -d: -f1)"
release_smoke_line="$(grep -n -m 1 -F 'smoke-production.sh' "$release_script" | cut -d: -f1)"
release_maintenance_line="$(grep -n -m 1 -F 'initialize-channel-post-expiring-summaries.sh' "$release_script" | cut -d: -f1)"
[[ "$release_dry_run_line" =~ ^[0-9]+$ && "$release_apply_line" =~ ^[0-9]+$ &&
   "$release_smoke_line" =~ ^[0-9]+$ && "$release_maintenance_line" =~ ^[0-9]+$ &&
   "$release_marker_line" =~ ^[0-9]+$ &&
   "$release_dry_run_line" -lt "$release_apply_line" &&
   "$release_apply_line" -lt "$release_smoke_line" &&
   "$release_smoke_line" -lt "$release_maintenance_line" &&
   "$release_maintenance_line" -lt "$release_marker_line" ]] ||
  fail 'the deploy runner must dry-run, apply, smoke, initialize summaries, then emit the verified rollout marker'
grep --fixed-strings --quiet 'kubectl rollout undo "$resource"' "$release_script" ||
  fail 'the release runner must automatically undo changed workload revisions on failure'
grep --fixed-strings --quiet 'currentRevision == .status.updateRevision' "$release_script" ||
  fail 'the release runner must refuse an unstable StatefulSet rollback baseline'
grep --fixed-strings --quiet 'status.updatedReplicas' "$release_script" ||
  fail 'the release runner must require all workload replicas to be updated and ready before apply'
grep --fixed-strings --quiet 'wait_for_job "$migration_job" 900' "$release_script" ||
  fail 'the release runner must wait for the exact migration Job created by this apply'
grep --fixed-strings --quiet 'wait_for_job "$rustfs_job" 600' "$release_script" ||
  fail 'the release runner must wait for the exact RustFS initializer Job created by this apply'
grep --fixed-strings --quiet 'Failed" and .status == "True"' "$release_script" ||
  fail 'the release runner must detect a failed Job immediately and recycle failed prior Jobs'
grep --fixed-strings --quiet 'kubectl logs "job/$job"' "$release_script" ||
  fail 'the release runner must include failed migration and initializer logs in the Deploy. run'
grep --fixed-strings --quiet 'kubectl rollout status "$resource"' "$release_script" ||
  fail 'the release runner must wait for every production workload rollout'
grep --fixed-strings --quiet 'kubectl delete pod "$pod"' "$release_script" ||
  fail 'the release runner must replace failed StatefulSet Pods after reverting their template'
grep --fixed-strings --quiet 'controller-revision-hash' "$release_script" ||
  fail 'the release runner must preserve Pods already on the restored StatefulSet revision'
grep --fixed-strings --quiet 'expected exactly one ready k3s tunnel connector' "$release_script" ||
  fail 'the production release must leave exactly one ready k3s tunnel connector'

maintenance_job=deploy/k8s/initialize-channel-post-expiring-summaries-job.yaml
[[ "$(yq -r '.kind' "$maintenance_job")" == Job &&
   "$(yq -r '.spec.template.spec.containers[] | select(.name == "initialize") | .command | join(" ")' "$maintenance_job")" == '/usr/local/bin/teagramd' &&
   "$(yq -r '.spec.template.spec.containers[] | select(.name == "initialize") | .args | join(" ")' "$maintenance_job")" == 'maintenance initialize-channel-post-expiring-summaries' &&
   "$(yq -r '.spec.template.spec.containers[] | select(.name == "initialize") | .env[] | select(.name == "TG_POSTGRES_DSN") | .valueFrom.secretKeyRef.name' "$maintenance_job")" == postgres-credentials &&
   "$(yq -r '.spec.template.spec.containers[] | select(.name == "initialize") | .env[] | select(.name == "TG_AUTHKEY_ENC_KEY_FILE") | .value' "$maintenance_job")" == /var/lib/teagramd/enc_key.hex &&
   "$(yq -r '.spec.template.spec.containers[] | select(.name == "initialize") | .volumeMounts[] | select(.name == "key-data") | .readOnly' "$maintenance_job")" == true &&
   "$(yq -r '.spec.template.spec.volumes[] | select(.name == "key-data") | .persistentVolumeClaim.claimName' "$maintenance_job")" == key-data-teagramd-0 &&
   "$(yq -r '.spec.template.spec.automountServiceAccountToken' "$maintenance_job")" == false ]] ||
  fail 'the summary maintenance Job must use the documented command with only its database secret and read-only identity PVC'

bash deploy/k8s/test-initialize-channel-post-expiring-summaries.sh >/dev/null ||
  fail 'the summary maintenance runner must continue until zero remain and fail closed on invalid batches'
bash deploy/k8s/test-postgres-backup-retention.sh >/dev/null ||
  fail 'backup retention must keep the seven newest verified dumps and clean stale temporary dumps'
bash deploy/k8s/test-postgres-backup-restore-guard.sh >/dev/null ||
  fail 'the destructive restore test must refuse non-GitHub and non-CI clusters'

backup_restore_test=deploy/k8s/test-postgres-backup-restore.sh
for restore_line in \
  'GITHUB_ACTIONS:-' \
  'https://127.0.0.1:6443' \
  'kubectl create job "$backup_job" --from=cronjob/postgres-backup' \
  'dropdb --username=postgres --force teagram' \
  'kubectl apply --filename deploy/k8s/test-postgres-backup-restore-job.yaml' \
  '--user "$(id -u):$(id -g)"' \
  '--username-file /username' \
  'backup=restore-sign-in-verified'; do
  grep --fixed-strings --quiet -- "$restore_line" "$backup_restore_test" ||
    fail 'the throwaway backup regression must restore a dump and sign in with the restored account'
done
release_test_step="$(yq -r '.jobs."test-k8s-deploy".steps[] | select(.name == "Verify successful release and automatic bad-image rollback") | .run' .github/workflows/ci.yml)"
[[ "$release_test_step" == *'deploy/k8s/deploy-release.sh'* &&
   "$release_test_step" == *'rollback=verified resource=statefulset/teagramd'* &&
   "$release_test_step" == *'test "$restored_image" = "$previous_image"'* ]] ||
  fail 'the K3s integration must force a bad deploy and verify automatic revision restoration'
early_rollback_test_step="$(yq -r '.jobs."check-deploy-workflow".steps[] | select(.name == "Test early post-apply release rollback") | .run' .github/workflows/ci.yml)"
[[ "$early_rollback_test_step" == 'python3 -B deploy/k8s/test-deploy-release-early-rollback.py' &&
   -f deploy/k8s/test-deploy-release-early-rollback.py ]] ||
  fail 'the deploy workflow must run the early post-apply template rollback regression'

printf 'K8s deployment contract passed\n'
