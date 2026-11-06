---
title: "Isolation Modes"
weight: 3
description: "Where the unbounded-agent runs the Kubernetes node stack: nspawn, none or container. Compare modes, choose one, and see how drift from the declared mode is reported."
---

The unbounded-agent turns a host into a Kubernetes worker. The agent and its
daemon always run on the host. An **isolation mode** decides only where the
node stack runs: containerd, kubelet, CNI plugins, and CoreDNS.

| Mode | Node stack runs | Status |
|---|---|---|
| [`nspawn`]({{< relref "reference/agent/nspawn" >}}) | In a systemd-nspawn machine | Default. Existing behavior. |
| [`none`]({{< relref "reference/agent/isolation-none" >}}) | Directly on the host as agent-owned systemd units | Experimental. |
| [`container`]({{< relref "reference/agent/isolation-container" >}}) | In a privileged OCI container supervised by an agent-owned unit | Experimental, and off unless explicitly enabled. |

{{< callout type="important" >}}
`nspawn` is the default and the only mode validated on real hosts. `none` and
`container` are experimental: they have not yet been validated on real hosts.
`container` is also gated: it is refused unless you enable it explicitly (see
[Enabling the container mode](#enabling-the-container-mode)). Hosts with no
recorded mode are `nspawn` hosts, and their behavior does not change.
{{< /callout >}}

## Comparison

| | `nspawn` | `none` | `container` |
|---|---|---|---|
| Filesystem isolation | Separate rootfs per slot | None, host filesystem | Separate rootfs per slot |
| Node stack source | OCI rootfs image, artifacts downloaded into it | Release artifacts in versioned directories, no distro packages | OCI rootfs image, artifacts downloaded into it |
| Host `/usr` | Writable (installs `systemd-container`) | Not modified, so it may be read-only (not yet validated on a read-only `/usr` host) | Not modified; `runc` is installed under `/opt/unbounded` |
| Supervised by | `systemd-nspawn@<slot>.service` | `unbounded-containerd.service`, `unbounded-kubelet.service` | `unbounded-node-<slot>.service` running `runc` |
| Slots and upgrade | Blue/green, `kube1` and `kube2` | Versioned directories and a `current` link | Blue/green, `kube1` and `kube2` |
| Upgrade downtime | Node moves to a new slot | Brief, the node stack restarts | Node moves to a new slot |
| bpffs | Private mount per machine | Host `/sys/fs/bpf` directly | Private mount per slot |
| Network | Host network namespace | Host | Host network namespace |
| Best for | General Linux hosts | Hosts where you accept running the node stack directly on the host | Image-based hosts that cannot run systemd-nspawn, such as Azure Container Linux (unvalidated) |

Across all modes these paths resolve identically for the node stack and for pods:
`/run/containerd`, `/etc/containerd/certs.d`, `/var/lib/kubelet`, `/opt/cni`, and
bpffs (the same-path contract). The agent writes configuration under them on the
host side, and the node stack sees it at the same path. unbounded-net-node and
gantry do not change per mode.

## Choosing a mode

1. Start with `nspawn` unless you have a reason not to. It is the default, it is
   the most exercised, and it keeps the node stack separate from host packages.
2. Choose `none` when you want the node stack on the host with no machine
   layer, and the host has no kubelet or containerd of its own. The agent
   refuses to bootstrap a host that already has one.
3. Consider `container` only to evaluate it, on a host that cannot run
   systemd-nspawn. It must be enabled explicitly.
4. The mode is always explicit. The agent never auto-detects it, and a refused
   host is reported with the reason rather than switched to another mode.

## Selecting a mode

Set the mode at provisioning time with any of:

- `spec.isolation` on the `Machine`.
- `--isolation` on `kubectl unbounded machine manual-bootstrap`.
- `Isolation` in the [agent config]({{< relref "reference/agent/configuration" >}}).
- `--isolation` on `unbounded-agent start` (and `unbounded-agent preflight`),
  which overrides `Isolation` from the config file.

An empty value is `nspawn`. Each of these accepts only the modes this release
can run, `nspawn`, `none` and `container`: the Machine CRD rejects any other
`spec.isolation`, `kubectl` rejects any other `--isolation` before it contacts
the cluster, and the agent rejects any other `Isolation` with a clear error
rather than running it as `nspawn`.

A host records the mode it was installed in. For `nspawn` the record is the
same whether the mode is unset or set to `nspawn`, so declaring the default on
an existing host changes nothing. Starting the agent in a different mode on an
installed host is refused with `installation intent differs; explicit reset is
required`, whether the mode comes from the flag or the config file, so a flag
never re-modes a running node.

```bash
kubectl unbounded machine manual-bootstrap my-node --site mysite \
  --isolation none | ssh user@host sudo bash
```

### Enabling the container mode

`container` is experimental and every path that selects it refuses it until it
is enabled:

| Where | How to enable | Refused without it |
|---|---|---|
| Agent | `"Experimental": {"ContainerIsolation": true}` in the [agent config]({{< relref "reference/agent/configuration" >}}) | The config fails validation, `--isolation container` is rejected, and a change to `container` at a repave is reported `Failed` with reason `ExperimentalNotEnabled` |
| Machina | `experimentalContainerIsolation: true` in `config.yaml` of the `machina-config` ConfigMap in `unbounded-system`. The operator keeps your edits to that ConfigMap and restarts machina when it changes. For manifests rendered from the repository, `make machina-manifests MACHINA_EXPERIMENTAL_CONTAINER_ISOLATION=true` sets it | Machina does not provision a Machine that declares `container` (it is marked `Failed`), and `IsolationDrift` reports `ExperimentalModeDisabled` |
| Metalman | `--experimental-container-isolation` on `metalman serve-pxe` (for the operator-managed Deployment, append it with a metalman `extraArgs` [workload override]({{< relref "reference/workload-overrides" >}})) | A `HostReplace` of a Machine that declares `container` fails before any power action (target reason `ExperimentalModeDisabled`), and netboot serves no agent configuration for it and no installer boot while an install is pending. Without a pending install, `grub.cfg` still chainloads the local disk, so the host keeps booting what it has installed |
| machine-ops-controller | `--experimental-container-isolation` on `machine-ops-controller` (`MACHINE_OPS_EXPERIMENTAL_CONTAINER_ISOLATION=true` for `make machine-ops-manifests`) | A `HostReplace` of a Machine that declares `container` fails before the provider runs |
| kubectl | `--experimental-container-isolation` on `kubectl unbounded machine manual-bootstrap` | `--isolation container` is rejected before the cluster is contacted |

The agent's setting is the one that decides. Machina, metalman and
machine-ops-controller with the setting, and `kubectl` with the flag, write it
into every agent configuration they produce, whatever mode the Machine
declares, so a node can later be changed to `container` at a repave. Enable it
on every provisioner that installs the node. The `IsolationDrift` condition is
maintained by machina for every Machine, metalman-provisioned ones included, so
a pending change to `container` is reported as `ExperimentalModeDisabled`
unless machina enables the mode as well.

```bash
kubectl unbounded machine manual-bootstrap my-node --site mysite \
  --isolation container --experimental-container-isolation | ssh user@host sudo bash
```

## Operations

Each mode page has an Operations table: how that mode bootstraps, starts, stops,
soft-reboots, repaves, resets, checks health, and handles exec, GPUs, and
LocalDNS.

Host-global state is mode independent. The agent owns nftables flush, the
LocalDNS dummy interface and rules, WireGuard keys, sysctls, and bpffs mounts in
every mode; a backend only declares ordering dependencies on them.

## Removing a node

`unbounded-agent reset` removes the node in every mode and returns the host to
a clean state. It always runs the `nspawn` teardown, which does nothing on a
host without machines, and adds a mode's own teardown only when the host has
that mode's units or directory, so it never removes what another installation
owns.

## Running commands and reading logs

`unbounded-agent exec` and `unbounded-agent logs` work in every mode and replace
mode-specific recipes such as `machinectl shell`. The other mode pages link here.

```bash
sudo unbounded-agent exec -- crictl ps
sudo unbounded-agent logs --unit kubelet --follow --lines 200
```

`exec` runs the command after `--` in the node stack. `logs` streams the journal
for a unit in the node stack: `--unit` selects it, `--follow` keeps the stream
open, and `--lines` limits output to the last N entries. Both find the node
from its applied config, as the agent daemon does.

## Changing the mode

`spec.isolation` declares the mode a node moves to at its next repave, which the
agent performs when the Node is deleted. Setting it never moves a running node.

A change between `nspawn` and `container` is made in place (`InPlace`): the
new mode's node is built in the other slot beside the running one, the old
node is stopped, and the new one is started and waited on. If it does not come
up it is removed and the old node is started again. The change is journaled in
`/etc/unbounded/agent/isolation-migration.json`, so an agent that restarts in
the middle finishes it or rolls it back. It needs no annotation, but the target
`container` must be enabled on the host (see
[Enabling the container mode](#enabling-the-container-mode)).

A change to or from `none` resets the host and bootstraps it again in the new
mode (a `Reprovision`). Because it wipes the node stack, it needs an
explicit opt-in separate from `spec.isolation`: the annotation
`unbounded-cloud.io/allow-reprovision: "true"` on the Machine. Without it, the
agent refuses the change, reports it in `status.isolationChange` as `Failed`
with reason `ReprovisionNotAllowed`, and repaves in the mode the node already
runs so version and configuration rollouts continue. A mode the agent cannot
run is refused the same way with reason `UnsupportedMode`.

To change the mode:

1. Set `spec.isolation` to the new mode and add the annotation.
2. Cordon and drain the node, then delete its Node object, as for any repave.

The agent then:

1. Checks the new mode's host requirements and, for a host that joins with a
   bootstrap token, that the API server still accepts the token. An expired
   token is caught when the API server can be reached; renew it, or bootstrap
   the host with attestation, and the change is tried again at the next
   repave. An attested host attests again instead.
2. Stages a copy of itself, the target configuration (with the Machine's
   download sources) and a journal in `/var/lib/unbounded-agent-reprovision`,
   and checks that the target configuration resolves. Nothing is reset if any
   of this fails.
3. Starts `unbounded-agent-reprovision.service`, which runs the agent reset and
   then the bootstrap in the new mode. For `none`, the bootstrap first refuses
   a host that still runs a kubelet or containerd the agent does not own.

The change is `InProgress` until the agent the bootstrap installs reports it
`Succeeded`, with `status.isolation` set in the same update. The service
retries a failing run, waiting longer each time, for 90 minutes from the start
of the change. It then gives up, reports `Failed` while it still has the
bootstrap token, and removes its staged files; the next agent to run on the
host reports it too. A change still `InProgress` after two hours shows as
`ChangeStale`.

A change that fails before the reset is not retried on its own: the node keeps
the mode it had and the next repave tries again.

The target's artifacts are downloaded by the bootstrap, after the reset, not
before it. The check in step 2 confirms the configuration resolves, but not
that its download sources are reachable. If they stay unreachable until the
service gives up, the host is left reset with no node stack: fix the download
sources (`spec.agent.downloads` or the offline bundle) and bootstrap the host
again. Check that the target mode's download sources work before you start a
reprovision. Removing the annotation after the change stops
later changes from resetting the host.

## Drift

The agent records the mode it applied in `status.isolation` on the `Machine`,
and its latest attempt to change the mode in `status.isolationChange`. Only the
agent writes these fields. Machina compares `status.isolation` with
`spec.isolation` and sets the `IsolationDrift` condition:

| Reason | Status | Meaning |
|---|---|---|
| `NoModeDeclared` | `False` | `spec.isolation` is empty; a repave leaves the node as it is |
| `AppliedModeMatches` | `False` | The declared mode runs |
| `AppliedModeDiffers` | `True` | A different mode is declared; it applies at the next repave |
| `ChangeInProgress` | `True` | The repave is changing the mode now |
| `ChangeStale` | `True` | The change has been `InProgress` for more than two hours without a new report; check the agent on the host |
| `ChangeFailed` | `True` | The last attempt at the declared mode failed; it is tried again at the next repave |
| `ChangeRefused` | `True` | The agent refused the change: it cannot run the mode (`UnsupportedMode`), the mode is experimental and the host's agent configuration does not enable it (`ExperimentalNotEnabled`), or the change needs a reprovision and the Machine lacks the `unbounded-cloud.io/allow-reprovision` annotation (`ReprovisionNotAllowed`). The message says what to do |
| `ExperimentalModeDisabled` | `True` | `spec.isolation` is `container` and machina does not enable it (`experimentalContainerIsolation`) |

The condition is absent until the agent has reported a mode. Nothing moves on
drift. The agent never writes `spec` on an existing Machine: the declaration is
yours, so a mismatch you create stays visible until a repave applies it or you
withdraw it. The only time the agent sets `spec.isolation` is when it creates
the Machine itself at self-registration, where it records the mode it runs. See
[Machina CRDs]({{< relref "reference/machina-crd" >}}).

Machina does not check the agent version. Pin an agent version that supports
the mode you set with `spec.agent.version`. An agent that predates isolation
modes does not know the `Isolation` config key and ignores it, so it runs the
node as `nspawn`. Such an agent also does not report `status.isolation`, so
`IsolationDrift` stays absent.

On a `none` or `container` host an older agent does not find the node at all:
the applied config of such a node is stored under a name older agents do not
read. An
agent upgrade, and the daemon's recovery to its last-known-good binary, refuse
an agent binary that cannot operate the mode the node runs in.

## See also

- [systemd-nspawn Isolation]({{< relref "reference/agent/nspawn" >}})
- [none Mode]({{< relref "reference/agent/isolation-none" >}})
- [container Mode (Experimental)]({{< relref "reference/agent/isolation-container" >}})
- [Agent Configuration]({{< relref "reference/agent/configuration" >}})
