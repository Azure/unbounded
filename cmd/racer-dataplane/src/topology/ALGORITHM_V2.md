# Racer topology algorithm v2

This document specifies `ALGORITHM_VERSION = 2`. Hash domains, field order,
integer arithmetic, ordering, and tie rules are interoperability contracts.
Changing them requires a new algorithm version and coordinated deployment.
Membership wire versions are snapshot counters, not algorithm versions.

Version 2 changes shortest-route tie selection and removes the total-work
configuration. Deploy participating dataplanes together; there is no dual-version
route implementation or algorithm negotiation. The algorithm constant identifies
this specification, not a wire negotiation field. The existing peer/control
encodings, placement hashes, ciphertext forwarding, graph edges, and RDMA rail
selection remain the same; their v1 hash domains are retained.

## Membership

The controller publishes membership; readiness and local link health never
change it. Validate a nonzero version, at most 100,000 members, positive u32
shares, nonempty ASCII graphic node IDs of at most 256 bytes, nonempty UTF-8
fabric names without NUL/CR/LF, IP socket endpoints with nonzero port and no
zone identifier, unique node IDs, and unique u16 rail IDs within each member.
Optional NUMA IDs must fit unsigned u32. Sort members by
node ID bytes and rails by unsigned rail ID. No normalization changes identity.
Empty memberships are valid, with zero candidates and no routes.

The control codec validates canonical UUID node identities and the aggregate
64 MiB publication bound before topology admission. The internal topology API
also supports short opaque node IDs for algorithm fixtures. Fabric names have
no additional local length cap, ASCII restriction, trimming, or Unicode
normalization. Unspecified/multicast endpoint addresses are wire-valid;
connectivity failures belong to transport health rather than membership
validation. These rules match `internal/racer/wire/validate.go` (`validRail`
and `validatePublication`) and `CONTROL_API.md`'s codec contract.

Members are immutable through one canonical `Arc<Membership>` per published version.
Cache-only publications reuse that allocation. Existing operation leases keep old
generations usable; the control publication owner admits at most the configured
old-generation limit plus the current generation. Its sole incoming-version registry
holds weak references; outgoing routes and endpoints use operation leases directly.
Topology caches hold only weak snapshot
references. A cache identity includes the immutable allocation, preventing
accidental reuse across different snapshots with the same version counter.

## Canonical encoding and slots

Use SHA-256. All integers are unsigned big endian. Variable fields use a u32
byte length followed by exact bytes. Domains below include the terminating
NUL byte. Cache IDs, node IDs, and strong ETags use their exact string bytes;
ETags include the HTTP quotes. Keys are 32 raw bytes, never hex text.

`object_page = len(cache_id) || cache_id || key[32] || page_number:u64`

`slot_hash = SHA256("racer/slot/v1\0" || object_page)`

Interpret the first four digest bytes as a big-endian u32 and shift right by
12. This is one of exactly 2^20 slots. ETag, membership counter, endpoints,
rails, health, and shares are absent. Metadata uses page zero.

## Weighted HRW

For each sorted member, exactly once:

`h = SHA256("racer/hrw/v1\0" || slot:u32 || len(node_id) || node_id)`

Interpret the first eight bytes as a u64 sample. Compute the Q32.32 integer
cost approximating `-log2((sample+1)/2^64)` as follows. This paragraph's
logarithm explains the distribution only; implementations use the integer
procedure, never a platform logarithm.

1. Set `v = u128(sample)+1` and `e = floor(log2(v))`, using bit length.
2. If `e == 64`, cost is 1 (strictly positive endpoint clamp).
3. Otherwise set `x = v << (63-e)`, `f = 0`.
4. For `b = 31,30,...,0`, set `x = (x*x) >> 63` in unsigned u128 arithmetic.
   If `x >= 2^64`, set `x = x >> 1` and `f = f | (1 << b)`.
5. Cost is `((64-e) << 32) - f`, an unsigned u64.

Rank ascending by cost/shares without division: compare `cost_a*shares_b`
against `cost_b*shares_a` in u128. Exact ties use ascending node ID. Retain
only the best three distinct members. Complexity is O(N) with O(1) score
storage, independent of shares; no virtual-node expansion exists. Finite hash
precision and the specified fixed-point truncation define the distribution.

The bounded FIFO ranking cache contains at most its configured number of
slots. Active entries cannot be evicted: if all entries are active, a new
cold slot fails `Overloaded`. `rank_async` shares partially completed work
for a resident snapshot/slot, hashes at most 256 members per poll, and yields.
Cancellation releases active admission; a later request can resume progress.
Capacity zero explicitly disables caching/coalescing. The synchronous `rank`
method is for callers that can budget the complete O(N) computation; reactor
paths should use `rank_async` and check their request scope around it.

