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
