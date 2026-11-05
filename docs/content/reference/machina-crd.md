---
title: "CRD Reference"
weight: 2
description: "API reference for the Machine custom resource."
---

API group: `unbounded-cloud.io/v1alpha3`

This document describes the custom resource definitions shipped with machina: **Machine** and **MachineOperation**.

## Machine

| Property | Value |
|----------|-------|
| Kind | `Machine` |
| Plural | `machines` |
| Short name | `mach` |
| Scope | Cluster |
| Status subresource | Yes |

**Printer columns:**

| Name | JSON Path | Description |
|------|-----------|-------------|
| Host | `.spec.ssh.host` | SSH target address |
| Phase | `.status.phase` | Current lifecycle phase |
| K8s Version | `.spec.kubernetes.version` | Desired Kubernetes version |
| Age | standard | Time since creation |

### spec.isolation

| Property | Type | Required | Description |
|----------|------|----------|-------------|
| `spec.isolation` | string | No | Where the node stack (containerd, kubelet, CNI, CoreDNS) runs on the host. `nspawn`, `none`, or `container` (experimental; machina provisions it only with `experimentalContainerIsolation`). Empty declares nothing: a node keeps the mode it runs, and a new node uses `nspawn`. Passed to the agent config as `Isolation`. The agent never writes this field on an existing Machine; it sets it only when it creates the Machine at self-registration. |

A change takes effect at the node's next repave. A change that needs a
reprovision also needs the annotation `unbounded-cloud.io/allow-reprovision:
"true"`. See [Isolation Modes]({{< relref "reference/agent/isolation" >}}).

### spec.ssh

SSH connection details. When `ssh` is nil, the machina controller skips the Machine entirely.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `ssh` | SSHSpec | No | - | SSH connection configuration. |
| `ssh.host` | string | Yes | - | Hostname or IP, optionally with port (e.g. `1.2.3.4:2222`). Port 22 is assumed when omitted. |
| `ssh.username` | string | No | `"azureuser"` | SSH username. |
| `ssh.privateKeyRef` | SecretKeySelector | Yes | - | Reference to a Secret containing the SSH private key. Must reside in the `unbounded-system` namespace. |
| `ssh.privateKeyRef.name` | string | Yes | - | Secret name. |
| `ssh.privateKeyRef.namespace` | string | Yes | - | Secret namespace (must be `unbounded-system`). |
| `ssh.privateKeyRef.key` | string | No | `"ssh-privatekey"` | Key within the Secret's `data` map. |
| `ssh.bastion` | BastionSSHSpec | No | - | Optional jump host for the SSH connection. |
| `ssh.bastion.host` | string | Yes | - | Bastion hostname or IP, optionally with port. |
| `ssh.bastion.username` | string | No | `"azureuser"` | Bastion SSH username. |
| `ssh.bastion.privateKeyRef` | *SecretKeySelector | No | Same as `ssh.privateKeyRef` | Bastion SSH key. Falls back to the parent `ssh.privateKeyRef` when omitted. |

### spec.host.netboot

Network boot configuration consumed by the Metalman controller. The released
top-level `spec.pxe` remains a deprecated fallback for existing Machines.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `host.netboot` | PXESpec | No | - | PXE boot configuration. |
| `host.netboot.image` | string | Yes | - | OCI machine image reference containing `/disk/disk.img.gz` (e.g. `"ghcr.io/azure/host-ubuntu2404:v1"`). |
| `host.netboot.architecture` | string | No | `amd64` | Target CPU architecture for PXE boot artifacts and machine images. Allowed values: `amd64`, `arm64`. |
| `host.netboot.netbootImage` | string | No | Metalman default | OCI netboot image reference containing PXE boot artifacts. |
| `host.netboot.bootProtocol` | string | No | `PXE` | Network boot trigger protocol for repaves. `PXE` uses DHCP/TFTP bootfile options. `HTTP` uses Redfish UEFI HTTP boot with a URL derived from the netboot image metadata. Allowed values: `PXE`, `HTTP`. |
| `host.netboot.dhcpLeases` | []DHCPLease | No | - | Provisioning network settings. They are served as static DHCP leases during PXE boot and used for Redfish firmware, installer, NoCloud, and installed-system static configuration during HTTP boot. |
| `host.netboot.dhcpLeases[].ipv4` | string | Yes | - | Static IPv4 address to assign. |
| `host.netboot.dhcpLeases[].mac` | string | Yes | - | NIC MAC address (matched case-insensitively). |
| `host.netboot.dhcpLeases[].subnetMask` | string | Yes | - | Subnet mask. |
| `host.netboot.dhcpLeases[].gateway` | string | Yes | - | Default gateway. |
| `host.netboot.dhcpLeases[].dns` | []string | No | - | DNS server addresses. |
| `host.netboot.targetDisk` | string | No | Installer-selected | Block device the installer writes the machine image to, such as `/dev/nvme0n1` or `/dev/disk/by-id/...`. When omitted, the initrd selects a disk automatically. |
| `host.netboot.redfish` | RedfishSpec | No | - | BMC access via the Redfish API. |
| `host.netboot.redfish.url` | string | Yes | - | Redfish endpoint URL. |
| `host.netboot.redfish.username` | string | Yes | - | Redfish username. |
| `host.netboot.redfish.deviceID` | string | No | `"1"` | Redfish system device ID. |
| `host.netboot.redfish.passwordRef` | RedfishPasswordSelector | Yes | - | Secret containing the Redfish password. |
| `host.netboot.redfish.passwordRef.name` | string | Yes | - | Secret name. |
| `host.netboot.redfish.passwordRef.namespace` | string | Yes | - | Secret namespace. |
| `host.netboot.redfish.passwordRef.key` | string | No | `"password"` | Key within the Secret's `data` map. |
| `host.netboot.cloudInit` | CloudInitSpec | No | - | Optional cloud-init customization for PXE-booted machines. |
| `host.netboot.cloudInit.userDataConfigMapRef` | ConfigMapKeySelector | No | - | Reference to a ConfigMap containing custom cloud-init user-data. |
| `host.netboot.cloudInit.userDataConfigMapRef.name` | string | Yes | - | ConfigMap name. |
| `host.netboot.cloudInit.userDataConfigMapRef.namespace` | string | Yes | - | ConfigMap namespace. |
| `host.netboot.cloudInit.userDataConfigMapRef.key` | string | No | `"user-data"` | Key within the ConfigMap. |

> **Migration note:** `redfish.passwordRef.key` used to default to
> `"ssh-privatekey"`. It now defaults to `"password"`. Existing Machines keep
> the key that was stored when they were created. New or replaced Machines
> that omit `key` read the `password` key. If your Redfish password Secret
> stores the password under `ssh-privatekey`, set `key: ssh-privatekey`.

### spec.kubernetes

