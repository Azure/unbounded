---
title: "Custom Resources"
weight: 2
description: "Field-level reference for the Site and net.unbounded-cloud.io custom resources that define sites, gateway pools, peerings, and health checks."
---

Unbounded networking is declarative. You describe where your nodes are, which nodes act as gateways, and which locations or gateway pools may exchange traffic. Controllers and node agents reconcile the data plane from those objects. This page is the field-level reference for the custom resources involved, with complete manifests and the defaults and validation rules that apply to each one.

The mental model is small. A `Site` is a network location and its address plan. A `GatewayPool` is a labeled set of nodes that forward traffic between locations. `SiteGatewayPoolAssignment` attaches sites to pools, `SitePeering` meshes sites directly, and `GatewayPoolPeering` links pools together. Two objects are observational: `SiteNodeSlice` is a controller-managed view of the worker nodes in a site, and `GatewayPoolNode` carries a gateway node's route advertisements and heartbeat. `HealthCheckSettings` is not a resource of its own; it is embedded in the other objects.

Read the pages in this order if you are new to the model: [Networking concepts]({{< relref "concepts/networking" >}}) for the problem being solved, this page for the objects and fields, and [Networking architecture]({{< relref "reference/networking/architecture" >}}) for how the controllers and agents fit together.

## API Groups

Unbounded networking spans two API groups:

- `unbounded-cloud.io/v1alpha3` contains `Site`. The group is shared with `machina`, `Metalman`, and the operator because a Site also carries the optional component configuration for that location.
- `net.unbounded-cloud.io/v1alpha1` contains `SiteNodeSlice`, `GatewayPool`, `GatewayPoolNode`, `SitePeering`, `SiteGatewayPoolAssignment`, and `GatewayPoolPeering`.

All seven resources are cluster-scoped. Names are global across the cluster, and a Site or GatewayPool name is often used verbatim inside another object. That is why references such as `spec.sites` and `spec.gatewayPools` are plain strings rather than namespace-qualified names.

Both groups are alpha APIs. Fields may change between releases, and the CRDs currently expose a single served version with no conversion webhook. Upgrade the CRDs before upgrading the controllers or node agents so that fields added to a status object are preserved by the API server. The [Machina custom resource reference]({{< relref "reference/machina-crd" >}}) covers the rest of the `unbounded-cloud.io` group.

The `net.unbounded-cloud.io` CRDs share a validating admission webhook for `CREATE` and `UPDATE`. The webhook checks CIDR syntax, cross-site overlap, mask consistency, regular expressions, and cross-object references. A separate set of validating admission policies protects Node labels, annotations, taints, and pod CIDRs from the service accounts that are allowed to modify them. You do not create or manage those policies yourself; they are installed with the controller manifests in `deploy/net`.

## Site

A `Site` declares a network location. It contains the CIDR blocks that hold the internal addresses of nodes at that location, the pod address pools those nodes may allocate from, and the defaults that govern tunnels, masquerading, and CNI ownership. It is the anchor object for every other networking resource.

Nodes are not listed in a Site. The Site controller watches Nodes and compares every address of type `InternalIP` against each Site's `spec.nodeCidrs`. A node whose internal address falls inside a Site's CIDR is assigned to that Site. The controller then labels the Node with `unbounded-cloud.io/site` and dual-writes the deprecated `net.unbounded-cloud.io/site` key. A node with several internal addresses matches if any one of them is inside the CIDR. A node with no internal address, or no matching Site, remains unassigned and is not placed in a SiteNodeSlice.

Because assignment is based on the node's underlay address, `spec.nodeCidrs` is not the pod network. It is the network in which the node itself lives: a VPC subnet, a data center VLAN, or an edge network. Pod addresses come from `spec.podCidrAssignments`, described below.

The following Site sets all networking fields and shows the shape of the status:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Site
metadata:
  name: site-east
spec:
  nodeCidrs:
    - "10.10.0.0/16"
    - "fd00:10:10::/48"
  podCidrAssignments:
    - assignmentEnabled: true
      cidrBlocks:
        - "100.64.0.0/16"
        - "fd00:64:1::/56"
      nodeBlockSizes:
        ipv4: 24
        ipv6: 72
      nodeRegex:
        - "^worker-.*"
      priority: 100
  manageCniPlugin: true
  nonMasqueradeCIDRs:
    - "172.16.0.0/12"
  localCidrs:
    - "192.168.0.0/16"
  healthCheckSettings:
    enabled: true
    detectMultiplier: 3
    receiveInterval: "15s"
    transmitInterval: "15s"
  tunnelProtocol: Auto
  tunnelMTU: 1420
status:
  nodeCount: 12
  sliceCount: 1
