# Racer with containerd as the image store

Status: proposed. This document describes how Gantry in Racer mode uses
containerd's content store as the only disk copy of image data, and the small
generic Racer features that make it possible.

## Summary

Today, when Gantry runs in Racer mode, every image byte a node pulls ends up on
disk twice: once in Racer's slab store and once in containerd's content store.
Pages that a node owns but never pulls also take Racer disk space. This design
removes the Racer copy for the `gantry` cache:

- A ClusterCache can set `spec.persistence: Adapter`. Racer then keeps that
  cache's pages in memory only and never writes them to its slab disks.
- When a page is not in memory, Racer first asks the origin adapter on the
  same node to serve it from a copy it already holds ("local-only" origin
  request). For Gantry, that copy is containerd's content store.
- The page owner remembers which nodes recently read an object (the holder
  table) and asks those nodes before it asks the remote registry.
- Gantry checks with the upstream registry that the caller may read an object
  before it serves the object from Racer, and verifies the digest of every
  whole blob it serves.

Racer stays OCI-agnostic. It sees opaque keys, an opaque origin, and a flag
that says "local copy only". All registry logic stays in Gantry.

## Problem

1. **Duplicate disk copies.** A node that pulls a 40GB image writes the layers
   to containerd's content store and unpacks them into a snapshot. In Racer
   mode with Racer persistence the same pages are also in Racer's slab store,
   on the owner nodes and on any node that keeps a local copy. Disk is the
   scarce resource on GPU and edge nodes, and image data competes with model
   and dataset caches for the same Racer capacity.
2. **Registry egress.** Racer already reads each page from the registry once
   per owner fill. When owners evict pages (for example after a long idle
   period), a later pull reads from the registry again, even though many nodes
   still hold the full blob in containerd.

## Goals

- containerd's content store is the only on-disk copy of image data on a node
  in Racer mode.
- The cluster reads each blob from the upstream registry about once per
  rollout, and later pulls are served from nodes that still have the blob.
- Racer gains only generic features: a persistence mode, a local-only origin
  request flag, a holder table, and an invalidate operation. None of them know
  about OCI.
- Racer never serves a cached object to a caller the registry would refuse.
- Every failure path ends with containerd pulling from the registry itself.

## Non-goals

- A new routing library. The `cachemesh` library from PR #1106 is not used.
  Racer's existing page placement and peer transport do the routing.
- A pluggable routing interface for Gantry. A future "routing plugin" may be
  designed separately and is out of scope here.
- Caching tags. Tag requests keep their current behavior (Gantry answers 503
  and containerd resolves the tag upstream). There is no anonymous tag cache.
- Changing containerd. Gantry only reads containerd's content store files.
- Default (p2p) Gantry mode. It is unchanged.

## Current code

These are the parts of `main` this design builds on:

- The operator switches Gantry to Racer mode while `ClusterCache/gantry`
  exists (`internal/operator/components/gantry/racer.go:32`, `racerMode` at
  `:61`). It sets `GANTRY_RACER_ENABLED`, mounts the cache sockets, and copies
  the `racer` DaemonSet placement (`applyRacerMode`, `:118`). A cache with
  `spec.origin` keeps Gantry in default mode (`:77`).
- The mirror handler serves digest requests through the Racer client
  (`internal/gantry/racer/racer.go:459`). It passes the caller's
  `Authorization` header as an opaque Racer credential (`:482`) and maps Racer
  errors to HTTP status, with 503 for unavailable (`:814`).
- The origin adapter accepts only configured registries (`:159-178`), parses
  the credential, and reads the registry with it (`:181-188`).
- Racer encrypts the credential before sending it to the owner node
  (`EncryptedAuthorization`, `cmd/racer/src/security.rs:132`; carried in the
  peer origin request, `cmd/racer/src/peer/protocol.rs:636`). Only the owner's
  local origin socket receives the clear value.
