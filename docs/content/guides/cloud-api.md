---
title: "Cloud API Provisioning"
weight: 4
description: "Drive a cloud provider API to launch worker machines, join them to a site, and release their infrastructure through MachineClass and MachineAllocation."
---

## Overview

Cloud API provisioning asks the provider to create the machine instead of
borrowing a machine that already exists. You describe the capacity you want in
a `MachineClass`, a capacity consumer requests one machine with a
`MachineAllocation`, and a provider adapter turns that request into a launch
call against the provider's compute API. The resulting host boots with
first-boot data that installs the unbounded-agent, the agent joins the node to
the site, and the allocation reports the Node that registered for it.

The mental model has three layers. A `MachineClass` is a template: a site, a
provider name, a provider configuration, a configuration version, and a
catalog of offerings that the provider resolved from its own account. A
`MachineAllocation` is one consumer's request for one machine out of that
catalog, with the exact class incarnation frozen into its status before any
remote call happens. A `Machine` is the object the controller creates to
describe the resulting host, and a `MachineOperation` of kind `HostAllocate` or
`HostRelease` is the durable record of the provider work. Every remote effect is
owned by the allocation UID, so retries and restarts cannot silently create a
second host.

{{< callout type="warning" >}}
This guide is under development, and architecture details may change. We
strongly encourage you to engage with the community via GitHub issues,
discussions, or social media.

The design of the system is iterative, and your feedback helps us build a
system that solves your problems. We value the Unbounded community's input and
welcome your insights.

Let us know what you think!

