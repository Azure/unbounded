---
title: "Routing Flows"
weight: 4
description: "Trace a packet through the Unbounded dataplane for same-node, same-site, gateway transit, and peered-site paths."
---

This page follows packets through the Unbounded network dataplane. It starts with two pods on one node, moves to two nodes in one site, then crosses a gateway to a remote site, and finishes with peered sites and the fallback paths that apply when a tunnel destination cannot be selected. Each section names the interfaces, the eBPF program, and the map entries that a packet touches.

The mental model is simple. Every overlay destination is represented by one longest-prefix-match entry in a single BPF trie. Kernel routes attract overlay traffic to a dummy interface, the TC egress program on that interface looks up the destination, chooses a nexthop, and redirects the packet to a tunnel device. The tunnel protocol attached to that nexthop decides whether the kernel wraps the packet in GENEVE, VXLAN, IPIP, or WireGuard, or sends it without encapsulation.

## The eBPF Dataplane

The dataplane has one TC classifier program, `unbounded_encap`, loaded from `bpf/unbounded_encap.c`. The node agent attaches it to the egress hook of the `unbounded0` dummy interface. There is no ingress eBPF program. Decapsulation is performed by the kernel tunnel drivers, and the resulting inner packet re-enters the ordinary routing stack.

Packets reach `unbounded0` because the node agent installs overlay routes that point at it. The routes cover remote pod CIDRs, site pod-CIDR pools, and gateway-advertised CIDRs. The node's own pod CIDRs are removed from the route set, and the connected route for a local pod CIDR is more specific than any site supernet, so pod-to-pod traffic on the same node never enters the tunnel path. The routes use `scope global`, which matters on gateway nodes: a packet that arrived from another interface must be allowed to forward through `unbounded0`, and a `scope link` route is treated as directly connected and cannot serve that role.

### A Single Egress Program

Attaching one program to one dummy interface keeps the fast path small. The program is not repeated per peer or per tunnel interface. Every overlay route funnels into the same egress hook, and the lookup result decides the tunnel device and the outer endpoint.

The dummy interface is created with `NOARP`. Because neighbor resolution is disabled, the kernel can hand the packet to `unbounded0` without an ARP exchange for a peer that is not on the local segment. The program still rewrites the inner Ethernet destination address, because the remote tunnel interface checks the address on the decapsulated frame.

### The Tunnel Map

The steering table is the `unb_endpts` LPM trie. Its key is a 16-byte address with a prefix length, and its value is a `tunnel_endpoint` containing up to four nexthops. IPv4 and IPv6 destinations share the same trie: an IPv4 destination is stored as `::ffff:<v4>`, and its prefix length is increased by 96 bits so the v4 portion is matched at the right depth. Native IPv6 destinations are stored as their 16 bytes. A runtime lookup in the program uses a prefix length of 128, so the trie returns the longest programmed prefix that covers the destination.

Each nexthop records the underlay endpoint, the VNI, the redirect ifindex, a health flag, and the tunnel protocol constant. The protocol constant is one of GENEVE, VXLAN, IPIP, WireGuard, or None. The program derives the redirect device and the encapsulation behavior from those fields. It does not keep a separate per-protocol map.

The map defaults to 16384 entries and is sized by `TunnelDataplaneMapSize`. The value type holds four nexthops because a destination prefix can be reachable through more than one gateway. Single-nexthop entries are the common case for a worker reaching a peer node or a single gateway. Multi-nexthop entries appear when several gateways advertise the same prefix.

The program also owns a ring buffer named `unb_trace`. Every invocation builds a trace event and tries to reserve a slot. When no consumer is attached, the ring saturates quickly and the reserve then fails, so the event is dropped. When you run `unroute --trace`, the events carry the 5-tuple, the matched prefix, the chosen nexthop, the protocol, and the redirect result.

### What the Program Does to a Packet

The program follows the same sequence for every packet that reaches it, and the nexthop fields decide the variations.

1. It reads the Ethernet header and checks the EtherType. Non-IP frames return `TC_ACT_OK` without a redirect.
2. For IPv4, it reads the destination from the IP header and builds the IPv4-mapped IPv6 key. For IPv6, it copies the 16-byte destination directly. Malformed or truncated headers return `TC_ACT_OK`.
3. It looks up the key in `unb_endpts`. A miss returns `TC_ACT_OK`, which leaves the packet on the normal path for `unbounded0` instead of redirecting it into a tunnel.
4. If the entry has one nexthop, the program selects it unconditionally. If the entry has several nexthops, it calls `bpf_get_hash_recalc` and runs Highest Random Weight selection over the healthy entries.
5. It rewrites the inner Ethernet destination to the deterministic MAC derived from the selected underlay endpoint. For an IPv4 endpoint the value is `02:<ip[0]>:<ip[1]>:<ip[2]>:<ip[3]>:FF`. For an IPv6 endpoint the last four bytes of the address are used.
6. If the protocol is GENEVE, VXLAN, or IPIP, it calls `bpf_skb_set_tunnel_key`. The tunnel ID is the VNI, the TTL is 64, and the remote endpoint is written as IPv4 or IPv6 depending on the address family. WireGuard and None skip this step.
7. It calls `bpf_redirect` with the nexthop ifindex and returns the helper result. The kernel then hands the frame to the selected tunnel device or to the underlay device for the None case.

The redirect is the only place where the path branches by protocol. Everything before it is shared, which is why the map can hold mixed protocol nexthops without a second lookup.

### Interfaces and Their Roles

The node agent owns a small, fixed set of interfaces. Names are defaults and can be changed with node-agent flags, but the routing roles do not change.

