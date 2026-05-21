---
title: "Machine Operations"
weight: 7
description: "Day-2 lifecycle operations for scaling, upgrades, and troubleshooting."
---

Machine operations are job-like custom resources that let you perform day-2
lifecycle actions on Unbounded machines. Each operation targets one or more
machines and is handled by the component that owns the relevant boundary.

## Boundaries

Unbounded has two lifecycle boundaries:

- **Host** - the VM, PXE host, or bare-metal machine. Host operations change
  the power state or replace the host entirely.
- **Node** - the `systemd-nspawn` container running kubelet, containerd, CNI
  plugins, and pod workloads. Node operations restart the container while
  leaving the host running.

The agent itself is a third operational target. Agent operations upgrade or
remove the host-resident agent binary and its managed resources.

## Operations at a Glance

| Operation | Boundary | Responsible Component | Description |
|-----------|----------|-----------------------|-------------|
| `HostPowerOff` | Host | `machine-ops-controller` / `metalman` | Power off the VM or physical host. |
| `HostPowerOn` | Host | `machine-ops-controller` / `metalman` | Power on or start the VM or physical host. |
| `HostReboot` | Host | `machine-ops-controller` / `metalman` | Reboot or power-cycle the host. |
| `HostReplace` | Host | `machine-ops-controller` / `metalman` | Delete and recreate the VM or reimage the physical host. |
| `NodeReboot` | Node | `unbounded-agent` | Restart the nspawn container without replacing the rootfs. |
| `AgentUpgrade` | Agent | `unbounded-agent` | Replace the host agent binary using blue-green staging. |
| `AgentReset` | Agent | `unbounded-agent` | Remove the agent and all managed resources from the host. |

## Component Ownership

`machine-ops-controller` owns cloud-provider host operations. It maps
`HostPowerOff`, `HostPowerOn`, `HostReboot`, and `HostReplace` to provider APIs
based on `Machine.spec.provider` and `Machine.spec.providerID`.

`metalman` owns bare-metal host operations for PXE-managed machines. It uses
Redfish/BMC control for power state and boot-order changes.

`unbounded-agent` owns node and agent operations because it runs on the host
alongside `machinectl`, systemd, and the nspawn rootfs.

## Creating an Operation

Operations are created by applying a `MachineOperation` resource. Target a
single machine with `spec.machineRef` or, for agent-handled operations
(`NodeReboot`, `AgentUpgrade`, `AgentReset`) and metalman bare-metal host
operations, a group of machines with `spec.machineSelector`. Bare-metal host
selectors must be scoped to one metalman site with
`unbounded-cloud.io/site=<site>`.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: reboot-worker-01
spec:
  machineRef: worker-01
  operationKind: HostReboot
```

```bash
kubectl apply -f reboot-worker-01.yaml
```

## Checking Status

Operations move through phases: `Pending`, `InProgress`, `Complete`, or
`Failed`.

```bash
# List all operations
kubectl get machineoperations

# Short name
kubectl get mop

# Watch a specific operation
kubectl get mop reboot-worker-01 -w

# View machines and operations together
kubectl get machines,mop

# Detailed status
kubectl describe mop reboot-worker-01
```

Example output:

```
NAME                KIND          MACHINE      PHASE       AGE
reboot-worker-01    HostReboot    worker-01    Complete    2m
```

## Cleanup

Set `spec.ttlSecondsAfterFinished` to automatically delete completed or failed
operations:

```yaml
spec:
  ttlSecondsAfterFinished: 3600
```

## Next Steps

- [Host Operations]({{< relref "guides/operations/host-operations" >}}) -
  power management and host replacement
- [Node Operations]({{< relref "guides/operations/node-operations" >}}) -
  restarting the nspawn container and node recreation
- [Agent Operations]({{< relref "guides/operations/agent-operations" >}}) -
  upgrading and resetting the agent
- [Automation Patterns]({{< relref "guides/operations/automation" >}}) -
  scaling, rolling upgrades, and troubleshooting workflows