```

### Spec Fields

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `spec.nodeCidrs` | `[]string` | Yes | None | CIDR blocks that contain the internal IP addresses of nodes at this site. At least one entry is required. |
| `spec.podCidrAssignments` | `[]PodCidrAssignment` | Yes | None | Pod CIDR allocation rules for nodes at this site. At least one entry is required, even when allocation is disabled, because the blocks define inter-site pod routes. |
| `spec.manageCniPlugin` | `*bool` | No | `true` | Controls whether the node agent writes CNI configuration and creates same-site tunnel peers. |
| `spec.nonMasqueradeCIDRs` | `[]string` | No | None | CIDRs that must not be masqueraded when traffic leaves through the default gateway. |
| `spec.localCidrs` | `[]string` | No | None | CIDRs considered local to the site. Traffic to these destinations is not routed through gateway pools. |
| `spec.healthCheckSettings` | `HealthCheckSettings` | No | Agent defaults | Health check parameters for same-site node routes. |
| `spec.tunnelProtocol` | `*string` | No | `Auto` | Encapsulation for this site's tunnel scopes. Valid values are `WireGuard`, `IPIP`, `GENEVE`, `VXLAN`, `None`, and `Auto`. |
| `spec.tunnelMTU` | `*int32` | No | None | MTU for routes through tunnels in this scope. Range is 576 to 9000. |
| `spec.components` | `SiteComponents` | No | None | Optional Unbounded components managed for the site. This is outside networking and is covered in [Workload overrides]({{< relref "reference/workload-overrides" >}}). |

The `nonMasqueradeCIDRs` field preserves the source address of pods for destinations that need to see it, such as a corporate network or a VPN peer. Traffic through tunnels and traffic to pod and node CIDRs is never masqueraded regardless of this field. On Azure nodes, the network interface must have IP forwarding enabled for this setting to take effect.

The `localCidrs` field is an exclusion list for gateway routing. If a local CIDR sits inside a broader prefix that a gateway advertises, the agent splits the prefix so the rest of it still uses the gateway while the local portion stays on the local network. Local CIDRs apply to learned gateway routes, tunnel allowed IPs, and aggregate overlay routes.

### PodCidrAssignment Fields

Each entry in `spec.podCidrAssignments` is a rule. The controller evaluates the rules in order of priority and gives each matching node a block from the selected rule's pools.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `assignmentEnabled` | `*bool` | No | Inherits `spec.manageCniPlugin` | Activates this rule. An explicit `true` or `false` overrides the inherited value. |
| `cidrBlocks` | `[]string` | Yes | None | CIDR pools to allocate from. IPv4 and IPv6 may be mixed. At least one entry is required. |
| `nodeBlockSizes` | `NodeBlockSizes` | No | See below | Per-node allocation sizes for each address family. |
| `nodeBlockSizes.ipv4` | `int` | No | `24` when IPv4 pools exist | IPv4 subnet mask for each node. Must be between 16 and 28. |
| `nodeBlockSizes.ipv6` | `int` | No | First IPv6 pool prefix plus 16, capped at 128 | IPv6 subnet mask for each node. Must be no more than 16 bits longer than the pool prefix. |
| `nodeRegex` | `[]string` | No | Empty matches every node | Regular expressions matched against the Node name. A node matches if any pattern matches. |
| `priority` | `*int32` | No | `100` | Rule ordering. Lower values win. Equal priorities are broken by the order of entries in the list, with the earlier entry winning. |

Every enabled assignment in a Site must use the same IPv4 mask and the same IPv6 mask. The admission webhook rejects a Site whose enabled rules disagree, because a node that matched two rules would otherwise receive differently sized blocks. A rule with an explicit `assignmentEnabled: false` is exempt from the mask checks and from the cross-site pool overlap check. A rule that is disabled only by inheritance is still included in the overlap scan, because its CIDR blocks remain part of the Site's declared pod ranges.

An empty `nodeRegex` is the common case: it makes the rule the default for every node in the Site. Use a regular expression when you need separate pools for different node classes, for example a larger IPv4 block for system nodes and a smaller one for batch workers. The patterns use Go regular expression syntax and are anchored only where you anchor them, so `^worker-` is usually clearer than `worker-`.

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `status.nodeCount` | `int` | Number of nodes assigned to this site, including gateway nodes. |
| `status.sliceCount` | `int` | Number of `SiteNodeSlice` objects owned by this site. Gateway nodes are excluded from slices, so this count covers worker nodes only. |
| `status.conditions` | `[]metav1.Condition` | Component readiness conditions published by the operator, for example `NetReady` or `MachinaReady`. One condition is published per component. |
| `status.overrides` | `OverrideStatus` | Summary of workload overrides that target this site's components. Present only when an override applies. |

`status.nodeCount` and `status.sliceCount` can differ even when the Site has nodes. A Site with three gateway nodes and no workers reports `nodeCount: 3` and `sliceCount: 0`. The count difference is expected and is not an error.

### manageCniPlugin Behavior

`spec.manageCniPlugin` decides who owns intra-site pod networking. When it is `true` or omitted, the node agent writes the CNI configuration, manages the local bridge, and treats every same-site node as a tunnel peer. When it is `false`, the agent leaves CNI configuration alone and creates only the tunnel links needed to reach gateway pools in other sites. The same-site mesh is skipped so that an external CNI plugin can own pod-to-pod delivery inside the location.

| Value | CNI configuration | Same-site tunnel peers | Omitted `assignmentEnabled` | Gateway links |
|-------|-------------------|------------------------|----------------------------|---------------|
| `true` (default) | Written and managed by the node agent | Created | Resolves to `true` | Created for other sites |
| `false` | Left untouched | Not created | Resolves to `false` | Created for other sites |

The inherited value is resolved when an assignment is selected. It is not written back into the object, so changing `spec.manageCniPlugin` changes the effective setting for every rule that omits `assignmentEnabled`. Explicit values on individual rules always win: setting `assignmentEnabled: false` disables a rule even under a managed CNI, and setting `assignmentEnabled: true` keeps a rule active under an external CNI.

`spec.podCidrAssignments` remains required in both modes. The CIDR blocks are the pod address ranges advertised across sites, so the controller needs them even when it is not allocating blocks itself. If an external CNI owns IP allocation, set `assignmentEnabled: false` and point `cidrBlocks` at the pool that the external CNI actually uses. If you want the controller to allocate Node `podCIDRs` while the external CNI handles forwarding, set `assignmentEnabled: true` and configure the external plugin to consume the assigned CIDRs.

The node agent reads `manageCniPlugin` at startup and keeps that mode for the lifetime of the process. Changing the field on a running Site does not switch an agent from managed to unmanaged CNI or back. Restart the `unbounded-net-node` Pods after the change so each agent adopts the new mode.

A Site with `manageCniPlugin: false` can coexist with any CNI that gives pods addresses from the pools declared in `podCidrAssignments`. The agent still needs the node's pod CIDRs to compute gateway health addresses and to build routes to other sites. If the external CNI does not write `spec.podCIDRs` on the Node, the agent attempts delegated IPAM and reports the assignment in the node health status.

### Admission Validation

The validating webhook enforces the following rules for `Site` objects. The same checks run when a Site is created or updated, and a rejection returns a message naming the field at fault.

- Every entry in `spec.nodeCidrs`, `spec.nonMasqueradeCIDRs`, and `spec.localCidrs` must be a valid CIDR.
- `spec.nonMasqueradeCIDRs` may not contain overlapping entries, `spec.localCidrs` may not contain overlapping entries, and no entry in one list may overlap an entry in the other.
- Node CIDRs may not overlap across different Sites. Overlap within a single Site is allowed, which lets one Site list several CIDRs that cover the same address space.
- Pod CIDR pools from enabled assignments may not overlap across Sites, and they may not overlap each other, including within the same Site. Assignments with an explicit `assignmentEnabled: false` are excluded from this check.
- `nodeBlockSizes.ipv4` must resolve to a mask between `/16` and `/28`. `nodeBlockSizes.ipv6` must be no more than 16 bits longer than every IPv6 pool it allocates from. All enabled assignments must agree on the IPv4 mask and on the IPv6 mask.
- Every `nodeRegex` pattern must compile. The webhook rejects invalid patterns rather than ignoring them.
- The value of `metadata.labels["unbounded-cloud.io/site"]` must be unique across Sites. Two Sites may not claim the same site label.
- Health check intervals must parse and fall between 10 milliseconds and 4,294,967 milliseconds.

References are validated when a peering or assignment is created or updated. `SitePeering.spec.sites` must name existing Sites, `SiteGatewayPoolAssignment.spec.gatewayPools` must name existing GatewayPools, and `GatewayPoolPeering.spec.gatewayPools` must name existing GatewayPools. Names must be non-empty and may not repeat within one list. The gateway pool name is also checked at creation time by a validating admission policy: it must be no more than 63 characters because the controller copies it into a Node label value.

{{< callout type="note" >}}
The validating webhook is configured with `failurePolicy: Ignore`. If the webhook service is unavailable, the API server accepts the object and the checks above do not run. The CRD schema still enforces types, enums, and numeric ranges, and the validating admission policies still protect Node fields. Treat the webhook as a guardrail, not as the only line of defense.
{{< /callout >}}

Deletion protection is separate from the webhook. The Site controller adds the finalizer `net.unbounded-cloud.io/protection` when at least one node is assigned to the Site, and removes it when the last node is unassigned. While the finalizer is present, Kubernetes keeps the Site in a terminating state instead of removing it. Drain or move the nodes first, then delete the Site.

## SiteNodeSlice

A `SiteNodeSlice` is a bounded view of the worker nodes in a Site. It exists because a Site can contain thousands of nodes, and embedding every node in one object would approach the maximum object size that the API server accepts. The controller groups nodes into slices of at most 500 and updates the slices as node addresses, WireGuard keys, and pod CIDRs change.

Slices are an implementation detail and a debugging aid. You read them; you do not write them. The Site controller creates a slice named `<site>-<index>`, sets the parent Site as its owner, and deletes slices whose index is no longer needed. It sorts nodes by name before assigning them to slices, so the contents are stable while the node set is stable. Gateway nodes are excluded from slices because they are represented in `GatewayPool.status.nodes` and in `GatewayPoolNode`.

The following example shows a two-node slice for `site-east`:

```yaml
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: SiteNodeSlice
metadata:
  name: site-east-0
  ownerReferences:
    - apiVersion: unbounded-cloud.io/v1alpha3
      kind: Site
      name: site-east
      uid: "<site-uid>"
