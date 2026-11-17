---
title: "Isolation Modes"
weight: 3
description: "Compare where the Unbounded node stack runs in the nspawn, none, and container isolation modes, and learn how to select and change a mode safely."
---

An isolation mode answers one question: where does the Kubernetes node stack run on this host? The node stack is containerd, kubelet, the CNI plugins, and CoreDNS. The agent and its daemon always run on the host, outside that boundary. They watch the Machine object, prepare artifacts, start and stop the stack, and report what they find. The mode decides only where the stack lives and how the agent operates it.

Unbounded supports three modes. `nspawn` runs the stack inside a systemd-nspawn machine and is the default. `none` runs the stack directly on the host as agent-owned systemd units. `container` runs the stack inside a privileged OCI container supervised by an agent-owned systemd unit, and it is experimental.

You choose a mode explicitly. Nothing is auto-detected from the host. A host with no recorded mode is treated as `nspawn`, which is also what an empty `spec.isolation` means and what a configuration written before the field existed means. This page explains the tradeoffs, how each mode is selected and enabled, what operations look like in each one, and how to move a running node from one mode to another without losing the host.

## The three modes

Every mode shares the same outer architecture. The `unbounded-agent` daemon runs as a host systemd service and holds the installation lock. It reads the applied agent configuration, finds the active node, and builds the backend for the recorded mode. Bootstrap, repave, start, and stop dispatch through the mode's backend, operator, and bootstrap plan, and `exec` and `logs` build the same backend from the applied configuration. Those three components are wired together in one composition table in the agent, so a mode is either fully wired or the agent refuses to use it.

The modes differ in the layer that holds the node stack.

### nspawn

In `nspawn` mode the node stack runs in a systemd-nspawn machine named `kube1` or `kube2`. The machine has its own filesystem rooted under `/var/lib/machines/<slot>`, its own systemd as PID 1, its own PID namespace, and its own cgroup subtree. It uses the host kernel and the host network namespace, so pods reach the host network directly and CNI plugins configure interfaces the host can see.

The agent prepares the machine rootfs from an OCI image, renders the machine configuration and service overrides, and uses `machinectl` to start and stop the machine. Commands inside the machine run through `systemd-run --machine=<slot> --pipe --wait`, and logs come from the machine journal. Host-side state, including the two machine names, the per-slot bpffs mounts under `/run/bpffs`, and the machine directories, is removed by the reset path even when the applied configuration is missing or damaged.

This is the mode with the longest history in the project, and the right default for a normal distribution host that can install or already ships `systemd-container`.

### none

In `none` mode the node stack runs directly on the host. The agent installs containerd, kubelet, CNI plugins, and CoreDNS into agent-owned versioned directories under `/opt/unbounded/node/<version>`, with a `current` symlink selecting the active version. It starts them as agent-owned units named `unbounded-containerd.service`, `unbounded-kubelet.service`, and, when LocalDNS is enabled, `unbounded-localdns.service`. These names are deliberately different from the distribution's units, so the agent never edits or removes a unit it did not create.

There is no filesystem or PID boundary between the node stack and the host. A process in a pod is a host process in the host PID namespace, and a containerd shim started by the node's containerd is visible in the host process table. The one concession to host safety is ownership: bootstrap refuses a host that already has a kubelet or containerd the agent does not own, whether that is a unit file, a binary on the standard paths, or a running process. A unit masked to `/dev/null`, which an nspawn host leaves behind, is not treated as foreign.

The mode is useful on hosts where a machine runtime is unavailable, unwanted, or impossible, and it is the only mode that runs the node stack from the host userland. It writes its own configuration under `/etc/unbounded/containerd/config.toml` rather than the distribution path, and it does not require a writable `/usr`.

### container

In `container` mode the node stack runs inside a privileged OCI container with systemd as PID 1. The agent unpacks the rootfs image into `/var/lib/unbounded/containers/<slot>`, generates an OCI bundle at `/var/lib/unbounded/containers/bundles/<slot>/config.json`, and writes a supervising unit named `unbounded-node-<slot>.service`. The container is started with `runc` from an agent-owned path, `/opt/unbounded/runtime/runc`, and the host's docker and containerd are not used.

