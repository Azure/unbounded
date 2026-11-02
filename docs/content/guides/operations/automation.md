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

Agent-handled operations (`NodeReboot`, `AgentUpgrade`, `AgentReset`,
`HostSoftReboot`) support `spec.machineSelector` to target machines by label. No
child operations are created. Each agent matches the selector against its
Node's labels, or its Machine's labels when the Node does not exist; Machine
labels are not copied to Nodes, so select on labels the Nodes carry. All
matching agents report into the one MachineOperation. It becomes terminal when
the first agent finishes, and agents that have not started by then skip it. To
act on every machine in a group reliably, create one operation per machine.
Metalman-managed bare-metal host operations also support selectors when the
selector is scoped to one metalman site with `unbounded-cloud.io/site=<site>`.

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
```

```bash
kubectl apply -f reboot-gpu-nodes.yaml
kubectl get mop reboot-gpu-nodes -w
```

> **Note:** Cloud VM host operations still require one operation per machine.
> Bare-metal host selectors must name the metalman site.

## Batch Host Operations

For cloud VM host operations, create one MachineOperation per machine:

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
kubectl get mop -o jsonpath='{range .items[?(@.status.phase=="Failed")]}{.metadata.name}{"\n"}{end}'
```

Do not use one `machineSelector` operation to upgrade a group of agents. The
operation finishes when the first agent finishes, and agents that have not
started by then skip it. Create one operation per machine, as above.

### Node Upgrade via Recreation

To roll out a new Kubernetes version or other node settings, make the change in
the MachineConfiguration, or point each Machine's `configurationRef.version` at
a version that has it. Then cordon, drain, and delete Node objects one at a time
and let the agent reconcile. The agent does not read `spec.kubernetes.version`
on the Machine here, and it does not rebuild a node whose configuration has not
changed.

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
| 1 | `NodeReboot` | Kubelet or containerd is stuck. Restarts the nspawn container and the containers running on it. |
| 2 | `HostHardReboot` | Node reboot did not help. Reboots the entire host. |
| 3 | `HostReplace` | The node or host OS is corrupted or needs reimaging. Deleting the Node does not rebuild a node whose configuration has not changed. |
| 4 | `AgentReset` | Decommission the node entirely. |

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

The MachineOperation CRD does not declare selectable fields, so
`--field-selector status.phase=...` is rejected. Filter on the client instead:

```bash
# Failed operations
kubectl get mop -o jsonpath='{range .items[?(@.status.phase=="Failed")]}{.metadata.name}{"\n"}{end}'

# In-progress operations
kubectl get mop -o jsonpath='{range .items[?(@.status.phase=="InProgress")]}{.metadata.name}{"\n"}{end}'
```

### View Machine and Operation Status Together

```bash
kubectl get machines,mop
```

Example output:

```
NAME                                   HOST        PHASE   K8S VERSION   AGE
machine.unbounded-cloud.io/worker-01   10.0.0.11   Ready   v1.34.0       7d
machine.unbounded-cloud.io/worker-02   10.0.0.12   Ready   v1.34.0       7d
machine.unbounded-cloud.io/worker-03   10.0.0.13   Ready   v1.34.0       7d

NAME                                              MACHINE     OPERATION        PHASE      AGE
machineoperation.unbounded-cloud.io/reboot-w01    worker-01   HostHardReboot   Complete   5m
machineoperation.unbounded-cloud.io/upgrade-w02   worker-02   AgentUpgrade     Complete   3m
```

## Automatic Cleanup

Use `ttlSecondsAfterFinished` to prevent completed operations from
accumulating:

```yaml
spec:
  ttlSecondsAfterFinished: 3600   # Clean up after 1 hour
```

Expired operations are deleted by `machine-ops-controller` or metalman. The
agent does not delete them, so the TTL has no effect in a cluster that runs
neither controller.

For long-running workflows, set a longer TTL to preserve audit history. For
high-frequency operations, use a shorter TTL to reduce resource count.

## See Also

- [Machine Operations Overview]({{< relref "guides/operations" >}})
- [Host Operations]({{< relref "guides/operations/host-operations" >}})
- [Node Operations]({{< relref "guides/operations/node-operations" >}})
- [Agent Operations]({{< relref "guides/operations/agent-operations" >}})
