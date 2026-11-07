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
if [[ "$(yq -r '.jobs."test-k8s-deploy".permissions.packages // "none"' .github/workflows/ci.yml)" != none ]]; then
  fail 'the k3s deployment lane must not require GHCR package credentials'
fi
k8s_deploy_run="$(yq -r '.jobs."test-k8s-deploy".steps[] | .run // ""' .github/workflows/ci.yml)"
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
grep --fixed-strings --quiet 'TG_MEDIA_KEY_CURRENT=$TG_MEDIA_KEY_CURRENT' deploy/k8s/bootstrap-alpha.sh ||
  fail 'bootstrap must preserve the live media encryption key version in teagramd-runtime'
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
grep --fixed-strings --quiet 'CUTOVER_TEST_DROP_S3_OBJECT' deploy/k8s/test-cutover-alpha-k3s.sh ||
  fail 'the cutover integration must fail closed on a per-object S3 checksum mismatch'
grep --fixed-strings --quiet 'check_source_media_keys' deploy/k8s/cutover-alpha.sh ||
  fail 'the cutover runner must verify that the target encrypted-media key set matches the source'
grep --fixed-strings --quiet 'check_source_blob_credentials' deploy/k8s/cutover-alpha.sh ||
  fail 'the cutover runner must verify that the target S3 credentials match the source'
grep --fixed-strings --quiet 'preserve_media_key_version' deploy/k8s/cutover-alpha.sh ||
  fail 'the cutover runner must preserve the source media encryption key version'
grep --fixed-strings --quiet 'source and target S3 object counts, byte totals, or per-object SHA-256 checksums do not match' deploy/k8s/cutover-alpha.sh ||
  fail 'the cutover runner must compare source and target S3 inventories before switching routes'
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
grep --fixed-strings --quiet '/run/teagramd/blob-mode/.' deploy/k8s/test-cutover-alpha-incus.sh ||
  fail 'the actual cutover runner test adapter must read blob-mode authority from the source PVC'
grep --fixed-strings --quiet 'cutover-alpha.sh --apply --test-mode' deploy/k8s/test-cutover-alpha-k3s.sh ||
  fail 'the k3s cutover test must execute the actual apply runner in its disposable namespace'
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

printf 'K8s deployment contract passed\n'
