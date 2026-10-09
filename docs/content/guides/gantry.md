---
title: "Distribute Container Images with Gantry"
weight: 8
description: "Deploy and operate Gantry as a peer-to-peer containerd registry mirror."
---

Gantry is a decentralized, peer-to-peer OCI distribution layer for Kubernetes.
It reduces duplicate origin-registry traffic during large rollouts by
coordinating each digest fetch on a small number of nodes and serving the
content from peers that already have it.

Large container images can put substantial pressure on a registry when hundreds
or thousands of nodes start the same workload at once. Without coordination,
every node downloads the same layers independently. This can amplify registry
traffic, trigger rate limits, and require additional registry replicas or
egress capacity.

Gantry moves that fan-out into the cluster. A bounded group of nodes downloads
new content from the origin, and nodes that finish a pull become providers for
other nodes.

Gantry runs as a DaemonSet and uses each node's existing containerd content
store. It does not introduce a separate image cache or a central metadata
service. Peer discovery uses libp2p and a distributed hash table (DHT), and
containerd continues to verify every content digest before using it.

After the node-level mirror configuration is installed, Gantry is transparent
to workloads. Pods continue to use normal OCI image references,
`imagePullSecrets`, and kubelet credential providers.

## Measured at scale

In a 1,000-node cold rollout of a 40 GiB image, the measured chair-design run
set used 99.2% less ACR Private Endpoint traffic than the direct-origin
reference set. Mean pod-start P95 was 24.5% lower. Every Gantry run completed
all 1,000 pods.

See [Gantry Performance at Scale](/guides/gantry-benchmark/) for raw run values,
methodology, calculations, and limitations.

## Design principles

| Principle | Behavior |
| --- | --- |
| Transparent to workloads | Pods keep their normal image references, pull policies, and credentials. |
| Decentralized | There is no central Gantry content server or cluster-wide catalog. |
| No separate cache | Gantry reads and writes the containerd content store already used by kubelet. |
| Digest addressed | Discovery and transfer operate on immutable OCI digests rather than tags. |
| Horizontally supplied | A node that completes a pull can immediately serve the same content to other nodes. |
| Registry fallback | Retriable mirror responses let containerd continue through its configured registry host chain. |

## Architecture

![Gantry architecture: Requester node with Workload, containerd, and Gantry A; Gantry peer network containing the libp2p DHT and a Selected puller node with Gantry B and its containerd content store; Gantry A looks up providers in the DHT and coordinates pull plus streams image content with Gantry B, which fetches once from the Origin registry, while the requester containerd retains a direct fallback path to the Origin registry](../../img/gantry-architecture.svg)

The DHT contains provider records, not image data. Manifests and layers stream
directly from the selected Gantry peer, while containerd remains responsible
for digest verification and the node-local content store.

Each network endpoint has one role:

| Port | Purpose | Transport |
| ---: | --- | --- |
| 5000 | Node-local containerd mirror | HTTP on node loopback through hostPort |
| 5001 | Peer manifest and layer transfer | Cleartext HTTP/2 inside the trusted cluster network |
| 5002 | Ask selected chairs to start origin pulls | TLS with chair identity validation |
| 4001 | libp2p discovery and coordination | Encrypted TCP and QUIC |
| 9095 | Readiness, health, and Prometheus metrics | HTTP |

## How Gantry Handles a Pull

Containerd still resolves image tags through the origin registry. That produces
immutable manifest and layer digests. Gantry handles the digest-addressed
requests behind containerd's mirror interface.

### Local and warm pulls

1. Gantry first opens the digest from the local containerd content store.
2. On a local miss, Gantry asks the DHT for nodes that advertise the digest.
3. Gantry filters stale, unavailable, self, and suspicious providers, then
   requests the content from a remaining peer.
4. The peer reads the bytes from its containerd store and streams them over
   the transfer endpoint.
5. The requester hashes the stream while containerd performs the final digest
   verification and commits the content.
6. After the commit, the requester advertises itself as another provider.

Supply therefore grows during a rollout. Nodes that complete a layer can serve
it to the next requesters instead of sending every request back to the original
seed.

If a peer stream ends partway through, Gantry can preserve the verified offset
and resume from another provider with an HTTP Range request instead of
restarting the entire transfer.

### Cold pulls

When the DHT has no provider, Gantry selects a bounded origin-puller cohort:

1. Gantry reads the current Kubernetes Lease assignments for the chair pool.
2. Every requester ranks the same chair slots for the digest using
   highest-random-weight selection.
3. The highest-ranked group receives concurrent requests to start the origin
   pull. Each selected chair starts at most one local pull for the digest.
4. A chair commits the content to its containerd store and advertises itself.
5. Waiting requesters discover the provider and continue through the warm path.

The default configuration has 64 chair slots and a full-pool seed target of
eight. Different digests produce different rankings, which distributes origin
work across the pool without a central per-layer coordinator.

Gantry routes content by digest. Tag resolution still reaches the origin, so
the exact reduction in registry requests depends on image composition and
rollout size. Large layers distributed to many nodes receive the greatest
benefit.

## Failure recovery

Gantry classifies failures so one bad provider does not restart the entire
rollout:

| Condition | Behavior |
| --- | --- |
| Peer returns 404 | Mark the provider stale for the digest and try another provider. |
| Peer returns 429 | Treat the peer as busy and reselect without quarantining it. |
| Peer is unavailable | Temporarily avoid that peer. |
| Stream stops | Resume the verified prefix from another provider when possible. |
| Digest mismatch | Reject the transfer and quarantine that provider for the digest. |
| Chair is already pulling | Treat the response as progress and wait for a provider advertisement. |
| Selected chair group makes no progress | Move to the next ranked group after bounded rechecks. |
| Warm providers are exhausted | Return a retriable response so containerd can use its next configured host. |
| Chair ranking is exhausted | Evaluate the guarded direct-origin fallback or return a retriable response. |