Kubernetes join configuration.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `kubernetes` | KubernetesSpec | No | - | Kubernetes join settings. |
| `kubernetes.version` | string | No | Cluster version | Desired Kubernetes version (e.g. `"v1.34.0"`). A `v` prefix is added automatically if missing. |
| `kubernetes.nodeRef` | *LocalObjectReference | No | - | Reference to the corresponding Node object. Set by the controller. |
| `kubernetes.nodeLabels` | map[string]string | No | - | Labels to apply to the Node (not yet propagated by the machina controller). |
| `kubernetes.bootstrapTokenRef.name` | string | Yes | - | Name of the bootstrap token Secret in `kube-system`. |

### spec.host

`host` groups host ownership and the desired host image. New Machines select at
most one of `netboot`, `azure`, or `external`. This keeps built-in host identity
on the Machine while preserving `external.machineRef` as an escape hatch for
providers whose own CRD has meaningful schema, status, or reconciliation.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `host.image` | string | No | Preserve current image | Opaque image identifier interpreted by the selected provider. |
| `host.netboot` | PXESpec | For new Metalman Machines | -- | Network boot image, DHCP, Redfish, and cloud-init settings owned by Metalman. |
| `host.azure.resourceID` | string | For Azure VMs | -- | Immutable full Azure Resource Manager VM ID. The provider is inferred as `AzureVM`. |
| `host.external.provider` | string | For external hosts | -- | Registered provider controller and credential key, such as `ANS` or a private provider. |
| `host.external.providerID` | string | Provider-dependent | -- | Opaque provider identity; mutable only to support provider replacement handoff. |
| `host.external.machineRef` | ProviderMachineReference | Provider-dependent | -- | Optional cluster-scoped provider-owned resource for rich provider state. |

If `spec.host.image` is omitted, a `HostReplace` inherits
`MachineConfigurationVersion.spec.template.host.image`. If both are omitted,
the provider preserves the host's current image. The resolved value is frozen
in the `MachineOperation` target before provider execution. Updating desired
image state does not initiate replacement; only an explicit `HostReplace`
MachineOperation authorizes that destructive action.

The built-in Azure provider stores its single machine-specific value inline:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: worker-01
spec:
  host:
    azure:
      resourceID: /subscriptions/<subscription>/resourceGroups/<group>/providers/Microsoft.Compute/virtualMachines/worker-01
```

The released top-level `spec.pxe`, `spec.provider`, and `spec.providerID` fields
remain readable as deprecated fallbacks. New host ownership cannot be mixed
with those legacy fields. Migration tooling is intentionally separate.

Machine operation credentials are selected by the Machine site label. Providers that support OIDC/workload identity use `WorkloadIdentity`; providers or sites that need provider-specific credential material use `ExternalPlugin` with a referenced Secret.
Custom Go controllers register the operations they support with
`pkg/machineops.NewProvider` and declare their provider-owned resource with
`WithProviderMachineKind`. Each operation selects either an immediate
callback or long-running begin and poll callbacks, plus optional replay,
replacement bootstrap, and cleanup behavior. The controller is installed with
`pkg/machineops/controller.AddToManager`; provider code does not reconcile
`MachineOperation` status directly. A provider that declares capacity with
`WithCapacity` installs the MachineClass controller with
`pkg/machineops/controller.AddCapacityToManager`, described in
[MachineClass](#machineclass). Long-running begin callbacks must be
idempotent for `OperationRequest.OperationUID` because the controller may call
them again until their operation handle has been persisted. `OperationRequest`
contains the exact external machine resource UID and generation, resolved host
image, and observed Machine generation frozen in target status. Providers
receive the canonical `host.external.providerID` or Azure resource ID; legacy
Machines continue to supply `Machine.spec.providerID`. Out-of-band host
operations targeting the same Machine are serialized in creation order, except
that metalman runs an operation still holding boot media, or whose boot from
disk after media release is unconfirmed, ahead of others, even if it is
terminal. A
`HostSoftReboot` does not start while an out-of-band host operation on its
Machine is unfinished. Out-of-band host operations do not wait for a
`HostSoftReboot`.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperationCredential
metadata:
  name: remote-azure
spec:
  siteName: remote
  provider: AzureVM
  auth:
    mode: WorkloadIdentity
```

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperationCredential
metadata:
  name: remote-example
spec:
  siteName: remote
  provider: ExampleCloud
  auth:
    mode: ExternalPlugin
    secretRef:
      namespace: unbounded-system
      name: remote-example-auth
```

### spec.allocationRef and spec.fenced

A Machine created for a `MachineAllocation` names it in `allocationRef`, and
`fenced` forbids the agent from starting that Machine's node again.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `allocationRef` | UIDReference | No | - | The `MachineAllocation` this Machine was created for. Can be set only when the Machine is created. |
| `allocationRef.name` | string | Yes | - | Allocation name. |
| `allocationRef.uid` | string | Yes | - | Allocation UID. |
| `fenced` | bool | No | `false` | Forbids the agent from starting this Machine's node again. Can only change from `false` to `true`. Requires `allocationRef`. |

The API server enforces these rules:

| Rule | Message |
|------|---------|
| `allocationRef` cannot be added, removed, or changed after create, including by an update that drops the whole `spec`. | `allocationRef is immutable` |
| `fenced` cannot go from `true` back to `false` or be removed, including by an update that drops the whole `spec`. | `fenced cannot be unset` |
| `fenced: true` needs `allocationRef`, on create and on update. | `fenced requires allocationRef` |

An agent whose kubelet `providerID` starts with
`unbounded:///machineallocations/` treats the rest of it as its allocation UID
and checks before every node start: at bootstrap, at every start of the nspawn
machine (including after a host reboot), at every start of the kubelet in
isolation mode `none`, including a restart after the kubelet exits, before a
Node Recreation, and before a `NodeReboot`. Once the agent holds its daemon-controller certificate, it reads
the Machine live with it and allows the start only when the Machine's
`allocationRef.uid` is this allocation's UID, `fenced` is not set, and the
Machine is not being deleted. Before that certificate exists, it checks that
the API server still accepts its bootstrap token. It never falls back from the
certificate to the token. The agent picks the credential again
before every read, so a certificate that expires while the agent is still
checking moves the check to the bootstrap token.

The agent refuses closed. A fenced, deleting, missing, or foreign Machine, or a
rejected credential, refuses at once. Other failures are read again every 10
seconds for up to 5 minutes, then refuse. A refused bootstrap reports Reason
`AllocationLaunchRefused` on the `AgentBootstrapped` condition, and a refused
`NodeReboot` finishes `Failed` with Reason `AllocationLaunchRefused`. A Node
Recreation refused for a transient failure is tried again after 5 minutes.

Every start of the nspawn machine runs the check in its pre-start, the
`unbounded-agent-regenerate-config@<slot>` unit:

- A transient refusal is retried every 10 seconds.
- A final refusal exits with status `77`. systemd does not retry it, the
  `unbounded-agent-regenerate-config@<slot>` unit is left failed, and so is the
  start of the nspawn machine. A `NodeReboot` whose restart is refused this way
  finishes `Failed` with Reason `ExecutionFailed`.