| Interface | Role | Default |
|-----------|------|---------|
| `unbounded0` | Dummy device that attracts overlay routes and carries the TC egress program | Fixed name |
| `geneve0` | Shared flow-based GENEVE device for same-site and private links | UDP 6081 |
| `vxlan0` | Shared flow-based VXLAN device | UDP 4789, source ports 47891 to 47922 |
| `ipip0` | Shared flow-based IPIP device | IP protocol 4, no ports |
| `wg<port>` | Per-port WireGuard device for mesh and gateway links | `wg51820` for the mesh, gateway ports from 51821 |
| `cbr0` | CNI bridge that connects pods on the node | `cbr0` |

The shared tunnel devices are created lazily. `geneve0` exists only when at least one peer uses GENEVE, Auto, or an empty protocol. `vxlan0` exists only when a peer uses VXLAN. `ipip0` exists only when a peer uses IPIP. WireGuard uses one device per listen port, with a mesh device plus a separate device for each gateway peer.

The MTU of each tunnel device is derived from the underlay MTU minus the protocol overhead, unless a scope sets an explicit `tunnelMTU`. The agent uses 58 bytes for GENEVE, 50 for VXLAN, 20 for IPIP, and 80 for WireGuard. This keeps encapsulated packets within the underlay MTU without relying on fragmentation.

## Same Node

Two pods on the same node do not use the tunnel dataplane at all. The pod network namespace has a veth pair, one end inside the pod and one end attached to `cbr0`. If both pods are in the node's pod CIDR, the destination is on-link for the pod, so the pod ARPs for the peer and sends the frame through its default interface. The bridge learns the destination MAC and forwards the frame to the correct veth. No host IP route lookup reaches `unbounded0`, no TC program runs, and no `unb_endpts` lookup occurs.

If the two local pods are in different local CIDRs, the pod still sends to its gateway address on `cbr0`, and the host routing table resolves the destination to a local address or to the bridge. The node agent removes its own pod CIDRs from the supernet set before installing routes on `unbounded0`, and `collectSupernets` deletes `state.nodePodCIDRs` explicitly. That removal is what guarantees the local path stays local. A stale or overly broad supernet that covered a local pod CIDR would send local traffic into a tunnel and break same-node communication, so the exclusion is part of the routing contract rather than an optimization.

Traffic between a pod and the node itself follows the same rule. The destination is a local address, the kernel delivers it locally, and the eBPF program is never involved. The deterministic MAC on `unbounded0` and the tunnel devices does not affect this path.

## Same Site, Different Nodes

When the destination pod is on another node in the same site, the packet leaves the local bridge and enters the overlay path. With the default private preference, the protocol for a same-site link is GENEVE. The packet is encapsulated once, crosses the site underlay, is decapsulated by the destination node, and is delivered through the destination bridge.

![Packet flow for two pods on different nodes in the same site, showing GENEVE encapsulation on the source node and decapsulation on the destination node](../../../img/networking-intra-site-geneve.svg)

### The Outbound Path

The source pod sends to a destination in the site's pod-CIDR pool. The destination is not on-link, so the pod sends the frame to the gateway address on `cbr0`. The host receives the frame on the bridge and performs a route lookup. The site supernet route, for example `100.64.0.0/16 dev unbounded0 scope global`, covers the destination and wins over the default route.

The packet is transmitted on `unbounded0`, which triggers `unbounded_encap` on the TC egress hook. The program parses the inner Ethernet and IP headers and builds a 128-bit lookup key from the destination. For an IPv4 destination the key is the IPv4-mapped form, so a destination such as `100.64.7.9` becomes `::ffff:100.64.7.9` with a prefix length of 128.

The `unb_endpts` trie returns the entry for the destination's prefix. A peer's pod CIDR produces a specific entry, while a gateway can contribute a broader routed CIDR or a host CIDR. Longest-prefix match means the most specific programmed entry wins. The node agent also pins each peer's underlay IP as a host CIDR, so traffic addressed to the peer node itself matches a /32 instead of the broader site supernet.

With a single nexthop, the program selects it directly. The nexthop points at the destination node's internal IP, carries the configured VNI, points at `geneve0`, and has the GENEVE protocol constant. The program rewrites the inner Ethernet destination to the deterministic MAC for that underlay IP, sets the tunnel key, and redirects the frame to `geneve0`.

The kernel GENEVE driver builds the outer headers. The outer destination is the peer's underlay IP, the outer destination UDP port is 6081, and the VNI comes from the tunnel metadata. The outer packet is then routed over the site underlay by the ordinary routing table, typically out the default interface. The underlay does not need to know about pod CIDRs.

### The Destination Node

The destination node receives the UDP packet on port 6081. The kernel matches it to the flow-based `geneve0` device and decapsulates it, producing the original inner frame. The inner destination MAC is the deterministic MAC that the source program derived, which matches the MAC configured on the destination `geneve0`.

Reverse-path filtering is disabled on the tunnel device and on the `all` pseudo-interface. A decapsulated packet carries an overlay source address that is not reachable through `geneve0`, so a strict reverse-path check would drop it. The agent writes `rp_filter=0` through `/proc/1/root/proc/sys` so the write reaches the host kernel rather than a container procfs overlay.

The kernel then routes the inner packet. The destination pod IP is local to this node, so the route resolves to `cbr0` and the bridge delivers the frame to the pod. The `unb_endpts` map is not consulted for this local delivery because the packet is already past the tunnel decapsulation and the destination is local.

### The Return Path

The destination pod replies to the source pod. Its route lookup finds the source node's CIDR on `unbounded0`, and `unbounded_encap` runs on the destination node. The map entry for the source pod's prefix points at the source node's underlay IP and `geneve0`, so the reply is encapsulated in the reverse direction.

The return path does not depend on connection state in the dataplane. Each node independently selects a tunnel for the destination prefix. When both nodes have a single nexthop for the peer's CIDR, the request and reply use the same pair of nodes and the same protocol. When a prefix has multiple gateways, the two directions can select different gateways, which the current forwarding model permits.

