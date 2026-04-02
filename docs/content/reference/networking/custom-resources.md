---
title: "Custom Resources"
weight: 2
description: "Specifications for all 7 unbounded-net CRDs."
---

unbounded-net uses seven Custom Resource Definitions (CRDs) in the
`net.unbounded-kube.io` API group. For a conceptual introduction to Sites and
GatewayPools, see [Networking Concepts]({{< relref "concepts/networking" >}}).

## Site

A Site represents a network location containing nodes. Nodes are automatically
assigned to sites based on their internal IP addresses matching the site's
`nodeCidrs`.

### Example

```yaml
apiVersion: net.unbounded-kube.io/v1alpha1
kind: Site
metadata:
  name: site-east
spec:
  nodeCidrs:
    - "10.0.0.0/16"
    - "10.1.0.0/16"
  podCidrAssignments:
    - assignmentEnabled: true
      cidrBlocks:
        - "100.64.0.0/16"
        - "fdde:1::/48"
      nodeBlockSizes:
        ipv4: 24
        ipv6: 80
      nodeRegex:
        - "^worker-.*"
      priority: 100
  manageCniPlugin: true
```

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec.nodeCidrs` | `[]string` | Yes | CIDR blocks containing internal IPs of nodes at this site. |
| `spec.podCidrAssignments` | `[]PodCidrAssignment` | No | Pod CIDR allocation rules for this site. |
| `spec.manageCniPlugin` | `*bool` | No | Controls CNI and WireGuard behavior (default: `true`). |
| `spec.nonMasqueradeCIDRs` | `[]string` | No | CIDRs that should NOT be masqueraded when traffic leaves via the default gateway. |
| `spec.localCidrs` | `[]string` | No | CIDRs considered local; traffic to these is never routed via gateway pools. |
| `spec.healthCheckSettings` | `HealthCheckSettings` | No | Health check settings for node-to-node routes within this site. |
| `spec.tunnelProtocol` | `string` | No | `WireGuard`, `IPIP`, `GENEVE`, `VXLAN`, `None`, or `Auto` (default). |
| `spec.tunnelMTU` | `*int32` | No | Tunnel MTU for routes in this scope (576-9000). |

### PodCidrAssignment Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `assignmentEnabled` | `*bool` | No | Enables this assignment (default: `true`). |
| `cidrBlocks` | `[]string` | No | CIDR pools to allocate from (IPv4 and/or IPv6). |
| `nodeBlockSizes.ipv4` | `int` | No | IPv4 subnet size for node allocations (default: `/24`). |
| `nodeBlockSizes.ipv6` | `int` | No | IPv6 subnet size for node allocations (default: pool prefix + 16). |
| `nodeRegex` | `[]string` | No | Regex patterns to match node names. Empty means no filtering. |
| `priority` | `*int32` | No | Assignment priority; lower values win (default: `100`). |

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `status.nodeCount` | `int` | Number of nodes assigned to this site. |
| `status.sliceCount` | `int` | Number of SiteNodeSlice objects. |

### manageCniPlugin Behavior

| Value | CNI Config | Same-Site WireGuard | Gateway WireGuard |
|-------|------------|---------------------|-------------------|
| `true` (default) | Written | Created | Created |
| `false` | Not written | Not created | Created |

Set to `false` when using an external CNI plugin (e.g., Cilium, Calico) for
intra-site networking while unbounded-net handles only cross-site routing.

### Admission Validation

- `nodeCidrs` must be valid CIDRs and cannot overlap across sites.
- Pod CIDR assignment blocks cannot overlap across sites.
- Pod CIDR mask sizes must be consistent within a site.
- IPv6 masks must be no more than 16 bits larger than the pool prefix.
- `nodeRegex` values must be valid regex patterns.
- Sites with active nodes cannot be deleted.

---

## SiteNodeSlice

Contains a slice of nodes belonging to a site. Each slice holds up to 500 nodes
to prevent exceeding Kubernetes object size limits.

> **Automatically managed by the controller.** Do not create, modify, or delete
> these manually. They are owned by their parent Site and garbage collected when
> the Site is deleted.

### Example

```yaml
apiVersion: net.unbounded-kube.io/v1alpha1
kind: SiteNodeSlice
metadata:
  name: site-east-0
  ownerReferences:
    - apiVersion: net.unbounded-kube.io/v1alpha1
      kind: Site
      name: site-east
