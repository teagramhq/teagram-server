# Offline rollout runner

This runner makes the later, separately authorized telegramd rollout a single
locked operation. Preparing and testing it does not deploy anything. Invoke
`rollout-runner.sh apply TARGET_SHA EXPECTED_BASELINE_SHA` only from the live
`/opt/telegram-server` checkout after its `## Changes` record is current and
the exact target has been authorized.

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

The runner builds `telegramd` with Compose and reads
`docker image inspect telegramd:local --format '{{.Id}}'` before any `up`.
The source SHA and full image ID are written together to a new, synced evidence
file. The target snapshot is compared with the pinned image ID using the exact
approved verifier. The exact approved schema gate then records the migration
60-62, constraints, defaults and empty-files checks. Bounded readiness requires
advertise output, no server errors, migrate exit 0, healthy Postgres, TCP
connectivity and verified TLS.

Every run reserves distinct mode-0700 baseline, backup, build, target and
rollback evidence directories directly under `/root`. Files are mode 0600,
created without overwriting existing paths, and synced before a gate can accept
them. Snapshots retain only reviewed metadata and digests. The verifier and
schema gate are SHA-256 pinned to their reviewed artifacts and rechecked before
work starts.

If the target comparison, readiness or schema gate rejects, the runner first
persists the target failure marker, then resets the checkout to the captured
baseline and tags the captured baseline image as `telegramd:local`. It
recreates only `telegramd` with `--no-build --no-deps`, compares all preservation fields
to the original snapshot, and runs separate bounded rollback readiness. The
rollback comparison uses equality for the baseline values, including valid
`unset`/`unset` replica settings; it does not apply target-only defaults to the
rollback. The runner never automatically retries the target. A production
database restore is never automatic: assess migration compatibility before
considering restore. The isolated dump restore above only validates the backup
in a temporary, disconnected container.

Offline fixtures execute the actual runner with mocked `git`, Docker, lock,
network probes and persistence commands. They cover target-vs-built image
identity, old and stale IDs, missing/malformed IDs, baseline-equivalent
unset/unset rollback, unrelated config drift, evidence collisions, permission
failure, bounded target readiness and rollback readiness. They do not contact
the live LXC, use credentials, restore a real database, or run browser probes.

```sh
bash -n deploy/telegramd/rollout-runner/*.sh
bash deploy/telegramd/rollout-runner/test-rollout-runner.sh
```