- The node stays down until something starts it again: a host reboot, a
  `NodeReboot`, or a Node Recreation. Each such start checks again.
- The agent daemon still starts, so `HostSoftReboot` and `AgentReset` still
  complete on such a host.

In isolation mode `none`, every start of `unbounded-kubelet.service` runs the
check in `unbounded-kubelet-launch-check.service`, which the kubelet unit
requires. That unit exists only on an allocation host.

- Transient and final refusals behave as for nspawn: a transient refusal is
  retried every 10 seconds, and a final refusal exits with status `77`, leaves
  the launch-check unit failed, and fails the kubelet's start without a restart.
- The kubelet stays down until something starts it again: a host reboot, a
  `NodeReboot`, a Node Recreation, or the kubelet's own restart. Each such start
  checks again.
- Pods that were already running when the kubelet stopped are not stopped by
  the check.

For a repave that changes the isolation mode, the agent checks before it hands
the host to the reprovision. A refusal leaves the node as it is and changes no
mode. The reset removes the daemon-controller certificate, so the bootstrap in
the new mode checks with the bootstrap token, before the node starts. A final
refusal there ends the reprovision at once, and the host stays reset with no
node.

The agent does not register a Machine for an allocation host. Fencing only
stops new starts: a node that is already running is not stopped.

## MachineOperation

| Property | Value |
|----------|-------|
| Kind | `MachineOperation` |
| Plural | `machineoperations` |
| Short name | `mop` |
| Scope | Cluster |
| Status subresource | Yes |

`MachineOperation` is a job-like CR for discrete operations. The in-host agent handles Kubernetes node operations such as `NodeReboot` and agent operations such as `AgentReset`; `machine-ops-controller` handles out-of-band VM operations such as Azure VM power actions. PXE/BMC operations remain owned by metalman for now.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec.machineRef` | string | No | Target `Machine` name. Either `machineRef` or `machineSelector` must be set. |
| `spec.machineSelector` | LabelSelector | No | Selects Machines by label. Supported for agent-handled operations (`NodeReboot`, `AgentUpgrade`, `AgentReset`). Each matching agent independently picks up the operation. Not supported for host operations. |
| `spec.operationKind` | string | Yes | One of `NodeReboot`, `AgentUpgrade`, `AgentReset`, `HostHardReboot`, `HostSoftReboot`, `HostPowerOff`, `HostPowerOn`, `HostReplace`. |
| `spec.parameters` | map[string]string | No | Operation-specific parameters. |
| `spec.ttlSecondsAfterFinished` | int32 | No | Delete completed or failed operations after this many seconds. |
| `status.phase` | string | No | `Pending`, `InProgress`, `Complete`, or `Failed`. |
| `status.message` | string | No | Human-readable status message. |
| `status.startedAt` | time | No | Operation start timestamp. |
| `status.completedAt` | time | No | Terminal phase timestamp. |
| `status.targets` | []TargetStatus | No | Per-Machine target status snapshot used by host operation controllers. |
| `status.conditions` | []Condition | No | Operation conditions. `Completed` tracks terminal state. `BootLoaderDownloaded=True` is latched by metalman when a target first downloads the initial PXE boot loader, usually over TFTP. `BootImageWritten` starts as `Unknown` for metalman `HostReplace`, transitions to `False` when the PXE installer requests `disk.img.gz`, and transitions to `True` when the existing `/pxe/disable` completion signal is received. `CloudInitDone` starts as `Unknown`, transitions to `False` when first-boot cloud-init starts, and transitions to `True` on final cloud-init success or `False` with reason `Failed` and a summarized error when cloud-init reports a failure. |

`AgentUpgrade` is handled by the in-host agent and requires `spec.parameters.downloadURL`. The URL may use HTTP or HTTPS and must point to an `unbounded-agent` release tarball. The optional `spec.parameters.sha256` is the expected digest of the compressed archive. The agent verifies the digest when provided, stages the archive's exact `unbounded-agent` member as the inactive blue/green daemon binary, records the previous binary as last known good, and restarts `unbounded-agent-daemon.service`. If systemd cannot keep the upgraded daemon running, `unbounded-agent-daemon-recovery.service` switches the daemon back to the last known good binary.

The Azure VM provider handles:

| Operation | Azure action |
|-----------|--------------|
| `HostHardReboot` | `VirtualMachinesClient.BeginRestart` |
| `HostPowerOff` | `VirtualMachinesClient.BeginPowerOff` |
| `HostPowerOn` | `VirtualMachinesClient.BeginStart` |
| `HostReplace` | `VirtualMachinesClient.Get`, `BeginDelete`, then `BeginCreateOrUpdate` |

`HostReplace` for `AzureVM` destructively replaces the VM: it reads the existing VM model, detaches NICs and data disks, deletes the VM resource, and recreates the same VM name with fresh cloud-init custom data that installs `unbounded-agent`. An explicit host image may be an Azure resource ID or a `publisher:offer:sku:version` reference; an omitted image preserves the existing image reference. The old OS disk is not reused. Operation completion means the replacement VM create operation completed; it does not mean the Kubernetes `Node` is Ready. The `Machine` controller continues tracking whether the Kubernetes `Node` disappears and rejoins. Configure `machine-ops-controller --api-server-endpoint` with an API server address reachable from replaced hosts; the generated agent bootstrap config uses that value.

This replacement flow avoids Azure standalone VM `customData` immutability during native reimage. It intentionally destroys host-local state on the old OS disk. The initial implementation retains the existing blocking clone-delete-create flow; a controller crash after deletion can require manual recovery because the captured VM model is not yet durably checkpointed.

Metalman handles bare-metal host operations for Machines with
`spec.host.netboot.redfish` (or deprecated `spec.pxe.redfish`) and no external
host owner. Bare-metal host operations may
target one Machine with `spec.machineRef` or a site-scoped set of Machines with
`spec.machineSelector`. Selector-based bare-metal host operations must select a
single metalman site with `unbounded-cloud.io/site=<site>`.

For host operations, `status.targets[]` is snapshotted when execution starts
and remains authoritative even if labels later change. Metalman records its
state-machine progress directly in each target. Resumable external providers
also store the provider operation handle on the target so polling can continue
after a controller restart. Each entry includes:

| Field | Type | Description |
|-------|------|-------------|
| `machineRef` | string | Target Machine name. |
| `phase` | string | Target phase: `Pending`, `InProgress`, `Complete`, or `Failed`. |
| `stage` | string | Target operation stage such as `WaitingOff`, `WaitingOn`, or `WaitingRepave`. |
| `message` | string | Human-readable target progress or failure message. |
| `startedAt` | time | Target start timestamp. |
| `completedAt` | time | Target terminal timestamp. |
| `observedGeneration` | int64 | Machine generation acted on. |
| `bootID` | string | Host kernel boot ID the agent observed before issuing a `HostSoftReboot`. |
| `input.providerRef` | ProviderMachineSnapshot | Provider resource group, kind, name, UID, and generation frozen before execution. |
| `input.hostImage` | string | Resolved provider-interpreted image frozen for `HostReplace`; empty means preserve the current image. |
| `attempts` | int32 | External action attempts for retryable Redfish operations. |
| `lastAttemptAt` | time | Most recent external action attempt timestamp. |
| `providerOperation` | ProviderOperationStatus | Resumable external operation metadata, including provider, operation ID, and an opaque non-secret resume token. |

### status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | Current lifecycle phase (see table below). |
| `message` | string | Human-readable status message. |
| `ssh.fingerprint` | string | SSH host key fingerprint (not yet implemented). |
| `redfish.certFingerprint` | string | BMC TLS certificate SHA-256 fingerprint. Set by metalman using TOFU. |
| `tpm.ekPublicKey` | string | TPM endorsement key in PEM format. Set by metalman attestation using TOFU. |
| `bootstrap.credentialID` | string | Credential ID (`JTI=<jti>`) of the last bootstrap token issued by metalman attestation. Used by metalman to approve the node's kubelet CSR. |
| `bootstrap.nodeName` | string | Kubernetes Node name the bootstrap token was issued for. Metalman approves the kubelet CSR only for this name. During a virtual-media `HostReplace` it is the name captured when the replacement started. |
| `bootstrap.issuedAt` | time | When that bootstrap token was issued. |
| `isolation` | string | The isolation mode the agent reports as applied on the host. Written by the agent daemon at startup. Empty until the agent reports one. Compared with `spec.isolation` to set the `IsolationDrift` condition. |
| `isolationChange` | object | The agent's last attempt to change the node to the mode in `spec.isolation`, made during a repave. Fields: `targetMode`, `phase` (`InProgress`, `Succeeded`, or `Failed`), `strategy` (`InPlace` or `Reprovision`), `reason` (for a refused change: `UnsupportedMode`, `ExperimentalNotEnabled`, or `ReprovisionNotAllowed`), `message` (at most 4096 bytes), and `lastTransitionTime`. |
| `conditions` | []Condition | Standard Kubernetes conditions (see below). |

### Conditions

| Type | Set By | Description |
|------|--------|-------------|
| `SSHReachable` | machina | `True` / `False` based on a TCP probe to the SSH port. |
| `Provisioning` | machina | `True` while the install script is running over SSH. `lastTransitionTime` records when provisioning started, used to detect stale provisioning attempts (e.g. after a controller restart). |
| `Provisioned` | machina | `True` after successful SSH provisioning. `ObservedGeneration` tracks the spec generation. |
| `CloudInitDone` | metalman | Observed first-boot cloud-init result for PXE machines. Metalman also mirrors cloud-init progress to active `HostReplace` `MachineOperation` conditions. |
| `IsolationDrift` | machina | Compares `spec.isolation` with `status.isolation` and `status.isolationChange`. Absent until the agent has reported a mode. The reasons are listed under [Drift]({{< relref "reference/agent/isolation#drift" >}}). |

### Phase lifecycle

The machina controller drives the following phases:

| Phase | Meaning | Requeue interval |
|-------|---------|------------------|
| `Pending` | SSH is unreachable. | 30 s |
| `Provisioning` | Install script is running over SSH. | - |
| `Joining` | Provisioned; waiting for a Node with the matching label. | 30 s |
| `Ready` | Node exists, or no `kubernetes` spec is present. | 5 min |
| `Failed` | Provisioning encountered an error. | 60 s |
| `Rebooting` | Reserved for metalman or provider controllers. | - |

### Labels and annotations

**Labels:**

| Label | Applied to | Description |
|-------|-----------|-------------|
| `unbounded-cloud.io/machine` | Node | Maps the Node back to its Machine CR. Set during provisioning. |
| `unbounded-cloud.io/site` | Machine | Scopes a metalman instance to a subset of Machines. |
| `unbounded-cloud.io/default-bootstrap-token` | Secret | Marks a Secret as the default bootstrap token for auto-discovery. |

**Annotations:**

| Annotation | Description |
|-----------|-------------|
| `unbounded-cloud.io/provider` | Associates a Machine with a provider controller (extension point). |

### Examples

**Minimal SSH-only Machine:**

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: worker-01
spec:
  ssh:
    host: "10.0.0.50"
    privateKeyRef:
      name: ssh-key
      namespace: unbounded-system
  kubernetes:
    version: v1.34.0
    bootstrapTokenRef:
      name: bootstrap-token-abc123
```