siteName: site-east
sliceIndex: 0
nodeCount: 2
nodes:
  - name: worker-a
    wireGuardPublicKey: "uJ3k...="
    internalIPs:
      - "10.10.1.15"
      - "fd00:10:10::15"
    podCIDRs:
      - "100.64.0.0/24"
      - "fd00:64:1::/72"
  - name: worker-b
    wireGuardPublicKey: "Yh9p...="
    internalIPs:
      - "10.10.1.16"
    podCIDRs:
      - "100.64.1.0/24"
```

`<site-uid>` is a placeholder for the UID Kubernetes assigned to the parent Site. The controller fills it in; you never type it by hand.

### Fields

`SiteNodeSlice` has no `spec` or `status` subresource. All fields are top-level.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `siteName` | `string` | Yes | Name of the parent Site. |
| `sliceIndex` | `int` | Yes | Zero-based index of this slice within the Site. |
| `nodeCount` | `int` | No | Number of nodes in this slice. Mirrors the length of `nodes`. |
| `nodes` | `[]NodeInfo` | No | Node details, at most 500 entries. |
| `nodes[].name` | `string` | Yes | Kubernetes Node name. |
| `nodes[].wireGuardPublicKey` | `string` | No | The node's WireGuard public key, copied from the `net.unbounded-cloud.io/wg-pubkey` annotation. |
| `nodes[].internalIPs` | `[]string` | No | Internal addresses of the node, copied from `status.addresses`. |
| `nodes[].podCIDRs` | `[]string` | No | Pod CIDRs assigned to the node. |

### Lifecycle and Deletion

The controller reconciles slices on every Site pass. When nodes join, it creates the slices needed for the new node count. When node information changes, it updates the existing slice in place. When nodes leave and the required slice count drops, it deletes the extra slices after rechecking that each one still belongs to the Site. When the parent Site is deleted, the owner reference lets Kubernetes garbage collect the slices.

A `SiteNodeSlice` has no protection finalizer of its own. The controller recreates a deleted slice on its next pass, but node agents build same-site mesh peers from the slices, so a manual deletion can interrupt those peers until reconciliation completes. Do not build automation that deletes slices, and do not depend on a particular node-to-slice mapping.

The protection that does exist applies to the parent Site. A Site with assigned nodes carries the `net.unbounded-cloud.io/protection` finalizer, so it cannot be deleted until its nodes are gone. That is the behavior to rely on when you decommission a location.

## GatewayPool

A `GatewayPool` selects the nodes that forward traffic between sites. It is a label selector plus a small set of routing options. The controller evaluates the selector against every Node, publishes the matching nodes in the pool's status, allocates a unique WireGuard port to each one, and labels the Node with its gateway pool assignment.

A node belongs to at most one pool. If a node matches several pools, the controller sorts the eligible pool names and picks the lexicographically first one. The Node receives the label `net.unbounded-cloud.io/gateway-pool` with that pool name. A node with no such label is a worker.

The following pool selects nodes with a user-defined gateway label and marks itself as an external pool:

```yaml
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: GatewayPool
metadata:
  name: east-gateways
spec:
  type: External
  nodeSelector:
    role: gateway
    topology.kubernetes.io/region: east
  routedCidrs:
    - "172.16.0.0/12"
    - "192.168.100.0/24"
  healthCheckSettings:
    enabled: true
    detectMultiplier: 3
    receiveInterval: "1s"
    transmitInterval: "1s"
  tunnelProtocol: Auto
  tunnelMTU: 1400
