# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import json
import pathlib
import stat
import tempfile
import unittest

import probe


class ProbeTests(unittest.TestCase):
    def test_conformance_targets(self):
        text = ("test topology::physical_owner_proof::b03_go_snapshot_consistency ... "
                "B03_TARGET live-head /first\n"
                "B03_TARGET live-get /second\nok\n")
        self.assertEqual(probe.conformance_targets(text, 2),
                         {"live-head": "/first", "live-get": "/second"})
        with self.assertRaises(AssertionError):
            probe.conformance_targets("running 0 tests\n", 2)
        with self.assertRaises(AssertionError):
            probe.conformance_targets(text + "B03_TARGET live-head /duplicate\n", 2)

    def test_registration_uses_enrollment_and_private_files(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = probe.ControlFixture.__new__(probe.ControlFixture)
            fixture.root = pathlib.Path(directory)
            fixture.url = "https://127.0.0.1:8444"
            config = fixture.root / "config.json"
            env = fixture.register(config, "01" * 32, "02" * 32, "go-export-pod")
            registration = json.loads((fixture.root / ("02" * 32 + ".json")).read_text())
            token = pathlib.Path(env["RACER_CONTROL_TOKEN_FILE"])
            self.assertEqual(token.read_text(), registration["token"])
            self.assertEqual(registration["podUID"], "go-export-pod")
            self.assertEqual(registration["config"], str(config))
            self.assertEqual(env["RACER_POD_UID"], "go-export-pod")
            self.assertEqual(env["RACER_TLS_TRUST_DIR"], directory)
            self.assertEqual(env["RACER_CONTROL_PLANE_URL"],
                             fixture.url + "/v3/" + "01" * 32 + "/" + "02" * 32)
            for path in fixture.root.iterdir():
                self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            with self.assertRaises(FileExistsError):
                fixture.register(config, "01" * 32, "02" * 32, "replacement")


if __name__ == "__main__":
    unittest.main()
