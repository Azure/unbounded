---
title: "Host Operations"
weight: 1
description: "How to run power, reboot, replace, and other host-level operations on Machines through the Unbounded API."
---

## Overview

The `machine-ops-controller` and `metalman` subsystems provide operators with various ways to manage host machines, ranging from standard cloud VM operations to more sophisticated bare metal management.

The `machine-ops-controller` system implements machine-specific operation logic, and is intended to support most host management scenarios you are likely to encounter. This subsystem is heavily utilized by `metalman`. When using `metalman` to manage PXE or BMC bare metal hosts, your `metalman` requests will be fulfilled by calls to the `machine-ops-controller` APIs.

Instead of interacting directly with `machine-ops-controller` APIs, `metalman` uses high-level resources to represent these operations. For more information on the available `metalman` resources and their interactions with `machine-ops-controller`, see [Machine operations overview].

### How it works

Use `machine-ops-controller` to manage Kubernetes machines that are cloud VMs. The machine types are controlled by `metalman` from the `metalman` machine resource types (see [Machine definitions](#machine-definitions) for more information).

Use `metalman` to manage bare metal machines that use PXE or BMC management. When you use `metalman` to manage machines, it uses `machine-ops-controller` under the hood to perform most of the required operations.

Operators can also use `unbounded-agent` to execute host reboot operations. These operations are executed by the agent, which is installed on the machine host operating system.

These operations use `spec.machineRef` and `spec.machineSelector` for targeting machines.

Ownership and management is decided by the presence of a machine's site label. Machines with a machine's site label, like `site.alpha`, fall under the ownership of a site and are managed by that site's corresponding `metalman` instance. Machines without a site label fall under `machine-ops-controller` management.

Metalman takes ownership of machines that use the selector and boot media (for example, when the machine fails to join Kubernetes but has PXE boot media configured). In this scenario, you are responsible for managing this machine manually.

In both cases, when a machine fails the ownership checks, the machine is eventually cleaned up.

All machine management operations are scoped to a specific site. The site and provider are selected using `--site` and `--provider` flags.

## Provider Requirements

Many machine-level operations require the ability to perform out-of-band management on the host. This means you must have access to the provider API or the BMC for your machine types.

This project supports a wide variety of machine types. While most operations require provider-level access, some operations run on the machine host OS directly.

| Provider Type           | SSH-joined | VM | BMC |
|-------------------------|-----------|----|-----|
| Cloud VM                | No        | Yes| No  |
| BMC-managed bare metal  | No        | No | Yes |
| SSH-joined Kubernetes   | Yes       | No | No  |

Cloud VMs and BMC-managed bare metal machines require out-of-band management. SSH-joined Kubernetes machines, by contrast, don't require BMC management.

### Provider API Access

Most bare metal machines require BMC management to perform out-of-band actions. The providers of these machines must support direct API communication for these management paths.

If you run bare metal machines in your own environment, make sure you configure BMC management access for all of your machine types. Otherwise, this machine type will not support machine-level operations.

### Exceptions

Some machine-level operations don't require provider API access. Instead, they run on the machine host operating system directly, by using the Kubernetes SSH system.

- `HostSoftReboot`: This operation is SSH-joined and doesn't require BMC management.
- Node operations: These also run on the machine host operating system and don't require BMC management. See [Node operations](#node-operations) for more information.

### SSH-joined Machine Types

You can also use SSH-joined machine types in Kubernetes. These types are always SSH-joined, and don't require BMC management. They are always managed by the `unbounded-agent` and can always join Kubernetes via SSH.

These machine types are usually not suitable for bare metal scenarios, and are generally not supported by `metalman`. Instead, use SSH-joined machine types in Kubernetes for cases that do require SSH, such as [Remote execution](#remote-execution) or [debugging](#debugging).

## HostHardReboot

`HostHardReboot` restarts or power cycles a machine through the provider or BMC. Use this operation to recover from a non-responsive host or to apply host-level configuration changes that require a restart.

This operation is available in `MachineOperation` manifests as both a machine-type-specific `operation` and as a subcommand on a machine resource. Use `kubectl apply` for the operation and `kubectl get mop` to track completion.

The operation moves through `Pending`, `InProgress`, and `Complete` phases.

### Provider Behavior

The operation behavior depends on the machine type.

- Azure VM: This operation calls `VirtualMachinesClient.BeginRestart` for the targeted VM.
- Bare metal: This triggers a Redfish power reset.

## HostSoftReboot

`HostSoftReboot` performs an operating system reboot on a machine. This is the equivalent of running `systemctl reboot` on the machine host operating system through the agent.

The operation shuts down services in systemd's ordered sequence before triggering a reboot to apply kernel or OS-level configuration changes.

Use `kubectl unbounded machine host-soft-reboot` command on the target machine resource. This command doesn't require provider or BMC access. If the machine isn't SSH-joined, this operation fails and you must use `HostHardReboot`.

### Status and Blocking

The `status.targets[].bootID` field returned by the operation is the kernel boot ID.

The operation moves through `InProgress`, `Complete`, or `Failed` phases.

In this project, all machine-level operations are blocked by any unfinished out-of-band operations on the machine. Completion depends on the operation type.

For example, `HostReplace` boot media or disk boot requires the machine's previous disk boot status to match before the machine is ready.

The `Pending` phase in `status.message` tells you the reason for waiting. You can delete operations that never finish.

## HostPowerOff

`HostPowerOff` shuts down a cloud VM or a physical bare metal machine.

Like most host operations, the machine remains registered in Kubernetes after the host is shut down.

Use this operation for scale-in events, or for other cost-saving or power-saving scenarios.

Use this operation for planned machine maintenance or other scenarios requiring a host power-off.

This operation is available in `MachineOperation` manifests as both a machine-type-specific `operation` and as a subcommand on a machine resource. Use `kubectl apply` for the operation.

### Provider Behavior

The operation behavior depends on the machine type.

- Azure VM: This operation calls `VirtualMachinesClient.BeginPowerOff` for the targeted VM.
- Bare metal: This triggers a Redfish power off.

## HostPowerOn

`HostPowerOn` starts up a cloud VM or a physical bare metal machine.

Use `HostPowerOn` to return a VM or machine to a running state after `HostPowerOff`.

Use `HostPowerOn` for scheduled workload brings.

This operation is available in `MachineOperation` manifests as both a machine-type-specific `operation` and as a subcommand on a machine resource. Use `kubectl apply` for the operation.

### Provider Behavior

The operation behavior depends on the machine type.

- Azure VM: This operation calls `VirtualMachinesClient.BeginStart` for the targeted VM.
- Bare metal: This triggers a Redfish power on.

## HostReplace

`HostReplace` replaces an existing machine with a new one.

Use this operation to delete and recreate a physical bare metal machine.

Use this operation to redeploy a machine with a new OS image.

Use this operation for bare-metal machines that require fresh cloud-init first-boot configuration. This involves deploying the `unbounded-agent` and having the node rejoin the Kubernetes cluster.

Use this operation only in disruptive scenarios. `HostReplace` takes a machine offline to recreate a new copy.

Use this operation to replace the entire operating system on a machine.

Use this operation to resize or replace a cloud VM, which requires a data disk detach.

Use `kubectl apply` for the operation and `kubectl get mop` to track completion.

The operation is considered complete when the host is ready, not when the Kubernetes Node becomes ready. For a PXE machine, the operation only completes when the node exists.

### Provider Behavior

This section describes how the provider interacts with the machine when this operation is invoked.

The behavior depends on the machine type:

- Azure VM: The operation reads the machine model, creates a new machine, attaches required data disks, configures the new machine with `unbounded-agent` installed, and applies required cloud-init configuration.
  The new machine is configured with a `--api-server-endpoint` flag to rejoin Kubernetes.
- If `spec.isolation` is `container`, replacing the machine replaces the isolation layer, so the machine requires the `--experimental-container-isolation` flag to rejoin Kubernetes.
- Bare metal: The operation generates a new PXE boot configuration to override existing boot media, configures the BMC to perform a Redfish power restart, wipes and rewrites the host OS image, and configures the `unbounded-agent` so the node can rejoin Kubernetes.

Progress for this operation is returned in `status.targets[]` as per-machine progress. Success is based on `BootLoaderDownloaded`, `BootImageWritten`, and `CloudInitDone` conditions.

### HostReplace vs Node Recreation

The difference between `HostReplace` and Node recreation lies in the scope of the replacement.

- `HostReplace`: Replaces the full host, including the kernel, rootfs, and all node software.

- Node recreation: Replaces only the rootfs and node software on an existing host.

In most scenarios, such as Kubernetes version upgrades, replace the rootfs and the `unbounded-agent` rather than replace the entire host. If you need to replace the host (for example, to change kernel modules or the operating system), use `HostReplace`.

For more information about the difference between `HostReplace` and Node recreation, see [Node operations](#node-operations).

## See Also

- [Machine operations overview]({{< relref "guides/operations" >}})
- `MachineOperation` CRD reference

