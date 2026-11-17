---
title: "CRD Reference"
weight: 2
description: "Field-level reference for the unbounded-cloud.io custom resources that describe machines, operations, configuration profiles, capacity classes, and sites."
---

## Overview

Unbounded describes physical and virtual hosts with custom resources in the
`unbounded-cloud.io` API group, served at version `v1alpha3`. This page is the
field-level reference for that group. It covers every kind the group serves, the
spec and status fields each kind accepts, the defaults the API server applies,
the printer columns `kubectl get` shows, and the lifecycle rules the controllers
enforce.

The mental model is a small set of layers. A `Machine` is one host and the
instructions for installing a node on it. A `MachineOperation` is one discrete
action against a Machine, such as a reboot or a host replacement. A
`MachineConfiguration` versions the provider-neutral settings that many Machines
share, and a `MachineConfigurationVersion` is one frozen snapshot of those
settings. A `MachineClass` turns a provider account and a configuration version
into a catalog of purchasable capacity, and a `MachineAllocation` is one
consumer's request for one machine from that catalog. A
`MachineOperationCredential` supplies the provider credential those capacity
controllers use, and a `Site` is the top-level location that all of it belongs
to.

Read [Architecture]({{< relref "reference/architecture" >}}) first if you want
the controller-level picture. This page assumes you know which kind you are
editing and need the exact field contract.

## API group conventions

Every kind in this group is cluster-scoped. There is no namespace in the object
name, references to Secrets and ConfigMaps carry an explicit namespace, and RBAC
is granted with ClusterRoles. The API server serves `v1alpha3` only, so manifests
that use an older version must be converted before they are applied.

The kubebuilder markers in `api/machina/v1alpha3` generate the CRDs under
`deploy/machina/crd`. Those generated files, not handwritten schemas, are what
the API server enforces, so a field that is absent from the CRD is pruned even if
a Go struct still mentions it.

`Machine`, `MachineOperation`, `MachineClass`, `MachineAllocation`,
`MachineConfiguration`, `MachineConfigurationVersion`, and `Site` expose a
`status` subresource. `MachineOperationCredential` has no status subresource; it
is a pure input object. Reports about a credential failure appear on the objects
that tried to use it, not on the credential itself.

The short names are `mach` for Machine, `mop` for MachineOperation, `mcls` for
MachineClass, `malloc` for MachineAllocation, `mc` for MachineConfiguration,
`mcv` for MachineConfigurationVersion, `mocred` for
MachineOperationCredential, and `st` for Site. Each kind also has a matching
list kind, which the API server returns for collection requests.

## Machine

`Machine` is the central resource. The machina controller uses it to install and
maintain a Kubernetes node over SSH, metalman uses it to network-boot a
bare-metal host, and the out-of-band operation controllers use it to power
cycle or replace the underlying host. A Machine can be as small as an SSH
address, a private key, and a Kubernetes version, or as complete as a
bare-metal definition with static networking, Redfish access, and a TPM-backed
bootstrap flow.

The resource is cluster-scoped with short name `mach`. It has a status
subresource and four printer columns: `Host` from `.spec.ssh.host`, `Phase` from
`.status.phase`, `K8s Version` from `.spec.kubernetes.version`, and `Age` from
the creation timestamp. The `Host` column is empty for a Machine that is
provisioned entirely by metalman, because that path does not use SSH.

The table below lists the Machine schema. Field paths are relative to the object
root. A field marked required must be present when its parent object is present.

| Field | Type | Required | Default | Notes |
| --- | --- | --- | --- | --- |
| `spec.ssh` | `SSHSpec` | No | none | SSH connection used by machina; omit for PXE-only hosts. |
| `spec.ssh.host` | `string` | Yes | none | Hostname or IP, optionally `host:port`; port 22 when omitted. |
| `spec.ssh.username` | `string` | No | `azureuser` | Login user for the target host. |
| `spec.ssh.privateKeyRef` | `SecretKeySelector` | Yes | none | Secret holding the private key; `namespace` is required but machina reads from its own namespace. |
| `spec.ssh.privateKeyRef.key` | `string` | No | `ssh-privatekey` | Key within the referenced Secret. |
| `spec.ssh.bastion` | `BastionSSHSpec` | No | none | Optional jump host. |
| `spec.ssh.bastion.host` | `string` | Yes | none | Bastion hostname or IP with optional port. |
| `spec.ssh.bastion.username` | `string` | No | `azureuser` | Bastion login user. |
| `spec.ssh.bastion.privateKeyRef` | `SecretKeySelector` | No | parent key | Falls back to the target Machine's `privateKeyRef`. |
| `spec.host` | `HostSpec` | No | none | Host ownership and provider-neutral host settings. |
| `spec.host.image` | `string` | No | none | Opaque provider image identifier; empty preserves the current image on replacement. |
| `spec.host.provisioningFormat` | `string` (`CloudInit`, `Ignition`) | No | `CloudInit` | First-boot payload the image consumes; resolution also considers the installed observation. |
| `spec.host.netboot` | `PXESpec` | No | none | Metalman network boot ownership. At most one owner may be set. |
| `spec.host.azure` | `AzureHostSpec` | No | none | Built-in Azure VM ownership. |
| `spec.host.azure.resourceID` | `string` | Yes | none | Full Azure Resource Manager ID; immutable. |
| `spec.host.external` | `ExternalHostSpec` | No | none | Ownership by a registered external provider. |
| `spec.host.external.provider` | `string` | Yes | none | Registered provider name; immutable. |
| `spec.host.external.providerID` | `string` | No | none | Opaque provider host identifier; at least one of `providerID` or `machineRef` must be set. |
| `spec.host.external.machineRef` | `ProviderMachineReference` | No | none | Provider-owned resource with rich state; immutable. |
| `spec.host.external.machineRef.apiGroup` | `string` | Yes | none | API group of the provider resource. |
| `spec.host.external.machineRef.kind` | `string` | Yes | none | Kind of the provider resource. |
| `spec.host.external.machineRef.name` | `string` | Yes | none | Name of the cluster-scoped provider resource. |
| `spec.pxe` | `PXESpec` | No | none | Deprecated legacy network boot field; new manifests use `spec.host.netboot`. |
| `spec.kubernetes` | `KubernetesSpec` | No | none | Kubernetes installation settings. |
| `spec.kubernetes.version` | `string` | No | cluster version | For example `v1.34.0`; omit to use the control plane's version. |
| `spec.kubernetes.nodeRef` | `LocalObjectReference` | No | none | Controller-managed reference to the Node; do not set by hand on an allocation host. |
| `spec.kubernetes.nodeLabels` | `map[string]string` | No | none | Passed to kubelet `--node-labels`. |
| `spec.kubernetes.registerWithTaints` | `[]string` | No | none | Passed to kubelet `--register-with-taints` in `key=value:Effect` form. |
| `spec.kubernetes.bootstrapTokenRef` | `LocalObjectReference` | No | none | Bootstrap token Secret in `kube-system`; machina requires it to provision over SSH. |
| `spec.agent` | `AgentSpec` | No | none | Settings for the unbounded node agent. |
| `spec.agent.image` | `string` | No | agent default | OCI image for the nspawn machine. |
| `spec.agent.version` | `string` | No | latest release | Agent release tag downloaded onto the host. |
| `spec.agent.baseURL` | `string` | No | GitHub releases | Base URL for the agent download; layout must match GitHub releases. |
| `spec.agent.url` | `string` | No | none | Fully qualified tarball URL; overrides `version` and `baseURL`. |
| `spec.agent.downloads` | `AgentDownloadsSpec` | No | upstream hosts | Per-artifact download overrides for kubelet, containerd, runc, CNI, crictl, and CoreDNS. |
| `spec.agent.downloads.<component>.baseURL` | `string` | No | upstream | Replaces the host and path prefix, preserving version and architecture substitution. |
| `spec.agent.downloads.<component>.url` | `string` | No | none | Fully qualified URL template; wins over `baseURL`. |
| `spec.agent.downloads.<component>.version` | `string` | No | derived | Overrides the derived artifact version. |
| `spec.agent.localDNS` | `LocalDNSSpec` | No | disabled cache | Optional CoreDNS cache inside the nspawn machine. |
| `spec.agent.localDNS.enabled` | `bool` | No | `false` | Enables the machine-local cache. |
| `spec.agent.localDNS.nodeListenerIP` | `string` | No | none | Address the cache listens on for node-local queries. |
| `spec.agent.localDNS.clusterListenerIP` | `string` | No | none | Address used for cluster DNS forwarding. |
| `spec.agent.localDNS.metricsAddress` | `string` | No | none | Metrics listener for the cache. |
| `spec.agent.localDNS.cpuLimitInMilliCores` | `int32` | No | none | CPU limit for the cache. |
| `spec.agent.localDNS.memoryLimitInMB` | `int32` | No | none | Memory limit for the cache. |
| `spec.agent.localDNS.requiredPlugins` | `[]string` | No | none | Plugins that must be present in the Corefile. |
| `spec.agent.localDNS.corefileTemplate` | `string` | No | none | Template used to render the Corefile. |
| `spec.configurationRef` | `MachineConfigurationRef` | No | none | Configuration profile for this Machine. |
| `spec.configurationRef.name` | `string` | Yes | none | MachineConfiguration name. |
| `spec.configurationRef.version` | `int32` | No | latest locked | Version number; the controller fills it when omitted. |
| `spec.isolation` | `string` (`""`, `nspawn`, `none`, `container`) | No | `""` | Declares where the node stack runs; see below. |
| `spec.provider` | `string` | No | none | Deprecated legacy external provider name; immutable once set. |
| `spec.providerID` | `string` | No | none | Deprecated legacy provider identifier. |
| `spec.allocationRef` | `UIDReference` | No | none | MachineAllocation that owns this Machine; create-time only. |
| `spec.allocationRef.name` | `string` | Yes | none | Allocation name. |
| `spec.allocationRef.uid` | `string` | Yes | none | Allocation UID; a recreated allocation is a different owner. |
| `spec.fenced` | `bool` | No | `false` | Forbids the agent from starting the node; may only change from false to true and requires `allocationRef`. |
| `status.observedProvisioningFormat` | `string` | No | none | The agent's installation observation; does not override desired replacement settings. |
| `status.isolation` | `string` | No | none | Applied isolation mode, written only by the agent. |
| `status.isolationChange` | `IsolationChangeStatus` | No | none | Last attempt to move to the declared mode during a repave. |
| `status.isolationChange.targetMode` | `string` | Yes | none | Mode the change was moving to. |
| `status.isolationChange.phase` | `string` (`InProgress`, `Succeeded`, `Failed`) | Yes | none | How far the change got. |
| `status.isolationChange.strategy` | `string` (`InPlace`, `Reprovision`) | No | none | How the change was carried out. |
| `status.isolationChange.reason` | `string` | No | none | CamelCase refusal reason such as `UnsupportedMode`. |
| `status.isolationChange.message` | `string` | No | none | Explanation, capped at 4096 bytes. |
| `status.isolationChange.lastTransitionTime` | `time` | No | none | When `phase` last changed. |
| `status.phase` | `string` | No | none | Human-facing lifecycle phase. |
| `status.message` | `string` | No | none | Additional human-facing detail. |
| `status.ssh.fingerprint` | `string` | No | none | Defined for a pinned host key fingerprint; machina does not currently write it. |
| `status.redfish.certFingerprint` | `string` | No | none | Pinned Redfish TLS certificate fingerprint. |
| `status.tpm.ekPublicKey` | `string` | No | none | TPM endorsement key recorded during attested bootstrap. |
| `status.bootstrap.credentialID` | `string` | No | none | Most recent bootstrap credential ID, in `JTI=<uuid>` form. |
| `status.bootstrap.nodeName` | `string` | No | none | Node the credential may claim. |
| `status.bootstrap.issuedAt` | `time` | No | none | When the credential was issued. |
| `status.agent.image` | `string` | No | none | Agent image that was applied. |
| `status.configuration.name` | `string` | No | none | MachineConfiguration applied at the last provisioning. |
| `status.configuration.version` | `int32` | No | none | Applied version number. |
| `status.configuration.versionName` | `string` | No | none | Applied MachineConfigurationVersion object name. |
| `status.conditions` | `[]metav1.Condition` | No | none | Latest observations; see the condition list below. |

### spec.isolation

