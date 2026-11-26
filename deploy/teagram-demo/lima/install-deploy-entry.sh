#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly REPO=/opt/teagram-demo
readonly SCRIPT_DIR="$REPO/deploy/teagram-demo/lima"
readonly DEPLOY_USER=teagram-deploy
readonly SSH_CONFIG=/etc/ssh/teagram-demo-deploy_sshd_config
readonly AUTHORIZED_KEYS_DIR=/etc/ssh/authorized_keys
readonly AUTHORIZED_KEYS="$AUTHORIZED_KEYS_DIR/$DEPLOY_USER"
readonly ENTRY_PATH=/usr/local/libexec/teagram-demo-deploy-entry
readonly ROOT_WRAPPER=/usr/local/sbin/teagram-demo-deploy
readonly SUDOERS_FILE=/etc/sudoers.d/teagram-deploy
readonly SERVICE_NAME=teagram-demo-deploy-sshd
readonly SERVICE_FILE=/etc/init.d/teagram-demo-deploy-sshd
readonly ADDRESS=100.117.107.78
readonly PORT=2223
readonly PUBLIC_KEY=${TEAGRAM_DEMO_DEPLOY_PUBLIC_KEY:-}

TMP_DIR=
SSH_BASE_HASH=
SSH_DROPIN_HASH=
SSH_BASE_EFFECTIVE=

fail() {
  printf 'lima deploy entry install: %s\n' "$1" >&2
  exit 1
}

