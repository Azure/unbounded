# Container Images

This directory contains container image definitions for the project. Each subdirectory with a
`Containerfile` represents a buildable image.

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

### Azure HPC Host Images

The `host-azhpc-ubuntu2404` and `host-azhpc-azlinux3` images are Metalman machine images containing
only a gzip-compressed raw disk at `/disk/disk.img.gz`. They wrap VHD artifacts produced from
[Azure/azhpc-images](https://github.com/Azure/azhpc-images). They do not rebuild or approximate Azure
HPC images in Docker. The build requires one VHD URL per architecture:

```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --build-arg AZHPC_VHD_URL_AMD64=https://example/ubuntu2404-x86_64.vhd \
  --build-arg AZHPC_VHD_URL_ARM64=https://example/ubuntu2404-aarch64.vhd \
  -f images/host-azhpc-ubuntu2404/Containerfile \
  -t ghcr.io/<owner>/host-azhpc-ubuntu2404:<tag> .
```

For GitHub Actions builds, pass the VHD URLs through `workflow_dispatch` inputs, or configure these
repository variables for tag-triggered builds:

```text
AZHPC_UBUNTU2404_VHD_URL_AMD64
AZHPC_UBUNTU2404_VHD_URL_ARM64
AZHPC_AZLINUX3_VHD_URL_AMD64
AZHPC_AZLINUX3_VHD_URL_ARM64
```

The VHDs should come from the upstream Packer workflow with `create_vhd=true` and the matching
`os_family`/`distro_version`: `ubuntu`/`24.04` for `host-azhpc-ubuntu2404`, and
`azurelinux`/`3.0` for `host-azhpc-azlinux3`. If an upstream variant does not publish one of the
architectures, omit that platform from the `docker buildx build --platform` list. The dedicated
GitHub Actions image workflow automatically builds only the AZHPC architectures with configured VHD
URLs unless the `platforms` workflow input is set explicitly.

Pair these machine images with Metalman's default `netboot` image, or set `spec.pxe.netbootImage` on
a Machine to override the default PXE boot environment.

### Metalman Netboot Image

The `netboot` image contains the reusable PXE boot environment: bootloaders, kernel, initrd overlay,
GRUB and cloud-init templates, metadata, and the `unbounded-agent` binary served during first boot.
It does not contain a machine disk image.

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
3. The workflow discovers it automatically - no workflow changes needed.
4. Tag and push to build: `git tag images/<your-image>/v0.1.0 && git push origin images/<your-image>/v0.1.0`.
