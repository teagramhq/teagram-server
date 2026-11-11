# Threat model: deploy ServiceAccount token

## Scope and assets

This model covers the `teagram-deployer` ServiceAccount in the `teagram`
namespace of the `teagram-k3s` LXC on `alpha`: its long-lived token Secret
`teagram-deployer-token`, the root-only kubeconfig
`/root/teagram-k3s-deploy.kubeconfig`, and the GitHub environment secret that
holds a copy of that kubeconfig. It does not cover the RSA server identity
(see `identity-threat-model.md`) or the daemon's separate agent credential.

The token is the only credential a deploy job presents to the k3s API. Its
Role can create, get, list, patch and watch Deployments and StatefulSets,
Jobs, Services and ConfigMaps in `teagram`. It can read ReplicaSets and
ControllerRevisions for rollout history and undo, and read PVCs, Pods, events
and Pod logs. It can delete namespace Pods so the release runner can replace a
stuck StatefulSet Pod after reverting its template. It cannot read or write
Secrets, create Pods, ServiceAccounts, TokenRequests, Roles or RoleBindings.
The production overlay's generated ConfigMaps and workload resources fit this
allowlist.

## Actors and trust

Trusted: the k3s control plane, root on `alpha`, root on the LXC, the Steward
member that installs the kubeconfig into GitHub, and Tech Lead, who approves
rotation. Delta's tagged device and the tagged deploy runner are the only
tailnet sources permitted to reach the API port. Everything else is untrusted
for this purpose: any workflow run that can read the GitHub environment secret
and any repository collaborator who can change a workflow that uses it.

## Scope of the token

The Role is limited to the named workload resources and verbs in
`deployer-access.yaml`; it grants no access to RBAC, ServiceAccounts, token
requests or Secrets. The bootstrap's `can-i` checks confirm that the token has
the permissions needed to apply and inspect workloads while denying
cluster-scoped resources, other namespaces, Pod creation and credential or
RBAC escalation. Because a workload could otherwise use `hostPath` and
privileged mode to reach the node, the namespace enforces Pod Security
Admission's `baseline` profile at Kubernetes `v1.37`. That profile blocks
privileged containers, host namespaces, and `hostPath` volumes while allowing
the current workload manifests. The bootstrap verifies the namespace label
and submits a server-side dry-run of a privileged `hostPath` pod; it fails
unless admission rejects that pod for both violations.
See Kubernetes' [Pod Security Admission](https://kubernetes.io/docs/concepts/security/pod-security-admission/)
and [Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/)
references for the namespace enforcement and baseline rules.

The credential cannot read Secret objects directly, but it can change
workloads whose controllers may mount those Secrets into Pods. This leaves the
accepted namespace data and availability impact of a compromised deploy job.
Pod Security blocks the direct node-root escape described above.

## GitHub secret storage

- The kubeconfig is stored only as a secret of the GitHub environment
  `k3s-prod`, used by the Kubernetes deploy job alone. Do not use the
  `production` environment, which already holds the Compose deploy's secrets
  and would expose this token to that job.
- Restrict `k3s-prod` deployment branches to `main`. No
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
the restore path on the issue. The Steward runs it on `alpha`. Before
rotating, inspect these name-only inventories and compare them with the
expected `default` and `teagram-deployer` ServiceAccounts, the
`teagram-deployer` RoleBinding, and the `teagram-deployer-token` service-account
token Secret. Do not print Secret data:

```sh
sudo incus exec teagram-k3s -- k3s kubectl get serviceaccounts --namespace teagram -o name
sudo incus exec teagram-k3s -- k3s kubectl get rolebindings --namespace teagram -o name
sudo incus exec teagram-k3s -- k3s kubectl get secrets --namespace teagram \
  -o custom-columns=NAME:.metadata.name,TYPE:.type --no-headers
```

During suspected exposure, remove unexpected RoleBindings first, then
unexpected ServiceAccounts and their `kubernetes.io/service-account-token`
Secrets. Re-list and verify only the expected entries remain.

1. Confirm no deploy job is running.
2. Delete the Secret `teagram-deployer-token`, then delete the
   `teagram-deployer` ServiceAccount. Deleting the ServiceAccount invalidates
   TokenRequest tokens bound to it; deleting the Secret invalidates its static
   token.
3. Remove `/root/teagram-k3s-deploy.kubeconfig`. The bootstrap refuses to
   overwrite a kubeconfig that does not match the cluster.
4. Run `sudo deploy/k8s/bootstrap-alpha.sh --apply`. It recreates the Secret,
   ServiceAccount, Role and RoleBinding, waits for the token and CA, and writes
   the new kubeconfig.
5. Replace the GitHub environment secret with the new kubeconfig, then check
   the workload permissions and denied RBAC, Secret and ServiceAccount
   operations with the bootstrap's `kubectl auth can-i` checks, plus a
   server-side dry run of the deploy manifests.
6. Delete any local copy of the old kubeconfig.

Steps 2 to 5 are the outage window: deploys fail until the new secret is in
GitHub, so run them with no deploy in flight. Restore path: the old token
cannot be restored once deleted. If step 4 fails, rerun it until it succeeds.
The workloads do not use this token (every workload sets
`automountServiceAccountToken: false`), so they keep running through a failed
rotation.

## Residual risk

- A holder of the token can change or disrupt production workloads and may
  delete namespace Pods or expose namespace Secrets by mounting them into
  workloads. The token has no direct Secret read or write permission, and
  controllers recreate deleted workload Pods.
- The token does not expire and is valid until its Secret is deleted.
- Secrets sit in plaintext in the k3s datastore until encryption at rest is
  enabled.
- The API port is reachable from two tags (delta's tag and `tag:k3s-deploy`),
  not the whole tailnet.
