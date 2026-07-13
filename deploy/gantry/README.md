# Gantry deployment artifacts

This directory carries the operator-facing pieces needed to roll out
the gantry agent as a Kubernetes DaemonSet.

## Files

| File | Purpose |
| --- | --- |
| `daemonset.yaml` | One-pod-per-node DaemonSet. |
| `serviceaccount.yaml` | ServiceAccount + ClusterRole + Role + PriorityClass. |
| `configmap.yaml` | Default `config.yaml` (mirrors `config.NewDefault()`). |
| `registry-secret.example.yaml` | Template Secret for upstream-registry credentials. |
| `examples/networkpolicy.yaml` | **Hardening overlay (NOT applied by default).** See [Hardening overlays](#hardening-overlays) below. |
| `hosts.toml.template` | containerd registry mirror config; one file per upstream registry under `/etc/containerd/certs.d/<host>/hosts.toml`. |

The container image is built from `images/gantry/Containerfile` via
`make image-gantry-local` (or `make image-gantry-push` to push).

## Apply order

```sh
kubectl apply -f deploy/serviceaccount.yaml
kubectl apply -f deploy/configmap.yaml
# Optional legacy shared-identity mode: edit
# registry-secret.example.yaml (rename it, fill in real
# username:password values keyed by registry `name:`) and apply,
# AND uncomment the matching `credentials_path:` line in
# configmap.yaml. The default ConfigMap ships credentials-free so
# the agent starts cleanly against public registries without any
# Secret being applied - origin.New eagerly reads every
# credentials_path at startup, so an unmatched path would
# crashloop the pod.
kubectl apply -f deploy/registry-secret.example.yaml   # private registries only
kubectl apply -f deploy/daemonset.yaml
# deploy/examples/networkpolicy.yaml is a hardening overlay; do NOT
# apply it as part of the initial install. See "Hardening overlays"
# below for the workflow.
```

### Requester-delegated registry authorization

For a private HTTPS registry, the first unauthenticated digest request makes
Gantry probe the configured upstream `/v2/` endpoint. Gantry validates and
caches a `Basic` or `Bearer` challenge, returns it to containerd, and containerd
retries the mirror using the credentials supplied by kubelet/CRI:

- For `Bearer`, containerd exchanges the pull credential at the registry's
   verified HTTPS token realm and sends Gantry only the scoped access token.
- For `Basic`, containerd sends the Basic credential to Gantry directly.

Gantry forwards the resulting request-scoped authorization through peer
fetches and encrypted `please_pull` RPCs. A peer with the digest ignores it; a
designated peer that must contact origin uses it for that request. Gantry never
caches the credential or falls back to its own identity after rejection.

Setting `credentials_path` explicitly opts that registry into the legacy
shared-identity mode and skips requester challenge negotiation. Leave it unset
to use containerd's pod/request credential. Anonymous HTTPS registries are
detected by a `200` response from `/v2/` and continue without authentication.

Peer transfer uses plaintext h2c on the cluster network. Forwarding Basic or
Bearer authorization therefore requires a trusted, isolated network or an
encryption layer such as a service mesh. Basic credentials may be longer-lived
than Bearer tokens. The `please_pull` copy travels inside encrypted libp2p.

#### Requester and puller on different nodes

If the pod runs on node A but HRW selects node B as the designated origin
puller, containerd on A first completes the Basic/Bearer challenge with its
local Gantry agent. Gantry A then sends `please_pull` to Gantry B with the
request-scoped `Authorization` value over encrypted libp2p. Gantry B ignores
the credential if the digest is already local; otherwise it uses that exact
credential for the HTTPS origin request. Node B does not need the pod's
`imagePullSecret`, kubelet access, or separate registry credentials.

`please_pull` acknowledges that background work started before the origin pull
finishes. If origin later rejects the delegated credential on B, that 401
cannot travel back through the completed RPC. No provider is advertised; A
eventually retries or falls back to origin, where normal challenge negotiation
can refresh the credential.

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
| Agents are running | `kubectl -n gantry-system get ds gantry` |
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
> [`examples/networkpolicy.yaml`](examples/networkpolicy.yaml) (or
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
   `kubectl -n gantry-system rollout status ds/gantry`,
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
4. Roll back with `kubectl delete networkpolicy -n gantry-system
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
| containerd socket access | `daemonset.yaml` | The pod runs with non-root UID 65532 and primary GID 0 because many nodes expose `containerd.sock` as `root:root` mode 0660. Validate this on your target node pool before production. If your runtime uses a dedicated socket group, patch `runAsGroup`/`fsGroup` to that group; if your policy forbids GID 0, adjust node socket ownership or run a site-specific privileged wrapper. **Clearing `containerd_socket` is no longer a valid escape hatch** - after plan-final-copilot-v2 §Phase 8 containerd is Gantry's sole storage backend; without socket access the agent has no content store to read from or write to. The `storage_mode` config value must remain `containerd`. |
| Kubernetes RBAC scope | `serviceaccount.yaml` | The agent only consumes `pods.list/watch` (informer) plus `pods.patch` (self-announce of libp2p + transfer addresses) in its own namespace, and `nodes.list/watch` cluster-wide for the zone label. There is no `get` on pods - informer events deliver the objects without point reads. Review `ClusterRole/Role` to confirm scope hasn't drifted. Membership setup failure is fatal in production mode (Downward-API env vars set), so an RBAC misconfig surfaces as a CrashLoop on rollout instead of a silent single-node fallback. |

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
