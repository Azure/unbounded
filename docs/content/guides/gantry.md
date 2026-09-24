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
store. The default direct backend uses libp2p discovery, a distributed
hash table (DHT), and Lease chairs without a separate image cache. The optional
Racer backend uses Racer's distributed cache after a local miss.
Containerd remains the committed image store and checks incoming content against
the expected OCI digest at commit. `storage_mode: containerd` is required with
either backend.

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

The following manifest workflow installs standalone Gantry with the direct
backend. For operator-managed Gantry, the operator installs the cluster-wide
DaemonSet when at least one live Site enables Gantry (the Site default), and
preserves the existing `gantry-config` ConfigMap. Configure upstream registries
there. To select Racer, follow [operator-managed enablement](#operator-managed-enablement).

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

For an identical registry on each node, use a ClusterIP Service with
`internalTrafficPolicy: Local` selecting the registry pods, and configure its
ordinary Service DNS name as the upstream endpoint on every Gantry node. Traffic
from each Gantry pod reaches only an endpoint on that node. If no local endpoint
exists, the request fails rather than falling back to another node's registry.
For the combined loadgen fixture, also set `publishNotReadyAddresses: true`:
its registry must be reachable before client readiness can pass the Gantry
startup gate. The registry itself returns 503 while preparing its dataset.
See the [loadgen example](https://github.com/Azure/unbounded/blob/main/e2e/racer/examples/container-image-loadgen.yaml).



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

Using Racer as Gantry's backend is opt-in. Operator-managed Gantry uses direct
distribution unless a valid user-managed P2PCache selects Racer through its
`unbounded-cloud.io/gantry-backing: "true"` annotation. Building
Gantry does not require building Racer; Racer mode additionally requires the
version-matched Racer control-plane and Linux dataplane deployment described in
the [Racer documentation](../../concepts/racer/).

### Operator-managed enablement

1. Ensure at least one live Site enables Gantry. Gantry is enabled by default;
   only `spec.components.gantry.enabled: false` opts a Site out.
2. Every nonterminating node must be Linux, assigned to a live Site through the
   canonical `unbounded-cloud.io/site` Node label, and not labeled
   `racer.unbounded-cloud.io/exclude: "true"`.
   Unassigned nodes, including control-plane nodes, must be accounted for.
   Custom `NoSchedule`/`NoExecute` taints unsupported by the managed Racer
   DaemonSet invalidate the selection.
3. Create a dedicated, user-managed, cluster-scoped cache. Save this as
   `gantry-cache.yaml` and run `kubectl apply -f gantry-cache.yaml`:

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: P2PCache
metadata:
  name: gantry
  annotations:
    unbounded-cloud.io/gantry-backing: "true"
spec:
  siteSelector: {}
  cacheGeneration: 1
  maxCandidateAttempts: 3
```

Any live P2PCache installs both Racer workloads, even with zero Sites. Selecting
it for Gantry additionally requires exactly one live cache annotated `"true"`, an
empty `siteSelector` (no match labels or expressions), full node coverage, and at
least one live Site enabling Gantry. The empty selector selects every live Site,
each with an independent cache universe. The operator starts Gantry's origin
without waiting for Racer readiness; verify both rollouts afterward.

You own the P2PCache, including its name, generation, and candidate policy. The
operator does not create, adopt, modify, or delete it. Use a dedicated cache,
never an unrelated application's P2PCache. The selected resource's name determines
both socket paths. Recreating it with the same name changes its UID and cache
identity and triggers a Gantry rollout.

An absent annotation or the exact string `"false"` does not select a cache.
Removing the annotation or deleting the selected cache returns Gantry to direct
when no other cache is selected. Invalid annotation values (including `"True"`
or an empty string), multiple selections, a nonempty selector, or failed
coverage/live-Site checks also select direct, with an `InvalidGantryBacking`
diagnostic in the Site's `GantryReady` condition. Terminating caches do not vote.
API read failures instead preserve the deployed pod configuration and retry.

The operator preserves `gantry-config`, including upstream registries, and rolls
Gantry when its payload changes. **There is no operator backend toggle in
`config.yaml`.** Generated `--content-backend` and `--racer-cache-name` arguments
override old YAML backend settings. Pod overrides cannot redirect the config
source or set `GANTRY_CONTENT_BACKEND`, `GANTRY_RACER_CACHE_NAME`,
`--content-backend`, or `--racer-cache-name`. Standalone processes still support
these flags, environment variables, and YAML settings.
The preserved YAML must still parse successfully; generated flags do not hide
malformed YAML or unknown configuration fields.

On upgrade, an old operator-created cache or its `unbounded-cloud.io/gantry-cache`
label is not selected automatically. To keep using that dedicated cache, ensure
it satisfies the checks above and annotate it explicitly:

```bash
kubectl annotate p2pcache gantry unbounded-cloud.io/gantry-backing=true --overwrite
```

There is no automatic cache migration. Existing ConfigMap backend values are
preserved but overridden by selection. Before upgrading, also copy any desired
Site cache sizes to Node annotations as described in
[Racer capacity and upgrade](../../concepts/racer/#cache-capacity-and-upgrade).

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

Gantry requests all digest-addressed manifests, configs, and layers through
Racer, including HEAD probes and content already in local containerd.
Racer distributes objects in **64 MiB stripes (pages)** and
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

Full Racer responses use the SDK's version-pinned `Stream`, without SHA-256
verification in Gantry. The SDK receives the raw downstream TCP connection so
Linux splice can forward payload without duplicating it for hashing. Single-range
responses use version-pinned `ReadRange`. Neither establishes the OCI digest.
Containerd's check against the expected OCI digest at commit is authoritative
for accepting downloaded image content. A complete HTTP response is forwarding
completion, not content acceptance: incorrect same-length bytes can complete
HTTP successfully and still fail containerd's commit.

Racer retains CRC64/ECMA-182 checks for peer transfer admission and background
disk scrubbing. It computes an admission CRC over origin bytes, but that CRC is
not proof that those bytes match an OCI digest. Incorrect origin bytes can have
a self-consistent CRC and be cached and forwarded. File-backed local cache hits
are not rehashed on each foreground read; background scrubbing detects changes
relative to the stored CRC, not an incorrect original OCI payload. Generic HTTP
consumers must validate content themselves. The SDK's `StreamVerified` remains
available for callers that supply an independent expected SHA-256 digest.
The direct backend still verifies its live registry streams in-process.

Racer mode is demand-only: no direct Gantry transfer server,
chair calls, please-pull coordination, DHT content advertising, or speculative
layer downloads. Gantry starts no libp2p host or DHT in Racer mode; Racer owns
peer discovery and transport. The containerd subscriber remains active to
populate the local media-type index through its image walks, preserving known
manifest and index types for local serving and Racer origin metadata.
Ports 5001/5002 and service-account token mounting are omitted. Fresh Racer-mode
installs do not create chair Leases or their Role/RoleBinding; existing chair
resources are retained for rollback and stop being reconciled in Racer mode.

The origin registry must support usable metadata and bounded byte-range reads.
Racer mode has no ordinary registry or direct local-content bypass. Unavailable
sockets, timeouts, invalid metadata, unsupported origin ranges, and stream
preparation failures return a retryable `503` before headers. Registry `401`/`403`
responses preserve authentication challenges; `404` and `429` retain their status,
and SDK-provided retry hints are preserved. Invalid client byte ranges return
`416` with the object size. An interrupted or invalid stream after headers is
aborted rather than spliced together with a second source. Racer's origin adapter
still reads registry/containerd content to fill Racer. There is no bypass flag.
Tag resolution and containerd's own configured host retry chain are separate
from Gantry's digest-serving path. Readiness requires working containerd, origin
UDS, and cache UDS. `P2PCache Ready` describes cache activation, not Gantry origin
availability, so verify Gantry readiness separately.

The operator reports invalid coverage and rolls Gantry back to direct. It
reevaluates selection after cache, Node, and Site changes. It does not provide
atomic fleet-wide cutover or prevent out-of-band workload changes. During rolling
updates direct and Racer agents can coexist temporarily but do not share Gantry
content protocols.
Backend transitions use normal rolling updates; transient retries and loss of
warm cache reuse are expected. Pause large image rollouts until both Racer and
Gantry are ready. Scheduling, socket, and identity overrides that break managed
origin coverage are unsupported.

### Verify and roll back

```bash
kubectl get p2pcache gantry
kubectl -n unbounded-system rollout status deployment/racer-controlplane
kubectl -n unbounded-system rollout status daemonset/racer-dataplane
kubectl -n unbounded-system rollout status daemonset/gantry
kubectl -n unbounded-system get pods -o wide
```

Inspect these signals while exercising a cold image pull and a repeated pull
across multiple nodes. Do not use DHT/chair metrics as Racer readiness evidence.

| Signal | Meaning |
| --- | --- |
| `gantry_racer_available` | Gantry's cache/origin UDS and containerd readiness. |
| `gantry_racer_stream_total{outcome="completed"}` | Full response forwarded; neither OCI verification nor containerd commit is implied. Replaces the former `verified` outcome. |
| `gantry_racer_stream_total{outcome="partial"}` | Range response forwarded successfully. |
| `gantry_racer_stream_total{outcome="aborted"}` | Response forwarding failed. Gantry no longer emits a Racer `digest_mismatch` outcome. |
| `gantry_racer_splice_calls_total`, `gantry_racer_splice_bytes_total` | Actual forwarding splice syscalls and bytes. |
| `gantry_racer_tee_calls_total`, `gantry_racer_tee_bytes_total` | Compatibility counters, expected to remain zero because this path has no verification tee. |
| `gantry_racer_buffered_bytes_total` | Payload forwarded through userspace, including buffered prefixes. |
| `gantry_racer_fallback_total` | Deprecated compatibility counter, exported as zero in Racer mode. An absent series on older deployments may be treated as zero; a nonzero value identifies old fallback behavior. |
| `gantry_origin_stream_completed_total{kind}` | Direct-backend registry response forwarded and digest-checked; does not imply a containerd commit. Racer origin fills do not increment this counter. |
| `gantry_origin_stream_failed_total{kind}` | Direct-backend registry stream failed, including transport and digest failures. |
| `gantry_mirror_bytes_served_total{source="racer"}` | Racer bytes forwarded, including incomplete responses. |
| `gantry_containerd_commit_observed_total` | Completed live-stream digests later observed openable in local containerd. |
| `gantry_containerd_commit_observation_duration_seconds`, `gantry_containerd_commit_latest_observation_duration_seconds` | Time from full response completion to observed openability; measurement resolution is bounded by the storage probe interval. |
| `gantry_containerd_commit_missing_after_stream_total` | No later openability observed within the observation window; not proof of corruption or a digest mismatch. Unavailable-containerd windows pause correlation. |

Update dashboards and alerts from `outcome="verified"` to `outcome="completed"`
and treat completion separately from consumer validation. A missing commit
observation does not trigger automatic cache-wide eviction or a generation bump.

To return operator-managed Gantry to direct distribution, remove the annotation
or set it to `"false"`, then wait for Gantry's rollout:

```bash
kubectl annotate p2pcache gantry unbounded-cloud.io/gantry-backing-
kubectl -n unbounded-system rollout status daemonset/gantry
```

Chair resources are reconciled again. Removing the annotation retains the
P2PCache and Racer slab; delete the unused cache explicitly after Gantry's
rollout. Deleting the selected cache also triggers direct selection. Returning
to direct does not uninstall Racer or migrate or delete containerd's committed
images. To uninstall Racer, remove all P2PCaches and then delete its Deployment
and DaemonSet in either order; see [manual uninstall](../../concepts/racer/#manual-uninstall).

### Recover from known incorrect cached content

Use explicit recovery after establishing a content mismatch:

1. Fix the source so subsequent origin reads return bytes matching the expected
   OCI digest. Stop or finish affected pulls, including pre-bump requests.
2. Read the dedicated P2PCache's current `spec.cacheGeneration` and increase it
   monotonically. For example, this compare-and-swap patch changes **1 to 2**
   only if the stored value is still 1:

   ```bash
   kubectl get p2pcache gantry -o yaml
   kubectl patch p2pcache gantry --type=json -p='[{"op":"test","path":"/spec/cacheGeneration","value":1},{"op":"replace","path":"/spec/cacheGeneration","value":2}]'
   ```

   Substitute the actual cache name, current value, and next value. If the test
   fails, reread the resource instead of overwriting a concurrent update.
3. Wait for `Ready=True` for the new Kubernetes `metadata.generation`, with
   `status.observedGeneration` and the Ready condition's `observedGeneration`
   matching it, and `status.participants.ready == status.participants.desired`
   with a nonzero desired count. Every serving participant must activate the
   resulting configuration. When inspecting dataplane `/status`, require the
   resulting `activeRevision == candidateRevision`, `localState: applied`,
   `ready: true`, no rejection, and all workers activated. Configuration revision,
   Kubernetes resource generation, and `spec.cacheGeneration` are distinct
   values. A patch response or an old Ready condition is not an activation barrier.
4. Inspect containerd in the failed pull's original namespace for a retained
   ingest. After the failed pull has returned and no writer can resume it,
   explicitly abort **only the known failed inactive ingest**, if present, using
   the containerd content API (`ContentStore.Abort(ctx, ref)`). The native test
   acquires the exact failed writer, inspects it, and closes it before aborting;
   keep concurrent retries stopped during this sequence.
5. Retry the same image reference and confirm containerd accepts the expected
   digest. Verify Gantry readiness and subsequent warm reads as well.

The generation bump is cache-wide logical invalidation for that P2PCache across
its selected Sites, not per-object eviction or immediate physical deletion of
old slab data. It does not delete containerd content or ingests, and activation
does not change an already-started stream. Repairing the origin alone can leave
warm bad pages reusable until invalidated or evicted.

In the native recovery campaign, containerd **daemon 2.2.1 / Go client 2.3.5**
retained a complete **1,179,648-byte** failed layer ingest after digest mismatch.
A normal `Client.Pull` retry after generation activation issued **zero additional
layer GETs** and failed on the same retained bytes. After the exact idle ingest
was acquired, closed, and aborted, the same image/ingest reference fetched the
repaired layer and committed successfully. This is tested behavior for that
runtime/client combination, not a guarantee for every containerd version or
consumer. Inspect the actual ingest state rather than assuming either automatic
cleanup or retention. See the
[native test contract](https://github.com/Azure/unbounded/blob/main/e2e/racer/README.md#explicit-generation-recovery-fixture).

### Synthetic image-pull benchmarks

The test-only `racer-loadgen -mode=container-image` workload provides separate
`-role=registry` and `-role=load` processes. The registry creates deterministic
OCI manifests/configs and digest-addressed synthetic layers. The puller discovers
the fixture catalog, then downloads and SHA-256 verifies every image object
through Gantry's mirror. Layer bytes are discarded; this measures simulated
image pulls, not containerd unpack or container startup.

The [loadgen guide](https://github.com/Azure/unbounded/blob/main/cmd/racer-loadgen/README.md#container-image-mode)
includes local commands, cold/warm comparisons, and Prometheus queries. The
[cluster example](https://github.com/Azure/unbounded/blob/main/e2e/racer/examples/container-image-loadgen.yaml)
deploys a fake registry and host-network pullers using Gantry's existing Racer
cache. Check completed Racer streams, zero tee counters, and fallback counters
alongside loadgen throughput and its downstream validation results to confirm
the intended data path.

### Standalone manifests

For a manually managed installation, render with `make gantry-manifests` and set
these fields in the Gantry ConfigMap (or use equivalent standalone flags):

```yaml
content_backend: racer
racer_cache_name: gantry
storage_mode: containerd
```

Standalone Gantry defaults to `content_backend: direct` when omitted. It does not
read the P2PCache backing annotation to select its backend. Deploy Racer and
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

For standalone rollback, set `content_backend: direct` and restore the direct
pod configuration and chair resources before restarting Gantry.
