# Ubuntu 24.04 GB200 test host image

This ARM64-only test image derives from the pinned immutable production image:

```text
ghcr.io/azure/host-ubuntu2404-gb200-nvidia-580167@sha256:39f4fd5312307ee2ef7ec5e236e420faa5d08e663853800d86fb6a3e162181bb
```

The production image is already published in GHCR as an OCI image containing
`/disk/disk.img.gz`. The test-image producer pulls the exact digest, copies that
disk from a stopped container, applies the changes below, and packages the
result under `host-ubuntu2404-gb200-test`.

The derivative makes only the test and recovery changes required for this
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

MetalMan preserves image-baked users for the exact GB200 test and production
image repositories so they survive vendor-data merging. Generic and lookalike
images retain the existing vendor user.

The embedded agent is available at:

```text
file:///opt/unbounded/bootstrap/unbounded-agent-linux-arm64.tar.gz
```

Build on a native ARM64 Linux host:

```bash
sudo apt-get install -y pigz
sudo images/host-ubuntu2404-gb200-test/build.sh
docker build \
  --platform linux/arm64 \
  --build-arg VERSION="$(git rev-parse HEAD)" \
  -t host-ubuntu2404-gb200-test:test \
  -f images/host-ubuntu2404-gb200-test/Containerfile .
```

Publishing is isolated in the `Publish GB200 test host image` workflow. It runs
on a native ARM64 runner for matching
`images/host-ubuntu2404-gb200-test/<version>` tags or a manual dispatch. The
generic image workflow excludes matching test-image tags.

Building or publishing the artifact does not update or reboot a live machine.
