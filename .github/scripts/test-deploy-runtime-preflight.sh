#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
workflow="$repo_root/.github/workflows/deploy.yml"
tmp_dir="$(mktemp -d)"
trap 'rm -rf -- "$tmp_dir"' EXIT
mkdir -p "$tmp_dir/bin" "$tmp_dir/runner"

fail() {
  printf 'Deploy runtime preflight test: %s\n' "$1" >&2
  exit 1
}

extract_step() {
  local step_name="$1" destination="$2"
  yq -r ".jobs.deploy-server.steps[] | select(.name == \"$step_name\") | .run" \
    "$workflow" > "$destination"
  [[ -s "$destination" ]] || fail "workflow step $step_name has no shell script"
}

cat > "$tmp_dir/bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == run && "${2:-}" == download ]]; then
  shift 2
  run_id="$1"
  shift
  destination=''
  artifact_name=''
  while (($#)); do
    case "$1" in
      --repo) [[ "$2" == "$GITHUB_REPOSITORY" ]] || exit 91; shift 2 ;;
      --name) artifact_name="$2"; shift 2 ;;
      --dir) destination="$2"; shift 2 ;;
      *) exit 92 ;;
    esac
  done
  [[ "$run_id" == "$CI_RUN_ID" &&
    "$artifact_name" == "k8s-image-digests-$DEPLOY_SHA" &&
    -n "$destination" ]] || exit 93
  mkdir -p "$destination"
  cp "$DEPLOY_RUNTIME_ARTIFACT_FIXTURE" "$destination/k8s-image-digests.json"
  exit 0
fi
if [[ "${1:-}" == attestation && "${2:-}" == verify ]]; then
  printf '%s\n' "$*" >> "$DEPLOY_RUNTIME_ATTESTATION_LOG"
  exit 0
fi
printf 'unexpected gh command: %s\n' "$*" >&2
exit 94
SH

cat > "$tmp_dir/bin/kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$DEPLOY_RUNTIME_KUBECTL_LOG"
case "$*" in
  *'config view --minify --output jsonpath={.clusters[0].cluster.server}')
    printf '%s\n' "$DEPLOY_RUNTIME_API_SERVER"
    ;;
  *'config view --minify --output jsonpath={.clusters[0].cluster.certificate-authority-data}')
    printf '%s\n' "$DEPLOY_RUNTIME_CA_DATA"
    ;;
  *'config view --minify --output jsonpath={.clusters[0].cluster.insecure-skip-tls-verify}')
    printf '%s\n' "$DEPLOY_RUNTIME_INSECURE_SKIP"
    ;;
  'auth can-i '*) printf '%s\n' "${DEPLOY_RUNTIME_CAN_I:-yes}" ;;
  'logs job/'*) printf '%s\n' "${DEPLOY_RUNTIME_BACKUP_LOG:-backup=verified path=/backups/teagram-20261110T010203Z-ci.dump bytes=17 sha256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}" ;;
  *) ;;
esac
SH
chmod 0755 "$tmp_dir/bin/gh" "$tmp_dir/bin/kubectl"

export PATH="$tmp_dir/bin:$PATH"
export RUNNER_TEMP="$tmp_dir/runner"
export GITHUB_REPOSITORY=teagramhq/teagram-server
export CI_RUN_ID=123456
export DEPLOY_SHA=0123456789abcdef0123456789abcdef01234567
export GH_TOKEN=test-token
export DEPLOY_RUNTIME_KUBECTL_LOG="$tmp_dir/kubectl.log"
export DEPLOY_RUNTIME_ATTESTATION_LOG="$tmp_dir/attestations.log"
export DEPLOY_RUNTIME_API_SERVER=https://teagram.tailaa4918.ts.net:6443
export DEPLOY_RUNTIME_CA_DATA=Y2x1c3Rlci1jYQ==
export DEPLOY_RUNTIME_INSECURE_SKIP=''
export KUBECONFIG_CONTENT=fixture-kubeconfig
export BACKUP_JOB=postgres-backup-predeploy-123456-1
: > "$DEPLOY_RUNTIME_KUBECTL_LOG"
: > "$DEPLOY_RUNTIME_ATTESTATION_LOG"

