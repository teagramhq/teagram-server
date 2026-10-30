# Isolated link edge

This Compose project runs the link selector and static landing page beside the
separately managed Telegram Web container. It does not use the server stack's
Compose file. The selector reaches Web through `teagram-web-edge-net` as
`web:8080` and reaches landing over the private `private-edge` bridge. Only the
selector and landing containers publish ports, both on host loopback. The
landing container also joins a dedicated bridge for its published port; that
bridge has IP masquerading disabled and no other service attached. The project
has no secrets, volumes, or server-service dependency. The landing page needs
the public HTTPS sign-up destination in `TG_INVITE_WEB_SIGNUP_URL`; set it in
the shell before running Compose commands. The optional `TG_INVITE_DESKTOP_URL`
is unset for the v1 web-only landing. These values are public destinations, not
credentials. The `--env-file /dev/null` flag still allows explicitly exported
shell variables while preventing the server stack's `.env` from entering this
project.
The production deploy runner reads `TG_INVITE_WEB_SIGNUP_URL` and, when set,
`TG_INVITE_DESKTOP_URL` from the server `.env` and exports only those values to
the selector rollout. Keep the entries as plain assignments, for example
`TG_INVITE_WEB_SIGNUP_URL=https://web.example.test/signup`; configure the real
Web sign-up URL before the next automated deployment.
The target LXC provides the resource boundary; its Docker daemon does not expose
memory or PID cgroup controllers for nested per-container limits.

For each preserved service identity, capture the identity before the deployment
starts: `container-identity-gate.sh id <container-ref>` prints the validated
complete 64-character ID, and that printed value is the saved before identity.
After the deployment, run `container-identity-gate.sh compare <saved-full-id>
<after-ref>`. The gate treats the saved value as authoritative and never resolves
it, so a removed container whose short prefix a replacement container later
answers cannot compare equal. It resolves only the after reference with `docker
inspect`, requires a complete 64-character ID, confirms the resolved ID matches
its reference, and compares all 64 characters. Never save a shortened `docker ps`
or Compose ID as the before identity: `compare` rejects anything that is not a
complete 64-character ID.

## Validate and build

From the repository root, configure the actual public Web sign-up URL (the
example host below is reserved for documentation), then inspect the resolved
service settings and build only these images:

```sh
export TG_INVITE_WEB_SIGNUP_URL=https://web.example.test/signup
unset TG_INVITE_DESKTOP_URL
docker compose --env-file /dev/null --project-directory deploy/link-edge --file deploy/link-edge/compose.yaml config
docker compose --env-file /dev/null --project-directory deploy/link-edge --file deploy/link-edge/compose.yaml build
```

Set `TG_INVITE_DESKTOP_URL` only when the desktop invite destination is ready;
it accepts an HTTPS or `teagram:` URL. When set, the landing page includes the
desktop action. The selector deploy script also requires
`TG_INVITE_WEB_SIGNUP_URL` in its environment. `/dev/null` prevents the server
stack's `.env` from entering this project.
Run the focused identity fixtures with
`bash deploy/link-edge/test-container-identity-gate.sh` before using the gate.
The selector's Docker health check requests a synthetic username path and Web
`/`; landing's check requests the same synthetic route directly. Neither probe
uses a new HTTP endpoint or prints a URL or response body.

To run the disposable, bounded stopped-upstream control and its evidence checks:

```sh
python3 -m unittest discover -s deploy/link-edge -p 'test_*.py'
go test ./cmd/linkprobe
python3 deploy/link-edge/isolated_control.py
```

The control builds the checked-out link-edge images with verified source and
revision labels, then uses a uniquely named Compose project on private networks.
The fixture publishes no ports; its diagnostic probe reaches services only
over the private bridge. Each observation independently records the selector's
`/syntheticname` and `/` statuses and direct landing status, along with health
checks, container states, combined probe exits, full identities, provenance,
and each case verdict. Evidence is JSONL in
a new mode-0700 directory with mode-0600 records; every append is synced before
the control gates or cleans up. The directory is retained for inspection. The
control never changes the production Compose project or Tailscale Serve state.

## Activation contract

