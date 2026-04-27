<!-- Copyright (c) Microsoft Corporation. Licensed under the MIT License. -->

# Architecture Guide

This document provides a detailed overview of unbounded-net's architecture, components, and their interactions.

## System Overview

unbounded-net is designed to provide seamless pod-to-pod networking across multiple Kubernetes sites using encrypted (WireGuard) or unencrypted (GENEVE, VXLAN, IPIP) tunnels. The encapsulation type is selected per link scope and can be set explicitly or resolved automatically based on link characteristics. The data plane supports two modes: **eBPF** (default) and **netlink**. The eBPF dataplane uses a TC egress BPF program and LPM trie maps for tunnel endpoint resolution, while the netlink dataplane uses per-peer tunnel interfaces and kernel routing tables.

```mermaid
graph TB
    subgraph "Control Plane"
        CTRL[unbounded-net-controller<br/>Leader Elected]

        subgraph "Controllers"
            SC[Site<br/>Controller]
            GC[GatewayPool<br/>Controller]
        end

        CTRL --> SC
        CTRL --> GC
    end

    subgraph "Data Plane (eBPF mode)"
        subgraph "Node 1"
            NA1[Node Agent]
            UB1[unbounded0<br/>dummy NOARP]
            BPF1[TC egress<br/>unbounded_encap]
            TUN1[geneve0 / vxlan0 / ipip0<br/>FlowBased]
            WG1[WireGuard<br/>wg&lt;port&gt;]
            CNI1[CNI<br/>Bridge]
        end

        subgraph "Node 2"
            NA2[Node Agent]
            UB2[unbounded0<br/>dummy NOARP]
            BPF2[TC egress<br/>unbounded_encap]
            TUN2[geneve0 / vxlan0 / ipip0<br/>FlowBased]
            WG2[WireGuard<br/>wg&lt;port&gt;]
            CNI2[CNI<br/>Bridge]
        end
    end

    subgraph "Custom Resources"
        SITE[Site]
        SNS[SiteNodeSlice]
        GP[GatewayPool]
        GPN[GatewayPoolNode]
        SPR[SitePeering]
        SGPA[SiteGatewayPoolAssignment]
        GPP[GatewayPoolPeering]
    end

    SC -->|labels nodes + assigns podCIDRs| NODE[Nodes]
    SC -->|creates| SNS
    GC -->|updates status| GP
    GC -->|creates| GPN
    GC -->|reads| SPR
    GC -->|reads| SGPA
    GC -->|reads| GPP

    NA1 -->|reads| SNS
    NA1 -->|reads| GP
    NA1 -->|reads| SPR
    NA1 -->|reads| GPP
    NA1 -->|configures| WG1
    NA1 -->|writes| CNI1
    NA1 -->|manages BPF maps| BPF1
    NA1 -->|routes to| UB1
    BPF1 -->|LPM lookup + redirect| TUN1
    BPF1 -->|redirect only| WG1

    NA2 -->|reads| SNS
    NA2 -->|reads| GP
    NA2 -->|reads| SPR
    NA2 -->|reads| GPP
    NA2 -->|configures| WG2
    NA2 -->|writes| CNI2
    NA2 -->|manages BPF maps| BPF2
    NA2 -->|routes to| UB2
    BPF2 -->|LPM lookup + redirect| TUN2
    BPF2 -->|redirect only| WG2

    TUN1 <-->|tunnel| TUN2
    WG1 <-->|tunnel| WG2
```

## Component Details

### Controller (`unbounded-net-controller`)

The controller runs as a Deployment with 2 replicas for high availability. Only the leader actively reconciles resources.

#### Pod CIDR Assignment (Site Controller)

Pod CIDRs are assigned by the Site controller based on per-site assignment rules:

