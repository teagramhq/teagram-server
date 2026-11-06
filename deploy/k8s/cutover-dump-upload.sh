#!/usr/bin/env bash
set -euo pipefail
umask 077

namespace=$1
pod=$2
source_file=$3
destination=$4
readonly receiver_port=29473
[[ -s $source_file ]] || { printf 'cutover dump is empty or missing\n' >&2; exit 1; }
tmp_dir=$(mktemp -d)
token=$(basename "$tmp_dir")
receiver_pid=''
forward_pid=''

cleanup() {
  local status=$? remote_pid=''
  trap - EXIT
  if [[ -n $receiver_pid && -s $tmp_dir/receiver.pid ]]; then
    read -r remote_pid < "$tmp_dir/receiver.pid"
    if [[ $remote_pid =~ ^[0-9]+$ ]]; then
      # shellcheck disable=SC2016 # The pod shell expands its positional arguments.
      if ! kubectl exec --namespace "$namespace" "$pod" -c postgres -- \
        sh -ec 'pid=$1; token=$2
          [ -r "/proc/$pid/cmdline" ] || exit 0
          command_line=$(tr "\000" " " < "/proc/$pid/cmdline")
          case "$command_line" in *"$token"*) kill -TERM "$pid" ;; esac' \
        -- "$remote_pid" "$token" >/dev/null 2>&1; then
        printf 'could not stop the PostgreSQL dump receiver\n' >&2
        status=1
      fi
    fi
  fi
  if [[ -n $forward_pid ]]; then
    kill "$forward_pid" 2>/dev/null || true
    wait "$forward_pid" 2>/dev/null || true
  fi
  if [[ -n $receiver_pid ]]; then
    kill "$receiver_pid" 2>/dev/null || true
    wait "$receiver_pid" 2>/dev/null || true
  fi
  if (( status != 0 )); then
    [[ ! -s $tmp_dir/receiver.log ]] || cat "$tmp_dir/receiver.log" >&2
    [[ ! -s $tmp_dir/forward.log ]] || cat "$tmp_dir/forward.log" >&2
  fi
  rm -rf -- "$tmp_dir"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# The receiver is reachable only through the pod's loopback interface. Its
# timeout also bounds its lifetime if the local process is killed abruptly.
# shellcheck disable=SC2016 # The pod shell expands its positional arguments.
kubectl exec --namespace "$namespace" "$pod" -c postgres -- \
  sh -ec 'destination=$1; port=$2; umask 077
    printf "%s\n" "$$"
    nc -v -w 300 -l -s 127.0.0.1 -p "$port" > "$destination" &
    child=$!
    trap '\''kill "$child" 2>/dev/null || :; wait "$child" 2>/dev/null || :; exit 1'\'' TERM HUP INT
    wait "$child"' \
  "$token" "$destination" "$receiver_port" > "$tmp_dir/receiver.pid" 2> "$tmp_dir/receiver.log" &
receiver_pid=$!
for ((attempt = 0; attempt < 300; attempt++)); do
  if grep -Fq "listening on 127.0.0.1:$receiver_port" "$tmp_dir/receiver.log"; then
    break
  fi
  kill -0 "$receiver_pid" 2>/dev/null || { printf 'PostgreSQL dump receiver failed to start\n' >&2; exit 1; }
  sleep 0.1
done
(( attempt < 300 )) || { printf 'PostgreSQL dump receiver did not become ready\n' >&2; exit 1; }

kubectl port-forward --address 127.0.0.1 --namespace "$namespace" "pod/$pod" \
  ":$receiver_port" > "$tmp_dir/forward.log" 2>&1 &
forward_pid=$!
local_port=''
for ((attempt = 0; attempt < 300; attempt++)); do
  local_port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9][0-9]*\) -> .*/\1/p' "$tmp_dir/forward.log")
  [[ -n $local_port ]] && break
  kill -0 "$forward_pid" 2>/dev/null || { printf 'PostgreSQL dump port-forward failed to start\n' >&2; exit 1; }
  sleep 0.1
done
[[ $local_port =~ ^[0-9]+$ ]] || { printf 'PostgreSQL dump port-forward did not become ready\n' >&2; exit 1; }

python3 - "$source_file" "$local_port" <<'PY'
import shutil
import socket
import sys

with open(sys.argv[1], "rb") as source, socket.create_connection(("127.0.0.1", int(sys.argv[2])), timeout=300) as connection:
    with connection.makefile("wb", buffering=0) as destination:
        shutil.copyfileobj(source, destination)
    connection.shutdown(socket.SHUT_WR)
    while connection.recv(65536):
        pass
PY
wait "$receiver_pid"
receiver_pid=''