Activate only after the image changes are reviewed and merged, latest-base CI
is green, the deployed Web and username-reservation prerequisites are current,
and a fresh count-only claim audit still shows no conflicting claimant. Confirm
the existing `web` alias on `teagram-web-edge-net`, loopback listeners 8081
and 8082 are unused, and Serve still has HTTPS `/` to Web on 8080, `/apiws` to
2444, `/admin` to 2445, and `/.well-known/teagramd/client` passed through to
2444. The WebSocket listener serves the discovery document at that path,
including `websocket.endpoint`. Keep Funnel off and do not change the MTProto
listeners.

The rollout starts and stops this project only:

```sh
docker compose --env-file /dev/null --project-directory deploy/link-edge --file deploy/link-edge/compose.yaml up -d --build
docker compose --env-file /dev/null --project-directory deploy/link-edge --file deploy/link-edge/compose.yaml ps -a
```

Before changing Serve, verify the health checks and synthetic landing over
`http://127.0.0.1:8081/` and `http://127.0.0.1:8082/syntheticname`; verify Web
`/` through the selector. Confirm `docker port` reports only the loopback
binding for landing and confirm the 8082 listener with `netstat`:

```sh
docker port "$(docker compose --env-file /dev/null --project-directory deploy/link-edge --file deploy/link-edge/compose.yaml ps --quiet linklanding)" 8082/tcp
netstat -ltn | grep -w 8082
```

Then change only the HTTPS `/` upstream to `http://127.0.0.1:8081` with the
installed Tailscale Serve CLI. Save a 0600 pre-change status snapshot, then
apply the root-only mount:

```sh
snapshot="/root/tailscale-serve-pre-link-edge-$(date +%Y%m%d%H%M%S).json"
umask 077
tailscale serve status --json > "$snapshot"
chmod 0600 "$snapshot"
tailscale serve --bg --https=443 --set-path=/ http://127.0.0.1:8081
tailscale serve status --json
```

The `--set-path=/` invocation updates the root handler; verify `/apiws`,
`/admin`, and the discovery path remain configured and reachable.
See the [Tailscale Serve CLI reference](https://tailscale.com/docs/reference/tailscale-cli/serve).
Do not replace the full Serve handler table or enable Funnel.

Run the synthetic route, malformed-path, no-log-leak, stopped-upstream,
Web/CSP, method/header, `/admin`, WebSocket, discovery, and network-isolation
checks from MAIN-1160 before acceptance. Use only synthetic link values. Verify
the published ports are still loopback-only and the selector cannot reach
Postgres or host admin listeners.

## Routine selector refresh

The server `Deploy.` workflow refreshes only the selector from the tested
server commit after the `teagramd` rollout. It preserves the current selector
image as `teagram-linkselector:rollback`, waits for the replacement health
check, then verifies the retired version path and all production discovery
routes through Tailscale Serve. A failed health or route check triggers a
restore of the previous selector image before the deploy exits unsuccessfully;
the deploy reports a rollback failure if it cannot verify that restore. The
landing container and Serve handler table are not changed.

To manually restore the last selector image, from `/opt/teagram` run:

```sh
docker image tag teagram-linkselector:rollback teagram-linkselector:local
docker compose --env-file /dev/null \
  --project-directory /opt/teagram/deploy/link-edge \
  --file /opt/teagram/deploy/link-edge/compose.yaml \
  --project-name teagram-link-edge \
  up -d --no-deps --wait --wait-timeout 60 selector
```

The restore wait is bounded to 60 seconds. The rollback tag points to the
selector image that was live before the latest refresh attempt.

## Rollback

Restore only HTTPS `/` to Web at `http://127.0.0.1:8080`, confirm `/apiws`,
`/admin`, and discovery are unchanged, then stop this isolated project:

```sh
tailscale serve --bg --https=443 --set-path=/ http://127.0.0.1:8080
tailscale serve status --json
docker compose --env-file /dev/null --project-directory deploy/link-edge --file deploy/link-edge/compose.yaml down
```

This removes only the selector, landing container, and this project's private
bridge. It does not touch the Web container, server stack, migrations, volumes,
or credentials. The pre-change Serve status and the exact restore commands must
be recorded on the deployment issue before activation.
