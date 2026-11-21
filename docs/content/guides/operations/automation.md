---
title: "Automation Patterns"
weight: 4
description: "Scaling, rolling upgrades, and troubleshooting workflows with MachineOperations."
---

This page shows common automation patterns using MachineOperations with kubectl
and shell scripts.

## Targeting a Single Machine

Most operations target a single machine with `spec.machineRef`:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: reboot-worker-01
spec:
  machineRef: worker-01
  operationKind: HostHardReboot
  ttlSecondsAfterFinished: 3600
```

## Targeting Multiple Machines with machineSelector

Every operation kind except `HostAllocate` and `HostRelease` can target a
group of machines with `spec.machineSelector`. The selector matches Machine
labels, not Node labels.

A selector operation is a parent. machina-controller matches the selector
against Machine labels and creates one child `MachineOperation` with
`spec.machineRef` for every matching Machine, named
`<parent>-<machine>-<suffix>` and owned by the parent. Agents and provider controllers execute only the children; they
never act on the parent. The whole spec of a selector operation is
immutable, so every machine gets the same operation.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: reboot-gpu-nodes
spec:
  machineSelector:
    matchLabels:
      role: gpu-worker
  operationKind: NodeReboot
  rollout:
    maxConcurrent: 1
    onFailure: Stop
```

```bash
kubectl apply -f reboot-gpu-nodes.yaml
kubectl get mop reboot-gpu-nodes -w
```

### Pace

Every child is created up front, about a hundred per second. The first ones
the pace allows are created ready to run. The rest are created held, with
`spec.suspend: true`, and machina-controller releases them by clearing it as
earlier ones finish. No agent or provider controller starts a held
operation, and a held operation does not make another operation on its
machine wait. `spec.suspend` can be set only when an operation is created
and only cleared afterward.

- `spec.rollout.maxConcurrent` is how many machines may be worked on at the
  same time: a number of at least 1, or a percentage from `1%` to `100%` of
  the matched machines. The default is one machine at a time. `100%`
  releases them all at once. Each agent then runs its own machine's
  operation, while a provider controller works through its queue of host
  operations a few at a time.
- `spec.rollout.onFailure: Stop` (the default) releases no further machines
  after one fails. The ones already running finish and the parent ends
  `Failed`. The machines never started stay held and are not run.
  `Continue` carries on.
- `spec.rollout.childRetention: Unsuccessful` (the default) removes the
  children that completed once the parent has finished, and keeps the rest:
  the failed ones, and the ones a stopped rollout never started. What is
  left is the list of machines to look at, and the parent's counts still
  say how many completed. A completed child that carries a boot media
  record is kept too. `All` keeps every child, for a per-machine record of
  when each one finished. With `kubectl unbounded machine operation
  create`, pass `--retain-children`. Either way the children go when the
  parent is deleted, including by `ttlSecondsAfterFinished`.

### Status

The children are the per-machine record. The parent does not list its
machines; its `status.summary` has the counts, so its size is the same for
ten machines and for thirty thousand.

| Count | Meaning |
|-------|---------|
| `total` | Machines the selector matched. Set once every child exists. |
| `held` | Children not yet released. |
| `active` | Children released and not finished. |
| `complete` | Children that completed. |
| `failed` | Children that failed, including any deleted before they finished. |

`status.summary.failedMachines` names up to ten failed machines. The parent
is `Complete` only when every child completed, and `Failed` when any failed.

Things to know when running one:

- The set of machines is fixed once every child exists (condition
  `TargetsResolved`). A Machine labeled afterward is not included. While the
  children are still being created, a Machine that starts to match is
  included, and a child deleted in that time is created again. A selector
  that matches no Machine fails at once with reason `NoMachinesMatched`.
- A released child fails with reason `NotPickedUp` when no agent or provider
  controller has taken it for 5 minutes and none has taken any other child
  of the same parent for 5 minutes either. A controller working through a
  long queue is not mistaken for an absent one, and an agent marks a child
  `Pending` as soon as it takes it.
- A child that has been taken is never timed out. If its machine dies while
  the operation is `Pending` or `InProgress`, the parent waits. Delete that
  child to count it as failed and let `onFailure` decide what happens next.
- Once the set is fixed, a deleted child is not created again. It counts as
  failed.
- An operation that waited, held, for its turn does not go ahead of one that
  started on the same machine in the meantime: it joins the machine's queue
  when it is released.
- A parent that has finished does not change. If someone releases a child
  that a stopped rollout left held, that child runs and the parent's counts
  no longer describe it.
