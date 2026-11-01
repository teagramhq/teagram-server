# Threat model: public WebSocket listener

## Scope and trust boundary

This model covers the public MTProto WebSocket listener at
`server.teagram.io`. The ingress contract sends only `/apiws` and
`/.well-known/teagramd/client` through the host-side Cloudflare Tunnel to
`127.0.0.1:2446`; Compose publishes that port for the public HAProxy frontend
on LXC loopback. Treat the
hostname as internet-facing: any caller that can reach it can fetch public
discovery and open a WebSocket, including a script or native client that does
not use the Teagram web app. The listener is separate from the default MTProto
TCP listener and the existing tailnet WebSocket listener.

The host loopback bind prevents direct connections from other network hosts.
The public HAProxy frontend is attached to a fixed internal network with no
other Compose services. Its first connection rule allows only that network's
gateway address, which is the peer Docker presents for the host-side tunnel's
published connection. The smoke test inspects and asserts that observed peer.
Host processes share this gateway address and can therefore forge the header;
the host and Docker daemon are trusted boundaries. Cloudflare's overwritten
`CF-Connecting-IP` value is also trusted. This rollout does not add a
Cloudflare Access identity policy, so application authentication must assume
an anonymous caller.

The protected assets are account and session integrity, registration invites,
authentication state, and availability of the public and tailnet services.
Neither the admin HTTP listener nor the tailnet TCP listener is part of the
public route. The listener split separates ingress and registration policy; it
does not create separate account or data stores. Public and tailnet accounts
belong to the same Teagram service. Public WebSocket per-client abuse budgets
are separate from the budgets shared by the default TCP and tailnet WebSocket
listeners.

## Authentication controls

| Surface | Who can reach it | Control and limitation |
|---|---|---|
| `auth.sendCode` | Any caller that opens the public WebSocket. | Starts the username authentication flow and creates code state; it does not create an account or authorize a session. The default limits are 10 calls per observed client network per hour and 20 distinct usernames per network per day. The public listener's counters are independent from the default TCP and tailnet WebSocket counters, and are charged before the username is looked up. These are throttles, not proof of identity. |
| `auth.signIn` and `auth.checkPassword` | Any caller can submit the RPCs, but password verification requires the pending state and a valid SRP proof. | Existing code-hash checks and failed-password limits still apply: five failed `auth.checkPassword` attempts per account per 10 minutes and ten per observed client network per hour by default. The IP counter is listener-scoped; the account counter remains shared. These controls limit guessing but do not hide the service from anonymous callers. |
| `auth.signUp` | Any caller can submit it; the public registration policy decides whether admission succeeds. | `TG_PUBLIC_REGISTRATION=invite` requires a live operator-issued invite bound to the requested username. The server consumes it in the same transaction that creates the account. A missing, invalid, expired, revoked, or already-used invite cannot admit an account. The public policy is independent of `TG_REGISTRATION=open` on the existing listener. The default sign-up limit is five attempts per observed client network per hour, with independent public and tailnet budgets. |
| `account.checkUsername` | Any unauthenticated caller on this listener. | Returns `boolTrue` or `boolFalse`, so a caller can test whether a chosen valid handle is occupied. The default is 20 checks per observed network per hour, charged before username validation. This is four times the five-per-hour sign-up call budget (when sign-up is active) to allow candidate corrections while leaving account creation on its lower cap. Registration mode does not gate this check. It limits broad single-source scans, not targeted checks or distributed sources; it is a throttle, not an account-privacy guarantee. |
| WebSocket `Origin` | Browser handshakes to `/apiws`. | `TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io` rejects a browser handshake carrying a different Origin. It is a cross-origin browser control, not authentication: requests without Origin remain accepted, and a non-browser client can set or omit the header. Discovery is public metadata and is served separately from the WebSocket Origin check. |

