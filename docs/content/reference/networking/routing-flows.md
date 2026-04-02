---
title: "Routing Flows"
weight: 4
description: "Packet-level routing flows for eBPF and netlink dataplanes, and protocol selection."
---

This document describes the detailed packet-level routing flows for both
dataplane modes. For architecture context, see
[Architecture]({{< relref "reference/networking/architecture" >}}).

## eBPF Dataplane Mode (default)

The eBPF dataplane uses a single TC egress BPF program (`unbounded_encap`)
attached to the `unbounded0` dummy interface. All overlay traffic is attracted
to `unbounded0` via kernel routes and then steered to the correct tunnel
interface by BPF LPM lookups.

### Key Interfaces

| Interface | Purpose |
|-----------|---------|
| `unbounded0` | Dummy (NOARP). Holds pod-CIDR gateway IPs and supernet routes. TC egress BPF attached here. |
| `geneve0` | Flow-based GENEVE (UDP 6081). |
| `vxlan0` | Flow-based VXLAN. |
| `ipip0` | Shared IPIP tunnel. |
| `wg<port>` | WireGuard. `wg51820` = mesh; `wg51821+` = gateway ports. |
| `cbr0` | Pod bridge. |

### Intra-Site Mesh (GENEVE)

Pod-to-pod traffic between two nodes in the same site:

```
  Node A (source)                              Node B (destination)
  ┌─────────────────┐                          ┌─────────────────┐
  │ Pod (src)        │                          │ Pod (dst)        │
  │   ↓              │                          │   ↑              │
  │ cbr0             │                          │ cbr0             │
  │   ↓              │                          │   ↑              │
  │ kernel routing   │                          │ kernel routing   │
  │ (supernet route  │                          │ (local table:    │
  │  on unbounded0)  │                          │  dst on cbr0)    │
  │   ↓              │                          │   ↑              │
  │ unbounded0       │                          │ unbounded0       │
  │ TC egress BPF    │                          │   ↑              │
  │   ↓              │                          │   │              │
  │ LPM lookup in    │                          │   │              │
  │ unbounded_       │                          │   │              │
  │ endpoints_v4     │                          │   │              │
  │   ↓              │                          │   │              │
  │ set_tunnel_key   │                          │   │              │
  │ (remote_ip, vni) │                          │   │              │
  │   ↓              │                          │   │              │
  │ derive inner     │                          │   │              │
  │ dst MAC          │                          │   │              │
  │   ↓              │                          │   │              │
  │ bpf_redirect     │                          │   │              │
  │ (geneve0)        │                          │ geneve0          │
  │   ↓              │                          │ (GENEVE decap)   │
  │ geneve0          │                          │   ↑              │
  │ (GENEVE encap)   │                          │   │              │
  │   ↓              │                          │   │              │
  │ eth0  ───────────┼──── UDP 6081 ───────────→│ eth0             │
  └─────────────────┘                          └─────────────────┘
```

**Step by step:**

1. **Pod egress**: Packet exits the pod via `cbr0` (CNI bridge).
2. **Kernel routing**: Supernet route (e.g., `100.64.0.0/16 dev unbounded0
   scope global`) attracts packet to `unbounded0`.
3. **TC egress BPF**: `unbounded_encap` runs on TC egress of `unbounded0`.
4. **LPM lookup**: Longest-prefix-match in `unbounded_endpoints_v4` (or v6).
5. **Tunnel metadata**: If `TUNNEL_F_SET_KEY` is set,
   `bpf_skb_set_tunnel_key()` is called with remote underlay IP, VNI, and
   TTL=64.
6. **Inner MAC rewrite**: Destination MAC set to
   `02:<ip[0]>:<ip[1]>:<ip[2]>:<ip[3]>:FF`.
7. **Redirect**: `bpf_redirect(geneve0)` sends packet to GENEVE driver, which
   encapsulates in UDP 6081.
8. **Network transit**: Outer UDP/IP packet traverses the underlay LAN.
9. **Decapsulation**: Remote kernel receives on port 6081, decapsulates via
   `geneve0`.
