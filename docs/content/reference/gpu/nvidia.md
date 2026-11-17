---
title: "NVIDIA GPU Support"
weight: 1
description: "How Unbounded discovers NVIDIA GPUs, passes the host driver into each isolation mode, prepares a driver root and CDI specification, and wires the NVIDIA container runtime."
---

NVIDIA support in Unbounded is a passthrough path with a vendor-specific setup
step. The host keeps its NVIDIA kernel driver, its device nodes, and its
userspace driver libraries. The agent discovers those pieces, carries them
across the isolation boundary, and then prepares the node stack so that a
container runtime can inject the driver into GPU containers. Unbounded does not
install the driver, partition the hardware, or advertise GPU capacity to
Kubernetes. Those jobs stay with the host installation, the NVIDIA device
plugin, and the workload image.

The division matters because it explains almost every failure you will see. A
node can have a perfect Unbounded configuration and still show zero allocatable
GPUs because no device plugin runs. A host can show every GPU in `nvidia-smi`
and still fail to start a GPU pod because the CDI specification was generated
before a driver upgrade. A GPU pod can start and then fail to load `libcuda`
because the specification it was started with does not name the current driver
libraries. This page separates those layers so you can tell which one is at
fault.

![NVIDIA GPU access across the host, the node stack, and containers](../../img/gpu-nvidia-host-container.svg)

Read this page if you are preparing an NVIDIA host, choosing an isolation mode
for GPU workloads, debugging a node that starts without GPUs, or working with
GPUDirect Storage and RDMA. It covers the host requirements that apply to every
mode, the way each mode exposes devices, the prepared driver root and CDI
specification that Unbounded builds, image selection, the boundaries around MIG
and time slicing, verification commands, and troubleshooting.

Start with [GPU Reference]({{< relref "reference/gpu" >}}) if you want the
vendor-neutral model first. The same model applies to
[AMD GPU support]({{< relref "reference/gpu/amd" >}}), which is a narrower
passthrough path without a driver root or CDI generation.

## Host requirements

Every requirement in this section applies to every isolation mode. Discovery and
the kernel interface are identical whether the node stack runs inside an nspawn
machine, directly on the host, or in a container. The modes differ only in how
the discovered devices and libraries cross into the node stack.

Unbounded never installs, upgrades, or repairs the NVIDIA driver. The `-nvidia`
rootfs images ship the NVIDIA Container Toolkit, not the kernel driver and not
`libcuda`. If the host driver is missing or half-installed, no image and no
agent configuration can compensate for it.

### Kernel driver and device nodes

The physical GPU must be bound to the `nvidia` kernel driver. A card that is
visible on the PCI bus but claimed by another driver, or one whose module failed
to load, is not usable. The `nvidia-driver` preflight check reads the vendor and
class of every PCI device, treats devices with vendor `0x10de` and a display or
3D class as NVIDIA GPUs, and reports an error when only some of them are bound to
`nvidia`.

The driver must also have created its device nodes and loaded its modules. The
preflight check reports the following severities, and discovery cannot produce a
usable GPU node without the error-level items:

| Requirement | Why it matters | Severity when missing |
| --- | --- | --- |
| `/dev/nvidia<N>` | One node per physical GPU; discovery anchors on these | Error |
| `/dev/nvidiactl` | Control device used by every CUDA context | Error |
| `/dev/nvidia-uvm` | Unified virtual memory, used by CUDA allocations | Error |
| `/dev/nvidia-uvm-tools` | UVM tools interface, used by diagnostics and some libraries | Warning |
| `/dev/nvidia-caps` | Capability device directory, including MIG capability nodes | Warning |
| `/dev/dri/renderD*` | DRI render nodes referenced by the generated CDI spec | Warning |
| `nvidia`, `nvidia_modeset`, `nvidia_uvm`, `nvidia_drm` modules | The loaded driver stack under `/sys/module` | Error |

The kernel modules matter beyond the command line. The agent binds
`/dev/nvidia-caps` and `/dev/nvidia-caps-imex-channels` as directories rather
than as individual files, so capability and IMEX channel nodes that the driver
creates later are still visible inside the node stack. If the modules are not
loaded at the time the machine starts, the devices do not exist to bind, and the
node comes up without usable GPUs. Load them with `nvidia-modprobe` or the
host's module configuration before you bootstrap the node.

On Azure Linux 3 hosts, discovery does this for you as part of readiness. When a
display-class NVIDIA PCI device is present, the agent runs `nvidia-modprobe -u
-c0` and `nvidia-modprobe -m`, then validates that a `/dev/nvidia<N>` node,
`nvidiactl`, `nvidia-uvm`, and `nvidia-modeset` exist. If `nvidia-modprobe` is
missing or the nodes still do not appear, bootstrap fails with a message naming
the command and the missing nodes. Other distributions are expected to load the
driver through their own configuration.

### Userspace libraries

The driver's userspace libraries must be visible to the dynamic linker. The
agent parses `ldconfig -p` and requires `libcuda.so.1` and `libnvidia-ml.so.1`
for the host architecture; the preflight check applies the same rule and also
verifies that the paths `ldconfig` reports actually exist. A library that
`ldconfig` lists but that has been deleted by a partial package upgrade is a
real failure, not a stale cache entry to ignore.

Discovery is broader than those two names. It collects the NVIDIA library
families that a GPU workload commonly needs, including the CUDA driver and
debugger libraries, `libnvidia-*`, the GLVND entry points, video decode and
OptiX libraries, and the VDPAU implementation in its conventional `vdpau`
subdirectory. Library discovery follows symlinks so that the real, versioned
files are included alongside the linker aliases. This matters because NVIDIA
tooling looks for names such as `libcuda.so.<driver-version>`, while
`ldconfig -p` often reports only the alias.

The driver version is read from the active `libcuda.so.1` or `libnvidia-ml.so.1`
target when possible, and falls back to `/sys/module/nvidia/version`. If no
version can be determined, the agent treats the NVIDIA state as incomplete and
refuses to start a GPU node. The version is not cosmetic: the prepared driver
root and the generated CDI specification both name versioned library files, and
a wrong version produces a specification that points at files which do not
exist.

On `amd64`, discovery also looks for 32-bit NVIDIA libraries with the same
driver version and copies them into the driver root when present. A missing
32-bit installation only produces a warning; it affects 32-bit applications, not
CUDA compute workloads.

### Diagnostic tooling and readiness

