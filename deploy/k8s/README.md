## Alpha cluster bootstrap

On `alpha`, review the host changes and then apply the bootstrap:

```sh
sudo deploy/k8s/bootstrap-alpha.sh --dry-run
sudo deploy/k8s/bootstrap-alpha.sh --apply
```

The script
creates the `teagram` namespace, transfers only the production credentials and
runtime values described below from the live `teagram` LXC, installs the
namespace-scoped deploy account, the restricted `teagram-agent` read account,
and the agent operations menu, and writes the
restricted deploy kubeconfig to `/root/teagram-k3s-deploy.kubeconfig` with mode
`0600`. Store that file as the `KUBECONFIG` secret in the `k3s-prod` GitHub
environment, restricted to deployments from `main`. It also
writes `/root/teagram-k3s-cutover.kubeconfig` with the k3s admin client
certificate, root-owned mode `0600`; use it only for root-run cutover, rollback,
target reset, and the one-time deployer-role refresh documented below on alpha.
Never store this admin kubeconfig in GitHub or give it to the deploy job. The API and operations SSH menu are exposed
through Tailscale Serve only; the Incus proxy listeners bind to host loopback.
The bootstrap also installs a cluster-scoped ValidatingAdmissionPolicy and
binding. Rerunning the bootstrap reapplies the same manifests without changing
them. The policy limits pod deletion by `teagram-agent` to controller-owned
teagramd, linkselector, linklanding, and cloudflared pods.

To roll back only agent access after bootstrap, run this whole block in one
shell. It deletes the ServiceAccount first to revoke its TokenRequest tokens,
then removes its RoleBinding. `set -euo pipefail` stops the sequence on either
failure, leaving admission protection in place until the agent identity and
its permissions are revoked:

```sh
set -euo pipefail
sudo incus exec teagram-k3s -- k3s kubectl delete serviceaccount teagram-agent \
  --namespace teagram --ignore-not-found
sudo incus exec teagram-k3s -- k3s kubectl delete rolebinding teagram-agent \
  --namespace teagram --ignore-not-found
sudo incus exec teagram-k3s -- k3s kubectl delete \
  validatingadmissionpolicybinding teagram-agent-pod-delete --ignore-not-found
sudo incus exec teagram-k3s -- k3s kubectl delete \
  validatingadmissionpolicy teagram-agent-pod-delete --ignore-not-found
sudo incus exec teagram-k3s -- k3s kubectl delete role teagram-agent \
  --namespace teagram --ignore-not-found
```

This removes the agent credential path without changing workloads or their
data. Reverting the code and rerunning the bootstrap restores the configured
access path.

The deploy job cannot update its own Role. When its allowed resource kinds
change, reapply `deployer-access.yaml` with the root-only cutover kubeconfig
before the next release; the workflow checks its required backup permissions
before creating any resources. After this change merges, refresh the role from
the reviewed checkout on alpha before the first `Deploy.` run:

```sh
sudo env KUBECONFIG=/root/teagram-k3s-cutover.kubeconfig \
  kubectl apply --filename deploy/k8s/deployer-access.yaml
```

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
Read the [agent access threat model](agent-access-threat-model.md) before
minting or using a `teagram-agent` TokenRequest token.

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
production teagramd, migration and mc-init image digests, required migration
version, migration Job name, and `prod/source-revision` together. The production
pin check must pass before applying that overlay. The cutover reads the live
Compose image revision, resolves that revision's published `sha-<revision>`
images to immutable digests, and derives the migration version and both Job
names from those images. It checks the live database migration version against
the resolved migration image before creating target workloads or fencing
source writes.

The production overlay expects these namespace-scoped resources to be supplied
by the cluster's secret and configuration management before it is applied:

