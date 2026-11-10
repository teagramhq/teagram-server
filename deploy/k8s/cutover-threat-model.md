# Retired alpha cutover threat model

The former Compose-to-k3s cutover is retired. The source `teagram` LXC was
deleted after its database dump and key archive were copied to the external
backup drive. `cutover-alpha.sh` refuses both apply and rollback; this file is
historical context and contains no operational migration procedure.

The active service routes and their trust boundaries are documented in
[`README.md`](README.md). The database dump, server identity and encryption
keys must be restored from their matching external backups before an existing
installation is brought online. Do not recreate the deleted LXC as an
intermediate step.
