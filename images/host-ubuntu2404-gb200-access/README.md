# Ubuntu 24.04 GB200 access host image

This ARM64-only image derives from the pinned immutable disk:

```text
ghcr.io/azure/host-ubuntu2404-gb200-nvidia-580167@sha256:39f4fd5312307ee2ef7ec5e236e420faa5d08e663853800d86fb6a3e162181bb
```

The derivative makes only the access and recovery changes required for this
image:

- installs pinned `ssh-import-id` and the reviewed locked cloud-init users;
- embeds the checksum-pinned ARM64 `unbounded-agent` archive
  `v0.17.0-rc.1`;
- installs `options nvidia NVreg_NvLinkDisable=1` in the rootfs and rebuilds
  the boot kernel's initramfs so the option is effective before driver load;
- clears cloud-init state, machine identity, and SSH host private keys; and
- verifies the final gzip stream and records its SHA-256.

NVLink and NVLS are deliberately disabled. Local CUDA and fallback collectives
remain available.

MetalMan omits its generic vendor user only for the exact
`ghcr.io/azure/host-ubuntu2404-gb200-access` repository so the baked users
survive vendor-data merging. Generic and lookalike images retain the existing
vendor user.

The embedded agent is available at:

```text
file:///opt/unbounded/bootstrap/unbounded-agent-linux-arm64.tar.gz
```

Build on a native ARM64 Linux host:

```bash
sudo apt-get install -y pigz
sudo images/host-ubuntu2404-gb200-access/build.sh
docker build \
  --platform linux/arm64 \
  --build-arg VERSION="$(git rev-parse HEAD)" \
  -t host-ubuntu2404-gb200-access:test \
  -f images/host-ubuntu2404-gb200-access/Containerfile .
```

Building or publishing the artifact does not update or reboot a live machine.
