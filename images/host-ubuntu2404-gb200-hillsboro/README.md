# Hillsboro Ubuntu 24.04 GB200 Metalman host image

This ARM64-only image is a Hillsboro access-policy derivative of the proven
GB200 machine image. It copies the source disk by immutable OCI digest and
changes only the installed root filesystem. The generic
`images/host-ubuntu2404` image and the Azure Linux netboot installer remain
unchanged.

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

The default base is the digest that successfully repaved the GB200 Ubuntu host
on 2026-10-21. The build uses `debugfs` against an extracted ext4 partition; it
does not use a privileged mount, contact GitHub APIs, publish an image, or
touch a Machine:

```bash
docker buildx build \
  --platform linux/arm64 \
  --load \
  -t localhost/unbounded/host-ubuntu2404-gb200-hillsboro:test \
  -f images/host-ubuntu2404-gb200-hillsboro/Containerfile .
```

The OCI artifact retains `/disk/disk.img.gz`. The build fails if the source
disk lacks cloud-init or account-management tools; contains an `administrator`,
`unbounded`, or `unbounded-metal` account; contains a password hash or captured
user SSH credential; or cannot be generalized. The producer verifies and
extracts the pinned Ubuntu `ssh-import-id` package into the disk before capture,
then installs the native cloud-init user fragment. It clears cloud-init state
and machine-id and removes SSH host keys before recompressing the disk.

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