status:
  nodeCount: 2
  connectedSites:
    - site-east
  reachableSites:
    - site-east
    - site-west
  nodes:
    - name: gw-east-1
      siteName: site-east
      internalIPs:
        - "10.10.2.10"
      externalIPs:
        - "203.0.113.10"
      healthEndpoints:
        - "100.64.10.1"
        - "fd00:64:10::1"
      wireGuardPublicKey: "kL2m...="
      gatewayWireguardPort: 51821
      podCIDRs:
        - "100.64.10.0/24"
        - "fd00:64:10::/72"
    - name: gw-east-2
      siteName: site-east
      internalIPs:
        - "10.10.2.11"
      externalIPs:
        - "203.0.113.11"
      healthEndpoints:
        - "100.64.11.1"
      wireGuardPublicKey: "pQ7r...="
      gatewayWireguardPort: 51822
      podCIDRs:
        - "100.64.11.0/24"
```

### Spec Fields

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `spec.type` | `string` | No | `External` | How the pool connects across sites. Valid values are `External` and `Internal`. |
| `spec.nodeSelector` | `map[string]string` | Yes | None | Label selector for gateway nodes. At least one label is required. |
| `spec.routedCidrs` | `[]string` | No | None | Additional CIDRs advertised through this pool, beyond the site and pod CIDRs the controller already derives. |
| `spec.healthCheckSettings` | `HealthCheckSettings` | No | Agent defaults | Health check parameters for same-pool gateway peers. |
| `spec.tunnelProtocol` | `*string` | No | `Auto` | Encapsulation for this pool's tunnel scope. Valid values are `WireGuard`, `IPIP`, `GENEVE`, `VXLAN`, `None`, and `Auto`. |
| `spec.tunnelMTU` | `*int32` | No | None | MTU for routes through tunnels in this scope. Range is 576 to 9000. |

An `External` pool requires every member to have at least one external address. The controller reads the address from the administrator annotation `net.unbounded-cloud.io/declared-public-ip` first, then from the Node's `ExternalIP` addresses, then from the agent-discovered annotation `net.unbounded-cloud.io/discovered-public-ip` while its expiry annotation is still in the future. An `Internal` pool does not require an external address. It is used when sites already reach each other over a private network and the gateways can connect without advertising a public endpoint.

`spec.routedCidrs` is for networks that sit behind the gateway but are not represented by a Site or a pod pool, such as an on-premises subnet reachable through a VPN. The agent includes these CIDRs in the routes it advertises and in the allowed IPs of its gateway tunnels.

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `status.nodeCount` | `int` | Number of nodes currently in the pool. |
| `status.connectedSites` | `[]string` | Sites directly attached to this pool, derived from SiteGatewayPoolAssignment and SitePeering relationships. |
| `status.reachableSites` | `[]string` | Sites reachable through this pool, including sites reached transitively through GatewayPoolPeering. |
| `status.nodes` | `[]GatewayNodeInfo` | Details for each member node, sorted by node name. |
| `status.nodes[].name` | `string` | Node name. |
| `status.nodes[].siteName` | `string` | Site the gateway node belongs to, taken from the Node's site label. |
| `status.nodes[].internalIPs` | `[]string` | Internal addresses of the gateway node. |
| `status.nodes[].externalIPs` | `[]string` | External addresses of the gateway node, when it has any. An `Internal` pool does not require them. |
| `status.nodes[].healthEndpoints` | `[]string` | Health check addresses, computed as the first usable address of each pod CIDR. |
| `status.nodes[].wireGuardPublicKey` | `string` | WireGuard public key of the gateway node. |
| `status.nodes[].gatewayWireguardPort` | `int32` | Controller-assigned listen port for gateway-to-gateway peering. Allocation starts at 51821 and is unique across all pools. |
| `status.nodes[].podCIDRs` | `[]string` | Pod CIDRs assigned to the gateway node. |

The controller writes `status.nodeCount` and `status.nodes`. The peering aggregation controller writes `status.connectedSites` and `status.reachableSites`. The two writers patch different fields, so a pool can show a fresh node list alongside a slightly older reachability view while the next aggregation pass completes.

### Gateway Node Requirements

A Node becomes a member of a GatewayPool when all of the following are true:

1. Its labels match every entry in `spec.nodeSelector`.
2. It has the `net.unbounded-cloud.io/wg-pubkey` annotation with a non-empty public key.
3. If the pool type is `External`, it has at least one external address from the sources described above.
4. It is not already claimed by a pool with a lexicographically earlier name. The first eligible pool wins, and the controller records the choice in the node's `net.unbounded-cloud.io/gateway-pool` label.

A node that matches any GatewayPool selector is excluded from `SiteNodeSlice` objects even if it also falls inside a Site's node CIDRs. A node that is actually adopted by a pool, which the controller records with the `net.unbounded-cloud.io/gateway-pool` label, is also excluded from the same-site mesh and uses dedicated gateway interfaces instead of the worker mesh. Adopted gateway nodes receive the `net.unbounded-cloud.io/gateway-node=true:NoSchedule` taint, which keeps ordinary workloads off the forwarding path.

Use a dedicated label for gateway nodes instead of selecting a broad role such as `node-role.kubernetes.io/worker`. A selector that matches every node in a Site leaves that Site with no worker slices and no mesh peers, because all of its nodes are claimed as gateways. A label such as `role: gateway` that you apply only to the nodes you intend to use is easier to audit and safer to change.

The controller protects the pool from premature deletion. When at least one node matches, it adds the `net.unbounded-cloud.io/protection` finalizer to the GatewayPool. The finalizer is removed after the last matching node is gone, so a pool that still has members cannot be deleted until the nodes are drained or relabeled.

### Gateway Peer IP Resolution

When a node builds a tunnel to a gateway, the address it uses depends on the relationship between the two locations and on the pool type. The rule has four cases:

| Peer relationship | Pool type | Endpoint used |
|-------------------|-----------|---------------|
| Gateway is in the same Site | Any | Internal address |
| Gateway is in a SitePeering with the local Site, but not the same Site | Any | Internal address |
| Gateway is in a different, non-peered Site | `External` | External address |
| Gateway is in a different, non-peered Site | `Internal` | No address is set; WireGuard learns the endpoint from incoming traffic |

The same-site and peered-site cases use the internal address because the underlay already provides reachability between those locations. The external case uses the public address so that two sites with no private path can still connect. The internal-pool case is deliberately passive: the remote gateway is expected to initiate the connection through whatever private path exists, and the local node learns the reachable endpoint from the handshake.

Port selection follows the same split. A gateway-to-gateway link uses the local gateway's assigned port, because the remote interface that serves that link listens on it. A worker-to-gateway link uses the standard mesh port. The gateway's own pool status publishes the assigned port so both ends agree without a separate lookup service.

## GatewayPoolNode

A `GatewayPoolNode` represents one gateway node's membership in a pool and carries the route advertisements that the node agent publishes. It is not the Kubernetes Node. The Node is the machine; the `GatewayPoolNode` is the advertisement object named after it. The controller creates one for each matching gateway node, and the agent on that node patches the status.

The object is named after the Node and has two owner references. The Node is the controlling owner and the GatewayPool is a non-controlling owner. That relationship lets Kubernetes clean up the advertisement when either the Node or the pool disappears. If a gateway node is moved to a different pool, the controller patches the existing object in place rather than deleting and recreating it.

```yaml
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: GatewayPoolNode
metadata:
  name: gw-east-1
  labels:
    net.unbounded-cloud.io/gateway-pool: east-gateways
    net.unbounded-cloud.io/node: gw-east-1
    unbounded-cloud.io/site: site-east
  ownerReferences:
    - apiVersion: v1
      kind: Node
      name: gw-east-1
      uid: "<node-uid>"
      controller: true
      blockOwnerDeletion: false
    - apiVersion: net.unbounded-cloud.io/v1alpha1
      kind: GatewayPool
      name: east-gateways
      uid: "<pool-uid>"
      controller: false
      blockOwnerDeletion: false
