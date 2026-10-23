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
belong to the same Teagram service.

## Authentication controls

| Surface | Who can reach it | Control and limitation |
|---|---|---|
| `auth.sendCode` | Any caller that opens the public WebSocket. | Starts the username authentication flow and creates code state; it does not create an account or authorize a session. The default limits are 10 calls per observed client network per hour and 20 distinct usernames per network per day. The limit is charged before the username is looked up. These are throttles, not proof of identity. |
| `auth.signIn` and `auth.checkPassword` | Any caller can submit the RPCs, but password verification requires the pending state and a valid SRP proof. | Existing code-hash checks and failed-password limits still apply: five failed `auth.checkPassword` attempts per account per 10 minutes and ten per observed client network per hour by default. These controls limit guessing but do not hide the service from anonymous callers. |
| `auth.signUp` | Any caller can submit it; the public registration policy decides whether admission succeeds. | `TG_PUBLIC_REGISTRATION=invite` requires a live operator-issued invite bound to the requested username. The server consumes it in the same transaction that creates the account. A missing, invalid, expired, revoked, or already-used invite cannot admit an account. The public policy is independent of `TG_REGISTRATION=open` on the existing listener. The default sign-up limit is five attempts per observed client network per hour. |
| WebSocket `Origin` | Browser handshakes to `/apiws`. | `TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io` rejects a browser handshake carrying a different Origin. It is a cross-origin browser control, not authentication: requests without Origin remain accepted, and a non-browser client can set or omit the header. Discovery is public metadata and is served separately from the WebSocket Origin check. |

The public listener intentionally leaves sign-in available while restricting
new account creation. Invite enforcement does not protect `auth.sendCode` or
password attempts for existing accounts. The username flow also returns
different later `auth.signIn` states for an existing account and a new
username, so this endpoint should not be treated as an account-privacy
boundary.

## Abuse limits and residual risk

The production Compose configuration uses `TG_CLIENT_ADDR_TRUST=socket`. The
authentication counters therefore use the address of the socket peer, not a
forwarded HTTP header. With a host-local tunnel connection, public visitors
may share one observed network bucket. This caps aggregate RPC traffic but is
not a per-visitor limit and may also throttle legitimate users together. Do
not trust `X-Forwarded-For` to change this; any client-address proxy mode
requires an explicitly allowlisted, trusted proxy that supplies the supported
PROXY-v2 metadata.

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
listeners and network routes. `TG_REGISTRATION=open` continues to apply only
there; public sign-up uses the separate invite policy. Deployment verification
must confirm the host publish remains loopback-only, the tunnel routes only
the two documented paths, public sign-up without an invite is refused, and
tailnet authentication still works through its existing listener.
