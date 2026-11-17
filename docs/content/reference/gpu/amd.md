---
title: "AMD GPU Support"
weight: 2
description: "How Unbounded discovers AMD GPUs on a host and passes the KFD and DRM devices through to the node stack in each isolation mode."
---

AMD GPU support in Unbounded is a passthrough path. The host keeps the `amdgpu`
kernel driver, the KFD device, and the DRM device nodes. The agent discovers
those devices, hands them to the isolation backend, and lets the node stack use
them exactly as a Kubernetes node installed directly on the host would. Unbounded
does not install the AMD driver, build a driver root, generate a CDI
specification, or register an AMD-specific container runtime. It also does not
deploy the AMD Kubernetes device plugin. Those pieces stay with the host
installation, the cluster add-on, and the workload image.

This division is the most important thing to understand about the page. On an
NVIDIA host, Unbounded performs a large amount of vendor setup: it stages driver
libraries, prepares a driver root, writes a CDI spec, and selects an `nvidia`
runtime. On an AMD host, the agent performs only discovery and device
passthrough. The AMD device plugin reads the exposed KFD and DRM interfaces,
advertises GPU capacity to the control plane, and arranges device access for
pods. ROCm user space libraries belong in the workload image, not in the node
stack.

This page explains what the host must provide, how the agent discovers AMD
hardware, how each isolation mode delivers the devices, how to verify the path
from host to pod, and what to do when a GPU does not appear. Read
[GPU Reference]({{< relref "reference/gpu" >}}) for the vendor-neutral model and
[NVIDIA GPU Support]({{< relref "reference/gpu/nvidia" >}}) for the deeper
integration that Unbounded provides on NVIDIA hardware.

## Prerequisites

The requirements below apply to every isolation mode. Discovery and the kernel
interface are the same whether the node stack runs inside an nspawn machine, on
the host, or in a container. The modes differ only in how the discovered devices
cross the isolation boundary.

| Requirement | What it means | Where it comes from |
| --- | --- | --- |
| AMD GPU hardware | A GPU supported by the host's `amdgpu` driver and by the ROCm release you run | Your hardware choice |
| Kernel driver | The `amdgpu` kernel module is loaded and bound to the GPU | Host distribution or your driver installation |
| KFD | The kernel exposes the Kernel Fusion Driver at `/dev/kfd` | The `amdkfd` component of the AMD driver stack |
| DRM nodes | Card and render nodes exist under `/dev/dri` | The `amdgpu` driver and host `udev` |
| Sysfs | `/sys/module/amdgpu`, `/sys/class/kfd`, `/sys/class/drm`, and `/sys/devices` are mounted and readable | The running kernel |
| Device plugin | The AMD Kubernetes device plugin runs on each AMD node | You install it separately |
| ROCm user space | The workload image contains the ROCm libraries that match the driver | Your application image |

### Supported hardware and drivers

Unbounded does not maintain a separate allowlist of AMD GPU models. The agent's
discovery is model-agnostic: it looks for the KFD device, then collects the DRM
card and render nodes that the kernel exposes. A GPU is usable when the host
kernel's `amdgpu` driver supports it and the ROCm release inside your workload
image supports it. The same path therefore applies to data center Instinct
accelerators and to Radeon cards, provided the installed driver and ROCm release
cover the model.

The driver comes from the host, not from Unbounded. Install it with the method
your distribution supports, keep the kernel modules and the ROCm user space
components on compatible versions, and confirm that the GPU is bound to
`amdgpu` before you ask Unbounded to provision the node. A host that has an AMD
display adapter but no loaded `amdgpu` driver looks like a host without AMD GPUs
to the agent: there is no KFD node to anchor discovery, so no AMD device grants
are produced.

The agent does not run `modprobe`, load kernel modules, install firmware, or
repair a half-installed driver. It also does not check for a minimum kernel
version. If a GPU needs a newer kernel than the host provides, the fix is a host
upgrade, not an agent setting. Use `lspci -k` on the host to confirm that the
device is present and that the kernel driver in use is `amdgpu`.

### Kernel requirements

The host must run a Linux kernel with AMD GPU support and KFD enabled. The
kernel must create `/dev/kfd` and the DRM nodes under `/dev/dri`, and it must
expose the AMD sysfs directories that describe the GPU and its KFD topology.
These are kernel responsibilities. A kernel that builds `amdgpu` without KFD, or
a host whose device nodes were never created by `udev`, cannot be repaired by
the agent.

The host kernel is also the kernel that the node stack uses in every isolation
mode. An nspawn machine and a container both share the host kernel, so upgrading
the rootfs image does not change the driver that the GPUs use. This matters when
the rootfs is newer than the host: the Kubernetes userland can be upgraded
independently, but a GPU that the host kernel cannot drive stays unavailable.

