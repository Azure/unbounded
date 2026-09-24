# Racer physical product routing

## Decision and scope

Routing algorithm 2 separates physical cache ownership from the peer graph. It
replaces the production algorithm-1 slot graph with a Cartesian product of small
diameter-two graphs. At a cold, exactly 10,000-member configuration, the selected
graph is Hoffman-Singleton(50) x Abas(200): **23 distinct bidirectional peers per
physical node and at most four successful forwarding edges**, including local
repair around one failed intermediate node.

This document supersedes earlier Racer slot-routing proposals for algorithm 2.
The implemented predecessor is algorithm 1 with RR01, not the algorithm-3/RF06
numbers in historical proposals. Algorithm 1 remains readable; the compiler emits
algorithm 2. The control API and persistent cache format retain their existing
versions. Deploy matching control planes and dataplanes for the new algorithm.

The contract counts all incident physical peers, including accepted connections.
It does not count one initiator's outgoing table as the entire graph. Multiple
worker/rail transport sessions to one peer are still multiple sockets: 23 is a
peer bound, not a process-wide socket limit or a throughput guarantee.

## Placement remains stable

Keep 262,144 placement slots and the existing key-to-slot mapping. Rank physical
Node identities with the existing universe-separated HRW score and lexical tie
break. The first candidate is exactly the previous algorithm's primary owner.
Publish the highest-ranked `min(max_candidate_attempts, member_count)` distinct
physical members for each slot, with a maximum width of eight. A candidate is a
possible owner, not a promise that a replica is already cached there.

Role assignments, graph factors, Pod identities, endpoints, and publication
revisions are not score inputs. Graph repair and profile changes therefore cause
no ownership changes. Adding an equal-capacity member moves an expected
`1/(N+1)` primary fraction; removing one moves its assignments only. These are
keyspace expectations, not byte or traffic bounds. This version retains equal
Node weights: storage resize does not introduce a new placement-weight policy.

The compiler computes the ranking once per universe. Its disposable accelerator
compares additions against existing rankings and rescans rows whose ranked
members disappeared. Cold compilation evaluates `S*N` scores with bounded top-k
insertion work, up to `O(S*N*k)` comparisons. At 10,000 members the score count is
2,621,440,000. No new cold-start capacity claim follows from this change.

Candidate tables use generation-local u32 member indexes. A width-three table is
3 MiB before protobuf framing. The current wire representation repeats this
table per volume and recipient; aggregate snapshot admission includes it. It is
not a delta protocol. Increasing volume count can therefore reach admission
limits before the nominal volume-count ceiling.

Cache namespace, page identity, generation, and slab format do not change.
Topology changes never invalidate locally cached bytes. Cache hits and normal
eviction keep their existing semantics. This design does not add proactive data
migration or previous-owner cache-only probes. Unchanged physical ownership
preserves normal discovery; moved ownership can require refilling. HRW's second
rank after one addition identifies the previous primary, but is not implicitly a
cache-only probe on a healthy primary miss.

Implementation: `cmd/racer-controlplane/src/placement_candidates.rs`,
`cmd/racer-controlplane/src/topology.rs`, and the existing dataplane namespace.

## Graph and membership

`internal/racer/product.rs` is shared source compiled by both Rust crates. Its
versioned factor codes are cliques K1 through K32, Hoffman-Singleton(50), and
Abas(200). Factor adjacency and shortest-path tables are initialized once.

