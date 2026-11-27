#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=.github/scripts/lima-memory-process-snapshot.sh
source "$script_dir/lima-memory-process-snapshot.sh"

LIMA_SERVICE_IMAGE=${LIMA_SERVICE_IMAGE:-teagramd:ci}
CONTAINER=teagramd-lima-memory
KEY_VOLUME=tg-lima-memory-key
BLOB_VOLUME=tg-lima-memory-blobs
EXPECTED_LIMIT=$((46 * 1024 * 1024))
MIN_HEADROOM=$((2 * 1024 * 1024))
MEMORY_SAMPLE_INTERVAL_MS=10
RUNTIME_SAMPLE_INTERVAL_BYTES=$((512 * 1024))
LIMA_GOMEMLIMIT=20MiB
LIMA_MEMORY_PROBE_ATTEMPTS=${LIMA_MEMORY_PROBE_ATTEMPTS:-3}
MEMORY_CGROUP_DIR=
MEMORY_CURRENT_FILE=
MEMORY_STAT_FILE=
MEMORY_PEAK_FILE=
MEMORY_EVENTS_FILE=
MEMORY_SAMPLE_PEAK_FILE=
MEMORY_SAMPLE_EVENTS_FILE=
MEMORY_SAMPLE_STAT_PEAK_FILE=
MEMORY_SAMPLE_CURRENT_FILE=
MEMORY_SAMPLE_ANON_FILE=
MEMORY_SAMPLE_FILE_FILE=
MEMORY_SAMPLE_KERNEL_FILE=
MEMORY_SAMPLE_PROCESS_FILE=
MEMORY_SAMPLE_RUNTIME_FILE=
MEMORY_SAMPLE_RUNTIME_PEAK_FILE=
MEMORY_SAMPLE_RUNTIME_READY_FILE=
MEMORY_SAMPLE_RUNTIME_STOP_FILE=
MEMORY_SAMPLE_ERROR_FILE=
MEMORY_SAMPLE_STOP_FILE=
MEMORY_SAMPLER_PID=
MEMORY_RUNTIME_SAMPLER_PID=
IMAGE_REVISION=
IMAGE_ID=
MEMORY_MAX_BYTES=unknown
MEMORY_METRICS_SOURCE=unavailable
PROBE_PHASE=server_startup
UPLOAD_OUTCOME=not_started

stop_memory_sampler() {
  if [[ -n $MEMORY_SAMPLER_PID ]]; then
    : >"$MEMORY_SAMPLE_STOP_FILE" 2>/dev/null || true
    wait "$MEMORY_SAMPLER_PID" 2>/dev/null || true
    MEMORY_SAMPLER_PID=
  fi
  if [[ -n $MEMORY_RUNTIME_SAMPLER_PID ]]; then
    : >"$MEMORY_SAMPLE_RUNTIME_STOP_FILE" 2>/dev/null || true
    wait "$MEMORY_RUNTIME_SAMPLER_PID" 2>/dev/null || true
    MEMORY_RUNTIME_SAMPLER_PID=
  fi
}