| Resource | Required keys or values |
| --- | --- |
| `teagramd-runtime` ConfigMap | `TG_PUBLIC_LINK_PREFIX`, `TG_ADVERTISE_ADDR`, `TG_MEDIA_KEY_CURRENT` |
| `link-edge-runtime` ConfigMap | `TG_INVITE_WEB_SIGNUP_URL`; optional `TG_INVITE_DESKTOP_URL` |
| `web` Service | ClusterIP endpoint on port 8080 for ordinary web traffic |
| `postgres-credentials` Secret | `POSTGRES_PASSWORD`, `TG_POSTGRES_DSN` |
| `rustfs-credentials` Secret | `RUSTFS_ACCESS_KEY`, `RUSTFS_SECRET_KEY` |
| `teagramd-blob-credentials` Secret | `TG_BLOB_S3_ACCESS_KEY_ID`, `TG_BLOB_S3_SECRET_ACCESS_KEY` |
| `teagramd-media-keys` Secret | Current key file `1.key` and any retained key versions |
| `teagramd-admin` Secret | `TG_ADMIN_TOKEN_HASH`; optional `TG_ADMIN_ORIGIN` |
| `teagramd-turnstile` Secret | `secret-key`, mounted read-only at `/run/secrets/teagramd-turnstile/secret-key` with mode `0440` |
| `cloudflared-token` Secret | `token` |
| Agent token (external credential, not a Kubernetes resource) | The Steward mints a TokenRequest token inside the LXC with the root-only admin credential and `kubectl create token teagram-agent --namespace teagram --duration=2160h` (expires in 90 days or less; record its exact expiry). Never store it in a `kubernetes.io/service-account-token` Secret or commit it. To rotate or revoke it, delete and recreate the `teagram-agent` ServiceAccount, then mint a replacement token. |

All images used by the production overlays are publicly pullable. Bootstrap does
not read Docker credentials or create a registry Secret; this also keeps the
cutover usable on an LXC with no Docker registry configuration. Public utility
images such as BusyBox are pinned from `mirror.gcr.io`.

## PostgreSQL backups

The production overlay enables a daily 03:17 UTC PostgreSQL custom-format dump.
`Deploy.` first applies the `backup-bootstrap/` overlay with the CronJob
suspended, runs and verifies a fresh dump, and only then applies the production
release. The nightly CronJob becomes active when `prod/` is applied. Both paths
write to the `postgres-backups` 20 GiB PVC and keep the newest seven verified
dumps. Each Job runs `pg_restore --list` before publishing a dump by an atomic
rename; its log records the path, byte count, and SHA-256 without credentials.

The backup PVC is local to the k3s node, unencrypted, and has no off-cluster
copy. It is a release recovery point, not protection against loss of the host
or its disk. The deploy job checks the pre-deploy Job result and stops before
changing application workloads if the dump does not verify. Nightly status is
visible through the CronJob's `lastSuccessfulTime`, retained Job records, pod
logs, and Kubernetes warning events:

```sh
kubectl get cronjob postgres-backup --namespace teagram \
  --output 'custom-columns=NAME:.metadata.name,SUSPENDED:.spec.suspend,LAST-SCHEDULE:.status.lastScheduleTime,LAST-SUCCESS:.status.lastSuccessfulTime'
kubectl get jobs --namespace teagram \
  --selector=app.kubernetes.io/name=postgres-backup --sort-by=.status.startTime
kubectl get events --namespace teagram --sort-by=.lastTimestamp
kubectl logs --namespace teagram job/<failed-backup-job> --all-containers=true
```

## Expiring channel-post summary initialization

After the production rollout is ready and smoke checks pass, `Deploy.` runs
`teagramd maintenance initialize-channel-post-expiring-summaries` in a
restricted, short-lived Job using the tested teagramd image. Each Job reads
`TG_POSTGRES_DSN` from `postgres-credentials` and mounts the server key PVC
read-only so the maintenance command can load its required auth-key key. The
runner starts another batch until the command reports `0 remaining`; a failed,
unreadable, or non-progressing batch fails the deployment and triggers workload
rollback. Completed batches are safe to resume, as described in
`docs/migrations.md`.

`test: k8s-deploy` creates a disposable account, takes a dump, recreates the
throwaway database from that dump, and signs in with the restored account. Do
not restore a production dump into the live database as a deploy step; schema
recovery requires the matching database snapshot and the recovery procedure.

The alpha cutover runner creates the `teagramd-admin` Secret from the existing
Compose `TG_ADMIN_TOKEN_HASH` and optional `TG_ADMIN_ORIGIN`, without printing
either value. Subsequent production deployments use the same Secret.