**SSH with bastion:**

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: worker-02
spec:
  ssh:
    host: "192.168.1.100:2222"
    username: ubuntu
    privateKeyRef:
      name: ssh-key
      namespace: unbounded-system
      key: id_ed25519
    bastion:
      host: "bastion.example.com"
      username: jump
  kubernetes:
    version: v1.34.0
    bootstrapTokenRef:
      name: bootstrap-token-abc123
```

**Azure VM with external power operations:**

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: azure-worker-01
spec:
  host:
    azure:
      resourceID: /subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-workers/providers/Microsoft.Compute/virtualMachines/azure-worker-01
  configurationRef:
    name: azure-workers
```

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: azure-worker-01-hardreboot
spec:
  machineRef: azure-worker-01
  operationKind: HostHardReboot
  ttlSecondsAfterFinished: 300
```

**PXE / bare-metal Machine:**

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: baremetal-01
  labels:
    unbounded-cloud.io/site: lab
spec:
  ssh:
    host: "10.0.0.60"
    privateKeyRef:
      name: ssh-key
      namespace: unbounded-system
  host:
    netboot:
      image: ghcr.io/azure/host-ubuntu2404:v1
      architecture: amd64
      dhcpLeases:
      - ipv4: "10.0.0.60"
        mac: "aa:bb:cc:dd:ee:ff"
        subnetMask: "255.255.255.0"
        gateway: "10.0.0.1"
        dns:
        - "8.8.8.8"
      redfish:
        url: "https://bmc-01.example.com"
        username: admin
        passwordRef:
          name: bmc-password
          namespace: unbounded-system
      cloudInit:
        userDataConfigMapRef:
          name: my-cloud-init
          namespace: unbounded-system
  kubernetes:
    version: v1.34.0
    bootstrapTokenRef:
      name: bootstrap-token-abc123
```

---

## PXE OCI Images

Metalman uses a machine image and a netboot image for PXE repaves. The machine
image is referenced by `spec.host.netboot.image` and contains `/disk/disk.img.gz`. The
netboot image is referenced by `spec.host.netboot.netbootImage`, or by Metalman's default
when that field is omitted, and contains the reusable PXE boot environment.
`spec.host.netboot.architecture` selects the OCI platform manifest to pull for both
images and defaults to `amd64`.

Both images are standard OCI container images built `FROM scratch` with artifacts
under `/disk/`. This follows the kubevirt containerDisk convention.

