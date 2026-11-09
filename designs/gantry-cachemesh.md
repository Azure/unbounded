# Gantry cachemesh content path

Status: implemented, off by default.

This document describes how Gantry uses the cachemesh routing library
(`internal/cachemesh`, design in `designs/cachemesh.md`) to move image
content between agents. The library only decides routes. Gantry owns the
transport, the chunk store, coalescing, retries, and metrics. The code is in
`internal/gantry/meshfetch`, `internal/gantry/mirror/cachemesh.go`,
`internal/gantry/transfer` (`WithExtraRoutes`), and
`cmd/gantry/agent_cachemesh.go`.

## Goals

- Pull each chunk of a layer from the origin registry once per cluster, not
  once per node.
- Spread the load of a large image over every node: each node owns some
  chunks and serves them to the others.
- Stream bytes through every hop as they arrive, with bounded memory.
- Leave the default (DHT peer-to-peer) path unchanged when disabled, and use
  it as the fallback when the mesh cannot serve a request.

## Configuration

All settings live under `cachemesh:` in the agent config
(`internal/gantry/config/cachemesh.go`). Nothing changes unless `enabled` is
true.

| YAML key | Env (`GANTRY_` prefix) | Default | Meaning |
|---|---|---|---|
| `enabled` | `CACHEMESH_ENABLED` | `false` | Turn the path on. |
| `namespace` | `CACHEMESH_NAMESPACE` | `chair_namespace` | Namespace of the leader Lease. |
| `lease_name` | `CACHEMESH_LEASE_NAME` | `gantry-cachemesh` | Leader Lease name. |
| `chunk_size` | `CACHEMESH_CHUNK_SIZE` | 64MiB | Stripe size; power of two, 1MiB to 1GiB, same on every node. |
| `expected_size` | `CACHEMESH_EXPECTED_SIZE` | 0 (library: 1000) | Expected node count. |
| `cell_size` | `CACHEMESH_CELL_SIZE` | 0 (library: 4) | Nodes per grid cell. |
| `max_neighbors` | `CACHEMESH_MAX_NEIGHBORS` | 32 (explicit 0: library default 256) | Data-plane neighbors per node. |
| `partition_bits` | `CACHEMESH_PARTITION_BITS` | 0 (library: 20) | log2 of the partition count. |
| `metadata_ttl` | `CACHEMESH_METADATA_TTL` | 5m | Tag cache freshness; at least 5m. |
| `cache_tags` | `CACHEMESH_CACHE_TAGS` | `true` | Serve anonymous tag requests from the mesh. |
| `verify_digest` | `CACHEMESH_VERIFY_DIGEST` | `true` | Hash full blobs before the last byte. |
| `chunk_cache_bytes` | `CACHEMESH_CHUNK_CACHE_BYTES` | 4GiB | Memory for cached plus in-flight chunks. |
| `max_concurrent_chunk_fills` | `CACHEMESH_MAX_CONCURRENT_CHUNK_FILLS` | 64 | Chunks fetched at once. |
| `max_concurrent_chunk_serves` | `CACHEMESH_MAX_CONCURRENT_CHUNK_SERVES` | 512 | Chunk responses to peers at once. |
| `lookahead` | `CACHEMESH_LOOKAHEAD` | 8 | Chunks of one blob fetched ahead. |
| `chunk_timeout` | `CACHEMESH_CHUNK_TIMEOUT` | 5m | Limit for one chunk attempt. |
| `chunk_stall_timeout` | `CACHEMESH_CHUNK_STALL_TIMEOUT` | 30s | Fail an attempt with no progress. |
| `chunk_attempts` | `CACHEMESH_CHUNK_ATTEMPTS` | 4 | Attempts per chunk. |
| `hedge` | `CACHEMESH_HEDGE` | `true` | Hedge slow chunk requests. |

Validation requires a Lease namespace and `pod_ip`. The Racer backend
ignores the block.