### What Changes with Another Protocol

VXLAN uses the same program and the same map shape. The nexthop protocol is VXLAN and the redirect ifindex points at `vxlan0`. The kernel builds a VXLAN header instead of a GENEVE header, using UDP destination port 4789 and the VNI from the tunnel metadata. Source ports are chosen from the configured range, 47891 to 47922 by default, which gives the underlay ECMP hashing something to spread across.

IPIP takes the shortest path through the kernel. The nexthop protocol is IPIP and the redirect ifindex points at `ipip0`. The kernel wraps the packet in an outer IP header with protocol number 4 and no UDP or VNI. The overhead is 20 bytes instead of 58, so IPIP carries the largest payload for a given underlay MTU, but it provides no tenant identifier and no encryption. The BPF program still sets a tunnel key for the remote endpoint because the IPIP device is flow-based, but the VNI has no meaning on this path.

WireGuard replaces the shared tunnel device with a per-port device. When the governing CRD selects WireGuard for a same-site link, the peer is filtered into the WireGuard set and the BPF nexthop points at `wg51820`. The program does not call `bpf_skb_set_tunnel_key` for WireGuard, because the WireGuard driver chooses the peer from its `AllowedIPs` and performs its own encryption. The outer packet is a WireGuard UDP datagram, and the receiving node decrypts it before the inner packet is routed.

None removes encapsulation entirely. The nexthop protocol is None and the redirect ifindex is the underlay default-route interface. The program does not set a tunnel key, so the kernel adds no GENEVE, VXLAN, IPIP, or WireGuard header and lets the underlay route the IP packet. This is only appropriate when the underlay already routes pod CIDRs between the sites.

## Different Sites Through Gateways

Gateways exist for sites that do not share a routable underlay. A worker in one site sends remote-site traffic to a gateway in its own site, the gateway forwards it across the wide area to a gateway in the destination site, and that gateway delivers it to the destination worker. The overlay destination never changes; only the outer tunnel endpoints change at each hop.

![Cross-site flow from a worker through two gateways to a worker in another site](../../../img/networking-cross-site-flow.svg)

### Choosing the Local Gateway

A worker learns gateway peers from its `SiteGatewayPoolAssignment`. The assignment names one or more gateway pools, and each pool publishes gateway nodes with underlay addresses, WireGuard public keys, pod CIDRs, and a gateway-specific WireGuard port. The worker's route planner combines the pool's routed CIDRs with CIDRs learned from sites reachable through the pool, then the node agent installs those prefixes as routes on `unbounded0`.

For each gateway peer, the worker adds one or more BPF nexthops for every advertised prefix. The nexthop's remote endpoint is the gateway's underlay address, and the redirect ifindex depends on the protocol selected for the worker-to-gateway link. That link is governed by the `SiteGatewayPoolAssignment`, with the `Site` setting as a fallback when the assignment is nil or Auto and the gateway is in the same site.

When the gateway is in the same site and the effective protocol is GENEVE, the worker's nexthop points at `geneve0`. The packet travels over the local site underlay to the gateway's internal IP and is decapsulated on the gateway's `geneve0`. When the assignment or site explicitly selects WireGuard, the worker's nexthop points at its own `wg<gatewayPort>` interface. That interface listens on the gateway's assigned port, but its configured endpoint is the gateway's mesh port, which is 51820 by default. The gateway decrypts the packet on `wg51820`, and its peer entry for that worker sends replies back to the worker's gateway port.

This distinction matters because the worker-to-gateway leg and the gateway-to-gateway leg are independent. A private local gateway link can use GENEVE while the cross-site link uses WireGuard. The packet is decapsulated at the gateway and encapsulated again with the protocol required for the next hop.

### Gateway Transit

The gateway receives the packet from the worker, either on `geneve0` or on its WireGuard port. The inner destination is still the remote pod IP. The gateway performs a route lookup and finds the remote site's prefix on `unbounded0`, because gateway-advertised CIDRs are included in the supernet routes. The TC egress program on the gateway then runs against the same `unb_endpts` map.

On the gateway, the entry for the remote prefix contains nexthops for the gateways that can reach it. When the next hop is a gateway in another pool, the nexthop points at that gateway's per-port WireGuard device, such as `wg51822`, and carries the WireGuard protocol constant. A gateway in the same pool is a mesh peer and uses the mesh device instead. For a private pool or a same-site destination, the nexthop can point at `geneve0` or `ipip0`. If the prefix has more than one gateway nexthop, the program uses HRW hashing over the healthy entries, so one flow stays on one gateway while other flows spread across the pool.

The redirect sends the packet to `wg51822` when the next hop is a cross-pool gateway. The WireGuard driver encrypts it and sends a UDP datagram to the remote gateway endpoint. On the remote gateway, the packet arrives on the WireGuard device that listens on that endpoint port, is decrypted, and re-enters the routing stack. The remote gateway's route lookup finds the destination site's CIDRs on `unbounded0`, and its `unb_endpts` entry points at the destination site's mesh tunnel, which is `geneve0` when the site uses the default private encapsulation.

For a shared tunnel, the final gateway sets the tunnel key and redirects to the mesh device, then the device encapsulates the packet and sends it to the destination worker's underlay IP. The worker decapsulates on `geneve0`, or decrypts on its mesh WireGuard device if the site uses WireGuard, then routes the inner packet to `cbr0` and delivers it to the pod. If the destination pod is on the gateway itself, the route resolves locally and the packet never reaches `unbounded0` for a third encapsulation.

