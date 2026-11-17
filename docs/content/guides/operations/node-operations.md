---
title: "Node Operations"
weight: 2
description: "Restart, reset, and upgrade the Kubernetes node stack on an Unbounded host without rebooting the host itself."
---

## Overview

A node operation acts on the Kubernetes node stack that runs on an Unbounded host. The stack contains the kubelet,
containerd, the CNI configuration, and the pods scheduled to the node. In the default `nspawn` isolation mode that
stack runs inside a `systemd-nspawn` machine. In `none` isolation it runs directly in host units, and in the
experimental `container` mode it runs in a privileged container. Node operations stop and restart that stack. They do
not reboot the host, do not power it off, and do not replace its operating system, so the host kernel and the agent
installation stay as they were.

Unbounded offers two ways to operate on a node, and they solve different problems. A `NodeReboot` is a discrete
`MachineOperation` that restarts the node stack in place and keeps its root filesystem. Node recreation is a
reconciliation workflow that builds a fresh node root filesystem from the desired configuration; you request it by
deleting the Kubernetes `Node` object, not by creating an operation. The first is a repair or a runtime refresh. The
second is how a node changes version or rebuilds its software.

| Operation | Request | Executor | Root filesystem |
| --- | --- | --- | --- |
| `NodeReboot` | `MachineOperation` with `operationKind: NodeReboot` | in-host `unbounded-agent` | Kept |
| Node recreation | Delete the Kubernetes `Node` object | in-host `unbounded-agent` | Rebuilt from desired configuration |
| `AgentUpgrade` | `AgentUpgrade` operation | in-host `unbounded-agent` | Kept |
| `AgentReset` | `AgentReset` operation | in-host `unbounded-agent` | Removed with the managed node stacks |

Both paths depend on the agent being healthy and watching the cluster. The agent is the only component that executes
node work: `machine-ops-controller` and `metalman` handle host power and replacement through providers and BMCs, and
they ignore node kinds. If the agent is not running, a `NodeReboot` waits and a deleted `Node` is not rebuilt. The
agent also watches a single Node name, taken from its applied configuration, so the name you delete must be the name
that agent manages.

The following sections describe each path in the order you use it: how to request it, what the agent does on the
host, how to observe progress, and how to recover when it does not finish. They assume you already have a Machine that
is provisioned and whose Node is registered. If you are still creating machines, start with
[Getting Started]({{< relref "guides/getting-started" >}}).

## NodeReboot

`NodeReboot` restarts the node stack in place. It stops the node services, restarts the node stack, and brings the
services back up without reprovisioning. The root filesystem is preserved, the `Machine` object is untouched, and the
Kubernetes `Node` object is kept. When the kubelet comes back it updates the existing Node rather than registering a
new one.

This makes `NodeReboot` the smallest disruptive action available for a node. It is appropriate when the node software
is intact but a process is wedged, when you have changed node-level configuration that needs a restart to take
effect, or when you need to re-run the node lifecycle hooks. It is not an upgrade: it starts the same installed
binaries from the same root filesystem.

### When to Use NodeReboot

