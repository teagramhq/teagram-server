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

die() { printf 'prepare-blobs: %s\n' "$1" >&2; exit 1; }
log() { printf 'prepare-blobs: %s\n' "$1"; }

create_blob_image() {
  local image=$1 bytes=$2
  if ! (set -o noclobber; : > "$image"); then
    printf 'prepare-blobs: could not create %s; check whether the path already exists\n' "$image" >&2
    return 1
  fi
  if ! truncate -s "$bytes" "$image"; then
    printf 'prepare-blobs: could not size newly created %s; verify it is partial and remove it before rerunning\n' \
      "$image" >&2
    return 1
  fi
  return 0
}

ensure_fstab_entry() {
  local fstab=$1 image=$2 mount_dir=$3 line old count current temp
  line="$image $mount_dir ext4 loop,nodev,nosuid,noexec,nofail 0 0"
  old="$image $mount_dir ext4 loop,nodev,nosuid,noexec 0 0"
  if ! count=$(awk -v target="$mount_dir" '$2 == target && $1 !~ /^#/ { n++ } END { print n + 0 }' "$fstab"); then
    printf 'prepare-blobs: could not read %s\n' "$fstab" >&2
    return 1
  fi
  if ((count > 1)); then
    printf 'prepare-blobs: multiple entries for %s in %s; resolve them before rerunning\n' "$mount_dir" "$fstab" >&2
    return 1
  fi
  if ((count == 1)); then
    if ! current=$(awk -v target="$mount_dir" '$2 == target && $1 !~ /^#/ { print; exit }' "$fstab"); then
      printf 'prepare-blobs: could not read the existing %s entry in %s\n' "$mount_dir" "$fstab" >&2
      return 1
    fi
    [[ $current == "$line" ]] && return 0
    if [[ $current != "$old" ]]; then
      printf 'prepare-blobs: unexpected fstab entry for %s; refusing to replace it\n' "$mount_dir" >&2
      return 1
    fi
  fi

  temp=$(mktemp "$(dirname "$fstab")/.teagram-demo-fstab.XXXXXX") || {
    printf 'prepare-blobs: could not create a temporary fstab beside %s\n' "$fstab" >&2
    return 1
  }
  if ((count == 0)); then
    if ! { cat -- "$fstab" && printf '%s\n' "$line"; } > "$temp"; then
      rm -f -- "$temp"
      printf 'prepare-blobs: could not prepare the nofail fstab entry in %s\n' "$fstab" >&2
      return 1
    fi
  else
    if ! awk -v old="$old" -v line="$line" '$0 == old { print line; next } { print }' "$fstab" > "$temp"; then
      rm -f -- "$temp"
      printf 'prepare-blobs: could not update the nofail fstab entry in %s\n' "$fstab" >&2
      return 1
    fi
  fi
  if ! chown --reference="$fstab" "$temp" || ! chmod --reference="$fstab" "$temp" || ! mv -f -- "$temp" "$fstab"; then
    rm -f -- "$temp"
    printf 'prepare-blobs: could not install the nofail fstab entry in %s\n' "$fstab" >&2
    return 1
  fi
  if ((count == 0)); then
    log "added $mount_dir to $fstab with nofail"
  else
    log "updated $mount_dir in $fstab to include nofail"
  fi
}

prepare_blob_filesystem() {
  local image=$1 created_here=$2 fs_type
  if [[ $created_here == 1 ]]; then
    if ! mkfs.ext4 -q -F -L teagram-blobs -m 0 "$image"; then
      printf 'prepare-blobs: could not format newly created %s; verify it is partial and remove it before rerunning\n' \
        "$image" >&2
      return 1
    fi
    log 'formatted ext4'
    return 0
  fi

  if ! fs_type=$(blkid -o value -s TYPE "$image" 2>/dev/null); then
    printf 'prepare-blobs: could not identify existing %s; refusing to format it. If it is partial, verify and remove it before rerunning\n' \
      "$image" >&2
    return 1
  fi
  if [[ $fs_type != ext4 ]]; then
    printf 'prepare-blobs: %s holds %s, not ext4; refusing to format it. If it is partial, verify and remove it before rerunning\n' \
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
for tool in truncate mkfs.ext4 blkid mountpoint install stat awk mktemp cat chown chmod mv rm; do
  command -v "$tool" >/dev/null 2>&1 || die "missing $tool"
done

install -d -m 0700 "$(dirname "$BACKING_IMAGE")"
created_image=0
[[ ! -L $BACKING_IMAGE ]] || die "$BACKING_IMAGE must not be a symlink"
if [[ ! -e $BACKING_IMAGE ]]; then
  create_blob_image "$BACKING_IMAGE" "$IMAGE_BYTES" || exit 1
  created_image=1
  log "created sparse $BACKING_IMAGE of $IMAGE_BYTES bytes"
fi
size=$(stat -c '%s' -- "$BACKING_IMAGE")
[[ $size == "$IMAGE_BYTES" ]] || die "$BACKING_IMAGE is $size bytes, expected $IMAGE_BYTES; if partial from a failed run, verify and remove it before rerunning"

# Reserved blocks are zero, so the whole 4 GiB is usable and the cap is the
# filesystem size itself. Existing images must be positively identified as
# ext4; blkid errors and unknown signatures never authorize formatting.
prepare_blob_filesystem "$BACKING_IMAGE" "$created_image" || exit 1

ensure_fstab_entry "$FSTAB" "$BACKING_IMAGE" "$BLOB_MOUNT" || exit 1
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
