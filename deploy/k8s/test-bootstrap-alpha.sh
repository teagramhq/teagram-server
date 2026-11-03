#!/usr/bin/env bash
set -euo pipefail

# Runs bootstrap-alpha.sh --apply twice against fake incus and tailscale. The
# script needs root and writes /root, so run this as root or in a throwaway
# container. Only the host and LXC control commands are faked.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bootstrap="$repo_root/deploy/k8s/bootstrap-alpha.sh"
mock="$repo_root/deploy/k8s/test-bootstrap-alpha-mock.py"
kubeconfig_path=/root/teagram-k3s-deploy.kubeconfig

fail() {
  printf 'bootstrap-alpha test: %s\n' "$*" >&2
  exit 1
}

[[ $(id -u) -eq 0 ]] || fail 'run as root (the bootstrap writes under /root)'
[[ ! -e $kubeconfig_path && ! -L $kubeconfig_path ]] ||
  fail "$kubeconfig_path exists; move it aside before running this test"

work=$(mktemp -d)
cleanup() {
  rm -rf -- "$work" "$kubeconfig_path"
}
trap cleanup EXIT

# Fixture values are fake. Every one must stay out of the script's output.
pg_password='tgtest-postgres-pw-91c2'
tunnel_token='tgtest-cloudflared-token-55'
rustfs_access_key='1a2b3c4d5e6f70819a2b'
rustfs_secret_key="$(printf 'e1%.0s' {1..32})"
blob_access_key_id='2b3c4d5e6f708192a3b4'
blob_secret_key="$(printf 'c3%.0s' {1..32})"
media_key='tgtest-media-key-body-1'
ghcr_auth='dGd0ZXN0OmdoY3ItcmVhZC10b2tlbg=='
deployer_token='tgtest-deployer-token-7f3a9c'
dsn="postgres://postgres:${pg_password}@postgres:5432/teagram?sslmode=disable"

src="$work/src"
containers="$src/containers"
mkdir -p "$src/root/.ssh" "$src/root/.docker" "$containers"
printf '%s\n' \
  'command="/usr/local/sbin/teagram-ops",restrict ssh-ed25519 AAAATESTOPSKEY multica-daemon@delta' \
  'ssh-ed25519 AAAATESTOTHERKEY ops@other' > "$src/root/.ssh/authorized_keys"
printf '%s\n' "{\"auths\":{\"ghcr.io\":{\"auth\":\"$ghcr_auth\"}}}" > "$src/root/.docker/config.json"

for name in teagram-postgres-1 teagram-teagramd-1 teagram-rustfs-1 \
  teagram-link-edge-selector-1 teagram-web-cloudflared; do
  mkdir -p "$containers/$name"
done
printf '%s\n' "POSTGRES_PASSWORD=$pg_password" > "$containers/teagram-postgres-1/env"
printf '%s\n' \
  "TG_POSTGRES_DSN=$dsn" \
  'TG_PUBLIC_LINK_PREFIX=https://tgtest.example.invalid/l/' \
  'TG_ADVERTISE_ADDR=tgtest.example.invalid:443' \
  "TG_BLOB_S3_ACCESS_KEY_ID=$blob_access_key_id" > "$containers/teagram-teagramd-1/env"
printf '%s\n' \
  'TG_INVITE_WEB_SIGNUP_URL=https://web.tgtest.example.invalid/signup' \
  'TG_INVITE_DESKTOP_URL=https://desktop.tgtest.example.invalid/invite' \
  > "$containers/teagram-link-edge-selector-1/env"
printf '%s\n' "TUNNEL_TOKEN=$tunnel_token" > "$containers/teagram-web-cloudflared/env"
mkdir -p "$containers/teagram-rustfs-1/run/secrets" \
  "$containers/teagram-teagramd-1/run/secrets" \
  "$containers/teagram-teagramd-1/run/teagramd/media-keys"
printf '%s' "$rustfs_access_key" > "$containers/teagram-rustfs-1/run/secrets/rustfs-root-access-key"
printf '%s' "$rustfs_secret_key" > "$containers/teagram-rustfs-1/run/secrets/rustfs-root-secret-key"
printf '%s' "$blob_secret_key" > "$containers/teagram-teagramd-1/run/secrets/teagramd-blob-secret-key"
printf '%s' "$media_key" > "$containers/teagram-teagramd-1/run/teagramd/media-keys/1.key"
printf '%s' 'tgtest-media-key-body-2' > "$containers/teagram-teagramd-1/run/teagramd/media-keys/2.key"

bin="$work/bin"
mkdir -p "$bin" "$work/state"
cat > "$bin/incus" <<EOF
#!/usr/bin/env bash
exec python3 "$mock" incus "\$@"
EOF
cat > "$bin/tailscale" <<EOF
#!/usr/bin/env bash
exec python3 "$mock" tailscale "\$@"
EOF
cat > "$bin/hostname" <<'EOF'
#!/usr/bin/env bash
printf 'alpha\n'
EOF
chmod 0755 "$bin/incus" "$bin/tailscale" "$bin/hostname"

export MOCK_STATE="$work/state"
export MOCK_SRC="$src"
export MOCK_ACTIONS="$work/actions.log"

run_bootstrap() {
  local output=$1 actions=$2
  : > "$MOCK_ACTIONS"
  if ! PATH="$bin:$PATH" "$bootstrap" --apply > "$output" 2>&1; then
    cat "$output" >&2
    fail "bootstrap --apply exited non-zero ($output)"
  fi
  cp "$MOCK_ACTIONS" "$actions"
}

# First apply: builds the LXC and writes the deploy kubeconfig.
run_bootstrap "$work/run1.out" "$work/run1.actions"
grep -Fq 'k3s bootstrap complete' "$work/run1.out" || fail 'first run did not report completion'
[[ $(stat -c '%a:%u' "$kubeconfig_path") == 600:0 ]] || fail 'kubeconfig is not a root-only file'
grep -Fq "$deployer_token" "$kubeconfig_path" || fail 'kubeconfig does not contain the deploy token'
grep -Fxq 'k3s-install' "$work/run1.actions" || fail 'first run did not install k3s'

cp "$kubeconfig_path" "$work/kubeconfig.before"

# Second apply: must change nothing and still exit 0.
run_bootstrap "$work/run2.out" "$work/run2.actions"
grep -Fq 'k3s bootstrap complete' "$work/run2.out" || fail 'rerun did not report completion'
if grep -E '^(init|device-add|device-override|start|serve |k3s-install|sysctl|systemctl|file-write )' \
  "$work/run2.actions"; then
  fail 'rerun performed a mutating action'
fi
cmp -s "$work/kubeconfig.before" "$kubeconfig_path" || fail 'rerun changed the deploy kubeconfig'

# No fixture secret may appear in what the script prints, on either run.
for secret in "$pg_password" "$dsn" "$tunnel_token" "$rustfs_access_key" "$rustfs_secret_key" \
  "$blob_access_key_id" "$blob_secret_key" "$media_key" "$ghcr_auth" "$deployer_token"; do
  if grep -Fq -- "$secret" "$work/run1.out" "$work/run2.out"; then
    fail 'the script printed a secret value'
  fi
done

printf 'bootstrap-alpha apply test passed: first apply, no-op rerun, no secret output\n'
