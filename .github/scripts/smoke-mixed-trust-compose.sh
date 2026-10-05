#!/usr/bin/env bash
set -euo pipefail

if [ "${GITHUB_ACTIONS:-}" != true ] || [ "${RUNNER_ENVIRONMENT:-}" != github-hosted ]; then
	echo "mixed-trust validation requires its disposable GitHub-hosted daemon" >&2
	exit 2
fi

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$repo_root"

work_dir=$(mktemp -d)
umask 077
trap 'rm -rf "$work_dir"' EXIT
env_file="$work_dir/.env"
project="main1286-proof-${GITHUB_RUN_ID:-local}-$$"
legacy_tcp_pid=
legacy_ws_pid=
proxy_tcp_pid=
proxy_ws_pid=

mapfile -t existing_network_ids < <(docker network ls --quiet)
if [ "${#existing_network_ids[@]}" -eq 0 ]; then
	echo "the disposable daemon has no Docker networks to inspect" >&2
	exit 1
fi
docker network inspect "${existing_network_ids[@]}" > "$work_dir/networks.json"
ip -j route show > "$work_dir/routes.json"
mapfile -t mixed_network_values < <(python3 - "$work_dir/networks.json" "$work_dir/routes.json" <<'PY'
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
            route_network = ipaddress.ip_network(destination, strict=False)
            if route_network.prefixlen > 0:
                occupied.append(route_network)
        except ValueError:
            pass

pool = ipaddress.ip_network("198.18.0.0/15")
for offset in range(pool.num_addresses // 256):
    candidate = ipaddress.ip_network((int(pool.network_address) + offset * 256, 24))
    if not any(candidate.overlaps(existing) for existing in occupied):
        print(candidate)
        print(candidate.network_address + 10)
        print(candidate.network_address + 21)
        print(candidate.network_address + 22)
        break
else:
    raise SystemExit("no non-overlapping /24 test subnet is available in 198.18.0.0/15")
PY
)
if [ "${#mixed_network_values[@]}" -ne 4 ]; then
	echo "could not select a non-overlapping mixed-trust subnet" >&2
	exit 1
fi

docker ps --format '{{.Ports}}' > "$work_dir/docker-ports.txt"
ss -H -ltn > "$work_dir/listening-ports.txt"
mapfile -t mixed_ports < <(python3 - "$work_dir/docker-ports.txt" "$work_dir/listening-ports.txt" <<'PY'
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
    selected = [base, base + 1, base + 10, base + 11]
    if not used.intersection(selected):
        print("\n".join(map(str, selected)))
        break
else:
    raise SystemExit("no non-overlapping host ports are available in 39000-50010")
PY
)
if [ "${#mixed_ports[@]}" -ne 4 ]; then
	echo "could not select non-overlapping mixed-trust host ports" >&2
	exit 1
fi

cat > "$env_file" <<EOF
POSTGRES_PASSWORD=localdev
TG_PUBLIC_LINK_PREFIX=https://links.example.test/
TG_LOG_LOGIN_CODES=false
TG_REGISTRATION=closed
TG_AUTHKEY_ENC_KEY=
TG_AUTHKEY_ENC_KEY_FILE=/var/lib/telegramd/enc_key.hex
TG_REPLICA_ID=legacy
TG_REPLICA_COUNT=3
TG_ADVERTISE_ADDR=127.0.0.1:${mixed_ports[2]}
TG_WEBSOCKET_ALLOWED_ORIGINS=https://web.example.test
TG_MIXED_TRUST_SUBNET=${mixed_network_values[0]}
TG_MIXED_TRUST_PROXY_IP=${mixed_network_values[1]}
TG_MIXED_TRUST_REPLICA_1_IP=${mixed_network_values[2]}
TG_MIXED_TRUST_REPLICA_2_IP=${mixed_network_values[3]}
TG_MIXED_TRUST_LEGACY_TCP_PORT=${mixed_ports[0]}
TG_MIXED_TRUST_LEGACY_WS_PORT=${mixed_ports[1]}
TG_MIXED_TRUST_TEMP_TCP_PORT=${mixed_ports[2]}
TG_MIXED_TRUST_TEMP_WS_PORT=${mixed_ports[3]}
EOF
chmod 600 "$env_file"

compose_base() {
	docker compose --project-name "$project" --env-file "$env_file" \
		-f docker-compose.yml -f docker-compose.mixed-trust.validation.yml "$@"
}

compose_mixed() {
	docker compose --project-name "$project" --env-file "$env_file" \
		-f docker-compose.yml -f docker-compose.mixed-trust.validation.yml \
		-f docker-compose.mixed-trust.yml "$@"
}

compose_probe() {
	docker compose --project-name "$project" --env-file "$env_file" \
		-f docker-compose.yml -f docker-compose.mixed-trust.validation.yml \
		-f docker-compose.mixed-trust.yml -f docker-compose.mixed-trust.probe.yml "$@"
}

compose_for_service() {
	service=$1
	shift
	case "$service" in
		telegramd-proxy-*) compose_mixed "$@" ;;
		*) compose_base "$@" ;;
	esac
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
	stop_probe "$legacy_tcp_pid"
	stop_probe "$legacy_ws_pid"
	stop_probe "$proxy_tcp_pid"
	stop_probe "$proxy_ws_pid"
	compose_mixed down --volumes --remove-orphans >/dev/null 2>&1 || true
	rm -rf "$work_dir"
	exit "$status"
}
trap cleanup EXIT

