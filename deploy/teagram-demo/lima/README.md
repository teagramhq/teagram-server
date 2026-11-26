# Public demo on lima

This is the public demo stack (`demo.teagram.io`) as a plain Docker Compose
project on lima, without qa-invite. The root `docker-compose.yml` and the
charlie demo files are unchanged; this directory is a separate Compose file.

## Files

| File | Purpose |
| --- | --- |
| `compose.yaml` | The stack: postgres, migrate, teagramd, teagramd-next (profile `candidate`), turnstile-egress, haproxy, edge, cloudflared. |
| `postgresql.conf` | Postgres settings sized for its 49 MiB limit. |
| `prepare-blobs.sh` | Root, idempotent: fully allocated 4 GiB ext4 filesystem at `/srv/teagram-demo-blobs`, preserving 100 MiB free on the host filesystem. Run once at cutover. |
| `check.sh` | Read-only preflight. Pulls nothing, starts and stops nothing, writes nothing. Ends with `would_apply` or `would_reject`. |
| `test-lima-compose.sh` | Renders the stack against fixture values and asserts the acceptance criteria. CI runs it through `test-demo-compose.sh`. |

## Root-only inputs on the host

Nothing secret is in the repository.

| Path | Mode | Contents |
| --- | --- | --- |
| `/var/lib/teagram-demo/lima.env` | root:root 0600 | `POSTGRES_PASSWORD`, `TG_PUBLIC_LINK_PREFIX`, `TG_AUTHKEY_ENC_KEY`, `TEAGRAM_DEMO_TURNSTILE_SITE_KEY`, `TEAGRAM_DEMO_IMAGE_REF` (a `name@sha256:` reference), and the `TG_RSA_KEY_FINGERPRINT` written by identity import. |
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

Limits of the long-running services, in MiB. The sum stays at or below 200 even
when the `candidate` profile is enabled.

| Service | Limit | Measured (charlie unless noted) |
| --- | --- | --- |
| postgres | 49 | 83 (128 MB `shared_buffers`); 43.82 under 9-client pgbench with 8 MB `shared_buffers` (local ARM64 image, measured under the prior 56 MiB cap) |
| teagramd | 44 | 46 on Charlie before Lima; 30.09 MiB profile-only at 40 MiB; pinned-image combined runs at 37.72 and 43.25 MiB with `GOMEMLIMIT=24MiB`; built-image WebSocket run at 39.13 MiB; current `GOMEMLIMIT=20MiB` gate: 35.87, 36.16, and 40.22 MiB (worst) |
| teagramd-next (candidate) | 44 | Same image and `GOMEMLIMIT` as teagramd; combined workload is measured on the primary slot |
| cloudflared | 25 | 25 |
| turnstile-egress (squid) | 24 | 23 |
| haproxy | 10 | 10 |
| edge (nginx) | 3 | 2 |
| **Total with candidate** | **199** | |

The lima Compose environment caps each server's pool at `TG_PG_MAX_CONNS=3`.
During rollout, both server pools, their two LISTEN connections, and migrate
use at most nine PostgreSQL client backends. A 20-second, 9-client pgbench run
sampled 43.82 MiB under the prior 56 MiB container limit; the Lima limit is
49 MiB, leaving 5.18 MiB above that sample. The local host is ARM64,
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
The gate runs three times on each head with `GOMEMLIMIT=20MiB` and fails unless
each run stays at or below 42 MiB. On PR head `9d960759718f21e34998f4cf33524e1742c2401f`, the job-built linux/amd64 `teagramd:ci` image (`sha256:e55bef28bd32f747b0e9e017fb769522965ab6ec07168713de11656cd9d536c8`, revision `5833cd415f8ee313449604effffb6817f15ff607`) ran with `GOMAXPROCS=1`, `TG_PG_MAX_CONNS=3`, and WebSocket listener `:2446` (`https://web.teagram.io`, `browser_only=false`). All three synchronized runs completed phase `concurrent_profile_and_media_photo_uploads`; profile and media-photo uploads passed in every run. Peaks were 35.87 MiB (37,609,472 bytes), 36.16 MiB (37,920,768 bytes), and 40.22 MiB (42,176,512 bytes). Each reported `memory.max=46,137,344`, `max=0`, `oom=0`, and `oom_kill=0`. The worst peak is 3.78 MiB below the 44 MiB cap ([CI job 139612009143](https://github.com/teagramhq/teagram-server/actions/runs/45730687633/job/139612009143)).

The default stack uses 159 MiB of limits. `migrate` (64 MiB) is one-shot and
outside the sum. The candidate is included because it can run beside teagramd
during rollout.

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

## Cutover notes

- The Cloudflare tunnel's origin for `demo.teagram.io` must be `http://edge:8080`
  once the stack runs on lima. The connector runs in a container, so
  `127.0.0.1` would be its own loopback.
- Deploy and rollout (the candidate beside the active server) are not in this
  directory. The deploy entry point is a later sub-issue.
