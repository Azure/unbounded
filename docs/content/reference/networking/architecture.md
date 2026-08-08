---
title: "Architecture"
weight: 1
description: "Deep dive into unbounded-net system design, dataplane, data flows, and security."
---

This document provides a detailed overview of unbounded-net's architecture,
components, and their interactions. For a conceptual introduction, see the
[Networking Concepts]({{< relref "concepts/networking" >}}) page.

## System Overview

unbounded-net provides seamless pod-to-pod networking across multiple Kubernetes
sites using encrypted (WireGuard) or unencrypted (GENEVE, VXLAN, IPIP) tunnels.
The encapsulation type is selected per link scope and can be set explicitly or
resolved automatically based on link characteristics.

The eBPF dataplane uses a TC egress BPF program and LPM trie maps for tunnel
endpoint resolution.

### High-Level Architecture

![unbounded-net high-level architecture: Control Plane with Site and GatewayPool controllers, Custom Resources tier, and per-Node Data Plane with eBPF, WireGuard, and gateway health monitoring](../../../img/networking-architecture-overview.svg)

## Component Details

### Controller (`unbounded-net-controller`)

The controller runs as a Deployment with 2 replicas for high availability. Only
the leader actively reconciles resources.

#### Pod CIDR Assignment

Pod CIDRs are assigned by the Site controller based on per-site assignment
rules:

1. When a node is created or updated, the controller checks if it already has
   pod CIDRs.
2. If not, it finds the matching Site based on the node's internal IP.
3. It selects an assignment by priority and node name regex.
4. A **fresh API fetch** is performed before allocation to prevent
   double-allocation from stale informer cache.
5. The controller allocates from the assignment's CIDR pools and patches the
   node.
6. If a CIDR pool is exhausted, the controller exits fatally to surface the
   problem immediately.

#### Site Controller

Manages site membership and SiteNodeSlice objects:

- Matches node internal IPs against Site `nodeCidrs`.
- Labels matching nodes with `unbounded-cloud.io/site=<name>`.
- Splits site nodes into **SiteNodeSlice** objects (max 500 nodes per slice to
  avoid etcd object size limits).
- Slices are named `{site-name}-{index}` with OwnerReferences for automatic
  garbage collection.

#### GatewayPool Controller

Maintains gateway node information:

- Lists nodes matching the pool's label selector.
- Filters for nodes with external IPs and WireGuard keys.
- Calculates health endpoints.
- Updates pool status with gateway node details.

### Node Agent (`unbounded-net-node`)

The node agent runs as a DaemonSet on every node, including control plane nodes.
It manages:

- WireGuard interfaces and key management
- Tunnel configuration
- eBPF program lifecycle and BPF map reconciliation
- Route programming
- CNI bridge configuration
- Gateway health monitoring

#### Initialization Flow

1. Check for existing WireGuard keys on the filesystem; generate if missing.
2. Annotate the node with the WireGuard public key.
3. Wait for podCIDRs to be assigned by the controller.
4. Write CNI configuration atomically.
5. Start watching Site, SiteNodeSlice, and GatewayPool resources.
6. Configure WireGuard and tunnel interfaces.

#### WireGuard Interface Architecture

**Regular nodes** use:
- `wg51820` (port 51820) for intra-site mesh peers.
- `wg51821`, `wg51822`, ... for dedicated gateway connections (one interface
  per gateway peer).

**Gateway nodes** use:
- `wg51820` for all peers (same-site and remote). Remote peers have no
  endpoint set -- WireGuard learns their address from incoming packets.

## Running with an Existing CNI

unbounded-net can run alongside a CNI you already operate in the primary cluster
(for example Cilium, Calico, or Azure CNI). In this mode the existing CNI keeps
ownership of intra-site pod networking, and unbounded-net is responsible only for
connecting remote sites back to the primary cluster. This is controlled per-Site
with the `manageCniPlugin` field.

### Configuring the Primary Site

For the Site that maps to your existing cluster, set `manageCniPlugin: false`.
This automatically disables pod CIDR assignment for that site regardless of the
`assignmentEnabled` value on individual `podCidrAssignments` rules. The
`podCidrAssignments` block is still required: it defines the CIDR ranges that the
other sites and gateways use to route traffic back to the primary cluster's pods.

```yaml
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: Site
metadata:
  name: primary
spec:
  nodeCidrs:
    - "10.224.0.0/16"
  # CNI already runs in this cluster, so unbounded-net does not write CNI
  # config or mesh same-site nodes, and pod CIDR assignment is disabled.
  manageCniPlugin: false
  podCidrAssignments:
      # Pod CIDR assignment is disabled by manageCniPlugin: false, but the CIDR
      # range is still declared so other sites can route to these pods.
    - cidrBlocks:
        - "10.244.0.0/16"
```

