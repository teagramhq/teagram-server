#!/usr/bin/env bash
set -Eeuo pipefail

# Keep this pool below the ubuntu-26.04 runner's core count; never size it from nproc.
readonly WORKER_COUNT=3
SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../.." && pwd)
readonly SCRIPT_DIR REPO_ROOT

CHECKS=(
  'Check the deploy workflow contract'
  'Exercise production deploy runtime preflight steps'
  'Check isolated QA invite deployment contract'
  'Check QA bot workflow and operation fixtures'
  'Test QA invite TLS constraints and renewal'
  'Test immutable K8s image digest publication'
  'Test production image pin resolution'
  'Test bounded K8s workload checks'
  'Test early post-apply release rollback'
  'Test the alpha bootstrap apply path with mocks'
  'Check RPC performance workflow contract'
  'Check PR CI cancellation policy'
  'Check workflow contract script syntax'
  'Test the alpha Incus host compatibility'
  'Check deploy script syntax'
  'Verify retired cutover refuses production actions'
  'Check invite signup URL operation fixtures'
  'Check teagram log access command'
  'Check teagram ops command'
  'Check QA password timestamp operation'
  'Check Bot API QA bot and relay fixtures'
  'Check media encryption and cleanup operations'
  'Check demo blob-mode authority helpers'
  'Check media dump validation against PostgreSQL 18'
  'Check teagram server start operation'
  'Check performance Postgres startup readiness'
  'Check teagram rollout failure report'
  'Check public demo Compose policy'
  'Check Lima demo egress isolation'
  'Check demo ops command menu'
  'Check demo ops installer key transaction'
  'Check pinned demo web artifact handling'
  'Check demo web live publication resolution'
  'Check demo web publication ordering'
  'Check demo Turnstile secret access'
  'Check demo image release pinning'
)

run_with_loopback_port_lock() (
  # These fixtures select a free port, release it, then bind it; keep their assignments distinct.
  exec 9>"$DEPLOY_CHECK_POOL_DIR/loopback-port.lock"
  flock -x 9
  "$@"
)

