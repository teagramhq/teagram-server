#!/usr/bin/env bash
set -Eeuo pipefail

# Keep this worker pool fixed; never size it from nproc.
readonly WORKER_COUNT=5
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
  'Lint deployment, performance, and real-client workflows'
  'Test deploy workflow worker preserves command failures'
)

# Assign all checks once across a fixed pool, balanced using measured CI task times.
readonly -a WORKER_1_TASKS=(26)
readonly -a WORKER_2_TASKS=(1 2 5 10 12 14 15 22 27 28 31 35)
readonly -a WORKER_3_TASKS=(3 4 6 7 16 20 21 30 32 34 36)
readonly -a WORKER_4_TASKS=(0 8 9 11 17 18 23 24 25 29 33)
readonly -a WORKER_5_TASKS=(13 19)

run_with_loopback_port_lock() (
  # These fixtures select a free port, release it, then bind it; keep their assignments distinct.
  local lock_dir=${DEPLOY_CHECK_LOCK_DIR:-${DEPLOY_CHECK_POOL_DIR:?worker pool directory is required}}
  mkdir -p "$lock_dir"
  exec 9>"$lock_dir/loopback-port.lock"
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
    'Check RPC performance workflow contract')
      GOPROXY='https://proxy.golang.org|direct' .github/scripts/test-perf-workflow-contract.sh
      ;;
    'Check PR CI cancellation policy')
      .github/scripts/test-ci-main-priority.sh
      ;;
    'Check workflow contract script syntax')
      bash -n .github/scripts/perf-run.sh .github/scripts/test-perf-workflow-contract.sh .github/scripts/test-ci-main-priority.sh
      shellcheck .github/scripts/test-perf-workflow-contract.sh .github/scripts/test-ci-main-priority.sh
      bash -n .github/scripts/test-deploy-workflow-worker-fail-fast.sh
      bash -n .github/scripts/test-deploy-workflow-loopback-port-lock.sh
      shellcheck .github/scripts/test-deploy-workflow-worker-fail-fast.sh .github/scripts/test-deploy-workflow-loopback-port-lock.sh
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
    'Test deploy workflow worker preserves command failures')
      .github/scripts/test-deploy-workflow-worker-fail-fast.sh
      bash .github/scripts/test-deploy-workflow-loopback-port-lock.sh
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
      bash .github/scripts/pull-base-images.sh --image mirror.gcr.io/library/postgres:18-alpine
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
      bash .github/scripts/pull-base-images.sh \
        --image mirror.gcr.io/library/postgres:18-alpine@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5
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
      deploy/teagram-demo/test-demo-compose.sh
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
    'Lint deployment, performance, and real-client workflows')
      go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
      actionlint -ignore 'label "ubuntu-26.04" is unknown' \
        .github/workflows/deploy-qa-invite.yml \
        .github/workflows/deploy-demo-target.yml \
        .github/workflows/deploy-dev-target.yml \
        .github/workflows/perf.yml \
        .github/workflows/real-client-browser-flow.yml
      # actionlint v1.7.12 predates GitHub's concurrency.queue key.
      # The deploy and QA contracts validate the permitted value and location.
      actionlint -ignore 'label "ubuntu-26.04" is unknown' \
        -ignore '^unexpected key "queue" for "concurrency" section' \
        .github/workflows/deploy-demo.yml .github/workflows/deploy.yml .github/workflows/qa-bot.yml
      actionlint -shellcheck= -ignore 'label "ubuntu-26.04" is unknown' .github/workflows/ci.yml
      node --check test/e2e/real_server_fixture/run_real_client_browser_flow.mjs
      ;;
    *)
      printf 'unknown deployment workflow check: %s\n' "$1" >&2
      return 64
      ;;
  esac
}

format_duration() {
  local milliseconds=$1
  printf '%d.%03ds' "$((milliseconds / 1000))" "$((milliseconds % 1000))"
}

