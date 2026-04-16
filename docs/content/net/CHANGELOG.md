<!-- Copyright (c) Microsoft Corporation. Licensed under the MIT License. -->

# Changelog

## Unreleased

## v1.2.2 (2026-04-10)

No changes, just testing release pipeline.

## v1.2.1 (2026-04-10)

### New Features

- **kubectl plugin overhaul**: Native kubectl tab completion via `kubectl_complete-unbounded-net` symlink. Active help hints for CIDR, duration, selector, and enum flag values. Long descriptions and examples on all five create commands. Aliases (st, gp, spr, sgpa, gpp) for create subcommands.
- **External CNI support**: Sites with `manageCniPlugin: false` now skip pod CIDR assignment. Pod CIDR blocks are still required for inter-site routing but per-node allocation is disabled.

### Improvements

- **CI pipeline cleanup**: Removed `push` trigger, GHCR login, image push logic, multi-arch manifest job, and `packages: write` permission from CI workflow -- CI now only validates and warms caches, with all image publishing handled by the release workflow.
- **Release plugin matrix**: Replaced the sequential inline plugin build loop in the release job with a parallel matrix job (matching CI's plugin job), adding proper Go caching per OS/arch and uploading artifacts for the release job to download.
- **CRD validation**: `podCidrAssignments` is now required (minItems=1) on Site resources. `cidrBlocks` within assignments is also required.
- **Plugin flag changes**: Renamed `--manage-cni-plugin` to `--no-manage-cni-plugin` (default is true). Added `--pod-cidr-block` as required flag. Expanded flag descriptions with format hints.
- **Plugin completion**: Hidden global kubeconfig flags from tab completions. Added `<name>` placeholder for positional args. Selectable values for `--type`, `--dry-run`, `--output`, `--tunnel-protocol`.

### Bug Fixes

- **Duplicate error output**: Fixed `kubectl create site` showing the same error twice.
- **Usage line**: `kubectl create site` now shows correct usage when invoked via symlink.
- **Dangling symlinks**: `symlink create` auto-replaces dangling symlinks without `--force`. Added `-f` shorthand.

## v1.2.0 (2026-04-10)

### Bug Fixes

- **kubectl plugin ZSH completion**: Fixed ZSH completion for `kubectl unbounded-net`. The completion script was referencing the old `unbounded cni` command name, causing completions to fail. Also updated the root command `Use` field and help text to use `unbounded-net` consistently.

### Improvements

- **Split CI/CD pipelines**: Split the monolithic `ci.yml` workflow into three separate workflows: `ci.yml` (Build/Test) for PR and push-to-main validation, `prepare-release.yml` (Prepare Release) for creating release branches and PRs via workflow_dispatch, and `release.yml` (Release) which triggers on successful Build/Test runs for release branch merges to create tags, re-tag images, and publish draft GitHub releases.

### Bug Fixes

- **Dashboard auth with group-based RBAC**: HMAC viewer tokens now include the user's groups from the `X-Remote-Group` headers. The dashboard SubjectAccessReview includes these groups, fixing 401 errors for users whose permissions come from group memberships (e.g., AKS `masterclient` via `system:masters`).
- **Dashboard command fails on token error**: The `kubectl unbounded-net dashboard` command now exits with an error if the viewer token cannot be obtained, instead of starting a proxy that returns 401 on every request.

### Improvements

- **Build cache optimization**: Replaced `-ldflags` version injection with `debug.ReadBuildInfo()` and a `/etc/unbounded-net/version` file so the `go build` Docker layer is fully cacheable across releases. Commit hash and build time are now read from Go's embedded VCS metadata; the release tag is written as the final Docker layer. The kubectl plugin retains ldflags for CI cross-compilation with a `debug.ReadBuildInfo()` fallback for local dev builds.
- **Microsoft Go builder image**: Switched Docker builder stage from `azurelinux/base/core:3.0` + `tdnf install golang` to `mcr.microsoft.com/oss/go/microsoft/golang`, ensuring timely Go patch releases and FIPS-compliant crypto. The Go major.minor version is parsed from `go.mod` and passed as a build arg.
- **Microsoft Go in CI**: All `setup-go` steps now use `go-download-base-url: 'https://aka.ms/golang/release/latest'` to install the Microsoft Go distribution.
- **CI Go version from go.mod**: CI workflows now use `go-version-file: go.mod` instead of a hardcoded Go version, keeping CI in sync with the project automatically.
- **Go tool dependencies**: Added `controller-gen` and `govulncheck` as `tool` directives in `go.mod`. CI and Makefile now use `go tool controller-gen` and `go tool govulncheck` instead of `go install`, eliminating separate install/cache steps.
- **CI build_only mode**: Added `build_only` workflow_dispatch input that runs the full build pipeline without pushing images, creating tags, or publishing releases. Useful for validating builds on any branch.
- **Go 1.26.2**: Bumped project Go version from 1.26.0 to 1.26.2.
- **Kubernetes client-go v0.35.0**: Upgraded all `k8s.io/*` dependencies to v0.35.0 and resolved deprecation warnings (typed workqueues, `NewClientset` replacing `NewSimpleClientset`).
- **CI frontend artifact sharing**: Frontend is built once in a dedicated job and shared via artifact to lint, test, and Docker build jobs, eliminating redundant Node.js setup and `npm ci` from 4 jobs.

### New Features

- **HMAC token authentication**: Added HMAC token endpoints (`/token/node` and `/token/viewer`) on the aggregated API server. Node agents and dashboard viewers now authenticate via short-lived HMAC-signed tokens instead of presenting Kubernetes service account tokens directly on push/dashboard endpoints. The HMAC signing key is stored in the serving certificate Secret and preserved across certificate rotations. Token lifetimes are configurable via `--node-token-lifetime` (default 4h) and `--viewer-token-lifetime` (default 30m) flags.
- **RBAC for token endpoints**: Node service account can now `create` `token/node` and viewers can `create` `token/viewer` in the `status.net.unbounded-kube.io` API group.
- **Dashboard authentication with RBAC**: Dashboard endpoints (`/status`, `/status/json`, `/status/ws`) require an HMAC bearer token with the `viewer` role when `requireDashboardAuth` is enabled (the default). Access is controlled by the `unbounded-net-status-viewer` ClusterRole, which aggregates into the built-in `view` ClusterRole.
- **Aggregated API status endpoint**: Status JSON is available via the Kubernetes aggregated API at `/apis/status.net.unbounded-kube.io/v1alpha1/status/json`.
- **`kubectl dashboard` command**: New top-level `kubectl unbounded-net dashboard` command opens the controller status dashboard in a browser via a local authenticated proxy. The existing `controller proxy` command now defaults to not opening the browser (`--no-browser` defaults to true).

### Improvements

- **Removed go-oidc dependency**: The in-process OIDC token verifier has been removed. Token validation at the HMAC token issuance endpoint now uses the Kubernetes TokenReview API exclusively. This removes the `github.com/coreos/go-oidc/v3` dependency and simplifies startup (no OIDC discovery required).
- **Removed `controller port-forward` subcommand**: The standalone port-forward subcommand has been removed in favour of the `controller proxy` and `dashboard` commands, which handle TLS and authentication automatically.
- **Proxy auto-reconnect**: The `kubectl controller proxy` and `dashboard` commands automatically reconnect the port-forward when the underlying connection drops (e.g. pod restart, network timeout).
- **Consolidated CI pipeline**: Merged separate lint, test, and security scan workflows into a single `ci.yml` with parallel stages. Added `workflow_dispatch` trigger for manual releases with version bump selector (patch/minor/major), draft and pre-release options. Releases auto-update the CHANGELOG.
- **Go 1.26 toolchain**: Updated Go from 1.25 to 1.26 across CI and go.mod.
- **Node.js 24 in CI**: Updated setup-node from Node.js 20 to 24.
- **Pre-commit hook**: Added `scripts/pre-commit` for gofmt validation on staged Go files.
- **CI lint checks**: Added gofmt, go mod tidy, and make generate staleness checks to the lint job.

## v1.1.2

### New Features

- **Unified TLS server with self-signed CA**: The controller now runs a single HTTPS server on port 9999 (previously split between plain HTTP on :9999 and self-signed TLS on :9443). A self-signed CA (10-year validity) is generated on first startup, persisted in a Secret along with the server certificate, and the CA public cert is published to a ConfigMap (`unbounded-net-serving-ca`) for node agents. Server certificates are auto-rotated 30 days before expiry; the CA is rotated when within 1 year of expiry. The serving certificate includes the controller service ClusterIP as an IP SAN so hostNetwork node agents can connect by IP address.
- **`kubectl controller proxy` command**: New `kubectl unbounded-net controller proxy` command starts a local HTTP server that proxies to the controller's HTTPS endpoint via port-forward, fetching the CA from the cluster ConfigMap. Enables browser access to the dashboard without TLS errors.

### Security

- **Remove InsecureSkipVerify**: Node agents now verify the controller's TLS certificate using the self-signed CA loaded from a ConfigMap volume mount (plus the cluster CA for API server fallback), resolving the `go/disabled-certificate-check` code scanning alert.
- **Resource creation VAP**: New ValidatingAdmissionPolicy restricts what Secrets, ConfigMaps, and EndpointSlices the controller service account may create, preventing creation of arbitrary resources in the namespace.

### Breaking Changes

- **Webhook service removed**: The `unbounded-net-webhook` Service (port 9443) is eliminated. Webhook and APIService configurations now reference `unbounded-net-controller` on port 9999. Existing deployments will have the old service cleaned up automatically during `make deploy`.
- **HTTPS probes**: Liveness and readiness probes use `scheme: HTTPS`. Requires Kubernetes 1.24+.
- **Node agent HTTPS**: Direct node-to-controller communication switches from `http://` to `https://` and `ws://` to `wss://`. Custom `statusPushURL` or `statusWSURL` configurations may need updating if they used explicit `http://` schemes.

### Bug Fixes

- **eBPF infrastructure for WG-only gateway nodes**: When a node joined a cluster solely via a WireGuard gateway assignment (no same-site mesh peers), the `unbounded0` dummy interface and TC BPF program were never created because `configureEBPFTunnelPeers()` returned early with no tunnel-protocol peers. This left the node with no forwarding path in eBPF mode. The function now always creates the BPF infrastructure regardless of tunnel peer count.
- **Mesh WG interface iptables FORWARD rule**: The iptables FORWARD ACCEPT rule (`ensureTunnelForwardAccept`) was applied to gateway WG interfaces and tunnel interfaces (geneve0, ipip0, vxlan0) but not to the main mesh WG interface (`wg<port>`), potentially causing forwarded traffic to be dropped by KUBE-FORWARD.
- **Link stats warnings not clearing**: Interface error/drop warnings (e.g. `tx_errors` on wg51820) persisted indefinitely because the collection interval was tied to the 10-minute informer resync period, and any counter increment (even +1) refreshed the warning timer. Fixed by using a dedicated 30-second collection interval, a rate threshold of 5 increments per interval, and reducing the warning timeout from 10 to 5 minutes.
- **Status page single-error display**: When a node had exactly one error, the summary fallback showed a generic `"1 error(s)"` count instead of the actual error message. Added a `firstError` field to NodeSummary so the frontend and kubectl plugin can display the message inline.
- **`unroute` multi-nexthop node names**: When a BPF map entry had multiple nexthops (e.g. two WireGuard gateways for the same supernet CIDR), all nexthops were assigned the same node name from the CIDR lookup. Fixed by prioritizing remote (underlay) IP lookup over CIDR lookup.
- **kubectl plugin UN Status column**: The web UI showed "Errors" status when `nodeErrors` existed, but the kubectl plugin showed "Healthy". Added the same `nodeErrors` check to the plugin's status computation.

### Improvements

- **Rename WG Status to UN Status**: Renamed the status column from "WG Status" to "UN Status" in both the web UI and kubectl plugin, and the page title/header from "Unbounded CNI" to "Unbounded Net".
- **kubectl `node show` displays errors**: Running `kubectl unbounded-net node show <name>` now displays active node errors (type and message) below the info pane.

### Chores

- **Remove mirrord config**: Removed tracked `.mirrord/` configuration directory.

## v1.1.1

### Bug Fixes

- **GitHub Actions Node.js 24 compatibility**: Updated checkout, setup-go, cache, and upload-artifact actions to versions compatible with Node.js 24.
- **golangci-lint v2 errors**: Fixed lint errors introduced by golangci-lint v2 and pinned the lint version in CI.
- **Integer overflow code quality alerts**: Resolved GitHub code quality alerts for potential integer overflow in type conversions.

### Improvements

- **Consolidated CI workflow**: Merged separate build and release workflows into a single CI pipeline.
- **Release workflow**: Moved release asset generation (manifests, kubectl plugins, SBOM) to a dedicated `release.yml` triggered by tag push.
- **OCI labels for GHCR**: Added standard OCI labels to Dockerfiles for GitHub Container Registry repository linking.
- **Docker layer caching**: Added Docker layer and CNI plugin caching to CI builds for faster iterations.

### Chores

- **Copilot instructions**: Added PR/release guidelines and lint-before-commit requirement.

## v1.1.0

### New Features

- **BPF ECMP with consistent hashing**: Tunnel endpoints now support up to 4 nexthops per CIDR prefix with HRW (Highest Random Weight) hashing for deterministic per-flow nexthop selection. Only affected flows rehash when a nexthop fails.
- **BPF health-aware forwarding**: Each BPF tunnel nexthop has a TUNNEL_F_HEALTHY flag. Unhealthy nexthops are skipped by the BPF program. Healthcheck probes (UDP 9997) are always forwarded to enable recovery detection.
- **Finalizer-based delete protection**: Sites and GatewayPools use the `net.unbounded-kube.io/protection` finalizer instead of webhook-based delete guards. SiteNodeSlices are protected by ownerReferences.
- **CRD schema validations**: Moved minItems, minProperties, enum, min/max, and maxLength validations from webhook to CRD OpenAPI schema.
- **Per-interface FORWARD ACCEPT rules**: Transit overlay traffic through gateways is accepted via per-interface iptables rules (`-i geneve0 -j ACCEPT`) instead of policy-based routing. Rules are added when interfaces are created and removed when interfaces are deleted.
- **`unroute` HEALTHY column**: The `unroute` CLI now displays per-nexthop health state.

### Deprecations

- **Policy-based routing (PBR)**: `enablePolicyRouting` now defaults to `false`. PBR is replaced by per-interface FORWARD ACCEPT rules for cross-site transit forwarding. The option remains available for backward compatibility but is deprecated.

### Bug Fixes

- **rp_filter on all tunnel datapaths**: Added `disableRPFilter()` calls to kernel GENEVE, kernel VXLAN, and eBPF tunnel paths (was only in eBPF path).
- **rp_filter writes through /proc/1/root**: Container procfs overlay silently discards writes. Node agent now writes through `/proc/1/root/proc/sys/` for real kernel sysctl changes.
- **rp_filter reapplied after interface deletion**: Kernel resets rp_filter on remaining interfaces when an interface is deleted.
- **unbounded0 address sync retry**: Address assignment on unbounded0 is retried on every reconcile instead of only on first TC attachment.
- **BUILD_TIME**: Makefile now uses `date -u` instead of `git log -1` for accurate build timestamps.
- **Concurrent map crash**: Fixed race condition on meshPeerHealthCheckEnabled/gatewayPeerHealthCheckEnabled maps between reconciliation and status server goroutines.
- **IPIP MAC address**: No longer attempts to set MAC on IPIP interfaces (layer 3, not supported).
- **Webhook consolidation**: 6 webhook entries collapsed to 1. failurePolicy changed to Ignore.
- **Init script enables IP forwarding**: `net.ipv4.ip_forward=1` and `net.ipv6.conf.all.forwarding=1` now set in init script.
- **Lazy tunnel interface creation**: Interfaces only created when peers need them, avoiding unnecessary churn.

### Breaking Changes

- **enablePolicyRouting default changed**: Default changed from `true` to `false`. Existing deployments using PBR should set `enablePolicyRouting: true` in their config explicitly if they need the old behavior, but the new per-interface FORWARD ACCEPT approach is recommended.
- **Webhook failurePolicy**: Changed from `Fail` to `Ignore`. The webhook is no longer required to be available for CRD operations. CRD schema validates most constraints.
- **Validating webhook count**: Reduced from 6 to 1. ValidatingAdmissionPolicy webhook-count check will enforce the new count after first update.

## v1.0.1

### Bug Fixes

- **Release manifest apiserverURL**: The `make render` target no longer bakes the
  local kubeconfig API server URL into the controller ConfigMap. Release manifests
  now ship with an empty `apiserverURL`, allowing each deployment to configure it
  independently.
- **ValidatingAdmissionPolicy null-safety**: Added `has()` null guards to all CEL
  expressions in the `unbounded-net-webhook-field-restriction` and
  `unbounded-net-node-field-restriction` ValidatingAdmissionPolicies that access
  `metadata.labels` and `metadata.annotations`. These fields are optional on
  Kubernetes objects and previously caused `no such key` errors when absent,
  blocking the controller from updating webhook caBundle on startup.

## v1.0.0

### Highlights

unbounded-net 1.0.0 is a major release introducing the **eBPF tunnel dataplane**, replacing per-peer netlink interfaces with a single BPF LPM trie architecture. This dramatically simplifies the data plane, reduces kernel resource usage, and enables O(1) per-packet routing decisions regardless of cluster size.

### New Features

- **eBPF tunnel dataplane** (default): A TC egress BPF program (`unbounded_encap`) on a dedicated `unbounded0` NOARP dummy device performs LPM trie lookups to determine the tunnel endpoint, sets the tunnel key, and redirects packets to shared flow-based tunnel interfaces (`geneve0`, `vxlan0`, `ipip0`). Supports GENEVE, VXLAN, IPIP, WireGuard, and direct routing (None). Configurable via `tunnelDataplane: ebpf` (default) or `tunnelDataplane: netlink` (legacy).
- **Dual-stack overlay over configurable underlay**: IPv4 and IPv6 overlay networks can run over either IPv4 or IPv6 underlay, controlled by the `tunnelIPFamily` configuration option (default: IPv4).
- **BPF map status reporting**: BPF LPM trie contents are reported in the node status, visible in the web UI (new BPF tab in node modal), `kubectl unbounded net node show <name> bpf`, and the JSON status API.
- **unroute debug tool**: BPF map inspector included in the node agent container. Dumps all LPM trie entries with node name annotation, performs single-IP LPM lookups, and supports JSON output. Available via `kubectl exec`.
- **unping health check probe tool**: Sends health check probes to a remote node's overlay IP and prints RTTs, useful for verifying overlay connectivity independently of the health check manager.
- **VXLAN source port range**: Configurable `vxlanSrcPortLow`/`vxlanSrcPortHigh` (default 47891-47922) to reduce the number of distinct flows created from VMs, helping avoid flow table limits on cloud platforms.
- **Unused tunnel device cleanup**: Tunnel interfaces (geneve0, vxlan0, ipip0) are automatically removed when no peers use the corresponding protocol.
- **IPIP-on-Azure detection**: The controller warns when nodes with azure:// providerID are configured with IPIP tunnel protocol, which Azure's platform networking blocks.
- **SGPA tunnelProtocol with Site fallback**: SiteGatewayPoolAssignment tunnelProtocol now correctly overrides the Site setting for gateway peers. When set to Auto or unset, falls back to the Site's tunnelProtocol.
- **Comprehensive documentation**: New quick-start guide, routing flows document (eBPF and netlink modes), and troubleshooting guide. Updated architecture, configuration, CRD, and operations documentation.

### Breaking Changes

- **Default tunnel dataplane changed to eBPF**: New deployments use the eBPF dataplane by default. Set `tunnelDataplane: netlink` to use the legacy per-peer interface mode.
- **Routing table changed from dedicated to main**: eBPF mode uses the main routing table (table 0) with supernet routes on `unbounded0` instead of a dedicated routing table (252). Old table 252 routes are cleaned up automatically.
- **rp_filter must be 0**: The eBPF dataplane requires `rp_filter=0` on tunnel interfaces and `net.ipv4.conf.all`. The node agent sets this automatically.
- **Build registry split**: Build artifacts are pushed to `unboundednettmebuild.azurecr.io` (no replicas) and published to `unboundednettme.azurecr.io`. Deployment manifests reference the publish registry.
- **CRD domain**: API group is `net.unbounded-kube.io` (changed from `unbounded.aks.azure.com`).
- **Project rename**: Binary and image names changed from `unbounded-cni-*` to `unbounded-net-*`.

### Improvements

- **Netlink cache**: All netlink operations (link lookups, route listing, address queries) go through a shared cache with configurable resync period, reducing syscall overhead on large clusters.
- **Lock-free status collection**: Narrowed the reconciliation lock scope so `getNodeStatus()` is never blocked by tunnel configuration.
- **Deterministic tunnel interface MACs**: Tunnel interfaces get MACs derived from the local underlay IP (`02:<ip_bytes>:FF`), ensuring inner Ethernet frames are classified as PACKET_HOST on the receiver. Required for kernel 6.11+ which assigns random MACs by default.
- **Scope global routes**: eBPF supernet routes on `unbounded0` use scope global instead of scope link, enabling cross-interface forwarding on gateway nodes for cross-site traffic.
- **Deferred BPF map reconcile**: All protocol paths (GENEVE, VXLAN, WireGuard) accumulate BPF entries before a single reconcile, preventing protocol paths from overwriting each other.
- **Gateway route advertisement**: Gateway nodes advertise their connected sites' pod/node CIDRs via GatewayPoolNode status, enabling cross-site routing through gateway pools.
- **Route annotation for eBPF**: The route expected/present annotation system correctly handles eBPF mode -- unbounded0 routes are marked as expected+present, phantom WG/tunnel nexthops are removed from the display.
- **K8s column in peerings tab**: The node detail modal's peerings tab now shows the Kubernetes readiness status of destination nodes.
- **Multi-arch builds**: Controller, node agent, and kubectl plugin built for linux/amd64 and linux/arm64.

### Bug Fixes

- **SGPA tunnelProtocol composite key**: The assignment pool tunnel protocol map was keyed by pool name alone instead of the composite `siteName|poolName` key used by the lookup, making SGPA tunnelProtocol overrides non-functional.
- **Gateway health check interface name**: In eBPF mode, gateway peer health checks used per-peer interface names (`gn<decimal>`) instead of the shared interface name (`geneve0`), causing health checks to never be marked as enabled.
- **Cross-site supernet routes**: Worker nodes were missing supernet routes for remote sites because `collectSupernets` only saw the protocol-filtered gateway peers, not the full list including WireGuard gateways.
- **WG gateway BPF interface name**: The `addWireGuardPeersToBPFMap` function used an incorrect interface name pattern (`gwwg<port>` instead of `wg<port>`), preventing WG gateway peer CIDRs from being added to the BPF map.
- **IPIP MTU**: The ipip0 interface was not getting its MTU set in eBPF mode, defaulting to 1480 instead of the configured tunnel MTU.
- **Peer deduplication**: Peers that switched from WireGuard to GENEVE/VXLAN could appear twice in the status output.
- **sitePodCIDRPools timing**: The pod CIDR pool list was stored on state after tunnel configuration ran, so the first reconciliation used empty pools and created redundant per-peer /24 routes.
- **Orphaned route cleanup**: The route manager's orphan cleanup now scans `unbounded0` in addition to `wg*` interfaces.

## v0.8.0

### New Features

- **Nebius site support**: New `scripts/add-nebius-site.sh` creates Nebius VPC networking (pools, network, route table, subnet), VMs across four pool types (external gateway, internal gateway, user, GPU), and Kubernetes CRDs (Site, GatewayPool, SiteGatewayPoolAssignment, GatewayPoolPeering). Features parallel creation (allocations, disks, instances via xargs -P4), static IP allocations for VPC route next-hops, gzip-compressed cloud-init userdata (<32KB), verbose mode, and confirmation prompts with pre-deployment resource resolution.
- **Non-Azure cloud-init template**: New `scripts/userdata-bootstrap-nonazure.yaml` for bootstrapping non-Azure nodes without azure.json, ACR credential provider, or Hyper-V netplan. Bootstrap script is gzip+base64 encoded via `encoding: gzip` and `content: !!binary`.
- **Nebius site removal**: New `scripts/remove-nebius-site.sh` for tearing down Nebius sites.
- **cloud-provider-nebius** (separate repo): New Go-based cloud controller manager that watches nodes with `providerID: nebius://`, patches ExternalIP from the Nebius compute API, manages VPC routes for pod CIDRs with exponential backoff retry, removes the cloud-provider uninitialized taint, and includes a DaemonSet node manager that sets `spec.providerID` from Nebius cloud-metadata.

### Breaking Changes

- **Renamed `add-external-site.sh` to `add-azure-site.sh`**: The script now adds `net.unbounded-kube.io/provider=azure` to node labels. Existing Azure external sites are unaffected but new deployments should use the renamed script.
- **cloud-node-manager-unmanaged restricted to Azure nodes**: The unmanaged cloud-node-manager DaemonSet now requires `net.unbounded-kube.io/provider=azure` in addition to `kubernetes.azure.com/cluster DoesNotExist`, preventing it from running on non-Azure external nodes.

### Improvements

- **Route display for tunnelProtocol: None**: The node status UI now shows pod CIDR routes on non-tunnel interfaces (e.g. eth0) by cross-referencing kernel routes against the route manager's managed prefix set. Previously, only routes on tunnel interfaces (wg*, gn*, ip*, vxlan*) were displayed.
- **Docker removal in bootstrap**: `bootstrap-node.sh` now removes conflicting Docker packages (docker-ce, docker-ce-cli, docker-ce-rootless-extras, docker-compose, docker-compose-plugin) and `/etc/docker` before installing containerd.
- **Provider label**: Azure external nodes get `net.unbounded-kube.io/provider=azure`, Nebius nodes get `net.unbounded-kube.io/provider=nebius`.

## v0.7.1

### Bug Fixes

- **Encap selection for mixed pool peerings**: Fixed tunnel protocol selection when an External gateway pool peers with an Internal gateway pool across sites. The External-side node was incorrectly selecting GENEVE instead of WireGuard because only the remote pool type was checked. WireGuard is now correctly selected when either end of a cross-site peering belongs to an External pool.
- **Preferred source IP on dual-stack nodes**: Fixed `SetPreferredSourceIPs` being called in a loop over dual-stack podCIDRs, where the IPv6 iteration cleared the IPv4 preferred source. All tunnel routes now have the correct `src` (podCIDR gateway IP), which also resolves reverse-path filtering drops on GENEVE interfaces without needing to disable rp_filter.
- **GENEVE/IPIP/VXLAN interface addresses**: Tunnel interfaces now get the node's podCIDR gateway IPs assigned (matching WireGuard interfaces). This fixes MASQUERADE for ClusterIP DNAT when traffic exits via tunnel interfaces, restoring controller connectivity from remote-site nodes.
- **SGPA scope for gateway mesh peers**: Gateway nodes now use the SiteGatewayPoolAssignment scope (not the Site scope) when resolving tunnel protocol for same-site mesh peers. This ensures both ends of a gateway-to-node link agree on the tunnel protocol.
- **VXLAN route encap preservation**: Fixed three code paths in the route manager that dropped the `Encap` field from `DesiredRoute`, causing VXLAN routes to be programmed without lightweight tunnel encap metadata. Affected paths: route merge in `SyncRoutes`, `RestoreNexthopForPeer` health-triggered updates, and `routeNeedsUpdate` encap change detection.
- **RouteDistances in tunnel peer status**: GENEVE/VXLAN/IPIP gateway peers now include `RouteDistances` in their status output, fixing expected/present route distance mismatches in the route validation UI.

### Improvements

- **Generalized tunnel route annotations**: Route expected/present annotations and route kind classification now recognize all tunnel interface types (GENEVE `gn*`, VXLAN `vxlan*`, IPIP `ipip*`) in addition to WireGuard (`wg*`). Updated in the controller, kubectl plugin, and web UI frontend.
- **`make render` includes CRDs**: The rendered manifest tarball now includes CRD YAML files in a `crds/` subfolder. The render target depends on `generate` to ensure CRDs are up to date, and cleans stale output before rendering.

## v0.7.0

### Breaking Changes

- **FRR removed**: Static routes are now programmed directly via netlink using kernel multipath routes. BFD has been replaced by a custom UDP health check protocol (similar to SBFD) that runs over WireGuard tunnels.
- **CRD field renames**:
  - `bfdSettings` renamed to `healthCheckSettings` on all CRDs.
  - `wireGuardMTU` renamed to `tunnelMTU` on all CRDs.
  - `encapsulationType` added then renamed to `tunnelProtocol` on all CRDs.
- **Status JSON restructure**:
  - `wireguardPeers` renamed to `peers`.
  - `buildInfo` moved to `nodeInfo.buildInfo`.
  - Top-level `wireguard` moved to `nodeInfo.wireGuard`.
  - Peer `healthCheckEnabled` moved to `healthCheck.enabled`.
  - Peer `allowedIPs` moved to `tunnel.allowedIPs` (formerly `wireGuard.allowedIPs`).
  - Peer `wireGuard` object renamed to `tunnel` with `tunnel.protocol` field.
  - `routingTable.ipv4Routes` and `ipv6Routes` merged to `routingTable.routes[]` with `family` field.
- **Config rename**: `removeIPRoutesOnShutdown` renamed to `cleanupNetlinkOnShutdown`.
- **kubectl plugin rename**: Binary renamed from `kubectl-unbounded` to `kubectl-unbounded-net`. Invoked as `kubectl unbounded cni`.
- **kubectl plugin subcommand changes**: `node show NODE peering[s]` renamed to `node show NODE peer[s]`. `node status-json NODE` replaced by `node show NODE json`.
- **Node annotation rename**: `net.unbounded-kube.io/wg-mtu` renamed to `net.unbounded-kube.io/tunnel-mtu`.

### Features

- **Multi-protocol tunnel support**: New `tunnelProtocol` field on all CRDs supporting WireGuard, GENEVE, IPIP, VXLAN, None, and Auto. Each link type is governed by its specific CRD object (Site for mesh, SitePeering for peered, SGPA for site-to-gateway, GatewayPool for same-pool, GatewayPoolPeering for cross-pool).
  - **GENEVE**: Per-peer GENEVE interfaces (`gn<decimal IP>`) with fixed remote IP. 58 bytes overhead.
  - **IPIP**: Per-peer IPIP interfaces (`ip<decimal IP>`). 20 bytes overhead. Not supported in Azure.
  - **VXLAN**: Single external-mode `vxlan0` interface with per-route `encap ip src/dst` lightweight tunnel directives. 50 bytes overhead.
  - **None**: Direct routing on the default route interface with no tunnel encapsulation. Zero overhead. Requires L3 reachability.
  - **Auto**: Selects based on link characteristics and ConfigMap preferences.
- **Configurable tunnel preferences**: `preferredPrivateNetworkEncapsulation` (default: GENEVE) and `preferredPublicNetworkEncapsulation` (default: WireGuard) in ConfigMap. Warning on startup if public traffic is unencrypted.
- **Security-wins rule**: WireGuard is enforced for public IP links unless explicitly overridden. Any CRD scope setting WireGuard forces WireGuard.
- **Shared route/HC code**: Route building and health check registration shared across all tunnel types via `tunnel_routes.go`. Only interface creation differs per protocol.
- **Protocol column**: Peer tables in both kubectl plugin and web UI show the tunnel protocol per peer.
- **Peer endpoint display**: GENEVE/IPIP/VXLAN peers show their tunnel remote IP in the endpoint field.
- **Conntrack monitoring**: Node agent checks `nf_conntrack_count` vs `nf_conntrack_max` every resync interval, reports warning when usage >= 80%.
- **Watch mode improvements**:
  - WebSocket auto-reconnection with exponential backoff (1s-30s).
  - HTTP polling fallback while WebSocket is disconnected.
  - Connection status indicator: `* Live` (green), `* Polling` (yellow), `* Disconnected` (red).
  - Alternate screen buffer (smcup/rmcup) -- terminal restored on exit.
  - Restructured layout: title + status + leader/time header, then table, then warnings, then instructions.
- New CRD field `tunnelMTU` (renamed from `wireGuardMTU`) on all specs, with per-scope MTU configuration (range 576-9000, minimum-wins).
- New config option `healthCheckPort` (default 9997) -- UDP port for health check probe listener.
- New config option `baseMetric` (default 1) -- base metric for programmed routes.
- Node pod reduced from 3 containers to 1 (removed `frr` and `frr-exporter` sidecars).
- Health check RTT displayed in milliseconds with adaptive precision.
- MTU column added to route table in both the frontend and kubectl plugin.
- ECMP routes display the route-level MTU on the parent row.
- WireGuard mesh interface deferred until peers exist; removed when no WG peers.

### Build

- kubectl plugin builds for linux/darwin/windows x amd64/arm64 (6 binaries).
- kubectl plugin included in main `build` target with SHA-256 build caching.
- ntttcp included in node image for network performance testing.
- Resource limits removed from node daemonset (requests kept).
- Makefile `GO_IN_REPO` fixed (deferred expansion for `$(GO)`).
- Stale FRR configmap references removed from deploy/undeploy targets.

### Bug Fixes

- Fix gateway node mesh peer endpoint port: gateway nodes use their assigned port for non-gateway peers (who listen on that port via dedicated gateway interfaces), and the standard mesh port for other gateway peers.
- Fix health check peer registration: peers are now registered with the health check manager after route sync, enabling health-based route withdrawal.
- Fix health check AddPeer to be idempotent: no-op for unchanged settings, restarts session if settings or overlay IP changes.
- Fix health check route withdrawal: peer ID prefix matching so bare hostnames from health check callbacks match nexthop IDs that include the interface suffix.
- Fix bootstrap route immunity: /32 and /128 host routes marked as HealthCheckImmune so they are never withdrawn when a peer's health check goes down.
- Fix ECMP route merging: routes with the same prefix from different gateway peers are correctly merged into multipath routes instead of the last entry overwriting previous ones.
- Fix ECMP metric filtering: only nexthops at the best (lowest) metric are merged into ECMP routes. Higher-metric indirect paths are excluded, preventing 6-hop routes that should be 2-hop.
- Fix route loop detection in gateway route advertisements: paths where the local gateway pool appears as an intermediate hop are rejected, preventing transitive route loops. Scoped per-pool fallback CIDRs to only sites connected/reachable via that specific pool.
- Fix multipath route status reporting: kernel ECMP routes (Route.MultiPath) are now correctly parsed and displayed with all nexthops.
- Filter proto-kernel address routes from status collection to avoid showing auto-generated interface address routes.
- Fix stale kernel route cleanup: orphaned proto-static routes from previous deployments (e.g., different metric) are removed on sync. Old-metric routes are deleted before replacing when metric changes.
- Fix WebSocket message size limit: increased from default 32 KiB to 256 KiB for both controller node-status and UI broadcast handlers.
- Fix route type classification for pod CIDR supernets: routes like 100.98.0.0/16 (a site's pod CIDR block) routed through gateways now correctly classify as "podCidr site s3" instead of "nodeCidr site s1".
- Fix route destination node display: shows the actual gateway node name resolved from the hop's gateway IP instead of site object names or all peer names.
- Fix health check status annotations on routes: mesh and gateway interface routes now report healthCheckEnabled based on device and gateway presence.
- Fix topology graphs and connectivity matrix showing red/down despite all links being up (case-insensitive health check status comparison).
- Fix warnings list not reopening on content change after being dismissed.
- Fix conflist existence check to ignore the node agent's own conflist file on restart.
- Rename Makefile build-check-arm64 to build-check-platforms (checks all platforms in PLATFORMS variable).

### Removed

- FRR dependency and all FRR-related code, configuration, and container images.
- FRR exporter sidecar and metrics port 9342.
- `frrVtyshPath` config option.
- Controller health check UDP listener (controller does not need to run a probe listener).
- BFD-specific metrics replaced by health check metrics (healthcheck_peers, healthcheck_session_flaps_total, healthcheck_rtt_seconds, healthcheck_packets_sent/received/timeout_total).

## v0.5.3

### Bug Fixes

- Fix incorrect leader election lease settings in the runtime configmap template.
- Ensure gateway APIService resources are deleted by `make undeploy` so namespace cleanup is not blocked.

### Build and Release

- Make `Makefile` targets path-safe from any working directory by anchoring operations to `REPO_ROOT`.
- Update `make render` to also produce `build/unbounded-net-manifests-$(VERSION).tar.gz` for GitHub release attachments.
- Add `VERSION` validation for explicit values (command line or environment), requiring `v`-prefixed semver format (for example, `v0.5.3`).

## v0.5.0

### Security

A comprehensive security review was conducted, resulting in fixes across all
major subsystems. Key hardening includes:

- Fix FRR command injection and batch error handling
- Fix WireGuard key leak and add mutex for key operations
- Fix netlink route_manager IP overflow, error handling, and unbounded map growth
- Fix netlink gateway policy, masquerade, and link manager issues
- Consolidate HTTP servers and fix authentication
- Replace fatal exits with error returns and Kubernetes events
- Fix gateway port allocator race, CRD ownership, and retry limits
- Remove configurable vtyshPath, fix process management and BFD overflow
- Fix bootstrap key handling and path validation
- Fix WireGuard state races, nexthop validation, and resource leaks
- Add CRD validation markers, fix routeplan recursion, add CIDR overlap detection
- Add events RBAC, tighten permissions, add explicit service account automount control
- Fix script variable quoting, temp file permissions, and add warnings

### Features

- Add comprehensive Prometheus metrics to controller and node agent
- Add Prometheus scrape annotations to controller and node manifests
- Implement peering enablement and optimize node status routing updates
- Add Azure Managed Prometheus and Grafana support to primary site deployment
- Add `--infra-only` flag to `create-primary-site.sh`

### Bug Fixes

- Stabilize gateway reconciliation and reduce FRR vtysh chatter
- Fix status metadata, logging consistency, and dashboard sizing behavior
- Fix nil pointer dereference in routeplan-debug tool
- Resolve deployment issues from server consolidation
- Restore status push on both ports, add fullSync ticker
- Include Missing nodes in default K8s status filter
- Expand sites table to fill its card in the dashboard
- Scope assignment BFD profiles per site-pool combination
- Scope BFD profiles per site-pool and add precedence tests
- Gateway nodes use assignment BFD profile over site peering for mesh peers
- Build frr_exporter with CGO_ENABLED=1 and cache mounts
- Run frr-exporter as frr user with frrvty group for socket access
- Add Unwrap() to metrics responseWriter for WebSocket hijack support
- Fix BFD peer metric and add WireGuard per-peer stats
- Use uppercase UDP for AKS allowedHostPorts protocol enum
- Drop loop paths from gateway node route advertisements
- Add TCP MSS clamping on WireGuard interfaces

### Performance

- Use informer cache instead of direct API GETs in site controller

### Refactoring

- Move dashboard WebSocket path from /ws to /status/ws
- Normalize peer route source attribution and polish status UI routing
- Fix stale route cleanup, show missing K8s nodes, consolidate informers

### Chore

- Adjust leader election defaults to reduce renewal frequency
- Fix lint warning in controller CIDR release
