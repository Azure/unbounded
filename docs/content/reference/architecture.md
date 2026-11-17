---
title: "Architecture"
weight: 1
description: "How the Unbounded operator, controllers, provisioning adapters, node agent, networking data plane, and Racer fit together inside a Kubernetes cluster."
---

## Overview

Unbounded Kubernetes is not a Kubernetes distribution and does not replace a
control plane. It installs custom resource definitions (CRDs), an operator, and
a set of controllers into an existing conformant cluster, then uses them to
provision worker hosts and connect those hosts back to the cluster network. The
cluster that receives these components is the management cluster for every
`Site` and `Machine` you create, even when the worker hosts run in another
cloud, another region, a rack, or an edge location.

The architecture separates three concerns that a normal cluster leaves implicit.
Desired state lives in cluster-scoped custom resources: a `Site` describes a
location and its address plan, and a `Machine` describes one host. Control-plane
controllers reconcile that desired state by creating component workloads,
provisioning hosts, issuing credentials, and allocating addresses. A per-node
data plane, made up of the agent and the networking node agent, turns each host
into a Kubernetes node and carries pod traffic across locations.

The mental model is a loop. You declare a `Site` and a `Machine`. A provisioning
adapter reaches the host and installs the agent. The agent builds the node stack
and completes TLS bootstrap, so a Kubernetes `Node` appears. The networking
controller then matches the node to its `Site`, assigns pod CIDRs, and the
networking node agent programs the CNI and tunnel data plane. Optional
components such as gantry, the token refresher, metalman, and Racer are
reconciled from the same `Site` API, so one object describes both the location
and the software that serves it.

![Unbounded architecture showing the management cluster, provisioning adapters, worker hosts, and encrypted tunnels](../../img/architecture-overview.svg)

This page describes each layer, the resources that connect them, and the
lifecycle of a `Machine` from registration to `Ready`. It assumes you have read
the [Project Overview]({{< relref "concepts/overview" >}}) or are comfortable
with Kubernetes controllers, CRDs, and node bootstrap.

## Components

### The management cluster

The management cluster can be AKS, EKS, GKE, a self-managed cluster, or any
other conformant Kubernetes distribution. Unbounded adds API types and
controllers to it, but it does not own the API server, the scheduler, or the
controller manager. Your existing cluster continues to run the control plane,
and worker nodes join it through the normal Kubernetes bootstrap and
certificate-issuance path.

Installation starts with `kubectl unbounded install`. The command applies the
embedded operator manifests with server-side apply, waits for the operator
rollout by default, and then waits for the CRDs the operator establishes. The
operator is the component that owns the CRD lifecycle after that point: it
installs and upgrades the CRDs at startup and repairs their deletion or drift.
This ordering matters because the typed `Site` informer cannot sync until the
`Site` CRD is served.

By default the operator and the workloads it manages run in the
`unbounded-system` namespace. The namespace is not hard-coded into the binaries.
The operator accepts a `--namespace` flag, and components read `POD_NAMESPACE`
when it is present, so an installation can be moved to another namespace without
rebuilding the binaries. The legacy namespaces `unbounded-kube` and
`unbounded-net` are recognized only as migration sources; the operator refuses
to install into them because its migration reaper would later delete them.

Every Unbounded custom resource shown here is cluster-scoped. A `Site`, a
`Machine`, a `MachineClass`, and a `ClusterCache` describe shared infrastructure
rather than a namespaced workload, so their names are unique across the cluster.
A `Machine` can still reference a namespaced Secret for an SSH key or a BMC
password, and the reference carries the namespace because the Secret does.

The operator is leader-elected and reconciles from the full set of `Site`
objects. The unbounded-net controller and metalman use leader election, and
machina supports it but disables it in its default configuration. The networking
node agent and the Racer data plane are DaemonSets, so they run on every node and
do not hold a cluster-wide leader.

### The operator and its component model

The operator is best understood as a registry-driven reconciler rather than a
fixed list of Deployments. Every component implements one of two interfaces. A
cluster component reconciles once per pass from the full set of `Site` objects
and is never tied to one `Site`; examples are networking, machina, gantry, the
token refresher, Racer, and the metalman bootstrap policy. A per-site component
runs only when a `Site` is present and is enabled there; metalman is the current
example.

Each component plans before anything is written. Planning may read cluster
state, because decisions such as whether a ConfigMap already exists depend on
it, but planning must not write. The executor applies the resulting plan, and
the operator records one status condition per component on the `Site`. That
split is why a failed plan does not leave a half-applied workload, and why the
`Site` conditions are a reliable readiness signal.

The built-in registry contains these components:

| Component | Interface | Condition | Responsibility |
|---|---|---|---|
| unbounded-net | Cluster | `NetReady` | Networking controller and node agent workloads |
| machina | Cluster | `MachinaReady` | SSH provisioning controller and daemon CSR approver |
| gantry | Cluster | `GantryReady` | Peer-to-peer OCI distribution agent |
| token-refresher | Cluster | `TokenRefresherReady` | Maintains bootstrap tokens for joining machines |
| racer | Cluster | `RacerReady` | On-demand Racer data plane for `ClusterCache` objects |
| metalman bootstrap | Cluster | `MetalmanBootstrapReady` | Bootstrap identity and admission policy for PXE |
| metalman | Site | `MetalmanReady` | One PXE controller per `Site` |

Networking and machina are required cluster singletons. The operator deploys
them whenever at least one `Site` exists and keeps them reconciled if an
installation already exists. Machina cannot be disabled through the `Site` API;
the `spec.components.machina` field is a deprecated compatibility setting that
the operator ignores. Gantry and the token refresher default to enabled and can
be opted out per `Site`. The token refresher is deliberately not run for the
reserved `cluster` Site. Metalman is opt-in through
`spec.components.metalman.enabled`.

Components install at the operator's own version and always target the
operator's namespace, so `Site` does not expose an image or namespace field. The
supported customization surface is a separate cluster-scoped ConfigMap named
`unbounded-component-overrides`. It can patch generated Deployments and
DaemonSets, for example to change an image or add an argument, and the operator
reports the result in `Site` status. Write access to that ConfigMap is
equivalent to cluster-admin, because the workloads it patches already run with
host networking, privileged containers, and host path mounts. See
[Workload Overrides]({{< relref "reference/workload-overrides" >}}) for the
format and the integrity rules.

