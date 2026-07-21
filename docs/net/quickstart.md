<!-- Copyright (c) Microsoft Corporation. Licensed under the MIT License. -->

# Quick Start Guide

This guide walks you through bootstrapping Unbounded Networking on a Kubernetes
cluster, creating your first Site, and optionally configuring gateway pools for
multi-site or AKS deployments.

## Prerequisites

- **Kubernetes cluster** (v1.27+) with WireGuard kernel module support on nodes
- **kubectl** configured for your cluster
- **Go toolchain** (if building from source or using `make -C hack/net deploy`)
- For **AKS**: Azure CLI (`az`), a subscription with permissions to create
  clusters, node pools, and public IP prefixes

## Deploying Unbounded Networking

Component workloads are deployed by `unbounded-operator` from `Site.spec.components`.
Bootstrap the CRDs and operator first, then create Site resources.

### Option A: Deploy with Make (recommended)

```bash
# Build the kubectl plugin, then bootstrap CRDs and unbounded-operator
make -C hack/net deploy
```

The operator deploys unbounded-system into the `unbounded-system` namespace by default
when a Site enables the net component.

For direct component debugging, use:

```bash
make -C hack/net deploy-direct
```

### Option B: Deploy with the plugin

```bash
kubectl unbounded install
```

### Option C: Manual deployment

If you are working from a pre-rendered manifest archive, apply the CRDs and
operator manifests. The operator will deploy controller and node workloads after
Sites are created.

#### 1. Install CRDs

```bash
kubectl apply -f deploy/machina/crd/
kubectl apply -f deploy/net/crd/
```

This installs the shared Site CRD in `unbounded-cloud.io` plus the networking
CRDs in `net.unbounded-cloud.io`:

| CRD | Short Name | Description |
|-----|-----------|-------------|
| `sites` | `st` | Shared network location containing nodes |
| `sitenodeslices` | `sns` | Controller-managed node membership slices |
| `gatewaypools` | `gp` | Pool of gateway nodes for inter-site routing |
| `gatewaypoolnodes` | `gpn` | Per-node gateway status (controller-managed) |
| `sitegatewaypoolassignments` | `sgpa` | Links sites to gateway pools |
| `sitepeerings` | `spr` | Direct peering between sites |
| `gatewaypoolpeerings` | `gpp` | Peering between gateway pools |

#### 2. Deploy the operator

```bash
kubectl apply -f deploy/unbounded-operator/rendered/
```

When using source templates, render first:

```bash
make unbounded-operator-manifests
kubectl apply -f deploy/unbounded-operator/rendered/
```

The operator deployment includes:

- **ServiceAccount**, **ClusterRole**, **ClusterRoleBinding** -- RBAC for
  reconciling enabled components.
- **Deployment** -- watches Sites and applies component manifests.

#### 3. Create a Site

Create a Site that enables unbounded-system:

A Site defines a network location, its pod CIDR allocation, and enabled
components:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Site
metadata:
  name: primary
spec:
  components:
    net:
      enabled: true
  nodeCidrs:
    - 10.240.0.0/16
  podCidrAssignments:
    - cidrBlocks:
        - 10.244.0.0/14
      nodeBlockSizes:
        ipv4: 24
  tunnelProtocol: Auto
```

```bash
kubectl apply -f site.yaml
```

Key fields:

- `nodeCidrs` (required) -- CIDRs containing the internal IPs of nodes at
  this site
- `podCidrAssignments` -- defines pod CIDR pools and per-node block sizes;
  the controller allocates `/24` blocks from the pool to each node
- `components.net.enabled` -- asks `unbounded-operator` to deploy unbounded-system
- `tunnelProtocol` -- tunnel encapsulation mode; `Auto` selects WireGuard for
  external IPs and GENEVE for internal IPs. Options: `WireGuard`, `IPIP`,
  `GENEVE`, `VXLAN`, `None`, `Auto`

See [docs/templates/site.yaml](templates/site.yaml) for a fully annotated
example with all optional fields.

#### 4. Verify

Check that the operator has reconciled the net component and the controller and
node agents are running:

```bash
# Operator
kubectl -n unbounded-system get deploy unbounded-operator

