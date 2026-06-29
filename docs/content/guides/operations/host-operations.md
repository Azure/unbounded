---
title: "Host Operations"
weight: 1
description: "Power management and host replacement operations."
---

Host operations change the power state of the VM, PXE host, or bare-metal
machine. They are handled by `machine-ops-controller` for cloud VMs and by
`metalman` for PXE/bare-metal machines with BMC.

For metalman-managed bare-metal machines, host operations may target a single
machine with `spec.machineRef` or multiple machines with `spec.machineSelector`.
Selector-based bare-metal host operations must include
`unbounded-cloud.io/site=<site>` so exactly one metalman instance owns the
operation. The default unlabeled metalman instance supports `machineRef` host
operations only.

For cloud VMs, run `machine-ops-controller` scoped to the Machine's site and
provider, for example `--site=remote --provider=AzureVM`. A scoped controller
only executes operations whose target Machine has the matching
`unbounded-cloud.io/site` label and `spec.provider`; other controllers ignore
the operation.

## Provider Requirements

Host operations require out-of-band management access to the machine through a
cloud provider API or BMC. The controller executing the operation communicates
directly with the provider rather than through the agent on the host.

Machines joined via SSH do not have a cloud provider or BMC backing them.
`HostPowerOn` and `HostReplace` cannot work for SSH-joined machines because
there is no out-of-band API to power on or recreate a machine that is off.
`HostReboot` and `HostPowerOff` can be performed from within the host OS by the
agent, but `HostPowerOn` after a power-off requires external intervention.

| Operation | Cloud VM | Bare metal (BMC) | SSH-joined |
|-----------|----------|-------------------|------------|
| `HostReboot` | Yes | Yes | Agent-initiated only |
| `HostPowerOff` | Yes | Yes | Agent-initiated only |
| `HostPowerOn` | Yes | Yes | Not supported |
| `HostReplace` | Yes | Yes | Not supported |

## HostReboot

Reboots or power-cycles the host through the provider or BMC. Use this when a
host is unresponsive or you need to apply host-level changes that require a
restart.

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
kubectl get mop reboot-worker-01 -w
```

```
NAME                KIND          MACHINE      PHASE        AGE
reboot-worker-01    HostReboot    worker-01    Pending      0s
reboot-worker-01    HostReboot    worker-01    InProgress   2s
reboot-worker-01    HostReboot    worker-01    Complete     45s
```

### Provider Behavior

| Provider | Action |
|----------|--------|
| Azure VM | `VirtualMachinesClient.BeginRestart` |
| OCI Instance | `RESET` |
| Bare metal (BMC) | Redfish power reset |

## HostPowerOff

Powers off the VM or physical host. The machine remains registered but stops
running workloads. Use this for scaling in, cost savings during off-hours, or
maintenance windows.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: poweroff-worker-01
spec:
  machineRef: worker-01
  operationKind: HostPowerOff
```

```bash
kubectl apply -f poweroff-worker-01.yaml
```

### Provider Behavior

| Provider | Action |
|----------|--------|
| Azure VM | `VirtualMachinesClient.BeginPowerOff` |
| OCI Instance | `STOP` |
| Bare metal (BMC) | Redfish power off |

## HostPowerOn

Powers on or starts the VM or physical host. Use this to scale back out after a
`HostPowerOff` or to bring machines online for scheduled workloads.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: poweron-worker-01
spec:
  machineRef: worker-01
  operationKind: HostPowerOn
```

```bash
kubectl apply -f poweron-worker-01.yaml
```

### Provider Behavior

| Provider | Action |
|----------|--------|
| Azure VM | `VirtualMachinesClient.BeginStart` |
| OCI Instance | `START` |
| Bare metal (BMC) | Redfish power on |

## HostReplace

Deletes and recreates the VM or reimages the physical host. The new host is
bootstrapped with fresh cloud-init or equivalent first-boot configuration,
installs `unbounded-agent`, and the agent recreates the node so it rejoins the
cluster.

This is the most disruptive host operation. Use it when you need to replace the
host OS, change VM sizing, or recover from corruption that a reboot cannot fix.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: replace-worker-01
spec:
  machineRef: worker-01
  operationKind: HostReplace
```

```bash
kubectl apply -f replace-worker-01.yaml
kubectl get mop replace-worker-01 -w
```

```
NAME                 KIND           MACHINE      PHASE        AGE
replace-worker-01    HostReplace    worker-01    Pending      0s
replace-worker-01    HostReplace    worker-01    InProgress   3s
replace-worker-01    HostReplace    worker-01    Complete     4m
```

Operation completion means the replacement VM or host has been created. It does
not mean the Kubernetes Node is Ready. The Machine controller continues tracking
whether the Node disappears and rejoins.

For bare-metal PXE replacements, metalman keeps the `MachineOperation` active
until the replacement Node object exists so provisioning retries can continue if
cloud-init times out before the node rejoins.

### Provider Behavior

**Azure VM** - reads the existing VM model, detaches NICs and data disks,
deletes the VM resource, and recreates the same VM name with fresh cloud-init
custom data that installs `unbounded-agent`. The old OS disk is not reused.
Configure `machine-ops-controller --api-server-endpoint` with an API server
address reachable from replaced hosts; the generated agent bootstrap config uses
that value.

**OCI Instance** - not currently supported. An identity-preserving OCI
replacement flow with fresh `user_data` injection has not been verified.

**Bare metal (PXE)** - metalman boots the machine through PXE, writes the
selected host OS image, installs or configures the agent, and lets the agent
create the nspawn node. The `MachineOperation` records per-machine progress in
`status.targets[]`; each target completes after the Machine status shows the
requested repave and reboot counters were observed, `Repaved=True`, and the
corresponding Kubernetes Node object exists. Metalman also latches
`status.conditions[type=BootLoaderDownloaded]` to `True` when the machine first
downloads the initial PXE boot loader for the operation, typically over TFTP.
For installer visibility, `status.conditions[type=BootImageWritten]` starts as
`Unknown`, transitions to `False` when the PXE installer starts and requests the
disk image, and transitions to `True` when the installer finishes and sends the
existing PXE disable signal.

### HostReplace vs Node Recreation

`HostReplace` deletes and recreates the entire host. If you only need to replace
the nspawn rootfs (for example, to upgrade the Kubernetes version), delete the
Kubernetes Node object instead and let the agent reconcile. See
[Node Operations]({{< relref "guides/operations/node-operations" >}}) for
details.

## See Also

- [Machine Operations Overview]({{< relref "guides/operations" >}})
- [MachineOperation CRD Reference]({{< relref "reference/machina-crd" >}})
