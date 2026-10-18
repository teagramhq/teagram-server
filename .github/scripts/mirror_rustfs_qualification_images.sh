#!/usr/bin/env bash
set -Eeuo pipefail

: "${GITHUB_ACTOR:?GitHub Actions actor is required}"
: "${GITHUB_TOKEN:?GitHub token is required}"

printf '%s' "$GITHUB_TOKEN" | skopeo login ghcr.io \
  --username "$GITHUB_ACTOR" --password-stdin

mirror_image() {
  local source=$1 destination=$2 expected_digest=$3 actual_digest
  if actual_digest=$(
    skopeo inspect --raw "docker://$destination" 2>/dev/null \
      | sha256sum | awk '{print "sha256:" $1}'
  ); then
    if [ "$actual_digest" = "$expected_digest" ]; then
      printf 'Verified %s at %s\n' "$destination" "$expected_digest"
      return 0
    fi
  fi

  skopeo copy --all --preserve-digests "docker://$source" "docker://$destination"
  actual_digest=$(
    skopeo inspect --raw "docker://$destination" \
      | sha256sum | awk '{print "sha256:" $1}'
  )
  if [ "$actual_digest" != "$expected_digest" ]; then
    printf 'Mirrored image digest mismatch for %s: expected %s, got %s\n' \
      "$destination" "$expected_digest" "$actual_digest" >&2
    return 1
  fi
  printf 'Verified %s at %s\n' "$destination" "$expected_digest"
}

mirror_image \
  'mirror.gcr.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea' \
  'ghcr.io/teagramhq/teagram-server-postgres:16-alpine' \
  'sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea'
mirror_image \
  'mirror.gcr.io/library/postgres@sha256:77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873' \
  'ghcr.io/teagramhq/teagram-server-postgres:18-alpine' \
  'sha256:77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873'
mirror_image \
  'mirror.gcr.io/library/busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e' \
  'ghcr.io/teagramhq/teagram-server-busybox:1.37' \
  'sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e'
