---
title: "Operations"
weight: 5
description: "Day-2 procedures for changing and troubleshooting Unbounded networking, including sites, gateway pools, peerings, CIDR growth, upgrades, health checks, and metrics."
---

Unbounded networking is declarative. You describe Sites, GatewayPools, and peerings in the Kubernetes API, and the networking controller translates that intent into node labels, pod CIDR assignments, gateway membership, and route advertisements. Each node agent then turns the assigned topology into WireGuard peers, tunnel interfaces, kernel routes, and eBPF map entries. Day-2 operations are therefore mostly about changing the right object, watching it converge, and knowing which signal proves that the dataplane agrees with the API.

The controller is a cluster singleton that runs in the `unbounded-system` namespace. Only the leader performs pod CIDR allocation and publishes gateway pool assignments, while node agents write their own node annotations and route advertisements. Node agents run as a DaemonSet on every node, including gateway nodes, and they watch the same objects. This split matters when you troubleshoot: a missing node label or pod CIDR is usually a controller problem, while a missing peer, route, or handshake is usually a node agent problem. Cross-site reachability needs both sides to agree.

This page covers the operational tasks you perform after the first install: adding and removing sites, changing gateway pools and peerings, expanding pod CIDR pools, rolling node agents, checking health, reading metrics, and diagnosing the failures that appear most often in production. For the resource model itself, see [Custom resources]({{< relref "reference/networking/custom-resources" >}}). For packet paths, see [Routing flows]({{< relref "reference/networking/routing-flows" >}}).

## Deployment

### Prerequisites

The networking dataplane assumes Linux worker nodes with a modern kernel. Before you install or add a site, confirm the following:

- The cluster API is reachable from every node that will run the node agent.
- Nodes support eBPF programs attached through the traffic control `clsact` qdisc. The agent attaches the `unbounded_encap` program to the `unbounded0` dummy interface.
- Nodes that will use encrypted tunnels have the WireGuard kernel module available. Tunnels selected as GENEVE, VXLAN, IPIP, or None do not require WireGuard.
- The container runtime and kubelet support CNI configuration. The agent writes `10-unbounded.conflist` when the site manages the CNI plugin.
- Sites can reach each other over UDP. WireGuard mesh traffic uses port 51820 by default, gateway-to-gateway peering uses ports allocated from 51821 upward, and health checks use UDP 9997 over the overlay.

Check WireGuard support on a node before you create a site that depends on it:

```bash
lsmod | grep wireguard
modprobe wireguard
wg --version
```

If `modprobe` succeeds but `lsmod` still shows nothing, the module is built into the kernel or the node image does not provide it. In the second case, choose an unencrypted tunnel protocol for links that stay inside a trusted network, or use a node image that includes WireGuard. The controller does not load kernel modules on your behalf.

### Bootstrapping the control components

`kubectl unbounded install` bootstraps the operator, not every component directly. The operator establishes the CRDs, deploys the networking controller and node DaemonSet when at least one Site exists, and keeps them reconciled afterward:

```bash
kubectl unbounded install
kubectl -n unbounded-system rollout status deployment/unbounded-operator
kubectl get crd | grep unbounded
```

The networking resources are cluster-scoped and use two API groups. Sites are `unbounded-cloud.io/v1alpha3`; GatewayPools, SiteGatewayPoolAssignments, SitePeerings, GatewayPoolPeerings, SiteNodeSlices, and GatewayPoolNodes are `net.unbounded-cloud.io/v1alpha1`.

{{< callout type="note" >}}
Networking is a cluster singleton. The current Site schema has no `spec.components.net.enabled` field. A Site with no components block still causes the operator to deploy networking, and the networking workloads are retained even if the last Site is deleted.
{{< /callout >}}

Wait for the operator to report the site condition before you debug the dataplane:

```bash
kubectl get site <site-name> -o jsonpath='{range .status.conditions[*]}{.type}={.status}{"\n"}{end}'
kubectl wait --for=condition=NetReady site/<site-name> --timeout=10m
```

### Creating the first Site and gateway pool

A Site defines a network boundary by node CIDR. The controller matches each node's internal IP against `spec.nodeCidrs`; a match is what makes the networking controller assign that node to the site and write the site label and pod CIDRs. Node CIDRs must not overlap another Site's node CIDRs, and pod CIDR blocks must not overlap any other pod block, even within the same Site, because the admission webhook rejects ambiguous ownership.

Create a site with an explicit node CIDR, a pod CIDR pool, and a per-node block size:

```bash
kubectl unbounded net create site site-east \
  --node-cidr 10.0.0.0/16 \
  --pod-cidr-block 10.244.0.0/16 \
  --pod-node-block-size-ipv4 24
```

Repeat for each independent location. If the cluster already runs Cilium, Calico, or another CNI, keep the pod CIDR blocks because they define inter-site routing, but disable CNI management and per-node assignment:

```bash
kubectl unbounded net create site site-west \
  --node-cidr 10.1.0.0/16 \
  --pod-cidr-block 10.245.0.0/16 \
  --pod-assignment-enabled=false \
  --no-manage-cni-plugin
```

A GatewayPool selects gateway nodes by label. The label is yours to choose; `role=gateway` is a common convention, while `unbounded-cloud.io/unbounded-net-gateway=true` is used by `kubectl unbounded site init`. External pools require nodes with an external IP and a published WireGuard key. Internal pools are for links that stay inside the cluster network.

```bash
kubectl unbounded net create gatewaypool main-gateways \
  --type External \
  --node-selector role=gateway \
  --routed-cidr 10.100.0.0/16

kubectl label node gateway-east-1 role=gateway
```

Bind sites to the pool, then verify that the pool selected the node:

```bash
kubectl unbounded net create sitegatewaypoolassignment main-assignment \
  --site site-east \
  --site site-west \
  --gateway-pool main-gateways

kubectl get gp main-gateways -o yaml
kubectl get nodes -L unbounded-cloud.io/site,net.unbounded-cloud.io/gateway-pool
```

For a fresh cluster that needs both a local `cluster` Site and a named remote Site, `kubectl unbounded site init` creates the pair, a `gw-main` gateway pool, the matching assignments, and a bootstrap token:

```bash
kubectl unbounded site init \
  --name site-edge \
  --cluster-node-cidr 10.0.0.0/16 \
  --cluster-pod-cidr 10.244.0.0/16 \
  --node-cidr 10.2.0.0/16 \
  --pod-cidr 10.246.0.0/16
```

Use `net create` for day-2 additions. `site init` is a bootstrap helper that also manages a bootstrap token and the local cluster site.

### Verifying the dataplane

Do not stop at `kubectl get`. Verify that the controller assigned pod CIDRs, that the node agent created a tunnel, and that traffic crosses it:

