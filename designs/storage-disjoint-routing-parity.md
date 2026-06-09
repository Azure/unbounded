# Storage disjoint routing: parity contract

## Purpose

`unbounded-storage` routes P2P cache requests over a Chord-style 64-bit
ring. By default every node derives its own finger table from the full
sorted roster of cluster nodes (`FingerTable::build`), which requires
each node to know every other node.

"Disjoint discovery" lets a node be configured with **only its direct
routing neighbors** via a `[p2p.routing_plan]` block instead of the full
roster. The node feeds those neighbors straight into
`FingerTable::from_explicit` and skips the global build entirely.

The key property is **path parity**: a node configured with a
`routing_plan` must route every key along the exact same path it would
have taken under the full-roster build. This holds because the runtime
routing math (`FingerTable::next_hop` / `closest_preceding`) only ever
consults a node's own `fingers`, `successor`, and `predecessor`; it never
needs global knowledge. Only the *selection* of those neighbors needs a
global view.

This document specifies the selection algorithm exactly, so a planner
with a global view (e.g. a future controller) can compute each node's
`routing_plan` offline and reproduce byte-identical finger tables, and
therefore identical routes, without ever shipping a node the full
roster.

There is intentionally **no cross-language golden-vector fixture**. This
document is the contract. The in-crate test
`p2p::fingers::tests::from_explicit_reproduces_built_routing` and the DST
guard `p2p::tests::disjoint_routing_matches_global` prove that
`from_explicit` fed the selection below reproduces `build`'s routes; any
re-implementation must match this same algorithm.

## Inputs

For a target node `L` being planned, the planner needs the global roster:
the set of all nodes, each with:

- `id`: the `u64` node id (the `[[peers]].id` / `local_node_id`).
- `labels`: the ordered topology label vector (coarsest to finest, e.g.
  `[region, zone, row, rack]`). Empty if unset.

and the cluster-wide fanout `k` = `fingers_per_node` (default 100,
clamped to at least 1).

## Ring math

All positions are on a 64-bit ring (`0 ..= 2^64 - 1`), wrapping.

### splitmix64

The standard splitmix64 finalizer, used as the deterministic mixer:

```
splitmix64(x):
    x = (x XOR (x >> 30)) * 0xbf58476d1ce4e5b9   # wrapping mul, mod 2^64
    x = (x XOR (x >> 27)) * 0x94d049bb133111eb   # wrapping mul, mod 2^64
    return x XOR (x >> 31)
```

All arithmetic is unsigned 64-bit with wraparound.

### Ring position of a node

```
node_to_ring(id) = splitmix64(id)
```

So every node's ring position is `splitmix64(node_id)`. (Stripe keys map
to the ring via the leading 8 bytes little-endian of their SHA-256
digest, but the planner does not need that for neighbor selection.)

### Forward ring distance

```
ring_distance(from, to) = (to - from) mod 2^64
```

This is 0 only when `from == to`.

### Topology distance

Right-pad both label vectors to the longer length with the wildcard
`"*"`, then scan left to right counting the longest prefix of slots that
are equal **and** non-wildcard. The distance is `len - prefix_len`:

```
topology_distance(a, b):
    n = max(len(a), len(b))
    if n == 0: return 0
    prefix = 0
    for i in 0..n:
        la = a[i] if i < len(a) else "*"
        lb = b[i] if i < len(b) else "*"
        if la == "*" or lb == "*" or la != lb: break
        prefix += 1
    return n - prefix
```

On a `[region, zone, row, rack]` vector: identical = 0, same row
different rack = 1, only region matches = 3, fully disjoint = 4. A
wildcard never matches anything (including another wildcard), so missing
slots count as "farther".

### Rendezvous hash

`GOLDEN_RATIO_64 = 0x9e3779b97f4a7c15`.

```
rendezvous_hash(local_ring, candidate_ring, arc) =
    splitmix64(local_ring XOR candidate_ring XOR (arc * GOLDEN_RATIO_64))
```

`arc * GOLDEN_RATIO_64` is a wrapping multiply.

## Selection algorithm

Compute `L`'s neighbors exactly as `FingerTable::build` does.

Let `local_ring = node_to_ring(L.id)`, `k = max(fingers_per_node, 1)`,
and `arc_span = floor((2^64 - 1) / k)` (integer division of `u64::MAX`
by `k`).

