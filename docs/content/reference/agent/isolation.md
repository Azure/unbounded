---
title: "Isolation Modes"
weight: 3
description: "Where the unbounded-agent runs the Kubernetes node stack: nspawn, none, or container. Compare modes and choose one."
---

The unbounded-agent turns a host into a Kubernetes worker. The agent and its
daemon always run on the host. An **isolation mode** decides only where the
node stack runs: containerd, kubelet, CNI plugins, and CoreDNS.

| Mode | Node stack runs | Status |
|---|---|---|
| [`nspawn`]({{< relref "reference/agent/isolation-nspawn" >}}) | In a systemd-nspawn machine | Default. Existing behavior. |
| [`none`]({{< relref "reference/agent/isolation-none" >}}) | Directly on the host as agent-owned systemd units | Experimental. |
| [`container`]({{< relref "reference/agent/isolation-container" >}}) | In a privileged OCI container, for Azure Container Linux | Experimental. |

{{< callout type="important" >}}
`nspawn` is the default and the only mode validated on real hosts. `none` and
`container` are experimental: they have not yet been validated on real hosts.
Hosts with no recorded mode are `nspawn` hosts, and their behavior does not
change.
{{< /callout >}}

## Comparison

| | `nspawn` | `none` | `container` |
|---|---|---|---|
| Filesystem isolation | Separate rootfs per slot | None, host filesystem | Separate rootfs per slot |
| Node stack source | OCI rootfs image, artifacts downloaded into it | Release artifacts in versioned directories, no distro packages | OCI rootfs image run by an OCI runtime |
| Host `/usr` | Writable (installs `systemd-container`) | Not modified, so it may be read-only (not yet validated on a read-only `/usr` host) | May be read-only |
| Supervised by | `systemd-nspawn@<slot>.service` | `unbounded-containerd.service`, `unbounded-kubelet.service` | `unbounded-node-<slot>.service`, an agent-owned unit running `runc` |
| Slots and upgrade | Blue/green, `kube1` and `kube2` | Versioned directories and a `current` link | Blue/green, `kube1` and `kube2` |
| Upgrade downtime | Node moves to a new slot | Brief, the node stack restarts | Node moves to a new slot |
| bpffs | Private mount per machine | Host `/sys/fs/bpf` directly | Host `/sys/fs/bpf` (unverified) |
| Network | Host network namespace | Host | Host network namespace |
| Best for | General Linux hosts | Hosts where you accept running the node stack directly on the host | Immutable hosts such as Azure Container Linux |

Across all modes these paths resolve identically for the node stack and for pods:
`/run/containerd`, `/etc/containerd/certs.d`, `/var/lib/kubelet`, `/opt/cni`, and
bpffs (the same-path contract). The agent writes configuration under them on the
host side, and the node stack sees it at the same path. unbounded-net-node and
gantry do not change per mode.

## Choosing a mode

1. Start with `nspawn` unless you have a reason not to. It is the default, it is
   the most exercised, and it keeps the node stack separate from host packages.
2. Choose `container` when the host has a read-only `/usr` and cannot install
   `systemd-container`, such as Azure Container Linux. It is experimental.
3. Choose `none` when you want the node stack on the host with no machine or
   container layer, and the host has no kubelet or containerd of its own. The
   agent refuses to start on a host that already has one.
4. The mode is always explicit. The agent never auto-detects it, and preflight
   fails with a suggested mode rather than switching silently.

## Selecting a mode

Set the mode at provisioning time with any of:

- `spec.isolation` on the `Machine` (`nspawn`, `none`, or `container`).
- `--isolation` on `kubectl unbounded machine manual-bootstrap`.
- `Isolation` in the [agent config]({{< relref "reference/agent/configuration" >}}).
- `--isolation` on `unbounded-agent start` (and `unbounded-agent preflight`),
  which overrides `Isolation` from the config file.

