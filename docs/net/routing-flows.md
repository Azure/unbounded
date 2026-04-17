<!-- Copyright (c) Microsoft Corporation. Licensed under the MIT License. -->

# Routing Flows

This document describes the detailed packet-level routing flows for both the
eBPF dataplane (default) and the netlink dataplane (legacy). It covers
intra-site mesh traffic, cross-site gateway transit, reply-path symmetry, and
protocol selection.

## eBPF Dataplane Mode (default)

The eBPF dataplane uses a single TC egress BPF program (`unbounded_encap`)
attached to the `unbounded0` dummy interface. All overlay traffic is attracted
to `unbounded0` via kernel routes and then steered to the correct tunnel
interface by BPF LPM lookups.

### Key Interfaces

```
unbounded0   Dummy interface. Holds pod-CIDR gateway IPs and supernet routes.
             TC egress BPF (unbounded_encap) is attached here.
geneve0      Flow-based GENEVE interface (UDP port 6081 by default, VNI per-packet).
vxlan0       Flow-based VXLAN interface (lightweight tunnel encap).
ipip0        Shared IPIP tunnel interface.
wg<port>     WireGuard interfaces. wg51820 = mesh; wg51821+ = gateway ports.
cbr0         Pod bridge (default name).
```

### Intra-Site Mesh (GENEVE)

Packet flow for pod-to-pod traffic between two nodes in the same site, using
GENEVE encapsulation via the eBPF dataplane.

```
  Node A (source)                              Node B (destination)
  +-----------------+                          +-----------------+
  | Pod (src)       |                          | Pod (dst)       |
  |   |             |                          |   ^             |
  |   v             |                          |   |             |
  | cbr0            |                          | cbr0            |
  |   |             |                          |   ^             |
  |   v             |                          |   |             |
  | kernel routing  |                          | kernel routing  |
  | (supernet route |                          | (local table:   |
  |  on unbounded0) |                          |  dst IP is on   |
  |   |             |                          |  unbounded0)    |
  |   v             |                          |   ^             |
  | unbounded0      |                          | unbounded0      |
  | TC egress BPF   |                          |   ^             |
  | (unbounded_encap)|                         |   |             |
  |   |             |                          |   |             |
  |   | LPM lookup  |                          |   |             |
  |   | in map      |                          |   |             |
  |   | unbounded_  |                          |   |             |
  |   | endpoints_v4|                          |   |             |
  |   |             |                          |   |             |
  |   | set_tunnel_ |                          |   |             |
  |   | key(remote_ |                          |   |             |
  |   | ipv4, vni)  |                          |   |             |
  |   |             |                          |   |             |
  |   | derive inner|                          |   |             |
  |   | dst MAC     |                          |   |             |
  |   |             |                          |   |             |
  |   | bpf_redirect|                          |   |             |
  |   | (geneve0)   |                          |   |             |
  |   v             |                          |   |             |
  | geneve0         |                          | geneve0         |
  | (GENEVE encap)  |                          | (GENEVE decap)  |
  |   |             |                          |   ^             |
  |   v             |                          |   |             |
  | eth0            |----------- LAN ----------| eth0            |
  +-----------------+    UDP dst port 6081     +-----------------+
```

#### Step-by-Step Detail

1. **Pod egress**: The source pod sends a packet to a remote pod IP. The packet
   exits the pod network namespace and arrives on `cbr0` (the CNI bridge).

2. **Kernel routing**: The kernel consults its routing table. A supernet route
   (e.g. `100.64.0.0/16 dev unbounded0 scope global`) attracts the packet to
   `unbounded0`.

3. **TC egress BPF**: The `unbounded_encap` program runs on TC egress of
   `unbounded0`. It parses the Ethernet and IP headers.

4. **LPM lookup**: The BPF program performs a longest-prefix-match lookup in
   `unbounded_endpoints_v4` (or `unbounded_endpoints_v6` for IPv6) using the
   destination IP with a /32 prefix length.

5. **Tunnel metadata**: If the matched endpoint has `TUNNEL_F_SET_KEY` set, the
   program calls `bpf_skb_set_tunnel_key()` with:
   - `remote_ipv4` = underlay IP of the destination node
   - `tunnel_id` = VNI from the map entry
   - `tunnel_ttl` = 64