```bash
kubectl get node <node-name> -o jsonpath='{.spec.podCIDRs}{"\n"}'
kubectl get nodes -L unbounded-cloud.io/site
kubectl get gp main-gateways -o jsonpath='{.status.nodeCount}{"\n"}'
kubectl unbounded net node show <gateway-node> peers
```

Then run a pod-to-pod test from one site to a pod in another:

```bash
kubectl run net-test --image=busybox --rm -it --restart=Never -- \
  ping -c 3 <pod-ip-in-remote-site>
```

A successful ping proves that address assignment, route advertisement, tunnel setup, and the return path all work. A failure at this stage is easier to debug before workloads depend on the path.

## Monitoring

The system exposes three views of the same topology. The Kubernetes API holds desired state and controller-written assignments. The controller status API holds a leader-maintained summary of what each node last reported. Prometheus metrics hold counters, gauges, and histograms that explain why a value is changing. Use all three when a problem spans the controller and the dataplane.

Node agents publish a full status snapshot periodically, 60 seconds by default, and send delta updates between snapshots. The controller treats a node's pushed status as stale after 90 seconds and falls back to pulling status directly from that node. The dashboard and the CLI therefore show the most recent successful push or pull, not a live packet capture.

### Web dashboard

The controller serves an authenticated dashboard on its HTTPS port. The plugin starts a local HTTP-to-HTTPS proxy, fetches the controller CA from the `unbounded-net-serving-ca` ConfigMap, and obtains a short-lived viewer token:

```bash
kubectl unbounded net dashboard
```

Use `--no-browser` when you want the URL without launching a browser, or use the proxy directly:

```bash
kubectl unbounded net controller proxy --no-browser
```

The proxy listens on `127.0.0.1:9999` by default. The dashboard shows the cluster summary, per-site node counts, node role and site membership, tunnel peer counts, gateway health, and pod information. It supports filtering, sorting, pagination, and a light or dark theme. It updates over a WebSocket with HTTP polling as a fallback. Selecting a node does not collect diagnostics; choose **Load data** to reuse an unexpired snapshot or **Refresh** to request a fresh collection.

The view is a summary. For the raw per-node detail, use `kubectl unbounded net node show <node-name> json`, which returns the diagnostic snapshot instead of the dashboard projection.

### Health endpoints

The controller serves probes and status on a single HTTPS listener. The node agent serves plain HTTP on its own health port. The distinction matters when you write probes, scrape configuration, or a `curl` command.

| Component | Port | Scheme | Path | Meaning |
| --- | --- | --- | --- | --- |
| Controller | 9999 | HTTPS | `/healthz` | Liveness. The controller can reach the Kubernetes API. |
| Controller | 9999 | HTTPS | `/readyz` | Readiness. Token verification, node-auth informer caches, and API access are ready. Leadership is not required. |
| Controller | 9999 | HTTPS | `/status/json` | Leader-only cluster summary. Authentication and authorization are required by default. |
| Controller | 9999 | HTTPS | `/status/node/<name>` | Cached per-node diagnostics. Authentication and authorization are required by default. |
| Controller | 9999 | HTTPS | `/metrics` | Prometheus metrics. The shipped scrape annotation uses the HTTPS scheme. |
| Node agent | 9998 | HTTP | `/healthz` | Liveness. The status server is serving. |
| Node agent | 9998 | HTTP | `/readyz` | Readiness. Informers are synced, gateway informers are ready, and CNI prerequisites are met. |
| Node agent | 9998 | HTTP | `/status/json` | Full node status, including tunnels, routes, health, and BPF entries. |
| Node agent | 9998 | HTTP | `/status/summary` | Node overview without the large diagnostic arrays. |
| Node agent | 9997 | UDP | health probe | Peer health checks over the overlay. |

To probe the controller without dealing with TLS by hand, use the plugin proxy:

```bash
kubectl unbounded net controller proxy --no-browser &
curl -fsS http://127.0.0.1:9999/healthz
curl -fsS http://127.0.0.1:9999/readyz
```

To probe a node agent, find the pod on the node and forward its port:

```bash
kubectl -n unbounded-system get pod \
  -l app.kubernetes.io/name=unbounded-net-node \
  --field-selector spec.nodeName=<node-name>

kubectl -n unbounded-system port-forward pod/<node-agent-pod> 9998:9998
curl -fsS http://127.0.0.1:9998/healthz
curl -fsS http://127.0.0.1:9998/readyz
```

A controller that is healthy but not the leader returns `503` from `/status/json`. That is expected. Check the lease before you assume the controller is broken:

```bash
kubectl -n unbounded-system get lease unbounded-net-controller \
  -o jsonpath='{.spec.holderIdentity}{"\n"}'
```

### Prometheus metrics

Both workloads expose `/metrics` through the shared metrics registry. The shipped pod annotations are enough for annotation-based discovery:

| Workload | Annotation port | Annotation path | Annotation scheme |
| --- | --- | --- | --- |
| `unbounded-net-controller` | 9999 | `/metrics` | `https` |
| `unbounded-net-node` | 9998 | `/metrics` | `http` (default) |

The controller metrics use the `unbounded_cni_controller_` prefix. The table lists the names without the prefix; prepend it when you query or alert. The most useful metrics for day-2 work are:

| Metric | Type | Labels | Use |
| --- | --- | --- | --- |
| `reconciliation_total` | counter | `controller`, `result` | Detect a controller that is retrying or failing. |
| `reconciliation_errors_total` | counter | `controller` | Identify which controller is failing. |
| `reconciliation_duration_seconds` | histogram | `controller` | Spot slow reconciles after a large site change. |
| `site_nodes_total` | gauge | `site` | Confirm site membership counts. |
| `site_node_slices_total` | gauge | `site` | Confirm slice fan-out for large sites. |
| `pod_cidr_allocations_total` | counter | none | Confirm new nodes are receiving CIDRs. |
| `pod_cidr_exhaustion_total` | counter | none | Detect a pool that has run out. |
| `gateway_pool_nodes_total` | gauge | `pool` | Confirm gateway selection. |
| `gateway_pool_reachable_sites_total` | gauge | `pool` | Confirm peering and route propagation. |
| `gateway_pool_routes_total` | gauge | `pool` | Confirm advertised routes through a pool. |
| `leader_is_leader` | gauge | none | Identify the active controller. |
| `websocket_connections` | gauge | none | Confirm node status streams are connected. |
| `node_status_pushes_total` | counter | `method`, `result` | Detect rejected or failed status pushes. |
| `peer_measurement_updates_total` | counter | `outcome` | Detect repeated resync or error outcomes. |
| `webhook_requests_total` | counter | `resource`, `operation`, `result` | Detect admission failures during edits. |

