# Racer mesh v2: per-cache node pools

## Summary

A `ClusterCache` can name a set of nodes with `spec.nodeSelector`. Only those nodes store, serve,
and read that cache. Each set of nodes is a pool. Each pool gets its own peer mesh, and every read
reaches its owner in at most 2 hops. A node's peers are the union of the meshes of its pools.

Mesh v2 replaces the 32-ring overlay. It is a cold cutover: control wire `SCHEMA_VERSION` 2,
new hash domains, no mixed-version support.

The Racer dataplane still runs as one DaemonSet. Nodes rebuild their peers from each publication.

## What we optimize

In order:

1. **Hops per read.** At most 2 links from reader to owner in every pool. Relays do not cache, so
   every extra link copies the whole page again (`peer_bytes = page bytes x links`).
2. **RDMA links.** Each device allows at most 128 queue pairs (`RDMA_PEERS`). Pools on the same
   node share them. Selector pools come first.
3. **Peers per node.** Fewer peers means fewer TCP connections and less fan-in per node.
4. **Hot keys.** An owner must not stream one hot page to every reader. Readers coalesce on a
   bounded set of helpers.
5. **Churn.** A join or leave changes O(sqrt(k)) edges in a pool of k nodes. Every node, and every
   primary after failover, computes the same mesh from the same publication.

Hop count is an internal design target. It is not written to the `ClusterCache` status. The
API has no status.

## Today (v1)

- One membership for the cluster. All caches place pages over all members with weighted HRW.
- 32 hash rings. Each member links to its ring neighbors (at most 64). Routes are found with BFS
  and are up to 4 links long, or 8 after a failure. Expected hops grow with cluster size.
- Hot pages: an owner sheds readers once it streams 2 bodies of one page. Shed readers go to
  about sqrt(N) helpers picked by hash over all members.

## API

`ClusterCacheSpec.nodeSelector` is an optional `metav1.LabelSelector`.

- Unset or empty: the cache uses the default pool, which is every member.
- Set: the cache uses the members whose Node labels match.
- It can change at any time. There is no migration. Pages move to the new pool's owners, and
  readers fill them again from origin.
- There is no status field.

## Pools

- The primary evaluates each cache's selector against Node labels of current members.
- Caches with the same canonical selector share a pool. The pool ID is `default` for the
  default pool, and `sel-` plus the first 32 hex digits of SHA-256 of the canonical selector
  JSON otherwise. The canonical form sorts `matchLabels` keys, sorts expressions by key, then
  operator, then values, and sorts each value list.
- A cache with an invalid selector is left out of the catalog. Other caches are not affected.
- Only pools that some cache uses are published.
- Pool rank orders pools for RDMA: `(selector ? 1 : 0) << 32 | max cache priority in the pool`.
  Higher wins.
- A pool can be empty. Its caches then have no owners, and reads fail with `Unavailable`.

## Mesh layout

The mesh of a pool depends only on the pool's member IDs and Sites. It does not depend on the
pool ID, so two pools with the same members have the same mesh and share every edge. Two pools
that overlap tend to share column edges, because a node's column hash does not depend on the pool.

### Hashes

All hashes use the member `DOMAIN` prefix and SHA-256, read as big-endian u64 from the first 8
bytes:

- `column(n)  = H("/mesh/v2/column\0",   id(n))`
- `key(n)     = H("/mesh/v2/delegate\0", id(n))`
- `point(n,v) = H("/mesh/v2/point\0",    id(n), v as u32)` for `v` in `0..16`

### Columns

Let `k` be the pool size.

1. If `k <= 64` (`FULL_MESH_MAX`), there is one column, and the pool is a full mesh. 1 hop.
2. Otherwise `C` is the largest power of two with `C * C <= k`, and the target column size is
   `T = ceil(k / C)`. `C` only changes when `k` crosses a power of four, so most joins and leaves
   do not move columns.
3. A Site with `k_s >= ceil(T / 2)` members is large. It gets `c_s = max(1, round(k_s / T))`
   columns of its own. Its members pick a column with jump consistent hashing:
   `jump(column(n), c_s)`.