### The command-line plugin

`kubectl-unbounded` is the operator-facing client. It is not a controller and it
does not stay running. It bootstraps the operator, initializes a `Site`, creates
the bootstrap token for that `Site`, registers `Machine` objects, generates
manual bootstrap payloads, and creates `MachineOperation` resources for day-2
actions. It uses the same Kubernetes API and server-side apply as the
controllers, so a resource created by the CLI is indistinguishable from one
created by any other client.

The CLI is also the place where install defaults are resolved. `install` accepts
`--namespace`, `--operator-image`, `--image-registry`, and
`--api-server-endpoint`. The API server endpoint is advertised to provisioned
machines; when it is empty, the operator and machina auto-discover it from
`kube-public/cluster-info`, or from the `KUBERNETES_SERVICE_HOST` FQDN on
clusters such as AKS that do not publish cluster-info.

## Custom Resources

Unbounded uses two API groups. Host and lifecycle resources live in
`unbounded-cloud.io/v1alpha3`. Network topology resources live in
`net.unbounded-cloud.io/v1alpha1`. Keeping the groups separate lets you grant a
networking operator access to topology without also granting access to machine
credentials, and it keeps the machine API independent of the networking
implementation. Racer adds a third group, `racer.unbounded-cloud.io/v1alpha1`,
for cache declarations.

### Site

A `Site` is a top-level location, not a namespace and not a cluster. It groups
nodes by the IP ranges they use, defines the pod address pools they may draw
from, and selects the components that run for that location. `Site` is
cluster-scoped, uses the short name `st`, and is served by
`unbounded-cloud.io/v1alpha3`.

The networking fields define membership and addressing. `spec.nodeCidrs` lists
the CIDRs that contain the internal IPs of nodes at the site. The networking
controller matches a node's internal address against those ranges and labels the
node with `unbounded-cloud.io/site=<name>`. `spec.podCidrAssignments` lists the
CIDR pools, per-node block sizes, node name patterns, and priorities used to
allocate pod addresses. Lower priority values win when several assignments
match. `spec.manageCniPlugin` controls whether the node agent writes CNI
configuration and meshes same-site nodes, and it supplies the default for
assignments that do not set `assignmentEnabled` themselves.

The remaining networking fields tune the data plane. `spec.localCidrs` marks
ranges that must never be routed through a gateway pool,
`spec.nonMasqueradeCIDRs` keeps selected destinations from being masqueraded on
egress, and `spec.healthCheckSettings` configures tunnel peer health checks.
`spec.tunnelProtocol` selects `WireGuard`, `IPIP`, `GENEVE`, `VXLAN`, `None`, or
`Auto`, and `spec.tunnelMTU` bounds the MTU on routes through tunnels. `Auto` is
the default: links that use external addresses choose WireGuard, and links that
use only internal addresses choose GENEVE.

`spec.components` is the provisioning side of the same object. It enables
metalman for a site, opts gantry out when a location should not run it, and
controls the token refresher. Because components are part of the `Site`, a
single apply describes both the location and the software that serves it.

`Site` status reports `nodeCount`, `sliceCount`, and one condition per
component. The condition names are the ones in the component table above, so
`kubectl wait --for=condition=MetalmanReady site/<name>` is a supported way to
wait for a component rollout. The bootstrap flow creates two `Site` objects: the
reserved `cluster` Site for the management cluster's own address ranges, and a
`Site` for the remote location. The reserved Site participates in routing like
any other, but it does not get a token refresher.

### Machine

A `Machine` is a cluster-scoped resource that represents one host and owns its
provisioning and lifecycle state. It uses the short name `mach` and is served by
`unbounded-cloud.io/v1alpha3`. The resource does not run anything itself. It
records how the host can be reached, how it should boot, which Kubernetes
version and labels it should use, and where the agent should run.

The reachability and boot fields are mutually shaped by the adapter you use.
`spec.ssh` holds the host name or address, the SSH username, the private key
reference, and an optional bastion. It is the input to machina.
`spec.host.netboot` holds the bare-metal boot configuration: the machine image,
an optional netboot image, DHCP leases, Redfish access, the boot protocol, and
the target disk. It is the input to metalman. `spec.pxe` is the deprecated
top-level form of the same idea and is still resolved for existing objects. A
`Machine` may set at most one host owner among `netboot`, `azure`, and
`external`; `spec.provider` and `spec.providerID` are legacy fields that new
callers should avoid.

The Kubernetes fields are adapter-independent. `spec.kubernetes.version`
selects the version to install and falls back to the management cluster's server
version. `spec.kubernetes.nodeLabels` and `spec.kubernetes.registerWithTaints`
are passed to kubelet. `spec.kubernetes.bootstrapTokenRef` names a bootstrap
token Secret in `kube-system`, and `spec.kubernetes.nodeRef` records the Node
that corresponds to the Machine. By convention the Node name equals the Machine
name; `nodeRef` exists for the cases where it does not.

The agent and isolation fields control the node stack. `spec.agent` selects the
agent image and the download sources for the rootfs binaries.
`spec.isolation` declares where the node stack runs: `nspawn` (the default),
`none`, or the experimental `container`. Setting the field does not move a
running node. The change happens at the next repave, and a change that requires
reprovisioning also needs the `unbounded-cloud.io/allow-reprovision: "true"`
annotation, so declaring a mode never wipes a host by itself.

`spec.configurationRef` points at a `MachineConfiguration` and optionally a
specific version. `spec.allocationRef` identifies the `MachineAllocation` that
created the Machine and is immutable after creation. `spec.fenced`, which
requires an allocation reference, forbids the agent from starting the node
again; it can only change from false to true.

Status is where the lifecycle becomes observable. `status.phase` carries
`Pending`, `Rebooting`, `Provisioning`, `Joining`, `Ready`, or `Failed`.
`status.conditions` carries the detailed state, including `Provisioned`,
`SSHReachable`, `Provisioning`, `AgentBootstrapped`, `NodeUpdated`,
`ConfigurationPending`, `IsolationDrift`, and `RepavePending`. Status also
records fields for an observed SSH fingerprint, the pinned Redfish certificate
fingerprint, the TPM endorsement key, the last bootstrap credential, the applied
agent settings, and the configuration version that was applied. Not every field
is written by every path; the controller that owns the step writes the state it
can observe.