Files with a `.tmpl` suffix in the netboot image are Go templates rendered
per-machine at serve time; other files are served verbatim. A `metadata.yaml`
file in the netboot image provides image-level configuration such as
`dhcpBootImageName` and `httpBootPath`.

### Image layout

![Netboot OCI image filesystem layout under /disk/: shimx64.efi, grubx64.efi, vmlinuz, initrd, init.cpio, unbounded-agent, metadata.yaml, grub/grub.cfg.tmpl, cloud-init templates](../../img/machina-netboot-layout.svg)

### Template data

Templates receive the following data object:

| Field | Type | Description |
|-------|------|-------------|
| `.Machine` | *Machine | The Machine CR that initiated the request. |
| `.BootLease` | *DHCPLease | The DHCP lease matching the request source IP, or the first lease when no match is available. Netboot templates use this to pass the provisioning NIC MAC, static IP, gateway, and DNS to the installer and NoCloud network configuration. |
| `.ApiserverURL` | string | External Kubernetes API server URL. |
| `.ServeURL` | string | External metalman HTTP URL. |
| `.KubernetesVersion` | string | Resolved Kubernetes version for the machine. |
| `.ClusterDNS` | string | Cluster DNS service IP. |

The default netboot template passes `.BootLease.MAC` as `unbounded.boot_mac`.
The installer initrd uses that MAC address to configure the provisioning
interface instead of relying on kernel interface names such as `eth0`.
If `spec.host.netboot.targetDisk` is set, the template passes it as `unbounded.disk`;
otherwise the installer falls back to automatic disk selection.

### Building images

Images are built, tagged, and pushed using standard container tooling:

```bash
docker build -t ghcr.io/azure/host-ubuntu2404:v1 -f images/host-ubuntu2404/Containerfile .
docker build -t ghcr.io/azure/netboot:v1 -f images/netboot/Containerfile .
docker push ghcr.io/azure/host-ubuntu2404:v1
docker push ghcr.io/azure/netboot:v1
```

See `images/host-ubuntu2404/` for a machine image Containerfile and `images/netboot/` for the
reusable netboot image Containerfile.

### metadata.yaml

```yaml
dhcpBootImageName: shimx64.efi
httpBootPath: shimx64.efi
```

The `dhcpBootImageName` field specifies the boot filename included in DHCP
responses (option 67) for `spec.host.netboot.bootProtocol: PXE`.

The `httpBootPath` field specifies the file path, relative to metalman's HTTP
artifact server, used for `spec.host.netboot.bootProtocol: HTTP`. If `httpBootPath` is
omitted, metalman falls back to `dhcpBootImageName` for the UEFI HTTP boot URL.

## MachineClass

| Property | Value |
|----------|-------|
| Kind | `MachineClass` |
| Plural | `machineclasses` |
| Short name | `mcls` |
| Scope | Cluster |
| Status subresource | Yes |

A `MachineClass` is a provider-neutral template that capacity consumers
request `MachineAllocation`s from. The provider interprets its provider
configuration and publishes the capacity offerings it can launch.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec.siteRef.name` | string | Yes | The `Site` that allocated machines join. Immutable. |
| `spec.provider` | string | Yes | The registered provider that interprets the class. Immutable. |
| `spec.providerConfigRef` | TypedObjectReference | Yes | Provider configuration. Only a core-group `ConfigMap` with a `namespace` is accepted. |
| `spec.configurationRef.name` | string | Yes | The `MachineConfiguration` name. |
| `spec.configurationRef.version` | int32 | Yes | The pinned `MachineConfigurationVersion` number. |
| `spec.bootstrap.filesRef` | NamespacedObjectReference | No | A `ConfigMap` of bootstrap files. Its `run` key is the script that runs as root before the agent installer. |
| `spec.bootstrap.agentArtifact.url` | string | No | Agent tarball location, an `https` or `file:///` URL. |
| `spec.bootstrap.agentArtifact.sha256` | string | No | Lowercase hex SHA-256 of the agent tarball. Required with `url`. |

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineClass
metadata:
  name: remote-spot
spec:
  siteRef:
    name: remote
  provider: ExampleCloud
  providerConfigRef:
    kind: ConfigMap
    namespace: unbounded-capacity
    name: remote-spot
  configurationRef:
    name: workers
    version: 3
