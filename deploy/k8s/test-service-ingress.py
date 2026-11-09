#!/usr/bin/env python3
"""Probe Service ingress from the CI tailnet and cloudflared test pods."""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import time


NAMESPACE = "teagram"
TAILNET_PROBE = "tailnet-policy-probe"
UNTRUSTED_PROBE = "untrusted-policy-probe"


def kubectl(*args: str) -> str:
    return subprocess.run(
        ["kubectl", *args], check=True, capture_output=True, text=True
    ).stdout


def exec_probe(pod: str, operation: str) -> None:
    kubectl(
        "exec",
        "--namespace",
        NAMESPACE,
        pod,
        "--",
        "python3",
        "/tests/test-networkpolicy.py",
        operation,
    )


def cloudflared_probe() -> str:
    output = kubectl(
        "get",
        "pods",
        "--namespace",
        NAMESPACE,
        "--selector=app.kubernetes.io/name=cloudflared",
        "--output",
        "json",
    )
    pods = json.loads(output).get("items", [])
    ready = [
        pod["metadata"]["name"]
        for pod in pods
        if any(
            condition.get("type") == "Ready" and condition.get("status") == "True"
            for condition in pod.get("status", {}).get("conditions", [])
        )
    ]
    if not ready:
        raise RuntimeError("cloudflared has no Ready CI probe pod")
    return ready[0]


def teagramd_ready() -> bool:
    output = kubectl(
        "get",
        "pods",
        "--namespace",
        NAMESPACE,
        "--selector=app.kubernetes.io/name=teagramd",
        "--output",
        "json",
    )
    pods = json.loads(output).get("items", [])
    return any(
        condition.get("type") == "Ready" and condition.get("status") == "True"
        for pod in pods
        for condition in pod.get("status", {}).get("conditions", [])
    )


def rollout_linkselector(cloudflared: str) -> int:
    kubectl("rollout", "restart", "deployment/linkselector", "--namespace", NAMESPACE)
    started = time.monotonic()
    probes = 0
    while time.monotonic() - started < 300:
        tick = time.monotonic()
        exec_probe(cloudflared, "linkselector")
        probes += 1
        deployment = json.loads(
            kubectl("get", "deployment", "linkselector", "--namespace", NAMESPACE, "--output", "json")
        )
        spec = deployment.get("spec", {})
        status = deployment.get("status", {})
        if (
            status.get("observedGeneration", 0) >= deployment["metadata"]["generation"]
            and status.get("readyReplicas", 0) >= spec.get("replicas", 1)
            and status.get("updatedReplicas", 0) >= spec.get("replicas", 1)
        ):
            break
        time.sleep(max(0.0, 1.0 - (time.monotonic() - tick)))
    else:
        raise TimeoutError("linkselector did not finish rolling out within 300 seconds")
    kubectl("rollout", "status", "deployment/linkselector", "--namespace", NAMESPACE, "--timeout=180s")
    exec_probe(cloudflared, "linkselector")
    return probes + 1


def rollout_teagramd() -> tuple[int, int]:
    kubectl("rollout", "restart", "statefulset/teagramd", "--namespace", NAMESPACE)
    started = time.monotonic()
    probes = 0
    failures_while_not_ready = 0
    first_ready_success = False
    while time.monotonic() - started < 300:
        tick = time.monotonic()
        ready_before = teagramd_ready()
        probes += 1
        try:
            exec_probe(TAILNET_PROBE, "discovery")
            if ready_before:
                first_ready_success = True
        except subprocess.CalledProcessError as error:
            if teagramd_ready():
                raise RuntimeError("teagramd discovery failed while a pod was Ready") from error
            failures_while_not_ready += 1

        if first_ready_success:
            break
        time.sleep(max(0.0, 1.0 - (time.monotonic() - tick)))
    else:
        raise TimeoutError("teagramd did not become Ready and answer discovery within 300 seconds")

    kubectl("rollout", "status", "statefulset/teagramd", "--namespace", NAMESPACE, "--timeout=180s")
    if not teagramd_ready():
        raise RuntimeError("teagramd rollout completed without a Ready pod")
    exec_probe(TAILNET_PROBE, "discovery")
    return probes, failures_while_not_ready


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.parse_args()

    for pod in (TAILNET_PROBE, UNTRUSTED_PROBE):
        kubectl("wait", "--for=condition=Ready", f"pod/{pod}", "--namespace", NAMESPACE, "--timeout=180s")
    cloudflared = cloudflared_probe()
    exec_probe(TAILNET_PROBE, "discovery")
    exec_probe(TAILNET_PROBE, "check-listeners")
    exec_probe(TAILNET_PROBE, "bot-api")
    exec_probe(cloudflared, "deny-tailnet")
    exec_probe(UNTRUSTED_PROBE, "deny-tailnet")

    link_probes = rollout_linkselector(cloudflared)
    teagramd_probes, failures = rollout_teagramd()
    print(f"linkselector rollout: {link_probes} one-second cloudflared probes, no failures")
    print(
        "teagramd rollout: "
        f"{teagramd_probes} one-second tailnet probes, {failures} failures while no pod was Ready; "
        "the first probe after Ready succeeded"
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as error:
        print(f"Service ingress check failed: {error}", file=sys.stderr)
        raise SystemExit(1)
