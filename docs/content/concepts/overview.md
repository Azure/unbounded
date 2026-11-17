---
title: "Project Overview"
weight: 1
description: "How Unbounded Kubernetes extends a conformant control plane to run worker nodes anywhere and connect them back over encrypted tunnels."
---

Unbounded Kubernetes lets you run Kubernetes worker nodes wherever the compute
is, while keeping a single control plane where it already runs. The control
plane can be AKS, EKS, GKE, a self-managed cluster, or any other conformant
Kubernetes distribution. The nodes can be virtual machines in another cloud, GPU
hosts at a specialized provider, servers in a rack behind NAT, or devices at an
edge site. Unbounded handles the parts that normally assume everything lives on
one flat network: provisioning the host, joining it to the cluster, and routing
pod traffic between locations.

This page explains the problem Unbounded solves, the components that implement
it, and the custom resources you work with. It also walks through what happens
when you add a node by SSH, by a cloud provider API, or by PXE. Read it before
the guides if you want the mental model first; each guide then fills in the
commands and flags for one path.

![Unbounded Kubernetes overview showing a control plane and worker nodes in several locations connected through encrypted tunnels](../../img/unbounded-overview.svg)

The central idea is that a Kubernetes cluster does not have to be a single
network. Unbounded separates three concerns that Kubernetes normally leaves
implicit:

- **Where the control plane runs.** This stays where it is. Unbounded installs
  CRDs and controllers into it, but does not replace it.
- **Where worker nodes run.** A node is a Linux host with kubelet and a
  container runtime. Unbounded provisions that host and joins it using a
  bootstrap token, regardless of where the host sits.
- **How traffic moves between nodes.** A `Site` groups nodes by location and
  address range, and unbounded-net builds tunnels between sites and, where
  appropriate, between individual nodes.

Once you separate those concerns, adding a node becomes a provisioning problem
rather than a networking problem. You describe the host as a `Machine`, choose a
delivery path for the agent, and let the controllers handle reachability, the
join, and the routes.

## The Problem

Kubernetes was designed around a cluster network that is mostly trustworthy and
mostly flat. A typical cluster assumes that every node can reach the API server,
that the API server can reach every kubelet, that nodes can reach each other
directly, and that pod and node CIDRs do not overlap. Those assumptions hold in a
single VPC or a single LAN. They break as soon as compute moves somewhere else.

### Flat network assumptions

The moment a node sits behind NAT, the API server cannot dial it, and the node
cannot be reached for logs, exec, or a kubelet connection. A node in another VPC
may not be routable at all without a peering, a VPN, or an overlay. A node on a
provider network may only expose a public address, while the cluster expects
private addresses. Each of these cases needs a different answer, and a cluster
that only understands one of them cannot accommodate the others.

### Additional cloud regions and providers

Using a second region of the same cloud usually means a second set of network
constructs. Using a different provider means a different API, different identity
and credentials, different instance metadata, and different networking. If your
provisioning tooling is built around one provider's virtual machine scale sets
or instance groups, adding a second provider requires a second tool.

### GPU compute from specialized providers

GPU capacity is often available from providers that are not your primary cloud,
and often at a different cost and availability profile. These providers expose
their own APIs and their own images. You want the pods to use that capacity
without moving the control plane or rebuilding the cluster around the provider.

### On-premises hardware behind NAT

On-premises servers are frequently behind a firewall with no inbound access.
The control plane cannot reach them, and they cannot expose a stable public
address. Provisioning them over SSH from the cluster may work only if a bastion
is available, and even then the node needs a secure path back into the cluster
network once it joins.

### Edge sites with limited connectivity

An edge site may have intermittent connectivity, a small pipe, or a NAT device
that rewrites addresses. The node still has to join the cluster and run pods,
but it cannot rely on a persistent, high-bandwidth connection to a central
network.

### Node provisioning outside the cluster network

