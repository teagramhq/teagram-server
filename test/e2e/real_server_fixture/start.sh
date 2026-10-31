#!/usr/bin/env bash
set -Eeuo pipefail

exec 3>&1
exec 1>&2
if (($# != 10)); then
	printf 'fixture runner requires revisions, a run ID, and fixture mode options\n' >&2
	exit 2
fi
SERVER_REVISION=$1
WEB_REVISION=$2
RUN_ID=$3
AUTH_PROGRESS=$4
AUTH_CONTROLS=$5
AUTH_PROGRESS_OUTPUT=$6
PAIR_KEY_INPUT=$7
PAIR_KEY_OUTPUT=$8
AUTH_PROGRESS_PREBASELINE_CONTROLS=$9
GUEST_SIGNUP=${10}
AUTH_PROGRESS_RUNNER_REVISION=097794ab18ef853ca09c3491b9df4e9a702d0ca6
AUTH_PROGRESS_ARTIFACT_SOURCE_REVISION=569529c1f36923c098760ef727808254dcb6a662
AUTH_PROGRESS_RELATIVE_PATH=auth-progress.json
AUTH_PROGRESS_BINDING_PATH=auth-progress-binding.json
AUTH_PROGRESS_BINDING_STATUS=auth-progress-binding-status
AUTH_PROGRESS_FRONT_COUNTER=front-websocket-101-count
AUTH_PROGRESS_FRONT_SOURCE=front.healthz.websocket_101_count
SCRIPT_DIR="$(cd -- "$(dirname -- "$0")" && pwd)"
REPO_ROOT="$(git -C "$SCRIPT_DIR/../../.." rev-parse --show-toplevel)"

if [[ ! $SERVER_REVISION =~ ^[0-9a-f]{40}$ || ! $WEB_REVISION =~ ^[0-9a-f]{40}$ ]]; then
	printf 'server and web revisions must be full lowercase 40-character SHAs\n' >&2
	exit 2
fi
if [[ $AUTH_PROGRESS != 0 && $AUTH_PROGRESS != 1 || $AUTH_CONTROLS != 0 && $AUTH_CONTROLS != 1 ||
	$AUTH_PROGRESS_PREBASELINE_CONTROLS != 0 && $AUTH_PROGRESS_PREBASELINE_CONTROLS != 1 || $GUEST_SIGNUP != 0 && $GUEST_SIGNUP != 1 ]]; then
	printf 'fixture auth-progress options are invalid\n' >&2
	exit 2
fi
if [[ $GUEST_SIGNUP == 1 && ( $AUTH_PROGRESS == 1 || $AUTH_CONTROLS == 1 || $AUTH_PROGRESS_PREBASELINE_CONTROLS == 1 ) ]]; then
	printf 'guest signup mode cannot be combined with auth-progress controls\n' >&2
	exit 2
fi
if [[ $AUTH_PROGRESS_PREBASELINE_CONTROLS == 1 && $AUTH_CONTROLS != 1 ]]; then
	printf 'prebaseline auth-progress controls require auth controls\n' >&2
	exit 2
fi
if [[ $AUTH_PROGRESS == 1 && ( $WEB_REVISION != "$AUTH_PROGRESS_ARTIFACT_SOURCE_REVISION" || -z $AUTH_PROGRESS_OUTPUT ) ]]; then
	printf 'auth-progress references or output path are invalid\n' >&2
	exit 2
fi
if ! git -C "$REPO_ROOT" cat-file -e "$SERVER_REVISION^{commit}" 2>/dev/null; then
	# A shallow harness checkout (the CI jobs fetch one commit) cannot see an
	# older published revision. Fetch that single commit from the harness
	# repository's own remote and let the full-SHA check below verify what
	# arrived. Only a shallow clone is written to: a complete checkout is never
	# rewritten, so a shared local repository keeps its history.
	if [[ "$(git -C "$REPO_ROOT" rev-parse --is-shallow-repository)" == true ]]; then
		git -C "$REPO_ROOT" fetch --no-tags --depth=1 origin "$SERVER_REVISION" >/dev/null 2>&1 || true
	fi
fi
if ! git -C "$REPO_ROOT" cat-file -e "$SERVER_REVISION^{commit}" 2>/dev/null; then
	printf 'server revision is unavailable in the harness repository\n' >&2
	exit 2
fi
VERIFIED_SERVER_REVISION="$(git -C "$REPO_ROOT" rev-parse "$SERVER_REVISION^{commit}")"
if [[ $VERIFIED_SERVER_REVISION != "$SERVER_REVISION" ]]; then
	printf 'server revision did not resolve to the requested full SHA\n' >&2
	exit 2
fi
HARNESS_REVISION="$(git -C "$REPO_ROOT" rev-parse HEAD)"
if [[ ! $HARNESS_REVISION =~ ^[0-9a-f]{40}$ ]]; then
	printf 'harness revision is not a full verified SHA\n' >&2
	exit 2
fi

RUN_PREFIX="$(cut -c1-30 <<<"$RUN_ID")"
USER_A="u$(printf '%sa' "$RUN_PREFIX")"
USER_B="u$(printf '%sb' "$RUN_PREFIX")"
PREFIX="teagram-fixture-$RUN_ID"
BROWSER_NET="$PREFIX"edge
SERVER_NET="$PREFIX"server
DATABASE="$PREFIX"database
BACKEND="$PREFIX"teagramd
FRONT="$PREFIX"tls-front
BROWSER="$PREFIX"browser
CLIENT="$PREFIX"client
ATLAS="$PREFIX"atlas
OWNER_TOKEN="$(openssl rand -hex 16)"
OWNER_LABEL="org.teagram.fixture.owner"
RUN_LABEL="org.teagram.fixture.run"
IMAGE="$PREFIX-$OWNER_TOKEN:local"
readonly PLAYWRIGHT_IMAGE_ARM64="mcr.microsoft.com/playwright:v1.61.1-noble@sha256:824f1a789072e648c62541c2cfa4479c4061a290d5c27766d67dc1dcbc19b321"
readonly PLAYWRIGHT_IMAGE_AMD64="mcr.microsoft.com/playwright:v1.61.1-noble@sha256:cf0daee9b994042e011bc29f20cdff1a9f682a039b43fcd738f7d8a9d3bcd9d6"
DSN="postgres://postgres@database:5432/teagram?sslmode=disable"
ENDPOINT="wss://teagramd.test/apiws"
ORIGIN="https://teagramd.test"
if [[ $GUEST_SIGNUP == 1 ]]; then
	REGISTRATION_MODE=open
	REGISTRATION_CLOSED_JSON=false
	GUEST_SIGNUP_JSON=true
	TURNSTILE_TEST_MODE_JSON=true
	BACKEND_TEST_ENV=(--env TG_TURNSTILE_TEST_MODE=true --env TG_TURNSTILE_SITE_KEY=1x00000000000000000000AA --env TG_TURNSTILE_SECRET_KEY=1x0000000000000000000000000000000AA --env TG_TURNSTILE_HOSTNAMES=teagramd.test --env TG_ADVERTISE_DEMO_GUEST_SIGN_UP=true --env SSL_CERT_FILE=/run/secrets/turnstile-ca.crt)
	FRONT_EXTRA_ENV=(--env TEAGRAM_FIXTURE_GUEST_SIGNUP=1)
	FRONT_SERVER_ALIASES=(--alias teagramd-proxy --alias teagramd.test --alias challenges.cloudflare.com)
else
	REGISTRATION_MODE=closed
	REGISTRATION_CLOSED_JSON=true
	GUEST_SIGNUP_JSON=false
	TURNSTILE_TEST_MODE_JSON=false
	BACKEND_TEST_ENV=(--env TG_ADVERTISE_DEMO_GUEST_SIGN_UP=false)
	FRONT_EXTRA_ENV=(--env TEAGRAM_FIXTURE_GUEST_SIGNUP=0)
	FRONT_SERVER_ALIASES=(--alias teagramd-proxy --alias teagramd.test)
fi

docker_context=${DOCKER_CONTEXT:-}
docker_endpoint=${DOCKER_HOST:-}
if [[ -n $docker_context ]]; then
	if ! docker_endpoint="$(docker context inspect "$docker_context" --format '{{.Endpoints.docker.Host}}' 2>/dev/null)"; then
		printf 'fixture could not resolve the selected Docker context; refusing to continue\n' >&2
		exit 2
	fi
elif [[ -z $docker_endpoint ]]; then
	if ! docker_context="$(docker context show 2>/dev/null)" || [[ -z $docker_context ]]; then
		printf 'fixture could not resolve the active Docker context; refusing to continue\n' >&2
		exit 2
	fi
	if ! docker_endpoint="$(docker context inspect "$docker_context" --format '{{.Endpoints.docker.Host}}' 2>/dev/null)"; then
		printf 'fixture could not resolve the active Docker context; refusing to continue\n' >&2
		exit 2
	fi
fi

local_docker_endpoint() {
	local endpoint=$1 port
	if [[ $endpoint == unix:///* && $endpoint != *[[:space:]]* ]]; then return 0; fi
	case "$endpoint" in
		tcp://localhost:*|tcp://127.0.0.1:*|tcp://\[::1\]:*)
			port=${endpoint##*:}
			[[ $port =~ ^[0-9]+$ ]]
			;;
		# The Multica workspace Docker service is an isolated per-workspace test daemon.
		tcp://multica-dind:2375) return 0 ;;
		*) return 1 ;;
	esac
}

if ! local_docker_endpoint "$docker_endpoint"; then
	printf 'fixture requires a local Docker daemon; refusing non-local endpoint\n' >&2
	exit 2
fi

# Pin every Docker command to the selection just checked, independent of later context changes.
if [[ -n $docker_context ]]; then
	DOCKER_CONTEXT=$docker_context
	export DOCKER_CONTEXT
	unset DOCKER_HOST
else
	DOCKER_HOST=$docker_endpoint
	export DOCKER_HOST
	unset DOCKER_CONTEXT
fi

for resource in "$BROWSER" "$FRONT" "$BACKEND" "$DATABASE" "$CLIENT" "$ATLAS"; do
	if docker container inspect "$resource" >/dev/null 2>&1; then
		printf 'resource name already exists: %s\n' "$resource" >&2
		exit 2
	fi
done
for resource in "$BROWSER_NET" "$SERVER_NET"; do
	if docker network inspect "$resource" >/dev/null 2>&1; then
		printf 'resource name already exists: %s\n' "$resource" >&2
		exit 2
	fi
done
if docker image inspect "$IMAGE" >/dev/null 2>&1; then
	printf 'resource name already exists: %s\n' "$IMAGE" >&2
	exit 2
fi

if ! docker_architecture="$(docker info --format '{{.Architecture}}')"; then
	printf 'fixture could not determine Docker daemon architecture\n' >&2
	exit 2
fi
case "$docker_architecture" in
	aarch64|arm64) PLAYWRIGHT_IMAGE=$PLAYWRIGHT_IMAGE_ARM64 ;;
	x86_64|amd64) PLAYWRIGHT_IMAGE=$PLAYWRIGHT_IMAGE_AMD64 ;;
	*)
		printf 'fixture does not support Docker daemon architecture %s\n' "$docker_architecture" >&2
		exit 2
		;;
esac

SECRET_ROOT=/dev/shm
if [[ ! -d $SECRET_ROOT || ! -w $SECRET_ROOT ]]; then
	printf 'fixture requires writable tmpfs at /dev/shm for private material\n' >&2
	exit 2
fi
BUILD_ROOT="$(printenv TMPDIR 2>/dev/null || true)"
if [[ -z $BUILD_ROOT || ! -d $BUILD_ROOT || ! -w $BUILD_ROOT ]]; then BUILD_ROOT=/tmp; fi
SECRET_DIR="$(mktemp -d "$SECRET_ROOT/$PREFIX.XXXXXXXX")"
BUILD_DIR="$(mktemp -d "$BUILD_ROOT/$PREFIX-build.XXXXXXXX")"
chmod 0700 "$SECRET_DIR" "$BUILD_DIR"
SERVER_WORKTREE="$BUILD_DIR/server-worktree-$OWNER_TOKEN"
SERVER_WORKTREE_ADDED=0

cleanup_failed=0
AUTH_PROGRESS_BASELINE_CAPTURED=0
AUTH_PROGRESS_BINDING_ATTEMPTED=0
ARTIFACT_DIGEST=
MANIFEST_ARTIFACT_DIGEST=
FROZEN_ARTIFACT_DIGEST=
BROWSER_STOPPED=0
RESOURCE_OWNER=
resource_owner() {
	local kind=$1 resource=$2 format output
	case "$kind" in
		container|image) format="{{index .Config.Labels \"$OWNER_LABEL\"}}" ;;
		network) format="{{index .Labels \"$OWNER_LABEL\"}}" ;;
		*) printf 'cleanup cannot inspect unsupported resource type %s\n' "$kind" >&2; return 2 ;;
	esac
	if output="$(docker "$kind" inspect --format "$format" "$resource" 2>&1)"; then
		RESOURCE_OWNER=$output
		return 0
	fi
	case "$kind:$output" in
		container:*"No such container:"*|container:*"No such object:"*|\
		network:*"network "*" not found"*|network:*"No such network:"*|network:*"No such object:"*|\
		image:*"No such image:"*|image:*"No such object:"*) return 1 ;;
		*) printf 'cleanup could not inspect %s %s: %s\n' "$kind" "$resource" "$output" >&2; return 2 ;;
	esac
}
cleanup_confirm_absent() {
	local kind=$1 resource=$2 status
	resource_owner "$kind" "$resource"
	status=$?
	if ((status == 1)); then return 0; fi
	if ((status == 0)); then
		printf 'cleanup left %s %s present\n' "$kind" "$resource" >&2
	fi
	return 1
}
cleanup_confirm_volume_absent() {
	local volume=$1 output
	if output="$(docker volume inspect "$volume" 2>&1)"; then
		printf 'cleanup left owned volume %s present\n' "$volume" >&2
		return 1
	fi
	case "$output" in
		*"no such volume"*) return 0 ;;
		*) printf 'cleanup could not verify volume %s is absent: %s\n' "$volume" "$output" >&2; return 1 ;;
	esac
}
front_websocket_accepted() {
	local response count
	response="$(docker exec "$FRONT" node -e 'require("node:https").get("https://127.0.0.1/healthz",{rejectUnauthorized:false,headers:{host:"teagramd.test"}},response=>response.pipe(process.stdout)).on("error",()=>process.exit(1))')" || return 1
	count="$(jq -er '.websocket_101_count | select(type == "number" and . == floor and . >= 0 and . <= 2147483647)' <<<"$response")" || return 1
	[[ $count =~ ^[0-9]+$ ]] || return 1
	printf '%s\n' "$count"
}
auth_progress_server_pid() {
	docker exec --user 0:0 "$BACKEND" /bin/sh -c '
		for entry in /proc/[0-9]*/comm; do
			[ -r "$entry" ] || continue
			if [ "$(cat "$entry" 2>/dev/null)" = teagramd ]; then
				pid=${entry#/proc/}
				pid=${pid%/comm}
				printf "%s\n" "$pid"
			fi
		done
	' | tail -n 1
}
signal_auth_progress_boundary() {
	local boundary=$1 index=$2 count server_pid report attempt
	count="$(front_websocket_accepted)" || {
		printf 'fixture could not read the front websocket counter\n' >&2
		return 1
	}
	printf '%s\n' "$count" | docker exec -i --user 65532:65532 "$BACKEND" /bin/sh -c "umask 077; cat > /run/log/$AUTH_PROGRESS_FRONT_COUNTER" || return 1
	server_pid="$(auth_progress_server_pid)"
	if [[ ! $server_pid =~ ^[0-9]+$ ]]; then
		printf 'fixture could not identify its auth-progress server process\n' >&2
		return 1
	fi
	if ! docker exec --user 65532:65532 "$BACKEND" /bin/sh -c "kill -USR1 $server_pid"; then
		printf 'fixture could not signal its auth-progress server process\n' >&2
		return 1
	fi
	for attempt in $(seq 1 100); do
		if report="$(docker exec "$BACKEND" cat "/run/log/$AUTH_PROGRESS_RELATIVE_PATH" 2>/dev/null)" &&
			jq -e --arg boundary "$boundary" --arg source "$AUTH_PROGRESS_FRONT_SOURCE" --argjson count "$count" --argjson index "$index" \
				'.boundaries | length >= $index and .[$index - 1].server.boundary == $boundary and .[$index - 1].front.ws_accepted == $count and .[$index - 1].front.source == $source' <<<"$report" >/dev/null 2>&1; then
			return 0
		fi
		sleep 0.1
	done
	printf 'fixture auth-progress boundary was not recorded\n' >&2
	return 1
}
bind_auth_progress_identity() {
	local identity=$1 server_pid status attempt
	printf '%s\n' "$identity" | docker exec -i --user 65532:65532 "$BACKEND" /bin/sh -c "umask 077; cat > /run/log/$AUTH_PROGRESS_BINDING_PATH" || return 1
	server_pid="$(auth_progress_server_pid)"
	if [[ ! $server_pid =~ ^[0-9]+$ ]] || ! docker exec --user 65532:65532 "$BACKEND" /bin/sh -c "kill -USR2 $server_pid"; then
		printf 'fixture could not request auth-progress identity binding\n' >&2
		return 1
	fi
	for attempt in $(seq 1 100); do
		if status="$(docker exec "$BACKEND" cat "/run/log/$AUTH_PROGRESS_BINDING_STATUS" 2>/dev/null)"; then
			case "$status" in
				bound) docker exec --user 65532:65532 "$BACKEND" rm -f "/run/log/$AUTH_PROGRESS_BINDING_PATH" "/run/log/$AUTH_PROGRESS_BINDING_STATUS" || return 1; return 0 ;;
				rejected) printf 'fixture server rejected the audited artifact identity binding\n' >&2; return 1 ;;
			esac
		fi
		sleep 0.1
	done
	printf 'fixture server did not acknowledge the artifact identity binding\n' >&2
	return 1
}
stop_auth_progress_server() {
	local server_pid attempt stopped=0
	server_pid="$(auth_progress_server_pid)"
	if [[ ! $server_pid =~ ^[0-9]+$ ]]; then
		printf 'fixture could not identify its auth-progress server process for shutdown\n' >&2
		return 1
	fi
	if ! docker exec --user 65532:65532 "$BACKEND" /bin/sh -c "kill -TERM $server_pid"; then
		printf 'fixture could not request bounded auth-progress shutdown\n' >&2
		return 1
	fi
	for attempt in $(seq 1 150); do
		if ! docker exec --user 0:0 "$BACKEND" /bin/sh -c "test -d /proc/$server_pid"; then
			stopped=1
			break
		fi
		sleep 0.1
	done
	if ((stopped == 0)); then
		printf 'fixture auth-progress shutdown exceeded its bound\n' >&2
		docker exec --user 65532:65532 "$BACKEND" /bin/sh -c "kill -KILL $server_pid" >/dev/null 2>&1
		return 1
	fi
	return 0
}
cleanup() {
	local original_status=$?
	local owner
	local inspect_status
	local container_volumes volume
	trap - EXIT INT TERM
	set +e
	if [[ $AUTH_PROGRESS == 1 ]]; then
		if resource_owner container "$BROWSER"; then
			owner=$RESOURCE_OWNER
			if [[ $owner == "$OWNER_TOKEN" ]]; then
				if [[ $BROWSER_STOPPED == 0 ]]; then docker stop --time 5 "$BROWSER" >/dev/null 2>&1 || cleanup_failed=1; fi
			else
				printf 'cleanup refused unowned browser container %s\n' "$BROWSER" >&2
				cleanup_failed=1
			fi
		fi
		if [[ $AUTH_PROGRESS_BASELINE_CAPTURED == 1 ]]; then
			if ! signal_auth_progress_boundary runner_exit 2; then cleanup_failed=1; fi
		fi
		if ! stop_auth_progress_server; then cleanup_failed=1; fi
		if [[ $AUTH_PROGRESS_BINDING_ATTEMPTED == 1 && -n $AUTH_PROGRESS_OUTPUT ]]; then
			if [[ -e $AUTH_PROGRESS_OUTPUT || -L $AUTH_PROGRESS_OUTPUT ]]; then
				printf 'fixture could not preserve its auth-progress report\n' >&2
				cleanup_failed=1
			elif ! (umask 077; docker exec "$BACKEND" cat "/run/log/$AUTH_PROGRESS_RELATIVE_PATH" > "$AUTH_PROGRESS_OUTPUT" 2>/dev/null); then
				rm -f -- "$AUTH_PROGRESS_OUTPUT"
				printf 'fixture could not preserve its auth-progress report\n' >&2
				cleanup_failed=1
			else
				chmod 0600 "$AUTH_PROGRESS_OUTPUT" || cleanup_failed=1
				if [[ ! -f $AUTH_PROGRESS_OUTPUT || -L $AUTH_PROGRESS_OUTPUT ]]; then cleanup_failed=1; fi
			fi
		fi
	fi
	for container in "$BROWSER" "$FRONT" "$BACKEND" "$DATABASE" "$CLIENT" "$ATLAS"; do
		if resource_owner container "$container"; then
			owner=$RESOURCE_OWNER
			if [[ $owner == "$OWNER_TOKEN" ]]; then
				if ! container_volumes="$(docker inspect --format '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}}{{"\n"}}{{end}}{{end}}' "$container" 2>&1)"; then
					printf 'cleanup could not record volume mounts for %s: %s\n' "$container" "$container_volumes" >&2
					cleanup_failed=1
					container_volumes=
				fi
				docker container rm -f -v "$container" >/dev/null 2>&1
				if ! cleanup_confirm_absent container "$container"; then cleanup_failed=1; fi
				while IFS= read -r volume; do
					[[ -n $volume ]] || continue
					if ! cleanup_confirm_volume_absent "$volume"; then cleanup_failed=1; fi
				done <<<"$container_volumes"
			else
				printf 'cleanup refused unowned container %s\n' "$container" >&2
				cleanup_failed=1
			fi
		else
			inspect_status=$?
			if ((inspect_status == 2)); then cleanup_failed=1; fi
		fi
	done
	for network in "$BROWSER_NET" "$SERVER_NET"; do
		if resource_owner network "$network"; then
			owner=$RESOURCE_OWNER
			if [[ $owner == "$OWNER_TOKEN" ]]; then
				docker network rm "$network" >/dev/null 2>&1
				if ! cleanup_confirm_absent network "$network"; then cleanup_failed=1; fi
			else
				printf 'cleanup refused unowned network %s\n' "$network" >&2
				cleanup_failed=1
			fi
		else
			inspect_status=$?
			if ((inspect_status == 2)); then cleanup_failed=1; fi
		fi
	done
	if resource_owner image "$IMAGE"; then
		owner=$RESOURCE_OWNER
		if [[ $owner == "$OWNER_TOKEN" ]]; then
			docker image rm "$IMAGE" >/dev/null 2>&1
			if ! cleanup_confirm_absent image "$IMAGE"; then cleanup_failed=1; fi
		else
			printf 'cleanup refused unowned image %s\n' "$IMAGE" >&2
			cleanup_failed=1
		fi
	else
		inspect_status=$?
		if ((inspect_status == 2)); then cleanup_failed=1; fi
	fi
	if [[ -n $SECRET_DIR && -d $SECRET_DIR ]]; then
		rm -rf -- "$SECRET_DIR"
		if [[ -e $SECRET_DIR || -e "$SECRET_DIR/server-key.pem" ]]; then cleanup_failed=1; fi
	fi
	if ((SERVER_WORKTREE_ADDED)); then
		if ! git -C "$REPO_ROOT" worktree remove --force "$SERVER_WORKTREE"; then
			printf 'cleanup could not remove the owned server worktree\n' >&2
			cleanup_failed=1
		fi
		SERVER_WORKTREE_ADDED=0
	fi
	if [[ -n $BUILD_DIR && -d $BUILD_DIR ]]; then
		# The staged tree is deliberately read-only; restore writability so the
		# owned directory can actually be removed.
		chmod -R u+rwX -- "$BUILD_DIR" 2>/dev/null || true
		rm -rf -- "$BUILD_DIR"
		if [[ -e $BUILD_DIR || -e "$BUILD_DIR/staged-artifact" ]]; then cleanup_failed=1; fi
	fi
	if [[ "$(printenv TEAGRAM_FIXTURE_TEST_CLEANUP_FAILURE 2>/dev/null || true)" == 1 ]]; then
		printf 'injected cleanup failure\n' >&2
		cleanup_failed=1
	fi
	if ((cleanup_failed)); then
		printf 'cleanup=failed\n' >&2
		exit 1
	fi
	printf 'cleanup=verified\n' >&2
	exit "$original_status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

TLS_SAN=DNS:teagramd.test
TLS_BASIC_CONSTRAINTS=critical,CA:FALSE
TLS_KEY_USAGE=critical,digitalSignature,keyEncipherment
if [[ $GUEST_SIGNUP == 1 ]]; then
	TLS_SAN+=,DNS:challenges.cloudflare.com
	TLS_BASIC_CONSTRAINTS=critical,CA:TRUE
	TLS_KEY_USAGE=critical,digitalSignature,keyEncipherment,keyCertSign
fi
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$SECRET_DIR/tls.key" -out "$SECRET_DIR/tls.crt" -days 1 \
	-subj /CN=teagramd.test -addext "subjectAltName=$TLS_SAN" \
	-addext "basicConstraints=$TLS_BASIC_CONSTRAINTS" -addext "keyUsage=$TLS_KEY_USAGE" \
	-addext extendedKeyUsage=serverAuth >/dev/null 2>&1
chmod 0400 "$SECRET_DIR/tls.key" "$SECRET_DIR/tls.crt"

git -C "$REPO_ROOT" worktree add --detach "$SERVER_WORKTREE" "$SERVER_REVISION" >&2
SERVER_WORKTREE_ADDED=1
# Historical revisions used cmd/telegramd before the rename. Build either
# package into the stable teagramd fixture binary path.
SERVER_PACKAGE=./cmd/teagramd
if [[ ! -d "$SERVER_WORKTREE/cmd/teagramd" && -d "$SERVER_WORKTREE/cmd/telegramd" ]]; then
	SERVER_PACKAGE=./cmd/telegramd
fi
if [[ $AUTH_PROGRESS == 1 ]]; then
	(
		cd "$SERVER_WORKTREE"
		CGO_ENABLED=0 go build -tags fixtureauthprogress -o "$BUILD_DIR/teagramd" "$SERVER_PACKAGE"
	)
else
	(
		cd "$SERVER_WORKTREE"
		CGO_ENABLED=0 go build -o "$BUILD_DIR/teagramd" "$SERVER_PACKAGE"
	)
fi
# Build the worker from the same immutable revision as teagramd. Historical
# fixture revisions predate the worker and do not need it.
PHOTO_THUMB_BINARY=
if [[ -f "$SERVER_WORKTREE/cmd/photothumb/main.go" ]]; then
	(
		cd "$SERVER_WORKTREE"
		CGO_ENABLED=0 go build -o "$BUILD_DIR/photothumb" ./cmd/photothumb
	)
	PHOTO_THUMB_BINARY="$BUILD_DIR/photothumb"
fi
# The login observer is harness-owned tooling, so it builds from the harness tree.
# Building it from the revision under test makes any revision older than this bridge
# unbuildable, and such a revision can then never be attempted at all.
CGO_ENABLED=0 go build -C "$REPO_ROOT" -o "$BUILD_DIR/fixture-auth-check" ./test/e2e/real_server_fixture/authcheck

docker build --quiet --build-arg "PLAYWRIGHT_IMAGE=$PLAYWRIGHT_IMAGE" --label "$OWNER_LABEL=$OWNER_TOKEN" --label "$RUN_LABEL=$RUN_ID" --tag "$IMAGE" "$SCRIPT_DIR"
docker network create --driver bridge --internal --ipv6=false --label "$OWNER_LABEL=$OWNER_TOKEN" --label "$RUN_LABEL=$RUN_ID" "$BROWSER_NET" >/dev/null
docker network create --driver bridge --internal --ipv6=false --label "$OWNER_LABEL=$OWNER_TOKEN" --label "$RUN_LABEL=$RUN_ID" "$SERVER_NET" >/dev/null
for network in "$BROWSER_NET" "$SERVER_NET"; do
	[[ "$(docker network inspect --format '{{.Internal}}' "$network")" == true ]]
	[[ "$(docker network inspect --format '{{.EnableIPv6}}' "$network")" == false ]]
done

docker run --detach --name "$DATABASE" --label "$OWNER_LABEL=$OWNER_TOKEN" --label "$RUN_LABEL=$RUN_ID" \
	--network "$SERVER_NET" --network-alias database --log-driver none \
	--read-only --user 999:999 --cap-drop ALL --security-opt no-new-privileges:true \
	--memory 512m --memory-swap 512m --cpus 0.5 --pids-limit 128 --ulimit core=0 \
	--tmpfs /var/lib/postgresql/18/docker:rw,noexec,nosuid,nodev,size=512m,uid=999,gid=999 \
	--tmpfs /var/run/postgresql:rw,noexec,nosuid,nodev,size=16m,uid=999,gid=999 \
	--tmpfs /tmp:rw,noexec,nosuid,nodev,size=16m,uid=999,gid=999 \
	--env POSTGRES_DB=teagram --env POSTGRES_HOST_AUTH_METHOD=trust \
	--env PGDATA=/var/lib/postgresql/18/docker \
	--health-cmd='pg_isready -q -U postgres -d teagram' --health-interval 1s --health-timeout 2s --health-retries 30 \
	mirror.gcr.io/library/postgres:18-alpine >/dev/null

wait_healthy() {
	local container=$1 attempt status
	for attempt in $(seq 1 60); do
		status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}' "$container")"
		if [[ $status == healthy ]]; then return 0; fi
		if [[ $status == unhealthy || $status == missing ]]; then
			printf 'health_failed container=%s state=%s\n' "$container" "$(docker inspect --format '{{.State.Status}}' "$container")" >&2
			docker inspect --format '{{json .State.Health.Log}}' "$container" >&2 || true
			return 1
		fi
		sleep 1
	done
	printf 'health_timeout container=%s\n' "$container" >&2
	docker inspect --format '{{json .State.Health.Log}}' "$container" >&2 || true
	return 1
}

