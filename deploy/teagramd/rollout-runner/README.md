# Offline rollout runner

The runner prepares an inspected local baseline, publishes generation 1 of the
durable blob-mode authority, and only then starts the guarded local Compose
target. Every operation holds the shared deployment lock. The exact application
target and reviewed runtime-source commit must both be full commits reachable
from `origin/main`; ordinary rollouts fast-forward only to the selected
application target. The first guarded local rollout has one pinned legacy
baseline/target pair on unrelated histories; after the verified backup, that
exact pair is reset to the reviewed target and can be restored to the baseline
on rollback. No other non-fast-forward transition is accepted. The runner
preserves the existing backup, build identity, readiness, selected-revision
Atlas schema, and rollback gates.

This rollout runner has one initializer: `initialize-local` publishes the
initial local record. Ordinary `apply` validates the existing head against
every running `teagramd*` container and the proposed Compose render before
build or replacement. A rollout cannot change backend. There is no fresh-S3 or
recovered-local publisher in this stage, and no manual record-writing path.
The separate `blob-encrypt-migrate` operator command is the only publisher of
`encrypted` authority, after its database-reference inventory passes. The
serving process has a separate fresh-S3 bootstrap for an empty authority
directory; it writes no authority and cannot bypass a nonempty record tree.
The rollout runner still requires a matching authority for every managed S3
render and transition.

## Encrypting accepted plaintext S3 media

The `blob-encrypt-migrate` profile is a one-time operator job for an existing
`s3-accepted` authority. It reads assembled-file rows and live upload-part rows
from PostgreSQL, streams each referenced plaintext object into the `encrypted/`
S3 namespace, authenticates a full readback, checks that the database inventory
did not change, and only then appends `encrypted` authority. It leaves source
objects and unreferenced strays untouched. The private progress journal,
inventory artifact, and authority report persist under `/root` by default and
survive interruption.

Run from the deployment checkout as root after choosing and recording the
actual write-quiescence state. `stopped` means every `teagramd` writer is
stopped. `drained` means writes were stopped at the request boundary and all
in-flight uploads finished. Keep writers quiescent until this command exits,
then restart every `teagramd` replica before allowing writes again so each
process loads the encrypted authority.

```sh
docker compose --profile migration run --rm blob-encrypt-migrate \
  --writes stopped \
  --progress /run/teagramd/reports/blob-encryption-progress.jsonl
```

If interrupted, rerun with the same `--writes` choice and journal path. The
command authenticates and reuses completed envelopes; it reports any stray
objects in its JSON result and never moves or deletes them. If an inventory
reference cannot be verified, the command names the affected IDs and keeps the
accepted authority active.

Plaintext removal is a separate opt-in operation after all replicas are
serving encrypted authority. Stop or drain writes again, then run:

```sh
docker compose --profile migration run --rm blob-encrypt-migrate \
  --writes stopped \
  --progress /run/teagramd/reports/blob-encryption-progress.jsonl \
  --cleanup
```

Use the same `--writes` value recorded for migration. Cleanup reauthenticates
each encrypted copy and compares it with the plaintext source before removing
that referenced source. It does not remove unreferenced objects. If changing
`TEAGRAM_BLOB_MODE_REPORT_ROOT`, set `ROLLOUT_RUNNER_EVIDENCE_ROOT` to the same
host directory so later authority validation reads the published report.

Preflight treats disabled public WebSocket settings and an empty
`TG_WELCOME_CHANNEL` as equivalent when they are unset or passed through as
empty Compose values. An active value for `TG_PUBLIC_WEBSOCKET_LISTEN_ADDR`,
`TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR`, `TG_PUBLIC_REGISTRATION`, or
`TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS` is accepted only when the operator names
that setting in `ROLLOUT_RUNNER_ADDED_ENV` and adds one assignment to the
verified `.env`. The resolved Compose value and replacement container must
match. `TG_PUBLIC_REGISTRATION` accepts `invite` or `closed`; `open` is always
rejected. Active Compose defaults without a named `.env` addition still fail
closed, as does any non-empty `TG_WELCOME_CHANNEL`. After an addition is
verified, ordinary rollouts retain its exact value without repeating the name.
Change a retained value only through `ROLLOUT_RUNNER_CHANGED_ENV` with the
previous `.env` backup. If a rollout that added public settings rolls back, the
runner restores that previous `.env` before recreating the previous container.

The public WebSocket replicas set `TG_REPLICA_COUNT=3` directly in their
Compose services. The base socket-trust `teagramd` keeps its `.env` value,
defaulting to `1`, so the split does not divide its process-local tailnet and
raw-TCP budgets. `deploy-main.sh` does not modify `.env` for this route; the
public topology checker rejects a base render whose count is not `1` before
target startup.

The public WebSocket route is applied with the `public` Compose profile and
`ROLLOUT_RUNNER_PUBLIC_WEBSOCKET=1`. `deploy-main.sh` takes the database dump
first. The public topology checker validates the isolated HAProxy frontend
and backend, the peer and header rules, the two proxy-v2 replicas, registration
and Origin policy, and their absence of host ports. It also requires the
bounded HAProxy healthcheck and logs. The runner checks HAProxy health, the
loopback-only publish, both replica environments and listener readiness after
startup.

If a deployed route must be reverted, free the public proxy's loopback 2446
publish before reverting. Read the current deployed SHA, then run the reviewed
teardown operation from that same commit:

```sh
set -eu
DEPLOYED_SHA=$(ssh teagram live | sed -n 's/^live_sha=//p')
[[ "$DEPLOYED_SHA" =~ ^[0-9a-f]{40}$ ]]
ssh teagram "run $DEPLOYED_SHA rollback-public-websocket $DEPLOYED_SHA"
```

The operation takes the shared deploy lock, verifies the live checkout and base
image revision, removes only the public containers in the `teagram` Compose
project, and confirms no container still publishes 2446. After it succeeds,
merge the route revert and let `Deploy.` recreate base `teagramd`; without this
step Compose retains the orphan HAProxy container and rejects the port binding.

The existing host tunnel keeps its `127.0.0.1:2446` origin; Compose now assigns
that publish to HAProxy, while the ordinary socket-trust service has no public
listener or 2446 publish. The public advertise address comes from the same
verified `.env` but is set only on the public replicas because the base process
rejects an advertise address when its public listener is disabled.

Snapshot normalization removes only the three reviewed public services and
their two fixed networks, the public-only environment settings, and the exact
loopback 2446 binding from the ordinary service's baseline digest. The separate
topology checker validates those removed fields in full. The base service's
public listener and 2446 publish must be absent after rollout. On failure, the
runner restores the previous checkout and `.env`, removes only containers
carrying this Compose project's public-service labels, recreates the prior
`teagramd`, and verifies baseline equivalence and readiness. The database dump
is retained at the path printed by `deploy-main.sh`; the previous `.env` is
retained as `/root/teagramd-env-before-<sha>-<timestamp>`.

The only approved `teagramd` logging change is the reviewed transition from
Compose's default logger to `syslog` at `/dev/log` with the `teagramd` tag,
daemon facility, and RFC5424 format. Preflight records that policy separately
from the general configuration digest; post-start inspection must confirm the
container received the exact logger settings. Other drivers and options remain
configuration drift.

The only approved login-code logging transition is `true` to `false`. The
runner records the Compose and inspected container settings separately and
checks both at preflight, target verification, and rollback. Login-code
settings are removed from the general Compose and container-environment hashes
only because their exact before and after values are compared separately.
Syslog targets require `TG_LOG_LOGIN_CODES=false`, so the retained sink cannot
be enabled while the legacy diagnostic emits phone numbers and codes.

### Retained log threat model