`spec.isolation` declares where the node stack runs. The three runnable values
are `nspawn`, `none`, and `container`. `nspawn` runs containerd, kubelet, the CNI
plugins, and CoreDNS inside a systemd-nspawn machine and is the default for a
new host. `none` runs them directly on the host. `container` runs them in a
privileged OCI container and is experimental: machina provisions it, and the
agent runs it, only when the relevant controller enables experimental container
isolation and the agent configuration opts in. Machina enables it through its
config file, while metalman's `serve-pxe` subcommand and the machine-ops
controller use the `--experimental-container-isolation` flag.

An empty value is not the same as `nspawn`. Empty means no declaration: a new
host is provisioned in nspawn, while a repave leaves a running node in whatever
mode it already uses. Setting the field does not move a running node. The node
changes mode at its next repave, which the agent performs when the Node object is
deleted.

The agent writes `status.isolation` after it applies a mode. Machina compares
the declared and applied values and publishes the `IsolationDrift` condition.
Drift is not an error; it is a declaration waiting for the next repave. When the
applied mode has not been reported, the condition is absent. The controller
never acts on the condition.

If a mode change would reset and re-bootstrap the host, the agent refuses it
unless the Machine carries the annotation
`unbounded-cloud.io/allow-reprovision: "true"`. This opt-in is deliberately
separate from `spec.isolation` so that declaring a mode cannot wipe a host by
itself. Without it, the agent records `Failed` in `status.isolationChange` with
reason `ReprovisionNotAllowed`, keeps the current mode, and lets the repave
proceed. The annotation has no effect on in-place changes. A change that the
agent cannot perform at all is reported as `UnsupportedMode`; a change to an
experimental mode that is not enabled is reported as `ExperimentalNotEnabled`.
The agent does not retry a failed change on its own; the next repave tries
again.

The agent is the only writer of `status.isolation` and `status.isolationChange`.
It patches both in one merge patch so the applied mode never trails a
successful change. Machina only reads them and derives the condition.

There is one exception to the rule that the agent does not write the spec. When
the agent starts on a host that has no Machine at all, it creates a minimal
Machine and records the mode it is running in `spec.isolation`. Without that
value, the new Machine would show `IsolationDrift` on its first reconcile. The
create is the only time the agent writes `spec.isolation`; it never writes the
spec of an existing Machine. An allocation-backed host is excluded from this
path entirely, because its Machine is created by the allocation controller.

### spec.ssh

If `spec.ssh` is nil, machina skips SSH provisioning entirely. That is the
normal shape for a bare-metal Machine managed by metalman, and for an external
host whose provider installs the agent through other means. If the field is
present, `host` and `privateKeyRef` are required, and machina uses the key to
connect, upload the agent configuration, and run the installer.

`spec.ssh.host` accepts a hostname or an IP address. A port may be appended as
`host:port`; when it is absent, port 22 is used. `spec.ssh.username` defaults to
`azureuser`. `spec.ssh.privateKeyRef` names a Secret and a namespace, because
Machines are cluster-scoped and the schema requires both. Machina itself reads
the key from its own deployment namespace, which is normally `unbounded-system`,
and ignores the namespace in the reference. Set the namespace to that value so
the manifest documents where the key actually lives. The key inside the Secret
defaults to `ssh-privatekey`; the Secret may use any key name if you set `key`
explicitly.

`spec.ssh.bastion` adds a jump host. Its structure mirrors the target SSH
settings, minus a recursive bastion field. The bastion has its own `host`,
`username`, and optional `privateKeyRef`. When the bastion key reference is
omitted, machina uses the same key as the target Machine. This is the common case
where one key is authorized on both the jump host and the target.

Machina's SSH client does not verify the host key in this release. It accepts
any key and does not write `status.ssh.fingerprint`, although the field is
defined for a pinned fingerprint. Do not treat an empty fingerprint as proof
that the connection failed. If your security model requires host key
verification, enforce it at the network layer or in a fork until the field is
wired up.

{{< callout type="note" >}}
A Machine with `spec.ssh` omitted has no `Host` printer column value. That is
expected for metalman hosts; it does not mean the Machine is misconfigured.
{{< /callout >}}

### spec.host.netboot

`spec.host.netboot` owns the host through metalman. The older `spec.pxe` field
is the same schema and is still read for existing Machines, but new manifests
must use `spec.host.netboot`. The controller method that resolves the effective
configuration checks `spec.host.netboot` first and falls back to `spec.pxe`.
Because `spec.host` ownership is immutable once set, migrating from `spec.pxe`
to `spec.host.netboot` is a deliberate edit of the ownership model, not a
transparent rename.

`image` is required and is the OCI image containing the machine disk. The image
must provide `/disk/disk.img.gz`, following the kubevirt containerDisk
convention. `architecture` selects the platform manifest and defaults to
`amd64`; `arm64` is the other accepted value. `netbootImage` optionally
overrides the netboot environment. When it is omitted, metalman uses the
default netboot image compiled into its release. `pullSecretRef` and
`netbootPullSecretRef` name Docker registry credential Secrets of type
`kubernetes.io/dockerconfigjson` or `kubernetes.io/dockercfg`.

`bootProtocol` selects how metalman triggers a network boot. `PXE`, the default,
uses DHCP and TFTP bootfile options. `HTTP` uses Redfish UEFI HTTP boot with a
URL derived from the netboot image metadata. `VirtualMedia` mounts an
operation-owned bootstrap ISO through Redfish. VirtualMedia requires `redfish`,
a digest-pinned `image`, and a digest-pinned `netbootImage`; the API server
rejects the object otherwise.

`dhcpLeases` carries the static IPv4 provisioning settings. Each entry has a
required `ipv4`, `mac`, `subnetMask`, and `gateway`, plus an optional `dns` list.
PXE boot uses the leases as DHCP reservations. HTTP boot uses the first entry to
configure the Redfish UEFI HTTP boot client. `holdLease`, when true, makes the
installed host run a DHCPv4 client on the lease's NIC so the site DHCP server
keeps the address reserved. The static address, gateway, and DNS remain
authoritative; routes, DNS, NTP, hostname, and domains learned from DHCP are
ignored. Leave it unset on a network without a DHCP server. Host network
declarations do not support `holdLease` on the first lease.

`targetDisk` names the block device the installer writes. Examples are
`/dev/nvme0n1`, `/dev/sda`, and a `/dev/disk/by-id/...` path. When it is
omitted, the installer chooses a target disk automatically. Set it explicitly
when a host has more than one candidate disk and the choice must be stable.

`network` declares the host's static topology. It has three lists and a DNS
list. `ethernets` is required when `network` is present, holds between 1 and 16
entries, and is keyed by `name`. Each entry matches a physical interface by its
permanent lowercase MAC address and carries the persisted interface name,
optional MTU (576 to 9216), up to eight static IPv4 addresses in CIDR form, an
optional default gateway, and up to 32 static routes with `to` and `via`.
`bonds` holds up to eight link aggregation interfaces, keyed by `name`, each
with a member list of ethernet names, a mode (`802.3ad` by default,
`active-backup`, or `balance-xor`), an MII monitor interval in milliseconds
(default 100), an optional transmit hash policy, and an optional LACP rate that
requires `802.3ad`. `vlans` holds up to 32 802.1Q interfaces, keyed by `name`,
each with a VLAN ID from 1 to 4094 and a `link` naming an ethernet or bond.
`dns` accepts up to three IPv4 nameservers.

Network declarations require `bootProtocol: VirtualMedia`, at least one
`dhcpLeases` entry, and a netboot image whose metadata advertises the local
installer format. The first lease remains the provisioning identity: its IPv4
address and gateway must be configured on exactly one declared interface, and
its MAC must be one of the declared ethernets. Metalman validates the
cross-references (bond members must be declared ethernets, VLAN links must be
declared ethernets or bonds) before it builds media. The installer brings the
topology up before fetching install instructions and persists it as netplan and
systemd-networkd configuration on the installed host.

`redfish` gives metalman out-of-band control. `url` is the Redfish endpoint and
may include a per-system path prefix such as `https://manager:8080/7`; do not
append `/redfish/v1`. `username` is required. `deviceID` defaults to `1` and is
ignored when the endpoint exposes a single system selected by its URL.
`passwordRef` names a Secret, a namespace, and a key; the key defaults to
`password`. The TLS certificate fingerprint is pinned in
`status.redfish.certFingerprint` on first connection.

`cloudInit.userDataConfigMapRef` points at a ConfigMap holding operator-managed
cloud-init user data. `name` and `namespace` are required and `key` defaults to
`user-data`. The value may be in `data` or `binaryData`. Unbounded merges its own
vendor data with this user data, so the ConfigMap controls only the user-data
portion. `cloudInitTimeout` bounds the wait from the installer reboot handoff
until first-boot cloud-init finishes and the Node registers, including firmware
POST. It must be a positive duration and defaults to 20 minutes. Large GPU
servers can spend most of that budget in POST alone.

### spec.kubernetes

`spec.kubernetes.version` is the Kubernetes version to install, such as
`v1.34.0`. The `v` prefix is conventional and is stripped where the installer
needs a bare version. When the field is omitted, the controller falls back to
the cluster's Kubernetes version.

`spec.kubernetes.nodeRef` is the link from the Machine to its Node. Machina and
the MachineAllocation controller write it; on an allocation host the controller
sets it once the Node registers, because the daemon CSR approver needs the
binding for certificate renewal. Do not set it by hand unless you are repairing
a Machine whose Node name is known.

`spec.kubernetes.nodeLabels` is passed to kubelet's `--node-labels` flag and
lands on the Node at registration. The values must be valid label values.
`spec.kubernetes.registerWithTaints` is passed to `--register-with-taints` and
uses the standard `key=value:Effect` form.

`spec.kubernetes.bootstrapTokenRef.name` names a bootstrap token Secret in
`kube-system`. The Secret must be of type `bootstrap.kubernetes.io/token` and
carry the well-known `token-id` and `token-secret` keys. Machina requires this
reference to provision a Machine over SSH and moves the Machine to `Failed` with
an explanatory message when it is missing or unreadable. The Site token
refresher maintains site-scoped tokens for hosts that register themselves.

### spec.host

`spec.host` contains provider-neutral host settings and at most one owner. The
three ownership forms are `netboot`, `azure`, and `external`. They are mutually
exclusive. Once one is set it cannot be removed, so changing ownership means
creating a new Machine. The legacy `spec.pxe`, `spec.provider`, and
`spec.providerID` fields cannot be combined with a `spec.host` owner; the API
server rejects that mix.

`spec.host.image` is an opaque identifier interpreted by the provider. For
Azure it is an image reference; for an external provider it is whatever that
provider accepts; for netboot it is not the boot image. If it is omitted,
`HostReplace` preserves the host's current image. `provisioningFormat` declares
whether the image consumes cloud-init or Ignition. When an image is preserved,
omission falls back to the installed observation and then to `CloudInit` for
legacy hosts. An explicit replacement image on a known Ignition host requires a
format.

An external provider declares the provider-owned resource kind it accepts with
`WithProviderMachineKind` when it calls `NewProvider`. If `machineRef` names a
different API group or kind, the operation fails before any provider call,
because the controller checks the reference against the registration. A
provider that declares no kind does not accept `machineRef` at all, and the
Machine must use `providerID` instead.

`HostReplace` is the destructive authorization in this model. It is a
`MachineOperation` kind, not a flag on the Machine, and it may delete and
recreate the host. The provider decides the mechanics. The Azure provider
detaches network interfaces, deletes the OS disk, detaches data disks, deletes
the VM, and creates a replacement with the same name and the requested image and
user data. An external provider performs the equivalent through its own API.
Because the effect is destructive, `HostReplace` is not accepted on a Machine
that is bound to a MachineAllocation.

### spec.allocationRef and spec.fenced

`spec.allocationRef` binds a Machine to the `MachineAllocation` that created it.
It carries both a name and a UID because a deleted and recreated allocation with
the same name is a different owner. The field can be set only when the Machine
is created. The CRD enforces this with a rule on the object root, not on `spec`,
because a rule on the spec node is skipped when an update drops the entire spec.
A separate ValidatingAdmissionPolicy requires the `allocate` verb on
`machines` for any identity that sets the field, which prevents an ordinary
Machine creator from binding a Machine to someone else's allocation.

`spec.fenced` forbids the agent from starting the Machine's node again. It can
only move from false to true, and it requires `allocationRef`. The API server
reports `fenced cannot be unset` when an update tries to clear it and
`fenced requires allocationRef` when a Machine tries to fence without an
allocation. Fencing never releases infrastructure by itself; it stops the node
from coming back while the allocation controller performs release.