Grid shape: gantry defaults `max_neighbors` to 32, unlike the cachemesh
library, which defaults to 256 because it is sized for clusters of up to 100k
members (`designs/cachemesh.md`). With about 150 nodes and 256 neighbors,
every node is a neighbor of every other node, so every request goes straight
to the owner, and the owner of a popular chunk serves every node itself.
With 32 the grid is two-dimensional, so requests relay through one
intermediate node that caches the chunk on the way and spreads the fan-out.
On 142 nodes pulling a 40GB image, 32 gave a p50 of about 91s against about
104s with 256 (`designs/gantry-cachemesh-benchmark.md`). An explicit
`max_neighbors: 0` in the agent config selects the library default. The
chart omits zero values, so to get 256 through the chart set it explicitly.

The Helm chart exposes the same knobs as `gantry.cachemesh.*` (camelCase).
It renders the block, and the `GANTRY_NODE_NAME` env var, only when
`gantry.cachemesh.enabled` is true. Example:

```yaml
gantry:
  cachemesh:
    enabled: true
    expectedSize: 150
resources:
  agent:
    limits:
      memory: 8Gi
```

## Membership

Each agent creates one `cachemesh.Mesh`:

- ID: the Kubernetes node name (`GANTRY_NODE_NAME`), else `POD_NAME`, else
  the hostname.
- Addr: `<pod IP>:<transfer port>`. Control and data requests share the
  transfer listener (default port 5001).
- Lease: `kubelease.Store` on the leader Lease. The agent Role already allows
  get, list, create, and update on Leases.
- Control transport: HTTP/1.1 `POST /cachemesh/control`, body bounded by
  `cachemesh.MaxControlMessageSize`.

`Run` starts after the transfer listener is up. Agent readiness does not wait
for the mesh; the mirror simply declines to use it until `Mesh.Ready()`. On
shutdown the agent calls `Leave` (5s budget) before the transfer listener
stops, so other members take over its partitions while it can still serve.

When the chunk cache is enabled and `GOMEMLIMIT` is not set, the agent sets
the Go soft memory limit to 90% of the cgroup memory limit, because gigabytes
of live chunk buffers would otherwise let the heap grow past the container
limit before a GC.

## Chunks and keys

A blob of size S is split into `ceil(S / chunk_size)` chunks; the last one
may be short. Chunk keys live in the immutable keyspace `gantry-blob`
(TTL 0). The routing key is the kind byte (blob or manifest), the digest
string, and the 8-byte chunk index, so chunks of one layer land on different
owners.

Manifests by digest use the same path with the manifest kind. They are small
and fit in one chunk; the owner keeps the upstream Content-Type.

The owner fills a chunk with a bounded `PullRange` request
(`origin.Client.PullRange`). The first response tells every hop the blob
size, so there is no separate HEAD request.

## Data path

`GET /cachemesh/chunk?r=<registry>&n=<repo>&d=<digest>&k=<kind>&i=<index>&cs=<chunk size>[&o=<offset>]`
with the 32-byte cachemesh header in `Gantry-Cachemesh-Route` (base64url).
The response is 200 with `Gantry-Blob-Size`, `Content-Type`, and
`Content-Length` of the rest of the chunk. Requests with another chunk size
are refused (400).

On a mirror GET for a digest that is not in the local containerd store (and
on open-ended `Range: bytes=N-` requests, which containerd uses to resume):

1. If the mesh is not ready, the mirror uses its default path.
2. The node routes chunk `N / chunk_size` with `Mesh.Route` and waits for
   the blob size. If that fails, nothing has been written and the mirror uses
   its default path (peers, then origin).
3. The mirror writes the status line and headers (200, or 206 with
   `Content-Range`).
4. A producer keeps `lookahead` later chunks in flight; the writer copies
   chunks to containerd in order as their bytes arrive.
5. If a chunk fails, the node routes it again and continues from the same
   byte, up to `chunk_attempts` times. After that the response is aborted
   (`http.ErrAbortHandler`) and containerd retries with a Range request,
   which goes through the same path.

Every node handles a chunk request the same way, whether it came from its
own mirror or from a peer:

- If the chunk is in the local store (complete or still filling), stream it
  from there. Concurrent requests for one chunk share one fill.
