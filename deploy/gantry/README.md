# Gantry deployment artifacts

This directory carries the operator-facing pieces needed to roll out
the gantry agent as a Kubernetes DaemonSet.

## Files

These are Go templates (`*.yaml.tmpl`). Render them with
`make gantry-manifests`, which writes plain manifests into
`deploy/gantry/rendered/`. The install namespace defaults to
`unbounded-system`. Lease rendezvous is the only discovery path. Upgrading an
existing cluster is a direct cutover: while the rollout is in progress, agents
that cannot yet find a peer fall back to pulling from the origin registry. Use
`GANTRY_RENDEZVOUS_SLOT_COUNT=<n>` to change the fixed slot count.

| Template | Rendered to | Purpose |
| --- | --- | --- |
| `daemonset.yaml.tmpl` | `rendered/daemonset.yaml` | One-pod-per-node DaemonSet. |
| `serviceaccount.yaml.tmpl` | `rendered/serviceaccount.yaml` | Namespace + ServiceAccount + fixed-slot RBAC + PriorityClass. |
| `rendezvous-leases.yaml.tmpl` | `rendered/rendezvous-leases.yaml` | Fixed, precreated Lease rendezvous slots. |
| `configmap.yaml.tmpl` | `rendered/configmap.yaml` | Default deployment `config.yaml`. |
| `examples/registry-secret.example.yaml.tmpl` | `rendered/examples/registry-secret.example.yaml` | Template Secret for upstream-registry credentials. |
| `examples/networkpolicy.yaml.tmpl` | `rendered/examples/networkpolicy.yaml` | **Hardening overlay (NOT applied by default).** See [Hardening overlays](#hardening-overlays) below. |
| `hosts.toml.template` | (not rendered) | containerd registry mirror config; one file per upstream registry under `/etc/containerd/certs.d/<host>/hosts.toml`. |
| `node-config.yaml` | (not rendered) | Standalone node configurator for containerd's default Gantry mirror. |

The container image is built from `images/gantry/Containerfile` via
`make image-gantry-local` (or `make image-gantry-push` to push).

## Apply order

```sh
# Render the templates into deploy/gantry/rendered/ first (defaults to the
# unbounded-system namespace; override with UNBOUNDED_NAMESPACE / GANTRY_NAMESPACE).
make gantry-manifests

kubectl apply -f deploy/gantry/rendered/serviceaccount.yaml
kubectl apply -f deploy/gantry/rendered/rendezvous-leases.yaml
kubectl apply -f deploy/gantry/rendered/configmap.yaml
# Operator: for any PRIVATE upstream registry, edit
# rendered/examples/registry-secret.example.yaml (rename it, fill in real
# username:password values keyed by registry `name:`) and apply,
# AND uncomment the matching `credentials_path:` line in
# configmap.yaml. The default ConfigMap ships credentials-free so
# the agent starts cleanly against public registries without any
# Secret being applied - origin.New eagerly reads every
# credentials_path at startup, so an unmatched path would
# crashloop the pod.
kubectl apply -f deploy/gantry/rendered/examples/registry-secret.example.yaml   # private registries only
kubectl apply -f deploy/gantry/rendered/daemonset.yaml
# rendered/examples/networkpolicy.yaml is a hardening overlay; do NOT
# apply it as part of the initial install. See "Hardening overlays"
# below for the workflow.
```

## Lease rendezvous

The agent performs no Pod or Node API read, watch, or patch. Agents directly
GET a bounded sample of predictably named Lease slots, conditionally update one
slot using its observed `resourceVersion`, and renew only a slot they hold. The
operator creates slots only when absent so reconciliation does not overwrite
holder state. It removes labeled slots outside the current rendered key space
when slot count decreases.

Rendering keeps the named Gantry ClusterRole and binding but applies
an empty rule set. This is deliberate: applying the new manifests revokes a
legacy Node `list/watch` grant instead of leaving it behind merely
because the object disappeared from the desired manifest set.

The DaemonSet injects slot count, `single_node=false`, and the finite NF5 jitter
cap through environment variables. This is intentional: the operator retains
an existing Gantry ConfigMap rather than replacing it, and these settings must
stay aligned with the rendered fixed key space and clustered readiness policy.
The config loader discards only the known removed membership/HRW keys (including
`rendezvous.mode`) before applying strict unknown-field validation, so a retained
ConfigMap upgrades cleanly without allowing new typos to pass silently.

The default values are validation parameters rather than production sizing:

- 64 fixed slots;
- 8 exact GETs per normal discovery round;
- 4 claim candidates per round;
- 8 contacts accepted per slot;
- a full fixed-slot scan every third round;
- 90-second duration and 30-second renewal.
- at most 32 randomized unique peer dials per bootstrap pass.

`FindProviders` remains the warm-content path. A provider miss uses DHT
`GetClosestPeers` plus self for cold-puller selection. Different routing views
can select different pullers, so duplicate origin pulls are accepted and must
be measured. A finite `nf5_jitter_cap` is required because the agent does not
maintain an exact membership count. Lease records are discovery hints, not
authorization; private-network PSK distribution remains unresolved.

Clustered readiness requires a connected routing-table peer and a successful
immediate DHT self-test. Explicit single-node mode skips both requirements.
The optional `gantry.io/bootstrap-sample` is accepted from slots but is not yet
published because its peer-selection and refresh policy remain open design
items.

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

See `docs/detailed-design.md` §7.6 for the full metric catalogue.

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
   behaviour (`kubectl get nodes -o yaml | grep -A2 podCIDR`,
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
| Kubernetes RBAC scope | `serviceaccount.yaml` | The namespaced Role grants only `get` and `update` on the rendered fixed Lease names. The retained ClusterRole is intentionally empty so applying the manifests revokes the Node permissions held by an older release. |

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
