# Threat model: public WebSocket listener

## Scope and trust boundary

This model covers the public MTProto WebSocket listener at
`server.teagram.io`. Production sends public WebSocket traffic through the
Cloudflare Tunnel connector to the in-cluster `teagramd` Service on port 2446.
The Service is `ClusterIP`, and the NetworkPolicy permits that listener only
from `cloudflared` pods. See the [k3s deployment guide](../deploy/k8s/README.md)
for the production ingress topology. Treat the hostname as internet-facing:
any caller that can reach it can fetch public discovery and open a WebSocket,
including a script or native client that does not use the Teagram web app. The
listener is separate from the default MTProto TCP listener and the tailnet
WebSocket listener.

The in-cluster Service and NetworkPolicy prevent direct external access to the
pod listener. The cluster, Cloudflare zone and tunnel administrators, and
tailnet membership are trusted boundaries. This deployment does not add a
Cloudflare Access identity policy, so application authentication must assume
an anonymous caller.

The protected assets are account and session integrity, registration invites,
authentication state, and availability of the public and tailnet services.
Neither the admin HTTP listener nor the tailnet TCP listener is part of the
public route. The listener split separates ingress and registration policy; it
does not create separate account or data stores. Public and tailnet accounts
belong to the same Teagram service. Public WebSocket per-client abuse budgets
are scoped to this listener and separate from the default TCP and tailnet
services.

## Authentication controls

| Surface | Who can reach it | Control and limitation |
|---|---|---|
| `auth.sendCode` | Any caller that opens the public WebSocket. | Starts the username authentication flow and creates code state; it does not create an account or authorize a session. The default limits are 10 calls per observed client network per hour and 20 distinct usernames per network per day. The public listener's counters are independent from the default TCP and tailnet WebSocket counters, and are charged before the username is looked up. These are throttles, not proof of identity. |
| `auth.signIn` and `auth.checkPassword` | Any caller can submit the RPCs, but password verification requires the pending state and a valid SRP proof. | Existing code-hash checks and failed-password limits still apply: five failed `auth.checkPassword` attempts per account per 10 minutes and ten per observed client network per hour by default. The IP counter is listener-scoped; the account counter remains shared. These controls limit guessing but do not hide the service from anonymous callers. |
| `auth.signUp` | Any caller can submit it; the public registration policy decides whether admission succeeds. | `TG_PUBLIC_REGISTRATION=invite` requires a live operator-issued invite bound to the requested username. The server consumes it in the same transaction that creates the account. A missing, invalid, expired, revoked, or already-used invite cannot admit an account. The public policy is independent of `TG_REGISTRATION=open` on the existing listener. The default sign-up limit is five attempts per observed client network per hour, with independent public and tailnet budgets. |
| `account.checkUsername` | Any unauthenticated caller on this listener. | Returns `boolTrue` or `boolFalse`, so a caller can test whether a chosen valid handle is occupied. The default is 20 checks per observed network per hour, charged before username validation. This is four times the five-per-hour sign-up call budget (when sign-up is active) to allow candidate corrections while leaving account creation on its lower cap. Registration mode does not gate this check. It limits broad single-source scans, not targeted checks or distributed sources; it is a throttle, not an account-privacy guarantee. |
| WebSocket `Origin` | Browser handshakes to `/apiws`. | `TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io` rejects a browser handshake carrying a different Origin. By default, requests without Origin remain accepted; `TG_PUBLIC_WEBSOCKET_BROWSER_ONLY=true` rejects those too and marks the 403 with `Teagram-Client-Policy: browser-only`. This is a cross-origin browser control, not authentication: a non-browser client can set or omit the header. Discovery is public metadata and is served separately from the WebSocket Origin check. |

The public listener intentionally leaves sign-in available while restricting
new account creation. Invite enforcement does not protect `auth.sendCode` or
password attempts for existing accounts. `account.checkUsername` is an
unauthenticated occupancy oracle: it can confirm that a known handle is
registered and can be used to probe candidate lists. It returns no profile
fields, but the endpoint is not an account-privacy boundary. The username flow
also returns different later `auth.signIn` states for an existing account and
a new username.

## Abuse limits and residual risk

The production overlay enables `TG_PUBLIC_WEBSOCKET_LISTEN_ADDR` and sets
`TG_CLIENT_ADDR_TRUST=socket`. The listener therefore uses the immediate
socket peer for RPC and pre-auth per-network limits; it does not use
`CF-Connecting-IP` or `X-Forwarded-For`. With the current Cloudflare Tunnel
path, that peer is a `cloudflared` pod address, so callers behind the same
connector peer share a network bucket. The NetworkPolicy allows port 2446 only
from `cloudflared` pods; the Cloudflare zone, tunnel administrators, and cluster
administrators remain trusted boundaries.

The default TCP listener and tailnet WebSocket listener continue to use their
socket peers. Request headers cannot choose or merge their listener scope. A
different deployment that trusts an L4 proxy must configure `proxy-v2` with its
explicit balancer allowlist.

The 20-per-hour `account.checkUsername` budget uses the observed socket peer
for public WebSocket requests. Its key is the listener-scoped IPv4 `/32` or
IPv6 `/64` bucket, shared across unauthenticated auth keys; invalid and
reserved-name probes consume it because charging precedes validation. Under
the current production trust mode this identifies the connector peer, not the
end-user address. The four-to-one ratio against
`auth.signUp` is correction headroom, not a measured anonymity threshold. The
limit slows broad probing from one network, but a small targeted list fits
within a window and multiple networks can multiply it. Deployments relying on
this throttle must keep the configured limit enabled and verify that
`TG_CLIENT_ADDR_TRUST` reflects the intended network boundary.

Unauthenticated callers can still consume connection and shared service
capacity; per-RPC authentication limits do not by themselves prevent a
connection-level denial of service. The public Origin check does not stop
direct clients, and the invite rule does not protect sign-in attempts for
existing accounts. This listener change does not enable or change Turnstile.
Keep `TG_LOG_LOGIN_CODES=false`; enabling it writes transient authentication
codes to logs. Rate limits, invites, and Origin checks reduce specific abuse
paths but do not make the public listener private or provide a general bot
defense.

The tailnet TCP and optional tailnet WebSocket paths remain on separate
listeners and the Tailscale proxy route. Production sets
`TG_REGISTRATION=open` for those listeners and
`TG_PUBLIC_REGISTRATION=invite` for public WebSocket sign-up. Verify the
ClusterIP and NetworkPolicy ingress restrictions, public sign-up without an
invite is refused, public budgets still limit traffic, and tailnet
authentication still works through TCP and WebSocket.

The isolated `demo.teagram.io` deployment has separate ingress and data
stores. Its online deployment dumps can retain live demo account and message
data until the weekly wipe; see the [demo stack data-retention threat
model](../deploy/teagram-demo/README.md#stack-data-retention-threat-model) for
backup access, retention and deletion controls.
