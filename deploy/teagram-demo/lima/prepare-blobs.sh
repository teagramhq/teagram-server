#!/usr/bin/env bash
# Prepares lima's blob store as a 4 GiB ext4 filesystem, loop-mounted at
# /srv/teagram-demo-blobs. A write past the cap fails with ENOSPC inside the
# demo and cannot consume the rest of lima's root disk.
#
# Run once as root at cutover. It needs mkfs.ext4 (e2fsprogs) and blkid on the
# host; the script stops with a clear message when one is missing.
# Re-running changes nothing that is already in place. It refuses to format a
# filesystem that is not ext4 and refuses to mount over a non-empty directory.
set -Eeuo pipefail

BACKING_IMAGE=/var/lib/teagram-demo/blobs.img
BLOB_MOUNT=/srv/teagram-demo-blobs
BLOB_DATA=$BLOB_MOUNT/data
IMAGE_BYTES=$((4 * 1024 * 1024 * 1024))
FSTAB=/etc/fstab
FSTAB_LINE="$BACKING_IMAGE $BLOB_MOUNT ext4 loop,nodev,nosuid,noexec 0 0"

die() { printf 'prepare-blobs: %s\n' "$1" >&2; exit 1; }
log() { printf 'prepare-blobs: %s\n' "$1"; }

[[ $EUID -eq 0 ]] || die 'run as root'
for tool in truncate mkfs.ext4 blkid mountpoint install stat; do
  command -v "$tool" >/dev/null 2>&1 || die "missing $tool"
done

install -d -m 0700 "$(dirname "$BACKING_IMAGE")"
if [[ ! -e $BACKING_IMAGE ]]; then
  truncate -s "$IMAGE_BYTES" "$BACKING_IMAGE"
  log "created sparse $BACKING_IMAGE of $IMAGE_BYTES bytes"
fi
size=$(stat -c '%s' -- "$BACKING_IMAGE")
[[ $size == "$IMAGE_BYTES" ]] || die "$BACKING_IMAGE is $size bytes, expected $IMAGE_BYTES; refusing to resize"

fs_type=$(blkid -o value -s TYPE "$BACKING_IMAGE" 2>/dev/null || true)
if [[ -z $fs_type ]]; then
  # Reserved blocks are zero, so the whole 4 GiB is usable and the cap is the
  # filesystem size itself.
  mkfs.ext4 -q -F -L teagram-blobs -m 0 "$BACKING_IMAGE"
  log 'formatted ext4'
elif [[ $fs_type != ext4 ]]; then
  die "$BACKING_IMAGE holds $fs_type, not ext4; refusing to format it"
fi

if ! grep -qE "[[:space:]]${BLOB_MOUNT}[[:space:]]" "$FSTAB"; then
  printf '%s\n' "$FSTAB_LINE" >> "$FSTAB"
  log "added $BLOB_MOUNT to $FSTAB"
fi
if ! grep -qx loop /etc/modules 2>/dev/null; then
  printf 'loop\n' >> /etc/modules
  log 'added the loop module to /etc/modules'
fi

install -d -m 0755 "$BLOB_MOUNT"
if ! mountpoint -q "$BLOB_MOUNT"; then
  if [[ -n $(ls -A -- "$BLOB_MOUNT") ]]; then
    die "$BLOB_MOUNT is not empty; move its contents aside before the mount hides them"
  fi
  modprobe loop 2>/dev/null || true
  mount "$BLOB_MOUNT"
  log "mounted $BLOB_MOUNT"
fi

read -r blocks block_size < <(stat -f -c '%b %S' -- "$BLOB_MOUNT")
(( blocks * block_size <= IMAGE_BYTES )) || die "$BLOB_MOUNT is larger than the 4 GiB cap"

install -d -o 65532 -g 65532 -m 0700 "$BLOB_DATA"
log "ready: $BLOB_DATA owned by 65532:65532, mode 0700; run lima/check.sh to confirm"
