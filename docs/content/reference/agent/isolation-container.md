---
title: "container Mode (Experimental)"
weight: 6
description: "Run the Unbounded node stack in a privileged OCI container supervised by an agent-owned systemd unit, and learn its requirements, layout, networking, lifecycle, and limits."
---

`container` mode runs the Kubernetes node stack inside a privileged OCI container on the worker node. The stack is the same set of components the other modes run: systemd as the container's PID 1, then containerd, kubelet, the CNI plugins, and CoreDNS. The Unbounded agent and its daemon stay on the host, outside the container, and operate the stack through the OCI runtime and a systemd unit they own.

The mental model is a slot with two layers. The filesystem layer is an unpacked OCI image under a per-slot directory. The execution layer is an OCI runtime bundle generated beside that directory, plus a supervising systemd unit that starts the bundle with `runc`. Inside the container, the node's own systemd starts the node services. On the host, the agent writes configuration, waits for readiness, and reports health. This is close to the `nspawn` model, but the boundary is an OCI container rather than a `systemd-nspawn` machine.

`container` mode is not the default and is not a drop-in replacement for `nspawn`. It exists to run the node on newer image-based hosts where a machine runtime such as `systemd-nspawn` is a poor fit, and it is gated as experimental in the agent, in provisioning, and in the CLI. Read this page before you enable it on a host, and read [Isolation Modes]({{< relref "reference/agent/isolation" >}}) first if you have not chosen a mode yet.

{{< callout type="warning" >}}
`container` mode is **experimental**. The backend has been exercised against fake runtimes and stubbed hosts, not against a real `runc`, systemd, or Azure Container Linux node. Several host integration details are marked provisional in the source. Do not treat a successful evaluation as a production recommendation, and do not enable it on a host you cannot rebuild.
{{< /callout >}}

## Target host

The target of the mode is a privileged OCI container, not a virtual machine and not a second copy of the host. It has no kernel of its own and no virtual network device. It runs on the host kernel, in the host network namespace, with a filesystem rooted at an unpacked image. The agent creates and runs it with `runc` directly; the host's Docker and containerd do not create or supervise the node container. The container is privileged in the capability sense because the node stack inside it needs to create nested containers, configure interfaces, and manage mounts. It is not privileged in the sense of having unrestricted access to every host device; device access is assembled from explicit grants plus a small baseline.

The shape is deliberate and can be summarized as follows. systemd runs as PID 1 from `/usr/lib/systemd/systemd` inside the rootfs. The process receives the full Linux capability set, including `CAP_SYS_ADMIN`, `CAP_NET_ADMIN`, `CAP_SYS_MODULE`, `CAP_MKNOD`, `CAP_BPF`, and the rest of the set through `CAP_CHECKPOINT_RESTORE`. The container keeps its own PID, mount, IPC, and cgroup namespaces, and it shares the host network and UTS namespaces, so the hostname is the node name and pods use host-reachable addresses.