10. **Local delivery**: Inner packet routed to `cbr0` and delivered to the
    destination pod.

### Cross-Site via Gateway (WireGuard)

Traffic between pods in different sites, transiting through gateway nodes:

```
  Site 1 Worker         Site 1 Gateway        Site 2 Gateway        Site 2 Worker
  ┌────────────┐       ┌──────────────┐      ┌──────────────┐      ┌────────────┐
  │ Pod (src)  │       │              │      │              │      │ Pod (dst)  │
  │  ↓         │       │              │      │              │      │  ↑         │
  │ cbr0       │       │              │      │              │      │ cbr0       │
  │  ↓         │       │              │      │              │      │  ↑         │
  │ unbounded0 │       │ wg51821      │      │ wg (decrypt) │      │ unbounded0 │
  │ BPF→redir  │       │ (decrypt)    │      │  ↓           │      │ BPF→redir  │
  │ (wg51821)  │       │  ↓           │      │ unbounded0   │      │ (geneve0)  │
  │  ↓         │       │ unbounded0   │      │ BPF→redir    │      │  ↑         │
  │ wg51821    │       │ BPF→redir    │      │ (geneve0)    │      │ geneve0    │
  │ (encrypt)  │       │ (wg51822)    │      │  ↓           │      │ (decap)    │
  │  ↓         │       │  ↓           │      │ geneve0      │      │  ↑         │
  │ eth0 ──────┼──LAN─→│ eth0         │      │ (encap)      │      │ eth0       │
  └────────────┘       │  ↓           │      │  ↓           │      └────────────┘
                       │ wg51822      │      │ eth0  ───LAN─┼─────→│            │
                       │ (encrypt)    │      └──────────────┘      └────────────┘
                       │  ↓           │
                       │ eth0 (pub)───┼──WAN──→ eth0 (pub)
                       └──────────────┘
```

**Step by step:**

1. Worker BPF LPM lookup finds WireGuard endpoint entry (no `TUNNEL_F_SET_KEY`
   flag). `bpf_redirect(wg51821)` sends packet to WireGuard.
2. WireGuard encrypts and sends UDP to Site 1 gateway.
3. Gateway decrypts, forwards via `unbounded0`. BPF lookup resolves to
   `wg51822` (gateway-to-gateway WireGuard).
4. Re-encrypted, sent over WAN to Site 2 gateway.
5. Site 2 gateway decrypts, BPF resolves destination to GENEVE.
6. GENEVE encap sent to destination worker over LAN.
7. Worker decapsulates and delivers to pod.

### Reply Path and Symmetric Routing

Gateway nodes ensure reply packets take the same WireGuard tunnel as the
request using connmark, fwmark, and policy routing:

**Framework:**

```
  Ingress (PREROUTING):
    1. CONNMARK --restore-mark   (reply packets get fwmark)
    2. For NEW pkts on wg<port>: CONNMARK --set-mark N

  Egress (OUTPUT):
    3. CONNMARK --restore-mark   (locally-generated replies get fwmark)
    4. ip rule: fwmark N → lookup table N (routes out same WG interface)
```

Each gateway WireGuard interface gets its own:
- **Routing table**: Table number = WireGuard port (e.g., 51821).
- **Default route**: Points out the corresponding WireGuard interface.
- **ip rule**: `fwmark <N> lookup <table>`.
- Marks are kept below `0x4000` to avoid conflict with kube-proxy's
  `KUBE-MARK-MASQ`.

### Key Requirements

#### rp_filter = 0 on Tunnel Interfaces

Reverse-path filtering must be disabled on tunnel interfaces **and**
`net.ipv4.conf.all` because the kernel takes the maximum of per-interface and
`all` values:

```
/proc/sys/net/ipv4/conf/geneve0/rp_filter = 0
/proc/sys/net/ipv4/conf/vxlan0/rp_filter  = 0
/proc/sys/net/ipv4/conf/ipip0/rp_filter   = 0
/proc/sys/net/ipv4/conf/all/rp_filter     = 0
```