When initializing the site with the CLI, pass `--manage-cni-plugin=false` so the
generated Site resource is configured the same way. See the
[Bring Your Own Cluster]({{< relref "guides/existing-cluster" >}}) guide for the
end-to-end workflow.

### Division of Responsibilities

| Concern | Existing CNI | unbounded-net |
|---------|:------------:|:-------------:|
| Pod IPAM and pod CIDR allocation in the primary cluster | ✓ | |
| CNI configuration on primary-site nodes | ✓ | |
| Intra-site pod-to-pod networking within the primary cluster | ✓ | |
| Cross-site routing to and from remote sites | | ✓ |
| Gateway WireGuard tunnels between sites | | ✓ |
| Pod CIDR allocation and CNI config on remote sites | | ✓ |

The existing CNI continues to handle everything inside the primary cluster:
allocating pod IPs, writing its own CNI configuration, and forwarding pod-to-pod
traffic between local nodes. unbounded-net does not write CNI config, mesh
same-site peers, or allocate pod CIDRs for that site.

unbounded-net handles only inter-site connectivity for the primary site: it
brings up gateway WireGuard tunnels to other sites and programs the routes that
carry traffic to and from remote pod CIDRs. Remote sites created by Unbounded are
unaffected; on those sites unbounded-net continues to act as the CNI, allocate
pod CIDRs, and mesh same-site nodes as usual.

{{< callout type="note" >}}
**Cilium CNI:** Remote Unbounded nodes do not run Cilium, so they have no Cilium
identity. Cilium derives identities from pod labels for in-cluster endpoints, but
remote pods are unknown to it. Any CiliumNetworkPolicy that needs to allow or deny
traffic to or from remote sites must reference those pods by IP/CIDR (for example
`toCIDR`/`fromCIDR`) rather than by identity or label selectors.
{{< /callout >}}

## Encapsulation Types

| Type | Overhead | Encrypted | Default For |
|------|----------|-----------|-------------|
| **WireGuard** | 80 bytes | Yes | External/public IP links |
| **GENEVE** | 58 bytes | No | Internal/private IP links |
| **VXLAN** | ~58 bytes | No | Links with NIC hardware offload |
| **IPIP** | 20 bytes | No | Minimal overhead links |
| **None** | 0 bytes | No | Direct L3 routing |

The `tunnelProtocol` field on CRD specs controls selection. When set to `Auto`
(default), the system chooses based on link characteristics. The
**security-wins rule** ensures that if any scope explicitly sets `WireGuard`,
the link uses WireGuard regardless of other scopes.

See [Routing Flows]({{< relref "reference/networking/routing-flows" >}}) for
the full protocol selection algorithm.

## Tunnel Dataplane

Uses a single dummy device (`unbounded0`) as a routing anchor, a TC egress BPF
program for tunnel endpoint resolution, and shared flow-based tunnel interfaces.

**Key interfaces:**

| Interface | Purpose |
|-----------|---------|
| `unbounded0` | Dummy (NOARP) device. Holds overlay IPs and supernet routes. TC egress BPF attached here. |
| `geneve0` | Flow-based GENEVE. BPF sets remote endpoint and VNI via `bpf_skb_set_tunnel_key()`. |
| `vxlan0` | Flow-based VXLAN. Same metadata-driven approach as GENEVE. |
| `ipip0` | Flow-based IP-in-IP. Tunnel key sets the remote endpoint. |
| `wg<port>` | WireGuard. BPF performs redirect-only (no tunnel key). Crypto routing handled by WireGuard AllowedIPs. |

#### BPF LPM Trie Map

A single unified trie stores overlay CIDR to tunnel endpoint mappings:

| Map | Key Size | Description |
|-----|----------|-------------|
| `unb_endpts` | 20 bytes | 16-byte address + 4-byte prefixlen; IPv4 entries use IPv4-mapped IPv6 form |

`BPF_MAP_TYPE_LPM_TRIE` with a default max of 16,384 entries. IPv4 destinations
are stored as `::ffff:<v4>` with prefixlen offset by 96, so the trie's
longest-prefix-match naturally segregates v4 from native v6 entries.

Each nexthop contains:
- **Remote endpoint** (16 bytes): underlay address, IPv4-mapped for v4 underlays.
- **Interface index**: target tunnel interface for `bpf_redirect()`.
- **Healthy** (u32): non-zero when eligible; only consulted by multi-nexthop
  ECMP (gateway pools).
- **Protocol**: GENEVE=1, VXLAN=2, IPIP=3, WireGuard=4, None=5. The BPF
  program derives whether to call `bpf_skb_set_tunnel_key` (GENEVE/VXLAN/IPIP)
  from this field.

