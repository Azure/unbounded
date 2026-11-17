---
title: "Networking Reference"
weight: 3
description: "Reference for the unbounded-net stack, covering its architecture, custom resources, runtime configuration, packet routing, and day-two operations."
---

This section is the reference for unbounded-net, the networking stack that gives pods a single routable address space across multiple Kubernetes sites. It documents the system as it is implemented in this repository: the controllers and node agents that make up the dataplane, the custom resources that describe topology, the runtime settings that operators control, and the paths that packets take between sites.

The reference is meant for platform engineers and cluster operators who are deploying or debugging a multi-site cluster. You should already be comfortable with Kubernetes nodes, pods, CNI plugins, and the basics of Linux routing. You do not need prior knowledge of this project, but you should read [Networking concepts]({{< relref "concepts/networking" >}}) first if you want the problem statement and the vocabulary before the field-level detail.

## What this reference covers

The networking stack answers one question: how does a pod on a node in one location reach a pod on a node in another location, when those nodes may sit behind different networks and may not be able to address each other directly? Unbounded-net answers it with an overlay. Nodes keep using their own underlay addresses, while pod traffic is encapsulated into tunnels whose endpoints are the nodes themselves. Some links carry encrypted WireGuard tunnels, others use unencrypted GENEVE, VXLAN, or IPIP, and the choice is made per link scope.

This section covers the system in five parts, each with its own page. The architecture page describes the controller and node agent, their reconciliation loops, the tunnel dataplane, and the failure modes they are designed to survive. The custom resource page documents the `Site`, `GatewayPool`, peering, and assignment objects, with field tables, validation rules, and complete manifests. The configuration page covers the shared configuration file, controller and node agent settings, protocol defaults, firewall requirements, and resource sizing. The routing flows page follows a packet from a pod interface to a tunnel, through the eBPF endpoint lookup, and back on the return path. The operations page covers installation, monitoring, health checks, diagnostics, and procedures such as adding a site or replacing a gateway.

The reference does not repeat the high-level product overview, and it does not document the broader `unbounded-cloud.io` API. For those, see [Networking concepts]({{< relref "concepts/networking" >}}), [Architecture]({{< relref "reference/architecture" >}}), and the [Machina custom resource reference]({{< relref "reference/machina-crd" >}}). The networking stack is also distinct from the CNI plugin that handles pod sandbox setup on a single node. It can own that role, or it can cooperate with an external CNI when `manageCniPlugin` is disabled, which the architecture and configuration pages describe in detail.

## The networking model

The model has three moving parts. A **site** is a network location and an address plan. A **gateway pool** is a set of nodes that forward traffic between locations. **Peering** objects decide which locations may exchange traffic directly and which must go through gateways. Everything else in the API is a refinement of those three ideas: node slices, gateway node status, assignments, health settings, and tunnel properties.

Read the model in this order. First decide where your nodes live and what addresses they use. Then decide which nodes, if any, will act as gateways. Then decide which sites and pools can talk to each other. Tunnel selection follows from those decisions and from the addresses that the links use.

### Sites

A `Site` declares a network location. Its required field is `spec.nodeCidrs`, the CIDR blocks that contain the internal IP addresses of the nodes at that location. This is the underlay network, such as a VPC subnet or a data center VLAN, not the pod network. The Site controller watches Nodes and assigns each node to a Site when any of its `InternalIP` addresses falls inside one of those blocks. The controller then labels the node with `unbounded-cloud.io/site` so that the rest of the stack can find its location.

Pod addresses come from `spec.podCidrAssignments`. Each assignment names one or more CIDR pools, the per-node block size for IPv4 and IPv6, an optional node name regular expression, and a priority. Lower priority values win when several assignments match. The controller allocates a block to a node and writes it to the node's `podCIDRs` field. If a node already has a pod CIDR, the controller keeps it rather than reallocating, so a restart does not renumber running pods.