The guarded direct-origin fallback checks DHT bootstrap and health,
deduplication, local rate capacity, a randomized delay, and one final provider
lookup before starting another registry pull. These gates favor retry over an
uncoordinated registry stampede.

## Security and integrity

- Containerd verifies every requested digest before using the content.
- Gantry also hashes live peer streams and quarantines a provider after a
  mismatch.
- libp2p discovery and coordination traffic is encrypted.
- Chair origin-pull requests use TLS and validate the current Lease holder's
  identity.
- The high-volume peer transfer endpoint uses cleartext HTTP/2. Restrict port
  5001 to trusted Gantry peers with NetworkPolicy or equivalent network
  controls.
- The containerd mirror is bound to node loopback through hostPort 5000.
- Request-scoped registry credentials are not persisted by Gantry.

## Deployment options

Gantry is available in two deployment models:

- **Operator managed:** the Unbounded operator owns the Gantry configuration,
  image, RBAC, chair Leases, and DaemonSet as part of an Unbounded cluster.
- **Standalone Helm chart:** third-party Kubernetes clusters can install and
  upgrade Gantry directly from the published OCI chart.

Use only one ownership model in a cluster. The standalone chart detects the
operator-managed priority class and refuses to take ownership of the same
resources.

## Installation

### 1. Confirm Compatibility

The standalone chart supports Linux nodes running containerd. Before installing,
confirm that every selected node exposes `/run/containerd/containerd.sock` and
that containerd reads registry host configuration from
`/etc/containerd/certs.d`.

The chart does not modify or restart containerd. Provision the `certs.d`
setting through your node-management system. For standalone installations, the
chart runs a node-config DaemonSet that continuously reconciles the Gantry
mirror route shown below. The Unbounded agent owns that route instead on
operator-managed nodes, where the chart's node-config resources are disabled.

The default route written on each node is:

```toml
# /etc/containerd/certs.d/_default/hosts.toml
[host."http://127.0.0.1:5000"]
   capabilities = ["pull", "resolve"]
   dial_timeout = "200ms"
```

Containerd must already have its CRI registry `config_path` set to
`/etc/containerd/certs.d`. The chart mounts the runtime directory and expects
the socket at `/run/containerd/containerd.sock` by default; use the
`containerd.*` chart values for a different layout.

Do not install the chart on a cluster where the Unbounded operator manages
Gantry. Both paths check `PriorityClass/gantry-low` and reject ownership by the
other manager.

### 2. Install the OCI Chart

Find the release on the
[Unbounded releases page](https://github.com/Azure/unbounded/releases). The
chart version omits the release tag's leading `v`:

```bash
export GANTRY_VERSION="<release-without-v>"
export GANTRY_IMAGE_DIGEST="sha256:<digest-from-release-bom>"

helm upgrade --install gantry oci://ghcr.io/azure/charts/gantry \
   --version "$GANTRY_VERSION" \
   --namespace gantry-system \
   --create-namespace \
   --set image.digest="$GANTRY_IMAGE_DIGEST" \
   --set gantry.upstreamRegistries[0].name=registry.example.com \
   --set gantry.upstreamRegistries[0].endpoint=https://registry.example.com \
   --wait \
   --timeout 15m
```

The chart owns the Gantry ConfigMap, image, RBAC, PriorityClass, chair Leases,
agent DaemonSet, and node-config DaemonSet. The node-config process checks its
`_default/hosts.toml` every five seconds and atomically restores it after drift
or a node upgrade reset. Upgrade those resources through `helm upgrade`, not
direct edits. Set `nodeConfig.enabled=false` only when another node-management
system owns mirror routing.

During graceful disable or uninstall, the node-config pod removes
`hosts.toml` only when it still matches the chart payload. Nodes unavailable
during uninstall may retain the file and should be checked before the Gantry
endpoint is considered fully removed.

### 3. Configure Upstream Registries

Set `gantry.upstreamRegistries` in your values file. This list is an opt-in allowlist: add only the
origin registries whose image pulls you want Gantry to accelerate. Registries
that are not configured here continue using containerd's normal direct-origin
path and are not distributed through Gantry.

```yaml
gantry:
   upstreamRegistries:
      - name: registry.example.com
         endpoint: https://registry.example.com
```

The `name` must match the registry host in the image reference and in
containerd's `certs.d` directory. Include the port when the image reference
uses a non-default port. Gantry requires at least one accelerated upstream
registry.



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

#### Shared-Identity Authentication

Shared identity is a compatibility mode for environments where kubelet or CRI
cannot provide a usable request credential. It gives every Gantry agent the
same registry identity.

To enable it, create `Secret/gantry-registry-credentials` in the release
namespace and set the matching registry's `credentialsPath` chart value.
The file contains a `username:password` pair keyed by the registry `name`.

Gantry reads configured credential files eagerly during startup. A missing
file causes the pod to fail, so do not add `credentials_path` unless the Secret
is present. Prefer requester-delegated authentication when possible because it
avoids distributing a shared registry credential to every node.

### 5. Verify Distribution

First verify one agent directly. Select a pod and forward its health and
metrics listener:

```bash
GANTRY_POD=$(kubectl -n gantry-system get pods \
    -l app.kubernetes.io/name=gantry \
    -o jsonpath='{.items[0].metadata.name}')

kubectl -n gantry-system port-forward "pod/${GANTRY_POD}" 9095:9095
```

In another shell:

```bash
curl -fsS http://127.0.0.1:9095/readyz
curl -fsS http://127.0.0.1:9095/metrics
```

Roll out the same multi-layer image to at least two nodes, then inspect these
metrics:

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
