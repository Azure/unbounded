#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

from copy import deepcopy
from pathlib import Path
import unittest

import verify


class VerifyTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.assets = Path(__file__).resolve().parent
        cls.image_config = verify.load_mapping(cls.assets / "90-hillsboro-users.cfg")
        cls.vendor_template = cls.assets / "vendor-data.tmpl"
        if not cls.vendor_template.exists():
            cls.vendor_template = (
                cls.assets.parents[1] / "netboot" / "assets" / "vendor-data.tmpl"
            )

    def test_reviewed_policy_and_real_cloud_init_merge(self):
        verify.validate_users(self.image_config)
        verify.verify_merge(self.image_config, self.vendor_template)

    def test_bcho_must_remain_keyless(self):
        config = deepcopy(self.image_config)
        config["users"][0]["ssh_import_id"] = ["gh:bcho"]

        with self.assertRaisesRegex(RuntimeError, "bcho must remain keyless"):
            verify.validate_users(config)

    def test_same_named_import_is_required(self):
        config = deepcopy(self.image_config)
        config["users"][1]["ssh_import_id"] = ["gh:different-user"]

        with self.assertRaisesRegex(RuntimeError, "same-named GitHub identity"):
            verify.validate_users(config)


if __name__ == "__main__":
    unittest.main()
