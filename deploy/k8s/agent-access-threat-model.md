# Threat model: agent Kubernetes read access

## Scope and assets

This model covers the `teagram-agent` ServiceAccount, its namespace-only Role
and RoleBinding, the cluster-scoped pod-delete admission policy, PostgreSQL
statement logging, and any TokenRequest token minted for an agent run. It does
not cover API network exposure or the deployer credential; see
`deploy-token-threat-model.md` and the separate API-access work.

Treat the token as held by an attacker. Every agent run on delta can read it,
and agents consume attacker-written text. In particular, pod logs can contain
client-supplied strings, so a prompt-injected agent can spend the token exactly
as an attacker would.

The protected assets are namespace Secrets and what they unlock, login codes
and user data that could reach logs, the integrity of the PostgreSQL and
RustFS volumes, and cluster control. The intended worst case is access to
workload specifications, events, and logs in `teagram`, plus restarts of four
stateless services.

## Token and Role

`teagram-agent` has token automount disabled. Its namespaced Role grants only
get, list, and watch on pods, pod logs, events, services, endpoints,
StatefulSets, Deployments, ReplicaSets, Jobs, and CronJobs; get and list on
ControllerRevisions; and delete on pods. It has no Secret access, pod
subresource access, create, update, or patch permission, or cluster-wide
binding. The deployer ServiceAccount and its permissions are unchanged.

The token is a TokenRequest token with a requested lifetime of 90 days or less.
The Steward mints it with the admin credential inside the LXC. Never create a
`kubernetes.io/service-account-token` Secret for the agent or commit the token.
Deleting and recreating the ServiceAccount invalidates tokens bound to its UID;
mint a replacement after recreation. The external-resource table in
`README.md` records the minting command and expiry limit.

## Logs

The Role can read every pod log, including PostgreSQL. PostgreSQL logs
statements slower than 250 ms. `base/postgresql.conf` therefore sets
`log_parameter_max_length = 0`, preventing bind values such as login codes,
`code_hash` values, and 2FA verifiers from appearing in those statement logs.
This does not remove other personal data from logs: phone numbers and client
addresses may still appear in constraint or connection diagnostics.

Keep `TG_LOG_LOGIN_CODES=false` and cloudflared at `info` in production.

## Restart

RBAC alone cannot constrain which pod a holder deletes or prevent a zero-second
grace period. A cluster-scoped ValidatingAdmissionPolicy with
`failurePolicy: Fail` matches DELETE requests for pods in `teagram` made by
`system:serviceaccount:teagram:teagram-agent`. It admits a request only when
the existing pod has a controller owner reference of kind ReplicaSet or
StatefulSet and its `app.kubernetes.io/name` is `teagramd`, `linkselector`,
`linklanding`, or `cloudflared`. Deletes by other identities, including the
deployer and administrators, do not match this policy. The namespace Role still
does not grant pod deletion to the deployer.

The policy excludes PostgreSQL, RustFS, and Job pods. In particular, deleting a
Job pod could stop a migration midway through a file, and force-deleting a
StatefulSet pod could make two PostgreSQL processes write the same volume.
Deleting teagramd is allowed and a forced deletion can briefly overlap the old
and replacement process against one database.

## Accepted residual risk

- Any process running as the daemon user on delta can use the token. Tailnet
  access identifies devices, not individual agents.
- A token holder can read workload specifications, events, and logs throughout
  `teagram`, including personal data that remains in logs.
- A token holder can repeatedly delete the four allowed service pods and keep
  production unavailable until the token is revoked. A forced teagramd delete
  can briefly run two processes against the same database and double its
  single-replica resource budget for a few seconds.
- k3s has no API audit log, so the cluster does not record which agent deleted
  a pod. Agents should record each restart on its issue and treat log contents
  as untrusted data, never as instructions.
