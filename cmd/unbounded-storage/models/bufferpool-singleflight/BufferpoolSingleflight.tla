--------------------- MODULE BufferpoolSingleflight ---------------------
(***************************************************************************
  Bufferpool single-flight + free-list liveness model for unbounded-storage.

  This is the concurrency companion to CowBtreeCrash (durability) and
  EngineReclamation (LBA lifecycle): those two defend SAFETY of the
  on-disk and allocator state; this one defends the LIVENESS of the
  in-memory page bufferpool, i.e. that the cooperative single-flight
  state machine plus the free-page allocator can never wedge into a state
  where some client still wants progress but nothing can move.

  -------------------------------------------------------------------------
  WHAT THE REAL CODE DOES (the parts we abstract)
  -------------------------------------------------------------------------
  A pool of fixed-size physical pages is handed out by a free list
  (src/bufferpool/free_list.rs): `free: Vec<u32>` of available page
  indices plus a FIFO `waiters: VecDeque<Waker>`. `alloc().await`
  (free_list.rs:48-84) pops a page or, if none is free, parks the
  caller's waker; `release(p)` (free_list.rs:52-62) pushes the page back
  and wakes the oldest waiter.

  Many clients may request the SAME logical page concurrently. To avoid
  N redundant backend reads, the pool runs a cooperative single-flight
  state machine per page slot (src/bufferpool/inflight.rs:69-81 and
  src/bufferpool/pool.rs:434-602). Each PageSlot has a SlotState:

      Idle  -> the first poller becomes the LEADER, flips the slot to
               Loading, allocates a page from the free list, and drives
               the I/O.                              (pool.rs:461-518)
      Loading(wakers) -> later pollers PARK as subscribers; their wakers
               are stored on the slot.               (pool.rs:482-488, 731-745)
      Ready -> the leader finished OK; subscribers read the bytes.
                                                      (pool.rs:578-584)
      Error -> the leader hit a fault; subscribers observe the error and
               return it (they do NOT re-lead).       (pool.rs:548-564)

  Two RAII guards control cleanup ordering on the leader:
    * ConsumerHold (pool.rs:267-334) bumps `consumer_holds`; the page is
      returned to the free list (recycle_if_terminal, pool.rs:336-377)
      only once the slot is in a terminal state (Ready/Error) AND
      consumer_holds == 0 (inflight.rs:64-66).
    * LeaderGuard (pool.rs:611-631) fires if the leader future is
      CANCELLED (dropped) mid-load: it resets the slot to Idle and wakes
      the parked subscribers so one of them takes over leadership.

  -------------------------------------------------------------------------
  WAKE / RELEASE ORDERING
  -------------------------------------------------------------------------
  When a leader is cancelled after it acquired a page, parked subscribers
  are woken so one of them can retry leadership. The model keeps the page
  lifecycle explicit at that handoff: the page is either preserved on the
  slot for the next leader, or it is returned to the free list in the same
  step that wakes subscribers. A wake-before-release ordering is modelled
  separately with `ReleaseBeforeWake = FALSE`; it detaches the page into an
  orphan bucket whose return is delayed until the slot becomes terminal.

  -------------------------------------------------------------------------
  PROPERTIES PROVEN
  -------------------------------------------------------------------------
    * SingleFlight     - at most one in-flight leader per page key.
    * LeaderConsistency- a slot is Loading iff it has exactly one leader,
                         and that leader is registered in `ldr`.
    * PageConservation - free + slot-owned + orphaned pages always sum to
                         the fixed pool size, and no page is simultaneously
                         reusable-on-a-slot and orphaned (no double-free,
                         no lost page).
    * DEADLOCK-FREEDOM - encoded by leaving TLC's deadlock detection ON
                         (CHECK_DEADLOCK TRUE) while modelling genuine task
                         completion: the only zero-successor states are
                         "every task done" states, which carry an explicit
                         Done stutter self-loop so they are NOT reported as
                         deadlocks. Any OTHER wedged state (a task still
                         wanting progress with nothing enabled) surfaces as
                         a TLC deadlock error. Under ReleaseBeforeWake=TRUE
                         no such state is reachable.
    * FIFO WAITER LIVENESS (WaiterLiveness, a temporal PROPERTY) - every task
                         that parks in free.alloc() (state "needpg", enqueued
                         on the FIFO waitQ) is EVENTUALLY served a stripe; no
                         waiter starves. Checked with weak fairness (WF_vars)
                         on the progress actions only - faults and cancels stay
                         unfair - so the result reflects real liveness, not
                         over-fairness. This is strictly stronger than the
                         deadlock-freedom above. NOTE: it is checked under the
                         `cancels <= MaxCancels` StateConstraint, so it is a
                         BOUNDED-cancellation guarantee (<= MaxCancels cancels),
                         not the unbounded case; see the StateConstraint
                         soundness note for why the bound leaves the property
                         faithfully exercised up to that limit.

 ***************************************************************************)

