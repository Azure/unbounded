# Gantry  -  Architecture

**Status:** Draft for team review
**Scope:** High-level design for Gantry cluster-internal container image distribution at 10k+ node scale.

---

## 1. Problem statement

Kubernetes clusters at 10k+ node scale routinely deploy the same container image across many nodes simultaneously. Naive behavior  -  every node pulling from the upstream registry  -  produces a thundering herd at the origin: 10,000 simultaneous TLS handshakes, registry-side rate limiting, link saturation between cluster and registry, and slow rollouts. This is the dominant cost of large-scale rollouts and a known operational pain point.

This document proposes a cluster-local peer-to-peer distribution layer: the origin registry is contacted **at most a small constant number of times per unique content digest** (manifest, config, or layer), after which the content propagates through the cluster between peer nodes. The design is fully decentralized, uses libp2p for content discovery only, and uses rendezvous hashing per digest to coordinate cold-start pulls without leader election or central state.

---

## 2. Goals and non-goals

### Goals

- Minimize origin registry pulls to a small constant per unique content digest per cluster (typically one), regardless of how many nodes need the image.
- Support clusters with 10,000+ nodes.
- Operate without a central coordinator, broker, tracker, or designated control plane node.
- Deploy as a stateless DaemonSet  -  no PVCs, no StatefulSets, no leader election.
- Transparent to workloads: pods don't know the system exists; image references in pod specs are unchanged.
- Resilient to node failures, network partitions, and origin outages (when content is already cached in cluster).

### Non-goals

- Replacing the upstream registry. Origin is still the source of truth and the only place images are authored/published.
- Cross-cluster distribution. This is single-cluster scope.
- Image signing/provenance. The system verifies digests; signature verification is delegated to existing tooling (Cosign, Notary, admission controllers).
- Acceleration of registry pulls when the image is already on the node (the node-local cache already handles that).

---

## 3. Requirements

### Functional

| ID  | Requirement |
|-----|-------------|
| F1  | The system pulls each unique content **digest** (manifest, config, or layer) from the origin registry **at most a small constant number of times per cluster** under normal operation  -  typically exactly once, bounded by ≤3 during transient informer-convergence windows (§5.3). Scope is digest-keyed requests only; tag-keyed cold-start is outside F1 (§5.1a, §6). |
| F2  | Image pulls by `kubelet` / `containerd` are served transparently  -  no changes to pod specs or workload configuration. |
| F3  | The agent runs as a Kubernetes DaemonSet. No PVCs, no per-node persistent identity managed by Kubernetes. |
| F4  | Peer discovery uses libp2p (Kademlia DHT). No central tracker or registry-side coordination is required. |
| F5  | Cold-start coordination (no peer has the digest yet) uses rendezvous hashing (HRW) **per digest** to deterministically select a designated puller. Each digest is coordinated independently. |
| F6  | When a digest is not yet cached anywhere, exactly one node pulls it from origin in the common case; redundant pulls occur only under failure (e.g., partition). |
| F7  | Content received from peers is verified against OCI digests before being served to `containerd`. |
| F8  | The system supports any OCI-compliant upstream registry. |
| F9  | Tag references (`manifests/<tag>`) resolve directly at origin via containerd's `hosts.toml` mirror-fallback chain. The agent maintains no tag->digest cache, no tag-keyed DHT advertisements, and no agent-layer tag freshness logic; only digest-keyed requests are routed through the P2P layer. |

### Non-functional

| ID   | Requirement |
|------|-------------|
| NF1  | Designed for clusters of 10,000+ nodes. |
| NF2  | Cold-start convergence time (time from first request to image available on all requesting nodes) should be bounded by image size and inter-node bandwidth, not by origin bandwidth. |
| NF3  | Recovery from designated-puller failure should not require coordination with a control plane. |
| NF4  | During network partition, the system should prefer liveness (both partitions make progress) over strict pull-once (one extra origin pull is acceptable). |
| NF5  | The system must not introduce hard dependencies that prevent pulls if libp2p discovery is degraded. The agent's HRW top-K probe (§5.2) is the authoritative discovery mechanism on DHT miss; direct origin fallback fires only when both DHT and HRW probe fail and is jittered + rate-limited (§7.7). |
| NF6  | Per-node resource footprint should be modest: <100 MB RAM, <5% CPU under steady state. |

---

## 4. Architecture

### 4.1 Components

The system consists of a single component  -  the **P2P agent**  -  running as a DaemonSet pod on every node. The agent has four responsibilities:

1. **Containerd registry mirror.** Listens on `127.0.0.1:5000` and serves the OCI distribution API. `containerd` is configured via `hosts.toml` to use the local agent as a mirror with `capabilities = ["pull", "resolve"]` for configured upstream registries.

2. **libp2p DHT participant.** Joins a cluster-scoped Kademlia DHT and announces locally-held content as provider records keyed by OCI digest.

3. **Local content-store integration.** Reads and writes flow through the local containerd content store (namespace `k8s.io` by default) via `internal/containerdstore`. There is no separate Gantry-owned cache. Background ingests are protected with bounded containerd leases; see §7.4.

4. **HRW coordinator.** Maintains a view of cluster membership (via Kubernetes informer) and computes rendezvous hashes locally to select designated pullers for cold-start scenarios.

### 4.2 Architecture overview

```
                          ┌───────────────────┐
                          │  Origin registry  │   (external, OCI-compliant)
                          └─┬──────┬──────┬───┘
                            │ D1   │ D2   │ D3
                            │      │      │   each unique digest pulled
                            │      │      │   at most a small constant
                            │      │      │   number of times cluster-wide
        ┌───────────────────┼──────┼──────┼──────────────────────────────┐
        │ Kubernetes        ▼      ▼      ▼                              │
        │ cluster      ┌────────┐┌────────┐┌────────┐                    │
        │              │ Node A ││ Node B ││ Node C │  ...               │
        │              │ puller ││ puller ││ puller │                    │
        │              │ of D1  ││ of D2  ││ of D3  │                    │
        │              └───┬────┘└───┬────┘└───┬────┘                    │
        │              D1  │     D2  │     D3  │                         │
        │                  ▼         ▼         ▼                         │
        │      ┌──────────────────────────────────────────────┐          │
        │      │ Remaining N-3 nodes fetch D1, D2, D3 from    │          │
        │      │ their respective pullers via libp2p          │          │
        │      │ discovery + HTTP/2 transfer on :5001         │          │
        │      └──────────────────────────────────────────────┘          │
        │                                                                │
        │ Per-digest HRW: an image's manifest, config, and layer         │
        │ digests generally HRW to *different* nodes, so cold-start      │
        │ origin contact spreads across the cluster instead of           │
        │ concentrating on one node. No traffic flows between the        │
        │ pullers themselves; each independently fans out its digest     │
        │ to the rest of the cluster.                                    │
        └────────────────────────────────────────────────────────────────┘
```

Per-node layout:

```
┌────────────────────── Node ───────────────────────┐
│                                                    │
│   kubelet -> containerd ──► hosts.toml mirror       │
│                              ▼                     │
│                      127.0.0.1:5000                │
│                              ▼                     │
│              ┌──────── P2P agent ────────┐         │
│              │  - Registry mirror server │         │
│              │  - libp2p host + DHT      │         │
│              │  - HTTP transfer server   │         │
│              │  - HRW + K8s informer     │         │
│              └────────────┬──────────────┘         │
│                           ▼                        │
│           containerd content store (k8s.io ns)     │
│           + hostPath: /var/lib/gantry/libp2p       │
│                         (identity key only)        │
│                                                    │
└────────────────────────────────────────────────────┘
```

### 4.3 Key design decisions

**libp2p for discovery only, not transfer.** Discovery (find peers with content X) is what Kademlia is good at. Bulk content transfer rides on plain HTTP/2 against a dedicated peer-facing port the DaemonSet exposes (see §4.4). This lets transfer benefit from kernel-level TCP optimizations, standard HTTP tooling for debugging, and a clean substrate for future range-based multi-peer striping. v1 ships single-peer fetch (the requester picks one provider and streams the whole blob); range-based parallel striping across multiple peers is supported by the endpoint contract but deferred to v2  -  see §5.1.

**Per-digest granularity, end to end.** Provider records, HRW computation, and cold-start coordination all operate on individual OCI digests, not on "images." An image is a manifest digest plus a config digest plus N layer digests; the agent treats these as N+2 independent units. Two images sharing a base layer share providers automatically. Cold-start for a fully-uncached image fans out to up to N+2 different designated pullers in parallel  -  one per digest  -  which maximizes ingress parallelism and spreads load across the cluster (popular base layers naturally HRW to different nodes per image-that-uses-them, rather than concentrating on a single "image owner"). Per-chunk granularity (for pipelined distribution of large layers) is deferred until empirical data justifies the additional complexity.

**Tradeoff: per-digest HRW costs N+2 separate origin TLS handshakes.** For a fully-cold N-layer image, origin sees up to N+2 TLS handshakes from up to N+2 distinct source IPs  -  not 1 handshake reused across N+2 keepalive'd requests as the single-puller-per-image alternative would produce. The puller-side TLS connection reuse described in §5.2a only helps when batched `please_pull` lands multiple digests on the same puller (the exception under uniformly-distributed HRW, not the rule). This is a deliberate tradeoff: image-pull rollouts are bursty events, and a registry that pushes back on N+2 simultaneous handshakes from distinct cluster nodes would push back harder on the thundering-herd alternative the design exists to prevent. Revisit only if real-world registries report per-handshake (rather than per-byte or per-request) cost as the binding constraint.

**Containerd content store as single source of truth.** Gantry reads and writes exclusively through the node's containerd content store in the configured namespace (normally `k8s.io`). No separate Gantry-owned cache exists. Local reads, peer transfer, and background `please_pull` ingest all go through containerd. DHT provider records are advertisements of content the node's containerd can actually open. Gantry-created background content is protected with bounded containerd leases until kubelet/containerd creates its own Image reference or the lease TTL expires; see §7.4.

**Tags resolve directly at origin; Gantry routes only digests.** Tag references (`registry/repo:tag`) are not content-addressable, and v1 deliberately does not route them through the P2P layer. When containerd asks the local agent for `manifests/<tag>`, the agent returns `5xx` immediately, and containerd's `hosts.toml` mirror-fallback chain reaches origin directly using that node's own credentials. Origin performs the tag->digest resolution; containerd records the binding in its own image table; the agent observes the resulting digests via image events (§7.3) and routes them through the normal digest-keyed P2P path (§5.1 / §5.2, F1-bounded). The agent maintains **no tag->digest cache, no tag-keyed DHT advertisements, and no tag-freshness logic**. This preserves OCI's "tag is a pointer at origin, resolved on every pull" semantic exactly: every cluster member reaches origin under its own credentials for tag resolution and gets containerd-authoritative bindings, rather than depending on any agent-layer tag cache that would need a TTL/refresh discipline to stay consistent with origin. The cost is per-node origin contact for the tag-resolution step (small  -  manifest body only, typically a few KB; layer and config bytes remain F1-bounded via the digest-keyed path) and loss of tag-pull availability under origin partition. §8 documents an HRW-coordinated TTL-refresh mechanism as a v2 candidate for operators who need bounded-staleness peer-served tag resolution.

