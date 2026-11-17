---
title: "Networking"
weight: 2
description: "How unbounded-net gives pods routable addresses and connects them across sites with encrypted tunnels, gateway pools, and an eBPF dataplane."
---

Kubernetes assumes that every pod can reach every other pod without thinking
about where the pod runs. That assumption is easy to keep inside one cluster on
one network. It becomes hard as soon as worker nodes live in different regions,
different clouds, or on-premises networks that were never designed to carry pod
traffic.

Unbounded networking, implemented by the `unbounded-net` components, keeps the
assumption intact. It gives every node a pod CIDR that does not collide with any
other site, programs routes for those CIDRs on every node, and encapsulates or
routes packets according to the path between the two endpoints. You keep using
normal Kubernetes pod IPs and normal Services. You do not run a routing daemon,
peer BGP sessions, or hand out address ranges in a spreadsheet.

This page explains the problem unbounded-net solves, the split between its
control plane and data plane, the resources you use to describe sites and
gateways, how tunnel protocols are selected, and what happens to a packet from
one pod to another. Read it before you configure a multi-site cluster, then use
the networking reference pages for exact fields and commands.

![Diagram showing pods on worker nodes in two sites connected through gateway nodes, with an overlay network and a controller providing address and route configuration](../../img/networking-architecture-overview.svg)

## Why Multi-Site Networking?

A single-site cluster can rely on the underlay. Nodes share a subnet, pod CIDRs
fit inside a private range, and the CNI plugin owns the only routing decision
that matters. Multi-site clusters break each of those assumptions at once.

### NAT and firewall traversal

Worker nodes frequently sit behind NAT or a restrictive firewall. They can make
outbound connections, but nothing on the public internet can dial them. Yet pod
traffic must flow in both directions between sites. Unbounded solves this by
using WireGuard interfaces that nodes establish outbound, and by placing
gateway nodes with reachable addresses at the edge of each site. A node behind
NAT keeps a tunnel open to its gateway. The gateway is the stable address that
other sites can reach.

This is also why the design does not require inbound holes for every worker. The
default WireGuard mesh port is 51820, and gateway ports are allocated from 51821
upward, so the set of ports you must allow is small and predictable. When a peer
has no dialable endpoint, the node agent can leave the endpoint unset and let
WireGuard learn the peer address from traffic, which is how gateway nodes accept
connections from many remote peers without tracking every worker address.

### Encryption over the public internet

Traffic that leaves a site can cross networks you do not control. Unbounded
encrypts that traffic with WireGuard, using per-node key pairs. The node agent
generates a key pair if one is not already present, stores the private key with
owner-only permissions, and publishes the public key on the Node object so peers
can authenticate it. Links that never leave a trusted internal network can use
an unencrypted encapsulation instead, which avoids the cryptographic overhead on
high-bandwidth links.

The important consequence is that encryption follows the path, not the cluster.
A link that uses only internal IPs is treated as private. A link that uses a
public IP is treated as public and resolves to WireGuard by default.

### Efficient routing without bottlenecks

Sending every cross-site packet through one central appliance creates a
bottleneck and a single point of failure. Unbounded instead lets you describe
where gateways are and balances traffic across them with ECMP. Sites that have a
fast, direct network path can peer with each other and skip gateways entirely.
The result is that traffic takes the shortest path your topology allows, while
still having a gateway path as a fallback for sites that are not peered.

### Automatic CIDR allocation

You declare address pools per site and the controller does the bookkeeping. It
matches a node to a site, selects the applicable assignment rule, allocates a
node-sized block from the pool, and writes the result to the Node's pod CIDRs.
When a node is deleted, its block is released. If a pool runs dry the controller
exits with a fatal error rather than silently leaving a node without addresses,
because a node without a pod CIDR cannot run pods correctly.

### Pod IP collision avoidance