EXTENDS Naturals, FiniteSets, Sequences, TLC

CONSTANTS
  Tasks,             \* set of client tasks (each fetches one page key)
  Keys,              \* set of logical page keys contended over
  P,                 \* number of physical pages in the pool
  StripePages,       \* model allocation unit for one logical key. Bounded by P.
  MaxCancels,        \* bound on total leader cancellations (keeps TLC finite)
  ReleaseBeforeWake, \* TRUE = release/preserve before wake; FALSE = wake first
  PreserveOnCancel,  \* TRUE = a cancelled leader preserves its page index on
                     \* the slot for the next leader to reuse (pool.rs:621-624).
  NONE               \* opaque "absent" sentinel; a TLC model value so it
                     \* compares unequal to any Task without a typing error.

ASSUME P \in Nat
ASSUME StripePages \in Nat /\ StripePages >= 1 /\ StripePages <= P
ASSUME MaxCancels \in Nat
ASSUME ReleaseBeforeWake \in BOOLEAN
ASSUME PreserveOnCancel \in BOOLEAN

VARIABLES
  st,          \* Tasks -> task status. The client's position in
               \* pool::fetch_page (pool.rs:454-601):
               \*   "init"   not yet started (no key chosen)
               \*   "want"   has a key, about to inspect the slot
               \*   "lead"   became leader (slot Loading), needs a page
               \*   "needpg" leader parked in free.alloc().await   (pool.rs:516)
               \*   "io"     leader has a page, driving the I/O
               \*   "park"   parked subscriber (Action::Park)       (pool.rs:482)
               \*   "read"   holding a transient ConsumerHold while copying
               \*            the bytes out of a Ready slot           (pool.rs:474-479)
               \*   "done"   finished: got bytes or got an error (terminal-good)
  tkey,        \* Tasks -> Keys \cup {NONE}. The key a task is fetching;
               \* chosen once at Start, NONE until then.
  ss,          \* Keys -> SlotState abstraction: "Idle"/"Loading"/"Ready"/
               \* "Error" (inflight.rs:69-81).
  ldr,         \* Keys -> Tasks \cup {NONE}. The current single-flight leader
               \* of the slot, NONE when not Loading.
  slotPage,    \* Keys -> BOOLEAN. TRUE iff a physical page is currently
               \* attached to (owned by) this slot (PageSlot::page_idx is
               \* Some, inflight.rs:41).
  orphanPage,  \* Keys -> BOOLEAN. TRUE iff a page was DETACHED from this
               \* slot by a wake-before-release cancel but not yet returned to the
                \* free list. Always FALSE under ReleaseBeforeWake=TRUE. This
               \* bucket represents a page unavailable to both the slot and
               \* the free list.
  parked,      \* Keys -> SUBSET Tasks. The slot's parked subscriber set
               \* (the SlotState::Loading(Vec<Waker>) list, inflight.rs:78).
  freeCount,   \* number of pages currently in the free list's `free` Vec
               \* (free_list.rs:21).
  waitQ,       \* an ORDERED sequence of tasks parked in free.alloc().await
                \* (the FIFO `waiters: VecDeque<Waker>`, free_list.rs:23). A
                \* leader that finds fewer than StripePages free pages appends
                \* itself; release wakes the HEAD (free_list.rs:53-62). Modelled
                \* as a sequence so FIFO service order can be checked.
  holds,       \* Keys -> Nat. The slot's live consumer_holds refcount
               \* (inflight.rs:64-66, ConsumerHold pool.rs:267-334). A reader
               \* of a Ready slot takes a transient hold while it copies the
               \* bytes; RecycleSlot is blocked while holds[k] > 0.
  teePending,  \* Keys -> BOOLEAN. TRUE iff a blockstore tee write-back is
               \* still in flight for this Ready slot (tee_pending,
               \* inflight.rs:44-47; pool.rs:586-595). RecycleSlot is blocked
               \* while teePending[k] is TRUE so the page is never freed
               \* underneath an in-flight write.
  cancels      \* total cancellations so far (bound via MaxCancels).

vars == <<st, tkey, ss, ldr, slotPage, orphanPage, parked, freeCount, waitQ, holds, teePending, cancels>>

(***************************************************************************
  Helpers.
 ***************************************************************************)

\* The tasks currently acting as an in-flight loader for key k. Single-
\* flight means this set never exceeds one element (pool.rs:461-464: only
\* an Idle slot can be claimed, and claiming flips it to Loading).
TasksLeading(k) == { t \in Tasks : tkey[t] = k /\ st[t] \in {"lead", "io", "needpg"} }