```mermaid
flowchart TD
    A[Node Created/Updated] --> B{Has podCIDRs?}
    B -->|Yes| C[Mark in Assignment Allocator]
    B -->|No| D[Find Site for Node]
    D -->|No Match| E[Skip]
    D -->|Match| F[Select Assignment by Priority + Regex]
    F --> G[Fresh API Fetch]
    G --> H{Still needs CIDRs?}
    H -->|No| I[Skip - Race Prevented]
    H -->|Yes| J[Allocate from Assignment Pools]
    J --> K{Pool Exhausted?}
    K -->|Yes| L[FATAL: Exit Controller]
    K -->|No| M[Patch Node]

    N[Node Deleted] --> O[Release CIDRs]
```

**Key Design Decisions:**
- Fresh API fetch before allocation prevents double-allocation from stale informer cache
- Pool exhaustion is fatal to surface the problem immediately
- Pod CIDRs are marked as allocated in assignment allocators based on current site rules

#### Site Controller

Manages site membership and SiteNodeSlice objects:

```mermaid
flowchart TD
    A[Node Event] --> B[Get Node Internal IPs]
    B --> C{Match Site nodeCidrs?}
    C -->|Yes| D[Label Node with Site]
    C -->|No| E[Remove Site Label]

    F[Site Event] --> G[Collect Site Nodes]
    G --> H[Split into 500-node Slices]
    H --> I[Create/Update SiteNodeSlices]
    I --> J[Set OwnerReference to Site]
    J --> K[Update Site Status]
```

**SiteNodeSlice Design:**
- Maximum 500 nodes per slice (prevents etcd object size limits)
- Slices named `{site-name}-{index}` (e.g., `site-east-0`, `site-east-1`)
- OwnerReferences enable automatic garbage collection when Site is deleted

#### GatewayPool Controller

Maintains gateway node information:

```mermaid
flowchart TD
    A[Node/Pool Event] --> B[Get GatewayPool]
    B --> C[List All Nodes]
    C --> D{Matches Selector?}
    D -->|No| E[Skip Node]
    D -->|Yes| F{Has External IP?}
    F -->|No| E
    F -->|Yes| G{Has WireGuard Key?}
    G -->|No| E
    G -->|Yes| H[Add to Pool Status]
    H --> I[Calculate Health Endpoints]
    I --> J[Update Pool Status]
```

### Node Agent (`unbounded-net-node`)

The node agent runs as a DaemonSet on every node, including control plane nodes. It manages WireGuard interfaces, tunnel configuration, eBPF program lifecycle and BPF map reconciliation, route programming, and CNI configuration. A companion `unroute` debug tool can dump and query BPF LPM trie maps for tunnel endpoint resolution diagnostics.

#### Initialization Flow

```mermaid
sequenceDiagram
    participant Agent
    participant FS as Filesystem
    participant K8s as Kubernetes API
    participant Kernel

    Agent->>FS: Check for WireGuard keys
    alt Keys exist
        Agent->>FS: Read existing keys
    else Keys missing
        Agent->>Kernel: Generate key pair
        Agent->>FS: Write keys (0600 perms)
    end

    Agent->>K8s: Annotate node with pubkey

    loop Wait for podCIDRs
        Agent->>K8s: Watch node object
        K8s-->>Agent: Node update
        alt Has podCIDRs
            Agent->>Agent: Continue to CNI setup
        else No podCIDRs
            Agent->>Agent: Wait and retry
        end
    end

    Agent->>FS: Write CNI config (atomic)
    Agent->>K8s: Watch Site, SiteNodeSlice, GatewayPool
    Agent->>Agent: Configure WireGuard
```

#### WireGuard Interface Architecture

**Regular Nodes:**

```mermaid
graph LR
    subgraph "Regular Node"
        subgraph "Interfaces"
            WG0[wg51820<br/>Port 51820]
            GW0[wg51821<br/>Port 51821]
            GW1[wg51822<br/>Port 51822]
        end

        subgraph "Mesh Peers"
            P1[Site Peer 1]
            P2[Site Peer 2]
            P3[Site Peer N]
        end

        subgraph "wg51821 Peer"
            G1[Gateway A]
        end

        subgraph "wg51822 Peer"
            G2[Gateway B]
        end

        WG0 --> P1
        WG0 --> P2
        WG0 --> P3
        GW0 --> G1
        GW1 --> G2
    end
```