- Racer serves a page that is already cached without asking the origin. It does
  not check the credential on a hit. This is the auth gap this design closes
  in Gantry (see [Auth model](#auth-model)).

## Architecture

```text
containerd --HTTP--> gantry mirror (node A)
                       | 1. upstream access check (HEAD, cached)
                       v
                    racer client socket (node A)
                       |
                       v
                    page owner (node B, ranked by Racer)
                       | memory -> local adapter replica -> holders
                       |   -> other ranked owners -> remote origin
                       v
                    gantry origin socket (node B or a holder node C)
                       | local-only: containerd content store file
                       | remote: upstream registry with caller credential
```

### Persistence mode

`ClusterCache.spec.persistence` takes `Racer` (default) or `Adapter`.

- `Racer`: today's behavior. Pages are cached in memory and on slab disks.
- `Adapter`: Racer never writes this cache's pages to its own disks. Pages
  live in memory and are evicted under memory pressure like any other memory
  page. A page that is evicted is refilled from the sources below.
- `Adapter` is rejected together with `passthrough` (which keeps nothing) and
  with `origin` (a Racer-managed origin has no adapter to hold copies).

### Local-only origin requests

Racer's origin-socket page GET gains a flag, carried as a request header
(`Racer-Source: local`) and exposed in the Go SDK as
`racersdk.OriginRequest.LocalOnly`. It means "serve this page only from a copy
you already hold; do not contact the remote origin". On a miss the adapter
answers 404 (`racersdk.ErrNotFound`). Racer treats any error from a
local-only request as a miss and moves to the next source.

### Fill order for Adapter caches

When an owner fills a page of an Adapter cache, it tries in order:

1. its own memory,
2. its own adapter with a local-only request,
3. holder nodes (see below), each with a local-only request to that node's
   adapter,
4. the other ranked owners of the page,
5. the remote origin through its own adapter, with the caller's sealed
   credential.

Concurrent fills of the same page still share one flight, so a cold rollout
reads each page from the registry once.

### Holder table

The page owner records which requesting nodes recently read pages of an
Adapter cache. For Gantry a reader is a node whose containerd is pulling the
blob, so it will soon hold the full blob on disk.

- Bounded per page (a small number of holders) and by a global cap, with
  about a 24h TTL. Oldest entries go first.
- On a miss, the owner asks holders before the remote origin. A holder that
  answers miss is dropped from the table.
- The table is memory-only. A restart loses it; the cost is a registry read.
- It is advisory. A holder is never trusted for correctness, only as a place
  to try (see [Digest verification](#digest-verification)).

### Invalidate

A new client operation drops one object version: `DELETE /v1/objects/<key>`
with `If-Match: <etag>`, exposed as `racersdk.Client.Invalidate(ctx, key,
etag)`. Racer drops the object locally and, best effort, on its page owners,
including the holder entries for it. Gantry calls it when it detects bad bytes.

### Gantry: upstream access check

Before Gantry serves a digest request from Racer, it asks the upstream
registry whether the caller may read it:
`HEAD /v2/<repo>/blobs/<digest>` or `HEAD /v2/<repo>/manifests/<digest>` with
the caller's credential.

- Results are cached for a short time, keyed by
  `hash(credential) + registry + repo + kind + digest`. The credential itself
  is not stored.
- 401: Gantry answers 401 with the registry's `WWW-Authenticate` challenge, so
  containerd can fetch a token and retry.
- 403 or 404: Gantry answers the same status.
- 5xx or network error: Gantry answers 503, and containerd falls through to
  the registry. Gantry never serves cached bytes when it cannot confirm access.
- 2xx: Gantry serves from Racer.

The check is one small request per object per credential per cache period. It
does not move image data.

### Gantry: local replica handler

Gantry's origin adapter handles `LocalOnly` requests by reading containerd's
content store file `<content root>/blobs/sha256/<hex>`. The content root is
mounted read-only into the Gantry pod at
`/var/lib/containerd/io.containerd.content.v1.content` by default.

- Only committed blobs are read. In-progress ingests are not.
- A missing file is a miss (404). A short read, or a file that disappears or
  shrinks during the read (containerd GC), is an error; Racer treats it as a
  miss and moves on.
- The adapter returns the same size and ETag (`"<digest>"`) as a remote read,
  so Racer cannot tell the sources apart.
- `LocalOnly` never contacts the registry and ignores the credential.

### Digest verification

Bytes can come from another node's disk, so Gantry must not trust them.
When Gantry serves a whole blob to containerd, it hashes the stream with
SHA-256 as it writes. If the digest does not match at the end, Gantry aborts
the connection (so containerd sees a failed transfer, not a complete blob)
and calls `Invalidate` for the object. containerd retries, which refills from
other sources or the registry. containerd also verifies every blob it commits,
so a bad range read cannot end up in the content store either.

### Operator wiring

- `ClusterCache/gantry` gets `persistence: Adapter`.
- The Gantry DaemonSet mounts the containerd content root read-only in Racer
  mode.
- Nothing else changes: socket mounts and placement work as today.

## End-to-end pull

1. kubelet asks containerd to pull `registry.example.com/app@sha256:...`.
   containerd resolves the tag upstream (Gantry answers 503 for tags), then
   asks the Gantry mirror on `127.0.0.1:5000` for each blob by digest, with
   its registry credential in `Authorization`.
2. Gantry on node A runs the upstream access check for that credential and
   digest. On 401/403/404 it answers that status. On error it answers 503 and
   containerd goes to the registry.
3. Gantry calls Racer on the local client socket with key = digest, metadata =
   registry, repo and kind, and the caller's `Authorization` as an opaque
   credential.
4. Racer on node A maps the requested range to pages and asks each page's
   owner. Pages already in node A's memory are served locally.
5. The owner (node B) fills a missing page: memory, then its own Gantry with
   a local-only request (containerd file on node B), then holder nodes, then
   other owners, then its Gantry with a normal request. Only in the last step
   does any node contact the registry, and it uses the caller's credential,
   which Racer carried encrypted from node A to node B. Node B records node A
   as a holder.
6. Pages stream back to node A. Gantry writes them to containerd while it
   hashes the blob. On a mismatch it aborts and invalidates.
7. containerd commits the blob in its content store. From now on node A's
   Gantry can serve local-only requests for it, and owners list node A as a
   holder.

A later pull of the same blob on a new node, after owners have evicted the
pages from memory, is served from node A's content store, not from the
registry.

## Auth model

- Racer does not interpret credentials. It carries them encrypted between
  nodes and hands the clear value only to the owner's local origin socket.
- Racer serves cached pages without any credential check. With only Racer, a
  caller with no access to a private repo could read a blob another tenant
  pulled, if it knew the digest. Gantry closes this by running the upstream
  access check before every Racer read. The registry stays the only authority.
- A local-only request reads only data the node already holds and ignores the
  credential. It is reached only through Racer after the requesting Gantry
  has passed the access check.
- The access check cache is keyed by a hash of the credential, so one
  caller's success never authorizes another credential.
- Anonymous access is checked like any other: the HEAD is sent without a
  credential.

## Failure handling

| failure | result |
|---|---|
| Gantry down on the pulling node | mirror dial refused; containerd pulls from the registry |
| Racer down or client socket error | Gantry answers 503; containerd pulls from the registry |
| access check: registry 5xx or unreachable | 503; containerd pulls from the registry |
| access check: 401 / 403 / 404 | same status to containerd (401 with challenge) |
| holder answers miss or errors | holder dropped; owner tries the next source |
| owner evicted the page, no holder has it | owner refetches from the registry |
| owner node lost | Racer ranks the next owner; it fills from holders or the registry |
| containerd GC removed the file during a local read | short or failed read; treated as a miss; next source |
| bad bytes from any source | SHA-256 mismatch; connection aborted; `Invalidate`; containerd retries |
| registry 401 on fill at the owner | Racer returns unauthorized; Gantry answers 401 with challenge |
| tag request | 503 as today; containerd resolves upstream |

## Memory budgeting

- Adapter pages share Racer's memory pool with other caches, weighted by
  `spec.priority`. They never move to disk; evicted pages are refilled.
- In a cold rollout every page is in flight or hot on its owner at about the
  same time, so peak Racer memory for the cache is about the rollout's working
  set divided across owners, bounded by Racer's per-cache memory share.
- The holder table costs a few node ids per page plus a global cap; it is
  small next to page memory.
- Gantry adds the access check cache (bounded entries, short TTL) and one
  SHA-256 state per open blob stream. The local replica handler reads the
  containerd file with bounded buffers and keeps nothing.

## Security considerations

- The containerd content root is mounted read-only. Gantry cannot change
  images on the node.
- Local-only reads open only `blobs/sha256/<hex>` with a validated 64-hex
  name, so a key cannot escape the content root.
- Bytes from any node are checked by SHA-256 before containerd sees a
  complete blob, and containerd checks again on commit.
- Credentials are never stored on disk, never put in Racer metadata, and only
  hashed in the access check cache key.
- The holder table holds node ids only, never credentials or data.
- Racer peer traffic stays encrypted and authenticated as today. In Adapter
  mode no image page is written to Racer's disks, so encrypted-at-rest pages
  do not apply to this cache; the at-rest copy is containerd's, as without
  Gantry.

## Testing

- Racer unit and simulation tests: persistence validation (`Adapter` rejected
  with `passthrough` and `origin`), no slab writes for Adapter caches, fill
  order, holder table bounds, TTL and drop-on-miss, local-only flag on the
  wire, invalidate with matching and stale ETag.
- SDK tests: `OriginRequest.LocalOnly` decoding, `Client.Invalidate`.
- Gantry unit tests: access check status mapping and cache keying (two
  credentials never share a result), local replica handler (hit, miss, short
  read, file removed mid-read, bad name), digest mismatch aborts and calls
  `Invalidate`.
- Operator tests: Adapter persistence on `ClusterCache/gantry`, read-only
  content root mount.
- e2e (kind): private repo pulled by an authorized pod, then denied to an
  unauthorized pod on another node; second wave served from holders with no
  registry egress.
- Scale: `hack/gantry-cluster-bench` (below).

## Benchmarking Racer mode

`hack/gantry-cluster-bench` drives the operator-managed Gantry. See its
README for details.

```bash
cd hack/gantry-cluster-bench
export REGISTRY_NODE=<origin node> TARGET_SELECTOR=<label selector>
./registry.sh up && ./registry.sh install-tool && ./prep.sh apply

./deploy.sh default;  DROP_CACHES=1 ./clean.sh; ./registry.sh gen r1
./run.sh default-r1 "$(./registry.sh addr)/bench/g40:r1"
./deploy.sh adapter;  DROP_CACHES=1 ./clean.sh; ./registry.sh gen r2
./run.sh adapter-r2 "$(./registry.sh addr)/bench/g40:r2"
./deploy.sh racer;    DROP_CACHES=1 ./clean.sh; ./registry.sh gen r3
./run.sh racer-r3 "$(./registry.sh addr)/bench/g40:r3"

go run ./hack/cmd/gantry-cluster-bench compare tmp/gantry-cluster-bench/{default-r1,adapter-r2,racer-r3}
```

What to look at:

- origin egress: about 1.00x the image size for `adapter` and `racer`;
- node disk writes: `adapter` should match `default` (containerd only);
  `racer` adds the slab copy;
- pull p50/p99 and makespan;
- a second wave (`NODES=<subset>` then the rest, same tag) should show
  about 0x origin egress in `adapter` mode.

## Relationship to PR #1106 (cachemesh)

PR #1106 added `internal/cachemesh`, a transport-agnostic routing library,
and an optional striped distribution path in Gantry built on it. Review asked
for it to be a standalone component, with a routing interface later. This
design takes the parts that worked and builds them on Racer instead.

Kept:

- containerd's content store as the only disk copy, with peers serving from
  it (the "containerd content store as the only disk tier" result of that
  PR).
- Asking holders before the origin.
- The benchmark harness, now `hack/gantry-cluster-bench` for Racer mode.

Dropped:

- The cachemesh routing library, its grid, leader election and partition
  table. Racer already ranks owners and moves pages between nodes.
- Anonymous tag caching. Tags stay upstream-resolved.
- Gantry-private chunk files and chunk sizes; Racer pages replace them.

### Historical cachemesh numbers

These numbers are from the cachemesh prototype on 2026-11-09, not from Racer.
They show what the containerd-as-disk idea achieved in that prototype and
set a target for Racer `adapter` mode. Setup: 142 workers (64 CPU, 30Gb/s NIC),
one `registry:3` origin capped at about 3.7GB/s, a 40GB image of four 10GiB
incompressible layers, all nodes pulling at once.

| mode (cachemesh prototype) | pull p50 | pull p99 | makespan | origin egress |
|---|---|---|---|---|
| Gantry default (DHT p2p) | 235-247s | 306-311s | 313-317s | 8.01x |
| cachemesh, 32-neighbor relay grid, memory cache | 90.1-91.9s | 96.0-97.4s | 96.2-98.9s | 1.00x |
| cachemesh, containerd store as only disk copy | 91.8-92.3s | 97.3-97.6s | 97.8-101.8s | 1.00x |
| one node straight from origin (baseline) | 94.7s | | | 1x per node |

In the containerd-store runs Gantry wrote 0 bytes to disk, peaked at about
2.5GiB RSS, and a second wave of 71 nodes pulled the same image 3 minutes
later with 0.00x origin egress (served from the first wave's content
stores). About 69s of every pull is containerd unpacking layers, which no
distribution path can remove.