The node agent uses `unbounded_cni_node_` for its own metrics. As with the controller table, the names omit the prefix. The transport metrics come from the same prefix and expose the state of the dataplane:

| Metric | Type | Labels | Use |
| --- | --- | --- | --- |
| `reconciliation_total` | counter | `result` | Detect repeated node reconcile failures. |
| `reconciliation_duration_seconds` | histogram | none | Measure the cost of a full node pass. |
| `wireguard_peers` | gauge | `interface` | Confirm peer count on each WireGuard device. |
| `wireguard_configure_errors_total` | counter | `interface` | Detect failed peer configuration. |
| `routes_installed` | gauge | `table` | Confirm the route table is populated. |
| `route_sync_errors_total` | counter | `source` | Detect route programming failures. |
| `ecmp_routes_installed` | gauge | none | Confirm multipath gateway routes. |
| `masquerade_rules` | gauge | `family` | Confirm SNAT rules exist. |
| `masquerade_sync_errors_total` | counter | none | Detect iptables or nftables failures. |
| `interfaces` | gauge | `type` | Confirm managed devices exist. |
| `interface_operation_errors_total` | counter | `operation` | Detect link create or delete failures. |
| `status_push_total` | counter | `method`, `result` | Detect a node that cannot publish status. |
| `status_push_duration_seconds` | histogram | `method` | Measure status publication latency. |
| `cni_config_writes_total` | counter | `result` | Confirm CNI configuration writes. |
| `public_ip_discovery_failures_total` | counter | none | Detect NAT or STUN discovery problems. |

Health checking adds metrics with the `healthcheck_` prefix on the node agent:

| Metric | Type | Labels | Use |
| --- | --- | --- | --- |
| `healthcheck_peers` | gauge | `state` | Count peers in `Up`, `Down`, and `AdminDown`. |
| `healthcheck_session_flaps_total` | counter | none | Detect unstable links. |
| `healthcheck_rtt_seconds` | histogram | none | Measure probe round-trip time. |
| `healthcheck_packets_sent_total` | counter | none | Confirm probes are being transmitted. |
| `healthcheck_packets_received_total` | counter | none | Confirm replies are arriving. |
| `healthcheck_packets_timeout_total` | counter | none | Confirm missed probes. |

A useful starting alert is a gateway pool with `gateway_pool_nodes_total == 0` while its assignment still exists, because that removes the path for every site bound to the pool. A second is a rising `pod_cidr_exhaustion_total` on the controller, because site-based allocation treats exhaustion as fatal and restarts the controller.

### Viewing tunnel and dataplane state

Run dataplane commands inside the node agent pod. The pod uses the host network namespace, so `ip`, `wg`, and `tc` see the node's real devices:

```bash
kubectl unbounded net node exec <node-name> -- wg show all
kubectl unbounded net node exec <node-name> -- ip link show unbounded0
kubectl unbounded net node exec <node-name> -- ip route show dev unbounded0
kubectl unbounded net node exec <node-name> -- tc filter show dev unbounded0 egress
```

The eBPF program uses a single LPM trie named `unb_endpts`. The shipped node image includes `unroute` but not `bpftool`; use `unroute` for the map contents, or run `bpftool` on the host when it is installed there:

```bash
# On a host that has bpftool installed:
bpftool map dump name unb_endpts
```

`unroute` is the project-specific view of the same map. It annotates entries with node and site information from the local status endpoint:

```bash
kubectl unbounded net node exec <node-name> -- unroute
kubectl unbounded net node exec <node-name> -- unroute 10.244.1.5
kubectl unbounded net node exec <node-name> -- unroute -4
kubectl unbounded net node exec <node-name> -- unroute --raw
kubectl unbounded net node exec <node-name> -- unroute --trace
```

Use `--json` for machine-readable output and `--trace` to stream per-packet events from the eBPF program. Stop a trace with Ctrl-C. The plugin also renders the same information without a shell:

```bash
kubectl unbounded net node show <node-name> peers
kubectl unbounded net node show <node-name> routes
kubectl unbounded net node show <node-name> bpf
kubectl unbounded net node show <node-name> json
```

### Interface verification

The dataplane uses a dummy device to attract overlay traffic and protocol-specific devices for encapsulation. Only devices for active protocols should exist.

| Interface | Type | Role |
| --- | --- | --- |
| `unbounded0` | dummy, NOARP | Attracts pod and overlay traffic; the TC eBPF program encapsulates it. |
| `wg51820` | WireGuard | Default mesh interface for nodes using WireGuard. |
| `wg51821` and above | WireGuard | Gateway-to-gateway interfaces using allocated ports. |
| `geneve0` | GENEVE, flow-based | Shared device for GENEVE peers. |
| `vxlan0` | VXLAN, flow-based | Shared device for VXLAN peers. |
| `ipip0` | IPIP, flow-based | Shared device for IPIP peers. |

Verify the dummy device has the `NOARP` flag and that the expected routes point at it:

```bash
kubectl unbounded net node exec <node-name> -- ip link show unbounded0
kubectl unbounded net node exec <node-name> -- ip route show dev unbounded0
kubectl unbounded net node exec <node-name> -- ip -d link show geneve0
```

`unbounded0` should report a state such as `UP,LOWER_UP` with `NOARP` in the flags. The supernet routes on it are the prefixes that the BPF program serves. Per-peer destinations live in the `unb_endpts` trie, not as individual kernel routes, which keeps the route table small in large clusters.

### Migrating from Cilium

There are two different goals, and they require different work. Coexistence keeps Cilium as the intra-site CNI while Unbounded networking carries cross-site traffic through gateway pools. Replacement removes Cilium and lets Unbounded networking own pod networking.

For coexistence, create the Site with `manageCniPlugin: false` and disable pod CIDR assignment. The site's `podCidrAssignments` are still required because the controller uses them to identify remote pod ranges, but the agent does not write CNI configuration and does not create same-site tunnel peers:

```bash
kubectl unbounded net create site site-west \
  --node-cidr 10.1.0.0/16 \
  --pod-cidr-block 10.245.0.0/16 \
  --pod-assignment-enabled=false \
  --no-manage-cni-plugin
```

For replacement, drain and re-image nodes where possible. Re-imaging removes Cilium's links, routes, iptables or nftables rules, and CNI configuration in one step. If you must reuse nodes in place, uninstall or scale down Cilium using its own documented procedure, then remove its remaining CNI configuration from `/etc/cni/net.d`. This repository does not ship a `post-uninstall-cleanup` helper; use the CNI project's cleanup path and verify the result yourself.

Check that stale state is gone before you let the node agent write `10-unbounded.conflist`:

