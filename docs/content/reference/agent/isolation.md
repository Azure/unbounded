---
title: "Isolation Modes"
weight: 3
description: "Where the unbounded-agent runs the Kubernetes node stack: nspawn or none. Compare modes, choose one, and see how drift from the declared mode is reported."
---

The unbounded-agent turns a host into a Kubernetes worker. The agent and its
daemon always run on the host. An **isolation mode** decides only where the
node stack runs: containerd, kubelet, CNI plugins, and CoreDNS.

| Mode | Node stack runs | Status |
|---|---|---|
| [`nspawn`]({{< relref "reference/agent/nspawn" >}}) | In a systemd-nspawn machine | Default. Existing behavior. |
| [`none`]({{< relref "reference/agent/isolation-none" >}}) | Directly on the host as agent-owned systemd units | Experimental. |

{{< callout type="important" >}}
`nspawn` is the default and the only mode validated on real hosts. `none` is
experimental: it has not yet been validated on real hosts. Hosts with no
recorded mode are `nspawn` hosts, and their behavior does not change.
{{< /callout >}}

## Comparison

| | `nspawn` | `none` |
|---|---|---|
| Filesystem isolation | Separate rootfs per slot | None, host filesystem |
| Node stack source | OCI rootfs image, artifacts downloaded into it | Release artifacts in versioned directories, no distro packages |
| Host `/usr` | Writable (installs `systemd-container`) | Not modified, so it may be read-only (not yet validated on a read-only `/usr` host) |
| Supervised by | `systemd-nspawn@<slot>.service` | `unbounded-containerd.service`, `unbounded-kubelet.service` |
| Slots and upgrade | Blue/green, `kube1` and `kube2` | Versioned directories and a `current` link |
| Upgrade downtime | Node moves to a new slot | Brief, the node stack restarts |
| bpffs | Private mount per machine | Host `/sys/fs/bpf` directly |
| Network | Host network namespace | Host |
| Best for | General Linux hosts | Hosts where you accept running the node stack directly on the host |

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
3. The mode is always explicit. The agent never auto-detects it, and a refused
   host is reported with the reason rather than switched to another mode.

## Selecting a mode

Set the mode at provisioning time with any of:

- `spec.isolation` on the `Machine`.
- `--isolation` on `kubectl unbounded machine manual-bootstrap`.
- `Isolation` in the [agent config]({{< relref "reference/agent/configuration" >}}).
- `--isolation` on `unbounded-agent start` (and `unbounded-agent preflight`),
  which overrides `Isolation` from the config file.

An empty value is `nspawn`. Each of these accepts only the modes this release
can run, `nspawn` and `none`: the Machine CRD rejects any other
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

The agent in this release cannot change the mode of a running node. A node is
put in `none` by bootstrapping the host in that mode. If a repave finds a
different mode declared, the agent refuses the change, reports it in
`status.isolationChange` as `Failed` with reason `UnsupportedMode`, and repaves
in the mode the node already runs so version and configuration rollouts
continue. To move a host to another mode, run `unbounded-agent reset` on it and
bootstrap it again in that mode.

A change that resets and bootstraps the host again (a `Reprovision`) also needs
an explicit opt-in separate from `spec.isolation`: the annotation
`unbounded-cloud.io/allow-reprovision: "true"` on the Machine. Without it, an
agent that supports such a change refuses it with reason
`ReprovisionNotAllowed` and repaves in the current mode.

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
| `ChangeRefused` | `True` | The agent refused the change: it cannot run the mode (`UnsupportedMode`), or the change needs a reprovision and the Machine lacks the `unbounded-cloud.io/allow-reprovision` annotation (`ReprovisionNotAllowed`). The message says what to do |

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

On a `none` host an older agent does not find the node at all: the applied
config of a `none` node is stored under a name older agents do not read. An
agent upgrade, and the daemon's recovery to its last-known-good binary, refuse
an agent binary that cannot operate the mode the node runs in.

## See also

- [systemd-nspawn Isolation]({{< relref "reference/agent/nspawn" >}})
- [none Mode]({{< relref "reference/agent/isolation-none" >}})
- [Agent Configuration]({{< relref "reference/agent/configuration" >}})
