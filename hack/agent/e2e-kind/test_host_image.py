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
import subprocess
import tempfile
import unittest
from dataclasses import replace
from pathlib import Path
from unittest.mock import patch

import e2e


def clear_image_pin(test: unittest.TestCase, source: str = "manifest") -> None:
    """Resolve from the given source regardless of the environment. CI exports
    the source and a pinned build or version for the job, which these tests
    must not inherit, and the lookups are cached for a real run's sake."""
    for name in ("ACL_IMAGE_URL", "ACL_IMAGE_SHA256", "ACL_IMAGE_BUILD_ID", "ACL_IMAGE_VERSION"):
        patcher = patch.object(e2e, name, "")
        patcher.start()
        test.addCleanup(patcher.stop)
    patcher = patch.object(e2e, "ACL_IMAGE_SOURCE", source)
    patcher.start()
    test.addCleanup(patcher.stop)
    e2e.acl_image_from_manifest.cache_clear()
    e2e.acl_gallery_version.cache_clear()
    test.addCleanup(e2e.acl_gallery_version.cache_clear)


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
            "url": "https://example.blob.core.windows.net/images/2026091817/acl.qcow2",
            "sha256": "7c45558dac005626c06d40594567964739fc96eefab22fdb62e7275191231f45",
            "size": 661192704,
        },
    }

    def test_file_name_is_derived_from_the_build(self):
        """A refreshed image must not be masked by an earlier run's file.

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
        """The build names the image file, so an empty or odd one could make
        different builds share a name, or a path. And an unverified image boots,
        so whatever goes wrong afterward looks like a product bug."""
        manifests = [dict(self.MANIFEST, build_id=build) for build in ("", None, 2026, "../x", "a b")]
        manifests += [dict(self.MANIFEST, qcow2=qcow2)
                      for qcow2 in ({}, {"url": "https://example.test/a.qcow2"}, {"sha256": "abc"})]
        for manifest in manifests:
            with self.subTest(manifest=manifest):
                e2e.acl_image_from_manifest.cache_clear()
                with patch.object(e2e, "http_get", return_value=json.dumps(manifest)):
                    with self.assertRaises(SystemExit):
                        e2e.acl_image_from_manifest()

    def test_image_values_must_be_safe_to_export_and_to_send_the_token_to(self):
        """The image URL gets the storage token, and resolve-host-image writes
        it and the digest to $GITHUB_ENV, one variable per line."""
        good_url, good_digest = self.MANIFEST["qcow2"]["url"], self.MANIFEST["qcow2"]["sha256"]
        bad = [
            ("http://example.blob.core.windows.net/a.qcow2", good_digest),
            ("https://example.test/a.qcow2", good_digest),
            ("https://example.blob.core.windows.net.example.test/a.qcow2", good_digest),
            ("https://example.blob.core.windows.net/a.qcow2\nGITHUB_PATH=/tmp", good_digest),
            ("file:///tmp/a.qcow2", good_digest),
            (good_url, "abc"),
            (good_url, good_digest + "\nX=1"),
            (good_url, "g" * 64),
        ]
        for url, digest in bad:
            manifest = dict(self.MANIFEST, qcow2={"url": url, "sha256": digest})
            with self.subTest(source="manifest", url=url, digest=digest):
                e2e.acl_image_from_manifest.cache_clear()
                with patch.object(e2e, "http_get", return_value=json.dumps(manifest)):
                    with self.assertRaises(SystemExit):
                        e2e.acl_image_from_manifest()
            with self.subTest(source="pin", url=url, digest=digest):
                e2e.acl_image_from_manifest.cache_clear()
                with patch.object(e2e, "ACL_IMAGE_URL", url), \
                        patch.object(e2e, "ACL_IMAGE_SHA256", digest), \
                        patch.object(e2e, "ACL_IMAGE_BUILD_ID", "2026091817"), \
                        patch.object(e2e, "http_get") as get:
                    with self.assertRaises(SystemExit):
                        e2e.acl_image_from_manifest()
                get.assert_not_called()

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
            # No step output: CI does not cache the image, so nothing is keyed
            # by its build.
            self.assertFalse(output_file.exists())

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

    def test_an_existing_image_that_does_not_match_is_downloaded_again(self):
        """A build republished under the same id must not keep booting the
        earlier image."""
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / "acl-b.qcow2").write_bytes(b"an earlier image")
            downloads = self._acquire(tmp, self.GOOD)
            self.assertEqual([p.name for p in downloads], ["acl-b.qcow2.part"])
            self.assertEqual((Path(tmp) / "acl-b.qcow2").read_bytes(), self.GOOD)

    def test_a_local_image_with_a_digest_is_checked(self):
        with tempfile.TemporaryDirectory() as tmp:
            source = Path(tmp) / "source.qcow2"
            source.write_bytes(b"something else")
            image = replace(self._image(), url=f"file://{source}")
            with patch.object(e2e, "VM_DIR", Path(tmp) / "vm"), patch.object(e2e, "run"):
                (Path(tmp) / "vm").mkdir()
                with self.assertRaises(SystemExit):
                    e2e.acquire_host_image(image)
            self.assertTrue(source.exists(), "only the link to a local image is removed")

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


class TestACLImageSource(unittest.TestCase):
    """Which source an Azure Container Linux run boots from."""

    def setUp(self):
        clear_image_pin(self, "gallery")

    def test_the_gallery_image_is_exported_rather_than_downloaded(self):
        """It names a gallery version and no URL, so acquire_host_image exports
        it, and it carries no storage token, which the export does not use."""
        with patch.dict(os.environ, {"HOST_IMAGE_PATH": ""}), \
                patch.object(e2e, "HOST_BASE_OS", "acl"), \
                patch.object(e2e, "ACL_IMAGE_VERSION", "3.20261007.1021"):
            image = e2e.resolved_host_image()

        self.assertEqual(image.gallery, f"{e2e.ACL_IMAGE_GALLERY_IMAGE}/Versions/3.20261007.1021")
        self.assertEqual(image.file_name, "acl-gallery-3.20261007.1021.qcow2")
        self.assertEqual((image.url, image.auth, image.sha256), ("", "", ""))

    def test_a_local_image_wins_over_the_gallery(self):
        with patch.dict(os.environ, {"HOST_IMAGE_PATH": __file__}), \
                patch.object(e2e, "HOST_BASE_OS", "acl"), \
                patch.object(e2e, "capture", side_effect=AssertionError("no az call")):
            image = e2e.resolved_host_image()

        self.assertTrue(image.url.startswith("file://"))
        self.assertEqual(image.gallery, "")

    def test_settings_for_the_other_source_are_refused(self):
        """Ignoring them would boot an image other than the one asked for."""
        cases = [("gallery", "ACL_IMAGE_URL"), ("gallery", "ACL_IMAGE_SHA256"),
                 ("gallery", "ACL_IMAGE_BUILD_ID"), ("manifest", "ACL_IMAGE_VERSION")]
        for source, setting in cases:
            with self.subTest(source=source, setting=setting):
                with patch.object(e2e, "ACL_IMAGE_SOURCE", source), patch.object(e2e, setting, "x"):
                    with self.assertRaises(SystemExit):
                        e2e.acl_image_source()

        with patch.object(e2e, "ACL_IMAGE_SOURCE", "blob"):
            with self.assertRaises(SystemExit):
                e2e.acl_image_source()


class TestGalleryVersion(unittest.TestCase):
    """Naming the gallery version, which names the cached image."""

    def setUp(self):
        clear_image_pin(self, "gallery")

    def test_latest_is_resolved_where_the_image_is_replicated(self):
        with patch.object(e2e, "ACL_IMAGE_SUBSCRIPTION", "sub"), \
                patch.object(e2e, "capture", return_value="3.20261007.1021") as az:
            self.assertEqual(e2e.acl_gallery_version(), "3.20261007.1021")

        args = az.call_args.args[0]
        self.assertEqual(args[:4], ["az", "sig", "image-version", "show-shared"])
        for flag, value in (("--gallery-unique-name", "b3e01d89-bd55-414f-bbb4-cdfeb2628caa-ACL"),
                            ("--gallery-image-definition", "acl-1es-eval"),
                            ("--gallery-image-version", "latest"),
                            ("--location", e2e.ACL_IMAGE_GALLERY_LOCATION),
                            ("--subscription", "sub")):
            self.assertEqual(args[args.index(flag) + 1], value)

    def test_a_pinned_version_is_not_looked_up(self):
        with patch.object(e2e, "ACL_IMAGE_VERSION", "3.1"), \
                patch.object(e2e, "capture", side_effect=AssertionError("no az call")):
            self.assertEqual(e2e.acl_gallery_version(), "3.1")

    def test_a_version_that_is_not_a_plain_name_is_refused(self):
        """It names the cached file, so it must not hold a path or be empty."""
        for version in ("", "../x", "a b"):
            with self.subTest(version=version):
                e2e.acl_gallery_version.cache_clear()
                with patch.object(e2e, "capture", return_value=version):
                    with self.assertRaises(SystemExit):
                        e2e.acl_gallery_version()

    def test_a_malformed_gallery_image_is_refused(self):
        with patch.object(e2e, "ACL_IMAGE_GALLERY_IMAGE", "/SharedGalleries/g/Images"):
            with self.assertRaises(SystemExit):
                e2e.acl_gallery_image()

    def test_resolve_host_image_exports_a_version_that_reads_back(self):
        """Later steps of the job boot the version resolved once, and the image
        file is named for it. Nothing goes to the step outputs: CI does not
        cache the image, so nothing is keyed by its version."""
        with tempfile.TemporaryDirectory() as tmp:
            env_file, output_file = Path(tmp) / "env", Path(tmp) / "output"
            with patch.object(e2e, "HOST_BASE_OS", "acl"), \
                    patch.object(e2e, "capture", return_value="3.20261007.1021"), \
                    patch.dict(os.environ, {"GITHUB_ENV": str(env_file), "GITHUB_OUTPUT": str(output_file)}):
                e2e.resolve_host_image()

            exported = dict(line.split("=", 1) for line in env_file.read_text().splitlines())
            self.assertFalse(output_file.exists())

        self.assertEqual(exported, {"ACL_IMAGE_SOURCE": "gallery", "ACL_IMAGE_VERSION": "3.20261007.1021"})
        e2e.acl_gallery_version.cache_clear()
        with patch.dict(os.environ, {"HOST_IMAGE_PATH": ""}), \
                patch.object(e2e, "HOST_BASE_OS", "acl"), \
                patch.object(e2e, "ACL_IMAGE_VERSION", exported["ACL_IMAGE_VERSION"]), \
                patch.object(e2e, "capture", side_effect=AssertionError("no az call")):
            self.assertEqual(e2e.resolved_host_image().file_name, "acl-gallery-3.20261007.1021.qcow2")


class FakeAz:
    """Answers the az calls the export makes, and records them. It also
    answers qemu-img info, which goes through the same capture."""

    SAS = "https://md-x.blob.core.windows.net/abcd/abcd?sv=2018-03-28&sr=b&sig=SECRET%3D"

    def __init__(self, fail: set[str] = frozenset(), disks: list[dict] | None = None,
                 virtual_size: int = 0):
        self.calls: list[list[str]] = []
        self.fail = fail
        self.disks = disks or []
        self.virtual_size = virtual_size

    def verb(self, args: list[str]) -> str:
        return " ".join(args[1:3])

    def __call__(self, args, **_kw):
        if args[:2] == ["qemu-img", "info"]:
            return json.dumps({"virtual-size": self.virtual_size})
        self.calls.append(args)
        verb = self.verb(args)
        if verb in self.fail:
            raise e2e.subprocess.CalledProcessError(1, args, "", f"{verb} refused")
        return {
            "disk list": json.dumps(self.disks),
            "disk grant-access": json.dumps({"accessSas": self.SAS}),
        }.get(verb, "")

    def verbs(self) -> list[str]:
        return [self.verb(args) for args in self.calls]


class TestGalleryExport(unittest.TestCase):
    """The temporary disk always goes, and only a converted image is kept."""

    DISK = b"\x01" * 2048

    def _export(self, tmp, az, download=None, rg="rg"):
        destination = Path(tmp) / "acl-gallery-3.1.qcow2.part"
        downloads = []
        self.converted = []

        def fake_download(sas, vhd, fetch=None):
            downloads.append(sas)
            if download:
                download()
            vhd.write_bytes(_fixed_vhd(self.DISK))

        def fake_run(args, **_kw):
            if args[:2] == ["qemu-img", "convert"]:
                self.converted.append((args[args.index("-f") + 1], Path(args[-2]).read_bytes()))
                Path(args[-1]).write_bytes(b"qcow2")

        az.virtual_size = az.virtual_size or len(self.DISK)
        with patch.object(e2e, "ACL_IMAGE_RESOURCE_GROUP", rg), \
                patch.object(e2e, "ACL_IMAGE_SUBSCRIPTION", ""), \
                patch.object(e2e, "capture", side_effect=az), \
                patch.object(e2e, "download_page_blob", side_effect=fake_download), \
                patch.object(e2e, "run", side_effect=fake_run):
            e2e.export_gallery_image("/SharedGalleries/g/Images/i/Versions/3.1", destination)
        return destination, downloads

    def test_the_raw_disk_is_converted_at_its_exact_size(self):
        """The footer is cut off rather than read: qemu-img before 10.0 sizes
        this VHD from its CHS geometry, 640 KiB short, and the guest then finds
        no GPT. A developer's host may still have one."""
        with tempfile.TemporaryDirectory() as tmp:
            self._export(tmp, FakeAz())

        self.assertEqual(self.converted, [("raw", self.DISK)])

    def test_a_qcow2_of_another_size_is_refused(self):
        az = FakeAz(virtual_size=len(self.DISK) - 512)
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(SystemExit):
                self._export(tmp, az)
            self.assertEqual(list(Path(tmp).iterdir()), [], "nothing is kept under any name")

    def test_the_disk_is_revoked_and_deleted_after_a_good_export(self):
        az = FakeAz()
        with tempfile.TemporaryDirectory() as tmp:
            destination, downloads = self._export(tmp, az)
            self.assertEqual(sorted(p.name for p in Path(tmp).iterdir()), [destination.name],
                             "only the converted image is kept")

        self.assertEqual(az.verbs(), ["disk list", "disk create", "disk grant-access",
                                      "disk revoke-access", "disk delete"])
        self.assertEqual(downloads, [FakeAz.SAS])
        create = az.calls[1]
        self.assertEqual(create[create.index("--gallery-image-reference") + 1],
                         "/SharedGalleries/g/Images/i/Versions/3.1")
        self.assertIn(f"purpose={e2e.ACL_EXPORT_DISK_TAG}", create)
        disk = create[create.index("-n") + 1]
        for call in az.calls[3:]:
            self.assertEqual(call[call.index("-n") + 1], disk)

    def test_a_failed_download_still_revokes_and_deletes(self):
        def fail():
            raise RuntimeError("blob storage answered HTTP 403 Forbidden")

        az = FakeAz()
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(SystemExit):
                self._export(tmp, az, download=fail)
            self.assertEqual(list(Path(tmp).iterdir()), [], "neither the VHD nor a partial image is left")

        self.assertEqual(az.verbs()[-2:], ["disk revoke-access", "disk delete"])

    def test_a_failed_cleanup_does_not_fail_the_export(self):
        """The image is good; the next export's sweep removes the disk."""
        az = FakeAz(fail={"disk revoke-access", "disk delete"})
        with tempfile.TemporaryDirectory() as tmp:
            destination, _ = self._export(tmp, az)
            self.assertEqual(destination.read_bytes(), b"qcow2")

    def test_the_resource_group_is_required(self):
        az = FakeAz()
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(SystemExit):
                self._export(tmp, az, rg="")

        self.assertEqual(az.calls, [])

    def test_the_sas_is_masked_and_on_no_command_line(self):
        """A failed command prints its arguments, and GitHub prints whatever
        is not masked."""
        az = FakeAz()
        with tempfile.TemporaryDirectory() as tmp, \
                patch.dict(os.environ, {"GITHUB_ACTIONS": "true"}), \
                patch("builtins.print") as printed:
            self._export(tmp, az)

        self.assertFalse([args for args in az.calls if any("SECRET" in arg for arg in args)])
        printed.assert_any_call(f"::add-mask::{FakeAz.SAS}", flush=True)
        # The signature on its own, decoded as parse_qs decodes it.
        printed.assert_any_call("::add-mask::SECRET=", flush=True)

    def test_the_sas_is_short_lived(self):
        """It is revoked when the download ends; the duration only bounds one
        a dead job never revoked."""
        az = FakeAz()
        with tempfile.TemporaryDirectory() as tmp:
            self._export(tmp, az)

        grant = next(args for args in az.calls if az.verb(args) == "disk grant-access")
        self.assertLessEqual(int(grant[grant.index("--duration-in-seconds") + 1]), 900)


