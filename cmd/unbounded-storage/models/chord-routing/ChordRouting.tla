------------------------- MODULE ChordRouting -------------------------
(***************************************************************************
  Chord-style finger routing under possibly-divergent membership views,
  for the unbounded-storage p2p layer.

  WHAT THIS MODELS

  unbounded-storage routes stripe reads around a 64-bit consistent-hash
  ring using a per-node Chord finger table (src/p2p/fingers.rs). Each node
  deterministically computes its own table from the cluster peer list and
  routes a lookup recursively with closest-preceding-finger hops until some
  node reports that it OWNS the target (src/p2p/handler.rs `classify`). A
  TTL ("hops_remaining") bounds the recursion as a backstop
  (handler.rs:243-251).

  The protocol deliberately has NO stabilization phase: fingers.rs:26-29
  states that every node computes the SAME table from the SAME peer list,
  "which is how the protocol avoids a stabilization phase." The DST tests
  only ever feed one consistent peer-list snapshot to every node. The
  routing risk is DIVERGENT per-node views during a join/leave: config reloads
  are per-node and not globally atomic, so for a window different nodes route
  over different peer sets.

  This model checks three things:

    1. TERMINATION / NO-LIVELOCK. We encode the standard Chord
       termination argument as a per-hop invariant `ProgressInv`: every
       forward step either lands directly on the OWNER of the target, or
       it strictly reduces the forward ring distance to the target. A
       measure that is bounded below by 0 and strictly decreases (except on
       the single final hop into an owner) cannot loop, so lookups
       terminate without relying on the TTL. The TTL (`MaxHops`) remains a
       modelled backstop, and `NoHopLimit` asserts that under convergent
       views the backstop is never needed.

    2. OWNERSHIP UNIQUENESS. For every ring position exactly one node
       considers itself the owner (`OwnershipUnique`), i.e. next_hop
       returns None at exactly one node. This is the (predecessor, self]
       arc partition of fingers.rs:163-176 being a true partition.

    3. NO SELF-LOOP. next_hop never returns the calling node
       (`NoSelfLoop`), mirroring the Rust test no_self_loop_in_next_hop
       (fingers.rs:414-428).

  Under CONVERGENT views all three properties hold and lookups always
  terminate at an owner without touching the TTL. Under DIVERGENT views
  `ProgressInv` and `OwnershipUnique` can fail: a node reached via the
  successor-forward rule may, under its own stale view, not actually own the
  target, so the non-decreasing successor hop is no longer rescued by
  immediate ownership and routing can fail to make progress. In that regime
  termination falls back to the TTL bound alone. The committed configuration
  runs the convergent baseline; set `Divergent` to TRUE to explore the
  reload window.

  ABSTRACTION / SIMPLIFICATION:

  * The 64-bit ring is parameterized as integers mod `Modulus` (committed at
    8). Forward ring distance is the same wraparound arithmetic as ring.rs
    `ring_distance` (ring.rs:45-49), just modulo a small number.

  * Node identity is collapsed onto ring position: nodes ARE their distinct
    ring positions. The production code hashes NodeId onto the ring
    (ring.rs:41-43); the hash only fixes positions, which we fix directly. The
    arc-winner tie-break by topology / rendezvous / raw id (fingers.rs:203-221)
    IS modelled (`Better`), with the topology metric and the rendezvous hash
    abstracted to deterministic stand-ins but the lexicographic order intact.

  * The k-arc finger table (fingers.rs:53-92) is modelled as a sparse table:
    each node
    builds the genuine sparse `K`-arc table with one winner per arc
    (`Fingers` / `ArcWinner` / `ArcIndex`), and `closest_preceding` searches
    that sparse table, so over/undershoot behaviour matches production rather
    than being hidden by a whole-view superset.

  * Per-node views may be reloaded mid-lookup (`Reload`, bounded by
    `MaxReloads`) and a set of `Lookups` route concurrently, so the model
    exercises the transient-divergence window with several in-flight requests
     straddling a reload, not just one lookup over a frozen view.
 ***************************************************************************)

EXTENDS Naturals, FiniteSets

