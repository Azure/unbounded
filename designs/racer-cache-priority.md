# Racer cache priority

## Summary

`ClusterCache` gets an optional `spec.priority`. It is a relative weight that
decides how ClusterCaches share cache capacity on each node. A cache with a
higher priority gets a larger share of disk, pending writes, and memory. Under
pressure, caches that use more than their share lose data first.

Priority is a weight, not a quota. Idle capacity is still used by any cache.

## Problem

All ClusterCaches on a node share one disk store, one pending write queue, and
one set of memory budgets. Today eviction ranks pages only by retention score
(heat plus an owner bonus) and age. Memory flow control splits limits equally
across caches. A busy, low-value cache can push out the working set of a
smaller, more important cache. Operators have no way to say which cache matters
more.

## Goals and non-goals

Goals:

- Let operators set a relative share of node cache capacity per ClusterCache.
- Apply the same weight to disk eviction, disk admission, the pending write
  queue, and memory flow control.
- Keep using idle capacity. A cache can use more than its share while no other
  cache needs the space.
- Change priority on a live cache without a restart.
- Keep per-worker work bounded and lock-free, as today.

Non-goals:

- Hard quotas or reservations. No cache gets a guaranteed byte count.
- Cross-node coordination. Each node, and each worker, decides alone.
- Mixed versions. The controller and dataplane must be upgraded together.
- Removing exact bytes of one cache from a shared segment.

## API

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: models
spec:
  priority: 900
