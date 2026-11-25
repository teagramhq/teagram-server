# Public demo on lima

This is the public demo stack (`demo.teagram.io`) as a plain Docker Compose
project on lima, without qa-invite. The root `docker-compose.yml` and the
charlie demo files are unchanged; this directory is a separate Compose file.

## Files

| File | Purpose |
| --- | --- |
| `compose.yaml` | The stack: postgres, migrate, teagramd, teagramd-next (profile `candidate`), turnstile-egress, haproxy, edge, cloudflared. |
| `postgresql.conf` | Postgres settings sized for its 56 MiB limit. |
| `prepare-blobs.sh` | Root, idempotent: fully allocated 4 GiB ext4 filesystem at `/srv/teagram-demo-blobs`, preserving 100 MiB free on the host filesystem. Run once at cutover. |
| `check.sh` | Read-only preflight. Pulls nothing, starts and stops nothing, writes nothing. Ends with `would_apply` or `would_reject`. |
| `test-lima-compose.sh` | Renders the stack against fixture values and asserts the acceptance criteria. CI runs it through `test-demo-compose.sh`. |

## Root-only inputs on the host

Nothing secret is in the repository.

| Path | Mode | Contents |
| --- | --- | --- |
| `/var/lib/teagram-demo/lima.env` | root:root 0600 | `POSTGRES_PASSWORD`, `TG_PUBLIC_LINK_PREFIX`, `TG_AUTHKEY_ENC_KEY`, `TEAGRAM_DEMO_TURNSTILE_SITE_KEY`, `TEAGRAM_DEMO_IMAGE_REF` (a `name@sha256:` reference). |
| `/var/lib/teagram-demo/secrets/` | root:root 0700 | Directory. |
| `.../secrets/turnstile-secret-key` | root:65532 0440 | Siteverify secret, readable by the server uid through its group. |
| `.../secrets/turnstile-qa-bypass-token` | root:65532 0440 | QA bypass token, readable by the server uid through its group. |
| `.../secrets/cloudflared.env` | root:root 0600 | `TUNNEL_TOKEN=...` and nothing else. |

`TEAGRAM_DEMO_LIMA_ENV_FILE` and `TEAGRAM_DEMO_SECRETS_DIR` override the
default paths, for tests only. `TEAGRAM_DEMO_BLOB_MOUNT` can likewise point the
preflight at a test fixture.

`docker compose config` prints environment values, including the password and
the tunnel token. Never print it to a log, a comment or a PR.

## Memory budget

Limits of the long-running services, in MiB. The sum stays at or below 200 even
when the `candidate` profile is enabled.

| Service | Limit | Measured (charlie unless noted) |
| --- | --- | --- |
| postgres | 49 | 83 (128 MB `shared_buffers`); 43.82 under 9-client pgbench with 8 MB `shared_buffers` (local ARM64 image, measured under the prior 56 MiB cap) |
| teagramd | 44 | 46 on Charlie before the Lima cap; 30.09 MiB profile-only at 40 MiB; combined profile and media-photo uploads hit the 40 MiB cap (`max=1`, `oom=0`) |
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

Hosted x86 CI first measured the pinned linux/amd64 service image with a
40 MiB memory and swap cap, `GOMEMLIMIT=24MiB`, `GOMAXPROCS=1`, and
`TG_PG_MAX_CONNS=3`. Two synchronized 1600×1600 `photos.uploadProfilePhoto`
requests passed at 30.09 MiB (31,555,584 bytes), with `max=0`, `oom=0`, and
`oom_kill=0`. The combined profile-photo and media-photo run also passed both
uploads, but reached the 40 MiB cap (`memory.peak=41,943,040`,
`memory.max=41,943,040`, `max=1`, `oom=0`, `oom_kill=0`). The server slots now
have 44 MiB limits and the combined probe is rerunning at that cap. The image was
`ghcr.io/teagramhq/teagram-server-demo@sha256:65c6c2445df3a1e9e4a11160d78c9cc45dbaf927d309262e255165e2d4ad41e6`, revision
`609ae698dbacbb94d1028d6d6e45327a82078080` ([hosted CI run](https://github.com/teagramhq/teagram-server/actions/runs/45662146460)).

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
