# Racer topology algorithm v3

Version 3 changes only equal-cost route selection. All membership, graph (radix
18, degree at most 36), top-three weighted HRW placement, authentication, budgets,
rail domains, and wire encodings in `ALGORITHM_V2.md` remain unchanged. Sorted
UIDs still define graph positions. Normal routes still use at most four links
(three intermediate nodes), never additional links for balancing. Failure
admission retains the existing eight-link ceiling, not a new routing allowance.

## Selection contract

Exclude visited nodes and failed source-incident edges exactly as v2. Alternate
complete BFS layers, source first, with ascending neighbors and FIFO discovery.
Carry a u64 first-hop bitset at each source-wave visit. On equal-depth discovery,
OR the predecessor's bits; on the first layer use the outgoing neighbor's bit.
The preceding layer is complete before children expand, so masks are complete.
Destination-wave visits retain their first-discovery parent.

Complete the entire first intersecting layer, rather than stopping at its first
intersection. All its intersections have the same minimum distance. Retain the
first meeting vertex for each source first-hop bit. This gives **every** admissible
shortest next hop, without enumerating every full path. Sort alternatives by
next-hop position. Choose index:

```text
h = SHA256("racer/next-hop/v3\0" || request_id[16] || attempt_id[16]
           || len(current_node):u32 || current_node
           || len(destination):u32 || destination)
index = first_u64_be(h) % number_of_alternatives
```

Lengths count exact bytes and are big endian. Modulo bias is bounded by the
64-bit sample space. One alternative needs no hash. Identity comes from the
existing signed route, not a new random value per planning invocation. A new
attempt can choose a different equal-cost hop. The same inputs, snapshot and
local failed-edge set always select the same next hop, independent of cache,
yield quantum or cache eviction. No caller-controlled load estimate is trusted.

For the selected bit, reconstruct the source prefix backward using the first
ascending neighbor with depth one less and that bit set (depth one connects to
source). Reconstruct the destination suffix with first-discovery parents.
This witness route supports existing rail checks. It does not prescribe future
relay choices: each relay replans using its own signed state and local health,
as before, and each hop still checks its own rail/hardware compatibility.

Requester signing and its subsequent page rail-plan recomputation use this same
selector. If health changes the chosen next hop after signing, exchange fails
`Unavailable`; it never sends that envelope to an unsigned alternative.

## Work and memory bounds

No all-pairs table, destination-wide BFS, random search retry, or per-request
cache key is introduced. Search retains sparse maps, at most two visits per
member, with an extra u64 bitset and depth per visit. Healthy four-link routes
expand at most 74 vertices and retain at most 2594 visits, independent of N.
Each expansion examines at most 36 edges; each async poll expands at most 32
vertices. Deadline checks, cancellation, health revalidation, and cold-search
admission `clamp(cache_capacity, 1, 8)` are shared with v2. Worst-case failed
eight-link searches remain O(N log N), at most 2N expansions, N <= 100000.

After search, witness reconstruction is bounded by 36 alternatives, eight links
each and at most 36 neighbor candidates per backward prefix step (at most 10368
candidate checks, no membership scan). This fixed work occurs in the finishing
poll. Cache entries contain at most 36 vectors of at most nine usize positions
(2592 position bytes plus vector/map/key/allocation overhead on 64-bit hosts).
FIFO capacity is still `RACER_CACHED_PATHS`, partitioned across workers. Entries
are weak snapshot leases keyed by snapshot/source/destination/link budget/
visited set/failed source edges. Cache request-independent alternatives, never
the first request's selected route. A hit hashes once and copies at most nine
Node IDs. Capacity zero disables retention but still permits one cold search.

## Interoperability vectors

N=1500, IDs `node-000000` through `node-001499`, source 0, destination 1499,
request ID 16 bytes of 0x01, attempt encoded as big-endian u128, healthy four-link
budget. Equal next hops are:
`83,166,333,416,583,666,833,916,1083,1166,1333,1416`.

