#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=.github/scripts/lima-memory-process-snapshot.sh
source "$script_dir/lima-memory-process-snapshot.sh"

LIMA_SERVICE_IMAGE=${LIMA_SERVICE_IMAGE:-teagramd:ci}
CONTAINER=teagramd-lima-memory
KEY_VOLUME=tg-lima-memory-key
BLOB_VOLUME=tg-lima-memory-blobs
EXPECTED_LIMIT=${LIMA_MEMORY_PROBE_CGROUP_LIMIT_BYTES:-$((46 * 1024 * 1024))}
if [[ ! $EXPECTED_LIMIT =~ ^[1-9][0-9]*$ ]]; then
  printf 'LIMA_MEMORY_PROBE_CGROUP_LIMIT_BYTES must be a positive integer, got %s\n' "$EXPECTED_LIMIT" >&2
  exit 2
fi
MIN_HEADROOM=$((2 * 1024 * 1024))
MEMORY_SAMPLE_INTERVAL_MS=10
RUNTIME_SAMPLE_INTERVAL_BYTES=$((512 * 1024))
LIMA_GOMEMLIMIT=20MiB
LIMA_GOMEMLIMIT_BYTES=$(( ${LIMA_GOMEMLIMIT%MiB} * 1024 * 1024 ))
LIMA_GO_MEMORY_LIMIT_CEILING_BYTES=$((14 * 1024 * 1024))
if (( EXPECTED_LIMIT > 64 * 1024 * 1024 )); then
  LIMA_EXPECTED_GO_MEMORY_LIMIT_BYTES=$LIMA_GOMEMLIMIT_BYTES
elif (( LIMA_GOMEMLIMIT_BYTES < LIMA_GO_MEMORY_LIMIT_CEILING_BYTES )); then
  LIMA_EXPECTED_GO_MEMORY_LIMIT_BYTES=$LIMA_GOMEMLIMIT_BYTES
else
  LIMA_EXPECTED_GO_MEMORY_LIMIT_BYTES=$LIMA_GO_MEMORY_LIMIT_CEILING_BYTES
fi
if (( EXPECTED_LIMIT % (1024 * 1024) != 0 )); then
  printf 'LIMA_MEMORY_PROBE_CGROUP_LIMIT_BYTES must be a positive whole MiB value, got %s\n' \
    "$EXPECTED_LIMIT" >&2
  exit 2
fi
DOCKER_MEMORY_LIMIT_MIB=$((EXPECTED_LIMIT / (1024 * 1024)))
LIMA_MEMORY_PROBE_ATTEMPTS=${LIMA_MEMORY_PROBE_ATTEMPTS:-3}
LIMA_MEMORY_PROBE_USERNAME_PREFIX=${LIMA_MEMORY_PROBE_USERNAME_PREFIX:-${LIMA_MEMORY_PROBE_ATTEMPT:-1}}
if [[ ! $LIMA_MEMORY_PROBE_USERNAME_PREFIX =~ ^[A-Za-z0-9_]{1,22}$ ]]; then
  printf 'LIMA_MEMORY_PROBE_USERNAME_PREFIX must contain 1-22 letters, digits, or underscores, got %s\n' \
    "$LIMA_MEMORY_PROBE_USERNAME_PREFIX" >&2
  exit 2