finish() {
  local status=$?
  trap - EXIT
  set +e

  stop_memory_sampler

  local runtime_sample=unavailable runtime_sample_current=unknown
  if [[ -r $MEMORY_SAMPLE_RUNTIME_FILE && -r $MEMORY_SAMPLE_RUNTIME_PEAK_FILE ]]; then
    runtime_sample=$(<"$MEMORY_SAMPLE_RUNTIME_FILE")
    runtime_sample_current=$(<"$MEMORY_SAMPLE_RUNTIME_PEAK_FILE")
  fi
  if [[ $status -eq 0 && $runtime_sample == unavailable ]]; then
    status=1
    printf 'Lima probe completed without a Go runtime memory sample at a cgroup peak\n' >&2
  fi

  local peak_bytes='' events=''
  if [[ -r $MEMORY_PEAK_FILE && -r $MEMORY_EVENTS_FILE ]]; then
    peak_bytes=$(<"$MEMORY_PEAK_FILE")
    events=$(<"$MEMORY_EVENTS_FILE")
    MEMORY_METRICS_SOURCE=live_cgroup
  elif [[ -r $MEMORY_SAMPLE_PEAK_FILE && -r $MEMORY_SAMPLE_EVENTS_FILE ]]; then
    peak_bytes=$(<"$MEMORY_SAMPLE_PEAK_FILE")
    events=$(<"$MEMORY_SAMPLE_EVENTS_FILE")
    MEMORY_METRICS_SOURCE=last_sample
  fi

  if [[ -n $peak_bytes && -n $events ]]; then
    local peak_mib memory_peak_unattributed_bytes oom oom_kill max_events stat_sample_peak_bytes sample_current_bytes stat_anon_bytes stat_file_bytes stat_kernel_bytes
    local process_sample_peak_bytes server_process_pid server_process_rss_bytes server_process_anon_bytes photo_worker_process_count photo_worker_processes
    local process_attribution process_incomplete_pids
    peak_mib=$(awk -v bytes="$peak_bytes" 'BEGIN { printf "%.2f", bytes / 1048576 }')
    memory_peak_unattributed_bytes=unknown
    oom=$(awk '$1 == "oom" { print $2 }' <<<"$events")
    oom_kill=$(awk '$1 == "oom_kill" { print $2 }' <<<"$events")
    max_events=$(awk '$1 == "max" { print $2 }' <<<"$events")
    sample_current_bytes=unknown
    stat_sample_peak_bytes=unknown
    stat_anon_bytes=unknown
    stat_file_bytes=unknown
    stat_kernel_bytes=unknown
    process_sample_peak_bytes=unknown
    server_process_pid=unknown
    server_process_rss_bytes=unknown
    server_process_anon_bytes=unknown
    photo_worker_process_count=unknown
    photo_worker_processes=unknown
    process_attribution=unknown
    process_incomplete_pids=unknown
    if [[ -r $MEMORY_SAMPLE_STAT_PEAK_FILE && -r $MEMORY_SAMPLE_CURRENT_FILE &&
      -r $MEMORY_SAMPLE_ANON_FILE && -r $MEMORY_SAMPLE_FILE_FILE && -r $MEMORY_SAMPLE_KERNEL_FILE ]]; then
      stat_sample_peak_bytes=$(<"$MEMORY_SAMPLE_STAT_PEAK_FILE")
      sample_current_bytes=$(<"$MEMORY_SAMPLE_CURRENT_FILE")
      stat_anon_bytes=$(<"$MEMORY_SAMPLE_ANON_FILE")
      stat_file_bytes=$(<"$MEMORY_SAMPLE_FILE_FILE")
      stat_kernel_bytes=$(<"$MEMORY_SAMPLE_KERNEL_FILE")
    fi
    if [[ $peak_bytes =~ ^[0-9]+$ && $sample_current_bytes =~ ^[0-9]+$ ]]; then
      memory_peak_unattributed_bytes=$(lima_memory_peak_unattributed_bytes \
        "$peak_bytes" "$sample_current_bytes") || memory_peak_unattributed_bytes=unknown
    fi
    if [[ -r $MEMORY_SAMPLE_PROCESS_FILE ]]; then
      IFS='|' read -r process_sample_peak_bytes server_process_pid server_process_rss_bytes server_process_anon_bytes photo_worker_process_count photo_worker_processes process_attribution process_incomplete_pids \
        <"$MEMORY_SAMPLE_PROCESS_FILE"
    fi
    printf 'LIMA_MEMORY_PROBE result=%s attempt=%s/%s phase=%s uploads=%s image=%s image_id=%s revision=%s gomemlimit=%s websocket_listener=127.0.0.1:2446 websocket_origin=https://web.teagram.io websocket_browser_only=false peak_mib=%s peak_bytes=%s memory_peak_unattributed_bytes=%s memory_stat_sample_interval_ms=%s memory_stat_sample_peak_bytes=%s memory_stat_sample_current_bytes=%s memory_stat_anon_bytes=%s memory_stat_file_bytes=%s memory_stat_kernel_bytes=%s memory_max_bytes=%s metrics_source=%s max_events=%s oom=%s oom_kill=%s memory_process_sample_peak_bytes=%s memory_process_attribution=%s memory_process_incomplete_pids=%s server_process_pid=%s server_process_rss_bytes=%s server_process_anon_bytes=%s photo_worker_process_count=%s photo_worker_processes=%s\n' \
      "$([[ $status -eq 0 ]] && printf passed || printf failed)" "${LIMA_MEMORY_PROBE_ATTEMPT:-1}" "$LIMA_MEMORY_PROBE_ATTEMPTS" "$PROBE_PHASE" "$UPLOAD_OUTCOME" \
      "$LIMA_SERVICE_IMAGE" "${IMAGE_ID:-unknown}" "$IMAGE_REVISION" "$LIMA_GOMEMLIMIT" "$peak_mib" "$peak_bytes" "$memory_peak_unattributed_bytes" "$MEMORY_SAMPLE_INTERVAL_MS" "$stat_sample_peak_bytes" "$sample_current_bytes" "$stat_anon_bytes" "$stat_file_bytes" "$stat_kernel_bytes" "$MEMORY_MAX_BYTES" "$MEMORY_METRICS_SOURCE" \
      "${max_events:-unknown}" "${oom:-unknown}" "${oom_kill:-unknown}" "$process_sample_peak_bytes" "$process_attribution" "$process_incomplete_pids" "$server_process_pid" "$server_process_rss_bytes" "$server_process_anon_bytes" "$photo_worker_process_count" "$photo_worker_processes"
    printf 'LIMA_GO_RUNTIME_MEMORY sample_current_bytes=%s metrics=%s\n' "$runtime_sample_current" "$runtime_sample"
    if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
      {
        printf '\n### Lima service memory probe\n\n'
        printf '| Attempt | Result | Phase | Uploads | Image | Image ID | Revision | GOMEMLIMIT | WebSocket settings | Peak cgroup memory | Peak gap above sampled current (unattributed) | Stat sample interval | Peak at stat sample | Sampled current | memory.stat anon | memory.stat file | memory.stat kernel | memory.max | Metrics source | max events | OOM events | OOM kills | Process sample peak | Process attribution | Incomplete process PIDs | Server PID | Server RSS | Server anon | Photo worker count | Photo worker processes (PID:RSS:anon bytes) |\n'
        printf '| ---: | --- | --- | --- | --- | --- | --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- | ---: | ---: | ---: | ---: | --- | --- | ---: | --- | ---: | --- | --- |\n'
        printf '| %s/%s | %s | %s | %s | %s | %s | %s | %s | 127.0.0.1:2446, https://web.teagram.io, browser_only=false | %s MiB (%s bytes) | %s bytes | %s ms | %s bytes | %s bytes | %s bytes | %s bytes | %s bytes | %s bytes | %s | %s | %s | %s | %s bytes | %s | %s | %s | %s bytes | %s bytes | %s | %s |\n' \
          "${LIMA_MEMORY_PROBE_ATTEMPT:-1}" "$LIMA_MEMORY_PROBE_ATTEMPTS" "$([[ $status -eq 0 ]] && printf passed || printf failed)" "$PROBE_PHASE" "$UPLOAD_OUTCOME" \
          "$LIMA_SERVICE_IMAGE" "${IMAGE_ID:-unknown}" "$IMAGE_REVISION" "$LIMA_GOMEMLIMIT" "$peak_mib" "$peak_bytes" "$memory_peak_unattributed_bytes" "$MEMORY_SAMPLE_INTERVAL_MS" "$stat_sample_peak_bytes" "$sample_current_bytes" "$stat_anon_bytes" "$stat_file_bytes" "$stat_kernel_bytes" "$MEMORY_MAX_BYTES" \
          "$MEMORY_METRICS_SOURCE" "${max_events:-unknown}" "${oom:-unknown}" "${oom_kill:-unknown}" "$process_sample_peak_bytes" "$process_attribution" "$process_incomplete_pids" "$server_process_pid" "$server_process_rss_bytes" "$server_process_anon_bytes" "$photo_worker_process_count" "$photo_worker_processes"
        printf '\nGo runtime memory sample at sampled current peak (%s bytes):\n\n%s\n' \
          "$runtime_sample_current" "$runtime_sample"
      } >>"$GITHUB_STEP_SUMMARY"
    fi
  fi

  if [[ $status -ne 0 ]]; then
    docker inspect --format 'LIMA_MEMORY_CONTAINER state={{.State.Status}} exit_code={{.State.ExitCode}} oom_killed={{.State.OOMKilled}} error={{.State.Error}}' \
      "$CONTAINER" 2>&1 || true
    docker logs "$CONTAINER" 2>&1 | tail -80
  fi
  docker rm -f "$CONTAINER" >/dev/null 2>&1
  docker volume rm "$KEY_VOLUME" "$BLOB_VOLUME" >/dev/null 2>&1
  exit "$status"
}
trap finish EXIT