```bash
ip link show | grep -E 'cilium|lxc|vxlan'
ip rule show
ls -la /etc/cni/net.d
```

Remove leftover Cilium conflists and confirm that socket-level load-balancing or service maps are not still present. After the node agent restarts, it writes `10-unbounded.conflist` and logs a warning if a lexicographically earlier file, such as `05-cilium.conflist`, still wins CNI selection. Do not run two active CNI plugins on the same node and expect predictable pod networking.

Drain and quiesce workloads before you switch the active CNI. After the switch, verify that the networking workloads are present and that the site reports `NetReady`:

```bash
kubectl -n unbounded-system get pods -l app.kubernetes.io/name=unbounded-net-node
kubectl -n unbounded-system get pods -l app.kubernetes.io/name=unbounded-net-controller
kubectl wait --for=condition=NetReady site/<site-name> --timeout=10m
```

If pod networking does not recover after cleanup, check the node agent logs and confirm that the TC program is attached to `unbounded0`. Avoid ad hoc BPF surgery. Restart the node agent first, and re-image the node if the attachment still fails.

## Troubleshooting

Most failures fall into one of four layers: the controller did not assign state, the node agent did not program the dataplane, the underlay cannot carry the tunnel, or the chosen topology has no path. Work from the API outward. If the Site or GatewayPool status is wrong, stop there. If the status is correct but the dataplane is empty, move to the node agent. If the dataplane is programmed but packets disappear, move to the underlay and firewall.

This table is a quick index. The sections below explain each row.

| Symptom | First check | Usual cause |
| --- | --- | --- |
| Node has no pod CIDRs | Site membership and assignment selection | Node IP outside `nodeCidrs`, assignment disabled, regex mismatch, pool exhausted, or controller not leader. |
| No WireGuard handshakes | `wg show` and the `wg-pubkey` annotation | Firewall on UDP 51820, missing module, missing site label, or key not published. |
| Same-site traffic works, cross-site fails | GatewayPool status and advertised routes | No eligible gateway, no external IP, no assignment, or peering disabled. |
| Remote routes disappear intermittently | Health check metrics and probe connectivity | Overlay path down, probes blocked, or gateway agent restarted. |
| Dashboard shows old data | Leader lease and EndpointSlice | Non-leader endpoint, stale push, or a node that stopped publishing. |
| Controller restarts repeatedly | Controller logs and `pod_cidr_exhaustion_total` | Site pod CIDR pool exhausted. |

### Nodes not getting pod CIDRs

A node receives pod CIDRs only when the controller matches its internal IP to a Site's `spec.nodeCidrs` and then selects an enabled `podCidrAssignments` rule. Start with the node's internal address and labels:

```bash
kubectl get node <node-name> -o wide
kubectl get node <node-name> -o jsonpath='{.status.addresses}{"\n"}'
kubectl get node <node-name> --show-labels
kubectl get node <node-name> -o jsonpath='{.spec.podCIDRs}{"\n"}'
```

Then compare the site's CIDRs and assignments:

```bash
kubectl get site <site-name> -o yaml
kubectl get nodes -L unbounded-cloud.io/site
```

The common causes are:

- The node's `InternalIP` is outside every `spec.nodeCidrs` value for the site.
- Every matching assignment has `assignmentEnabled: false`, or the site has `manageCniPlugin: false` and the assignment inherits it.
- A `nodeRegex` on the selected assignment does not match the node name.
- The winning assignment has no capacity, or two assignments are inconsistent about block size.
- The chosen CIDR pool is exhausted.
- The controller is not the leader, or its allocator caches have not seeded yet.

Assignment selection is deterministic. Lower `priority` values win, and an omitted priority defaults to 100. Ties go to the earlier assignment in the list. Check the effective selection rather than assuming the first assignment in the file is the one in use.

Inspect the allocator state through the controller proxy:

```bash
kubectl unbounded net controller proxy --no-browser &
curl -fsS 'http://127.0.0.1:9999/status/json?debug=allocators' | python3 -m json.tool
```

The response lists each allocator key, its CIDR blocks, and its allocated set. If the controller is crash-looping, read the previous container log:

```bash
kubectl -n unbounded-system logs deployment/unbounded-net-controller --previous --tail=200
```

{{< callout type="warning" >}}
For site-based pod CIDR assignment, IPv4 or IPv6 pool exhaustion calls `klog.Fatalf` in the controller. The process exits, restarts, and exits again until the pool is expanded or nodes stop consuming it. `pod_cidr_exhaustion_total` increments before the exit, but the restart loop may make the controller logs the only durable clue.
{{< /callout >}}

### WireGuard tunnels not establishing

First confirm the interface exists and is configured:

```bash
kubectl unbounded net node exec <node-name> -- ip link show wg51820
kubectl unbounded net node exec <node-name> -- wg show wg51820
kubectl unbounded net node exec <node-name> -- wg show all
```

The node must publish a WireGuard public key for the controller and its peers to use it:

```bash
kubectl get node <node-name> \
  -o jsonpath='{.metadata.annotations.net\.unbounded-cloud\.io/wg-pubkey}{"\n"}'
```

If the annotation is empty, the node agent has not completed key generation or has not been able to patch the Node. Check its logs:

```bash
kubectl unbounded net node logs <node-name> --tail=200
```

If the key exists but there is no handshake, check the underlay. Both sides must be able to reach the advertised endpoint on UDP 51820, or on the allocated gateway port in the node's `net.unbounded-cloud.io/wireguard-port` annotation. A node behind a NAT may need a declared public IP:

```bash
kubectl annotate node <node-name> \
  net.unbounded-cloud.io/declared-public-ip=<public-ip>
```

Also confirm that the node has a site label. A node without site membership has no peers to configure:

```bash
kubectl get node <node-name> -L unbounded-cloud.io/site
```

Finally, check that the kernel module is loaded on both ends. The agent cannot create a WireGuard device if the module is missing, even when the rest of the configuration is correct.

### Cross-site traffic failing

If same-site pod traffic works and cross-site traffic does not, the local dataplane is probably healthy and the path through a gateway is missing. Start with the pool and its assignment:

```bash
kubectl get gp -o wide
kubectl get gp <pool-name> -o yaml
kubectl get sgpa
kubectl get spr
```

Check that the pool has nodes and that each selected node has a usable external address:

```bash
kubectl get gp <pool-name> \
  -o jsonpath='{range .status.nodes[*]}{.name}{" "}{.externalIPs}{"\n"}{end}'
```

The controller considers, in order, the `net.unbounded-cloud.io/declared-public-ip` annotation, the node's `ExternalIP` addresses, and an unexpired `net.unbounded-cloud.io/discovered-public-ip` annotation. An External pool skips a node when none of those resolve. The node also needs the WireGuard public key annotation before it can join any pool.