Load the driver before the node starts. A common failure is a host that boots
without the AMD modules and loads them later, after the nspawn machine or
container has already been created. In `nspawn` mode the machine configuration
is regenerated on the next start, so restarting the machine after the driver
loads is enough. In `container` mode the container must be recreated because
AMD device grants are fixed when it is prepared. In `none` mode the host-native
stack sees the devices as soon as they exist, but the device plugin still has to
notice them.

### The device plugin

The AMD Kubernetes device plugin is a cluster add-on, not part of the agent. It
runs as a pod on each AMD node, reads the KFD and DRM interfaces, registers the
node's GPU capacity with the kubelet, and returns the device nodes that a pod
needs when the scheduler assigns a GPU. Unbounded does not install, upgrade, or
configure this plugin. Install it from its upstream project with the settings
that project documents.

The plugin needs the same interfaces that the agent discovers. In `nspawn` and
`container` modes those interfaces exist inside the node stack because the agent
passed them through. The plugin's pod therefore sees them where it expects them,
provided the pod is scheduled onto the AMD node and is allowed to use the
devices. In `none` mode the plugin sees the host's own devices directly.

The official AMD device plugin advertises GPUs under the `amd.com/gpu` resource
name. If you use a different plugin or a GPU operator, use the resource name that
plugin documents. The name matters for scheduling and for the verification
commands later on this page.

A plugin that exits with `amdgpu driver unavailable` is reporting a host
problem, not a Kubernetes problem. It could not find the KFD interface it needs,
which usually means the driver is not loaded or `/dev/kfd` is missing from the
view of the pod. The troubleshooting sections below cover that case.

### What Unbounded does not provide

The scope difference is easier to see as a table. The left column is supplied by
Unbounded, and the right column is supplied by you or by the host.

| Unbounded provides | You or the host provide |
| --- | --- |
| Discovery of `/dev/kfd` and DRM nodes | The `amdgpu` driver and KFD |
| Device and sysfs grants for each isolation mode | The AMD Kubernetes device plugin |
| `Bind=` and `DeviceAllow=` rules in nspawn | ROCm libraries in workload images |
| OCI devices and cgroup rules in container mode | The runtime class or CDI configuration, if your plugin uses one |
| Native device visibility in `none` mode | Driver and firmware updates |

Unbounded publishes no AMD-specific rootfs image. NVIDIA hosts can receive an
image with the NVIDIA Container Toolkit, but the AMD path needs no toolkit in
the node stack, so the normal image for the host distribution is used. Image
selection considers whether NVIDIA devices were discovered; AMD discovery does
not change the image. A custom image is still allowed, but it does not need to
contain AMD drivers or ROCm for the passthrough to work.

## GPU Support by Isolation Mode

Discovery is identical in every isolation mode. The agent finds the KFD device,
the DRM nodes, and the AMD sysfs paths on the host, then turns them into
backend-neutral grants. Each isolation backend applies those grants with its own
primitives.

| Mode | How AMD devices reach the node stack | Sysfs access | Main caveat |
| --- | --- | --- | --- |
| `nspawn` | `Bind=` for device nodes and `DeviceAllow=<path> rwm` cgroup rules | Read-only `BindReadOnly=` mounts | A device added later appears at the next machine start |
| `none` | The node stack runs on the host and sees the devices natively | The host's own sysfs view | No remapping is possible |
| `container` | OCI device entries and per-device cgroup rules | Read-only OCI bind mounts | Grants are fixed at container creation; mode is experimental |

The table describes AMD specifically. The mode itself may have other
requirements, such as cgroup v2 or a writable `/etc` in container mode. Follow
the isolation mode page for those; this page covers only the GPU portion.

### nspawn

In `nspawn` mode the node stack runs inside a systemd-nspawn machine with its own
root filesystem. The host's `/dev` is not the machine's `/dev`, so a device that
is not explicitly bound does not exist inside the machine. The agent renders the
AMD devices into two generated files before the machine starts.

The machine's `.nspawn` file at `/etc/systemd/nspawn/<machine>.nspawn` receives
one `Bind=` line per discovered device node. It receives one `BindReadOnly=` line
per AMD sysfs path that exists on the host. The generated block looks like this
on a host with one AMD GPU:

```text
# AMD GPU support - bind-mount AMD device nodes into the container so the AMD
# Kubernetes device plugin can detect GPUs inside nspawn.
Bind=/dev/dri/card0
Bind=/dev/dri/renderD128
Bind=/dev/kfd
BindReadOnly=/sys/module/amdgpu
BindReadOnly=/sys/class/kfd
BindReadOnly=/sys/class/drm
BindReadOnly=/sys/devices
```

