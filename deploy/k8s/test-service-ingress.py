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
LINKSELECTOR_PROBE = "linkselector-policy-probe"


class ProbeFailure(RuntimeError):
    """A probe command failed and includes kubectl's captured stderr."""


def kubectl(*args: str) -> str:
    try:
        return subprocess.run(
            ["kubectl", *args], check=True, capture_output=True, text=True
        ).stdout
    except subprocess.CalledProcessError as error:
        stderr = (error.stderr or "").strip()
        command = " ".join(("kubectl", *args))
        raise RuntimeError(
            f"{command} failed with exit {error.returncode}: "
            f"{stderr or 'no stderr output'}"
        ) from error


def exec_probe(pod: str, operation: str, container: str | None = None) -> None:
    try:
        args = ["exec", "--namespace", NAMESPACE]
        if container is not None:
            args.extend(("--container", container))
        args.extend(
            (
                pod,
                "--",
                "python3",
                "/tests/test-networkpolicy.py",
                operation,
            )
        )
        kubectl(*args)
    except RuntimeError as error:
        raise ProbeFailure(str(error)) from error


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


def teagramd_state() -> tuple[dict, list[dict]]:
    statefulset = json.loads(
        kubectl("get", "statefulset", "teagramd", "--namespace", NAMESPACE, "--output", "json")
    )
    pod_list = json.loads(
        kubectl(
            "get",
            "pods",
            "--namespace",
            NAMESPACE,
            "--selector=app.kubernetes.io/name=teagramd",
            "--output",
            "json",
        )
    )
    return statefulset, pod_list.get("items", [])


def linkselector_state() -> tuple[dict, list[dict]]:
    deployment = json.loads(
        kubectl("get", "deployment", "linkselector", "--namespace", NAMESPACE, "--output", "json")
    )
    pod_list = json.loads(
        kubectl(
            "get",
            "pods",
            "--namespace",
            NAMESPACE,
            "--selector=app.kubernetes.io/name=linkselector,app.kubernetes.io/component=server",
            "--output",
            "json",
        )
    )
    return deployment, pod_list.get("items", [])


def pod_is_ready(pod: dict) -> bool:
    if pod.get("metadata", {}).get("deletionTimestamp") is not None:
        return False
    return any(
        condition.get("type") == "Ready" and condition.get("status") == "True"
        for condition in pod.get("status", {}).get("conditions", [])
    )


def rollout_complete(statefulset: dict, pods: list[dict], generation: int, old_uid: str) -> bool:
    spec = statefulset.get("spec", {})
    status = statefulset.get("status", {})
    desired = spec.get("replicas", 1)
    current_revision = status.get("currentRevision")
    update_revision = status.get("updateRevision")
    updated_pod = next(
        (
            pod
            for pod in pods
            if pod.get("metadata", {}).get("name") == "teagramd-0"
            and pod.get("metadata", {}).get("uid") != old_uid
            and pod_is_ready(pod)
        ),
        None,
    )
    return (
        statefulset.get("metadata", {}).get("generation", 0) >= generation
        and status.get("observedGeneration", 0) >= generation
        and status.get("updatedReplicas", 0) >= desired
        and status.get("readyReplicas", 0) >= desired
        and current_revision is not None
        and current_revision == update_revision
        and updated_pod is not None
    )


def linkselector_rollout_complete(
    deployment: dict, pods: list[dict], generation: int, old_uids: set[str]
) -> bool:
    spec = deployment.get("spec", {})
    status = deployment.get("status", {})
    desired = spec.get("replicas", 1)
    ready_uids = {
        pod.get("metadata", {}).get("uid")
        for pod in pods
        if pod_is_ready(pod) and pod.get("metadata", {}).get("uid")
    }
    new_ready_pod = bool(ready_uids - old_uids)
    return (
        deployment.get("metadata", {}).get("generation", 0) >= generation
        and status.get("observedGeneration", 0) >= generation
        and status.get("replicas", 0) == desired
        and status.get("updatedReplicas", 0) == desired
        and status.get("readyReplicas", 0) == desired
        and status.get("availableReplicas", 0) == desired
        and new_ready_pod
    )