Two sites that both use `10.244.0.0/16` cannot route to each other without
translation. Unbounded prevents the situation at admission time: the validating
webhook rejects overlapping node CIDRs and overlapping pod CIDR assignment pools
across sites, along with inconsistent node block sizes and IPv6 blocks that are
too large for their pool. You find the mistake when you create the Site, not when
traffic mysteriously disappears weeks later.

### CNI functionality and multi-site pod routing

Each node runs a CNI bridge and receives routes for remote pod CIDRs. Locally,
the bridge moves traffic between pods and the host. Remotely, routes point at the
overlay device so the dataplane can select a tunnel. The node agent writes the
CNI configuration for you by default. If your clusters already run Cilium,
Calico, or another CNI, you can set `manageCniPlugin: false` and let unbounded-net
handle only the cross-site path through gateways.

## Architecture

Unbounded networking separates the part that decides from the part that forwards.
The control plane runs as a Kubernetes Deployment named
`unbounded-net-controller`. The data plane runs as a DaemonSet named
`unbounded-net-node` on every node, including control plane nodes. The controller
watches Nodes and the networking custom resources, then writes desired state:
site labels, pod CIDRs, slices, gateway pool membership, and peer status. The
node agents read that state and program the kernel.

That split matters when something goes wrong. A controller outage does not stop
existing tunnels or routes; nodes keep forwarding with the last state they
received. A node agent restart does not reassign addresses; the controller's
allocation is already recorded on the Node. You can reason about each layer
independently.

### Controller

The controller is a leader-elected Deployment. Only the leader reconciles. Its
work falls into four areas:

- **Site membership.** It matches each Node's internal IP addresses against each
  Site's `spec.nodeCidrs`, applies the canonical `unbounded-cloud.io/site` label,
  and removes the label when a node no longer matches.
- **Pod CIDR allocation.** It allocates a per-node pod CIDR from the pools in
  `Site.spec.podCidrAssignments`, using priority and node name regex to choose a
  rule. It performs a fresh API read before allocating so a stale cache cannot
  produce a double allocation.
- **Slice publication.** It groups the nodes of a Site into `SiteNodeSlice`
  objects of at most 500 nodes each, named `<site>-<index>`, with an owner
  reference so they are garbage collected with the Site. The cap keeps the
  objects below etcd size limits, and the slices give node agents a compact way
  to discover their mesh peers.
- **Gateway pool status.** It resolves each pool's node selector, checks that a
  node has the annotations and addresses the pool type requires, assigns the
  gateway WireGuard port, computes health endpoint addresses, and publishes the
  result in `GatewayPool.status`.

The controller also serves admission and status APIs for the web dashboard, but
those do not affect forwarding. The forwarding contract is the set of custom
resources and Node fields described on this page.

### Node Agent

The node agent is the only component that changes the kernel. On startup it
loads or generates its WireGuard key pair, publishes the public key on the Node,
and waits until the Node has pod CIDRs. It then writes the CNI configuration
atomically, watches Sites, SiteNodeSlices, GatewayPools, peerings, and
assignments, and reconciles four kinds of state:

- **CNI and bridge configuration**, unless the Site disables CNI management.
- **WireGuard interfaces and peers**, including the mesh interface and one
  interface per gateway link.
- **Tunnel interfaces and eBPF maps**, which hold the overlay prefix to tunnel
  endpoint mappings used by the dataplane.
- **Kernel routes and policy rules**, including ECMP nexthop groups for gateway
  paths and per-interface routing tables for symmetric replies.

The agent also runs UDP health probes over the tunnels. Probes default to a 15
second transmit and receive interval with a detect multiplier of 3. When a
gateway stops answering, the agent raises the metric on routes through that
gateway instead of deleting them, so traffic moves to a healthy gateway and
returns when the failed one recovers. Gateway health is therefore a routing
preference, not a hard failure.

## Core Concepts

Seven custom resources describe the network. You normally author only `Site`,
`GatewayPool`, `SitePeering`, `SiteGatewayPoolAssignment`, and
`GatewayPoolPeering`. The controller owns the rest.

