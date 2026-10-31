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

# Threat model: private message-search postings

## Assets and boundaries

`message_search_postings.lexeme` stores normalized plaintext terms derived from
private message bodies, alongside owner, dialog, and message identifiers. It is
not a hash or an anonymized representation: common and distinctive terms can
reveal message content. Treat the postings table, its index, database backups,
and database access as sensitive at least as the source `messages` rows. The
`message_search_unready_dialogs` table contains owner and dialog identifiers,
but no message text or lexemes. The search query itself is sensitive too:
slow-statement diagnostics can retain bound search text under the existing
[PostgreSQL diagnostic policy](#postgresql-query-diagnostics).

`messages.search` passes the authenticated MTProto owner ID to the store. Both
search paths constrain the owner and dialog; the postings path joins back to
the same owner-local message row and rechecks the complete query, deletion
state, and expiry before returning source message rows. No RPC returns posting
lexemes. The migration adds no row-level security or per-owner database grants;
tenant isolation is enforced in application SQL. The current Compose runtime
uses the shared `postgres` role, so a database operator or compromised runtime
credential can read lexemes across owners, as it can read source messages.

## Abuse cases and controls

| Abuse case | Control and remaining risk |
|---|---|
| A caller selects another owner's or another dialog's terms | The RPC derives the owner from the authenticated request; the store query scopes by owner and dialog, joins on the same owner, and rechecks the source message's full text vector and visibility. This is application-layer isolation, so a query or store authorization defect could cross that boundary. |
| A database reader infers private text from the postings | Lexemes are stored in plaintext form and can be dictionary-matched. This migration adds no separate encryption or reader role. Restrict database and backup access under the same controls as message content; do not treat postings as de-identified data. |
| An edit, delete, or expiry leaves searchable terms behind | The database trigger replaces postings in the same transaction as a text or dialog change, and removes them on soft delete. Hard deletion of a source message cascades to its postings. Search excludes expired messages immediately; the existing five-minute bounded history sweep marks expired copies deleted, which removes their postings transactionally. If that worker is delayed or failing, physical removal can lag while the search filter still hides the message. |

## Retention, erasure, and rollback

There is no separate posting lifetime. Live postings follow the corresponding
message copy: edits replace them, soft deletion removes them, and hard deletion
cascades to them. A soft-deleted message body remains in its tombstone row in
`messages`; removing its postings is not full erasure of that source body.
Expired copies are also soft-deleted by the periodic history sweep. The server
does not set a general expiry for message history, so messages without a TTL
remain until the existing message-delete path changes them. User deletions do
not scrub already-created database backups; backup retention remains an
operator control, as described in
[self-hosting](self-hosting.md#back-up-and-restore).

An application-only rollback leaves the additive trigger active. Older code
ignores the postings, but new message writes continue populating them and the
rollback does not erase existing lexemes. Stopping that collection or erasing
the live derived rows requires a separately planned schema operation; dropping
the postings table does not erase source messages or backup copies. See the
[migration behavior](migrations.md#ordered-message-search-postings) for the
additive schema details.
