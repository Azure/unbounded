# Gantry deployment artifacts

This directory carries the operator-facing pieces needed to roll out
the gantry agent as a Kubernetes DaemonSet.

## Files

These are Go templates (`*.yaml.tmpl`); image and namespace are render inputs.
The install namespace defaults to `unbounded-system`. Render them with
`make gantry-manifests` (override with `GANTRY_NAMESPACE=<ns>` or the unified
`UNBOUNDED_NAMESPACE=<ns>`), which writes plain manifests into
`deploy/gantry/rendered/`.

| Template | Rendered to | Purpose |
| --- | --- | --- |
| `daemonset.yaml.tmpl` | `rendered/daemonset.yaml` | One-pod-per-node DaemonSet. |
| `serviceaccount.yaml.tmpl` | `rendered/serviceaccount.yaml` | Namespace + ServiceAccount + ClusterRole + Role + PriorityClass. |
| `configmap.yaml.tmpl` | `rendered/configmap.yaml` | Default `config.yaml` (mirrors `config.NewDefault()`). |
| `examples/registry-secret.example.yaml.tmpl` | `rendered/examples/registry-secret.example.yaml` | Template Secret for upstream-registry credentials. |
| `examples/networkpolicy.yaml.tmpl` | `rendered/examples/networkpolicy.yaml` | **Hardening overlay (NOT applied by default).** See [Hardening overlays](#hardening-overlays) below. |
| `examples/racer-cache.yaml.tmpl` | `rendered/examples/racer-cache.yaml` | Optional dedicated Gantry P2PCache. |
| `examples/racer-daemonset-patch.yaml.tmpl` | `rendered/examples/racer-daemonset-patch.yaml` | Optional strategic merge patch for Racer sockets, groups, and ports. |
| `hosts.toml.template` | (not rendered) | containerd registry mirror config; one file per upstream registry under `/etc/containerd/certs.d/<host>/hosts.toml`. |
| `node-config.yaml` | (not rendered) | Standalone node configurator for containerd's default Gantry mirror. |

The container image is built from `images/gantry/Containerfile` via
`make image-gantry-local` (or `make image-gantry-push` to push).

## Standalone direct-backend apply order

```sh
# Render the templates into deploy/gantry/rendered/ first (defaults to the
# unbounded-system namespace; override with UNBOUNDED_NAMESPACE / GANTRY_NAMESPACE).
make gantry-manifests

kubectl apply -f deploy/gantry/rendered/serviceaccount.yaml
kubectl apply -f deploy/gantry/rendered/configmap.yaml
# Private registries normally use requester-delegated authentication.
# Only explicit shared-identity mode needs an edited credentials Secret and
# credentials_path. Never apply the example Secret with placeholder values.
kubectl apply -f deploy/gantry/rendered/rendezvous-leases.yaml
kubectl apply -f deploy/gantry/rendered/daemonset.yaml
# rendered/examples/networkpolicy.yaml is a hardening overlay; do NOT
# apply it as part of the initial install. See "Hardening overlays"
# below for the workflow.
```

## Optional Racer content backend

Direct distribution remains the default and `storage_mode: containerd` remains
required. For operator-managed deployments, create a user-managed P2PCache with
the backing annotation. The rendered example supplies this resource:

```sh
kubectl apply -f deploy/gantry/rendered/examples/racer-cache.yaml
```

Any live P2PCache installs both Racer workloads, even with zero Sites. Gantry
selection requires exactly one live P2PCache with
`unbounded-cloud.io/gantry-backing: "true"`, an empty `siteSelector`, at least one
live Site enabling Gantry, and full node coverage. Every nonterminating Node
must be Linux, have canonical `unbounded-cloud.io/site` membership in a live
Site, lack Racer exclusion, and have no taints unsupported by the managed Racer
dataplane. Each Site is an independent universe. The operator starts Gantry's
local origin without waiting for Racer readiness and rolls the Gantry pod
template with the selected cache name and UID.

The cache is user-owned: the operator never creates, adopts, edits, or deletes
it. A missing/removed annotation, `"false"`, or deletion of the selected cache
returns Gantry to direct when no other cache is selected. Invalid values,
multiple selections, or invalid selector/coverage/live-Site checks produce a
direct rollout plus an `InvalidGantryBacking` diagnostic. Same-name cache
recreation changes cache identity and triggers a rollout through its new UID.
Rolling transitions can temporarily mix backends, cause retries, and lose warm
cache reuse; they do not migrate cached content.

There is no operator backend toggle in `gantry-config`. Its existing payload and
registry settings are preserved, but generated backend arguments override old
`content_backend`/`racer_cache_name` values. The old
`unbounded-cloud.io/gantry-cache` label does not select a cache. To keep an old
dedicated cache on upgrade, explicitly annotate it with
`unbounded-cloud.io/gantry-backing: "true"` after checking coverage. Main-config
and backend overrides through pod env or flags are rejected for managed pods.

Gantry mounts the shared **parent directory** `/dev/racer` read/write, never an
individual socket inode. The init container prepares `/dev/racer/gantry` with
mode `2770` and group `65532`; Gantry retains UID `65532` and containerd's primary
group `0`, with supplemental group `65532`. Gantry serves
`/dev/racer/gantry/origin` (`0660`), and Racer serves `/dev/racer/gantry/cache`.
Racer mode omits transfer/chair ports and the service-account token. Chair
Leases/RBAC are not created or reconciled in this mode; old resources are retained
for rollback. The P2PCache is retained when returning to direct.

For standalone deployments, deploy Racer, enforce coverage yourself, apply the
rendered `examples/racer-cache.yaml`, and set these Gantry ConfigMap fields (or
equivalent standalone flags):

```yaml
content_backend: racer
racer_cache_name: gantry
```

Standalone Gantry does not use the backing annotation to choose its backend.
Then use `kubectl patch --type=strategic --patch-file` with the rendered
`examples/racer-daemonset-patch.yaml`. Restart Gantry after changing its ConfigMap.
The patch is **not** a standalone manifest and assumes cache name `gantry`.
These examples are excluded from the operator's top-level manifest applies.

For operator-managed rollback, remove the backing annotation or set it to
`"false"` and wait for the Gantry rollout. Returning to direct retains the cache
unless you delete it explicitly. Racer retains each existing workload
independently after the last P2PCache is removed, updating but not recreating it.
To uninstall Racer, remove all P2PCaches, then delete
`deployment/racer-controlplane` and `daemonset/racer-dataplane` in either order.
Support resources alone do not reinstall them. Before upgrading, copy desired
Site cache sizes to Node `racer.unbounded-cloud.io/cache-size` annotations;
absent Node annotations now use `10Gi`, with no Site fallback. See
[Racer installation and capacity](../../docs/content/concepts/racer.md#cluster-wide-installation).

See [the public Gantry guide](../../docs/content/guides/gantry.md#optional-racer-backend)
for rollout, authentication trust boundaries, 64 MiB stripes, fallback limits,
readiness signals, and rollback. Run `make gantry-integration-test` for the bounded
operator/config/template checks; live e2e coverage is a separate suite.

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

Use `node-config.yaml` only for standalone installs or non-agent-managed nodes
that still need the default Gantry mirror entry written onto the node.

For each upstream registry the cluster pulls from, drop a
`hosts.toml` at:

```
/etc/containerd/certs.d/<registry-host>/hosts.toml
```

derived from `hosts.toml.template` (substitute `${REGISTRY_SERVER}`
with the registry's `https://...` URL). containerd reloads `certs.d`
on its own; no restart needed.

## What to verify after a direct-backend rollout

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
