## Alpha cluster bootstrap

On `alpha`, review the host changes and then apply the bootstrap:

```sh
sudo deploy/k8s/bootstrap-alpha.sh --dry-run
sudo deploy/k8s/bootstrap-alpha.sh --apply
```

The script
creates the `teagram` namespace, transfers only the production credentials and
runtime values described below from the live `teagram` LXC, installs the
namespace-scoped deploy account and agent operations menu, and writes the
restricted deploy kubeconfig to `/root/teagram-k3s-deploy.kubeconfig` with mode
`0600`. Store that file in the designated GitHub environment secret. It also
writes `/root/teagram-k3s-cutover.kubeconfig` with the k3s admin client
certificate, root-owned mode `0600`; use it only for the root-run cutover,
rollback, and target reset on alpha. Never store this admin kubeconfig in GitHub
or give it to the deploy job. The API and operations SSH menu are exposed
through Tailscale Serve only; the Incus proxy listeners bind to host loopback.

Before workloads have written production data, rollback on `alpha` with:

```sh
sudo tailscale serve --yes --tcp=6443 off
sudo tailscale serve --yes --tcp=2222 off
sudo incus delete --force teagram-k3s
sudo rm -f /root/teagram-k3s-deploy.kubeconfig
sudo rm -f /root/teagram-k3s-cutover.kubeconfig
```

The old `teagram` LXC remains untouched. After production data is written,
first restore the matching PostgreSQL and PVC snapshot using the recovery
procedure below before removing the cluster.

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
Read the [alpha cutover threat model](cutover-threat-model.md) before moving
production state or exposing the cutover listeners.
Read the [deploy credential threat model](deploy-token-threat-model.md) before
storing, using, or rotating the production kubeconfig. The bootstrap enforces
the Pod Security `baseline` profile on `teagram` and verifies it rejects a
privileged `hostPath` pod.

The migration Job name includes the first 16 characters of its image digest,
so each pinned release gets a new Job instead of attempting to mutate an
immutable completed Job. `prod/kustomization.yaml` pins the required migration
version with its image digests. The base `migration-version` may remain at an
earlier migration while a new migration is being developed; CI checks that it
names a migration that exists, rather than requiring every migration PR to edit
`deploy/k8s/`. The production validator checks that the required version matches
the newest migration in the pinned migration image and that this image contains
every migration, with identical contents, present in the pinned teagramd image.
The CI workflow sets its disposable overlay to the newest migration before
rendering and applying it, so teagramd waits until that migration has run.

After a migration merges and CI publishes the corresponding images, update the
production teagramd and migration image digests, required migration version,
and migration Job name together. The production pin check must pass before
applying that overlay. This keeps a server image that needs a new migration
from being deployed with an older migration image.

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
| `teagramd-admin` Secret | `TG_ADMIN_TOKEN_HASH`; optional `TG_ADMIN_ORIGIN` |
| `cloudflared-token` Secret | `token` |
| `ghcr-pull` Secret | Docker config for the private GHCR packages |

The alpha cutover runner creates the `teagramd-admin` Secret from the existing
Compose `TG_ADMIN_TOKEN_HASH` and optional `TG_ADMIN_ORIGIN`, without printing
either value. Subsequent production deployments use the same Secret.

Keep the Postgres password in `TG_POSTGRES_DSN` identical to
`POSTGRES_PASSWORD`. Do not commit the Secret objects or credentials. The alpha
bootstrap creates the `teagram` namespace; the overlays intentionally do not
include a Namespace object so the namespace-scoped deploy account can apply
them. The server's RSA identity and auth-key encryption key live on the
`key-data` PVC;
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
kubectl wait --for=create pvc/key-data-teagramd-0 \
  --namespace teagram --timeout=180s
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

## Alpha production cutover

After `bootstrap-alpha.sh` has created `teagram-k3s` and its namespace
credentials, run the cutover from a reviewed checkout on `alpha`:

```sh
sudo deploy/k8s/cutover-alpha.sh --dry-run
sudo deploy/k8s/cutover-alpha.sh --apply
```

The script keeps the k8s app and tunnel connector at zero replicas while it
restores PostgreSQL, copies `server_key.pem`, `enc_key.hex`, and the local blob
tree into the new PVCs, applies the pinned Atlas migration, then encrypts and
checksums the copied blobs into RustFS with the current media key. It saves a
mode-0600 custom-format database dump on the PostgreSQL PVC and keeps a
read-only local-blob copy in `legacy-blobs`. The database dump and old Compose
volumes are recovery points for at least seven days. A fresh smoke test dumps
and restores a provisioned account, then completes a new SRP login with the
same password and RSA identity; CI runs it in `test: smoke`.

