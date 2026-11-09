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
| `namespace` | `CACHEMESH_NAMESPACE` | `chair_namespace` | Namespace of the leader Lease (named `gantry-cachemesh`). |
| `chunk_size` | `CACHEMESH_CHUNK_SIZE` | 64MiB | Stripe size; power of two, 1MiB to 1GiB, same on every node. |
| `expected_size` | `CACHEMESH_EXPECTED_SIZE` | 0 (library: 1000) | Expected node count. |
| `max_neighbors` | `CACHEMESH_MAX_NEIGHBORS` | 32 | Data-plane neighbors per node; at least 1. |
| `content_root` | `CACHEMESH_CONTENT_ROOT` | `/var/lib/containerd/io.containerd.content.v1.content` | containerd content store directory, mounted read-only; the only disk tier. Empty or unreadable: serve peers from memory only. |
| `inflight_bytes` | `CACHEMESH_INFLIGHT_BYTES` | 2GiB | Memory fills borrow for lagging readers, recent relay chunks, and owned chunks. |
| `max_concurrent_chunk_fills` | `CACHEMESH_MAX_CONCURRENT_CHUNK_FILLS` | 64 | Chunks fetched at once. |
| `cache_tags` | `CACHEMESH_CACHE_TAGS` | `true` | Serve anonymous tag requests from the mesh. |
| `verify_digest` | `CACHEMESH_VERIFY_DIGEST` | `true` | Hash full blobs before the last byte. |

Validation requires a Lease namespace and `pod_ip`. The Racer backend
ignores the block. Unknown keys fail at load time (`KnownFields`), and the
chart schema rejects unknown values.

The rest are fixed defaults in `meshfetch` (`tuning` in
`internal/gantry/meshfetch/node.go`): 1MiB pages, a fill window of 4 pages,
512 concurrent chunk responses to peers (more get 429), 8 chunks of one
blob fetched ahead, 5m per fill attempt, 30s without progress fails an
attempt, 4 attempts per chunk, hedging on, 30s linger for unused chunks in
memory, and a 5m tag TTL (`cachemesh.MinTTL`). The grid cell size and
partition count use the cachemesh library defaults.

Grid shape: gantry defaults `max_neighbors` to 32, unlike the cachemesh
library, which defaults to 256 because it is sized for clusters of up to 100k
members (`designs/cachemesh.md`). With about 150 nodes and 256 neighbors,
every node is a neighbor of every other node, so every request goes straight
to the owner, and the owner of a popular chunk serves every node itself.
With 32 the grid is two-dimensional, so requests relay through one
intermediate node that caches the chunk on the way and spreads the fan-out.
On 142 nodes pulling a 40GB image, 32 gave a p50 of about 91s against about
104s with 256 (`designs/gantry-cachemesh-benchmark.md`). Set 256 explicitly
for the fully direct grid.

The Helm chart exposes the same knobs as `gantry.cachemesh.*` (camelCase,
except `namespace`). Zero values are omitted, so the agent defaults apply.
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
4. A producer keeps 8 later chunks in flight; the writer copies chunks to
   containerd in order as their bytes arrive.
5. A fill retries its sources up to 4 times in all, resuming at the byte it
   reached (`Node.route` in `fetch.go`). If the writer's reader loses bytes
   the fill no longer holds (`errLost`), it obtains a new reader, which may
   start a new fill, and continues from the same byte, up to 4 times
   (`Node.copyChunk`). A failed fill is not retried again by the writer.
   After that the response is aborted (`http.ErrAbortHandler`) and
   containerd retries with a Range request, which goes through the same
   path.

Every node handles a chunk request the same way, whether it came from its
own mirror or from a peer:

- If the chunk is in memory (a fill in progress, or a complete chunk
  lingering), stream it from there. Concurrent requests for one chunk share
  one fill.
- Otherwise start a fill. If the local containerd content store holds the
  whole chunk (a committed blob, or an ingest containerd is still writing),
  the fill reads it from there. Otherwise the fill is routed. The decision comes from `Route` (local request) or
  `Forward` with the request header (peer request):
  - `KindSelf`: this node owns the chunk (or stands in as a backup). It first
    reads through from `PrevOwner` during a handoff, and from the owner when
    acting as a backup, using a "peek" request that only returns chunks the
    other node has complete or is reading from the origin. A backup's peek
    (`peek=owner`) additionally makes a node that is the owner in its own
    view start its origin fill and serve it, so a hedge to a backup does not
    read the chunk from the origin a second time; a handoff peek
    (`peek=local`) never starts a fill, since the previous owner's own fill
    may be waiting on the new owner. Every peek is also served from the
    peer's content store when it holds the whole chunk. The owner then asks
    recent requesters of the chunk (the holder directory, below), also with
    `peek=local`. Otherwise it pulls the chunk from the origin. Peeks wait
    2s for response headers, holder peeks 1s.
  - Relay, owner, backup, gateway, direct: send to `Next` with the decision
    header. After `HedgeAfter`, if `TryHedge` allows, send a copy with
    `FlagHedge` to `Alternate` using `AlternateHeader`; the first response
    wins. On failure, retry to `Alternate`, then route again.
  - No route: fail.