A Site also carries the defaults that apply to traffic within it: `manageCniPlugin` controls whether the node agent configures the CNI and the intra-site mesh, `localCidrs` marks networks that should never be reached through a gateway pool, `nonMasqueradeCIDRs` marks addresses that should not be source-NATed on the way out, and `tunnelProtocol` and `tunnelMTU` set the site-wide tunnel preferences. `healthCheckSettings` on the Site govern node-to-node routes inside that site.

Sites do not list their nodes. Instead, the controller maintains one or more `SiteNodeSlice` objects per Site, each holding up to 500 nodes and their WireGuard public keys, internal addresses, and pod CIDRs. Slices exist to keep individual Kubernetes objects below the size limit on large clusters. They are entirely controller-managed and should never be edited.

### Gateways and gateway pools

A `GatewayPool` selects gateway nodes with a label selector. Gateway nodes are ordinary Kubernetes nodes that also forward traffic for a location. The pool's `type` is `External` by default, which means cross-site links to its gateways resolve to the nodes' external IP addresses unless the sites are network-peered, in which case internal addresses are used. An `Internal` pool does not advertise an endpoint for cross-site links and lets the tunnel learn it, which suits pools that are reachable only over a private network. The pool can add `routedCidrs` for networks that should be reached through it, and it can set its own tunnel protocol, MTU, and health check profile.

The gateway pool controller publishes each member as a `GatewayPoolNode` and assigns it a WireGuard listen port for gateway-to-gateway peering, starting at 51821. The node agent updates that object with route advertisements and a heartbeat. The pool's status then summarizes the member count, the sites that are directly connected, and every site that is reachable through the pool, including transitive reachability.

Membership has a consequence that surprises new users. A node that matches any gateway pool selector is removed from its site's `SiteNodeSlice`, even when a mesh peering covers that site, because it is expected to serve as a gateway rather than as a mesh peer. A selector that matches every node in a site therefore leaves that site with no mesh peers. Select gateways with a dedicated label, such as `unbounded-cloud.io/unbounded-net-gateway: "true"`, and keep that label off ordinary workers.

A `SiteGatewayPoolAssignment` connects the two halves of the model. It lists sites and the gateway pools that serve them. It can also override the Site's tunnel protocol for the site-to-gateway links, which lets one site use WireGuard toward an external pool while another uses GENEVE toward a private one. Without an assignment, a site has no gateway path to other locations.

### Peering

Peering answers the permission question. A `SitePeering` lists at least two sites that may exchange traffic. With `meshNodes: true`, which is the default, every node in the listed sites creates direct tunnels to every other node, as if they shared a site. Gateway routing for the node and pod CIDRs of those sites is not built, so the peering is a shortcut that bypasses gateways entirely. This works well when the sites already have fast, reliable connectivity and the node count is modest.

With `meshNodes: false`, the sites are still considered network-peered, but nodes do not mesh. Gateway pool peerings between pools in those sites then use internal IP addresses instead of external ones. This is the right shape when two sites share a private network but a full node-to-node mesh would be too large or too expensive.

A `GatewayPoolPeering` links gateway pools to each other for cross-pool routing. The pools in the peering establish links between their gateway nodes using the selected protocol. If several enabled peerings specify conflicting protocols for the same link, the first peering in lexicographic name order wins and the node agent logs the conflict. An unset protocol does not mask another peering's explicit setting, and an explicit `Auto` on the peering uses endpoint-based preferences without inheriting a lower-scope protocol.

Assignments and peerings answer different questions. An assignment attaches a site to a pool, which gives its workers a local gateway. A peering links two sites or two pools and creates the path for destinations beyond the local pool. If two sites are served by different pools, you need a `GatewayPoolPeering` between those pools before traffic can cross. If they share a pool, their gateway nodes already peer within it, and no pool peering is required. A site with an assignment but no path to a destination, either through a shared pool or through a peering, does not reach that destination.

### Tunnel selection

Five scopes carry a `tunnelProtocol`: `Site`, `GatewayPool`, `SitePeering`, `SiteGatewayPoolAssignment`, and `GatewayPoolPeering`. Each accepts `WireGuard`, `GENEVE`, `VXLAN`, `IPIP`, `None`, or `Auto`. `Auto` is the default and resolves links by the addresses they use: links that traverse external or public addresses become WireGuard, and links that use only internal addresses become GENEVE. The two endpoint-based defaults can be changed with the node agent's `--preferred-private-encap` (default `GENEVE`) and `--preferred-public-encap` (default `WireGuard`) settings.