Before the final snapshot, the script stops every running Compose service other
than PostgreSQL, including `teagramd` and any active database or blob writer,
and stops `teagram-web-cloudflared`. It verifies PostgreSQL has no remaining
client connections. Compose volumes and PostgreSQL data are left intact. The
fence-to-switch interval is planned downtime; a failure before the complete
edge switch restarts the stopped Compose services.

The cutover keeps the k8s app and connector at zero while it restores the
database, identity and blobs. It then starts the WebSocket and admin listeners
on the k3s teagramd Service and installs restartable `systemd` port-forwards in
`teagram-k3s`. Tailscale Serve keeps all five existing paths and sends `/` to
linkselector, `/apiws`, both client discovery paths, and `/admin` to the
matching teagramd Service ports. It also forwards the existing raw MTProto TCP
listener on port `2443` to the k3s teagramd Service and verifies the listener is
reachable before changing Serve. Funnel remains off. The Cloudflare connector
stays at zero until the dashboard route changes from `http://selector:8081` to
`http://linkselector:8081`; confirm that change before the k8s connector starts.
No Cloudflare API credential is provisioned.

The first live Serve route is the switch boundary. A failure after that point
leaves k3s serving and requires the printed explicit rollback command; it will
not automatically restart Compose and discard writes accepted by k3s. Rollback
requires confirmation of that data loss, scales the k8s app and connector to
zero, and verifies their pods have stopped before restoring the Cloudflare
route, all five Serve paths, and the MTProto TCP route. It then restarts the
stopped Compose services and stops both port-forwards. PostgreSQL and PVC data
remain intact. Writes made on k3s after the switch are not carried back to
Compose.

### Retry after a pre-switch failure

An apply failure before the first live Serve route attempts to restart the
stopped Compose services; if recovery reports incomplete, resolve that state
before continuing. It leaves the cutover phase and target resources for
diagnosis. The printed `--rollback` command restores the old routes and records
`rolled-back`; it does not remove the imported target data. Do not reset the
target until rollback has completed, the old Compose services and edges are
healthy, and no cutover or rollback command is running. This reset deletes the
failed target copy, so use it only after a pre-switch failure, when k3s has not
served production traffic. A rollback after the first live Serve route warns
that k3s writes are not copied back; preserve any such data before removing the
target.

After accepting that the failed target copy can be discarded, remove the
resources from the cutover overlay and its separately-created admin Secret and
PVCs, then remove the phase and recovery evidence directory. The namespace and
bootstrap credentials are retained. The first command removes the importer and
waits for its volume mounts to be released:

```sh
set -e
sudo env KUBECONFIG=/root/teagram-k3s-cutover.kubeconfig kubectl delete pod/cutover-volume-importer \
  --namespace teagram --ignore-not-found=true --wait=true --timeout=300s
sudo env KUBECONFIG=/root/teagram-k3s-cutover.kubeconfig kubectl delete \
  -k deploy/k8s/prod-cutover --ignore-not-found=true --wait=true
sudo env KUBECONFIG=/root/teagram-k3s-cutover.kubeconfig kubectl delete \
  secret teagramd-admin --namespace teagram --ignore-not-found=true
sudo env KUBECONFIG=/root/teagram-k3s-cutover.kubeconfig kubectl delete \
  pvc data-postgres-0 data-rustfs-0 key-data-teagramd-0 \
  legacy-blobs-teagramd-0 blob-mode-teagramd-0 \
  --namespace teagram --ignore-not-found=true --wait=true
sudo rm -rf -- /var/lib/teagram-k3s-cutover
```

The overlay deletion removes the workloads, Services, Jobs and overlay PVCs.
The explicit PVC command also removes the PostgreSQL and RustFS claims from the
base production manifests. Once the commands complete, a fresh `--apply` can
run from the same reviewed checkout.

CI runs the actual cutover runner with a disposable source namespace and a
separate target namespace in k3s. The source database, identity files, and a
blob fixture pass through the runner's fence, snapshot, transfer, and pinned
migration Job; CI signs into the restored account on the target, invokes the
runner's explicit rollback, then signs into the untouched source. No production
namespace or live cluster is used.