```

### MachineClass controller

The MachineClass controller resolves each class it serves and publishes the
result in the class status. It serves a class only when the class's
`spec.provider` is a registration that declares capacity and, when the
controller is scoped to a Site, the class's `spec.siteRef.name` is that Site.
It reads its inputs in this order and stops at the first failure:

1. The `Site` must exist and must not be deleting.
2. The provider-config `ConfigMap` must be in the configured provider-config
   namespace, must set `immutable: true`, and must not have `binaryData`.
3. The bootstrap files `ConfigMap`, when `spec.bootstrap.filesRef` is set,
   follows the same rules and must have a non-blank `run` key.
4. The `MachineConfigurationVersion` named `<name>-v<version>` must exist, and
   its `spec.version` and `unbounded-cloud.io/machine-configuration` label must
   match the reference.
5. Exactly one `MachineOperationCredential` must match the class's Site and
   provider. Its Secret, if any, must be in the configured credential Secret
   namespace.

The controller then calls the provider's `ResolveClass` with the class name and
UID, the Site name, a copy of the provider-config data, and the credential.
Every offering must have a unique non-empty `name`, an `instanceType`, a valid
`capacityType` and `availability`, a `validUntil`, and, when a `price` is set, a
valid currency and hourly price. The controller publishes the offerings sorted
by name.

Two `CapacityOptions` intervals control when a class is resolved again:

| Option | Default | Description |
|--------|---------|-------------|
| `ClassRefreshInterval` | 5m | How long to wait after a successful resolve when no offering expires sooner, and after an input failure. Must be at least the effective `MinRequeueInterval`. |
| `MinRequeueInterval` | 10s | The shortest wait after a successful resolve. Must be at least 1s and at most the effective `ClassRefreshInterval`. |

After a successful resolve the controller waits until the earliest unexpired
`validUntil` or for `ClassRefreshInterval`, whichever is sooner, but never less
than `MinRequeueInterval`. `OfferingsCurrent` can therefore stay `True` for up
to `MinRequeueInterval` after the earliest `validUntil`. A transient provider
error is retried with backoff. The controller also resolves a class again when
its spec changes or when a referenced `Site`, `ConfigMap`,
`MachineConfigurationVersion`, or `MachineOperationCredential` changes. Secrets
are not watched; a Secret change is picked up at the next refresh.

### MachineClass status

| Field | Type | Description |
|-------|------|-------------|
| `observedGeneration` | int64 | The class generation of the last successful resolve. |
| `resolvedRevision` | string | Identifies the launch inputs of the last successful resolve. |
| `providerConfig` | ObjectContentSnapshot | Namespace, name, UID, and SHA-256 of the provider-config data. |
| `bootstrapFiles` | ObjectContentSnapshot | Namespace, name, UID, and SHA-256 of the bootstrap files data. Absent without `filesRef`. |
| `configuration` | ConfigurationContentSnapshot | Configuration name, version, version object name, UID, and SHA-256 of the template. |
| `site` | UIDReference | Name and UID of the Site. |
| `credential` | UIDReference | Name and UID of the matched `MachineOperationCredential`. |
| `offerings` | []MachineClassOffering | The provider's offerings, sorted by name. |
| `conditions` | []Condition | `Ready` and `OfferingsCurrent`. |

`resolvedRevision` is the SHA-256 of the provider, the `site` and `credential`
references, the `providerConfig`, `bootstrapFiles`, and `configuration`
snapshots, and `spec.bootstrap.agentArtifact`. It excludes the offerings, all
timestamps, the conditions, the class generation, name, and UID, and Secret
content, so refreshed offerings keep the same revision. Only a successful
resolve changes `observedGeneration`, `resolvedRevision`, the snapshots, the
references, and the offerings, and it changes them together. A failure keeps
the last published values and reports itself in the conditions, whose
`observedGeneration` is the generation that was read.

| Condition | Status | Reason | Meaning |
|-----------|--------|--------|---------|
| `Ready` | `True` | `Resolved` | The inputs resolved and the provider accepted them. |
| `Ready` | `False` | `SiteNotFound`, `SiteDeleting` | The Site is missing or being deleted. |
| `Ready` | `False` | `ProviderConfigInvalid`, `ProviderConfigForbidden`, `ProviderConfigNotFound`, `ProviderConfigNotImmutable` | The provider-config reference is not a namespaced core `ConfigMap`, or the `ConfigMap` is outside the allowed namespace, missing, not immutable, or has `binaryData`. |
| `Ready` | `False` | `BootstrapFilesForbidden`, `BootstrapFilesNotFound`, `BootstrapFilesNotImmutable`, `BootstrapFilesInvalid` | The bootstrap files `ConfigMap` is outside the allowed namespace, missing, not immutable, has `binaryData`, or has no `run` key. |
| `Ready` | `False` | `ConfigurationInvalid`, `ConfigurationNotFound`, `ConfigurationMismatch` | The configuration version is unset, missing, or does not match the reference. |
| `Ready` | `False` | `AuthNotFound`, `AuthAmbiguous`, `AuthInvalid`, `AuthSecretForbidden`, `AuthSecretNotFound` | No single usable credential matches the class's Site and provider. |
| `Ready` | `False` | `ProviderConfigRejected` | The provider returned a `PermanentError`. The message carries its text. |
| `Ready` | `False` | `ResolveFailed`, `OfferingsInvalid` | A transient provider error or invalid offerings. When `Ready` was `True` and neither the inputs nor the generation changed since the last successful resolve, `Ready` stays `True` instead. |
| `OfferingsCurrent` | `True` | `OfferingsAvailable` | At least one offering is `Available` and its `validUntil` has not passed. |
| `OfferingsCurrent` | `False` | `NoOfferingsAvailable` | The class resolved, but no offering is `Available` and unexpired. |
| `OfferingsCurrent` | `False` | `ClassNotReady` | `Ready` is `False` because of an input failure or `ProviderConfigRejected`. |
| `OfferingsCurrent` | `False` | `ResolveFailed`, `OfferingsInvalid` | The last resolve failed transiently or returned invalid offerings. |

### Installing the MachineClass controller

A custom provider controller registers capacity with `WithCapacity` and
installs the MachineClass controller with
`pkg/machineops/controller.AddCapacityToManager`:

```go
err := controller.AddCapacityToManager(mgr, providers, controller.CapacityOptions{
	ProviderName:              "ExampleCloud",
	SiteName:                  "remote",
	CredentialSecretNamespace: "unbounded-system",
	ProviderConfigNamespace:   "unbounded-capacity",
})
```

The controller runs only while the manager holds its leader-election lease, so
enable leader election with a lease ID unique to the provider and Site scope.
Two deployments must not serve overlapping scopes, such as an unscoped
deployment next to one scoped to a Site. Set `EnableAllocationController` to
also install the MachineAllocation controller, described in
[MachineAllocation](#machineallocation).

`deploy/machine-ops/04-machineclass-rbac.yaml.tmpl` defines three ClusterRoles
and no bindings. Bind them to the controller's ServiceAccount:

| ClusterRole | Grants | Bind with |
|-------------|--------|-----------|
| `unbounded-machineclass-controller` | `machineclasses` read, `machineclasses/status` update, and `sites`, `machineconfigurationversions`, `machineoperationcredentials` read | A ClusterRoleBinding. |
| `unbounded-machineclass-controller-provider-config` | `configmaps` `get`, `list`, `watch` | A RoleBinding in the provider-config namespace only. |
| `unbounded-machineclass-controller-credentials` | `secrets` `get` | A RoleBinding in the credential Secret namespace only. |

The controller's manager also needs its own leader-election Lease permissions.

## MachineAllocation

| Property | Value |
|----------|-------|
| Kind | `MachineAllocation` |
| Plural | `machineallocations` |
| Short name | `malloc` |
| Scope | Cluster |
| Status subresource | Yes |

A `MachineAllocation` is one consumer's request for one machine from a
`MachineClass`. Its UID owns every provider effect made for it. Every spec
field except `lifecycle` is immutable.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec.consumerRef` | object | Yes | The requesting object: `apiGroup`, `kind`, `name`, and `uid`. |
| `spec.classRef` | UIDReference | Yes | The exact `MachineClass`, by name and UID. |
| `spec.resources` | ResourceList | No | The minimum resources the machine must have after overhead. |
| `spec.requirements` | []Requirement | No | Label requirements on the offering, with `In`, `NotIn`, `Exists`, `DoesNotExist`, `Gt`, or `Lt`, and an optional `minValues`. |
| `spec.nodeTemplate` | object | No | `labels`, `taints`, and `startupTaints` the Node registers with. |
| `spec.lifecycle` | string | No | `Active` (default) or `Released`. It moves only from `Active` to `Released`. |

`spec.lifecycle: Released` is the only thing that authorizes releasing the
host. A consumer sets it after its own workload and storage steps are done.
Deleting the allocation, or losing its consumer, fences the host but does not
release it.

### MachineAllocation controller

The MachineAllocation controller serves an allocation when its provider, taken
from `status.snapshot` or else from the class `spec.classRef` names, is a
registration that declares capacity and, when the controller is scoped to a
Site, the allocation's Site is that Site. An allocation with neither a class nor
a snapshot is served by every allocation controller and fails with
`InvalidRequest`. It launches a host in this order:

1. It checks that `spec.classRef.name` and `spec.consumerRef.uid`, which become
   the `unbounded-cloud.io/machine-class` and `unbounded-cloud.io/consumer-uid`
   label values as written, are valid label values (at most 63 characters).
   Otherwise the allocation fails with `InvalidRequest` and gets no snapshot.
   Once the class is `Ready` for its current generation and its live inputs
   still match the class status, the controller selects an offering. It also
   checks that the class's Site name and the node labels the Machine will
   register with are valid labels, and fails with `InvalidRequest` otherwise.
   It then calls the provider's `PlanAllocation`, and writes `status.snapshot`,
   `status.providerID` (`unbounded:///machineallocations/<uid>`), the planned
   instance type, zone, capacity type, image, capacity, allocatable, and labels
   in one status write.