The scopes form a precedence chain rather than a free-for-all. A more specific scope wins for the links it governs, with two deliberate exceptions. First, the security-wins rule: if any scope in the hierarchy explicitly selects `WireGuard`, the link uses WireGuard regardless of what other scopes say. Encryption is never downgraded by a broader default. Second, an explicit protocol on a `GatewayPoolPeering` takes precedence for the cross-pool gateway link, while an unset value leaves the gateway pool and site fallbacks in place.

`tunnelMTU` is available on the same scopes and accepts a value from 576 to 9000. When it is unset, the node agent derives the effective MTU from the route to each peer and subtracts the encapsulation overhead, and the `node.mtu` setting can cap the result. Setting a scope MTU too high causes large packets to be dropped after encapsulation, which usually appears as connections that establish but stall during bulk transfer.

The following table summarizes the protocols and the interfaces they use. It is a map, not a substitute for the routing and configuration pages: the agent creates each shared tunnel device only when at least one peer needs it, and removes it when the last peer goes away.

| Protocol | Tunnel device | Typical default | Encapsulation overhead |
|----------|---------------|-----------------|------------------------|
| `WireGuard` | `wg51820` for the mesh, `wg51821` and up for gateways | Links that use external or public addresses | 80 bytes in the IPv6 worst case |
| `GENEVE` | `geneve0`, UDP destination port 6081 | Links that use only internal addresses | 58 bytes |
| `VXLAN` | `vxlan0`, UDP destination port 4789 | When the underlay offers better VXLAN offload | 50 bytes |
| `IPIP` | `ipip0` | When encryption is not needed and overhead must be minimal | 20 bytes |
| `None` | No tunnel device | When nodes already have direct L3 reachability | 0 bytes |

`None` is not a way to avoid configuring connectivity. It requires that the pod and node networks are already routable between the peers, and it removes the isolation that an overlay provides.

In practice, leave `Auto` in place unless you have a specific reason to override it. Choose GENEVE for private links when you want the default balance of overhead and metadata. Choose VXLAN when your underlay or hardware offloads VXLAN better than GENEVE, which can matter on high-bandwidth links. Choose IPIP only when the links are trusted, because it carries just an outer IP header with no UDP wrapper. Choose WireGuard whenever traffic crosses a network you do not control, and accept the extra bytes and the encryption cost. Each of these decisions can be made per scope, so you can keep the defaults almost everywhere and override only the links that need it.

### The packet path at a glance

Every node has a dummy interface named `unbounded0` with the NOARP flag, and routes for remote overlay CIDRs point at it. Because the interface is a dummy, the kernel selects it as the egress device and then hands the packet to a TC egress program called `unbounded_encap`. That program looks up the destination in an LPM trie named `unb_endpts`, which holds the tunnel endpoint for each remote prefix. It stamps the packet with the tunnel metadata, rewrites the inner MAC addresses as needed, and redirects the packet to the correct tunnel device, either a shared GENEVE, VXLAN, or IPIP device or a WireGuard interface.

Same-site traffic normally follows that path into `geneve0` and is delivered directly to the peer node. Cross-site traffic for a site without a mesh peering follows a different path: the worker sends the packet to a local gateway over the site-to-gateway tunnel, which is WireGuard by default, the gateway decapsulates it, decides where it goes next, and either re-encapsulates it toward a remote gateway or sends it into a remote-site tunnel. The return path is symmetric because the route and tunnel maps on each node describe both directions. The [routing flows]({{< relref "reference/networking/routing-flows" >}}) page walks through each hop, including the exact BPF helpers and the interface roles.

This design has two practical consequences. There are no per-peer tunnel devices for the shared encapsulations, so a mesh of hundreds of nodes does not create hundreds of interfaces. And route selection stays in the kernel, so ECMP, policy routing, and ordinary troubleshooting tools such as `ip route` continue to work.

