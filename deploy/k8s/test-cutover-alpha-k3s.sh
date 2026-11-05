#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

: "${KUBECONFIG:?KUBECONFIG must point to the disposable CI cluster}"
: "${RUNNER_TEMP:?RUNNER_TEMP must point to the disposable runner directory}"

readonly SOURCE_NAMESPACE=teagram
readonly TARGET_NAMESPACE=teagram-cutover
readonly CUTOVER_IMAGE=ghcr.io/teagramhq/teagramd@sha256:e307d5d93ab000d61dc77251a599b00e031e6dac7cdf026ef8d2d621734e7c3f
tmp_dir=$(mktemp -d "$RUNNER_TEMP/teagram-cutover.XXXXXX")
port_forward_pid=''
linkselector_forward_pid=''

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
  local status=$?
  trap - EXIT
  stop_port_forward
  if (( status == 0 )); then
    kubectl delete namespace "$TARGET_NAMESPACE" --wait=false >/dev/null 2>&1 || true
  fi
  kubectl delete job cutover-account-seed --namespace "$SOURCE_NAMESPACE" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  rm -rf -- "$tmp_dir"
  exit "$status"
}
trap cleanup EXIT
trap 'printf "cutover test failed at line %s: %s\n" "$LINENO" "$BASH_COMMAND" >&2' ERR

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

wait_for_no_pods() {
  local namespace=$1 selector=$2 elapsed=0 pods
  while (( elapsed < 180 )); do
    pods=$(kubectl get pods --namespace "$namespace" --selector="$selector" \
      --output 'jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}')
    [[ -z $pods ]] && return 0
    sleep 2
    elapsed=$((elapsed + 2))
  done
  printf 'pods remained for %s in namespace %s: %s\n' "$selector" "$namespace" "$pods" >&2
  return 1
}

