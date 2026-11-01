# Public demo deployment

The demo runs in its own `teagram-demo` LXC and Docker engine, separate from the
dev `teagram` LXC. Its Compose project has its own PostgreSQL volume, RSA
identity volume, auth-key encryption secret, Turnstile secret file, blob mount,
Docker networks, checkout, forced SSH deploy key and Cloudflare tunnel. Do not
reuse `/opt/teagram`, the dev LXC's volumes or its deploy credentials.

The install script intentionally stops until the public client config exposes
`wss://demo.teagram.io/apiws` (MAIN-2196) and the tracked API seeder is ready.
It keeps the edge stopped until it has closed the admin election, seeded and
verified the content, restored production settings and verified the Turnstile
gate.

## LXC prerequisites

The Steward provisions the separate `teagram-demo` Incus LXC and its network
policy. Required layout and policy:

- Root-owned checkout at `/opt/teagram-demo`, branch `main`.
- Incus `teagram-demo` on charlie, Debian 13, with `limits.cpu=1`,
  `limits.cpu.priority=0`, `limits.memory=2GiB` and a 20 GiB root disk. This
  intentionally stays below the original 2-vCPU / 4-GiB guidance.
- Docker Engine and Compose v5.6.1. The bootstrap overlay uses
  Compose's `!override` merge tag.
- Bash, coreutils, curl, Docker CLI, Git, `grep`, jq, OpenSSL, Python 3, `flock` and a
  systemd timer service. The included weekly reset installer uses systemd.
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
- `teagramd` remains attached only to the internal backend network. Its HTTPS
  verification calls use the `turnstile-egress` Squid proxy; that proxy alone
  joins the outbound bridge and permits CONNECT from the fixed server and
  HAProxy health-check addresses to `challenges.cloudflare.com:443`. The TLS
  tunnel stays end-to-end, and the application sends site-verification
  requests to `/turnstile/v0/siteverify`. The proxy has no host-published port.
- Run the demo's Cloudflare Tunnel connector in the demo LXC and route only
  `demo.teagram.io` to `http://127.0.0.1:8080`. The edge container publishes
  that origin on loopback only. The root serves the pinned Teagram Web private
  artifact after its first eligible master publication; before then it returns
  the plain status page with HTTP 200. `/teagram-config.json` provides the fixed
  demo server and auto-guest settings. The edge also serves the client
  discovery document and `/apiws`, returns 404 for `/admin`, and applies SPA
  fallback only to client routes. For
  `/apiws`, Nginx forwards Cloudflare's `CF-Connecting-IP` to the private
  HAProxy service. HAProxy validates that there is exactly one IP address,
  sends it to teagramd in a PROXY-v2 header, and teagramd trusts only HAProxy's
  fixed `198.18.130.2/32` address. HAProxy also forwards internal MTProto TCP
  probes with PROXY-v2; its `2443` listener is available only on the internal
  backend network. Neither HAProxy nor teagramd publishes a host port.
- Restrict the Cloudflare Tunnel and DNS configuration to `demo.teagram.io`.
  The demo does not serve the dev hostname or admin dashboard.

The public origin and discovery document advertise
`demo.teagram.io:443`; browser clients use the separate WSS endpoint. Discovery
CORS remains limited to `https://web.teagram.io`; WebSocket origin checks allow
both `https://web.teagram.io` and `https://demo.teagram.io`. The app CSP keeps
the current web directives and restricts `connect-src` to the demo WSS origin
and Cloudflare Turnstile. Registration is open, the existing signup
limit remains five attempts per IP per hour, login-code logging is disabled,
client uploads are disabled by `TG_UPLOADS_DISABLED=true`, and the existing
500 KB per-file cap, upload-byte budget and 256 MiB per-account storage cap
remain as defense in depth.
Sticker and custom emoji pack imports are disabled (`TG_STICKER_IMPORT=off`);
catalogue packs remain available to install and send.