Then verify the route and the gateway path on a worker:

```bash
kubectl unbounded net node show <worker-node> routes
kubectl unbounded net node exec <worker-node> -- ip route | grep <remote-site-cidr>
kubectl unbounded net node show <worker-node> peers
```

A route to the remote pod CIDR should point at the gateway path, and the peer list should contain the gateway. If routes are missing, check `gateway_pool_reachable_sites_total` and `gateway_pool_routes_total`; if they are present but traffic fails, move to health checks and firewall rules.

Also check for a `localCidrs` entry that accidentally covers the remote range. Local CIDRs are never routed through gateway pools by design.

### Gateway health check failures

Health checks run as UDP probes over the tunnel overlay. The defaults are a 15 second transmit interval, a 15 second receive interval, and a detect multiplier of 3, so a peer is marked down after roughly 45 seconds of missed probes. If you lower the intervals, detection gets faster but the control traffic increases.

You can override the defaults on the scope that owns the link: a Site, SitePeering, SiteGatewayPoolAssignment, GatewayPool, or GatewayPoolPeering. The intervals accept a duration string or an integer interpreted as milliseconds:

```yaml
spec:
  healthCheckSettings:
    enabled: true
    detectMultiplier: 3
    transmitInterval: 300ms
    receiveInterval: 300ms
```

Disabling health checks is supported, but it removes the primary liveness signal for unencrypted tunnels. Use it only when the tunnel protocol or the underlay provides its own failure detection.

Start with peer state and probe counters:

```bash
kubectl unbounded net node exec <gateway-node> -- \
  sh -c "wget -qO- http://127.0.0.1:9998/metrics | grep -E 'healthcheck_(peers|rtt|packets)'"
```

`healthcheck_peers{state="Down"}` identifies the affected peers. A high `healthcheck_packets_timeout_total` with no corresponding receive count usually means the overlay path or the reverse path is broken. A high `healthcheck_session_flaps_total` means the path is unstable rather than completely down.

Probe manually from one node agent to the peer's overlay health IP. The health IP is the first address of the peer's pod CIDR, and it is recorded in the GatewayPool status as `healthEndpoints`:

```bash
kubectl get gp <pool-name> -o jsonpath='{.status.nodes[*].healthEndpoints}'
kubectl unbounded net node exec <local-node> -- \
  unping -c 3 -p 9997 <peer-overlay-ip>
```

If manual probes fail while `wg show` reports a recent handshake, the tunnel is up but the overlay route or firewall is wrong. Check the gateway node agent readiness and its logs:

```bash
kubectl -n unbounded-system get pod \
  -l app.kubernetes.io/name=unbounded-net-node \
  --field-selector spec.nodeName=<gateway-node>

kubectl unbounded net node logs <gateway-node> --tail=300
```

A gateway that loses its site assignment, its pool selection, or its pod CIDRs stops advertising routes. Health checks then remove the paths that depended on it, which looks like a health failure but is really an assignment failure.

### Dashboard shows stale data

The dashboard is a view of the controller's status cache. It can be correct and still lag the cluster. Check leadership and the service endpoint first:

```bash
kubectl -n unbounded-system get lease unbounded-net-controller \
  -o jsonpath='{.spec.holderIdentity}{"\n"}'

kubectl -n unbounded-system get endpointslices \
  -l kubernetes.io/service-name=unbounded-net-controller

kubectl -n unbounded-system get endpoints unbounded-net-controller
```

The controller Service deliberately has no selector. The leader publishes its own EndpointSlice and, on Kubernetes versions that still require it, a legacy `Endpoints` object. Do not add a selector to the Service; doing so can send status requests to a standby pod that returns `503`.

If the endpoint is correct, check whether a specific node is publishing:

```bash
kubectl unbounded net node list --output wide
kubectl unbounded net node show <node-name>
```

A node whose last push is older than the 90 second stale threshold is polled directly by the controller. If both push and pull fail, the dashboard keeps the last known values and marks them as stale. Look at `status_push_total` on that node and at the controller logs for rejected pushes. A controller restart also empties the cache until nodes publish again, which is normal for one or two push intervals.

If a stale `Endpoints` object from an older controller version points at a dead pod IP, delete it and let the leader recreate the correct endpoint:

```bash
kubectl -n unbounded-system delete endpoints unbounded-net-controller
kubectl -n unbounded-system rollout restart deployment/unbounded-net-controller
```

### Diagnostic commands

The following commands cover most investigations without opening a shell on a host.

| Goal | Command |
| --- | --- |
| List sites and node counts | `kubectl get st` |
| Show one site in full | `kubectl get site <site-name> -o yaml` |
| List gateway pools | `kubectl get gp` |
| Show pool membership and reachable sites | `kubectl get gp <pool-name> -o yaml` |
| List site-to-pool assignments | `kubectl get sgpa` |
| List site peerings | `kubectl get spr` |
| List pool peerings | `kubectl get gpp` |
| Show node site and pool labels | `kubectl get nodes -L unbounded-cloud.io/site,net.unbounded-cloud.io/gateway-pool` |
| Show a node's pod CIDRs | `kubectl get node <node-name> -o jsonpath='{.spec.podCIDRs}{"\n"}'` |
| Show controller nodes | `kubectl unbounded net node list --output wide` |
| Show node diagnostics | `kubectl unbounded net node show <node-name> json` |
| Show node peers | `kubectl unbounded net node show <node-name> peers` |
| Show node routes | `kubectl unbounded net node show <node-name> routes` |
| Show BPF entries | `kubectl unbounded net node show <node-name> bpf` |
| Force fresh diagnostics | `kubectl unbounded net node show <node-name> peers --refresh` |
| Follow controller logs | `kubectl unbounded net controller logs --tail=200 -f` |
| Follow one node's agent logs | `kubectl unbounded net node logs <node-name> --tail=200 -f` |
| Show WireGuard state | `kubectl unbounded net node exec <node-name> -- wg show all` |
| Show dataplane routes | `kubectl unbounded net node exec <node-name> -- ip route show dev unbounded0` |
| Show TC attachment | `kubectl unbounded net node exec <node-name> -- tc filter show dev unbounded0 egress` |
| Show CNI configuration | `kubectl unbounded net node exec <node-name> -- cat /host/etc/cni/net.d/10-unbounded.conflist` |
| Show controller lease | `kubectl -n unbounded-system get lease unbounded-net-controller -o yaml` |
| Show controller summary | `kubectl unbounded net controller status-json` |
| Show cluster from aggregated API | `kubectl get --raw /apis/status.net.unbounded-cloud.io/v1alpha1/status/json` |

