# teagram-server media operations

The default Compose stack includes a private RustFS service with no published
ports. The live stack retains the root and app credentials at
`/opt/telegram-server/.env.rustfs-parked`; the app key is mounted from
`/opt/teagram/.secrets/teagramd-blob-secret-key` (mode 0444 in a mode-0700
directory). The cutover runner loads the current `/opt/teagram/.env` first and
the parked credential file second. Never print, copy, or commit these files.
Before freezing writers, the runner requires the parked app secret to match
the existing mounted app-key file. A mismatch fails closed; the runner does
not reset an existing RustFS user.

The optional public auth Turnstile secret is also mounted as a file. Because
local Compose file secrets preserve the source file's permissions, stage the
existing root-only credential under `.secrets/` with mode 0444 inside that
mode-0700 directory, then set `TEAGRAM_TURNSTILE_SECRET_KEY_FILE` in the
protected `.env` to that path. For example, from `/opt/teagram`:

```sh
install -d -o root -g root -m 0700 .secrets
install -o root -g root -m 0444 /root/turnstile-teagram-web.secret .secrets/teagramd-turnstile
```

The application receives only `/run/secrets/teagramd-turnstile`; the source
path and secret value are not added to its environment. Leave the host variable
unset when Turnstile is not configured. The sign-in gate defaults off.

Do not run `deploy/bootstrap-rustfs-secrets.sh` during this cutover or generate
replacement keys while the parked set is present. The server reads the
root-owned durable authority mounted at `/run/teagramd/blob-mode`; starting
the default S3 Compose stack without a valid matching authority fails closed.
Do not hand-write a mode record or use the retired
`.state/blob-migration-complete` marker.

## Deploys

Successful push CI runs on `main` trigger the `Deploy.` workflow
(`.github/workflows/deploy.yml`). It deploys the newest successful CI push head
when the forge comparison confirms that it equals or descends from the
triggering run's head; otherwise it uses the triggering head. This prevents
out-of-order CI completions from displacing a newer pending deployment while
deploying only commits whose own CI passed. It joins the tailnet and
sends the commit SHA as the ssh command to a key that is a forced command on
the LXC: the host checks the SHA and runs that commit's
`deploy/teagram/deploy-main.sh` with `DEPLOY_SHA` set to it. The
script refuses a commit that is not on `origin/main`, skips one older than the
live checkout (`live is newer (<head>), skipped <sha>`), and otherwise takes
the deploy lock, dumps the database, stages the rollout runner from that
commit, runs `rollout-runner.sh apply`, and checks the image label, runner
identity record and fingerprint pin. Its last line is `deployed <sha>` or
`already at <sha> with runner provenance`; any failure exits nonzero and fails
the run.

To redeploy by hand, run the `Deploy.` workflow from the Actions tab
(`workflow_dispatch`, no inputs; it deploys the newest `main` commit with a
successful CI push run), or from a checkout of `main`:

```sh
ssh teagram-server 'DEPLOY_SHA=<full sha> bash -s' < deploy/teagram/deploy-main.sh
```

`deploy/teagram/test-deploy-main.sh` covers the script's target checks.

Immediately after the `move-stack` action, the live container records the
tracked selection `docker-compose.yml`, `docker-compose.local-blobs.yml`, and
the remapped `docker-compose.override.yml`. The deploy entrypoint accepts that
exact selection for the first Teagram rollout, as well as the pinned local
artifact used by later local-backed rollouts. Keep the file order reported by
the running container.

## Initial local rollout

The currently available publication path inspects an already-running local
baseline, publishes `initial-local` under the shared deploy lock, and starts a
guarded local target. Follow the exact command sequence and SHA requirements in
[`../teagramd/rollout-runner/README.md`](../teagramd/rollout-runner/README.md).
The initial transition uses its pinned initial-local artifact followed by the
existing override. Later local-backed deploys must carry forward the exact
Compose files reported by the running `teagramd` container's
`com.docker.compose.project.config_files` label. Keep that selection for every
Compose command; a bare `docker compose` renders the default S3 stack. The
runner rejects a missing override and keeps the authority bind read-only on
every `teagramd*` service.

New application targets reuse the live `.rollout-compose.local-*` file when
its non-comment content matches the last reviewed local Compose content. Do not
create a per-target artifact or ticket for unchanged content. If its Compose
content must change, review and pin the new artifact before rollout.

Before an operational rollout, the runner writes a Postgres dump and verifies
it with an isolated restore. This verified dump and restore are the backup gate;
a whole-LXC snapshot, including Docker volumes, is not a prerequisite.

The runner compares the authority against every running replica, the proposed
render, and the inspected full names of `tgblobs` and `rustfsdata` before
building or replacing a container. Before recreating a baseline after target
failure, it re-inspects the currently running containers and validates them
with the rollback render against the same durable authority. If their backend
or volume does not match, it stops without replacing the current containers.

## RustFS cutover and recovery

The reviewed live path runs from `/opt/teagram` as root with
`deploy/teagramd/rollout-runner/blob-transition-runner.py accept-s3`. It checks
the running local Compose selection, freezes `teagramd*`, verifies a fresh
Postgres dump restore, makes one checksum-verified copy from `tgblobs`, checks
the destination census, publishes `s3-accepted`, and starts S3-backed serving.
The operation and exact command are documented in
[`../teagramd/rollout-runner/README.md`](../teagramd/rollout-runner/README.md).

Use `blob-transition-runner.py recover-local` to restore S3 objects into the
retained `tgblobs` volume and publish `recovered-local` before starting the
local backend. Run `accept-s3` again to move back to RustFS. Both paths use the
same shared deployment lock and publish authority only through
`blob-mode-state.py`.

Never remove `tgblobs` or run `docker compose down -v` on the deployment. The
volume remains the local rollback copy and is written only when `recover-local`
restores S3 objects into it.
