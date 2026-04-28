<!-- Copyright (c) Microsoft Corporation. Licensed under the MIT License. -->

# Security & Bug Review

Full codebase review performed 2026-03-02. Findings ordered by criticality.

## CRITICAL

| # | Area | File | Issue | Comments |
|---|------|------|-------|----------|
| 1 | FRR | `pkg/frr/route_manager.go:724,744,762` | **Command injection via `Route.Gateway`** -- Gateway is used directly in vtysh commands without `net.ParseIP()` validation. An attacker-controlled CRD could inject arbitrary FRR commands. | FIXED. Added validateRoute() with net.ParseIP check. |
| 2 | FRR | `pkg/frr/route_manager.go:727,747,765` | **Command injection via `Route.Interface`** -- Interface name is concatenated into vtysh commands with zero validation. Newlines/semicolons could inject arbitrary commands. | FIXED. Added validateRoute() with strict regex. |
| 3 | FRR | `pkg/frr/route_manager.go:646,733,268` | **Command injection via BFD profile name** -- Profile names are only `TrimSpace()`d before embedding in commands. No character validation. | FIXED. Added validateName() with strict regex. |
| 4 | Node agent | `cmd/.../runtime_utils.go:178,235,412` | **Path injection via `vtyshPath` config** -- `cfg.FRRVTYSHPath` is passed directly to `exec.Command` without validation. An attacker controlling config could execute arbitrary binaries. | FIXED. Removed config field; hardcoded to /usr/bin/vtysh with Dockerfile symlink. |
| 5 | Controller | `pkg/controller/controller.go:281,291` + `site_controller.go:312,1413,1425` | **Fatal exits on CIDR exhaustion/validation** -- `klog.Fatalf()` terminates the entire controller process, causing cluster-wide outage instead of graceful degradation. | FIXED. Replaced with klog.Errorf + error return + Kubernetes events on both Node and Site objects. |
| 6 | Debug tool | `cmd/unbounded-net-routeplan-debug/main.go:177` | **Nil pointer dereference** -- Accesses `peer.BFD.Status` without nil check. Confirmed crash via test. | FIXED. Added nil check for peer.BFD. |

## HIGH

| # | Area | File | Issue | Comments |
|---|------|------|-------|----------|
| 7 | Controller | `cmd/unbounded-net-controller/` | **WebSocket auth bypass** -- Loopback + header-based auth is exploitable via SSRF or compromised pods. Could allow unauthorized status injection. | FIXED. Consolidated servers; removed proxy+loopback auth; implemented front-proxy cert auth. |
| 8 | FRR | `pkg/frr/route_manager.go:955-1015` | **Inconsistent state on partial batch failure** -- Batch vtysh errors are logged at V(3) only; state can diverge between `rm.installed` and actual FRR routing table. | FIXED. Logging at V(2); individual op verification on batch failure. |
| 9 | Webhook | `pkg/webhook/validation.go:41` | **No request body size limit** -- `io.ReadAll(r.Body)` without `MaxBytesReader`. Attacker can OOM the webhook server. | FIXED. Added http.MaxBytesReader with 1MB limit. |
| 10 | FRR | `pkg/frr/route_manager.go:724,744` | **Gateway IP not validated before FRR commands** -- Complements the injection issue; even non-malicious but malformed gateways could corrupt FRR config. | FIXED. Covered by validateRoute() in items 1-3. |
| 11 | Controller | `pkg/controller/controller.go:84-111` | **Race condition in CIDR release** -- Delete handler releases CIDRs outside workqueue without synchronization. | FIXED. Moved CIDR release into workqueue processing path. |
| 12 | Controller | `pkg/controller/gatewaypool_controller.go:315-330` | **Missing mutex on gateway port allocator maps** -- Concurrent goroutines access maps without lock protection. | FIXED. Added sync.Mutex for port allocator maps. |
| 13 | Controller | `pkg/controller/site_controller.go:1276-1286` | **Duplicate podCIDR race** -- Concurrent allocations can produce overlapping CIDRs. | FIXED. Added in-flight CIDR tracking to prevent duplicates. |
| 14 | Netlink | `pkg/netlink/wireguard_manager.go:59` | **WireGuard private key leaked in error message** -- Parse failure wraps the key material into the error string, which gets logged. | FIXED. Error message no longer includes key material. |
| 15 | Netlink | `pkg/netlink/wireguard_manager.go:23-26` | **No mutex on WireGuard manager** -- Concurrent `Configure()` calls can apply conflicting peer states. | FIXED. Added sync.Mutex to WireGuardManager. |
| 16 | Netlink | `pkg/netlink/route_manager.go:371-374` | **Gateway IP byte increment overflow** -- Last-byte increment for ECMP gateway IPs overflows at 0xff without carry propagation. Corrupts IPv6 routes. | FIXED. Added incrementIP() with proper carry propagation. |
| 17 | Deploy | `deploy/node/03-daemonset.yaml.tmpl:106,158,47` | **Privileged containers** -- Node agent, FRR sidecar, and init container all run `privileged: true` instead of minimal capabilities (`NET_ADMIN`, `NET_RAW`). | PEND. Will be tested later. |
| 18 | Deploy | `deploy/node/03-daemonset.yaml.tmpl:27` | **hostPID namespace** -- Exposes all host processes to containers, leaking env vars and credentials. | PEND. Will be tested later. |
| 19 | Scripts | `scripts/bootstrap-node.sh` + `Makefile:238-239` | **No checksum verification on downloaded binaries** -- FRR, CNI plugins, kubelet, containerd all downloaded without hash verification. Supply chain risk. | FIXED. Added warning comment; script is for testing only. |
| 20 | Scripts | `scripts/start-frr.sh:3-4` | **Unvalidated sourced script** -- Sources `/frr/sbin/frrcommon.sh` and executes `$(daemon_list)` without validation; unquoted expansion. | FIXED. Quoted variable expansion. |
| 21 | Node agent | `cmd/.../reconciliation_helpers.go:681` | **`pkill -f` matches process command lines broadly** -- Could kill unintended processes matching the pattern. | FIXED. Replaced with exact process name match. |

