# Mixed-trust Compose topology

`docker-compose.mixed-trust.yml` is an explicit configuration foundation for
running the current direct listener beside two replacement replicas. Normal
`docker compose up` does not load it. The overlay leaves the existing service,
its published bindings, and the `pgdata`, `tgkey`, and `tgblobs` volumes in
place. Do not change tailnet forwarding or direct listener bindings as part of
this step.

The legacy service uses socket trust. Replacement replicas use PROXY v2 and
trust only the HAProxy container's exact `/32` address. HAProxy sends PROXY v2
to both application backends and has no route to the legacy service. The
application frontends are TCP passthrough, so WebSocket `Host` and `Origin`
headers reach the application unchanged. The existing explicit Origin allowlist
continues to decide which browser origins can connect. HAProxy's health URI is
bound to its private container loopback and reports whether an MTProto backend
is reachable. WebSocket readiness and application readiness still require a
real client RPC.

Before enabling the overlay, keep the same Compose project name, environment
file, and existing file order used by the running stack. If the deployment
uses an untracked `docker-compose.override.yml`, include it explicitly before
the mixed-trust overlay so its existing legacy bindings and settings remain in
the merged configuration:

```bash
docker compose \
  -f docker-compose.yml \
  -f docker-compose.override.yml \
  -f docker-compose.mixed-trust.yml \
  up -d tcp-proxy telegramd-proxy-1 telegramd-proxy-2
```

Omit the override file only when the existing stack does not use one. Set
`TG_REPLICA_COUNT` in the Compose `.env` file or shell environment to the
fleet-wide count used by the legacy process and replacements. Supply
`TG_RSA_KEY_FINGERPRINT`, `TG_ADVERTISE_ADDR`, and
`TG_WEBSOCKET_ALLOWED_ORIGINS` in the Compose `.env` file or shell environment;
service-level values in an override file are not available to Compose
interpolation. The overlay also requires a pre-provisioned shared
`TG_AUTHKEY_ENC_KEY` or key file in `tgkey`. Compose `extends` copies the
tracked `telegramd` service definition, not arbitrary changes made to it in an
override file. If an override changes required application settings, mirror
those settings under both proxy services before routing traffic.
Choose `TG_MIXED_TRUST_SUBNET` so it does not overlap any host or Docker
network; the configured proxy address must remain a single address in that
subnet. The default proxy endpoints are temporary loopback ports 12443 and
12444. The application replicas publish no host ports, and the overlay does
not expose the admin listener on 2445.

Inspect `docker compose ps` and the proxy and replica logs during a disposable
preflight. HAProxy's health state alone does not prove identity, discovery, or
application readiness. The repository's mixed-trust smoke scenario establishes
real TCP and WebSocket RPC streams through both listeners, starts the proxy
while legacy streams are active, rejects an untrusted forged PROXY-v2 header,
and checks that a disallowed Origin receives HTTP 403.

This overlay publishes only the temporary loopback test/bootstrap endpoints.
It does not configure production forwarding, tailnet source preservation, or
the later routing change. Until that work is separately validated, keep client
traffic on the existing listener. For rollback in this coexistence stage,
stop the two replacement replicas and then `tcp-proxy`; the legacy listener
continues serving its original bindings. If a later forwarding step has sent
traffic to the temporary endpoint, remove that forwarding first and keep both
the proxy and replicas running until dependent streams have closed or have
been safely aborted. Do not run `down -v` on the live Compose project.
