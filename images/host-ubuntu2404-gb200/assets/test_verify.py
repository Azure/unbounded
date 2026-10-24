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
        self.mana_builtin = False
        self.ssh_import_id_version = "5.11-0ubuntu2.24.04.1"
        self.packages = (
            "linux-image\t7.0\tarm64\n"
            "nvidia-utils-580-server\t580.178.04\tarm64\n"
            "nvidia-fabricmanager-580\t580.178.04\tarm64\n"
            "libnvidia-nscq-580\t580.178.04\tarm64\n"
            "ssh-import-id\t5.11-0ubuntu2.24.04.1\tall"
        )
        self.put(
            f"/boot/config-{self.kernel}",
            "CONFIG_ARM64_64K_PAGES=y\n"
            "CONFIG_MICROSOFT_MANA=m\n"
            "CONFIG_MANA_INFINIBAND=m\n",
        )
        self.put("/etc/machine-id", "")
        self.put(f"/boot/initrd.img-{self.kernel}", "initramfs fixture")
        self.put("/etc/initramfs-tools/update-initramfs.conf", "update_initramfs=yes\n")
        self.put(
            "/etc/modules-load.d/nvidia.conf",
            "nvidia\nnvidia_modeset\nnvidia_uvm\nnvidia_drm\n",
        )
        fabric_enabled = self.path(
            "/etc/systemd/system/multi-user.target.wants/nvidia-fabricmanager.service"
        )
        fabric_enabled.parent.mkdir(parents=True, exist_ok=True)
        fabric_enabled.symlink_to("/lib/systemd/system/nvidia-fabricmanager.service")
        self.put("/usr/bin/nv-fabricmanager", "fabric manager fixture")
        self.put(
            "/usr/share/nvidia/nvswitch/gb200_nvl4r1_c2g2_etf_topology",
            "topology fixture",
        )
        self.put(
            "/usr/lib/aarch64-linux-gnu/libnvidia-nscq.so",
            "NSCQ fixture",
        )
        self.put("/usr/bin/ssh-import-id", "ssh-import-id fixture")
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
        patch.dict(
            os.environ,
            KVER=self.kernel,
            NVIDIA_DRIVER_VERSION=self.driver,
            NVIDIA_MODULE_SOURCE="ubuntu-precompiled-server-open",
            GB200_PROFILE="canonical-control",
            SSH_IMPORT_ID_VERSION="5.11-0ubuntu2.24.04.1",
            SOURCE_COMMIT="a" * 40,
        ).start()

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
            if args[-1] == "ssh-import-id":
                return self.ssh_import_id_version
            return self.packages
        if args[0] == "lsinitramfs":
            return self.initrd
        if args[0] == "modinfo":
            field, name = args[4:]
            if name == "mana" and self.mana_builtin:
                return {"filename": "(builtin)"}[field]
            return {
                "filename": f"/lib/modules/{self.kernel}/{name}.ko",
                "vermagic": self.vermagic,
                "version": self.module_version,
                "license": self.license,
                "signer": self.signer,
            }[field]
        self.fail(f"unexpected command: {args}")

    def configure_agentbaker_kernel(self):
        self.mana_builtin = True
        self.put(
            f"/boot/config-{self.kernel}",
            "CONFIG_ARM64_64K_PAGES=y\n"
            "CONFIG_MICROSOFT_MANA=y\n"
            "CONFIG_MANA_INFINIBAND=m\n",
        )
        self.put(
            f"/lib/modules/{self.kernel}/modules.builtin",
            "kernel/drivers/net/ethernet/microsoft/mana/mana.ko\n",
        )

    def rejected(self, message):
        with self.assertRaisesRegex(RuntimeError, message):
            verify.verify()

    def test_valid_rootfs_writes_provenance(self):
        verify.verify()
        result = json.loads(self.path("/usr/share/unbounded/image-inputs.json").read_text())
        self.assertEqual(result["page_size"], 65536)
        self.assertEqual(result["nvidia_driver"], self.driver)
        self.assertEqual(result["ssh_import_id"], "5.11-0ubuntu2.24.04.1")
        self.assertEqual(set(result["modules"]), {"mana", "mana_ib", "nvidia", "nvidia_uvm"})
        self.assertTrue(result["apt_inrelease_sha256"])

    def test_target_initramfs_bootstrapped_before_nvidia_postinst(self):
        source = Path(__file__).with_name("rootfs.sh").read_text()
        kernel = source.index('"linux-image-${KVER}=${KERNEL_VERSION}"')
        bootstrap = source.index('test -s "/boot/initrd.img-$KVER"')
        defer = source.index("update_initramfs=no")
        nvidia = source.index('"nvidia-kernel-common-580-server=${NVIDIA_VERSION}"')
        restore = source.index("update_initramfs=yes")
        self.assertLess(kernel, bootstrap)
        self.assertLess(bootstrap, defer)
        self.assertLess(defer, nvidia)
        self.assertLess(nvidia, restore)

    def test_profiles_pin_controlled_stack_deltas(self):
        source = Path(__file__).with_name("rootfs.sh").read_text()
        versions = Path(__file__).with_name("versions.env").read_text()
        for profile in (
            "canonical-control",
            "canonical-nvidia-580167",
            "agentbaker-driver",
            "agentbaker-doca",
            "agentbaker-doca-imex",
        ):
            self.assertIn(profile, source)
        self.assertIn("NVIDIA_REFERENCE_VERSION=580.167.08-1ubuntu1", versions)
        self.assertIn("SSH_IMPORT_ID_VERSION=5.11-0ubuntu2.24.04.1", versions)
        self.assertIn("AGENTBAKER_KVER=6.14.0-1009-azure-nvidia", versions)
        self.assertIn("AGENTBAKER_KERNEL_VERSION=6.14.0-1009.9", versions)
        self.assertIn("AGENTBAKER_NVIDIA_VERSION=580.159.04-1ubuntu1", versions)
        self.assertIn("DOCA_VERSION=3.1.0-091513", versions)
        self.assertIn("gcc-13", source)
        self.assertIn('"libnvidia-gl-580=${NVIDIA_VERSION}"', source)
        self.assertIn('"nvidia-dkms-580-open=${NVIDIA_VERSION}"', source)
        self.assertIn('"nvidia-kernel-common-580=${NVIDIA_VERSION}"', source)
        self.assertIn('"nvidia-imex=${NVIDIA_VERSION}"', source)
        self.assertIn('"doca-ofed=${DOCA_VERSION}"', source)

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
        self.put(
            f"/boot/config-{self.kernel}",
            "CONFIG_MICROSOFT_MANA=m\nCONFIG_MANA_INFINIBAND=m\n",
        )
        self.rejected("CONFIG_ARM64_64K_PAGES")

    def test_missing_mana_configuration(self):
        self.put(
            f"/boot/config-{self.kernel}",
            "CONFIG_ARM64_64K_PAGES=y\nCONFIG_MANA_INFINIBAND=m\n",
        )
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

    def test_missing_ssh_import_id_rejected(self):
        self.path("/usr/bin/ssh-import-id").unlink()
        self.rejected("missing ssh-import-id")

    def test_wrong_ssh_import_id_version_rejected(self):
        self.ssh_import_id_version = "unexpected"
        self.rejected("missing pinned ssh-import-id")

    def test_dkms_fallback_rejected(self):
        self.packages += "\nnvidia-dkms-580-server-open\t580\tarm64"
        self.rejected("unexpected DKMS")

    def test_missing_nvidia_utilities_rejected(self):
        self.packages = "linux-image\t7.0\tarm64"
        self.rejected("missing pinned nvidia-utils-580-server")

    def test_missing_fabric_manager_package_rejected(self):
        self.packages = self.packages.replace(
            "nvidia-fabricmanager-580\t580.178.04\tarm64\n", ""
        )
        self.rejected("missing pinned nvidia-fabricmanager-580")

    def test_profile_module_source_mismatch_rejected(self):
        os.environ["GB200_PROFILE"] = "canonical-nvidia-580167"
        self.rejected("unexpected NVIDIA module source")

    def test_agentbaker_driver_profile_accepts_pinned_open_dkms(self):
        self.configure_agentbaker_kernel()
        os.environ["GB200_PROFILE"] = "agentbaker-driver"
        os.environ["NVIDIA_MODULE_SOURCE"] = "nvidia-dkms-open"
        os.environ["NVIDIA_DRIVER_VERSION"] = "580.159.04"
        self.driver = "580.159.04"
        self.module_version = self.driver
        self.packages = (
            "linux-azure-nvidia\t6.14.0-1009.9\tarm64\n"
            "nvidia-dkms-580-open\t580.159.04-1ubuntu1\tarm64\n"
            "nvidia-utils-580\t580.159.04-1ubuntu1\tarm64\n"
            "nvidia-fabricmanager\t580.159.04-1ubuntu1\tarm64\n"
            "libnvidia-nscq\t580.159.04-1ubuntu1\tarm64"
        )
        verify.verify()

    def test_canonical_reference_driver_accepts_pinned_open_dkms(self):
        os.environ["GB200_PROFILE"] = "canonical-nvidia-580167"
        os.environ["NVIDIA_MODULE_SOURCE"] = "nvidia-dkms-open"
        os.environ["NVIDIA_DRIVER_VERSION"] = "580.167.08"
        self.driver = "580.167.08"
        self.module_version = self.driver
        self.signer = ""
        self.packages = (
            "linux-image\t7.0.0-1019.19~24.04.2\tarm64\n"
            "nvidia-dkms-580-open\t580.167.08-1ubuntu1\tarm64\n"
            "nvidia-utils-580\t580.167.08-1ubuntu1\tarm64\n"
            "nvidia-fabricmanager\t580.167.08-1ubuntu1\tarm64\n"
            "libnvidia-nscq\t580.167.08-1ubuntu1\tarm64"
        )
        verify.verify()

    def test_agentbaker_driver_profile_requires_open_dkms(self):
        self.configure_agentbaker_kernel()
        os.environ["GB200_PROFILE"] = "agentbaker-driver"
        os.environ["NVIDIA_MODULE_SOURCE"] = "nvidia-dkms-open"
        os.environ["NVIDIA_DRIVER_VERSION"] = "580.159.04"
        self.driver = "580.159.04"
        self.module_version = self.driver
        self.packages = (
            "nvidia-utils-580\t580.159.04-1ubuntu1\tarm64\n"
            "nvidia-fabricmanager\t580.159.04-1ubuntu1\tarm64\n"
            "libnvidia-nscq\t580.159.04-1ubuntu1\tarm64"
        )
        self.rejected("missing pinned open DKMS driver")

    def test_agentbaker_rejects_legacy_device_initialization_service(self):
        self.configure_agentbaker_kernel()
        os.environ["GB200_PROFILE"] = "agentbaker-driver"
        os.environ["NVIDIA_MODULE_SOURCE"] = "nvidia-dkms-open"
        os.environ["NVIDIA_DRIVER_VERSION"] = "580.159.04"
        self.driver = "580.159.04"
        self.module_version = self.driver
        self.packages = (
            "nvidia-dkms-580-open\t580.159.04-1ubuntu1\tarm64\n"
            "nvidia-utils-580\t580.159.04-1ubuntu1\tarm64\n"
            "nvidia-fabricmanager\t580.159.04-1ubuntu1\tarm64\n"
            "libnvidia-nscq\t580.159.04-1ubuntu1\tarm64"
        )
        self.put(
            "/etc/systemd/system/nvidia-device-init.service",
            "[Service]\nExecStart=/usr/local/sbin/unbounded-nvidia-device-init\n",
        )
        self.rejected("unexpected NVIDIA device initialization service")

    def test_agentbaker_rejects_legacy_device_initialization_script(self):
        self.configure_agentbaker_kernel()
        os.environ["GB200_PROFILE"] = "agentbaker-driver"
        os.environ["NVIDIA_MODULE_SOURCE"] = "nvidia-dkms-open"
        os.environ["NVIDIA_DRIVER_VERSION"] = "580.159.04"
        self.driver = "580.159.04"
        self.module_version = self.driver
        self.packages = (
            "nvidia-dkms-580-open\t580.159.04-1ubuntu1\tarm64\n"
            "nvidia-utils-580\t580.159.04-1ubuntu1\tarm64\n"
            "nvidia-fabricmanager\t580.159.04-1ubuntu1\tarm64\n"
            "libnvidia-nscq\t580.159.04-1ubuntu1\tarm64"
        )
        self.put("/usr/local/sbin/unbounded-nvidia-device-init", "#!/bin/sh\n")
        self.rejected("unexpected NVIDIA device initialization script")

    def test_doca_profile_requires_doca_ofed(self):
        self.configure_agentbaker_kernel()
        os.environ["GB200_PROFILE"] = "agentbaker-doca"
        os.environ["NVIDIA_MODULE_SOURCE"] = "nvidia-dkms-open"
        os.environ["NVIDIA_DRIVER_VERSION"] = "580.159.04"
        self.driver = "580.159.04"
        self.module_version = self.driver
        self.packages = (
            "nvidia-dkms-580-open\t580.159.04-1ubuntu1\tarm64\n"
            "nvidia-utils-580\t580.159.04-1ubuntu1\tarm64\n"
            "nvidia-fabricmanager\t580.159.04-1ubuntu1\tarm64\n"
            "libnvidia-nscq\t580.159.04-1ubuntu1\tarm64"
        )
        self.rejected("missing pinned DOCA-OFED")

    def test_doca_profile_accepts_external_mana_modules(self):
        self.configure_agentbaker_kernel()
        os.environ["GB200_PROFILE"] = "agentbaker-doca"
        os.environ["NVIDIA_MODULE_SOURCE"] = "nvidia-dkms-open"
        os.environ["NVIDIA_DRIVER_VERSION"] = "580.159.04"
        self.driver = "580.159.04"
        self.module_version = self.driver
        self.put(
            "/etc/modules-load.d/nvidia.conf",
            "nvidia\nnvidia_modeset\nnvidia_uvm\nnvidia_drm\nnvidia_peermem\n",
        )
        self.packages = (
            "nvidia-dkms-580-open\t580.159.04-1ubuntu1\tarm64\n"
            "nvidia-utils-580\t580.159.04-1ubuntu1\tarm64\n"
            "nvidia-fabricmanager\t580.159.04-1ubuntu1\tarm64\n"
            "libnvidia-nscq\t580.159.04-1ubuntu1\tarm64\n"
            "doca-ofed\t3.1.0-091513\tarm64"
        )
        verify.verify()

    def test_imex_profile_requires_parameters_and_service(self):
        self.configure_agentbaker_kernel()
        os.environ["GB200_PROFILE"] = "agentbaker-doca-imex"
        os.environ["NVIDIA_MODULE_SOURCE"] = "nvidia-dkms-open"
        os.environ["NVIDIA_DRIVER_VERSION"] = "580.159.04"
        self.driver = "580.159.04"
        self.module_version = self.driver
        self.packages = (
            "nvidia-dkms-580-open\t580.159.04-1ubuntu1\tarm64\n"
            "nvidia-utils-580\t580.159.04-1ubuntu1\tarm64\n"
            "nvidia-fabricmanager\t580.159.04-1ubuntu1\tarm64\n"
            "libnvidia-nscq\t580.159.04-1ubuntu1\tarm64\n"
            "doca-ofed\t3.1.0-091513\tarm64\n"
            "nvidia-imex\t580.159.04-1ubuntu1\tarm64"
        )
        self.rejected("invalid AgentBaker NVIDIA module parameters")

    def test_missing_fabric_manager_topology_rejected(self):
        self.path(
            "/usr/share/nvidia/nvswitch/gb200_nvl4r1_c2g2_etf_topology"
        ).unlink()
        self.rejected("missing GB200 Fabric Manager topology")

    def test_disabled_fabric_manager_rejected(self):
        self.path(
            "/etc/systemd/system/multi-user.target.wants/nvidia-fabricmanager.service"
        ).unlink()
        self.rejected("NVIDIA Fabric Manager service is not enabled")

    def test_missing_nvidia_boot_module_configuration_rejected(self):
        self.path("/etc/modules-load.d/nvidia.conf").unlink()
        self.rejected("NVIDIA modules are not configured for boot")

    def test_persistence_retry_service_rejected(self):
        self.put(
            "/etc/systemd/system/nvidia-persistence-mode.service",
            "[Service]\nExecStart=/usr/bin/nvidia-smi -pm 1\nRestart=on-failure\n",
        )
        self.rejected("unexpected NVIDIA persistence retry")

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