def rollout_linkselector(cloudflared: str) -> int:
    _, before_pods = linkselector_state()
    old_uids = {
        pod.get("metadata", {}).get("uid")
        for pod in before_pods
        if pod.get("metadata", {}).get("uid")
    }
    if not old_uids or not any(pod_is_ready(pod) for pod in before_pods):
        raise RuntimeError("linkselector has no Ready pod UID to verify against before restart")

    kubectl("rollout", "restart", "deployment/linkselector", "--namespace", NAMESPACE)
    restarted, _ = linkselector_state()
    target_generation = restarted["metadata"]["generation"]
    started = time.monotonic()
    probes = 0
    while time.monotonic() - started < 300:
        tick = time.monotonic()
        exec_probe(cloudflared, "linkselector", "ingress-probe")
        probes += 1
        deployment, pods = linkselector_state()
        if linkselector_rollout_complete(deployment, pods, target_generation, old_uids):
            break
        time.sleep(max(0.0, 1.0 - (time.monotonic() - tick)))
    else:
        raise TimeoutError("linkselector did not finish rolling out within 300 seconds")
    kubectl("rollout", "status", "deployment/linkselector", "--namespace", NAMESPACE, "--timeout=180s")
    deployment, pods = linkselector_state()
    if not linkselector_rollout_complete(deployment, pods, target_generation, old_uids):
        raise RuntimeError("linkselector rollout completed without all replicas on a new Ready pod")
    exec_probe(cloudflared, "linkselector", "ingress-probe")
    return probes + 1


def rollout_teagramd() -> tuple[int, int]:
    before, before_pods = teagramd_state()
    old_pod = next(
        (
            pod
            for pod in before_pods
            if pod.get("metadata", {}).get("name") == "teagramd-0"
            and pod.get("metadata", {}).get("uid")
        ),
        None,
    )
    if old_pod is None:
        raise RuntimeError("teagramd StatefulSet has no pod UID to verify against before restart")
    old_uid = old_pod["metadata"]["uid"]

    kubectl("rollout", "restart", "statefulset/teagramd", "--namespace", NAMESPACE)
    restarted, _ = teagramd_state()
    target_generation = restarted["metadata"]["generation"]
    started = time.monotonic()
    probes = 0
    failures_while_not_ready = 0
    last_probe_error = ""
    while time.monotonic() - started < 300:
        tick = time.monotonic()
        _, current_pods = teagramd_state()
        ready_before_probe = any(pod_is_ready(pod) for pod in current_pods)
        probes += 1
        try:
            exec_probe(TAILNET_PROBE, "discovery")
            last_probe_error = ""
        except ProbeFailure as error:
            last_probe_error = str(error)
            if ready_before_probe:
                raise RuntimeError(
                    f"teagramd discovery failed while a pod was Ready: {last_probe_error}"
                ) from error
            failures_while_not_ready += 1

        statefulset, pods = teagramd_state()
        if not last_probe_error and rollout_complete(
            statefulset, pods, target_generation, old_uid
        ):
            break
        time.sleep(max(0.0, 1.0 - (time.monotonic() - tick)))
    else:
        raise TimeoutError(
            "teagramd did not complete its new-generation rollout and answer discovery "
            f"within 300 seconds; old pod UID={old_uid}; last probe error="
            f"{last_probe_error or 'none'}"
        )

    kubectl("rollout", "status", "statefulset/teagramd", "--namespace", NAMESPACE, "--timeout=180s")
    _, final_pods = teagramd_state()
    final_pod = next(
        (pod for pod in final_pods if pod.get("metadata", {}).get("name") == "teagramd-0"),
        None,
    )
    if final_pod is None or not pod_is_ready(final_pod):
        raise RuntimeError("teagramd rollout completed without a Ready pod")
    if final_pod.get("metadata", {}).get("uid") == old_uid:
        raise RuntimeError(f"teagramd rollout kept the original pod UID {old_uid}")
    exec_probe(TAILNET_PROBE, "discovery")
    return probes, failures_while_not_ready


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.parse_args()

    for pod in (TAILNET_PROBE, UNTRUSTED_PROBE, LINKSELECTOR_PROBE):
        kubectl("wait", "--for=condition=Ready", f"pod/{pod}", "--namespace", NAMESPACE, "--timeout=180s")
    cloudflared = cloudflared_probe()
    exec_probe(TAILNET_PROBE, "kube-api")
    exec_probe(TAILNET_PROBE, "discovery")
    exec_probe(TAILNET_PROBE, "check-listeners")
    exec_probe(TAILNET_PROBE, "bot-api")
    exec_probe(TAILNET_PROBE, "deny-public")
    exec_probe(TAILNET_PROBE, "deny-egress")
    exec_probe(cloudflared, "public-ingress", "ingress-probe")
    exec_probe(cloudflared, "deny-tailnet", "ingress-probe")
    for pod in (UNTRUSTED_PROBE, LINKSELECTOR_PROBE):
        exec_probe(pod, "deny-tailnet")
        exec_probe(pod, "deny-public")

    link_probes = rollout_linkselector(cloudflared)
    teagramd_probes, failures = rollout_teagramd()
    print(f"linkselector rollout: {link_probes} one-second cloudflared probes, no failures")
    print(
        "teagramd rollout: "
        f"{teagramd_probes} one-second tailnet probes, {failures} failures while no pod was Ready; "
        "the new StatefulSet generation completed with a changed Ready pod UID"
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as error:
        print(f"Service ingress check failed: {error}", file=sys.stderr)
        raise SystemExit(1)
