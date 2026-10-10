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
