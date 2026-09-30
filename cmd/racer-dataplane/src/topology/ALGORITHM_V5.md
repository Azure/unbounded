# Racer routing algorithm v5: radix32, at most 64 neighbors

V5 is the default (`RACER_ROUTING_ALGORITHM=5`). Explicit `2`, `3`, and `4`
preserve their radix18 graphs and exact route/selection contracts. V5 changes
only graph degree and its coupled bounded resources, retaining V4's complete
eligible shortest-next-hop set, canonical witnesses, authenticated-share weights,
integer rejection sampling, and `racer/next-hop/v4\0` selection hash domain.
Placement, membership ordering, rails, signatures and socket budget defaults do
not change. Normal routes still have four links; failure routes have eight.

## Graph and bounds

For N UID-sorted members, outgoing edges are `(32*i+j) mod N`, j=0..31.
The undirected graph is their union with incoming edges; remove self and duplicate
edges, then sort by member position. Inverse enumeration is `(i+j*N)/32`,
j=0..31, using integer division. Each vertex has at most 64 neighbors.
This is a different graph, not a superset of radix18: individual pairs can get
longer even though representative balanced demand has lower aggregate hop count.

`MAX_DEGREE` centralizes the largest supported degree. First-hop masks use u64
bits 0..63, including the 64th neighbor; never compute `1 << 64`. Weighted totals
are at most `64 * (2^32-1)`, safely within u64. Search expands at most 32 vertices
per async poll and examines at most 64 edges per expansion. A healthy four-link
search expands at most `2*(1+64)=130` vertices and stores at most
`2*(1+64+64*63)=8194` visited entries, independent of membership size. Each search
retains at most 64 canonical paths; each failure-budget path has at most nine
positions. Reconstruction examines at most `64*8*64=32768` neighbor candidates.
The existing deadline, cancellation, search-admission and cache-entry limits stay
in effect; increased degree increases per-search CPU and memory bounds.

The production link-health table accommodates 64 neighbors. RDMA session admission
allows at most `per_neighbor * MAX_DEGREE` live entries while preserving the
per-neighbor cap. This does not raise native QP pools, socket admission, or any
configured connection budgets: independent resource limits may bind first.

## Independent interoperability vectors

N=1500, names `node-000000` through `node-001499`, source 0, destination 1499,
request ID sixteen 0x01 bytes, attempt as big-endian u128. Shares are 1 at positions
divisible by 3 and 4 elsewhere. Independent outgoing-edge BFS finds these eligible
next hops (all two-link witnesses):

```text
46 93 140 187 234 281 328 421 468 515 562 609 656 703
796 843 890 937 984 1031 1078 1171 1218 1265 1312 1359 1406 1453
```

Total weight is 88. Python hashlib with V4's exact encoding gives:

| Attempt | First u64 (hex), counter 0 | Ticket | Next hop |
| --- | --- | ---: | ---: |
| 0 | a9f87d811a3b45c5 | 77 | 1312 |
| 1 | 1f5a7acb5fa15aed | 53 | 937 |
| 2 | 4ea070a9bdbae502 | 42 | 703 |
| 127 | 7ebb0b4de1a2ea2f | 55 | 937 |

Legacy vectors remain unchanged. Tests cover independent shortest-path and
eligible-hop oracles, failure/visited exclusions, all-neighbor failure, the
64th mask bit in direct and propagated witnesses, and bounded default searches.
The deterministic 1500-pair hop comparison uses `(source+731) mod 1500`, one flow
per source and destination: 3790 legacy links versus 2936 V5 links (22.53% fewer),
with 856 pairs improved. This is a topology property, not a throughput claim.

## Coordinated rollout required

Algorithm selection is local startup configuration, not negotiated or encoded in
membership. Peer endpoint validation and path computation both use that selection.
Do not assume mixed radix18/radix32 dataplanes interoperate: edges valid for one
can be rejected by another. Wire framing version 4 is unrelated to routing V5.

Before upgrading a live legacy cluster, explicitly pin every participating
dataplane to its existing `2`, `3`, or `4` value. Absence now selects V5, so a
rolling image upgrade with an absent setting is not a safe topology transition.
After all images support V5, quiesce new transfers, drain old transfers, coordinate
the setting/restart to 5 across all participants, verify image/config coverage,
then resume traffic. There is no negotiated mixed-topology rollout. Reverse the
same quiesce/drain/coordinated switch to an explicit legacy value before restoring
older images. Membership counters do not establish algorithm adoption.

No deployment or operational rollout is performed by this code change. Shipped
configuration omits this setting and therefore follows the new code default.