if [[ $(uname -m) != x86_64 ]]; then
  echo 'Lima service memory probe requires an x86_64 runner' >&2
  exit 1
fi
# Each repeated probe uses the same runner temp directory. Keep the generator's
# exclusive-create guard, but remove this attempt's previous photo fixture.
rm -f "$RUNNER_TEMP/lima-memory-start-gate" "$RUNNER_TEMP/lima-memory-photo.jpg" \
  "$RUNNER_TEMP/lima-memory-sampled-peak" "$RUNNER_TEMP/lima-memory-sampled-events" \
  "$RUNNER_TEMP/lima-memory-sampled-stat-peak" "$RUNNER_TEMP/lima-memory-sampled-stat-peak.tmp" \
  "$RUNNER_TEMP/lima-memory-sampled-current" "$RUNNER_TEMP/lima-memory-sampled-anon" \
  "$RUNNER_TEMP/lima-memory-sampled-file" "$RUNNER_TEMP/lima-memory-sampled-kernel" \
  "$RUNNER_TEMP/lima-memory-sampled-processes" "$RUNNER_TEMP/lima-memory-sampled-error" \
  "$RUNNER_TEMP/lima-memory-sampled-stop" "$RUNNER_TEMP/lima-memory-sampled-runtime" \
  "$RUNNER_TEMP/lima-memory-sampled-runtime-peak" "$RUNNER_TEMP/lima-memory-sampled-runtime-ready" \
  "$RUNNER_TEMP/lima-memory-sampled-runtime-stop" \
  "$RUNNER_TEMP/lima-memory-sampled-runtime.tmp"
