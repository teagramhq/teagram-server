#!/usr/bin/env bash
set -euo pipefail

if [ "${GITHUB_ACTIONS:-}" != true ] || [ "${RUNNER_ENVIRONMENT:-}" != github-hosted ]; then
	echo "public WebSocket smoke requires its disposable GitHub-hosted daemon" >&2
	exit 2
fi

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$repo_root"

# This disposable local-mode proof must not inherit a real Turnstile secret.
unset TEAGRAM_TURNSTILE_SITE_KEY TEAGRAM_TURNSTILE_SECRET_KEY_FILE TG_TURNSTILE_SIGNIN_ENABLED

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT
umask 077
env_file="$work_dir/.env"
project="public-websocket-proof-${GITHUB_RUN_ID:-local}-$$"
public_ws_pid_a=
public_ws_pid_b=
public_smoke_started=0
test_haproxy_name="${project}-attribution-proxy"
test_echo_name="${project}-attribution-echo"

mapfile -t existing_network_ids < <(docker network ls --quiet)
if [ "${#existing_network_ids[@]}" -eq 0 ]; then
	echo "the disposable daemon has no Docker networks to inspect" >&2
	exit 1
fi
docker network inspect "${existing_network_ids[@]}" > "$work_dir/networks.json"
ip -j route show > "$work_dir/routes.json"
python3 - "$work_dir/networks.json" "$work_dir/routes.json" <<'PY'
import ipaddress
import json
import sys

networks, routes = [json.load(open(path, encoding="utf-8")) for path in sys.argv[1:]]
occupied = []
for network in networks:
    ipam = network.get("IPAM") or {}
    for config in ipam.get("Config") or []:
        subnet = config.get("Subnet")
        if subnet:
            occupied.append(ipaddress.ip_network(subnet, strict=False))
for route in routes:
    destination = route.get("dst")
    if destination and destination != "default":
        try:
            occupied.append(ipaddress.ip_network(destination, strict=False))
        except ValueError:
            pass
for reserved in (ipaddress.ip_network("198.18.131.0/24"), ipaddress.ip_network("198.18.132.0/24")):
    if any(reserved.overlaps(existing) for existing in occupied):
        raise SystemExit(f"public-WebSocket test subnet {reserved} overlaps an existing network or host route")
PY

docker ps --format '{{.Ports}}' > "$work_dir/docker-ports.txt"
ss -H -ltn > "$work_dir/listening-ports.txt"
mapfile -t test_ports < <(python3 - "$work_dir/docker-ports.txt" "$work_dir/listening-ports.txt" <<'PY'
import re
import sys

docker_ports, listeners = [open(path, encoding="utf-8").read() for path in sys.argv[1:]]
used = set()
for match in re.finditer(r":(\d+)(?:-(\d+))?->", docker_ports):
    first, last = int(match.group(1)), int(match.group(2) or match.group(1))
    used.update(range(first, last + 1))
for match in re.finditer(r"(?:\]|:)(\d+)\s", listeners):
    used.add(int(match.group(1)))

for base in range(39000, 50000):
    selected = [base, base + 1]
    if not used.intersection(selected):
        print("\n".join(map(str, selected)))
        break
else:
    raise SystemExit("no non-overlapping host ports are available in 39000-50000")
PY
)
if [ "${#test_ports[@]}" -ne 2 ]; then
	echo "could not select non-overlapping public WebSocket host ports" >&2
	exit 1
fi

cat > "$env_file" <<EOF
POSTGRES_PASSWORD=localdev
TG_PUBLIC_LINK_PREFIX=https://links.example.test/
TG_LOG_LOGIN_CODES=false
TG_REGISTRATION=open
TG_AUTHKEY_ENC_KEY=
TG_AUTHKEY_ENC_KEY_FILE=/var/lib/teagramd/enc_key.hex
TG_REPLICA_ID=base
TG_REPLICA_COUNT=3
TG_ADVERTISE_ADDR=
TG_WEBSOCKET_ALLOWED_ORIGINS=https://web.example.test
TEAGRAM_TURNSTILE_SITE_KEY=
TEAGRAM_TURNSTILE_SECRET_KEY_FILE=
TG_TURNSTILE_SIGNIN_ENABLED=false
TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=:2446
TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR=server.example.test:443
TG_PUBLIC_REGISTRATION=invite
TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS=https://web.example.test
TG_PUBLIC_HAPROXY_HOST_PORT=${test_ports[0]}
EOF
chmod 600 "$env_file"

