# Threat model: deploy ServiceAccount token

## Scope and assets

This model covers the `teagram-deployer` ServiceAccount in the `teagram`
namespace of the `teagram-k3s` LXC on `alpha`: its long-lived token Secret
`teagram-deployer-token`, the root-only kubeconfig
`/root/teagram-k3s-deploy.kubeconfig`, and the GitHub environment secret that
holds a copy of that kubeconfig. It does not cover the RSA server identity
(see `identity-threat-model.md`) or the forced-command operations menu.

The token is the only credential a deploy job presents to the k3s API. Its
Role grants every verb on every resource in `teagram`, so whoever holds it
can read every Secret in that namespace: the PostgreSQL, RustFS and blob
credentials, the media keys, the Cloudflare tunnel token and the GHCR pull
credential listed in `README.md`.

## Actors and trust

Trusted: the k3s control plane, root on `alpha`, root on the LXC, the Steward
member that installs the kubeconfig into GitHub, and Tech Lead, who approves
rotation. Everything else is untrusted for this purpose: any workflow run that
can read the GitHub environment secret, any repository collaborator who can
change a workflow that uses it, and any tailnet node that can reach the API
port (it still needs the token, but the port is reachable).

## Scope of the token

The Role is `*` on `*` inside `teagram` only. The bootstrap's `can-i` checks
confirm that the token cannot manage cluster-scoped resources or the `default`
namespace. They do not confirm that the token cannot escape the namespace
through a workload.

The namespace has no Pod Security Admission label and no manifest in
`deploy/k8s` mounts a `hostPath`. Nothing stops the token from creating a pod
that mounts the node filesystem or runs privileged. On the LXC that reaches
root, the k3s datastore and every Secret, including Secrets in other
namespaces. This is the largest residual risk in the design. It is not closed
by this change, because the verbs the deploy workflow actually needs are not
yet defined (sibling ticket), and a narrower Role would break that workflow.

Required before the deploy workflow ships or the kubeconfig is stored in
GitHub, whichever comes first: either (a) Tech Lead records acceptance of the
pod-creation risk on the issue, or (b) the namespace gets a Pod Security
Admission label (`baseline` enforce is the candidate, since the manifests use
no `hostPath` and the only added capabilities are `CHOWN` and `FOWNER`),
introduced first in `warn` and `audit` mode and checked against the live
workloads, and the Role is narrowed to the verbs the deploy job uses.

## GitHub secret storage

- The kubeconfig is stored only as a secret of a GitHub environment used by
  the Kubernetes deploy job alone. Do not use the `production` environment,
  which already holds the Compose deploy's secrets and would expose this token
  to that job.
- Restrict that environment's deployment branches to `main`. No
  `pull_request` or fork-triggered workflow may reference it.
- Protect the workflow files that reference the environment with branch
  protection and required review, because anyone who can change such a
  workflow on `main` can read the secret.
- The deploy job writes the kubeconfig to a file with mode `0600`, never
  prints it, and removes it when the job ends. It is never committed, placed
  in an image, uploaded as an artifact, or pasted into an issue or comment.
- The Steward installs the value. Nobody reads it back into a ticket.
- Copies that remain outside GitHub: `/root/teagram-k3s-deploy.kubeconfig` on
  `alpha` (root only), and the `teagram-deployer-token` Secret object in the
  k3s datastore.
- The bootstrap does not enable k3s secrets encryption, so the Secret object
  is plaintext in the datastore on the LXC disk. Enabling encryption at rest
  before production data is written is a follow-up. Changing it later
  requires a datastore rotation, so it is not part of this change.

## Rotation

The token never expires. It is a static `kubernetes.io/service-account-token`
Secret. Rotate it when any of these happens: a suspected exposure, a change in
who runs the Steward or deploy jobs, a compromised deploy job or workflow, or
on a fixed interval (quarterly is the recommendation).

Rotation changes a production credential, so it requires Tech Lead approval on
the issue. Take a backup of the namespace Secrets before starting, and record
the restore path on the issue. The Steward runs it on `alpha`:

1. Confirm no deploy job is running.
2. Delete the Secret `teagram-deployer-token`. The old token stops working at
   once.
3. Remove `/root/teagram-k3s-deploy.kubeconfig`. The bootstrap refuses to
   overwrite a kubeconfig that does not match the cluster.
4. Run `sudo deploy/k8s/bootstrap-alpha.sh --apply`. It recreates the Secret,
   waits for the token and CA, and writes the new kubeconfig.
5. Replace the GitHub environment secret with the new kubeconfig, then check
   the token with `kubectl auth can-i '*' '*' --namespace teagram` and a
   server-side dry run of the deploy manifests.
6. Delete any local copy of the old kubeconfig.

Steps 2 to 5 are the outage window: deploys fail until the new secret is in
GitHub, so run them with no deploy in flight. Restore path: the old token
cannot be restored once deleted. If step 4 fails, rerun it until it succeeds.
The workloads do not use this token (every workload sets
`automountServiceAccountToken: false`), so they keep running through a failed
rotation.

## Residual risk

- A holder of the token can reach node root through a pod, as described above,
  until the namespace is hardened or the risk is accepted on the issue.
- The token does not expire and is valid until its Secret is deleted.
- Secrets sit in plaintext in the k3s datastore until encryption at rest is
  enabled.
- The API port is reachable from every tailnet node, so the token is the only
  control on the API.
