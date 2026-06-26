---
title: "AMD GPU Support"
weight: 2
description: "How the unbounded-agent exposes AMD GPUs so the AMD Kubernetes device plugin can discover them."
---

The unbounded-agent detects AMD GPUs on the host and exposes the device nodes
and sysfs paths required by the AMD Kubernetes device plugin inside the
`systemd-nspawn` machine. Kubernetes resource advertisement is still handled by
a user-deployed AMD device plugin.

## Prerequisites

Before the agent can expose AMD GPUs, the host must have the AMDGPU/KFD driver
loaded and the expected device nodes present:

- `/dev/kfd`
- `/dev/dri/card*`
- `/dev/dri/renderD*`

The AMD Kubernetes device plugin must also be deployed in the cluster. The agent
does not install the plugin, configure ROCm userspace libraries, or create a
custom containerd runtime for AMD GPUs.

## How It Works

AMD GPU support is intentionally limited to making host hardware visible inside
the nspawn machine so the AMD device plugin can do the Kubernetes-level
registration:

1. **Device discovery.** The agent checks for `/dev/kfd`. When present, it also
   collects `/dev/dri/card*` and `/dev/dri/renderD*` device nodes.
2. **nspawn device exposure.** The agent writes `Bind=` entries for the
   discovered AMD device nodes and matching `DeviceAllow=<path> rwm` entries in
   the systemd service override.
3. **sysfs exposure.** The agent bind-mounts AMD-related sysfs paths read-only
   into the nspawn machine so the AMD device plugin can inspect the amdgpu
   driver and KFD topology.
4. **Kubernetes advertisement.** After kubelet starts, the AMD Kubernetes device
   plugin detects the exposed devices and registers AMD GPU resources on the
   node.

## Exposed Host Paths

When `/dev/kfd` exists, the agent exposes these device nodes when present:

- `/dev/kfd`
- `/dev/dri/card*`
- `/dev/dri/renderD*`

The agent also exposes these sysfs paths read-only when present:

- `/sys/module/amdgpu`
- `/sys/class/kfd`
- `/sys/class/drm`
- `/sys/devices`

These sysfs mounts are required because the AMD device plugin reads paths such
as `/sys/module/amdgpu/drivers/pci:amdgpu/...` and
`/sys/class/kfd/kfd/topology/...` during discovery.

## Troubleshooting

### Device plugin exits with `amdgpu driver unavailable`

If the AMD device plugin logs an error like this:

```text
amdgpu driver unavailable. exiting with exit code 2. error: stat /sys/module/amdgpu/drivers/: no such file or directory
```

the nspawn machine was likely started without the AMD sysfs bind mounts. Verify
that the host has the AMD driver loaded and restart or recreate the nspawn
machine after the agent has rendered the updated `.nspawn` configuration.

On the host, check:

```bash
ls -ld /sys/module/amdgpu /sys/class/kfd /sys/class/drm
ls -l /dev/kfd /dev/dri/card* /dev/dri/renderD*
```

Inside the nspawn machine, check:

```bash
machinectl shell <machine-name> /bin/ls -ld /sys/module/amdgpu /sys/class/kfd /sys/class/drm
machinectl shell <machine-name> /bin/ls -l /dev/kfd /dev/dri/card* /dev/dri/renderD*
```
