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
| `host.netboot.transport` | string | No | `TFTP` | Firmware artifact transport. Allowed values: `TFTP`, `HTTP`. |
| `host.netboot.configurationSource` | string | No | `DHCP` | Source of the firmware boot target. Allowed values: `DHCP`, `Redfish`. |
| `host.netboot.networkMode` | string | No | `DHCP` | Firmware provisioning network mode. Allowed values: `DHCP`, `Static`. Static requires Redfish. |
| `host.netboot.endpointRef` | string | Yes | - | Name of the `NetbootEndpoint` serving this Machine. |
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
| `host.netboot.redfish.passwordRef` | SecretKeySelector | Yes | - | Secret containing the Redfish password. |
| `host.netboot.cloudInit` | CloudInitSpec | No | - | Optional cloud-init customization for PXE-booted machines. |
| `host.netboot.cloudInit.userDataConfigMapRef` | ConfigMapKeySelector | No | - | Reference to a ConfigMap containing custom cloud-init user-data. |
| `host.netboot.cloudInit.userDataConfigMapRef.name` | string | Yes | - | ConfigMap name. |
| `host.netboot.cloudInit.userDataConfigMapRef.namespace` | string | Yes | - | ConfigMap namespace. |
| `host.netboot.cloudInit.userDataConfigMapRef.key` | string | No | `"user-data"` | Key within the ConfigMap. |

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
| `host.external.provider` | string | For external hosts | -- | Registered provider controller and credential key, such as `OCIInstance`, `ANS`, or a private provider. |
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
`MachineOperation` status directly. Long-running begin callbacks must be
idempotent for `OperationRequest.OperationUID` because the controller may call
them again until their operation handle has been persisted. `OperationRequest`
contains the exact external machine resource UID and generation, resolved host
image, and observed Machine generation frozen in target status. Providers
receive the canonical `host.external.providerID` or Azure resource ID; legacy
Machines continue to supply `Machine.spec.providerID`. Host operations targeting
the same Machine are serialized.

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
  name: remote-oci
spec:
  siteName: remote
  provider: OCIInstance
  auth:
    mode: ExternalPlugin
    secretRef:
      namespace: unbounded-system
      name: remote-oci-auth
```

## NetbootEndpoint

Cluster-scoped `NetbootEndpoint` resources declare stable client-facing
netboot addresses. A Machine references one by name through
`spec.host.netboot.endpointRef`.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec.siteRef` | string | Yes | Site whose Machines may use the endpoint. |
| `spec.type` | string | Yes | `ManagedL2`, `ExternalL2`, or `HTTP`. |
| `spec.externalURL` | string | Yes | Stable HTTP(S) base URL snapshotted into each session. |
| `spec.tls.trust` | string | Yes | `TrustedLAN` or `Public`. Public endpoints require HTTPS. |
| `spec.tls.mode` | string | Yes | `Disabled`, `Secret`, or `External`. |
| `spec.tls.secretRef` | Secret reference | For Secret mode | Namespaced TLS Secret copied to the managed edge. |
| `spec.managedL2.nodeSelector` | LabelSelector | For ManagedL2 | Selects nodes attached to the provisioning network. |
| `spec.managedL2.interface` | string | For ManagedL2 | Host interface used for DHCP and TFTP. |
| `spec.managedL2.address` | string | For ManagedL2 | Stable edge address on the provisioning network. |
| `spec.http.serviceType` | string | For HTTP | `ClusterIP`, `NodePort`, or `LoadBalancer`; defaults to `ClusterIP`. |

`ManagedL2` creates a host-network edge. `HTTP` creates replicated HTTP edge
pods and a Service. `ExternalL2` creates no in-cluster workload; an external
process claims it through status.

Status records the processed generation, current external claimant and renewal
time, and conditions. A session becomes Ready only after endpoint
`status.observedGeneration` matches its generation and `Ready=True`.

## NetbootSession