6. **Inner MAC rewrite**: The inner Ethernet destination MAC is rewritten to a
   deterministic value derived from the remote node's underlay IP:
   `02:<ip[0]>:<ip[1]>:<ip[2]>:<ip[3]>:FF`. This ensures the remote tunnel
   interface accepts the decapsulated frame.

7. **Redirect**: `bpf_redirect(ep->ifindex, 0)` sends the packet to `geneve0`.
   The kernel's GENEVE driver encapsulates the frame in a GENEVE header using
   the tunnel key metadata, wraps it in UDP (destination port 6081), and sends
   it via `eth0`.

8. **Network transit**: The outer UDP/IP packet traverses the underlay network
   to the remote node.

9. **Decapsulation**: On the remote node, the kernel receives the UDP packet on
   port 6081, matches it to `geneve0`, and decapsulates it. The inner frame is
   delivered to the kernel's network stack.

10. **Local delivery**: The kernel routes the inner packet. Because the
    destination pod IP is assigned to `unbounded0` (as a /32 or gateway IP on
    the local node), the local routing table resolves it. The packet is
    forwarded to `cbr0` and delivered to the destination pod.

### Cross-Site via Gateway (WireGuard)

Packet flow for traffic between pods in different sites, transiting through
WireGuard-encrypted gateway nodes.

```
  Site 1 Worker          Site 1 Gateway         Site 2 Gateway         Site 2 Worker
  +------------+        +---------------+      +---------------+      +------------+
  | Pod (src)  |        |               |      |               |      | Pod (dst)  |
  |  |         |        |               |      |               |      |  ^         |
  |  v         |        |               |      |               |      |  |         |
  | cbr0       |        |               |      |               |      | cbr0       |
  |  |         |        |               |      |               |      |  ^         |
  |  v         |        |               |      |               |      |  |         |
  | kernel     |        |               |      |               |      | kernel     |
  | routing    |        |               |      |               |      | routing    |
  |  |         |        |               |      |               |      |  ^         |
  |  v         |        |               |      |               |      |  |         |
  | unbounded0 |        | wg51821       |      | wg51822       |      | unbounded0 |
  | TC egress  |        | (WG decrypt)  |      | (WG decrypt)  |      | TC egress  |
  | BPF        |        |  |            |      |  |            |      | BPF        |
  |  |         |        |  v            |      |  v            |      |  ^         |
  |  | LPM     |        | kernel fwd    |      | kernel fwd    |      |  |         |
  |  | lookup  |        |  |            |      |  |            |      |  | LPM     |
  |  |         |        |  v            |      |  v            |      |  | lookup  |
  |  v         |        | unbounded0    |      | unbounded0    |      |  |         |
  | bpf_redir  |        | TC egress BPF |      | TC egress BPF |      | bpf_redir |
  | (wg51821)  |        |  |            |      |  |            |      | (geneve0) |
  |  |         |        |  | LPM lookup |      |  | LPM lookup |      |  ^         |
  |  v         |        |  | -> wg51822 |      |  | -> geneve0 |      |  |         |
  | wg51821    |        |  v            |      |  v            |      | geneve0    |
  | (WG encr)  |        | wg51822       |      | geneve0       |      | (decap)    |
  |  |         |        | (WG encrypt)  |      | (GENEVE encap)|      |  ^         |
  |  v         |        |  |            |      |  |            |      |  |         |
  | eth0 ------+------->| eth0          |      | eth0          |----->| eth0       |
  +------------+  LAN   |  |            |      |  ^            | LAN  +------------+
                        |  v            |      |  |            |
                        | eth0 (public) |------| eth0 (public) |
                        +---------------+  WG  +---------------+
                              Internet / WAN
```

#### Step-by-Step Detail

1. **Worker egress**: Same as intra-site steps 1-4. The BPF LPM lookup matches
   a remote site's CIDR and finds a WireGuard endpoint entry.

2. **BPF redirect to WireGuard**: The entry has no `TUNNEL_F_SET_KEY` flag (WG
   handles its own encapsulation). `bpf_redirect(ep->ifindex, 0)` sends the
   packet directly to the `wg51821` interface.