The agent enforces fencing from inside the host. Every start of an
allocation-backed node runs a launch check, and the check refuses when the
Machine does not bind the allocation UID, is fenced, or is being deleted. The
check prefers a stored daemon-controller certificate and falls back to a
bootstrap token review; it never falls back in the other direction. A final
refusal, where the cluster answered no, stops the start and is not retried. A
transient failure is retried every 10 seconds for up to 5 minutes before the
check refuses anyway. The refusal is fail-closed: if the host cannot confirm
that it is allowed to start, it does not start. This matters because a fenced
host that kept running would continue to consume the provider resource that the
allocation is trying to release.

In `none` isolation mode, the kubelet start is gated by
`unbounded-kubelet-launch-check.service`, which runs the same check before every
kubelet start. In `nspawn` mode, the check runs as the machine's pre-start hook.
A final refusal exits with a dedicated status that systemd does not restart.
After a repave, the agent also checks the isolation mode before starting. A host
that is allocation-backed never self-registers a Machine; its identity comes
from the allocation, and a Machine created by the host would not carry the
allocation reference.

### Conditions

Machine conditions follow the standard `metav1.Condition` shape, with `type`,
`status`, `reason`, `message`, `lastTransitionTime`, and `observedGeneration`.
The controller and the agent publish the following types.

`Provisioning` is true while machina is installing the node. Its
`lastTransitionTime` records when the attempt began, which lets the controller
detect an attempt that outlived its timeout after a restart.

`Provisioned` is true after a successful installation. Its
`observedGeneration` records which generation of the Machine spec was
provisioned, so a later spec edit is visibly newer than the condition.

`SSHReachable` reports the result of the SSH probe. Its transition time and
message are updated on probe results.

`AgentBootstrapped` reports the agent's first node bootstrap. It is false with
reason `Running` while the agent prepares the nspawn node, false with a failure
reason when nspawn startup or kubelet TLS bootstrap fails, and true with reason
`Succeeded` once the kubelet has bootstrapped.

`NodeUpdated` is defined to report a node update performed by the agent daemon,
with reason `InProgress` during the update, `Succeeded` after it, and `Failed`
when it fails. The agent in this worktree does not publish it, so do not depend
on it to observe an update.

`ConfigurationPending` is true when no MachineConfiguration is assigned, either
directly through `spec.configurationRef` or through a matching
`machineSelector`. The Machine waits in that state until a configuration is
assigned. It becomes false with reason `Resolved` once a version is selected.

`IsolationDrift` is true when `spec.isolation` differs from the applied
`status.isolation`. Its reasons include `AppliedModeDiffers`, `NoModeDeclared`,
`ChangeInProgress`, `ChangeRefused`, `ChangeFailed`, `ChangeStale`, and
`ExperimentalModeDisabled`. The condition is absent until the agent reports an
applied mode, and it is removed when the applied mode is cleared.

`RepavePending` is true when the desired configuration version has not yet been
applied to the running Node. For an `OnDelete` update strategy it stays true
until the operator deletes the Node and the agent repaves with the desired
version.

### Phase lifecycle

`status.phase` is for human consumption and follows the state machine rather
than driving it. The API defines `Pending`, `Provisioning`, `Joining`, `Ready`,
`Failed`, and `Rebooting`.

A Machine with no configuration or no matching configuration starts in
`Pending`. The configuration binding controller sets `Pending` only when the
phase is still empty, so it cannot push a healthy Machine back onto the
provisionable path. Machina requeues a Pending Machine every 30 seconds. It
provisions from `Pending`, `Failed`, `Ready` that was never provisioned, or an
empty phase. It does not provision from `Joining`.

`Provisioning` is written before the installer runs. If the controller restarts
mid-attempt, the `Provisioning` condition's transition time bounds how long it
waits before declaring the attempt stale and moving the Machine to `Failed`.
Requeueing during that wait uses the same 30-second interval.

`Joining` is written after a successful installation. It means the node stack
is installed and machina is waiting for the Node object to appear. The requeue
interval is 30 seconds. `Ready` is written once the Machine is reachable and
either has no Kubernetes configuration or its Node is present and healthy. The
requeue interval is 5 minutes. `Failed` carries a message and is retried
every minute, so a transient failure recovers without an edit. `Rebooting` is
defined in the API for a host that is cycling, but machina does not currently
write it; reboot operations are tracked on the MachineOperation instead.

### Labels and annotations

`unbounded-cloud.io/site` is the canonical site-membership label. It scopes a
Machine to a Site and is also the label the net controller applies to Nodes.
The legacy `net.unbounded-cloud.io/site` key is still accepted in a few paths,
but a Machine that carries both keys with different values is rejected as
ambiguous.

`unbounded-cloud.io/provider` is an annotation that associates a Machine with a
provider controller for reboot and repave operations. It is an annotation rather
than a label because it names a controller registration, not a selectable
attribute.

`unbounded-cloud.io/machine-configuration-version` is set on the Kubernetes Node
object to record which MachineConfigurationVersion bootstrapped the machine. It
is informational and is not used to select the next version.

`unbounded-cloud.io/allow-reprovision` is the Machine annotation that permits an
isolation change which resets the host. See `spec.isolation` above.

`unbounded-cloud.io/default-bootstrap-token` is set on a bootstrap token Secret
to mark it as usable beyond a single Site. The CSR approver accepts a token for
node bootstrap when the token carries a site label or this label set to `true`.
The default token is a fallback for clusters that have not adopted site-scoped
tokens, and site-scoped tokens remain the recommended shape.

Allocations add their own labels, which are covered under MachineAllocation.

### Examples

A minimal SSH Machine needs an address, a key reference, a token, and a
Kubernetes version. The token Secret is created separately in `kube-system`.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: edge-01
  labels:
    unbounded-cloud.io/site: edge
spec:
  ssh:
    host: 203.0.113.10
    username: azureuser
    privateKeyRef:
      name: edge-01-ssh
      namespace: unbounded-system
  kubernetes:
    version: v1.34.0
    nodeLabels:
      node-role.kubernetes.io/edge: ""
    bootstrapTokenRef:
      name: bootstrap-token-abcdef
```

Add a bastion when the host is only reachable through a jump host. The bastion
can reuse the target key by omitting its own `privateKeyRef`.

```yaml
spec:
  ssh:
    host: 10.0.0.21
    username: azureuser
    privateKeyRef:
      name: edge-01-ssh
      namespace: unbounded-system
    bastion:
      host: 203.0.113.5:2222
      username: jump
```

An Azure Machine names the VM by its full resource ID. Operations against the
VM use the built-in `AzureVM` provider and the credential selected by the
Machine's site label.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: gpu-01
  labels:
    unbounded-cloud.io/site: west
spec:
  host:
    azure:
      resourceID: /subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/gpu/providers/Microsoft.Compute/virtualMachines/gpu-01
  kubernetes:
    version: v1.34.0
    bootstrapTokenRef:
      name: bootstrap-token-abcdef
```

A bare-metal Machine uses `spec.host.netboot` and omits SSH. Metalman boots the
installer from the netboot image and writes the machine image to the target
disk. The Redfish block gives the controller power control for the replacement
flow.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: metal-01
  labels:
    unbounded-cloud.io/site: dc1
spec:
  host:
    netboot:
      image: ghcr.io/azure/host-ubuntu2404:v1
      architecture: amd64
      bootProtocol: PXE
      targetDisk: /dev/nvme0n1
      dhcpLeases:
        - ipv4: 192.0.2.21
          mac: aa:bb:cc:dd:ee:21
          subnetMask: 255.255.255.0
          gateway: 192.0.2.1
          dns:
            - 192.0.2.2
      redfish:
        url: https://bmc-01.example.com
        username: admin
        passwordRef:
          name: bmc-01
          namespace: unbounded-system
  kubernetes:
    version: v1.34.0
    bootstrapTokenRef:
      name: bootstrap-token-abcdef
