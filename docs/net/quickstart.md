<!-- Copyright (c) Microsoft Corporation. Licensed under the MIT License. -->

# Quick Start Guide

This guide walks you through deploying unbounded-net on a Kubernetes cluster,
creating your first Site, and optionally configuring gateway pools for
multi-site or AKS deployments.

## Prerequisites

- **Kubernetes cluster** (v1.27+) with WireGuard kernel module support on nodes
- **kubectl** configured for your cluster
- **Go toolchain** (if building from source or using `make -C hack/net deploy`)
- For **AKS**: Azure CLI (`az`), a subscription with permissions to create
  clusters, node pools, and public IP prefixes

## Deploying unbounded-net

You can deploy unbounded-net either with `make -C hack/net deploy` (recommended -- handles
template rendering and ordering) or by applying manifests manually.

### Option A: Deploy with Make (recommended)

```bash
# Deploy everything: CRDs, namespace, config, controller, and node agent
make -C hack/net deploy
```

By default this deploys into the `unbounded-net` namespace. Override with:

```bash
make -C hack/net deploy NET_NAMESPACE=kube-system
```

You can also deploy components individually:

```bash
make -C hack/net deploy-crds          # CRDs only
make -C hack/net deploy-controller    # Controller (includes CRDs, namespace, config)
make -C hack/net deploy-node          # Node agent (includes CRDs, namespace, config)
```

### Option B: Manual deployment

If you are working from a pre-rendered manifest archive or want to apply
resources step by step, follow the order below.

#### 1. Install CRDs

```bash
kubectl apply -f deploy/net/crd/
```

This installs the following CustomResourceDefinitions in the
`net.unbounded-cloud.io` API group:

| CRD | Short Name | Description |
|-----|-----------|-------------|
| `sites` | `st` | Network location containing nodes |
| `sitenodeslices` | `sns` | Controller-managed node membership slices |
| `gatewaypools` | `gp` | Pool of gateway nodes for inter-site routing |
| `gatewaypoolnodes` | `gpn` | Per-node gateway status (controller-managed) |
| `sitegatewaypoolassignments` | `sgpa` | Links sites to gateway pools |
| `sitepeerings` | `spr` | Direct peering between sites |
| `gatewaypoolpeerings` | `gpp` | Peering between gateway pools |

#### 2. Create namespace

Skip this step if deploying into `kube-system`.

```bash
kubectl create namespace unbounded-net
```

#### 3. Deploy controller

The controller manifests are in `deploy/net/controller/` (as `.yaml.tmpl`
templates that must be rendered -- see `hack/cmd/render-manifests`). After
rendering, apply them in order:

```bash
# Render templates first
make net-manifests

# Then apply rendered manifests
kubectl apply -f deploy/net/rendered/01-configmap.yaml
kubectl apply -f deploy/net/rendered/controller/
```

The controller deployment includes:

- **ConfigMap** (`unbounded-net-config`) -- shared configuration for controller
  and node agent, with sections for `common`, `controller`, and `node` settings
- **ServiceAccount**, **ClusterRole**, **ClusterRoleBinding** -- RBAC for
  managing nodes, CRDs, webhooks, and leader election leases
- **Deployment** (1 replica) -- runs with leader election enabled, non-root
  user, read-only root filesystem
- **Services** -- ClusterIP service for metrics/health (port 9999) and webhook
  service (port 9999)
- **ValidatingWebhookConfiguration** -- validates Site, GatewayPool, SitePeering,
  and other CRD mutations
- **MutatingWebhookConfiguration** -- mutates new nodes (e.g., pod CIDR
  assignment)
- **APIService** -- aggregated API server for node status push
- **ValidatingAdmissionPolicies** -- restricts which fields the controller
  service accounts can modify on webhooks, API services, and nodes

#### 4. Deploy node agent

The node agent manifests are in `deploy/net/node/` (also `.yaml.tmpl` templates).

```bash
kubectl apply -f deploy/net/rendered/node/
```

The node agent DaemonSet:

- Runs on **all nodes** (tolerates all taints, `system-node-critical` priority)
- Uses `hostNetwork: true` and `hostPID: true`
- Runs as **privileged** to manage network interfaces and routing
- **Init container** installs CNI plugins onto the host (`/opt/cni/bin`)
- **Host mounts**:
  - `/etc/cni/net.d` -- CNI configuration directory
  - `/opt/cni/bin` -- CNI plugin binaries
  - `/etc/wireguard` -- WireGuard key storage
  - `/etc/iproute2` -- iproute2 configuration (e.g., rt_tables)
- **Ports**: health on 9998/TCP, health check probe on 9997/UDP

#### 5. Create a Site

A Site defines a network location and its pod CIDR allocation. Create a Site
resource to start managing nodes:

```yaml
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: Site
metadata:
  name: primary
spec:
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
- `tunnelProtocol` -- tunnel encapsulation mode; `Auto` selects WireGuard for
  external IPs and GENEVE for internal IPs. Options: `WireGuard`, `IPIP`,
  `GENEVE`, `VXLAN`, `None`, `Auto`

See [docs/templates/site.yaml](templates/site.yaml) for a fully annotated
example with all optional fields.

#### 6. Verify

Check that the controller and node agents are running:

```bash
# Controller pod
kubectl -n unbounded-net get pods -l app.kubernetes.io/name=unbounded-net-controller

# Node agent pods (one per node)
kubectl -n unbounded-net get pods -l app.kubernetes.io/name=unbounded-net-node -o wide

# Nodes should be labeled with their site
kubectl get nodes -L net.unbounded-cloud.io/site
```

If you have the kubectl plugin installed (`make kubectl-unbounded`):

```bash
kubectl unbounded net node list
```

Check controller health:

```bash
kubectl unbounded-net dashboard
```

## AKS-Specific Setup

### Creating an AKS cluster

Use `--network-plugin none` so that unbounded-net replaces the built-in CNI:

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
