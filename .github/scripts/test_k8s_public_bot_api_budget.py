#!/usr/bin/env python3
"""Regression tests for the public Bot API client-address budget probe."""

from __future__ import annotations

import importlib.util
from pathlib import Path
import sys
import unittest
from unittest import mock


REPO_ROOT = Path(__file__).parents[2]
PROBE_DISCOVERY_SPEC = importlib.util.spec_from_file_location(
    "probe_discovery", REPO_ROOT / "deploy/k8s/probe-discovery.py"
)
if PROBE_DISCOVERY_SPEC is None or PROBE_DISCOVERY_SPEC.loader is None:
    raise RuntimeError("could not load the shared discovery probe")
PROBE_DISCOVERY = importlib.util.module_from_spec(PROBE_DISCOVERY_SPEC)
sys.modules[PROBE_DISCOVERY_SPEC.name] = PROBE_DISCOVERY
PROBE_DISCOVERY_SPEC.loader.exec_module(PROBE_DISCOVERY)
SPEC = importlib.util.spec_from_file_location(
    "test_networkpolicy", REPO_ROOT / "deploy/k8s/test-networkpolicy.py"
)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError("could not load the network policy probe")
PROBE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROBE)


class FakeResponse:
    def __init__(self, status: int):
        self.status = status

    def read(self) -> bytes:
        return b""


class FakeConnection:
    """Match HTTPConnection's request API without its context-manager protocol."""

    def __init__(self, statuses: list[int]):
        self.statuses = list(statuses)
        self.requests: list[tuple[str, str, dict[str, str]]] = []
        self.closed = False

    def request(self, method: str, path: str, headers: dict[str, str]) -> None:
        self.requests.append((method, path, headers))

    def getresponse(self) -> FakeResponse:
        return FakeResponse(self.statuses.pop(0))

    def close(self) -> None:
        self.closed = True


class PublicBotApiBudgetProbeTest(unittest.TestCase):
    def test_separates_client_budgets_and_closes_connections(self):
        connections: list[FakeConnection] = []
        response_sequences = iter(
            [[401] * PROBE.BOT_API_AUTH_BURST + [429], [401]]
        )

        def create_connection(*args, **kwargs):
            connection = FakeConnection(next(response_sequences))
            connections.append(connection)
            return connection

        with mock.patch.object(
            PROBE.http.client, "HTTPConnection", side_effect=create_connection
        ):
            PROBE.probe_public_bot_api_budget("teagramd")

        self.assertEqual(len(connections), 2)
        self.assertTrue(all(connection.closed for connection in connections))
        self.assertEqual(len(connections[0].requests), PROBE.BOT_API_AUTH_BURST + 1)
        self.assertEqual(len(connections[1].requests), 1)
        self.assertTrue(
            all(
                request[2]["CF-Connecting-IP"] == "198.51.100.10"
                for request in connections[0].requests
            )
        )
        self.assertEqual(
            connections[1].requests[0][2]["CF-Connecting-IP"], "198.51.101.10"
        )


if __name__ == "__main__":
    unittest.main()
