# Azure HPC Ubuntu 24.04 Metalman Host Image

This image is a Metalman netboot wrapper around a VHD produced by
[Azure/azhpc-images](https://github.com/Azure/azhpc-images) for Ubuntu HPC 24.04.

Build with one VHD URL per target architecture:

```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --build-arg AZHPC_VHD_URL_AMD64=https://example/ubuntu2404-x86_64.vhd \
  --build-arg AZHPC_VHD_URL_ARM64=https://example/ubuntu2404-aarch64.vhd \
  -f images/host-azhpc-ubuntu2404/Containerfile \
  -t host-azhpc-ubuntu2404:dev .
```

The VHD must be produced by upstream `azhpc-images` with `os_family=ubuntu` and
`distro_version=24.04`.