\* Pages physically attached to slots, and pages detached-but-not-freed.
OwnedPages   == { k \in Keys : slotPage[k] }
OrphanedPages == { k \in Keys : orphanPage[k] }

\* A slot is terminal once the leader has resolved it Ready or Error
\* (slot_is_terminal, pool.rs:420-422).
IsTerminal(k) == ss[k] \in {"Ready", "Error"}

\* Every client has reached its terminal-good state. This is the ONLY
\* intended no-further-work condition; the Done stutter below keeps such
\* states from registering as TLC deadlocks.
Finished == \A t \in Tasks : st[t] = "done"

Init ==
  /\ st = [t \in Tasks |-> "init"]
  /\ tkey = [t \in Tasks |-> NONE]
  /\ ss = [k \in Keys |-> "Idle"]
  /\ ldr = [k \in Keys |-> NONE]
  /\ slotPage = [k \in Keys |-> FALSE]
  /\ orphanPage = [k \in Keys |-> FALSE]
  /\ parked = [k \in Keys |-> {}]
  /\ freeCount = P
  /\ waitQ = <<>>
  /\ holds = [k \in Keys |-> 0]
  /\ teePending = [k \in Keys |-> FALSE]
  /\ cancels = 0

(***************************************************************************
  Actions.
 ***************************************************************************)

(***************************************************************************
  Start(t): a client begins and picks the page key it will fetch. In the
  real pool the key is a function argument of fetch_page (pool.rs:434-439);
  here a task nondeterministically targets some key so TLC explores every
  contention pattern.
 ***************************************************************************)
Start(t) ==
  /\ st[t] = "init"
  /\ \E k \in Keys :
       /\ tkey' = [tkey EXCEPT ![t] = k]
       /\ st' = [st EXCEPT ![t] = "want"]
  /\ UNCHANGED <<ss, ldr, slotPage, orphanPage, parked, freeCount, waitQ, holds, teePending, cancels>>

(***************************************************************************
  BecomeLeader(t): the slot is Idle, so this poller wins leadership and
  flips it to Loading (pool.rs:461-464). Exactly one task can do this per
  Idle->Loading transition, which is the source of single-flight.
 ***************************************************************************)
BecomeLeader(t) ==
  /\ st[t] = "want"
  /\ LET k == tkey[t] IN
       /\ ss[k] = "Idle"
       /\ ss' = [ss EXCEPT ![k] = "Loading"]
       /\ ldr' = [ldr EXCEPT ![k] = t]
       /\ st' = [st EXCEPT ![t] = "lead"]
  /\ UNCHANGED <<tkey, slotPage, orphanPage, parked, freeCount, waitQ, holds, teePending, cancels>>

