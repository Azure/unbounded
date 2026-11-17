---
title: "systemd-nspawn Isolation"
weight: 3
description: "How the unbounded agent runs a Kubernetes node stack inside a systemd-nspawn machine, including its rootfs, host configuration, devices, networking, and lifecycle."
---

systemd-nspawn is the default isolation mode of the unbounded agent. The agent and its daemon stay on the host, while the node stack (systemd, containerd, kubelet, the kubelet configuration, and the CNI binaries) runs inside a lightweight machine created by `systemd-nspawn`. The machine uses the host kernel and the host network namespace, but it has its own filesystem, PID namespace, cgroup subtree, and service manager.

This page explains what that arrangement gives you, what the agent puts in the machine, how the host-side configuration is generated, how devices and GPUs reach the machine, and what the lifecycle looks like from first boot to removal. Read it when you are debugging a node that will not start, when you need to pass an extra device or host path into the node, or when you are deciding whether nspawn is the right mode for a host.

![Diagram of the host and systemd-nspawn machine boundary, showing the agent and machined on the host and systemd, containerd, and kubelet inside the machine.](../../../img/nspawn-host-container-boundary.svg)

## Why nspawn

The nspawn mode exists because a Kubernetes node needs a newer, more specific userland than many host images provide, and because installing that userland next to the host's own packages causes conflicts. A host may be an immutable image with a read-only `/usr`, a distribution release that is older than the Kubernetes version you want, or a machine whose owners do not want kubelet and containerd added to the base operating system. The machine rootfs gives the node stack a private copy of the distribution userland without changing the host's package database.

Filesystem isolation is the first practical benefit. The node's binaries, configuration files, container runtime state, and pod filesystems all live under `/var/lib/machines/<machine>`. The host sees that directory as ordinary files, which makes it easy to inspect and easy to delete, but processes inside the machine cannot write to the host's `/usr`, `/etc`, or `/var` except through the bind mounts the agent configures. Replacing the machine rootfs is therefore a directory replacement rather than a package transaction on the host.

Process isolation is the second benefit. systemd runs as PID 1 inside the machine, and every node service and every pod container descends from it. The machine lives in its own cgroup subtree, so the node's containers do not appear as siblings of host services, and a container runtime inside the machine can manage its own slice hierarchy. The PID namespace also means that the node sees a consistent process tree that is separate from the agent, the machine manager, and unrelated host workloads.

systemd supervision is the third benefit. The host starts the machine with the `systemd-nspawn@<machine>.service` unit, enables it so it returns after a host reboot, and can inspect it with `machinectl`. Inside the machine, systemd starts containerd and kubelet as normal units, so logs, restart policy, and dependency ordering work the way a Kubernetes operator expects. The agent does not run a second supervisor of its own inside the machine.

The network namespace is deliberately shared. The agent sets `VirtualEthernet=no`, so the machine does not get a veth pair, a bridge, or NAT. kubelet, kube-proxy, and the CNI plugins operate on the host's interfaces and addresses. This keeps pod networking identical to a node installed directly on the host, at the cost of sharing the host's port space and firewall rules.

Upgrades use rootfs replacement. The agent provisions a new rootfs in the alternate machine slot (`kube1` or `kube2`), starts the new machine, verifies that kubelet is healthy, and only then removes the old machine and its applied configuration. That sequence is why the machine names alternate and why the rootfs is treated as disposable state rather than as a pet filesystem.

{{< callout type="warning" >}}
The nspawn boundary is a compatibility and collision boundary, not a security boundary between tenants. The generated configuration grants all capabilities and runs without a private user namespace so that runc can create nested containers. Treat code inside the machine as having host-level privilege.
{{< /callout >}}

## Container Rootfs

The machine rootfs is a plain directory at `/var/lib/machines/<machine>`, where `<machine>` is either `kube1` or `kube2`. systemd-nspawn names a machine after its directory, so the directory name and the unit instance always agree. The agent's `RootFS` goal state points every rootfs phase at this directory and derives the machine name from its base name.

The first time the directory is empty or missing, the agent bootstraps it from an OCI image. The image is acquired, its platform manifest is checked against the host architecture, and its layers are unpacked into the machine directory. If the directory is not empty, the ordinary bootstrap path logs a warning and skips the unpack, because an existing machine may already contain state that should not be overwritten. Owned initial installs use a separate replay path that clears an incomplete directory and writes a `.unbounded-rootfs-complete` marker only after a durable unpack.

The OCI image supplies the distribution userland, not the Kubernetes node itself. After the unpack, the agent installs the node artifacts into the rootfs and writes their configuration at the paths the node expects. The Kubernetes binaries are verified against their published SHA256 checksums, and containerd, runc, and the CNI plugins are installed from their release archives. A rootfs image can therefore be rebuilt without republishing the node artifacts.

The following table lists the most important paths that the rootfs phases populate. All paths are relative to the machine root inside the machine, and resolve to `<machine directory>` plus the same path on the host.