**Stateless agent (from the orchestration perspective).** The pod itself has no Kubernetes-managed state  -  no PVCs, no per-node identity managed by Kubernetes. Content lives in the node's containerd content store (§7.4); the only Gantry-owned on-disk state is the optional libp2p identity key persisted to `hostPath` (§7.2). The agent does maintain in-memory state  -  in-flight pull map, recent-failures negative cache (§5.8), DHT health rolling stats (§7.7), informer cache  -  but all of it is reconstructible from containerd and the Kubernetes informer on restart. On pod restart, the agent rebuilds its peer view from the Kubernetes informer and re-announces containerd-resident content to the DHT.

**Direct RPC for cold-start coordination, with HRW top-K probe as authoritative discovery on DHT miss.** When DHT lookup returns no providers, the agent does **not** treat that as ground truth. `dht.FindProviders` returning empty is ambiguous  -  it may mean genuinely no provider, slow/timed-out lookup, sparse local routing table, or expired provider records under load. The agent therefore treats the DHT as a fast-path optimization and uses HRW top-K probe as the authoritative discovery mechanism on DHT miss: it dials the K HRW-ranked nodes for the digest with an enriched query that asks both "do you have it cached?" and "are you already pulling it?". Only if all reachable top-K answer "neither" is the request treated as a true cold-start. HRW computation is local and deterministic and does not depend on DHT health, so this routes around DHT degradation entirely. No PubSub layer. No heartbeat protocol. Receiver-side dedupe handles concurrent requests.

**Topology-aware (optional).** HRW can be scoped per availability zone for clusters where cross-zone bandwidth is the bottleneck. This produces one designated puller per zone instead of one per cluster.

### 4.4 Wire protocols

The agent exposes two distinct wire surfaces. They are versioned independently.

**Coordination RPCs (libp2p stream).** `pull_intent_query` and `please_pull` are carried as length-prefixed protobuf messages over libp2p streams. Single protocol ID `/gantry/coord/1.0.0`; one stream per request/response pair, closed after the response is written. Schema lives in `proto/gantry/coord/v1/coord.proto`. Forward-compat policy: additive changes bump the minor (`/gantry/coord/1.1.0`), breaking changes bump the major (`/gantry/coord/2.0.0`). gRPC is intentionally avoided  -  `go-msgio` length-delimited framing is sufficient and avoids HTTP-over-libp2p complexity. The message envelope uses `oneof` so a single protocol handler dispatches both RPC kinds:

```proto
syntax = "proto3";
package gantry.coord.v1;
import "google/protobuf/timestamp.proto";

enum FailureClass {
  FAILURE_CLASS_UNSPECIFIED  = 0;
  FAILURE_CLASS_AUTH         = 1;
  FAILURE_CLASS_NOT_FOUND    = 2;
  FAILURE_CLASS_RATE_LIMITED = 3;
  FAILURE_CLASS_TRANSIENT    = 4;
}

message Envelope {
  oneof msg {
    PullIntentRequest   pull_intent_request   = 1;
    PullIntentResponse  pull_intent_response  = 2;
    PleasePullRequest   please_pull_request   = 3;
    PleasePullResponse  please_pull_response  = 4;
  }
}

message PullIntentRequest  { string digest = 1; }

message PullIntentResponse {
  bool has_cached                          = 1;
  bool in_flight                           = 2;
  google.protobuf.Timestamp started_at     = 3;
  int32 hrw_rank                           = 4; // recipient's own rank in recipient's view;
                                                // requester compares against its own rank
                                                // computation to detect informer divergence
                                                // (§5.3) and emit `p2p_hrw_rank_mismatch_total`
  bool recently_failed                     = 5;
  google.protobuf.Timestamp cooldown_until = 6;
  FailureClass failure_class               = 7;
}

message PleasePullRequest {
  repeated string digests   = 1;            // batched (§5.2a); all digests in a single batch
                                            // MUST share `upstream_registry` and `repository`.
                                            // Cross-repo digests (e.g., a layer reachable via
                                            // OCI cross-repo blob mount) require separate calls.
  string upstream_registry  = 2;            // e.g. "registry.example.com"
  string repository         = 3;            // e.g. "library/nginx"
}

message PleasePullResponse {
  message Result {
    enum Outcome {
      OUTCOME_UNSPECIFIED     = 0;
      OUTCOME_ALREADY_PULLING = 1;
      OUTCOME_STARTED         = 2;
      OUTCOME_RECENTLY_FAILED = 3;
      reserved 4;                   // OUTCOME_DECLINED: held for per-source
                                    // `please_pull` rate-limiting (§8 open question)
    }
    string digest                            = 1;
    Outcome outcome                          = 2;
    google.protobuf.Timestamp started_at     = 3;
    google.protobuf.Timestamp cooldown_until = 4;
    FailureClass failure_class               = 5;
  }
  repeated Result results = 1;
}
```

**Transfer endpoint (HTTP).** Each agent binds an HTTP/2 server on `0.0.0.0:5001` (peer-facing; the containerd mirror endpoint on `127.0.0.1:5000` is separate and loopback-only). NetworkPolicy restricts `:5001` to inter-node traffic. The endpoint mirrors the OCI Distribution API so peer-side code reuses the same registry-client codepath:

- `GET /v2/` -> `200` (capability probe).
- `GET /v2/<repo>/blobs/sha256:<hex>` -> blob bytes; supports `Range: bytes=N-M` with `206 Partial Content` + `Content-Range`. Range support is required at the protocol level even though v1 clients do not use it (it unblocks v2 striping without a protocol change).
- `GET /v2/<repo>/manifests/sha256:<hex>` -> manifest bytes by digest.
- `GET /v2/<repo>/manifests/<tag>` -> tag-keyed; returns `404` unconditionally on this peer-facing endpoint. Peers never request tags in v1 (the DHT carries no tag keys; see §4.3 and §5.1a), so a tag-shaped request arriving on `:5001` indicates a misconfigured peer and `404` is the appropriate response. **This differs from the containerd mirror endpoint on `127.0.0.1:5000`**, which returns `503` for tag-shaped requests so that containerd's `hosts.toml` mirror-fallback promotes the request to the next host in the chain (§5.1a, §7.1). Different endpoints, different audiences, different status codes  -  `:5001` is peer-to-peer where a tag request is a bug, `:5000` is the containerd mirror where a tag request is the design-intended path that must trigger fallback.
- `GET /healthz` -> `200` for Kubernetes liveness/readiness probes.

**`Gantry-Mirrored: 1` header.** The requesting agent sets `Gantry-Mirrored: 1` on every fetch to a peer's `:5001`. Its presence switches the serving peer's handler into peer-fetch mode:

- Serve **only** from local store. Return `404` on miss. Never trigger DHT lookup, HRW probe, `please_pull`, or origin contact in response to a peer fetch.
- Tag-shaped manifest requests: `404` unconditionally (peers do not request tags in v1; the DHT carries no tag keys).
- Increment `p2p_peer_serve_total` rather than `p2p_cache_hit_total` (which counts workload-side hits via the mirror endpoint).

The header is a behavior switch, not an authorization mechanism. Trust comes from NetworkPolicy scoping `:5001` to inter-node traffic. Any peer reachable on that port is treated as trusted; see the `please_pull` abuse open question in §8.

---

## 5. Protocol flows

Manifest requests come in two forms: by digest (`manifests/sha256:...`) and by tag (`manifests/<tag>`). Tag requests are handled separately (§5.1a) because they are not content-addressable. All other requests  -  manifests by digest, config blobs, and layer blobs  -  go through the digest-keyed warm path (§5.1) or cold-start path (§5.2).

### 5.1 Warm path  -  digest-keyed content exists in cluster

1. `containerd` requests `127.0.0.1:5000/v2/<image>/manifests/sha256:abc` (or a blob by digest).
2. Agent checks the local containerd content store. **Miss.**
3. Agent calls `dht.FindProviders(CID)` where CID derives from the OCI digest. **Hit**  -  returns peer multiaddrs.
4. Agent picks the first reachable provider (ordered roughly by Kademlia proximity) and issues a single HTTP `GET` against that peer's `:5001` transfer endpoint (§4.4) with `Gantry-Mirrored: 1`. No `Range` header in v1.
5. Bytes stream through the agent to the requesting containerd. Gantry hashes the stream and records any mismatch, but containerd remains the final commit verifier because bytes have already been delivered by the time the final digest check completes.
6. Gantry does **not** independently ingest the live mirror response into containerd. The requesting containerd commits the content; cdsub/advertiser reconciliation observes the later openable content and issues `DHT.Provide`.

**v1 transfer policy (single-peer).** v1 ships single-peer fetch: one provider per blob, whole-blob GET. If the stream stalls (no bytes received for 10 s) or errors, the agent cancels and reissues against the next provider returned by `FindProviders`. After 3 providers fail in succession the agent gives up on the warm path and returns `5xx` to containerd, which falls through the mirror chain to origin (§7.1). Single-peer fetch is the simplest correct implementation and makes the v1 surface easy to reason about; the transfer-endpoint contract (§4.4) preserves `Range` support so v2 can add striping without a protocol change.

**v2 sketch (deferred).** Each blob is split into fixed-size chunks (proposed 4 MiB). Up to 4 providers issue `Range` requests in parallel; a chunk whose progress falls below 0.5× the median chunk-throughput is canceled and reissued against a different provider. Triggered for evaluation only if v1 cold-start convergence times for large layers (>1 GB) prove unacceptable in practice. See §8 open question on chunk-level granularity.

### 5.1a Tag reference path

Containerd resolves `image:tag` to a digest by fetching `manifests/<tag>` before any blob requests. This is the first request on every cold pull. **Gantry v1 does not handle tag resolution**: tag-shaped requests are short-circuited back to containerd's `hosts.toml` mirror chain, which falls through to origin. This keeps tag->digest resolution exactly where OCI puts it (at origin, on every pull, under each node's own credentials) and avoids any agent-layer tag->digest cache, tag advertisement in the DHT, or rebinding-coherence mechanism. The flow:

1. `containerd` requests `127.0.0.1:5000/v2/<repo>/manifests/<tag>?ns=<registry>`. The `ns=` query parameter is supplied by containerd's `hosts.toml` mirror configuration (see §7.1) and is required  -  without it, a bare tag reference cannot be disambiguated to an upstream registry.
2. Agent recognizes a tag-shaped manifest request and returns `503 Service Unavailable` immediately. No DHT lookup, no local-store consultation, no log at WARN level (this is the design-intended path). The response body identifies the case for diagnostics. `503` is chosen because containerd's `Resolve` loop in `core/remotes/docker/resolver.go` falls through to the next host in the `hosts.toml` chain on any response status `> 299` (with `404` special-cased but still falling through; `> 399` falls through with the error recorded for later return if all hosts fail). Verified across the `release/1.6`, `release/1.7`, and `main` (2.x) branches: any 5xx, including `503`, causes containerd to advance to the next host. The in-host retry path (`retryRequest`) differs across branches  -  1.x retries only on `408` and `429`, while 2.x additionally retries on `500`/`503`/`504`  -  but the 2.x extension is gated on `lastHost`, so when the local agent is the first host in the chain (which it always is in this design), the in-host retry never fires and mirror-fallback happens immediately. `503` is also semantically apt: the agent at this endpoint is *not* the authority for tag resolution and is genuinely unavailable for the request, which `503 Service Unavailable` describes more accurately than alternatives like `502 Bad Gateway` (no upstream was attempted) or `500 Internal Server Error` (nothing failed internally). If a future containerd version narrows the cross-host fallback condition to exclude `503`, substitute another in-set 5xx; the choice is not load-bearing beyond “triggers mirror-fallback.”
3. Containerd's `hosts.toml` mirror chain (§7.1) promotes the request to the next host. The terminal entry is origin, which performs the tag->digest resolution and returns the manifest body. Containerd records the tag->digest binding in its own image table; the agent maintains no tag-binding state.
4. The pull then proceeds digest-keyed: containerd requests the config and each layer (and, for multi-arch images, the per-arch manifest) by digest from `127.0.0.1:5000`. Those requests flow through §5.1 / §5.2 and are F1-bounded. The agent's containerd image-event subscription (§7.3) picks up the new digests as they land in containerd's content store and advertises them in the DHT.

**Origin contact bound for tag-keyed pulls.** Every node hits origin once per pod-start for the tag-resolution step. The cost per contact is small: a single `GET /v2/<repo>/manifests/<tag>` returning the manifest body (typically 2–50 KB; ~10 KB for a multi-arch manifest list) plus one TLS handshake. **Layer bytes do not come from origin** for any node beyond the F1-bounded designated puller: the per-node manifest fetch yields a digest, the digest-keyed config and layer pulls go through Gantry, and F1 applies. For a 1000-node rollout of a 2 GB image, origin sees ~10 MB of manifest-resolution traffic plus 1 × 2 GB of layer egress (F1-bounded), not 1000 × 2 GB.

**Steady-state aggregate origin load.** Manifest-resolution traffic scales as `tag-keyed-pod-starts-per-unit-time × manifest-body-size`, with no Gantry-side aggregation, and **the cluster-wide rate does not decrease with cluster size**  -  adding nodes adds more pod-starts, each of which contacts origin independently for the resolution step. Two reference points to help operators place their workloads:

- *Stable workload (typical).* 10,000 pods restarting once per day produces ~10,000 manifest fetches/day ≈ ~415 fetches/hour ≈ ~4 MB/hour at ~10 KB per manifest. A trickle; origin barely notices.
- *High-churn workload.* 10,000 pods restarting once per hour produces ~10,000 fetches/hour (~100 MB/hour); restarting every 10 minutes produces ~60,000 fetches/hour (~600 MB/hour). Meaningful tag-resolution traffic; size origin capacity accordingly, or pin digests in pod specs (which avoids the tag-resolution path entirely), or wait for §8's v2 HRW-with-TTL mechanism, which is the candidate for bounding this aggregate without operator action.

The boundary between these regimes is where the operational v1/v2 tradeoff bites. Stable workloads can run v1 indefinitely with no concern; high-churn workloads should evaluate the v2 mechanism against their tolerance for TTL-bounded staleness.

**Tag-herd throttling deferred to origin.** Each node's tag-resolution retry rate is throttled by kubelet's `ImagePullBackOff` on failure. Origin's own rate-limiting / per-IP limits apply to repeated tag queries from the cluster. The agent does not add a tag-keyed negative cache or coordination layer; §5.8's negative cache applies to digest-keyed pulls only, where it bounds the cluster-wide retry burst on origin auth / not-found / rate-limited responses for content the agent is pulling.

**Availability under origin partition.** A consequence of v1's deferral: when origin is unreachable, tag-keyed pulls fail across the cluster, even on nodes whose containerd already has the tag binding cached locally (kubelet may still re-resolve depending on `imagePullPolicy`). Digest-keyed pulls  -  running pods, pre-pinned digests in pod specs, image-restart of already-cached images  -  continue to succeed peer-to-peer.

**Operator guidance for v1.** Three options for surviving origin partition on tag-keyed workloads, in order of preference:

1. *Pin digests in pod specs* (`image: registry/repo@sha256:...`). The strongest guarantee  -  no tag-resolution path is taken at all, and pulls succeed peer-to-peer for any digest already in the cluster cache. Recommended for critical workloads.
2. *Set `imagePullPolicy: IfNotPresent`* on workloads referencing tags. When the image is already present on the node, kubelet skips re-resolution and the pod starts from the local content store without contacting origin. **This is already kubelet's default for any tag reference other than `:latest`**, so most workloads need no operator action; the explicit-configuration recommendation applies specifically to workloads using `:latest` or otherwise setting `imagePullPolicy: Always`, which require origin reachability on every pod-start for the tag-resolution step  -  v1's behavior by design, not a regression. This option protects pod *restarts* on already-cached nodes but does not help fresh schedules onto nodes that have never pulled the image.
3. *Wait for the §8 v2 HRW-with-TTL mechanism*, which restores peer-served tag resolution within a bounded staleness window.

### 5.2 Cold-start path  -  no provider exists

The interesting case. Walked through in detail because every failure mode in §6 references this flow. **Applies to digest-keyed requests only**  -  manifests by digest, config blobs, and layer blobs. Tag references never enter this path; they are handled by §5.1a.

**HRW is per digest, not per image.** Each digest is coordinated independently. The flow below describes the cold-start for a single digest. Containerd requests digests in dependency order (manifest, then config, then layers in parallel), so the agent will run this flow N+2 times for a fully-cold image  -  possibly concurrently for the blob digests once the manifest has been parsed (see §5.2a).

**DHT-empty is not ground truth.** A `FindProviders` returning empty can mean (a) genuinely no provider, (b) the lookup was slow or timed out, (c) the local routing table is sparse (e.g., during bootstrap or after partition heal), or (d) provider records expired under DHT load before refresh. The agent does not distinguish these cases at the DHT layer; instead, it treats DHT as a fast-path optimization and uses HRW top-K probe as the authoritative cold-start arbiter (steps 3–6 below). HRW is local-only, deterministic, and unaffected by DHT health, so this routes around DHT degradation by construction.

1. `containerd` requests a manifest or blob by digest. Agent has cache miss.
2. Agent calls `dht.FindProviders(CID)`. **Hit:** proceed to §5.1 warm path. **Empty (or below quorum):** continue  -  do not infer cold-start yet.
3. Agent computes HRW: `score(node, digest) = SHA256(node_id || digest)` for every node in the local Kubernetes informer cache. Selects the top-K (K=3 by default) by score using a partial sort / heap (not a full sort over all nodes). The top-K are the designated pullers for **this digest** in priority order. Different digests of the same image will generally HRW to different top-K sets  -  that is the desired behavior.
4. Agent dials all K in parallel with `pull_intent_query(digest)`. The response carries authoritative state for that node, with field names and types matching the `PullIntentResponse` proto in §4.4: `has_cached: bool`, `in_flight: bool`, `started_at: Timestamp`, `hrw_rank: int32`, `recently_failed: bool`, `cooldown_until: Timestamp`, `failure_class: FailureClass` (enum, not string  -  see §4.4). The first four fields drive normal discovery; the last three carry origin-failure circuit-breaker state from §5.8 and are meaningful only when `recently_failed` is true. This RPC is the discovery mechanism; the DHT result was advisory.
5. Agent collects responses with a 2-second timeout. Constructs the set of **reachable top-K nodes**, then evaluates the response set against the following rules **in priority order** (first matching rule wins; do not fall through):
   1. **Cluster-wide failure short-circuit.** If **any** reachable node reports `recently_failed` with `failure_class` in `{auth, not_found, rate_limited}`, return 5xx to containerd immediately. These classes are cluster-wide-trusted per §5.8: asking rank-1 or polling an in-flight pull on rank-0 is futile because rank-0's pull will fail identically (`auth` and `not_found` are deterministic; `rate_limited` requires backoff regardless). This must precede the `has_cached` / `in_flight` rules below because an `in_flight` pull from a node that *also* reports a recent failure in the same response is racing toward the same `401` / `404` / `429`. Note: a node that has `has_cached` for the digest cannot simultaneously report `recently_failed` for it (the cache entry was produced by a successful pull, which clears `recent_failures[digest]` per §5.8); the priority is only relevant across *different* reachable nodes.
   2. **Cache hit.** If any reachable node reports `has_cached`, fetch from that node via the warm-path transfer (§5.1 from step 4 onward). Do not invoke `please_pull`. The DHT lookup was a false-empty; the digest exists in cluster.
  3. **In-flight piggyback.** If any reachable node reports `in_flight` with a fresh `started_at` (`now - started_at` is within the per-digest timeout from §5.2a  -  5 s for manifest/config; `expected_pull_seconds × 3` for layers), **poll the local DHT** for providers of the CID at the per-digest interval (200 ms manifest/config, 1 s layer; see §5.2a). The digest is being fetched; piggyback on the puller's eventual advertiser-owned `DHT.Provide`. If `now - started_at` exceeds the per-digest timeout, treat the pull as stalled (§5.6) and exclude the reporter from `please_pull` candidates.
   4. **Transient cooldown.** If any reachable node reports `recently_failed` with `failure_class = transient`, apply the local **honor window** of `min(cooldown_until - now, 30 s)` before retrying. Do not proceed to step 6 within the honor window.
   5. **All-unreachable expansion.** If no top-K node responds within the timeout, expand the probe to top-2K and re-run step 4 against the new candidates. This rule applies regardless of DHT health score: a fully-unreachable top-K is itself a partition-or-failure symptom, and very likely a provider exists at rank-K+1..rank-2K. NF5 (§7.7) fires only if the expanded probe also fails.
   6. **Degraded-health eager expansion.** If DHT health is Degraded (§7.7) and all reachable top-K report `neither cached nor in-flight`, expand to top-2K before declaring cold-start. Under healthy DHT the honest "neither" answer is trusted; under degraded DHT it may be wrong (the top-K node may have evicted because `dht.Provide` is failing cluster-wide).
   7. **Cold-start.** Only if **all reachable top-K** report `has_cached: false`, `in_flight: false`, and `recently_failed: false`, **and** none of the expansion rules above apply, proceed to step 6. This is the only condition that justifies a true cold-start.