compose_base() {
	docker compose --project-name "$project" --env-file .env --env-file "$env_file" \
		-f docker-compose.yml -f docker-compose.local-blobs.yml "$@"
}

compose_public() {
	docker compose --project-name "$project" --env-file .env --env-file "$env_file" \
		--profile public -f docker-compose.yml -f docker-compose.local-blobs.yml \
		-f docker-compose.public-websocket.local-validation.yml "$@"
}

public_advertise_override="$work_dir/public-advertise-override.yml"
cat > "$public_advertise_override" <<'YAML'
services:
  teagramd:
    environment:
      TG_ADVERTISE_ADDR: tailnet.example.test:2443
YAML
compose_public_with_advertise_override() {
	compose_public -f "$public_advertise_override" "$@"
}

stop_probe() {
	pid=$1
	if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
		kill "$pid"
		wait "$pid" || true
	fi
}

cleanup() {
	status=$?
	if [ "$status" -ne 0 ] && [ "$public_smoke_started" -eq 1 ]; then
		echo "public WebSocket smoke diagnostics:" >&2
		compose_public ps -a >&2 || true
		compose_public logs --no-color public-haproxy teagramd-public-1 teagramd-public-2 >&2 || true
		for probe_log in "$work_dir"/public-ws-*.log; do
			if [ ! -f "$probe_log" ]; then
				continue
			fi
			echo "public probe output ($probe_log):" >&2
			cat "$probe_log" >&2 || true
		done
		for diagnostic_container in "$test_haproxy_name" "$test_echo_name"; do
			if docker inspect "$diagnostic_container" >/dev/null 2>&1; then
				echo "$diagnostic_container logs:" >&2
				docker logs --timestamps "$diagnostic_container" >&2 || true
			fi
		done
	fi
	docker rm -f "$test_haproxy_name" "$test_echo_name" >/dev/null 2>&1 || true
	stop_probe "$public_ws_pid_a"
	stop_probe "$public_ws_pid_b"
	compose_public down --volumes --remove-orphans >/dev/null 2>&1 || true
	rm -rf "$work_dir"
	exit "$status"
}
trap cleanup EXIT
if [ "$(stat -c '%a' "$env_file")" != 600 ]; then
	echo "disposable Compose environment must be mode 0600" >&2
	exit 1
fi

if ! docker image inspect teagramd:local >/dev/null 2>&1; then
	docker build -t teagramd:local .
fi

compose_default_json="$work_dir/default-compose.json"
docker compose --project-name "$project" --env-file .env --env-file "$env_file" \
	-f docker-compose.yml config --format json > "$compose_default_json"
compose_base config --format json > "$work_dir/base-compose.json"
compose_public config --format json > "$work_dir/public-compose.json"
python3 -B deploy/teagramd/rollout-runner/public-websocket-check.py \
	--haproxy-config deploy/teagramd/haproxy-public-websocket.cfg \
	--host-port "${test_ports[0]}" \
	< "$work_dir/public-compose.json"
compose_public_with_advertise_override config --format json > "$work_dir/public-override-compose.json"
public_advertise_addr=$(jq -er '.services.teagramd.environment.TG_ADVERTISE_ADDR' \
	"$work_dir/public-override-compose.json")
TG_ADVERTISE_ADDR="$public_advertise_addr" compose_public_with_advertise_override \
	config --format json > "$work_dir/public-override-resolved-compose.json"
python3 -B deploy/teagramd/rollout-runner/public-websocket-check.py \
	--haproxy-config deploy/teagramd/haproxy-public-websocket.cfg \
	--host-port "${test_ports[0]}" \
	< "$work_dir/public-override-resolved-compose.json"
jq -e --arg advertise_addr "$public_advertise_addr" '
	(.services.teagramd.environment.TG_ADVERTISE_ADDR == $advertise_addr)
	and (.services["teagramd-public-1"].environment.TG_ADVERTISE_ADDR == $advertise_addr)
	and (.services["teagramd-public-2"].environment.TG_ADVERTISE_ADDR == $advertise_addr)