| Resource | API group | Purpose |
|----------|-----------|---------|
| `Site` | `unbounded-cloud.io/v1alpha3` | A location, its node address ranges, and its pod CIDR pools |
| `SiteNodeSlice` | `net.unbounded-cloud.io/v1alpha1` | A controller-managed batch of up to 500 nodes in a Site |
| `GatewayPool` | `net.unbounded-cloud.io/v1alpha1` | A labeled set of nodes that route traffic between sites |
| `GatewayPoolNode` | `net.unbounded-cloud.io/v1alpha1` | Controller-managed membership and route advertisements for one gateway |
| `SitePeering` | `net.unbounded-cloud.io/v1alpha1` | A direct relationship between two or more Sites |
| `SiteGatewayPoolAssignment` | `net.unbounded-cloud.io/v1alpha1` | Which gateway pools serve which Sites |
| `GatewayPoolPeering` | `net.unbounded-cloud.io/v1alpha1` | A gateway-to-gateway relationship between pools |

The names are also available as short names in `kubectl`, and the
[custom resources reference]({{< relref "reference/networking/custom-resources" >}})
documents every field, default, and validation rule.

### Sites

A Site is a physical or logical location: a cloud region, a datacenter, a
factory floor, or a set of racks behind one router. You define it by the address
ranges its nodes use. `spec.nodeCidrs` is required and lists the CIDR blocks that
contain node internal IPs. The controller matches a node to the Site whose ranges
contain one of its internal addresses, then labels the node with the Site name.
Nodes that match no Site are left alone, so a cluster can contain nodes that
unbounded-net does not manage.

Site membership is based on internal IPs, not on node names or cloud tags. That
choice keeps the controller independent of any provider API. It also means you
must include every range a node might use, including secondary ranges if your
provider assigns addresses from more than one block. When ranges overlap between
Sites, admission rejects the second Site, because a node could otherwise belong
to two places at once.

Pod addresses come from `spec.podCidrAssignments`. Each assignment entry
contains one or more `cidrBlocks` pools, optional `nodeBlockSizes` for IPv4 and
IPv6, optional `nodeRegex` patterns, and a `priority`. Lower priority numbers win
when more than one rule matches a node. The IPv4 node block defaults to `/24`,
and the IPv6 block defaults to the pool prefix plus 16 bits. Every node receives
one block per address family, which becomes the node's pod CIDRs and the range
the local CNI bridge hands to pods.

The controller publishes the resulting node list as `SiteNodeSlice` objects.
Each slice holds at most 500 nodes, is named after its Site and index, and is
owned by the Site. Node agents watch slices instead of building a full mesh
watch over every Node object. You should never create, edit, or delete a slice
yourself.

A Site also carries policy that applies to its local links:

- `spec.manageCniPlugin` controls whether the agent writes CNI configuration and
  creates same-site WireGuard peers. It defaults to `true`. Set it to `false`
  when an external CNI owns intra-site networking.
- `spec.localCidrs` lists destinations that must never be sent through a gateway,
  such as a site's own management network or a range reached by a local router.
- `spec.nonMasqueradeCIDRs` lists destinations that should keep the pod source IP
  when traffic leaves through the host default gateway.
- `spec.tunnelProtocol` and `spec.tunnelMTU` set the default encapsulation and
  MTU for same-site links.
- `spec.healthCheckSettings` tunes probes for node-to-node routes inside the
  Site.

A Site with active nodes cannot be deleted. Remove or move the nodes first, or
the admission webhook rejects the deletion. This prevents an accidental delete
from stranding workloads whose addresses and routes are still in use.

### Gateway Pools

A gateway pool is a set of nodes that act as routers between sites. You select
them with `spec.nodeSelector`, usually with a dedicated label such as
`unbounded-cloud.io/unbounded-net-gateway: "true"`. A node becomes a member only
if it matches the selector, belongs to no other pool by the controller's
preferred-pool rule, and satisfies the pool's address requirements. External
pools also require an external IP, and every gateway must have published
its WireGuard public key. A matching node without a key is held out of the pool
until the annotation appears.

