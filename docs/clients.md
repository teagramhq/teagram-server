# Connecting a client to telegramd

`telegramd` speaks MTProto and stores accounts, messages, and media metadata in
Postgres. Media bodies use local storage by default. Teagram clients sign in
with a username and password; they do not use phone codes or QR scans.

Teagram Desktop is the supported client. Stock Telegram clients use Telegram's
production data centers and RSA keys, so they cannot connect to this server.

## 1. Build and run the server

```bash
go build -o telegramd ./cmd/telegramd
TG_POSTGRES_DSN="postgres://user:pass@localhost:5432/telegramd?sslmode=disable" \
  TG_AUTHKEY_ENC_KEY="$(openssl rand -hex 32)" \
  TG_PUBLIC_LINK_PREFIX="https://links.example.test/" \
  ./telegramd
```

Configuration is read from environment variables in `internal/config/config.go`:

| Variable            | Default          | Notes                                      |
|---------------------|------------------|---------------------------------------------|
| `TG_LISTEN_ADDR`    | `:2443`          | `host:port` (or `:port`) the server binds   |
| `TG_WEBSOCKET_LISTEN_ADDR` | *(unset)* | Enables the WebSocket MTProto listener on this address; browser clients connect to `/apiws` |
| `TG_RPC_DEADLINE` | `23s` | Per-request timeout; overrides must be positive and no greater than `45s`. `0s` and values above `45s`, including the former `90s` setting, fail startup so admitted requests fit the fixed shutdown drain |
| `TG_WEBSOCKET_ALLOWED_ORIGINS` | *(unset)* | Comma-separated browser origins allowed to connect to `/apiws`; unset rejects every request carrying an `Origin` header |
| `TG_ADVERTISE_ADDR` | *(derived from `TG_LISTEN_ADDR`)* | `host:port` clients are told to dial, used verbatim. Derived when unset: the listen address with an empty or wildcard host (`:2443`, `0.0.0.0`, `::`) replaced by `127.0.0.1`. A value that is not `host:port`, has no host, or has a port that is not an integer in 1–65535 fails startup |
| `TG_PUBLIC_LINK_PREFIX` | *(required)* | Lowercase HTTPS origin for client and invite links, with a root path and no port (e.g. `https://links.example.test/`) |
| `TG_POSTGRES_DSN`   | *(required)*     | Postgres connection string; no default, server fails to start without it |
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
| `TG_DC_ID`          | `2`              | DC id this server advertises as `ThisDC`    |
| `TG_RATE_LIMIT_DISCOVERY` | `60` | Cluster-wide valid local-direct preflight response attempts per fixed window, counted in Postgres; `0` disables this bound, and a negative or non-integer value fails startup |
| `TG_RATE_LIMIT_DISCOVERY_WINDOW` | `1m` | Fixed window for the cluster-wide discovery bound; it must be positive while that bound is enabled |
| `TG_RATE_LIMIT_DISCOVERY_IP` | `10` | Valid local-direct preflight response attempts per client network (`/32` for IPv4 or `/64` for IPv6) per fixed window, shared through Postgres; `0` disables this bound |
| `TG_RATE_LIMIT_DISCOVERY_IP_WINDOW` | `1m` | Fixed window for the per-network discovery bound; it must be positive while that bound is enabled |
| `TG_LOG_LOGIN_CODES`| `false`          | Legacy login-code diagnostic. It is not used by Teagram username/password sign-in; leave it disabled. A non-boolean value fails startup |
| `TG_REGISTRATION`   | `closed`         | Accepted values are `closed`, `invite`, and `open`. `closed` rejects `auth.signUp`, `invite` requires an operator-issued invite, and `open` admits usernames without one. An unrecognized value fails startup. Sign-in for accounts that already exist is unaffected by this setting |
| `TG_ADMIN_LISTEN_ADDR` | *(unset)* | Enables the separate authenticated admin HTTP listener; must be set with `TG_ADMIN_TOKEN_HASH` and should remain on an operator-only network |
| `TG_ADMIN_TOKEN_HASH` | *(unset)* | Lowercase SHA-256 hex digest of the raw admin token; never put the raw token in configuration or a URL. See `docs/observability.md` |
| `TG_ADMIN_ORIGIN` | *(unset)* | Fixed origin for admin login/logout behind a proxy; canonical lowercase ASCII HTTPS origin, or HTTP only for localhost/loopback. Unset, empty, or whitespace-only derives it from the listener. See `docs/observability.md` |
| `TG_REPLICA_ID`     | *(unset)*        | Optional stable operator-supplied identity shown on authenticated admin metrics; when set, `TG_RSA_KEY_FINGERPRINT` is required. The value must be 1–64 characters from `A-Z`, `a-z`, `0-9`, `.`, `_`, and `-` |
| `TG_REPLICA_COUNT` | `1` | Maximum number of `telegramd` replicas running at once. Set the same value on every replica; it divides local connection budgets, including the default per-user cap of 20, so the combined cap stays at or below the configured total. A non-zero local budget smaller than this count fails startup. Do not mix replica counts during a rollout |
| `TG_RATE_LIMIT_GET_FILE` | `50` | Per-account `upload.getFile` calls in one fixed window. `0` disables this bound; a negative or non-integer value fails startup |
| `TG_RATE_LIMIT_GET_FILE_WINDOW` | `1s` | Window for the per-account `upload.getFile` bound. It must be positive while that bound is enabled; an invalid or negative duration fails startup |
| `TG_RATE_LIMIT_GET_FILE_REPLICA` | `400` | Cluster-wide aggregate `upload.getFile` calls across all accounts in one fixed window, counted in Postgres. `0` disables this bound; a negative or non-integer value fails startup |
| `TG_RATE_LIMIT_GET_FILE_REPLICA_WINDOW` | `1s` | Window for the cluster-wide aggregate `upload.getFile` bound. It must be positive while that bound is enabled; an invalid or negative duration fails startup |
| `TG_CLIENT_ADDR_TRUST`| `socket`       | Where the address a per-IP limit is keyed on comes from: `socket` or `proxy-v2`. Any other value fails startup by name. `socket` is the connection's own peer address and assumes one peer address is one client, which fails from either end: behind a proxy or an L4 load balancer every peer address is the balancer's, so one bucket holds every client and the per-IP cap becomes a global one; behind a carrier NAT one address covers thousands of mobile subscribers, who then spend each other's budget. The server warns about both once at startup while any per-IP limit is on. `proxy-v2` takes the address from a PROXY protocol v2 header and is what to run behind an L4 load balancer; it needs `TG_CLIENT_ADDR_PROXY_CIDRS` and emits no such warning, because the misconfiguration it warns about fails the start instead |
| `TG_MAX_PREAUTH_CONNS`| `1024`       | Cluster-wide concurrent connections that have not authenticated yet. `TG_REPLICA_COUNT` divides this into per-replica accept-loop caps, so sockets past a share are closed before they cost a goroutine, a deadline or a read. `0` disables it; a negative or non-integer value fails startup |
| `TG_MAX_PREAUTH_CONNS_PER_IP`| `64`    | The same, per client network, divided by `TG_REPLICA_COUNT` and enforced in each replica's memory. Keyed on the network the per-IP rate limits already use — an address for IPv4, a **/64** for IPv6, since a host on a routed v6 allocation mints addresses inside its own /64 for free — and on the address `TG_CLIENT_ADDR_TRUST` names, which in `proxy-v2` mode is the one the balancer reports and never the socket peer. A connection carrying no address at all (a `LOCAL` health check, or a socket peer the transport could not report) is charged to nothing and stays bounded by the other two. It is a concurrency cap and not a rate. `0` disables it; a negative or non-integer value fails startup |
| `TG_PREAUTH_LIFETIME`| `2m`          | How long a connection may stay unauthenticated, measured from accept. Past it the socket is closed whatever it is sending, which is the only bound that reaches a peer that stays inside every deadline by dripping one small frame per read timeout. It ends at the first frame that decrypts under a key the server issued, so a client completing its password challenge is not cut off. Do not set it below a minute: gotd applies a 60s timeout per read inside key exchange, a shorter ceiling starts cutting handshakes that are merely slow, and the server warns at startup if you do. `0` disables it; a negative or unparseable duration fails startup |
| `TG_MAX_CONNS_PER_UNBOUND_KEY`| `8`  | Concurrent connections one auth key with nobody signed in on it may hold, divided by `TG_REPLICA_COUNT` and enforced in each replica's memory. It covers the population between pre-auth connections and signed-in sessions. A connection is charged only once a frame has decrypted under the key. Past the cap the frame in hand is answered and the socket is then closed. `0` disables it; a negative or non-integer value fails startup |
| `TG_MAX_PENDING_LOGIN_CONNS`| `1024` | Cluster-wide concurrent connections waiting for `auth.checkPassword` after `SESSION_PASSWORD_NEEDED`, held in Postgres leases. A pending connection remains counted in the unbound-key hold and this cap, and receives a single ten-minute absolute read lease (`2 × srp.DefaultTTL`); activity never refreshes it. The SRP challenge still expires after five minutes. Past the cap the connection is closed immediately. `0` disables it; a negative or non-integer value fails startup |
| `TG_CLIENT_ADDR_PROXY_CIDRS`| *(unset)* | Comma-separated addresses or CIDRs (`10.0.0.0/8, 192.0.2.7`) of the balancers a PROXY protocol v2 header is accepted from. An IPv4-mapped entry takes its IPv4 meaning (`::ffff:192.0.2.0/120` is `192.0.2.0/24`), since peer addresses are matched unmapped; one too short to name an IPv4 network fails startup rather than starting and matching nothing. Required by, and only read in, `TG_CLIENT_ADDR_TRUST=proxy-v2`: an empty list there fails startup, and a list set in `socket` mode does too, since it means the balancer is in place but every client is being keyed on its address. Both directions then fail closed — a connection from a listed balancer without a valid v2 header is dropped rather than served on the balancer's address, and a header from anywhere else is dropped rather than believed. Only v2: the v1 text form is refused. An address is read only from `PROXY` over `AF_INET`/`AF_INET6` with the `STREAM` transport; the two headers that name no client — the `LOCAL` command a health check sends, and `AF_UNSPEC` — connect but carry no address and so cannot call `auth.sendCode`; every other family or transport is refused. Keep this list to the balancer addresses, not a VPC or subnet range: a connection whose header names no client is charged to no bucket, so anything inside an allowlisted CIDR can send a `LOCAL`/`AF_UNSPEC` header and sit outside `TG_MAX_PREAUTH_CONNS_PER_IP` entirely — with `10.0.0.0/8` that is every workload in the network, with the balancer's own addresses it is the balancer |

