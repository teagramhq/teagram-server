# Threat model: S3 media encryption

## Scope and trust boundary

This model covers uploaded file parts and assembled media when `teagramd` uses
the S3 backend. Production uses the in-cluster RustFS service on k3s; current
storage and backup procedures are in the
[k3s deployment guide](../deploy/k8s/README.md).
The former Compose-era production transition and its mode-publication procedure
are retired. This describes application behavior, not the mode of the live
object corpus, which is deployment state. It does not make cloud-chat media
end-to-end encrypted: the server receives and serves plaintext for authorized
requests, and PostgreSQL continues to hold file and message metadata.

The protected assets are media bytes, the media key ring, and the S3 credentials
scoped to the Teagram bucket prefix. The k3s nodes and administrators, running
`teagramd` and PostgreSQL pods, RustFS, and operators who can read the external
Kubernetes secrets are trusted. A compromise of those components can reach
plaintext or keys. The object store is not trusted to preserve object
confidentiality or integrity; its availability is still required to serve
media.

## Key custody

Each media-key version is a separate 32-byte key-encryption key in a
`<version>.key` file. `TG_MEDIA_KEY_DIR` names the directory and
`TG_MEDIA_KEY_CURRENT` selects the version used for new objects; neither
setting contains key bytes. The key is provisioned outside the repository,
separately from both the S3 credential and `TG_AUTHKEY_ENC_KEY`, and a key equal
to the auth-key key is rejected. The production StatefulSet mounts the
externally supplied media-key Secret read-only at `/run/teagramd/media-keys`;
`TG_MEDIA_KEY_CURRENT` comes from the runtime ConfigMap. The loader rejects
directories and files writable by group or others. See
[S3 setup](clients.md#object-store-backend) for local development and the
[k3s deployment guide](../deploy/k8s/README.md) for production resources.

The server loads every version in the directory into its in-memory key ring.
It writes with the selected current version and needs older versions to read
objects wrapped under them. The ring's string, JSON, and log representations
expose versions but not key bytes. This is file-based key custody; the process
and its host can access the keys. Protect key backups separately from S3
credentials and object backups. Losing a version makes objects that use it
unreadable; removing a version before its objects are migrated has the same
effect. Do not retire a version until an inventory confirms no live objects
depend on it.

## Key rotation

`blob-key-rotate` requires encrypted blob-mode authority and always writes with
`TG_MEDIA_KEY_CURRENT`. Its default mode rewraps each referenced object's DEK
under the current version and copies the authenticated chunk bytes unchanged.
Use `--rewrite` after a wrapping-key leak, with a fresh media key version
already selected as current; it assigns every referenced object a new DEK and
rewrites its chunks. Both modes include assembled files and live upload parts,
verify each replacement by authenticated read-back, and keep a private progress
journal so an interrupted run can resume.

The `blob-key-rotate` binary supports `--target-version N`, `--rewrite`, and
`--progress PATH`. Its former Compose migration profile and host-volume journal
instructions describe the retired production stack; do not use them to rotate
the current production store. This repository's current production guide covers
RustFS storage and backups, not a live key-rotation procedure.

Run `blob-inventory` after rotation. Remove an old `<version>.key` only when
the inventory reports that version retirable, and run the rotation again after
any writer that still used the old current version has switched. A concurrent
old-version upload appears in the next inventory and is handled by another
rotation pass.

## Object-store access and tampering

For each `Put`, the server generates a fresh random data key and salt. It wraps
the data key with HKDF-SHA256 and AES-256-GCM under the current media key, then
seals the object in AES-256-GCM chunks. Authentication binds the fixed header,
object key, chunk index, and final-chunk marker. Moving an object to another
key, changing its header or ciphertext, or reordering chunks makes a read of
that data fail authentication. A read reaching the final chunk also detects a
truncated or extended object. After the normal message-row authorization
check, the client receives a generic internal error for a corrupt envelope
rather than the bucket or object details. Encryption does not replace that
authorization check: `upload.getFile` still requires an owned, non-deleted
message row that references the file; an `(id, access_hash)` pair alone is not
a capability.

The header is not encrypted. An object-store reader can learn its key version,
logical length and chunk size, as well as object keys, physical sizes, and
access patterns. Range reads authenticate the header and the chunks they
return; they do not validate unrelated chunks elsewhere in the object. The
envelope does not prove freshness: replaying a previously valid envelope at
the same object key can return its old contents. A store operator can also
delete or withhold objects. Authentication detects many integrity attacks but
does not prevent rollback or denial of service.

The S3 credential alone does not decrypt fresh encrypted objects. It can still
read, replace, or delete objects within its permitted prefix, affecting
availability and triggering authentication failures. Keep the bucket private
and the credential restricted to that prefix. HTTPS certificate verification
is enabled for external S3 endpoints. Production RustFS uses HTTP on an
internal cluster service with no public ingress; the cluster network and its
administrators are trusted. An observer on that network can see S3 requests and
metadata. The envelope protects ciphertext against undetected object-body
modification, not the S3 request channel or service availability.

## Legacy plaintext compatibility

The server retains a compatibility mode for older plaintext S3 objects. A
persisted mode record selects whether the S3 store reads and writes raw objects
or the encrypted envelope; media-key presence alone does not prove that the
existing corpus is encrypted. The current k3s pod mounts
`/run/teagramd/blob-mode` read-only, so its contents are runtime state and
cannot be inferred from the manifests or this repository. The old Compose-era
publisher and cutover instructions are retired; do not edit or publish these
records manually.

When a persisted record selects the legacy plaintext mode, an object-store
reader or network observer can read media bytes. A transition to encrypted
serving mode requires verified encrypted copies before the mode record changes.
Fresh S3 stores without a plaintext-mode record require the media key ring and
fail startup if it is missing; the server does not silently select plaintext
mode. See the [k3s deployment guide](../deploy/k8s/README.md) for the current
production storage and backup topology.

## Residual risk

The envelope does not hide object sizes or access patterns, protect plaintext
inside the server, provide end-to-end encryption, prevent deletion or
same-key rollback, or replace protected key and media backups. The RustFS HTTP
path relies on the private cluster network. The active object-mode record and
media-key material are deployment state; a repository review cannot prove the
mode or encryption coverage of the live object corpus.