A node that matches any gateway pool selector is deliberately left out of
`SiteNodeSlice` objects, even if a `SitePeering` with `meshNodes: true` covers its
Site. Other nodes reach it as a gateway peer, not as a mesh peer. If one selector
matches every node in a Site, that Site ends up with no slices and no mesh peers,
which is rarely what you want. Use a dedicated gateway label rather than a broad
selector such as `node-role.kubernetes.io/worker`.

`spec.type` chooses how peers resolve the gateway endpoint. `External`, the
default, is for gateways that other sites reach over public addresses. `Internal`
is for gateways reachable only on private addresses, such as pools connected by a
transit network or a private interconnect. The distinction changes which address
the node agent dials; it does not change the encapsulation by itself.

The controller publishes each member in `GatewayPool.status.nodes` with its
internal and external addresses, WireGuard public key, gateway WireGuard port,
pod CIDRs, and health endpoints. Health endpoints are the first address of each
pod CIDR, one per address family. Gateway ports start at 51821 and are allocated
per node, while the mesh port stays at 51820.

`SiteGatewayPoolAssignment` connects the two halves of the model. It lists the
Sites that may use a set of gateway pools. Without an assignment, a Site has no
gateway path to other Sites. The assignment is also a policy scope: its
`tunnelProtocol` controls worker-to-gateway encapsulation and overrides the
Site's setting for those links, because both ends must agree on the protocol.

When several gateways serve a Site, the node agent installs routes through all of
them as a single ECMP nexthop group. A packet from a pod in one Site to a pod in
another follows a sequence like this:

1. The pod sends to the destination pod IP, and the local bridge passes the
   packet to the host.
2. The host route for the remote pod CIDR points at a gateway nexthop.
3. The packet is encapsulated toward a gateway in the local Site.
4. The local gateway forwards it to a gateway in the remote Site, or directly to
   the remote worker when the path allows.
5. The remote gateway or worker decapsulates and delivers the packet to the
   destination pod.

Healthy gateways remain in the group; unhealthy ones have their route metric
raised so the kernel prefers the remaining paths. Because the group is an ECMP
nexthop object, you get load sharing without a userspace proxy and without
rewriting packet headers.

### Peering

Peering describes Sites that can reach each other directly, without treating
every packet as gateway transit. `SitePeering` lists at least two Sites in
`spec.sites`. `spec.enabled` defaults to `true`; setting it to `false` makes the
object behave as if it does not exist, which is a convenient way to suspend a
relationship without deleting it.

`spec.meshNodes` is the switch that changes the shape of the network:

- With the default `true`, every node in the listed Sites creates direct
  tunnels to every node in the other listed Sites, exactly as if they
  were in one Site. Gateway routing for the peered Sites' node and pod CIDRs is
  not built, and the pod CIDRs of all peered Sites are combined into the local
  route table. Traffic takes the direct path and never touches a gateway.
- With `false`, the Sites are network-peered but nodes do not mesh. Traffic still
  uses gateways, but gateway pool peerings between pools in those Sites resolve
  to internal IPs instead of external IPs, because the Sites can already reach
  each other at the network level.

Use `meshNodes: true` when the direct path is fast and the number of nodes is
modest. A full mesh grows with the square of the node count, so very large Sites
usually prefer `meshNodes: false` and let gateways carry the traffic. Use
`meshNodes: false` as well when the underlying network already provides
connectivity and you only want to tell unbounded-net that external addresses are
unnecessary.

`GatewayPoolPeering` does the same job one level down: it declares that two
gateway pools can exchange traffic directly. The controller uses it to build
routes between pools and to resolve which pool addresses to use. Pools that are
not peered can still be reachable transitively through another pool, which is
why `GatewayPool.status` distinguishes `connectedSites` from `reachableSites`.

Health check settings follow the most specific scope that governs a link. A
`Site` profile covers node-to-node routes inside that Site. A `SitePeering`
profile covers routes between the Sites it lists. A `GatewayPool` profile covers
same-pool gateway peers, and a `GatewayPoolPeering` profile covers routes between
the peered pools. If two peerings define conflicting settings for the same Site,
the controller processes them in name order and keeps the first profile, so the
outcome is deterministic.

