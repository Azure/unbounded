---
title: "Unbounded"
description: "Run Kubernetes worker nodes anywhere and connect them back to one control plane over encrypted tunnels."
---

Unbounded Kubernetes extends a conformant Kubernetes control plane so that worker
nodes can run wherever the compute is. The control plane stays where it already
runs, whether that is AKS, EKS, GKE, a self-managed cluster, or any other
conformant distribution. The nodes can be virtual machines in a second cloud, GPU
hosts at a specialized provider, servers in a rack behind NAT, or devices at an
edge site. From the control plane's point of view they are ordinary Kubernetes
nodes, and the pods they run use ordinary pod IPs and ordinary Services.

You do not replace your control plane to get this. Unbounded installs a small set
of controllers and custom resources into the cluster you already have, then uses
them to provision hosts, join them, and route traffic between locations. Adding a
node becomes a provisioning problem rather than a networking project: you describe
the host, choose how the agent should reach it, and let the controllers handle
reachability, the join, and the routes.

![Unbounded Kubernetes overview showing a control plane and worker nodes in several locations connected through encrypted tunnels](img/unbounded-overview.svg)

## The Problem It Solves

Kubernetes was designed around a cluster network that is mostly flat and mostly
trustworthy. A typical cluster assumes that every node can reach the API server,
that the API server can reach every kubelet, that nodes can reach each other
directly, and that pod and node CIDRs do not overlap. Those assumptions hold in a
single VPC or a single LAN. They break the moment compute moves somewhere else.

### Capacity Is Tied to a Network

If your provisioning tooling is built around one provider's instance groups or
virtual machine scale sets, adding a second provider means building a second tool.
Using another region of the same cloud usually means a second set of network
constructs. GPU capacity is often available from providers that are not your
primary cloud, with their own APIs, images, and availability profile. You want to
use that capacity without moving the control plane or rebuilding the cluster
around each provider.

### Provisioning Assumes Reachability

A host behind a firewall has no inbound access, so the control plane cannot dial
it. Provisioning it over SSH may work only through a bastion, and even then the
node needs a secure path back into the cluster network once it joins. A host that
has no operating system at all cannot run an SSH agent, so it needs DHCP, a
bootloader, a kernel, and a first boot that installs the agent before any of the
usual join steps can happen. Each case raises the same questions: how does the
agent get onto the host, how does the host learn the API server endpoint and the
cluster CA, and how do you keep the bootstrap token from leaking or being reused
on the wrong machine?

### Addresses Collide Across Locations

Even after nodes join, pods in one location have to reach pods in another. That
requires each location to have a non-overlapping pod CIDR, a way to route between
CIDRs, and a data plane that can carry the traffic across links that may be
encrypted, NATed, or intermittent. Without a coordinated address plan, two sites
can allocate the same pod IP and break routing for both.

## Who Unbounded Is For

Unbounded is for platform teams that already run a Kubernetes control plane and
want to attach compute that does not live next to it. If you operate a cluster in
one cloud and need capacity in another, or you have on-premises servers that
should join a managed cluster, this is the path that keeps one API, one identity
model, and one scheduling domain.

It is also for teams whose work is shaped by hardware rather than by regions. AI
and machine learning teams often need GPU capacity from providers that expose
their own APIs, and they need large models, datasets, and container images to
reach many nodes quickly. Operators of edge sites need nodes to join over links
that are not fast or always available. Hardware teams that rack new servers need
a boot path that works before an operating system exists. Unbounded treats each of
these as a delivery and networking variation on the same node lifecycle, not as a
separate product.

## What You Can Do

The capabilities below are different entry points into the same model. Whichever
one you use, the result is a Kubernetes node that the control plane can schedule
to, with addressing and routes supplied by the networking layer.

### Add a Host over SSH

If a host already runs Linux and you can reach it over SSH, machina can install
and start the agent for you. It checks reachability with a TCP probe, connects
through a bastion when one is configured, copies a generated agent configuration,
and runs the installer with `sudo`. The `Machine` then moves through provisioning
and joining phases until the corresponding Kubernetes `Node` appears. This is the
simplest path to reason about and the right one for existing machines and
on-premises servers.

### Allocate Capacity from a Provider API

For automatic capacity, Unbounded supplies a Karpenter build with a
Kubernetes-native cloud provider. When pods cannot be scheduled, Karpenter creates
a `MachineAllocation`, the machine operations controller turns it into a
`Machine`, and a provider adapter launches the instance with cloud-init that
installs the agent on first boot. A `MachineClass` describes what the provider can
offer, so the same autoscaling loop works across providers without putting a
provider SDK inside Karpenter.

### Boot Bare Metal over the Network