## Graph and bounded shortest paths

For sorted position i and N members, outgoing neighbors are `(18*i+j)%N`
for j=0..17. Incoming neighbors are `floor((i+k*N)/18)` for k=0..17.
The latter enumerates exactly the inverse edges by lifting i into [0,18*N).
Union both sets, remove self and duplicates, and sort numerically. This uses
36 candidates regardless of N and produces at most 36 symmetric neighbors.

Search alternates complete BFS layers, starting from the source. Each wave has
a FIFO queue and a sparse visited map holding the first-discovery parent. Visit
neighbors in ascending sorted position, never replace parents, and check the
opposite visited map immediately on discovery. Return at the first intersection,
reconstructing just that path. A yield never changes the layer or queue order.
Before each layer the visited balls are disjoint, so an intersection must have
distance equal to the sum of their previous radii plus one: any shorter route
would already have intersected. This chooses a deterministic shortest path,
without globally lexicographic equivalent-path enumeration.

Stop after L total layers (source radius ceil(L/2), destination radius floor(L/2))
or when either wave's reachable component is exhausted. Each side discovers a
node at most once, and expands only nodes inside its radius. Visited nodes from
prior senders are excluded; shortest paths cannot contain a loop. There is no
membership-wide initialization, final scan, or all-pairs table. Sparse maps and
queues use O(V) scratch storage for explored vertices, at most O(N) per search.
Each expansion examines at most 36 edges; ordered map operations cost O(log V).
Thus worst-case search work is O(N log N) with at most 2N expansions. Healthy
four-link routes expand at most 2*(1+36) = 74 vertices and store at most
2*(1+36+36*35) = 2594 visited entries, independent of membership size.

Only the source's incident failed links are excluded: local health does not
claim the neighbor is unreachable through another node. Every relay replans
using its own local information. A route cache key includes the snapshot,
source, destination, remaining links, visited set, and unavailable source
links. Successful routes are bounded FIFO entries; failures are not cached.
Concurrent cold searches per worker are limited to `clamp(cache_capacity, 1, 8)`
to bound aggregate scratch memory; saturated admission fails `Overloaded`.
The 100,000-member validation ceiling also bounds failed eight-link searches.
Dropping an operation releases its scratch state and admission immediately.

Normal admission chooses 4 links; failure-tolerant admission may choose 8.
An already admitted attempt never increases its budget. `visited` contains
prior senders and excludes the current recipient. A forward appends the
sender, decrements remaining links exactly once, and preserves identity,
destination, membership and deadline. `validate_forwarded` permits a shorter
deadline, never a longer one. It rejects loops, changed bindings, and budget
extension. The sum of visited count and remaining links is at most 8.
The signature verifier must invoke this check on authenticated successive
hop states. It must not create fresh budgets when rerouting the same attempt.

The peer signature profile represents outbound wire state with the sender
already in `visited` and the outgoing link still included in `remaining_links`.
`peer::search_budget` converts it to this search convention: at the originating
sender it removes that sender from `visited`; at a recipient it decrements
the incoming link first. The security verifier enforces its own equivalent
monotonic wire-state transitions. Do not mix wire and search conventions or
decrement a link twice. `RouteBudget::forwarded`/`validate_forwarded` describe
search-state transitions, not a replacement for the peer wire conversion.

Expired deadlines fail `DeadlineExceeded`; unknown nodes or versions fail
`IncompatibleMembership`; loops/malformed state fail `InvalidRequest`; zero
remaining links before reaching destination fails `HopBudgetExhausted`.
No admissible path within the remaining links fails `Unavailable`. There is no
configurable total-work allowance: visited sets and hop limits bound the search.
`shortest_async` yields after at most 32 vertex expansions (1152 edge visits),
checks request-scope cancellation and deadline each poll, checks the route deadline
before each vertex expansion, and validates current local health again before
returning. A canceled scope returns `Cancelled` and releases admission without
caching a route. Dropping the future also cancels computation. The synchronous
API runs the same search and checks the route deadline before each expansion.

## Local circuits

Each `LinkHealth` instance belongs to one worker/source. A failure opens a
neighbor circuit immediately. Failure k uses base milliseconds
`min(100 << min(k-1,8), 20000)`. Deterministic jitter is the first two big-endian
bytes of `SHA256("racer/link-backoff/v1\0" || len(node) || node || k:u32)`,
modulo `(base/4+1)`. Thus retry delay is bounded by 25 seconds. The failure
counter saturates at u32::MAX. `available` is only a routing hint.