The container's rootfs propagation is `rshared`, and the host's `/var/lib/kubelet` is bound in with shared propagation. That combination lets pod volume mounts made by the kubelet and by CSI drivers appear on the host, which is what the same-path contract requires. The rest of the filesystem boundary is the unpacked image plus a small set of host bind mounts described under [Mounts](#mounts).

### What it isolates

The boundary separates the node's userland from the host's userland. The node gets its own distribution files, its own package set, its own systemd, and its own process tree. Replacing or upgrading the node replaces an image tree under `/var/lib/unbounded/containers`, not the host operating system. That is the practical benefit, and it is the same benefit `nspawn` provides through a machine rootfs.

The boundary also gives the node its own service manager and cgroup subtree. Stopping the supervising unit stops the container and the services inside it. A crash or restart loop in the node does not become a host service restart loop. The agent can inspect the container through the runtime and run commands inside it without installing node tooling on the host.

### What it does not isolate

The container shares the host kernel, the host network stack, and several host directories. It is not a security boundary between tenants and it is not a sandbox for untrusted workloads. A process with `CAP_SYS_ADMIN` in the host network namespace has substantial power over the host, and the generated bundle grants that power on purpose because a Kubernetes node needs it.

The mode also does not isolate the node's storage from the host. `/run/containerd`, `/etc/containerd/certs.d`, `/var/lib/kubelet`, and `/opt/cni` resolve to the same host paths inside and outside the container. The host's own containerd and kubelet units are disabled during preparation. A host containerd would own `/run/containerd/containerd.sock`, which the container's containerd also needs.

Because of those shared paths, only one slot may run at a time. The two supervising units conflict with each other, the unit that starts first disables the other slot's unit, and the pre-start hook refuses to start a slot while its peer's container is still alive. A failed stop therefore cannot leave two node stacks writing the same kubelet and CNI directories.

## Requirements

The requirements below are enforced by the backend's preflight, by the bootstrap plan, or by both. Preflight is non-mutating: it inspects the host, reports every failure it finds, and changes nothing. Bootstrap then installs what it owns and prepares the slot.

| Requirement | Why it is needed | Where it is checked |
|---|---|---|
| systemd 254 or newer | The supervising unit uses `DelegateSubgroup=`, `RestartSteps=`, and `RestartMaxDelaySec=`. Older systemd ignores them, which leaves `runc` in the unit's own cgroup and starts the container without the memory and I/O controllers. | `systemctl --version` during preflight |
| cgroup v2 unified hierarchy | The unit delegates its cgroup and splits it into `supervisor` and `payload` subgroups, and cgroup v2 keeps the no-internal-processes rule. | `/sys/fs/cgroup/cgroup.controllers` during preflight |
| Writable `/etc`, `/opt`, and `/var` | The agent writes host configuration, installs the runtime under `/opt/unbounded`, and unpacks slot state under `/var/lib/unbounded`. | Preflight probes each path |
| A `runc` binary at `/opt/unbounded/runtime/runc` | Only `runc` is supported. The agent installs it during bootstrap, so preflight tolerates a missing file on a clean host, but a file that is present must be executable. | Preflight, and again before each start |
| An SELinux label the kernel accepts when enforcing | The bundle sets the process label `system_u:system_r:spc_t:s0` when SELinux is enforcing. Preflight writes the label through `/sys/fs/selinux/context` and reads it back. | Preflight when `/sys/fs/selinux/enforce` is `1` |
| Host-loaded kernel modules | A container does not load kernel modules at boot. The bootstrap plan loads the node's modules on the host, and `/lib/modules` is bound read-only so `modprobe` inside the node sees the host kernel's modules. | Bootstrap preparation |
| `curl`, `nft`, and `mountpoint` | The host phases use them for downloads, the nftables flush unit, and private bpffs checks. On an image-managed host the image must provide them; the mode never requires `systemd-container`. | Package phase and preflight |
| Provisioning format recorded | The agent records `ProvisioningFormat` for every mode. `container` mode adds no separate format check. | Configuration validation |

The first six rows are the ones that preflight, bootstrap, or a start can fail on. The package row can also fail on an image-managed host whose image omits a tool: the agent does not attempt package mutation on a read-only `/usr`, so the missing capability is reported as a prerequisite. The provisioning format row is a clarification rather than a gate. `ProvisioningFormat` accepts `cloud-init` or `ignition` for any mode, and neither the container backend nor its preflight branches on the value.

The runtime is a host artifact owned by the agent. It is installed from the same resolved `runc` version and download source as the runtime inside the node rootfs, and an install replaces the file atomically so a running container's supervisor never sees a partial binary. If the host kernel release is one that the `libpathrs` library linked into newer `runc` releases cannot parse, the agent selects an older fallback release instead. That fallback exists because such a `runc` panics at startup and every container create fails.

{{< callout type="note" >}}
A preflight that runs before bootstrap is allowed to report a missing runtime, because the agent installs it later in the same bootstrap. A preflight on an installed host is not: the runtime is required to exist and be executable before the unit can start.
{{< /callout >}}

### Host types that support it

The backend does not select on a distribution name. It checks capabilities: a cgroup v2 hierarchy, systemd 254 or newer, an accepted SELinux label when enforcing, writable `/etc`, `/opt`, and `/var`, and an executable `runc`. Any host that satisfies those checks and provides the required tools can pass preflight, and any host that does not is refused even if its name suggests otherwise.

The design target is an image-based host. Azure Container Linux is the specific case the design work names: it is a Flatcar-derived, immutable image with a read-only `/usr` and no usable package manager. The agent distinguishes it from mutable Azure Linux 3 when it reads `/etc/os-release`, and it selects the Azure Linux 3 rootfs image for both because they share the kernel and userland ABI. The container mode does not require an Azure-specific host API, so a conventional Ubuntu or Azure Linux host with the required capabilities can be used for evaluation as well.

There is one consequence for immutable hosts. Because the image is the unit of delivery, a required tool that the image does not provide cannot be installed. The host must already resolve `curl`, `nft`, and `mountpoint`, whether they are baked into the image or supplied by a system extension. The same rule applies to `runc`: the agent installs it under `/opt/unbounded`, which must be writable, and the image must permit executing from there.

### Status

The mode's status is experimental. Its backend, bootstrap plan, node operator, and CLI wiring exist and are exercised by tests, but the implementation is written against the design and against fake runtimes and stubbed hosts. It has not been validated on a real Azure Container Linux node. Treat a successful run as an evaluation result, not as evidence that the mode is ready for production.

The mode has two gates, and they must agree. A provisioner must be allowed to put a Machine into the mode, and the agent on the host must be configured to operate it. The agent gate is the deciding one: the agent refuses to bootstrap in the mode, and refuses to change a running node to it, unless the configuration opts in.

The agent setting is `Experimental.ContainerIsolation`:

```json
{
  "Isolation": "container",
  "Experimental": {
    "ContainerIsolation": true
  }
}
```

A running agent also accepts `--isolation` on `start` and `preflight`, but the flag overrides only the mode name. It does not override the experimental gate. Passing `--isolation container` to a configuration that does not set `Experimental.ContainerIsolation` fails validation with a message that names the missing setting.

Provisioners write the same field. Machina reads `experimentalContainerIsolation` from its own configuration, metalman accepts `serve-pxe --experimental-container-isolation`, the machine-ops-controller accepts `--experimental-container-isolation`, and `kubectl unbounded machine manual-bootstrap` accepts the flag when it generates an agent configuration. If a provisioning path cannot set the opt-in, it cannot create a container-mode host.

## How it works

The bootstrap plan for the mode reuses the shared host-native sequence. Host preparation installs the mode's packages, loads the kernel modules, configures the OS and nftables, disables Docker, containerd, and kubelet on the host, disables swap, and removes package-manager hardening. It omits `systemd-container` and the machined hardening because there is no machine. Node preparation then installs the host runtime and lifecycle helper, unpacks the image, renders the OCI bundle and the supervising unit, and provisions the rootfs with the Kubernetes, CRI, and CNI binaries and the OS configuration.

The result is a prepared slot. Nothing runs yet. Starting the slot enables and starts the unit, waits for systemd inside the container to reach `running` or `degraded`, waits for the shared LocalDNS network when LocalDNS is enabled, starts containerd, imports staged container images, starts kubelet, restarts services whose configuration changed, and waits for kubelet to bootstrap.

### Node Lifecycle

The supervising unit is named `unbounded-node-<slot>.service`, where `<slot>` is `kube1` or `kube2`. It is written to `/etc/systemd/system` and it is the only host unit that runs the node container. Its start sequence is what turns a prepared slot into a running node.

Before `runc` starts, the unit deletes any stale runtime state for the container. It creates and mounts the slot's private bpffs at `/run/bpffs/<slot>`, makes the host's `/var/lib/kubelet` a shared mount, and makes that mount recursively shared. Doing this on every start is what keeps pod volume propagation working after a host reboot. The unit then runs the agent's hidden `node-lifecycle pre-start` hook, which refuses to start while the peer slot's container is alive, regenerates `config.json` from the recorded node spec, refreshes NVIDIA grants when the host has them, and reloads systemd.

The unit then runs `/opt/unbounded/runtime/runc run --bundle /var/lib/unbounded/containers/bundles/<slot> unbounded-<slot>`. The container starts systemd as PID 1 and the node services follow. After the runtime starts the container, the unit runs `node-lifecycle post-start`, which waits for the stack to report running and then reconciles NVIDIA state on a GPU host. If the post-start phase fails, the unit start fails and systemd retries according to the restart policy.

Stopping is deliberately strict. The backend stops and disables the unit, then waits up to 30 seconds for the runtime to report the container gone. A container that outlives the unit's stop is killed with `SIGKILL` and its runtime state is deleted, and the backend waits again. A container that survives all of that is an error, because it still holds the host paths both slots share. Destroy refuses to remove a slot's files while the runtime still has a live container for it; the files are removed on a later attempt once the container is confirmed gone.

`systemctl restart` on the unit is the soft reboot. It restarts the container only, never the host. The unit's hooks regenerate the bundle, the systemd inside starts the enabled containerd and kubelet, and the agent waits for LocalDNS and an active kubelet. A host reboot is different: the unit is enabled, so systemd brings the container back, and the pre-start hook reconstructs the runtime state that `/run` lost.

### Image layout

The on-disk layout keeps the image tree, the runtime bundle, and the supervision separate. That separation matters when you inspect a node: the rootfs is a pure image tree, `config.json` is regenerated state, and the unit is the entry point systemd uses to start the container.

| Path | Contents |
|---|---|
| `/var/lib/unbounded/containers/<slot>` | Unpacked OCI image rootfs for the slot |
| `/var/lib/unbounded/containers/bundles/<slot>/config.json` | Generated OCI runtime specification |
| `/var/lib/unbounded/containers/bundles/<slot>/nodespec.json` | Recorded node spec used to regenerate `config.json` |
| `/etc/systemd/system/unbounded-node-<slot>.service` | Supervising unit |
| `/opt/unbounded/runtime/runc` | Host OCI runtime installed by the agent |
| `/run/bpffs/<slot>` | Slot-private bpffs mount point on the host |
| `/etc/unbounded/agent/container-<slot>-applied-config.json` | Applied configuration record for the slot |

The rootfs is unpacked with platform selection, so a multi-platform image yields the manifest for the host architecture. The owned unpack path cleans an interrupted unpack and rebuilds it, and it keeps a completed one. Do not point an unpack at a slot that is running a node; the running container owns its rootfs. The bundle directory is kept apart from the rootfs so the image tree stays a pure image tree, and `bundles` is not a valid slot name, so the two never collide.

The runtime and the lifecycle helper are host artifacts, not node artifacts. The runtime is installed under `/opt/unbounded`, and the helper is the agent binary that provides the hidden `node-lifecycle` command. The unit names the helper path, so moving the agent's host root requires re-rendering the unit. The daemon does that without restarting a running container by rewriting the unit and reloading systemd.

### Agent

The agent process itself never enters the container. It runs as a host systemd service, holds the installation lock, watches the Machine, and dispatches operations through the container backend. The backend maps a slot to an unpacked rootfs and to the container that mounts it in a private mount namespace, and it runs commands inside the container through `<runtime> exec`. That is how the rootfs and node-start phases configure the node: they write below the slot root, or to the same-path host directory when a bind mount would hide a rootfs path, and they execute through the runtime.

The container's namespace set is small and intentional. It has a new PID namespace because PID 1 must be systemd. It has a new mount namespace so the rootfs and its binds are private. It has a new IPC namespace and a new cgroup namespace. It has no network namespace entry, so it uses the host network stack, and it has no UTS namespace entry, so it uses the host hostname. The cgroup path is `/system.slice/unbounded-node-<slot>.service/payload`, inside the unit's delegated cgroup, so stopping the unit ends the container with it.

The process environment is minimal: a standard `PATH` and `container=oci`. The root filesystem path is the slot directory, and the root is not mounted read-only. The process runs as UID 0 and GID 0 with the full capability set, and the bundle applies masked and read-only paths that protect host-wide kernel interfaces the node does not need. The container gets a small baseline device set that includes `/dev/kmsg`, because kubelet opens it to watch for OOM kills and exits when it is missing.

The rootfs image supplies the node's userland, not the Kubernetes binaries themselves. After the unpack, the rootfs phases install the Kubernetes, CRI, and CNI artifacts and write their configuration at the paths the node expects. The image is selected the same way as in `nspawn`: an explicit `OCIImage` wins, environment overrides can replace it, and otherwise the agent chooses a default image for the detected host distribution and GPU presence. A GPU host uses the NVIDIA variant of the image, which ships the container toolkit the node needs.

The container's own udev units are masked in the rootfs. The host owns the devices, and a second udev inside the container would process the same kernel events and rewrite device nodes and permissions that the host also manages. Masking `systemd-udevd.service` and its sockets, trigger, and settle units keeps device handling on the host side. This is also why `/sys` is mounted read-only and why `/proc` is masked with the runtime's default set of protected paths.

### Cluster

From the cluster's point of view, a container-mode host registers an ordinary Kubernetes Node. The control plane does not need a special API, a different kubelet endpoint, or a new resource. The agent that runs on the host is the component that knows the node lives in a container, and it keeps that knowledge local.

The slot still matters because it is the blue/green unit of upgrade. A first install builds `kube1`. A repave builds `kube2` beside the running `kube1`, stops and disables the old container, cleans host networking, resets the shared kubelet state, configures and starts the new slot, and waits for kubelet to become active. The applied configuration is persisted only after the new slot is healthy. The old slot's record and files are then removed. If anything fails before the new slot is healthy, the new slot is destroyed and, once the runtime confirms it is gone, the old container is started again from the configuration snapshot taken before the switch.

The kubelet configuration lives on the host side and is mounted into the container through the same-path mount at `/var/lib/kubelet`. The agent writes the kubelet config, kubeconfig, and related files where the container's kubelet reads them. The shared directory is also why a new slot resets the kubelet identity: pod volume mounts made by the old kubelet propagated to the host and outlive the stopped container, so the agent unmounts everything strictly below `/var/lib/kubelet` and removes the previous identity before a new slot bootstraps. Direct kubeconfig and exec credential modes are rewritten by the configuration phase instead.

The node network is the host network. The agent creates the shared LocalDNS network and its nftables rules on the host, and when LocalDNS is enabled the container unit `Requires=` and orders after `unbounded-localdns-network.service`. There is no per-slot network unit. The control plane reaches the node through the same addresses and ports it would use for a host-installed node.

### Extensions

This worktree contains no container-mode-specific extension hook, webhook, or sidecar manager. The host-side add-on that the mode actually wires is the shared LocalDNS network unit described above. kube-proxy is staged as a normal Kubernetes binary and runs as part of the node stack; it is not a container-mode extension.

Earlier design notes described a kubelet-proxy extension that would generate kubelet configuration through a mutating webhook and run a sidecar in the node container. Nothing in `cmd/agent` or `pkg/agent` implements that extension, so do not plan around it. If you need a proxy or a custom kubelet configuration today, use the supported configuration fields and the node's normal kubelet configuration path.

### Networking

The container uses the host network namespace. The bundle has no network namespace entry, so the runtime does not create one, and the container sees the host's interfaces, addresses, routes, and firewall rules. There is no veth pair, no bridge, and no NAT layer between the node and the host. A pod address is reachable on the host in the same way it is in the other isolation modes, and the CNI plugins configure interfaces that the host can see.

The network filesystem view follows from that choice. The container mounts its own `/proc`, `/dev`, `/run`, and `/tmp`, while `/sys` is the host's sysfs mounted read-only. `/proc/sys` is read-only except for `/proc/sys/net`, which stays writable because CNI plugins and kube-proxy set network sysctls that apply to the shared network namespace. The explicit read-only list covers `abi`, `crypto`, `debug`, `dev`, `fs`, `kernel`, `user`, and `vm`; a subtree the kernel does not have is skipped by the runtime.

The node's container runtime and CNI depend on three shared paths. `/opt/cni` is bound in at the same path so plugins installed by the agent and by cluster add-ons are visible to the container's containerd. `/run/containerd` is bound in so the container's containerd and its shims use the same runtime socket directory as the host would; this is also why a host containerd must be disabled. `/etc/containerd/certs.d` is bound in so registry host configuration, including any Gantry endpoint, reaches the container's containerd.

The bpffs is different. Each slot gets its own mount at `/run/bpffs/<slot>` on the host, and that mount is bound at `/sys/fs/bpf` inside the container. The path is the same one an nspawn machine for the slot uses, so reset can remove either mode's pinned-object namespace with one task. The mount is not shared, because each slot needs its own pinned BPF object namespace rather than a common one.

LocalDNS is a host-global concern. The agent creates the `localdns` dummy interface and the nftables rules on the host, and the container's resolv.conf and CoreDNS configuration point at that listener. When LocalDNS is enabled, the supervising unit requires `unbounded-localdns-network.service` and orders after it, so the network exists before the container starts. Nothing about DNS requires a container-specific port or address.

Pod volume propagation is the one place where the network and storage choices meet. The unit makes `/var/lib/kubelet` a shared mount before each start, the container binds it with `rshared`, and the bundle sets rootfs propagation to `rshared`. A CSI driver inside the container can therefore mount a volume under `/var/lib/kubelet` and have the host see it, which is what the kubelet on the host side and the container runtime need in order to share pod filesystems.

### Mounts

The mount set has three groups: the container's own pseudo filesystems, the same-path binds that satisfy the storage contract, and extra binds or grants that add host resources. The bundle is generated deterministically from the slot, the grants, the SELinux state, and the devices that exist on the host. It is written to `/var/lib/unbounded/containers/bundles/<slot>/config.json`.

| Destination | Source | Mode | Purpose |
|---|---|---|---|
| `/proc` | `proc` | `nosuid,noexec,nodev` | Container's own process view |
| `/sys` | `sysfs` | read-only | Host kernel view, no device reconfiguration |
| `/sys/fs/cgroup` | `cgroup` | read-write | systemd inside the container manages its cgroup |
| `/dev` | `tmpfs` | `nosuid,strictatime,mode=755` | Container device directory |
| `/dev/pts`, `/dev/shm`, `/dev/mqueue` | kernel filesystems | standard options | Terminal, shared memory, and message queues |
| `/run` | `tmpfs` | `nosuid,nodev,mode=755` | Runtime state, recreated on every start |
| `/tmp` | `tmpfs` | `nosuid,nodev,mode=1777` | Temporary files |
| `/run/containerd` | `/run/containerd` | `rbind,rw` | Shared containerd socket directory |
| `/etc/containerd/certs.d` | `/etc/containerd/certs.d` | `rbind,rw` | Registry host configuration |
| `/var/lib/kubelet` | `/var/lib/kubelet` | `rbind,rw,rshared` | Kubelet state and pod volume propagation |
| `/opt/cni` | `/opt/cni` | `rbind,rw` | CNI binaries and configuration |
| `/sys/fs/bpf` | `/run/bpffs/<slot>` | `rbind,rw` | Slot-private pinned BPF objects |
| `/var/lib/unbounded/container-images` | host `current` symlink | `rbind,ro` | Staged offline image archives, when present |
| `/lib/modules` | `/lib/modules` | `rbind,ro` | Host kernel modules for `modprobe`, when present |

The same-path binds are why the agent needs a path mapping rather than a simple root prefix. A file written below the unpacked rootfs at `/var/lib/kubelet/...` would be hidden by the bind mount, so the container backend resolves paths under those directories to the host directory itself. The mapping covers `/run/containerd`, `/etc/containerd/certs.d`, `/var/lib/kubelet`, `/opt/cni`, and the slot's bpffs. Every other in-node path resolves below the slot's rootfs.

Extra binds are conditional. The image archive bind and the kernel modules bind are added only when their source exists on the host, because the runtime refuses a bundle with a missing bind source. The pre-start hook renders the bundle again on every start, so a source that appears later, such as a staged offline archive, is picked up on the next start.

Grants add the rest. A mount grant exposes a host path at an absolute source and an in-node target, read-only or read-write. A device grant exposes a device node with its major and minor numbers, or a directory of device nodes as a bind mount. A library path grant exposes a directory read-only and adds it to `LD_LIBRARY_PATH` inside the container. Every target is cleaned before it is used, and a target under `/proc` or `/sys` is rejected because it would replace the container's own view of the kernel. A target that conflicts with an existing mount is also rejected, so a grant cannot silently replace a same-path bind or another mount.

### Agent Config

The agent configuration is the same structure in every mode. This excerpt shows only the keys that decide the container mode; a real configuration also carries the cluster, kubelet, CRI, and CNI settings.

```json
{
  "Isolation": "container",
  "Experimental": {
    "ContainerIsolation": true
  },
  "OCIImage": "ghcr.io/azure/agent-azlinux3:v20261031",
  "ProvisioningFormat": "ignition"
}
```

| Field | Effect in this mode |
|---|---|
| `Isolation` | Selects `container`. An empty value means `nspawn`, not container. |
| `Experimental.ContainerIsolation` | The agent-side gate. Without it, bootstrap and mode changes to `container` are refused. |
| `OCIImage` | Selects the rootfs image. Empty uses the built-in default for the detected host distribution and GPU presence. |
| `ProvisioningFormat` | Records `cloud-init` or `ignition` for the installation. The container backend does not branch on it. |
| `CRI.Runc.Version` | Pins the `runc` version used both inside the node rootfs and as the host runtime under `/opt/unbounded`. |

The mode is part of the installation identity. A host installed in `nspawn` cannot be started directly in `container`, and a host installed in `container` cannot be started directly in `nspawn`; the agent refuses instead of building a second node stack beside the first. The supported way to change the mode is an isolation change at the node's next repave. A move between `nspawn` and `container` is an in-place migration because both modes use slots. A move involving `none` is a reprovision, because `none` has no slot.

The applied configuration records the mode and slot. Container nodes use `container-kube1` and `container-kube2`, while nspawn nodes use the bare slot names. That naming is what lets an older agent refuse a host it does not understand instead of reading a container node as an nspawn machine.

## Devices and GPUs

The container does not get the host's `/dev` wholesale. It gets a generated `/dev` populated with the standard pseudo terminals, shared memory, and message queues, plus `/dev/kmsg`, plus whatever the grants add. The cgroup device rules start from an allow-all rule, because a privileged node container needs to create and use devices that appear after it starts, and then add explicit allow rules for each granted device and for device directories. The explicit rules are what keep a grant usable if the baseline is tightened later.

A device grant that names a directory, such as a directory of NVIDIA device nodes, becomes a bind mount rather than a single device entry. Binding the directory means nodes that the host creates in it later appear in the container without regenerating the bundle. A device grant that names a single node is written as a device entry with its type, major, minor, and mode. A device grant whose source does not exist fails bundle generation with an error naming the grant, because there is no major and minor to write.

The bundle always creates `/dev/kmsg`. kubelet opens that node to watch for OOM kills and exits when it is missing, and the runtime's default `/dev` does not include it. The explicit device entry and its allow rule are added to every bundle, independently of grants.

NVIDIA support follows the nspawn flow rather than inventing a second one. A GPU host uses the NVIDIA rootfs image, which ships the container toolkit. The grants place the host's NVIDIA device nodes and driver library directories into the container as devices, cgroup rules, and read-only binds under `/run/host-nvidia*`. The post-start hook then does inside the container what the nspawn lifecycle does inside a machine: it stops containerd and kubelet, links the driver libraries, builds the driver root, generates the CDI specification, selects CDI mode, and starts containerd and kubelet again.

A host reboot or a driver update can move devices and libraries after the slot was prepared. The pre-start hook rediscovers the NVIDIA state and replaces the recorded NVIDIA grants before the bundle is rendered again, so the container follows the host. A node that was provisioned without NVIDIA grants is left alone: adding a GPU to a CPU-provisioned node needs a new rootfs, not a grant refresh. If the host no longer reports complete NVIDIA state, the refresh fails instead of starting a container with stale device paths.

AMD devices and the sysfs paths the AMD device plugin reads arrive through the same grant mechanism, so they are present in the bundle as device nodes and mounts. The container-specific post-start reconciliation in this worktree is NVIDIA-only. If you run AMD GPUs, verify the node's device plugin behavior against your image and driver stack before treating the mode as ready.

## Operations

The agent exposes the same high-level operations in every mode. The table below describes what each one does in container mode, and the paragraphs after it cover the details that differ from nspawn.

| Operation | What happens in container mode |
|---|---|
| Preflight | Runs the container backend's checks: runtime, cgroup v2, systemd version, SELinux label, writable paths. |
| Bootstrap and start | Prepares the host, installs the runtime and helper, unpacks the image, renders the bundle and unit, provisions the rootfs, starts the unit, waits for readiness. |
| Stop | Stops kubelet and containerd inside the container, stops and disables the unit, and waits for the runtime to confirm the container is gone. |
| Restart | Restarts the container unit only, then waits for systemd, LocalDNS, and kubelet. The host is not rebooted. |
| Repave or upgrade | Builds the alternate slot, switches to it, waits for kubelet, then retires the old slot. Rolls back on failure. |
| Mode change | In place between `nspawn` and `container`; a reprovision when either side is `none`. |
| Reset | Stops and removes both slots, the shared kubelet, CNI, and image directories, the layout, and the host runtime. |
| Status and health | The unit must be active and the runtime must report the container running. |
| Exec | Runs a command through `<runtime> exec` in the container. |
| Logs | Runs `journalctl` inside the container through the runtime and streams it to you. |
| GPU inspection | Uses the node's own tools, such as `nvidia-smi`, inside the container. |

A stop is graceful when the container is running: the agent asks the container's systemd to stop kubelet and containerd first, so the container stop does not have to kill them. A failure to stop either service is logged and the stop proceeds, because the unit stop and the runtime kill are the backstop. A stop of an already stopped node succeeds.

A restart is a soft reboot. It re-runs the lifecycle hooks, including bundle regeneration and the NVIDIA refresh, and it waits for the node stack to come back. Use it when you changed configuration that the node reads at start. Use a host reboot when you need to test the boot path, including the unit's reconstruction of `/run` mounts and the private bpffs.

A repave is the normal upgrade path. The caller has already cordoned, drained, and deleted the Node, and the agent builds the new slot beside the running one. The new slot is prepared and started, host networking is cleaned as a repave does, the shared kubelet state is reset, and kubelet is waited on. Only after the new slot is healthy does the agent persist its applied configuration and remove the old slot's record and files. If the new slot fails, the agent destroys it and starts the old container again from the snapshot of the shared configuration, but only after the runtime confirms the failed slot's container is gone.

A mode change is declared in the Machine's `spec.isolation` and made at the node's next repave. The agent does not edit the spec of an existing Machine and does not switch a running node immediately. A move between `nspawn` and `container` is in place because both modes use the two slots. A move involving `none` resets and bootstraps the host in the new mode, because `none` has no second slot to build beside.

`unbounded-agent exec` works the same way as in the other modes, but the command runs inside the container:

```bash
sudo unbounded-agent exec -- crictl ps
sudo unbounded-agent exec -- systemctl is-system-running
```

`unbounded-agent logs` streams the container's journal. Without `--unit` it shows all units in the container, and with `--unit kubelet` it shows just the kubelet unit:

```bash
sudo unbounded-agent logs --unit kubelet --lines 200
sudo unbounded-agent logs --unit containerd --follow
```

The host unit's own journal is separate. Use `journalctl -u unbounded-node-kube1.service` to see the unit's start, stop, hook output, and runtime errors. Use `unbounded-agent logs` when you need the node's view, and the host journal when you need the supervisor's view.

The applied configuration is a plain file, so you can inspect which mode and slot the agent believes it is running:

```bash
sudo cat /etc/unbounded/agent/container-kube1-applied-config.json
```

There is no separate config-dump command in this agent. The applied configuration file and the Machine status are the two records to compare when you suspect drift.

Container state is visible through the runtime as well as through systemd. The container ID for a slot is `unbounded-<slot>`, so `unbounded-kube1` is the ID for the first slot:

```bash
sudo /opt/unbounded/runtime/runc state unbounded-kube1
sudo /opt/unbounded/runtime/runc list
```

Mounts inside the container can be listed without leaving the agent interface:

```bash
sudo unbounded-agent exec -- cat /proc/self/mountinfo
```

Restart history and start-limit state come from systemd:

```bash
sudo systemctl show unbounded-node-kube1.service \
  --property=NRestarts --property=ActiveState --property=SubState
```

Network tracing uses the host network namespace because the container shares it. You can run `ss`, `ip`, `tcpdump`, or `conntrack` on the host, or run the same tools inside the container with `unbounded-agent exec`, and you are looking at the same interfaces and routes either way.

## Limits

The mode has hard limits that come from the implementation, and softer limits that come from its experimental status. Know both before you schedule a workload that depends on a device, a host path, or a specific upgrade behavior.

| Limit | Effect |
|---|---|
| Experimental status | The mode is gated, marked provisional in the source, and not validated on a real host. |
| Host devices | Only granted devices and device directories appear in `/dev`; there is no blanket host device passthrough. |
| Directory rewrites | The same-path directories are shared by both slots, so preparing or repaving one slot rewrites files the other slot also sees. |
| NVIDIA GPU | GPU support follows the nspawn flow, including a post-start stop and restart of containerd and kubelet, and an added GPU needs a new rootfs. |
| `runc` runtime | Only `runc` is supported. Alternative OCI runtimes are not selectable. |
| Bind targets | A target under `/proc` or `/sys`, or one that collides with an existing mount, is rejected. An optional extra bind whose source does not exist is skipped. |
| Restart backoff | A failing unit retries with increasing delay, and the keys that implement that need systemd 254 or newer. |
| Start limits | Repeated failed starts stop after ten attempts in an hour, and the unit stays failed until the agent starts it again. |
| Timeouts | Start, stop, readiness, and gone checks are bounded, and a slow node can hit them. |
| Provisioning integrations | A provisioner that cannot write the experimental opt-in cannot create a container-mode host. |
| Concurrent slots | Two slots must never run at once over the shared host paths. |

The device limit is the one most likely to surprise a workload author. A workload that expects an arbitrary host device to be visible because the container is privileged will not find it. The cgroup baseline allows devices broadly, but a device node still has to exist in the container's `/dev` through a grant or a bound device directory. If you add a device to the host after the slot is prepared, change the configuration and reapply it so goal-state resolution rediscovers the device. The pre-start hook re-renders the bundle from the recorded spec on every start, and NVIDIA grants are rediscovered on every start as well.

The directory-rewrite limit is a consequence of the same-path contract. Preparing a new slot writes CNI binaries, kubelet configuration, and kubeconfig files that the old slot also reads. Upgrade saves those shared files before the switch and restores them if it rolls back, but a manual change made outside the agent during an upgrade can be lost. Make configuration changes through the agent configuration or through the supported host operations, not by editing files under `/var/lib/kubelet` or `/opt/cni` while a repave is in progress.

The NVIDIA limit has two parts. The first is that the post-start flow stops and restarts containerd and kubelet to rebuild the driver state, so the node has a brief disruption after a start on a GPU host. The second is that a node provisioned with the CPU image cannot gain a GPU by refreshing grants; the rootfs image itself must be the NVIDIA variant. Plan GPU placement before you provision the host.

The runtime limit is strict: the backend uses `runc` from `/opt/unbounded/runtime/runc` and does not have a configuration switch for another runtime. The install task also requires that the runtime file be named `runc`, because the version check matches the installed file by that name. If you need a different runtime, this mode is not the place to experiment with it.

The timing limits are worth knowing when you debug a slow host. The supervising unit's start timeout is the post-start budget plus one minute. The post-start budget is two minutes for readiness plus three minutes for the after-start work, so the start timeout is six minutes. Stop has a 120 second unit timeout. The backend's own wait for a stopped container to be gone is 30 seconds, polling every 500 milliseconds, and it applies both before and after a kill. A node that needs longer than these bounds will be reported as failed even if it eventually becomes healthy.

The provisioning limit is about reach, not capability. The agent library is consumed by integrations that map their own configuration schema onto the agent configuration. A path such as AKS Flex Node is one of those consumers, and it must set `Experimental.ContainerIsolation` in the configuration it produces. This repository does not contain that mapping, so container mode is not available through an integration that does not expose the setting, regardless of what the host can do.

Finally, the mode is not a security boundary. It isolates userland and processes from the host's userland, but it shares the kernel, the network, and a set of host directories, and it runs with the full capability set. Treat code inside the node container as having host-level privilege, and do not use this mode to run workloads from mutually untrusted tenants.

## Troubleshooting

Start with the gate, then the host, then the unit, then the node. Most failures are reported before anything is mutated, and the error text usually names the missing capability or setting. Run the steps in this order so you do not change a host that preflight would already have refused.

1. Confirm that the binary can operate the mode at all. `unbounded-agent supports-isolation container` exits zero when this agent has a complete wiring for the mode and non-zero otherwise. It reads nothing from the host.
2. Confirm that the agent configuration opts in. `Experimental.ContainerIsolation` must be `true`, and the mode must be `container` in the same configuration. A command-line `--isolation container` does not replace the opt-in.
3. Run preflight with the same configuration the agent will use. It is non-mutating, it reports every failure it finds, and JSON output makes the failing check easy to find.
4. If preflight passes, inspect the unit and its journal. Bootstrap failures after preflight are usually unit start failures, and the hook output is in the host journal.
5. If the unit is active, inspect the container. Check the runtime state, then run `systemctl is-system-running` and `crictl ps` inside the container.

```bash
sudo unbounded-agent supports-isolation container
sudo env UNBOUNDED_AGENT_CONFIG_FILE=<path-to-agent-config.json> \
  unbounded-agent preflight --output json
sudo systemctl status unbounded-node-kube1.service
sudo journalctl -u unbounded-node-kube1.service --no-pager -n 200
```

Replace `<path-to-agent-config.json>` with the configuration file the host uses, typically under `/etc/unbounded/agent`. The table below maps the common messages to the check that produced them and to the first thing to fix.

| Message or symptom | Likely cause | First fix |
|---|---|---|
| `isolation mode "container" is experimental` | The configuration does not set `Experimental.ContainerIsolation`. | Add the opt-in to the configuration that the host uses. |
| `this agent cannot operate isolation mode "container"` | The binary predates the mode or its wiring is incomplete. | Use an agent build whose mode table includes the mode. |
| `container runtime ... is not installed` | The runtime is missing at start time. | Run bootstrap to install it, or correct the runtime path. |
| `container runtime ... is not executable` | The file exists but is a directory or lacks execute permission. | Replace it with the agent-installed runtime. |
| `cgroup v2 is required` | The host booted a legacy or hybrid cgroup hierarchy. | Boot the host with cgroup v2 unified and retry. |
| `systemd ... is too old` | The host systemd is older than 254. | Upgrade systemd or choose a different isolation mode. |
| `selinux does not accept label` | SELinux is enforcing and the policy does not define the `spc_t` type. | Use an image whose policy accepts the label, or adjust your SELinux policy. |
| `/etc`, `/opt`, or `/var` is not writable | The host filesystem is read-only or mounted with restrictive options. | Provide writable paths or choose a host that has them. |
| `refusing to start kube2 while kube1 is still running` | The peer slot's container outlived its stop. | Stop and confirm the peer container is gone before retrying. |
| `container ... is still alive after stopping` | The runtime did not report the container gone after a kill. | Inspect the runtime state and the host process table; do not delete files while it lives. |
| `node stack ... not running after 2m` | The post-start hook did not see the stack become ready. | Read the container journal and the host journal for the failing unit. |
| `extra bind target ... under /proc or /sys` | A grant targets a path the container mounts itself. | Change the grant target to a path outside `/proc` and `/sys`. |
| `grant target ... conflicts with an existing mount` | Two grants or a grant and a same-path bind share a target. | Give the grant a unique target. |
| `device grant ... has no device information` | The device path does not exist when the bundle is rendered. | Confirm the device exists on the host before starting. |
| Pod volumes are missing on the host | Mount propagation is not shared, or the pre-start mount step failed. | Check the unit's pre-start output and the host's `/var/lib/kubelet` mount. |
| Kubelet exits immediately | `/dev/kmsg` is missing or the kubelet configuration is wrong. | Check the bundle's device list and the kubelet log. |
| A kernel module is missing inside the node | The host did not load it, or `/lib/modules` is not bound. | Load the module on the host and confirm the bind source exists. |

The peer-slot check deserves emphasis. Both slots share `/var/lib/kubelet`, `/opt/cni`, `/run/containerd`, and the same host network. If a container from the other slot is still alive, starting this slot would give two kubelets and two containerd instances the same state directories. The pre-start hook refuses rather than risk that. If a stop reported success but the runtime still shows a container, resolve the container with the runtime, not by deleting the slot directory. Deleting files while a container lives can delete host data through its bind mounts.

Mount propagation problems usually show up as pod volumes that exist inside a pod but not on the host, or as a kubelet that cannot find a volume the CSI driver mounted. The unit makes `/var/lib/kubelet` shared at every start, so first confirm that the unit's start actually reached that step. Then confirm that the container's `/var/lib/kubelet` is the shared bind and not a plain directory in the rootfs. `unbounded-agent exec -- cat /proc/self/mountinfo` shows the mount options the container sees.

Kernel module problems come from the container's boot model. The node does not load modules at boot the way a machine does, so the bootstrap plan loads the node's module list on the host. Inside the container, `modprobe` reads `/lib/modules` through the read-only bind. If the host kernel has no module for the requested feature, or the bind source is absent, the module cannot appear. Check the host with `lsmod` and check that `/lib/modules` exists before you look inside the container.

SELinux problems are less common but harder to read. When SELinux is enforcing, the bundle applies `system_u:system_r:spc_t:s0` to the container process. Preflight writes that label to the kernel's context interface and reads it back, so an unsupported type is reported before the unit starts. If preflight passes and the container still fails under enforcing mode, compare the host policy with the image's policy and check the audit log. Do not switch the host to permissive just to get past a label error unless that is an explicit policy decision for the host.

If the unit is active but the node is not healthy, separate the layers. The host unit being active means `runc` started the container, not that systemd inside it finished. Use `unbounded-agent exec -- systemctl is-system-running` to check the container's systemd, `unbounded-agent exec -- crictl ps` to check the container runtime, and `unbounded-agent logs --unit kubelet` to check kubelet. Then compare what you see with the host journal. The host journal records the unit's hook output and runtime errors; the container journal records the node's own services.

If a start keeps failing, systemd stops retrying after ten failed starts in an hour. The unit stays failed until something starts it again, which the agent does on its next start or restart. Clearing the failed state manually with `systemctl reset-failed` is safe when you have fixed the cause, but it does not fix the cause. Read the journal first, then reset the state.

## Related

- [Isolation Modes]({{< relref "reference/agent/isolation" >}}) compares `nspawn`, `none`, and `container`, and explains how to select and change a mode.
- [systemd-nspawn Isolation]({{< relref "reference/agent/nspawn" >}}) describes the default mode that container mode mirrors.
- [No Isolation Mode]({{< relref "reference/agent/isolation-none" >}}) describes the host-native mode and when it is the better fit.
- [Agent Configuration]({{< relref "reference/agent/configuration" >}}) documents every configuration field, including `Isolation` and `Experimental.ContainerIsolation`.
- [Agent Preflight]({{< relref "reference/agent/preflight" >}}) explains the checks, their names, and how to read their output.
- [Provisioning Format]({{< relref "reference/agent/provisioning-format" >}}) explains how `cloud-init` and `ignition` records are written and consumed.
- [Agent Operations]({{< relref "guides/operations/agent-operations" >}}) covers day-two operations, including upgrades and resets.

