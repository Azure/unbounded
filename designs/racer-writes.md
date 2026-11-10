# Racer write-back caching

Status: proposed. This document is the spec for adding object writes to Racer:
cache-resident writes with N-copy durability, optional write-back to Azure Blob,
and client APIs including a PyTorch Distributed Checkpoint (DCP) storage plug-in.
Implementers should be able to build every work package (WP) from this document
alone. Every claim about current code cites `file:line` against the tree this
document was written on; re-check a citation before relying on it, because the
code is authoritative.

## Summary

A client `PUT`s an object to its node's per-cache client socket. The node
(the *writer*) mints a new immutable version, cuts the body into pages, encrypts
each page with the existing per-page AEAD, and asks the top N ranked owners of
each page to *pull* the page from it. Each owner verifies the ciphertext, writes
it to a pinned *write segment*, makes it durable (`fdatasync`), records it in a
per-worker fsynced *write journal*, and acknowledges. When every page has N
durable copies, the page-zero owner (the *coordinator*) durably records a
manifest and makes the version visible to readers. The PUT then returns
`201 Created` with the new `ETag`.

Separately, and according to the object's upload mode, the coordinator uploads
the object to Azure Blob: each page's holder sends one Put Block, and the
coordinator commits with a conditional Put Block List. Objects whose upload mode
is `never` or `manual` stay pinned in the cache for a bounded time and serve
cache-only checkpoints.

Writes are create-once: a PUT to a key that already exists (in the cache's write
directory or at the origin) fails with `412`. Written objects can be deleted
from the cache with `DELETE`. Overwrite is not supported in v1.

Racer remains a cache. Data that is written but not yet uploaded has N-copy
durability on node disks, not origin durability.

## Goals and non-goals

Goals:

- Checkpoint-class write throughput: page data moves once per replica over the
  existing peer transport (including RDMA), and the client sees an acknowledgment
  after N durable copies, without waiting for object storage.
- Configurable durability per request: `replicas=1..3`, or `origin` (ack only
  after the Blob commit).
- Cache-only objects with bounded pins (`Racer-Upload: never|manual`).
- Asynchronous, rate-limited upload to Azure Blob with conditional commit.
- Survives node crashes and restarts (journal + fsync), and repairs copies lost to
  node loss before upload.
- Deterministic-simulation coverage of crash, partition, and membership change.

Non-goals for v1:

- Overwrite or append of an existing object, multi-writer objects, or
  read-modify-write.
- Deleting origin objects. `DELETE` only removes cache-resident written objects.
- Strong cross-object transactions. DCP consistency is built from create-once
  objects and a metadata file written last.