The Turnstile secret is supplied externally as `teagramd-turnstile`; its value
is never stored in the repository. The teagramd container reads the mounted
`secret-key` file through `TG_TURNSTILE_SECRET_KEY_FILE`.

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
`Deploy.` records the prior revisions of PostgreSQL, RustFS, teagramd, the link
edge Deployments, and cloudflared. If apply, a Job, rollout, or smoke fails, its
release runner issues `kubectl rollout undo` for each changed controller and
waits for the previous revision to become ready. If an Atlas migration already
completed, use the matching pre-deploy dump on the `postgres-backups` PVC before
resuming application traffic.

The Cloudflare Tunnel's remotely managed route for `web.teagram.io` should
target `http://linkselector:8081`. The selector sends the exact public API paths
`/apiws`, `/.well-known/teagramd/client`, and the legacy
`/.well-known/telegramd/client` to `http://teagramd:2446`; it sends teagramd
links to `http://linklanding:8082` and ordinary web traffic to the `web`
Service on port 8080. For `server.teagram.io`, route `^/apiws` and
`^/\.well-known/teagramd/client` to `http://teagramd:2446`. The public
listener is a ClusterIP Service port, advertises `server.teagram.io:443`, allows
the `https://web.teagram.io` origin, and uses the live
`TG_PUBLIC_REGISTRATION=invite` policy. It keeps `TG_CLIENT_ADDR_TRUST=socket` while
Cloudflare client-address trust is disabled. It is separate from the tailnet
WebSocket listener on 2444. Configure the external
MTProto TCP route separately before setting `TG_ADVERTISE_ADDR`; the Cloudflare
HTTP hostname route does not expose the MTProto listener. The
Cloudflared metrics endpoint is exposed only inside the cluster at
`cloudflared-metrics:2000/metrics`. Prometheus-compatible scrapers can use the
service annotations. Container logs are written to stdout and stderr for the
cluster's node or central log collector. After cutover, `Deploy.` requires the
k3s `cloudflared` Deployment to have exactly one ready replica; the cutover
must leave the old Compose connector stopped before enabling that replica.

## Alpha production cutover

After `bootstrap-alpha.sh` has created `teagram-k3s` and its namespace
credentials, run the cutover from a reviewed checkout on `alpha`:

```sh
sudo deploy/k8s/cutover-alpha.sh --dry-run
sudo --preserve-env=GH_TOKEN,GHCR_USERNAME deploy/k8s/cutover-alpha.sh --apply
```

The alpha host needs the GitHub CLI (`gh`) for the image-provenance check. Before
`--apply`, load a read-only GitHub token from the approved secret store into the
invoking shell as `GH_TOKEN` and set `GHCR_USERNAME` to the token owner's GitHub
username. The token needs read access to repository attestations and the GHCR
packages. Run the apply command with
`sudo --preserve-env=GH_TOKEN,GHCR_USERNAME`; the runner uses the token for
GitHub API and GHCR verification, writes GHCR auth to a mode-0600 temporary
Docker config, then removes it on exit. It does not persist the token or
registry credentials.

Before applying workloads, the runner reads the live teagramd revision and
pulls the five published k8s images tagged for that SHA and resolves their
immutable registry digests. For each digest it verifies GitHub build provenance
with `gh attestation verify`, using the temporary GHCR credentials. It requires
source ref `refs/heads/main`, the exact live source commit, and
`.github/workflows/ci.yml` as the signer. Main CI emits
these attestations only on successful main pushes; a PR-produced attestation
cannot satisfy the source and signer checks. It pins only verified digests in
the rendered manifest and derives migration versions and immutable Job names
from those images. Missing provenance, a digest from a PR build, a label
mismatch, or a database mismatch stops the run before source writes are fenced.
Immediately before the first Serve route changes, the runner verifies that
every fenced Compose container is still stopped, PostgreSQL has no client
connections, RustFS is running, and the teagramd revision has not changed.