3. **WireGuard encryption**: The WireGuard driver on the worker encrypts the
   packet and sends it as a UDP datagram to the site 1 gateway's WireGuard
   endpoint.

4. **Gateway ingress (site 1)**: The gateway receives the encrypted packet on
   `eth0`, which is delivered to `wg51821` (the gateway's WireGuard interface
   for this port). WireGuard decrypts it, producing the original inner IP
   packet.

5. **Gateway forwarding (site 1)**: The kernel forwards the decrypted packet.
   The supernet route on `unbounded0` (scope global) attracts it. The TC egress
   BPF program runs again, performs an LPM lookup, and finds an entry pointing
   to `wg51822` (the gateway-to-gateway WireGuard interface for site 2).

6. **Cross-site WireGuard**: The packet is encrypted again and sent over the
   WAN/internet to the site 2 gateway.

7. **Gateway ingress (site 2)**: The site 2 gateway decrypts on its own
   `wg<port>` interface. The kernel forwards the packet, and the BPF LPM lookup
   on `unbounded0` now resolves the destination to a local site 2 worker via
   GENEVE.

8. **GENEVE encap to worker**: `set_tunnel_key` + `bpf_redirect(geneve0)`
   encapsulates the packet in GENEVE and sends it to the destination worker.

9. **Worker delivery**: The site 2 worker decapsulates on `geneve0`, routes to
   `cbr0`, and delivers to the destination pod.

### Reply Path and Symmetric Routing

Gateway nodes must ensure that reply packets take the same WireGuard tunnel as
the original request. Without this, asymmetric routing would cause connection
failures (the reply arrives from a different source, or rp_filter drops it).

#### Per-Interface FORWARD ACCEPT Rules (default)

The current default approach uses per-interface iptables FORWARD ACCEPT rules
instead of policy-based routing. When a tunnel or WireGuard gateway interface is
created, the node agent inserts:

```
iptables -I FORWARD 1 -i <iface> -j ACCEPT
```

on both the tunnel interface (e.g., `geneve0`, `vxlan0`) and the WireGuard
gateway interfaces (e.g., `wg51821`). This allows the kernel to forward
decapsulated overlay traffic between tunnel and WireGuard interfaces without
connmark/fwmark bookkeeping.

Rules are removed when the interface is deleted. This approach is simpler
than PBR and avoids the complexity of per-interface routing tables and fwmark
management.

#### Policy-Based Routing (Deprecated)

> **Deprecated:** Policy-based routing is disabled by default since v1.0.2.
> The `enablePolicyRouting` option defaults to `false`. Per-interface FORWARD
> ACCEPT rules (above) replace PBR for cross-site transit forwarding. The PBR
> code is retained for backward compatibility. Set `enablePolicyRouting: true`
> explicitly if you need the old behavior.

The legacy PBR approach uses a combination of connmark, fwmark, and policy routing:

#### Framework

```
  Ingress (PREROUTING)                    Egress (OUTPUT / POSTROUTING)
  +-----------------------------+         +-----------------------------+
  | 1. CONNMARK --restore-mark  |         | 3. CONNMARK --restore-mark  |
  |    (reply packets get their |         |    (locally-generated reply  |
  |     original fwmark back)   |         |     packets get fwmark)     |
  |                             |         |                             |
  | 2. For NEW ORIGINAL pkts   |         | 4. ip rule: fwmark N        |
  |    arriving on wg<port>:    |         |    -> lookup table N        |
  |    CONNMARK --set-mark N    |         |    (route reply out same    |
  |    (associates flow with    |         |     WG interface)           |
  |     this interface)         |         +-----------------------------+
  +-----------------------------+
```

#### iptables Mangle Rules

All rules live in the `mangle` table, in custom chains:

- **UNBOUNDED-GW-PRE** (jumped to from `PREROUTING`):
  1. `CONNMARK --restore-mark` -- for REPLY-direction packets, restores the
     previously saved connmark into the packet's fwmark. This causes the ip
     rule to route the reply out the correct WireGuard interface.
  2. Per-interface rules: `-m conntrack --ctstate NEW --ctdir ORIGINAL
     -i wg<port> -j CONNMARK --set-mark <N>` -- tags new connections arriving
     on a specific gateway interface with mark N.

