---
title: "Machine Operations"
weight: 7
description: "Run, target, observe, and clean up day-2 MachineOperation resources for hosts, nodes, and the unbounded-agent."
---

After a Machine is provisioned, you change it by creating `MachineOperation` resources. Each operation is a
cluster-scoped, job-like object: you declare a target, an operation kind, and optional parameters, and the component
that owns that kind carries it out and records the outcome in the operation's status. This page explains the boundary
between host, node, and agent operations, who executes each kind, how to create and target operations safely, how to
follow progress, and how completed operations are retired.

The model is deliberately uniform. `kubectl apply` and `kubectl unbounded machine operation create` produce the same
resource, every kind reports through the same `Pending`, `InProgress`, `Complete`, or `Failed` phases, and every kind
leaves an audit record in the cluster. What differs is the blast radius. A node restart leaves the host and its disks
alone; a host power cycle takes everything on the machine down; replacing a host destroys local state. Choosing the
right boundary is the most important decision you make before creating an operation.

## Boundaries

Unbounded divides the machine into three operational layers. They are nested, so an operation at an outer layer can
interrupt everything inside it, while an operation at an inner layer leaves the outer layers running.

### The Host

The host boundary is the virtual machine, the PXE-booted machine, or the bare-metal server itself. Host operations act
on power state, boot, or the existence of the machine: powering it on or off, rebooting it out of band, or replacing it
with a freshly provisioned host. They are executed from outside the guest by a provider API or a BMC, so they do not
require a healthy operating system, a running kubelet, or a reachable agent. That independence is what makes host
operations the reliable fallback when a machine is wedged, and also what makes them dangerous: a hard reboot or power
off does not drain pods or wait for the node to shut down cleanly.

### The Node

The node boundary is the workload stack that runs Kubernetes on the host. In `nspawn` isolation that stack lives inside
a `systemd-nspawn` container, and in the other isolation modes it is represented by host units or a container. It
contains the kubelet, the container runtime (containerd), the CNI configuration, and the pods you scheduled. A node
operation restarts that stack in place: services stop, the node is restarted, and services come back up. The root
filesystem is retained and the host operating system is not rebooted, so the disk, the agent installation, and the host
network configuration stay as they were.

### The Agent

The agent is the third operational target. The `unbounded-agent` runs on the host, manages the node, and watches
`MachineOperation` resources addressed to its Machine. Agent operations change the agent binary itself or remove the
agent and the resources it manages. They are the only operations that can run without involving a provider, a BMC, or
node reboot logic: the agent is already running on the host and has the host-resident access needed to stage a new
binary, stop its own service, or call `machinectl` and `systemctl`.

Treat the three layers as a decision tree. If the operating system is healthy but the node stack is stuck, operate on
the node. If the host is unreachable or the OS cannot reboot itself, operate on the host. If you need a new agent
version or want to decommission the agent's installation, operate on the agent.

## Operations at a Glance

The API defines the following operation kinds. The executor column is the component that handles the kind in a steady
state deployment.

| Operation | Scope | Executed by | What happens |
| --- | --- | --- | --- |
| `HostPowerOff` | Host | `machine-ops-controller` or `metalman` | The provider or BMC powers the host off. |
| `HostPowerOn` | Host | `machine-ops-controller` or `metalman` | The provider or BMC powers the host on. |
| `HostHardReboot` | Host | `machine-ops-controller` or `metalman` | The host is power cycled out of band. |
| `HostSoftReboot` | Host OS | `unbounded-agent` | The host runs `systemctl reboot` and reports after it boots again. |
| `HostReplace` | Host | `machine-ops-controller` or `metalman` | The VM is recreated or the physical host is reimaged with fresh bootstrap data. |
| `NodeReboot` | Node | `unbounded-agent` | The node stack restarts in place and the root filesystem is retained. |
| `AgentUpgrade` | Agent | `unbounded-agent` | A new agent binary is staged in the inactive slot and the daemon restarts from it. |
| `AgentReset` | Agent | `unbounded-agent` | The agent and the resources it manages are removed, and the daemon stops last. |