```

## MachineOperation

`MachineOperation` is a Job-like request to perform one action against one
Machine or a label-selected set of Machines. A client or controller creates the
object, the appropriate executor claims it, and progress is reported in status.
The resource is cluster-scoped with short name `mop`, has a status subresource,
and prints `Machine` from `.spec.machineRef`, `Operation` from
`.spec.operationKind`, `Phase` from `.status.phase`, and `Age`.

The executors are split by operation kind. The in-host agent handles
`NodeReboot`, `AgentUpgrade`, `AgentReset`, and `HostSoftReboot`. The shared
machine-ops controller handles out-of-band host operations through a registered
provider. Metalman handles the same host operations for bare-metal Redfish
Machines that it owns. The Azure VM provider is the built-in out-of-band
provider, and external providers register their own handlers. `HostAllocate` and
`HostRelease` are different: they act on a MachineAllocation and are processed
only by the capacity path, never by the agent, metalman, or the shared host
operation controller.

The operation kinds are enumerable, so they are listed here rather than in the
resource table.

- `NodeReboot`, executed by the agent, restarts the node stack in place without
  reprovisioning.
- `AgentUpgrade`, executed by the agent, stages a new host-side agent binary and
  restarts the daemon from it.
- `AgentReset`, executed by the agent, removes the agent and its resources from
  the host.
- `HostHardReboot`, executed by a provider or metalman, power cycles the host
  through its BMC.
- `HostSoftReboot`, executed by the agent, reboots the host operating system with
  `systemctl reboot`.
- `HostPowerOff`, executed by a provider or metalman, powers the host off out of
  band.
- `HostPowerOn`, executed by a provider or metalman, powers the host on out of
  band.
- `HostReplace`, executed by a provider or metalman, replaces the host and
  reinstalls the agent so the node can rejoin.
- `HostAllocate`, executed by the capacity operation path, launches the host for
  an allocation.
- `HostRelease`, executed by the capacity operation path, releases the host and
  the provider resources the allocation owns.

The table below lists the MachineOperation schema. The `targets` list is the
per-Machine execution record. It is a map keyed by `machineRef`, so a selector
operation has one entry per matched Machine and a single-Machine operation has
one entry.

| Field | Type | Required | Default | Notes |
| --- | --- | --- | --- | --- |
| `spec.machineRef` | `string` | No | none | Name of the target Machine; one of this or `machineSelector` must be set. |
| `spec.machineSelector` | `metav1.LabelSelector` | No | none | Selects Machines by label; the match set is snapshotted at execution. |
| `spec.operationKind` | `string` | Yes | none | One of the kinds listed above. |
| `spec.parameters` | `map[string]string` | No | none | Executor-specific key-value input. |
| `spec.ttlSecondsAfterFinished` | `int32` | No | keep forever | Deletes a terminal operation this many seconds after it finishes; minimum 0. |
| `spec.allocationRef` | `UIDReference` | No | none | Required for `HostAllocate` and `HostRelease`, forbidden for every other kind, immutable. |
| `spec.allocationRef.name` | `string` | Yes | none | Allocation name. |
| `spec.allocationRef.uid` | `string` | Yes | none | Allocation UID; a recreated allocation is a different target. |
| `status.phase` | `string` | No | none | `Pending`, `InProgress`, `Complete`, or `Failed`. |
| `status.message` | `string` | No | none | Human-readable state. |
| `status.startedAt` | `time` | No | none | When execution began. |
| `status.completedAt` | `time` | No | none | When the operation reached a terminal phase. |
| `status.observedMachineGeneration` | `int64` | No | none | Machine generation seen when execution began. |
| `status.targets` | `[]MachineOperationTargetStatus` | No | none | Per-Machine state, keyed by `machineRef`. |
| `status.targets[].machineRef` | `string` | Yes | none | Targeted Machine name. |
| `status.targets[].phase` | `string` | No | none | Per-target phase. |
| `status.targets[].stage` | `string` | No | none | Operation-specific stage, such as `PoweringOff` or `WaitingCloudInit`. |
| `status.targets[].message` | `string` | No | none | Per-target progress or failure detail. |
| `status.targets[].startedAt` | `time` | No | none | When this target began. |
| `status.targets[].completedAt` | `time` | No | none | When this target became terminal. |
| `status.targets[].observedGeneration` | `int64` | No | none | Target Machine generation acted on. |
| `status.targets[].bootID` | `string` | No | none | Kernel boot ID observed before a `HostSoftReboot`. |
| `status.targets[].attempts` | `int32` | No | none | External action attempts; polling does not increment it. |
| `status.targets[].lastAttemptAt` | `time` | No | none | When the latest external action was attempted. |
| `status.targets[].input` | `MachineOperationTargetInput` | No | none | Frozen controller-resolved input for this target. |
| `status.targets[].input.providerRef` | `ProviderMachineSnapshot` | No | none | Exact provider resource the target referenced at initialization. |
| `status.targets[].input.providerRef.apiGroup` | `string` | Yes | none | Provider resource API group. |
| `status.targets[].input.providerRef.kind` | `string` | Yes | none | Provider resource kind. |
| `status.targets[].input.providerRef.name` | `string` | Yes | none | Provider resource name. |
| `status.targets[].input.providerRef.uid` | `string` | Yes | none | Provider resource UID. |
| `status.targets[].input.providerRef.generation` | `int64` | Yes | none | Provider resource generation whose inputs were observed. |
| `status.targets[].input.hostImage` | `string` | No | none | Frozen image for `HostReplace`; empty preserves the current image. |
| `status.targets[].input.provisioningFormat` | `string` | No | none | Frozen format paired with `hostImage`. |
| `status.targets[].providerOperation` | `ProviderOperationStatus` | No | none | Resumable provider handle. |
| `status.targets[].providerOperation.provider` | `string` | Yes | none | Provider that accepted the operation. |
| `status.targets[].providerOperation.operationID` | `string` | Yes | none | Provider-assigned operation ID. |
| `status.targets[].providerOperation.resumeToken` | `string` | No | none | Opaque resume data; must not contain secrets. |
| `status.targets[].bootMedia` | `MachineOperationBootMediaStatus` | No | none | Immutable media ownership and cleanup progress. |
| `status.targets[].conditions` | `[]metav1.Condition` | No | none | Target-scoped conditions that are not copied to the Machine. |
| `status.allocation` | `MachineOperationAllocationResult` | No | none | Provider result for `HostAllocate`; immutable once set. |
| `status.allocation.providerResourceID` | `string` | No | none | Provider-native host identifier. |
| `status.allocation.instanceType` | `string` | No | none | Launched instance type. |
| `status.allocation.zone` | `string` | No | none | Launched zone. |
| `status.allocation.image` | `string` | No | none | Launched host image. |
| `status.allocation.ownedResources` | `[]ProviderOwnedResource` | No | none | Confirmed provider resources, keyed by `id`. |
| `status.conditions` | `[]metav1.Condition` | No | none | Operation-level observations. |

### Operation lifecycle and status

A new operation begins with an empty phase or `Pending`. The executor moves it
to `InProgress` before it performs the external action, and then to `Complete`
or `Failed`. `status.startedAt` and `status.completedAt` bracket the work.
`status.observedMachineGeneration` records the Machine generation that the
operation acted on, so a client can tell whether a spec edit landed before or
after execution.

`spec.parameters` is an untyped string map interpreted by the executor. It is
deliberately simple. `AgentUpgrade` requires a `downloadURL` parameter and
accepts an optional `sha256` parameter. The agent downloads the tarball, verifies
the digest when one is supplied, stages the binary, and restarts the daemon from
it. The operation stays `InProgress` across that restart. A pending signal file
records the operation name and the observed Machine generation so the daemon
that starts in the new slot can report the result, and a recovery path reports
it if the new daemon cannot start. That two-slot arrangement is the blue/green
recovery: the previous binary remains available as the last-good fallback, and a
candidate that cannot run every isolation mode present on the host is rejected
before the switch.

`spec.ttlSecondsAfterFinished` makes the controller delete a terminal operation
after the given number of seconds. It has no effect on `HostAllocate` and
`HostRelease`, where the allocation owns the operation's lifetime and the API
server rejects the field. When it is unset, a finished operation is kept until
someone deletes it.

`spec.machineSelector` selects Machines by label. A selector operation runs as
one operation with one entry in `status.targets` per matched Machine. The match
set is snapshotted when execution begins and remains authoritative even if the
selector matches change later. That avoids a situation where a label edit midway
through a rollout changes the set of hosts half way through. The external power
controller leaves selector operations alone, and metalman executes them. Metalman
requires the selector to resolve to a single site, because one operation must not
span sites.

### Host operation execution

The shared machine-ops controller turns a Machine and its host ownership into a
provider request. The request carries the Machine name, UID, and generation, the
operation name and UID, the host `providerID`, the resolved `hostImage`, the
operation kind, the resolved credential, and, for HostAllocate, the allocation
snapshot. A provider is built with `NewProvider` and handed to the controller
through `AddToManager`; the same registration can also carry capacity callbacks.
A provider implements either an immediate operation or a resumable Begin/Poll
pair. Before execution, the controller initializes the target input
and persists it. The input freezes the provider resource reference, the host
image, and the provisioning format, so retries do not silently adopt a Machine
edit made during execution.

An out-of-band host operation and a `HostSoftReboot` do not run at the same
time. The agent will not start a `HostSoftReboot` while a host operation on the
same Machine is unfinished. The host operation, in turn, does not wait for a
`HostSoftReboot`; the agent waits for the host operation. This ordering avoids a
power action racing a graceful reboot.

The Azure provider maps operations to Azure actions. `HostHardReboot` restarts
the VM, `HostPowerOff` powers it off, `HostPowerOn` starts it, and `HostReplace`
performs a destroy-and-recreate. The replacement path reads the existing VM
model, detaches its network interfaces, marks the OS disk for deletion and data
disks for detach, updates the model with those delete options, deletes the VM,
and creates a replacement with the same name, the supplied user data, and the
requested image. The original VM model is required: if the VM is already gone,
the operation fails instead of guessing a replacement shape. Custom data is
validated against the Azure limit before any destructive call.

Metalman handles the same four host operations for a bare-metal Redfish Machine
that it owns. Its scope is narrower: a target that is not a bare-metal Redfish
Machine fails with `UnsupportedTarget`, and a Machine that another metalman
instance owns fails rather than being acted on. Metalman advances the operation
through power and boot stages and reports `BootLoaderDownloaded`,
`BootImageWritten`, and `CloudInitDone` as it observes the host.

### Allocation operations

`HostAllocate` launches a host for the allocation named by
`spec.allocationRef`. The operation is created by the allocation controller
under a deterministic name, and its spec is immutable. The capacity path checks
that the operation is the one the allocation recorded before it calls the
provider. An unrecorded operation under the deterministic name waits while the
allocation controller may still record it; any other unrecorded operation
fails. This check prevents a client from injecting a second launch under a name
the allocation might later adopt.

`HostRelease` releases the host and the provider resources the allocation owns.
It may not begin until the lifecycle is `Released`, the Machine is fenced or
being deleted, the recorded `HostAllocate` is terminal, and that allocate
operation's ownership is known. If the allocate operation ended with
`OwnershipUnknown`, release holds instead of deleting resources it cannot prove
belong to the allocation. A release that fails for a credential reason can be
retried after the credential is repaired; a terminal release is not replaced.

The provider result for `HostAllocate` is written to `status.allocation` with
the terminal target phase, before the operation phase. It is accepted only on a
`HostAllocate` and cannot change or be removed once set. A failure can still
carry owned resources, because a partially created set is exactly what release
needs to clean up.

### Conditions

`Completed` is the terminal condition. Its reason carries the outcome, such as
`Succeeded`, a provider failure reason, or an allocation hold reason. A crash
between the target write and the operation phase write keeps the reason because
the reason is written with the terminal target.

`BootLoaderDownloaded` is set by metalman when a target first fetches the PXE
boot loader. Once true for an operation it stays latched.

`BootImageWritten` is set when the installer has written the boot image to disk.
`CloudInitDone` is set when the installed OS reports a final cloud-init result.
A failed cloud-init also proves the host left the installer and booted from disk,
which is why release logic treats it as a successful handoff for media cleanup.

`ProviderOperationStalled` reports that an accepted external provider operation
has exceeded its expected duration. It is informational; it does not cancel the
provider operation.

`status.targets[].bootMedia` is the media ownership record for a target. It
carries the media ID, source server and source name, phase, Machine UID, the
netboot spec, the certificate fingerprint, the image-written and cleanup flags,
and the timestamps and attempt counter that bound retries. It exists so a
controller crash does not orphan a mounted ISO or lose track of a host that is
still running the installer.

Target conditions are separate. They are stored under `status.targets[].conditions`
and are not copied to the Machine. For example,
`RedfishDisableBootOverrideUnsupported` records that a BMC could not disable its
boot override, so metalman fell back to `Hdd` or `Continuous` for that target
and that operation only. Keeping it target-scoped avoids turning a per-BMC
quirk into a permanent claim about the Machine.

## PXE OCI Images

The network boot path uses two OCI images with different jobs. The machine image
is the installed operating system. It is referenced by
`spec.host.netboot.image` and must contain `/disk/disk.img.gz`. The netboot image
is the installation environment. It is referenced by
`spec.host.netboot.netbootImage` and contains the bootloader, kernel, initrd,
installer, and the Go templates metalman renders per host. Keeping the two
separate lets you change the installer without rebuilding every machine image,
and lets you use one machine image for many host models.

`architecture` selects the platform manifest for both images. The value is
`amd64` or `arm64` and defaults to `amd64`. The OCI image must be a multi-arch
index or a single-platform image for the requested architecture. A mismatch
between the Machine's architecture and the image manifest fails at image
resolution, before any host is touched.

Both images are built `FROM scratch` and place their content under `/disk/`.
That is the kubevirt containerDisk convention: the runtime unpacks the layer and
exposes `/disk` as the image's payload. The machine image needs only
`/disk/disk.img.gz`. The netboot image needs the boot artifacts, the installer
initramfs content, `metadata.yaml`, a `grub/grub.cfg.tmpl`, and the cloud-init
templates under `cloud-init/`. Files whose names end in `.tmpl` are Go templates
and are rendered; every other file is served byte for byte.

The netboot image's `metadata.yaml` is read before any boot is served. It has
four fields. `dhcpBootImageName` is the boot filename sent in DHCP option 67.
`httpBootPath` is the path relative to metalman's HTTP artifact server used for
UEFI HTTP boot. When `httpBootPath` is empty, metalman falls back to
`dhcpBootImageName`. `mediaFormat` gates the scoped installer callback and
bootstrap layout; VirtualMedia requires `unbounded-uefi-v1`.
`localInstaller` advertises an installer that boots from local media and brings
up the declared host network itself; a Machine that sets `network` requires
`unbounded-local-installer-v1`.

A typical `metadata.yaml` for an amd64 netboot image looks like this. The exact
filenames follow the shim and GRUB layout in `/disk/`.

```yaml
dhcpBootImageName: shimx64.efi
httpBootPath: bootx64.efi
mediaFormat: unbounded-uefi-v1
localInstaller: unbounded-local-installer-v1
```

### Image layout

The machine image contains one artifact:

```text
/disk/disk.img.gz
```

The netboot image contains the boot environment and the templates:

```text
/disk/vmlinuz
/disk/initrd
/disk/init.cpio
/disk/shimx64.efi
/disk/grubx64.efi
/disk/bootx64.efi
/disk/metadata.yaml
/disk/grub/grub.cfg.tmpl
/disk/cloud-init/meta-data.tmpl
/disk/cloud-init/network-config.tmpl
/disk/cloud-init/vendor-data.tmpl
```

The `init.cpio` archive is the local installer overlay. It carries the modules
needed for disk, network, bonding, VLAN, and filesystem setup, plus the `ip`
binary the installer uses when the initrd's own copy lacks bond or VLAN support.
`grub.cfg.tmpl` renders the kernel command line and, when no install is pending,
chainloads the locally installed bootloader. `vendor-data.tmpl` writes the agent
configuration and install script, and `network-config.tmpl` renders either the
declared host netplan or the single-NIC lease configuration.

### Template data

Templates are rendered with a fixed data map. The fields are:

| Name | Meaning |
| --- | --- |
| `.Machine` | The Machine object being provisioned. |
| `.BootLease` | The lease selected for the request, or nil when none matches. |
| `.DHCPHoldLease` | True when the selected lease sets `holdLease`. |
| `.ApiserverURL` | API server URL from the cluster-info ConfigMap. |
| `.ServeURL` | Base URL of metalman's artifact server. |
| `.AgentConfigJSON` | Rendered agent configuration as JSON. |
| `.InstallScript` | The agent install script. |
| `.InstallEnv` | Environment assignments for the install script. |
| `.InstallRequested` | True when an install is pending. |
| `.VirtualMedia` | True when serving a bootstrap ISO. |
| `.ArtifactRoot` | URL prefix for kernel and initrd artifacts. |
| `.MediaCallbackURL` | Callback URL for the virtual media installer. |
| `.HostNetplan` | Declared host network as netplan version 2, or empty. |

The machine templates use `.Machine` and `.BootLease` heavily. For example, the
GRUB template passes `unbounded.boot_mac` from `.BootLease.MAC`,
`unbounded.disk` from `.Machine.Spec.Netboot.TargetDisk`, and the node identity
from `.Machine.Name` and `.Machine.Namespace`. The installer reads those kernel
parameters. `unbounded.boot_mac` is required when a persistent static network is
declared, because the installer matches the physical NIC by MAC rather than by
the kernel's installation-time name. The template can also emit
`unbounded.dhcp_hold=1` when the lease asks the installed host to keep its DHCP
reservation, and can pass the lease's DNS servers.

The template data does not expose the Kubernetes version or cluster DNS as
top-level fields in this release. Those values reach the host through
`.AgentConfigJSON`, which carries the cluster endpoint and version. A custom
template that needs them should read the agent configuration or a value it
writes into the Machine, not a template variable that is not present.

### Building images

Build both images from the repository root, because the Containerfiles copy
`LICENSE` and `NOTICE` from the build context. Replace the registry, owner, and
tag placeholders with values your cluster can pull.

```bash
podman build -f images/host-ubuntu2404/Containerfile \
  -t ghcr.io/<owner>/host-ubuntu2404:<tag> .
