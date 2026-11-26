#!/usr/bin/env bash

DEMO_IMAGE_HISTORY_FILE=${DEMO_IMAGE_HISTORY_FILE:-$DEMO_STATE_DIR/successful-images.json}

demo_image_history_valid() {
  local history_file=$DEMO_IMAGE_HISTORY_FILE
  [[ -f "$history_file" && ! -L "$history_file" &&
    "$(stat -c '%u:%a' "$history_file")" == '0:600' ]] || {
    demo_fail 'successful image history must be a root-owned mode 0600 file'
    return 1
  }
  jq -e --arg repository "$DEMO_IMAGE_REPOSITORY" '
    type == "array" and length <= 3
    and all(.[];
      type == "object"
      and (.revision | type == "string" and test("^[0-9a-f]{40}$"))
      and (.image_id | type == "string" and test("^sha256:[0-9a-f]{64}$"))
      and (.image_ref | type == "string" and startswith($repository + "@sha256:")
        and (.[(($repository + "@sha256:") | length):] | test("^[0-9a-f]{64}$")))
    )
    and ([.[].image_id] | unique | length) == length
  ' "$history_file" >/dev/null || {
    demo_fail 'successful image history is invalid'
    return 1
  }
}

demo_initialize_image_history() {
  local revision=$1 image_id=$2 image_ref=$3 temporary
  [[ ! -L "$DEMO_STATE_DIR" ]] || {
    demo_fail 'demo state directory must not be a symlink'
    return 1
  }
  if [[ -e "$DEMO_IMAGE_HISTORY_FILE" || -L "$DEMO_IMAGE_HISTORY_FILE" ]]; then
    demo_image_history_valid
    return $?
  fi
  install -d -o root -g root -m 0755 "$DEMO_STATE_DIR" || return 1
  temporary=$(mktemp "$DEMO_STATE_DIR/successful-images.XXXXXXXX") || return 1
  if [[ "$revision" =~ ^[0-9a-f]{40}$ && "$image_id" =~ ^sha256:[0-9a-f]{64}$ &&
    "$image_ref" == "$DEMO_IMAGE_REPOSITORY@sha256:"* &&
    "${image_ref#"$DEMO_IMAGE_REPOSITORY@sha256:"}" =~ ^[0-9a-f]{64}$ ]]; then
    jq -n --arg revision "$revision" --arg image_id "$image_id" --arg image_ref "$image_ref" \
      '[{revision: $revision, image_id: $image_id, image_ref: $image_ref}]' > "$temporary"
  else
    printf '[]\n' > "$temporary"
  fi
  chmod 0600 "$temporary" || { rm -f -- "$temporary"; return 1; }
  mv -f -- "$temporary" "$DEMO_IMAGE_HISTORY_FILE" || { rm -f -- "$temporary"; return 1; }
  demo_image_history_valid
}

demo_record_successful_image() {
  local revision=$1 image_id=$2 image_ref=$3 previous_revision=$4 previous_image_id=$5 previous_image_ref=$6
  local history temporary
  [[ "$revision" =~ ^[0-9a-f]{40}$ && "$image_id" =~ ^sha256:[0-9a-f]{64}$ &&
    "$image_ref" == "$DEMO_IMAGE_REPOSITORY@sha256:"* &&
    "${image_ref#"$DEMO_IMAGE_REPOSITORY@sha256:"}" =~ ^[0-9a-f]{64}$ ]] || {
    demo_fail 'new successful image identity is invalid'
    return 1
  }
  demo_image_history_valid || return 1
  history=$(<"$DEMO_IMAGE_HISTORY_FILE") || return 1
  temporary=$(mktemp "$DEMO_STATE_DIR/successful-images.XXXXXXXX") || return 1
  jq -n --arg repository "$DEMO_IMAGE_REPOSITORY" \
    --arg revision "$revision" --arg image_id "$image_id" --arg image_ref "$image_ref" \
    --arg previous_revision "$previous_revision" \
    --arg previous_image_id "$previous_image_id" --arg previous_image_ref "$previous_image_ref" \
    --argjson history "$history" '
      ([{revision: $revision, image_id: $image_id, image_ref: $image_ref}]
       + (if ($previous_revision | test("^[0-9a-f]{40}$"))
            and ($previous_image_id | test("^sha256:[0-9a-f]{64}$"))
            and ($previous_image_ref | startswith($repository + "@sha256:")
              and (.[(($repository + "@sha256:") | length):] | test("^[0-9a-f]{64}$")))
          then [{revision: $previous_revision, image_id: $previous_image_id, image_ref: $previous_image_ref}]
          else [] end)
       + $history)
      | reduce .[] as $item ([]; if any(.[]; .image_id == $item.image_id) then . else . + [$item] end)
      | .[:3]
    ' > "$temporary" || { rm -f -- "$temporary"; return 1; }
  chmod 0600 "$temporary" || { rm -f -- "$temporary"; return 1; }
  mv -f -- "$temporary" "$DEMO_IMAGE_HISTORY_FILE" || { rm -f -- "$temporary"; return 1; }
  demo_image_history_valid
}

