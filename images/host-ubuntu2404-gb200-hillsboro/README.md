# Hillsboro Ubuntu 24.04 GB200 Metalman host image

This ARM64-only image builds the proven GB200 NVIDIA 580.167.08 root filesystem
directly, using the shared producer under `images/host-ubuntu2404-gb200`. The
generic `images/host-ubuntu2404` image and the Azure Linux netboot installer
remain unchanged.

The build adds all members in the verified 2026-10-23
`Azure/unbounded-dev` snapshot to `/etc/cloud/cloud.cfg.d` as native cloud-init
users. Each same-named account has a locked password, reviewed passwordless
sudo, and `ssh_import_id: [gh:<user>]`. No SSH keys, custom account script, or
systemd cloud-final hook are embedded in the image.

The snapshot includes `bcho`, who published no GitHub SSH keys when it was
verified. The producer intentionally keeps that account in policy as a
qualification blocker rather than silently dropping a team member. Do not
activate a built image until every listed member publishes at least one usable
GitHub SSH key and the blocker is re-reviewed.

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

MetalMan vendor-data intentionally does not define `users`; otherwise its
higher-priority list would replace the image fragment. The image build runs
Ubuntu Noble cloud-init's merge implementation against the actual vendor-data
template and verifies that the image users and MetalMan's `reporting`,
`write_files`, and `runcmd` configuration all survive unchanged. A Machine
user-data ConfigMap may still intentionally override `users`, because user-data
has the highest cloud-init precedence.

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
user-data that does not define `users` before any `HostReplace`. This repository
includes a fail-closed preflight for the referenced ConfigMap content:

```bash
kubectl -n <namespace> get configmap gb200-metal-host-access \
  -o jsonpath='{.data.user-data}' |
  go run ./hack/cmd/hostimage-userdata-preflight -
```

The command exits nonzero for invalid cloud-config or any top-level `users`
key. Running the preflight is read-only; it does not update the Machine or
ConfigMap.

Updating the image reference alone is non-destructive. Metalman uses the new
disk only during a later, separately authorized `HostReplace`. Do not create a
`HostReplace`, reboot a host, or otherwise activate this image as part of the
build or publication step.
