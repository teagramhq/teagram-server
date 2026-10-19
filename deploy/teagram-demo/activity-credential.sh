#!/usr/bin/env bash

demo_stage_activity_credential() {
  local credentials_dir="${CREDENTIALS_DIRECTORY:-}"
  local runtime_dir="${RUNTIME_DIRECTORY:-}"
  local credential_file staged_file

  if [[ -z "$credentials_dir" || "$credentials_dir" != /* || -L "$credentials_dir" ]]; then
    demo_fail 'systemd activity credential directory is missing or unsafe'
    return 1
  fi
  credential_file="$credentials_dir/inventory.json"
  if [[ ! -f "$credential_file" || -L "$credential_file" ]]; then
    demo_fail 'systemd activity credential is missing or unsafe'
    return 1
  fi
  if [[ "$(stat -c '%u:%a' "$credential_file")" != '0:400' ]]; then
    demo_fail 'systemd activity credential must be root-owned mode 0400'
    return 1
  fi

  if [[ "$runtime_dir" != /run/* || ! -d "$runtime_dir" || -L "$runtime_dir" ]]; then
    demo_fail 'activity credential runtime directory is missing or unsafe'
    return 1
  fi
  if [[ "$(stat -c '%u:%a' "$runtime_dir")" != '0:700' ]]; then
    demo_fail 'activity credential runtime directory must be root-owned mode 0700'
    return 1
  fi

  staged_file="$runtime_dir/inventory.json"
  if [[ -e "$staged_file" || -L "$staged_file" ]]; then
    demo_fail 'activity credential staging file already exists'
    return 1
  fi
  if ! install -o root -g root -m 0400 -- "$credential_file" "$staged_file"; then
    demo_fail 'could not stage activity credential for the container runtime'
    return 1
  fi
  [[ "$(stat -c '%u:%a' "$staged_file")" == '0:400' ]] || {
    rm -f -- "$staged_file"
    demo_fail 'staged activity credential must be root-owned mode 0400'
    return 1
  }

  printf '%s\n' "$staged_file"
}

demo_cleanup_activity_credential() {
  local staged_file="${1:-}"
  [[ -z "$staged_file" ]] || rm -f -- "$staged_file"
}