- **UNBOUNDED-GW-OUT** (jumped to from `OUTPUT`):
  1. `CONNMARK --restore-mark` -- for locally-generated reply packets, restores
     the fwmark so the routing decision uses the correct table.

#### Policy Routing Tables

Each gateway WireGuard interface gets its own routing table:

- **Table number** = gateway WireGuard port number (e.g. 51821, 51822).
- **Table name** = interface name (e.g. `wg51821`), written to
  `/etc/iproute2/rt_tables`.
- **Default route** in the table points out the corresponding WireGuard
  interface.

The `ip rule` mapping:

- `fwmark <N> lookup <table>` where N = `port - (wireguardBasePort - 100)`.
- Rule priority = same normalized value.
- Marks are kept below bit `0x4000` to avoid conflict with kube-proxy's
  `KUBE-MARK-MASQ` path.

#### Example

For a gateway with interfaces `wg51821` and `wg51822`:

```
# ip rule show
1:    from all fwmark 0x65 lookup 51821    (0x65 = 101 decimal)
2:    from all fwmark 0x66 lookup 51822    (0x66 = 102 decimal)

# ip route show table 51821
default dev wg51821

# ip route show table 51822
default dev wg51822

# iptables -t mangle -L UNBOUNDED-GW-PRE
CONNMARK  all  --  anywhere  anywhere  CONNMARK restore
CONNMARK  all  --  anywhere  anywhere  ctstate NEW ctdir ORIGINAL iif wg51821 CONNMARK set 0x65
CONNMARK  all  --  anywhere  anywhere  ctstate NEW ctdir ORIGINAL iif wg51822 CONNMARK set 0x66

# iptables -t mangle -L UNBOUNDED-GW-OUT
CONNMARK  all  --  anywhere  anywhere  CONNMARK restore
```

### Key Requirements

#### rp_filter = 0 on Tunnel Interfaces

Reverse-path filtering must be disabled on tunnel interfaces **and** on
`net.ipv4.conf.all` and `net.ipv4.conf.default`. The kernel takes the maximum
of the per-interface and `all` values, so both must be 0. The init script sets
`rp_filter=0` on `all` and `default` so that newly created interfaces inherit
the correct value. The node agent also writes `0` explicitly after creating each
tunnel interface as defense-in-depth.

The node agent writes to `/proc/1/root/proc/sys/` (not `/proc/sys/`) to bypass
the container procfs overlay, which silently discards sysctl writes. The kernel
also resets `rp_filter` on remaining interfaces when an interface is deleted, so
`disableRPFilter()` is reapplied after interface cleanup.

```
/proc/sys/net/ipv4/conf/geneve0/rp_filter = 0
/proc/sys/net/ipv4/conf/vxlan0/rp_filter  = 0
/proc/sys/net/ipv4/conf/ipip0/rp_filter   = 0
/proc/sys/net/ipv4/conf/all/rp_filter     = 0
/proc/sys/net/ipv4/conf/default/rp_filter = 0
```

Without this, the kernel drops decapsulated packets because their source IP
(an overlay pod IP) is not reachable via the interface they arrived on.

See `cmd/unbounded-net-node/ebpf_geneve_config.go:disableRPFilter()`, which is
called from all tunnel datapaths (eBPF, kernel GENEVE, kernel VXLAN). Writes go
through `/proc/1/root/proc/sys/` to bypass the container procfs overlay.

#### Deterministic MAC on Tunnel Interfaces

All tunnel interfaces use a deterministic locally-administered MAC address
derived from the node's underlay IP:

```
02:<ip[0]>:<ip[1]>:<ip[2]>:<ip[3]>:FF
```

For example, a node with underlay IP `10.0.1.5` gets MAC `02:0a:00:01:05:ff`.

This is critical because:
- The BPF program rewrites the inner Ethernet destination MAC to match the
  remote node's tunnel interface MAC before redirecting.
- Without a stable, predictable MAC, the remote kernel would drop the
  decapsulated frame (wrong destination MAC).

Both the BPF program (`derive_mac_from_ipv4()` in `bpf/unbounded_encap.c`) and
the Go control plane (`TunnelMACFromIP()` in `pkg/ebpf/tunnel_map.go`) must
produce identical values.