- Every peer outcome is reported with `Observe` (latency to headers).
  429 and 503 count as `Overload`; stalls as `Timeout`.
- `ErrTTLExhausted`: serve only from memory or the content store, else 508.

## Chunk store

Gantry keeps no copy of image data on disk. The only disk copy is
containerd's own content store: containerd writes every blob while it
pulls through the gantry mirror, and the snapshotter unpacks from that same
blob. `meshfetch` (`internal/gantry/meshfetch/store.go`, `content.go`, and the
holder directory in `fetch.go`) holds chunks in bounded memory and
reads the content store back to serve peers.

### Fills and the in-flight window

A fill is one chunk being fetched or already complete. The filler reads the
chunk into a window of 4 page buffers of 1MiB.
Readers (the local blob stream and peers) that keep up copy bytes out of
those pages as they arrive, so every hop is cut-through and a hot fan-out
reads each byte from the network once.

A page leaves the window once no reader needs it from memory. A reader needs
memory from `max(position, local floor)` on, where the local floor is the
end of the whole pages of the chunk that the content store held when last
asked (`fill.needLocked`, `fill.localFloorLocked` in `store.go`). So a page
is dropped when every reader has passed it, or when containerd has written
it and lagging readers can read it back. A reader that falls behind the
window reads the page from the content store through the page cache below;
if the content store does not have it either, the reader gets `errLost`, the
local blob stream starts a new fill from the same byte, and a peer request
that needs bytes a fill has dropped starts a new fill.

The fill asks the content store how much of its chunk it holds
(`fill.refreshLocal`) at most every 10ms, and only when it matters: when its
window is full and a reader holds it back, every 40ms while it waits, and
when a reader finds its page gone.

When the window is full and a reader the content store does not cover still
needs the oldest page, the fill:

1. borrows another page from the store-wide in-flight budget
   (`inflight_bytes`) if peers read the fill, so a slow reader does not
   slow the others down;
2. otherwise waits for the slowest reader. A fill that only the local blob
   stream reads always does this: nobody else is waiting, and containerd
   writes those bytes right after the reader passes them. A reader that
   holds the fill back for 30s (the stall timeout) is dropped with `errLost`.

There is no spill: the previous design spilled lagging fills to private
chunk files. With the content store covering everything the local pull has
written, the only readers that can still exhaust the budget are peers of a
chunk this node is not pulling itself, and those pace the fill.

### Lingering and owned chunks

A fill that a peer has read, or that this node fills as the chunk's owner,
keeps every page in memory while the in-flight budget allows, unless the
content store already holds the chunk: instead of dropping a page that all
readers have passed, it borrows a new one. When such a fill completes with
the whole chunk in memory, it stays in the map on a memory LRU list, so
peers that ask later (in the relay grid, nodes reach a chunk a few seconds
apart) join it instead of fetching it again. These chunks give their pages
back:

- when a fill needs to borrow and the budget is used up (least recently used
  first, chunks this node does not own before owned ones), and
- after 30s without use, so an idle agent returns the memory.

Owned chunks linger because the owner's own pull reaches a given chunk at
an arbitrary time: without it, an owner whose fill completed before any
peer asked dropped the chunk, and a later peer request made it pull the
chunk from the origin again (seen as duplicate origin pulls in the tests).
Each node owns about `image size / nodes` of an image (0.3GB of 43GB on 142
nodes), so this fits the budget.

A complete chunk that left memory is served again from the content store:
a new fill reads it (`Node.fillFromLocal`), and concurrent requests share
that fill.

### Reading the content store

`ContentSource` (`content.go`) is implemented by `DirContent`, which reads
the content root directly. Committed blobs are at `blobs/sha256/<hex>`.
In-progress downloads are at `ingest/<key>/data`, where `<key>` is a hash of
the namespace and ingest ref, so the directory is listed (at most every
100ms, on a miss) and each `ref` file is parsed for the digest (CRI uses
`layer-sha256:<hex>`); the readable length is the size of `data`, capped at
`total`. containerd writes ingests sequentially with plain `write` calls, so
bytes below the file size are complete and in the page cache. A blob is
opened as committed, else as its ingest, else as committed again in case
containerd renamed it in between. On commit containerd renames `data` into
`blobs/`, so an open descriptor stays valid. Descriptors are shared per
blob with a reference count and closed 2s after their last use by a timer,
so a blob that containerd garbage collects does not keep its space for
long (opening the file for every 1MiB page read made serving from the
content store about a third slower). A short read (an ingest restarted
with a truncate, or a deleted blob) drops the descriptor and loses the
reader, which retries elsewhere. A permission error disables the source with one warning.
Digests are validated as 64 lowercase hex characters before they become a
path.