A gateway can repeat this process more than once when gateway pools are chained by `GatewayPoolPeering`. Each gateway receives a decrypted or decapsulated packet, looks up the destination in its own `unb_endpts` map, and redirects it toward the next gateway. The overlay packet is unchanged across the chain; the outer headers are replaced at every hop.

### Forwarding Permission and Conntrack

A gateway forwards packets that arrive on one tunnel interface and leave on another. The node agent installs an `UNBOUNDED-FORWARD` chain in the filter table. Each managed tunnel or WireGuard interface gets a jump rule in `FORWARD` on ingress (`-i <iface> -j UNBOUNDED-FORWARD`) and an accept rule in the custom chain on egress (`-o <iface> -j ACCEPT`). The jump is inserted at the top of `FORWARD` so it is evaluated before Kubernetes' own forward rules.

This design accepts tunnel-to-tunnel forwarding without accepting arbitrary forwarded traffic. It uses two rules per interface instead of a blanket accept on every source interface, and it removes the rules when an interface is deleted. The older per-interface `iptables -I FORWARD 1 -i <iface> -j ACCEPT` form is detected and cleaned up as a legacy rule.

On gateway nodes, the agent also installs raw-table notrack rules for transit traffic. The `UNBOUNDED-NOTRACK` chain returns for destinations that are local to the node or belong to local pods, applies `CT --notrack` to transit supernets, and falls through to normal conntrack for everything else. Transit packets that are merely forwarded between tunnels do not consume conntrack entries and do not depend on connection state at the gateway.

### Gateway Ports and Interface Names

Gateway WireGuard ports are allocated by the controller. The first gateway port is 51821, and each gateway node receives a unique port across all gateway pools. A gateway's assigned port identifies that gateway to its peers; it is not necessarily the local listen port on the gateway itself.

When the worker-to-gateway link uses WireGuard, a non-gateway worker creates a `wg<gatewayPort>` interface for that gateway. The interface is named after the gateway's assigned port and listens on that port, but the configured endpoint is the gateway's mesh port, 51820 by default. On the gateway, the worker's packets arrive on `wg51820`, and the gateway's peer entry for the worker points back at the worker's gateway port. Replies therefore use the worker's per-gateway interface while requests use the gateway's mesh interface.

Cross-pool gateway links use a per-port device and cross the listen and endpoint ports. The node agent creates a device named after the peer's assigned port and listens on that port, while it sends to the peer at its own assigned port. The peer mirrors the arrangement, so each side receives on the port that identifies it to the other side. This is why a packet can arrive on one `wg<port>` at a gateway and leave on a different `wg<port>` toward the next gateway.

Mesh traffic uses `wg51820` by default. Cross-pool gateway traffic uses the gateway-specific ports. The node agent derives every WireGuard interface name by concatenating the configured prefix, `wg` by default, with the port number. It also removes stale WireGuard devices whose names do not match the current prefix, because a kernel WireGuard device owns its UDP listen port and a leftover device would block the new one with an address-in-use error.

### External and Internal Gateway Pools

A gateway pool's type is `External` when the field is unset. An external pool requires every gateway node to publish an external IP, and cross-site links through it resolve to WireGuard when the protocol is Auto. For a cross-site gateway peer in an external pool, the external address is the WireGuard endpoint, so the encrypted UDP datagrams traverse the public network without exposing pod addresses.

An internal pool can use internal addresses and can use a private encapsulation for gateway links. When two sites are network-peered, a gateway pool peering between pools in those sites may also use internal addresses even if the pool type would otherwise call for external ones. The protocol resolver treats network-peered sites as private for this decision, so an Auto link can resolve to GENEVE or IPIP instead of WireGuard.

![Two sites connected by gateway nodes, with the external underlay carrying WireGuard between the gateways](../../../img/networking-cross-site-gateway.svg)

The pool type affects both endpoint selection and protocol selection. It does not change the BPF lookup. The map still maps a destination prefix to one or more gateway underlay endpoints and an ifindex, and the program still redirects to that ifindex with or without a tunnel key.

## Externally Peered Sites

A `SitePeering` joins two or more sites without requiring a gateway pool. In this page, externally peered means sites joined by a `SitePeering` rather than by membership in one site. The behavior depends on `meshNodes` and on whether the sites share an underlay that can carry pod traffic.

### Direct Mesh Peering

With `meshNodes` set to true, which is the default, every non-gateway node in a peered site that has a WireGuard key and an assigned pod CIDR becomes a mesh peer of the equivalent nodes in the other peered site. The worker's route table contains the remote site's pod CIDRs on `unbounded0`, and `unb_endpts` maps those prefixes directly to the remote node's underlay IP. Traffic does not pass through a gateway.

The direct path reuses the same encapsulation logic as a same-site path. The source program selects the remote node, rewrites the inner MAC, sets the tunnel key when the protocol requires one, and redirects to `geneve0`, `vxlan0`, `ipip0`, or a WireGuard device. The destination node decapsulates or decrypts, routes to `cbr0`, and delivers the packet. The only difference from the same-site case is that the outer packet crosses a site boundary.

For an Auto link with internal addresses, the preferred private encapsulation is GENEVE, so the default direct peering path is GENEVE over the shared underlay. An explicit `tunnelProtocol` on the `SitePeering` can select WireGuard instead. In that case both nodes use `wg51820` for the direct link, the WireGuard driver encrypts the overlay packet, and the outer UDP datagram crosses the underlay without a GENEVE or VXLAN header. The BPF map still selects the peer and the redirect ifindex; the WireGuard protocol constant tells the program to skip `bpf_skb_set_tunnel_key`.

Direct mesh peering assumes that each node can reach the other node's internal address. The peer construction uses the node's internal IPs, not its external IPs. If two sites have no shared or routed private underlay, a `SitePeering` with `meshNodes: true` does not create a usable path by itself. The sites need a gateway pool with external endpoints, or the peering must be combined with a routing layer that makes the internal addresses reachable.

