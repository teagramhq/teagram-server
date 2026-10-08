# Offline rollout runner

This runner makes the later, separately authorized telegramd rollout a single
locked operation. Preparing and testing it does not deploy anything. Keep the
runner and its two gate scripts outside the live checkout in the root-only
`/root/telegramd-rollout-runner` directory. This matters when the live baseline
predates these files: an untracked copy inside the checkout would block its
fast-forward, while staging the files would make the checkout dirty. The
runner checks each external file against the corresponding blob in the exact
authorized target before taking a backup.

After the target has been authorized, stage its runtime files and invoke the
runner from the live checkout:

```sh
TARGET_SHA=<authorized-full-commit-sha>
EXPECTED_BASELINE_SHA=<live-checkout-commit-sha>
sudo git -C /opt/telegram-server fetch -q origin main
sudo mkdir -m 700 /root/telegramd-rollout-runner
sudo env TARGET_SHA="$TARGET_SHA" bash -c '
  set -eu
  for name in rollout-runner.sh rollout-verifier.sh schema-result-gate.sh; do
    git -C /opt/telegram-server show \
      "$TARGET_SHA:deploy/telegramd/rollout-runner/$name" \
      > "/root/telegramd-rollout-runner/$name"
    chmod 600 "/root/telegramd-rollout-runner/$name"
  done
'
cd /opt/telegram-server
sudo bash /root/telegramd-rollout-runner/rollout-runner.sh apply \
  "$TARGET_SHA" "$EXPECTED_BASELINE_SHA"
```

Invoke only after the live checkout's `## Changes` record is current and the
exact target has been authorized. The destination directory must not already
exist; this prevents stale copies from being reused accidentally.

## Supported scope

PR #462 supports the ordinary local `tgblobs` backend only. It allows the
documented `TG_REPLICA_COUNT=1` and `TG_CLIENT_ADDR_TRUST=socket` transition;
the other resolved Compose configuration, `.env`, override and mount state
must remain identical to the baseline. RustFS services, S3 settings or secrets,
and the media mount added by #450 intentionally fail that preservation check.
A RustFS transition needs a separate reviewed cutover with a checksum-verified
copy and reverse-restore path. This runner has no digest waiver and accepts
only the authorized `origin/main` target from the `main` checkout.

The runner waits on the same lock used by the deployment skill:
`exec 9>/tmp/telegram-server-deploy.lock; flock -x 9`. Under that lock it
checks that the checkout is `main`, its current SHA is the expected baseline,
and fetched `origin/main` is exactly the requested target. It repeats the
`origin/main` check after the backup/restore proof and immediately before the
fast-forward.

It records allowlisted baseline metadata first, then makes a fresh mode-0600
Postgres dump. The dump must contain PostgreSQL's completion marker and is
restored into a temporary container made from the running Postgres image. That
container has `--network none`, no host mounts or published ports, and only a
temporary data tmpfs. Its cleanup must succeed before deployment continues.
The dump and restore diagnostics stay in root-only evidence.

After the fast-forward, it resolves Compose again while the baseline is still
running and persists a preflight comparison. The only admitted configuration
delta is `TG_REPLICA_COUNT=1` and `TG_CLIENT_ADDR_TRUST=socket`; the canonical
non-exempt Compose digest, `.env`, override and grace must match the baseline.
This catches service, overlay, binding, volume or unrelated environment drift
before building or replacing a container.

Before the backup and again immediately before the build, it rejects staged or
unstaged tracked edits and any untracked or ignored files under the Docker
source inputs (`cmd/`, `internal/`, `components/`, `utils/`). Rejection leaves
operator files in place and prevents a build from consuming source absent from
the authorized target SHA.

The runner builds `telegramd` with Compose and reads
`docker image inspect telegramd:local --format '{{.Id}}'` before any `up`.
The source SHA and full image ID are written together to a new, synced evidence
file. The target snapshot is compared with the pinned image ID using the exact
approved verifier. After readiness, the exact approved schema gate records the
post-migration result separately from the earlier Compose preflight. It
requires exactly Atlas revisions 60-65, the file metadata, trusted-reply and
poll-description-entity schemas, exact dialog-pin and cloud-draft key columns,
and an empty `files` table.
Bounded readiness requires
advertise output, no server errors, migrate exit 0, healthy Postgres, TCP
connectivity and verified TLS. `ROLLOUT_RUNNER_READY_SECONDS` bounds both target
and rollback polling from 1 to 120 seconds; the verifier gives only a two-second
shutdown margin to persist a bounded-timeout result.

This runner and schema gate are approved for one cumulative rollout batch only:
migrations 60-65, the validated file, dialog-pin, cloud-draft and poll
description-entity schemas, and an empty `files` table. Any missing revision or
revision after 65 rejects. Do not
reuse the schema gate after this batch is deployed, after file rows exist, or
for a later migration set. A later batch needs its own reviewed schema
expectations, fixtures and pinned gate hash.

Every run reserves distinct mode-0700 baseline, backup, build, target and
rollback evidence directories directly under `/root`. Files are mode 0600,
created without overwriting existing paths, and synced before a gate can accept
them. Snapshots retain only reviewed metadata and digests. The verifier and
schema gate are copied into the baseline evidence directory, SHA-256 checked,
and run from those pinned copies after the checkout fast-forward. The runner
also re-executes its own pinned copy before changing the checkout.

If the target comparison, readiness or schema gate rejects, the runner tries to
persist the target failure marker, then resets the checkout to the captured
baseline and tags the captured baseline image as `telegramd:local` even if that
marker write fails. Missing-marker failures are reported to stderr and recorded
in rollback evidence when possible. It
recreates only `telegramd` with `--no-build --no-deps`, compares all preservation fields
to the original snapshot, and runs separate bounded rollback readiness. The
rollback comparison uses equality for the baseline values, including valid
`unset`/`unset` replica settings; it does not apply target-only defaults to the
rollback. The runner never automatically retries the target. A production
database restore is never automatic: assess migration compatibility before
considering restore. The isolated dump restore above only validates the backup
in a temporary, disconnected container.

Offline fixtures execute the actual runner with mocked `git`, Docker, lock,
network probes and persistence commands, including the production verifier and
schema-gate command path under root-owned temporary evidence. Fixture mode only
substitutes the checkout and lock paths; it does not bypass the production
snapshot, comparison, schema, readiness, uid, evidence ownership or private-input
checks. They cover target-vs-built image identity, old and stale IDs,
missing/malformed IDs, baseline-equivalent unset/unset rollback, unrelated config
drift, evidence collisions, permission and failure-marker persistence failures,
bounded target and rollback readiness, failed-log unknowns, and checkout rewrites
after runtime pinning. They also reject tracked and untracked build-input edits
before backup and before build, and reject a missing applied revision for each
approved migration 60-65 and an extra revision, an invalid dialog-pin schema,
and a draft-sync primary key with the wrong columns. The mocked database
answers every revision check from a modeled applied-revision set keyed on the
version each gate predicate names, and the suite asserts the gate SQL names all
six approved revision IDs exactly. They do not contact the live LXC, use
credentials, restore a real database, or run browser probes. The fixture suite
runs in CI as root
because production evidence checks require root-owned paths.

```sh
sudo env TMPDIR=/root bash -n deploy/telegramd/rollout-runner/*.sh
sudo env TMPDIR=/root bash deploy/telegramd/rollout-runner/test-rollout-runner.sh
```
