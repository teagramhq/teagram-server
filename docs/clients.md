# Connecting a client to teagramd

`teagramd` speaks MTProto and stores accounts, messages, and media metadata in
Postgres. Media bodies use local storage by default. Teagram clients sign in
with a username and password; they do not use phone codes or QR scans.

Teagram Desktop is the supported client. Stock Telegram clients use Telegram's
production data centers and RSA keys, so they cannot connect to this server.

## 1. Build and run the server

```bash
go build -o teagramd ./cmd/teagramd
TG_POSTGRES_DSN="postgres://user:pass@localhost:5432/teagram?sslmode=disable" \
  TG_AUTHKEY_ENC_KEY="$(openssl rand -hex 32)" \
  TG_PUBLIC_LINK_PREFIX="https://links.example.test/" \
  ./teagramd
```

Configuration is read from environment variables in `internal/config/config.go`:

| Variable            | Default          | Notes                                      |
|---------------------|------------------|---------------------------------------------|
| `TG_LISTEN_ADDR`    | `:2443`          | `host:port` (or `:port`) the server binds   |
| `TG_WEBSOCKET_LISTEN_ADDR` | *(unset)* | Enables the WebSocket MTProto listener on this address; browser clients connect to `/apiws`, and the listener serves `/.well-known/teagramd/client` |
| `TG_PUBLIC_WEBSOCKET_LISTEN_ADDR` | *(unset)* | Enables the separate WebSocket MTProto listener for public traffic; use an address such as `:2446` and expose it only through the intended public proxy |
| `TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR` | *(unset)* | Optional `host:port` used only for the public listener's discovery `websocket.endpoint`; its host becomes `wss://host/apiws`, while MTProto and `client-config` keep `TG_ADVERTISE_ADDR`. Requires `TG_PUBLIC_WEBSOCKET_LISTEN_ADDR`; malformed values fail server startup |
| `TG_RPC_DEADLINE` | `23s` | Per-request timeout; overrides must be positive and no greater than `45s`. `0s` and values above `45s`, including the former `90s` setting, fail startup so admitted requests fit the fixed shutdown drain |
| `TG_WEBSOCKET_ALLOWED_ORIGINS` | *(unset)* | Comma-separated browser origins allowed to connect to `/apiws`; unset rejects every request carrying an `Origin` header |
| `TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS` | *(unset)* | Comma-separated origins allowed on the public WebSocket listener; set `https://web.teagram.io` for the public web client, and add the demo origin only when it is deployed |
| `TG_ADVERTISE_ADDR` | *(derived from `TG_LISTEN_ADDR`)* | `host:port` clients are told to dial, used verbatim. Its host also forms the discovery `websocket.endpoint` as `wss://host/apiws`. Derived when unset: the listen address with an empty or wildcard host (`:2443`, `0.0.0.0`, `::`) replaced by `127.0.0.1`. A value that is not `host:port`, has no host, or has a port that is not an integer in 1–65535 fails startup |
| `TG_ADVERTISE_DEMO_GUEST_SIGN_UP` | `false` | Set to `true` to advertise demo guest sign-up and the stored wipe generation in discovery and `client-config`; requires the migrated Postgres row, and a non-boolean value fails configuration loading |
| `TG_PUBLIC_LINK_PREFIX` | *(required)* | Lowercase HTTPS origin for client and invite links, with a root path and no port (e.g. `https://links.example.test/`) |
| `TG_POSTGRES_DSN`   | *(required)*     | Postgres connection string; no default, server fails to start without it |
| `TG_PG_MAX_CONNS` | `8` | Maximum pooled Postgres sessions per process, at least 2 |
| `TG_PG_MIN_CONNS` | `2` | Minimum pooled sessions, from 0 through `TG_PG_MAX_CONNS` |
| `TG_PG_MAX_CONN_IDLE_TIME` | `5m` | Positive duration after which idle pooled sessions above the minimum may close |
| `TG_STATEMENT_TIMEOUT` | `17s` | Postgres timeout for each statement on every pooled session; `0s` disables it |
| `TG_AUTHKEY_ENC_KEY`| *(required)*     | 64 hex chars (32 bytes) — master key that encrypts auth keys at rest; must stay stable and be the same on every replica, or persisted sessions can no longer be decrypted |
| `TG_AUTHKEY_ENC_KEY_FILE`| *(unset)*   | Path the master key is read from when `TG_AUTHKEY_ENC_KEY` is empty. A missing file is generated into (0600) on first start as a dev key only when both `TG_REPLICA_ID` and `TG_RSA_KEY_FINGERPRINT` are unset. With either identity pin, the file must already exist; startup never generates a pinned key |
| `TG_RSA_KEY_PATH`   | `server_key.pem` | Path to the server's RSA private key        |
| `TG_RSA_KEY_FINGERPRINT` | *(unset)* | Signed decimal Telegram fingerprint expected from the loaded RSA key. Set it on every replica to pin the deployment identity; required when `TG_REPLICA_ID` is set. Existing single-replica deployments may leave it unset until the proxy rollout |
| `TG_BLOB_DIR`       | `blobs`           | Local filesystem blob root; used when all `TG_BLOB_S3_*` variables are unset or empty |
| `TG_BLOB_S3_ENDPOINT` | *(unset)*       | Enables the S3-compatible blob backend; setting any non-empty `TG_BLOB_S3_*` variable selects it and requires the complete configuration |
| `TG_BLOB_S3_BUCKET` | *(unset)*         | Private bucket containing blobs |
| `TG_BLOB_S3_PREFIX` | *(unset)*         | Required non-root prefix assigned to this server |
| `TG_BLOB_S3_REGION` | `us-east-1`       | SigV4 signing region |
| `TG_BLOB_S3_ACCESS_KEY_ID` | *(unset)* | Operations-only key scoped to this bucket and prefix |
| `TG_BLOB_S3_SECRET_ACCESS_KEY` | *(unset)* | Raw secret accepted only for compose or CI secret injection; prefer the file form |
| `TG_BLOB_S3_SECRET_ACCESS_KEY_FILE` | *(unset)* | Preferred file containing the object-store secret; read at startup |
| `TG_BLOB_S3_CA_PATH` | *(unset)*        | PEM bundle for a private endpoint CA; TLS verification remains enabled |
| `TG_BLOB_S3_ALLOW_INSECURE_HTTP` | `false` | Explicit loopback/compose-only plaintext opt-in; startup warns when enabled |
| `TG_MEDIA_KEY_DIR` | *(unset)* | Directory of versioned media keys (`<version>.key`); both media key settings must be set together |
| `TG_MEDIA_KEY_CURRENT` | *(unset)* | Version used for newly written encrypted S3 objects; old versions remain readable while their key files stay available |
| `TG_UPLOADS_DISABLED` | `false` | Set to `true` to reject client RPCs that upload file bytes with the stable `UPLOADS_DISABLED` error; existing media downloads continue to work, and the demo's private seed overlay enables its API only while public ingress is stopped |
| `TEAGRAM_MEDIA_KEY_SOURCE_DIR` | `/etc/teagramd/media-keys` | Host directory bind-mounted read-only into the default Compose server; provision outside the repository |
| `TG_DC_ID`          | `2`              | DC id this server advertises as `ThisDC`    |
| `TG_RATE_LIMIT_DISCOVERY` | `60` | Cluster-wide valid local-direct preflight response attempts per fixed window, counted in Postgres; `0` disables this bound, and a negative or non-integer value fails startup |
| `TG_RATE_LIMIT_DISCOVERY_WINDOW` | `1m` | Fixed window for the cluster-wide discovery bound; it must be positive while that bound is enabled |
| `TG_RATE_LIMIT_DISCOVERY_IP` | `10` | Valid local-direct preflight response attempts per client network (`/32` for IPv4 or `/64` for IPv6) per fixed window, shared through Postgres; `0` disables this bound |
| `TG_RATE_LIMIT_DISCOVERY_IP_WINDOW` | `1m` | Fixed window for the per-network discovery bound; it must be positive while that bound is enabled |
| `TG_LOG_LOGIN_CODES`| `false`          | Legacy login-code diagnostic. It is not used by Teagram username/password sign-in; leave it disabled. A non-boolean value fails startup |
| `TG_LOG_AUTH_KEY_UNBINDS` | `false` | Structured unbind diagnostics with operation, reason, outcome, auth-key ID, and user ID. Demo enables it for troubleshooting; no key bytes, tokens, or credentials are logged. See the [demo log access and retention model](../deploy/teagram-demo/README.md#auth-key-unbind-diagnostic-logs). A non-boolean value fails startup |
| `TG_SESSION_IP` | `false` | Store session IPs and return them in `account.getAuthorizations`. Unset means no session IP stored by default; a non-boolean value fails startup |
| `TG_SESSION_DEVICE_LABELS` | `true` | Store and return device model, platform, OS version, API ID, and app version for sessions. A non-boolean value fails startup |
| `TG_RATE_LIMIT_SEND_CODE_IP` | `10` | `auth.sendCode` calls per client network per window; `0` disables this counter |
| `TG_RATE_LIMIT_SEND_CODE_IP_WINDOW` | `1h` | Window for the per-network `auth.sendCode` call counter; `0` or a negative window also disables this counter |
| `TG_RATE_LIMIT_SEND_CODE_IP_PHONES` | `20` | Distinct `auth.sendCode` identifiers per client network per window; `0` disables this counter |
| `TG_RATE_LIMIT_SEND_CODE_IP_PHONES_WINDOW` | `24h` | Window for distinct `auth.sendCode` identifiers; `0` or a negative window also disables this counter |
| `TG_RATE_LIMIT_CHECK_PASSWORD_IP` | `10` | Failed `auth.checkPassword` attempts per client network per window; `0` disables this counter |
| `TG_RATE_LIMIT_CHECK_PASSWORD_IP_WINDOW` | `1h` | Window for the per-network `auth.checkPassword` counter; `0` or a negative window also disables this counter |
| `TG_RATE_LIMIT_GET_PASSWORD_IP` | `20` | Unauthenticated `account.getPassword` calls per client network per window; `0` disables this counter |
| `TG_RATE_LIMIT_GET_PASSWORD_IP_WINDOW` | `1h` | Window for the per-network `account.getPassword` counter; `0` or a negative window also disables this counter |
| `TG_RATE_LIMIT_SIGN_UP_IP` | `5` | `auth.signUp` calls per client network per window; `0` disables this counter |
| `TG_RATE_LIMIT_SIGN_UP_IP_WINDOW` | `1h` | Window for the per-network `auth.signUp` counter; `0` or a negative window also disables this counter |
| `TG_RATE_LIMIT_CHECK_USERNAME_IP` | `20` | Unauthenticated `account.checkUsername` calls per client network per window; `0` disables this counter |
| `TG_RATE_LIMIT_CHECK_USERNAME_IP_WINDOW` | `1h` | Window for the per-network unauthenticated `account.checkUsername` counter; `0` or a negative window also disables this counter |
| `TG_REGISTRATION`   | `closed`         | Accepted values are `closed`, `invite`, and `open`. `closed` rejects `auth.signUp`, `invite` requires an operator-issued invite, and `open` admits usernames without one. An unrecognized value fails startup. Sign-in for accounts that already exist is unaffected by this setting |
| `TG_USER_INVITE_QUOTA` | `0` | Non-negative integer; a positive value enables user-issued invite creation and redemption on listeners in `invite` mode. Every invite created in the trailing seven days counts toward the user's quota, whatever its state. Unset or zero disables it |
| `TG_USER_INVITE_MIN_ACCOUNT_AGE` | `168h` | Minimum age of an account allowed to create user invites, measured from `users.created_at`; accepts Go duration syntax and must be non-negative |
| `TG_TERMS_OF_SERVICE` | built-in statement | Terms shown to new accounts in `auth.authorizationSignUpRequired`; set it to replace the default. See the [self-hosting guide](self-hosting.md#sign-up-terms) for the default text |
| `TG_PUBLIC_REGISTRATION` | `closed` | Registration mode for the public WebSocket listener only. Defaults to `closed` even when `TG_REGISTRATION=open`; `invite` requires an operator-issued invite and permits user-issued invites when the quota is positive |
| `TG_WELCOME_CHANNEL` | *(unset)* | Optional handle of a public broadcast channel joined by new accounts after `auth.signUp`; invalid targets are skipped with a warning |
| `TG_RECOVERY_EMAIL` | `off` | Exact lowercase `off` or `contact`; any other set value, including empty or whitespace, fails startup. `off` discards emails sent with a password set/change. `contact` retains them for operator-assisted reset; no mail transport or automated recovery RPC is provided |
| `TG_TURNSTILE_SITE_KEY` | *(unset)* | Public Cloudflare Turnstile widget key. Required together with one secret source to enable the `auth.signUp` challenge |
| `TG_TURNSTILE_SECRET_KEY` | *(unset)* | Private Siteverify key supplied directly through the environment; prefer the file form |
| `TG_TURNSTILE_SECRET_KEY_FILE` | *(unset)* | Preferred file containing the Siteverify key. The gate stays disabled when both secret sources and the site key are unset |
| `TG_TURNSTILE_HOSTNAMES` | `web.teagram.io` | Comma-separated DNS hostnames accepted in successful Siteverify responses; the demo sets `web.teagram.io,demo.teagram.io` |
| `TG_TURNSTILE_TEST_MODE` | `false` | Local/CI-only mode. Allows the gate to stay off or accepts Cloudflare's official test key pairs; rejects real secrets and advertised `*.teagram.io` hosts |
| `TG_TURNSTILE_QA_BYPASS_TOKEN_FILE` | *(unset)* | Optional demo-only signup bypass token file. Allowed only when the configured deployment hostname is `demo.teagram.io`; never set it on production |
| `TG_TURNSTILE_SIGNIN_ENABLED` | `false` | Also challenge username sign-in at `auth.sendCode`; requires a valid site key and Siteverify secret |
| `TG_ADMIN_LISTEN_ADDR` | *(unset)* | Enables the separate authenticated admin HTTP listener; must be set with `TG_ADMIN_TOKEN_HASH` and should remain on an operator-only network |
| `TG_ADMIN_TOKEN_HASH` | *(unset)* | Lowercase SHA-256 hex digest of the raw admin token; never put the raw token in configuration or a URL. See `docs/observability.md` |
| `TG_ADMIN_ORIGIN` | *(unset)* | Fixed origin for admin login/logout behind a proxy; canonical lowercase ASCII HTTPS origin, or HTTP only for localhost/loopback. Unset, empty, or whitespace-only derives it from the listener. See `docs/observability.md` |
| `TG_REPLICA_ID`     | *(unset)*        | Optional stable operator-supplied identity shown on authenticated admin metrics; when set, `TG_RSA_KEY_FINGERPRINT` is required. The value must be 1–64 characters from `A-Z`, `a-z`, `0-9`, `.`, `_`, and `-` |
| `TG_REPLICA_COUNT` | `1` | Maximum number of `teagramd` replicas running at once. Set the same value on every replica; it divides local connection budgets, including the default per-user cap of 20, so the combined cap stays at or below the configured total. A non-zero local budget smaller than this count fails startup. Do not mix replica counts during a rollout |
| `TG_RATE_LIMIT_GET_FILE` | `50` | Per-account `upload.getFile` calls in one fixed window. `0` disables this bound; a negative or non-integer value fails startup |
| `TG_RATE_LIMIT_GET_FILE_WINDOW` | `1s` | Window for the per-account `upload.getFile` bound. It must be positive while that bound is enabled; an invalid or negative duration fails startup |
| `TG_RATE_LIMIT_GET_FILE_REPLICA` | `400` | Cluster-wide aggregate `upload.getFile` calls across all accounts in one fixed window, counted in Postgres. `0` disables this bound; a negative or non-integer value fails startup |
| `TG_RATE_LIMIT_GET_FILE_REPLICA_WINDOW` | `1s` | Window for the cluster-wide aggregate `upload.getFile` bound. It must be positive while that bound is enabled; an invalid or negative duration fails startup |
| `TG_CLIENT_ADDR_TRUST`| `socket`       | Where the address a per-IP limit is keyed on comes from: `socket` or `proxy-v2`. Any other value fails startup by name. `socket` is the connection's own peer address and assumes one peer address is one client, which fails from either end: behind a proxy or an L4 load balancer every peer address is the balancer's, so one bucket holds every client and the per-IP cap becomes a global one; behind a carrier NAT one address covers thousands of mobile subscribers, who then spend each other's budget. The server warns about both once at startup while any per-IP limit is on. `proxy-v2` takes the address from a PROXY protocol v2 header and is what to run behind an L4 load balancer; it needs `TG_CLIENT_ADDR_PROXY_CIDRS` and emits no such warning, because the misconfiguration it warns about fails the start instead |
| `TG_MAX_PREAUTH_CONNS`| `1024`       | Cluster-wide concurrent connections that have not authenticated yet. `TG_REPLICA_COUNT` divides this into per-replica accept-loop caps, so sockets past a share are closed before they cost a goroutine, a deadline or a read. `0` disables it; a negative or non-integer value fails startup |
| `TG_MAX_PREAUTH_CONNS_PER_IP`| `64`    | The same, per client network, divided by `TG_REPLICA_COUNT` and enforced in each replica's memory. Keyed on the network the per-IP rate limits already use — an address for IPv4, a **/64** for IPv6, since a host on a routed v6 allocation mints addresses inside its own /64 for free — and on the address `TG_CLIENT_ADDR_TRUST` names, which in `proxy-v2` mode is the one the balancer reports and never the socket peer. A connection carrying no address at all (a `LOCAL` health check, or a socket peer the transport could not report) is charged to nothing and stays bounded by the other two. It is a concurrency cap and not a rate. `0` disables it; a negative or non-integer value fails startup |
| `TG_KEY_EXCHANGE_PER_CONNECTION` | `2` | Maximum anonymous key exchanges on one connection. The count survives the pre-auth slot clearing after the first MAC-valid frame. `0` disables it; a negative or non-integer value fails startup |
| `TG_RATE_LIMIT_KEY_EXCHANGE_IP` | `20` | Fixed-window anonymous key exchanges per client network, enforced in Postgres and shared by replicas. Uses the same IPv4-address / IPv6-**/64** key and trusted client address as the other per-IP limits; public WebSocket and TCP/tailnet listeners keep separate counters. If explicitly set, this value applies to both listener families unless the tailnet setting below overrides it. A connection with no keyable address skips only this layer. Store errors refuse the exchange. `0` disables it; a negative or non-integer value fails startup |
| `TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET` | `20` | Tailnet-only per-network key-exchange limit for TCP and tailnet WebSocket listeners. An explicit value overrides `TG_RATE_LIMIT_KEY_EXCHANGE_IP`; `0` disables it. A negative or non-integer value fails startup |
| `TG_RATE_LIMIT_KEY_EXCHANGE_IP_WINDOW` | `1h` | Fixed window for the public and tailnet per-network key-exchange budgets. `0` disables those layers; a negative or invalid duration fails startup |
| `TG_KEY_EXCHANGE_GLOBAL_RATE` | `5` | Deployment-wide in-memory key-exchange ceiling in attempts per second. `TG_REPLICA_COUNT` divides it across replicas. `0` disables the ceiling; when enabled, a value smaller than the replica count fails startup. A negative or non-integer value also fails startup |
| `TG_KEY_EXCHANGE_GLOBAL_BURST` | `30` | Burst capacity of the deployment-wide in-memory key-exchange ceiling, divided across replicas. `0` disables the ceiling; when enabled, a value smaller than the replica count fails startup. A negative or non-integer value also fails startup |
| `TG_PREAUTH_LIFETIME`| `2m`          | How long a connection may stay unauthenticated, measured from accept. Past it the socket is closed whatever it is sending, which is the only bound that reaches a peer that stays inside every deadline by dripping one small frame per read timeout. It ends at the first frame that decrypts under a key the server issued, so a client completing its password challenge is not cut off. Do not set it below a minute: gotd applies a 60s timeout per read inside key exchange, a shorter ceiling starts cutting handshakes that are merely slow, and the server warns at startup if you do. `0` disables it; a negative or unparseable duration fails startup |
| `TG_MAX_CONNS_PER_UNBOUND_KEY`| `8`  | Concurrent connections one auth key with nobody signed in on it may hold, divided by `TG_REPLICA_COUNT` and enforced in each replica's memory. It covers the population between pre-auth connections and signed-in sessions. A connection is charged only once a frame has decrypted under the key. Past the cap the frame in hand is answered and the socket is then closed. `0` disables it; a negative or non-integer value fails startup |
| `TG_MAX_PENDING_LOGIN_CONNS`| `1024` | Cluster-wide concurrent connections waiting for `auth.checkPassword` after `SESSION_PASSWORD_NEEDED`, held in Postgres leases. A pending connection remains counted in the unbound-key hold and this cap, and receives a single ten-minute absolute read lease (`2 × srp.DefaultTTL`); activity never refreshes it. The SRP challenge still expires after five minutes. Past the cap the connection is closed immediately. `0` disables it; a negative or non-integer value fails startup |
| `TG_CLIENT_ADDR_PROXY_CIDRS`| *(unset)* | Comma-separated addresses or CIDRs (`10.0.0.0/8, 192.0.2.7`) of the balancers a PROXY protocol v2 header is accepted from. An IPv4-mapped entry takes its IPv4 meaning (`::ffff:192.0.2.0/120` is `192.0.2.0/24`), since peer addresses are matched unmapped; one too short to name an IPv4 network fails startup rather than starting and matching nothing. Required by, and only read in, `TG_CLIENT_ADDR_TRUST=proxy-v2`: an empty list there fails startup, and a list set in `socket` mode does too, since it means the balancer is in place but every client is being keyed on its address. Both directions then fail closed — a connection from a listed balancer without a valid v2 header is dropped rather than served on the balancer's address, and a header from anywhere else is dropped rather than believed. Only v2: the v1 text form is refused. An address is read only from `PROXY` over `AF_INET`/`AF_INET6` with the `STREAM` transport; the two headers that name no client — the `LOCAL` command a health check sends, and `AF_UNSPEC` — connect but carry no address and so cannot call `auth.sendCode`; every other family or transport is refused. Keep this list to the balancer addresses, not a VPC or subnet range: a connection whose header names no client is charged to no bucket, so anything inside an allowlisted CIDR can send a `LOCAL`/`AF_UNSPEC` header and sit outside `TG_MAX_PREAUTH_CONNS_PER_IP` entirely — with `10.0.0.0/8` that is every workload in the network, with the balancer's own addresses it is the balancer |

### Session data and request processing

The session metadata settings are deployment-wide and must have the same values
on every replica. By default, no session IP, recovery email, or phone number is
kept as account or session data: `TG_SESSION_IP=false`,
`TG_RECOVERY_EMAIL=off`, and Teagram sign-in uses a username rather than a
phone number. Device and app labels are stored by default with
`TG_SESSION_DEVICE_LABELS=true`, and the server operator can read them.

The session IP setting controls session metadata only. Every connection's IP is
still processed in memory for pre-auth connection caps, PROXY-v2 parsing, and
per-network abuse limits. Counters are deployment-wide within each listener
scope: the default TCP and tailnet WebSocket listeners share their existing
budgets, while the public WebSocket listener has independent per-client
budgets. Defaults are 10
`auth.sendCode` calls per network per hour, 20 distinct `auth.sendCode`
identifiers per network per 24 hours, 10 failed `auth.signIn` attempts per
network per hour, 10 failed `auth.checkPassword` attempts per network per hour,
20 unauthenticated `account.getPassword` calls per network per hour, and 5
`auth.signUp` calls and 20 pre-auth `account.checkUsername` calls per network
per hour. The `TG_RATE_LIMIT_*` settings above can change their limits and
windows; setting a configurable limit to `0` disables only that counter. The
failed `auth.signIn` counter has no environment override. Teagram puts a
username in the protocol's `phone_number` field and rejects phone-shaped
identifiers. Its `auth.sendCode` distinct-identifier counter may retain those
usernames in Postgres for the configured window (24h by default), plus up to
five minutes before the sweep removes them.

Per-network counters use an IPv4 `/32` or IPv6 `/64` bucket based on
`TG_CLIENT_ADDR_TRUST`. Listener scope comes from the accepted socket, not
`Host`, `Origin`, or forwarded HTTP headers. The `auth.sendCode` tables store
that network prefix; other IP rate-limit rows store a deterministic FNV-derived
key, which is enumerable for IPv4 and pseudonymous rather than anonymous.
Counter rows expire with their configured windows and are pruned by the limiter
or its sweeper.
Slow-query logs that include bind values (default threshold 250 ms) and
pre-deploy database dumps can hold older values beyond those windows.

An operator with access to the database can read its contents, messages
included. Messages are not end-to-end encrypted. These settings do not erase
database dumps or slow-query logs, and turning a setting off does not bulk-erase
historical rows.

Disabling session IP collection hides stored IPs from `account.getAuthorizations`
immediately and clears them on the next activity touch, logout, revocation, or
inactivity expiry. An idle session that is never touched again can retain its
historical IP at rest until its normal lifecycle clears it. The switch does not
bulk erase or backfill rows.

Device and app labels, including the API ID, are stored and visible to the
operator while `TG_SESSION_DEVICE_LABELS=true`. Turning it off hides those
fields in `account.getAuthorizations` and stops future writes; historical
labels remain until overwritten or the session is logged out or revoked. It
does not bulk erase them. Turning either session setting back on resumes
collection for new writes without backfilling older rows.

When `TG_WEBSOCKET_LISTEN_ADDR` is set, configure
`TG_WEBSOCKET_ALLOWED_ORIGINS` with the browser origins that may connect to
`/apiws`. For teagram-web, add the exact origin `https://web.teagram.io`; do
not use `*` or a wildcard pattern. Requests without an `Origin` header remain
valid for native clients; requests carrying one are rejected unless it matches
the configured list. `GET` and `OPTIONS` on exactly
`/.well-known/teagramd/client` return `Access-Control-Allow-Origin: *` without
credentials; other routes have no CORS headers. This does not relax the
WebSocket origin check. Keep this listener within the intended network
boundary.

The public WebSocket listener has its own address, registration mode, and
origin allowlist. Its registration mode is bound to the listener in the server;
client headers and RPC fields cannot select the tailnet policy. `help.getAppConfig`
reports the mode of the listener that accepted the connection. Existing
accounts can sign in through either listener.

Turnstile is disabled by default. Set a site key and a Siteverify secret to
challenge each `auth.signUp`; users complete a Cloudflare challenge and Cloudflare
processes its token. The server requires the `signup` action and a hostname from
`TG_TURNSTILE_HOSTNAMES` in a successful Siteverify response before charging the
sign-up IP limit. The hostname defaults to `web.teagram.io`; the demo deployment
allows both `web.teagram.io` and `demo.teagram.io` so either supported client
origin can complete verification.

`TG_TURNSTILE_TEST_MODE=true` is for local and CI stacks. With credentials
unset, Turnstile stays disabled and registration policy is unchanged. With
credentials set, startup accepts only Cloudflare's official test sitekey and
secret pairs and rejects any advertised `*.teagram.io` hostname. Cloudflare's
[always-pass and always-block test keys](https://developers.cloudflare.com/turnstile/troubleshooting/testing/)
exercise successful and rejected challenge paths without live credentials.
The base `docker-compose.yml` keeps test mode off and defaults the hostname
allowlist to `web.teagram.io` for public deployments. The root `.env.example`
opts local Compose into test mode explicitly and overrides the allowlist to
`localhost,127.0.0.1`.

The demo can set `TG_TURNSTILE_QA_BYPASS_TOKEN_FILE` to a root-provisioned
token file. A matching token bypasses Siteverify only for `auth.signUp`, is
compared in constant time, and is never logged. The signup IP limit and all
other registration checks still apply. Configuration rejects the token on any
stack other than `demo.teagram.io`. Set `TG_TURNSTILE_SIGNIN_ENABLED=true` to
also challenge username sign-in at `auth.sendCode`; this switch defaults to
`false`, and other RPCs remain ungated. Behind `TG_CLIENT_ADDR_TRUST=proxy-v2`,
the server also sends the client IP to Cloudflare for verification.

Before asking for registration details, an unauthenticated client may call
`help.getAppConfig`. The `help.appConfig` response keeps its standard TL shape;
its JSON object contains a `registration_mode` string with one of `closed`,
`invite`, or `open`, plus `user_invites_enabled` and `user_invite_quota`. These
values describe the listener that accepted the connection. User invites are
enabled only when that listener is in `invite` mode and
`TG_USER_INVITE_QUOTA` is positive. The fields are extensions to the JSON
object, so clients that do not read them can continue to ignore them. This call
is also available to provisional sessions.

### Public enrollment discovery and local preflight

The server-side contract in this section is provided by teagram-server
revision `b4b18c12` (`MAIN-736`). Deploy that server revision, or a later
revision that retains the contract, as the document consumer. With demo
advertisement disabled, the command renders the public identity without
opening Postgres or loading the auth-key master secret:

```bash
teagramd bootstrap-identity
teagramd client-config > client.json
```

`bootstrap-identity` writes a new 2048-bit PKCS#1 PEM key to `TG_RSA_KEY_PATH`
with mode `0600`, publishes it atomically, and refuses to overwrite an existing
destination. Run it once for a fresh install. Normal serving and
`client-config` only load an existing key and fail closed if it is missing or
invalid. For a replica deployment, copy the printed fingerprint into
`TG_RSA_KEY_FINGERPRINT` on every replica and keep the same RSA key and
`TG_AUTHKEY_ENC_KEY` available to each one. Startup refuses a mismatched RSA
fingerprint or an auth-key encryption key that cannot decrypt a stored auth
key. An empty auth-key table is accepted as the explicit first-bootstrap case.

`client-config` loads the same persistent RSA key that `teagramd serve` uses.
`TG_ADVERTISE_ADDR` and `TG_DC_ID` therefore need
to resolve to the same values for both commands; both have defaults, and an
unset advertise address is derived from `TG_LISTEN_ADDR`. The output is one
deterministic UTF-8 JSON
object with no private-key material and standard padded base64 of DER
SubjectPublicKeyInfo. Its `websocket.endpoint` uses the configured advertised
host and the `/apiws` path:

```json
{"version":1,"mtproto":{"endpoint":"mtproto.example.com:443","dc_id":2,"rsa_spki":"<standard-padded-base64-DER-SPKI>"},"websocket":{"endpoint":"wss://mtproto.example.com/apiws"}}
```

`TG_ADVERTISE_DEMO_GUEST_SIGN_UP` defaults to `false`. When unset or false,
the document omits `demo`, performs no generation lookup, and keeps the existing
v1 wire bytes. When true, both `teagramd serve` and `client-config` read the
database's one stored wipe generation and fail closed if its row is missing or
the value is not 64 lowercase hexadecimal characters. The document then adds
`"demo":{"guest_sign_up":true,"wipe_generation":"<64 lowercase hex characters>"}`
alongside the existing identity and transport fields. A migration seeds the
value from Postgres's cryptographic random UUID generator; a normal restart or
deploy keeps it stable, while a fresh database created by the weekly wipe gets
a new value. The value is public and clients compare it for equality only. It
does not open registration, change password or Turnstile requirements, or grant
authentication privileges. The server's registration policy and abuse controls
remain authoritative.

When Turnstile is configured, discovery also includes
`"turnstile":{"site_key":"..."}`. The public `site_key` must contain 1–256
ASCII letters, digits, underscores, or hyphens; the Siteverify secret is never
included. If `demo` is present, `guest_sign_up` must be the boolean `true`.

`teagramd serve` serves a configured document at
`/.well-known/teagramd/client` on each WebSocket listener. Route that path and
`/apiws` from each HTTPS origin to its intended listener. `client-config` still
does not publish or serve its output; it can be used when a deployment serves
the document as a static file instead. HTTPS certificate serving, web-root
publication, and TLS termination are outside teagramd.

When `TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR` is set, the public WebSocket listener
uses that address's host for its discovery `websocket.endpoint`. The listener's
document keeps the canonical `mtproto.endpoint` and public-only capabilities;
the default WebSocket listener and `client-config` continue to use
`TG_ADVERTISE_ADDR`.

The optional same-endpoint local-direct preflight is a separate TCP
discriminator. It is checked only on the normal TCP listener, after a trusted
PROXY-v2 header has been consumed when `TG_CLIENT_ADDR_TRUST=proxy-v2`, and
before MTProto framing detection. WebSocket does not expose it. Its exact
wire format and delimiter are:

```text
request  = 16 ASCII bytes "telegramd-key-v1" || 32 fresh opaque nonce bytes
response = 16 ASCII bytes "telegramd-key-r1" || 32 echoed nonce bytes
           || uint32 body_length (big-endian)
           || int32 dc_id (big-endian, positive)
           || uint16 spki_length (big-endian)
           || DER SubjectPublicKeyInfo
```

The request is exactly 48 bytes followed by the client's TCP write-half-close.
The server observes EOF before it responds. A 49th byte, or failure to reach
EOF within the original absolute pre-auth deadline, is malformed and receives
no discovery data. That same deadline covers detection, waiting for EOF, and
the complete response; a configured write timeout may shorten it but never
extend it.

`spki_length` must be 1..4096 and `body_length` must be exactly
`6 + spki_length`; the response contains the running server's configured DC
and RSA public key. A valid request emits at most one bounded response and
closes without codec negotiation, auth-key or session state, RPC dispatch, or
database access. A partial, unterminated, malformed, overlong, or wrong-version
request receives no discovery response. Bytes consumed while identifying an
ordinary MTProto stream are replayed in order, so plaintext and obfuscated
MTProto retain their existing behavior.

Discovery response attempts are bounded independently of the existing
whole-operation pre-auth deadline and connection caps. By default a process may
admit 60 valid requests to the response path per minute and one `/32` IPv4 or
`/64` IPv6 client network may admit 10 per minute. `TG_RATE_LIMIT_DISCOVERY=0` and
`TG_RATE_LIMIT_DISCOVERY_IP=0` disable the respective bounds; their window
variables must remain positive while the bound is enabled. The
`TG_DISCOVERY_RATE_LIMIT`, `TG_DISCOVERY_RATE_LIMIT_WINDOW`,
`TG_DISCOVERY_RATE_LIMIT_PER_IP`, and `TG_DISCOVERY_RATE_LIMIT_PER_IP_WINDOW`
spellings are accepted as aliases, but a canonical variable and its alias may
not both be set. In PROXY-v2 mode, the per-network bucket uses the trusted
reported client address, never the untrusted socket peer.

The local TCP exchange is not an authenticity channel. A network attacker can
race the first local connection and return a different public key; the nonce
only prevents stale response replay. Compare the preflight SPKI with the
HTTPS discovery document or an out-of-band fingerprint before trusting it,
and treat HTTPS as the bootstrap trust anchor. Do not interpret a successful
preflight as proof that the endpoint is the intended server.

### Object-store backend

With all `TG_BLOB_S3_*` variables unset or empty, uploaded blobs use the local
filesystem at `TG_BLOB_DIR`, exactly as before. Setting any object-store
variable to a non-empty value selects S3 mode and requires an endpoint, bucket,
non-empty prefix, access key, and exactly one secret source. The server validates
the S3 client
and lists the configured namespace before it opens for service; a failed check
stops startup and never falls back to local storage.

Use `TG_BLOB_S3_SECRET_ACCESS_KEY_FILE` for the secret. The file should be
readable by the server process and contain only the secret, optionally followed
by a newline. The raw `TG_BLOB_S3_SECRET_ACCESS_KEY` form is accepted for
compose or CI secret injection only; do not put it in a developer shell,
checked-in environment file, command line, or ordinary process environment.

The access key must be scoped to operations on this one bucket and this one
prefix: list, get, put, and delete objects below the prefix only. Do not grant
bucket-root, wildcard, ACL, or policy-management permissions. The server does
not set object ACLs; keep the bucket private. Existing local blobs are not
migrated automatically when S3 mode is enabled.

HTTPS certificate verification is always enabled. Set `TG_BLOB_S3_CA_PATH`
only when the endpoint uses a private CA bundle. Plaintext HTTP is rejected
unless `TG_BLOB_S3_ALLOW_INSECURE_HTTP=true` is explicitly set for a loopback
or compose-only endpoint; startup logs a warning when this escape hatch is
used. There is no TLS verification bypass setting.

The main Compose file uses RustFS as its default blob store. Generate separate
RustFS root and `teagramd` credentials on the box, keep `.env` at mode 0600,
and let Compose mount those values as read-only files for the services that
need them. The app key is limited to the `teagram` bucket's `teagramd/`
prefix. Provision a separate media key outside the repository before starting
the default S3 stack:

```bash
sudo install -d -o 65532 -g 65532 -m 0700 /etc/teagramd/media-keys
sudo sh -c 'umask 077; openssl rand -hex 32 > /etc/teagramd/media-keys/1.key'
sudo chown 65532:65532 /etc/teagramd/media-keys/1.key
sudo chmod 0600 /etc/teagramd/media-keys/1.key
```

The default Compose service mounts that directory read-only and selects version
1. Fresh S3 objects are encrypted at rest; a server with an accepted plaintext
cutover authority keeps serving those existing objects in plaintext mode until
the separately reviewed migration stage. RustFS and its console have no
published ports. The internal S3 URL is plaintext on the private Compose
network; the explicit HTTP setting is logged at startup. See the
[S3 media threat model](media-storage-threat-model.md) for the key-custody,
object-tampering, and accepted-plaintext boundaries.

On first S3 startup, Compose may create an empty `.state/blob-mode` bind source.
The server treats it as no authority only while the directory has no entries,
then requires the media key and selects encrypted mode. Existing authority
records are always validated and keep their selected mode. For a deployment
without an authority, use a fresh S3 namespace; existing plaintext objects need
their `s3-accepted` authority until the reviewed migration completes.

For local development, copy `.env.example` to `.env`, generate credentials,
then run:

```bash
chmod 600 .env
./deploy/bootstrap-rustfs-secrets.sh
docker compose up
```

The bucket and scoped app user are created idempotently during startup.
`docker compose down` preserves RustFS data; never use `down -v` on the
teagram-server LXC.

The server and Compose files include an S3 backend, but the available rollout
runner supports only inspected local initialization and same-backend local
updates. Do not use `blob-migrate` or `blob-restore` to switch or recover a
production deployment. Publishing S3 or recovered-local authority and running
the verified copy/restore transition are deferred to a separately reviewed
orchestration path. Keep the local overlay and `tgblobs` volume in use until
that path is available. Include the `rustfsdata` named volume in any LXC backup
or snapshot because Compose persistence alone is not backup or restore.

### What the pre-auth bounds do and do not cover

The three `TG_*PREAUTH*` settings bound connections that have not authenticated:
an unauthenticated population spread over k client networks holds at most
`min(TG_MAX_PREAUTH_CONNS, k × TG_MAX_PREAUTH_CONNS_PER_IP)` sockets, each for at
most `TG_PREAUTH_LIFETIME`.

They end at the first frame that decrypts under a key the server issued, not at
sign-in: a client completing its password challenge must not be closed. Each
zero-key-id exchange is separately charged before RSA work to
`TG_KEY_EXCHANGE_PER_CONNECTION`, the listener's per-network Postgres budget
when enabled, and the deployment-wide in-memory ceiling, in that order. A
normal client exchanges once and may continue signing in or reconnecting on
that key; encrypted frames and reconnects on an existing key are not charged.
A refused exchange receives `-429` and the connection closes. The
per-connection count survives pre-auth slot clearing and renegotiation.

The per-network limit uses the same IPv4 address or IPv6 **/64** key as the
other per-IP limits, with separate public WebSocket and TCP/tailnet budgets.
With no network-limit settings, the public and default listeners both keep the
20-per-hour default in every topology. The default listener's `tailnet`
classification does not prove a peer came through Tailscale, and
`TG_LISTEN_ADDR` binds all interfaces by default. Set
`TG_RATE_LIMIT_KEY_EXCHANGE_IP` to apply the same network limit to both listener
families. `TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET` overrides that value for only
the tailnet listeners; set it to `0` only when the deployment independently
restricts their reachability to the tailnet. The per-connection cap and
in-memory ceiling still apply. Unkeyable addresses skip the network layer only.
The in-memory ceiling is divided by
`TG_REPLICA_COUNT`, so it bounds exchange work even when an attacker spreads
requests across many networks. `TG_MAX_CONNS_PER_UNBOUND_KEY` still bounds the
connections one unbound key can hold: a peer with m keys nobody has signed in on
holds at most `m × TG_MAX_CONNS_PER_UNBOUND_KEY` connections beyond the pre-auth
population above. Each persisted key now costs an exchange from all three
budgets, but the number m can still accumulate over time until the sibling
retention sweep expires eligible keys. Once someone signs in on a key, the
per-user connection cap takes over, including if that key is unbound again
afterwards, as the 2FA path does.

When a sign-in stages a password challenge, that connection keeps its unbound-key
hold and also consumes one `TG_MAX_PENDING_LOGIN_CONNS` slot until its goroutine
exits. Its reads then use one absolute ten-minute lease, not a refreshed
per-frame timeout; the SRP challenge itself still expires after five minutes.

One thing still sits outside all of it, on purpose: a connection carrying no
client address is charged to no network bucket, so keep
`TG_CLIENT_ADDR_PROXY_CIDRS` to the balancers.

Postgres must already be reachable at `TG_POSTGRES_DSN`, and its schema must
already be migrated with Atlas (`atlas migrate apply --env local`; see
`docs/migrations.md`). The server does not apply migrations — `store.Open`
verifies the schema is current and fails fast otherwise.

The pool defaults budget 8 sessions per process for the 2-vCPU, 4-GiB host.
Two sessions stay warm and idle sessions above that floor retire after 5 minutes.
These limits override `pool_*` values in the DSN for `teagramd` and its operator
commands. Budget each replica separately, plus the dedicated notification
listener and operator connections, against Postgres's connection limit. The
existing 17-second statement timeout still applies to every pooled session.
The upload assembly reserve is computed from the configured maximum.

The server does not generate an RSA identity during startup. For a fresh
installation, run `teagramd bootstrap-identity` once with `TG_RSA_KEY_PATH`
set to the persistent key volume. It writes a 2048-bit PKCS#1 PEM key with
mode `0600`, atomically publishes the complete file, and refuses to overwrite
any existing destination. Existing deployments keep their mounted key and do
not regenerate it. Serving and `client-config` load the existing key only;
missing, unreadable, or invalid keys stop startup before listeners open.

For a replica deployment, set `TG_REPLICA_ID` and the same
`TG_RSA_KEY_FINGERPRINT` on each process. The fingerprint is the signed
decimal value printed by bootstrap or startup. A mismatch stops startup. The
auth-key encryption key must also be provisioned identically on every replica,
through `TG_AUTHKEY_ENC_KEY` or an existing `TG_AUTHKEY_ENC_KEY_FILE`. When
either `TG_REPLICA_ID` or `TG_RSA_KEY_FINGERPRINT` is set, a missing key file
stops startup without creating one.
Startup checks that one stored auth key decrypts under the configured key. With
no stored auth keys, startup reports the first-bootstrap case and continues.

## 2. Get the RSA key identity

At startup the server logs the key it loaded:

```
level=INFO msg="server RSA key" key_id=<64 hex chars in 16 dash-separated groups of 4> fingerprint=<int64> path=server_key.pem
```

(`cmd/teagramd/main.go`, right after `rsakey.Load`).

- `key_id` is the SHA-256 of the DER SubjectPublicKeyInfo encoding of the
  public key, hex-encoded as 16 dash-separated groups of 4 characters
  (e.g. `a1b2-c3d4-e5f6-a7b8-...`). **This is the value to compare out of
  band** — a client UI that displays the same digest for the key it loaded
  must render the identical grouped format. The int64 `fingerprint` is the
  legacy Telegram value; it is retained for clients that still match on it
  but is too short to be a trustworthy out-of-band check. Byte-level oracle
  for the digest, against a public key file (SPKI or PKCS#1 PEM — OpenSSL
  normalises both to SPKI on `-pubin`):

  ```bash
  openssl pkey -pubin -in server_pub.pem -outform DER | sha256sum
  ```
- A client must verify this identity through its trusted enrollment source and
  use the matching RSA key for the auth-key handshake. Teagram Desktop obtains
  the server identity during enrollment and pins it to the account; see the
  [server enrollment guide](https://github.com/teagramhq/teagram-desktop/blob/dev/docs/server_enrollment.md).

Also note the `listening addr=... advertise=... dc=...` log line that
follows — it confirms the actual bind address, the address clients are told
to dial, and the DC id the process is using, which may differ from what you
passed if `TG_LISTEN_ADDR` was left at default.

## 3. Point a client at this server

For Teagram Desktop, add the server through the enrollment flow. The client
discovers its endpoint and RSA identity, verifies and pins them to the account,
then asks for the username and password. For public DNS, publish the discovery
document at `/.well-known/teagramd/client`; eligible local endpoints can use
the local-direct preflight described above. See the [server enrollment
guide](https://github.com/teagramhq/teagram-desktop/blob/dev/docs/server_enrollment.md).

Other Teagram clients must use the advertised address and DC id and verify the
server's RSA identity through a trusted enrollment source before sending
credentials. Stock Telegram Desktop and mobile apps cannot connect because
their production data centers and RSA keys cannot be changed in the app.

Once connected, the client's `help.getConfig` call gets back a single-DC
`tg.Config` (see `api.DefaultConfig` in `internal/api/config.go`) describing
this server as `ThisDC`.

## 4. Username and password sign-in

Teagram clients use usernames and passwords only. They do not ask for a phone
number or QR scan. The pinned MTProto schema uses `auth.sendCode` and
`auth.signIn` to open a username password challenge. The returned sent-code
hash is a handshake token; no login code is delivered or entered. The client
gets SRP parameters with `account.getPassword` and completes sign-in with an
SRP proof through `auth.checkPassword`.

The schema calls the username field `phone_number`; clients send the username
there. For an existing account, `auth.signIn` starts the password challenge.
The `phone_*` field names are protocol names and do not mean a phone number or
phone code is used.

`TG_LOG_LOGIN_CODES` is a legacy diagnostic option, not a Teagram sign-in
step. Leave it disabled for Teagram clients.

### Recovery-email retention

`TG_RECOVERY_EMAIL` defaults to `off`. In that mode, setting or changing a
password writes a NULL recovery email, and password settings never return an
email. `account.getPassword` always reports `has_recovery=false`. Set
`TG_RECOVERY_EMAIL=contact` to retain the email supplied with a password set or
change. It is unverified, stored in plaintext, and returned only by
`account.getPasswordSettings` after a valid SRP proof. The server sends no mail
and has no automated recovery RPC.

Email-only `account.updatePasswordSettings` calls remain accepted no-ops. They
cannot add, replace, or remove an email, and they do not change the password.
Switching to `off` stops future email writes and responses while preserving
existing rows; a later password set or change can overwrite the row with NULL.
Switching back to `contact` exposes any retained row again and does not backfill
missing emails. There is no bulk erase. The local `teagramd admin set-password`
command changes the password verifier and leaves its recovery-email row
untouched.

An operator can read `contact` emails from the database. The verifier is sealed
at rest; the email is not. The email is not proof of identity: authenticate a
reset requester through a separate channel or factor before using
`teagramd admin set-password`. A database dump made before switching to `off`
continues to contain emails stored at dump time. A slow password upsert can
also include bound values in the slow-query log.

## 5. Register a username account

### 5a. Registration policy

The regular MTProto and WebSocket listeners use `TG_REGISTRATION` for
`auth.signUp`: `closed` (the default) rejects new sign-ups, `invite` requires an
operator-issued registration invite, and `open` allows a username without an
invite. The separately configured public WebSocket listener uses
`TG_PUBLIC_REGISTRATION`, which defaults to `closed` independently of the
tailnet policy. Existing accounts can sign in in every mode.

Before a client asks a user to register, it can call `help.getAppConfig` and
read `registration_mode` to learn whether sign-up is closed, invite-only, or
open.

An unauthenticated client can call `account.checkUsername` before submission:
`boolTrue` means the candidate is available, and `boolFalse` means it is
occupied. This exposes one bit of account occupancy for a chosen handle and
allows repeated candidate probing; it returns no profile data and is not an
account-privacy boundary. The default budget is 20 calls per observed network
per hour, charged before username validation. That is four times the default
`auth.signUp` call budget of five per hour when sign-up is active, leaving
headroom for correcting a candidate while keeping the final account-creation
quota lower. The check is not gated by registration mode, so its occupancy
signal remains available in closed and invite modes. This is an abuse throttle,
not a privacy guarantee: targeted probes fit within one window and distributed
sources multiply the budget. `auth.signUp` remains authoritative if availability
changes after the check. See the
[public listener threat model](public-websocket-threat-model.md#authentication-controls)
for shared-bucket and residual-risk details.

### 5b. New account flow

When registration is enabled, a Teagram client collects a username, display
name, and password. In invite mode, the user also provides the registration
invite secret. The username handshake returns `authorizationSignUpRequired` for
a new account; the client carries the invite secret in the schema's `phone_code`
field on `auth.signIn`, then creates the account through `auth.signUp`. Open
mode does not need that secret. The client sets the password with
`account.updatePasswordSettings`. The account remains provisional until
password setup succeeds, and it cannot use normal RPCs before then.

For a fresh database, set `TG_REGISTRATION=open` and complete the first account
sign-up and password setup. Keep registration open on the development server so
users can continue creating accounts; other deployments can close registration
or switch to invite mode after initial setup. The first account committed by
the admission transaction becomes the durable server administrator. For later
invited accounts, issue an invite with
`teagramd invite issue <username>`; the command prints the secret once.
Deliver it to the user through a private channel. `teagramd invite list`
and `teagramd invite revoke <id>` manage outstanding invites.
For user-issued invites, `teagramd invite revoke-user <user-id>` revokes all of
that user's live invites, `teagramd invite invitees <user-id>` lists redeemed
invitees and redemption times, and `teagramd invite inviter <user-id>` shows the
recorded inviter or that the account was created without a user invite.

### 5c. User-issued registration invites

Signed-in, non-provisional accounts can create invite links on a listener in
`invite` mode when its configured `TG_USER_INVITE_QUOTA` is positive. The
account must be at least `TG_USER_INVITE_MIN_ACCOUNT_AGE` old. The quota counts
all invites created by that account during the trailing seven days, including
used, expired and revoked invites. Revoking an invite does not free a quota
slot. The default minimum account age is seven days; the default quota is zero.

The create response is the only response that contains the secret. Copy or
share the link when it is created. The fixed landing path is `/invite`, and the
link format is `TG_PUBLIC_LINK_PREFIX` + `invite#` + the 64-character lowercase
hexadecimal secret. The server stores only the SHA-256 digest. List responses
never include the secret, digest or redeemer.

The methods and result constructors below extend the pinned Telegram layer with
these custom `teagram.*` definitions. `int` dates are Unix seconds and `long`
fields are signed 64-bit integers.

```text
teagram.createUserInvite#bf4f8b0e = teagram.UserInviteLink;
teagram.listUserInvites#2ad32052 = teagram.UserInvites;
teagram.revokeUserInvite#a483022c id:long = Bool;

teagram.userInviteLink#0078b3b0 id:long link:string expires_at:int = teagram.UserInviteLink;
teagram.userInvite#ff81dc6c id:long state:string created_at:int expires_at:int = teagram.UserInvite;
teagram.userInvites#b5cfec46 invites:Vector<teagram.UserInvite> quota:long weekly_count:long flags:# next_available_at:flags.0?long = teagram.UserInvites;
```

`teagram.listUserInvites` returns only the caller's invites, newest first, up to
100 entries. The state is `pending`, `used`, `expired` or `revoked`. Its summary
contains the configured quota, the number of invites that currently count
toward it, and the creation time plus seven days for the oldest counted invite.
`next_available_at` is absent when no invites count toward the quota.
`teagram.revokeUserInvite` returns `BoolTrue` for an owned live invite and also
for an absent, foreign, expired, used or already revoked id, so it does not
reveal whether another user's id exists.

Create errors are stable: `USER_INVITE_REGISTRATION_CLOSED` (400) when the
accepting listener is not in `invite` mode, `USER_INVITES_DISABLED` (400) when
the quota is zero, `USER_INVITE_ACCOUNT_TOO_NEW_WAIT_<seconds>` (420) with the
remaining wait rounded up to seconds, and `USER_INVITE_WEEKLY_LIMIT_REACHED`
(400) when the trailing-seven-day quota is full. An unauthenticated or
provisional session receives `AUTH_KEY_UNREGISTERED` (401). List and revoke
require an authorized session but remain available if invite creation has
since been disabled.

### 5d. Promote an existing operator after the administration migration

When applying the server-administration migration to a non-empty deployment,
the existing account remains unchanged and the durable administrator grant is
left unassigned. Run this one-shot local maintenance command with the same
database and encryption-key environment as the deployment:

```bash
./teagramd maintenance assign-operator
```

The command takes no user ID. It succeeds only when the database contains
exactly one canonical username-mode `operator` account with one readable
verifier and a closed, unassigned administration singleton. A retry after a
successful assignment is a no-op; account, username, and verifier data are
never created or changed.

### 5e. Reset an existing username password from the local CLI

`teagramd admin set-password` is a local operator command. It has no RPC or
admin-dashboard route. Run it inside the deployment container with a protected
file on stdin; it refuses terminal input, password flags, empty input, and
multi-line input. The command accepts one line and removes one trailing LF or
CRLF. It does not validate registration policy or start server listeners, so it
also works when `TG_REGISTRATION=open`.

On the LXC, create a root-only directory and credential file, then pass the file
to the container through stdin:

```bash
install -d -o root -g root -m 0700 /root/teagram-password-reset
umask 077
openssl rand -base64 24 > /root/teagram-password-reset/tester1.password
chmod 0600 /root/teagram-password-reset/tester1.password
docker compose exec -T teagramd teagramd admin set-password --username tester1 \
  < /root/teagram-password-reset/tester1.password
```

The handle may be supplied with or without `@`, and matching is ASCII
case-insensitive. Success writes only the canonical handle and user ID to
stderr. The new password must not appear in argv, environment variables, shell
history, logs, issue comments, or pull requests. Keep the credential file
root-owned and private; hand it to the account owner over the deployment's
approved private channel, wait for login confirmation, then remove it.

Before a production reset, take a recoverable PostgreSQL backup containing the
current `public.user_passwords` row and test the restore procedure in an
isolated database. If the backup tool cannot filter individual rows, back up
the whole table rather than relying on an unsupported row-filter option. Keep
the backup root-only and the encryption master key under its separate secret
handling policy. If login verification fails, restore only the affected row in
a reviewed transaction, verify that its old verifier decrypts with the original
master key, and confirm the prior login before removing the backup. Do not
blindly replay a whole-table dump into a live database.

## 6. Supported Teagram clients

| Client | Status |
|---|---|
| Teagram Desktop | Supported macOS client. It lets users choose a server, verifies and pins the server identity to the account, then requests username and password. See the [desktop README](https://github.com/teagramhq/teagram-desktop/blob/dev/README.md) and [server enrollment guide](https://github.com/teagramhq/teagram-desktop/blob/dev/docs/server_enrollment.md). |
| Teagram Web | Username/password sign-in is in progress and does not work yet (MAIN-1541). |
| Stock Telegram clients | Unsupported. They use Telegram production DC addresses and RSA keys. |

Other Teagram clients must use the server advertised address and DC id, and
verify its RSA identity before sending account credentials.

### Sticker pack import

Before uploading files for a sticker or emoji pack, a client can call
`help.getAppConfig` and read the `sticker_import_mode` string. `on` means the
instance permits accounts to create sticker sets and add stickers to their
sets. `off` means the client should hide or disable “Import pack” and explain
that this instance has disabled pack imports. The setting applies to every
account on the instance. Existing sets remain available to resolve, install,
send, and use.

## Known ceilings

- **Single DC.** The server only ever advertises itself (`api.DefaultConfig`
  builds one `tg.DCOption`). No multi-DC routing, no migration between DCs.
- **A partial RPC surface.** The 56 methods registered in `api.New`
  (`internal/api/handler.go`) are the whole of it. Every other method falls to
  `handlers.handleUnknown`, which returns `INPUT_METHOD_INVALID` and logs one
  line per call:

  ```
  level=WARN msg="method not implemented" type_id=0xec86017a method=account.registerDevice#ec86017a error_code=400 error=INPUT_METHOD_INVALID
  ```

  `method` is resolved through `tg.TypesMap()` and reads `unknown` for a
  constructor the pinned layer has no name for, which means either a client on
  a different layer or a method added after `gotd/td v0.161.0`. Grep the log
  for `"method not implemented"` to inventory what a client wanted and did not
  get. A full-featured client trips this often once past login.
- **Placeholder access hash.** The user returned by `auth.signIn` uses its
  own numeric ID as `AccessHash` (see the `self access hash placeholder`
  comment in `userTL`, `internal/api/passwords.go`) rather than a real
  per-session hash. Peer access hashes everywhere else are derived per viewer
  by `internal/peerhash`.