### Convergence and health

The control plane runs as a controller Deployment with two replicas and leader-only reconciliation. The leader assigns pod CIDRs, maintains site membership and node slices, resolves gateway pool membership and health endpoints, and computes reachability. The node agent runs as a DaemonSet on every node, including control plane nodes. It manages WireGuard keys and interfaces, writes CNI configuration, programs routes with netlink nexthop objects, and reconciles the eBPF maps and programs.

Health checking is part of the routing model rather than an afterthought. Each gateway node exposes health endpoints derived from its pod CIDRs, and peers probe them over the tunnels. Routes to a pool are installed as ECMP nexthop groups, so an unhealthy path can be deprioritized without deleting the route or waiting for a routing protocol to reconverge. Health settings can be tuned per site, per peering, per pool, and per pool peering. When several objects govern the same peer, the agent resolves them in a fixed order, and an explicitly disabled profile blocks every fallback.

The agent publishes status continuously. Controllers and clients can run in a summary mode, where nodes publish compact status and full details are fetched on demand, or in a full mode, where the detail is part of the published status. The cache lifetime and request timeout are configurable, and the [configuration]({{< relref "reference/networking/configuration" >}}) page explains how to change them and in what order to upgrade controllers and consumers.

## Choosing a topology

There is no single topology that fits every cluster. The choice comes down to how many sites you have, how well they are connected, and whether you want to pay for gateway capacity. Three patterns cover most deployments, and they can be combined in one cluster.

A **flat mesh** relies on `SitePeering` with `meshNodes: true`. Nodes in the peered sites tunnel directly to each other, and gateway pools are bypassed for those destinations. It has the shortest path and the fewest moving parts, but the number of peer links grows with the product of the node counts. It suits a small number of sites with low-latency private connectivity, such as two regions on the same cloud backbone.

A **gateway-routed topology** assigns each site to one or more gateway pools, then peers the pools with `GatewayPoolPeering`. Workers send cross-site traffic to a local gateway, and gateways forward it to the destination site. The path is longer, but the peer count grows with the number of gateways rather than the number of workers, and the gateways centralize encryption and routing policy. This is the usual choice for many sites, for sites behind NAT, and for edge locations with constrained links.

A **hybrid** meshes some sites directly and routes the rest through gateways. For example, two data centers with a dedicated interconnect can mesh, while edge sites reach them through a shared gateway pool. The objects compose without extra configuration: a `SitePeering` covers the direct pair, and assignments plus pool peerings cover the rest. Traffic for a meshed destination does not fall back to a gateway, so you do not need to exclude those CIDRs manually.

| Pattern | Path | Peer count grows with | Use when |
|---------|------|-----------------------|----------|
| Flat mesh | Node to node, direct tunnels | Nodes times nodes in peered sites | Few sites, strong private connectivity, modest node counts |
| Gateway routed | Worker to local gateway, gateway to gateway | Gateway nodes | Many sites, NAT, edge links, centralized policy |
| Hybrid | Direct between selected sites, gateways elsewhere | Mixed | Some site pairs are close and some are remote |

Two details decide whether a pattern behaves as intended. First, a node matching a gateway pool selector leaves the site mesh, so keep gateway labels exclusive. Second, a `SitePeering` with `meshNodes: true` suppresses gateway routes for the peered sites, so adding a gateway assignment does not create a competing path. If traffic between two sites unexpectedly takes a gateway, check for a mesh peering that covers them before you debug the dataplane.

## Address planning and allocation

Address planning is the part of the model that is hardest to change later. The admission webhook enforces the invariants: `spec.nodeCidrs` must be valid CIDRs and cannot overlap across sites, and pod CIDR pools likewise cannot overlap across sites. This prevents two locations from claiming the same pod address and producing ambiguous routes. Plan both the underlay and the overlay globally, even if a site is the only one using a block today.

