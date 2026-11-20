# Racer ClusterCacheFill

Status: draft, implemented with the limits in [Limits](#limits). This document
is the spec for the API, the control plane, and the dataplane. Where it
disagrees with older prose, this document wins (see
[Superseded statements](#superseded-statements)).

## Summary

`ClusterCacheFill` is a cluster-scoped resource that pre-fetches a set of
objects into a `ClusterCache` and keeps them resident. Unlike a prefetch
ticket, which is best effort, a fill is an admitted reservation: while a fill
is `Ready`, its pages are protected from capacity eviction, and pages lost to
node replacement or restart are fetched again.

A fill names blob-name prefixes, not Racer keys. Racer keys are hashes of the
canonical object identity and have no meaningful prefix, and Racer has no
object index. The control plane therefore lists the cache's object storage to
expand a prefix into objects before it pins anything.

Only caches that read directly from object storage (`spec.origin`, Azure Blob
today) are supported. A cache served by a self-deployed origin, or a
passthrough cache, is rejected with `UnsupportedOrigin` or
`UnsupportedCache`.

## Goals and non-goals

Goals:

- Reserve disk for a named object set and keep it there across eviction
  pressure.
- Pack pinned pages so a small fill does not pin most of the disk.
- Self-heal after node replacement, dataplane restart, key rotation, or a
  manifest change.
- Report the reservation and its progress so users can track disk use.
- Keep the dataplane's read path unchanged for clients and keep pins out of
  the spill and prefetch budgets.

Non-goals:

- Reads bypassing the normal path. Filled objects are served from the same
  page cache as any other read.
- Guaranteed origin isolation. Pinned pages serve reads, but a page whose
  page key has been pruned is refetched from the origin.
- A cluster-wide hard quota enforced by a single scheduler. Admission is a
  plan; each node enforces its own budget.
- Self-deployed origins, non-Azure providers, or caches without `spec.origin`.
- Listing more than 8192 objects in one fill.
- Compacting a partly pinned segment after a fill shrinks. Unpinned segments
  are reclaimable once their last pin is released.

## API

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCacheFill
metadata:
  name: llama-70b
spec:
  cacheName: models
  prefixes: ["meta-llama/Llama-3-70B/"]
  replicas: 2
  maxFootprint: 322122547200   # 300 GiB, in bytes
  resyncInterval: 1h
status:
  observedGeneration: 1
  cacheUID: 5b0f...
  manifest:
    objects: 30
    logicalBytes: 140000000000
    digest: 9c2e...
    listedAt: "2026-11-19T10:00:00Z"
  footprintBytes: 151000000000
  residentBytes: 150000000000
  underReplicatedBytes: 1000000000
  nodes: 8
  conditions: [...]
```

Defaults and validation:

| Field | Default | Limits |
| --- | --- | --- |
| `spec.cacheName` | required | immutable; must name an existing `ClusterCache` |
| `spec.prefixes` | required | 1 to 16 distinct entries, each at most 1024 Unicode characters; overlapping prefixes allowed; empty string selects the whole container |
| `spec.replicas` | 2 | 1 to 3 |
| `spec.maxFootprint` | required | positive int64 byte count, expressed as a YAML number |
| `spec.resyncInterval` | 1h | 1m to 24h |

`maxFootprint` counts pinned payload, not object size: every object's length
rounded up to a page, times `replicas`, summed. Actual disk use can be higher
because a pinned segment keeps its unused tail until the last pin in it is
released; `RACER_PINNED_SEGMENT_PERCENT` bounds that overhead. A selection that
would exceed `maxFootprint` is rejected with `ExceedsMaxFootprint`; it is never
truncated. This checks the fill's own limit, not available cluster capacity;
each node enforces its local pin budget separately.

## Listing and the manifest

Every process lists each fill's origin independently, with the cache's
credential:

Prefixes are a set of distinct strings, each at most 1024 Unicode characters.
Overlapping prefixes are allowed and matching objects are deduplicated. This
prefix limit is separate from the byte-based object identity limit below.

1. `GET <containerURL>?restype=container&comp=list&prefix=<p>&marker=<m>`
   for each prefix, following `NextMarker` until the list is complete.
2. For every blob, build the canonical Racer identity
   `{"schema":1,"bucket":<container>,"key":<blob name>}` in Go `encoding/json`
   form, hash it with SHA-256 to get the Racer key, and send the same JSON as
   `Racer-Metadata` on reads. This is the same identity the Go SDK, the
   `racer-object` sidecar, and the Python client compute.
3. Take the ETag, length, and content type from the listing. Azure versioned
   blobs are not expanded; a fill pins the current version of each blob.

Every selected blob must satisfy Racer's schema-1 identity contract: a
nonempty UTF-8 name of at most 1024 bytes, canonical identity JSON of at most
8192 bytes, and an ETag accepted by Racer's strong ETag wire type. Names are
not decoded or normalized. Azure's name limit counts characters, so a legal
Azure name containing multibyte characters can exceed Racer's byte limit.
Such archives are unsupported as a whole-fill selection: one unsupported
selected object fails the entire listing with `ListFailed`, retaining any
previous manifest for retry. Racer never silently omits it and claims a
complete reservation. Rename those objects or explicitly narrow the prefixes
to a supported subset. A logical-byte total that exceeds `u64` also fails
the listing instead of wrapping or publishing an inexact total.

The manifest is the sorted list of `(key, metadata, etag, content type, length)`
entries. Its digest is the SHA-256 of a canonical JSON array of
`{"etag","key","length"}` objects, in key order; the content type travels
outside the digest because it is not part of a page's identity. Two processes
that list the same object versions produce the same digest.

The primary and each cache's pool members list independently. Other nodes wait
without listing; pool membership and admission inputs come from the publication
the local dataplane adopted, not an independent selector evaluation. The primary
lists even when it is outside the pool so it can write status. It writes its digest to
`status.manifest.digest`; a node includes its own digest in its progress
report, and the primary counts only reports whose digest and spec generation
match its own. A node that listed a different set does not corrupt the
accounting; it is not counted until its listing aligns. A non-primary process
uses the primary's status digest for the current spec generation and cache UID
as a convergence hint. A successful but different listing retries after a
bounded delay instead of waiting for `resyncInterval`; repeated mismatches do
not spin. Failed lists retain their own backoff. This does not distribute an
immutable manifest: a continuously changing origin can still prevent agreement,
and a node cannot reconstruct object versions no longer returned by the origin.
Matching listings otherwise re-list at `resyncInterval`.

A plan-only fill edit, such as raising `maxFootprint` or changing `replicas`,
reuses the valid manifest and listing schedule. Admission and the footprint are
recomputed for the new generation before the view is exposed; `listedAt` still
records the original listing completion. Changing prefixes, the named cache,
the cache UID, or the container URL invalidates the manifest and schedules a new
listing. A listing from a superseded container URL cannot replace the new plan.

This trades independent listing per participating process for no manifest wire
format, ConfigMap, or manifest-distribution RBAC. Listing needs a credential with list
permission on the container, which is broader than blob read. The cache's existing
credential is used; a fill on a cache whose credential cannot list is rejected
with `Forbidden` rather than falling back to a partial set.

## Pinning

A pin is a long-lived hold on an object version, renewed every 5 minutes with a
1 hour TTL. Renewal makes the pin self-healing: a restart, a lost checkpoint,
or a pruned page key is repaired by the next reconcile instead of failing
permanently.

Pinned writes go to a **pinned segment class**:

- Each worker keeps one open pinned segment in addition to its normal open
  segment. A page published with a pin flag is appended there, so pins from
  one fill, and from several fills, pack together instead of scattering
  across normal segments.
- A segment containing any pin is never reclaimed, whether or not a page
  still carries an explicit hold. Reclaim skips non-empty pinned segments and
  normal segments with held pages, as it does today.
- A sealed pinned segment with no remaining pins becomes ordinary sealed
  capacity and is reclaimed normally. There is no compaction: a partly used
  pinned segment stays reserved until its last pin is released.
- Pinned pages do not count against `RACER_RETAINED_SEGMENT_PERCENT` (the
  spill cap). They count against a new per-worker budget,
  `RACER_PINNED_SEGMENT_PERCENT` (default 50 percent of segments), and a fill
  whose pins do not fit is reported as `CapacityLost` rather than evicting
  client data silently.

Pinning an object proceeds page by page. For each page the node owns, it
reserves a pin, then fetches the page through the normal dispatch and flight
path if it is missing, using the manifest's version and content type. A page
that is already resident is pinned without an origin read. The reservation is
released if the fetch fails. A page whose page key is no longer readable is
released and fetched again, so a fill survives key rotation.

## Ownership, replicas, and self-healing

Page ownership is weighted rendezvous hashing over the cache's pool, as for
every other page. A node pins a page when its rank is below `spec.replicas`.
With replicas 2, the two highest-ranked owners each pin a copy; losing either
node leaves one copy and a new owner to refill.

- **Node replacement**: membership is keyed by Node UID. A node that is not
  Ready for 30 seconds leaves the pool, which changes ownership of the pages
  it held. New owners pin the pages they now own, fetching from surviving
  owners first and from the origin only if no copy remains.
- **Node return**: a returning node with its disk intact re-pins the pages it
  still has; the nodes that filled in release their copies when they adopt the
  new membership version.
- **Dataplane restart or cold start**: the reconciler pins owned pages again.
- **Ownership change**: a node discards its plan and releases the pins for
  pages it no longer owns when it adopts the new membership version. The new
  owners then pin the pages they now own. This is not a make-before-break
  handoff, so a page can be briefly unpinned during a membership change; status
  shows the shortfall through `underReplicatedBytes` and `Degraded`.
- **Fill deletion**: every node releases the fill's pins when the object is
  gone. Data leaves through normal eviction; it is not deleted outright.

Reads are unchanged. A client read of a filled object hits the same pages; a
pinned page is simply one that eviction cannot choose.

## Status

Runtime coordination selects the primary to write the status subresource with
`update`. All Racer Pods share a ServiceAccount and therefore hold that same
permission. Neither RBAC nor admission restricts status updates to the current
primary; primary ownership is not a security boundary.

Every member reports, in its separate Pod
`racer.unbounded-cloud.io/fills` annotation, one entry per fill it is
reconciling:

```json
{"fill": "<uid>", "generation": 3, "digest": "9c2e", "membership": 17,
 "owned_pages": 120, "pinned_pages": 118, "owned_bytes": 5000000,
 "pinned_bytes": 4900000, "refused_bytes": 0}
```

The primary sums the entries whose `generation`, `digest`, and `membership`
match its own. Fill reports use the separate 64 KiB fills annotation; the
membership report retains its 32 KiB limit. Report-slot admission rejects
excess fills with `TooManyFills` instead of silently omitting admitted fills
from progress reporting. Each node has at most 16 report slots.

Conditions:

| Type | True when | Reasons |
| --- | --- | --- |
| `Accepted` | The cache exists, has a supported origin, is not passthrough, and the planned footprint is at most `maxFootprint` | `CacheNotFound`, `UnsupportedCache`, `UnsupportedOrigin`, `ExceedsMaxFootprint`, `TooManyObjects` |
| `Listed` | The current generation has a manifest | `ListFailed`, `Forbidden` |
| `Progressing` | The first fill has not completed | `Filling` |
| `Ready` | Every page has its replica copies confirmed by the counted reports | `Complete` |
| `Degraded` | The fill was Ready (or already Degraded) and is now short of copies, or `spec.replicas` exceeds the cache pool's node count | `MemberLost`, `Refilling`, `CapacityLost` |

`footprintBytes` is the planned reservation. `residentBytes` is the summed
page-rounded payload of confirmed pins. `underReplicatedBytes` is
`max(0, footprintBytes - residentBytes)`, so it counts missing nodes, node
capacity refusals, and pages a worker has not visited yet. A `Ready` fill has
zero `underReplicatedBytes`.

`Degraded` does not mean reads fail. Reads fall back to peers and then the
origin as usual; it means the reservation no longer holds at the requested
replica count.

## Failure modes

- **Origin listing fails or is forbidden**: `Accepted` stays true if admission
  already succeeded, `Listed` becomes false, and the previous manifest keeps
  serving the fill until the next successful resync.
- **Listing is too large or contains an invalid object**: retain the previous
  manifest and retry at `resyncInterval`, not on the transient-failure cadence.
  A spec or cache identity change resets that deadline. Transient listing
  failures retry with capped exponential backoff and jitter; a primary digest
  hint does not override a failure deadline.
- **Origin unavailable while filling**: missing pages stay missing; the fill
  stays `Progressing` or `Degraded` and retries on the next reconcile.
- **Pinned budget exhausted**: the node stops pinning, reports
  `refused_bytes`, and the fill becomes `Degraded` with `CapacityLost`.
- **Page key pruned before renewal**: the page is unreadable, so it is
  released and fetched again. The guarantee is "pinned and served from disk
  when readable", not "never touches the origin again".
- **Loss of every copy**: reads go to the origin, and the fill repairs at the
  next reconcile.
- **Membership churn**: ownership follows membership; a node releases pins it
  no longer owns on its next 2 second refresh, so a page can be briefly
  unpinned while a new owner takes over.

## Limits

- Azure Blob origins only, and only caches with `spec.origin`.
- A workload-identity cache must set
  `spec.origin.azure.credential.workloadIdentity.tenantID`. The listing path
  exchanges the projected token for that tenant directly and does not discover
  a tenant from a storage challenge, so without it the fill reports
  `Forbidden`.
- One object set per fill, at most 8192 objects. A larger listing is rejected
  with `TooManyObjects` instead of being truncated.
- At most 16 fills can occupy report slots on a node. Excess fills are rejected
  with `TooManyFills`, not silently removed from reports.
- Pins are node-local. Status is the per-fill accounting surface; per-fill
  Prometheus metrics are not added yet, and the existing per-cache disk
  metrics continue to count pinned pages as resident cache data.

## Superseded statements

- `designs/racer-prefetch.md:14-15` says "No pages are pinned". That remains
  true for prefetch tickets. Fills are a separate mechanism and do pin.
- `designs/racer-prefetch.md:235-237` says prefetch success "is not a
  residency guarantee". Fills are the residency guarantee; prefetch stays
  best effort.
- `designs/racer-cache-priority.md:35` lists "Hard quotas or reservations" as
  a non-goal. Priority shares remain soft, but a fill's pinned segments are a
  reservation and are not shared by priority.
- `docs/content/reference/racer.md` says `ClusterCache` has no status. Fills
  add a status subresource to a new resource; `ClusterCache` still has none.