**Gateway Nodes:**

```mermaid
graph LR
    subgraph "Gateway Node"
        subgraph "Interface"
            WG0[wg51820<br/>Port 51820]
        end

        subgraph "Same-Site Peers"
            P1[Local Peer 1]
            P2[Local Peer 2]
        end

        subgraph "Remote Peers (no endpoint)"
            R1[Remote Site A Node 1]
            R2[Remote Site A Node 2]
            R3[Remote Site B Node 1]
        end

        WG0 --> P1
        WG0 --> P2
        WG0 -.-> R1
        WG0 -.-> R2
        WG0 -.-> R3
    end
```

#### Route Management

Routes are programmed directly into the kernel via netlink using nexthop objects. No external routing daemon is involved.

#### Encapsulation Types

The node agent supports multiple encapsulation types for tunnel links:

- **WireGuard**: Encrypted tunneling using the WireGuard protocol. Used by default for links that traverse untrusted networks (external/public IPs). Overhead: 80 bytes (IPv6 worst case).
- **GENEVE**: Unencrypted Generic Network Virtualization Encapsulation. Used by default for links using only internal IPs (same-site, network-peered sites, internal gateway pools). Overhead: 58 bytes. A single `geneve0` interface with FDB entries handles all GENEVE peers for higher throughput on high-bandwidth links.
- **VXLAN**: VXLAN encapsulation using a single external flow-based `vxlan0` interface. Similar overhead to GENEVE (~58 bytes). Useful when the underlying network has better VXLAN hardware offload support.
- **IPIP**: IP-in-IP tunneling with minimal overhead (20 bytes). Uses per-peer tunnel interfaces. Best for environments where encryption is not needed and minimal encapsulation overhead is desired.
- **None**: Direct routing with no encapsulation. Requires L3 reachability between nodes. Zero overhead but no isolation.
- **Auto**: System selects based on link characteristics. Links using external IPs resolve to WireGuard; links using only internal IPs resolve to GENEVE (configurable via `--preferred-private-encap` and `--preferred-public-encap` flags).

The `tunnelProtocol` field on Site, GatewayPool, SitePeering, SiteGatewayPoolAssignment, and GatewayPoolPeering specs controls the selection. When set to `Auto` (the default), the system chooses based on link characteristics. The **security-wins rule** means that if any scope in the hierarchy explicitly sets `WireGuard`, the link uses WireGuard regardless of other scopes.

```mermaid
flowchart TD
    subgraph "Mesh Routes"
        R1[Site Pod CIDRs]
        R2[Remote Pod CIDRs<br/>Gateway Only]
    end

    subgraph "Gateway Routes"
        R3[All Other Sites' CIDRs]
        R4[Health Endpoint /32s]
    end

    subgraph "ECMP via Nexthop Groups (netlink)"
        E1[Nexthop: wg51821 gateway]
        E2[Nexthop: wg51822 gateway]
        E3[Nexthop Group<br/>Kernel ECMP]
    end

    R3 --> E1
    R3 --> E2
    E1 --> E3
    E2 --> E3
```

## Tunnel Dataplane Modes

The node agent supports two tunnel dataplane modes, selected by the `--tunnel-dataplane` flag: `ebpf` (default) and `netlink`.

### eBPF Dataplane (default)

The eBPF dataplane uses a single dummy device (`unbounded0`) as a routing anchor, a TC egress BPF program for tunnel endpoint resolution, and shared flow-based tunnel interfaces. This design avoids per-peer interface creation and scales to large meshes with minimal kernel resource usage.

#### Architecture