The service drop-in at
`/etc/systemd/system/systemd-nspawn@<machine>.service.d/override.conf` receives a
matching cgroup rule for each device node:

```text
# AMD GPU support - allow the container to access AMD device nodes via the
# cgroup device controller. The AMD Kubernetes device plugin needs these nodes
# visible and usable inside nspawn.
DeviceAllow=/dev/dri/card0 rwm
DeviceAllow=/dev/dri/renderD128 rwm
DeviceAllow=/dev/kfd rwm
```

Both halves are required. The bind mount makes the node exist inside the
machine, and the `DeviceAllow` rule makes it usable through the cgroup device
controller. A node that is bound but not allowed appears in a directory listing
and then fails when a process opens it. The sysfs mounts do not need
`DeviceAllow` entries because they are directories, not device nodes.

The machine configuration is not a one-time snapshot. The config regeneration
unit runs the agent's pre-start hook before every start of the machine, and that
hook resolves the host state again. A driver loaded after bootstrap, a GPU added
to the host, or a sysfs path that appeared later is therefore picked up on the
next start. When AMD devices are found, the agent logs `AMD GPU devices
detected, configuring nspawn bind-mounts` with the number of device paths it is
about to bind.

This regeneration behavior is also the remedy for a machine that started before
the AMD driver was ready. Restarting the machine is enough to regenerate the
configuration and bind the devices. Editing the generated files by hand is not
durable, because the next pre-start rewrites them from the host's current state.

### none

In `none` mode the node stack runs directly on the host. The kubelet and
containerd use the host's own device nodes and sysfs tree, so there is no bind
mount, no OCI device entry, and no device cgroup rule to write. An AMD device
that exists on the host is visible to the node stack at the same path.

The agent still checks the grants before it starts the stack. For a device grant,
the source must exist and any requested target must be the same path. For a
mount grant, the source must exist and the target must be the same path unless
the grant is one of the NVIDIA library or helper remaps. AMD grants use their
own host paths, so they satisfy that rule. A device that was discovered and then
disappears before the stack starts fails the check with the missing path. A host
that never had `/dev/kfd` is a different case: discovery produces no AMD grants
at all, so the stack starts as a CPU-only node.

Read-only cannot be enforced in `none` mode because nothing is mounted. The
agent normally logs a warning when a read-only mount grant is accepted without
enforcement. AMD sysfs grants are exempt from that warning: they exist only so a
tool can read the path, and the host's own copy is what the node stack reads.
There is nothing to enforce and nothing to remount.

The practical consequence is that `none` mode has the fewest moving parts and
the least isolation. A container on such a node still needs the device plugin to
inject devices into its namespaces, but the device plugin and the runtime see
the host's devices without any agent-created configuration. This mode is a good
fit when the host's own package management and runtime configuration are already
managed by your automation.

### container

In `container` mode the node stack runs in a privileged OCI container with a
private root. The agent renders the AMD grants into the OCI bundle as device
entries and read-only bind mounts, and writes matching cgroup device rules. The
bundle is at `/var/lib/unbounded/containers/bundles/<slot>/config.json`, where
`<slot>` is `kube1` or `kube2`.

The container starts with an allow-all device rule as its baseline, and the
agent adds an explicit allow rule for every granted AMD device node. The
explicit rules mean the devices keep working if the baseline is tightened later.
Read-only sysfs grants become read-only bind mounts. Unlike nspawn, there is no
`.nspawn` file and no systemd `DeviceAllow` line; the OCI runtime config is the
single source of truth.

Container mode is experimental and requires
`Experimental.ContainerIsolation` in the agent configuration. The AMD path
inherits that status. Treat it as provisional on Azure Container Linux and
verify it on hardware you can reprovision.

The most important limitation is that the set of AMD devices is fixed when the
container is prepared. The pre-start hook re-renders the bundle from the
recorded node specification, so it follows a device whose major and minor
numbers changed, and it fails the start if a recorded device has disappeared.
It does not rediscover AMD devices. Only the NVIDIA grant set is refreshed on
pre-start. A GPU or DRM node that appears after the container was prepared is
not added to the bundle, so it is not visible inside the container until the
container is recreated.

## How It Works

AMD support is split into three steps: discovery on the host, conversion into
backend-neutral grants, and application by the isolation backend. No step
requires the GPU to be idle, and no step writes a driver or a runtime
configuration. The result is a description of which host paths the node stack
must be able to see.

### Discovery at agent startup

