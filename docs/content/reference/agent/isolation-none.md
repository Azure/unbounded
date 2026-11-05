---
title: "none Mode"
weight: 5
description: "Run the Kubernetes node stack directly on the host as agent-owned systemd units."
---

In `none` mode there is no machine and no container. containerd and kubelet run
directly on the host as systemd units that the agent owns, installed from the
same release artifacts the agent downloads for the other modes. The agent and
its daemon run on the host, as in every mode. See
[Isolation Modes]({{< relref "reference/agent/isolation" >}}) to compare.

{{< callout type="important" >}}
`none` mode is experimental and has not yet been validated on real hosts.
{{< /callout >}}

## Requirements

- Writable `/etc`, `/opt`, and `/var`. `/usr` is not modified.
- `systemctl` on the host.
- **No kubelet or containerd that the agent does not own.** Bootstrap refuses
  the host, before it changes anything, if it finds, for the standard names, a
  `kubelet.service` or `containerd.service` unit file in the systemd unit
  directories, a `kubelet` or `containerd` executable on the standard binary
  paths, or a running `kubelet` or `containerd` process. The `preflight`
  subcommand reports the same findings. A unit masked to `/dev/null` (which an
  `nspawn` installation leaves behind) is not a finding. Remove them, or use
  `nspawn`.
- `/usr` is never modified, so a read-only `/usr` is expected to work. This has
  not yet been validated on a read-only `/usr` host.
- Units start after `network-online.target`.

Preflight does not change the node. Its writability check creates and removes a
small temporary file in `/etc`, `/var` and the node base directory (or the
nearest existing parent of it).

Select it as described in
[Selecting a mode]({{< relref "reference/agent/isolation#selecting-a-mode" >}}).

## Layout