```mermaid
graph LR
    subgraph "eBPF Dataplane on a Node"
        POD[Pod traffic] -->|route| UB0[unbounded0<br/>dummy NOARP]
        UB0 -->|TC egress| BPF["unbounded_encap<br/>(BPF program)"]
        BPF -->|LPM lookup| MAPS["LPM Tries<br/>v4: 8B key, 20B val<br/>v6: 20B key, 32B val"]
        MAPS -->|match| BPF
        BPF -->|"set_tunnel_key + redirect"| GEN[geneve0<br/>FlowBased]
        BPF -->|"set_tunnel_key + redirect"| VXL[vxlan0<br/>FlowBased]
        BPF -->|"set_tunnel_key + redirect"| IPIP[ipip0<br/>FlowBased]
        BPF -->|"redirect only"| WG[wgXXXXX<br/>WireGuard]
    end
```

#### unbounded0 Device

`unbounded0` is a **dummy** network interface configured with:

- `NOARP` flag (no ARP resolution needed)
- Overlay IPs assigned directly (scope global)
- Supernet routes pointing to it for all remote pod CIDRs
- A `clsact` qdisc for TC program attachment

All traffic destined for remote overlay CIDRs is routed to `unbounded0`, where the TC egress BPF program intercepts it.

#### TC Egress BPF Program

The `unbounded_encap` program (defined in `bpf/unbounded_encap.c`) is attached as a TC egress classifier on `unbounded0`. For each outgoing packet, it:

1. Extracts the destination IP (v4 or v6)
2. Performs an LPM (longest-prefix-match) lookup against the appropriate BPF trie map
3. If the entry has `TUNNEL_F_SET_KEY` set, calls `bpf_skb_set_tunnel_key()` with the remote endpoint address and tunnel ID
4. If the entry has `TUNNEL_F_IPV6_UNDERLAY` set, uses the IPv6 remote address with `BPF_F_TUNINFO_IPV6`
5. Derives a deterministic inner Ethernet destination MAC and rewrites the packet header
6. Calls `bpf_redirect()` to send the packet to the target tunnel interface

#### BPF LPM Trie Maps

Two maps store overlay CIDR to tunnel endpoint mappings:

| Map | Key struct | Key size | Value struct | Value size |
|-----|-----------|----------|-------------|------------|
| `unbounded_endpoints_v4` | `lpm_key_v4` (prefixlen + IPv4 addr) | 8 bytes | `tunnel_endpoint_v4` (5x u32) | 20 bytes |
| `unbounded_endpoints_v6` | `lpm_key_v6` (prefixlen + IPv6 addr) | 20 bytes | `tunnel_endpoint_v6` (IPv6 union + 4x u32) | 32 bytes |

Both maps are `BPF_MAP_TYPE_LPM_TRIE` with `BPF_F_NO_PREALLOC` and a default max of 16384 entries.

Each map entry contains:

- **Remote IP**: The underlay endpoint address for the tunnel
- **Interface index**: The target tunnel interface for `bpf_redirect()`
- **Flags**: Bitmask controlling tunnel behavior
  - `TUNNEL_F_SET_KEY` (0x01) -- set tunnel key/metadata (used by GENEVE, VXLAN, IPIP)
  - `TUNNEL_F_IPV6_UNDERLAY` (0x04) -- use IPv6 underlay addressing
- **Protocol**: Diagnostic identifier for the entry type
  - `PROTO_GENEVE` (1), `PROTO_VXLAN` (2), `PROTO_IPIP` (3), `PROTO_WIREGUARD` (4), `PROTO_NONE` (5)
  - The protocol field is stored for observability and diagnostic tooling (e.g., `unroute`) but does not drive BPF program behavior

#### BPF Map Reconciliation

The node agent reconciles BPF map entries whenever the desired peer state changes. Entries are accumulated into a pending map (`state.pendingBPFEntries`) across all peer configuration functions, then a single `Reconcile()` call:

1. Iterates existing LPM entries
2. Deletes stale entries no longer in the desired set
3. Upserts new or changed entries
4. Keeps IPv4 and IPv6 tries synchronized

#### Shared Tunnel Interfaces

In eBPF mode, tunnel interfaces are shared across all peers of the same type:

- **`geneve0`**: Flow-based GENEVE interface. BPF sets the remote endpoint and VNI via `bpf_skb_set_tunnel_key()`.
- **`vxlan0`**: Flow-based VXLAN interface. Same metadata-driven approach as GENEVE.
- **`ipip0`**: Flow-based IP-in-IP interface. Tunnel key sets the remote endpoint.
- **WireGuard (`wgXXXXX`)**: Not flow-based. BPF performs redirect-only (no tunnel key). Crypto routing is handled by the WireGuard driver using AllowedIPs on each peer. Multiple WireGuard interfaces may exist (one per port/gateway link).

#### Deterministic MAC Derivation

The BPF program and Go code both derive the inner Ethernet destination MAC deterministically from the destination IP, avoiding the need for ARP/ND on tunnel interfaces:

- **IPv4**: `02:<ip[0]>:<ip[1]>:<ip[2]>:<ip[3]>:FF`
- **IPv6**: `02:<last 4 bytes of IPv6>:FF`

This MAC is written into the Ethernet header before `bpf_redirect()` and is also configured on tunnel interface endpoints so the receiving side accepts the frame.

#### rp_filter Requirements

Reverse path filtering must be disabled on tunnel interfaces because decapsulated packets arrive on `geneve0`/`vxlan0`/`ipip0` while the overlay routes point to `unbounded0`. The node agent writes `0` to:

- `/proc/sys/net/ipv4/conf/<tunnel-iface>/rp_filter`
- `/proc/sys/net/ipv4/conf/all/rp_filter`
- `/proc/sys/net/ipv4/conf/default/rp_filter`

The node init script sets `net.ipv4.conf.all.rp_filter=0` and `net.ipv4.conf.default.rp_filter=0` so that newly created tunnel interfaces inherit the correct value. The node agent also explicitly writes `0` after creating each tunnel interface as defense-in-depth.

Writes go through `/proc/1/root/proc/sys/` to bypass the container procfs overlay, which silently discards direct `/proc/sys/` writes. The kernel also resets `rp_filter` on remaining interfaces when an interface is deleted, so `disableRPFilter()` is reapplied after interface cleanup.

### Netlink Dataplane

The netlink dataplane uses per-peer tunnel interfaces and kernel routing tables with a dedicated routing table and ip rule:

- Each GENEVE/VXLAN/IPIP peer gets a dedicated tunnel interface
- Routes are programmed with lightweight tunnel encap (`ip_encap`) into a separate routing table
- An ip rule directs overlay traffic to the dedicated table
- WireGuard interfaces work identically in both modes

The netlink dataplane is simpler but creates more kernel objects (interfaces, routes) as the mesh grows.

### unroute Debug Tool

The `unroute` tool (`cmd/unroute/main.go`) is a diagnostic utility for the eBPF dataplane. It can:

- Dump all entries in the `unbounded_endpoints_v4` and `unbounded_endpoints_v6` LPM trie maps
- Perform longest-prefix-match lookups for specific IPs to show which tunnel endpoint would be selected
- Dump the `local_cidrs` map
- Output in human-readable text or JSON format

## Data Flow

### Pod-to-Pod Communication (Same Site, eBPF mode)

```mermaid
sequenceDiagram
    participant PodA as Pod A<br/>10.1.0.5
    participant NodeA as Node A<br/>unbounded0 + TC BPF
    participant TunA as geneve0 / wg51820
    participant TunB as geneve0 / wg51820
    participant NodeB as Node B<br/>unbounded0 + TC BPF
    participant PodB as Pod B<br/>10.1.1.10

    PodA->>NodeA: Packet to 10.1.1.10
    NodeA->>NodeA: Route lookup<br/>10.1.0.0/16 via unbounded0
    NodeA->>TunA: TC egress BPF:<br/>LPM lookup + redirect
    TunA->>TunB: Tunnel encapsulated<br/>via internal IP
    TunB->>NodeB: Decapsulate + route
    NodeB->>PodB: Deliver to pod
```

### Pod-to-Pod Communication (Cross-Site)