Cluster-scoped `NetbootSession` is the immutable provisioning contract for one
MachineOperation target. Metalman creates it automatically; users should not
edit or reuse sessions.

| Field | Description |
|-------|-------------|
| `spec.machine` | Exact Machine name, UID, and generation. |
| `spec.operation` | Exact MachineOperation name, UID, and generation. |
| `spec.endpoint` | Endpoint name, UID, and external URL snapshot. |
| `spec.boot` | Transport, configuration source, network mode, firmware artifact, architecture, leases, and target disk. |
| `spec.provisioning` | Cluster, Kubernetes, agent, provider-label, and resolved cloud-init inputs used for rendering. |
| `spec.artifacts` | Machine/netboot OCI references pinned to SHA-256 digests and the allowed public file names. |
| `spec.expiresAt` | Last time authenticated session requests are accepted. |

Session spec is immutable through CRD validation. Status contains the
preparation phase, signing-key identifier, endpoint readiness, and exact-target
milestones such as `BootLoaderDownloaded`, `BootImageWritten`,
`CloudInitDone`, and `Attested`. The bearer capability itself is never stored
in Kubernetes.

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
| `spec.operationKind` | string | Yes | One of `NodeReboot`, `AgentUpgrade`, `AgentReset`, `HostReboot`, `HostPowerOff`, `HostPowerOn`, `HostReplace`. |
| `spec.parameters` | map[string]string | No | Operation-specific parameters. |
| `spec.ttlSecondsAfterFinished` | int32 | No | Delete completed or failed operations after this many seconds. |
| `status.phase` | string | No | `Pending`, `InProgress`, `Complete`, or `Failed`. |
| `status.message` | string | No | Human-readable status message. |
| `status.startedAt` | time | No | Operation start timestamp. |
| `status.completedAt` | time | No | Terminal phase timestamp. |
| `status.targets` | []TargetStatus | No | Per-Machine target status, immutable input, session reference, attempts, and target-scoped conditions. Metalman records `BootLoaderDownloaded`, `BootImageWritten`, and `CloudInitDone` on the exact target. |
| `status.conditions` | []Condition | No | Operation-wide conditions such as terminal `Completed`. Provisioning milestones are target-scoped. |

`AgentUpgrade` is handled by the in-host agent and requires `spec.parameters.downloadURL`. The URL must point to an `unbounded-agent` release tarball; the agent stages it as the inactive blue/green daemon binary, records the previous binary as last known good, and restarts `unbounded-agent-daemon.service`. If systemd cannot keep the upgraded daemon running, `unbounded-agent-daemon-recovery.service` switches the daemon back to the last known good binary.

The Azure VM provider handles:

| Operation | Azure action |
|-----------|--------------|
| `HostReboot` | `VirtualMachinesClient.BeginRestart` |
| `HostPowerOff` | `VirtualMachinesClient.BeginPowerOff` |
| `HostPowerOn` | `VirtualMachinesClient.BeginStart` |
| `HostReplace` | `VirtualMachinesClient.Get`, `BeginDelete`, then `BeginCreateOrUpdate` |

`HostReplace` for `AzureVM` destructively replaces the VM: it reads the existing VM model, detaches NICs and data disks, deletes the VM resource, and recreates the same VM name with fresh cloud-init custom data that installs `unbounded-agent`. An explicit host image may be an Azure resource ID or a `publisher:offer:sku:version` reference; an omitted image preserves the existing image reference. The old OS disk is not reused. Operation completion means the replacement VM create operation completed; it does not mean the Kubernetes `Node` is Ready. The `Machine` controller continues tracking whether the Kubernetes `Node` disappears and rejoins. Configure `machine-ops-controller --api-server-endpoint` with an API server address reachable from replaced hosts; the generated agent bootstrap config uses that value.

This replacement flow avoids Azure standalone VM `customData` immutability during native reimage. It intentionally destroys host-local state on the old OS disk. The initial implementation retains the existing blocking clone-delete-create flow; a controller crash after deletion can require manual recovery because the captured VM model is not yet durably checkpointed.

