# Gantry deployment artifacts

This directory carries the Helm chart and operator-facing rendered manifests
needed to roll out the Gantry agent as a Kubernetes DaemonSet.

## Files

The Helm chart under `chart/` is the source of truth for resources shared by
standalone and operator-managed installations. `make gantry-manifests` renders
the internal operator profile into `deploy/gantry/rendered/`; the Unbounded
operator embeds those files and applies its own image, ConfigMap, and Lease
ownership semantics. The target also renders the standalone node configurator
and examples used by development and benchmark tooling.

| Source | Rendered to | Purpose |
| --- | --- | --- |
| `chart/templates/daemonset.yaml` | `rendered/daemonset.yaml` | One-pod-per-node DaemonSet. |
| `chart/templates/serviceaccount.yaml` | `rendered/serviceaccount.yaml` | Namespace + ServiceAccount + Role + PriorityClass. |
| `chart/templates/configmap.yaml` | `rendered/configmap.yaml` | Default `config.yaml` (mirrors `config.NewDefault()`). |
| `chart/templates/rendezvous-leases.yaml` | `rendered/rendezvous-leases.yaml` | Fixed chair Lease set. |
| `chart/templates/node-config.yaml` | Standalone chart only | Continuously reconciles containerd's default Gantry mirror route. |
| `examples/registry-secret.example.yaml.tmpl` | `rendered/examples/registry-secret.example.yaml` | Template Secret for upstream-registry credentials. |
| `examples/networkpolicy.yaml.tmpl` | `rendered/examples/networkpolicy.yaml` | **Hardening overlay (NOT applied by default).** See [Hardening overlays](#hardening-overlays) below. |
| `hosts.toml.template` | (not rendered) | containerd registry mirror config; one file per upstream registry under `/etc/containerd/certs.d/<host>/hosts.toml`. |

## Installation paths

- Operator-managed clusters use the manifests embedded in the
   `unbounded-operator` binary. The operator never runs Helm.
- Clusters without the operator install the released OCI chart. The chart
   continuously reconciles `/etc/containerd/certs.d/_default/hosts.toml` on
   every selected node. Containerd must already be configured to read
   `/etc/containerd/certs.d`; the chart does not edit or restart containerd.

The paths are mutually exclusive. `PriorityClass/gantry-low` records the active
manager, and both installers reject ownership by the other path.

```sh
helm upgrade --install gantry oci://ghcr.io/azure/charts/gantry \
   --version <release-without-v> \
   --namespace gantry-system \
   --create-namespace \
   --set image.digest=sha256:<gantry-image-digest> \
   --set gantry.upstreamRegistries[0].name=registry.example.com \
   --set gantry.upstreamRegistries[0].endpoint=https://registry.example.com
```

The container image is built from `images/gantry/Containerfile` via
`make image-gantry-local` (or `make image-gantry-push` to push).

## Operator Profile

```sh
# Render the profile embedded by unbounded-operator.
make gantry-manifests

# Validate and package the standalone chart.
make gantry-chart-lint
make gantry-chart-package GANTRY_CHART_VERSION=0.1.0 GANTRY_CHART_APP_VERSION=v0.1.0
```

## Building the image locally

```sh
# Single-arch into local container engine:
make image-gantry-local

# Build and push to $(CONTAINER_REGISTRY):
make image-gantry-push CONTAINER_REGISTRY=ghcr.io/your-org

# Explicit tag:
make image-gantry-local VERSION=v0.6.0
```

## Per-node containerd setup

Nodes provisioned by `unbounded-agent` already have containerd configured to
read `/etc/containerd/certs.d` and carry the managed default Gantry mirror
entry in `/etc/containerd/certs.d/_default/hosts.toml`. On those nodes, install
the Gantry DaemonSet normally; the mirror activates when the pod starts
listening on `127.0.0.1:5000`.

Standalone Helm installations run `DaemonSet/gantry-containerd-config`. Its
resident reconciler checks the default `hosts.toml` every five seconds and
atomically restores the chart-owned payload when the file is missing or
different, including after a node upgrade resets host configuration. Graceful
shutdown removes the file only when it still matches the chart payload. Set
`nodeConfig.enabled=false` when another node-management system owns this file.

Externally managed installations can instead drop a registry-specific
`hosts.toml` at:

```
/etc/containerd/certs.d/<registry-host>/hosts.toml
```

derived from `hosts.toml.template` (substitute `${REGISTRY_SERVER}`
with the registry's `https://...` URL). containerd reloads `certs.d`
on its own; no restart needed.

## ACR Artifact Streaming

This integration configures an existing AKS Artifact Streaming node pool. It
does not install OverlayBD or register its snapshotter with containerd. Enable
it only on nodes where AKS already provides
`/opt/acr/tools/overlaybd/config.sh`, `overlaybd-tcmu`, and
`overlaybd-snapshotter`.

For a standalone Helm installation, enable the Gantry range endpoint and its
host configurator together, with an explicit selector for only the streaming
node pool:

```yaml
gantry:
   artifactStreaming:
      enabled: true

overlaybdConfig:
   enabled: true
   nodeSelector:
      kubernetes.azure.com/agentpool: <streaming-node-pool>
```

The configurator waits for `/artifact-streaming/readyz`, snapshots the current
host configuration under `/var/lib/gantry/overlaybd-config`, then uses the
AKS-provided configuration tool to set OverlayBD's P2P address to
`http://localhost:5000/blobs`. It restarts the OverlayBD services only when the
effective configuration changes. A second writer is never overwritten: if the
host file differs from Gantry's managed snapshot, apply or rollback preserves
that file and reports the conflict.

For an operator-managed installation, opt in through every participating
`Site` using the same selector:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Site
metadata:
   name: <site-name>
spec:
   components:
      gantry:
         enabled: true
         artifactStreaming:
            enabled: true
            nodeSelector:
               kubernetes.azure.com/agentpool: <streaming-node-pool>
```

The operator rejects an empty selector, a Site that enables Artifact Streaming
while disabling Gantry, or conflicting selectors across Sites.

### Artifact Streaming rollback

Drain workloads using active OverlayBD devices before disabling the operator
setting. The operator issues deletion of
`DaemonSet/gantry-overlaybd-config` before applying the non-streaming Gantry
DaemonSet, and the configurator's `preStop` restores the original host
configuration when it still owns the current value. Kubernetes deletion and
pod termination are asynchronous, which is why workloads must be drained
before this transition.

For standalone Helm, keep the Gantry endpoint available during restoration:

```sh
helm upgrade gantry oci://ghcr.io/azure/charts/gantry \
   --reuse-values \
   --set overlaybdConfig.enabled=false \
   --set gantry.artifactStreaming.enabled=true \
   --wait

kubectl -n gantry-system wait --for=delete \
   daemonset/gantry-overlaybd-config --timeout=5m
```

After the configurator is gone and streaming workloads are drained, disable
`gantry.artifactStreaming.enabled`. If the host OverlayBD file was changed by
another owner after Gantry configured it, rollback deliberately leaves that
new value in place; inspect the configurator logs and resolve ownership before
removing the saved state.

## What to verify after rollout

| Check | How |
| --- | --- |
| Agents are running | `kubectl -n unbounded-system get ds gantry` |
| Liveness / readiness | `/livez`, `/readyz` on 9095 per pod |
| Metrics | `curl http://<pod-ip>:9095/metrics` or scrape from Prometheus |
| Routing-table grew | `p2p_dht_health_score` ≥ 0.7 |
| Mirror is being used | `p2p_cache_hit_total` increments while a workload rolls out (= containerd content-store hits on the mirror endpoint after Phase 8) |
| Storage backend is containerd | `gantry_storage_mode_info{mode="containerd"} == 1` |
| Advertiser reconciling | `gantry_advertise_reconcile_total` increases at the configured cadence |
| Leases are being created on `please_pull` | `gantry_containerd_lease_created_total` increments during cold-start rollouts |
| Origin fallback is rare | `p2p_origin_fallback_total` stays at ~0 |
| Streaming endpoint is ready | `/artifact-streaming/readyz` returns 200 on port 5000 for enabled target nodes |
| Streaming source transition | `gantry_streaming_requests_total{source="origin",outcome="success"}` serves cold ranges, then `source="peer"` increases after complete providers advertise |

See `docs/detailed-design.md` §7.6 for the full metric catalog.

## Hardening overlays

`deploy/examples/` carries optional hardening manifests that are
intentionally **not** part of the default `kubectl apply` workflow.
Every overlay there contains at least one site-specific value (CIDR,
endpoint, label) that no shipped manifest can guess correctly across
arbitrary clusters, so applying them unedited will fail the cluster
into a state that is hard to debug.

> **Production guidance:** the default install leaves the mirror
> listener (5000) and transfer listener (5001) reachable from other
> pods on the cluster network at `<podIP>:port`. The `hostIP:
> 127.0.0.1` binding on the DaemonSet's hostPort only restricts
> *host-side* reach; the listener inside the pod is still
> `0.0.0.0`. Production installs **should** adopt
> [`examples/networkpolicy.yaml.tmpl`](examples/networkpolicy.yaml.tmpl) (or
> an equivalent NetworkPolicy in their own overlay) to close that
> pod-network gap. The overlay is shipped as an example rather than
> a default because its allow-list depends on the cluster's
> node-CIDR range, which is site-specific (see the workflow below).

### `examples/networkpolicy.yaml`

Locks transfer (5001), libp2p (4001), mirror (5000), and metrics
(9095) to the minimum traffic each port needs. Holds the manifest
shape required by §7.5 but **defers four CIDR choices to the
operator** - apiserver endpoint, kubelet probe source, mirror DNAT
source, registry egress. See the long "OPERATOR ACTION REQUIRED"
block at the top of the file and the [Production caveats](#production-caveats)
table below.

Workflow:

1. Roll out the DaemonSet without the overlay and verify
   `kubectl -n unbounded-system rollout status ds/gantry`,
   `p2p_cache_hit_total`, and a successful workload pull.
2. Copy the overlay into your own repository (or a Kustomize /
   Helm chart), edit every ipBlock marked "OPERATOR ACTION
   REQUIRED", and review against your CNI's hostPort SNAT
   behavior (`kubectl get nodes -o yaml | grep -A2 podCIDR`,
   etc.).
3. Apply with `kubectl apply -f your-overlay/networkpolicy.yaml`.
   Watch `/readyz` and any in-flight mirror pulls for at least one
   full image pull cycle - a wrong CIDR will surface as
   `dht routing table empty` (no peer libp2p traffic) or as
   containerd `connection refused` on 5000 (wrong mirror source
   CIDR), not as a NetworkPolicy validation error.
4. Roll back with `kubectl delete networkpolicy -n unbounded-system
   gantry-agent` if anything regresses.

Future hardening overlays (Pod Security Standards, dedicated
PriorityClass, alternative `hostNetwork: true` topology) will live
in the same directory and follow the same "deferred to operator,
not in default install" rule.

## Production caveats

A few configuration knobs that need operator attention before going
to production:

| Item | Where | What to change |
| --- | --- | --- |
| API server egress CIDR | `examples/networkpolicy.yaml` | The egress to TCP/443 and TCP/6443 defaults to `0.0.0.0/0` because managed control planes (EKS / GKE / AKS) and self-hosted clusters reach the apiserver at IPs that don't match a `namespaceSelector`. Replace with the apiserver's actual CIDR - `kubectl get endpoints kubernetes -n default -o jsonpath='{.subsets[*].addresses[*].ip}'` for self-hosted clusters; the managed-service docs for hosted control planes. |
| Origin registry egress | `examples/networkpolicy.yaml` | The egress to TCP/443 for origin pulls also defaults to `0.0.0.0/0`. If the cluster only pulls from a known set of registry endpoints (your private registry, ghcr.io, etc.), restrict this rule to those IPs or labels. |
| Kubelet probe source | `examples/networkpolicy.yaml` | Metrics ingress on TCP/9095 currently allows `0.0.0.0/0` so kubelet liveness/readiness probes (sourced from the node IP) reach the pod on strict CNIs. Replace with the node CIDR - `kubectl get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="InternalIP")].address}'`. |
| Mirror port 5000 source | `examples/networkpolicy.yaml` | Ingress on TCP/5000 defaults to a deliberately-narrow `127.0.0.1/32` placeholder. Most CNIs (Calico, Cilium, and managed offerings) SNAT hostPort traffic so the in-pod source-IP after DNAT is the node IP, NOT 127.0.0.1 - the placeholder will then drop containerd's mirror pulls. Replace with the node CIDR (same command as the kubelet probe row). MUST NOT widen to the pod-network CIDR: that bypasses the `hostIP: 127.0.0.1` binding's loopback-only intent. |
| containerd socket access | `daemonset.yaml` | The pod mounts `/run/containerd`, rather than the socket file, so reconnects observe the replacement socket after containerd restarts. It runs with non-root UID 65532 and primary GID 0 because many nodes expose `containerd.sock` as `root:root` mode 0660. Validate this on your target node pool before production. If your runtime uses a dedicated socket group, patch `runAsGroup`/`fsGroup` to that group; if your policy forbids GID 0, adjust node socket ownership or run a site-specific privileged wrapper. **Clearing `containerd_socket` is no longer a valid escape hatch** - after plan-final-copilot-v2 §Phase 8 containerd is Gantry's sole storage backend; without socket access the agent has no content store to read from or write to. The `storage_mode` config value must remain `containerd`. |
| Kubernetes RBAC scope | `serviceaccount.yaml` | The agent's only Kubernetes access is `leases` (`get`, `list`, `create`, `update`) on the 64 chair Leases in its own namespace. It runs no informer and opens no watch, and it needs no access to Pods or Nodes. Review the `Role` to confirm scope hasn't drifted; a `ClusterRole` should not exist. |

### HEAD semantics on cache miss

`GET /v2/<repo>/blobs/<digest>` on a cache miss warms the cache as a
side effect; `HEAD` on the same URL does NOT. This is intentional
(see the comment block in `internal/mirror/mirror.go` at the HEAD
return after `writeBlobHeaders`) - caching a multi-GB blob just
because a client asked for its size would defeat the bandwidth
amplification fix Gantry exists to provide. A subsequent GET for
the same digest follows the cache-miss path normally and warms
the cache then.

If your client emits HEAD-then-GET patterns where you'd prefer to
amortize the origin metadata round-trip, raise the issue upstream
(containerd's puller, BuildKit's resolver, etc.) - those clients
generally have a one-shot resolve-and-pull mode that skips the
HEAD entirely.
