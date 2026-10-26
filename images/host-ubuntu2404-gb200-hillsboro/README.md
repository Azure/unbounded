# Hillsboro Ubuntu 24.04 GB200 host image

This ARM64-only MetalMan image derives from the previously qualified immutable
GB200 host disk:

```text
ghcr.io/azure/host-ubuntu2404-gb200-nvidia-580167@sha256:39f4fd5312307ee2ef7ec5e236e420faa5d08e663853800d86fb6a3e162181bb
```

The derivative keeps that disk's kernel, NVIDIA stack, firmware, and image
provenance. It installs pinned `ssh-import-id` and adds a native cloud-init user
fragment for the reviewed lab accounts. Passwords remain locked, shells are
`/bin/bash`, and each account has passwordless sudo. `bcho` deliberately has no
`ssh_import_id` entry because it currently has no published GitHub keys. The
other accounts import keys from their same-named GitHub identities.

The build uses libguestfs without mounting the disk or requiring privileged
containers. It verifies:

- the source and package checksums;
- the exact user policy and real Ubuntu Noble cloud-init merge behavior;
- the installed `ssh-import-id` package version;
- empty machine identity and absent SSH host private keys; and
- the final gzip stream and disk checksum.

Build natively on ARM64:

```bash
docker build \
  --platform linux/arm64 \
  -t host-ubuntu2404-gb200-hillsboro:test \
  -f images/host-ubuntu2404-gb200-hillsboro/Containerfile .
```

## Activation prerequisite

MetalMan omits its generic `unbounded-metal` vendor user only when the Machine
image repository is exactly
`ghcr.io/azure/host-ubuntu2404-gb200-hillsboro`. Generic images retain their
current vendor user.

Cloud-init user-data has higher precedence and can still replace the baked
users. Before a separately authorized `HostReplace`, remove or replace any
temporary Machine user-data ConfigMap reference whose cloud-config defines
`users`.

Building or publishing this artifact does not update a Machine, start a
`HostReplace`, reboot a host, or otherwise activate the image.