start_port_forward() {
  local namespace=$1 attempt
  kubectl port-forward --address 127.0.0.1 --namespace "$namespace" \
    service/teagramd 12443:2443 12444:2444 12445:2445 > "$tmp_dir/port-forward-$namespace.log" 2>&1 &
  port_forward_pid=$!
  for ((attempt = 1; attempt <= 30; attempt++)); do
    if curl --silent --output /dev/null --max-time 2 http://127.0.0.1:12444/.well-known/teagramd/client; then
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
  docker run --rm --network host \
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

kubectl create namespace "$TARGET_NAMESPACE" >/dev/null
for secret in ghcr-pull postgres-credentials rustfs-credentials \
  teagramd-blob-credentials teagramd-media-keys; do
  copy_secret "$secret"
done
for configmap in teagramd-runtime link-edge-runtime; do
  copy_configmap "$configmap"
done

printf 'ci-placeholder-token' > "$tmp_dir/cloudflared-token"
printf '%064d' 0 > "$tmp_dir/admin-token-hash"
kubectl create secret generic cloudflared-token --namespace "$TARGET_NAMESPACE" \
  --from-file=token="$tmp_dir/cloudflared-token" >/dev/null
kubectl create secret generic teagramd-admin --namespace "$TARGET_NAMESPACE" \
  --from-file=TG_ADMIN_TOKEN_HASH="$tmp_dir/admin-token-hash" >/dev/null

kubectl kustomize deploy/k8s/prod-cutover > "$tmp_dir/cutover.yaml"
sed "s/namespace: teagram$/namespace: $TARGET_NAMESPACE/" "$tmp_dir/cutover.yaml" > "$tmp_dir/cutover-target.yaml"
kubectl apply --dry-run=server --filename "$tmp_dir/cutover-target.yaml" >/dev/null
kubectl apply --filename "$tmp_dir/cutover-target.yaml" >/dev/null
cutover_job_image=$(kubectl get job local-blob-migration --namespace "$TARGET_NAMESPACE" \
  --output 'jsonpath={.spec.template.spec.containers[?(@.name=="migrate")].image}')
[[ $cutover_job_image == "$CUTOVER_IMAGE" ]] || {
  printf 'cutover migration image is %s, want %s\n' "$cutover_job_image" "$CUTOVER_IMAGE" >&2
  exit 1
}

kubectl rollout status statefulset/postgres --namespace "$TARGET_NAMESPACE" --timeout=600s >/dev/null
kubectl rollout status statefulset/rustfs --namespace "$TARGET_NAMESPACE" --timeout=600s >/dev/null
kubectl rollout status deployment/linklanding --namespace "$TARGET_NAMESPACE" --timeout=300s >/dev/null
kubectl rollout status deployment/linkselector --namespace "$TARGET_NAMESPACE" --timeout=300s >/dev/null
kubectl wait --for=condition=complete jobs --selector=app.kubernetes.io/name=rustfs-init \
  --namespace "$TARGET_NAMESPACE" --timeout=600s >/dev/null
kubectl wait --for=condition=Ready pod/cutover-volume-importer \
  --namespace "$TARGET_NAMESPACE" --timeout=300s >/dev/null
[[ $(kubectl get deployment cloudflared --namespace "$TARGET_NAMESPACE" -o jsonpath='{.spec.replicas}') == 0 ]]

# Spool every transfer through a file instead of piping one kubectl exec into
# another: a receiver such as tar stops reading at the end-of-archive marker,
# and the sending kubectl then dies of SIGPIPE (exit 141) while still writing
# the archive padding.
kubectl exec --namespace "$SOURCE_NAMESPACE" postgres-0 -c postgres -- \
  pg_dump --format=custom --no-owner --no-acl --username=postgres --dbname=teagram > "$tmp_dir/source.dump"
kubectl exec --namespace "$TARGET_NAMESPACE" --stdin postgres-0 -c postgres -- \
  sh -ec 'umask 077; install -d -m 0700 /var/lib/postgresql/cutover-backups; cat > /var/lib/postgresql/cutover-backups/source.dump' \
  < "$tmp_dir/source.dump"

source_pod=$(kubectl get pods --namespace "$SOURCE_NAMESPACE" \
  --selector=app.kubernetes.io/name=teagramd --output 'jsonpath={.items[0].metadata.name}')
[[ -n $source_pod ]]
for file in server_key.pem enc_key.hex; do
  kubectl exec --namespace "$SOURCE_NAMESPACE" "$source_pod" -- \
    cat "/var/lib/teagramd/$file" > "$tmp_dir/$file"
  kubectl exec --namespace "$TARGET_NAMESPACE" --stdin pod/cutover-volume-importer -c importer -- \
    sh -ec "cat > /target/key-data/$file" < "$tmp_dir/$file"
done
kubectl exec --namespace "$SOURCE_NAMESPACE" "$source_pod" -- \
  tar -cf - -C /var/lib/teagramd-blobs . > "$tmp_dir/source-blobs.tar"
kubectl exec --namespace "$TARGET_NAMESPACE" --stdin pod/cutover-volume-importer -c importer -- \
  tar -xpf - -C /target/legacy-blobs < "$tmp_dir/source-blobs.tar"
mkdir -p "$tmp_dir/blob-fixture/parts/aa"
printf 'alpha cutover migration fixture\n' > "$tmp_dir/blob-fixture/parts/aa/0123456789abcdef0123456789abcd"
tar -cf "$tmp_dir/blob-fixture.tar" -C "$tmp_dir/blob-fixture" parts
kubectl exec --namespace "$TARGET_NAMESPACE" --stdin pod/cutover-volume-importer -c importer -- \
  tar -xpf - -C /target/legacy-blobs < "$tmp_dir/blob-fixture.tar"
kubectl exec --namespace "$TARGET_NAMESPACE" pod/cutover-volume-importer -c importer -- \
  sh -ec 'chown -R 65532:65532 /target/key-data /target/legacy-blobs /target/blob-mode; chmod 0700 /target/key-data /target/blob-mode; chmod 0600 /target/key-data/server_key.pem /target/key-data/enc_key.hex' >/dev/null
kubectl delete pod cutover-volume-importer --namespace "$TARGET_NAMESPACE" --wait=true --timeout=300s >/dev/null

# shellcheck disable=SC2016 # The inner shell expands POSTGRES_PASSWORD in the pod.
kubectl exec --namespace "$TARGET_NAMESPACE" postgres-0 -c postgres -- \
  sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" pg_restore --clean --if-exists --no-owner --no-acl --exit-on-error --username=postgres --dbname=teagram /var/lib/postgresql/cutover-backups/source.dump' >/dev/null
migration_job=$(kubectl get jobs --namespace "$TARGET_NAMESPACE" \
  --selector=app.kubernetes.io/name=db-migrations --output name)
[[ -n $migration_job && $(wc -l <<< "$migration_job") -eq 1 ]]
kubectl patch "$migration_job" --namespace "$TARGET_NAMESPACE" --type=merge \
  --patch '{"spec":{"suspend":false}}' >/dev/null
kubectl wait --for=condition=complete "$migration_job" --namespace "$TARGET_NAMESPACE" --timeout=900s >/dev/null
kubectl patch job/local-blob-migration --namespace "$TARGET_NAMESPACE" --type=merge \
  --patch '{"spec":{"suspend":false}}' >/dev/null
kubectl wait --for=condition=complete job/local-blob-migration \
  --namespace "$TARGET_NAMESPACE" --timeout=21600s >/dev/null
# Drain the log stream; grep -q can SIGPIPE kubectl under pipefail.
kubectl logs --namespace "$TARGET_NAMESPACE" job/local-blob-migration -c migrate |
  grep --fixed-strings 'blob_copy=pass mode=encrypted objects=1 bytes=32' >/dev/null

kubectl scale statefulset/teagramd --replicas=1 --namespace "$TARGET_NAMESPACE" >/dev/null
kubectl rollout status statefulset/teagramd --namespace "$TARGET_NAMESPACE" --timeout=900s >/dev/null
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
websocket_status=$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --max-time 10 http://127.0.0.1:12444/apiws)
[[ $websocket_status != 000 ]]
curl --fail --silent --show-error --max-time 10 http://127.0.0.1:12444/.well-known/teagramd/client |
  jq --exit-status '.mtproto.rsa_spki | type == "string" and length > 0' >/dev/null
curl --fail --silent --show-error --max-time 10 http://127.0.0.1:12444/.well-known/telegramd/client |
  jq --exit-status '.mtproto.rsa_spki | type == "string" and length > 0' >/dev/null
admin_status=$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --max-time 10 http://127.0.0.1:12445/admin)
[[ $admin_status =~ ^(200|302|401|403)$ ]]
smoke_restored_account
stop_port_forward

# Model the confirmed edge switch and rollback with disposable connector pods.
source_connector_pods=$(kubectl get pods --namespace "$SOURCE_NAMESPACE" \
  --selector=app.kubernetes.io/name=cloudflared --output 'jsonpath={.items[*].metadata.name}')
[[ -n $source_connector_pods ]]
[[ $(kubectl get deployment cloudflared --namespace "$TARGET_NAMESPACE" -o jsonpath='{.spec.replicas}') == 0 ]]
kubectl scale deployment/cloudflared --replicas=0 --namespace "$SOURCE_NAMESPACE" >/dev/null
wait_for_no_pods "$SOURCE_NAMESPACE" app.kubernetes.io/name=cloudflared
kubectl scale statefulset/teagramd --replicas=0 --namespace "$TARGET_NAMESPACE" >/dev/null
kubectl scale deployment/linkselector deployment/linklanding --replicas=0 --namespace "$TARGET_NAMESPACE" >/dev/null
wait_for_no_pods "$TARGET_NAMESPACE" app.kubernetes.io/name=teagramd

kubectl patch deployment cloudflared --namespace "$TARGET_NAMESPACE" \
  --type=strategic --patch-file deploy/k8s/test-cutover-cloudflared-patch.yaml >/dev/null
kubectl scale deployment/cloudflared --replicas=1 --namespace "$TARGET_NAMESPACE" >/dev/null
kubectl rollout status deployment/cloudflared --namespace "$TARGET_NAMESPACE" --timeout=180s >/dev/null
target_connector_pods=$(kubectl get pods --namespace "$TARGET_NAMESPACE" \
  --selector=app.kubernetes.io/name=cloudflared --output 'jsonpath={.items[*].metadata.name}')
[[ -n $target_connector_pods ]]

kubectl scale deployment/cloudflared --replicas=0 --namespace "$TARGET_NAMESPACE" >/dev/null
wait_for_no_pods "$TARGET_NAMESPACE" app.kubernetes.io/name=cloudflared
kubectl scale deployment/cloudflared --replicas=1 --namespace "$SOURCE_NAMESPACE" >/dev/null
kubectl rollout status deployment/cloudflared --namespace "$SOURCE_NAMESPACE" --timeout=180s >/dev/null
start_port_forward "$SOURCE_NAMESPACE"
smoke_restored_account
stop_port_forward

printf 'cutover migration, restored-password sign-in, connector handoff, and source rollback smoke passed\n'
