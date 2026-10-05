# Isolated browser acceptance runtime

This is a one-shot Compose project named `telegram-browser-acceptance`. It is
separate from the server stack and owns two containers: an internal-only
Chromium runner and the dependency-free CONNECT observer. The browser can reach
only the observer. The observer is dual-homed and permits CONNECT to the one
pinned Tailscale target after validating the deployed artifact metadata.

The runner selects the architecture-specific Playwright image digest in
`run.sh`; the image adds only the integrity-locked `@playwright/test@1.61.1`
and `playwright@1.61.1` packages. The approved QA runner is pinned by its SHA-256
before it can be mounted. Both containers run as UID 1000 with all capabilities
dropped, `no-new-privileges`, a read-only root, no log driver, zero core dumps,
equal memory and memory-plus-swap limits, and bounded CPU and memory. Browser
profile, HOME, `/tmp`, and `/dev/shm` are bounded tmpfs mounts with `noexec`,
`nosuid`, and `nodev`. Chromium binaries and Node modules stay in the image.

The host runner requires curl 8.4.0 or newer so unknown-length responses are
stopped during transfer at 16 KiB plus one byte. Its project lock is held on a
mode-0700 directory under `XDG_RUNTIME_DIR`, or `$HOME/.local/run` when that
variable is unset.

The custom seccomp profile is derived from Docker's default profile vendored by
Moby release `v28.5.2`, commit
`89c5e8fd66634b6128fc4c0e6f1236e2540e46e0`, SHA-256
`01536f1d1df938ae611eba20d6349e0de7a99b6ecdee1549427a0b01b8301e28`. The
profile test reconstructs the change and rejects any difference beyond the
Chromium namespace `clone` rule, `unshare`, and the unconditional `chroot`
allowance. The runtime profile does not add capabilities or disable the
Chromium sandbox.

## CONNECT observer contract

`connect-observer.mjs` has no startup side effects. The runtime integration
starts it through `observer-service.mjs`, which passes the compiled
`ALLOWED_HOST` to `createConnectObserver`; release-manifest data does not
configure the observer's host pin. The compiled pin is
`telegram-server.tailaa4918.ts.net`.

Separately, before any Docker command, `run.sh` validates the supplied release
record and fetches the served `mtproto-target.json` from the pinned HTTPS host.
It checks the source revision, content digest, fixed WSS hostname, and
fingerprint format. Those preflight checks validate release metadata; they do
not set the observer's compiled host pin.

The proxy accepts only CONNECT to that exact host on port 443 with exactly one
matching `Host` header. It resolves DNS only after those authority checks,
requires every answer to be in `100.64.0.0/10` or `fd7a:115c:a1e0::/48`, and
opens the upstream socket to a validated IP without resolving the name again.
Other CONNECT authorities are refused before DNS or socket access. Other HTTP
methods, including absolute-form GET, receive 405. Tunnel bytes pass through
without TLS inspection, and `Proxy-Authorization` is ignored.

Request headers are capped at 8 KiB. Header parsing has a two-second deadline,
the HTTP request timeout is five seconds, and DNS and upstream connection
attempts each have a two-second timeout. The observer never logs request
authorities, headers, payloads, or error details.

The separate `controlServer` listens only when started by the runtime
integration. Bind it to the private runtime network and do not publish its
port. `GET /healthz` returns a fixed snapshot with HTTP 200 when the observer
is healthy and 503 otherwise; other GET paths return 404 and non-GET methods
return 405. Health requests cannot become proxy traffic or change proxy
counters. The runtime may instead read `observer.snapshot()` in the owning
process.

The snapshot schema is exactly nine fields: `status`, `allowed_host`, and the
seven non-negative integer counters below.

- `allowed_connects`: established CONNECT tunnels.
- `blocked_requests`: requests rejected by policy or failed before a tunnel
  was established.
- `telegram_attempts`: blocked authorities ending in the `telegram.org`,
  `t.me`, `telegram.me`, or `telesco.pe` DNS suffixes.
- `other_blocked_count`: blocked requests outside those official suffixes.
- `dns_lookups`: lookups attempted for the pinned host after authority checks.
- `upstream_connects`: upstream socket attempts after address validation.
- `upstream_failures`: DNS, address-policy, or upstream connection failures.

`status: healthy` means the proxy listener is active and neither listener has
reported an internal server error; it does not establish readiness. Consumers
must call `isReadySnapshot(snapshot)`, which rejects missing or extra fields,
wrong types, an unhealthy status, blocked requests, and upstream failures. It
also requires at least one established allowed tunnel and consistent DNS,
upstream-attempt, and successful-tunnel counts. This check does not cover the
browser's HTTP or WSS readiness.

`resolveHost` and `openUpstream` are unit-test seams. Runtime code must use the
default resolver and connector and must not override them.

## Credential-free commands

Run from `/opt/telegram-server`. `VERIFIED_MANIFEST` is the nonsecret release
record supplied by the reviewed deployment. It must have exactly these fields:

```json
{
  "sourceCommit": "<40 lowercase hex characters>",
  "archiveSha256": "sha256:<64 lowercase hex characters>",
  "contentDigest": "sha256:<64 lowercase hex characters>",
  "url": "https://telegram-server.tailaa4918.ts.net/<path>?v=<sourceCommit>"
}
```