run_check() {
  case "$1" in
    'Check the deploy workflow contract')
      .github/scripts/test-deploy-workflow-contract.sh
      ;;
    'Exercise production deploy runtime preflight steps')
      .github/scripts/test-deploy-runtime-preflight.sh
      ;;
    'Check isolated QA invite deployment contract')
      deploy/qa-invite/test-qa-invite-contract.sh
      ;;
    'Check QA bot workflow and operation fixtures')
      bash deploy/k8s/test-qa-bot-workflow-contract.sh
      python3 -B deploy/k8s/test_qa_bot_operation.py
      python3 -B deploy/qa-invite/test_qa_bot_writer.py
      sh -n deploy/k8s/qa-bot-operation.sh
      for script in deploy/k8s/wait-qa-bot-job.sh deploy/k8s/test-qa-bot-k8s.sh deploy/qa-invite/qa-account-writer.sh; do
        bash -n "$script"
      done
      shellcheck deploy/k8s/qa-bot-operation.sh deploy/k8s/wait-qa-bot-job.sh deploy/k8s/test-qa-bot-k8s.sh deploy/qa-invite/qa-account-writer.sh deploy/k8s/test-qa-bot-workflow-contract.sh
      ;;
    'Test QA invite TLS constraints and renewal')
      run_with_loopback_port_lock sudo -n bash deploy/qa-invite/test-qa-invite-tls.sh
      ;;
    'Test immutable K8s image digest publication')
      .github/scripts/test-k8s-image-digests.sh
      ;;
    'Test production image pin resolution')
      python3 -B .github/scripts/test-update-k8s-prod-pins.py
      ;;
    'Test bounded K8s workload checks')
      .github/scripts/test-k8s-deploy-contract.sh
      ;;
    'Test early post-apply release rollback')
      python3 -B deploy/k8s/test-deploy-release-early-rollback.py
      ;;
    'Test the alpha bootstrap apply path with mocks')
      # The test writes a fixed /root kubeconfig; a private mount namespace gives it its own /root copy.
      sudo -n unshare --mount --propagation private bash -c '
        mount -t tmpfs -o mode=0755 tmpfs /root
        exec "$1"
      ' bootstrap-alpha-test "$REPO_ROOT/deploy/k8s/test-bootstrap-alpha.sh"
      ;;
    'Check RPC performance workflow contract')
      GOPROXY='https://proxy.golang.org|direct' .github/scripts/test-perf-workflow-contract.sh
      ;;
    'Check PR CI cancellation policy')
      .github/scripts/test-ci-main-priority.sh
      ;;
    'Check workflow contract script syntax')
      bash -n .github/scripts/perf-run.sh .github/scripts/test-perf-workflow-contract.sh .github/scripts/test-ci-main-priority.sh
      shellcheck .github/scripts/test-perf-workflow-contract.sh .github/scripts/test-ci-main-priority.sh
      bash -n .github/scripts/publish-k8s-image-digests.sh .github/scripts/test-k8s-image-digests.sh
      shellcheck .github/scripts/publish-k8s-image-digests.sh .github/scripts/test-k8s-image-digests.sh
      bash -n deploy/k8s/deploy-release.sh \
        deploy/k8s/initialize-channel-post-expiring-summaries.sh \
        deploy/k8s/test-initialize-channel-post-expiring-summaries.sh \
        deploy/k8s/test-postgres-backup-restore.sh \
        deploy/k8s/test-postgres-backup-retention.sh \
        deploy/k8s/test-postgres-backup-restore-guard.sh
      shellcheck deploy/k8s/deploy-release.sh \
        deploy/k8s/initialize-channel-post-expiring-summaries.sh \
        deploy/k8s/test-initialize-channel-post-expiring-summaries.sh \
        deploy/k8s/test-postgres-backup-restore.sh \
        deploy/k8s/test-postgres-backup-retention.sh \
        deploy/k8s/test-postgres-backup-restore-guard.sh
      shellcheck --shell=sh deploy/k8s/base/postgres-backup/backup.sh
      bash -n .github/scripts/test-k8s-deploy-contract.sh
      shellcheck .github/scripts/test-k8s-deploy-contract.sh
      bash -n .github/scripts/test-deploy-runtime-preflight.sh
      shellcheck .github/scripts/test-deploy-runtime-preflight.sh
      bash -n .github/scripts/test-k8s-agent-access.sh deploy/k8s/check-agent-rbac.sh
      shellcheck .github/scripts/test-k8s-agent-access.sh deploy/k8s/check-agent-rbac.sh
      bash -n .github/scripts/check-k8s-migration-version.sh
      shellcheck .github/scripts/check-k8s-migration-version.sh
      bash -n .github/scripts/set-k8s-ci-migration-version.sh
      shellcheck .github/scripts/set-k8s-ci-migration-version.sh
      bash -n .github/scripts/check-k8s-prod-image-pins.sh
      shellcheck .github/scripts/check-k8s-prod-image-pins.sh
      bash -n .github/scripts/run-deploy-workflow-checks.sh
      shellcheck .github/scripts/run-deploy-workflow-checks.sh
      ;;
    'Test the alpha Incus host compatibility')
      # Fixed /tmp fixture paths are inside this check's fresh anonymous container.
      deploy/k8s/test-alpha-incus-compat.sh
      ;;
    'Check deploy script syntax')
      ./deploy/k8s/bootstrap-alpha.sh --dry-run
      bash -n deploy/k8s/alpha-incus.sh deploy/k8s/bootstrap-alpha.sh
      bash -n deploy/k8s/test-bootstrap-alpha.sh
      bash -n deploy/k8s/test-alpha-incus-compat.sh
      bash -n deploy/k8s/install-tailnet-proxy-access.sh
      ./deploy/k8s/cutover-alpha.sh --dry-run
      bash -n deploy/k8s/cutover-alpha.sh deploy/k8s/test-cutover-alpha.sh
      bash -n deploy/k8s/update-public-link-prefix.sh
      shellcheck deploy/k8s/update-public-link-prefix.sh
      bash -n deploy/teagram/teagram-logs.sh
      sh -n deploy/teagram/teagram-ops.sh
      bash -n deploy/teagram/ops/invite-web-signup-url.sh
      bash -n deploy/teagram/test-invite-web-signup-url.sh
      bash -n deploy/teagram/ops/rollout-evidence.sh
      bash -n deploy/teagram/test-teagram-logs.sh
      bash -n deploy/teagram/ops/target-failure-report.sh
      bash -n deploy/teagram/test-teagram-ops.sh
      bash -n deploy/teagram/test-target-failure-report.sh
      bash -n deploy/teagram/ops/start-server.sh
      bash -n deploy/teagram/ops/bot-api-qa.sh
      bash -n deploy/teagram/ops/bot-api-qa-bot.sh deploy/teagram/test-bot-api-qa-bot-ops.sh deploy/teagram/test-bot-api-relays.sh
      bash -n deploy/teagram/ops/encrypt-media.sh
      bash -n deploy/teagram/ops/cleanup-media.sh
      bash -n deploy/teagram/ops/backup-media-key.sh
      bash -n deploy/teagram/ops/attest-blob-mode-reports.sh
      bash -n deploy/teagram/ops/quarantine-unaccounted-media.sh
      bash -n deploy/teagram/ops/qa-password-timestamps.sh deploy/teagram/test-qa-password-timestamps.sh
      bash -n deploy/teagram/ops/blob-encryption-common.inc
      bash -n deploy/teagram/test-media-encryption-ops.sh
      bash -n deploy/teagram/test-media-encryption-pg18.sh
      bash -n deploy/teagram/test-start-server.sh
      bash -n deploy/teagram-demo/prepare-env.sh
      bash -n deploy/teagram-demo/demo-common.sh
      bash -n deploy/teagram-demo/init-demo.sh
      bash -n deploy/teagram-demo/forced-command.sh
      bash -n deploy/qa-invite/remote.sh
      bash -n deploy/qa-invite/rollback.sh
      bash -n deploy/qa-invite/tls.sh
      bash -n deploy/qa-invite/qa-account-writer.sh
      bash -n deploy/qa-invite/test-qa-invite-browser-flow.sh
      bash -n deploy/qa-invite/test-qa-invite-contract.sh
      bash -n deploy/qa-invite/test-qa-invite-rollback.sh
      bash -n deploy/qa-invite/test-qa-invite-tls.sh
      bash -n deploy/teagram-demo/ops/teagram-demo-ops
      bash -n deploy/teagram-demo/ops/install-teagram-demo-ops.sh
      sh -n deploy/teagram-demo/ops/install-qa-invite-command.sh
      bash -n deploy/teagram-demo/ops/turnstile-signin-rollout.sh
      bash -n deploy/teagram-demo/ops/turnstile-signin-rollback.sh
      bash -n deploy/teagram-demo/ops/rotate-turnstile-qa-bypass-token.sh
      sh -n deploy/teagram-demo/ops/turnstile-signin-on.sh
      sh -n deploy/teagram-demo/ops/turnstile-signin-off.sh
      bash -n deploy/teagram-demo/test-install-teagram-demo-ops.sh
      bash -n deploy/teagram-demo/test-ops-menu.sh
      bash -n deploy/teagram-demo/deploy-main.sh
      bash -n deploy/teagram-demo/publish-web-release.sh
      bash -n deploy/teagram-demo/build-web-artifact.sh
      bash -n deploy/teagram-demo/wipe-weekly.sh
      bash -n deploy/teagram-demo/test-general-moderation-compose.sh
      bash -n deploy/teagram-demo/test-general-moderation-isolated-compose.sh
      bash -n deploy/teagram-demo/install-weekly-wipe.sh
      bash -n deploy/teagram-demo/run-activity.sh
      bash -n deploy/teagram-demo/demo-cron.sh
      bash -n deploy/teagram-demo/activity-guards.sh
      bash -n deploy/teagram-demo/test-demo-scheduling.sh
      bash -n deploy/teagram-demo/test-demo-compose.sh
      bash -n deploy/teagram-demo/test-tailnet-source-address.sh
      bash -n deploy/teagram-demo/image-retention.sh
      bash -n deploy/teagram-demo/lima/command-contract.sh
      bash -n deploy/teagram-demo/lima/deploy-main.sh
      bash -n deploy/teagram-demo/lima/deploy-root-wrapper.sh
      bash -n deploy/teagram-demo/lima/install-deploy-entry.sh
      bash -n deploy/teagram-demo/lima/test-deploy-contract.sh
      sh -n deploy/teagram-demo/lima/teagram-demo-deploy-sshd.initd
      bash -n deploy/teagram-demo/test-identity-import.sh
      bash -n deploy/teagram-demo/lima/install-egress-isolation.sh
      bash -n deploy/teagram-demo/ops/weekly-wipe.sh
      bash -n deploy/teagram-demo/test-turnstile-signin-rollout.sh
      bash -n deploy/teagram-demo/test-image-release.sh
      bash -n deploy/teagram-demo/test-seed-compose.sh
      bash -n deploy/link-edge/deploy-main.sh
      bash -n deploy/link-edge/test-deploy-main.sh
      bash -n deploy/teagram-demo/test-turnstile-secret-access.sh
      bash -n .github/scripts/perf-postgres.sh .github/scripts/test-perf-postgres.sh
      bash -n .github/scripts/test-lima-server-memory.sh .github/scripts/lima-memory-process-snapshot.sh .github/scripts/test-lima-memory-process-snapshot.sh
      bash .github/scripts/test-lima-memory-process-snapshot.sh
      shellcheck \
        deploy/k8s/bootstrap-alpha.sh \
        deploy/k8s/alpha-incus.sh \
        deploy/k8s/test-bootstrap-alpha.sh \
        deploy/k8s/test-alpha-incus-compat.sh \
        deploy/k8s/install-tailnet-proxy-access.sh \
        deploy/k8s/cutover-alpha.sh \
        deploy/k8s/test-cutover-alpha.sh \
        deploy/teagram/teagram-logs.sh \
        deploy/teagram/teagram-ops.sh \
        deploy/teagram/ops/invite-web-signup-url.sh \
        deploy/teagram/test-invite-web-signup-url.sh \
        deploy/teagram/ops/rollout-evidence.sh \
        deploy/teagram/test-teagram-logs.sh \
        deploy/teagram/ops/target-failure-report.sh \
        deploy/teagram/test-teagram-ops.sh \
        deploy/teagram/test-target-failure-report.sh \
        deploy/teagram/ops/start-server.sh \
        deploy/teagram/ops/bot-api-qa.sh \
        deploy/teagram/ops/bot-api-qa-bot.sh \
        deploy/teagram/test-bot-api-qa-bot-ops.sh \
        deploy/teagram/test-bot-api-qa-optional-restore.sh \
        deploy/teagram/test-bot-api-relays.sh \
        deploy/teagram/ops/encrypt-media.sh \
        deploy/teagram/ops/cleanup-media.sh \
        deploy/teagram/ops/backup-media-key.sh \
        deploy/teagram/ops/attest-blob-mode-reports.sh \
        deploy/teagram/ops/quarantine-unaccounted-media.sh \
        deploy/teagram/ops/qa-password-timestamps.sh \
        deploy/teagram/test-qa-password-timestamps.sh \
        deploy/teagram/test-start-server.sh \
        deploy/teagram-demo/prepare-env.sh \
        deploy/teagram-demo/demo-common.sh \
        deploy/teagram-demo/init-demo.sh \
        deploy/teagram-demo/forced-command.sh \
        deploy/qa-invite/remote.sh \
        deploy/qa-invite/rollback.sh \
        deploy/qa-invite/tls.sh \
        deploy/qa-invite/qa-account-writer.sh \
        deploy/qa-invite/test-qa-invite-browser-flow.sh \
        deploy/qa-invite/test-qa-invite-contract.sh \
        deploy/qa-invite/test-qa-invite-rollback.sh \
        deploy/qa-invite/test-qa-invite-tls.sh \
        deploy/teagram-demo/ops/teagram-demo-ops \
        deploy/teagram-demo/ops/install-teagram-demo-ops.sh \
        deploy/teagram-demo/ops/install-qa-invite-command.sh \
        deploy/teagram-demo/ops/turnstile-signin-rollout.sh \
        deploy/teagram-demo/ops/turnstile-signin-rollback.sh \
        deploy/teagram-demo/ops/turnstile-signin-on.sh \
        deploy/teagram-demo/ops/turnstile-signin-off.sh \
        deploy/teagram-demo/test-install-teagram-demo-ops.sh \
        deploy/teagram-demo/test-ops-menu.sh \
        deploy/teagram-demo/deploy-main.sh \
        deploy/teagram-demo/publish-web-release.sh \
        deploy/teagram-demo/build-web-artifact.sh \
        deploy/teagram-demo/wipe-weekly.sh \
        deploy/teagram-demo/test-general-moderation-compose.sh \
        deploy/teagram-demo/test-general-moderation-isolated-compose.sh \
        deploy/teagram-demo/install-weekly-wipe.sh \
        deploy/teagram-demo/run-activity.sh \
        deploy/teagram-demo/demo-cron.sh \
        deploy/teagram-demo/activity-guards.sh \
        deploy/teagram-demo/image-retention.sh \
        deploy/teagram-demo/test-demo-scheduling.sh \
        deploy/teagram-demo/test-demo-compose.sh \
        deploy/teagram-demo/test-tailnet-source-address.sh \
        deploy/teagram-demo/test-identity-import.sh \
        deploy/teagram-demo/lima/check.sh \
        deploy/teagram-demo/lima/command-contract.sh \
        deploy/teagram-demo/lima/deploy-main.sh \
        deploy/teagram-demo/lima/deploy-root-wrapper.sh \
        deploy/teagram-demo/lima/install-deploy-entry.sh \
        deploy/teagram-demo/lima/teagram-demo-deploy-sshd.initd \
        deploy/teagram-demo/lima/test-deploy-contract.sh \
        deploy/teagram-demo/lima/install-egress-isolation.sh \
        deploy/teagram-demo/lima/prepare-blobs.sh \
        deploy/teagram-demo/lima/test-lima-compose.sh \
        deploy/teagram-demo/ops/weekly-wipe.sh \
        deploy/teagram-demo/test-turnstile-signin-rollout.sh \
        deploy/teagram-demo/test-image-release.sh \
        deploy/teagram-demo/test-seed-compose.sh \
        deploy/link-edge/deploy-main.sh \
        deploy/link-edge/test-deploy-main.sh \
        deploy/teagram-demo/test-turnstile-secret-access.sh \
        .github/scripts/test-lima-server-memory.sh \
        .github/scripts/lima-memory-process-snapshot.sh \
        .github/scripts/test-lima-memory-process-snapshot.sh \
        deploy/teagram/test-media-encryption-ops.sh \
        deploy/teagram/test-media-encryption-pg18.sh \
        .github/scripts/perf-postgres.sh \
        .github/scripts/test-perf-postgres.sh \
        .github/scripts/validate-deploy-target.sh \
        .github/scripts/test-deploy-workflow-contract.sh \
        .github/scripts/run-deploy-workflow-checks.sh
      shellcheck --shell=bash deploy/teagram/ops/blob-encryption-common.inc
      shellcheck deploy/teagram-demo/ops/rotate-turnstile-qa-bypass-token.sh
      ;;
    'Verify retired cutover refuses production actions')
      deploy/k8s/test-cutover-alpha.sh
      ;;
    'Check invite signup URL operation fixtures')
      sudo -n env "PATH=$PATH" bash deploy/teagram/test-invite-web-signup-url.sh
      ;;
    'Check teagram log access command')
      deploy/teagram/test-teagram-logs.sh
      ;;
    'Check teagram ops command')
      deploy/teagram/test-teagram-ops.sh
      ;;
    'Check QA password timestamp operation')
      deploy/teagram/test-qa-password-timestamps.sh
      ;;
    'Check Bot API QA bot and relay fixtures')
      bash deploy/teagram/test-bot-api-qa-bot-ops.sh
      bash deploy/teagram/test-bot-api-qa-optional-restore.sh
      # Each run gets a unique Compose project; lock the loopback port while its fixture binds it.
      run_with_loopback_port_lock bash deploy/teagram/test-bot-api-relays.sh
      ;;
    'Check media encryption and cleanup operations')
      bash deploy/teagram/test-media-encryption-ops.sh
      python3 -B deploy/teagram/test-quarantine-unaccounted-media.py
      ;;
    'Check demo blob-mode authority helpers')
      python3 -B deploy/teagram-demo/blob-mode/test-blob-mode-state.py
      python3 -B deploy/teagram/ops/test-blob-transition-runner.py
      python3 -B deploy/teagram/ops/test-blob-mode-report-attestation.py
      ;;
    'Check media dump validation against PostgreSQL 18')
      # Each invocation uses private temp files, random container names, and an isolated network.
      PG18_IMAGE=mirror.gcr.io/library/postgres:18-alpine@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5 \
        bash deploy/teagram/test-media-encryption-pg18.sh
      ;;
    'Check teagram server start operation')
      python3 -B deploy/teagram/test-local-compose-ops-contract.py
      bash deploy/teagram/test-start-server.sh
      ;;
    'Check performance Postgres startup readiness')
      bash .github/scripts/test-perf-postgres.sh
      ;;
    'Check teagram rollout failure report')
      deploy/teagram/test-target-failure-report.sh
      ;;
    'Check public demo Compose policy')
      false
      ;;
    'Check Lima demo egress isolation')
      python3 -B deploy/teagram-demo/lima/test-egress-isolation-rules.py
      ;;
    'Check demo ops command menu')
      deploy/teagram-demo/test-ops-menu.sh
      ;;
    'Check demo ops installer key transaction')
      sudo -n bash deploy/teagram-demo/test-install-teagram-demo-ops.sh
      ;;
    'Check pinned demo web artifact handling')
      python3 deploy/teagram-demo/test-web-release.py
      ;;
    'Check demo web live publication resolution')
      python3 deploy/teagram-demo/test-web-artifact-pin.py
      ;;
    'Check demo web publication ordering')
      python3 deploy/teagram-demo/test-web-publication-state.py
      python3 deploy/teagram-demo/test-web-publication-rollout.py
      ;;
    'Check demo Turnstile secret access')
      sudo -n deploy/teagram-demo/test-turnstile-secret-access.sh
      sudo -n python3 deploy/teagram-demo/test-rotate-qa-bypass-token.py
      ;;
    'Check demo image release pinning')
      deploy/teagram-demo/test-image-release.sh
      ;;
    *)
      printf 'unknown deployment workflow check: %s\n' "$1" >&2
      return 64
      ;;
  esac
}