(***************************************************************************
  LeaderAcquire(t): the leader runs `free.alloc().await` (pool.rs:515-518).
  It obtains StripePages physical pages as one model allocation unit. If at
  least StripePages pages are free and no waiter is ahead of it on the model
  wait queue, it takes them and proceeds to I/O; otherwise it appends itself
  to the FIFO waiter queue (st -> "needpg", waitQ' = Append(waitQ, t)). The
  `waitQ = <<>>` guard on the fast path is the FIFO discipline checked by this
  model: a fresh allocation never jumps ahead of an already-parked waiter.

  Under PreserveOnCancel a previous leader may have left its stripe attached to
  this slot (slotPage[k] already TRUE). The new leader then REUSES that
  preserved stripe directly, skipping free.alloc() entirely (pool.rs:621-624,
  "preserve page_idx for reuse").
 ***************************************************************************)
LeaderAcquire(t) ==
  /\ st[t] = "lead"
  /\ LET k == tkey[t] IN
       \/ /\ slotPage[k] = TRUE
          /\ st' = [st EXCEPT ![t] = "io"]
          /\ UNCHANGED <<ss, ldr, tkey, slotPage, orphanPage, parked, freeCount, waitQ, holds, teePending, cancels>>
       \/ /\ slotPage[k] = FALSE
          /\ freeCount >= StripePages
          /\ waitQ = <<>>
          /\ freeCount' = freeCount - StripePages
          /\ slotPage' = [slotPage EXCEPT ![k] = TRUE]
          /\ st' = [st EXCEPT ![t] = "io"]
          /\ UNCHANGED <<ss, ldr, tkey, orphanPage, parked, waitQ, holds, teePending, cancels>>
       \/ /\ slotPage[k] = FALSE
          /\ (freeCount < StripePages \/ waitQ # <<>>)
          /\ st' = [st EXCEPT ![t] = "needpg"]
          /\ waitQ' = Append(waitQ, t)
          /\ UNCHANGED <<ss, ldr, tkey, slotPage, orphanPage, parked, freeCount, holds, teePending, cancels>>

(***************************************************************************
  NeedPgGrab(t): a leader parked in free.alloc() is woken once a stripe is
  released and takes it (free_list.rs:53-62 wakes the OLDEST waiter, which
  re-polls AllocFuture and pops its pages, free_list.rs:78-79). In the model,
  only the head of waitQ may grab the next stripe, and it must wait until a
  full StripePages worth of pages is free. This is the serve/wake action
  carried by the WaiterLiveness fairness assumption.
 ***************************************************************************)
NeedPgGrab(t) ==
  /\ st[t] = "needpg"
  /\ freeCount >= StripePages
  /\ waitQ # <<>>
  /\ Head(waitQ) = t
  /\ LET k == tkey[t] IN
       /\ freeCount' = freeCount - StripePages
       /\ slotPage' = [slotPage EXCEPT ![k] = TRUE]
       /\ st' = [st EXCEPT ![t] = "io"]
       /\ waitQ' = Tail(waitQ)
  /\ UNCHANGED <<ss, ldr, tkey, orphanPage, parked, holds, teePending, cancels>>

(***************************************************************************
  ParkSub(t): the slot is already Loading, so this poller parks as a
  subscriber (Action::Park, pool.rs:482-488; waker pushed onto the slot,
  pool.rs:731-745).
 ***************************************************************************)
ParkSub(t) ==
  /\ st[t] = "want"
  /\ LET k == tkey[t] IN
       /\ ss[k] = "Loading"
       /\ st' = [st EXCEPT ![t] = "park"]
        /\ parked' = [parked EXCEPT ![k] = @ \cup {t}]
  /\ UNCHANGED <<ss, ldr, tkey, slotPage, orphanPage, freeCount, waitQ, holds, teePending, cancels>>

(***************************************************************************
  ReadStart(t): the slot is Ready; a waiting or parked client takes a
  transient ConsumerHold (pool.rs:474-479, holds[k]++) and begins copying
  the bytes out. While the hold is live RecycleSlot cannot reclaim the slot
  (inflight.rs:64-66). The reader leaves the parked set, since it is now an
  active consumer rather than a waiter.
 ***************************************************************************)
ReadStart(t) ==
  /\ st[t] \in {"want", "park"}
  /\ LET k == tkey[t] IN
       /\ ss[k] = "Ready"
       /\ st' = [st EXCEPT ![t] = "read"]
       /\ holds' = [holds EXCEPT ![k] = @ + 1]
       /\ parked' = [parked EXCEPT ![k] = @ \ {t}]
  /\ UNCHANGED <<ss, ldr, tkey, slotPage, orphanPage, freeCount, waitQ, teePending, cancels>>

(***************************************************************************
  ReadDone(t): the reader finishes copying and drops its ConsumerHold
  (holds[k]--), reaching its terminal-good state. Dropping the last hold is
  what finally lets recycle_if_terminal reclaim the slot (pool.rs:336-377).
 ***************************************************************************)
ReadDone(t) ==
  /\ st[t] = "read"
  /\ LET k == tkey[t] IN
       /\ st' = [st EXCEPT ![t] = "done"]
       /\ holds' = [holds EXCEPT ![k] = @ - 1]
  /\ UNCHANGED <<ss, ldr, tkey, slotPage, orphanPage, parked, freeCount, waitQ, teePending, cancels>>

(***************************************************************************
  ReadError(t): the slot is Error; the client observes the error and
  returns it (pool.rs:481, pool.rs:498). An error is a legitimate terminal
  outcome, so the task is "done" - it does NOT re-lead. An error read takes
  no page hold (there is no page to copy).
 ***************************************************************************)
ReadError(t) ==
  /\ st[t] \in {"want", "park"}
  /\ LET k == tkey[t] IN
       /\ ss[k] = "Error"
       /\ st' = [st EXCEPT ![t] = "done"]
       /\ parked' = [parked EXCEPT ![k] = @ \ {t}]
  /\ UNCHANGED <<ss, ldr, tkey, slotPage, orphanPage, freeCount, waitQ, holds, teePending, cancels>>

(***************************************************************************
  LeaderOk(t): the leader's I/O succeeds. It marks the slot Ready and
  wakes all parked subscribers so they consume concurrently (pool.rs:578-
  584). Waking is modelled by moving parked subscribers back to "want" so
  they re-poll and hit ReadStart. The page stays attached until recycled,
  and a blockstore tee write-back is now pending for this Ready slot
  (teePending[k] := TRUE, pool.rs:586-595).
 ***************************************************************************)
LeaderOk(t) ==
  /\ st[t] = "io"
  /\ LET k == tkey[t] IN
       /\ ss' = [ss EXCEPT ![k] = "Ready"]
       /\ ldr' = [ldr EXCEPT ![k] = NONE]
       /\ st' = [u \in Tasks |-> IF u = t THEN "done"
                                 ELSE IF u \in parked[k] THEN "want"
                                 ELSE st[u]]
       /\ parked' = [parked EXCEPT ![k] = {}]
       /\ teePending' = [teePending EXCEPT ![k] = TRUE]
  /\ UNCHANGED <<tkey, slotPage, orphanPage, freeCount, waitQ, holds, cancels>>

(***************************************************************************
  LeaderFault(t): the leader's I/O fails. It marks the slot Error, takes the
  parked wakers, sets Error, then wakes them
  (pool.rs:548-563). Woken subscribers see Error and return it; none
  re-leads. The page is recycled later once the slot is unattached.
 ***************************************************************************)
LeaderFault(t) ==
  /\ st[t] = "io"
  /\ LET k == tkey[t] IN
       /\ ss' = [ss EXCEPT ![k] = "Error"]
       /\ ldr' = [ldr EXCEPT ![k] = NONE]
       /\ st' = [u \in Tasks |-> IF u = t THEN "done"
                                 ELSE IF u \in parked[k] THEN "want"
                                 ELSE st[u]]
       /\ parked' = [parked EXCEPT ![k] = {}]
  /\ UNCHANGED <<tkey, slotPage, orphanPage, freeCount, waitQ, holds, teePending, cancels>>