spec:
  nodeName: gw-east-1
  gatewayPool: east-gateways
  site: site-east
status:
  lastUpdated: "2026-11-17T12:00:10Z"
  forwardingMTU: 1400
  routes:
    "10.10.0.0/16":
      type: NodeCidr
      paths:
        - - type: Site
            name: site-east
          - type: GatewayPool
            name: east-gateways
    "100.64.0.0/16":
      type: PodCidr
      paths:
        - - type: Site
            name: site-east
          - type: GatewayPool
            name: east-gateways
    "172.16.0.0/12":
      type: RoutedCidr
      paths:
        - - type: Site
            name: site-east
          - type: GatewayPool
            name: east-gateways
```

`<node-uid>` and `<pool-uid>` are placeholders for the UIDs Kubernetes assigned to the Node and the GatewayPool. The controller writes them; you do not.

### Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec.nodeName` | `string` | Yes | Kubernetes Node name that publishes this advertisement. |
| `spec.gatewayPool` | `string` | Yes | Owning GatewayPool name. |
| `spec.site` | `string` | No | Site label of the gateway node at the time the object was reconciled. |
| `status.lastUpdated` | `metav1.Time` | No | Heartbeat timestamp written by the node agent. |
| `status.forwardingMTU` | `*int32` | No | Effective local forwarding MTU in bytes, published after a successful data plane reconciliation. Range is 1 to 65535. Absent when the agent has not published a limit. |
| `status.routes` | `map[string]GatewayNodeRoute` | No | Reachable CIDRs advertised by this gateway, keyed by CIDR prefix. At most 1000 entries. |
| `status.routes[].type` | `string` | Yes | Route source type: `NodeCidr`, `PodCidr`, or `RoutedCidr`. |
| `status.routes[].source` | `GatewayNodePathHop` | No | Originating object for the route, as a `type` and `name` pair. |
| `status.routes[].intermediateHops` | `[]GatewayNodePathHop` | No | Additional path objects between the source and the local advertiser. |
| `status.routes[].paths` | `[][]GatewayNodePathHop` | No | Complete ordered path sequences from origin to the local advertiser. At most 100 entries. |

The route map is the agent's view of what sits behind this gateway. A `NodeCidr` route covers a site's node addresses, a `PodCidr` route covers a site's pod pools, and a `RoutedCidr` route covers an extra prefix from `GatewayPool.spec.routedCidrs`. The paths let a receiving node distinguish a directly attached prefix from one reached through another pool, which matters for both next-hop selection and MTU aggregation.

### Forwarding MTU

`status.forwardingMTU` is the MTU that this gateway can actually forward after encapsulation. The agent publishes it only after the data plane has been reconciled and the active tunnel MTUs have been verified, so a value that is present is a value that was measured against the current configuration. The field is optional because older agents do not write it and because a gateway that has not completed a reconciliation has nothing to publish.

Workers use the value as a cap when they install a path through a gateway. They also apply the published values of candidate gateways in intermediate pools on accepted paths, which protects a worker even when it does not use the route that imposed the lower limit. The result is that a path is installed with the smallest MTU on the chain rather than the MTU of the first hop.

Gateways do not consume other gateways' published forwarding MTUs. Each gateway derives its own limit from its interfaces and its local configuration. Avoiding a shared feedback loop keeps a low value from one gateway from permanently pinning every other gateway in the mesh, and it lets a previously lowered minimum rise again after the underlying interface recovers.

Route advertisements have a heartbeat. The agent refreshes `status.lastUpdated` every 10 seconds. If the timestamp is missing or older than four heartbeat intervals, which is 40 seconds, the receiving agent treats the advertisement as stale and uses its fallback route set. A gateway that has no `GatewayPoolNode` object at all is not used as a route destination; the peer is skipped rather than assumed healthy. That rule prevents a node that has been removed from a pool from continuing to attract traffic through a cached status entry.

Install the CRD that includes `status.forwardingMTU` before you upgrade the node agents. If an older CRD is still installed, the API server prunes the unknown field from the status patch, and workers will not see the published limit. The older behavior still works through topology-derived MTU handling, but the additional protection for shared interfaces requires both the updated CRD and the updated agent.

## SitePeering

A `SitePeering` declares that two or more sites should be treated as directly connected. By default, nodes in the listed sites create direct tunnels to each other and route pod traffic across those tunnels, bypassing the gateway path. You can also use the object to declare that sites are network-peered without meshing their nodes, which lets gateway links between those sites use internal addresses.

```yaml
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: SitePeering
metadata:
  name: east-west-peering
spec:
  enabled: true
  sites:
    - site-east
    - site-west
  meshNodes: true
  healthCheckSettings:
    enabled: true
    detectMultiplier: 3
    receiveInterval: "1s"
    transmitInterval: "1s"
  tunnelProtocol: Auto
  tunnelMTU: 1400
status:
  peeredSiteCount: 2
  totalNodeCount: 20
```

