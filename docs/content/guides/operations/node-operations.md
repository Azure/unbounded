---
title: "Node Operations"
weight: 2
description: "Restarting the nspawn container and recreating nodes."
---

Node operations act on the `systemd-nspawn` container that runs kubelet,
containerd, CNI plugins, and pod workloads. They leave the host running and are
handled by `unbounded-agent`.

## NodeReboot

Restarts the nspawn machine with `systemctl restart systemd-nspawn@<machine>`.
kubelet, containerd and the node's other services stop and start with the
machine, and so do the containers running on it. The node is not drained
first. The rootfs is not replaced.

Use this when you need to restart Kubernetes components without touching the
host, for example after applying node-level configuration changes or to clear
transient runtime issues.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: node-reboot-worker-01
spec:
  machineRef: worker-01
  operationKind: NodeReboot
```

```bash
kubectl apply -f node-reboot-worker-01.yaml
kubectl get mop node-reboot-worker-01 -w
```

```
NAME                    MACHINE     OPERATION    PHASE        AGE
node-reboot-worker-01   worker-01   NodeReboot   Pending      0s
node-reboot-worker-01   worker-01   NodeReboot   InProgress   1s
node-reboot-worker-01   worker-01   NodeReboot   Complete     30s
```

After the operation completes, the Node object remains and kubelet re-registers
with the API server.

## Node Recreation

Recreating the node rootfs is not a MachineOperation. Instead, cordon and drain
the Kubernetes Node, then delete the Node object and let the agent reconcile.

When the agent sees the Node being deleted, it:

1. Resolves the desired configuration from the Machine's `configurationRef`
2. If it differs from the applied configuration, stops the old nspawn machine
   and creates a new one from the desired configuration, then lets kubelet join
   the cluster again
3. Otherwise records the desired version as applied and changes nothing

The agent reacts only to deletions it sees while its daemon is running. A Node
deleted while the daemon is stopped is not acted on.

The agent reacts to Node deletion; it does not evict pods or perform
CNI-specific dataplane cleanup. Drain the node first so kubelet, containerd, and
the CNI can run normal pod teardown. For eBPF CNIs such as Cilium, follow the
CNI's own guidance if a full dataplane cleanup is required before reimaging the
nspawn rootfs.

```bash
# Prepare the Node for recreation
kubectl cordon worker-01
kubectl drain worker-01 --ignore-daemonsets --delete-emptydir-data

# Delete the Node to trigger recreation after drain completes
kubectl delete node worker-01

# Watch the machine until the node rejoins
kubectl get machines worker-01 -w
```

Use node recreation to apply a changed configuration from the Machine's
`configurationRef`, such as a new Kubernetes version, node labels, taints, agent
image or local DNS settings. Make the change in the MachineConfiguration, or
point `configurationRef.version` at a version that has it, before you delete the
Node. This path does not read `spec.kubernetes.version` on the Machine.

Deleting the Node does not rebuild a node whose configuration has not changed,
so it does not recover a corrupted rootfs. Use `HostReplace` for that.

### Why Not a MachineOperation?

Rootfs recreation is a reconciliation workflow driven by desired state, not a
discrete action. The agent already watches for Node deletion and applies the
desired configuration. Adding operation kinds like `NodeReimage`, `NodeUpgrade`, or
`NodeRecreate` would duplicate this declarative path.

## Choosing the Right Approach

| Symptom | Action | Why |
|---------|--------|-----|
| Kubelet or containerd is stuck | `NodeReboot` | Restarts the nspawn container without replacing the rootfs. |
| Need a new Kubernetes version | Update the MachineConfiguration, then cordon, drain, and delete the Node | Agent recreates the node with the version in `configurationRef`. |
| Node rootfs is corrupted | `HostReplace` | Reinstalls the host and the node. Deleting the Node does not rebuild an unchanged node. |
| Host kernel or OS needs updating | `HostSoftReboot` or `HostReplace` | Requires host-level action. See [Host Operations]({{< relref "guides/operations/host-operations" >}}). |
| Host is unresponsive | `HostHardReboot` | Out-of-band reboot through provider or BMC. |

## See Also

- [Machine Operations Overview]({{< relref "guides/operations" >}})
- [Host Operations]({{< relref "guides/operations/host-operations" >}})
- [MachineOperation CRD Reference]({{< relref "reference/machina-crd" >}})