An empty value is `nspawn`. The agent rejects any unrecognized value with a
clear error rather than running it as `nspawn`.

A host records the mode it was installed in. Starting the agent in a different
mode on that host is refused with `installation intent differs; explicit reset
is required`, whether the mode comes from the flag or the config file, so a flag
never re-modes a running node. To switch, see
[Changing the Isolation Mode]({{< relref "reference/agent/isolation-migration" >}}).

```bash
kubectl unbounded machine manual-bootstrap my-node --site mysite \
  --isolation container | ssh user@host sudo bash
```

## Operations

Each mode page has an Operations table: how that mode bootstraps, starts, stops,
soft-reboots, repaves, resets, checks health, and handles exec, GPUs, and
LocalDNS.

Host-global state is mode independent. The agent owns nftables flush, the
LocalDNS dummy interface and rules, WireGuard keys, sysctls, and bpffs mounts in
every mode; a backend only declares ordering dependencies on them.

## Uninstall script and reset

The uninstall script reads the mode from the applied config (a host with no
recorded mode is an `nspawn` host), removes that mode's node stack, and then the
parts that do not depend on the mode: the WireGuard and tunnel interfaces and
keys, the nftables flush service, `/etc/unbounded/kube`, the Kubernetes sysctl
file and the Docker `daemon.json`, unmasking Docker, restoring swap from the
saved `/etc/fstab.bak`, policy routing rules, `/usr/local/bin/unbounded-agent*`
and `/etc/unbounded/agent`.

The script is not a reset. It does not stop the agent daemon, and what it leaves
behind depends on the mode; each mode page says what. `unbounded-agent reset` is
the only way to a clean host.

## Running commands and reading logs

`unbounded-agent exec` and `unbounded-agent logs` work in every mode and replace
mode-specific recipes such as `machinectl shell`. The other mode pages link here.

```bash
sudo unbounded-agent exec -- crictl ps
sudo unbounded-agent logs --unit kubelet --follow --lines 200
```

`exec` runs the command after `--` in the node stack. `logs` streams the journal
for a unit in the node stack: `--unit` selects it, `--follow` keeps the stream
open, and `--lines` limits output to the last N entries.

## Drift

The agent records the mode it applied in `status.isolation` on the
`Machine`, and its attempts to change the mode in `status.isolationChange`.
Machina compares `status.isolation` with `spec.isolation` and sets the
`IsolationDrift` condition:

| Reason | Status | Meaning |
|---|---|---|
| `NoModeDeclared` | `False` | `spec.isolation` is empty; a repave leaves the node as it is |
| `AppliedModeMatches` | `False` | The declared mode runs |
| `AppliedModeDiffers` | `True` | A different mode is declared; it applies at the next repave |
| `ChangeInProgress` | `True` | The repave is changing the mode now |
| `ChangeFailed` | `True` | The last attempt at the declared mode failed; a new attempt needs a new trigger |

The condition is absent until the agent has reported a mode. Nothing moves on
drift. The agent never writes `spec`: the declaration is yours, so a mismatch you
create stays visible until a repave applies it or you withdraw it. See
[Machina CRDs]({{< relref "reference/machina-crd" >}}).

Machina does not check the agent version. Pin an agent version that supports the
mode you set with `spec.agent.version`. An agent that predates isolation modes
does not know the `Isolation` config key and ignores it, so it runs the node as
`nspawn`. Such an agent also does not report `status.isolation`, so
`IsolationDrift` stays absent and does not flag the mismatch.

## See also

- [nspawn Mode]({{< relref "reference/agent/isolation-nspawn" >}})
- [none Mode]({{< relref "reference/agent/isolation-none" >}})
- [container Mode]({{< relref "reference/agent/isolation-container" >}})
- [Changing the Isolation Mode]({{< relref "reference/agent/isolation-migration" >}})
- [Agent Configuration]({{< relref "reference/agent/configuration" >}})
