---
title: "Architecture"
weight: 1
description: "How the unbounded-net controller and node agent build encrypted and unencrypted pod-to-pod networking across sites."
---

Unbounded-net connects pods across Kubernetes sites without requiring a single flat
Layer 2 network. Each node runs a small dataplane that attracts overlay traffic,
looks up the destination in an eBPF map, and redirects the packet onto a WireGuard,
GENEVE, VXLAN, or IPIP path. A cluster-wide controller assigns addresses, groups
nodes into sites, and publishes the topology that every node agent consumes.

This page explains the control plane and the data plane separately, then follows a
packet through both. It also covers the custom resources that tie the two halves
together, the lifecycle of a site, and the failure modes you should expect when a
controller, gateway, or network link goes away. Read it before the more focused
pages on [custom resources]({{< relref "reference/networking/custom-resources" >}}),
[configuration]({{< relref "reference/networking/configuration" >}}), and
[routing flows]({{< relref "reference/networking/routing-flows" >}}).

## System Overview

Unbounded-net provides pod-to-pod networking across multiple Kubernetes sites. A
site is a group of nodes whose internal IP addresses fall inside one or more CIDR
blocks declared on a `Site` object. Nodes in the same site can usually reach each
other over a local network, while nodes in different sites need an overlay that
crosses an untrusted or routed boundary. The project treats both cases as links
with different characteristics rather than as separate products.

Encrypted links use WireGuard. Unencrypted links use GENEVE, VXLAN, or IPIP, and a
link can also be configured for direct L3 routing with no encapsulation at all.
The encapsulation choice is made per link scope, which means the same node can use
GENEVE to a peer in the same rack and WireGuard to a gateway in another cloud. A
scope can name a protocol explicitly, or it can leave the decision to `Auto`, which
resolves the protocol from whether the link uses public or private addresses.

The dataplane is deliberately small. One TC egress BPF program, `unbounded_encap`,
runs on a dummy interface named `unbounded0`. The program performs a longest-prefix
match against a single LPM trie named `unb_endpts` and redirects the packet to a
shared tunnel interface or to a per-port WireGuard interface. This design avoids
creating one tunnel device per peer, which keeps kernel resource use bounded as a
mesh grows.

### High-Level Architecture

![Diagram of the unbounded-net control plane, custom resources, and per-node dataplane](../../../img/networking-architecture-overview.svg)

The system has three tiers. The first tier is the control plane, made up of the
`unbounded-net-controller` Deployment and the Kubernetes API server. The second
tier is a set of cluster-scoped custom resources that describe sites, gateway
pools, peerings, and assignments. The third tier is the per-node data plane,
implemented by the `unbounded-net-node` DaemonSet and the kernel objects it owns.

The controller is not in the packet path. It never forwards traffic and it does not
program routes on nodes directly. Instead, it assigns pod CIDRs, labels nodes with
their site, allocates gateway ports, and writes topology objects. Node agents watch
those objects and converge the local kernel toward them. When the controller is
unavailable, an already configured node keeps forwarding traffic with the last
state it observed.

The API server is the meeting point between the tiers. Every topology change
becomes an object update, and every agent consumes updates through an informer
cache rather than polling. This makes the control plane level-triggered: a node
agent that restarts reconstructs its desired state by reading the current objects,
not by replaying a stream of events.

## Component Details

The controller and the node agent have different jobs and different failure
domains. The controller owns cluster-wide invariants such as unique pod CIDRs and
unique gateway ports. The node agent owns everything that is local to a node,
including interfaces, WireGuard peers, routes, iptables chains, and eBPF maps.

### Controller (`unbounded-net-controller`)

The controller runs as a Deployment in the `unbounded-system` namespace with one
replica by default. It uses host networking and serves health, metrics, and status
endpoints on port 9999. Leader election is enabled by default, with a 30 second
lease duration, a 15 second renew deadline, and a 10 second retry period. Running
more than one replica is supported, and only the leader reconciles resources.

The controller process starts several independent reconcilers. The Site
controller handles site membership, pod CIDR assignment, and `SiteNodeSlice`
objects. The GatewayPool controller handles gateway membership, gateway ports, and
`GatewayPoolNode` objects. The NodeAssignment controller replaces a node-agent Pod
when its acknowledged site or pool no longer matches the Node labels. The Peering
aggregation controller computes reachability and writes peering and pool status.
A managed kube-proxy controller creates DaemonSets for sites that the provider
kube-proxy does not cover.

Each reconciler follows the same pattern. It registers informer event handlers,
puts a key on a rate-limited workqueue, and processes that key in a worker. A
successful sync calls `Forget`; a failed sync calls `AddRateLimited`. Caches must
be synchronized before work begins, and the Site controller additionally seeds its
CIDR allocators from the existing Nodes before it reports itself ready. Admission
requests and node assignments are refused until that seeding completes, which
prevents a restart from handing out an address that is already in use.

The controller also runs a webhook server and an aggregated API server for node
status. It obtains a serving certificate through the Kubernetes CSR API and injects
the CA bundle into its webhook and APIService registrations. RBAC for the
controller is intentionally narrow around Nodes: a ValidatingAdmissionPolicy
limits which labels and annotations the controller may write, and limits pod CIDR
changes to the pod CIDR fields.

#### Pod CIDR Assignment

Pod CIDR assignment is a Site controller responsibility. A Node create or update
puts the node name on the workqueue. The controller looks up the node's internal IP
addresses and compares them with `spec.nodeCidrs` on every Site. The first Site
whose CIDR contains one of those addresses wins, and the node is labeled with that
site name. A node that matches no Site is left unlabeled and receives no CIDR from
unbounded-net.