### Spec Fields

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `spec.enabled` | `*bool` | No | `true` | Activates the peering. When `false`, the object is treated as if it did not exist. |
| `spec.sites` | `[]string` | No | None | Site names to peer. At least two names are required when the list is present. |
| `spec.meshNodes` | `*bool` | No | `true` | Whether nodes in the listed sites create direct tunnels to one another. |
| `spec.healthCheckSettings` | `HealthCheckSettings` | No | Agent defaults | Health check parameters for inter-site node routes. |
| `spec.tunnelProtocol` | `*string` | No | `Auto` | Encapsulation for this peering's tunnel scope. Valid values are `WireGuard`, `IPIP`, `GENEVE`, `VXLAN`, `None`, and `Auto`. |
| `spec.tunnelMTU` | `*int32` | No | None | MTU for routes through tunnels in this scope. Range is 576 to 9000. |

The `sites` list is optional in the schema but constrained to at least two entries when present. An empty or missing list produces a peering that matches nothing and has no effect. The admission webhook additionally requires every named Site to exist at the time the peering is created or updated, and it rejects empty or duplicate names.

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `status.peeredSiteCount` | `int` | Number of sites in `spec.sites` that currently exist. |
| `status.totalNodeCount` | `int` | Sum of `status.nodeCount` for the sites that exist, including gateway nodes. |

The status is an observation, not a gate. If a Site named in the peering is deleted, the peering remains and `status.peeredSiteCount` decreases on the next aggregation pass. Recreate the Site or remove its name from the list to bring the counts back in line.

### Behavior

The `meshNodes` field selects between two distinct forwarding models:

- With `meshNodes: true` (the default), nodes in the listed sites create direct tunnels to each other. The sites are treated as one routing domain for pod traffic, and pod CIDRs from all listed sites are combined. Traffic between peered sites does not go through a gateway pool.
- With `meshNodes: false`, the sites remain network-peered, but nodes do not create direct tunnels. Gateway routing is still used. The difference is that gateway pool peerings between pools in those sites can use internal addresses instead of external addresses, because the underlay is known to be reachable.

Setting `tunnelProtocol: None` disables encapsulation for the peering scope. Routes are programmed on the default route interface using the peer's internal address as the gateway, which requires layer 3 reachability between the nodes. Use this only when an external network already provides that reachability and you do not want a tunnel header on the path.

A peering with `meshNodes: true` and a large `sites` list creates a full mesh among all member nodes. That is simple and fast for small and medium sites, but the number of tunnels grows with the square of the node count. For large sites, keep `meshNodes: false` and let the gateway pools carry the traffic.

### Use Cases

- Low-latency cross-site traffic when the sites are in the same region or connected by a dedicated, high-bandwidth link.
- Simplified routing when a gateway hop adds latency or operational complexity that the underlay does not require.
- Externally peered networks, such as ExpressRoute, Direct Connect, or a site-to-site VPN, where layer 3 reachability already exists and `tunnelProtocol: None` or an internal gateway pool is the right model.
- Large sites that should be network-peered for internal address resolution but should not build a full node mesh.

## SiteGatewayPoolAssignment

A `SiteGatewayPoolAssignment` attaches sites to gateway pools. It is the object that makes a pool usable for a site's traffic. Without an assignment, a GatewayPool can exist and have members, but a worker in a site has no reason to select it as the next hop for another location.

The object also carries the tunnel protocol and MTU for the site-to-pool link, plus health check settings for that relationship. It has no status subresource and writes no observed state.

```yaml
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: SiteGatewayPoolAssignment
metadata:
  name: east-to-gateways
spec:
  enabled: true
  sites:
    - site-east
  gatewayPools:
    - east-gateways
    - east-backup-gateways
  healthCheckSettings:
    enabled: true
    detectMultiplier: 3
    receiveInterval: "1s"
    transmitInterval: "1s"
  tunnelProtocol: Auto
  tunnelMTU: 1400
```

### Spec Fields

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `spec.enabled` | `*bool` | No | `true` | Activates the assignment. When `false`, the object is treated as if it did not exist. |
| `spec.sites` | `[]string` | No | None | Sites that may use the listed pools. At least one name is required when the list is present. |
| `spec.gatewayPools` | `[]string` | No | None | Pools assigned to those sites. At least one name is required when the list is present. |
| `spec.healthCheckSettings` | `HealthCheckSettings` | No | Agent defaults | Health check parameters for the site-to-pool relationship. |
| `spec.tunnelProtocol` | `*string` | No | `Auto` | Encapsulation for the site-to-pool link. Valid values are `WireGuard`, `IPIP`, `GENEVE`, `VXLAN`, `None`, and `Auto`. |
| `spec.tunnelMTU` | `*int32` | No | None | MTU for routes through tunnels in this scope. Range is 576 to 9000. |

The assignment protocol has a narrow but useful override rule. When `spec.tunnelProtocol` is set to a concrete value, it governs the site-to-pool link. When it is unset or `Auto` and the gateway is in the same site as the worker, the Site's `spec.tunnelProtocol` is used as the fallback. When the gateway is in a different site and the assignment is `Auto`, the choice falls through to the node agent's preferred encapsulation, which normally means WireGuard for links that use external addresses and GENEVE for links that use only internal addresses.

A site may have several assignments, and one assignment may list several pools. All enabled assignments that name the site contribute candidate pools. The controller and the agent do not treat the list as ordered; the pool that wins is the one the routing and node-selection logic considers best for the destination, not simply the first entry.

## GatewayPoolPeering

A `GatewayPoolPeering` links gateway pools so that traffic can cross between them. It is used when sites are not directly peered and the path between them runs through more than one pool, or when a pool should be able to reach another pool's advertised prefixes without a site-level relationship.

```yaml
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: GatewayPoolPeering
metadata:
  name: east-west-pool-peering
spec:
  enabled: true
  gatewayPools:
    - east-gateways
    - west-gateways
  healthCheckSettings:
    enabled: true
    detectMultiplier: 5
    receiveInterval: "500ms"
    transmitInterval: "500ms"
  tunnelProtocol: WireGuard
  tunnelMTU: 1380
```