Two further kinds exist in the API, `HostAllocate` and `HostRelease`. They act on a `MachineAllocation`, not on an
existing host, and neither the external machine-ops controller, `metalman`, nor the agent processes them. The
allocation controller creates them as part of launching and releasing hosts, and they forbid fields that the day-2
kinds allow, including `machineSelector` and `ttlSecondsAfterFinished`. The `kubectl unbounded machine operation create`
command does not accept them as `--kind` values. Use this guide for operations on hosts, nodes, and agents that already
exist.

The boundary and the executor are related but not identical. `HostSoftReboot` is a host operation that only the agent
can perform, because the reboot is issued from inside the host by `systemctl` rather than by a provider. Conversely,
`HostHardReboot`, `HostPowerOff`, `HostPowerOn`, and `HostReplace` require out-of-band access and are therefore handled
by the component that owns the provider or BMC connection for that Machine. `NodeReboot`, `AgentUpgrade`, and
`AgentReset` are always agent work.

## Component Ownership

Each `MachineOperation` is owned by exactly one component, and the components divide Machines by site and provider.
This matters in multi-site deployments, where several controllers watch the same cluster and each must avoid acting on
another site's hardware.

### machine-ops-controller

`machine-ops-controller` implements host operations for cloud providers. It is deployed per site and provider pair:
`--site` and `--provider` must be set together, and the controller only acts on Machines whose site label matches
`--site` and whose resolved host provider matches `--provider`. When both flags are empty, the controller runs without
that scoping.

For a Machine that sets `spec.host.azure.resourceID`, the built-in Azure provider maps the supported kinds to Azure VM
actions: `HostHardReboot` restarts the VM, `HostPowerOff` powers it off, `HostPowerOn` starts it, and `HostReplace`
destroys and recreates it with replacement user data. Machines that use a registered external provider set
`spec.host.external.provider` and either `providerID` or `machineRef`, and the same controller dispatches to that
provider's registered operations. The legacy top-level `spec.provider` and `spec.providerID` fields are still read for
Machines created before the `spec.host` layout.

Provider credentials are selected with `MachineOperationCredential` resources, which match on the Machine's site label
and on the provider name. A credential either uses workload identity available to the controller process or delegates
to an external plugin with a secret reference. For provider-owned Machines, the controller also snapshots the exact
provider resource (`apiGroup`, `kind`, `name`, `uid`, and `generation`) before it executes, so a retry cannot silently
adopt a Machine that was deleted and recreated in the meantime.

### metalman

`metalman` owns host operations for bare-metal Machines. It talks to the BMC over Redfish, reads and changes power
state, and controls boot order for reimaging. The kinds it handles are `HostPowerOff`, `HostPowerOn`, `HostHardReboot`,
and `HostReplace`; a `HostReplace` either repaves over PXE or installs through virtual media, depending on the Machine's
netboot configuration. It does not handle `HostSoftReboot`, `NodeReboot`, or the agent kinds.

A `metalman` instance is scoped to one site with `--site`. An operation that targets a single Machine is owned by the
site that owns that Machine. An operation that uses `spec.machineSelector` must name exactly one site in the selector,
using the `unbounded-cloud.io/site` label. A selector that names no site or more than one site is rejected, because no
instance can prove it owns the target. This is a deliberate safety rule: a selector such as `role=gpu` is not enough to
address bare-metal power hardware across sites.

### unbounded-agent

`unbounded-agent` handles the four agent-side kinds: `NodeReboot`, `AgentUpgrade`, `AgentReset`, and `HostSoftReboot`.
The daemon watches `MachineOperation` resources and picks up an operation only when it targets its own Machine, either
through `spec.machineRef` or through a `spec.machineSelector` that matches. For selector matching, the agent checks the
Node's labels first and falls back to the Machine's labels when no Node exists yet, so a selector can be written against
either object during provisioning.

Because the agent runs on the host, it has the access these operations need: it can stop and start the node through the
isolation-mode-specific operator (including `machinectl` on `nspawn` hosts), replace its own binary through a
blue-green layout, and stop its own systemd service last during a reset. The trade-off is that agent operations require
a reachable API server and a running daemon. If the host or the daemon is down, use a host operation instead.

## Creating an Operation

You can create an operation either by applying a manifest or by using the `kubectl unbounded` plugin. Both paths create
the same object. Use a manifest when the operation belongs in version control or is generated by a pipeline, and use the
plugin when you want a generated name, confirmation prompts, or a blocking wait.

