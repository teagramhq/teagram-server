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
  local status=$? mock_pid
  trap - EXIT
  stop_port_forward
  if [[ -d $tmp_dir/incus-state/units ]]; then
    for pid_file in "$tmp_dir"/incus-state/units/*.pid; do
      [[ -f $pid_file ]] || continue
      mock_pid=$(<"$pid_file")
      kill "$mock_pid" 2>/dev/null || true
    done
  fi
  if (( status == 0 )); then
    kubectl delete namespace "$TARGET_NAMESPACE" --wait=false >/dev/null 2>&1 || true
    rm -rf -- "$RUNNER_TEMP/teagram-cutover-runner"
  fi
  kubectl delete pod cutover-key-reader --namespace "$SOURCE_NAMESPACE" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete pod cutover-blob-seed --namespace "$SOURCE_NAMESPACE" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
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

create_target_namespace() {
  kubectl create namespace "$TARGET_NAMESPACE" >/dev/null
  kubectl label namespace "$TARGET_NAMESPACE" cutover-test=true >/dev/null
  for secret in ghcr-pull postgres-credentials rustfs-credentials \
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

kubectl label namespace "$SOURCE_NAMESPACE" cutover-test=true --overwrite >/dev/null
create_target_namespace

kubectl apply --filename deploy/k8s/test-cutover-blob-seed.yaml >/dev/null
kubectl wait --for=condition=Ready pod/cutover-blob-seed \
  --namespace "$SOURCE_NAMESPACE" --timeout=180s >/dev/null
kubectl delete pod cutover-blob-seed --namespace "$SOURCE_NAMESPACE" --wait=true --timeout=180s >/dev/null
kubectl apply --filename deploy/k8s/test-cutover-key-reader.yaml >/dev/null
kubectl wait --for=condition=Ready pod/cutover-key-reader \
  --namespace "$SOURCE_NAMESPACE" --timeout=180s >/dev/null

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
    "TCP": {"2443": {"TCPForward": "127.0.0.1:2443"}},
    "AllowFunnel": {"443": False, "2443": False},
}))
PY

real_kubectl=$(command -v kubectl)
incus_test_env=(PATH="$tmp_dir/bin:$PATH" INCUS_STATE_DIR="$tmp_dir/incus-state" \
  INCUS_TEST_TARGET_IP=127.0.0.2 REAL_KUBECTL="$real_kubectl")
cat > "$tmp_dir/bin/kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ ${CUTOVER_FAIL_PG_ISREADY:-} == true && " $* " == *' pg_isready '* ]]; then
  printf 'injected disposable-cluster failure after the source write fence\n' >&2
  exit 42
fi
if [[ ${CUTOVER_FAIL_RESOURCE_LOOKUP:-} == cloudflared &&
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
exec "$REAL_KUBECTL" "$@"
SH
chmod 0700 "$tmp_dir/bin/kubectl"
chmod 0600 "$KUBECONFIG"
if printf 'ROUTE-SWITCHED\n' | env "${incus_test_env[@]}" CUTOVER_FAIL_PG_ISREADY=true \
  bash deploy/k8s/cutover-alpha.sh --apply --test-mode \
  > "$tmp_dir/pre-switch-failure.log" 2>&1; then
  printf 'cutover runner unexpectedly passed the injected post-fence failure\n' >&2
  exit 1
fi
grep --fixed-strings --quiet 'injected disposable-cluster failure after the source write fence' \
  "$tmp_dir/pre-switch-failure.log"
[[ $(kubectl get statefulset teagramd --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 1 ]]
[[ $(kubectl get deployment cloudflared --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 1 ]]
! grep --fixed-strings --quiet 'automatic recovery is disabled' "$tmp_dir/pre-switch-failure.log"
printf 'pre-switch failure recovery restarted the fenced source services\n'
kubectl delete namespace "$TARGET_NAMESPACE" --wait=true --timeout=300s >/dev/null
rm -rf -- "$RUNNER_TEMP/teagram-cutover-runner"
create_target_namespace

if printf 'ROUTE-SWITCHED\n' | env "${incus_test_env[@]}" \
  CUTOVER_FAIL_PG_ISREADY=true CUTOVER_FAIL_RESOURCE_LOOKUP=cloudflared \
  bash deploy/k8s/cutover-alpha.sh --apply --test-mode \
  > "$tmp_dir/recovery-lookup-failure.log" 2>&1; then
  printf 'cutover runner unexpectedly recovered after a Kubernetes resource lookup failure\n' >&2
  exit 1
fi
grep --fixed-strings --quiet 'could not determine whether the k8s Cloudflare connector exists during recovery' \
  "$tmp_dir/recovery-lookup-failure.log"
grep --fixed-strings --quiet 'recovery was incomplete' "$tmp_dir/recovery-lookup-failure.log"
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

if printf 'ROUTE-SWITCHED\n' | env "${incus_test_env[@]}" CUTOVER_FAIL_OLD_CONNECTOR_CHECK=true \
  bash deploy/k8s/cutover-alpha.sh --apply --test-mode \
  > "$tmp_dir/post-switch-failure.log" 2>&1; then
  printf 'cutover runner unexpectedly passed the injected post-switch failure\n' >&2
  exit 1
fi
grep --fixed-strings --quiet 'injected disposable-cluster failure after the Cloudflare connector started' \
  "$tmp_dir/post-switch-failure.log"
grep --fixed-strings --quiet 'automatic recovery is disabled' "$tmp_dir/post-switch-failure.log"
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
assert all(value is False for value in serve["AllowFunnel"].values())
PY
printf 'post-switch failure kept k3s live and Compose fenced\n'
cutover_job_image=$(kubectl get job local-blob-migration --namespace "$TARGET_NAMESPACE" \
  --output 'jsonpath={.spec.template.spec.containers[?(@.name=="migrate")].image}')
[[ $cutover_job_image == "$CUTOVER_IMAGE" ]] || {
  printf 'cutover migration image is %s, want %s\n' "$cutover_job_image" "$CUTOVER_IMAGE" >&2
  exit 1
}
kubectl logs --namespace "$TARGET_NAMESPACE" job/local-blob-migration -c migrate |
  grep --fixed-strings 'blob_copy=pass mode=encrypted objects=1 bytes=32' >/dev/null
printf 'actual runner completed the snapshot and encrypted blob migration before the injected post-switch failure\n'

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
printf 'restored-account sign-in on target\n'
smoke_restored_account
printf 'target listener and restored-account smoke passed\n'
stop_port_forward
kubectl delete pod cutover-key-reader --namespace "$SOURCE_NAMESPACE" --wait=true --timeout=180s >/dev/null

if printf 'K3S-WRITES-LOST\nROUTE-RESTORED\n' | env "${incus_test_env[@]}" \
  CUTOVER_FAIL_RESOURCE_LOOKUP=cloudflared \
  bash deploy/k8s/cutover-alpha.sh --rollback --test-mode \
  > "$tmp_dir/connector-lookup-failure.log" 2>&1; then
  printf 'rollback runner ignored a Kubernetes API lookup failure\n' >&2
  exit 1
fi
grep --fixed-strings --quiet 'could not determine whether deployment/cloudflared exists' \
  "$tmp_dir/connector-lookup-failure.log"
[[ $(kubectl get statefulset teagramd --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
[[ $(kubectl get deployment cloudflared --namespace "$SOURCE_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
[[ $(kubectl get deployment cloudflared --namespace "$TARGET_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 1 ]]
[[ $(kubectl get statefulset teagramd --namespace "$TARGET_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 1 ]]
[[ -z $(kubectl get pods --namespace "$TARGET_NAMESPACE" \
  --selector=app.kubernetes.io/name=cloudflared --output name) ]]

if printf 'K3S-WRITES-LOST\nROUTE-RESTORED\n' | env "${incus_test_env[@]}" \
  CUTOVER_FAIL_RESOURCE_LOOKUP=teagramd \
  bash deploy/k8s/cutover-alpha.sh --rollback --test-mode \
  > "$tmp_dir/teagramd-lookup-failure.log" 2>&1; then
  printf 'rollback runner ignored a Kubernetes teagramd lookup failure\n' >&2
  exit 1
fi
grep --fixed-strings --quiet 'could not determine whether statefulset/teagramd exists' \
  "$tmp_dir/teagramd-lookup-failure.log"
[[ $(kubectl get deployment cloudflared --namespace "$TARGET_NAMESPACE" \
  --output 'jsonpath={.spec.replicas}') == 0 ]]
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
grep --fixed-strings --quiet 'injected disposable-cluster failure after the first Serve route changed' \
  "$tmp_dir/partial-route-rollback.log"
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

printf 'K3S-WRITES-LOST\nROUTE-RESTORED\n' | env "${incus_test_env[@]}" \
  bash deploy/k8s/cutover-alpha.sh --rollback --test-mode
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
PY
kubectl rollout status statefulset/teagramd --namespace "$SOURCE_NAMESPACE" --timeout=300s >/dev/null
kubectl rollout status deployment/cloudflared --namespace "$SOURCE_NAMESPACE" --timeout=300s >/dev/null
start_port_forward "$SOURCE_NAMESPACE"
smoke_restored_account
printf 'restored-account sign-in passed after the actual cutover runner rollback\n'
stop_port_forward

printf 'actual cutover runner migration, target sign-in, post-switch failure safety, explicit rollback, and source sign-in passed\n'