```mermaid
sequenceDiagram
    participant PodA as Pod A<br/>Site East<br/>10.1.0.5
    participant NodeA as Node A
    participant GwA as Gateway A<br/>Site East
    participant GwB as Gateway B<br/>Site West
    participant NodeB as Node B
    participant PodB as Pod B<br/>Site West<br/>10.2.0.10

    PodA->>NodeA: Packet to 10.2.0.10
    NodeA->>NodeA: Route: 10.2.0.0/16 via wg51821
    NodeA->>GwA: WireGuard (internal IP)
    GwA->>GwB: WireGuard (external IP)
    GwB->>NodeB: WireGuard (internal IP)
    NodeB->>PodB: Deliver to pod
```

### Gateway Health Check Flow

```mermaid
sequenceDiagram
    participant Agent as Node Agent
    participant HC as Health Check Manager
    participant Peer as Peer Health Check Listener
    participant Routes as Netlink Route Table

    Agent->>HC: Start health check sessions for peers
    loop Every transmitInterval (default 1s)
        HC->>Peer: UDP probe (over WireGuard tunnel)
        alt Healthy
            Peer-->>HC: UDP response
            HC-->>Agent: Peer Up
            Agent->>Routes: Ensure base metric on routes
        else No response within detectMultiplier * interval
            HC-->>Agent: Peer Down
            Agent->>Routes: Increase route metric (deprioritize)
        end
    end
```

**Key Design Decisions:**
- Probes are sent and received over WireGuard tunnels at configurable intervals (default 1s)
- Failure detection uses `detectMultiplier * max(transmitInterval, receiveInterval)` to determine when a peer is down
- On failure, route metrics are increased to deprioritize unhealthy paths rather than removing routes entirely
- On recovery, route metrics are restored to their base values

### Push-Based Status Aggregation

Node agents periodically push their status to the controller, which caches and broadcasts updates to dashboard clients via WebSocket.

```mermaid
sequenceDiagram
    participant Agent as Node Agent
    participant Controller as Controller (Leader)
    participant Cache as Status Cache
    participant WS as WebSocket Clients

    loop Every 10 seconds
        Agent->>Agent: getNodeStatus()<br/>(snapshot state, WireGuard, routes, pingmesh)
        Agent->>Controller: POST /status/push<br/>(gzip JSON, Bearer token)
        Controller->>Controller: Authenticate token
        Controller->>Cache: Store status
    end

    loop Every 3 seconds
        Controller->>Cache: Fetch all cached statuses
        Controller->>Controller: Build ClusterStatusResponse
        Controller->>WS: Broadcast delta-compressed update
    end
```

**Key Design Decisions:**
- The controller Service has **no selector** -- the leader manages its own EndpointSlice, ensuring only the leader receives pushes
- On leader election, the controller cleans up stale `v1/Endpoints` resources left by previous controller versions to prevent kube-proxy routing to dead pods
- HTTP POST is sent asynchronously with an atomic in-flight guard to prevent ticker drift
- Status collection uses a snapshot-and-release pattern to minimize lock hold time
- A pod informer watches `unbounded-net-node` pods to display pod name, restart count, and age in the dashboard

## State Management

### Controller State

```mermaid
graph TD
    subgraph "Allocator State"
        A1[allocated map<br/>CIDR -> true]
        A2[IPv4 Pools]
        A3[IPv6 Pools]
    end

    subgraph "Site Controller State"
        S1[Sites Cache]
        S2[Node -> Site mapping]
    end

    subgraph "GatewayPool Controller State"
        G1[Pools Cache]
    end

    subgraph "Dashboard State"
        D1[Status Cache<br/>node -> pushed status]
        D2[Pod Informer<br/>unbounded-net-node pods]
        D3[WebSocket Broadcaster<br/>delta-compressed updates]
    end

    A1 --> |"Thread-safe<br/>mutex"| A2
    A1 --> |"Thread-safe<br/>mutex"| A3
    D1 --> |"sync.Map"| D3
```

### Node Agent State