The Hoffman-Singleton construction uses five pentagons and five pentagrams with
the standard cross edges. The Abas graph is the degree-16 Cayley graph on
`Z10^2 semidirect Z2`, where the nontrivial Z2 action swaps coordinates. Its
inverse-closed generating set is specified directly in the shared source and
matches [Abas, Theorem 4.1](https://arxiv.org/html/1509.00842v4#S4).
Factor degree, symmetry, diameter, and failure properties have executable tests.

For `N` in 1 through 100,000, cold selection enumerates supported factor pairs
whose product order `R` is at most N, minimizing

```
ceil(N / R) * (left_degree + right_degree)
```

Ties prefer the largest base, then the lexically first factor pair. This is an
optimum over the implemented profiles, not over all possible graphs. The graph
research establishes only a necessary degree lower bound of 13 for this
10,000-node, four-edge, single-fault objective; 23 is not a global-optimality claim.

Each physical node occupies exactly one base role. Every role is nonempty.
Multiple members of one role form an independent bundle, not multiple logical
vertices hosted by one node. Each base edge becomes all physical edges between
its endpoint bundles. With occupancy at most b, maximum peers are at most b
times base degree. Same-role endpoints use two edges through a neighboring role;
the final edge always chooses the actual destination member.

Bundles are balanced to within one member. A bundled base must have degree at
least two: K1 is valid only at N=1 and K2 only at N=2. Otherwise a singleton K2
bridge could disconnect two surviving same-role endpoints after one failure.
Both compiler and dataplane enforce the shared membership predicate.

While a profile remains admissible, the compiler retains survivor roles, assigns
new members to least-loaded roles, and moves only excess members needed to fill
vacancies or rebalance. Retention requires `R <= N <= 2R` and its conservative
peer bound to be no more than twice the cold-selected bound. Outside that band,
select a new profile. Thus a cold 10,000-node graph has 23 peers, but a graph grown
to 10,000 under a retained profile need not have 23. The 10,000-role profile can
grow through 20,000 members with at most 46 peers.

Role state is ephemeral, consistent with existing control-plane recovery.
Leadership restart deterministically rebuilds from sorted membership and may
rewire roles. It does not reshuffle physical HRW ownership. No new durable
assignment record or fencing protocol is introduced.

All active members receive configured volumes, even if they own zero primary
slots: they may be required forwarders. A member's direct peer set is exactly all
physical members of adjacent roles, usable in both directions. The authenticated
membership catalog remains separate from direct connectivity.

## Four-edge routing and failure model

The guarantee assumes a stable admitted topology, one failed intermediate
physical node, surviving endpoints, and an intact request retained at the
surviving predecessor. Incident failure detection can take time. Count all
successful physical edges, including the prefix before failure discovery.
Timeouts, attempted failed transmissions, response traversal, and a crash after
accepting the only request copy are not bounded by this graph theorem.

For two nontrivial diameter-two factors, healthy routing always advances a
coordinate at remaining distance two before one at distance one. From `(2,2)`
the distance states are `(1,2)`, `(1,1)`, then `(0,1)` and `(0,0)`. Completing an
entire coordinate first is not equivalent and can exhaust the repair budget.

If a proposed next role fails while the other coordinate is unfinished, advance
the other coordinate, finish the blocked coordinate in that different layer,
then finish the remaining coordinate. This costs the same remaining distance.
If only distance two in one coordinate remains, the priority rule means this
must be the initial state: moving to another layer, taking two factor steps,
and returning costs four edges. A distance-one final step reaches the live
destination. Degenerate single-factor profiles have their bounded replacement
paths covered by the factor tests.

The implementation chooses a deterministic shortest replacement with a
four-edge BFS limit and rejects it unless successful prefix plus suffix is at
most four. If another physical member of the failed role exists, substitute
that member without changing the base path. Same-role endpoints choose another
neighbor if their first intermediate fails. One repair is allowed per candidate.

This theorem is stronger than fault diameter alone. Undirected decimal de Bruijn
has a verified 20-peer, four-edge surviving-path result, but a packet discovering
a remote failure after taking a prefix can require more than four total edges.
That construction was rejected for single-copy local recovery.

## Wire validation and retry authority

`Topology.product` contains factor codes, sorted physical member identities,
aligned role assignments, the local member index, candidate width, and a flat
slot-major candidate table. Receivers validate complete balanced roles, unique
members and candidate rows, local ownership, local identity, membership binding,
and exact adjacent-role peers. Existing work, record, and wire limits still
bound admission.

RR02 is distinct from RR01. Its fixed 71-byte body includes the topology digest,
source member, placement slot, candidate ordinal, current position, up to five
physical path indexes, failed member, and repair position. Unused path entries
have canonical padding. The digest binds namespace, epoch, graph, members, roles,
and rankings. Each receiver reconstructs and checks the full deterministic path
or its repair, verifies owner binding, and checks its own position. See
`cmd/racer-dataplane/CONTRACT.md` for byte layout and descriptor admission limits.

Only qualifying initiated transport evidence about the immediate intermediate
can authorize repair. Busy, cancellation, malformed responses, authentication
errors, and breaker rejection cannot masquerade as a failed node. RDMA first
falls back to HTTP at the same peer. Validated owner failure still authorizes
only ingress to advance to the next physical owner. A relay never changes the
candidate ordinal.

Same-candidate repair preserves the candidate deadline, absolute caller
deadline, receive-admission rank, and existing hop/work authority. Each forwarding
edge reserves 500 ms once in the child's wire budget; the owner's backend has
its separate return reserve. Tests cover seven and eight candidates over four
cold HTTP/TLS edges. Failure evidence is fenced by HTTP breaker generation, so
stale completion cannot erase newer failure evidence or undo recovery.

The graph bound does not replace the existing eight-hop/255-work execution
budget. Failed attempts and transport recovery spend finite authority. Small
receive pools can reject a four-edge cold request; the default eight-buffer
configuration provides more allowance than the four-buffer minimum. Product
flights are request-local to avoid repair-induced self-wait cycles. Publication
authority is dropped before refilling; CRC/semantic validation precedes success;
post-header streaming failure still aborts the response.

## Publication, transitions, and availability

Topology publication remains independently applied full snapshots. A subscription
receipt is not a fleet-wide application acknowledgment. Workers retain the
existing local prepare/commit and generation-drain mechanisms.

A product request with another topology identity fails closed. It does not
translate member indexes, rebase to a new role, or reset the four-edge progress.
Mixed generations can therefore interrupt requests until neighbors converge;
finite execution and correct ownership take precedence over silently exceeding
the path bound. There is no claim of uninterrupted rolling reconfiguration.

An occupied-role join changes only edges incident to the joining member within
one stable profile. Singleton departure is covered as the one failed role until
replacement/rebalancing. Two accumulated holes exceed the single-failure model.
Changes in a published membership set create a new identity even when most roles
stay fixed. Healthy configurations and single-failure routing have the stated
bounds; arbitrary partially installed graphs do not.

During local generation draining, connections for old and new graphs may coexist.
The steady-state peer bound is not a transition-time socket cap. A profile
replacement may change the entire peer set. Existing bounded retention limits
apply; this change introduces neither a global rollout barrier nor a new
make-before-break connection-cap guarantee.

## Verification and operational limits

Coverage includes independent factor checks, prefix-plus-repair paths, physical
bundles and small-cluster growth, malformed snapshots and cursors, exact HRW
primary preservation, distinct rankings, incremental/cold equivalence, 10,000-role
degree checks, compiler-to-dataplane protobuf conformance, and real io_uring/TLS
four-hop streaming with repair, hashes, and buffer recovery. Deadline retention,
maximum candidate budgets, and stale breaker evidence have regression tests.

The 10,000-role graph tests use bounded placement fixtures; they do not certify
10,000 concurrent production subscribers or full-size cold compilation latency.
Hardware RDMA offload, a production Kubernetes failure campaign, and ignored
capacity tests need their own qualified environments. A graph theorem does not
establish origin capacity, bandwidth balance, multi-failure availability, or
failure-detection latency.

Primary research includes [Hoffman and Singleton](https://doi.org/10.1147/rd.45.0497),
[Abas](https://arxiv.org/html/1509.00842v4),
[HRW](https://www.cs.ucr.edu/~ravi/Papers/Jrnl/HRW98.pdf), and
[PolarFly](https://htor.inf.ethz.ch/publications/img/besta-pf.pdf). The local product
repair proof and independent-bundle lifting above are derived here, not claims
attributed to those papers.
