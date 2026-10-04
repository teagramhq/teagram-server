# Kubernetes deployment plan

**Plan baseline:** 2026-10-04. This document plans a second deployment target. It does not provision a cluster or change the live deployment.

## Goal and current state

Keep Docker Compose and Kubernetes as supported ways to run `telegramd`, using one application image, the same `TG_*` configuration names, and the same Atlas migration files. Compose remains suitable for local use and a single-host deployment with one replica and local filesystem blobs. Kubernetes is the multi-replica target, with shared database and blob services.

The production instance is currently an Incus LXC on `alpha`, running `postgres`, `migrate`, and one `telegramd` container with `pgdata`, `tgkey`, and `tgblobs` volumes. MTProto is tailnet-only. `TG_LOG_LOGIN_CODES` is enabled there, so this exposure boundary must remain in place. The repository already has an S3-compatible RustFS Compose overlay, a PROXY-v2 parser with an allowlist, and Postgres `LISTEN/NOTIFY` delivery across server instances. The current image is distroless and has no shell or health command.

**Blast radius if this plan is executed incorrectly:** telegram-server clients could lose connections, and database rows, uploaded blobs, or the server identity could diverge or become unavailable.

## Target cluster and prerequisites

**Compose:** Keep the current single-host Compose target and its local Postgres and named volumes. The ordinary local run stays single-replica; no Compose deployment is moved or removed by this plan.

**Kubernetes:** Run at least two `telegramd` replicas on separate worker failure domains. Use shared services outside the application pods for Postgres and blobs. The cluster and its ingress must not depend on one worker or one Tailscale proxy pod.

**Choice and reason:** Recommend a dedicated multi-node cluster for telegram-server. The only kubeconfig contexts available here are `finwire-staging` and `finwire-prod`; the fleet access brief identifies both as single-tenant finwire clusters, and staging is single-node. They are not suitable targets for this service. A new dedicated cluster avoids coupling the server to another product and can meet the failure-domain requirement.

**Open question:** Confirm the target, node capacity, control-plane availability, storage classes, and fleet standard against the fleet wiki before provisioning. The wiki was not exposed by the available Multica CLI, so this plan does not claim that a dedicated cluster already exists. If the wiki identifies an approved shared cluster with isolation and capacity, reassess it before creating a new one.

Complete and verify these prerequisites before a production rollout:

- MAIN-1244: graceful drain and zero-downtime rollout behavior.
- MAIN-1248: 2FA challenges shared across replicas.
- MAIN-1249: per-process limits enforced across replicas.
- MAIN-1250: admin totals and per-replica reporting.

The four issues are not all complete as of this plan baseline. Do not treat their proposed behavior as shipped until their acceptance evidence is available.

## Workloads, health, and rollout

**Compose:** Keep one `telegramd` service by default. After the graceful-drain work lands, Compose should use the same SIGTERM shutdown behavior and a `stop_grace_period` long enough for the measured drain. Its health check should exercise the same application health contract used by Kubernetes.