`node show` loads diagnostics explicitly. It does not watch, and `--watch` is rejected with a message that points at `node list --watch`. Reuse an unexpired snapshot by default, or pass `--refresh` to request fresh collection. The default detail lifetime is 300 seconds.

### Debug logging

Both binaries use klog verbosity. The operator passes `-v=$(LOG_LEVEL)` from the `unbounded-net-config` ConfigMap. The log level in `config.yaml` is watched and applied without a restart, but most other configuration fields are startup-only.

To raise verbosity temporarily, edit the ConfigMap and wait for the file watch to apply:

```bash
kubectl -n unbounded-system edit configmap unbounded-net-config
```

Set `common.logLevel` to `4` for detailed peer, route, and reconciliation messages. Level `2` is the normal operational level, and `3` adds change-level detail. For a one-off investigation, `kubectl unbounded net node logs <node-name> --tail=500` is usually enough; raising the global level on a large cluster produces a lot of output.

## Operational procedures

The procedures in this section assume the controller is healthy and you have cluster-admin access to the cluster-scoped networking resources. Change one layer at a time. If you change a Site and a GatewayPool in the same maintenance window, you cannot tell which change caused a route to disappear.

### Adding a new site

Adding a site is mostly an address-planning exercise. Choose a node CIDR that is unique across every Site and a pod CIDR block that does not overlap any other pod block. The admission webhook rejects overlapping node or pod CIDRs, so a mistake here usually fails at apply time rather than at packet time.

1. Choose the node CIDR and pod CIDR block.
2. Create the Site.
3. Wait for the site condition and the networking workloads.
4. Join worker nodes whose internal IPs fall inside the node CIDR.
5. Add a gateway or peering path if the site must reach other sites.
6. Verify node assignment and cross-site traffic.

```bash
kubectl unbounded net create site site-new \
  --node-cidr 10.3.0.0/16 \
  --pod-cidr-block 10.247.0.0/16 \
  --pod-node-block-size-ipv4 24
```

If the site will use a gateway pool, create the assignment, or add the site to an existing assignment:

```bash
kubectl patch sitegatewaypoolassignment main-assignment --type=merge \
  -p '{"spec":{"sites":["site-east","site-west","site-new"]}}'
```

Wait for the operator and the controller to converge:

```bash
kubectl wait --for=condition=NetReady site/site-new --timeout=10m
kubectl get site site-new -o yaml
kubectl get nodes -L unbounded-cloud.io/site
```

New nodes are matched by their `InternalIP`. If a node does not appear under the site, check that address before you change the Site. Adding a node to the wrong site is not a label edit; the controller owns the label. Correct the node's address or the Site's CIDR, then let the controller reconcile.

If the site needs a dedicated gateway, create the pool with a selector that does not overlap the shared pool, label the node, and then point the site's assignment at the new pool when you are ready to use it:

```bash
kubectl unbounded net create gatewaypool site-new-gw \
  --type External \
  --node-selector gateway-pool=site-new-gw

kubectl label node gateway-new-1 gateway-pool=site-new-gw
kubectl get gp site-new-gw -o yaml
```

Do not create a second External pool whose selector matches an existing gateway node unless you intend to move that node. A node belongs to exactly one pool, and the controller chooses the lexicographically first eligible pool name when several match.

### Removing a site

Removing a site is a two-part operation because Sites are protected by a finalizer. The controller adds `net.unbounded-cloud.io/protection` while any node, including a gateway node, is assigned to the site. Deleting the Site before the nodes are gone leaves it in `Terminating` until the node count reaches zero.

A safe sequence is:

1. Remove the site from every SiteGatewayPoolAssignment that references it, and update or delete any SitePeering that names it. A SitePeering requires at least two sites, so a two-site peering must be deleted rather than reduced to one.
2. Drain and remove its workloads.
3. Remove or delete the nodes that match the site's node CIDRs.
4. Remove gateway labels from nodes that should not remain gateways.
5. Delete the Site.
6. Confirm that its SiteNodeSlices are gone.

```bash
kubectl patch sitegatewaypoolassignment main-assignment --type=merge \
  -p '{"spec":{"sites":["site-east","site-west"]}}'

kubectl drain <node-name> --ignore-daemonsets --delete-emptydir-data
kubectl delete node <node-name>
kubectl delete site site-old
```

SiteNodeSlices are owned by the Site and are garbage collected when the Site is deleted. GatewayPoolNodes are owned by their pool and are cleaned up when the node leaves the pool. Neither should be backed up or edited by hand.

If the Site is stuck in `Terminating`, inspect the finalizer and the observed node count:

```bash
kubectl get site site-old -o jsonpath='{.metadata.finalizers}{"\n"}'
kubectl get site site-old -o jsonpath='{.status.nodeCount}{"\n"}'
kubectl get nodes -L unbounded-cloud.io/site | grep site-old
```

Do not remove the finalizer as a shortcut. It exists to prevent you from deleting the API object while nodes still depend on its address plan. Remove the node assignment instead.

Deleting the last Site does not uninstall networking. The operator deliberately retains the networking workloads so that deleting a Site cannot break pod networking for the rest of the cluster.

### Changing gateway pools and replacing a gateway node

GatewayPool changes affect selection, not the node's configuration directly. The controller compares the pool selector and type against every node, writes `net.unbounded-cloud.io/gateway-pool` on the selected node, allocates a WireGuard port annotation, and publishes the node in `status.nodes`. The node agent then applies the gateway taint and configures gateway peers.

To move a gateway without a gap, add the replacement first:

```bash
kubectl label node gateway-new-2 role=gateway
kubectl get gp main-gateways -o jsonpath='{.status.nodes[*].name}{"\n"}'
kubectl get node gateway-new-2 -o jsonpath='{.metadata.labels.net\.unbounded-cloud\.io/gateway-pool}{"\n"}'
```

Wait until the new node appears in the pool status and its WireGuard port annotation is set. Then remove the old selector label:

```bash
kubectl label node gateway-old-1 role-
kubectl get gp main-gateways -o jsonpath='{.status.nodeCount}{"\n"}'
```

The controller removes the old pool label and port annotation once the node matches no other pool, and the node agent removes the `net.unbounded-cloud.io/gateway-node=true:NoSchedule` taint once the node is no longer a gateway. If the taint remains, the node agent has not reconciled; check its logs rather than removing the taint by hand.

Changing a pool's `type` has a larger blast radius. Switching from Internal to External drops every node without an external address, and switching from External to Internal can change which nodes are eligible. For a production pool, create a new pool, move nodes deliberately, and delete the old pool after its `status.nodeCount` reaches zero.

Adding a routed CIDR advertises an additional prefix through the pool:

```bash
kubectl patch gatewaypool main-gateways --type=merge \
  -p '{"spec":{"routedCidrs":["10.100.0.0/16","10.101.0.0/16"]}}'
```