if [ "$(stat -c '%a' "$env_file")" != 600 ]; then
	echo "disposable Compose environment must be mode 0600" >&2
	exit 1
fi

if ! docker image inspect telegramd:local >/dev/null 2>&1; then
	docker build -t telegramd:local .
fi

compose_default_json="$work_dir/default-compose.json"
docker compose --project-name "$project" --env-file "$env_file" \
	-f docker-compose.yml config --format json > "$compose_default_json"
compose_base config --format json > "$work_dir/legacy-compose.json"

python3 - "$compose_default_json" "$work_dir/legacy-compose.json" <<'PY'
import json
import sys

default, legacy = [json.load(open(path, encoding="utf-8")) for path in sys.argv[1:]]
assert "tcp-proxy" not in default["services"]
assert "telegramd-proxy-1" not in default["services"]
assert "telegramd-proxy-2" not in default["services"]
assert any(
    port.get("host_ip") == "127.0.0.1"
    and str(port.get("published")) == "2443"
    and str(port.get("target")) == "2443"
    for port in default["services"]["telegramd"]["ports"]
)
assert legacy["services"]["telegramd"]["environment"]["TG_CLIENT_ADDR_TRUST"] == "socket"
assert legacy["services"]["telegramd"]["environment"]["TG_REPLICA_COUNT"] == "3"
assert legacy["services"]["telegramd"]["environment"]["TG_WEBSOCKET_ALLOWED_ORIGINS"] == "https://web.example.test"
print("default Compose stays direct; validation legacy is socket-trust")
PY

bootstrap_output=$(compose_base run --rm --no-deps -T telegramd bootstrap-identity)
fingerprint=$(printf '%s\n' "$bootstrap_output" | sed -n 's/^RSA identity bootstrapped: fingerprint=\(-\?[0-9][0-9]*\) path=.*/\1/p')
unset bootstrap_output
if [ -z "$fingerprint" ]; then
	echo "fresh test identity bootstrap did not report a fingerprint" >&2
	exit 1
fi
printf 'TG_RSA_KEY_FINGERPRINT=%s\n' "$fingerprint" >> "$env_file"

compose_base create telegramd >/dev/null
legacy_container=$(compose_base ps -aq telegramd | head -1)
key_volume=$(docker inspect "$legacy_container" --format '{{range .Mounts}}{{if and (eq .Type "volume") (eq .Destination "/var/lib/telegramd")}}{{.Name}}{{end}}{{end}}')
if [ -z "$key_volume" ]; then
	echo "legacy service has no test-owned key volume" >&2
	exit 1