| Attempt | SHA-256 | Selected next hop |
| --- | --- | --- |
| 0 | 5e2225545627c35ee4f8decd305f340496995de4e83df45e007d6065bf1b9a36 | 833 |
| 1 | ab97ecb86e54ce1cfd4f614347c391415bee636659ffdad7c4d38d3cb4171146 | 83 |
| 2 | 66d3f8ad76a81b452b5bd79a5acc745cfc3436e14d134e8cabb53d15ff4a4a7c | 1166 |
| 127 | 1f56a41e02051b60d0802e50c0f65091c5ab6aa96d6219a24e049ef8e27416f9 | 83 |

These were independently calculated with Python hashlib and destination BFS.
Tests also compare every returned first hop against an independent graph/oracle,
including failed links, visited nodes, and insufficient remaining links.

## Deployment, compatibility and A/B

`ALGORITHM_VERSION=3` means latest supported specification, not wire negotiation.
`RACER_ROUTING_ALGORITHM` defaults to `2`; exactly `3` enables this selector at
worker construction. Upgrade all participating **racer-dataplane images** with
the setting absent/2 first. No controller, operator, Gantry, or client image
change is required; membership counters are not algorithm versions. No cluster
changes or image pushes are part of this implementation.

After image rollout, explicitly set `RACER_ROUTING_ALGORITHM=3` through the
preserved dataplane ConfigMap or deployment environment and coordinate restart
of participating dataplanes. Wait for rollout and old transfers to drain before
an A/B window. Each sender signs its locally selected receiver; intermediate
mixed-version operation does not change signature verification or graph edges,
but is not an all-v3 fairness measurement and may have different witness rail
plans. There is no negotiated v3 capability. Old images ignore this unknown
setting and still route v2, so verify image coverage, not just configuration.
Rollback the setting to `2` and restart before rolling back images.

The deterministic production-path model uses 1500 SHA-derived UID-like members,
144000 equal-sized transfers (96 per source and owner), and replans at every hop.
Both TX and RX peak/mean were **4.7867 v2 vs 1.2914 v3**, with identical mean
path length **2.556993**. The regression requires v3 <1.4 and v2 >3.5; all routes
must match independent BFS length. This is modeled NIC load, not a measured
cluster throughput claim or a guarantee for adversarial/skewed demand. Observe
max-node TX/RX, throughput, CPU, admission failures, latency and hop counts in
the controlled A/B; do not change shares or placement to mask routing skew.

## Validation evidence (2026-09-28)

Base `a47e96e4`, dedicated worktree `tmp/racer-equal-cost-routing`. All commands
used external TERM timeouts with a ten-second kill grace and a 300-second cap.

- Full all-feature Rust library suite: 816 passed, 10 ignored (hardware/opt-in
  benchmarks). Subsequently added signed wire/cache-eviction test: 1 passed.
- Scoped topology suite before the final health regression: 40 passed; the final
  health regression is included in the full library result above.
- Scoped peer suite: 36 passed, 2 ignored; configuration suite: 26 passed.
- `cargo check --all-targets --no-default-features`: passed.
- Changed Rust sources pass `rustfmt --check`; `git diff --check` passes.
- Strict Clippy is blocked by 96 findings in existing code, including
  `app.rs:1192` (collapsible if) and `config.rs:628` (collapsible if), not the
  new selector. No unrelated lint cleanup is included.
- Scoped `make fmt` was attempted as required, but the installed Go lint
  importer rejects Go 1.27 export-data version 4. The existing project-local
  Go 1.27-built linter has the same importer limitation. No Go files changed.

Implementation anchors (relative to `cmd/racer-dataplane/src`):
`topology/equal_cost.rs:75` completes bounded layers; `:143` reconstructs
alternatives; `topology/paths.rs:305` specifies the hash selector; `:503` checks
all next hops against the oracle; `:739` models balanced NIC demand;
`peer/tests.rs:804` checks signed wire recomputation and cache eviction;
`app.rs:661` applies the selected algorithm in production;
`config.rs:168` enforces the default/opt-in version contract.
