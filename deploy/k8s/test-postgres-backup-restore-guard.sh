#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
restore_test="$repo_root/deploy/k8s/test-postgres-backup-restore.sh"
tmp_dir="$(mktemp -d)"
trap 'rm -rf -- "$tmp_dir"' EXIT
mkdir -p "$tmp_dir/bin" "$tmp_dir/runner"
cat > "$tmp_dir/bin/kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$KUBECTL_GUARD_TEST_LOG"
if [[ "$*" == 'config view --minify --output jsonpath={.clusters[0].cluster.server}' ]]; then
  printf '%s\n' "$KUBECTL_GUARD_TEST_SERVER"
  exit 0
fi
exit 91
SH
chmod 0755 "$tmp_dir/bin/kubectl"

export PATH="$tmp_dir/bin:$PATH"
export KUBECONFIG="$tmp_dir/fixture.kubeconfig"
export RUNNER_TEMP="$tmp_dir/runner"
export KUBECTL_GUARD_TEST_LOG="$tmp_dir/kubectl.log"
export KUBECTL_GUARD_TEST_SERVER=https://127.0.0.1:6443
: > "$KUBECTL_GUARD_TEST_LOG"

if GITHUB_ACTIONS=false bash "$restore_test" >"$tmp_dir/no-actions.out" 2>&1; then
  printf 'backup restore guard accepted a non-Actions invocation\n' >&2
  exit 1
fi
[[ ! -s "$KUBECTL_GUARD_TEST_LOG" ]] || {
  printf 'backup restore guard contacted Kubernetes outside Actions\n' >&2
  exit 1
}

if GITHUB_ACTIONS=true KUBECTL_GUARD_TEST_SERVER=https://prod.example:6443 \
  bash "$restore_test" >"$tmp_dir/wrong-cluster.out" 2>&1; then
  printf 'backup restore guard accepted a non-CI API server\n' >&2
  exit 1
fi
[[ "$(cat "$KUBECTL_GUARD_TEST_LOG")" == \
  'config view --minify --output jsonpath={.clusters[0].cluster.server}' ]] || {
  printf 'backup restore guard ran cluster commands after detecting a wrong API server\n' >&2
  exit 1
}

printf 'PostgreSQL backup restore safety guard test passed\n'