wait_healthy "$DATABASE"
docker create --rm --name "$ATLAS" --label "$OWNER_LABEL=$OWNER_TOKEN" --label "$RUN_LABEL=$RUN_ID" --network "$SERVER_NET" \
	mirror.gcr.io/arigaio/atlas:1.2.0-alpine migrate apply --dir file:///migrations --url "$DSN" >/dev/null
docker cp "$SERVER_WORKTREE/migrations/." "$ATLAS:/migrations"
docker start --attach "$ATLAS" >/dev/null

umask 077
openssl rand -hex 32 > "$SECRET_DIR/authkey.hex"
openssl rand -hex 32 > "$SECRET_DIR/a-password"
openssl rand -hex 32 > "$SECRET_DIR/b-password"
if [[ -n $PAIR_KEY_INPUT ]]; then
	cp -- "$PAIR_KEY_INPUT" "$SECRET_DIR/server-key.pem"
	chmod 0400 "$SECRET_DIR/server-key.pem"
else
	TG_RSA_KEY_PATH="$SECRET_DIR/server-key.pem" "$BUILD_DIR/teagramd" bootstrap-identity > "$SECRET_DIR/bootstrap.log"
	if [[ -n $PAIR_KEY_OUTPUT ]]; then
		(umask 077; set -o noclobber; cat "$SECRET_DIR/server-key.pem" > "$PAIR_KEY_OUTPUT")
		chmod 0400 "$PAIR_KEY_OUTPUT"
	fi