The script keeps the k8s app and tunnel connector at zero replicas while it
restores PostgreSQL and applies the matching Atlas migration. It copies
`server_key.pem`, `enc_key.hex`, the blob-mode authority, and the retained local
blob tree into the new PVCs; it also verifies the k3s media-key Secret against
the live key set and sets `TG_MEDIA_KEY_CURRENT` to the source runtime's active
key version. It mirrors the source RustFS `teagram/teagramd/` prefix into
the new RustFS PVC with object bytes unchanged. The source bucket and Compose
volumes are read-only. Before the route switch, the runner checks object count,
total bytes, and each object's SHA-256 against the target. The source S3
credential is used only by a temporary client, which is removed after the
copy. A target-only port-forward is removed when the copy completes.

The runner saves a mode-0600 custom-format database dump on the PostgreSQL PVC
and keeps a read-only local-blob copy in `legacy-blobs`. The database dump and
old Compose volumes are recovery points for at least seven days. CI uploads a
document through the source S3-backed teagramd before cutover and downloads
the same document through the target after cutover, comparing the bytes.

Before the final snapshot, the script stops every running Compose service other
than PostgreSQL and RustFS, including `teagramd` and any active database or blob
writer, and stops `teagram-web-cloudflared`. PostgreSQL and RustFS are verified
to remain running, and PostgreSQL has no remaining client connections. RustFS
must stay online because the post-fence source S3 listing, per-object checksum
reads, and mirror use `http://rustfs:9000`. Compose volumes and PostgreSQL data
are left intact. The fence-to-switch interval is planned downtime; a failure
before the first live edge switch restarts only the Compose services the runner
stopped. PostgreSQL and RustFS are not stopped during recovery.

After a successful cutover, the old Compose application and edge services stay
stopped, while PostgreSQL and source RustFS remain running with their existing
data. The k8s application uses the copied objects in target RustFS; the source
bucket remains the pre-switch recovery copy for at least seven days. Apply and
rollback leave the source PostgreSQL and RustFS services and their data in
place. Retiring them requires a separate cleanup after the recovery period.

The cutover keeps the k8s app and connector at zero while it restores the
database, identity and blobs. It applies teagram-web's pinned k8s base and adds
the `web` Service on port `8080`, which lets linkselector reach the app inside
the cluster. It then starts the WebSocket and admin listeners on the k3s
teagramd Service and installs restartable `systemd` port-forwards in
`teagram-k3s`. The Bot API binds to pod loopback and uses the same Unix socket
and TCP relay arrangement as Compose, preserving Tailscale's PROXY protocol.
Tailscale Serve keeps all five existing paths and sends `/` to linkselector,
`/apiws`, both client discovery paths, and `/admin` to the matching teagramd
Service ports. The Cloudflare routes for `server.teagram.io` use the separate
public WebSocket Service port 2446. It forwards the existing raw MTProto TCP
listener on port `2443` and Bot API on `2447` to their k3s Service ports,
verifying both before changing Serve. Funnel remains off. The Cloudflare
connector stays at zero until the dashboard routes change from `web.teagram.io`
`http://127.0.0.1:8081` to `http://linkselector:8081`, and `server.teagram.io`
paths `^/apiws` and `^/\.well-known/teagramd/client` from
`http://127.0.0.1:2446` to `http://teagramd:2446`; confirm those changes before
the k8s connector starts. No Cloudflare API credential is provisioned.

The first live Serve route is the switch boundary. A failure after that point
leaves k3s serving and requires the printed explicit rollback command; it will
not automatically restart Compose and discard writes accepted by k3s. Rollback
requires confirmation of that data loss, scales the k8s app and connector to
zero, and verifies their pods have stopped before restoring the Cloudflare
route, all five Serve paths, the MTProto TCP route, and the old Bot API route.
It then restarts only the Compose services stopped by apply and stops both
port-forwards. PostgreSQL and source RustFS were already running, so the restored
Compose teagramd reconnects to the original bucket. PostgreSQL and PVC data
remain intact. Writes made on k3s after the switch are not copied to the source
RustFS bucket or carried back to Compose.

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

CI runs the actual cutover runner with a disposable S3-backed source namespace
and a separate target namespace in k3s. The source database, encrypted S3
object, identity files, blob-mode authority, and retained local blob fixture
pass through the runner's fence, snapshot, transfer, and revision-matched
migration Job. CI downloads the pre-cutover document through target teagramd,
invokes the runner's explicit rollback, then signs into the untouched source.
No production namespace or live cluster is used.