siteName: site-east
sliceIndex: 0
nodeCount: 2
nodes:
  - name: node-001
    wireGuardPublicKey: "abc123...="
    internalIPs: ["10.0.1.5"]
    podCIDRs: ["100.64.0.0/24", "fdde:1::100/80"]
  - name: node-002
    wireGuardPublicKey: "def456...="
    internalIPs: ["10.0.1.6"]
    podCIDRs: ["100.64.1.0/24"]
```

### Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `siteName` | `string` | Yes | Parent Site name. |
| `sliceIndex` | `int` | Yes | Zero-based index of this slice. |
| `nodeCount` | `int` | No | Number of nodes in this slice. |
| `nodes[].name` | `string` | Yes | Node name. |
| `nodes[].wireGuardPublicKey` | `string` | No | WireGuard public key. |
| `nodes[].internalIPs` | `[]string` | No | Internal IP addresses. |
| `nodes[].podCIDRs` | `[]string` | No | Pod CIDRs assigned to this node. |

Deletion is blocked while a slice still references active nodes.

---

## GatewayPool

Defines a pool of gateway nodes selected by labels. Gateway nodes route traffic
between sites.

### Example

```yaml
apiVersion: net.unbounded-kube.io/v1alpha1
kind: GatewayPool
metadata:
  name: main-gateways
spec:
  type: External
  nodeSelector:
    net.unbounded-kube.io/gateway: "true"
  routedCidrs:
    - "172.16.0.0/12"
  healthCheckSettings:
    enabled: true
  tunnelProtocol: Auto
```

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec.type` | `string` | No | `External` (default) or `Internal`. Controls IP resolution for cross-site connections. |
| `spec.nodeSelector` | `map[string]string` | Yes | Label selector for gateway nodes. |
| `spec.routedCidrs` | `[]string` | No | Additional CIDRs routed through this pool. |
| `spec.healthCheckSettings` | `HealthCheckSettings` | No | Health check settings for routes to peers. |
| `spec.tunnelProtocol` | `string` | No | Tunnel encapsulation (default: `Auto`). |
| `spec.tunnelMTU` | `*int32` | No | Tunnel MTU (576-9000). |

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `status.nodeCount` | `int` | Number of nodes in the pool. |
| `status.connectedSites` | `[]string` | Sites directly connected via peering. |
| `status.reachableSites` | `[]string` | All reachable sites (including transitive). |
| `status.nodes[].name` | `string` | Node name. |
| `status.nodes[].siteName` | `string` | Node's site. |
| `status.nodes[].internalIPs` | `[]string` | Internal IPs. |
| `status.nodes[].externalIPs` | `[]string` | External IPs. |
| `status.nodes[].healthEndpoints` | `[]string` | Health check IPs. |
| `status.nodes[].wireGuardPublicKey` | `string` | WireGuard public key. |
| `status.nodes[].gatewayWireguardPort` | `int32` | WireGuard listen port (assigned by controller, starting at 51821). |
| `status.nodes[].podCIDRs` | `[]string` | Pod CIDRs. |

### Gateway Node Requirements

A node is included in the pool status when it:

1. Matches the `nodeSelector` labels.
2. Has a WireGuard public key annotation.

### Gateway Peer IP Resolution

| Condition | IP Used |
|-----------|---------|
| Same site | Internal IP |
| Sites are network-peered (SitePeering) | Internal IP |
| Pool type is External, different site | External IP |
| Pool type is Internal, different site | No endpoint (WireGuard learns) |

Deletion is blocked while a pool has active matching nodes.

---

## GatewayPoolNode

Represents an individual node's membership in a gateway pool. Automatically
created by the controller; the node agent patches status with route
advertisements and heartbeat timestamps.

