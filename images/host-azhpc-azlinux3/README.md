# Azure HPC Azure Linux 3 Metalman Machine Image

This image contains only a gzip-compressed raw disk at `/disk/disk.img.gz`, converted from a VHD
produced by [Azure/azhpc-images](https://github.com/Azure/azhpc-images) for Azure Linux HPC 3.0.
Metalman installs it using the reusable `netboot` image selected by the controller default or by
`spec.pxe.netbootImage`.

Build with one VHD URL per target architecture:

```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --build-arg AZHPC_VHD_URL_AMD64=https://example/azlinux3-x86_64.vhd \
  --build-arg AZHPC_VHD_URL_ARM64=https://example/azlinux3-aarch64.vhd \
  --build-arg AZHPC_VHD_DESCRIPTION="Azure HPC Azure Linux 3 VHD" \
  --build-arg AZHPC_IMAGE_DESCRIPTION="Metalman machine image for Azure HPC Azure Linux 3 VHDs" \
  -f images/host-azhpc/Containerfile \
  -t host-azhpc-azlinux3:dev .
```

The VHD must be produced by upstream `azhpc-images` with `os_family=azurelinux` and
`distro_version=3.0`.
