#!/usr/bin/env python3
"""Tests for the demo's bounded private-network isolation probes."""

from __future__ import annotations

import pathlib
import socket
import sys
import unittest
from unittest import mock


SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPT_DIR))
import probe_tailnet_isolation  # noqa: E402


class ProbeTailnetIsolationTests(unittest.TestCase):
    def test_known_private_targets_timeout(self) -> None:
        with mock.patch.object(probe_tailnet_isolation, "probe_target") as probe:
            probe_tailnet_isolation.main()

        self.assertEqual(probe.call_count, 3)
        self.assertEqual(
            [call.args[:3] for call in probe.call_args_list],
            list(probe_tailnet_isolation.TARGETS),
        )

    def test_timeout_is_the_only_success_condition(self) -> None:
        with mock.patch.object(
            probe_tailnet_isolation.socket,
            "create_connection",
            side_effect=TimeoutError,
        ):
            probe_tailnet_isolation.probe_target("tailnet", "100.124.236.66", 22)

    def test_reachable_destination_fails(self) -> None:
        connection = mock.Mock()
        with mock.patch.object(
            probe_tailnet_isolation.socket,
            "create_connection",
            return_value=connection,
        ), self.assertRaises(SystemExit):
            probe_tailnet_isolation.probe_target("tailnet", "100.124.236.66", 22)
        connection.close.assert_called_once_with()

    def test_unreachable_route_error_is_not_reported_as_a_timeout(self) -> None:
        with mock.patch.object(
            probe_tailnet_isolation.socket,
            "create_connection",
            side_effect=OSError("network unreachable"),
        ), self.assertRaises(SystemExit):
            probe_tailnet_isolation.probe_target("tailnet", "100.124.236.66", 22)


if __name__ == "__main__":
    unittest.main()