| Path inside the machine | Contents |
| --- | --- |
| `/usr/local/bin/kubelet` | kubelet binary for the configured Kubernetes version |
| `/usr/local/bin/kubectl` | kubectl binary for the configured Kubernetes version |
| `/usr/local/bin/kube-proxy` | kube-proxy binary staged with the Kubernetes downloads |
| `/usr/local/bin/crictl` | CRI debugging client matched to the Kubernetes version |
| `/usr/local/bin/containerd` | containerd binary |
| `/usr/local/bin/runc` | runc binary |
| `/usr/local/bin/containerd-shim-runc-v2` | containerd shim for runc |
| `/opt/cni/bin` | CNI plugin binaries, including `bridge`, `host-local`, and `loopback` |
| `/etc/cni/net.d` | CNI network configuration directory read by containerd |
| `/etc/containerd/config.toml` | containerd configuration generated by the agent |
| `/etc/containerd/conf.d` | containerd drop-ins, including the NVIDIA runtime drop-in |
| `/etc/containerd/certs.d` | Registry host configuration, including the Gantry endpoint |
| `/etc/systemd/system/containerd.service` | containerd unit run by the machine's systemd |
| `/etc/systemd/system/kubelet.service` | kubelet unit run by the machine's systemd |
| `/etc/kubernetes` | kubelet PKI material and static pod manifests |
| `/var/lib/kubelet` | kubelet configuration and kubeconfig |
| `/etc/cdi/nvidia.yaml` | NVIDIA CDI specification on GPU machines |

The rootfs phase also writes operating system configuration that the node depends on. It writes `/etc/hostname` from the host hostname so the machine identifies itself consistently, installs a tmpfiles rule that creates the `/dev/kmsg` symlink kubelet expects, installs a `modules-load.d` file for the kernel modules the node needs, and writes an architecture-specific apt sources list. That sources list is pinned to Ubuntu Noble content, so keep it in mind if you build a custom rootfs image for another distribution.

The rootfs is not a disk image, a partition, or a virtual machine. Nothing is installed into a filesystem image, and there is no separate kernel. The machine boots the host kernel using the host's `/lib/modules`, which the generated `.nspawn` file mounts read-only. This is why a rootfs image must be compatible with the host kernel's module layout and why the agent never tries to replace the host kernel from inside the machine.

### OCI Images

The agent selects the rootfs image from the first source that is present. An explicit `OCIImage` in the agent configuration wins. If it is empty, `AGENT_DISABLE_OCI_IMAGE` can disable image usage, and `AGENT_OCI_IMAGE` can name an image without editing the configuration. When none of those is set, the agent picks a built-in default based on the host distribution and on whether NVIDIA GPUs were discovered.

Registry images are copied into a local OCI layout with ORAS before their layers are unpacked. A local layout or an HTTPS archive is acquired into the same layout abstraction, so the unpack code sees one format regardless of where the image came from.

Host distribution detection reads `/etc/os-release` and recognizes Ubuntu 24.04, Ubuntu 26.04, Azure Linux 3, and Azure Container Linux. Azure Container Linux is classified before Azure Linux because it reports an Azure Linux identifier but is an immutable Flatcar-derived image with a read-only `/usr`. Hosts that identify as RPM-based without a more specific match also fall back to the Azure Linux 3 rootfs.

The default images and their purpose are listed below. The tags are dated constants in the agent source; treat them as the version shipped with this agent and override `OCIImage` when you need a different build.

| Host condition | Default image |
| --- | --- |
| Ubuntu 24.04, no NVIDIA GPU | `ghcr.io/azure/agent-ubuntu2404:v20261031` |
| Ubuntu 24.04, NVIDIA GPU | `ghcr.io/azure/agent-ubuntu2404-nvidia:v20261031` |
| Ubuntu 26.04, no NVIDIA GPU | `ghcr.io/azure/agent-ubuntu2604:v20261031` |
| Ubuntu 26.04, NVIDIA GPU | `ghcr.io/azure/agent-ubuntu2604-nvidia:v20261031` |
| Azure Linux 3 or another RPM-based host, no NVIDIA GPU | `ghcr.io/azure/agent-azlinux3:v20261031` |
| Azure Linux 3 or another RPM-based host, NVIDIA GPU | `ghcr.io/azure/agent-azlinux3-nvidia:v20261031` |
| Unrecognized host, no NVIDIA GPU | `ghcr.io/azure/agent-ubuntu2404:v20261031` |
| Unrecognized host, NVIDIA GPU | `ghcr.io/azure/agent-ubuntu2404-nvidia:v20261031` |

The `-nvidia` variants extend the base image with the NVIDIA Container Toolkit. That matters because the toolkit lives inside the machine, not on the host, when the node runs in nspawn mode. The host supplies the kernel driver, the device nodes, and the driver's userspace libraries. The `NvidiaToolkit` configuration field is explicitly documented as applying to `none` isolation only; nspawn uses the copies in the rootfs image.

`OCIImage` accepts three forms. It can be an OCI registry reference such as `ghcr.io/azure/agent-ubuntu2404:v20261031`, a local OCI image layout directory, or an HTTPS URL to a tarred OCI layout. An HTTPS archive must contain exactly one tagged image reference, which the agent selects automatically. HTTPS URLs may contain signed query parameters, and the agent redacts the query string when it logs the image. The image must use OCI media types and include a platform manifest matching the host architecture.

## Machine Configuration