- Issue tracker: [github.com/Azure/unbounded/issues](https://github.com/Azure/unbounded/issues)
- Discussions: [github.com/Azure/unbounded/discussions](https://github.com/Azure/unbounded/discussions)
{{< /callout >}}

## How API-driven provisioning differs from SSH and PXE

All three provisioning paths end with the same thing: a Kubernetes Node running
the unbounded-agent and joined to a site. They differ in who creates the host,
what the controller can assume about it, and where the first-boot data comes
from.

### SSH provisioning

With [SSH provisioning]({{< relref "guides/ssh" >}}), the machine already exists
and already runs Linux. You publish its address and a private key, the machina
controller connects over SSH, copies the install script and a generated agent
configuration, and waits for the Node to register. The controller needs inbound
reachability to the host, and a failure to connect is visible as a failed
provisioning attempt rather than as a missing cloud resource.

### PXE provisioning

With [bare metal PXE]({{< relref "guides/pxe" >}}), metalman owns the network
boot path. It serves DHCP, TFTP, and HTTP artifacts, drives the BMC over
Redfish, writes a disk image, and hands the host its first-boot data. The
machine is physical, the controller must reach the BMC, and the network has to
allow the provisioning traffic to the target segment.

### Provider API provisioning

With provider APIs, neither of those assumptions holds. There is no inbound SSH
to an existing host and no BMC to drive. The provider creates a virtual machine
from an image the adapter selects, passes the first-boot data as instance user
data, and returns an instance identity. The controller plans that identity
before the host exists (`status.snapshot.hostProviderID`), uses it as the kubelet
`providerID`, and later confirms that the Node which registered carries that
exact `providerID`. If a host exists but registers with a different identity,
the allocation reports `NodeConflict` instead of adopting it.

| Concern | SSH | PXE | Provider API |
| --- | --- | --- | --- |
| Who creates the host | You do | Metalman images it | The provider API does |
| Access the controller needs | SSH to the host | Redfish and the provisioning network | Provider credential and API endpoint |
| First-boot payload | Agent config and install script over SSH | Installer plus cloud-init | Instance user data |
| Identity proof | SSH host key fingerprint | Redfish certificate and TPM | Node `providerID` derived from the allocation UID |
| Teardown | You delete the host | You delete the host | `HostRelease` acts on resources the allocation owns |

The practical consequence is that API provisioning is the only path where
Unbounded both creates and destroys infrastructure. That is why this path has
its own ownership rules: an allocation may release only the provider resources
it recorded as created, and it may not guess.

## The participating controllers and resources

Five kinds in the `unbounded-cloud.io` group carry the API provisioning
contract. They are cluster-scoped, and their field-level schemas live in the
[CRD reference]({{< relref "reference/machina-crd" >}}).

| Kind | Short name | Role in API provisioning |
| --- | --- | --- |
| `MachineClass` | `mcls` | Provider-neutral template plus a catalog of offerings resolved from the provider account. |
| `MachineAllocation` | `malloc` | One consumer's request for one machine from a class; owns every remote effect by UID. |
| `MachineOperationCredential` | `mocred` | The one credential source for a site and provider pair. |
| `Machine` | `mach` | The host and its node instructions; allocation Machines also carry `spec.allocationRef` and `spec.fenced`. |
| `MachineOperation` | `mop` | Records `HostAllocate` and `HostRelease`, and the day-2 host operations the same provider can serve. |

Two controllers do the work. The `MachineClass` controller resolves class
inputs in a fixed order, calls the provider's `ResolveClass` callback, and
publishes normalized offerings with availability and expiry. The
`MachineAllocation` controller freezes a snapshot of those inputs, mints a
bootstrap token, creates the Machine, creates the `HostAllocate` operation, and
later fences the Machine and, once the consumer authorizes release and the fence
is confirmed, creates `HostRelease`.

A third path, a separate entry point on the same shared reconciler, executes the
provider callbacks for `HostAllocate` and `HostRelease`. The reconciler also
handles day-2 operations such as `HostPowerOff` and `HostReplace`.

### What ships in this tree today

The capacity contract is defined, covered by tests, and installable as RBAC and
admission policy manifests, but the end-to-end launch path is not wired into a
controller binary in this tree. Two facts make that precise:

- The only provider adapter under `internal/machineops/providers` is `azurevm`,
  and it registers only immediate host operations (`HostHardReboot`,
  `HostPowerOff`, `HostPowerOn`, `HostReplace`). It does not register capacity,
  so it cannot serve a `MachineClass` or launch an allocation.
- `cmd/machine-ops-controller` calls `AddToManager`, which registers the
  MachineOperation reconciler and sets up the allocation reconciler only when a
  selected provider declares capacity. It never calls `AddCapacityToManager`,
  which is what starts the MachineClass controller, and no selected provider
  declares capacity, so neither the MachineClass controller nor the
  MachineAllocation controller runs in that binary.

In other words, the schemas, controllers, provider interfaces, RBAC, and
admission policies for API provisioning are present and tested, and a provider
adapter that launches cloud VMs is not present in this tree. The rest of this
page describes the contract you implement and the objects you will create once
such an adapter is registered, and it calls out the parts that already run,
such as Azure VM day-2 operations.

## The MachineClass and its inputs

A `MachineClass` pins the two fields that decide where its machines come from:
`spec.siteRef` and `spec.provider` cannot change after creation, because an
allocation freezes the class UID, generation, and resolved revision before it
does anything remote. Changing the provider would otherwise leave an allocation
whose frozen inputs describe a different adapter. Every other class input
evolves by generation: you edit the class, the controller resolves it again, and
each allocation keeps the inputs it already froze.

The example below is the shape of a class. Replace the provider name with the
adapter registered in your deployment, and replace the site and configuration
names with objects that exist in your cluster.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineClass
metadata:
  name: gpu-pool
spec:
  siteRef:
    name: edge-west
  provider: <provider-name>
  providerConfigRef:
    kind: ConfigMap
    name: gpu-pool-provider-config
    namespace: unbounded-system
  configurationRef:
    name: worker
    version: 1
```

### The provider configuration

`spec.providerConfigRef` is restricted on purpose. It must name a core-group
`ConfigMap`, it must include a namespace, the namespace must be the one the
controller was configured to read, the ConfigMap must set `immutable: true`,
and it must not carry `binaryData`. Those rules let the controller hash the
content, record the hash in the class status, and trust later that the hash
still describes the object. The provider is the only component that interprets
the keys in that ConfigMap, so its contents are adapter-specific.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: gpu-pool-provider-config
  namespace: unbounded-system
immutable: true
data:
  region: westus3
  resourceGroup: unbounded-workers
  image: <provider-image-reference>
```

### The configuration version

`spec.configurationRef` must pin a version. The controller loads the matching
`MachineConfigurationVersion` and checks two things: that the version object's
`spec.version` equals the reference, and that its
`unbounded-cloud.io/machine-configuration` label equals the configuration name.
A mismatch is reported as a configuration mismatch instead of silently using
another generation of the content.

The version's template is frozen into each allocation, so a class does not
follow later edits to a `MachineConfiguration`. It also decides the
first-boot format. Allocated hosts support only `CloudInit`; a version whose
template declares `Ignition` fails the allocation with `InvalidRequest` before
any provider call, because the allocation path renders cloud-init user data.

### Bootstrap files and the agent artifact

`spec.bootstrap.filesRef` optionally adds a second, immutable ConfigMap of
bootstrap files. Every file is written to `/var/lib/unbounded/bootstrap` with
mode `0700`, and the required `run` key executes as root before the agent
installer starts. The installer runs only if that script succeeds, so a failing
bootstrap script fails the boot rather than producing a half-configured node.
Validation rejects a missing or blank `run` key, and it rejects bootstrap file
names that are not plain file names.

`spec.bootstrap.agentArtifact` overrides where the agent tarball comes from and
pins its SHA-256 digest. The URL must be an `https` URL with a DNS name or
dotted IPv4 host (optionally with a port) or a `file:///` path, and it must not
embed credentials, a query string, or a fragment. The digest is required and
must be 64 lowercase hex characters. When the field is omitted, the agent
installer uses its normal download source.

Neither bootstrap mechanism is a place for credentials. Bootstrap files become
part of host user data, and user data is visible to the provider and to anyone
who can read the instance metadata.

### Readiness and offerings

A class is usable only when it is `Ready` for its current generation and has at
least one current offering. Readiness requires that the controller observed the
current generation and published the resolved revision plus the snapshots for
the provider configuration, configuration version, site, and credential. The
controller reports failures as `Ready=False` with a reason that names the input,
such as `SiteNotFound`, `SiteDeleting`, `ProviderConfigNotFound`,
`ProviderConfigNotImmutable`, `BootstrapFilesInvalid`,
`ConfigurationNotFound`, `ConfigurationMismatch`, `AuthNotFound`, or
`AuthInvalid`. A failure does not clear the previous revision and offerings,
which lets existing allocations keep using the inputs they froze.

Offerings are the normalized capacity shapes the provider says it can sell.
Each offering carries a name that is unique within the class, an instance type,
an optional zone, a capacity type of `OnDemand` or `Spot`, optional labels and
capacity, optional overhead split into kube-reserved, system-reserved, and
eviction-threshold, an availability of `Available`, `Unavailable`, or
`Unknown`, an optional price, and an observation time. An offering is usable
only when it is `Available` and its `validUntil` is in the future. An offering
without `validUntil` is never considered current, because the controller cannot
tell whether the observation still holds.

`OfferingsCurrent` is the condition that summarizes this. It is true when at
least one offering is available and unexpired, and false with
`NoOfferingsAvailable` when none qualifies. The controller resolves the class
again on its refresh interval, or sooner when an offering expires, and it
watches the site, configuration versions, credentials, and the ConfigMaps it
reads.

```bash
kubectl get machineclasses
kubectl get machineclass gpu-pool -o yaml
kubectl get machineclass gpu-pool \
  -o jsonpath='{range .status.offerings[*]}{.name}{"\t"}{.instanceType}{"\t"}{.availability}{"\t"}{.capacityType}{"\n"}{end}'
```

### Provider credentials

A class resolves exactly one `MachineOperationCredential` for its site and
provider pair. Two or more matches are `AuthAmbiguous`; no match is
`AuthNotFound`. The credential names a site, a provider, and an authentication
mode.

`WorkloadIdentity` uses the identity the controller process already has, which
is the usual choice when the controller runs with a workload identity or a
managed identity. `ExternalPlugin` requires `spec.auth.secretRef` and delegates
the secret contents to the provider adapter. The adapter decides which keys it
requires. The built-in Azure VM adapter, for example, reads `tenantID`,
`clientID`, and `clientSecret` from that Secret and builds a service principal
credential.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperationCredential
metadata:
  name: gpu-pool-credential
spec:
  siteName: edge-west
  provider: <provider-name>
  auth:
    mode: ExternalPlugin
    secretRef:
      name: gpu-pool-provider-secret
      namespace: unbounded-system
```

The referenced Secret must live in the credential namespace the controller was
started with. A reference outside that namespace is rejected as forbidden, and
a missing Secret is reported as `AuthSecretNotFound`. Secret content is
deliberately excluded from the class's resolved revision, and the controller
does not watch Secrets. A rotation is therefore picked up on the next class
refresh or on another watched event, not immediately.

## The MachineAllocation and its request

A `MachineAllocation` is owned by its consumer, not by the platform. Its UID is
the owner of every remote effect, and its name is only the human label. If you
delete an allocation and recreate one with the same name, the new object has a
different UID, and nothing from the old allocation is reused. That rule is what
makes release safe: the controller may act only on provider resources whose
identities trace back to this UID.

| Field | Required | Notes |
| --- | --- | --- |
| `spec.consumerRef` | Yes | `apiGroup`, `kind`, `name`, and `uid` of the requesting object; immutable. |
| `spec.classRef` | Yes | `name` and `uid` of the exact `MachineClass`; immutable. |
| `spec.resources` | No | Minimum requested resources, used to filter offerings; immutable. |
| `spec.requirements` | No | Constraints on offering labels; immutable. |
| `spec.nodeTemplate` | No | Labels, taints, and startup taints applied to the Node at registration; immutable. |
| `spec.lifecycle` | No | Defaults to `Active` and may move only to `Released`. |

A requirement has a key, an operator, optional values, and an optional
`minValues`. The operators are `In`, `NotIn`, `Exists`, `DoesNotExist`, `Gt`,
and `Lt`; `Gt` and `Lt` compare the label value as an integer. `minValues` is
the consumer's flexibility floor: at least that many distinct values of the key
must remain among the offerings that satisfy every requirement, or the
allocation fails with `InsufficientCapacity`. Once the filter passes, the
controller resolves to one of the surviving offerings.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineAllocation
metadata:
  name: training-pool-0
spec:
  consumerRef:
    apiGroup: karpenter.sh
    kind: NodeClaim
    name: training-pool-0
    uid: <consumer-uid>
  classRef:
    name: gpu-pool
    uid: <machineclass-uid>
  resources:
    cpu: "32"
    memory: 256Gi
    nvidia.com/gpu: "8"
  requirements:
    - key: unbounded-cloud.io/capacity-type
      operator: In
      values:
        - OnDemand
  nodeTemplate:
    labels:
      workload: training
  lifecycle: Active
```

Both UIDs are required. The class UID is checked against the live object, and a
mismatch fails the allocation with `InvalidRequest`. The consumer UID is what
the controller compares when it checks whether the consumer still exists, so a
mismatch makes the allocation look orphaned. Copy both from the live objects
rather than typing them by hand:

```bash
kubectl get machineclass gpu-pool -o jsonpath='{.metadata.uid}{"\n"}'
kubectl get nodeclaim training-pool-0 -o jsonpath='{.metadata.uid}{"\n"}'
```

The controller treats the class reference as an exact incarnation. A class
deleted and recreated under the same name has a new UID and is therefore a
different class, even though the name in your manifest still resolves.

## The cloud provider contract

A provider adapter is a Go package that registers lifecycle strategies with the
shared `pkg/machineops` package. The registration is immutable once built, and
it is validated at construction time. A provider that registers capacity must
also register `HostAllocate` and `HostRelease` as long-running operations, and a
provider that registers either of those kinds must register capacity. One
without the other fails at startup rather than at the first allocation.

### Resolving a class into offerings

`ResolveClass` receives the class name and UID, the site name, the provider
configuration data, and the resolved credential. It must return normalized
offerings, each with truthful availability and a `validUntil` after which the
offering must not be used. It must not allocate infrastructure; it is a
catalog query. A permanent configuration error fails the class as not ready,
while a transient provider error is retried with backoff and does not clear a
previously healthy resolve.

### Planning a host identity

`PlanAllocation` receives the allocation name and UID, the planned Machine name,
the provider configuration, and the selected offering. It must be pure:
deterministic for the same request, with no remote calls and no reservations.
Its result carries the provider locator that becomes the Machine's
`host.external.providerID` before the host exists, and the host image the
provider will launch. A plan that returns an empty provider ID fails the
allocation with `InvalidRequest`.

The controller uses the allocation UID to generate a deterministic Machine
name, so the same allocation always plans the same host name and the same
provider locator. That is what allows a retry to distinguish "my host was
already created" from "somebody else's resource has this identity".

### Allocating and releasing

`HostAllocate` is a long-running operation with `Begin` and `Poll`. `Begin`
starts the provider work and must be idempotent for a stable operation UID:
repeated calls must return a usable handle for the same logical operation until
that handle has been persisted. `Begin` may return a permanent error only when
it created nothing. Once it may have created something, it must return a handle
and report failure, with the owned resources, through `Poll`.

`Poll` observes the operation and returns one of three outcomes. Success
reports the provider-native resource ID, the instance type, the zone, the image,
and the list of resources the allocation owns. Failure reports the same
ownership list so release can clean up. Cancellation does the same. Every
terminal outcome must set the allocation result; a missing report is treated as
`OwnershipUnknown`, which holds the allocation for manual recovery, because
release may act only on resources it can prove this allocation created.

`HostRelease` is also long-running. It receives the resources recorded as owned
and may act only on those. A provider that cannot prove ownership of a resource
must not delete it, even when its name matches.

These two kinds belong to the allocation controller alone. Metalman and the
in-VM agent do not process `HostAllocate` or `HostRelease`, and the shared
MachineOperation reconciler ignores them, so a hand-written allocation
operation is never executed by the day-2 path.

### The allocation snapshot

Before any remote call, the controller writes `status.snapshot` with the class
UID, generation, and resolved revision; the provider name; the site name and
UID; the exact credential reference; the selected offering; the provider
configuration, bootstrap files, and configuration content snapshots; the host
image; and the planned host provider ID. The snapshot is immutable and cannot be
removed. If the controller holds the allocation finalizer but the snapshot is
missing, it refuses to launch again, because launching could start a second
host, and it reports that manual recovery is needed.

## Bootstrap tokens, kubelet identity, and networking

An allocated host is not touched by an operator. Everything it needs to join
must arrive in its first-boot data, and everything the cluster uses to recognize
it must derive from the allocation, not from a later lookup.

### The allocation bootstrap token

The allocation controller mints a Kubernetes bootstrap token for the host. The
Secret lives in `kube-system`, has type `bootstrap.kubernetes.io/token`, and
carries the canonical site label plus the allocation UID label. Its name and
token ID derive from the allocation UID, so the same allocation always asks for
the same Secret. If a Secret with that name exists and is not labeled with this
allocation's UID, the allocation fails with `ProviderConflict` instead of
adopting a credential that belongs to something else. The token expires after 24
hours.

Admission narrows the controller's `kube-system` Secret writes to bootstrap token
Secrets that carry an allocation UID label. The validating admission policy in
`deploy/machine-ops/07-machineallocation-bootstrap-token-policy.yaml.tmpl`
applies that rule to any identity granted the `allocate` verb on Machines, which
keeps a compromised controller from writing arbitrary Secrets in `kube-system`.

The token is not the object that `kubectl unbounded site init` creates. Site init
issues a site-scoped join token, which SSH and manual flows reference through
`spec.kubernetes.bootstrapTokenRef`. The allocation path never reads that token:
it mints, labels, and later revokes a token for its own allocation UID.

### First-boot user data

Before calling `HostAllocate`, the shared MachineOperation reconciler builds the
user data and passes it to the provider. It reads the bootstrap token, resolves
the cluster endpoint, CA bundle, cluster DNS address, and Kubernetes version,
and renders a cloud-init document with the agent configuration and the install
command. The agent configuration is generated for the Machine name, so the node
name the host claims is fixed before the host exists.

On an allocation host, the kubelet configuration also carries a `providerID`.
That value is `unbounded:///machineallocations/<allocation-uid>`, and it is how
the controller later proves that the Node belongs to this allocation. The same
string is written to `status.providerID`, which is immutable once set.

If the class references bootstrap files, they are written first and the `run`
script executes before the installer. The install command runs only when that
script exits successfully, and the agent configuration and installer are
removed when the command exits. If there are no bootstrap files, only the
installer runs. The allocation path emits cloud-init only; it cannot generate
Ignition.

Because user data can contain the bootstrap token, providers must treat it as
secret. The provider contract requires an adapter to never log user data, never
persist it, and never copy it into a resume token, a status message, or an
error. A provider that violates that rule has leaked a join credential into its
own logs or object status.

### Node registration

The Node name is the Machine name, which the controller derives from the
allocation UID with the `ub-` prefix. When the controller sees a Node with that
name, it compares `Node.spec.providerID` with the allocation's provider ID. A
mismatch sets the `Registered` condition to false with `NodeConflict` and the
allocation does not adopt the Node. A match records `status.nodeRef`, copies the
Node's allocatable resources when they are reported, sets `Registered=True`, and
patches the Machine's `spec.kubernetes.nodeRef` so that later certificate
renewals are approved for that name.

### Site networking

The host joins through the site named in its class. The Machine and its token
carry `unbounded-cloud.io/site`, and the nodes the site manages carry the same
label. Connectivity between sites is not created by the allocation path. It is
the job of unbounded-net, which runs a WireGuard mesh with per-node key pairs
and gateway nodes for cross-site traffic. Read
[Networking]({{< relref "concepts/networking" >}}) for how tunnels, gateways,
and encapsulation are selected, and
[L3 connectivity]({{< relref "guides/l3-connectivity" >}}) for a walkthrough.

The practical split is that the provider path is responsible for a host that
boots, installs the agent, and registers; the networking components are
responsible for what that host can reach afterward. If a node registers but
cannot reach pods in another site, the provider call succeeded and the problem
is in the network layer, not in the allocation.

## Lifecycle phases and conditions

There are three separate state machines in play, and mixing them up is the most
common source of confusion when reading status. The allocation has conditions,
the MachineOperation has a phase, and the Machine has a phase that only the SSH
path drives.

### The allocation lifecycle

An allocation moves through these stages:

1. **Snapshot.** The controller verifies that it is in scope for the class,
   checks that the class is ready and its live inputs still match the published
   hashes, selects an offering, asks the provider to plan a host identity, and
   writes the snapshot, provider ID, instance type, zone, capacity type, image,
   capacity, allocatable floor, and labels in one status write.
2. **Protect and mint.** It adds its finalizer and labels, adds an input
   finalizer to each provider configuration and bootstrap files ConfigMap, and
   creates the bootstrap token Secret.
3. **Create the Machine and allocate.** It creates the Machine with the frozen
   configuration, labels, taints, and `spec.allocationRef`, then creates the
   `HostAllocate` MachineOperation. From this point the operation is recorded in
   `status.operations.allocate`.
4. **Observe the launch.** It copies the completed operation's result into the
   allocation status, including `status.providerResourceID`, the launched
   instance type, zone, and image, and the owned resources.
5. **Register.** It waits for the Node with the planned name and the planned
   provider ID, then records `status.nodeRef` and sets `Registered=True`.
6. **Retire and release.** When the consumer releases the allocation, deletes
   it, is itself deleted, or the Machine goes away, the controller fences the
   Machine and revokes the token. It creates `HostRelease` only after the
   consumer sets `spec.lifecycle` to `Released`, and only after the fence is
   confirmed.
7. **Commit.** Once release succeeds, `Released=True` is written, and only then
   are the allocation finalizers removed. A released allocation is left alone.

Fencing and release are separate on purpose. Fencing stops the host from
starting its node again and removes the join credential, but it never deletes
infrastructure by itself. Release deletes infrastructure, and it happens only
when the consumer says the workload and its storage are safe to lose.

### Allocation conditions

| Condition | Meaning |
| --- | --- |
| `Allocated` | The host launch is planned or complete. False reasons include `Launching`, `ClassNotReady`, `ClassInputsChanged`, and `SnapshotMissing`; the true reason is `Allocated`. |
| `Registered` | A Node with the planned name registered with the planned provider ID. False reasons include `NodeNotRegistered` and `NodeConflict`. |
| `Fenced` | The Machine may no longer repave or rejoin. Fencing does not release infrastructure. |
| `Released` | Every remote effect and the per-allocation bootstrap credential were released. |
| `Drifted` | Defined in the API to report that the frozen class revision differs from the class's current revision. No controller in this tree writes it, so compare the revisions yourself. |

`Drifted` is informational by design. A class that changes its provider
configuration, bootstrap files, or configuration template produces a new
resolved revision. Existing hosts keep running with the inputs they froze.
Changing them means creating a new allocation, not editing the old one. Because
the condition is not written in this tree, treat the revision comparison in
[Detect drift](#detect-drift) as the actual check.

### Failure reasons

A terminal launch failure sets `status.failureReason` and `status.failureMessage`
from a fixed vocabulary:

| Reason | Meaning |
| --- | --- |
| `InsufficientCapacity` | No offering satisfied the request, or the selected offering stopped being available before launch. |
| `QuotaExceeded` | The provider refused the launch for quota. |
| `InvalidRequest` | The allocation or its class cannot be satisfied as written, including bad label values, a missing class or site, a missing input ConfigMap, or a non-cloud-init configuration. |
| `AuthFailed` | The provider rejected the credential. |
| `ProviderConflict` | A provider resource, Machine, operation, or token with the planned identity exists and is not owned by this allocation. |
| `ProviderFailed` | The provider failed for another reason. |

### Machine and operation phases

`MachineOperation.status.phase` is `Pending`, `InProgress`, `Complete`, or
`Failed`. The `Completed` condition becomes true at a terminal phase. For
`HostAllocate`, `status.allocation` carries the provider's result, including
the owned resources, and is immutable once set. The API allows that result only
on `HostAllocate`, so a `HostRelease` reports through its phase and conditions
without an allocation result.

`Machine.status.phase` is `Pending`, `Rebooting`, `Provisioning`, `Joining`,
`Ready`, or `Failed`. Those phases describe the SSH provisioning state machine,
and the machina controller explicitly skips any Machine without `spec.ssh`.
An allocation Machine has no SSH configuration, so its phase is not the signal
to watch. Watch the allocation's `Allocated` and `Registered` conditions, the
Node object, and the MachineOperation phase instead. This is a real gap in the
current tree, not a documentation shortcut: no controller in this tree drives
`Machine.status.phase` for external hosts.

## Creating a machine through the API

Creating a machine is a sequence of declarative objects, not a single command.
You declare a site, a configuration, a class, and a credential once, then each
machine is one `MachineAllocation` object.

### Prepare the site and configuration

If the site does not exist, create it with the CLI. The command installs the
site resources and issues the site join token, and it is also the prerequisite
for any class that names that site.

```bash
kubectl unbounded site init \
  --name edge-west \
  --cluster-node-cidr 10.0.0.0/16 \
  --cluster-pod-cidr 10.244.0.0/16 \
  --node-cidr 10.10.0.0/16 \
  --pod-cidr 10.200.0.0/16
```

Then create a configuration profile for the worker nodes. The machina
controller creates version 1 of the configuration automatically, which is the
version the class pins.

```bash
kubectl unbounded machine config create worker \
  --kubernetes-version v1.34.0 \
  --agent-image ghcr.io/azure/unbounded-agent:latest
```

The worker template must not request `Ignition`. Allocated hosts support only
`CloudInit`, and a class whose configuration version declares another format
fails every allocation before the provider is called.

### Declare the class and credential

Apply the provider configuration ConfigMap, the class, and the credential in the
order the controller reads them. The class must reference a provider adapter
that is registered in the controller process; the name is not free-form.

```bash
kubectl apply -f gpu-pool-provider-config.yaml
kubectl apply -f gpu-pool-class.yaml
kubectl apply -f gpu-pool-credential.yaml
```

Wait for the class to become ready before requesting machines. A class that is
not ready will hold every allocation at `ClassNotReady`, and the allocation will
not call the provider.

```bash
kubectl wait machineclass/gpu-pool \
  --for=condition=Ready \
  --timeout=120s
```

{{< callout type="note" >}}
On a cluster built from this tree, the class does not become ready and the
allocation is not launched, because `cmd/machine-ops-controller` starts neither
the MachineClass controller nor the MachineAllocation controller and registers
no capacity provider. The objects and their validation still apply, so the
manifests below are the correct shape, but the launch path requires a provider
adapter wired through `AddCapacityToManager`.
{{< /callout >}}

### Create the allocation

Write the allocation with the class UID and the consumer UID filled in, then
apply it. Do not edit any other field after creation; the API server rejects
changes to the consumer, class, resources, requirements, and node template.

```bash
kubectl apply -f training-pool-0.yaml
```

If you do not have a real consumer object, an allocation still needs a
`consumerRef` that names something with a UID. When the consumer's kind is one
of the controller's watched consumer kinds, the controller compares the UID
when it checks whether the consumer is gone, and a consumer it cannot resolve is
treated as gone, which retires the allocation. Point the reference at a real
object whose lifetime you control, not at a fabricated UID. If the controller
was started with no watched consumer kinds, consumer loss is never observed.

### The Karpenter path

Karpenter does not create Machines directly. The Unbounded Karpenter plugin
reads `MachineClass` objects and creates a `MachineAllocation` for each
`NodeClaim` it decides to launch. The plugin is a separate Go module under
`plugins/karpenter`, and it depends only on the common class and allocation
types, not on the rest of the repository. Build it with `make karpenter`.

From the cluster's point of view the result is the same allocation object
described above, with a `NodeClaim` as the consumer. Karpenter then waits for
the allocation to become a Node, and its lifecycle controller deletes the
`NodeClaim` when the allocation cannot become one. See
[Automation patterns]({{< relref "guides/operations/automation" >}}) for how
operation objects fit into an autoscaling workflow.

## Observing a machine

The allocation is the primary object to watch, because it aggregates the plan,
the provider result, the token, and the Node registration. The Machine and the
MachineOperation are supporting records.

### Read the allocation status

```bash
kubectl get machineallocations
kubectl get machineallocation training-pool-0 -o yaml
```

The printer columns give a fast summary: the class name, the lifecycle, the
`Allocated` condition, the registered Node name, and the object's age. The
status body gives the details:

```bash
kubectl get machineallocation training-pool-0 -o jsonpath='{.status.providerID}{"\n"}'
kubectl get machineallocation training-pool-0 \
  -o jsonpath='{.status.providerResourceID}{"\t"}{.status.instanceType}{"\t"}{.status.zone}{"\n"}'
kubectl get machineallocation training-pool-0 \
  -o jsonpath='{range .status.conditions[*]}{.type}{"="}{.status}{" "}{.reason}{": "}{.message}{"\n"}{end}'
```

The provider ID should be `unbounded:///machineallocations/` followed by the
allocation UID. The resource ID is the provider's native instance identity.
`status.labels` records the full label set selected for the allocation: the
configuration template's node labels, the allocation's node template, and the
labels derived from the offering, such as instance type, capacity type, and
zone. The labels passed to kubelet leave out the well-known labels kubelet sets
itself, such as instance type and zone, and register the rest. The site label is
applied to the Node by the net controller, not by the kubelet.

### Follow the operation

```bash
kubectl get machineoperations
kubectl get machineoperation training-pool-0-allocate -o yaml
```

An allocation's operations use deterministic names derived from the Machine
name: `<machine>-allocate` and `<machine>-release`. While the launch is in
flight, the phase is `InProgress`. A `Complete` operation carries
`status.allocation` with the owned resources. A `Failed` operation carries the
normalized reason and message, and the allocation mirrors it into
`status.failureReason` and `status.failureMessage`.

### Check the Node and the token

```bash
kubectl get node ub-<allocation-uid-without-dashes> \
  -o jsonpath='{.spec.providerID}{"\n"}'
kubectl get nodes -l unbounded-cloud.io/site=edge-west
kubectl get secrets -n kube-system \
  -l unbounded-cloud.io/allocation-uid=<allocation-uid>
```

The Node's provider ID must equal `status.providerID` on the allocation. If the
Node name exists but the provider ID differs, the allocation reports
`NodeConflict` and does not adopt the Node. If the token Secret is missing, the
host cannot join even when the provider call succeeded, so check both.

### Detect drift

```bash
kubectl get machineallocation training-pool-0 \
  -o jsonpath='{.status.snapshot.classRevision}{"\n"}'
kubectl get machineclass gpu-pool \
  -o jsonpath='{.status.resolvedRevision}{"\n"}'
```

Different values mean the class moved on after this allocation froze its
inputs. The `Drifted` condition is defined for this case, but no controller in
this tree writes it, so this comparison is how you detect it. Adopting the new
inputs means creating a new host.

## Idempotency and retries

Every step of the launch path is designed so that a controller restart, a
duplicate reconcile, or a repeated provider call does not create a second host
or a second credential. The mechanism is a set of deterministic names, frozen
status, and create-or-adopt logic.

### Deterministic object identities

All names an allocation creates derive from the allocation UID alone. The
Machine is `ub-` followed by the UID with its dashes removed. The operations are
that name plus `-allocate` and `-release`. The bootstrap token ID is six
characters derived from a SHA-256 of the UID over a fixed alphabet, and the
Secret name is the standard bootstrap token name for that ID. The input
finalizer contains the UID as well, so one allocation cannot unprotect another
allocation's ConfigMaps.

Because the names are deterministic, a retry looks up the same objects instead
of creating new ones. A missing or replaced object is detected by UID: the
controller confirms cache misses with a live read, and it records the UIDs of
the Machine and the operations in `status.machineRef` and
`status.operations`.

### Frozen inputs and write-once status

The allocation snapshot is written once and is immutable. `status.providerID`
is immutable once set, and the recorded allocate and release operations are
immutable once set. The API server enforces these rules with CEL validations on
the resource, and the controller refuses to proceed when the finalizer is
present but the snapshot is missing. That combination is what prevents a
restarted controller from launching a second host after it lost the status that
described the first one.

### Create or adopt

When the controller needs a Machine, a token, or an operation, it first reads
the object by its deterministic name. If the object exists, the controller
checks that it is exactly the object it would have created: the same allocation
reference, the same machine reference, the same operation kind, and no extra
parameters, selector, or TTL. An object that differs is a conflict, and the
allocation fails with `ProviderConflict` rather than adopting it. This matters
most for the token, because adopting a Secret minted for another allocation
would hand this host someone else's join credential.

### Provider replay

The provider contract pushes the same discipline into the adapter. `Begin` must
be idempotent for a stable operation UID, and it must key replay on the
allocation UID as well as the operation UID, because the allocation UID is the
owner of every effect. `Poll` is retried with the same persisted handle on an
ordinary error, and a `PermanentError` terminates the operation. A provider that
loses its handle must still be able to answer "did this operation already create
something" from its own records, because the controller will not call `Begin`
again with a fresh identity.

Immediate operations can declare themselves replay safe, which allows the
controller to execute them again when reconciliation resumes from `InProgress`.
Allocation operations do not use that flag; they are long-running by
construction, and their safety comes from the begin and poll contract.

### Retry pacing

A class that is not ready, a live input that changed, or an operation the
controller is waiting for causes a requeue after 15 seconds. Transient provider
errors are returned to the workqueue so the controller-runtime backoff applies.
The class controller resolves on its refresh interval, which defaults to five
minutes and is shortened to the earliest offering expiry when that is sooner.
A launch that takes longer than the configured stall threshold is marked with
the `ProviderOperationStalled` condition on the operation, but the controller
keeps polling at the stalled interval rather than failing the allocation.

## Credentials handling

Credentials are separated from every object that becomes user data. A
`MachineOperationCredential` names a site and a provider, and it either uses the
controller's own identity or points at a Secret. The provider adapter receives
the resolved credential with each callback and is the only code that interprets
its contents.

### Choose an authentication mode

`WorkloadIdentity` is the default choice for a controller that runs with a
managed identity or a projected service account token. The adapter uses the
ambient credential chain, so there is no Secret to rotate. The built-in Azure
VM adapter falls back to `DefaultAzureCredential` when the mode is empty or
`WorkloadIdentity`.

`ExternalPlugin` is for a static credential or a credential that the platform
injects as a Secret. It requires `spec.auth.secretRef`, and the referenced
Secret must exist in the controller's credential namespace. The adapter decides
which keys it needs. The Azure VM adapter requires `tenantID`, `clientID`, and
`clientSecret` and builds a client secret credential from them. A missing key is
an error from the adapter, not a generic authentication failure.

### Keep credentials out of user data

A class must never put credentials in `spec.bootstrap.filesRef`. Those files
become part of instance user data, and the provider contract says user data may
contain credentials and must not be logged or persisted. The bootstrap token is
the only credential the allocation deliberately places in user data, and it
expires after 24 hours. Provider credentials stay in the controller's Secret and
never travel to the host.

Secret content is excluded from the class's resolved revision, so rotating a
Secret does not mark every allocation as drifted. The controller does not watch
Secrets, so a rotation is observed on the next class refresh or another watched
event. Plan for that delay when you rotate a credential that an in-flight
launch depends on. A launch that fails with `AuthFailed` may simply have used
the old Secret.

### Credential failures

Each failure names the object that caused it. `AuthNotFound` means no
`MachineOperationCredential` matches the site and provider pair. `AuthAmbiguous`
means more than one does, and you must delete or rename the extra one.
`AuthInvalid` covers an empty mode, an `ExternalPlugin` credential without a
Secret reference, or an incomplete reference. `AuthSecretForbidden` means the
reference points outside the allowed namespace, and `AuthSecretNotFound` means
the Secret is absent. These appear as `Ready=False` reasons on the class, which
is why a class that suddenly stops resolving usually points at a credential
change.

## Day-2 operations on API-provisioned hosts

Once a host exists, the same provider registration can serve host-level
operations. This is the part of the API path that runs in this tree today: the
built-in `AzureVM` adapter registers `HostHardReboot`, `HostPowerOff`,
`HostPowerOn`, and `HostReplace`, and `cmd/machine-ops-controller` starts the
reconciler that executes them.

The adapter accepts a Machine whose host is Azure-owned, either through
`spec.host.azure.resourceID` or through `host.external.provider` set to
`AzureVM`. Provider IDs may use the Kubernetes-style
`azure:///subscriptions/<subscription>/resourceGroups/<group>/providers/Microsoft.Compute/virtualMachines/<name>`
form or the `azure:` prefix, and the adapter normalizes the result to the Azure
Resource Manager ID. A provider ID that does not identify a
`Microsoft.Compute/virtualMachines` resource is rejected.

```bash
kubectl unbounded machine hard-reboot <machine-name>
kubectl unbounded machine replace <machine-name> --force
```

`HostReplace` is destructive. The adapter deletes the VM, then creates it again
from the original model with new bootstrap user data, and optionally a new host
image. OS disk state is lost. The operation needs
`spec.kubernetes.bootstrapTokenRef` on the Machine, because the controller has
to render fresh cloud-init, and it needs the controller to know the API server
endpoint. The generated user data is cloud-init only, and the Azure custom data
limit of 65535 bytes applies, so a very large bootstrap payload fails
validation before the VM is touched.

A release is not a day-2 operation. `HostRelease` belongs to the allocation, and
the shared MachineOperation reconciler deliberately ignores it outside the
allocation path. Do not hand-write one and expect the day-2 controller to run
it.

## Troubleshooting

Start with the class, then the allocation, then the operation, then the Node.
Each layer reports a different kind of failure, and guessing across layers
wastes time.

| Symptom | Likely cause | Check |
| --- | --- | --- |
| Class never becomes `Ready` | Provider not registered, input ConfigMap rejected, configuration version mismatch, or credential missing | `kubectl describe machineclass <name>` and the condition reason |
| Class `Ready=False` with `ProviderConfigNotImmutable` | Referenced ConfigMap is missing `immutable: true` | `kubectl get configmap -n <ns> <name> -o yaml` |
| Class `Ready=False` with `ProviderConfigForbidden` | ConfigMap is outside the provider-config namespace the capacity controller was started with | Compare with `CapacityOptions.ProviderConfigNamespace` |
| Class `Ready=False` with `AuthAmbiguous` | More than one credential matches the site and provider | `kubectl get machineoperationcredentials` |
| Class `Ready=False` with `AuthSecretNotFound` | Secret reference is wrong or the Secret is in another namespace | `kubectl get secret -n <ns> <name>` |
| Allocation stays at `ClassNotReady` | The class has no ready revision, so nothing can be frozen | Fix the class first |
| Allocation fails `InvalidRequest` naming a ConfigMap | An input ConfigMap was deleted, replaced, or edited | Restore the exact immutable content the class resolved |
| Allocation fails `InvalidRequest` naming the format | Configuration version declares `Ignition` | Edit the configuration and pin a cloud-init version |
| Allocation fails `InsufficientCapacity` | Resources exceed every offering, requirements exclude all, or the offering expired | Inspect `status.offerings` and the offering expiry |
| Allocation fails `ProviderConflict` | A Machine, operation, or token with the planned name exists and is not this allocation's | Inspect the object's labels, especially the allocation UID label |
| `Allocated=False` with `Launching` for a long time | The provider operation has not completed | `kubectl get machineoperation <machine>-allocate -o yaml` |
| Operation reports `ProviderOperationStalled` | The provider accepted the work but exceeded its expected duration | Check the provider console and the controller logs |
| `Registered=False` with `NodeNotRegistered` | The host did not join with the planned name | Check the Node, the token Secret, and the host console output |
| `Registered=False` with `NodeConflict` | A Node with that name has a different provider ID | Delete the stale Node only after confirming it is not the intended host |
| `Released=False` with `AwaitingRelease` | The consumer retired the allocation but did not authorize release | Set `spec.lifecycle: Released` after workload and storage safety checks |
| `Released=False` with `ReleaseFailed` | The provider refused or failed the release | Inspect the release operation and the owned resources |
| Frozen and current class revisions differ | The class has a newer resolved revision | Expected; create a new allocation to adopt the new inputs |

### The allocation never launches

If the class is ready and the allocation still has no `status.snapshot`, check
whether the allocation controller is running at all. In this tree, nothing calls
`AddCapacityToManager`, so no MachineClass controller starts, and because no
provider declares capacity, `AddToManager` skips the allocation controller too.
A cluster that applies the CRDs from this tree gets valid objects that stay
unlaunched. That is the single most important thing to check before debugging a
provider.

### The host launches but never registers

Work outward from the token. Confirm the Secret exists, is labeled with the
allocation UID, and has not expired. Then confirm the user data reached the
instance: many providers expose the instance user data or a serial console,
which shows whether the bootstrap script ran and whether the agent installer
failed. A bootstrap `run` script that exits non-zero stops the installer by
design. A host that runs the installer but cannot reach the API server usually
reports a TLS or connection error in the same console output.

The Node name is fixed before boot. If the host joins under a different name,
the allocation will never see it, and the `Registered` condition stays false.
Check that the agent configuration served to the host carries the planned node
name.

### Release does not finish

Release has two gates. The allocation must be retired, which happens when the
consumer sets `spec.lifecycle: Released`, deletes the allocation, is itself
deleted, or the Machine goes away. And the fence must be confirmed: the Machine
must be fenced, being deleted, or gone, and the token must be revoked. If the
consumer is gone and the lifecycle is still `Active`, the controller waits
rather than deleting infrastructure, and reports `AwaitingRelease`. That is the
intended safety behavior, not a stuck controller.

### Manual recovery

Three conditions require a human: `SnapshotMissing` when the finalizer is
present but the snapshot is gone, `AllocateOperationMissing` when the recorded
`HostAllocate` was deleted or replaced, and `OwnershipUnknown` when the provider
did not report the resources it created. In all three cases the controller
holds the allocation rather than guessing. Do not delete provider resources
blindly, because release is limited to recorded ownership and you may delete
something another allocation owns. Recover the provider inventory first, then
remove the allocation finalizer only when you have accounted for every resource.

### Getting help

Include the class conditions, the allocation status, the operation status, and
the controller logs when you ask for help. The controller logs the scope it was
started with, which tells a maintainer whether the object was in scope at all.
Redact user data and Secret contents before posting; both can contain
credentials.

## Related

- [CRD reference]({{< relref "reference/machina-crd" >}}) for every field,
  default, and condition in the `unbounded-cloud.io` group.
- [Architecture]({{< relref "reference/architecture" >}}) for how the
  controllers and operators fit together.
- [SSH provisioning]({{< relref "guides/ssh" >}}) for an existing host that you
  want to convert into a worker node.
- [Bare metal PXE]({{< relref "guides/pxe" >}}) for physical hosts that need
  network boot and BMC control.
- [Networking]({{< relref "concepts/networking" >}}) for the WireGuard mesh and
  gateway model that carries traffic after the node registers.
- [Automation patterns]({{< relref "guides/operations/automation" >}}) for
  driving operations from CI, GitOps, and autoscalers.