- Deleting the parent deletes its children. `ttlSecondsAfterFinished` on the
  parent deletes it, and so its children, after it finishes.
- Only an identity that may create MachineOperations can write a selector
  operation's status. Node agents cannot.

### Scale

Each agent watches only the operations that name its own machine
(`spec.machineRef` is a selectable field), so what the API server sends an
agent does not grow with the fleet. This needs Kubernetes 1.31 or later. On
an older API server the agent logs a warning and watches every
MachineOperation, which does not scale to large fleets.

## Worked Example: Upgrade the Agents of One Rack

Upgrade the agents of one rack, one machine at a time (the default pace),
and wait for the parent to finish:

```bash
kubectl unbounded machine operation create upgrade-rack-01 \
  --kind AgentUpgrade \
  --selector topology.unbounded-cloud.io/rack=dc-a.rack-01 \
  --param downloadURL=https://example.com/unbounded-agent-linux-amd64.tar.gz \
  --wait
```

`--wait` prints the counts as they change. It returns when the parent
reaches a terminal phase and fails when the parent is `Failed`, naming up to
ten failed machines.

Read the counts on the parent. The `Message` column, shown with `-o wide`,
carries them too:

```bash
kubectl get mop upgrade-rack-01 -o wide
kubectl get mop upgrade-rack-01 -o jsonpath='{.status.summary}{"\n"}'
```

List the operation of each machine by the label the controller sets on the
children, or only the ones that failed or are still held:

```bash
uid=$(kubectl get mop upgrade-rack-01 -o jsonpath='{.metadata.uid}')
kubectl get mop -l unbounded-cloud.io/parent-operation-uid=$uid
kubectl get mop -l unbounded-cloud.io/parent-operation-uid=$uid --field-selector status.phase=Failed
kubectl get mop --field-selector spec.machineRef=worker-01
```

## Batch Host Operations

To act on a known list of machines individually, create one MachineOperation
per machine:

```bash
# Reboot all machines in a list
for machine in worker-01 worker-02 worker-03; do
  cat <<EOF | kubectl apply -f -
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: reboot-${machine}
spec:
  machineRef: ${machine}
  operationKind: HostHardReboot
  ttlSecondsAfterFinished: 3600
EOF
done

# Watch all operations
kubectl get mop -w
```

## Scaling In and Out

### Scale In (Power Off)

Power off machines during off-hours or when demand is low:

```bash
for machine in worker-01 worker-02 worker-03; do
  cat <<EOF | kubectl apply -f -
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: poweroff-${machine}
spec:
  machineRef: ${machine}
  operationKind: HostPowerOff
  ttlSecondsAfterFinished: 3600
EOF
done

# Verify machines are powered off
kubectl get machines worker-01 worker-02 worker-03
```

### Scale Out (Power On)

Bring machines back online when capacity is needed:

```bash
for machine in worker-01 worker-02 worker-03; do
  cat <<EOF | kubectl apply -f -
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: poweron-${machine}
spec:
  machineRef: ${machine}
  operationKind: HostPowerOn
  ttlSecondsAfterFinished: 3600
EOF
done

# Watch nodes rejoin
kubectl get nodes -w
```

## Rolling Upgrades

### Agent Upgrade

Upgrade agents across a fleet sequentially, waiting for each to complete before
proceeding:

```bash
for machine in worker-01 worker-02 worker-03; do
  cat <<EOF | kubectl apply -f -
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: upgrade-agent-${machine}
spec:
  machineRef: ${machine}
  operationKind: AgentUpgrade
  parameters:
    downloadURL: https://example.com/releases/unbounded-agent-v1.2.0-linux-amd64.tar.gz
    sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  ttlSecondsAfterFinished: 7200
EOF

  echo "Waiting for ${machine}..."
  kubectl wait mop upgrade-agent-${machine} \
    --for=jsonpath='{.status.phase}'=Complete \
    --timeout=120s
done
```

If any upgrade fails, the agent automatically rolls back to the last-known-good
binary. Check failed operations before continuing:

```bash
kubectl get mop --field-selector status.phase=Failed
```

Alternatively, use a selector operation to upgrade all matching agents.
It matches the selector against Machine labels, creates one child
operation per matching Machine, and acts on them at the pace set by
`spec.rollout`; see the worked example above.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: upgrade-all-gpu-agents
spec:
  machineSelector:
    matchLabels:
      role: gpu-worker
  operationKind: AgentUpgrade
  rollout:
    maxConcurrent: 1
    onFailure: Stop
  parameters:
    downloadURL: https://example.com/releases/unbounded-agent-v1.2.0-linux-amd64.tar.gz
    sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