class TestExportSweep(unittest.TestCase):
    """Canceled jobs leave disks behind. Only old ones carrying the export's
    tag are deleted, so a running export elsewhere is never touched."""

    NOW = 1_800_000_000.0

    def _sweep(self, az):
        with patch.object(e2e, "ACL_IMAGE_RESOURCE_GROUP", "rg"), \
                patch.object(e2e, "ACL_IMAGE_SUBSCRIPTION", ""), \
                patch.object(e2e, "capture", side_effect=az):
            e2e.sweep_stale_export_disks(now=self.NOW)

    @staticmethod
    def _iso(seconds_ago):
        stamp = e2e.datetime.datetime.fromtimestamp(TestExportSweep.NOW - seconds_ago, e2e.datetime.timezone.utc)
        # ARM reports seven fractional digits.
        return stamp.strftime("%Y-%m-%dT%H:%M:%S.1234567+00:00")

    def test_only_stale_disks_are_deleted(self):
        az = FakeAz(disks=[
            {"name": "old-sas", "created": self._iso(7 * 3600), "state": "ActiveSAS"},
            {"name": "old", "created": self._iso(7 * 3600), "state": "Unattached"},
            {"name": "running", "created": self._iso(600), "state": "ActiveSAS"},
        ])
        self._sweep(az)

        query = az.calls[0][az.calls[0].index("--query") + 1]
        self.assertIn(f"tags.purpose=='{e2e.ACL_EXPORT_DISK_TAG}'", query)
        touched = [(az.verb(args), args[args.index("-n") + 1]) for args in az.calls[1:]]
        self.assertEqual(touched, [("disk revoke-access", "old-sas"), ("disk delete", "old-sas"),
                                   ("disk delete", "old")])

    def test_a_failed_listing_is_not_fatal(self):
        az = FakeAz(fail={"disk list"})
        self._sweep(az)
        self.assertEqual(az.verbs(), ["disk list"])

    def test_a_disk_of_unknown_age_is_left_alone(self):
        """It could be a running export's, and the sweep must not fail the
        export it runs before."""
        az = FakeAz(disks=[
            {"name": "no-time", "state": "Unattached"},
            {"name": "null-time", "created": None, "state": "Unattached"},
            {"name": "odd-time", "created": "yesterday", "state": "Unattached"},
            {"name": "old", "created": self._iso(7 * 3600), "state": "Unattached"},
        ])
        self._sweep(az)

        touched = [(az.verb(args), args[args.index("-n") + 1]) for args in az.calls[1:]]
        self.assertEqual(touched, [("disk delete", "old")])

    def test_az_output_cannot_start_a_workflow_command(self):
        """az's stderr is not ours; a line break in it must not reach the log
        as a command of its own."""
        error = subprocess.CalledProcessError(1, ["az"], stderr="failed\n::add-mask::x\r\n100%")
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "true"}), \
                patch.object(e2e, "ACL_IMAGE_SUBSCRIPTION", ""), \
                patch.object(e2e, "capture", side_effect=error), \
                patch("builtins.print") as printed:
            self.assertIsNone(e2e._acl_az_best_effort(["disk", "list"], "list disks"))

        lines = [call.args[0] for call in printed.call_args_list]
        for line in lines:
            self.assertNotIn("\n", line)
            self.assertNotIn("\r", line)
        self.assertTrue(any(line.startswith("::warning::") and "%0A::add-mask::x" in line for line in lines))


