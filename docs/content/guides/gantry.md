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

#### Transparent Operator-Managed ACR Authentication on Flex Nodes

The operator can install node-wide ACR credentials without changing application
Pod specifications or ServiceAccounts. It uses a dedicated infrastructure
ServiceAccount, `registry-pull`, in the operator namespace. All broker replicas
federate that account to the explicitly configured Azure pull identity.

```text
unchanged application -> kubelet exec adapter -> local Unix-socket broker
  -> infrastructure ServiceAccount federation -> ACR credentials
  -> kubelet/containerd -> Gantry -> ACR or peers
```

The broker runs as `DaemonSet/gantry-acr-auth` on Linux nodes labeled
`kubernetes.azure.com/nodepool-type=FlexNodes`. The label describes pool
membership, not where the machine runs: no Azure VM identity, IMDS endpoint,
application annotations, or application token projection is required. Native
AKS nodes and their existing providers are not modified.

The platform administrator or provisioning tool must prepare:

- A pull identity with `AcrPull` on the ACR, or the appropriate repository-reader
  role for an ABAC-enabled registry. It does not need attachment to a VM.
- Federation trust for the cluster's OIDC issuer, subject
  `system:serviceaccount:<operator-namespace>:registry-pull`, and audience
  `api://AzureADTokenExchange`.

The broker Pod explicitly projects its own short-lived token; the Azure
Workload Identity pod-mutation webhook is not required for this flow. The
operator does not create Azure identities, trust bindings, or role assignments,
and does not need Azure IAM-administrator permissions. It never falls back to
IMDS or another identity when federation fails.

Installation policy belongs in **`data.node-auth.yaml` alongside
`data.config.yaml`** in `ConfigMap/gantry-config`:

```yaml
mode: workloadIdentity
registry: example.azurecr.io
clientID: 11111111-2222-3333-4444-555555555555
tenantID: aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee
# Optional existing private image for an authorization probe.
probeImage: example.azurecr.io/private/application:v1
```

The optional `registry` field asks the operator to add that HTTPS upstream to
Gantry. It replaces the untouched sole `registry.example.com` placeholder;
other upstreams, settings, and comments are preserved under an optimistic merge
patch. It also scopes the broker's credential provider to that selected server.
Without `registry`, configure `upstream_registries` normally; eligible ACR entries
then share the explicitly configured pull identity. Identity
identifiers are not secrets. No identity is inferred from a registry hostname.

The deployment script `hack/scripts/aks-flex-cluster.sh --acr NAME` provisions
one pull identity per cluster by default, prepares trust and the registry-scoped
role, then hands this policy to the operator. An existing identity can instead
be supplied explicitly with `--acr-pull-identity`. A shared ACR is never created,
adopted, or deleted by that script. The operator release must contain this broker
feature; the network-only `v0.8.0` baseline does not, and capability checks stop
onboarding before registry grants when the operator is incompatible.

The helper installs a credential-provider exec adapter and its separately owned
kubelet systemd drop-in. It preserves the agent-generated unit, argument and
environment expansions, and containerd mirror route. A narrow Lease coordinates
kubelet restarts across nodes. The adapter only communicates over the root-only
Unix socket: it has no Azure token file or identity and does not forward
application ServiceAccount tokens. Registry credentials are not stored in host
provider configuration, ConfigMaps, Kubernetes Secrets, or logs by this feature.

The normal versioned operator image must be pullable before private ACR auth is
installed; do not make the installer depend on the registry it is configuring.
For development or separately published helpers, `--registry-auth-image` (or
`UNBOUNDED_REGISTRY_AUTH_IMAGE`) selects an exact operator image containing the
broker commands without changing unrelated component image settings.

Check reconciliation with:

```bash
kubectl -n unbounded-system rollout status daemonset/gantry-acr-auth
kubectl -n unbounded-system logs -l app.kubernetes.io/name=gantry-acr-auth
kubectl get sites -o yaml
```

`GantryReady` remains false while configured federation or installation is not
ready. The optional `probeImage` performs an authenticated manifest HEAD on each
node. It verifies access to that image, not all repositories or end-to-end Gantry
distribution. Without a probe, status explicitly reports repository permission
as unverified. Missing trust, wrong client/tenant IDs, and rejected registry
credentials fail visibly rather than switching identity.

`mode: external` is the default when no policy is supplied. Use it to retain
normal `imagePullSecrets` or externally managed providers. Existing providers
and unmarked host files are never overwritten. Shared Gantry `credentials_path`
entries are not automatically migrated to federation. This node-wide model
intentionally gives eligible workloads shared pull access; it does not add
per-application or cached-content tenant isolation. Public Azure ACR is the
initial supported cloud profile.

Owned-file drift is continuously repaired, including after node updates. Broker
replacement does not remove authentication. Explicit external mode removes only
owned adapter configuration under the same restart coordination. Prepare
replacement authentication first if private pulls are still needed, and wait
for cleanup before uninstalling the operator. Offline nodes may retain owned
files until their helper can run again.

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

## Use Racer as the Backend

