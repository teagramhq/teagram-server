# Public demo deployment

The demo is a separate Compose project for `demo.teagram.io`. It has its own
PostgreSQL volume, RSA identity volume, auth-key encryption secret, blob mount,
Docker networks and forced SSH deploy key. Do not reuse `/opt/teagram`, the dev
LXC's volumes or its deploy credentials.

The application and Compose stack are ready for an isolated LXC. The install
script intentionally stops until the public client config exposes
`wss://demo.teagram.io/apiws` (MAIN-2196). The demo auto-seed, welcome join and
scheduled group activity must also land as a tracked `seed-demo.sh` before the
weekly wipe timer can be installed. The application and web sign-up flow also
need to consume the demo's dedicated Turnstile keys before public launch. No
live host was changed by this repository change.

## LXC prerequisites

The Steward provisions the separate `teagram-demo` Incus LXC and its network
policy. Required layout and policy:

- Root-owned checkout at `/opt/teagram-demo`, branch `main`.
- Docker Engine and Compose v2.39.4 or later. The bootstrap overlay uses
  Compose's `!override` merge tag.
- Bash, coreutils, curl, Docker CLI, Git, jq, OpenSSL, Python 3, `flock` and a
  systemd timer service. The included weekly reset installer uses systemd.
- At least 2 vCPU, 4 GiB memory and 20 GiB root storage.
- A dedicated filesystem between 3 GiB and 4 GiB, mounted at
  `/srv/teagram-demo-blobs`. The aggregate upload store is a bind volume at
  `/srv/teagram-demo-blobs/data`; it must never fall back to the root disk.
- The data directory must be owned by uid/gid `65532`, mode `0700`:

  ```sh
  install -d -o 65532 -g 65532 -m 0700 /srv/teagram-demo-blobs/data
  ```

- The host firewall blocks egress to `10.0.0.0/8`, `172.16.0.0/12`,
  `192.168.0.0/16` and `100.64.0.0/10`, and allows no route between the demo
  and dev LXCs. The LXC is not joined to the tailnet. Do not expose `2443`,
  `2446`, Postgres or Docker.
- Run the demo's Cloudflare Tunnel connector in the demo LXC and route only
  `demo.teagram.io` to `http://127.0.0.1:8080`. The edge container publishes
  that origin on loopback only. It serves the client discovery document and
  `/apiws`, returns 404 for `/admin`, and returns 404 for all other paths.
- Restrict the Cloudflare Tunnel and DNS configuration to `demo.teagram.io`.
  The demo does not serve the dev hostname or admin dashboard.

The public origin and discovery document advertise
`demo.teagram.io:443`; browser clients use the separate WSS endpoint. CORS is
limited to `https://web.teagram.io`. Registration is open, the existing signup
limit remains five attempts per IP per hour, login-code logging is disabled,
and uploads are capped at 16 MiB per file and 256 MiB per account.

## First install

Do not begin until the Dev deploy for the selected `main` commit succeeds and
that commit's `client-config` includes the approved WebSocket endpoint. Run as
root on the demo LXC:

```sh
cd /opt/teagram-demo
deploy/teagram-demo/prepare-env.sh
install -d -o 65532 -g 65532 -m 0700 /srv/teagram-demo-blobs/data
deploy/teagram-demo/init-demo.sh
```

`prepare-env.sh` prompts for the demo-specific Turnstile site and secret keys,
then creates root-owned `.env` mode 0600 with those keys and two different
random 64-hex runtime secrets. It refuses to replace an existing file. Keep
`POSTGRES_PASSWORD` and `TG_AUTHKEY_ENC_KEY` unchanged during deploys and
weekly resets. Never display or copy `.env`. The server does not yet consume
the Turnstile secret, so configuring it does not itself enable signup
verification.

`init-demo.sh` validates that the blob path is on the dedicated size-limited
filesystem, that the volumes and blob directory are fresh, and that Compose
renders the exposure and storage contract. It creates the RSA identity once,
generates the discovery document, runs migrations, and uses the existing
`blob-mode-state.py initialize-local` publisher to record the durable local
blob authority before it starts the guarded server and edge containers. It
then checks the loopback discovery route, CORS, WebSocket handshake and admin
404. A failed partial install is not repaired by deleting volumes; inspect the
authority and containers before retrying.

## Deploy path

The `Deploy.` workflow runs the existing dev deploy first. The optional
`deploy-demo` job depends on that job and receives the exact same tested SHA.
It is disabled unless the `demo` GitHub environment variable
`TEAGRAM_DEMO_DEPLOY_ENABLED` is set to `true`.

Before enabling it, configure the `demo` GitHub environment with:

| Name | Kind | Value |
|---|---|---|
| `DEMO_DEPLOY_HOST` | variable | Demo LXC's tailnet host or IP |
| `DEMO_DEPLOY_KNOWN_HOSTS` | variable | Pinned SSH host key line for that host |
| `DEMO_DEPLOY_SSH_KEY` | secret | Dedicated key forced to the demo deploy command |
| `TEAGRAM_DEMO_DEPLOY_ENABLED` | variable | `true` only after host setup and first install |

Make the Tailscale OAuth client ID and secret available to the `demo`
environment as well, unless they are repository-level secrets.

The key's `authorized_keys` entry must have no shell, forwarding or PTY access:

```text
command="/opt/teagram-demo/deploy/teagram-demo/forced-command.sh",restrict ssh-ed25519 <public-key> teagram-demo-deploy
```

`forced-command.sh` accepts one full commit SHA, fetches `origin/main`, rejects
non-main revisions and skips stale deploys. `deploy-main.sh` revalidates the
blob authority and running containers, writes a root-only PostgreSQL custom
dump, checks the dump with `pg_restore --list`, builds the tested image,
preserves the RSA public key, stops the edge and server before migrations,
starts the new image and runs the smoke checks. A failed migration or smoke
check restores the previous checkout, image, client config and database, then
restarts and checks the previous server. The backup directory is reported on
failure and success under `.state/teagram-demo/backups/`.

Do not use `docker compose down -v`. The named `pgdata`, `tgkey` and `tgblobs`
volumes have independent lifetimes; especially, removing `tgkey` changes the
server fingerprint and invalidates existing client authorizations.

## Weekly reset

`wipe-weekly.sh` drops and recreates only the `teagram` database and removes
all files below the capped blob mount. It preserves the `tgkey` volume, the
`.env` encryption key and the RSA public identity, then runs migrations and
re-seeds through the public API.

The timer is Sunday at 03:00 UTC with a persistent catch-up and randomized
delay. Install it only after the tracked, executable
`deploy/teagram-demo/seed-demo.sh` supports `--check` and seeds the demo through
the API:

```sh
cd /opt/teagram-demo
deploy/teagram-demo/install-weekly-wipe.sh
```

The installer refuses to enable a timer without that seed command. Check a
run with `systemctl status teagram-demo-weekly-wipe.service` and
`journalctl -u teagram-demo-weekly-wipe.service`; manually triggering the
service is a destructive reset of public demo content.

## Monitoring

`gatus.yaml` supplies the `teagram-demo` group checks for the discovery JSON,
the WebSocket listener and the public admin 404, including TLS expiry. Merge
these endpoints into the existing Gatus status page and attach the alert
provider already configured there. The alert provider credentials live outside
this repository, so the snippet does not invent a provider name or secret.
