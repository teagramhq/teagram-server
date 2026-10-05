# Confined poll lifecycle probe

`pollprobe` is a bounded operator tool for checking the server's poll lifecycle
with four pre-provisioned synthetic accounts. It does not sign up or delete
accounts, accept arbitrary peers, or repeat its scenario automatically. Each
invocation creates one group containing `synthpoll_a`, `synthpoll_b`, and
`synthpoll_c`, plus its poll and text-message fixtures. It uses
`synthpoll_d` only as an outside-group account. The probe closes the polls it
creates on successful completion and logs out every session it opens; it does not
delete the group or messages. A failed assertion halts later scenario writes,
so objects created before a failure can remain for inspection.

Build the command:

```bash
go build -o pollprobe ./cmd/pollprobe
```

The operator must provision these existing accounts and their passwords first:

```text
synthpoll_a.password
synthpoll_b.password
synthpoll_c.password
synthpoll_d.password
```

Place those files in a directory owned by the invoking user with mode `0700`.
Each file must be owned by that user, readable only by that user (`0400` or
`0600`), and contain one non-empty password line. The directory must contain
exactly those four files. Supply the passwords from a secret manager or another
protected local process; do not put them in command arguments or environment
variables.

Copy the server public key into a separate public-key PEM file. For a PKCS#1
server key, OpenSSL can export its public half:

```bash
openssl rsa -in server_key.pem -pubout -out server_pub.pem
```

Use the server's logged `key_id` as the full SHA-256 SPKI pin. Verify the key
and key ID through a trusted channel separate from the endpoint connection.
The probe checks that pin before connecting.

Run the fixed scenario with the server endpoint, key file, key ID, and
credential directory:

```bash
./pollprobe \
  --endpoint mtproto.example.com:2443 \
  --rsa-public-key ./server_pub.pem \
  --rsa-key-id 'a1b2-c3d4-e5f6-a7b8-0000-1111-2222-3333-4444-5555-6666-7777-8888-9999-aaaa-bbbb' \
  --credentials-dir "$HOME/pollprobe-credentials"
```

The key ID above is only an example; replace it with the full value logged by
the server. Output contains assertion names, counts, and sanitized RPC error
codes. The scenario has a three-minute deadline, stops scenario writes on the
first failure, and then makes a bounded best-effort logout attempt for every
session it authorized.