Pod allocation is per node, not per pod. An assignment's `cidrBlocks` are divided into per-node blocks. The IPv4 block size defaults to `/24` when IPv4 pools exist, and the IPv6 block size defaults to the first IPv6 pool prefix plus 16 bits. The webhook requires mask sizes to be consistent within a site, and it rejects an IPv6 node mask that is more than 16 bits larger than its pool prefix, so a `/64` pool can go down to `/80` per node but no further. `priority` defaults to `100`, and lower values win. `nodeRegex`, when set, restricts an assignment to nodes whose names match one of the patterns; when it is empty, no name filtering applies.

The site controller preserves existing pod CIDRs. A node that already has one keeps it across controller restarts and configuration changes, which avoids renumbering running pods. If allocation runs out of addresses in every matching assignment pool, the controller exits instead of leaving nodes with no pod network. That is deliberate: a crash loop is easier to notice and diagnose than a cluster that silently stops scheduling pods. Expand the pools and restart the controller to recover. The custom resource page documents the exact fields and validation messages, and the operations page includes the procedure for expanding a pool.

## Design principles

Five ideas explain most of the implementation choices in this stack. They are worth keeping in mind when a behavior seems surprising.

The first is that **topology is declarative**. Sites, pools, assignments, and peerings are Kubernetes objects, and the controllers reconcile the dataplane toward them. There is no separate routing configuration file to keep in sync, and no imperative command that changes a node without a corresponding object. When the objects and the dataplane disagree, the objects win after the next reconcile.

The second is that **forwarding stays in the kernel**. Routes are installed as netlink nexthop objects, and ECMP groups let the kernel spread traffic across gateways. The eBPF program is a single egress hook, not a full routing daemon. This keeps the fast path small and preserves the diagnostic tools you already use, such as `ip route`, `ip neigh`, and `tc filter`.

The third is that **shared tunnel devices scale better than per-peer devices**. The GENEVE, VXLAN, and IPIP links all use one flow-based interface each, with the destination supplied as tunnel metadata per packet. Only WireGuard needs per-port interfaces, and those are limited to the mesh port and the gateway ports. A large site does not create a device per neighbor.

The fourth is that **encryption follows the address family of the link**. Public addresses get WireGuard by default, private addresses get GENEVE. That is a safe default rather than a policy engine: an explicit WireGuard setting anywhere in the scope hierarchy forces encryption, so you can tighten a link without changing every object that touches it.

The fifth is that **health is a routing input**. A gateway path that stops working is deprioritized in the ECMP group instead of being removed, so recovery does not require a full reconvergence and a flapping path does not churn the route table. The health profile is resolved per link scope, which lets you use aggressive probing on a critical pair and conservative probing everywhere else.

## Security boundaries

WireGuard provides the encryption in this stack. Each node generates a key pair at startup if one is not already present, stores the private key with mode 0600 under the WireGuard directory, and publishes the public key in a node annotation. Peers authenticate each other by public key, and `AllowedIPs` restricts which source addresses a peer may send. There are no pre-shared keys in the default setup.

GENEVE, VXLAN, IPIP, and `None` do not encrypt. They exist for trusted internal networks where the encryption overhead would limit throughput, and `Auto` keeps links that cross external addresses on WireGuard. The security-wins hierarchy rule is the guardrail: an explicit WireGuard setting at any scope prevents a broader `Auto` or unencrypted default from downgrading that link. If your threat model includes the underlay between two sites, set `tunnelProtocol: WireGuard` on the peering or pool peering that covers them rather than relying on address classification.

Gateway nodes usually deserve isolation. Because they forward traffic for other locations, they are commonly tainted so that ordinary workloads are not scheduled on them, with DaemonSets tolerating the taint. The [operations]({{< relref "reference/networking/operations" >}}) page covers key rotation, audit logging, and the network policy considerations that go with this boundary.

## Failure behavior

The dataplane does not depend on the controller being available. If the leader dies, a follower acquires the lease and reinitializes allocation state from the nodes, and existing tunnels and routes continue to forward traffic in the meantime. Changes that require reconciliation, such as a new site or a new pool member, wait until a leader is active again.

Gateways start in an unhealthy state and must prove connectivity before they receive traffic. A gateway that fails a probe becomes degraded, and after enough consecutive failures it becomes unhealthy; its path is deprioritized in the ECMP group and traffic moves to another gateway. Recovery is symmetric: the path needs the same number of consecutive successes to be considered healthy again. This hysteresis avoids flapping when a link is marginal.

