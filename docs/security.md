# Threat model: admin login and pending password admission

## Assets and boundaries

The controls covered here protect the availability of the admin login and the
capacity available to clients completing `auth.checkPassword`. The raw admin
token is a secret; the server is configured with its hash, and Postgres stores
the shared admission state. Admin login attempts are keyed from the HTTP
connection's `RemoteAddr`,
normalized to an IPv4 `/32` or IPv6 `/64`. Forwarded headers are not trusted.
Pending-login admission begins when a client reaches
`SESSION_PASSWORD_NEEDED`; each admitted connection occupies one fleet-wide
slot until disconnect or the absolute connection lifetime ends.

## Abuse cases and controls

| Abuse case | Control and remaining risk |
|---|---|
| Guessing the admin token | `POST /admin/login` charges a shared Postgres counter before parsing the body, checking Origin/CSRF, or comparing the token. The first five attempts per network in 30 seconds are immediate. Each later attempt waits two seconds, then still reaches normal validation; this is throttling, not a lockout. Attackers using multiple network buckets get independent budgets. Keep the token high-entropy and restrict admin network access. |
| Draining the admin login budget | Malformed or cross-origin POSTs also consume the source network's budget because admission precedes request validation. Requests from clients behind one reverse proxy share its socket peer address and can delay each other's logins. Restrict proxy access to the admin listener; the handler does not infer client identity from forwarded headers. |
| Holding password-required connections | Postgres leases cap pending password logins at 1024 by default across replicas. A client that reaches `SESSION_PASSWORD_NEEDED` can occupy a slot, so a coordinated attacker may temporarily deny other pending logins, but cannot exceed the cap. Each connection closes at the ten-minute pending-login lifetime; disconnect releases its lease sooner. Setting `TG_MAX_PENDING_LOGIN_CONNS=0` disables this bound. |
| Losing a replica during admission | Lease expiry reclaims abandoned rows after the owner disappears. The lease TTL is one second longer than the ten-minute connection lifetime, so a failed release can temporarily consume a slot but cannot persist indefinitely. |

## Database failures

Postgres is required for both shared controls. An unavailable database or a
missing migration is not treated as an unlimited budget and does not fall back
to per-process state. An admin login rate-limit error is logged and returns a
generic HTTP 500 before the handler reads the submitted credentials. A pending
login lease acquisition error is logged and the connection is closed before it
can continue through the password-required state. These failures reduce
availability while preserving the admission bounds. If a lease release fails,
the row remains until its expiry and may temporarily reduce available capacity.

The admin limiter uses the existing `rate_limits` schema. Pending login
admission requires the `server_limit_leases` migration on the shared database.

## PostgreSQL query diagnostics

The PostgreSQL 18 configuration enables `log_min_duration_statement = 250ms`
and loads `pg_stat_statements`. Treat both surfaces as sensitive operational
data because they can expose SQL text and workload metadata.

For the recurring review queries, cadence, and action thresholds, see the
[PostgreSQL statistics review](postgres-stats-review.md).

### Sensitive data exposure