### Spec Fields

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `spec.enabled` | `*bool` | No | `true` | Activates the peering. When `false`, the object is treated as if it did not exist. |
| `spec.gatewayPools` | `[]string` | No | None | Pools to peer. At least two names are required when the list is present. |
| `spec.healthCheckSettings` | `HealthCheckSettings` | No | Agent defaults | Health check parameters for routes between the peered pools. |
| `spec.tunnelProtocol` | `*string` | No | Unset, preserving the pool and site fallback | Protocol for the cross-pool gateway links. Valid values are `WireGuard`, `IPIP`, `GENEVE`, `VXLAN`, `None`, and `Auto`. |
| `spec.tunnelMTU` | `*int32` | No | None | MTU for routes through tunnels in this scope. Range is 576 to 9000. |

The protocol field on this object is the highest-priority setting for a cross-pool gateway link. An explicit value overrides the GatewayPool, Site, and SiteGatewayPoolAssignment settings for that link. An explicit `Auto` uses the endpoint-based defaults rather than inheriting a lower-scope protocol. Omitting the field leaves the existing pool and site fallback in place, which is different from setting it to `Auto`.

If several enabled peerings name the same pair of pools with different protocols, the agent processes the objects in lexicographic name order and keeps the first explicit protocol it sees. A conflict is logged rather than rejected. An unset protocol does not mask another peering's explicit setting, so you can add a specific override without rewriting every peering that mentions the pool.

A GatewayPoolPeering does not require the pools to be attached to any site. It extends the reachability graph that the aggregation controller walks: `status.connectedSites` on a pool reflects direct attachments, and `status.reachableSites` adds sites reached through pool peerings. That is why a chain of pool peerings can make a site appear reachable even when no single SiteGatewayPoolAssignment names the far end.

## HealthCheckSettings

`HealthCheckSettings` appears in five resource types: `Site`, `SitePeering`, `GatewayPool`, `SiteGatewayPoolAssignment`, and `GatewayPoolPeering`. It controls the liveness probes that the node agent exchanges with tunnel peers. The probes detect a path that has stopped passing traffic before the routing table has to fail over purely on routing metrics.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `enabled` | `*bool` | No | `true` | Turns health checking on or off for this scope. |
| `detectMultiplier` | `*int32` | No | `3` | Multiplier used to determine session down detection. Larger values make failure detection less sensitive. Range is 1 to 255. |
| `receiveInterval` | `int` or `string` | No | `15s` | Minimum interval between received probes. A number is interpreted as milliseconds; a string is parsed as a Go duration. |
| `transmitInterval` | `int` or `string` | No | `15s` | Minimum interval between transmitted probes. A number is interpreted as milliseconds; a string is parsed as a Go duration. |

Both intervals accept either form, so `receiveInterval: 1000`, `receiveInterval: "1000"`, and `receiveInterval: "1s"` all mean one second. The webhook rejects values below 10 milliseconds or above 4,294,967 milliseconds, and it rejects strings that are neither a duration nor an integer.

The defaults are applied after the governing scope is selected. If a scope omits `receiveInterval`, the agent uses 15 seconds rather than inheriting an interval from a less specific scope. That distinction matters when you set a short interval on a `Site` and expect a `GatewayPool` without settings to inherit it: it does not. The pool uses the defaults for any field it omits, and only the association structure is inherited.

### Precedence

Health check resolution follows the link type. Each link has exactly one governing object for the profile, and the agent resolves that object before falling back to broader scopes.

| Link type | Governing object |
|-----------|------------------|
| Node to node within one Site | `Site` |
| Node to node across a SitePeering | `SitePeering` |
| Worker to a gateway in an assigned pool | `SiteGatewayPoolAssignment` |
| Gateway to gateway in the same pool | `GatewayPool` |
| Gateway to gateway in different pools | `GatewayPoolPeering` |

For mesh peers, the resolution order from most specific to least specific is:

1. An explicit peer profile. This is set from `GatewayPool` settings for same-pool gateway peers and from `GatewayPoolPeering` settings for cross-pool gateway peers.
2. A `SiteGatewayPoolAssignment` that connects the peer's site to the relevant pool.
3. A `SitePeering` that connects the two sites.
4. The peer's `Site`.

For gateway peers, the order depends on the node's role. A gateway node uses its explicit peer profile first, then the profile of the peer's `GatewayPool`, then a `SiteGatewayPoolAssignment` keyed by the remote site and pool. A worker uses the assignment profile keyed by its own site and the peer's pool. A gateway node does not use a worker's local assignment profile, because the link is a gateway-to-gateway relationship rather than a site-to-pool one.

Shared, non-WireGuard tunnels have one extra fallback. When no pool or assignment profile exists for a gateway peer, a GENEVE or IPIP link can fall back to the local Site's profile, because those protocols have no WireGuard handshake to serve as a liveness signal. WireGuard gateway peers do not use that Site fallback.

An explicitly disabled profile blocks every later fallback. If `healthCheckSettings.enabled` is `false` on the governing object, the agent records the scope as administratively disabled and does not consult the next scope. This is intentional: disabling health checks at a specific association should not be silently overridden by a broader default.

When several peerings define conflicting settings for the same target site or pool, the agent processes the enabled objects in lexicographic name order and keeps the first profile. It logs the conflict with both object names. To force a particular profile, give the winning object a name that sorts before the others, or remove the duplicate association entirely.

## Labels, Annotations, and Taints

The controllers and node agents communicate part of the network state through standard Kubernetes Node metadata. These keys are part of the contract even though they are not CRD fields.

