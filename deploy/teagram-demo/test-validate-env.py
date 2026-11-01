#!/usr/bin/env python3
"""Check the demo environment validator's optional Turnstile settings."""

from __future__ import annotations

import contextlib
import importlib.util
import io
import pathlib
import unittest
from unittest import mock


SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("validate_env", SCRIPT_DIR / "validate-env.py")
assert SPEC is not None and SPEC.loader is not None
VALIDATOR = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VALIDATOR)


class ValidateEnvTurnstileTests(unittest.TestCase):
    def setUp(self) -> None:
        self.values = {
            **VALIDATOR.EXPECTED,
            "TG_WEBSOCKET_ALLOWED_ORIGINS": "https://web.teagram.io,https://demo.teagram.io",
            "POSTGRES_PASSWORD": "a" * 64,
            "TG_AUTHKEY_ENC_KEY": "b" * 64,
            "TEAGRAM_DEMO_TURNSTILE_SITE_KEY": "fixture-site-key",
        }
        self.secret = mock.patch.object(VALIDATOR, "read_turnstile_secret", return_value="fixture-secret")
        self.secret.start()
        self.addCleanup(self.secret.stop)
        self.qa_bypass_token = mock.patch.object(
            VALIDATOR, "read_qa_bypass_token", return_value="fixture-qa-bypass-token"
        )
        self.qa_bypass_token.start()
        self.addCleanup(self.qa_bypass_token.stop)

    def validate(self, values: dict[str, str]) -> None:
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            VALIDATOR.validate_values(values)

    def test_missing_signin_flag_keeps_false_default(self) -> None:
        self.validate(self.values)

    def test_signin_flag_accepts_both_boolean_values(self) -> None:
        for value in ("false", "true"):
            with self.subTest(value=value):
                self.validate({**self.values, "TG_TURNSTILE_SIGNIN_ENABLED": value})

    def test_signin_flag_rejects_non_boolean_values(self) -> None:
        for value in ("", "1", "yes"):
            with self.subTest(value=value), self.assertRaises(SystemExit):
                self.validate({**self.values, "TG_TURNSTILE_SIGNIN_ENABLED": value})

    def test_signup_advertisement_remains_true_when_flag_is_enabled(self) -> None:
        self.validate({
            **self.values,
            "TG_ADVERTISE_DEMO_GUEST_SIGN_UP": "true",
            "TG_TURNSTILE_SIGNIN_ENABLED": "true",
        })

    def test_signup_advertisement_cannot_be_disabled(self) -> None:
        with self.assertRaises(SystemExit):
            self.validate({**self.values, "TG_ADVERTISE_DEMO_GUEST_SIGN_UP": "false"})

    def test_unknown_environment_setting_is_rejected(self) -> None:
        with self.assertRaises(SystemExit):
            self.validate({**self.values, "TG_UNAPPROVED_SETTING": "true"})


if __name__ == "__main__":
    unittest.main()