### With a Manifest

A single-machine operation names the Machine in `spec.machineRef`. The following manifest requests a hard reboot and
asks for the record to be deleted five minutes after it finishes:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: reboot-worker-01
spec:
  machineRef: worker-01
  operationKind: HostHardReboot
  ttlSecondsAfterFinished: 300
```

Apply it with `kubectl apply -f reboot-worker-01.yaml`. A selector-based operation replaces `machineRef` with
`machineSelector` and must not set both:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: reboot-gpu-workers
spec:
  machineSelector:
    matchLabels:
      role: gpu
      unbounded-cloud.io/site: rack-a
  operationKind: HostHardReboot
```

The manifest fields are the API fields: `spec.machineRef`, `spec.machineSelector`, `spec.operationKind`,
`spec.parameters` (a string map passed to the executor), and `spec.ttlSecondsAfterFinished`. `spec.operationKind` is
required. Exactly one of `machineRef` or `machineSelector` must identify the target. Parameters are operation-specific;
for example, `AgentUpgrade` requires a `downloadURL` parameter and accepts an optional `sha256` digest.

### With the CLI

The plugin exposes an explicit create command plus one convenience command per common operation. The explicit command
gives you full control over the name, targeting, parameters, and cleanup:

```bash
kubectl unbounded machine operation create reboot-worker-01 \
  --kind HostHardReboot \
  --machine worker-01 \
  --ttl 300 \
  --wait
```

The flags are:

| Flag | Purpose |
| --- | --- |
| `--kind` | The operation kind. Required; create rejects an empty or unsupported kind. |
| `--machine` | Targets one Machine by name. |
| `--selector`, `-l` | Targets Machines by label selector. Exactly one of `--machine` or `--selector` is required. |
| `--param` | A `key=value` parameter for the executor. Repeatable. |
| `--ttl` | Seconds after the terminal phase before cleanup. Omitting it, or passing `0`, leaves the field unset and keeps the record. |
| `--wait` | Streams status until the operation reaches `Complete` or `Failed`. |
| `--timeout` | Bounds `--wait`. `0` waits indefinitely. |
| `--output`, `-o` | `name`, `yaml`, or `json`. |
| `--dry-run` | `none`, `client`, or `server`. `client` prints the object without contacting the API server. |
| `--field-manager` | The managed fields owner recorded on create. |
| `--kubeconfig` | Path to a kubeconfig file. |

`--kind` accepts the eight day-2 kinds listed earlier. `HostAllocate` and `HostRelease` are not accepted, because they
are created by the allocation controller rather than by an operator.

`--wait` cannot be combined with `--dry-run`, and it cannot be combined with `-o yaml` or `-o json`, because the
progress stream is meant for a terminal. A selector-based create looks like this:

```bash
kubectl unbounded machine operation create reboot-gpu-workers \
  --kind NodeReboot \
  --selector 'role=gpu' \
  --wait --timeout 10m
```

Selector-based operations are only executed by `metalman` and by the agent. The external provider controller ignores
them, so a selector that matches cloud VMs will not power them off. To act on cloud VMs, create one operation per
Machine with `machineRef`, or generate the operations in a loop.

The convenience commands omit the `operation` subcommand, generate a name of the form
`<machine>-<action>-<timestamp>`, set `--ttl 300` by default, and wait for completion by default. Each takes a Machine
name as its only positional argument:

| Command | Kind |
| --- | --- |
| `kubectl unbounded machine node-reboot NAME` | `NodeReboot` |
| `kubectl unbounded machine host-soft-reboot NAME` | `HostSoftReboot` |
| `kubectl unbounded machine host-hard-reboot NAME` | `HostHardReboot` |
| `kubectl unbounded machine power-off NAME` | `HostPowerOff` |
| `kubectl unbounded machine power-on NAME` | `HostPowerOn` |
| `kubectl unbounded machine replace NAME` | `HostReplace` |
| `kubectl unbounded machine agent-upgrade NAME` | `AgentUpgrade` |
| `kubectl unbounded machine agent-reset NAME` | `AgentReset` |