When a Site matches, the controller chooses a `podCidrAssignments` entry. Lower
`priority` values win, and an omitted priority defaults to 100. If an entry has
`nodeRegex`, the node name must match at least one pattern. When two entries have
the same priority, the earlier entry in the list wins. An entry with no regex
matches every node in the site. The selected assignment must also be enabled: an
explicit `assignmentEnabled` value wins, and an omitted value inherits the Site's
`manageCniPlugin` setting.

Allocation is deliberately careful about races. Before allocating, the controller
reads the Node from the API server rather than trusting the informer cache. A key
can be requeued while an earlier sync is still patching the same Node, and the
fresh read lets the second sync observe the first sync's result instead of
allocating a second block. If the Node already has pod CIDRs, the controller marks
them as allocated in the matching allocator and returns without patching.

The allocator draws from `cidrBlocks` using the sizes in `nodeBlockSizes`. When
`nodeBlockSizes` is omitted, IPv4 allocations default to a `/24` and IPv6
allocations default to the first IPv6 pool prefix plus 16 bits. IPv4 and IPv6 are
allocated independently, so a dual-stack site gets both a v4 and a v6 block per
node. The controller writes `spec.podCIDR` and `spec.podCIDRs` in a single patch,
together with the site labels when the node is being labeled for the first time.

Pool exhaustion is fatal. If no block remains in any matching pool, the controller
logs the site and assignment and exits. This is intentional: continuing would leave
nodes permanently unassigned while the controller looks healthy. The remedy is to
add address space to the Site or remove nodes, not to wait for a retry.

Node deletion moves the node's CIDRs into a pending release map and processes the
release through the workqueue. This avoids a race between the delete event and a
concurrent sync that still holds the old Node object. The Site controller also
performs a periodic duplicate CIDR audit across all Nodes and logs any address
claimed by more than one node.

#### Site Controller

The Site controller turns site membership into two durable artifacts: Node labels
and `SiteNodeSlice` objects. It writes both the canonical
`unbounded-cloud.io/site` label and the deprecated
`net.unbounded-cloud.io/site` label during the deprecation window. Readers prefer
the canonical key and fall back to the deprecated one, so a node labeled by an
older controller still resolves correctly.

Slices exist because a site can contain thousands of nodes, and putting every node
into one object would approach etcd object size limits. The controller sorts nodes
by name, splits them into groups of at most 500, and names the slices
`<site-name>-<index>`, starting at index 0. Each slice records the node name, its
WireGuard public key, its internal IPs, and its pod CIDRs. The slice carries an
OwnerReference to the Site, so deleting the Site garbage-collects the slices.

Slice reconciliation is dirty-flag driven rather than event driven. Node and slice
event handlers mark specific sites dirty, and a five second loop rebuilds only the
affected sites. The loop also deletes slices whose index is no longer needed after
re-reading the live object, which prevents a stale informer entry from deleting a
slice that has been reassigned. A separate low-frequency repair pass looks for
orphaned slices whose Site no longer exists.

The controller updates `Site.status.nodeCount` and `Site.status.sliceCount` as it
reconciles. It also manages the `net.unbounded-cloud.io/protection` finalizer. The
finalizer is added when a Site has assigned nodes and removed when the last node
is gone, so a Site with active nodes cannot be deleted out from under them.

#### GatewayPool Controller

A `GatewayPool` selects gateway nodes with a label selector. The controller lists
all Nodes, applies the selector, and then filters the result. A node must have a
WireGuard public key annotation to join any pool. A pool with `type: External`
also requires at least one external IP. A pool with `type: Internal` accepts nodes
with internal or external addresses, and cross-site links to an internal pool are
expected to run over a private network.

A node belongs to at most one pool. If several pools match, the controller chooses
the lexicographically first pool name and logs that the node matched multiple
pools. It writes the choice to the `net.unbounded-cloud.io/gateway-pool` Node
label. Removing the label, and any assigned port annotation, is how the controller
returns a node to the worker role.

Gateway ports start at 51821 and are globally unique across all pools. The
controller remembers assignments by node name and writes the result to the
`net.unbounded-cloud.io/wireguard-port` annotation. Uniqueness matters because each
gateway peer gets its own WireGuard interface named `wg<port>`, and the kernel
binds a WireGuard device to a UDP listen port. Two gateways sharing a port on the
same node would collide.

For each member, the feed publishes the node's name, site, internal and external
IPs, WireGuard public key, pod CIDRs, gateway port, and health endpoints. Health
endpoints are the first usable address of each pod CIDR, which is the bridge
gateway address on that node. The controller writes this information into
`GatewayPool.status.nodes`, creates or updates a `GatewayPoolNode` object for the
node, and removes `GatewayPoolNode` objects for nodes that no longer belong.

The pool controller also manages a protection finalizer. It is added while the
pool has members and removed when the pool is empty. If a member's discovered
public IP has an expiry, the controller schedules a requeue for that expiry so pool
membership is recomputed when the address can no longer be trusted.

### Lifecycle of a Site

Creating a `Site` starts a sequence that ends with every matching node running a
converged dataplane. The sequence is level-triggered, so a failure at any step is
retried when the relevant object changes or when an informer resyncs.

1. The API server admits the Site through the validating webhook. The webhook
   checks that `nodeCidrs` are valid and do not overlap another Site, that every
   `cidrBlocks` pool is valid and does not overlap another Site, that each
   `nodeRegex` compiles, and that block sizes are consistent within the Site.
2. The Site controller adds the Site to its cache. Node events and the initial
   all-node reconciliation compare each Node's internal IPs with `nodeCidrs` and
   select the assignment entry that applies.
3. Matching Nodes are labeled with the Site and receive pod CIDRs in the same
   patch. The Site's slices are marked dirty, and the five second loop creates the
   `SiteNodeSlice` objects with the node keys, internal IPs, and pod CIDRs.
