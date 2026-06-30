# Azure HPC Ubuntu 24.04 Metalman Machine Image

This image contains only a gzip-compressed raw disk at `/disk/disk.img.gz`, converted from a VHD
produced by [Azure/azhpc-images](https://github.com/Azure/azhpc-images) for Ubuntu HPC 24.04.
Metalman installs it using the reusable `netboot` image selected by the controller default or by
`spec.pxe.netbootImage`.

Build with one VHD URL per target architecture:

```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --build-arg AZHPC_VHD_URL_AMD64=https://example/ubuntu2404-x86_64.vhd \
  --build-arg AZHPC_VHD_URL_ARM64=https://example/ubuntu2404-aarch64.vhd \
  --build-arg AZHPC_VHD_DESCRIPTION="Azure HPC Ubuntu 24.04 VHD" \
  --build-arg AZHPC_IMAGE_DESCRIPTION="Metalman machine image for Azure HPC Ubuntu 24.04 VHDs" \
  -f images/host-azhpc/Containerfile \
  -t host-azhpc-ubuntu2404:dev .
```

The VHD must be produced by upstream `azhpc-images` with `os_family=ubuntu` and
`distro_version=24.04`.
