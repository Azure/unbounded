# P2P Storage Protocol

Adaptive replication over a structured ring for unbounded-storage-p2p.

## TL;DR

**Membership**

- Nodes hash onto a ring. Each node links to peers at offsets `2^0, 2^1,
  ...` ahead of itself, so any address is reachable in `O(log n)` hops.
  Membership comes from Kubernetes, so every node computes the same ring.
  No gossip, no coordinator.
- Blobs split into fixed-width chunks. Width is computed from blob length
  alone, so clients find chunk boundaries without a manifest.
- Each chunk has one **owner**: the next node clockwise from
  `hash(blob_id, chunk_index)`. Only the owner cold-fetches from the
  backing store.

**Read path**

- Lookup descends the neighbor table in `O(log n)` hops.
- Bytes flow back along the same path. Each transit node sees every
  chunk crossing it and may cache it.

**Adaptive replication**

- Each transit node tracks observed chunks in a SIEVE queue.
- A chunk that stays hot for `τ_promote` sweeps gets cached, if the node
  is arc-eligible (see below).
- Arc-eligibility uses the owner's `O(log n)` predecessor bands at
  doubling distances. Replicas cluster near hot readers but are also
  spread at multiple scales around the ring. Demand decides which bands
  fill in; the geometry handles spacing without coordination.
- After `τ_evict` cold sweeps the node evicts and eventually forgets the
  chunk.

**Emergent properties**

- Hot chunks gain replicas near their readers. Lookups shrink from
  `O(log n)` toward `O(1)` as replica levels grow.
- Synchronised reads (e.g. 1000 nodes pulling the same image) form a
  multicast tree with average fan-out 2 over `log2 n` levels.
- Membership changes do not trigger migration. Existing replicas keep
  serving until SIEVE drains them; the new owner cold-fetches on first
  miss.

## Ring and Neighbors

Nodes hash onto a ring of size `2^m`. Each node `q` has `m` neighbor slots
at offsets `2^0..2^(m-1)`. With `n` nodes, only the top `~log2 n` slots
resolve to distinct peers, so out-degree is `O(log n)`. Membership comes
from Kubernetes, so every node computes the same ring.

**Proximity Neighbor Selection (PNS):** for slot `i`, `q` picks any peer
in the arc `[q + 2^i, q + 2^(i+1))`, preferring topologically near peers
(rack, PCIe complex, interconnect). The arc keeps the distance-halving
property, so `O(log n)` hops still hold.

## Chunk Placement

Blobs are striped into uniform chunks. Width depends only on blob length:

    width(L) = clamp(round_pow2(L / TARGET_CHUNKS), MIN_WIDTH, MAX_WIDTH)

Clients know `L`, so they compute width without coordination or a
manifest. All chunks in a blob are the same size (last may be partial).
Across blobs, width varies between `MIN_WIDTH` and `MAX_WIDTH`. Chunk id
is `hash(blob_id, chunk_index)`. The **owner** is the first node
clockwise from that hash; only the owner may cold-fetch from the backend.

`width` must depend only on `L`. Any dependence on cluster size, time, or
mutable config breaks the no-coordination property. Power-of-two
bucketing absorbs minor length-reporting jitter.

## Transport

Routing is recursive at the byte level. Bytes flow along the lookup
path, so every transit node sees the chunks crossing it. Cold-read
latency is `O(log n)` hops; popular chunks shed hops as replicas grow.

The lookup descends the neighbor path with hops that halve the remaining
ring distance, so depth is `O(log n)`. Bytes retrace the path back; each
transit node observes chunk `c` exactly once and updates its SIEVE.

## Eligibility

The `i`-th **predecessor-neighbor arc** of owner `p` is
`(p - 2^(i+1), p - 2^i]`. Node `q` is eligible at level `i` iff `q` lies
in that arc, equivalently iff `p` lies in `q`'s forward arc
`[q + 2^i, q + 2^(i+1))`. This depends only on the two ids.

Eligibility is a superset of "p is q's actual neighbor entry"; the two
match only when an arc holds at most one node (`2^i ≲ 2^m / n`). The
slack is harmless: SIEVE gating (below) means arc-eligible nodes off
every lookup path never accumulate hits, so they never promote.

**Shortcut property:** any lookup terminating at `p` crosses
arc-eligible nodes in order on its way. The first one holding a replica
answers, short-circuiting the lookup.

## SIEVE

Each node maintains a SIEVE queue over **all transit chunks**, not just
owned or cached ones. The queue is byte-weighted: a chunk of width `w`
contributes `w` bytes, the hand advances at a configured bytes/sec rate,
and a sweep is one full pass over `bytes(queue)`. Each chunk is examined
once per sweep regardless of width, so visit cadence is uniform across
chunk sizes. Two derived counters drive transitions:

- `s_hit(c)`: consecutive sweeps with the visited bit set at the hand.
- `s_miss(c)`: consecutive sweeps with it clear. Resets on re-reference.

Node `q` promotes chunk `c` (owner `p`) when:
1. `q` is eligible,
2. `s_hit(c) ≥ τ_promote`, and
3. `q` wins the fault-domain tiebreak.

Promotion is local: `q` tees the next verified pass-through and serves
`c`. Demotion is local too: delete when `s_miss(c) ≥ τ_evict`. Both
thresholds are `> 1`, so brief bursts or troughs cannot drive a
transition.

**Fault-domain tiebreak.** Among eligible nodes past `τ_promote`, group
by fault domain (chassis, NIC, PCIe complex). Within each domain, the
node minimising `hash(node_id, chunk_id)` is the sole promoter. Gating
on `s_hit` before the tiebreak matters under PNS: a hash-only minimum
could pick an arc-eligible node that never sees transit, leaving the
domain uncovered. Keying on `chunk_id` spreads winners across nodes
instead of concentrating them on one low-id node per domain.

Thresholds are sweep counts. A sweep takes `bytes(queue) / sweep_rate`,
so `τ` reads directly as "hot/cold across this many full working-set
traversals" without depending on chunk-width distribution.

Per-chunk lifecycle on a single node:

- **promote:** `s_hit ≥ τ_promote` ∧ eligible ∧ wins fault-domain
  tiebreak.
- **demote:** `s_miss ≥ τ_evict` while serving; chunk stays in the
  SIEVE queue but is no longer served.
- **forget:** `s_miss ≥ τ_evict` while merely observed; chunk drops out
  of the queue.

The visited-bit churn (set on re-reference, cleared by the hand) is
queue bookkeeping that drives `s_hit` and `s_miss`, not a state
transition. Most chunks live and die in `Observed`; only sustained hits
on an eligible node cross into `Replica`.

## Recursive Structure

Once `q` serves `c`, lookups that previously terminated at `p` now
terminate at `q`. `q`'s own predecessor-neighbors then see transit for
`c` and may promote in turn. Levels emerge with no explicit tracking.
With `r` levels, expected lookup length is `log2 n - r`, reaching `O(1)`
at saturation.

Edges are undirected: the tree records the replica hierarchy, not data
flow. A reader's lookup terminates at the first replica it crosses, so
attaching at level `r` saves `r` hops. The same picture read in reverse
describes synchronised reads (e.g. an image pull at job start): bytes
propagate from `p` along the tree as a multicast, with average fan-out
2 across `log2 n` levels.

## Physical Clustering and Multicast

PNS biases each hop toward the nearest peer in its arc. Hot chunks
acquire a dense replica cluster near their dominant readers and a
thinner reach into colder regions, with no explicit topology placement.

For synchronised large-blob reads (e.g. 1000 nodes pulling the same
image), the same mechanism builds a self-organising multicast tree.
After `~log n` waves the chunk reaches every node. Fan-out analysis is
per-blob and relies on within-blob chunk uniformity, which the width
function preserves. A balanced tree of `n` leaves over `log2 n` levels
has *average* fan-out `n^(1/log2 n) = 2`, bounding per-replica bandwidth
as the cluster scales.

This is an average, not a per-node bound. A replica has up to `log2 n`
predecessor-neighbor arcs, and under skewed demand may fan out to more
children, with bandwidth tracking local fan-out.

## Failure and Liveness

- **Owner failure.** Replicas remain on disk, but only those on the
  lookup path to the new successor `p'` intercept new requests.
  High-index predecessors of `p` are also predecessors of `p'`, so most
  of the tree keeps serving. Low-index replicas may fall off-path until
  re-promoted. Misses fall through to `p'`, which cold-fetches.
- **Membership change.** Chunks are not migrated. Existing replicas
  serve until SIEVE drains them; the new owner cold-fetches only on a
  miss. Hot chunks regrow the tree under the new geometry.
- **In-flight transactions.** A hop failure surfaces as a transport
  error. The client retries; partial bytes are discarded by the
  reader's content-hash check.

## Alternatives Considered

The shape of this design is driven by three invariants: placement is a
pure function of `(blob_id, chunk_index)` and current membership, bytes
flow along the lookup path so caching is a routing side-effect, and
replication geometry emerges from demand. Each alternative below
sacrifices at least one of those.

### Centralized dedicated cache tier