cleanup() {
  local rc=$?
  trap - EXIT HUP INT TERM
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
  install -d -o root -g root -m 0755 "$AUTHORIZED_KEYS_DIR"
  [[ ! -L "$AUTHORIZED_KEYS_DIR" ]] || fail 'authorized keys directory must not be a symlink'
  if [[ -e "$AUTHORIZED_KEYS" || -L "$AUTHORIZED_KEYS" ]]; then
    [[ -f "$AUTHORIZED_KEYS" && ! -L "$AUTHORIZED_KEYS" ]] || fail 'deploy authorized keys file is unsafe'
    if [[ "$(stat -c '%u:%a' "$AUTHORIZED_KEYS")" == '0:644' && "$(<"$AUTHORIZED_KEYS")" == "$content" ]]; then
      return 0
    fi
    fail 'deploy authorized keys file differs from the requested deploy key'
  fi
  temporary=$(mktemp "$AUTHORIZED_KEYS_DIR/.${DEPLOY_USER}.XXXXXXXX") || fail 'could not stage deploy authorized keys'
  if ! printf '%s\n' "$content" > "$temporary" || ! chown root:root "$temporary" || ! chmod 0644 "$temporary" ||
    ! mv -f -- "$temporary" "$AUTHORIZED_KEYS"; then
    rm -f -- "$temporary"
    fail 'could not install deploy authorized keys'
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
for executable in \
  /opt "$REPO" "$REPO/.git" "$REPO/deploy/teagram-demo/forced-command.sh" \
  "$SCRIPT_DIR/command-contract.sh" "$SCRIPT_DIR/deploy-root-wrapper.sh" \
  "$SCRIPT_DIR/deploy-entry.sh" "$SCRIPT_DIR/deploy-main.sh" "$SCRIPT_DIR/check.sh" \
  "$REPO/deploy/teagram-demo/demo-common.sh" "$REPO/deploy/teagram-demo/image-retention.sh"; do
  require_root_owned "$executable"
done
unsafe_checkout_path=
while IFS= read -r -d '' path; do
  path_state=$(stat -c '%u:%a' "$path") || fail "could not inspect checkout path: $path"
  path_owner=${path_state%%:*}
  path_mode=${path_state#*:}
  if [[ "$path_owner" != 0 ]] || (( (8#$path_mode & 0022) != 0 )); then
    unsafe_checkout_path=$path
    break
  fi
done < <(find "$REPO" -xdev -print0)
[[ -z "$unsafe_checkout_path" ]] || fail "checkout contains a non-root-owned or writable path: $unsafe_checkout_path"
[[ -x "$SCRIPT_DIR/deploy-main.sh" && -x "$SCRIPT_DIR/check.sh" ]] || fail 'lima deployment scripts must be executable'

[[ -n "$PUBLIC_KEY" && "$PUBLIC_KEY" != *$'\n'* ]] || fail 'TEAGRAM_DEMO_DEPLOY_PUBLIC_KEY must contain one public key line'
read -r KEY_TYPE KEY_DATA KEY_COMMENT <<< "$PUBLIC_KEY"
[[ "$KEY_TYPE" == ssh-ed25519 && "$KEY_DATA" =~ ^[A-Za-z0-9+/]+={0,2}$ ]] ||
  fail 'the deploy public key must be one Ed25519 key'
[[ -n "$KEY_COMMENT" ]] || KEY_COMMENT=teagram-demo-deploy
KEY_LINE="restrict,command=\"$ENTRY_PATH\" $KEY_TYPE $KEY_DATA $KEY_COMMENT"

[[ -f /etc/ssh/sshd_config && ! -L /etc/ssh/sshd_config ]] || fail 'the existing SSH config is unavailable'
command -v sshd >/dev/null 2>&1 || fail 'OpenSSH server is not installed'
sshd -T -f /etc/ssh/sshd_config > /dev/null || fail 'the existing SSH config does not pass sshd -T'
SSH_BASE_HASH=$(sha256sum /etc/ssh/sshd_config | awk '{print $1}')
SSH_DROPIN_HASH=$(directory_hash /etc/ssh/sshd_config.d)
TMP_DIR=$(mktemp -d /tmp/teagram-demo-deploy-entry.XXXXXXXX) || fail 'could not create installer verification directory'
SSH_BASE_EFFECTIVE="$TMP_DIR/sshd-main.before"
sshd -T -f /etc/ssh/sshd_config > "$SSH_BASE_EFFECTIVE" || fail 'could not capture the current SSH port 22 configuration'

command -v apk >/dev/null 2>&1 || fail 'Alpine apk is required to install sudo when missing'
if ! command -v sudo >/dev/null 2>&1; then
  apk add --no-cache sudo || fail 'could not install sudo for the restricted deploy user'
fi
command -v visudo >/dev/null 2>&1 || fail 'visudo is required to validate sudoers'
[[ -r /etc/sudoers && $(grep -Ec '^[[:space:]]*(@includedir|#includedir)[[:space:]]+/etc/sudoers.d([[:space:]]|$)' /etc/sudoers) -eq 1 ]] ||
  fail '/etc/sudoers must include /etc/sudoers.d exactly once'

if ! awk -F: -v name="$DEPLOY_USER" '$1 == name { count++; home=$6; shell=$7 } END { exit !(count == 1 && home == "/var/empty" && shell == "/bin/sh") }' /etc/passwd; then
  if awk -F: -v name="$DEPLOY_USER" '$1 == name { found=1 } END { exit !found }' /etc/passwd; then
    fail 'existing deploy account must use /var/empty and /bin/sh'
  fi
  install -d -o root -g root -m 0755 /var/empty
  adduser -S -D -H -h /var/empty -s /bin/sh "$DEPLOY_USER" || fail 'could not create the deploy account'
fi
[[ "$(id -u "$DEPLOY_USER")" != 0 ]] || fail 'deploy account must not be root'
groups=$(id -nG "$DEPLOY_USER") || fail 'could not inspect deploy account groups'
read -r -a account_groups <<< "$groups"
for group in "${account_groups[@]}"; do
  case "$group" in docker|wheel|sudo) fail "deploy account must not belong to $group" ;; esac
done

install -d -o root -g root -m 0755 /usr/local/libexec /usr/local/sbin /etc/sudoers.d
for path in /usr/local /usr/local/libexec /usr/local/sbin /etc/sudoers.d; do
  require_root_owned "$path"
done
if [[ -d /etc/sudoers.d ]]; then
  while IFS= read -r file; do
    [[ "$file" == "$SUDOERS_FILE" ]] && continue
    if grep -Eq '^[[:space:]]*teagram-deploy[[:space:]]' "$file"; then
      fail "another sudoers rule already grants access to $DEPLOY_USER"
    fi
  done < <(find /etc/sudoers.d -type f -print)
fi
if grep -Eq '^[[:space:]]*teagram-deploy[[:space:]]' /etc/sudoers; then
  fail 'another sudoers rule already grants access to teagram-deploy'
fi

if ! command -v sudo >/dev/null 2>&1; then
  fail 'sudo is not available after package installation'
fi
if [[ -f "$SUDOERS_FILE" && ! -L "$SUDOERS_FILE" ]]; then
  [[ "$(grep -Ev '^[[:space:]]*(#|$)' "$SUDOERS_FILE")" == 'teagram-deploy ALL=(root) NOPASSWD: /usr/local/sbin/teagram-demo-deploy' ]] ||
    fail 'existing deploy sudoers file contains an unexpected rule'
fi

install_file "$SCRIPT_DIR/sshd_config" "$SSH_CONFIG" 0600
install_file "$SCRIPT_DIR/teagram-demo-deploy-sshd.initd" "$SERVICE_FILE" 0755
install_file "$SCRIPT_DIR/deploy-entry.sh" "$ENTRY_PATH" 0755
install_file "$SCRIPT_DIR/deploy-root-wrapper.sh" "$ROOT_WRAPPER" 0755
install_file "$SCRIPT_DIR/teagram-deploy.sudoers" "$SUDOERS_FILE" 0440
install_key_file "$KEY_LINE"

visudo -c -f "$SUDOERS_FILE" >/dev/null || fail 'deploy sudoers rule failed validation'
visudo -c >/dev/null || fail 'complete sudoers configuration failed validation'
sudo -l -U "$DEPLOY_USER" > "$TMP_DIR/sudo-list" || fail 'deploy sudoers rule is not effective'
grep -Fq 'env_reset' "$TMP_DIR/sudo-list" || fail 'sudo env_reset must remain enabled'
! grep -Eq '!env_reset|SETENV' "$TMP_DIR/sudo-list" || fail 'deploy sudo rule must not disable env_reset or allow SETENV'
grep -Fq 'NOPASSWD: /usr/local/sbin/teagram-demo-deploy' "$TMP_DIR/sudo-list" ||
  fail 'deploy user must have exactly the root wrapper command'

sshd -t -f "$SSH_CONFIG" || fail 'dedicated deploy sshd config failed validation'
sshd -T -f "$SSH_CONFIG" > "$TMP_DIR/sshd-deploy.effective" || fail 'could not inspect dedicated deploy sshd config'
for expected in \
  'port 2223' 'addressfamily inet' 'listenaddress 100.117.107.78:2223' \
  'allowusers teagram-deploy' 'authenticationmethods publickey' \
  'passwordauthentication no' 'kbdinteractiveauthentication no' 'permitrootlogin no' \
  'disableforwarding yes' 'permittty no' 'permittunnel no' \
  'permituserenvironment no' 'permituserrc no' \
  'forcecommand /usr/local/libexec/teagram-demo-deploy-entry'; do
  grep -Fqx "$expected" "$TMP_DIR/sshd-deploy.effective" || fail "dedicated sshd config is missing: $expected"
done
[[ "$(grep -c '^listenaddress ' "$TMP_DIR/sshd-deploy.effective")" == 1 ]] ||
  fail 'dedicated sshd must listen on the tailnet address only'

rc-update add "$SERVICE_NAME" default || fail 'could not enable the dedicated SSH listener at boot'
if rc-service "$SERVICE_NAME" status >/dev/null 2>&1; then
  rc-service "$SERVICE_NAME" restart || fail 'could not restart the dedicated SSH listener'
else
  rc-service "$SERVICE_NAME" start || fail 'could not start the dedicated SSH listener'
fi

listener_ready=0
for _ in {1..10}; do
  if netstat -ltn 2>/dev/null | awk -v endpoint="$ADDRESS:$PORT" '$4 == endpoint { found=1 } END { exit !found }'; then
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
sshd -T -f /etc/ssh/sshd_config > "$TMP_DIR/sshd-main.after" || fail 'port 22 sshd config failed its post-install check'
cmp -s "$SSH_BASE_EFFECTIVE" "$TMP_DIR/sshd-main.after" || fail 'effective port 22 sshd config changed'

printf 'deploy_user=%s listener=%s:%s key_file=%s port22_config=unchanged\n' \
  "$DEPLOY_USER" "$ADDRESS" "$PORT" "$AUTHORIZED_KEYS"