demo_prune_images() {
  local running_image_id inventory image_id repository refs tags containers rc=0 keep_json
  local -a keep_ids=() image_ids=()
  demo_image_history_valid || return 1
  keep_json=$(jq -r '.[].image_id' "$DEMO_IMAGE_HISTORY_FILE") || return 1
  while IFS= read -r image_id; do
    [[ "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]] && keep_ids+=("$image_id")
  done <<< "$keep_json"
  for running_image_id in "$@"; do
    [[ "$running_image_id" =~ ^sha256:[0-9a-f]{64}$ ]] || {
      demo_fail 'running demo image ID is invalid during retention'
      return 1
    }
    keep_ids+=("$running_image_id")
  done
  if ! inventory=$(docker image ls --digests --no-trunc --format '{{.ID}} {{.Repository}}'); then
    demo_fail 'demo image inventory could not be read'
    return 1
  fi
  while read -r image_id repository; do
    [[ "$repository" == "$DEMO_IMAGE_REPOSITORY" && "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]] &&
      image_ids+=("$image_id")
  done <<< "$inventory"
  mapfile -t image_ids < <(printf '%s\n' "${image_ids[@]}" | sed '/^$/d' | sort -u)
  mapfile -t keep_ids < <(printf '%s\n' "${keep_ids[@]}" | sed '/^$/d' | sort -u)
  for image_id in "${image_ids[@]}"; do
    if printf '%s\n' "${keep_ids[@]}" | grep -Fxq -- "$image_id"; then
      continue
    fi
    if ! containers=$(docker ps -a -q --no-trunc --filter "ancestor=$image_id"); then
      printf 'could not check container references for demo image %s\n' "$image_id" >&2
      rc=1
      continue
    fi
    if [[ -n "$containers" ]]; then
      printf 'could not remove demo image %s; it is referenced by container(s)\n' "$image_id" >&2
      rc=1
      continue
    fi
    if ! refs=$(docker image inspect --format '{{json .RepoDigests}}' "$image_id") ||
      ! tags=$(docker image inspect --format '{{json .RepoTags}}' "$image_id"); then
      printf 'could not inspect demo image references for %s\n' "$image_id" >&2
      rc=1
      continue
    fi
    if ! jq -en --arg repository "$DEMO_IMAGE_REPOSITORY" --argjson values "$refs" \
      'all($values[]; startswith($repository + "@"))' >/dev/null ||
      ! jq -en --arg repository "$DEMO_IMAGE_REPOSITORY" --argjson values "$tags" \
        'all($values[]; . == "<none>:<none>" or startswith($repository + ":"))' >/dev/null; then
      printf 'preserving demo image %s because it is also named by another repository\n' "$image_id" >&2
      continue
    fi
    if ! docker image rm "$image_id"; then
      printf 'could not remove demo image %s\n' "$image_id" >&2
      rc=1
    fi
  done
  return "$rc"
}