- Otherwise start a fill. The decision comes from `Route` (local request) or
  `Forward` with the request header (peer request):
  - `KindSelf`: this node owns the chunk (or stands in as a backup). It first
    reads through from `PrevOwner` during a handoff, and from the owner when
    acting as a backup, using a "peek" request that only returns chunks the
    other node has complete or is reading from the origin. A backup's peek
    (`peek=owner`) additionally makes a node that is the owner in its own
    view start its origin fill and serve it, so a hedge to a backup does not
    read the chunk from the origin a second time; a handoff peek (`peek=1`)
    never starts a fill, since the previous owner's own fill may be waiting
    on the new owner. Otherwise it pulls the chunk from the origin.
  - Relay, owner, backup, gateway, direct: send to `Next` with the decision
    header. After `HedgeAfter`, if `TryHedge` allows, send a copy with
    `FlagHedge` to `Alternate` using `AlternateHeader`; the first response
    wins. On failure, retry to `Alternate`, then route again.
  - No route: fail.
- Every peer outcome is reported with `Observe` (latency to headers).
  429 and 503 count as `Overload`; stalls as `Timeout`.
- `ErrTTLExhausted`: serve only from the local store, else 508.

## Chunk store

`meshfetch.store` keeps chunks in memory under `chunk_cache_bytes`:

- The budget covers cached chunks and fills in progress. A fill is never
  evicted. Complete chunks are evicted least recently used first, with one
  exception: chunks this agent filled as their owner are kept on a separate
  list and evicted only when no other complete chunk is left or when owned
  chunks hold more than half of the budget. Every agent caches the transit
  copies of everything it pulls or relays, so with a plain LRU an owner
  evicted its own chunks while slower requesters still needed them and
  refilled them from the origin (1.33x origin egress on a 142-node 40GB pull
  with `maxNeighbors: 32`).
- When fills alone use the budget (or `max_concurrent_chunk_fills` is
  reached), peer requests that need a new fill get 429 and local requests
  wait.
- A fill buffer is allocated once at full chunk length. Readers stream
  `buf[off:n]` straight to the socket while the filler appends, so every hop
  is cut-through with no extra copy and no disk I/O.
- Buffers are reference counted and reused through a pool once no reader
  holds them. A reader may keep an evicted chunk alive briefly, so actual
  memory can exceed the budget by the chunks being written at that moment.

## Integrity

Chunks cannot be checked one by one: OCI only gives the digest of the whole
blob. Containerd checks the full digest when it commits. With
`verify_digest`, the serving node also hashes the full blob (offset 0 only)
and holds back the last byte until the hash matches. On a mismatch it drops
every cached chunk of the blob and aborts the response, so a bad chunk from
a peer or the origin cannot be committed and is not served again from this
node's cache. Other nodes that cached the bad chunk find out on their own
verification.

## Tags and metadata

Tag resolutions use a separate keyspace, `gantry-meta`, with TTL
`metadata_ttl`. The routing key is `<registry>/<repo>:<tag>`.

- The owner resolves a tag with one origin GET
  (`origin.Client.ManifestByTag`), stamps it with `Mesh.Stamp`, and caches
  the manifest bytes, media type, and digest.
- Other nodes fetch through `GET /cachemesh/meta?r=&n=&t=` with the route
  header, check that the body matches `Docker-Content-Digest`, and cache the
  entry with the owner's stamp (`Gantry-Stamp-Expires`, `Gantry-Stamp-Gen`).
- Every holder checks `Mesh.Fresh` before using a copy. One resolution per
  key runs at a time on each node.

The mirror answers a tag request from this cache only when `cache_tags` is
on, the request has no `Authorization` header, and the registry does not
require authentication (`AuthenticationChallenge`). Otherwise it keeps the
existing 503, and containerd goes to the origin. A tag cannot be verified
against content, so a node that lies about a tag can point other nodes at
the wrong image for up to the TTL (see the tag-binding note in
`designs/gantry-detailed-design.md`). Manifests by digest are safe because
containerd verifies them.

## Interaction with the default path