### Externally Peered Sites

Some sites already share a private Layer 3 path. ExpressRoute, Direct Connect,
and hardware VPNs are common examples: the provider or your network team has
already built an interconnect that carries private addresses between the two
locations. Tunneling pod traffic across that interconnect works, but it adds
encapsulation overhead and a layer of state for a path that is already private
and already routed.

Unbounded lets you describe this case directly. Create a `SitePeering` for the
Sites, set `meshNodes: false`, and set `tunnelProtocol: None`. The peering marks
the Sites as network-peered, so the controller stops treating their addresses as
public. `None` tells the node agent to program routes without encapsulation. Pod
packets leave the node with their original headers and are forwarded by the
interconnect like any other private traffic.

There are three requirements for this to work:

1. Node internal addresses in each Site must be reachable from the other Site
   over the interconnect. The controller assumes reachability; it does not test
   it for you.
2. The interconnect must carry the pod CIDR ranges, not only the node ranges.
   Advertise the pod pools from `spec.podCidrAssignments` on both sides.
3. Gateway pools, if you use them, must be able to reach peer gateway internal
   addresses. Because the Sites are network-peered, gateway pool peering resolves
   internal IPs rather than external ones, which is exactly what the interconnect
   provides.

The outcome is that the two Sites behave like one logical network for pod
traffic. Packets are routed, not encapsulated. You still get the rest of the
model: pod CIDRs come from the controller, nodes are labeled by Site, and routes
are programmed from the same resources. The difference is only the data path.

Direct and overlay routing coexist. A Site can be directly peered with one
location over an interconnect and use WireGuard gateways to reach another. Each
link scope selects its own protocol, so a `None` peering does not downgrade the
encrypted links to other Sites. This is also why `tunnelProtocol` is a field on
each relationship rather than a cluster-wide switch.

{{< callout type="warning" >}}
`tunnelProtocol: None` and the unencrypted encapsulations provide no
confidentiality. Use them only when the underlay is private and trusted, and
confirm that the interconnect carries pod CIDRs before you rely on the path.
{{< /callout >}}

## Tunnel Types

Every link between two nodes has an encapsulation. The `tunnelProtocol` field
selects it per scope, and `Auto` lets the node agent decide from the addresses
involved. The supported values are the same on Site, GatewayPool, SitePeering,
SiteGatewayPoolAssignment, and GatewayPoolPeering.

| Value | Encapsulation | Overhead | Encryption | Typical use |
|-------|---------------|----------|------------|-------------|
| `WireGuard` | WireGuard UDP tunnel | 80 bytes worst case | Yes | Links that cross public networks |
| `GENEVE` | GENEVE over UDP | 58 bytes | No | Default for private links |
| `VXLAN` | VXLAN over UDP | About 58 bytes | No | Private links with VXLAN offload |
| `IPIP` | IP-in-IP | 20 bytes | No | Private links needing lowest overhead |
| `None` | Direct L3 routing | 0 bytes | No | Externally routed interconnects |
| `Auto` | Resolved per link | Varies | Varies | Default when unset |

### WireGuard

WireGuard is the only encrypted option. It gives you authenticated, encrypted
tunnels with a small configured state per peer and no cipher negotiation. The
node agent creates one interface for the mesh on port 51820 and additional
interfaces for gateway links on ports allocated from 51821 upward. Each peer's
`AllowedIPs` restricts which source addresses it may send, and the public key
published on the Node identifies it.

WireGuard is the right choice whenever a packet crosses a boundary you do not
control. The cost is the 80-byte worst-case overhead and the CPU spent on
encryption. On very high bandwidth private links that cost can limit throughput,
which is why the unencrypted options exist.

### GENEVE

GENEVE is the default for links that use only internal IPs. It carries a virtual
network identifier per packet, which lets one shared `geneve0` interface serve
every peer instead of creating an interface per peer. The agent uses UDP port
6081 unless configured otherwise. Overhead is 58 bytes.