### Network Peering Without a Node Mesh

With `meshNodes: false`, the sites are still considered network-peered, but nodes do not create direct mesh peers for each other. The node agent records the remote site in its network-peered set without adding its nodes to the mesh peer list. When an assignment names a gateway pool, pod traffic between the sites follows the gateway path through that pool.

The network-peered flag still changes protocol and endpoint selection. A gateway pool peering between pools in network-peered sites is treated as private for the external-address rule, so an Auto gateway link can use internal addresses and a private encapsulation. The remote site's node CIDRs are excluded from gateway-advertised routes, because the underlay already reaches those node addresses. Pod CIDRs can still be routed through the gateways, since the pods are not reachable without the overlay.

This mode is useful when a full node mesh would be too large or when the sites are connected at the network layer but node-to-node tunnels are not desired. It separates reachability of the underlay from reachability of the pod network: the underlay can be private while the pod traffic still transits gateways.

### Peering and Gateway Pools Together

When `meshNodes` is true, the intended path for the peered site's pod CIDRs is the direct mesh. The route planner installs direct mesh entries for the peered pod CIDRs, and it filters the peer site's node-CIDR paths out of gateway advertisements. The node's `unb_endpts` entry for the peered prefix points at the remote node, so traffic to a peered site does not detour through a gateway.

When `meshNodes` is false, the same site is network-peered but not mesh-peered. Gateway routes can be built for its pod CIDRs, and gateway peers can use internal addresses because the site is in the network-peered set. The result is a gateway path over what may still be a private underlay, which is the intended combination for large or segmented environments.

The gateway bypass is a routing decision, not a special packet mode. The source program sees a destination prefix and chooses the nexthop that the route planner programmed. If the planner chose a direct peer, the packet goes directly. If it chose a gateway, the packet goes to the gateway. The eBPF path has no additional "peered" flag.

## Fallback Paths

Fallbacks exist at two levels: the control plane chooses a protocol and a peer, and the dataplane chooses what to do when the preferred choice is unavailable.

### Protocol Resolution Fallbacks

An explicit tunnel protocol on the governing object is always honored. When the value is nil or `Auto`, the resolver falls back to the endpoint-based preference. A link that uses external or public IPs resolves to the preferred public encapsulation, which defaults to WireGuard. A link that uses only internal addresses resolves to the preferred private encapsulation, which defaults to GENEVE. If the configured preference string is empty or unrecognized, the resolver uses its built-in fallback: WireGuard for public links and GENEVE for private links.

The security rule is implemented in the resolver rather than in the dataplane. When a link uses external IPs and the public preference is WireGuard, the result is WireGuard. If an operator explicitly sets the public preference to another protocol, the resolver honors it and the node agent prints a startup warning that public traffic will be unencrypted. The dataplane simply executes the protocol it receives in the map.

The site-to-gateway scope has its own fallback. A worker resolves a gateway link through the `SiteGatewayPoolAssignment` keyed by site and pool. If that value is nil or Auto and the gateway is in the same site, the resolver falls back to the `Site` setting, but only when the `Site` value is explicitly set. If both scopes are Auto, the ConfigMap preference decides. A gateway node uses the same assignment scope for its same-site mesh peers so both ends of the worker-to-gateway link agree on the protocol.

A `GatewayPoolPeering` has a deliberate exception. An explicit peering protocol, including an explicit `Auto`, governs cross-pool gateway links and does not inherit the `GatewayPool` or `Site` setting. That lets an operator say "use endpoint defaults here" without accidentally inheriting a lower-scope protocol. When the peering field is omitted, the existing pool fallback applies, with the same-site site fallback where it is relevant.

### Dataplane Fallbacks

The first dataplane fallback is the LPM miss. If the destination has no entry in `unb_endpts`, `unbounded_encap` returns `TC_ACT_OK` without a redirect. The packet is not encapsulated and is not sent to a tunnel device. Because `unbounded0` is a dummy interface, a miss normally means the packet cannot be delivered to a remote pod; the value of the behavior is that a missing entry does not crash the program or redirect to a random interface.

The second fallback is nexthop selection. A single-nexthop entry is always forwarded, even if its health flag is clear. This is intentional: a singleton peer has no alternative path, and dropping its traffic locally would only prevent the peer from recovering through real traffic. A multi-nexthop entry skips every nexthop whose health flag is clear. If no nexthop is healthy, the program returns `TC_ACT_OK` without a redirect. That leaves the packet on the normal path instead of sending it through a gateway known to be down.

Peer health is maintained by the healthcheck manager. It sends probes to peer overlay addresses on UDP port 9997 by default, and a state change callback updates every BPF nexthop that belongs to the peer through `SetPeerHealth`. The callback runs for the mesh and gateway peers that were registered with the manager. A peer that goes down is withdrawn from multi-nexthop selection; a peer that comes back is marked healthy again.

The third fallback is peer eligibility. The node agent skips a mesh peer that has no internal IP or no pod CIDR, and it skips a gateway peer that is not ready for tunnel programming. A gateway in a managed site without a pod CIDR stays out of the map until the CNI has assigned one. A gateway in a site with an unmanaged CNI can still be programmed when it advertises routed CIDRs. Skipping a peer removes its nexthop from the desired state, and the next reconcile deletes any stale entry.

The fourth fallback is interface availability. Shared tunnel devices are created only when at least one peer needs them. If no peer uses VXLAN, `vxlan0` is removed and no map entry points at it. If no peer uses GENEVE, `geneve0` is removed. WireGuard gateway devices are created per peer port and removed when the assignment or peering disappears. This keeps the map and the interface set consistent: a nexthop never points at a device that the agent has decided not to own.

## Reply Paths and Symmetric Routing

