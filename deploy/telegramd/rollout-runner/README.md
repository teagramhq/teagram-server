# Offline rollout runner

The runner prepares an inspected local baseline, publishes generation 1 of the
durable blob-mode authority, and only then starts the guarded local Compose
target. Every operation holds the shared deployment lock. It accepts only a
full reviewed `origin/main` SHA on the `main` checkout and preserves the
existing backup, build identity, readiness, schema 60-65, and rollback gates.

This stage has one publisher: `initialize-local` for the initial local record.
Ordinary `apply` validates the existing head against every running `telegramd*`
container and the proposed Compose render before build or replacement. A
rollout cannot change backend. There is no fresh-S3 or recovered-local
publisher in this stage, and no manual record-writing path. A default S3 render
without a matching authority intentionally fails closed.

## First guarded local rollout

Before using the runner, take and verify the deployment's normal LXC snapshot
and record its restore identifier and path with the deployment work. The
runner also creates and restores a fresh Postgres dump before replacing the
service; that dump does not replace the LXC snapshot.

The target must be a reviewed revision whose `docker-compose.local-blobs.yml`
resolves to the local backend. Capture the live baseline SHA before fast-forward
and use the exact reviewed target SHA:

```sh
TARGET_SHA=<reviewed-full-commit-sha>
EXPECTED_BASELINE_SHA=$(sudo git -C /opt/telegram-server rev-parse HEAD)
COMPOSE_FILE=docker-compose.yml
if [ -e /opt/telegram-server/docker-compose.override.yml ] || [ -L /opt/telegram-server/docker-compose.override.yml ]; then
  COMPOSE_FILE="$COMPOSE_FILE:docker-compose.override.yml"
fi
COMPOSE_FILE="$COMPOSE_FILE:docker-compose.local-blobs.yml"

sudo git -C /opt/telegram-server fetch -q origin main
sudo mkdir -m 700 -p /root/telegramd-rollout-runner
sudo env TARGET_SHA="$TARGET_SHA" bash -c '
  set -eu
  for name in rollout-runner.sh rollout-verifier.sh schema-result-gate.sh blob-mode-state.py; do
    git -C /opt/telegram-server show \
      "$TARGET_SHA:deploy/telegramd/rollout-runner/$name" \
      > "/root/telegramd-rollout-runner/$name"
    chmod 600 "/root/telegramd-rollout-runner/$name"
  done
'
cd /opt/telegram-server
sudo env COMPOSE_FILE="$COMPOSE_FILE" \
  bash /root/telegramd-rollout-runner/rollout-runner.sh initialize-local \
  "$TARGET_SHA" "$EXPECTED_BASELINE_SHA"
```

For each reviewed target, stage all four runtime files from that exact target
into the fixed root-only source directory. The runner rejects stale runtime
copies by comparing all four files with the requested target.
`initialize-local` checks the live unguarded containers and baseline
render, validates that the target is guarded and still local, and publishes a
private synced report plus the journal/head before it builds or replaces
anything. The report binds the generation, transition ID, backend, inspected
containers, baseline SHA, and target SHA. The server bind is read-only and is
present on every rendered `telegramd*` service only.
When `docker-compose.override.yml` exists, keep it in `COMPOSE_FILE` before the
local overlay; the runner rejects an explicit file list that omits it before
creating evidence or capturing the live stack.

After initialization succeeds, stage the next target's four runtime files and
refresh both SHAs before each later same-backend rollout. Set
`EXPECTED_BASELINE_SHA` to the current live checkout head:

```sh
TARGET_SHA=<next-reviewed-full-commit-sha>
EXPECTED_BASELINE_SHA=$(sudo git -C /opt/telegram-server rev-parse HEAD)
sudo git -C /opt/telegram-server fetch -q origin main
sudo env TARGET_SHA="$TARGET_SHA" bash -c '
  set -eu
  for name in rollout-runner.sh rollout-verifier.sh schema-result-gate.sh blob-mode-state.py; do
    git -C /opt/telegram-server show \
      "$TARGET_SHA:deploy/telegramd/rollout-runner/$name" \
      > "/root/telegramd-rollout-runner/$name"
    chmod 600 "/root/telegramd-rollout-runner/$name"
  done
'
sudo env COMPOSE_FILE="$COMPOSE_FILE" \
  bash /root/telegramd-rollout-runner/rollout-runner.sh apply \
  "$TARGET_SHA" "$EXPECTED_BASELINE_SHA"
```

`apply` requires the existing journal and `mode.json` to agree. It checks the
full Compose volume names and effective backend from all running replicas,
including proxies, against both the authority and proposed render before
building. It rejects mount changes in Compose overrides, a changed volume name,
or any backend mismatch before `up`.

## Interrupted publication

Publication commits the synced report, then a non-overwriting journal entry,
then identical `mode.json` bytes. If the runner stops after the journal entry
is committed but before the mode head is replaced, both server validation and
ordinary rollout fail closed. With the exact reviewed target still checked out
on `main`, reconcile only that interrupted initial-local publication:

```sh
sudo env COMPOSE_FILE="$COMPOSE_FILE" \
  bash /root/telegramd-rollout-runner/rollout-runner.sh reconcile "$TARGET_SHA"
```

If `initialize-local` reports a failure after `journal/0000000001.json` was
committed, it leaves the reviewed target checked out and does not start it.
Keep that checkout at `TARGET_SHA`; resetting to the baseline strands the
committed authority. After `reconcile` succeeds, resume the guarded rollout
by rerunning the ordinary `apply` invocation with that same SHA as both the
target and expected baseline.

If the journal entry was not committed, the runner restores the baseline
checkout and image tag, so initialization can be retried after correcting the
cause.

Reconciliation rehashes and syncs the root-only report and accepts only a
matching journal/report pair. An uncommitted temporary entry is discarded by a
fresh `initialize-local` attempt and receives a new transition ID and report.
If the journal, report, or head is ambiguous or mismatched, leave all files and
containers unchanged and stop for review. Never edit or remove authority files
by hand.

## Backend transition boundary

This runner initializes and rolls out local storage only. It does not publish
`s3-accepted` or `recovered-local`, start a fresh S3 backend, copy media, restore
media, or remove the authority mount. Guardless rollback is available only for
the inspected initial-local baseline. S3 and recovered-local states require a
later reviewed orchestration path with the complete copy or restore proof; a
guardless target is rejected for those outcomes.

After a target failure, the runner re-inspects the currently running containers
before recreating the baseline. If their backend or volume does not match the
authority, it stops without replacing them.

The old `.state/blob-migration-complete` marker and
`migrate-local-blobs.sh` are retired. They must not be used to authorize a
backend or to skip the runner.

## Checks

The root-only fixture suite runs the actual runner with mocked Docker, Git,
locking, and persistence commands. It covers initialization, same-backend
replacement, report and volume binding, mount placement, read-only preservation,
ambiguous state, interrupted publication, and the existing rollout gates.

```sh
bash -n deploy/telegramd/rollout-runner/*.sh
python -B deploy/telegramd/rollout-runner/test-blob-mode-state.py
sudo env TMPDIR=/root bash deploy/telegramd/rollout-runner/test-rollout-runner.sh
```