' "$work_dir/public-override-resolved-compose.json" >/dev/null

python3 - "$compose_default_json" "$work_dir/base-compose.json" "$work_dir/public-compose.json" "${test_ports[0]}" <<'PY'
import json
import sys

default, base_config, public = [json.load(open(path, encoding="utf-8")) for path in sys.argv[1:4]]
assert "tcp-proxy" not in default["services"]
assert "teagramd-proxy-1" not in default["services"]
assert "teagramd-proxy-2" not in default["services"]
assert "public-haproxy" not in default["services"]
assert not any(
    str(port.get("published")) == "2446" and str(port.get("target")) == "2446"
    for port in default["services"]["teagramd"].get("ports", [])
)
assert default["services"]["teagramd"]["environment"]["TG_PUBLIC_WEBSOCKET_LISTEN_ADDR"] == ""
assert default["services"]["teagramd"]["environment"]["TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR"] == ""
assert any(
    port.get("host_ip") == "127.0.0.1"
    and str(port.get("published")) == "2443"
    and str(port.get("target")) == "2443"
    for port in default["services"]["teagramd"]["ports"]
)
base = base_config["services"]["teagramd"]
assert base["environment"]["TG_CLIENT_ADDR_TRUST"] == "socket"
assert base["environment"]["TG_PUBLIC_WEBSOCKET_LISTEN_ADDR"] == ""
assert base["environment"]["TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR"] == ""
assert not any(key.startswith("TG_BLOB_S3_") for key in base["environment"])
services = public["services"]
public_base = services["teagramd"]
proxy = services["public-haproxy"]
assert public["networks"]["public-ingress"].get("internal", False) is False
assert public_base["environment"]["TG_CLIENT_ADDR_TRUST"] == "socket"
assert public_base["environment"]["TG_PUBLIC_WEBSOCKET_LISTEN_ADDR"] == ""
assert public_base["environment"]["TG_REPLICA_COUNT"] == "1"
assert public_base["environment"]["TG_RATE_LIMIT_KEY_EXCHANGE_IP"] == ""
assert not public_base.get("ports")
assert proxy["image"].split("@sha256:")[-1] and "@sha256:" in proxy["image"]
assert proxy["networks"]["public-ingress"]["ipv4_address"] == "198.18.131.10"
assert proxy["networks"]["public-backend"]["ipv4_address"] == "198.18.132.10"
proxy_mounts = proxy.get("volumes", [])
assert len(proxy_mounts) == 1
proxy_mount = proxy_mounts[0]
assert proxy_mount["type"] == "bind"
assert proxy_mount["source"].endswith("/deploy/teagramd/haproxy-public-websocket.cfg")
assert proxy_mount["target"] == "/usr/local/etc/haproxy/haproxy.cfg"
assert proxy_mount["read_only"] is True
assert proxy_mount["bind"].get("create_host_path") is False
assert any(
    port.get("host_ip") == "127.0.0.1"
    and str(port.get("published")) == sys.argv[4]
    and str(port.get("target")) == "2446"
    for port in proxy["ports"]
)
for name, expected_ip in (("teagramd-public-1", "198.18.132.21"), ("teagramd-public-2", "198.18.132.22")):
    replica = services[name]
    environment = replica["environment"]
    assert environment["TG_CLIENT_ADDR_TRUST"] == "proxy-v2"
    assert environment["TG_CLIENT_ADDR_PROXY_CIDRS"] == "198.18.132.10/32"
    assert environment["TG_PUBLIC_WEBSOCKET_LISTEN_ADDR"] == ":2446"
    assert environment["TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR"] == "server.example.test:443"
    assert environment["TG_PUBLIC_REGISTRATION"] == "invite"
    assert environment["TG_REGISTRATION"] == "open"
    assert environment["TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS"] == "https://web.example.test"
    assert environment["TG_WEBSOCKET_LISTEN_ADDR"] == ""
    assert environment["TG_WEBSOCKET_ALLOWED_ORIGINS"] == ""
    assert environment["TG_REPLICA_COUNT"] == "3"
    # gotd's initial PFS connection performs two exchanges: permanent and temporary.
    assert environment["TG_RATE_LIMIT_KEY_EXCHANGE_IP"] == "2"
    assert replica.get("ports", []) == []
    assert replica["networks"]["public-backend"]["ipv4_address"] == expected_ip
    assert "public-ingress" not in replica["networks"]
