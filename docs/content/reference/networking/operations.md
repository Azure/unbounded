---
title: "Operations"
weight: 5
description: "Deployment, monitoring, troubleshooting, and operational procedures for unbounded-net."
---

This guide covers deployment, monitoring, troubleshooting, and day-2 operations.
For configuration details, see
[Configuration]({{< relref "reference/networking/configuration" >}}).

## Deployment

### Prerequisites

1. **Kubernetes cluster** (1.24+)
2. **WireGuard** kernel module on all nodes (for encrypted tunnels), or
   eBPF/TC kernel support (for GENEVE/VXLAN/IPIP tunnels)
3. Container runtime with CNI support
4. Network connectivity between sites (UDP ports)

### Verifying WireGuard Support

```bash
# Check if WireGuard module is loaded
lsmod | grep wireguard

# Load if needed
modprobe wireguard

# Verify tools
wg --version
```

### Installation Steps

1. **Deploy CRDs**: `kubectl apply -f deploy/machina/crd/`
2. **Deploy Controller**: `kubectl apply -f deploy/controller/`
3. **Deploy Node Agent**: `kubectl apply -f deploy/node/`
4. **Create Sites**: Define Site resources with `nodeCidrs` and
   `podCidrAssignments`.
5. **Create GatewayPools**: Define pools with `nodeSelector`.
6. **Assign Sites to Pools**: Create SiteGatewayPoolAssignment resources.
7. **Label Gateway Nodes**:
   `kubectl label node <name> net.unbounded-cloud.io/gateway=true`
8. **Verify Connectivity**: Test with pod-to-pod ping across sites.

> **Note:** When using unbounded-kube, steps 1-7 are handled automatically by
> `kubectl unbounded site init`. See the
> [Getting Started guide]({{< relref "guides/getting-started" >}}).

---

## Monitoring

### Web Dashboard

The controller provides a real-time web dashboard:

```bash
kubectl -n kube-system port-forward deploy/unbounded-net-controller 9999:9999
# Open http://localhost:9999/status
```

Features:
- Cluster health overview (node counts, site counts, gateway status)
- Per-site node counts and health indicators
- Node-to-node connectivity matrix (pingmesh results)
- Detailed node list with filtering, sorting, and pagination
- Tunnel peer status, gateway health, site membership
- WebSocket real-time updates with delta compression
- Dark/light theme toggle

### Health Endpoints

| Component | Endpoint | Purpose |
|-----------|----------|---------|
| Controller | `:9999/healthz` | Liveness |
| Controller | `:9999/readyz` | Readiness |
| Controller | `:9999/status/json` | Cluster status JSON |
| Controller | `:9999/status/node/<name>` | Per-node status |
| Node Agent | `:9998/healthz` | Liveness |
| Node Agent | `:9998/readyz` | Readiness |
| Node Agent | `:9998/status/json` | Full node status JSON |
| Node Agent | `:9998/metrics` | Prometheus metrics |

### Prometheus Metrics

All components expose metrics at `/metrics`:

| Component | Port | Description |
|-----------|------|-------------|
| Controller (HTTP) | 9999 | Controller, client-go, Go runtime metrics |
| Controller (TLS) | 9443 | Same metrics via webhook TLS port |
| Node Agent | 9998 | Node agent, client-go, Go runtime metrics |

All pods carry `prometheus.io/*` annotations for automatic discovery.

#### Key Custom Metrics

**Controller:**
- `reconciliation_duration_seconds` / `reconciliation_total`
- `site_nodes_total` (per site)
- `pod_cidr_allocations_total` / `pod_cidr_exhaustion_total`
- `gateway_pool_nodes_total` (per pool)
- `leader_is_leader` (1/0)
- `websocket_connections`

**Node Agent:**
- `wireguard_peers` (per interface)
- `routes_installed` (per table)
- `status_push_total` / `status_push_duration_seconds`
- `cni_config_writes_total`

**Health Check:**
- `peer_state` (0=down, 1=up, 2=admin-down, per peer)
- `probe_duration_seconds` (per peer)
- `probes_sent_total` / `probes_received_total`

### Viewing Tunnel Status