GENEVE is a good default because it is flexible and widely supported, and because
the shared flow-based interface keeps kernel object counts flat as the mesh
grows. It provides no encryption and no authentication, so it belongs on trusted
networks.

### VXLAN

VXLAN is functionally similar to GENEVE for this use case, with roughly the same
overhead. It uses a single flow-based `vxlan0` interface. Choose it when your
NICs or virtual switches have better VXLAN hardware offload than GENEVE, or when
your environment already standardizes on VXLAN. Like GENEVE, it is unencrypted
and should stay on private links.

### IPIP

IPIP wraps an IP packet inside another IP header with the least overhead of the
encapsulating options, 20 bytes. It uses one shared `ipip0` interface. It is the
leanest way to tunnel over a private network when you do not need the metadata
that GENEVE or VXLAN provide. It offers no encryption and no tenant separation
beyond the addresses themselves.

### None

`None` disables encapsulation. The node agent programs routes to the remote pod
CIDRs through the normal underlay next hop. This is the correct choice for
externally peered Sites, where an interconnect already routes private addresses.
It has zero encapsulation overhead and zero cryptographic protection. It also
requires real L3 reachability; if the underlay cannot route the pod CIDR, packets
are dropped rather than tunneled.

### Selecting a protocol per scope

Protocol selection is hierarchical because each kind of link has a different
governing object. The agent resolves the protocol for a link from exactly one
scope:

| Link | Governing resource |
|------|--------------------|
| Same-site mesh peer | `Site` |
| Node in a peered Site | `SitePeering` |
| Worker to gateway pool | `SiteGatewayPoolAssignment` |
| Same-pool gateway peer | `GatewayPool` |
| Gateway pool to gateway pool | `GatewayPoolPeering` |

When the governing value is `Auto` or unset, the agent checks whether the link
uses external addresses. If it does, the link resolves to WireGuard. If it uses
only internal addresses, it resolves to the preferred private encapsulation,
which defaults to GENEVE and can be changed with the node agent's
`--preferred-private-encap` flag. The public default can be changed with
`--preferred-public-encap`, but doing so removes encryption from public links and
the agent warns at startup.

This is the security-wins rule in practice: an `Auto` link over public addresses
does not silently become an unencrypted tunnel. You have to opt out explicitly.
The `SiteGatewayPoolAssignment` scope exists because worker-to-gateway links have
two ends that must agree. A worker resolves the link through its assignment, and
a gateway resolving the same link to a same-site mesh peer uses the same
assignment scope, falling back to the Site only when the assignment is `Auto` or
unset. Without that shared scope the two ends could pick different protocols and
the link would fail.

`tunnelMTU` sets the MTU used on routes in a scope, with a range of 576 to 9000
bytes. Leave it unset unless you know the underlay MTU and need to override the
derived value. Too high an MTU causes fragmentation or black holes after
encapsulation; too low a value wastes bandwidth. Gateways also publish their
effective forwarding MTU in `GatewayPoolNode.status.forwardingMTU`, and workers
apply the value published by their next-hop gateway and by candidate gateways on
accepted paths, including shared-interface limits.

## Dataplane

The dataplane is deliberately small. Instead of creating a tunnel interface per
peer, the node agent creates one dummy interface, attaches one eBPF program, and
shares one flow-based interface per encapsulation type. The kernel object count
stays roughly constant as the mesh grows, which is what makes large multi-site
clusters practical.

### The unbounded0 anchor

`unbounded0` is a dummy interface with the `NOARP` flag. It holds the overlay
addresses assigned to the node and a set of supernet routes covering every
remote pod CIDR that should leave the node. A `clsact` qdisc is attached to it so
the TC egress program can run on every packet that the kernel routes toward the
overlay.

Dummy interfaces do not transmit. The packet reaches `unbounded0`, the eBPF
program inspects it, and then redirects it to a real tunnel interface. Using a
single anchor keeps routing policy in one place: the kernel chooses the overlay
device, and BPF chooses the tunnel.

