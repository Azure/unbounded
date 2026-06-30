# Azure HPC Azure Linux 3 Metalman Host Image

This image is a Metalman netboot wrapper around a VHD produced by
[Azure/azhpc-images](https://github.com/Azure/azhpc-images) for Azure Linux HPC 3.0.

Build with one VHD URL per target architecture:

```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --build-arg AZHPC_VHD_URL_AMD64=https://example/azlinux3-x86_64.vhd \
  --build-arg AZHPC_VHD_URL_ARM64=https://example/azlinux3-aarch64.vhd \
  -f images/host-azhpc-azlinux3/Containerfile \
  -t host-azhpc-azlinux3:dev .
```

The VHD must be produced by upstream `azhpc-images` with `os_family=azurelinux` and
`distro_version=3.0`.