podman build -f images/netboot/Containerfile \
  -t ghcr.io/<owner>/netboot:<tag> .
```

For an arm64 netboot image, select the arm64 platform. The default Ubuntu kernel
works for most arm64 hosts. Hosts with a Microsoft MANA NIC need the Azure Linux
installer kernel, which the Containerfile exposes as a build argument, and Secure
Boot must be off because the Ubuntu shim does not trust that kernel.

```bash
podman build --platform linux/arm64 \
  --build-arg INSTALLER_KERNEL=azurelinux \
  -f images/netboot/Containerfile \
  -t ghcr.io/<owner>/netboot-azurelinux:<tag> .
```

Push the images after building them, or build directly into a registry that
your cluster can reach.

```bash
podman push ghcr.io/<owner>/host-ubuntu2404:<tag>
podman push ghcr.io/<owner>/netboot:<tag>
```

The host image definitions live in `images/host-ubuntu2404/` and the netboot
definition in `images/netboot/`. The repository's image workflow builds these
on a tag of the form `images/<name>/<version>` and on manual dispatch, and it
publishes multi-arch images for `linux/amd64` and `linux/arm64`. A local build is
enough when you are iterating on a template, because metalman reads whatever
image reference the Machine names.

### metadata.yaml details

`dhcpBootImageName` is the filename the DHCP server offers as option 67. It is
resolved inside the netboot image's `/disk/` directory. For an amd64 Ubuntu
netboot image it is the signed shim, for example `shimx64.efi`; for arm64 it is
the arm64 shim. The DHCP server serves the file over TFTP, so the name must match
a file that exists in the image.

`httpBootPath` is the path appended to metalman's HTTP serve URL for UEFI HTTP
boot. A leading slash is stripped. If the field is empty, metalman uses
`dhcpBootImageName` instead, so an image that only supports PXE does not need to
repeat the name. For the amd64 image built in this repository, the HTTP path is
`bootx64.efi` while the DHCP name is the shim, because the HTTP boot firmware
loads the removable-media path directly.

`mediaFormat` is required for VirtualMedia. When the value is not
`unbounded-uefi-v1`, metalman refuses to build the bootstrap ISO and reports
that the netboot image lacks compatible media artifacts. This is a capability
check, not a version negotiation: the installer and the controller must agree on
the media layout.

`localInstaller` is required when the Machine declares `network`. When the
value is not `unbounded-local-installer-v1`, metalman refuses to build media for
a host with bonds or VLANs. The declaration tells the controller that the
installer can bring up the declared topology itself, which is necessary because
the bootstrap ISO path has no network access until the local installer
configures it.

## MachineClass

`MachineClass` is a provider-neutral template from which capacity consumers
request machines. It names a Site, a registered provider, a provider
configuration object, and a pinned configuration version. It does not contain
provider-specific instance types or zones itself. Those come from the provider
adapter, which resolves the class into a list of offerings and publishes them in
status. A consumer picks an offering indirectly, by asking for resources and
label requirements, and the allocation controller picks a matching offering.

The resource is cluster-scoped with short name `mcls`, has a status subresource,
and prints `Site` from `.spec.siteRef.name`, `Provider` from `.spec.provider`,
`Ready` from the `Ready` condition, and `Age`. `Ready` is a string column, so it
shows `True` or `False` rather than a symbol.

| Field | Type | Required | Default | Notes |
| --- | --- | --- | --- | --- |
| `spec.siteRef` | `LocalObjectReference` | Yes | none | Site the allocated machines join; immutable. |
| `spec.siteRef.name` | `string` | Yes | none | Site name. |
| `spec.provider` | `string` | Yes | none | Registered provider adapter; immutable. |
| `spec.providerConfigRef` | `TypedObjectReference` | Yes | none | Provider-specific configuration. |
| `spec.providerConfigRef.apiGroup` | `string` | No | core group | Must be empty or the core group. |
| `spec.providerConfigRef.kind` | `string` | Yes | none | Must be `ConfigMap`. |
| `spec.providerConfigRef.name` | `string` | Yes | none | ConfigMap name. |
| `spec.providerConfigRef.namespace` | `string` | Yes | none | Required for a ConfigMap. |
| `spec.configurationRef` | `MachineConfigurationRef` | Yes | none | Configuration version frozen into allocations. |
| `spec.configurationRef.name` | `string` | Yes | none | MachineConfiguration name. |
| `spec.configurationRef.version` | `int32` | Yes | none | Version number; required for a class. |
| `spec.bootstrap` | `MachineClassBootstrap` | No | none | First-boot artifact delivery overrides. |
| `spec.bootstrap.filesRef` | `NamespacedObjectReference` | No | none | ConfigMap of bootstrap files. |
| `spec.bootstrap.filesRef.namespace` | `string` | Yes | none | ConfigMap namespace. |
| `spec.bootstrap.filesRef.name` | `string` | Yes | none | ConfigMap name; must have a non-blank `run` key. |
| `spec.bootstrap.agentArtifact` | `AgentArtifact` | No | release default | Digest-pinned agent tarball. |
| `spec.bootstrap.agentArtifact.url` | `string` | Yes | none | `https` URL or `file:///` path; no credentials, query, or fragment. |
| `spec.bootstrap.agentArtifact.sha256` | `string` | Yes | none | Lowercase hex SHA-256 of the tarball. |
| `status.observedGeneration` | `int64` | No | none | Class generation whose inputs were resolved. |
| `status.resolvedRevision` | `string` | No | none | Hash of the resolved inputs; allocations compare against it. |
| `status.providerConfig` | `ObjectContentSnapshot` | No | none | Provider config observed for the revision. |
| `status.providerConfig.namespace` | `string` | Yes | none | ConfigMap namespace. |
| `status.providerConfig.name` | `string` | Yes | none | ConfigMap name. |
| `status.providerConfig.uid` | `string` | Yes | none | ConfigMap UID. |
| `status.providerConfig.hash` | `string` | Yes | none | SHA-256 of the ConfigMap data. |
| `status.bootstrapFiles` | `ObjectContentSnapshot` | No | none | Bootstrap files observed for the revision. |
| `status.configuration` | `ConfigurationContentSnapshot` | No | none | Configuration version observed for the revision. |
| `status.configuration.name` | `string` | Yes | none | MachineConfiguration name. |
| `status.configuration.version` | `int32` | Yes | none | Version number. |
| `status.configuration.versionName` | `string` | Yes | none | MachineConfigurationVersion object name. |
| `status.configuration.uid` | `string` | Yes | none | Version UID. |
| `status.configuration.hash` | `string` | Yes | none | SHA-256 of the configuration template. |
| `status.site` | `UIDReference` | No | none | Exact Site instance the revision was resolved for. |
| `status.credential` | `UIDReference` | No | none | Exact MachineOperationCredential used. |
| `status.offerings` | `[]MachineClassOffering` | No | none | Normalized capacity offerings, keyed by `name`. |
| `status.offerings[].name` | `string` | Yes | none | Unique offering name within the class. |
| `status.offerings[].instanceType` | `string` | Yes | none | Provider instance type. |
| `status.offerings[].zone` | `string` | No | regional | Provider zone. |
| `status.offerings[].capacityType` | `string` (`OnDemand`, `Spot`) | Yes | none | Purchase model. |
| `status.offerings[].labels` | `map[string]string` | No | none | Well-known labels Nodes from this offering register with. |
| `status.offerings[].capacity` | `corev1.ResourceList` | No | none | Machine capacity before overhead. |
| `status.offerings[].overhead` | `OfferingOverhead` | No | none | Capacity reserved from workloads. |
| `status.offerings[].overhead.kubeReserved` | `corev1.ResourceList` | No | none | Reserved for Kubernetes components. |
| `status.offerings[].overhead.systemReserved` | `corev1.ResourceList` | No | none | Reserved for operating system daemons. |
| `status.offerings[].overhead.evictionThreshold` | `corev1.ResourceList` | No | none | Hard eviction threshold. |
| `status.offerings[].availability` | `string` (`Available`, `Unavailable`, `Unknown`) | Yes | none | Observed availability. |
| `status.offerings[].price` | `OfferingPrice` | No | none | Observed or configured price. |
| `status.offerings[].price.currency` | `string` | Yes | none | ISO 4217 code, three uppercase letters. |
| `status.offerings[].price.perHour` | `string` | Yes | none | Decimal price per instance hour. |
| `status.offerings[].price.source` | `string` | No | none | Where the price came from. |
| `status.offerings[].price.observedAt` | `time` | No | none | When the price was observed. |
| `status.offerings[].observedAt` | `time` | No | none | When availability and capacity were observed. |
| `status.offerings[].validUntil` | `time` | No | none | When the observation expires; an expired offering must not launch. |
| `status.conditions` | `[]metav1.Condition` | No | none | `Ready` and `OfferingsCurrent`. |

### MachineClass controller

The MachineClass controller runs only for classes in its scope. A controller
started with a provider name selects only that provider registration. A
controller started with a Site name selects only classes whose
`spec.siteRef.name` matches. With neither, it selects every class whose provider
is registered in that process. Two deployments with overlapping scopes both
write the same class status, so each scoped deployment needs its own leader
election lease and scopes must not overlap.

The controller resolves class inputs in a fixed order and stops at the first
failure. It reads the Site, then the provider configuration ConfigMap, then the
bootstrap files ConfigMap if one is referenced, then the configuration version,
then the provider credential. Resolving the Site first means a missing or
deleting Site is reported before a later input is blamed.

The provider configuration must be a core-group ConfigMap in the namespace the
controller was configured to read. It must set `immutable: true` and must not
carry `binaryData`. The bootstrap files ConfigMap follows the same rules, and
its `run` key must be non-blank because it is the script that runs as root
before the agent installer. These restrictions let the controller hash the
content and trust that the hash still describes the object later.

The configuration reference must name a specific version. The controller loads
the corresponding MachineConfigurationVersion and checks that its
`spec.version` equals the reference and that its
`unbounded-cloud.io/machine-configuration` label equals the referenced
configuration name. A mismatch is reported as `ConfigurationMismatch` rather
than silently using a different generation of the content.

The provider credential is resolved from the site name and provider name. There
must be exactly one matching `MachineOperationCredential`. More than one is
`AuthAmbiguous`; none is `AuthNotFound`. If the credential references a Secret,
the Secret must exist in the allowed credential namespace, and an
`ExternalPlugin` credential without a `secretRef` is invalid.

The adapter resolves the class through its `ResolveClass` callback and returns
offerings. The controller normalizes and validates them before publishing. An
invalid offering set fails the resolve with `OfferingsInvalid`; it is not
partially published. The callback also receives the provider configuration data
and the resolved credential, so it can query the provider for current capacity
and prices without the controller knowing provider-specific details.

