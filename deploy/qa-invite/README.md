# Invite-mode QA stack

This is an isolated Compose project on the teagram-demo LXC. It owns its
PostgreSQL and server identity volumes, its internal-only Docker network, and
its TLS/WebSocket proxy. It publishes only `10.96.46.70:443`; the public demo's
PostgreSQL, edge, HAProxy, Cloudflare Tunnel, and release state are untouched.
The listener is reachable at `teagram-demo.incus:443` from teagram-qa. Its
certificate is self-signed and must be trusted by the QA browser before using
the web client. Turnstile is absent.

The Compose memory limits total 1088 MiB at peak migration time. Postgres and
the server have separate persistent volumes. Logs use Docker's bounded JSON
driver. The `up` operation validates the pinned image revision and digest,
checks Postgres and the TLS discovery route, and takes a root-only database
dump before any later migration. A failed upgrade restores the previous image
and database where the previous release is available. `down` removes only the
containers and network; it retains volumes, the environment, TLS identity, and
rollback dumps.

## Operations

Run the `Deploy qa-invite.` workflow from `main` with one operation:

| Operation | Effect |
|---|---|
| `up` | Starts or updates the stack using the digest from successful main CI. |
| `status` | Shows service health, active image, and the private endpoint. |
| `create-account` | Creates an operator account and stores its handle/password in `/home/qa/.qa-accounts-invite` with mode 0600. |
| `down` | Stops the stack while preserving its data and identity. |

If the demo SSH response is lost after credentials are stored, the workflow
fails without deleting the QA credential or reporting account creation as
successful. Reconcile by checking sign-in from teagram-qa with the saved
credential. If the account exists, keep using that record. If it does not,
remove its stale line from the QA-owned credential file only after confirming
absence, then retry the workflow. A successful summary is written only after
the temporary runner password files and SSH keys are removed and verified.

The server uses `TG_REGISTRATION=invite`, `TG_USER_INVITE_QUOTA=5`, and
`TG_USER_INVITE_MIN_ACCOUNT_AGE=0` on the raw and WebSocket listeners. The
workflow accepts account names from 2 to 32 ASCII letters, digits, or
underscores, beginning with a letter.

From the teagram-qa checkout root, verify `help.getAppConfig` with the
repository smoke client. Its HTTPS discovery URL uses the stack's self-signed
certificate, so first save that certificate and trust it only for the QA
probe. Import the same certificate into the QA browser profile before using
the web client over WSS.

```sh
openssl s_client -connect teagram-demo.incus:443 -servername teagram-demo.incus \
  -showcerts </dev/null 2>/dev/null |
  sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' \
  > "$HOME/.qa-invite-server.crt"
chmod 0600 "$HOME/.qa-invite-server.crt"
SSL_CERT_FILE="$HOME/.qa-invite-server.crt" go run ./cmd/mtproto-smoke \
  --expect-user-invite-quota=5 \
  --endpoint teagram-demo.incus:443 \
  --discovery-url https://teagram-demo.incus/.well-known/teagramd/client
```

After merge, update the demo deploy key to the stable reviewed wrapper once via
the already restricted demo ops menu:

```sh
ssh -T teagram-demo "run <main-sha> install-qa-invite-command"
```

That script verifies the current-main wrapper, backs up the root
`authorized_keys`, and changes only the demo deploy key's forced-command path.
Its backup is under `/var/backups/teagram-demo-deploy/`.

## Account writer setup

`create-account` requires a restricted Ed25519 key in the `demo` GitHub
environment. Its public key is installed in teagram-qa's `qa` account with an
`authorized_keys` forced command pointing to `qa-account-writer.sh`; the key
cannot start a shell, forward ports, or open a PTY. Configure these `demo`
environment values:

| Name | Kind | Value |
|---|---|---|
| `QA_ACCOUNTS_HOST` | variable | `100.100.162.10` |
| `QA_ACCOUNTS_PORT` | variable | `2222` |
| `QA_ACCOUNTS_KNOWN_HOSTS` | variable | Pinned teagram-qa host key for charlie port 2222 |
| `QA_ACCOUNTS_SSH_KEY` | secret | Private half of the restricted account-writer key |

Install `qa-account-writer.sh` as
`/home/qa/.local/bin/qa-account-writer` with mode 0700. Add the generated
public key to `/home/qa/.ssh/authorized_keys` with this exact option form:

```text
command="/home/qa/.local/bin/qa-account-writer",restrict ssh-ed25519 <public-key> qa-invite-account-writer
```

The key is generated for this workflow only. Do not reuse a login or deploy
key. Set the private half as `QA_ACCOUNTS_SSH_KEY`; the public half is safe to
store in `authorized_keys`. The workflow's health check must return
`qa-account-writer=ready` before it stores or creates an account.

The demo deployment itself continues to use only `DEMO_DEPLOY_SSH_KEY` on
charlie port 2223. The QA writer key is used only by the create-account
operation.

## Rollback and teardown

Run `down` to remove only the QA Compose containers and isolated network. The
database volume and generated secrets remain available for a later `up`. If a
new image fails its checks, the deployment script restores the previous image
and a validated database dump; inspect the emitted backup path before retrying.
The project state is under `/opt/teagram-demo/.state/qa-invite`, with dumps in
its `backups/` directory. Reviewed Compose snapshots are retained in
`releases/<main-sha>` so bind-mounted configuration remains available after a
Docker or LXC restart. Do not remove this state while the QA issue is active.
