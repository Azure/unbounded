#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Fail closed on the offline GB200 rootfs contract; never invoke the GPU."""

import hashlib
import json
import os
from pathlib import Path
import re
import struct
import subprocess


def command(*args):
    return subprocess.check_output(args, text=True).strip()


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def verify():
    kernel = os.environ["KVER"]
    driver = os.environ["NVIDIA_DRIVER_VERSION"]
    profile = os.environ["GB200_PROFILE"]
    module_source = os.environ["NVIDIA_MODULE_SOURCE"]
    source = os.environ["SOURCE_COMMIT"]
    profile_module_sources = {
        "canonical-control": "ubuntu-precompiled-server-open",
        "canonical-nvidia-580167": "nvidia-dkms-open",
        "agentbaker-driver": "nvidia-dkms-open",
        "agentbaker-doca": "nvidia-dkms-open",
        "agentbaker-doca-imex": "nvidia-dkms-open",
    }
    require(profile in profile_module_sources, f"unsupported GB200 profile: {profile}")
    require(
        module_source == profile_module_sources[profile],
        f"unexpected NVIDIA module source for {profile}: {module_source}",
    )
    canonical_kernel = profile in ("canonical-control", "canonical-nvidia-580167")
    require(re.fullmatch(r"[0-9a-f]{40}", source), "missing exact source commit")
    require(command("dpkg", "--print-architecture") == "arm64", "not ARM64")
    # Canonical's signed NVIDIA package also ships an empty shared kernel/ directory.
    shared = Path("/lib/modules/kernel")
    if shared.exists() or shared.is_symlink():
        require(
            shared.is_dir() and not shared.is_symlink()
            and all(p.is_dir() and not p.is_symlink() for p in shared.rglob("*")),
            "unexpected files in unversioned module directory",
        )
    kernels = sorted(p.name for p in Path("/lib/modules").iterdir() if p.name != "kernel")
    require(kernels == [kernel], f"expected exactly the pinned kernel {kernel}, found {kernels}")
    config = Path(f"/boot/config-{kernel}").read_text()
    require(
        sorted(p.name for p in Path("/boot").glob("initrd.img-*")) == [f"initrd.img-{kernel}"],
        "unexpected builder-kernel initramfs",
    )
    require(
        "update_initramfs=yes" in Path("/etc/initramfs-tools/update-initramfs.conf").read_text().splitlines(),
        "automatic initramfs updates were not restored",
    )
    require(
        "CONFIG_ARM64_64K_PAGES=y" in config.splitlines(),
        "missing kernel setting: CONFIG_ARM64_64K_PAGES=y",
    )
    mana_setting = (
        "CONFIG_MICROSOFT_MANA=m"
        if canonical_kernel
        else "CONFIG_MICROSOFT_MANA=y"
    )
    require(mana_setting in config.splitlines(), f"missing kernel setting: {mana_setting}")
    require(
        "CONFIG_MANA_INFINIBAND=m" in config.splitlines(),
        "missing kernel setting: CONFIG_MANA_INFINIBAND=m",
    )
    modules = {}
    for name in ("mana", "mana_ib", "nvidia", "nvidia_uvm"):
        filename = command("modinfo", "-k", kernel, "-F", "filename", name)
        if name == "mana" and not canonical_kernel:
            require(filename == "(builtin)", "MANA Ethernet driver is not built into the kernel")
            builtins = Path(f"/lib/modules/{kernel}/modules.builtin")
            require(
                "kernel/drivers/net/ethernet/microsoft/mana/mana.ko"
                in builtins.read_text().splitlines(),
                "MANA Ethernet driver is absent from built-in module metadata",
            )
            modules[name] = {"builtin": True, "metadata_sha256": sha256(builtins)}
            continue
        path = Path(filename)
        vermagic = command("modinfo", "-k", kernel, "-F", "vermagic", name)
        require(vermagic, f"{name} lacks kernel ABI metadata")
        require(vermagic.split()[0] == kernel, f"{name} kernel ABI mismatch")
        require("aarch64" in vermagic.split(), f"{name} is not ARM64")
        modules[name] = {"vermagic": vermagic, "sha256": sha256(path)}
        if name.startswith("nvidia"):
            require(
                command("modinfo", "-k", kernel, "-F", "version", name) == driver,
                f"{name} driver version mismatch",
            )
            require(
                "MIT" in command("modinfo", "-k", kernel, "-F", "license", name),
                f"{name} is not the open kernel module",
            )
            signer = command("modinfo", "-k", kernel, "-F", "signer", name)
            if module_source == "ubuntu-precompiled-server-open":
                require(signer, f"{name} lacks a module signature")
            modules[name]["signer"] = signer
    initrd = command("lsinitramfs", f"/boot/initrd.img-{kernel}").splitlines()
    if canonical_kernel:
        require(
            any(re.search(r"/mana\.ko(?:\.(?:zst|xz|gz))?$", p) for p in initrd),
            "MANA is absent from the initramfs",
        )
    firmware = {}
    for name in ("BOOTAA64.EFI", "grubaa64.efi", "mmaa64.efi"):
        path = Path("/boot/efi/EFI/BOOT") / name
        data = path.read_bytes()
        require(data[:2] == b"MZ", f"{name} is not PE/COFF")
        offset = struct.unpack_from("<I", data, 0x3C)[0]
        require(data[offset:offset + 4] == b"PE\0\0", f"{name} lacks a PE header")
        require(
            struct.unpack_from("<H", data, offset + 4)[0] == 0xAA64,
            f"{name} is not an ARM64 EFI binary",
        )
        firmware[name] = sha256(path)
    packages = command("dpkg-query", "-W", "-f=${Package}\t${Version}\t${Architecture}\n")
    if module_source == "ubuntu-precompiled-server-open":
        require("nvidia-dkms-" not in packages, "unexpected DKMS driver installation")
        package_names = (
            "nvidia-utils-580-server",
            "nvidia-fabricmanager-580",
            "libnvidia-nscq-580",
        )
    else:
        require(
            re.search(
                rf"^nvidia-dkms-580-open\t{re.escape(driver)}(?:[-+~][^\t]*)?\tarm64$",
                packages,
                re.MULTILINE,
            ),
            "missing pinned open DKMS driver",
        )
        package_names = ("nvidia-utils-580", "nvidia-fabricmanager", "libnvidia-nscq")
    for package in package_names:
        require(
            re.search(
                rf"^{package}\t{re.escape(driver)}(?:[-+~][^\t]*)?\tarm64$",
                packages,
                re.MULTILINE,
            ),
            f"missing pinned {package}",
        )
    if profile in ("agentbaker-doca", "agentbaker-doca-imex"):
        require(
            re.search(r"^doca-ofed\t3\.1\.0-091513\tarm64$", packages, re.MULTILINE),
            "missing pinned DOCA-OFED",
        )
        require(
            command("modinfo", "-k", kernel, "-F", "filename", "nvidia_peermem"),
            "missing nvidia_peermem",
        )
    if profile == "agentbaker-doca-imex":
        require(
            re.search(
                rf"^nvidia-imex\t{re.escape(driver)}(?:[-+~][^\t]*)?\tarm64$",
                packages,
                re.MULTILINE,
            ),
            "missing pinned NVIDIA IMEX",
        )
        parameters = Path("/etc/modprobe.d/nvidia-gb200.conf")
        require(
            parameters.exists()
            and parameters.read_text().splitlines()
            == [
                "options nvidia NVreg_RestrictProfilingToAdminUsers=0",
                "options nvidia NVreg_CreateImexChannel0=1",
                "options nvidia NVreg_CoherentGPUMemoryMode=driver",
                'options nvidia NVreg_RegistryDwords="RMBug5172204War=4"',
            ],
            "invalid AgentBaker NVIDIA module parameters",
        )
        require(
            Path("/etc/systemd/system/multi-user.target.wants/nvidia-imex.service").is_symlink(),
            "NVIDIA IMEX service is not enabled",
        )
    require(Path("/usr/bin/nv-fabricmanager").is_file(), "missing Fabric Manager binary")
    require(
        any(Path("/usr/share/nvidia/nvswitch").glob("gb200_*_topology")),
        "missing GB200 Fabric Manager topology",
    )
    require(
        Path("/usr/lib/aarch64-linux-gnu/libnvidia-nscq.so").exists(),
        "missing NVSwitch configuration library",
    )
    require(
        Path(
            "/etc/systemd/system/multi-user.target.wants/nvidia-fabricmanager.service"
        ).is_symlink(),
        "NVIDIA Fabric Manager service is not enabled",
    )
    module_load_path = Path("/etc/modules-load.d/nvidia.conf")
    expected_boot_modules = ["nvidia", "nvidia_modeset", "nvidia_uvm", "nvidia_drm"]
    if profile in ("agentbaker-doca", "agentbaker-doca-imex"):
        expected_boot_modules.append("nvidia_peermem")
    require(
        module_load_path.exists()
        and module_load_path.read_text().splitlines() == expected_boot_modules,
        "NVIDIA modules are not configured for boot",
    )
    require(
        not Path("/etc/systemd/system/nvidia-persistence-mode.service").exists(),
        "unexpected NVIDIA persistence retry service",
    )
    device_init = Path("/etc/systemd/system/nvidia-device-init.service")
    device_init_enabled = Path(
        "/etc/systemd/system/multi-user.target.wants/nvidia-device-init.service"
    )
    device_init_script = Path("/usr/local/sbin/unbounded-nvidia-device-init")
    if module_source == "ubuntu-precompiled-server-open":
        require(not device_init.exists(), "unexpected NVIDIA device initialization service")
        require(
            not device_init_script.exists(),
            "unexpected NVIDIA device initialization script",
        )
    else:
        require(
            device_init.exists()
            and device_init.read_text().splitlines()
            == [
                "[Unit]",
                "Description=Initialize NVIDIA GPU and NVSwitch device nodes",
                "Before=nvidia-fabricmanager.service cloud-final.service",
                "",
                "[Service]",
                "Type=oneshot",
                "ExecStart=/usr/local/sbin/unbounded-nvidia-device-init",
                "RemainAfterExit=yes",
                "",
                "[Install]",
                "WantedBy=multi-user.target",
            ],
            "invalid NVIDIA device initialization service",
        )
        require(
            device_init_script.exists()
            and device_init_script.read_text().splitlines()
            == [
                "#!/bin/sh",
                "set -eu",
                "",
                "/usr/bin/nvidia-modprobe -c 0",
                "",
                "attempt=0",
                'while [ "$attempt" -lt 30 ]; do',
                "    /usr/bin/nvidia-modprobe -u",
                "    /usr/bin/nvidia-modprobe -s",
                "    missing=",
                "    for path in \\",
                "        /dev/nvidiactl \\",
                "        /dev/nvidia0 \\",
                "        /dev/nvidia-uvm \\",
                "        /dev/nvidia-uvm-tools \\",
                "        /dev/nvidia-nvswitchctl; do",
                '        if [ ! -e "$path" ]; then',
                '            missing="$missing $path"',
                "        fi",
                "    done",
                '    if [ -z "$missing" ]; then',
                "        exit 0",
                "    fi",
                "    attempt=$((attempt + 1))",
                "    sleep 1",
                "done",
                "",
                'echo "missing NVIDIA device nodes after initialization:$missing" >&2',
                "exit 1",
            ],
            "invalid NVIDIA device initialization script",
        )
        require(
            device_init_enabled.is_symlink(),
            "NVIDIA device initialization service is not enabled",
        )
    require(not Path("/etc/ssh/ssh_host_ed25519_key").exists(), "baked-in SSH key")
    require(Path("/etc/machine-id").read_bytes() == b"", "baked-in machine ID")
    result = {
        "schema": "unbounded.gb200.host-image.v1",
        "source_commit": source,
        "profile": profile,
        "architecture": "arm64",
        "ubuntu": "24.04",
        "kernel": kernel,
        "page_size": 65536,
        "nvidia_driver": driver,
        "nvidia_module_source": module_source,
        "modules": modules,
        "efi_sha256": firmware,
        "bootloader_packages": Path("/usr/share/unbounded/efi-packages.tsv").read_text(),
        "bootloader_package_sha256": Path("/usr/share/unbounded/efi-packages.sha256").read_text(),
        "packages_tsv": packages,
        "apt_inrelease_sha256": {
            p.name: sha256(p) for p in sorted(Path("/var/lib/apt/lists").glob("*_InRelease"))
        },
        "qualification": "offline rootfs checks only; no physical GPU, MANA traffic or NVLink qualification",
        "secure_boot": (
            "signed distribution modules; platform trust/enrollment not qualified"
            if module_source == "ubuntu-precompiled-server-open"
            else "DKMS module signing and platform trust/enrollment not qualified"
        ),
        "fabric_services": (
            "NVIDIA Fabric Manager, NSCQ, and IMEX installed"
            if profile == "agentbaker-doca-imex"
            else "NVIDIA Fabric Manager and NSCQ installed; IMEX/NVLSM not activated"
        ),
    }
    require(result["apt_inrelease_sha256"], "missing signed APT release provenance")
    Path("/usr/share/unbounded/image-inputs.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps({k: v for k, v in result.items() if k != "packages_tsv"}, indent=2))


if __name__ == "__main__":
    verify()