if ! docker image inspect "$LIMA_SERVICE_IMAGE" >/dev/null 2>&1; then
  printf 'Lima service image is not present in the local daemon: %s\n' "$LIMA_SERVICE_IMAGE" >&2
  exit 1
fi
IMAGE_ID=$(docker image inspect --format '{{.Id}}' "$LIMA_SERVICE_IMAGE")
image_platform=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$IMAGE_ID")
if [[ $image_platform != linux/amd64 ]]; then
  printf 'Lima service platform is %s, want linux/amd64\n' "$image_platform" >&2
  exit 1
fi
IMAGE_REVISION=$(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' "$IMAGE_ID")
test -n "$IMAGE_REVISION"
if [[ -z ${TEAGRAMD_SOURCE_REVISION:-} ]]; then
  echo 'TEAGRAMD_SOURCE_REVISION is required to bind the probe to the workflow commit' >&2
  exit 1
fi
if [[ $IMAGE_REVISION != "$TEAGRAMD_SOURCE_REVISION" ]]; then
  printf 'Lima service image revision is %s, want %s\n' "$IMAGE_REVISION" "$TEAGRAMD_SOURCE_REVISION" >&2
  exit 1
fi
docker volume create "$KEY_VOLUME" >/dev/null
docker volume create "$BLOB_VOLUME" >/dev/null
docker run --rm --volume "$KEY_VOLUME:/var/lib/teagramd" "$IMAGE_ID" bootstrap-identity >/dev/null

# This disposable CI server is loopback-only, has registration closed, and its volumes are
# removed on exit. Keep auth budgets above the twenty test sign-ins from the loopback client.
docker run --detach --name "$CONTAINER" \
  --network host \
  --memory 46m --memory-swap 46m --cpus 0.5 --pids-limit 128 \
  --read-only \
  --volume "$KEY_VOLUME:/var/lib/teagramd" \
  --volume "$BLOB_VOLUME:/var/lib/teagramd-blobs" \
  --env TG_POSTGRES_DSN --env TG_AUTHKEY_ENC_KEY \
  --env TG_PUBLIC_LINK_PREFIX=https://links.example.test/ \
  --env TG_ADVERTISE_ADDR=127.0.0.1:2443 --env TG_LISTEN_ADDR=127.0.0.1:2443 \
  --env TG_WEBSOCKET_LISTEN_ADDR=127.0.0.1:2446 \
  --env TG_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io \
  --env TG_WEBSOCKET_BROWSER_ONLY=false \
  --env TG_LIMA_MEMORY_DIAGNOSTICS=1 \
  --env TG_REGISTRATION=closed --env TG_PG_MAX_CONNS=3 \
  --env TG_RATE_LIMIT_SEND_CODE_IP=40 --env TG_RATE_LIMIT_SEND_CODE_IP_PHONES=40 \
  --env TG_RATE_LIMIT_CHECK_PASSWORD_IP=40 --env TG_RATE_LIMIT_GET_PASSWORD_IP=40 \
  --env GOMAXPROCS=1 --env "GOMEMLIMIT=$LIMA_GOMEMLIMIT" \
  "$IMAGE_ID" >/dev/null

# Setup CLI processes do not belong in the server's measured 46 MiB cgroup.
run_setup_command() {
  local -a docker_flags=(
    --rm
    --read-only
    --network host
    --volume "$KEY_VOLUME:/var/lib/teagramd"
    --volume "$BLOB_VOLUME:/var/lib/teagramd-blobs"
    --env TG_POSTGRES_DSN
    --env TG_AUTHKEY_ENC_KEY
    --env TG_PUBLIC_LINK_PREFIX=https://links.example.test/
    --env TG_ADVERTISE_ADDR=127.0.0.1:2443
    --env TG_LISTEN_ADDR=127.0.0.1:2443
    --env TG_WEBSOCKET_LISTEN_ADDR=127.0.0.1:2446
    --env TG_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io
    --env TG_WEBSOCKET_BROWSER_ONLY=false
    --env TG_REGISTRATION=closed
    --env TG_PG_MAX_CONNS=3
  )
  if [[ ${1:-} == --interactive ]]; then
    docker_flags+=(--interactive --user 0)
    shift
  fi
  docker run "${docker_flags[@]}" "$IMAGE_ID" "$@"
}

container_pid=$(docker inspect --format '{{.State.Pid}}' "$CONTAINER")
if [[ $container_pid =~ ^[1-9][0-9]*$ ]]; then
  cgroup_path=$(awk -F: '$1 == "0" { print $3 }' "/proc/$container_pid/cgroup")
  if [[ -n $cgroup_path ]]; then
    MEMORY_CGROUP_DIR="/sys/fs/cgroup$cgroup_path"
    MEMORY_CURRENT_FILE="$MEMORY_CGROUP_DIR/memory.current"
    MEMORY_STAT_FILE="$MEMORY_CGROUP_DIR/memory.stat"
    MEMORY_PEAK_FILE="$MEMORY_CGROUP_DIR/memory.peak"
    MEMORY_EVENTS_FILE="$MEMORY_CGROUP_DIR/memory.events"
    MEMORY_SAMPLE_PEAK_FILE="$RUNNER_TEMP/lima-memory-sampled-peak"
    MEMORY_SAMPLE_EVENTS_FILE="$RUNNER_TEMP/lima-memory-sampled-events"
    MEMORY_SAMPLE_STAT_PEAK_FILE="$RUNNER_TEMP/lima-memory-sampled-stat-peak"
    MEMORY_SAMPLE_CURRENT_FILE="$RUNNER_TEMP/lima-memory-sampled-current"
    MEMORY_SAMPLE_ANON_FILE="$RUNNER_TEMP/lima-memory-sampled-anon"
    MEMORY_SAMPLE_FILE_FILE="$RUNNER_TEMP/lima-memory-sampled-file"
    MEMORY_SAMPLE_KERNEL_FILE="$RUNNER_TEMP/lima-memory-sampled-kernel"
    MEMORY_SAMPLE_PROCESS_FILE="$RUNNER_TEMP/lima-memory-sampled-processes"
    MEMORY_SAMPLE_RUNTIME_FILE="$RUNNER_TEMP/lima-memory-sampled-runtime"
    MEMORY_SAMPLE_RUNTIME_PEAK_FILE="$RUNNER_TEMP/lima-memory-sampled-runtime-peak"
    MEMORY_SAMPLE_RUNTIME_READY_FILE="$RUNNER_TEMP/lima-memory-sampled-runtime-ready"
    MEMORY_SAMPLE_RUNTIME_STOP_FILE="$RUNNER_TEMP/lima-memory-sampled-runtime-stop"
    MEMORY_SAMPLE_ERROR_FILE="$RUNNER_TEMP/lima-memory-sampled-error"
    MEMORY_SAMPLE_STOP_FILE="$RUNNER_TEMP/lima-memory-sampled-stop"
    if [[ -r $MEMORY_PEAK_FILE && -r $MEMORY_EVENTS_FILE && -r $MEMORY_CURRENT_FILE && -r $MEMORY_STAT_FILE && -r $MEMORY_CGROUP_DIR/memory.max && -r $MEMORY_CGROUP_DIR/cgroup.procs ]]; then
      MEMORY_MAX_BYTES=$(<"$MEMORY_CGROUP_DIR/memory.max")
    else
      echo 'service container cgroup does not expose the required memory and process files' >&2
      exit 1
    fi

    lima_memory_sample_cgroup_loop "$MEMORY_CGROUP_DIR" /proc "$container_pid" \
      "$RUNNER_TEMP/lima-memory-sampled" "$MEMORY_SAMPLE_STOP_FILE" "$MEMORY_SAMPLE_ERROR_FILE" \
      0.01 &
    MEMORY_SAMPLER_PID=$!
    lima_memory_sample_runtime_metrics_loop "$MEMORY_SAMPLE_STAT_PEAK_FILE" "$RUNTIME_SAMPLE_INTERVAL_BYTES" \
      "$MEMORY_SAMPLE_RUNTIME_READY_FILE" "$MEMORY_SAMPLE_RUNTIME_FILE" \
      "$MEMORY_SAMPLE_RUNTIME_PEAK_FILE" "$MEMORY_SAMPLE_ERROR_FILE" "$MEMORY_SAMPLE_RUNTIME_STOP_FILE" \
      0.01 &
    MEMORY_RUNTIME_SAMPLER_PID=$!
  else
    echo 'runner does not expose the service container cgroup v2 path' >&2
    exit 1
  fi
else
  echo "service container exited before measurement (pid=$container_pid)" >&2
  exit 1
fi

for attempt in $(seq 30); do
  if docker logs "$CONTAINER" 2>&1 | grep -q 'msg=listening'; then
    break
  fi
  if [[ $(docker inspect --format '{{.State.Running}}' "$CONTAINER") != true ]]; then
    echo 'pinned Lima service container exited before reaching msg=listening' >&2
    exit 1
  fi
  if [[ $attempt -eq 30 ]]; then
    echo 'pinned Lima service image did not reach msg=listening' >&2
    exit 1
  fi
  sleep 1
done

memory_limit=$(<"$MEMORY_CGROUP_DIR/memory.max")
if [[ $memory_limit != "$EXPECTED_LIMIT" ]]; then
  printf 'service memory.max is %s bytes, want %s\n' "$memory_limit" "$EXPECTED_LIMIT" >&2
  exit 1
fi

memory_cap_log=$(docker logs "$CONTAINER" 2>&1 | awk '/msg="Lima Go memory cap applied"/ { line = $0 } END { if (line != "") print line }')
if [[ -z $memory_cap_log ]]; then
  echo 'Lima Go memory cap applied log is missing from the server container' >&2
  exit 1
fi
case "$memory_cap_log" in
  *"cgroup_memory_max_bytes=$EXPECTED_LIMIT"*"go_memory_limit_bytes=$((12 * 1024 * 1024))"*) ;;
  *)
    printf 'Lima Go memory cap log does not match the probe cgroup and 12 MiB cap: %s\n' "$memory_cap_log" >&2
    exit 1
    ;;
