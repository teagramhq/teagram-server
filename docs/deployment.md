# Compose deployment

The default Compose stack publishes `telegramd` directly on `127.0.0.1:2443`
and keeps socket-address trust. This matches the current deployment, where
`docker compose up -d` also loads the live, untracked override that publishes
the tailnet and loopback endpoints on `telegramd`. Keep that override and its
bind addresses unchanged.

The proxy-v2 candidate is an explicit opt-in file, `docker-compose.proxy.yml`.
It is not auto-loaded by normal Compose or deployment commands. Use it only in
a disposable validation environment with explicit `-f docker-compose.yml -f
docker-compose.proxy.yml` arguments; it clears `telegramd`'s host port and
publishes loopback MTProto and WebSocket through HAProxy. Admin remains private
per replica and is not load-balanced through HAProxy.

The opt-in overlay defaults to `172.18.0.0/16` with HAProxy at `172.18.0.10`.
If that subnet overlaps a local route, set `TG_DOCKER_SUBNET` and `TG_PROXY_IP`
in `.env` to a non-overlapping subnet and an unused address inside it. The
exact proxy address is the only non-loopback address trusted for PROXY-v2
headers; do not widen the allowlist to the subnet.

## Readiness and rolling replacement

The `telegramd` container healthcheck performs a local discovery preflight and
validates the nonce and response format. It does not compare a replica's RSA
fingerprint or prove that the existing identity key was mounted. In the opt-in
proxy configuration, HAProxy's server checks send a PROXY-v2 header so the
server consumes the expected protocol, but they only check that the listener
accepts a TCP connection. They do not wait for Docker health, and Docker DNS
can return unhealthy containers. HAProxy's internal-only `/healthz` monitor
means at least one backend passed that TCP check; it does not report Docker or
application readiness.

The two `server-template` slots resolve the Compose `telegramd` service name,
so a second replica can be discovered without publishing another host port. CI
also runs a fresh-nonce discovery preflight from the host through the published
HAProxy port and validates the response, exercising the client-to-proxy-to-
server PROXY-v2 path. The Compose dependency order stops `telegramd` before the
proxy, and the proxy has a 120-second stop grace period. This keeps the proxy
process alive through the server's stop window, but does not itself drain
sessions: the current SIGTERM path closes active sessions. MAIN-1264 defines a
90-second TCP/WebSocket drain and requires `stop_grace_period` of at least 120
seconds; that server behavior remains a separate gate.

Every replica must advertise the same canonical proxy address and use the
existing shared `tgkey` volume and auth-key encryption key. The discovery
probe does not prove those identities match or refuse a missing key;
MAIN-1263 owns the must-exist startup/readiness check. Keep these guarantees
separate from the proxy's liveness/readiness result.

Before rollout, keep the remaining gates explicit: MAIN-1262's reconnect-safe
pending expiry, MAIN-1263's must-exist key and identity readiness, and
MAIN-1248's shared SRP challenge. Later acceptance must prove two clients get
distinct IP-limit buckets, a replica missing `tgkey` refuses readiness, the
public admin route is unreachable, and shared-volume/sweeper behavior is safe.
MAIN-1265/1266 own message/update and shared-blob media end-to-end coverage.
The internal health endpoint must remain private and reveal no identity,
version, backend count, or error details.

The live Docker network is already `172.18.0.0/16`; Postgres and telegramd use
`.2` and `.3`, and the candidate proxy address `.10` is unused. This makes the
default subnet suitable for isolated validation, but does not make the overlay
part of the live deployment. Its fixed proxy address must remain the only
non-loopback address trusted for PROXY-v2. HAProxy passes WebSocket `Host` and
`Origin` unchanged. Replica-local limits can still multiply during scale-out;
track that separately in MAIN-1249.

The LXC does not have the `docker rollout` CLI command installed. Its current
`Deploy telegram-server` procedure remains authoritative: serialize with the
deployment flock, fetch and deploy `origin/main` only, record the Changes
section, verify a database dump before schema changes, and retain the dump and
restore path. A future rollout change must add and validate its scale-aware
command in that procedure; the candidate command is not executable on the
current LXC. Do not use a whole-stack `docker compose up -d` as a substitute
for the current serialized deployment procedure.

HAProxy writes TCP/backend logs to stdout, and Docker health status is visible
with `docker compose ps -a`. The LXC has no external alerting integration, and
this candidate has no traffic metrics or actionable alert yet. Those are
required before the proxy can be treated as an operated production service.

## First port-owner handoff feasibility

The live, untracked Compose override publishes `100.124.236.66:2443` for
tailnet MTProto and `127.0.0.1:2443`, `:2444`, and `:2445` for loopback
MTProto, WebSocket, and admin. The current `telegramd` container owns all of
them through Docker's `docker-proxy` listeners. Keep the override untracked and
preserve its environment entries and exact bind addresses.

