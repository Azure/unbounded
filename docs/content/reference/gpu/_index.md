---
title: "GPU Reference"
weight: 3
description: "How Unbounded nodes discover host GPUs, pass them through each isolation mode, and expose them to Kubernetes workloads."
---

Unbounded lets you run GPU workloads on Kubernetes without making the control
plane care where the hardware lives. A node joins the cluster the same way
whether it has eight GPUs or none, and the workloads that request a GPU are
scheduled by Kubernetes using the resources that node advertises. This section
explains the model behind that behavior, what the host must provide, how each
isolation mode changes the way devices cross the boundary into the node stack,
and where NVIDIA and AMD support differ.

The most important idea is that Unbounded is not a GPU orchestrator. It does not
partition GPUs, create MIG instances, virtualize devices, or decide which pod
gets which card. It also does not install a device plugin. What it does is carry
the host's existing driver and device access into the node stack it manages, so
the kubelet and container runtime on that node can use the GPUs as they would on
a conventional Kubernetes node. The rest of the GPU stack, including the device
plugin that advertises capacity, is supplied by the vendor ecosystem.

Read this page to build the mental model and to choose an isolation mode. Follow
the vendor page for the exact paths, images, commands, and troubleshooting steps:
[NVIDIA GPU support]({{< relref "reference/gpu/nvidia" >}}) and
[AMD GPU support]({{< relref "reference/gpu/amd" >}}).

## The GPU access model

GPU access starts on the host. Before the node stack starts, the agent inspects
the host for the devices, kernel driver state, userspace libraries, and vendor
tools that make up a usable GPU installation. That inspection happens at agent
startup and produces a vendor-specific view of the host. For NVIDIA, the view
includes device nodes, driver library directories, a driver version, and helper
binary paths. For AMD, it includes the KFD device, the DRM card and render nodes,
and the sysfs paths that the AMD device plugin reads.

The agent then starts the node stack, which is the kubelet, containerd, and the
supporting services that register the machine with your control plane. The
isolation mode decides where that stack runs. In `nspawn` mode it runs inside a
systemd-nspawn machine with its own root filesystem. In `none` mode it runs
directly on the host. In `container` mode it runs in a container with a private
root. In every mode, the agent has to make the host's GPU devices reachable from
where the stack runs, and that is the point where the modes diverge.

The mechanism that carries this access is a grant. A grant is a backend-neutral
request for a device node, a mount, or a library directory. The agent derives
grants from host discovery and hands them to the isolation backend, which
translates them into its own primitives: `Bind=` and `DeviceAllow=` entries for
`nspawn`, native visibility for `none`, and OCI device entries plus cgroup rules
for `container`. Grants are also how the agent keeps the modes honest. A grant
that names a path the host does not have fails rather than silently producing a
node without that device.

Once the stack is running, the kubelet registers the node with the control plane
and the container runtime creates containers on it. The GPUs appear to
Kubernetes only after a device plugin discovers them and advertises a resource
such as `nvidia.com/gpu`. This split matters when you debug: a node can have a
perfectly wired GPU stack and still show zero GPU capacity because no plugin is
running. The agent's responsibility ends at making the devices and driver
libraries usable; advertising and allocating them belongs to the plugin.

Because the agent passes devices through instead of emulating them, a GPU
workload runs against the real hardware and the real host driver. That keeps
performance predictable and avoids a second driver layer, but it also means the
workload image must be compatible with the driver installed on the host. A CUDA
image built for a much newer driver than the host runs will fail even when every
device node is present.

## What the host must provide

Every supported configuration starts with a working host GPU installation. The
kernel driver must be loaded, the device nodes must exist, and the userspace
libraries must be visible to the dynamic linker. Unbounded does not install,
upgrade, or repair drivers in any isolation mode. The `-nvidia` rootfs images
ship the NVIDIA Container Toolkit, not the kernel driver or the driver
libraries.

The physical GPU must be bound to its kernel driver. A card that is visible on
the PCI bus but claimed by another driver, or one whose module failed to load,
is not usable. On NVIDIA hosts the `nvidia-driver` preflight check reports PCI
devices that are present but not bound to the kernel driver. Unbounded's
inventory tooling can also record GPU details such as the PCI address, model,
serial number, VRAM, firmware, and driver version, but inventory is observation,
not configuration.

