# Racer routing algorithm v4: capacity-weighted shortest next hops

Select explicitly with `RACER_ROUTING_ALGORITHM=4`. Default is now 5; see
`ALGORITHM_V5.md` for its coordinated topology rollout requirement. V2 and
v3 choices, placement hashing, radix-18 graph, degree <=36, endpoints, rails,
authentication, deadlines, visited-node exclusion, and link budgets are unchanged.
Use the complete eligible shortest-next-hop set and canonical witnesses defined
by `ALGORITHM_V3.md`. Never lengthen a route to balance it. Normal routes retain
at most four links (three intermediates); failure admission retains its existing
eight-link limit. A local destination or sole eligible hop needs no weighted draw.

## Public selection specification

Alternatives are ordered by ascending next-hop UID position. Weight `w[i]` is
that next hop's positive u32 `Member.shares` from the request's immutable,
controller-authorized membership lease. No host identities, local NIC guesses,
unsignaled metrics, new key material, or zero-weight exclusion are introduced.
The same shares already affect placement: this opt-in couples the two uses.
Routing-only weights with unchanged owners would require a separate authenticated
membership attribute and control contract; there is no hidden local override.

Let `T = sum(w)` in u64, at most `36 * (2^32-1)`. For counter 0 through 63:

```text
h = SHA256("racer/next-hop/v4\0" || request_id[16] || attempt_id[16]
           || len(current_node):u32be || current_node
           || len(destination):u32be || destination || counter:u32be)
s = first_u64_be(h)
reject if s < (2^64 mod T)
ticket = s mod T
choose first i where ticket < sum(w[0..=i])
```

Lengths are byte counts. Rejection removes modulo bias under uniform hash samples;
selection is proportional to shares, not inverse distance or the number of full
paths. Equal weights are uniform but do not preserve v3 hash vectors (new domain).
Every draw checks the deadline. Exhausting 64 rejections returns `Unavailable`,
never a biased fallback. The maximum rejection probability per draw is <2^-26.
Replanning with identical signed IDs, lease, eligible hops and health is stable
across cache eviction and sync/async execution. Relays reselect at each hop.

Search/cancellation/admission and the bounded weak-lease FIFO cache are unchanged.
Cache request-independent alternatives, not weighted choices. Selection adds at
most 36 u64 weights and 64 SHA256 draws; no new cache or membership-wide scan.

## Independent vectors

Use v3's N=1500 `node-000000` through `node-001499` fixture, source 0,
destination 1499, request ID sixteen 0x01 bytes, attempt big-endian u128.
Weights are 1 at positions 83 and 833, otherwise 4. T=42, counter=0:

| Attempt | SHA256 | Ticket | Next hop |
| --- | --- | ---: | ---: |
| 0 | a9f87d811a3b45c5e92358f633977eb115f62e79a3d9c3968fa50585aaed7458 | 11 | 416 |
| 1 | 1f5a7acb5fa15aeda35ff3360f1f2b6ffedd20d00df482f8f331c71a0733c61d | 9 | 416 |
| 2 | 4ea070a9bdbae50212b207266cb4a86c7da4ca5cbddf6967eb9bf08de093a208 | 30 | 1166 |
| 127 | 7ebb0b4de1a2ea2fd466df1321f9ff7769e43ed4330da921ef7d0842681b028b | 19 | 666 |

## Compatibility and deployment boundary

`ALGORITHM_VERSION=5` is the latest supported-specification constant, not negotiation.
There is no topology-v3 wire header at this base revision. Signed route headers
encode membership, request/attempt, destination, visited list, links, acquisition
attempt budget and deadline (`security/protocol.rs:175-191`). The existing peer
profile/outer framing is already v4, independently of routing algorithm numbering.
Neither changes here. The selected receiver remains signed; a later recomputation
must not send an existing signature to a different receiver.

Selection is read at process startup, not carried in membership or negotiated.
A future use requires compatible images on all participating dataplanes, explicit
coordinated setting/restart, verified image/config coverage and old-transfer drain.
Mixed v2/v3/v4 selectors retain graph/signature/budget safety but are not an all-v4 model
or fairness result. Older images supporting the setting reject unknown value 4.
Do not infer algorithm adoption from a membership counter. Roll back the setting
before older images. No build, image push, restart, deployment, or annotation
change is part of this implementation. In particular preserve process affinity.
