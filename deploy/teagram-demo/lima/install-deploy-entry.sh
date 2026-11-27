#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly REPO=/opt/teagram-demo
readonly SCRIPT_DIR="$REPO/deploy/teagram-demo/lima"
readonly DEPLOY_USER=root
readonly LEGACY_DEPLOY_USER=teagram-deploy
readonly SSH_CONFIG=/etc/ssh/teagram-demo-deploy_sshd_config
readonly AUTHORIZED_KEYS_DIR=/etc/ssh/authorized_keys
readonly AUTHORIZED_KEYS="$AUTHORIZED_KEYS_DIR/teagram-demo-deploy-root"
readonly LEGACY_AUTHORIZED_KEYS="$AUTHORIZED_KEYS_DIR/$LEGACY_DEPLOY_USER"
readonly LEGACY_ENTRY_PATH=/usr/local/libexec/teagram-demo-deploy-entry
readonly ROOT_WRAPPER=/usr/local/sbin/teagram-demo-deploy
readonly LEGACY_SUDOERS_FILE=/etc/sudoers.d/teagram-deploy
readonly SERVICE_NAME=teagram-demo-deploy-sshd
readonly SERVICE_FILE=/etc/init.d/teagram-demo-deploy-sshd
readonly ADDRESS=100.117.107.78
readonly PORT=2223
readonly PUBLIC_KEY=${TEAGRAM_DEMO_DEPLOY_PUBLIC_KEY:-}

TMP_DIR=
SSH_BASE_HASH=
SSH_DROPIN_HASH=
SSH_BASE_EFFECTIVE=
SSHD_BIN=
STAGED_SSH_CONFIG=
STAGED_SERVICE_FILE=
STAGED_ROOT_WRAPPER=
STAGED_AUTHORIZED_KEYS=
ACTIVATION_STARTED=0
ACTIVATION_COMPLETE=0
SERVICE_WAS_ACTIVE=0
SERVICE_WAS_ENABLED=0

fail() {
  printf 'lima deploy entry install: %s\n' "$1" >&2
  exit 1
}