esac
printf '%s\n' "$memory_cap_log"

websocket_listener_ready=false
for attempt in $(seq 30); do
  if curl --noproxy '*' --connect-timeout 1 --max-time 2 --silent --output /dev/null \
    http://127.0.0.1:2446/apiws; then
    websocket_listener_ready=true
    break
  fi
  sleep 1
done
if [[ $websocket_listener_ready != true ]]; then
  echo 'Lima WebSocket listener did not accept an HTTP connection on 127.0.0.1:2446' >&2
  exit 1
fi
if ! curl --noproxy '*' --fail --connect-timeout 1 --max-time 2 --silent --show-error \
  --output /dev/null http://127.0.0.1:2447/debug/runtime-memory; then
  echo 'Lima runtime memory diagnostics did not accept a request on 127.0.0.1:2447' >&2
  exit 1
fi
touch "$MEMORY_SAMPLE_RUNTIME_READY_FILE"

run_setup_command client-config >"$RUNNER_TEMP/lima-memory-client.json"
chmod 600 "$RUNNER_TEMP/lima-memory-client.json"
go build -o "$RUNNER_TEMP/mtproto-smoke" ./cmd/mtproto-smoke
go build -o "$RUNNER_TEMP/generate-lima-memory-photo" ./.github/scripts/lima-profile-photo-fixture
(cd "$RUNNER_TEMP" && ./generate-lima-memory-photo)