```

| Field | Type | Rules |
|-------|------|-------|
| `spec.priority` | `int32` | Optional. 0 to 1000. Unset means 500. Mutable. |

The CRD has no default. The controller resolves an unset value to 500 when it
builds the catalog, so the stored object keeps what the user wrote.

## Semantics

Let `w` be the resolved priority of a cache, and `W` the sum of `w` over all
caches in the published catalog. For each resource with capacity `C`, a cache's
fair share is:

```
share = C * w / W
```

If `W` is 0 (all caches have priority 0), all shares are equal.

Shares are computed per dataplane I/O worker, using that worker's capacity.
Pages are hash-distributed across workers, so per-worker shares approximate
node-wide shares.

A cache whose resident usage is above its share is **over share**. Otherwise it
is **under share**.

Rules:

- Under pressure, eviction prefers over-share candidates within bounded scans.
- Within the same class (over or under share), the existing retention score
  (heat 0 to 3, plus 1 if this node is the placement owner) decides, then age.
- Hard holds (warm and spill) are never evicted, as today.
- Against positive-priority competitors, priority 0 has no weighted share but
  can use idle capacity. Working-memory floors and allowances still apply.

## Per-tier mechanics

### Disk eviction

Page-index eviction ranks a bounded set of candidates by the tuple
`(under_share, score, age)`. It does not search for the best page overall.

Retention keeps resident caches ordered by resident bytes per unit of weight.
Over-share caches are always a prefix of this order, so each eviction takes the
over-share caches among the top 64, without classifying every cache. The order
changes in O(log C) when a cache's resident bytes change and is rebuilt only
after a priority catalog change.

Each eviction splits a 64-page budget across those caches and scans each
through its own age order with its own cursor, so older under-share pages
cannot hide them. If no unheld page is found, it ranks the next 64 pages of the
global age order. An under-share page can then be evicted even though an
unheld over-share page exists outside the candidates, for example when the
scanned pages of every candidate cache are held, or when more than 64 caches
are over share.

Segment reclamation works on whole segments, and a segment holds pages from many
caches. Segment value weights under-share pages heavily, so segments that hold
mostly over-share data are recycled first. While no cache is over share, segment value
is the plain retention score, as before. Priority cannot remove only one
cache's bytes from a shared segment; under-share pages in a recycled segment are
lost with it.

### Pending write queue

When the queue is full, replacement ranks by `(under_share, score)`. An
unheld over-share page never displaces an under-share page, either through
replacement or through reclaim of optional writes for an owned page's quota.
A held incoming page keeps its existing exemption. Queued held writes cannot
be evicted. Write order uses the same ranking.

### Memory

Plaintext and ciphertext flow control give each cache a fair limit. The equal
split is replaced by:

```
equal = limit / active_caches
cache_limit = clamp(max(limit * w / W, min(progress, equal)), floor, limit)
```

Plaintext and ciphertext are also request working memory. `progress` is one
range window plus one page, the same amount config validation requires of the
whole budget (dirty ciphertext has none). So priority 0 keeps
`min(progress, equal)`, or the floor if that is larger.

This does not promise that every read makes progress. With many active caches
or a small total budget, the equal split can be smaller than `progress`, and a
read can still be rejected with Overloaded.

Only the memory classes plaintext (reader working memory), ciphertext, and
dirty ciphertext are weighted. Request-serving classes (request context,
registered buffers, flights, waiters, connections, pipes, control progress,
and relays) keep the equal split across active caches. Priority describes
retention, not request service, and a priority 0 cache must still be able to
reserve the full request context needed to read a request while a
higher-priority cache is busy.

The memory cache retains only ciphertext. Plaintext is working memory held by
readers, so plaintext pressure frees nothing from the cache. When a cache is
above its own limit, it reclaims only its own idle ciphertext. When the total is
full but the requester is within its limit, reclaim first takes idle
ciphertext, and queued write copies, from caches above their limit. It then
falls back to plain age order. Each worker keeps a per-cache age order for
verified and for unverified ciphertext, updated in O(log C + log entries) on
insert, hit, and removal. Each reclaim checks at most 8 caches from a rotating
cursor and scans at most 256 entries of each kind per cache. It does not rank
all caches. So a cache outside this window, or one whose scanned entries are
all busy, can let an under-share page go first. The next reclaim resumes after
the last cache examined. When each reclaim examines a full window, every cache
is checked within `ceil(C / 8)` reclaims; a reclaim that ends early advances
by fewer caches but skips none. Busy entries are never evicted. The optional
persistence headroom check uses the same order, with no queued copies
discarded.

### Disk admission

Optional (non-owned) pages are rejected when their cache is over share and the
worker has no free segments beyond the reserve. Owned pages are always admitted
and compete through eviction.

## Accounting

Each worker tracks indexed payload bytes per cache:

- Add on retention publish, subtract on evict, using the existing retention
  hooks.
- Clear the entry when the cache is removed from the catalog.
- Rebuild from restored index mappings on checkpoint recovery.

Accounting uses logical payload bytes, the same unit as
`racer_disk_indexed_payload_bytes`. It does not include headers, padding, or
records whose mappings are already gone.

## Live updates

The controller compares priority when deciding whether the catalog changed, so a
priority change republishes the catalog. Workers recompute shares when they
apply a new catalog. Existing data is not moved or dropped at once. Pages leave
through normal eviction as pressure appears.

## Wire format and upgrade

`CacheDefinition` gains a required `priority` field. The schema version stays 1.
Decoders are strict, so an old dataplane rejects a new catalog and a new
dataplane rejects an old one. Upgrade the controller and dataplane together.
Racer is pre-release and cached data is disposable; there is no mixed-version
path.

## Observability

New per-cache gauges, labeled by cache name. Series exist only for caches in
the published catalog:

| Metric | Meaning |
|--------|---------|
| `racer_cache_disk_resident_bytes{cache}` | Indexed payload bytes for the cache, summed over workers |
| `racer_cache_disk_share_bytes{cache}` | Fair-share bytes for the cache, summed over workers |

The unlabeled counter `racer_retention_share_rejected_total` counts optional
pages skipped by disk admission because their cache was over share.

A worker's disk capacity for shares is the smaller of index entries times the
page size and slab segments times the segment size. A cache whose share is the
whole capacity (a single cache, or before any catalog is published) is never
over share.

A cache is over share on a worker when its resident bytes exceed its share on
that worker. The node sums can hide per-worker differences.

## Testing

- API: validation of range 0 to 1000, unset handling, and mutability.
- Controller: unset resolves to 500; a priority change republishes the catalog.
- Wire: encode and decode with `priority`; strict decode rejects a missing field.
- Share math: weighted shares, all-zero equal split, single cache, and removal.
- Accounting: publish, evict, cache removal, and checkpoint rebuild.
- Each tier: bounded scans prefer over-share victims; score and age still order
  within a class; holds are never evicted; an unheld over-share page never
  displaces an under-share page in the queue; memory limits follow
  weights with floor and max; non-owned admission is rejected only when over
  share and free segments are at the reserve.
- Deterministic simulation: two caches with priorities 900 and 100 under steady
  churn converge toward a resident ratio near 9:1. Priority 0 next to a nonzero
  cache keeps only idle capacity.
- Live update: change priority mid-run and check the ratio moves toward the new
  weights without restart.

## Alternatives considered

- **Strict tiers.** Evict all lower-priority data before any higher-priority
  data. Simple, but one high-priority cache can starve all others, and it gives
  no proportional sharing.
- **Score boosts.** Add priority to the retention score. It mixes two meanings
  in one number, and a cold high-priority page could outlive a hot page with no
  limit on how much space the cache takes.
- **Per-cache segment pools.** Give each cache its own segments. This allows
  exact eviction, but it wastes space through fragmentation and partly filled
  segments, and it makes idle capacity hard to lend out.
