#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

from pathlib import Path
import unittest


class VerifyTest(unittest.TestCase):
    def test_nvlink_policy_is_exact(self):
        config = Path(__file__).with_name("99-nvidia-disable-nvlink.conf")
        self.assertEqual(config.read_text(), "options nvidia NVreg_NvLinkDisable=1\n")


if __name__ == "__main__":
    unittest.main()