CONSTANTS
  Modulus,    \* size of the abstract ring; positions are 0 .. Modulus-1.
  Node,       \* set of node ring positions (distinct integers in 0..Modulus-1).
  MaxHops,    \* TTL backstop, mirrors handler.rs hops_remaining.
  K,          \* number of finger arcs per node (FingerTableConfig.k,
              \* fingers.rs:15-18). 1 <= K <= Modulus; a sparse table needs
              \* K < |Node| so two peers can collide in one arc.
  MaxReloads, \* bound on the number of mid-lookup config reloads. Keeps
              \* the run finite; the reload window is the motivating risk.
  Lookups,    \* set of concurrent in-flight lookup slots: independent
              \* recursive routings that may each observe a different snapshot
              \* of the per-node views as reloads land between their steps.
  Divergent,  \* FALSE: every node's view = the full cluster (convergent,
              \* the production stated assumption). TRUE: each node may hold
              \* its own arbitrary nonempty view containing itself, to model
              \* a non-atomic config reload window.
  NONE        \* opaque "no next hop / owner" sentinel; a TLC model value so
              \* it compares inequal to any node without a typing error.
              \* Mirrors fingers.rs next_hop returning Option::None.

ASSUME Node \subseteq (0 .. (Modulus - 1))
ASSUME MaxHops \in Nat
ASSUME K \in Nat /\ K >= 1 /\ K <= Modulus
ASSUME MaxReloads \in Nat
ASSUME Lookups /= {}
ASSUME Divergent \in BOOLEAN

Pos == 0 .. (Modulus - 1)

VARIABLES
  view,     \* Node -> SUBSET Node. Each node's local membership view. Reload
            \* may change one node's view while lookups are in flight.
  cur,      \* [Lookups -> Node \cup {NONE}] current node holding each in-flight
            \* lookup, or NONE when that lookup slot is idle.
  tgt,      \* [Lookups -> Pos] target ring position of each lookup.
  hops,     \* [Lookups -> 0..MaxHops] hops_remaining TTL of each lookup.
  status,   \* [Lookups -> {"idle","routing","owner","hoplimit"}].
  reloads   \* count of mid-lookup config reloads taken so far (2.1), bounded
            \* by MaxReloads.

vars == <<view, cur, tgt, hops, status, reloads>>

(***************************************************************************
  Ring arithmetic. Forward distance from `a` to `b`, wrapping at Modulus,
  exactly mirroring ring.rs `ring_distance` (ring.rs:45-49): the result is
  0 iff a = b, and otherwise the number of forward steps a -> b around the
  ring. With distinct integer positions, +Modulus before %Modulus keeps the
  subtraction non-negative for TLC's Naturals modulo.
 ***************************************************************************)
RingDist(a, b) == ((b - a) + Modulus) % Modulus

(***************************************************************************
  The peers a node routes over: its view minus itself. Mirrors the
  `cand.node == local.node { continue }` filter in fingers.rs build
  (fingers.rs:64-66) and the same filter in closest_preceding
  (fingers.rs:131-133).
 ***************************************************************************)
Peers(self, V) == V \ {self}

(***************************************************************************
  Immediate ring successor: the peer with the smallest nonzero forward ring
  distance from `self`. Mirrors fingers.rs:74-78. NONE only for a peerless
  (single-node) view. Distinct positions give distinct distances, so the
  CHOOSE has a unique minimizer and needs no tie-break (this is also why
  ring positions must NOT go in a symmetry group).
 ***************************************************************************)
Successor(self, V) ==
  LET cands == Peers(self, V) IN
  IF cands = {} THEN NONE
  ELSE CHOOSE p \in cands : \A q \in cands : RingDist(self, p) <= RingDist(self, q)

(***************************************************************************
  Immediate ring predecessor: the peer with the smallest nonzero BACKWARD
  ring distance to `self` (i.e. smallest RingDist(peer, self)). Mirrors
  fingers.rs:79-83. NONE only for a peerless view.
 ***************************************************************************)
Predecessor(self, V) ==
  LET cands == Peers(self, V) IN
  IF cands = {} THEN NONE
  ELSE CHOOSE p \in cands : \A q \in cands : RingDist(p, self) <= RingDist(q, self)

(***************************************************************************
  k-ARC FINGER TABLE, mirroring fingers.rs:53-92.

  The model partitions the abstract ring into K half-open arcs of width
  `ArcSpan`. A peer at forward distance d from `self` lands in arc
  floor(d / ArcSpan), capped at K-1.
 ***************************************************************************)
ArcSpan == Modulus \div K

ArcIndex(self, p) ==
  LET raw == RingDist(self, p) \div ArcSpan
  IN IF raw >= K THEN K - 1 ELSE raw

