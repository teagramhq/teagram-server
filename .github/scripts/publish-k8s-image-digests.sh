#!/usr/bin/env bash
set -euo pipefail

if (($# != 2)); then
  printf 'usage: %s <digest-file> <source-revision>\n' "$0" >&2
  exit 2
fi

digest_file="$1"
source_revision="$2"
if [[ ! "$source_revision" =~ ^[0-9a-f]{40}$ ]]; then
  printf 'Invalid source revision for K8s image publication: %s\n' "$source_revision" >&2
  exit 2
fi

images=(
  ghcr.io/teagramhq/teagramd
  ghcr.io/teagramhq/teagram-migrations
  ghcr.io/teagramhq/teagramd-mc-init
  ghcr.io/teagramhq/link-edge-landing
  ghcr.io/teagramhq/link-edge-selector
)

tmp_digest_file="$(mktemp "${digest_file}.tmp.XXXXXX")"
trap 'rm -f -- "$tmp_digest_file"' EXIT
printf '{"commit":"%s","images":[' "$source_revision" > "$tmp_digest_file"
separator=

for image in "${images[@]}"; do
  if ! revision_output="$(docker image inspect "$image:ci" --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' 2>&1)"; then
    printf 'Could not inspect source revision for %s: %s\n' "$image" "$revision_output" >&2
    exit 1
  fi
  if [[ "$revision_output" != "$source_revision" ]]; then
    printf 'Source revision mismatch for %s: expected %s, got %s\n' \
      "$image" "$source_revision" "${revision_output:-<empty>}" >&2
    exit 1
  fi

  tag="$image:sha-$source_revision"
  if ! tag_output="$(docker image tag "$image:ci" "$tag" 2>&1)"; then
    printf 'Could not tag %s as %s: %s\n' "$image" "$tag" "${tag_output:-<empty>}" >&2
    exit 1
  fi
  if ! push_output="$(docker image push --quiet "$tag" 2>&1)"; then
    printf 'Could not push %s: %s\n' "$tag" "${push_output:-<empty>}" >&2
    exit 1
  fi

  if ! repo_digests="$(docker image inspect "$tag" --format '{{range .RepoDigests}}{{println .}}{{end}}' 2>&1)"; then
    printf 'Could not inspect pushed image %s; output: %s\n' \
      "$tag" "${repo_digests:-<empty>}" >&2
    exit 1
  fi
  digest_reference=
  while IFS= read -r reference; do
    if [[ "$reference" == "$image@sha256:"* ]]; then
      digest_reference="$reference"
      break
    fi
  done <<< "$repo_digests"
  digest="${digest_reference##*@}"
  if [[ ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    printf 'No valid GHCR digest found for %s after push; RepoDigests output: %s\n' \
      "$image" "${repo_digests:-<empty>}" >&2
    exit 1
  fi

  printf '%s{"name":"%s","reference":"%s"}' \
    "$separator" "$image" "$digest_reference" >> "$tmp_digest_file"
  separator=,
done

printf ']}\n' >> "$tmp_digest_file"
jq --exit-status --arg commit "$source_revision" \
  '.commit == $commit and (.images | length == 5) and all(.images[]; (.reference | test("@sha256:[0-9a-f]{64}$")))' \
  "$tmp_digest_file" >/dev/null
mv -- "$tmp_digest_file" "$digest_file"