# Controller pod
kubectl -n unbounded-system get pods -l app.kubernetes.io/name=unbounded-net-controller

# Node agent pods (one per node)
kubectl -n unbounded-system get pods -l app.kubernetes.io/name=unbounded-net-node -o wide

# Nodes should be labeled with their site
kubectl get nodes -L unbounded-cloud.io/site
```

If you have the kubectl plugin installed (`make kubectl-unbounded`):

```bash
kubectl unbounded net node list
```

Check controller health:

```bash
kubectl unbounded-system dashboard
```

## AKS-Specific Setup

### Creating an AKS cluster

Use `--network-plugin none` so that unbounded-system replaces the built-in CNI:

```bash
az aks create \
  --resource-group <resource-group> \
  --name <cluster-name> \
  --network-plugin none \
  --node-count 3 \
  --zones 1 2 3 \
  --generate-ssh-keys
```

### Adding an external gateway pool with instance-level public IPs

Create a node pool with public IPs for use as external gateways:

```bash
az aks nodepool add \
  --resource-group <rg> \
  --cluster-name <cluster> \
  --name extgw1 \
  --node-count 2 \
  --enable-node-public-ip \
  --node-public-ip-prefix <prefix-id> \
  --labels net.unbounded-cloud.io/agentpool=extgw1
```

### Required NSG rules for gateway nodes

Allow inbound UDP traffic to gateway nodes on these ports:

| Port(s) | Protocol | Purpose |
|----------|----------|---------|
| 51820-51830 | UDP | WireGuard mesh and gateway tunnels |
| 6081 | UDP | GENEVE (if using GENEVE for cross-site tunnels) |
| 4789 | UDP | VXLAN (if using VXLAN for cross-site tunnels) |

### Creating the gateway pool

```yaml
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: GatewayPool
metadata:
  name: extgw1
spec:
  nodeSelector:
    net.unbounded-cloud.io/agentpool: extgw1
  type: External
```

See [docs/templates/gatewaypool.yaml](templates/gatewaypool.yaml) for a fully
annotated example.

### Creating the site-gateway assignment

Link the site to the gateway pool so nodes can route traffic through gateways:

```yaml
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: SiteGatewayPoolAssignment
metadata:
  name: primary-extgw1
spec:
  enabled: true
  sites:
    - primary
  gatewayPools:
    - extgw1
```

See
[docs/templates/sitegatewaypoolassignment.yaml](templates/sitegatewaypoolassignment.yaml)
for a fully annotated example.

## Multi-Site Setup

To connect two sites, first create Site resources for each location, then
establish connectivity using either SitePeering (direct mesh) or
GatewayPoolPeering (routed through gateways).

### Direct peering with SitePeering

Use SitePeering when sites have direct network reachability (e.g., same cloud
region or VPN-connected). All nodes across the listed sites form direct tunnels:

```yaml
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: SitePeering
metadata:
  name: east-west
spec:
  sites:
    - site-east
    - site-west
  meshNodes: true
```

See [docs/templates/peering.yaml](templates/peering.yaml) for a fully annotated
example.

### Routed peering with GatewayPoolPeering

Use GatewayPoolPeering when sites are connected through gateway nodes (e.g.,
across the internet). Traffic flows through the gateway pools rather than
directly between all nodes:

```yaml
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: GatewayPoolPeering
metadata:
  name: gw-east-west
spec:
  gatewayPools:
    - gw-east
    - gw-west
```

## Next Steps

- [Architecture](architecture.md) -- system design, component roles, and data flow
- [Configuration](configuration.md) -- ConfigMap settings for controller and node agent
- [Custom Resources](custom-resources.md) -- full CRD reference with all fields
- [Operations](operations.md) -- health endpoints, metrics, and day-2 procedures
- [Troubleshooting](troubleshooting.md) -- common issues and diagnostic commands