When `content_root` is empty or unreadable (`cachemeshContent` in
`cmd/gantry/agent_cachemesh.go`), the node runs without a content source:
it serves peers from memory only, and every fill goes to peers or the
origin.

Reads are buffered (`pread`), not `O_DIRECT`: the point is to hit the page
cache that containerd has just filled. On the 142-node benchmark agents read
180-280GB per run from the content stores (41k-65k page reads behind fill
windows plus 2.1k-3.2k chunk fills) while node disk reads stayed at 0.0GB,
so every read came from the page cache. The page cache belongs to containerd's cgroup (it wrote
the pages), so it does not count against the agent: the agent cgroup's page
cache stayed at about 0.1MiB. Since buffered reads did not touch the disk at
all, there is no `O_DIRECT` knob.

### Holder directory

Chunk requests made for a node's own containerd pull carry that node's
member address in `Gantry-Cachemesh-Requester`; relays forward it. Every
node remembers the last four requesters of each chunk (`holders` in
`fetch.go`; at most
32k chunks, entries expire after 6h). When the owner has the chunk neither
in memory nor in its own content store, it asks up to two of those
requesters with `peek=local`, which they serve only from memory or their
content store, before it goes to the origin. Requesters recorded less than
20s ago are skipped: they are most likely still pulling the chunk,
typically in the very request the owner is filling. A requester that no
longer has the blob answers 404 quickly (1s header timeout).

In a two-wave test (71 nodes, then the other 71 three minutes later) the
second wave pulled 0.00x the image from the origin: relays and owners that
pulled in the first wave served from their content stores, and owners that
did not asked holders (312 holder reads, 2 misses).

### Page coalescing

Content store reads go through a page cache keyed by (digest, absolute page
index), so readers of different fills of the same chunk share pages.
Concurrent readers of the same page share one read (singleflight). Each
entry is a reference-counted page buffer. When its last reader is done the
page joins a ring of the 32 most recently released pages, so readers of a
fan-out that trail a few pages behind share the read too; a page pushed
out of the ring is freed. Evicting a digest (failed verification) frees
its pages.

### Memory bound

Chunk memory is page buffers only:

- in-flight windows: 4 x 1MiB per fill, at most
  `max_concurrent_chunk_fills` fills (256MiB with the defaults);
- borrowed pages, including lingering and owned chunks: at most
  `inflight_bytes`;
- one page per active reader while it writes that page to its socket;
- the recent page ring: 32 pages (32MiB).

With the defaults that is about 2.3GiB plus the pages being sent. The
benchmark, run with a 512MiB hot page tier that has since been removed,
measured a working set peak of about 3.0GiB and about 0.26GiB 15s after the
pull. Page
buffers are anonymous `mmap` mappings outside the Go heap: they do not raise
the garbage collector's heap goal, and `munmap` returns them to the system
as soon as they are freed (an idle pool keeps at most 64MiB).

### Alternative: write the blob on behalf of containerd

Gantry could claim CRI's ingest ref (`layer-sha256:<hex>` in namespace
`k8s.io`) before CRI does, write the blob into the content store itself
from the mesh, and commit it, so that CRI's `OpenWriter` returns
`AlreadyExists` and CRI skips the download. That would let owners assemble
blobs they are not pulling, and avoid the stream-through hop. It was not
chosen:

- While gantry holds the ref, CRI's `OpenWriter` retries with random backoff
  capped at about 2s (`core/content/helpers.go` in containerd 2.1), so every
  layer could start unpacking up to 2s late, and unpack only starts after
  the commit anyway.
- A gantry writer that hangs or crashes holds the ref until its lease or
  the ingest expires, blocking the pull; the stream-through path can always
  fall back to the origin.
- It relies on CRI's internal ref naming and namespace, which is not an API.
- Gantry would have to learn the layer list from the manifest before CRI
  opens its writers, racing CRI on every layer.

It may be worth revisiting for owners that are not pulling an image (they
could keep owned chunks in a committed blob instead of memory), behind a
knob, if memory ever limits chunk lingering. The measured design matched
the previous disk-backed results without it.

## Integrity