The agent generates three host-side files for each machine. The settings file lives in the trusted systemd-nspawn directory, the service drop-in supplies environment variables and cgroup device rules, and the oneshot unit regenerates the settings before the machine starts.

| Host path | Purpose |
| --- | --- |
| `/etc/systemd/nspawn/<machine>.nspawn` | nspawn settings for the machine |
| `/etc/systemd/system/systemd-nspawn@<machine>.service.d/override.conf` | service drop-in with environment variables and `DeviceAllow=` rules |
| `/etc/systemd/system/unbounded-agent-regenerate-config@<machine>.service` | oneshot unit that refreshes host-side configuration before each start |

The split between the settings file and the drop-in is deliberate. Settings that systemd-nspawn reads from its own configuration, such as capabilities, the user namespace choice, the syscall filter, the network mode, and bind mounts, belong in the `.nspawn` file. Settings that systemd itself must apply before starting the machine, such as the service restart policy, the environment variables consumed by systemd-nspawn, and the cgroup device controller rules, belong in the drop-in.

The drop-in does not override `ExecStart`. The default `systemd-nspawn@.service` template starts systemd-nspawn with `--settings=override`, which gives settings in the trusted directory precedence over command-line flags. That is why `PrivateUsers=no` in the settings file overrides `-U`, and why `VirtualEthernet=no` overrides `--network-veth`, without replacing the vendor unit. Put durable changes in the agent configuration. The generated settings and drop-in are replaced on the next regeneration, and the vendor unit is not the place to carry local policy.

The following directives are the core of the generated settings. They are present on every machine, not only on GPU hosts.

| Directive | Effect |
| --- | --- |
| `Capability=all` | Grants the capabilities runc needs for nested containers, including `CAP_SYS_ADMIN`, `CAP_NET_ADMIN`, and `CAP_MKNOD` |
| `PrivateUsers=no` | Keeps real UID 0 inside the machine so runc can create namespaces |
| `SystemCallFilter=@keyring bpf perf_event_open` | Adds the syscall groups and calls containerd, runc, and eBPF CNIs need |
| `VirtualEthernet=no` | Shares the host network namespace instead of creating a veth pair |
| `BindReadOnly=/lib/modules` | Exposes the host kernel modules to the machine |
| `Bind=<bpffs>:/sys/fs/bpf` | Gives the machine its own pinned BPF object namespace |
| `BindReadOnly=<archive>:/var/lib/unbounded/container-images` | Exposes staged offline container image archives |

The service drop-in sets two environment variables that change how systemd-nspawn initializes the machine. `SYSTEMD_NSPAWN_UNIFIED_HIERARCHY=1` forces a cgroups v2 hierarchy inside the machine even when the nspawn version would otherwise infer an older hierarchy from the container image's systemd version. `SYSTEMD_NSPAWN_API_VFS_WRITABLE=network` makes `/proc/sys/net` writable while leaving the rest of `/proc/sys` read-only, so CNI plugins and kube-proxy can set network sysctls that apply to the shared network namespace.

The drop-in also always allows the `char-ipvtap` and `char-macvtap` device classes with read, write, and mknod access. Those classes support network virtualization workloads that create tap devices, and they are allowed even when no tap device exists at start time so that a device created later can be used without regenerating configuration.

Device discovery runs when the goal state is resolved, which happens before the machine is built. The agent probes for `/dev/kvm`, `/dev/mshv`, `/dev/net/tun`, `/dev/vhost-net`, block device nodes listed in `/sys/class/block`, and the `/dev/infiniband` directory. A path is included only when it exists and is accessible to the agent. Block devices are included as whole disks and partitions, with `loop`, `ram`, `zram`, `fd`, and `sr` entries excluded as pseudo or removable devices. Device-mapper (`dm-*`) and software RAID (`md*`) devices are not excluded because they back real storage.

Every discovered device path becomes a `Bind=` entry and a matching `DeviceAllow=<path> rwm` rule. A path that grants access to a device node is not enough on its own, because the cgroup device controller must also allow the node. The generated configuration keeps the two together so that a bound device is usable inside the machine.

InfiniBand is handled as a directory rather than as individual nodes. When `/dev/infiniband` exists, the agent mounts the whole directory and grants the device classes `char-infiniband_verbs`, `char-infiniband_mad`, and `char-infiniband_cm`, plus the path `/dev/infiniband/rdma_cm`. The class rules cover device nodes created after the machine starts, which is important for RDMA workloads that create nodes on demand. If the directory is absent, nothing is mounted, because a bind source that does not exist prevents systemd-nspawn from starting. When RDMA-CM is supported but its node is missing, discovery attempts to load the `rdma_ucm` or `rdma_cm` module and creates the node from the major and minor numbers published by the kernel.

### Containerd image verification

containerd supports verifying an image with external executables before it is admitted. The agent enables that plugin in the generated containerd configuration with a fixed binary directory inside the machine, a maximum of ten verifiers, and a ten second timeout per verifier.

```toml
[plugins."io.containerd.image-verifier.v1.bindir"]
  bin_dir = "/opt/containerd/image-verifier/bin"
  max_verifiers = 10
  per_verifier_timeout = "10s"
```

