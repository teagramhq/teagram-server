# Threat model: S3 media encryption

## Scope and trust boundary

This model covers uploaded file parts and assembled media when `teagramd` uses
the S3 backend. It describes the application envelope added for fresh S3
stores. It does not make cloud-chat media end-to-end encrypted: the server
receives and serves plaintext for authorized requests, and PostgreSQL continues
to hold file and message metadata.

The protected assets are media bytes, the media key ring, and the S3 credentials
scoped to the Teagram bucket prefix. The host, kernel, Docker daemon, running
`teagramd` process, PostgreSQL, and operators who can read the media-key files
are trusted. A compromise of those components can reach plaintext or keys. The
object store is not trusted to preserve object confidentiality or integrity;
its availability is still required to serve media.

## Key custody

Each media-key version is a separate 32-byte key-encryption key in a
`<version>.key` file. `TG_MEDIA_KEY_DIR` names the directory and
`TG_MEDIA_KEY_CURRENT` selects the version used for new objects; neither
setting contains key bytes. The key is provisioned outside the repository,
separately from both the S3 credential and `TG_AUTHKEY_ENC_KEY`, and a key equal
to the auth-key key is rejected. The default Compose deployment bind-mounts the
host directory read-only at `/run/teagramd/media-keys`; its documented host
permissions are directory mode `0700` and key-file mode `0600`, owned by the
server UID. The loader rejects directories and files writable by group or
others. See [S3 setup](clients.md#object-store-backend) for provisioning.

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

Run it from the deployment checkout through the one-shot migration profile:

```sh
docker compose --profile migration run --rm blob-key-rotate
```

The image includes `blob-key-rotate`; the service reads the encrypted authority
and media keys read-only and persists its journal under
`/run/teagramd/reports/blob-key-rotation.jsonl` in the dedicated Compose volume
`teagram_blob_key_rotate_reports`. Find the volume's host mountpoint with
`docker volume inspect --format '{{ .Mountpoint }}' teagram_blob_key_rotate_reports`
to read the journal. Optional flags are
`--target-version N`, `--rewrite`, and `--progress PATH`. If omitted, the target
is the configured current version and the journal uses that default path.

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
is enabled for external S3 endpoints. The default RustFS Compose endpoint uses
HTTP on an isolated private Docker network with no published RustFS ports; that
network and its administrators are trusted. An observer on it can see S3
requests and metadata. The envelope protects ciphertext against undetected
object-body modification, not the S3 request channel or service availability.

## Accepted-plaintext interval

The durable `s3-accepted` blob-mode authority is a compatibility state for the
existing plaintext RustFS corpus. While that authority is active,
`teagramd` deliberately uses the raw S3 store even if media keys are present:
existing objects remain readable and new uploads also remain plaintext. The
server logs a warning at startup. The live cutover recorded by MAIN-2395 is in
this state.

The `s3-accepted` record is not evidence that objects are encrypted. During
this interval, anyone who can read the RustFS objects or observe their
plaintext transfer can read media. This PR does not re-encrypt that corpus or
shorten the interval. A separate reviewed migration must rewrite and verify
the objects and publish the encrypted authority before the at-rest envelope
protection applies to that data. A fresh S3 deployment without an accepted
plaintext authority requires the media key ring and fails startup if it is
missing; it does not silently select plaintext mode. Compose may materialize
the authority bind source as an empty directory on first boot. The server
treats that directory as no authority only for S3. Any nonempty authority tree
is validated normally, so an existing `s3-accepted` record continues to select
plaintext until the separate migration publishes a new authority. With no
authority, the encrypted mode remains selected on later starts and never falls
back to plaintext; an authority-free deployment should use a fresh namespace,
since existing plaintext objects will fail envelope authentication on read.

## Residual risk

The envelope does not hide object sizes or access patterns, protect plaintext
inside the server, provide end-to-end encryption, prevent deletion or
same-key rollback, or replace protected key and media backups. The default
RustFS HTTP path relies on the private Docker network. Running key rotation on
a live host and migrating the accepted plaintext corpus remain separate
operational work.
