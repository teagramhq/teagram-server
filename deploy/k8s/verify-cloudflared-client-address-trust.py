#!/usr/bin/env python3
"""Verify the ready cloudflared pod is trusted by both public listeners."""

from __future__ import annotations

import ipaddress
import json
import subprocess
import sys


NAMESPACE = "teagram"
TRUST_SETTINGS = (
    "TG_CLIENT_ADDR_CF_PROXY_CIDRS",
    "TG_BOT_API_PUBLIC_PROXY_CIDRS",
)


def ready_cloudflared_pod(document: dict) -> tuple[str, ipaddress.IPv4Address | ipaddress.IPv6Address]:
    items = document.get("items", [])
    ready = [
        pod
        for pod in items
        if pod.get("metadata", {}).get("deletionTimestamp") is None
        and any(
            condition.get("type") == "Ready" and condition.get("status") == "True"
            for condition in pod.get("status", {}).get("conditions", [])
        )
    ]
    if len(ready) != 1:
        raise RuntimeError(f"expected one Ready cloudflared pod, found {len(ready)}")

    pod = ready[0]
    name = pod.get("metadata", {}).get("name")
    raw_ip = pod.get("status", {}).get("podIP")
    if not name or not raw_ip:
        raise RuntimeError("Ready cloudflared pod has no name or podIP")
    try:
        address = ipaddress.ip_address(raw_ip)
    except ValueError as error:
        raise RuntimeError(f"Ready cloudflared pod has invalid podIP {raw_ip!r}") from error
    return name, address


def trust_prefixes(statefulset: dict, setting: str) -> tuple[str, list]:
    containers = [
        container
        for container in statefulset.get("spec", {})
        .get("template", {})
        .get("spec", {})
        .get("containers", [])
        if container.get("name") == "teagramd"
    ]
    if len(containers) != 1:
        raise RuntimeError(f"expected one teagramd container, found {len(containers)}")
    entries = [
        item
        for item in containers[0].get("env", [])
        if item.get("name") == setting
    ]
    if len(entries) != 1 or not isinstance(entries[0].get("value"), str):
        raise RuntimeError(f"{setting} must be set once to a CIDR list")

    raw_value = entries[0]["value"]
    prefixes = []
    for entry in raw_value.split(","):
        entry = entry.strip()
        if not entry:
            continue
        try:
            prefixes.append(ipaddress.ip_network(entry, strict=False))
        except ValueError as error:
            raise RuntimeError(f"{setting} contains invalid CIDR {entry!r}") from error
    if not prefixes:
        raise RuntimeError(f"{setting} has no CIDRs")
    return raw_value, prefixes


def verify_trust(pods: dict, statefulset: dict) -> tuple[str, str, dict[str, str]]:
    pod_name, pod_ip = ready_cloudflared_pod(pods)
    configured = {}
    for setting in TRUST_SETTINGS:
        raw_value, prefixes = trust_prefixes(statefulset, setting)
        if not any(pod_ip in prefix for prefix in prefixes):
            raise RuntimeError(
                f"cloudflared pod {pod_name} IP {pod_ip} is outside {setting}={raw_value}"
            )
        configured[setting] = raw_value
    return pod_name, str(pod_ip), configured


def kubectl_json(*args: str) -> dict:
    command = ["kubectl", *args]
    try:
        result = subprocess.run(command, check=True, capture_output=True, text=True)
    except subprocess.CalledProcessError as error:
        stderr = (error.stderr or "").strip()
        raise RuntimeError(
            f"{' '.join(command)} failed: {stderr or 'no stderr output'}"
        ) from error
    try:
        document = json.loads(result.stdout)
    except json.JSONDecodeError as error:
        raise RuntimeError(f"{' '.join(command)} returned invalid JSON") from error
    if not isinstance(document, dict):
        raise RuntimeError(f"{' '.join(command)} returned a non-object JSON value")
    return document


def main() -> int:
    try:
        pods = kubectl_json(
            "get",
            "pods",
            "--namespace",
            NAMESPACE,
            "--selector=app.kubernetes.io/name=cloudflared",
            "--output",
            "json",
        )
        statefulset = kubectl_json(
            "get",
            "statefulset",
            "teagramd",
            "--namespace",
            NAMESPACE,
            "--output",
            "json",
        )
        pod_name, pod_ip, configured = verify_trust(pods, statefulset)
    except (RuntimeError, subprocess.SubprocessError) as error:
        print(f"cloudflared client-address trust check failed: {error}", file=sys.stderr)
        return 1

    print(
        "cloudflared-trust=verified "
        f"pod={pod_name} pod_ip={pod_ip} "
        f"{TRUST_SETTINGS[0]}={configured[TRUST_SETTINGS[0]]} "
        f"{TRUST_SETTINGS[1]}={configured[TRUST_SETTINGS[1]]}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