#### Deterministic MAC on Tunnel Interfaces

All tunnel interfaces use a MAC derived from the node's underlay IP:
- IPv4: `02:<ip[0]>:<ip[1]>:<ip[2]>:<ip[3]>:FF`
- IPv6: `02:<last 4 bytes>:FF`

Both the BPF program and Go code must produce identical values.

#### scope global on unbounded0 Routes

Supernet routes use `scope global` (not `scope link`) so they participate in
cross-interface forwarding on gateway nodes.

#### Single Deferred BPF Map Reconcile

BPF map entries are accumulated in `pendingBPFEntries`, then a single
`reconcilePendingBPFEntries()` call atomically deletes stale entries and
upserts desired entries. This prevents partial/inconsistent state.

---

## Netlink Dataplane Mode (legacy)

The netlink dataplane creates per-peer tunnel interfaces and uses kernel
routing tables.

### Intra-Site Mesh (GENEVE)

Each peer gets `gn<decimal_ip>` (uint32 of peer's underlay IPv4). The interface
has a fixed remote endpoint and VNI; kernel routes for the peer's pod CIDRs
point to it.

### Per-Peer IPIP

Each peer gets `ip<decimal_ip>`. Minimal overhead (20 bytes), no VNI
multiplexing, no encryption.

### Shared VXLAN

A single `vxlan0` interface with per-peer routing via lightweight tunnel
encap metadata:
```
ip route add <peer_cidr> encap ip src <local_ip> dst <peer_ip> dev vxlan0
```

### WireGuard in Netlink Mode

Identical to eBPF mode. Kernel routes point directly to `wg<port>` interfaces.

---

## Protocol Selection

### Scope Hierarchy

| Link Type | Governing CRD |
|-----------|---------------|
| Same-site mesh | Site |
| Peered nodes (diff sites) | SitePeering |
| Worker to gateway pool | SiteGatewayPoolAssignment |
| Same-pool gateway mesh | GatewayPool |
| Gateway-pool to gateway-pool | GatewayPoolPeering |

### Resolution Algorithm

```
  CRD scope value
       ↓
  ┌──────────────────┐
  │ nil or "Auto"?   │── no ──→ Use the explicit protocol
  └──────────────────┘
       │ yes
       ↓
  ┌──────────────────┐
  │ Uses external /  │── yes ──→ WireGuard (security-wins)
  │ public IPs?      │
  └──────────────────┘
       │ no
       ↓
  ┌──────────────────┐
  │ Use ConfigMap    │
  │ --preferred-     │
  │ private-encap    │
  │ (default: GENEVE)│
  └──────────────────┘
```

### SGPA Override and Fallback

The SiteGatewayPoolAssignment `tunnelProtocol` overrides the Site setting for
worker-to-gateway links. When SGPA value is `Auto` or nil and the gateway is
in the same site, the system falls back to the Site's `tunnelProtocol` (only
if explicitly set).

### External Gateway Pools

External pools (default type) always resolve to WireGuard for cross-site,
non-network-peered links.

### Where Protocols Apply

| CRD Field | Controls |
|-----------|----------|
| `Site.tunnelProtocol` | Intra-site mesh tunneling |
| `SitePeering.tunnelProtocol` | Directly peered site tunneling |
| `SiteGatewayPoolAssignment.tunnelProtocol` | Worker-to-gateway tunneling |
| `GatewayPool.tunnelProtocol` | Same-pool gateway mesh tunneling |
| `GatewayPoolPeering.tunnelProtocol` | Cross-pool gateway tunneling |

## Next Steps

- **[Architecture]({{< relref "reference/networking/architecture" >}})** --
  System design and data flows.
- **[Configuration]({{< relref "reference/networking/configuration" >}})** --
  All flags and settings.
- **[Operations]({{< relref "reference/networking/operations" >}})** --
  Deployment, monitoring, and troubleshooting.