Discovery is anchored on a single path. The agent stats `/dev/kfd`. If that path
does not exist, discovery returns an empty AMD state and stops. It does not fall
back to scanning `/dev/dri` on its own, because a DRM node without KFD is not a
usable AMD compute device for this path. This is why a host with a Radeon display
adapter and no `amdgpu` driver reports no AMD GPUs.

When `/dev/kfd` exists, the agent builds the device list in three steps:

1. Add `/dev/kfd` itself.
2. Read `/dev/dri`, skip directories, and add every entry whose name starts with
   `card` or `renderD`.
3. Sort the result so the generated configuration is stable across runs.

The scan deliberately accepts more than one card and more than one render node.
A multi-GPU host, or a host with several DRM functions, produces several
entries. The scan does not open the devices and does not query the driver. It
only relies on the device nodes that the kernel and `udev` created.

The sysfs side is filtered by existence. The agent considers four paths:

| Sysfs path | What it describes |
| --- | --- |
| `/sys/module/amdgpu` | The loaded AMD GPU module and its parameters |
| `/sys/class/kfd` | The KFD class and the topology the device plugin reads |
| `/sys/class/drm` | The DRM cards, render nodes, and connectors |
| `/sys/devices` | The device tree behind the class links |

Each path is added only if `stat` succeeds. A kernel that does not expose one of
them produces a smaller grant set rather than a failure. That tolerance is
useful on minimal or unusual kernels, but it also means a missing sysfs path can
degrade device plugin discovery without stopping the node from starting.

Discovery is independent of NVIDIA discovery. The two vendors have separate
resolution functions, and neither depends on the other's result. The only
interaction is the overlap exclusion described next.

### Grants and vendor overlap

Discovery produces an `AMDHost` value with two lists: the GPU device paths and
the sysfs paths. The agent converts those lists into grants:

| Grant | Purpose | Source | Read-only |
| --- | --- | --- | --- |
| Device | `amd-device` | Each `/dev/kfd` or DRM node | No |
| Mount | `amd-sysfs` | Each existing sysfs path | Yes |

The grants are appended after the host device, additional mount, and NVIDIA
grants. When the final grant list is built, AMD device paths that NVIDIA already
owns are removed. This matters on a mixed host because NVIDIA discovery also
collects the DRI nodes it needs for its CDI specification. Without the
exclusion, the same path would be bound twice and would receive duplicate device
rules. A path is compared as a string, so it is either owned by NVIDIA and
skipped, or it is not.

The exclusion is narrow. It only removes exact paths that NVIDIA reported. The
agent does not detect whether a remaining DRM node belongs to AMD, Intel, or
another vendor. If `/dev/kfd` exists, every non-directory `card*` or `renderD*`
entry that NVIDIA did not claim becomes an AMD device grant. On a host that
mixes an AMD compute GPU with another vendor's display adapter, the other
adapter can therefore be exposed as well. This is a deliberate limitation:
discovery avoids missing non-standard AMD layouts until sysfs vendor filtering is
validated across target hardware. The AMD vendor ID is `0x1002`, so you can
identify the real AMD nodes yourself when diagnosing a mixed host.

### nspawn rendering

The nspawn backend consumes the grants in `ApplyGrants`. Device grants become
entries in `AMDGPUDevicePaths`, and sysfs grants become entries in
`AMDSysFSPaths`. The `.nspawn` template emits `Bind=` for each device, then
`BindReadOnly=` for each sysfs path. The service override template emits a
`DeviceAllow=<path> rwm` line for each device path only. Systemd applies both
files when the machine starts.

The templates are rendered by the rootfs phase, which also writes the
config-regeneration unit. That unit runs the pre-start hook before the machine
starts. The hook loads the applied agent configuration, resolves the host state
again, and rewrites the machine files. This is why an AMD GPU that appears after
bootstrap is picked up by a machine restart, and why hand edits are not durable.

The device paths are emitted in the order discovery produced them, which is
sorted. The sysfs paths are emitted in the fixed order shown earlier, with
missing entries omitted. The exact order is not part of a contract, but it makes
diffs and log inspection predictable.

### container rendering

The container backend consumes the same grants in `RenderBundle`. A device grant
is described with `stat`, which yields the device type, major number, minor
number, and mode, and becomes an OCI device entry at the same path plus a cgroup
allow rule. A mount grant becomes an `rbind` mount, read-only when the grant asks
for it. The sysfs grants are read-only, so the container gets the same
read-only view that nspawn gets.

The bundle also contains the container's baseline. The renderer begins with an
allow-all device rule and adds explicit rules for the granted nodes. A device
node that exists at render time gets a rule; a device class or a node that does
not exist has no major and minor to describe and is covered by the baseline
instead.