The driver and its userspace libraries must come from the same installation. A
container image does not provide the driver, and a stale library from an older
package can make the runtime generate a specification that names files which no
longer exist. This is a common cause of failures that look like a node problem
but are really a half-finished host upgrade. Keep the kernel module, the
userspace libraries, and the diagnostic tools on the same version.

For NVIDIA, the host must also pass a readiness gate. Before discovery accepts
the host, the agent runs `nvidia-smi -L` and requires at least one physical GPU.
A missing tool, a timeout, or output that does not describe GPUs fails
bootstrap. The host also needs the device nodes under `/dev` and, on systems
with NVSwitch, a running Fabric Manager. Persistence mode is recommended because
it keeps the driver loaded when no process holds the device, but the agent does
not enable it and only reports when the service is absent.

For AMD, the host must have the `amdgpu` driver loaded, `/dev/kfd` present, and
the DRM card and render nodes under `/dev/dri` present. The KFD device is the
anchor: if it is missing, the agent treats the host as having no AMD GPUs and
does not collect the DRM nodes. The sysfs paths used by the device plugin are
also read from the host, and the ones that exist are passed through with the
devices.

Neither vendor path replaces the host's package management. Install the driver
with the vendor's supported method, keep the matching userspace components with
it, and verify the host outside Kubernetes before you ask Unbounded to provision
a node. The preflight checks and the verification steps on the vendor pages are the
fastest way to separate a host problem from a node problem.

## How isolation modes change GPU passthrough

The isolation mode changes how the host's devices and libraries cross into the
node stack, not what the kernel must provide. Discovery runs on the host in every
mode and applies the same vendor prerequisites. What differs is who supplies the
container toolkit, whether the stack sees the host filesystem directly, and
which primitives the agent uses to grant access.

| Mode | Where the node stack runs | How GPUs reach it | Tooling source |
| --- | --- | --- | --- |
| `nspawn` | Inside a systemd-nspawn machine | Device binds, cgroup device rules, read-only library mounts, and a generated CDI spec | Rootfs image |
| `none` | Directly on the host | Native host devices and libraries, plus a CDI spec generated against `/` | Host installation |
| `container` | In a container with a private root | OCI device entries, cgroup rules, and read-only mounts | Rootfs image |

### systemd-nspawn

In `nspawn` mode the node stack runs in a machine with its own root filesystem,
so the host's device nodes do not exist inside it by default. The agent renders
the discovered devices into the machine configuration as `Bind=` entries and
into the service drop-in as `DeviceAllow=<path> rwm` rules. For NVIDIA it also
binds the host library directories read-only under `/run/host-nvidia/<index>` and
stages the host helper binaries under `/run/host-nvidia-bin`. AMD devices and
sysfs paths are bound in the same way, with the sysfs mounts read-only.

The machine configuration is regenerated when the machine starts. That means a
device or library directory that was added or removed on the host is picked up
on the next start, rather than requiring you to edit the machine by hand. For
NVIDIA, a post-start hook then links the host libraries into the rootfs, builds a
prepared driver root under `/run/nvidia/driver`, generates the CDI specification,
selects CDI mode in the NVIDIA runtime, and restarts containerd and kubelet
around the change. The rootfs image must be one of the Unbounded `-nvidia`
images, because that image supplies the toolkit.

This is the most complete GPU path and the one used for the published GPU
images. It is also the mode where library injection is most visible: the
machine sees the host's libraries through read-only mounts, and the prepared
driver root copies them into a self-contained tree that NVIDIA tooling can
inspect.

### none

In `none` mode the node stack runs directly on the host, so the kubelet and
containerd already see the host's device nodes and libraries. There is nothing to
bind and no driver root to assemble. The agent still has to prepare the
container runtime, which means the NVIDIA Container Toolkit must be installed on
the host rather than coming from an image.

If GPUs are present and the toolkit is missing, the agent fails the start with a
message that names the missing path and states that `none` mode requires the
toolkit on GPU hosts. The agent runs `nvidia-ctk` to select CDI mode and to
generate `/etc/cdi/nvidia.yaml` against the host's own driver installation. A
containerd drop-in repeats that preparation before every containerd start, so a
reboot or driver update does not leave containers running against a stale
specification.

`none` mode is the right choice when you already manage the host's container
runtime and toolkit and want the node stack to use them directly. It is also the
mode where host policy applies without translation: read-only library mounts
cannot be enforced because the stack is on the host, and a host path is a host
path. The `NvidiaToolkit` configuration block exists for this mode only. It lets
you point at a toolkit installed outside `/usr/bin`, and the agent rejects it
under `nspawn` or `container` because those modes use the toolkit in their
image.

