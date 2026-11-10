# Racer with containerd as the image store

Status: implemented on this branch. This document describes how Gantry in
Racer mode uses containerd's content store as the only disk copy of image
data, and the small generic Racer features that make it possible.

It supersedes the `cachemesh` design from PR #1106. Review of that PR asked
not to add a separate routing library to Gantry and its Helm chart. Racer
already places pages on owner nodes and moves them between peers, so this
design builds the same idea on Racer instead (see
[Relationship to PR #1106](#relationship-to-pr-1106-cachemesh)).

## Summary

In Racer mode with today's defaults, every image byte a node pulls is on disk
twice: in Racer's slab store and in containerd's content store. Pages a node
owns but never pulls also use Racer disk. This design removes the Racer copy:

- A ClusterCache can set `spec.persistence: Adapter`. Racer then keeps that
  cache's pages in memory only and never writes them to its disks.
- On a memory miss, Racer first asks the origin adapter on the same node for
  a copy it already holds (a "local-only" origin request). For Gantry that
  copy is a file in containerd's content store.
- A page owner remembers which nodes it served (the holder table) and asks
  them before it walks the other owners and the remote origin.
- Clients can drop one cached version (invalidate).
- Gantry checks with the upstream registry that the caller may read an object
  before it serves it from Racer, and verifies the digest of every whole blob
  it serves. On a mismatch it aborts the response and invalidates the version.

Racer stays OCI-agnostic. It sees opaque keys, an opaque origin, and a flag
that says "local copy only". All registry logic stays in Gantry.

## Goals

- containerd's content store is the only on-disk copy of image data on a node
  in Racer mode.
- The cluster reads each blob from the upstream registry about once per
  rollout. Later pulls are served from nodes that still have the blob.
- Racer gains only generic features: a persistence mode, a local-only origin
  request, a holder table, and invalidate. None of them know about OCI.
- Racer never serves a cached object to a caller the registry would refuse.
- Every failure path ends with containerd pulling from the registry itself.

## Non-goals

- A new routing library or a pluggable routing interface for Gantry.
- Caching tags. Tag requests keep their current behavior.
- Changing containerd. Gantry only reads containerd's content store files.
- Changing default (p2p) Gantry mode.

## Architecture

```text
containerd --HTTP--> gantry mirror (node A)
                       | 1. upstream access check (HEAD, cached)
                       v
                    racer client socket (node A)
                       |
                       v
                    page owner (node B, ranked by Racer)
                       | memory -> own adapter (local-only) -> holders
                       |   -> other ranked owners -> remote origin
                       v
                    gantry origin socket (node B, or holder node C)
                       | local-only: containerd content store file
                       | normal: upstream registry with caller credential
```

### Persistence mode

`ClusterCache.spec.persistence` is `Racer` (the default when unset) or
`Adapter` (`api/racer/v1alpha1/clustercache_types.go`).

- `Racer`: pages are cached in memory and on Racer's disks, as before.
- `Adapter`: Racer never writes the cache's pages to its own disks. Pages are
  evicted from memory like any other page and refilled from the sources below.
- `Adapter` cannot be combined with `passthrough` or `origin`. The CRD
  rejects both (CEL rules on `ClusterCacheSpec`), and the Racer authority
  rejects them again when it builds the catalog
  (`cmd/racer/authority/src/rules/catalog.rs`, `mode`).
- The field is mutable. When a cache switches to `Adapter`, pages Racer
  already wrote to disk for it are dropped, and disk writes in flight are
  discarded (`cmd/racer/src/store.rs`).

### Local-only origin requests

A local-only request is a normal origin page GET with the request header
`Racer-Source: local` (`cmd/racer/wire/src/object/origin.rs`,
`SOURCE_HEADER`). The Go SDK exposes it as `racersdk.OriginRequest.LocalOnly`.
It means "serve this page only from a copy you already hold; do not contact
the remote origin".

- The adapter answers a miss with 404 (`racersdk.ErrNotFound`) and a
  different version with 412 (`racersdk.ErrVersionMismatch`). Racer treats
  both as a miss (`cmd/racer/src/origin.rs`, `local_page_reserved_at`).
- Racer treats any other failure, except cancellation and deadline, as a miss
  too. Local-only results never feed origin failure notes, link health, or
  cached misses (`cmd/racer/src/read/fill.rs`, `acquire_adapter_replica`).

### Fill order for Adapter caches

When an owner fills a page of an `Adapter` cache, it tries in order
(`racer_core::read::local_sources`, `cmd/racer/core/src/read/mod.rs`):

1. its own memory,
2. its own adapter, with a local-only request,
3. holder nodes (below),
4. the other ranked owners of the page,
5. the remote origin through its own adapter, with the caller's credential.

Pages found in steps 2 and 3 are published to memory like origin pages.
Concurrent fills of the same page still share one flight, so a cold rollout
reads each page from the registry once.

### Holder table

`cmd/racer/core/src/read/holders.rs`. When an owner serves a page of an
`Adapter` cache to a peer, it records that peer as a holder. For Gantry the
peer is a node whose containerd is pulling the blob, so it will soon hold the
whole blob on disk.

| bound | value |
|---|---|
| holders per page (`HOLDERS_PER_PAGE`) | 4, newest first, deduplicated |
| TTL per holder (`HOLDER_TTL`) | 24h |
| pages tracked per node (`HOLDER_CAPACITY`) | 65536, oldest page evicted first |
| holders asked per fill (`HOLDER_ATTEMPTS`) | 2 |
| deadline per probe (`HOLDER_TIMEOUT`) | 2s |

- A probe is a peer page request with `FetchMode::Holder`. The holder answers
  from its memory or from its own adapter with a local-only request. It never
  forwards or contacts the remote origin. A cache that is not `Adapter` on the
  holder always misses (`cmd/racer/src/read.rs`, `serve_holder`).
- A copy from a holder is decrypted, which authenticates it.
- A holder that misses, fails, or returns a copy that does not authenticate is
  dropped from the table.
- Keys are pages of one exact version, so a new version never routes to
  holders of an old one.
- The table is memory only. A restart loses it; the cost is a registry read.

### Invalidate

`DELETE /v1/objects/<key>` on the client socket, with a required strong
`If-Match: <etag>` (`cmd/racer/wire/src/object/invalidate.rs`). The Go SDK
exposes it as `racersdk.Client.Invalidate(ctx, key, etag)`.

- Racer answers `204 No Content` whether or not anything was cached, then
  closes the connection. A newer version of the key is never dropped.
- The local node drops the version's metadata, pages, and holder records at
  once. It then sends a signed peer operation to the ranked owners of each
  page; page zero's owners also drop the metadata. Receivers drop local state
  only and never forward it (`cmd/racer/src/read/invalidate.rs`).
- Bounds: at most 1024 pages per invalidation (a larger object drops its
  metadata and first pages only), 8 concurrent invalidations per node (more
  are refused and the SDK returns `ErrUnavailable`), and a 5s wait for peers.
  Peer failures are counted and never fail the client.

### Gantry: upstream access check

`internal/gantry/racer/access.go`. Racer serves cached pages without checking
the credential, and its key is the digest alone. So before Gantry reads Racer
it sends `HEAD /v2/<repo>/blobs/<digest>` or `HEAD /v2/<repo>/manifests/<digest>`
to the registry with the caller's exact credential (or none).

| registry result | Gantry answers | cached for |
|---|---|---|
| 2xx | serves from Racer | 60s |
| 401 | 401 with the registry's `WWW-Authenticate` challenge | 10s |
| 403 | 403 | 10s |
| 404 | 404 | not cached |
| 5xx, timeout (5s), or network error | 503; containerd goes to the registry | not cached |

- The cache key is SHA-256 of credential, registry, repository, kind, and
  digest. The credential itself is never stored. One caller's result never
  applies to another credential.
- The cache holds at most 16384 entries. Concurrent identical checks share
  one HEAD.

### Gantry: local replica handler

`internal/gantry/racer/local.go`, `internal/gantry/contentstore`. Gantry's
origin adapter answers local-only requests from containerd's content store
file `<racer_content_root>/blobs/sha256/<hex>`.

- `racer_content_root` (env `GANTRY_RACER_CONTENT_ROOT`, flag
  `--racer-content-root`) defaults to
  `/var/lib/containerd/io.containerd.content.v1.content`. Empty disables local
  replicas; then every local-only request misses.
- Only committed blobs are read, never in-progress ingests. The digest must
  be `sha256:` plus 64 lowercase hex, and on Linux the file is opened beneath
  a root opened with `O_PATH`, so a key cannot escape the root
  (`internal/gantry/contentstore/open_linux.go`).
- A missing or unusable file is a miss (404). An `If-Match` for another
  version is 412.
- The answer has the same size and ETag (`"sha256:<hex>"`) as a registry
  read. Blobs report `application/octet-stream`. A manifest reports its
  `mediaType` field; a manifest without one is a miss so the registry supplies
  the header.
- A local-only request never contacts the registry and ignores the
  credential. Normal requests always use the registry.
- containerd verifies a blob before it commits it, so local pages are not
  re-hashed per page.

### Gantry: digest verification

`internal/gantry/racer/verify.go`. Pages may come from another node's disk,
so Gantry does not trust them. For a full response (offset 0), Gantry hashes
the stream with SHA-256 and holds back the final byte until the hash matches.

On a mismatch Gantry:

1. aborts the connection, so containerd sees a short transfer and never a
   complete bad blob,
2. counts `gantry_racer_mirror_digest_mismatches_total`,
3. calls `Invalidate` for the version (5s budget, best effort), so a retry
   refills from other sources or the registry
   (`cmd/gantry/agent_racer.go`, `racer.WithInvalidator`).

Ranged responses (offset > 0) are not hashed here. containerd checks the
digest when it commits the blob.

### Operator wiring

`internal/operator/components/gantry/racer.go`, `applyRacerMode`. While
`ClusterCache/gantry` exists (and sets no `origin`), the operator:

- sets `GANTRY_RACER_ENABLED=true` and
  `GANTRY_RACER_CONTENT_ROOT=/var/lib/containerd/io.containerd.content.v1.content`,
- mounts the Racer client and origin socket directories,
- mounts the host's containerd content root read-only at the same path. The
  hostPath has no type check, so a node whose containerd uses another root
  still starts Gantry and only loses local replicas,
- copies the `racer` DaemonSet placement to Gantry.

The operator does not set `spec.persistence`. The user creates
`ClusterCache/gantry` with `persistence: Adapter` to use this design.

## End-to-end pull

1. kubelet asks containerd to pull an image. containerd resolves the tag
   upstream, then asks the Gantry mirror on `127.0.0.1:5000` for each blob by
   digest, with its registry credential in `Authorization`.
2. Gantry on node A runs the access check. On 401, 403, or 404 it answers
   that status. On error it answers 503 and containerd goes to the registry.
3. Gantry reads Racer on the local client socket. The key is the digest, and
   the caller's `Authorization` is an opaque credential.
4. Racer on node A asks each page's owner. Pages in node A's memory are
   served locally.
5. The owner (node B) fills a missing page in the order above. Only the last
   step contacts the registry, with the caller's credential, which Racer
   carried encrypted from node A. Node B records node A as a holder.
6. Pages stream back to node A. Gantry writes them to containerd while it
   hashes the blob.
7. containerd commits the blob. Node A's Gantry can now serve local-only
   requests for it.

A later pull on a new node, after owners evicted the pages, is served from
node A's content store instead of the registry.

## Auth model

- Racer does not interpret credentials. It carries them encrypted between
  nodes (`EncryptedAuthorization`, `cmd/racer/src/security.rs`) and gives the
  clear value only to the owner's local origin socket.
- Racer serves cached pages without a credential check. Gantry's access check
  closes this gap: the registry stays the only authority.
- Local-only and holder requests read only data the node already holds and
  carry no credential. They are reached only through Racer, after the
  requesting Gantry passed the access check.

## Failure handling

| failure | result |
|---|---|
| Gantry down on the pulling node | mirror dial refused; containerd pulls from the registry |
| Racer down or client socket error | Gantry answers 503; containerd pulls from the registry |
| access check: registry 5xx, timeout, or unreachable | 503; containerd pulls from the registry |
| access check: 401, 403, or 404 | same status (401 with challenge) |
| own adapter misses or fails | next source |
| holder misses, fails, times out, or returns a bad copy | holder dropped; next holder or source |
| owner evicted the page and no holder has it | owner reads the registry |
| owner node lost | Racer ranks the next owner, which fills the same way |
| containerd GC removes a blob | the next local-only request misses; an open file stays readable until closed (inference: Linux unlink semantics) |
| bad bytes from any source | digest mismatch; connection aborted; `Invalidate`; containerd retries |
| registry 401 on fill at the owner | Racer returns unauthorized; Gantry answers 401 with challenge |
| tag request | unchanged; containerd resolves upstream |

## Metrics

- Racer: `racer_adapter_local_{hits,misses,failures}_total`,
  `racer_holder_{records,probes_served,hits,misses,failures}_total`,
  `racer_invalidate_{requests,rejected,truncated}_total`,
  `racer_invalidate_peer_{requests,failures,served}_total`
  (`cmd/racer/src/telemetry/metrics.rs`).
- Gantry: `gantry_racer_mirror_digest_mismatches_total`.

## Memory budgeting

- `Adapter` pages share Racer's memory with other caches, weighted by
  `spec.priority`. Evicted pages are refilled, never moved to disk.
- The holder table is bounded by 65536 pages times 4 node ids per node.
- Gantry adds the access check cache (16384 entries) and one SHA-256 state
  per full blob stream. The local replica handler keeps nothing.

## Security considerations

- The content root is mounted read-only. Gantry cannot change images.
- Local reads open only `blobs/sha256/<64 hex>` under a root opened with
  `O_PATH`, so a key cannot escape the root.
- Bytes from any node are hashed before containerd sees a complete blob, and
  containerd checks again on commit.
- Credentials are never written to disk or Racer metadata, and only hashed in
  the access check key.
- The holder table holds node ids only.
- In `Adapter` mode no image page is on Racer's disks, so Racer's
  encryption at rest does not apply. The only at-rest copy is containerd's,
  as without Gantry.

## Benchmarking

`hack/gantry-cluster-bench` drives the operator-managed Gantry in `default`,
`racer`, and `adapter` modes. See its README for the runbook. Compare:

- origin egress: about 1.00x the image size for `adapter` and `racer`,
- node disk writes: `adapter` should match `default` (containerd only);
  `racer` adds the slab copy,
- pull p50, p99, and makespan,
- a second wave on other nodes with the same tag: about 0x origin egress in
  `adapter` mode.

## Relationship to PR #1106 (cachemesh)

PR #1106 added `internal/cachemesh`, a transport-agnostic routing library,
and a striped distribution path in Gantry built on it. Review asked not to
put a routing library into Gantry and its Helm chart. This design keeps what
worked and builds it on Racer.

Kept:

- containerd's content store as the only disk copy, with peers serving from
  it.
- Asking nodes that hold a blob before the origin.
- The benchmark harness, now `hack/gantry-cluster-bench` for Racer mode.

Dropped:

- The cachemesh library: its grid, leader election, and partition table.
  Racer already ranks owners and moves pages.
- Anonymous tag caching.
- Gantry-private chunk files; Racer pages replace them.

### Historical cachemesh numbers

From the cachemesh prototype on 2026-11-09, not from Racer. They set a target
for `adapter` mode. Setup: 142 workers (64 CPU, 30Gb/s NIC), one `registry:3`
origin capped at about 3.7GB/s, a 40GB image of four 10GiB incompressible
layers, all nodes pulling at once.

| mode (cachemesh prototype) | pull p50 | pull p99 | makespan | origin egress |
|---|---|---|---|---|
| Gantry default (DHT p2p) | 235-247s | 306-311s | 313-317s | 8.01x |
| cachemesh, relay grid, memory cache | 90.1-91.9s | 96.0-97.4s | 96.2-98.9s | 1.00x |
| cachemesh, containerd store as only disk copy | 91.8-92.3s | 97.3-97.6s | 97.8-101.8s | 1.00x |
| one node straight from origin (baseline) | 94.7s | | | 1x per node |

In the containerd-store runs Gantry wrote 0 bytes to disk and peaked at about
2.5GiB RSS. A second wave of 71 nodes pulled the same image 3 minutes later
with 0.00x origin egress. About 69s of every pull is containerd unpacking
layers, which no distribution path can remove.