(***************************************************************************
  LeaderCancel(t): the leader future is dropped mid-load, firing
  LeaderGuard (pool.rs:611-631): reset the slot to Idle and wake the
  parked subscribers so one of them retries leadership. The interesting
  question is the ORDER of "give the page back" vs "wake the subscriber",
  controlled by ReleaseBeforeWake (see the top-of-file discussion and the
  drop-order comment at pool.rs:504-507).

  ReleaseBeforeWake = TRUE: the page is returned to the free
    list in the same atomic step that wakes the subscriber. A woken
    subscriber that re-leads will find a free page.

  ReleaseBeforeWake = FALSE: the wake happens but the page is only DETACHED
    into orphanPage; its return-to-free is deferred
    to DrainOrphan, which is gated on the slot being terminal. A woken
    subscriber re-leads and drives the slot back to Loading, so the orphan
    can never drain -> the retrying subscriber blocks in free.alloc()
    forever (tests.rs:331-382).

  We only model cancellation from the I/O phase, where the leader actually
  holds a page (slotPage[k] = TRUE); a cancel before acquiring a page
  releases nothing and is uninteresting for this property.

  PreserveOnCancel = TRUE (pool.rs:621-624): the cancelled leader leaves its
  page index ATTACHED to the slot (slotPage[k] stays TRUE)
  for the next leader to reuse directly, and resets the slot to Idle. The
  page is neither freed nor orphaned; it is recovered either by the next
  leader's LeaderAcquire fast path or by RecycleIdle. This supersedes the
  ReleaseBeforeWake handling below (which only applies when
  PreserveOnCancel = FALSE).
 ***************************************************************************)
LeaderCancel(t) ==
  /\ st[t] = "io"
  /\ cancels < MaxCancels
  /\ LET k == tkey[t] IN
       /\ ss' = [ss EXCEPT ![k] = "Idle"]
       /\ ldr' = [ldr EXCEPT ![k] = NONE]
       /\ st' = [u \in Tasks |-> IF u = t THEN "done"
                                 ELSE IF u \in parked[k] THEN "want"
                                 ELSE st[u]]
       /\ parked' = [parked EXCEPT ![k] = {}]
       /\ cancels' = cancels + 1
        /\ IF PreserveOnCancel
           THEN /\ UNCHANGED <<slotPage, freeCount, orphanPage>> \* stripe preserved on slot
           ELSE /\ slotPage' = [slotPage EXCEPT ![k] = FALSE]
                /\ IF ReleaseBeforeWake
                   THEN /\ freeCount' = freeCount + StripePages   \* release-before-wake
                        /\ UNCHANGED orphanPage
                   ELSE /\ orphanPage' = [orphanPage EXCEPT ![k] = TRUE]  \* wake-before-release
                        /\ UNCHANGED freeCount
  /\ UNCHANGED <<tkey, waitQ, holds, teePending>>

(***************************************************************************
  RecycleSlot(k): recycle_if_terminal (pool.rs:336-377). Once a slot is
  terminal (Ready/Error), nobody waits on it (no leader, no parked
  subscriber), no consumer hold is live (holds[k] = 0, inflight.rs:64-66),
  and no blockstore tee write-back is in flight (!teePending[k],
  pool.rs:586-595), its page returns to the free list and the slot is
  dropped back to Idle (the StripeFetch entry is removed, pool.rs:360-376).
 ***************************************************************************)
