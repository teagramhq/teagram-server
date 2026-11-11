#!/usr/bin/env python3
"""Probe Bot API routing after a failed release restores the pre-PR pod."""

from __future__ import annotations

import json
from pathlib import Path
import subprocess
import unittest


REPO_ROOT = Path(__file__).parents[2]
LEGACY_SERVICE_TARGET_PORT = "bot-api-ingress"
LEGACY_POLICY_PORTS = [2448]
LEGACY_POD_PORTS = {"bot-api-ingress": 2448}
BOT_API_SERVICE_QUERY = (
    'select(.metadata.name == "teagramd")'
    ' | .spec.ports[] | select(.name == "bot-api")'
    ' | {"port": .port, "targetPort": .targetPort}'
)


def yq_json(path: str, expression: str):
    result = subprocess.run(
        ["yq", "--output-format=json", expression, str(REPO_ROOT / path)],
        check=True,
        capture_output=True,
        text=True,
    )
    return json.loads(result.stdout)


def resolve_named_port(target: int | str, pod_ports: dict[str, int]) -> int:
    if isinstance(target, int):
        return target
    return pod_ports[target]


def policy_allows(ports: list[int | str], pod_ports: dict[str, int], target: int) -> bool:
    return any(resolve_named_port(port, pod_ports) == target for port in ports)


class BotApiRollbackCompatibilityTest(unittest.TestCase):
    def test_post_apply_rollback_routes_to_the_pre_pr_listener(self):
        # The pre-PR StatefulSet ran bot-api-ingress:2448 in a sidecar, and
        # the old Service and policies targeted/allowed TCP 2448.
        self.assertEqual(
            resolve_named_port(LEGACY_SERVICE_TARGET_PORT, LEGACY_POD_PORTS), 2448
        )
        self.assertTrue(policy_allows(LEGACY_POLICY_PORTS, LEGACY_POD_PORTS, 2448))

        # deploy-release rolls controllers back after a failed apply but leaves
        # Services and NetworkPolicies applied. Evaluate those current resources
        # against the restored pre-PR pod, which is the state the rollback must serve.
        service = yq_json(
            "deploy/k8s/base/teagramd-service.yaml",
            BOT_API_SERVICE_QUERY,
        )
        ingress_policy_ports = yq_json(
            "deploy/k8s/base/teagramd-networkpolicy.yaml",
            "[.spec.ingress[0].ports[].port]",
        )
        egress_policy_ports = yq_json(
            "deploy/k8s/base/tailscale-proxy-egress.yaml",
            "[.spec.egress[0].ports[].port]",
        )

        self.assertEqual(service["port"], 2447)
        self.assertEqual(service["targetPort"], LEGACY_SERVICE_TARGET_PORT)
        self.assertTrue(
            policy_allows(ingress_policy_ports, LEGACY_POD_PORTS, 2448),
            "teagramd ingress policy blocks the restored pre-PR listener",
        )
        self.assertTrue(
            policy_allows(egress_policy_ports, LEGACY_POD_PORTS, 2448),
            "Tailscale proxy egress policy blocks the restored pre-PR listener",
        )

    def test_current_release_resolves_the_same_port_name_to_2447(self):
        containers = yq_json(
            "deploy/k8s/base/teagramd-statefulset.yaml",
            ".spec.template.spec.containers",
        )
        pod_ports = {
            port["name"]: port["containerPort"]
            for container in containers
            for port in container.get("ports", [])
        }
        self.assertEqual(pod_ports[LEGACY_SERVICE_TARGET_PORT], 2447)

        ingress_policy_ports = yq_json(
            "deploy/k8s/base/teagramd-networkpolicy.yaml",
            "[.spec.ingress[0].ports[].port]",
        )
        egress_policy_ports = yq_json(
            "deploy/k8s/base/tailscale-proxy-egress.yaml",
            "[.spec.egress[0].ports[].port]",
        )
        self.assertTrue(policy_allows(ingress_policy_ports, pod_ports, 2447))
        self.assertTrue(policy_allows(egress_policy_ports, pod_ports, 2447))


if __name__ == "__main__":
    unittest.main()