password_file="$RUNNER_TEMP/lima-memory-password"
printf 'lima-memory-probe-only-password\n' >"$password_file"
chmod 600 "$password_file"
for suffix in a b; do
  # The repeated attempts share a PostgreSQL service, so each needs fresh users.
  username="limamem_${LIMA_MEMORY_PROBE_ATTEMPT:-1}_$suffix"
  username_file="$RUNNER_TEMP/lima-memory-username-$suffix"
  printf '%s\n' "$username" >"$username_file"
  chmod 600 "$username_file"
  run_setup_command --interactive admin create-user --username "$username" <"$password_file"
done

PROBE_PHASE=photo_client_setup
"$RUNNER_TEMP/mtproto-smoke" \
  --endpoint 127.0.0.1:2443 \
  --discovery-file "$RUNNER_TEMP/lima-memory-client.json" \
  --username-file "$RUNNER_TEMP/lima-memory-username-a" \
  --password-file "$password_file" \
  --profile-photo-upload-file "$RUNNER_TEMP/lima-memory-photo.jpg" \
  --profile-photo-start-gate-file "$RUNNER_TEMP/lima-memory-start-gate" \
  >"$RUNNER_TEMP/lima-memory-client-a.log" 2>&1 &
client_a_pid=$!

"$RUNNER_TEMP/mtproto-smoke" \
  --endpoint 127.0.0.1:2443 \
  --discovery-file "$RUNNER_TEMP/lima-memory-client.json" \
  --username-file "$RUNNER_TEMP/lima-memory-username-b" \
  --password-file "$password_file" \
  --media-photo-upload-file "$RUNNER_TEMP/lima-memory-photo.jpg" \
  --media-photo-require-thumbnails \
  --media-photo-start-gate-file "$RUNNER_TEMP/lima-memory-start-gate" \
  >"$RUNNER_TEMP/lima-memory-client-media.log" 2>&1 &