Provisioning a host outside the cluster network raises questions that a
same-network cluster never asks. How does the agent get onto the host? How does
the host learn the API server endpoint and the cluster CA? How does it obtain a
bootstrap token, and how do you keep that token from leaking? How do you know
the host that received the token is the host you intended to provision?

### Cross-site pod traffic routing

Even after nodes join, pods in one location need to reach pods in another. That
requires each location to have a non-overlapping pod CIDR, a way to route
between CIDRs, and a data plane that can carry the traffic. Without a
coordinated address plan, two sites can allocate the same pod IP and break
routing for both.

### Bare-metal PXE boot gaps

Bare metal adds a bootstrapping problem before any of the above. The machine has
no operating system, so it cannot run an SSH agent. It needs DHCP, a bootloader,
a kernel, an initramfs, and a first-boot configuration that installs the agent.
Redfish-based power control and hardware-rooted attestation are often needed to
make that path unattended and safe. Many Kubernetes provisioning tools do not
cover it.

## The Solution

Unbounded treats the control plane as fixed and everything else as pluggable.
It installs a small set of controllers and CRDs into an existing cluster, then
uses those controllers to provision hosts and wire them into the cluster
network. You do not replace your control plane, and you do not need a special
distribution.

The first property is that worker nodes can run anywhere. A node is described
by a cluster-scoped `Machine` resource. The `Machine` records how to reach the
host, how to install the agent, which Kubernetes version to run, and which
labels and taints the node should register with. The same resource works for an
SSH-reachable server, a cloud instance, and a bare-metal host, because the
provisioning details are fields on the resource rather than assumptions in the
controller.

The second property is an encrypted path back to the cluster. Depending on the
path, the agent either receives a bootstrap token directly or attests to a
metalman instance before receiving one. It then performs normal Kubernetes TLS
bootstrap: kubelet presents the token, the control plane issues a client
certificate, and the node registers. From the control plane's point of view,
nothing unusual happened. The node is a normal Kubernetes node.

The third property is a unified networking layer. unbounded-net groups nodes
into sites, allocates pod CIDRs per site and per node, and programs routes and
tunnels on each node. Traffic between sites can use WireGuard for encryption,
GENEVE, VXLAN, or IPIP for lower overhead on trusted links, or no encapsulation
at all when the underlying network already provides direct L3 reachability. The
choice is made per link scope and can be left to `Auto`.

The fourth property is that there is more than one way to deliver the agent.
You can let machina install it over SSH, let a cloud provider's cloud-init run
it on first boot, let metalman serve it during a PXE boot, or run the manual
bootstrap script yourself on a host you already manage. All four paths end in
the same place: the agent runs, the node joins, unbounded-net assigns addressing
and routes, and the node is reachable through the cluster network.

{{< callout type="note" >}}
Unbounded does not require you to change your CNI for a single-site cluster.
If your cluster already has a CNI, set `manageCniPlugin: false` on the `Site`
and unbounded-net will manage only the inter-site tunnels and routing. See
[Networking]({{< relref "concepts/networking" >}}) for the tradeoffs.
{{< /callout >}}

## Components

Unbounded is split into an operator, a set of controllers, a node agent, and a
command-line plugin. The operator is the entry point: `kubectl unbounded install`
deploys it, and it owns the CRDs and the component workloads declared by
`Site.spec.components`. Networking and machina are required cluster singletons
and are reconciled whenever at least one `Site` exists. Metalman, gantry, and
the token refresher are configured through the same `Site` component block.

The table lists the main pieces and where they run. The sections that follow
explain what each one does and which decisions it owns.