fi
lima_memory_configure_mapping_endpoint
MEMORY_CGROUP_DIR=
MEMORY_CURRENT_FILE=
MEMORY_STAT_FILE=
MEMORY_PEAK_FILE=
MEMORY_EVENTS_FILE=
MEMORY_SAMPLE_PEAK_FILE=
MEMORY_SAMPLE_EVENTS_FILE=
MEMORY_SAMPLE_STAT_PEAK_FILE=
MEMORY_SAMPLE_RUNTIME_FILE=
MEMORY_SAMPLE_RUNTIME_READY_FILE=
MEMORY_SAMPLE_RUNTIME_STOP_FILE=
MEMORY_SAMPLE_MAPPING_READY_FILE=
MEMORY_SAMPLE_MAPPING_STOP_FILE=
MEMORY_SAMPLE_ERROR_FILE=
MEMORY_SAMPLE_STOP_FILE=
MEMORY_SAMPLER_PID=
MEMORY_RUNTIME_SAMPLER_PID=
MEMORY_MAPPING_SAMPLER_PID=
IMAGE_REVISION=
IMAGE_ID=
MEMORY_MAX_BYTES=unknown
MEMORY_METRICS_SOURCE=unavailable
PROBE_PHASE=server_startup
UPLOAD_OUTCOME=not_started

stop_memory_sampler() {
  if [[ -n $MEMORY_RUNTIME_SAMPLER_PID ]]; then
    : >"$MEMORY_SAMPLE_RUNTIME_STOP_FILE" 2>/dev/null || true
    wait "$MEMORY_RUNTIME_SAMPLER_PID" 2>/dev/null || true
    MEMORY_RUNTIME_SAMPLER_PID=
  fi
  if [[ -n $MEMORY_SAMPLER_PID ]]; then
    : >"$MEMORY_SAMPLE_STOP_FILE" 2>/dev/null || true
    wait "$MEMORY_SAMPLER_PID" 2>/dev/null || true
    MEMORY_SAMPLER_PID=
  fi
  if [[ -n $MEMORY_MAPPING_SAMPLER_PID ]]; then
    : >"$MEMORY_SAMPLE_MAPPING_STOP_FILE" 2>/dev/null || true
    wait "$MEMORY_MAPPING_SAMPLER_PID" 2>/dev/null || true
    MEMORY_MAPPING_SAMPLER_PID=
  fi
}

