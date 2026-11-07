#!/usr/bin/env bash
set -euo pipefail

readonly SOURCE_NAMESPACE=teagram
readonly STATE_DIR=${INCUS_STATE_DIR:?INCUS_STATE_DIR is required for the disposable cutover test}
mkdir -p "$STATE_DIR/units"

resource_for_container() {
  case "$1" in
    teagram-postgres-1|postgres) printf 'statefulset/postgres\n' ;;
    teagram-teagramd-1|teagramd) printf 'statefulset/teagramd\n' ;;
    teagram-link-edge-selector-1|linkselector) printf 'deployment/linkselector\n' ;;
    teagram-linklanding-1|linklanding) printf 'deployment/linklanding\n' ;;
    teagram-web-cloudflared|cloudflared) printf 'deployment/cloudflared\n' ;;
    *) return 1 ;;
  esac
}

container_status() {
  local resource replicas
  resource=$(resource_for_container "$1")
  replicas=$(kubectl get "$resource" --namespace "$SOURCE_NAMESPACE" -o jsonpath='{.spec.replicas}')
  [[ $replicas =~ ^[0-9]+$ && $replicas -gt 0 ]] && printf 'running\n' || printf 'exited\n'
}

scale_container() {
  local action=$1 container=$2 resource replicas=1
  resource=$(resource_for_container "$container")
  [[ $action == stop ]] && replicas=0
  kubectl scale "$resource" --namespace "$SOURCE_NAMESPACE" --replicas="$replicas" >/dev/null
}

secret_archive() {
  local secret_name=$1 source_key=${2:-}
  kubectl get secret "$secret_name" --namespace "$SOURCE_NAMESPACE" --output json |
    python3 -c '
import base64
import io
import json
import sys
import tarfile

try:
    secret = json.load(sys.stdin)
    data = secret.get("data")
    if not isinstance(data, dict):
        raise ValueError("missing Secret data")
    source_key = sys.argv[1]
    if source_key:
        entries = {"teagramd-blob-secret-key": data[source_key]}
    else:
        entries = data
    with tarfile.open(fileobj=sys.stdout.buffer, mode="w|") as archive:
        for name, encoded in entries.items():
            content = base64.b64decode(encoded, validate=True)
            member = tarfile.TarInfo(name)
            member.size = len(content)
            member.mode = 0o600
            archive.addfile(member, io.BytesIO(content))
except (KeyError, TypeError, ValueError, json.JSONDecodeError):
    raise SystemExit("disposable cutover test could not read a fixture Secret")
' "$source_key"
}

