## Kustomize layouts

`base/` contains the single-replica teagramd, PostgreSQL, and RustFS workloads,
the RustFS initialization and Atlas migration Jobs, the link landing and
selector services, and the Cloudflare Tunnel connector. `prod/` pins the
application images by digest. `ci/` supplies disposable runtime settings and
replaces cloudflared with a local process so the deployment can be exercised
without a tunnel credential. `bootstrap/` contains a separate, explicitly
applied first-install Job for the server RSA identity; it is not included in
the production workload overlay.

Read the [Kubernetes server-identity threat model](identity-threat-model.md)
before bootstrapping, backing up, restoring, or replacing that identity.

The migration Job name includes the first 16 characters of its image digest,
so each pinned release gets a new Job instead of attempting to mutate an
immutable completed Job. The teagramd init container waits until Atlas reports
at least the required version from `base/migration-version`; CI checks that
file against the newest migration filename before validating or deploying the
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
| `cloudflared-token` Secret | `token` |
| `ghcr-pull` Secret | Docker config for the private GHCR packages |

Keep the Postgres password in `TG_POSTGRES_DSN` identical to
`POSTGRES_PASSWORD`. Do not commit the Secret objects or credentials. The
server's RSA identity and auth-key encryption key live on the `key-data` PVC;
back up and restore that volume together with Postgres when moving an existing
installation. `legacy-blobs` is mounted read-only and retained for recovery.
RustFS data lives on its own PVC.

Apply the production overlay only after the external resources exist:

```sh
kubectl apply -k deploy/k8s/prod
```

On a first installation only, provision the persistent claim and explicitly
bootstrap the server identity before expecting teagramd to become ready. The
normal teagramd process refuses to start when its RSA key is absent; it never
creates a replacement identity. Wait for the PVC, stop the StatefulSet pod so
the ReadWriteOnce claim is not mounted by both workloads, run the dedicated
Job, then restore the replica:

```sh
kubectl wait --for=jsonpath='{.status.phase}'=Bound pvc/key-data-teagramd-0 \
  --namespace teagram --timeout=180s
kubectl scale statefulset/teagramd --replicas=0 --namespace teagram
kubectl wait --for=delete pod/teagramd-0 --namespace teagram --timeout=180s
kubectl delete -k deploy/k8s/bootstrap --ignore-not-found=true
kubectl apply -k deploy/k8s/bootstrap
if ! kubectl wait --for=condition=complete job/teagramd-identity-bootstrap \
  --namespace teagram --timeout=300s; then
  kubectl logs job/teagramd-identity-bootstrap --namespace teagram \
    --all-containers=true --prefix=true || true
  kubectl describe job/teagramd-identity-bootstrap --namespace teagram || true
  exit 1
fi
kubectl logs job/teagramd-identity-bootstrap --namespace teagram
kubectl delete -k deploy/k8s/bootstrap
kubectl scale statefulset/teagramd --replicas=1 --namespace teagram
kubectl rollout status statefulset/teagramd --namespace teagram --timeout=180s
```

The bootstrap Job uses the same pinned teagramd image digest as `prod/` and
refuses to overwrite an existing key. It establishes the permanent server
identity, so run it only during a new installation before accepting client
accounts. If an established installation is missing its key, restore the
backed-up `key-data` volume instead; do not run the bootstrap Job to repair it.

The first deployment applies the Atlas Job before teagramd can become ready.
On later releases, applying a new image digest creates a new migration Job and
the updated teagramd pod stays in its init phase until the required migration
version has been applied. Before a production change, snapshot PostgreSQL and
the persistent volumes. An application rollback restores the prior image
digests and reapplies the overlay; schema rollback requires restoring the
matching database snapshot because the Job does not run down migrations.

The Cloudflare Tunnel's remotely managed route for links should target
`http://linkselector:8081`. The selector sends teagramd links to
`http://linklanding:8082` and ordinary web traffic to the `web` Service on
port 8080. `teagramd` is exposed only by a ClusterIP Service here. Configure
its external TCP route separately before setting `TG_ADVERTISE_ADDR`; the
Cloudflare HTTP hostname route does not expose the MTProto listener. The
Cloudflared metrics endpoint is exposed only inside the cluster at
`cloudflared-metrics:2000/metrics`. Prometheus-compatible scrapers can use the
service annotations. Container logs are written to stdout and stderr for the
cluster's node or central log collector.

This repository's CI renders the production, CI, and bootstrap overlays and
deploys `ci/` to a disposable k3s cluster. It does not apply production
manifests to a live cluster.