The OCI instance provider handles:

| Operation | OCI action |
|-----------|------------|
| `HostReboot` | `RESET` |
| `HostPowerOff` | `STOP` |
| `HostPowerOn` | `START` |
| `HostReplace` | `STOP` old instance, `LaunchInstance` replacement, patch `Machine.spec.host.external.providerID`, then terminate old instance |

`HostReplace` for `OCIInstance` creates a replacement instance because OCI launch `user_data` is immutable after instance creation. The controller stops the old instance, launches a new instance in the same availability domain, subnet, shape, and fault domain, requests a public IP for bootstrap egress, patches `Machine.spec.host.external.providerID` to the new instance OCID after the replacement reaches `RUNNING`, and then terminates the old instance. The replacement reuses the original `Machine` name as the kubelet node name so it rejoins through the existing Kubernetes `Node` object. Operation completion means the replacement is running, provider ID handoff succeeded, and old-instance cleanup succeeded; it does not wait for the Kubernetes `Node` to become Ready.

The OCI replacement flow copies display name, defined tags, freeform tags, selected agent/availability/shape settings, and primary VNIC subnet/NSG/source-destination-check settings. It adds Unbounded freeform tags for idempotent retry lookup. It does not preserve the exact private IP, boot volume, or attached data volumes; active attached data volumes fail the operation before the old instance is stopped. An omitted host image preserves the source instance image. `spec.parameters.imageID` remains as a temporary compatibility override, while new callers should use the Machine or MachineConfiguration host image. Set `spec.parameters.sshAuthorizedKeys` to append SSH authorized keys to replacement metadata for break-glass debugging.

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
| `conditions` | []Condition | Standard Kubernetes conditions (see below). |

### Conditions

| Type | Set By | Description |
|------|--------|-------------|
| `SSHReachable` | machina | `True` / `False` based on a TCP probe to the SSH port. |
| `Provisioning` | machina | `True` while the install script is running over SSH. `lastTransitionTime` records when provisioning started, used to detect stale provisioning attempts (e.g. after a controller restart). |
| `Provisioned` | machina | `True` after successful SSH provisioning. `ObservedGeneration` tracks the spec generation. |
| `CloudInitDone` | metalman | Observed first-boot cloud-init result for PXE machines. Metalman also mirrors cloud-init progress to active `HostReplace` `MachineOperation` conditions. |

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
  operationKind: HostReboot
  ttlSecondsAfterFinished: 300
```

**OCI instance with external power operations:**

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: oci-worker-01
spec:
  host:
    external:
      provider: OCIInstance
      providerID: oci://ocid1.instance.oc1...
  configurationRef:
    name: oci-workers
```

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: oci-worker-01-poweroff
spec:
  machineRef: oci-worker-01
  operationKind: HostPowerOff
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
from the immutable NetbootSession snapshot; other files are served verbatim. A `metadata.yaml`
file in the netboot image provides image-level configuration such as
`dhcpBootImageName` and `httpBootPath`.

### Image layout

![Netboot OCI image filesystem layout under /disk/: shimx64.efi, grubx64.efi, vmlinuz, initrd, init.cpio, unbounded-agent, metadata.yaml, grub/grub.cfg.tmpl, cloud-init templates](../../img/machina-netboot-layout.svg)

### Template data

Templates receive the following data object:

| Field | Type | Description |
|-------|------|-------------|
| `.Machine` | *Machine | A synthetic Machine built from the immutable session snapshot. |
| `.BootLease` | *DHCPLease | The snapshotted provisioning lease selected for rendering. |
| `.ApiserverURL` | string | External Kubernetes API server URL. |
| `.ArtifactBaseURL` | string | Capability-scoped session artifact URL. |
| `.ServeURL` | string | Capability-scoped session base URL used by callbacks. |
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
responses for `transport: TFTP`.

The `httpBootPath` field specifies the firmware artifact used for
`transport: HTTP`. Metalman signs a session capability URL for that artifact.

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