finish() {
  local status=$?
  trap - EXIT
  set +e

  stop_memory_sampler

  local runtime_sample=unavailable runtime_sample_current=unknown runtime_gc_cpu_fraction=unknown
  local runtime_cgroup_sample_unix_nano=unknown runtime_sample_alignment_delta_ns=unknown runtime_cgroup_sample='' runtime_bundle=''
  local runtime_cgroup_anon_bytes=unknown runtime_cgroup_file_bytes=unknown runtime_cgroup_kernel_bytes=unknown
  local runtime_cgroup_peak_bytes=unknown runtime_timestamp
  local runtime_process_attribution=unmatched runtime_process_incomplete_pids=unknown
  local process_sample_unix_nano=unknown process_sample_peak_bytes=unknown process_sample_memory_peak_bytes=unknown
  local process_sample_anon_bytes=unknown process_sample_file_bytes=unknown process_sample_kernel_bytes=unknown
  local server_process_pid=unknown server_process_rss_bytes=unknown server_process_anon_bytes=unknown
  local photo_worker_process_count=unknown photo_worker_processes=unknown
  local process_attribution=unmatched process_incomplete_pids=unknown best_process_sample=''
  local mapping_label mapping_file
  if [[ -r $MEMORY_SAMPLE_RUNTIME_FILE ]]; then
    runtime_bundle=$(<"$MEMORY_SAMPLE_RUNTIME_FILE")
    if runtime_sample=$(jq -ec '.runtime | select(type == "object")' <<<"$runtime_bundle") &&
      runtime_cgroup_sample=$(jq -er '.cgroup_sample | select(type == "string")' <<<"$runtime_bundle"); then
    IFS='|' read -r runtime_cgroup_sample_unix_nano runtime_sample_current runtime_cgroup_anon_bytes \
      runtime_cgroup_file_bytes runtime_cgroup_kernel_bytes _ _ _ _ _ _ \
      runtime_process_attribution \
      runtime_process_incomplete_pids runtime_cgroup_peak_bytes \
      <<<"$runtime_cgroup_sample"
    if runtime_timestamp=$(jq -er '.sample_unix_nano | select(type == "number" and . > 0)' <<<"$runtime_sample") &&
      [[ $runtime_timestamp =~ ^[0-9]+$ && $runtime_cgroup_sample_unix_nano =~ ^[0-9]+$ ]]; then
      runtime_sample_alignment_delta_ns=$((runtime_cgroup_sample_unix_nano - runtime_timestamp))
      (( runtime_sample_alignment_delta_ns < 0 )) && runtime_sample_alignment_delta_ns=$((-runtime_sample_alignment_delta_ns))
    else
      runtime_cgroup_sample_unix_nano=unknown
      runtime_sample_alignment_delta_ns=unknown
    fi
    if ! runtime_gc_cpu_fraction=$(jq -er '.gc_cpu_fraction | select(type == "number" and . >= 0 and . <= 1)' <<<"$runtime_sample"); then
      status=1
      printf 'Go runtime memory sample is missing a valid cumulative GC CPU fraction\n' >&2
    fi
    else
      runtime_sample=unavailable
      status=1
      printf 'Go runtime sample bundle is missing its paired cgroup/process sample\n' >&2
    fi
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
    local peak_mib memory_peak_unattributed_bytes memory_peak_above_go_sample_bytes oom oom_kill max_events
    local stat_sample_peak_bytes sample_current_bytes stat_anon_bytes stat_file_bytes stat_kernel_bytes
    peak_mib=$(awk -v bytes="$peak_bytes" 'BEGIN { printf "%.2f", bytes / 1048576 }')
    memory_peak_unattributed_bytes=unknown
    memory_peak_above_go_sample_bytes=unknown
    oom=$(awk '$1 == "oom" { print $2 }' <<<"$events")
    oom_kill=$(awk '$1 == "oom_kill" { print $2 }' <<<"$events")
    max_events=$(awk '$1 == "max" { print $2 }' <<<"$events")
    sample_current_bytes=unknown
    stat_sample_peak_bytes=unknown
    stat_anon_bytes=unknown
    stat_file_bytes=unknown
    stat_kernel_bytes=unknown
    if [[ $runtime_sample_current =~ ^[0-9]+$ && $runtime_cgroup_peak_bytes =~ ^[0-9]+$ ]]; then
      stat_sample_peak_bytes=$runtime_cgroup_peak_bytes
      sample_current_bytes=$runtime_sample_current
      stat_anon_bytes=$runtime_cgroup_anon_bytes
      stat_file_bytes=$runtime_cgroup_file_bytes
      stat_kernel_bytes=$runtime_cgroup_kernel_bytes
    fi
    if [[ $peak_bytes =~ ^[0-9]+$ && $sample_current_bytes =~ ^[0-9]+$ ]]; then
      memory_peak_above_go_sample_bytes=$(lima_memory_peak_unattributed_bytes \
        "$peak_bytes" "$sample_current_bytes") || memory_peak_above_go_sample_bytes=unknown
    fi
    if best_process_sample=$(lima_memory_best_cgroup_sample "$RUNNER_TEMP/lima-memory-sampled-history") &&
      [[ -n $best_process_sample ]]; then
      IFS='|' read -r process_sample_unix_nano process_sample_peak_bytes process_sample_anon_bytes \
        process_sample_file_bytes process_sample_kernel_bytes _ server_process_pid server_process_rss_bytes \
        server_process_anon_bytes photo_worker_process_count photo_worker_processes process_attribution \
        process_incomplete_pids process_sample_memory_peak_bytes <<<"$best_process_sample"
      if [[ $peak_bytes =~ ^[0-9]+$ && $process_sample_peak_bytes =~ ^[0-9]+$ ]]; then
        memory_peak_unattributed_bytes=$(lima_memory_peak_unattributed_bytes \
          "$peak_bytes" "$process_sample_peak_bytes") || memory_peak_unattributed_bytes=unknown
      fi
    fi
    printf 'LIMA_MEMORY_PROBE result=%s attempt=%s/%s phase=%s uploads=%s image=%s image_id=%s revision=%s gomemlimit=%s websocket_listener=127.0.0.1:2446 websocket_origin=https://web.teagram.io websocket_browser_only=false peak_mib=%s peak_bytes=%s memory_peak_unattributed_bytes=%s memory_peak_above_go_sample_bytes=%s memory_stat_sample_interval_ms=%s memory_stat_sample_unix_nano=%s memory_runtime_sample_alignment_delta_ns=%s memory_stat_sample_peak_bytes=%s memory_stat_sample_current_bytes=%s memory_stat_anon_bytes=%s memory_stat_file_bytes=%s memory_stat_kernel_bytes=%s memory_max_bytes=%s metrics_source=%s max_events=%s oom=%s oom_kill=%s memory_process_sample_unix_nano=%s memory_process_sample_peak_bytes=%s memory_process_sample_memory_peak_bytes=%s memory_process_stat_anon_bytes=%s memory_process_stat_file_bytes=%s memory_process_stat_kernel_bytes=%s memory_process_attribution=%s memory_process_incomplete_pids=%s server_process_pid=%s server_process_rss_bytes=%s server_process_anon_bytes=%s photo_worker_process_count=%s photo_worker_processes=%s go_paired_process_attribution=%s go_paired_process_incomplete_pids=%s\n' \
      "$([[ $status -eq 0 ]] && printf passed || printf failed)" "${LIMA_MEMORY_PROBE_ATTEMPT:-1}" "$LIMA_MEMORY_PROBE_ATTEMPTS" "$PROBE_PHASE" "$UPLOAD_OUTCOME" \
      "$LIMA_SERVICE_IMAGE" "${IMAGE_ID:-unknown}" "$IMAGE_REVISION" "$LIMA_GOMEMLIMIT" "$peak_mib" "$peak_bytes" "$memory_peak_unattributed_bytes" "$memory_peak_above_go_sample_bytes" "$MEMORY_SAMPLE_INTERVAL_MS" "$runtime_cgroup_sample_unix_nano" "$runtime_sample_alignment_delta_ns" "$stat_sample_peak_bytes" "$sample_current_bytes" "$stat_anon_bytes" "$stat_file_bytes" "$stat_kernel_bytes" "$MEMORY_MAX_BYTES" "$MEMORY_METRICS_SOURCE" \
      "${max_events:-unknown}" "${oom:-unknown}" "${oom_kill:-unknown}" "$process_sample_unix_nano" "$process_sample_peak_bytes" "$process_sample_memory_peak_bytes" "$process_sample_anon_bytes" "$process_sample_file_bytes" "$process_sample_kernel_bytes" "$process_attribution" "$process_incomplete_pids" "$server_process_pid" "$server_process_rss_bytes" "$server_process_anon_bytes" "$photo_worker_process_count" "$photo_worker_processes" \
      "${runtime_process_attribution:-unknown}" "${runtime_process_incomplete_pids:-unknown}"
    printf 'LIMA_GO_RUNTIME_MEMORY gc_cpu_fraction=%s cgroup_sample_unix_nano=%s alignment_delta_ns=%s sample_current_bytes=%s cgroup_memory_peak_bytes=%s metrics=%s\n' \
      "$runtime_gc_cpu_fraction" "$runtime_cgroup_sample_unix_nano" "$runtime_sample_alignment_delta_ns" "$runtime_sample_current" "$runtime_cgroup_peak_bytes" "$runtime_sample"
    if [[ ${LIMA_MEMORY_CAPTURE_MAPS:-0} == 1 ]]; then
      for mapping_label in typical spike; do
        mapping_file="$RUNNER_TEMP/lima-memory-sampled-mappings-$mapping_label"
        if [[ -r $mapping_file ]]; then
          printf 'LIMA_MEMORY_MAPPING_SAMPLE label=%s ' "$mapping_label"
          tr '\n' ' ' <"$mapping_file"
          printf '\n'
        fi
      done
    fi
    if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
      {
        printf '\n### Lima service memory probe\n\n'
        printf '| Attempt | Result | Phase | Uploads | Image | Image ID | Revision | GOMEMLIMIT | WebSocket settings | Peak cgroup memory | Peak gap above independent max-current sample | Peak gap above Go-paired current | Stat sample interval | Go-paired sample timestamp (ns) | Go/cgroup alignment delta (ns) | Go-paired current | Go-paired memory.stat anon/file/kernel | memory.max | Metrics source | max events | OOM events | OOM kills | Independent process sample timestamp (ns) | Independent max-current sample | Its memory.stat anon/file/kernel | Process attribution | Incomplete process PIDs | Server PID | Server RSS | Server anon | Photo worker count | Photo worker processes (PID:RSS:anon bytes) | Go-paired process attribution |\n'
        printf '| ---: | --- | --- | --- | --- | --- | --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | --- | --- | ---: | --- | ---: | --- | --- | --- |\n'
        printf '| %s/%s | %s | %s | %s | %s | %s | %s | %s | 127.0.0.1:2446, https://web.teagram.io, browser_only=false | %s MiB (%s bytes) | %s bytes | %s bytes | %s ms | %s | %s | %s bytes | %s/%s/%s bytes | %s bytes | %s | %s | %s | %s | %s | %s bytes (memory.peak=%s) | %s/%s/%s bytes | %s | %s | %s | %s bytes | %s bytes | %s | %s | %s |\n' \
          "${LIMA_MEMORY_PROBE_ATTEMPT:-1}" "$LIMA_MEMORY_PROBE_ATTEMPTS" "$([[ $status -eq 0 ]] && printf passed || printf failed)" "$PROBE_PHASE" "$UPLOAD_OUTCOME" \
          "$LIMA_SERVICE_IMAGE" "${IMAGE_ID:-unknown}" "$IMAGE_REVISION" "$LIMA_GOMEMLIMIT" "$peak_mib" "$peak_bytes" "$memory_peak_unattributed_bytes" "$memory_peak_above_go_sample_bytes" "$MEMORY_SAMPLE_INTERVAL_MS" "$runtime_cgroup_sample_unix_nano" "$runtime_sample_alignment_delta_ns" "$sample_current_bytes" "$stat_anon_bytes" "$stat_file_bytes" "$stat_kernel_bytes" "$MEMORY_MAX_BYTES" \
          "$MEMORY_METRICS_SOURCE" "${max_events:-unknown}" "${oom:-unknown}" "${oom_kill:-unknown}" "$process_sample_unix_nano" "$process_sample_peak_bytes" "$process_sample_memory_peak_bytes" "$process_sample_anon_bytes" "$process_sample_file_bytes" "$process_sample_kernel_bytes" "$process_attribution" "$process_incomplete_pids" "$server_process_pid" "$server_process_rss_bytes" "$server_process_anon_bytes" "$photo_worker_process_count" "$photo_worker_processes" "${runtime_process_attribution:-unknown}"
        printf '\nGo runtime memory sample paired with cgroup/process sample %s (alignment delta %s ns):\n\n%s\n' \
          "$runtime_cgroup_sample_unix_nano" "$runtime_sample_alignment_delta_ns" "$runtime_sample"
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
  "$RUNNER_TEMP/lima-memory-sampled-processes" "$RUNNER_TEMP/lima-memory-sampled-history" \
  "$RUNNER_TEMP/lima-memory-sampled-mappings-typical" "$RUNNER_TEMP/lima-memory-sampled-mappings-typical.tmp" \
  "$RUNNER_TEMP/lima-memory-sampled-mappings-spike" "$RUNNER_TEMP/lima-memory-sampled-mappings-spike.tmp" \
  "$RUNNER_TEMP/lima-memory-sampled-mappings-spike-current" "$RUNNER_TEMP/lima-memory-sampled-mappings-spike-current.tmp" \
  "$RUNNER_TEMP/lima-memory-sampled-error" \
  "$RUNNER_TEMP/lima-memory-sampled-stop" "$RUNNER_TEMP/lima-memory-sampled-runtime" \
  "$RUNNER_TEMP/lima-memory-sampled-runtime-peak" "$RUNNER_TEMP/lima-memory-sampled-runtime.bundle.tmp" \
  "$RUNNER_TEMP/lima-memory-sampled-runtime-ready" \
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
  --memory "${DOCKER_MEMORY_LIMIT_MIB}m" --memory-swap "${DOCKER_MEMORY_LIMIT_MIB}m" --cpus 0.5 --pids-limit 128 \
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
    MEMORY_SAMPLE_RUNTIME_FILE="$RUNNER_TEMP/lima-memory-sampled-runtime"
    MEMORY_SAMPLE_RUNTIME_READY_FILE="$RUNNER_TEMP/lima-memory-sampled-runtime-ready"
    MEMORY_SAMPLE_RUNTIME_STOP_FILE="$RUNNER_TEMP/lima-memory-sampled-runtime-stop"
    MEMORY_SAMPLE_MAPPING_READY_FILE="$RUNNER_TEMP/lima-memory-sampled-mappings-ready"
    MEMORY_SAMPLE_MAPPING_STOP_FILE="$RUNNER_TEMP/lima-memory-sampled-mappings-stop"
    MEMORY_SAMPLE_ERROR_FILE="$RUNNER_TEMP/lima-memory-sampled-error"
    MEMORY_SAMPLE_STOP_FILE="$RUNNER_TEMP/lima-memory-sampled-stop"
    rm -f -- "$RUNNER_TEMP"/lima-memory-sampled-*
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
    lima_memory_sample_runtime_metrics_loop "$MEMORY_SAMPLE_STAT_PEAK_FILE" \
      "$RUNNER_TEMP/lima-memory-sampled-history" "$RUNTIME_SAMPLE_INTERVAL_BYTES" \
      "$MEMORY_SAMPLE_RUNTIME_READY_FILE" "$MEMORY_SAMPLE_RUNTIME_FILE" \
      "$MEMORY_SAMPLE_ERROR_FILE" "$MEMORY_SAMPLE_RUNTIME_STOP_FILE" \
      0.01 &
    MEMORY_RUNTIME_SAMPLER_PID=$!
    if [[ ${LIMA_MEMORY_CAPTURE_MAPS:-0} == 1 ]]; then
      lima_memory_sample_mapping_loop /proc "$container_pid" "$RUNNER_TEMP/lima-memory-sampled" \
        "$RUNNER_TEMP/lima-memory-sampled-history" "$MEMORY_SAMPLE_MAPPING_READY_FILE" \
        "$MEMORY_SAMPLE_MAPPING_STOP_FILE" "$MEMORY_SAMPLE_ERROR_FILE" 0.01 &
      MEMORY_MAPPING_SAMPLER_PID=$!
    fi
  else
    echo 'runner does not expose the service container cgroup v2 path' >&2
    exit 1
  fi
else
  echo "service container exited before measurement (pid=$container_pid)" >&2
  exit 1
fi

runtime_diagnostics_ready=false
for _ in $(seq 300); do
  if curl --noproxy '*' --fail --connect-timeout 1 --max-time 2 --silent --show-error \
    --output /dev/null http://127.0.0.1:2447/debug/runtime-memory; then
    runtime_diagnostics_ready=true
    break
  fi
  if [[ $(docker inspect --format '{{.State.Running}}' "$CONTAINER") != true ]]; then
    echo 'service container exited before runtime memory diagnostics became ready' >&2
    exit 1
  fi
  sleep 0.1
done
if [[ $runtime_diagnostics_ready != true ]]; then
  echo 'Lima runtime memory diagnostics did not accept a startup request on 127.0.0.1:2447' >&2
  exit 1
fi
touch "$MEMORY_SAMPLE_RUNTIME_READY_FILE"
if [[ ${LIMA_MEMORY_CAPTURE_MAPS:-0} == 1 ]]; then
  typical_request_row=
  for _ in $(seq 500); do
    if [[ -r $RUNNER_TEMP/lima-memory-sampled-history ]]; then
      typical_request_row=$(tail -n 1 "$RUNNER_TEMP/lima-memory-sampled-history")
      [[ -n $typical_request_row ]] && break
    fi
    sleep 0.01
  done
  if [[ -z $typical_request_row ]]; then
    echo 'mapping diagnostics did not record a startup cgroup/process baseline' >&2
    exit 1
  fi
  if ! lima_memory_queue_mapping_request "$RUNNER_TEMP/lima-memory-sampled" typical \
    "$typical_request_row"; then
    echo 'mapping diagnostics could not queue the startup typical sample' >&2
    exit 1
  fi
  touch "$MEMORY_SAMPLE_MAPPING_READY_FILE"
  typical_capture_complete=false
  for _ in $(seq 500); do
    if [[ -s $RUNNER_TEMP/lima-memory-sampled-mappings-typical ]]; then
      typical_capture_complete=true
      break
    fi
    if [[ -r $MEMORY_SAMPLE_ERROR_FILE ]]; then
      cat "$MEMORY_SAMPLE_ERROR_FILE" >&2
      exit 1
    fi
    sleep 0.01
  done
  if [[ $typical_capture_complete != true ]]; then
    echo 'mapping diagnostics did not capture the startup baseline' >&2
    exit 1
  fi
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

if (( EXPECTED_LIMIT <= 64 * 1024 * 1024 )); then
  memory_cap_log=$(docker logs "$CONTAINER" 2>&1 | awk '/msg="Lima Go memory cap applied"/ { line = $0 } END { if (line != "") print line }')
else
  memory_cap_log=$(docker logs "$CONTAINER" 2>&1 | awk '/msg="Lima Go memory cap not applied"/ { line = $0 } END { if (line != "") print line }')
fi
if [[ -z $memory_cap_log ]]; then
  echo 'Lima Go memory cap decision log is missing from the server container' >&2
  exit 1
fi
if (( EXPECTED_LIMIT <= 64 * 1024 * 1024 )); then
  case "$memory_cap_log" in
    *"cgroup_memory_max_bytes=$EXPECTED_LIMIT go_memory_limit_bytes=$LIMA_EXPECTED_GO_MEMORY_LIMIT_BYTES") ;;
    *)
      printf 'Lima Go memory cap log does not match the probe cgroup and effective GOMEMLIMIT=%s target (%s bytes): %s\n' \
        "$LIMA_GOMEMLIMIT" "$LIMA_EXPECTED_GO_MEMORY_LIMIT_BYTES" "$memory_cap_log" >&2
      exit 1
      ;;
  esac