if [[ "${1:-}" == '--task' ]]; then
  task_index=${2:?task index is required}
  task_name=${CHECKS[$task_index]:?unknown task index: $task_index}
  pool_dir=${DEPLOY_CHECK_POOL_DIR:?worker pool directory is required}
  started_ns=$(date +%s%N)
  # shellcheck disable=SC2329 # EXIT traps invoke this worker finalizer.
  finish_task() {
    local status=$? finished_ns
    trap - EXIT
    finished_ns=$(date +%s%N)
    printf '%s\t%s\t%s\n' "$status" "$started_ns" "$finished_ns" > "$pool_dir/$task_index.status"
    # The parent reads the result file; keeping workers successful makes xargs drain every check.
    exit 0
  }
  trap finish_task EXIT
  exec > "$pool_dir/$task_index.log" 2>&1
  printf 'Running %s\n' "$task_name"
  cd "$REPO_ROOT"
  run_check "$task_name"
  exit 0
fi

[[ $# -eq 0 ]] || { printf 'usage: %s\n' "${BASH_SOURCE[0]}" >&2; exit 64; }

pool_dir=$(mktemp -d)
# shellcheck disable=SC2329 # EXIT trap removes the worker artifacts after reporting.
cleanup() { rm -rf -- "$pool_dir"; }
trap cleanup EXIT
export DEPLOY_CHECK_POOL_DIR=$pool_dir
pool_started_ns=$(date +%s%N)
task_count=${#CHECKS[@]}
xargs_status=0
if {
  for ((task_index = 0; task_index < task_count; task_index++)); do
    printf '%s\n' "$task_index"
  done
} | xargs -r -n 1 -P "$WORKER_COUNT" bash "$SCRIPT_DIR/run-deploy-workflow-checks.sh" --task \
  > "$pool_dir/xargs.log" 2>&1; then
  xargs_status=0
else
  xargs_status=$?
fi
pool_finished_ns=$(date +%s%N)
pool_elapsed_ms=$(((pool_finished_ns - pool_started_ns) / 1000000))
pool_failed=0
summary_rows=()

format_duration() {
  local milliseconds=$1
  printf '%d.%03ds' "$((milliseconds / 1000))" "$((milliseconds % 1000))"
}

for ((task_index = 0; task_index < task_count; task_index++)); do
  task_name=${CHECKS[$task_index]}
  status_file="$pool_dir/$task_index.status"
  log_file="$pool_dir/$task_index.log"
  printf '\n===== %s =====\n' "$task_name"
  if [[ ! -f $status_file ]]; then
    printf 'FAILED: %s did not report a result\n' "$task_name"
    pool_failed=1
    summary_rows+=("| $task_name | unknown | failed |")
    continue
  fi

  IFS=$'\t' read -r task_status task_started_ns task_finished_ns < "$status_file"
  task_elapsed_ms=$(((task_finished_ns - task_started_ns) / 1000000))
  [[ ! -s $log_file ]] || cat "$log_file"
  if [[ $task_status == 0 ]]; then
    printf 'PASS: %s (%s)\n' "$task_name" "$(format_duration "$task_elapsed_ms")"
    summary_rows+=("| $task_name | $(format_duration "$task_elapsed_ms") | passed |")
  else
    printf 'FAILED: %s (exit %s, %s)\n' "$task_name" "$task_status" "$(format_duration "$task_elapsed_ms")"
    summary_rows+=("| $task_name | $(format_duration "$task_elapsed_ms") | failed |")
    pool_failed=1
  fi
done

if [[ -s $pool_dir/xargs.log ]]; then
  printf '\n===== Worker pool launcher =====\n'
  cat "$pool_dir/xargs.log"
  pool_failed=1
fi
if ((xargs_status != 0)); then
  printf 'FAILED: xargs worker pool exited with status %s\n' "$xargs_status"
  pool_failed=1
fi

printf '\nDeployment workflow checks used %d fixed workers; pool wall time %s.\n' \
  "$WORKER_COUNT" "$(format_duration "$pool_elapsed_ms")"
if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
  {
    printf '\n### Deployment workflow contract check timings\n\n'
    printf '| Check | Worker time | Result |\n| --- | ---: | --- |\n'
    printf '%s\n' "${summary_rows[@]}"
    printf '| %d-worker pool wall time | %s | %s |\n' \
      "$WORKER_COUNT" "$(format_duration "$pool_elapsed_ms")" \
      "$([[ $pool_failed == 0 ]] && printf passed || printf failed)"
  } >> "$GITHUB_STEP_SUMMARY"
fi

exit "$pool_failed"