4. Members of small Sites form one mixed group `M`.
   - If `|M| >= ceil(T / 2)`, `M` gets `max(1, round(|M| / T))` mixed columns, picked the same way.
   - Otherwise, if there is a large Site, members of `M` join the large Sites' columns:
     `jump(column(n), sum of c_s)` over the large Site columns in order.
   - Otherwise `M` is the whole pool and gets one column.
5. Columns are numbered: large Sites in byte order of the Site name, each Site's columns in jump
   order, then mixed columns. The empty Site name is a Site like any other.

Column sizes stay between about `T / 2` and `3T / 2` (small Site members can add up to `T / 2`
to a large Site column). Columns of a large Site have one Site, so column edges can use RDMA.

### Edges

- **Column edges.** Members of the same column are all linked to each other.
- **Delegate edges.** For each other column `c`, member `n` links to `delegate(n, c)`: the
  member of `c` that owns the first ring point at or after `key(n)` on column `c`'s ring
  (wrapping). Each member puts 16 points on its column's ring, which spreads delegate load to
  within about +/-25% of the mean.

A member has about `T - 1` column edges, `C - 1` outgoing delegate edges, and about `C - 1`
incoming delegate edges. With `T` about `sqrt(2k)` and `C` about `sqrt(k/2)`, that is about
`2.83 * sqrt(k)` peers. Examples (one Site):

| k | C | T | peers per node |
| --- | --- | --- | --- |
| 64 | 1 | 64 | 63 |
| 256 | 16 | 16 | about 45 |
| 1,024 | 32 | 32 | about 93 |
| 4,096 | 64 | 64 | about 189 |
| 20,000 | 128 | 157 | about 410 |
| 100,000 | 256 | 391 | about 900 |

A packed grid could reach about `2 * sqrt(k)` peers. It is not stable under churn, so we
accept about 1.4x more peers.

### Two hops, two paths

For reader `r` and owner `o` in the same pool:

- Same column: `r` and `o` are linked. 1 hop.
- Otherwise:
  - Primary: `r -> d -> o` with `d = delegate(r, col(o))`. `d` is in `o`'s column, so `d - o`
    is a column edge.
  - Alternate: `r -> d' -> o` with `d' = delegate(o, col(r))`. `d'` is in `r`'s column.
  - `d` and `d'` are in different columns, so the two paths share no middle node.
- After both fail: `r -> m -> delegate(m, col(o)) -> o` for any column mate `m` of `r`. There
  are `T - 1` such paths.

So every pool has diameter at most 2, and routes need no search. Routing works on the union
graph: any node that is linked to the destination can relay to it.

### Churn

- A join or leave changes the member's own edges, plus the delegate edges whose ring arc it
  takes or gives back: about `C` edges. In total O(sqrt(k)).
- When `C` or a Site's `c_s` changes, jump hashing moves the fewest members. This happens
  rarely, at power-of-four pool sizes and at Site rounding thresholds.

### Memory

The mesh is not stored as neighbor lists. At 100k nodes that would be about 700 MiB. Each pool
stores the column of each member, the sorted members of each column, each column's ring
(16 points per member), and all members sorted by `key`. Neighbors are computed when needed,
and `is_neighbor` is a few binary searches per shared pool.

## RDMA and TCP

Every edge is a TCP edge. Some edges are also RDMA edges.

- An edge can use RDMA if both ends have the same nonempty Site and share a rail.
- Each edge gets a rank. Both ends compute the same rank from the publication:
  1. the highest rank of a pool that has the edge,
  2. column edge before delegate edge (within that pool),
  3. a symmetric hash of the two IDs: `H("/mesh/v2/edge\0", min(id), max(id))`.
- Each node takes its 128 best ranked RDMA capable edges (`RDMA_PEERS`).
- An edge uses RDMA only if both ends took it. The responder refuses other link setups. The
  initiator never asks. The page then goes over TCP. This is counted in metrics as overflow.