### Configuration, operations, and capacity

`MachineConfiguration` is a versioned configuration profile for a class of
machines. Its `spec.template` holds host, Kubernetes, and agent settings.
`spec.machineSelector` assigns the configuration to matching new machines,
`spec.priority` resolves competing matches (higher wins, with lexicographic name
order as the tiebreaker), and `spec.revisionHistoryLimit` bounds retained
versions. `spec.updateStrategy` defaults to `OnDelete`, where you cordon, drain,
and delete the Node to trigger the repave; `RollingUpdate` is reserved for a
future implementation. The controller creates `MachineConfigurationVersion`
objects named `<configuration>-v<number>`. A version is editable until a machine
uses it; after that it is treated as immutable, and cleanup skips deployed
versions and versions with deployed machines. The binding controller resolves
the Machine's desired version and publishes the `ConfigurationPending`
condition when no configuration is assigned or matched.

`MachineOperation` is the day-2 action resource, modeled on a Kubernetes Job. It
targets a Machine by `spec.machineRef` or creates one child per match from
`spec.machineSelector`. `spec.operationKind` selects the action, and
`spec.parameters` carries operation-specific values. The operation phases are
`Pending`, `InProgress`, `Complete`, and `Failed`, and
`spec.ttlSecondsAfterFinished` cleans up terminal operations. The agent handles
node-local operations such as `NodeReboot`, `HostSoftReboot`, `AgentUpgrade`,
and `AgentReset`. Metalman handles PXE host operations. The machine-ops
controller handles provider host operations. Operations for a boundary a
controller does not own are ignored by that controller.

`MachineClass` and `MachineAllocation` are the capacity contract used by
autoscalers. A `MachineClass` names a `Site`, a provider adapter, a provider
configuration ConfigMap, and a pinned `MachineConfiguration` version. It
publishes offerings that consumers such as the Unbounded Karpenter plugin
present as instance types. A `MachineAllocation` is one consumer's request for
one machine from a class. It records the consumer by UID, the exact class by
UID, the requested resources, requirements, and a node template. Its lifecycle
moves only from `Active` to `Released`, and its UID owns every remote effect made
on its behalf.

### Netboot OCI images

Bare-metal provisioning uses two OCI images, both built `FROM scratch` and both
using `/disk/` as the artifact root, following the kubevirt containerDisk
convention. The machine image (`spec.host.netboot.image`, or the deprecated
`spec.pxe.image`) contains `/disk/disk.img.gz`, a gzip-compressed raw disk image
that the installer writes to the target disk. The reusable netboot image
(`spec.host.netboot.netbootImage`) contains bootloaders, a kernel, an initrd,
templates, and metadata. When no netboot image is set, metalman uses the
release-matched default configured by `--default-netboot-image`.

Files whose names end in `.tmpl` are Go templates rendered per machine at serve
time; other files are served verbatim. A `metadata.yaml` file in the netboot
image provides image-level settings such as `dhcpBootImageName` and
`httpBootPath`. The boot platform is selected by
`spec.host.netboot.architecture` (or the deprecated `spec.pxe.architecture`)
and defaults to `amd64`. This split lets you rebuild a host OS image without
rebuilding the boot environment, and lets one netboot image serve many machine
images.

### Resource relationships

The resources form a small graph rather than a hierarchy. A `Machine` references
Secrets for SSH keys, BMC passwords, and bootstrap tokens. It references OCI
images by name. A `MachineAllocation` owns its `Machine` and references a
`MachineClass` by UID, which in turn references a `Site` and a configuration
version. A `Machine` and a Kubernetes `Node` are linked by name or by
`spec.kubernetes.nodeRef`. The networking resources reference `Site` objects by
name rather than by owner reference, because a peering spans more than one site.

![Relationships between Site, Machine, MachineConfiguration, MachineOperation, and network resources](../../img/architecture-resource-relationships.svg)

Ownership is deliberate. `MachineAllocation` and its Machine share a lifecycle
so a release operation can find and remove the host it created. `Site` owns its
per-site metalman Deployment through a controller owner reference, so deleting
the Site garbage-collects the controller and the operator does not need a
separate delete API. Networking resources such as `SiteNodeSlice` and
`GatewayPoolNode` are controller-owned: you read them, you do not create them.

## Provisioning adapters

A provisioning adapter owns the part that varies by environment: how the
controller reaches the host and how the agent gets onto it. Everything after
that is shared. Each adapter eventually produces a running `unbounded-agent`
with a JSON configuration containing the cluster endpoint, CA, bootstrap
credential, node name, labels, and rootfs source. The agent then performs the
same host preparation and TLS bootstrap regardless of which adapter delivered
it.

There are four delivery paths. Three are controller-driven and one is manual.

| Adapter | Host state | Who drives it | Credential source |
|---|---|---|---|
| SSH (`machina`) | Existing Linux with SSH | machina Deployment | `spec.kubernetes.bootstrapTokenRef` Secret |
| PXE (`metalman`) | No operating system | Per-site metalman Deployment | TPM-attested, short-lived ServiceAccount token |
| Cloud capacity | Provider API creates the host | machine-ops controller plus a provider adapter | Allocation bootstrap token |
| Manual bootstrap | Host you already manage | `kubectl unbounded machine manual-bootstrap` | Site bootstrap token embedded in the payload |

### SSH provisioning with machina

machina is the adapter for hosts that already run an operating system and are
reachable over SSH. It is a controller-runtime manager with a Machine
reconciler, a `MachineConfiguration` controller, a configuration binding
controller, and a daemon CSR approver. It watches `Machine` objects and maps
`Node` events back to the Machine with the same name or the configured
`nodeRef`. The operator runs one machina replica by default, and machina's own
configuration disables leader election by default.

The first step is reachability, and it is a TCP probe rather than a full SSH
login. The probe uses a short timeout and, when `spec.ssh.bastion` is set,
establishes an SSH connection to the bastion and dials the target through it.
A failed probe is not a terminal error. The Machine stays in `Pending`, the
`SSHReachable` condition becomes false with the probe error, and the controller
retries after 30 seconds.

