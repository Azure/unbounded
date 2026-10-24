# Ubuntu 24.04 GB200 Metalman producer

This directory contains the shared ARM64 Ubuntu 24.04 GB200 rootfs, disk, and
qualification logic. Its Containerfile builds the Canonical control profile.
Dedicated variants reuse these assets and select a profile with
`GB200_PROFILE`.

Every produced OCI image contains `/disk/disk.img.gz` for the existing
Metalman netboot installer. The producer installs cloud-init, ssh-import-id,
SSH, the pinned kernel and NVIDIA stack, MANA support, and signed ARM64 EFI
bootloaders before assembling a new GPT disk. It generalizes machine-id and SSH
host keys before capture.

The generic producer does not include lab users or SSH identities. Dedicated
site variants may add a cloud-init fragment before `rootfs.sh finish` captures
the root filesystem.

## Qualification

GB200 images build on a native ARM64 GitHub Actions runner. Publication occurs
only after:

- the offline rootfs verifier checks the exact kernel, NVIDIA, MANA, EFI,
  cloud-init, ssh-import-id, package provenance, machine identity, and SSH host
  key contract;
- the disk is assembled without loop devices or privileged mounts; and
- `verify-boot.sh` boots the generated disk with ARM64 UEFI/QEMU and verifies
  the 64K kernel, cloud-init, networkd, SSH, MANA, and NVIDIA module path.

QEMU does not qualify physical GPU execution, MANA traffic/RDMA, NVLink, or
platform Secure Boot enrollment. Activation still requires a separately
authorized one-host canary and `HostReplace`.