All of them accept `--operation-name`, `--ttl`, `--wait`, `--timeout`, and `--kubeconfig`. `agent-upgrade` additionally
requires `--download-url` and accepts `--sha256`. `agent-reset` and `replace` ask for confirmation unless you pass
`--force`. Both prompts require you to type the Machine name, and both refuse in a non-interactive session without
`--force`. These commands attach an owner reference to the target Machine, so Kubernetes garbage collection removes the
operation when the Machine is deleted.

```bash
kubectl unbounded machine agent-upgrade worker-01 \
  --download-url https://example.com/unbounded-agent-linux-amd64.tar.gz \
  --sha256 <sha256-digest>
```

Replace `<sha256-digest>` with the digest published for the release. Omitting `--sha256` skips digest verification, so
supply it whenever you have a published digest to compare against.

### Safety and Ordering

Operations are asynchronous, so assume that anything you create can overlap with work someone else started. The
controllers reduce that risk in specific ways, and you should design around them.

Host operations on the same Machine are serialized by the executor that owns them: a host operation that finds an
older, unfinished host operation on the same Machine waits for it and reports
`waiting for older host operation <name>`. `machine-ops-controller` keys this check on the Machine, and `metalman`
also detects overlap between selector-based operations that resolve to the same hardware. The older operation wins
regardless of which one you consider more urgent, so create host operations in the order you want them to run, and
wait for the current one to finish before you plan the next.

`HostSoftReboot` is the exception to the ordering rule. Before the agent issues `systemctl reboot`, it checks for any
unfinished out-of-band host operation on the same Machine, including selector-based operations, and waits if one
exists. It rechecks every ten seconds and reports `waiting for out-of-band host operation <name>`. In the other
direction, out-of-band host operations do not wait for a soft reboot. If a provider is about to power the host off,
the soft reboot yields rather than the provider operation.

`HostReplace` is destructive. For a cloud VM it deletes and recreates the host with fresh bootstrap data; for a
bare-metal host it reimages the machine. Local OS disk state is destroyed, and the node rejoins only after the agent is
installed again. Confirm that the Machine's configuration, credentials, and provider references are current before you
replace it. The replacement resolves the host image and provisioning format when the operation starts, then reuses that
frozen input for retries.

{{< callout type="warning" >}}
`HostReplace` and `AgentReset` both remove state. `HostReplace` destroys the host's OS disk. `AgentReset` removes the
agent and the resources it manages, and the host stops being managed until you install an agent again.
{{< /callout >}}

`AgentUpgrade` has a two-step effect. The agent downloads and verifies the release, stages it in the inactive slot,
switches the current link atomically, and then restarts its own daemon. The operation remains `InProgress` across the
restart, and the daemon that starts from the new binary reports `Complete`. If the new daemon cannot start, recovery
reports the operation as failed with the daemon's failure message. Supply the `sha256` digest when you control the
release, and keep a known-good binary path in mind: the agent protects the last-good slot before replacing the inactive
one.

`NodeReboot` rejects a start that the host is not allowed to make. Before restarting, the agent verifies the allocation
launch for allocation-backed Machines and fails the operation with reason `AllocationLaunchRefused` when the Machine is
fenced, is being deleted, does not belong to the allocation, or the check cannot be confirmed. A refusal leaves the
running node untouched, which is the intended outcome: the operation reports a problem instead of forcing a restart
that policy forbids.

## Checking Status

Every operation reports one phase for the operation as a whole and, for multi-target operations, one phase per target.
The phases are:

| Phase | Meaning |
| --- | --- |
| `Pending` | The operation exists and has not started, or it is waiting for an earlier operation to finish. |
| `InProgress` | The owning component has begun the action. For long actions, progress messages change while this stays the same. |
| `Complete` | The action finished successfully. This is terminal. |
| `Failed` | The action could not be completed. This is terminal and the message explains why. |

A newly created operation has no phase until a controller or agent picks it up. `kubectl get` shows the print columns
`Machine`, `Operation`, `Phase`, and `Age`, with `Operation` holding `spec.operationKind`.

List operations across the cluster with the resource name, the plural, or the short name `mop`:

```bash
kubectl get machineoperations
kubectl get mop
kubectl get mop reboot-worker-01 -o yaml
```

For a live view, watch one object. The phase changes in place as the owner works:

```bash
kubectl get mop reboot-worker-01 -w
```

