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
| `chunk_dir` | `CACHEMESH_CHUNK_DIR` | `/var/lib/gantry/cachemesh` | Directory of chunk files; wiped at startup. |
| `disk_cache_bytes` | `CACHEMESH_DISK_CACHE_BYTES` | 20GiB | Disk budget for chunk files; 0 disables the disk tier. |
| `memory_cache_bytes` | `CACHEMESH_MEMORY_CACHE_BYTES` | 512MiB | Hot page tier in memory; 0 disables it. |
| `inflight_bytes` | `CACHEMESH_INFLIGHT_BYTES` | 2GiB | Memory fills borrow for lagging readers and recent relay chunks. |
| `page_size` | `CACHEMESH_PAGE_SIZE` | 1MiB | Unit of buffers, disk I/O, coalescing, and the hot tier. |
| `fill_pages` | `CACHEMESH_FILL_PAGES` | 4 | In-flight pages per fill. |
| `page_cache` | `CACHEMESH_PAGE_CACHE` | `bypass` | `bypass` (O_DIRECT), `drop` (fadvise DONTNEED), or `keep`. |
| `persist` | `CACHEMESH_PERSIST` | `owned` | Chunks written to disk: `all`, `shared`, or `owned`. |
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

When cachemesh is enabled and `GOMEMLIMIT` is not set, the agent sets the Go
soft memory limit to 90% of the cgroup memory limit. Chunk page buffers are
anonymous mappings outside the Go heap (see the chunk store), so the limit
only governs the heap of the rest of the agent.

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

`meshfetch.store` (`internal/gantry/meshfetch/store.go`, `fill.go`,
`pages.go`, `disk.go`) keeps chunk bytes in files on local disk and holds
only a small, bounded amount in memory.

### Fills and the in-flight window

A fill is one chunk being fetched or already complete. The filler reads the
chunk into a window of `fill_pages` page buffers (4 x 1MiB by default).
Readers (the local blob stream and peers) that keep up copy bytes out of
those pages as they arrive, so every hop is cut-through and a hot fan-out
reads each byte from the network once. A page leaves the window when:

- the fill is written to disk and the page is on disk, or
- the fill is not written to disk and every reader has passed it.

A reader that falls behind the window reads the page back from disk. If the
page is not on disk either, the reader gets `errLost`; the local blob stream
then starts a new fill from the same byte, and a peer request that needs
bytes a fill has dropped detaches that fill from the map and starts a new
one.

When the window is full and a reader still needs its oldest page, the fill:

1. borrows another page from the store-wide in-flight budget
   (`inflight_bytes`) if peers read the fill or it is written to disk, so a
   slow reader does not slow the others down;
2. when the budget is used up, spills to disk: it starts a chunk file at its
   oldest window page, and lagging readers read from it;
3. otherwise waits for the slowest reader. A fill that only the local blob
   stream reads always does this: nobody else is waiting, and it keeps the
   lookahead fills from using memory. A reader that holds the fill back for
   `chunk_stall_timeout` is dropped with `errLost`.

### Lingering relay chunks

A fill that a peer has read and that is not written to disk keeps every page
in memory while the in-flight budget allows: instead of dropping a page that
all readers have passed, it borrows a new one. When such a fill completes
with the whole chunk still in memory, it stays in the map on a memory LRU
list, so peers that ask later (in the relay grid, nodes reach a chunk a few
seconds apart) join it instead of making the relay fetch it again. These
chunks give their pages back:

- when a fill needs to borrow and the budget is used up (least recently used
  first; lagging readers and new relay chunks win over old ones), and
- after 30s without use, so an idle agent returns the memory.

Without this, relays kept nothing and late requests fetched each chunk
again from upstream: about 80k extra chunk fills and 1.7x the bytes received
per node on the 142-node benchmark, and a p50 of about 101s instead of 91s.

### Disk tier

- Chunk files live in `chunk_dir`, one file per chunk, created with
  `O_DIRECT` and preallocated with `fallocate`. Pages are written in order
  by one writer goroutine per fill with `pwrite` (`WriteAt`); the final
  partial page is padded to the 4KiB `O_DIRECT` alignment.
- The directory is wiped at startup. Chunks are not digest-verified one by
  one, so files from an earlier process are never trusted.
- `disk_cache_bytes` bounds chunk files, including fills being written.
  Complete chunks are evicted least recently used first. Chunks this agent
  filled as their owner are on a separate list and are evicted only when no
  other complete chunk is left or when they hold more than half of the
  budget. Every agent caches transit copies, so with a plain LRU an owner
  evicted its own chunks while slower requesters still needed them and
  refilled them from the origin (1.33x origin egress on a 142-node 40GB pull
  with `maxNeighbors: 32`).
- A fill that finds no disk budget, or whose file cannot be created
  (`ENOSPC` pauses disk use for 10s), streams through memory. A failed
  write turns the fill into a stream-through fill; a failed read drops the
  chunk. A pull never fails because of the disk.
- `persist` selects which chunks are written: `all`, `shared` (owned
  chunks and chunks a peer asked for), or `owned` (default: only chunks this
  agent owns). Spills happen with any policy.
- The Helm chart mounts an `emptyDir` at `/var/lib/gantry/cachemesh`. It is
  writable by the non-root agent and goes away with the pod, so no chunk
  outlives it, and `gantry.cachemesh.chunkDirSizeLimit` can cap it. A
  `hostPath` would survive pod restarts, but the agent wipes the directory
  at startup anyway, and it would need ownership fixes for the non-root
  agent.

