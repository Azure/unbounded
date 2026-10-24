#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

from pathlib import Path
import tempfile
import unittest

from render_boot_vendor_data import render_vendor_data


class RenderBootVendorDataTest(unittest.TestCase):
    def test_preserves_metalman_shape_with_safe_boot_fixtures(self):
        assets = Path(__file__).resolve().parent
        template = assets.parents[1] / "netboot" / "assets" / "vendor-data.tmpl"

        rendered = render_vendor_data(
            template,
            assets / "verify-boot-install.sh",
            assets / "verify-boot-ssh-import-id.sh",
        )

        self.assertNotIn("{{", rendered)
        self.assertNotIn("\nusers:", rendered)
        self.assertEqual(rendered.count("reporting:"), 1)
        self.assertIn("type: log", rendered)
        self.assertEqual(rendered.count("write_files:"), 1)
        self.assertIn("path: /usr/local/bin/ssh-import-id", rendered)
        self.assertEqual(rendered.count("runcmd:"), 1)
        self.assertIn("bash /usr/local/bin/unbounded-agent-install.sh", rendered)

    def test_rejects_template_shape_drift(self):
        assets = Path(__file__).resolve().parent
        with tempfile.TemporaryDirectory() as directory:
            template = Path(directory) / "vendor-data.tmpl"
            template.write_text("#cloud-config\nwrite_files: []\nruncmd: []\n")

            with self.assertRaisesRegex(RuntimeError, "reporting template changed"):
                render_vendor_data(
                    template,
                    assets / "verify-boot-install.sh",
                    assets / "verify-boot-ssh-import-id.sh",
                )


if __name__ == "__main__":
    unittest.main()
