# Threat model: Kubernetes server identity

## Scope and assets

This model covers the long-lived RSA identity used by the Kubernetes
`teagramd` workload for MTProto authentication, including its first-install
bootstrap, storage, backup, restore, and replacement. The private key is
`server_key.pem` on the `key-data-teagramd-0` PVC. Clients that pin the
corresponding public identity use it to decide which server they trust, so an
unauthorized replacement can break client trust, and a stolen private key can
be used to impersonate the server.

The same PVC also stores `enc_key.hex`, the master key used to encrypt stored
authorization keys. It is distinct from the RSA identity, but both files are
required for a recoverable existing installation: the auth-key encryption key
must match the PostgreSQL state whose authorization keys it decrypts. The
public fingerprint and key ID are not secrets; the private key and auth-key
encryption key are.

The cluster control plane, namespace operators who can alter workloads or
access the PVC, node and storage administrators, and the running `teagramd`
process are trusted. A principal that can read or replace the PVC can steal or
change the server identity. This model does not protect against compromise of
those trusted components, denial of service from lost storage, or malicious
changes to the application image.

## Generation and client trust

The server does not create an RSA key during normal startup. On a fresh
installation, the operator explicitly runs the `teagramd-identity-bootstrap`
Job from `bootstrap/` after `key-data-teagramd-0` is bound and the StatefulSet
pod is stopped. `teagramd bootstrap-identity` generates a 2048-bit PKCS#1 PEM
key with mode `0600`, publishes the complete file atomically, and refuses to
overwrite an existing path. A missing or invalid key makes normal startup fail
closed. The bootstrap Job is a create-once operation, not a repair or rotation
mechanism.

The Job prints the public fingerprint, not the private key. Record the
fingerprint and public key ID through a trusted deployment channel and compare
them with the identity shown during client enrollment. Do not copy the private
PEM into Git, an image, a CI artifact, or a support log. CI bootstraps only its
disposable k3s PVC; that identity must never be promoted to production.

## PVC access and controls

`key-data-teagramd-0` is a 1 GiB `ReadWriteOnce` claim using `local-path`. The
StatefulSet and the explicit bootstrap Job both mount it read-write at
`/var/lib/teagramd`. The application and Job run as UID/GID `65532`, with no
Linux capabilities, a runtime-default seccomp profile, and a read-only
container root filesystem. The directory-preparation init container runs as
root with only `CHOWN` and `FOWNER` capabilities to set the volume ownership
and permissions. `automountServiceAccountToken: false` prevents these pods
from using a mounted Kubernetes API token.

These settings reduce accidental access from the container, but they do not
make the PVC read-only or encrypt its contents. File mode `0600` protects
against other unprivileged processes on the mounted filesystem; it does not
protect against a privileged pod, a principal with pod exec or workload
mutation access, node root, a storage administrator, or someone who can read a
volume snapshot. The manifests do not configure or verify storage encryption
at rest. Restrict RBAC for `pods/exec`, ephemeral containers, workload and Job
mutation, PVC access, and storage snapshots to trusted operators. Apply the
cluster's storage encryption and snapshot access controls separately.

## Backup and restore

Back up the `key-data` PVC and PostgreSQL as one recoverable set. Stop
`teagramd` before a coordinated backup so it cannot create or use keys while
the set is captured, and use a database-consistent PostgreSQL backup rather
than treating a live database volume copy as sufficient. Keep the key files
and database backup from the same recovery point. Encrypt backups in transit
and at rest, restrict access to the smallest trusted operator group, and keep
them off the cluster. Store backup-encryption keys separately from the
encrypted backups and restrict access to them independently.

For recovery, restore the matching key-data and PostgreSQL set before starting
`teagramd`. Verify that the restored RSA fingerprint and key ID match the
recorded client identity, then verify normal startup and authorization-key
decryption before reopening client traffic. Restoring PostgreSQL without the
matching `enc_key.hex` can make stored authorization keys undecryptable;
restoring only the PVC can pair keys with incompatible database state. A
restore to an older recovery point can also discard later database writes.

## Replacement and residual risk

If the key is missing or damaged on an established installation, restore the
known-good PVC and matching database backup. Do not delete the claim to force
bootstrap, and do not rerun the create-once Job as a repair. The current
manifests do not provide an automated key replacement procedure.

If the prior identity cannot be recovered, or compromise requires retiring
it, treat replacement as a server identity migration. Create a new key only
through a separately planned bootstrap on empty storage, preserve the
PostgreSQL state and matching `enc_key.hex` only when that state is still
trusted, and distribute the new fingerprint through an authenticated client
enrollment channel. Clients pinned to the previous public key will not
silently trust the replacement. Replacing the RSA key alone does not rotate
the auth-key encryption key or revoke existing authorization keys. Keep the
old encrypted backup available for a defined rollback period; if the old key
was compromised, do not restore it as a rollback identity. Restrict that
backup and dispose of it under the incident retention policy after cutover.

The `local-path` claim is a single-node persistence choice, not a cross-node
recovery guarantee. Loss of its node or storage can stop the service until the
PVC and matching database backup are restored. File permissions and image
digest pins do not protect secrets from cluster or node administrators; those
roles remain inside the trust boundary.