The process is privileged in the capability sense: it receives the full capability set, and it runs in the host network and UTS namespaces while keeping its own PID, mount, IPC, and cgroup namespaces. Device access is not unrestricted in intent, but the generated bundle starts from an allow-all device rule and then adds explicit rules for the devices and device directories the node needs. The container shares host paths for `/run/containerd`, `/etc/containerd/certs.d`, `/var/lib/kubelet`, and `/opt/cni`, and it gets its own bpffs at `/sys/fs/bpf` from the slot's private mount at `/run/bpffs/<slot>`.

The mode is experimental. It is exercised against fake runtimes and stubbed hosts, and parts of its host integration are marked provisional in the code. A host whose applied agent configuration does not opt in cannot bootstrap in it, and a running node cannot be switched to it unless the configuration already enables it.

### Hosts with no recorded mode

The agent records the mode it used in an applied configuration file next to the node's other agent state. The search order prefers the nspawn slots first, then the `none` slot, then the container slots, so an older host is found the way older agents find it. If no record is present at all, the mode is the default, `nspawn`. That is also why a Machine with no `spec.isolation` and an agent configuration with no `Isolation` field are safe to interpret: they mean nspawn, not "unknown".

A host that has no node bootstrapped reports that no active node exists, and `exec` and `logs` fail with that explanation instead of guessing.

## Comparison

The table below summarizes the differences that matter when you choose a mode or debug one. The prose after it explains each row, including the failure modes that the table cannot show.

| Property | `nspawn` | `none` | `container` |
|---|---|---|---|
| Where the node stack runs | systemd-nspawn machine `kube1` or `kube2` | Directly on the host, agent-owned units | Privileged OCI container with systemd as PID 1 |
| Filesystem isolation | Machine rootfs under `/var/lib/machines/<slot>` | None; host filesystem | Container rootfs under `/var/lib/unbounded/containers/<slot>`, own mount namespace |
| Node stack artifacts | OCI image unpacked into the machine | Release artifacts in `/opt/unbounded/node/<version>`, selected by `current` | OCI image unpacked into the container rootfs, OCI bundle generated beside it |
| Host `/usr` and package install | Installs `systemd-container` and host tools through the package manager | Installs `curl`, `nftables`, `util-linux`; never modifies `/usr` | Installs `curl`, `nftables`, `util-linux`; runc installed under `/opt/unbounded` |
| Supervising units | machined machine unit, started with `machinectl` | `unbounded-containerd.service`, `unbounded-kubelet.service`, optional `unbounded-localdns.service` | `unbounded-node-kube1.service` or `unbounded-node-kube2.service` |
| Slots and upgrade | Blue/green `kube1`/`kube2` | Versioned directory plus `current` symlink, brief downtime with rollback | Blue/green `kube1`/`kube2`, target built beside the source and switched |
| bpffs | Private mount per machine at `/run/bpffs/<slot>` | Host `/sys/fs/bpf` directly, no private mount | Private mount per slot at `/run/bpffs/<slot>`, bound at `/sys/fs/bpf` |
| Network namespace | Host network namespace | Host network namespace | Host network namespace, with `/proc/sys/net` writable |
| Best fit | General-purpose distribution hosts, the default | Hosts that cannot or should not run a machine | Evaluation on newer image-based hosts, experimental |
| Same-path contract | Bound into the machine | Trivial, paths already are host paths | Bound into the container, with `/var/lib/kubelet` shared |

Filesystem isolation is strongest in `nspawn` and `container`, but neither is a security boundary in the sense a hardened sandbox would be. Both share the host kernel and both need access to host devices and to parts of the host filesystem. The practical benefit is that the node's userland and package set are separate from the host's, so replacing or upgrading the node does not rewrite the host operating system. In `none` there is no such separation: the node's binaries live in an agent-owned directory, but its processes, mounts, and network configuration are the host's.

The node stack source also differs. The nspawn and container modes both start from an OCI rootfs image, which is unpacked into their respective directories. That image supplies the distribution userland the node expects, including the runtime binaries the node runs inside its own root. The `none` mode instead installs the release artifacts the agent downloads, versioned by a directory name derived from the Kubernetes version and the artifact versions. It links CNI plugins one by one through `current` into `/opt/cni/bin`, so plugins that cluster add-ons install there keep working across an upgrade.

Package installation is where the modes diverge most visibly. The nspawn package set includes `systemd-container` because the mode needs `systemd-nspawn` and `machinectl`. On a host with `apt-get`, `tdnf`, or `dnf`, bootstrap installs the missing packages in a single transaction. On an image-managed host whose `/usr` is read-only, the agent refuses to install and instead reports which capabilities the image or a system extension must provide. The `none` and `container` package sets drop `systemd-container`; they still need `curl`, `nftables`, and `util-linux`, or the executables those packages provide. The container mode additionally downloads `runc` into `/opt/unbounded/runtime/runc`; a preflight run that happens before that install tolerates a missing runtime because bootstrap installs it later, but a present runtime must be executable.

