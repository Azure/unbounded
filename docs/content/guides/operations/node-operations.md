---
title: "Node Operations"
weight: 2
description: "Restarting the nspawn container and recreating nodes."
---

Node operations act on the `systemd-nspawn` container that runs kubelet,
containerd, CNI plugins, and pod workloads. They leave the host running and are
handled by `unbounded-agent`.

## NodeReboot

Stops kubelet and containerd inside the nspawn machine, stops the nspawn
container, then restarts it and brings kubelet and containerd back up. The rootfs
is not replaced.

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
NAME                     KIND          MACHINE      PHASE        AGE
node-reboot-worker-01    NodeReboot    worker-01    Pending      0s
node-reboot-worker-01    NodeReboot    worker-01    InProgress   1s
node-reboot-worker-01    NodeReboot    worker-01    Complete     30s
```

After the operation completes, the Node object remains and kubelet re-registers
with the API server.

## Node Recreation

Recreating the node rootfs is not a MachineOperation. Instead, cordon and drain
the Kubernetes Node, then delete the Node object and let the agent reconcile.

When the agent observes that a Machine still exists but the corresponding Node
does not, it:

1. Resolves the desired `MachineConfiguration`
2. Stops the old nspawn machine
3. Creates a new nspawn machine from the desired configuration
4. Lets kubelet join the cluster again

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

Use node recreation when you need to:

- Upgrade the Kubernetes version on a node
- Apply rootfs-level changes from a new `MachineConfiguration`
- Recover from nspawn container corruption

### Why Not a MachineOperation?

Rootfs recreation is a reconciliation workflow driven by desired state, not a
discrete action. The agent already watches for a missing Node and reconciles
automatically. Adding operation kinds like `NodeReimage`, `NodeUpgrade`, or
`NodeRecreate` would duplicate this declarative path.

## Choosing the Right Approach

| Symptom | Action | Why |
|---------|--------|-----|
| Kubelet or containerd is stuck | `NodeReboot` | Restarts the nspawn container without replacing the rootfs. |
| Need a new Kubernetes version | Cordon, drain, then delete the Node | Agent recreates with the desired MachineConfiguration. |
| Node rootfs is corrupted | Cordon, drain, then delete the Node | Agent replaces the rootfs entirely. |
| Host kernel or OS needs updating | `HostReboot` or `HostReplace` | Requires host-level action. See [Host Operations]({{< relref "guides/operations/host-operations" >}}). |
| Host is unresponsive | `HostReboot` | Out-of-band reboot through provider or BMC. |

## See Also

- [Machine Operations Overview]({{< relref "guides/operations" >}})
- [Host Operations]({{< relref "guides/operations/host-operations" >}})
- [MachineOperation CRD Reference]({{< relref "reference/machina-crd" >}})