The current dataplane is stateless with respect to connections. Each node selects the tunnel entry from the destination prefix, and that entry determines the protocol and the set of eligible nexthops. Within a multi-nexthop entry, the member is chosen from the flow hash, but the choice never depends on conntrack state. A reply is routed by the same rule as a request: the destination prefix selects the entry. When both directions have a single nexthop for each other's prefixes, the request and reply take the same pair of nodes and the same protocol.

When a prefix has several nexthops, the two directions can select different gateways. HRW hashes the flow, and the reverse flow has a different 5-tuple, so it can land on a different member of the pool. The current forwarding model allows this because gateways forward tunnel-to-tunnel traffic without relying on conntrack and without source NAT. Asymmetric gateway selection is therefore not a failure mode by itself. It becomes a problem only when an external device such as a stateful firewall requires both directions to cross the same link, in which case the legacy policy routing mode can pin replies to the arrival interface.

The forwarding chain is what makes the stateless model work. The `UNBOUNDED-FORWARD` chain accepts a packet that arrives on a managed tunnel interface and leaves on another managed tunnel interface. The notrack chain keeps transit packets out of conntrack on gateway nodes. Neither mechanism depends on the reply arriving on the same interface as the request.

![Request and reply paths in the same site, with each node selecting the peer from its own tunnel map](../../../img/networking-symmetric-routing.svg)

### Legacy Policy Routing

The repository still contains a policy-based routing implementation for gateways, but it is disabled by default. The node agent's `EnablePolicyRouting` option defaults to false. The supported cross-site path uses the `UNBOUNDED-FORWARD` chain described above. The policy routing code is retained for backward compatibility when `enablePolicyRouting` is explicitly set to true.

The legacy mode uses connmark, fwmark, and per-interface routing tables. It installs two mangle chains, `UNBOUNDED-GW-PRE` and `UNBOUNDED-GW-OUT`. The prerouting chain restores a saved connection mark for reply packets and tags new original connections that arrive on a specific gateway WireGuard interface. The output chain restores the mark for locally generated reply packets. The mark is then used by an `ip rule` to select a routing table whose default route points back out the same WireGuard interface.

Each gateway WireGuard interface gets a table number equal to its port. With the mesh port at 51820 and the policy offset at 100, the table base is 51720. An interface on port 51821 therefore gets table 51821, a normalized mark of 101 (`0x65`), and rule priority 101. Marks are kept below `0x4000`, because kube-proxy toggles that bit on its masquerade path and a collision would cause unintended source NAT.

The table name is the interface name, written to `/etc/iproute2/rt_tables`, and the table contains a default route through that interface. The rule and the table are independent of the overlay route on `unbounded0`. The mark only decides which WireGuard interface carries the reply; the BPF program still decides whether the packet belongs in a tunnel at all.

You can inspect the legacy state on a gateway with ordinary iproute2 commands:

```bash
ip rule show
ip route show table 51821
iptables -t mangle -L UNBOUNDED-GW-PRE
iptables -t mangle -L UNBOUNDED-GW-OUT
```

The output shows the fwmark rules, the default route through the gateway interface, and the connmark restore and set rules. If the chains are empty or the option is false, the gateway is using the default forwarding chain instead.

## Dataplane Requirements

Several kernel and control-plane behaviors are required for the flows above to work. They are easy to miss because they are not visible in the packet diagram.

### Reverse-Path Filtering

`rp_filter` must be zero on every tunnel interface and on the `all` pseudo-interface. The kernel uses the maximum of the per-interface value and the `all` value, so setting only the tunnel device is not enough when `all` is strict. A decapsulated packet arrives on `geneve0` with an overlay source address, but the route back to that source points at `unbounded0` or another tunnel device, so a strict reverse-path check would drop the packet.

The node agent writes `rp_filter=0` to the interface and to `all` after it creates or modifies a tunnel device. It writes through `/proc/1/root/proc/sys` rather than `/proc/sys`, because the container's procfs may be an overlay that accepts writes without forwarding them to the host kernel. The node image's init script also sets `all` and `default` to zero and walks the existing interfaces, so newly created devices inherit a permissive default.

Deleting an interface can cause the kernel to reset `rp_filter` on the remaining interfaces. The agent therefore reapplies the setting after cleanup, not only at creation time. The WireGuard path performs the same write on the mesh and gateway interfaces, because a delegated CNI can reset `default.rp_filter` after the init container has run.

{{< callout type="warning" >}}
If you manage sysctls outside the node agent, keep `net.ipv4.conf.all.rp_filter=0`. Setting only the tunnel interface to zero while `all` remains strict still drops decapsulated traffic, because the effective value is the maximum of the two.
{{< /callout >}}

### Deterministic MAC Addresses

The shared GENEVE and VXLAN interfaces use a locally administered MAC derived from the node's underlay IP. For IPv4 the address is `02:<ip[0]>:<ip[1]>:<ip[2]>:<ip[3]>:FF`. For IPv6 the last four bytes of the address are used in the same pattern. The value is set when the interface is created and reconciled afterward in case an older device exists with a random MAC. IPIP has no MAC, and WireGuard devices do not use the derived value.

The BPF program rewrites the inner Ethernet destination to the remote node's derived MAC before redirecting to a GENEVE or VXLAN device. The remote kernel delivers the decapsulated frame to the interface whose MAC matches that value. If the two sides computed different values, the frame would be dropped even though the outer tunnel was correct. The BPF helper `derive_mac_from_ipv4_bytes` and the Go function `TunnelMACFromIP` implement the same formula and must stay in parity.

IPIP is a layer 3 tunnel and does not use a MAC address. The agent skips the MAC assignment for `ipip0`, and the kernel returns `ENOTSUP` if a caller tries to set one. The IPIP path therefore relies on the outer IP endpoint and does not depend on the inner Ethernet destination.