> **Automatically managed.** Do not create or modify manually.

### Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec.nodeName` | `string` | Yes | Kubernetes node name. |
| `spec.gatewayPool` | `string` | Yes | Parent GatewayPool. |
| `spec.site` | `string` | No | Site name (from node labels). |
| `status.lastUpdated` | `string` | -- | Last heartbeat timestamp. |
| `status.routes` | `map[string]GatewayNodeRoute` | -- | Route advertisements keyed by CIDR. |

---

## SitePeering

Defines direct peering between sites. When `meshNodes` is `true` (default),
nodes in peered sites create direct WireGuard tunnels, bypassing gateways.

### Example

```yaml
apiVersion: net.unbounded-kube.io/v1alpha1
kind: SitePeering
metadata:
  name: east-west-peering
spec:
  sites:
    - "site-east"
    - "site-west"
    - "site-central"
  meshNodes: true
  healthCheckSettings:
    enabled: true
    detectMultiplier: 3
    receiveInterval: 300ms
    transmitInterval: 300ms
  tunnelProtocol: Auto
```

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec.enabled` | `*bool` | No | Whether active (default: `true`). |
| `spec.sites` | `[]string` | No | Sites to peer (min 2). |
| `spec.meshNodes` | `*bool` | No | Direct node-to-node tunnels (default: `true`). When `false`, sites are network-peered but nodes don't mesh directly. |
| `spec.healthCheckSettings` | `HealthCheckSettings` | No | Health check settings for inter-site routes. |
| `spec.tunnelProtocol` | `string` | No | Tunnel encapsulation (default: `Auto`). |
| `spec.tunnelMTU` | `*int32` | No | Tunnel MTU (576-9000). |

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `status.peeredSiteCount` | `int` | Number of peered sites that exist. |
| `status.totalNodeCount` | `int` | Total nodes across peered sites. |

### Behavior

| meshNodes | Effect |
|-----------|--------|
| `true` (default) | Direct node-to-node WireGuard tunnels. Traffic bypasses gateways. Pod CIDRs from all peered sites combined in route table. |
| `false` | No direct tunnels. Gateway pool peerings between these sites use internal IPs instead of external IPs. Combine with `tunnelProtocol: None` for externally-peered sites. |

### Use Cases

- **Low-latency cross-site**: Sites with fast connectivity (same cloud region,
  dedicated interconnect). Use `meshNodes: true` for direct WireGuard tunnels.

- **Externally peered sites (ExpressRoute, Direct Connect, VPN)**: Two sites
  already connected over a private L3 link can be merged into one logical
  network. Use `meshNodes: false` and `tunnelProtocol: None` to route pod
  traffic directly over the existing connection with zero encapsulation overhead.
  WireGuard is still available for any other site pairs in the same cluster.

  ```yaml
  spec:
    sites:
      - site-on-prem
      - site-azure
    meshNodes: false
    tunnelProtocol: None
  ```

- **Large sites without full mesh**: Use `meshNodes: false` to avoid O(n²)
  tunnels while still benefiting from internal IP routing via gateways.

---

## SiteGatewayPoolAssignment

Defines which gateway pools serve which sites.

### Example

```yaml
apiVersion: net.unbounded-kube.io/v1alpha1
kind: SiteGatewayPoolAssignment
metadata:
  name: east-gateways
spec:
  sites:
    - "site-east"
  gatewayPools:
    - "main-gateways"
    - "backup-gateways"
  healthCheckSettings:
    enabled: true
  tunnelProtocol: Auto
```

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec.enabled` | `*bool` | No | Whether active (default: `true`). |
| `spec.sites` | `[]string` | No | Sites to assign. |
| `spec.gatewayPools` | `[]string` | No | Gateway pools to assign. |
| `spec.healthCheckSettings` | `HealthCheckSettings` | No | Health check settings for site-to-pool routes. |
| `spec.tunnelProtocol` | `string` | No | Overrides Site's `tunnelProtocol` for gateway peer connections. Falls back to Site's setting when `Auto` or nil. |
| `spec.tunnelMTU` | `*int32` | No | Tunnel MTU (576-9000). |