verify_checkout_ownership() {
  local path path_state path_owner path_mode unsafe_checkout_path=
  while IFS= read -r -d '' path; do
    path_state=$(stat -c '%u:%a' "$path") || {
      printf 'could not inspect checkout path: %s\n' "$path" >&2
      return 1
    }
    path_owner=${path_state%%:*}
    path_mode=${path_state#*:}
    if [[ "$path_owner" != 0 ]] || (( (8#$path_mode & 0022) != 0 )); then
      unsafe_checkout_path=$path
      break
    fi
  done < <(find "$REPO" -xdev -print0)
  [[ -z "$unsafe_checkout_path" ]] || {
    printf 'checkout contains a non-root-owned or writable path: %s\n' "$unsafe_checkout_path" >&2
    return 1
  }
}

migrate_legacy_runtime_path() {
  local legacy_path=$1 runtime_path=$2 runtime_contents legacy_device runtime_device
  if [[ ! -e "$legacy_path" && ! -L "$legacy_path" ]]; then
    return 0
  fi
  [[ ! -L "$legacy_path" && -d "$legacy_path" ]] || fail "legacy runtime path is not a real directory: $legacy_path"
  [[ ! -L "$runtime_path" && -d "$runtime_path" ]] || fail "runtime destination is not a real directory: $runtime_path"
  runtime_contents=$(find "$runtime_path" -mindepth 1 -maxdepth 1 -print -quit) || fail "could not inspect runtime destination: $runtime_path"
  [[ -z "$runtime_contents" ]] || fail "runtime destination is not empty: $runtime_path"
  legacy_device=$(stat -c '%d' "$legacy_path") || fail "could not inspect legacy runtime device: $legacy_path"
  runtime_device=$(stat -c '%d' "$RUNTIME_DIR") || fail "could not inspect runtime device: $RUNTIME_DIR"
  [[ "$legacy_device" == "$runtime_device" ]] || fail "legacy runtime migration crosses filesystems: $legacy_path"
  rmdir -- "$runtime_path" || fail "could not remove empty runtime destination: $runtime_path"
  mv -- "$legacy_path" "$runtime_path" || fail "could not migrate legacy runtime path: $legacy_path"
}

migrate_legacy_runtime_paths() {
  local legacy_state="$REPO/.state/teagram-demo" legacy_tmp="$REPO/.state/teagram-demo/tmp" path
  for path in "$REPO/.state" "$legacy_state" "$legacy_tmp"; do
    [[ ! -L "$path" ]] || fail "legacy runtime parent is a symlink: $path"
  done

  prepare_runtime_dirs || fail 'could not prepare proxy runtime paths for legacy migration'
  migrate_legacy_runtime_path "$legacy_tmp/turnstile-egress" "$RUNTIME_DIR/tmp/turnstile-egress"
  migrate_legacy_runtime_path "$legacy_tmp/haproxy" "$RUNTIME_DIR/tmp/haproxy"
  migrate_legacy_runtime_path "$legacy_tmp/edge" "$RUNTIME_DIR/tmp/edge"
  migrate_legacy_runtime_path "$legacy_state/haproxy-run" "$RUNTIME_DIR/haproxy-run"
  prepare_runtime_dirs || fail 'migrated proxy runtime paths have unexpected ownership or modes'
}

cleanup() {
  local rc=$?
  trap - EXIT HUP INT TERM
  if (( ACTIVATION_STARTED == 1 && ACTIVATION_COMPLETE == 0 )); then
    rollback_deploy_entry || rc=1
  fi
  [[ -z "$TMP_DIR" ]] || rm -rf -- "$TMP_DIR" || rc=1
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

require_root_owned() {
  local path=$1 mode
  [[ -e "$path" && ! -L "$path" && "$(stat -c '%u' "$path")" == 0 ]] ||
    fail "required root-owned path is unavailable: $path"
  mode=$(stat -c '%a' "$path") || fail "could not inspect permissions: $path"
  (( (8#$mode & 0022) == 0 )) || fail "path is group- or world-writable: $path"
}

install_file() {
  local source=$1 destination=$2 mode=$3 directory temporary
  [[ -f "$source" && ! -L "$source" ]] || fail "installer source is missing: $source"
  directory=$(dirname -- "$destination")
  install -d -o root -g root -m 0755 "$directory"
  [[ ! -L "$directory" ]] || fail "installer destination directory is unsafe: $directory"
  if [[ -e "$destination" || -L "$destination" ]]; then
    [[ -f "$destination" && ! -L "$destination" ]] || fail "installer destination is unsafe: $destination"
    if cmp -s "$source" "$destination" && [[ "$(stat -c '%u:%a' "$destination")" == "0:$mode" ]]; then
      return 0
    fi
  fi
  temporary=$(mktemp "$directory/.${destination##*/}.XXXXXXXX") || fail "could not stage $destination"
  if ! cat -- "$source" > "$temporary" || ! chown root:root "$temporary" || ! chmod "$mode" "$temporary" ||
    ! mv -f -- "$temporary" "$destination"; then
    rm -f -- "$temporary"
    fail "could not install $destination"
  fi
}

install_key_file() {
  local content=$1 temporary
  [[ ! -L "$AUTHORIZED_KEYS_DIR" ]] || fail 'authorized keys directory must not be a symlink'
  install -d -o root -g root -m 0755 "$AUTHORIZED_KEYS_DIR"
  require_root_owned "$AUTHORIZED_KEYS_DIR"
  if [[ -e "$AUTHORIZED_KEYS" || -L "$AUTHORIZED_KEYS" ]]; then
    [[ -f "$AUTHORIZED_KEYS" && ! -L "$AUTHORIZED_KEYS" ]] || fail 'deploy authorized keys file is unsafe'
    if [[ "$(stat -c '%u:%a' "$AUTHORIZED_KEYS")" == '0:600' && "$(<"$AUTHORIZED_KEYS")" == "$content" ]]; then
      return 0
    fi
    fail 'deploy authorized keys file differs from the requested deploy key'
  fi
  temporary=$(mktemp "$AUTHORIZED_KEYS_DIR/.${DEPLOY_USER}.XXXXXXXX") || fail 'could not stage deploy authorized keys'
  if ! printf '%s\n' "$content" > "$temporary" || ! chown root:root "$temporary" || ! chmod 0600 "$temporary" ||
    ! mv -f -- "$temporary" "$AUTHORIZED_KEYS"; then
    rm -f -- "$temporary"
    fail 'could not install deploy authorized keys'
  fi
}

validate_deploy_key_file() {
  [[ ! -L "$AUTHORIZED_KEYS_DIR" ]] || fail 'authorized keys directory must not be a symlink'
  if [[ -e "$AUTHORIZED_KEYS_DIR" ]]; then
    [[ -d "$AUTHORIZED_KEYS_DIR" ]] || fail 'authorized keys path must be a directory'
    require_root_owned "$AUTHORIZED_KEYS_DIR"
  fi
  if [[ -e "$AUTHORIZED_KEYS" || -L "$AUTHORIZED_KEYS" ]]; then
    [[ -f "$AUTHORIZED_KEYS" && ! -L "$AUTHORIZED_KEYS" ]] || fail 'deploy authorized keys file is unsafe'
    [[ "$(stat -c '%u:%a' "$AUTHORIZED_KEYS")" == '0:600' && "$(<"$AUTHORIZED_KEYS")" == "$KEY_LINE" ]] ||
      fail 'deploy authorized keys file differs from the requested deploy key'
  fi
}

snapshot_file() {
  local source=$1 destination=$2
  if [[ -e "$source" || -L "$source" ]]; then
    [[ -f "$source" && ! -L "$source" ]] || fail "installer destination is unsafe: $source"
    require_root_owned "$source"
    cp -p -- "$source" "$destination" || fail "could not snapshot installer destination: $source"
  fi
}

restore_file() {
  local backup=$1 destination=$2 directory mode temporary
  directory=$(dirname -- "$destination") || return 1
  if [[ ! -f "$backup" ]]; then
    rm -f -- "$destination" || return 1
    return 0
  fi
  [[ -d "$directory" && ! -L "$directory" ]] || return 1
  mode=$(stat -c '%a' "$backup") || return 1
  temporary=$(mktemp "$directory/.${destination##*/}.rollback.XXXXXXXX") || return 1
  if ! cat -- "$backup" > "$temporary" || ! chown root:root "$temporary" ||
    ! chmod "$mode" "$temporary" || ! mv -f -- "$temporary" "$destination"; then
    rm -f -- "$temporary"
    return 1
  fi
}

dedicated_listener_ready() {
  netstat -ltn 2>/dev/null | awk -v endpoint="$ADDRESS:$PORT" '$4 == endpoint { found=1 } END { exit !found }'
}

rollback_deploy_entry() {
  local rollback_failed=0 runlevel_entry="/etc/runlevels/default/$SERVICE_NAME"
  if rc-service "$SERVICE_NAME" status >/dev/null 2>&1; then
    rc-service "$SERVICE_NAME" stop >/dev/null 2>&1 || {
      printf 'lima deploy entry install: rollback could not stop the new listener\n' >&2
      rollback_failed=1
    }
  fi

  restore_file "$TMP_DIR/previous-sshd-config" "$SSH_CONFIG" || {
    printf 'lima deploy entry install: rollback could not restore sshd config\n' >&2
    rollback_failed=1
  }
  restore_file "$TMP_DIR/previous-service-file" "$SERVICE_FILE" || {
    printf 'lima deploy entry install: rollback could not restore the SSH service file\n' >&2
    rollback_failed=1
  }
  restore_file "$TMP_DIR/previous-root-wrapper" "$ROOT_WRAPPER" || {
    printf 'lima deploy entry install: rollback could not restore the root wrapper\n' >&2
    rollback_failed=1
  }
  restore_file "$TMP_DIR/previous-authorized-keys" "$AUTHORIZED_KEYS" || {
    printf 'lima deploy entry install: rollback could not restore deploy authorized keys\n' >&2
    rollback_failed=1
  }

  if (( SERVICE_WAS_ENABLED == 0 )) && [[ -e "$runlevel_entry" || -L "$runlevel_entry" ]]; then
    rc-update del "$SERVICE_NAME" default >/dev/null 2>&1 || {
      printf 'lima deploy entry install: rollback could not restore listener runlevel state\n' >&2
      rollback_failed=1
    }
  fi
  if (( SERVICE_WAS_ACTIVE == 1 )); then
    if rc-service "$SERVICE_NAME" status >/dev/null 2>&1; then
      rc-service "$SERVICE_NAME" restart >/dev/null 2>&1 || {
        printf 'lima deploy entry install: rollback could not restart the previous listener\n' >&2
        rollback_failed=1
      }
    else
      rc-service "$SERVICE_NAME" start >/dev/null 2>&1 || {
        printf 'lima deploy entry install: rollback could not start the previous listener\n' >&2
        rollback_failed=1
      }
    fi
    rc-service "$SERVICE_NAME" status >/dev/null 2>&1 || {
      printf 'lima deploy entry install: previous listener is not active after rollback\n' >&2
      rollback_failed=1
    }
    local listener_ready=0
    for _ in {1..10}; do
      if dedicated_listener_ready; then
        listener_ready=1
        break
      fi
      sleep 1
    done
    if (( listener_ready != 1 )); then
      printf 'lima deploy entry install: previous listener did not bind %s:%s after rollback\n' "$ADDRESS" "$PORT" >&2
      rollback_failed=1
    fi
  fi

  if (( rollback_failed == 1 )); then
    printf 'lima deploy entry install: rollback of previous files or listener failed\n' >&2
    return 1
  fi
}

stage_deploy_entry() {
  local expected
  STAGED_SSH_CONFIG="$TMP_DIR/sshd_config"
  STAGED_SERVICE_FILE="$TMP_DIR/teagram-demo-deploy-sshd.initd"
  STAGED_ROOT_WRAPPER="$TMP_DIR/deploy-root-wrapper.sh"
  STAGED_AUTHORIZED_KEYS="$TMP_DIR/authorized_keys"

  install_file "$SCRIPT_DIR/sshd_config" "$STAGED_SSH_CONFIG" 0600
  install_file "$SCRIPT_DIR/teagram-demo-deploy-sshd.initd" "$STAGED_SERVICE_FILE" 0755
  install_file "$SCRIPT_DIR/deploy-root-wrapper.sh" "$STAGED_ROOT_WRAPPER" 0755
  sh -n "$STAGED_SERVICE_FILE" || fail 'staged dedicated SSH service failed shell validation'
  printf '%s\n' "$KEY_LINE" > "$STAGED_AUTHORIZED_KEYS" || fail 'could not stage deploy authorized keys'
  chown root:root "$STAGED_AUTHORIZED_KEYS" && chmod 0600 "$STAGED_AUTHORIZED_KEYS" ||
    fail 'could not set staged deploy authorized keys permissions'
  ssh-keygen -lf "$STAGED_AUTHORIZED_KEYS" >/dev/null || fail 'staged deploy authorized key failed validation'
  validate_deploy_key_file

  bash -n "$STAGED_ROOT_WRAPPER" || fail 'staged root wrapper failed shell validation'
  "$SSHD_BIN" -t -f "$STAGED_SSH_CONFIG" || fail 'dedicated deploy sshd config failed validation'
  "$SSHD_BIN" -T -f "$STAGED_SSH_CONFIG" > "$TMP_DIR/sshd-deploy.effective" ||
    fail 'could not inspect staged dedicated deploy sshd config'
  for expected in \
    'port 2223' 'addressfamily inet' 'listenaddress 100.117.107.78:2223' \
    'allowusers root' 'authorizedkeysfile /etc/ssh/authorized_keys/teagram-demo-deploy-root' \
    'authenticationmethods publickey' 'passwordauthentication no' \
    'kbdinteractiveauthentication no' 'permitrootlogin forced-commands-only' \
    'disableforwarding yes' 'permittty no' 'permittunnel no' \
    'permituserenvironment no' 'permituserrc no' \
    'forcecommand /usr/local/sbin/teagram-demo-deploy'; do
    grep -Fqx "$expected" "$TMP_DIR/sshd-deploy.effective" || fail "dedicated sshd config is missing: $expected"
  done
  [[ "$(grep -c '^listenaddress ' "$TMP_DIR/sshd-deploy.effective")" == 1 ]] ||
    fail 'dedicated sshd must listen on the tailnet address only'
}

activate_deploy_entry() {
  snapshot_file "$SSH_CONFIG" "$TMP_DIR/previous-sshd-config"
  snapshot_file "$SERVICE_FILE" "$TMP_DIR/previous-service-file"
  snapshot_file "$ROOT_WRAPPER" "$TMP_DIR/previous-root-wrapper"
  snapshot_file "$AUTHORIZED_KEYS" "$TMP_DIR/previous-authorized-keys"

  SERVICE_WAS_ACTIVE=0
  if rc-service "$SERVICE_NAME" status >/dev/null 2>&1; then
    SERVICE_WAS_ACTIVE=1
  fi
  SERVICE_WAS_ENABLED=0
  if [[ -e "/etc/runlevels/default/$SERVICE_NAME" || -L "/etc/runlevels/default/$SERVICE_NAME" ]]; then
    SERVICE_WAS_ENABLED=1
  fi

  ACTIVATION_STARTED=1
  install_key_file "$KEY_LINE"
  install_file "$STAGED_SSH_CONFIG" "$SSH_CONFIG" 0600
  install_file "$STAGED_SERVICE_FILE" "$SERVICE_FILE" 0755
  # Keep this last so a legacy listener can continue through the staged wrapper
  # compatibility path until its config has been reloaded.
  install_file "$STAGED_ROOT_WRAPPER" "$ROOT_WRAPPER" 0755

  rc-update add "$SERVICE_NAME" default || fail 'could not enable the dedicated SSH listener at boot'
  if (( SERVICE_WAS_ACTIVE == 1 )); then
    rc-service "$SERVICE_NAME" restart || fail 'could not restart the dedicated SSH listener'
  else
    rc-service "$SERVICE_NAME" start || fail 'could not start the dedicated SSH listener'
  fi

  local listener_ready=0
  for _ in {1..10}; do
    if dedicated_listener_ready; then
      listener_ready=1
      break
    fi
    sleep 1
  done
  (( listener_ready == 1 )) || fail "dedicated listener did not bind $ADDRESS:$PORT"
  if netstat -ltn 2>/dev/null | awk -v endpoint="0.0.0.0:$PORT" -v ipv6=":::${PORT}" \
    '$4 == endpoint || $4 == ipv6 { found=1 } END { exit !found }'; then
    fail 'dedicated listener unexpectedly bound a wildcard address'
  fi

  [[ "$(sha256sum /etc/ssh/sshd_config | awk '{print $1}')" == "$SSH_BASE_HASH" ]] ||
    fail 'port 22 sshd_config changed during deploy entry installation'
  [[ "$(directory_hash /etc/ssh/sshd_config.d)" == "$SSH_DROPIN_HASH" ]] ||
    fail 'port 22 sshd_config.d changed during deploy entry installation'
  "$SSHD_BIN" -T -f /etc/ssh/sshd_config > "$TMP_DIR/sshd-main.after" ||
    fail 'port 22 sshd config failed its post-install check'
  cmp -s "$SSH_BASE_EFFECTIVE" "$TMP_DIR/sshd-main.after" || fail 'effective port 22 sshd config changed'
  ACTIVATION_COMPLETE=1
}

reject_unmanaged_sudoers_rule() {
  local file=$1 violation=$2 grep_status
  if grep -Eq "^[[:space:]]*${LEGACY_DEPLOY_USER}[[:space:]]" "$file"; then
    fail "$violation"
  else
    grep_status=$?
  fi
  [[ "$grep_status" -eq 1 ]] || fail "could not inspect sudoers file: $file"
}

verify_legacy_deploy_access() {
  local account_line account_uid account_home account_shell groups group path account_groups sudoers_scan
  account_line=$(awk -F: -v name="$LEGACY_DEPLOY_USER" '$1 == name { print $3 ":" $6 ":" $7 }' /etc/passwd) ||
    fail 'could not inspect legacy deploy account'
  if [[ -n "$account_line" ]]; then
    [[ "$account_line" != *$'\n'* ]] || fail 'multiple legacy deploy accounts exist'
    IFS=: read -r account_uid account_home account_shell <<< "$account_line"
    [[ "$account_uid" =~ ^[1-9][0-9]*$ && "$account_home" == /var/empty && "$account_shell" == /bin/sh ]] ||
      fail 'legacy deploy account has unexpected identity settings'
    groups=$(id -nG "$LEGACY_DEPLOY_USER") || fail 'could not inspect legacy deploy account groups'
    read -r -a account_groups <<< "$groups"
    for group in "${account_groups[@]}"; do
      case "$group" in docker|wheel|sudo) fail "legacy deploy account unexpectedly belongs to $group" ;; esac
    done
    command -v deluser >/dev/null 2>&1 || fail 'deluser is required to remove the legacy deploy account'
  fi

  if [[ -e "$LEGACY_SUDOERS_FILE" || -L "$LEGACY_SUDOERS_FILE" ]]; then
    [[ -f "$LEGACY_SUDOERS_FILE" && ! -L "$LEGACY_SUDOERS_FILE" &&
      "$(stat -c '%u:%a' "$LEGACY_SUDOERS_FILE")" == '0:440' ]] ||
      fail 'legacy deploy sudoers file is unsafe'
    [[ "$(grep -Ev '^[[:space:]]*(#|$)' "$LEGACY_SUDOERS_FILE")" == 'teagram-deploy ALL=(root) NOPASSWD: /usr/local/sbin/teagram-demo-deploy' ]] ||
      fail 'legacy deploy sudoers file contains an unexpected rule'
  fi
  if [[ -f /etc/sudoers ]]; then
    reject_unmanaged_sudoers_rule /etc/sudoers \
      'an unmanaged sudoers rule still grants access to the legacy deploy account'
  fi
  if [[ -d /etc/sudoers.d ]]; then
    sudoers_scan=$(mktemp "$TMP_DIR/sudoers-files.XXXXXXXX") || fail 'could not stage sudoers file scan'
    if ! find /etc/sudoers.d -type f -print0 > "$sudoers_scan"; then
      fail 'could not enumerate sudoers files'
    fi
    while IFS= read -r -d '' path; do
      [[ "$path" == "$LEGACY_SUDOERS_FILE" ]] && continue
      reject_unmanaged_sudoers_rule "$path" \
        "an unmanaged sudoers file still grants access to the legacy deploy account: $path"
    done < "$sudoers_scan"
    rm -f -- "$sudoers_scan" || fail 'could not remove staged sudoers file scan'
  fi

  for path in "$LEGACY_AUTHORIZED_KEYS" "$LEGACY_ENTRY_PATH"; do
    if [[ -e "$path" || -L "$path" ]]; then
      [[ -f "$path" && ! -L "$path" ]] || fail "legacy deploy path is unsafe: $path"
      require_root_owned "$path"
    fi
  done
}

remove_legacy_deploy_access() {
  verify_legacy_deploy_access
  [[ ! -e "$LEGACY_SUDOERS_FILE" && ! -L "$LEGACY_SUDOERS_FILE" ]] ||
    rm -- "$LEGACY_SUDOERS_FILE" || fail 'could not remove legacy deploy sudoers file'
  [[ ! -e "$LEGACY_AUTHORIZED_KEYS" && ! -L "$LEGACY_AUTHORIZED_KEYS" ]] ||
    rm -- "$LEGACY_AUTHORIZED_KEYS" || fail 'could not remove legacy deploy authorized keys'
  [[ ! -e "$LEGACY_ENTRY_PATH" && ! -L "$LEGACY_ENTRY_PATH" ]] ||
    rm -- "$LEGACY_ENTRY_PATH" || fail 'could not remove legacy deploy entrypoint'
  if awk -F: -v name="$LEGACY_DEPLOY_USER" '$1 == name { found=1 } END { exit !found }' /etc/passwd; then
    deluser "$LEGACY_DEPLOY_USER" || fail 'could not remove legacy deploy account'
  fi
}

directory_hash() {
  local directory=$1
  if [[ ! -d "$directory" ]]; then
    printf 'absent\n'
    return 0
  fi
  find "$directory" -type f -print0 | sort -z | xargs -0 -r sha256sum | sha256sum | awk '{print $1}'
}

[[ "$EUID" -eq 0 ]] || fail 'must run as root'
[[ -d "$REPO/.git" && ! -L "$REPO" && ! -L "$REPO/.git" ]] || fail 'root-owned lima checkout is unavailable'
TMP_DIR=$(mktemp -d /tmp/teagram-demo-deploy-entry.XXXXXXXX) || fail 'could not create installer verification directory'
for executable in \
  /opt "$REPO" "$REPO/.git" "$REPO/deploy/teagram-demo/forced-command.sh" \
  "$SCRIPT_DIR/command-contract.sh" "$SCRIPT_DIR/deploy-root-wrapper.sh" \
  "$SCRIPT_DIR/deploy-main.sh" "$SCRIPT_DIR/check.sh" \
  "$SCRIPT_DIR/prepare-blobs.sh" "$REPO/deploy/teagram-demo/demo-common.sh" \
  "$REPO/deploy/teagram-demo/image-retention.sh"; do
  require_root_owned "$executable"
done
# shellcheck source=deploy/teagram-demo/lima/prepare-blobs.sh
source "$SCRIPT_DIR/prepare-blobs.sh"
migrate_legacy_runtime_paths || fail 'could not migrate legacy proxy runtime paths out of the checkout'
verify_checkout_ownership || fail 'checkout ownership or permissions are unsafe'
[[ -x "$SCRIPT_DIR/deploy-main.sh" && -x "$SCRIPT_DIR/check.sh" ]] || fail 'lima deployment scripts must be executable'
verify_legacy_deploy_access

[[ -n "$PUBLIC_KEY" && "$PUBLIC_KEY" != *$'\n'* ]] || fail 'TEAGRAM_DEMO_DEPLOY_PUBLIC_KEY must contain one public key line'
read -r KEY_TYPE KEY_DATA KEY_COMMENT <<< "$PUBLIC_KEY"
[[ "$KEY_TYPE" == ssh-ed25519 && "$KEY_DATA" =~ ^[A-Za-z0-9+/]+={0,2}$ ]] ||
  fail 'the deploy public key must be one Ed25519 key'
[[ -n "$KEY_COMMENT" ]] || KEY_COMMENT=teagram-demo-deploy
KEY_LINE="restrict,command=\"$ROOT_WRAPPER\" $KEY_TYPE $KEY_DATA $KEY_COMMENT"

[[ -f /etc/ssh/sshd_config && ! -L /etc/ssh/sshd_config ]] || fail 'the existing SSH config is unavailable'
SSHD_BIN=$(command -v sshd) || fail 'OpenSSH server is not installed'
[[ "$SSHD_BIN" == /* ]] || fail 'OpenSSH server path is not absolute'
"$SSHD_BIN" -T -f /etc/ssh/sshd_config > /dev/null || fail 'the existing SSH config does not pass sshd -T'
SSH_BASE_HASH=$(sha256sum /etc/ssh/sshd_config | awk '{print $1}')
SSH_DROPIN_HASH=$(directory_hash /etc/ssh/sshd_config.d)
SSH_BASE_EFFECTIVE="$TMP_DIR/sshd-main.before"
"$SSHD_BIN" -T -f /etc/ssh/sshd_config > "$SSH_BASE_EFFECTIVE" || fail 'could not capture the current SSH port 22 configuration'

install -d -o root -g root -m 0755 /usr/local/sbin
for path in /usr/local /usr/local/sbin; do
  require_root_owned "$path"
done

stage_deploy_entry
activate_deploy_entry
remove_legacy_deploy_access

printf 'deploy_user=%s listener=%s:%s key_file=%s port22_config=unchanged\n' \
  "$DEPLOY_USER" "$ADDRESS" "$PORT" "$AUTHORIZED_KEYS"