| Component | Workload or binary | Runs on | Responsibility |
|---|---|---|---|
| `unbounded-operator` | Deployment | Control plane | CRD lifecycle and per-site component reconciliation |
| `unbounded-agent` | Binary on each host | Worker host | Host preparation, rootfs, kubelet and container runtime |
| `machina` | Deployment | Control plane | SSH provisioning of `Machine` resources and node join |
| `machine-ops-controller` | Deployment | Control plane | Provider operations behind `MachineAllocation` |
| `unbounded-karpenter` | Deployment | Control plane | Upstream Karpenter with the Unbounded cloud provider |
| `metalman` | Deployment, one per site | Site provisioning network | DHCP, TFTP, HTTP boot, Redfish, TPM attestation |
| `unbounded-net-controller` | Deployment | Control plane | Sites, pod CIDR allocation, gateway pools, peering |
| `unbounded-net-node` | DaemonSet | Every node | CNI, tunnels, routes, eBPF dataplane |
| `kubectl-unbounded` | CLI plugin | Operator workstation | Install, site initialization, machine registration |

### unbounded-agent: Node Bootstrap Agent

The agent is a single binary that you deliver to each host. Delivery is the part
that varies by provisioning path; once the binary is on the host, the work is
the same. The agent reads a JSON configuration file, prepares the host, builds
the node root filesystem, starts the container runtime and kubelet, and
completes TLS bootstrap with the token or credential it was given.

The default isolation mode is `nspawn`. The agent prepares the host, then starts
a `systemd-nspawn` machine whose root filesystem comes from an OCI image. Kubelet
and containerd run inside that machine, which keeps the node stack separate from
the host's own packages and services. The `none` mode runs the node stack
directly on the host, and the experimental `container` mode runs it in a
privileged container. You select the mode with `spec.isolation` on the
`Machine` or with `--isolation` where the CLI supports it.

During the rootfs phase the agent detects the host CPU architecture and any
available GPUs. Architecture detection matters because kubelet, containerd,
runc, the CNI plugins, and the OCI rootfs are all architecture-specific. GPU
detection matters because the node needs the right device grants and runtime
configuration before pods that request a GPU can start. The agent also checks
the host during `preflight`, which lets you find missing kernel features,
packages, or devices before a join is attempted.

A minimal agent configuration looks like this. The controllers generate the real
file for you; the shape is useful when you debug a host or build your own
delivery mechanism.

```json
{
  "MachineName": "node-01",
  "Cluster": {
    "CaCertBase64": "<base64-encoded cluster CA>",
    "ClusterDNS": "10.96.0.10",
    "Version": "v1.34.0"
  },
  "Kubelet": {
    "ApiServer": "https://<api-server>:443",
    "Auth": {
      "BootstrapToken": "<token-id>.<token-secret>"
    },
    "Labels": {
      "unbounded-cloud.io/site": "my-site"
    },
    "RegisterWithTaints": []
  }
}
```

The file is passed to the agent through the `UNBOUNDED_AGENT_CONFIG_FILE`
environment variable. `unbounded-agent start` runs the host, rootfs, and
node-start phases in order. A long-running `unbounded-agent daemon` then keeps
the node lifecycle managed as a systemd unit, and the same binary provides
`exec` and `logs` commands for inspecting the node stack. The agent records
install state under `/var/lib/unbounded/agent/` so a retry can reuse completed
work instead of reinstalling everything.

### machina: SSH Provisioning Controller

machina is the controller for hosts that already run an operating system and
are reachable over SSH. It watches `Machine` resources, checks whether the host
is reachable, and, when it is, installs and starts the agent. This is the
simplest path to reason about: if you can SSH to the host, you can add it to the
cluster.

The reachability check is a TCP connection probe, not a full SSH login. It dials
the host's SSH address with a short timeout. If a bastion is configured, the
probe first establishes an SSH connection to the bastion and dials the target
through that connection. A failed probe is not an error that gives up. The
`Machine` stays in `Pending`, the SSH reachable condition is set to false, and
the controller retries later.

When the probe succeeds, machina builds an SSH client from the `Machine` spec,
using the private key referenced by `spec.ssh.privateKeyRef`. A machine behind a
jump host sets `spec.ssh.bastion`, and the bastion can use its own key or inherit
the machine's key. This covers the common on-premises case where the cluster can
reach only a bastion and the target is on the far side.