fi
openssl pkey -in "$SECRET_DIR/server-key.pem" -pubout -out "$SECRET_DIR/server.pub.pem" >/dev/null 2>&1
chmod 0400 "$SECRET_DIR/server-key.pem" "$SECRET_DIR/authkey.hex" "$SECRET_DIR/server.pub.pem"
chmod 0400 "$SECRET_DIR/a-password" "$SECRET_DIR/b-password"
FINGERPRINT="$("$BUILD_DIR/fixture-auth-check" --fingerprint-only --public-key "$SECRET_DIR/server.pub.pem")"
if [[ $FINGERPRINT == fbb62871f07fae2a ]]; then
	printf 'fixture generated the prohibited production MTProto fingerprint\n' >&2
	exit 1
fi

docker run --detach --name "$BACKEND" --label "$OWNER_LABEL=$OWNER_TOKEN" --label "$RUN_LABEL=$RUN_ID" \
	--network "$SERVER_NET" --network-alias teagramd --log-driver none \
	--read-only --user 65532:65532 --cap-drop ALL --security-opt no-new-privileges:true \
	--memory 512m --memory-swap 512m --cpus 0.5 --pids-limit 64 --ulimit core=0 \
	--tmpfs /usr/local/bin:rw,exec,nosuid,nodev,size=16m,uid=0,gid=0,mode=0755 \
	--tmpfs /run/app:rw,exec,nosuid,nodev,size=64m,uid=65532,gid=65532 \
	--tmpfs /run/secrets:rw,noexec,nosuid,nodev,size=2m,uid=65532,gid=65532 \
	--tmpfs /run/log:rw,noexec,nosuid,nodev,size=8m,uid=65532,gid=65532 \
	--tmpfs /var/lib/teagramd-blobs:rw,noexec,nosuid,nodev,size=16m,uid=65532,gid=65532 \
	--env "TG_POSTGRES_DSN=$DSN" --env TG_AUTHKEY_ENC_KEY_FILE=/run/secrets/authkey.hex \
	--env TG_RSA_KEY_PATH=/run/secrets/server-key.pem --env TG_PUBLIC_LINK_PREFIX=https://teagramd.test/ \
	--env TG_LISTEN_ADDR=:2443 --env TG_ADVERTISE_ADDR=teagramd:2443 \
	--env TG_WEBSOCKET_LISTEN_ADDR=:2444 --env TG_WEBSOCKET_ALLOWED_ORIGINS=https://teagramd.test \
	--env "TG_REGISTRATION=$REGISTRATION_MODE" --env TG_LOG_LOGIN_CODES=false \
	--env TG_BLOB_DIR=/var/lib/teagramd-blobs \
	"${BACKEND_TEST_ENV[@]}" \
	--entrypoint /bin/sh mirror.gcr.io/library/postgres:18-alpine -c 'exec sleep 86400' >/dev/null