When the probe succeeds, machina reads the SSH private key from the Secret named
by `spec.ssh.privateKeyRef`. The default username is `azureuser`, the SSH
connect timeout is 30 seconds, and a bastion may use its own key or inherit the
machine's key. The controller then reads the bootstrap token from the Secret
named by `spec.kubernetes.bootstrapTokenRef` in `kube-system`. If the reference
is missing or the Secret lacks `token-id` or `token-secret`, the Machine moves
to `Failed` instead of guessing a credential.

Provisioning itself is deliberately simple. machina copies the embedded install
script to `/tmp/machina-agent-install.sh`, uploads the generated agent JSON to
`/tmp/unbounded-agent.json`, and runs the script with `sudo -E` and
`UNBOUNDED_AGENT_CONFIG_FILE` pointing at the uploaded file. Both temporary
files are removed when the operation finishes, whether it succeeded or failed.
The install script downloads the agent release for the host architecture,
optionally verifies a SHA-256 checksum, runs `unbounded-agent preflight`, and
then runs `unbounded-agent start`.

A provisioning attempt is bounded. The controller records the start time in the
`Provisioning` condition and treats an attempt older than the configured
provisioning timeout (five minutes by default) as failed, which prevents a
stale attempt after a controller restart from blocking the Machine forever.
After a successful script run, machina sets `Provisioned=True` with the observed
generation, publishes the applied agent image in status, and moves the Machine
to `Joining`.

The join is observed, not assumed. In `Joining`, machina looks for the Node
named by `spec.kubernetes.nodeRef` or by the Machine name. If the Node does not
exist yet, it requeues after 30 seconds. When the Node appears, the controller
records `nodeRef` if it was not set and moves the Machine to `Ready`. In
`Ready`, the controller requeues every five minutes and keeps watching the
Node. If the Node disappears, the Machine returns to `Joining`, so a node that
was deleted for a repave does not continue to look healthy.

machina also runs the daemon CSR approver. The agent's long-running daemon needs
its own client credential to keep watching the Machine after the bootstrap token
is gone. The approver accepts a bootstrap token when the token is scoped to the
Machine's Site or is the default token, and it requires an explicit
Machine-to-Node binding before it approves a renewal. That binding is what keeps
a long-lived daemon credential attached to the same node.

![SSH provisioning flow from Machine registration through agent install and node join](../../img/architecture-ssh-provisioning.svg)

### PXE provisioning with metalman

metalman handles hosts that have no operating system. It is one binary with the
boot servers built in, deployed once per `Site` by the operator as
`metalman-controller-<site>`. The Deployment uses host networking, a
`CriticalAddonsOnly` toleration, and a node selector on
`unbounded-cloud.io/site=<site>`. It serves DHCP on 67/udp, TFTP on 69/udp, HTTP
on 8880/tcp, and health probes on 8081/tcp.

The DHCP server derives static leases from the `dhcpLeases` entries on
`Machine` objects. It has three modes: interface mode binds a named interface
and listens for broadcast traffic, auto-interface mode discovers the interface
from the server bind address, and relay mode listens for unicast packets from a
DHCP relay. Leader election is always enabled, and each site gets its own
`metalman-<site>` lease, so several sites can coexist on one cluster with
independent failover.

A PXE repave is a `HostReplace` operation. The operator or CLI creates the
operation; metalman configures the Redfish boot override for the selected
protocol, optionally writes DHCPv4 settings for UEFI HTTP boot, and issues a
Redfish force restart. The host boots the netboot image, the installer writes
the machine image to the target disk, and the installer calls `/pxe/disable`
when it is done. On first boot, cloud-init installs and starts the agent. The
agent then obtains a bootstrap token by attesting to metalman with its TPM
endorsement key. metalman matches the request's TCP peer address to a Machine
NIC and returns a token encrypted to that key. It records the credential ID and
issue time on the Machine before handing the token out, and the CSR approver
accepts kubelet requests only when the credential ID and node name match that
Machine.

Two consequences follow from that design. The source address is the direct TCP
peer address; `X-Forwarded-For`, `X-Real-IP`, and `Forwarded` are ignored, so
metalman is not supported behind an HTTP proxy that rewrites the client address.
TPM attestation also needs ServiceAccount tokens that carry a `jti` claim,
which requires Kubernetes 1.30 or later. The token expires after one hour, so a
host must complete the join while the credential is valid.

On later reboots the boot order returns to the local disk. Metalman clears the
Redfish boot override after the installer signals completion, so the installed
system boots normally and only an explicit repave sends the host back to the
network.

### Cloud capacity and provider adapters

The cloud path removes SSH from the provisioning loop. Upstream Karpenter,
compiled with the Unbounded provider, watches for unschedulable pods and decides
that the cluster needs more capacity. It reads `MachineClass` offerings as
instance types and creates one `MachineAllocation` per NodeClaim. The
`unbounded-karpenter` binary is a separate deployment with its own manifests; it
is not a per-site component and it contains no cloud SDK, credentials, or
provider-specific code.

The machine-ops controller consumes allocations. For each allocation it freezes
the class inputs into a snapshot, resolves the provider adapter named by
`MachineClass.spec.provider`, creates a `Machine` with an `allocationRef`, and
starts a `HostAllocate` operation through that adapter. The adapter launches the
host against its provider API and passes cloud-init user data that installs and
starts the agent with the allocation's bootstrap token. When the Node registers,
the allocation records the node reference and reports capacity back to
Karpenter. A `HostRelease` operation reverses the process when the consumer sets
`spec.lifecycle: Released`.

The provider contract lives in `pkg/machineops`. A provider registers an
immutable set of operation strategies. An operation is either immediate or
long-running; a long-running operation persists a provider handle and is polled
one step per reconcile. Capacity is all or nothing: a provider that registers
capacity must also register `HostAllocate` and `HostRelease` as long-running
operations, and those operation kinds require capacity registration. The
in-tree Azure VM adapter implements the host operations (`HostPowerOff`,
`HostPowerOn`, `HostHardReboot`, `HostReplace`); a capacity provider supplies
the launch and release callbacks at the same boundary.

