#!/usr/bin/env python3
"""Unit tests for Kubernetes Service ingress probes."""

from __future__ import annotations

import runpy
import unittest
from pathlib import Path


pod_is_ready = runpy.run_path(
    str(Path(__file__).with_name("test-service-ingress.py"))
)["pod_is_ready"]


class PodReadinessTests(unittest.TestCase):
    def test_terminating_ready_pod_is_not_live_for_service_probes(self) -> None:
        pod = {
            "metadata": {"deletionTimestamp": "2026-11-11T06:05:34Z"},
            "status": {
                "conditions": [{"type": "Ready", "status": "True"}],
            },
        }

        self.assertFalse(pod_is_ready(pod))


if __name__ == "__main__":
    unittest.main()