4. Node agents observe their Site label, acknowledge the effective assignment on
   their Pod, and either write CNI configuration or switch to external-CNI mode.
   They then read the slices, build WireGuard and tunnel peers, program routes and
   BPF entries, and start health checks.
5. Gateway pools, `SiteGatewayPoolAssignment`, `SitePeering`, and
   `GatewayPoolPeering` objects connect the Site to other locations. Workers use
   the gateways assigned to their Site, and gateways learn reachable CIDRs from
   the objects that describe the path between pools.
6. When a Node moves to a different Site, the controller changes its labels. The
   NodeAssignment controller deletes the old agent Pod once its acknowledged
   assignment no longer matches, and the replacement agent adopts the new Site and
   rebuilds its local state for that Site. An agent that is still starting also
   exits when it notices that its adopted assignment has changed.
7. Deleting a Site is blocked by the protection finalizer while it still has
   nodes. After the nodes are moved or removed, the finalizer is cleared, the
   Site's slices are garbage-collected through their OwnerReferences, and the
   Site can be deleted.

### Node Agent (`unbounded-net-node`)

The node agent runs as a DaemonSet on every node, including control plane nodes.
The Pod uses the host network and host PID namespaces and runs privileged,
because it must create links, change addresses, write sysctls, and attach BPF
programs in the host kernel. It tolerates every taint and uses the
`system-node-critical` priority class, since pod networking depends on it.

The agent has a wider set of responsibilities than its name suggests. It manages
WireGuard interfaces and keys, configures tunnel peers, loads the eBPF program and
reconciles its maps, programs routes, writes CNI configuration, monitors gateway
health, maintains iptables chains for forwarding and masquerading, and publishes
node status. It also annotates the Node with its WireGuard public key and maximum
tunnel MTU so the controller can use both when building pools and slices.

Most of the agent's state is local. It keeps manager objects for links, routes,
WireGuard, BPF maps, masquerade rules, MSS clamping, notrack rules, and gateway
policy routing. Each manager applies a desired state differentially: missing
objects are added, changed objects are updated, and objects that are no longer
desired are removed. This keeps reconciliation from tearing down a healthy
WireGuard session or flushing a route table every time an unrelated object
changes.

#### Initialization Flow

Startup is ordered so that identity exists before topology is consumed, and so
that CNI configuration exists before pods are scheduled onto the node.

1. The agent validates its IPAM settings, installs the bundled `unbounded-ipam`
   binary into the host CNI binary directory, and starts the IPAM Unix socket
   server that the CNI plugin calls.
2. It loads or generates the WireGuard key pair, then patches the Node with the
   public key annotation. A failure here is fatal, because a node without a
   published key cannot join a mesh or a gateway pool.
3. It creates informers for Sites, SiteNodeSlices, SitePeerings, and
   SiteGatewayPoolAssignments, and starts the health server if one is configured.
4. It waits until the Node has been adopted into a Site and, optionally, a
   GatewayPool. Adoption is recorded on the node-agent Pod with the
   `net.unbounded-cloud.io/effective-site` and
   `net.unbounded-cloud.io/effective-gateway-pool` annotations. An empty pool
   annotation means the worker role, and a missing annotation means the
   acknowledgment is still pending.
5. It detects the default route MTU, annotates the Node with the maximum tunnel
   MTU it can support, and starts optional public IP discovery. Discovery can use
   a declared address, a provider external IP, or STUN when enabled.
6. It starts the informers, waits for their caches to synchronize, and starts the
   netlink cache that supplies read-only kernel snapshots to the managers.
7. If the Site manages the CNI plugin, the agent waits for pod CIDRs and writes
   the CNI configuration atomically. If the Site does not manage CNI, the agent
   skips the write but still reads the Node's pod CIDRs for tunnel addressing.
8. It enters the long-running Site watch, which reconciles WireGuard interfaces,
   peers, routes, BPF entries, health checks, and status until the process exits.

Adoption has a lifecycle of its own. The controller watches node-agent Pods and
compares their acknowledged annotations with the desired Node labels. If a node is
moved to a different Site or GatewayPool, the controller deletes the old agent Pod
and lets the DaemonSet create a replacement. The agent also watches its own Node
object during startup and exits if the adopted assignment changes, which prevents
a Pod from continuing to program the wrong site's topology.

#### WireGuard Interface Architecture

WireGuard interfaces are named by listen port. The main mesh interface is
`wg51820`, because the default WireGuard port is 51820. It carries same-site mesh
peers, peers in sites that are directly peered with mesh enabled, and same-pool
gateway peers. All peers on this interface share one kernel device, and WireGuard
selects the peer by public key and restricts source addresses with `AllowedIPs`.

A gateway node also listens on its assigned gateway port, which is at least 51821.
That port exists so other gateways can build a dedicated link to this gateway
without sharing the mesh device. A node that uses a gateway creates one interface
per gateway peer, named `wg<gateway-port>`, and puts exactly one peer on each
interface. Separate interfaces give the route manager a distinct link index for
each gateway, which is what allows kernel ECMP and per-gateway health handling.

Endpoint selection follows the relationship between the two sites. A peer in the
same site uses an internal IP. A peer in a network-peered site also uses an
internal IP, because the sites can already reach each other. A gateway in an
external pool at a different site uses the peer's public or external address. A
gateway in an internal pool at a different, non-peered site has no endpoint at
all, and WireGuard learns the reachable endpoint from incoming handshake traffic.

Gateway-to-gateway links use a crossover port rule. The endpoint port is the local
gateway's own assigned port, not the remote port, because the remote gateway's
interface for this peer listens on the port this gateway advertises. Non-gateway
nodes are simpler: they send to the gateway's standard mesh port, where the
gateway has a peer entry for them.

