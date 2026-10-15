# Ubuntu 24.04 NVIDIA GB200 MetalMan image

This image contains a bootable arm64 GPT/EFI disk at `/disk/disk.img.gz` for
MetalMan. It is intentionally separate from the generic `host-ubuntu2404`
image and from the MANA-capable netboot installer.

The image pins one package contract:

- Ubuntu Azure NVIDIA kernel `6.8.0-1039-azure-nvidia`
- NVIDIA server-open driver `580.178.04`
- NVIDIA Container Toolkit `1.19.1`
- Fabric Manager and NSCQ `580.178.04`
- NVIDIA IMEX `580.178.04`

The build verifies that `mana`, `mana_ib`, and NVIDIA kernel modules are
present. IMEX is installed but disabled and masked; ComputeDomain activation
must configure, unmask, and start it.

## Publish

Release through the repository image workflow with an immutable version tag:

```bash
git tag images/host-ubuntu2404-nvidia/gb200-ubuntu2404-20261015
git push origin images/host-ubuntu2404-nvidia/gb200-ubuntu2404-20261015
```

After publication, resolve and record the digest:

```bash
docker buildx imagetools inspect \
  ghcr.io/azure/host-ubuntu2404-nvidia:gb200-ubuntu2404-20261015
```

Use only the resulting digest in a Machine:

```yaml
spec:
  pxe:
    image: ghcr.io/azure/host-ubuntu2404-nvidia@sha256:<published-arm64-manifest-digest>
    netbootImage: ghcr.io/azure/netboot-azurelinux@sha256:1555146e913ef55c22a0abb162d43057c72aed4a74764031f4eccac03623011f
```

Publishing the OCI image does not qualify it on GB200 hardware. Hardware boot,
GPU/NVSwitch validation, ComputeDomain activation, and a MetalMan repave are
separate gates.
