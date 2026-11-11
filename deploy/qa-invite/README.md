# Invite-mode QA stack

This isolated Compose project shares a host with the public demo but owns its
PostgreSQL and server identity volumes, its internal-only Docker network, and
its TLS/WebSocket proxy. It publishes only `10.96.46.70:443`; the public demo's
PostgreSQL, edge, HAProxy, Cloudflare Tunnel, and release state are untouched.
The listener is reachable at `teagram-demo.incus:443` from teagram-qa. Its TLS
certificate is issued by a private QA root CA. The edge serves the verified
public Web artifact at `https://teagram-demo.incus/` with a same-origin config
that selects the invite stack. The page and API WebSocket share the QA-only
origin, so the browser does not make cross-address-space requests from
`web.teagram.io`. Turnstile is absent.

HAProxy sends the browser peer address to Nginx with PROXY v2. Nginx trusts
that preamble only from HAProxy's fixed backend address, then overwrites
`CF-Connecting-IP` with the resolved client address on discovery and WebSocket
requests. teagramd trusts that header only from Nginx's fixed backend address;
the raw MTProto listener keeps socket-based addressing. The isolated QA backend
uses `198.18.131.0/24`, separate from the public demo's `198.18.130.0/24`.

The Compose memory limits total 1088 MiB at peak migration time. Postgres and
the server have separate persistent volumes. Logs use Docker's bounded JSON
driver. The `up` operation validates the pinned image revision and digest,
checks Postgres, TLS discovery, the Web config, and the Web root, and takes a
root-only database dump before any later migration. A failed upgrade restores
the previous image and database where the previous release is available.
`down` removes only the containers and network; it retains volumes, the
environment, TLS identity, and rollback dumps.

## Operations

Run the `Deploy qa-invite.` workflow from `main` with one operation:

| Operation | Effect |
|---|---|
| `up` | Starts or updates the stack using the digest from successful main CI. |
| `status` | Shows service health, active image, and the private Web endpoint. |
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

The first successful `up` prints the public QA root certificate and its SHA-256
fingerprint in the Actions summary. Copy the certificate to
`$HOME/.qa-invite-root.crt` on the QA box, then verify its fingerprint against
`tls_ca_sha256` in that summary. The root private key stays in the QA stack's
root-only state directory and is not mounted into a container. A Steward
installs the verified root in the system trust store so the desktop can
validate the server:

```sh
openssl x509 -in "$HOME/.qa-invite-root.crt" -noout -fingerprint -sha256 |
  cut -d= -f2 | tr -d ':' | tr '[:upper:]' '[:lower:]'

sudo install -o root -g root -m 0644 "$HOME/.qa-invite-root.crt" \
  /usr/local/share/ca-certificates/teagram-qa-invite.crt
sudo update-ca-certificates
```

As the QA user, import the same root into the Chromium certificate database
used for QA, then restart the browser:

```sh
nss_db="$HOME/.local/share/pki/nssdb"
if [[ -d "$HOME/.pki/nssdb" ]]; then nss_db="$HOME/.pki/nssdb"; fi
mkdir -p "$nss_db"
if [[ ! -f "$nss_db/cert9.db" ]]; then certutil -d "sql:$nss_db" -N --empty-password; fi
certutil -d "sql:$nss_db" -A -t 'C,,' -n 'Teagram QA Invite Root' \
  -i "$HOME/.qa-invite-root.crt"
```

The root has critical TLS server usage and name constraints for
`teagram-demo.incus` and `10.96.46.70`; subdomains are excluded. Client
enforcement of trust anchor constraints varies, so the root is still sensitive
system trust. The
`up` operation renews a leaf with 30 days or less remaining using the existing
CA and server key, reloads the TLS edge, and verifies the served certificate.
`status` reports both expiry dates and fails inside that window. Renewal occurs
only on `up`; arrange regular status checks while the stack is in use.

An existing root without the required constraints is rejected and retained.
Do not replace it silently: plan a trust migration that removes the old root
from both QA trust stores and installs the independently verified new root.
The same migration is required before the CA expires. `server.crt` contains the
active leaf followed by the root; a legacy `server-leaf.crt` is not used.
There is no CRL or OCSP service. If the CA or leaf key is compromised, isolate
the QA endpoint, remove this root from both trust stores, and coordinate a
verified replacement before reconnecting; assess affected credentials and
sessions.

From the teagram-qa checkout root, verify `help.getAppConfig` with the
repository smoke client. It uses normal system TLS verification:

```sh
go run ./cmd/mtproto-smoke \
  --expect-user-invite-quota=5 \
  --endpoint teagram-demo.incus:443 \
  --discovery-url https://teagram-demo.incus/.well-known/teagramd/client
```

The account should sign in at `https://teagram-demo.incus/`. The Chromium NSS
database path can vary; use the one shown by `chrome://settings/certificates`.
On Debian, install `libnss3-tools` if `certutil` is missing. See the
[Chromium Linux certificate guide](https://chromium.googlesource.com/chromium/src/+/main/docs/linux/cert_management.md)
for the browser's current NSS database path.
The browser must trust the root CA and should not use a TLS exception or per-app
certificate override.

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
If a proxy or Web artifact change needs to be reverted, run `down`, revert the
reviewed source change on `main`, then run `up` from the reverted main commit.
The database, operator accounts, legacy TLS files, QA CA, and rollback dumps
remain in place. Remove the QA root from the system and browser trust stores
only if the QA endpoint is being retired.
The project state is under `/opt/teagram-demo/.state/qa-invite`, with dumps in
its `backups/` directory. Reviewed Compose snapshots are retained in
`releases/<main-sha>` so bind-mounted configuration remains available after a
Docker or LXC restart. Do not remove this state while the QA issue is active.
