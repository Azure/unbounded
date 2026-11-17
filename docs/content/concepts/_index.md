---
title: "Concepts"
description: "The mental model behind Unbounded Kubernetes: a control plane that stays put, worker locations described by Site and Machine, pluggable provisioning paths, and shared networking and caching layers."
---

Unbounded Kubernetes extends a Kubernetes control plane so that worker nodes can
run in places the control plane was never designed to reach. The control plane
stays where it is. The workers can be virtual machines in another cloud, GPU
hosts at a specialized provider, servers in a rack behind NAT, or hardware at an
edge site. What changes is not Kubernetes itself but the assumptions around it:
how a host is provisioned, how it joins the cluster, and how pod traffic moves
between locations.

This page gives you the mental model you need before the individual concept
pages. It explains the split between the control plane and worker locations, the
difference between a `Site` and a `Machine`, the provisioning paths that deliver
the agent, and the networking and caching layers that make distributed workers
behave like one cluster. Read it once end to end, then follow the link to the
page that covers the part you are about to configure.

## The control plane stays where it is

The starting point is an existing, conformant Kubernetes cluster. It can be AKS,
EKS, GKE, a self-managed cluster, or any other distribution that passes
conformance. Unbounded does not replace it, fork it, or require a special build.
`kubectl unbounded install` deploys `unbounded-operator` into that cluster, and
the operator owns the lifecycle of the custom resource definitions and the
component workloads that Unbounded adds.

That design decision shapes everything else. Because the control plane is
unchanged, the API server, scheduler, controllers, and etcd keep their normal
roles. A worker that joins through Unbounded is an ordinary Kubernetes `Node`
with ordinary kubelet credentials. You can inspect it, cordon it, drain it, and
delete it with the tools you already use. The unusual parts are confined to how
the host was prepared and how packets reach it.

The operator is the entry point for component management. Networking and machina
are required cluster singletons and are reconciled whenever at least one `Site`
exists. Metalman, Gantry, and the token refresher are controlled per site through
the site's component block, so a location that does not need PXE does not run
metalman, and a site can opt out of Gantry. Cluster-scoped configuration
that applies across sites, such as workload overrides, lives outside the `Site`
resource because the underlying singletons are shared.

## Worker locations are separate networks

A worker node is still a Linux host running kubelet and a container runtime. What
differs is its network position. A node in a second VPC may have no route to the
cluster VPC. A node behind NAT cannot be dialed by the API server. A node at a
provider may expose only a public address, while the cluster expects private
addresses. An edge device may have an intermittent link and a small pipe.

Kubernetes normally leaves those details implicit: it assumes nodes can reach the
API server, the API server can reach kubelets, nodes can reach each other, and
address ranges do not overlap. Unbounded makes each assumption explicit and gives
you a place to describe the answer. That is why the documentation talks about
locations rather than about a single cluster network. A cluster can contain
several locations, and each location has its own node range and pod address
pools.

Once you separate "where the control plane runs" from "where workers run", adding
a node stops being a networking project. You describe the host, choose how the
agent reaches it, and let the controllers handle reachability, the Kubernetes
join, address allocation, and routes. The next section explains the two resources
that carry those descriptions.

## Site and Machine are different questions

New users often assume a `Site` is a group of machines or that a `Machine`
belongs to a site object. They are related, but they answer different questions,
and both are cluster-scoped. A `Site` answers "where is this location, what
addresses belong to it, and how does it connect to other locations?" A `Machine`
answers "which host is this, how do I reach it, and how should it join the
cluster?"

A `Site` is defined primarily by the CIDR ranges that contain its nodes'
internal IPs. The networking controller matches a node to a site by those ranges
and labels the node with the site name. The site also declares pod CIDR pools and
per-node block sizes, so every node gets a non-overlapping slice of the pod
address space. Tunnel protocol, tunnel MTU, local CIDRs, and the optional
component block are site-level decisions because they apply to the whole
location.

A `Machine` describes one host. Its access block covers SSH, including an
optional bastion. Its netboot block covers DHCP leases, the machine image, the
netboot image, and Redfish access for bare metal. Its Kubernetes block carries
the version, node labels, taints, the bootstrap token reference, and the node
reference. Its isolation field selects where the node stack runs, and its agent
block selects the agent image and download sources. The same resource works for
an SSH-reachable server, a cloud instance, and a bare-metal host because those
details are fields rather than assumptions baked into a controller.

