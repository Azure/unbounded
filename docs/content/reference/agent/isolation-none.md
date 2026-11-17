---
title: "none Mode"
weight: 5
description: "How the none isolation mode runs the Unbounded node stack directly on the host as agent-owned systemd units, including its requirements, layout, networking, devices, operations, and risks."
---

The `none` isolation mode runs the Kubernetes node stack directly on the host. The agent creates no systemd-nspawn machine, starts no container, and unpacks no rootfs image. Instead, the host's own systemd supervises containerd, kubelet, and optionally CoreDNS as agent-owned services, while the binaries they execute come from agent-owned versioned directories under `/opt/unbounded/node`. The agent and its daemon still run on the host, outside the stack, exactly as they do in every other mode.

The mental model is simple: in `none`, the boundary between the node and the host is removed. The node stack shares the host's mount namespace, PID namespace, network namespace, and cgroup tree. The node's shims and containers are visible as host processes, pod volume mounts are host mounts, and the container runtime socket is the host's socket at the usual path. That is the defining property of the mode, and it is both the reason to choose it and the reason to avoid it.

This page explains what the mode runs, what a host must provide, how the version directory and units are arranged, how networking and cgroups behave, how devices and GPUs reach workloads, what bootstrap, upgrade, reset, and diagnostics do, which configuration keys matter, and where the sharp edges are. Read it when you are deciding whether `none` fits a host, when a `none` bootstrap refuses a host, or when a node in this mode will not start.

{{< callout type="warning" >}}
`none` is not a sandbox and not a security boundary. The node stack runs as root on the host with host devices, host mounts, and host kernel facilities. Choose it only for a host that is dedicated to this node and whose owner accepts that authority. The mode is implemented end to end, but it has not been validated on real hosts to the degree `nspawn` has, and the design record still lists systemd, containerd, kubelet readiness, pod survival across a unit stop, the GPU path, and a read-only `/usr` as open verification items.
{{< /callout >}}

## What runs on the host

The node stack is a small set of host services that the agent owns, plus the files those services read. The services are ordinary systemd units with names that begin with `unbounded-`, so they cannot collide with a distribution unit of the same purpose.

`unbounded-containerd.service` runs containerd from the active version's `bin` directory with the agent's own configuration at `/etc/unbounded/containerd/config.toml`. It is a `Type=notify` service with `Delegate=yes`, `KillMode=process`, and an always-restart policy with a bounded backoff. The unit loads the `overlay` module as a best-effort pre-start step, raises its task, file descriptor, and core limits, and lowers its out-of-memory score so the runtime is not the first process the kernel reclaims under pressure. Its rendered configuration imports drop-ins from `/etc/containerd/conf.d`, uses the systemd cgroup driver for runc, reads CNI configuration from `/etc/cni/net.d`, and looks for plugins in `/opt/cni/bin`.

`unbounded-kubelet.service` runs kubelet from the same version's `bin` directory with `/var/lib/kubelet/config.yaml`. It wants and is ordered after `network-online.target` and the agent's containerd, never the distribution's. It has a `ConditionPathExists` on the version's `kubelet` binary, so a broken `current` link or an incomplete version directory shows up as a failed condition instead of an opaque execution error. The unit accepts exit status 143 as success, which keeps a deliberate termination from looking like a crash, and it passes four environment variables to the kubelet command line: `KUBELET_NODE_CONFIG_ARGS`, `KUBELET_TUNING_ARGS`, `KUBELET_KUBECONFIG_ARGS`, and `KUBELET_EXTRA_ARGS`.

`unbounded-localdns.service` runs the CoreDNS supervisor as a host unit when LocalDNS is enabled. It is the only stack unit with a hardened service sandbox: a systemd `DynamicUser`, only `CAP_NET_BIND_SERVICE` in its ambient and bounding sets, `NoNewPrivileges`, a private temporary directory, and strict protection of the system, home, kernel tunables, kernel modules, and control groups. It runs in `localdns.slice`, which carries the configured CPU and memory limits.

A fourth unit, `unbounded-kubelet-launch-check.service`, exists only on an allocation host. It is a oneshot that runs the agent's allocation launch check before every kubelet start, and a kubelet drop-in makes the kubelet require it and order after it. It is a separate unit rather than an `ExecStartPre` because `RestartPreventExitStatus` applies only to a unit's main process: a final allocation refusal has to fail the kubelet's start, not loop forever under the kubelet's `Restart=always`.

The CNI plugins are part of the version, not of the host package set. The CNI installer writes them to `/opt/unbounded/node/<version>/cni/bin`, and the backend links each plugin into `/opt/cni/bin` at a path that resolves through the `current` link. That is how a version flip also flips the plugins, while plugins that cluster add-ons install into the same directory remain usable.

The agent daemon is not part of this set. It runs as a host service in every mode, holds the installation lock, watches the Machine object, resolves the goal state, and drives the mode's backend. `unbounded-net-node`, gantry, and their charts are also host services rather than node stack components. They use the same host paths in every mode, so selecting `none` does not change their manifests, units, or network setup.

The distribution's `kubelet.service` and `containerd.service` are never used, edited, started, or removed. The agent-owned names are deliberately different so that a bug in this mode cannot stop a unit the host package manager owns. If the host already runs either distribution service, bootstrap refuses the host instead of taking it over.

## Why the mode exists

`nspawn` gives the node a private userland and a private service manager, and it is the right default for a general-purpose distribution host. It assumes, however, that the host can run a machine. Some hosts cannot. An image-managed host may have a read-only `/usr` and no supported way to install `systemd-container`. A host owner may refuse a machine layer for operational or compliance reasons. A host may be deliberately minimal, with no container manager and no intention of gaining one. In those cases the node still has to run, and `none` is the mode that does not require a machine runtime.

The mode also satisfies the same-path contract trivially. In the other modes, the node and the host must agree on paths such as `/run/containerd`, `/var/lib/kubelet`, `/etc/cni/net.d`, and `/opt/cni`, and the backend binds them into a private root. In `none` there is no private root, so those paths are already the host's paths. A CSI driver, a CNI plugin, a debug command, or a host-side automation that expects a host path sees exactly the path it expects, with no translation layer to get wrong.

`none` is useful when you want the node to use host-provided tools, devices, or kernel facilities directly, and when you are debugging whether a problem belongs to the node stack or to the machine layer. With `none`, there is no machine layer left to blame.

The tradeoff is explicit. `none` provides no filesystem, process, or user separation between the node and the host. If you need any of those, choose [`nspawn`]({{< relref "reference/agent/nspawn" >}}). If you need a machine only because the documentation says so, and the host is dedicated to the node, `none` removes a layer without pretending to add isolation.

## Requirements

A `none` host must be prepared for a node that writes to host paths and is supervised by host systemd. `unbounded-agent preflight --isolation none` checks the requirements without changing anything, and bootstrap enforces the ones it depends on as it runs, before the corresponding change. The preflight page describes the shared checks that every mode runs; this section covers what is specific to `none` or especially easy to miss.

### Writable paths and an untouched `/usr`

