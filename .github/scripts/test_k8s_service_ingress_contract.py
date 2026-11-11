#!/usr/bin/env python3
"""Contract tests for the Kubernetes service ingress rollout check."""

import importlib.util
import pathlib
import unittest


SCRIPT_PATH = pathlib.Path(__file__).parents[2] / "deploy/k8s/test-service-ingress.py"
SPEC = importlib.util.spec_from_file_location("service_ingress", SCRIPT_PATH)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError("could not load service ingress script")
SERVICE_INGRESS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(SERVICE_INGRESS)


class PodReadinessContractTest(unittest.TestCase):
    def test_nonterminating_ready_pod_is_available(self):
        pod = {"status": {"conditions": [{"type": "Ready", "status": "True"}]}}

        self.assertTrue(SERVICE_INGRESS.pod_is_ready(pod))

    def test_terminating_pod_is_not_available_even_if_ready_condition_is_stale(self):
        pod = {
            "metadata": {"deletionTimestamp": "2026-11-11T04:34:19Z"},
            "status": {"conditions": [{"type": "Ready", "status": "True"}]},
        }

        self.assertFalse(SERVICE_INGRESS.pod_is_ready(pod))


if __name__ == "__main__":
    unittest.main()