(***************************************************************************
  Tie-break terms for the arc winner (fingers.rs:203-221 `better`):
  lexicographic on (topology_distance, rendezvous_hash, raw ring id).

  * Topology distance (ring.rs:60-75) is abstracted to a two-level zone
    metric: 0 within a zone, 1 across. A node's zone is the half of the ring
    its position lies in. The exact prefix-match metric is irrelevant to
    routing; what this model verifies is that breaking arc ties by this lexicographic
    order never selects a finger that violates progress or ownership, and a
    metric that sometimes ties (two peers same zone) and sometimes does not is
    enough to exercise every branch of `better`.
  * Rendezvous hash (ring.rs:80-82) is abstracted to a deterministic,
    arc-decorrelated mixing of (self, p, arc); only determinism and spread
    matter to the tie-break, not the splitmix64 bit-mixing.
  * The final fallback is the raw ring position, which is distinct per node,
    so `Better` is a strict total order and every arc has a unique winner.
 ***************************************************************************)
Zone(n) == n \div (Modulus \div 2)
TopoDist(self, p) == IF Zone(self) = Zone(p) THEN 0 ELSE 1
RH(self, p, arc) == (self * 31 + p * 17 + arc * 13) % 97

Better(self, cand, inc, arc) ==
  IF TopoDist(self, cand) /= TopoDist(self, inc)
  THEN TopoDist(self, cand) < TopoDist(self, inc)
  ELSE IF RH(self, cand, arc) /= RH(self, inc, arc)
       THEN RH(self, cand, arc) < RH(self, inc, arc)
       ELSE cand < inc

(***************************************************************************
  Winner of arc `a` among `self`'s peers: the candidate that beats every
  other candidate in the arc under `Better` (fingers.rs:68-72). NONE when no
  peer falls in the arc (the "slot stores a clone of local" sentinel,
  fingers.rs:33-36, filtered out of routing). `Better` is a strict total
  order, so the winner is unique.
 ***************************************************************************)
ArcWinner(self, V, a) ==
  LET cands == { p \in Peers(self, V) : ArcIndex(self, p) = a } IN
  IF cands = {} THEN NONE
  ELSE CHOOSE w \in cands : \A q \in cands \ {w} : Better(self, w, q, a)

(***************************************************************************
  The SPARSE finger set: one winner per non-empty arc. Unlike the previous
  abstraction (finger set = whole view), this is a genuine sparse table, so
  `closest_preceding` can under/overshoot exactly as the production search
  does (fingers.rs:30-37, 123-144).
 ***************************************************************************)
Fingers(self, V) == { ArcWinner(self, V, a) : a \in 0 .. (K - 1) } \ {NONE}