### Why `persist: owned` is the default

On the benchmark nodes containerd alone writes about 86GB per 40GB pull to
the OS disk, which sustains about 1.07GB/s of writes (`dd oflag=direct`),
so the disk is already the limit during a pull. Writing transit chunks too
made the 142-node pull 30-60% slower (`designs/gantry-cachemesh-benchmark.md`).
With `owned`, each agent writes only its own share of the image (about 0.3GB
of 43GB). Relays stream chunks through their windows and keep recent
peer-read chunks in the in-flight budget (see above). `all` or `shared` suit
nodes whose disk has room to spare or workloads where nodes pull at
different times.

### Page coalescing and the hot tier

Disk reads go through a page cache keyed by (fill, page index). Concurrent
readers of the same page share one disk read (singleflight). Each entry is a
reference-counted page buffer. When the last reader is done the page is
dropped, unless it showed repeated demand:

- two or more readers held it at once, or
- it was read again while its key was in a ghost list of recently dropped
  pages (four times the hot tier's page count).

Such pages stay in the hot tier, an LRU bounded by `memory_cache_bytes`.
Pages read by one reader once are never kept. Hot pages unused for 30s are
dropped, and evicting a chunk drops its hot pages.

### Memory bound

Chunk memory is page buffers only:

- in-flight windows: `fill_pages x page_size` per fill, at most
  `max_concurrent_chunk_fills` fills (256MiB with the defaults);
- borrowed pages, including lingering relay chunks: at most
  `inflight_bytes`;
- the hot tier: at most `memory_cache_bytes`;
- one page per active reader while it writes that page to its socket (a
  reader holds a reference only during one `Write`, so a slow reader pins at
  most one page and then falls back to disk).

With the defaults that is about 3.3GiB at most. Page buffers are anonymous
`mmap` mappings outside the Go heap: they do not raise the garbage
collector's heap goal, and `munmap` returns them to the system as soon as
they are freed (an idle pool keeps at most 64MiB).

### Page cache

Kubernetes counts active page cache in `container_memory_working_set_bytes`.
`page_cache: bypass` (default) uses `O_DIRECT`, so chunk files never enter
the page cache; the benchmark measured at most a few MiB of page cache in
the agent cgroup. `drop` uses buffered I/O and calls `sync_file_range` plus
`posix_fadvise(DONTNEED)` after each write and read. `keep` leaves caching to
the kernel. `sendfile` was not used: pages are read into shared buffers so
that concurrent readers coalesce, and the copy to the socket is cheap next to
the disk.

### Containerd duplication

Every node also writes each blob to containerd's content store and unpacks
it. The store does not try to serve completed blobs from containerd; with
`persist: owned` the extra disk writes are small enough not to matter.

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
- `gantry_cachemesh_chunk_evicted_bytes_total` (disk chunk store)
- `gantry_cachemesh_control_errors_total`,
  `gantry_cachemesh_lease_errors_total{op}`
- Gauges: `gantry_cachemesh_members`, `gantry_cachemesh_view_term`,
  `gantry_cachemesh_view_seq`, `gantry_cachemesh_leading`,
  `gantry_cachemesh_ready`, `gantry_cachemesh_neighbors`,
  `gantry_cachemesh_chunk_disk_bytes`, `gantry_cachemesh_chunk_store_chunks`,
  `gantry_cachemesh_chunk_fills_inflight`,
  `gantry_cachemesh_page_memory_bytes`, `gantry_cachemesh_hot_page_bytes`,
  `gantry_cachemesh_inflight_borrowed_bytes`
- Chunk store counters: `gantry_cachemesh_disk_page_reads_total`,
  `gantry_cachemesh_disk_page_writes_total`,
  `gantry_cachemesh_disk_errors_total`,
  `gantry_cachemesh_shared_page_reads_total` (reads served by another
  reader's disk read or the hot tier), `gantry_cachemesh_spills_total`,
  `gantry_cachemesh_stream_fills_total`

## Transport choices

- Data requests use a dedicated HTTP/1.1 client (`meshfetch.NewDataClient`):
  one TCP connection per concurrent chunk avoids HTTP/2 flow-control windows
  capping a single stream, and large read buffers cut syscalls.
- The cachemesh routes are registered with `transfer.WithExtraRoutes`, outside
  the otelhttp wrapper, so chunk traffic creates no spans.

Benchmarks (`go test -bench . ./internal/gantry/meshfetch`) cover a cold
fetch through owners, a fetch from a peer's store with and without digest
verification (SHA-256 caps one blob at about 1.8 GB/s), serving a chunk
from disk in each page cache mode, fan-out of one chunk to 32 concurrent
readers (about one disk read per page), and a fill followed by 8
cut-through readers. Disk numbers depend on the device: on a busy
development VM one `O_DIRECT` reader managed about 75MB/s at queue depth 1,
against about 370MB/s with `drop` and page-cache speed with `keep`.

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
- The chunk directory is wiped at startup, so a restarted node starts with
  an empty chunk store.
- Chunks on disk are not digest-verified one by one; only whole blobs are
  (`verify_digest`), so silent disk corruption is caught only at the blob
  level.
- With the default `persist: owned`, a relay keeps a chunk only in memory,
  for at most 30s unused and only while the in-flight budget has room. A
  peer that asks later makes the relay fetch the chunk again from upstream.
- Disk reads are synchronous, one page at a time per reader (queue depth 1
  per reader). There is no read-ahead.