python3 - "$tmp_dir/digests.json" "$DEPLOY_SHA" <<'PY'
import json
import pathlib
import sys

images = (
    "ghcr.io/teagramhq/teagramd",
    "ghcr.io/teagramhq/teagram-migrations",
    "ghcr.io/teagramhq/teagramd-mc-init",
    "ghcr.io/teagramhq/link-edge-landing",
    "ghcr.io/teagramhq/link-edge-selector",
)
entries = [
    {"name": image, "reference": ""}
    for image in images
]
for index, entry in enumerate(entries, start=1):
    entry["reference"] = f"{entry['name']}@sha256:{index:02x}" + "0" * 62
pathlib.Path(sys.argv[1]).write_text(
    json.dumps({"commit": sys.argv[2], "images": entries}), encoding="utf-8"
)
PY
export DEPLOY_RUNTIME_ARTIFACT_FIXTURE="$tmp_dir/digests.json"

extract_step 'Download the trusted K8s image digests' "$tmp_dir/download.sh"
: > "$tmp_dir/download-output"
GITHUB_OUTPUT="$tmp_dir/download-output" bash "$tmp_dir/download.sh"
digest_file="$(sed -n 's/^path=//p' "$tmp_dir/download-output")"
[[ -s "$digest_file" ]] || fail 'artifact download did not write the advertised digest path'

mkdir -p "$tmp_dir/pin-root/deploy/k8s/prod" "$tmp_dir/pin-root/migrations"
cp "$repo_root/deploy/k8s/prod/kustomization.yaml" \
  "$tmp_dir/pin-root/deploy/k8s/prod/kustomization.yaml"
cp "$repo_root/deploy/k8s/prod/source-revision" \
  "$tmp_dir/pin-root/deploy/k8s/prod/source-revision"
: > "$tmp_dir/pin-root/migrations/20261110000001_runtime_test.sql"
python3 "$repo_root/.github/scripts/update-k8s-prod-pins.py" \
  --root "$tmp_dir/pin-root" --digest-file "$digest_file" \
  --expected-sha "$DEPLOY_SHA" > "$tmp_dir/pin-outputs"
grep --fixed-strings --quiet "sha=$DEPLOY_SHA" "$tmp_dir/pin-outputs" ||
  fail 'the downloaded artifact could not be consumed by production pin resolution'

extract_step 'Verify build provenance for every production image' "$tmp_dir/attest.sh"
GITHUB_OUTPUT="$tmp_dir/attest-output" \
TEAGRAMD_DIGEST=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
MIGRATIONS_DIGEST=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
MC_INIT_DIGEST=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc \
LINK_LANDING_DIGEST=sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd \
LINK_SELECTOR_DIGEST=sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee \
  bash "$tmp_dir/attest.sh"
[[ "$(wc -l < "$DEPLOY_RUNTIME_ATTESTATION_LOG" | tr -d '[:space:]')" == 5 ]] ||
  fail 'the provenance verifier did not check all five production images'
while IFS= read -r attestation; do
  [[ "$attestation" == *'--repo teagramhq/teagram-server'* &&
    "$attestation" == *'--source-ref refs/heads/main'* &&
    "$attestation" == *"--source-digest $DEPLOY_SHA"* &&
    "$attestation" == *'--signer-workflow teagramhq/teagram-server/.github/workflows/ci.yml'* ]] ||
    fail 'a provenance verification omitted its repository, source, or signer constraints'
done < "$DEPLOY_RUNTIME_ATTESTATION_LOG"
for image in \
  ghcr.io/teagramhq/teagramd \
  ghcr.io/teagramhq/teagram-migrations \
  ghcr.io/teagramhq/teagramd-mc-init \
  ghcr.io/teagramhq/link-edge-landing \
  ghcr.io/teagramhq/link-edge-selector; do
  grep --fixed-strings --quiet "oci://$image@" "$DEPLOY_RUNTIME_ATTESTATION_LOG" ||
    fail "provenance verification omitted $image"
done

extract_step 'Install the restricted cluster kubeconfig' "$tmp_dir/kubeconfig.sh"
: > "$tmp_dir/kubeconfig-env"
: > "$tmp_dir/kubeconfig-output"
GITHUB_ENV="$tmp_dir/kubeconfig-env" GITHUB_OUTPUT="$tmp_dir/kubeconfig-output" \
  bash "$tmp_dir/kubeconfig.sh"