media_client_pid=$!

gate_ready=0
for attempt in $(seq 300); do
  if grep -q 'Profile photo upload ready' "$RUNNER_TEMP/lima-memory-client-a.log" 2>/dev/null &&
    grep -q 'Media photo upload ready' "$RUNNER_TEMP/lima-memory-client-media.log" 2>/dev/null; then
    gate_ready=1
    break
  fi
  if ! kill -0 "$client_a_pid" 2>/dev/null || ! kill -0 "$media_client_pid" 2>/dev/null; then
    cat "$RUNNER_TEMP/lima-memory-client-a.log" "$RUNNER_TEMP/lima-memory-client-media.log"
    echo 'a photo client exited before reaching the upload barrier' >&2
    exit 1
  fi
  sleep 0.1
done
if [[ $gate_ready -ne 1 ]]; then
  echo 'profile and media photo clients did not reach the upload barrier' >&2
  exit 1
fi
PROBE_PHASE=concurrent_profile_and_media_photo_uploads
touch "$RUNNER_TEMP/lima-memory-start-gate"

set +e
wait "$client_a_pid"
client_a_status=$?
wait "$media_client_pid"
media_client_status=$?
set -e
cat "$RUNNER_TEMP/lima-memory-client-a.log" "$RUNNER_TEMP/lima-memory-client-media.log"
if [[ $client_a_status -ne 0 || $media_client_status -ne 0 ]]; then
  UPLOAD_OUTCOME="failed(profile=$client_a_status,media_photo=$media_client_status)"
  printf 'concurrent profile and media photo uploads failed (client statuses %d, %d)\n' \
    "$client_a_status" "$media_client_status" >&2
  exit 1
