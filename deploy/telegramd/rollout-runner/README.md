# Offline rollout runner

The runner prepares an inspected local baseline, publishes generation 1 of the
durable blob-mode authority, and only then starts the guarded local Compose
target. Every operation holds the shared deployment lock. The exact application
target and reviewed runtime-source commit must both be full commits reachable
from `origin/main`; the runner fast-forwards only to the selected application
target. It preserves the existing backup, build identity, readiness,
selected-revision Atlas schema, and rollback gates.

This stage has one publisher: `initialize-local` for the initial local record.
Ordinary `apply` validates the existing head against every running `telegramd*`
container and the proposed Compose render before build or replacement. A
rollout cannot change backend. There is no fresh-S3 or recovered-local
publisher in this stage, and no manual record-writing path. A default S3 render
without a matching authority intentionally fails closed.

## First guarded local rollout

Before using the runner, take the usual pre-deploy Postgres dump. The runner
also creates and restores a fresh Postgres dump before replacing the service.
Whole-LXC snapshots and restore drills are not prerequisites for this
readiness stage.

The fixed application target for this initial-local rollout is
`777742cc4b3ab0fda6b504a82b314a90aa60918b`. Use the reviewed tool-source commit
that contains this runner fix separately. The target must be an ancestor of
both `origin/main` and the tool-source commit, and the live baseline must be an
ancestor of the target. The standalone Compose artifact is pinned to this
application target and contains only Postgres, migration, and local-backed
`telegramd` services. It does not render or mount RustFS credentials. The
existing deployment override follows it in `COMPOSE_FILE`, preserving the
deployment's bindings and exposure.

```sh
TARGET_SHA=777742cc4b3ab0fda6b504a82b314a90aa60918b
TOOL_SHA=<reviewed-full-tool-source-commit-sha>
EXPECTED_BASELINE_SHA=$(sudo git -C /opt/telegram-server rev-parse HEAD)

sudo git -C /opt/telegram-server fetch -q origin main
sudo git -C /opt/telegram-server cat-file -e "$TARGET_SHA^{commit}"
sudo git -C /opt/telegram-server cat-file -e "$TOOL_SHA^{commit}"
sudo git -C /opt/telegram-server merge-base --is-ancestor "$TARGET_SHA" origin/main
sudo git -C /opt/telegram-server merge-base --is-ancestor "$TOOL_SHA" origin/main
sudo git -C /opt/telegram-server merge-base --is-ancestor "$TARGET_SHA" "$TOOL_SHA"
sudo mkdir -m 700 -p /root/telegramd-rollout-runner
sudo env TOOL_SHA="$TOOL_SHA" bash -c '
  set -eu
  umask 077
  [[ "$TOOL_SHA" =~ ^[0-9a-f]{40}$ ]]
  for name in rollout-runner.sh rollout-verifier.sh schema-result-gate.sh schema-result-gate.py blob-mode-state.py; do
    git -C /opt/telegram-server show \
      "$TOOL_SHA:deploy/telegramd/rollout-runner/$name" \
      > "/root/telegramd-rollout-runner/$name"
    chmod 600 "/root/telegramd-rollout-runner/$name"
  done
  git -C /opt/telegram-server show \
    "$TOOL_SHA:deploy/telegramd/rollout-runner/initial-local-compose-777742.yml" \
    > /opt/telegram-server/.rollout-compose.initial-local.yml
  printf "%s  %s\n" \
    3a4f158c6e1f2ead6676fba85d8d95cfb15557a0fbd8e82230361e0af988e0f7 \
    /opt/telegram-server/.rollout-compose.initial-local.yml | sha256sum --check
  chmod 600 /opt/telegram-server/.rollout-compose.initial-local.yml
'
cd /opt/telegram-server
COMPOSE_FILE=.rollout-compose.initial-local.yml
if [ -e docker-compose.override.yml ] || [ -L docker-compose.override.yml ]; then
  COMPOSE_FILE="$COMPOSE_FILE:docker-compose.override.yml"
fi
sudo env ROLLOUT_RUNNER_SOURCE_SHA="$TOOL_SHA" COMPOSE_FILE="$COMPOSE_FILE" \
  bash /root/telegramd-rollout-runner/rollout-runner.sh initialize-local \
  "$TARGET_SHA" "$EXPECTED_BASELINE_SHA"
```