`prepare-env.sh` keeps the Turnstile site key in `.env` and writes the Siteverify
secret to `/var/lib/teagram-demo/secrets/turnstile-secret-key`. The merged demo
setup and deploy scripts provision a separate random QA bypass token at
`/var/lib/teagram-demo/secrets/turnstile-qa-bypass-token`; neither token is
printed. Both files are root-owned, group-readable only by runtime gid 65532,
and mounted only into teagramd. The demo sets
`TG_TURNSTILE_HOSTNAMES=web.teagram.io,demo.teagram.io` and
`TG_ADVERTISE_DEMO_GUEST_SIGN_UP=true` so web.teagram.io advertises demo signup.
`TG_UPLOADS_DISABLED=true` is persisted in `.env`; deploy preflight adds it to
older prepared files and rejects a value that would enable client uploads.
The seed overlay temporarily enables uploads for the private API seeder.
Bootstrap and weekly reseed stop edge and HAProxy before seeding, then recreate
teagramd with production configuration and verify the private path before edge
starts. During a rolling deploy, the current server continues serving while
the candidate is seeded. The candidate has no host-published port and is not
in HAProxy's backend until seed verification finishes and Compose recreates it
with production configuration; HAProxy enables it only afterward.

### Upload shutdown threat model

Client uploads may contain personal photos and documents. Their bytes first
occupy per-user temporary upload parts, then assembled file metadata and blob
objects; short-lived upload grants bind staged files to their owner. An
authenticated client could otherwise try the regular or big part RPCs, or
bypass them through uploaded message media, profile photos, or chat/channel
photos. With uploads disabled, each of those byte-ingress RPCs returns the
same `UPLOADS_DISABLED` error before its storage path. The setting is loaded
at process startup and applies through the shared handler on every listener.
There is no HTTP file-upload route. Demo sticker import is off, and catalog
sticker assets are bundled rather than accepted from clients.

`messages.sendMultiMedia` accepts owner-bound grants created by
`messages.uploadMedia`; it carries no file bytes, and disabled mode prevents
new grants from being issued. The switch does not erase parts, blobs, or grants
accepted before it was enabled. Temporary parts and grants keep their existing
TTL; completed file reads still enforce file-location and owner checks, and
`upload.getFile` remains available for authorized existing media. The seed
override is private to the internal Docker backend:
bootstrap and weekly wipe stop both public proxy paths, while rolling deploys
keep the candidate out of HAProxy until production configuration is restored.
The backend network has no external route or published server port. LXC root
and containers already attached to that backend are trusted; control of either
can reach the seed API while the override is active.

For an authorized browser QA run, a test harness can set the demo QA token in
the web client origin's local storage under the key
`__teagram_turnstile_qa_bypass_token` before starting guest signup. The client
uses that test-only path without rendering the widget; there is no user-facing
setting or UI. Without the token, normal Turnstile verification remains in
place. The bypass applies only to `auth.signUp`; it does not skip the demo's
signup IP limit or registration checks. Keep token values out of terminal
transcripts, CI output, browser recordings, and issue comments.

The demo is budgeted for 2 GiB: teagramd uses `GOMAXPROCS=1`,
`GOMEMLIMIT=320MiB` and a 448 MiB container cap. PostgreSQL is capped at 320 MiB with
128 MiB shared buffers, 4 MiB work memory and 64 MiB maintenance work memory in
the demo-specific `postgresql.conf`; the dev stack keeps its own larger profile.
The seed and probe containers are capped at 256 MiB. If a host-side image pull
or the API seed cannot complete within the LXC limit, report it here rather
than increasing the host allocation. The demo LXC never builds the server
image.
Successful main CI publishes `ghcr.io/teagramhq/teagram-server-demo` from a
hosted runner, tagged with the tested commit SHA. The package must be public so
the isolated LXC can pull it without registry credentials. The SHA tag is only
for registry organization: deploys use the image digest recorded as an artifact
on the successful main CI run, never a tag lookup.

## First install

Do not begin until the Dev deploy for the selected `main` commit succeeds and
that commit's `client-config` includes the approved WebSocket endpoint.

On a trusted workstation, download the digest artifact from that commit's
successful CI push run and copy its value for the host command below:

```sh
gh run download "$CI_RUN_ID" -R teagramhq/teagram-server \
  --name "demo-image-digest-$MAIN_SHA" --dir ./demo-image
cat ./demo-image/demo-image-digest.txt
```

Run as root on the demo LXC, passing that value as `IMAGE_DIGEST`:

```sh
cd /opt/teagram-demo
# Only on a new host with no prepared .env or Turnstile secret:
deploy/teagram-demo/prepare-env.sh
install -d -o 65532 -g 65532 -m 0700 /srv/teagram-demo-blobs/data
# Copy the sha256 digest from the demo-image-digest-<main-SHA> artifact of that
# commit's successful CI push run, then enter it here:
read -r -p 'Trusted CI image digest: ' IMAGE_DIGEST
deploy/teagram-demo/init-demo.sh "$IMAGE_DIGEST"
unset IMAGE_DIGEST
```

The prepared charlie host already has its `.env` and Siteverify secret, so do
not rerun `prepare-env.sh` there. The next merged init or deploy script creates
the QA bypass token once if it is absent. Existing `.env` files may omit
`TG_ADVERTISE_DEMO_GUEST_SIGN_UP`; Compose supplies the approved `true` default
and validates the resolved value without rewriting the secrets.

`prepare-env.sh` prompts for the demo-specific Turnstile site and secret keys,
then creates root-owned `.env` mode 0600 with the site key, the secret-file
path and two different random 64-hex runtime secrets. Both the Siteverify
secret and the generated QA bypass token are stored outside the checkout in
root-owned files with group 65532 and mode 0440, inside a root-owned mode 0700
directory. Compose file secrets are bind-mounted and ignore uid/gid/mode
remapping, so each file's runtime group must match the non-root server uid/gid
65532. The root-only parent directory keeps the host paths inaccessible to
other host users; only the server containers are granted the secret mounts.
`demo_require_env` creates the QA token once when absent, preserves it across
deploys and weekly resets, and validates its ownership and format. It safely
upgrades a prepared root:root 0600 Siteverify secret to the runtime-readable
mode before validation. The setup refuses to replace existing secrets. Keep
`POSTGRES_PASSWORD` and `TG_AUTHKEY_ENC_KEY` unchanged during deploys and weekly
resets. Never display `.env`, the Siteverify secret, or the QA token in logs or
terminal transcripts.

`init-demo.sh` validates that the blob path is on the dedicated size-limited
filesystem, that the volumes and blob directory are fresh, and that Compose
renders the exposure and storage contract. It pulls only the digest supplied
from the selected main CI run, verifies the OCI revision label and pulled
repository digest, and stores that digest with the selected server slot in a
root-only release file.
Compose uses that immutable digest for the API seed, server and maintenance
containers. It creates the RSA identity once,
generates the discovery document, runs migrations, and uses the existing
`blob-mode-state.py initialize-local` publisher to record the durable local
blob authority. It then creates the ordinary seed owner, fills and verifies
the demo through the internal client API, restores production settings, checks
Turnstile, and only then starts HAProxy and edge. It checks the loopback
discovery route, CORS, WebSocket handshake and admin 404. A failed partial
install keeps the public edge down; inspect the authority and containers
before retrying.

## Deploy path

The `Deploy.` workflow runs the dev server deploy and publishes a small
handoff artifact containing the exact tested SHA and CI run ID. On success,
the separate `Deploy demo.` workflow deploys that exact SHA to the public demo
and reports its own result. The demo deploy is disabled unless the `demo` GitHub
environment variable `TEAGRAM_DEMO_DEPLOY_ENABLED` is set to `true`.

Before enabling it, configure the `demo` GitHub environment with:

| Name | Kind | Value |
|---|---|---|
| `DEMO_DEPLOY_HOST` | variable | `100.100.162.10` (charlie's tailnet address; SSH forwards to the LXC) |
| `DEMO_DEPLOY_PORT` | variable | `2223` |
| `DEMO_DEPLOY_KNOWN_HOSTS` | variable | Pinned SSH host key line for `[100.100.162.10]:2223` |
| `DEMO_DEPLOY_SSH_KEY` | secret | Dedicated key forced to the demo deploy command |
| `TEAGRAM_DEMO_DEPLOY_ENABLED` | variable | `true` only after host setup and first install |

Make the Tailscale OAuth client ID and secret available to the `demo`
environment as well, unless they are repository-level secrets.

The key's `authorized_keys` entry must have no shell, forwarding or PTY access:

```text
command="/opt/teagram-demo/deploy/teagram-demo/forced-command.sh",restrict ssh-ed25519 <public-key> teagram-demo-deploy
```

`forced-command.sh` accepts the full commit SHA and trusted server image digest,
then separately accepts a bounded web archive tied to the current demo
checkout. It fetches `origin/main`, rejects non-main revisions and skips stale
server deploys. `Deploy demo.` downloads the server digest from the successful
server CI run named by the handoff artifact. `web-artifact-pin.json` first
records teagram-web PR #121's head, successful validation run, and artifact
digest reported by that run's log; this candidate pin is never deployed. After
#121 merges, the pin changes to the green `master` publication. The
read-only deploy preflight then requires the pin to match the public web
manifest and a successful, checksum-named publication artifact. The
secret-free `demo-web-artifact` CI job rebuilds and verifies the exact pinned
source, then uploads its archive and SHA-256 sidecar with that successful
server CI run. The credentialed deploy job downloads the archive from that
exact run and verifies its checksum; it does not check out teagram-web or run
its dependency code. The forced command requires the release identity to match
the checked-in published pin, and the LXC recomputes the published artifact's
content digest before activation. The LXC never installs Node or builds the
web app. The web source commit and artifact digest are stored in
`.state/teagram-demo/active-release.json`. A PR workflow cannot influence the
server image digest by overwriting the package's SHA tag. `deploy-main.sh` pulls
only the supplied server digest and checks both its repository digest and
revision label before touching the database. It revalidates the blob authority and
running containers, then takes and validates a root-only PostgreSQL dump while
the existing server and public edge keep running. It applies migrations while
the old server remains live, starts the candidate in the inactive slot,
verifies seeded API reads and both HAProxy listener checks, then sends new
sessions to the candidate and drains the old server before stopping it.
The active slot and image digest are persisted atomically so a host or proxy
restart recreates the same release. WebSocket sessions already connected to
the old process drain during its configured 120-second graceful stop.

Before rendering the mounted HAProxy config, a deploy compares the running
HAProxy and edge Compose hashes with the target. A proxy image, `haproxy.cfg`
or HAProxy renderer change still stops the deploy for the maintenance path.
Nginx config is mounted as a directory so a checkout can replace
`nginx-config/nginx.conf` without leaving the edge container pinned to the old
file inode. A config-only change is syntax-checked and applied with Nginx's
graceful reload; the edge container is not recreated, and old workers drain
existing requests and WebSockets. `nginx.conf` remains an identical compatibility
copy for already-installed demo ops wrappers that fingerprint that path.

The first deploy that changes the existing single-file mount to this directory
layout recreates the only edge container once. That migration briefly interrupts
requests and drops active WebSockets at the public demo origin. This one-time
interruption is accepted for the weekly-wiped demo. Afterward, normal Nginx
config updates reload gracefully without recreating edge. If the server rollout
fails, the previous Nginx config is restored inside the mounted directory and
gracefully reloaded.

On a pre-cutover failure, the old server and edge stay available, the candidate
stops, and the previous release reference remains active. If traffic has reached
the candidate, the script switches back to the old server when possible. It
preserves the migrated database and the validated dump instead of restoring an
online snapshot that could discard writes accepted during rollout. Keep schema
changes backward-compatible with the previous server for this rollback path.
The dump and metadata remain under `.state/teagram-demo/backups/` until the
weekly wipe.

## Agent operations menu

The separate `multica-daemon@delta` root key is restricted to
`/usr/local/sbin/teagram-demo-ops`. It accepts `hostname`, `live`, `ps`,
`env-names`, `fingerprints`, `logs <RFC3339-UTC>`, and
`run <main-sha> <name>`. `run` fetches `origin/main` and executes only the
executable `deploy/teagram-demo/ops/<name>.sh` blob from that commit. The menu
does not accept shell commands. The `DEMO_DEPLOY_SSH_KEY` remains bound to
`forced-command.sh`; demo deploys still send a tested SHA and image digest.

After the installer commit is on `main`, stream that reviewed installer from
a trusted checkout to the demo LXC. It fetches and verifies the requested main
SHA before writing:

```sh
set -e
git show <main-sha>:deploy/teagram-demo/ops/install-teagram-demo-ops.sh |
  ssh -T teagram-demo 'sh -s -- check <main-sha>'
git show <main-sha>:deploy/teagram-demo/ops/install-teagram-demo-ops.sh |
  ssh -T teagram-demo 'sh -s -- apply <main-sha>'
```

The installer changes only the `multica-daemon@delta` line. Before replacing
it, it saves and verifies `authorized_keys` under the root-only
`/var/backups/teagram-demo-ops/` directory. Restore that saved file from a
separate root channel if the menu must be rolled back. The Gatus definitions
in `gatus.yaml` are HTTPS probes and do not use an SSH key.

The reviewed `turnstile-signin-on` and `turnstile-signin-off` ops scripts change
only `TG_TURNSTILE_SIGNIN_ENABLED` in the demo `.env`. They use the
`ROLLOUT_RUNNER_ADDED_ENV` path for the first assignment and the
`ROLLOUT_RUNNER_CHANGED_ENV` path for later boolean changes. Each change saves
the prior root-only `.env` under
`/var/backups/teagram-demo-turnstile-signin/`, validates the rendered Compose
settings and redeploys the active tested image through `deploy-main.sh`, which
takes a database dump and checks the candidate before HAProxy cutover. Signup
keys, signup advertisement and verification remain unchanged. To disable the
signin challenge, run `ssh -T teagram-demo 'run <main-sha> turnstile-signin-off'`;
to restore it, use `turnstile-signin-on`. If a deployment fails, the script
restores the prior `.env` snapshot and redeploys that flag state. It never prints
Turnstile key values.

After enabling public signin, verify signin and signup challenges from the
public web origin with
`go run ./deploy/teagram-demo/turnstile-signin-probe.go active`. The probe reads
only `TEAGRAM_DEMO_TURNSTILE_SITE_KEY`; it sends an invalid phone number and
prints the challenge family with the site key redacted. The enable/disable ops
script also requires TCP timeouts to the documented tailnet host, demo-host
LAN address and LAN router before reporting success. If the public signin probe
fails, disable signin with
`ssh -T teagram-demo 'run <main-sha> turnstile-signin-off'` and verify the
rollback with `go run ./deploy/teagram-demo/turnstile-signin-probe.go disabled`.

Do not use `docker compose down -v`. The named `pgdata`, `tgkey` and `tgblobs`
volumes have independent lifetimes; especially, removing `tgkey` changes the
server fingerprint and invalidates existing client authorizations.

## Weekly reset

`wipe-weekly.sh` drops and recreates only the `teagram` database and removes
all files below the capped blob mount. It preserves the `tgkey` volume, the
encryption keys and the RSA public identity, then runs migrations and seeds
through the private API path while the public edge, HAProxy and Turnstile egress
proxy are stopped. The wipe operates on the persisted active server slot.
It creates the ordinary `demo_seed_owner` account through
`teagramd admin create-user` to close the first-admin election before seeding.
Its freshly generated password is kept in the root-only
`/var/lib/teagram-demo/secrets/seed-owner-password` file. After the seed reports
all expected handles through the API, the wipe recreates teagramd with the
production proxy and Turnstile settings, requires a bare sign-up probe to
return `RECAPTCHA_CHECK`, and only then starts HAProxy and the public edge.

## Public guest account threat model

`autoGuest=true` removes signup friction on the public demo, so an anonymous
internet user can create a guest identity and use the public messaging and file
APIs. The primary abuse risks are automated account creation, spam, attempts to
consume shared CPU, connection and storage capacity, and content that users
should not treat as private. Demo accounts and their data are visible to the
server operator, are not suitable for sensitive information, and are removed
with the database and blob files during the weekly wipe.

Signup requires a valid Cloudflare Turnstile `signup` challenge before the
server charges its per-client-network limit of 5 signup attempts per hour.
Authentication code requests are limited to 10 calls per network per hour and
20 target phone numbers per network per 24 hours. Unauthenticated connections
are capped at 64 per client network and 1,024 per server process, with a
120-second lifetime. Per-account limits are 60 message sends per minute,
20 MiB uploaded per hour, and 256 MiB stored. Each file is capped at 512,000
bytes (`TG_UPLOAD_MAX_BYTES` overrides the 16 MiB base setting). Sticker
imports are disabled, and the HTTP admin paths return 404.

These are application-level abuse limits, not a global DDoS boundary. An
attacker with many source networks can distribute signup and connection load;
users behind the same carrier NAT or corporate egress can also share and exhaust
a per-network bucket. Resource limits bound the local blast radius but cannot
guarantee availability under a distributed flood. Turnstile and rate limits do
not make guest content private or retained: users should expect account, message
and file data to be inspectable by the operator and erased by the weekly reset.

## Stack data-retention threat model

MAIN-2203 covers sign-up, auto-join, seeding and scheduled activity; this
section covers the demo stack's storage and deploy backups. Demo users control
account, message and uploaded-file data. A stale copy can recover that data
after the live service has been wiped.

The retained copies are the `teagram` database in `pgdata`, uploaded files in
`tgblobs`, and deploy snapshots under `.state/teagram-demo/backups/`. Each
online PostgreSQL snapshot is an unfiltered copy of the live database and can
contain sign-up, account, authentication and message data. The dump is not
encrypted separately by the stack. Snapshots also contain rollback metadata,
including the prior client configuration. The backup root and each snapshot
directory are root-owned mode `0700`; dump and metadata files are root-owned
mode `0600`. Direct filesystem access is therefore limited to LXC root, but
the Docker daemon and privileged Incus or host administrators can also read
or copy these files. The forced CI deploy key has no shell, forwarding or PTY
and accepts only a commit SHA; it cannot browse or download the backup tree.

There is no per-deploy expiry. Successful deploys, failed rollouts and
incomplete backup attempts retain their backup directories until the next
successful weekly wipe, scheduled for Sunday 03:00 UTC with up to 15 minutes
of randomized delay. If preflight or pruning fails, the wipe aborts before
changing the database or blobs and backups remain in place. After preflight
and stopping writers, the wipe removes every entry under the backup root
before dropping the live database and clearing blobs. It never restores a
pre-wipe snapshot. This is filesystem deletion, not verified cryptographic
erasure; the stack does not establish retention for host-level snapshots or
copies outside the LXC. Do not keep such copies beyond the same weekly
erasure window. The `tgkey` volume and `.env` keys preserve server identity
and encryption; they are not user content and must not be removed or exposed.

Docker logs are size-bounded but are not cleared by the wipe, so application
and deploy logs must not contain message bodies, authentication tokens or
passwords.

### Auth-key unbind diagnostic logs

The demo enables `TG_LOG_AUTH_KEY_UNBINDS` on both server slots. Each event
contains the operation, reason, outcome, numeric auth-key ID and, when known,
numeric user ID. These identifiers can correlate authorization events and link
them to a demo account, so treat the entries as sensitive account data even
though they contain no auth-key bytes, tokens, passwords, phone numbers, IP
addresses or message content. The IDs are correlators, not login credentials.

LXC root and Docker daemon administrators can read the raw container logs. The
separate `multica-daemon@delta` root key is restricted to the operations menu;
its `logs` command can return at most 200 lines per running `teagramd` or
`teagramd-next` container since a requested UTC timestamp. The CI deploy key is
restricted to the deploy command and cannot invoke that log reader. Limit
host/Docker administration and the operations key to trusted demo operators.
Compose uses Docker's `json-file` driver with `max-size=10m` and `max-file=3`
per container. This is a size limit, not an age limit: quiet logs can outlive
the weekly database wipe, which does not clear Docker logs. The stack
configures no external log forwarder; any exported or forwarded copy needs the
same access controls and an explicit retention limit.

The seed script interface is:

```sh
deploy/teagram-demo/seed-demo.sh --seed \
  --endpoint teagramd:2443 \
  --owner-username demo_seed_owner \
  --owner-password-file /var/lib/teagram-demo/secrets/seed-owner-password
deploy/teagram-demo/seed-demo.sh --verify-seed \
  --endpoint teagramd:2443 \
  --owner-username demo_seed_owner \
  --owner-password-file /var/lib/teagram-demo/secrets/seed-owner-password
deploy/teagram-demo/seed-demo.sh --verify-turnstile --endpoint haproxy:2443
```

The seed endpoint is available only on the demo's internal Docker network. The
Turnstile probe uses HAProxy's private MTProto listener so its request reaches
the production server with an accepted PROXY-v2 header. The seed and verification
modes use the client API; they do not write account data with SQL. Ten fictional
accounts include the ordinary seed owner. Nine generated
passwords are stored in `/var/lib/teagram-demo/secrets/demo-accounts/` as a
root-owned mode 0600 JSON inventory under a root-owned mode 0700 directory.
After a weekly database wipe, the accounts receive fresh random passwords;
password values never appear in process arguments, environment variables,
logs, or this checkout. The seed container mounts only the client config and
the credential files it needs, has no database credentials, and runs on the
internal backend network with a read-only root filesystem.

`test-seed-compose.sh` runs a second seed pass against the private Compose API
and checks that verified marker counts and search results stay stable. It also
checks that the stored wipe generation survives a server restart; the weekly
wipe passes the previous value so the test confirms a fresh database changed
it before the public edge starts.

## Scheduled activity

Scheduled activity is disabled until explicitly installed. From
`/opt/teagram-demo`, enable it with one command:

```sh
sudo deploy/teagram-demo/install-activity.sh
```

The timer runs about every ten minutes with a randomized delay. Each run uses
the seeded `demo_ren` account to post one fixed fictional message in the
`river` megagroup (`teagramdemoriver`). The root-owned settings file at
`/etc/default/teagram-demo-activity` can select `river`, `garden`, or
`workshop`, and set a limit from 1 to 6 posts per hour and a minimum interval
of at least five minutes. The client checks the resolved peer is one of those
seeded megagroups before sending. It does not reply or mention anyone.

The activity session and persistent rate limit live in
`/var/lib/teagram-demo-activity`, outside the weekly wipe. The wipe pauses the
timer during reseeding and resumes it only after the seed and public checks
succeed. It refreshes the systemd credential from the seed's root-only account
inventory for every run. The runner stages it in a root-only transient `/run`
directory for Docker's bind mount, removes the file after the run, and systemd
removes the directory on stop. The password is not passed in arguments,
environment variables, or logs. The activity container can reach only the
internal backend network and has no database credentials.

The three content broadcast channels each contain 32 posts, including text,
replies, a generated photo, a poll, and a reserved `.invalid` link. The welcome
channel has the pinned weekly-wipe note. Three public groups contain fictional
conversations and pinned messages. A sample private note between two demo
accounts carries a reaction because the server currently supports reactions on
user and basic-chat messages, not channel messages.
The fresh `demo_searcher` account verifies that every public channel and group
appears in search without joining it. Avatar and landscape JPEGs are generated
deterministically in `cmd/demo-seed/assets.go`; no remote image or external
license is involved. The temporary seed Compose overlay disables Turnstile,
uses bounded signup/code limits, and disables the message-send rate limit only
while edge and HAProxy are down. Production Compose settings are restored
before the edge can start.

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

`gatus.yaml` supplies the `teagram-demo` group checks for upstream health, the
discovery JSON, the WebSocket listener and the public admin 404, including TLS
expiry. Pebble checks Squid's local listener, and the upstream check includes
HAProxy's server and a CONNECT probe through Squid to Cloudflare, so it alerts
if the Turnstile egress path is unavailable. Both new and existing services
use bounded container logs.
Merge these endpoints into the existing Gatus status page and attach the alert
provider already configured there. The alert provider credentials live outside
this repository, so the snippet does not invent a provider name or secret.