Provisioning over SSH is deliberately boring. machina copies the agent install
script to `/tmp/machina-agent-install.sh`, uploads a generated agent JSON
configuration to `/tmp/unbounded-agent.json`, and executes the script with
`sudo` and `UNBOUNDED_AGENT_CONFIG_FILE` pointing at that file. Both temporary
files are removed when the operation finishes. The bootstrap token comes from
the Secret named by `spec.kubernetes.bootstrapTokenRef`; if the reference is
missing, the `Machine` moves to `Failed` rather than guessing a credential.

After the script succeeds, machina moves the `Machine` to `Joining` and watches
the Kubernetes `Node` with the machine's node name, or the explicit name in
`spec.kubernetes.nodeRef`. When the `Node` appears, the `Machine` becomes
`Ready`. If the `Node` later disappears, the `Machine` returns to `Joining` so
the controller can wait for it to come back instead of declaring success
forever.

machina also owns the versioned machine configuration and the daemon credential
side of the join. `MachineConfiguration` behaves like a Deployment: edits
produce versioned snapshots that become immutable once a machine uses them, and
a machine can pin a version or follow the latest locked version. The controller
approves client certificate requests from the agent's daemon controller when the
request presents a valid bootstrap token or a credential recorded on the
matching `Machine`, and it requires an explicit Machine-to-Node binding before
it approves a renewal. That keeps a token from being reused to claim a different
node.

### Cloud API Provisioning: Karpenter-based Controller

The cloud API path removes SSH from the picture. Upstream Karpenter watches for
pods that cannot be scheduled, decides that the cluster needs more capacity, and
asks a cloud provider to create it. Unbounded supplies a Karpenter build with a
Kubernetes-native provider, so the same autoscaling loop works without a cloud
SDK or provider credentials inside Karpenter itself. The Karpenter plugin is
installed from its own manifests rather than by `kubectl unbounded install`, and
it is not a per-site component.

The provider reads `MachineClass` resources and presents their offerings as
instance types. A `MachineClass` names a `Site`, selects a provider adapter, and
points at provider-specific configuration in a ConfigMap. When Karpenter decides
to add capacity, it creates a `MachineAllocation`, one per NodeClaim. The
allocation is a consumer-owned request for one machine from a class, and its UID
owns every remote effect made on its behalf.

The machine-ops controller then takes over. It freezes the allocation's inputs
into a snapshot, creates a `Machine`, and starts a `HostAllocate` operation
through the provider adapter registered for the class. The adapter carries out
the launch against its provider API, passing cloud-init user data that installs
and starts the agent. The agent joins with the allocation's bootstrap token, and
the allocation reports the resulting node back to Karpenter. A release operation
reverses the process when the capacity is no longer needed.

A provider adapter has to implement both `HostAllocate` and `HostRelease` to
serve allocations; the controller rejects a provider that registers capacity
without them. The repository ships an Azure VM adapter for day-2 host
operations, so the provider that actually launches cloud instances is supplied
at the adapter layer. This is the same boundary that makes the API
provider-neutral: the `MachineClass` and `MachineAllocation` contract stays
fixed while adapters translate it to their own provider API.

GPU and specialized compute fit this path naturally because the provider adapter
and the `MachineClass` offerings are the only provider-specific parts. The
provider publishes what it can offer, including architecture labels and the
extended resources its instances provide, and Karpenter schedules against those
offerings.

### metalman: Bare-Metal PXE Controller

metalman handles hosts that have no operating system yet. It is a single binary
with the boot servers built in, so a site needs one Deployment rather than a
collection of DHCP, TFTP, and HTTP services. The operator creates that
Deployment when a `Site` enables metalman, and each instance is scoped to one
site by the `unbounded-cloud.io/site` label.

