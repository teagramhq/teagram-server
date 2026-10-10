# telegram-server media operations

The default Compose stack includes a private RustFS service with no published
ports. Its named `rustfsdata` volume and root/app credentials are provisioned by
the existing bootstrap. Credential values live in `.env` (mode 0600); the app
key is written to `.secrets/telegramd-blob-secret-key` (mode 0444 in a
mode-0700 directory). Never copy these files off the deployment or commit them.

Do not run `deploy/bootstrap-rustfs-secrets.sh`, add RustFS keys, or change the
serving backend before the MAIN-1332 cutover. The server reads the root-owned
durable authority mounted at `/run/telegramd/blob-mode`; starting the default
S3 Compose stack without a valid matching authority fails closed. Do not
hand-write a mode record or use the retired `.state/blob-migration-complete`
marker.

## Initial local rollout

The currently available publication path inspects an already-running local
baseline, publishes `initial-local` under the shared deploy lock, and starts a
guarded local target. Follow the exact command sequence and SHA requirements in
[`../telegramd/rollout-runner/README.md`](../telegramd/rollout-runner/README.md).
The initial transition uses its pinned initial-local artifact followed by the
existing override. Later local-backed deploys must carry forward the exact
Compose files reported by the running `telegramd` container's
`com.docker.compose.project.config_files` label. Keep that selection for every
Compose command; a bare `docker compose` renders the default S3 stack. The
runner rejects a missing override and keeps the authority bind read-only on
every `telegramd*` service.

New application targets reuse the live `.rollout-compose.local-*` file when
its non-comment content matches the last reviewed local Compose content. Do not
create a per-target artifact or ticket for unchanged content. If its Compose
content must change, review and pin the new artifact before rollout.

Before an operational rollout, take and verify the normal backup or snapshot of
the whole LXC, including Docker volumes. Record the restore identifier and path
with the deployment work. The rollout runner's verified Postgres dump is an
additional restore check, not a replacement for the LXC snapshot.

The runner compares the authority against every running replica, the proposed
render, and the inspected full names of `tgblobs` and `rustfsdata` before
building or replacing a container. Before recreating a baseline after target
failure, it re-inspects the currently running containers and validates them
with the rollback render against the same durable authority. If their backend
or volume does not match, it stops without replacing the current containers.

## RustFS transition boundary

This stage does not authorize a fresh S3 startup, copy media, restore media, or
publish `s3-accepted` or `recovered-local`. The read-only RustFS qualification
gate is evidence for a later reviewed orchestration path; it cannot publish a
serving authority on its own. Until that path lands, keep the local overlay and
do not change the backend or remove the authority mount.

Never remove `tgblobs` or run `docker compose down -v` on the deployment. The
retained volume is the local data store for this stage and remains available
for the later verified transition or recovery process.