That is also the answer to the host `/usr` question. nspawn may modify the host package database and `/usr` when it installs `systemd-container`, and the node's own runc comes from the machine image, not from the host. The `none` mode never modifies `/usr`, installs its node binaries and runc under `/opt/unbounded/node`, and therefore works on hosts with a read-only `/usr`. The container mode installs no host package that provides a runtime, because the runtime is an agent-downloaded artifact under `/opt/unbounded`; it does still require writable `/opt`, `/etc`, and `/var`.

Supervision differs in shape. In nspawn, systemd's machine manager owns the machine unit and the agent drives it with `machinectl`; the node's services are supervised by the systemd running as PID 1 inside the machine. In `none`, the host's systemd supervises each agent-owned unit directly, and the agent starts them in order, waiting at readiness gates such as LocalDNS before starting containerd. In container mode, one host unit supervises the container through `runc`, and the node's services are supervised by the systemd inside the container. Stopping the host unit stops the container, and the backend waits until the runtime reports the container gone, killing it if it outlives the stop.

The slot and upgrade story follows from supervision. nspawn and container both use blue/green slots: a repave builds the new node in the alternate slot, starts it, waits for it to become ready, and then removes the old slot. The container backend also disables the other slot's unit when it starts one, so only one slot comes back after a boot. The `none` mode has no second slot; its unit of upgrade is the version directory. It stops the units, ends the pods that survive a containerd stop, flips the `current` symlink, writes the new units, and starts them again, with automatic rollback to the previous version and its configuration if any step fails. The node is briefly down during that flip. A soft reboot in `none` restarts the units and leaves the pods running; in the slot modes it restarts the machine or container unit and re-runs its lifecycle hooks.

bpffs placement is a compatibility detail that matters to eBPF-based CNIs. Each nspawn machine gets a private bpffs mount under `/run/bpffs/<slot>`, and the container mode uses the same host path for the same slot, bound at `/sys/fs/bpf` inside the container. The `none` mode has no private bpffs and uses the host's `/sys/fs/bpf` directly. If you run Cilium or another CNI that expects a per-node pinned-object namespace, that difference is worth knowing before you choose `none`.

The network namespace is shared with the host in all three modes, and that is deliberate. Pods need host-reachable addresses, CNI plugins need to configure host interfaces, and kube-proxy needs to program host rules. The difference is what the node can write. The nspawn machine and the container both see the host's `/sys` and `/proc/sys` read-only except `/proc/sys/net`, because CNI plugins and kube-proxy set network sysctls. Everything else in `/proc/sys` is the host's, and the agent writes the Kubernetes sysctls on the host before the node starts. In `none`, there is no boundary at all, so the node writes host sysctls as itself.

The same-path contract is the set of host paths that must resolve identically inside the node and on the host: `/run/containerd`, `/etc/containerd/certs.d`, `/var/lib/kubelet`, `/opt/cni`, and the slot's bpffs. The nspawn machine binds them in, and the container binds them in with `/var/lib/kubelet` shared so pod volume mounts made by the kubelet and CSI drivers propagate back to the host. The `none` mode satisfies the contract trivially because there is no second mount namespace. The contract is why a host containerd must be disabled even in container mode: the container shares `/run/containerd`, and a host containerd would own the socket the node's containerd needs.

Finally, `unbounded-net-node` and gantry are not part of the node stack and are unaffected by this choice. They run as host services with their own configuration and deployment manifests. Selecting an isolation mode does not change their units, their network setup, or the paths they use, and none of the mode backends operates them. You can change the node's isolation mode without redeploying either one.

## Choosing a mode

Choose `nspawn` unless a specific constraint rules it out. It is the default for a reason: it gives the node a private userland and service manager, it has a blue/green upgrade path, and it has the longest history in this project. A normal distribution host that can install `systemd-container` or already ships it is an nspawn host.

Choose `none` when the host cannot or should not run a machine. That includes a host with no usable `systemd-nspawn`, an image whose package model makes installing `systemd-container` impossible, and a host whose owner wants no machine layer at all. The mode also fits when you need host-native execution and you accept that the node has no filesystem or PID isolation from the host. Remember two consequences before you commit: the mode uses the host's `/sys/fs/bpf` directly rather than a private per-slot mount, and it requires a host with no unmanaged kubelet or containerd.