- Local hits, HEAD requests, and the DHT, cold-start, and origin paths are
  unchanged. The mesh runs after the local-store miss and before the peer
  lookup.
- Bytes served through the mesh count in
  `gantry_mirror_bytes_served_total{source="cachemesh"}` and the completion
  hooks, so the stream-commit tracker and the advertiser still see them.
  Completed blobs are announced in the DHT as before.
- With cachemesh on, the layer prefetcher only tracks layer progress; it no
  longer asks chairs to seed layers from the origin, which would duplicate
  the mesh's origin pulls.

## Metrics

All in the `cachemesh` subsystem:

- `gantry_cachemesh_chunk_lookups_total{requester,result}`
- `gantry_cachemesh_chunk_fills_total{source,outcome}`,
  `gantry_cachemesh_chunk_fill_bytes_total{source}`,
  `gantry_cachemesh_chunk_fill_duration_seconds{source,outcome}`
- `gantry_cachemesh_peer_requests_total{result}`,
  `gantry_cachemesh_peer_request_seconds{result}`
- `gantry_cachemesh_hedges_total{outcome}`
- `gantry_cachemesh_chunk_serves_total{outcome}`,
  `gantry_cachemesh_chunk_serve_bytes_total`
- `gantry_cachemesh_blob_requests_total{outcome}`,
  `gantry_cachemesh_blob_bytes_total`,
  `gantry_cachemesh_blob_duration_seconds{outcome}`
- `gantry_cachemesh_tag_lookups_total{outcome}`
- `gantry_cachemesh_chunk_evicted_bytes_total`
- `gantry_cachemesh_control_errors_total`,
  `gantry_cachemesh_lease_errors_total{op}`
- Gauges: `gantry_cachemesh_members`, `gantry_cachemesh_view_term`,
  `gantry_cachemesh_view_seq`, `gantry_cachemesh_leading`,
  `gantry_cachemesh_ready`, `gantry_cachemesh_neighbors`,
  `gantry_cachemesh_chunk_cache_bytes`, `gantry_cachemesh_chunk_cache_chunks`,
  `gantry_cachemesh_chunk_fills_inflight`

## Transport choices

- Data requests use a dedicated HTTP/1.1 client (`meshfetch.NewDataClient`):
  one TCP connection per concurrent chunk avoids HTTP/2 flow-control windows
  capping a single stream, and large read buffers cut syscalls.
- The cachemesh routes are registered with `transfer.WithExtraRoutes`, outside
  the otelhttp wrapper, so chunk traffic creates no spans.

Benchmarks (`go test -bench . ./internal/gantry/meshfetch`, loopback, 16
cores, 256MiB blob, 16MiB chunks): cold fetch through owners and an
in-memory origin about 7 GB/s; fetch from a peer's cache about 13.6 GB/s;
the same with digest verification about 1.8 GB/s (SHA-256 bound, per blob).

## Security

The control, chunk, and metadata endpoints have no authentication, like
`/v2/` on the transfer port. Anyone who can reach port 5001 can join the mesh
control plane, read chunks of any blob a node can pull, and make owners pull
from the origin. Chunk owners pull with the agent's own registry identity
(configured credentials or anonymous), never with a requester's delegated
credentials, so a node can obtain blobs that only another node's pod was
allowed to pull. Use NetworkPolicy to limit the port to Gantry agents.

## Limitations

- Routing loops can form while nodes see different views. A loop between two
  nodes is refused at once (the receiver checks whether its in-progress fill
  reads from the sender); longer loops end when the stall timeout fires.
- A backup that stands in for a slow owner reads through from the owner, but
  goes to the origin when the owner has not started the chunk, so a chunk
  may be pulled twice.
- HEAD requests on a local miss still go to the origin from every node.
- Owners pull from the origin with the agent's origin client, which uses
  HTTP/2 for HTTPS registries; many concurrent chunk pulls from one owner
  share a connection.
- The chunk size must match on every node; a mixed rollout refuses requests
  between nodes with different sizes, and the mirror falls back.
- Everything is in memory: a restarted node starts with an empty chunk cache.