| Question | Resource |
|---|---|
| Where do nodes run, and which addresses belong to that location? | `Site` |
| Which host joins, how do we reach it, and what does it register as? | `Machine` |
| What capacity should exist for an autoscaling group? | `MachineClass` and `MachineAllocation` |

The two resources meet at the node. When a machine joins, the node's internal
address determines its site, and the site's assignment rules determine its pod
CIDRs. Nothing about the machine's provisioning path changes that mapping. A
cloud instance, an SSH host, and a PXE boot all become the same kind of node once
they register.

## Four ways to deliver the agent

Every path ends with `unbounded-agent` running on the host, but the path
determines how the binary gets there and how the host earns a bootstrap token.
Choosing a path is mostly a question of what already exists on the machine.

The SSH path is for hosts that already run Linux and are reachable over SSH,
directly or through a bastion. machina watches `Machine` resources, probes the
host with a TCP connection, installs the agent over SSH with `sudo`, and waits
for the resulting `Node` to appear. This is the simplest path to reason about,
and it fits existing servers, on-premises hardware, and any host where you
already hold credentials. A failed probe leaves the machine in `Pending` and the
controller retries, so a firewall change or a corrected key is enough to make
progress.

The cloud API path is for capacity that should appear automatically in response
to unschedulable pods. Upstream Karpenter runs with the Unbounded provider, reads
`MachineClass` resources as instance type offerings, and creates a
`MachineAllocation` when it decides to add capacity. The machine-ops controller
snapshots the allocation, creates a `Machine`, and calls the provider adapter to
launch the instance with cloud-init user data that installs the agent. There is
no SSH step and no bastion. The tradeoff is that provider behavior lives in the
adapter and its configuration, and a class must be ready before an allocation can
proceed.

The PXE path is for bare metal that has no operating system yet. metalman serves
DHCP, TFTP, and HTTP from one Deployment scoped to a site, boots the host from
the network, writes a machine image to disk, and relies on Redfish for power and
boot control. On first boot the host obtains a short-lived bootstrap token tied
to its source address and encrypted to its TPM endorsement key. This is the path
that makes an unattended rack rollout possible, and it has the most environment
requirements: a provisioning network, matching DHCP leases, and a cluster new
enough to support the credential flow.

The manual path is for hosts you already manage and want to bring up yourself.
You deliver the agent binary and its JSON configuration, then run the agent with
a token you obtained separately. It is useful for custom images, existing
automation, and debugging, because it exposes the same inputs the controllers
generate for you.

All four paths converge on normal Kubernetes TLS bootstrap. The agent prepares
the host, starts the node stack, presents the bootstrap token, and waits for the
API server to issue a client certificate. For SSH machines, `status.phase` tracks
the progression through `Pending`, `Provisioning`, `Joining`, `Ready`, and
`Failed`, and returns to `Joining` if the node disappears later. Machines
provisioned by other paths report progress through the allocation or the
`MachineOperation` that owns the work.

## The networking layer

Networking is where most of the complexity of distributed workers would normally
land, so Unbounded makes it a first-class layer. `unbounded-net` has a
control-plane controller and a node agent. The controller assigns pod CIDRs and
tracks site membership, gateway pools, and peering. The node agent writes CNI
configuration, creates tunnel interfaces, programs routes and eBPF maps, and
reports health.

A site's pod address pools are not bookkeeping you do by hand. You declare the
pools and rules, the controller matches each node to a site, allocates a
node-sized block, and records it on the `Node`. Overlapping node CIDRs and
overlapping pod assignment pools are rejected when you create the site, which
turns a routing failure that would otherwise appear weeks later into an admission
error you can fix immediately.

Sites can peer directly, or traffic can pass through gateway nodes selected by a
`GatewayPool`. The protocol is chosen per link scope: `WireGuard` for encrypted
traffic over networks you do not control, `GENEVE`, `VXLAN`, or `IPIP` for lower
overhead on trusted links, and `None` when the underlay already provides direct
L3 reachability. `Auto` resolves based on whether the link uses public or internal
addresses. Encryption therefore follows the path rather than the whole cluster.

The layer is also optional in part. If your cluster already runs Cilium, Calico,
or another CNI, set `manageCniPlugin: false` on the site and unbounded-net manages
only the inter-site tunnels and routes. The site still needs pod CIDR assignment
rules so remote ranges are known, but intra-site networking stays with the CNI
you already operate.

