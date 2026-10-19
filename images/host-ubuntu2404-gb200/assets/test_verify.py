#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import json
import os
from pathlib import Path
import struct
import tempfile
import unittest
from unittest.mock import patch

import verify


class RootfsContractTest(unittest.TestCase):
    def setUp(self):
        scratch = Path(__file__).resolve().parents[3] / "tmp"
        scratch.mkdir(exist_ok=True)
        self.temporary = tempfile.TemporaryDirectory(prefix="gb200-rootfs-test-", dir=scratch)
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.kernel = "7.0.0-1019-nvidia-64k"
        self.driver = "580.178.04"
        self.vermagic = f"{self.kernel} SMP preempt mod_unload modversions aarch64"
        self.initrd = f"usr/lib/modules/{self.kernel}/kernel/drivers/net/mana.ko.zst"
        self.license = "Dual MIT/GPL"
        self.signer = "Canonical Ltd. Secure Boot Signing"
        self.module_version = self.driver
        self.packages = "linux-image\t7.0\tarm64\nnvidia-utils-580-server\t580.178.04\tarm64"
        self.put(f"/boot/config-{self.kernel}", "CONFIG_ARM64_64K_PAGES=y\nCONFIG_MICROSOFT_MANA=m\n")
        self.put("/etc/machine-id", "")
        self.put(f"/boot/initrd.img-{self.kernel}", "initramfs fixture")
        self.put("/etc/initramfs-tools/update-initramfs.conf", "update_initramfs=yes\n")
        self.put(
            "/etc/modules-load.d/nvidia.conf",
            "nvidia\nnvidia_modeset\nnvidia_uvm\nnvidia_drm\n",
        )
        self.put(
            "/etc/systemd/system/nvidia-persistence-mode.service",
            "[Unit]\nAfter=systemd-modules-load.service\n"
            "ConditionPathExistsGlob=/sys/bus/pci/drivers/nvidia/0*\n"
            "[Service]\nExecStart=/usr/bin/nvidia-smi -pm 1\n",
        )
        enabled = self.path(
            "/etc/systemd/system/multi-user.target.wants/nvidia-persistence-mode.service"
        )
        enabled.parent.mkdir(parents=True, exist_ok=True)
        enabled.symlink_to("../nvidia-persistence-mode.service")
        self.put("/usr/share/unbounded/efi-packages.tsv", "shim-signed\t1\tarm64\n")
        self.put("/usr/share/unbounded/efi-packages.sha256", "test-package-hash\n")
        self.put("/var/lib/apt/lists/ubuntu_InRelease", "signed release fixture\n")
        for name in ("mana", "mana_ib", "nvidia", "nvidia_uvm"):
            self.put(f"/lib/modules/{self.kernel}/{name}.ko", "module fixture")
        for name in ("BOOTAA64.EFI", "grubaa64.efi", "mmaa64.efi"):
            data = bytearray(256)
            data[:2] = b"MZ"
            struct.pack_into("<I", data, 0x3C, 128)
            data[128:132] = b"PE\0\0"
            struct.pack_into("<H", data, 132, 0xAA64)
            self.put(f"/boot/efi/EFI/BOOT/{name}", bytes(data))
        self.addCleanup(patch.stopall)
        patch.object(verify, "Path", side_effect=self.path).start()
        patch.object(verify, "command", side_effect=self.command).start()
        patch.dict(os.environ, KVER=self.kernel, NVIDIA_DRIVER_VERSION=self.driver,
                   SOURCE_COMMIT="a" * 40).start()

    def path(self, name):
        return self.root / str(name).lstrip("/")

    def put(self, name, value):
        path = self.path(name)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(value if isinstance(value, bytes) else value.encode())

    def command(self, *args):
        if args[0] == "dpkg":
            return "arm64"
        if args[0] == "dpkg-query":
            return self.packages
        if args[0] == "lsinitramfs":
            return self.initrd
        if args[0] == "modinfo":
            field, name = args[4:]
            return {
                "filename": f"/lib/modules/{self.kernel}/{name}.ko",
                "vermagic": self.vermagic,
                "version": self.module_version,
                "license": self.license,
                "signer": self.signer,
            }[field]
        self.fail(f"unexpected command: {args}")

    def rejected(self, message):
        with self.assertRaisesRegex(RuntimeError, message):
            verify.verify()

    def test_valid_rootfs_writes_provenance(self):
        verify.verify()
        result = json.loads(self.path("/usr/share/unbounded/image-inputs.json").read_text())
        self.assertEqual(result["page_size"], 65536)
        self.assertEqual(result["nvidia_driver"], self.driver)
        self.assertEqual(set(result["modules"]), {"mana", "mana_ib", "nvidia", "nvidia_uvm"})
        self.assertTrue(result["apt_inrelease_sha256"])

    def test_target_initramfs_bootstrapped_before_nvidia_postinst(self):
        source = Path(__file__).with_name("rootfs.sh").read_text()
        phases = source.split("apt-get install -y --no-install-recommends")
        self.assertEqual(len(phases), 3)
        self.assertIn('"linux-image-${KVER}=${KERNEL_VERSION}"', phases[1])
        self.assertIn('test -s "/boot/initrd.img-$KVER"', phases[1])
        self.assertNotIn('"nvidia-kernel-common-', phases[1])
        self.assertIn('"nvidia-kernel-common-', phases[2])
        self.assertIn("update_initramfs=no", phases[1])
        self.assertIn("update_initramfs=yes", phases[2])

    def test_boot_uses_platform_virtio_network_device(self):
        source = Path(__file__).resolve().parents[1].joinpath("verify-boot.sh").read_text()
        self.assertIn("-device virtio-net-device,netdev=net0", source)
        self.assertNotIn("-device virtio-net-pci", source)
        self.assertIn("-cpu cortex-a72", source)
        self.assertNotIn("-cpu max", source)

    def test_builder_kernel_initramfs_rejected(self):
        self.put("/boot/initrd.img-builder-kernel", "wrong initramfs")
        self.rejected("unexpected builder-kernel")

    def test_initramfs_updates_must_be_restored(self):
        self.put("/etc/initramfs-tools/update-initramfs.conf", "update_initramfs=no\n")
        self.rejected("not restored")

    def test_wrong_kernel_abi(self):
        self.vermagic = "wrong-kernel SMP aarch64"
        self.rejected("kernel ABI mismatch")

    def test_wrong_module_architecture(self):
        self.vermagic = f"{self.kernel} SMP x86_64"
        self.rejected("not ARM64")

    def test_non_64k_kernel(self):
        self.put(f"/boot/config-{self.kernel}", "CONFIG_MICROSOFT_MANA=m\n")
        self.rejected("CONFIG_ARM64_64K_PAGES")

    def test_missing_mana_configuration(self):
        self.put(f"/boot/config-{self.kernel}", "CONFIG_ARM64_64K_PAGES=y\n")
        self.rejected("CONFIG_MICROSOFT_MANA")

    def test_missing_mana_initramfs(self):
        self.initrd = "usr/lib/modules/nvidia.ko"
        self.rejected("MANA is absent")

    def test_driver_version_mismatch(self):
        self.module_version = "595.91.07"
        self.rejected("driver version mismatch")

    def test_proprietary_driver_rejected(self):
        self.license = "NVIDIA"
        self.rejected("not the open kernel module")

    def test_unsigned_driver_rejected(self):
        self.signer = ""
        self.rejected("lacks a module signature")

    def test_amd64_efi_rejected(self):
        path = self.path("/boot/efi/EFI/BOOT/BOOTAA64.EFI")
        data = bytearray(path.read_bytes())
        struct.pack_into("<H", data, 132, 0x8664)
        path.write_bytes(data)
        self.rejected("not an ARM64 EFI")

    def test_baked_machine_identity_rejected(self):
        self.put("/etc/machine-id", "inherited-id\n")
        self.rejected("baked-in machine ID")

    def test_baked_ssh_key_rejected(self):
        self.put("/etc/ssh/ssh_host_ed25519_key", "inherited-key")
        self.rejected("baked-in SSH key")

    def test_dkms_fallback_rejected(self):
        self.packages += "\nnvidia-dkms-580-server-open\t580\tarm64"
        self.rejected("unexpected DKMS")

    def test_missing_nvidia_utilities_rejected(self):
        self.packages = "linux-image\t7.0\tarm64"
        self.rejected("missing pinned NVIDIA utilities")

    def test_missing_nvidia_boot_module_configuration_rejected(self):
        self.path("/etc/modules-load.d/nvidia.conf").unlink()
        self.rejected("NVIDIA modules are not configured for boot")

    def test_disabled_persistence_service_rejected(self):
        self.path(
            "/etc/systemd/system/multi-user.target.wants/nvidia-persistence-mode.service"
        ).unlink()
        self.rejected("NVIDIA persistence service is not enabled")

    def test_missing_apt_provenance_rejected(self):
        self.path("/var/lib/apt/lists/ubuntu_InRelease").unlink()
        self.rejected("missing signed APT")

    def test_multiple_kernels_rejected(self):
        self.path("/lib/modules/other-kernel").mkdir()
        self.rejected("exactly the pinned kernel")

    def test_distribution_empty_shared_module_directory_accepted(self):
        self.path("/lib/modules/kernel/nvidia-580srv-open").mkdir(parents=True)
        verify.verify()

    def test_unversioned_module_file_rejected(self):
        self.put("/lib/modules/kernel/nvidia-580srv-open/nvidia.ko", "unbound module")
        self.rejected("unexpected files in unversioned")

    def test_unversioned_module_symlink_rejected(self):
        self.path("/lib/modules/kernel").symlink_to(self.path(f"/lib/modules/{self.kernel}"))
        self.rejected("unexpected files in unversioned")

    def test_unbound_source_rejected(self):
        os.environ["SOURCE_COMMIT"] = "latest"
        self.rejected("missing exact source commit")


if __name__ == "__main__":
    unittest.main()