docker_command() {
  local subcommand=$1 format container service path image
  shift
  local -a args=("$@")
  case "$subcommand" in
    inspect)
      format=$2
      container=${args[-1]}
      case "$format" in
        *'.State.Status'*)
          if [[ $container == teagram-web-cloudflared &&
            ${CUTOVER_FAIL_OLD_CONNECTOR_CHECK:-} == true &&
            -f $RUNNER_TEMP/teagram-cutover-runner/phase &&
            $(<"$RUNNER_TEMP/teagram-cutover-runner/phase") == route-switched &&
            ! -e $STATE_DIR/old-connector-check-failure-injected ]]; then
            : > "$STATE_DIR/old-connector-check-failure-injected"
            printf 'injected disposable-cluster failure after the Cloudflare connector started\n' >&2
            return 42
          fi
          container_status "$container"
          ;;
        *'com.docker.compose.service'*)
          case "$container" in
            teagramd) printf 'teagramd\n' ;;
            postgres) printf 'postgres\n' ;;
            linkselector) printf 'linkselector\n' ;;
            linklanding) printf 'linklanding\n' ;;
            *) return 1 ;;
          esac
          ;;
        *'.Name'*)
          case "$container" in
            teagramd) printf '/teagram-teagramd-1\n' ;;
            postgres) printf '/teagram-postgres-1\n' ;;
            linkselector) printf '/teagram-link-edge-selector-1\n' ;;
            linklanding) printf '/teagram-linklanding-1\n' ;;
            *) return 1 ;;
          esac
          ;;
        *'org.opencontainers.image.revision'*)
          docker inspect "${INCUS_TEST_SOURCE_CONTAINER:?}" --format "$format"
          ;;
        *'.Config.Env'*)
          printf 'TG_ADMIN_TOKEN_HASH=%064d\n' 0
          printf 'TG_ADMIN_ORIGIN=https://links.example.test\n'
          printf 'TG_BLOB_DIR=/var/lib/teagramd-blobs\n'
          printf 'TG_MEDIA_KEY_CURRENT=1\n'
          printf 'TG_BLOB_S3_ENDPOINT=http://rustfs:9000\n'
          printf 'TG_BLOB_S3_BUCKET=teagram\n'
          printf 'TG_BLOB_S3_PREFIX=teagramd/\n'
          printf 'TG_BLOB_S3_ACCESS_KEY_ID=0123456789abcdef0123\n'
          printf 'TG_BLOB_S3_SECRET_ACCESS_KEY_FILE=/run/secrets/teagramd-blob-secret-key\n'
          ;;
        *) return 1 ;;
      esac
      ;;
    ps)
      for service in teagramd postgres linkselector linklanding; do
        if [[ $(container_status "$service") == running ]]; then
          printf '%s\n' "$service"
        fi
      done
      ;;
    stop|start)
      container=${args[-1]}
      scale_container "$subcommand" "$container"
      ;;
    exec)
      container=$1
      shift
      case "$container" in
        teagram-postgres-1)
          kubectl exec --namespace "$SOURCE_NAMESPACE" postgres-0 -c postgres -- "$@"
          ;;
        teagram-teagramd-1)
          # Exercise the real distroless container, which has no test binary.
          kubectl exec --namespace "$SOURCE_NAMESPACE" teagramd-0 -- "$@"
          ;;
        *) return 1 ;;
      esac
      ;;
    cp)
      path=$1
      [[ $2 == - ]] || return 1
      path=${path#*:}
      case "$path" in
        /var/lib/teagramd-blobs/.)
          kubectl exec --namespace "$SOURCE_NAMESPACE" pod/cutover-key-reader -c reader -- \
            tar -cf - -C /var/lib/teagramd-blobs .
          ;;
        /run/teagramd/blob-mode/.)
          kubectl exec --namespace "$SOURCE_NAMESPACE" pod/cutover-key-reader -c reader -- \
            tar -cf - -C /run/teagramd/blob-mode .
          ;;
        /run/teagramd/media-keys/.)
          secret_archive teagramd-media-keys
          ;;
        /run/secrets/teagramd-blob-secret-key)
          secret_archive teagramd-blob-credentials TG_BLOB_S3_SECRET_ACCESS_KEY
          ;;
        *) docker cp "${INCUS_TEST_SOURCE_CONTAINER:?}:$path" - ;;
      esac
      ;;
    image)
      [[ ${args[0]} == inspect ]] || return 1
      image=${args[1]}
      format=${args[-1]}
      docker image inspect "$image" --format "$format"
      ;;
    *) return 1 ;;
  esac
}

tailscale_command() {
  local subcommand=$1 arg path='' target='' tcp_port='' proxy_protocol=''
  shift
  if [[ $subcommand == serve && ${1:-} == status && ${2:-} == --json ]]; then
    cat "$STATE_DIR/serve.json"
    return
  fi
  [[ $subcommand == serve ]] || return 1
  if [[ ${CUTOVER_FAIL_FIRST_SERVE_ROUTE:-} == true && ! -e $STATE_DIR/serve-route-failure-injected ]]; then
    : > "$STATE_DIR/serve-route-failure-injected"
    printf 'injected disposable-cluster failure before the first Serve route changed\n' >&2
    return 42
  fi
  for arg in "$@"; do
    case "$arg" in
      --set-path=*) path=${arg#*=} ;;
      --tcp=*) tcp_port=${arg#*=} ;;
      --proxy-protocol=*) proxy_protocol=${arg#*=} ;;
      http://*|tcp://*) target=$arg ;;
    esac
  done
  if [[ -n $tcp_port ]]; then
    python3 - "$STATE_DIR/serve.json" "$tcp_port" "$target" "$proxy_protocol" <<'PY'
import json
import pathlib
import sys

path, port, target, proxy_protocol = pathlib.Path(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4]
state = json.loads(path.read_text())
handler = {"TCPForward": target.removeprefix("tcp://")}
if proxy_protocol:
    handler["ProxyProtocol"] = int(proxy_protocol)
state.setdefault("TCP", {})[port] = handler
path.write_text(json.dumps(state))
PY
  elif [[ -n $path && -n $target ]]; then
    python3 - "$STATE_DIR/serve.json" "$path" "$target" <<'PY'
import json
import pathlib
import sys

file, route, target = pathlib.Path(sys.argv[1]), sys.argv[2], sys.argv[3]
state = json.loads(file.read_text())
web = state.setdefault("Web", {}).setdefault("alpha.example.ts.net:443", {"Handlers": {}})
web.setdefault("Handlers", {})[route] = {"Proxy": target}
file.write_text(json.dumps(state))
PY
    if [[ ${CUTOVER_FAIL_AFTER_FIRST_SERVE_ROUTE:-} == true &&
      ! -e $STATE_DIR/serve-route-failure-injected ]]; then
      : > "$STATE_DIR/serve-route-failure-injected"
      printf 'injected disposable-cluster failure after the first Serve route changed\n' >&2
      return 42
    fi
  else
    return 1
  fi
}