6. Agent selects the **lowest-ranked reachable node** and sends it `please_pull(digest)`.
7. The chosen designated puller checks its in-flight map for the digest:
   - If already pulling, responds `already_pulling(started_at)`. No-op.
   - If not, starts pulling from origin, adds to in-flight map.
8. The requesting agent polls the **local DHT** for providers of the CID at the per-digest interval defined in §5.2a, bounded by the per-digest timeout. Requesters do **not** poll the puller's `:5001` directly  -  see §5.2a for the rationale.
9. When the puller completes origin fetch, commit, reopen, and advertiser mark-present, the advertiser calls `DHT.Provide(CID)` and the puller removes the digest from its in-flight map.
10. All polling requesters discover the provider and pull via the warm path (§5.1).

**Why this matters under DHT pathology.** With DHT degraded (false-empty rate non-trivial), the previous design would conclude cold-start on every false-empty and either (a) pile up redundant `please_pull` calls or (b) eventually trip NF5 fallback and produce a cluster-wide thundering herd against origin. Under the revised flow, a false DHT-empty is caught at step 5: the top-K node almost always has the digest cached (rank-0 was the original puller and remains a provider for the lifetime of its cache entry), and the request is served peer-to-peer with no origin contact. DHT degradation now produces only a small RPC-overhead penalty, not an origin storm.

**Cost.** K extra dials on every DHT miss. With K=3 and dials in parallel, this is bounded by the 2-second `pull_intent_query` timeout and adds no serialized latency.

**Residual hole.** A digest cached only on nodes *not* currently in the top-K (e.g., a node was rank-2 yesterday but is now rank-47 because the cluster grew) is invisible to this probe. Two safeguards: (i) the historical-puller continuity is implicit  -  a node remains a DHT provider for a digest as long as containerd retains the content, and Gantry adds no replication-aware eviction on top of containerd's own GC (see §7.4), so original pullers tend to remain providers for the lifetime of their containerd entry; (ii) the top-2K expansion rules in step 5  -  unconditionally on all-unreachable, eagerly under Degraded DHT health  -  widen the probe before declaring cold-start (see §7.7).

### 5.2a Per-digest cold-start in practice

Containerd's pull sequence is fixed: manifest -> config -> layers (in parallel). The agent runs §5.2 once per digest as containerd asks for it, with these specifics per digest type:

- **Manifest digest.** Cold-start with a **fixed short timeout** (default 5 s) and a **local-DHT polling interval of 200 ms** while waiting for the puller's advertiser-owned `DHT.Provide` to land. Manifests are kB-scale; if the puller hasn't produced one in 5 s, treat it as stalled and run §5.6's takeover. The fixed timeout avoids a chicken-and-egg with size-aware timeouts (the size information lives in the manifest itself). "Manifest" here includes OCI manifest lists / Docker schema-2 manifest lists: the agent treats each as a separate digest, and platform selection (picking the platform-specific manifest digest after the list is fetched) happens entirely inside containerd  -  the agent sees the list digest and the platform manifest digest as two independent §5.2 runs.
- **Config digest.** Same as manifest  -  kB-scale, fixed short timeout, 200 ms local-DHT polling interval.
- **Layer digests.** Once the manifest has been received and parsed, the agent knows every layer's size. Cold-start for layer digests uses a size-aware timeout derived from layer size and a configured floor bandwidth assumption (default `expected_pull_seconds × 3`, with `expected_pull_seconds = max(10, layer_bytes / 50 MB/s)`). The local-DHT polling interval is 1 s for layers (lower frequency than manifest/config: layers take longer; the polling cost is amortized). Layer cold-starts run **in parallel** for all layers that miss the DHT  -  they do not serialize.

**Polling targets the local DHT, not the puller's transfer endpoint.** Each requester polls its own `dht.FindProviders(CID)` at the interval above. The puller publishes through the advertiser on completion; that provider record propagates through the DHT and surfaces in requesters' local lookups within seconds. Requesters do **not** poll the puller's `:5001` directly  -  a 200 ms-interval poll across 10,000 requesters during a thundering-herd cold-start would generate ~50,000 inbound HTTP RPS at the puller, on top of its actual transfer load. The local-DHT polling cost is bounded by libp2p's per-node lookup caching and does not concentrate on any one node.

**Batched `please_pull` for layers.** Once layer digests are known, the agent fans out per-digest `please_pull` calls in parallel. As an optimization, when multiple cold-start layers all HRW to the same designated puller (which happens often when K is small relative to the cluster), the agent may send a single `please_pull([digest1, digest2, …])` carrying all such digests. The receiver's per-digest in-flight dedupe is unchanged  -  batching is purely a wire-level RPC reduction.

**Origin connection reuse on the puller side.** A designated puller that receives multiple `please_pull` requests for digests of the same image (manifest + config + layers, or several layers from a batched call) SHOULD reuse a single TLS connection to origin. This is a local implementation concern, not a protocol-visible behavior, but it materially reduces TLS handshake cost and cooperates with origin keepalive.

**Origin pulls per fully-cold image.** Up to N+2 origin pulls (manifest + config + N layers), each from a *potentially different* designated puller. This is the F1 invariant: one origin pull per unique digest, not one per image. For an image with 10 layers, expect ~12 distinct origin connections cluster-wide on first pull, generally distributed across ~12 different nodes. This is bounded, parallel, and load-balanced by construction; no node becomes a bottleneck.

### 5.3 Concurrent cold-start requests (thundering herd)

The exact scenario the design must handle: 10,000 nodes all want image Y in the same second.

- All 10,000 independently run §5.2 steps 1–3 **per digest**. Because HRW is deterministic and inputs (node list + digest) are identical across the cluster, **all 10,000 arrive at the same top-K for any given digest**. Different digests of the image generally produce different top-K sets, spreading the inbound RPC load across many nodes rather than concentrating on three.
- For each digest, all 10,000 dial that digest's top-3 with `pull_intent_query`. Across an image with N+2 digests, this is ~3(N+2) distinct nodes receiving RPCs from the cluster, each receiving ~10,000 inbound queries for its assigned digests. Manageable (single-RPC, no state writes), but consider rate-limiting if hot-spotting on a particular node becomes an issue.
- For each digest, all 10,000 select the same designated puller and send `please_pull`. The puller's per-digest in-flight dedupe handles this: first request starts the pull, the other 9,999 get `already_pulling` immediately.
- All 10,000 poll the DHT per digest. As each digest's puller finishes and calls `Provide`, the warm path activates per digest and the swarm distributes that digest P2P-style.

**Origin sees one pull per unique digest.** For an image with N+2 unique digests, that is N+2 origin pulls cluster-wide  -  not N+2 per node. This is the property the system exists to provide.

**Caveat  -  informer divergence (accepted limitation).** This claim assumes every agent has the same node list at the same instant. In reality, Kubernetes informer caches lag during membership changes (rolling node addition/eviction); the convergence window is **expected** to be <5 s in typical environments based on Kubernetes informer behavior, but this should be validated empirically at scale. When agents disagree on the node set, their HRW rankings differ and multiple designated pullers may be selected concurrently for the same digest. Receiver-side dedupe bounds the damage at each puller, but cross-puller dedupe does not exist  -  so the property degrades from "one origin pull" to "a small number, bounded by the number of distinct rank-0 selections across divergent views." The **anticipated** bound during a rolling-update window is ≤3 origin pulls per affected digest (hypothesis pending scale validation; the requester compares its own HRW computation to each `PullIntentResponse.hrw_rank` reported by the recipient and emits `p2p_hrw_rank_mismatch_total` when they disagree, giving operators a direct view of the actual divergence rate). The design accepts this as a known limitation rather than introducing a synchronized membership protocol (which would be exactly the kind of side-channel coordinator the design forbids  -  see §2 non-goals). F1 reflects this with the "small constant" wording.

### 5.4 Designated puller has no local demand

The case where rank-0's HRW says "you pull this digest," but no pod on rank-0 is asking for the digest.

This is handled automatically by §5.2 step 6: the requesting node sends an explicit `please_pull` RPC to rank-0. Rank-0 starts pulling **even though it has no local pod demand for that digest**, because it has been explicitly asked to. After completion it serves the content to peers; it can later evict the content if it remains the only consumer (see §7.4).

### 5.5 Designated puller is down or unreachable

`pull_intent_query` to rank-0 (for this digest) times out within 2 seconds.

- The requesting agent's "reachable top-K" set excludes rank-0.
- Rank-1 becomes the lowest-ranked reachable node for this digest.
- `please_pull` goes to rank-1. Rank-1 starts pulling.

The takeover is **bounded by the dial timeout** (~2s), not by content size. This is the central reason for fan-out over single-target dial.

### 5.6 Designated puller stalls mid-pull

The puller responded "starting" but its origin connection is now hung. Provider record never appears in the DHT.

- Requesters polling the DHT eventually hit their per-digest max-wait timer (the timeout values from §5.2a are the concrete "implausibly long" thresholds: 5 s for manifest/config, `expected_pull_seconds × 3` for layers).
- Each requester re-runs §5.2 from step 3 **for that digest only**. The stall is per-digest; other digests of the same image are unaffected and may be progressing normally on different designated pullers.
- The new `pull_intent_query` may still see rank-0 as alive (TCP-wise), but its response includes the in-flight state with `started_at`  -  if `(now - started_at)` exceeds the same per-digest timeout from §5.2a, requesters treat the pull as stalled and exclude rank-0 from `please_pull` candidates.
- Rank-1 receives `please_pull`, starts pulling from origin in parallel with rank-0's stalled attempt.
- Whichever finishes first calls `Provide`. The slower one eventually completes (or times out) and also calls `Provide`. No correctness issue.

Origin sees two pulls of that digest. Acceptable price for liveness under failure.

### 5.7 Network partition

Cluster splits into partition A and partition B. Rank-0 is in A; some requesters are in B.

