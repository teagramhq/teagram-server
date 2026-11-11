# teagramd and invite landing configuration

This reference describes environment variables used by the `teagramd` server and its invite landing commands. Defaults, accepted values, validation and purpose reflect the code at this revision.

## Server configuration

Rate-limit counts use integer syntax and windows use Go duration syntax. A zero count disables that limit; the runtime enforces a limit only when both its count and window are positive. Startup checks ranges and windows for the surfaces that validate them. Other surfaces retain parsed signed integers and durations, and values that are not both positive do not enable enforcement.

| Variable            | Default          | Notes                                      |
|---------------------|------------------|---------------------------------------------|
| `TG_LISTEN_ADDR`    | `:2443`          | `host:port` (or `:port`) the server binds   |
| `TG_LISTEN_PATH` | `limited` | Assigns the raw TCP listener policy: `limited` applies caller-keyed rate and connection limits; `tailnet` exempts that listener from those limits. The policy is chosen by the listener and cannot be enabled by a request header or PROXY data |
| `TG_WEBSOCKET_LISTEN_ADDR` | *(unset)* | Enables the WebSocket MTProto listener on this address; browser clients connect to `/apiws`, and the listener serves `/.well-known/teagramd/client` |
| `TG_WEBSOCKET_LISTEN_PATH` | `limited` | Assigns the default WebSocket listener policy: `limited` or `tailnet`. Only use `tailnet` when the listener is reachable exclusively through the trusted tailnet |
| `TG_PUBLIC_WEBSOCKET_LISTEN_ADDR` | *(unset)* | Enables the separate WebSocket MTProto listener for public traffic; use an address such as `:2446` and expose it only through the intended public proxy |
| `TG_PUBLIC_WEBSOCKET_LISTEN_PATH` | `limited` | The public WebSocket listener always applies caller-keyed limits; startup rejects `tailnet` |
| `TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR` | *(unset)* | Optional `host:port` used only for the public listener's discovery `websocket.endpoint`; its host becomes `wss://host/apiws`, while MTProto and `client-config` keep `TG_ADVERTISE_ADDR`. Requires `TG_PUBLIC_WEBSOCKET_LISTEN_ADDR`; malformed values fail server startup |
| `TG_RPC_DEADLINE` | `23s` | Per-request timeout; overrides must be positive and no greater than `45s`. `0s` and values above `45s`, including the former `90s` setting, fail startup so admitted requests fit the fixed shutdown drain |
| `TG_WEBSOCKET_ALLOWED_ORIGINS` | *(unset)* | Comma-separated browser origins allowed to connect to `/apiws`; unset rejects every request carrying an `Origin` header |
| `TG_WEBSOCKET_BROWSER_ONLY` | `false` | When `true`, the default WebSocket listener rejects requests without an allowed `Origin`, including native clients, with HTTP 403 and `Teagram-Client-Policy: browser-only`; requires `TG_WEBSOCKET_LISTEN_ADDR`; its discovery document also sets `websocket.browser_only: true`; invalid values fail startup |
| `TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS` | *(unset)* | Comma-separated origins allowed on the public WebSocket listener; set `https://web.teagram.io` for the public web client, and add the demo origin only when it is deployed |
| `TG_PUBLIC_WEBSOCKET_BROWSER_ONLY` | `false` | When `true`, the public WebSocket listener rejects requests without an allowed `Origin`, including native clients, with HTTP 403 and `Teagram-Client-Policy: browser-only`; requires `TG_PUBLIC_WEBSOCKET_LISTEN_ADDR`; its discovery document also sets `websocket.browser_only: true`; invalid values fail startup |
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
| `TEAGRAM_MEDIA_KEY_SOURCE_DIR` | `/etc/teagramd/media-keys` | Host directory bind-mounted read-only into the default Compose server; provision outside the repository |
| `TG_UPLOADS_DISABLED` | `false` | Set to `true` to reject client RPCs that upload file bytes with the stable `UPLOADS_DISABLED` error; existing media downloads continue to work, and the demo's private seed overlay enables its API only while public ingress is stopped |
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
| `TG_RATE_LIMIT_CHANNEL_UNREAD_COUNT` | `60` | Per-account unread summary aggregations, including first-page `messages.getDialogs`, per window; `0` disables the limit |
| `TG_RATE_LIMIT_CHANNEL_UNREAD_COUNT_WINDOW` | `1m` | Unread summary limit window; must be positive when the limit is enabled |
| `TG_RATE_LIMIT_CREATE_STICKER_SET` | `3` | Per-account `stickers.createStickerSet` calls in one fixed window, charged before validation so rejected requests count. `0` disables this bound; a negative or non-integer value fails startup |
| `TG_RATE_LIMIT_CREATE_STICKER_SET_WINDOW` | `1m` | Window for the per-account sticker-set creation bound. It must be positive while that bound is enabled; an invalid or negative duration fails startup |
| `TG_CLIENT_ADDR_TRUST`| `socket`       | Where the address a per-IP limit is keyed on comes from: `socket`, `proxy-v2`, or `cf-connecting-ip`. Any other value fails startup by name. `socket` is the connection's own peer address and assumes one peer address is one client, which fails from either end: behind a proxy or an L4 load balancer every peer address is the balancer's, so one bucket holds every client and the per-IP cap becomes a global one; behind a carrier NAT one address covers thousands of mobile subscribers, who then spend each other's budget. The server warns about socket-keyed listeners while any per-IP limit is on. `proxy-v2` takes the address from a PROXY protocol v2 header and is what to run behind an L4 load balancer; it needs `TG_CLIENT_ADDR_PROXY_CIDRS`. `cf-connecting-ip` reads one IP from `CF-Connecting-IP` on the public WebSocket listener only; other listeners stay socket-keyed. The root Compose file selects it only when `TG_PUBLIC_WEBSOCKET_LISTEN_ADDR` is enabled, and the host must publish that listener only on loopback for the Cloudflare Tunnel. Missing, duplicate, malformed, or comma-separated header values are refused. `TG_CLIENT_ADDR_PROXY_CIDRS` is only valid with `proxy-v2` |
| `TG_MAX_PREAUTH_CONNS`| `1024`       | Cluster-wide concurrent connections that have not authenticated yet. `TG_REPLICA_COUNT` divides this into per-replica accept-loop caps, so sockets past a share are closed before they cost a goroutine, a deadline or a read. `0` disables it; a negative or non-integer value fails startup |
| `TG_MAX_PREAUTH_CONNS_PER_IP`| `64`    | The same, per client network, divided by `TG_REPLICA_COUNT` and enforced in each replica's memory. It uses the same network key as per-IP rate limits: an IPv4 address or an IPv6 **/64**, since a host on a routed v6 allocation can mint addresses inside its /64 for free, and the source named by `TG_CLIENT_ADDR_TRUST`. In `proxy-v2` mode this is the balancer-reported address; in `cf-connecting-ip` mode public WebSocket connections are charged after the HTTP header is validated. A connection carrying no address (for example, a `LOCAL` health check or a socket peer the transport could not report) is charged to no network bucket and stays bounded by the global and lifetime limits. This is a concurrency cap, not a rate. `0` disables it; a negative or non-integer value fails startup |
| `TG_KEY_EXCHANGE_PER_CONNECTION` | `2` | Maximum anonymous key exchanges on one connection. The count survives the pre-auth slot clearing after the first MAC-valid frame. `0` disables it; a negative or non-integer value fails startup |
| `TG_RATE_LIMIT_KEY_EXCHANGE_IP` | `20` | Fixed-window anonymous key exchanges per client network, enforced in Postgres and shared by replicas. Uses the same IPv4-address / IPv6-**/64** key and trusted client address as the other per-IP limits; public WebSocket and TCP/tailnet listeners keep separate counters. If explicitly set, this value applies to both listener families unless the tailnet setting below overrides it. A connection with no keyable address skips only this layer. Store errors refuse the exchange. `0` disables it; a negative or non-integer value fails startup |
| `TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET` | `20` | Tailnet-only per-network key-exchange limit for TCP and tailnet WebSocket listeners. An explicit value overrides `TG_RATE_LIMIT_KEY_EXCHANGE_IP`; `0` disables it. A negative or non-integer value fails startup |
| `TG_RATE_LIMIT_KEY_EXCHANGE_IP_WINDOW` | `1h` | Fixed window for the public and tailnet per-network key-exchange budgets. `0` disables those layers; a negative or invalid duration fails startup |
| `TG_KEY_EXCHANGE_GLOBAL_RATE` | `5` | Deployment-wide in-memory key-exchange ceiling in attempts per second. `TG_REPLICA_COUNT` divides it across replicas. `0` disables the ceiling; when enabled, a value smaller than the replica count fails startup. A negative or non-integer value also fails startup |
| `TG_KEY_EXCHANGE_GLOBAL_BURST` | `30` | Burst capacity of the deployment-wide in-memory key-exchange ceiling, divided across replicas. `0` disables the ceiling; when enabled, a value smaller than the replica count fails startup. A negative or non-integer value also fails startup |
| `TG_PREAUTH_LIFETIME`| `2m`          | How long a connection may stay unauthenticated, measured from accept. Past it the socket is closed whatever it is sending, which is the only bound that reaches a peer that stays inside every deadline by dripping one small frame per read timeout. It ends at the first frame that decrypts under a key the server issued, so a client completing its password challenge is not cut off. Do not set it below a minute: gotd applies a 60s timeout per read inside key exchange, a shorter ceiling starts cutting handshakes that are merely slow, and the server warns at startup if you do. `0` disables it; a negative or unparseable duration fails startup |
| `TG_MAX_CONNS_PER_UNBOUND_KEY`| `8`  | Concurrent connections one auth key with nobody signed in on it may hold, divided by `TG_REPLICA_COUNT` and enforced in each replica's memory. It covers the population between pre-auth connections and signed-in sessions. A connection is charged only once a frame has decrypted under the key. Past the cap the frame in hand is answered and the socket is then closed. `0` disables it; a negative or non-integer value fails startup |
| `TG_MAX_PENDING_LOGIN_CONNS`| `1024` | Cluster-wide concurrent connections waiting for `auth.checkPassword` after `SESSION_PASSWORD_NEEDED`, held in Postgres leases. A pending connection remains counted in the unbound-key hold and this cap, and receives a single ten-minute absolute read lease (`2 × srp.DefaultTTL`); activity never refreshes it. The SRP challenge still expires after five minutes. Past the cap the connection is closed immediately. `0` disables it; a negative or non-integer value fails startup |
| `TG_CLIENT_ADDR_PROXY_CIDRS` | *(unset)* | Comma-separated addresses or CIDRs of balancers allowed to send PROXY protocol v2 headers. Required and only valid with `TG_CLIENT_ADDR_TRUST=proxy-v2`; an empty list in that mode, or any list in another mode, fails startup. IPv4-mapped entries are matched after unmapping; an entry too short to name an IPv4 network fails startup. Only v2 `PROXY` headers over `AF_INET` or `AF_INET6` with `STREAM` transport are accepted. A listed balancer without a valid header, or any unlisted peer with a header, is dropped. `LOCAL` and `AF_UNSPEC` health checks carry no client address and skip per-network limits, so list only balancer addresses rather than broad networks. |
| `TG_CLIENT_ADDR_CF_PROXY_CIDRS` | unset (legacy trust accepts any peer) | Comma-separated addresses or CIDRs for forwarders trusted to supply `CF-Connecting-IP` on the public WebSocket listener. When unset, any socket peer's header is trusted for legacy compatibility. Set a non-empty list to restrict trust to those forwarders; any other peer's header is ignored and limits use that peer address. A non-empty list is only valid with `TG_CLIENT_ADDR_TRUST=cf-connecting-ip`; otherwise startup rejects it |

