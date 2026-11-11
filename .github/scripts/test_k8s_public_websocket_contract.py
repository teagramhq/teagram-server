#!/usr/bin/env python3
"""Contract tests for the public WebSocket connector smoke check."""

import importlib.util
import pathlib
import unittest


SCRIPT_PATH = pathlib.Path(__file__).with_name("test-k8s-public-websocket.py")
SPEC = importlib.util.spec_from_file_location("public_websocket_smoke", SCRIPT_PATH)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError("could not load public WebSocket smoke script")
SMOKE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(SMOKE)


class DiscoveryDocumentContractTest(unittest.TestCase):
    def test_accepts_canonical_nested_mtproto_identity(self):
        document = {
            "version": 1,
            "mtproto": {
                "endpoint": "server.example.test:443",
                "dc_id": 2,
                "rsa_spki": "canonical-spki",
            },
            "websocket": {"endpoint": "wss://server.example.test/apiws"},
        }

        SMOKE.validate_discovery_document(document)

    def test_rejects_missing_nested_mtproto_identity(self):
        with self.assertRaisesRegex(RuntimeError, "mtproto.rsa_spki"):
            SMOKE.validate_discovery_document({"version": 1, "rsa_spki": "top-level-only"})

    def test_rejects_discovery_for_a_different_public_host(self):
        with self.assertRaisesRegex(RuntimeError, "wrong WebSocket endpoint"):
            SMOKE.validate_discovery_document(
                {
                    "mtproto": {"rsa_spki": "canonical-spki"},
                    "websocket": {"endpoint": "wss://other.example.test/apiws"},
                }
            )


if __name__ == "__main__":
    unittest.main()