The host must be able to enumerate its GPUs. Before discovery accepts the host,
the agent runs `nvidia-smi -L` with a 20-second timeout and requires at least one
physical GPU line. A missing `nvidia-smi`, an error, a timeout, or output that
does not describe GPUs fails the readiness check, and the resulting error tells
you to repair the host driver and verify `nvidia-smi -L` before retrying
bootstrap.

The parser is deliberately strict. A physical GPU line must look like the output
`nvidia-smi` produces for a real card, with a GPU index and a `GPU-` UUID. Lines
describing MIG instances are accepted only after a physical GPU line, which
matches how `nvidia-smi` reports a MIG-enabled device. A host that prints only
MIG lines is rejected as invalid enumeration. This is a sanity check on the
driver, not a MIG configuration step.

Diagnostic tools are also staged for the driver root. The agent looks for
`nvidia-smi`, `nvidia-imex`, and `nvidia-imex-ctl` in `/usr/bin`,
`/usr/local/bin`, and `/bin`. When present, the host copies are placed in the
prepared driver root so GPU containers receive a version-matched `nvidia-smi`
rather than the one from the rootfs image, or none at all. The IMEX helpers are
included for DRA ComputeDomain workloads that run `nvidia-imex` from the driver
root.

### Fabric Manager on NVSwitch systems

NVSwitch-based multi-GPU systems, such as the eight-GPU H100, H200, and A100
ND-series machines, require the NVIDIA Fabric Manager service on the host. The
GPUs are connected through NVSwitch rather than through direct NVLink, and
without Fabric Manager the driver cannot bring the fabric up. CUDA
initialization then fails with error 802, `SYSTEM_DRIVER_MISMATCH`, even though
every device node and library is present.

Fabric Manager is a host responsibility and Unbounded does not install or check
it. The preflight check has no Fabric Manager test, because it cannot reliably
identify which SKUs require one. The Azure Linux host image does not include
Fabric Manager or IMEX either. On a system that needs it, install
`nvidia-fabricmanager` for the exact driver version, enable the
`nvidia-fabricmanager` unit, and verify it is running before you bootstrap the
node.

The helper script in the repository does this for Azure GPU virtual machines. It
maps VM SKUs to a GPU generation, a driver flavor, a driver version, and whether
Fabric Manager is required, installs the matching package, and enables the
service. If you install the driver another way, apply the same rule yourself:
Fabric Manager and the kernel driver must be the same version.

### Persistence mode

Persistence mode keeps the driver loaded when no process holds the GPU. It
reduces cold-start latency and avoids repeated driver initialization on nodes
that alternate between busy and idle. The preflight check looks for an
`nvidia-persistence-mode.service` unit and reports a warning when it is missing
or inactive; it does not enable it.

The project's Azure Linux host image ships such a unit. It runs
`nvidia-modprobe -u -c=0` and then `nvidia-smi -pm 1` after the module load, and
it is conditioned on a GPU being bound to the driver so it is harmless on hosts
without one. The Azure GPU VM helper script installs an equivalent unit and
orders it after Fabric Manager when that service exists. On other hosts, enable
persistence mode with the mechanism your distribution provides, or add a small
oneshot unit of your own.

Persistence mode is a recommendation, not a gate. A GPU node starts without it,
but the first GPU workload after an idle period pays for driver initialization,
and the Azure GPU VM helper enables it on every host it prepares because some
multi-GPU configurations expect it.

### Installing the driver

Use the vendor's supported method for your distribution and machine type. The
repository contains one worked example,
`hack/agent/azure-gpu-vm/install-nvidia-drivers.sh`, for Azure GPU virtual
machines. It takes a VM SKU and installs either the proprietary DKMS module for
older GPUs or NVIDIA's open kernel module for newer ones. When Secure Boot is
enabled, it installs Canonical's pre-built, pre-signed module package for the
running Azure kernel instead of building an unsigned DKMS module that the kernel
would reject.

The script ends by installing the persistence unit and, when the SKU map says
the machine needs it, Fabric Manager. It then reboots, because the kernel module
has to load before the device nodes appear. If you use the script, treat the
reboot as part of the installation rather than as an optional step.

## Discovery

Discovery runs on the host and produces a single value that every later step
consumes. It records the GPU device paths, the host directories that contain the
driver libraries, the mapping from each library to its path inside a private
root, the active driver version, and the helper binary paths. If any part of the
core state is missing while GPUs are present, discovery returns an error rather
than a partial result.

### Device discovery

Device discovery scans `/dev` for entries whose names begin with `nvidia`. It
skips directories at the top level and then adds two directory binds when they
exist: `/dev/nvidia-caps` and `/dev/nvidia-caps-imex-channels`. Those directories
are bound rather than enumerated so that capability and IMEX channel nodes
created after the node starts remain visible.

When at least one NVIDIA device was found, discovery also collects the entries
under `/dev/dri`. These are the DRI card and render nodes that the NVIDIA driver
creates, and the generated CDI specification refers to them. A GPU stack without
them can fail container creation with `ENOENT` even though the CUDA device nodes
are present. The final list is sorted so that the generated configuration is
stable across runs.

Discovery does not open the devices, query the driver for a device count, or
filter by PCI address. It uses the nodes that the kernel and `udev` created. That
keeps discovery fast and unprivileged, but it also means a driver that failed to
create a node is invisible to discovery, which is why the readiness check and
the preflight check exist.

### Library discovery

Library discovery starts from `ldconfig -p`. It filters the output to the host
architecture, using the `x86-64` tag on `amd64` and `aarch64` on `arm64`, so that
multilib entries from another architecture do not win. Entries whose names begin
with one of the recognized NVIDIA prefixes are kept, and duplicate basenames are
collapsed so that the linker's priority order decides which copy is used.

The agent then scans the directories those libraries live in for the real,
versioned files. It matches the CUDA driver and debugger libraries, the
`libnvidia-*` family, the GLVND entry points, video decode and OptiX libraries,
and the VDPAU implementation in its `vdpau` subdirectory. Symlinks are resolved
so that a target outside the alias directory, for example one managed by
`update-alternatives`, is also included.

Every unique host directory becomes a bind mount at `/run/host-nvidia/<index>`,
numbered in sorted order. The i386 directories, when present, use
`/run/host-nvidia-i386/<index>`. Each library mapping records three paths: the
host file that was discovered, the resolved host file that is actually mounted,
and the path inside the mount. A later step uses those paths to create symlinks
in the standard multiarch directory, so the linker inside the private root finds
the host's libraries at the names it expects.

### Architecture support

Support is explicit rather than best effort. The architecture of the agent
binary selects the linker tag and the multiarch directory:

