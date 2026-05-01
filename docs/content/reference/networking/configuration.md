---
title: "Configuration"
weight: 3
description: "All flags, environment variables, ConfigMap settings, and tuning guidance for unbounded-net."
---

This document describes all configuration options for unbounded-net components.
For a conceptual introduction, see [Networking Concepts]({{< relref "concepts/networking" >}}).

## Runtime Configuration

Both the controller and node agent load runtime settings from a shared YAML
file mounted from the `unbounded-net-config` ConfigMap.

- Default path: `/etc/unbounded-net/config.yaml`
- Override: `--config-file=<path>`
- Startup behavior: fail-fast if the config file is missing or invalid.
- CLI flags still work as explicit overrides when set.

### Config Structure

```yaml
common:
  azureTenantId: ""           # Only for Azure Portal links in the UI
  apiserverURL: ""            # Override API server URL (empty = in-cluster)
  logLevel: 2                 # klog verbosity (0-10), watched for live changes

controller:
  healthPort: 9999
  nodeAgentHealthPort: 9998
  informerResyncPeriod: 300s
  statusStaleThreshold: 40s
  registerAggregatedAPIServer: true
  leaderElection:
    enabled: true
    leaseDuration: 15s
    renewDeadline: 5s
    retryPeriod: 10s

node:
  cniConfDir: /host/etc/cni/net.d
  cniConfFile: 10-unbounded.conflist
  bridgeName: cbr0
  wireGuardDir: /host/etc/wireguard
  wireGuardPort: 51820
  mtu: 1280
  healthPort: 9998
  tunnelDataplane: ebpf
  tunnelDataplaneMapSize: 16384
  tunnelIPFamily: IPv4
  preferredPrivateEncap: GENEVE
  preferredPublicEncap: WireGuard
  genevePort: 6081
  geneveVni: 1
  vxlanPort: 4789
```

---

## Controller Configuration

### Leader Election

| Flag | Default | Description |
|------|---------|-------------|
| `--leader-elect` | `false` | Enable leader election for HA. |
| `--leader-elect-lease-duration` | `15s` | Duration of the leader lease. |
| `--leader-elect-renew-deadline` | `5s` | Deadline for renewing leadership. |
| `--leader-elect-retry-period` | `10s` | Retry period for acquiring leadership. |

### Health and Monitoring

| Flag | Default | Description |
|------|---------|-------------|
| `--health-port` | `9999` | Health check HTTP server port (0 to disable). |
| `--node-agent-health-port` | `9998` | Node agent health port (for dashboard links). |
| `--status-stale-threshold` | `40s` | Duration after which pushed status is stale. |
| `--register-aggregated-apiserver` | `true` | Enable aggregated API status endpoints. |
| `--informer-resync-period` | `300s` | Informer resync interval. |

### Logging

| Flag | Default | Description |
|------|---------|-------------|
| `-v` | `0` | Log verbosity level (0-10). |
| `--logtostderr` | `true` | Log to stderr. |

---

## Node Agent Configuration

### General

| Flag | Default | Env Var | Description |
|------|---------|---------|-------------|
| `--node-name` | -- | `NODE_NAME` | Name of this node (required). |
| `--health-port` | `9998` | -- | Health check server port. |
| `--informer-resync-period` | `3600s` | -- | Informer resync period. |
| `--route-table-id` | `252` | -- | Custom routing table ID. |
| `--preferred-private-encap` | `GENEVE` | -- | Preferred encap for internal links. |
| `--preferred-public-encap` | `WireGuard` | -- | Preferred encap for external links. |
| `--health-flap-max-backoff` | `120s` | -- | Max backoff for health check flap dampening. |

### CNI Configuration

| Flag | Default | Description |
|------|---------|-------------|
| `--cni-conf-dir` | `/etc/cni/net.d` | CNI configuration directory. |
| `--cni-conf-file` | `10-unbounded.conflist` | CNI configuration file name. |
| `--bridge-name` | `cbr0` | Bridge interface name. |
| `--mtu` | `1280` | MTU for tunnel and bridge interfaces. |

### MTU Guidance

The `node.mtu` setting controls the MTU on tunnel and bridge interfaces.
Encapsulation overhead:

| Type | Overhead |
|------|----------|
| WireGuard | 80 bytes |
| GENEVE / VXLAN | 58 bytes |
| IPIP | 20 bytes |

**Formula:** `node.mtu = <lowest physical-link MTU> - 80`

Using 80 (the largest overhead) ensures the value is safe for all tunnel types.
For standard 1500-byte links: `1500 - 80 = 1420`.

**Behavior:**
- Each node agent detects its default-route interface MTU and annotates itself
  with `net.unbounded-cloud.io/tunnel-mtu`.
- Effective MTU = `min(configured MTU, detected MTU)`.
- If configured MTU exceeds detected, the node logs an error and surfaces an
  `mtuMismatch` status error.
- A value of `0` is normalized to `1280`.

### WireGuard

| Flag | Default | Description |
|------|---------|-------------|
| `--wireguard-dir` | `/etc/wireguard` | WireGuard key storage directory. |
| `--wireguard-port` | `51820` | WireGuard listen port. |

### GENEVE

| Flag | Default | Description |
|------|---------|-------------|
| `--geneve-port` | `6081` | GENEVE UDP destination port. |
| `--geneve-vni` | `1` | GENEVE Virtual Network Identifier. |
| `--geneve-interface` | `geneve0` | GENEVE tunnel interface name. |