else
  case "$memory_cap_log" in
    *"cgroup_memory_max_bytes=$EXPECTED_LIMIT threshold_bytes=$((64 * 1024 * 1024))") ;;
    *)
      printf 'Lima Go memory cap skip log does not match the diagnostic cgroup limit: %s\n' "$memory_cap_log" >&2
      exit 1
      ;;
  esac
fi
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
  username="limamem_${LIMA_MEMORY_PROBE_USERNAME_PREFIX}_$suffix"
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
if [[ ${LIMA_MEMORY_CAPTURE_MAPS:-0} == 1 ]]; then
  mapping_cgroup_peak=$(<"$MEMORY_PEAK_FILE")
  mapping_typical_bytes=${LIMA_MEMORY_MAPPING_TYPICAL_BYTES:-31457280}
  mapping_spike_delta_bytes=$(lima_memory_mapping_spike_delta_bytes) || exit 1
  mapping_anon_tolerance_bytes=${LIMA_MEMORY_MAPPING_ANON_TOLERANCE_BYTES:-131072}
  if ! lima_memory_mapping_capture_check "$mapping_cgroup_peak" "$mapping_typical_bytes" \
    "$mapping_spike_delta_bytes" "$mapping_anon_tolerance_bytes" \
    "$RUNNER_TEMP/lima-memory-sampled-mappings-typical" \
    "$RUNNER_TEMP/lima-memory-sampled-mappings-spike"; then
    exit 1
  fi