## MEDIUM

| # | Area | File | Issue | Comments |
|---|------|------|-------|----------|
| 22 | Node agent | `cmd/.../site_watch_reconcile.go:628` + `wireguard_config.go:20-30` | **Race condition in WireGuard state** -- `configureWireGuard` modifies state fields without holding `state.mu`. | FIXED. Caller (updateWireGuardFromSlices) holds state.mu for the entire configureWireGuard call; configureWireGuard documents this requirement instead of locking internally (sync.Mutex is not reentrant). |
| 23 | Node agent | `cmd/.../bootstrap_helpers.go:184-185` | **Path traversal in WireGuard key paths** -- `cfg.WireGuardDir` used without validation; could write keys to unexpected locations. | FIXED. Added path validation (absolute, no traversal). |
| 24 | Node agent | `cmd/.../bootstrap_helpers.go:239` | **JSON injection in node annotation patch** -- Uses `fmt.Sprintf` instead of `json.Marshal` for JSON construction. | FIXED. Replaced with json.Marshal. |
| 25 | Node agent | `cmd/.../wireguard_config.go:434-443` | **Inconsistent nil-check on gateway nexthop** -- Empty nexthop creates invalid BFD-enabled routes. | FIXED. Added empty nexthop validation with warning log. |
| 26 | Controller | `cmd/unbounded-net-controller/` | **Unbounded goroutines for status pulling** -- Large clusters spawn thousands of goroutines without limits. | FIXED. Added semaphore limiting concurrent WebSocket connections (500 max). |
| 27 | Controller | `cmd/unbounded-net-controller/` | **Missing HTTP server timeouts** -- Slowloris vulnerability on health/webhook endpoints. | FIXED. Added ReadHeaderTimeout (10s), ReadTimeout (30s), WriteTimeout (60s), IdleTimeout (120s) to both TLS and HTTP servers. These do not affect upgraded WebSocket connections. |
| 28 | Controller | `cmd/unbounded-net-controller/` | **Token cache DoS** -- Poor eviction policy allows cache exhaustion. | FIXED. Added LRU eviction with TTL expiry and max cache size. |
| 29 | Controller | `pkg/controller/site_controller.go:549-584` | **Allocator seeding marks all CIDRs in all pools** -- Causes incorrect allocation state. | FIXED. Scoped seeding to relevant pool only. |
| 30 | Controller | `pkg/controller/site_controller.go:1146-1155` | **Unbounded map growth** -- Internal tracking maps grow without limit, causing memory leaks. | FIXED. Added max size limit for tracking maps. |
| 31 | Controller | `pkg/controller/site_controller.go:471-484` | **Invalid regex patterns silently ignored** -- Bad patterns in CRDs don't generate errors. | FIXED. Now returns error on invalid regex, causing requeue. |
| 32 | Controller | `pkg/controller/crd.go:56-104` | **CRD spec replacement without ownership checks** -- Could overwrite CRDs managed by other controllers. | FIXED. Added ownership label check before CRD spec replacement. |
| 33 | Netlink | `pkg/netlink/route_manager.go:112` | **Stale link index after interface recreation** -- Route ops use outdated link index if interface is deleted/recreated. | FIXED. Added link index validation and warning on change. |
| 34 | Netlink | `pkg/netlink/route_manager.go:204,521` | **Fragile error string comparison** -- `err.Error() != "no such process"` breaks if netlink library changes message. | FIXED. Replaced with errors.Is(err, syscall.ESRCH). |
| 35 | Netlink | `pkg/netlink/gateway_policy_manager.go:366` | **Interface name injection into rt_tables** -- Writes to `/etc/iproute2/rt_tables` without sanitizing interface names. | FIXED. Added strict regex validation for interface names. |
| 36 | Netlink | `pkg/netlink/gateway_policy_manager.go:316,360-368` | **Non-atomic writes to rt_tables** -- Crash during write leaves system config inconsistent. | FIXED. Using atomic write (temp file + rename). |
| 37 | Netlink | `pkg/netlink/route_manager.go:20,68` | **Unbounded installedRoutes map** -- No reconciliation against kernel; orphaned entries leak memory. | FIXED. Added ReconcileInstalledRoutes to prune stale entries. |
| 38 | Netlink | `pkg/netlink/masquerade_manager.go:188-218` | **iptables rule accumulation** -- Parse failures prevent stale rule removal; rules accumulate over time. | FIXED. Added chain flush-and-rebuild on persistent parse failures. |
| 39 | Webhook | `pkg/webhook/server.go:175-179` | **No TLS minimum version** -- Allows TLS 1.0/1.1 by default. | FIXED. Set MinVersion: tls.VersionTLS12. |
| 40 | Webhook | `pkg/webhook/server.go:447-451` | **Certificate rotation race** -- Dual storage (`s.cert` + `s.certValue`) could become inconsistent. | FIXED. Removed dual storage; using only atomic.Value. |
| 41 | APIs | `pkg/apis/.../types.go:324` | **Unbounded GatewayNodeStatus.Routes map** -- No `MaxProperties` kubebuilder validation; memory exhaustion risk. | FIXED. Added MaxProperties=1000 and MaxItems on Paths. |
| 42 | APIs | `pkg/apis/.../types.go:94-98` | **Missing NodeBlockSizes validation** -- No min/max on IPv4/IPv6 mask sizes; negative or >128 values crash allocator. | FIXED. Added Minimum/Maximum validation markers. |
| 43 | Routeplan | `pkg/routeplan/routeplan.go:663` | **Recursive call without depth limit** -- Large peer lists could cause stack overflow. | FIXED. Refactored to use iteration instead of recursion. |
| 44 | Deploy | `deploy/controller/03-deployment.yaml.tmpl:23` + `deploy/node/03-daemonset.yaml.tmpl:26` | **hostNetwork on both controller and node** -- Bypasses network isolation; exposes endpoints on host network. | WON'T FIX. Required since this IS the CNI plugin; pods wouldn't get IPs without hostNetwork. |
| 45 | Deploy | `deploy/controller/04-service.yaml.tmpl:14` | **LoadBalancer exposes controller externally** -- Health endpoint leaks cluster state information. | FIXED. Manually removed LoadBalancer default. |
| 46 | Deploy | `deploy/controller/02-rbac.yaml.tmpl` | **Over-broad RBAC** -- Controller can patch all nodes, create arbitrary secrets, list all pods cluster-wide, CRUD all leases. Node agent can patch any node (not scoped to own). | FIXED. Moved leases to namespaced Role; added inline documentation on remaining broad permissions and rationale. |
| 47 | Deploy | All templates | **No image digest enforcement** -- Template variables allow tag-based images; supply chain risk. | WON'T FIX. Testing-only templates. |
| 48 | Scripts | `scripts/add-azure-site.sh:337` + others | **Passwords via CLI arguments** -- Visible in `ps` output and logs. | WON'T FIX. Scripts are for testing only. |
| 49 | Scripts | `scripts/add-azure-site.sh:567-573` | **Sensitive data in predictable temp files** -- Bootstrap tokens/passwords written to `/tmp/` with default umask. | FIXED. Added umask 077 before temp file creation. |
| 50 | FRR | `pkg/frr/route_manager.go:564-570` | **Map modification during iteration** -- `RemoveAllRoutes` deletes from map while iterating; undefined Go behavior. | FIXED. Collect keys first, then iterate and delete. |
| 51 | FRR | `pkg/frr/route_manager.go:1018-1025` | **No timeout on vtysh operations** -- Deadlocked FRR hangs the entire RouteManager while holding mutex. | FIXED. Added 30s default timeout when context has no deadline. |
| 52 | FRR | `pkg/frr/route_manager.go:667-684` | **Silent BFD profile removal failures** -- Failed removals accumulate in `appliedBFDProfiles` map forever. | FIXED. Proper failure tracking and logging at V(2). |