For IPv6 underlay, the last 4 bytes of the IPv6 address are used:
`02:<ip[12]>:<ip[13]>:<ip[14]>:<ip[15]>:FF`.

#### Scope Global on unbounded0 Routes

Supernet routes installed on `unbounded0` use `scope global`
(`netlink.SCOPE_UNIVERSE`) rather than the default `scope link`.

This is essential for gateway nodes: `scope link` routes are treated as
"directly connected" and cannot participate in cross-interface forwarding. A
packet arriving on `wg51821` destined for an overlay IP must be forwarded via
`unbounded0` -- this only works if the route has `scope global`.

See `DesiredRoute.ScopeGlobal` in `pkg/netlink/route_manager.go`.

#### BPF Map Reconciliation: Single Deferred Reconcile

To avoid partial/inconsistent state, BPF map entries are not applied
incrementally as each protocol (GENEVE, VXLAN, WireGuard) adds its entries.
Instead:

1. Each protocol appends entries to `state.pendingBPFEntries` (a shared map).
2. After all protocols have contributed, a single call to
   `reconcilePendingBPFEntries()` performs an atomic reconcile:
   - Splits entries into v4 and v6 maps.
   - Deletes stale keys (present in the kernel map but not in desired state).
   - Updates/inserts desired keys.

This ensures that during the reconcile window, all tunnel destinations are
consistent and no stale entries cause misdirected packets.

See `cmd/unbounded-net-node/site_watch_reconcile.go` and
`pkg/ebpf/tunnel_map.go:Reconcile()`.

#### BPF ECMP: Multi-Nexthop with HRW Hashing

Each BPF LPM trie entry supports up to 4 nexthops per CIDR prefix, enabling
ECMP (Equal-Cost Multi-Path) load balancing at the BPF level. Nexthop selection
uses **HRW (Highest Random Weight)** consistent hashing, also known as
rendezvous hashing.

**How it works:**

1. For each outgoing packet, the BPF program computes a 5-tuple hash (src IP,
   dst IP, src port, dst port, protocol).
2. For each nexthop in the entry, HRW computes a weight from the combination of
   the flow hash and the nexthop index.
3. The nexthop with the highest weight is selected for that flow.

**Advantages of HRW over modulo hashing:**

- When a nexthop fails, only flows assigned to that nexthop are rehashed.
  All other flows keep their existing nexthop assignment.
- When a nexthop is added, only ~1/N of existing flows migrate (where N is
  the new total nexthop count).

**Health-aware selection:**

Each nexthop carries a `TUNNEL_F_HEALTHY` flag (bit 0x08 in the entry flags
bitmask). The health check system sets or clears this flag based on probe
results. During nexthop selection:

- Unhealthy nexthops (TUNNEL_F_HEALTHY not set) are skipped.
- If all nexthops are unhealthy, the first nexthop is used as a fallback.
- Healthcheck probes (UDP destination port 9997) are always forwarded
  regardless of health state, ensuring that recovery probes reach the remote
  side.

**Diagnostics:**

The `unroute` CLI displays per-nexthop health state in the HEALTHY column:

```
kubectl exec -n unbounded-net <pod> -c node -- unroute
```

Output includes a HEALTHY column showing `Y` (healthy) or `N` (unhealthy) for
each nexthop entry.

#### Tunnel Interface Lifecycle

Tunnel interfaces (`geneve0`, `vxlan0`, `ipip0`) and WireGuard gateway
interfaces are created lazily -- only when at least one peer requires them.
This avoids interface churn when protocols are not in use.

On creation, the node agent:

1. Creates the interface via netlink.
2. Inserts `iptables -I FORWARD 1 -i <iface> -j ACCEPT` for the interface.
3. Sets `rp_filter=0` on the interface.
4. Sets the deterministic MAC address.

On deletion, the node agent:

1. Removes the `iptables -D FORWARD -i <iface> -j ACCEPT` rule.
2. Deletes the interface.
3. Reapplies `rp_filter=0` on remaining interfaces (kernel resets values on
   interface deletion).

---

## Netlink Dataplane Mode (legacy)

The netlink dataplane does not use BPF. Instead, it creates per-peer tunnel
interfaces and relies on kernel routes to steer traffic. This mode is selected
with `--tunnel-dataplane=netlink`.