ingress_members = {name for name, service in services.items() if "public-ingress" in service.get("networks", {})}
assert ingress_members == {"public-haproxy"}
for name in ("teagramd-public-1", "teagramd-public-2"):
    env = services[name]["environment"]
    assert env["TG_BLOB_DIR"] == public_base["environment"]["TG_BLOB_DIR"]
    for key in (
        "TG_BLOB_S3_ENDPOINT", "TG_BLOB_S3_BUCKET", "TG_BLOB_S3_PREFIX",
        "TG_BLOB_S3_REGION", "TG_BLOB_S3_ACCESS_KEY_ID",
        "TG_BLOB_S3_SECRET_ACCESS_KEY", "TG_BLOB_S3_SECRET_ACCESS_KEY_FILE",
        "TG_BLOB_S3_CA_PATH", "TG_BLOB_S3_ALLOW_INSECURE_HTTP",
        "TG_MEDIA_KEY_DIR", "TG_MEDIA_KEY_CURRENT",
    ):
        assert env.get(key, "") == ""
    assert services[name].get("secrets") == public_base.get("secrets")
    for key in ("TG_TURNSTILE_SITE_KEY", "TG_TURNSTILE_SECRET_KEY_FILE", "TG_TURNSTILE_SIGNIN_ENABLED"):
        assert env[key] == public_base["environment"][key]
print("default Compose stays direct; public Compose isolates invite-only WebSocket listeners behind the pinned proxy-v2 frontend")
PY

python3 - deploy/teagramd/haproxy-public-websocket.cfg <<'PY'
import sys

path = sys.argv[1]
content = open(path, encoding="utf-8").read()
frontend = content.split("frontend public_websocket\n", 1)[1].split("\nbackend ", 1)[0]
backend = content.split("backend public_websocket_replicas\n", 1)[1].split("\nfrontend ", 1)[0]
rules = [line.strip() for line in frontend.splitlines() if line.strip().startswith(("tcp-request", "http-request"))]
assert rules[0] == "tcp-request connection reject unless { src 198.18.131.1/32 }"
assert rules[1] == "tcp-request connection set-var(sess.peer) src"
assert rules[2] == "http-request deny deny_status 400 unless { req.hdr_cnt(CF-Connecting-IP) eq 1 }"
assert rules[3] == "http-request deny deny_status 400 unless { req.hdr_ip(CF-Connecting-IP) -m found }"
assert rules[4] == "http-request set-src req.hdr_ip(CF-Connecting-IP)"
assert "http-reuse never" in backend
assert "send-proxy-v2" in backend
assert "X-Forwarded-For" not in content
assert "capture request header CF-Connecting-IP len 64" in frontend
assert 'public_peer=%[var(sess.peer)]' in content
assert 'client=%[capture.req.hdr(0)]' in content
print("public HAProxy pins its socket peer, validates one Cloudflare address, and disables backend reuse")
PY
compose_public run --rm --no-deps public-haproxy haproxy -c -f /usr/local/etc/haproxy/haproxy.cfg

bootstrap_output=$(compose_base run --rm --no-deps -T teagramd bootstrap-identity)
fingerprint=$(printf '%s\n' "$bootstrap_output" | sed -n 's/^RSA identity bootstrapped: fingerprint=\(-\?[0-9][0-9]*\) path=.*/\1/p')
unset bootstrap_output
if [ -z "$fingerprint" ]; then
	echo "fresh test identity bootstrap did not report a fingerprint" >&2
	exit 1
fi
printf 'TG_RSA_KEY_FINGERPRINT=%s\n' "$fingerprint" >> "$env_file"

compose_base create teagramd >/dev/null
base_container=$(compose_base ps -aq teagramd | head -1)
key_volume=$(docker inspect "$base_container" --format '{{range .Mounts}}{{if and (eq .Type "volume") (eq .Destination "/var/lib/teagramd")}}{{.Name}}{{end}}{{end}}')
if [ -z "$key_volume" ]; then
	echo "base service has no test-owned key volume" >&2
	exit 1