The pre-start hook calls `Regenerate`, which reads the recorded node
specification and re-renders the bundle. The NVIDIA refresh hook replaces NVIDIA
grants at this point. AMD grants are not refreshed, so the device set is stable
for the life of the container. Re-rendering still stats each recorded device, so
a changed device number is followed and a removed device causes the start to
fail with the missing path.

## Exposed Host Paths

The same host paths are exposed in every mode where exposure is needed. In
`none` mode they are already present, so the table describes what the node stack
must be able to see rather than what the agent mounts.

### Device nodes

| Path | Purpose | Notes |
| --- | --- | --- |
| `/dev/kfd` | KFD compute interface | Anchor for all AMD discovery; without it no AMD grants are produced |
| `/dev/dri/card*` | DRM card nodes | Added when the entry exists and is not a directory |
| `/dev/dri/renderD*` | DRM render nodes | Added on the same rule; these are the nodes used for compute and rendering |

The agent does not create these nodes. They must exist on the host with
permissions that let the node stack open them. In `nspawn` mode the cgroup rule
grants read, write, and `mknod` access to the bound path. In `container` mode the
explicit device rule grants the same access. In `none` mode the host's own
permissions apply.

A missing card or render node does not fail discovery as long as `/dev/kfd`
exists. The device plugin may then see fewer GPUs than the host physically has,
or fail to initialize a device that has no render node. If the host has a GPU
but the expected nodes are absent, repair the driver and `udev` on the host
before changing anything in Unbounded.

### Sysfs paths

| Path | Typical use | If the path is missing |
| --- | --- | --- |
| `/sys/module/amdgpu` | Confirming the module and reading its parameters | The grant is omitted; plugin behavior depends on its version |
| `/sys/class/kfd` | KFD topology and GPU discovery | The grant is omitted; the plugin may not enumerate KFD topology |
| `/sys/class/drm` | Card and render node metadata | The grant is omitted; the plugin may not resolve DRM devices |
| `/sys/devices` | Resolving the device tree behind class links | The grant is omitted; topology lookups may be incomplete |

The exact set the plugin reads is defined by the plugin, not by Unbounded. The
agent passes through the paths that the kernel exposes. These are read-only
mounts in `nspawn` and `container` modes. The device plugin does not need to
write them, and making them read-only keeps a pod from changing host device
state through sysfs. In `none` mode the node stack sees the host's own sysfs
tree, including its permissions.

Because the agent adds only paths that exist at discovery time, a missing sysfs
entry is not reported as an error. Check for it explicitly when the device plugin
starts but cannot enumerate GPUs. A path that appears after the machine starts is
picked up when the nspawn configuration is regenerated on the next start. In
container mode a missing path at prepare time stays missing until the container
is recreated.

## Verification

Verify in three layers: the host, the node stack, and the cluster. A failure at
the host layer cannot be fixed by Kubernetes, and a failure at the cluster layer
is usually a missing device plugin rather than a missing device. Working from
the bottom up keeps the diagnosis short.

### Host checks

Start with the driver and the device nodes. These commands run on the host, not
inside the node stack.

```bash
ls -l /dev/kfd
ls -l /dev/dri
lsmod | grep -E '^(amdgpu|amdkfd)'
lspci -nnk | grep -iA3 -E 'vga|display|3d|processing accel'
```

A healthy host has `/dev/kfd`, at least one card node, and at least one render
node. The `lsmod` output should list both `amdgpu` and `amdkfd`; a host where
`amdgpu` is built into the kernel may not show module entries, in which case
check that the device nodes and sysfs paths exist instead. The `lspci` output
should show the AMD device with `Kernel driver in use: amdgpu`.

Confirm that the sysfs paths the agent looks for are present and readable:

```bash
ls -ld /sys/module/amdgpu /sys/class/kfd /sys/class/drm /sys/devices
```

On a mixed host, identify which DRM nodes are AMD before you interpret a
selection problem. The AMD PCI vendor ID is `0x1002`:

```bash
for d in /sys/class/drm/card*/device/vendor; do printf '%s ' "$d"; cat "$d"; done
```

A node whose vendor file reports `0x1002` is an AMD device. A node with a
different vendor is not, even though the agent's current discovery may still
pass it through when `/dev/kfd` exists. This is the check to use when a
non-AMD display adapter shows up in the AMD grant set.

You can also run the agent's preflight:

```bash
sudo unbounded-agent preflight
```

Preflight does not have an AMD-specific check. Its GPU diagnostic is the
`nvidia-driver` check, which succeeds with `no NVIDIA GPU hardware detected` on
an AMD-only host. A green run therefore means the rest of the host checks
passed; it does not prove that the AMD driver is healthy. Use the commands above
for that. See [Agent Preflight]({{< relref "reference/agent/preflight" >}}) for
the full check list and for how to make a check non-fatal during diagnosis.

