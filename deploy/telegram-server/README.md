# telegram-server media operations

The Compose stack includes a private RustFS service with no published ports.
Its named `rustfsdata` volume and root/app credentials are provisioned by the
existing bootstrap. Credential values live in `.env` (mode 0600); the app key
is written to `.secrets/telegramd-blob-secret-key` (mode 0444 in a mode-0700
directory). Never copy these files off the deployment or commit them.

```sh
cd /opt/telegram-server
./deploy/bootstrap-rustfs-secrets.sh
```

This creates credentials; it does not authorize `telegramd` to use S3. The
server now reads the root-owned durable authority mounted at
`/run/telegramd/blob-mode`. Starting the default S3 Compose stack without a
valid matching authority fails closed. Do not hand-write a mode record or use
the retired `.state/blob-migration-complete` marker.

## Initial local rollout

The currently available publication path inspects an already-running local
baseline, publishes `initial-local` under the shared deploy lock, and starts a
guarded local target. Follow the exact command sequence and SHA requirements in
[`../telegramd/rollout-runner/README.md`](../telegramd/rollout-runner/README.md).
Keep Compose on `docker-compose.yml` plus `docker-compose.local-blobs.yml` for
this operation. The target remains local and the authority bind is read-only on
every `telegramd*` service.

Before an operational rollout, take and verify the normal backup or snapshot of
the whole LXC, including Docker volumes. Record the restore identifier and path
with the deployment work. The rollout runner's verified Postgres dump is an
additional restore check, not a replacement for the LXC snapshot.

The runner compares the authority against every running replica, the proposed
render, and the inspected full names of `tgblobs` and `rustfsdata` before
building or replacing a container. A backend or volume mismatch leaves the
existing containers running. Its target failure path restores the captured
checkout and image only after validating the proposed rollback render against
the same durable authority.

## RustFS transition boundary

This stage does not authorize a fresh S3 startup, copy media, restore media, or
publish `s3-accepted` or `recovered-local`. The read-only RustFS qualification
gate is evidence for a later reviewed orchestration path; it cannot publish a
serving authority on its own. Until that path lands, keep the local overlay and
do not change the backend or remove the authority mount.

Never remove `tgblobs` or run `docker compose down -v` on the deployment. The
retained volume is the local data store for this stage and remains available
for the later verified transition or recovery process.
