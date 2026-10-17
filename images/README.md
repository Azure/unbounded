# Container Images

This directory contains container image definitions for the project. Most subdirectories with a
`Containerfile` represent one buildable image.

## Directory Structure

```
images/
  <image-name>/
    Containerfile       # required - defines the container build
    ...                 # any supporting assets
```

The directory name becomes the image name in the registry. For example, `images/agent-ubuntu2404/`
produces `ghcr.io/<owner>/agent-ubuntu2404`.

## Building Images

Images are built automatically by the **Build Container Images** GitHub Actions workflow
(`.github/workflows/images.yaml`). All builds produce multi-arch images for `linux/amd64` and
`linux/arm64`.

### Metalman Netboot Image

The `netboot` image contains the reusable PXE boot environment: bootloaders, kernel, initrd overlay,
GRUB and cloud-init templates, and metadata. The cloud-init template downloads and installs
`unbounded-agent` from the configured release/source during first boot. It does not contain a
machine disk image.

### Azure Linux NVIDIA Grace Host Image

`host-azurelinux3-nvidia` is a multi-architecture Metalman machine image.
AMD64 retains the existing single-HWE-kernel layout. ARM64 installs both the
standard Azure Linux kernel and `kernel-hwe`, then defaults unconditionally to
`kernel-hwe` without hardware-specific kernel arguments. The standard kernel
remains available as a recovery entry. The separate generic
`host-azurelinux3` ARM64 image defaults to the standard kernel without those
arguments. In particular, the NVIDIA image must not set
`irqchip.gicv3_nolpi=y`: MANA NIC MSI-X interrupts use the GICv3 LPI/ITS path,
so disabling LPIs prevents host networking during boot.

The package inputs are recorded in
`images/host-azurelinux3-nvidia/versions.env`. Builds fail if the kernel,
NVIDIA driver, OFED, DKMS, or GRUB package versions differ, or if the package
matched `smbios.mod` is absent. The ARM64 image uses NVIDIA's versioned,
signed Azure Linux 3 local repository for R580.178.04. It verifies the
repository archive and key hashes, checks every embedded RPM signature, then
builds and installs all five open GPU modules explicitly for
`6.18.48.1-1.azl3`. The build rejects missing modules, mixed userspace or GSP
firmware versions, wrong vermagic, and the unrequested Fabric Manager, IMEX,
or `nvlink5` packages. AMD64 retains its existing package path.

Build and offline-inspect the disk without publishing it:

```bash
make verify-host-azurelinux3-nvidia
```

### Tagged Release

Push a git tag matching the pattern `images/<image-name>/<version>`:

```bash
git tag images/agent-ubuntu2404/v1.0.0
git push origin images/agent-ubuntu2404/v1.0.0
```

This builds and pushes:

```
ghcr.io/<owner>/agent-ubuntu2404:v1.0.0
```

The `<version>` portion of the tag is used as-is for the image tag, so use whatever versioning
scheme fits (e.g. `v1.0.0`, `v2.0.0-rc.1`, `20260401`).

### On-Demand Build (workflow_dispatch)

You can also trigger a build manually from **Actions > Build Container Images > Run workflow**.
Provide the image directory name (e.g. `agent-ubuntu2404`) as the input. The resulting image is
tagged with the full git commit SHA:

```
ghcr.io/<owner>/agent-ubuntu2404:<commit-sha>
```

## Adding a New Image

1. Create a new directory under `images/` with a descriptive name.
2. Add a `Containerfile` in that directory.
3. The workflow discovers it automatically - no workflow changes needed unless the image reuses a
   shared Containerfile.
4. Tag and push to build: `git tag images/<your-image>/v0.1.0 && git push origin images/<your-image>/v0.1.0`.