Selector pools with up to about 2,000 nodes mostly fit within 128 RDMA peers, so both hops can use
RDMA. Bigger pools, and the default pool on a large cluster, use TCP for the overflow edges.

TCP limits cover the local peer count. Link health capacity, the peer admission key limit,
and the HTTP pool endpoint limit (plus 64 for origins and sockets) are fixed at 4,096. That
covers the degree of a 100,000 node pool (about 900) four times over. All three create
entries only when traffic needs them and drop idle ones, so a large limit costs nothing
until it is used. A fixed limit avoids resizing on every membership change.

Outbound connection slots stay at a quarter of `client_connections` per worker. They bound
open sockets, not peers: a worker connects only to peers it is talking to right now, which
in-flight limits keep well below the degree. When slots run out, the pool closes idle
connections and retries, so a node with more peers than slots reconnects more often but does
not fail. Holding one socket per peer per worker would need degree times workers file
descriptors.

## Hot keys

v1 relied on bounded degree to cap owner fan-in. v2 has larger degree, so the shed and helper
rule changes to use the mesh:

- An owner never sheds a requester in its own column (in the page's pool), or another owner. At
  most `T * 3/2` such nodes exist, so owner fan-in for one page is bounded by the column size.
- An owner sheds other readers the same way as before (2 hot bodies of one page).
- A shed reader `r` asks its helper: `delegate(r, col(o))`, the middle node of its primary path.
  The helper is a column mate of the owner, and coalesces every reader that delegates to it
  (about `C` readers) into one fetch.
- So a hot page becomes a 2 level tree: the owner feeds at most one column (about `sqrt(2k)`
  helpers), and each helper feeds about `sqrt(k/2)` readers. v1 used about `sqrt(N)` helpers
  hashed over all nodes, which could be any distance away. v2 helpers are always 1 hop from both
  the reader and the owner.
- Helpers do not chain. They are never owners, so they never reach origin.

## Clients and origins

- The dataplane binds a cache's client and origin sockets only on members of the cache's pool.
  When a node leaves a pool, it unbinds the sockets.
- The admission webhook looks up the `ClusterCache` named by `racer.unbounded-cloud.io/client-cache`.
  It rejects unknown caches. It adds required node affinity from the selector, so client pods
  schedule only on pool nodes.
- The operator adds the selector to the origin DaemonSet's node affinity.

## Control wire v2

- `SCHEMA_VERSION = 2`. Version 1 publications are rejected.
- A publication has `pools`: `{ id, rank, members, caches }`.
  - `members` is a sorted list of indexes into the publication's member list. It is left out for
    the default pool, which means every member.
  - `caches` lists the cache IDs that use the pool. Each cache is in exactly one pool.
- The membership version hash covers members and pools. Moving a cache to another pool, or
  changing a pool's members, publishes a new membership version.

## Dataplane

- `topology::Membership` holds pools. Its graph is the union of the pool meshes.
- Placement is per pool: one `topology::Placement` per pool per worker. The memory budget is
  split evenly. The page key is unchanged (cache ID, object key, page).
  A selector pool ranks over a placement-only membership of its members. A cache that no pool
  lists uses every member.
- Routing uses the mesh routes above. BFS is gone. A route takes no pool hint: it uses the highest
  ranked pool that has both ends, which is 2 hops like the page's own pool.
- A shed reader does not know which owner shed it. It asks the helper for the first of the 3
  ranked owners that gives one that is not an owner.
- `MAX_DEGREE` is gone. Capacities come from the local node's peer count.

## Simulator

New metrics: owner peak fan-in and egress per hot page, peers per node, hops by transport, and
edges changed per join or leave.

Scenarios: default pools of 512, 4k, and 20k nodes; a 256 node selector pool in a 20k cluster;
a hot page storm (v1 numbers are in the old reports); churn; and a primary failover that must
give the same mesh.

## Rollout

Cold cutover. Stop every Racer pod, roll out the new image, then start. Mixed versions reject
each other's publications and requests (schema and hash domains differ).