fi
if [[ ! -r $MEMORY_SAMPLE_RUNTIME_FILE ]]; then
  echo 'memory probe did not capture a timestamp-aligned Go runtime and cgroup/process sample' >&2
  exit 1
fi
runtime_cgroup_sample=$(jq -er '.cgroup_sample | select(type == "string")' "$MEMORY_SAMPLE_RUNTIME_FILE")
IFS='|' read -r runtime_cgroup_sample_unix_nano sample_current_bytes stat_anon_bytes stat_file_bytes stat_kernel_bytes \
  runtime_paired_process_current _ _ _ _ _ runtime_paired_process_attribution runtime_paired_incomplete_pids \
  runtime_cgroup_peak_at_go_sample \
  <<<"$runtime_cgroup_sample"
if [[ ! $runtime_cgroup_sample_unix_nano =~ ^[0-9]+$ ||
  ! $sample_current_bytes =~ ^[0-9]+$ ||
  ! $runtime_paired_process_current =~ ^[0-9]+$ ||
  $runtime_paired_process_current != "$sample_current_bytes" ||
  ! $runtime_cgroup_peak_at_go_sample =~ ^[0-9]+$ ||
  ( $runtime_paired_process_attribution != complete && $runtime_paired_process_attribution != incomplete ) ||
  ( $runtime_paired_process_attribution == complete && $runtime_paired_incomplete_pids != - ) ||
  ( $runtime_paired_process_attribution == incomplete &&
    ( $runtime_paired_incomplete_pids == - || $runtime_paired_incomplete_pids == unknown ) ) ]]; then
  printf 'Go-paired cgroup sample is missing its timestamp, current, high-water, or process-attribution fields\n' >&2
  exit 1