(***************************************************************************
  Closest-preceding-finger lookup over the SPARSE finger set, faithfully
  including the deliberate INCLUSIVE bound `d <= limit` of fingers.rs:114-144
  (the production code's one intentional deviation from textbook Chord).
  Among fingers whose forward distance d from `self` is nonzero and does not
  overshoot the target (`limit = RingDist(self, target)`), return the one
  with the LARGEST such d; NONE if there is none. The inclusive `<= limit`
  lets a finger sitting exactly on the target be returned directly.
 ***************************************************************************)
ClosestPreceding(self, V, t) ==
  LET limit == RingDist(self, t) IN
  IF limit = 0 THEN NONE
  ELSE LET cands == { f \in Fingers(self, V) :
                        RingDist(self, f) /= 0 /\ RingDist(self, f) <= limit } IN
       IF cands = {} THEN NONE
       ELSE CHOOSE p \in cands :
              \A q \in cands : RingDist(self, p) >= RingDist(self, q)

(***************************************************************************
  next_hop: the Chord find_successor of fingers.rs:146-192, applying the
  four documented termination rules in order over the node's LOCAL view:

    (1) Own it if target in (predecessor, self]  -> NONE   (fingers.rs:163-176)
    (2) Else forward to successor if target in (self, successor]
                                                  -> succ   (fingers.rs:178-185)
    (3) Else hop through the closest preceding finger
                                                  -> cp     (fingers.rs:187-190)
    (4) Else fall back to the successor           -> succ   (fingers.rs:191)

  A peerless view (single-node cluster) owns everything: NONE
  (fingers.rs:173-176). Returns NONE for an owner, otherwise the node to
  forward to.

  Rule (4) remains explicit because a sparse finger table may have no usable
  closest-preceding finger even when a successor exists.
 ***************************************************************************)
NextHop(self, V, t) ==
  IF t = self THEN NONE
  ELSE LET pred == Predecessor(self, V) IN
       IF pred = NONE THEN NONE
       ELSE IF LET span == RingDist(pred, self)
                    off  == RingDist(pred, t)
                IN off /= 0 /\ off <= span
            THEN NONE
            ELSE LET succ == Successor(self, V) IN
                 IF LET span2 == RingDist(self, succ)
                        off2  == RingDist(self, t)
                    IN off2 /= 0 /\ off2 <= span2
                 THEN succ
                 ELSE LET cp == ClosestPreceding(self, V, t) IN
                      IF cp /= NONE THEN cp ELSE succ

(***************************************************************************
  The views a single node may hold. Convergent (Divergent = FALSE): the one
  configuration in which the node sees the full cluster, the production
  stated assumption (fingers.rs:26-29). Divergent: any view that contains
  the node itself and at least one peer, modelling the per-node config-reload
  window in which views disagree.
 ***************************************************************************)
AllowedNodeView(n) ==
  IF Divergent
  THEN { S \in SUBSET Node : n \in S /\ S /= {n} }
  ELSE { Node }

(***************************************************************************
  The set of whole-cluster view assignments allowed by the run: every node
  independently holds one of its allowed views.
 ***************************************************************************)
AllowedViews ==
  { vw \in [Node -> SUBSET Node] : \A n \in Node : vw[n] \in AllowedNodeView(n) }

(***************************************************************************
  Initial state: pick a view assignment; every lookup slot sits idle. tgt is
  given a harmless default; Start overwrites it.
 ***************************************************************************)
Init ==
  /\ view \in AllowedViews
  /\ cur = [l \in Lookups |-> NONE]
  /\ tgt = [l \in Lookups |-> 0]
  /\ hops = [l \in Lookups |-> MaxHops]
  /\ status = [l \in Lookups |-> "idle"]
  /\ reloads = 0

(***************************************************************************
  Start(l, s, t): begin a fresh lookup in slot l for target position t at
  node s, with a full TTL. Enabled only while that slot is idle. TLC fans out
  over every (l, s, t); with several slots, multiple lookups route
  concurrently (2.4), each reading the views independently as it advances, so
  a reload landing between two slots' steps is observed by one and not the
  other. Mirrors a request entering RecursiveHandler with hops_remaining =
  MaxHops.
 ***************************************************************************)
Start(l, s, t) ==
  /\ status[l] = "idle"
  /\ cur' = [cur EXCEPT ![l] = s]
  /\ tgt' = [tgt EXCEPT ![l] = t]
  /\ hops' = [hops EXCEPT ![l] = MaxHops]
  /\ status' = [status EXCEPT ![l] = "routing"]
  /\ UNCHANGED <<view, reloads>>

(***************************************************************************
  Step(l): one routing step for lookup l, mirroring handler.rs `classify` +
  the forward branch (handler.rs:243-251, 323-347). The lookup's current node
  consults ITS OWN view:
    * next_hop = NONE        -> Owner: this node owns the target; terminate.
    * next_hop = Some, hops 0 -> HopLimit: TTL exhausted; terminate.
    * next_hop = Some, hops>0 -> Forward: move to the next hop, TTL - 1.
 ***************************************************************************)
Step(l) ==
  /\ status[l] = "routing"
  /\ LET h == NextHop(cur[l], view[cur[l]], tgt[l]) IN
       IF h = NONE
       THEN /\ status' = [status EXCEPT ![l] = "owner"]
            /\ UNCHANGED <<cur, hops>>
       ELSE IF hops[l] = 0
            THEN /\ status' = [status EXCEPT ![l] = "hoplimit"]
                 /\ UNCHANGED <<cur, hops>>
            ELSE /\ cur' = [cur EXCEPT ![l] = h]
                 /\ hops' = [hops EXCEPT ![l] = hops[l] - 1]
                 /\ status' = [status EXCEPT ![l] = "routing"]
  /\ UNCHANGED <<view, tgt, reloads>>

(***************************************************************************
  Reload: a single node swaps its membership view MID-LOOKUP, modelling
  the non-atomic per-node config reload that motivated this whole model. Every
  in-flight lookup keeps its own cur / tgt / hops, so requests that already
  passed some hops now continue over a changed topology, and concurrent
  lookups (2.4) may straddle the reload differently. Bounded by MaxReloads via
  the `reloads` counter so the run stays finite.

  Under CONVERGENT views (Divergent = FALSE) the only allowed view is the
  full cluster, so the `S /= view[n]` guard is never satisfiable and Reload
  is disabled: the committed baseline is unchanged and stays green. Under
  DIVERGENT views Reload lets an in-flight lookup observe a topology change
  partway through, the precise transient the model exists to probe; there it
  can drive a lookup into the TTL backstop (NoHopLimit fails by design).
 ***************************************************************************)
Reload(n, S) ==
  /\ \E l \in Lookups : status[l] = "routing"
  /\ S \in AllowedNodeView(n)
  /\ S /= view[n]
  /\ reloads < MaxReloads
  /\ view' = [view EXCEPT ![n] = S]
  /\ reloads' = reloads + 1
  /\ UNCHANGED <<cur, tgt, hops, status>>

(***************************************************************************
  A terminated lookup (owner or hoplimit) takes no further action. With
  CHECK_DEADLOCK FALSE these are simply absorbing leaves of the state graph,
  as in the CowBtreeCrash model.
 ***************************************************************************)
Next ==
  \/ \E l \in Lookups, s \in Node, t \in Pos : Start(l, s, t)
  \/ \E l \in Lookups : Step(l)
  \/ \E n \in Node : \E S \in AllowedNodeView(n) : Reload(n, S)

Spec == Init /\ [][Next]_vars

(***************************************************************************
  TERMINATION / NO-LIVELOCK. The Chord termination measure as a
  per-hop invariant, quantified over every node and every target so it holds
  in every reachable state regardless of the in-flight lookup. For any node
  n that does NOT own target t, let h be the hop it forwards to. The step is
  "good" iff EITHER h strictly reduces the forward ring distance to t (a
  measure bounded below by 0, so finitely many such hops), OR h is the
  immediate owner of t under h's own view (the single terminal hop that the
  successor-forward rule 2 makes). A routing that only ever takes good steps
  cannot livelock.

  Under convergent views this holds (rule 3 strictly decreases the distance;
  rule 2 forwards to the genuine owner). Under divergent views the rule-2
  forward target may not own t in its stale view, breaking the disjunction.
 ***************************************************************************)
