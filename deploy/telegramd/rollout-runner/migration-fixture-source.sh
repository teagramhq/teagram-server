#!/usr/bin/env bash

readonly IMMUTABLE_MIGRATION_SOURCE_COMMIT=9139dd19222d002a2dfe83ae7fd261e0e0134d9e

load_production_migration_pins() {
  local schema_gate=$1
  local -a revision_rows=()

  source <(sed -n \
    -e '/^readonly -a RELEASE_FILES=(/,/^)/p' \
    -e '/^readonly -a RELEASE_SHA256=(/,/^)/p' \
    -e '/^readonly APPROVED_ATLAS_SUM_SHA256=/p' \
    "$schema_gate")

  if [ "${#RELEASE_FILES[@]}" -ne 8 ] || \
     [ "${#RELEASE_SHA256[@]}" -ne "${#RELEASE_FILES[@]}" ] || \
     [[ ! "$APPROVED_ATLAS_SUM_SHA256" =~ ^[0-9a-f]{64}$ ]]; then
    printf '%s\n' 'production schema gate migration pins are incomplete' >&2
    return 1
  fi

  mapfile -t revision_rows < <(
    sed -n \
      '/^[[:space:]]*JOIN (VALUES$/,/^[[:space:]]*) AS approved(version, expected_hash) ON approved.version = revision.version::text/p' \
      "$schema_gate" |
      sed -nE "s/^[[:space:]]*\('([0-9]{14})', '(h1:[A-Za-z0-9+/=]+)'\),?$/\1 \2/p"
  )
  if [ "${#revision_rows[@]}" -ne "${#RELEASE_FILES[@]}" ]; then
    printf '%s\n' 'production schema gate Atlas revision pins are incomplete' >&2
    return 1
  fi

  APPROVED_REVISIONS=()
  APPROVED_REVISION_HASHES=()
  local index row filename revision hash
  for index in "${!RELEASE_FILES[@]}"; do
    filename=${RELEASE_FILES[$index]}
    revision=${filename%%_*}
    row=${revision_rows[$index]}
    hash=${row#* }
    if [ "${row%% *}" != "$revision" ] || [[ ! "$hash" =~ ^h1:[A-Za-z0-9+/=]+$ ]]; then
      printf 'production schema gate Atlas revision pin does not match %s\n' "$filename" >&2
      return 1
    fi
    APPROVED_REVISIONS+=("$revision")
    APPROVED_REVISION_HASHES+=("$hash")
  done
}

prepare_immutable_migration_source() {
  local repo_root=$1 destination=$2 migrations_dir first_version path basename version
  local expected actual_sha index
  local -a actual_releases=() expected_releases=()

  if ! git -c "safe.directory=$repo_root" -C "$repo_root" cat-file -e \
      "$IMMUTABLE_MIGRATION_SOURCE_COMMIT^{commit}" 2>/dev/null; then
    printf 'immutable migration source commit is missing: %s\n' \
      "$IMMUTABLE_MIGRATION_SOURCE_COMMIT" >&2
    return 1
  fi

  mkdir -m 700 -- "$destination"
  if ! git -c "safe.directory=$repo_root" -C "$repo_root" archive \
      --format=tar "$IMMUTABLE_MIGRATION_SOURCE_COMMIT" migrations |
      tar -xf - -C "$destination"; then
    printf '%s\n' 'could not extract immutable migration source commit' >&2
    return 1
  fi
  migrations_dir="$destination/migrations"
  [ -d "$migrations_dir" ] || {
    printf '%s\n' 'immutable migration source has no migrations directory' >&2
    return 1
  }

  first_version=${RELEASE_FILES[0]%%_*}
  while IFS= read -r -d '' path; do
    basename=${path##*/}
    if [[ "$basename" =~ ^([0-9]{14})_.*\.sql$ ]]; then
      version=${BASH_REMATCH[1]}
      if [[ "$version" > "$first_version" || "$version" = "$first_version" ]]; then
        actual_releases+=("${path#"$migrations_dir"/}")
      fi
    elif [[ "$basename" = *.sql ]]; then
      printf 'immutable migration filename is malformed: %s\n' "$basename" >&2
      return 1
    fi
  done < <(find "$migrations_dir" -type f -print0)

  mapfile -t actual_releases < <(printf '%s\n' "${actual_releases[@]}" | sort)
  mapfile -t expected_releases < <(printf '%s\n' "${RELEASE_FILES[@]}" | sort)
  if [ "${actual_releases[*]}" != "${expected_releases[*]}" ]; then
    printf '%s\n' 'immutable migration source release set differs from the production gate pins' >&2
    return 1
  fi

  for index in "${!RELEASE_FILES[@]}"; do
    path="$migrations_dir/${RELEASE_FILES[$index]}"
    if [ ! -f "$path" ] || [ -L "$path" ]; then
      printf 'immutable migration source is missing %s\n' "${RELEASE_FILES[$index]}" >&2
      return 1
    fi
    actual_sha=$(sha256sum -- "$path") || return 1
    actual_sha=${actual_sha%% *}
    expected=${RELEASE_SHA256[$index]}
    if [ "$actual_sha" != "$expected" ]; then
      printf 'immutable migration source pin mismatch for %s\n' "${RELEASE_FILES[$index]}" >&2
      return 1
    fi
  done

  if [ ! -f "$migrations_dir/atlas.sum" ] || [ -L "$migrations_dir/atlas.sum" ]; then
    printf '%s\n' 'immutable migration source Atlas sum is missing or not a regular file' >&2
    return 1
  fi
  actual_sha=$(sha256sum -- "$migrations_dir/atlas.sum") || return 1
  actual_sha=${actual_sha%% *}
  if [ "$actual_sha" != "$APPROVED_ATLAS_SUM_SHA256" ]; then
    printf '%s\n' 'immutable migration source Atlas sum pin mismatch' >&2
    return 1
  fi
}