Choose `container` only for evaluation. It exists to explore running the node on newer image-based hosts where a machine runtime is a poor fit, and it is gated as experimental in every component that can provision it. It requires a host with systemd 254 or newer, a cgroup v2 unified hierarchy, an SELinux label the kernel accepts when SELinux is enforcing, and writable `/etc`, `/opt`, and `/var`. Its design assumptions have not been verified on a real Azure Container Linux node, so do not treat a successful evaluation as a production recommendation.

The mode is always explicit. The agent does not inspect the host and pick a mode, and it does not fall back from a missing mode to a different one. An unrecognized value is rejected rather than coerced to `nspawn`, so an older agent never silently runs a newer mode as if it were the default. If a host cannot support the mode you selected, bootstrap or preflight refuses it and reports why, and the node does not start in a different mode.

## Selecting a mode

You can select a mode at the Machine, at the provisioning command, or at the agent. All three paths feed the same validated mode, and the last one that applies wins at bootstrap time.

### The Machine spec

The desired mode belongs in `spec.isolation` on the Machine:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: my-node
spec:
  isolation: none
```

The field is an enum of `""`, `nspawn`, `none`, and `container`. An empty value is not a fourth mode; it means you have declared nothing. A new host is then provisioned in `nspawn`, and a repave leaves a running node in the mode it already uses. Setting `spec.isolation` does not move a running node. The node changes mode at its next repave, which the agent performs when the Node object is deleted.

### The provisioning command

`kubectl unbounded machine manual-bootstrap` accepts `--isolation` and writes the value into the generated agent configuration and the Machine it creates:

```bash
kubectl unbounded machine manual-bootstrap my-node --site dc1 --isolation none
```

For the experimental mode, the command also requires its own opt-in flag:

```bash
kubectl unbounded machine manual-bootstrap my-node --site dc1 \
  --isolation container --experimental-container-isolation