The artifact digest is pinned in `rollout-runner.sh` and rechecked before any
backup or publication. The runner records the runtime source revision and
artifact SHA-256 in the private `runtime-pins.txt` evidence file. It rejects
extra Compose files, a missing override, an unpinned artifact, missing required
PostgreSQL/public-origin inputs, or any source commit that is not reviewed on
`origin/main` before capturing the live stack. The runner verifies its five
root-only runtime files against `TOOL_SHA`, then advances the application
checkout only to `TARGET_SHA`; later application commits and migrations remain
outside this rollout.

The local artifact gives `telegramd` a writable `tgblobs` volume, retains the
existing `tgkey` and `pgdata` volumes, keeps migration completion as a startup
dependency, mounts the runner-published authority read-only, and preserves the
120-second grace period. It includes no RustFS/init/migration/restore service,
dependency, or S3 credential secret. The pinned runner selection remains in
force through preflight, build, startup, readiness, and rollback.

This artifact is scoped to target `777742cc4b3ab0fda6b504a82b314a90aa60918b`.
Do not advance this target or reuse its fixed render for a different app SHA.

## Guarded local rollout for 0828cbb

The next local-backed application target is fixed at
`0828cbb2037844ce78695eee9fba3f52f82bdf91`. Run it only after the
`777742` deployment has completed and produced a valid local blob-mode
authority. This stage uses ordinary `apply`; it does not initialize or rewrite
the authority. The ordinary pre-deploy Postgres dump and restore checks remain
in force; whole-LXC snapshots and `rustfsdata` restore drills are not readiness
prerequisites.

Keep `TARGET_SHA` fixed even if the separately reviewed runner and artifact
land in a later source commit. `TOOL_SHA` must be that reviewed full source
commit, reachable from `origin/main` and descending from `TARGET_SHA`. The
expected baseline is the live checkout head. Stage the five root-only runner
files and the target-specific artifact from `TOOL_SHA`; the runner verifies
their bytes against that source revision. The Compose selection must contain
only the pinned artifact followed by the existing override. The runner rejects
an absent override, wrong artifact, mismatched target, or local authority,
backend, or volume mismatch before it creates a database dump. It records the
target, reviewed source, expected baseline, and artifact digest in private
`runtime-pins.txt` evidence. The later schema, backup restore, build, readiness,
and rollback gates remain in force.

```sh
TARGET_SHA=0828cbb2037844ce78695eee9fba3f52f82bdf91
TOOL_SHA=<reviewed-full-tool-source-commit-sha>
EXPECTED_BASELINE_SHA=$(sudo git -C /opt/telegram-server rev-parse HEAD)

sudo git -C /opt/telegram-server fetch -q origin main
sudo git -C /opt/telegram-server cat-file -e "$TARGET_SHA^{commit}"
sudo git -C /opt/telegram-server cat-file -e "$TOOL_SHA^{commit}"
sudo git -C /opt/telegram-server merge-base --is-ancestor "$TARGET_SHA" origin/main
sudo git -C /opt/telegram-server merge-base --is-ancestor "$TOOL_SHA" origin/main
sudo git -C /opt/telegram-server merge-base --is-ancestor "$TARGET_SHA" "$TOOL_SHA"
sudo mkdir -m 700 -p /root/telegramd-rollout-runner
sudo env TOOL_SHA="$TOOL_SHA" bash -c '
  set -eu
  umask 077
  [[ "$TOOL_SHA" =~ ^[0-9a-f]{40}$ ]]
  for name in rollout-runner.sh rollout-verifier.sh schema-result-gate.sh schema-result-gate.py blob-mode-state.py; do
    git -C /opt/telegram-server show \
      "$TOOL_SHA:deploy/telegramd/rollout-runner/$name" \
      > "/root/telegramd-rollout-runner/$name"
    chmod 600 "/root/telegramd-rollout-runner/$name"
  done
  git -C /opt/telegram-server show \
    "$TOOL_SHA:deploy/telegramd/rollout-runner/local-compose-0828cbb.yml" \
    > /opt/telegram-server/.rollout-compose.local-0828cbb.yml
  printf "%s  %s\n" \
    ecac480969bc5b6f1c7e115dfc6bc9033d6d665dc191c8b304e9351fa14d17f1 \
    /opt/telegram-server/.rollout-compose.local-0828cbb.yml | sha256sum --check
  chmod 600 /opt/telegram-server/.rollout-compose.local-0828cbb.yml
'
cd /opt/telegram-server
COMPOSE_FILE=.rollout-compose.local-0828cbb.yml:docker-compose.override.yml
sudo env ROLLOUT_RUNNER_SOURCE_SHA="$TOOL_SHA" COMPOSE_FILE="$COMPOSE_FILE" \
  bash /root/telegramd-rollout-runner/rollout-runner.sh apply \
  "$TARGET_SHA" "$EXPECTED_BASELINE_SHA"
```