expected_host='teagram.tailaa4918.ts.net'
[[ "$(stat -c '%a' "$RUNNER_TEMP/teagram-k3s.kubeconfig")" == 600 &&
  "$(cat "$tmp_dir/kubeconfig-env")" == "KUBECONFIG=$RUNNER_TEMP/teagram-k3s.kubeconfig" &&
  "$(cat "$tmp_dir/kubeconfig-output")" == "api_host=$expected_host" ]] ||
  fail 'kubeconfig installation or API host parsing produced an unexpected result'
if DEPLOY_RUNTIME_API_SERVER=https://:6443 \
  GITHUB_ENV="$tmp_dir/kubeconfig-env" GITHUB_OUTPUT="$tmp_dir/kubeconfig-output" \
  bash "$tmp_dir/kubeconfig.sh" >"$tmp_dir/bad-host.out" 2>&1; then
  fail 'kubeconfig host parsing accepted an empty API host'
fi
if DEPLOY_RUNTIME_API_SERVER=https://other.example:6443 \
  GITHUB_ENV="$tmp_dir/kubeconfig-env" GITHUB_OUTPUT="$tmp_dir/kubeconfig-output" \
  bash "$tmp_dir/kubeconfig.sh" >"$tmp_dir/wrong-endpoint.out" 2>&1; then
  fail 'kubeconfig validation accepted a non-tailnet API endpoint'
fi
if DEPLOY_RUNTIME_CA_DATA='' \
  GITHUB_ENV="$tmp_dir/kubeconfig-env" GITHUB_OUTPUT="$tmp_dir/kubeconfig-output" \
  bash "$tmp_dir/kubeconfig.sh" >"$tmp_dir/missing-ca.out" 2>&1; then
  fail 'kubeconfig validation accepted missing cluster CA data'
fi
if DEPLOY_RUNTIME_INSECURE_SKIP=true \
  GITHUB_ENV="$tmp_dir/kubeconfig-env" GITHUB_OUTPUT="$tmp_dir/kubeconfig-output" \
  bash "$tmp_dir/kubeconfig.sh" >"$tmp_dir/insecure-tls.out" 2>&1; then
  fail 'kubeconfig validation accepted insecure TLS verification'
fi

extract_step 'Validate the production release manifest' "$tmp_dir/preflight.sh"
: > "$DEPLOY_RUNTIME_KUBECTL_LOG"
bash "$tmp_dir/preflight.sh"
: > "$DEPLOY_RUNTIME_KUBECTL_LOG"
if DEPLOY_RUNTIME_CAN_I=no bash "$tmp_dir/preflight.sh" \
  >"$tmp_dir/denied.out" 2>&1; then
  fail 'deployer authorization preflight accepted a denied permission'
fi
! grep --fixed-strings --quiet 'apply --dry-run=server' "$DEPLOY_RUNTIME_KUBECTL_LOG" ||
  fail 'manifest validation proceeded after a denied authorization check'

extract_step 'Take and verify a pre-deploy PostgreSQL backup' "$tmp_dir/backup.sh"
: > "$DEPLOY_RUNTIME_KUBECTL_LOG"
GITHUB_OUTPUT="$tmp_dir/backup-output" bash "$tmp_dir/backup.sh"
grep --extended-regexp --quiet \
  '^backup=verified path=/backups/teagram-[A-Za-z0-9TZ._-]+\.dump bytes=[1-9][0-9]* sha256=[0-9a-f]{64}$' \
  "$RUNNER_TEMP/predeploy-backup.log" ||
  fail 'the backup verification step did not accept its documented log format'
if DEPLOY_RUNTIME_BACKUP_LOG='backup=verified path=/backups/newest.dump bytes=17 sha256=invalid' \
  GITHUB_OUTPUT="$tmp_dir/backup-output" bash "$tmp_dir/backup.sh" \
  >"$tmp_dir/bad-backup.out" 2>&1; then
  fail 'the backup verification accepted a malformed backup record'
fi

printf 'Deploy workflow runtime preflight tests passed\n'