### container

In `container` mode the node stack runs in a container with a private root. GPU
support mirrors the `nspawn` flow: the host's NVIDIA devices and library
directories become OCI device entries, cgroup rules, and read-only mounts, and a
post-start hook inside the container links libraries, builds the driver root,
generates the CDI specification, and restarts containerd and kubelet. The image
is the same `-nvidia` rootfs used by `nspawn`, so the toolkit comes from the
image.

GPU support in this mode is provisional. The flow is implemented and tested as
code, but it has not been validated against real GPU hardware, runc, or an Azure
Container Linux node. Device grants are fixed when the container is created,
with a pre-start refresh that rediscovers NVIDIA devices and libraries, so a
device that appears later requires a container restart to become visible. Treat
it as experimental and verify it on hardware you can reprovision.

### What stays the same

Across all three modes, the host kernel provides the driver and the real
devices, and Kubernetes allocates GPUs only through a device plugin. The modes
change the delivery mechanism, not the contract with the workload. A workload
that requests one GPU runs with the host's driver libraries in every mode; what
differs is whether those libraries arrived through a bind mount, through the
host filesystem, or through an OCI mount.

The modes also share the same failure shape. If the host is missing a driver,
the node either fails bootstrap with an explicit error or starts without usable
GPUs. If the stack is wired correctly but no device plugin runs, the node has no
allocatable GPU resource. If the runtime or CDI specification is stale, a pod
that requests a GPU is created but fails to start. Checking the host, then the
node stack, then the cluster in that order usually identifies which layer is at
fault.

## Driver and runtime expectations

The driver belongs to the host in every mode. The container image you run for a
workload does not carry the kernel driver, and the Unbounded `-nvidia` images do
not either: they add the NVIDIA Container Toolkit and leave `libcuda`,
`libnvidia-ml`, and the other driver libraries to the host. This separation lets
one image serve many driver versions, but it means the host is the source of
truth for what the workload can do.

The container toolkit, by contrast, is supplied differently depending on the
mode. In `nspawn` and `container` modes it comes from the `-nvidia` rootfs image,
which installs the toolkit package and provides `nvidia-ctk` and
`nvidia-container-runtime` at `/usr/bin`. In `none` mode the host must provide
those tools. The toolkit performs device and library injection when a container
starts; it is not a driver and cannot compensate for a missing or mismatched
kernel module.

The runtime contract is a CDI specification plus an NVIDIA runtime handler.
Containerd is configured with CDI enabled, the specification directories
`/etc/cdi` and `/var/run/cdi`, and a runtime named `nvidia` that runs
`nvidia-container-runtime` with `SystemdCgroup = true`. On a GPU machine the
`nvidia` runtime is also set as the default, so a pod does not need a
`RuntimeClass` to reach the driver. The CDI specification describes the devices,
the driver libraries, and the symlinks and linker cache entries that a container
needs. If the specification is empty, the runtime has nothing to inject even
though the host is healthy.

Unbounded does not install a device plugin. Kubelet advertises
`nvidia.com/gpu` only when an add-on such as the NVIDIA device plugin or a GPU
operator is running and registering the resource. The agent provides the parts
that add-on depends on: visible devices, host driver libraries, a working
`nvidia` runtime handler, and a CDI specification. If all of those are correct
and the node still reports no capacity, the missing piece is the plugin. The
same division exists on the AMD side, where the AMD Kubernetes device plugin is
a prerequisite that you install separately.

Architecture support is explicit. NVIDIA discovery supports `amd64` and `arm64`
and selects the library directory and linker tag from the agent binary's
architecture. Any other value fails discovery before a node starts. The
published `-nvidia` images are built for both architectures. AMD discovery does
not apply a separate architecture filter, but it still depends on the host's
driver and device layout being what the agent expects.

For image selection, the agent chooses a rootfs based on the configured image,
environment overrides, and finally a built-in default that considers the host
distribution and whether a GPU was discovered. A host with NVIDIA devices
normally receives an Ubuntu 24.04, Ubuntu 26.04, or Azure Linux 3 `-nvidia`
image. A host with no NVIDIA devices receives the non-NVIDIA image for its
distribution. If you force a CPU image onto a GPU host, the agent still finds the
devices but the rootfs has no toolkit, so the in-machine setup fails.

## NVIDIA compared with AMD