When `TG_WEBSOCKET_LISTEN_ADDR` is set, configure
`TG_WEBSOCKET_ALLOWED_ORIGINS` with the browser origins that may connect to
`/apiws`. Requests without an `Origin` header remain valid for native clients;
requests carrying one are rejected unless it matches the configured list.
Keep this listener within the intended network boundary.

Before asking for registration details, an unauthenticated client may call
`help.getAppConfig`. The `help.appConfig` response keeps its standard TL shape;
its JSON object contains a `registration_mode` string with one of `closed`,
`invite`, or `open`. The value is the running server configuration, and the
field is an extension to the JSON object, so clients that do not read it can
continue to ignore it. This call is also available to provisional sessions.

### Static enrollment discovery and local preflight

The server-side contract in this section is provided by telegram-server
revision `b4b18c12` (`MAIN-736`). Deploy that server revision, or a later
revision that retains the contract, as the document consumer. The command
renders the public identity without opening Postgres or loading the auth-key
master secret:

```bash
telegramd bootstrap-identity
telegramd client-config > client.json
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

`client-config` loads the same persistent RSA key that `telegramd serve` uses.
`TG_ADVERTISE_ADDR` and `TG_DC_ID` therefore need
to resolve to the same values for both commands; both have defaults, and an
unset advertise address is derived from `TG_LISTEN_ADDR`. The output is one
deterministic UTF-8 JSON
object with no private-key material and standard padded base64 of DER
SubjectPublicKeyInfo:

```json
{"version":1,"mtproto":{"endpoint":"mtproto.example.com:443","dc_id":2,"rsa_spki":"<standard-padded-base64-DER-SPKI>"}}
```

The command does not publish or serve the file. Put `client.json` at
`/.well-known/telegramd/client` on the selected public HTTPS origin, for
example with a static web server or object store, and keep the file and its
HTTPS certificate under the same deployment's control. HTTPS certificate
serving, web-root publication, and TLS termination are outside telegramd.

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
RustFS root and `telegramd` credentials on the box, keep `.env` at mode 0600,
and let Compose mount those values as read-only files for the services that
need them. The app key is limited to the `telegram` bucket's `telegramd/`
prefix. RustFS and its console have no published ports. The internal S3 URL is
plaintext on the private Compose network; the explicit HTTP setting is logged
at startup.

For local development, copy `.env.example` to `.env`, generate credentials,
then run:

```bash
chmod 600 .env
./deploy/bootstrap-rustfs-secrets.sh
docker compose up
```

The bucket and scoped app user are created idempotently during startup.
`docker compose down` preserves RustFS data; never use `down -v` on the
telegram-server LXC.

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
sign-in: a client completing its password challenge must not be closed, so one
completed key exchange buys connections those three settings no longer
count. `TG_MAX_CONNS_PER_UNBOUND_KEY` is what counts those, so the worst case
extends rather than stopping there. A peer holding m keys nobody has signed in
on holds at most `m × TG_MAX_CONNS_PER_UNBOUND_KEY` connections beyond the
pre-auth population above, and each of those m keys costs its own key exchange,
which had to pass the pre-auth bounds to happen at all. The number m itself is
unbounded until MAIN-252 ships retention of persisted unbound keys, which bounds
m over time by expiring keys nobody signed in on. Once someone does sign
in on a key, what its connections may hold is the per-user connection cap's to
decide and this bound lets them go for good — including if the key is unbound
again afterwards, as the 2FA path does.

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

The server does not generate an RSA identity during startup. For a fresh
installation, run `telegramd bootstrap-identity` once with `TG_RSA_KEY_PATH`
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

(`cmd/telegramd/main.go`, right after `rsakey.Load`).

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
document at `/.well-known/telegramd/client`; eligible local endpoints can use
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

## 5. Register a username account

### 5a. Registration policy

Account creation through `auth.signUp` is controlled by `TG_REGISTRATION`.
`closed` (the default) rejects new sign-ups. `invite` requires an
operator-issued registration invite. `open` allows a username without an
invite. Existing accounts can sign in in every mode.

Before a client asks a user to register, it can call `help.getAppConfig` and
read `registration_mode` to learn whether sign-up is closed, invite-only, or
open.

### 5b. New account flow

When registration is enabled, a Teagram client collects a username, display
name, and password. In invite mode, the user also provides the registration
invite secret. The username handshake returns `authorizationSignUpRequired` for
a new account; the client carries the invite secret in the schema's `phone_code`
field on `auth.signIn`, then creates the account through `auth.signUp`. Open
mode does not need that secret. The client sets the password with
`account.updatePasswordSettings`. The account remains provisional until
password setup succeeds, and it cannot use normal RPCs before then.

For a fresh database, temporarily set `TG_REGISTRATION=open`, complete the
first account sign-up and password setup, then close registration again. The
first account committed by the admission transaction becomes the durable server
administrator. For later invited accounts, issue an invite with
`telegramd invite issue <username>`; the command prints the secret once.
Deliver it to the user through a private channel. `telegramd invite list`
and `telegramd invite revoke <id>` manage outstanding invites.

### 5c. Promote an existing operator after the administration migration

When applying the server-administration migration to a non-empty deployment,
the existing account remains unchanged and the durable administrator grant is
left unassigned. Run this one-shot local maintenance command with the same
database and encryption-key environment as the deployment:

```bash
./telegramd maintenance assign-operator
```

The command takes no user ID. It succeeds only when the database contains
exactly one canonical username-mode `operator` account with one readable
verifier and a closed, unassigned administration singleton. A retry after a
successful assignment is a no-op; account, username, and verifier data are
never created or changed.

### 5d. Reset an existing username password from the local CLI

`telegramd admin set-password` is a local operator command. It has no RPC or
admin-dashboard route. Run it inside the deployment container with a protected
file on stdin; it refuses terminal input, password flags, empty input, and
multi-line input. The command accepts one line and removes one trailing LF or
CRLF. It does not validate registration policy or start server listeners, so it
also works when `TG_REGISTRATION=open`.

On the LXC, create a root-only directory and credential file, then pass the file
to the container through stdin:

```bash
install -d -o root -g root -m 0700 /root/telegram-password-reset
umask 077
openssl rand -base64 24 > /root/telegram-password-reset/tester1.password
chmod 0600 /root/telegram-password-reset/tester1.password
docker compose exec -T telegramd telegramd admin set-password --username tester1 \
  < /root/telegram-password-reset/tester1.password
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