### Node Upgrade via Recreation

To roll out a new Kubernetes version or rootfs configuration, cordon, drain, and
delete Node objects one at a time and let the agent reconcile:

```bash
for node in worker-01 worker-02 worker-03; do
  echo "Recreating ${node}..."
  kubectl cordon ${node}
  kubectl drain ${node} --ignore-daemonsets --delete-emptydir-data
  kubectl delete node ${node}

  # Wait for the node to rejoin
  kubectl wait node ${node} --for=condition=Ready --timeout=300s
done
```

Drain must complete before deletion so kubelet, containerd, and the configured
CNI can tear down workload pod state. If the cluster uses an eBPF CNI such as
Cilium and requires destructive dataplane cleanup, run the CNI-specific cleanup
procedure before deleting the Node.

### Host Replacement

For full host reimaging, use `HostReplace` operations sequentially:

```bash
for machine in worker-01 worker-02 worker-03; do
  cat <<EOF | kubectl apply -f -
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: replace-${machine}
spec:
  machineRef: ${machine}
  operationKind: HostReplace
  ttlSecondsAfterFinished: 7200
EOF

  echo "Waiting for ${machine} replacement..."
  kubectl wait mop replace-${machine} \
    --for=jsonpath='{.status.phase}'=Complete \
    --timeout=600s

  # Wait for the node to rejoin after replacement
  kubectl wait node ${machine} --for=condition=Ready --timeout=300s
done
```

## Troubleshooting Escalation

When a node is misbehaving, escalate through progressively more disruptive
operations:

| Step | Operation | When to Use |
|------|-----------|-------------|
| 1 | `NodeReboot` | Kubelet or containerd is stuck. Restarts the nspawn container. |
| 2 | `HostHardReboot` | Node reboot did not help. Reboots the entire host. |
| 3 | Delete the Node | Host is running but the node needs a fresh rootfs. |
| 4 | `HostReplace` | Host OS is corrupted or needs reimaging. |
| 5 | `AgentReset` | Decommission the node entirely. |

Example escalation:

```bash
# Step 1: Try a node reboot
cat <<EOF | kubectl apply -f -
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: troubleshoot-worker-01-step1
spec:
  machineRef: worker-01
  operationKind: NodeReboot
  ttlSecondsAfterFinished: 3600
EOF

kubectl wait mop troubleshoot-worker-01-step1 \
  --for=jsonpath='{.status.phase}'=Complete \
  --timeout=120s

# Check if the node is healthy now
kubectl get node worker-01

# Step 2: If still unhealthy, escalate to host reboot
cat <<EOF | kubectl apply -f -
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: troubleshoot-worker-01-step2
spec:
  machineRef: worker-01
  operationKind: HostHardReboot
  ttlSecondsAfterFinished: 3600
EOF
```

## Monitoring Operations

### Watch All Operations

```bash
kubectl get mop -w
```

### Filter by Phase

```bash
# Failed operations
kubectl get mop --field-selector status.phase=Failed

# In-progress operations
kubectl get mop --field-selector status.phase=InProgress
```

### View Machine and Operation Status Together

```bash
kubectl get machines,mop
```

Example output:

```
NAME                                    PROVIDER    PHASE     AGE
machine.unbounded-cloud.io/worker-01    AzureVM     Ready     7d
machine.unbounded-cloud.io/worker-02    AzureVM     Ready     7d
machine.unbounded-cloud.io/worker-03    AzureVM     Ready     7d

NAME                                              KIND              MACHINE      PHASE      AGE
machineoperation.unbounded-cloud.io/reboot-w01     HostHardReboot    worker-01    Complete   5m
machineoperation.unbounded-cloud.io/upgrade-w02    AgentUpgrade      worker-02    Complete   3m
```

## Automatic Cleanup

Use `ttlSecondsAfterFinished` to prevent completed operations from
accumulating:

```yaml
spec:
  ttlSecondsAfterFinished: 3600   # Clean up after 1 hour
```

For long-running workflows, set a longer TTL to preserve audit history. For
high-frequency operations, use a shorter TTL to reduce resource count.

## See Also

- [Machine Operations Overview]({{< relref "guides/operations" >}})
- [Host Operations]({{< relref "guides/operations/host-operations" >}})
- [Node Operations]({{< relref "guides/operations/node-operations" >}})
- [Agent Operations]({{< relref "guides/operations/agent-operations" >}})