### Node checks in every mode

`unbounded-agent exec` runs a command where the node stack runs, whatever the
isolation mode. These checks work on `nspawn`, `none`, and `container` nodes:

```bash
sudo unbounded-agent exec -- ls -l /dev/kfd
sudo unbounded-agent exec -- ls -l /dev/dri
sudo unbounded-agent exec -- ls -ld /sys/module/amdgpu /sys/class/kfd /sys/class/drm
```

In `none` mode the output is the host's own device layout. In `nspawn` and
`container` modes the output is the node stack's view, which should contain the
granted devices. A path that exists on the host but not in this output did not
cross the isolation boundary.

### nspawn configuration checks

On an nspawn node, inspect the generated machine files. The active slot is
usually `kube1`; during a blue/green upgrade it can be `kube2`, and both slots
have their own files.

```bash
sudo grep -E '^(Bind|BindReadOnly)=/dev/(kfd|dri)' /etc/systemd/nspawn/kube1.nspawn
sudo grep -E '^BindReadOnly=/sys/' /etc/systemd/nspawn/kube1.nspawn
sudo grep -E '^DeviceAllow=/dev/(kfd|dri)' /etc/systemd/system/systemd-nspawn@kube1.service.d/override.conf
```

The first command should show the KFD node and the DRM nodes. The second should
show the sysfs paths that existed at the last pre-start. The third should show a
rule for every device path. If the binds exist but the `DeviceAllow` lines do
not, the machine can see the nodes but cannot use them.

The agent logs the AMD decision at info level. Look for it in the node stack
logs or in the host journal for the config-regeneration unit:

```bash
sudo journalctl -u systemd-nspawn@kube1.service -n 100 --no-pager
```

The message `AMD GPU devices detected, configuring nspawn bind-mounts` includes
the count of device paths. If it is absent, discovery found no `/dev/kfd` at the
last start.

### container configuration checks

On a container node, inspect the OCI bundle. The bundle path is separate from
the rootfs and is not removed by a rootfs unpack.

```bash
sudo jq '.linux.devices' /var/lib/unbounded/containers/bundles/kube1/config.json
sudo jq '.linux.resources.devices' /var/lib/unbounded/containers/bundles/kube1/config.json
sudo jq '.mounts[] | select(.destination | startswith("/sys/"))' /var/lib/unbounded/containers/bundles/kube1/config.json
```

The device list should include `/dev/kfd` and the DRM nodes, the resource list
should contain allow rules for them, and the mount list should contain the
read-only sysfs binds. If the lists are empty, the container was prepared before
AMD devices were discovered or the grants were dropped during rendering. A
device that was added after preparation does not appear until the container is
recreated.

### Cluster checks

A node with a working host and a correct node stack still shows no AMD capacity
until the device plugin registers it. Check the node's allocatable resources:

```bash
kubectl get nodes -o 'custom-columns=NAME:.metadata.name,GPU:.status.allocatable.amd\.com/gpu'
kubectl describe node <node-name>
```

Replace `<node-name>` with the name of the node that has the AMD GPU. The
allocatable value should be a positive integer. If it is missing, the device
plugin is not running, is not scheduled on that node, or cannot see the KFD
interface. Check the plugin pods and their logs:

```bash
kubectl get pods -A | grep -i amd
kubectl logs <plugin-pod> -n <plugin-namespace>
```

The pod name, namespace, and labels depend on how you installed the plugin.
Use the upstream project's manifest for those details. If the plugin logs
`amdgpu driver unavailable`, return to the host checks; the plugin is reporting
that its view of `/dev/kfd` is missing.

To prove the full path, schedule a pod that requests one GPU and runs a ROCm
tool. Replace the image with one that contains the ROCm user space tools and
matches the driver on the host.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: amd-gpu-check
spec:
  restartPolicy: Never
  containers:
    - name: rocm
      image: <rocm-image-with-rocm-smi>
      command: ["rocm-smi"]
      resources:
        limits:
          amd.com/gpu: 1