| Agent architecture | ldconfig tag | Container library directory | 32-bit directory |
| --- | --- | --- | --- |
| `amd64` | `x86-64` | `/usr/lib/x86_64-linux-gnu` | `/usr/lib/i386-linux-gnu` |
| `arm64` | `aarch64` | `/usr/lib/aarch64-linux-gnu` | None |

Any other architecture fails discovery before a node starts, with an error naming
the unsupported value. This is deliberate: guessing a library path on an
untested architecture would produce a node that starts and then fails at the
first GPU workload.

### When discovery is complete

The agent treats NVIDIA state as available only when it has at least one GPU
device path, at least one library mapping, and a driver version. That triple is
the gate for every later step. A host with devices but no discoverable libraries,
or libraries but no readable driver version, fails the start instead of producing
a machine with a CDI specification that cannot work.

Discovery runs again in the lifecycle hooks. The nspawn pre-start hook re-resolves
the host configuration before the machine starts, and the post-start hook
re-resolves the GPU state before it rewires the machine. In `none` mode the agent
resolves the state when it starts the node and regenerates the CDI specification
on every containerd start. The practical consequence is that a device or library
directory added on the host is picked up by a restart, not by editing a
generated file.

## How GPUs reach the node stack

The isolation mode decides where the node stack runs, and therefore which
mechanism carries the host's devices and libraries into it. The host kernel still
provides the driver and the real devices in every mode. The mode changes the
delivery, not the contract with the workload.

| Mode | Where the node stack runs | How GPUs reach it | Where the toolkit comes from |
| --- | --- | --- | --- |
| `nspawn` | In a systemd-nspawn machine with its own root filesystem | Device binds, cgroup device rules, read-only library mounts, and a generated CDI specification | Rootfs image |
| `none` | Directly on the host | The host's own devices and libraries, plus a CDI specification generated against `/` | Host installation |
| `container` | In a privileged OCI container with a private root | OCI device entries, cgroup rules, and read-only mounts | Rootfs image |

All three modes end at the same interface: containerd with CDI enabled and an
`nvidia` runtime handler that runs `nvidia-container-runtime`. What differs is
how the driver files got to the place where that runtime looks for them.

### nspawn

In `nspawn` mode the node stack runs in a machine whose root filesystem is the
rootfs image, so the host's device nodes do not exist inside it by default. The
agent renders the discovered devices into the machine configuration and the
service drop-in.

The `.nspawn` file receives one `Bind=` line per discovered device path and one
`BindReadOnly=` line per library directory. The caps directories are bound as
directories. The service override receives a matching `DeviceAllow=<path> rwm`
line for each device node, because a bind mount alone makes the node visible but
the cgroup device controller still blocks access. For the two caps directories
the rule names the kernel device class, `char-nvidia-caps` or
`char-nvidia-caps-imex-channels`, so capability nodes created later are covered
by the same rule.

The read-only library mounts appear at `/run/host-nvidia/<index>` and
`/run/host-nvidia-i386/<index>`. The host directory that holds `nvidia-smi`,
`nvidia-imex`, and `nvidia-imex-ctl` is mounted read-only at
`/run/host-nvidia-bin`. That mount is a staging area, not an execution path: the
post-start hook copies the host-matched binaries into the driver root, and the
machine never runs them from the bind mount.

The machine also receives `/run/udev` read-only when NVIDIA devices are present.
cuFile uses the udev database to resolve properties of the host's block devices,
so GPUDirect Storage reads can identify the backing device correctly. Without
that mount, cuFile can fail to find the device attributes it needs even though
the character devices are present.

After the machine is up, its post-start hook does the driver work. It waits for
the machine to answer commands, stops the kubelet and containerd inside it,
rebuilds the NVIDIA state, then starts containerd, reimports any staged container
image archives, and starts the kubelet. Stopping the consumers first is what
makes the rewiring safe: containerd holds the CDI specification and the runtime
configuration, and the kubelet would otherwise keep creating pods against the old
state.

The hook is a no-op on a host without NVIDIA devices, and it refuses to continue
when devices were found but the state is incomplete. A machine that fails during
this step fails its unit, and the generated drop-in restarts it after ten seconds
with the start-rate limit disabled, so a transient failure does not leave the
node permanently failed.

The rootfs must be one of the `-nvidia` images. The in-machine setup runs
`nvidia-ctk`, and a plain CPU image does not have it. If you force a CPU image
onto a GPU host, the host still discovers the devices, the machine starts, and
the post-start hook fails when it cannot run the toolkit. That failure is
reported as a failed start rather than a node with silently missing GPUs.

### none

In `none` mode the kubelet and containerd run directly on the host, so they
already see the host's device nodes and libraries. There is nothing to bind and
no private root in which to assemble a driver root. What GPU workloads still need
is the NVIDIA Container Toolkit and a CDI specification that describes the
devices present at that start.

The toolkit is not shipped in this mode. Before the stack starts, the agent
checks for `nvidia-ctk` and `nvidia-container-runtime` at their configured paths,
which default to `/usr/bin/nvidia-ctk` and `/usr/bin/nvidia-container-runtime`.
If GPUs are present and either path is missing, preparation fails with a message
that names the missing path and states that `none` requires the NVIDIA Container
Toolkit on GPU hosts. The agent does not try to install it.

Use the `NvidiaToolkit` configuration block when the toolkit is installed
somewhere else, for example `/usr/local/nvidia/toolkit`. Both fields are
absolute, clean paths without `..`, and the block is valid only with
`Isolation: none`. In any other mode the agent rejects the configuration with
`NvidiaToolkit is only supported with Isolation "none"`, because those modes run
the toolkit from their rootfs image. See
[Agent Configuration]({{< relref "reference/agent/configuration" >}}) for the
field reference.

The preparation itself is two `nvidia-ctk` commands:

1. `nvidia-ctk config --set nvidia-container-runtime.mode=cdi --in-place`
   selects CDI mode, so requests expressed through environment variables resolve
   against the generated specification.
2. `nvidia-ctk cdi generate --driver-root / --dev-root / --output
   /etc/cdi/nvidia.yaml --disable-hook all --enable-hook create-symlinks
   --enable-hook update-ldcache` generates the specification from the host's own
   driver installation.

The hook selection is deliberately narrow. Only `create-symlinks` and
`update-ldcache` are enabled, because the other upstream hooks assume a private
root or make changes that are unnecessary, or harmful, when the driver is the
host's own. The same selection is used in `nspawn`, where the driver root is
private.