Application diagnostics are sensitive: they can include account identifiers
and request context. Do not log message bodies, passwords, tokens, or
authentication codes. `TG_LOG_LOGIN_CODES` is pinned to `false`; the
pre-syslog snapshot and the forced reader also discard legacy `login code
issued` records.

The QA SSH key can retrieve at most 5,000 matching `teagramd` lines through
the forced `/usr/local/sbin/teagram-logs` command. It cannot open a shell or
invoke Docker. Restrict that key to trusted responders and revoke it when it
is no longer needed. Host root and Docker administrators can read or copy the
raw files. The files are not encrypted at rest.

BusyBox syslog keeps a 512 MiB active `/var/log/messages` file and 15
rotations, for an 8 GiB maximum. This is a size limit, not an age limit; high
volume can evict records quickly, while low volume can leave them longer. A
normal deployment does not clear these files. Once the rotation set is full,
the oldest file is overwritten as new rotations are created. Host snapshots
and copies outside the LXC are outside this retention control.

Before replacing a pre-syslog container, the runner can preserve its last
seven days of logs, up to 5,000 lines, in the root-only
`/root/teagramd-legacy-logs.log`, capped at 32 MiB and mode `0600`. The
forced reader filters this file too. The snapshot is not age-expired: it
remains until a later archive update or an operator removes it. Filesystem
deletion does not establish cryptographic erasure or remove host-level copies.
The operational logging procedure and reader contract are documented in
[`../../teagram/README.md#retained-log-threat-model`](../../teagram/README.md#retained-log-threat-model).

For every local-backed rollout, read the running `teagramd` container's
`com.docker.compose.project.config_files` label and carry that Compose selection
forward unchanged. With ordinary local authority, it must start with a
`.rollout-compose.local-*` file and include the existing
`docker-compose.override.yml` after it. After a stack move publishes
`stack-moved-local` authority, the exact tracked
`docker-compose.yml:docker-compose.local-blobs.yml:docker-compose.override.yml`
selection is also accepted. The runner gets `TEAGRAMD_STACK_MOVE_BLOB_DIR` from
the validated authority record before rendering that selection; the rendered
backend and volume inventory must match the authority before backup and again
after fast-forward. This tracked selection has no per-commit artifact pin, so
every `main` commit remains eligible for deployment. A render mismatch still
rejects before backup. Use the same `COMPOSE_FILE` for preflight, build,
startup, verification, and rollback; never rely on the default S3 stack or omit
the override.

Ordinary `apply` leaves the migration profile disabled. The local overlay
keeps `teagramd` on the default network even when the base S3 Compose file adds
the isolated RustFS network; the blob transition runner validates that network
when performing a blob transition.

A later application target may carry only one of two reviewed Compose bodies.
The measured non-comment digest `d025a25d3e38…` selects
`local-compose-df9ffe5.yml`; `577dd9885aa9…` selects
`local-compose-cce00d1.yml`. The staged filename and header comment do not
select the contract. The one approved d025-to-577dd selection swap for
MAIN-1769 changes only the forwarded build revision; the existing override
and every other deployment invariant remain subject to the normal preflight.
For the 577dd body, the resolved build argument and context must match the
validated target checkout, and the built image revision label must match the
target before Compose can start it. A provenance failure restores the previous
checkout and `teagramd:local` tag. The d025 body keeps its historical label
contract, with the observed label recorded but not asserted.

Private rollout evidence records the reviewed artifact origin separately from
the actual target and tool-source revision, plus the full staged-file digest
and measured body digest. Changes to either reviewed body require a new
reviewed artifact and content pin before rollout.

## Compose project move

Ordinary `apply` compares the checked-out baseline Compose project and
persistent mounts with the target render before creating evidence, dumping the
database, building, or stopping a container. The only additional layout change
allowed during an ordinary rollout is the initial exact read-only bind from
`/etc/teagramd/media-keys` to `/run/teagramd/media-keys` on the existing
`teagramd` service. It requires `TG_MEDIA_KEY_CURRENT=1` and a matching
mode-0600 key file owned by uid/gid 65532 in a mode-0700 directory outside the
application checkout. The runner pins the host path and key version after that
first rollout; later changes remain blocked. Any project, volume, or other
mount change fails closed and asks for the explicit `move-stack` action.
`deploy-main.sh` continues to call `apply`, so a main-branch deployment that
contains the project rename exits before changing the running stack until the
move action is run. The normal readiness and rollback gates still apply when
adding the media-key mount.

The move action accepts only a reviewed fast-forward target on `origin/main`.
It takes both the old and new deploy locks, stages `/opt/teagram` as a worktree,
copies the private deployment inputs, remaps the private override's `telegramd`
service settings onto `teagramd`, and checks the old Compose project and every
target volume before stopping writers. An S3-authorized blob source is rejected
before stopping the old stack; bucket movement is tracked by MAIN-1977. The
move copies `tgkey`, `tgblobs`, and RustFS data while their writers are stopped,
restores `telegram` into PostgreSQL 18 with the target Compose password and
authentication defaults as the target Compose database, and starts `teagram`.
A failure after the old stack stops brings the old Compose project back up. The
four old volumes, the move phase's `pgdump-pre-*.sql` dump and `pgglobals.sql`
remain available until the MAIN-1979 sign-in check passes. A failed attempt
keeps its target worktree and any partially copied target volumes for
inspection; another attempt refuses those paths or nonempty volumes. After
MAIN-1979 passes, require `human-approval` before the irreversible cleanup that
deletes the four old `telegram-server_*` volumes, the move's dump and globals
file, and any failed attempt's partial `teagram_*` target volumes. This cleanup
prevents retained pre-cutover copies from exposing data or restoring data users
have erased. Once `teagram` has taken traffic, rolling back onto the old volumes
discards post-cutover writes and reverses erasures; that is not a routine
rollback and also requires `human-approval`.

Run this only for the reviewed commit that contains the target Compose layout:

```sh
TARGET_SHA=<reviewed-full-commit-sha>
SOURCE_PROJECT=telegram-server
SOURCE_CHECKOUT="/opt/$SOURCE_PROJECT"
EXPECTED_BASELINE_SHA=$(sudo git -C "$SOURCE_CHECKOUT" rev-parse HEAD)
sudo git -C "$SOURCE_CHECKOUT" fetch -q origin main
sudo git -C "$SOURCE_CHECKOUT" merge-base --is-ancestor "$TARGET_SHA" origin/main
cd "$SOURCE_CHECKOUT"
LIVE_CF=$(sudo docker inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' telegram-server-telegramd-1 | tr , :)
sudo env ROLLOUT_RUNNER_SOURCE_SHA="$TARGET_SHA" \
  COMPOSE_FILE="$LIVE_CF" \
  bash /root/teagramd-rollout-runner/rollout-runner.sh move-stack \
  "$TARGET_SHA" "$EXPECTED_BASELINE_SHA"
```

The move writes private evidence under the runner's target phase directory,
including the source and target table counts, role attributes, the unchanged
PostgreSQL 16 volume inventory, per-file key and object checksums, and the
built image identity. The `move-stack` action is not
part of ordinary automatic deployment, and this repository change does not run
it against the live stack.

## First guarded local rollout

Before replacing the service, the runner creates a fresh Postgres dump and
verifies it with an isolated restore. This verified dump and restore are the
backup gate; whole-LXC snapshots and restore drills are not prerequisites for
this readiness stage.

The fixed application target for this initial-local rollout is
`777742cc4b3ab0fda6b504a82b314a90aa60918b`. Use the reviewed tool-source commit
that contains this runner fix separately. The target must be an ancestor of
both `origin/main` and the tool-source commit. For the approved live legacy
checkout, `EXPECTED_BASELINE_SHA` must be
`932994e26a86eb1c9ad60f81b3d222b19d3f40b7`; only that baseline and this exact
target may use the one-time reset. The standalone historical Compose artifact
is pinned to this application target and retains its original server service
name. It contains only Postgres, migration, and local-backed services. It does
not render or mount RustFS credentials. The
existing deployment override follows it in `COMPOSE_FILE`, preserving the
deployment's bindings and exposure.

```sh
TARGET_SHA=777742cc4b3ab0fda6b504a82b314a90aa60918b
TOOL_SHA=<reviewed-full-tool-source-commit-sha>
EXPECTED_BASELINE_SHA=$(sudo git -C /opt/teagram rev-parse HEAD)

sudo git -C /opt/teagram fetch -q origin main
sudo git -C /opt/teagram cat-file -e "$TARGET_SHA^{commit}"
sudo git -C /opt/teagram cat-file -e "$TOOL_SHA^{commit}"
sudo git -C /opt/teagram merge-base --is-ancestor "$TARGET_SHA" origin/main
sudo git -C /opt/teagram merge-base --is-ancestor "$TOOL_SHA" origin/main
sudo git -C /opt/teagram merge-base --is-ancestor "$TARGET_SHA" "$TOOL_SHA"
sudo mkdir -m 700 -p /root/teagramd-rollout-runner
sudo env TOOL_SHA="$TOOL_SHA" bash -c '
  set -eu
  umask 077
  [[ "$TOOL_SHA" =~ ^[0-9a-f]{40}$ ]]
  for name in rollout-runner.sh rollout-verifier.sh schema-result-gate.sh schema-result-gate.py blob-mode-state.py qualify-rustfs-transition.py stack-move-runner.py; do
    git -C /opt/teagram show \
      "$TOOL_SHA:deploy/teagramd/rollout-runner/$name" \
      > "/root/teagramd-rollout-runner/$name"
    chmod 600 "/root/teagramd-rollout-runner/$name"
  done
  git -C /opt/teagram show \
    "$TOOL_SHA:deploy/teagramd/rollout-runner/initial-local-compose-777742.yml" \
    > /opt/teagram/.rollout-compose.initial-local.yml
  printf "%s  %s\n" \
    d764d291ad2ea91cff841d14a0cd2bbcb5981961dbc67406bfeba1196a7b3fa4 \
    /opt/teagram/.rollout-compose.initial-local.yml | sha256sum --check
  chmod 600 /opt/teagram/.rollout-compose.initial-local.yml
'
cd /opt/teagram
COMPOSE_FILE=.rollout-compose.initial-local.yml
if [ -e docker-compose.override.yml ] || [ -L docker-compose.override.yml ]; then
  COMPOSE_FILE="$COMPOSE_FILE:docker-compose.override.yml"
fi
sudo env ROLLOUT_RUNNER_SOURCE_SHA="$TOOL_SHA" COMPOSE_FILE="$COMPOSE_FILE" \
  bash /root/teagramd-rollout-runner/rollout-runner.sh initialize-local \
  "$TARGET_SHA" "$EXPECTED_BASELINE_SHA"
```

The artifact digest is pinned in `rollout-runner.sh` and rechecked before any
backup or publication. The runner records the runtime source revision and
artifact SHA-256 in the private `runtime-pins.txt` evidence file. It rejects
extra Compose files, a missing override, an unpinned artifact, missing required
PostgreSQL/public-origin inputs, or any source commit that is not reviewed on
`origin/main` before capturing the live stack. The runner verifies its seven
root-only runtime files against `TOOL_SHA`, then advances the application
checkout only to `TARGET_SHA`; later application commits and migrations remain
outside this rollout.

`initialize-local` records the running baseline from container inspection and the
desired guarded state from the pinned Compose render as separate evidence. Both
root-only captures are validated before the database dump; an existing
authority record or ambiguous state rejects initialization. The accepted mount
change is only the absent baseline authority bind becoming the exact read-only
target bind, and the authority report names each evidence source and the
artifact digest.

The local artifact gives `teagramd` a writable `tgblobs` volume, retains the
existing `tgkey` and `pgdata` volumes, keeps migration completion as a startup
dependency, mounts the runner-published authority read-only, and preserves the
120-second grace period. It includes no RustFS/init/migration/restore service,
dependency, or S3 credential secret. The pinned runner selection remains in
force through preflight, build, startup, readiness, and rollback.

This artifact is scoped to target `777742cc4b3ab0fda6b504a82b314a90aa60918b`.
Do not advance this target or reuse its fixed render for a different app SHA.

## Historical rollout: e58ba20

The next local-backed application target is fixed at
`e58ba203505a37bf5ad3181f22e59a934d4a7442`. Run it only after the
`777742` deployment has completed and produced a valid local blob-mode
authority. This stage uses ordinary `apply`; it does not initialize or rewrite
the authority. The runner's verified Postgres dump and isolated restore remain
the backup gate; whole-LXC snapshots and `rustfsdata` restore drills are not
readiness prerequisites.

The pinned Compose artifact is
`deploy/teagramd/rollout-runner/local-compose-e58ba20.yml`, staged at
`.rollout-compose.local-e58ba20.yml`. Its SHA-256 is
`d3426792677d0510a75dd254fbbd5fd9d9b19a01d710ec5b9b59f3e8a31447c7`.

Keep `TARGET_SHA` fixed even if the separately reviewed runner and artifact
land in a later source commit. `TOOL_SHA` must be that reviewed full source
commit, reachable from `origin/main` and descending from `TARGET_SHA`. The
expected baseline is the live checkout head. Stage the seven root-only runner
files and the target-specific artifact from `TOOL_SHA`; the runner verifies
their bytes against that source revision. The Compose selection must contain
only the pinned artifact followed by the existing override. The runner rejects
an absent override, wrong artifact, mismatched target, or local authority,
backend, or volume mismatch before it creates a database dump. It records the
target, reviewed source, expected baseline, and artifact digest in private
`runtime-pins.txt` evidence. The later schema, backup restore, build, readiness,
and rollback gates remain in force.

## Historical rollout: 598359e

The fixed application target is
`598359e900130c307dd84022b0077fd3276f6f36`; its expected live baseline is
`fb53e4fbded211adfb5bd26dd37f02ff7bfcc052`. Confirm that baseline during the
MAIN-1625 execution. This artifact uses the live baseline's image references;
it does not copy the default stack's mirror registry changes.

The committed artifact is
`deploy/teagramd/rollout-runner/local-compose-598359e.yml`, staged at
`.rollout-compose.local-598359e.yml`. Its SHA-256 is
`714d7f870645e2581234feba81a86e9e529facd0b664b32b2f429eb583dc754f`. The live
`.rollout-compose.local-fb53e4f.yml` was read at the expected baseline: its
SHA-256 is `0daed9674dac9041c2bf896657f34a44387e519f1adc0ee24f90fcb8bb4080c5`,
and the bytes after its two header comments hash to
`a0ac0c82d9bda6a7b032e7c81e8ef9d0e7cdf81b52edac2281fd6e03ea3343c4`. Those
bytes match both the reviewed e58ba20 artifact and this artifact; the only
artifact differences are the target header comments.

At the time of this rollout, local-backend apply required a committed artifact
pinned to the exact application target. The 598359e selection was that artifact
followed by the existing override. Initialization, S3 transition, and rollback
to the captured baseline keep their existing paths.

Keep `TARGET_SHA` fixed even though this tool and artifact land after the
application target. `TOOL_SHA` is the full reviewed source commit, reachable
from `origin/main` and descending from `TARGET_SHA`. Confirm the expected
baseline before staging the seven root-only runner files and target artifact:

```sh
TARGET_SHA=598359e900130c307dd84022b0077fd3276f6f36
TOOL_SHA=<reviewed-full-tool-source-commit-sha>
EXPECTED_BASELINE_SHA=fb53e4fbded211adfb5bd26dd37f02ff7bfcc052
ACTUAL_BASELINE_SHA=$(sudo git -C /opt/teagram rev-parse HEAD)
[ "$ACTUAL_BASELINE_SHA" = "$EXPECTED_BASELINE_SHA" ]
COMPOSE_FILE=.rollout-compose.local-598359e.yml:docker-compose.override.yml

sudo git -C /opt/teagram fetch -q origin main
sudo git -C /opt/teagram cat-file -e "$TARGET_SHA^{commit}"
sudo git -C /opt/teagram cat-file -e "$TOOL_SHA^{commit}"
sudo git -C /opt/teagram merge-base --is-ancestor "$TARGET_SHA" origin/main
sudo git -C /opt/teagram merge-base --is-ancestor "$TOOL_SHA" origin/main
sudo git -C /opt/teagram merge-base --is-ancestor "$TARGET_SHA" "$TOOL_SHA"
sudo mkdir -m 700 -p /root/teagramd-rollout-runner
sudo env TOOL_SHA="$TOOL_SHA" bash -c '
  set -eu
  umask 077
  [[ "$TOOL_SHA" =~ ^[0-9a-f]{40}$ ]]
  for name in rollout-runner.sh rollout-verifier.sh schema-result-gate.sh schema-result-gate.py blob-mode-state.py qualify-rustfs-transition.py stack-move-runner.py; do
    git -C /opt/teagram show \
      "$TOOL_SHA:deploy/teagramd/rollout-runner/$name" \
      > "/root/teagramd-rollout-runner/$name"
    chmod 600 "/root/teagramd-rollout-runner/$name"
  done
  git -C /opt/teagram show \
    "$TOOL_SHA:deploy/teagramd/rollout-runner/local-compose-598359e.yml" \
    > /opt/teagram/.rollout-compose.local-598359e.yml
  printf "%s  %s\n" \
    714d7f870645e2581234feba81a86e9e529facd0b664b32b2f429eb583dc754f \
    /opt/teagram/.rollout-compose.local-598359e.yml | sha256sum --check
  chmod 600 /opt/teagram/.rollout-compose.local-598359e.yml
'
cd /opt/teagram
sudo env ROLLOUT_RUNNER_SOURCE_SHA="$TOOL_SHA" COMPOSE_FILE="$COMPOSE_FILE" \
  bash /root/teagramd-rollout-runner/rollout-runner.sh apply \
  "$TARGET_SHA" "$EXPECTED_BASELINE_SHA"
```

## Historical rollout: df9ffe5

The fixed application target is
`df9ffe538defd4b99bd9edb2405caacddd1aa1f6`. Its expected live baseline is the
successfully deployed
`598359e900130c307dd84022b0077fd3276f6f36`; confirm that exact checkout during
the MAIN-1631 deployment. This artifact preserves the existing 598359e local
Compose contract and image references.

For this historical target, the committed artifact was
`deploy/teagramd/rollout-runner/local-compose-df9ffe5.yml`, staged at
`.rollout-compose.local-df9ffe5.yml`. Its SHA-256 is
`b48e1bc4727b9ed5e05d247fb7f5e3424eca8c1f9b496727e56127b67f7a5ce1`. Its
service, environment, image, volume, exposure, authority-mount, migration, and
grace-period configuration matches the reviewed 598359e artifact after the
two target header comments. At the time, local apply accepted this artifact for
the exact df9ffe5 target, followed by the existing override. This Compose body is the
reviewed content anchor used to validate later live-file reuse. The captured
598359e baseline remains the healthy rollback target if rollout verification
fails; preserve it unchanged. MAIN-1631 required this runner and target-artifact
pairing for its fixed target.

Until MAIN-1332, do not bootstrap RustFS or generate credentials for this local
rollout. Keep both parked credential files in place and unchanged; do not
inspect or rotate them. MAIN-1332 reuses those existing files.

Keep `TARGET_SHA` fixed even though the reviewed runner and artifact land in a
later source revision. `TOOL_SHA` is the full reviewed source commit reachable
from `origin/main` and descending from the application target. Confirm the
expected live baseline before staging the seven runtime files and artifact:

```sh
TARGET_SHA=df9ffe538defd4b99bd9edb2405caacddd1aa1f6
TOOL_SHA=<reviewed-full-tool-source-commit-sha>
EXPECTED_BASELINE_SHA=598359e900130c307dd84022b0077fd3276f6f36
ACTUAL_BASELINE_SHA=$(sudo git -C /opt/teagram rev-parse HEAD)
[ "$ACTUAL_BASELINE_SHA" = "$EXPECTED_BASELINE_SHA" ]
COMPOSE_FILE=.rollout-compose.local-df9ffe5.yml:docker-compose.override.yml

sudo git -C /opt/teagram fetch -q origin main
sudo git -C /opt/teagram cat-file -e "$TARGET_SHA^{commit}"
sudo git -C /opt/teagram cat-file -e "$TOOL_SHA^{commit}"
sudo git -C /opt/teagram merge-base --is-ancestor "$TARGET_SHA" origin/main
sudo git -C /opt/teagram merge-base --is-ancestor "$TOOL_SHA" origin/main
sudo git -C /opt/teagram merge-base --is-ancestor "$TARGET_SHA" "$TOOL_SHA"
sudo mkdir -m 700 -p /root/teagramd-rollout-runner
sudo env TOOL_SHA="$TOOL_SHA" bash -c '
  set -eu
  umask 077
  [[ "$TOOL_SHA" =~ ^[0-9a-f]{40}$ ]]
  for name in rollout-runner.sh rollout-verifier.sh schema-result-gate.sh schema-result-gate.py blob-mode-state.py qualify-rustfs-transition.py stack-move-runner.py; do
    git -C /opt/teagram show \
      "$TOOL_SHA:deploy/teagramd/rollout-runner/$name" \
      > "/root/teagramd-rollout-runner/$name"
    chmod 600 "/root/teagramd-rollout-runner/$name"
  done
  git -C /opt/teagram show \
    "$TOOL_SHA:deploy/teagramd/rollout-runner/local-compose-df9ffe5.yml" \
    > /opt/teagram/.rollout-compose.local-df9ffe5.yml
  printf "%s  %s\n" \
    b48e1bc4727b9ed5e05d247fb7f5e3424eca8c1f9b496727e56127b67f7a5ce1 \
    /opt/teagram/.rollout-compose.local-df9ffe5.yml | sha256sum --check
  chmod 600 /opt/teagram/.rollout-compose.local-df9ffe5.yml
'
cd /opt/teagram
sudo env ROLLOUT_RUNNER_SOURCE_SHA="$TOOL_SHA" COMPOSE_FILE="$COMPOSE_FILE" \
  bash /root/teagramd-rollout-runner/rollout-runner.sh apply \
  "$TARGET_SHA" "$EXPECTED_BASELINE_SHA"
```

## Revision-qualified target: cce00d1

Application target `cce00d185e8e7734b482f116e465647f6ec1fa26` has a separate
local Compose artifact at
`deploy/teagramd/rollout-runner/local-compose-cce00d1.yml`. Its SHA-256 is
`72a91a77db188f106b0062420722166436d38c6cb1974b73b9dd2873fbc5c11c`; its
comment-independent body SHA-256 is
`577dd9885aa9babd3370a97417d1653884f0567c6f50e7feb208ca9d11ca15bd`. Select
only `.rollout-compose.local-cce00d1.yml:docker-compose.override.yml` for this
target. The artifact keeps the reviewed local service and volume contract while
forwarding `TEAGRAMD_SOURCE_REVISION` as the image build argument. The reviewed
tool-source commit is separate from this application target.

For MAIN-1769 activation, use the reviewed tool-source commit after it is on
`origin/main`; confirm the live checkout head for the expected baseline. Stage
the runner files and artifact from that tool-source commit, verify the artifact
digest above, then run the ordinary `apply` command with this target. On a
failed target, the existing runner restores the captured checkout and image
while retaining the existing named volumes; it uses the same Compose selection
for verification and rollback.

```sh
TARGET_SHA=cce00d185e8e7734b482f116e465647f6ec1fa26
TOOL_SHA=<reviewed-full-tool-source-commit-sha>
EXPECTED_BASELINE_SHA=$(sudo git -C /opt/teagram rev-parse HEAD)
COMPOSE_FILE=.rollout-compose.local-cce00d1.yml:docker-compose.override.yml

sudo git -C /opt/teagram fetch -q origin main
sudo git -C /opt/teagram cat-file -e "$TOOL_SHA^{commit}"
sudo git -C /opt/teagram merge-base --is-ancestor "$TOOL_SHA" origin/main
sudo git -C /opt/teagram merge-base --is-ancestor "$TARGET_SHA" "$TOOL_SHA"
sudo mkdir -m 700 -p /root/teagramd-rollout-runner
sudo env TOOL_SHA="$TOOL_SHA" bash -c '
  set -eu
  umask 077
  for name in rollout-runner.sh rollout-verifier.sh schema-result-gate.sh schema-result-gate.py blob-mode-state.py qualify-rustfs-transition.py stack-move-runner.py; do
    git -C /opt/teagram show \
      "$TOOL_SHA:deploy/teagramd/rollout-runner/$name" \
      > "/root/teagramd-rollout-runner/$name"
    chmod 600 "/root/teagramd-rollout-runner/$name"
  done
  git -C /opt/teagram show \
    "$TOOL_SHA:deploy/teagramd/rollout-runner/local-compose-cce00d1.yml" \
    > /opt/teagram/.rollout-compose.local-cce00d1.yml
  printf "%s  %s\n" \
    72a91a77db188f106b0062420722166436d38c6cb1974b73b9dd2873fbc5c11c \
    /opt/teagram/.rollout-compose.local-cce00d1.yml | sha256sum --check
  chmod 600 /opt/teagram/.rollout-compose.local-cce00d1.yml
'
sudo env ROLLOUT_RUNNER_SOURCE_SHA="$TOOL_SHA" COMPOSE_FILE="$COMPOSE_FILE" \
  bash /root/teagramd-rollout-runner/rollout-runner.sh apply \
  "$TARGET_SHA" "$EXPECTED_BASELINE_SHA"
```

The exact `COMPOSE_FILE` selection above remains in force for every Compose
operation during this rollout, including verification and rollback. Any direct
Compose check must use the same `COMPOSE_FILE` value.

### Data access, backup, and recovery

`pgdata` contains PostgreSQL data, `tgblobs` contains uploaded file bodies, and
`tgkey` contains the persistent server identity and key file. The Postgres
service has no published port in this artifact; `teagramd` receives database
credentials and read/write mounts for the key and blob volumes. The existing
Compose override preserves the current protocol exposure. A host-root or Docker
administrator can read the volumes, container environment, and deployment
evidence, so access to the LXC and its Docker daemon is trusted access to this
data. The runner's filesystem permissions protect its evidence from ordinary
LXC users, but the runner does not encrypt the dump itself.

The runner writes `pg_dump` under a root-owned mode-0700 run directory and
mode-0600 dump file, hashes it, and verifies a restore in a disposable,
network-isolated Postgres container using tmpfs before replacing the service.
That backup covers PostgreSQL only; it does not copy `tgblobs` or `tgkey`. A
failed target is rolled back to the captured checkout and image while retaining
the existing named volumes. Recovery from loss or corruption of either volume
therefore requires an independent volume backup; this runner does not provide
that restore path, and rollback depends on the volumes remaining intact. That
volume backup is for recovery, not a rollout prerequisite; the verified Postgres
dump and isolated restore are the runner's backup gate.

```sh
TARGET_SHA=e58ba203505a37bf5ad3181f22e59a934d4a7442
TOOL_SHA=<reviewed-full-tool-source-commit-sha>
EXPECTED_BASELINE_SHA=$(sudo git -C /opt/teagram rev-parse HEAD)

sudo git -C /opt/teagram fetch -q origin main
sudo git -C /opt/teagram cat-file -e "$TARGET_SHA^{commit}"
sudo git -C /opt/teagram cat-file -e "$TOOL_SHA^{commit}"
sudo git -C /opt/teagram merge-base --is-ancestor "$TARGET_SHA" origin/main
sudo git -C /opt/teagram merge-base --is-ancestor "$TOOL_SHA" origin/main
sudo git -C /opt/teagram merge-base --is-ancestor "$TARGET_SHA" "$TOOL_SHA"
sudo mkdir -m 700 -p /root/teagramd-rollout-runner
sudo env TOOL_SHA="$TOOL_SHA" bash -c '
  set -eu
  umask 077
  [[ "$TOOL_SHA" =~ ^[0-9a-f]{40}$ ]]
  for name in rollout-runner.sh rollout-verifier.sh schema-result-gate.sh schema-result-gate.py blob-mode-state.py qualify-rustfs-transition.py stack-move-runner.py; do
    git -C /opt/teagram show \
      "$TOOL_SHA:deploy/teagramd/rollout-runner/$name" \
      > "/root/teagramd-rollout-runner/$name"
    chmod 600 "/root/teagramd-rollout-runner/$name"
  done
  git -C /opt/teagram show \
    "$TOOL_SHA:deploy/teagramd/rollout-runner/local-compose-e58ba20.yml" \
    > /opt/teagram/.rollout-compose.local-e58ba20.yml
  printf "%s  %s\n" \
    d3426792677d0510a75dd254fbbd5fd9d9b19a01d710ec5b9b59f3e8a31447c7 \
    /opt/teagram/.rollout-compose.local-e58ba20.yml | sha256sum --check
  chmod 600 /opt/teagram/.rollout-compose.local-e58ba20.yml
'
cd /opt/teagram
COMPOSE_FILE=.rollout-compose.local-e58ba20.yml:docker-compose.override.yml
sudo env ROLLOUT_RUNNER_SOURCE_SHA="$TOOL_SHA" COMPOSE_FILE="$COMPOSE_FILE" \
  bash /root/teagramd-rollout-runner/rollout-runner.sh apply \
  "$TARGET_SHA" "$EXPECTED_BASELINE_SHA"
```

## Subsequent local-backed deployments

Reuse the Compose file reported by the live `teagramd` container. A
target-specific filename is not required; the runner accepts a selected
`.rollout-compose.local-*` file when its non-comment content matches the
reviewed anchor and it is followed only by the existing override. The runner
still validates the live authority, proposed render, backup restore, build,
readiness, and rollback before changing the service. If local Compose content
must change, introduce and review a new artifact and content pin first. Keep the
RustFS credentials parked and do not run
`deploy/bootstrap-rustfs-secrets.sh` before the MAIN-1332 cutover.

Set `EXPECTED_BASELINE_SHA` to the current live checkout head and derive
`COMPOSE_FILE` from the running container label:

```sh
TARGET_SHA=<next-reviewed-full-commit-sha>
TOOL_SHA=<reviewed-full-tool-source-commit-sha>
EXPECTED_BASELINE_SHA=$(sudo git -C /opt/teagram rev-parse HEAD)
sudo git -C /opt/teagram fetch -q origin main
sudo git -C /opt/teagram cat-file -e "$TOOL_SHA^{commit}"
sudo git -C /opt/teagram merge-base --is-ancestor "$TOOL_SHA" origin/main
sudo git -C /opt/teagram merge-base --is-ancestor "$TARGET_SHA" "$TOOL_SHA"
LIVE_COMPOSE_FILES=$(sudo docker inspect --format \
  '{{index .Config.Labels "com.docker.compose.project.config_files"}}' \
  teagram-server-teagramd-1 | tr , :)
COMPOSE_FILE=$LIVE_COMPOSE_FILES
sudo env TOOL_SHA="$TOOL_SHA" bash -c '
  set -eu
  umask 077
  for name in rollout-runner.sh rollout-verifier.sh schema-result-gate.sh schema-result-gate.py blob-mode-state.py qualify-rustfs-transition.py stack-move-runner.py; do
    git -C /opt/teagram show \
      "$TOOL_SHA:deploy/teagramd/rollout-runner/$name" \
      > "/root/teagramd-rollout-runner/$name"
    chmod 600 "/root/teagramd-rollout-runner/$name"
  done
'
sudo env ROLLOUT_RUNNER_SOURCE_SHA="$TOOL_SHA" COMPOSE_FILE="$COMPOSE_FILE" \
  bash /root/teagramd-rollout-runner/rollout-runner.sh apply \
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

### `.env` baseline, additions and supported value changes

Every verified target or rollback records the digest of the `.env` it ran with
in `.state/rollout-env`. Each `apply` requires the current `.env` to match that
record before the backup gate, so an unrecorded edit, even to a variable the
Compose content does not read, is rejected. Without a record, the digest comes
from the latest verified target or rollback evidence under `/root`. A host with
neither adopts the live `.env` once with `ROLLOUT_RUNNER_ADOPT_ENV=1`, which is
refused as soon as a record or verified evidence exists.

A release that needs a new `.env` variable names it for that one `apply`.
Copy the recorded `.env` to a root-owned mode-0600 backup, append one
`NAME=value` line per new variable, then pass the names and the backup:

```sh
sudo env ROLLOUT_RUNNER_SOURCE_SHA="$TOOL_SHA" COMPOSE_FILE="$COMPOSE_FILE" \
  ROLLOUT_RUNNER_ADDED_ENV=TG_EXAMPLE_ONE,TG_EXAMPLE_TWO \
  ROLLOUT_RUNNER_PREVIOUS_ENV_FILE=/root/.env.bak-<date> \
  bash /root/teagramd-rollout-runner/rollout-runner.sh apply \
  "$TARGET_SHA" "$EXPECTED_BASELINE_SHA"
```

The backup must match the record, and the current `.env` must be the backup
followed only by one assignment per named variable, none of them set in the
backup or in the live container. Names under
`TG_RSA_KEY_*`, `TG_AUTHKEY_ENC_KEY*`, `TG_POSTGRES_*`, `POSTGRES_*`,
`TG_BLOB_*`, `RUSTFS_*`, `TG_REPLICA_*`, `TG_CLIENT_ADDR_TRUST`, `COMPOSE_*` and
`DOCKER_*` are refused. The baseline evidence `env-baseline.txt` records the
recorded and current `.env` digests. Snapshots leave the named variables out of
`container_env_sha256` and list the ones present in `container_env_added`, so
target and rollback containers still match the baseline on every other entry.
A variable reaches `teagramd` only when the reviewed local Compose content maps
it. Once a release with the variable is live, later deploys pass no names.
The public server's Turnstile inputs are host-side aliases: name
`TEAGRAM_TURNSTILE_SITE_KEY` and `TEAGRAM_TURNSTILE_SECRET_KEY_FILE` here;
the verifier checks their corresponding `TG_TURNSTILE_*` container settings
and the read-only secret-file mount. The disabled defaults this Compose gives a
live container (empty site key and secret path, sign-in `false`, secret mounted
from `/dev/null`) count as unset, so a Turnstile name can be added over them.

`TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET` changes only the tailnet per-network
key-exchange budget. Add it through `ROLLOUT_RUNNER_ADDED_ENV` or change its
existing assignment through `ROLLOUT_RUNNER_CHANGED_ENV`; both paths require
the recorded `.env` backup and verify that the resolved Compose value matches
the running container. Its exact-empty Compose mapping remains equivalent to
an absent value, and a configured value stays pinned in the inspected container
environment. Setting it to `0` disables only the tailnet bucket; the public
per-network, per-connection, and global budgets remain separately configured.

`TG_USER_INVITE_QUOTA` can be added through the same named `.env` path and must
be a non-negative decimal integer whose value matches the resolved Compose
value and replacement container. Its exact-empty Compose default is equivalent
to an absent setting, so rollback restores the disabled default without
preserving an empty container variable. After the addition is verified, later
rollouts retain the configured quota in the environment and configuration
digests.

The runner allows changing `TG_REGISTRATION` between `closed` and `open`,
`TG_TURNSTILE_SIGNIN_ENABLED` between `false` and `true`, and WebSocket origin
allowlists through the same named path. `TG_WEBSOCKET_ALLOWED_ORIGINS` accepts
a new string value; when it is assigned in `.env`, the runner restores the
previous `.env` before recreating the baseline container. If the effective
value is assigned only in `docker-compose.override.yml`, back up that file to a
root-owned mode-0600 file, change only its origin assignment, and pass the
backup as `ROLLOUT_RUNNER_PREVIOUS_OVERRIDE_FILE` instead. The runner requires
the `.env` to remain unchanged and contain no origin assignment, checks the
backup digest against the last verified Compose snapshot, and verifies that
only the override's named origin value changed. It confirms the old override
value matches the running baseline and that the replacement matches resolved
Compose. A failed rollout restores the previous override before recreating the
baseline container, then checks its recorded override digest. Public WebSocket
settings are also supported. `TG_PUBLIC_REGISTRATION` may change only between
`closed` and `invite`; the other three public settings accept a new string
value, including empty to disable them. `open` is never accepted for
`TG_PUBLIC_REGISTRATION`.
Back up the verified `.env` to a root-owned
mode-0600 file, change only that assignment in place, then pass the changed
name and backup to the same `apply`:

```sh
sudo env ROLLOUT_RUNNER_SOURCE_SHA="$TOOL_SHA" COMPOSE_FILE="$COMPOSE_FILE" \
  ROLLOUT_RUNNER_CHANGED_ENV=TG_REGISTRATION \
  ROLLOUT_RUNNER_PREVIOUS_ENV_FILE=/root/.env.bak-<date> \
  bash /root/teagramd-rollout-runner/rollout-runner.sh apply \
  "$TARGET_SHA" "$EXPECTED_BASELINE_SHA"
```

The backup digest must match the record; every other line must remain
byte-for-byte identical, and the named value must switch between its two
listed values. Target and rollback snapshots verify that the running container
receives the value from resolved Compose, record `container_env_changed`, and
omit that one named value from `container_env_sha256` so all other entries
remain pinned. A successful target records the changed `.env` digest. A
verified rollback records that digest only when the old Compose render also
retains the changed value; otherwise rollback verification fails closed.
To deactivate public sign-in after activation, set
`ROLLOUT_RUNNER_CHANGED_ENV=TG_TURNSTILE_SIGNIN_ENABLED` and change only its
assignment from `true` to `false`; the signup credentials remain in place.

For an override-only WebSocket origin change, retain the `.env` unchanged and
use the previous override backup:

```sh
sudo env ROLLOUT_RUNNER_SOURCE_SHA="$TOOL_SHA" COMPOSE_FILE="$COMPOSE_FILE" \
  ROLLOUT_RUNNER_CHANGED_ENV=TG_WEBSOCKET_ALLOWED_ORIGINS \
  ROLLOUT_RUNNER_PREVIOUS_OVERRIDE_FILE=/root/docker-compose.override.yml.bak-<date> \
  bash /root/teagramd-rollout-runner/rollout-runner.sh apply \
  "$TARGET_SHA" "$EXPECTED_BASELINE_SHA"
```

## Registration-open threat model

`TG_REGISTRATION=open` removes the invite requirement from `auth.signUp`; the
existing auth-key, sign-up-code, input-validation, and unique-username checks
still apply. It does not change listener bindings or advertise a new endpoint.
Any client that can reach the listener selected by the existing Compose
artifact and override can attempt to register without an invite. Open mode has
no automatic expiry and remains active until an operator changes it.

Assets and threats:

- The username namespace and account records can be claimed, impersonated, or
  filled with unwanted accounts. A newly admitted account can also be used for
  spam or other service abuse.
- Repeated account creation consumes database and service capacity. Unless
  overridden, the shipped defaults limit `auth.signUp` to 5 attempts per
  client network per hour, `auth.sendCode` to 10 calls per network per hour,
  and code requests to 20 distinct usernames per network per day; a zero limit
  disables enforcement. These are per-network limits, not a global admission
  budget; an attacker can distribute attempts across networks. They depend on
  `TG_CLIENT_ADDR_TRUST` reporting the real client network.
- On a database where the one-time server-administrator election is still
  open, the first successful signup can win that election. Verify that the
  intended administrator is already elected and the election is closed before
  allowing open registration.
- `TG_LOG_LOGIN_CODES` is a legacy diagnostic, not part of username/password
  sign-in. When enabled, it writes phone numbers and codes in cleartext. Keep it
  false for any deployment with retained or shared logs.

Operator controls and limits:

- The runner permits only an explicitly named `TG_REGISTRATION` change
  between `closed` and `open`. It requires a root-owned mode-0600 copy of the
  last verified `.env`, rejects any other line change, verifies the resolved
  Compose value against the running container, and records the resulting
  digest. This protects the change path; it does not decide whether open
  registration is appropriate.
- An operator can stop new signups by changing the existing assignment to
  `closed` through the same runner path and verifying the live container
  value. Closing registration does not revoke accounts already created. The
  runner does not currently provide a transition to `invite`.
- Rejected `auth.signUp` calls produce sampled, redacted reason logs. Those
  logs omit usernames and client addresses and do not count successful
  signups; this change adds no success alert or global signup cap. Operators
  must monitor account and resource growth through their existing operational
  access while registration is open. If abuse exceeds the accepted level,
  close registration and investigate the affected accounts separately.

## Interrupted publication

Publication commits the synced report, then a non-overwriting journal entry,
then identical `mode.json` bytes. If the runner stops after the journal entry
is committed but before the mode head is replaced, both server validation and
ordinary rollout fail closed. With the exact reviewed target still checked out
on `main`, reconcile only that interrupted initial-local publication:

```sh
sudo env COMPOSE_FILE="$COMPOSE_FILE" \
  bash /root/teagramd-rollout-runner/rollout-runner.sh reconcile "$TARGET_SHA"
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

## Current live transition

`blob-transition-runner.py accept-s3` cuts over the current local stack without
a migration-set pin. It reads the active `teagramd` Compose labels and requires
the exact local selection `docker-compose.yml`,
`docker-compose.local-blobs.yml`, then `docker-compose.override.yml`. It renders
that local selection and the S3 candidate (`docker-compose.yml` plus the
existing override), checking both against blob-mode authority before stopping
a writer. RustFS must stay on `rustfs_private`, an internal bridge using the
isolated IPv4 gateway mode with IPv6 disabled, with no published ports or extra
network attachments. The runner inspects the live RustFS container and Docker network
before copying media and before recovery; after activation it checks the live
backend, mounts, port bindings, and network attachments against authority.

Under `/tmp/teagram-deploy.lock`, the runner stops every `teagramd*` service,
checks the host for remaining writers, captures `pg_dump` and verifies it by
restoring into a temporary database, then records one source census. It loads
the existing RustFS credentials from `/opt/teagram/.env` and the root-owned
parked `/opt/telegram-server/.env.rustfs-parked` file without copying them. It
checks that the parked app secret matches the existing mounted key file and
rejects a mismatch before stopping writers. It starts RustFS and `rustfs-init`,
makes one checksum-verified copy, and compares
the full copy manifest with an independent destination census. A mismatch
leaves local authority active and restarts local serving. After the publisher
records `s3-accepted`, it starts S3 serving. Activation failure after
publication leaves S3 authority in place for diagnosis.

Run as root from `/opt/teagram` after MAIN-1977 and deployment of this runner:

```sh
cd /opt/teagram
python3 deploy/teagramd/rollout-runner/blob-transition-runner.py accept-s3 \
  --checkout /opt/teagram \
  --state-dir /opt/teagram/.state/blob-mode \
  --report-root /root \
  --lock-path /tmp/teagram-deploy.lock
```

`blob-transition-runner.py recover-local` is the rollback to `TG_BLOB_DIR`. It
checks the active S3 and proposed local renders, freezes `teagramd*`, verifies
a fresh Postgres dump restore, records an S3 census, restores S3 objects into
the retained `tgblobs` volume, and compares the restore manifest and
destination census. Local-only keys already in `tgblobs` are kept and counted.
It publishes `recovered-local` before starting local serving. A rejection
before publication leaves S3 authority active and resumes S3 serving. The
rollback writes to `tgblobs`; it never removes the volume.

Run recovery with the same paths. To return to S3 later, run `accept-s3`
again; it copies the current local contents through the same checksum and
census checks.

```sh
cd /opt/teagram
python3 deploy/teagramd/rollout-runner/blob-transition-runner.py recover-local \
  --checkout /opt/teagram \
  --state-dir /opt/teagram/.state/blob-mode \
  --report-root /root \
  --lock-path /tmp/teagram-deploy.lock
```

Every authority publication is made by `blob-mode-state.py` under the shared
lock. Never hand-write mode JSON. If publication stops after the journal entry
is committed but before the head is replaced, run the existing `reconcile`
action with this reviewed checkout before restarting either backend.

## Historical migration qualification boundary

The ordinary rollout runner still initializes and rolls out local storage
only. The qualified actions are retained for earlier pinned qualification
workflows; the current `accept-s3` and `recover-local` actions above do not
invoke them. `blob-transition-runner.py accept-s3-qualified` exercises the
retired cutover sequence with a root-owned pre-copy bundle beneath the report
root. Under the shared lock it verifies the current local render against local
authority, stops every configured
`teagramd*` writer, and checks that no writers remain before it recaptures the
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
remaining `teagramd*` writer or writable source-volume mount. Surviving project
service names must match the frozen inventory before RustFS startup. It makes two
verified copies with an independent destination census after each, runs the
full qualification gate, publishes the synced
report/journal/head, then starts `teagramd*` without rerunning migrations. A
pre-publication rejection leaves local authority and evidence in place and
resumes the local serving services. An interruption after journal publication
follows the same `reconcile` rules above. Activation failures after S3
publication do not roll back to local.

`blob-transition-runner.py recover-local-qualified` consumes a separate root-owned
recovery bundle containing `recovery.json`, `frozen-containers.json`,
`postgres.dump`, and `migrations.json`. The qualification record binds the
fresh dump and schema evidence to the freeze and retained `tgblobs` volume.
Under the shared lock the runner verifies the current S3 and proposed local
renders against authority, stops all `teagramd*` writers, checks that the
surviving project service names match the frozen inventory, then captures a
fresh Postgres dump, live Atlas/schema query, and reference queries against the
stopped deployment. For R70 it captures both revision snapshots and the
seven-surface inert query before the dump, then records their freeze times and
the pinned query digest in `recovery.json`. The migration schema capture is
bound to the dump digest; the reference rows must be covered by the fresh S3
census. It then records two
fresh S3 censuses, a local pre-restore census, two verified restores, two local
censuses, and the retained local-only key set. It syncs the evidence and publishes `recovered-local` before starting
the local serving services. A pre-publication rejection resumes S3 under the
unchanged S3 authority; local start remains rejected until the restore proof is
published. The private recovery report publishes the aggregate retained-key
count and never claims physical erasure.

Both qualified transition commands reject live execution while the MAIN-1418/1419/1420
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

## Blob-mode authority threat model

The authority decides which blob backend may serve and receive writes. A wrong
record, or a serving container that ignores it, splits media between local and
S3 storage or exposes it beyond the reviewed boundary.

Assets:

- The authority tree under `.state/blob-mode`: `mode.json`, the append-only
  `journal/`, and the root-only `teagramd-blob-mode-report-<id>.json` each
  record binds by SHA-256.
- The `tgblobs` volume (user media), the `tgkey` volume (auth-key encryption
  key), the `pgdata` volume, and any S3/RustFS credentials.
- `teagramd`'s network exposure: published ports, network mode, and attached
  networks.
- The verified pre-rollout Postgres dump that makes the rollout reversible.

Trust boundaries and what each side may do:

- Root on the LXC is trusted. The runner, the helper, and the shared deployment
  lock run as root; the helper's publishing commands refuse to run as anyone
  else or without the lock. Authority files, evidence, and their directories must be root-owned,
  not group- or world-writable, and not symlinks; private evidence is `0600`.
- Git is trusted only for reviewed commits. Target and tool-source commits must
  be reachable from `origin/main`, the runner verifies its runtime files against
  `TOOL_SHA`, and local Compose artifacts are accepted only by pinned digest.
- The Docker daemon is trusted to report what it runs. `docker inspect` is the
  only source for baseline facts (backend environment, mounts, ports, network
  mode, attached networks). A compromised daemon is out of scope.
- `docker compose config --format json` is trusted only as the desired state of
  the files the runner selected: the pinned artifact plus the existing
  override. The helper reads an allowlist from it (backend settings, blob,
  key, and pgdata mounts, authority mount, ports, network mode and networks,
  and an environment digest); everything else in the render is not evidence.
- The override is operator-edited input. It may not mention the authority
  mount, `TG_BLOB_*` settings, or `tgblobs`, and for initial-local every
  allowlisted field it changes must equal the running baseline.
- `teagramd` and helper containers are untrusted with respect to the
  authority. Serving replicas get it as a read-only bind and refuse to start
  when a present head is stale or does not match their backend; helper services may not mount it at all.

Attacker capabilities considered:

- An override, `.env`, or Compose change that retargets blob storage, removes
  or makes the authority writable, swaps a named volume, changes the
  environment, publishes a port, switches network mode, or attaches
  `teagramd` to an extra network. Initial-local rejects each before the
  backup; ordinary `apply` rejects backend, volume, and authority-mount drift
  before build.
- A target render or running container that changes between preflight and
  publication. Captures are immutable, hashed, and re-compared before
  publication.
- A crash or concurrent run during publication. The lock serializes runners;
  report, journal, and head are written in order with fsync, and only an exact
  journal/report pair can be reconciled.
- A compromised serving process. It can read media it already serves but
  cannot write the authority or redirect other replicas.

Out of scope and residual risk:

- Root on the LXC or host, the Docker daemon, the kernel, and the GitHub origin.
- Compose fields outside the allowlist (for example `privileged`, `cap_add`,
  `devices`, `extra_hosts`, or volumes other than the guarded ones) are not
  compared against the baseline. They are bounded by the pinned artifact and
  override review, not by this gate.
- `service:` network modes render differently from their inspected
  `container:` form, so they never match and initial-local rejects them.

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
sudo env TMPDIR=/root bash deploy/teagramd/rollout-runner/test-container-id-enumeration.sh
sudo env TMPDIR=/root bash deploy/teagramd/rollout-runner/test-stack-move-entrypoint.sh
```

The stack move entrypoint fixture uses the real runtime validation with a
legacy `telegramd` source Compose and container. It checks the baseline deploy
lock, rejects S3-authorized blob state before stopping the source, and allows
validated local authority to reach target worktree creation.

The Compose path preflight fixture checks relative, absolute, and normalized
selections from a checkout path containing spaces. It also rejects an omitted
existing override, missing or broken inputs, and a symlinked artifact. Run it on
GNU coreutils and on the pinned Alpine BusyBox runtime:

```sh
sudo env TMPDIR=/root bash deploy/teagramd/rollout-runner/test-compose-path-preflight.sh
docker run --rm \
  --volume "$PWD:/workspace:ro" \
  --workdir /workspace \
  mirror.gcr.io/library/node:24-alpine3.22@sha256:191c9f0080fcbbc6547a85dc0ff7988072214a355aabdc1d2ec55a7dae5eea8a \
  sh -ec '
    busybox realpath --help 2>&1 | grep -Fq "BusyBox v1.37.0"
    apk add --no-cache bash
    bash deploy/teagramd/rollout-runner/test-compose-path-preflight.sh
  '
```

```sh
bash -n deploy/teagramd/rollout-runner/*.sh
bash deploy/teagramd/rollout-runner/test-initial-local-compose.sh
bash deploy/teagramd/rollout-runner/test-local-compose-e58ba20.sh
bash deploy/teagramd/rollout-runner/test-local-compose-598359e.sh
bash deploy/teagramd/rollout-runner/test-local-compose-df9ffe5.sh
python3 -B deploy/teagramd/rollout-runner/test-schema-result-gate.py
python -B deploy/teagramd/rollout-runner/test-blob-mode-state.py
bash deploy/teagramd/rollout-runner/test-rollout-runtime-pins.sh
bash deploy/teagramd/rollout-runner/test-rollout-verifier-env-additions.sh
sudo env TMPDIR=/root python3 -B deploy/teagramd/rollout-runner/test-blob-transition-runner.py
sudo env TMPDIR=/root bash deploy/teagramd/rollout-runner/test-rollout-runner.sh
```

The pinned source-revision gate runs in the Docker Compose CI job. It checks out
application target `5602c7c13eb1571278f43129345a4b52f0630377` in a detached
worktree, renders the artifact with the revision supplied and genuinely unset,
then builds and inspects both images with synthetic overrides and no Docker
credentials. Each build is bounded to six minutes. Run it with:

```sh
bash deploy/teagramd/rollout-runner/test-local-compose-5602c7c.sh
bash deploy/teagramd/rollout-runner/test-local-compose-cce00d1.sh
```

## RustFS qualification release boundary

R69 (`60-69`) is exhausted by any row in `user_photos`,
`profile_photo_state`, `profile_upload_receipt`, or `profile_delete_operation`,
by a nonempty `files` table, or by migration 70 or later. R70 (`60-70`) is
exhausted by any row in those four tables or `erasure_outbox`, `erasure_epoch`,
or `erasure_epoch_completion`, by a nonempty `files` table, or by changed
migration provenance or migration 71 or later. The read-only qualification
rejects exhausted states; it does not widen the census or repair the schema.
Ordinary rollout remains the only migration authority. The immutable pins,
closed catalog contracts, capture commands and PostgreSQL 18 CI proofs are
documented in `QUALIFICATION.md`.

Run the real PostgreSQL and Atlas qualification locally with:

```sh
sudo env "PATH=$PATH" TMPDIR=/root bash deploy/teagramd/rollout-runner/test-rustfs-schema-postgres.sh
```