RecycleSlot(k) ==
  /\ IsTerminal(k)
  /\ ldr[k] = NONE
  /\ parked[k] = {}
  /\ holds[k] = 0
  /\ ~teePending[k]
  /\ slotPage[k] = TRUE
  /\ ss' = [ss EXCEPT ![k] = "Idle"]
  /\ slotPage' = [slotPage EXCEPT ![k] = FALSE]
  /\ freeCount' = freeCount + StripePages
  /\ UNCHANGED <<st, tkey, ldr, orphanPage, parked, waitQ, holds, teePending, cancels>>

(***************************************************************************
  TeeWrite(k): the blockstore tee write-back completes (pool.rs:586-595).
  Clearing teePending models the TeePendingGuard dropping. Per the
  production drop ordering (pool.rs:633-638) the tee-pending guard is
  dropped BEFORE the consumer hold, so this action is independent of
  holds[k] and may fire while a reader still holds the slot; only once both
  the tee write has cleared AND the last consumer hold has dropped can
  RecycleSlot reclaim the page. It is always enabled while teePending[k] is
  set, so it can never wedge.
 ***************************************************************************)
TeeWrite(k) ==
  /\ teePending[k] = TRUE
  /\ teePending' = [teePending EXCEPT ![k] = FALSE]
  /\ UNCHANGED <<st, tkey, ss, ldr, slotPage, orphanPage, parked, freeCount, waitQ, holds, cancels>>

(***************************************************************************
  RecycleIdle(k): return a PRESERVED page from an idle slot to the free
  list. Under PreserveOnCancel a cancelled leader leaves its page attached
  to an Idle slot for reuse; if no leader ever picks the slot back up, the
  preserved page must still be reclaimable so it is not stranded across
  keys. This models the slot's eventual drop returning its page_idx to the
  free list (the StripeFetch entry being removed while Idle). It only fires
  on an Idle slot that still owns a page and has no leader or waiters, so it
  cannot race a live single-flight.
 ***************************************************************************)
RecycleIdle(k) ==
  /\ ss[k] = "Idle"
  /\ ldr[k] = NONE
  /\ parked[k] = {}
  /\ slotPage[k] = TRUE
  /\ slotPage' = [slotPage EXCEPT ![k] = FALSE]
  /\ freeCount' = freeCount + StripePages
  /\ UNCHANGED <<st, tkey, ss, ldr, orphanPage, parked, waitQ, holds, teePending, cancels>>

(***************************************************************************
  DrainOrphan(k): the deferred return-to-free of a page detached by a
  wake-before-release cancel. In production this is the leader_hold ConsumerHold
  finally dropping and running recycle_if_terminal AFTER the wake
  (pool.rs:558-563); recycle only releases when the slot is terminal
  (pool.rs:352). Hence the terminal guard: if a woken subscriber has
  meanwhile re-led the slot (Loading), this is disabled and the page is
  stranded. Under ReleaseBeforeWake=TRUE (or
  PreserveOnCancel=TRUE) no orphan ever exists, so this action never fires.
 ***************************************************************************)
DrainOrphan(k) ==
  /\ orphanPage[k] = TRUE
  /\ IsTerminal(k)
  /\ orphanPage' = [orphanPage EXCEPT ![k] = FALSE]
  /\ freeCount' = freeCount + StripePages
  /\ UNCHANGED <<st, tkey, ss, ldr, slotPage, parked, waitQ, holds, teePending, cancels>>

(***************************************************************************
  Done: an explicit stutter self-loop enabled exactly when every task has
  finished. With CHECK_DEADLOCK TRUE this is what distinguishes the
  legitimate "all work complete" terminal (which has this successor and is
  therefore NOT a deadlock) from a genuine wedge (a task still wanting
  progress with no enabled action), which TLC reports as a deadlock.
 ***************************************************************************)
Done ==
  /\ Finished
  /\ UNCHANGED vars

Next ==
  \/ \E t \in Tasks : Start(t)
  \/ \E t \in Tasks : BecomeLeader(t)
  \/ \E t \in Tasks : LeaderAcquire(t)
  \/ \E t \in Tasks : NeedPgGrab(t)
  \/ \E t \in Tasks : ParkSub(t)
  \/ \E t \in Tasks : ReadStart(t)
  \/ \E t \in Tasks : ReadDone(t)
  \/ \E t \in Tasks : ReadError(t)
  \/ \E t \in Tasks : LeaderOk(t)
  \/ \E t \in Tasks : LeaderFault(t)
  \/ \E t \in Tasks : LeaderCancel(t)
  \/ \E k \in Keys : RecycleSlot(k)
  \/ \E k \in Keys : TeeWrite(k)
  \/ \E k \in Keys : RecycleIdle(k)
  \/ \E k \in Keys : DrainOrphan(k)
  \/ Done

