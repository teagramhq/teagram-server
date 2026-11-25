# Public demo on lima

This is the public demo stack (`demo.teagram.io`) as a plain Docker Compose
project on lima, without qa-invite. The root `docker-compose.yml` and the
charlie demo files are unchanged; this directory is a separate Compose file.

## Files

| File | Purpose |
| --- | --- |
| `compose.yaml` | The stack: postgres, migrate, teagramd, teagramd-next (profile `candidate`), turnstile-egress, haproxy, edge, cloudflared. |
| `postgresql.conf` | Postgres settings sized for its 56 MiB limit. |
| `prepare-blobs.sh` | Root, idempotent: 4 GiB ext4 filesystem at `/srv/teagram-demo-blobs`. Run once at cutover. |
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
default paths, for tests only.

`docker compose config` prints environment values, including the password and
the tunnel token. Never print it to a log, a comment or a PR.

## Memory budget

Limits of the long-running services, in MiB. The sum stays at or below 200 even
when the `candidate` profile is enabled.

| Service | Limit | Measured on charlie |
| --- | --- | --- |
| postgres | 56 | 83 (128 MB `shared_buffers`); 52 with the lima config under 8-client pgbench |
| teagramd | 40 | 46 before the lima `GOMEMLIMIT` reduction to 24 MiB |
| teagramd-next (candidate) | 40 | Same image and `GOMEMLIMIT` as teagramd; not measured at the new setting |
| cloudflared | 25 | 25 |
| turnstile-egress (squid) | 24 | 23 |
| haproxy | 10 | 10 |
| edge (nginx) | 4 | 2 |
| **Total with candidate** | **199** | |

The default stack uses 159 MiB of limits. `migrate` (64 MiB) is one-shot and
outside the sum. The candidate is included because it can run beside teagramd
during rollout.

## Blobs

`prepare-blobs.sh` makes a 4 GiB sparse image at
`/var/lib/teagram-demo/blobs.img`, formats it ext4 with no reserved blocks, and
mounts it at `/srv/teagram-demo-blobs` through `/etc/fstab`. The demo's
`/srv/teagram-demo-blobs/data` directory (owner 65532, mode 0700) is a bind
mount, so a write past 4 GiB fails inside the demo with ENOSPC. The rest of the
root disk is untouched. The script formats only an image it created during that
invocation. An existing image must be identified as ext4; unknown images and
`blkid` errors are rejected without formatting. It also refuses to mount over a
non-empty directory.

## Checking

```sh
TEAGRAM_DEMO_LIMA_ENV_FILE=/var/lib/teagram-demo/lima.env \
  deploy/teagram-demo/lima/check.sh
```

The check needs `docker`, `jq` and `python3`. It fails if the blob cap is not
mounted, so it is expected to reject until `prepare-blobs.sh` has run.

## Cutover notes

- The Cloudflare tunnel's origin for `demo.teagram.io` must be `http://edge:8080`
  once the stack runs on lima. The connector runs in a container, so
  `127.0.0.1` would be its own loopback.
- Deploy and rollout (the candidate beside the active server) are not in this
  directory. The deploy entry point is a later sub-issue.