systemctl_command() {
  local subcommand=$1 unit unit_path command_line pid_file pid
  shift
  if [[ $subcommand == daemon-reload ]]; then
    [[ $# -eq 0 ]]
    return
  fi
  (( $# > 0 )) || return 1
  local -a args=("$@")
  unit=${args[-1]}
  unit_path="$STATE_DIR/units/$unit"
  pid_file="$STATE_DIR/units/$unit.pid"
  case "$subcommand" in
    cat) [[ -f $unit_path ]] && cat "$unit_path" ;;
    show)
      if [[ -f $unit_path ]]; then printf 'loaded\n'; else printf 'not-found\n'; fi
      ;;
    enable)
      command_line=$(sed -n 's/^ExecStart=//p' "$unit_path")
      [[ -n $command_line ]] || return 1
      local -a command=()
      read -r -a command <<< "$command_line"
      # Close the runner's lock descriptor (9) in the stand-in unit: a real
      # systemd unit does not inherit it, and a child that keeps it open would
      # hold the cutover lock after the runner exits.
      (sleep "${CUTOVER_FORWARD_DELAY_SECONDS:-0}"; exec kubectl "${command[@]:2}") >"$STATE_DIR/units/$unit.log" 2>&1 </dev/null 9>&- &
      pid=$!
      printf '%s\n' "$pid" > "$pid_file"
      return 0
      ;;
    disable)
      if [[ ${CUTOVER_FAIL_STOP_PORT_FORWARD:-} == true && ! -e $STATE_DIR/forward-stop-failure-injected ]]; then
        : > "$STATE_DIR/forward-stop-failure-injected"
        printf 'injected port-forward stop failure\n' >&2
        return 42
      fi
      [[ -f $pid_file ]] || return 0
      pid=$(<"$pid_file")
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
      rm -f -- "$pid_file"
      ;;
    *) return 1 ;;
  esac
}

case "${1:-}" in
  info)
    [[ ${2:-} == teagram || ${2:-} == teagram-k3s ]]
    ;;
  list)
    printf 'RUNNING\n'
    ;;
  exec)
    instance=$2
    shift 2
    [[ ${1:-} == -- ]] || exit 2
    shift
    command_name=$1
    shift
    args=("$@")
    if [[ $instance == teagram ]]; then
      case "$command_name" in
        command) exit 0 ;;
        docker) docker_command "$@" ;;
        tailscale) tailscale_command "$@" ;;
        busybox)
          [[ ${1:-} == wget ]] || exit 2
          [[ ${2:-} != --help ]] || exit 0
          shift 4
          curl -fsS "$@"
          ;;
        *) exit 2 ;;
      esac
    elif [[ $instance == teagram-k3s ]]; then
      case "$command_name" in
        command) exit 0 ;;
        systemctl) systemctl_command "$@" ;;
        ip)
          printf '2: eth0    inet 127.0.0.2/8 scope global eth0\n'
          ;;
        sh)
          if [[ ${2:-} == *'ip -o -4 addr show dev eth0'* ]]; then
            printf '127.0.0.2\n'
          else
            "$command_name" "$@"
          fi
          ;;
        install)
          destination=${args[-1]}
          cat > "$STATE_DIR/units/${destination##*/}"
          ;;
        curl) curl "$@" ;;
        *) exit 2 ;;
      esac
    else
      exit 2
    fi
    ;;
  *) exit 2 ;;
esac