- UDS origin adapter writes and racer-object S3 `PutObject` (described as later
  WPs, [WP13](#wp13-later-uds-origin-adapter-writes) and
  [WP14](#wp14-later-racer-object-s3-putobject)).

## Current code this builds on

Re-verified against the current crate layout
(`cmd/racer/{alloc,authority,core,crypto,dst,flow,flow-core,gpu,http,io,peer-wire,src,topology,verbs,wire}`,
workspace members at `cmd/racer/Cargo.toml:2`).

### Client socket

- The semantic parser is `RequestParser::parse` in
  `cmd/racer/wire/src/object/client.rs:169`. Only targets under `/v1/objects/`
  are accepted (`client.rs:173-176`), with the exact suffixes `host-read` and
  `gpu-read` selecting a direct read (`client.rs:177-183`).
- A body is rejected: `Content-Length` is accepted only as `0`
  (`client.rs:236`), any other value, `Transfer-Encoding`, `Expect`, and
  `If-None-Match` are `InvalidRequest` (`client.rs:237-252`).
- Method dispatch: `HEAD` and `POST` only; anything else is
  `Error::MethodNotAllowed` (`client.rs:307-356`), encoded as `405` with
  `Allow: HEAD, POST` (`client.rs:539-543`). Error responses have an empty body
  (`client.rs:540`).
- `ClientRequest` carries `kind`, `key`, opaque `metadata`
  (`Racer-Metadata`), and `authorization` (`client.rs:139-147`).
- `ReadKind` (`client.rs:17-54`) and `Destination` (`client.rs:59-63`) are the
  read request kinds; `HostRead` writes into a client descriptor received with
  the head.
- The listener reads each head with `recvmsg` so descriptors arrive with it
  (`cmd/racer/src/client/listener.rs:957-960`). Prefetch requests are routed
  before the object parser by `prefetch::is_prefetch` and served by
  `prefetch::serve`, which reads an exact JSON body
  (`listener.rs:971-987`, `cmd/racer/src/client/prefetch.rs:22`, `:57-61`).
  This is the precedent for a request with a body.
- The HTTP connection already exposes body reads: `read_body_exact`
  (`cmd/racer/http/src/connection.rs:1030`) and `read_body_range`
  (`connection.rs:1069`).
- HEAD success heads are built by `success_head` (`client.rs:505-536`): `ETag`,
  `Racer-Expires-At`, optional `Racer-Content-Type`, `Content-Length`.

### Model types

- `CacheKey([u8; 32])` (`cmd/racer/wire/src/object/model.rs:44`),
  `StrongEtag(String)` (`model.rs:62`), `ObjectId { cache, key }`
  (`model.rs:124-128`), `ObjectVersion { object, etag }` (`model.rs:132-136`),
  `PageId { version, number }` (`model.rs:140-144`).
- Page size: `PageSize::MAX` and `DEFAULT` are 16 MiB (`model.rs:166-167`);
  `PAGE_BYTES` is `PageSize::MAX` (`model.rs:151`). The cluster publishes the
  page size actually used (`cmd/racer/src/origin/azure.rs:362`).

### Peer protocol

- Every peer request is `POST /racer/peer/v1` with `racer-kind: request` and
  `content-length: 0`; encoders never include page bytes
  (`cmd/racer/src/peer/protocol.rs:1-3`, `:491-499`).
- Operations: `Operation::{Subscribe, Bootstrap, Page, Metadata, Prefetch}`
  (`protocol.rs:271-297`). `Prefetch` is the closest precedent for a write
  operation: an owner-directed action carrying a page list and object length
  (`protocol.rs:299-313`, encoded at `protocol.rs:570-595`, decoded by
  `prefetch_action` at `protocol.rs:650-684`).
- Fetch modes `CopyOnly`, `Acquire`, `Offer`, `Ping` (`protocol.rs:52-68`).
  `CopyOnly` serves only copies the responder already has; it is the "pull a
  known version from a specific node" primitive the read path already uses
  (for example `cmd/racer/src/read/candidates.rs:2070-2096`).
- Responses: `racer_core::read::PeerResponse` (`cmd/racer/core/src/read/mod.rs:144-173`)
  with variants `Selected`, `Bootstrap`, `Page`, `Metadata`, `Miss`,
  `NotFound`, `VersionUnavailable`, `Unavailable`, `Overloaded`, `FlightFull`,
  `OriginRejected`, `OriginForbidden`, `StaleMembership`, `Prefetch`.
- The node-side handler is `impl LocalPageService for Coordinator`
  (`cmd/racer/src/read.rs:600`); `Offer` and `Ping` are handled first
  (`read.rs:618-640`). Cross-worker work is routed by `Work` variants such as
  `Work::PrefetchFill` (`cmd/racer/src/read/dispatch.rs:147`, `:614`, `:1116`).
- Peer body cap tracks the page contract:
  `peer_wire::MAX_BODY == PAGE_BYTES + 16` (`protocol.rs:377`).

### Placement

- Weighted rendezvous hashing, `REPLICAS = 3` owners
  (`cmd/racer/topology/src/placement.rs:10`), `RANKED_DEPTH = REPLICAS + 2`
  exactly ranked members (`placement.rs:18`). Demoted members keep copies they
  fetched as owners and are asked before origin (`placement.rs:12-17`).
- The placement key is cache ID, object key, and page number; the version is not
  part of it (`cmd/racer/src/topology.rs:662-675`). `Placement::rank(membership,
  object, page)` returns ranked `NodeId`s (`topology.rs:734-749`).
- Within a node, `WorkerMap::owner(page)` and `metadata_owner(object)` (page 0)
  select the worker (`dispatch.rs:1413-1418`).

### Metadata and the page-zero owner

- Metadata resolution: `resolve_inner` tries a local copy, then joins or leads
  a refresh cohort (`cmd/racer/src/read/metadata.rs:466-599`). The leader runs
  `refresh` (`metadata.rs:601`), and the origin step is `refresh_origin`
  (`metadata.rs:707-783`). A pinned origin miss probes copy-only candidates
  before reporting the pin unavailable (`metadata.rs:732-744`, `:785`).
- The catalog holds immutable version descriptors (`publish_version`,
  `cmd/racer/src/store/catalog.rs:1040`) and a volatile current pointer with an
  expiry set by the page-zero owner after fresh revalidation (`publish_current`,
  `catalog.rs:1071-1093`; `current` filters expired pointers at
  `catalog.rs:1058-1069`).

### Storage

- Page records: `MAGIC = b"RCRPAGE1"`, 128-byte header prefix, 32-byte header
  digest (`cmd/racer/src/store.rs:1999-2001`), `RecordHeader` with generation,
  envelope, metadata, logical bytes, extent (`store.rs:2003-2010`). There is no
  CRC in the store; integrity is the SHA-256 header digest plus the AEAD tag.
  (The earlier "CRC-64" note in prior investigation is stale.)
- `StoreWriter::persist_held` forces an encrypted copy to disk for a held version;
  success means "a completed, published disk write, not queue acceptance or
  fsync durability" (`store.rs:783-859`).
- `Slab::fence_writes` "does not call fsync/fdatasync and does NOT promise crash
  durability" (`cmd/racer/alloc/src/slab.rs:276-284`). Slabs open with
  `O_DIRECT` (`slab.rs:433`, `:495`); raw devices open with
  `O_RDWR | O_DIRECT | O_EXCL` (`cmd/racer/src/app/devices.rs:302`).
- No dataplane store path calls fsync today. The io ring already supports it:
  `Reactor::fsync(fd, datasync, ctx) -> Fsync` (`cmd/racer/io/src/ring/ops.rs:1100-1103`,
  opcode at `cmd/racer/io/src/ring/op.rs:213-218`). The simulated disk models a
  volatile and a durable image, crash loss, and fsync fault injection
  (`cmd/racer/io/src/sim/disk.rs:1`, `:197-224`, `:248-302`).
- Checkpoints `checkpoint.0`/`checkpoint.1` (`cmd/racer/src/store/checkpoint.rs:45`)
  live in `RACER_SLAB_DIRECTORY` even in raw-device mode
  (`docs/content/reference/racer.md:612-614`); they are published without fsync
  (`cmd/racer/io/src/fs/checkpoint.rs:161-162`). A changed layout rejects recovery
  and starts cold (`docs/content/reference/racer.md:641-645`).
- Hard holds: `MAX_HOLD_TTL` = 30 days (`cmd/racer/src/store/holds.rs:15`); each
  held page charges one whole segment (`holds.rs:1`); holds are reader counts
  per page, no holder kinds remain (`holds.rs:101-110`). Held segments are not
  evictable (`catalog.rs:1477-1480`). Hold capacity is a percent of pages
  (`catalog.rs:222-232`), from `RACER_RETAINED_SEGMENT_PERCENT`
  (`cmd/racer/src/config.rs:418`).
- The writer queue serves `CopyOnly` from pending (queued, not yet written)
  pages (`store.rs:1126-1133`).

### Encryption

- `CryptoClient::encrypt(page, page_size, plaintext, charge, scope)` encrypts
  once and the ciphertext is reused verbatim for peers and disk
  (`cmd/racer/src/security.rs:495-503`).
- AAD domain `racer/page/aead/v2`, binding cache UID, ETag, page number, page
  size, and lengths (`security.rs:192-230`, `page_aad` at `security.rs:255`).
  XAES-256-GCM (`cmd/racer/crypto/src/lib.rs:120`).
- Key rotation keeps seven readable page keys; older keys are dropped
  (`docs/content/reference/racer.md:1381-1383`, `RACER_RETAINED_PAGE_KEYS` at
  `racer.md:524`). Long-lived pinned pages must therefore be re-encrypted before
  their key ages out (see [WP8](#wp8-repair-rekey-and-garbage-collection)).

### Origin

- `trait Origin` has `bootstrap_reserved`, `page_reserved`, `metadata`,
  `page_size`; reads only (`cmd/racer/src/origin.rs:50-80`).
- Azure request heads: `AzureMethod::{Head, Get}` (`cmd/racer/src/origin/azure/request.rs:22-27`),
  `head(...)` builds `x-ms-version`, `x-ms-date`, optional `x-ms-range`,
  `If-Match`, and `Authorization: Bearer` (`request.rs:120-170`); service version
  `2026-12-06` (`request.rs:18`). Credentials are bearer tokens
  (`cmd/racer/src/origin/azure.rs:59`, `cmd/racer/src/origin/azure_auth.rs:468`).
- UDS origin adapter: `request(context, method)` (`origin.rs:458`); Go SDK
  `ServeOrigin` (`pkg/racersdk/origin.go:173`) with
  `type Origin func(ctx, OriginRequest) (Metadata, io.ReadCloser, error)`
  (`origin.go:47`).
- racer-object rejects anything but GET/HEAD
  (`internal/racerobject/racerobject.go:137-139`).

### Control plane

- `CacheDefinition { id, name, client_socket, origin_socket, priority, mode, origin }`
  (`cmd/racer/wire/src/control.rs:584-602`). The wire DTO is
  `#[serde(deny_unknown_fields)]` (`control.rs:1343-1345`); optional fields are
  omitted when absent so canonical bytes are unchanged (`control.rs:1357-1363`).
- The authority maps ClusterCache JSON to definitions in
  `cmd/racer/authority/src/rules/catalog.rs` (`CacheInfo::from_json` at `:28`,
  `mode` at `:58`, `origin_definition` at `:74`, `build` at `:230`).
- CRD: `ClusterCacheSpec { Origin, Priority, Passthrough, NodeSelector }`
  (`api/racer/v1alpha1/clustercache_types.go:33-77`). Generation: `make racer-generate`.

### Clients

- Python: `Client.stat`, `read`, `host_read`, `gpu_read`, prefetch methods
  (`clients/python/racer/src/racer/client.py:374`, `:622`, `:693`, `:708`, `:398`);
  keys in `clients/python/racer/src/racer/keys.py:59-86`; tests and fakes in
  `clients/python/racer/tests/` (`fakeserver.py`, `test_client.py`,
  `test_wire_golden.py`).
- Go: `Client.Stat`, `Get` (`pkg/racersdk/client.go:221`, `:328`), `HostRead`
  sends the head with `SCM_RIGHTS` (`pkg/racersdk/hostread.go:64`, `:208-228`),
  `Request { Key, Metadata, Authorization }` (`pkg/racersdk/types.go:56-63`),
  test server under `pkg/racersdk/racersdktest`.

### Simulation and tests

- Whole-application DST on the simulated OS: `cmd/racer/src/app/dst.rs`
  (`run` at `:312`), with fault actions in `cmd/racer/src/app/dst/faults.rs`
  including `disk_corruption` (`:235`), `crash_inflight` (`:486`),
  `crash_pending_write` (`:512`), `key_retirement` (`:544`). Its oracle "owns
  immutable origin versions" (`dst.rs:1-2`).
- Model DST (`racer-dst` binary): scenarios in `cmd/racer/dst/src/scenarios.rs`
  (`catalog` at `:983`), invariants in `cmd/racer/dst/src/invariants.rs`, seeds
  in `cmd/racer/dst/seeds/{ci,regressions}.txt`.
- Integration tests: `cmd/racer/tests/` (for example `process_restart.rs`,
  `production_dataplane.rs`, `client_origin_conformance.rs`).
- Kind e2e: `e2e/racer/` (`racer_e2e_test.go`, `harness_e2e.go`).
- Fuzz targets: `http_head small_body peer_envelope control_documents store_record checkpoint`
  (`Makefile:624`).

## Prior art

| System | What it does | What we take |
|--------|-------------|-------------|
| Alluxio `ASYNC_THROUGH` + `replication.durable`; Alluxio AI `WRITE_BACK` with `writeReplicas: 2` | Acks after in-cache replicas, uploads later; pins unpersisted data with a cap (ratio 0.3), out-of-space backpressure, orphan cleanup after 1 h | Two copies before ack by default; dirty cap with backpressure; orphan GC by timeout |
| JuiceFS `--writeback` | Local staging only; data lost if the node is lost; upload delay lets deleted files skip upload | Drop superseded/deleted data without upload; but we refuse single-node durability by default |
| HDFS pipeline, `LAZY_PERSIST` | Replica pipeline, RAM tier persisted lazily | Owner set fixed by placement; lazy persist is our `auto` upload |
| SageMaker HyperPod managed tiered checkpointing | RAM + peer replication over RDMA, S3 every Nth step, DCP `StorageWriter` | Peer replicas over existing RDMA transport; DCP plug-in that persists every Nth checkpoint |
| GKE multi-tier checkpointing + Orbax emergency checkpointing | RAM disk + peer + GCS, restore from "last known good complete checkpoint" | DCP `.metadata` written last; reader treats a checkpoint without metadata as incomplete |
| NVIDIA nvidia-resiliency-ext local checkpointing (`replication_factor=2`); Gemini (SOSP '23, m=2 group placement); MegaScale; ByteCheckpoint | Two in-cluster copies, placement-aware | `replicas=2` default, placement by rendezvous ranking |
| PyTorch DCP | `.metadata` written last via tmp + rename; pluggable `StorageWriter`; `async_save` | Plug-in writes data files then `.metadata`; create-once replaces rename |
| 3FS | CRAQ chain replication, receivers pull via RDMA READ | Receivers pull; requests stay body-less |
| Mooncake store | `PutStart`/`PutEnd`, client fan-out, discard timeouts | Session open/commit with lease and discard on expiry |
| Azure Blob Put Block / Put Block List | Up to 50,000 uncommitted blocks per blob; uncommitted blocks GC after 7 days; all block IDs of a blob must have the same length; account ingress limits (about 60 Gbps) | One block per page (16 MiB pages cap objects at 50,000 x 16 MiB = 781.25 GiB); fixed-length block IDs; conditional commit; per-cache rate limit |
| AMLFS auto-export, Mountpoint for S3 | Asynchronous export; sequential create-only writes | Create-once objects, asynchronous export |

Common pattern: two copies before ack for checkpoint tiers, pin dirty data with a
cap and backpressure, write the commit marker or manifest last, decouple upload
frequency from save frequency, and garbage-collect orphans by timeout.

## Decisions

These were made by the user and are binding unless a section below says why the
code forced a refinement.

1. Default acknowledgment is 2 durable owner copies. `Racer-Durability:
   replicas=N` with `N` in `1..=3` (3 is `REPLICAS`, `placement.rs:10`), or
   `origin` to ack after the Blob commit. The writer counts as a copy only when
   it is one of the ranked store targets for that page.
2. Cache-only checkpoints: `Racer-Upload: auto|manual|never`. `manual` and
   `never` objects are pinned with a TTL bounded by `MAX_HOLD_TTL`.
   `POST /v1/objects/<key>/persist` uploads a `manual` object. Objects deleted
   (or whose session is aborted) before upload are dropped without upload.
3. Create-once with `If-None-Match: *` semantics; existing key is `412`.
   `DELETE /v1/objects/<key>` removes a cache-resident written object.
4. Azure Blob first: Put Block with fixed-length block IDs derived from version
   and page, `x-ms-content-crc64`, Put Block List with `If-None-Match: *` by the
   coordinator, uploads spread across holders, per-cache rate limit. UDS adapter
   and racer-object writes are later WPs.
5. Python and Go write APIs (`write`, `host_write` via `SCM_RIGHTS`, Go `Put`,
   `HostWrite`) and a DCP `StorageWriter`/`StorageReader` that writes data files
   with `replicas=2`, `.metadata` last, and persists every Nth checkpoint.

### Refinements forced by the code

- **Write segments instead of per-page hard holds.** A hard hold charges one
  whole segment per page (`holds.rs:1`); with 16 MiB pages that would charge a
  64 MiB segment per page. Written pages are instead appended to dedicated
  *write segments* drawn from the same slab, and the whole segment is
  non-evictable while it holds any live written record. Eviction already skips
  held segments (`catalog.rs:1477-1480`); WP3 extends that predicate.
- **Durability needs new fsync calls.** Nothing in the store fsyncs today
  (`slab.rs:276-284`, `store.rs:783-786`). WP3 adds `fdatasync` on the slab
  descriptor (file mode) or the device descriptor (raw mode) through
  `Reactor::fsync` (`ops.rs:1100`), followed by the journal `fdatasync`.
- **The journal is authoritative, not the checkpoint.** Checkpoints are not
  fsynced and a layout change starts cold (`checkpoint.rs` publish without sync,
  `racer.md:641-645`). Recovery replays the journal *before* the allocator may
  reuse segments, even on a cold start with compatible geometry.
- **No CRC in records.** Journal records use a SHA-256 trailer, matching the
  store's header digest (`store.rs:2001`). Azure `x-ms-content-crc64` needs a
  CRC-64 implementation; WP6 adds a small table-driven one (no new dependency).
- **Key rotation.** Pinned pages can outlive the seven retained page keys
  (`racer.md:1381-1383`). WP8 re-encrypts pinned written pages whose key is
  about to age out.
- **Wire compatibility.** The catalog DTO denies unknown fields
  (`control.rs:1343-1345`), so a node that predates `writes` rejects a catalog
  that contains it. `writes` is omitted when unset, and enabling it requires
  every dataplane in the cluster to run a version with this feature (documented
  precondition, checked by the operator; see WP7).
- **Disabled writes look like today.** When a cache does not enable writes, or
  is passthrough, `PUT`/`DELETE`/`persist`/`host-write` return the same `405`
  with `Allow: HEAD, POST` as today (`client.rs:539-543`). Clients cannot tell an
  old node from a write-disabled cache, which is intended.

## Client API

All requests use HTTP/1.1 on `/run/racer/<cache>/client/socket` with
`Host: racer`, like reads. `<key>` is the 64-hex `CacheKey`.

| Method | Path | Purpose |
|--------|------|---------|
| `PUT` | `/v1/objects/<key>` | Create an object from the request body |
| `POST` | `/v1/objects/<key>/host-write` | Create an object from a client file descriptor |
| `POST` | `/v1/objects/<key>/persist` | Start upload of a `manual` object |
| `DELETE` | `/v1/objects/<key>` | Remove a cache-resident written object |
| `HEAD` | `/v1/objects/<key>` | Unchanged, plus `Racer-Upload-State` for written objects |

When writes are enabled, `405` responses on `/v1/objects/<key>` carry
`Allow: HEAD, POST, PUT, DELETE`.

### Request headers

| Header | Applies to | Rule |
|--------|-----------|------|
| `Content-Length` | `PUT` | Required, decimal, `1..=max_object_bytes` where `max_object_bytes = 50_000 * page_size`. Missing: `411`. Too large: `413`. Zero-length objects are rejected with `400` in v1 (the prefetch page model already requires a nonempty object, `protocol.rs:336-341`, and writes keep that rule). |
| `If-None-Match` | `PUT`, `host-write` | Required, exactly `*`. Anything else, or absent: `428 Precondition Required`. |
| `Racer-Metadata` | `PUT`, `host-write` | Required when the upload mode is not `never`; opaque bytes, validated like reads (`validate_opaque`, `client.rs:289-292`). Must identify the object without a version ID (Azure: `{"schema":1,"bucket","key"}`); a `versionId` is `400`. |
| `Authorization` | all | Optional, as for reads; carried as encrypted origin context. |
| `Racer-Durability` | `PUT`, `host-write` | `replicas=1`, `replicas=2`, `replicas=3`, or `origin`. Default `replicas=<spec.writes.replicas>`. `origin` with `Racer-Upload: never|manual` is `400`. |
| `Racer-Upload` | `PUT`, `host-write` | `auto`, `manual`, `never`. Default `spec.writes.upload.mode`. A cache without an origin accepts only `never`. |
| `Racer-Hold-Seconds` | `PUT`, `host-write` | Decimal `1..=2_592_000` (`MAX_HOLD_TTL`). Default `spec.writes.holdTTL`. For `auto`, the hold lasts until upload commits or the TTL ends, whichever comes first. |
| `Racer-Content-Type` | `PUT`, `host-write` | Optional, stored like origin content types (`model.rs:383-385`). |
| `Racer-Source-Offset`, `Racer-Source-Length` | `host-write` | Required, decimal; window of the passed descriptor. `Racer-Source-Length` is the object length; `Content-Length` must be `0`. |

`Expect: 100-continue` stays rejected. Clients send the body immediately.

### Responses

| Status | When | Headers |
|--------|------|---------|
| `201 Created` | Durable at the requested level and visible to readers | `ETag: "racer-<32 lowercase hex>"`, `Racer-Upload-State`, `Content-Length: 0` |
| `204 No Content` | `DELETE` or `persist` accepted | `Racer-Upload-State` for `persist` |
| `400` | Malformed headers | |
| `404` | `DELETE`/`persist` of a key with no written object | |
| `405` | Writes disabled, passthrough cache, or wrong method | `Allow` |
| `409` | `persist` on `never`/`committed`; `DELETE` of an uploaded object | `Racer-Write-Error` |
| `411`, `413`, `428` | See above | |
| `412` | Key exists in the write directory or at the origin, or Put Block List found an existing blob (`origin` durability) | |
| `502` | Upload failed under `origin` durability | `Racer-Write-Error: upload` |
| `503` | Dirty cap reached, too few reachable owners, coordinator unavailable | `Retry-After` (seconds), `Racer-Write-Error` |
| `504` | Request deadline elapsed before durability | |

`Racer-Write-Error` is a diagnostic token: `dirty-cap`, `owners`, `coordinator`,
`lease`, `upload`, `uploaded`, `conflict`, `not-manual`. Bodies stay empty.

`Racer-Upload-State` values: `pending` (waiting to upload), `uploading`,
`committed`, `failed`, `never` (cache-only), `manual` (waiting for `persist`).

On `HEAD`, a written object returns its Racer version `ETag`, `Content-Length`,
`Racer-Expires-At` (the hold deadline, or the cache's normal metadata TTL after
upload), and `Racer-Upload-State`. After upload commits, a fresh `HEAD` still
returns the Racer `ETag` while the alias is known (see
[Visibility and aliasing](#visibility-and-aliasing)).

### Body streaming

The listener reads the body one page at a time into a plaintext buffer charged
to a new admission class `ResourceClass::WritePlaintext` (sized one page per
in-flight page). `read_body_exact` (`connection.rs:1030`) fills each page; the
page is encrypted with `CryptoClient::encrypt` (`security.rs:496`) and moved to
the write staging table. A short body is `400` and aborts the session. The
writer keeps at most `RACER_WRITE_WINDOW_PAGES` (default 8) pages in flight per
request; the socket is not read while the window is full, so TCP backpressure
paces the client.

`host-write` maps the client descriptor read-only through the same mechanism as
`host-read` (`cmd/racer/src/client/direct/host_map.rs`) and reads pages from
`[source_offset, source_offset + source_length)`. The descriptor rules mirror
host-read (regular file or memfd; see `pkg/racersdk/hostread.go:57`).

## Architecture

### Roles

- **Writer**: the node whose client socket received the PUT. Mints the version,
  encrypts pages, stages ciphertext in memory, drives the session.
- **Store target** (for a page): the first `N` reachable members of
  `Placement::rank(membership, object, page)` (`topology.rs:734`), scanning at
  most `RANKED_DEPTH` (`placement.rs:18`) members. `N` is the request's
  replicas. Reads already consult demoted members (`placement.rs:12-17`), so a
  copy on rank 3 or 4 is still found.
- **Holder**: a store target that acknowledged a durable copy.
- **Coordinator**: the first reachable member of the page-zero ranking
  (`rank(membership, object, PageNumber(0))`). It owns the write session, the
  manifest, create-once checks, visibility, upload, and repair. Its manifest is
  replicated to the next `N - 1` reachable page-zero ranked members (the
  *manifest replicas*), so a coordinator loss is survivable at the same
  durability level as the data.
- Inside a node, the worker that owns a page is `WorkerMap::owner(page)`, and
  the coordinator worker is `metadata_owner(object)` (`dispatch.rs:1413-1418`).

### Version minting

The writer mints `ETag = "racer-" + hex(16 random bytes)`, quoted, using the
worker context RNG (`Signatures::ctx`, `protocol.rs:117-120`;
`ctx().rng.fill` as in `metadata.rs:509-514`). The value is a valid
`StrongEtag` (`model.rs:77`). Because the AAD binds the ETag
(`security.rs:208`), ciphertext of one version can never be confused with
another version of the same key. Placement does not include the version
(`topology.rs:672`), so written pages land exactly where a later origin fill of
the same key would.

### Write flow

```
client          writer W                coordinator C            store target S (each page, N of them)
  | PUT ------->  |                        |                          |
  |               | WriteSession(open) --> | check create-once,       |
  |               |                        | journal Open, fsync      |
  |               | <-- Opened(lease) -----|                          |
  |  body page i  |                        |                          |
  | ------------> | encrypt, stage         |                          |
  |               | WriteStore(page i) ---------------------------->  |
  |               |                        |   Page CopyOnly (pull) <-|
  |               | ---- ciphertext (TCP or RDMA write) ------------> |
  |               |                        |   verify, write segment, |
  |               |                        |   fdatasync, journal     |
  |               | <-------------------------------- Stored(page i)  |
  |               | (repeat; window of 8 pages)                       |
  |               | WriteSession(commit, holders) -> journal Commit,  |
  |               |                        | replicate manifest,      |
  |               |                        | publish current version  |
  |               | <-- Committed ---------|                          |
  | <-- 201 ETag  |                        |                          |
```

1. **Open.** W sends `WriteSession{action: open}` to C with the version, length,
   page size, content type, replicas, upload mode, hold deadline, and the
   request's origin context (`PeerOriginContext`, carried like other `Acquire`
   requests, `protocol.rs:621-644`). C rejects with `412` if its write directory
   has a live or committed session for the object, or, when the cache has an
   origin, if an origin `HEAD` (via the existing `Origin::metadata`, fresh
   selector) finds the object. C journals an `Open` record (fsync) and returns a
   lease (`RACER_WRITE_LEASE_SECS`, default 60).
2. **Pages.** For each page, W encrypts and inserts the ciphertext into the
   write staging table, then sends `WriteStore` to each store target in
   parallel. If W is itself a store target, it persists locally through the same
   code path without a network hop.
3. **Store.** Each target pulls the page with a directed `Page`/`CopyOnly`
   request to W, verifies it (AEAD verify through the existing receive path),
   appends it to a write segment, makes it durable, journals a `Page` record,
   and replies `Stored`. A target that cannot store (dirty cap, disk error)
   replies with a reason; W retries the page on the next ranked member within
   `RANKED_DEPTH`, and fails the request with `503` when it cannot reach `N`.
4. **Renew.** W renews the lease every `lease / 3` while the body streams.
5. **Commit.** After every page has `N` holders, W sends `WriteSession{commit}`
   with the holder list per page. C journals `Commit` (fsync), replicates the
   manifest to `N - 1` manifest replicas (each journals and fsyncs), publishes
   the version descriptor and current pointer (see below), and replies.
6. **Ack.** For `replicas=N`, W returns `201`. For `origin`, W waits (long poll
   with `WriteSession{status, wait}`) until C reports `committed` or `failed`.
7. **Abort.** Any failure before commit sends `WriteSession{abort}`; C journals
   `Abort` and sends `WriteRelease` to every target that might hold pages.
   Lease expiry has the same effect.

### Staging on the writer

Ciphertext waits in a per-worker `WriteStaging` table keyed by `PageId` until
all `N` targets ack or the session ends. The `CopyOnly` serving path must find
staged pages: `cached_page` (`dispatch.rs:292`) gains a staging lookup before
its existing cache checks. Staged bytes are charged to a new admission class
`ResourceClass::WriteStaging`. The staging table never writes to disk; only
store targets do. This keeps "requests carry no page data"
(`protocol.rs:1-3`) and uses the receiver-paced pull path, including RDMA
(inference: directed `CopyOnly` page responses use the same transport selection
as other page responses in `cmd/racer/src/peer/transport.rs`; WP5 must confirm
with an RDMA-enabled test).

### Durable storage on holders

- **Write segments.** A worker allocates segments from its slab for written
  records only. A segment with any live written record is non-evictable
  (`can_evict`, `catalog.rs:1477`, gains `&& !state.writes.segment_live(segment)`).
  Records use the existing `RCRPAGE1` format (`store.rs:1999`), so reads of
  written pages use the unchanged read path. Write segments are compacted only
  when fully dead (all records released), then returned to the free list.
- **Durability barrier.** After the record write completes, the holder issues
  `fdatasync` on the slab descriptor (file mode) or the device descriptor (raw
  mode). Linux `fdatasync` on a block device flushes the device write cache
  (FUA/flush), so raw mode needs no separate FUA path.
- **Journal.** Then it appends a journal record and `fdatasync`s the journal.
  Barriers are group-committed: one pair of fsyncs per worker covers all records
  completed since the last barrier, bounded by `RACER_WRITE_GROUP_COMMIT_MICROS`
  (default 500) and 64 records.
- **Dirty cap.** Live written bytes per cache per worker are capped at
  `spec.writes.maxDirtyPercent` of that worker's segment capacity (default 30).
  A store target over its cap replies `Full`; the writer tries the next ranked
  member; a writer that cannot place a page returns `503` with
  `Retry-After: 1` and `Racer-Write-Error: dirty-cap`.
- **Recovery.** At startup, before the allocator hands out segments, each
  worker replays its journal: it re-reads each live page record's header to
  verify the digest and generation, re-inserts it into the catalog, and marks
  its segment live. Records that fail verification are reported to the
  coordinator as lost (repair handles them). This runs whether the checkpoint
  was accepted or rejected; if the geometry changed (different devices or
  worker count), the journal's binding check fails, all its pages are reported
  lost, and the journal is truncated.

### Visibility and aliasing

- C publishes the version descriptor with `publish_version` (`catalog.rs:1040`)
  and the current pointer with `publish_current` (`catalog.rs:1074`), with
  `expires_at` equal to the hold deadline. Manifest replicas do the same.
- Fresh metadata refresh on the page-zero owner consults the write directory
  first: `refresh_origin` (`metadata.rs:707`) gains a check before calling the
  origin. While a written object exists in the directory (any state except
  deleted), the refresh returns the Racer version without contacting the
  origin. This is required because the origin returns `404` before upload.
- Pinned reads (`If-Match: "racer-..."`) work unchanged once pages are cached:
  a pinned origin miss already probes copy-only candidates
  (`metadata.rs:732-744`).
- After Put Block List succeeds, C records the Blob ETag as an *alias* of the
  Racer version in the manifest. If later the directory entry is gone (released
  after upload and evicted), a fresh refresh goes to the origin, sees the Blob
  ETag, and finds the alias in C's directory (aliases are retained for
  `RACER_WRITE_ALIAS_SECS`, default 7 days, then dropped). With an alias, the
  refresh publishes the Racer version so cached pages are reused. Without one,
  it is an ordinary miss and pages refill from the origin under the Blob ETag.

### Upload

- Eligibility: `auto` objects after commit; `manual` objects after `persist`;
  never `never` objects.
- For each page, C picks the first holder in rank order that is reachable, and
  sends `WriteUpload{pages}`, batching contiguous pages per holder. The holder
  decrypts the page and sends Put Block:
  `PUT <blob>?comp=block&blockid=<url-escaped base64 block ID>` with
  `Content-Length`, `x-ms-content-crc64`, `x-ms-version`, `x-ms-date`,
  `Authorization: Bearer`. It replies `Uploaded` or a failure.
- Block ID: the 48 ASCII bytes `"{version_hex32}{page:016x}"`, base64 encoded
  (64 characters). All blocks of a blob have the same ID length, as Azure
  requires.
- When every page is uploaded, C sends Put Block List
  (`PUT <blob>?comp=blocklist` with `If-None-Match: *` and the XML list
  `<BlockList><Latest>id</Latest>...</BlockList>`, plus
  `x-ms-blob-content-type` when a content type is set). `201` commits; `409`
  `BlobAlreadyExists` or `412` marks the object `failed` with
  `Racer-Write-Error: conflict` (another writer won); other errors retry with
  backoff up to `RACER_WRITE_UPLOAD_ATTEMPTS` (default 10), then `failed`.
- Rate limiting: each node limits upload bytes per cache to
  `spec.writes.upload.maxBytesPerSecond` (token bucket; `0` means unlimited)
  and concurrent Put Blocks per cache to `spec.writes.upload.maxConcurrentBlocks`
  (default 16). Limits are per node; the documented cluster-wide rate is the
  per-node limit times the pool size.
- After commit, holders release their write pins; pages stay in the cache as
  ordinary cached pages under the Racer version until evicted.
- Deleted or aborted objects are never uploaded. A `DELETE` during upload stops
  scheduling and skips Put Block List; uncommitted blocks are left for Azure's
  7-day garbage collection.
- `failed` objects stay pinned until their hold deadline so an operator or
  client can read them back; HEAD reports `failed`.

### Repair

C watches membership changes (the same membership publication the read path
uses). For each committed, not-yet-uploaded page whose holder set lost a member
that left the membership (or reported a lost record), C picks the next ranked
member not already holding it and sends `WriteStore` with `source` set to a
surviving holder. A page with zero surviving holders before upload marks the
object `failed` (`Racer-Write-Error: lost`). If C itself leaves, the first
surviving manifest replica in page-zero rank order becomes C (it already has
the manifest) and re-replicates the manifest to restore `N` copies.

### Deletion and expiry

- `DELETE` goes to C. C journals `Delete`, sends `WriteRelease` to every holder,
  removes the current pointer (`invalidate` the catalog entry), and replies
  `204`. A committed upload returns `409 uploaded` (Racer does not delete
  origin objects).
- Hold expiry: C journals `Expire` and releases holders. For `auto` objects,
  expiry before upload completion still uploads if the upload already started;
  otherwise the object is dropped and `failed` is not reported (it is gone).
- Orphans: a holder with a journaled page whose session C does not know after
  `RACER_WRITE_ORPHAN_SECS` (default 3600) asks C (`WriteSession{status}`);
  `unknown` releases the page.

## Wire formats

### Peer operations (WP4)

All are `POST /racer/peer/v1`, `content-length: 0`, signed and routed like
existing operations. New values for `racer-operation` and new headers:

| `racer-operation` | Rust variant | Mode | Headers |
|-------------------|--------------|------|---------|
| `write-session` | `Operation::WriteSession { version, action }` | `acquire` (carries origin context) | `racer-write-action` = `open` \| `renew` \| `commit` \| `abort` \| `status` \| `delete` \| `persist` \| `replicate` \| `release-alias`; per action below |
| `write-store` | `Operation::WriteStore { version, store }` | `copy` | `racer-write-pages` (same encoding as `racer-prefetch-pages`, `protocol.rs:574-576`), `racer-object-length`, `racer-page-size`, optional `racer-content-type`, `racer-write-source` (node ID), `racer-write-session` (base64, 16 bytes), `racer-write-hold-until` (Unix millis) |
| `write-release` | `Operation::WriteRelease { version, pages }` | `copy` | `racer-write-pages`, `racer-write-session` |
| `write-upload` | `Operation::WriteUpload { version, pages }` | `acquire` | `racer-write-pages`, `racer-write-session` |

`write-session` action fields:

- `open`: `racer-object-length`, `racer-page-size`, optional
  `racer-content-type`, `racer-write-replicas` (1..3),
  `racer-write-upload` (`auto|manual|never`),
  `racer-write-durability` (`replicas|origin`), `racer-write-hold-until`,
  `racer-write-session` (writer-chosen 16 random bytes, base64).
- `renew`, `abort`, `persist`, `delete`: `racer-write-session` (for `delete`
  and `persist` the session is the one returned by `status`; clients do not
  see it).
- `commit`: `racer-write-session`, `racer-write-holders`: base64 of the binary
  holder table: for each page in order, `u8 count`, then `count` entries of
  `u16 BE length` + node ID bytes.
- `status`: optional `racer-write-wait-millis` (0..=30000).
- `replicate`: C to manifest replicas; `racer-write-manifest`: base64 of the
  `Manifest` encoding (journal record body of kind `Commit`, below).

Responses use a new `PeerResponse::Write(racer_core::write::WriteReply)`
variant (`core/src/read/mod.rs:144`) encoded with `racer-outcome: write` and:

- `racer-write-result`: `opened` \| `stored` \| `committed` \| `released` \|
  `uploaded` \| `status` \| `exists` \| `full` \| `lease-expired` \|
  `unknown` \| `failed`.
- `racer-write-lease-until` (millis) for `opened` and `renew`.
- `racer-write-upload-state` for `committed` and `status`.
- `racer-write-stored`: page list (prefetch page encoding) for `stored` and
  `uploaded`.

Unknown operations are rejected by older receivers; the sender treats that
like `Unavailable` for that target, so a mixed-version pool degrades to
`503 owners` rather than silent loss.

### Racer core types (WP2)

New pure module `cmd/racer/core/src/write/` (`mod.rs`, `session.rs`,
`journal.rs`, `upload.rs`, `holders.rs`). racer-core is pure
(`hack/scripts/racer-purity-graph.py:16`); no IO types.

```rust
pub enum UploadMode { Auto, Manual, Never }
pub enum Durability { Replicas(u8 /* 1..=3 */), Origin }
pub enum UploadState { Pending, Manual, Uploading, Committed, Failed, Never }
pub struct SessionId(pub [u8; 16]);

pub struct WriteSpec {
    pub length: u64,
    pub page_size: u64,
    pub content_type: Option<String>,
    pub replicas: u8,          // copies required before commit
    pub durability: Durability,
    pub upload: UploadMode,
    pub hold_until_millis: u64,
}

pub enum WriteAction {
    Open { session: SessionId, spec: WriteSpec },
    Renew { session: SessionId },
    Commit { session: SessionId, holders: Holders },
    Abort { session: SessionId },
    Status { wait_millis: u32 },
    Delete,
    Persist,
    Replicate { manifest: Manifest },
}

pub struct Holders(pub Vec<Vec<NodeIdBytes>>); // index = page number
pub struct Manifest {
    pub session: SessionId,
    pub etag: String,
    pub spec: WriteSpec,
    pub holders: Holders,
    pub state: UploadState,
    pub blob_etag: Option<String>,
}

pub enum WriteReply {
    Opened { lease_until_millis: u64 },
    Stored { pages: Vec<u64> },
    Committed { state: UploadState },
    Released,
    Uploaded { pages: Vec<u64> },
    Status { state: UploadState, etag: String },
    Exists, Full, LeaseExpired, Unknown, Failed,
}

/// Fixed-length block ID: base64("{version_hex32}{page:016x}").
pub fn block_id(version_hex: &[u8; 32], page: u64) -> [u8; 64];

/// Coordinator state machine. Inputs are events, outputs are effects; no IO.
pub struct Session { /* ... */ }
impl Session {
    pub fn open(spec: WriteSpec, now_millis: u64, lease_millis: u64) -> Self;
    pub fn on_event(&mut self, event: SessionEvent, now_millis: u64) -> Vec<SessionEffect>;
}
pub enum SessionEvent { Renewed, Committed(Holders), Aborted, LeaseTick, HolderLost { page: u64, node: NodeIdBytes },
    PageUploaded(u64), BlockListCommitted { blob_etag: String }, BlockListConflict, UploadFailed,
    PersistRequested, DeleteRequested, HoldExpired }
pub enum SessionEffect { Journal(JournalRecord), Release { nodes: Vec<NodeIdBytes> },
    Upload { node: NodeIdBytes, pages: Vec<u64> }, CommitBlockList, Restore { page: u64, from: NodeIdBytes },
    Publish, Unpublish, Notify(UploadState) }
```

### Journal record format (WP2 codec, WP3 IO)

File: `<RACER_SLAB_DIRECTORY>/writes/worker-<id>.journal`, one per worker,
opened `O_WRONLY | O_APPEND | O_CREAT | O_CLOEXEC`. The directory is created and
fsynced once.

File header (64 bytes, written once, fsynced with the directory):
`magic b"RCRWJNL1"` | `u32 BE format = 1` | `u16 BE worker` | 2 zero bytes |
`32-byte layout digest` (the same layout digest checkpoints use,
`checkpoint.rs:96`) | 16 zero bytes.

Records, appended back to back:

```
u32 BE body_length | u8 kind | body (body_length bytes) | 32-byte SHA-256 over (kind || body)
```

Kinds and bodies (integers big-endian, strings `u16 BE length` + bytes, node
IDs as strings, cache ID as string, key as 32 bytes, etag as string):

| Kind | Name | Body |
|------|------|------|
| 1 | `Page` | cache, key, etag, `u64 page`, `u64 segment`, `u64 generation`, `u64 offset`, `u32 length`, `SessionId`, `u64 hold_until_millis` |
| 2 | `PageRelease` | cache, key, etag, `u64 page` |
| 3 | `Open` | cache, key, etag, `SessionId`, `WriteSpec` (fields in struct order; content type as optional string with a `u8` presence byte), `u64 lease_until_millis` |
| 4 | `Commit` | cache, key, `Manifest` (fields in struct order) |
| 5 | `State` | cache, key, etag, `u8 UploadState`, optional blob etag |
| 6 | `Abort` | cache, key, etag |
| 7 | `Delete` | cache, key, etag |
| 8 | `Checkpoint` | `u64 sequence`; marks a compaction point (see below) |

Replay stops at the first record whose length runs past end of file or whose
digest does not match (a torn tail); the file is truncated there. Compaction:
when the journal exceeds `RACER_WRITE_JOURNAL_BYTES` (default 64 MiB), the
worker writes the live state to `worker-<id>.journal.tmp`, fsyncs it, renames it
over the journal, and fsyncs the directory.

### Client wire (WP1)

New pure module `cmd/racer/wire/src/object/write.rs`:

```rust
pub enum WriteKind {
    Put { content_length: u64 },
    HostWrite { source: Destination },        // offset/length of the source window
    Persist,
    Delete,
}
pub struct WriteRequest<'a> {
    pub kind: WriteKind,
    pub key: CacheKey,
    pub metadata: Option<&'a [u8]>,
    pub authorization: Option<&'a [u8]>,
    pub content_type: Option<ContentType>,
    pub durability: Option<Durability>,       // None = cache default
    pub upload: Option<UploadMode>,           // None = cache default
    pub hold_seconds: Option<u64>,
}
pub fn is_write(head: &MessageHead) -> bool;  // PUT/DELETE on /v1/objects/<key>, POST .../host-write|persist
pub fn parse_write<'a>(head: &'a MessageHead, header_limit: usize) -> Result<WriteRequest<'a>>;
pub fn write_created_head(etag: &StrongEtag, state: UploadState) -> MessageHead;
pub fn write_error_head(status: u16, error: Option<&str>, retry_after: Option<u32>) -> MessageHead;
pub fn success_head_with_upload(metadata: &ObjectMetadata, state: Option<UploadState>) -> Result<MessageHead>;
```

`Durability` and `UploadMode` are re-exported from `racer_core::write` or
defined in racer-wire and re-used by racer-core (racer-core already depends on
racer-wire for `ObjectMetadata`; WP1 defines them in
`wire/src/object/write.rs`, WP2 imports them).

### Control plane (WP7)

CRD (`api/racer/v1alpha1/clustercache_types.go`):

```go
// Writes enables object writes for this cache.
// +optional
Writes *Writes `json:"writes,omitempty"`

type Writes struct {
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=3
    // +kubebuilder:default=2
    Replicas *int32 `json:"replicas,omitempty"`
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=90
    // +kubebuilder:default=30
    MaxDirtyPercent *int32 `json:"maxDirtyPercent,omitempty"`
    // Default hold for written objects, Go duration, at most 720h.
    // +kubebuilder:default="24h"
    HoldTTL *metav1.Duration `json:"holdTTL,omitempty"`
    // +optional
    Upload *WriteUpload `json:"upload,omitempty"`
}
type WriteUpload struct {
    // +kubebuilder:validation:Enum=Auto;Manual;Never
    // +kubebuilder:default=Auto
    Mode *string `json:"mode,omitempty"`
    // 0 means unlimited.
    MaxBytesPerSecond *int64 `json:"maxBytesPerSecond,omitempty"`
    // +kubebuilder:default=16
    MaxConcurrentBlocks *int32 `json:"maxConcurrentBlocks,omitempty"`
}
```

Validation (CEL on the spec): `writes` with `passthrough` is invalid;
`upload.mode` other than `Never` requires `origin` (UDS-origin writes are a later
WP).

Wire (`cmd/racer/wire/src/control.rs`): `CacheDefinition.writes:
Option<WriteDefinition>` with

```rust
pub struct WriteDefinition {
    pub replicas: u8,
    pub max_dirty_percent: u8,
    pub hold_ttl: Duration,
    pub upload_mode: UploadMode,
    pub upload_max_bytes_per_second: u64,
    pub upload_max_concurrent_blocks: u32,
}
```

DTO field `writes` with `#[serde(default, skip_serializing_if = "Option::is_none")]`
and keys `replicas`, `maxDirtyPercent`, `holdTTLMillis`, `uploadMode`
(`auto|manual|never`), `uploadMaxBytesPerSecond`, `uploadMaxConcurrentBlocks`.
Authority mapping in `authority/src/rules/catalog.rs` (`CacheInfo` gains
`writes: Option<Value>`; a `write_definition` function mirrors
`origin_definition` at `catalog.rs:74`; an invalid `writes` fails the cache like
an invalid origin).

Node settings (`cmd/racer/src/config.rs`; the entries live in
`config/table.rs`, with a new `Writes` group and `Micros` unit in
`config/spec.rs`), parsed into `Config::writes: WriteSettings`:
`RACER_WRITE_WINDOW_PAGES` (8, 1..64), `RACER_WRITE_LEASE_SECS` (60, 5..3600),
`RACER_WRITE_GROUP_COMMIT_MICROS` (500, 0..100000),
`RACER_WRITE_JOURNAL_BYTES` (64 MiB, 1 MiB..16 GiB),
`RACER_WRITE_ORPHAN_SECS` (3600, 60..604800), `RACER_WRITE_ALIAS_SECS`
(604800, 3600..2592000), `RACER_WRITE_UPLOAD_ATTEMPTS` (10, 1..100). The
generated reference table in `docs/content/reference/racer.md` and the
operator's `dataplane-settings.json` are regenerated from the table.

As implemented: `racer_wire::control` re-exports the single `UploadMode`
from `racer_wire::object::write` (see "WP1 and WP2 as built"), so the catalog
and the `Racer-Upload` header share one type and spelling.
`MAX_WRITE_REPLICAS` and `MAX_WRITE_HOLD_TTL` in
`cmd/racer/wire/src/control/writes.rs` derive from `object::write::MAX_REPLICAS`
and `MAX_HOLD_SECONDS`, which the store's `MAX_HOLD_TTL` also uses. The
CRD uses a typed `UploadMode` string (`Auto`, `Manual`, `Never`) rather than
`*string`, and `WriteUpload.MaxConcurrentBlocks` is bounded 1..1024 and
`MaxBytesPerSecond` at least 0; the wire enforces the same bounds.
`validate_definitions` rejects `writes` on a passthrough cache and an upload
mode other than `never` without a native origin, so these rules hold on decode
as well as in the CRD and authority. `CacheDefinition::writes()` returns the
cache's settings to the dataplane.

## Implementation plan

### Dependency graph

```
WP1 client wire ----\
WP2 core types -----+--> WP5 write service --> WP8 repair/rekey/GC --> WP11 DST + e2e
WP3 durable store --+        ^                                         ^
WP4 peer protocol --/        |                                         |
WP6 Azure upload ------------+ (needs WP2 block_id)                   |
WP7 control plane ------------> WP5 (config)                           |
WP9 SDKs (Python, Go) -> WP10 DCP plug-in ------------------------------+
WP12 docs (after WP5..WP10 settle)
WP13, WP14 later
```

Parallel batch 1 (separate worktrees, disjoint files): **WP1, WP2, WP3, WP4,
WP6, WP7, WP9**. WP3, WP4, WP6 import WP2 types: start WP2 first and have the
others code against the type definitions in this document, then rebase on WP2.
Batch 2: **WP5** (integrates everything). Batch 3: **WP8, WP10, WP12** in
parallel. Batch 4: **WP11**. Later: **WP13, WP14**.

File ownership per WP is listed below so parallel worktrees do not conflict.
Shared files that more than one WP must touch are called out with the owning WP;
others only add `mod` lines there and must rebase.

### WP1: client wire parsing

Files: new `cmd/racer/wire/src/object/write.rs`; `cmd/racer/wire/src/object.rs`
(`pub mod write;`); `cmd/racer/wire/src/object/client.rs` (only `error_head`:
an `allow_writes: bool` variant that emits `Allow: HEAD, POST, PUT, DELETE`).

Work:

- Implement the types and functions in [Client wire](#client-wire-wp1),
  following the parser style of `RequestParser::parse` (`client.rs:169-366`):
  duplicate header rejection, header byte limit, `Host: racer` required, opaque
  value validation.
- `is_write`: `PUT` or `DELETE` on `/v1/objects/<64 hex>`, or `POST` on
  `/v1/objects/<64 hex>/host-write` or `/persist`. The existing parser keeps
  rejecting these (it is not changed), so writes must be routed before it.
- Header rules from [Request headers](#request-headers). `If-None-Match: *`
  required on `PUT`/`host-write`; `Content-Length` must be `0` on `persist`,
  `DELETE`, `host-write`.

Tests (unit, in `write.rs`): every header rule success and failure; duplicate
headers; `If-None-Match` other than `*`; size limits at `50_000 * page_size`
boundary (the parser receives `page_size` as an argument); `Racer-Hold-Seconds`
of `0` and `2_592_001`; golden encodings of `write_created_head` and
`write_error_head`. Add a fuzz target `write_head` in `cmd/racer/fuzz`
mirroring `http_head`, and add it to `RACER_FUZZ_TARGETS` (`Makefile:624`).

### WP2: core write types, session state machine, journal codec

Files: new `cmd/racer/core/src/write/{mod.rs,session.rs,journal.rs,upload.rs,holders.rs}`;
`cmd/racer/core/src/lib.rs` (`pub mod write;`).

Work:

- Types in [Racer core types](#racer-core-types-wp2).
- `journal.rs`: `JournalRecord` enum for kinds 1..8,
  `encode(&JournalRecord, &mut Vec<u8>)`, `decode(&[u8]) -> Result<(JournalRecord, usize), JournalError>`
  with `JournalError::{Torn, Corrupt}`, and `replay(bytes) -> (Vec<JournalRecord>, valid_prefix_len)`.
- `session.rs`: the coordinator `Session` state machine with the event and effect
  lists above. Rules: open -> (pages) -> committed or aborted; lease expiry
  aborts an uncommitted session; `Commit` requires `holders[p].len() >= replicas`
  for every page and distinct nodes per page; `Delete` releases all holders and
  is terminal; `Persist` is valid only in `Manual`; upload completes only after
  every page is uploaded and the block list commits.
- `upload.rs`: `block_id`, block list XML builder
  `block_list_xml(version_hex, pages) -> Vec<u8>`, and an `UploadPlan` that
  assigns pages to the first reachable holder and bounds per-holder batches.
- `holders.rs`: binary holder table encoding for `racer-write-holders`.

Tests (unit): exhaustive state-machine transition table tests; property test
that random event sequences never produce `Publish` before `Commit` and never
`CommitBlockList` before all pages uploaded; journal round trip for every kind;
torn tail at every byte offset of a two-record stream yields exactly the first
record; digest flip yields `Corrupt`; block ID length is constant 64 for pages
`0` and `49_999`; XML golden test. Add fuzz target `write_journal` decoding
arbitrary bytes. `make racer-purity-graph` must pass.

### WP1 and WP2 as built

The code refined these interfaces; the code is authoritative.

- `UploadMode`, `Durability`, and `UploadState` live in
  `cmd/racer/wire/src/object/write.rs` (the response heads need
  `UploadState`); `racer_core::write` re-exports them with `MIN_REPLICAS` and
  `MAX_REPLICAS`, and `racer_wire::control` re-exports `UploadMode`.
- `parse_write(head, header_limit, page_size: PageSize) -> WriteResult<WriteRequest>`.
  The shared `object::Error` has no `411`, `413`, or `428` variant, so writes
  return `WriteError::{Object(Error), LengthRequired, PayloadTooLarge,
  PreconditionRequired}` with `WriteError::status()`. `Content-Length` may be
  absent or `0` on `DELETE`, `persist`, and `host-write`. Creation fields
  (`If-None-Match`, `Racer-Durability`, and so on) on `DELETE` or `persist`
  are `400`. Whether `Racer-Metadata` is present for the resolved upload mode,
  and whether it names a version, is checked by the caller (WP5), because the
  parser does not know cache defaults or the origin's metadata schema.
- Extra helpers: `max_object_bytes(page_size)`, `MAX_HOLD_SECONDS`,
  `error_token::*` constants, `write_no_content_head(Option<UploadState>)` for
  `204`, and `client::error_head_with_allow(status, length, allow_writes)`.
  The head codec omits reason phrases.
- `NodeIdBytes` is `Vec<u8>`. `Holders::{encode, decode, validate, nodes}`.
- `Session::open(identity: WriteIdentity, spec, now, lease)`: the session needs
  the cache, key, ETag, and session ID to build journal records. The caller
  journals `Session::open_record()`. `Session::from_manifest` rebuilds a
  committed session (an interrupted upload restarts as `Pending`).
  Refused events return a single `SessionEffect::Reject(Rejection)`.
  `SessionEffect::ReleaseTargets` releases every ranked target before commit,
  when the coordinator does not know who stored pages.
  `SessionEvent::HolderRestored { page, node }` records a repaired copy. Hold
  expiry is journaled as `Delete` (there is no separate expire kind).
- Journal records carry `cache: String`, `key: [u8; 32]`, `etag: String`.
  `journal::encode` returns `Result<(), JournalError>` and adds
  `JournalError::Oversized` for fields that exceed their length prefixes.
  Inside a manifest, the holder table is prefixed by a `u32 BE` byte length.
  `encode_manifest`/`decode_manifest` produce the `racer-write-manifest`
  payload, and `encode_header`/`decode_header` handle the 64-byte file header.
- `block_list_xml(version_hex, pages: u64)` commits pages `0..pages` and starts
  with an XML declaration. `UploadPlan::new(holders, pages, reachable, max_batch)`
  yields `batches` and `unassigned`.

### WP3: durable write storage

Files: new `cmd/racer/src/store/writes.rs` (write segment table, dirty
accounting, durability barrier) and `cmd/racer/src/store/journal.rs` (journal
file IO, group commit, compaction, replay); `cmd/racer/src/store.rs`
(`StoreWriter::persist_written`); `cmd/racer/src/store/catalog.rs`
(`can_evict` predicate, write-pin bookkeeping); `cmd/racer/src/app/recovery.rs`
(replay hook); `cmd/racer/src/admission.rs` (new `ResourceClass::WriteStaging`,
`WritePlaintext`).

Interfaces:

```rust
impl StoreWriter {
    /// Write a verified ciphertext page into a write segment, make it durable
    /// (data fdatasync, then journal fdatasync), and pin it. Idempotent for an
    /// already durable identical record.
    pub fn persist_written<'a>(&'a self, page: CiphertextCopy, session: SessionId,
        hold_until: SystemTime, scope: &'a RequestScope) -> Operation<'a, WrittenLocation>;
    /// Unpin and journal a release; the record becomes ordinary evictable cache
    /// once its segment has no live written records... (see below)
    pub fn release_written(&self, page: &PageId) -> Result<()>;
    pub fn written_pages(&self) -> impl Iterator<Item = (PageId, WrittenEntry)>;
    pub fn dirty_bytes(&self, cache: &CacheId) -> u64;
}
pub struct WriteJournal { /* per worker */ }
impl WriteJournal {
    pub async fn open(dir: &Path, worker: WorkerId, layout: [u8; 32], reactor: &Reactor) -> Result<(Self, Vec<JournalRecord>)>;
    pub fn append(&self, record: JournalRecord) -> Operation<'_, ()>; // resolves after group-commit fsync
}
```

Release semantics: a released page stays readable as long as its segment is not
reclaimed. Because write segments are reclaimed only when wholly dead,
release marks the record dead in the write table and journals `PageRelease`;
the catalog entry stays until the segment is reclaimed, when the normal
`remove_if_matches` (`catalog.rs:1144`) drops it.

Work:

- Write segments: a per-worker `WriteSegments` that takes free segments from the
  slab allocator and appends records using the existing record encoder
  (`store.rs:2226-2229` region) and slab write path. Keep the existing `O_DIRECT`
  alignment rules (`record_alignment`, `store.rs:2110`).
- Barrier: `Reactor::fsync(slab_fd, true, ctx)` after the record write completes,
  then journal append and `fsync(journal_fd, true)`. Group commit as specified.
- Dirty cap: refuse with `Error::Overloaded` mapped to `WriteReply::Full`.
- Recovery: open journal, replay, verify each live record by reading its header
  (`read_with_token`, `store.rs:155`), re-publish into the catalog, mark segments
  live, before any allocation. Wire it in `app/recovery.rs`.
- Expiry: a per-worker timer releases pages whose `hold_until` passed unless the
  coordinator extended them (the coordinator sends `WriteRelease` or a new
  `WriteStore` with a later hold for already stored pages, which only updates the
  hold).

Tests:

- Unit (`store/tests.rs` style, using the simulated OS): durable after barrier
  survives `SimOs` crash (`disk.rs:248`); a crash before the barrier loses the
  page and replay does not report it; an injected `fsync` EIO
  (`sim.inject("fsync", Fault::Errno(EIO))`, as at `cmd/racer/src/control.rs:701`)
  fails `persist_written` and does not journal; torn journal tail is truncated;
  write segments are never chosen for eviction while live; dirty cap boundary;
  group commit issues one fsync pair for many concurrent appends; compaction
  round trip; recovery with a rejected checkpoint still restores written pages;
  geometry change reports all pages lost.
- Extend `cmd/racer/tests/process_restart.rs` with a written-page restart test.
- Add fuzz target `write_journal_file` replaying arbitrary journal files.

### WP4: peer protocol for writes

Files: `cmd/racer/src/peer/protocol.rs` (variants, encode, decode, response
encoding); `cmd/racer/core/src/read/mod.rs` (`PeerResponse::Write`,
`ResponseKind::Write`); `cmd/racer/src/peer/tests.rs` (codec tests);
`cmd/racer/fuzz` corpus for `peer_envelope`.

Work:

- Add `Operation::{WriteSession, WriteStore, WriteRelease, WriteUpload}` with
  payload structs from [Peer operations](#peer-operations-wp4). Encode in
  `request_head` next to `Prefetch` (`protocol.rs:570-595`), decode next to
  `prefetch_action` (`protocol.rs:650`). Modes: `write-session` and
  `write-upload` are `Acquire` (they carry origin context for open-time origin
  HEAD and for upload credentials, like `PREFETCH_MODE`, `protocol.rs:648`);
  `write-store` and `write-release` are `CopyOnly` with no origin context.
- Add `PeerResponse::Write(WriteReply)` and its response encoding/decoding in
  `response_head` (`protocol.rs:691`) and the matching decoder. Update every
  exhaustive `match` on `PeerResponse`/`ResponseKind` (compile errors will list
  them).
- In `Coordinator::serve_peer` (`read.rs:600`) add a single early branch that
  returns `Err(Error::InvalidRequest)` for write operations; WP5 replaces it with
  real dispatch. This keeps WP4 mergeable alone.

Tests: encode/decode round trip for every action and reply; rejection of
missing/duplicate headers, out-of-range replicas, page lists outside the object;
signed request verification still binds every new header (extend the existing
signature tests in `peer/tests.rs`); fuzz corpus seeds for each new operation.

### WP4 as built

The code refined these interfaces; the code is authoritative.

- Operations (`cmd/racer/src/peer/protocol.rs`), re-exporting
  `racer_core::write::{SessionId, WriteAction, WriteReply}`:
  ```rust
  Operation::WriteSession { object: ObjectId, etag: Option<StrongEtag>, action: Box<WriteAction> }
  Operation::WriteStore { version: ObjectVersion, store: Box<WriteStore> }
  Operation::WriteRelease { version: ObjectVersion, session: SessionId, pages: Vec<u64> }
  Operation::WriteUpload { version: ObjectVersion, session: SessionId, pages: Vec<u64> }
  impl Operation { pub fn is_write(&self) -> bool; pub fn write_object(&self) -> Option<&ObjectId>; }
  ```
  `action` and `store` are boxed to keep `Operation` small. `WriteSession`
  takes an object and an optional ETag instead of an `ObjectVersion`, because
  `status`, `delete`, and `persist` come from client requests that do not
  know the Racer version. The ETag (`racer-etag`) is required for `open`,
  `renew`, `commit`, `abort`, and `replicate`
  (`write::action_requires_etag`), and `replicate` requires it to equal the
  manifest ETag.
- Codec module `cmd/racer/src/peer/protocol/write.rs` (`protocol::write`):
  ```rust
  pub struct WriteStore { pub session: SessionId, pub pages: Vec<u64>, pub length: u64,
      pub page_size: PageSize, pub content_type: Option<ContentType>, pub source: NodeId,
      pub hold_until_millis: u64 }
  pub fn action_name(&WriteAction) -> &'static str;
  pub fn action_requires_etag(&WriteAction) -> bool;
  pub fn pages_in_object(pages: &[u64], length: u64, page_size: u64) -> bool;
  ```
  plus `pub const` header names (`WRITE_ACTION`, `WRITE_SESSION`,
  `WRITE_PAGES`, `WRITE_SOURCE`, `WRITE_HOLD_UNTIL`, `WRITE_REPLICAS`,
  `WRITE_DURABILITY`, `WRITE_UPLOAD`, `WRITE_HOLDERS`, `WRITE_WAIT_MILLIS`,
  `WRITE_MANIFEST`, `WRITE_RESULT`, `WRITE_LEASE_UNTIL`,
  `WRITE_UPLOAD_STATE`, `WRITE_STORED`, `WRITE_ETAG`). `request_head` and
  `SecurityCodec::request` encode and decode them; `response_head` and the
  response decoder handle `racer-outcome: write`.
- Wire details that differ from the table above: `racer-write-durability`
  uses the client header form (`replicas=N` or `origin`), and must agree
  with `racer-write-replicas` (`WriteSpec::validate`). `delete` and
  `persist` carry no session, matching `WriteAction` (WP2).
  `racer-write-wait-millis` is omitted when zero, so an explicit `0` is
  rejected as non-canonical. `WriteReply::Status` carries its ETag in
  `racer-write-etag` (a strong ETag). Page lists use the prefetch encoding,
  so every list has 1..=256 ascending, unique pages; `Stored` and
  `Uploaded` cannot be empty.
- Validation: `open` and `replicate` specs pass `WriteSpec::validate`, a page
  size `PageSize::from_wire` accepts, and a valid content type. `write-store`
  pages must lie inside `length` at `page_size`. A `commit` holder table must
  name at least one page and one holder per page; the page count and replica
  floor are left to the coordinator (`Holders::validate`). Modes are fixed:
  `write-session` and `write-upload` are `acquire`; `write-store` and
  `write-release` are `copy`, carry no origin metadata or authorization, and
  spend no route attempts.
- Size limit: a write request head, including the `commit` holder table and
  the `replicate` manifest, must fit the peer head limit
  (`peer_wire::MAX_HEAD`, 64 KiB, and `MAX_SIGNED_HEAD` once signed). A
  holder table for many pages does not fit; WP5 must split large commits or
  send a smaller holder encoding.
- Responses: `PeerResponse::Write(WriteReply)` and `ResponseKind::Write` in
  `core/src/read/mod.rs`; `response_error(Write)` is `None`.
  `racer_core::route::Outcome::Write` asserts success (only the destination
  may sign it). `forwarding::write_response_matches` accepts a write reply
  only for a write operation, `stored` only for `write-store`, and
  `uploaded` only for `write-upload`, with pages a subset of the request.
  Finer per-action result rules are left to WP5.
- Dispatch: `OperationKind::Write` (core), which `serve_action` rejects.
  `Coordinator::serve_peer` (`read.rs`) returns `Err(Error::InvalidRequest)`
  for any write operation before other dispatch, and the dispatcher routes
  write operations to the object's metadata worker. WP5 replaces the early
  return.
- Tests: `cmd/racer/src/peer/forwarding/tests/write.rs` (round trips of
  every action and reply through the signed envelope, encode and decode
  rejections, signature binding of every write header, reply binding), and
  `read::tests::peer_copies::write_operations_are_rejected_until_writes_are_served`.
  Fuzz: `peer_envelope` seeds `seed-write-*` (generated in `fuzz/tests/seeds.rs`).
- The Azure origin's duplicate block ID and XML helpers are gone:
  `racer_core::write::block_list_xml_from_ids(&[[u8; 64]]) -> Vec<u8>` builds
  the body, `block_list_xml(version_hex, pages)` calls it, and
  `origin::azure::request::block_list_xml` validates IDs and calls it.

### WP5: write service (integration)

Files: new `cmd/racer/src/write.rs` and `cmd/racer/src/write/{service.rs,staging.rs,coordinator.rs,store_target.rs,directory.rs,client.rs}`;
`cmd/racer/src/lib.rs` (`mod write;`); `cmd/racer/src/client/listener.rs`
(route `is_write` heads before the read parser, next to the prefetch branch at
`listener.rs:971-987`); `cmd/racer/src/read.rs` (`serve_peer` dispatch of write
operations); `cmd/racer/src/read/dispatch.rs` (new `Work::Write*` variants and
staging lookup in `cached_page`, `dispatch.rs:292`); `cmd/racer/src/read/metadata.rs`
(write-directory check in `refresh_origin`, `metadata.rs:707`; upload state in
metadata replies); `cmd/racer/src/app.rs` (assemble and configure the service
per cache from `CacheDefinition.writes`); `cmd/racer/src/telemetry/metrics.rs`
(metrics below).

Components:

- `write/client.rs`: serves a parsed `WriteRequest` on a client connection:
  resolve cache config (405 when disabled or passthrough), mint version, open
  session, stream pages (window), fan out `WriteStore`, commit, respond. For
  `host-write`, reuse `client/direct/host_map.rs` for read-only mapping.
- `write/staging.rs`: `WriteStaging` table (per worker), charged to
  `ResourceClass::WriteStaging`, consulted by `cached_page`.
- `write/store_target.rs`: handles `WriteStore`: directed `CopyOnly` pull from
  `source` (add `Candidates::copy_from(node, page)` in `read/candidates.rs`
  modeled on the directed request at `candidates.rs:2070-2096`), then
  `StoreWriter::persist_written` (WP3), reply `Stored`/`Full`.
- `write/coordinator.rs`: hosts `racer_core::write::Session` instances per
  object on the page-zero worker, executes effects (journal via WP3, publish via
  `publish_version`/`publish_current`, release, upload via WP6, replicate),
  and answers `status` long polls.
- `write/directory.rs`: per-cache map `ObjectId -> DirectoryEntry { etag, state,
  expires, blob_etag }` on the coordinator and manifest replicas, rebuilt from the
  journal at startup; `current(object)` used by `refresh_origin`.
- Metrics: `racer_write_requests_total{cache,outcome}`,
  `racer_write_bytes_total{cache}`, `racer_write_dirty_bytes{cache}`,
  `racer_write_fsync_seconds` (histogram), `racer_write_upload_bytes_total{cache}`,
  `racer_write_upload_state{cache,state}` (gauge), `racer_write_repairs_total{cache}`.

Tests:

- Unit/integration in `cmd/racer/src/write/tests.rs` using the existing
  multi-node test fixtures (see `cmd/racer/src/read/tests/remote.rs` for a
  multi-node harness): PUT then GET/host-read round trip on a 3-node pool;
  `replicas=1,2,3` produce exactly that many holders; writer as a store target;
  412 on second PUT; 412 when the origin has the object; 405 when disabled and in
  passthrough; dirty-cap 503; client disconnect mid-body aborts and releases;
  lease expiry aborts; HEAD reports upload states; DELETE releases holders.
- `cmd/racer/tests/production_dataplane.rs`: one end-to-end PUT and read through
  the production assembly.
- RDMA: one test under `--features rdma` (or the existing RDMA test harness in
  `cmd/racer/src/rdma/links/tests.rs`) that a store-target pull uses an RDMA link
  when available.

### WP6: Azure upload

Files: `cmd/racer/src/origin/azure/request.rs` (`AzureMethod::{PutBlock,
PutBlockList}` and head builders); new `cmd/racer/src/origin/azure/crc64.rs`;
new `cmd/racer/src/origin/azure/upload.rs`; `cmd/racer/src/origin/azure.rs`
(public upload entry points); new `cmd/racer/src/write/upload.rs` is owned by
WP5, so WP6 exposes a trait in `cmd/racer/src/origin/upload.rs`:

```rust
pub trait UploadSink {
    /// Put one block. `crc64` is the Azure CRC-64 of `plaintext`.
    fn put_block<'a>(&'a self, context: &'a OriginContext, block_id: &'a [u8; 64],
        plaintext: &'a [u8], scope: &'a RequestScope) -> Operation<'a, ()>;
    /// Commit blocks in order with `If-None-Match: *`. Returns the Blob ETag.
    fn put_block_list<'a>(&'a self, context: &'a OriginContext, block_ids: &'a [[u8; 64]],
        content_type: Option<&'a str>, scope: &'a RequestScope)
        -> Operation<'a, Result<StrongEtag, CommitConflict>>;
}
pub struct CommitConflict;
```

Work:

- Heads: `PUT {target}?comp=block&blockid={query_escape(base64 id)}` and
  `PUT {target}?comp=blocklist`, both with `Host`, `x-ms-version`
  (`request.rs:18`), `x-ms-date`, `User-Agent`, `Authorization: Bearer`,
  `Content-Length`; Put Block adds `x-ms-content-crc64` (base64 of the 8-byte
  CRC); Put Block List adds `If-None-Match: *`, `Content-Type:
  application/xml`, optional `x-ms-blob-content-type`. Reuse `target`
  (`request.rs:73`) without the `versionid` query (writes have no version ID).
- `crc64.rs`: table-driven CRC-64 with the Azure Storage polynomial
  (reflected `0x9A6C9329AC4BC9B5`, initial and final XOR `!0`). Verify against
  test vectors from the Azure SDK for Go `azblob` CRC64 tests; this polynomial
  claim is from prior art research, not from this repository, and must be
  confirmed by those vectors before merge.
- Transport: reuse the existing Azure transport and token path
  (`azure/transport.rs`, `azure_auth.rs:468`), adding request bodies
  (`write_body`, `cmd/racer/http/src/connection.rs:996`). Map `201` success,
  `409 BlobAlreadyExists` and `412` to `CommitConflict`, `403` to
  `OriginForbidden`, `5xx` and `429` to retryable `Unavailable`.
- Rate limiting: `UploadLimiter` per cache (token bucket on bytes, semaphore on
  concurrent blocks) using the worker clock.
- A `MemoryUploadSink` test double (in-memory blocks per blob, conditional
  commit, fault injection) for WP5, WP8, and WP11.

Tests: head golden tests (header set and order, escaping of block IDs); CRC
vectors; transport tests against the existing scripted transport in
`origin/azure/transport/tests.rs`: success, 409 conflict, 403, retry on 503;
limiter properties.

As implemented (code is authoritative; these notes record where it refines the
plan above):

- CRC-64 verified: azblob v1.8.2 `internal/shared/shared.go` uses
  `crc64.MakeTable(0x9A6C9329AC4BC9B5)` with Go `crc64.Checksum` (reflected,
  initial and final XOR `!0`; this is CRC-64/NVME, check value
  `0xAE8B14860A799888`), and `internal/exported/transfer_validation_option.go`
  sends the CRC as 8 little-endian bytes in standard base64. Vectors in
  `origin/azure/crc64.rs` were produced with Go `hash/crc64` and that table.
- `AzureTransport` gains a required `exchange_body(url, head, body, scope)`.
  `PoolTransport` sends the head, copies the body into admitted
  request-context storage (`write_body_bytes`), and never retries on a stale
  pooled connection, since a write may have reached the service.
- Head builders are `request::put_block_head` and
  `request::put_block_list_head`; `request::head` rejects the write methods.
  Put Block List also sends `x-ms-content-crc64` of its XML body (optional per
  the REST docs). The body is
  `<?xml version="1.0" encoding="utf-8"?><BlockList><Latest>id</Latest>...</BlockList>`
  (`request::block_list_xml`, taking block IDs; WP2's `block_list_xml` takes
  `(version_hex, pages)`, so the integrator may keep one and adapt the other).
  A test-only `origin::azure::upload::block_id` mirrors WP2's `block_id`.
- A response to Put Block that echoes a different `x-ms-content-crc64` is
  `BadGateway`. `401` gets one retry with a refreshed token (as for reads),
  then `OriginRejected`. A `409` other than `BlobAlreadyExists` is classified
  like reads (`Internal`), not a conflict.
- Sinks make one attempt per call. Retries are the caller's:
  `origin::upload::RetryPolicy` (default 10 attempts, 200 ms base doubling to
  a 30 s cap, jittered into `[d/2, d]`) with `RetryPolicy::run`, and
  `origin::upload::retryable`. This keeps attempt counting and
  stop-on-delete with the coordinator (WP5).
- `UploadLimiter::try_acquire(bytes, now)` is non-blocking and returns
  `Admission::{Ready(UploadPermit), Wait(Instant), Busy}`; the caller sleeps
  on the worker clock. The bucket holds one second of burst and may go into
  debt, so a block larger than the burst is admitted from a full bucket.
  `UploadLimits` is the node-wide per-cache registry.
- `MemoryUploadSink` (with `UploadCall`, `MemoryBlob`) lives in
  `origin/upload.rs`; a commit naming an unstaged block fails with
  `InvalidRequest`, like Azure `400 InvalidBlockList`.

### WP7: control plane and CRD

Files: `api/racer/v1alpha1/clustercache_types.go` (+ generated deepcopy and CRD
via `make racer-generate`); `cmd/racer/wire/src/control.rs` (`WriteDefinition`,
DTO); `cmd/racer/authority/src/rules/catalog.rs` (mapping and tests);
`cmd/racer/src/config.rs`, `cmd/racer/src/config/spec.rs` (node settings);
operator RBAC docs only (no manifest change grants Azure RBAC).

Work: as specified in [Control plane](#control-plane-wp7). Canonical bytes of
caches without `writes` must not change (test with the existing canonical
fixtures). Add CEL validation tests under the envtest suite
(`make racer-envtest`).

Tests: catalog mapping success/defaults/invalid (mirroring
`passthrough_route_maps_to_mode` and `origin_maps_to_definition`,
`catalog.rs:349`, `:545`); wire round trip and canonical byte stability; an old
catalog fixture still decodes; envtest for CRD defaults and CEL; config table
tests for each new variable (`config.rs` test table near `:2132`).

### WP8: repair, rekey, and garbage collection

Files: new `cmd/racer/src/write/{repair.rs,rekey.rs,gc.rs}`; small hooks in
`cmd/racer/src/write/coordinator.rs` (owned by WP5; WP8 starts after WP5 merges).

Work:

- Repair on membership change and on lost-record reports (see
  [Repair](#repair)). Coordinator failover to the first surviving manifest
  replica.
- Rekey: each holder checks its written pages' key IDs against the cache keyring;
  a page whose key is among the two oldest retained decrypt keys is decrypted and
  re-encrypted under the active key (`CryptoClient::encrypt`), written as a new
  record, journaled, and the old record released.
- Orphan GC and alias expiry.

Tests: unit tests with a scripted membership; integration test killing one of
two holders before upload and observing restore to two holders; rekey test
driven by forced key rotation; orphan GC timeout.

### WP9: SDKs (Python and Go)

Files (Python): `clients/python/racer/src/racer/client.py` (`write`,
`host_write`, `persist`, `delete`), `_wire.py` (head encoding),
`__init__.py` exports, `tests/fakeserver.py` (write support),
`tests/test_write.py`, `tests/test_wire_golden.py` (golden heads).
Files (Go): new `pkg/racersdk/write.go` (`Put`, `HostWrite`, `Persist`,
`Delete`), `pkg/racersdk/types.go` (`WriteOptions`, `WriteResult`,
`UploadState`), `pkg/racersdk/internal/...` wire helpers, `pkg/racersdk/racersdktest`
fake server support, tests `write_test.go`.

API:

```python
class WriteResult:  # dataclass
    etag: str
    upload_state: str

def write(self, request: Request, data: bytes | memoryview | BinaryIO, *, length: int | None = None,
          replicas: int | None = None, durability_origin: bool = False,
          upload: str | None = None, hold_seconds: int | None = None,
          content_type: str | None = None) -> WriteResult
def host_write(self, request: Request, fd: int, src_offset: int, src_length: int, **same_options) -> WriteResult
def persist(self, request: Request) -> str        # returns upload state
def delete(self, request: Request) -> None
```

```go
type WriteOptions struct {
    Replicas      int    // 0 = cache default
    OriginDurable bool
    Upload        UploadMode // "", "auto", "manual", "never"
    Hold          time.Duration
    ContentType   string
}
type WriteResult struct { ETag string; UploadState UploadState }
func (c *Client) Put(ctx context.Context, request Request, body io.Reader, length int64, options WriteOptions) (WriteResult, error)
func (c *Client) HostWrite(ctx context.Context, request Request, file *os.File, offset, length int64, options WriteOptions) (WriteResult, error)
func (c *Client) Persist(ctx context.Context, request Request) (UploadState, error)
func (c *Client) Delete(ctx context.Context, request Request) error
```

Errors map to typed errors: `ErrExists` (412), `ErrWritesDisabled` (405),
`ErrBusy` with `RetryAfter` (503), `ErrUploadFailed` (502). Keys for writes are
computed without `versionId` (`keys.py:59-86`); passing a version ID is a client
error. Both SDKs retry `503 dirty-cap` only when the caller opts in.

As implemented (WP9), the SDKs refine this API:

- Go: a rejected write returns `*WriteError{Op, Status, Code, RetryAfter}`,
  whose `Is` matches the sentinels above plus `ErrConflict` (409),
  `ErrNotFound` (404), `ErrInvalidRequest` (400, 411, 413, 428, 431), and
  `ErrUnavailable` (503, 504). `WriteOptions.RetryBusy time.Duration` is the
  opt-in: it retries `503` for up to that long, waiting `Retry-After` (at least
  1 s), and `Put` retries only when `body` is an `io.ReadSeeker`.
  `UploadState` constants are `UploadStatePending`, `UploadStateUploading`,
  `UploadStateCommitted`, `UploadStateFailed`, `UploadStateNever`,
  `UploadStateManual`. `Metadata.UploadState` carries `Racer-Upload-State`
  from `Stat`. `racersdktest.NewClientWithWrites(t, origin)` returns a client
  and a `*Writes` fake directory (`Object`, `Calls`, `Reject`, `SetEnabled`,
  `SetDefaultUpload`, `SetUploadState`); `HostWrite` descriptors arrive via
  `SCM_RIGHTS` in the fake.
- Python: `write` and `host_write` take `retry_busy: float = 0.0` (seconds,
  same rule; `write` retries only bytes-like or seekable data). Errors are
  `WriteError(StatusError)` subclasses `Exists`, `WritesDisabled`, `Busy`
  (also an `Unavailable`), `UploadFailed`, `Conflict`, with `code` and
  `retry_after`. `Metadata.upload_state` carries `Racer-Upload-State`.

Tests: golden heads; fake-server round trips for each status; `SCM_RIGHTS`
descriptor passing for `host_write` (mirror `pkg/racersdk/hostread_test.go`).

### WP10: PyTorch DCP storage plug-in

Files: new `clients/python/racer/src/racer/dcp.py`; tests
`clients/python/racer/tests/test_dcp.py` (skipped when torch is absent);
optional extra `dcp = ["torch>=2.4"]` in `clients/python/racer/pyproject.toml`.

Design:

- `RacerStorageWriter(torch.distributed.checkpoint.StorageWriter)` with
  `(client, bucket, prefix, checkpoint_id, persist_every: int = 0, step: int,
  replicas: int = 2, hold_seconds: int | None = None)`. Each rank writes its data
  files (`__<rank>_<n>.distcp`) with `Racer-Upload: auto` when
  `persist_every > 0 and step % persist_every == 0`, else `never`.
  `finish()` (rank 0) writes `.metadata` last with the same upload mode, after
  all data writes returned `201`.
- `RacerStorageReader(StorageReader)` reads `.metadata` first; a missing
  `.metadata` means the checkpoint is incomplete (the "last known good complete
  checkpoint" rule from GKE/Orbax).
- Uses `host_write` from a memfd when tensors are on CPU; GPU tensors are staged
  to pinned host memory first (no GPU write path in v1).
- Keys: `object_key(bucket, f"{prefix}/{checkpoint_id}/{name}")`.

Tests: with torch installed, save and load a small state dict through the fake
server; `.metadata` written strictly after all data files; upload mode selection
by step; incomplete checkpoint detection.

As implemented: the constructor is `RacerStorageWriter(client, bucket, prefix,
checkpoint_id=None, *, step, persist_every=0, replicas=2, hold_seconds=None,
retry_busy=0.0)`; `checkpoint_id` may come from `dcp.save(checkpoint_id=...)`
through `reset`. Each rank writes one data object, `__<rank>_0.distcp`, with
`host_write` from a memfd (tensors are serialized with `torch.save`, as
`FileSystemWriter` does). `.metadata` is a pickled DCP `Metadata` sent with
`write`. The reader reads each item's byte range pinned to the data object's
ETag, raises `IncompleteCheckpoint` (a `RacerError` and `FileNotFoundError`)
when `.metadata` is missing, and offers `is_complete()`. Re-saving an existing
`checkpoint_id` fails with `Exists` (create-once).

### WP11: simulation and e2e

Files: new `cmd/racer/src/app/dst/writes.rs` (traffic and faults);
`cmd/racer/src/app/dst.rs` (register; oracle extended to own written versions);
`cmd/racer/src/app/dst/faults.rs` (new actions); `cmd/racer/dst/src/scenarios.rs`
and `invariants.rs` (model scenario `write-back` if the model layer is used);
`cmd/racer/dst/seeds/ci.txt`; new `e2e/racer/write_e2e_test.go`.

DST actions: `write_put` (random sizes including last-page short), `write_delete`,
`write_persist`, `crash_holder_before_barrier`, `crash_holder_after_barrier`,
`crash_coordinator_after_commit`, `fsync_eio`, `partition_writer`,
`membership_remove_holder`, `upload_conflict`, `upload_faults` (via
`MemoryUploadSink`).

Invariants:

1. A PUT that returned `201` with `replicas=N` stays readable with identical
   bytes after any set of crashes affecting fewer than `N` holders and fewer than
   `N` manifest holders, until delete or hold expiry.
2. No reader ever sees a version before its `201` (or before commit for
   in-flight PUTs).
3. A deleted or aborted object is never committed to the upload sink.
4. Each committed blob's bytes equal the written bytes; the block list is
   committed at most once per object.
5. Dirty bytes never exceed the cap.

e2e (kind, `make e2e-racer`): a cache with `writes` and an Azure origin
emulated by the existing harness origin (or Azurite if the harness already
supports it; otherwise `upload.mode: Never` coverage plus a WP6 transport test):
PUT, read on another node, delete one Racer pod and read again, `persist` flow.

### WP12: documentation

Files: `docs/content/reference/racer.md` (new sections after "Prefetch",
`racer.md:831`: "Writes" with API, headers, statuses, upload states, settings,
metrics; ClusterCache `writes` after "Azure origin", `racer.md:155`; "Storage"
env vars at `racer.md:383`), `docs/content/concepts/racer.md` ("How a Write
Works" after "How a Read Works", `concepts/racer.md:225`; "What Stays on Disk"
at `:242` gains written data and the journal). State plainly: Racer remains a
cache; written-but-not-uploaded data has N-copy disk durability; deleting a
ClusterCache or changing node storage layout loses unuploaded data; Azure writes
need the `Storage Blob Data Contributor` role for the cache's identity. Use the
`docs` skill.

### WP13 (later): UDS origin adapter writes

Extend the UDS origin protocol (`origin.rs:458`) with `PUT` carrying a body per
object (single request, streamed from holders via the coordinator) and the Go
`ServeOrigin` (`pkg/racersdk/origin.go:173`) with an optional
`OriginWriter func(ctx, OriginWriteRequest, io.Reader) (Metadata, error)`.
Implements `UploadSink` with one-shot upload (no blocks).

### WP14 (later): racer-object S3 PutObject

Allow `PUT` in `internal/racerobject/racerobject.go:137` and translate to
`racersdk.Put` with `If-None-Match: *` semantics, mapping `412` to S3
`PreconditionFailed`.

## Build, test, and lint commands

Every implementer runs, from the repository root, with timeouts (these are the
Makefile targets, which already wrap commands in `timeout --signal=TERM`):

- Rust format: `make racer-fmt` (`Makefile:612-613`).
- Rust lint: `make racer-lint` (fmt check, workspace clippy with `-D warnings`,
  purity test; `Makefile:618-621`).
- Rust tests: `make racer-rust-test`
  (`cargo test --locked --manifest-path cmd/racer/Cargo.toml --workspace`,
  `Makefile:604-605`). For a focused run:
  `timeout --signal=TERM 600s cargo test --locked --manifest-path cmd/racer/Cargo.toml -p racer <filter>`
  (or `-p racer-core`, `-p racer-wire`).
- DST: `make racer-dst` (`Makefile` `racer-dst` target; `DST_FILTER`,
  `DST_SEEDS`, `DST_STEPS`).
- Fuzz (after adding targets): `make racer-fuzz FUZZ_SECONDS=30`.
- Go API and CRD: `make racer-generate`, `make racer-go-test`, `make racer-envtest`.
- Go SDK: `timeout --signal=TERM 300s go test -race ./pkg/racersdk/...`, then
  `make fmt` and `make lint`.
- Python: `make racer-python-test`.
- e2e (WP11 only): `make e2e-racer`.

## Risks

- **Disk write amplification.** Each written byte is written `N` times plus a
  journal record; fsync latency bounds per-page ack latency. Group commit
  mitigates.
- **Pinned capacity.** Written data competes with cache capacity up to
  `maxDirtyPercent`; `never` objects with long holds can starve reads. The cap is
  per worker, so a hot key range can hit it early.
- **Mixed versions.** Old nodes reject catalogs containing `writes`; enabling
  writes requires a full rollout first.
- **Key rotation.** Rekey (WP8) must run before keys age out; a stalled rekey is
  surfaced as a metric and logged.
- **Azure limits.** 50,000 blocks per blob caps objects at 781.25 GiB with 16 MiB
  pages; account ingress limits make the per-node upload rate limit important.