For an application target after `0828cbb`, first supply a separately reviewed
local Compose artifact bound to that exact target. Do not reuse either the
`777742` or `0828cbb` artifact. Then stage the next target's five runtime files
and set `COMPOSE_FILE` to its artifact followed by the existing override. Set
`EXPECTED_BASELINE_SHA` to the current live checkout head:

```sh
TARGET_SHA=<next-reviewed-full-commit-sha>
EXPECTED_BASELINE_SHA=$(sudo git -C /opt/telegram-server rev-parse HEAD)
COMPOSE_FILE=<next-reviewed-local-artifact-and-override-selection>
sudo git -C /opt/telegram-server fetch -q origin main
sudo env TARGET_SHA="$TARGET_SHA" bash -c '
  set -eu
  for name in rollout-runner.sh rollout-verifier.sh schema-result-gate.sh schema-result-gate.py blob-mode-state.py; do
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

Before building or starting the target, the schema gate validates the selected
checkout's migration files and atlas.sum, then accepts only a complete ordered
prefix of those revisions in the database. After readiness, it requires the
exact complete successful revision set. Both checks use the read-only migrate
mount and persist private per-revision evidence. The verifier never applies,
repairs, or retries migrations.

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

The ordinary rollout runner still initializes and rolls out local storage
only. `blob-transition-runner.py accept-s3` exercises the cutover sequence with
a root-owned pre-copy bundle beneath the report root. Under the shared lock it
verifies the current local render against local authority, stops every configured
`telegramd*` writer, and checks that no writers remain before it recaptures the
Postgres dump, live Atlas/schema query, reference queries, and source-volume
census. The live Atlas observation records each applied revision's completion
counts, error and hash; every row must be complete, error-free and match its
pinned hash. The schema observation is also checked against pinned schema
evidence and recorded in `migrations.json` with the matching dump SHA-256. The
fresh dump and reference bytes must match the provenance-bound bundle before
the pre-copy gate can pass. Each
attempt copies the immutable inputs into its own private work bundle, so a retry
after interruption receives a new attempt ID and starts with fresh phase outputs.
The gate also checks exact baseline provenance and the candidate render, and
the runner inspects all running host containers after freeze, rejecting any
remaining `telegramd*` writer or writable source-volume mount. Surviving project
service names must match the frozen inventory before RustFS startup. It makes two
verified copies with an independent destination census after each, runs the
full qualification gate, publishes the synced
report/journal/head, then starts `telegramd*` without rerunning migrations. A
pre-publication rejection leaves local authority and evidence in place and
resumes the local serving services. An interruption after journal publication
follows the same `reconcile` rules above. Activation failures after S3
publication do not roll back to local.

`blob-transition-runner.py recover-local` consumes a separate root-owned
recovery bundle containing `recovery.json`, `frozen-containers.json`,
`postgres.dump`, and `migrations.json`. The qualification record binds the
fresh dump and schema evidence to the freeze and retained `tgblobs` volume.
Under the shared lock the runner verifies the current S3 and proposed local
renders against authority, stops all `telegramd*` writers, checks that the
surviving project service names match the frozen inventory, then captures a
fresh Postgres dump, live Atlas/schema query, and reference queries against the
stopped deployment. The migration schema capture is bound to the dump digest;
the reference rows must be covered by the fresh S3 census. It then records two
fresh S3 censuses, a local pre-restore census, two verified restores, two local
censuses, and the retained local-only key set. It syncs the evidence and publishes `recovered-local` before starting
the local serving services. A pre-publication rejection resumes S3 under the
unchanged S3 authority; local start remains rejected until the restore proof is
published. The private recovery report publishes the aggregate retained-key
count and never claims physical erasure.

Both transition commands reject live execution while the MAIN-1418/1419/1420
60–67 release requirements remain pending. `BLOB_TRANSITION_TEST_MODE=1` alone
cannot enable a transition: root-only fixtures must also provide a private
0700 fixture root, keep every input and authority path inside it, and pin all
Docker calls to the synthetic fixture adapter. CI uses synthetic bundles and
mocked Docker commands only.
MAIN-1332's client/photo prerequisites still gate media acceptance.

The pre-copy bundle contains the inspected baseline, the complete writer-freeze
inventory, fresh verified dump, unchanged pre/post-freeze reference rows and
source census, and exact 60–66 schema evidence. Those inputs are immutable and
root-only. Copy and destination-census outputs must be absent when the runner
starts. Root-only synthetic fixtures exercise this contract; they do not
provision or copy production data.

`blob-mode-state.py` accepts `recovered-local` only when its private proof has
two equal fresh S3 censuses, two equal restore manifests, two equal local
censuses, and the exact retained-local-only key set. It publishes the aggregate
retained-key count. Restore evidence does not establish media acceptance:
MAIN-1332's client/photo prerequisites still gate that decision. MAIN-1418/1419/1420 also
remain required before any live post-67 transition. The transition runner never
applies migrations 63–67 or drops migration-67 indexes on rollback.

After a target failure, the runner re-inspects the currently running containers
before recreating the baseline. If their backend or volume does not match the
authority, it stops without replacing them.

The old `.state/blob-migration-complete` marker and
`migrate-local-blobs.sh` are retired. They must not be used to authorize a
backend or to skip the runner.

## Checks

The credential-free Compose fixture renders the pinned artifact with only
disposable PostgreSQL/public-origin inputs, then verifies missing ordinary
inputs reject. Its S3 control uses throwaway credentials and a temporary secret
file to confirm the existing S3 render stays credential-gated and private.

The root-only fixture suite runs the actual runner with mocked Docker, Git,
locking, and persistence commands. It covers initialization, same-backend
replacement, report and volume binding, mount placement, read-only preservation,
ambiguous state, interrupted publication, and the existing rollout gates.
The RustFS transition CI job additionally runs the qualification gate, the
transition runner, and authority interruption fixtures as root.

The focused container-identity fixture sources the runner's inventory capture
function and checks both running and all-container enumeration against Docker's
12-character default quiet output. It also checks enumeration failures, empty
inventories, invalid IDs, a missing reference, and a mismatched reference.
Run it separately with:

```sh
sudo env TMPDIR=/root bash deploy/telegramd/rollout-runner/test-container-id-enumeration.sh
```

The Compose path preflight fixture checks relative, absolute, and normalized
selections from a checkout path containing spaces. It also rejects an omitted
existing override, missing or broken inputs, and a symlinked artifact. Run it on
GNU coreutils and on the pinned Alpine BusyBox runtime:

```sh
sudo env TMPDIR=/root bash deploy/telegramd/rollout-runner/test-compose-path-preflight.sh
docker run --rm \
  --volume "$PWD:/workspace:ro" \
  --workdir /workspace \
  node:24-alpine3.22@sha256:191c9f0080fcbbc6547a85dc0ff7988072214a355aabdc1d2ec55a7dae5eea8a \
  sh -ec '
    busybox realpath --help 2>&1 | grep -Fq "BusyBox v1.37.0"
    apk add --no-cache bash
    bash deploy/telegramd/rollout-runner/test-compose-path-preflight.sh
  '
