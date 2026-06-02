------------------------- MODULE FabricCompletion -------------------------
(***************************************************************************
  Fabric in-flight request tracking + completion correlation + connection
  state machine model for unbounded-storage.

  This specifies, in one abstract state machine, the three correctness
  contracts the fabric layer relies on but cannot test exhaustively in
  Rust because the adversary (libfabric / the io_uring kernel) is free to
  deliver completion events in any order and to tear the connection down
  at any instant:

    1. BOUNDED IN-FLIGHT.  The completion registry reserves capacity at
       allocate time and only releases it when the boxed slot is freed
       (src/fabric/completion.rs:233-261 `allocate`, :175-182 `Drop`,
       :205-217 `RegistryShared`/`CompletionRegistry`).  No more than
       `max_inflight` (src/fabric/config.rs:38 `max_inflight`) operations
       may be outstanding at once, and the registry's live counter must
       always equal the true number of outstanding operations
       (the `submitted` counter in src/ring/core.rs:73-75 carries the
       same obligation for the ring's own back-pressure queue).

    2. EXACTLY-ONCE COMPLETION.  Each operation owns one `CompletionSlot`
       whose result should be stored and woken once
       (src/fabric/completion.rs:151-160 `complete`, :71-80
       `AtomicWaker::wake`).  The progress thread reclaims the slot Box
       that libfabric handed back as the op_context, and the model requires
       each op_context to be completed only while its operation is still
       outstanding (src/fabric/progress.rs:168-185 `deliver_success`,
       :187-211 `drain_errors`).  No stray or duplicate completion may be
       delivered for an id that is not outstanding.

    3. SEND_ZC TWO-CQE BUFFER SAFETY.  An `IORING_OP_SEND_ZC` submission
       yields EXACTLY TWO CQEs: a send-completion carrying
       `IORING_CQE_F_MORE` (the byte count) followed by a notification
       that clears `F_MORE` and signals the source buffer is released
       (src/ring/core.rs:23-32 doc, :392-419 `progress`, :96-104 `Slot`,
       src/ring/network.rs:796-802 the test that pins the protocol).  The
       send buffer must be released ONLY after the second (notification)
       CQE; releasing it after the first is a use-after-free.

  The Connecting -> Handshaking -> Established -> TearingDown -> Down
  lifecycle captures the request-issuance and teardown contract: requests may
  only be issued while Established, and teardown must resolve every still-
  outstanding request before the connection goes Down, never silently stranding
  a pinned buffer.


  The adversary may deliver CQEs in any order and may begin teardown at
  any time; the back-pressure cap, the correlation, and the buffer-release
  ordering must hold across every interleaving.
 ***************************************************************************)

EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
  Ids,          \* set of abstract slot identities reused over time. `gen`
                \* distinguishes successive lifetimes of the same identity.
  MaxInflight,  \* back-pressure cap; == FabricConfig.max_inflight
  MaxOps        \* bound on TOTAL ops ever issued (slots are reused, so this,
                \* not |Ids|, is what keeps the run finite)

ASSUME MaxInflight \in Nat /\ MaxInflight >= 1
ASSUME MaxOps \in Nat /\ MaxOps >= 1

(***************************************************************************
  Per-request status.  An id starts "Unused", becomes "Issued" (== live in
  the registry / outstanding) when submitted, and reaches exactly one of
  two terminal states: "Completed" (its completion CQE(s) arrived) or
  "Failed" (resolved as failed during connection teardown).  Mirrors the
  registry live-count lifecycle: alloc -> live -> box dropped.
 ***************************************************************************)
Status == {"Unused", "Issued", "Completed", "Failed"}

(***************************************************************************
  Operation kind, decided at issue time.  "Zc" is a SEND_ZC op that owes
  two CQEs; "NonZc" is any single-CQE op (recv, write, accept, the
  non-zero-copy send path).  "None" is the placeholder for an Unused id.
  Keeping the kind per-id means permuting ids permutes their kinds, so the
  Ids symmetry reduction stays sound.
 ***************************************************************************)
Kind == {"None", "Zc", "NonZc"}

(***************************************************************************
  Connection lifecycle.  Down is the quiescent end of one connection, but
  the registry and ring outlive it, so Down may Reconnect to Connecting
  (any still-pending late CQEs carry across the recovery).
 ***************************************************************************)
ConnState == {"Connecting", "Handshaking", "Established", "TearingDown", "Down"}

VARIABLES
  conn,       \* current ConnState.
  status,     \* [Ids -> Status].
  kind,       \* [Ids -> Kind].
  cqe,        \* [Ids -> 0..2]  number of CQEs delivered for this id.
              \* SEND_ZC needs 2; single-CQE ops need 1.
  replySet,   \* [Ids -> BOOLEAN]  whether `CompletionSlot::complete` has
              \* fired for this id (the reply-slot / result store).  Set on
              \* the send-result CQE, exactly once.
  bufPinned,  \* [Ids -> BOOLEAN]  whether the source buffer is still
              \* pinned (must not be freed).  Pinned from issue until the
              \* releasing event (2nd CQE for Zc, the single CQE for NonZc,
              \* or a teardown failure).
  inflight,   \* Nat.  The registry's live counter
              \* (completion.rs:207 `live`).  Tracked separately from the
              \* set of Issued ids so the model can catch a counter that
              \* drifts from reality (counter integrity).
  gen,        \* [Ids -> Nat]  per-slot GENERATION in the abstract model. A
              \* reused identity is reissued carrying gen+1, so a stale CQE
              \* captured at an old generation can be told apart from the live
              \* op now occupying the same identity.
  issued,     \* Nat.  TOTAL ops ever issued (monotone).  Bounded by MaxOps so
              \* that slot reuse does not produce an unbounded run.
  ringInflight, \* Nat.  The io_uring `submitted` back-pressure counter
              \* (src/ring/core.rs:73-75).  Distinct from `inflight` (the
              \* libfabric `live` registry counter), with its OWN cap: a
              \* SEND_ZC op that is torn down between its two CQEs leaves the
              \* libfabric op resolved (inflight drops, freeing libfabric
              \* capacity) while its ring notification slot is still
              \* outstanding (ringInflight stays up) until the kernel late
              \* CQE.  Because the two caps are independent, ringInflight can
              \* legitimately exceed MaxInflight by the number of such
              \* still-pending notifications (|lateCqes|).
  lateCqes    \* SUBSET [id: Ids, gen: Nat].  Notification CQEs the kernel still
              \* owes for SEND_ZC ops that were failed during teardown between
              \* their two CQEs.  Each carries the generation it was issued at.

vars == <<conn, status, kind, cqe, replySet, bufPinned, inflight,
          gen, issued, ringInflight, lateCqes>>

(***************************************************************************
  Helpers.
 ***************************************************************************)

\* The set of outstanding (live / in-flight) request ids.  This is the
\* ground truth the registry's `live` counter is supposed to mirror.
Outstanding == { i \in Ids : status[i] = "Issued" }

Init ==
  /\ conn = "Connecting"
  /\ status = [i \in Ids |-> "Unused"]
  /\ kind = [i \in Ids |-> "None"]
  /\ cqe = [i \in Ids |-> 0]
  /\ replySet = [i \in Ids |-> FALSE]
  /\ bufPinned = [i \in Ids |-> FALSE]
  /\ inflight = 0
  /\ gen = [i \in Ids |-> 0]
  /\ issued = 0
  /\ ringInflight = 0
  /\ lateCqes = {}

(***************************************************************************
  Connection transitions.

  Connecting -> Handshaking -> Established model the bring-up before any
  op may be submitted; Established -> TearingDown begins shutdown (the
  adversary may trigger it at any time); TearingDown -> Down completes
  shutdown but ONLY once no request is still outstanding.  That last guard
  is the contract that teardown must drain / fail every in-flight op
  before the connection is declared Down, never leaving a pinned buffer
  behind (analogue of the progress thread's refusal to tear down on
  transient errors and strand in-flight ops, src/fabric/progress.rs:160-165).
 ***************************************************************************)
Bringup ==
  /\ \/ /\ conn = "Connecting"
        /\ conn' = "Handshaking"
     \/ /\ conn = "Handshaking"
        /\ conn' = "Established"
  /\ UNCHANGED <<status, kind, cqe, replySet, bufPinned, inflight,
                 gen, issued, ringInflight, lateCqes>>

\* Begin teardown.  Outstanding ops are not touched here; they are
\* resolved one at a time by FailRequest below.
BeginTeardown ==
  /\ conn = "Established"
  /\ conn' = "TearingDown"
  /\ UNCHANGED <<status, kind, cqe, replySet, bufPinned, inflight,
                 gen, issued, ringInflight, lateCqes>>

\* Finish teardown.  Permitted only when nothing is outstanding, so a
\* pinned buffer can never survive into the Down state.
FinishTeardown ==
  /\ conn = "TearingDown"
  /\ Outstanding = {}
  /\ conn' = "Down"
  /\ UNCHANGED <<status, kind, cqe, replySet, bufPinned, inflight,
                 gen, issued, ringInflight, lateCqes>>

\* Reconnect after a Down.  The completion registry and the io_uring ring
\* outlive any single connection, so a fresh connection may be brought up
\* while a stale SEND_ZC notification CQE from the previous connection is
\* still pending in `lateCqes`.  This is what lets a recycled slot be
\* reissued (at a bumped generation) WHILE its old occupant's late CQE is
\* still in flight, the exact collision GenerationDetectable must rule out.
\* It carries no buffers or counters across: only `conn` changes.
Reconnect ==
  /\ conn = "Down"
  /\ conn' = "Connecting"
  /\ UNCHANGED <<status, kind, cqe, replySet, bufPinned, inflight,
                 gen, issued, ringInflight, lateCqes>>

(***************************************************************************
  Issue a request.

  Models `CompletionRegistry::allocate` (src/fabric/completion.rs:233-261):
  capacity is reserved first (`live.fetch_add`), and on overflow it is
  given back, so a fresh slot only exists when `live <= cap`.  We may only
  submit while the connection is Established, we pick a free (Unused) slot
  identity (one whose previous op, if any, has terminated and been
  recycled), bump both the libfabric live counter and the io_uring submitted
  counter, and pin the source buffer.  The op is live at the slot's current
  generation `gen[i]`.  `issued < MaxOps` bounds the total number of ops so
  that slot reuse keeps the run finite.  `k` chooses whether this is a
  SEND_ZC op (two CQEs owed) or a single-CQE op.
 ***************************************************************************)
Issue(i, k) ==
  /\ conn = "Established"
  /\ inflight < MaxInflight
  /\ issued < MaxOps
  /\ status[i] = "Unused"
  /\ k \in {"Zc", "NonZc"}
  /\ status' = [status EXCEPT ![i] = "Issued"]
  /\ kind' = [kind EXCEPT ![i] = k]
  /\ bufPinned' = [bufPinned EXCEPT ![i] = TRUE]
  /\ inflight' = inflight + 1
  /\ issued' = issued + 1
  /\ ringInflight' = ringInflight + 1
  /\ UNCHANGED <<conn, cqe, replySet, gen, lateCqes>>

(***************************************************************************
  Deliver the FIRST CQE of a SEND_ZC op: the send-completion carrying
  `IORING_CQE_F_MORE` (src/ring/core.rs:392-402).  It records the result
  and fires the reply-slot (`complete`, src/fabric/completion.rs:151-160),
  but it does NOT resolve / release: the buffer stays pinned and the op
  stays Issued until the notification CQE.  The `replySet[i] = FALSE`
  guard (and `cqe[i] = 0`) enforce that the reply-slot is set exactly once.
 ***************************************************************************)
DeliverFirstCqeZc(i) ==
  /\ status[i] = "Issued"
  /\ kind[i] = "Zc"
  /\ cqe[i] = 0
  /\ replySet[i] = FALSE
  /\ cqe' = [cqe EXCEPT ![i] = 1]
  /\ replySet' = [replySet EXCEPT ![i] = TRUE]
  /\ UNCHANGED <<conn, status, kind, bufPinned, inflight,
                 gen, issued, ringInflight, lateCqes>>

(***************************************************************************
  Deliver the SECOND CQE of a SEND_ZC op: the notification that clears
  `F_MORE` and signals the source buffer is released (src/ring/core.rs:404-419).
  Only now may the buffer be unpinned; the op resolves (Completed), the
  slot Box is dropped and the live counter drops by one
  (src/fabric/completion.rs:175-182; src/ring/core.rs:412-413
  `submitted.set(n - 1)`).  Requires the first CQE to have already arrived
  (`cqe[i] = 1`), which is what makes "release only after both CQEs" hold.
 ***************************************************************************)
DeliverSecondCqeZc(i) ==
  /\ status[i] = "Issued"
  /\ kind[i] = "Zc"
  /\ cqe[i] = 1
  /\ cqe' = [cqe EXCEPT ![i] = 2]
  /\ status' = [status EXCEPT ![i] = "Completed"]
  /\ bufPinned' = [bufPinned EXCEPT ![i] = FALSE]
  /\ inflight' = inflight - 1
  /\ ringInflight' = ringInflight - 1
  /\ UNCHANGED <<conn, kind, replySet, gen, issued, lateCqes>>

(***************************************************************************
  Deliver the single CQE of a non-zero-copy op.  One completion both fires
  the reply-slot and releases the buffer, resolving the op and dropping
  the live counter (the single-CQE branch of src/ring/core.rs:404-419 and
  the ordinary `deliver_success` path, src/fabric/progress.rs:168-185).
 ***************************************************************************)
DeliverCqeNonZc(i) ==
  /\ status[i] = "Issued"
  /\ kind[i] = "NonZc"
  /\ cqe[i] = 0
  /\ cqe' = [cqe EXCEPT ![i] = 1]
  /\ replySet' = [replySet EXCEPT ![i] = TRUE]
  /\ status' = [status EXCEPT ![i] = "Completed"]
  /\ bufPinned' = [bufPinned EXCEPT ![i] = FALSE]
  /\ inflight' = inflight - 1
  /\ ringInflight' = ringInflight - 1
  /\ UNCHANGED <<conn, kind, gen, issued, lateCqes>>

(***************************************************************************
  Fail an outstanding request during teardown.  This is the resolution
  path teardown owes every in-flight op: the op reaches the terminal
  Failed state, its buffer is forcibly unpinned, and the libfabric live
  counter drops, so no pinned buffer survives into Down.  We do NOT set the
  reply-slot here: a teardown failure is a resolution, not a completion,
  so it must not look like a delivered completion.

  The two CQE layers diverge here.  A SEND_ZC op torn down between its two
  CQEs (kind = "Zc", cqe[i] = 1) has already consumed its F_MORE CQE; the
  kernel still owes the notification CQE, so the io_uring submitted counter
  CANNOT drop yet.  We record that pending notification in `lateCqes`,
  stamped with the op's generation, and leave ringInflight untouched; the
  late CQE is drained later by DeliverLate.  In every other case (a NonZc
  op, or a Zc op failed before its first CQE) the ring slot is cancelled
  outright and ringInflight drops with inflight.
 ***************************************************************************)
FailRequest(i) ==
  /\ conn = "TearingDown"
  /\ status[i] = "Issued"
  /\ status' = [status EXCEPT ![i] = "Failed"]
  /\ bufPinned' = [bufPinned EXCEPT ![i] = FALSE]
  /\ inflight' = inflight - 1
  /\ \/ /\ kind[i] = "Zc"
        /\ cqe[i] = 1
        /\ lateCqes' = lateCqes \cup {[id |-> i, gen |-> gen[i]]}
        /\ UNCHANGED ringInflight
     \/ /\ ~(kind[i] = "Zc" /\ cqe[i] = 1)
        /\ ringInflight' = ringInflight - 1
        /\ UNCHANGED lateCqes
  /\ UNCHANGED <<conn, kind, cqe, replySet, gen, issued>>

(***************************************************************************
  Deliver a stale notification CQE that the kernel still owed for a SEND_ZC
  op failed mid-teardown.  It carries an old generation, so it cannot be
  mistaken for the live op (if any) now occupying the same slot identity:
  the only effect is to drain the ring's submitted counter.  It never
  touches op status, never re-fires a reply-slot, and never unpins a
  buffer.  This is the io_uring counterpart of dropping a CQE whose
  user_data no longer maps to a live ring slot.
 ***************************************************************************)
DeliverLate(lc) ==
  /\ lc \in lateCqes
  /\ lateCqes' = lateCqes \ {lc}
  /\ ringInflight' = ringInflight - 1
  /\ UNCHANGED <<conn, status, kind, cqe, replySet, bufPinned, inflight,
                 gen, issued>>

(***************************************************************************
  Recycle a terminal abstract slot identity back into the free pool, bumping
  its generation.  The slot becomes Unused and may be reissued; the generation
  bump is what lets any still-pending stale CQE for the old occupant be
  disambiguated from the new one.  Recycling does not touch any counter: the
  live and ring counters were already settled when the op terminated, and a
  still-pending late CQE stays in `lateCqes` carrying the OLD generation.
 ***************************************************************************)
Recycle(i) ==
  /\ status[i] \in {"Completed", "Failed"}
  /\ status' = [status EXCEPT ![i] = "Unused"]
  /\ kind' = [kind EXCEPT ![i] = "None"]
  /\ cqe' = [cqe EXCEPT ![i] = 0]
  /\ replySet' = [replySet EXCEPT ![i] = FALSE]
  /\ bufPinned' = [bufPinned EXCEPT ![i] = FALSE]
  /\ gen' = [gen EXCEPT ![i] = gen[i] + 1]
  /\ UNCHANGED <<conn, inflight, issued, ringInflight, lateCqes>>

Next ==
  \/ Bringup
  \/ BeginTeardown
  \/ FinishTeardown
  \/ Reconnect
  \/ \E i \in Ids, k \in {"Zc", "NonZc"} : Issue(i, k)
  \/ \E i \in Ids : DeliverFirstCqeZc(i)
  \/ \E i \in Ids : DeliverSecondCqeZc(i)
  \/ \E i \in Ids : DeliverCqeNonZc(i)
  \/ \E i \in Ids : FailRequest(i)
  \/ \E i \in Ids : Recycle(i)
  \/ \E lc \in lateCqes : DeliverLate(lc)

(***************************************************************************
  FAIRNESS. To check that no request is left perpetually in flight we add
  weak fairness on the COMPLETION-DELIVERY actions only: the io_uring / kernel
  is assumed to eventually deliver the CQE(s) it owes for a submitted op
  (src/ring/core.rs:392-419, src/fabric/progress.rs:168-185). Concretely:

    * DeliverCqeNonZc(i)   - the single CQE of a non-zero-copy op,
    * DeliverFirstCqeZc(i) - the F_MORE send CQE of a SEND_ZC op,
    * DeliverSecondCqeZc(i)- the notification CQE that resolves a SEND_ZC op.

  While a request is Issued exactly one of these is enabled for it (NonZc with
  cqe=0; Zc with cqe=0 then cqe=1), so weak fairness drives every issued
  request to a Completed terminal along its delivery path.

  We DELIBERATELY leave the teardown / failure / connection-lifecycle actions
  UNFAIR: BeginTeardown, FinishTeardown, Reconnect, Bringup, FailRequest,
  Recycle, Issue and DeliverLate carry NO fairness. In particular FailRequest
  is NOT fair, so the liveness result does not come from forcibly failing
  every op during a teardown that may never be triggered; an op terminates
  because its completion was delivered (the normal path) OR, optionally,
  because a teardown that did happen failed it. Forcing teardown or failure
  would make completion hold for the wrong reason.
 ***************************************************************************)
Fairness ==
  /\ \A i \in Ids : WF_vars(DeliverCqeNonZc(i))
  /\ \A i \in Ids : WF_vars(DeliverFirstCqeZc(i))
  /\ \A i \in Ids : WF_vars(DeliverSecondCqeZc(i))

Spec == Init /\ [][Next]_vars /\ Fairness

(***************************************************************************
  COMPLETION LIVENESS (no request perpetually in flight). Every request that
  is Issued (outstanding in the registry) EVENTUALLY reaches a terminal
  state: either Completed (its completion CQE(s) arrived) or Failed (it was
  resolved during a connection teardown). This is the real liveness contract
  the fabric layer owes: an in-flight op never hangs forever.
 ***************************************************************************)
EventualCompletion ==
  \A i \in Ids : (status[i] = "Issued") ~> (status[i] \in {"Completed", "Failed"})

(***************************************************************************
  Invariants.
 ***************************************************************************)

\* Type-correctness / domain sanity for every variable.
TypeOK ==
  /\ conn \in ConnState
  /\ status \in [Ids -> Status]
  /\ kind \in [Ids -> Kind]
  /\ cqe \in [Ids -> 0..2]
  /\ replySet \in [Ids -> BOOLEAN]
  /\ bufPinned \in [Ids -> BOOLEAN]
  /\ inflight \in 0..MaxInflight
  /\ issued \in 0..MaxOps
  /\ ringInflight \in 0..(MaxInflight + MaxOps)
  /\ \A i \in Ids : gen[i] \in Nat
  /\ \A lc \in lateCqes : lc.id \in Ids /\ lc.gen \in Nat

(***************************************************************************
  BOUNDED IN-FLIGHT (back-pressure cap + counter integrity).

  The live counter never exceeds the cap, and it equals the true number of
  outstanding ops at all times.  The second conjunct is the one that
  catches a registry counter that has drifted from reality (a leaked or
  double-released slot), the failure `allocate`'s reserve-then-give-back
  dance and `Drop`'s single decrement exist to prevent
  (src/fabric/completion.rs:233-238, :175-182).
 ***************************************************************************)
BoundedInflight ==
  /\ inflight <= MaxInflight
  /\ inflight = Cardinality(Outstanding)

(***************************************************************************
  RING / LIBFABRIC COUNTER CORRELATION.

  The two back-pressure counters track different lifetimes of the same op:
  the libfabric `live` counter (`inflight`) drops as soon as the op is
  resolved, while the io_uring `submitted` counter (`ringInflight`) only
  drops when the kernel's last CQE for that op is consumed.  They differ by
  exactly the SEND_ZC notification CQEs the kernel still owes for ops that
  were failed mid-teardown, i.e. the size of `lateCqes`.  This invariant is
  what catches a ring slot leaked or double-drained relative to the
  libfabric registry (src/ring/core.rs:73-75 vs src/fabric/completion.rs:207).
 ***************************************************************************)
RingCorrelation ==
  ringInflight = inflight + Cardinality(lateCqes)

(***************************************************************************
  STALE-CQE GENERATION DISAMBIGUATION.

  A notification CQE still owed for a slot that was already failed and
  recycled must never be mistaken for the live op now occupying that slot
  identity.  Because recycling always bumps the generation before the slot
  can be reissued, every pending late CQE carries a generation strictly
  below the slot's current one, so no live (Issued) op ever shares a
  (slot, generation) pair with a pending stale CQE.  This is the model's
  statement that generation tagging in the abstract id space makes a late
  completion detectable as stale rather than delivered to the wrong op.
 ***************************************************************************)
GenerationDetectable ==
  \A lc \in lateCqes : ~(status[lc.id] = "Issued" /\ gen[lc.id] = lc.gen)

(***************************************************************************
  EXACTLY-ONCE COMPLETION (no stray / duplicate / premature completion).

  - A reply-slot is only ever set for an id that was actually issued
    (never for an Unused id): no stray completion.
  - Once set, the reply-slot implies at least one CQE was delivered, and
    the CQE count never exceeds the two a SEND_ZC owes (or the one a
    single-CQE op owes): no duplicate completion.  The exactly-once
    set is enforced structurally by the `replySet = FALSE` / `cqe = 0`
    guards on the deliver actions.
  - A single-CQE (NonZc) op is Completed iff its one CQE has fired and its
    reply-slot is set.
 ***************************************************************************)
ExactlyOnceCompletion ==
  /\ \A i \in Ids : replySet[i] => status[i] # "Unused"
  /\ \A i \in Ids : replySet[i] => cqe[i] >= 1
  /\ \A i \in Ids : status[i] = "Unused" =>
        (cqe[i] = 0 /\ replySet[i] = FALSE /\ bufPinned[i] = FALSE)
  /\ \A i \in Ids : (kind[i] = "NonZc" /\ status[i] = "Completed") =>
        (cqe[i] = 1 /\ replySet[i] = TRUE)

(***************************************************************************
  SEND_ZC BUFFER SAFETY.

  - Any outstanding op keeps its buffer pinned: it is never released while
    still in flight (covers both Zc and NonZc; the only releases happen at
    the resolving CQE or a teardown failure).
  - A SEND_ZC op may only reach Completed after BOTH CQEs are observed,
    and a completed op's buffer is unpinned: the buffer is released only
    after the second (notification) CQE.
  - Equivalently, no SEND_ZC op is ever in the forbidden state "buffer
    freed but the notification CQE (cqe = 2) not yet seen" UNLESS it was
    forcibly failed during teardown.  This is the use-after-free guard
    from src/ring/core.rs:392-419 and src/ring/network.rs:796-802.
 ***************************************************************************)
SendZcBufferSafety ==
  /\ \A i \in Ids : status[i] = "Issued" => bufPinned[i] = TRUE
  /\ \A i \in Ids : (kind[i] = "Zc" /\ status[i] = "Completed") =>
        (cqe[i] = 2 /\ bufPinned[i] = FALSE)
  /\ \A i \in Ids :
        (kind[i] = "Zc" /\ bufPinned[i] = FALSE /\ status[i] # "Unused") =>
           (cqe[i] = 2 \/ status[i] = "Failed")

(***************************************************************************
  NO COMPLETION ON A DOWN CONNECTION.

  Once Down there is nothing outstanding and the live counter is zero, so
  no completion CQE can be delivered for any id (the deliver actions all
  require status = "Issued").  Every request that was ever issued has by
  then reached a terminal state, resolved either as a real Completed or as
  a teardown Failed; none is left silently stranded.
 ***************************************************************************)
NoCompletionOnDown ==
  conn = "Down" =>
    /\ Outstanding = {}
    /\ inflight = 0
    /\ \A i \in Ids : status[i] \in {"Unused", "Completed", "Failed"}

(***************************************************************************
  State-space bound.  `inflight` can never exceed |Ids| (at most |Ids| ids
  are Issued at once) and is a sound no-op safety net.  `issued` and the
  per-slot generations are the genuinely unbounded counters introduced by
  slot reuse; capping them at MaxOps keeps the run finite (a slot can be
  recycled at most once per issued op, so gen[i] <= issued <= MaxOps).
 ***************************************************************************)
StateConstraint ==
  /\ inflight <= Cardinality(Ids)
  /\ issued <= MaxOps
  /\ \A i \in Ids : gen[i] <= MaxOps

(***************************************************************************
  Symmetry reduction.  All per-id treatment is uniform and no operator
  does a CHOOSE over Ids, so collapsing states that differ only by a
  permutation of request ids is sound.  NOTE: this operator is deliberately
  NOT referenced by the committed .cfg, because TLC warns that symmetry
  reduction during LIVENESS checking can miss temporal violations;
  EventualCompletion is therefore checked over the full state space.  It is
  retained for safety-only experimentation.
 ***************************************************************************)
Symmetry == Permutations(Ids)

=============================================================================