docker exec -i "$BACKEND" /bin/sh -c 'umask 077; cat > /run/app/teagramd' < "$BUILD_DIR/teagramd"
if [[ -n $PHOTO_THUMB_BINARY ]]; then
	docker exec -i --user 0:0 "$BACKEND" /bin/sh -c 'umask 077; cat > /usr/local/bin/photothumb' < "$PHOTO_THUMB_BINARY"
	docker exec --user 0:0 "$BACKEND" chmod 0755 /usr/local/bin/photothumb
	if [[ "$(docker exec "$BACKEND" stat -c '%u:%g:%a' /usr/local/bin/photothumb)" != "0:0:755" ]]; then
		printf 'fixture photo worker is not root-owned and executable\n' >&2
		exit 1
	fi
fi
docker exec -i "$BACKEND" /bin/sh -c 'umask 077; cat > /run/secrets/server-key.pem' < "$SECRET_DIR/server-key.pem"
docker exec -i "$BACKEND" /bin/sh -c 'umask 077; cat > /run/secrets/authkey.hex' < "$SECRET_DIR/authkey.hex"
if [[ $GUEST_SIGNUP == 1 ]]; then
	docker exec -i "$BACKEND" /bin/sh -c 'umask 077; cat > /run/secrets/turnstile-ca.crt' < "$SECRET_DIR/tls.crt"
	docker exec "$BACKEND" chown 65532:65532 /run/secrets/turnstile-ca.crt
	docker exec "$BACKEND" chmod 0440 /run/secrets/turnstile-ca.crt
fi
docker exec "$BACKEND" chown 65532:65532 /run/app/teagramd /run/secrets/server-key.pem /run/secrets/authkey.hex
docker exec "$BACKEND" chmod 0755 /run/app/teagramd
docker exec "$BACKEND" chmod 0440 /run/secrets/server-key.pem /run/secrets/authkey.hex
READINESS_ATTEMPTS=60
if [[ "$(printenv TEAGRAM_FIXTURE_TEST_READINESS_TIMEOUT 2>/dev/null || true)" == 1 ]]; then
	READINESS_ATTEMPTS=1
else
	if [[ $AUTH_PROGRESS == 1 ]]; then
		docker exec --detach --user 65532:65532 \
			--env "TG_FIXTURE_AUTH_PROGRESS=/run/log/$AUTH_PROGRESS_RELATIVE_PATH" \
			--env "TG_FIXTURE_AUTH_PROGRESS_RUN_ID=$RUN_ID" \
			--env "TG_FIXTURE_AUTH_PROGRESS_RUNNER_REVISION=$AUTH_PROGRESS_RUNNER_REVISION" \
			--env "TG_FIXTURE_AUTH_PROGRESS_ARTIFACT_SOURCE_REVISION=$AUTH_PROGRESS_ARTIFACT_SOURCE_REVISION" \
			--env "TG_FIXTURE_AUTH_PROGRESS_HARNESS_REVISION=$HARNESS_REVISION" \
			"$BACKEND" /bin/sh -c 'exec /run/app/teagramd serve >/run/log/teagramd.log 2>&1'
	else
		docker exec --detach --user 65532:65532 "$BACKEND" /bin/sh -c 'exec /run/app/teagramd serve >/run/log/teagramd.log 2>&1'
	fi
fi

ready=0
for _ in $(seq 1 "$READINESS_ATTEMPTS"); do
	if docker exec "$BACKEND" /bin/sh -c 'grep -q "msg=listening" /run/log/teagramd.log && grep -q "WebSocket MTProto listening" /run/log/teagramd.log' >/dev/null 2>&1; then
		ready=1
		break
	fi
	state="$(docker inspect --format '{{.State.Status}}' "$BACKEND")"
	if [[ $state != running ]]; then
		printf 'teagramd exited before readiness\n' >&2
		docker exec "$BACKEND" cat /run/log/teagramd.log >&2 || true
		exit 1
	fi
	sleep 1
done
if ((ready == 0)); then
	printf 'teagramd readiness timeout\n' >&2
	docker exec "$BACKEND" cat /run/log/teagramd.log >&2 || true
	exit 1
fi
admin_state() {
	docker exec "$DATABASE" psql -U postgres -d teagram -X -qAt -c \
		"SELECT election_closed::text || '|' || COALESCE(administrator_user_id::text, 'NULL') FROM server_administration WHERE singleton_id = 1"
}
db_count() {
	docker exec "$DATABASE" psql -U postgres -d teagram -X -qAt -c "$1"
}

for account in a b; do
	if [[ $account == a ]]; then username=$USER_A; password_file="$SECRET_DIR/a-password"; else username=$USER_B; password_file="$SECRET_DIR/b-password"; fi
	if ! docker exec -i --user 0:65532 "$BACKEND" /run/app/teagramd admin create-user --username "$username" < "$password_file" > "$SECRET_DIR/create-user-$account.out" 2> "$SECRET_DIR/create-user-$account.err"; then
		printf 'production create-user failed for synthetic account %s\n' "$account" >&2
		cat "$SECRET_DIR/create-user-$account.err" >&2
		exit 1
	fi
	if [[ -s $SECRET_DIR/create-user-$account.out ]]; then
		printf 'production create-user wrote unexpected stdout\n' >&2
		exit 1
	fi
	if ! grep -Eq "^User created: $username \\(user id: [0-9]+\\)$" "$SECRET_DIR/create-user-$account.err"; then
		printf 'production create-user confirmation was missing for synthetic account %s\n' "$account" >&2
		exit 1
	fi
done

if [[ $(admin_state) != 'true|NULL' ]]; then
	printf 'administrator election state was not closed and empty before authentication\n' >&2
	exit 1
fi
USER_COUNTS="$(db_count "SELECT count(*) || '|' || count(*) FILTER (WHERE login_mode = 'username') || '|' || (SELECT count(*) FROM user_passwords) FROM users")"
USER_HANDLES="$(db_count "SELECT string_agg(handle, ',' ORDER BY handle) FROM usernames")"
EXPECTED_HANDLES="$USER_A,$USER_B"
[[ $USER_COUNTS == '2|2|2' && $USER_HANDLES == "$EXPECTED_HANDLES" ]] || {
	printf 'fixture account rows failed ordinary-user assertions\n' >&2
	exit 1
}
INITIAL_AUTH_KEYS="$(db_count 'SELECT count(*) FROM auth_keys WHERE user_id IS NOT NULL')"
INITIAL_MESSAGES="$(db_count 'SELECT count(*) FROM messages')"
[[ $INITIAL_AUTH_KEYS == 0 && $INITIAL_MESSAGES == 0 ]] || {
	printf 'fresh database contained pre-existing user sessions or messages\n' >&2
	exit 1
}

docker run --detach --name "$CLIENT" --label "$OWNER_LABEL=$OWNER_TOKEN" --label "$RUN_LABEL=$RUN_ID" --network "$SERVER_NET" \
	--log-driver none --read-only --user 1001:1001 --cap-drop ALL --security-opt no-new-privileges:true \
	--memory 128m --memory-swap 128m --cpus 0.25 --pids-limit 64 --ulimit core=0 \
	--tmpfs /run/app:rw,exec,nosuid,nodev,size=32m,uid=1001,gid=1001 \
	--tmpfs /run/secrets:rw,noexec,nosuid,nodev,size=2m,uid=1001,gid=1001 \
	--entrypoint /bin/sh mirror.gcr.io/library/postgres:18-alpine -c 'exec sleep 86400' >/dev/null