## LOW

| # | Area | File | Issue | Comments |
|---|------|------|-------|----------|
| 53 | Node agent | `bootstrap_helpers.go:206-208` | WireGuard key clamping has no post-validation | FIXED. Added all-zero key check after clamping. |
| 54 | Node agent | `bootstrap_helpers.go:188-195` | TOCTOU race on key file check | FIXED. Replaced with atomic open-or-create pattern. |
| 55 | Node agent | `reconciliation_helpers.go:198-234` | BFD interval int overflow on 32-bit | FIXED. Added bounds clamping to [1, 60000] ms. |
| 56 | Node agent | `wireguard_config.go:656-686` | Missing context cancellation checks between FRR ops | FIXED. Added ctx.Err() checks between operations. |
| 57 | Node agent | `site_watch_reconcile.go:562-580` | Event handler registration resource leak on error | FIXED. Added deferred cleanup for partial registration failures. |
| 58 | Controller | `peering_aggregation_controller.go:188-203` | No max retry limit; potential infinite loop | FIXED. Added max retry limit (10) with exponential backoff. |
| 59 | Netlink | `masquerade_manager.go:249-256` | Fragile iptables rule string comparison | FIXED. Added whitespace normalization for robust comparison. |
| 60 | Netlink | `link_manager.go:133-135` | Link-local addresses skipped in cleanup | FIXED. Added removeAll parameter for full cleanup support. |
| 61 | Netlink | `gateway_policy_manager.go:61-69` | Integer underflow possible in mark calculation | FIXED. Added explicit bounds check before subtraction. |
| 62 | Netlink | `route_manager.go:785-795` | ECMP `intSlicesEqual` is order-dependent | FIXED. Now sorts copies before comparison. |
| 63 | Webhook | `validation.go:380-382` | No ReDoS protection on user-provided regex | FIXED. Added 1024-char pattern length limit. |
| 64 | APIs | `types.go` multiple fields | No CIDR overlap detection across fields | FIXED. Added overlap detection in webhook validation. |
| 65 | Routeplan | `routeplan.go:1118-1123` | Route distance arithmetic lacks bounds checking | FIXED. Added upper bound cap at 20 hops. |
| 66 | FRR | `route_manager.go:647-649` | BFD parameters lack range validation | FIXED. Added range validation in sanitizeBFDProfile. |
| 67 | Deploy | All | Missing NetworkPolicy resources | WON'T FIX. NetworkPolicy not supported on hostNetwork pods (required for CNI plugin). |
| 68 | Deploy | `01-serviceaccount.yaml.tmpl` | No explicit `automountServiceAccountToken` | FIXED. Added explicit automountServiceAccountToken: true. |
| 69 | Scripts | `generate-customdata.sh:40-41` | Unquoted variable expansion | FIXED. Quoted variable references. |

