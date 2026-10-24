#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import importlib.util
from pathlib import Path
import unittest

MODULE_PATH = Path(__file__).with_name("customize.py")
SPEC = importlib.util.spec_from_file_location("customize", MODULE_PATH)
assert SPEC and SPEC.loader
customize = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(customize)


class IdentityStateTest(unittest.TestCase):
    def test_accepts_locked_accounts_without_credentials(self):
        customize.validate_identity_state(
            "root:x:0:0:root:/root:/bin/bash\nubuntu:x:1000:1000::/home/ubuntu:/bin/bash\n",
            "root:!:1:0:99999:7:::\nubuntu:*:1:0:99999:7:::\n",
            [],
        )

    def test_rejects_password_hash(self):
        with self.assertRaisesRegex(RuntimeError, "captured password credential"):
            customize.validate_identity_state(
                "ubuntu:x:1000:1000::/home/ubuntu:/bin/bash\n",
                "ubuntu:$6$captured:1:0:99999:7:::\n",
                [],
            )

    def test_rejects_authorized_keys(self):
        with self.assertRaisesRegex(RuntimeError, "captured SSH credential"):
            customize.validate_identity_state(
                "ubuntu:x:1000:1000::/home/ubuntu:/bin/bash\n",
                "ubuntu:!:1:0:99999:7:::\n",
                ["/home/ubuntu/.ssh/authorized_keys"],
            )


if __name__ == "__main__":
    unittest.main()