### Arc index

Each candidate falls into one of `k` arcs by forward distance from `L`,
with the **last arc absorbing the remainder** so the partition is total
even when `2^64 - 1` is not divisible by `k`:

```
arc_index(local_ring, cand_ring, arc_span, k):
    d = ring_distance(local_ring, cand_ring)
    raw = d / arc_span                 # integer division
    return (k - 1) if raw >= k else raw
```

### Fingers

Initialize one slot per arc as empty. For every candidate node `c` where
`c.id != L.id`:

1. `a = arc_index(local_ring, node_to_ring(c.id), arc_span, k)`.
2. If arc `a` is empty, place `c` there. Otherwise replace the incumbent
   `inc` with `c` iff `better(c, inc, a)` (below).

```
better(c, inc, arc):                   # is challenger c better than incumbent inc?
    ct = topology_distance(L.labels, c.labels)
    it = topology_distance(L.labels, inc.labels)
    if ct != it: return ct < it
    cr = rendezvous_hash(local_ring, node_to_ring(c.id), arc)
    ir = rendezvous_hash(local_ring, node_to_ring(inc.id), arc)
    if cr != ir: return cr < ir
    return node_to_ring(c.id) < node_to_ring(inc.id)   # raw u64 ring position
```

That is: prefer the topologically nearer candidate; break ties by lower
rendezvous hash; break remaining ties by lower raw ring position. The
selection is therefore total and deterministic, independent of candidate
iteration order.

The resulting `fingers` are the set of distinct arc winners, **excluding
`L` itself**. (Empty arcs contribute nothing; `from_explicit` does not
materialize self-clones for empty arcs the way `build` does internally,
and the lookup path ignores them either way.)

### Successor and predecessor

Over the same candidates (`c.id != L.id`):

- `successor` = the candidate minimizing `ring_distance(local_ring,
  node_to_ring(c.id))` among nonzero distances (the nearest node
  *forward* on the ring).
- `predecessor` = the candidate minimizing `ring_distance(node_to_ring(
  c.id), local_ring)` among nonzero distances (the nearest node
  *backward* on the ring).

Both are absent only for a single-node cluster. If two candidates tie on
distance (only possible with colliding ring positions, which
`splitmix64` makes vanishingly unlikely for distinct ids), the first
encountered wins; planners should treat a tie as a misconfiguration.

## Output mapping

The planner emits, for node `L`:

- `[p2p.routing_plan].fingers` = the list of arc-winner node ids
  (deduplicated, `L` excluded). Order does not matter.
- `[p2p.routing_plan].successor` = the successor node id (omit for a
  single-node cluster).
- `[p2p.routing_plan].predecessor` = the predecessor node id (omit for a
  single-node cluster).

`L`'s `[[peers]]` list must contain exactly one entry per id referenced
by the routing plan (fingers, successor, predecessor), carrying that
peer's transport/address so `L` opens connections only to its routing
neighbors. `unbounded-storage` validates this at config load:

- every routing-plan id must be a known `[[peers]].id`
  (`RoutingPlanUnknownPeer`),
- no routing-plan id may equal `local_node_id`
  (`RoutingPlanSelfReference`),
- finger ids must be unique (`RoutingPlanDuplicateFinger`).

Labels for finger peers are looked up from their `[[peers]]` entry (or
empty if unset); labels do not affect runtime routing, only the
build-time selection above, which the planner has already resolved.

## Why the paths match

`FingerTable::next_hop(target)` decides a hop using only `local`,
`fingers`, `successor`, and `predecessor`:

- `target == local_ring`, or `target` in `(predecessor, local]`, or no
  predecessor (single node): `L` owns the key, terminate.
- `target` in `(local, successor]`: hop to `successor`.
- otherwise: hop to `closest_preceding(target)` (the finger with the
  largest forward distance `<= ring_distance(local, target)`), falling
  back to `successor`.

None of these consult the global roster. Because `from_explicit` stores
the same `fingers`/`successor`/`predecessor` that `build` would have
derived, `next_hop` returns the same neighbor at every node, so the
recursive walk visits the same sequence of nodes and terminates at the
same owner. Fingers only shorten the path; correctness (termination at
the right owner) rests on the successor pointers forming a complete ring,
which the planner must preserve for every node.