## Future: ValidatingAdmissionPolicy (CEL) Migration

The following improvements can be implemented using Kubernetes ValidatingAdmissionPolicy
(GA in K8s 1.30+) with CEL expressions, replacing or augmenting the existing webhook:

### Node Agent Self-Scoping (RBAC item 46 follow-up)
Node agents currently have cluster-wide node patch permissions. A VAP can enforce that
each node agent can only patch its own node using bound service account token identity:
```cel
// Match: node patch requests from unbounded-net-node SA
request.userInfo.extra["authentication.kubernetes.io/node-name"][0] == object.metadata.name
```

### Object Creation Restrictions (RBAC items 32, 46 follow-up)
Restrict the controller to only creating CRDs, secrets, webhooks, and API services
with expected names (closing the RBAC gap where `create` cannot use `resourceNames`):
- CRDs: `object.metadata.name.endsWith(".net.unbounded-cloud.io")`
- Secrets: `object.metadata.name == "unbounded-net-serving-cert"`
- ValidatingWebhookConfigurations: `object.metadata.name == "unbounded-net-validating-webhook"`
- APIServices: `object.metadata.name == "v1alpha1.status.net.unbounded-cloud.io"`
- EndpointSlices: `object.metadata.name == "unbounded-net-controller"`
- GatewayPoolNodes: node agent can only create objects matching its own node name

### Field-Level Validation Migration
The following webhook validations can be expressed as CEL rules on the CRDs,
removing the need for the webhook to handle them:
- GatewayPool: `spec.type` enum, `spec.nodeSelector` min size, CIDR format, BFD ranges
- SitePeering: `spec.sites` min items, duplicate check, BFD ranges
- SiteGatewayPoolAssignment: `spec.sites`/`spec.gatewayPools` min items, duplicates, BFD
- GatewayPoolPeering: `spec.gatewayPools` min items, duplicates, BFD
- Site: `spec.nodeCidrs` min length, CIDR format, regex length limit, mask size ranges
- Intra-site CIDR overlap (NonMasqueradeCIDRs/LocalCIDRs within single object)

### Must Stay in Webhook (requires API server lookups)
- Site/GatewayPool/SiteNodeSlice delete protection (counting active nodes)
- Referential integrity (SitePeering references existing Sites, etc.)
- Cross-site CIDR overlap detection
- Pod CIDR allocator validation
