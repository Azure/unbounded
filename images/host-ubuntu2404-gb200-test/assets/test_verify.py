#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

from pathlib import Path
import unittest


class VerifyTest(unittest.TestCase):
    def test_boot_kernel_discovery_uses_boot_artifact(self):
        customize = Path(__file__).with_name("customize.sh").read_text()
        self.assertIn("set -- /boot/vmlinuz-*", customize)
        self.assertNotIn("readlink -f /vmlinuz", customize)

    def test_pinned_package_install_resolves_dependencies(self):
        customize = Path(__file__).with_name("customize.sh").read_text()
        self.assertIn(
            "dpkg --install /tmp/wget.deb /tmp/ssh-import-id.deb",
            customize,
        )
        self.assertNotIn("apt-get install", customize)

    def test_nvlink_policy_is_exact(self):
        config = Path(__file__).with_name("99-nvidia-disable-nvlink.conf")
        self.assertEqual(config.read_text(), "options nvidia NVreg_NvLinkDisable=1\n")


if __name__ == "__main__":
    unittest.main()