2. It adds the finalizer `unbounded-cloud.io/machineallocation` and the labels
   `unbounded-cloud.io/allocation-uid`, `unbounded-cloud.io/consumer-uid`, and
   `unbounded-cloud.io/machine-class`.
3. It adds the finalizer `machineallocations.unbounded-cloud.io/<uid>` to the
   provider-config and bootstrap files `ConfigMap`s. It removes it once the
   `HostAllocate` has a provider handle or has finished, or when release
   finishes.
4. It creates the bootstrap token Secret, then the Machine, then the
   `HostAllocate` `MachineOperation`, and records the operation in
   `status.operations.allocate`. Just before it creates the operation it checks
   that the offering is still `Available` and its `validUntil` has not passed.

The controller never creates the token again once the `HostAllocate` is
recorded. The recorded `operations.allocate` and `operations.release` can
never be changed or removed.

An offering is a candidate when it is `Available`, its `validUntil` is in the
future, every requested resource fits its capacity minus `kubeReserved`,
`systemReserved`, and `evictionThreshold`, and every requirement matches its
labels plus `node.kubernetes.io/instance-type`, `topology.kubernetes.io/zone`,
and `unbounded-cloud.io/capacity-type`. Each requirement with `minValues` needs
that many distinct values among the candidates. Priced offerings come before
unpriced ones, then the lower hourly price, then the name. `status.labels` are
the configuration's node labels, then `spec.nodeTemplate.labels`, then the
offering's labels, the later ones winning.

Everything the controller creates has a name derived from the allocation UID:

| Object | Name | Finalizer |
|--------|------|-----------|
| Machine, and the Node it registers | `ub-<uid without dashes>` | `unbounded-cloud.io/machineallocation` |
| `HostAllocate` operation | `<machine>-allocate` | `unbounded-cloud.io/machineallocation` |
| `HostRelease` operation | `<machine>-release` | `unbounded-cloud.io/machineallocation` |
| Bootstrap token Secret in `kube-system` | `bootstrap-token-<id>`, with a 6-character ID derived from the UID | - |

The Machine is bound to the allocation in `spec.allocationRef`, owned by it,
and has `host.external` set to the planned provider ID. It has no
`bootstrapTokenRef`. Its `kubernetes.nodeLabels` are the labels in
`status.labels` that a kubelet may set on its own Node, and its
`registerWithTaints` are the configuration taints, the node template taints,
and the startup taints.

The token Secret carries the `unbounded-cloud.io/site` label, which the daemon
CSR approver requires, and the `unbounded-cloud.io/allocation-uid` label. A
Secret with the `unbounded-cloud.io/allocation-uid` label is never chosen as a
site token, so the token refresher and `kubectl unbounded` never hand it to
another host.

An allocation retires when `spec.lifecycle` is `Released`, when it is being
deleted, when its bound Machine is being deleted, or when its consumer is
terminating or gone. A consumer is observed only for the kinds listed in
`AllocationConsumerKinds`. A retired allocation is fenced: the controller sets
`spec.fenced` on the Machine and deletes the token Secret. It never deletes a
Secret with the token's name that does not carry this allocation's
`unbounded-cloud.io/allocation-uid` label. Once the Machine is fenced, being
deleted, or gone, and the token is gone, `Fenced` is `True`. A consumer that
is lost without setting `Released` leaves the allocation fenced and holding
with `AwaitingRelease`; set `spec.lifecycle: Released` to release the host.

When release is authorized, the controller waits for the recorded
`HostAllocate` to finish, creates the `HostRelease` operation, records it in
`status.operations.release`, and waits for it. The `HostRelease` acts only on
the resources in `status.ownedResources` and those the `HostAllocate`
reported. When the `HostAllocate` may have created a host but reported no owned
resources, the controller holds with `OwnershipUnknown` instead of releasing.
When the `HostRelease` completes, the controller first sets `Released=True` in a
status write of its own. That is the commit point: from then on it only removes
its finalizers, from the operations, the Machine, and the input `ConfigMap`s,
and its own finalizer last, and it never looks at the operations again. An
allocation with no recorded `HostAllocate` finishes without a `HostRelease`. The controller does not delete the Machine
or the Node; deleting the allocation garbage-collects the Machine and the
operations it owns.

| Condition | Status | Reason | Meaning |
|-----------|--------|--------|---------|
| `Allocated` | `False` | `ClassNotReady`, `ClassInputsChanged` | Waiting for the class to resolve, or for its inputs to match its status again. Checked again every 15 seconds. |
| `Allocated` | `False` | `Launching` | The snapshot is written and the `HostAllocate` has not finished. |
| `Allocated` | `True` | `Allocated` | The `HostAllocate` completed. It does not look at Node readiness. |
| `Allocated` | `False` | `InsufficientCapacity`, `QuotaExceeded`, `InvalidRequest`, `AuthFailed`, `ProviderConflict`, `ProviderFailed` | A terminal failure, also in `status.failureReason` and `status.failureMessage`. The allocation never launches; release it with `spec.lifecycle: Released`. |
| `Allocated` | `False` | `SnapshotMissing` | Hold: the finalizer is present but `status.snapshot` is gone, so the status was lost. |
| `Registered` | `True` | `NodeRegistered` | The Node named after the Machine registered with `status.providerID`. `status.nodeRef` names the first such Node, and the Machine's `kubernetes.nodeRef` is set. |
| `Registered` | `False` | `NodeNotRegistered`, `NodeConflict` | No such Node, or a Node with that name and another `providerID`. |
| `Fenced` | `True` | `LifecycleReleased`, `AllocationDeleting`, `ConsumerTerminating`, `ConsumerGone`, `MachineDeleting` | The fence is complete, and the reason names the signal. |
| `Fenced` | `False` | `Fencing` | The token or the Machine is not yet confirmed. The message names a Secret the controller will not delete. |
| `Released` | `False` | `AwaitingRelease` | Fenced, and waiting for `spec.lifecycle: Released`. |
| `Released` | `False` | `AwaitingAllocateOperation` | Waiting for the recorded `HostAllocate` to finish. |
| `Released` | `False` | `Releasing` | Waiting for the `HostRelease`. |
| `Released` | `False` | `ReleaseFailed` | Hold: the `HostRelease` failed. |
| `Released` | `False` | `OwnershipUnknown` | Hold: the `HostAllocate` failed with `OwnershipUnknown`, or has a provider handle and no reported result, so a host may exist that release would not know about. The failure is `ProviderFailed`. |
| `Released` | `False` | `AllocateOperationMissing`, `ReleaseOperationMissing` | Hold: a recorded operation is gone or was replaced. `AllocateOperationMissing` also sets the `ProviderConflict` failure. |
| `Released` | `False` | `MachineMissing` | Hold: the Machine is gone before its host was released. |
| `Released` | `False` | `BeginOutcomeUnknown` | Hold: the `HostAllocate` may have begun and cannot be begun again. |
| `Released` | `False` | `ProviderConflict` | Hold: a `MachineOperation` with the `HostRelease` name belongs to someone else. |
| `Released` | `True` | `Released` | The host and the token are released. |