The agent does not install a verifier. In a default installation the directory is absent or empty, and containerd has no verifier to run, so pulls proceed without verification. To opt in, mount a host directory of verifier executables read-only over `/opt/containerd/image-verifier/bin` with `AdditionalHostMounts`:

```json
{
  "AdditionalHostMounts": [
    {
      "Source": "/opt/image-verifiers",
      "Target": "/opt/containerd/image-verifier/bin",
      "ReadOnly": true
    }
  ]
}
```

Add that array to the agent configuration that is used for the machine, then restart the node so the nspawn settings are regenerated. The source must exist before the machine starts, because systemd-nspawn refuses a bind whose source is missing. The target directory does not need to exist; systemd-nspawn creates the mount point when it starts the machine. Each executable must be built for the machine's architecture, which is the host architecture.

The verifier applies when containerd pulls an image through its CRI plugin. A verifier that exits non-zero denies the pull, and an executable that cannot be run also denies the pull. The agent does not install or order the executables; containerd enumerates the directory and applies the configured count limit. Images that are already in containerd's content store are not verified again on node restart. The agent's offline image path imports archives with `ctr images import`, which also bypasses the CRI verifier, so treat verification as a control on new pulls rather than as an audit of everything the node can run.

### Customization points

Most customizations belong in the agent configuration, not in the generated files. The relevant fields are `OCIImage`, `AdditionalHostDevices`, `AdditionalHostMounts`, and `NSpawnLifecycle`. Generated files are rewritten whenever the machine's configuration is regenerated, including by the pre-start hook, so an edit made by hand survives only until the next start.

`AdditionalHostDevices` accepts either an absolute clean path under `/dev` or a systemd device group specifier such as `char-input`, `char-pts`, or `block-*`. Entries may not contain whitespace or a colon. Path entries become `Bind=` and `DeviceAllow=` pairs; group specifiers become `DeviceAllow=` entries only. The following example passes a game controller and the input device class into the machine:

```json
{
  "AdditionalHostDevices": ["/dev/uinput", "char-input"]
}
```

`AdditionalHostMounts` accepts a `Source`, an optional `Target`, an optional `ReadOnly` flag, and optional `DeviceAllow` entries. Both paths must be absolute, clean, and free of whitespace and colons. An empty `Target` defaults to the `Source` path. The agent recommends read-only mounts unless the workload needs to write back to the host, and the field maps directly to `BindReadOnly=` or `Bind=`. The `DeviceAllow` entries in a mount become cgroup rules, which is how a mounted directory of device nodes becomes usable without regenerating the configuration for each new node.

```json
{
  "AdditionalHostMounts": [
    {
      "Source": "/srv/models",
      "Target": "/srv/models",
      "ReadOnly": true
    },
    {
      "Source": "/dev/dri",
      "ReadOnly": false,
      "DeviceAllow": ["char-drm"]
    }
  ]
}
```

NVIDIA devices and libraries are discovered automatically and do not need to be listed in `AdditionalHostDevices`. The agent binds every discovered GPU device node, maps `/dev/nvidia-caps` and `/dev/nvidia-caps-imex-channels` to their kernel device classes so capability nodes created later remain usable, and mounts each unique host library directory read-only under `/run/host-nvidia/<index>` with the i386 directories under `/run/host-nvidia-i386/<index>`. `/run/udev` is mounted read-only so GPUDirect Storage can resolve host block device properties. The host directory containing `nvidia-smi`, `nvidia-imex`, and `nvidia-imex-ctl` is mounted read-only at `/run/host-nvidia-bin`, and the post-start hook copies the host-matched helpers into the driver root it builds inside the machine.

AMD GPU discovery is independent of NVIDIA discovery. When `/dev/kfd` exists, the agent binds it together with the `/dev/dri` card and render nodes, and mounts `/sys/module/amdgpu`, `/sys/class/kfd`, `/sys/class/drm`, and `/sys/devices` read-only so the AMD device plugin can read KFD topology. A device path that NVIDIA already owns is not granted twice.

#### System call filter

The generated syscall filter extends the default system call allow list with the `@keyring` group and the `bpf` and `perf_event_open` syscalls. Each has a concrete consumer inside the machine:

- `@keyring` is needed by containerd, which uses the kernel keyring subsystem for image and runtime bookkeeping.
- `bpf` is needed by runc and by eBPF-based CNI plugins to create, load, and query BPF maps and programs.
- `perf_event_open` is needed by Cilium and similar CNIs that use perf event rings to receive kernel events.

If you replace the CNI with one that needs a syscall outside that set, the machine starts, but the CNI or its pods fail at runtime. Add the syscall to the filter rather than disabling the filter entirely, and test the change on a host that can be reprovisioned. Because the settings file is regenerated on every start, a hand edit to the filter is not a durable fix.

### Lifecycle hooks and the helper

Each machine has a pre-start and a post-start hook. By default both hooks run a rollback-stable helper binary installed on the host. The helper is a copy of the running agent, installed under the agent's host root, so an AgentUpgrade that moves the daemon's current symlink does not break hooks that were generated by an older agent. The helper's commands are `<host root>/bin/unbounded-agent-nspawn-lifecycle nspawn-lifecycle pre-start <machine>` and the matching `post-start` command.

