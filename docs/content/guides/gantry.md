---
title: "Distribute Container Images with Gantry"
weight: 8
description: "Deploy and operate Gantry as a peer-to-peer containerd registry mirror."
---

Gantry is a decentralized, peer-to-peer OCI distribution layer for Kubernetes.
It reduces duplicate origin-registry traffic during large rollouts by
coordinating each digest fetch on a small number of nodes and serving the
content from peers that already have it.

Gantry runs as a DaemonSet and uses each node's existing containerd content
store. The default `content_backend: direct` uses libp2p discovery, a distributed
hash table (DHT), and Lease chairs without a separate image cache. The optional
`content_backend: racer` uses Racer's distributed cache after a local miss.
Containerd remains the committed image store and verifies every content digest
before using it. `storage_mode: containerd` is required with either backend.

After the node-level mirror configuration is installed, Gantry is transparent
to workloads. Pods continue to use normal OCI image references,
`imagePullSecrets`, and kubelet credential providers.

## Architecture

The diagram and pull sequence below describe the default direct backend. See
[Optional Racer backend](#optional-racer-backend) for the alternate content path.

![Gantry architecture: Requester node with Workload, containerd, and Gantry A; Gantry peer network containing the libp2p DHT and a Selected puller node with Gantry B and its containerd content store; Gantry A looks up providers in the DHT and coordinates pull plus streams image content with Gantry B, which fetches once from the Origin registry, while the requester containerd retains a direct fallback path to the Origin registry](../../img/gantry-architecture.svg)

The DHT contains provider records, not image data. Manifests and layers stream
directly from the selected Gantry peer, while containerd remains responsible
for digest verification and the node-local content store.

## How Gantry Handles a Pull

1. Containerd resolves an image tag at the origin registry and obtains a
   content digest.
2. Containerd requests that digest through the node-local Gantry mirror.
3. Gantry checks the local containerd content store and asks the DHT for peers
   that already provide the digest.
4. If no provider exists, Gantry uses deterministic highest-random-weight
  (HRW) selection, a consistent-hashing algorithm, to choose one of a small
  set of nodes to fetch the digest. Every agent reaches the same choice
  without a consensus service.
5. Other nodes stream the content from that peer. The requesting containerd
   verifies and commits it to its own content store.

Gantry routes content by digest. Tag resolution still reaches the origin, so
the exact reduction in registry requests depends on image composition and
rollout size. Large layers distributed to many nodes receive the greatest
benefit.

## Installation

### 1. Select a Released Gantry Image

Find the latest Gantry release on the
[Unbounded releases page](https://github.com/Azure/unbounded/releases) and use
its published image:

```bash
export GANTRY_IMAGE="ghcr.io/azure/gantry:<release-version>"
```

### 2. Configure Upstream Registries

Run `make gantry-manifests GANTRY_IMAGE="$GANTRY_IMAGE"`, then edit
`deploy/gantry/rendered/configmap.yaml` and replace the example
`upstream_registries` entry. This list is an opt-in allowlist: add only the
origin registries whose image pulls you want Gantry to accelerate. Registries
that are not configured here continue using containerd's normal direct-origin
path and are not distributed through Gantry.

```yaml
upstream_registries:
  - name: "registry.example.com"
    endpoint: "https://registry.example.com"
```

The `name` must match the registry host in the image reference and in
containerd's `certs.d` directory. Include the port when the image reference
uses a non-default port. Gantry requires at least one accelerated upstream
registry.



### 3. Deploy Gantry


```bash
kubectl apply -f deploy/gantry/rendered/serviceaccount.yaml
# Only for nodes whose mirror configuration is not managed by unbounded-agent:
kubectl apply -f deploy/gantry/rendered/node-config.yaml
kubectl -n unbounded-system rollout status daemonset/gantry-containerd-config
kubectl apply -f deploy/gantry/rendered/configmap.yaml
kubectl apply -f deploy/gantry/rendered/rendezvous-leases.yaml
kubectl apply -f deploy/gantry/rendered/daemonset.yaml

kubectl -n unbounded-system rollout status daemonset/gantry
kubectl -n unbounded-system get pods -o wide
```


### 4. Private Registry Authentication

#### Requester-Delegated Authentication

The workload supplies credentials in the normal Kubernetes way, such as an
`imagePullSecret` or a kubelet credential provider. For a private HTTPS
registry, the flow is:

1. Containerd sends an unauthenticated digest request to its local Gantry
   agent.
2. Gantry probes the registry's verified HTTPS `/v2/` endpoint and relays its
   Basic or Bearer `WWW-Authenticate` challenge.
3. Containerd answers the challenge using the credential supplied by kubelet
   or CRI. For Bearer authentication, containerd exchanges that credential at
   the registry's HTTPS token service and sends Gantry the scoped token.
4. Gantry carries the request-scoped authorization to the selected origin
   puller. The selected node does not need its own registry identity or access
   to the requester's `imagePullSecret`.
5. Gantry discards the authorization after the request. It does not persist
   the credential or fall back to the puller's identity if the registry
   rejects it.

Requester-delegated private-registry authentication requires an HTTPS origin.
Private plaintext HTTP registries are not supported by this mode.

Cached content is shared within the trusted cluster. Neither containerd-local
hits nor Racer cache hits perform a fresh registry authorization check for each
digest. Request-scoped credentials authenticate upstream requests; they are not
a per-tenant cache access-control boundary. Restrict access to Gantry's mirror,
the containerd API, and Racer's local sockets to trusted node components.

#### Shared-Identity Authentication

Shared identity is a compatibility mode for environments where kubelet or CRI
cannot provide a usable request credential. It gives every Gantry agent the
same registry identity.

To enable it, copy and edit
`deploy/gantry/examples/registry-secret.example.yaml`, apply the resulting
Secret, and set the matching registry's `credentials_path` in the ConfigMap.
The file contains a `username:password` pair keyed by the registry `name`.

Gantry reads configured credential files eagerly during startup. A missing
file causes the pod to fail, so do not add `credentials_path` unless the Secret
is present. Prefer requester-delegated authentication when possible because it
avoids distributing a shared registry credential to every node.

### 5. Verify Distribution

First verify one agent directly. Select a pod and forward its health and
metrics listener:

```bash
GANTRY_POD=$(kubectl -n unbounded-system get pods \
    -l app.kubernetes.io/name=gantry \
    -o jsonpath='{.items[0].metadata.name}')

kubectl -n unbounded-system port-forward "pod/${GANTRY_POD}" 9095:9095
```

In another shell:

```bash
curl -fsS http://127.0.0.1:9095/readyz
curl -fsS http://127.0.0.1:9095/metrics
```

Roll out the same multi-layer image to at least two nodes, then inspect these
metrics:

The peer/chair/advertiser signals in this table apply to the direct backend.

| Signal | Expected result |
| --- | --- |
| `p2p_dht_health_score` | Reaches at least `0.7` after the routing table forms. |
| `gantry_storage_mode_info{mode="containerd"}` | Equals `1` on every agent. |
| `p2p_cache_hit_total` | Increases when the local containerd store satisfies mirror requests. |
| `p2p_peer_fetch_total{outcome="hit"}` | Increases when a node receives content from a peer. |
| `p2p_origin_pull_total` | Increases only on designated origin pullers during a cold rollout. |
| `gantry_origin_bytes_total{kind}` | Counts bytes read from upstream registries, including partial transfers and retries. |
| `gantry_peer_fetch_bytes_total{kind}` | Counts bytes received from peer Gantry agents. |
| `gantry_peer_serve_bytes_total{kind}` | Counts bytes transmitted to peers; range requests count only the transmitted range. |
| `gantry_mirror_bytes_served_total{kind,source}` | Counts bytes served to local containerd, split by `cache`, `peer`, or `origin`. |
| `p2p_origin_fallback_total` | Remains near zero during healthy operation. |
| `gantry_advertise_reconcile_total` | Continues increasing as Gantry reconciles containerd content with the DHT. |
| `gantry_containerd_lease_created_total` | Increases when coordinated background pulls ingest new content. |

## Optional Racer Backend

Racer is opt-in. Existing installations with an omitted `content_backend`, or
with `content_backend: direct`, continue using direct distribution. Building
Gantry does not require building Racer; Racer mode additionally requires the
version-matched Racer control-plane and Linux dataplane deployment described in
the [Racer documentation](../../concepts/racer/).

### Operator-managed enablement

1. Enable `spec.components.racer.enabled: true` on **every Gantry-enabled Site**.
   Gantry and Racer must be enabled together on all participating Sites. Gantry
   is a cluster-wide singleton, not a per-Site backend choice.
2. Every node covered by the Gantry DaemonSet must be Linux, assigned to one of
   those Sites, and not labeled `racer.unbounded-cloud.io/exclude: "true"`.
   Unassigned nodes, including control-plane nodes, must be accounted for.
   Custom `NoSchedule`/`NoExecute` taints unsupported by the managed Racer
   DaemonSet are rejected. Wait for each Racer DaemonSet to become ready first.
3. Edit the existing ConfigMap, preserving your registry configuration:

   ```bash
   kubectl -n unbounded-system edit configmap gantry-config
   ```

   Set these fields inside `data.config.yaml`:

   ```yaml
   content_backend: racer
   racer_cache_name: gantry
   storage_mode: containerd
   ```

The operator reads this ConfigMap as the authoritative deployment-wide source,
validates coverage, and rolls Gantry when the payload changes. Pod overrides
cannot redirect the config source or set `GANTRY_CONTENT_BACKEND`,
`GANTRY_RACER_CACHE_NAME`, `--content-backend`, or `--racer-cache-name`.
Those environment variables and flags remain available for manually managed
Gantry processes.

The operator creates this cluster-scoped resource:

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: P2PCache
metadata:
  name: gantry
  labels:
    unbounded-cloud.io/gantry-cache: "true"
spec:
  siteSelector: {}
  cacheGeneration: 1
  maxCandidateAttempts: 3
```

An empty selector selects every Racer-enabled Site. The enablement checks make
this exactly the participating Gantry Site set. Each Site has an independent
cache universe. The operator rejects an existing cache with a different owner
label or a nonempty selector, and preserves the generation and candidate policy
of an existing compatible cache. Use a dedicated cache name, never an unrelated
application's P2PCache. Changing `racer_cache_name` changes the P2PCache name and
both socket paths.

| Resource | Node-local mapping |
| --- | --- |
| P2PCache `gantry` | Racer serves `/dev/racer/gantry/cache` |
| Gantry origin | Gantry serves `/dev/racer/gantry/origin` with mode `0660` |
| Shared hostPath | `/dev/racer`, mounted read/write as a parent directory, without `subPath` |
| Permissions | Parent and cache directory mode `2770`, group `65532`; Gantry UID `65532`, primary GID `0` for containerd, supplemental group `65532` |

Mounting the directory lets each process reconnect after socket replacement.
Do not bind-mount individual socket files. The initialization container sets
permissions before Gantry starts, independent of which daemon starts first.

### Content path, authentication, and limits

Gantry checks local containerd first, then requests digest-addressed content
through Racer. Racer distributes objects in **64 MiB stripes (pages)** and
stores disposable cached data in its slab, normally under `/var/lib/racer`.
Gantry's origin adapter serves local containerd content or bounded registry
range reads. The origin never calls the Gantry mirror or Racer recursively.
OCI target keys include registry, repository, object kind, and digest.

Basic/Bearer authorization travels with the request through the Racer SDK and
authenticated peer transport to the node-local origin for upstream access.
Credentials are not cache keys and are not persisted with content. All origin
nodes need the same upstream allowlist, endpoint settings, and any explicitly
configured shared-identity credentials. The trusted-cache limitation in the
authentication section applies here too.

Full responses use verified streaming: Linux splice forwards payload while tee
feeds SHA-256 verification. Single-range responses are version-pinned but are
not reported as full-object digest verification. Containerd still verifies the
completed object. Racer mode is demand-only: no direct Gantry transfer server,
chair calls, please-pull coordination, DHT content advertising, or speculative
layer downloads. Libp2p uses a separate `/gantry/racer` protocol namespace.
Ports 5001/5002 and service-account token mounting are omitted. Fresh Racer-mode
installs do not create chair Leases or their Role/RoleBinding; existing chair
resources are retained for rollback and stop being reconciled in Racer mode.

The origin registry must support usable metadata and bounded byte-range reads
for acceleration. Unsupported metadata/ranges or cache unavailability before
response headers can use Gantry's ordinary registry fallback. This does not
activate the direct Gantry peer backend. Authentication failures are propagated;
an interrupted or invalid stream after headers is aborted rather than spliced
together with a second source. Readiness requires working containerd, origin
UDS, and cache UDS. `P2PCache Ready` describes cache activation, not Gantry origin
availability, so verify Gantry readiness separately.

The operator reports invalid coverage rather than silently selecting direct.
It does not provide atomic fleet-wide cutover or prevent later node label,
taint, Site, or out-of-band workload changes. During rolling updates direct and
Racer agents can coexist temporarily but do not share Gantry content protocols.
Pause large image rollouts until both Racer and Gantry are ready. Scheduling,
socket, and identity overrides that break managed origin coverage are unsupported.

### Verify and roll back

```bash
kubectl get p2pcache gantry
kubectl -n unbounded-system rollout status daemonset/gantry
kubectl -n unbounded-system get pods -o wide
```

Inspect `gantry_racer_available`, `gantry_racer_stream_total{outcome}`, actual
`gantry_racer_splice_bytes_total` / `gantry_racer_tee_bytes_total`,
`gantry_racer_buffered_bytes_total`, and `gantry_racer_fallback_total` alongside
`gantry_mirror_bytes_served_total{source="racer"}`. Exercise a cold image pull and
a repeated pull across multiple nodes. Do not use DHT/chair metrics as Racer
readiness evidence.

To return to direct distribution, set `content_backend: direct` in the same
ConfigMap and wait for Gantry's rollout before disabling Racer. Chair resources
are reconciled again. The dedicated P2PCache and Racer slab are retained; remove
the unused cache explicitly only after no Gantry process uses it. Changing
backend never migrates or deletes containerd's committed images.

### Standalone manifests

For a manually managed installation, render with `make gantry-manifests`, set
the same ConfigMap fields, deploy Racer and
`deploy/gantry/rendered/examples/racer-cache.yaml`, and apply the socket/port patch:

```bash
kubectl -n unbounded-system patch daemonset gantry --type=strategic \
  --patch-file deploy/gantry/rendered/examples/racer-daemonset-patch.yaml
kubectl -n unbounded-system rollout restart daemonset/gantry
```

The patch assumes cache name `gantry`; change its directory initialization if
you choose another name. Skip `rendezvous-leases.yaml` for a fresh Racer-only
install. Standalone deployments must enforce the same all-node origin coverage
and identical config themselves. The operator-managed path performs these checks
and applies the pod changes automatically.