```mermaid
graph TD
    subgraph "wireGuardState"
        W1[peers]
        W2[gatewayPeers]
        W3[remotePeers]
        W4[sitePodCIDRs]

        subgraph "Gateway Tracking"
            G1[gatewayHealthEndpoints]
            G2[gatewayRoutes]
            G3[gatewaySiteCIDRs]
            G4[gatewayPodCIDRs]
            G5[gatewayFailureCounts]
            G6[gatewaySuccessCounts]
            G7[gatewayHealthy]
        end

        subgraph "Pingmesh"
            P1[allNodesForPingmesh]
            P2[pingmeshResults]
        end

        subgraph "Managers"
            M1[linkManager]
            M2[routeManager]
            M3[wireguardManager]
            M4[gatewayLinkManagers]
            M5[gatewayRouteManagers]
            M6[gatewayWireguardManagers]
        end
    end
```

## Security Model

### WireGuard Security

```mermaid
graph LR
    subgraph "Key Management"
        K1[Private Key<br/>/etc/wireguard/server.priv<br/>Mode 0600]
        K2[Public Key<br/>/etc/wireguard/server.pub<br/>Mode 0644]
        K3[Node Annotation<br/>wg-pubkey]
    end

    K1 --> K2
    K2 --> K3

    subgraph "Peer Authentication"
        P1[AllowedIPs restrict source]
        P2[Public key verification]
        P3[No pre-shared keys]
    end
```

> **Unencrypted Tunnel Note:** GENEVE, VXLAN, IPIP, and None tunnels do not provide encryption. They are designed for trusted internal networks where encryption overhead would limit throughput on high-bandwidth links (100Gbps+). The `tunnelProtocol` field's Auto mode ensures that links crossing untrusted boundaries (external IPs) automatically use WireGuard. The security-wins hierarchy rule prevents accidental downgrade to unencrypted tunneling when any scope explicitly requests WireGuard.

### RBAC Model

```mermaid
graph TD
    subgraph "Controller RBAC"
        CR1[Nodes: get, list, watch, patch, update]
        CR2[Sites: get, list, watch + status]
        CR3[SiteNodeSlices: full access]
        CR4[GatewayPools: get, list, watch + status]
        CR5[Leases: full access - leader election]
        CR6[CRDs: get, create]
        CR7[EndpointSlices: get, create, update, delete]
        CR8[Pods: get, list, watch]
        CR9[TokenReviews: create]
        CR10[Endpoints: delete - cleanup stale v1 resources]
        CR11[SitePeerings: get, list, watch + status]
        CR12[SiteGatewayPoolAssignments: get, list, watch]
        CR13[GatewayPoolPeerings: get, list, watch]
        CR14[GatewayPoolNodes: full access]
    end

    subgraph "Node Agent RBAC"
        NR1[Nodes: get, watch, patch]
        NR2[Sites: get, list, watch]
        NR3[SiteNodeSlices: get, list, watch]
        NR4[GatewayPools: get, list, watch]
        NR5[SitePeerings: get, list, watch]
        NR6[SiteGatewayPoolAssignments: get, list, watch]
        NR7[GatewayPoolPeerings: get, list, watch]
        NR8[GatewayPoolNodes: get, list, watch, create, update]
    end
```

### Gateway Node Isolation

```mermaid
graph TD
    GN[Gateway Node] -->|Taint| T1["net.unbounded-cloud.io/gateway-node=true:NoSchedule"]
    T1 -->|Effect| E1[No regular workloads scheduled]
    E1 -->|Reason| R1[Gateway lacks pod CIDR routes<br/>to other sites]

    DaemonSets -->|Tolerate all| GN
```

## Failure Modes and Recovery

### Controller Failure

```mermaid
stateDiagram-v2
    [*] --> Leader: Pod starts
    Leader --> Follower: Loses lease
    Follower --> Leader: Acquires lease
    Leader --> [*]: Pod dies

    note right of Leader: Fresh allocator state<br/>Re-initializes from nodes
    note right of Follower: Health checks pass<br/>Ready for failover
```