## Additional server settings

| Variable | Default | Notes |
|---|---|---|
| `TG_AUTH_KEY_SWEEP_IDLE_WINDOW` | `24h` | Go duration for the idle window before an auth key that was never bound to an account is swept. `0` disables the sweep; when enabled, the value must cover the pending-login lifetime and auth-key touch interval, and must not exceed `TG_AUTH_KEY_SWEEP_MAX_AGE` when that setting is enabled. |
| `TG_AUTH_KEY_SWEEP_MAX_AGE` | `72h` | Go duration for the maximum age of an auth key that was never bound to an account. `0` disables the sweep; when both sweep settings are enabled, this must be at least `TG_AUTH_KEY_SWEEP_IDLE_WINDOW`. |
| `TG_BLOB_SCAN_REPORT_INTERVAL` | `0` | Non-negative Go duration between disk scans and reports of abandoned blob files. The report is off by default; `0` disables it. |
| `TG_BLOB_SCAN_TEMP_MIN_AGE` | `24h` | Positive Go duration: a temporary blob must be at least this old before the scan reports it as abandoned or reclaims its assembled blob. |
| `TG_BOOTSTRAP_USERNAME` | *(unset)* | Deprecated account-bootstrap setting. It must remain unset; any presence, including an empty value, fails configuration loading. The first admitted account becomes the administrator. |
| `TG_BOOTSTRAP_PASSWORD` | *(unset)* | Deprecated account-bootstrap setting. It must remain unset; any presence, including an empty value, fails configuration loading. The first admitted account becomes the administrator. |
| `TG_BOOTSTRAP_PASSWORD_FILE` | *(unset)* | Deprecated account-bootstrap setting. It must remain unset; any presence, including an empty value, fails configuration loading. The first admitted account becomes the administrator. |
| `TG_BOT_API_LISTEN_ADDR` | *(unset)* | Optional Bot API HTTP listener address. Set an explicit loopback or Tailscale IP literal with a port from 1–65535; the address must not overlap another listener. |
| `TG_MAX_FILE_BYTES` | `100 MiB` | Positive integer from 1 byte through 1 TiB. Legacy alias for the maximum size of one uploaded file; `TG_UPLOAD_MAX_BYTES`, if set, overrides it. |
| `TG_MAX_USER_STORAGE_BYTES` | `2 GiB` | Positive integer. Maximum combined size of an account's uploaded files. |
| `TG_MEDIA_ERASURE_MIN_AGE` | `24h` | Positive Go duration. A media file must be at least this old before the report names it or an enabled erasure sweep removes it. |
| `TG_MEDIA_ERASURE_REPORT_INTERVAL` | `0` | Non-negative Go duration between reports of media files eligible for erasure. The report is off by default; `0` disables it. |
| `TG_MEDIA_ERASURE_INTERVAL_MIN` | `0` | Non-negative Go duration forming the lower end of the randomized media-erasure sweep interval. Set this and `TG_MEDIA_ERASURE_INTERVAL_MAX` together; a zero minimum disables the sweep. |
| `TG_MEDIA_ERASURE_INTERVAL_MAX` | `0` | Non-negative Go duration forming the upper end of the randomized media-erasure sweep interval. Set this and `TG_MEDIA_ERASURE_INTERVAL_MIN` together; the maximum must exceed the minimum. |
| `TG_MEDIA_ERASURE_DESTRUCTIVE` | `false` | Go boolean. When `true`, the enabled media-erasure sweep removes eligible blobs; it requires both interval bounds. When unset or false, the sweep does not delete files. |
| `TG_PROFILE_DELETE_OP_RETENTION` | `2328h` | Go duration at least 2328 hours (97 days: the 90-day restore horizon plus 7 days of backup cleanup grace). Retention promise for local profile-delete operation deduplication records. |
| `TG_PROFILE_GALLERY_RETENTION` | `2328h` | Go duration at least 2328 hours (97 days: the 90-day restore horizon plus 7 days of backup cleanup grace). Retention promise for identifier-only profile-gallery mutation records. |
| `TG_PROFILE_RECEIPT_RETENTION` | `max(2328h, gallery retention, upload-part TTL + RPC deadline)` | Go duration at least 2328 hours (97 days) and long enough to outlive both profile-gallery retention and the upload-part TTL plus RPC deadline. Retention promise for terminal upload receipts. |
| `TG_RATE_LIMIT_ADD_CHAT_USER` | `120` | Integer count per account per window for adding chat members. `0` disables this bound. |
| `TG_RATE_LIMIT_ADD_CHAT_USER_WINDOW` | `24h` | Go duration for the per-account chat-member-add bound. |
| `TG_RATE_LIMIT_CHECK_PASSWORD` | `5` | Integer failed `auth.checkPassword` attempts per account per window. `0` disables this bound. |
| `TG_RATE_LIMIT_CHECK_PASSWORD_WINDOW` | `10m` | Go duration for the per-account failed `auth.checkPassword` bound. |
| `TG_RATE_LIMIT_CREATE_CHANNEL` | `20` | Integer channel-creation calls per account per window. `0` disables this bound. |
| `TG_RATE_LIMIT_CREATE_CHANNEL_WINDOW` | `24h` | Go duration for the per-account channel-creation bound. |
| `TG_RATE_LIMIT_CREATE_CHAT` | `20` | Integer chat-creation calls per account per window. `0` disables this bound. |
| `TG_RATE_LIMIT_CREATE_CHAT_WINDOW` | `24h` | Go duration for the per-account chat-creation bound. |
| `TG_RATE_LIMIT_DISCARD_ENCRYPTION` | `30` | Non-negative integer secret-chat discard calls per account per window. `0` disables this bound; a positive limit requires a positive window. |
| `TG_RATE_LIMIT_DISCARD_ENCRYPTION_WINDOW` | `1h` | Go duration for the per-account secret-chat discard bound. |
| `TG_RATE_LIMIT_GET_PASSWORD` | `20` | Integer authorized `account.getPassword` calls per account per window. `0` disables this bound; unauthenticated calls use the separate `TG_RATE_LIMIT_GET_PASSWORD_IP` limit. |
| `TG_RATE_LIMIT_GET_PASSWORD_WINDOW` | `1h` | Go duration for the per-account authorized `account.getPassword` bound. |
| `TG_RATE_LIMIT_PASSWORD_PROOF` | `5` | Integer password-proof attempts per account per window, shared by `account.getPasswordSettings` and `account.updatePasswordSettings`. `0` disables this bound. |
| `TG_RATE_LIMIT_PASSWORD_PROOF_WINDOW` | `10m` | Go duration for the per-account password-proof bound. |
| `TG_RATE_LIMIT_POLL_VOTE` | `60` | Integer poll-vote calls per account per window. `0` disables this bound. |
| `TG_RATE_LIMIT_POLL_VOTE_WINDOW` | `1m` | Go duration for the per-account poll-vote bound. |
| `TG_RATE_LIMIT_REQUEST_ENCRYPTION` | `10` | Integer secret-chat request calls per account per window. `0` disables this bound; negative values fail startup, and a positive limit requires a positive window. |
| `TG_RATE_LIMIT_REQUEST_ENCRYPTION_WINDOW` | `1h` | Positive Go duration for the per-account secret-chat request bound while it is enabled. |
| `TG_RATE_LIMIT_SAVE_FILE_PART` | `600` | Integer file-part upload calls per account per window. `0` disables this bound. |
| `TG_RATE_LIMIT_SAVE_FILE_PART_WINDOW` | `1m` | Go duration for the per-account file-part upload bound. |
| `TG_RATE_LIMIT_SEARCH_CONTACTS` | `300` | Integer `contacts.search` calls per account per window. `0` disables this bound. |
| `TG_RATE_LIMIT_SEARCH_CONTACTS_WINDOW` | `1h` | Go duration for the per-account contacts-search bound. |
| `TG_RATE_LIMIT_SEARCH_GLOBAL` | `300` | Integer global message-search calls per account per window. `0` disables this bound. |
| `TG_RATE_LIMIT_SEARCH_GLOBAL_WINDOW` | `1h` | Go duration for the per-account global-search bound. |
| `TG_RATE_LIMIT_SEARCH_MESSAGES` | `300` | Integer message-search calls per account per window. `0` disables this bound. |
| `TG_RATE_LIMIT_SEARCH_MESSAGES_WINDOW` | `1h` | Go duration for the per-account message-search bound. |
| `TG_RATE_LIMIT_SEND` | `60` | Integer message-send calls per account per window. `0` disables this bound. |
| `TG_RATE_LIMIT_SEND_WINDOW` | `1m` | Go duration for the per-account message-send bound. |
| `TG_RATE_LIMIT_UPDATE_PROFILE` | `20` | Integer profile-update calls per account per window. `0` disables this bound. |
| `TG_RATE_LIMIT_UPDATE_PROFILE_WINDOW` | `24h` | Go duration for the per-account profile-update bound. |
| `TG_RATE_LIMIT_UPLOAD_BYTES` | `0` (disabled) | Optional non-negative integer byte budget per account per window for uploaded file data. Set a positive value to enable it; a positive limit requires a positive window. |
| `TG_RATE_LIMIT_UPLOAD_BYTES_WINDOW` | `1h` | Go duration for the per-account uploaded-byte budget. |
| `TG_STICKER_CATALOG_BUNDLE` | *(unset)* | Path to a prebuilt sticker catalog bundle loaded at startup. An invalid or unreadable bundle fails startup. |
| `TG_STICKER_IMPORT` | `on` | `on` or `off` (unset means `on`). Controls whether accounts can create or add to sticker sets. |
| `TG_UPLOAD_MAX_BYTES` | `100 MiB` | Positive integer from 1 byte through 1 TiB. Maximum size of one uploaded file; overrides `TG_MAX_FILE_BYTES` when both are set. |
| `TG_UPLOAD_PART_TTL` | `6h` | Positive Go duration before an unassembled upload part is deleted. |