A successful resolve requeues after the class refresh interval, or sooner if an
offering's `validUntil` is earlier. The interval defaults to 5 minutes and has a
minimum requeue of 10 seconds. The controller watches Sites,
MachineConfigurationVersions, MachineOperationCredentials, and the ConfigMaps it
reads. It does not watch Secrets. A change to credential Secret content is
therefore picked up on the next interval or on another watched event, not
immediately. Rotate a credential Secret with that gap in mind.

### MachineClass status

`status.resolvedRevision` is a SHA-256 hash of the resolved inputs in a fixed
order: provider name, Site reference with UID, credential reference with UID,
provider configuration snapshot, bootstrap files snapshot, configuration
snapshot, and agent artifact. It deliberately excludes offerings, timestamps,
conditions, the class generation, the class name and UID, and Secret content.
Two resolves that produce the same inputs produce the same revision even if the
class's metadata generation moved. Allocations freeze this revision and compare
it with the class's current value to detect drift.

A successful resolve writes the revision, all snapshots, the Site and credential
references, and the offerings in one status update. A failure does not clear
them. `Ready` becomes false with a reason that names the failed input, such as
`SiteNotFound`, `ProviderConfigInvalid`, `ProviderConfigNotFound`,
`ProviderConfigNotImmutable`, `BootstrapFilesInvalid`,
`ConfigurationInvalid`, `ConfigurationNotFound`, `ConfigurationMismatch`,
`AuthNotFound`, or `AuthInvalid`. The previously published revision and
offerings remain readable, which lets existing allocations continue with the
inputs they froze.

A transient provider failure is different from an input failure. It reports
`ResolveFailed` or `OfferingsInvalid`, but it keeps `Ready` true when neither the
revision nor the generation moved since the last success. That avoids flapping a
healthy class because one provider API call failed. The error is returned to the
workqueue so the retry backs off.

`OfferingsCurrent` is true when at least one offering is `Available` and has a
`validUntil` in the future. It is false with reason `NoOfferingsAvailable` when
none qualifies, and false with reason `ClassNotReady` when the class itself is
not ready. An offering without `validUntil` is never considered current, even if
its availability is `Available`, because the controller cannot tell whether the
observation is still valid.

### Installing the MachineClass controller

A provider registers capacity by passing `WithCapacity(resolveClass,
planAllocation)` to `NewProvider`. The resolve callback returns offerings for a
class. The plan callback turns an allocation request into a concrete launch
plan. Without that option the provider can still execute host operations but
cannot serve classes.

The process that runs the controller calls `AddCapacityToManager` with the same
providers and a `CapacityOptions` value. `CredentialSecretNamespace` and
`ProviderConfigNamespace` are required. `ProviderName` and `SiteName` narrow the
scope. `ClassRefreshInterval` and `MinRequeueInterval` tune the cadence. The
manager must enable leader election with a lease ID unique to the scope, because
two controllers that both think they own a class will fight over its status.

The controller needs the `unbounded-machineclass-controller` ClusterRole bound
with a ClusterRoleBinding. It grants read access to classes, Sites,
configuration versions, and credentials, plus status writes on classes. The
`unbounded-machineclass-controller-provider-config` role must be bound with a
RoleBinding in the provider-config namespace only, and
`unbounded-machineclass-controller-credentials` with a RoleBinding in the
credential namespace only. A ClusterRoleBinding for either would let the
controller read every ConfigMap or Secret in the cluster.

## MachineAllocation

`MachineAllocation` is one consumer's request for one machine from a
MachineClass. It is owned by the consumer, not by the platform, and its UID owns
every remote effect made on its behalf. The allocation controller launches a
host, creates a Machine for it, mints a bootstrap token, and later releases the
host when the consumer authorizes it. The UID is the important part: a
recreated allocation with the same name is a different owner, and provider
resources are only released when their identity is traceable to this UID.

The resource is cluster-scoped with short name `malloc`, has a status
subresource, and prints `Class` from `.spec.classRef.name`, `Lifecycle` from
`.spec.lifecycle`, `Allocated` from the `Allocated` condition, `Node` from
`.status.nodeRef.name`, and `Age`.

| Field | Type | Required | Default | Notes |
| --- | --- | --- | --- | --- |
| `spec.consumerRef` | `MachineAllocationConsumerReference` | Yes | none | Requesting object; immutable. |
| `spec.consumerRef.apiGroup` | `string` | Yes | none | Consumer API group; empty means core. |
| `spec.consumerRef.kind` | `string` | Yes | none | Consumer kind. |
| `spec.consumerRef.name` | `string` | Yes | none | Consumer name. |
| `spec.consumerRef.uid` | `string` | Yes | none | Consumer UID; a recreated consumer is a different object. |
| `spec.classRef` | `UIDReference` | Yes | none | Exact MachineClass; immutable. |
| `spec.classRef.name` | `string` | Yes | none | Class name. |
| `spec.classRef.uid` | `string` | Yes | none | Class UID. |
| `spec.resources` | `corev1.ResourceList` | No | none | Minimum requested resources; immutable. |
| `spec.requirements` | `[]MachineAllocationRequirement` | No | none | Offering label constraints; immutable. |
| `spec.requirements[].key` | `string` | Yes | none | Label key. |
| `spec.requirements[].operator` | `string` | Yes | none | `In`, `NotIn`, `Exists`, `DoesNotExist`, `Gt`, or `Lt`. |
| `spec.requirements[].values` | `[]string` | No | none | Operator values. |
| `spec.requirements[].minValues` | `int32` | No | none | Minimum distinct values that must remain; at least 1. |
| `spec.nodeTemplate` | `MachineAllocationNodeTemplate` | No | none | Applied to the Node at registration; immutable. |
| `spec.nodeTemplate.labels` | `map[string]string` | No | none | Labels added at registration. |
| `spec.nodeTemplate.taints` | `[]corev1.Taint` | No | none | Taints added at registration. |
| `spec.nodeTemplate.startupTaints` | `[]corev1.Taint` | No | none | Taints removed by their owners after initialization. |
| `spec.lifecycle` | `string` (`Active`, `Released`) | No | `Active` | Authorizes release; cannot return to `Active`. |
| `status.observedGeneration` | `int64` | No | none | Allocation generation last reconciled. |
| `status.snapshot` | `MachineAllocationSnapshot` | No | none | Frozen input for every remote effect; immutable once set. |
| `status.snapshot.classUID` | `string` | Yes | none | Class UID the snapshot was taken from. |
| `status.snapshot.classGeneration` | `int64` | Yes | none | Class generation at snapshot time. |
| `status.snapshot.classRevision` | `string` | Yes | none | Class resolved revision at snapshot time. |
| `status.snapshot.provider` | `string` | Yes | none | Provider adapter name. |
| `status.snapshot.siteName` | `string` | Yes | none | Site the machine joins. |
| `status.snapshot.siteUID` | `string` | Yes | none | Exact Site instance. |
| `status.snapshot.credential` | `UIDReference` | Yes | none | Exact credential instance. |
| `status.snapshot.offering` | `MachineClassOffering` | Yes | none | Selected offering. |
| `status.snapshot.providerConfig` | `ObjectContentSnapshot` | Yes | none | Exact provider configuration content. |
| `status.snapshot.bootstrapFiles` | `ObjectContentSnapshot` | No | none | Exact bootstrap files content. |
| `status.snapshot.agentArtifact` | `AgentArtifact` | No | none | Digest-pinned agent tarball. |
| `status.snapshot.configuration` | `ConfigurationContentSnapshot` | Yes | none | Configuration version content. |
| `status.snapshot.configurationTemplate` | `MachineConfigurationTemplate` | Yes | none | Frozen template for the allocation's Machines. |
| `status.snapshot.hostImage` | `string` | Yes | none | Provider-interpreted host image. |
| `status.snapshot.hostProviderID` | `string` | Yes | none | Planned provider host ID before the host exists. |
| `status.providerID` | `string` | No | none | Exact Node `spec.providerID`; immutable once set. |
| `status.providerResourceID` | `string` | No | none | Provider-native host identifier. |
| `status.instanceType` | `string` | No | none | Launched instance type. |
| `status.zone` | `string` | No | none | Launched zone. |
| `status.capacityType` | `string` | No | none | Launched purchase model. |
| `status.image` | `string` | No | none | Launched host image. |
| `status.capacity` | `corev1.ResourceList` | No | none | Machine capacity. |
| `status.allocatable` | `corev1.ResourceList` | No | none | Observed Node allocatable, otherwise capacity minus overhead. |
| `status.labels` | `map[string]string` | No | none | Labels the Node registers with. |
| `status.ownedResources` | `[]ProviderOwnedResource` | No | none | Confirmed provider resources, keyed by `id`. |
| `status.ownedResources[].id` | `string` | Yes | none | Provider resource identifier. |
| `status.ownedResources[].nativeID` | `string` | Yes | none | Provider-assigned identity; a recreated resource differs. |
| `status.machineRef` | `UIDReference` | No | none | Machine created for the allocation. |
| `status.nodeRef` | `UIDReference` | No | none | First Node registered for the allocation. |
| `status.operations.allocate` | `UIDReference` | No | none | Recorded `HostAllocate` operation. |
| `status.operations.release` | `UIDReference` | No | none | Recorded `HostRelease` operation. |
| `status.failureReason` | `string` | No | none | Terminal launch failure category. |
| `status.failureMessage` | `string` | No | none | Terminal launch failure detail. |
| `status.conditions` | `[]metav1.Condition` | No | none | `Allocated`, `Registered`, `Fenced`, `Released`. |

### MachineAllocation controller

The allocation controller is part of the capacity controllers. It runs only for
allocations in its scope, which is narrowed by the same provider and Site
options as the class controller. The class must be in the controller's provider
set, and an allocation that names a class outside that set is left alone.

The controller validates the allocation's labels before it does anything else.
`spec.consumerRef.uid` becomes the
`unbounded-cloud.io/consumer-uid` label and `spec.classRef.name` becomes the
`unbounded-cloud.io/machine-class` label, both as written. A value that is not a
valid Kubernetes label value fails the allocation with `InvalidRequest`. The
check happens before a host is launched, because a label that cannot be written
would make the allocation impossible to find and its effects impossible to
clean up.

The launch path is a sequence of small steps, each of which is safe to retry.
The controller first selects an offering and writes the snapshot in one status
update. It then adds the finalizer and labels, protects the input ConfigMaps
with a per-allocation finalizer, mints the bootstrap token, creates the Machine,
and creates or adopts the `HostAllocate` operation. The finalizer is added only
after the snapshot is written, so a crash between those steps leaves a status
that says "not started" rather than a second launch.

Offering selection considers the requested resources and the label
requirements. Only current offerings are candidates: an offering must be
`Available` and unexpired. `minValues` is a flexibility floor: at least that
many distinct values of the requirement's key must remain among the offerings
that satisfy every other requirement, or the allocation fails with
`InsufficientCapacity`. The allocation then resolves to one of them. The
offering is checked again immediately before the first create, because capacity
can disappear between the snapshot write and the launch.

The created objects have deterministic names derived from the allocation UID.
The Machine is named `ub-` followed by the UID with dashes removed. The
`HostAllocate` operation is `<machine>-allocate`, and the `HostRelease`
operation is `<machine>-release`. The bootstrap token ID is six characters and
the Secret is the usual `bootstrap-token-<id>` in `kube-system`. The Secret
carries the site label and `unbounded-cloud.io/allocation-uid`, but not the
default-token label. The controller refuses to reuse a Machine or Secret name
that belongs to another allocation.

The Machine created for the allocation carries `spec.allocationRef` with the
allocation name and UID, `spec.host.external.provider` and `providerID` from the
frozen snapshot, and `spec.kubernetes` fields derived from the frozen
configuration. It is owned by the allocation and carries the allocation
finalizer. The Node registers with the provider ID
`unbounded:///machineallocations/<uid>`; that is what the allocation controller
uses to recognize the Node as its own. Node labels start with the frozen
configuration template's labels, then `spec.nodeTemplate.labels`, then the
offering's labels, which win on a conflict. Taints combine the template's
registration taints with the node template's taints and startup taints. Startup
taints are added at registration and removed by whoever owns them.

The allocation controller watches the recorded operations and copies their
progress into `status.operations` and the `Allocated` condition. It does not
launch a second operation if the recorded one is missing or replaced; it keeps
the reference and reports `AllocateOperationMissing` or
`ReleaseOperationMissing`, because a replacement could duplicate a host.