A `metalman serve-pxe` process serves four things. DHCP assigns addresses from
the static leases declared on `Machine` resources and points the firmware at the
bootloader. TFTP serves the initial bootloader. HTTP serves the kernel,
initramfs, templated configuration, artifacts, and the attestation endpoint.
A health endpoint exposes liveness and readiness probes. The default ports are
67 for DHCP, 69 for TFTP, 8880 for HTTP, and 8081 for health.

DHCP has three operating modes. Interface mode binds to a named interface and
listens for broadcast requests, which suits a controller directly attached to
the provisioning network. Auto-interface mode discovers the interface from the
server bind address. Relay mode, with neither option set, listens for unicast
packets only and expects a DHCP relay to forward requests from a remote subnet.
Choose the mode that matches the network between metalman and the BMC or host.

Redfish gives metalman power control and boot-override control. A `Machine` with
a `redfish` block can be rebooted or repaved without physical access, and
metalman pins the BMC's TLS certificate fingerprint on first connection. This is
what makes `kubectl unbounded machine repave` work unattended.

Token delivery is the security-sensitive part. metalman issues a short-lived
bootstrap token based on the source IP of the request, matching the requesting
address to a `Machine` NIC. It delivers the token using the TPM 2.0 credential
encryption workflow, so the host can prove the token was encrypted to its
endorsement key. The endorsement key is recorded on the `Machine`, and a
new attestation replaces the previous credential. The certificate approver then
accepts kubelet requests only when the credential ID matches that `Machine`.
Because the source IP is the TCP peer address, metalman is not supported behind
an HTTP proxy that rewrites the client address.

### unbounded-net: Multi-Site Networking

unbounded-net is the networking layer that makes multiple locations behave like
one cluster. It has a control-plane controller and a node agent. The controller
assigns pod CIDRs and tracks site membership, gateway pools, and peering. The
node agent writes CNI configuration, creates tunnel interfaces, programs routes
and eBPF maps, and reports health.

A `Site` defines a location by the CIDR ranges that contain its nodes' internal
IPs. The controller matches nodes to a site by those ranges and labels them with
the site name. It then allocates pod CIDRs from the site's assignment rules.
Assignments can filter by node name, set per-node block sizes, and use
priorities, which lets a site carve out different pools for different node
classes. Every node gets a slice of the pod address space, so pods in different
sites do not collide.

Sites can be peered directly, and traffic can travel through gateway nodes
instead of a full mesh. A `GatewayPool` selects gateway nodes by label, and a
`SitePeering` declares which sites should exchange traffic and whether their
nodes should mesh directly. Gateway pools are useful when a site has many nodes
and you do not want a tunnel between every pair, or when a site is reachable only
through a small number of routers.

The data plane is per node and uses eBPF. A TC egress program attached to a
dummy interface called `unbounded0` looks up the destination in LPM trie maps and
redirects the packet to the right tunnel. Shared `geneve0`, `vxlan0`, and `ipip0`
interfaces carry unencrypted tunnels, while WireGuard uses per-peer interfaces.
The protocol can be set per scope to `WireGuard`, `IPIP`, `GENEVE`, `VXLAN`, or
`None`. `Auto` chooses based on the link: links that use public addresses get
WireGuard, and links that use only internal addresses get GENEVE.

## Key Custom Resources

Unbounded uses two API groups. Everything about a host and its lifecycle lives
in `unbounded-cloud.io/v1alpha3`. Everything about how traffic moves between
hosts lives in `net.unbounded-cloud.io/v1alpha1`. Keeping the groups separate
means you can grant a networking operator access to the network topology without
also granting access to machine credentials, and it keeps the machine API from
depending on the networking implementation.

All of the resources below are cluster-scoped. They describe shared
infrastructure, not a namespaced workload, so their names are unique across the
cluster. A `Machine` can still reference a namespaced Secret for an SSH key or
a BMC password; the reference carries the namespace because the Secret does.