fi
docker run --rm --user 65532:65532 -v "$key_volume:/key" busybox:1.37 sh -ec \
	'umask 077; od -An -N32 -tx1 /dev/urandom | tr -d " \n" > /key/enc_key.hex; chmod 600 /key/enc_key.hex'

compose_mixed config --format json > "$work_dir/mixed-compose.json"
python3 - "$work_dir/mixed-compose.json" "${mixed_network_values[0]}" "${mixed_network_values[1]}" "${mixed_ports[2]}" "${mixed_ports[3]}" <<'PY'
import json
import sys

config = json.load(open(sys.argv[1], encoding="utf-8"))
subnet, proxy_ip, temp_tcp_port, temp_ws_port = sys.argv[2:]
services = config["services"]
legacy = services["telegramd"]
proxy1 = services["telegramd-proxy-1"]
proxy2 = services["telegramd-proxy-2"]
proxy = services["tcp-proxy"]
assert legacy["environment"]["TG_CLIENT_ADDR_TRUST"] == "socket"
assert legacy["environment"]["TG_REPLICA_COUNT"] == "3"
for service in (proxy1, proxy2):
    assert service["environment"]["TG_CLIENT_ADDR_TRUST"] == "proxy-v2"
    assert service["environment"]["TG_CLIENT_ADDR_PROXY_CIDRS"] == f"{proxy_ip}/32"
    assert service.get("ports", []) == []
    assert service["environment"]["TG_WEBSOCKET_ALLOWED_ORIGINS"] == "https://web.example.test"
    assert service["environment"]["TG_RSA_KEY_FINGERPRINT"]
    assert service["environment"]["TG_ADVERTISE_ADDR"] == f"127.0.0.1:{temp_tcp_port}"
assert proxy["healthcheck"]["test"][-1] == "http://127.0.0.1:8404/healthz"
assert all(port.get("host_ip") == "127.0.0.1" for port in proxy["ports"])
assert {str(port.get("published")) for port in proxy["ports"]} == {temp_tcp_port, temp_ws_port}
assert config["networks"]["mixed-trust"]["ipam"]["config"][0]["subnet"] == subnet
for replica in (proxy1, proxy2):
    volume_targets = {mount["target"]: mount["source"] for mount in replica["volumes"]}
    assert volume_targets["/var/lib/telegramd"] == "tgkey"
    assert volume_targets["/var/lib/telegramd-blobs"] == "tgblobs"
print("mixed Compose keeps exact trust modes, shared identity volumes, private app ports, and loopback-only temporary ports")
PY

python3 - deploy/telegramd/haproxy-mixed-trust.cfg <<'PY'
import sys

path = sys.argv[1]
content = open(path, encoding="utf-8").read()
sections = {}
current = None
for line in content.splitlines():
    value = line.strip()
    if value in {"global", "defaults"}:
        current = value
        sections[current] = []
    elif value.startswith(("resolvers ", "frontend ", "backend ")):
        current = value.split()[1]
        sections[current] = []
    elif value:
        sections.setdefault(current, []).append(value)

assert "mode tcp" in sections["defaults"]
for frontend in ("mtproto", "websocket"):
    assert "mode http" not in sections[frontend]
assert "bind 127.0.0.1:8404" in sections["health"]
assert ":2445" not in content
targets = []
for line in content.splitlines():
    fields = line.split()
    if fields and fields[0] == "server-template":
        assert "check" in fields and "send-proxy-v2" in fields
        targets.append(fields[3])
assert set(targets) == {"telegramd-proxy:2443", "telegramd-proxy:2444"}
print("HAProxy uses TCP passthrough, exact proxy-v2 replacement targets, and private readiness")
PY
compose_mixed run --rm --no-deps tcp-proxy haproxy -c -f /usr/local/etc/haproxy/haproxy.cfg