### Intra-Site Mesh (GENEVE)

Each peer gets a dedicated GENEVE interface named `gn<decimal_ip>`, where
`<decimal_ip>` is the big-endian uint32 representation of the peer's underlay
IPv4 address.

```
  Node A                                       Node B
  +------------------+                         +------------------+
  | Pod (src)        |                         | Pod (dst)        |
  |   |              |                         |   ^              |
  |   v              |                         |   |              |
  | cbr0             |                         | cbr0             |
  |   |              |                         |   ^              |
  |   v              |                         |   |              |
  | kernel routing   |                         | kernel routing   |
  | (per-/24 route   |                         | (local delivery) |
  |  dev gn<B_ip>)   |                         |   ^              |
  |   |              |                         |   |              |
  |   v              |                         |   |              |
  | gn<B_decimal_ip> |                         | gn<A_decimal_ip> |
  | (GENEVE encap)   |                         | (GENEVE decap)   |
  | fixed remote=B   |                         | fixed remote=A   |
  | fixed VNI        |                         | fixed VNI        |
  |   |              |                         |   ^              |
  |   v              |                         |   |              |
  | eth0  -----------+---- UDP 6081 -----------+ eth0             |
  +------------------+                         +------------------+
```

#### Detail

- Interface name example: node B has IP `172.16.1.5` --
  `gn2886729989` (= `0xAC100105`).
- Each GENEVE interface has a fixed remote endpoint IP and VNI configured at
  creation time (not flow-based).
- Kernel routes for the peer's pod CIDRs (typically /24s) point to the
  per-peer interface.
- The deterministic MAC (`02:<ip_bytes>:FF`) is set on each interface.
- `rp_filter=0` is set on each per-peer interface.

See `cmd/unbounded-net-node/geneve_config.go:geneveIfaceName()`.

### Per-Peer IPIP

Similar to GENEVE, but using IPIP encapsulation. Each peer gets a dedicated
interface named `ip<decimal_ip>`.

```
  ip<decimal_ip>     IPIP tunnel interface, one per peer.
                     Fixed local/remote underlay IPs.
                     Kernel routes for peer pod CIDRs point here.
```

IPIP provides the lowest overhead (no UDP header) but no VNI multiplexing and
no encryption. Suitable for private networks where encryption is handled at a
different layer.

See `cmd/unbounded-net-node/geneve_config.go:ipipIfaceName()`.

### Shared VXLAN

VXLAN in netlink mode uses a single shared `vxlan0` interface (not per-peer).
Per-peer routing is achieved via lightweight tunnel encapsulation metadata on
each route:

```
  ip route add <peer_cidr> encap ip src <local_ip> dst <peer_ip> dev vxlan0
```

The `vxlan0` interface is configured with source port range and destination
port settings from the node configuration.

See `cmd/unbounded-net-node/vxlan_config.go`.

### WireGuard in Netlink Mode

WireGuard operates the same way in both dataplane modes. The WireGuard driver
handles its own encapsulation/decryption. In netlink mode, kernel routes for
remote CIDRs point directly to the `wg<port>` interface (rather than being
handled by BPF redirect).

---

## Protocol Selection

Tunnel protocol selection follows a hierarchical scope system. Each link type
has a governing CRD object, and multiple override/fallback rules apply.

### Scope Hierarchy

| Link Type                      | Governing CRD              | Key                    |
|-------------------------------|----------------------------|------------------------|
| Same-site mesh peers          | Site                       | `siteName`             |
| Peered nodes (diff sites)    | SitePeering                | `remoteSiteName`       |
| Worker to gateway pool        | SiteGatewayPoolAssignment  | `siteName\|poolName`    |
| Same-pool gateway mesh        | GatewayPool                | `poolName`             |
| Gateway-pool to gateway-pool  | GatewayPoolPeering         | `poolPeeringName`      |

### Resolution Algorithm

```
  CRD scope value
       |
       v
  +------------------+
  | nil or "Auto"?   |--- no ---> Use the explicit protocol value
  +------------------+
       | yes
       v
  +------------------+
  | Uses external/   |--- yes --> WireGuard (security-wins rule)
  | public IPs?      |           (unless ConfigMap explicitly overrides
  +------------------+            --preferred-public-encap)
       | no
       v
  +------------------+
  | Use ConfigMap    |
  | --preferred-     |
  | private-encap    |
  | (default: GENEVE)|
  +------------------+
```