The machine-ops controller is deployed from `deploy/machine-ops`, separately
from the operator. Its `--site` and `--provider` flags must be set together and
scope one controller instance to one provider in one site. It reads provider
credentials from Secrets in the system namespace, uses its own leader-election
lease per provider and site, and accepts `--api-server-endpoint` so replacement
hosts get the correct bootstrap endpoint. A provider adapter that supports
`HostReplace` can inject new user data; replacing a Machine that declares the
experimental container isolation mode additionally needs
`--experimental-container-isolation`.

### Manual delivery

The manual path is for hosts you already manage, or for environments where you
run your own provisioning tool. `kubectl unbounded machine manual-bootstrap`
emits a payload rather than creating a controller. It supports three variants:
`script` (the default self-contained bash script), `cloud-init` user data, and
`ignition` for image-based hosts. `--site` selects that site's bootstrap token
and adds `unbounded-cloud.io/site` as a node label unless you override it, so
the node is matched to the right site when it joins.

This path is useful for VM scale sets, immutable images, and repair of a host
whose agent was removed. It ends in the same place as the controller paths: the
payload installs the agent, the agent joins the cluster, and unbounded-net picks
up the node.

## The agent and the node stack

The agent is a single binary that runs as a host process. It is the only
Unbounded component that must exist on every worker host, and it remains present
after the node is running. It reads a JSON configuration file named by
`UNBOUNDED_AGENT_CONFIG_FILE`, normally
`/etc/unbounded/agent/config.json`, and it supports four modes of operation:
`start` for initial bootstrap, `daemon` for long-running lifecycle management,
`preflight` for non-mutating host checks, and `reset` for removing the agent and
its resources. It also provides `exec` and `logs` for inspecting the node stack.

The agent always runs on the host; the isolation mode decides only where the
node stack runs. In `nspawn`, the default, the agent prepares the host and
starts a systemd-nspawn machine whose root filesystem comes from an OCI image.
containerd, kubelet, the CNI plugins, and CoreDNS run inside that machine, which
keeps the node stack separate from the host's own packages and services. The
agent alternates between two machine names, `kube1` and `kube2`, so an upgrade
can stage the next root filesystem before tearing down the previous one. In
`none`, the same stack runs directly on the host as agent-owned systemd units.
In `container`, the stack runs in a privileged OCI container; it is experimental
and requires an explicit opt-in on the provisioning controller and on the host.

`unbounded-agent start` runs three phases in order. The host phase installs
host packages, sets kernel parameters, prepares the container runtime
prerequisites, and handles swap and storage. The rootfs phase detects the CPU
architecture and available GPUs, resolves the rootfs OCI image or offline
artifact source, and downloads the architecture-specific binaries: containerd,
runc, the CNI plugins, kubelet, kube-proxy, and crictl. The node-start phase
writes kubelet configuration, starts containerd and kubelet, and performs TLS
bootstrap with the configured token. The resulting Node registers with the
labels and taints from the Machine.

The agent records install state under `/var/lib/unbounded/agent/`, including
`install-state.json`, and takes a shared installation lock. A retry can reuse
completed downloads, a prepared rootfs, and a running machine instead of
reinstalling everything. The phase code inspects the host before each stage and
skips work that is already correct. This is what makes a repeated `start` safe
after a transient network failure or a controller restart.

After bootstrap, `unbounded-agent daemon` runs as a systemd unit. It watches the
local Machine, the Node, and MachineOperations that target the Machine. It
handles `NodeReboot`, `HostSoftReboot`, `AgentUpgrade`, and `AgentReset`. It also
owns repave: when the Kubernetes Node is deleted but the Machine remains, the
daemon resolves the desired configuration, compares it with the applied config,
and rebuilds the node stack. That is the normal way to apply a new
`MachineConfigurationVersion`, an agent setting, or a declared isolation mode.
The daemon reports progress in Machine status and conditions such as
`AgentBootstrapped`, `NodeUpdated`, `IsolationDrift`, and `RepavePending`.

The node stack is therefore not a static install. It is a goal state reconciled
by a host-local controller. The controllers in the management cluster decide
what the Machine should be; the agent decides how to make the local host match
it without breaking a running node.

## Network architecture

Unbounded networking has two cooperating layers, and it is a mistake to
collapse them into one. The first layer is a conventional per-node CNI that
gives pods an address and connects them to a bridge on the node. The second
layer is a tunnel data plane that carries traffic between nodes and between
sites. The CNI does not implement encryption, and the tunnel does not allocate
pod addresses. They meet in the kernel routing table: routes for remote pod
CIDRs point at the tunnel data plane, while local pods stay on the bridge.

That split is what lets a site keep an existing CNI. It is also why the
component is not a "WireGuard CNI". WireGuard is one protocol the tunnel layer
can choose. GENEVE, VXLAN, IPIP, and no encapsulation are alternatives, and the
choice is made per link scope rather than per cluster.

![Network data plane showing the CNI bridge and the separate eBPF tunnel path](../../img/architecture-network.svg)

### The control-plane side

`unbounded-net-controller` is a Deployment that watches Nodes and the
networking custom resources. It has four jobs. It matches a node's internal IP
against each `Site`'s `nodeCidrs`, labels the node with the resolved site, and
creates or repairs the Site's `SiteNodeSlice` objects. It allocates pod CIDRs
for nodes from the matching `PodCidrAssignment` and patches
`Node.spec.podCIDRs`. It maintains `GatewayPool` status, including which nodes
are eligible gateways, their external addresses, their WireGuard public keys,
their assigned WireGuard ports, and the sites reachable through the pool. It
also aggregates peering and publishes the route and health view that node
agents consume.

Slices are a scale mechanism, not a user-facing API. Each `SiteNodeSlice` holds
at most 500 nodes, and the controller splits a large site across several
slices. A node agent reads slices instead of listing every node in a large
cluster, which keeps its watch and memory cost bounded. `GatewayPoolNode`
objects are controller-owned in the same way.

The controller also serves operational views. It exposes node status and
topology detail to the dashboard and can register an aggregated API server for
status, depending on the runtime configuration. These interfaces are read-only
views over the same state the controller reconciles; they do not participate in
packet forwarding.

### The node agent and the CNI

