# Lima deploy SSH threat model

## Scope and trust assumptions

This model covers the CI deployment path to the Lima demo host at
`100.117.107.78:2223`. It protects the host, the root-owned deploy checkout,
the public demo service and its data, and the published web artifact. It does
not protect against compromise of Lima root or Docker, a malicious change
merged to `main`, a compromised deployment runner, or control of the GitHub or
Tailscale administration accounts. Those components can bypass the forced
command and are trusted parts of the deployment system.

The GitHub `demo` environment stores `DEMO_DEPLOY_SSH_KEY` and has a branch
policy for `main`. It currently has no required-reviewer rule. The checked-in
workflow resolves the tested main commit and image digest before using the key;
manual dispatch also selects a successful main CI run. GitHub workflow
permissions and the `main` branch controls remain part of the authorization
boundary. Recheck the environment rules when changing deployment policy.

The same `DEMO_DEPLOY_SSH_KEY` secret is referenced by the Lima demo, dev, and
QA-invite workflows. Treat it as a shared credential: compromise may affect
every host that trusts its public key, and rotation must account for every
consumer.

## Key custody and revocation

The private key is stored in the GitHub secret store, not in the repository or
on Lima. The demo workflow writes it to a mode-0700 runner temporary directory
with a mode-0600 key file, uses pinned `known_hosts` with strict host-key
checking, and removes the temporary directory in an `always()` step. A
compromised runner can still read the key while a deployment job is running.
Lima stores only the public key, in the root-owned mode-0600 file
`/etc/ssh/authorized_keys/teagram-demo-deploy-root`. That file contains the
`restrict,command="/usr/local/sbin/teagram-demo-deploy"` options.

For a planned rotation, pause deployments using the shared secret, create a
replacement Ed25519 keypair in the secret store, and update the public key on
every host that accepts it. The Lima installer deliberately fails if the
existing key file differs from the requested key. From an independent
host-admin session, remove the old Lima key file and rerun the trusted
installer with the new public key. It recreates the file with root ownership
and mode 0600, validates the SSH configuration, and restarts the listener.
Update the GitHub secret and verify the new forced-command path before
resuming deployments.

For suspected exposure, first set `TEAGRAM_DEMO_DEPLOY_ENABLED=false` and use
the independent host-admin path to stop `teagram-demo-deploy-sshd` or remove
the Lima public-key file and restart the service. Replacing the GitHub secret
alone does not revoke a private-key copy already obtained by an attacker.
Replace the shared secret and update or disable all of its other consumers as
part of the same incident response. Do not re-enable deployment until the old
key is absent from every authorized-key file and the replacement key has been
verified.

## Authorized command effects

The dedicated sshd accepts public-key authentication for `root` only, applies
`ForceCommand`, and disables forwarding, tunnels, agent forwarding, X11,
interactive terminals, user environment, and user rc files. The key also has a
forced command. The root wrapper accepts no SSH arguments, checks that the
execution paths are root-owned, validates `SSH_ORIGINAL_COMMAND` against the
fixed command contract, and starts the deploy script with a clean environment.
This prevents an SSH client from requesting a shell or arbitrary host command.
It is not an OS sandbox: accepted deploy operations run with root and Docker
authority.

On Lima, the accepted server command names a 40-character main commit and a
SHA-256 image digest. The forced command rejects commits outside `origin/main`
and the deploy preflight verifies the pinned image. A server rollout backs up
the PostgreSQL database, runs migrations, replaces the server container, and
checks `/healthz`; a failed candidate restores the previous image. The database
dump is kept under `.state/teagram-demo/backups/`, but database restoration is
manual and is not part of image rollback.

The web command supplies the demo revision, web source revision, artifact
digest, archive checksum, and optional rollback run ID. The host verifies the
published artifact identity and checksum, stages the archive, changes the
active web release link, and runs a smoke check. If that check fails, it
restores the previous link. The generic command grammar includes QA-invite
operations, but the Lima forced command rejects them; QA-invite remains pinned
to Charlie.

## Listener isolation

The dedicated OpenRC-managed sshd uses `AddressFamily inet` and listens only on
`100.117.107.78:2223`, the Lima tailnet address. It permits only root with
public-key authentication and `PermitRootLogin forced-commands-only`. The
installer validates the effective configuration, checks that the listener is
not bound to a wildcard address, and verifies the regular sshd configuration
and drop-in hashes are unchanged. This key is not installed in
`/root/.ssh/authorized_keys`; the regular sshd uses its separate configuration.
The CI workflows join Tailscale with `tag:ci-deploy` and use the configured
host key with strict checking.

Tailnet reachability depends on Tailscale ACLs, which are not managed in this
repository. Keep the `ci-deploy` tag limited to the intended hosts and deploy
port. The installer proves that port 22 configuration did not change; it does
not prove that a host-admin credential can still use port 22. Verify an
independent admin path before cutover.

## Recovery and residual risk

If the root listener or key stops working, recover through the independent
port-22 host-admin path or Lima console, not through the deploy key. Validate a
repaired config with `sshd -t -f /etc/ssh/teagram-demo-deploy_sshd_config`,
restart `teagram-demo-deploy-sshd`, and verify the tailnet-only listener and a
permitted command. If the root forced-command design must be rolled back,
restore the previous trusted main checkout and run its installer with the
public key; it reinstates the `teagram-deploy` account, sudo rule, and prior
listener. Test that route before re-enabling deployment. For data recovery,
use the verified PostgreSQL dump; image rollback alone does not undo schema
changes.

The remaining high-impact risks are a stolen shared private key, a compromised
runner or trusted main workflow, and a Tailscale ACL that grants broader
reachability than intended. The command contract limits remote entry points,
but those trusted components can still cause root-level deployment changes,
service downtime, or incompatible migrations.