Slow-query logs include the SQL text of each completed statement that takes at
least 250 ms. The configuration does not set `log_parameter_max_length`; its
PostgreSQL 18 default is `-1`, so bind values can be logged in full for these
statement-duration messages. The default
`log_parameter_max_length_on_error = 0` omits bind values from error messages,
but error logs can still include SQL text. Literal values embedded in SQL are
not redacted. Query text can contain user content, account identifiers, or
other sensitive values, so log entries must not be treated as anonymized.
See [PostgreSQL 18 logging settings](https://www.postgresql.org/docs/18/runtime-config-logging.html).

`pg_stat_statements` retains representative query text, query identifiers,
database and role identifiers, and execution counters. It normally replaces
literal constants with placeholders, but PostgreSQL documents cases where
constants can remain visible, including high entry churn. Statistics are saved
across restarts by default and this repository configures no age-based reset.
Treat the view and its query-text file as sensitive as well. See the
[PostgreSQL 18 `pg_stat_statements` documentation](https://www.postgresql.org/docs/18/pgstatstatements.html).

### Log access and retention

The Compose PostgreSQL service publishes no host port. PostgreSQL writes to
standard error, which Docker captures. The service does not set a logging
driver or rotation options, so the Docker daemon configuration controls where
logs are stored and how long they remain. Docker's default `json-file` driver
does not rotate logs unless rotation is configured. The repository therefore
does not establish a time or size limit for PostgreSQL logs; operators must
configure and verify host-level rotation and any log forwarding before
deployment. See [Docker logging configuration](https://docs.docker.com/engine/logging/configure/)
and the [JSON-file driver options](https://docs.docker.com/engine/logging/drivers/json-file/).

Reading `docker compose logs postgres` or the daemon's log storage requires
Docker or host access. Docker daemon control can also inspect the database
container and its data volume, so grant it only to trusted host operators.
Forwarded log destinations inherit the same sensitive-data classification and
need access and retention controls of their own.

### Statistics-reader boundary

The `pg_stat_statements` migration revokes `SELECT` on
`public.pg_stat_statements` and `public.pg_stat_statements_info`, and `EXECUTE`
on their read functions, from `PUBLIC`. It grants those same privileges to the
predefined `pg_read_all_stats` role. PostgreSQL allows members of that role, as
well as superusers, to see query text and query identifiers for statements
executed by other database roles. Only explicitly trusted database readers
should be made members of `pg_read_all_stats`; it is not a statistics-only view
of redacted queries. The migration does not grant membership to application
or probe roles, and the migration test verifies that `browser_acceptance_probe`
cannot read these objects.

The Compose application DSN uses the `postgres` database role, and this
change does not provision a separate least-privilege runtime role. That
credential is therefore inside the trusted reader boundary: compromise of
the running application credential can expose query statistics and the
database contents it already accesses. The `pg_read_all_stats` grants do not
contain that risk or provide isolation from the application.

# Threat model: profile-photo clear and request replay

## Assets and boundaries

The clear RPC acts on the authenticated account's current profile-photo
selection. It accepts the empty-photo input, not a caller-selected owner or
file ID. The handler passes the authenticated owner to the store, which scopes
the current-state, gallery, upload-receipt, and clear-receipt operations to
that owner. A clear removes the profile/gallery reference and advances the
profile revision; it does not erase file bytes or unrelated message, chat, or
channel references. File reclamation and erasure of those other references are
separate operations.

PostgreSQL is the shared boundary for serialization across server replicas and
for durable retry identity. A clear receipt stores the owner and
`(auth_key_id, session_id, msg_id)` tuple, a random operation key, and optional
historical file and revision IDs. It contains no photo bytes. The transport
tuple stays in the local receipt table; recovery uses the separate opaque
operation key. Receipts older than 90 days no longer match a replay and are
pruned for that owner on a later clear. Treat the database, backups, and query
diagnostics as sensitive because the receipt IDs can be joined to account and
photo metadata.

## Abuse cases and controls

| Abuse case | Control and remaining risk |
|---|---|
| An account tries to clear another account's photo | The RPC derives its owner from the authenticated MTProto request and accepts no target ID. Store queries and the current-photo/gallery relationship are owner-scoped. A stolen session can still clear its own account's current photo. |
| A completed clear is replayed after a new photo is selected | The store checks the durable receipt for the same owner, auth key, session, and message ID before reading the current selection. Clear, receipt, and rate charge commit in one transaction under the per-owner advisory lock. A match does not resolve or delete the newer selection. After 90 days the receipt no longer deduplicates; MTProto's separate message-ID age check remains the wire-level bound for old requests. |
| Concurrent retries repeat the clear or consume extra rate budget | The shared per-owner lock serializes replicas. Only the first request with a new tuple clears and charges `update_profile`; same-tuple retries return through the receipt path without another charge. Each fresh tuple is rate-limited. Same-tuple retries still use database and lock capacity, and lock contention can return `FLOOD_WAIT_1` after the one-second wait. |
| A database error leaves a photo cleared but no receipt, or charges without clearing | The clear, receipt insert, and rate-limit update share one transaction, so a failed transaction rolls back all three. Database unavailability can reduce availability; the handler returns a generic internal error for storage failures. |
| Operators mistake profile clear for complete personal-data erasure | The operation removes the current gallery reference, not the underlying file bytes or references outside the profile gallery. Those have separate retention and erasure controls and must be considered for a full erasure request. |

# Threat model: published input policies and username authentication

## Assets and boundaries

The protected assets are account identity, username ownership, authentication
code state, and the binding between an authenticated key and its account. An
unauthenticated client can request `help.getAppConfig` before sign-in. That
response publishes the accepted username syntax and other client-facing input
bounds. Username values also arrive from untrusted clients through
`auth.sendCode`, `auth.signIn`, and `auth.signUp`; client-side checks do not
establish trust.

## Abuse cases and controls

| Abuse case | Control and remaining risk |
|---|---|
| A client ignores the advertised username rules or sends a crafted username | The server validates usernames in each auth entry point using the same minimum length, maximum length, and pattern constants that `help.getAppConfig` publishes. Auth handlers normalize accepted usernames to lowercase. Reserved-name checks remain server-side. A client can still submit arbitrary requests, but malformed values do not skip server validation. |
| A caller probes usernames or floods code and sign-up requests | `auth.sendCode` and active-mode `auth.signUp` apply per-IP rate limits before identifier-dependent lookup or admission. Registration mode, invite checks, and configured Turnstile verification gate account creation. These controls bound abuse volume; the existing sign-in flow can return different states for existing and new usernames, so it is not an account-privacy boundary. |
| A caller reads app config to obtain authentication data | `help.getAppConfig` is intentionally available before sign-in and exposes policy values and registration mode. It contains no login codes, credentials, or account records. The rules make valid client behavior predictable but do not grant authentication or account creation. |

The published username policy is advisory to clients; the server remains the
enforcement boundary. This change does not alter login-code verification,
password verification, session binding, or account admission semantics.

# Threat model: signup Turnstile verifier

## Scope and trust boundary

This model covers the optional server-side Turnstile check on `auth.signUp`.
The protected assets are account creation under the configured registration
policy, the Cloudflare Siteverify secret, and the demo QA bypass token. Signup
RPCs and their challenge tokens are attacker-controlled; the site key is
public. The server process, its startup configuration and secret mounts, the
deployment operators, and Cloudflare's Siteverify endpoint over HTTPS are
trusted. A successful challenge is an anti-automation signal, not proof of
identity or authorization. Registration mode, invite validation, and signup
rate limits remain independent server-side controls.

## Construction and configuration

`cmd/teagramd` creates the verifier from the `TurnstileConfig` loaded and
validated at startup. The gate is disabled when credentials are unset. When
enabled, configuration requires the public `TG_TURNSTILE_SITE_KEY` and exactly
one Siteverify secret source: `TG_TURNSTILE_SECRET_KEY_FILE` (preferred) or
`TG_TURNSTILE_SECRET_KEY`. The secret stays server-side and remains in process
memory while the verifier is in use. `NewTurnstileVerifierWithConfig` takes
explicit values; the runtime relies on callers supplying validated startup
configuration, since the constructor itself does not enforce the deployment
policy. Any other in-process caller is therefore inside the trusted
configuration boundary.

`TG_TURNSTILE_HOSTNAMES` is a comma-separated list of exact DNS hostnames,
defaulting to `web.teagram.io`. The demo allows `web.teagram.io` and
`demo.teagram.io`. The verifier normalizes case and a trailing dot, then
requires Siteverify success, the expected action, and an exact hostname match.
This check uses Cloudflare's response, not a client-supplied `Host`, `Origin`,
or RPC field; wildcards are not supported. Operators must keep the list limited
to hostnames that should be allowed to submit signup challenges. Local and CI
test mode accepts only Cloudflare's official test key pairs and refuses to
start when a public `*.teagram.io` deployment hostname is advertised.

With proxy-v2 client-address trust, the verifier may send the connection
address reported by the allowlisted proxy to Siteverify. Direct socket peers
and client-provided forwarding headers do not become this value. The verifier
posts to the fixed HTTPS Siteverify endpoint, disables redirects, bounds the
token and response sizes, limits concurrent checks, and fails closed on
provider errors, malformed responses, action mismatches, or host mismatches.
After verification, signup still passes through registration, IP-rate-limit,
session-state, username, and invite checks.

## Demo QA bypass

`TG_TURNSTILE_QA_BYPASS_TOKEN_FILE` is an operator-provisioned static token
loaded at startup. Configuration permits it only when the advertised stack
includes `demo.teagram.io` and no production Teagram hostname; it must contain
32 to 256 allowed characters and must differ from the Siteverify secret. The
demo deploy mounts this separate file only into `teagramd`. A constant-time
exact match bypasses Siteverify only for the `signup` action. It is not
one-use: anyone who obtains it can reuse it while it remains configured. The
token is not logged, and use of it still requires the normal registration
policy, signup IP limit, and account-admission checks. It is not a production
credential or a recovery path.

## Abuse cases and residual risk

| Abuse case | Control and remaining risk |
|---|---|
| A caller removes or forges the challenge UI or submits a fabricated token | The server verifies the token with Siteverify and checks its action and exact response hostname. This does not establish the caller's identity; distributed callers can still obtain valid challenges. |
| An operator uses test credentials on a public stack | Test mode accepts only Cloudflare's official test key pairs and rejects advertised public Teagram deployment hostnames. Test mode does not open registration or disable signup limits. |
| The hostname allowlist includes an unintended site | Only exact configured hostnames match, but every configured hostname broadens the accepted Siteverify response set. DNS syntax validation cannot decide which domains the operator intended. |
| The demo QA token leaks | A holder can skip the challenge repeatedly on the demo stack until the token is rotated and the server restarted. Registration rules and the signup IP limit still apply; distributed sources can multiply per-IP budgets. |
| Cloudflare Siteverify is unavailable or the verifier is saturated | Verification fails closed; callers cannot complete signup until the provider or local capacity recovers. This preserves the gate at an availability cost. |
| Turnstile is left unconfigured | Signup has no Turnstile check by design. The configured registration policy, invite checks, and rate limits remain the admission controls. |

The host, deployment configuration, and secret-file access are part of the
trusted boundary. Compromise of them can expose the Siteverify credential or
demo bypass token; Turnstile does not protect against a compromised server or
operator.