The agent runs those commands before it starts the stack. The agent-owned
containerd unit also runs them as `ExecStartPre` entries on every start, through
the `10-gpu-setup.conf` drop-in. That second part matters after a reboot or a
driver upgrade: systemd can start containerd on boot before the agent runs, and
a GPU container started against a stale specification would inject libraries
that no longer exist. A failing `ExecStartPre` fails the containerd start, and
the unit's restart policy retries it.

Grant handling in `none` is native. A device node must already be at its own
path, because the stack has no private root in which to create it. A mount whose
source and target are the same path is satisfied by checking that the source
exists. The NVIDIA library and helper-directory grants that `nspawn` places under
`/run/host-nvidia/...` are satisfied by the host view, since the libraries are
already on the loader path and the helpers are already on `PATH`.

Two grant shapes cannot be honored. A device or mount that asks for a different
target is refused, because the host cannot be remapped for one node. A
library-path grant is refused outright, because the node uses the host's library
configuration and the agent cannot change it per node. A read-only mount grant is
accepted but not enforced, and the agent logs a warning rather than pretending
otherwise; do not rely on `ReadOnly` to protect a host path in this mode.

A reset removes the CDI specification, which the agent generated, and leaves the
toolkit's runtime configuration, which belongs to the toolkit installation. A
driver update changes what the specification must contain, so restart the stack
or repave the node after a driver update rather than assuming the next agent start
is enough.

### container

In `container` mode the node stack runs in a privileged OCI container with a
private root. GPU support mirrors the `nspawn` flow with different primitives:
the host's NVIDIA devices become OCI device entries with matching cgroup rules,
the library and helper directories become read-only bind mounts at
`/run/host-nvidia/...`, and a post-start hook inside the container links the
libraries, builds the driver root, generates the CDI specification, and restarts
containerd and kubelet. The image is the same `-nvidia` rootfs that `nspawn`
uses, so the toolkit comes from the image.

The OCI bundle is the single source of truth. There is no `.nspawn` file and no
systemd `DeviceAllow` line; the runtime configuration carries the device nodes,
the cgroup allow rules, and the mounts. The bundle lives at
`/var/lib/unbounded/containers/bundles/<slot>/config.json`, where `<slot>` is
`kube1` or `kube2`.

GPU support in this mode is provisional. The flow is implemented and tested as
code, but it has not been validated against real GPU hardware, runc, or an Azure
Container Linux node. Treat it as experimental and verify it on hardware you can
reprovision.

The NVIDIA grant set is the one thing that is refreshed at pre-start. The
pre-start hook reruns discovery and replaces the recorded NVIDIA devices and
library mounts, so a driver change or a host reboot is followed the way the
`nspawn` post-start hook follows it. A node that was provisioned without NVIDIA
grants is left alone: adding a GPU to a CPU-provisioned node needs a new rootfs,
not a refresh. A GPU that appears after the container was created becomes visible
after the container is restarted.

## Prepared driver root

In the modes with a private root, Unbounded assembles a self-contained driver
root at `/run/nvidia/driver` and points NVIDIA tooling at it. The root is what
makes `nvidia-ctk` work inside a rootfs image that does not contain the driver.
Without `--driver-root`, the tool would inspect the image's own filesystem, find
missing or mismatched libraries and helper binaries, and either fail or generate
a specification for the wrong files.

The layout is small and predictable:

| Path | Contents |
| --- | --- |
| `/run/nvidia/driver/lib/<multiarch>` | The host's 64-bit driver libraries, including real files named `libcuda.so.<version>` and `libnvidia-ml.so.<version>` |
| `/run/nvidia/driver/lib/i386-linux-gnu` | Matching 32-bit libraries when the host has them |
| `/run/nvidia/driver/usr/lib/<multiarch>` | A relative symlink to `../../lib/<multiarch>` |
| `/run/nvidia/driver/usr/bin` | Host-matched `nvidia-smi`, `nvidia-imex`, and `nvidia-imex-ctl` when present |
| `/run/nvidia/driver/sbin/ldconfig` | A symlink to the machine's `/sbin/ldconfig` |
| `/run/nvidia/driver/etc/ld.so.conf` | Linker search directories inside the root |
| `/run/nvidia/driver/etc/ld.so.cache` | The linker cache built from that configuration |

The root is rebuilt on every start. The task removes the old directory, creates
the directory skeleton, and copies each discovered library from its read-only
mount with `cp -aL`. Copying rather than symlinking is intentional: the root must
work when NVIDIA tooling mounts or inspects it at another path, and a link back
into the host mount would not resolve there.

Some host installations expose only `.so` and `.so.1` names even after the
filesystem scan. For each library family, the task picks the best available copy
and materializes a real file named with the active driver version. It then
validates that `libcuda.so.<version>` and `libnvidia-ml.so.<version>` exist and
are regular files, failing the start otherwise. NVIDIA tooling rejects a SONAME
alias where it expects a versioned file, so this check turns a confusing
generation error into a clear start failure.

The linker configuration lists `/lib/<multiarch>` and `/usr/lib/<multiarch>`,
plus the i386 directories when 32-bit libraries were found. The task runs
`ldconfig -r /run/nvidia/driver`, which builds `etc/ld.so.cache` inside the root
and lets the tooling resolve SONAME aliases without touching the machine's own
cache. The paths in `etc/ld.so.conf` are interpreted relative to the root passed
with `-r`; they do not refer to the machine's real `/lib`.

The helper binaries are optional. `nvidia-smi` is copied when discovery found it,
and the resulting specification exposes a version-compatible diagnostic tool to
GPU containers. Compute-only CUDA workloads do not need it. The IMEX binaries are
copied when present because DRA ComputeDomain daemon pods run `nvidia-imex` from
the driver root at `/run/nvidia/driver`.

Once the root is valid, the agent generates the specification:

```bash
nvidia-ctk cdi generate \
  --driver-root /run/nvidia/driver \
  --dev-root / \
  --output /etc/cdi/nvidia.yaml \
  --disable-hook all \
  --enable-hook create-symlinks \
  --enable-hook update-ldcache
```

`--dev-root /` is not a mistake. The device nodes keep their normal, bind-mounted
paths inside the private root, so the device discovery root is the root
filesystem while the driver discovery root is the prepared tree. Disabling every
hook and re-enabling only `create-symlinks` and `update-ldcache` avoids hooks that
mount a tmpfs over `/proc` paths or apply CUDA compatibility logic that does not
fit this arrangement. On NVSwitch systems those extra hooks are a common source
of failures that look like driver problems.

The finished specification is written to `/etc/cdi/nvidia.yaml` inside the
machine or container. It describes the GPU device nodes, the driver libraries,
the DRI nodes, and the linker changes a container needs. If it is empty or names
devices that no longer exist, the runtime has nothing useful to inject even
though the host is healthy.