fi
docker run --rm --user 65532:65532 -v "$key_volume:/key" \
	mirror.gcr.io/library/busybox:1.37 sh -ec \
	'umask 077; od -An -N32 -tx1 /dev/urandom | tr -d " \n" > /key/enc_key.hex; chmod 600 /key/enc_key.hex'

probe_binary="$work_dir/public-websocket-probe"
CGO_ENABLED=0 go build -o "$probe_binary" ./cmd/public-websocket-probe
chmod 755 "$probe_binary"

start_hold() {
	local transport_name=$1
	local address=$2
	local log_file=$3
	local config_path=$4
	shift 4
	"$probe_binary" hold --transport "$transport_name" --address "$address" \
		--config "$config_path" "$@" > "$log_file" 2>&1 &
	started_hold_pid=$!
}

wait_hold() {
	pid=$1
	log_file=$2
	for _ in $(seq 60); do
		if grep -q '^stream-ready ' "$log_file"; then
			cat "$log_file"
			return 0
		fi
		if ! kill -0 "$pid" 2>/dev/null; then
			cat "$log_file" >&2
			return 1
		fi
		sleep 1
	done
	cat "$log_file" >&2
	echo "public WebSocket client did not become ready" >&2
	return 1
}

heartbeat_count() {
	grep -c '^stream-heartbeat ' "$1" || true
}

wait_for_heartbeat_after() {
	local pid=$1
	local log_file=$2
	local baseline=$3
	local stream_name=$4
	for _ in $(seq 15); do
		if ! kill -0 "$pid" 2>/dev/null; then
			cat "$log_file" >&2
			printf '%s stream exited before its post-start RPC heartbeat\n' "$stream_name" >&2
			return 1
		fi
		if [ "$(heartbeat_count "$log_file")" -gt "$baseline" ]; then
			printf '%s completed a post-start application RPC heartbeat\n' "$stream_name"
			return 0
		fi
		sleep 1
	done
	cat "$log_file" >&2
	printf '%s did not complete a post-start application RPC heartbeat\n' "$stream_name" >&2
	return 1
}

public_smoke_started=1
compose_public up -d teagramd-public-1 teagramd-public-2 public-haproxy
for service in teagramd-public-1 teagramd-public-2; do
	for _ in $(seq 60); do
		if compose_public logs "$service" 2>&1 | grep -F 'Public WebSocket MTProto listening' >/dev/null; then
			break
		fi
		sleep 1
	done
	if ! compose_public logs "$service" 2>&1 | grep -F 'Public WebSocket MTProto listening' >/dev/null; then
		compose_public ps -a
		compose_public logs "$service"
		echo "$service did not start the dedicated public WebSocket listener" >&2
		exit 1
	fi
done

public_haproxy=$(compose_public ps -q public-haproxy)
for _ in $(seq 45); do
	public_haproxy_health=$(docker inspect "$public_haproxy" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}')
	if [ "$public_haproxy_health" = healthy ]; then
		break
	fi
	sleep 1
done
if [ "$public_haproxy_health" != healthy ]; then
	compose_public ps -a
	compose_public logs public-haproxy teagramd-public-1 teagramd-public-2
	echo "public HAProxy did not become healthy" >&2
	exit 1
fi

public_config="$work_dir/public-client-config.json"
# Reuse the running replica; `run` would request its already-assigned static IP.
compose_public exec -T teagramd-public-1 /usr/local/bin/teagramd client-config > "$public_config"
start_hold websocket "127.0.0.1:${test_ports[0]}" "$work_dir/public-ws-a.log" "$public_config" --origin https://web.example.test --cf-connecting-ip 203.0.113.10
public_ws_pid_a=$started_hold_pid
wait_hold "$public_ws_pid_a" "$work_dir/public-ws-a.log"
start_hold websocket "127.0.0.1:${test_ports[0]}" "$work_dir/public-ws-b.log" "$public_config" --origin https://web.example.test --cf-connecting-ip 203.0.114.10
public_ws_pid_b=$started_hold_pid
wait_hold "$public_ws_pid_b" "$work_dir/public-ws-b.log"
public_ws_heartbeats_a=$(heartbeat_count "$work_dir/public-ws-a.log")
public_ws_heartbeats_b=$(heartbeat_count "$work_dir/public-ws-b.log")
wait_for_heartbeat_after "$public_ws_pid_a" "$work_dir/public-ws-a.log" "$public_ws_heartbeats_a" "public WebSocket client A"
wait_for_heartbeat_after "$public_ws_pid_b" "$work_dir/public-ws-b.log" "$public_ws_heartbeats_b" "public WebSocket client B"
stop_probe "$public_ws_pid_a"
public_ws_pid_a=
stop_probe "$public_ws_pid_b"
public_ws_pid_b=