```

```sh
bash -n deploy/telegramd/rollout-runner/*.sh
bash deploy/telegramd/rollout-runner/test-initial-local-compose.sh
bash deploy/telegramd/rollout-runner/test-local-compose-0828cbb.sh
python3 -B deploy/telegramd/rollout-runner/test-schema-result-gate.py
python -B deploy/telegramd/rollout-runner/test-blob-mode-state.py
sudo env TMPDIR=/root python3 -B deploy/telegramd/rollout-runner/test-blob-transition-runner.py
sudo env TMPDIR=/root bash deploy/telegramd/rollout-runner/test-rollout-runner.sh
```

## RustFS qualification release boundary

R69 (`60-69`) is exhausted by any row in `user_photos`,
`profile_photo_state`, `profile_upload_receipt`, or `profile_delete_operation`,
by a nonempty `files` table, or by migration 70 or later. The read-only
qualification rejects those states; it does not widen the census or repair the
schema. Ordinary rollout remains the only migration authority. The immutable
pins, closed catalog contract, capture commands and PostgreSQL 16 CI proof are
documented in `QUALIFICATION.md`.

Run the real PostgreSQL and Atlas qualification locally with:

```sh
sudo env "PATH=$PATH" TMPDIR=/root bash deploy/telegramd/rollout-runner/test-rustfs-schema-postgres.sh
```
