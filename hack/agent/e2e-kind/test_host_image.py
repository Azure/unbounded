#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Tests for host image selection and acquisition.

These cover the decisions the harness makes before a VM exists: which image to
boot, where to get it, and which paths the agent will use on it. Getting any of
them wrong produces a run that fails somewhere else entirely, usually as a guest
that will not boot or an assertion against a path nothing ever wrote to.
"""
import json
import os
import unittest
from pathlib import Path
from unittest.mock import patch

import e2e


class TestHostImageSelection(unittest.TestCase):
    """The per-OS differences that the rest of the harness reads."""

    def setUp(self):
        # The manifest lookup is cached so a real run fetches it once. These
        # feed it different manifests, so each starts from a clear cache.
        e2e.acl_image_from_manifest.cache_clear()

    def test_conventional_hosts_use_cloud_init_and_the_default_prefix(self):
        """Every pre-existing host must keep the behavior it had.

        The prefix and provisioning fields were added for one image. If they
        changed the answer for any other, the change would show up as a
        different install location on hosts that were working.
        """
        for base_os in ("ubuntu2404", "ubuntu2604", "fedora", "almalinux9",
                        "almalinux10", "centosstream9", "centosstream10"):
            with self.subTest(base_os=base_os):
                with patch.object(e2e, "HOST_BASE_OS", base_os):
                    image = e2e.host_image()

                self.assertEqual(image.provisioning, "cloud-init")
                self.assertEqual(image.host_prefix, "")
                self.assertEqual(image.ssh_user, "ubuntu")
                self.assertEqual(image.auth, "")
                self.assertTrue(image.packages, "a host with a package manager installs prerequisites")

    def test_acl_declares_an_immutable_host(self):
        """The four properties that make ACL different, asserted together.

        They are not independent. Ignition provisioning is why there is no
        package installation step, no package installation is why the image has
        to carry the tools, and a read-only /usr is why the prefix moves. A
        change to any one of them without the others describes a host that does
        not exist.
        """
        with patch.dict(os.environ, {"HOST_IMAGE_PATH": __file__}):
            with patch.object(e2e, "HOST_BASE_OS", "acl"):
                image = e2e.host_image()

        self.assertEqual(image.provisioning, "ignition")
        self.assertEqual(image.ssh_user, "core")
        self.assertEqual(image.host_prefix, "/opt/unbounded")
        self.assertEqual(image.packages, [])

    def test_acl_from_the_manifest_carries_download_credentials(self):
        """The blob needs a token too, not just the manifest.

        These are two separate requests, and only the manifest fetch names its
        credential inline. The image download reads it off the HostImage, so an
        image resolved from the manifest has to carry one or the download gets a
        401 after the manifest has already succeeded.
        """
        manifest = json.dumps(TestACLImageResolution.MANIFEST)

        with patch.dict(os.environ, {"HOST_IMAGE_PATH": ""}):
            with patch.object(e2e, "HOST_BASE_OS", "acl"):
                with patch.object(e2e, "http_get", return_value=manifest):
                        image = e2e.host_image()

        self.assertEqual(image.auth, "azure-storage")
        self.assertEqual(image.sha256, TestACLImageResolution.MANIFEST["qcow2"]["sha256"])

    def test_a_local_image_needs_no_credentials(self):
        """HOST_IMAGE_PATH is the developer path and must not require an Azure
        login to boot a file already on disk."""
        with patch.dict(os.environ, {"HOST_IMAGE_PATH": __file__}):
            with patch.object(e2e, "HOST_BASE_OS", "acl"):
                image = e2e.host_image()

        self.assertEqual(image.auth, "")
        self.assertEqual(image.sha256, "", "a local file has no published digest to check")
        self.assertTrue(image.url.startswith("file://"))

    def test_unsupported_host_names_the_supported_ones(self):
        with patch.object(e2e, "HOST_BASE_OS", "windows"):
            with self.assertRaises(SystemExit):
                e2e.host_image()



class TestACLImageResolution(unittest.TestCase):
    """Resolving the image from the published manifest."""

    def setUp(self):
        e2e.acl_image_from_manifest.cache_clear()

    MANIFEST = {
        "build_id": "2026091817",
        "qcow2": {
            "url": "https://example.test/images/2026091817/acl.qcow2",
            "sha256": "7c45558dac005626c06d40594567964739fc96eefab22fdb62e7275191231f45",
            "size": 661192704,
        },
    }

    def test_file_name_is_derived_from_the_build(self):
        """A refreshed image must not be masked by a cached file.

        The harness reuses an image already in VM_DIR rather than downloading
        again. If every build landed under the same name, a machine that had run
        the suite before would silently keep booting the old one.
        """
        with patch.object(e2e, "http_get", return_value=json.dumps(self.MANIFEST)):
            url, file_name, digest = e2e.acl_image_from_manifest()

        self.assertEqual(url, self.MANIFEST["qcow2"]["url"])
        self.assertEqual(file_name, "acl-2026091817.qcow2")
        self.assertEqual(digest, self.MANIFEST["qcow2"]["sha256"])

    def test_manifest_is_read_with_storage_credentials(self):
        """The account disables anonymous access and shared keys alike, so the
        manifest is unreadable without a bearer token."""
        with patch.object(e2e, "http_get", return_value=json.dumps(self.MANIFEST)) as get:
            e2e.acl_image_from_manifest()

        get.assert_called_once()
        self.assertEqual(get.call_args.kwargs.get("auth"), "azure-storage")

    def test_build_override_must_match_the_manifest(self):
        """The override exists to pin a known-good build when a new one breaks
        the suite. Silently ignoring it when the manifest has moved on would
        leave the run on exactly the build it was trying to avoid."""
        with patch.object(e2e, "http_get", return_value=json.dumps(self.MANIFEST)):
            with patch.object(e2e, "ACL_IMAGE_BUILD_ID", "2026010101"):
                with self.assertRaises(SystemExit):
                    e2e.acl_image_from_manifest()

            with patch.object(e2e, "ACL_IMAGE_BUILD_ID", "2026091817"):
                _url, file_name, _digest = e2e.acl_image_from_manifest()

        self.assertEqual(file_name, "acl-2026091817.qcow2")

    def test_manifest_without_a_digest_is_refused(self):
        """An unverified image is the one thing worse than no image: it boots,
        and whatever goes wrong afterwards looks like a product bug."""
        for qcow2 in ({}, {"url": "https://example.test/a.qcow2"}, {"sha256": "abc"}):
            with self.subTest(qcow2=qcow2):
                manifest = json.dumps({"build_id": "b", "qcow2": qcow2})
                with patch.object(e2e, "http_get", return_value=manifest):
                    with self.assertRaises(SystemExit):
                        e2e.acl_image_from_manifest()


class TestVerifySHA256(unittest.TestCase):
    def test_mismatch_removes_the_file_and_fails(self):
        """The file is deleted so the next run downloads again rather than
        reusing a bad image that is now sitting under the expected name."""
        import tempfile

        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "image.qcow2"
            target.write_bytes(b"not the image")

            with self.assertRaises(SystemExit):
                e2e.verify_sha256(target, "0" * 64)

            self.assertFalse(target.exists(), "a corrupt download must not be left in place")

    def test_matching_digest_keeps_the_file(self):
        import hashlib
        import tempfile

        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "image.qcow2"
            target.write_bytes(b"contents")

            e2e.verify_sha256(target, hashlib.sha256(b"contents").hexdigest())

            self.assertTrue(target.exists())


if __name__ == "__main__":
    unittest.main()