The public listener intentionally leaves sign-in available while restricting
new account creation. Invite enforcement does not protect `auth.sendCode` or
password attempts for existing accounts. `account.checkUsername` is an
unauthenticated occupancy oracle: it can confirm that a known handle is
registered and can be used to probe candidate lists. It returns no profile
fields, but the endpoint is not an account-privacy boundary. The username flow
also returns different later `auth.signIn` states for an existing account and
a new username.

## Abuse limits and residual risk

HAProxy rejects a request unless it has exactly one parseable
`CF-Connecting-IP` value, then sets its source address from that value. Missing,
duplicate, comma-separated or invalid values receive HTTP 400. The route never
reads `X-Forwarded-For`. HAProxy sends PROXY v2 only to the dedicated public
listener. Its backend sets `http-reuse never`, so idle connections are not
shared between sessions; the smoke test sends two upgrades with different
addresses over one keep-alive frontend connection and checks both addresses at
the PROXY-v2 backend.

The two public replicas use `TG_CLIENT_ADDR_TRUST=proxy-v2`, trust only
HAProxy's fixed backend address, and publish no host ports. `clientAddrUntil`
closes peers outside that allowlist on every listener of each public replica.
The public replicas bind the dedicated public listener and retain its invite
registration rule, Origin allowlist, advertise address and Turnstile settings.
The base `teagramd` service remains in `socket` trust and keeps tailnet TCP
2443 and the optional tailnet WebSocket listener. It no longer binds or
publishes the public listener, so traffic cannot fall through to
`TG_REGISTRATION` or the tailnet budget scope.

Per-IP limits remain per network prefix, not per individual address: IPv4 uses
its address and IPv6 uses its network prefix. A caller with many networks can
therefore obtain many buckets; per-account and global limits remain the
backstop. Cloudflare's client address claim and any host process using the
pinned Docker gateway are trusted by design.

The 20-per-hour `account.checkUsername` budget shares this source-address
behavior. With the host-local tunnel, all public visitors may contend for one
bucket; 20 checks per hour is an aggregate ceiling, not a per-person allowance.
The key is the existing listener-scoped IPv4 `/32` or IPv6 `/64` bucket, shared
across unauthenticated auth keys; invalid and reserved-name probes consume it
because charging precedes validation. The four-to-one ratio against
`auth.signUp` is correction headroom, not a measured anonymity threshold. The
limit slows broad probing from one network, but a small targeted list fits
within a window and multiple networks can multiply it. Deployments relying on
this throttle must keep the configured limit enabled and verify that
`TG_CLIENT_ADDR_TRUST` reflects the intended network boundary.

Unauthenticated callers can still consume connection and shared service
capacity; per-RPC authentication limits do not by themselves prevent a
connection-level denial of service. The public Origin check does not stop
direct clients, and the invite rule does not protect sign-in attempts for
existing accounts. Keep `TG_LOG_LOGIN_CODES=false`; enabling it writes transient authentication
codes to logs. Rate limits, invites, and Origin checks reduce specific abuse
paths but do not make the public listener private or provide a general bot
defense.

The tailnet TCP and optional tailnet WebSocket paths remain on their existing
listeners and network routes and share their original per-client counters.
Existing rows remain assigned to that tailnet scope; the additive migration
creates separate public counters without resetting or copying them.
`TG_REGISTRATION=open` continues to apply only there; public sign-up uses the
separate invite policy. Deployment verification checks HAProxy health and
bounded logs, the loopback-only publish, the gateway peer seen on the public
frontend, rejection from the Compose default network, header validation,
per-client PROXY-v2 attribution across a keep-alive connection, public sign-up
without an invite, and tailnet authentication through TCP and WebSocket.

The isolated `demo.teagram.io` deployment has separate ingress and data
stores. Its online deployment dumps can retain live demo account and message
data until the weekly wipe; see the [demo stack data-retention threat
model](../deploy/teagram-demo/README.md#stack-data-retention-threat-model) for
backup access, retention and deletion controls.