| Key | Kind | Applied to | Purpose |
|-----|------|------------|---------|
| `unbounded-cloud.io/site` | Label | Node | Canonical site membership. The Site controller writes it when a node's internal address matches a Site. |
| `net.unbounded-cloud.io/site` | Label | Node | Deprecated site membership key. The controller still writes it alongside the canonical key and reads it as a fallback; a future release will remove it. |
| `net.unbounded-cloud.io/gateway-pool` | Label | Node, GatewayPoolNode | The single GatewayPool adopted by the node. An absent label on a Node means the node is a worker. |
| `net.unbounded-cloud.io/node` | Label | GatewayPoolNode | Node name copied onto the advertisement for easier selection. |
| `net.unbounded-cloud.io/wg-pubkey` | Annotation | Node | WireGuard public key. The node agent writes it, and the GatewayPool controller requires it for pool membership. |
| `net.unbounded-cloud.io/wireguard-port` | Annotation | Node | Gateway-to-gateway listen port assigned by the GatewayPool controller. |
| `net.unbounded-cloud.io/tunnel-mtu` | Annotation | Node | Detected maximum tunnel MTU, used to warn when a configured MTU exceeds what the node can carry. |
| `net.unbounded-cloud.io/declared-public-ip` | Annotation | Node | Administrator-declared public address. Highest precedence for an External pool. |
| `net.unbounded-cloud.io/discovered-public-ip` | Annotation | Node | Address discovered by the agent. Used only while its expiry annotation is in the future. |
| `net.unbounded-cloud.io/discovered-public-ip-expires-at` | Annotation | Node | RFC3339 expiry for the discovered address. Required when the discovered address is present. |
| `net.unbounded-cloud.io/gateway-node` | Taint | Node | Applied with value `true` and effect `NoSchedule` to keep ordinary workloads off gateway nodes. |

The node agent acknowledges its adopted role by writing the annotations `net.unbounded-cloud.io/effective-site` and `net.unbounded-cloud.io/effective-gateway-pool` on its own Pod. An empty effective gateway pool value means the worker role, and an absent key means the acknowledgment is still pending. Those annotations let the controller tell the difference between an agent that has adopted its assignment and one that has not reconciled yet.

The site label is the one key that has a deprecation path. Use `unbounded-cloud.io/site` in your own selectors and automation. The deprecated key is preserved so that older agents and mixed-version clusters continue to work during the transition, but it is not the long-term contract.

## Short Names

Every resource has a short name so that interactive commands stay readable.

| Resource | Short name | Example |
|----------|------------|---------|
| `Site` | `st` | `kubectl get st` |
| `SiteNodeSlice` | `sns` | `kubectl get sns` |
| `GatewayPool` | `gp` | `kubectl get gp` |
| `GatewayPoolNode` | `gpn` | `kubectl get gpn` |
| `SitePeering` | `spr` | `kubectl get spr` |
| `SiteGatewayPoolAssignment` | `sgpa` | `kubectl get sgpa` |
| `GatewayPoolPeering` | `gpp` | `kubectl get gpp` |

Useful inspection commands include:

```bash
# List sites and their node and slice counts
kubectl get st

# Show a gateway pool selector and its current node count
kubectl get gp -o wide

# Read the advertised routes and heartbeat of a gateway
kubectl get gpn gw-east-1 -o yaml

# Inspect the slices for one site
kubectl get sns -l unbounded-cloud.io/site=site-east

# Watch reachability as peerings are added
kubectl get gp east-gateways -o jsonpath='{.status.connectedSites}{"\n"}{.status.reachableSites}{"\n"}'
```

All of these resources are cluster-scoped, so no `-n` flag is needed.

## CRD Relationships

![Diagram showing Site ownership of SiteNodeSlice and selection of Nodes, GatewayPool selection of gateway Nodes, GatewayPoolNode membership, SitePeering between Sites, SiteGatewayPoolAssignment from Sites to GatewayPools, and GatewayPoolPeering between GatewayPools](../../img/networking-crd-relationships.svg)

The objects form two layers. The first layer describes locations and membership:

- A `Site` owns zero or more `SiteNodeSlice` objects through owner references.
- A `Site` selects Nodes indirectly, by internal address, and marks them with a site label.
- A `GatewayPool` selects Nodes by label. A Node that matches a selector is excluded from worker slices; a Node that is adopted by the pool also becomes a gateway.
- A `GatewayPool` owns its `GatewayPoolNode` objects as a non-controlling owner, while each Node controls its own advertisement object.

The second layer describes reachability. These objects reference other objects by name and do not own them:

- `SitePeering` names two or more Sites.
- `SiteGatewayPoolAssignment` names one or more Sites and one or more GatewayPools.
- `GatewayPoolPeering` names two or more GatewayPools.

Because the reachability objects reference rather than own their targets, deleting a Site does not delete a GatewayPool, and deleting a GatewayPool does not delete a peering that names it. The admission webhook checks that references exist when an object is created or updated, but it does not keep checking afterward. The controllers tolerate dangling references: counts include only objects that still exist, and a pool that is named but missing is skipped.

The runtime flow is easier to follow as a sequence:

1. You create a Site with `nodeCidrs` and pod pools. The Site controller labels matching Nodes, allocates pod CIDRs when assignment is enabled, and writes `SiteNodeSlice` objects for the workers.
2. You create a GatewayPool with a selector. The controller claims eligible nodes, assigns each a WireGuard port, creates `GatewayPoolNode` objects, and publishes the members in `status.nodes`. The agent on each claimed node applies the gateway taint and builds the gateway interfaces.
3. You create a `SiteGatewayPoolAssignment` so workers in a Site know which pools serve their cross-site traffic.
4. You create a `SitePeering` if two sites should mesh directly. Set `meshNodes: false` if you only want the sites to be treated as network-peered for internal address resolution.
5. You create a `GatewayPoolPeering` when traffic must cross between pools that are not joined by a direct site peering.
6. Each gateway agent publishes routes and a forwarding MTU in its `GatewayPoolNode`. Workers combine those advertisements with the local Site's address plan and install the lowest-MTU path that reaches the destination.

The [routing flows reference]({{< relref "reference/networking/routing-flows" >}}) traces the packet path for same-site, cross-site, and multi-pool topologies. The [configuration reference]({{< relref "reference/networking/configuration" >}}) covers the encapsulation and MTU choices that the protocol fields select, and the [operations reference]({{< relref "reference/networking/operations" >}}) covers health checks and troubleshooting.

## Next Steps

- Read [Networking architecture]({{< relref "reference/networking/architecture" >}}) for the controller and agent reconciliation loops behind these objects.
- Read [Networking configuration]({{< relref "reference/networking/configuration" >}}) to choose a tunnel protocol, set MTU values, and tune health checks.
- Read [Routing flows]({{< relref "reference/networking/routing-flows" >}}) to follow packets across sites and gateway pools.
- Read [Networking operations]({{< relref "reference/networking/operations" >}}) for health monitoring, failure modes, and day-two tasks.
- Read the [Machina custom resource reference]({{< relref "reference/machina-crd" >}}) for the rest of the `unbounded-cloud.io` API group.