**WireGuard mode:**
```bash
wg show all
```

**eBPF mode -- verify BPF attachment:**
```bash
tc filter show dev unbounded0 egress
# Expected: bpf filter with "unbounded_encap direct-action"
```

**eBPF mode -- dump BPF maps:**
```bash
bpftool map list | grep unbounded
bpftool map dump name unbounded_endpo  # kernel truncates name to 15 chars
```

**Using the `unroute` diagnostic tool** (included in node agent image):
```bash
kubectl -n kube-system exec <node-agent-pod> -- unroute           # dump all
kubectl -n kube-system exec <node-agent-pod> -- unroute <ip>      # lookup
kubectl -n kube-system exec <node-agent-pod> -- unroute --local   # local CIDRs
```

**Via kubectl plugin:**
```bash
kubectl unbounded net node show <name> bpf     # BPF entries
kubectl unbounded net node show <name> routes  # routes
kubectl unbounded net node show <name> peers   # peers
kubectl unbounded net node show <name> json    # full status
```

### Interface Verification

**eBPF mode:**
```bash
ip link show unbounded0       # Should have NOARP flag
ip link show geneve0          # Flow-based GENEVE (if active)
ip link show vxlan0           # Flow-based VXLAN (if active)
ip link show ipip0            # Shared IPIP (if active)
ip route show dev unbounded0  # Supernet routes (scope global)
```

**WireGuard mode:**
```bash
ip link show type wireguard
ip route show dev wg51820
ip route show dev wg51821
```

---

## Troubleshooting

### Nodes Not Getting Pod CIDRs

**Symptoms:** Node has no `spec.podCIDRs`; pods stuck in `ContainerCreating`.

**Check:**
```bash
kubectl get node <name> -L net.unbounded-cloud.io/site
kubectl get sites -o yaml
kubectl -n kube-system logs -l app=unbounded-net-controller | grep -i alloc
```

**Common causes:**
- Node internal IP doesn't match any Site `nodeCidrs`.
- No matching `nodeRegex` in the site's assignments.
- Assignment has `assignmentEnabled: false`.
- CIDR pools exhausted (controller exits fatally).
- Controller not running or not leader.

### WireGuard Tunnels Not Establishing

**Symptoms:** No WireGuard handshakes; pod-to-pod fails.

**Check:**
```bash
ip link show wg51820
wg show wg51820
kubectl get node <name> -o jsonpath='{.metadata.annotations.net\.unbounded-kube\.io/wg-pubkey}'
```

**Common causes:**
- Firewall blocking UDP 51820.
- WireGuard kernel module not loaded (`modprobe wireguard`).
- Node not labeled with site.

### Cross-Site Traffic Failing

**Symptoms:** Intra-site works, cross-site times out.

**Check:**
```bash
kubectl get gp -o yaml
kubectl get gp main-gateways -o jsonpath='{.status.nodes[*].externalIPs}'
ip route | grep <remote-site-cidr>
```

**Common causes:**
- No gateways configured or labeled.
- Gateways not reachable (firewall on external IPs).
- Health checks failing.

### Gateway Health Check Failures

**Symptoms:** Routes to remote sites disappear.

**Check:**
```bash
curl -v http://<gateway-health-ip>:9998/healthz
kubectl -n kube-system logs <gateway-node-agent-pod>
```

**Common causes:**
- Gateway node agent not running.
- Health server not started.
- Network partition.

### Dashboard Shows Stale Data

**Symptoms:** Nodes show "Stale cache" status.

**Check:**
```bash
kubectl -n kube-system get endpointslices -l kubernetes.io/service-name=unbounded-net-controller
kubectl -n kube-system get endpoints unbounded-net-controller 2>&1
```

**Common causes:**
- Stale `v1/Endpoints` from a previous controller version. The controller
  cleans these on leader election, but during upgrades it may be needed:
  `kubectl -n kube-system delete endpoints unbounded-net-controller`

> **Note:** The controller Service has **no selector**. The leader manages its
> own EndpointSlice. Do not add a selector.

### Diagnostic Commands

