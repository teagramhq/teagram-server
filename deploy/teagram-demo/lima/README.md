# Public demo on lima

This is the public demo stack (`demo.teagram.io`) as a plain Docker Compose
project on lima, without qa-invite. The root `docker-compose.yml` and the
charlie demo files are unchanged; this directory is a separate Compose file.

## Files

| File | Purpose |
| --- | --- |
| `compose.yaml` | The stack: postgres, migrate, teagramd, teagramd-next (profile `candidate`), turnstile-egress, haproxy, edge, cloudflared. |
| `postgresql.conf` | Postgres settings sized for its 64 MiB limit. |
| `prepare-blobs.sh` | Root, idempotent: 4 GiB ext4 filesystem at `/srv/teagram-demo-blobs`. Run once at cutover. |
| `check.sh` | Read-only preflight. Pulls nothing, starts and stops nothing, writes nothing. Ends with `would_apply` or `would_reject`. |
| `test-lima-compose.sh` | Renders the stack against fixture values and asserts the acceptance criteria. CI runs it through `test-demo-compose.sh`. |

## Root-only inputs on the host

Nothing secret is in the repository.

| Path | Mode | Contents |
| --- | --- | --- |
| `/var/lib/teagram-demo/lima.env` | 0600 | `POSTGRES_PASSWORD`, `TG_PUBLIC_LINK_PREFIX`, `TG_AUTHKEY_ENC_KEY`, `TEAGRAM_DEMO_TURNSTILE_SITE_KEY`, `TEAGRAM_DEMO_IMAGE_REF` (a `name@sha256:` reference). |
| `/var/lib/teagram-demo/secrets/` | 0700 | Directory. |
| `.../secrets/turnstile-secret-key` | 0444 | Siteverify secret, read by the server uid through a bind mount. |
| `.../secrets/turnstile-qa-bypass-token` | 0444 | QA bypass token. |
| `.../secrets/cloudflared.env` | 0600 | `TUNNEL_TOKEN=...` and nothing else. |

`TEAGRAM_DEMO_LIMA_ENV_FILE` and `TEAGRAM_DEMO_SECRETS_DIR` override the
default paths, for tests only.

`docker compose config` prints environment values, including the password and
the tunnel token. Never print it to a log, a comment or a PR.

## Memory budget

Limits of the long-running services, in MiB. The sum must stay at or below 200.

| Service | Limit | Measured on charlie |
| --- | --- | --- |
| postgres | 64 | 83 (128 MB `shared_buffers`) |
| teagramd | 48 | 46 |
| cloudflared | 32 | 25 |
| turnstile-egress (squid) | 32 | 23 |
| haproxy | 16 | 10 |
| edge (nginx) | 8 | 2 |
| **Total** | **200** | 189 |

migrate (64 MiB) is one-shot and outside the sum. teagramd-next (48 MiB) runs
only as a rollout candidate, and its headroom is the deploy's concern.

## Blobs

`prepare-blobs.sh` makes a 4 GiB sparse image at
`/var/lib/teagram-demo/blobs.img`, formats it ext4 with no reserved blocks, and
mounts it at `/srv/teagram-demo-blobs` through `/etc/fstab`. The demo's
`/srv/teagram-demo-blobs/data` directory (owner 65532, mode 0700) is a bind
mount, so a write past 4 GiB fails inside the demo with ENOSPC. The rest of the
root disk is untouched. The script refuses to format anything that is not ext4,
and refuses to mount over a non-empty directory.

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