To delete a pool, first make it select no nodes. A pool with members carries the same protection finalizer as a Site and stays in `Terminating` until it is empty:

```bash
kubectl label node gateway-old-1 role-
kubectl get gp main-gateways -o jsonpath='{.status.nodeCount}{"\n"}'
kubectl delete gatewaypool main-gateways
```

### Changing peering

There are three peering objects, and they answer different questions. A `SitePeering` connects sites directly, with all nodes meshing when `meshNodes` is true (the default). A `SiteGatewayPoolAssignment` connects sites through a gateway pool, which is the usual choice when sites cannot reach each other directly. A `GatewayPoolPeering` connects two gateway pools, which extends reachability between external networks without adding a direct site-to-site path.

Change a direct peering by replacing the site list. A merge patch replaces the whole list, so include every site that should remain:

```bash
kubectl patch sitepeering east-west --type=merge \
  -p '{"spec":{"sites":["site-east","site-west","site-new"]}}'
```

Disable a peering without deleting it when you want to keep the object for a later rollback:

```bash
kubectl patch sitepeering east-west --type=merge \
  -p '{"spec":{"enabled":false}}'
```

For a site-to-pool path, add or remove sites and pools on the assignment:

```bash
kubectl patch sitegatewaypoolassignment main-assignment --type=merge \
  -p '{"spec":{"gatewayPools":["main-gateways","backup-gateways"]}}'
```

For pool-to-pool reachability, use a `GatewayPoolPeering`:

```bash
kubectl unbounded net create gatewaypoolpeering main-to-backup \
  --gateway-pool main-gateways \
  --gateway-pool backup-gateways
```

The webhook checks that referenced sites and pools exist on create and update. It does not clean up references when you delete a Site or pool, so remove references first. Node agents watch all three object types and recompute peers and routes without a restart, but propagation is not instantaneous. Allow at least one status interval and check the status fields:

```bash
kubectl get spr east-west -o jsonpath='{.status}{"\n"}'
kubectl get gp main-gateways -o jsonpath='{.status.connectedSites}{" "}{.status.reachableSites}{"\n"}'
kubectl get gp -o wide
```

If you change peering and routes do not follow, check `gateway_pool_reachable_sites_total` and `gateway_pool_routes_total` on the controller before you change anything else. A peer that is disabled in the API is removed from the node agent's desired state, so routes for that path disappearing after a disable is expected rather than a failure.

### Expanding CIDR pools

Pod CIDR capacity is a function of the pool prefix and the per-node block size. For IPv4, the node block defaults to `/24` when `nodeBlockSizes.ipv4` is omitted. The number of allocatable blocks in one pool is `2^(nodeMask - poolPrefix)`:

| Pool | Node block | Approximate node capacity |
| --- | --- | --- |
| `/16` | `/24` | 256 |
| `/14` | `/24` | 1,024 |
| `/12` | `/24` | 4,096 |
| `/16` | `/26` | 1,024 |
| `/20` | `/24` | 16 |

The webhook enforces an IPv4 node mask between `/16` and `/28`, requires the mask to be larger than the pool prefix, and requires every enabled assignment in a site to use the same IPv4 mask size. IPv6 defaults to the pool prefix plus 16 bits, capped at `/128`, and the mask cannot exceed that bound for any pool in the assignment.

To add capacity, append a block to `spec.podCidrAssignments[].cidrBlocks`:

```bash
kubectl edit site site-east
```

```yaml
# Keep the existing nodeCidrs and other fields; append the new block.
spec:
  podCidrAssignments:
    - cidrBlocks:
        - 10.244.0.0/16
        - 10.248.0.0/16
      nodeBlockSizes:
        ipv4: 24
```

The controller rebuilds that assignment's allocator when the pool-defining fields change, then reseeds it from the CIDRs already held by Nodes and pending reservations. Existing nodes keep their current pod CIDRs. New allocations can use the new block, and the controller will not hand out a block that an existing node already holds.

Check the result without waiting for the next node:

```bash
kubectl unbounded net controller proxy --no-browser &
curl -fsS 'http://127.0.0.1:9999/status/json?debug=allocators' | python3 -m json.tool
```

Two cautions apply. First, expansion does not re-address existing pods. If you also change `nodeBlockSizes`, the new size applies to future allocations, while existing nodes keep their old blocks and are treated as overlapping reservations. Second, expansion does not help if the controller has already exhausted the old pool and exited; restore capacity and let the controller restart, then confirm it becomes leader and allocates for the waiting nodes.

Do not shrink a pool below the blocks already in use. Existing pod CIDRs remain allocated, and removing their pool from the assignment leaves them outside the allocator's containment checks. Add the replacement block first, wait for new nodes to land in it, and only then remove the old block during a maintenance window.

### Rolling upgrades

The operator owns the controller Deployment and node DaemonSet. An operator upgrade should be performed with the install command rather than by editing the controller image directly, because the operator will reconcile the workload back to its own desired image:

```bash
kubectl unbounded install \
  --operator-image unbounded-operator:<tag> \
  --image-registry <registry>/<organization>
```

The controller Deployment uses `maxSurge: 0` and `maxUnavailable: 1` because the controller is host-networked and binds port 9999. A surge pod cannot start while the old pod holds the port on the same node. Expect a short gap in the status API and webhook service during the rollout. The webhook uses `failurePolicy: Ignore`, so CRD edits are not blocked during that gap, but admission validation is temporarily unavailable.

The node DaemonSet ships with `maxUnavailable: 100%`, which allows all node agents to be replaced at once on a small cluster. That is convenient in a lab and risky when every gateway restarts together. If you need a slower rollout, set a lower DaemonSet update strategy through a workload override before you restart, or restart during a window where a brief dataplane reprogramming delay is acceptable.

A configuration-only restart uses the standard rollout commands:

```bash
kubectl -n unbounded-system rollout restart daemonset/unbounded-net-node
kubectl -n unbounded-system rollout status daemonset/unbounded-net-node --timeout=10m

kubectl -n unbounded-system rollout restart deployment/unbounded-net-controller
kubectl -n unbounded-system rollout status deployment/unbounded-net-controller --timeout=10m
```

After the rollout, verify the control plane and a representative path:

```bash
kubectl -n unbounded-system get pods -l app.kubernetes.io/name=unbounded-net-node -o wide
kubectl -n unbounded-system get lease unbounded-net-controller -o jsonpath='{.spec.holderIdentity}{"\n"}'
kubectl unbounded net node list
kubectl unbounded net node show <node-name> peers
```

Only the log level reloads dynamically. Other fields in `unbounded-net-config`, including health ports, status intervals, WireGuard ports, route table IDs, and CNI paths, are read at startup and require a restart. Treat a config edit as a rollout even if the file watcher applies the log-level change immediately.