fi
stat_sample_peak_bytes=$runtime_cgroup_peak_at_go_sample
best_process_sample=$(lima_memory_best_cgroup_sample "$RUNNER_TEMP/lima-memory-sampled-history")
if [[ -z $best_process_sample ]]; then
  echo 'memory probe did not record an independent cgroup-current/process sample' >&2
  exit 1
fi
IFS='|' read -r process_sample_unix_nano process_sample_peak_bytes process_sample_anon_bytes \
  process_sample_file_bytes process_sample_kernel_bytes _ server_process_pid server_process_rss_bytes \
  server_process_anon_bytes photo_worker_process_count photo_worker_processes process_attribution \
  process_incomplete_pids process_sample_memory_peak_bytes <<<"$best_process_sample"
if [[ ! $process_sample_unix_nano =~ ^[0-9]+$ || ! $process_sample_peak_bytes =~ ^[0-9]+$ ||
  ! $process_sample_memory_peak_bytes =~ ^[0-9]+$ ||
  ( $process_attribution != complete && $process_attribution != incomplete ) ||
  ( $process_attribution == complete && $process_incomplete_pids != - ) ||
  ( $process_attribution == incomplete && ( $process_incomplete_pids == - || $process_incomplete_pids == unknown ) ) ||
  ! $server_process_pid =~ ^[1-9][0-9]*$ ||
  ! $server_process_rss_bytes =~ ^[0-9]+$ ||
  ! $server_process_anon_bytes =~ ^[0-9]+$ ||
  ! $photo_worker_process_count =~ ^[0-9]+$ ||
  $photo_worker_processes == unknown ]]; then
  printf 'independent maximum-current process sample is malformed: status=%s pids=%s\n' \
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
