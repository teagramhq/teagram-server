## Alpha cluster and deployment access

The `teagram-k3s` cluster was provisioned on alpha by MAIN-3278. The former
application `teagram` LXC has been deleted after its database dump and key
archive were copied to the external backup drive. Do not rerun the old
Compose-to-k3s cutover or configure application routes on an LXC host.

`Deploy.` is owned by MAIN-3281 (#1126). GitHub Actions joins the tailnet with
the `k3s-prod` OAuth client tagged only `tag:k3s-deploy`. Its namespace-scoped
`KUBECONFIG` targets `https://teagram.tailaa4918.ts.net:6443`; the in-cluster
Tailscale device forwards raw TCP to `kubernetes.default.svc:443`, so TLS ends
at kube-apiserver. The cluster CA is pinned in the kubeconfig, so a server
presenting another certificate fails TLS verification before Kubernetes gets
the bearer token. The workflow skips
until the repository variable `K3S_TAILNET_API_READY` is `true`; after the
one-time cutover and API reachability check, set it and manually dispatch the
first `Deploy.` run.

Admin access is `sudo incus exec teagram-k3s -- k3s kubectl` on alpha. The
bootstrap never exports a cluster-admin kubeconfig. Normal releases use only
the namespace-scoped deployment credential.

The alpha bootstrap also installs the restricted `teagram-agent` read account
and a cluster-scoped ValidatingAdmissionPolicy that limits its pod deletion to
the approved stateless controllers.

`bootstrap-alpha.sh` provisions only the k3s host and namespace access. It does
not read application values or keys from the deleted `teagram` LXC and does
not create workload ConfigMaps or Secrets. Restore those resources from the
external secret and configuration store listed below before applying `prod/`.
The bootstrap does not configure alpha Tailscale Serve, Incus API or SSH proxy
devices, or an SSH operations menu.

On a migration from the previous alpha API endpoint, the bootstrap preserves
the old tailnet IPv4 SAN, adds the cluster device DNS SAN, rotates the k3s
dynamic listener certificate served on port 6443, and verifies that certificate
against the cluster CA and endpoint DNS before updating the namespace-scoped
deploy kubeconfig. It backs up the old k3s service unit at
`/etc/systemd/system/k3s.service.pre-main-3607`. To roll back, restore that
unit, run `systemctl daemon-reload`, and restart k3s; the certificate may keep
the added DNS SAN. Run these commands on alpha:

```sh
sudo incus exec teagram-k3s -- install -o root -g root -m 0600 \
  /etc/systemd/system/k3s.service.pre-main-3607 \
  /etc/systemd/system/k3s.service
sudo incus exec teagram-k3s -- systemctl daemon-reload
sudo incus exec teagram-k3s -- systemctl restart k3s
```

To roll back only agent access after bootstrap, run this whole block in one
shell. It deletes the ServiceAccount first to revoke its TokenRequest tokens,
then removes its RoleBinding and admission protection:

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
change, reapply `deployer-access.yaml` from the reviewed checkout on alpha
before the next release; the workflow checks the permissions required for
backup and rollback before creating any resources:

```sh
sudo incus exec teagram-k3s -- k3s kubectl apply --filename=- \
  < deploy/k8s/deployer-access.yaml
```
## Kustomize layouts

`base/` contains the single-replica teagramd, PostgreSQL, and RustFS workloads,
the RustFS initialization and Atlas migration Jobs, the link landing and
selector services, the Cloudflare Tunnel connector, the in-cluster Tailscale
proxy, and the default-deny teagramd ingress policy. `prod/` pins application
images by digest. `ci/` supplies disposable runtime settings, replaces
cloudflared with a local probe pod, and disables the real Tailscale proxy so
CI never registers a production tailnet device. `bootstrap/` contains a
separate, explicitly applied first-install Job for the server RSA identity; it
is not included in the production workload overlay.

Read the [Kubernetes server-identity threat model](identity-threat-model.md)
before bootstrapping, backing up, restoring, or replacing that identity.
The [alpha cutover threat model](cutover-threat-model.md) is retained as
historical context only; the Compose cutover and its listeners are retired.
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
pin check must pass before applying that overlay. The retired Compose cutover
does not select images or modify the live database.

The production overlay expects these namespace-scoped resources to be restored
from the external secret and configuration management before it is applied.
The bootstrap deliberately leaves them untouched:

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
| `tailscale-auth` Secret | `authkey`; one-use auth key supplied through the external secret store |
| `teagram-tailscale-state` Secret | Persistent node state created once by the access installer |
| Agent token (external credential, not a Kubernetes resource) | Mint inside the LXC with `sudo incus exec teagram-k3s -- k3s kubectl create token teagram-agent --namespace teagram --duration=2160h` (expires in 90 days or less; record its exact expiry). Never store it in a `kubernetes.io/service-account-token` Secret or commit it. To rotate or revoke it, delete and recreate the `teagram-agent` ServiceAccount, then mint a replacement token. |
| GitHub `k3s-prod` environment secrets | `KUBECONFIG`, `K3S_TS_OAUTH_CLIENT_ID`, and `K3S_TS_OAUTH_SECRET`; the OAuth client can register only `tag:k3s-deploy` devices. Do not copy these secrets to `production` or `demo`. |
| GitHub Actions repository variable | `K3S_TAILNET_API_READY`; set to `true` only after the tagged API route, pinned kubeconfig, and first-run prerequisites are verified. |

All images used by the production overlays are publicly pullable. Bootstrap
does not read Docker credentials or create a registry Secret. Public utility
images are pinned from their configured registries.

## PostgreSQL backups

The production overlay enables a daily 03:17 UTC PostgreSQL custom-format dump.
`Deploy.` first applies the `backup-bootstrap/` overlay, runs and verifies a
fresh dump, and only then applies the production release. The bootstrap overlay
keeps the nightly CronJob active so a failed deployment cannot leave scheduled
backups disabled. Both paths write to the `postgres-backups` 20 GiB PVC and
keep the newest seven verified dumps. Each Job runs `pg_restore --list` before
publishing a dump by an atomic rename; its log records the path, byte count,
and SHA-256 without credentials.

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

Supply the Turnstile secret externally as `teagramd-turnstile`; its value is
never stored in the repository. Teagramd reads the mounted `secret-key` file
through `TG_TURNSTILE_SECRET_KEY_FILE`.

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
waits for the previous revision to become ready. After a StatefulSet rollback,
it deletes only Pods whose `controller-revision-hash` differs from the restored
revision; Kubernetes can otherwise leave an unready Pod on the failed template
and stall the rollback. If an Atlas migration already completed, use the
matching pre-deploy dump on the `postgres-backups` PVC before resuming
application traffic.

Before applying, `Deploy.` checks the digest-named migration and RustFS Jobs.
If one is still present with `Failed=True` from an earlier attempt, it prints
that Job's logs, sets its TTL to zero, and waits for the TTL controller to
remove it so the same SHA can recreate the Job. Newly failed Jobs are detected
within the polling interval and their logs are included in the failed run.

The Cloudflare Tunnel's remotely managed route for `web.teagram.io` targets
`http://linkselector:8081`. The selector sends teagramd links to
`http://linklanding:8082` and ordinary web traffic to the `web` Service on
port 8080. Route `server.teagram.io` `/apiws` and discovery paths directly to
`http://teagramd:2446`; it advertises `server.teagram.io:443` and allows the
`https://web.teagram.io` origin. The public Bot API uses `teagramd:2449`.
`teagramd` remains ClusterIP-only, and the public listener does not expose
MTProto port 2443.

## In-cluster ingress paths

| Path | Route | Listener |
| --- | --- | --- |
| Public web, `web.teagram.io` | cloudflared to `linkselector:8081` | linkselector routes to `web:8080` and `linklanding:8082` |
| Public desktop, `server.teagram.io` | cloudflared to `teagramd:2446` for `/apiws` and both discovery aliases | public WebSocket listener |
| Public Bot API | cloudflared to `teagramd:2449` | public Bot API listener |
| Tailnet web, `https://teagram.tailaa4918.ts.net/` | in-cluster Tailscale proxy to `linkselector:8081` | linkselector |
| Tailnet desktop and discovery | Tailscale proxy to `teagramd:2444` | tailnet WebSocket listener |
| Tailnet admin | Tailscale proxy to `teagramd:2445` | admin listener |
| Tailnet MTProto | Tailscale userspace forwarding to the proxy pod's loopback `socat` listener on 2443, then `teagramd:2443` | Half-close-preserving MTProto listener |
| Tailnet Bot API | TCP 2447 through Tailscale proxy with PROXY v1 to `teagramd:2447` | Bot API relay on pod port 2448 |
| Tailnet Kubernetes API | raw TCP 6443 through the cluster Tailscale device to `kubernetes.default.svc:443` | kube-apiserver terminates TLS; only delta and the deploy runner tags may connect |

Tailscale Serve's TCP forward closes both sockets on the first EOF. Tailnet
Bot API clients must keep the write side open until they receive the HTTP
response.

The public `server.teagram.io` and Bot API routes remain behind `hold-merge`
until MAIN-3599 and MAIN-3600 are live. Funnel stays disabled. The tunnel and
Tailscale Serve configuration target Kubernetes Service names only; neither
path depends on the deleted application LXC.

`teagramd-networkpolicy` selects teagramd pods and defaults ingress to deny.
It admits ports 2443, 2444, 2445, and 2448 only from the Tailscale proxy pod
identity, and ports 2446 and 2449 only from cloudflared. It does not trust pod
CIDRs or request headers. Linkselector and other application pods have a
separate source identity and are refused by both listener groups; CI probes
check the tailnet-labelled, linkselector-labelled, and untrusted sources.
Public traffic cannot assert tailnet identity because cloudflared has no access
to tailnet-only ports and caller input cannot change the in-cluster source pod
identity. The proxy also has default-deny egress with
only the teagramd and linkselector Service ports, cluster DNS, the k3s API,
and Tailscale control/relay UDP and HTTPS destinations allowed. The API
Service at `kubernetes.default.svc:443` is translated to the node endpoint at
port `6443`. The one-time access installer reads the ready addresses from the
`kubernetes` Endpoints object and creates a separate `/32` egress rule for each
API endpoint on TCP 6443. Its dry-run shows the exact policy, and rerunning it
refreshes the allowlist if an endpoint address changes. Rollback removes this
policy after confirming the proxy is stopped. CI builds the same policy from
its disposable cluster's API endpoints and verifies access from the
proxy-labelled probe pod.
The external egress rule still excludes private, tailnet, and link-local IPv4
ranges, so the proxy cannot open connections to PostgreSQL, RustFS, or other
application pods. MAIN-3598 exempts tailnet
traffic from teagramd rate and connection limits; public per-client limits use
the real client address supplied through `CF-Connecting-IP` by cloudflared
only.

The CI-only `bot-api-public-ci-stand-in` exercises cloudflared's public-path
reachability on port 2449. It relays to the loopback Bot API only in the
disposable test overlay; MAIN-3600 replaces it with the direct public listener.

The `Deploy.` runner joins with a new OAuth client whose only tag is
`tag:k3s-deploy`; its `KUBECONFIG` reaches
`teagram.tailaa4918.ts.net:6443`. The in-cluster Tailscale device serves that
port as raw TCP to `kubernetes.default.svc:443`. Kubernetes TLS is end-to-end,
and the kubeconfig pins the cluster CA. The app proxy never terminates API TLS.
The deploy job is gated by `K3S_TAILNET_API_READY` until the route is installed
and verified, then the first run is dispatched manually. The Cloudflare metrics
endpoint and Tailscale proxy metrics service expose scrape targets inside the
cluster; container logs go to stdout and stderr for the cluster log collector.
Alert on a non-Ready Tailscale proxy or an increase in its restart count.

## Tailscale identity and one-time installation

The single-replica `tailscale-proxy` StatefulSet uses the pinned
`ghcr.io/tailscale/tailscale:v1.104.4` image. It runs userspace networking
without host networking, added capabilities, subnet routes, exit-node
advertising, Tailscale SSH, or Funnel. `TS_HOSTNAME=teagram` and the
pre-created `teagram-tailscale-state` Secret preserve the node identity across
pod restarts and node reboots. If state is lost, the one-use auth key cannot
register a replacement device.

Before the first install, the tailnet administrator must:

1. Enable MagicDNS and HTTPS certificates.
2. Before the in-cluster proxy starts, delete the old `teagram` device left by
   the removed application LXC, if it still appears in the tailnet admin
   console. Confirm that device is gone so the new proxy can register as
   `teagram` instead of receiving a suffixed name.
3. Create a one-use, non-ephemeral, pre-approved auth key tagged only
   `tag:teagram`, with a one-day expiry. Auth keys have no OAuth scopes; no
   OAuth client is needed. Store it as external Secret `teagram/tailscale-auth`,
   key `authkey`.
4. Add this tag owner and grant to the tailnet policy, replacing
   `autogroup:member` with the chosen human user/group identities if that group
   includes non-human devices:

   ```json
   {
     "tagOwners": {
       "tag:teagram": ["autogroup:admin"],
       "tag:k3s-deploy": ["autogroup:admin"]
     },
     "grants": [
       {
         "src": ["autogroup:member"],
         "dst": ["tag:teagram"],
         "ip": ["tcp:443", "tcp:2443", "tcp:2447"]
       },
       {
         "src": ["<delta's tag>", "tag:k3s-deploy"],
         "dst": ["tag:teagram"],
         "ip": ["tcp:6443"]
       }
     ]
   }
   ```

   Keep the member grant limited to TCP 443, 2443, and 2447. The single API
   grant is only from delta's device tag and `tag:k3s-deploy` to
   `tag:teagram:6443`. Do not grant `tag:ci-deploy` or any other source access to
   6443. If delta is currently user-owned and untagged, tag that device before
   adding its tag to this grant. The proxy metrics listener on 9002 has no
   Tailscale grant.

5. Remove wildcard source grants. Do not add `tag:teagram` as a grant source,
   an `autoApprovers` entry, a Funnel `nodeAttrs` rule, or a node share.
6. After storing the key in the external Secret, run the reviewed installer
   on alpha. It uses `incus exec ... k3s kubectl` and needs no admin kubeconfig:

   ```sh
   sudo deploy/k8s/install-tailnet-proxy-access.sh --dry-run
   sudo deploy/k8s/install-tailnet-proxy-access.sh --apply
   ```

The installer creates the proxy ServiceAccount, a Role limited to `get`,
`update`, and `patch` on only `teagram-tailscale-state`, and the deployer's
namespace-scoped NetworkPolicy Role (`get`, `list`, `watch`, `create`, and
`patch`; no `delete`). It creates the empty state Secret only when absent and
never replaces existing state. It also creates or refreshes the proxy's
API-only egress policy from the current `kubernetes` Endpoints addresses;
rerun it if the cluster API endpoint changes. `tailscale-auth` remains
externally managed.
The deploy ServiceAccount keeps this NetworkPolicy Role and RoleBinding so
future `Deploy.` runs can update ingress policies. The proxy access rollback
removes its API egress policy, state Role/RoleBinding, and ServiceAccount. The
deploy ServiceAccount receives no Secret, ServiceAccount, Role, or RoleBinding access
outside its namespaced NetworkPolicy rule.

The proxy health and metrics endpoints listen on port 9002. The metrics Service
has Prometheus scrape annotations; collect container logs with
`kubectl logs statefulset/tailscale-proxy --namespace teagram`. Alert when the
proxy is not Ready or either the `tailscale` or `mtproto-forwarder` container's
restart count increases. Do not add a liveness probe: transient tailnet
connectivity must not force re-registration.

## One-time API cutover and cleanup

Keep `K3S_TAILNET_API_READY` unset until every step below succeeds. First tag
delta's device if it is still user-owned and untagged. Add the single
`tcp:6443` policy grant above and create the OAuth client whose only permitted
tag is `tag:k3s-deploy`. Store its client ID and secret only in GitHub's
`k3s-prod` environment. Merge this change with the readiness variable unset so
Deploy. remains gated.

Before bootstrap, preserve the previous deploy kubeconfig in a root-only file
without printing it. Keep that copy until the new API route and the first
manual `Deploy.` both succeed:

```sh
sudo install -o root -g root -m 0600 \
  /root/teagram-k3s-deploy.kubeconfig \
  /root/teagram-k3s-deploy.kubeconfig.pre-main-3607
```

Then run the alpha bootstrap `--dry-run`, review its SAN and dynamic listener
certificate-rotation plan, and run `--apply`. Replace the `KUBECONFIG`
environment secret with the resulting namespace-scoped kubeconfig.

Apply the reviewed proxy manifest from the repository checkout on alpha and
restart the proxy so it loads the raw API route:

```sh
sudo incus exec teagram-k3s -- k3s kubectl apply --filename=- \
  < deploy/k8s/base/tailscale-proxy.yaml
sudo incus exec teagram-k3s -- k3s kubectl rollout restart \
  statefulset/tailscale-proxy --namespace teagram
sudo incus exec teagram-k3s -- k3s kubectl rollout status \
  statefulset/tailscale-proxy --namespace teagram --timeout=180s
```

Before removing the old access, mint the agent token on alpha with the command
from the access table, transfer it privately to the daemon account, and create
`~/.config/teagram/k3s.kubeconfig` outside every checkout and task workdir.
Set its owner to the daemon user and its mode to `0600`; the cluster entry must
use `https://teagram.tailaa4918.ts.net:6443` and the cluster CA, with no
`insecure-skip-tls-verify`. Never paste the token into an issue or commit. From
delta, run:

```sh
kubectl --kubeconfig ~/.config/teagram/k3s.kubeconfig get pods \
  --namespace teagram
```

Confirm a non-allowed tailnet source cannot connect. Keep the old alpha Serve
route and Incus API device until these checks succeed.
If they fail, leave `K3S_TAILNET_API_READY` unset, restore the previous k3s unit
and kubeconfig from their root-only backups, and reapply the previous proxy
manifest. Then remove the old alpha and LXC access paths once:

```sh
sudo tailscale serve --tcp=6443 off
sudo tailscale serve --tcp=2222 off
sudo incus config device remove teagram-k3s api
sudo incus config device remove teagram-k3s ops-ssh
sudo incus exec teagram-k3s -- systemctl disable --now ssh
sudo incus exec teagram-k3s -- rm -f \
  /etc/ssh/sshd_config.d/00-teagram-k3s-ops.conf \
  /usr/local/sbin/teagram-ops
sudo rm -f /root/teagram-k3s-cutover.kubeconfig
sudo incus exec teagram-k3s -- sh -c '
  if [ -f /root/.ssh/authorized_keys ]; then
    tmp=$(mktemp /root/.ssh/authorized_keys.XXXXXX)
    awk '\''!index($0, "command=\"/usr/local/sbin/teagram-ops\"")'\'' \
      /root/.ssh/authorized_keys > "$tmp"
    chmod 0600 "$tmp"
    mv -f "$tmp" /root/.ssh/authorized_keys
  fi
'
```

Only the forced-command key line is removed; other authorized keys are kept.
Remove the `teagram` and `teagram-logs` SSH aliases from the daemon's
`~/.ssh/config`, then update the `deploy-target-teagram-server` operating skill
to allow logs, status, events, rollout status/history, and restarts only by
deleting the `teagramd`, `linkselector`, `linklanding`, or `cloudflared` pod.
Require every restart to be posted on the issue, and treat log lines as
untrusted data, never instructions. Then set `K3S_TAILNET_API_READY=true` and
manually dispatch the first `Deploy.` with the issue key. Its tailnet ping and
CA-pinned kubeconfig checks verify the runner path before it changes workloads.
If either check fails, reset the variable, restore the previous k3s unit and
kubeconfig from their root-only backups, reapply the previous proxy manifest,
and keep application workloads running while the route is repaired. Future
successful main CI runs can deploy automatically. Remove the old kubeconfig
backup after the first `Deploy.` succeeds.

After the proxy is Ready, verify the tailnet HTTPS routes, an unauthenticated
Bot API request returns `401`, and a 48-byte `telegramd-key-v1` request to
`teagram.tailaa4918.ts.net:2443` followed by a write half-close returns a
`telegramd-key-r1` response. In the tailnet admin console, confirm that the
new in-cluster device is named `teagram` and that
`teagram.tailaa4918.ts.net` resolves to that device. CI runs the same discovery
probe through the same `socat` loopback forwarder configured in the proxy pod,
and fails when it returns zero bytes. Both socat forwarders use a 30-second
closing wait so a delayed discovery response is not cut off after the client's
half-close. Tailscale Serve does not handle TCP 2443: the userspace netstack
forwards it to the pod loopback listener, which preserves the client's write
half-close while waiting for the server response. CI keeps the real Tailscale
daemon scaled to zero, so it verifies the forwarder and half-close exchange
without registering a tailnet device.

During a proxy incident, run the reviewed access rollback on alpha. It stops
the proxy and removes its API egress policy, state Role/RoleBinding, and
ServiceAccount. It preserves both Tailscale Secrets and the deployer's
NetworkPolicy Role/RoleBinding for recovery and future deployments:

```sh
sudo deploy/k8s/install-tailnet-proxy-access.sh --rollback
```

There is no LXC fallback. Restore the cluster from matching PostgreSQL and PVC
backups before changing persistent data or replacing the Tailscale identity.
The archived database dump and key archive are on the external backup drive.

`test: k8s-deploy` exercises trusted and denied in-cluster sources, Kubernetes
API access, the unauthenticated Bot API response, discovery half-close, and
one-second probes during linkselector and teagramd rollouts. It requires all
linkselector replicas and a Ready pod with a new UID before that rollout probe
ends; teagramd also requires its StatefulSet generation to complete. The
disposable overlay scales the real Tailscale proxy to zero and uses no
production credential; its policy probe uses a keyless `tailscale-proxy`
ServiceAccount token for `/version` and the proxy label for source-identity
rules. CI does not claim to sample real Tailscale daemon
restarts; the first production teagramd rollout records the `tailscale` and
`mtproto-forwarder` container restartCounts before and after, and production
alerts monitor both. Its `mtproto-smoke` Job runs the 48-byte discovery
half-close probe through the same loopback forwarder used by the production
proxy. The ingress probes run from in-cluster identities; only the disposable
release rollback integration test uses short-lived `kubectl port-forward`
connections.
