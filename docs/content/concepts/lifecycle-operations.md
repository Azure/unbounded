---
title: "Lifecycle Operations"
weight: 5
description: "Proposed operation boundaries and executor ownership for MachineOperation requests."
---

This page defines the proposed `MachineOperation` model for Unbounded lifecycle
actions. The operation names and executor ownership need team consensus before
they are treated as API commitments.

Unbounded has two lifecycle boundaries: the **host** and the **node**. Host
operations change the power state of the VM, PXE host, or bare-metal machine.
Node operations change the `systemd-nspawn` container that runs kubelet,
containerd, CNI plugins, and pod containers while leaving the host running.

Operations are requested with `MachineOperation` and target a `Machine` by
`spec.machineRef` or `spec.machineSelector`. Each operation is handled by the
component that owns the relevant boundary. Controllers that do not own an
operation ignore it.

## Proposed operations

| Operation | Boundary | Responsible component | Meaning |
|---|---|---|---|
| `HostPowerOff` | Host | `machine-ops-controller` for cloud VMs; `metalman` for PXE/bare metal with BMC | Power off the VM or physical host through the provider or BMC. |
| `HostPowerOn` | Host | `machine-ops-controller` for cloud VMs; `metalman` for PXE/bare metal with BMC | Power on or start the VM or physical host through the provider or BMC. |
| `HostReboot` | Host | `machine-ops-controller` for cloud VMs; `metalman` for PXE/bare metal with BMC | Reboot, reset, or power-cycle the host through the provider or BMC. |
| `NodeStop` | Node container | `unbounded-agent` on the host | Stop kubelet and containerd inside the nspawn machine, then stop the nspawn machine. The rootfs remains intact. |
| `NodeStart` | Node container | `unbounded-agent` on the host | Start an existing nspawn machine, then start containerd and kubelet. |
| `NodeReboot` | Node container | `unbounded-agent` on the host | Perform `NodeStop` followed by `NodeStart` without replacing the rootfs. |
| `AgentUpgrade` | Host agent | `unbounded-agent`, with controller coordination | Replace the host-resident agent binary and restart it safely. |

## Component ownership

`machine-ops-controller` owns cloud-provider host operations. Today it maps
`HostPowerOff`, `HostPowerOn`, and `HostReboot` to Azure VM and OCI instance
APIs based on `Machine.spec.provider` and `Machine.spec.providerID`.

`metalman` owns bare-metal host operations for PXE-managed machines. It uses
Redfish/BMC control for power state and boot-order changes.

`unbounded-agent` owns node operations because it runs on the host next to
`machinectl`, systemd, and the nspawn rootfs under `/var/lib/machines`. Node
operations do not power the host on or off.

## Not MachineOperation values

Rootfs recreation is a reconciliation workflow, not a separate operation. To
reimage or upgrade a node, delete the Kubernetes `Node` object. The agent should
observe that the `Machine` still exists but the corresponding `Node` does not,
resolve the desired `MachineConfiguration`, delete the old nspawn rootfs, create
a new nspawn machine, and let kubelet join the cluster again.

For that reason, these are intentionally not operation names:

| Avoid | Use instead |
|---|---|
| `NodePowerOff` | `NodeStop` |
| `NodePowerOn` | `NodeStart` |
| `NodeReimage` | Delete the Kubernetes `Node` and let the agent recreate from desired state. |
| `NodeUpgrade` | Update desired configuration if needed, delete the Kubernetes `Node`, and let the agent recreate from desired state. |
| `NodeRecreate` | Delete the Kubernetes `Node` and let reconciliation recreate the nspawn machine. |

## Status and cleanup

`MachineOperation` status follows a job-like lifecycle: `Pending`,
`InProgress`, `Complete`, or `Failed`. Implementations should set
`status.startedAt`, `status.completedAt`, a human-readable `status.message`, and
conditions that identify the executor and failure reason. Completed and failed
operations may be removed with `spec.ttlSecondsAfterFinished`.

## Risks and open questions

If no component claims an operation, the operation may remain pending forever.
The implementation needs an ownership or admission strategy that prevents
unsupported operations from silently hanging.

Rollback semantics are intentionally open. A rollback may be a node recreation
against a previously deployed `MachineConfiguration` or
`MachineConfigurationVersion`, but the exact selection and safety rules need a
separate design.