## CDI and the containerd runtime

The runtime contract has two halves: a CDI specification that lists the devices
and driver files a container needs, and a containerd runtime handler that runs
the NVIDIA runtime. Both are created by the agent, and both live on the node that
runs the containers.

Containerd is configured to read CDI specifications from `/etc/cdi` and
`/var/run/cdi`, with CDI enabled. The agent adds a drop-in at
`/etc/containerd/conf.d/99-nvidia-runtime.toml` on a GPU machine:

```toml
[plugins."io.containerd.grpc.v1.cri"]
cdi_spec_dirs = ["/etc/cdi", "/var/run/cdi"]
enable_cdi = true

[plugins."io.containerd.grpc.v1.cri".containerd]
default_runtime_name = "nvidia"

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.nvidia]
runtime_type = "io.containerd.runc.v2"

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.nvidia.options]
BinaryName = "/usr/bin/nvidia-container-runtime"
SystemdCgroup = true
```

The main containerd configuration imports `/etc/containerd/conf.d/*.toml`, so the
drop-in applies whether the node uses the machine's `/etc/containerd/config.toml`
or the agent-owned configuration under `/etc/unbounded/containerd` in `none`
mode. When the NVIDIA runtime is not enabled, the drop-in is removed rather than
left behind, so a reconfigured node does not keep a runtime handler that points
at a missing binary.

The handler is named `nvidia`, uses the runc v2 shim, and runs
`nvidia-container-runtime` with systemd cgroups. On a GPU machine it is also the
default runtime, so a pod does not need a `RuntimeClass` to reach the driver.
The agent does not create a `RuntimeClass` object. If you want pods to select the
runtime explicitly, create a `RuntimeClass` whose handler is `nvidia`; the
handler name is the part the node provides.

The runtime binary is upstream NVIDIA software. So is `libnvidia-container`,
which performs the actual device and library injection when a container starts.
Unbounded configures them and points them at the specification; it does not patch
or wrap their composition. That boundary matters when you debug an injection
failure: the node-side files are the CDI specification, the runtime binary path,
and the drop-in. The injection itself is NVIDIA's code.

The toolkit is always put into CDI mode explicitly. Every flow runs
`nvidia-ctk config --set nvidia-container-runtime.mode=cdi --in-place` before it
generates the specification, in `nspawn`, in `container`, and on the host in
`none`. That mode is what lets environment-variable requests from the device
plugin resolve against the generated specification, so the two mechanisms do not
disagree about where device information comes from. Do not switch the runtime
back to legacy mode by hand; environment-variable requests would then stop
resolving against the generated specification.

## Images

The node rootfs comes from an OCI image. The `-nvidia` variants extend the base
images with the NVIDIA Container Toolkit, which is what the in-machine or
in-container setup needs. They do not contain the kernel driver, `libcuda`, or
the driver libraries; those come from the host in every mode.

| Image | Base | When it is the built-in default |
| --- | --- | --- |
| `ghcr.io/azure/agent-ubuntu2404-nvidia:v20261031` | Ubuntu 24.04 | Every other host with NVIDIA GPUs, including Ubuntu 24.04 and unrecognized distributions |
| `ghcr.io/azure/agent-ubuntu2604-nvidia:v20261031` | Ubuntu 26.04 | Ubuntu 26.04 host with NVIDIA GPUs |
| `ghcr.io/azure/agent-azlinux3-nvidia:v20261031` | Azure Linux 3 | Azure Linux 3 or Azure Container Linux host with NVIDIA GPUs |

The image is chosen from the first source that is present. An explicit `OCIImage`
in the agent configuration wins. If it is empty, `AGENT_DISABLE_OCI_IMAGE` can
disable image usage, and `AGENT_OCI_IMAGE` can name an image without editing the
configuration. When none of those is set, the agent picks a default based on the
host distribution and on whether NVIDIA devices were discovered. A host with
NVIDIA devices normally receives one of the images above; a host without them
receives the non-NVIDIA image for its distribution.

Each image installs the `nvidia-container-toolkit` package from NVIDIA's
repositories: the apt repository with a signed keyring on the Ubuntu images, and
the rpm repository on the Azure Linux image. The package brings in `nvidia-ctk`,
`nvidia-container-runtime`, and the `libnvidia-container` components. Their
versions are whatever the repository resolves at build time, which is why the
image can be updated independently of the host driver. Nothing in the image
loads a kernel module or provides a driver library.

That separation is the design, not an accident. A single image can serve hosts
with different driver versions because the driver always comes from the host.
The cost is that a host driver upgrade changes the files the CDI specification
must name, so the specification is regenerated on the next node start rather than
baked into the image.

Do not confuse the node images with the host images. The
`images/host-azurelinux3-nvidia` artifact is a Metalman machine image for an
Azure Linux 3 host. It installs `cuda-open-hwe`, configures the module load list,
and installs the persistence-mode unit. It deliberately does not include Fabric
Manager or IMEX. It is a host operating system image, not a node rootfs, and it
is never selected by the rootfs image logic.

If you force a CPU image onto a GPU host, discovery still succeeds and the
machine still starts. The in-machine setup then fails when it cannot execute the
toolkit, which is reported as a failed node start. Check the image name before
you debug the driver in that situation.

## MIG and time slicing

Multi-Instance GPU and time slicing are partitioning schemes that sit above the
device. MIG divides one physical GPU into isolated instances with their own
memory and compute slices. Time slicing lets several workloads share one GPU by
switching between them. Kubernetes sees either arrangement through the device
plugin, which advertises different resources and decides which pod gets which
instance or share.

Unbounded implements neither feature. There is no agent configuration field for a
MIG profile, an instance ID, a replica count, or a time-slicing interval. The
agent does not create or destroy MIG instances, and it does not configure the
device plugin. What it does is regenerate the CDI specification from the host
driver on every node start, which means the specification reflects whatever MIG
configuration the host has at that moment.

That leads to one practical rule. If you enable, disable, or reshape MIG
instances on a running host, restart the node before you schedule workloads that
expect the new layout. The specification on disk was generated at the last start
and names the devices that existed then. `none` mode is slightly better here
because the containerd unit regenerates the specification before every start, so
restarting containerd is enough to pick up a MIG change.

Readiness tolerates MIG, but it does not configure it. `nvidia-smi -L` on a
MIG-enabled host prints a physical GPU line followed by one line per instance,
and the readiness parser accepts the instance lines after a GPU line. Output that
contains only MIG lines, without a physical GPU line, is rejected as invalid
enumeration. The check is asking whether the driver can enumerate its hardware,
not whether MIG is set up the way you intended.