ingress_network=$(jq -er '.networks["public-ingress"].name' "$work_dir/public-compose.json")
backend_network=$(jq -er '.networks["public-backend"].name' "$work_dir/public-compose.json")
default_network=$(jq -er '.networks.default.name' "$work_dir/public-compose.json")
ingress_gateway=$(docker network inspect "$ingress_network" | jq -er '.[0].IPAM.Config[] | select(.Subnet == "198.18.131.0/24") | .Gateway')
if [ "$ingress_gateway" != 198.18.131.1 ]; then
	echo "public ingress gateway is $ingress_gateway, expected the pinned HAProxy peer gateway" >&2
	exit 1
fi
if default_network_output=$(docker run --rm --network "$default_network" --read-only --cap-drop ALL \
		mirror.gcr.io/library/busybox:1.37 sh -ec '
			command -v nc >/dev/null
			if nc -z -w 2 198.18.131.10 2446; then
				echo tcp-connect=connected
				exit 0
			else
				status=$?
				echo "tcp-connect=failed status=$status"
				exit "$status"
			fi
		' 2>&1); then
	echo "a container on the Compose default network reached the public HAProxy frontend" >&2
	exit 1
else
	if ! printf '%s\n' "$default_network_output" | grep -Fxq 'tcp-connect=failed status=1'; then
		printf '%s\n' "$default_network_output" >&2
		echo "could not confirm the default-network TCP connection was refused" >&2
		exit 1
	fi
fi

public_replica_id=$(compose_public ps -q teagramd-public-1)
public_replica_ip=$(docker inspect "$public_replica_id" | jq -er --arg network "$default_network" '.[0].NetworkSettings.Networks[$network].IPAddress')
docker run --rm --network "$default_network" --read-only --cap-drop ALL \
	--security-opt no-new-privileges --user 65532:65532 \
	-v "$probe_binary:/public-websocket-probe:ro" --entrypoint /public-websocket-probe teagramd:local \
	forged --address "$public_replica_ip:2446"

"$probe_binary" origin --address "127.0.0.1:${test_ports[0]}" --origin https://web.example.test --status 400
"$probe_binary" origin --address "127.0.0.1:${test_ports[0]}" --origin https://web.example.test \
	--cf-connecting-ip '198.51.100.10, 198.51.100.11' --status 400
"$probe_binary" origin --address "127.0.0.1:${test_ports[0]}" --origin https://web.example.test \
	--cf-connecting-ip invalid-address --status 400
"$probe_binary" origin --address "127.0.0.1:${test_ports[0]}" --origin https://evil.example \
	--cf-connecting-ip 203.0.115.10 --status 403
"$probe_binary" public-signup --address "127.0.0.1:${test_ports[0]}" --config "$public_config" \
	--cf-connecting-ip 203.0.116.10

public_haproxy_logs=$(compose_public logs --no-color public-haproxy)
observed_ingress_peer=$(printf '%s\n' "$public_haproxy_logs" | sed -n \
	's/.*public_peer=\([^ ]*\) client=203\.0\.113\.10 .*/\1/p' | head -1)
if [ -z "$observed_ingress_peer" ]; then
	printf '%s\n' "$public_haproxy_logs" >&2
	echo "HAProxy did not log the observed socket peer for the first public request" >&2
	exit 1
fi
if [ "$observed_ingress_peer" != "$ingress_gateway" ]; then
	echo "HAProxy observed peer $observed_ingress_peer, expected inspected gateway $ingress_gateway" >&2
	exit 1
fi
for client_ip in 203.0.113.10 203.0.114.10; do
	if ! printf '%s\n' "$public_haproxy_logs" | grep -F "public_peer=$observed_ingress_peer client=$client_ip" >/dev/null; then
		printf '%s\n' "$public_haproxy_logs" >&2
		echo "HAProxy did not report the inspected gateway peer and client address $client_ip" >&2
		exit 1
	fi
done

haproxy_image=$(jq -er '.services["public-haproxy"].image' "$work_dir/public-compose.json")
python3 - deploy/teagramd/haproxy-public-websocket.cfg "$work_dir/haproxy-attribution.cfg" <<'PY'
import sys

source, target = sys.argv[1:]
content = open(source, encoding="utf-8").read()
if content.count("bind 198.18.131.10:2446") != 1:
    raise SystemExit("public HAProxy config must bind the production frontend once")
content = content.replace("bind 198.18.131.10:2446", "bind 198.18.131.11:2446")
lines = content.splitlines()
servers = [index for index, line in enumerate(lines) if line.strip().startswith("server-template public 2 ")]
if len(servers) != 1:
    raise SystemExit("public HAProxy config must declare one public backend template")
lines[servers[0]] = "  server attribution 198.18.132.23:2446 send-proxy-v2"
open(target, "w", encoding="utf-8").write("\n".join(lines) + "\n")
PY
# This generated config has no secrets; the validation container drops all caps.
chmod 644 "$work_dir/haproxy-attribution.cfg"
docker run --rm --network none --read-only --cap-drop ALL \
	-v "$work_dir/haproxy-attribution.cfg:/usr/local/etc/haproxy/haproxy.cfg:ro" \
	"$haproxy_image" haproxy -c -f /usr/local/etc/haproxy/haproxy.cfg
docker run --detach --name "$test_echo_name" --network "$backend_network" --ip 198.18.132.23 \
	--read-only --cap-drop ALL --security-opt no-new-privileges --user 65532:65532 \
	-v "$probe_binary:/public-websocket-probe:ro" --entrypoint /public-websocket-probe teagramd:local \
	proxy-v2-echo --listen :2446 --requests 2 >/dev/null
for _ in $(seq 30); do
	if docker logs "$test_echo_name" 2>&1 | grep -F 'proxy-v2-echo-ready' >/dev/null; then
		break
	fi
	sleep 1
done
if ! docker logs "$test_echo_name" 2>&1 | grep -F 'proxy-v2-echo-ready' >/dev/null; then
	docker logs "$test_echo_name" >&2
	echo "PROXY-v2 attribution backend did not start" >&2
	exit 1
fi
docker run --detach --name "$test_haproxy_name" --network "$ingress_network" --ip 198.18.131.11 \
	--publish "127.0.0.1:${test_ports[1]}:2446" --read-only --cap-drop ALL \
	--security-opt no-new-privileges --memory 96m --cpus 0.25 --pids-limit 64 \
	-v "$work_dir/haproxy-attribution.cfg:/usr/local/etc/haproxy/haproxy.cfg:ro" \
	"$haproxy_image" -f /usr/local/etc/haproxy/haproxy.cfg >/dev/null
docker network connect --ip 198.18.132.11 "$backend_network" "$test_haproxy_name"
for _ in $(seq 30); do
	if docker logs "$test_haproxy_name" 2>&1 | grep -F 'Loading success.' >/dev/null; then
		break
	fi
	sleep 1
done
if ! docker logs "$test_haproxy_name" 2>&1 | grep -F 'Loading success.' >/dev/null; then
	echo "attribution HAProxy did not load its configuration" >&2
	exit 1
fi
"$probe_binary" keepalive-attribution --address "127.0.0.1:${test_ports[1]}" \
	--first-ip 203.0.113.101 --second-ip 203.0.114.102
docker wait "$test_echo_name" >/dev/null
echo_attribution=$(docker logs "$test_echo_name" 2>&1 | sed -n 's/^proxy-v2-attribution=//p')
if [ "$echo_attribution" != "203.0.113.101,203.0.114.102" ]; then
	docker logs "$test_echo_name" >&2
	echo "keepalive requests reached backend with PROXY-v2 addresses $echo_attribution" >&2
	exit 1
fi
echo "public WebSocket ingress validation, invite policy, peer attribution, rate limits, and keepalive attribution passed"