`NSpawnLifecycle` replaces the helper commands when an application runs the lifecycle phases another way, such as from a container. If the field is set, both `PreStartCommand` and `PostStartCommand` are required. Each must be a single systemd command that begins with the command's absolute path, must not contain control characters or a lone semicolon, and must not end in a backslash. The agent does not install the helper when configured commands are supplied, and it reinstalls the helper when a machine returns to the default behavior and the helper is missing.

The pre-start hook migrates the agent's host root before it regenerates configuration, because the generated hook command names the helper under that root. If the host root cannot be migrated, the machine starts with the configuration it already has rather than not starting at all. The daemon refuses to start on such a host, which is where the migration problem is reported.

The pre-start hook does two jobs. It regenerates the machine's host-side configuration from the applied config, and it runs the allocation launch guard before the machine is allowed to start. The guard is what prevents a host whose allocation was withdrawn from restarting the node after a reboot. A transient guard failure exits with status 1 and the regeneration unit runs the hook again. A final allocation refusal exits with status 77, which the unit names in `RestartPreventExitStatus=`, so the start fails instead of looping.

The pre-start hook reads the applied configuration at `/etc/unbounded/agent/<machine>-applied-config.json` and its SHA-256 sidecar at `/etc/unbounded/agent/<machine>-applied-config.json.sha256`. The agent writes that file after a successful node start and removes it when the machine is removed. During the initial bootstrap window, before any applied config exists, the hook keeps the freshly provisioned configuration instead of failing.

The post-start hook reconciles NVIDIA state after the machine is up. It waits for the machine to answer commands, rebuilds the driver root at `/run/nvidia/driver` from the host libraries and helpers, creates the container library links, runs `ldconfig`, and generates `/etc/cdi/nvidia.yaml` with `nvidia-ctk cdi generate`. On a host without NVIDIA devices it logs that it is skipping the rewiring and returns without changing the machine.

The generated configuration unit runs before the machine unit and depends on `systemd-udev-settle.service`, so device nodes have settled before configuration is rendered. The machine unit requires and orders after the regeneration unit and after `nftables-flush.service`. This ordering is what gives each start a configuration that reflects current devices and a ruleset that starts from a clean state.

## What Runs Inside the Container

Inside the machine, systemd is PID 1 and owns the service graph. It starts containerd first, then kubelet, and it starts the optional LocalDNS units when that feature is enabled. Each service is a normal systemd unit with a normal journal, so `systemctl` and `journalctl` inside the machine behave as they would on a host.

containerd is the container runtime for pods. It runs under the machine's cgroup subtree, uses runc as its runtime, reads its configuration from `/etc/containerd/config.toml` and its drop-ins, and writes container state below the machine root. The NVIDIA runtime handler is registered through a drop-in when a GPU is present, and the CDI specification at `/etc/cdi/nvidia.yaml` tells containerd how to inject the driver into GPU containers.

kubelet runs inside the machine, reads its kubeconfig and configuration from `/var/lib/kubelet`, registers the node with the Kubernetes API server, and asks containerd to start pods. The CNI plugins under `/opt/cni/bin` create pod network interfaces in the shared network namespace. Those interfaces are visible from the host as well as from the machine, because both share the same network namespace.

The optional LocalDNS feature also runs inside the machine. Its CoreDNS instance reads a Corefile, an upstream list, and an environment file under `/etc/unbounded/localdns`, listens on the node and cluster listener addresses, and uses a systemd slice for its CPU and memory limits. The agent waits for both readiness endpoints before it continues to start containerd and kubelet.

On the host, the unbounded agent and its daemon run outside the machine. The daemon resolves goal states, writes the rootfs and host configuration, starts and stops the machine, executes commands inside it, streams its journal, and reconciles the node after restarts. `machinectl` is the host-side machine control command, and it can enter the machine when you need an interactive shell. `systemctl` exists on both sides: the host systemd owns the machine unit, while the machine's systemd owns containerd and kubelet.

The agent reaches the machine with `systemd-run --machine=<machine> --pipe --wait`, which runs a command in the machine's systemd and waits for it to finish. It reads logs with `journalctl --machine=<machine>`, optionally filtered by unit. This is also what the `unbounded-agent exec` and log commands use, so the same path works for operators and for the agent's own phases.

NVIDIA drivers are not installed inside the machine. The host kernel driver owns the devices, and the machine receives the device nodes plus read-only mounts of the host's userspace libraries. The post-start hook assembles those libraries into a driver root that NVIDIA tooling can consume and generates the CDI specification from that root. This is why the rootfs image can be updated independently of the host driver, and why a host driver upgrade requires a post-start reconciliation rather than a rootfs rebuild.

| Component | Runs on the host | Runs inside the machine |
| --- | --- | --- |
| unbounded agent daemon | Yes | No |
| nspawn lifecycle helper | Yes | No |
| systemd | Host systemd manages the machine unit | PID 1 inside the machine |
| containerd and runc | No | Yes |
| kubelet | No | Yes |
| CNI plugins | No | Yes |
| LocalDNS | No | Yes, when enabled |
| NVIDIA kernel driver | Yes | No |
| NVIDIA userspace libraries | Yes, mounted read-only | Yes, linked into the driver root |