The agent does not rely on persistent keepalives for liveness. Health checks send
UDP probes over the tunnel, and the resulting up or down state is fed into the eBPF
map. This keeps probe traffic under the control of the health check settings
instead of the WireGuard driver.

## Running with an Existing CNI

Unbounded-net can coexist with Cilium, Calico, Azure CNI, or another CNI that owns
intra-site pod networking. The integration point is the Site's `manageCniPlugin`
field. When it is true, the node agent writes a CNI conflist that uses the bridge
plugin, `unbounded-ipam`, portmap, and loopback. When it is false, the agent does
not write CNI configuration and does not create same-site mesh peers.

The scope of the remaining work is cross-site connectivity. A worker still builds
tunnels to the gateways assigned to its site, using WireGuard by default for
cross-site links, and still installs routes for remote pod CIDRs and routed CIDRs.
The external CNI remains responsible for assigning pod addresses and for moving
packets between pods in the same site. The controller can still allocate pod CIDRs for the Node when a
`podCidrAssignments` entry sets `assignmentEnabled: true`, which is useful when the
external CNI reads pod CIDRs from the Node spec.

### Configuring the Primary Site

Set `manageCniPlugin: false` on the Site and make the pod CIDR assignment explicit.
The CIDR blocks are still required even when the controller does not allocate to
every node, because they define the address ranges that gateways advertise to
other sites.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Site
metadata:
  name: primary
spec:
  nodeCidrs:
    - 10.0.0.0/16
  manageCniPlugin: false
  podCidrAssignments:
    - cidrBlocks:
        - 10.244.0.0/16
        - fdde:1::/48
      assignmentEnabled: true
      nodeBlockSizes:
        ipv4: 24
