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

For the full server configuration reference, including defaults, accepted values, and purpose, see [teagramd configuration](configuration.md).

### Session data and request processing

The session metadata settings are deployment-wide and must have the same values
on every replica. By default, no session IP, recovery email, or phone number is
kept as account or session data: `TG_SESSION_IP=false`,
`TG_RECOVERY_EMAIL=off`, and Teagram sign-in uses a username rather than a
phone number. Device and app labels are stored by default with
`TG_SESSION_DEVICE_LABELS=true`, and the server operator can read them.

The session IP setting controls session metadata only. Peer addresses are
processed in memory for connection admission and per-network abuse limits, but
are not retained as session metadata when `TG_SESSION_IP=false`. Client
budgets follow each listener's configured path: tailnet listeners bypass
per-client rate and connection limits, while limited listeners enforce them
and the public WebSocket listener has its own caller-keyed budgets. Defaults
are 10 `auth.sendCode` calls per network per hour, 20 distinct `auth.sendCode`
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
`TG_CLIENT_ADDR_TRUST`. In `cf-connecting-ip` mode, only the public WebSocket
listener uses its validated `CF-Connecting-IP` header; other listeners use
their socket peers. The production k3s overlay currently sets this trust mode
to `socket`, so teagramd uses its immediate peer address there and ignores that
header. Listener scope comes from the accepted socket, not `Host`, `Origin`, or
`X-Forwarded-For`. The `auth.sendCode` tables store that network prefix; other
IP rate-limit rows
store a deterministic FNV-derived key, which is enumerable for IPv4 and
pseudonymous rather than anonymous.
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
valid for native clients unless `TG_WEBSOCKET_BROWSER_ONLY=true`; requests
carrying one are rejected unless it matches the configured list. The public
listener uses the same policy through its own origin allowlist and
`TG_PUBLIC_WEBSOCKET_BROWSER_ONLY` setting. Browser-only refusals return HTTP
403 with `Teagram-Client-Policy: browser-only`; this lets native clients
distinguish the policy from other handshake failures. `GET` and `OPTIONS` on exactly
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
token file. A matching token bypasses Siteverify only as the `signup` action,
is compared in constant time, and is never logged. The isolated demo deploy
probe may wrap `auth.sendCode` with this token to reach the signup path while
the sign-in gate is on; that code is bound to signup and cannot authorize a
sign-in. The signup IP limit and all other registration checks still apply.
Configuration rejects the token on any stack other than `demo.teagram.io`.
Set `TG_TURNSTILE_SIGNIN_ENABLED=true` to also challenge username sign-in at
`auth.sendCode`; this switch defaults to `false`, and other RPCs remain
ungated. Behind `TG_CLIENT_ADDR_TRUST=proxy-v2`, the server also sends the
client IP to Cloudflare for verification.

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

This contract is implemented by the current teagram-server source. With demo
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

When the listener's `TG_WEBSOCKET_BROWSER_ONLY` or
`TG_PUBLIC_WEBSOCKET_BROWSER_ONLY` setting is true, its discovery document adds
`"browser_only":true` inside `websocket`. The field is omitted otherwise.
The document still advertises its `mtproto` binding, but Desktop treats this
flag as a refusal of the whole server and stops before committing that binding.
Enable either setting only when the advertised raw MTProto endpoint is
unreachable to native clients; `teagramd` cannot verify reachability outside
the process. The startup line records `browser_only` and
`native_mtproto_endpoint_must_be_unreachable` for each WebSocket listener.
Existing discovery documents without the field keep their current behavior.

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

For local Compose development, the main Compose file uses RustFS as its
default blob store. Generate separate RustFS root and `teagramd` credentials,
keep `.env` at mode 0600, and let Compose mount those values as read-only files
for the services that need them. The app key is limited to the `teagram`
bucket's `teagramd/` prefix. Provision a separate media key outside the
repository before starting that local S3 stack:

```bash
sudo install -d -o 65532 -g 65532 -m 0700 /etc/teagramd/media-keys
sudo sh -c 'umask 077; openssl rand -hex 32 > /etc/teagramd/media-keys/1.key'
sudo chown 65532:65532 /etc/teagramd/media-keys/1.key
sudo chmod 0600 /etc/teagramd/media-keys/1.key
```

The local Compose service mounts that directory read-only and selects version
1. Fresh S3 objects use the encrypted envelope described in the
[S3 media threat model](media-storage-threat-model.md). The current production
deployment uses the in-cluster RustFS service and persistent volumes described
in the [k3s deployment guide](../deploy/k8s/README.md); its media keys and S3
credentials come from externally supplied Kubernetes resources. A separate
legacy local-blob volume is mounted read-only for recovery. Do not use the old
Compose rollout procedures to switch or restore production media.

For local development, copy `.env.example` to `.env`, generate credentials,
then run:

```bash
chmod 600 .env
./deploy/bootstrap-rustfs-secrets.sh
docker compose up
```

The bucket and scoped app user are created idempotently during local startup.
`docker compose down` preserves RustFS data; do not use `down -v` when local
data must be retained. Production media backup and recovery use the k3s
procedures in the deployment guide, not the retired Compose rollout path or
its blob-mode publication commands.

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
missing emails. There is no bulk erase.

An operator can read `contact` emails from the database. The verifier is sealed
at rest; the email is not. A database dump made before switching to `off`
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
changes after the check.

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

### 5e. Existing username password command

`teagramd admin set-password` is a local operator command with no RPC or
admin-dashboard route. It changes the password verifier and leaves its
recovery-email row untouched.

## 6. Supported Teagram clients

| Client | Status |
|---|---|
| Teagram Desktop | Supported macOS client. It lets users choose a server, verifies and pins the server identity to the account, then requests username and password. See the [desktop README](https://github.com/teagramhq/teagram-desktop/blob/dev/README.md) and [server enrollment guide](https://github.com/teagramhq/teagram-desktop/blob/dev/docs/server_enrollment.md). |
| Teagram Web | Supports username/password sign-in to its configured server. It does not use phone-number or QR sign-in. |
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