A network partition behaves as you would expect. Nodes within each site keep communicating, and each side keeps using its local gateways. Cross-site traffic fails until the partition clears, and the health checks mark the affected paths so that traffic does not keep choosing a dead gateway. The architecture page describes the state machines and the tunable thresholds in more detail.

## How this reference is organized

The five child pages are meant to be read in different ways depending on your task. The architecture page explains why the system is shaped the way it is. The custom resource page is the field-level contract you write against. The configuration page covers the knobs that live outside Kubernetes objects. The routing flows page is for understanding and debugging the dataplane. The operations page is the runbook.

### Architecture

[Architecture]({{< relref "reference/networking/architecture" >}}) describes the controller and node agent in detail, including pod CIDR assignment, site and gateway pool reconciliation, WireGuard key management, the tunnel dataplane, the security model, and the failure modes the design tolerates. Read it when you need to know which component owns a behavior, or when you are planning capacity and availability for the control plane. It also covers running with an existing CNI, which is the common choice when another plugin already owns pod sandbox networking on a site.

### Custom resources

[Custom resources]({{< relref "reference/networking/custom-resources" >}}) is the field-level reference for every networking object. It documents the two API groups, the cluster-scoped naming rules, the validating webhook and admission policies, and the status fields that controllers and agents publish. Each resource has a complete manifest and a table of fields, defaults, and validation constraints. Use it when you are writing or reviewing manifests, or when you need to know which object owns a particular setting.

### Configuration

[Configuration]({{< relref "reference/networking/configuration" >}}) covers the runtime configuration that is shared by the controller and node agent, the command-line overrides, environment variables, and the defaults for CNI, WireGuard, and encapsulation. It includes MTU guidance, firewall requirements for every tunnel protocol, resource requests and limits, and tuning advice for high availability and large clusters. It also documents the status publication modes and the detail request API, which matter when you operate more than a few hundred nodes.

### Routing flows

[Routing flows]({{< relref "reference/networking/routing-flows" >}}) follows packets through the dataplane. It explains the single TC egress program, the tunnel map, the interface layout, intra-site GENEVE delivery, cross-site WireGuard forwarding through gateways, and the symmetric return path. Read it before you look at `unroute` output or try to interpret a BPF map dump, because the map keys and the interface names only make sense once you know which hop you are inspecting.

### Operations

[Operations]({{< relref "reference/networking/operations" >}}) is the day-two companion. It covers prerequisites, installation, the dashboard and health endpoints, Prometheus metrics, tunnel and route inspection, migration from Cilium, common failure symptoms, diagnostic commands, and procedures for adding or removing sites, replacing gateways, expanding CIDR pools, and rolling restarts. It also documents backup and recovery, key rotation, and network policy considerations.

## Finding the right page

The table below maps common tasks to the page that answers them first. Most tasks then send you to one of the other pages for detail, which is expected: the reference is split by concern rather than by workflow.

| Task | Start with |
|------|------------|
| Understand how controllers and agents split responsibility | [Architecture]({{< relref "reference/networking/architecture" >}}) |
| Write a `Site`, `GatewayPool`, or peering manifest | [Custom resources]({{< relref "reference/networking/custom-resources" >}}) |
| Decide between a direct mesh and gateway routing | The networking model above and [Custom resources]({{< relref "reference/networking/custom-resources" >}}) |
| Change encapsulation, MTU, or status detail behavior | [Configuration]({{< relref "reference/networking/configuration" >}}) |
| Understand why a packet uses a specific tunnel | [Routing flows]({{< relref "reference/networking/routing-flows" >}}) |
| Install the stack, monitor it, or recover from a failure | [Operations]({{< relref "reference/networking/operations" >}}) |
| Look up the CLI that inspects nodes | [CLI reference]({{< relref "reference/cli" >}}) |
| Understand the problem the overlay solves | [Networking concepts]({{< relref "concepts/networking" >}}) |