**Kubernetes:** Use a `Deployment` with at least two replicas, a readiness probe, a liveness probe, and a startup probe if startup duration requires it. Roll out with `maxUnavailable: 0` and `maxSurge: 1` or greater. Add a `PodDisruptionBudget` with at least one available replica, plus required pod anti-affinity or topology spread across workers. A `preStop` hook starts the application drain; `terminationGracePeriodSeconds` must cover the hook, endpoint removal propagation, and measured maximum drain time. The Deployment strategy controls rollout availability; the PDB covers voluntary eviction and is not a substitute for that strategy. See the [Kubernetes probe guidance](https://kubernetes.io/docs/concepts/workloads/pods/probes/) and [disruption guidance](https://kubernetes.io/docs/concepts/workloads/pods/disruptions/).

**Choice and reason:** Add application-owned `/livez`, `/readyz`, and drain behavior before Kubernetes rollout, then use the same health contract in both targets. Drain first marks the instance unready and stops new accepts, finishes in-flight RPCs, flushes pending updates, and closes idle connections over a bounded window before SIGTERM exits the process. MAIN-1244 covers graceful drain; if its implementation does not include health endpoints, add those before starting Kubernetes work. Liveness should report an unrecoverable process failure; readiness should reflect whether the instance and its required database connection can accept traffic. This avoids treating a listening TCP socket as proof that the server is ready and avoids restarts during a recoverable dependency outage.

**Open questions:** Set probe thresholds and drain/grace values from staging measurements. The distroless image has no shell or common probe utilities, so hooks must call application functionality or use HTTP handlers; do not plan a shell-based `preStop` command.

## Image, packaging, and deployment control

**Compose:** Continue to build and run the repository's `Dockerfile`. Keep the default Compose project single-replica, with the RustFS overlay available for S3-backend tests.

**Kubernetes:** Publish immutable images to a registry and deploy by digest. Keep the manifests in the repository as Kustomize base and target overlays. Use Flux pull-based reconciliation for the production overlay. A deployment ticket promotes a tested image digest through a reviewed manifest change; merging application code alone must not deploy it. The ticket continues to carry the `## Changes` changelog.

**Choice and reason:** Choose Kustomize plus Flux. It keeps the deployment declarative and auditable in Git without giving a GitHub Actions runner a long-lived cluster-admin credential. Flux supports Git-based image promotion and rollback; see its [image update workflow](https://fluxcd.io/flux/guides/image-update/). Keep production promotion explicit rather than enabling automatic latest-tag updates.

**Open questions:** Confirm Flux is the fleet standard, choose the registry and retention policy, and define how the deployment ticket links to the image-promotion change. If the selected cluster does not run Flux, use a manually triggered deploy job with a narrowly scoped service account and the same immutable digest and ticket record.

## Postgres and migrations

**Compose:** Keep the local `postgres` service and `pgdata` volume for the default stack. Continue to run the pinned Atlas migration command before starting `telegramd`. Make the application `TG_POSTGRES_DSN` configurable while defaulting it to the local Compose database, so the Compose target can also be used for a rollback against the shared production database.

**Kubernetes:** Use a dedicated external or operator-managed Postgres service, not a database container in each application pod. Use a direct session-mode connection for the server's `LISTEN/NOTIFY` listener; transaction pooling is not compatible. Add automated backups, WAL archiving, and point-in-time recovery to storage outside the application cluster. Run Atlas migrations as a versioned Job before the Deployment rolls out. Only one Job applies a migration set; never have every replica migrate on startup. Require expand/contract migrations so the old and new binaries can overlap and the LXC can remain a rollback target.

**Choice and reason:** Prefer a dedicated external Postgres endpoint if the fleet has one that supports direct sessions and the required recovery objectives. It separates database availability from application-cluster failures and allows the Compose rollback target to use the same database. CloudNativePG in a dedicated cluster is the fallback if it meets the same backup, restore, and connectivity requirements.

**Open questions:** Select the service, Postgres version, connection limits, RPO/RTO, backup retention, and PITR restore procedure. Prove `LISTEN/NOTIFY` through the selected endpoint. Measure restore time before choosing between a bounded maintenance window and database replication for cutover.

## Uploaded files

**Compose:** Keep `TG_BLOB_DIR` on the existing `tgblobs` named volume by default. Keep `docker-compose.rustfs.yml` as the opt-in local S3-compatible test path.

**Kubernetes:** Configure `TG_BLOB_S3_*` to use one private S3-compatible bucket and a service-specific prefix. Do not put uploaded bytes on pod-local storage or rely on a volume shared between pods. Keep the object store outside the application worker failure domain.

**Choice and reason:** Use S3-compatible storage in Kubernetes because every replica must read the same uploaded blobs, while single-replica Compose can continue to use local files. The existing RustFS overlay gives CI and developers a local test endpoint without making RustFS the production durability layer.

**Open questions:** Select the production object store, endpoint, region, retention, lifecycle rules, and recovery process. Pre-copy the current `tgblobs` data and verify object counts and checksums before cutover; estimate the final write freeze from measured data volume.

## Secrets and durable identity

**Compose:** Keep the current `tgkey` volume and the existing environment/file configuration. The RSA key and auth-key encryption key must survive container replacement. `.env` remains local-only and must not enter Git.

**Kubernetes:** Store `TG_AUTHKEY_ENC_KEY`, the existing server RSA private key, database credentials, S3 credentials, and admin token material in encrypted Kubernetes Secrets managed through SOPS or the fleet secret store. Mount file-form secrets read-only where the application supports file settings. Give every replica the same existing RSA key and auth-key encryption key; never generate replacement keys during migration.

**Choice and reason:** Preserve the current key bytes and server fingerprint across both deployment targets. A replica needs the same decryption key and server identity to serve the same Postgres state. Secret encryption and narrowly scoped access keep those values out of manifests, image layers, logs, and CI output.

**Open questions:** Choose SOPS or the fleet secret store, define who can decrypt the source values, and rehearse transfer without exposing them. Record the old and new server-key fingerprints for comparison, but never record key material.

## Network paths and exposure

**Compose:** Keep MTProto bound to the existing tailnet address on the LXC. Preserve loopback-only local ports and the untracked live override behavior. WebSocket and admin access remain operator-only.

**Kubernetes:** Expose MTProto TCP `:2443` through a tailnet-only Tailscale operator service, using a highly available proxy group. Put a PROXY-v2-capable TCP balancer between that service and `telegramd` when needed to preserve the source address. Expose WebSocket `:2444` and admin `:2445` through separate tailnet-only HTTP routes. Keep the admin listener authenticated and private. Do not expose any listener publicly while login codes are written to logs or public exposure remains undecided. Tailscale documents Layer 3 TCP service exposure and recommends high availability for production ingress proxies in its [Kubernetes ingress guidance](https://tailscale.com/docs/kubernetes-operator/ingress).

The server already accepts PROXY-v2 only from configured balancer addresses. Configure `TG_CLIENT_ADDR_TRUST=proxy-v2` and restrict `TG_CLIENT_ADDR_PROXY_CIDRS` to the exact, stable balancer addresses when that balancer emits v2 headers. Do not trust a broad pod or node subnet. If the tailnet path preserves the original source address directly and no proxy emits a header, retain socket-address mode instead of expecting a header that is not sent.

**Choice and reason:** Keep the current tailnet-only boundary and preserve the real client address used by per-IP limits. Use PROXY-v2 only at a hop that actually emits a valid v2 header; the trust configuration fails closed if it is missing or comes from an untrusted peer.

**Open questions:** In staging, verify the source address seen at the TCP balancer and by `telegramd` through the selected Tailscale operator service. Confirm that PROXY-v2 headers come from stable, individually allowlisted addresses; otherwise prove that the source address survives directly and use socket mode. Also confirm how the stable tailnet DNS name will move at cutover and which tailnet ACLs protect the three listeners.

## CI, logs, metrics, and alerts

**Compose:** Retain the existing Compose stack smoke checks for the local filesystem backend and RustFS overlay. They verify startup, migrations, and blob-backend behavior on a single replica.

**Kubernetes:** Add a kind or k3d smoke job with a multi-node cluster, temporary Postgres and S3-compatible services, and two `telegramd` replicas. Build the image once, run the Compose and Kubernetes smoke paths against that same image digest, and publish that digest only after the checks pass. The Kubernetes smoke should cover readiness, cross-replica delivery, rolling update, pod termination, node drain, and PROXY-v2 behavior using a test balancer. Staging must separately verify the actual Tailscale ingress and tailnet ACLs.

Collect container logs from stdout/stderr with pod and replica identity, and never enable login-code logging in Kubernetes. Add alerts for no ready replicas, stalled rollouts, repeated process restarts, Postgres connection or backup/PITR failures, S3 operation failures, and failed MTProto RPCs or connection attempts. The existing admin metrics are authenticated and per replica; `docs/observability.md` says there is no fleet collector, and MAIN-1250 is a prerequisite. Do not claim fleet totals until that work is complete. Production needs an internal collection path and a dashboard/alert for user-visible symptoms before cutover.

**Choice and reason:** Keep Compose smoke as the fast single-host check, and add a multi-node kind/k3d check for Kubernetes behavior. Testing one immutable image across both targets catches differences in environment wiring without maintaining a second image.

**Open questions:** Select the fleet log and metrics backend, confirm whether the admin JSON can be collected safely or needs a private scrape endpoint, and set alert thresholds from staging baselines. CI must not require production credentials or apply production manifests.

## Cutover, verification, and rollback

**Compose:** Keep the LXC image, Compose configuration, key material, and a tested external-database/S3 override available as the rollback target for one week after cutover. Keep the current deployment ticket and `## Changes` record.

**Kubernetes:** Validate rendered Kustomize output before any apply. First deploy to an isolated staging or preview cluster, then run restore and failure drills. Production cutover runs both targets alongside only for validation on isolated data; do not allow two writable production servers on independent databases or blobs. Drain the LXC, perform the measured final database and blob sync (or approved replication), start Kubernetes, verify the service, and move the stable tailnet endpoint only after the acceptance checks pass.

**Choice and reason:** Start with a measured maintenance window for the final data copy. Use replication only if the rehearsal exceeds the agreed downtime budget. Keep the LXC for seven days and point it to the same Postgres/S3 state if rollback is needed; expand/contract schema changes preserve compatibility during that window. This makes rollback an image and traffic switch rather than a restore of stale local state.

**Open questions:** Set the downtime budget and RPO/RTO with measured data. Prove the LXC can reach the shared Postgres and object store before cutover. Define the exact tailnet name switch and the restore path for each backup; record the verified backup location on the deployment ticket.

**Rollback:** Before cutover, record the currently serving image digest, database backup/PITR point, blob backup/sync state, and key fingerprints. If the Kubernetes rollout fails, stop routing new clients to it and switch the tailnet endpoint back to the LXC only after draining Kubernetes, so there is one writer. If the database or blob state is damaged, restore from the verified backup/PITR procedure before routing traffic. After the first week, retire the LXC only in a separate deployment ticket.

**Acceptance gates before production cutover:**

- E2E clients hold sessions mid-chat and mid-upload through a two-replica rolling update, with zero failed RPCs, lost or duplicated updates, or incomplete upload resumes.
- A pod termination and a worker-node drain complete with no user-visible disruption.
- A restore drill meets the chosen RPO/RTO and verifies database rows, uploaded blobs, and the unchanged server fingerprint.
- A rollback to the LXC is rehearsed against the same Postgres and object-store state, with one writer at all times.
- Alerts detect no-ready-replica, failed rollout, database recovery, blob storage, and user-facing connection failures.

## Ordered implementation work

Rough sizes use engineer time: **S** is 1–2 days, **M** is 3–5 days, and **L** is 1–2 weeks. Estimates exclude waiting for provider or fleet decisions.

| Order | Work | Size | Verification gate |
|---|---|---:|---|
| 1 | Finish MAIN-1244, MAIN-1248, MAIN-1249, and MAIN-1250; confirm acceptance evidence. | M–L | Existing per-issue rollout, cross-replica login/limit, drain, and admin tests pass. |
| 2 | Confirm the dedicated cluster or approved isolated target, registry, Postgres, object store, secret store, tailnet ingress, monitoring, and recovery objectives. | S | Architecture decision recorded before provisioning. |
| 3 | Make Compose accept the same application settings with a local default and an external Postgres/S3 mode for rollback. | M | Existing Compose filesystem and RustFS smoke paths pass; LXC rollback configuration is rendered and reviewed without applying it. |
| 4 | Add readiness, liveness, and drain behavior shared by Compose and Kubernetes. Measure probe and drain thresholds. | M | Compose stop and multi-replica termination tests show no refused RPCs. |
| 5 | Build and publish immutable images; add Kustomize base/overlays, Flux reconciliation, encrypted secrets, migration Job ordering, and network policies. | L | `kustomize build`, Flux dry-run/reconciliation in staging, and secret scan pass. |
| 6 | Add multi-node kind/k3d CI coverage and production monitoring, logs, dashboards, and alerts. | M–L | Compose and Kubernetes smoke use the same digest; rolling update, replica loss, node drain, and S3 paths pass. |
| 7 | Rehearse backups, PITR restore, key preservation, blob copy, and LXC rollback on isolated data; measure the final migration window. | M | Restore and rollback results meet agreed RPO/RTO with matching key fingerprint and verified blob checksums. |
| 8 | Deploy to staging, then perform the production cutover under a dedicated deployment ticket; keep the LXC rollback path for one week. | L | All production acceptance gates above pass; ticket records deployed digest, `## Changes`, and backup/restore paths. |

No production provisioning, manifests, deploy changes, or data migration belong to this planning change. Each implementation step should ship in its own reviewed change and use a rendered diff plus staging evidence before production.