### SGPA Override and Fallback

The `SiteGatewayPoolAssignment` (SGPA) `tunnelProtocol` field overrides the
`Site` setting for links between workers and gateway nodes. This is necessary
because both ends of the link must agree on the protocol:

1. A non-gateway worker resolves its link to a gateway via the SGPA scope
   (keyed by `mySiteName|poolName`).
2. A gateway node resolves its link to same-site mesh peers also via the SGPA
   scope (so it matches what the worker chose).
3. If the SGPA value is `nil` or `Auto`, and the gateway is in the same site,
   the system falls back to the `Site` tunnelProtocol -- but only if the Site
   value is explicitly set (not Auto).

```
  Worker resolving link to gateway:
    scope = SGPA[mySite|pool]  (primary)
    if scope is nil/Auto AND sameSite:
      scope = Site[mySite]     (fallback)

  Gateway resolving link to same-site mesh peer:
    scope = SGPA[mySite|localPool[0]]  (primary)
    if scope is nil/Auto:
      scope = Site[mySite]             (fallback)
```

### External Gateway Pools and WireGuard

External gateway pools (type `External`, which is the default when unset)
always resolve to WireGuard for cross-site, non-network-peered links. This
applies when:

- The remote gateway pool type is `External`, OR
- The local node belongs to an External pool AND is a gateway node

AND the link is cross-site (different site names) AND the sites are not
network-peered.

The logic is:

```go
usesExternal := (peerPoolType == "External" || (isGatewayNode && localPoolIsExternal)) &&
    peerSiteName != mySiteName &&
    !networkPeeredSites[peerSiteName]
```

When `usesExternal` is true and the protocol is Auto, `resolveAutoTunnelProtocol`
returns `WireGuard`.

### Supported Protocol Values

| Value       | Description                                              |
|------------|----------------------------------------------------------|
| `Auto`     | Automatic selection based on link type and ConfigMap      |
| `GENEVE`   | Generic Network Virtualization Encapsulation (UDP 6081)   |
| `VXLAN`    | Virtual Extensible LAN                                   |
| `IPIP`     | IP-in-IP encapsulation (lowest overhead, no encryption)   |
| `WireGuard`| Encrypted tunnel (required for public/external links)     |
| `None`     | Direct routing, no tunnel encapsulation                   |

### Where Protocols Apply

- **Site.tunnelProtocol**: Controls intra-site mesh peer tunneling.
- **SitePeering.tunnelProtocol**: Controls tunneling between directly peered
  sites (via SitePeering CRD).
- **SiteGatewayPoolAssignment.tunnelProtocol**: Controls worker-to-gateway
  tunneling. Overrides Site for gateway peers; falls back to Site when
  Auto/nil.
- **GatewayPool.tunnelProtocol**: Controls same-pool gateway-to-gateway mesh
  tunneling.
- **GatewayPoolPeering.tunnelProtocol**: Controls cross-pool gateway-to-gateway
  tunneling.

---

## Source References

| Area                     | File                                                      |
|--------------------------|-----------------------------------------------------------|
| BPF program              | `bpf/unbounded_encap.c`                                   |
| BPF map management       | `pkg/ebpf/tunnel_map.go`                                  |
| eBPF tunnel setup        | `cmd/unbounded-net-node/ebpf_geneve_config.go`            |
| Netlink GENEVE/IPIP      | `cmd/unbounded-net-node/geneve_config.go`                 |
| Netlink VXLAN            | `cmd/unbounded-net-node/vxlan_config.go`                  |
| WireGuard config         | `cmd/unbounded-net-node/wireguard_config.go`              |
| Gateway policy routing   | `pkg/netlink/gateway_policy_manager.go`                   |
| Route manager            | `pkg/netlink/route_manager.go`                            |
| Protocol selection       | `cmd/unbounded-net-node/encapsulation.go`                 |
| Reconcile orchestration  | `cmd/unbounded-net-node/site_watch_reconcile.go`          |
| CRD types                | `pkg/apis/unboundednet/v1alpha1/types.go`                 |
