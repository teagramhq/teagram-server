# Public demo on lima

This is the public demo stack (`demo.teagram.io`) as a plain Docker Compose
project on lima, without qa-invite. The root `docker-compose.yml` and the
charlie demo files are unchanged; this directory is a separate Compose file.

## Files

| File | Purpose |
| --- | --- |
| `compose.yaml` | The stack: postgres, migrate, one teagramd, turnstile-egress, haproxy, edge, cloudflared. |
| `postgresql.conf` | Postgres settings sized for its 49 MiB limit. |
| `prepare-blobs.sh` | Root, idempotent: fully allocated 4 GiB ext4 filesystem at `/srv/teagram-demo-blobs`, preserving 100 MiB free on the host filesystem, and proxy runtime directories outside the privileged checkout. Run once before starting the stack. |
| `install-egress-isolation.sh` | Root, idempotent: installs source-scoped `FORWARD`, `DOCKER-USER`, and host `INPUT` rules and enables ordered OpenRC firewall and stack services. |
| `egress-isolation-threat-model.md` | Scope, trust boundaries, controls, and residual risks for the Lima demo firewall policy. |
| `check.sh` | Read-only preflight. Pulls nothing, starts and stops nothing, writes nothing. Ends with `would_apply` or `would_reject`. |
| `test-lima-compose.sh` | Renders the stack against fixture values and asserts the acceptance criteria. CI runs it through `test-demo-compose.sh`. |
| `deploy-main.sh` | Stops the old server, migrates, starts one new server, checks `/healthz`, and restores the previous image if the new release fails. |
| `install-deploy-entry.sh` | Idempotent root installer for the `teagram-deploy` account and the port 2223 SSH listener. It moves the Stage 1 proxy temp and HAProxy socket paths into the external runtime directory before scanning checkout ownership. Run by Steward at cutover. |

## Root-only inputs on the host

Nothing secret is in the repository.

| Path | Mode | Contents |
| --- | --- | --- |
| `/var/lib/teagram-demo/lima.env` | root:root 0600 | `POSTGRES_PASSWORD`, `TG_PUBLIC_LINK_PREFIX`, `TG_AUTHKEY_ENC_KEY`, `TEAGRAM_DEMO_TURNSTILE_SITE_KEY`, the bootstrap `TEAGRAM_DEMO_IMAGE_REF` (`name@sha256:`), and the `TG_RSA_KEY_FINGERPRINT` written by identity import. Once present, `.state/teagram-demo/active-release.json` selects the validated active image on boot. |
| `/var/lib/teagram-demo/secrets/` | root:root 0700 | Directory. |
| `.../secrets/turnstile-secret-key` | root:65532 0440 | Siteverify secret, readable by the server uid through its group. |
| `.../secrets/turnstile-qa-bypass-token` | root:65532 0440 | QA bypass token, readable by the server uid through its group. |
| `.../secrets/cloudflared.env` | root:root 0600 | `TUNNEL_TOKEN=...` and nothing else. |

`TEAGRAM_DEMO_LIMA_ENV_FILE` and `TEAGRAM_DEMO_SECRETS_DIR` override the
default paths, for tests only. `TEAGRAM_DEMO_BLOB_MOUNT` can likewise point the
preflight at a test fixture.

Import the copied Charlie identity before starting the Lima stack:

Set `CHARLIE_RSA_SPKI` to `.mtproto.rsa_spki` from Charlie's live
`client-config` output. Use that live value, not a value copied with the key.

```sh
deploy/teagram-demo/init-demo.sh \
  --import-key /root/server_key.pem \
  --expected-rsa-spki "$CHARLIE_RSA_SPKI"
```

The command reads the root-only `lima.env`, installs the key into the Compose
`tgkey` volume, verifies `client-config`, and writes the matching public RSA
fingerprint into `lima.env`. Lima server starts require that fingerprint.

`docker compose config` prints environment values, including the password and
the tunnel token. Never print it to a log, a comment or a PR.

## Memory budget

Limits of the long-running services, in MiB. The sum stays at or below 200.

| Service | Limit | Measured (charlie unless noted) |
| --- | --- | --- |
| postgres | 49 | 83 (128 MB `shared_buffers`); 43.82 under 9-client pgbench with 8 MB `shared_buffers` (local ARM64 image, measured under the prior 56 MiB cap) |
| teagramd | 46 | 46 on Charlie before Lima; 30.09 MiB profile-only at 40 MiB; pinned-image combined runs at 37.72 and 43.25 MiB with `GOMEMLIMIT=24MiB`; prior built-image WebSocket and memory-gate runs used a 44 MiB cap |
| cloudflared | 25 | 25 |
| turnstile-egress (squid) | 24 | 23 |
| haproxy | 10 | 10 |
| edge (nginx) | 3 | 2 |
| **Total** | **157** | |