## Lifecycle

A machine moves through a small number of states. It is provisioned into an empty rootfs, configured, started, reconciled, restarted when its configuration changes, and eventually stopped or removed. The agent keeps the host-side state and the machine state separate so that a failed start does not require reprovisioning the rootfs, and so that a rootfs replacement can be rolled forward without touching the old machine until the new one is healthy.

### Agent daemon rollback and start limits

The daemon that drives all of this has its own recovery path, because a daemon that cannot start cannot repair a machine. The daemon unit is a simple service with a start limit of three starts in sixty seconds. When it fails, systemd starts the recovery unit through `OnFailure=`.

The recovery unit runs a script that selects the last-known-good agent binary, refuses a binary that cannot operate the isolation mode recorded for the node, resets the unit's failed state, and restarts the daemon. The mode check matters because a rollback to an agent that predates a mode change would either fail to find the node or operate it as an nspawn machine when it is not one.

If the restart fails, the script reads the daemon unit's effective `StartLimitIntervalUSec` and waits for that window plus a five second margin before making one retry. It accepts only a finite, positive interval of at most five minutes, and it exits with a diagnostic for an unknown, infinite, zero, or excessive interval rather than waiting forever or silently disabling the limit. It also checks that the binary selection has not changed while it slept, so it does not overwrite a newer selection.

After the retry, the script waits five seconds and verifies that the daemon is active and that the main process still runs the expected last-known-good executable. This catches a simple service that reports startup success and then exits immediately. The recovery unit itself has a seven minute start timeout, which covers the bounded cooldown, the service operations, and the liveness checks.

### Startup

Startup begins on the host. The host preparation phase ensures the packages the mode needs are present: `systemd-container`, `curl`, `nftables`, and `util-linux` on both Debian and RPM families. It installs them on hosts with a usable package manager and probes for their executables on image-managed hosts where package installation is not possible. It writes `/etc/sysctl.d/99-kubernetes.conf` and applies it with `sysctl --system`, so the kernel settings kubelet and the CNI expect are already correct before any machine exists. It also installs and enables the nftables flush unit, which gives each boot a clean ruleset before the node starts.

The rootfs phase comes next. It bootstraps the machine directory from the OCI image if the directory is empty, writes the generated nspawn files, installs the lifecycle helper when the machine uses it, and then downloads the Kubernetes, CRI, and CNI artifacts in parallel with writing the operating system configuration. On a machine that is being reprovisioned into an alternate slot, this work happens entirely in the new slot while the old machine keeps running.

The node start phase writes containerd and kubelet configuration into the rootfs, writes the LocalDNS inputs and network units, and starts the machine. Starting the machine means enabling and starting `systemd-nspawn@<machine>.service` through `machinectl`, which also runs the unit's post-start hook and reconciles NVIDIA state. If the post-start hook fails, the machine unit fails and the drop-in restarts it after ten seconds; the start-rate limit is disabled so a transient failure cannot leave the unit permanently failed. If the start fails because a stale machined registration already exists, the agent clears the unit's failed state, terminates the stale registration, waits up to fifteen seconds for it to disappear, and retries once. It then waits up to thirty seconds for the machine to answer a trivial command, because `machinectl start` returns before the machine's D-Bus is ready.

Once the machine answers, the agent waits for LocalDNS readiness when enabled, starts containerd, imports any staged container image archives, and starts kubelet. The surrounding bootstrap or repave flow then waits for kubelet to become healthy. If the node was already running before this start and the configuration it wrote differed from what was on disk, it restarts the services whose configuration it changed so they read the new files.

A restart of an existing machine is a restart of the machine unit, not a reprovision. The agent restarts `systemd-nspawn@<machine>.service`, which runs the pre-start configuration refresh and the post-start hook again, then waits for LocalDNS and kubelet. The rootfs is preserved. A NodeReboot operation follows the same path: it verifies the allocation launch guard while the node is still running, restarts the machine, and reports completion. It does not rebuild the rootfs or change the Kubernetes version.

After a host reboot, systemd starts the enabled machine unit, which requires the regeneration unit and orders after the nftables flush. The pre-start hook checks the allocation launch guard and refreshes the configuration, then the machine starts and the post-start hook reconciles devices and NVIDIA state. This is why the applied configuration file and the helper binary must survive reboots and agent upgrades.

### Removal

Removing a machine starts with a graceful stop inside the machine. The agent asks systemd inside the machine to stop kubelet and then containerd, tolerating failures, so the nspawn teardown does not have to force-kill the node's processes. It then disables the machine unit with `machinectl disable`.

The disable step can fail for benign reasons, most often because the machine was never enabled. The agent does not ignore that failure blindly, because an enablement symlink outlives the configuration and rootfs that removal deletes, and a unit left enabled makes the next boot try to start a machine that no longer exists. It inspects the unit's `UnitFileState` and continues only when the state cannot start the unit at boot, such as `disabled`, `not-found`, `masked`, or `static`. A failed inspection aborts the removal.

The agent inventories machined to see whether the machine is still registered. If it is not, there is nothing left to stop. If it is, the agent stops the nspawn service and waits up to thirty seconds for the machine to deregister. If it is still registered after that, the agent calls `machinectl terminate` and waits up to fifteen more seconds. A machine that remains registered after a forced termination is an error rather than a silent success.