## The caching and distribution layers

The last layer is about bytes rather than packets. Nodes in remote locations
often share a slow or metered link to the same origin, and a rollout can make
every node request the same content at once. Unbounded ships two complementary
systems that keep repeated reads close to the workers.

Racer is a read-through cache for large, read-mostly objects such as datasets,
models, and checkpoints. It runs in the node dataplane, stores immutable object
pages on local disks and in memory, and serves local reads over a Unix domain
socket. When a node misses, it asks the small set of peer nodes that own the page
instead of going back to the origin. Pages are 16 MiB, each page has up to three
owners chosen by weighted rendezvous hashing, and the origin remains the source
of truth. The cache is disposable by design: it can be lost and refilled, and it
is never written to the origin.

Gantry addresses a different kind of fan-out: container images. It runs as a
peer-to-peer OCI distribution agent that presents itself to containerd as a
registry mirror, then resolves digest-addressed content from the local containerd
content store, from a peer that already holds it, or from the upstream registry
through a bounded set of designated pullers. It reuses the node's existing image
store rather than maintaining a second cache, and it never accepts pushes. On a
site where Gantry is enabled, which is the default, the agent is part of the
same distribution story as Racer: one reduces repeated image pulls, the other
reduces repeated object reads.

Both systems depend on the cluster being able to place pods and route traffic
between nodes, which is why they come after the provisioning and networking
layers in the mental model. They also both degrade gracefully. A cold cache or a
Gantry cluster with no prior copy of an image is slower on the first read, not
broken, because the origin path remains available.

## How the pieces fit together

A typical installation follows the model in order. You install the operator into
an existing cluster. You run `kubectl unbounded site init` to create a reserved
`cluster` site for the existing cluster, a site for the remote location, a
default gateway pool, a site-to-gateway-pool assignment, and a bootstrap token
scoped to the site. You then register a machine, either by SSH with
`kubectl unbounded machine register` or by creating a `Machine` for a cloud or
PXE path.

From there the controllers take over. The machine is provisioned and the agent
joins the node. The node appears with the labels and taints from the machine
spec. unbounded-net matches it to a site, assigns pod CIDRs, writes CNI
configuration, and programs tunnels and routes. Gateway peers and site peers
establish connectivity, and remote pod CIDRs become reachable through the local
dataplane. Workloads then pull images through Gantry and read objects through
Racer, using the same pod network as any other cluster.

When something does not converge, follow the same order in reverse. Check the
resource that owns the failing step first: the `Machine` for provisioning, the
allocation for cloud capacity, the `MachineOperation` for a PXE boot, and the
`Site` or node for addressing and routes. Conditions on those resources usually
name the component that needs attention.

## Concept pages

The [Project Overview]({{< relref "concepts/overview" >}}) page is the fullest
statement of this model. It explains the problem Unbounded solves, describes each
component, lists the custom resources in both API groups, and walks through the
SSH, cloud API, and PXE paths step by step with their failure modes.

The [Networking]({{< relref "concepts/networking" >}}) page goes deeper on the
network layer. It covers the controller and dataplane split, site membership and
CIDR allocation, gateway pools and peering, protocol selection, and the path a
packet takes from one pod to another across sites.

The [Bare Metal Provisioning]({{< relref "concepts/bare-metal" >}}) page explains
the PXE path in detail: the boot sequence, the image model, DHCP modes, Redfish
power control, TPM attestation, and the operations that repave or reboot a host
without physical access.

The [Racer Distributed Cache]({{< relref "concepts/racer" >}}) page explains the
cache layer. It covers when a distributed read-through cache is a better fit than
a dedicated cache tier, how capacity and membership work, and the internal page
and ownership model you need in order to operate it.

## Next steps

If you are new to the project, continue from here to the
[Project Overview]({{< relref "concepts/overview" >}}), then follow the
[Getting Started]({{< relref "guides/getting-started" >}}) walkthrough. When you
want to add machines to a cluster you already run, use
[Bring Your Own Cluster]({{< relref "guides/existing-cluster" >}}).

For component and data-flow detail beyond the concept pages, read the
[architecture reference]({{< relref "reference/architecture" >}}). For exact
fields, commands, and protocol behavior, use the
[Machina CRD reference]({{< relref "reference/machina-crd" >}}), the
[CLI reference]({{< relref "reference/cli" >}}), and the
[networking reference]({{< relref "reference/networking" >}}).