A separate fleet of cache nodes sits between consumers and the backing
store. Examples: registry pull-through mirrors (Harbor, distribution),
Dragonfly's supernode/seed-peer tier used without peer-to-peer fan-out,
classical CDN edge nodes.

- **Provisioning is decoupled from demand.** The cache tier is sized
  ahead of time. Idle outside bursts; saturated during synchronized
  pulls. Our design uses the same nodes that *are* the demand as the
  fabric, so capacity scales with the workload by construction.
- **Locality is coarse.** Cache nodes are not on the consumer's data
  path. Best case is one cache per rack, hand-placed. PNS gives us
  rack/PCIe-aware replicas as a side-effect of demand, no placement
  policy to maintain.
- **Hot-spot under fan-out.** 1000 nodes pulling the same image
  saturate the cache tier's NICs in proportion to fleet size. The
  multicast tree in section "Recursive Structure" bounds *average*
  per-replica bandwidth as the cluster grows; a centralized tier does
  not.
- **Extra control plane.** Another component to deploy, monitor,
  upgrade, and recover. The ring derives from the existing Kubernetes
  membership Unbounded already trusts.
- **Concentrated fault domain.** Cache tier failure degrades every
  consumer simultaneously. Our scheme degrades gradually as replicas
  drain.

### Centralized metadata, distributed blocks

A metadata service tracks which nodes hold which blocks; data lives
distributed. Examples: HDFS NameNode, Ceph MDS, BitTorrent tracker,
IPFS provider records on a DHT, S3-style indirection.

- **Manifest-free placement is lost.** The whole point of `width(L)`
  + `hash(blob_id, idx)` is that any client computes chunk boundaries
  and owners locally. A metadata layer reintroduces a per-read lookup
  before any byte moves.
- **Critical-path dependency.** Metadata service must be HA, must
  scale to read RPS, and must be reachable before any data flows. The
  ring + Kubernetes membership has none of those failure modes.
- **Membership churn rewrites metadata.** Adding or removing a node
  triggers placement-record updates proportional to held blocks. Our
  scheme migrates nothing on membership change; existing replicas keep
  serving and the new owner cold-fetches on first miss.
- **Coordination cost on writes.** Every chunk landing somewhere has
  to register that fact. Our promotion is purely local.
- **Read amplification at small scale.** For a small blob the metadata
  round-trip can dominate the actual transfer.

### Typical BitTorrent

Tracker or Mainline-DHT peer discovery, rarest-first piece selection,
choke/unchoke tit-for-tat, unstructured swarm. Designed for adversarial
public swarms with churning, selfish peers.

- **Wrong replication objective.** Rarest-first deliberately spreads
  rare pieces to keep the swarm healthy. We want the *opposite*: hot
  pieces gain replicas near hot readers. SIEVE + arc-eligibility
  encodes that directly.
- **No structured routing.** Peer discovery is announce/gossip or DHT
  lookup. Lookup cost is not `O(log n)` over the *cluster*; it is a
  function of swarm size and DHT health. We get `O(log n)` cold and
  `O(1)` hot from ring geometry.
- **Transit nodes see nothing.** A BT peer only sees the pieces it
  explicitly requests. Recursive byte-level routing means every transit
  node already observes the chunks crossing it; SIEVE just decides
  which ones to retain. That is the cheap caching property; BT cannot
  reproduce it.
- **No locality awareness.** Peer selection is random or
  upload-rate-driven. PNS biases hops to the nearest peer in each arc
  for free.
- **Coordination overhead.** Bitfield exchange, HAVE messages, choke
  algorithms, optimistic unchoke. All necessary in an adversarial
  setting, all wasted overhead in a cooperative cluster.
- **Tracker or DHT is itself a centralization point** with the same
  problems as "centralized metadata" above, just less honestly named.

### Adjacent real-world systems

For grounding, points on the design space currently in production:

- **Spegel.** libp2p + mDNS/DHT for in-cluster registry mirroring.
  Closest to "typical BitTorrent" with cluster-local discovery; loses
  structured routing and demand-shaped replication.
- **Dragonfly.** Supernode-coordinated P2P. Hybrid of "centralized
  metadata" (supernode tracks pieces) and swarm transfer between
  agents. Supernode is the bottleneck and SPOF the design above
  removes.
- **Uber Kraken.** Origin cluster + tracker + agents, BitTorrent-style
  transfer between agents. Same tracker dependency; locality is
  rack-aware but not free, requires explicit configuration.

None of these provide the "placement is a pure function, caching is a
routing side-effect, replication shape emerges from demand" combination
that motivates this design.