### VXLAN

| Flag | Default | Description |
|------|---------|-------------|
| `--vxlan-port` | `4789` | VXLAN UDP destination port. |
| `--vxlan-src-port-low` | `47891` | VXLAN source port range low. |
| `--vxlan-src-port-high` | `47922` | VXLAN source port range high. |

The narrow source port range (32 ports) limits distinct flows from VMs, helping
avoid flow table limits on cloud platforms (e.g., Azure).

### Tunnel Dataplane

| Flag | Default | Description |
|------|---------|-------------|
| `--tunnel-dataplane` | `ebpf` | `ebpf` (BPF LPM tries) or `netlink` (per-peer interfaces). |
| `--tunnel-dataplane-map-size` | `16384` | Max entries per BPF LPM trie map (eBPF only). |
| `--tunnel-ip-family` | `IPv4` | Underlay IP family for tunnel encapsulation (`IPv4` or `IPv6`). |

### Tunnel Protocol Selection

The `tunnelProtocol` field is available on all scope CRDs:

| Value | Overhead | Encrypted | Use Case |
|-------|----------|-----------|----------|
| `WireGuard` | 80 bytes | Yes | Cross-site links over public networks |
| `GENEVE` | 58 bytes | No | High-throughput internal links |
| `VXLAN` | ~58 bytes | No | Links with VXLAN hardware offload |
| `IPIP` | 20 bytes | No | Minimal overhead internal links |
| `None` | 0 bytes | No | Direct L3 routing |
| `Auto` | Varies | Varies | System selects based on link type (default) |

When `Auto`, links using external IPs use WireGuard; internal-only links use
the preferred private encap (default GENEVE). The **security-wins rule** ensures
WireGuard is used if any scope explicitly requests it.

See [Routing Flows]({{< relref "reference/networking/routing-flows" >}}) for
the full protocol selection algorithm.

### Status Push

| Flag | Default | Description |
|------|---------|-------------|
| `--status-push-enabled` | `true` | Push status to controller. |
| `--status-push-interval` | `10s` | Push interval. |
| `--status-ws-enabled` | `true` | Enable WebSocket transport. |
| `--status-ws-apiserver-mode` | `fallback` | `never`, `fallback`, or `preferred` for API server relay. |
| `--status-critical-interval` | `1s` | Max critical-delta publish frequency. |
| `--status-stats-interval` | `15s` | Max statistics-delta publish frequency. |

### Health Check (UDP Probes)

| Flag | Default | Description |
|------|---------|-------------|
| `--healthcheck-port` | `9997` | UDP health check probe port (0 to disable). |
| `--base-metric` | `1` | Base metric for programmed routes. |

Health check sessions are automatically created for all routes with nexthops.
Route metric adjustment on failure provides fast failover.

---

## Firewall Requirements

| Port | Protocol | Direction | Purpose |
|------|----------|-----------|---------|
| 51820 | UDP | Inbound | WireGuard intra-site mesh |
| 51821+ | UDP | Inbound | WireGuard gateway links |
| 6081 | UDP | Inbound | GENEVE tunnels |
| 4789 | UDP | Inbound | VXLAN tunnels |
| 9997 | UDP | Inbound | Health check probes |
| 9998 | TCP | Inbound | Node status endpoint |

---

## Resource Requirements

### Controller

```yaml
resources:
  requests: { cpu: 10m, memory: 64Mi }
  limits:   { cpu: 100m, memory: 128Mi }
```

### Node Agent

```yaml
resources:
  requests: { cpu: 10m, memory: 32Mi }
  limits:   { cpu: 100m, memory: 64Mi }
```

### Scaling

| Cluster Size | Controller Memory | Notes |
|--------------|-------------------|-------|
| < 100 nodes | 64Mi | Low informer load |
| 100-500 nodes | 128Mi | Medium |
| 500-1000 nodes | 256Mi | Consider longer resync |
| > 1000 nodes | 512Mi+ | Use `--informer-resync-period=600s` |

---

## Environment Variables

### Controller

| Variable | Description |
|----------|-------------|
| `LOG_LEVEL` | Initial klog verbosity. For live changes, edit `common.logLevel` in ConfigMap. |
| `POD_NAME` | Pod name for leader election identity (downward API). |
| `POD_NAMESPACE` | Namespace for leader election lease (default: `kube-system`). |
| `POD_IP` | Pod IP for EndpointSlice management. |
| `NODE_NAME` | Node name, displayed in dashboard. |

### Node Agent

| Variable | Description |
|----------|-------------|
| `LOG_LEVEL` | Initial klog verbosity. |
| `NODE_NAME` | Node name (required, downward API). |

---

## Tuning Recommendations

### High Availability

```yaml
replicas: 2
args:
  - --leader-elect=true
  - --leader-elect-lease-duration=15s
  - --leader-elect-renew-deadline=5s
```

### Large Clusters (1000+ nodes)

```yaml
# Controller
args:
  - --informer-resync-period=600s

# Node agent
args:
  - --informer-resync-period=7200s
```

## Next Steps

- **[Custom Resources]({{< relref "reference/networking/custom-resources" >}})** --
  CRD specifications.
- **[Routing Flows]({{< relref "reference/networking/routing-flows" >}})** --
  Packet-level routing details.
- **[Operations]({{< relref "reference/networking/operations" >}})** --
  Deployment, monitoring, and troubleshooting.
