# Lifecycle Operations

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
| `HostReplace` | Host | `machine-ops-controller` for cloud VMs; `metalman` for PXE/bare metal | Delete and recreate the VM or reimage the physical host, inject bootstrap configuration through cloud-init or an equivalent first-boot mechanism, and have the agent recreate the node so it rejoins the cluster. |
| `NodeReboot` | Node container | `unbounded-agent` on the host | Stop kubelet and containerd inside the nspawn machine, stop the nspawn machine, then restart it and bring kubelet and containerd back up without replacing the rootfs. |
| `AgentUpgrade` | Host agent | `unbounded-agent`, with controller coordination | Replace the host-resident agent binary and restart it safely. |

## Component ownership

`machine-ops-controller` owns cloud-provider host operations. Today it maps
`HostPowerOff`, `HostPowerOn`, and `HostReboot` to Azure VM and OCI instance
APIs based on `Machine.spec.provider` and `Machine.spec.providerID`. For
`HostReplace`, a cloud provider implementation should delete and recreate the VM
using the provider's API and inject the bootstrap configuration through
cloud-init or an equivalent first-boot mechanism.

`metalman` owns bare-metal host operations for PXE-managed machines. It uses
Redfish/BMC control for power state and boot-order changes. For `HostReplace`,
metalman should boot the machine through PXE, write the selected host OS image,
install or configure the agent, and let the agent create the nspawn node.

`unbounded-agent` owns node operations because it runs on the host next to
`machinectl`, systemd, and the nspawn rootfs under `/var/lib/machines`. Node
operations do not power the host on or off.

## Not MachineOperation values

Rootfs recreation is a reconciliation workflow, not a separate operation. To
reimage or upgrade a node, delete the Kubernetes `Node` object. The agent should
observe that the `Machine` still exists but the corresponding `Node` does not,
resolve the desired `MachineConfiguration`, delete the old nspawn rootfs, create
a new nspawn machine, and let kubelet join the cluster again.

This is distinct from `HostReplace`. `HostReplace` deletes and recreates the
host itself. Node recreation replaces only the nspawn rootfs on an otherwise
running host.

For that reason, these are intentionally not operation names:

| Avoid | Use instead |
|---|---|
| `NodeReimage` | Delete the Kubernetes `Node` and let the agent recreate from desired state. |
| `NodeUpgrade` | Update desired configuration if needed, delete the Kubernetes `Node`, and let the agent recreate from desired state. |
| `NodeRecreate` | Delete the Kubernetes `Node` and let reconciliation recreate the nspawn machine. |

## Status and cleanup

`MachineOperation` status follows a job-like lifecycle: `Pending`,
`InProgress`, `Complete`, or `Failed`. Implementations should set
`status.startedAt`, `status.completedAt`, a human-readable `status.message`, and
conditions that identify the executor and failure reason. Completed and failed
operations may be removed with `spec.ttlSecondsAfterFinished`.

Controllers for providers with long-running operations persist the accepted
provider operation handle in the matching `status.targets[]` entry and poll it
one step per reconciliation. Begin callbacks must be idempotent for the stable
`MachineOperation` UID because they may be repeated until the handle is
persisted. Each provider request uses the provider ID from the current Machine.
Host operations targeting the same Machine are serialized. This keeps provider
operations resumable without changing the four-phase MachineOperation
lifecycle.

## Risks and open questions

If no component claims an operation, the operation may remain pending forever.
The implementation needs an ownership or admission strategy that prevents
unsupported operations from silently hanging.

Rollback semantics are intentionally open. A rollback may be a node recreation
against a previously deployed `MachineConfiguration` or
`MachineConfigurationVersion`, but the exact selection and safety rules need a
separate design.