The agent needs to write `/etc`, `/var`, and its own base directory `/opt/unbounded/node`. It does not need `/usr` to be writable, and it does not write into `/usr/bin`, `/usr/sbin`, `/usr/lib`, `/usr/local/bin`, `/usr/local/lib`, or `/usr/local/sbin`. Node binaries and helper scripts stay under `/opt/unbounded/node`, which is why the mode works on image-managed hosts whose `/usr` is read-only.

The writability probe does not require the target directory to exist. It walks up to the nearest existing parent, creates a temporary file named `.unbounded-write-check-*` there, removes it, and reports a failed removal as an error, because a probe file left behind is itself a problem. This means a host where `/opt/unbounded/node` has not been created yet can pass, as long as `/opt` can be written. A path that exists but is not a directory is reported as an error, and the failure names the path you asked about rather than only the parent that was probed.

### Host commands and cgroups

The stack needs a working host `systemctl`, because every start, stop, restart, status check, and log query goes through it. The backend also expects a cgroup filesystem at `/sys/fs/cgroup`, and preflight fails when that path is missing. When the agent tears a node down, it kills pod cgroups through the cgroup v2 control files. It uses `cgroup.kill` when the kernel provides it and otherwise walks the subtree to signal every process in `cgroup.procs`, so an older kernel is not automatically disqualified, but a host without a cgroup mount is.

The mode has no requirement for `systemd-nspawn`, `machinectl`, or `systemd-container`. That is the point of the mode. If a check ever demands `systemd-container` on a host you are provisioning as `none`, the mode selection is wrong rather than the host.

The host must run systemd as its init and service manager. The mode has no other supervisor: units, drop-ins, readiness, and journal access all assume host systemd. Both preflight and bootstrap require root for the package, unit, and service operations, and preflight reports a non-root run as an error.

### Exclusive ownership of kubelet and containerd

This is the requirement most likely to stop a bootstrap. The agent refuses a host that already has a kubelet or containerd it does not own, because two kubelets or two container runtimes on one host fight over the same sockets, directories, and network rules. Ownership is checked three ways:

1. A unit file named `kubelet.service` or `containerd.service` in `/etc/systemd/system`, `/run/systemd/system`, `/usr/lib/systemd/system`, or `/lib/systemd/system`.
2. A `kubelet` or `containerd` executable in `/usr/local/sbin`, `/usr/local/bin`, `/usr/sbin`, `/usr/bin`, `/sbin`, or `/bin`.
3. A running process whose command name is `kubelet` or `containerd`, unless its executable lives under the agent's base directory.

A unit masked to `/dev/null` is exempt, because a mask cannot start and therefore is not a competing service. That exemption matters on a host that previously ran `nspawn`, because `nspawn` masks the distribution units and can leave the masks behind. A process whose executable is under `/opt/unbounded/node` is treated as the agent's own, which keeps a half-installed node from blocking its own repair.

The refusal is mandatory at bootstrap, not merely advisory in preflight. Bootstrap will not stop, disable, or take over a distribution unit, because doing so would mean guessing which service the host owner wants. The error names each finding and tells you to remove the existing kubelet and containerd or to use `nspawn`. It never switches the mode silently.

### Ordering and readiness

The kubelet unit wants and is ordered after `network-online.target` and the agent's containerd. The containerd unit itself starts earlier, after `network.target` and `local-fs.target`, and does not wait for full network online. When LocalDNS is enabled, a drop-in makes containerd and kubelet require LocalDNS and orders them after LocalDNS and the shared LocalDNS network unit. A start therefore waits at explicit gates instead of racing: LocalDNS readiness after its unit starts, then containerd, then kubelet.

The kubelet unit carries `ConditionPathExists` on the version's `kubelet` binary. If the `current` link is broken or the version directory is incomplete, the unit does not start and systemd reports the condition failure. That is a deliberate fail-fast in place of an exec error whose cause is harder to see.

### Package installation

The package set for `none` is `curl`, `nftables`, and `util-linux`. `systemd-container` is removed from the set for every mode other than `nspawn`, because only a machine needs it. The packages matter for the capabilities they provide, not for their names: `curl` for downloads, `nft` for the nftables flush unit and LocalDNS rules, and `mountpoint` for the private bpffs cleanup shared with the other modes.

No host package provides kubelet, containerd, runc, or the CNI plugins in this mode. Those are release artifacts installed into the version directory, so the host package database never learns about the node's runtime and a host package upgrade cannot replace it.

On a host with `apt-get`, the agent refreshes with `apt-get update -y` and installs missing packages with `apt-get install -y --no-install-recommends`. On a host with `tdnf` or `dnf`, it refreshes the metadata cache and installs with `-y`. Detection is by executable, not by distribution name. Only missing packages are installed, in a single transaction.

On an image-managed host, package installation is refused. The agent checks that every required capability already resolves and, if one is missing, reports that the host's `/usr` is read-only and the tool must come from the image or a system extension. It deliberately does not suggest installing anything there, because there is nowhere to install to.

A host with no package manager at all is handled by capability rather than by name. The agent checks for `curl`, `nft`, and `mountpoint` on `PATH`. If all three resolve, the host passes. If any is missing, bootstrap fails with a message naming the missing tool and the package that normally provides it. This path exists for immutable images that ship no supported package manager but already contain the tools.

### Selecting the mode

You can declare the mode at the Machine, at provisioning time, or in the agent configuration. The desired mode belongs in `spec.isolation` on the Machine:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: my-node
spec:
  isolation: none