docker exec -i "$CLIENT" /bin/sh -c 'umask 077; cat > /run/app/fixture-auth-check' < "$BUILD_DIR/fixture-auth-check"
docker exec -i "$CLIENT" /bin/sh -c 'umask 077; cat > /run/secrets/server.pub.pem' < "$SECRET_DIR/server.pub.pem"
docker exec -i "$CLIENT" /bin/sh -c 'umask 077; cat > /run/secrets/a-password' < "$SECRET_DIR/a-password"
docker exec -i "$CLIENT" /bin/sh -c 'umask 077; cat > /run/secrets/b-password' < "$SECRET_DIR/b-password"
docker exec "$CLIENT" chown 1001:1001 /run/app/fixture-auth-check /run/secrets/server.pub.pem /run/secrets/a-password /run/secrets/b-password
docker exec "$CLIENT" chmod 0755 /run/app/fixture-auth-check
docker exec "$CLIENT" chmod 0400 /run/secrets/server.pub.pem /run/secrets/a-password /run/secrets/b-password
docker create --name "$FRONT" --label "$OWNER_LABEL=$OWNER_TOKEN" --label "$RUN_LABEL=$RUN_ID" \
	--network "$BROWSER_NET" --network-alias tls-front --log-driver none \
	--read-only --user 1001:1001 --cap-drop ALL --security-opt no-new-privileges:true \
	--sysctl net.ipv4.ip_unprivileged_port_start=0 \
	--memory 128m --memory-swap 128m --cpus 0.5 --pids-limit 64 --ulimit core=0 \
	--tmpfs /tmp:rw,noexec,nosuid,nodev,size=16m,uid=1001,gid=1001 \
	--tmpfs /run:rw,noexec,nosuid,nodev,size=4m,uid=1001,gid=1001 \
	--tmpfs /srv/artifact:rw,noexec,nosuid,nodev,size=256m,uid=1001,gid=1001,mode=0700 \
	--health-cmd='node -e "require(\"https\").get(\"https://127.0.0.1/healthz\",{rejectUnauthorized:false,headers:{host:\"teagramd.test\"}},r=>process.exit(r.statusCode===200?0:1)).on(\"error\",()=>process.exit(1))"' \
	--health-interval 1s --health-timeout 2s --health-retries 30 \
	"${FRONT_EXTRA_ENV[@]}" \
	--entrypoint node "$IMAGE" -e 'setInterval(() => {}, 1 << 30)' >/dev/null
docker network connect "${FRONT_SERVER_ALIASES[@]}" "$SERVER_NET" "$FRONT"
docker start "$FRONT" >/dev/null
docker exec -i "$FRONT" sh -c 'umask 077; cat > /run/tls.key' < "$SECRET_DIR/tls.key"
docker exec -i "$FRONT" sh -c 'umask 077; cat > /run/tls.crt' < "$SECRET_DIR/tls.crt"
docker exec "$FRONT" chmod 0400 /run/tls.key /run/tls.crt

PUBLIC_KEY="$(cat "$SECRET_DIR/server.pub.pem")"
# The descriptor publishes the PEM through a shell variable, which drops the
# trailing newline, so the hash must cover exactly the bytes it publishes.
PUBLIC_KEY_SHA256="$(printf '%s' "$PUBLIC_KEY" | sha256sum | cut -d ' ' -f 1)"
if [[ $GUEST_SIGNUP == 1 ]]; then
	RSA_SPKI="$(openssl pkey -pubin -in "$SECRET_DIR/server.pub.pem" -outform DER | base64 -w0)"
	jq -cn --arg server teagramd.test '{server:$server,autoGuest:false}' > "$SECRET_DIR/teagram-config.json"
	jq -cn --arg rsaSpki "$RSA_SPKI" \
		'{version:1,mtproto:{endpoint:"teagramd.test:2443",dc_id:2,rsa_spki:$rsaSpki},websocket:{endpoint:"wss://teagramd.test/apiws"},turnstile:{site_key:"1x00000000000000000000AA"},demo:{guest_sign_up:true}}' \
		> "$SECRET_DIR/teagramd-client-discovery.json"
	docker exec -i "$FRONT" sh -c 'umask 077; cat > /run/teagram-config.json' < "$SECRET_DIR/teagram-config.json"
	docker exec -i "$FRONT" sh -c 'umask 077; cat > /run/teagramd-client-discovery.json' < "$SECRET_DIR/teagramd-client-discovery.json"
	docker exec "$FRONT" chown 1001:1001 /run/teagram-config.json /run/teagramd-client-discovery.json
	docker exec "$FRONT" chmod 0440 /run/teagram-config.json /run/teagramd-client-discovery.json
fi
MANIFEST_ENDPOINT=$ENDPOINT
MANIFEST_PUBLIC_KEY_SHA256=$PUBLIC_KEY_SHA256
if [[ "$(printenv TEAGRAM_FIXTURE_TEST_MISMATCH_TARGET 2>/dev/null || true)" == 1 ]]; then
	MANIFEST_PUBLIC_KEY_SHA256=0000000000000000000000000000000000000000000000000000000000000000
fi
jq -cn --arg endpoint "$MANIFEST_ENDPOINT" --arg fingerprint "$FINGERPRINT" --arg publicKeySHA256 "$MANIFEST_PUBLIC_KEY_SHA256" \
	'{endpoint:$endpoint,fingerprint:$fingerprint,publicKeySHA256:$publicKeySHA256}' > "$SECRET_DIR/mtproto-target.json"
docker exec -i "$FRONT" sh -c 'umask 077; cat > /run/mtproto-target.json' < "$SECRET_DIR/mtproto-target.json"
TLS_SPKI="$(docker exec "$FRONT" sh -c 'openssl x509 -in /run/tls.crt -pubkey -noout | openssl pkey -pubin -outform DER | openssl dgst -sha256 -binary | base64 -w0')"
docker exec --detach --user 1001:1001 "$FRONT" sh -c 'node /opt/real-server-fixture/front.mjs >/run/front.log 2>&1 & echo $! > /run/front.pid'
wait_healthy "$FRONT"

docker exec -i "$CLIENT" /bin/sh -c 'umask 077; cat > /run/secrets/tls.crt' < "$SECRET_DIR/tls.crt"
docker exec "$CLIENT" chown 1001:1001 /run/secrets/tls.crt
docker exec "$CLIENT" chmod 0400 /run/secrets/tls.crt
docker exec "$CLIENT" /run/app/fixture-auth-check --endpoint "$ENDPOINT" --tls-root-file /run/secrets/tls.crt --public-key /run/secrets/server.pub.pem --username "$USER_A" --password-file /run/secrets/a-password >/dev/null
docker exec "$CLIENT" /run/app/fixture-auth-check --endpoint "$ENDPOINT" --tls-root-file /run/secrets/tls.crt --public-key /run/secrets/server.pub.pem --username "$USER_B" --password-file /run/secrets/b-password >/dev/null
if [[ $GUEST_SIGNUP != 1 ]]; then
	docker exec "$CLIENT" /run/app/fixture-auth-check --endpoint "$ENDPOINT" --tls-root-file /run/secrets/tls.crt --public-key /run/secrets/server.pub.pem --closed-signup >/dev/null
fi

if [[ $(admin_state) != 'true|NULL' || $(db_count 'SELECT count(*) FROM auth_keys WHERE user_id IS NOT NULL') != 2 ]]; then
	printf 'administrator election or authenticated-session state changed unexpectedly\n' >&2
	exit 1
fi
if [[ $(db_count 'SELECT count(*) FROM messages') != 0 || $(db_count "SELECT count(*) || '|' || count(*) FILTER (WHERE login_mode = 'username') || '|' || (SELECT count(*) FROM user_passwords) FROM users") != '2|2|2' || $(db_count "SELECT string_agg(handle, ',' ORDER BY handle) FROM usernames") != "$EXPECTED_HANDLES" ]]; then
	printf 'authentication or closed signup changed account or message state\n' >&2
	exit 1
fi
if docker exec "$BACKEND" /bin/sh -c 'grep -Eq "login code issued|TG_LOG_LOGIN_CODES is on" /run/log/teagramd.log'; then
	printf 'login-code logging was observed\n' >&2
	exit 1
fi
SERVER_ENV="$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$BACKEND")"
for expected in "TG_REGISTRATION=$REGISTRATION_MODE" 'TG_LOG_LOGIN_CODES=false' 'TG_WEBSOCKET_ALLOWED_ORIGINS=https://teagramd.test'; do
	if ! printf '%s\n' "$SERVER_ENV" | grep -Fxq "$expected"; then
		printf 'teagramd security environment assertion failed\n' >&2
		exit 1
	fi
done
if [[ $GUEST_SIGNUP == 1 ]]; then
	for expected in 'TG_TURNSTILE_TEST_MODE=true' 'TG_TURNSTILE_SITE_KEY=1x00000000000000000000AA' \
		'TG_TURNSTILE_SECRET_KEY=1x0000000000000000000000000000000AA' 'TG_TURNSTILE_HOSTNAMES=teagramd.test' \
		'TG_ADVERTISE_DEMO_GUEST_SIGN_UP=true' 'SSL_CERT_FILE=/run/secrets/turnstile-ca.crt'; do
		if ! printf '%s\n' "$SERVER_ENV" | grep -Fxq "$expected"; then
			printf 'teagramd guest-signup test environment assertion failed\n' >&2
			exit 1
		fi
	done
fi
CLIENT_WSS_BASELINE="$(docker exec "$FRONT" node -e 'require("https").get("https://127.0.0.1/healthz",{rejectUnauthorized:false,headers:{host:"teagramd.test"}},response=>response.pipe(process.stdout))' | jq -r '.websocket_101_count')"
[[ $CLIENT_WSS_BASELINE =~ ^[0-9]+$ ]]

