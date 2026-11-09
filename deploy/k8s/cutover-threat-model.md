# Threat model: alpha cutover

The cutover moves the existing server identity, database and media from the
Compose LXC to k3s. The trusted boundary includes alpha root, both LXCs, the
k3s control plane and nodes, storage administrators, and principals allowed
to create workloads or exec into pods in the teagram namespace. Compromise of
any of these can expose or replace the copied identity or data. The separate
[identity threat model](identity-threat-model.md) covers key storage and backup.

The `sha-<revision>` image tags are mutable because same-repository pull
requests can publish packages. The cutover treats those tags only as lookup
handles: before fencing source writes, it resolves each tag to a registry
digest and verifies its GitHub build provenance against the exact live source
commit, `refs/heads/main`, and `.github/workflows/ci.yml` as signer. Main CI
emits attestations only on successful main pushes; a PR-produced attestation
cannot satisfy the source and signer checks. A PR image, an un-attested digest,
or provenance for a different revision fails closed before the fence. The
rendered workloads use the verified digest references rather than the mutable
tags.

The runner and recovery commands use the root-owned, mode-0600
`/root/teagram-k3s-cutover.kubeconfig`, which contains the k3s admin client
certificate. It is separate from the restricted `teagram-deployer` token and
must remain on alpha; it is never stored in GitHub or passed to deployment
jobs. A holder can administer the cluster, including reading and changing
Secrets and workloads, so alpha root and the k3s control plane are trusted.

The temporary importer mounts the identity, legacy blobs and blob-mode PVCs
read-write. It runs as root with CHOWN and FOWNER, no service account token,
no privilege escalation, a read-only root filesystem and runtime-default
seccomp. It can read or alter the mounted identity despite file permissions.
Only trusted operators may create or exec into it. The runner deletes it after
copying, on failure and on rollback; an API or node failure can prevent removal,
so operators must inspect and remove any remaining importer before granting
additional namespace access. Its one-day sleep is not a security boundary.

The encrypted S3 copy uses a temporary client with the source blob credential.
It lists and reads the source `teagram/teagramd/` prefix and writes only to the
new RustFS Service. The client joins the source RustFS network and the
teagramd Compose network so it keeps access to RustFS after the source server is
stopped and can reach the target port-forward. The runner checks source and target object counts, byte
totals and per-object SHA-256 values before switching routes, then deletes the
client. The target-only RustFS port-forward is bound to the k3s LXC bridge
address and removed after the copy. The source credential can technically
write or delete objects under its policy, so the copy command and its helper
image are part of the trusted cutover path; source aliases are used only for
listing and reads.

The teagramd ClusterIP Service exposes MTProto, WebSocket and admin ports to
cluster workloads. The cutover also binds port-forwards to the k3s LXC's bridge
address so the source LXC can reach them. These listeners are reachable from
other systems with network access to that address, not only through Tailscale
Serve. Network access to the bridge and cluster must therefore be restricted to
trusted hosts and workloads; the manifests do not enforce a NetworkPolicy or a
host firewall. Bridge HTTP traffic is unencrypted and a network observer can
capture administrative session credentials. WebSocket and MTProto clients must be treated as untrusted, and
the admin endpoint requires its existing authentication and origin controls;
network reachability alone is not authorization. Funnel must remain off, and
operators must verify Serve routes and bridge firewall policy before switching.

The fenced source is retained for rollback. Once any new Serve route is live,
the target may have accepted writes; switching back discards those writes from
the restored source. If route state cannot be verified, recovery fails closed
and requires an explicit rollback decision. Temporary admin credentials and
the dump spool are private files and are removed after use; evidence and the
database dump remain private for recovery. A failed cleanup requires operator
attention and does not guarantee secret deletion from storage or backups.
