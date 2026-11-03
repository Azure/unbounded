# Ubuntu 24.04 GB200 test host image

This ARM64-only test image is built directly from the pinned Ubuntu 24.04
container image:

```text
docker.io/library/ubuntu@sha256:18b33ff3829707f31ada654a963b9ee35f2e6f0241b964bd17a22a9b330cb63d
```

The producer installs the pinned ARM64 kernel and NVIDIA 580.167.08 open driver,
constructs the root filesystem and EFI boot files, assembles
`/disk/disk.img.gz`, and packages the result under
`host-ubuntu2404-gb200-test`. It does not depend on another published host
image.

The image:

- installs pinned `ssh-import-id`, retains the locked `unbounded-metal` user,
  and adds the reviewed locked cloud-init users;
- embeds the checksum-pinned ARM64 `unbounded-agent` archive
  `v0.17.0-rc.1`;
- installs `options nvidia NVreg_NvLinkDisable=1` in the rootfs and rebuilds
  the boot kernel's initramfs so the option is effective before driver load;
- clears cloud-init state, machine identity, and SSH host private keys; and
- verifies the final gzip stream and records its SHA-256.

NVLink and NVLS are deliberately disabled. Local CUDA and fallback collectives
remain available.

MetalMan recognizes the exact GB200 test image repository, including tagged and
digest-pinned references, and does not replace its image-baked users with the
vendor-data user list. Other images retain the existing vendor user behavior.

Before HostReplace, the temporary Machine user-data ConfigMap reference must be
removed or replaced with configuration that retains the complete user list.

The embedded agent is available at:

```text
file:///opt/unbounded/bootstrap/unbounded-agent-linux-arm64.tar.gz
```

Build on a native ARM64 Linux host:

```bash
docker buildx build --load \
  --platform linux/arm64 \
  --build-arg SOURCE_COMMIT="$(git rev-parse HEAD)" \
  -t host-ubuntu2404-gb200-test:test \
  -f images/host-ubuntu2404-gb200-test/Containerfile .
```

Publishing is isolated in the `Publish GB200 test host image` workflow. It runs
on a native ARM64 runner for matching
`images/host-ubuntu2404-gb200-test/<version>` tags or a manual dispatch. The
generic image workflow excludes matching test-image tags.

The build still downloads signed Ubuntu and NVIDIA repository packages and the
checksum-pinned agent release. Self-contained here means that it constructs the
host disk itself instead of requiring a previously published host image.

Building or publishing the artifact does not update or reboot a live machine.