`unbounded-net-node` runs as a DaemonSet on every node, including control-plane
nodes. It waits for `Node.spec.podCIDRs` to be assigned, then writes a CNI
configuration file at `/etc/cni/net.d/10-unbounded.conflist`. The conflist is
named `unbounded-net` and chains three plugins: the standard `bridge` plugin on
`cbr0` with gateway and default-gateway behavior, the standard `portmap` plugin
for host port mappings, and the `loopback` plugin. IP allocation is delegated to
`unbounded-ipam`, a small CNI IPAM binary that the node agent installs and
serves over a Unix socket. The bridge MTU follows the tunnel MTU so a pod packet
does not exceed the path once it is encapsulated.

This is the entire CNI responsibility. It creates the pod veth pair, attaches
it to the bridge, allocates an address from the node's pod CIDR, programs the
default route inside the pod network namespace, and handles port mappings. It
does not create tunnels and it does not encrypt anything.

The node agent also generates the node's WireGuard key pair, stores it under
`/etc/wireguard`, and annotates the node with the public key. It creates the
tunnel interfaces, programs routes, installs masquerade rules, and reports
health. On startup it waits for pod CIDRs and then reconciles the whole local
data plane from the Site, peering, gateway, and slice state it reads from the
API.

### The tunnel data plane

The tunnel layer is built around a dummy interface named `unbounded0`. The
interface is deliberately `NOARP`: it has no peer discovery of its own. Routes
for remote pod CIDRs and for gateway supernets point at `unbounded0`, which
attracts overlay traffic into the eBPF program attached to its egress hook.

That program is `unbounded_encap`, a TC egress classifier compiled from
`bpf/unbounded_encap.c`. It performs a longest-prefix-match lookup in a single
LPM trie named `unb_endpts`. The trie key is a 16-byte address, with IPv4
destinations stored in IPv4-mapped IPv6 form so one trie serves both families.
The value is a set of up to four nexthops. Each nexthop names the peer's underlay
address, a VNI, the tunnel interface index to redirect to, a health flag, and a
protocol code. The program redirects the packet to the chosen tunnel interface;
the tunnel device performs the encapsulation. WireGuard encryption, when
selected, happens in the WireGuard device, not in the BPF program.

The tunnel devices themselves come in two shapes. WireGuard uses per-port
interfaces named `wg<port>`, which lets gateway links use separate interfaces
and equal-cost multipath. GENEVE, VXLAN, and IPIP use shared flow-based
interfaces named `geneve0`, `vxlan0`, and `ipip0`, with the kernel flow
dissector choosing the remote endpoint per flow. The defaults are GENEVE on UDP
6081 with VNI 1, VXLAN on UDP 4789, IPIP, and WireGuard on UDP 51820.

Protocol selection is scoped. A `Site`, a `SitePeering`, a `GatewayPool`, a
`SiteGatewayPoolAssignment`, or a `GatewayPoolPeering` can set
`tunnelProtocol` and `tunnelMTU` for the links it governs. `Auto` resolves to
WireGuard for links that use external addresses and GENEVE for links that use
only internal addresses. An explicit value overrides that default for the whole
scope, including a deliberate `None` when the underlay already provides direct
L3 reachability. Health checks can withdraw an unhealthy nexthop from selection
without deleting the route.

### Topology and address planning

A `Site` is the unit of address planning. `spec.nodeCidrs` defines which nodes
belong to it. `spec.podCidrAssignments` defines the pools from which node pod
CIDRs are carved, with per-node block sizes, node name regex filters, and
priorities. The assignment is authoritative: if the pool is exhausted, the
controller does not invent space. Sites must have non-overlapping pod ranges for
routing to work, and the management cluster's own ranges are described by the
reserved `cluster` Site so that remote sites know which CIDRs are local to the
control plane.

Peering has two forms. A `SitePeering` lists two or more sites that should
exchange traffic directly and controls whether their nodes mesh. A
`GatewayPool` selects gateway nodes by label, requires external addresses and
WireGuard keys, and can route declared CIDRs. `SiteGatewayPoolAssignment` binds
sites to pools, and `GatewayPoolPeering` links two pools. Gateway pools are the
right tool when a site has many nodes and a full mesh would be too expensive, or
when a site is reachable only through a small number of routers.

When `spec.manageCniPlugin` is true, the node agent writes CNI configuration and
treats same-site nodes as tunnel peers. When it is false, the agent does not
write CNI configuration and does not mesh same-site nodes; an external CNI such
as Cilium or Calico owns intra-site networking. The Site must still declare
`podCidrAssignments`, because those ranges tell the tunnel layer which remote
addresses to route. Inter-site links to gateway pools are still created. This is
the supported way to adopt Unbounded networking without replacing the CNI in a
single-site cluster.

## Racer

Racer is the optional distributed read-through cache for large, read-mostly
objects. It is not part of the node bootstrap path and it is not required to run
a cluster. It exists because pulling the same model, dataset, checkpoint, or
container layer from an origin to every node turns origin bandwidth into the
ceiling on a rollout. Racer keeps immutable object pages on the worker nodes and
lets a node serve a page it already has to another node that needs it.

The API surface is `ClusterCache` in `racer.unbounded-cloud.io/v1alpha1`. It is
cluster-scoped, has no status field, and uses the Kubernetes object UID as the
wire cache identity. Its `spec.origin` optionally declares an Azure Blob
Storage source, `spec.priority` sets its relative share of capacity,
`spec.passthrough` disables caching and proxies reads to the origin, and
`spec.nodeSelector` constrains client pods to nodes that host the cache. A
`ClusterCache` is not a PersistentVolume or a PersistentVolumeClaim, and it
makes no durability promise: the origin remains the source of truth.

The operator installs Racer on demand. When no `ClusterCache` exists, an
existing installation is retained and its admission guards are enforced, but
nothing new is installed. When at least one cache exists, the operator creates
the static objects: the `racer` DaemonSet, its ServiceAccount and RBAC, the
admission policies, the `racer-object-sidecar-injector` webhook configuration,
and an empty `racer-state` Secret. The operator deliberately holds no keys, no
certificates, and no authority state.

Every Racer data plane Pod runs the same binary and can take a control role.
Pods claim control seats through Leases named `racer-seat-0` through
`racer-seat-15`, and one seat holder also holds the `racer-primary` Lease. The
primary computes membership and the catalog, reconciles keys and trust roots,
and commits state to the `racer-state` Secret before publishing it. Other seats
serve the committed state. Clients discover seats through the Kubernetes API,
spread connections across them, and retry another seat when a connection fails.
The Secret's UID is the cluster identity, and a term-based fencing protocol
prevents an old primary from overwriting its successor's state after a lease
change.