(***************************************************************************
  FAIRNESS. To check the FIFO waiter-liveness property we need weak fairness
  on the PROGRESS actions: a continuously-enabled progress step must
  eventually fire. We deliberately do NOT make the optional/failure actions
  fair - LeaderFault, LeaderCancel and DrainOrphan stay UNFAIR so the
  liveness claim reflects production reality (a fault or a cancel is allowed
  but never forced) and so over-fairness cannot mask a genuine starvation.
  The set below is exactly the actions that drain a "needpg" waiter or that
  free a stripe back to the pool so a waiter can be served:
    * Start / BecomeLeader / LeaderAcquire / LeaderOk drive a leader to a
      terminal state that releases its stripe.
    * ReadStart / ReadDone / ReadError drain the consumers blocking recycle.
    * TeeWrite clears the tee guard blocking recycle.
    * RecycleSlot / RecycleIdle return a freed stripe to the free list.
    * NeedPgGrab is the serve/wake step itself.
 ***************************************************************************)
Fairness ==
  /\ \A t \in Tasks : WF_vars(Start(t))
  /\ \A t \in Tasks : WF_vars(BecomeLeader(t))
  /\ \A t \in Tasks : WF_vars(LeaderAcquire(t))
  /\ \A t \in Tasks : WF_vars(NeedPgGrab(t))
  /\ \A t \in Tasks : WF_vars(ReadStart(t))
  /\ \A t \in Tasks : WF_vars(ReadDone(t))
  /\ \A t \in Tasks : WF_vars(ReadError(t))
  /\ \A t \in Tasks : WF_vars(LeaderOk(t))
  /\ \A k \in Keys : WF_vars(TeeWrite(k))
  /\ \A k \in Keys : WF_vars(RecycleSlot(k))
  /\ \A k \in Keys : WF_vars(RecycleIdle(k))

Spec == Init /\ [][Next]_vars /\ Fairness

(***************************************************************************
  FIFO WAITER LIVENESS (no starvation on the free list). Every task that
  parks in free.alloc().await (state "needpg", enqueued on waitQ) eventually
  leaves that state, i.e. is served a stripe (free_list.rs:53-62). Under the
  committed fix (PreserveOnCancel=TRUE, ReleaseBeforeWake=TRUE) a freed stripe
  always reaches the HEAD of the FIFO queue and the WF_vars(NeedPgGrab) step
  fires, so no waiter starves. This is strictly stronger than the
  deadlock-freedom the safety run proves: it rules out the wait-forever case
  even when other actions could keep firing indefinitely.

  SCOPE: this property is model-checked under the `cancels <= MaxCancels`
  StateConstraint, so it is a BOUNDED-cancellation result - it holds for every
  interleaving with at most MaxCancels leader cancellations, not for the
  unbounded case. See the soundness note on StateConstraint below for why the
  constraint (a monotonic counter pruning only a finite suffix) leaves the
  property faithfully exercised up to that bound.
 ***************************************************************************)