The Lima Compose environment caps the server pool at `TG_PG_MAX_CONNS=3`.
The rollout uses a short maintenance window: it stops the old teagramd before
starting the new one, checks `/healthz`, and restarts the previous image if the
new image fails. The two revisions never run together, so deploys do not need
headroom for a second server. The 64 MiB `migrate` container runs while the
server is stopped. A 20-second, 9-client pgbench run sampled 43.82 MiB under
the prior 56 MiB container limit; the Lima limit is 49 MiB, leaving 5.18 MiB
above that sample. The local host is ARM64,
so this measurement used its local PostgreSQL 18.7 ARM64 image; the Lima
Compose image remains pinned to linux/amd64.

Hosted x86 CI measured the pinned linux/amd64 service image in a 44 MiB memory
and swap cgroup with `GOMEMLIMIT=24MiB`, `GOMAXPROCS=1`, and
`TG_PG_MAX_CONNS=3`. One synchronized 1600×1600 profile-photo upload and one
media-photo send both passed in phase
`concurrent_profile_and_media_photo_uploads`. Run
[`45710872548`](https://github.com/teagramhq/teagram-server/actions/runs/45710872548)
peaked at 37.72 MiB (39,555,072 bytes); run
[`45715431287`](https://github.com/teagramhq/teagram-server/actions/runs/45715431287)
peaked at 43.25 MiB (45,346,816 bytes), 0.75 MiB below the cap. Both had
`memory.max=46,137,344`, `max=0`, `oom=0`, and `oom_kill=0`; both uploads passed.
These pinned release runs did not enable the WebSocket listener. The image was
`ghcr.io/teagramhq/teagram-server-demo@sha256:65c6c2445df3a1e9e4a11160d78c9cc45dbaf927d309262e255165e2d4ad41e6`, revision
`609ae698dbacbb94d1028d6d6e45327a82078080`. At `df13493`, the built-image
probe enabled Lima's WebSocket listener at `:2446` with the Compose origin and
browser-only settings. It peaked at 39.13 MiB (41,029,632 bytes), with
`memory.max=46,137,344`, `max=0`, `oom=0`, and `oom_kill=0`; both uploads passed
([CI job](https://github.com/teagramhq/teagram-server/actions/runs/45721450279/job/139580703653)).
The current gate runs three times on each head in a 46 MiB cgroup with
`GOMEMLIMIT=18MiB` and fails unless each run stays at or below 44 MiB. The
previous 20 MiB soft limit reached 44.31 MiB and 44.04 MiB on PR head
`fab6469765ac507753abbba443ee2e42559e9b00`, leaving less than the required
two-MiB margin. The lower limit keeps that margin under concurrent uploads; the
46 MiB cap remains within the 200 MiB Lima budget. Earlier 44 MiB-capped runs
completed the same synchronized uploads successfully. The run on PR head
`e1b212ae3457194d5500219bbfc76c6593f4e58e` peaked at 43.00,
40.13, and 41.98 MiB; the first exceeded the former 42 MiB threshold, although
all uploads passed and `max`, `oom`, and `oom_kill` stayed at zero. On PR head
`9d960759718f21e34998f4cf33524e1742c2401f`, the prior
44 MiB gate had peaks of 35.87, 36.16, and 40.22 MiB; each run reported
`max=0`, `oom=0`, and `oom_kill=0` ([CI job 139612009143](https://github.com/teagramhq/teagram-server/actions/runs/45730687633/job/139612009143)).

The long-running Lima stack uses 157 MiB of limits. `migrate` (64 MiB) is
one-shot and outside the sum.

## Blobs

`prepare-blobs.sh` reserves the full 4 GiB backing image at
`/var/lib/teagram-demo/blobs.img`, formats it ext4 with no reserved blocks, and
mounts it at `/srv/teagram-demo-blobs` through `/etc/fstab`. It checks that at
least 100 MiB remains available on the backing filesystem before and after
reservation. The demo's
`/srv/teagram-demo-blobs/data` directory (owner 65532, mode 0700) is a bind
mount, so a write past 4 GiB fails inside the demo with ENOSPC. The rest of the
root disk keeps at least 100 MiB free. The script formats only an image it
created during that invocation. An existing image must be identified as ext4;
unknown images and `blkid` errors are rejected without formatting. It also
refuses to mount over a non-empty directory. The host needs GNU coreutils and
`fallocate` from util-linux, in addition to e2fsprogs and util-linux `blkid`.

## Checking

```sh
TEAGRAM_DEMO_LIMA_ENV_FILE=/var/lib/teagram-demo/lima.env \
  deploy/teagram-demo/lima/check.sh
```

The check needs `docker`, `jq`, `python3`, GNU `stat`, and `mountpoint` and
`findmnt` from util-linux. It fails if the blob cap is not mounted read-write,
so it is expected to reject until `prepare-blobs.sh` has run. Compose parser
diagnostics are withheld because malformed dotenv values can appear in its
error text.

## Deploy entry and rollout

At cutover, derive the public half of the existing `DEMO_DEPLOY_SSH_KEY` and
pass it to the root installer as `TEAGRAM_DEMO_DEPLOY_PUBLIC_KEY`. The installer
creates a non-root `teagram-deploy` account outside the Docker group and a
separate OpenRC managed sshd bound only to `100.117.107.78:2223`. The account
can authenticate with the configured public key and invoke only
`/usr/local/sbin/teagram-demo-deploy`; the root wrapper checks the command again
before it reaches the root-owned checkout. Lima's regular sshd configuration
and listener on port 22 are verified unchanged.

The deploy workflow continues to read host, port, known hosts, and login user
from the `demo` environment variables. At cutover, set those values to lima and
`teagram-deploy`. QA invite remains pinned to charlie.

Before stopping the current service, the deploy checks the Lima setup, verifies
the active image is locally available for rollback, checks the live `/healthz`,
and writes a verified PostgreSQL dump under `.state/teagram-demo/backups/`.
It pulls the digest-pinned image, stops and removes the old server container,
runs migrations, starts the new image, and waits up to 60 seconds for `/healthz`
on loopback port 8080. If the new image fails, it removes the candidate,
restores the previous image and checks it. The database is not restored during
rollback; schema changes must remain compatible with the previous image, and
the verified dump remains available for recovery. If rollback health fails,
the deploy reports both revisions and exits non-zero without further database
operations.

After a successful deploy or rollback, Docker images for this server are kept
by image ID: the running release and the two previous successful releases.
Other repositories and images referenced by any container are left alone.
This retention policy also applies to charlie's `teagram-dev` deployments.

## Egress isolation

Run the root installer while Docker is running:

```sh
deploy/teagram-demo/lima/install-egress-isolation.sh
rc-service teagram-demo-stack start
deploy/teagram-demo/lima/install-egress-isolation.sh check
```

The installer manages the `TEAGRAM_DEMO_EGRESS` chain and one source hook per
demo subnet at the start of `FORWARD` and in `DOCKER-USER`, plus a separate
`TEAGRAM_DEMO_INPUT` chain for traffic addressed to Lima. The source-scoped
`FORWARD` hooks run before host rules that could terminate demo packets, while
preserving the order of those host rules for all traffic. The egress chain
allows traffic between demo networks, drops RFC1918, carrier-grade NAT,
link-local and multicast destinations, then returns all other traffic to
Docker. The INPUT chain allows established replies first, then blocks those
destinations and each bridge gateway address without a subnet return. Docker's
embedded DNS is queried at `127.0.0.11`; no direct DNS exception to the bridge
gateways is needed. Other containers and host-originated traffic do not match
the demo source ranges.

If any Docker demo networks are absent, apply reports their names and defers
IPv6 updates without removing an existing IPv6 chain; `check` rejects until all
three networks exist. The stack service creates the networks without starting
containers, reapplies IPv4 and IPv6 rules, then starts the stack.

OpenRC enables `teagram-demo-egress` after Docker and a supervised
`teagram-demo-stack` service after the firewall service. Every Compose service
has `restart: "no"`, so dockerd cannot start demo containers before OpenRC has
installed isolation. The stack supervisor creates the networks, applies the
rules, and then starts Compose; every ten seconds it checks the six long-running
services and restarts only missing services one at a time in dependency order,
waiting up to 120 seconds for each service to become healthy or start. Recovery
starts `teagramd` before waiting for HAProxy's health check, and disables
dependency startup so the one-shot `migrate` service stays stopped. A first boot
without an active release uses project Compose startup to run the bootstrap
migration; respawns with an active release use migration-free recovery. It
reapplies the rules before recovery. It retries
while dockerd is unavailable and shares the deploy lock, so it does not start an
old release during a rollout. OpenRC respawns the supervisor if it exits. Its
output is sent to syslog with the `teagram-demo-stack` tag. Start and stop the
stack with `rc-service teagram-demo-stack start|stop`; invoking Compose directly
bypasses the ordering guarantee. The read-only `check` mode reports missing,
duplicate, misordered or drifted hooks and chain rules without changing them.

On Lima, run the probe as root after the Compose stack is up:

```sh
python3 deploy/teagram-demo/probe_tailnet_isolation.py
```

The probe first confirms the tailnet peer, Lima's private default-route
gateway, and each demo bridge gateway answer from the host namespace, then
enters the running `turnstile-egress` container's network namespace. It
requires timeouts to those gateways and Lima's own private addresses; it adds
Lima's IPv6 tailnet address only when Docker reports IPv6 enabled on the
`teagram-demo_egress` network. It then requires Docker embedded DNS and a
verified HTTPS response from `example.com`. The existing `--charlie` mode keeps
teagram-dev's host-network checks for its Turnstile rollout.

To roll the firewall change back, run as root:

```sh
rc-service teagram-demo-stack stop
rc-update del teagram-demo-stack default
rc-update del teagram-demo-egress default
deploy/teagram-demo/lima/install-egress-isolation.sh remove
```

## Cutover notes

- The Cloudflare tunnel's origin for `demo.teagram.io` must be `http://edge:8080`
  once the stack runs on lima. The connector runs in a container, so
  `127.0.0.1` would be its own loopback.
- The deploy image runs only after `check.sh` succeeds. No second teagramd is
  started beside the active server.
