#!/usr/bin/env python3
"""Render a proxy egress policy for the current Kubernetes API endpoints."""

from __future__ import annotations

import ipaddress
import json
import sys


def main() -> int:
    try:
        endpoints = json.load(sys.stdin)
    except json.JSONDecodeError as error:
        raise SystemExit("Kubernetes API endpoints are not valid JSON") from error

    if not isinstance(endpoints, dict) or not isinstance(endpoints.get("subsets"), list):
        raise SystemExit("Kubernetes API endpoints have an invalid structure")

    addresses = set()
    for subset in endpoints["subsets"]:
        if not isinstance(subset, dict):
            raise SystemExit("Kubernetes API endpoint subset has an invalid structure")
        ready_addresses = subset.get("addresses", [])
        if not isinstance(ready_addresses, list):
            raise SystemExit("Kubernetes API ready addresses have an invalid structure")
        for address in ready_addresses:
            if not isinstance(address, dict) or not isinstance(address.get("ip"), str):
                raise SystemExit("Kubernetes API endpoint address has an invalid IP")
            addresses.add(address["ip"])
    if not addresses:
        raise SystemExit("Kubernetes API Service has no ready endpoints")

    cidrs = []
    for address in sorted(addresses):
        try:
            parsed = ipaddress.ip_address(address)
        except ValueError as error:
            raise SystemExit(f"Kubernetes API endpoint is not an IP address: {address!r}") from error
        if parsed.version != 4:
            raise SystemExit("Kubernetes API endpoint must use IPv4")
        cidrs.append(f"{parsed}/32")

    print("apiVersion: networking.k8s.io/v1")
    print("kind: NetworkPolicy")
    print("metadata:")
    print("  name: tailscale-proxy-api-egress")
    print("  namespace: teagram")
    print("  labels:")
    print("    app.kubernetes.io/name: tailscale-proxy")
    print("spec:")
    print("  podSelector:")
    print("    matchLabels:")
    print("      app.kubernetes.io/name: tailscale-proxy")
    print("  policyTypes:")
    print("    - Egress")
    print("  egress:")
    print("    - to:")
    for cidr in cidrs:
        print("        - ipBlock:")
        print(f"            cidr: {cidr}")
    print("      ports:")
    print("        - protocol: TCP")
    print("          port: 6443")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
