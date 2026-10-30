## Kustomize layouts

`base/` contains the single-replica teagramd, PostgreSQL, and RustFS workloads,
the two-replica public WebSocket deployment with its NetworkPolicy-gated
HAProxy sidecar,
the RustFS initialization and Atlas migration Jobs, the link landing and
selector services, and the Cloudflare Tunnel connector. `prod/` pins the
application images by digest. `ci/` supplies disposable runtime settings and
replaces cloudflared with a local process so the deployment can be exercised
without a tunnel credential.

The migration Job name includes the first 16 characters of its image digest,
so each pinned release gets a new Job instead of attempting to mutate an
immutable completed Job. The teagramd init container waits until Atlas reports
at least the required version from `base/migration-version`; CI checks that file
against the newest migration filename before validating or deploying the
overlay.

The production overlay expects these namespace-scoped resources to be supplied
by the cluster's secret and configuration management before it is applied:

| Resource | Required keys or values |
| --- | --- |
| `teagramd-runtime` ConfigMap | `TG_PUBLIC_LINK_PREFIX`, `TG_ADVERTISE_ADDR` |
| `link-edge-runtime` ConfigMap | `TG_INVITE_WEB_SIGNUP_URL`; optional `TG_INVITE_DESKTOP_URL` |
| `web` Service | ClusterIP endpoint on port 8080 for ordinary web traffic |
| `postgres-credentials` Secret | `POSTGRES_PASSWORD`, `TG_POSTGRES_DSN` |
| `rustfs-credentials` Secret | `RUSTFS_ACCESS_KEY`, `RUSTFS_SECRET_KEY` |
| `teagramd-blob-credentials` Secret | `TG_BLOB_S3_ACCESS_KEY_ID`, `TG_BLOB_S3_SECRET_ACCESS_KEY` |
| `teagramd-media-keys` Secret | Current key file `1.key` and any retained key versions |
| `teagramd-public-identity` ConfigMap | `TG_RSA_KEY_FINGERPRINT` from the primary pod's `ensure-identity` init container |
| `cloudflared-token` Secret | `token` |
| `ghcr-pull` Secret | Docker config for the private GHCR packages |

Keep the Postgres password in `TG_POSTGRES_DSN` identical to
`POSTGRES_PASSWORD`. Do not commit the Secret objects or credentials. The
server's RSA identity and auth-key encryption key live on the `key-data` PVC;
back up and restore that volume together with Postgres when moving an existing
installation. `legacy-blobs` is mounted read-only and retained for recovery.
RustFS data lives on its own PVC.

The `teagramd-public` deployment shares the `key-data`, `legacy-blobs`, and
`blob-mode` claims with the primary `teagramd-0` pod. Its two replicas and
`TG_REPLICA_COUNT=2` must stay aligned.

The primary `teagramd` StatefulSet runs `ensure-identity` against its key PVC
before serving. That command bootstraps an empty PVC and validates an existing
RSA key without replacing it. The primary server keeps its auth-key encryption
key on the same PVC. On the first install, apply the overlay to initialize the
primary pod, then create the identity ConfigMap from its init-container output:

```sh
kubectl logs --namespace teagram teagramd-0 --container ensure-server-identity
kubectl create configmap teagramd-public-identity --namespace teagram \
  --from-literal=TG_RSA_KEY_FINGERPRINT=<fingerprint>
```

The public replicas wait for that ConfigMap and pin their loaded RSA identity
to its fingerprint while reading the shared auth-key encryption key.

The first deployment applies the Atlas Job before teagramd can become ready.
On later releases, applying a new image digest creates a new migration Job and
the updated teagramd pod stays in its init phase until the required migration
version has been applied. Before a production change, snapshot PostgreSQL and
the persistent volumes. An application rollback restores the prior image
digests and reapplies the overlay; schema rollback requires restoring the
matching database snapshot because the Job does not run down migrations.

The Cloudflare Tunnel's remotely managed public hostname route for links should
target `http://linkselector:8081`; route `server.teagram.io` to
`http://teagramd-public:2446` for the public WebSocket listener. The selector
sends teagramd links to
`http://linklanding:8082` and ordinary web traffic to the `web` Service on
port 8080. `teagramd` is exposed only by a ClusterIP Service here. Configure
its external TCP route separately before setting `TG_ADVERTISE_ADDR`; the
Cloudflare HTTP hostname route described above does not expose the MTProto
listener. The Cloudflared metrics endpoint is exposed only inside the cluster
at `cloudflared-metrics:2000/metrics`. Prometheus-compatible scrapers can use
the service annotations. Container logs are written to stdout and stderr for
the cluster's node or central log collector.

Apply the overlay only after the external resources exist:

```sh
kubectl apply -k deploy/k8s/prod
```

This repository's CI renders both overlays and deploys `ci/` to a disposable
k3s cluster. It does not apply `prod/` to a live cluster.