```

Without `--experimental-container-isolation`, the command rejects `--isolation container` during validation, before it generates any payload.

### The agent configuration

The agent configuration has an `Isolation` field. An empty or absent field means `nspawn`, including in configurations written before the field existed.

```json
{
  "MachineName": "my-node",
  "Isolation": "none"
}
```

This excerpt shows only the keys that matter for the mode; a real configuration carries the cluster, kubelet, CRI, and CNI settings as well.

`unbounded-agent start` and `unbounded-agent preflight` accept `--isolation`, which overrides the loaded configuration for that run:

```bash
unbounded-agent start --isolation none
unbounded-agent preflight --isolation none
```

The flag is validated the same way as the configuration value, so an invalid value is reported as `invalid --isolation`. For `container`, the opt-in must be present in the configuration file. Passing `--isolation container` to a configuration that does not set `Experimental.ContainerIsolation` fails, because the flag overrides only the mode, not the experimental gate.

### Validation and recording

Validation happens at three layers. The CRD enum rejects an unknown mode at the API server. The `kubectl` command parses the value and applies the experimental gate before generating a payload. The agent parses the value again, checks that the mode is one this binary can operate, and checks the experimental opt-in before it touches the host.

The agent records the mode it applied in an applied configuration file for the mode and slot. The locations are `kube1` and `kube2` for nspawn, `none-kube1` for the host-native mode, and `container-kube1` and `container-kube2` for the container mode. This record is how a later agent, `exec`, `logs`, and the reset path find the running node. The agent also reports the applied mode in `status.isolation` on the Machine, which is what the drift condition compares against.

## Enabling the container mode

The container mode has two gates that must agree: a provisioner must be allowed to provision the host in that mode, and the agent on the host must be configured to operate it. The agent setting is the deciding gate, because the agent refuses to bootstrap in the mode, or to change a running node to it, unless the configuration enables it. Provisioners write that setting when their own flag is on.

The agent gate is `Experimental.ContainerIsolation` in the agent configuration:

```json
{
  "Experimental": {
    "ContainerIsolation": true
  }
}
```

The Machina gate is the `experimentalContainerIsolation` key in the Machina configuration. When it is false, Machina refuses to provision a Machine that declares `spec.isolation: container`. When it is true, every agent configuration Machina provisions for such a Machine opts in to the mode.

```yaml
experimentalContainerIsolation: true
```

Metalman takes `--experimental-container-isolation` on its `serve-pxe` command, and the machine-ops-controller takes the same flag. The deployment template for the machine-ops-controller adds the flag only when its `ExperimentalContainerIsolation` value is `true`. Both provisioners also write the opt-in into the agent configuration they generate, which is what lets the agent accept the mode.

The `kubectl unbounded machine manual-bootstrap` command takes `--experimental-container-isolation` for the same reason, and the generated agent configuration carries `Experimental.ContainerIsolation`.

If Machina does not enable the mode, the drift condition reports the declared mode as refused with the reason `ExperimentalModeDisabled` and explains that the node keeps running its applied mode. Machina does not treat a pending change to a mode it will not provision as a change that is on its way.

## Operations

The mode changes where a task runs, not whether the agent has a task for it. Every mode implements the same backend surface: prepare, start, stop, restart, destroy, status, exec, and logs. The daemon and bootstrap call that surface, so the operational model stays the same even though the commands underneath differ.

| Operation | `nspawn` | `none` | `container` |
|---|---|---|---|
| Bootstrap | Host prep, unpack OCI rootfs into the slot, render machine configuration, start the machine, start the stack inside | Host prep, install versioned artifacts, write agent units, start the units in order | Host prep, install runc, unpack the rootfs, generate the OCI bundle and unit, start the container, start the stack inside |
| Start | Start the machine, then the in-machine units | Enable and start each agent unit in order | Enable and start the supervising unit, then the in-container units |
| Stop | Stop kubelet and containerd inside the machine, then stop the machine | Stop the agent units in reverse order | Stop the in-container units, then stop the container and wait until it is gone |
| Soft reboot | Restart the machine unit, which re-runs its hooks | Restart containerd, then kubelet, and wait for ready | Restart the container unit, which re-runs its lifecycle hooks |
| Repave or upgrade | Build the alternate slot, switch, remove the old slot | Stop units, end pods, flip `current`, start again, roll back on failure | Build the alternate slot, switch, remove the old slot |
| Health | The machine answers a command | Every agent unit is active | The supervising unit is active and the runtime reports the container running |
| Exec and logs | `systemd-run --machine`, machine journal | Host command, unit journal | `runc exec`, container journal |
| GPU and devices | Devices and libraries derived from the host when the machine is built | Native host devices, host toolkit paths | Devices, binds, and cgroup rules rendered into the OCI bundle |
| LocalDNS | CoreDNS inside the machine | CoreDNS as a host unit | CoreDNS inside the container |

Bootstrap and start are two different moments. Bootstrap prepares the host and materializes the node root, then starts the node and records the applied configuration. A later start only starts what was prepared. In nspawn, the machine is prepared by unpacking the rootfs and rendering its configuration before it is started; in container mode, the rootfs, the bundle, the recorded node spec, and the supervising unit are all prepared before the unit starts. In `none`, the version directory and the units are prepared before the units are enabled.

Stop is the reverse of start in every mode, and the modes are deliberately thorough about what a teardown leaves behind. The `none` backend's teardown ends the pods as well as the units, because containerd runs with `KillMode=process` so pods survive a containerd restart and would otherwise survive a stop. It kills the shims of the agent's containerd and the pods' cgroups. The container backend stops and disables the unit, then waits up to 30 seconds for the runtime to report the container gone, kills and deletes it if it is still alive, and fails if it survives that too. A container that survives holds the host paths both slots share, so leaving it running is not an option.

A soft reboot is not a host reboot. It restarts the node stack only: the machine in nspawn, the agent units in `none`, the container in container mode. Pods are expected to survive where the mode allows it, and the agent waits for the stack to become ready again. A host reboot is a separate operation and is not part of the isolation backend.

Repave and upgrade differ because the modes have different units of replacement. The slot modes build a complete second node beside the first, so the running node keeps serving until the new one is ready. The `none` mode has no second slot, so it accepts a brief outage while it flips the symlink and restarts the units, and it compensates with an automatic rollback to the previous version and its own configuration if anything after the flip fails.

Health checks are backend-specific. nspawn reports running when a command can be executed inside the machine. `none` reports running when every unit in the stack is active. Container mode reports running when the supervising unit is active and the runtime's state for the container is `running`. The post-start hook uses the runtime-level check instead of the unit-level one, because it runs while the unit is still activating.

### Host-global state

Some state belongs to the host rather than to a mode, and every mode has to leave it consistent.

The nftables flush unit is installed in every mode. It runs a oneshot at boot that flushes all nftables rules to a clean state before the node starts, so stale rules left by Docker or an earlier configuration cannot interfere with Kubernetes networking. In nspawn the machines order themselves after it. In `none` and container mode the unit is ordered before the node's own units, because there is no machine to order against. Reset removes the unit for every mode, since it is host-global rather than part of a mode's teardown.

LocalDNS shares a host network unit across all modes. That unit creates the dummy interface and the NOTRACK rules the local DNS path needs, and the node's units are ordered after it and depend on it. In nspawn, CoreDNS runs inside the machine; in `none` it is an agent-owned host unit; in container mode it runs inside the container. The network unit is the same in all three, so the host-side plumbing does not change when you change modes.

WireGuard keys and Kubernetes sysctls are also host-global. Reset removes the WireGuard keys it installed, and the agent writes the Kubernetes sysctl file at `/etc/sysctl.d/99-kubernetes.conf` on the host before the node starts, because the node sees the host kernel's `/proc/sys` and kubelet verifies the values at startup. These are not per-mode decisions, but they are part of what a reset has to undo.

bpffs is the one host-global path with a per-mode shape. nspawn mounts a private bpffs per machine under `/run/bpffs/<slot>` and binds it into the machine. Container mode uses the same host path for the same slot and binds it at `/sys/fs/bpf`; the supervising unit mounts it before the container starts. `none` does not create a private mount at all. Reset removes both nspawn slots and the container slots, so a host that has run either mode is cleaned the same way.

## Removing a node

`unbounded-agent reset` is the removal command in every mode. It stops the daemon, removes the installation, and returns the host to a state where bootstrap can run again.

The nspawn teardown always runs, even on a host whose applied configuration is missing or damaged. Every step of it is idempotent, so it is harmless on a host with no machine, and the agent never skips it on the strength of a configuration that might be stale. That is what makes reset the reliable escape hatch after a failed bootstrap.

Each non-nspawn mode adds its own teardown only when the host actually has that mode's traces, meaning its units or its directory. A teardown never runs on a host that has nothing of its mode, so reset never removes another installation's state. A `none` node's units and version directory are removed by the `none` teardown; a container slot's unit, bundle, rootfs, and bpffs mount are removed by the container teardown. A container whose runtime still reports it alive keeps its unit, bundle, and rootfs, because removing the files while the container still owns them could delete host data through its binds.

The order is the same for every mode: remove the daemon unit, stop the node, remove WireGuard keys, remove the node configuration, remove the node itself, remove bpffs, clean network and routes, remove the nftables flush unit, remove the first-boot bootstrap unit, remove agent artifacts, and reload systemd. Mode-specific tasks sit in the position of the nspawn task they correspond to. Reset also removes a reprovision handoff left behind by an interrupted mode change, so a host that failed halfway through a switch can be cleaned and bootstrapped again.

## Running commands and reading logs

`unbounded-agent exec` runs a command where the node stack runs. The mode is read from the applied configuration, so you do not tell the command which mode the host uses:

```bash
unbounded-agent exec -- crictl ps
```

Everything after `--` is the command and its arguments. In nspawn it runs inside the machine through `systemd-run`. In `none` it runs on the host, resolving a bare command name against the active version's own `bin` directory first, so you get the same `ctr`, `crictl`, or `kubectl` the node runs. In container mode it runs through `runc exec` in the container.

`unbounded-agent logs` streams the journal of the node stack:

```bash
unbounded-agent logs --unit kubelet --lines 200
unbounded-agent logs --unit containerd --follow
```

Without `--unit`, all node stack units are shown. `--follow` keeps the stream open, and `--lines` limits the output to the last N entries. In nspawn the command reads the machine journal, in `none` it reads the host journal for the agent-owned units, and in container mode it runs `journalctl` inside the container. This command is the supported replacement for `machinectl shell` when you need to inspect a node, and it works the same way in all three modes.

## Changing the mode

Changing the mode is a repave-time operation, not a live switch. You declare the target in `spec.isolation`, and the agent makes the change at the node's next repave, the one it performs when the Node object is deleted. Declaring a mode by itself never moves a running node and never wipes a host.

The safe sequence is the same one you use for any planned repave. Declare the mode, then cordon the Node so no new pods land on it, drain it so existing pods move elsewhere, and delete the Node object. The deletion is the trigger. If a change needs a reprovision and the Machine does not allow it, the agent refuses the change, reports why, and goes ahead with the repave in the mode the node already runs.

The strategy depends on the pair of modes. A move between `nspawn` and `container` is in place, because both modes run in a slot and the target can be built beside the source. A move to or from `none` is a reprovision, because `none` has no slot to build beside. A move to the mode the node already runs is an error, not a no-op.

The agent reports every outcome in `status.isolationChange` and explains refusals with a machine-readable reason. `ReprovisionNotAllowed` means the change needs the `unbounded-cloud.io/allow-reprovision` annotation and the Machine does not have it set to `"true"`. `UnsupportedMode` means this agent cannot run the target mode or cannot change a running node to it. `ExperimentalNotEnabled` means the target is the experimental mode and the agent's applied configuration does not opt in.

A failed change is recorded and not retried in the same repave, so a node is not put through repeated outages. The repave goes ahead in the current mode, and the agent restores the Node registration if the kubelet did not come back by itself. The next repave tries the change again.

### In-place change

An in-place change builds the target in the alternate slot while the source keeps running. The migration first runs the target backend's preflight and downloads the target's artifacts. Nothing is mutated if either fails. It then snapshots the host configuration files that both stacks write, such as the kubelet identity, because a rollback has to restart the source with those files as they were.

The migration writes a journal at `/etc/unbounded/agent/isolation-migration.json` and records each step before it takes it. The journal is a write-ahead log: after a crash, it says what may have happened, not merely what finished. The steps prepare the target, stop the source, clean up host networking the source left behind, start the target, persist the target's applied configuration, and wait for the target to become ready. The wait is bounded at five minutes. After success, the migration prepares host state for the target mode, destroys the source, and removes the source's applied configuration, then marks the change complete.

The target can be `container` when the source is nspawn, and the reverse is also supported. A change to nspawn installs the apt guards that keep `systemd-machined` from being restarted under a running machine. A change to container removes those guards, because only an nspawn machine needs them. Both directions rewrite the nftables flush unit for the new mode. The flush unit is not started while the target node runs, so the new ordering takes effect at the next boot.

Rollback is part of the same operation. If any step after the source stops fails, the migration stops and destroys the target, confirms that the target is no longer running, restores the shared configuration snapshot, and starts the source again. The source is started only after the target is known to be gone. Two node stacks on one host would fight over the kubelet identity and the host networking, so when the target cannot be confirmed stopped, the source stays stopped and the change is reported as failed with a rollback failure. That leaves the node for an operator rather than running it twice.

A daemon that restarts in the middle of an in-place change resumes from the journal. Before the source was stopped, it removes the partial target. Once the source may be stopped, it rolls back to the source and reports the change as interrupted, rather than repeating a step that could have crashed the process. When the target was already running and recorded, it waits for readiness as the original run would, then finishes or rolls back.

### Reprovision change

A reprovision change resets the host and bootstraps it again in the target mode. The agent checks the target mode's host requirements and the bootstrap token before it stages anything. The token check matters because the reset removes the node, and a host that could not rejoin the cluster afterward would be left without a node.

The handoff stages everything it needs in `/var/lib/unbounded-agent-reprovision`: a journal, a copy of the agent binary, and the target configuration. The reset does not remove that directory. A systemd unit named `unbounded-agent-reprovision.service` runs the staged binary's hidden `isolation-reprovision` command, which waits up to ten minutes for the daemon that started the handoff to release installation ownership, then resets the host and bootstraps it in the target mode.

The daemon that starts the handoff reports the change as `InProgress` and ends the reconcile without waiting, because the reset will remove it. The handoff keeps retrying inside a 90-minute window. Runs are paced by a 30-second restart delay plus a backoff that doubles up to ten minutes, and the unit's start limit allows up to 30 runs in an hour. The 90-minute window is deliberately shorter than Machina's two-hour stale threshold, so a handoff that gives up reports `Failed` before the condition would call the change stale. `ChangeStale` therefore means the agent could not report at all, not that it was slowly retrying.

When the bootstrap succeeds, the daemon that starts afterward reports the change as `Succeeded` and records the target as the applied mode. When the bootstrap fails, the handoff reports `Failed` with the error and cleans up the staged unit and binary, and the first daemon that starts on the host reports it too if the handoff could not. If the reset itself never completed, the old node may still be partly in place, and the message says so.

The most common reprovision failure is a download source the host cannot reach after the reset. The bootstrap after the reset uses the same mirrors and download overrides a repave would, so a source that is unreachable from the host fails the bootstrap and leaves the host reset without a node. Check that the host can reach every configured download source before you allow a reprovision, especially on an isolated network.

## Drift

Drift is the difference between the mode you declared and the mode the node actually runs. It is expected and harmless: `spec.isolation` describes the next repave, while `status.isolation` describes the current node. The controller surfaces the difference so you can tell a pending change from a change that failed.

The agent owns both status fields. `status.isolation` is the mode the agent applied on the host. `status.isolationChange` is the latest attempt to move the node to the declared mode, with a target mode, a phase of `InProgress`, `Succeeded`, or `Failed`, a strategy of `InPlace` or `Reprovision`, a reason, a message, and the time of the last transition. The agent writes them in one merge patch with no resource version, because it is the only writer of those fields, and the two fields land together or not at all. That is what keeps `status.isolation` from trailing a `Succeeded` change by a reconcile.

The `IsolationDrift` condition compares the two. It is absent until the agent has reported an applied mode, because there is nothing to compare before that. It is false with reason `NoModeDeclared` when `spec.isolation` is empty and the node keeps its applied mode, and false with reason `AppliedModeMatches` when the two agree. It is true with reason `AppliedModeDiffers` when the declaration is waiting for the next repave. While a change is being made, the reason refines to `ChangeInProgress`, or to `ChangeStale` when an `InProgress` report is older than two hours. A failed change gets `ChangeFailed`, except that a refusal gets `ChangeRefused`, and a declaration of the experimental mode on a Machina that has not enabled it gets `ExperimentalModeDisabled`.

| Condition reason | Meaning |
|---|---|
| `NoModeDeclared` | `spec.isolation` is empty; a repave leaves the node in its applied mode |
| `AppliedModeMatches` | The declared and applied modes agree |
| `AppliedModeDiffers` | The declaration is waiting for the next repave |
| `ChangeInProgress` | The agent started the change and has not reported the outcome yet |
| `ChangeStale` | The change has been `InProgress` for over two hours without a report |
| `ChangeFailed` | The change failed; the message says whether the node was rolled back |
| `ChangeRefused` | The agent refused the change, for example for want of the reprovision opt-in |
| `ExperimentalModeDisabled` | The declared mode is experimental and Machina has not enabled it |

Nothing moves on drift. The controller never changes the mode in response to the condition, and the agent never rewrites `spec.isolation` on an existing Machine. The spec is yours; the status is the agent's. The only spec write in the registration path is the initial registration of a Machine the agent creates for itself, not a correction of an existing declaration.

### Agent version compatibility

The agent version on a host is pinned by `spec.agent.version`. An empty value tracks the latest published release, and the base URL or a fully qualified download URL can point at a mirror. Pinning is how you keep a host on an agent that understands its mode, and how you roll forward deliberately.

Compatibility follows from how the mode is stored. An agent configuration written before the `Isolation` field existed decodes as an empty mode, which is `nspawn`, so an older agent that does not know the field runs the node in nspawn. That is safe for a host already in nspawn and wrong for a host in another mode, which is why a binary that cannot operate the recorded mode is not allowed to run the node.

Before the daemon switches to an upgraded agent binary, it asks the candidate to answer `supports-isolation <mode>` for every mode other than nspawn that has an applied configuration on the host. The command reads nothing from the host and exits zero only when the binary can bootstrap and operate that mode. A binary that predates the command fails it, which is the answer wanted. The recovery script performs the same check on the last-good binary, so a recovery does not silently fall back to a binary that cannot run the node's mode.

The practical consequence is that an upgrade or recovery never puts an unsupported binary in charge of a node. If you need to move a host from nspawn to `none` or `container`, first run an agent version that supports the target mode, then make the change. If you are downgrading, confirm that the older version supports the mode the host is running before you pin it.

## See also

- [systemd-nspawn isolation]({{< relref "reference/agent/nspawn" >}}) explains the default mode in depth.
- [none mode]({{< relref "reference/agent/isolation-none" >}}) covers running the node stack directly on the host.
- [container mode]({{< relref "reference/agent/isolation-container" >}}) covers the experimental OCI container backend.
- [Agent configuration]({{< relref "reference/agent/configuration" >}}) puts the `Isolation` and `Experimental` fields in context.
- [Agent preflight]({{< relref "reference/agent/preflight" >}}) documents the checks each mode runs before a host is changed.
- [Node operations]({{< relref "guides/operations/node-operations" >}}) covers cordon, drain, repave, and Node deletion workflows.