MIG capability nodes live under `/dev/nvidia-caps`. That directory is bound into
the private root as a directory, and the cgroup rule names the
`char-nvidia-caps` class, so capability nodes created after the machine starts
remain usable. That support is about device visibility, not about instance
management.

Time slicing is further from the agent's view. It is a feature of the device
plugin, which advertises several replicas of one GPU to the scheduler and gives
each replica access to the same device. Unbounded does not configure it, and it
does not set `NVIDIA_VISIBLE_DEVICES` for ordinary pods; that is the device
plugin's job. Because the node's runtime is the NVIDIA runtime in CDI mode, the
mechanism the plugin relies on is present, but the decision to share a GPU is
entirely upstream.

Neither scheme is a security boundary that Unbounded adds. MIG provides hardware
isolation between instances. Time slicing does not: pods share the same GPU and
the same memory, and a misbehaving workload can affect its neighbors. If you need
tenant isolation, use MIG or separate nodes; do not expect the isolation mode to
provide it. The `nspawn` and `container` modes isolate the node stack from the
host, not GPU workloads from each other.

## GPUDirect Storage and GPUDirect RDMA

GPUDirect Storage lets a GPU read directly from local NVMe instead of staging
through host memory. GPUDirect RDMA lets a NIC write directly into GPU memory.
Both paths bypass a host copy, and both depend on host kernel support that
Unbounded does not provide. Unbounded's contribution is to make the relevant
device nodes and udev information visible to the node stack.

### Device nodes and udev

GPUDirect Storage uses `/dev/nvidia-fs*` character devices created by the
`nvidia-fs` kernel module. Discovery collects every `/dev` entry whose name
begins with `nvidia`, so those nodes are discovered like any other NVIDIA device
and bound into the private root. The `.nspawn` configuration receives a `Bind=`
line and the service override receives a matching `DeviceAllow=<path> rwm` rule.
In `none` mode the host's own nodes are already visible, and in `container` mode
they become OCI device entries.

cuFile also needs to know which block device backs a file. It reads the host's
udev database for that, which is why `/run/udev` is mounted read-only into the
private root whenever NVIDIA devices are present. Without it, a file can be
readable and the character devices can be present while cuFile still fails to
resolve the device attributes it needs.

Both private-root modes treat `nvidia-fs` devices exactly like GPU devices. There
is no separate configuration switch for them, and no validation that the module
is loaded. If the nodes exist, they are bound; if they do not, the node simply
has no GPUDirect Storage devices.

### Installing the storage module

The host must load `nvidia-fs`. The Azure GPU VM helper script installs it when
you pass `--gds`. That path has two hard requirements: the SKU must use NVIDIA's
open kernel module, and Secure Boot must be disabled. The module is built with
DKMS, which produces an unsigned module that a Secure Boot host rejects unless
you enroll a machine owner key. The script refuses rather than leaving you with a
module that cannot load.

The installation takes `nvidia-fs-dkms` from NVIDIA's CUDA apt repository and
pins that repository so only `nvidia-fs` and the DKMS version it requires come
from it; the driver continues to come from Ubuntu. It then writes
`/etc/modules-load.d/nvidia-fs.conf` so the module loads at boot. As with the
driver itself, a reboot is required, and the script does it. Verify after the
reboot with `lsmod | grep nvidia_fs`, `ls -la /dev/nvidia-fs*`, and
`cat /proc/driver/nvidia-fs/stats`.

There is an important limit. On a stock distribution kernel, installing
`nvidia-fs` alone does not enable GPUDirect reads from local NVMe, because the
patched peer-to-peer interface that the module expects is part of the
MLNX_OFED-patched kernel, not the stock one. The alternative is the PCI
peer-to-peer DMA path in cuFile 1.13 and later, which needs no `nvidia-fs`. The
Racer dataplane exposes that as `RACER_GPU_PCI_P2PDMA`, default `false`. When it
is `true`, the dataplane asks cuFile to use PCI P2PDMA before the driver opens.
If cuFile cannot set the parameter, the open fails and cuFile stays off for that
process, so reads fall back to host memory rather than failing the workload. See
[Racer Reference]({{< relref "reference/racer" >}}) for the setting and
[Racer: vLLM for GPUs]({{< relref "guides/racer-vllm" >}}) for the tuning
context.

The GPU-side path is not the only thing that determines whether a read is direct.
The file must be on a supported filesystem, aligned to the record boundaries the
driver expects, and openable by cuFile on that device. A file that does not meet
those conditions is read through host memory instead. That fallback is deliberate
and is why a misconfigured GPUDirect path usually shows up as lower throughput,
not as an error.

### RDMA landing

GPUDirect RDMA is used by the Racer dataplane for peer transfers. When GPU
decryption and RDMA are both enabled, each decrypting GPU gets a landing buffer
sized by `RACER_GPU_LANDING_BYTES`, 256 MiB per GPU by default, and `0` disables
it. The dataplane exports the buffer as a Linux dma-buf and registers slices of
it with each RDMA rail through `ibv_reg_dmabuf_mr`, so a peer's NIC can write
ciphertext straight into GPU memory.

The host prerequisites are outside the agent. The kernel driver must support
dma-buf export, and the RDMA stack must support registering a dma-buf as a
memory region. The project's Azure Linux host image installs NVIDIA's open
kernel module through `cuda-open-hwe`, the same open-module flavor that GPUDirect
Storage requires. The image also omits `nvidia_peermem` from its initramfs, so
that module is not loaded during early boot; load it at runtime if your RDMA
provider needs it.

Failure is soft by design. If the dataplane cannot allocate, export, or register
a landing buffer on a rail, that rail and GPU stay host-only and transfers fall
back to host memory. A partially working GPU path is preferred over a workload
that does not start. The metrics that show this are per rail and per GPU, so a
node that works but is slower than expected is worth checking there before you
look at the driver.

The agent's role in both paths is the same as everywhere else on this page: the
device nodes and the udev database reach the node stack, the driver root and CDI
specification describe the driver, and the workload's container runtime injects
what it needs. If `nvidia-fs` is missing, no specification change can create the
device; if the host kernel lacks dma-buf support, no agent setting can add it.

## Verification

Verify in layers, from the host outward. Each layer answers a different question,
and skipping a layer usually sends you to the wrong place. If the host cannot
enumerate GPUs, nothing above it can work. If the host is healthy but the node
stack has no specification, the problem is in the node, not the driver. If both
are healthy but Kubernetes has no GPU resource, the missing piece is the device
plugin.

### On the host