Chunks cannot be checked one by one: OCI only gives the digest of the whole
blob. Containerd checks the full digest when it commits. With
`verify_digest`, the serving node also hashes the full blob (offset 0 only)
and holds back the last byte until the hash matches. On a mismatch it drops
every cached chunk of the blob and aborts the response, so a bad chunk from
a peer or the origin cannot be committed and is not served again from this
node's cache. It also stops reading that blob from the content store for
10 minutes: the bad bytes may still sit in containerd's ingest. Other nodes
that cached the bad chunk find out on their own verification.

The content store serves ingests before containerd has verified them. The
bytes there are exactly what this node's gantry served to containerd, so
they are as trustworthy as the chunks in memory; containerd still verifies
the full digest before commit on every node.

## Tags and metadata

Tag resolutions use a separate keyspace, `gantry-meta`, with a 5m TTL
(`cachemesh.MinTTL`). The routing key is `<registry>/<repo>:<tag>`.

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
- `gantry_cachemesh_control_errors_total`,
  `gantry_cachemesh_lease_errors_total{op}`
- Gauges: `gantry_cachemesh_members`, `gantry_cachemesh_view_term`,
  `gantry_cachemesh_view_seq`, `gantry_cachemesh_leading`,
  `gantry_cachemesh_ready`, `gantry_cachemesh_neighbors`,
  `gantry_cachemesh_chunk_store_chunks`, `gantry_cachemesh_memory_chunks`,
  `gantry_cachemesh_chunk_fills_inflight`,
  `gantry_cachemesh_page_memory_bytes`,
  `gantry_cachemesh_inflight_borrowed_bytes`
- Chunk store counters: `gantry_cachemesh_local_page_reads_total` (pages
  read from the content store), `gantry_cachemesh_local_fills_total`,
  `gantry_cachemesh_local_errors_total`,
  `gantry_cachemesh_shared_page_reads_total` (reads served by another
  reader's read), `gantry_cachemesh_holder_peeks_total`,
  `gantry_cachemesh_holder_misses_total`
- Fill sources (`source` label): `origin`, `peer`, `prev_owner`, `owner`,
  `local` (the content store), `holder` (a recent requester's content
  store).

## Transport choices

- Data requests use a dedicated HTTP/1.1 client (`meshfetch.NewDataClient`):
  one TCP connection per concurrent chunk avoids HTTP/2 flow-control windows
  capping a single stream, and large read buffers cut syscalls.
- The cachemesh routes are registered with `transfer.WithExtraRoutes`, outside
  the otelhttp wrapper, so chunk traffic creates no spans.

Benchmarks (`go test -bench . ./internal/gantry/meshfetch`) cover a cold
fetch through owners, a fetch from a peer's store with and without digest
verification (SHA-256 caps one blob at about 1.8 GB/s), serving a chunk
from a content store on disk (page cache warm), fan-out of one chunk to 32
concurrent readers (one content store read per page), and a fill followed
by 8 cut-through readers.

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
- A backup that stands in for a slow owner reads through from the owner with
  `peek=owner`, and an owner without the chunk starts its own origin fill
  and serves it. The backup still goes to the origin when the owner does
  not answer (or does not see itself as the owner), so a chunk may be
  pulled twice.
- HEAD requests on a local miss still go to the origin from every node.
- Owners pull from the origin with the agent's origin client, which uses
  HTTP/2 for HTTPS registries; many concurrent chunk pulls from one owner
  share a connection.
- The chunk size must match on every node; a mixed rollout refuses requests
  between nodes with different sizes, and the mirror falls back.
- Reading the content store relies on containerd's on-disk layout
  (`blobs/sha256/<hex>`, `ingest/<key>/{ref,total,data}`, rename on commit,
  sequential ingest writes), verified against containerd 2.1. A layout
  change makes the directory reader find nothing (or a permission error
  disables it), and cachemesh falls back to memory and the origin; it does
  not serve wrong bytes unless a file under a blob's path holds other data.
- The agent must be able to read the content root: the chart mounts it
  read-only, and the non-root agent relies on containerd's default
  permissions (directories 0755, blobs 0444, ingest files 0644).
- containerd may garbage collect a blob at any time (immediately, with the
  default GC settings, once no image or lease references it). Readers then
  get short reads and retry elsewhere; descriptors are closed after 2s idle
  so the space is reclaimed.
- A relay or owner that is not pulling an image keeps its chunks only in
  memory, for at most 30s unused and while the in-flight budget has room.
  After that its share is served from requesters' content stores (holder
  directory) or, if none still has the blob, from the origin.
- The holder directory only knows requesters whose requests reached this
  node: with relays, a coalesced relay fill carries one requester. It is
  lost on restart.
- Content store reads are synchronous, one page at a time per reader. There
  is no read-ahead.
