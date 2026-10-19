# Ubuntu 24.04 GB200 Metalman host image

This ARM64-only image supplies `/disk/disk.img.gz` for the existing Metalman
netboot installer. It does not replace the installer, deploy an agent, modify a
Machine, initiate HostReplace, or reboot a host.

The pinned tuple in `assets/versions.env` uses Ubuntu's NVIDIA 64K kernel
`7.0.0-1019-nvidia-64k`, Canonical's matching precompiled **server-open**
580.178.04 modules, and the matching headless server userspace. MANA Ethernet
(`mana`) and RDMA (`mana_ib`) come from that kernel, not DOCA or an unrelated
NIC driver. Kernel and NVIDIA package versions fail closed if unavailable;
there is no DKMS or different-kernel fallback.

The image contains a GPT disk with an EFI partition, signed Ubuntu ARM64
shim/GRUB binaries, and an ext4 root with at least 5 GiB additional space.
Metalman replaces `50-cloud-init.yaml` with its MAC-matched static netplan and
injects its NoCloud datasource. Machine IDs and SSH host keys are generated
at first boot. The image does not install or activate GPU Operator, DRA, IMEX,
Fabric Manager or NVLSM.

## Build and qualification

Dispatch `.github/workflows/images.yaml` with `image=host-ubuntu2404-gb200`.
It requires `platforms=linux/arm64` (also the default for this image) and uses
a native `ubuntu-24.04-arm` runner.

Publication follows rootfs checks and a bounded QEMU ARM64 UEFI boot check.
The checks cover the 64K kernel, matching signed open NVIDIA module
version/vermagic, MANA modules and initramfs inclusion, ARM64 EFI binaries,
cloud-init, networkd and SSH. The workflow retains `image-inputs.json`,
compressed-disk SHA256 and the boot log. The OCI is tagged by source commit;
activation must use its immutable registry digest.

QEMU has no GB200 or physical MANA device. Passing these checks does **not**
qualify GPU execution, physical MANA traffic/RDMA, NVLink, Secure Boot
enrollment, or the rack's fabric-service configuration. A separately
authorized one-host canary is required before any fleet rollout.