Rootfs removal uses `machinectl remove` in a retry loop for up to sixty seconds, with a two second interval, because the image can stay busy briefly while cgroup and mount teardown completes. The loop also checks registration on each attempt. Once machined no longer knows the machine, the agent falls back to deleting the directory directly if `machinectl remove` still fails. That fallback covers hosts where the remove command is denied, for example Fedora with SELinux enforcing denying systemd-machined permission to create its image lock file under `/run/systemd/nspawn/locks`. The agent refuses the direct deletion if the machine is still registered.

The remaining host artifacts are removed in the same sequence. The `.nspawn` file, the service drop-in directory, and the configuration regeneration unit are removed first. The machine rootfs is removed next. The machine's private bpffs mount is unmounted and removed last. The mount is only unmounted when `mountpoint` reports that it is a mount, and a path that is not a mount is left for the directory removal rather than treated as an error.

A repave uses the same primitives in a different order. The agent provisions the new rootfs in the alternate slot, stops the old node, cleans up node-level network state, starts the new node, persists its applied configuration, waits for kubelet on the new machine, and only then removes the old machine and its applied configuration. The old rootfs is therefore available for inspection until the new node is proven healthy.

A plain stop is different from removal. Stopping a node disables and stops the machine unit and waits for deregistration, but leaves the rootfs and the generated configuration in place. The next start can reuse them. Reset and repave are the operations that delete state.

## Networking

The machine uses the host network namespace. `VirtualEthernet=no` is the setting that makes this true, and it is present on every generated machine configuration. The agent does not create a veth pair, a bridge, a NAT rule, or a per-machine network namespace. Inside the machine, `lo` and the host's physical interfaces look exactly as they do on the host, and every address the machine binds is an address on the host.

kubelet listens on `0.0.0.0:10250` by default in that shared namespace, which means the kubelet port is reachable on the host's addresses. When `Kubelet.NodeIP` is set, the agent passes it to kubelet as `--node-ip`, so the node advertises the address you chose instead of letting kubelet infer one. This is the usual way to make a node's identity stable when the host has multiple interfaces.

CNI plugins create pod interfaces in the same namespace. A pod's veth peer, the host-side address, and any routes or policy routing the CNI installs are visible from the host. That is intentional: it keeps pod networking identical to a node installed directly on the host, and it lets host-network pods, kube-proxy, and the CNI agent see the same interfaces. The cost is that pod and node traffic share the host's routing table and firewall.

Each machine gets a private bpffs mount on the host at `/run/bpffs/<machine>`, bind-mounted into the machine at `/sys/fs/bpf`. The machine unit creates the host mount point and mounts a bpffs there before systemd-nspawn starts, if it is not already a mount. eBPF-based CNIs can create and pin maps in that mount without seeing pins from the other machine slot. The cleanup path unmounts and removes it when the machine is removed.

The host applies the kernel settings that kubelet and the CNI expect. These include `net.ipv4.ip_forward`, the bridge netfilter calls for iptables and ip6tables, `net.ipv4.conf.all.src_valid_mark`, and relaxed reverse path filtering. The file is written to `/etc/sysctl.d/99-kubernetes.conf` and applied before the machine starts. Inside the machine, most of `/proc/sys` remains read-only, but `/proc/sys/net` is writable so CNI plugins and kube-proxy can set network sysctls at runtime.

DNS is handled without a resolver daemon inside the machine. The rootfs masks `systemd-resolved` so it cannot overwrite `/etc/resolv.conf` with an empty configuration, and copies the host's resolver configuration into the machine as a static file. Because the network namespace is shared, the host's stub resolver at `127.0.0.53` remains reachable from inside the machine.

When LocalDNS is enabled, the agent writes a Corefile, upstream list, environment file, and slice into the machine, and points the machine's resolver at the LocalDNS node listener. On the host, it also creates a dummy interface and NOTRACK rules that the listener addresses depend on, and it installs a drop-in that makes the machine unit require and order after the LocalDNS network unit. The agent waits for both LocalDNS readiness endpoints before starting containerd and kubelet.

The host's nftables ruleset is shared. The agent installs the `nftables-flush.service` oneshot unit, which runs on every boot while enabled and flushes the ruleset to a clean state before the node starts. The machine unit orders after that service. If the agent finds a node already registered while it is deciding whether to start the flush, it leaves the live ruleset alone, because flushing would erase the running node's kube-proxy and CNI rules. This is also why the flush unit is a boot-time reset rather than something the agent runs on every start.

Port conflicts are real in this mode and are checked before installation. Preflight verifies that kubelet's `0.0.0.0:10250` and containerd's metrics address are not already in use, and accepts a listener only when it can prove that it belongs to this installation. Two nodes cannot share a host in nspawn mode, and the alternate slot exists for replacement, not for running both machines at once.

## Limitations

The shared network namespace is the most important limitation. There is no per-machine network isolation, no separate IP address for the machine, and no NAT boundary between the machine and the host. A host firewall rule, a routing change, or a port conflict affects the node, and a node rule affects the host. If you need a machine with its own network identity, nspawn is not the right mode.

