#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Tests for host image selection and acquisition.

These cover the decisions the harness makes before a VM exists: which image to
boot, where to get it, and which paths the agent will use on it. Getting any of
them wrong produces a run that fails somewhere else entirely, usually as a guest
that will not boot or an assertion against a path nothing ever wrote to.
"""
import hashlib
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import e2e


def clear_image_pin(test: unittest.TestCase) -> None:
    """Resolve from the manifest regardless of the environment. CI exports a
    pinned build for the job, which these tests must not inherit."""
    for name in ("ACL_IMAGE_URL", "ACL_IMAGE_SHA256", "ACL_IMAGE_BUILD_ID"):
        patcher = patch.object(e2e, name, "")
        patcher.start()
        test.addCleanup(patcher.stop)


class TestHostImageSelection(unittest.TestCase):
    """The per-OS differences that the rest of the harness reads."""

    def setUp(self):
        # The manifest lookup is cached so a real run fetches it once. These
        # feed it different manifests, so each starts from a clear cache.
        e2e.acl_image_from_manifest.cache_clear()
        clear_image_pin(self)

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
                    unresolved = e2e.host_image()
                    image = e2e.resolved_host_image()

        self.assertEqual(image.auth, "azure-storage")
        self.assertEqual(image.sha256, TestACLImageResolution.MANIFEST["qcow2"]["sha256"])
        self.assertEqual(image.url, TestACLImageResolution.MANIFEST["qcow2"]["url"])

        # The unresolved form names no blob. Resolving it reads the published
        # manifest, and host_image is called for the ssh user and provisioning far
        # more often than for the image, including at import, so it must not
        # drag a network call along with it.
        self.assertEqual(unresolved.url, "")
        self.assertEqual(unresolved.provisioning, "ignition")

    def test_a_local_image_needs_no_credentials(self):
        """HOST_IMAGE_PATH is the developer path and must not require an Azure
        login to boot a file already on disk."""
        with patch.dict(os.environ, {"HOST_IMAGE_PATH": __file__}):
            with patch.object(e2e, "HOST_BASE_OS", "acl"):
                image = e2e.host_image()

        self.assertEqual(image.auth, "")
        self.assertEqual(image.sha256, "", "a local file has no published digest to check")
        self.assertTrue(image.url.startswith("file://"))

class TestACLImageResolution(unittest.TestCase):
    """Resolving the image from the published manifest."""

    def setUp(self):
        e2e.acl_image_from_manifest.cache_clear()
        clear_image_pin(self)

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
        with patch.object(e2e, "http_get", return_value=json.dumps(self.MANIFEST)) as get:
            url, file_name, digest = e2e.acl_image_from_manifest()

        self.assertEqual(url, self.MANIFEST["qcow2"]["url"])
        self.assertEqual(file_name, "acl-2026091817.qcow2")
        self.assertEqual(digest, self.MANIFEST["qcow2"]["sha256"])
        # The account disables anonymous access and shared keys alike.
        self.assertEqual(get.call_args.kwargs.get("auth"), "azure-storage")

    def test_build_override_must_match_the_manifest(self):
        """On its own the build id checks the manifest rather than pinning it.
        Silently ignoring it when the manifest has moved on would leave the run
        on a build it was not expecting."""
        with patch.object(e2e, "http_get", return_value=json.dumps(self.MANIFEST)):
            with patch.object(e2e, "ACL_IMAGE_BUILD_ID", "2026010101"):
                with self.assertRaises(SystemExit):
                    e2e.acl_image_from_manifest()

            with patch.object(e2e, "ACL_IMAGE_BUILD_ID", "2026091817"):
                _url, file_name, _digest = e2e.acl_image_from_manifest()

        self.assertEqual(file_name, "acl-2026091817.qcow2")

    def test_a_malformed_manifest_is_refused(self):
        """The build names the cached file, so an empty or odd one could make
        different builds share a name, or a path. And an unverified image boots,
        so whatever goes wrong afterwards looks like a product bug."""
        manifests = [dict(self.MANIFEST, build_id=build) for build in ("", None, 2026, "../x", "a b")]
        manifests += [dict(self.MANIFEST, qcow2=qcow2)
                      for qcow2 in ({}, {"url": "https://example.test/a.qcow2"}, {"sha256": "abc"})]
        for manifest in manifests:
            with self.subTest(manifest=manifest):
                e2e.acl_image_from_manifest.cache_clear()
                with patch.object(e2e, "http_get", return_value=json.dumps(manifest)):
                    with self.assertRaises(SystemExit):
                        e2e.acl_image_from_manifest()

    def test_a_partial_pin_is_refused(self):
        for url, digest, build in (("u", "", "b"), ("", "d", "b"), ("u", "d", "")):
            with self.subTest(url=url, digest=digest, build=build):
                e2e.acl_image_from_manifest.cache_clear()
                with patch.object(e2e, "ACL_IMAGE_URL", url), \
                        patch.object(e2e, "ACL_IMAGE_SHA256", digest), \
                        patch.object(e2e, "ACL_IMAGE_BUILD_ID", build), \
                        patch.object(e2e, "http_get", return_value=json.dumps(self.MANIFEST)):
                    with self.assertRaises(SystemExit):
                        e2e.acl_image_from_manifest()

    def test_resolve_host_image_exports_a_pin_that_reads_back(self):
        """What resolve-host-image writes for later steps has to resolve to the
        same image without reading the manifest."""
        with tempfile.TemporaryDirectory() as tmp:
            env_file, output_file = Path(tmp) / "env", Path(tmp) / "output"
            with patch.object(e2e, "HOST_BASE_OS", "acl"), \
                    patch.object(e2e, "http_get", return_value=json.dumps(self.MANIFEST)), \
                    patch.dict(os.environ, {"GITHUB_ENV": str(env_file), "GITHUB_OUTPUT": str(output_file)}):
                e2e.resolve_host_image()

            exported = dict(line.split("=", 1) for line in env_file.read_text().splitlines())
            self.assertEqual(output_file.read_text(), "build=2026091817\n")

        e2e.acl_image_from_manifest.cache_clear()
        with patch.object(e2e, "ACL_IMAGE_URL", exported["ACL_IMAGE_URL"]), \
                patch.object(e2e, "ACL_IMAGE_SHA256", exported["ACL_IMAGE_SHA256"]), \
                patch.object(e2e, "ACL_IMAGE_BUILD_ID", exported["ACL_IMAGE_BUILD_ID"]), \
                patch.object(e2e, "http_get") as get:
            self.assertEqual(e2e.acl_image_from_manifest(), (
                self.MANIFEST["qcow2"]["url"], "acl-2026091817.qcow2", self.MANIFEST["qcow2"]["sha256"]))
        get.assert_not_called()

class TestAcquireHostImage(unittest.TestCase):
    """Only a verified image is kept under the name later runs trust."""

    GOOD = b"the image"

    def _image(self):
        return e2e.HostImage(url="https://example.test/acl.qcow2", file_name="acl-b.qcow2",
                             backing_format="qcow2", sudo_group="sudo", packages=[],
                             ssh_user="core", provisioning="ignition",
                             sha256=hashlib.sha256(self.GOOD).hexdigest(), auth="")

    def _acquire(self, tmp, download_writes):
        downloads = []

        def download(url, destination, auth=""):
            downloads.append(destination)
            destination.write_bytes(download_writes)

        with patch.object(e2e, "VM_DIR", Path(tmp)), \
                patch.object(e2e, "download_file", side_effect=download), \
                patch.object(e2e, "run"):
            e2e.acquire_host_image(self._image())
        return downloads

    def test_an_existing_image_is_reused(self):
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / "acl-b.qcow2").write_bytes(self.GOOD)
            self.assertEqual(self._acquire(tmp, self.GOOD), [])

    def test_a_download_is_renamed_only_once_verified(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(SystemExit):
                downloads = self._acquire(tmp, b"wrong bytes")
            self.assertEqual(sorted(p.name for p in Path(tmp).iterdir()), [],
                             "neither the partial file nor the trusted name may be left behind")

            downloads = self._acquire(tmp, self.GOOD)
            self.assertEqual([p.name for p in downloads], ["acl-b.qcow2.part"])
            self.assertEqual((Path(tmp) / "acl-b.qcow2").read_bytes(), self.GOOD)
            self.assertFalse((Path(tmp) / "acl-b.qcow2.part").exists())


class TestCurlAuthConfig(unittest.TestCase):
    """The storage token must not reach curl's arguments, which a failed
    command prints."""

    def test_the_token_goes_to_stdin_and_is_masked(self):
        calls = []

        def fake_run(args, **kw):
            calls.append((args, kw))

        with patch.object(e2e, "capture", return_value="secret-token"), \
                patch.object(e2e, "run", side_effect=fake_run), \
                patch.dict(os.environ, {"GITHUB_ACTIONS": "true"}), \
                patch("builtins.print") as printed:
            e2e.download_file("https://example.test/x", Path("/tmp/x"), auth="azure-storage")

        args, kw = calls[0]
        self.assertNotIn("secret-token", " ".join(args))
        self.assertIn("--config", args)
        self.assertIn('header = "Authorization: Bearer secret-token"', kw["input"])
        printed.assert_any_call("::add-mask::secret-token", flush=True)


if __name__ == "__main__":
    unittest.main()
