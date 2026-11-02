#!/usr/bin/env python3
"""Tests for the production Compose restart policy contract."""

from __future__ import annotations

import importlib.util
import unittest
from pathlib import Path
from typing import Any


CHECKER_PATH = Path(__file__).with_name("check-prod-compose-restart-policy.py")
SPEC = importlib.util.spec_from_file_location("prod_compose_restart_policy", CHECKER_PATH)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"cannot load {CHECKER_PATH}")
CHECKER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECKER)


def resolved_compose() -> dict[str, Any]:
    services = {
        name: {"restart": "no"}
        for name in CHECKER.ONE_SHOT_SERVICES
    }
    services.update(
        {
            "postgres": {"restart": "unless-stopped"},
            "rustfs": {"restart": "unless-stopped"},
            "teagramd": {"restart": "unless-stopped"},
            "public-haproxy": {"restart": "unless-stopped"},
            "teagramd-public-1": {"restart": "unless-stopped"},
            "teagramd-public-2": {"restart": "unless-stopped"},
        }
    )
    return {"services": services}


class RestartPolicyContractTests(unittest.TestCase):
    def test_accepts_all_long_running_profiles_and_excludes_one_shots(self) -> None:
        compose = resolved_compose()
        compose["services"]["new-worker"] = {"restart": "on-failure:3"}

        self.assertEqual([], CHECKER.validate_compose(compose))

    def test_rejects_a_long_running_service_without_a_policy(self) -> None:
        compose = resolved_compose()
        compose["services"]["new-worker"] = {"image": "worker:latest"}

        errors = CHECKER.validate_compose(compose)

        self.assertTrue(any("new-worker" in error and "needs a restart policy" in error for error in errors))

    def test_rejects_explicit_no_on_a_long_running_service(self) -> None:
        compose = resolved_compose()
        compose["services"]["new-worker"] = {"restart": "no"}

        errors = CHECKER.validate_compose(compose)

        self.assertTrue(any("new-worker" in error and "needs a restart policy" in error for error in errors))

    def test_rejects_restart_policy_on_a_one_shot(self) -> None:
        compose = resolved_compose()
        compose["services"]["migrate"]["restart"] = "unless-stopped"

        errors = CHECKER.validate_compose(compose)

        self.assertTrue(any("one-shot service 'migrate' must not restart" in error for error in errors))


if __name__ == "__main__":
    unittest.main()