`kubectl describe mop reboot-worker-01` is the most useful single view. It shows the spec, the phase and message, the
start and completion timestamps, the Machine generation the executor observed, conditions, and the per-target entries.
The `ObservedMachineGeneration` field is worth checking after an operation finishes: if someone edited the Machine
while the operation ran, the operation acted on the generation recorded here, not on the latest spec.

You can also list Machines and operations together to correlate a host with its recent work:

```bash
kubectl get machines,machineoperations
```

Multi-target operations are visible in `status.targets`. Each entry carries `machineRef`, `phase`, `stage`, `message`,
timestamps, `observedGeneration`, the attempt count, and, when the provider returned one, a resumable
`providerOperation` handle. The `stage` field is the most granular progress signal: host operations move through
`PoweringOff`, `WaitingOff`, `PoweringOn`, `WaitingOn`, `RepaveRequested`, `WaitingRepave`, `WaitingCloudInit`, and
`WaitingNode`, and `WaitingProviderOperation` covers an external operation that is still being polled. A target stays
`InProgress` while its stage advances, so use the stage rather than the phase to tell the difference between a stalled
operation and a slow one.

If you do not want to poll, ask the plugin to block:

```bash
kubectl unbounded machine operation wait reboot-worker-01 --timeout 10m
```

The wait returns successfully on `Complete` and returns an error on `Failed`, printing the phase changes and
per-target transitions as they happen. The convenience commands wait by default, so `--wait=false` is the way to get
them to return immediately.

A `Failed` phase is final. The owner does not retry it, and the operation remains a record of what went wrong. Read
`status.message`, the `Completed` condition, and the target messages, fix the cause, and create a new operation. Common
reasons include a Machine that is not a valid target for the kind, a selector that names no site or more than one site
for `metalman`, an unsupported kind for the provider, a missing provider reference, and an agent refusal such as
`AllocationLaunchRefused`.

## Cleanup

Operations are records, not daemons, but they still accumulate. Set `spec.ttlSecondsAfterFinished` when you want the
object to disappear automatically. The value is a whole number of seconds counted from `status.completedAt`, which is
set when the operation reaches `Complete` or `Failed`. Leaving the field unset keeps the object indefinitely.

A literal zero is not the same as leaving the field unset. Zero is a valid value, and it puts the deadline at the
completion time, so the owning controller can delete the record as soon as it observes the terminal phase. The plugin's
`--ttl 0` does not write a zero: it leaves the field unset, which is why the command's help says that zero keeps the
record. If you write manifests by hand, omit `ttlSecondsAfterFinished` to keep an operation.

The TTL is enforced by the controller that owns the operation: `machine-ops-controller` for cloud provider hosts and
`metalman` for bare-metal hosts, each of which schedules a requeue at the deadline and deletes the object when it
expires. The agent records the terminal phase but does not run a cleanup loop, so an agent-handled operation stays in
the cluster until you delete it, or until the Machine that owns it is deleted when the operation has an owner
reference. This is convenient when you want upgrade and reset history, and noisy when you do not.

The convenience commands default to `--ttl 300` and attach an owner reference to the Machine. That combination keeps
the cluster tidy twice: the operation is removed five minutes after it finishes, and if it is still around when the
Machine is deleted, Kubernetes garbage collection removes it with the Machine. The explicit
`machine operation create` command does not attach an owner reference, so set `--ttl` if you create operations in a
pipeline. `HostAllocate` and `HostRelease` reject `ttlSecondsAfterFinished` entirely, and the API server enforces that
at admission.

## Next Steps

Each of the following pages covers one scope in depth, including the manifests, flags, and failure modes specific to
that scope:

1. [Host operations]({{< relref "guides/operations/host-operations" >}}) covers power management and host replacement
   through providers and BMCs.
2. [Node operations]({{< relref "guides/operations/node-operations" >}}) covers restarting the node stack and the
   reasoning behind the supported node actions.
3. [Agent operations]({{< relref "guides/operations/agent-operations" >}}) covers agent upgrades, resets, and the
   blue-green handoff that follows an upgrade.
4. [Automation patterns]({{< relref "guides/operations/automation" >}}) covers generating operations from selectors,
   running them in pipelines, and troubleshooting stalled work.