---

## GatewayPoolPeering

Defines peering between gateway pools, enabling cross-pool routing.

### Example

```yaml
apiVersion: net.unbounded-kube.io/v1alpha1
kind: GatewayPoolPeering
metadata:
  name: east-west-pool-peering
spec:
  gatewayPools:
    - "east-gateways"
    - "west-gateways"
  healthCheckSettings:
    enabled: true
  tunnelProtocol: Auto
```

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec.enabled` | `*bool` | No | Whether active (default: `true`). |
| `spec.gatewayPools` | `[]string` | No | Pools to peer. |
| `spec.healthCheckSettings` | `HealthCheckSettings` | No | Health check settings. |
| `spec.tunnelProtocol` | `string` | No | Tunnel encapsulation (default: `Auto`). |
| `spec.tunnelMTU` | `*int32` | No | Tunnel MTU (576-9000). |

---

## HealthCheckSettings

The `healthCheckSettings` object is shared across Site, SitePeering,
GatewayPool, SiteGatewayPoolAssignment, and GatewayPoolPeering:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | `*bool` | `true` | Enable health checks for routes in this scope. |
| `detectMultiplier` | `*int32` | 3 | Number of consecutive failures before marking unhealthy. |
| `receiveInterval` | `string` | `300ms` | Expected interval between received probes. |
| `transmitInterval` | `string` | `300ms` | Interval between sent probes. |

### Precedence

- **Same-site routes**: `Site.spec.healthCheckSettings`
- **Peered-site routes**: `SitePeering.spec.healthCheckSettings`
- **Gateway pool routes**: `SiteGatewayPoolAssignment.spec.healthCheckSettings`
  → `GatewayPool.spec.healthCheckSettings` → `Site.spec.healthCheckSettings`

If multiple peerings define conflicting settings, peerings are processed in
deterministic name order and the first profile is kept.

---

## Labels, Annotations, and Taints

### Labels

| Label | Applied To | Description |
|-------|-----------|-------------|
| `net.unbounded-kube.io/site` | Node | Site membership (set by controller). |

### Annotations

| Annotation | Applied To | Description |
|------------|-----------|-------------|
| `net.unbounded-kube.io/wg-pubkey` | Node | WireGuard public key (set by node agent). |

### Taints

| Taint | Effect | Description |
|-------|--------|-------------|
| `net.unbounded-kube.io/gateway-node=true` | NoSchedule | Prevents regular workloads on gateway nodes. |

---

## Short Names

| Resource | Short Name | Example |
|----------|------------|---------|
| Site | `st` | `kubectl get st` |
| SiteNodeSlice | `sns` | `kubectl get sns` |
| GatewayPool | `gp` | `kubectl get gp` |
| GatewayPoolNode | `gpn` | `kubectl get gpn` |
| SitePeering | `spr` | `kubectl get spr` |
| SiteGatewayPoolAssignment | `sgpa` | `kubectl get sgpa` |
| GatewayPoolPeering | `gpp` | `kubectl get gpp` |

---

## CRD Relationships

```
  Site ──owns──→ SiteNodeSlice (max 500 nodes per slice)
  Site ──contains──→ Node (matched by nodeCidrs)
  GatewayPool ──selects──→ Node (matched by nodeSelector)
  GatewayPool ──contains──→ GatewayPoolNode
  SitePeering ──peers──→ Site (2+ sites)
  SiteGatewayPoolAssignment ──assigns──→ Site + GatewayPool
  GatewayPoolPeering ──peers──→ GatewayPool (2+ pools)
```

## Next Steps

- **[Architecture]({{< relref "reference/networking/architecture" >}})** --
  System internals and data flows.
- **[Configuration]({{< relref "reference/networking/configuration" >}})** --
  All flags and tuning options.
- **[Operations]({{< relref "reference/networking/operations" >}})** --
  Deployment and troubleshooting.
