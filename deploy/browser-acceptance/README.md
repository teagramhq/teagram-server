# Browser acceptance CONNECT observer

This directory contains an importable, dependency-free Node observer. Importing
it creates no listener and starts no service. The later runtime integration
owns startup and must verify the deployed artifact manifest digest before it
passes the manifest's `host` to `createConnectObserver`. The observer checks
that value against the single compiled pin,
`telegram-server.tailaa4918.ts.net`; manifest data cannot add an allowed host.

The proxy accepts only HTTP `CONNECT` for that host on port 443, with one
matching `Host` header. It resolves DNS only after those checks, requires every
answer to be in `100.64.0.0/10` or `fd7a:115c:a1e0::/48`, and opens the upstream
socket to the validated IP address. It does not resolve the name again. Other
CONNECT authorities are refused without DNS or an upstream socket. Other HTTP
methods, including absolute-form `GET`, receive 405. Tunnel bytes are piped
without TLS interception or inspection. `Proxy-Authorization` is ignored.

The server caps request headers at 8 KiB. Header parsing has a two-second
deadline, Node's request timeout is five seconds, and DNS and upstream
connection attempts each have a two-second timeout. Responses and retained
counts use fixed fields and values; request authorities, headers, payloads,
and error details are never written to stdout or stderr.

## Health and counts

The module also exposes a separate `controlServer`. It does not listen until
the runtime integration starts it. The integration must bind it only on the
private runtime network and must not publish its port. `GET /healthz` returns
the fixed JSON snapshot with HTTP 200 when the observer is healthy and 503
when it is unhealthy. Other GET paths return an empty 404 and non-GET methods
return an empty 405. This listener is separate from the CONNECT proxy, so
health requests cannot become proxy traffic or change proxy counts. The runtime
may instead read `observer.snapshot()` in the owning process.

The fixed snapshot contains `status`, `allowed_host`, and these non-negative
integer counters:

- `allowed_connects`: established CONNECT tunnels.
- `blocked_requests`: requests rejected by policy or failed before a tunnel
  was established.
- `telegram_attempts`: blocked requests whose authority has a DNS-label suffix
  of `telegram.org`, `t.me`, `telegram.me`, or `telesco.pe`.
- `other_blocked_count`: blocked requests outside those official suffixes.
- `dns_lookups`: lookups attempted for the pinned host after authority checks.
- `upstream_connects`: upstream socket attempts after address validation.
- `upstream_failures`: DNS, address-policy, or upstream connection failures.

`status: healthy` means the proxy listener is active and neither listener has
reported an internal server error. It does not mean a readiness check passed.
The consumer must call `isReadySnapshot(snapshot)` and treat a missing field,
extra field, wrong type, unhealthy status, any blocked request, or any upstream
failure as non-ready. Readiness also requires at least one established allowed
tunnel and consistent DNS, upstream-attempt, and successful-tunnel counts. This
observer contract does not include the browser's HTTP or WSS checks.

`resolveHost` and `openUpstream` are narrow test seams used by the unit tests.
Runtime code should use the defaults and must not provide alternate resolver
or connector functions.

## Checks

```sh
node --test deploy/browser-acceptance/*.test.mjs
node --check deploy/browser-acceptance/connect-observer.mjs
node --check deploy/browser-acceptance/connect-observer.test.mjs
```
