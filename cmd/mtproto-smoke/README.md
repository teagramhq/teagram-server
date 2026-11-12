# MTProto deploy smoke security model

`Deploy.` runs one no-account MTProto exchange after a successful deployment.
The canonical deploy script runs the probe from the exact deployed image in a
one-shot container on the teagram host. Host networking lets it fetch the same
HTTPS discovery URL and reach the tailnet MTProto listener without granting the
GitHub runner access to the Serve endpoint. The probe verifies the public
endpoint and key-exchange path; it does not verify user sign-in.

## Key and data flow

The command fetches the public client-discovery document over HTTPS, refuses
redirects, and validates its version, DC, port, endpoint, and RSA public key. It
then dials the fixed tailnet address `100.124.236.66:2443` using obfuscated
abridged transport. It sends no phone number, username, password, account
identifier, or application RPC. The generated auth key exists in process
memory only and is not logged or written to the runner workspace; Go does not
guarantee zeroization before process exit.

The separate `--username-file` and `--password-file` mode signs in to a
provisioned account and verifies that the authenticated username matches. It
requires `--discovery-file`, protected regular files with one line each, and is
used only by the disposable k8s cutover migration test. That test creates a
temporary account in the source database, migrates its account and server
identity, signs in on the restored target, then repeats sign-in against the
source after the target is stopped for rollback. The default deployment probe
does not enable this mode or read credentials.

The client requests a 60-second temporary key, but that is not a server-side
expiry guarantee today. The MTProto server's exchange path drops the temporary
expiry metadata and persists every resulting key in Postgres, encrypted at
rest, as an unbound auth-key row. The bound-authorization sweeper does not
remove unbound rows, and `auth.logOut` only unbinds. Therefore each successful
probe leaves one encrypted, unbound key row until explicit cleanup. That key
does not represent an account session and the probe performs no login RPC, but
the current server does not expire it after 60 seconds. Avoid ad hoc repeated
production probes; the workflow runs it once per deployment.

The optional `--expect-user-invite-quota N` mode calls `help.getAppConfig`
through the selected MTProto endpoint. Use `--websocket-endpoint` and
`--websocket-origin` to check a public WebSocket listener; its app-config probe
uses the supplied discovery document for the server identity and requires the
listener's allowed Origin. It verifies the advertised registration mode,
invite-enabled flag, and exact quota. The call creates one unbound auth-key row
as described above; it sends no account credentials or login RPC.

The deploy and QA probes accept `--endpoint` and `--discovery-url` overrides.
The production workflow keeps the defaults above; a private QA run can target
its own HTTPS discovery document and raw MTProto endpoint without changing the
production probe target.

## Privilege boundary

The workflow runs after successful CI on `main` or manual dispatch, in the
production environment, with read-only repository permissions. The canonical
script runs the smoke binary from the deployed image as uid 65532 in a
read-only container with all capabilities dropped and no mounted files or
credentials. The container uses the host network only so it can reach the
host's Tailscale Serve URL and MTProto listener. The runner does not check out
or execute the probe; its deploy key is removed with an `always()` step after
the remote deploy command returns. Output reports the endpoint and exchange
result without logging key material.