## Terminology

The pages use a small vocabulary consistently. The entries below are the terms that cause the most confusion when you move between the resource reference and the packet-level pages.

| Term | Meaning |
|------|---------|
| Site | A network location and its address plan. Nodes join a Site by matching one of their internal IPs against `spec.nodeCidrs`. |
| Node CIDR | A CIDR block of node underlay addresses that identifies a Site. It is not a pod network. |
| Pod CIDR | A block of pod addresses allocated to a node from a Site's `podCidrAssignments`. |
| Gateway node | A node selected by a `GatewayPool` that forwards traffic between locations. |
| Gateway pool | A labeled group of gateway nodes with a shared type, protocol profile, and optional routed CIDRs. |
| Assignment | A `SiteGatewayPoolAssignment`, which says which pools serve which sites. |
| Peering | A `SitePeering` or `GatewayPoolPeering`, which permits traffic between sites or pools and decides whether nodes mesh directly. |
| Mesh | Direct node-to-node tunnels between sites in a `SitePeering` with `meshNodes: true`. |
| Network-peered | Sites that can reach each other privately without a node mesh. Gateway links between them use internal IPs. |
| Link scope | The object that governs a link and therefore its protocol and MTU. |
| Supernet route | A route for a remote pod CIDR installed on `unbounded0` so that the eBPF program can encapsulate it. |
| Health endpoint | An address derived from a gateway node's pod CIDR that peers probe to decide whether the path is usable. |

## Assumptions and API stability

The stack assumes Linux nodes with eBPF and TC support, a container runtime with CNI support, and UDP connectivity between the addresses that tunnels use. Encrypted links require the WireGuard kernel module. If you use GENEVE, VXLAN, or IPIP, the corresponding protocol must be permitted by the firewalls and security groups between sites; IPIP is an IP protocol, not a UDP port, and is blocked by some cloud networks by default. The [configuration]({{< relref "reference/networking/configuration" >}}) page lists the ports and the per-protocol firewall rules.

Kubernetes 1.24 or later is the supported baseline in the operations material. The stack can run with an external CNI when `manageCniPlugin` is disabled, in which case it handles only inter-node and inter-site traffic and leaves pod sandbox setup to the other plugin. That mode changes what the node agent writes and which peers it meshes, so read the architecture page's section on existing CNIs before enabling it on a cluster that already has a CNI.

The `Site` resource lives in the `unbounded-cloud.io/v1alpha3` group, which it shares with Machina and Metalman. The other networking resources live in `net.unbounded-cloud.io/v1alpha1`. All of them are cluster-scoped, and both groups are alpha APIs with a single served version and no conversion webhook. Fields can change between releases. Upgrade the CRDs before you upgrade the controller or node agents, so that status fields added by the new controllers are preserved by the API server. A validating admission webhook checks CIDR syntax, cross-site overlap, mask consistency, regular expressions, and cross-object references; sites with active nodes cannot be deleted, and pools with active members cannot be deleted.

The stack does not replace Kubernetes Services or DNS, and it does not provide L7 policy or a service mesh. It provides L3 connectivity between pods and, when configured, between pod networks and additional routed CIDRs. Network policy enforcement, if you need it, comes from the CNI or from Kubernetes network policy support in the underlying cluster. The operations page discusses the security boundaries and key rotation in more detail.

## Related reading

If you are new to the feature, read [Networking concepts]({{< relref "concepts/networking" >}}) for the VPN model and the distinction between a tunnel overlay and a CNI overlay. Then read [Architecture]({{< relref "reference/networking/architecture" >}}) for the components and [Custom resources]({{< relref "reference/networking/custom-resources" >}}) for the objects you will create.

For deployment workflows that include networking, see [L3 connectivity]({{< relref "guides/l3-connectivity" >}}) and the [operations guide]({{< relref "guides/operations" >}}). The [CLI reference]({{< relref "reference/cli" >}}) documents the `kubectl unbounded` commands used to inspect nodes, peers, routes, and BPF state, and the [agent reference]({{< relref "reference/agent" >}}) covers the node agent that runs on the host side of the same machines.