FRONT_IP="$(docker inspect --format "{{(index .NetworkSettings.Networks \"$BROWSER_NET\").IPAddress}}" "$FRONT")"
BACKEND_IP="$(docker inspect --format "{{(index .NetworkSettings.Networks \"$SERVER_NET\").IPAddress}}" "$BACKEND")"
DATABASE_IP="$(docker inspect --format "{{(index .NetworkSettings.Networks \"$SERVER_NET\").IPAddress}}" "$DATABASE")"
FRONT_SERVER_IP="$(docker inspect --format "{{(index .NetworkSettings.Networks \"$SERVER_NET\").IPAddress}}" "$FRONT")"

docker run --detach --name "$BROWSER" --label "$OWNER_LABEL=$OWNER_TOKEN" --label "$RUN_LABEL=$RUN_ID" --network "$BROWSER_NET" \
	--log-driver none --read-only --user 1001:1001 --cap-drop ALL \
	--security-opt no-new-privileges:true --security-opt "seccomp=$SCRIPT_DIR/seccomp-browser.json" \
	--memory 3g --memory-swap 3g --cpus 2 --pids-limit 256 --ulimit core=0 \
	--tmpfs /tmp:rw,noexec,nosuid,nodev,size=256m,uid=1001,gid=1001 \
	--tmpfs /home/pwuser:rw,noexec,nosuid,nodev,size=64m,uid=1001,gid=1001 \
	--tmpfs /dev/shm:rw,noexec,nosuid,nodev,size=128m,uid=1001,gid=1001 \
	--env HOME=/tmp --env TMPDIR=/tmp \
	--env http_proxy= --env https_proxy= --env HTTP_PROXY= --env HTTPS_PROXY= \
	--env ALL_PROXY= --env all_proxy= --env NO_PROXY= --env no_proxy= \
	--entrypoint node "$IMAGE" -e 'setInterval(() => {}, 1 << 30)' >/dev/null
docker exec -i "$BROWSER" /bin/sh -c 'umask 077; cat > /tmp/probe.cjs' < "$SCRIPT_DIR/probe.cjs"
docker exec -i "$BROWSER" /bin/sh -c 'umask 077; cat > /tmp/artifact_response_policy.cjs' < "$SCRIPT_DIR/artifact_response_policy.cjs"

network_line() {
	docker inspect --format '{{range $name, $value := .NetworkSettings.Networks}}{{$name}}={{$value.IPAddress}};{{end}}' "$1" | tr ';' '\n' | sed '/^$/d' | sort | tr '\n' ';'
}
assert_networks() {
	local container=$1 expected=$2 actual bindings
	actual="$(network_line "$container")"
	if [[ $actual != "$expected" ]]; then
		printf 'unexpected network membership for %s\n' "$container" >&2
		return 1
	fi
	bindings="$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$container")"
	if [[ $bindings != null && $bindings != '{}' ]]; then
		printf 'published port found on %s\n' "$container" >&2
		return 1
	fi
}

BROWSER_IP="$(docker inspect --format "{{(index .NetworkSettings.Networks \"$BROWSER_NET\").IPAddress}}" "$BROWSER")"
DATABASE_IP="$(docker inspect --format "{{(index .NetworkSettings.Networks \"$SERVER_NET\").IPAddress}}" "$DATABASE")"
CLIENT_IP="$(docker inspect --format "{{(index .NetworkSettings.Networks \"$SERVER_NET\").IPAddress}}" "$CLIENT")"
assert_networks "$DATABASE" "$SERVER_NET=$DATABASE_IP;"
assert_networks "$BACKEND" "$SERVER_NET=$BACKEND_IP;"
assert_networks "$CLIENT" "$SERVER_NET=$CLIENT_IP;"
assert_networks "$FRONT" "$BROWSER_NET=$FRONT_IP;$SERVER_NET=$FRONT_SERVER_IP;"
assert_networks "$BROWSER" "$BROWSER_NET=$BROWSER_IP;"

route_table="$(docker exec "$BROWSER" node -e "process.stdout.write(require('node:fs').readFileSync('/proc/net/route','utf8'))")"
if printf '%s\n' "$route_table" | awk 'NR > 1 && $2 == "00000000" { found=1 } END { exit !found }'; then
	printf 'browser IPv4 default route found\n' >&2
	exit 1
fi
ipv6_route_table="$(docker exec "$BROWSER" node -e "process.stdout.write(require('node:fs').readFileSync('/proc/net/ipv6_route','utf8'))")"
ipv6_defaults="$(printf '%s\n' "$ipv6_route_table" | awk 'NF >= 10 && $1 == "00000000000000000000000000000000" && $2 == "00" { print $9, $10 }')"
if [[ -n $ipv6_defaults ]]; then
	while read -r route_flags route_device; do
		if [[ $route_device != lo ]] || (( (16#$route_flags & 512) == 0 )); then
			printf 'browser IPv6 default route found\n' >&2
			exit 1
		fi
	done <<< "$ipv6_defaults"
fi
if docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$BROWSER" | grep -Eiq '^(http_proxy|https_proxy|HTTP_PROXY|HTTPS_PROXY|ALL_PROXY|all_proxy|NO_PROXY|no_proxy)=[^[:space:]]+'; then
	printf 'browser proxy environment is configured\n' >&2
	exit 1
fi

run_browser_probe() {
	local injection=$1 output=$2 status=0 expected_wss_before_browser=$CLIENT_WSS_BASELINE
	if [[ $injection == 1 ]]; then expected_wss_before_browser=$((expected_wss_before_browser + 1)); fi
	if timeout 90 docker exec \
		--env FRONT_IP="$FRONT_IP" --env BACKEND_IP="$BACKEND_IP" --env DATABASE_IP="$DATABASE_IP" \
		--env TLS_SPKI="$TLS_SPKI" --env INJECT_UNEXPECTED="$injection" \
		--env EXPECTED_WSS_BEFORE_BROWSER="$expected_wss_before_browser" \
		--env MTPROTO_ENDPOINT="$ENDPOINT" --env MTPROTO_FINGERPRINT="$FINGERPRINT" \
		--env MTPROTO_PUBLIC_KEY_SHA256="$PUBLIC_KEY_SHA256" \
		"$BROWSER" node /tmp/probe.cjs > "$output" 2> "$SECRET_DIR/browser-$injection.stderr"; then
		status=0
	else
		status=$?
	fi
	return "$status"
}
if ! run_browser_probe 0 "$SECRET_DIR/browser-positive.json"; then
	printf 'page/worker egress or real-server WSS verification failed\n' >&2
	cat "$SECRET_DIR/browser-positive.json" >&2 2>/dev/null || true
	cat "$SECRET_DIR/browser-0.stderr" >&2
	exit 1
fi
if ! jq -e --arg endpoint "$ENDPOINT" --arg fingerprint "$FINGERPRINT" --arg publicKeySHA256 "$PUBLIC_KEY_SHA256" \
	'.status == "passed" and .http_status == 200 and .manifest_endpoint == $endpoint and .manifest_fingerprint == $fingerprint and .manifest_public_key_sha256 == $publicKeySHA256 and .wss_upgrade_status == 101 and .observed_allowed_wss == 1 and .worker_probes.page.attempted == 8 and .worker_probes.page.blocked == 8 and .worker_probes.shared_worker.attempted == 8 and .worker_probes.shared_worker.blocked == 8 and .worker_probes.service_worker.attempted == 8 and .worker_probes.service_worker.blocked == 8 and .observer_controlled_attempts.page == 8 and .observer_controlled_attempts.shared_worker == 8 and .observer_controlled_attempts.service_worker == 8 and .direct_tcp.attempted == 5 and .direct_tcp.blocked == 5 and (.observer_unexpected_attempts | length) == 0 and (.observer_errors | length) == 0' \
	"$SECRET_DIR/browser-positive.json" >/dev/null; then
	printf 'page/worker egress evidence was incomplete or unexpected\n' >&2
	cat "$SECRET_DIR/browser-positive.json" >&2
	exit 1
fi

negative_status=0
run_browser_probe 1 "$SECRET_DIR/browser-negative.json" || negative_status=$?
if [[ $negative_status != 86 ]] || ! jq -e \
	'.status == "expected_injected_failure" and .worker_probes.page.attempted == 8 and .worker_probes.page.blocked == 8 and .worker_probes.shared_worker.attempted == 8 and .worker_probes.shared_worker.blocked == 8 and .worker_probes.service_worker.attempted == 8 and .worker_probes.service_worker.blocked == 8 and (.observer_unexpected_attempts | length) == 6 and (.observer_errors | length) == 0' \
	"$SECRET_DIR/browser-negative.json" >/dev/null; then
	printf 'unexpected egress negative control was not detected as six independent attempts\n' >&2
	cat "$SECRET_DIR/browser-negative.json" >&2 2>/dev/null || true
	exit 1
fi

if [[ "$(printenv TEAGRAM_FIXTURE_TEST_FAIL_AFTER 2>/dev/null || true)" == authenticated ]]; then
	printf 'injected startup failure after authentication\n' >&2
	exit 42
fi
[[ $(docker inspect --format '{{.State.Status}}' "$BACKEND") == running ]]
[[ $(docker inspect --format '{{.State.Status}}' "$DATABASE") == running ]]
[[ $(docker inspect --format '{{.State.Status}}' "$FRONT") == running ]]
[[ $(docker inspect --format '{{.State.Status}}' "$BROWSER") == running ]]

FINAL_AUTH_KEYS="$(db_count 'SELECT count(*) FROM auth_keys WHERE user_id IS NOT NULL')"
[[ $FINAL_AUTH_KEYS == 2 ]]
SECURITY_JSON="$(jq -cn \
	--argjson ordinaryUsers "${USER_COUNTS%%|*}" --argjson usernameAccounts "$(cut -d '|' -f 2 <<<"$USER_COUNTS")" \
	--argjson passwordVerifiers "$(cut -d '|' -f 3 <<<"$USER_COUNTS")" \
	--argjson initialAuthKeys "$INITIAL_AUTH_KEYS" --argjson initialMessages "$INITIAL_MESSAGES" --argjson finalAuthKeys "$FINAL_AUTH_KEYS" \
	--argjson registrationClosed "$REGISTRATION_CLOSED_JSON" --argjson guestSignUpEnabled "$GUEST_SIGNUP_JSON" \
	--argjson turnstileTestMode "$TURNSTILE_TEST_MODE_JSON" \
	'{registrationClosed:$registrationClosed,guestSignUpEnabled:$guestSignUpEnabled,turnstileTestMode:$turnstileTestMode,loginCodeLogging:false,electionClosed:true,administratorIsNull:true,ordinaryUsers:$ordinaryUsers,usernameAccounts:$usernameAccounts,passwordVerifiers:$passwordVerifiers,initialAuthKeys:$initialAuthKeys,initialMessages:$initialMessages,finalAuthKeys:$finalAuthKeys}')"
EVIDENCE_JSON="$(jq -c --argjson detected true '{httpStatus:.http_status,wssUpgradeStatus:.wss_upgrade_status,allowedWssObserved:.observed_allowed_wss,workerProbes:.worker_probes,observerControlledAttempts:.observer_controlled_attempts,directTCP:.direct_tcp,unexpectedAttempts:(.observer_unexpected_attempts|length),unexpectedDetectionVerified:$detected}' "$SECRET_DIR/browser-positive.json")"
READY_JSON="$(jq -cn \
	--arg event server-ready --arg status ready --arg runId "$RUN_ID" --arg harnessRevision "$HARNESS_REVISION" \
	--arg serverRevision "$SERVER_REVISION" --arg webRevision "$WEB_REVISION" \
	--arg evidenceClass production-teagramd --arg endpoint "$ORIGIN" --arg wssEndpoint "$ENDPOINT" \
	--arg mtprotoPublicKeyPEM "$PUBLIC_KEY" --arg publicKeySHA256 "$PUBLIC_KEY_SHA256" --arg fingerprint "$FINGERPRINT" \
	--arg leafSPKI "$TLS_SPKI" --arg usernameA "$USER_A" --arg passwordFileA "$SECRET_DIR/a-password" \
	--arg usernameB "$USER_B" --arg passwordFileB "$SECRET_DIR/b-password" \
	--argjson security "$SECURITY_JSON" --argjson evidence "$EVIDENCE_JSON" \
	'{event:$event,status:$status,runId:$runId,harnessRevision:$harnessRevision,serverRevision:$serverRevision,webRevision:$webRevision,evidenceClass:$evidenceClass,endpoint:$endpoint,wssEndpoint:$wssEndpoint,mode:"private",mtprotoPublicKeyPEM:$mtprotoPublicKeyPEM,publicKeySHA256:$publicKeySHA256,fingerprint:$fingerprint,leafSPKI:$leafSPKI,credentials:[{username:$usernameA,passwordFile:$passwordFileA},{username:$usernameB,passwordFile:$passwordFileB}],security:$security,evidence:$evidence}')"
printf '%s\n' "$READY_JSON" >&3

if [[ "$(printenv TEAGRAM_FIXTURE_TEST_FAIL_AFTER 2>/dev/null || true)" == server-ready ]]; then
	printf 'injected startup failure after server-ready\n' >&2
	exit 42
fi

if ! IFS= read -r control_command; then
	printf 'fixture requires exactly one artifact attachment command\n' >&2
	exit 2
fi
if [[ $control_command != attach\ * ]]; then
	printf 'fixture control command must be one attach command\n' >&2
	exit 2
fi
ARTIFACT_SOURCE=${control_command#attach }
if [[ -z $ARTIFACT_SOURCE ]]; then
	printf 'artifact attachment path is empty\n' >&2
	exit 2
fi

STAGED_ARTIFACT="$BUILD_DIR/staged-artifact"
if ! ARTIFACT_AUDIT_JSON="$(python3 "$SCRIPT_DIR/artifact.py" stage \
	--source "$ARTIFACT_SOURCE" --destination "$STAGED_ARTIFACT" --secret-dir "$SECRET_DIR" \
	--build-dir "$BUILD_DIR" --repo-root "$REPO_ROOT" --endpoint "$ENDPOINT" \
	--fingerprint "$FINGERPRINT" --web-revision "$WEB_REVISION")"; then
	exit 1
fi
printf '%s\n' "$ARTIFACT_AUDIT_JSON" > "$SECRET_DIR/artifact-audit.json"
FROZEN_ARTIFACT_DIGEST="$(jq -er '.artifactDigest | select(type == "string" and test("^sha256:[0-9a-f]{64}$"))' "$SECRET_DIR/artifact-audit.json")"
ARTIFACT_DIGEST=$FROZEN_ARTIFACT_DIGEST
MANIFEST_ARTIFACT_DIGEST="$(jq -er '.manifestArtifactDigest | select(type == "string" and test("^sha256:[0-9a-f]{64}$"))' "$SECRET_DIR/artifact-audit.json")"
if [[ $AUTH_PROGRESS == 1 || $AUTH_PROGRESS_PREBASELINE_CONTROLS == 1 ]]; then
	if [[ $WEB_REVISION != "$AUTH_PROGRESS_ARTIFACT_SOURCE_REVISION" ]]; then
		printf 'auth-progress artifact source does not match the pinned web revision\n' >&2
		exit 1
	fi
	AUTH_PROGRESS_IDENTITY="$(jq -cn \
		--arg runId "$RUN_ID" --arg runnerRevision "$AUTH_PROGRESS_RUNNER_REVISION" \
		--arg artifactSourceRevision "$WEB_REVISION" --arg artifactDigest "$ARTIFACT_DIGEST" \
		--arg harnessRevision "$HARNESS_REVISION" --arg serverRevision "$SERVER_REVISION" \
		'{run_id:$runId,source_refs:{runner_revision:$runnerRevision,artifact_source_revision:$artifactSourceRevision,artifact_digest:$artifactDigest,harness_revision:$harnessRevision,server_revision:$serverRevision}}')"
	BINDING_EVENT="$(jq -cn --arg event artifact-binding --arg status ready --argjson identity "$AUTH_PROGRESS_IDENTITY" \
		'{event:$event,status:$status,identity:$identity}')"
	printf '%s\n' "$BINDING_EVENT" >&3
	AUTH_PROGRESS_BINDING_ATTEMPTED=1
	if ! IFS= read -r binding_command || [[ $binding_command != bind ]]; then
		printf 'fixture requires consumer acknowledgement before binding the artifact identity\n' >&2
		exit 1
	fi
	if [[ $AUTH_PROGRESS == 1 ]] && ! bind_auth_progress_identity "$AUTH_PROGRESS_IDENTITY"; then
		exit 1
	fi
fi

# The front container's rootfs is read-only, which makes docker cp refuse the
# copy; stream the staged tree into its /srv/artifact tmpfs instead. The exec
# runs as the container user that owns that tmpfs, so the tree arrives owned.
# The archive is created with writable directory modes so extraction can descend
# into them; the read-only modes are restored inside the container afterwards.
if ! tar -C "$STAGED_ARTIFACT" -cf - --mode=0755 . | docker exec -i "$FRONT" sh -c 'tar -xf - -C /srv/artifact'; then
	printf 'staged artifact could not be loaded into the front container\n' >&2
	exit 1
fi
docker exec "$FRONT" sh -c 'find /srv/artifact -mindepth 1 -type d -exec chmod 0555 {} + && find /srv/artifact -mindepth 1 -type f -exec chmod 0444 {} + && chmod 0555 /srv/artifact'
measure_front_artifact() {
	local snapshot=$1 digest
	local snapshot_directory="$BUILD_DIR/front-artifact-$snapshot"
	mkdir -m 0700 "$snapshot_directory"
	if docker exec "$FRONT" sh -c 'find /srv/artifact -perm /222 -print -quit | grep -q .' ; then
		printf 'served artifact contains writable entries\n' >&2
		return 1
	fi
	if ! docker exec "$FRONT" tar -C /srv/artifact -cf - . | tar -C "$snapshot_directory" -xf -; then
		printf 'fixture could not snapshot the read-only served artifact\n' >&2
		return 1
	fi
	find "$snapshot_directory" -type d -exec chmod 0555 {} +
	find "$snapshot_directory" -type f -exec chmod 0444 {} +
	chmod 0555 "$snapshot_directory"
	digest="$(python3 "$SCRIPT_DIR/artifact.py" digest --directory "$snapshot_directory" | jq -er '.artifactDigest')" || return 1
	printf '%s\n' "$digest"
}
# The old front process is reparented to the container's PID 1, which never
# reaps it, so `kill -0` would keep seeing a zombie; and the container's
# health status stays stale across an in-container restart. Watch the listener
# itself instead: wait for the port to close, then for the reloaded server to
# answer before any browser touches it.
front_listening() {
	docker exec "$FRONT" node -e 'require("https").get("https://127.0.0.1/healthz",{rejectUnauthorized:false,headers:{host:"teagramd.test"}},r=>process.exit(r.statusCode===200?0:1)).on("error",()=>process.exit(1))' >/dev/null 2>&1
}
docker exec "$FRONT" sh -c 'kill -TERM "$(cat /run/front.pid)" 2>/dev/null || true'
front_stopped=0
for attempt in $(seq 1 50); do
	if ! front_listening; then front_stopped=1; break; fi
	sleep 0.1
done
if ((front_stopped == 0)); then
	printf 'front server kept listening after the artifact reload signal\n' >&2
	exit 1
fi
LEG_DIGEST_BEFORE="$(measure_front_artifact before)" || exit 1
if [[ $LEG_DIGEST_BEFORE != "$FROZEN_ARTIFACT_DIGEST" ]]; then
	printf 'served artifact changed before this leg: expected=%s actual=%s\n' "$FROZEN_ARTIFACT_DIGEST" "$LEG_DIGEST_BEFORE" >&2
	exit 1
fi
docker exec --detach --user 1001:1001 "$FRONT" sh -c 'node /opt/real-server-fixture/front.mjs --artifact-dir /srv/artifact >/run/front.log 2>&1 & echo $! > /run/front.pid'
front_serving=0
for attempt in $(seq 1 100); do
	if front_listening; then front_serving=1; break; fi
	sleep 0.1
done
if ((front_serving == 0)); then
	printf 'front server did not serve the staged artifact\n' >&2
	docker exec "$FRONT" cat /run/front.log >&2 2>/dev/null || true
	exit 1
fi

run_artifact_browser_probe() {
	local output=$1 status=0
	if timeout 90 docker exec \
		--env FRONT_IP="$FRONT_IP" --env TLS_SPKI="$TLS_SPKI" --env ARTIFACT_PROBE=1 \
		--env MTPROTO_ENDPOINT="$ENDPOINT" --env MTPROTO_FINGERPRINT="$FINGERPRINT" \
		--env WEB_REVISION="$WEB_REVISION" \
		--env ARTIFACT_INDEX_SHA256="$(jq -r '.indexSHA256' "$SECRET_DIR/artifact-audit.json")" \
		--env ARTIFACT_MANIFEST_SHA256="$(jq -r '.manifestSHA256' "$SECRET_DIR/artifact-audit.json")" \
		"$BROWSER" node /tmp/probe.cjs > "$output" 2> "$SECRET_DIR/browser-artifact.stderr"; then
		status=0
	else
		status=$?
	fi
	return "$status"
}
if ! run_artifact_browser_probe "$SECRET_DIR/browser-artifact.json"; then
	printf 'production artifact browser verification failed\n' >&2
	cat "$SECRET_DIR/browser-artifact.json" >&2 2>/dev/null || true
	cat "$SECRET_DIR/browser-artifact.stderr" >&2
	exit 1
fi
ARTIFACT_INDEX_SHA256="$(jq -r '.indexSHA256' "$SECRET_DIR/artifact-audit.json")"
ARTIFACT_MANIFEST_SHA256="$(jq -r '.manifestSHA256' "$SECRET_DIR/artifact-audit.json")"
if ! jq -e --arg entry "$ARTIFACT_INDEX_SHA256" --arg manifest "$ARTIFACT_MANIFEST_SHA256" \
	'.status == "passed" and .entry_sha256 == $entry and .manifest_sha256 == $manifest and .entry_response_status == 200 and .manifest_response_status == 200 and .artifact_responses > 0 and .artifact_responses_with_private_csp == .artifact_responses and .worker_targets.shared_worker > 0 and .worker_targets.service_worker > 0 and .unexpected_attempts == 0 and .observer_errors == 0' \
	"$SECRET_DIR/browser-artifact.json" >/dev/null; then
	printf 'production artifact browser evidence did not satisfy the accepted contract\n' >&2
	cat "$SECRET_DIR/browser-artifact.json" >&2
	exit 1
fi

AUDIT_CHECKS="$(jq -c '.checks' "$SECRET_DIR/artifact-audit.json")"
FROZEN_ARTIFACT_DIGEST="$(jq -r '.artifactDigest' "$SECRET_DIR/artifact-audit.json")"
ARTIFACT_FILE_COUNT="$(jq -r '.fileCount' "$SECRET_DIR/artifact-audit.json")"
ARTIFACT_TOTAL_BYTES="$(jq -r '.totalBytes' "$SECRET_DIR/artifact-audit.json")"
PRODUCT_REFERENCES="$(jq -c '.productReferences' "$SECRET_DIR/artifact-audit.json")"
PRODUCT_REFERENCE_COUNT="$(jq -r '.productReferenceCount' "$SECRET_DIR/artifact-audit.json")"
BROWSER_EVIDENCE="$(jq -c \
	'{status:.status,entrySHA256:.entry_sha256,manifestSHA256:.manifest_sha256,entryResponseStatus:.entry_response_status,manifestResponseStatus:.manifest_response_status,artifactResponses:.artifact_responses,artifactResponsesWithPrivateCSP:.artifact_responses_with_private_csp,workerTargets:.worker_targets,unexpectedAttempts:.unexpected_attempts,observerErrors:.observer_errors}' \
	"$SECRET_DIR/browser-artifact.json")"
run_auth_progress_controls() {
	local account username password_file control_output
	for account in a b; do
		if [[ $account == a ]]; then username=$USER_A; password_file=/run/secrets/a-password; else username=$USER_B; password_file=/run/secrets/b-password; fi
		if ! control_output="$(docker exec "$CLIENT" /run/app/fixture-auth-check --endpoint "$ENDPOINT" --tls-root-file /run/secrets/tls.crt --public-key /run/secrets/server.pub.pem --username "$username" --password-file "$password_file")" || [[ $control_output != authentication=authorized ]]; then
			printf 'auth-progress positive authentication control failed\n' >&2
			return 1
		fi
	done
	if ! control_output="$(docker exec "$CLIENT" /run/app/fixture-auth-check --endpoint "$ENDPOINT" --tls-root-file /run/secrets/tls.crt --public-key /run/secrets/server.pub.pem --closed-signup)" || [[ $control_output != registration=closed ]]; then
		printf 'auth-progress closed-signup control failed\n' >&2
		return 1
	fi
	AUTH_CONTROLS_JSON="$(jq -cn \
		--arg runId "$RUN_ID" --arg runnerRevision "$AUTH_PROGRESS_RUNNER_REVISION" \
		--arg artifactSourceRevision "$WEB_REVISION" --arg artifactDigest "$ARTIFACT_DIGEST" \
		--arg harnessRevision "$HARNESS_REVISION" --arg serverRevision "$SERVER_REVISION" \
		'{event:"auth-controls",status:"passed",identity:{run_id:$runId,source_refs:{runner_revision:$runnerRevision,artifact_source_revision:$artifactSourceRevision,artifact_digest:$artifactDigest,harness_revision:$harnessRevision,server_revision:$serverRevision}},results:{"auth.sendCode":{result:"success",accounts:2},"auth.signIn":{result:"password_required",accounts:2},"account.getPassword":{result:"success",accounts:2},"auth.checkPassword":{result:"success",accounts:2},"auth.signUp":{result:"invalid_request",accounts:1}}}')"
	printf '%s\n' "$AUTH_CONTROLS_JSON" >&3
}

if [[ $AUTH_CONTROLS == 1 && $AUTH_PROGRESS_PREBASELINE_CONTROLS == 1 ]]; then
	run_auth_progress_controls
fi
if [[ $AUTH_PROGRESS == 1 ]]; then
	if [[ $WEB_REVISION != "$AUTH_PROGRESS_ARTIFACT_SOURCE_REVISION" ]]; then
		printf 'auth-progress artifact source does not match the pinned web revision: source=%s\n' "$WEB_REVISION" >&2
		exit 1
	fi
	if ! signal_auth_progress_boundary artifact_ready 1; then
		printf 'fixture could not capture its artifact-ready auth-progress boundary\n' >&2
		exit 1
	fi
	AUTH_PROGRESS_BASELINE_CAPTURED=1
fi
ARTIFACT_READY_JSON="$(jq -cn \
	--arg event artifact-ready --arg status ready --arg runId "$RUN_ID" --arg harnessRevision "$HARNESS_REVISION" \
	--arg serverRevision "$SERVER_REVISION" --arg webRevision "$WEB_REVISION" --arg endpoint "$ORIGIN" --arg wssEndpoint "$ENDPOINT" \
	--arg fingerprint "$FINGERPRINT" --arg artifactDigest "$ARTIFACT_DIGEST" --arg manifestArtifactDigest "$MANIFEST_ARTIFACT_DIGEST" --arg frozenArtifactDigest "$FROZEN_ARTIFACT_DIGEST" --arg legDigestBefore "$LEG_DIGEST_BEFORE" \
	--arg manifestSHA256 "$ARTIFACT_MANIFEST_SHA256" --arg indexSHA256 "$ARTIFACT_INDEX_SHA256" \
	--argjson fileCount "$ARTIFACT_FILE_COUNT" --argjson totalBytes "$ARTIFACT_TOTAL_BYTES" \
	--argjson productReferences "$PRODUCT_REFERENCES" --argjson productReferenceCount "$PRODUCT_REFERENCE_COUNT" \
	--argjson auditChecks "$AUDIT_CHECKS" --argjson browser "$BROWSER_EVIDENCE" \
	'{event:$event,status:$status,runId:$runId,harnessRevision:$harnessRevision,serverRevision:$serverRevision,webRevision:$webRevision,endpoint:$endpoint,wssEndpoint:$wssEndpoint,fingerprint:$fingerprint,artifactDigest:$artifactDigest,manifestArtifactDigest:$manifestArtifactDigest,frozenArtifactDigest:$frozenArtifactDigest,legDigestBefore:$legDigestBefore,manifestSHA256:$manifestSHA256,indexSHA256:$indexSHA256,fileCount:$fileCount,totalBytes:$totalBytes,auditChecks:$auditChecks,productReferences:$productReferences,productReferenceCount:$productReferenceCount,browser:$browser}')"
printf '%s\n' "$ARTIFACT_READY_JSON" >&3

if [[ $AUTH_CONTROLS == 1 && $AUTH_PROGRESS_PREBASELINE_CONTROLS != 1 ]]; then
	run_auth_progress_controls
fi

while IFS= read -r control_command; do
	case "$control_command" in
		stop|"") break ;;
		*) printf 'unknown fixture control command\n' >&2; exit 2 ;;
	esac
done

if [[ $GUEST_SIGNUP == 1 ]]; then
	POST_GUEST_USER_COUNTS="$(db_count "SELECT count(*) || '|' || count(*) FILTER (WHERE login_mode = 'username') || '|' || (SELECT count(*) FROM user_passwords) FROM users")"
	POST_GUEST_HANDLE_COUNT="$(db_count "SELECT count(*) FROM usernames WHERE handle ~ '^guest_[a-z2-7]{16}$'")"
	POST_BASE_HANDLE_COUNT="$(db_count "SELECT count(*) FROM usernames WHERE handle IN ('$USER_A','$USER_B')")"
	POST_GUEST_AUTH_KEYS="$(db_count 'SELECT count(*) FROM auth_keys WHERE user_id IS NOT NULL')"
	POST_GUEST_MESSAGES="$(db_count 'SELECT count(*) FROM messages')"
	SITEVERIFY_REQUEST_COUNT="$(docker exec "$FRONT" node -e 'require("node:https").get("https://127.0.0.1/healthz",{rejectUnauthorized:false,headers:{host:"teagramd.test"}},response=>response.pipe(process.stdout)).on("error",()=>process.exit(1))' | jq -er '.siteverify_request_count | select(type == "number" and . == floor and . >= 0)')"
	if [[ $POST_GUEST_USER_COUNTS != '3|3|2' || $POST_GUEST_HANDLE_COUNT != 1 || $POST_BASE_HANDLE_COUNT != 2 ||
		$POST_GUEST_AUTH_KEYS != 3 || $POST_GUEST_MESSAGES != 0 || $SITEVERIFY_REQUEST_COUNT != 1 ]]; then
		printf 'guest signup evidence failed: users=%s guest_handles=%s base_handles=%s auth_keys=%s messages=%s siteverify=%s\n' \
			"$POST_GUEST_USER_COUNTS" "$POST_GUEST_HANDLE_COUNT" "$POST_BASE_HANDLE_COUNT" \
			"$POST_GUEST_AUTH_KEYS" "$POST_GUEST_MESSAGES" "$SITEVERIFY_REQUEST_COUNT" >&2
		exit 1
	fi
fi

if [[ $AUTH_CONTROLS == 1 ]]; then
	if resource_owner container "$BROWSER"; then
		if [[ $RESOURCE_OWNER != "$OWNER_TOKEN" ]] || ! docker stop --time 5 "$BROWSER" >/dev/null 2>&1; then
			printf 'fixture could not stop the owned browser leg before its final artifact check\n' >&2
			exit 1
		fi
		BROWSER_STOPPED=1
	else
		inspect_status=$?
		if ((inspect_status != 1)); then
			printf 'fixture could not verify browser leg completion before its final artifact check\n' >&2
			exit 1
		fi
		BROWSER_STOPPED=1
	fi
	LEG_DIGEST_AFTER="$(measure_front_artifact after)" || exit 1
	if [[ $LEG_DIGEST_AFTER != "$FROZEN_ARTIFACT_DIGEST" ]]; then
		printf 'served artifact changed during this leg: expected=%s actual=%s\n' "$FROZEN_ARTIFACT_DIGEST" "$LEG_DIGEST_AFTER" >&2
		exit 1
	fi
	if [[ $AUTH_PROGRESS == 1 ]]; then
		ENDED_IDENTITY=$AUTH_PROGRESS_IDENTITY
	else
		ENDED_IDENTITY="$(jq -cn \
			--arg runId "$RUN_ID" --arg runnerRevision "$AUTH_PROGRESS_RUNNER_REVISION" \
			--arg artifactSourceRevision "$WEB_REVISION" --arg artifactDigest "$ARTIFACT_DIGEST" \
			--arg harnessRevision "$HARNESS_REVISION" --arg serverRevision "$SERVER_REVISION" \
			'{run_id:$runId,source_refs:{runner_revision:$runnerRevision,artifact_source_revision:$artifactSourceRevision,artifact_digest:$artifactDigest,harness_revision:$harnessRevision,server_revision:$serverRevision}}')"
	fi
	ARTIFACT_ENDED_JSON="$(jq -cn --arg event artifact-leg-ended --arg status verified \
		--argjson identity "$ENDED_IDENTITY" --arg legDigestBefore "$LEG_DIGEST_BEFORE" --arg legDigestAfter "$LEG_DIGEST_AFTER" \
		'{event:$event,status:$status,identity:$identity,legDigestBefore:$legDigestBefore,legDigestAfter:$legDigestAfter}')"
	printf '%s\n' "$ARTIFACT_ENDED_JSON" >&3
fi