The selected first-handoff sequence requires temporary forwarding for new
flows from the canonical endpoints to the proxy's alternate endpoint while
existing flows stay on the legacy listener. Tailnet packets need a prerouting
rule; loopback connections need an output-path rule. Connection tracking must
keep each established flow on the destination selected by its first packet.
After the legacy replica drains, the proxy can take the canonical publications
and the temporary rules can be removed. Reversal needs the same ordering with
new flows sent to the legacy endpoint while the proxy remains available. Do
not stop either listener or flush conntrack entries during that transfer.

The old direct listener must stay in socket-trust mode while it accepts new
direct flows; the proxy-routed replica uses PROXY-v2 trust from the exact proxy
address. A direct path to a proxy-trust replica must be closed. This proxy
overlay sets one trust mode for every `telegramd` container, so it cannot yet
represent both sides of that bootstrap at once. A separate, tested
service/configuration is required before the port transfer. Do not widen the
trusted CIDRs or use `X-Forwarded-For` to bridge the two modes.

Read-only checks in the actual LXC found `ip_forward=1`, `CAP_NET_ADMIN` in the
effective capability mask, and an active conntrack table. However, `nft` is not
installed, and `iptables -t nat -S` fails with `table 'nat' is incompatible,
use 'nft' tool`. The existing published ports are handled by `docker-proxy`.
Therefore this LXC currently has no verified way to install the required
prerouting and output forwarding rules. The handoff and reversal have not been
tested, and the selected design is not yet operable here. No two-replica DNS
discovery or high-port handoff/reversal test was run on this LXC. The effective
network capability, rather than the nominal capability mask or `ip_forward`,
must be verified before an isolated test can begin.

These read-only commands reproduce the LXC capability and port-owner checks;
the NAT command is expected to fail in the current environment:

```sh
ssh telegram-server 'cat /proc/sys/net/ipv4/ip_forward; grep "^CapEff:" /proc/self/status; command -v nft || echo nft-unavailable'
ssh telegram-server 'iptables -t nat -S'
ssh telegram-server 'docker ps --format "{{.Names}} {{.Ports}}"; ps -ef | grep "[d]ocker-proxy"'
ssh telegram-server 'docker network inspect telegram-server_default --format "{{range .Containers}}{{.Name}} {{.IPv4Address}}{{println}}{{end}}"'
ssh telegram-server 'docker rollout --help'
```

The candidate CI job checks both default and opt-in rendered Compose shapes,
parses the HAProxy configuration with its pinned image, and validates the
preflight response through the published proxy port. Those checks do not
exercise live DNS discovery, connection tracking, or the handoff sequence.

The discovery runner has Docker and an engine but no Compose CLI plugin. A
bounded setup prerequisite is Docker Compose v2.36.2 in a disposable discovery
runtime, using its isolated Docker daemon; no change to the live LXC is needed.
With that plugin available, these commands validate the opt-in rendering and
HAProxy syntax without starting the application stack:

```sh
docker compose --env-file .env.example -f docker-compose.yml -f docker-compose.proxy.yml config --format json >/dev/null
docker compose --env-file .env.example -f docker-compose.yml -f docker-compose.proxy.yml run --rm --no-deps tcp-proxy haproxy -c -f /usr/local/etc/haproxy/haproxy.cfg
```

In that disposable runtime, start the opt-in stack and run the host-side
preflight to verify the published HAProxy-to-telegramd path:

```sh
docker compose --env-file .env.example -f docker-compose.yml -f docker-compose.proxy.yml up -d
env -u TG_CLIENT_ADDR_TRUST go run ./cmd/telegramd-healthcheck
```

Safe next step: keep the legacy port owner and defer the first proxy cutover
until the LXC has a working, reversible NAT control surface or a separately
provisioned stable front door. If NAT capability must be changed outside this
LXC, route that host-level work as a separate Steward capability task. A
stop-old/start-proxy transition would create the rejected listener gap and is
not an acceptable substitute.

## Work that can proceed without a live port change

MAIN-1262/1263/1264 server changes, MAIN-1248 SRP persistence, and MAIN-1249
shared limits can be implemented and tested while the current direct listener
and its untracked override stay in place. Proxy health/configuration, security
negative cases, metrics, and alerting can be validated in CI or a disposable
Compose runtime with test-only volumes and ports. Keep production port
ownership, existing volumes, and the override unchanged until the mixed
socket/proxy trust arrangement and reversible forwarding have executable
evidence.

## Rollback and preserved deployment state

Before any future cutover, back up the untracked override and verify a complete
pre-deploy database dump with its restore path recorded. Preserve `pgdata`,
`tgkey`, and `tgblobs`; they hold the database, RSA identity/auth-key encryption
key, and uploaded bodies. Never use `docker compose down -v`. A failed isolated
forwarding test must remove only its test rules and test resources. A production
rollback must keep a listener accepting throughout the reversal, use a binary
compatible with any applied migration, and restore the dump only if the prior
binary cannot run on the migrated schema. No live port cutover is authorized by
this feasibility note.
