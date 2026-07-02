---
title: "systemd-nspawn Isolation"
weight: 3
description: "How the unbounded-agent uses systemd-nspawn to isolate Kubernetes worker node components."
---

The unbounded-agent runs all Kubernetes worker node components inside a
[systemd-nspawn](https://www.freedesktop.org/software/systemd/man/latest/systemd-nspawn.html)
container. This provides lightweight OS-level isolation (separate filesystem,
PID, and cgroup namespaces) while sharing the host kernel. The container runs
a full systemd init tree so that kubelet, containerd, and their dependencies
are managed as regular systemd services, decoupled from anything running on the
host.

![Host and nspawn container boundary: unbounded-agent, machinectl, and systemd-nspawn on the host; systemd, containerd, kubelet, CNI plugins, and pod containers inside the nspawn container; shared network namespace across both](../../../img/nspawn-host-container-boundary.svg)

## Why nspawn

Running kubelet directly on the host requires careful package management and
risks conflicting with existing services. An nspawn container gives the agent a
clean, reproducible environment:

- **Filesystem isolation.** Kubernetes binaries, configuration, and runtime
  state live inside the container's rootfs and do not interfere with the host.
- **Process isolation.** The container has its own PID namespace. A full
  systemd instance (PID 1 inside the container) supervises containerd and
  kubelet with automatic restart policies.
- **Cgroup isolation.** The container gets its own cgroup subtree. Nested
  containers (pods) created by runc operate under cgroups v2 inside this
  subtree.
- **Shared network namespace.** The container shares the host's network stack
  (`VirtualEthernet=no`), so kubelet uses the host's routable IP directly and
  CNI plugins can create interfaces (WireGuard tunnels, overlay bridges) that
  are visible on the host.
- **Simplified upgrades.** Upgrading Kubernetes is a matter of replacing the
  container rootfs and restarting the nspawn machine. The host OS remains
  untouched, eliminating the need to coordinate package upgrades across the
  host and the node stack.

## Container Rootfs

The rootfs is a plain directory tree at `/var/lib/machines/<MachineName>`. It
is created during the **rootfs** bootstrap phase by pulling a pre-built OCI
image via ORAS and unpacking it into the machine directory. A GPU variant
includes the NVIDIA Container Toolkit.

After the rootfs exists, the agent downloads Kubernetes binaries (kubelet,
kubectl, kube-proxy), containerd, runc, and CNI plugins directly into
the rootfs before the container boots. Configuration files (containerd
config, kubelet service units, bootstrap kubeconfig, CA certificates, hostname,
and `resolv.conf`) are also written into the rootfs at their standard
absolute paths (e.g. `/var/lib/machines/<MachineName>/etc/containerd/config.toml`
becomes `/etc/containerd/config.toml` inside the container).

### OCI Images

The default rootfs image is selected automatically based on the host distro and
whether NVIDIA GPUs are detected on the host. Supported automatic matches are
Ubuntu 24.04, Ubuntu 26.04, and Azure Linux 3.0. RPM-based hosts fall back to
Azure Linux 3.0. Other unknown host distros fall back to Ubuntu 24.04.

| Image | Default repository | Description |
|---|---|---|
| [`agent-ubuntu2404`](https://github.com/Azure/unbounded/pkgs/container/agent-ubuntu2404) | `ghcr.io/azure/agent-ubuntu2404` | Base image with systemd, dbus, curl, iproute2, nftables, kmod, wireguard-tools, and bpftool. ([Containerfile](https://github.com/Azure/unbounded/tree/main/images/agent-ubuntu2404/Containerfile)) |
| [`agent-ubuntu2404-nvidia`](https://github.com/Azure/unbounded/pkgs/container/agent-ubuntu2404-nvidia) | `ghcr.io/azure/agent-ubuntu2404-nvidia` | Extends the base image with the NVIDIA Container Toolkit (`nvidia-ctk`, `nvidia-container-runtime`). ([Containerfile](https://github.com/Azure/unbounded/tree/main/images/agent-ubuntu2404-nvidia/Containerfile)) |
| [`agent-ubuntu2604`](https://github.com/Azure/unbounded/pkgs/container/agent-ubuntu2604) | `ghcr.io/azure/agent-ubuntu2604` | Ubuntu 26.04 base image with systemd, dbus, curl, iproute2, nftables, kmod, wireguard-tools, and bpftool. ([Containerfile](https://github.com/Azure/unbounded/tree/main/images/agent-ubuntu2604/Containerfile)) |
| [`agent-ubuntu2604-nvidia`](https://github.com/Azure/unbounded/pkgs/container/agent-ubuntu2604-nvidia) | `ghcr.io/azure/agent-ubuntu2604-nvidia` | Ubuntu 26.04 image with the NVIDIA Container Toolkit (`nvidia-ctk`, `nvidia-container-runtime`). ([Containerfile](https://github.com/Azure/unbounded/tree/main/images/agent-ubuntu2604-nvidia/Containerfile)) |
| [`agent-azlinux3`](https://github.com/Azure/unbounded/pkgs/container/agent-azlinux3) | `ghcr.io/azure/agent-azlinux3` | Azure Linux 3.0 base image with systemd, dbus, curl, iproute, nftables, kmod, wireguard-tools, and bpftool. ([Containerfile](https://github.com/Azure/unbounded/tree/main/images/agent-azlinux3/Containerfile)) |
| [`agent-azlinux3-nvidia`](https://github.com/Azure/unbounded/pkgs/container/agent-azlinux3-nvidia) | `ghcr.io/azure/agent-azlinux3-nvidia` | Azure Linux 3.0 image with the NVIDIA Container Toolkit (`nvidia-ctk`, `nvidia-container-runtime`). ([Containerfile](https://github.com/Azure/unbounded/tree/main/images/agent-azlinux3-nvidia/Containerfile)) |

The agent pins a specific image tag by default at build time. The `OCIImage`
field in the agent config can override the full image reference for custom or
pinned builds.

Image sources are maintained in the
[images/](https://github.com/Azure/unbounded/tree/main/images)
directory of the repository.

## Machine Configuration

The agent configures the nspawn machine to share the host network namespace
(`VirtualEthernet=no`) so that kubelet uses the host's routable IP directly and
CNI plugins can manage interfaces visible on the host. It also makes
`/proc/sys/net` writable inside the container so that CNI plugins and
kube-proxy can set network sysctls, while the rest of `/proc/sys` stays
read-only. Cgroups v2 is forced inside the container to ensure consistent
behavior regardless of the systemd version in the rootfs.

The default `systemd-nspawn@.service` launcher may still show flags such as
`--network-veth`. Those flags come from the systemd template, but the generated
`.nspawn` file is written to the trusted `/etc/systemd/nspawn/` directory and
the template runs with `--settings=override`. As a result, the generated
`VirtualEthernet=no` setting is authoritative and the machine shares the host
network namespace. Host interfaces, host firewall and routing rules, and
loopback listeners are therefore visible from inside the nspawn machine.

When GPUs are detected on the host, the agent automatically exposes the host
paths needed by the corresponding Kubernetes device plugin:

- **NVIDIA GPUs.** Bind-mounts GPU device nodes and host driver libraries,
  grants cgroup device permissions, generates a CDI spec, and configures the
  NVIDIA container runtime. See [NVIDIA GPU Support]({{< relref "reference/gpu/nvidia" >}}).
- **AMD GPUs.** Bind-mounts `/dev/kfd` and DRM device nodes, grants cgroup
  device permissions, and exposes AMD sysfs paths read-only so the AMD
  Kubernetes device plugin can discover GPUs inside nspawn. See
  [AMD GPU Support]({{< relref "reference/gpu/amd" >}}).

When virtualization device nodes are present on the host, the agent
automatically bind-mounts them into the container and grants cgroup device
permissions so that workloads inside the container can run virtual machines:

- `/dev/kvm` for hardware virtualization (for example QEMU/KVM).
- `/dev/net/tun` for TAP interfaces used by VM networking.
- `/dev/vhost-net` for accelerated VirtIO networking.

The agent also auto-mounts host storage and InfiniBand hardware:

- **Block (storage) devices.** Every entry under `/sys/class/block` is
  bind-mounted by its device node (e.g. `/dev/sda`, `/dev/sda1`,
  `/dev/nvme0n1`, `/dev/nvme0n1p1`), including whole disks and their
  partitions as well as device-mapper (`/dev/dm-*`) and software RAID
  (`/dev/md*`) nodes. Pseudo and virtual devices are excluded: `loop*`,
  `ram*`, `zram*`, `fd*`, and `sr*` (optical).
- **InfiniBand HCA devices.** Every character device under
  `/dev/infiniband` (e.g. `uverbs0`, `umad0`, `issm0`, `rdma_cm`) is
  bind-mounted so that RDMA workloads inside the container can reach the
  host's HCAs.
- **Configured extra devices.** `spec.agent.additionalHostDevices` can list
  non-standard host device nodes under `/dev` (for example `/dev/uinput`) that
  should be exposed to workloads inside the machine.

Device discovery runs once when the machine is provisioned. Disks or HCAs
hot-plugged after the machine has started are not picked up until the machine
is re-provisioned or soft-rebooted.

The configuration is written to two files on the host before the machine boots:

| File | Path |
|---|---|
| nspawn config | `/etc/systemd/nspawn/<MachineName>.nspawn` |
| Service override | `/etc/systemd/system/systemd-nspawn@<MachineName>.service.d/override.conf` |

### Customization points

| Setting | File | Purpose |
|---|---|---|
| `Capability=all` | nspawn config | Grants all capabilities for nested container runtimes (runc). |
| `PrivateUsers=no` | nspawn config | Disables user namespace remapping so runc can use real root. |
| `SystemCallFilter=@keyring bpf perf_event_open` | nspawn config | Allows kernel keyring, eBPF, and perf event syscalls used by containerd, runc, and eBPF CNIs. |
| `VirtualEthernet=no` | nspawn config | Shares the host network namespace. |
| `Bind=/run/bpffs/<MachineName>:/sys/fs/bpf` | nspawn config | Exposes a machine-scoped bpffs mount to eBPF CNIs such as Cilium. |
| bpffs `ExecStartPre=` mount commands | Service override | Creates and mounts the machine-scoped host bpffs before the machine starts. |
| `SYSTEMD_NSPAWN_UNIFIED_HIERARCHY=1` | Service override | Forces cgroups v2 inside the container. |
| `SYSTEMD_NSPAWN_API_VFS_WRITABLE=network` | Service override | Makes `/proc/sys/net` writable for CNI and kube-proxy. |
| `Bind=/dev/kvm` | nspawn config | KVM device bind-mount (auto-generated when `/dev/kvm` is present). |
| `Bind=<block device>` | nspawn config | Storage block device bind-mount (auto-generated for non-virtual `/sys/class/block` entries, including partitions, `dm-*`, and `md*`). |
| `Bind=/dev/infiniband/*` | nspawn config | InfiniBand HCA device bind-mount (auto-generated when `/dev/infiniband` devices are present). |
| `Bind=<configured /dev path>` | nspawn config | Additional host device bind-mount (configured with `spec.agent.additionalHostDevices`). |
| `Bind=` / `BindReadOnly=` | nspawn config | GPU device and library bind-mounts (auto-generated when GPUs are present). |
| `DeviceAllow=` | Service override | Cgroup device permissions for all bind-mounted host device nodes (KVM, block, InfiniBand, configured extra devices, GPU). |

#### System call filter

The `SystemCallFilter=@keyring bpf perf_event_open` setting keeps nspawn's
default syscall filtering, but explicitly allows syscalls used by the worker
stack:

- `@keyring` allows the kernel keyring syscall group that containerd uses for
  snapshotter and container setup operations.
- `bpf` allows eBPF program and map operations used by runc, cgroup handling,
  and eBPF CNIs.
- `perf_event_open` allows eBPF CNIs such as Cilium to create per-CPU perf ring
  buffers for datapath events. If nspawn blocks it, Cilium startup fails when
  creating those perf rings.

## What Runs Inside the Container

The nspawn container hosts the complete Kubernetes worker node stack:

- **systemd** (PID 1 inside the container)
- **containerd** (container runtime)
- **runc** (OCI runtime)
- **kubelet** (Kubernetes node agent)
- **kubectl**, **kube-proxy**
- **CNI plugins** (under `/opt/cni/bin`)
- All Kubernetes **pod containers** (managed by containerd and runc)

The **unbounded-agent** itself and host-side management tools such as
**machinectl** and **systemctl** remain on the host. `machinectl` is used to
start, inspect, and access the machine, while the lifecycle of the backing
`systemd-nspawn@<MachineName>.service` is managed via `systemctl`. NVIDIA
kernel drivers and userspace libraries also stay on the host and are forwarded
into the container via bind-mounts.

## Lifecycle

### Startup

The agent's three-phase bootstrap drives the nspawn lifecycle:

1. **Host preparation.** Installs the `systemd-container` package (provides
   `systemd-nspawn` and `machinectl`), sets kernel sysctl values that the
   container cannot write (because `/proc/sys` is read-only), and installs a
   `nftables-flush.service` oneshot that clears stale firewall rules before
   any `systemd-nspawn@.service` starts.

2. **Rootfs preparation.** Creates the rootfs, writes the `.nspawn` config and
   service override, downloads Kubernetes and container runtime binaries, and
   configures the OS inside the rootfs (hostname, DNS, kernel modules).

3. **Node start.** Starts the nspawn machine, polls until it is responsive,
   then enables containerd and kubelet inside it.

### Removal

During reset and blue/green repave operations, the agent first stops the active
`systemd-nspawn@<MachineName>.service` and waits until `machinectl` no longer
knows the machine. It then asks `machinectl remove <MachineName>` to remove the
image/rootfs registration.

On some hosts, `machinectl remove` can still fail after the machine is stopped
because of host-side policy or packaging behavior rather than because the
container is still running. Fedora with SELinux enforcing can deny
`systemd-machined` permission to create its nspawn image lock file under
`/run/systemd/nspawn/locks`, causing `machinectl remove` to report
`Access denied` even though `machinectl show <MachineName>` already reports no
such machine. In that state, the agent falls back to deleting the inactive
rootfs directory directly so the same machine name can be reused on a later
blue/green cycle.

## Networking

The container operates in the host's network namespace (`VirtualEthernet=no`):

- No virtual ethernet pair, bridge, or NAT is created.
- Kubelet binds to `0.0.0.0` and uses the host's routable IP.
- CNI plugins create network interfaces (WireGuard, VXLAN, overlay bridges)
  that are visible on both the host and inside the container.
- Each nspawn machine gets a private bpffs mount at `/sys/fs/bpf`, backed by
  `/run/bpffs/<MachineName>` on the host. This isolates pinned BPF
  object paths between alternating nspawn machines, but it does not by itself
  detach host-visible TC/XDP/cgroup programs or remove CNI-created network
  devices.
- `/proc/sys/net` is writable inside the container so that CNI plugins and
  kube-proxy can configure network sysctls.
- Host-level sysctl values (`net.ipv4.ip_forward`, `net.bridge.bridge-nf-call-iptables`,
  etc.) are pre-set on the host because the rest of `/proc/sys` is read-only.
- DNS resolution uses a static copy of the host's `resolv.conf`.
  `systemd-resolved` is masked inside the container to prevent it from
  overwriting the file. The host's stub resolver at `127.0.0.53` is reachable
  because the network namespace is shared.

## Key Paths

| Path | Description |
|---|---|
| `/var/lib/machines/<MachineName>` | Container rootfs directory. |
| `/etc/systemd/nspawn/<MachineName>.nspawn` | nspawn configuration file. |
| `/etc/systemd/system/systemd-nspawn@<MachineName>.service.d/override.conf` | Systemd service override. |
| `/run/host-nvidia/<index>/` | (Inside container) Read-only bind-mount of host NVIDIA library directories. |

## See Also

- **[Agent Configuration]({{< relref "reference/agent/configuration" >}})**:
  JSON config file specification including `MachineName` and `OCIImage`.
- **[NVIDIA GPU Support]({{< relref "reference/gpu/nvidia" >}})**:
  How GPU devices and libraries are forwarded into the nspawn container.
- **[Agent Guide]({{< relref "guides/agent" >}})**:
  End-to-end walkthrough of the three-phase bootstrap.
- **[Agent Operations]({{< relref "guides/operations/agent-operations" >}})**:
  Upgrading the agent and resetting hosts.
