#!/usr/bin/env python3
import base64
import string
import unittest

from qa_bot_record import build_qa_bot_record


class QABotRecordTest(unittest.TestCase):
    def setUp(self):
        self.secret = bytes(range(32))
        encoded = base64.urlsafe_b64encode(self.secret).decode("ascii").rstrip("=")
        self.token = f"42:{encoded}"

    def test_valid_token_is_encoded_for_qa_account_writer(self):
        self.assertEqual(
            build_qa_bot_record(self.token),
            f"bot_42\t{self.secret.hex()}\n",
        )

    def test_noncanonical_base64url_is_rejected(self):
        alphabet = string.ascii_uppercase + string.ascii_lowercase + string.digits + "-_"
        encoded = self.token.rsplit(":", 1)[1]
        final_value = alphabet.index(encoded[-1])
        noncanonical = alphabet[(final_value & ~3) | ((final_value + 1) & 3)]
        with self.assertRaises(ValueError):
            build_qa_bot_record(f"42:{encoded[:-1]}{noncanonical}")

    def test_wrong_length_token_is_rejected(self):
        encoded = base64.urlsafe_b64encode(bytes(range(31))).decode("ascii").rstrip("=")
        with self.assertRaises(ValueError):
            build_qa_bot_record(f"42:{encoded}")


if __name__ == "__main__":
    unittest.main()