| Resource | API group | Purpose |
|---|---|---|
| `Machine` | `unbounded-cloud.io/v1alpha3` | One host, its access method, and its Kubernetes join settings |
| `Site` | `unbounded-cloud.io/v1alpha3` | A location, its node CIDRs, pod CIDR rules, and optional components |
| `MachineClass` | `unbounded-cloud.io/v1alpha3` | A provider-neutral capacity template for cloud provisioning |
| `MachineAllocation` | `unbounded-cloud.io/v1alpha3` | A request for one machine from a `MachineClass` |
| `MachineConfiguration` | `unbounded-cloud.io/v1alpha3` | A versioned configuration profile for a class of machines |
| `MachineConfigurationVersion` | `unbounded-cloud.io/v1alpha3` | An immutable snapshot of a configuration profile |
| `MachineOperation` | `unbounded-cloud.io/v1alpha3` | An action such as reboot, replace, or allocation launch |
| `GatewayPool` | `net.unbounded-cloud.io/v1alpha1` | A labeled set of nodes that route traffic between sites |
| `SitePeering` | `net.unbounded-cloud.io/v1alpha1` | Direct peering between a set of sites |
| `SiteNodeSlice` | `net.unbounded-cloud.io/v1alpha1` | A batch of node mappings published for node agents |
| `SiteGatewayPoolAssignment` | `net.unbounded-cloud.io/v1alpha1` | The gateway pool a site uses for a scope |
| `GatewayPoolPeering` | `net.unbounded-cloud.io/v1alpha1` | Peering between two gateway pools |

The `Machine` resource is the one you will touch most often. Its `spec.ssh`
block covers SSH access, including a bastion. Its `spec.pxe` and
`spec.host.netboot` blocks cover bare-metal boot configuration, including DHCP
leases, the machine image, the netboot image, the boot protocol, and Redfish
access. Its `spec.kubernetes` block carries the Kubernetes version, node labels,
taints, the bootstrap token reference, and the node reference. Its
`spec.isolation` field selects where the node stack runs, and `spec.agent`
selects the OCI image and download sources for the agent.

The `Site` resource is the bridge between provisioning and networking. On the
provisioning side, `spec.components` enables optional components such as
metalman or gantry. On the networking side, `spec.nodeCidrs` defines membership,
`spec.podCidrAssignments` defines the pod address pools, and
`spec.tunnelProtocol` and `spec.tunnelMTU` define how the site connects to its
peers. `status.conditions` publishes one condition per component, so you can
wait on a `Site` and know when its workloads are ready.

The networking resources describe topology rather than individual machines.
`GatewayPool` selects nodes by label and declares which CIDRs they route.
`SitePeering` lists the sites that should exchange traffic and controls whether
their nodes mesh directly. The controller creates `SiteNodeSlice` objects in
batches so a node agent can read the node-to-site mapping without listing every
node in a large cluster. Do not create `SiteNodeSlice` or `GatewayPoolNode`
objects by hand; they are controller-owned.

## What Happens When You Add a Node

The three automated paths differ in how the agent reaches the host and how the
host proves it should receive a token. They converge on the same join sequence.
The sections below follow each path in order and then describe the shared
ending.

### SSH Path

The SSH path assumes the host already runs Linux, has SSH enabled, and is
reachable from the cluster or through a bastion. It is the path to use for
existing machines, on-premises servers, and any host where you already have
credentials.

1. Install the operator. `kubectl unbounded install` applies the operator
   manifests and waits for the operator rollout. The operator owns CRD lifecycle,
   so the API types appear only after it starts.
2. Initialize a site. `kubectl unbounded site init` creates a reserved
   `cluster` Site for the existing cluster, a Site for the remote location, a
   default gateway pool, a site-to-gateway-pool assignment, and a bootstrap
   token scoped to the site.
3. Register the machine. `kubectl unbounded machine register` applies the SSH
   Secret when you pass a key file, resolves the site's bootstrap token, detects
   the cluster Kubernetes version, and creates a `Machine` labeled with the site
   name. It fails with a pointer to `site init` if the site has no token yet.