An allocation is retired when the consumer sets `spec.lifecycle: Released`, when
the allocation is being deleted, when the consumer is terminating or gone, or
when the Machine is being deleted. Consumer termination is observed only for the
kinds listed in `CapacityOptions.AllocationConsumerKinds`, and those kinds must
be cluster-scoped. Retiring does not release anything by itself. The controller
fences the Machine, revokes the bootstrap token, and waits for the consumer to
set `Released`. That wait is reported as `AwaitingRelease`, with the signal that
retired the allocation in the message.

Release is a sequence with a clear commit point. The controller fences the
Machine and confirms that the token is gone. It then requires the release
authorization and a terminal, ownership-known `HostAllocate`. It calls the
provider's release path for exactly the resources in `status.ownedResources`,
removes the input finalizers, and sets `Released=True`. The `Released` condition
is written before the finalizers are removed; once it is true, the object can be
deleted.

Several conditions hold instead of failing. `SnapshotMissing` means the
finalizer exists but the snapshot was lost, which requires manual recovery.
`BeginOutcomeUnknown` means a provider call may have run without returning a
result. `OwnershipUnknown` means the provider could not prove which resources it
created. `MachineMissing` means the recorded Machine is gone before release can
use it. These holds are deliberate: releasing infrastructure that cannot be
proven to belong to the allocation is worse than leaving it in place, so the
controller asks for a human to reconcile the evidence.

### Conditions and failure reasons

`Allocated` reports whether the host launched. It is true with reason
`Allocated` after a successful allocate, and false with a failure reason or a
hold reason while the launch is pending or stuck. Its message carries the
provider detail.

`Registered` reports whether the expected Node registered with the allocation's
provider ID. It is false with `NodeNotRegistered` while waiting, false with
`NodeConflict` when a Node exists at the expected name with a different provider
ID, and true with `NodeRegistered` once the binding is confirmed.

`Fenced` reports whether the host may no longer repave or rejoin. Fencing never
releases infrastructure by itself; it only stops the node from coming back
while release is arranged.

`Released` reports whether every remote effect and the per-allocation bootstrap
credential were confirmed released. It is the commit point described above.

The API also declares a `Drifted` condition for an allocation whose frozen class
revision differs from the class's current resolved revision. The allocation
controller described here does not publish it, so do not use it as the only
signal that an allocation is behind. Compare `status.snapshot.classRevision`
with the class's `status.resolvedRevision` directly.

A terminal launch failure records a normalized `status.failureReason`. The
values are `InsufficientCapacity` (no offering satisfies the request),
`QuotaExceeded` (the provider refused for quota), `InvalidRequest` (the
allocation or class cannot be satisfied as written), `AuthFailed` (the provider
rejected the credential), `ProviderConflict` (a provider resource with the
planned identity exists and is not owned by this allocation), and
`ProviderFailed` (any other provider failure). `failureMessage` carries the
detail. A failed allocation may still have `ownedResources`, because a partial
create is exactly what release must clean up.

### Running HostAllocate and HostRelease

The operations themselves are executed by the path registered by `AddToManager`
with the same providers. Installing only the allocation controller is not
enough; it creates the operations, but nothing launches the host. The
`machineoperation-allocation` controller is the part that calls the provider's
allocate and release callbacks.

Before Begin, every input is rebuilt from the snapshot and checked against the
live object. The provider configuration ConfigMap, bootstrap files,
configuration version, Site, and credential must still be the exact instances
the snapshot froze, and the agent artifact is digest-pinned in the snapshot. A
missing or changed input is an `allocationInputError`, and Begin is never called
with it. This is why the snapshot records UIDs and content hashes rather than
names alone.

The operation must be the one the allocation recorded for its kind. An
unrecorded operation under the deterministic name waits while the allocation
controller may still record it; any other unrecorded operation fails. A
`HostAllocate` Begin that may have run before and now returns a permanent error
is reported as `OwnershipUnknown`, because it could have created a host that it
no longer reports.

`HostAllocate` renders user data from the frozen configuration, the allocation
bootstrap token, the install environment, and the optional bootstrap files. The
cloud-init path installs the agent and configures the host. The provider returns
a handle, the controller persists it in
`status.targets[].providerOperation`, and polls until the operation succeeds or
fails. A successful result is copied to `status.allocation` and the
`MachineAllocation` status.

`HostRelease` has stricter preconditions than allocate. It requires
`spec.lifecycle: Released`, a fenced or deleting Machine, a terminal recorded
`HostAllocate`, and known ownership. Credential failures during release are
recoverable: repair the `MachineOperationCredential` or its Secret and the
recorded release operation can resume. A terminal release is never replaced.

`HostReplace`, `HostPowerOn`, and `HostPowerOff` are not supported on a Machine
that is bound to an allocation. The host operation path rejects them with
`UnsupportedOperation`. Power and replacement are lifecycle actions the platform
owns for allocated capacity, not actions a consumer can take on a machine it
does not own.

### Installing the MachineAllocation controller

The controller is enabled with `CapacityOptions.EnableAllocationController`.
`AllocationConsumerKinds` lists the cluster-scoped consumer kinds whose
termination and loss the controller observes; it requires the enable flag and
must contain a version and kind for each entry. With an empty list, consumer
loss is never observed and only explicit release or deletion retires an
allocation.

Five ClusterRoles are involved. `unbounded-machineallocation-controller` grants
the controller its reads and its status and Machine writes, including the
`allocate` verb on Machines that lets it set `spec.allocationRef`.
`unbounded-machineallocation-controller-provider-config` grants read access to
the provider-config namespace and is bound there with a RoleBinding.
`unbounded-machineallocation-controller-bootstrap-tokens` grants Secret create,
get, and delete in `kube-system`. `unbounded-machineallocation-operations`
grants the operation path its allocation and Site reads.
`unbounded-machineallocation-operations-bootstrap-tokens` grants the operation
path read access to the allocation's token Secret so it can render user data.

Two ValidatingAdmissionPolicies complete the boundary.
`unbounded-machine-allocationref` requires the `allocate` verb to create a
Machine with `spec.allocationRef`. `unbounded-machineallocation-bootstrap-tokens`
limits Secret creates and deletes to bootstrap token Secrets carrying the
allocation UID label, because RBAC cannot express that condition by name.

Install the CRDs and the allocation controller before you create allocations.
If the controller lacks one of these grants, it cannot write the labels, create
the token, or bind the Machine, and the allocation stays unlaunched rather than
half-created. The policies close the admission gap that RBAC alone cannot
express, so install them with the controller. When upgrading, apply the CRDs
first, then the RBAC and policies, then the controller deployment. A controller
that starts before its roles exist will report failures until they are in place.

## MachineConfiguration

`MachineConfiguration` stores a configuration profile for a class of Machines.
It behaves like a Deployment: edits to its spec create or update a child
`MachineConfigurationVersion`, the way a Deployment edit creates or updates a
ReplicaSet. The template holds only provider-neutral fields. Per-machine host
ownership stays on the Machine, not on the shared profile.

The resource is cluster-scoped with short name `mc`, has a status subresource,
and prints `Latest Version` from `.status.latestVersion`, `Update Strategy` from
`.spec.updateStrategy.type`, and `Age`.

| Field | Type | Required | Default | Notes |
| --- | --- | --- | --- | --- |
| `spec.template` | `MachineConfigurationTemplate` | Yes | none | Fields copied into each version. |
| `spec.template.host` | `MachineConfigurationHostSpec` | No | none | Provider-neutral host settings. |
| `spec.template.host.provisioningFormat` | `string` (`CloudInit`, `Ignition`) | No | none | First-boot format understood by the image. |
| `spec.template.host.image` | `string` | No | none | Opaque provider image identifier. |
| `spec.template.kubernetes` | `MachineConfigurationKubernetes` | No | none | Kubernetes settings. |
| `spec.template.kubernetes.version` | `string` | No | cluster version | Kubernetes version to install. |
| `spec.template.kubernetes.nodeLabels` | `map[string]string` | No | none | Kubelet `--node-labels`. |
| `spec.template.kubernetes.registerWithTaints` | `[]corev1.Taint` | No | none | Kubelet `--register-with-taints`. |
| `spec.template.agent` | `MachineConfigurationAgent` | No | none | Agent settings. |
| `spec.template.agent.image` | `string` | Yes | none | OCI image for the nspawn machine, required when `agent` is set. |
| `spec.template.agent.localDNS` | `LocalDNSSpec` | No | disabled | CoreDNS cache settings. |
| `spec.machineSelector` | `metav1.LabelSelector` | No | none | Selects Machines that receive this configuration. |
| `spec.priority` | `int32` | No | `0` | Higher values win when several configurations match. |
| `spec.updateStrategy` | `MachineConfigurationUpdateStrategy` | No | none | Rollout behavior. |
| `spec.updateStrategy.type` | `string` (`OnDelete`, `RollingUpdate`) | No | `OnDelete` | `RollingUpdate` is reserved for future implementation. |
| `spec.updateStrategy.maxUnavailable` | `int32` | No | none | Only meaningful for `RollingUpdate`. |
| `spec.revisionHistoryLimit` | `int32` | No | `10` | Old undeployed versions to retain; minimum 0. |
| `status.latestVersion` | `int32` | No | none | Highest version ever created. |
| `status.currentVersion` | `int32` | No | none | Defined as the latest non-deployed editable version, or 0 when all are deployed; machina does not currently write it. |
| `status.conditions` | `[]metav1.Condition` | No | none | Latest observations. |

The controller creates the first version, `v1`, for a new configuration. Its
logic is structured around deployment: while the latest version is not deployed,
a spec edit updates it in place; once a version is deployed, the next spec edit
is supposed to create a new version with the next number. Version numbers are
never reused, because `status.latestVersion` records the high-water mark.

There is an implementation gap to know about. Machina does not currently write
`status.deployed`, `status.deployedMachines`, or `status.currentVersion` on a
MachineConfigurationVersion. As a result, in this worktree the latest version
stays in the undeployed branch and a spec edit updates it in place. No new
version is created automatically, and the cleanup path, which only considers
undeployed versions beyond `revisionHistoryLimit`, does not run. Do not rely on
a version being frozen merely because a Machine references it.

Child versions are named `<configuration>-v<number>` and carry the
`unbounded-cloud.io/machine-configuration` and
`unbounded-cloud.io/machine-configuration-version` labels. The controller sets
an owner reference so deleting the configuration eventually deletes its
versions. When the deployment status is populated, cleanup is designed to remove
only old undeployed versions beyond the history limit and to leave versions with
a nonzero deployed count in place.

`spec.machineSelector` is the automatic assignment path. The binding controller
evaluates every configuration with a selector against the Machine's labels. The
highest `priority` wins, and a tie is broken by lexicographic order of the
configuration names. The selected configuration's latest locked version is
written into `spec.configurationRef.version` and the Machine learns the exact
version. An explicit `spec.configurationRef` on the Machine wins over selectors,
so a Machine can be pinned without changing the profile. When nothing matches,
the Machine gets `ConfigurationPending=True` with reason `Pending`.

`OnDelete` is the implemented update strategy. A configuration change does not
touch a running node; the operator cordons, drains, and deletes the Node, and
the agent repaves with the desired version. While that work is outstanding, the
`RepavePending` condition on the Machine remains true. `RollingUpdate` is
accepted by the schema but is not implemented, and `maxUnavailable` has no
effect unless that strategy lands.

## MachineConfigurationVersion

`MachineConfigurationVersion` is intended as an immutable-once-deployed
snapshot of a MachineConfiguration. It exists so that a Machine can name exactly
the content it bootstrapped with, even after the parent profile changes. The controller
manages these objects; you normally read them rather than create them.

The resource is cluster-scoped with short name `mcv`, has a status subresource,
and prints `Configuration` from the
`unbounded-cloud.io/machine-configuration` label, `Version` from
`.spec.version`, `Deployed` from `.status.deployed`, `Machines` from
`.status.deployedMachines`, and `Age`.