metalman handles hosts that have no operating system. One Deployment per site
provides DHCP, TFTP, an HTTP boot service, Redfish power control, and a health
endpoint. A machine boots from the network, receives its kernel and initramfs,
writes the machine image to disk, and on first boot installs the agent. Token
delivery uses TPM 2.0 attestation so the host can prove the token was encrypted to
its endorsement key, and Redfish lets you reboot or repave the host without
physical access.

### Connect Sites into One Network

unbounded-net makes multiple locations behave like one cluster. A `Site` groups
nodes by the address ranges that contain them, and the controller allocates a pod
CIDR to each node so addresses never collide between sites. Sites can be peered
directly or connected through gateway pools, which avoids a tunnel between every
pair of nodes. The per-node data plane uses eBPF and can carry traffic with
WireGuard, GENEVE, VXLAN, or IPIP, or use no encapsulation when the underlay
already provides direct L3 reachability.

### Distribute Images and Cache Large Objects

Two components reduce the cost of moving the same bytes to many nodes. Gantry is a
peer-to-peer OCI distribution agent that presents itself to containerd as a
registry mirror, so a small cohort of nodes fetches each layer once and the rest
stream it from neighbors instead of the upstream registry. Racer is a distributed
read-through cache for large, read-mostly objects that keeps pages on node-local
disks and in memory and serves cache hits over a Unix socket without crossing the
network. Both are optional and both are most useful at fleet scale.

### Run GPU Workloads

GPU and specialized compute fit the same model. The agent detects available GPUs
during the rootfs phase and configures the device grants and runtime settings
before pods that request a GPU can start. Provider offerings can publish
architecture labels and the extended resources their instances provide, so
Karpenter schedules against real capacity rather than a fixed instance catalog.

## How the Pieces Fit Together

Unbounded separates three concerns that Kubernetes normally leaves implicit:
where the control plane runs, where worker nodes run, and how traffic moves
between them. The control plane is fixed and stays in place. A worker node is a
Linux host described by a cluster-scoped `Machine` resource, which records how to
reach the host, how to install the agent, which Kubernetes version to run, and
which labels and taints the node should register with. Networking is described by
resources in a separate API group, so access to the topology can be granted
without also granting access to machine credentials.

The operator is the entry point. `kubectl unbounded install` deploys it, and it
owns the custom resource definitions and the component workloads declared by
`Site.spec.components`. Networking and machina are required cluster singletons
and are reconciled whenever at least one `Site` exists. Metalman, Gantry, and the
token refresher are enabled through the same component block. On each host, a
single agent binary prepares the machine, builds the node root filesystem, starts
the container runtime and kubelet, and completes TLS bootstrap. By default the
node stack runs inside a `systemd-nspawn` machine so it stays separate from the
host's own packages and services.

| Resource | What it describes |
|---|---|
| `Machine` | One host, how to reach it, and how it joins the cluster |
| `Site` | A location, its node CIDRs, pod CIDR rules, and optional components |
| `MachineClass` | A provider-neutral template for cloud capacity |
| `MachineAllocation` | A request for one machine from a `MachineClass` |
| `GatewayPool` | A labeled set of nodes that route traffic between sites |
| `SitePeering` | Which sites exchange traffic and whether nodes mesh directly |

Provisioning paths differ in how the agent reaches the host and how the host earns
a bootstrap token. They converge on the same join sequence: kubelet presents the
token, the control plane issues a client certificate, the `Node` registers, and
unbounded-net matches the node to its site, assigns pod CIDRs, and programs the
CNI configuration and tunnels. If the node disappears later, the owning
controller notices and moves the machine out of its ready state rather than
reporting success forever.

## Where to Start

If you are new to the project, read the concepts overview first to get the mental
model, then follow the getting started walkthrough, which builds a cluster and
adds a remote node over SSH. When you already operate a cluster, use the existing
cluster guide to attach machines to it. From there, pick the provisioning path
that matches your hardware and the networking configuration that matches your
sites.

Two practical rules keep the first deployment simple. Give each site node and pod
CIDRs that do not overlap with the cluster or with each other, because the
networking layer cannot repair a collision after the fact. Then watch the status
of the resource that owns the step that is failing: the `Machine` conditions for
provisioning and reachability, the `Site` conditions for component readiness, and
the allocation or machine operation for paths that are not driven by SSH.

## Learn More

- [Concepts]({{< relref "concepts" >}}) explains sites, machines, networking, bare
  metal, and the Racer cache.
- [Concepts overview]({{< relref "concepts/overview" >}}) is the full mental model
  of the components and the node lifecycle.
- [Getting Started]({{< relref "guides/getting-started" >}}) is the end-to-end
  walkthrough for a new cluster and a remote node.
- [Guides]({{< relref "guides" >}}) collects the provisioning, networking, and
  operations tasks you will repeat.
- [Architecture]({{< relref "reference/architecture" >}}) describes the components
  and data flows in depth.
- [Reference]({{< relref "reference" >}}) holds exact fields, flags, and API
  details for the CLI, custom resources, GPUs, and networking.