def _fixed_vhd(data: bytes) -> bytes:
    footer = bytearray(e2e.VHD_FOOTER_SIZE)
    footer[:8] = b"conectix"
    footer[48:56] = len(data).to_bytes(8, "big")
    footer[60:64] = (2).to_bytes(4, "big")
    return data + bytes(footer)


class TestPageBlobDownload(unittest.TestCase):
    """Only the written pages of a disk are fetched, and they land where the
    disk has them."""

    def test_page_ranges_are_split_into_bounded_chunks(self):
        chunks = e2e.page_chunks([(0, 511), (1024, 1024 + 2500 - 1)], chunk=1000)
        self.assertEqual(chunks, [(0, 511), (1024, 2023), (2024, 3023), (3024, 3523)])

    def _serve(self, disk: bytes, ranges: list[tuple[int, int]]):
        """Serve a page blob in two page-list pages, as storage does for a
        disk with many ranges."""
        requests = []

        def fetch(url, headers):
            requests.append((url, headers))
            meta = {"x-ms-blob-content-length": str(len(disk))}
            if "comp=pagelist" in url:
                page = ranges[1:] if "marker=" in url else ranges[:1]
                body = "<PageList>" + "".join(
                    f"<PageRange><Start>{s}</Start><End>{e}</End></PageRange>" for s, e in page)
                body += "" if "marker=" in url else "<NextMarker>next page</NextMarker>"
                return (body + "</PageList>").encode(), meta
            start, stop = map(int, headers["Range"].removeprefix("bytes=").split("-"))
            return disk[start:stop + 1], meta

        return fetch, requests

    def test_written_pages_land_at_their_offsets(self):
        data = bytearray(8192)
        data[0:512] = b"\x01" * 512
        data[4096:4608] = b"\x02" * 512
        disk = _fixed_vhd(bytes(data))
        # The middle range is listed but holds zeros, and the footer is a page.
        ranges = [(0, 511), (2048, 2559), (4096, 4607), (len(disk) - 512, len(disk) - 1)]
        fetch, requests = self._serve(disk, ranges)

        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "disk.vhd"
            e2e.download_page_blob("https://sas?sig=x", target, fetch=fetch)
            self.assertEqual(target.read_bytes(), disk)

        self.assertTrue(any("marker=next%20page" in url for url, _ in requests), "the second page is listed")

    def test_a_disk_that_is_not_a_fixed_vhd_is_refused(self):
        disk = bytes(4096)
        fetch, _ = self._serve(disk, [(0, 511), (len(disk) - 512, len(disk) - 1)])
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaisesRegex(RuntimeError, "not a fixed VHD"):
                e2e.download_page_blob("https://sas?sig=x", Path(tmp) / "disk.vhd", fetch=fetch)

    def test_errors_do_not_name_the_url(self):
        """The URL is the SAS."""
        url = FakeAz.SAS
        denied = e2e.urllib.error.HTTPError(url, 403, "Forbidden", {}, None)
        with patch.object(e2e.urllib.request, "urlopen", side_effect=denied):
            with self.assertRaises(RuntimeError) as raised:
                e2e._blob_request(url, {})
        self.assertNotIn("SECRET", str(raised.exception))
        self.assertIsNone(raised.exception.__cause__)

    def test_transient_failures_are_retried(self):
        class Response:
            headers = {"X-Ms-Blob-Content-Length": "3"}

            def read(self):
                return b"abc"

            def __enter__(self):
                return self

            def __exit__(self, *_):
                return False

        busy = e2e.urllib.error.HTTPError("https://example.test/u", 503, "Server Busy", {}, None)
        with patch.object(e2e.urllib.request, "urlopen", side_effect=[busy, OSError("reset"), Response()]), \
                patch.object(e2e.time, "sleep"):
            body, headers = e2e._blob_request("https://example.test/u", {})
        self.assertEqual((body, headers["x-ms-blob-content-length"]), (b"abc", "3"))


if __name__ == "__main__":
    unittest.main()