These commands run on the host, before or after bootstrap:

| Command | What a healthy result looks like |
| --- | --- |
| `nvidia-smi -L` | One `GPU <n>: ... (UUID: GPU-...)` line per physical GPU, optionally followed by MIG lines |
| `ls -la /dev/nvidia*` | `nvidia0`, `nvidiactl`, `nvidia-uvm`, and the caps directories exist |
| `ldconfig -p \| grep -E 'libcuda\.so\.1\|libnvidia-ml\.so\.1'` | Both libraries resolve to existing paths for the host architecture |
| `modinfo -F version nvidia` | A driver version, matching the versioned library files |
| `systemctl status nvidia-persistence-mode.service` | Active on hosts that use persistence mode |
| `systemctl status nvidia-fabricmanager` | Active on NVSwitch systems |
| `lsmod \| grep nvidia` | `nvidia`, `nvidia_modeset`, `nvidia_uvm`, and `nvidia_drm` are loaded |

If a GPU is present but `nvidia-smi -L` fails, stop here. Reinstalling the agent
or recreating the node will not fix a host driver that cannot talk to its
hardware. The same is true for a driver version that does not match the versioned
library files: finish the host upgrade or roll it back, then start the node.

### With preflight

The agent's preflight command runs the host checks without mutating anything. It
reports `nvidia-driver` results when NVIDIA hardware is present, and a single
success result when none is found, or a warning if `/dev` cannot be inspected:

```bash
sudo unbounded-agent preflight --output text
```

Use `--output json` when you want to feed the result into a script. The
`nvidia-driver` check can produce several results under one name, one per target
such as `NVIDIA PCI driver`, `NVIDIA kernel modules`, `NVIDIA device nodes`,
`NVIDIA driver libraries`, `NVIDIA diagnostic tooling`, and `NVIDIA persistence
service`. Preflight reports problems but does not enforce readiness the way a
real start does, which is exactly what makes it useful before you provision. See
[Agent Preflight]({{< relref "reference/agent/preflight" >}}) for the full list
of targets and severities.

### Inside the node stack

`unbounded-agent exec` runs a command where the node stack runs, whatever the
isolation mode. The mode is read from the applied configuration, so the same
command works on an `nspawn`, `none`, or `container` node:

```bash
sudo unbounded-agent exec -- ls -l /dev/nvidia*
sudo unbounded-agent exec -- cat /etc/cdi/nvidia.yaml
# nspawn and container modes
sudo unbounded-agent exec -- systemctl status containerd
# none mode, where the agent owns the unit
sudo unbounded-agent exec -- systemctl status unbounded-containerd
```

On a healthy node the device listing shows the same nodes as the host, the CDI
specification is not empty and names the current driver version, and containerd
is active. In `nspawn` mode, `exec` enters the machine; in `none` mode it runs on
the host; in `container` mode it enters the container.

The runtime configuration is worth reading as well. On `none` and `container`
nodes the drop-in is at `/etc/containerd/conf.d/99-nvidia-runtime.toml`; on
`nspawn` nodes the same path exists inside the machine. It should enable CDI,
list `/etc/cdi` and `/var/run/cdi`, name the `nvidia` runtime, and point
`BinaryName` at `nvidia-container-runtime`. On a GPU node it also sets the
runtime as the default.

For an `nspawn` machine, the host-side generated files show what was bound and
allowed:

```bash
grep -E 'nvidia|udev' /etc/systemd/nspawn/kube1.nspawn
grep DeviceAllow /etc/systemd/system/systemd-nspawn@kube1.service.d/override.conf
```

The first command should show one `Bind=` line per device, the library
directories under `/run/host-nvidia/`, and the read-only `/run/udev` mount. The
second should show a matching `DeviceAllow=` line for every device node, plus the
`char-nvidia-caps` class rules for the capability directories.

The node stack journal is the place to look when a start failed:

```bash
sudo unbounded-agent logs --unit containerd --lines 200
sudo journalctl -u systemd-nspawn@kube1.service -n 200
```

### In the cluster

A healthy node is not the same as a schedulable GPU node. Kubernetes sees GPUs
only after a device plugin advertises them:

```bash
kubectl get node <node-name> -o jsonpath='{.status.allocatable.nvidia\.com/gpu}{"\n"}'
kubectl describe node <node-name> | grep -A5 'Allocatable'
```

An empty value or a zero means no plugin has registered the resource, even if
every command in the previous sections succeeded. Install the NVIDIA device
plugin or the GPU operator, then check again. Unbounded does not install or
configure either one.

A minimal GPU pod proves the path end to end. Replace `<cuda-image>` with an
image that contains `nvidia-smi`, and use the resource name your plugin
advertises:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-check
spec:
  restartPolicy: Never
  containers:
    - name: check
      image: <cuda-image>
      command: ["nvidia-smi", "-L"]
      resources:
        limits:
          nvidia.com/gpu: "1"