### Global Scope on unbounded0 Routes

Supernet routes on `unbounded0` use `scope global`, not the default `scope link`. A `scope link` route is treated as directly connected to the output interface and cannot participate in cross-interface forwarding. On a gateway, a packet arrives on a WireGuard or GENEVE interface and must be forwarded through `unbounded0` to reach the BPF program. That lookup only succeeds when the route is global.

The route manager sets the scope from `DesiredRoute.ScopeGlobal`, which the node agent sets for every supernet route it builds on `unbounded0`. The route also carries the computed tunnel MTU and the configured base metric. The next hop is the `unbounded0` link itself, because the BPF program, not the kernel, chooses the real tunnel destination.

### One Deferred BPF Reconcile

The node agent does not write the BPF map incrementally as each protocol is configured. The shared-tunnel phase for GENEVE, VXLAN, and IPIP and the WireGuard phase each append their nexthops to a shared `pendingBPFEntries` map. After all protocols have contributed, a single `reconcilePendingBPFEntries` call passes the combined desired state to `TunnelMap.Reconcile`.

The reconcile walks the kernel map, deletes keys that are no longer desired, and writes the desired keys. Because the write set is complete, the map never contains a half-built gateway entry or a tunnel destination that points at an interface the agent is about to remove. A transient partial map could misdirect packets during a resync, so the deferred apply is a correctness requirement rather than a performance choice.

A failed update is logged and does not abort the whole reconcile. The next resync retries the desired state. Stale entries are deleted even when a new entry for the same prefix is being written, so an address change does not leave the old nexthop behind.

### Tunnel Interface Lifecycle

Shared tunnel devices are created lazily. On creation, the agent brings the device up, clears stale addresses, sets the deterministic MAC where the protocol supports one, sets the MTU, disables `rp_filter`, and registers the device with the forwarding manager. On gateway nodes it also registers the device with the notrack manager. On deletion, it removes the forwarding and notrack rules, deletes the link, and reapplies `rp_filter=0` to the tunnel devices that remain.

WireGuard devices follow the same lifecycle on a per-port basis. The mesh device exists while there is at least one mesh peer. A gateway device exists while a gateway peer with that port is configured. When the last peer disappears, the device is removed and its port is released. This keeps the kernel interface set aligned with the map and avoids leaving a device that owns a port but has no peers.

## Protocol Selection

Tunnel protocol selection is hierarchical. Each link type has one governing custom resource, and the value on that resource is resolved before it reaches the dataplane. The dataplane never resolves a policy; it receives a protocol constant in the map and executes it.

### Scope Hierarchy

| Link | Governing object | Scope key |
|------|------------------|-----------|
| Same-site mesh peers | `Site` | Site name |
| Directly peered nodes in different sites | `SitePeering` | Remote site name |
| Worker to gateway pool | `SiteGatewayPoolAssignment` | Site name and pool name |
| Same-pool gateways | `GatewayPool` | Pool name |
| Gateway pool to gateway pool | `GatewayPoolPeering` | Pool peering name |

The scopes are intentionally narrow. A worker does not read a `GatewayPool` directly, and a gateway does not read a `SitePeering` for a link that is governed by a pool peering. Narrow scopes keep one object from silently changing a link type that it does not own.

Gateway nodes are the one place where the scope choice is not obvious. A gateway with same-site mesh peers uses the `SiteGatewayPoolAssignment` scope, not the `Site` scope, because the worker on the other end resolves the same link as a site-to-pool connection. Both ends must agree on the protocol, so the gateway adopts the worker's scope and falls back to `Site` only when the assignment is nil or Auto and the site value is explicit.

### Resolution Algorithm

The resolver first reads the governing object. An explicit value other than Auto is returned unchanged. Auto and nil are treated the same way and fall through to the endpoint-based preference.

The next question is whether the link uses external or public addresses. If it does, the resolver uses the preferred public encapsulation, which defaults to WireGuard. If it does not, the resolver uses the preferred private encapsulation, which defaults to GENEVE. The result is then stored on the peer and passed to the tunnel configuration path.

```text
governing object value
  |
  +-- explicit protocol ------------------------> use it
  |
  +-- nil or Auto
        |
        +-- link uses external/public IPs -----> preferred public (WireGuard)
        |
        +-- link uses internal IPs only -------> preferred private (GENEVE)
```

The public and private preferences come from the node agent configuration, which can be set through flags or the runtime ConfigMap. A public preference other than WireGuard is allowed but prints a warning, because it means public traffic will not be encrypted by the tunnel. A private preference can be GENEVE, VXLAN, IPIP, WireGuard, or None. An unrecognized value falls back to the built-in default for that endpoint class.

### Assignment Override and Fallback

The `SiteGatewayPoolAssignment` protocol overrides the `Site` protocol for worker-to-gateway links. The override exists because the two ends of the link resolve different objects: the worker sees a site-to-pool assignment, while the gateway sees a same-site mesh peer. The gateway uses the assignment scope for its same-site peers so both ends compute the same protocol.

The fallback is deliberately constrained. If the assignment value is nil or Auto, and the gateway is in the same site, the resolver reads the `Site` value. It uses that value only when it is explicitly set to something other than Auto. If the site value is also Auto, the ConfigMap preference decides. A site value of Auto does not create a special "inherit" behavior that overrides the ConfigMap.

For a worker reaching a gateway in a different site, the same-site fallback does not apply. The assignment value governs, and an Auto assignment resolves through the external-address rule. That is why a cross-site external pool normally produces WireGuard even when the local site's protocol is GENEVE.

### External Gateway Pools

A gateway pool with no type is treated as `External`. External pools select gateway nodes that publish external IPs, and they require an external IP for every gateway node they include. When a link crosses sites through an external pool and the sites are not network-peered, an Auto protocol resolves to WireGuard.