WaiterLiveness == \A t \in Tasks : (st[t] = "needpg") ~> (st[t] # "needpg")

(***************************************************************************
  Invariants.
 ***************************************************************************)

(***************************************************************************
  SINGLE-FLIGHT (the safety property). At most one in-flight loader exists
  per key, because only an Idle slot can be claimed and claiming flips it
  to Loading (pool.rs:461-464). This is exactly the property the DST guards
  as `invariant_single_flight_per_page` (tests.rs).
 ***************************************************************************)
SingleFlight == \A k \in Keys : Cardinality(TasksLeading(k)) <= 1

(***************************************************************************
  LEADER CONSISTENCY. A slot is Loading exactly when it has a registered
  leader, and that leader is a task genuinely leading this key. Ties the
  `ldr` bookkeeping to the SlotState so the single-flight count above is
  not fooled by stale leader records.
 ***************************************************************************)
LeaderConsistency ==
  \A k \in Keys :
    /\ (ss[k] = "Loading") <=> (ldr[k] # NONE)
    /\ (ldr[k] # NONE) => /\ ldr[k] \in Tasks
                          /\ tkey[ldr[k]] = k
                          /\ st[ldr[k]] \in {"lead", "io", "needpg"}

(***************************************************************************
  PAGE CONSERVATION (no double-free, no lost page). Free pages plus pages
  attached to slots plus pages temporarily orphaned by a wake-before-release cancel
  always sum to the fixed pool size, and no single page is both attached
  and orphaned. This is the bufferpool analogue of EngineReclamation's
  Conservation and guards the free_list accounting (free_list.rs:21,
  53-62). A stranded page remains accounted for as an orphan, so the model
  distinguishes accounting safety from progress.
 ***************************************************************************)
PageConservation ==
  /\ freeCount + StripePages * Cardinality(OwnedPages)
       + StripePages * Cardinality(OrphanedPages) = P
  /\ \A k \in Keys : ~(slotPage[k] /\ orphanPage[k])
  /\ freeCount >= 0

(***************************************************************************
  WAIT-QUEUE CONSISTENCY. The FIFO free-list queue contains exactly the
  tasks currently parked in free.alloc() (state "needpg"), with no duplicate
  entry. Ties the waitQ bookkeeping to reality so the FIFO head-of-line
  service the WaiterLiveness property relies on is over a faithful queue
  (free_list.rs:23 `waiters: VecDeque<Waker>`).
 ***************************************************************************)
WaitQConsistent ==
  /\ { waitQ[i] : i \in DOMAIN waitQ } = { t \in Tasks : st[t] = "needpg" }
  /\ \A i \in DOMAIN waitQ : \A j \in DOMAIN waitQ : waitQ[i] = waitQ[j] => i = j

(***************************************************************************
  HOLDS CONSISTENCY. The per-slot consumer_holds refcount equals the number
  of tasks currently copying bytes out of that slot (in the "read" state),
  and is never negative. Ties the holds[k] bookkeeping to reality so the
  RecycleSlot gate (holds[k] = 0) genuinely means "no live PageGuard"
  (inflight.rs:64-66).
 ***************************************************************************)
HoldsConsistent ==
  \A k \in Keys :
    /\ holds[k] = Cardinality({ t \in Tasks : tkey[t] = k /\ st[t] = "read" })
    /\ holds[k] >= 0

(***************************************************************************
  TEE SAFETY (no recycle while a blockstore write is in flight). A slot with
  a pending tee write-back still owns its page: the page is never returned
  to the free list (and so never handed to a fresh allocation) while a
  blockstore write is reading from it. This is the use-after-free guard of
  pool.rs:586-595; RecycleSlot's !teePending gate enforces it, and this
  invariant pins down the resulting state predicate.
 ***************************************************************************)
TeeSafety ==
  \A k \in Keys : teePending[k] => slotPage[k]

(***************************************************************************
  Bound the cancellation counter so the state space is finite (mirrors the
  StateConstraint discipline in CowBtreeCrash / EngineReclamation).

  SOUNDNESS NOTE for the WaiterLiveness PROPERTY. This CONSTRAINT is active
  while WaiterLiveness (a temporal property) is checked, and TLC rightly
  warns that declaring a state constraint during liveness checking is
  dangerous (Specifying Systems sec 14.3.5): in general a constraint can
  prune successor states and hide a real non-progress cycle. Two facts make
  the bounded run meaningful here, but they DO NOT promote it to an
  unconditional proof:
    (1) `cancels` is monotonically non-decreasing - only LeaderCancel ever
        writes it, always as cancels + 1 - so `cancels <= MaxCancels` can
        only prune a FINITE SUFFIX of any behaviour (everything after the
        (MaxCancels+1)-th cancel). It never deletes an internal transition,
        so it cannot fabricate or sever a liveness cycle inside the explored
        prefix.
    (2) Consequently WaiterLiveness is genuinely exercised across every
        interleaving with AT MOST MaxCancels cancellations.
  The guarantee is therefore SCOPED to <= MaxCancels cancellations, not the
  unbounded-cancellation case. Raising MaxCancels widens the scope (the
  committed value is 3); it does not make the property unconditional.
 ***************************************************************************)
StateConstraint == cancels <= MaxCancels

(***************************************************************************
  Symmetry reduction. Tasks are fully interchangeable (no action breaks a
  tie over a specific task identity) and Keys are interchangeable (no
  CHOOSE over Keys with an order-dependent tie-break, unlike CowBtreeCrash's
  MetaSlots). Permuting either set maps behaviours to behaviours. NOTE: this
  operator is deliberately NOT referenced by the committed .cfg, because TLC
  warns that symmetry reduction during LIVENESS checking can miss temporal
  violations; WaiterLiveness is therefore checked WITHOUT symmetry reduction,
  i.e. over the full state graph that the `cancels <= MaxCancels` constraint
  admits. ("Full" here means un-symmetry-reduced, NOT unbounded: the cancel
  bound still scopes the explored space - see the StateConstraint note above.)
  It is retained for safety-only experimentation.
 ***************************************************************************)
Symmetry == Permutations(Tasks) \cup Permutations(Keys)

=============================================================================
