# MTProto deploy smoke security model

`Deploy.` runs one no-account MTProto exchange after a successful deployment.
The probe verifies the public endpoint and key-exchange path; it does not
verify user sign-in.

## Key and data flow

The command fetches the public client-discovery document over HTTPS, refuses
redirects, and validates its version, DC, port, endpoint, and RSA public key. It
then dials the fixed tailnet address `100.124.236.66:2443` using obfuscated
abridged transport. It sends no phone number, username, password, account
identifier, or application RPC. The generated auth key exists in process
memory only and is not logged or written to the runner workspace; Go does not
guarantee zeroization before process exit.

The client requests a 60-second temporary key, but that is not a server-side
expiry guarantee today. The MTProto server's exchange path drops the temporary
expiry metadata and persists every resulting key in Postgres, encrypted at
rest, as an unbound auth-key row. The bound-authorization sweeper does not
remove unbound rows, and `auth.logOut` only unbinds. Therefore each successful
probe leaves one encrypted, unbound key row until explicit cleanup. That key
does not represent an account session and the probe performs no login RPC, but
the current server does not expire it after 60 seconds. Avoid ad hoc repeated
production probes; the workflow runs it once per deployment.

## Privilege boundary

The workflow runs after successful CI on `main` or manual dispatch, in the
production environment, with read-only repository permissions. The deploy SSH
key is removed with an `always()` step before source checkout and the smoke;
checkout uses the exact tested SHA with persisted credentials disabled. The
smoke step receives no SSH key, database credential, or user credential. The
runner remains connected to the tailnet as `tag:ci-deploy`; the tailnet ACL
defines every destination available to the tested code, including the fixed
MTProto endpoint and HTTPS discovery service. A compromised tested commit or
runner could issue pre-auth traffic to any ACL-permitted destination, but this
step has no account session or database credential. Output reports the endpoint
and exchange result without logging key material.
