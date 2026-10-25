# Threat model: public WebSocket listener

## Scope and trust boundary

This model covers the public MTProto WebSocket listener at
`server.teagram.io`. The ingress contract sends only `/apiws` and
`/.well-known/telegramd/client` through the host-side Cloudflare Tunnel to
`127.0.0.1:2446`; Compose publishes that listener on LXC loopback. Treat the
hostname as internet-facing: any caller that can reach it can fetch public
discovery and open a WebSocket, including a script or native client that does
not use the Teagram web app. The listener is separate from the default MTProto
TCP listener and the existing tailnet WebSocket listener.

The host loopback bind prevents direct connections from other network hosts.
It does not isolate the listener from host processes, the Docker daemon, or
containers attached to the Compose network. The tunnel, host, Docker daemon,
Cloudflare zone and tunnel administrators, and tailnet membership are trusted
boundaries. This rollout does not add a Cloudflare Access identity policy, so
application authentication must assume an anonymous caller.

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
| WebSocket `Origin` | Browser handshakes to `/apiws`. | `TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io` rejects a browser handshake carrying a different Origin. It is a cross-origin browser control, not authentication: requests without Origin remain accepted, and a non-browser client can set or omit the header. Discovery is public metadata and is served separately from the WebSocket Origin check. |

The public listener intentionally leaves sign-in available while restricting
new account creation. Invite enforcement does not protect `auth.sendCode` or
password attempts for existing accounts. The username flow also returns
different later `auth.signIn` states for an existing account and a new
username, so this endpoint should not be treated as an account-privacy
boundary.

## Abuse limits and residual risk

The production Compose configuration keeps `TG_CLIENT_ADDR_TRUST=socket` for
TCP and tailnet listeners. The public WebSocket listener may use
`CF-Connecting-IP` only when `TG_PUBLIC_WEBSOCKET_CF_CONNECTING_IP_TRUSTED_PEERS`
names the exact tunnel socket peer address. A header from every other peer is
ignored. A missing, repeated, or invalid value uses the tunnel peer's shared,
fail-closed bucket. Public and tailnet budgets remain separate. Do not trust
`X-Forwarded-For`; add or change the live trusted-peer value through the rollout
runner's approved environment path.

Unauthenticated callers can still consume connection and shared service
capacity; per-RPC authentication limits do not by themselves prevent a
connection-level denial of service. The public Origin check does not stop
direct clients, and the invite rule does not protect sign-in attempts for
existing accounts. This listener change does not enable or change Turnstile.
Keep `TG_LOG_LOGIN_CODES=false`; enabling it writes transient authentication
codes to logs. Rate limits, invites, and Origin checks reduce specific abuse
paths but do not make the public listener private or provide a general bot
defense.

The tailnet TCP and optional tailnet WebSocket paths remain on their existing
listeners and network routes and share their original per-client counters.
Existing rows remain assigned to that tailnet scope; the additive migration
creates separate public counters without resetting or copying them.
`TG_REGISTRATION=open` continues to apply only there; public sign-up uses the
separate invite policy. Deployment verification must confirm the host publish
remains loopback-only, the tunnel routes only the two documented paths, public
sign-up without an invite is refused, public budgets still limit traffic, and
tailnet authentication still works through TCP and WebSocket.

The isolated `demo.teagram.io` deployment has separate ingress and data
stores. Its online deployment dumps can retain live demo account and message
data until the weekly wipe; see the [demo stack data-retention threat
model](../deploy/teagram-demo/README.md#stack-data-retention-threat-model) for
backup access, retention and deletion controls.