Applications read through a Unix domain socket. A Pod that carries the
`racer.unbounded-cloud.io/client-cache: <name>` annotation is matched by the
sidecar injector, which mounts the client socket. The `racer-object` sidecar is
a separate, optional adapter that turns the Racer socket into a read-only
S3-compatible or Azure Blob API. It is a cache-aware proxy, not a storage layer,
and it does not support writes, deletes, upstream credentials, or fallbacks.
For the full cache model, see
[Racer Distributed Cache]({{< relref "concepts/racer" >}}) and the
[Racer reference]({{< relref "reference/racer" >}}).

Racer is a separate component from unbounded-net. It uses the cluster network
for peer-to-peer page transfer, so sites with constrained links benefit from the
same tunnels the pods use, but the cache and the tunnel data plane remain
independent components with separate failure modes.

## The lifecycle of a Machine

The lifecycle is easiest to follow on the SSH path, because machina owns every
step and reports it in one status. The other adapters replace the delivery step
and then converge on the same join and readiness behavior.

1. You initialize the site. `kubectl unbounded site init` creates the reserved
   `cluster` Site and the remote Site, a default `GatewayPool`, a
   `SiteGatewayPoolAssignment`, and a bootstrap token in `kube-system` labeled
   with the site name.
2. You register the machine. `kubectl unbounded machine register` writes the SSH
   Secret when you pass a key file and otherwise references the existing Secret,
   resolves the site's bootstrap token, reads the management cluster's Kubernetes
   version, and applies a cluster-scoped `Machine` named `<site>-<name>` (or
   `<site>-<host>` when you omit `--name`). The Machine carries the site label,
   the SSH reference, the bootstrap token reference, the node labels you passed,
   and any agent download overrides.
3. The binding controller resolves configuration. If the Machine has an
   explicit `spec.configurationRef`, that version is used. Otherwise a
   `MachineConfiguration` whose selector matches is chosen by priority and name.
   With no match, the Machine reports `ConfigurationPending`; this condition is
   informative and does not by itself stop the SSH bootstrap.
4. machina probes the host. The TCP probe succeeds directly or through the
   bastion. On failure the Machine stays `Pending`, `SSHReachable` is false, and
   the controller retries.
5. machina prepares the credentials. It reads the SSH private key from the
   Secret and the bootstrap token from `kube-system`. A missing key, bad token
   reference, or unsupported isolation mode moves the Machine to `Failed` with
   the reason in status.
6. machina provisions. It sets `Provisioning=True` with a start timestamp,
   copies the install script and agent configuration to the host, runs the
   script with `sudo`, and waits for it to finish. A timeout or script failure
   moves the Machine to `Failed`; the controller retries from that phase, so you
   can repair the host or the spec and let it try again.
7. The agent bootstraps the host. It runs the host, rootfs, and node-start
   phases, writes kubelet configuration, starts containerd and kubelet, and
   presents the bootstrap token to the API server. The control plane issues a
   client certificate and the kubelet registers the Node with the requested
   labels and taints.
8. machina observes the join. It moves the Machine to `Joining` and looks for
   the Node by `spec.kubernetes.nodeRef` or by Machine name. When the Node
   appears, it records `nodeRef` and sets the phase to `Ready`.
9. The node enters the network. unbounded-net matches the Node's internal
   address to a site, labels it, allocates pod CIDRs, and creates or updates the
   site's slices. `unbounded-net-node` sees the assigned CIDRs, writes the CNI
   configuration, and programs the bridge, routes, tunnels, and BPF maps.
10. The Machine stays under reconciliation. machina requeues a Ready Machine
    every five minutes and watches the Node. If the Node disappears, the phase
    returns to `Joining`. The agent daemon sees the missing Node and repaves the
    host from the desired configuration, and the cycle ends in `Ready` again.

The phase values are human-readable summaries; the conditions carry the detail.

| Phase | Set by | Meaning | Typical next step |
|---|---|---|---|
| `Pending` | machina | The host is not reachable yet, or provisioning has not started | Fix the network, key, or bastion and wait for the next probe |
| `Provisioning` | machina | The install script is running on the host | Wait, or inspect the `Provisioning` condition and host logs |
| `Rebooting` | Reserved | Defined for host reboot transitions; current controllers do not set it | Read the `MachineOperation` instead |
| `Joining` | machina | The agent ran and the controller is waiting for the Node | Inspect kubelet and the bootstrap token if the Node does not appear |
| `Ready` | machina | The Node exists and is bound to the Machine | Use the node; day-2 actions are available |
| `Failed` | machina | Provisioning failed and the controller will retry | Read the condition message, repair, and let the controller retry |

The same lifecycle has different owners on the other paths. A PXE Machine is
created directly with `spec.host.netboot`, and metalman reports boot progress
through a `MachineOperation` rather than through machina phases. A cloud Machine
is created by the machine-ops controller from a `MachineAllocation`, and the
allocation reports the node reference. A manually bootstrapped host creates its
own Node and carries the site label from the payload. In every case, the
authoritative readiness signal for the worker is the Kubernetes `Node`, and the
authoritative readiness signal for the location is the `Site` component
conditions.

## Security model

Unbounded assumes the management cluster is trusted and the network between the
cluster and a worker may not be. The security model is built around short-lived
credentials, explicit bindings between a credential and a Machine, and
minimizing what each controller can read.

SSH credentials are stored in Kubernetes Secrets, not in the `Machine` spec. The
CLI writes the key to a Secret named `ssh-<site>` by default, and the `Machine`
references it by name, namespace, and key. machina reads the Secret through the
API and never writes the key back to status or logs. The private key is the
authentication boundary, so protect the Secret and the namespace that holds it.

One caveat belongs here because it affects deployment choices. The SSH client in
this worktree accepts any host key when it connects
(`ssh.InsecureIgnoreHostKey`), so it authenticates the client to the host but
does not verify the host's identity. The `Machine` status has a field for an
observed SSH fingerprint, but do not treat it as an enforced pin. Run SSH
provisioning over a trusted management network or through a bastion whose
address you control.