## Discovery aliases

The `TG_DISCOVERY_*` spellings are alternatives to the corresponding `TG_RATE_LIMIT_DISCOVERY*` variables. Do not set both spellings for the same limit or window; startup rejects that combination.

| Variable | Default | Notes |
|---|---|---|
| `TG_DISCOVERY_RATE_LIMIT` | *(unset)* | Integer alias for `TG_RATE_LIMIT_DISCOVERY`, the cluster-wide valid local-direct preflight response limit. `0` disables this bound. |
| `TG_DISCOVERY_RATE_LIMIT_WINDOW` | *(unset)* | Go duration alias for `TG_RATE_LIMIT_DISCOVERY_WINDOW`, the cluster-wide preflight window. |
| `TG_DISCOVERY_RATE_LIMIT_PER_IP` | *(unset)* | Integer alias for `TG_RATE_LIMIT_DISCOVERY_IP`, the per-network local-direct preflight response limit. `0` disables this bound. |
| `TG_DISCOVERY_RATE_LIMIT_PER_IP_WINDOW` | *(unset)* | Go duration alias for `TG_RATE_LIMIT_DISCOVERY_IP_WINDOW`, the per-network preflight window. |

## Invite landing URLs

`cmd/linklanding` and `cmd/linkselector` read these settings through `internal/linklanding`; `teagramd` does not read them.

| Variable | Default | Notes |
|---|---|---|
| `TG_INVITE_WEB_SIGNUP_URL` | *(required)* | Absolute HTTPS URL used as the web destination for invite landing pages. It must not include credentials or a fragment. |
| `TG_INVITE_DESKTOP_URL` | *(unset)* | Optional desktop destination for invite landing pages. Accepts an absolute HTTPS or `teagram` URL without credentials or a fragment. |

## Compose media-key mount

`TEAGRAM_MEDIA_KEY_SOURCE_DIR` defaults to `/etc/teagramd/media-keys`. Docker Compose bind-mounts this host directory read-only for the server; provision the directory outside the repository.