Use `NodeReboot` when the kubelet or containerd is stuck on one node and a restart is likely to clear it, when a
runtime or kubelet configuration change needs the stack to restart, or when the node is healthy enough to run the
agent but not healthy enough to run workloads. It also works as a repair step after an accidental Node deletion, which
is covered in [Troubleshooting Stuck Operations](#troubleshooting-stuck-operations).

Do not use it to move to a new Kubernetes version, to change the agent image, or to repair a corrupted root
filesystem. Those changes need a new root filesystem and are handled by node recreation. Do not use it when the host
kernel or host operating system needs to change; that is a host operation. And do not use it as a substitute for
draining workloads that cannot tolerate a restart.

### Requesting a NodeReboot

The shortest form is the convenience command, which creates a `MachineOperation`, waits for it by default, and
records a 300 second cleanup delay on the operation:

```bash
kubectl unbounded machine node-reboot worker-01
```

The positional argument is the `Machine` name, not the Node name. The command generates a name of the form
`worker-01-node-reboot-<timestamp>`. It accepts `--operation-name` to choose the name yourself, `--ttl` to change the
cleanup delay (`0` keeps the record), `--wait=false` to return immediately after creation, `--timeout` to bound the
wait, and `--kubeconfig` for a non-default cluster.

The same request as a manifest is useful in GitOps and change records:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: worker-01-node-reboot
spec:
  machineRef: worker-01
  operationKind: NodeReboot
  ttlSecondsAfterFinished: 300
```

Apply it and watch the operation:

```bash
kubectl apply -f worker-01-node-reboot.yaml
kubectl get mop worker-01-node-reboot -w
```

`MachineOperation` is cluster-scoped and has the short name `mop`. The print columns are the target machine, the
operation kind, the phase, and the age. The full status carries the message, timestamps, the machine generation the
agent observed, and conditions.

To restart several nodes with one object, select machines by label instead of naming one:

```bash
kubectl unbounded machine operation create reboot-gpu-workers \
  --kind NodeReboot \
  --selector 'role=gpu' \
  --wait --timeout 10m
```

Selector matching for node kinds is done by each agent against its local Node labels. Before the Node has joined, the
agent falls back to the labels on the local `Machine`. A selector that matches no Node or Machine leaves the
operation untouched, so it stays without a terminal phase. Prefer `machineRef` when you need a guaranteed single
target, and check the selector with `kubectl get nodes --show-labels` before you use it for a fleet action.

### What the Agent Does

The agent daemon reconciles its own `MachineOperation` resources through a controller that serializes host-local work.
It claims an operation only while the phase is empty or `Pending`, and it handles only one request at a time. That
serialization is deliberate: restarts, repaves, resets, and agent upgrades all mutate the same systemd units,
machines, and files.

When the agent picks up a `NodeReboot`, it takes the installation mutation lock, records `InProgress`, resolves the
local `Machine`, and finds the active node slot from the applied configuration. If another installation owns the
host, the operation waits and is retried every two seconds; it has not started yet, so the phase stays as it was.

For a Machine that is backed by a `MachineAllocation`, the agent verifies the allocation before doing anything
disruptive. If the Machine is fenced, is being deleted, does not belong to the allocation, or the check cannot be
confirmed, the operation fails with reason `AllocationLaunchRefused` and the running node is left alone. This is a
policy refusal, not an execution failure, and it should not be retried blindly.

The restart itself depends on the isolation mode:

| Mode | What `NodeReboot` restarts | Host rebooted | Pods |
| --- | --- | --- | --- |
| `nspawn` (default) | The `systemd-nspawn@<slot>.service` unit, including its pre-start configuration and post-start hooks | No | Containers stop with the machine and start again with it |
| `none` | LocalDNS, containerd, and kubelet, in start order | No | Containers are not stopped by the service restarts |
| `container` (experimental) | The container unit, which re-runs the lifecycle hooks | No | Containers stop with the container and start again with it |

After the restart, the agent waits for the node stack to become healthy. In `nspawn` and `container` modes it also
waits for LocalDNS. The kubelet check polls `systemctl is-active kubelet` every two seconds with a 60 second timeout.
When that check passes, the agent marks the operation `Complete` with reason `Succeeded` and the message
`NodeReboot completed`, and records the Machine generation it acted on.

Nothing in this path cordons, drains, evicts, or reschedules pods. In `nspawn` and `container` modes the pods on the
node are interrupted by the restart; in `none` mode their containers keep running while the control services restart.
If your workload cannot tolerate that interruption, cordon and drain the node before you create the operation and
uncordon it afterward.

### Observing a NodeReboot

The phase and the message are the first things to read. A node operation normally appears with no phase, moves to
`InProgress` when the agent claims it, and ends in `Complete` or `Failed`. The API also defines `Pending` for queued
work, but a node operation that cannot start yet keeps the phase it had, which is usually empty. The terminal phases
are final: the agent does not execute a completed or failed operation again.

```bash
kubectl get mop worker-01-node-reboot
kubectl describe mop worker-01-node-reboot
```

The `Completed` condition mirrors the phase. While the operation runs it is `False` with reason `InProgress`. On
success it is `True` with reason `Succeeded`. On failure it is `False` with the failure reason, such as
`ExecutionFailed` or `AllocationLaunchRefused`. `status.startedAt` and `status.completedAt` bound the execution, and
`status.observedMachineGeneration` tells you which version of the `Machine` spec the agent acted on.

The `Node` object is not replaced. You can watch its status and the pods on it:

```bash
kubectl get node worker-01 -w
kubectl get pods -A --field-selector spec.nodeName=worker-01 -w
```

For host-side detail, read the daemon journal on the machine. The unit is `unbounded-agent-daemon.service`:

```bash
sudo journalctl -u unbounded-agent-daemon.service -f
```

From inside the node stack, the agent's `logs` subcommand streams the kubelet and containerd journals for the active
mode, which is useful when the restart completed but workloads did not return:

```bash
sudo unbounded-agent logs --unit kubelet --lines 200
sudo unbounded-agent logs --unit containerd --follow
```

### What NodeReboot Preserves

`NodeReboot` preserves the node root filesystem and everything installed in it. In `nspawn` mode that means the
machine under `/var/lib/machines/<slot>` keeps its container image contents, its installed Kubernetes version, its
containerd state directory, and its kubelet credentials. The host operating system, the agent binary, the host network
configuration, and the Machine and Node objects are untouched. Because the kubelet starts against the same
configuration and credentials, it updates the existing Node rather than joining as a new one.

The operation does not preserve running pod containers in `nspawn` and `container` modes. Those containers are inside
the machine or container that restarts, so they stop and are started again by the kubelet when it comes back. A pod
whose container exits during that window may be restarted with backoff. Pods that are managed by a controller are
rescheduled normally; pods that were only running on this node and have no controller remain gone.

`NodeReboot` also does not clean up host networking or CNI state. It restarts the stack in place, and the lifecycle
hooks regenerate the host-side configuration for the same slot. If a previous failure left stale interfaces or
routes, a reboot may not remove them; node recreation is the path that runs host network cleanup between the old and
new stack.

## Node Recreation

Node recreation replaces the node stack and its root filesystem. It does not replace the host, does not delete the
`Machine` object, and does not change the host kernel. What changes is the software inside the node boundary: the
container image contents, the installed Kubernetes version, the kubelet configuration, the CNI configuration and
plugins, the agent-managed runtime versions, and everything else that lives in the node root filesystem.

Despite the name, you do not ask for recreation with a `MachineOperation`. The API intentionally has no
`NodeRecreate`, `NodeReimage`, or `NodeUpgrade` kind. Recreation is a reconciliation workflow: you delete the
Kubernetes `Node` object, and the agent on that host notices that its Node is gone, resolves the desired
configuration from the `Machine`, and builds a new node stack. The operation is requested by a delete and observed
through the Node and Machine objects rather than through an operation phase.

### When to Use Node Recreation

Use node recreation when the desired configuration changes in a way that needs a new root filesystem. The most common
case is a Kubernetes version upgrade: the new version has to be installed and the kubelet restarted from the new
version directory, which a `NodeReboot` does not do. Other cases include a new agent OCI image for the node stack,
changes to node labels or taints that are applied at registration, changes to LocalDNS or kubelet settings, and
recovery from a corrupted node root filesystem.

Recreation is also how a declared isolation mode change takes effect. Setting `spec.isolation` on the `Machine`
records intent, and the next repave applies it. The agent never changes the mode in the middle of a running node. For
details on the modes themselves, see [Isolation Modes]({{< relref "reference/agent/isolation" >}}) and the
`nspawn` reference at [nspawn isolation]({{< relref "reference/agent/nspawn" >}}).

Do not use node recreation to update the host kernel or the host operating system. The new node stack runs on the same
host, so it still uses the same kernel and the same host packages. Use `HostSoftReboot` for an in-place host OS
restart, or `HostReplace` to reimage or recreate the host. The
[Host Operations]({{< relref "guides/operations/host-operations" >}}) guide covers those paths.

### Cordon and Drain

The agent does not cordon, drain, or evict pods. Its repave contract says that the caller has already removed the Node
from service. If you skip that step, the kubelet and the node stack can disappear while pods are still bound to the
Node, and Kubernetes has to clean up the consequences.

Start by marking the node unschedulable so the scheduler stops placing new pods on it:

```bash
kubectl cordon worker-01
```

Then evict the workloads. `kubectl drain` respects pod disruption budgets, so a drain can block if a budget does not
allow another eviction. That is a signal to wait, not to force the drain:

```bash
kubectl drain worker-01 \
  --ignore-daemonsets \
  --delete-emptydir-data \
  --timeout=10m
```

`--ignore-daemonsets` is required on most clusters because DaemonSet pods cannot be evicted. They are terminated when
the Node is deleted and recreated on the replacement. `--delete-emptydir-data` allows the drain to remove pods that
use `emptyDir` volumes, which destroys that temporary data. Pods with persistent volume claims are evicted and
rescheduled, subject to their volume topology. Pods with local storage that is not backed by a claim need your
judgment before you continue.

Wait until the workload pods are gone and the important state has moved:

```bash
kubectl get pods -A --field-selector spec.nodeName=worker-01
```

If the drain fails because a pod has no controller, you can delete that pod explicitly after you confirm it is safe.
Do not use `--force` on the drain as a shortcut; a forced drain bypasses the eviction path and can leave workloads
running on a Node that is about to disappear.

### Deleting the Node

Once the node is drained, delete the Kubernetes `Node` object:

```bash
kubectl delete node worker-01
```

Delete the Node, not the Machine. The `Machine` is the desired-state object that tells the agent what to build; the
`Node` is the Kubernetes registration that the agent watches. Deleting the Machine removes the target the agent needs
to resolve configuration and would not trigger a repave. Deleting the Node removes only the registration, which is
the trigger.

The agent watches the Node name recorded in its applied configuration, using a field selector for that single name.
A Node whose name does not match is ignored. The trigger is the delete event observed by the running daemon, so keep
the daemon running through the deletion. If the daemon is stopped when you delete the Node, it does not see the event
when it starts again.

After the delete, the Node object is gone until the new stack registers. That is expected. The pods that remain bound
to the old Node are cleaned up by Kubernetes, and the replacement Node starts empty apart from DaemonSets and any
controllers that reschedule onto it.

### The Repave Workflow

On the delete event, the agent's repave reconciler takes the installation mutation lock and resolves the desired
configuration. It reads `spec.configurationRef` on the `Machine`, resolves the referenced
`MachineConfigurationVersion`, and merges that template with the settings the agent owns, such as the node name, the
download sources, and the declared isolation mode. It then compares the result with the configuration recorded as
applied. If nothing that requires a repave differs, it skips the repave, which is discussed below.

When the desired configuration differs, the agent repaves according to the mode:

| Mode | Repave sequence |
| --- | --- |
| `nspawn` | Provision the new rootfs in the alternate slot (`kube1` or `kube2`), stop the old machine gracefully, clean host networking, start the new machine, persist the applied configuration, wait for kubelet, then remove the old machine and its applied configuration |
| `none` | Install the new version beside the running one, stop the units and end the pods, clean host networking, flip the `current` link, start the new units, wait for kubelet, and remove the superseded version last; a failed start flips back to the previous version |
| `container` | Build the new slot beside the running one, stop and disable the old container, clean host networking, start the new container, wait for kubelet, then remove the old slot; a failed start removes the new slot and starts the old container again |

The `nspawn` and `container` modes use two slots, so the new stack is built while the old one still exists. The old
slot is stopped only after the new root filesystem is ready. In `nspawn` mode the stop pre-stops kubelet and
containerd inside the machine before stopping the machine itself, which keeps the shutdown quick and avoids leaving
services to be force-killed. The old machine and its root filesystem are removed only after the new kubelet is
active, so a failed repave leaves the previous files in place for inspection and retry.

The `none` mode runs a single host-level stack, so it cannot switch to a second running stack. Its upgrade is an
in-place switch with a journal and automatic rollback. It records the version being replaced before anything changes,
and if the new version does not come up it starts the previous version with its own configuration and networking. A
repave that changes only configuration keeps the same version directory, so the version name alone does not tell you
whether a repave happened.

Network cleanup between the old and new stack removes the agent's own host networking: LocalDNS rules, WireGuard and
tunnel or overlay interfaces, and the associated routes. It does not remove CNI-specific dataplane state, which is
covered under [What the Agent Does Not Do](#what-the-agent-does-not-do).

### Observing a Repave

There is no operation phase to watch, so use the Node, the Machine, and the daemon journal together. The Node leaves
and returns:

```bash
kubectl get node worker-01 -w
```

The Machine reports which configuration version the agent has applied and whether the desired version is still
pending:

```bash
kubectl get machine worker-01 -o yaml
kubectl get machine worker-01 -w
```

`status.configuration` records the `MachineConfiguration` name, version number, and version object name that the agent
last applied. The `RepavePending` condition summarizes the comparison. When the applied configuration matches the
version pinned in `spec.configurationRef`, the agent sets it to `False` with reason `Applied`. When they differ, it
sets it to `True` with reason `Pending`. The agent writes the condition from its applied state at the end of a
repave, so read `status.configuration` directly if the condition looks stale while you are preparing a change.

On the host, the daemon journal is the detailed record. Look for the repave start, the old and new slot names, and
the version transition:

```bash
sudo journalctl -u unbounded-agent-daemon.service -f
```

For `nspawn`, the running node stack is also visible through `machinectl`:

```bash
sudo machinectl list
```

The new machine should appear under the alternate slot name, and the old one should disappear after the repave
completes. Both slots never run at the same time, because the repave serializes them.

### What the Agent Does Not Do

The agent does not evict pods. Draining is your responsibility, and the repave assumes it has happened. The agent also
does not perform CNI-specific dataplane cleanup. It cleans the networking that it created and owns, then starts the
new node stack and lets the CNI initialize from the new root filesystem.

That distinction matters for eBPF CNIs that keep host-level state outside the node root filesystem. Cilium, for
example, can leave host routes, links, socket load-balancing programs, or map entries behind after its agent stops.
If you run Cilium and you recreate a node, make sure its per-node state is handled according to the CNI's own
guidance. The [Networking Operations]({{< relref "reference/networking/operations" >}}) reference describes Cilium
migration and cleanup steps for the networking layer.

### Why Not a MachineOperation?

Recreation is a reconciliation workflow, not a discrete action. The agent has to observe that the desired state and
the running state differ, resolve the desired configuration, build a new stack, and let the kubelet join again. Every
step depends on state that the agent discovers on the host and in the cluster. Modeling that as a one-shot operation
would duplicate the reconciliation path and create two ways to ask for the same rebuild, with different failure and
retry behavior.

For that reason the API rejects `NodeReimage`, `NodeUpgrade`, and `NodeRecreate` as operation kinds. The documented
replacement is the Node delete. A `NodeReboot` remains an operation because it is a bounded action with a clear
start, a known end, and a result that can be reported in a phase. Recreation is a state transition whose success is
observed through the Node and Machine, and whose retries are driven by the reconciler rather than by a new operation
object.

## Node Upgrades

A Kubernetes version upgrade is a node recreation, not a reboot. The new version has to be installed in a new node
root filesystem, and the kubelet has to start from that version. `NodeReboot` restarts whatever is already installed,
so using it for an upgrade does nothing except restart the old version. The same rule applies to a new node OCI image,
a new CNI plugin set, and changes to kubelet configuration that the agent derives from the desired configuration.

A typical version upgrade has five steps. First, update the `MachineConfiguration` so it describes the version you
want, and confirm that a `MachineConfigurationVersion` exists for it:

```bash
kubectl unbounded machine config versions my-config
```

Second, make sure each affected `Machine` references the version you intend. `spec.configurationRef.version` pins a
specific version; when it is omitted, the controller selects the latest locked version at the time it resolves the
reference. Pinning is how you make the upgrade deliberate rather than implicit:

```bash
kubectl unbounded machine config assign worker-01 --config=my-config --version=<version>
```

Use the version number reported by the `versions` command. The placeholder is a whole number, not a version string.

Third, cordon and drain the node. Fourth, delete the `Node`. Fifth, watch the Node return and verify its version and
health. Repeat one node at a time so the cluster keeps enough capacity and so a failed upgrade does not take the whole
fleet down.

The configuration fields that trigger a repave are the ones that change the node stack. They include the Kubernetes
version, the agent OCI image, node labels and taints applied at registration, LocalDNS settings, and the kubelet and
credential settings derived from the configuration. Changes to host image fields are not applied by a node repave;
they belong to host replacement. The repave comparison is field-based, so a change that does not reach the resolved
agent configuration does not cause the node to rebuild.

Rollback uses the same path. Pin the `Machine` back to the previous version, then cordon, drain, and delete the Node
again. Older `MachineConfigurationVersion` objects are retained according to `spec.revisionHistoryLimit`, and a
version that is still referenced by a Machine is not deleted even when the limit would otherwise remove it. Confirm
that the version you want still exists before you rely on a rollback.

Agent upgrades are a separate operation. `AgentUpgrade` replaces the host-side agent binary in a blue-green slot and
restarts the daemon; it does not rebuild the node root filesystem, and it is not a node recreation. Use the
[Agent Operations]({{< relref "guides/operations/agent-operations" >}}) guide for the binary upgrade and reset
workflows. A node recreation picks up a new agent OCI image for the node stack, which is a different artifact from the
host-side agent binary.

## Choosing the Right Approach

The decision follows the layer that is broken or changing. Use the innermost layer that can solve the problem, because
each outer layer has a larger blast radius.

| Symptom or goal | Use | Why |
| --- | --- | --- |
| Kubelet or containerd is stuck on one node | `NodeReboot` | Restarts the stack in place and keeps the root filesystem |
| A running service needs a restart | `NodeReboot` | The installed software and configuration stay the same |
| A Node was deleted and needs to re-register | `NodeReboot` | Restarting the stack makes the kubelet register the Node again |
| New Kubernetes version | Node recreation | The new version must be installed in a new root filesystem |
| New node image, labels, taints, or LocalDNS | Node recreation | These are part of the resolved node configuration |
| Corrupted node root filesystem | Node recreation | A new root filesystem is built from desired state |
| Host kernel or host OS changes | `HostSoftReboot` or `HostReplace` | The host operating system boundary is involved |
| Host is unresponsive | `HostHardReboot` | The power cycle must come from outside the guest |
| Replace the host or reimage it | `HostReplace` | The host itself is recreated or rewritten |
| Remove the agent and its resources | `AgentReset` | The agent boundary is being torn down, not the node |

Two mistakes are common. The first is using `NodeReboot` for an upgrade, which restarts the old version and leaves
the desired version pending. The second is deleting a Node without changing the desired configuration, which the
agent interprets as no drift and skips. If the goal is to rebuild a node that is already at the desired version, you
need a reason for the repave to run, such as a configuration change, or you need to repair the node with a
`NodeReboot` instead. Rebuilding from an unchanged configuration is not a supported way to force a clean install.

`HostSoftReboot`, `HostHardReboot`, `HostPowerOff`, `HostPowerOn`, and `HostReplace` are documented in
[Host Operations]({{< relref "guides/operations/host-operations" >}}). They act on the host boundary and do not
require the node to be healthy, which is why they are the fallback when the agent cannot run at all.

## Safety Practices

Treat node operations as production changes even when the operation is a restart. The following practices apply to
both paths.

Confirm the Node name before you act. The agent watches the Node name recorded in its applied configuration, which is
an explicit override if one was set, otherwise the host hostname, otherwise the Machine name. These are often the same
string, but not always. List the nodes and the machines before you delete anything:

```bash
kubectl get nodes
kubectl get machines
```

Drain before you disrupt. `NodeReboot` does not drain, and node recreation assumes you already did. A drain that
blocks on a pod disruption budget is telling you that the workload cannot absorb another eviction. Wait for the
budget, add capacity, or reschedule the workload deliberately. Do not bypass the budget with a forced drain.

Check that the cluster can absorb the loss. A node recreation removes one node from service for the length of a
download, a root filesystem build, and a kubelet start. If the remaining nodes cannot run the evicted pods, the drain
will not complete or the pods will stay pending. For a fleet rollout, do one node at a time and verify capacity
between steps.

Make the configuration change before you delete the Node. If you delete first and change the configuration later,
the agent sees no drift and skips the repave, and the Node stays absent. If you need to change a `MachineConfiguration`
version, pin it on the Machine and confirm that `status.configuration` still shows the old version, so you know the
repave has work to do.

Keep the daemon running. The repave trigger is a delete event delivered to the running agent, and the reboot executor
is the agent itself. If you stop the daemon, delete a Node, and then start the daemon again, the event is gone. For
recreation, keep the host up and the agent healthy until the new Node is `Ready`.

For allocation-backed Machines, check the fence before you delete the Node. A Machine can be fenced when its
allocation is being released or when it no longer belongs to the allocation. The agent refuses both `NodeReboot` and
the repave in that state, and a refused repave leaves the Node absent. Clear the allocation state first, then repair
the registration with `NodeReboot`.

Verify the result, not just the phase. A `NodeReboot` can report `Complete` while a workload is still crashing, and a
repave can finish while the CNI is still initializing. Check that the Node is `Ready`, that DaemonSets have pods on
it, and that the workloads you drained have been rescheduled. Uncordon the node when you are satisfied:

```bash
kubectl uncordon worker-01
kubectl get node worker-01
kubectl get pods -A --field-selector spec.nodeName=worker-01
```

Finally, keep the records useful. A `MachineOperation` is an audit record of one action, not a desired-state object.
Do not edit a terminal operation to try to run it again; create a new one with a new name. The agent records the
terminal phase but does not enforce `ttlSecondsAfterFinished` itself, so an agent-handled record remains until you
delete it or its owning Machine is deleted. The convenience commands attach an owner reference to the Machine, which
gives you garbage collection when the Machine goes away.

## Troubleshooting Stuck Operations

Most node operation problems fall into one of four states: the operation never starts, it starts and never finishes,
it fails with a message, or the Node never comes back. Read the same fields in every case: `status.phase`,
`status.message`, `status.conditions`, `status.startedAt`, `status.completedAt`, and
`status.observedMachineGeneration` on the operation, plus `status.configuration` and the conditions on the Machine.

| Signal | Where to look | What it usually means |
| --- | --- | --- |
| No phase, unchanged for minutes | `kubectl get mop -o yaml` | No executor has claimed the operation |
| `InProgress`, far longer than expected | `status.startedAt`, daemon journal | The executor is waiting or was interrupted |
| `Failed` | `status.message`, condition reason | The action could not complete; the message names the error |
| `Complete` but the Node is not `Ready` | Node status, kubelet logs | The action finished but the node did not become healthy |

### A NodeReboot That Never Starts

A `NodeReboot` is executed only by the agent for the targeted Machine. If the operation has no phase and does not
change, the agent has not claimed it. Check that the Machine exists, that the agent daemon is running on that host,
and that the operation's `machineRef` names the Machine the agent manages. For a selector operation, check that the
labels match the Node, or the Machine before the Node has joined.

```bash
kubectl get machine worker-01
kubectl get mop worker-01-node-reboot -o yaml
sudo systemctl status unbounded-agent-daemon.service
```

The daemon journal explains most cases. A message about deferring an operation means the daemon is stopping for a
host reboot, an agent restart, or an isolation reprovision; those operations stay unstarted and the next daemon picks
them up. With verbose logging enabled, a message about ignoring an operation with no local handler means the kind is
not executed by this agent.
If the daemon is not running, no node operation can start, and a deleted Node will not be rebuilt.

### A NodeReboot Stuck in InProgress

An `InProgress` node reboot is doing work. The kubelet check alone has a 60 second timeout, so a restart can stay
`InProgress` for a minute or more, and a slow machine restart adds to that. Give it a minute before you conclude that
it is stuck.

After that, look for an interruption. The agent defers new operations once it has issued a host reboot, staged an
agent restart, or handed the host to a reprovision, but an operation that was already running when the daemon stopped
can be left `InProgress` with nothing to finish it. When the daemon restarts, it claims operations only while the
phase is empty or `Pending`, so it does not resume an `InProgress` one. Delete the stuck operation and create a new
one with a new name:

```bash
kubectl delete mop worker-01-node-reboot
kubectl unbounded machine node-reboot worker-01 --operation-name worker-01-node-reboot-retry
```

Do not delete the Machine to clear an operation. That removes the desired state the agent needs and does not fix the
node.

### A NodeReboot That Failed

A failed operation is terminal. Read `status.message` first; it carries the executor's error. A typical failure is
`ExecutionFailed` with a message that the kubelet did not become active within 60 seconds. That means the restart
happened but the node stack did not come back healthy. Inspect the node stack journals before you retry:

```bash
sudo unbounded-agent logs --unit kubelet --lines 200
sudo unbounded-agent logs --unit containerd --lines 200
sudo journalctl -u unbounded-agent-daemon.service --since "10 minutes ago"
```

If the message says `AllocationLaunchRefused`, the node was left untouched by design. The Machine is fenced, is being
deleted, does not belong to its allocation, or the check could not be confirmed. Resolve the allocation state instead
of retrying; the same refusal will happen again.

After you fix the cause, create a new operation. A terminal operation is never re-executed, even if you edit its spec
back to a runnable state.

### A Repave That Never Begins

The most common reason a repave does not begin is that there is no drift. Recreating a node is not a forced reinstall.
If the desired configuration resolves to the same values that are already applied, the agent logs that the repave was
skipped and returns without touching the node. The Node stays deleted, because a running kubelet does not create a
deleted Node object by itself. Compare the applied configuration with the desired reference:

```bash
kubectl get machine worker-01 -o jsonpath='{.status.configuration}{"\n"}'
kubectl get machine worker-01 -o jsonpath='{.spec.configurationRef}{"\n"}'
```

If both show the same version, either change the configuration and pin the new version, or repair the registration
with a `NodeReboot`. If the desired configuration cannot be resolved, the agent logs that the repave is skipped until
it is available and retries every 30 seconds. Check that the referenced `MachineConfigurationVersion` exists and that
the `MachineConfiguration` is assigned.

A repave can also be suppressed by the allocation guard. On an allocation-backed Machine, a final refusal leaves the
running node alone and does not rebuild the deleted Node. Correct the allocation or fence state first, then use
`NodeReboot` to make the kubelet register the Node again.

If the daemon is not running, or the Node you deleted is not the name the agent watches, no repave is triggered at
all. Confirm the Node name from the agent's applied configuration, and check the daemon journal for the delete event
and the repave start.

### A Node That Never Comes Back

A repave has several long steps: downloading artifacts, provisioning a root filesystem, stopping the old stack,
starting the new one, and waiting for the kubelet. The Node is absent for the whole sequence, so an absent Node is not
by itself a failure. Watch the daemon journal and the machine list:

```bash
sudo journalctl -u unbounded-agent-daemon.service -f
sudo machinectl list
kubectl get nodes -w
```

If the repave failed, the journal names the failed phase and the error. In `nspawn` mode the old root filesystem is
removed only after the new kubelet is active, so a failure before that point leaves the old slot available. Fix the
cause and let the reconciler retry; a failed repave returns an error and is retried with backoff. If the reconciler
returned successfully without rebuilding, for example because there was no drift or an allocation refusal, no retry
will happen and you need a new trigger after you correct the state.

If the new stack is running but the Node is not registered, the kubelet is the place to look. A kubelet that cannot
reach the API server, cannot read its credentials, or fails its configuration check will not create the Node. The
node stack journals and the daemon journal usually show the reason. If the root filesystem itself is damaged, a
`NodeReboot` cannot repair files that are missing or corrupt. Recovery needs a repave that has a configuration change
to apply, or host replacement if the host itself is at fault.

### Cleaning Up Operation Records

Node operations are records, and records accumulate. A convenience command sets `ttlSecondsAfterFinished` to 300 and
attaches an owner reference to the Machine, but the agent does not run a TTL cleanup loop. Delete finished operations
yourself when you no longer need the history, or let Kubernetes garbage collection remove them when the Machine is
deleted:

```bash
kubectl get mop
kubectl delete mop worker-01-node-reboot
```

Do not delete an operation that is still `InProgress` unless you have decided to abandon the action. Deleting the
record does not stop the executor; it only removes the status you would use to diagnose the result.

## See Also

- [Machine Operations]({{< relref "guides/operations" >}}) for the `MachineOperation` API, targeting, phases, and
  status fields shared by every kind.
- [Host Operations]({{< relref "guides/operations/host-operations" >}}) for power, reboot, and replacement at the
  host boundary.
- [Agent Operations]({{< relref "guides/operations/agent-operations" >}}) for `AgentUpgrade` and `AgentReset`.
- [Automation Patterns]({{< relref "guides/operations/automation" >}}) for selectors, watches, and GitOps workflows
  around operations.
- [MachineOperation CRD reference]({{< relref "reference/machina-crd" >}}) for the full schema.
- [CLI reference]({{< relref "reference/cli" >}}) for the exact flags of `machine operation create`, `node-reboot`,
  and the other convenience commands.
- [Isolation Modes]({{< relref "reference/agent/isolation" >}}) for how `nspawn`, `none`, and `container` differ.
- [Networking Operations]({{< relref "reference/networking/operations" >}}) for CNI and Cilium cleanup around node
  replacement.

