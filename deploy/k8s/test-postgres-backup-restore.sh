#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"
: "${KUBECONFIG:?KUBECONFIG must point to the disposable CI cluster}"
: "${RUNNER_TEMP:?RUNNER_TEMP must point to the disposable runner directory}"
if [[ "${GITHUB_ACTIONS:-}" != true ]]; then
  printf 'PostgreSQL backup restore test is restricted to GitHub Actions k3s\n' >&2
  exit 2
fi
api_server="$(kubectl config view --minify \
  --output 'jsonpath={.clusters[0].cluster.server}')"
if [[ "$api_server" != https://127.0.0.1:6443 ]]; then
  printf 'PostgreSQL backup restore test refused unexpected API server: %s\n' \
    "${api_server:-<empty>}" >&2
  exit 2
fi

tmp_dir="$(mktemp -d "$RUNNER_TEMP/postgres-backup-restore.XXXXXX")"
port_forward_pid=''
backup_job="postgres-backup-test-${GITHUB_RUN_ID:-$$}"

# cleanup is called by the EXIT trap, which ShellCheck cannot follow.
# shellcheck disable=SC2329
cleanup() {
  local status=$?
  trap - EXIT
  if [[ -n "$port_forward_pid" ]]; then
    kill "$port_forward_pid" 2>/dev/null || true
    wait "$port_forward_pid" 2>/dev/null || true
  fi
  kubectl delete job postgres-backup-account-seed postgres-backup-restore-test \
    --namespace teagram --ignore-not-found --wait=true >/dev/null 2>&1 || true
  rm -rf -- "$tmp_dir"
  exit "$status"
}
trap cleanup EXIT
trap 'printf "PostgreSQL backup restore test failed at line %s: %s\\n" "$LINENO" "$BASH_COMMAND" >&2' ERR

printf 'backup_restore_smoke\n' > "$tmp_dir/username"
openssl rand -hex 24 > "$tmp_dir/password"
chmod 0600 "$tmp_dir/username" "$tmp_dir/password"

kubectl apply --filename deploy/k8s/test-postgres-backup-account-job.yaml >/dev/null
account_pod="$(kubectl get pods --namespace teagram \
  --selector=job-name=postgres-backup-account-seed \
  --output 'jsonpath={.items[0].metadata.name}')"
[[ -n "$account_pod" ]]
kubectl wait --for=jsonpath='{.status.phase}'=Running "pod/$account_pod" \
  --namespace teagram --timeout=180s >/dev/null
kubectl attach "$account_pod" --namespace teagram \
  --stdin --container create-account < "$tmp_dir/password" >/dev/null
kubectl wait --for=condition=complete job/postgres-backup-account-seed \
  --namespace teagram --timeout=180s >/dev/null

kubectl create job "$backup_job" --from=cronjob/postgres-backup \
  --namespace teagram >/dev/null
kubectl wait --for=jsonpath='{.status.phase}'=Bound pvc/postgres-backups \
  --namespace teagram --timeout=180s
if ! kubectl wait --for=condition=complete "job/$backup_job" \
  --namespace teagram --timeout=300s; then
  kubectl logs "job/$backup_job" --namespace teagram --all-containers=true || true
  exit 1
fi
kubectl logs "job/$backup_job" --namespace teagram --all-containers=true \
  | tee "$tmp_dir/backup.log"
grep --extended-regexp --quiet \
  '^backup=verified path=/backups/teagram-[A-Za-z0-9TZ._-]+\.dump bytes=[1-9][0-9]* sha256=[0-9a-f]{64}$' \
  "$tmp_dir/backup.log"
grep --fixed-strings --quiet 'backup=retention-complete retained=7' "$tmp_dir/backup.log"

kubectl scale statefulset/teagramd --replicas=0 --namespace teagram >/dev/null
kubectl wait --for=delete pod/teagramd-0 --namespace teagram --timeout=180s >/dev/null
kubectl exec pod/postgres-0 --namespace teagram -- \
  dropdb --username=postgres --force teagram
kubectl exec pod/postgres-0 --namespace teagram -- \
  createdb --username=postgres --owner=postgres teagram
kubectl apply --filename deploy/k8s/test-postgres-backup-restore-job.yaml >/dev/null
if ! kubectl wait --for=condition=complete job/postgres-backup-restore-test \
  --namespace teagram --timeout=900s; then
  kubectl logs job/postgres-backup-restore-test --namespace teagram \
    --all-containers=true || true
  exit 1
fi
kubectl logs job/postgres-backup-restore-test --namespace teagram \
  --all-containers=true | tee "$tmp_dir/restore.log"
grep --fixed-strings --quiet 'restore=verified path=/backups/teagram-' "$tmp_dir/restore.log"

kubectl scale statefulset/teagramd --replicas=1 --namespace teagram >/dev/null
kubectl rollout status statefulset/teagramd --namespace teagram --timeout=180s
app_pod="$(kubectl get pods --namespace teagram \
  --selector=app.kubernetes.io/name=teagramd \
  --output 'jsonpath={.items[0].metadata.name}')"
kubectl exec --namespace teagram "$app_pod" -- \
  /usr/local/bin/teagramd client-config > "$tmp_dir/client.json"
chmod 0644 "$tmp_dir/client.json"
kubectl port-forward --address 127.0.0.1 --namespace teagram \
  service/teagramd 12443:2443 > "$tmp_dir/port-forward.log" 2>&1 &
port_forward_pid=$!
for ((attempt = 0; attempt < 30; attempt++)); do
  if docker run --rm --network host \
    --user "$(id -u):$(id -g)" \
    --volume "$tmp_dir/client.json:/client.json:ro" \
    --volume "$tmp_dir/username:/username:ro" \
    --volume "$tmp_dir/password:/password:ro" \
    --entrypoint /usr/local/bin/mtproto-smoke \
    ghcr.io/teagramhq/teagramd:ci \
    --endpoint 127.0.0.1:12443 \
    --discovery-file /client.json \
    --username-file /username \
    --password-file /password; then
    printf 'backup=restore-sign-in-verified user=backup_restore_smoke\n'
    exit 0
  fi
  sleep 2
done
cat "$tmp_dir/port-forward.log" >&2
printf 'restored backup sign-in smoke did not complete\n' >&2
exit 1