```

Check the result with `kubectl logs amd-gpu-check`. A pod that stays `Pending`
with an insufficient GPU event is a scheduling problem. A pod that is created
but fails to start is usually a device injection problem: the plugin advertised
capacity, but the runtime could not create the device nodes the pod requested.

## Troubleshooting

The sections below cover failures that are specific to the AMD path. For host
and bootstrap problems that are not GPU-specific, see
[Agent Preflight]({{< relref "reference/agent/preflight" >}}) and
[Host Operations]({{< relref "guides/operations/host-operations" >}}).

### Device plugin exits with `amdgpu driver unavailable`

This message comes from the device plugin, not from the agent. It means the
plugin could not find the KFD interface it needs. The usual causes are a driver
that is not loaded, a `/dev/kfd` node that was never created, or a pod that
cannot see the node because the host path was not passed into the node stack.

Check the host first:

```bash
lsmod | grep -E '^(amdgpu|amdkfd)'
ls -l /dev/kfd
sudo dmesg | grep -i amdgpu | tail -50
```

If the modules are missing, install or load the AMD driver and reboot if the
installation requires it. If the modules are present but `/dev/kfd` is missing,
check `udev` and the kernel log for a probe failure. If both are present on the
host but missing inside the node stack, the isolation configuration is the
problem.

In `nspawn` mode, restart or recreate the machine so the pre-start regenerates
the configuration. In `container` mode, recreate the container so the bundle is
rendered with the current devices. In `none` mode there is no passthrough to
repair, so check that the plugin pod uses the host's `/dev` and that its
security context allows the device.

### Agent cannot find `/sys/module/amdgpu`

The agent checks for this path during discovery. If it is absent, the path is
simply omitted from the sysfs grants. This is not a bootstrap failure, but it
can leave the device plugin without the module information it expects.

Verify on the host:

```bash
ls -ld /sys/module/amdgpu
mount | grep ' /sys '
```

The path should exist on a host with the `amdgpu` driver loaded. If sysfs is not
mounted, mount it before starting the node. If the path is present on the host
but absent inside an nspawn machine, check the generated `.nspawn` file and
restart the machine. If another agent setting or a custom mount obscures the
path, remove that conflict rather than adding a duplicate bind.

Do not create a fake `/sys/module/amdgpu` directory. The device plugin reads real
kernel state, and a synthetic path produces misleading results.

### Agent cannot find device nodes

If the agent finds `/dev/kfd` but no DRM nodes, the AMD state contains only the
KFD device. Discovery does not fail, but the device plugin may not be able to
enumerate GPUs or may fail when it tries to open a render node.

Check the host:

```bash
ls -l /dev/dri
sudo dmesg | grep -i -E 'amdgpu|drm' | tail -50
```

The directory should contain card and render nodes. If it is empty, the driver
did not create them, `udev` did not process the device, or the GPU is bound to a
different driver. Fix the host before changing the agent. In `nspawn` mode a
restart regenerates the configuration after the nodes exist. In `container` mode
recreate the container.

### Agent cannot find `/dev/kfd`

This is the most consequential AMD failure. Without `/dev/kfd`, discovery
returns an empty AMD state, no AMD device grants are produced, and the node
provisions as if the host had no AMD GPUs. Nothing in the agent logs an error,
because a host without KFD is a valid CPU-only host.

Verify:

```bash
ls -l /dev/kfd
lsmod | grep -E '^(amdgpu|amdkfd)'
lspci -nnk | grep -iA3 -E 'vga|display|3d|processing accel'
```

If the GPU is present and bound to `amdgpu` but `/dev/kfd` is absent, the kernel
does not provide KFD. That can happen with a kernel built without AMD KFD support
or with a driver installation that did not load the full stack. Install a kernel
and driver combination that provides KFD, then reboot and confirm the node
exists before starting the agent again.

### Device nodes not accessible to the node container

When the nodes exist in the node stack's view but a process cannot open them,
the problem is usually the cgroup rule rather than the bind mount. In `nspawn`
mode, check both generated files and confirm that each device path in the
`.nspawn` file has a matching `DeviceAllow=<path> rwm` line in the service
drop-in. In `container` mode, check the OCI device entries and the device rules
in `config.json`.

On an SELinux-enforcing host, a denial can also make a visible node unusable.
Check the audit log for `avc: denied` entries that mention `kfd` or `dri`. The
agent labels a container node with `spc_t` when SELinux is enforcing, but a
custom policy can still deny access. Fix the policy on the host rather than
weakening the container.

Finally, check the permissions on the host nodes. A node that is readable only
by a group the container does not use behaves as if it were absent. Compare
`ls -l` on the host with `ls -l` inside the node stack.

### Node shows no AMD capacity

If the host and node checks pass and the node still has no allocatable GPUs, the
device plugin is the missing piece. Unbounded does not install it. Confirm that
a plugin pod is running on that node, that it is not crash-looping, and that its
logs show successful device discovery. Then check the node again with
`kubectl describe node`.

A plugin that runs but registers zero devices usually cannot see `/dev/kfd` or
the DRM nodes. A plugin that registers a device but cannot allocate it usually
has a runtime or permission problem. The two cases have different fixes, which
is why the node stack check comes before the cluster check.

### GPU disappears after a reboot or driver upgrade

A host reboot can change device minor numbers, reload modules in a different
order, or leave a driver unloaded until something probes the GPU. In `nspawn`
mode the next machine start regenerates the configuration and follows the new
device numbers. In `container` mode the pre-start regeneration follows changed
numbers for recorded paths, but it does not add a path that was not present when
the container was prepared. If a GPU moved to a different node path, recreate
the container.

After a driver upgrade, confirm that the module is loaded and that `/dev/kfd`
and `/dev/dri` exist before you restart the node. Restarting first and
diagnosing later only adds a failed unit to the investigation.

### Mixed AMD and NVIDIA host exposes unexpected DRM nodes

This is expected with the current discovery rule. When `/dev/kfd` exists, all
non-directory `card*` and `renderD*` entries under `/dev/dri` are collected,
except the exact paths that NVIDIA discovery already reported. Other vendors'
DRM nodes are not filtered by PCI vendor ID. Use the vendor file check shown in
the host verification section to identify the AMD nodes, and account for the
extra nodes when you read the device plugin's output.

## Known Limitations

The AMD path is intentionally smaller than the NVIDIA path. The limitations
below follow from that design, and most of them are visible in the code rather
than being deployment-specific.

| Limitation | What it means | What to do |
| --- | --- | --- |
| No AMD-specific rootfs image | The built-in image choice keys on NVIDIA discovery, not AMD | Use the normal image for the host distribution; put ROCm in the workload image |
| No AMD runtime or CDI setup | The agent does not register a runtime handler or generate a CDI spec | Use the device plugin's documented mechanism, or supply your own runtime configuration |
| No AMD preflight check | Preflight passes on an AMD host without validating the AMD driver | Run the host checks in this page before provisioning |
| Missing KFD is silent | A host without `/dev/kfd` is treated as a CPU-only host | Confirm `/dev/kfd` exists before the agent starts |
| No vendor filtering for DRM nodes | Every card and render node is exposed when KFD exists | Identify AMD nodes by the `0x1002` vendor ID on mixed hosts |
| No hotplug in container mode | The AMD device set is fixed at container creation | Recreate the container after adding or moving a GPU |
| No driver or library staging | The node stack never receives AMD user space libraries from the host | Ship matching ROCm libraries in the workload image |
| Container mode is experimental | The mode requires an explicit opt-in and has not been validated on all target hosts | Verify on reprovisionable hardware before production use |
| Racer GPU acceleration is CUDA-based | The AMD passthrough path does not make Racer's GPU implementation run on ROCm | Keep Racer GPU workloads on supported NVIDIA hardware; see [Racer Reference]({{< relref "reference/racer" >}}) |

The absence of a preflight check deserves emphasis. On NVIDIA, a host with
broken drivers fails bootstrap with an explicit GPU readiness error. On AMD,
the agent has no equivalent gate. It checks whether `/dev/kfd` exists, and it
does not run a vendor tool to prove that the GPU works. A host that passes
preflight but has a broken AMD driver therefore provisions a node that looks
healthy until the device plugin or a workload tries to use the GPU. Treat the
host checks in this page as mandatory before you bootstrap an AMD node, not as
optional diagnostics.

The lack of library staging is also deliberate. The agent cannot know which
ROCm release your application needs, and installing a host ROCm stack into the
node rootfs would couple the node image to the host driver. Keeping ROCm in the
workload image makes the dependency explicit and lets different applications use
different ROCm versions, provided each version supports the host driver.

## Related

- [GPU Reference]({{< relref "reference/gpu" >}}) for the vendor-neutral access
  model and the comparison between NVIDIA and AMD.
- [NVIDIA GPU Support]({{< relref "reference/gpu/nvidia" >}}) for the deeper
  integration, including driver roots, CDI, and the NVIDIA runtime.
- [nspawn Isolation]({{< relref "reference/agent/nspawn" >}}) for the machine
  layout, generated configuration, and lifecycle hooks.
- [none Isolation]({{< relref "reference/agent/isolation-none" >}}) for the
  host-native node stack.
- [Container Isolation]({{< relref "reference/agent/isolation-container" >}})
  for the experimental mode and its limits.
- [Agent Configuration]({{< relref "reference/agent/configuration" >}}) for
  `AdditionalHostDevices`, `AdditionalHostMounts`, and isolation settings.
- [Agent Preflight]({{< relref "reference/agent/preflight" >}}) for the host
  checks that run before bootstrap.
- [Racer Reference]({{< relref "reference/racer" >}}) for the GPU dataplane and
  its CUDA-based implementation.