Bootstrap tokens are scoped and observable. `site init` creates a bootstrap
token Secret in `kube-system` labeled with the site name. `machine register`
resolves the newest valid token for the site and stores only its name in the
Machine. The token refresher keeps a valid token available for sites that need
one. The daemon CSR approver accepts a bootstrap token for a daemon credential
when the token is valid for the Machine's site or is the default token.
Bootstrap tokens are site-scoped credentials rather than single-Machine leases,
so any node in the site may use one during its validity window. Renewal is
stricter: the approver requires an explicit Machine-to-Node binding through
`spec.kubernetes.nodeRef`, and clearing that reference severs renewal.

The PXE path adds hardware-rooted proof. metalman matches the attestation
request's source address to a Machine NIC, encrypts a one-hour ServiceAccount
token to the host's TPM endorsement key, and records the token's credential ID
on the Machine before returning it. The CSR approver then requires the
credential ID and node name to match that Machine. The endorsement key is pinned
on first use, and a new attestation replaces the previous credential. This
requires Kubernetes 1.30 or later, because the token must carry a `jti` claim.

Redfish credentials follow the same trust-on-first-use pattern. metalman
captures the BMC's TLS certificate fingerprint on first connection and pins it
in `status.redfish.certFingerprint` for later requests. After that, a certificate
whose fingerprint does not match is rejected. The pin does not update on its own,
so plan a BMC certificate rotation deliberately; an unexpected mismatch is also
what an interception attempt looks like.

Each component runs with its own ServiceAccount and RBAC. The operator creates
the namespace, identities, roles, and bindings for the workloads it manages, and
the generated manifests carry only the permissions the component needs. Secrets
are read through the Kubernetes API; they are not copied into ConfigMaps,
annotations, or Machine specs. Workload overrides are the deliberate exception
to "the operator owns the workloads": write access to
`unbounded-component-overrides` is equivalent to cluster-admin, because a
patched DaemonSet can run arbitrary code on every node. Treat that ConfigMap as
a privileged object.

Experimental features are off by default. The `container` isolation mode
requires `--experimental-container-isolation` on machina and on the machine-ops
controller, and the agent accepts it only when its own configuration opts in.
This keeps an accidental mode change from reprovisioning a host into a path that
is not yet supported for production.

## Deployment

Deployment is intentionally plain for the operator-managed path. The operator
and its components are published as numbered YAML manifests, and templates are
rendered into `deploy/*/rendered/` directories by the repository's render
tooling. The operator embeds the component manifests it needs at build time.
`kubectl unbounded install` applies the embedded operator manifests with
server-side apply under the field manager `kubectl-unbounded`, waits for the
operator rollout and CRDs, and leaves the rest to the operator.

Gantry is the one component with a second installation path. Operator-managed
clusters use the manifests embedded in the operator, and the operator never runs
Helm. A cluster without the operator can install the released gantry OCI chart
instead. The two paths are mutually exclusive, and each rejects workloads owned
by the other.

The CRDs themselves are generated by `controller-gen` from the Go API types.
They live under `deploy/machina/crd/` for the host and lifecycle group and
`deploy/net/crd/` for the networking group; Racer's generated CRD is embedded
from `api/racer/v1alpha1/crd/`. Do not edit generated CRDs by hand; change the
API type and regenerate. The operator installs and upgrades them at startup, so
a cluster that already has an older operator can be moved forward by applying a
new operator image rather than by running a separate CRD apply step.

The workloads follow the same conventions. Components install into the
operator's namespace at the operator's version. Machina and unbounded-net are
required singletons. Metalman is a per-site Deployment with host networking
because DHCP and TFTP need the provisioning network. The networking node agent
and the Racer data plane are DaemonSets. The operator adds a
`CriticalAddonsOnly` toleration where a component must run on a tainted
control-plane or system node, and the generated pods carry resource requests and
limits appropriate to their role. For example, a metalman Deployment requests
100m CPU and 128Mi memory with limits of 500m CPU and 256Mi memory.

Images are built with multi-stage Containerfiles. The agent and host images have
Azure Linux 3.0 and Ubuntu variants, and the NVIDIA variants add the GPU runtime
pieces. The operator and most controller binaries are Go, while the Racer data
plane and the `racer-object` sidecar are Rust. Because the component image is
selected by the operator rather than by the `Site`, a release upgrade is an
operator image change, followed by the operator rolling the components it owns.

Customization should go through `unbounded-component-overrides` rather than
direct edits to generated Deployments. Direct edits are drift: the operator
reconciles the workload back to its desired shape, and may reclaim fields it
owns. The overrides ConfigMap is read on every pass, validated, hashed, and
reported in `Site` status, so an invalid override fails visibly instead of
leaving a partially patched workload.

For the commands that perform these steps, see the
[CLI reference]({{< relref "reference/cli" >}}) and the
[Getting Started]({{< relref "guides/getting-started" >}}) guide.

## See also

- [Project Overview]({{< relref "concepts/overview" >}}) for the problem and the
  high-level component model.
- [Networking Concepts]({{< relref "concepts/networking" >}}) for site and
  address planning without the implementation detail.
- [Networking Architecture]({{< relref "reference/networking/architecture" >}})
  for the controller, node agent, and data plane in depth.
- [Networking Custom Resources]({{< relref "reference/networking/custom-resources" >}})
  for the full field reference.
- [Machine CRD Reference]({{< relref "reference/machina-crd" >}}) for every
  `Machine` field and condition.
- [Agent Configuration]({{< relref "reference/agent/configuration" >}}) for the
  JSON file the agent reads.
- [Agent Isolation]({{< relref "reference/agent/isolation" >}}) for the
  `nspawn`, `none`, and `container` modes.
- [Racer Distributed Cache]({{< relref "concepts/racer" >}}) and the
  [Racer reference]({{< relref "reference/racer" >}}) for the cache layer.
- [Bare Metal Concepts]({{< relref "concepts/bare-metal" >}}) and the
  [PXE guide]({{< relref "guides/pxe" >}}) for the metalman path.
- [Cloud API Provisioning]({{< relref "guides/cloud-api" >}}) for the
  `MachineClass` and `MachineAllocation` path.
- [Workload Overrides]({{< relref "reference/workload-overrides" >}}) for the
  supported customization surface.