| Path | Description |
|---|---|
| `/opt/unbounded/node/<version>/` | Artifacts for one version. Executables are under `bin/`, CNI plugins under `cni/bin/`. |
| `/opt/unbounded/node/current` | Relative symlink to the active version. Units run binaries through `current/bin`, so flipping the link changes what they run. |
| `/etc/systemd/system/unbounded-containerd.service` | containerd, agent-owned. |
| `/etc/systemd/system/unbounded-kubelet.service` | kubelet, agent-owned. Wants and starts after the containerd unit. |
| `/etc/systemd/system/unbounded-localdns.service` | CoreDNS, agent-owned, when LocalDNS is enabled. Starts before containerd and kubelet. |
| `/etc/systemd/system/<unit>.d/` | Drop-ins for the agent-owned units: `PATH` with the active version first, kubelet environment, LocalDNS ordering. |
| `/opt/unbounded/node/<version>/libexec/` | The LocalDNS supervisor script for that version. |
| `/opt/cni/bin/<plugin>` | A symlink per plugin of the active version, through `current`. Plugins other components install here (for example unbounded-net's IPAM plugin) are left alone. |
| `/etc/unbounded/containerd/config.toml` | The containerd configuration, agent-owned. The distro's `/etc/containerd/config.toml` is never written. |
| `/etc/unbounded/agent/none-kube1-applied-config.json` | The applied config. |

The node has no blue/green slot; the version directory plays that role. The
applied config is recorded for the slot `kube1` as
`none-kube1-applied-config.json`. Agents that predate `none` read only
`kube1-applied-config.json` and `kube2-applied-config.json`, so such an agent
finds no node on this host and refuses to operate it rather than treating it as
an `nspawn` machine. An agent upgrade, and the daemon's recovery to its
last-known-good binary, refuse a binary that does not answer
`unbounded-agent supports-isolation none`. The version directory is named for
the Kubernetes version plus a short digest of the other artifact versions, so an
unchanged configuration reuses its directory.

The agent never writes or edits the distro's `kubelet.service` or
`containerd.service`.

The same-path contract holds, as in every mode; see
[Isolation Modes]({{< relref "reference/agent/isolation" >}}).

## Operations

| Operation | Behavior |
|---|---|
| Bootstrap | The ownership check above, host preparation (the same nftables, sysctl, swap and docker phases as `nspawn`, plus loading `overlay` and `br_netfilter`; the apt and needrestart guards against systemd-machined restarts, which first-boot provisioning writes for every mode, are removed), install the version directory and units, reload systemd, point `current` at the version, write the configuration at the shared host paths, start the units, wait for kubelet to bootstrap. |
| Start | Enable and start LocalDNS (and wait for it), then containerd (and import preloaded images), then kubelet. |
| Stop | Stop kubelet, then containerd, then LocalDNS. Every unit is attempted even if one fails. |
| Soft reboot (`NodeReboot`) | Restart LocalDNS, containerd, then kubelet, wait for LocalDNS and for kubelet to be active. The host does not reboot. |
| Repave and upgrade | The new version is installed beside the running one. Then the units stop, the pods left running are ended (the shims of the agent's containerd and the pods' cgroups), host networking is cleaned as for an `nspawn` repave, kubelet's old credentials are removed so it bootstraps again, `current` flips, the units are written for the new configuration, the units start, and the agent waits for LocalDNS and kubelet. If any step after the flip fails, the link is flipped back and the previous version is started with the units, configuration and host networking it ran with. The node is briefly down. The caller drains and deletes the Node first, as for `nspawn`. A reconfiguration that turns LocalDNS off disables its unit after the stack has stopped, before the unit file is removed. A declared change of `spec.isolation` is refused, and the repave goes ahead in `none`. |
| Reset and delete | `unbounded-agent reset`. It always runs the `nspawn` teardown, which does nothing on a host without machines, and runs the `none` teardown because the host has the agent's units or `/opt/unbounded/node`. Same order as `nspawn`: stop the units, remove WireGuard keys, remove the configuration the agent wrote at shared paths, end the shims of the agent's containerd and every process left in the pods' cgroup (`kubepods.slice`, or `kubepods` with the cgroupfs driver), remove the units, drop-ins, the agent's links in `/opt/cni/bin` and `/opt/unbounded/node`, unmount and remove `/var/lib/kubelet`, `/var/lib/containerd` and `/etc/cni/net.d`, then the host-global cleanup. A unit that does not stop keeps its file and the installed versions stay, so a second reset can finish. A data directory with a mount that cannot be unmounted is kept and the failure is reported, so a volume mounted below it is never deleted through the mount; a mount table that cannot be read is an error. |
| Health | Both units are active, kubelet is healthy, and LocalDNS answers when enabled. |
| Exec and logs | `unbounded-agent exec` runs on the host. `unbounded-agent logs` reads the journal of the agent-owned units; `--unit kubelet` and `--unit containerd` select them. See [Running commands and reading logs]({{< relref "reference/agent/isolation" >}}#running-commands-and-reading-logs). |
| GPU and devices | Native. Nothing is bind-mounted because the stack sees the host directly. NVIDIA GPUs need the NVIDIA Container Toolkit on the host. See [Toolkit and CDI in `none` mode]({{< relref "reference/gpu/nvidia#toolkit-and-cdi-in-none-mode" >}}) and [AMD]({{< relref "reference/gpu/amd" >}}). |
| LocalDNS | CoreDNS runs as the `unbounded-localdns.service` host unit, as a systemd `DynamicUser`, so `/etc/unbounded` and `/etc/unbounded/localdns` are made world-traversable (`0755`). The LocalDNS network unit is unchanged. The host's own `/etc/resolv.conf` is not rewritten; kubelet uses its dedicated resolver file. |

## Limits

- **No filesystem isolation.** The node stack shares the host filesystem and
  process table. Host packages and the node stack can conflict, which is why
  preflight refuses an existing kubelet or containerd.
- **No per-machine bpffs.** The host `/sys/fs/bpf` is used directly. eBPF CNIs
  such as Cilium that expect a machine-scoped bpffs see the host's. This is a
  documented difference from `nspawn`.
- **Upgrades are not blue/green.** The node is briefly down during the flip.
  Automatic rollback restores the previous binaries. Configuration files live at
  the shared host paths and are not versioned, so the agent saves them before an
  upgrade (the containerd, kubelet, CNI and LocalDNS configuration and the
  kubelet credentials, in `/etc/unbounded/agent/none-upgrade-config`) and puts them back,
  byte for byte, before the previous version starts again. If they cannot be put
  back the error says so and the saved copy is kept. Before anything changes the
  agent records the version being replaced, and a digest of the configuration
  being applied, in `/etc/unbounded/agent/none-upgrade-journal.json`; if the
  agent stops in the middle of an upgrade, its next start rolls the node back to
  that version, or finishes the upgrade if the applied config shows it had
  already succeeded, and either way leaves the stack running. A recovery that
  fails is logged, the daemon keeps running, and the next repave tries it again
  before it upgrades. CNI configuration in `/etc/cni/net.d` and the CNI state in
  `/var/lib/cni` are removed before the new version starts, as a new `nspawn`
  machine starts without them, so pods get addresses from the new Node's pod
  CIDR.
- **Pods outlive the units.** containerd runs with `KillMode=process`, so a plain
  unit stop leaves pods running, and ending a shim reparents the pod's processes
  instead of stopping them. Reset ends the shims and then kills the pods' whole
  cgroup (`cgroup.kill`; on a kernel without it, each process listed in the
  cgroup). An upgrade does the same before it cleans host networking. A soft
  reboot does not: the pods keep running while the units restart.
- **NVIDIA GPUs.** The host driver and the NVIDIA Container Toolkit must be
  installed on the host; the agent does not install them in this mode. See
  [Toolkit and CDI in `none` mode]({{< relref "reference/gpu/nvidia#toolkit-and-cdi-in-none-mode" >}}).
- **Changing the mode.** A change to or from `none` is a reprovision: at the
  next repave the agent resets the host and bootstraps it again in the declared
  mode, and only when the Machine carries the annotation
  `unbounded-cloud.io/allow-reprovision: "true"`. Without it the change is
  reported as refused and repaves go ahead in the current mode. See
  [Changing the mode]({{< relref "reference/agent/isolation#changing-the-mode" >}}).

## Troubleshooting

| Symptom | Check |
|---|---|
| Bootstrap or preflight reports an unmanaged kubelet or containerd | Remove the listed unit, binary, or process, or choose another mode. The message lists each finding. |
| An agent upgrade is refused with "cannot operate isolation mode" | The candidate agent predates `none`. Use an agent version that supports it. |
| Preflight reports a path is not writable | `/etc`, `/opt/unbounded/node`, and `/var` must be writable. Check for a read-only mount. |
| Node not Ready after an upgrade | `sudo unbounded-agent logs --unit kubelet --lines 200`. The agent flips `current` back automatically; `readlink /opt/unbounded/node/current` shows which version is active. |
| A unit is down | `systemctl status unbounded-kubelet.service unbounded-containerd.service`. |
| Cilium or another eBPF CNI cannot find its bpffs | Expected difference: `none` uses the host `/sys/fs/bpf`. |