- Partition A: rank-0 is reachable; behaves like §5.2.
- Partition B: rank-0 is unreachable. The reachable top-K (from B's view of cluster membership) excludes rank-0. The lowest-ranked reachable node  -  say rank-1 in B's view  -  becomes the designated puller for partition B.
- Both partitions make independent progress. Origin sees two pulls.
- When the partition heals, both rank-0 and rank-1 are providers. The DHT merges naturally; subsequent requesters find both.

**Liveness is preserved at the cost of one extra origin pull per partition.** Acceptable.

**Negative cache after partition heal.** Negative cache entries (§5.8) are puller-local and do not merge across partitions. After heal, each former-puller carries its own `recent_failures` history, applied only to digests on that puller. There is no cross-partition reconciliation; the worst case after heal is that a digest-and-puller pair carries a stale cooldown that delays the next attempt by at most the configured cooldown ceiling (default 10 min). Acceptable.

**Provider visibility during partition.** Each partition's DHT reflects only its own providers; designated-puller selection and `pull_intent_query` answers in partition B run against B's view alone. Because Gantry adds no replication-aware eviction over containerd (§7.4), the per-partition provider set is just "whichever nodes containerd has the digest on," with no Gantry-side deferral logic to skew under the split. After heal, DHT records from both partitions reconverge within the libp2p provider-record TTL refresh interval; the stale-provider cache (`gantry_peer_fetch_total{outcome="stale_provider"}`) absorbs the window where a previously-advertised digest has since been GC'd by containerd on its side.

### 5.8 Origin is down or rejecting pulls

If origin is unreachable or returns errors, the designated puller's pull fails. After exhausting retries, it must avoid both (a) returning failure indefinitely while origin recovers, and (b) allowing every subsequent requester to re-trigger a fresh designated-puller cascade against a known-broken origin. The puller therefore maintains a **per-digest negative cache** with circuit-breaker semantics.

**Failure classification.** On terminal failure of an origin pull, the puller classifies the cause:

- `auth`  -  401/403 from origin. Credentials are wrong or revoked. Same credentials will fail identically on every node, so the failure is cluster-relevant.
- `not_found`  -  404 from origin. The digest does not exist at the configured upstream registry. Cluster-relevant: rank-1 will get the same answer.
- `rate_limited`  -  429 from origin. Origin is back-pressuring; respect it.
- `transient`  -  connection refused/reset, 5xx, timeout, DNS failure. May be intermittent; may be flapping.

**Negative cache structure (puller-local, in-memory):**

```
recent_failures[digest] = {
    last_failure: time,
    failure_count: int,
    failure_class: "auth" | "not_found" | "rate_limited" | "transient",
    cooldown_until: time,
}
```

**Cooldown schedule (exponential, capped):** 1st failure -> 10 s, 2nd -> 30 s, 3rd -> 2 min, 4th+ -> 10 min cap. The first successful pull of the digest clears the entry. Configurable knobs in §7.7.

**Signal propagation via the existing probe RPCs (no new RPCs).** While a digest is in cooldown, the puller's responses change:

- `pull_intent_query(digest)` returns `{has_cached: false, in_flight: false, recently_failed: true, cooldown_until: T, failure_class: X, hrw_rank: R}`.
- `please_pull(digest)` returns `recently_failed(cooldown_until=T, failure_class=X)` instead of starting a pull.

**Requester behavior on `recently_failed`.** When a requester receives `recently_failed` from any reachable top-K node during the §5.2 step 5 probe:

- `auth`, `not_found`: trust cluster-wide. Asking rank-1, rank-2, or any other node is futile  -  same credentials and same digest produce the same answer everywhere. Requester returns 5xx to `containerd` immediately. No `please_pull` to anyone for this digest.
- `rate_limited`: trust cluster-wide. Requester returns 5xx; `kubelet`'s exponential retry naturally surfaces a new attempt later, after origin's rate window has likely reset.
- `transient`: trust per-digest, but apply a local **honor window** of `min(cooldown_until - now, 30 s)` before sending `please_pull` for this digest to *any* top-K node. A flapping origin will fail rank-1 the same way it failed rank-0; sequential retries within the cooldown window only generate origin pressure. After the honor window expires, the requester re-probes; by then rank-0's own cooldown may also have expired and the next attempt is single-shot.

**Cluster-wide effect.** Origin pull rate is bounded to roughly **one attempt per cooldown interval per affected digest cluster-wide**, regardless of how many requesters are waiting. Under a sustained outage, that is ≤ 6 origin attempts/hour for a given digest at the 10-min cooldown ceiling, no matter how many pods want it.

**Self-healing.** When origin recovers, the next attempt after cooldown succeeds. The puller clears `recent_failures[digest]`, marks the digest present through the advertiser, and serves peers via the warm path. No operator action required.

**Why the negative cache is local-only, not propagated via DHT.** Same rationale as gap #4's fix (§7.7): a stale cluster-wide "this digest failed" marker that outlived an actual recovery would be a serious bug. The puller-plus-honor-window pair already bounds the cascade adequately without introducing eventual-consistency hazards.

**For images that have never been pulled into the cluster** and where origin is also down, the agent returns 5xx to `containerd`. `kubelet` retries naturally on its own backoff. The system does not invent content it cannot fetch.

**Distinguishing puller failure from origin failure.** A puller that has crashed or is unreachable returns no `recently_failed` response  -  the requester sees a TCP-level timeout, and the existing §5.5 (puller down) takeover applies, routing to rank-1. The negative cache is only consulted when the puller is alive and able to respond. This separation is intentional: puller failures should reroute; origin failures should back off.

### 5.9 Node joins / leaves cluster

- **Join:** the agent starts up, the Kubernetes informer reports the new node to all other agents within a few seconds, HRW rankings update naturally. The new node bootstraps its libp2p host using peers from the informer's existing pod list and joins the DHT.
- **Leave:** existing provider records held by the departed node expire from the DHT (TTL, default 24h with 12h refresh). HRW rankings update on all surviving agents as the informer removes the departed node. If the departed node was a designated puller for an in-flight pull, the stall-detection path in §5.6 recovers.

---

## 6. Failure modes summary

| Scenario | Behavior | Origin pulls | Recovery time |
|---|---|---|---|
| Single node down | DHT TTL expires; HRW reroutes | 0 (warm) or 1 per affected digest (cold) | DHT TTL refresh interval |
| Designated puller unreachable (cold start) | Fan-out detects, rank-1 takes over | 1 per digest | ~2s dial timeout |
| Designated puller stalls (cold start) | Per-digest timeout, rank-1 takes over | 2 for the stalled digest; other digests unaffected | Per-digest timeout (5s manifest/config, size-aware for layers) |
| Origin down, content cached | Warm path serves from peers | 0 | None |
| Origin down, content not cached (transient) | Negative cache on puller; honor window on requesters; cooldown 10 s -> 10 min exponential | ≤ 1 per digest per cooldown interval cluster-wide (≤ 6/hour at the cap, regardless of cluster size) | Until origin recovers; first post-cooldown attempt succeeds and clears entry |
| Origin returns 401/403 (auth) | Negative cache marks `auth`; trusted cluster-wide; requesters return 5xx immediately | 1 cluster-wide per cooldown interval; no cascade across top-K | Operator credential fix |
| Origin returns 404 (not_found) | Negative cache marks `not_found`; trusted cluster-wide; requesters return 5xx immediately | 1 cluster-wide per cooldown interval; no cascade across top-K | Operator deployment fix |
| Origin returns 429 (rate_limited) | Negative cache marks `rate_limited`; requesters return 5xx; kubelet retry surfaces later | 1 cluster-wide per cooldown interval | Until origin's rate window resets |
| Network partition | Each partition pulls independently | 2 per affected digest | Until partition heals |
| Thundering herd (digest-keyed) | HRW selects single puller per digest; receiver dedupes | N+2 cluster-wide for an N-layer image (one per unique digest), spread across up to N+2 different pullers | Single pull duration of the slowest digest |
| Membership view divergence (informer lag during rolling updates) | Divergent views select different rank-0; receiver-side dedupe at each puller bounds per-puller damage but not cross-puller | ≤3 origin pulls per affected digest (typical 1) | Bounded by informer convergence (<5 s typical) |
| Tag-keyed pull, origin reachable (steady state, by design) | Agent returns `503` for tag-shaped requests; containerd's `hosts.toml` chain reaches origin; origin resolves the tag and returns the manifest body. Subsequent config and layer pulls are digest-keyed and F1-bounded. See §5.1a. | 1 per node per pod-start for manifest resolution (~10 KB body + 1 TLS handshake); cluster-wide rate scales with pod-start rate, not cluster size. **Layer bytes remain F1-bounded.** F1 does not cover this; §8 v2 HRW-with-TTL is the candidate to bound it. | N/A (steady-state behavior, not a failure-recovery cycle). |
| Tag-keyed pull, origin unreachable | Agent returns `503`; containerd reaches origin via fallback; origin contact fails. New pod-starts that require tag resolution fail across the cluster. Digest-keyed pulls (running pods, pinned digests, `imagePullPolicy: IfNotPresent` on already-cached nodes) continue to succeed peer-to-peer. See §5.1a operator guidance. | 0 successful; failed attempts proportional to pod-start rate, throttled per node by kubelet `ImagePullBackOff` | Until origin recovers. Per-node retry rate is bounded by kubelet `ImagePullBackOff`; cluster-wide recovery is single-shot once origin returns. |
| DHT degraded | HRW top-K probe is authoritative on DHT miss; false-empties caught at top-K (typical case: rank-0 has the digest cached and serves it). Under Degraded/Unhealthy health, top-2K expansion is eager (§7.7). Origin is contacted only when probe also confirms cold-start. NF5 direct-origin fallback is jittered + rate-limited (§7.7). | 0 in the typical case; bounded by per-node rate-limit (default 2/min) only if probe also fails | RPC-overhead penalty only; no origin impact in typical case |
| All top-K unreachable for a digest (partition/failure) | Probe expands to top-2K unconditionally (§5.2 step 5), regardless of DHT health. If still empty, NF5 jittered fallback fires under rate-limit. | 0 in the typical case (probe finds a provider at rank-K+1..rank-2K); otherwise bounded by per-node rate-limit | Bounded by 2 s + expanded-probe timeout |

---

## 7. Implementation notes

### 7.1 Containerd integration

Configure `containerd` via `/etc/containerd/certs.d/<registry>/hosts.toml`:

```toml
server = "https://registry.example.com"

[host."http://127.0.0.1:5000"]
  capabilities = ["pull", "resolve"]
  skip_verify = true
```

The agent must respond to standard OCI Distribution API endpoints (`/v2/`, `/v2/<name>/manifests/<ref>`, `/v2/<name>/blobs/<digest>`). On any internal error, the agent should return 5xx, prompting `containerd` to fall back to the next configured host (the actual origin).

The `server = ...` directive causes containerd to attach `?ns=<registry>` to every mirrored request. This is required for tag references (§5.1a)  -  without it, the agent cannot determine which upstream registry a bare tag like `library/nginx:latest` belongs to. The mirror-fallback to origin on 5xx is load-bearing for tag references: every tag-shaped request to the agent returns `503` by design, and the fallback chain is the only mechanism by which tags are ever resolved (§5.1a). `skip_verify = true` is safe here because the mirror endpoint is `127.0.0.1` over loopback; do not propagate this setting to non-loopback mirrors.

### 7.2 libp2p configuration

- **Transport:** TCP + QUIC. Noise for encryption.
- **DHT mode:** Server mode on every agent (all agents serve queries). With 10k+ nodes, this is fine  -  Kademlia scales to this size comfortably (IPFS runs at much larger scale).
- **Bootstrap:** the agent's Kubernetes informer provides a list of peer pod IPs. On startup the agent draws a random subset of **8** peer IPs and dials them in parallel. If fewer than **4** respond within 5 s, the agent draws another random subset of 8 from the remaining pool and retries. Total dials per startup are capped at **32**; after that the agent proceeds with whatever routing-table state it has and relies on lazy routing-table growth as DHT queries flow. The 8-peer subset is sized to populate multiple Kademlia buckets (bucket size 20 in `go-libp2p-kad-dht`) on a single round while remaining cheap; the cap prevents pathological retry on a freshly-rolled-out DaemonSet where no peer is yet ready  -  in that case the bootstrap-window suppression (§7.7) and NF5 jitter/rate-limit handle the genuinely-cold case.
- **Provider record TTL:** 24h with 12h refresh (libp2p default). Dead nodes age out automatically.
- **Identity persistence:** the libp2p private key is generated on first start and persisted to `hostPath`. Lost identity is not catastrophic  -  the agent rejoins with a new ID; old DHT records expire.

### 7.3 Cluster membership

A Kubernetes informer watches the DaemonSet's own pods (or `Nodes` with a label selector). Cached list updates on add/remove events. This is the single Kubernetes API dependency.

- HRW computation uses pod IDs (or node names) and is recomputed locally on every cold-start; no persistent state.
- For topology-aware HRW, the informer also tracks node labels (`topology.kubernetes.io/zone`).

**Containerd image-event subscription.** The agent subscribes to containerd's image events (`containerd.client.EventService()`), filtered to the three relevant topics. The subscription's purpose is to learn which content digests now live in containerd's content store so they can be advertised in the DHT and served to peers. **Tags are not advertised** (see §4.3 / §5.1a).

- `/containerd/event/v1/Image/Create`  -  a new image appeared in containerd's image table. The agent walks the manifest tree (manifest -> config, layers; for manifest lists, also per-arch manifests) and routes every resulting digest through the advertiser. The advertiser verifies local openability before issuing `DHT.Provide`. No tag key is advertised.
- `/containerd/event/v1/Image/Update`  -  a tag was rebound to a different digest. Functionally identical to Create from the agent's perspective: walk the new manifest tree and advertise the new digests through the advertiser. Previously-advertised digests remain advertised only while containerd still reports them present/openable; periodic inventory reconcile and delete events drive best-effort withdraw. Because the agent advertises only digests and not tag->digest bindings, there is no cluster-wide tag inconsistency arising from this event  -  every peer-served fetch is by digest and unambiguous. The rebinding itself is invisible at the Gantry layer: other nodes do not learn the new binding through Gantry, new pods on other nodes pick up the new digest via §5.1a's per-pod-start origin resolution path (subject to `imagePullPolicy`), and pods already running on the old digest are unaffected per normal OCI semantics.
- `/containerd/event/v1/Image/Delete` and content-delete events  -  the event is routed through the advertiser as absent. `DHT.Withdraw` is best-effort; remote provider records may remain visible until libp2p TTL expiry, so peer 404/stale-provider handling remains the correctness mechanism.

**Event filtering.** Subscription is filtered to image references whose registry component matches the agent's configured upstream-registry list. This excludes sideloaded images (`ctr image import`) and images for registries the agent is not mirroring.

**Advertiser-owned deduplication.** Background `please_pull`, startup reconcile, and cdsub create/update/delete notifications all route through `internal/advertise`. The advertiser owns the local announced set, so duplicate present notifications are idempotent and disappeared digests can trigger best-effort `Withdraw` without depending on cdsub's process-local event history.

**Stream-loss handling.** If the event stream drops, the agent reconnects with exponential backoff. On reconnect, it runs a full reconciliation against containerd's image list (`ImageService().List(ctx)`), walks each image's manifest tree, and re-advertises every digest currently present in containerd's content store. Bounded by content-store size; cheap.

The subscription is per-agent and does not require any cluster-wide coordination.

### 7.4 Containerd store and lease lifecycle

**The agent does not maintain a separate content cache.** As of
plan-final-copilot-v2 §Phase 8 the `gantry-cache` hostPath backend was
removed. Reads and writes flow through the local containerd content
store via `internal/containerdstore`, which wraps
`content.Store.{Info, ReaderAt, Writer, Commit}` and exposes the
`ifaces.LocalContentStore` surface the rest of the agent uses.

- **Inventory.** `cdsub` walks `images.List` -> `images.Children`
  (downgrading `errdefs.IsNotFound` to absent so a half-pulled
  multi-arch index does not poison the entire reconcile), keeping only
  digests whose content store entry is **present and openable**. That
  set is fed into `internal/advertise`, which diffs against the
  previously-announced set on every reconcile and calls
  `dht.Provide` / `dht.Withdraw` for the delta. Reconcile metrics:
  `gantry_advertise_reconcile_total`,
  `gantry_advertise_reconcile_duration_seconds`,
  `gantry_advertise_reconcile_digest_count`,
  `gantry_advertise_reconcile_added_total`,
  `gantry_advertise_reconcile_removed_total`,
  `gantry_advertise_withdraw_total`.
- **GC is containerd's, not Gantry's.** Containerd's existing GC walks
  Image references, snapshotter mounts, and explicit leases. Gantry
  adds no replication-aware deferral; the operator-visible knob is
  containerd's own retention policy.
- **Lease lifecycle (`please_pull` ingest).** Before background ingest
  opens the containerd writer, the puller creates a containerd lease
  labeled `gantry.io/managed=true` plus
  `gantry.io/source / repository / digest / created` and binds the
  expected content digest as a lease resource. The lease protects the
  blob throughout write/commit and through the window before kubelet's
  `pull_image` CRI call creates the eventual Image reference. Failed
  ingests release their lease immediately; successful ingests keep it
  until TTL cleanup. Default TTL is
  `60m` (`containerd_lease_ttl`); a sweeper goroutine runs every
  `containerd_lease_cleanup_interval` (default `30m`) and deletes
  leases whose age exceeds the TTL  -  kubelet's Image reference owns
  retention thereafter. Lease metrics:
  `gantry_containerd_lease_created_total`,
  `gantry_containerd_lease_cleanup_error_total`,
  `gantry_containerd_ingest_total`,
  `gantry_containerd_ingest_failure_total`.
- **Stale-provider semantics.** Because the DHT can return providers
  whose containerd has since evicted, restarted, or never observed
  the commit event, `internal/mirror` classifies peer 404 /
  digest-mismatch responses as `stale_provider`, caches the
  `(digest, peer)` pair for a bounded TTL, and short-circuits past
  it on subsequent requests. When every DHT-returned provider is
  stale the request falls through to the cold-start path. Self
  providers are filtered after the local containerd miss. Metric:
  `gantry_peer_fetch_total{outcome="stale_provider"}`.
- **Storage-mode gauge.** `gantry_storage_mode_info{mode="containerd"} 1`
  is emitted by the agent so dashboards can confirm the active
  backend. The `gantry-cache` value is rejected at config-validation
  time with a migration error pointing at this section.

The two-store-drift problem the pre-Phase-8 architecture
exposed  -  DHT advertising digests whose Gantry-cache had been purged
while containerd still held them, and vice versa  -  is gone by
construction: there is only one store, and the reconcile loop keeps
the DHT consistent with it.

### 7.5 Security

- **Transport encryption:** libp2p Noise (built-in).
- **Content verification:** OCI digest verification on every byte received from peers. Non-negotiable.
- **Origin auth:** the agent uses the same registry credentials as `containerd` for pulls from origin (read from a Secret mounted into the DaemonSet pod). v1 assumes those credentials are uniform cluster-wide; per-node credentials (IRSA, Workload Identity, per-node ServiceAccount) are out of scope and have specific consequences for the authorization model documented in §7.8.
- **NetworkPolicy:** the transfer port (HTTP/2) and libp2p listen ports are restricted to inter-node traffic only.
- **Signature verification:** out of scope. Existing tooling (Cosign, admission controllers) handles this and is unaffected by the P2P layer.

### 7.6 Metrics

Critical for operating this at scale. Minimum set:

- `p2p_cache_hit_total` / `p2p_cache_miss_total`  -  local containerd-store effectiveness (workload-side, via the containerd mirror endpoint). After plan-final-copilot-v2 §Phase 8 the "cache" here is the local containerd content store, not a separate Gantry-managed cache.
- `p2p_peer_serve_total`  -  increments when this agent serves a blob or manifest to a peer over the `:5001` transfer endpoint (i.e., requests carrying `Gantry-Mirrored: 1`; see §4.4). Per-peer contribution to cluster-wide P2P distribution.
- `gantry_storage_mode_info{mode}`  -  gauge always at 1 for the active backend (`containerd`). Lets dashboards filter by storage mode and confirm a rollout completed.
- `gantry_advertise_reconcile_total` / `gantry_advertise_reconcile_error_total`  -  advertiser reconcile loop counters.
- `gantry_advertise_reconcile_duration_seconds`  -  histogram of reconcile latency.
- `gantry_advertise_reconcile_digest_count`  -  gauge of the present-set size observed at the most recent reconcile.
- `gantry_advertise_reconcile_added_total` / `gantry_advertise_reconcile_removed_total`  -  Provide / Withdraw deltas emitted by the reconcile loop. Steady-state values reflect containerd churn.
- `gantry_advertise_withdraw_total` / `gantry_advertise_withdraw_error_total`  -  Withdraw call counters. Withdraw failures are logged and metered but do not corrupt local state  -  the next reconcile retries.
- `gantry_containerd_lease_created_total` / `gantry_containerd_lease_cleanup_error_total`  -  lease lifecycle counters (§7.4).
- `gantry_containerd_ingest_total` / `gantry_containerd_ingest_failure_total`  -  `please_pull`-driven Commit success / failure counts. Mirror these against `p2p_origin_pull_total` to spot pull-succeeded-but-Commit-failed regressions.
- `gantry_peer_fetch_total{outcome="hit|notfound|stale_provider|…"}`  -  peer transfer outcomes. `stale_provider` is the new outcome introduced by plan-final-copilot-v2 §Phase 1: peer 404 / digest-mismatch on a digest the local node has not yet committed.
- `p2p_dht_lookup_duration_seconds`  -  DHT health.
- `p2p_dht_lookup_total{outcome="hit|miss"}`  -  DHT lookup counts by outcome. All lookups in v1 are digest-keyed (tag-shaped containerd requests are short-circuited to the mirror-fallback chain without a DHT lookup; see §5.1a); misses fall through to HRW top-K probe (§5.2).
- `p2p_dht_advertise_total`  -  DHT advertisement counts. All advertisements in v1 are digest-keyed (manifest, config, layer digests advertised via §7.3's image-event subscription); the agent never advertises tag keys.
- `p2p_peer_dial_success_total` / `p2p_peer_dial_failure_total`  -  peer reachability.
- `p2p_hrw_rank_mismatch_total{digest_kind="manifest|config|layer"}`  -  increments when a requester's locally-computed HRW rank for a recipient disagrees with the `hrw_rank` the recipient reports in its `PullIntentResponse`. Direct measurement of Kubernetes informer divergence (§5.3 caveat); steady-state value is near-zero, spikes track rolling-update windows. The `digest_kind` label distinguishes whether manifest, config, or layer cold-starts are most affected. Implementations MAY additionally expose a `recipient_node` label for per-node mismatch attribution, but this should be opt-in due to 10k-cardinality concerns at full cluster scale; the WARN log line that accompanies the metric increment carries the recipient node identifier unconditionally.
- `p2p_origin_pull_total{kind="manifest|config|layer"}`  -  **counted per digest, not per image.** A fully-cold image with 10 layers produces 12 increments (one manifest, one config, ten layers) under normal operation. Sustained values much higher than the digest count of recently-rolled-out images indicate HRW or DHT pathology.
- `p2p_dht_false_empty_total`  -  increments when `FindProviders` returns empty but the subsequent HRW top-K probe finds the digest cached on a top-K node. A direct measurement of DHT pathology; should be near-zero in steady state.
- `p2p_topk_probe_hit_total`  -  increments when the HRW top-K probe finds a cached or in-flight provider after a DHT miss. The cluster's safety margin against DHT degradation; spikes track DHT health regressions.
- `p2p_topk_expansion_total{reason="all_unreachable|degraded_health|unhealthy_health"}`  -  increments when the probe is expanded from top-K to top-2K. `all_unreachable` is the partition/failure indicator; the other two track DHT-health-driven eagerness.
- `p2p_dht_health_score`  -  gauge in [0, 1] from §7.7's health calculation.
- `p2p_origin_fallback_total`  -  increments only when NF5's direct-origin fallback fires (DHT empty, top-K probe empty, top-2K probe empty, rate-limiter permits). Should be effectively zero in normal operation. Any non-zero value warrants investigation.
- `p2p_origin_failure_total{class="auth|not_found|rate_limited|transient"}`  -  counts terminal origin-pull failures per failure class. Sustained non-zero `auth` or `not_found` indicates an operator action is needed (bad credentials or missing image); sustained `transient` indicates origin instability.
- `p2p_negative_cache_entries`  -  gauge of digests currently in cooldown on this puller. Should be near-zero in steady state; non-trivial values track origin-side health.
- `p2p_negative_cache_hit_total{class="…"}`  -  increments when a `pull_intent_query` or `please_pull` response was suppressed because of a cooldown entry. Measures the back-pressure the negative cache is providing.
- `p2p_designated_puller_takeover_total`  -  frequency of rank-1 / rank-2 takeover. Spikes indicate rank-0 reliability issues.
- `p2p_cold_start_duration_seconds`  -  wall-clock latency from cache miss to image available.
- `p2p_in_flight_pulls`  -  gauge of currently-active origin pulls.

### 7.7 DHT health and origin-fallback gating

`dht.FindProviders` returning empty is ambiguous (see §5.2). The agent maintains a continuous local DHT health score and uses it to gate the NF5 direct-origin fallback. Direct origin fallback is the last-resort safety net; it must not become a thundering-herd vector under DHT pathology.

**Health score inputs (each agent computes locally):**

- **Routing table coverage:** `routing_table_size / expected_size`, where `expected_size = min(informer_node_count, kademlia_max_routing_table_size)`. Score component 1.0 when full; degrades linearly as buckets thin out.
- **Lookup latency:** rolling p95 of `dht.FindProviders` duration over the last 5 minutes. Score component 1.0 when p95 < 200 ms; degrades to 0 at p95 > 5 s.
- **Self-test success rate:** every 60 s, the agent issues `Provide(self_id)` followed by a `FindProviders(self_id)` from a random subset of bootstrap peers; success rate over the last 10 self-tests. Score component is the success rate directly.

The aggregate health score is the geometric mean of the three components, exposed as `p2p_dht_health_score`. Health states:

- **Healthy** (score ≥ 0.7): default behavior. DHT hits drive the warm path; DHT misses fall through to HRW top-K probe (§5.2 step 3). Top-2K expansion fires only on the all-top-K-unreachable rule (§5.2 step 5).
- **Degraded** (0.3 ≤ score < 0.7): top-K probe expands to top-2K **eagerly**  -  even when all top-K respond with `neither cached nor in-flight`  -  before declaring cold-start. NF5 fallback remains blocked unless the top-2K probe also fails.
- **Unhealthy** (score < 0.3): treat every `FindProviders` result as advisory only and always run HRW top-K probe regardless. Eager top-2K expansion continues to apply. Bootstrap-window suppression also triggers (see below).

Note that all-top-K-unreachable triggers top-2K expansion **regardless of health state** (§5.2 step 5); Degraded/Unhealthy only adds eager expansion on top-K-honestly-empty.

**Bootstrap-window suppression.** For the first 30 s after agent startup (or until the routing table reaches **25%** of expected size, whichever comes first), the agent does not trust DHT-empty as cold-start evidence. All discovery goes through HRW top-K probe. If both DHT and probe fail to find a provider during this window, the agent defers origin fallback by re-checking every 2 s until either window expires or a provider is found. (For a 10k-node cluster, expected routing-table size is approximately `20 × log2(10000) ≈ 266` entries; 25% is ~67. With 8-peer bootstrap dials and a 32-dial cap (§7.2), the threshold relies on transitive discovery to converge within the window. Re-tune empirically if bootstrap windows commonly elapse before the threshold is met.)

**NF5 jitter and rate-limit (origin-fallback gating).** Direct origin fallback fires only after: DHT lookup empty, HRW top-K probe empty, top-2K probe empty (when expansion triggered  -  see §5.2 step 5 and the Degraded/Unhealthy rules above), `please_pull` to all reachable top-K returns failure (e.g., origin down for them too), AND the per-node fallback rate-limiter permits. The agent applies:

- **Random delay** drawn from `[0, base × ln(N))` where N is `informer_node_count` and `ln` is the natural logarithm. With N = 10,000 and `base = 3 s`, jitter spans roughly 0–28 s (`3 × ln(10000) ≈ 27.6`). After the delay, the agent re-runs DHT lookup and HRW top-K probe; if either succeeds in the meantime, the fallback is canceled.
- **Token-bucket per-node:** at most M direct-origin fallbacks per minute per agent (default M = 2). At most 1 in-flight direct-origin fallback per digest per agent. Excess requests block on a condition variable that wakes when the in-flight fallback completes; they then re-enter the discovery flow at §5.2 step 2.
- **Negative caching is intentionally not used.** A "this digest does not exist" marker propagated via DHT would only address the case of genuine non-existence (Case A in the gap analysis); cases B/C/D are false-empties for which no consensus exists to write a marker. A stale negative marker outliving an actual pull is also a serious bug. The HRW top-K probe addresses all four cases at the requester without introducing cluster-wide negative state.

**What this changes for §5.2 and §6.** The pre-Mitigation §6 row "DHT degraded -> up to N origin pulls" is replaced. Under the revised flow, DHT degradation produces only RPC-overhead penalty in the typical case (top-K probe finds the digest). Origin fallback is reachable only via the conjunction above, and is bounded by jitter + rate-limit even when reached.

**Origin-failure circuit breaker (§5.8)  -  distinct from NF5 fallback.** §5.8's per-puller negative cache also gates origin contact, but for a different reason: it suppresses repeated origin attempts for digests that have *recently failed* at origin (auth/not_found/rate_limited/transient). NF5's gating above answers "should I bypass the P2P path?"; §5.8's gating answers "is it worth trying origin again at all?". Both apply: a request that gets through the NF5 gate is still subject to the §5.8 cooldown if the digest is in the negative cache. Configurable knobs:

- **NF5 fallback gating:** `nf5_jitter_base` (default 3 s, scaled by `ln(N)`), `nf5_per_node_rate_limit` (default 2/min), `bootstrap_window` (default 30 s or 25% routing table), `topk_expansion_when_degraded` (default 2K).
- **§5.8 origin-failure circuit breaker:** `origin_failure_cooldown_initial` (default 10 s), `origin_failure_cooldown_max` (default 10 min), `origin_failure_cooldown_multiplier` (default 3×), `origin_failure_honor_window` at requesters (default `min(cooldown_until - now, 30 s)`), `origin_failure_classes_trusted_cluster_wide` (default `{auth, not_found, rate_limited}`; `transient` is honored locally only).

### 7.8 Authorization model and registry credentials

v1's authorization model rests on a single load-bearing assumption: **registry credentials are uniform across every node in the cluster.** Every agent holds the same Secret-mounted credential and uses it for origin pulls. Several pieces of the design rely on this directly:

- **§5.8 cluster-wide trust of `auth` and `not_found`.** "Rank-1 will get the same answer as rank-0" is only true if rank-0 and rank-1 hold the same credentials and present the same identity to origin. Per-node credentials break this: rank-0's `401/403` does not imply rank-1 will fail, and propagating the `auth` failure class cluster-wide would incorrectly block requesters whose credentials are valid.
- **Peer-fetch authorization (§4.4 `:5001`).** The transfer endpoint serves any cached blob to any peer that can reach it on the network (NetworkPolicy-scoped, but otherwise unauthenticated at the application layer). Under uniform credentials this is equivalent to the requester pulling from origin under its own (identical) credentials. Under per-node credentials, a peer with no origin-read authorization for a given blob can still fetch that blob from a peer that pulled it under different credentials  -  an authorization escalation.

**Out of scope for v1.** Per-node identity models (IRSA on EKS, Workload Identity on GKE, ServiceAccount-mediated registry auth) are explicitly out of scope. Operators using these models must accept that:

- F1's "small constant origin pulls" property degrades roughly to "one origin pull per credential-domain per digest," since each credential domain effectively forms an independent cluster from origin's perspective.
- The §5.8 cluster-wide `auth`/`not_found` trust must be turned off or downgraded to per-digest-and-credentials trust, which requires a non-trivial protocol change.
- A peer with weaker credentials can read content pulled by a peer with stronger credentials. This is acceptable only when all credential domains within a cluster trust each other transitively.

**Future direction (deferred).** Candidate approaches for closing the gap, none of which v1 should absorb:

- *Requester-mediated pulls.* The requester forwards a short-lived origin credential token in `please_pull`; the puller uses it for one request and discards. Adds a credential-handling RPC and rotates the `please_pull` abuse surface (§8 open question) into a credential-laundering risk.
- *Per-credential-domain HRW.* Form independent P2P swarms per credential, sacrificing cross-domain layer sharing for correctness. Cleanest semantics; worst load-balancing.
- *Out-of-band authorization assertions* at the `:5001` endpoint. Requires an external trust source (e.g., SPIFFE/SPIRE), which is a substantial dependency.

See §8 open question on per-node credentials.

---

## 8. Open questions

- **K value tuning.** Default K=3 for HRW top-K. Is this right for our cluster sizes? Empirically validate. Also: when degraded, is top-2K sufficient or should the expansion be larger?
- **Topology scoping.** Default to cluster-wide HRW or per-zone HRW? Depends on cross-zone bandwidth cost in our environment.
- **Containerd retention tuning.** Gantry now serves from containerd directly and protects background-ingested content with bounded leases. Production rollout still needs empirical tuning of lease TTL, cleanup cadence, and containerd's own GC settings under real image churn.
- **Per-node registry credentials.** v1 assumes uniform cluster-wide credentials (§7.8). Per-node IAM models (IRSA, Workload Identity, per-node ServiceAccount auth) break §5.8's cluster-wide `auth`/`not_found` trust and introduce an authorization-escalation surface at the peer-fetch endpoint. Out of scope for v1; v2 candidates are outlined in §7.8.
- **Eviction provider-count threshold.** Removed in plan-final-copilot-v2 §Phase 8. Containerd's own GC is now the only eviction authority; the `EvictionProviderCountThreshold` knob, the 5% forced-eviction headroom, and the cache-pressure scoring loop no longer exist. The remaining operator knobs that affect retention of Gantry-ingested content are `containerd_lease_ttl` and `containerd_lease_cleanup_interval` (§7.4).
- **Chunk-level granularity.** Deferred. Reconsider if cold-start convergence times for large images (>1 GB) are unacceptable in practice.
- **Image admission signaling.** When a new image is pushed to origin, should the system pre-warm? Out of scope for v1; revisit if rollout latency is a problem.
- **Negative-cache cooldown schedule.** Defaults: 10 s -> 30 s -> 2 min -> 10 min cap. Tune empirically against observed origin recovery patterns.
- **NF5 fallback gating constants.** Jitter base (default 3 s, scaled by `ln(N)`), per-node rate limit (default 2/min), bootstrap window (default 30 s or 25% routing table), top-K-to-top-2K expansion threshold. All should be tuned empirically at scale.
- **NF6 RAM budget.** The <100 MB/<5% CPU target was set before the negative cache, DHT health stats, and view tracking were added. Re-validate against real workloads at 10k-node scale before committing to this number; IPFS server-mode DHT nodes typically run hotter than 100 MB.
- **DHT inbound RPC fan-in under thundering herd.** §5.3 asserts that ~10,000 inbound `pull_intent_query` RPCs per top-K node is "manageable." This is asserted, not load-tested. Validate at scale before relying on it.
- **Eviction-time DHT lookup load.** Removed in plan-final-copilot-v2 §Phase 8 along with the Gantry-owned eviction loop  -  there is no longer a per-eviction-candidate DHT lookup. Containerd's own GC owns content lifetime now.
- **`please_pull` abuse and peer authorization.** Currently any peer can send `please_pull(arbitrary_digest)` (origin-pull oracle / cost DoS) or fetch any locally-cached blob from any node. Single-cluster context probably acceptable, but explicit decision needed: rate-limit per-source on `please_pull` (and reintroduce `OUTCOME_DECLINED` in the proto when this lands); explicit policy on cross-namespace blob access.
- **HRW-coordinated tag resolution with TTL refresh (v2 candidate).** A peer-served tag-resolution layer that builds on top of v1's defer-to-origin design (§5.1a). v1's tag handling is correct and clean but has two operational costs: every pod-start hits origin for tag resolution (small per-pull  -  manifest body only  -  but proportional to pod-start rate), and tag-keyed pulls fail when origin is unreachable. The v2 mechanism converts both into bounded staleness. **Mechanism:** compute HRW on the tag key, designate rank-0 as the resolution leader, add a `tag_intent_query` RPC (analogous to `pull_intent_query`), have rank-0 maintain a `tag -> (digest, fetched_at, generation)` cache with a TTL and re-resolve against origin every `TTL/2` (jittered). Followers cache the answer locally for the remainder of the TTL window and re-query the leader on expiry. **Properties:**
  - *Origin contact rate per tag drops from one-per-pod-start to one-per-TTL* regardless of cluster size. Default TTL candidate: 30 s; validate empirically against rebinding rates observed in target environments.
  - *Availability under origin partition is restored* within the cached binding's TTL. Tag-keyed pulls succeed peer-to-peer as long as some node has resolved the tag within the last TTL period.
  - *Staleness is TTL-bounded.* Pods scheduled inside the staleness window get the previous binding; the cluster converges to current origin truth within at most TTL seconds. This deviates from OCI's implicit "resolve-on-every-pull" semantic and **must be operator-opt-in per registry**  -  expose a per-registry switch so operators who want exact origin-resolution semantics for specific registries (typically registries with frequent tag mutation like `latest` channels) can disable v2 tag resolution selectively.
  - *Poisoning surface is bounded* to "the deterministic rank-0 node for that specific tag." In a hypothetical alternative design where any caching node can advertise a tag binding, an attacker who compromises any single node can poison any tag in the cluster; HRW-for-tags restricts the poisoning surface for each tag to the one deterministic leader for that tag's key, which the attacker can predict but not choose. Still not eliminated; tag resolution remains unverifiable by followers (tags aren't content-addressed). Under §7.8's uniform-cluster-credentials assumption, this is the most disciplined trust topology achievable without introducing peer-side tag signatures (out of scope).
  - *Pre-positioning attack  -  a v2-only surface that v1 entirely avoids.* Tag HRW introduces an attack surface that digest HRW does not have, because consumers cannot verify a tag->digest binding the way they verify content against a digest. A node-ID-grinding attacker who can add nodes to the cluster can target a specific tag by computing `SHA256(node_id || tag)` offline and choosing a `node_id` that outranks existing nodes for that tag; the attacker then becomes rank-0 for that tag and can advertise an arbitrary `tag -> digest` binding to followers within their TTL windows. Whether this matters depends on the cluster's threat model for node admission. Under typical Kubernetes RBAC, adding a node is operator-equivalent, so the attack reduces to “an operator can lie about tags,” which is true regardless of Gantry. The attack is sharper in environments with automated node admission  -  autoscalers with weak identity binding, multi-tenant clusters without per-tenant node pools, or any setting where node-ID choice is decoupled from operator authority. v2 should treat tag-binding integrity as a separable design problem rather than assuming HRW alone bounds the surface; candidate directions include constraining node IDs through a trusted identity authority (SPIFFE/SPIRE-style), requiring quorum agreement across multiple top-K nodes before followers cache a binding, or accepting the residual risk under an explicit cluster-trust assumption documented alongside §7.8. **This entire surface is absent from v1.** By deferring tag resolution to origin, v1 has no peer-served tag-binding state to attack  -  the only authority for `tag -> digest` is origin, accessed under each node's own credentials, exactly as OCI specifies. That is a genuine v1 advantage worth naming explicitly when weighing the v2 promotion.
  - *Leader failover* reuses the digest top-K mechanism: if rank-0 is unreachable, rank-1 takes over and resolves against origin on its next refresh tick. The view-skew dedupe machinery from §5.3 applies: two nodes can independently believe they are rank-0 during informer-convergence windows. If a rebinding lands between their queries they will advertise different digests and the cluster will oscillate until view skew heals  -  bounded by the same view-convergence dynamics as digest HRW.
  - *Credentials* are subsumed by §7.8's uniform-cluster-credentials assumption; HRW-for-tags does not introduce a new credential delegation problem beyond what already exists for HRW-for-digests. Per-node credentials remain explicitly out of scope (§7.8).
  - *Rebinding invalidation* is handled by the refresh tick + generation number: when rank-0's re-resolve returns a different digest, generation increments and the new binding is advertised. Followers that re-query after their cache expires get the new digest. Followers that pulled during the prior generation are unaffected (already-running pods keep their pulled digest per normal OCI semantics).
  - *Per-registry opt-in.* Where enabled, the leader handles resolution for that registry's tags. Where disabled, v1's §5.1a defer-to-origin behavior applies exactly.
  - *Open sub-question:* should the cluster maintain a single TTL for all tags, per-registry TTLs, or per-tag-pattern TTLs (e.g., shorter TTL for `*:latest`, longer for `*:vX.Y.Z`)? Per-pattern is more powerful but adds configuration surface.

---

## 9. Prior art and references

- **Dragonfly** (https://d7y.io)  -  P2P distribution, but more centralized (uses supernodes / managers). Does not match the decentralization requirement.
- **Kraken** (Uber)  -  P2P with central tracker. Also does not match decentralization requirement.
- **Rendezvous hashing (HRW)**  -  Thaler & Ravishankar, 1998. Properties: deterministic, minimal disruption on membership change, no coordination needed.
- **libp2p Kademlia DHT**  -  go-libp2p-kad-dht. Production-tested at IPFS scale.

---

## 10. Glossary

- **HRW / Rendezvous Hashing**  -  a hashing scheme that maps a key to one or more nodes deterministically by computing `hash(node || key)` for every node and selecting the top-K by score. Property: when nodes are added/removed, only `1/N` of keys are remapped.
- **CID**  -  Content Identifier. Here, derived from the OCI digest of a layer or manifest.
- **OCI digest**  -  a `sha256:...` content hash defined by the OCI image spec.
- **Designated puller**  -  for a given **digest**, the node that HRW ranks highest among reachable cluster members. Responsible for pulling that digest from origin in cold-start scenarios. An image's manifest, config, and layer digests generally have *different* designated pullers, since HRW is computed independently per digest.
- **Warm path**  -  per-digest pull served from peer-cached content via DHT discovery.
- **Cold path**  -  per-digest pull where no peer has the content; requires origin contact via the designated puller.