{{< callout type="warning" >}}
Racer is pre-release. Its APIs, flags, and on-disk formats may change without a
migration path. Cached data is disposable; Racer is not durable storage. Start
with the [Azure Blob cache guide]({{< relref "guides/azure-blob-cache" >}}).
{{< /callout >}}

The installation and distribution flow above describe Gantry's default containerd
backend. In Racer mode, Racer caches image content and shares it between nodes.
Gantry still serves the local registry mirror, and it reads from upstream
registries when Racer asks for content it does not have. Gantry does not use
containerd storage or its own peer network in this mode.

Racer mode needs the operator-managed Gantry install and the Racer
[node requirements]({{< relref "reference/racer" >}}#node-requirements). The
standalone Helm chart does not support Racer mode.

### Turn On Racer Mode

1. Enable Gantry on a Site. Gantry is on by default for every Site unless
   `spec.components.gantry.enabled` is `false`.
2. Create a cluster-scoped cache named `gantry`:

   ```yaml
   apiVersion: racer.unbounded-cloud.io/v1alpha1
   kind: ClusterCache
   metadata:
     name: gantry
   ```

   Do not set `spec.origin` on this cache. Gantry is its origin. If
   `spec.origin` is set, the operator keeps Gantry in default mode and reports
   the reason `RacerOriginConflict`.

Creating any ClusterCache makes the operator install Racer. When the `gantry`
cache exists and Gantry is enabled, the operator changes the `gantry`
DaemonSet in `unbounded-system`:

- It sets `GANTRY_RACER_ENABLED=true`.
- It mounts the Racer sockets from `/run/racer/gantry/client` and
  `/run/racer/gantry/origin`.
- It copies node affinity, node selector, and tolerations from the
  `racer` DaemonSet. Gantry then runs on every Racer node, so each
  node can serve origin reads for the cache.

The operator waits for `racer` to exist before it makes these
changes. The cache does not install Gantry. If no Site enables Gantry, the
`gantry` cache is a plain Racer cache. The exception is a Gantry install the
operator already manages. It stays in place after every Site turns Gantry off,
and it still switches to Racer when the `gantry` cache exists. Workload
overrides on the `gantry` DaemonSet still apply on top of these changes.

### Configure Upstream Registries

The operator creates `ConfigMap/gantry-config` in `unbounded-system` once and
does not overwrite it. Edit `upstream_registries` there:

```yaml
upstream_registries:
  - name: "registry.example.com"
    endpoint: "https://registry.example.com"
```

In Racer mode:

- `name` must be a bare `host` or `host:port`, with no scheme or path.
- Gantry fetches only from registries in this list. Requests for other
  registries return `404`.
- Gantry uses only requester-delegated credentials. It ignores
  `credentials_path`. Delegated credentials need an `https` endpoint; `http`
  endpoints work only for anonymous registries.
- Containerd and the transfer listener are not used.

Use the same `gantry-config` on every node. Any node can be asked to read from
an upstream registry.

### Verify Racer Mode

Check that the DaemonSet runs in Racer mode and that each pod is ready:

```bash
kubectl -n unbounded-system get daemonset gantry \
    -o jsonpath='{.spec.template.spec.containers[?(@.name=="gantry")].env}'
kubectl -n unbounded-system rollout status daemonset/gantry
kubectl -n unbounded-system logs -l app.kubernetes.io/name=gantry -c gantry \
    --prefix --tail=-1 | grep "mirror startup gate released"
```

`/readyz` returns `503` until both Racer sockets accept connections. Then pull
an image from a configured registry and check these metrics:

| Signal | Expected result |
| --- | --- |
| `gantry_racer_mirror_requests_total{method,status,outcome}` | Increases for each mirror request served through Racer. |
| `gantry_racer_mirror_bytes_total{method}` | Counts bytes sent to local containerd. |
| `gantry_racer_mirror_duration_seconds` | Shows mirror request latency. |
| `gantry_racer_origin_requests_total{method,status}` | Increases when Racer asks this node to read from an upstream registry. Stays flat when Racer already has the content. |
| `gantry_racer_origin_bytes_total{kind}` | Counts bytes read from upstream registries. |

### Troubleshooting

- Every response from the Racer backend has a `Gantry-Racer-Request-ID`
  header. Search Gantry logs for that ID.
- A `503` from the mirror means Racer was not available. Gantry does not fall
  back to its own peer network in Racer mode. Containerd handles the error as it
  does for any failed mirror host.
- A `404` without a `Gantry-Racer-Request-ID` header means the registry is not
  in `upstream_registries`.
- If Gantry pods stay not ready, check the `racer` pod on the same
  node and confirm that `/run/racer/gantry/client` and
  `/run/racer/gantry/origin` exist.
- If the operator reports `RacerPending` for Gantry, it is waiting for the
  `racer` DaemonSet.

### Turn Off Racer Mode

Delete the cache:

```bash
kubectl delete clustercache gantry
```

The operator removes the Racer settings and rolls Gantry back to the
containerd backend. Racer stays installed while other ClusterCaches exist.
