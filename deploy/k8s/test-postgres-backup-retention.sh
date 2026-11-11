#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
backup_script="$repo_root/deploy/k8s/base/postgres-backup/backup.sh"
tmp_dir="$(mktemp -d)"
trap 'rm -rf -- "$tmp_dir"' EXIT

# Seed newest and oldest timestamps in deliberately scrambled creation order.
for timestamp in 20261109000009 20261109000002 20261109000007 \
  20261109000001 20261109000005 20261109000003 20261109000008 \
  20261109000004 20261109000006; do
  : > "$tmp_dir/teagram-${timestamp}-fixture.dump"
done

touch -d '2 hours ago' "$tmp_dir/.teagram-stale.dump.tmp"
: > "$tmp_dir/.teagram-recent.dump.tmp"

sh "$backup_script" "$tmp_dir" --prune-only

for timestamp in 20261109000003 20261109000004 20261109000005 \
  20261109000006 20261109000007 20261109000008 20261109000009; do
  [[ -f "$tmp_dir/teagram-${timestamp}-fixture.dump" ]] || {
    printf 'newest backup was pruned: %s\n' "$timestamp" >&2
    exit 1
  }
done
for timestamp in 20261109000001 20261109000002; do
  [[ ! -e "$tmp_dir/teagram-${timestamp}-fixture.dump" ]] || {
    printf 'old backup was retained: %s\n' "$timestamp" >&2
    exit 1
  }
done
[[ ! -e "$tmp_dir/.teagram-stale.dump.tmp" ]] || {
  printf 'stale temporary dump was not removed\n' >&2
  exit 1
}
[[ -e "$tmp_dir/.teagram-recent.dump.tmp" ]] || {
  printf 'recent temporary dump was removed\n' >&2
  exit 1
}

printf 'PostgreSQL backup retention test passed\n'
