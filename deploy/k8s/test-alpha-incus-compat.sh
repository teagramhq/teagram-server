#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"

for script in deploy/k8s/bootstrap-alpha.sh deploy/k8s/cutover-alpha.sh; do
  if grep -En '/dev/stdin|incus exec .*-- command -v' "$repo_root/$script"; then
    printf 'alpha host script still relies on an unsupported Incus command form: %s\n' \
      "$script" >&2
    exit 1
  fi
done
if grep -En 'mktemp.*XXXXXX\.[[:alpha:]]' \
  "$repo_root/deploy/k8s/bootstrap-alpha.sh" "$repo_root/deploy/k8s/cutover-alpha.sh"; then
  printf 'alpha host scripts place a suffix after BusyBox mktemp XXXXXX\n' >&2
  exit 1
fi
grep --fixed-strings --quiet 'alpha_install_remote_file' \
  "$repo_root/deploy/k8s/bootstrap-alpha.sh"
grep --fixed-strings --quiet 'alpha_install_remote_file' \
  "$repo_root/deploy/k8s/cutover-alpha.sh"
grep --fixed-strings --quiet 'alpha_ensure_sshd_runtime_dir' \
  "$repo_root/deploy/k8s/bootstrap-alpha.sh"
grep --fixed-strings --quiet 'alpha_for_each_line' \
  "$repo_root/deploy/k8s/cutover-alpha.sh"

readonly TEST_IMAGE=debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
container_output=$(docker run --rm -i \
  --volume "$repo_root/deploy/k8s/alpha-incus.sh:/alpha-incus.sh:ro" \
  --env DEBIAN_FRONTEND=noninteractive \
  "$TEST_IMAGE" bash -s <<'CONTAINER_TEST'
set -euo pipefail
apt-get update -qq
apt-get install -y -qq --no-install-recommends openssh-server busybox-static util-linux >/dev/null
ssh-keygen -A >/dev/null
source /alpha-incus.sh

# Model the target LXC's denied reopen: fd 0 remains usable, while opening the
# /dev/stdin pathname fails even for the LXC's mapped root user.
rm -f /dev/stdin
: > /dev/stdin
chmod 000 /dev/stdin

mkdir -p /tmp/busybox-bin
ln -s /usr/bin/busybox /tmp/busybox-bin/mktemp
PATH=/tmp/busybox-bin:$PATH
export PATH
if mktemp /tmp/alpha-invalid.XXXXXX.yaml >/dev/null 2>&1; then
  printf 'BusyBox mktemp unexpectedly accepted a suffix after XXXXXX\n' >&2
  exit 1
fi
temporary=$(alpha_mktemp_file /tmp alpha-mktemp-check)
[[ -f $temporary ]]
rm -f -- "$temporary"

test_instance=alpha-compat
incus() {
  [[ ${1:-} == exec ]] || return 2
  shift
  while [[ ${1:-} == --mode=* ]]; do shift; done
  shift
  [[ ${1:-} == -- ]] || return 2
  shift
  # setpriv removes DAC_OVERRIDE to reproduce the path access denial from an
  # unprivileged LXC. The command still receives the actual inherited stdin.
  setpriv --bounding-set=-dac_override "$@"
}

if printf 'unreadable-by-path' | incus exec "$test_instance" -- \
  install -D -o root -g root -m 0640 /dev/stdin /tmp/old-transfer; then
  printf 'opening /dev/stdin unexpectedly worked in the unprivileged LXC fixture\n' >&2
  exit 1
fi

printf 'transferred over inherited stdin' > /tmp/alpha-transfer-source
alpha_install_remote_file "$test_instance" /tmp/alpha-compat/config 0640 \
  < /tmp/alpha-transfer-source
[[ $(cat /tmp/alpha-compat/config) == 'transferred over inherited stdin' ]]
[[ $(stat -c '%a:%u:%g' /tmp/alpha-compat/config) == 640:0:0 ]]
chmod 0700 /tmp/alpha-compat
alpha_install_remote_file "$test_instance" /tmp/alpha-compat/config 0640 \
  < /tmp/alpha-transfer-source
[[ $(stat -c '%a:%u:%g' /tmp/alpha-compat) == 700:0:0 ]]
alpha_remote_file_matches "$test_instance" /tmp/alpha-compat/config \
  /tmp/alpha-transfer-source
printf 'different input' > /tmp/alpha-transfer-source
if alpha_remote_file_matches "$test_instance" /tmp/alpha-compat/config \
  /tmp/alpha-transfer-source; then
  printf 'remote file comparison accepted different stdin\n' >&2
  exit 1
fi
alpha_remote_command_exists "$test_instance" sh
if alpha_remote_command_exists "$test_instance" command-that-does-not-exist; then
  printf 'remote command check accepted an absent command\n' >&2
  exit 1
fi

# Show that an exec which reads stdin consumes the old loop's input, then prove
# the shared fd-3 iterator keeps all records available across those execs.
old_records=()
while IFS= read -r record; do
  old_records+=("$record")
  incus exec "$test_instance" -- sh -c 'cat >/dev/null'
done <<< $'one\ntwo\nthree'
[[ ${#old_records[@]} == 1 ]]
new_records=()
record_with_exec() {
  new_records+=("$1")
  incus exec "$test_instance" -- sh -c 'cat >/dev/null'
}
alpha_for_each_line $'one\ntwo\nthree' record_with_exec <<< 'consumed-by-exec'
[[ ${#new_records[@]} == 3 && ${new_records[0]} == one &&
  ${new_records[1]} == two && ${new_records[2]} == three ]]

if incus exec "$test_instance" -- sshd -t >/tmp/sshd-before.log 2>&1; then
  printf 'sshd -t unexpectedly passed without /run/sshd\n' >&2
  exit 1
fi
grep --fixed-strings --quiet 'Missing privilege separation directory: /run/sshd' \
  /tmp/sshd-before.log
alpha_ensure_sshd_runtime_dir "$test_instance"
[[ $(stat -c '%a:%u:%g' /run/sshd) == 755:0:0 ]]
incus exec "$test_instance" -- sshd -t
printf 'alpha Incus compatibility test passed\n'
CONTAINER_TEST
)
[[ $container_output == *'alpha Incus compatibility test passed'* ]]