```

If the pod is created but the container fails to start, the runtime could not
resolve the CDI device, which points at the specification or the runtime handler.
If the pod starts and `nvidia-smi` reports no devices, the container did not
receive the device nodes, which points at the injection path. If the pod stays
pending, the scheduler has no allocatable GPU, which points at the device plugin.

## Troubleshooting

Most NVIDIA failures on an Unbounded node fall into a small number of categories.
The sections below start with the error you are most likely to see.

### CUDA error 802 (`SYSTEM_DRIVER_MISMATCH`)

Fabric Manager is not running on an NVSwitch system, or it is running at a
different version from the kernel driver. Every device node, library, and CDI
specification can be correct while CUDA still refuses to initialize. Check the
service on the host:

```bash
systemctl status nvidia-fabricmanager
journalctl -u nvidia-fabricmanager -n 100
```

Install the package that matches the installed driver version, enable the unit,
and restart the node. Upgrading the driver without upgrading Fabric Manager
produces the same error, so treat them as a pair.

### `nvidia-ctk cdi generate` fails with `ERROR_LIBRARY_NOT_FOUND`

In `nspawn` or `container` mode this usually means the host libraries did not
reach the private root. Check, inside the node stack, that the bind mounts exist
and contain the expected files:

```bash
sudo unbounded-agent exec -- ls -l /run/host-nvidia/0
sudo unbounded-agent exec -- ldconfig -p | grep libcuda
```

If `/run/host-nvidia/0` is empty or missing, the bind mount was not generated
(the `.nspawn` file in `nspawn` mode, the OCI bundle in `container` mode), which
usually means discovery found no libraries. Run `ldconfig -p` on the host and
confirm the NVIDIA libraries are listed. If the mount exists but the library is
not on the loader path, the symlink step did not run or the driver root was built
from a different directory set; restart the machine so the post-start hook
rebuilds it.

In `none` mode there is no bind mount to check. The equivalent failures are a
toolkit that is not installed, a driver root that is not `/`, or a host
`ldconfig` that does not list the driver libraries.

### The CDI specification has no device entries

An empty or device-less specification means NVIDIA tooling could not see the
devices when it generated the file. Start with the host:

```bash
ls -l /dev/nvidia0
nvidia-smi -L
sudo unbounded-agent exec -- grep -c 'name:' /etc/cdi/nvidia.yaml
```

If `/dev/nvidia0` is missing, the driver is not loaded or the device nodes were
never created; reboot after installing the driver or load the modules with
`nvidia-modprobe`. If the device exists but the specification is empty, check
whether the in-machine generation ran at all. In `nspawn` and `container` modes it
is part of the post-start hook, so a hook failure leaves the previous or no
specification. In `none` mode it runs before the stack starts and again before
every containerd start, so inspect the `10-gpu-setup.conf` drop-in and the
containerd journal for a failing `ExecStartPre`.

### The node fails with `required NVIDIA host state is unavailable`

Discovery found devices or NVIDIA display hardware but readiness failed. The
wrapped error names the reason, which is usually a missing `nvidia-smi`, a
timeout, or output the parser did not accept. Run `nvidia-smi -L` on the host and
read the output literally. A driver that is loaded but wedged, a containerized
host environment that hides `/dev`, or a `nvidia-smi` from a different driver
version all produce this result.

This error is a refusal, not a warning. The agent will not start a node that it
cannot verify, because a node that starts without usable GPUs is harder to
diagnose than one that refuses to start.

### The node fails with `NVIDIA is required but resolved setup state is incomplete`

The machine was provisioned with NVIDIA capability, but the resolved state lacks
one of the three required pieces: device paths, library mappings, or a driver
version. This is the same underlying condition as the previous error, seen at a
later phase. Compare `ls /dev/nvidia*`, `ldconfig -p`, and
`/sys/module/nvidia/version` on the host. A common cause is a driver package
upgrade that replaced the kernel module but not the userspace libraries, or the
reverse.

### A GPU node starts but advertises no `nvidia.com/gpu`

The node stack is healthy and the device plugin is missing or failing. Check the
plugin's pods and logs, not the agent. This is the most common false alarm on a
correctly built GPU node: every host and node check passes, and the cluster still
shows zero capacity because nothing registered the resource.

The same answer applies to MIG and time-sliced resources. The names
`nvidia.com/mig-*` and a time-sliced replica count come from the plugin, so a
missing or misconfigured plugin is the first thing to inspect.

### Containers fail to create with an unresolved CDI device

containerd fails container creation when a CDI device cannot be resolved. This is
why the Racer dataplane makes its CDI annotation opt-in with
`RACER_NVIDIA=cdi`: on a node without an NVIDIA specification, the annotation
would break the DaemonSet. On your own workloads, check that the requested device
name exists in `/etc/cdi/nvidia.yaml` and that the specification was generated
after the last driver change. A pod that requests a device name the node does not
have is a scheduling or naming mistake, not an injection failure.

### Missing toolkit in `none` mode

The error names the missing path and says that `none` requires the NVIDIA
Container Toolkit on GPU hosts. Install the toolkit, or set
`NvidiaToolkit.CTKPath` and `NvidiaToolkit.ContainerRuntimePath` if it is
installed outside `/usr/bin`. Both paths must be absolute and clean, and the
block is rejected in any isolation mode other than `none`.

### The toolkit configuration is rejected in `nspawn` or `container`

`NvidiaToolkit is only supported with Isolation "none"` means exactly what it
says. Those modes run the toolkit from the rootfs image, so a host path would
have no effect. Remove the block or switch the node to `none`.

### Driver changes are not reflected

A driver upgrade changes the library version and the files the CDI specification
must name. The agent regenerates the specification at the next node start, not
while the node is running. Restart the machine or the node stack after the
upgrade, and verify that the specification names the new version. In `none` mode
the containerd unit regenerates it on every start, so a containerd restart is
enough; in the private-root modes the post-start hook does it, so restart the
machine or container.

If a host reboot is involved, remember that the generated `.nspawn` configuration
is refreshed by the pre-start hook, which also runs discovery again. A GPU that
was added while the host was down is picked up at the next machine start.

### A machine fails and restarts repeatedly

The post-start hook fails the machine unit when the NVIDIA rewiring fails, and
the generated drop-in restarts it after ten seconds with the start-rate limit
disabled. A repeated failure is therefore a repeated, deterministic error, not a
transient one. Read the unit journal for the hook's last error. The most common
causes are a missing toolkit in the rootfs, a driver version that could not be
determined, and host libraries that disappeared between discovery and the hook.

### Warnings that are safe to ignore

A missing 32-bit NVIDIA library installation on `amd64` produces a warning and
does not affect 64-bit CUDA workloads. A missing `nvidia-uvm-tools` node,
capability nodes, or DRI render nodes produces a preflight warning; investigate
them if a workload that needs graphics, video, or MIG fails, but they do not
block a compute node from starting. A missing persistence service is also a
warning; the node works, but the first GPU job after idle pays for driver
initialization.

## Related

- [GPU Reference]({{< relref "reference/gpu" >}}) for the vendor-neutral access
  model and how to choose an isolation mode.
- [AMD GPU support]({{< relref "reference/gpu/amd" >}}) for the passthrough path
  without a driver root or CDI generation.
- [Agent Preflight]({{< relref "reference/agent/preflight" >}}) for every host
  check, including the `nvidia-driver` targets and severities.
- [nspawn Isolation]({{< relref "reference/agent/nspawn" >}}) for the machine
  layout, generated files, and lifecycle hooks that carry the GPU setup.
- [none Isolation]({{< relref "reference/agent/isolation-none" >}}) for the
  host-native stack, its units, and its GPU and CDI preparation.
- [Container Isolation]({{< relref "reference/agent/isolation-container" >}}) for
  the provisional container mode and its OCI bundle.
- [Agent Configuration]({{< relref "reference/agent/configuration" >}}) for
  `NvidiaToolkit`, `OCIImage`, and the isolation settings.
- [Racer Reference]({{< relref "reference/racer" >}}) for `RACER_GPU_PCI_P2PDMA`,
  GPU landing buffers, and the other dataplane settings.
- [Racer: vLLM for GPUs]({{< relref "guides/racer-vllm" >}}) for the workload
  path that uses these GPU features.
