# Synthetic restore drill checker

`go build -o restore-drill-checker ./test/e2e/restore_drill_checker` builds the
opt-in checker executable. The trusted scratch orchestrator sets
`TEAGRAM_RESTORE_DRILL_INVOCATION_FILE` to the protected `invocation.json` path.
The executable always invokes `/run/app/teagramd admin create-user`; it does not
accept target or binary arguments.

The orchestrator must create the scratch database, daemon, credentials, and
identity in a run-owned isolated namespace before writing the invocation. It
must attest target ownership, network isolation, the generated daemon key, and
closed registration. Those booleans are trusted evidence from that orchestrator,
not a substitute for its isolation checks. The checker additionally requires a
literal loopback WebSocket endpoint and a private invocation directory owned by
its effective UID.

## Invocation files

The invocation directory must be mode `0700` and owned by the checker effective
UID. Each file must be a regular, single-link file owned by that UID, with the
exact mode shown below. Symlinks, extra JSON fields, and a prior `result.json`
are rejected.

| File | Mode | Contents |
| --- | --- | --- |
| `invocation.json` | `0600` | Schema and trusted run attestations shown below |
| `server.pub.pem` | `0400` | Newly generated 2048-bit RSA public key with exponent 65537 |
| `postgres.dsn` | `0600` | Scratch PostgreSQL URI used only by the admin command |
| `authkey.hex` | `0600` | 32-byte generated auth-key encryption value as 64 hex digits |
| `username` | `0600` | Fresh lowercase synthetic handle, optionally newline-terminated |
| `password` | `0600` | Fresh synthetic password, optionally newline-terminated |

Example `invocation.json`:

```json
{
  "schema_version": 1,
  "mode": "synthetic-only",
  "endpoint": "ws://127.0.0.1:2444/apiws",
  "scratch_target_owned": true,
  "network_namespace_isolated": true,
  "daemon_identity_pinned": true,
  "registration_closed": true
}
```

The endpoint must use `ws`, a literal IPv4 or IPv6 loopback address, an explicit
port, and `/apiws`. DNS names, TLS, redirects, and external addresses are not
accepted. The checker pins its MTProto client to that endpoint and the supplied
RSA key. It validates `help.getConfig` content but never uses advertised DC
addresses to choose a connection.

The admin command receives the PostgreSQL DSN and auth-key path in a restricted
environment and reads the password through stdin. Its identifier-bearing
stdout/stderr are captured in mode `0700` checker-owned temporary storage and
removed before return. The checker keeps session data in memory only. It needs
root to run `teagramd admin create-user`; a non-root invocation requires
non-interactive `sudo` configured for that command. The checker never passes a
password or DSN as a command argument.

## Result contract and cleanup

The process writes one JSON result to stdout. Once it opens an accepted
invocation directory, it also writes a mode `0600` `result.json` there; it
cannot write an artifact when the directory itself is rejected or the artifact
already exists. Stderr is silent. Exit code `0` means both checker assertions
passed; `1` means the checker failed or could not publish its result.
The schema contains only fixed phase/category/assertion values:

```json
{
  "schema_version": 1,
  "phase": "application_assertions",
  "category": "assertions_passed",
  "help_getConfig": "pass",
  "synthetic_signin": "pass"
}
```

Possible categories are `assertions_passed`, `invocation_invalid`,
`identity_collision`, `auth_failure`, `key_mismatch`,
`configuration_invalid`, `identity_mismatch`, `connection_failure`,
`provisioning_failure`, `timeout`, `checker_cleanup_failure`, and
`artifact_failure`. The two assertion values are `pass`, `fail`, and `not_run`.
The result identifies neither the account nor any target, credential, resource,
raw error, or timing. A successful result only records the configuration and
synthetic sign-in assertions; it does not claim that a restore drill or cleanup
passed.

Phases are `invocation`, `help_get_config`, `provisioning`, `synthetic_signin`,
`application_assertions`, `checker_cleanup`, and `result_artifact`.

After collecting the result, the outer orchestrator owns removal of the
invocation directory and its secret files, the scratch account/database,
namespace, daemon and generated keys. The checker intentionally does not delete
the account, database, daemon, or invocation inputs. A collision result means
the insert-only create-user command left the existing account unchanged; the
orchestrator must still discard its entire scratch target.
