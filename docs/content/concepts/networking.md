---
title: "Networking"
weight: 2
description: "How unbounded-net provides pod-to-pod connectivity across sites."
---

## Why Multi-Site Networking?

When worker nodes join a Kubernetes cluster from different physical locations,
the standard CNI model (which assumes all nodes can reach each other directly)
no longer works. Pods on a remote node need a network path back to pods in the
main cluster, and that path must handle:

- NAT and firewall traversal
- Encryption for traffic over the public internet
- Efficient routing to avoid bottlenecks
- Automatic CIDR allocation so pod IPs don't collide

**[unbounded-net](https://github.com/Azure/unbounded-net)** solves
these problems. It is a Kubernetes networking system that provides CNI
functionality and multi-site pod routing.

## Architecture

unbounded-net runs two components:

**Controller** (`unbounded-net-controller`) -- A leader-elected Deployment that
manages the control plane:

- Allocates pod CIDRs to nodes from per-site pools.
- Auto-assigns nodes to sites by matching their internal IP against
  `Site.spec.nodeCidrs`.
- Tracks gateway pool membership and health.
- Manages `SiteNodeSlice` objects (batches of up to 500 node-to-CIDR mappings
  to avoid etcd object size limits).

**Node Agent** (`unbounded-net-node`) -- A DaemonSet on every node that handles
the data plane:

- Writes CNI bridge configuration for local pod networking.
- Manages WireGuard keys and tunnel interfaces.
- Programs routes using eBPF or netlink.
- Monitors gateway health with UDP probes.

## Core Concepts

### Sites

A **Site** represents a physical location -- a data center, cloud region, or
edge deployment. Each site is defined by:

- **Node CIDRs** -- IP ranges that identify which nodes belong to this site.
  Nodes are automatically assigned to a site when their internal IP falls
  within one of these ranges.
- **Pod CIDRs** -- IP ranges allocated to pods on nodes in this site. The
  controller hands out per-node slices from these pools.

When a node joins the cluster, the controller matches its internal IP against
all Site `nodeCidrs` and labels it with `net.unbounded-kube.io/site=<name>`.

### Gateway Pools

A **GatewayPool** defines a set of nodes that act as routers between sites.
Gateway nodes forward traffic from one site's pods to another using tunnels.

Key properties:

- Gateway nodes are identified by a label selector (e.g.,
  `unbounded-kube.io/unbounded-net-gateway=true`).
- Multiple gateways in a pool provide **ECMP load balancing** -- traffic is
  distributed across all healthy gateways.
- Sites are bound to gateway pools via **SiteGatewayPoolAssignment** resources.

Traffic flow: Pod on Site A &rarr; local node routes to a gateway &rarr;
gateway tunnels to a gateway in Site B &rarr; gateway routes to the destination
pod's node.

### Peering

Two types of peering allow finer-grained control over traffic paths:

- **SitePeering** -- Declares a relationship between two or more sites. By
  default (`meshNodes: true`) this creates direct node-to-node WireGuard
  tunnels, bypassing gateways. When `meshNodes: false`, no overlay tunnels are
  created -- instead, the system routes between sites over whatever L3
  connectivity already exists (see [Externally Peered Sites](#externally-peered-sites)
  below).
- **GatewayPoolPeering** -- Creates gateway-to-gateway tunnels between
  different pools.

### Externally Peered Sites

When two sites already have a private L3 connection -- such as **Azure
ExpressRoute**, **AWS Direct Connect**, a hardware VPN, or any other dedicated
interconnect -- you can merge them into one logical network without adding a
WireGuard overlay.

Create a `SitePeering` with `meshNodes: false` and `tunnelProtocol: None`:

```yaml
apiVersion: net.unbounded-kube.io/v1alpha1
kind: SitePeering
metadata:
  name: azure-expressroute-peering
spec:
  sites:
    - site-on-prem
    - site-azure
  meshNodes: false       # no WireGuard overlay
  tunnelProtocol: None   # use the existing L3 path directly
```

This tells unbounded-net to:

1. Treat nodes in both sites as **network-reachable via their internal IPs**,
   using the existing private connection.
2. Program routes using those internal IPs directly -- no tunnel encapsulation
   overhead.
3. Let gateway pool peerings between these sites also resolve to **internal IPs**
   instead of external IPs, keeping traffic on the fast path.

The result is a single larger logical site from a pod-routing perspective.
Traffic flows at wire speed over the dedicated interconnect, and WireGuard
remains available as an encrypted overlay for any links where you still want it.
Both approaches can coexist in the same cluster: some site pairs use WireGuard
over the internet, others use direct routing over ExpressRoute or Direct Connect.

## Tunnel Types

unbounded-net supports five tunnel encapsulation types. The choice affects
overhead, encryption, and performance:

| Type | Header Overhead | Encrypted | Typical Use |
|------|----------------|-----------|-------------|
| **WireGuard** | 80 bytes | Yes | Cross-site links over public networks (default for external IPs) |
| **GENEVE** | 58 bytes | No | High-throughput internal links (default for internal IPs) |
| **VXLAN** | 58 bytes | No | Internal links with NIC hardware offload support |
| **IPIP** | 20 bytes | No | Minimal overhead for trusted internal links |
| **None** | 0 bytes | No | Direct L3 routing -- for externally peered sites (ExpressRoute, Direct Connect, VPN) |

Tunnel types are selected per scope (intra-site, inter-site via gateways,
peered sites). A **security-wins rule** ensures that if any scope explicitly
configures WireGuard for a given link, WireGuard is used regardless of what
other scopes request.

Choosing `None` on a `SitePeering` with `meshNodes: false` is the standard
pattern for sites already connected over a dedicated private link. Traffic
travels at full line rate with no encapsulation overhead.

## Dataplane Modes

The node agent supports two dataplanes for programming routes:

### eBPF (default)

The eBPF dataplane attaches a TC egress BPF program to a `unbounded0` dummy
interface. It uses LPM (Longest Prefix Match) trie maps for per-destination
tunnel endpoint resolution. Tunnel interfaces (`geneve0`, `vxlan0`, `ipip0`)
are shared across all peers using flow-based encapsulation.

**Advantages:** Fewer kernel objects at scale, shared tunnel interfaces,
efficient map-based lookups.

### Netlink (legacy)

The netlink dataplane creates per-peer tunnel interfaces and programs routes
using standard kernel routing tables.

**Advantages:** Simpler debugging with standard `ip route` tools.

## How It Relates to unbounded-kube

unbounded-net is the **networking layer** for the broader Project Unbounded
system:

- **unbounded-kube** handles node provisioning -- getting remote machines
  registered as Kubernetes nodes (via SSH or PXE).
- **unbounded-net** handles networking -- making those nodes reachable for pod
  traffic.

They share the same API group prefix (`unbounded-kube.io` / `net.unbounded-kube.io`)
and are designed to work together. When you run `kubectl unbounded site init`,
it installs both the machina controller and the unbounded-net CNI plugin.

## Next Steps

- **[Getting Started]({{< relref "guides/getting-started" >}})** -- The
  quickstart installs unbounded-net as part of site initialization.
- **[Networking Reference]({{< relref "reference/networking" >}})** -- Full
  CRD specifications, configuration flags, routing flows, and operational
  guides.
- **[Architecture Reference]({{< relref "reference/architecture" >}})** --
  How networking fits into the overall system design.