A hold keeps every finalizer and is never retried on a timer. Each needs an
operator: find and release the provider resources by hand, using the allocation
UID, `status.ownedResources`, and the operations' status, and then remove the
finalizers. Never delete or edit an allocation `MachineOperation`, and never
remove its finalizer, while the allocation exists: the operation is the record
of what the provider may have started.

### Running HostAllocate and HostRelease

`pkg/machineops/controller.AddToManager` registers a second controller,
`machineoperation-allocation`, when a selected registration declares capacity.
It is the only code that calls `HostAllocate` and `HostRelease`; the
host-operation controller still ignores them. The same manager that installs
the MachineAllocation controller must call `AddToManager` with the same
providers.

Before it begins a `HostAllocate`, the path checks that the operation is the
one the allocation recorded, that neither the allocation nor the Machine is
fenced, released, or being deleted, that the credential and the Site are the
instances in the snapshot, and that the input `ConfigMap`s, the token, and the
providerID match the snapshot. An unrecorded operation under the deterministic
name waits for the record. Any other failed check fails the operation, or,
once `Begin` may have run, holds it with the reason in the target message and
the `Completed` condition, such as `BeginOutcomeUnknown` or
`AuthorityReplaced`. A provider must report its owned resources in
`OperationResult.Allocation` on every terminal `HostAllocate` Poll result,
including failures, and its `Begin` may return a `PermanentError` only when it
created nothing. A terminal result without a report, a Poll `PermanentError`,
or a `Begin` `PermanentError` after `Begin` may already have run fails the
operation with `OwnershipUnknown`. The user data is `cloud-init` that writes the bootstrap
files to `/var/lib/unbounded/bootstrap`, runs its `run` file, and then installs
the agent, verifying `spec.bootstrap.agentArtifact.sha256` when the class sets
an artifact. The agent's kubelet registers with `status.providerID`. A
`HostRelease` begins only once `spec.lifecycle` is `Released`, the Machine is
fenced or being deleted, and the recorded `HostAllocate` has finished with
known ownership.

On a Machine bound to an allocation, `HostReplace`, `HostPowerOn`, and
`HostPowerOff` fail with `UnsupportedOperation`, and every host operation on a
fenced or deleting one fails with `MachineFenced`.

### Installing the MachineAllocation controller

```go
err := controller.AddCapacityToManager(mgr, providers, controller.CapacityOptions{
	ProviderName:               "ExampleCloud",
	SiteName:                   "remote",
	CredentialSecretNamespace:  "unbounded-system",
	ProviderConfigNamespace:    "unbounded-capacity",
	EnableAllocationController: true,
	AllocationConsumerKinds: []schema.GroupVersionKind{
		{Group: "karpenter.sh", Version: "v1", Kind: "NodeClaim"},
	},
})
```

| Option | Description |
|--------|-------------|
| `EnableAllocationController` | Installs the MachineAllocation controller next to the MachineClass controller. |
| `AllocationConsumerKinds` | The cluster-scoped consumer kinds whose termination and loss retire an allocation. Each needs `EnableAllocationController`, a `Kind` and a `Version`, and a distinct group and kind. Empty means consumer loss is never observed. Grant the controller `get`, `list`, and `watch` on each. |

`deploy/machine-ops/05-machineallocation-rbac.yaml.tmpl` defines five
ClusterRoles and no bindings:

| ClusterRole | Grants | Bind with |
|-------------|--------|-----------|
| `unbounded-machineallocation-controller` | `machineallocations` read and `patch`, `machineallocations/status` update, `machineclasses`, `sites`, `machineconfigurationversions` and `nodes` read, `machines` read, `create`, `patch` and `allocate`, and `machineoperations` read, `create` and `patch` | A ClusterRoleBinding to the allocation controller. |
| `unbounded-machineallocation-controller-provider-config` | `configmaps` `get`, `list`, `watch`, `patch` | A RoleBinding in the provider-config namespace only. |
| `unbounded-machineallocation-controller-bootstrap-tokens` | `secrets` `get`, `create`, `delete` | A RoleBinding in `kube-system` only. |
| `unbounded-machineallocation-operations` | `machineallocations` and `sites` `get` | A ClusterRoleBinding to the identity that runs `AddToManager`, with `unbounded-machineclass-controller-provider-config` bound in the provider-config namespace. |
| `unbounded-machineallocation-operations-bootstrap-tokens` | `secrets` `get` | A RoleBinding in `kube-system` only, to the identity that runs `AddToManager`. |

Install the two ValidatingAdmissionPolicies with the roles:

- `unbounded-machine-allocationref`
  (`06-machine-allocationref-policy.yaml.tmpl`) lets only an identity granted
  the `allocate` verb on `machines` create a Machine with
  `spec.allocationRef`.
- `unbounded-machineallocation-bootstrap-tokens`
  (`07-machineallocation-bootstrap-token-policy.yaml.tmpl`) limits the Secret
  creates and deletes in `kube-system` of an identity granted `allocate` on
  `machines` to bootstrap token Secrets labeled
  `unbounded-cloud.io/allocation-uid`. An identity allowed `*` on `machines`,
  such as `cluster-admin` or `system:masters`, is exempt. Never grant the
  allocation controller `*` on `machines`, because that also lifts its Secret
  limit.

Both policies fail closed. Upgrade every shared machine-ops controller older
than the one that ignores `HostAllocate` and `HostRelease`, and every agent
older than the one with the allocation launch check, before you set
`EnableAllocationController`: an older machine-ops controller treats
`HostAllocate` as a host operation, and an older agent starts a fenced host.

---

## CRD relationships

![Machine CRD relationships: Machine spec fields reference OCI Image, Secrets in unbounded-system and kube-system namespaces, with bidirectional Machine-Node link via label](../../img/machina-crd-relationships.svg)

## See Also

- **[SSH Guide]({{< relref "guides/ssh" >}})** -- SSH provisioning walkthrough
  using these CRDs.
- **[PXE Guide]({{< relref "guides/pxe" >}})** -- Bare-metal provisioning
  walkthrough using Machine and OCI netboot images.
- **[Networking CRDs]({{< relref "reference/networking/custom-resources" >}})**
  -- Site, GatewayPool, and related CRDs from unbounded-net.
- **[CLI Reference]({{< relref "reference/cli" >}})** -- The `kubectl unbounded`
  commands that create these resources.
- **[Architecture]({{< relref "reference/architecture" >}})** -- How these
  CRDs drive the provisioning pipelines.
