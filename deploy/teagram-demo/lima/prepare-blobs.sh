#!/usr/bin/env bash
# Prepares lima's blob store as a 4 GiB ext4 filesystem, loop-mounted at
# /srv/teagram-demo-blobs. A write past the cap fails with ENOSPC inside the
# demo and cannot consume the rest of lima's root disk.
#
# Run once as root at cutover. It needs mkfs.ext4 (e2fsprogs) and blkid on the
# host; the script stops with a clear message when one is missing.
# Re-running leaves an existing ext4 image intact. It formats only an image
# created in this invocation and refuses unknown images and non-empty mounts.
set -Eeuo pipefail

BACKING_IMAGE=/var/lib/teagram-demo/blobs.img
BLOB_MOUNT=/srv/teagram-demo-blobs
BLOB_DATA=$BLOB_MOUNT/data
IMAGE_BYTES=$((4 * 1024 * 1024 * 1024))
FSTAB=/etc/fstab
FSTAB_LINE="$BACKING_IMAGE $BLOB_MOUNT ext4 loop,nodev,nosuid,noexec 0 0"

die() { printf 'prepare-blobs: %s\n' "$1" >&2; exit 1; }
log() { printf 'prepare-blobs: %s\n' "$1"; }

prepare_blob_filesystem() {
  local image=$1 created_here=$2 fs_type
  if [[ $created_here == 1 ]]; then
    if ! mkfs.ext4 -q -F -L teagram-blobs -m 0 "$image"; then
      printf 'prepare-blobs: could not format newly created %s\n' "$image" >&2
      return 1
    fi
    log 'formatted ext4'
    return 0
  fi

  if ! fs_type=$(blkid -o value -s TYPE "$image" 2>/dev/null); then
    printf 'prepare-blobs: could not identify existing %s; refusing to format it\n' "$image" >&2
    return 1
  fi
  if [[ $fs_type != ext4 ]]; then
    printf 'prepare-blobs: %s holds %s, not ext4; refusing to format it\n' \
      "$image" "${fs_type:-an unrecognized filesystem}" >&2
    return 1
  fi
  return 0
}

# Sourcing exposes the image decision for the regression test without running
# host preparation or changing files.
if [[ ${BASH_SOURCE[0]} != "$0" ]]; then
  return 0
fi

umask 077
[[ $EUID -eq 0 ]] || die 'run as root'
for tool in truncate mkfs.ext4 blkid mountpoint install stat; do
  command -v "$tool" >/dev/null 2>&1 || die "missing $tool"
done

install -d -m 0700 "$(dirname "$BACKING_IMAGE")"
created_image=0
[[ ! -L $BACKING_IMAGE ]] || die "$BACKING_IMAGE must not be a symlink"
if [[ ! -e $BACKING_IMAGE ]]; then
  (set -o noclobber; : > "$BACKING_IMAGE") 2>/dev/null || die "$BACKING_IMAGE appeared or could not be created"
  truncate -s "$IMAGE_BYTES" "$BACKING_IMAGE"
  created_image=1
  log "created sparse $BACKING_IMAGE of $IMAGE_BYTES bytes"
fi
size=$(stat -c '%s' -- "$BACKING_IMAGE")
[[ $size == "$IMAGE_BYTES" ]] || die "$BACKING_IMAGE is $size bytes, expected $IMAGE_BYTES; refusing to resize"

# Reserved blocks are zero, so the whole 4 GiB is usable and the cap is the
# filesystem size itself. Existing images must be positively identified as
# ext4; blkid errors and unknown signatures never authorize formatting.
prepare_blob_filesystem "$BACKING_IMAGE" "$created_image" || exit 1

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