ProgressInv ==
  \A n \in Node, t \in Pos :
    LET h == NextHop(n, view[n], t) IN
      (h /= NONE) =>
        \/ RingDist(h, t) < RingDist(n, t)
        \/ NextHop(h, view[h], t) = NONE

(***************************************************************************
  OWNERSHIP UNIQUENESS. Exactly one node owns each ring position: next_hop
  returns NONE (owner) at precisely one node. This is the (predecessor,
  self] arc partition (fingers.rs:163-176) being a genuine partition of the
  ring. Holds under convergent views; can fail under divergent views (zero
  or several nodes may each believe they own t).
 ***************************************************************************)
Owners(t) == { n \in Node : NextHop(n, view[n], t) = NONE }

OwnershipUnique ==
  \A t \in Pos : Cardinality(Owners(t)) = 1

(***************************************************************************
  NO SELF-LOOP. next_hop never hands a request back to the calling node,
  mirroring the Rust test no_self_loop_in_next_hop (fingers.rs:414-428).
  NONE (owner) trivially satisfies this since NONE /= n.
 ***************************************************************************)
NoSelfLoop ==
  \A n \in Node, t \in Pos : NextHop(n, view[n], t) /= n

(***************************************************************************
  The TTL backstop is never needed under convergent views: every lookup
  terminates at an owner without exhausting hops_remaining. If this holds,
  termination is established by monotone progress alone, not by the TTL.
  Divergent views may burn the whole TTL.
 ***************************************************************************)
NoHopLimit ==
  \A l \in Lookups : status[l] /= "hoplimit"

(***************************************************************************
  Keep the TTL within its declared bound (it only ever counts down from
  MaxHops). Mirrors the StateConstraint discipline of CowBtreeCrash; here it
  is a cheap sanity bound rather than a finiteness necessity.
 ***************************************************************************)
StateConstraint ==
  /\ \A l \in Lookups : hops[l] <= MaxHops
  /\ reloads <= MaxReloads

=============================================================================
