#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

: "${KUBECONFIG:?KUBECONFIG must point to the disposable CI cluster}"
: "${RUNNER_TEMP:?RUNNER_TEMP must point to the disposable runner directory}"

readonly SOURCE_NAMESPACE=teagram
readonly TARGET_NAMESPACE=teagram-cutover
readonly CUTOVER_IMAGE=ghcr.io/teagramhq/teagramd:ci
tmp_dir=$(mktemp -d "$RUNNER_TEMP/teagram-cutover.XXXXXX")
port_forward_pid=''
linkselector_forward_pid=''
source_container=''

stop_port_forward() {
  if [[ -n $port_forward_pid ]]; then
    kill "$port_forward_pid" 2>/dev/null || true
    wait "$port_forward_pid" 2>/dev/null || true
    port_forward_pid=''
  fi
  if [[ -n $linkselector_forward_pid ]]; then
    kill "$linkselector_forward_pid" 2>/dev/null || true
    wait "$linkselector_forward_pid" 2>/dev/null || true
    linkselector_forward_pid=''
  fi
}

cleanup() {
  local status=$? mock_pid
  trap - EXIT
  stop_port_forward
  if [[ -n $source_container ]]; then
    docker rm --force "$source_container" >/dev/null 2>&1 || true
  fi
  if [[ -d $tmp_dir/incus-state/units ]]; then
    for pid_file in "$tmp_dir"/incus-state/units/*.pid; do
      [[ -f $pid_file ]] || continue
      mock_pid=$(<"$pid_file")
      kill "$mock_pid" 2>/dev/null || true
    done
  fi
  kubectl delete pod cutover-key-reader --namespace "$TARGET_NAMESPACE" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  if (( status == 0 )); then
    kubectl delete namespace "$TARGET_NAMESPACE" --wait=false >/dev/null 2>&1 || true
    rm -rf -- "$RUNNER_TEMP/teagram-cutover-runner"
  fi
  kubectl delete pod cutover-key-reader --namespace "$SOURCE_NAMESPACE" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete pod cutover-blob-seed --namespace "$SOURCE_NAMESPACE" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete pod cutover-blob-mode-seed --namespace "$SOURCE_NAMESPACE" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete pod cutover-s3-client --namespace "$SOURCE_NAMESPACE" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete job cutover-account-seed --namespace "$SOURCE_NAMESPACE" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  rm -rf -- "$tmp_dir"
  exit "$status"
}
trap cleanup EXIT
trap 'printf "cutover test failed at line %s: %s\n" "$LINENO" "$BASH_COMMAND" >&2' ERR

expect_log() {
  local expected=$1 log_file=$2
  if ! grep --fixed-strings --quiet -- "$expected" "$log_file"; then
    printf 'expected log message not found: %s\n' "$expected" >&2
    cat "$log_file" >&2
    return 1
  fi
}

copy_secret() {
  local name=$1
  kubectl get secret "$name" --namespace "$SOURCE_NAMESPACE" --output json |
    jq --arg namespace "$TARGET_NAMESPACE" '
      del(.metadata.creationTimestamp, .metadata.managedFields, .metadata.resourceVersion, .metadata.uid)
      | .metadata.namespace = $namespace
    ' |
    kubectl apply --filename - >/dev/null
}

copy_configmap() {
  local name=$1 source_name
  source_name=$(kubectl get configmaps --namespace "$SOURCE_NAMESPACE" --output json |
    jq -er --arg name "$name" '
      [.items[].metadata.name | select(. == $name or startswith($name + "-"))]
      | if index($name) then $name
        elif length == 1 then .[0]
        elif length == 0 then error("ConfigMap not found: " + $name)
        else error("multiple generated ConfigMaps found for: " + $name)
        end
    ')
  kubectl get configmap "$source_name" --namespace "$SOURCE_NAMESPACE" --output json |
    jq --arg namespace "$TARGET_NAMESPACE" --arg name "$name" '
      del(.metadata.creationTimestamp, .metadata.managedFields, .metadata.resourceVersion, .metadata.uid)
      | .metadata.namespace = $namespace
      | .metadata.name = $name
    ' |
    kubectl apply --filename - >/dev/null
}

create_target_namespace() {
  kubectl create namespace "$TARGET_NAMESPACE" >/dev/null
  kubectl label namespace "$TARGET_NAMESPACE" cutover-test=true >/dev/null
  for secret in postgres-credentials rustfs-credentials \
    teagramd-blob-credentials teagramd-media-keys; do
    copy_secret "$secret"
  done
  for configmap in teagramd-runtime link-edge-runtime; do
    copy_configmap "$configmap"
  done
  printf 'ci-placeholder-token' > "$tmp_dir/cloudflared-token"
  kubectl create secret generic cloudflared-token --namespace "$TARGET_NAMESPACE" \
    --from-file=token="$tmp_dir/cloudflared-token" >/dev/null
}

assert_no_pull_secret() {
  local namespace=$1 secret
  secret=$(kubectl get secret ghcr-pull --namespace "$namespace" \
    --ignore-not-found --output name)
  [[ -z $secret ]] || {
    printf 'namespace %s unexpectedly contains a GHCR pull Secret\n' "$namespace" >&2
    return 1
  }
}

start_port_forward() {
  local namespace=$1 attempt
  local -a ports=(12443:2443)
  # The source smoke needs only MTProto; the target also checks web and admin.
  [[ $namespace == "$TARGET_NAMESPACE" ]] && ports+=(12444:2444 12445:2445 12447:2447)
  kubectl port-forward --address 127.0.0.1 --namespace "$namespace" \
    service/teagramd "${ports[@]}" > "$tmp_dir/port-forward-$namespace.log" 2>&1 &
  port_forward_pid=$!
  for ((attempt = 1; attempt <= 30; attempt++)); do
    if [[ $namespace == "$TARGET_NAMESPACE" ]]; then
      curl --silent --output /dev/null --max-time 2 \
        http://127.0.0.1:12444/.well-known/teagramd/client && return 0
    elif grep --fixed-strings --quiet 'Forwarding from 127.0.0.1:12443' "$tmp_dir/port-forward-$namespace.log"; then
      return 0
    fi
    sleep 2
  done
  cat "$tmp_dir/port-forward-$namespace.log" >&2
  return 1
}

start_linkselector_forward() {
  local namespace=$1 attempt
  kubectl port-forward --address 127.0.0.1 --namespace "$namespace" \
    service/linkselector 12881:8081 > "$tmp_dir/linkselector-port-forward-$namespace.log" 2>&1 &
  linkselector_forward_pid=$!
  for ((attempt = 1; attempt <= 30; attempt++)); do
    if curl --fail --silent --output /dev/null --max-time 2 \
      http://127.0.0.1:12881/+k3s-smoke; then
      return 0
    fi
    sleep 2
  done
  cat "$tmp_dir/linkselector-port-forward-$namespace.log" >&2
  return 1
}

smoke_restored_account() {
  timeout --signal=TERM --kill-after=5s 45s docker run --rm --network host \
    --user 0:0 \
    --volume "$tmp_dir/client.json:/client.json:ro" \
    --volume "$tmp_dir/username:/username:ro" \
    --volume "$tmp_dir/password:/password:ro" \
    --entrypoint /usr/local/bin/mtproto-smoke \
    ghcr.io/teagramhq/teagramd:ci \
    --endpoint 127.0.0.1:12443 \
    --discovery-file /client.json \
    --username-file /username \
    --password-file /password
}

smoke_upload_preserved_media() {
  timeout --signal=TERM --kill-after=5s 120s docker run --rm --network host \
    --volume "$tmp_dir/media:/media" \
    --volume "$tmp_dir/username:/username:ro" \
    --volume "$tmp_dir/password:/password:ro" \
    --volume "$tmp_dir/source-client.json:/client.json:ro" \
    --entrypoint /usr/local/bin/mtproto-smoke \
    ghcr.io/teagramhq/teagramd:ci \
    --endpoint 127.0.0.1:12443 \
    --discovery-file /client.json \
    --username-file /username \
    --password-file /password \
    --media-upload-file /media/payload.bin \
    --media-manifest-file /media/reference.json
}

smoke_download_preserved_media() {
  timeout --signal=TERM --kill-after=5s 120s docker run --rm --network host \
    --volume "$tmp_dir/media:/media" \
    --volume "$tmp_dir/username:/username:ro" \
    --volume "$tmp_dir/password:/password:ro" \
    --volume "$tmp_dir/client.json:/client.json:ro" \
    --entrypoint /usr/local/bin/mtproto-smoke \
    ghcr.io/teagramhq/teagramd:ci \
    --endpoint 127.0.0.1:12443 \
    --discovery-file /client.json \
    --username-file /username \
    --password-file /password \
    --media-download-file /media/downloaded.bin \
    --media-manifest-file /media/reference.json
  cmp -- "$tmp_dir/media/payload.bin" "$tmp_dir/media/downloaded.bin"
}

smoke_bot_api() {
  python3 - 127.0.0.1 12447 <<'PY'
import socket
import sys

host, port = sys.argv[1], int(sys.argv[2])
request = (
    b"PROXY TCP4 100.64.1.2 100.64.0.1 1234 2447\r\n"
    b"GET /bot0:cutover-smoke-invalid/getMe HTTP/1.1\r\n"
    b"Host: teagram.invalid\r\n"
    b"Connection: close\r\n\r\n"
)
with socket.create_connection((host, port), timeout=5) as connection:
    connection.settimeout(5)
    connection.sendall(request)
    response = bytearray()
    while len(response) < 65536:
        chunk = connection.recv(4096)
        if not chunk:
            break
        response.extend(chunk)
if not response.startswith(b"HTTP/1.1 401 ") or b'"description":"Unauthorized"' not in response:
    raise SystemExit("unexpected Bot API response")
PY
}

printf 'cutover_smoke\n' > "$tmp_dir/username"
openssl rand -hex 24 > "$tmp_dir/password"
chmod 0600 "$tmp_dir/username" "$tmp_dir/password"
kubectl apply --filename deploy/k8s/test-cutover-account-job.yaml >/dev/null
seed_pod=$(kubectl get pods --namespace "$SOURCE_NAMESPACE" --selector=job-name=cutover-account-seed \
  --output 'jsonpath={.items[0].metadata.name}')
[[ -n $seed_pod ]]
kubectl wait --for=jsonpath='{.status.phase}'=Running "pod/$seed_pod" \
  --namespace "$SOURCE_NAMESPACE" --timeout=180s >/dev/null
kubectl attach "$seed_pod" --namespace "$SOURCE_NAMESPACE" \
  --stdin --container create-account < "$tmp_dir/password" >/dev/null
kubectl wait --for=condition=complete job/cutover-account-seed \
  --namespace "$SOURCE_NAMESPACE" --timeout=180s >/dev/null
printf 'source account seeded\n'
bash deploy/k8s/test-cutover-dump-upload.sh "$SOURCE_NAMESPACE" postgres-0

assert_no_pull_secret "$SOURCE_NAMESPACE"
kubectl label namespace "$SOURCE_NAMESPACE" cutover-test=true --overwrite >/dev/null
create_target_namespace
assert_no_pull_secret "$TARGET_NAMESPACE"

kubectl apply --filename deploy/k8s/test-cutover-blob-seed.yaml >/dev/null
kubectl wait --for=condition=Ready pod/cutover-blob-seed \
  --namespace "$SOURCE_NAMESPACE" --timeout=180s >/dev/null
kubectl delete pod cutover-blob-seed --namespace "$SOURCE_NAMESPACE" --wait=true --timeout=180s >/dev/null
kubectl apply --filename deploy/k8s/test-cutover-blob-mode-seed.yaml >/dev/null
kubectl wait --for=condition=Ready pod/cutover-blob-mode-seed \
  --namespace "$SOURCE_NAMESPACE" --timeout=180s >/dev/null
mkdir -m 0700 "$tmp_dir/blob-mode"
python3 - "$tmp_dir/blob-mode" <<'PY'
import hashlib
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
journal = root / "journal"
journal.mkdir(mode=0o755)
local_backend = {"kind": "local", "dir": "/var/lib/teagramd-blobs"}
s3_backend = {"kind": "s3", "endpoint": "http://rustfs:9000",
              "bucket": "teagram", "prefix": "teagramd/"}
records = [
    {
        "schema": "teagram.blob-mode/v1", "generation": 1,
        "transition_id": "123e4567-e89b-12d3-a456-426614174000", "supersedes": None,
        "outcome": "initial-local", "backend": local_backend,
        "volumes": {"tgblobs": "teagram_tgblobs", "rustfsdata": None},
        "evidence": {"report_sha256": hashlib.sha256(b"initial-local").hexdigest()},
        "published_at": "2026-11-07T00:00:00Z",
    },
    {
        "schema": "teagram.blob-mode/v1", "generation": 2,
        "transition_id": "123e4567-e89b-12d3-a456-426614174001",
        "supersedes": "123e4567-e89b-12d3-a456-426614174000",
        "outcome": "s3-accepted", "backend": s3_backend,
        "volumes": {"tgblobs": "teagram_tgblobs", "rustfsdata": "teagram_rustfsdata"},
        "evidence": {
            "report_sha256": hashlib.sha256(b"s3-accepted").hexdigest(),
            "source_manifest_sha256": hashlib.sha256(b"source").hexdigest(),
            "destination_manifest_sha256": hashlib.sha256(b"destination").hexdigest(),
            "object_count": 0, "byte_total": 0, "copy_passes": 1,
        },
        "published_at": "2026-11-07T00:01:00Z",
    },
    {
        "schema": "teagram.blob-mode/v1", "generation": 3,
        "transition_id": "123e4567-e89b-12d3-a456-426614174002",
        "supersedes": "123e4567-e89b-12d3-a456-426614174001",
        "outcome": "encrypted", "backend": s3_backend,
        "volumes": {"tgblobs": "teagram_tgblobs", "rustfsdata": "teagram_rustfsdata"},
        "evidence": {
            "report_sha256": hashlib.sha256(b"encrypted").hexdigest(),
            "encrypted_inventory_sha256": hashlib.sha256(b"encrypted-inventory").hexdigest(),
            "object_count": 0, "byte_total": 0, "key_version": 1, "writes": "stopped",
        },
        "published_at": "2026-11-07T00:02:00Z",
    },
]
latest = b""
for index, record in enumerate(records, 1):
    latest = (json.dumps(record, separators=(",", ":")) + "\n").encode()
    entry = journal / f"{index:010d}.json"
    entry.write_bytes(latest)
    entry.chmod(0o644)
(root / "mode.json").write_bytes(latest)
(root / "mode.json").chmod(0o644)
PY
# Make the mounted PVC writable only for this root-only fixture seeding step.
# Exclude directory entries so tar cannot reset the PVC or journal permissions.
kubectl exec --namespace "$SOURCE_NAMESPACE" pod/cutover-blob-mode-seed -c seed -- chmod 0700 /blob-mode
(
  cd "$tmp_dir/blob-mode"
  tar -cf - mode.json journal/*.json
) |
  kubectl exec --namespace "$SOURCE_NAMESPACE" --stdin pod/cutover-blob-mode-seed -c seed -- \
    tar -xf - -C /blob-mode
kubectl exec --namespace "$SOURCE_NAMESPACE" pod/cutover-blob-mode-seed -c seed -- \
  sh -ec 'chown -R 0:0 /blob-mode; chmod 0755 /blob-mode /blob-mode/journal; chmod 0644 /blob-mode/mode.json /blob-mode/journal/*.json'
kubectl delete pod cutover-blob-mode-seed --namespace "$SOURCE_NAMESPACE" --wait=true --timeout=180s >/dev/null
kubectl apply --filename deploy/k8s/test-cutover-key-reader.yaml >/dev/null
kubectl wait --for=condition=Ready pod/cutover-key-reader \
  --namespace "$SOURCE_NAMESPACE" --timeout=180s >/dev/null
# Copy the disposable identity into the actual distroless image so the source
# adapter exercises Docker's archive API against a container without a shell.
source_container=$(docker create ghcr.io/teagramhq/teagramd:ci)
kubectl exec --namespace "$SOURCE_NAMESPACE" pod/cutover-key-reader -c reader -- \
  tar -cf - -C /var/lib/teagramd server_key.pem enc_key.hex |
  docker cp - "$source_container:/var/lib/teagramd/"

mkdir -m 0700 "$tmp_dir/media"
python3 - "$tmp_dir/media/payload.bin" <<'PY'
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
path.write_bytes(bytes((index * 73 + 19) % 256 for index in range(512 * 1024 + 137)))
path.chmod(0o600)
PY
source_pod=$(kubectl get pods --namespace "$SOURCE_NAMESPACE" \
  --selector=app.kubernetes.io/name=teagramd --output 'jsonpath={.items[0].metadata.name}')
kubectl exec --namespace "$SOURCE_NAMESPACE" "$source_pod" -- \
  /usr/local/bin/teagramd client-config > "$tmp_dir/source-client.json"
chmod 0600 "$tmp_dir/source-client.json"
start_port_forward "$SOURCE_NAMESPACE"
smoke_upload_preserved_media
stop_port_forward
[[ -s $tmp_dir/media/reference.json ]]
printf 'encrypted S3 media object written before cutover\n'

mkdir -p "$tmp_dir/bin" "$tmp_dir/incus-state/units"
ln -s "$repo_root/deploy/k8s/test-cutover-alpha-incus.sh" "$tmp_dir/bin/incus"
python3 - "$tmp_dir/incus-state/serve.json" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
path.write_text(json.dumps({
    "Web": {"alpha.example.ts.net:443": {"Handlers": {
        "/": {"Proxy": "http://127.0.0.1:8081"},
        "/apiws": {"Proxy": "http://127.0.0.1:2444/apiws"},
        "/admin": {"Proxy": "http://127.0.0.1:2445/admin"},
        "/.well-known/teagramd/client": {"Proxy": "http://127.0.0.1:2444/.well-known/teagramd/client"},
        "/.well-known/telegramd/client": {"Proxy": "http://127.0.0.1:2444/.well-known/telegramd/client"},
    }}},
    "TCP": {"443": {"HTTPS": True},
            "2443": {"TCPForward": "127.0.0.1:2443"},
            "2447": {"TCPForward": "127.0.0.1:2447", "ProxyProtocol": 1}},
}))
PY

real_kubectl=$(command -v kubectl)
test_migration_version=$(kubectl get configmap required-migration-version --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.data.migration-version}')
test_media_key_current=$(kubectl get configmap teagramd-runtime --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.data.TG_MEDIA_KEY_CURRENT}')
test_migrations_job=$(kubectl get jobs --namespace "$SOURCE_NAMESPACE" \
  --selector=app.kubernetes.io/name=db-migrations --output json |
  jq -er 'if (.items | length) == 1 then .items[0].metadata.name else error("expected one source migration Job") end')
incus_test_env=(PATH="$tmp_dir/bin:$PATH" INCUS_STATE_DIR="$tmp_dir/incus-state" \
  INCUS_TEST_SOURCE_CONTAINER="$source_container" \
  REAL_KUBECTL="$real_kubectl" CUTOVER_FORWARD_DELAY_SECONDS=2 \
  CUTOVER_TEST_PINNED_REVISION="$K8S_IMAGE_REVISION" \
  CUTOVER_TEST_MIGRATION_VERSION="$test_migration_version" \
  CUTOVER_TEST_MIGRATIONS_JOB_NAME="$test_migrations_job" \
  CUTOVER_TEST_TEAGRAMD_IMAGE=ghcr.io/teagramhq/teagramd:ci \
  CUTOVER_TEST_MIGRATIONS_IMAGE=ghcr.io/teagramhq/teagram-migrations:ci \
  CUTOVER_TEST_MC_IMAGE=ghcr.io/teagramhq/teagramd-mc-init:ci \
  CUTOVER_TEST_TARGET_S3_ENDPOINT=http://rustfs.teagram-cutover.svc.cluster.local:9000)
cat > "$tmp_dir/bin/kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ -n ${CUTOVER_FAIL_PREFLIGHT_RESOURCE:-} &&
  " $* " == *" get $CUTOVER_FAIL_PREFLIGHT_RESOURCE "* && " $* " == *' --ignore-not-found '* ]]; then
  printf 'injected preflight API failure\n' >&2
  exit 42
fi
if [[ ${CUTOVER_FAIL_RESOURCE_LOOKUP:-} == cloudflared &&
  ( ${CUTOVER_TEST_FAIL_AFTER_SOURCE_FENCE:-} != true || -f $RUNNER_TEMP/teagram-cutover-runner/phase ) &&
  " $* " == *' get deployment/cloudflared '* && " $* " == *' --ignore-not-found '* ]]; then
  printf 'injected disposable-cluster Cloudflare lookup failure\n' >&2
  exit 42
fi
if [[ ${CUTOVER_FAIL_RESOURCE_LOOKUP:-} == teagramd &&
  " $* " == *' get statefulset/teagramd '* && " $* " == *' --ignore-not-found '* ]]; then
  : > "$INCUS_STATE_DIR/teagramd-lookup-failure-injected"
  printf 'injected disposable-cluster teagramd lookup failure\n' >&2
  exit 42
fi
if [[ ${CUTOVER_FAIL_IMPORTER_DELETE:-} == true && " $* " == *' delete pod/cutover-volume-importer '* ]]; then
  printf 'injected importer deletion failure\n' >&2
  exit 42
fi
exec "$REAL_KUBECTL" "$@"
SH
chmod 0700 "$tmp_dir/bin/kubectl"
chmod 0600 "$KUBECONFIG"
[[ $(stat -c '%a' "$KUBECONFIG") == 600 ]] || {
  printf 'disposable test kubeconfig must be private before runner fixtures execute\n' >&2
  exit 1
}

kubectl apply --filename deploy/k8s/deployer-access.yaml >/dev/null
kubectl wait --for=create secret/teagram-deployer-token \
  --namespace "$SOURCE_NAMESPACE" --timeout=60s >/dev/null
kubectl wait --for='jsonpath={.data.token}' secret/teagram-deployer-token \
  --namespace "$SOURCE_NAMESPACE" --timeout=120s >/dev/null
kubectl wait --for='jsonpath={.data.ca\.crt}' secret/teagram-deployer-token \
  --namespace "$SOURCE_NAMESPACE" --timeout=120s >/dev/null
kubectl config view --raw --minify --output=json > "$tmp_dir/admin-kubeconfig.json"
kubectl get secret teagram-deployer-token --namespace "$SOURCE_NAMESPACE" --output=json \
  > "$tmp_dir/deployer-token.json"
chmod 0600 "$tmp_dir/admin-kubeconfig.json" "$tmp_dir/deployer-token.json"
python3 - "$tmp_dir/admin-kubeconfig.json" "$tmp_dir/deployer-token.json" \
  "$tmp_dir/deployer.kubeconfig" <<'PY'
import base64
import json
import os
import pathlib
import sys

admin_path, token_path, output_path = map(pathlib.Path, sys.argv[1:])
admin = json.loads(admin_path.read_text())
secret = json.loads(token_path.read_text())
context_name = admin.get("current-context")
contexts = [entry["context"] for entry in admin.get("contexts", [])
            if entry.get("name") == context_name]
if len(contexts) != 1:
    raise SystemExit("CI admin kubeconfig has no unique current context")
cluster_name = contexts[0].get("cluster")
clusters = [entry["cluster"] for entry in admin.get("clusters", [])
            if entry.get("name") == cluster_name]
if len(clusters) != 1:
    raise SystemExit("CI admin kubeconfig has no unique current cluster")
server = clusters[0].get("server")
ca = clusters[0].get("certificate-authority-data")
encoded_token = secret.get("data", {}).get("token")
if not server or not ca or not encoded_token:
    raise SystemExit("CI deployer kubeconfig inputs are incomplete")
token = base64.b64decode(encoded_token, validate=True).decode("ascii")
config = {
    "apiVersion": "v1",
    "kind": "Config",
    "clusters": [{"name": cluster_name, "cluster": {
        "server": server,
        "certificate-authority-data": ca,
    }}],
    "contexts": [{"name": "teagram-deployer", "context": {
        "cluster": cluster_name,
        "namespace": "teagram",
        "user": "teagram-deployer",
    }}],
    "current-context": "teagram-deployer",
    "users": [{"name": "teagram-deployer", "user": {"token": token}}],
}
fd = os.open(output_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "w", encoding="utf-8") as output:
    json.dump(config, output, separators=(",", ":"))
    output.write("\n")
PY
[[ $(stat -c '%a' "$tmp_dir/deployer.kubeconfig") == 600 ]]
if env "${incus_test_env[@]}" KUBECONFIG="$tmp_dir/deployer.kubeconfig" \
  bash deploy/k8s/cutover-alpha.sh --apply --test-mode \
  > "$tmp_dir/deployer-boundary.log" 2>&1; then
  printf 'cutover runner passed preflight with the restricted deployer kubeconfig\n' >&2
  exit 1
fi
expect_log 'Forbidden' "$tmp_dir/deployer-boundary.log"
[[ $(kubectl get statefulset teagramd --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 1 ]]
[[ ! -e $RUNNER_TEMP/teagram-cutover-runner/phase ]]
printf 'bootstrap deployer kubeconfig was denied before cutover mutation\n'

if env "${incus_test_env[@]}" CUTOVER_TEST_PINNED_REVISION=0000000000000000000000000000000000000000 \
  bash deploy/k8s/cutover-alpha.sh --apply --test-mode \
  > "$tmp_dir/revision-mismatch.log" 2>&1; then
  printf 'cutover runner accepted a source/pin revision mismatch\n' >&2
  exit 1
fi
expect_log "live teagramd revision $K8S_IMAGE_REVISION does not match pinned production revision 0000000000000000000000000000000000000000" \
  "$tmp_dir/revision-mismatch.log"
[[ $(kubectl get statefulset teagramd --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 1 ]]
[[ $(kubectl get statefulset postgres --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 1 ]]
[[ -z $(kubectl get statefulset postgres --namespace "$TARGET_NAMESPACE" \
  --ignore-not-found --output name) ]]
[[ -z $(kubectl get secret teagramd-admin --namespace "$TARGET_NAMESPACE" \
  --ignore-not-found --output name) ]]
[[ ! -e $RUNNER_TEMP/teagram-cutover-runner/phase ]]
printf 'revision mismatch named both SHAs and failed before fencing or applying target resources\n'

for resource in statefulset/postgres pvc/key-data-teagramd-0 secret/teagramd-admin; do
  if env "${incus_test_env[@]}" CUTOVER_FAIL_PREFLIGHT_RESOURCE="$resource" \
    bash deploy/k8s/cutover-alpha.sh --apply --test-mode > "$tmp_dir/preflight-failure.log" 2>&1; then
    printf 'cutover accepted a failed preflight lookup for %s\n' "$resource" >&2
    exit 1
  fi
  expect_log 'injected preflight API failure' "$tmp_dir/preflight-failure.log"
  expect_log 'could not determine whether' "$tmp_dir/preflight-failure.log"
  [[ -z $(kubectl get pod/cutover-volume-importer --namespace "$TARGET_NAMESPACE" --ignore-not-found --output name) ]]
  rm -rf -- "$RUNNER_TEMP/teagram-cutover-runner"
done
printf '[Service]\n' > "$tmp_dir/incus-state/units/teagram-linkselector-port-forward.service"
printf '[Service]\n' > "$tmp_dir/incus-state/units/teagramd-port-forward.service"
if printf 'ROUTE-SWITCHED\n' | env "${incus_test_env[@]}" \
  CUTOVER_TEST_FAIL_AFTER_SOURCE_FENCE=true CUTOVER_FAIL_STOP_PORT_FORWARD=true \
  bash deploy/k8s/cutover-alpha.sh --apply --test-mode \
  > "$tmp_dir/pre-switch-failure.log" 2>&1; then
  printf 'cutover runner unexpectedly passed the injected post-fence failure\n' >&2
  exit 1
fi
if ! grep --fixed-strings --quiet 'injected disposable-cluster failure after the source write fence' \
  "$tmp_dir/pre-switch-failure.log"; then
  cat "$tmp_dir/pre-switch-failure.log" >&2
  exit 1
fi
expect_log 'injected port-forward stop failure' "$tmp_dir/pre-switch-failure.log"
expect_log 'failed to stop target port-forward units during recovery' "$tmp_dir/pre-switch-failure.log"
expect_log 'recovery was incomplete' "$tmp_dir/pre-switch-failure.log"
[[ $(kubectl get statefulset teagramd --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 1 ]]
[[ $(kubectl get deployment cloudflared --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 1 ]]
[[ $(kubectl get statefulset teagramd --namespace "$TARGET_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
[[ $(kubectl get deployment cloudflared --namespace "$TARGET_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
[[ -z $(kubectl get pods --namespace "$TARGET_NAMESPACE" \
  --selector=app.kubernetes.io/name=teagramd --output name) ]]
[[ -z $(kubectl get pods --namespace "$TARGET_NAMESPACE" \
  --selector=app.kubernetes.io/name=cloudflared --output name) ]]
[[ -z $(kubectl get pod/cutover-volume-importer --namespace "$TARGET_NAMESPACE" --ignore-not-found --output name) ]]
rm -f -- "$tmp_dir/incus-state/units/teagram-linkselector-port-forward.service" \
  "$tmp_dir/incus-state/units/teagramd-port-forward.service" \
  "$tmp_dir/incus-state/forward-stop-failure-injected"
if grep --fixed-strings --quiet 'automatic recovery is disabled' "$tmp_dir/pre-switch-failure.log"; then
  printf 'the pre-switch failure incorrectly selected post-switch recovery\n' >&2
  exit 1
fi
printf 'pre-switch failure recovery restarted the fenced source services\n'
kubectl delete namespace "$TARGET_NAMESPACE" --wait=true --timeout=300s >/dev/null
rm -rf -- "$RUNNER_TEMP/teagram-cutover-runner"
create_target_namespace

if printf 'ROUTE-SWITCHED\n' | env "${incus_test_env[@]}" \
  CUTOVER_TEST_FAIL_AFTER_SOURCE_FENCE=true CUTOVER_FAIL_RESOURCE_LOOKUP=cloudflared \
  bash deploy/k8s/cutover-alpha.sh --apply --test-mode \
  > "$tmp_dir/recovery-lookup-failure.log" 2>&1; then
  printf 'cutover runner unexpectedly recovered after a Kubernetes resource lookup failure\n' >&2
  exit 1
fi
expect_log 'could not determine whether the k8s Cloudflare connector exists during recovery' "$tmp_dir/recovery-lookup-failure.log"
expect_log 'recovery was incomplete' "$tmp_dir/recovery-lookup-failure.log"
[[ $(kubectl get statefulset teagramd --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
[[ $(kubectl get deployment cloudflared --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
[[ $(kubectl get statefulset teagramd --namespace "$TARGET_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
[[ $(kubectl get deployment cloudflared --namespace "$TARGET_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
printf 'resource lookup failure during recovery kept Compose fenced\n'
printf 'ROUTE-RESTORED\n' | env "${incus_test_env[@]}" \
  bash deploy/k8s/cutover-alpha.sh --rollback --test-mode
[[ $(kubectl get statefulset teagramd --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 1 ]]
[[ $(kubectl get deployment cloudflared --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 1 ]]
kubectl delete namespace "$TARGET_NAMESPACE" --wait=true --timeout=300s >/dev/null
rm -rf -- "$RUNNER_TEMP/teagram-cutover-runner"
create_target_namespace

if printf 'ROUTE-SWITCHED\n' | env "${incus_test_env[@]}" CUTOVER_TEST_DROP_S3_OBJECT=true \
  bash deploy/k8s/cutover-alpha.sh --apply --test-mode > "$tmp_dir/dropped-s3-object.log" 2>&1; then
  printf 'cutover runner accepted a mismatched encrypted S3 object set\n' >&2
  exit 1
fi
expect_log 'source and target S3 object counts, byte totals, or per-object SHA-256 checksums do not match' \
  "$tmp_dir/dropped-s3-object.log"
[[ $(kubectl get statefulset teagramd --namespace "$SOURCE_NAMESPACE" --output 'jsonpath={.spec.replicas}') == 1 ]]
[[ -z $(kubectl get pod/cutover-volume-importer --namespace "$TARGET_NAMESPACE" --ignore-not-found --output name) ]]
kubectl delete namespace "$TARGET_NAMESPACE" --wait=true --timeout=300s >/dev/null
rm -rf -- "$RUNNER_TEMP/teagram-cutover-runner"
create_target_namespace

if printf 'ROUTE-SWITCHED\n' | env "${incus_test_env[@]}" CUTOVER_FAIL_FIRST_SERVE_ROUTE=true \
  bash deploy/k8s/cutover-alpha.sh --apply --test-mode > "$tmp_dir/first-route-failure.log" 2>&1; then
  printf 'cutover runner accepted a failed first Serve route\n' >&2
  exit 1
fi
expect_log 'injected disposable-cluster failure before the first Serve route changed' "$tmp_dir/first-route-failure.log"
[[ $(kubectl get statefulset teagramd --namespace "$SOURCE_NAMESPACE" --output 'jsonpath={.spec.replicas}') == 1 ]]
[[ $(kubectl get deployment cloudflared --namespace "$SOURCE_NAMESPACE" --output 'jsonpath={.spec.replicas}') == 1 ]]
if grep --fixed-strings --quiet 'automatic recovery is disabled' "$tmp_dir/first-route-failure.log"; then
  printf 'unchanged Serve routes incorrectly selected post-switch recovery\n' >&2
  exit 1
fi
rm -f "$tmp_dir/incus-state/serve-route-failure-injected"
kubectl delete namespace "$TARGET_NAMESPACE" --wait=true --timeout=300s >/dev/null
rm -rf -- "$RUNNER_TEMP/teagram-cutover-runner"
create_target_namespace

if printf 'ROUTE-SWITCHED\n' | env "${incus_test_env[@]}" CUTOVER_FAIL_OLD_CONNECTOR_CHECK=true \
  bash deploy/k8s/cutover-alpha.sh --apply --test-mode \
  > "$tmp_dir/post-switch-failure.log" 2>&1; then
  printf 'cutover runner unexpectedly passed the injected post-switch failure\n' >&2
  exit 1
fi
expect_log 'injected disposable-cluster failure after the Cloudflare connector started' "$tmp_dir/post-switch-failure.log"
expect_log 'automatic recovery is disabled' "$tmp_dir/post-switch-failure.log"
[[ $(kubectl exec --namespace "$SOURCE_NAMESPACE" pod/cutover-key-reader -c reader -- \
  stat -c '%a:%u' /var/lib/teagramd/server_key.pem) == 600:65532 ]] || {
  printf 'source RSA fixture does not exercise a private non-root key\n' >&2
  exit 1
}
sed 's/^  namespace: teagram$/  namespace: teagram-cutover/' deploy/k8s/test-cutover-key-reader.yaml |
  kubectl apply --filename - >/dev/null
kubectl wait --for=condition=Ready pod/cutover-key-reader \
  --namespace "$TARGET_NAMESPACE" --timeout=180s >/dev/null
for key_file in server_key.pem enc_key.hex; do
  source_hash=$(kubectl exec --namespace "$SOURCE_NAMESPACE" pod/cutover-key-reader -c reader -- \
    sha256sum "/var/lib/teagramd/$key_file" | awk '{print $1}')
  target_hash=$(kubectl exec --namespace "$TARGET_NAMESPACE" pod/cutover-key-reader -c reader -- \
    sha256sum "/var/lib/teagramd/$key_file" | awk '{print $1}')
  [[ $source_hash =~ ^[0-9a-f]{64}$ && $source_hash == "$target_hash" ]] || {
    printf 'cutover identity file does not match its source: %s\n' "$key_file" >&2
    exit 1
  }
  target_stat=$(kubectl exec --namespace "$TARGET_NAMESPACE" pod/cutover-key-reader -c reader -- \
    stat -c '%a:%u:%g' "/var/lib/teagramd/$key_file")
  # Once teagramd starts, kubelet applies the pod fsGroup, which adds group
  # access to the file (0660); the file must still be closed to everyone else.
  [[ $target_stat =~ ^[0-7][0-7]0:65532:65532$ ]] || {
    printf 'cutover identity file is not private and owned by teagramd: %s (mode:uid:gid is %s)\n' \
      "$key_file" "$target_stat" >&2
    exit 1
  }
done
kubectl delete pod cutover-key-reader --namespace "$TARGET_NAMESPACE" --wait=true --timeout=180s >/dev/null
[[ $(<"$RUNNER_TEMP/teagram-cutover-runner/phase") == route-switched ]]
[[ $(kubectl get statefulset teagramd --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
[[ $(kubectl get deployment cloudflared --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
[[ $(kubectl get deployment cloudflared --namespace "$TARGET_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 1 ]]
python3 - "$tmp_dir/incus-state/serve.json" <<'PY'
import json
import pathlib
import sys

serve = json.loads(pathlib.Path(sys.argv[1]).read_text())
handlers = next(iter(serve["Web"].values()))["Handlers"]
assert len(handlers) == 5
assert handlers["/"]["Proxy"] == "http://127.0.0.2:18081"
assert handlers["/apiws"]["Proxy"] == "http://127.0.0.2:18444/apiws"
assert handlers["/admin"]["Proxy"] == "http://127.0.0.2:18445/admin"
assert handlers["/.well-known/teagramd/client"]["Proxy"] == \
    "http://127.0.0.2:18444/.well-known/teagramd/client"
assert handlers["/.well-known/telegramd/client"]["Proxy"] == \
    "http://127.0.0.2:18444/.well-known/telegramd/client"
assert serve["TCP"]["2443"]["TCPForward"] == "127.0.0.2:2443"
assert serve["TCP"]["2447"] == {"TCPForward": "127.0.0.2:2447", "ProxyProtocol": 1}
assert not any(serve.get("AllowFunnel", {}).values())
assert serve["TCP"]["443"] == {"HTTPS": True}
PY
printf 'post-switch failure kept k3s live and Compose fenced\n'
cutover_job=$(kubectl get job "$test_migrations_job" --namespace "$TARGET_NAMESPACE" --output json)
cutover_job_image=$(jq -er '.spec.template.spec.containers[] | select(.name == "migrate") | .image' \
  <<< "$cutover_job")
[[ $cutover_job_image == ghcr.io/teagramhq/teagram-migrations:ci ]] || {
  printf 'cutover migration image is %s, want the verified live-revision CI image\n' \
    "$cutover_job_image" >&2
  exit 1
}
jq --exit-status '.status.conditions | any(.type == "Complete" and .status == "True")' \
  <<< "$cutover_job" >/dev/null
target_migration_version=$(kubectl exec --namespace "$TARGET_NAMESPACE" postgres-0 -c postgres -- \
  psql --username=postgres --dbname=teagram --tuples-only --no-align \
  --command="SELECT COALESCE(MAX(version)::text, '') FROM atlas_schema_revisions.atlas_schema_revisions")
[[ $target_migration_version == "$test_migration_version" ]]
target_media_key_current=$(kubectl get configmap teagramd-runtime --namespace "$TARGET_NAMESPACE" \
  --output 'jsonpath={.data.TG_MEDIA_KEY_CURRENT}')
[[ $target_media_key_current == "$test_media_key_current" ]]
printf 'actual runner completed the matching migration Job and preserved the live database version\n'

target_pod=$(kubectl get pods --namespace "$TARGET_NAMESPACE" \
  --selector=app.kubernetes.io/name=teagramd --output 'jsonpath={.items[0].metadata.name}')
[[ -n $target_pod ]]
kubectl exec --namespace "$TARGET_NAMESPACE" "$target_pod" -- \
  /usr/local/bin/teagramd client-config > "$tmp_dir/client.json"
chmod 0600 "$tmp_dir/client.json"

start_port_forward "$TARGET_NAMESPACE"
start_linkselector_forward "$TARGET_NAMESPACE"
curl --fail --silent --show-error --max-time 10 http://127.0.0.1:12881/+k3s-smoke |
  grep --fixed-strings 'Open this in Teagram' >/dev/null
curl --fail --silent --show-error --max-time 10 http://127.0.0.1:12881/ \
  --output "$tmp_dir/web-through-linkselector.html"
grep --fixed-strings --quiet 'id="page-chats"' "$tmp_dir/web-through-linkselector.html"
smoke_bot_api
printf 'restored-account sign-in on target\n'
smoke_restored_account
smoke_download_preserved_media
printf 'target listener, restored-account, and pre-cutover encrypted media download smokes passed\n'
stop_port_forward

# Apply the normal production resources after cutover. Keep the already-tested
# workload image and substitute the disposable Cloudflare connector fixture.
prod_digest=$(yq -r '.images[] | select(.name == "ghcr.io/teagramhq/teagramd") | .digest' \
  deploy/k8s/prod/kustomization.yaml)
[[ $prod_digest =~ ^sha256:[0-9a-f]{64}$ ]]
kubectl kustomize deploy/k8s/prod |
  sed -e "s/^\( *\)namespace: teagram$/\1namespace: $TARGET_NAMESPACE/" \
    -e "s|ghcr.io/teagramhq/teagramd@$prod_digest|$CUTOVER_IMAGE|g" |
  yq 'select(.kind != "Deployment" or .metadata.name != "cloudflared")' - > "$tmp_dir/normal-prod.yaml"
printf '\n---\n' >> "$tmp_dir/normal-prod.yaml"
kubectl kustomize deploy/k8s/ci |
  sed "s/^\( *\)namespace: teagram$/\1namespace: $TARGET_NAMESPACE/" |
  yq 'select(.kind == "Deployment" and .metadata.name == "cloudflared")' - >> "$tmp_dir/normal-prod.yaml"
kubectl apply --dry-run=server --filename "$tmp_dir/normal-prod.yaml" >/dev/null
kubectl apply --filename "$tmp_dir/normal-prod.yaml" >/dev/null
kubectl rollout status statefulset/teagramd --namespace "$TARGET_NAMESPACE" --timeout=600s >/dev/null
kubectl rollout restart statefulset/teagramd --namespace "$TARGET_NAMESPACE" >/dev/null
kubectl rollout status statefulset/teagramd --namespace "$TARGET_NAMESPACE" --timeout=600s >/dev/null
kubectl rollout status deployment/cloudflared --namespace "$TARGET_NAMESPACE" --timeout=300s >/dev/null
start_port_forward "$TARGET_NAMESPACE"
start_linkselector_forward "$TARGET_NAMESPACE"
curl --fail --silent --show-error --max-time 10 http://127.0.0.1:12881/+k3s-smoke |
  grep --fixed-strings 'Open this in Teagram' >/dev/null
discovery_key=$(curl --fail --silent --show-error --max-time 10 \
  http://127.0.0.1:12444/.well-known/teagramd/client | jq -er '.mtproto.rsa_spki | strings | select(length > 0)')
[[ $discovery_key == "$(jq -er '.mtproto.rsa_spki' "$tmp_dir/client.json")" ]]
python3 - <<'PY'
import base64
import socket

request = (
    b"GET /apiws HTTP/1.1\r\nHost: 127.0.0.1:12444\r\n"
    b"Connection: Upgrade\r\nUpgrade: websocket\r\n"
    b"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: "
    + base64.b64encode(bytes(16)) + b"\r\n"
    b"Sec-WebSocket-Protocol: binary\r\n\r\n"
)
with socket.create_connection(("127.0.0.1", 12444), timeout=10) as connection:
    connection.sendall(request)
    response = b""
    while b"\r\n\r\n" not in response and len(response) < 16384:
        chunk = connection.recv(4096)
        if not chunk:
            break
        response += chunk
    if not response.startswith(b"HTTP/1.1 101 "):
        raise SystemExit("WebSocket upgrade failed after the normal production apply")
PY
admin_status=$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --max-time 10 http://127.0.0.1:12445/admin/metrics)
[[ $admin_status == 401 ]] || { printf 'admin auth returned HTTP %s after the normal production apply\n' "$admin_status" >&2; exit 1; }
smoke_bot_api
smoke_restored_account
printf 'normal production apply preserved discovery, WebSocket, admin authentication, Bot API, and restored-account sign-in\n'
stop_port_forward
kubectl delete pod cutover-key-reader --namespace "$SOURCE_NAMESPACE" --wait=true --timeout=180s >/dev/null

if printf 'K3S-WRITES-LOST\nROUTE-RESTORED\n' | env "${incus_test_env[@]}" \
  CUTOVER_FAIL_RESOURCE_LOOKUP=cloudflared \
  bash deploy/k8s/cutover-alpha.sh --rollback --test-mode \
  > "$tmp_dir/connector-lookup-failure.log" 2>&1; then
  printf 'rollback runner ignored a Kubernetes API lookup failure\n' >&2
  exit 1
fi
expect_log 'could not determine whether deployment/cloudflared exists' "$tmp_dir/connector-lookup-failure.log"
[[ $(kubectl get statefulset teagramd --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
[[ $(kubectl get deployment cloudflared --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
[[ $(kubectl get deployment cloudflared --namespace "$TARGET_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 1 ]]
[[ $(kubectl get statefulset teagramd --namespace "$TARGET_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 1 ]]

if printf 'K3S-WRITES-LOST\nROUTE-RESTORED\n' | env "${incus_test_env[@]}" \
  CUTOVER_FAIL_RESOURCE_LOOKUP=teagramd \
  bash deploy/k8s/cutover-alpha.sh --rollback --test-mode \
  > "$tmp_dir/teagramd-lookup-failure.log" 2>&1; then
  printf 'rollback runner ignored a Kubernetes teagramd lookup failure\n' >&2
  exit 1
fi
expect_log 'could not determine whether statefulset/teagramd exists' "$tmp_dir/teagramd-lookup-failure.log"
[[ $(kubectl get deployment cloudflared --namespace "$TARGET_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
[[ -z $(kubectl get pods --namespace "$TARGET_NAMESPACE" \
  --selector=app.kubernetes.io/name=cloudflared --output name) ]]
[[ $(kubectl get statefulset teagramd --namespace "$TARGET_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 1 ]]
[[ $(kubectl get statefulset teagramd --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
[[ $(kubectl get deployment cloudflared --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]

if printf 'K3S-WRITES-LOST\nROUTE-RESTORED\n' | env "${incus_test_env[@]}" \
  CUTOVER_FAIL_AFTER_FIRST_SERVE_ROUTE=true \
  bash deploy/k8s/cutover-alpha.sh --rollback --test-mode \
  > "$tmp_dir/partial-route-rollback.log" 2>&1; then
  printf 'rollback runner unexpectedly passed the injected partial Serve restore failure\n' >&2
  exit 1
fi
expect_log 'injected disposable-cluster failure after the first Serve route changed' "$tmp_dir/partial-route-rollback.log"
[[ $(kubectl get statefulset teagramd --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
[[ $(kubectl get deployment cloudflared --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
[[ $(kubectl get statefulset teagramd --namespace "$TARGET_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
[[ $(kubectl get deployment cloudflared --namespace "$TARGET_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
[[ -z $(kubectl get pods --namespace "$TARGET_NAMESPACE" \
  --selector=app.kubernetes.io/name=cloudflared --output name) ]]
[[ -z $(kubectl get pods --namespace "$TARGET_NAMESPACE" \
  --selector=app.kubernetes.io/name=teagramd --output name) ]]
python3 - "$tmp_dir/incus-state/serve.json" <<'PY'
import json
import pathlib
import sys

serve = json.loads(pathlib.Path(sys.argv[1]).read_text())
handlers = next(iter(serve["Web"].values()))["Handlers"]
assert handlers["/"]["Proxy"] == "http://127.0.0.1:8081"
assert handlers["/apiws"]["Proxy"] == "http://127.0.0.2:18444/apiws"
PY
printf 'interrupted partial Serve rollback left both connectors stopped and Compose fenced\n'

if printf 'K3S-WRITES-LOST\nROUTE-RESTORED\n' | env "${incus_test_env[@]}" \
  CUTOVER_FAIL_IMPORTER_DELETE=true CUTOVER_FAIL_STOP_PORT_FORWARD=true \
  bash deploy/k8s/cutover-alpha.sh --rollback --test-mode \
  > "$tmp_dir/importer-delete-failure.log" 2>&1; then
  printf 'rollback reported success after importer deletion failed\n' >&2
  exit 1
fi
expect_log 'injected importer deletion failure' "$tmp_dir/importer-delete-failure.log"
expect_log 'injected port-forward stop failure' "$tmp_dir/importer-delete-failure.log"
expect_log 'could not stop all target port-forwards' "$tmp_dir/importer-delete-failure.log"
expect_log 'manual cleanup is required' "$tmp_dir/importer-delete-failure.log"
expect_log 'Serve routes and Compose services were restored' "$tmp_dir/importer-delete-failure.log"
if grep --fixed-strings --quiet 'Rollback complete' "$tmp_dir/importer-delete-failure.log"; then
  printf 'rollback claimed completion despite importer cleanup failure\n' >&2
  exit 1
fi
[[ $(<"$RUNNER_TEMP/teagram-cutover-runner/phase") == rolled-back ]]
python3 - "$tmp_dir/incus-state/serve.json" <<'PY'
import json
import pathlib
import sys

serve = json.loads(pathlib.Path(sys.argv[1]).read_text())
handlers = next(iter(serve["Web"].values()))["Handlers"]
assert handlers == {
    "/": {"Proxy": "http://127.0.0.1:8081"},
    "/apiws": {"Proxy": "http://127.0.0.1:2444/apiws"},
    "/admin": {"Proxy": "http://127.0.0.1:2445/admin"},
    "/.well-known/teagramd/client": {
        "Proxy": "http://127.0.0.1:2444/.well-known/teagramd/client"},
    "/.well-known/telegramd/client": {
        "Proxy": "http://127.0.0.1:2444/.well-known/telegramd/client"},
}
assert serve["TCP"]["2443"]["TCPForward"] == "127.0.0.1:2443"
assert serve["TCP"]["2447"] == {"TCPForward": "127.0.0.1:2447", "ProxyProtocol": 1}
PY
kubectl rollout status statefulset/teagramd --namespace "$SOURCE_NAMESPACE" --timeout=300s >/dev/null
kubectl rollout status deployment/cloudflared --namespace "$SOURCE_NAMESPACE" --timeout=300s >/dev/null
start_port_forward "$SOURCE_NAMESPACE"
for attempt in 1 2 3 4 5 6 7 8 9 10; do
  smoke_restored_account && break
  [[ $attempt -lt 10 ]]
  sleep 3
done
printf 'restored-account sign-in passed after the actual cutover runner rollback\n'
stop_port_forward

printf 'actual cutover runner migration, target sign-in, post-switch failure safety, explicit rollback, and source sign-in passed\n'