The wrapper checks this schema, both digest formats, the source revision, and
the fixed HTTPS hostname. Before any Docker command, it fetches the served
`mtproto-target.json` from that pinned host and checks its source revision,
content digest, fixed WSS hostname, and fingerprint format. The browser then
fetches the manifest through Chromium and rechecks its source, artifact digest,
endpoint hostname, and fingerprint format before reporting ready. `run.sh`
closes stdin automatically for readiness and blocked-control; acceptance keeps
stdin attached to the separately approved host-local credential pipe.

```sh
cd /opt/telegram-server
./deploy/browser-acceptance/run.sh readiness --manifest "$VERIFIED_MANIFEST" </dev/null
./deploy/browser-acceptance/run.sh blocked-control --manifest "$VERIFIED_MANIFEST" </dev/null
```

Readiness first runs the blocked-host control, verifies that the denied request
caused no DNS lookup or upstream connection and that direct browser-container
TCP to the target Tailnet address on port 443 fails. It then restarts the
observer to reset its counters and opens a fresh sandboxed browser context for
the positive origin and WSS checks. The readiness command prints one fixed
metadata JSON line only when both phases pass. The separate blocked-control
command prints:

```json
{"status":"blocked_expected","blocked_telegram_org_attempts":1,"telegram_org_upstream_connects":0,"payloads_retained":0}
```

Readiness prints:

```json
{"status":"ready","http_status":200,"asset_502_count":0,"browser_wss_status":101,"browser_wss_unique_targets":1,"observer_success_hosts":1,"observer_target_host":"telegram-server.tailaa4918.ts.net","telegram_org_attempts":0,"payloads_retained":0}
```

The wrapper rejects extra or malformed output and maps failures to
`{"status":"error","code":"<fixed-enum>"}`. It suppresses Compose and
container stderr. The one-shot lock serializes this fixed project; preflight
requires at least 2 GiB free under Docker storage. An exit trap removes only
this project's containers, network, and local image with `down --rmi local`.
It never prunes shared images or volumes.

## Compose and image verification

Inspect the resolved Compose policy from the repository root:

```sh
docker compose --env-file /dev/null --project-directory deploy/browser-acceptance --file deploy/browser-acceptance/compose.yaml config --format json
```

`compose-policy.test.mjs` asserts the resolved service, network, mount,
logging, image pin, memory, swap, CPU, seccomp, and capability policy. On
native amd64 and arm64 builders, pass the matching pinned digest from `run.sh`
as `PLAYWRIGHT_IMAGE` and `linux/amd64` or `linux/arm64` as
`PLAYWRIGHT_PLATFORM`, then build the `browser` service. Each CI image job runs
the sandbox and egress smoke plus Chromium readiness against an ephemeral,
credential-free HTTPS/WSS origin through the CONNECT observer. The readiness
smoke covers a successful HTTP/WSS handshake, manifest mismatch, asset 502,
missing WSS, and failed WSS. The LXC deploy ticket uses the same wrapper on its
native arm64 platform.

The expected verification set for this directory is:

```sh
npm ci --prefix deploy/browser-acceptance --ignore-scripts --no-audit --no-fund
node --test deploy/browser-acceptance/*.test.mjs
for file in deploy/browser-acceptance/*.mjs; do node --check "$file"; done
docker compose --env-file /dev/null --project-directory deploy/browser-acceptance --file deploy/browser-acceptance/compose.yaml config --format json
```

On-LXC browser sandbox, MagicDNS/Serve reachability, direct-egress, and
attached-stdout checks belong to the reviewed deployment ticket. No staging
LXC is available for this runtime; the PR's Compose policy and synthetic tests
are the preview gate. Roll back by reverting the PR and using the normal
deployment path, then run:

```sh
docker compose --env-file /dev/null --project-directory deploy/browser-acceptance --file deploy/browser-acceptance/compose.yaml --project-name telegram-browser-acceptance down --rmi local
```

## QA adapter contract

The corrected QA-owned script is a read-only bind mount whose SHA-256 must
match the approved runner. Its fresh Chromium profile and page use the same
explicit observer proxy, strict empty bypass policy, sandbox argument checks,
and live observer controls as readiness. The adapter exports `snapshot`,
`startIndependentCapture`, `cleanupNewBinding`, `browserLaunchPolicy`, and
`verifySandbox`, following the exact typed schemas in the approved runner
instructions. Reports may contain only normalized target metadata and typed
counts; the browser must not retain frames, request/response bodies, console
messages, HAR, traces, video, screenshots, raw errors, credentials, or session
material.

The protected host snapshot and independent capture portions deliberately fail
closed in this PR. They require host-only registration and auth-binding
observations plus a separately reviewed independent capture provider; those
cannot be inferred from container state, observer counters, or the public
manifest. Until that provider is delivered and reviewed, `acceptance` exits
before reading stdin. No real authentication is performed by this runtime
change.

When the host adapter is approved, the only credential-bearing invocation is
the host-local provider pipe into `docker compose run -T`; credentials never
enter environment variables, argv, files, SSH, or logs:

```sh
./deploy/browser-acceptance/run.sh acceptance --manifest "$VERIFIED_MANIFEST" --script "$APPROVED_QA_SCRIPT" < <(host-local-credential-provider)
```

`run.sh` accepts only the runner's pinned SHA-256, mounts the manifest and QA
script read-only, uses `-T`, and leaves stdin attached directly to Compose.
Registration closure, the fresh reviewed artifact, and the approved host
adapter remain prerequisites owned by the deployment/acceptance workflow.