### The TC egress program

The `unbounded_encap` program in `bpf/unbounded_encap.c` runs as a TC egress
classifier on `unbounded0`. For each packet it:

1. Reads the destination address from the IPv4 or IPv6 header.
2. Builds a 16-byte longest-prefix-match key. IPv4 addresses are stored in
   IPv4-mapped IPv6 form, `::ffff:<v4>`, so both families share one trie. The
   prefix length for IPv4 is offset by 96 so longest-prefix semantics stay
   correct.
3. Looks up the destination in the `unb_endpts` LPM trie.
4. Chooses a nexthop. A single-nexthop entry is used unconditionally. A
   multi-nexthop entry is an ECMP set, and the program hashes the packet to pick
   among nexthops marked healthy.
5. If the chosen protocol needs metadata, calls `bpf_skb_set_tunnel_key()` with
   the remote endpoint and VNI. GENEVE, VXLAN, and IPIP need it; WireGuard and
   `None` do not.
6. Derives a deterministic inner destination MAC from the destination address
   and rewrites the Ethernet header, so no ARP or neighbor discovery is needed on
   the tunnel interface.
7. Redirects the packet to the chosen tunnel interface with `bpf_redirect()`.

The map is a `BPF_MAP_TYPE_LPM_TRIE` named `unb_endpts`, with no preallocation
and a default maximum of 16384 entries. The key is 20 bytes: a prefix length plus
a 16-byte address. Each value holds an array of nexthops and a count, so one
prefix can point at several gateways. Each nexthop carries the remote underlay
address, VNI, interface index, a healthy flag, and a protocol number. IPv6
underlays are handled by selecting the IPv6 tunnel key form, so a v4 overlay can
ride a v6 underlay and the reverse.

The `healthy` flag is only consulted for multi-nexthop entries. A single-nexthop
entry forwards unconditionally, which lets an operator drain a bad gateway
without dropping traffic to a peer that has only one path. The flag is how health
probing turns into load balancing instead of a hard route withdrawal.

### Shared tunnel interfaces

`geneve0`, `vxlan0`, and `ipip0` are flow-based interfaces. The BPF program sets
the remote endpoint and VNI metadata per packet, and the kernel encapsulation
code builds the outer header from that metadata. One interface serves every peer
using that protocol.

WireGuard is different. It is not flow-based and does not accept a per-packet
remote endpoint from BPF. The program performs a redirect only, and the
WireGuard driver encrypts and routes according to the peer's `AllowedIPs`. There
may be several WireGuard interfaces, one per port or gateway link, but there is
still no interface per peer.

Reverse path filtering must be disabled on the tunnel interfaces. A decapsulated
packet arrives on `geneve0` while the overlay route points at `unbounded0`, so
strict reverse path filtering would classify valid traffic as spoofed. The node
agent writes `0` to the relevant `rp_filter` settings, and the node initialization
also sets the system defaults so newly created interfaces inherit the value.

### Observing the dataplane

The `unroute` command dumps and queries the `unb_endpts` trie. It can show every
prefix and nexthop, look up a specific address to see which path would be chosen,
filter to IPv4 or IPv6, print raw key and value bytes, and emit JSON. Use it when
a route exists but traffic does not arrive, or when you need to confirm that the
agent pushed the entry you expected.

## The Packet Path

The path a packet takes depends on whether the destination is in the same Site
and whether the two Sites are peered.

### Same-site delivery

For two pods on different nodes in one Site using the default private
encapsulation:

1. The source pod sends to the destination pod IP. The packet crosses the pod
   veth and arrives on the node's CNI bridge, `cbr0` by default.
2. The kernel looks up the destination. A supernet route sends the remote pod
   CIDR to `unbounded0`.
3. The TC egress program matches the destination in the LPM trie and finds the
   remote node's underlay address and the shared tunnel interface.
4. The program sets the tunnel key, rewrites the inner MAC, and redirects to
   `geneve0`.
