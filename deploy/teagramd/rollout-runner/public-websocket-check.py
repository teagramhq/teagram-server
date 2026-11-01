#!/usr/bin/env python3
"""Fail closed unless the rendered public WebSocket trust boundary is exact."""

import argparse
import json
import os
import re
import sys


def require(condition: bool, reason: str) -> None:
    if not condition:
        raise ValueError(reason)


def environment(service: dict, name: str) -> dict:
    values = service.get("environment")
    require(isinstance(values, dict), f"{name} environment is missing")
    return values


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--haproxy-config", required=True)
    parser.add_argument("--host-port", type=int, default=2446)
    args = parser.parse_args()
    require(1 <= args.host_port <= 65535, "public HAProxy host port is outside the TCP port range")
    compose = json.load(sys.stdin)
    services = compose.get("services")
    require(isinstance(services, dict), "Compose services are missing")
    base = services.get("teagramd")
    proxy = services.get("public-haproxy")
    require(isinstance(base, dict), "base teagramd service is missing")
    require(isinstance(proxy, dict), "public HAProxy service is missing; enable the public profile")

    base_env = environment(base, "teagramd")
    require(base_env.get("TG_CLIENT_ADDR_TRUST") == "socket", "base teagramd must retain socket trust for tailnet and raw TCP")
    require(base_env.get("TG_PUBLIC_WEBSOCKET_LISTEN_ADDR", "") == "", "base teagramd must not bind the public WebSocket listener")
    require(base_env.get("TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR", "") == "", "base teagramd must not advertise the public WebSocket listener")
    require(not any(str(port.get("target")) == "2446" for port in base.get("ports", [])), "base teagramd must not publish port 2446")
    require(base_env.get("TG_REPLICA_COUNT") == "1", "base teagramd must retain TG_REPLICA_COUNT=1 for its process-local listener budgets")
    require(base_env.get("TG_TURNSTILE_TEST_MODE", "false") == "false", "production replicas must not use Turnstile test mode")
    require(base_env.get("TG_TURNSTILE_HOSTNAMES", "web.teagram.io") in ("", "web.teagram.io"), "production replicas must accept only the web hostname")
    require(base_env.get("TG_TURNSTILE_QA_BYPASS_TOKEN_FILE", "") == "", "production replicas must not use the demo QA bypass token")

    proxy_env = proxy.get("environment") or {}
    require(isinstance(proxy_env, dict), "public HAProxy environment must be an object when present")
    ingress = proxy.get("networks")
    require(isinstance(ingress, dict), "public HAProxy networks are missing")
    require(set(ingress) == {"public-ingress", "public-backend"}, "public HAProxy must join only the ingress and backend networks")
    require(ingress["public-ingress"].get("ipv4_address") == "198.18.131.10", "public HAProxy ingress address changed")
    require(ingress["public-backend"].get("ipv4_address") == "198.18.132.10", "public HAProxy backend address changed")
    require(not proxy_env, "public HAProxy must not receive application credentials or environment settings")
    require("@sha256:" in proxy.get("image", ""), "public HAProxy image must be pinned by digest")
    require(proxy.get("read_only") is True, "public HAProxy filesystem must be read-only")
    require(proxy.get("cap_drop") == ["ALL"], "public HAProxy must drop all Linux capabilities")
    require("no-new-privileges:true" in proxy.get("security_opt", []), "public HAProxy must disable privilege escalation")
    require(proxy.get("restart") == "unless-stopped", "public HAProxy must restart after an unplanned exit")
    require(proxy.get("stop_grace_period") == "2m0s", "public HAProxy must outlive the teagramd drain")
    require(str(proxy.get("mem_limit")) == "100663296", "public HAProxy memory must be bounded to 96 MiB")
    require(str(proxy.get("memswap_limit")) == "100663296", "public HAProxy swap must be bounded to 96 MiB")
    require(str(proxy.get("cpus")) == "0.25", "public HAProxy CPU must be bounded to 0.25 cores")
    require(proxy.get("pids_limit") == 64, "public HAProxy process count must be bounded")
    published = proxy.get("ports", [])
    require(len(published) == 1, "public HAProxy must publish exactly one port")
    port = published[0]
    require(
        port.get("host_ip") == "127.0.0.1"
        and str(port.get("published")) == str(args.host_port)
        and str(port.get("target")) == "2446"
        and port.get("protocol", "tcp") == "tcp",
        "public HAProxy must publish one loopback host port to container port 2446",
    )

    networks = compose.get("networks", {})
    ingress_network = networks.get("public-ingress", {})
    backend_network = networks.get("public-backend", {})
    require(ingress_network.get("internal") is not True, "public ingress network must allow the host loopback port publish")
    require(backend_network.get("internal") is True, "public backend network must be internal")
    ingress_ipam = ingress_network.get("ipam", {}).get("config", [])
    backend_ipam = backend_network.get("ipam", {}).get("config", [])
    require(any(item.get("subnet") == "198.18.131.0/24" and item.get("gateway") == "198.18.131.1" for item in ingress_ipam), "public ingress gateway or subnet changed")
    require(any(item.get("subnet") == "198.18.132.0/24" and item.get("gateway") == "198.18.132.1" for item in backend_ipam), "public backend gateway or subnet changed")

    ingress_members = {name for name, service in services.items() if "public-ingress" in service.get("networks", {})}
    require(ingress_members == {"public-haproxy"}, "public ingress must have no other Compose containers")
    backend_members = {name for name, service in services.items() if "public-backend" in service.get("networks", {})}
    expected_backends = {"public-haproxy", "teagramd-public-1", "teagramd-public-2"}
    require(backend_members == expected_backends, "public backend network membership changed")

    for name, address in (("teagramd-public-1", "198.18.132.21"), ("teagramd-public-2", "198.18.132.22")):
        service = services.get(name)
        require(isinstance(service, dict), f"{name} service is missing")
        env = environment(service, name)
        require(env.get("TG_CLIENT_ADDR_TRUST") == "proxy-v2", f"{name} must use proxy-v2 trust")
        require(env.get("TG_CLIENT_ADDR_PROXY_CIDRS") == "198.18.132.10/32", f"{name} must trust only HAProxy's fixed address")
        require(env.get("TG_REPLICA_COUNT") == "3", f"{name} has a different replica count")
        require(env.get("TG_PUBLIC_WEBSOCKET_LISTEN_ADDR") == ":2446", f"{name} must bind the dedicated public listener on 2446")
        advertise = env.get("TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR", "")
        require(bool(re.fullmatch(r"[A-Za-z0-9.-]+:443", advertise)), f"{name} must advertise the public HTTPS WebSocket origin")
        require(bool(env.get("TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS")), f"{name} must keep the public Origin allowlist")
        require(env.get("TG_PUBLIC_REGISTRATION") == "invite", f"{name} must require invites for public registration")
        for key in ("TG_PUBLIC_REGISTRATION", "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS"):
            require(env.get(key) == base_env.get(key), f"{name} must inherit {key} from teagramd")
        require(env.get("TG_WEBSOCKET_LISTEN_ADDR", "") == "", f"{name} must not run the ordinary WebSocket listener")
        require(env.get("TG_WEBSOCKET_ALLOWED_ORIGINS", "") == "", f"{name} must not inherit the ordinary Origin policy")
        require(env.get("TG_ADVERTISE_ADDR") == base_env.get("TG_ADVERTISE_ADDR"), f"{name} must retain the base tailnet advertise address")
        require(env.get("TG_BLOB_DIR") == base_env.get("TG_BLOB_DIR"), f"{name} must retain the base blob directory")
        for key in (
            "TG_BLOB_S3_ENDPOINT", "TG_BLOB_S3_BUCKET", "TG_BLOB_S3_PREFIX",
            "TG_BLOB_S3_REGION", "TG_BLOB_S3_ACCESS_KEY_ID",
            "TG_BLOB_S3_SECRET_ACCESS_KEY", "TG_BLOB_S3_SECRET_ACCESS_KEY_FILE",
            "TG_BLOB_S3_CA_PATH", "TG_BLOB_S3_ALLOW_INSECURE_HTTP",
            "TG_MEDIA_KEY_DIR", "TG_MEDIA_KEY_CURRENT",
        ):
            require(env.get(key, "") == base_env.get(key, ""), f"{name} must retain the base {key} setting")
        turnstile_defaults = {
            "TG_TURNSTILE_SITE_KEY": "",
            "TG_TURNSTILE_SECRET_KEY_FILE": "",
            "TG_TURNSTILE_SIGNIN_ENABLED": "",
            "TG_TURNSTILE_TEST_MODE": "false",
            "TG_TURNSTILE_HOSTNAMES": "web.teagram.io",
            "TG_TURNSTILE_QA_BYPASS_TOKEN_FILE": "",
        }
        for key, default in turnstile_defaults.items():
            require(env.get(key, default) == base_env.get(key, default), f"{name} must inherit {key} from teagramd")
        require(service.get("ports", []) == [], f"{name} must not publish any host ports")
        require(service.get("secrets", []) == base.get("secrets", []), f"{name} must keep the base service secret mounts")
        require(service.get("volumes", []) == base.get("volumes", []), f"{name} must keep the base service data mounts")
        networks = service.get("networks", {})
        require(set(networks) == {"default", "rustfs_private", "public-backend"}, f"{name} has an unexpected network attachment")
        require(networks["public-backend"].get("ipv4_address") == address, f"{name} backend address changed")
        require("public-ingress" not in networks, f"{name} must not join public ingress")

    volumes = proxy.get("volumes", [])
    require(isinstance(volumes, list) and all(isinstance(volume, dict) for volume in volumes), "public HAProxy volumes are invalid")
    config_mounts = [
        volume for volume in volumes
        if volume.get("target") == "/usr/local/etc/haproxy/haproxy.cfg"
    ]
    require(len(config_mounts) == 1, "public HAProxy must mount exactly one config file")
    config_mount = config_mounts[0]
    config_source = config_mount.get("source")
    require(
        config_mount.get("type") == "bind"
        and config_mount.get("read_only") is True
        and isinstance(config_source, str)
        and os.path.realpath(config_source) == os.path.realpath(args.haproxy_config),
        "public HAProxy must mount the reviewed config file read-only",
    )
    require(proxy.get("healthcheck", {}).get("test", [])[-1:] == ["http://127.0.0.1:8404/healthz"], "public HAProxy must expose a private readiness monitor")
    require(proxy.get("logging", {}).get("driver") == "json-file", "public HAProxy must retain bounded container logs")
    log_options = proxy.get("logging", {}).get("options", {})
    require(log_options.get("max-size") == "10m" and log_options.get("max-file") == "3", "public HAProxy log retention must be bounded")

    config = open(args.haproxy_config, encoding="utf-8").read()
    frontend_names = re.findall(r"(?m)^frontend\s+(\S+)", config)
    require(frontend_names == ["public_websocket", "health"], "HAProxy must expose only the public route and private health frontend")
    bind_lines = [line.strip() for line in config.splitlines() if line.strip().startswith("bind ")]
    require(bind_lines == ["bind 198.18.131.10:2446", "bind 127.0.0.1:8404"], "HAProxy bind addresses changed")
    frontend = config.split("frontend public_websocket\n", 1)[1].split("\nbackend ", 1)[0]
    rules = [line.strip() for line in frontend.splitlines() if line.strip().startswith(("tcp-request", "http-request"))]
    require(rules == [
        "tcp-request connection reject unless { src 198.18.131.1/32 }",
        "tcp-request connection set-var(sess.peer) src",
        "http-request deny deny_status 400 unless { req.hdr_cnt(CF-Connecting-IP) eq 1 }",
        "http-request deny deny_status 400 unless { req.hdr_ip(CF-Connecting-IP) -m found }",
        "http-request set-src req.hdr_ip(CF-Connecting-IP)",
    ], "public HAProxy peer and header rules must appear in the reviewed order")
    backend_match = re.search(
        r"(?ms)^backend public_websocket_replicas\s*\n(.*?)(?=^(?:frontend|backend)\s)",
        config,
    )
    require(backend_match is not None, "public replica backend is missing")
    backend_rules = [line.strip() for line in backend_match.group(1).splitlines() if line.strip()]
    require(backend_rules == [
        "http-reuse never",
        "server-template public 2 teagramd-public:2446 check send-proxy-v2 inter 2s fall 3 rise 2 resolvers docker init-addr none",
    ], "public backend must disable reuse and send PROXY-v2 only to the dedicated public replicas")
    require("X-Forwarded-For" not in config, "public HAProxy must not use X-Forwarded-For")
    require("public_peer=%[var(sess.peer)]" in config, "public HAProxy must log the socket peer for smoke verification")
    require("capture request header CF-Connecting-IP len 64" in frontend, "public HAProxy must capture the validated client address for logs")
    require("client=%[capture.req.hdr(0)]" in config, "public HAProxy must log the captured client address")
    print("public WebSocket trust boundary validated")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError, KeyError, IndexError, json.JSONDecodeError) as error:
        print(f"public WebSocket topology rejected: {error}", file=sys.stderr)
        raise SystemExit(1)