4. machina probes the host and installs the agent. The controller copies the
   install script and generated JSON configuration, runs the script with `sudo`,
   and removes the temporary files.
5. The agent prepares the host and starts the node stack. It installs host
   packages, downloads the rootfs artifacts, starts the `nspawn` machine, starts
   containerd and kubelet, and bootstraps TLS.
6. machina watches for the `Node` and moves the `Machine` to `Ready`.

The command sequence looks like this. The CIDR flags describe your existing
cluster's node and pod ranges and the new site's node and pod ranges; they must
not overlap.

```bash
kubectl unbounded install

kubectl unbounded site init \
  --name my-site \
  --cluster-node-cidr 10.0.0.0/16 \
  --cluster-pod-cidr 10.0.1.0/16 \
  --node-cidr 10.244.0.0/16 \
  --pod-cidr 10.244.1.0/16

kubectl unbounded machine register \
  --site my-site \
  --name node-01 \
  --host 203.0.113.10 \
  --ssh-username azureuser \
  --ssh-private-key ~/.ssh/id_ed25519
```

If the TCP probe fails, the `Machine` stays in `Pending` and reports the SSH
reachable condition as false. That usually means a firewall rule, a wrong port,
or a bastion that is not accepting the configured key. If the probe succeeds but
provisioning fails, the `Machine` moves to `Failed`; the controller retries from
that phase, so you can fix the host or the spec and let it try again. A
provisioning attempt that stays in progress past the controller's timeout is
also moved to `Failed`, which prevents a stale attempt from blocking the machine
indefinitely.

### Cloud API Path

The cloud API path assumes you have a provider adapter and a `MachineClass` that
the adapter considers ready. It is the path to use when you want capacity to
appear automatically in response to unschedulable pods.

1. Karpenter sees a pod that cannot be scheduled and decides to add capacity.
2. Karpenter's scheduler evaluates the offerings published for the class and
   chooses an instance type that satisfies the pod's requirements.
3. Karpenter creates a `MachineAllocation` for the chosen offering. The
   allocation records the consumer, the class, the requested resources, and the
   node template.
4. The machine-ops controller snapshots the allocation inputs, creates a
   `Machine`, and starts a `HostAllocate` operation with the provider adapter.
5. The provider adapter launches the instance and passes cloud-init user data.
   The agent installs, joins with the allocation's token, and reports status.
6. The allocation records the node reference and reports capacity back to
   Karpenter.

This path has no SSH step and no bastion. The host comes up with the agent
already running, because cloud-init runs the install on first boot. The tradeoff
is that provider-specific behavior lives in the adapter and the provider
configuration ConfigMap, and the `MachineClass` must be ready before an
allocation can proceed. If the class is not ready, the allocation waits; if its
inputs change, the allocation reports the change instead of launching against a
stale snapshot.

### PXE Path

The PXE path assumes a bare-metal host with a BMC, a provisioning network, and a
machine image. It is the path to use for new hardware that has no operating
system.

1. Create a `Machine` with a netboot image, DHCP leases, and optionally Redfish
   credentials.
2. The host boots from the network. metalman's DHCP server offers the static
   lease and points the firmware at the bootloader.
3. TFTP serves the bootloader, and HTTP serves the kernel, initramfs, and
   templated configuration.
4. The installer writes the machine image to the target disk and boots the
   installed system.
5. The first boot runs cloud-init, which installs and starts the agent. The
   agent obtains a bootstrap token from metalman using TPM attestation.
6. The node joins the cluster, and metalman reports boot progress through the
   associated `MachineOperation`.

A minimal `Machine` for this path looks like this. The `redfish` block is
optional, but it is what allows metalman to reboot and repave the host without
physical access.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: node-01
  labels:
    unbounded-cloud.io/site: rack-a
