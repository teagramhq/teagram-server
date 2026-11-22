# Production deployment

Production runs on the single-node `teagram-k3s` cluster. Releases use the
GitHub Actions `Deploy.` workflow (`.github/workflows/deploy.yml`); there is no
Compose-based production deploy command.

The workflow deploys only an image revision whose CI succeeded. It verifies the
candidate images, saves a pre-deploy PostgreSQL backup, server-side validates
and applies the release, runs the migration Job, waits for rollout, and runs
smoke checks. A failed rollout triggers an automatic undo. Read
[`../k8s/README.md`](../k8s/README.md) for the cluster, recovery, and manual
operations runbook.

The old Compose LXC deploy entrypoint and rollout runner have been retired.
The root `docker-compose.yml` remains for local development; the demo keeps its
own Compose stack. Neither is a production deployment path.