## Backup and recovery

Back up the declarative objects, not the derived ones. The controller can rebuild node slices, gateway pool nodes, and route advertisements from Sites, pools, peerings, and node labels. It can allocate pod CIDRs for nodes that lack them, but it cannot restore an exact previous allocation.

| Object | Back up | Why |
| --- | --- | --- |
| `Site` | Yes | Holds node CIDRs, pod CIDR pools, CNI management, tunnel protocol, MTU, and health settings. |
| `GatewayPool` | Yes | Holds the selector, pool type, routed CIDRs, and tunnel settings. |
| `SitePeering` | Yes | Holds direct site-to-site topology and health settings. |
| `SiteGatewayPoolAssignment` | Yes | Holds the site-to-pool bindings. |
| `GatewayPoolPeering` | Yes | Holds pool-to-pool reachability. |
| `SiteNodeSlice` | No | Derived by the controller from nodes and Sites. |
| `GatewayPoolNode` | No | Derived by the controller from pool selection. |
| Node gateway labels | Yes | They are the input to pool selection and are not stored in the networking CRDs. |
| `unbounded-net-config` ConfigMap | Yes | Holds ports, intervals, tunnel defaults, and log level. |
| Workload overrides ConfigMap | If used | Holds supported customizations that the operator would otherwise overwrite. |

A single command captures the five declarative networking resources:

```bash
kubectl get sites,gatewaypools,sitepeerings,sitegatewaypoolassignments,gatewaypoolpeerings \
  -o yaml > unbounded-net-backup.yaml
```

Also record the gateway labels and the controller configuration:

```bash
kubectl get nodes -L unbounded-cloud.io/site,net.unbounded-cloud.io/gateway-pool \
  > unbounded-net-node-labels.txt

kubectl -n unbounded-system get configmap unbounded-net-config -o yaml \
  > unbounded-net-config-backup.yaml
```

If you use workload overrides, back up that ConfigMap too. Do not back up the controller Deployment or node DaemonSet as the source of truth. The operator owns them, and restoring an old copy can reintroduce an image or configuration that the current operator will immediately revert.

### Recovery

Recovery has a fixed order because the operator must establish the CRDs before the resources can be restored:

1. Bootstrap the operator.
2. Restore the networking resources.
3. Restore node labels.
4. Restore the configuration ConfigMap if you customized it.
5. Wait for the controller to become leader and for node agents to reconcile.
6. Verify site membership, pool membership, and a cross-site path.

```bash
kubectl unbounded install
kubectl -n unbounded-system rollout status deployment/unbounded-operator

kubectl apply -f unbounded-net-backup.yaml
kubectl label node <gateway-node> role=gateway
kubectl apply -f unbounded-net-config-backup.yaml

kubectl get st
kubectl get gp
kubectl get nodes -L unbounded-cloud.io/site,net.unbounded-cloud.io/gateway-pool
kubectl unbounded net node list
```

The controller recreates SiteNodeSlices and GatewayPoolNodes as it reconciles. Do not restore old copies of those objects; stale slices can briefly advertise obsolete pod CIDRs or WireGuard keys. If you restore into a different cluster, recheck that node CIDRs and pod CIDR blocks do not overlap before you apply the Sites.

## Security considerations

### Network policies

The node agent uses the host network, so many CNI implementations do not enforce pod network policy against its traffic. Where policy is enforced, allow the underlay ports that carry the tunnels and the health/status endpoint:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-unbounded-net
  namespace: unbounded-system
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: unbounded-net-node
  policyTypes:
    - Ingress
    - Egress
  ingress:
    - ports:
        - protocol: UDP
          port: 51820
        - protocol: TCP
          port: 9998
  egress:
    - {}
```

This example allows the default WireGuard port and the node status port. Gateway-to-gateway tunnels use ports allocated from 51821 upward and recorded in each node's `net.unbounded-cloud.io/wireguard-port` annotation. If your gateways sit behind a firewall, allow the full allocated range you observe in the cluster, not only 51820.

Health checks use UDP 9997 on the overlay. In an encrypted tunnel, the underlay packet is still WireGuard on 51820, so a policy that permits the tunnel also permits the probe. In an unencrypted tunnel, verify that your policy does not drop the probe before it reaches the node agent.

### Key rotation

WireGuard private keys are stored on the host at `/etc/wireguard/server.priv` and mounted into the node agent. To rotate a node's key, remove both key files and restart the agent so it generates a new pair and republishes the public key annotation:

```bash
rm -f /etc/wireguard/server.priv /etc/wireguard/server.pub
kubectl -n unbounded-system delete pod <node-agent-pod>
```

The node's public key changes, so peers must learn the new key from the controller. Expect a brief interruption for traffic to and from that node. Rotate one node at a time, and do not rotate all gateways in a pool together. If the annotation does not update, check the node agent's RBAC and logs; the agent cannot publish a key it could not write to the Node.

Unencrypted tunnel protocols, including GENEVE, VXLAN, IPIP, and None, do not use WireGuard keys. Their confidentiality depends on the underlay network. Use WireGuard or another encrypted underlay for links that cross an untrusted network.

### Audit logging

At minimum, audit metadata changes to the networking API groups. The following policy records request metadata without logging full object bodies:

```yaml
apiVersion: audit.k8s.io/v1
kind: Policy
rules:
  - level: Metadata
    resources:
      - group: "net.unbounded-cloud.io"
        resources:
          - gatewaypoolnodes
          - gatewaypoolpeerings
          - gatewaypools
          - sitegatewaypoolassignments
          - sitenodeslices
          - sitepeerings
      - group: "unbounded-cloud.io"
        resources:
          - sites
```

Audit records for `patch` and `update` are especially useful during a topology change because they show who edited a Site, changed a pool, or disabled a peering. Pair the audit policy with RBAC that limits write access to the networking CRDs; a user who can edit a Site can change cluster-wide address assignment.

## Next steps

- Read [Architecture]({{< relref "reference/networking/architecture" >}}) for the controller, node agent, and dataplane components.
- Read [Custom resources]({{< relref "reference/networking/custom-resources" >}}) for every field used in this page.
- Read [Configuration]({{< relref "reference/networking/configuration" >}}) for tunnel protocol selection, MTU, and CNI settings.
- Read [Routing flows]({{< relref "reference/networking/routing-flows" >}}) for the packet paths behind gateway routing and health checks.
- Follow [L3 connectivity]({{< relref "guides/l3-connectivity" >}}) when sites are connected by an existing routed network.
- Use the [CLI reference]({{< relref "reference/cli" >}}) for the full set of `kubectl unbounded net` commands and flags.