spec:
  host:
    netboot:
      image: ghcr.io/azure/host-ubuntu2404:v1
      dhcpLeases:
        - mac: "aa:bb:cc:dd:ee:01"
          ipv4: "10.0.0.11"
          subnetMask: "255.255.255.0"
          gateway: "10.0.0.1"
          dns: ["10.0.0.1"]
      redfish:
        url: https://10.0.10.11
        username: admin
        passwordRef:
          name: bmc-node-01-pass
          namespace: unbounded-system
          key: password
```

The DHCP lease must match the host's NIC address, and the bootstrap token is
issued only to the address that matches a `Machine` NIC. If the host is behind a
proxy that rewrites the client address, attestation will not work. TPM
attestation also depends on Kubernetes ServiceAccount tokens that carry a
credential ID, which requires Kubernetes 1.30 or later. Without attestation, the
boot still needs a token source, so plan the network and the cluster version
before you rely on an unattended PXE path.

### All Paths Converge Here

Whichever path delivered the agent, the ending is the same. The agent writes
kubelet configuration, starts kubelet with the bootstrap token, and waits for the
API server to issue a client certificate. Once kubelet registers, the `Node`
object appears with the labels and taints from the `Machine` spec. unbounded-net
sees the node, matches it to a site, assigns pod CIDRs, and programs the local
CNI and tunnel configuration. Gateway peers and site peers establish their
tunnels, and routes for remote pod CIDRs point at the local data plane.

For SSH-provisioned machines, machina tracks this progression in
`status.phase`. The phases are useful when you are debugging because they tell
you which part of the join the controller is waiting on. Machines provisioned by
other paths report their progress through the owning controller: the allocation
for a cloud instance, and the `MachineOperation` for a PXE boot.

| Phase | Meaning |
|---|---|
| `Pending` | The machine exists but is not yet reachable or not yet provisioned |
| `Rebooting` | The host is being rebooted as part of an operation |
| `Provisioning` | The controller is installing or starting the agent |
| `Joining` | The agent has run and the controller is waiting for the `Node` |
| `Ready` | The `Node` exists and matches the machine |
| `Failed` | Provisioning or an operation failed; the controller retries |

A `Machine` in `Ready` is not frozen. The controller keeps watching the `Node`,
and if the `Node` disappears it moves the `Machine` back to `Joining`. That
distinction matters for operations: a node that is being repaved should pass
through a non-ready phase, and a node that silently vanished should not continue
to look healthy.

When something does not converge, start with the status of the resource that
owns the failing step. For an SSH machine, the provisioning, provisioned, and
SSH reachable conditions usually name the failure. Then check the component that
owns that step: machina for SSH provisioning, the machine-ops controller and
provider adapter for cloud allocations, metalman for PXE and attestation, and
unbounded-net for addressing and routes.

## Next Steps

If you are new to the project, start with the
[Getting Started]({{< relref "guides/getting-started" >}}) walkthrough, then use
[Bring Your Own Cluster]({{< relref "guides/existing-cluster" >}}) when you want
to add machines to a cluster you already run.

- For SSH provisioning details and bastion configuration, see the
  [SSH guide]({{< relref "guides/ssh" >}}) and the
  [agent guide]({{< relref "guides/agent" >}}).
- For automated capacity, see
  [Cloud API Provisioning]({{< relref "guides/cloud-api" >}}).
- For bare-metal boot, see [Bare Metal Provisioning]({{< relref "concepts/bare-metal" >}})
  and the [PXE guide]({{< relref "guides/pxe" >}}).
- For sites, tunnels, and gateway pools, see
  [Networking]({{< relref "concepts/networking" >}}) and the
  [networking reference]({{< relref "reference/networking" >}}).
- For a deeper component and data-flow description, see the
  [architecture reference]({{< relref "reference/architecture" >}}).
- For resource fields and CLI commands, see the
  [Machina CRD reference]({{< relref "reference/machina-crd" >}}) and the
  [CLI reference]({{< relref "reference/cli" >}}).

