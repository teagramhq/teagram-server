# Self-host Teagram Server

This guide runs `teagramd` as a systemd service on one Linux host, with
PostgreSQL 18 in Docker and Caddy providing HTTPS and WebSocket TLS. It uses the
server's local filesystem media backend. It does not use the project's guarded
deployment Compose stack or its rollout runner.

This is a single-host setup. Keep the host patched, restrict shell and Docker
access to trusted operators, and keep encrypted backups off the host.

## Requirements

- A Linux host with systemd, a public DNS name such as `server.example.net`,
  and enough disk space for the database, uploaded media, and backups.
- [Docker Engine](https://docs.docker.com/engine/install/), Go 1.27.1 or
  newer from the [Go install guide](https://go.dev/doc/install), GNU make,
  Atlas (see the [migration guide](migrations.md)), Git, OpenSSL, and Caddy 2
  from the [Caddy install guide](https://caddyserver.com/docs/install).
- Inbound TCP ports 80 and 443 for ACME certificate issuance and HTTPS/WSS.
  Port 2443 is also needed for clients using the direct MTProto TCP transport.
- A Teagram client configured to use this server. Teagram Web discovers a
  server through its HTTPS discovery document; see the
  [client connection guide](clients.md) for the discovery and enrollment
  details.

Create a public DNS A or AAAA record for the server name. The record must point
to this host before Caddy can issue a public certificate. Replace
`server.example.net` and `https://web.example.net` below with your public server
name and the exact HTTPS origin where your Teagram Web build is served.

## Prepare persistent storage and PostgreSQL

The server keeps its PostgreSQL data, media directory, RSA identity, and
auth-key encryption key across restarts and upgrades. Store the last two keys
outside the source checkout, restrict their permissions, and include them in
every backup.

Create the service account and directories:

```sh
sudo useradd --system --home-dir /var/lib/teagram --create-home --shell /usr/sbin/nologin teagram
sudo install -d -o teagram -g teagram -m 0700 /var/lib/teagram/blobs
sudo install -d -o root -g root -m 0700 /etc/teagram
sudo sh -c 'umask 077; openssl rand -hex 32 > /etc/teagram/postgres.password; openssl rand -hex 32 > /etc/teagram/authkey.hex'
```

Start a persistent Postgres 18 container. The database port is bound only to
loopback, and the password is read from a host file rather than a container
environment variable:

```sh
sudo docker volume create teagram-pgdata
sudo docker run --detach --name teagram-postgres --restart unless-stopped \
  --publish 127.0.0.1:5432:5432 \
  --mount type=volume,src=teagram-pgdata,dst=/var/lib/postgresql \
  --mount type=bind,src=/etc/teagram/postgres.password,dst=/run/secrets/postgres-password,readonly \
  --env POSTGRES_USER=postgres \
  --env POSTGRES_DB=teagram \
  --env POSTGRES_PASSWORD_FILE=/run/secrets/postgres-password \
  --env PGDATA=/var/lib/postgresql/18/docker \
  postgres:18-alpine
```

Wait for Postgres before applying migrations:

```sh
until sudo docker exec teagram-postgres pg_isready -U postgres -d teagram; do sleep 2; done
```

## Build and configure the server

Choose a release tag or a full commit SHA from
[teagram-server](https://github.com/teagramhq/teagram-server), then build that
source revision. If no release tag is available, use a selected full commit
SHA. The repository does not require a project registry or private deployment
credentials.

```sh
sudo git clone https://github.com/teagramhq/teagram-server.git /opt/teagram
cd /opt/teagram
release_ref=FULL_COMMIT_SHA
sudo git checkout "$release_ref"
go build -trimpath -o /tmp/teagramd ./cmd/teagramd
sudo install -d -o root -g root -m 0755 /opt/teagram/bin
sudo install -o root -g root -m 0755 /tmp/teagramd /opt/teagram/bin/teagramd
```

Create `/etc/teagram/teagram.env` as a root-only file. This command reads the
generated secrets into the file without printing them. Replace the two public
origins for your installation:

```sh
sudo sh -c 'set -eu
umask 077
{
  printf "TG_POSTGRES_DSN=postgres://postgres:%s@127.0.0.1:5432/teagram?sslmode=disable\n" "$(cat /etc/teagram/postgres.password)"
  printf "TG_AUTHKEY_ENC_KEY=%s\n" "$(cat /etc/teagram/authkey.hex)"
  printf "%s\n" \
    "TG_RSA_KEY_PATH=/var/lib/teagram/server_key.pem" \
    "TG_BLOB_DIR=/var/lib/teagram/blobs" \
    "TG_PUBLIC_LINK_PREFIX=https://server.example.net/" \
    "TG_LISTEN_ADDR=:2443" \
    "TG_ADVERTISE_ADDR=server.example.net:2443" \
    "TG_WEBSOCKET_LISTEN_ADDR=127.0.0.1:2444" \
    "TG_WEBSOCKET_ALLOWED_ORIGINS=https://web.example.net" \
    "TG_REGISTRATION=open" \
    "TG_LOG_LOGIN_CODES=false"
} > /etc/teagram/teagram.env'
```

No `TG_BLOB_S3_*` variables are set, so uploads use the persistent local
directory in `TG_BLOB_DIR`. Keep `TG_AUTHKEY_ENC_KEY` stable: the server needs
it to decrypt stored authorization keys. Do not enable login-code logging;
username/password sign-in does not use phone codes.

Apply the schema migrations from the checked-out source. Atlas uses Docker for
its temporary validation database, so run it as an account with access to the
Docker daemon:

```sh
sudo sh -c 'set -a; . /etc/teagram/teagram.env; set +a; cd /opt/teagram && make migrate'
```

Create the RSA server identity once. It is stored outside the checkout and must
not be regenerated after users have enrolled the server:

```sh
sudo -u teagram TG_RSA_KEY_PATH=/var/lib/teagram/server_key.pem \
  /opt/teagram/bin/teagramd bootstrap-identity
```

Install `/etc/systemd/system/teagramd.service`:

```ini
[Unit]
Description=Teagram Server
After=network-online.target docker.service
Wants=network-online.target

[Service]
User=teagram
Group=teagram
WorkingDirectory=/opt/teagram
EnvironmentFile=/etc/teagram/teagram.env
ExecStart=/opt/teagram/bin/teagramd
Restart=on-failure
RestartSec=5
TimeoutStopSec=120
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/teagram

[Install]
WantedBy=multi-user.target
```

## Put HTTPS and WSS in front of the server

Caddy obtains and renews a certificate for the public host name. Add this site
to the Caddyfile so that discovery and WebSocket requests reach the server's
loopback-only listener:

```caddyfile
server.example.net {
    @teagram path /apiws /.well-known/teagramd/client /.well-known/telegramd/client
    reverse_proxy @teagram 127.0.0.1:2444
}
```

Load the Caddy configuration, then enable the server:

```sh
sudo systemctl reload caddy
sudo systemctl daemon-reload
sudo systemctl enable --now teagramd
```

The WebSocket listener advertises `wss://server.example.net/apiws` from the
host in `TG_ADVERTISE_ADDR`. Caddy terminates TLS on port 443. Port 2443 is the
separate direct MTProto TCP listener for native clients; MTProto encrypts that
transport, but it is not TLS. Keep the database port private.

Check the service and the public discovery document:

```sh
sudo systemctl is-active teagramd
curl --fail --show-error https://server.example.net/.well-known/telegramd/client
sudo journalctl -u teagramd --since=-10m
```

The discovery document must report the expected server name, DC, and public RSA
key. Caddy must have a valid certificate, and the server must be active before
you enroll a client.

## Create the first account

While `TG_REGISTRATION=open`, a new user can sign up without an invite. Add
`server.example.net` through the Teagram client's server-selection or enrollment
flow. A Teagram Web build must allow its own exact origin in
`TG_WEBSOCKET_ALLOWED_ORIGINS`; for example, use
`http://localhost:8080` when testing from the local Web development server.
The client discovers the server over HTTPS, then connects to its WSS endpoint.

Before opening registration, temporarily restrict inbound TCP 443 and 2443 to
the operator's public IP. This keeps another network user from claiming the
first administrator account. Remove the restriction after the account exists
and registration is closed.

Create an account with a username, display name, and password. The first
account committed to the empty database becomes the server administrator. As
soon as that account is created, change `TG_REGISTRATION` to `closed` in
`/etc/teagram/teagram.env` and restart the service:

```sh
sudo systemctl restart teagramd
```

Use invite-only registration for later users if they should not be able to sign
up freely. Registration mode is checked when each sign-up request reaches the
server.

## What the server stores

The data stays in the storage and log locations that the operator controls:

- PostgreSQL stores usernames, display names, password verifiers, profiles,
  settings, contacts and blocks, sessions, chats, message history, and media
  metadata. Accounts use usernames and passwords; a phone number is not
  required. Passwords are not stored in plaintext. Authorization keys are
  encrypted at rest with `TG_AUTHKEY_ENC_KEY`.
- `TG_BLOB_DIR` stores uploaded photo and document bodies. Keep this directory
  with the matching PostgreSQL backup.
- Secret-chat payloads are client-encrypted; the server still stores relay and
  chat metadata. Ordinary cloud-chat content is stored by the server, so a host
  operator with database access can read it.
- Server, Caddy, and PostgreSQL logs can contain connection, account, and
  operational details. PostgreSQL slow-query logs can include SQL text and bind
  values. Protect and rotate logs as sensitive data.

The server does not set a general expiration period for message history. User
deletions do not remove copies already present in backups, so set and enforce a
retention period for backup and log copies yourself.

## Back up and restore

Stop `teagramd` to make the database and media backup consistent. The database
dump contains application data; the file archive includes the RSA identity,
media bodies, and server configuration, including the encryption key. Keep
these artifacts together, encrypt them before moving them off the host, and
regularly test restoring them on an isolated machine.

```sh
set -eu
backup_dir="/var/backups/teagram/$(date -u +%Y%m%dT%H%M%SZ)"
restart_teagramd() { sudo systemctl start teagramd; }
trap restart_teagramd EXIT
sudo systemctl stop teagramd
sudo sh -c 'set -eu
umask 077
backup_dir="$1"
install -d -m 0700 "$backup_dir"
docker exec teagram-postgres pg_dump -U postgres -d teagram --format=custom > "$backup_dir/database.dump"
tar --numeric-owner -C / -cpf "$backup_dir/files.tar" var/lib/teagram etc/teagram
printf "Backup saved to %s\n" "$backup_dir"' sh "$backup_dir"
sudo systemctl start teagramd
trap - EXIT
```

To restore, stop the server first. Restoring replaces the current database and
server files with the backup and loses any writes made since that backup. Verify
the backup and select the intended restore point before running these commands:

```sh
set -eu
# Replace with the timestamp printed by the backup command above.
backup_dir='/var/backups/teagram/REPLACE_WITH_TIMESTAMP'
sudo systemctl stop teagramd
sudo docker exec teagram-postgres dropdb --if-exists -U postgres teagram
sudo docker exec teagram-postgres createdb -U postgres teagram
sudo sh -c 'docker exec -i teagram-postgres pg_restore -U postgres --exit-on-error --dbname=teagram < "$1"' sh \
  "$backup_dir/database.dump"
sudo tar --numeric-owner -C / -xpf "$backup_dir/files.tar"
sudo chown -R teagram:teagram /var/lib/teagram
sudo systemctl start teagramd
```

After restore, check the service status and HTTPS discovery endpoint as above.
Never delete the Postgres volume as a substitute for restoring a dump.

## Update to a newer release

Use a release tag or a reviewed full commit SHA. Back up the database, media,
identity, and configuration before updating because a release may include
database migrations. Keep the previous source ref and binary until the new
server starts and passes the discovery check.

```sh
set -eu
cd /opt/teagram
backup_dir="/var/backups/teagram/$(date -u +%Y%m%dT%H%M%SZ)"
restart_teagramd=1
restart_teagramd_on_backup_failure() {
  if [ "$restart_teagramd" -eq 1 ]; then
    sudo systemctl start teagramd
  fi
}
trap restart_teagramd_on_backup_failure EXIT
sudo systemctl stop teagramd
sudo sh -c 'set -eu
umask 077
backup_dir="$1"
install -d -m 0700 "$backup_dir"
docker exec teagram-postgres pg_dump -U postgres -d teagram --format=custom > "$backup_dir/database.dump"
tar --numeric-owner -C / -cpf "$backup_dir/files.tar" var/lib/teagram etc/teagram
printf "Backup saved to %s\n" "$backup_dir"' sh "$backup_dir"
# The update keeps teagramd stopped after the backup succeeds.
restart_teagramd=0
sudo cp -p /opt/teagram/bin/teagramd "$backup_dir/teagramd.previous"
sudo sh -c 'cd /opt/teagram && git rev-parse HEAD > "$1"' sh "$backup_dir/source.previous"
sudo git fetch --tags origin
release_ref=FULL_COMMIT_SHA
sudo git checkout "$release_ref"
go build -trimpath -o /tmp/teagramd.new ./cmd/teagramd
sudo sh -c 'set -a; . /etc/teagram/teagram.env; set +a; cd /opt/teagram && make migrate'
sudo install -o root -g root -m 0755 /tmp/teagramd.new /opt/teagram/bin/teagramd
sudo systemctl start teagramd
curl --fail --show-error https://server.example.net/.well-known/telegramd/client
trap - EXIT
```

If the migration or startup fails, stop the service, restore the complete
pre-update database and file backup, reinstall
`$backup_dir/teagramd.previous`, and start the server. Restoring the backup
removes writes made after it was taken. Keep the matching source ref with each
backup set.