The machine is not a security boundary. The generated configuration grants all capabilities, keeps real UID 0, and adds syscalls that container runtimes need. A process inside the machine can reach host devices that are bound into it, and a pod that escapes its container reaches a root context with broad privileges. Use nspawn for compatibility and filesystem isolation, and use separate hosts or stronger sandboxing when you need tenant isolation.

The rootfs must match the host architecture and be compatible with the host kernel. The agent downloads artifacts for the host architecture and the machine boots the host kernel with the host's `/lib/modules`. A rootfs image built for a different architecture, or one whose userland expects a kernel feature the host lacks, will fail at unpack time or during boot.

Configuration changes that affect the nspawn settings require a machine restart. The pre-start hook regenerates the settings, but the running machine does not adopt a new device bind, mount, or environment variable until its unit restarts. The agent restarts the unit when it needs to apply a change, so expect a short node outage for changes such as adding a host mount or a device.

Machine names are fixed to the `kube1` and `kube2` alternation. The lifecycle helper validates the machine name, and the repave flow assumes the alternate slot is free. Do not rename a machine directory or create additional machine names by hand; the agent's cleanup and applied configuration paths are keyed to those names.

The offline image path has a verification gap. The CRI image verifier runs when containerd pulls an image, not when the agent imports an archive with `ctr images import` or when an image is already in the content store. If verification is a requirement for your environment, treat imported images as pre-approved and enforce their provenance before staging them.

InfiniBand and other hot-plug devices depend on discovery at configuration time. The InfiniBand directory and its device class rules cover nodes created later, but a device directory that does not exist when the configuration is rendered is not mounted at all. A device that appears after the machine starts is visible only if its directory was mounted or its class was allowed. Adding a new device class requires a configuration change and a machine restart.

## Key Paths

The following table collects the host paths that operators most often need. Paths under the agent host root are shown with the default root; on hosts installed by an older release, the same files may live under `/usr/local` until the host root is migrated.

| Host path | Purpose |
| --- | --- |
| `/var/lib/machines/<machine>` | Machine rootfs directory |
| `/etc/systemd/nspawn/<machine>.nspawn` | Generated nspawn settings |
| `/etc/systemd/system/systemd-nspawn@<machine>.service.d/override.conf` | Generated service drop-in |
| `/etc/systemd/system/unbounded-agent-regenerate-config@<machine>.service` | Generated configuration regeneration unit |
| `/run/bpffs/<machine>` | Host side of the machine's private bpffs |
| `/var/lib/unbounded/container-images/current` | Stable host symlink to staged image archives |
| `/var/lib/unbounded/container-images` | Archive path as seen inside the machine |
| `/etc/unbounded/agent/<machine>-applied-config.json` | Applied configuration read by the pre-start hook |
| `/etc/unbounded/agent/<machine>-applied-config.json.sha256` | Checksum sidecar for the applied configuration |
| `/opt/unbounded/agent/bin/unbounded-agent-nspawn-lifecycle` | Default lifecycle helper on current installations |
| `/usr/local/bin/unbounded-agent-nspawn-lifecycle` | Lifecycle helper path on legacy installations |
| `/opt/unbounded/agent/bin/unbounded-agent-daemon-recovery.sh` | Daemon recovery script on current installations |
| `/run/host-nvidia/<index>` | Read-only mount of a host NVIDIA library directory inside the machine |
| `/run/host-nvidia-i386/<index>` | Read-only mount of a host i386 NVIDIA library directory inside the machine |
| `/run/host-nvidia-bin` | Read-only mount of the host directory holding NVIDIA helpers |
| `/run/nvidia/driver` | Driver root assembled inside the machine for NVIDIA tooling |
| `/etc/cdi/nvidia.yaml` | Generated NVIDIA CDI specification inside the machine |
| `/opt/containerd/image-verifier/bin` | containerd image verifier directory inside the machine |
| `/etc/sysctl.d/99-kubernetes.conf` | Host kernel settings applied before the node starts |

Use `machinectl list` on the host to see registered machines and `journalctl --machine=<machine>` to read a machine's journal. The agent's own `exec` and log commands use the same machine access paths, so they work even when you do not have a shell on the host.

## See Also

- [Agent configuration reference]({{< relref "reference/agent/configuration" >}}) for the fields that drive image selection, devices, mounts, and lifecycle hooks.
- [Isolation modes]({{< relref "reference/agent/isolation" >}}) for how nspawn compares with the `none` and `container` modes.
- [none isolation]({{< relref "reference/agent/isolation-none" >}}) and [container isolation]({{< relref "reference/agent/isolation-container" >}}) for the alternative backends.
- [GPU support]({{< relref "reference/gpu" >}}) for driver and device plugin details that apply inside the machine.
- [Agent bootstrap guide]({{< relref "guides/agent" >}}) for installing an agent and producing the configuration used here.
- [Agent operations]({{< relref "guides/operations/agent-operations" >}}) for start, stop, restart, reset, and upgrade procedures.
- [Host operations]({{< relref "guides/operations/host-operations" >}}) for host packages, kernel settings, and reboot behavior.
- [Agent preflight]({{< relref "reference/agent/preflight" >}}) for the checks that run before a host is accepted.