fi
UPLOAD_OUTCOME=profile_passed,media_photo_passed

stop_memory_sampler
if [[ -r $MEMORY_SAMPLE_ERROR_FILE ]]; then
  cat "$MEMORY_SAMPLE_ERROR_FILE" >&2
  exit 1
fi
if [[ ! -r $MEMORY_SAMPLE_PROCESS_FILE || ! -r $MEMORY_SAMPLE_STAT_PEAK_FILE ]]; then
  echo 'memory probe did not capture process attribution at a sampled peak' >&2
  exit 1
fi
IFS='|' read -r process_sample_peak_bytes server_process_pid server_process_rss_bytes server_process_anon_bytes photo_worker_process_count photo_worker_processes process_attribution process_incomplete_pids \
  <"$MEMORY_SAMPLE_PROCESS_FILE"
stat_sample_peak_bytes=$(<"$MEMORY_SAMPLE_STAT_PEAK_FILE")
if [[ ! $process_sample_peak_bytes =~ ^[0-9]+$ ||
  ! $stat_sample_peak_bytes =~ ^[0-9]+$ ||
  $process_sample_peak_bytes != "$stat_sample_peak_bytes" ||
  ( $process_attribution != complete && $process_attribution != incomplete ) ||
  ( $process_attribution == complete && $process_incomplete_pids != - ) ||
  ( $process_attribution == incomplete && ( $process_incomplete_pids == - || $process_incomplete_pids == unknown ) ) ||
  ! $server_process_pid =~ ^[1-9][0-9]*$ ||
  ! $server_process_rss_bytes =~ ^[0-9]+$ ||
  ! $server_process_anon_bytes =~ ^[0-9]+$ ||
  ! $photo_worker_process_count =~ ^[0-9]+$ ||
  $photo_worker_processes == unknown ]]; then
  printf 'memory probe process attribution is incomplete or does not match the sampled peak: status=%s pids=%s\n' \
    "${process_attribution:-unknown}" "${process_incomplete_pids:-unknown}" >&2
  exit 1
fi
if ! lima_memory_process_attribution_check \
  "$LIMA_MEMORY_PROBE_ATTEMPTS" "$process_sample_peak_bytes" "$process_attribution" "$process_incomplete_pids"; then
  exit 1
fi

peak_bytes=$(<"$MEMORY_PEAK_FILE")
events=$(<"$MEMORY_EVENTS_FILE")
max_events=$(awk '$1 == "max" { print $2 }' <<<"$events")
oom=$(awk '$1 == "oom" { print $2 }' <<<"$events")
oom_kill=$(awk '$1 == "oom_kill" { print $2 }' <<<"$events")
if (( peak_bytes > EXPECTED_LIMIT - MIN_HEADROOM )); then
  printf 'memory probe did not retain 2 MiB headroom: peak=%s limit=%s max=%s oom=%s oom_kill=%s\n' \
    "$peak_bytes" "$EXPECTED_LIMIT" "${max_events:-unknown}" "${oom:-unknown}" "${oom_kill:-unknown}" >&2
  exit 1
fi
if [[ ${max_events:-0} != 0 || ${oom:-0} != 0 || ${oom_kill:-0} != 0 ]]; then
  printf 'memory probe recorded cgroup pressure: peak=%s max=%s oom=%s oom_kill=%s\n' \
    "$peak_bytes" "${max_events:-unknown}" "${oom:-unknown}" "${oom_kill:-unknown}" >&2
  exit 1
fi