Sends call `try_acquire` to reserve the single half-open probe, then report
success/failure with `observe`. A lost probe becomes eligible after one
second. Success removes the circuit. Capacity bounds stored circuits; a new
failure with a full table returns `Overloaded` instead of silently forgetting
an existing failure. On topology changes, call `retain_neighbors` to reclaim
obsolete circuits. Healthy links can serve concurrent pooled operations.

## Rails and HTTP fallback

Intersect `(rail_id, fabric)` pairs across every node on the authenticated
route. Any node with alignment disabled or no rails forces HTTP. Different
node-local NUMA IDs do not make a shared rail incompatible. Sort surviving
rails by u16 ID. Compute:

`r = SHA256("racer/rail/v1\0" || object_page || len(strong_etag) || strong_etag)`

Select the first big-endian u64 modulo the compatible rail count. Reversing
the same route selects the same rail. An empty intersection uses HTTP.
`select` considers authenticated publication inputs; `select_with_local` and
`local_compatible` additionally check actual local hardware. Hardware must
have one unique selected rail with the same fabric and, when specified, the
published NUMA ID. Every hop must confirm hardware before RDMA admission;
an unavailable or incompatible hop falls back to HTTP. Discovery can veto
RDMA, never rewrite membership or choose a different rail for one hop.

## Golden vectors

These vectors were independently calculated with Python SHA-256 and integer
arithmetic and are asserted in Rust tests. Cache ID is `cache-a`, key is byte
0x42 repeated 32 times, ETag is the four bytes `"v1"`. Node IDs are
`node-000000`, `node-000001`, `node-000002`, `node-000003`, with shares 1,3,6,4.
Compatible rails are [2,7]. Ranking values below are node suffixes.

| Page | Slot hash (hex) | Slot | Top three | Rail |
| --- | --- | --- | --- | --- |
| 0 | d8b632a58acf4dc92ccc3abe711290968975c95221982118f58393fbdead4781 | 887651 | 1,3,2 | 2 |
| 1 | 55303dce9847e82a06df4d42dfe8143810ddd6bee5cbb1e00df7c54a492aa9e8 | 348931 | 3,2,1 | 7 |
| 18446744073709551615 | a267040dcd7fc46c92defba1c1405f9395387059987c839e9b5e812e7861c723 | 665200 | 2,1,3 | 7 |

For slot 887651:

| Node suffix | HRW SHA-256 | Q32.32 cost |
| --- | --- | --- |
| 0 | e41095812e885f6f0ae7e3c1a93d8ec04999df01dbb5820765c7e972b2c07a9c | 715971622 |
| 1 | c4251196ce419070d2df41cf51ac2193cc266e9669aeeb4d2c624510a500a881 | 1650232626 |
| 2 | 9322018f0806e768456ec86946014c6d1c608416d368e094cee33c40e0171bf9 | 3431784333 |
| 3 | b379deaba20d903a288905a7939651ed51dc5014bf7c50989482855e36ab2cd0 | 2200536977 |

Rail hashes for the three pages above:

```text
500304ace38caf7cc36f8f97ae12bdfc89b92f7eb64c8bb060d1daf49f76314c
d62f99be192c5c55bf7fcdd6f6e82510e4dfb15edc633c10e7e3c9920e6b1a15
1f5ac57db992579deb3ef38af57963721ecf01c5957e2e23481e471faa6f7c54
```

Integer edge vectors: sample 0 costs 274877906944, sample 2^63-1 costs
4294967296, and sample 2^64-1 costs 1. Equal ratios tie on node ID.

Version 2 route vectors (sorted positions, healthy graph, four-link budget):

| Members | Source | Destination | Route |
| --- | --- | --- | --- |
| 1000 | 0 | 999 | 0,55,999 |
| 100000 | 0 | 80003 | 0,13,246,4444,80003 |
| 401 | 37 | 309 | 37,358,309 |

## Scoped verification

Run `cargo test --lib topology:: --all-features` from `cmd/racer-dataplane`.
Tests cover small graph inverse edges exhaustively, every 100,000-node edge
for symmetry/degree, four-link reachability to every destination from five
sources, default-config routing at every distance from zero through four,
independent shortest-path oracle comparisons, local failure isolation, monotonic
budgets, bounded cooperative work/cache ownership, cancellation, deadlines,
admission saturation, distribution, churn,
membership order invariance, and end-to-end rail/hardware fallback.