```

When you initialize the site with the CLI, pass `--manage-cni-plugin=false` to
`kubectl unbounded site init` so the generated Site starts in the same mode. The
node agent reads `manageCniPlugin` only at startup. If you change it later, restart
the node agents so they switch between managed and unmanaged CNI behavior.

### Division of Responsibilities

| Responsibility | Primary cluster with an existing CNI | Remote site managed by unbounded-net |
|----------------|--------------------------------------|---------------------------------------|
| Pod IP allocation | External CNI | `unbounded-ipam` and the Site allocator |
| Intra-site pod networking | External CNI | `cbr0` bridge and the mesh tunnel |
| Cross-site routing | unbounded-net gateways | unbounded-net gateways |
| Gateway WireGuard links | Node agent | Node agent |
| Pod CIDR records on Nodes | Controller, when enabled | Controller |
| CNI configuration | External CNI | Node agent |

Cilium identities do not exist on remote nodes that run unbounded-net's own CNI, so
Cilium network policies that select pods by identity cannot enforce policy across
those sites. Use CIDR-based policy with `toCIDR` and `fromCIDR` for cross-site
traffic, and keep identity-based policy inside the cluster where Cilium runs.

### Encapsulation Types

Unbounded-net supports five tunnel protocols plus direct routing. The protocol is
selected by the `tunnelProtocol` field on the object that governs a link, and the
default is `Auto`.

| Protocol | Encapsulation | Overhead | Typical use |
|----------|---------------|----------|-------------|
| `WireGuard` | Encrypted UDP | 80 bytes | Links that cross public networks |
| `GENEVE` | Unencrypted UDP | 58 bytes | Private links, same-site default |
| `VXLAN` | Unencrypted UDP | 50 bytes | Private links with VXLAN offload |
| `IPIP` | IP-in-IP | 20 bytes | Private links with minimal overhead |
| `None` | Direct L3 routing | 0 bytes | Networks that already route pod CIDRs |
| `Auto` | Resolved per link | Varies | Default when the field is unset |

The overhead values are the constants the agent uses to derive interface and route
MTUs. WireGuard's 80 bytes covers the worst case with IPv6 outer headers. The agent
subtracts the relevant overhead from the underlay MTU, or uses a configured
`tunnelMTU` when one is set, and clamps the result to the range the API accepts.

`Auto` resolves to `GENEVE` for links that use only internal addresses and to
`WireGuard` for links that use external addresses. The private and public
preferences can be changed with the `--preferred-private-encap` and
`--preferred-public-encap` flags, whose defaults are `GENEVE` and `WireGuard`.
Choosing a non-WireGuard public preference is allowed, but the agent prints a
startup warning because traffic on public networks will then be unencrypted.

The governing object is a single scope, not a hierarchy of overrides. Same-site
mesh links are governed by the Site. Links between peered sites are governed by the
`SitePeering`. Links from a site to a gateway pool are governed by the
`SiteGatewayPoolAssignment`. Same-pool gateway links are governed by the
`GatewayPool`, and links between different pools are governed by the
`GatewayPoolPeering` when one exists. Both ends of a link must resolve the same
protocol, so gateway nodes use the assignment scope for same-site peers when that
is how the peer on the other end sees the link.

The security rule for automatic selection is simple: if a link uses external or
public addresses, `Auto` resolves to WireGuard unless the public preference was
explicitly changed. For gateway pools, a cross-site link that is not network-peered
and touches an `External` pool also requires WireGuard, even if the other pool is
`Internal`, because one end of the path is using public addresses.

## Tunnel Dataplane

The dataplane is built around one routing anchor, one TC program, and a small set
of shared tunnel devices. This section describes how those pieces fit together
before the data flow sections trace a packet through them.

### Interfaces and Routing Anchor

`unbounded0` is a dummy interface with the `NOARP` flag set. It has no real link,
and because ARP and neighbor discovery are disabled, the kernel does not try to
resolve a next-hop MAC on it. The agent assigns the gateway address of each local
pod CIDR to `unbounded0` as a host address, either `/32` for IPv4 or `/128` for
IPv6. Those addresses become the preferred source for overlay traffic.

The agent then installs one supernet route per overlay CIDR that should enter the
dataplane. A supernet can cover many peer pod CIDRs and many gateway-advertised
routed CIDRs; the route only has to attract traffic into the BPF program. The
program performs the per-destination decision, so the routing table does not need
one route per peer. Gateway underlay addresses also get host entries in the trie,
which prevents node-directed traffic from matching a broader site prefix and being
load-balanced to the wrong gateway.

Three shared tunnel devices are created on demand. `geneve0` is a flow-based GENEVE
interface with a configurable VNI and UDP port, defaulting to VNI 1 and port 6081.
`vxlan0` is a flow-based VXLAN interface with UDP port 4789 and a narrow source
port range from 47891 to 47922. `ipip0` is an IP-in-IP interface. The BPF program
sets the remote endpoint, and for GENEVE and VXLAN the VNI, through
`bpf_skb_set_tunnel_key` before redirecting to the device.

WireGuard is different. A WireGuard device is not flow-based, and the BPF program
does not set a tunnel key for it. The program only redirects the packet to the
`wg<port>` interface, and the WireGuard driver selects the peer from `AllowedIPs`
and encrypts the packet. This is why WireGuard links still need one interface per
gateway even though every other protocol shares a single device.

The agent disables reverse path filtering on tunnel and WireGuard interfaces.
Decapsulated packets arrive on `geneve0`, `vxlan0`, `ipip0`, or `wg<port>`, while
the overlay route points at `unbounded0`, so strict reverse path filtering would
drop valid traffic. The agent writes zero to the per-interface, `all`, and
`default` `rp_filter` sysctls through the host procfs mount, and reapplies the
setting after interface cleanup because the kernel can reset it when a device is
deleted.

Several iptables managers complete the forwarding path. `UNBOUNDED-FORWARD`
accepts traffic between managed tunnel interfaces without opening a blanket
forward policy. `UNBOUNDED-MASQUERADE` handles pod egress through the default
interface and honors the Site's `nonMasqueradeCIDRs`. `UNBOUNDED-MSS-CLAMP` clamps
TCP MSS to the fabric MTU so tunneled sessions do not black-hole on large packets.
On gateway nodes, `UNBOUNDED-NOTRACK` skips conntrack for transit traffic between
tunnels, while still tracking traffic destined to the node itself and to local
pods.

### TC Egress Program

`unbounded_encap` is attached as a TC egress classifier on `unbounded0` under a
`clsact` qdisc. It runs once per packet that the kernel routes to the anchor. The
program is small enough to stay within the verifier budget and does not allocate
per-peer state.

For an IPv4 packet, the program reads the destination address and writes it into a
16-byte key in IPv4-mapped IPv6 form, `::ffff:<v4>`. For an IPv6 packet, it copies
the native 16-byte destination. Non-IP packets and truncated headers return
`TC_ACT_OK`, which lets the kernel continue normal processing. A missing map entry,
or an entry with zero nexthops, also returns `TC_ACT_OK`, so traffic that the
dataplane does not own falls through instead of being dropped.

Nexthop selection depends on the entry width. An entry with exactly one nexthop
forwards unconditionally. The healthy flag is ignored in that case, because there
is no alternative path and dropping locally would only prevent the destination
from recovering through real traffic. An entry with more than one nexthop reads the
packet's flow hash with `bpf_get_hash_recalc` and runs Highest Random Weight
selection over the healthy nexthops. If every nexthop is unhealthy, the program
returns `TC_ACT_OK` and lets the kernel route decide.

Once a nexthop is chosen, the program derives a destination MAC from the underlay
endpoint and writes it into the Ethernet header. It then calls
`bpf_skb_set_tunnel_key` for GENEVE, VXLAN, and IPIP, using the IPv4 or IPv6 form
that matches the underlay address. Finally it calls `bpf_redirect` with the
nexthop's interface index. The redirect is what moves the packet from the dummy
anchor to the real tunnel or WireGuard device.

### BPF LPM Trie Map

The map is named `unb_endpts` and its type is `BPF_MAP_TYPE_LPM_TRIE` with
`BPF_F_NO_PREALLOC`. The default capacity is 16,384 entries and can be changed
with `--tunnel-dataplane-map-size`. The key is 20 bytes: a 4-byte prefix length
followed by a 16-byte address. The value is a fixed array of four nexthops plus a
count, so one prefix can carry an ECMP group without a second map.

IPv4 destinations share the trie with IPv6 destinations. The IPv4 address is
stored in IPv4-mapped form, and the prefix length is increased by 96 so the match
depth lands on the IPv4 portion of the address. A `100.80.0.0/14` overlay prefix,
for example, becomes `::ffff:100.80.0.0/110`. Native IPv6 prefixes use their normal
length. This lets one lookup cover both families and keeps the program free of a
family branch after the key is built.

Each nexthop holds a 16-byte remote endpoint, a VNI, an interface index, a healthy
flag, and a protocol identifier. The protocol values are `1` for GENEVE, `2` for
VXLAN, `3` for IPIP, `4` for WireGuard, and `5` for direct routing. The program
uses the protocol to decide whether a tunnel key is needed, so there is no separate
set-key flag. The remote endpoint uses IPv4-mapped form for IPv4 underlays and a
native address for IPv6 underlays, and the program inspects the first 12 bytes to
choose the right helper argument.

The same BPF object also defines `unb_trace`, a 256 KiB ring buffer. When a
consumer is attached, the program emits one record per packet with addresses,
ports, the matched prefix length, the chosen nexthop, and the helper return values.
When no consumer is attached, the reserve fails after the ring fills and the
program drops the trace event without affecting forwarding. The `unroute` tool
reads this map and can also trace lookups for a specific address.

### Deterministic MAC Derivation

Tunnel interfaces do not use ARP or neighbor discovery for the inner Ethernet
header. Instead, both the Go code and the BPF program derive a locally administered
MAC address from an underlay IP address. For IPv4 the address is
`02:<ip[0]>:<ip[1]>:<ip[2]>:<ip[3]>:ff`. For IPv6 it is
`02:<last four bytes>:ff`. The `02` prefix marks the address as locally
administered, and the trailing `ff` avoids collisions with ordinary unicast
addresses.

Each node configures its own underlay-derived MAC on the shared tunnel device, and
the sender derives the destination MAC from the peer's underlay endpoint. The
kernel therefore accepts the frame without an ARP request or a neighbor entry. The
formula is identical on both sides, so no coordination is needed when a peer or
gateway is added.

### BPF Map Reconciliation

The agent does not write the trie one peer at a time during reconciliation.
Instead, each peer configuration function appends entries to a pending map on the
shared state. After mesh peers, gateway peers, and WireGuard peers have all
contributed, the agent calls `Reconcile()` once with the complete desired set.

`Reconcile()` iterates the existing trie, deletes every key that is not in the
desired set, and upserts every desired key. The operation is serialized by the map
manager's mutex, and missing keys are treated as success during deletion. The
manager also keeps a copy of the last desired state, which lets a health check
transition update only the nexthops that belong to a named peer.

Health updates use `SetPeerHealth`, which walks the last reconciled state, flips
the healthy flag on matching nexthops, and writes the modified entries back to the
trie. New entries start healthy so a newly added peer is usable immediately. If
the peer then fails a health check, the flag is cleared and the nexthop is skipped
on multi-nexthop entries.

## Data Flows

The following paths assume the default `Auto` protocol resolution. An explicit
scope can change any hop, but the shape of the path stays the same: a packet is
routed to `unbounded0`, the BPF program redirects it, and the receiving node
decapsulates and routes it to the pod.

### Same-Site Pod-to-Pod (GENEVE)

A pod on node A sends a packet to a pod on node B in the same site. The pod's
default route points at the `cbr0` bridge, and the bridge forwards the packet to
the host. The host routing table matches the destination against a supernet route
whose device is `unbounded0`, so the packet enters the dummy interface.

The TC egress program reads the destination, finds the peer entry for node B, and
selects the GENEVE nexthop. It sets the remote endpoint to node B's internal IP,
sets the VNI, rewrites the destination MAC, and redirects to `geneve0`. The GENEVE
device encapsulates the packet in UDP and sends it over the local network.

Node B receives the GENEVE packet, decapsulates it, and routes the inner packet to
`cbr0`. The bridge delivers it to the destination pod. If the Site or a governing
scope selects WireGuard instead, the same path uses `wg51820` and the packet is
encrypted before it leaves node A.

### Cross-Site Pod-to-Pod (via Gateway, WireGuard)

A pod on a worker in site East sends a packet to a pod in site West. The worker has
no direct link to site West, so its route points at a gateway pool. The BPF program
selects a `wg<gateway-port>` nexthop and redirects the packet to that interface.
WireGuard encrypts it and sends it to the local gateway over the worker's internal
network or the gateway's reachable address.

The local gateway decrypts the packet. Its own routing table sends the packet to
the remote gateway, which is reachable through another WireGuard interface. The
gateway-to-gateway link uses the external addresses of an external pool, or the
private addresses of an internal pool, and the endpoint port follows the crossover
rule described earlier.

The remote gateway decrypts the packet and forwards it toward the destination
worker. In the default private configuration, that last hop uses GENEVE, because
the remote gateway and its same-site worker resolve their link through the
`SiteGatewayPoolAssignment` scope and both use private addresses. The worker
receives the tunnel packet, decapsulates it, and delivers it to the destination
pod.

Not every cross-site path uses a gateway. A `SitePeering` with `meshNodes` true,
which is the default, makes nodes in the peered sites direct mesh peers. Those
nodes build tunnels to each other and the packet does not transit a gateway. A
peering with `meshNodes: false` still marks the sites as network-peered, so
gateways can use internal addresses, but nodes do not build direct mesh tunnels to
each other and pod-to-pod traffic stays on the gateway path.

### Gateway Health Checks

The node agent registers a health check session for each mesh and gateway peer
that has a pod CIDR address to probe. Probes are UDP packets sent to the peer's
overlay address on port 9997. Each scope can override the settings: the Site, the
`SitePeering`, the `GatewayPool`, the `SiteGatewayPoolAssignment`, or the
`GatewayPoolPeering`. When a scope does not override them, the defaults are a 15
second transmit interval, a 15 second receive interval, and a detect multiplier
of 3.

A session starts down and returns to up only after it receives the required number
of consecutive replies. It moves down when a reply has not arrived within the
detection timeout, which is the multiplier times the larger of the transmit and
receive intervals. An administrative disable moves it to `AdminDown` and stops the
timeout logic. State changes are delivered to the agent through an ordered callback
channel, and the agent applies each change to the BPF map by setting or clearing
the healthy flag on the peer's nexthops.

Recovery backoff is deliberately symmetric with failure detection. A session that
has flapped must accumulate more consecutive replies before it returns to up, and
the required count grows with the number of recent flaps, up to a configurable
maximum backoff of 120 seconds. This prevents a marginal link from oscillating
between healthy and unhealthy and repeatedly reshuffling ECMP selection.

Health does not rewrite route metrics. The path state lives in the BPF nexthop
flag, and a recovering gateway is restored by setting the flag again. This avoids
route churn and keeps the kernel routing table stable while a gateway is drained.

### Status Aggregation

Node agents publish status to the controller leader. The shipped configuration
uses a 60 second push interval for both the direct controller path and the
aggregated API server path. Summaries are sent as gzip-compressed JSON with a
bearer token issued by the controller. The controller keeps the latest status per node, marks a node
stale after 90 seconds, and exposes the combined view to the dashboard and to the
`kubectl unbounded` commands.

Periodic pushes use deltas by default, and the agent also performs a full sync
every two minutes so the controller can recover from a missed delta. When a
WebSocket is available, the agent can publish summaries over it and fall back to
the aggregated API server path if the direct connection fails.

Dashboard clients receive summary updates over a WebSocket. The broadcaster
coalesces changes on a two second ticker and forces a broadcast every ten seconds
even when nothing changed, so a client that missed a delta still converges.
Detailed diagnostic payloads are not broadcast. The dashboard or a CLI command
requests them explicitly, the controller forwards a request to the node, and the
response is cached for 300 seconds by default with a 120 second request timeout.

## Security Model

Unbounded-net has two trust zones. WireGuard protects traffic that crosses a
boundary the operator does not control, and the unencrypted protocols assume the
underlay is already private. The API server is the only authority for topology,
and the node agents authenticate to it with their ServiceAccount credentials.

### WireGuard Key Management

Each node stores its key pair under the WireGuard directory, which defaults to
`/etc/wireguard` on the host. The private key is `server.priv` with mode `0600`,
and the public key is `server.pub` with mode `0644`. The directory is created with
mode `0700` and is validated against a small list of safe prefixes before the agent
writes anything into it.

When the private key is absent, the agent generates 32 random bytes, clamps them
for Curve25519, derives the public key with `ScalarBaseMult`, and writes both files
as base64. When the private key exists, the agent reads the existing public key
and reuses the pair. It does not rotate keys on a schedule. Removing the key files
creates a new identity on the next start, and every peer that cached the old public
key must learn the new one.

The public key is published on the Node as the
`net.unbounded-cloud.io/wg-pubkey` annotation. The controller copies that value
into `SiteNodeSlice` entries and `GatewayPool.status.nodes`, and peer agents use it
to configure WireGuard peers. The private key never leaves the node. There are no
pre-shared keys in the configuration.

Peer authentication has two parts. WireGuard verifies the public key during the
handshake, and `AllowedIPs` restricts which source addresses a peer may send.
`AllowedIPs` is also what tells the WireGuard driver which peer owns a destination,
so the same field provides both routing and a source-address check. Endpointless
peers are legitimate for internal pools: the node cannot initiate the handshake,
but it accepts the peer once the remote side connects.

### Unencrypted Tunnel Note

GENEVE, VXLAN, IPIP, and `None` do not encrypt traffic. They are intended for
trusted internal networks where the CPU cost of encryption would limit throughput
on high-bandwidth links. An operator who selects one of these protocols for a
public link is explicitly choosing to send pod traffic in the clear.

`Auto` prevents that choice by accident. Links that use external or public
addresses resolve to WireGuard, and a non-peered gateway link that touches an
`External` pool also requires WireGuard. The public preference can be changed, but the node agent
logs a warning at startup when the configured preference is not WireGuard. This is
a deliberate escape hatch for networks that provide encryption elsewhere, not a
recommended default.

Site-to-gateway and gateway-to-gateway links can still use GENEVE or IPIP when both
ends are private and the governing scope selects those protocols. All hops are
resolved independently, so a path may be encrypted on the wide area link and
unencrypted on the local access link. Plan the trust boundary per link, not per
site.

### RBAC

The controller and the node agent have separate ServiceAccounts and separate
ClusterRoles. The controller's Node access is broad because it must label and patch
any node, but a ValidatingAdmissionPolicy restricts what it may change. It can
write only the unbounded labels and annotations, the pod CIDR fields, and the exact
gateway taint. The policy prevents a compromised controller from rewriting
arbitrary node metadata.

| Resource | Controller | Node agent |
|----------|------------|------------|
| Nodes | get, list, watch, patch, update | get, watch, patch |
| Sites | get, list, watch, update, patch, status | get, list, watch |
| SiteNodeSlices | full access | get, list, watch |
| GatewayPools | get, list, watch, update, patch, status | get, list, watch |
| GatewayPoolNodes | full access | get, list, watch, create, update, patch, status |
| SitePeerings, SiteGatewayPoolAssignments, GatewayPoolPeerings | get, list, watch, status for peerings | get, list, watch |
| Leases | full access in the controller namespace | none |
| EndpointSlices, Endpoints | leader manages the controller endpoint | none |
| TokenReviews | create | none |
| Node status API | serves and stores pushes | create push, request a node token |

The node agent's write surface is intentionally small. It can patch its own Node,
publish `GatewayPoolNode` status, create a status push, and request a node token.
It cannot read Secrets, modify Sites, or change gateway pool membership. The
controller holds the lease for leader election and manages the Service endpoint so
that status pushes reach only the active leader.

### Gateway Node Isolation

A node that joins a gateway pool receives the
`net.unbounded-cloud.io/gateway-node=true:NoSchedule` taint. Regular workloads
cannot schedule there, because a gateway does not have the full set of local pod
routes that a worker has. Keeping application pods off the gateway avoids sending
their traffic through a node whose forwarding policy is tuned for transit.

DaemonSets tolerate all taints, so the node agent, CNI, logging, and monitoring
agents still run on gateway nodes. When a node is demoted back to a worker, the
agent removes the taint as part of acknowledging the empty gateway pool assignment.
The controller also deletes the old node-agent Pod so the replacement starts with
the worker role and configures CNI normally.

## Failure Modes

The architecture separates control plane failures from data plane failures. Losing
the controller does not stop existing pods from communicating, and losing one
gateway does not necessarily stop cross-site traffic. The sections below describe
what is preserved and what is not.

### Controller Failure

The controller is not in the packet path, so a controller outage does not interrupt
established pod-to-pod flows. Node agents keep their last applied WireGuard peers,
routes, and BPF entries. They continue to health check peers and update nexthop
flags locally. What stops is topology change: new nodes do not receive pod CIDRs,
site membership updates do not propagate, and gateway pool membership does not
change.

Leader election uses a Lease in the controller namespace. A standby process can
take over when the lease expires, and the new leader creates fresh informers and
seeds its allocators from the Nodes and Sites it finds. Pod CIDR allocation is
fenced to the period in which the process has confirmed it holds the lease, so a
deposed leader that has not yet noticed cannot allocate an address.

The default Deployment runs one replica, so a Pod restart is a gap rather than an
instant failover. Running two replicas removes that gap but requires the second Pod
to be schedulable on a system or control-plane node. Because the controller uses
host networking and a fixed port, the rollout strategy terminates the old Pod
before starting the new one on the same node.

### Gateway Failure

A gateway failure is detected by the health checks described earlier. On a
multi-nexthop entry, the failing gateway's nexthops are marked unhealthy and the
BPF program stops selecting them. HRW hashing means only the flows that were
assigned to that gateway are rehashed onto the remaining gateways. With three
gateways, losing one moves roughly one third of the affected flows and leaves the
rest untouched.

A single-nexthop entry is a special case. The BPF program forwards it
unconditionally, even when the health flag is clear, because there is no alternate
path. If that gateway is down, traffic to the destination is black-holed until the
gateway recovers or a new gateway is added to the pool. This is a deliberate
trade-off: dropping the packet locally would not restore connectivity and would
hide the failure from ordinary path detection.

Gateway nodes advertise their reachable CIDRs in `GatewayPoolNode.status.routes`
and refresh a heartbeat every 10 seconds. A consumer treats an advertisement as
stale after four heartbeat intervals, or 40 seconds, and falls back to the pool's
configured `routedCidrs` for that gateway. This keeps a crashed gateway from
leaving permanently stale routes in the topology after its process disappears.

When a gateway restarts, the agent recreates its interfaces and peers from the
objects it observes. WireGuard sessions re-handshake, routes are reinstalled, and
the health check state starts from a clean session. Unchanged peers on other nodes
are not disturbed, because their desired state has not changed and the managers
apply differences rather than rebuilding everything.

### Network Partition

A partition between sites stops cross-site traffic until one side can reach a
gateway on the other side, but it does not stop intra-site traffic. Pods in the
same site continue to reach each other through `cbr0` and the local mesh, or
through the external CNI when unbounded-net does not manage CNI. Gateways on each
side continue to serve their local workers.

If a worker can reach some gateways but not others, ECMP keeps traffic on the
reachable paths and the health checks withdraw the unreachable ones. If no gateway
is reachable, the worker still has routes, but the packets are dropped at the
tunnel or at the first gateway that cannot forward them.

A control plane partition has a different effect. Node agents can still read from
their local informer caches and keep forwarding, but they cannot observe topology
changes or publish fresh status. The controller marks nodes stale after 90 seconds,
so the dashboard shows the last known state rather than pretending the nodes are
current. New node assignments and gateway pool changes wait until the API server is
reachable again.

## Performance Considerations

The design favors a small, stable dataplane over a large per-peer configuration.
Every control-plane component uses Kubernetes informers, so updates arrive as
watch events and the local cache serves reads without an API call. The controller's workqueue
coalesces repeated events for the same key, and its slice loop coalesces topology
changes into periodic passes.

All managers apply differential updates. The WireGuard manager compares current
and desired peers, the route manager tracks expected routes per interface, and
the BPF reconciler computes a desired trie and deletes only stale keys. This
preserves existing WireGuard handshakes, avoids interface flapping, and keeps route
table churn low during resyncs.

Routes are programmed with netlink nexthop objects. Multiple gateways on the same
prefix become an ECMP group, and the kernel distributes traffic per flow. The route
manager merges duplicate prefix entries, keeps only the nexthops at the best
metric, and reuses nexthop IDs by peer so a change to one gateway does not rewrite
unrelated groups.

The BPF fast path is a single LPM lookup and one redirect. The trie holds up to
four nexthops per prefix, which is enough for gateway redundancy without turning
the program into a loop over an unbounded list. HRW selection adds a small,
fixed amount of hashing only for multi-nexthop entries, and the trace ring drops
events when no consumer is attached at the cost of one failed reserve per packet.

Health check traffic is modest by default. A 15 second interval per peer is far
less traffic than a one second probe, and flap backoff prevents a failing link from
amplifying probe volume. The trade-off is detection latency: with the default
settings, a dead peer can take up to 45 seconds to be declared down.

Status reporting is summary-first for the same reason. Routine pushes contain
counts and health summaries without walking the BPF map, and detailed payloads are
collected only when a dashboard or CLI asks for them. The controller's WebSocket
broadcaster coalesces updates, and its forced ten second refresh keeps clients
converged without a busy publish loop.

## Next Steps

- Read the [custom resources reference]({{< relref "reference/networking/custom-resources" >}})
  for the full field list on Site, GatewayPool, SitePeering, and the assignment
  objects.
- Read the [configuration reference]({{< relref "reference/networking/configuration" >}})
  for runtime settings, health check profiles, and CNI options.
- Read the [routing flows reference]({{< relref "reference/networking/routing-flows" >}})
  for packet-level diagrams of same-site, cross-site, and gateway transit paths.
- Read the [operations reference]({{< relref "reference/networking/operations" >}})
  for deployment, upgrade, and troubleshooting procedures.
- Start with [Networking concepts]({{< relref "concepts/networking" >}}) if you want
  the shorter conceptual introduction before working with the API objects.