```bash
# Cluster overview
kubectl get st                                    # Sites
kubectl get gp                                    # Gateway pools
kubectl get nodes -L net.unbounded-cloud.io/site   # Node assignments

# Per-node (eBPF)
tc filter show dev unbounded0 egress              # BPF program
ip route show dev unbounded0                      # Supernet routes
ip link | grep -E 'unbounded0|geneve0|vxlan0|ipip0'

# Per-node (WireGuard)
wg show all

# Per-node (common)
ip route show table main | grep -E 'wg|cbr|unbounded'
cat /etc/cni/net.d/10-unbounded.conflist

# Controller
kubectl -n kube-system get lease unbounded-net-controller -o yaml
kubectl -n kube-system logs -l app=unbounded-net-controller --tail=100

# Node agent
kubectl -n kube-system logs -l app=unbounded-net-node --tail=100
```

### Debug Logging

```yaml
args:
  - -v=4   # 0=errors only, 2=normal, 3=detailed, 4+=debug
```

---

## Operational Procedures

### Adding a New Site

1. Create Site resource with `nodeCidrs` and `podCidrAssignments`.
2. Create SiteGatewayPoolAssignment to bind site to a gateway pool.
3. Deploy nodes whose IPs fall within the site's `nodeCidrs`.
4. Label a gateway node:
   `kubectl label node <name> net.unbounded-cloud.io/gateway=true`
5. Verify: `kubectl get gp <pool> -o yaml`

### Removing a Site

1. Drain workloads from site nodes.
2. Remove gateway labels.
3. Delete the Site: `kubectl delete site <name>`
4. SiteNodeSlices are automatically garbage collected.

### Replacing a Gateway Node

1. Label the new gateway node.
2. Verify it appears in the pool: `kubectl get gp <pool> -o yaml`
3. Wait for routes to update (~10s health check interval).
4. Remove old gateway label.
5. Drain old node if needed.

### Expanding CIDR Pools

Edit the Site to add CIDR blocks under `podCidrAssignments[].cidrBlocks`.

### Rolling Restart

```bash
kubectl -n kube-system rollout restart daemonset/unbounded-net-node
kubectl -n kube-system rollout status daemonset/unbounded-net-node
```

---

## Backup and Recovery

### What to Backup

```bash
kubectl get sites -o yaml > sites-backup.yaml
kubectl get gatewaypools -o yaml > gatewaypools-backup.yaml
kubectl get sitepeerings -o yaml > sitepeerings-backup.yaml
kubectl get sitegatewaypoolassignments -o yaml > sgpa-backup.yaml
kubectl get gatewaypoolpeerings -o yaml > gpp-backup.yaml
```

SiteNodeSlices and GatewayPoolNodes are automatically regenerated and don't
need backup.

### Recovery

1. Restore CRDs: `kubectl apply -f deploy/machina/crd/`
2. Restore resources from backup YAMLs.
3. Deploy controller and node agent.
4. Re-apply gateway labels.

---

## Security Considerations

### Network Policies

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-unbounded-net
  namespace: kube-system
spec:
  podSelector:
    matchLabels:
      app: unbounded-net-node
  policyTypes: [Ingress, Egress]
  ingress:
    - ports:
        - { protocol: UDP, port: 51820 }
        - { protocol: TCP, port: 9998 }
  egress:
    - {}
```

### Key Rotation

```bash
# On the node:
rm /etc/wireguard/server.priv /etc/wireguard/server.pub
# Then restart the node agent pod -- briefly disrupts connectivity.
```

### Audit Logging

```yaml
apiVersion: audit.k8s.io/v1
kind: Policy
rules:
  - level: Metadata
    resources:
      - group: "net.unbounded-cloud.io"
        resources: ["sites", "sitenodeslices", "gatewaypools",
                    "gatewaypoolnodes", "sitepeerings",
                    "sitegatewaypoolassignments", "gatewaypoolpeerings"]
```

## Next Steps

- **[Architecture]({{< relref "reference/networking/architecture" >}})** --
  System internals.
- **[Custom Resources]({{< relref "reference/networking/custom-resources" >}})** --
  CRD specifications.
- **[Configuration]({{< relref "reference/networking/configuration" >}})** --
  All flags and settings.
