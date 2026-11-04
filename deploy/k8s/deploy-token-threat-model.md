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
namespace. Because a workload could otherwise use `hostPath` and privileged
mode to reach the node, the namespace enforces Pod Security Admission's
`baseline` profile at Kubernetes `v1.37`. That profile blocks privileged
containers, host namespaces, and `hostPath` volumes while allowing the current
workload manifests. The bootstrap verifies the namespace label and submits a
server-side dry-run of a privileged `hostPath` pod as the deploy ServiceAccount;
it fails unless admission rejects that pod for both violations.
See Kubernetes' [Pod Security Admission](https://kubernetes.io/docs/concepts/security/pod-security-admission/)
and [Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/)
references for the namespace enforcement and baseline rules.

The credential can still read every Secret in `teagram` and change any
namespace workload. The profile prevents the direct node-root escape described
above; it does not reduce the namespace-wide data and availability impact of a
compromised deploy job.

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

- A holder of the token can read namespace Secrets and change or disrupt
  production workloads.
- The token does not expire and is valid until its Secret is deleted.
- Secrets sit in plaintext in the k3s datastore until encryption at rest is
  enabled.
- The API port is reachable from every tailnet node, so the token is the only
  control on the API.