validate_worker_assignments() {
  local task_index
  local -A seen=()
  for task_index in \
    "${WORKER_1_TASKS[@]}" "${WORKER_2_TASKS[@]}" "${WORKER_3_TASKS[@]}" \
    "${WORKER_4_TASKS[@]}" "${WORKER_5_TASKS[@]}"; do
    [[ $task_index =~ ^[0-9]+$ && $task_index -lt ${#CHECKS[@]} ]] || {
      printf 'invalid deploy workflow task index: %s\n' "$task_index" >&2
      return 1
    }
    [[ ! ${seen[$task_index]+present} ]] || {
      printf 'deploy workflow task assigned more than once: %s\n' "${CHECKS[$task_index]}" >&2
      return 1
    }
    seen[$task_index]=1
  done
  [[ ${#seen[@]} -eq ${#CHECKS[@]} ]] || {
    printf 'worker assignments cover %d of %d deploy workflow checks\n' \
      "${#seen[@]}" "${#CHECKS[@]}" >&2
    return 1
  }
}

run_worker() {
  local worker=$1 requested_task=${2:-} task_index task_name task_log task_status_file
  local started_ns finished_ns elapsed_ms task_status task_process_status
  local requested_task_assigned=0 failed=0
  local -a task_indices=() summary_rows=()
  case "$worker" in
    1) task_indices=("${WORKER_1_TASKS[@]}") ;;
    2) task_indices=("${WORKER_2_TASKS[@]}") ;;
    3) task_indices=("${WORKER_3_TASKS[@]}") ;;
    4) task_indices=("${WORKER_4_TASKS[@]}") ;;
    5) task_indices=("${WORKER_5_TASKS[@]}") ;;
    *) printf 'worker must be between 1 and %d\n' "$WORKER_COUNT" >&2; return 64 ;;
  esac
  validate_worker_assignments || return $?
  if [[ -n $requested_task ]]; then
    [[ $requested_task =~ ^[0-9]+$ ]] || {
      printf 'task index must be a non-negative integer: %s\n' "$requested_task" >&2
      return 64
    }
    for task_index in "${task_indices[@]}"; do
      if [[ $task_index == "$requested_task" ]]; then
        requested_task_assigned=1
      fi
    done
    [[ $requested_task_assigned == 1 ]] || {
      printf 'task index %s is not assigned to worker %s\n' "$requested_task" "$worker" >&2
      return 64
    }
    task_indices=("$requested_task")
  fi
  local worker_dir="${DEPLOY_CHECK_POOL_DIR:?worker pool directory is required}/worker-$worker"
  mkdir -p "$worker_dir"
  exec 3>&2
  exec >"$worker_dir/output.log" 2>&1
  cd "$REPO_ROOT"

  for task_index in "${task_indices[@]}"; do
    task_name=${CHECKS[$task_index]}
    task_log="$worker_dir/$task_index.log"
    task_status_file="$worker_dir/$task_index.status"
    rm -f -- "$task_log" "$task_status_file"
    started_ns=$(date +%s%N)
    printf 'worker %s: start %s\n' "$worker" "$task_name" >&3
    # Foreground execution preserves signal handling; the task shell keeps
    # errexit active inside run_check and records its check status separately.
    DEPLOY_CHECK_POOL_DIR="$worker_dir" bash "$SCRIPT_DIR/run-deploy-workflow-checks.sh" --task "$task_index"
    task_process_status=$?
    finished_ns=$(date +%s%N)
    elapsed_ms=$(((finished_ns - started_ns) / 1000000))
    if [[ -f $task_status_file ]]; then
      IFS=$'\t' read -r task_status _ _ < "$task_status_file"
    else
      task_status=$task_process_status
      [[ $task_status != 0 ]] || task_status=1
    fi
    if [[ $task_status == 0 && $task_process_status != 0 ]]; then
      task_status=$task_process_status
    fi
    if [[ $task_status != 0 ]]; then
      failed=1
    fi
    printf '\n===== %s =====\n' "$task_name"
    [[ ! -s $task_log ]] || cat "$task_log"
    if [[ $task_status == 0 ]]; then
      printf 'worker %s: PASS %s (%s)\n' \
        "$worker" "$task_name" "$(format_duration "$elapsed_ms")" >&3
      printf 'PASS: %s (%s)\n' "$task_name" "$(format_duration "$elapsed_ms")"
      summary_rows+=("| $task_name | $(format_duration "$elapsed_ms") | passed |")
    else
      printf 'worker %s: FAILED %s (exit %s, %s)\n' \
        "$worker" "$task_name" "$task_status" "$(format_duration "$elapsed_ms")" >&3
      printf 'FAILED: %s (exit %s, %s)\n' \
        "$task_name" "$task_status" "$(format_duration "$elapsed_ms")"
      summary_rows+=("| $task_name | $(format_duration "$elapsed_ms") | failed |")
    fi
  done

  {
    printf '\n### Deploy workflow checks: worker %s/%s\n\n' "$worker" "$WORKER_COUNT"
    printf '| Check | Worker time | Result |\n| --- | ---: | --- |\n'
    printf '%s\n' "${summary_rows[@]}"
  } > "$worker_dir/summary.md"
  printf '%s\n' "$failed" > "$worker_dir/status"
  return "$failed"
}