5. The kernel encapsulates the frame in GENEVE and sends it over the underlay.
6. The destination node decapsulates and routes the inner packet. The local
   route for the destination pod, which is assigned to that node, sends it to the
   bridge.
7. The bridge delivers the packet to the destination pod.

When the two nodes share a private network, the underlay is the node network. No
gateway is involved.

### Cross-site delivery

For two pods in Sites that are not peered and have no direct node mesh:

1. The source pod follows the same first steps, but the LPM lookup resolves the
   destination CIDR to a gateway nexthop instead of a direct peer.
2. The packet is encapsulated and sent to a local gateway. That gateway decrypts
   or decapsulates it, then forwards it based on its own routes.
3. On a WireGuard path, the local gateway encrypts the packet again toward the
   remote gateway. The remote gateway decrypts it and forwards toward the
   destination node.
4. The final hop follows the same-site path: the remote gateway or node
   encapsulates to the destination node if it is not directly attached.
5. The destination node decapsulates and delivers to the pod.

The return path uses the same gateways. To keep replies symmetric, the node
agent installs policy rules and per-interface routing tables so traffic that
belongs to a gateway link is not sent back through a different gateway or
directly to the peer. Without that, stateful middleboxes and asymmetric routing
would drop return packets even though the forward path worked.

{{< callout type="note" >}}
A gateway is a router, not a proxy. The packet keeps the original pod source and
destination addresses across every hop. Only the outer encapsulation changes.
This is what allows NetworkPolicy, conntrack, and service load balancing to work
the same way across Sites as they do inside one.
{{< /callout >}}

## How It Relates to unbounded-kube

Unbounded fills two separate roles. Machina and metalman provision machines and
turn them into Kubernetes nodes. Unbounded-net connects the pods that those nodes
run. The two roles share a control plane and an installation flow but not a data
path.

The API groups reflect that separation. The promoted `Site` type lives in
`unbounded-cloud.io/v1alpha3` alongside the machine APIs, because a Site is the
unit that both provisioning and networking reason about. The networking
resources, including `GatewayPool`, `SitePeering`, and the slice types, live in
`net.unbounded-cloud.io/v1alpha1`.

You bootstrap the pieces in order:

1. `kubectl unbounded install` installs the CRDs and the `unbounded-operator`.
   The operator owns CRD lifecycle and reconciles Sites.
2. `kubectl unbounded site init` creates the Site resources and bootstrap
   credentials. It refuses to run if the API types the operator serves are
   missing, so install comes first.
3. The operator deploys `unbounded-net` because networking is a required cluster
   singleton whenever at least one Site exists. You do not enable it per Site;
   `Site.spec.components` controls optional components such as metalman, gantry,
   and the token refresher.

A minimal initialization with example ranges looks like this. Replace the ranges
with ones that match your environment; all four are required and must be valid
IPv4 CIDRs.

```bash
kubectl unbounded install
kubectl unbounded site init \
  --name site-west \
  --cluster-node-cidr 10.0.0.0/16 \
  --cluster-pod-cidr 100.64.0.0/16 \
  --node-cidr 10.1.0.0/16 \
  --pod-cidr 100.65.0.0/16
```

After the Site exists, add gateway pools and peerings with `kubectl apply`. The
controller picks up the new objects, labels matching nodes, and the node agents
reconcile routes and tunnels without a restart.

## Next Steps

- Follow the [getting started guide]({{< relref "guides/getting-started" >}}) to
  install Unbounded and create your first Site.
- Read [L3 connectivity]({{< relref "guides/l3-connectivity" >}}) when your Sites
  already have a routed path and you want to use it directly.
- Use the [networking reference]({{< relref "reference/networking" >}}) for the
  full resource and configuration surface.
- See the [networking architecture reference]({{< relref "reference/networking/architecture" >}})
  for component details and internal state.
- Review the [configuration reference]({{< relref "reference/networking/configuration" >}})
  for runtime flags, MTU guidance, and encapsulation settings.

