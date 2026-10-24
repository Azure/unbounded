# Hillsboro Ubuntu 24.04 GB200 Metalman host image

This ARM64-only image builds the proven GB200 NVIDIA 580.167.08 root filesystem
directly, using the shared producer under `images/host-ubuntu2404-gb200`. The
generic `images/host-ubuntu2404` image and the Azure Linux netboot installer
remain unchanged.

The build adds all members in the verified 2026-10-23
`Azure/unbounded-dev` snapshot to `/etc/cloud/cloud.cfg.d` as native cloud-init
users. Each same-named account has a locked password, reviewed passwordless
sudo, and, when the member has published keys, `ssh_import_id: [gh:<user>]`.
No SSH keys, custom account script, or systemd cloud-final hook are embedded
in the image.

The snapshot includes `bcho`, who published no GitHub SSH keys when it was
verified. Its entry deliberately omits `ssh_import_id`, so cloud-init creates
the locked `bcho` account without turning the absence of keys into a boot
failure. It has no key-based access. Publishing a key does not grant access
automatically; a later reviewed policy snapshot must restore the import entry.
This does not block image readiness or affect key-based access for the other
listed users.

## Build

The selected NVIDIA profile matches the source revision used to build the
digest that successfully repaved the GB200 Ubuntu host on 2026-10-21. The
producer installs `ssh-import-id` with APT before disk capture, so the package
and its dependencies are registered normally. It builds the GPT disk directly
from the root filesystem without a privileged mount, contacting GitHub APIs,
publishing an image, or touching a Machine:

```bash
docker buildx build \
  --platform linux/arm64 \
  --build-arg SOURCE_COMMIT="$(git rev-parse HEAD)" \
  --load \
  -t localhost/unbounded/host-ubuntu2404-gb200-hillsboro:test \
  -f images/host-ubuntu2404-gb200-hillsboro/Containerfile .
```

The OCI artifact retains `/disk/disk.img.gz`. Before capture, the producer
verifies the pinned kernel, NVIDIA stack, MANA drivers, EFI binaries,
cloud-init, and `ssh-import-id`. It clears machine identity and SSH host keys.
The root filesystem is assembled into a new disk, so it cannot inherit
captured user accounts, password hashes, authorized keys, or host keys from an
earlier machine.

The publication boot gate uses the rendered MetalMan vendor-data with a
side-effect-free installer fixture. It proves cloud-init creates every named
account, keeps `bcho` keyless, and writes a nonempty deterministic
`authorized_keys` file for every other listed user.

Generic MetalMan vendor-data continues creating the `unbounded-metal` account.
For this exact Hillsboro image repository, MetalMan suppresses that top-level
`users` stanza so its higher-priority list cannot replace the image fragment.
The image build runs Ubuntu Noble cloud-init's merge implementation against
that rendered vendor-data and verifies that the image users and MetalMan's
`reporting`, `write_files`, and `runcmd` configuration all survive unchanged.
MetalMan also rejects a top-level `users` key in this image's Machine user-data
ConfigMap; generic images retain their existing override behavior.

## Metalman activation

After separately publishing and reviewing an immutable digest, replace only
the machine-image field:

```yaml
spec:
  host:
    netboot:
      image: ghcr.io/azure/host-ubuntu2404-gb200-hillsboro@sha256:<digest>
      netbootImage: <existing-azure-linux-netboot-image>
```

For a legacy Machine, replace `spec.pxe.image` instead and leave
`spec.pxe.netbootImage` unchanged.

Before changing the image reference, verify that the Machine's cloud-init
user-data does not define a top-level `users` key. User-data has higher
precedence than both vendor-data and the image's `cloud.cfg.d`; a `users` key
would replace the image-baked access policy.

The current temporary
`spec.host.netboot.cloudInit.userDataConfigMapRef: gb200-metal-host-access`
defines `users: [unbounded-metal]`. It must be removed or replaced with
user-data that does not define `users` before any `HostReplace`. MetalMan
automatically rejects a HostReplace and refuses to serve conflicting user-data
from either ConfigMap `data` or `binaryData` for this dedicated image. Other
machine images retain generic cloud-init user-data behavior.

The repository also includes a read-only CLI wrapper around the same
production validator:

```bash
kubectl -n <namespace> get configmap gb200-metal-host-access \
  -o jsonpath='{.data.user-data}' |
  go run ./hack/cmd/hostimage-userdata-preflight -
```

The command exits nonzero unless the input is exactly one cloud-config mapping
document without a top-level `users` key. Running it does not update the
Machine or ConfigMap.

Updating the image reference alone is non-destructive. Metalman uses the new
disk only during a later, separately authorized `HostReplace`. Do not create a
`HostReplace`, reboot a host, or otherwise activate this image as part of the
build or publication step.