if [[ "${1:-}" == '--check-groups' ]]; then
  [[ $# -eq 1 ]] || { printf 'usage: %s --check-groups\n' "${BASH_SOURCE[0]}" >&2; exit 64; }
  validate_worker_assignments
  printf '%d workers cover all %d deploy workflow checks exactly once\n' \
    "$WORKER_COUNT" "${#CHECKS[@]}"
  exit 0
fi

if [[ "${1:-}" == '--worker' ]]; then
  if [[ $# -eq 2 ]]; then
    if run_worker "$2"; then
      exit 0
    else
      exit 1
    fi
  elif [[ $# -eq 4 && $3 == '--task-index' ]]; then
    if run_worker "$2" "$4"; then
      exit 0
    else
      exit 1
    fi
  else
    printf 'usage: %s --worker <1-%d> [--task-index <index>]\n' \
      "${BASH_SOURCE[0]}" "$WORKER_COUNT" >&2
    exit 64
  fi
fi

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
export DEPLOY_CHECK_LOCK_DIR=$pool_dir
pool_started_ns=$(date +%s%N)
validate_worker_assignments
xargs_status=0
if {
  for ((worker = 1; worker <= WORKER_COUNT; worker++)); do
    printf '%s\n' "$worker"
  done
} | xargs -r -n 1 -P "$WORKER_COUNT" \
  bash "$SCRIPT_DIR/run-deploy-workflow-checks.sh" --worker; then
  xargs_status=0
else
  xargs_status=$?
fi
pool_finished_ns=$(date +%s%N)
pool_elapsed_ms=$(((pool_finished_ns - pool_started_ns) / 1000000))
pool_failed=0
for ((worker = 1; worker <= WORKER_COUNT; worker++)); do
  worker_dir="$pool_dir/worker-$worker"
  printf '\n===== Worker %d/%d =====\n' "$worker" "$WORKER_COUNT"
  if [[ ! -f $worker_dir/status ]]; then
    printf 'FAILED: worker %d did not report a result\n' "$worker"
    pool_failed=1
    continue
  fi
  [[ ! -s $worker_dir/output.log ]] || cat "$worker_dir/output.log"
  [[ ! -s $worker_dir/summary.md || -z ${GITHUB_STEP_SUMMARY:-} ]] || cat "$worker_dir/summary.md" >> "$GITHUB_STEP_SUMMARY"
  worker_status=$(<"$worker_dir/status")
  if [[ $worker_status != 0 ]]; then
    printf 'FAILED: worker %d reported status %s\n' "$worker" "$worker_status"
    pool_failed=1
  fi
done

if ((xargs_status != 0)); then
  printf 'FAILED: xargs worker pool exited with status %s\n' "$xargs_status"
  pool_failed=1
fi

printf '\nDeployment workflow checks used %d fixed workers; pool wall time %s.\n' \
  "$WORKER_COUNT" "$(format_duration "$pool_elapsed_ms")"
if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
  {
    printf '\n### Deploy workflow worker pool\n\n'
    printf '| %d-worker pool wall time | %s | %s |\n' \
      "$WORKER_COUNT" "$(format_duration "$pool_elapsed_ms")" \
      "$([[ $pool_failed == 0 ]] && printf passed || printf failed)"
  } >> "$GITHUB_STEP_SUMMARY"
fi

exit "$pool_failed"
