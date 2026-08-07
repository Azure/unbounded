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
  operationKind: HostReboot
  ttlSecondsAfterFinished: 3600
```

## Targeting Multiple Machines with machineSelector

Agent-handled operations (`NodeReboot`, `AgentUpgrade`, `AgentReset`) support
`spec.machineSelector` to target machines by label. Each agent independently
checks whether its machine matches the selector and executes the operation if it
does. Metalman-managed bare-metal host operations also support selectors when
the selector is scoped to one metalman site with `unbounded-cloud.io/site=<site>`.

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
> Bare-metal host selectors are handled only by the metalman instance for the
> selected site.

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
  operationKind: HostReboot
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

Alternatively, use `machineSelector` to upgrade all matching agents at once:

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
| 2 | `HostReboot` | Node reboot did not help. Reboots the entire host. |
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
  operationKind: HostReboot
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

NAME                                              KIND          MACHINE      PHASE      AGE
machineoperation.unbounded-cloud.io/reboot-w01     HostReboot    worker-01    Complete   5m
machineoperation.unbounded-cloud.io/upgrade-w02    AgentUpgrade  worker-02    Complete   3m
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