### Gateway Failure

```mermaid
stateDiagram-v2
    [*] --> New
    New --> Healthy: N successes
    New --> New: failure (reset count)
    Healthy --> Degraded: 1 failure
    Degraded --> Healthy: Success (reset count)
    Degraded --> Unhealthy: N failures (configurable)
    Unhealthy --> Recovering: 1 success
    Recovering --> Unhealthy: failure (reset count)
    Recovering --> Healthy: N successes

    note right of New: Gateways start unhealthy<br/>Must prove connectivity first
    note right of Unhealthy: Route metrics increased<br/>Unhealthy paths deprioritized<br/>Traffic fails over
    note right of Recovering: Symmetric recovery:<br/>Same successes needed<br/>as failures to go down
```

### Network Partition

```mermaid
graph TD
    subgraph "Site A"
        A1[Node 1]
        A2[Node 2]
        AG[Gateway]
    end

    subgraph "Site B"
        B1[Node 1]
        B2[Node 2]
        BG[Gateway]
    end

    AG ---|Partition| BG

    A1 -->|Still works| A2
    B1 -->|Still works| B2
    A1 -.->|Fails until<br/>recovery| B1
```

## Performance Considerations

### Informer-Based Architecture

All components use Kubernetes informers rather than polling:

- Efficient watch-based updates
- Local caching reduces API server load
- Immediate reaction to changes

### Differential Updates

All configuration managers use differential (delta) updates to minimize disruption:

```mermaid
flowchart LR
    D[Desired State] --> C{Compare}
    A[Actual State] --> C
    C --> Add[Add Missing]
    C --> Remove[Remove Extra]
    C --> Keep[Keep Matching]
```

**Components using delta updates:**

| Component | What it manages | Delta behavior |
|-----------|-----------------|----------------|
| **WireGuard Manager** | Peer configuration | Compares current vs desired peers; only adds new, updates changed, removes stale |
| **Route Manager** | IP routes | Tracks expected routes per interface; only modifies differences |
| **Link Manager** | Network interfaces | Creates/updates interfaces; removes stale gateway interfaces |
| **Masquerade Manager** | iptables NAT rules | Compares current vs desired rules; only adds/removes differences |
| **ECMP Route Manager** | Multi-path routes | Adds/removes gateways from ECMP nexthop groups |
| **BPF Map Reconciler** | LPM trie entries (eBPF mode) | Iterates existing map entries; deletes stale, upserts changed, keeps matching |

This approach ensures:
- No unnecessary interface flapping
- No route table churn during configuration resyncs
- Minimal iptables rule updates
- Preserved WireGuard handshakes for unchanged peers

### ECMP Load Balancing

Multiple gateway interfaces enable kernel-level ECMP load balancing via netlink nexthop groups:

- Routes programmed directly into the kernel using nexthop objects
- Nexthop groups provide ECMP across multiple gateway interfaces
- Kernel automatically distributes traffic per-flow (same flow = same path)

In eBPF dataplane mode, BPF-level ECMP is also available. Each LPM trie entry
supports up to 4 nexthops per CIDR prefix with **HRW (Highest Random Weight)**
consistent hashing. HRW selects a nexthop per 5-tuple flow so that when a
nexthop fails, only flows assigned to that nexthop are rehashed. Each nexthop
has a `TUNNEL_F_HEALTHY` flag updated by the health check system; unhealthy
nexthops are skipped by the BPF program. Healthcheck probes (UDP 9997) are
always forwarded regardless of health state to enable recovery detection.

> **Note:** Policy-based routing (PBR) using connmark/fwmark/ip-rule is
> deprecated. Cross-site transit forwarding now uses per-interface iptables
> FORWARD ACCEPT rules (`iptables -I FORWARD 1 -i <iface> -j ACCEPT`) on
> tunnel and WireGuard gateway interfaces. The `enablePolicyRouting` option
> defaults to `false` and is retained only for backward compatibility.