```

The provisioning command accepts the same value and writes it into the generated agent configuration:

```bash
kubectl unbounded machine manual-bootstrap my-node --site dc1 --isolation none
```

An existing agent configuration can set the `Isolation` field directly:

```json
{
  "MachineName": "my-node",
  "Isolation": "none"
}
```

`unbounded-agent start` and `unbounded-agent preflight` accept `--isolation`, which overrides the loaded configuration for that run:

```bash
unbounded-agent preflight --isolation none
unbounded-agent start --isolation none
```

The flag is validated the same way as the configuration value. Unlike `container`, `none` needs no experimental opt-in in the configuration. Validation still rejects an unknown mode rather than coercing it to `nspawn`.

A host that has applied `none` records that mode under the name `none-kube1`, not `kube1`, in `/etc/unbounded/agent/none-kube1-applied-config.json`. An agent that predates the mode looks only for `kube1` and `kube2`, finds no applied configuration, and refuses the host instead of treating it as an `nspawn` machine. The corresponding binary capability check is `unbounded-agent supports-isolation none`, which exits zero only when the binary can bootstrap and operate the mode and reads nothing from the host.

## Layout

The layout is intentionally small. The agent owns one base directory and one set of unit names, and everything else is a host path that the node and the host share.

### Directory layout

The base directory is `/opt/unbounded/node`. Each installed version gets a directory below it, and the active one is selected by a relative symlink:

| Path | Contents |
|---|---|
| `/opt/unbounded/node/<version>/bin` | `containerd`, `runc`, `kubelet`, `coredns` when LocalDNS is enabled, and the other executables of that version |
| `/opt/unbounded/node/<version>/cni/bin` | the CNI plugin executables for that version |
| `/opt/unbounded/node/<version>/libexec` | the LocalDNS supervisor script, when LocalDNS is enabled |
| `/opt/unbounded/node/current` | a relative symlink to the active version |
| `/etc/unbounded/containerd/config.toml` | the containerd configuration the unit runs with |
| `/etc/unbounded/agent/none-kube1-applied-config.json` | the applied agent configuration recorded for this host |

The `current` link is relative, so the whole layout can be relocated. The base, version, and `bin` directories are created with mode `0755` regardless of the process umask, because the LocalDNS `DynamicUser` has to traverse them to reach its supervisor and configuration. A parent that already existed keeps its mode; the agent changes only what it created. The same rule applies to `libexec`, which the agent creates explicitly for the supervisor script.

The rest of the stack uses host paths directly. `/var/lib/kubelet`, `/var/lib/containerd`, `/run/containerd`, `/etc/cni/net.d`, `/opt/cni/bin`, and `/etc/kubernetes/pki` are the same paths the node would see in another mode. The installers that would otherwise write to `/usr/local/bin`, `/usr/local/libexec`, and `/opt/cni/bin` are redirected into the version directory. That redirection is what makes an upgrade a symlink flip rather than an overwrite of the running node's files.

A `none` node has no second blue/green slot. Its applied configuration is recorded under the fixed slot `kube1` as `none-kube1`, and its unit of replacement is the version directory. The old directory remains on disk until a successful upgrade prunes it, which is what allows a rollback to start the previous version again.

### Units and drop-ins

The mode manages the following units:

| Unit | Purpose |
|---|---|
| `unbounded-containerd.service` | Runs containerd with the agent configuration, delegates its cgroup subtree, and restarts on failure |
| `unbounded-kubelet.service` | Runs kubelet, ordered after network-online and the agent's containerd |
| `unbounded-localdns.service` | Runs the CoreDNS supervisor, only when LocalDNS is enabled |
| `unbounded-kubelet-launch-check.service` | Runs the allocation launch check before kubelet, only on an allocation host |

The backend writes the first two with every unit write and the last two only when the configuration asks for them. A LocalDNS unit that a previous configuration installed is disabled and, when the node is stopped, stopped before its file is removed. Retiring it in that order matters because the stack is derived from the unit files: a deleted unit would never be stopped again, and CoreDNS would keep the node's listeners.

The units are accompanied by drop-ins, which carry the host-specific values so the unit templates stay stable. They live in `/etc/systemd/system/unbounded-containerd.service.d` and `/etc/systemd/system/unbounded-kubelet.service.d`; the LocalDNS unit has its own drop-in directory for its path. The drop-ins are:

| Drop-in | Effect |
|---|---|
| `00-path.conf` | Puts the active version's `bin` first on `PATH` for containerd and kubelet |
| `05-localdns.conf` | Makes containerd and kubelet require LocalDNS and the LocalDNS network unit, and orders them after both |
| `10-launch-check.conf` | Makes kubelet require the launch-check oneshot and order after it |
| `10-gpu-setup.conf` | Adds the CDI regeneration commands as containerd `ExecStartPre` entries on a GPU host |

The `PATH` drop-in is what makes the version self-contained. containerd finds `containerd-shim-runc-v2` and `runc` in the version's `bin` directory, and kubelet finds the tools it shells out to there, without the agent changing the host's `PATH`. The GPU drop-in is written only when the resolved goal state requires NVIDIA support; on a node without GPUs it is removed, so a reconfigured host does not keep a stale command that would fail every containerd start.

The kubelet unit also reads an optional `EnvironmentFile=-/etc/default/kubelet`. The agent does not write that file and does not need it, but if the host has one, its `KUBELET_*` variables are part of the kubelet's command line. This is the one place where a host file can change the agent-owned kubelet without the agent rewriting it, and it is worth checking when kubelet behavior differs from the configuration the agent reports.

### Versioning and the current link

A version name is the Kubernetes version with any leading `v` removed, a hyphen, and eight hexadecimal characters from a SHA-256 digest. The digest covers the containerd version, the runc version, the CNI plugin version, the CoreDNS version, the host architecture, whether LocalDNS is enabled, and any download overrides. A configuration that changes only the CNI plugin version therefore lands in a new directory, while an unchanged configuration reuses the installed one.

The link is replaced atomically. The agent creates `<base>/current.new` as a relative symlink, renames it over `current`, and syncs the base directory so the flip survives a power loss. An update never leaves a moment in which an existing link is missing, and a crash either leaves the old link or the new one, never a partial state. The version name cannot be empty, `.` or `..`, cannot contain a slash or a NUL byte, and cannot be `current`; those rules keep a configuration value from escaping the base directory or replacing the link.

The version directory is also the rollback boundary. An upgrade installs the new version beside the running one, and a failed switch flips the link back and starts the previous version. The old version's binaries are not overwritten during preparation, so the rollback has something to run.

## Namespaces and cgroups

There is no namespace boundary in `none`. The units run in the host's mount, PID, network, IPC, UTS, and cgroup namespaces. A pod process appears in the host process table, a pod mount appears in the host mount table, and a container runtime socket is the host's socket. There is no user namespace either: containerd and kubelet run as root, and the only stack service that does not is LocalDNS, which runs as a systemd `DynamicUser` because it needs nothing more.

The host's systemd supervises the units directly. `unbounded-containerd.service` sets `Delegate=yes`, so the cgroup subtree below it is delegated to containerd and kubelet can create the pod cgroups there. The kubelet configuration sets `cgroupDriver: systemd` and `cgroupsPerQOS: true`, so pods live under `kubepods.slice` with the systemd driver. The backend's teardown also recognizes the `kubepods` name that the cgroupfs driver would create, because the cleanup must work even if the kubelet configuration was changed.

`KillMode=process` is a deliberate cgroup behavior, not an oversight. It lets the pods survive a containerd restart, which keeps a runtime upgrade or a crash from killing every workload on the node. The same setting means the pods also survive a plain stop of the containerd unit. Only an upgrade, a destroy, or a reset ends them: those paths kill the containerd shims that belong to the agent's containerd, then kill the `kubepods.slice` and `kubepods` cgroups. Killing a shim alone is not enough, because the pod's processes are reparented and keep running with their mounts and files open.

The cgroup cleanup uses `cgroup.kill` when it exists and falls back to reading `cgroup.procs` in every directory below the pod cgroup and signaling each process. A host whose cgroup mount is missing fails preflight, because neither path can work without it. A shim scan is skipped when the backend is built without a procfs path, which is how tests and offline tools avoid signaling real processes.

Because the node stack shares the host PID namespace, a container configured with `hostPID` can see the host's processes, and a privileged container can signal them. An ordinary pod keeps its own PID namespace, but that namespace is a child of the host's, not a boundary the host cannot see through. That transparency is useful for debugging and unacceptable for a hostile multi-tenant host. The security section returns to this point.

## Host networking and CNI

The node stack uses the host network namespace. Pods reach the host's interfaces directly, CNI plugins configure interfaces the host can see, and kube-proxy programs host rules. This is true in every isolation mode because pods need host-reachable addresses, but in `none` there is no second mount namespace and no machine or container boundary through which the networking has to be translated.

The CNI plugins are versioned with the node. The installer writes them to `/opt/unbounded/node/<version>/cni/bin`, and the backend links each plugin into `/opt/cni/bin`, where containerd looks for CNI binaries. The link resolves through `current`, so a version flip switches the plugins with the rest of the node. The link step is idempotent. A file in `/opt/cni/bin` that is not a symlink, or a symlink that points outside the agent's base, belongs to another component and is left alone with a warning. That is what lets cluster add-ons install their own plugins, such as the Unbounded networking IPAM plugin, without the agent deleting them on the next reconcile.

CNI configuration lives at `/etc/cni/net.d` and is written by the cluster's networking components, not by the isolation backend. The backend removes the directory during a reset because it has no meaning once the node is gone. During an upgrade, the CNI state is reset after the old pods end, and the configuration snapshot taken before the upgrade is restored on a rollback. The IPAM state that names pods which no longer exist is discarded on the forward path.

LocalDNS has a shared host network unit in every mode. `unbounded-localdns-network.service` creates the dummy interface and the NOTRACK rules that the local DNS path needs, and `unbounded-localdns.service` wants and is ordered after it. When LocalDNS is enabled, the node units require LocalDNS and are ordered after LocalDNS and the network unit through their drop-ins. The unit is restarted only when its files actually changed. That change tracking is not an optimization: restarting the network unit restarts every unit that requires it, which is the whole stack, so an unnecessary restart is an unnecessary outage.

The agent writes the Kubernetes sysctls to `/etc/sysctl.d/99-kubernetes.conf` and applies them with `sysctl --system` before the node starts. The values include the bridge netfilter settings that the CNI and kube-proxy paths need. The bridge settings require `br_netfilter`, so the agent loads the kernel modules before it applies the sysctls. On a `none` host there is no second namespace to contain the changes: the node writes host kernel tunables as itself, and a sufficiently privileged pod can do the same.

The nftables flush unit is host-global and installed in every mode. It runs a oneshot at boot that flushes the entire nftables ruleset to a clean state before the node starts, so stale rules left by Docker or an earlier configuration cannot interfere with Kubernetes networking. The `none` variant is ordered before the agent's containerd and kubelet units, and it orders itself after `iptables.service`, `ip6tables.service`, and `nftables.service` when those exist, so a host firewall is not applied after the flush. It answers "is a node running" by asking `systemctl is-active` about the two node units, and it fails closed: only a unit that is clearly inactive or failed counts as not running, so a live ruleset is never flushed on a guess.

Docker is disabled and configured during host preparation for the same reason. The agent stops and masks `docker.socket` and `docker.service`, so a host Docker cannot take the runtime socket back at boot, and it writes `/etc/docker/daemon.json` with `"iptables": false` so a Docker installation does not compete with Kubernetes for the host's rules. It does not remove Docker; it makes Docker yield the host's networking and runtime socket to the node.

bpffs is the one host-global path with a per-mode shape. `nspawn` and `container` create a private bpffs per slot under `/run/bpffs/<slot>` and bind it at `/sys/fs/bpf`. `none` has no private mount and uses the host's `/sys/fs/bpf` directly. If you run Cilium or another eBPF-based CNI that expects a per-node pinned-object namespace, that difference is worth understanding before you choose the mode. It is not a configuration option in `none`; the host path is the only path there is.

The host's `/etc/resolv.conf` is not modified in `none` mode. When LocalDNS is enabled, the agent writes a dedicated resolver file at `/etc/unbounded/localdns/resolv.conf` and points the kubelet at it through the kubelet configuration. The host resolver stays the host's, which is why the `none` flow writes one resolver file rather than rewriting `/etc/resolv.conf` as the machine flow does inside the machine. If you see the host resolver unchanged on a `none` host, that is correct.

## Devices, GPUs, and grants

There is nothing to bind in `none`. The kubelet and containerd see the host directly, so device nodes and driver libraries are already visible. What the agent still does is check that every resource the configuration asks for is where the host-native stack will look for it, so a configuration mistake becomes a preparation error instead of a runtime surprise.

A grant is honored natively when the thing it names is at its own path. A device node must be at its source path and cannot be remapped: a grant that asks for `/dev/foo` at `/dev/bar` is refused, because the stack has no private root in which to create the second name. A device grant that names only a device class, such as `char-input`, is accepted because the units run without a device cgroup policy and every class is already reachable. That is a check of the configuration, not a restriction on what a pod can open.

A mount whose target is empty or equal to its source is satisfied by checking that the source exists. A mount that would need a different target is refused, with one exception: the NVIDIA library and helper-binary grants that `nspawn` binds under `/run/host-nvidia` paths are satisfied by the host view, because on the host the libraries are already on the loader path and the helper binaries are on `PATH`. A library-path grant is refused outright, because the node uses the host's library configuration and the agent cannot change it for one node.

A read-only mount grant is accepted but not enforced. The stack runs on the host and sees the source with the host's own permissions, so there is no bind to make read-only. The agent logs a warning rather than silently pretending the request was honored. Grants that exist only so a tool can read a path, such as the AMD sysfs path or the NVIDIA library and helper-binary directories, are exempt from the warning because there is nothing to restrict. Do not rely on `ReadOnly` in a `none` configuration to protect a host path; use host file permissions instead.

A mount grant may carry a `DeviceAllow` list because the same configuration format serves `nspawn`. In `none` there is no device cgroup policy on the units to apply it to, so every device class is already reachable and the list changes nothing. It is not an error; it is simply a directive for a backend that has a device policy.

GPU support starts with the NVIDIA Container Toolkit, which must be installed on the host. The agent does not ship it in this mode, and it does not install it. Before the stack starts, it checks for `nvidia-ctk` and `nvidia-container-runtime` at their configured paths, which default to `/usr/bin/nvidia-ctk` and `/usr/bin/nvidia-container-runtime`. If GPUs are present and either path is missing, preparation fails with a message naming the path and saying that `none` requires the toolkit on GPU hosts. The `NvidiaToolkit` fields in the agent configuration override the paths when a host installs the toolkit elsewhere, and that field is valid only for `none`.

Preparation then changes two host-global files, in this order:

1. `nvidia-ctk config --set nvidia-container-runtime.mode=cdi --in-place` selects CDI runtime mode, so environment-variable requests from the device plugin resolve against the generated spec.
2. `nvidia-ctk cdi generate --driver-root / --dev-root / --output /etc/cdi/nvidia.yaml --disable-hook all --enable-hook create-symlinks --enable-hook update-ldcache` generates the spec from the host driver. The hook selection is deliberately narrow: only `create-symlinks` and `update-ldcache` are enabled.

The agent runs these before it starts the stack, and the containerd unit runs the same commands as `ExecStartPre` on every start through `10-gpu-setup.conf`. The second part matters because a host reboot or a driver change alters the devices and libraries the spec names. A stack started by systemd after a reboot must regenerate the spec before containerd starts GPU containers, not only when the agent happens to start it. The NVIDIA runtime itself is enabled through the agent-written containerd drop-in at `/etc/containerd/conf.d/99-nvidia-runtime.toml`, at the same path the machine's containerd uses.

AMD GPUs need no toolkit equivalent on this backend. Their device nodes and the sysfs paths the device plugin reads are host paths already, so the grants are existence checks and the stack uses them directly.

A reset removes the CDI spec and the NVIDIA runtime drop-in, which the agent generated, and leaves the toolkit's own runtime configuration, which belongs to the toolkit installation. Reinstalling or upgrading the toolkit is your responsibility, and a driver update is a reason to restart or repave the node so the spec is regenerated against the new driver.

## Operations

The operational surface is the same as in every mode: bootstrap, start, stop, restart, upgrade, destroy, status, exec, and logs. The difference is what each one does to the host. The sections below follow the order an operator meets them.

### Bootstrap and start

Bootstrap is the only path allowed to prepare a host from scratch. It runs in a fixed sequence, and each step refuses before changing anything if the host is not what the mode expects.

1. **Clean-host check.** The agent looks for artifacts of any previous node deployment. For `none` that includes the base directory and the agent-owned units. A host with traces of a previous node needs `unbounded-agent reset` before it can be bootstrapped again.
2. **Ownership check.** The agent refuses a kubelet or containerd it does not own, as described in the requirements. This check is mandatory here because the rest of the plan relies on it. The same condition appears in preflight as the `none-backend` check, but bootstrap does not depend on preflight having been run.
3. **Input resolution.** The agent resolves download overrides, offline container image archives, and the goal state for the `kube1` slot. GPU and device discovery runs once here, and the resolved goal state is reused by the node, so the same devices drive both bootstrap and the unit templates.
4. **Host preparation.** The agent prepares the host root, installs the bootstrap binary, installs missing packages, loads the Kubernetes kernel modules, and applies the host-global configuration. The module list is written to `/etc/modules-load.d/unbounded-kubernetes.conf` and contains `overlay` and `br_netfilter`; the modules are also loaded immediately, because the bridge netfilter sysctls need `br_netfilter` first. The sysctls go to `/etc/sysctl.d/99-kubernetes.conf` and are applied with `sysctl --system`.
5. **Host-global services.** The agent installs the nftables flush unit ordered before the node units, stops and masks Docker, writes the Docker daemon configuration, disables swap and masks active swap units, and removes the apt and needrestart guards that only an `nspawn` machine needs. Removing those guards matters: first-boot provisioning writes them for every mode, and they would otherwise hold back systemd and libcap security upgrades on a host with no machine.
6. **Node root preparation.** If the node is already running, the agent leaves it alone, because a running node owns its files. Otherwise it prepares credentials, downloads the offline archives, installs the version, links the CNI plugins, writes the units and drop-ins, reloads systemd, and activates the `current` link. The installers are the same Kubernetes, CRI, CNI, and CoreDNS installers the `nspawn` rootfs uses, pointed at the host root, plus the kernel module list and the LocalDNS supervisor script.
7. **Configuration and start.** The agent writes the containerd configuration, the kubelet configuration, and the LocalDNS inputs in parallel, sets up the LocalDNS network, and starts the stack in order. The allocation launch guard runs before any unit is configured or started when the host is an allocation host. The applied configuration is recorded only when this run built the node.

The start order is LocalDNS, containerd, then kubelet. Each unit is enabled and started with `systemctl enable --now`. After LocalDNS starts, the agent waits for both of its readiness listeners. After containerd starts, it imports the staged container image archives. A failure at any gate stops the start rather than leaving a half-configured stack running.

When the stack was already running and only its configuration changed, the agent restarts only what changed, containerd first. That keeps a configuration reapply from restarting kubelet and every pod for a change that affects only the runtime. The start path still runs its readiness gates for the units that were started.

Bootstrap starts kubelet and then waits for TLS bootstrap by polling for a non-empty `/var/lib/kubelet/kubeconfig` for up to two minutes. The kubeconfig is the artifact that says the node can authenticate to the API server. A unit that is active without a kubeconfig is not a successful start. The separate 60 second kubelet-active wait belongs to a soft reboot, which is described below.

### Stop and soft reboot

Stop is the reverse of start: kubelet, containerd, then LocalDNS. Every unit is attempted even if an earlier one fails, and the errors are joined so one broken unit does not hide the others. A unit that is not present is skipped by the teardown path, which keeps a half-installed node removable.

A plain stop does not disable the units, so a host reboot starts them again. That is deliberate: the units are enabled when the agent starts them, and a stop is a runtime action rather than an uninstall. If you want the node to stay down across a reboot, disable the units yourself, or use reset. If you stop only to restart the stack, use the soft reboot path instead.

A plain stop also does not kill pod processes. The containerd unit runs with `KillMode=process`, which is what lets pods survive a containerd restart, and the same setting lets them survive a stop. Pods are reaped during an upgrade, a destroy, or a reset, not by a stop. If you need the pods gone, use the teardown path rather than assuming a stop ended them.

A soft reboot restarts the stack without rebooting the host. The backend runs the pre-start hook, which prepares GPU support against the devices present now, restarts LocalDNS, containerd, and kubelet in start order, waits for LocalDNS after it restarts, and then waits for kubelet to be active again for up to 60 seconds. Pods keep running. This is the operation behind `kubectl unbounded machine node-reboot`; a host reboot is the separate `host-soft-reboot` operation and is not part of the isolation backend.

The soft reboot is useful when a runtime or kubelet configuration changed in a way that requires a restart but the host itself is healthy. It is not a substitute for an upgrade when binaries changed: a restart runs the same version directory the `current` link already names.

### Repave and upgrade

An upgrade is a repave-time operation. The caller has already cordoned the Node, drained it, and deleted the Node object. The deletion is the trigger. The mode does not have a second live slot, so the node is briefly down while the link flips and the new stack starts.

The sequence is built to be recoverable:

1. **Recover first.** An upgrade that was interrupted is recovered before anything else. If the recovery cannot finish it, the repave stops with that error rather than starting a second upgrade; the next daemon start retries the recovery.
2. **Resolve.** The agent resolves the new configuration's download overrides and offline archives, and stages the archives.
3. **Journal.** Before anything changes, it writes `/etc/unbounded/agent/none-upgrade-journal.json` with the previous version, the target version, and a digest of the configuration being applied.
4. **Snapshot.** It saves the host configuration files both stacks share, including the containerd and kubelet configuration, the kubelet identity and PKI, the CNI configuration, and the LocalDNS files. A rollback can then restore the previous node's configuration rather than starting it on files the new version already rewrote.
5. **Install beside.** It installs the new version directory and writes the new configuration without restarting the running units. The running version keeps executing from the old directory through the old link.
6. **Stop and end pods.** It stops the stack and reaps the containerd shims and pod cgroups. The pods have to be gone before the host networking they use is cleaned.
7. **Flip, clean, and start.** It points `current` at the new version, writes the units for the new configuration, cleans the host networking the old pods left and sets it up for the new configuration, resets the CNI state, removes the old kubelet identity, starts the stack in order, and waits for kubelet and TLS bootstrap. The identity reset is what makes the repaved node bootstrap fresh credentials, as a new `nspawn` machine would.
8. **Record and prune.** Only after the new version is healthy does the caller persist the applied configuration. The agent then commits the upgrade and removes the superseded version directories.

If anything fails after the flip, the backend stops the new stack, ends its pods, flips the link back, rewrites the previous configuration's units, restores the saved configuration, resets host networking to what the previous version ran with, and starts the previous version. The returned error says the rollback happened. If the failure happened before the flip, the node is restarted unchanged rather than left down, and the saved configuration is put back first.

A crash in the middle is handled by the same journal. At daemon startup, the recovery reads the journal and compares the current link and the configuration digest. If the link already points at the target and the applied configuration has the recorded digest, the upgrade had succeeded and is committed; if the stack is not running, it is started. Otherwise the node is rolled back to the previous version, including its units, saved configuration, and host networking. The journal is removed only after the outcome is durable, and a failed rollback leaves it in place so the next start tries again.

Two limits are worth knowing. A version name alone is not enough to decide whether an upgrade finished, because a repave that changes only configuration keeps the same version name; the digest is what distinguishes the cases. And the rollback starts the previous version with the units and gates of the configuration it ran, not with the new configuration's, so a failed upgrade does not leave the old version waiting on a new LocalDNS listener or a new GPU setup.

A declared change of `spec.isolation` is not handled by this path. The repave reconciler refuses a mode change before it reaches the node operator, reports it in `status.isolationChange`, and goes ahead with the repave in the mode the node already runs. A change to or from `none` is a reprovision, not an in-place upgrade, because `none` has no slot to build beside. It requires the `unbounded-cloud.io/allow-reprovision` annotation on the Machine and resets the host before bootstrapping the target mode.

### Reset and delete

`unbounded-agent reset` stops the daemon and removes the node, returning the host to a state where bootstrap can run again:

```bash
unbounded-agent reset
```

The `nspawn` teardown always runs, because every step is idempotent and reset must work even when the applied configuration is missing or damaged. The `none` teardown runs in addition, but only when the host has traces of a `none` node: the base directory or one of the agent-owned units, including the launch-check unit. A teardown never runs on a host that has nothing of its mode, so reset does not remove another installation's state.

The reset order is the same for every mode, and the `none` tasks slot into the positions of the `nspawn` tasks they replace:

1. Remove the daemon unit.
2. Stop the node stack. For `none`, each unit that exists is stopped, and a unit that is absent is skipped so a half-installed node still tears down.
3. Remove WireGuard keys.
4. Remove the configuration the agent wrote. This includes the agent's containerd configuration, the NVIDIA runtime drop-in, the CDI spec, the API server CA, `localdns.slice`, the modules-load list, and the Gantry hosts file if it carries the agent's marker. `/etc/unbounded/localdns` is removed recursively. Directories the agent created are removed only when empty, so anything an operator or a workload put there stays.
5. Destroy the node: stop and disable the units, remove the units and their drop-ins, stop and remove the launch check, reload systemd, remove the CNI plugin links the agent made, and remove the whole base directory.
6. Remove bpffs mounts, clean host network and routes, remove the nftables flush unit, remove the first-boot bootstrap unit, remove agent artifacts, and reload systemd.

Destroy also ends what stopping the units leaves running. It kills containerd shims that are in the agent's containerd cgroup and leaves shims of any other containerd alone. It then kills the pod cgroups `kubepods.slice` and `kubepods` through `cgroup.kill` when the kernel has it, or by signaling every process in the subtree when it does not. Killing a shim is not enough: the pod's processes are reparented and keep running with their mounts open.

A unit that did not stop keeps its file and keeps the installed versions, because removing them would leave a running service that systemd can no longer name and whose next reset could not find it to stop. Destroy reports that outcome instead of pretending the node is gone.

Data directories are removed last, and conditionally. `/var/lib/kubelet`, `/var/lib/containerd`, and `/etc/cni/net.d` have no meaning once the node is gone. Before removing the first two, the agent unmounts anything pods left mounted below them. If a mount cannot be unmounted, the directory is kept and the failure is reported. This is deliberate: removing a directory with a live mount below it would follow the mount and delete a CSI, NFS, or subPath volume's data. An unreadable mount table is treated the same way and keeps every directory that could hold mounts.

A second reset on an already-clean host is harmless. The `none` teardown is not selected when no traces remain, and the `nspawn` steps are idempotent.

### Health and diagnostics

The `none` backend reports running only when every unit in the stack is active. If one is not, the status detail names that unit, which is usually enough to know where to look. This is stricter than checking that the daemon is alive: the daemon can be healthy while kubelet is not. A LocalDNS unit counts as part of the stack when its file is installed, so a node with LocalDNS enabled is not running if CoreDNS is down.

The readiness waits are separate from status. A soft reboot waits for kubelet to report `active` for up to 60 seconds. Bootstrap completion means `/var/lib/kubelet/kubeconfig` exists and is non-empty within two minutes. LocalDNS is ready when both its node listener and cluster listener answer, with a one-minute deadline. These checks fail for different reasons, so read the error text rather than assuming the stack is down as a whole.

Two commands cover most inspections. `unbounded-agent exec` runs a command where the stack runs, which for `none` is the host:

```bash
unbounded-agent exec -- crictl ps
```

A bare command name is resolved against the active version's `bin` directory first, so `ctr`, `crictl`, and `kubectl` are the versions the node runs. Anything the version does not ship resolves on the host's `PATH` as usual. `unbounded-agent logs` streams the journal of the agent-owned units:

```bash
unbounded-agent logs --unit kubelet --lines 200
unbounded-agent logs --unit containerd --follow
```

Without `--unit`, all stack units are shown. The unit names users type are mapped to the agent-owned names, so `kubelet` means `unbounded-kubelet.service`, not the distribution unit. `--follow` keeps the stream open and `--lines` limits it to the last N entries. This is the supported replacement for `machinectl shell` on a `none` host.

GPU and device access need no special diagnostics command because there is no bind layer. The stack sees the host's device nodes and libraries directly, and the NVIDIA CDI spec at `/etc/cdi/nvidia.yaml` is regenerated at every start. If a GPU workload fails, check the CDI spec and the toolkit paths before you check the pod.

LocalDNS is the one unit with a service sandbox. It runs as a `DynamicUser` with only `CAP_NET_BIND_SERVICE`, a strict filesystem posture, and a private temporary directory. Its configuration directory and the version's `libexec` directory must be traversable by that dynamic user, which is why the agent forces mode `0755` on the directories it creates. A permission error in the LocalDNS journal usually means a parent directory was created by something else with a stricter mode.

## Configuration keys

The mode is selected with `Isolation`, and the rest of the agent configuration feeds the same goal state the other backends use. The keys below have a direct effect on a `none` node. The agent configuration reference documents the full schema, including the fields that are shared with other modes.

| Key | Effect in `none` |
|---|---|
| `Isolation` | Selects the mode. `"none"` is the mode; an empty value is `nspawn`. Only the modes this binary can operate are accepted. |
| `Cluster.Version` | Selects the Kubernetes binary set and the leading part of the version directory name. |
| `CRI.Containerd.Version`, `CRI.Runc.Version`, `CNI.PluginVersion` | Select the containerd, runc, and CNI plugin artifacts and contribute to the version digest. |
| `CRI.Containerd.SandboxImage` | Sets the CRI sandbox image in the generated containerd configuration. |
| `LocalDNS.Enabled` | Adds `unbounded-localdns.service`, the LocalDNS drop-ins, the CoreDNS artifact, and the kubelet's dedicated resolver file. |
| `LocalDNS.NodeListenerIP`, `LocalDNS.ClusterListenerIP` | Set the two listeners CoreDNS serves and that readiness checks query. They must be distinct and distinct from `Cluster.ClusterDNS`. |
| `LocalDNS.CPULimitInMilliCores`, `LocalDNS.MemoryLimitInMB` | Set the limits in `localdns.slice`. |
| `LocalDNS.MetricsAddress`, `LocalDNS.RequiredPlugins`, `LocalDNS.CorefileTemplate` | Shape the generated Corefile and the metrics endpoint. |
| `NvidiaToolkit.CTKPath`, `NvidiaToolkit.ContainerRuntimePath` | Point the host toolkit checks and CDI commands at a non-default installation. Valid only for `none`. |
| `AdditionalHostDevices` | Adds device nodes under `/dev` or systemd device group specifiers to the grants. Each node path must exist; a class is accepted without a device policy. |
| `AdditionalHostMounts` | Adds host mounts. In `none` the target must be empty or equal the source, and `ReadOnly` is accepted with a warning but not enforced. |
| `Kubelet.NodeIP`, `Kubelet.Labels`, `Kubelet.RegisterWithTaints`, `Kubelet.Configuration` | Shape the generated kubelet configuration and the node configuration drop-in. |
| `OfflineArtifacts.Source` | Supplies the node artifacts from an offline source instead of the network. |
| `Gantry.Disabled` | Skips the agent-written containerd hosts file that points pulls at Gantry. |

`Experimental.ContainerIsolation` is not needed for this mode. It gates `container` and has no effect on `none`. `ProvisioningFormat` describes the host's first-boot format and does not change how the node stack runs.

The two host-path settings that matter most are easy to overlook. `NvidiaToolkit` is rejected for any mode other than `none`, because the other modes run the toolkit from their rootfs image. `AdditionalHostMounts` with a different target is rejected by `none`, because the stack has no private root in which to place the second path. The first is a common validation error when one configuration is reused across modes; the second is a common preparation error when an `nspawn` mount list is moved to `none`.

## Security implications

`none` has no isolation boundary between the node stack and the host. There is no mount namespace, PID namespace, or user namespace between them. The node's processes are host processes, its mounts are host mounts, and its files are host files. That is the defining property of the mode, not a bug to work around.

The node stack runs as root on the host, except for LocalDNS, which uses a dynamic user. containerd and kubelet can mount host paths for pods, and a privileged pod is effectively host root. A compromised kubelet, a malicious DaemonSet with the right privileges, or a container escape has the host's own authority because there is no second root to escape into. If that is unacceptable for your threat model, the mode is the wrong choice.

Device grants are checks, not a device cgroup policy. The units run without a device policy, so every device class is reachable to the stack. The grant checks verify that a named device exists at its own path and fail when it does not, which turns a configuration mistake into a clear preparation error. They do not add a restriction on what a pod can open; any restriction comes from the pod's own runtime configuration.

A read-only mount grant is accepted with a warning and is not enforced. Do not rely on `ReadOnly` in a `none` configuration to protect a host path. Use host file permissions, or a mode with a private root instead.

The host's kernel, kernel modules, and userland libraries are shared. The node's binaries are versioned under `/opt/unbounded/node`, which protects you from a host package upgrade replacing the runtime or kubelet, but it does not protect you from a kernel change, a driver change, or a host library that the binaries load. The host package set is deliberately small and `/usr` is left alone, but `none` still assumes the host is a stable, dedicated platform.

Pods survive a plain stop because containerd runs with `KillMode=process`. This is intentional: a containerd restart should not kill every workload on the node. It also means that stopping the units is not the same as ending the node's workloads. Only an upgrade, a destroy, or a reset reaps the shims and the pod cgroups.

The upgrade path has a brief outage. `nspawn` keeps the old slot serving while the new one is prepared. `none` stops the stack, flips the link, and starts the new stack, so the node is unavailable for the length of that sequence. The automatic rollback limits how long a bad upgrade hurts, but it does not make the operation zero downtime.

Finally, the mode assumes exclusive ownership of the host's kubelet and containerd. You cannot co-locate it with another Kubernetes node, a host Docker that owns the runtime socket, or a distribution kubelet. Bootstrap refuses rather than sharing, because two node stacks on one host fight over the same sockets, directories, and network rules.

## When to choose none

Choose `nspawn` by default. It gives the node a private userland and service manager, a blue/green upgrade path with no planned node outage, a private bpffs per slot, and the longest history in this project. A general-purpose distribution host that can install or already ships `systemd-container` is an `nspawn` host.

Choose `none` when one of these constraints is real:

- The host cannot run a machine: no usable `systemd-nspawn`, no `machinectl`, or a package model that will not allow `systemd-container`.
- The host's `/usr` is read-only and image-managed, and you cannot add a system extension for the machine runtime.
- The host owner refuses a machine layer for operational or compliance reasons, and accepts that the node runs directly on the host.
- You need the node to use host-provided tools, devices, or kernel facilities without a bind layer, and you accept the lack of isolation.
- You are debugging whether a problem belongs to the node stack or to the machine layer, and you want the machine layer removed from the experiment.

Do not choose `none` when any of these is true:

- You need a security boundary between the node and the host, or between pods and the host.
- Another kubelet, containerd, or Docker must keep running on the host.
- You need a per-slot bpffs namespace for an eBPF-based CNI.
- You cannot accept a brief node outage during an upgrade.
- The host's kernel, driver, or userland changes without coordination, and you need the node's runtime to be insulated from those changes.
- You need the blue/green rollback story that only a second slot provides.

| Question | Choose `nspawn` | Choose `none` |
|---|---|---|
| Is a machine runtime available and acceptable? | Yes | No, or explicitly refused |
| Must `/usr` stay read-only? | Not required, though the mode can work with it | Yes, and no `systemd-container` extension |
| Is a private bpffs per node required? | Yes | No |
| Is a brief upgrade outage acceptable? | An outage is not needed | Yes, a brief outage is expected |
| Is the host dedicated to this node? | Recommended | Required |
| Is host-level isolation required? | Yes | No |

If you are unsure, start with `nspawn`. Moving a node to `none` later is a reprovision, which is a larger operation than a normal repave, and it needs the Machine to allow reprovisioning. Starting in the simpler mode and moving deliberately is cheaper than discovering the isolation tradeoff after workloads are running.

## Troubleshooting

### Bootstrap refuses an unmanaged kubelet or containerd

The error names the finding, for example a unit at `/usr/lib/systemd/system/kubelet.service`, a binary at `/usr/bin/containerd`, or a running process with a PID. The agent is telling you that the host already has a node component it did not create. A unit masked to `/dev/null` is not a finding.

Remove the competing unit and binary, or choose `nspawn`. Do not mask a live unit and expect bootstrap to proceed: a mask is exempt from the check only because a masked unit cannot start, and the rest of the host may still be configured to run a node. If the host previously ran `nspawn`, a leftover mask is expected and does not need to be removed.

### A required path is not writable

The error names the path you asked about and the nearest existing parent that was probed. Check whether the failure is on `/etc`, `/opt/unbounded/node`, or `/var`. A read-only `/usr` is not a failure in this mode, so do not try to remount `/usr` read-write unless something else requires it.

The probe creates and removes a temporary file. If the removal fails, the error is reported and a probe file may be left in the directory. Remove it before you retry so the next run does not accumulate files.

### Package installation fails

On a normal host, check that `apt-get`, `tdnf`, or `dnf` can reach its repositories and that the package names resolve. The required capabilities are `curl`, `nft`, and `mountpoint`; the packages that provide them are `curl`, `nftables`, and `util-linux`.

On an image-managed host, the error says the tools must come from the image or a system extension. That is not a transient failure and retrying will not fix it. Build the tools into the image or add an extension, then run preflight again.

On a host with no package manager, the error lists the missing capability and the package that normally provides it. Add the executable to the image or `PATH`, or install a supported package manager.

### Units do not start after a reboot

Start with the unit that failed, not the whole stack:

```bash
systemctl status unbounded-containerd.service
systemctl status unbounded-kubelet.service
journalctl -u unbounded-containerd.service -n 200
journalctl -u unbounded-kubelet.service -n 200
```

A kubelet that does not start with a condition failure usually means the `current` link is broken or the version's `kubelet` binary is missing. Check `readlink /opt/unbounded/node/current` and the file it names. A containerd that does not start usually means the configuration file is missing or invalid, or the `overlay` module did not load. Check `modprobe overlay` and the containerd journal.

The node units are enabled by the agent when it starts them, so they should come back after a reboot. They are not enabled by the distribution and they are not aliases of the distribution units. If `systemctl status kubelet` shows the distribution unit, you are looking at the wrong service. The same applies to `journalctl -u kubelet`: use the `unbounded-` name or `unbounded-agent logs --unit kubelet`.

### Kubelet is active but the Node never appears

An active kubelet is not a bootstrapped kubelet. Check whether `/var/lib/kubelet/kubeconfig` exists and is non-empty. If it does not, the kubelet cannot authenticate to the API server. Look at its journal for TLS bootstrap errors, and check that the API server CA at `/etc/kubernetes/pki/apiserver-client-ca.crt` matches the cluster.

You can inspect the node's own view with:

```bash
unbounded-agent exec -- crictl ps
unbounded-agent logs --unit kubelet --lines 200
```

If the kubeconfig exists but the Node object is missing, check whether the kubelet is pointed at the right API server and whether the bootstrap token or credentials are still valid. A repave removes the old kubelet identity on purpose, so a repaved node expects to bootstrap fresh credentials.

### Kubelet starts with different flags than the configuration shows

Check for `/etc/default/kubelet`. The kubelet unit reads that file if it exists, and its `KUBELET_EXTRA_ARGS` and other `KUBELET_*` variables become part of the kubelet's command line. The agent does not write the file, so a value there can pre-date or override the node's own drop-ins. Remove or correct it if it is not intentional.

### LocalDNS never becomes ready

LocalDNS readiness means both the node listener and the cluster listener answer. If the unit is active but the wait fails, check the unit journal and the shared network unit:

```bash
systemctl status unbounded-localdns.service
systemctl status unbounded-localdns-network.service
journalctl -u unbounded-localdns.service -n 200
```

A permission error usually means a parent directory is not traversable by the dynamic user. The agent forces its own directories to `0755`, but a directory created by another component may be stricter. Check the path from `/etc/unbounded/localdns` up to the version's `libexec` directory. A missing dummy interface or NOTRACK rule points at the network unit, which is shared with every mode and is not specific to `none`.

### Pods survive a stop

This is expected. The containerd unit runs with `KillMode=process`, so stopping it does not kill the pods it started, and a plain stop does not reap them. Use an upgrade, a reset, or a destroy when you need the pods gone. The teardown kills the agent containerd's shims and the `kubepods.slice` and `kubepods` cgroups, which is what actually ends the pod processes.

### An upgrade refuses because a journal exists

The error says an earlier upgrade was interrupted and has not been recovered. Do not delete the journal by hand. Restart the agent, which runs the recovery at startup, or repave again. The recovery either finishes the upgrade if the link and configuration digest show it succeeded, or rolls back to the version the journal names. Both outcomes are safer than removing the only record of what the host was doing.

### Reset keeps a data directory

A reset keeps `/var/lib/kubelet` or `/var/lib/containerd` when a mount below it could not be unmounted, or when the mount table could not be read. The directory is kept on purpose, because removing it would follow the mount and could delete a volume's data. Find the mount with `findmnt` or `mount`, unmount it, and run reset again. A second reset is safe.

### GPU workloads fail

Check that the toolkit paths named in the error exist, that `/etc/cdi/nvidia.yaml` exists and names the devices present now, and that the containerd unit's `10-gpu-setup.conf` drop-in and the `/etc/containerd/conf.d/99-nvidia-runtime.toml` drop-in are present. A driver update or a host reboot changes what the CDI spec should contain, and the unit regenerates the spec at every start. If you changed the toolkit after the node started, restart the stack or repave so the spec matches the driver.

### The node cannot be found

`exec` and `logs` read the applied configuration to find the node. If no node has been bootstrapped, or the applied configuration is missing or unreadable, they fail with that explanation instead of guessing. Bootstrap the host, or restore the applied configuration from your automation if the host is known to be installed. A reset removes the applied configuration as part of removing the node.

## Related

- [Isolation modes]({{< relref "reference/agent/isolation" >}}) for the comparison, selection, and mode-change rules that apply to all backends.
- [systemd-nspawn isolation]({{< relref "reference/agent/nspawn" >}}) for the default mode and its blue/green slots.
- [container isolation]({{< relref "reference/agent/isolation-container" >}}) for the other host-native, experimental backend.
- [Agent configuration]({{< relref "reference/agent/configuration" >}}) for the `Isolation` field and the rest of the agent configuration.
- [Agent preflight]({{< relref "reference/agent/preflight" >}}) for the checks that run before a host is changed.
- [Node operations]({{< relref "guides/operations/node-operations" >}}) for cordon, drain, repave, and Node deletion.
- [Agent operations]({{< relref "guides/operations/agent-operations" >}}) for starting, upgrading, and resetting the agent.
- [NVIDIA GPUs]({{< relref "reference/gpu/nvidia" >}}) for the toolkit and CDI model that `none` uses directly on the host.
