#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

from copy import deepcopy
import hashlib
import importlib.util
import io
from pathlib import Path
import struct
import tarfile
import tempfile
import unittest

import verify


class VerifyTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.assets = Path(__file__).resolve().parent
        cls.image_config = verify.load_mapping(
            cls.assets / "90-gb200-access-users.cfg"
        )
        cls.vendor_template = (
            cls.assets.parents[1] / "netboot" / "assets" / "vendor-data.tmpl"
        )

    def test_reviewed_policy_and_real_cloud_init_merge(self):
        if importlib.util.find_spec("cloudinit") is None:
            self.skipTest("cloud-init Python module is unavailable")
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

    def test_agent_archive_requires_exact_arm64_payload(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory) / "agent.tar.gz"
            self.write_agent_archive(archive)
            digest = hashlib.sha256(archive.read_bytes()).hexdigest()

            verify.validate_agent_archive(archive, digest)

    def test_agent_archive_rejects_wrong_architecture(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory) / "agent.tar.gz"
            self.write_agent_archive(archive, machine=62)
            digest = hashlib.sha256(archive.read_bytes()).hexdigest()

            with self.assertRaisesRegex(RuntimeError, "not ARM64"):
                verify.validate_agent_archive(archive, digest)

    def test_agent_archive_rejects_wrong_checksum(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory) / "agent.tar.gz"
            self.write_agent_archive(archive)

            with self.assertRaisesRegex(RuntimeError, "checksum"):
                verify.validate_agent_archive(archive, "0" * 64)

    def test_agent_archive_rejects_changed_members(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory) / "agent.tar.gz"
            self.write_agent_archive(archive, include_notice=False)
            digest = hashlib.sha256(archive.read_bytes()).hexdigest()

            with self.assertRaisesRegex(RuntimeError, "members changed"):
                verify.validate_agent_archive(archive, digest)

    @staticmethod
    def write_agent_archive(
        path: Path, *, machine: int = verify.ELF_MACHINE_AARCH64, include_notice=True
    ) -> None:
        header = bytearray(20)
        header[:4] = b"\x7fELF"
        header[5] = 1
        struct.pack_into("<H", header, 18, machine)
        files = [("LICENSE", b"license", 0o644)]
        if include_notice:
            files.append(("NOTICE", b"notice", 0o644))
        files.append(("unbounded-agent", bytes(header), 0o755))

        with tarfile.open(path, "w:gz") as archive:
            for name, content, mode in files:
                info = tarfile.TarInfo(name)
                info.mode = mode
                info.size = len(content)
                archive.addfile(info, io.BytesIO(content))


if __name__ == "__main__":
    unittest.main()
