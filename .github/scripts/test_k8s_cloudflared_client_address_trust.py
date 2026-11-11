#!/usr/bin/env python3
"""Contract tests for the post-deploy cloudflared address trust guard."""

from __future__ import annotations

import importlib.util
from pathlib import Path
import unittest


SCRIPT_PATH = Path(__file__).parents[2] / "deploy/k8s/verify-cloudflared-client-address-trust.py"
SPEC = importlib.util.spec_from_file_location("cloudflared_client_address_trust", SCRIPT_PATH)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError("could not load cloudflared client-address trust checker")
CHECKER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECKER)


def ready_pods(pod_ip: str = "10.42.1.17") -> dict:
    return {
        "items": [
            {
                "metadata": {"name": "cloudflared-abc123"},
                "status": {
                    "podIP": pod_ip,
                    "conditions": [{"type": "Ready", "status": "True"}],
                },
            }
        ]
    }


def teagramd_state(
    client_cidrs: str = "10.42.0.0/16",
    bot_api_cidrs: str = "10.42.0.0/16",
) -> dict:
    return {
        "spec": {
            "template": {
                "spec": {
                    "containers": [
                        {
                            "name": "teagramd",
                            "env": [
                                {
                                    "name": "TG_CLIENT_ADDR_CF_PROXY_CIDRS",
                                    "value": client_cidrs,
                                },
                                {
                                    "name": "TG_BOT_API_PUBLIC_PROXY_CIDRS",
                                    "value": bot_api_cidrs,
                                },
                            ],
                        }
                    ]
                }
            }
        }
    }


class CloudflaredClientAddressTrustContractTest(unittest.TestCase):
    def test_ready_connector_ip_must_be_covered_by_both_public_trust_lists(self):
        name, address, configured = CHECKER.verify_trust(
            ready_pods(), teagramd_state("10.42.0.0/16", "10.42.1.17/32")
        )

        self.assertEqual(name, "cloudflared-abc123")
        self.assertEqual(address, "10.42.1.17")
        self.assertEqual(
            configured,
            {
                "TG_CLIENT_ADDR_CF_PROXY_CIDRS": "10.42.0.0/16",
                "TG_BOT_API_PUBLIC_PROXY_CIDRS": "10.42.1.17/32",
            },
        )

    def test_fails_when_websocket_trust_does_not_include_connector_ip(self):
        with self.assertRaisesRegex(RuntimeError, "TG_CLIENT_ADDR_CF_PROXY_CIDRS"):
            CHECKER.verify_trust(ready_pods(), teagramd_state("10.43.0.0/16"))

    def test_fails_when_bot_api_trust_does_not_include_connector_ip(self):
        with self.assertRaisesRegex(RuntimeError, "TG_BOT_API_PUBLIC_PROXY_CIDRS"):
            CHECKER.verify_trust(
                ready_pods(), teagramd_state(bot_api_cidrs="10.43.0.0/16")
            )

    def test_fails_without_exactly_one_ready_connector(self):
        with self.assertRaisesRegex(RuntimeError, "expected one Ready cloudflared pod"):
            CHECKER.verify_trust({"items": []}, teagramd_state())

    def test_fails_on_invalid_trust_cidr(self):
        with self.assertRaisesRegex(RuntimeError, "contains invalid CIDR"):
            CHECKER.verify_trust(ready_pods(), teagramd_state(client_cidrs="not-a-cidr"))


if __name__ == "__main__":
    unittest.main()
