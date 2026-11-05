#!/usr/bin/env bash
set -euo pipefail

namespace=$1
pod=$2
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$repo_root"
tmp_dir=$(mktemp -d)
remote_path="/tmp/$(basename "$tmp_dir").dump"
cleanup() {
  kubectl exec --namespace "$namespace" "$pod" -c postgres -- rm -f -- "$remote_path" >/dev/null 2>&1 || true
  rm -rf -- "$tmp_dir"
}
trap cleanup EXIT

dd if=/dev/zero of="$tmp_dir/source.dump" bs=1048576 count=16 status=none
if bash deploy/k8s/cutover-dump-upload.sh "$namespace" "$pod" "$tmp_dir/source.dump" \
  "/tmp/$(basename "$tmp_dir")/missing.dump" > "$tmp_dir/error.log" 2>&1; then
  printf 'cutover dump upload accepted an unwritable destination\n' >&2
  exit 1
fi
bash deploy/k8s/cutover-dump-upload.sh "$namespace" "$pod" "$tmp_dir/source.dump" "$remote_path"
local_hash=$(sha256sum "$tmp_dir/source.dump" | awk '{print $1}')
remote_hash=$(kubectl exec --namespace "$namespace" "$pod" -c postgres -- sha256sum "$remote_path" | awk '{print $1}')
[[ $local_hash == "$remote_hash" ]] || {
  printf 'cutover dump upload did not preserve the complete file\n' >&2
  exit 1
}
printf 'cutover dump upload preserved 16 MiB\n'