| Field | Type | Required | Default | Notes |
| --- | --- | --- | --- | --- |
| `spec.version` | `int32` | Yes | none | Monotonic version within the parent; minimum 1. |
| `spec.template` | `MachineConfigurationTemplate` | Yes | none | Snapshot copied from the parent. |
| `spec.template.host` | `MachineConfigurationHostSpec` | No | none | Host settings in the snapshot. |
| `spec.template.host.provisioningFormat` | `string` | No | none | First-boot format. |
| `spec.template.host.image` | `string` | No | none | Opaque image identifier. |
| `spec.template.kubernetes` | `MachineConfigurationKubernetes` | No | none | Kubernetes settings in the snapshot. |
| `spec.template.kubernetes.version` | `string` | No | none | Kubernetes version. |
| `spec.template.kubernetes.nodeLabels` | `map[string]string` | No | none | Node labels. |
| `spec.template.kubernetes.registerWithTaints` | `[]corev1.Taint` | No | none | Registration taints. |
| `spec.template.agent` | `MachineConfigurationAgent` | No | none | Agent settings in the snapshot. |
| `spec.template.agent.image` | `string` | Yes | none | Agent image, required when `agent` is set. |
| `spec.template.agent.localDNS` | `LocalDNSSpec` | No | none | CoreDNS cache settings. |
| `status.deployed` | `bool` | No | `false` | Defined as true once any Machine has provisioned with this version; machina does not currently write it. |
| `status.deployedMachines` | `int32` | No | `0` | Defined as the count of Machines referencing this version; machina does not currently write it. |
| `status.conditions` | `[]metav1.Condition` | No | none | Latest observations. |

While `status.deployed` is false, the parent controller may update the template
in place. The field is not currently populated, so a version is edited in place
in practice and the `Deployed` and `Machines` columns stay empty. Treat that as
an implementation gap rather than as evidence that no Machine uses the version.
If you need an immutable snapshot today, create a new MachineConfiguration
instead of editing an existing one.

## MachineOperationCredential

`MachineOperationCredential` defines the credential source an external
MachineOperation controller uses for machines in a Site. The lookup key is the
pair of site name and provider name. It is a pure input object: it has no status
subresource, and failures to resolve it appear on the class or operation that
tried to use it.

The resource is cluster-scoped with short name `mocred` and prints `Site` from
`.spec.siteName`, `Provider` from `.spec.provider`, `Auth Mode` from
`.spec.auth.mode`, and `Age`.

| Field | Type | Required | Default | Notes |
| --- | --- | --- | --- | --- |
| `spec.siteName` | `string` | Yes | none | Matched against the Machine site label. |
| `spec.provider` | `string` | Yes | none | External control provider this credential applies to. |
| `spec.auth` | `MachineOperationCredentialAuth` | Yes | none | Credential source configuration. |
| `spec.auth.mode` | `string` (`WorkloadIdentity`, `ExternalPlugin`) | Yes | none | How the provider authenticates. |
| `spec.auth.secretRef` | `NamespacedSecretReference` | No | none | Provider plugin configuration; required for `ExternalPlugin`. |
| `spec.auth.secretRef.name` | `string` | Yes | none | Secret name. |
| `spec.auth.secretRef.namespace` | `string` | Yes | none | Secret namespace. |

`WorkloadIdentity` uses the OIDC or workload identity credentials available to
the controller process. That is the default Azure path, and it needs no Secret.
`ExternalPlugin` delegates to provider-specific configuration in the referenced
Secret. The Secret must exist in the credential namespace the controller was
configured with, and every data key is passed to the provider as string data.
The Azure external-plugin path reads `tenantID`, `clientID`, and `clientSecret`;
other providers define their own keys.

There must be exactly one credential for a site and provider pair. Two matching
credentials are reported as `AuthAmbiguous`, because the controller will not
guess which one is intended. A missing Secret is `AuthSecretNotFound`, and a
Secret outside the allowed namespace is `AuthSecretForbidden`.

## Site

`Site` defines a top-level Unbounded location and the optional components
enabled for it. Machines join a Site through the `unbounded-cloud.io/site`
label. The Site carries the node and pod CIDR plan, the tunnel settings, and the
component toggles that the operator uses to reconcile cluster-wide workloads.

The resource is cluster-scoped with short name `st`, has a status subresource,
and prints `Node CIDRs`, `Pod CIDR Assignments`, `Nodes`, `Slices`, and `Age`.
The `Metalman`, `Gantry`, `Token Refresher`, and `Overrides` columns have
priority 1, so they appear only in wide output.

| Field | Type | Required | Default | Notes |
| --- | --- | --- | --- | --- |
| `spec.nodeCidrs` | `[]string` | Yes | none | CIDRs containing node internal IPs; at least one. |
| `spec.podCidrAssignments` | `[]PodCidrAssignment` | Yes | none | Pod CIDR allocation plan; at least one. |
| `spec.podCidrAssignments[].assignmentEnabled` | `bool` | No | site default | Overrides `manageCniPlugin` for this assignment. |
| `spec.podCidrAssignments[].cidrBlocks` | `[]string` | Yes | none | CIDR pools to allocate from; at least one. |
| `spec.podCidrAssignments[].nodeBlockSizes` | `NodeBlockSizes` | No | none | Per-node IPv4 and IPv6 block sizes. |
| `spec.podCidrAssignments[].nodeRegex` | `[]string` | No | none | Node name patterns for the assignment. |
| `spec.podCidrAssignments[].priority` | `int32` | No | `100` | Lower values win when several assignments match. |
| `spec.manageCniPlugin` | `bool` | No | `true` | Writes CNI config and creates same-site tunnel endpoints. |
| `spec.nonMasqueradeCIDRs` | `[]string` | No | none | Destinations that must not be masqueraded. |
| `spec.localCidrs` | `[]string` | No | none | CIDRs considered local and never routed through gateway pools. |
| `spec.healthCheckSettings` | `HealthCheckSettings` | No | none | Inter-site tunnel health checks. |
| `spec.tunnelProtocol` | `string` | No | `Auto` | `WireGuard`, `IPIP`, `GENEVE`, `VXLAN`, `None`, or `Auto`. |
| `spec.tunnelMTU` | `int32` | No | none | Route MTU through tunnels; 576 to 9000. |
| `spec.components` | `SiteComponents` | No | none | Optional component configuration. |
| `spec.components.machina` | `MachinaComponentSpec` | No | ignored | Deprecated compatibility setting; machina is always required. |
| `spec.components.metalman` | `MetalmanComponentSpec` | No | disabled | Metalman PXE controller for the site. |
| `spec.components.metalman.enabled` | `bool` | No | `false` | Enables the component. |
| `spec.components.metalman.dhcpAutoInterface` | `bool` | No | none | Lets metalman choose the DHCP interface. |
| `spec.components.metalman.replicas` | `int32` | No | `1` | Desired replicas; minimum 0. |
| `spec.components.gantry` | `GantryComponentSpec` | No | enabled | Gantry peer-to-peer distribution agent. |
| `spec.components.gantry.enabled` | `bool` | No | `true` | Set false to opt the site out. |
| `spec.components.gantry.artifactStreaming` | `GantryArtifactStreamingSpec` | No | disabled | AKS artifact streaming proxy. |
| `spec.components.gantry.artifactStreaming.enabled` | `bool` | No | `false` | Enables the range endpoint and configurator. |
| `spec.components.gantry.artifactStreaming.nodeSelector` | `map[string]string` | No | none | Required when enabled; at least one entry. |
| `spec.components.tokenRefresher` | `TokenRefresherComponentSpec` | No | enabled | Maintains bootstrap tokens for joining machines. |
| `spec.components.tokenRefresher.enabled` | `bool` | No | `true` | Set false to disable. |
| `status.nodeCount` | `int` | No | none | Nodes matched to the site by unbounded-net. |
| `status.sliceCount` | `int` | No | none | SiteNodeSlice objects for the site. |
| `status.conditions` | `[]metav1.Condition` | No | none | One condition per component, such as `NetReady` or `MachinaReady`. |
| `status.overrides` | `OverrideStatus` | No | none | Workload override summary; absent when none apply. |
| `status.overrides.phase` | `string` | Yes | none | `None`, `Applied`, or `Degraded`. |
| `status.overrides.observedResourceVersion` | `string` | No | none | Resource version of the overrides ConfigMap that produced the result. |
| `status.overrides.workloads` | `[]OverriddenWorkload` | No | none | Per-workload override state, keyed by kind and name. |
| `status.overrides.workloads[].kind` | `string` | Yes | none | Workload kind. |
| `status.overrides.workloads[].name` | `string` | Yes | none | Workload name. |
| `status.overrides.workloads[].desiredHash` | `string` | No | none | Hash the operator wanted. |
| `status.overrides.workloads[].appliedHash` | `string` | No | none | Hash the workload carries. |
| `status.overrides.workloads[].versionDrift` | `string` | No | none | `<container>=<image>` when an override changed an image. |
| `status.overrides.workloads[].state` | `string` | No | none | `Applied`, `Pending`, `Withheld`, or `Failed`. |
| `status.overrides.message` | `string` | No | none | Explains a degraded phase. |

`manageCniPlugin` controls whether the node agent writes CNI configuration and
creates tunnel endpoints for same-site nodes. When it is true, which is the
default, CNI config is written and all same-site nodes are tunnel peers. When it
is false, only tunnel links to gateway pools for other sites are created,
allowing an external CNI plugin to manage intra-site networking. An omitted
`assignmentEnabled` inherits this setting; an explicit value overrides it. The
pod CIDR assignments are still required even when assignment is disabled,
because inter-site routing needs the ranges.

`spec.components.gantry` defaults to enabled whenever a `components` block is
present. A site that omits the components block entirely is not defaulted.
`tokenRefresher` also defaults to enabled, and it is ignored for the reserved
cluster site. `machina` is a deprecated compatibility field: machina is a
required singleton and cannot be disabled, so the field is ignored.

The Site conditions are published per component rather than as one aggregate,
which is what lets `kubectl wait` target a specific component. The workload
override summary is separate because one override document routinely targets
several components. `status.overrides.phase` reports whether the merge was
written, not whether the resulting pods are healthy. A workload with an image
override also records `versionDrift`, because a pinned image survives operator
upgrades and is the likeliest reason a component behaves differently from the
version the operator reports.

## CRD relationships

The diagram below shows the main references. An arrow means the source names the
target; it does not imply ownership or cascading deletion.

![Machina custom resource relationships, showing Site, Machine, MachineOperation, MachineClass, MachineAllocation, MachineConfiguration, and their Secrets and Node](../img/machina-crd-relationships.svg)

A Machine belongs to a Site by label. It can reference a MachineConfiguration
directly or receive one through a selector match. When it is bound to an
allocation, it also references the MachineAllocation by name and UID and the
allocation's provider by name. The Machine names Secrets for SSH keys and Redfish
passwords, and the bootstrap token Secret lives in `kube-system`. The
Machine-to-Node link is `spec.kubernetes.nodeRef`, which is set by the
controller and is needed for certificate renewal on allocation hosts.

A MachineClass names a Site, a provider ConfigMap, a configuration version, and
optionally a bootstrap files ConfigMap and agent artifact. A MachineAllocation
names the class by name and UID and freezes the resolved content in its
snapshot. The allocation creates one Machine and two MachineOperations, one to
allocate and one to release. The MachineOperation names the Machine and, for
allocation operations, the allocation.

The Secret namespaces are deliberate. Provider credentials live in the
namespace the capacity controller is configured to read, which is normally
`unbounded-system`. SSH and Redfish Secrets can live wherever the Machine
references them, subject to the controller's RBAC. Bootstrap tokens live in
`kube-system` because that is where the Kubernetes bootstrap authenticator looks
for them. A Secret reference always carries a namespace because the custom
resources are cluster-scoped.

## See Also

- [SSH provisioning]({{< relref "guides/ssh" >}}) for the key and bastion setup
  behind `spec.ssh`.
- [PXE and bare metal]({{< relref "guides/pxe" >}}) for the image build and boot
  workflow behind `spec.host.netboot`.
- [Networking custom resources]({{< relref "reference/networking/custom-resources" >}})
  for the net group kinds that consume Site CIDRs and tunnel settings.
- [Networking configuration]({{< relref "reference/networking/configuration" >}})
  for tunnel protocol and MTU selection.
- [Isolation modes]({{< relref "reference/agent/isolation" >}}) for how the agent
  runs each `spec.isolation` value.
- [Agent configuration]({{< relref "reference/agent/configuration" >}}) for the
  agent settings a Machine and a configuration version feed into.
- [Cloud API operations]({{< relref "guides/cloud-api" >}}) for the credential
  and provider model behind `MachineOperationCredential`.
- [CLI reference]({{< relref "reference/cli" >}}) for the `kubectl unbounded`
  commands that create and inspect these objects.
- [Architecture]({{< relref "reference/architecture" >}}) for the controllers
  that reconcile them.