Both vendors use the same overall model: discover host hardware, pass it through
the isolation boundary, let the kubelet and runtime use it, and leave capacity
advertisement to a device plugin. The difference is how much vendor-specific
work Unbounded performs beyond passthrough.

NVIDIA support is the deeper integration. Discovery scans `/dev` for entries
whose names begin with `nvidia`, collects the DRI nodes, parses `ldconfig -p` for
driver libraries, determines the active driver version, and verifies readiness
with `nvidia-smi`. The agent can then materialize a prepared driver root,
generate a CDI specification, and register an `nvidia` runtime handler. It also
carries GPU-specific details such as the NVSwitch Fabric Manager requirement and
the `nvidia-fs` device nodes used by GPUDirect Storage. The result is a node
where GPU containers can start without a hand-written runtime configuration.

AMD support is narrower and more direct. Discovery is anchored on `/dev/kfd`.
When that node exists, the agent collects the DRM card and render nodes under
`/dev/dri` and passes them through as device grants, along with read-only sysfs
mounts for `/sys/module/amdgpu`, `/sys/class/kfd`, `/sys/class/drm`, and
`/sys/devices` when those paths exist. It does not stage driver libraries, build
a driver root, generate a CDI specification, or register a vendor runtime
handler. The AMD device plugin reads the exposed devices and sysfs paths to
discover GPUs and KFD topology, and the ROCm userspace libraries are not
installed by Unbounded, so provide them through your image or host setup.

That difference changes what you install. On NVIDIA, the host needs the driver
and either the toolkit (in `none` mode) or an Unbounded `-nvidia` image (in
`nspawn` and `container` modes). On AMD, the host needs the driver and device
nodes, the node needs the AMD Kubernetes device plugin, and the ROCm userspace
libraries must be installed by you. Both paths still require the GPU to be bound
to its kernel driver and both still require a device plugin before Kubernetes
can schedule GPU pods.

There is also a behavioral difference in device selection. NVIDIA discovery
stages only the devices and libraries it recognizes, and the generated
specification describes those devices. AMD discovery exposes every DRM card and
render node when `/dev/kfd` exists, which is broader than strictly necessary on
a host that mixes AMD GPUs with another vendor's display devices. The agent does
this deliberately today to avoid missing non-standard AMD layouts.

## Choosing a mode and a vendor

For most GPU deployments, start with `nspawn` and an Unbounded `-nvidia` image
on NVIDIA hardware. It is the most complete path, the image supplies the
toolkit, and the agent handles the driver root and CDI setup for you. Use `none`
when the host's own runtime and toolkit are already managed by your
configuration management and you want the node stack to use them directly.
Treat `container` as experimental for GPU workloads until you have validated it
on your hardware.

The vendor choice follows the hardware and the software you intend to run.
Unbounded passes through devices; it does not translate CUDA to ROCm or provide
a compatibility layer between vendors. If your application is built for CUDA,
use NVIDIA. If it is built for ROCm, use AMD and plan to supply the device plugin
and install the ROCm userspace libraries yourself. In both cases, verify the host
driver and devices before provisioning the node, and verify the device plugin
before concluding that the node is broken.

The vendor pages go deeper on each path. The NVIDIA page covers host
prerequisites, the prepared driver root, CDI generation, GPU images, GPUDirect,
verification, and troubleshooting. The AMD page covers the device and sysfs
model, the paths exposed to the node, and the device plugin failures you are
likely to see.

## Related

- [NVIDIA GPU support]({{< relref "reference/gpu/nvidia" >}}) for the complete
  NVIDIA host and node flow.
- [AMD GPU support]({{< relref "reference/gpu/amd" >}}) for the AMD device and
  sysfs model.
- [Agent Preflight]({{< relref "reference/agent/preflight" >}}) for host checks,
  including the `nvidia-driver` check.
- [nspawn Isolation]({{< relref "reference/agent/nspawn" >}}) for the machine
  layout, lifecycle hooks, and image selection.
- [none Isolation]({{< relref "reference/agent/isolation-none" >}}) for the
  host-native stack and its unit layout.
- [Container Isolation]({{< relref "reference/agent/isolation-container" >}}) for
  the provisional container mode.
- [Agent Configuration]({{< relref "reference/agent/configuration" >}}) for
  `NvidiaToolkit`, `OCIImage`, and isolation settings.
- [Racer Reference]({{< relref "reference/racer" >}}) for GPU dataplane settings
  used by the Racer workload path.