The rule has two parts. The first is that the remote pool is External. The second is that the local node belongs to an External pool and is acting as a gateway. Either condition makes the link external for protocol purposes, as long as the peer is in a different site and the sites are not network-peered. The network-peered exception lets two private sites use internal addresses and a private encapsulation even when a pool is typed External.

An Internal pool can publish internal addresses or external addresses, but it does not trigger the external rule on its own. When neither end triggers that rule, a link through an internal pool can use GENEVE, VXLAN, or IPIP with Auto and private endpoints. The choice is still made per link, so one site can use WireGuard to an external pool and GENEVE to an internal pool at the same time.

### Supported Protocols and Their Effect on the Path

| Value | Outer form | MTU overhead | Tunnel key |
|-------|------------|--------------|------------|
| `GENEVE` | UDP destination port 6081, VNI per packet | 58 bytes | Remote endpoint, VNI, TTL |
| `VXLAN` | UDP destination port 4789, VNI per packet | 50 bytes | Remote endpoint, VNI, TTL |
| `IPIP` | IP protocol 4, no ports | 20 bytes | Remote endpoint, no VNI meaning |
| `WireGuard` | UDP mesh or gateway port, encrypted | 80 bytes | Not set by BPF |
| `None` | No encapsulation | 0 bytes | Not set by BPF |
| `Auto` | Resolved before programming | Depends on result | Depends on result |

The protocol changes the outer header and the encryption state, not the overlay routing decision. GENEVE and VXLAN both carry a VNI from the map, so the same `geneve0` or `vxlan0` device can serve every peer that the map distinguishes. IPIP carries no VNI and has the smallest header. WireGuard encrypts the packet and selects its peer from `AllowedIPs`. None adds no header and relies on the underlay to route the packet.

### Where the Field Applies

`Site.spec.tunnelProtocol` controls intra-site mesh peers. `SitePeering.spec.tunnelProtocol` controls direct links between peered sites. `SiteGatewayPoolAssignment.spec.tunnelProtocol` controls worker-to-gateway links and overrides the site value for gateway peers, with the fallback described above. `GatewayPool.spec.tunnelProtocol` controls gateway-to-gateway links within one pool. `GatewayPoolPeering.spec.tunnelProtocol` controls links between different pools joined by that peering.

Each field accepts `WireGuard`, `IPIP`, `GENEVE`, `VXLAN`, `None`, or `Auto`. The CRD schema enumerates those values, so an invalid string is rejected before the node agent sees it. An omitted field behaves as Auto for resolution purposes, even though the stored object may have no value at all.

## Observability

The `unroute` tool reads `unb_endpts` directly from the kernel and prints the same entries that the dataplane uses. It is included in the node agent image and runs inside the node agent container.

```bash
kubectl -n unbounded-system exec <node-agent-pod> -c node -- unroute
kubectl -n unbounded-system exec <node-agent-pod> -c node -- unroute 100.64.7.9
kubectl -n unbounded-system exec <node-agent-pod> -c node -- unroute --trace
```

Replace `<node-agent-pod>` with the name of the node agent pod on the node you want to inspect. The first command prints one summary line per CIDR, with a single nexthop inline or a CIDR header followed by one indented nexthop line per member. Each nexthop shows the underlay endpoint, the device, the protocol, the VNI when it is nonzero, the MTU when it is known, and a healthy or unhealthy tag.

The second command filters the output to the entries that match a specific destination, which is useful when you are tracing one flow. The third command streams per-packet events from the `unb_trace` ring buffer. Each event carries the packet's addresses and ports, the matched prefix length, the number of nexthops, the chosen index, the protocol, and the redirect result. It is the fastest way to confirm that a packet reached `unbounded_encap`, which prefix it matched, and which tunnel it was sent to.

Use `unroute -j` when you need machine-readable output. The JSON form groups nexthops under their CIDR and preserves the same fields as the text output. This is useful for comparing a node's map against a route plan or for collecting a snapshot from several nodes during an incident.

## End-to-End Summary

| Scenario | First hop from the pod | Tunnel decision | Final delivery |
|----------|------------------------|-----------------|----------------|
| Same node | `cbr0` bridge | No BPF lookup | Local pod |
| Same site | `cbr0`, then `unbounded0` | `unb_endpts` to peer, GENEVE by default | `geneve0`, `cbr0` |
| Different sites through gateways | `cbr0`, then `unbounded0` | Local gateway, then remote gateway | Destination mesh tunnel, `cbr0` |
| Directly peered sites | `cbr0`, then `unbounded0` | `unb_endpts` to remote node, no gateway | Peer `geneve0` or `wg51820`, `cbr0` |
| Network-peered without mesh | `cbr0`, then `unbounded0` | Gateway pool with internal endpoints | Gateway, then peer tunnel |
| Explicit None | `cbr0`, then `unbounded0` | Redirect to default underlay device | Underlay routing |

The table is a map of the paths, not a replacement for the sections above. The important invariant is that the overlay destination selects one LPM entry, and that entry selects one tunnel device and one underlay endpoint. Everything else, including encryption, header format, and gateway chaining, follows from the protocol and the nexthop that the control plane programmed.

## Related

- [Networking architecture]({{< relref "reference/networking/architecture" >}}) explains how the node agent and controller divide responsibility.
- [Networking custom resources]({{< relref "reference/networking/custom-resources" >}}) documents the fields that govern protocol selection and peering.
- [Networking configuration]({{< relref "reference/networking/configuration" >}}) covers the flags and ConfigMap values that supply the fallback preferences.
- [Networking operations]({{< relref "reference/networking/operations" >}}) covers health checks, diagnostics, and day-two procedures for the dataplane.