compose_base up -d
wait_listening() {
	service=$1
	for _ in $(seq 60); do
		if compose_for_service "$service" logs "$service" 2>&1 | grep -q 'msg=listening'; then
			cid=$(compose_for_service "$service" ps -q "$service")
			if [ -n "$cid" ] && [ "$(docker inspect "$cid" --format '{{.State.Status}}')" = running ]; then
				return 0
			fi
		fi
		sleep 1
	done
	compose_for_service "$service" ps -a
	compose_for_service "$service" logs "$service"
	echo "$service did not reach application readiness" >&2
	return 1
}

wait_listening telegramd
legacy_container_before=$(compose_base ps -q telegramd)
legacy_config="$work_dir/legacy-client-config.json"
compose_base run --rm --no-deps -T telegramd client-config > "$legacy_config"

probe_binary="$work_dir/mixed-trust-probe"
go build -o "$probe_binary" ./cmd/mixed-trust-probe

start_hold() {
	local transport_name=$1
	local address=$2
	local log_file=$3
	shift 3
	"$probe_binary" hold --transport "$transport_name" --address "$address" \
		--config "$legacy_config" "$@" > "$log_file" 2>&1 &
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
	echo "real application stream did not become ready" >&2
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

start_hold tcp "127.0.0.1:${mixed_ports[0]}" "$work_dir/legacy-tcp.log"
legacy_tcp_pid=$started_hold_pid
wait_hold "$legacy_tcp_pid" "$work_dir/legacy-tcp.log"
start_hold websocket "127.0.0.1:${mixed_ports[1]}" "$work_dir/legacy-ws.log" --origin https://web.example.test
legacy_ws_pid=$started_hold_pid
wait_hold "$legacy_ws_pid" "$work_dir/legacy-ws.log"

proxy_config_1="$work_dir/proxy-1-client-config.json"
proxy_config_2="$work_dir/proxy-2-client-config.json"
compose_mixed run --rm --no-deps -T telegramd-proxy-1 client-config > "$proxy_config_1"
compose_mixed run --rm --no-deps -T telegramd-proxy-2 client-config > "$proxy_config_2"
python3 - "$legacy_config" "$proxy_config_1" "$proxy_config_2" "${mixed_ports[2]}" <<'PY'
import json
import sys

legacy, first, second = [json.load(open(path, encoding="utf-8")) for path in sys.argv[1:4]]
for replica in (first, second):
    assert replica["mtproto"]["endpoint"] == f"127.0.0.1:{sys.argv[4]}"
    assert replica["mtproto"]["dc_id"] == legacy["mtproto"]["dc_id"]
    assert replica["mtproto"]["rsa_spki"] == legacy["mtproto"]["rsa_spki"]
print("both proxy replicas advertise the canonical endpoint and the legacy RSA identity")
PY

expect_identity_refusal() {
	name=$1
	match=$2
	shift 2
	output="$work_dir/$name.log"
	if docker run --rm --network none --read-only -v "$key_volume:/var/lib/telegramd:ro" \
		-e TG_PUBLIC_LINK_PREFIX=https://links.example.test/ \
		-e TG_POSTGRES_DSN=postgres://postgres:localdev@localhost:5432/telegram?sslmode=disable \
		-e TG_RSA_KEY_FINGERPRINT="$fingerprint" -e TG_REPLICA_ID=readiness-probe \
		-e TG_AUTHKEY_ENC_KEY= -e TG_AUTHKEY_ENC_KEY_FILE=/var/lib/telegramd/enc_key.hex \
		-e TG_CLIENT_ADDR_TRUST=proxy-v2 -e TG_CLIENT_ADDR_PROXY_CIDRS="${mixed_network_values[1]}/32" \
		"$@" telegramd:local > "$output" 2>&1; then
		echo "$name unexpectedly reached server startup" >&2
		cat "$output" >&2
		return 1
	fi
	if ! grep -Fq "$match" "$output"; then
		echo "$name failed for a reason other than the missing identity material" >&2
		cat "$output" >&2
		return 1
	fi
	printf '%s refused as required\n' "$name"
}

expect_identity_refusal missing-rsa 'server_key_missing.pem' -e TG_RSA_KEY_PATH=/var/lib/telegramd/server_key_missing.pem
expect_identity_refusal missing-encryption-key 'TG_AUTHKEY_ENC_KEY_FILE' -e TG_RSA_KEY_PATH=/var/lib/telegramd/server_key.pem -e TG_AUTHKEY_ENC_KEY_FILE=/var/lib/telegramd/missing_enc_key.hex

compose_mixed up -d --no-deps tcp-proxy
compose_mixed up -d --no-deps telegramd-proxy-1 telegramd-proxy-2
for service in telegramd-proxy-1 telegramd-proxy-2; do
	wait_listening "$service"
	compose_mixed logs "$service" | grep -q 'auth-key master key loaded from file'
done

proxy_container=$(compose_mixed ps -q tcp-proxy)
for _ in $(seq 45); do
	proxy_health=$(docker inspect "$proxy_container" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}')
	if [ "$proxy_health" = healthy ]; then
		break
	fi
	sleep 1
done
if [ "$proxy_health" != healthy ]; then
	compose_mixed ps -a
	compose_mixed logs tcp-proxy telegramd-proxy-1 telegramd-proxy-2
	echo "temporary HAProxy did not become healthy" >&2
	exit 1
fi

legacy_container_after=$(compose_mixed ps -q telegramd)
if [ "$legacy_container_before" != "$legacy_container_after" ]; then
	echo "starting the replacement topology recreated the legacy listener" >&2
	exit 1
fi

start_hold tcp "127.0.0.1:${mixed_ports[2]}" "$work_dir/proxy-tcp.log"
proxy_tcp_pid=$started_hold_pid
wait_hold "$proxy_tcp_pid" "$work_dir/proxy-tcp.log"
start_hold websocket "127.0.0.1:${mixed_ports[3]}" "$work_dir/proxy-ws.log" --origin https://web.example.test
proxy_ws_pid=$started_hold_pid
wait_hold "$proxy_ws_pid" "$work_dir/proxy-ws.log"

legacy_tcp_heartbeats=$(heartbeat_count "$work_dir/legacy-tcp.log")
legacy_ws_heartbeats=$(heartbeat_count "$work_dir/legacy-ws.log")
proxy_tcp_heartbeats=$(heartbeat_count "$work_dir/proxy-tcp.log")
proxy_ws_heartbeats=$(heartbeat_count "$work_dir/proxy-ws.log")

"$probe_binary" origin --address "127.0.0.1:${mixed_ports[3]}" --origin https://evil.example --status 403
compose_probe --profile validation build mixed-trust-probe
compose_probe --profile validation run --rm --no-deps mixed-trust-probe forged --address telegramd-proxy-1:2443

wait_for_heartbeat_after "$legacy_tcp_pid" "$work_dir/legacy-tcp.log" "$legacy_tcp_heartbeats" "legacy TCP"
wait_for_heartbeat_after "$legacy_ws_pid" "$work_dir/legacy-ws.log" "$legacy_ws_heartbeats" "legacy WebSocket"
wait_for_heartbeat_after "$proxy_tcp_pid" "$work_dir/proxy-tcp.log" "$proxy_tcp_heartbeats" "proxy TCP"
wait_for_heartbeat_after "$proxy_ws_pid" "$work_dir/proxy-ws.log" "$proxy_ws_heartbeats" "proxy WebSocket"

for pid in "$legacy_tcp_pid" "$legacy_ws_pid" "$proxy_tcp_pid" "$proxy_ws_pid"; do
	if ! kill -0 "$pid" 2>/dev/null; then
		echo "an established real application stream exited during topology startup" >&2
			exit 1
	fi
done

printf '%s\n' "legacy and proxy TCP/WebSocket streams completed post-start application RPC heartbeats; forged PROXY-v2 and unapproved Origin were refused"