#### Deterministic MAC Derivation

Both the BPF program and Go code derive inner Ethernet destination MACs
deterministically from the destination IP:

- **IPv4**: `02:<ip[0]>:<ip[1]>:<ip[2]>:<ip[3]>:FF`
- **IPv6**: `02:<last 4 bytes>:FF`

This avoids ARP/ND on tunnel interfaces.

#### BPF Map Reconciliation

Entries are not applied incrementally. Each protocol appends entries to a
pending map, then a single `Reconcile()` call atomically deletes stale entries
and upserts desired entries, keeping IPv4 and IPv6 tries synchronized.

## Data Flows

### Same-Site Pod-to-Pod (GENEVE)

![Same-site pod-to-pod flow: Pod A through cbr0, kernel routing, unbounded0, TC egress BPF with LPM lookup, GENEVE encap over LAN to Pod B](../../../img/networking-same-site-flow.svg)

### Cross-Site Pod-to-Pod (via Gateway, WireGuard)

![Cross-site pod-to-pod flow via gateway: Pod A on Site 1 Worker through WireGuard to Site 1 Gateway, WAN to Site 2 Gateway, GENEVE to Site 2 Worker, delivered to Pod B](../../../img/networking-cross-site-flow.svg)

### Gateway Health Checks

Node agents send UDP probes over WireGuard tunnels at configurable intervals
(default 1s). Failure detection uses `detectMultiplier * max(transmitInterval,
receiveInterval)`. On failure, route metrics are increased to deprioritize
unhealthy paths. On recovery, metrics are restored.

### Status Aggregation

Node agents push status to the controller every 10 seconds (gzip JSON, Bearer
token auth). The controller caches status and broadcasts delta-compressed
updates to WebSocket dashboard clients every 3 seconds.

## Security Model

### WireGuard Key Management

- Private keys stored at `/etc/wireguard/server.priv` (mode 0600).
- Public keys published as node annotations.
- Peer authentication via AllowedIPs and public key verification.
- No pre-shared keys used.

### Unencrypted Tunnel Note

GENEVE, VXLAN, IPIP, and None tunnels provide no encryption. They are designed
for trusted internal networks. The `Auto` mode ensures links crossing untrusted
boundaries (external IPs) automatically use WireGuard. The security-wins
hierarchy prevents accidental downgrade.

### RBAC

**Controller** requires: Nodes (get/list/watch/patch/update), Sites and
GatewayPools (get/list/watch + status), SiteNodeSlices and GatewayPoolNodes
(full access), Leases (leader election), CRDs (get/create), EndpointSlices
(CRUD), TokenReviews (create).

**Node Agent** requires: Nodes (get/watch/patch), Sites/SiteNodeSlices/
GatewayPools/SitePeerings/SiteGatewayPoolAssignments/GatewayPoolPeerings
(get/list/watch), GatewayPoolNodes (get/list/watch/create/update).

### Gateway Node Isolation

Gateway nodes are tainted with
`net.unbounded-cloud.io/gateway-node=true:NoSchedule` to prevent regular
workloads from being scheduled on them, since gateways lack pod CIDR routes to
other sites.

## Failure Modes

### Controller Failure

The controller uses leader election. On failover, the new leader re-initializes
allocator state from existing node objects. Follower replicas pass health checks
and are ready for immediate failover.

### Gateway Failure

Gateways start in a "New" state and must prove connectivity before being marked
healthy. The state machine:

![Gateway health state machine: New to Healthy (N successes), Healthy to Degraded (1 failure), Degraded to Unhealthy (N failures) with routes deprioritized, Unhealthy to Recovering (1 success), Recovering to Healthy (N successes)](../../../img/networking-gateway-health.svg)

Recovery requires the same number of successes as failures to go down
(symmetric recovery).

### Network Partition

Intra-site traffic continues to work. Cross-site traffic fails until the
partition heals. ECMP across multiple gateways provides resilience against
single-gateway failures.

## Performance Considerations

- **Informer-based architecture**: Watch-based updates with local caching.
- **Differential updates**: All managers (WireGuard, routes, links, BPF maps,
  masquerade, ECMP) use delta reconciliation -- only adding, updating, or
  removing what changed.
- **ECMP load balancing**: Multiple gateway interfaces enable kernel-level
  ECMP via netlink nexthop groups.

## Next Steps

- **[Custom Resources]({{< relref "reference/networking/custom-resources" >}})** --
  Full CRD specifications.
- **[Configuration]({{< relref "reference/networking/configuration" >}})** --
  All flags and settings.
- **[Routing Flows]({{< relref "reference/networking/routing-flows" >}})** --
  Packet-level routing details.
- **[Operations]({{< relref "reference/networking/operations" >}})** --
  Deployment and troubleshooting.
