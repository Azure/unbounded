# Ubuntu 24.04 GB200 access host image

This ARM64-only MetalMan image derives from the previously qualified immutable
GB200 host disk:

```text
ghcr.io/azure/host-ubuntu2404-gb200-nvidia-580167@sha256:39f4fd5312307ee2ef7ec5e236e420faa5d08e663853800d86fb6a3e162181bb
```

The derivative keeps that disk's NVIDIA stack, firmware, and image provenance,
then installs the archived Ubuntu 24.04 kernel
`6.14.0-1003-azure-nvidia` (`6.14.0-1003.3`) from a fixed Ubuntu snapshot. It
removes the source image's newer kernel so the required version is the only
installed boot kernel. It also installs pinned `ssh-import-id` and adds a native
cloud-init user fragment for the reviewed host access accounts. Passwords remain
locked, shells are `/bin/bash`, and each account has passwordless sudo. `bcho`
deliberately has no `ssh_import_id` entry because it currently has no published
GitHub keys. The other accounts import keys from their same-named GitHub
identities.

The dedicated build script runs on a native ARM64 host. It mounts the qualified
raw disk in a private mount namespace, performs the package and DKMS work in the
disk's own ARM64 userspace, then boots a disposable overlay. It uses KVM when
available and otherwise uses TCG to verify that the exact target kernel starts.
Full userspace and driver validation belongs on the separately authorized
target-hardware canary. The build verifies:

- the source and package checksums;
- Ubuntu `24.04` and kernel `6.14.0-1003-azure-nvidia` package versions;
- the boot kernel symlink and rebuilt NVIDIA DKMS module;
- NVIDIA module loading and device-node initialization before cloud-final;
- the cloud-init user fragment schema;
- the installed `ssh-import-id` package version;
- the EFI GRUB entries and exact target-kernel boot, reaching userspace when
  compatible KVM is available;
- empty machine identity and absent SSH host private keys; and
- the final gzip stream and disk checksum.

Build natively on ARM64:

```bash
sudo apt-get install -y pigz qemu-efi-aarch64 qemu-system-arm qemu-utils
sudo images/host-ubuntu2404-gb200-access/build.sh
docker build \
  --platform linux/arm64 \
  --build-arg SOURCE_COMMIT="$(git rev-parse HEAD)" \
  -t host-ubuntu2404-gb200-access:test \
  -f images/host-ubuntu2404-gb200-access/Containerfile .
```

## Activation prerequisite

MetalMan omits its generic `unbounded-metal` vendor user only when the Machine
image repository is exactly
`ghcr.io/azure/host-ubuntu2404-gb200-access`. Generic images retain their
current vendor user.

Cloud-init user-data has higher precedence and can still replace the baked
users. Before a separately authorized `HostReplace`, remove or replace any
temporary Machine user-data ConfigMap reference whose cloud-config defines
`users`.

Building or publishing this artifact does not update a Machine, start a
`HostReplace`, reboot a host, or otherwise activate the image.
