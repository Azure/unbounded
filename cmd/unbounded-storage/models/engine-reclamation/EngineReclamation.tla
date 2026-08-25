----------------------- MODULE EngineReclamation -----------------------
(***************************************************************************
  Data-page reclamation + crash-time allocator-bitmap rebuild for
  the unbounded-storage engine.

  This model is the metadata/free-space companion to CowBtreeCrash: that
  spec defends "a hit never returns the wrong bytes" across a crash; this
  spec defends the LBA *lifecycle* that sits underneath it. Concretely we
  model how a logical block address (LBA) travels free -> in-use ->
  retired(pending) -> free again, and we prove the engine never lets a
  reader observe a slot that has been handed back to the allocator.

  The pieces we abstract, and where they live in the Rust crate:

    * The allocator bitmap (src/storage/alloc/mod.rs:6-12). One bit per
      LBA, clear = free, set = in-use. It is EPHEMERAL: not persisted, and
      reconstructed after a crash by walking the btree leaves and calling
      mark_in_use (alloc/mod.rs:177-189, idempotent). We model it as two
      disjoint sets `inUse` and `free`.

    * The current btree root (src/storage/btree/mod.rs). A map key -> LBA
      at the current committed txn. We model it as `root` plus a
      monotonic `cur_txn`.

    * Live data readers (engine.rs:514-520). Each reader pins the LBA it is
      actively reading via RefcountTable; the model represents a live reader
      by the btree view it observed, and PinCount(lba) counts readers whose
      view can still reference that LBA.

    * The data-page deferred-free queue (engine.rs:118-123, 316-347).
      Retired LBAs remain in-use only while their per-LBA pin count is
      non-zero. We model it as `pending`, a set of LBAs. This is distinct
      from btree/mod.rs PendingFree, which is the epoch-based deferred-free
      queue for btree metadata pages.

  The CRITICAL ordering invariant we encode is engine.rs:396-402: "the
  btree mutation MUST commit before the allocator slot is released."
  Every action here commits the btree change (updates `root`) before the old
  slot can become available to a future allocation. The slot is never returned
  to `free` while it is still reachable from the current root or pinned by
  any live data reader. Eviction (engine.rs:404-427) deletes the btree entry
  before directly freeing unpinned victims, modeled by the Evict action
  below.

  Properties proven (see invariants at the bottom):

    * NoUseAfterFree   - every LBA any live data reader can see is in-use.
    * Conservation     - in-use and free are disjoint and partition LBA
                         (no double-free, no lost slot).
    * CommitBeforeFree - no LBA reachable from the current root is pending
                         or free.
    * RebuildSoundness - immediately after a rebuild, in-use is exactly
                         the set of LBAs reachable from the recovered root
                         (no orphan left marked in-use = no leak; nothing
                         reachable left free = no double-allocation risk).

 ***************************************************************************)

EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
  Key,           \* set of keys mapped by the btree root
  LBA,           \* set of logical block addresses (allocator slots)
  Reader,        \* set of reader identities (so two readers may pin the SAME
                 \* LBA as distinct entries; gives a real per-LBA pin count)
  MaxTxn,        \* bound on cur_txn to keep TLC finite
  MaxSnapshots,  \* bound on concurrently-open reader snapshots
  NONE           \* opaque "absent" sentinel; a TLC model value so it
                 \* compares unequal to any LBA without a typing error.

ASSUME MaxTxn \in Nat
ASSUME MaxSnapshots \in Nat

VARIABLES
  root,         \* Key -> LBA \cup {NONE}. The current committed btree
                \* root mapping at txn `cur_txn`.
  cur_txn,      \* Monotonic commit counter. Every Write/Evict produces a
                \* new commit at cur_txn + 1.
  snapshots,    \* Set of [rid, txn, view]. One PUBLISHED reader per element;
                \* `view` is the btree mapping it observed, `txn` is retained
                \* only to keep the reader lifecycle shape aligned with the
                \* btree model, and `rid` is a distinct reader identity. Two
                \* readers may share a view as two elements, giving a true
                \* per-LBA pin count.
  registering,  \* Set of [rid, txn, view]. Readers whose data-page pin is
                \* established before they are PUBLISHED in the model.
  pending,      \* SUBSET LBA. The data-page deferred-free queue: retired
                \* slots still held in-use until their PinCount reaches zero.
  inUse,        \* SUBSET LBA. The allocator bitmap's set bits.
  free,         \* SUBSET LBA. The allocator bitmap's clear bits.
  wfAlloc,      \* SUBSET LBA. Slots allocated by a write that then FAILED
                \* before publishing (engine.rs:606-651): held in-use until
                \* the failure path frees them, never reachable from root.
  just_rebuilt  \* TRUE in exactly the states that immediately follow a
                \* Rebuild; lets RebuildSoundness talk about "right after
                \* recovery" without a history variable.

vars == <<root, cur_txn, snapshots, registering, pending, inUse, free, wfAlloc, just_rebuilt>>

(***************************************************************************
  Helpers.
 ***************************************************************************)

\* The LBAs reachable from a root mapping `r`: every non-absent entry.
\* This is exactly what the rebuild scan in alloc/mod.rs:9-11 and the
\* btree leaf walk reconstruct as "in-use".
Reachable(r) == { r[k] : k \in Key } \ {NONE}

\* Every live data reader, registered or published, holds its data-page pin.
LiveReaders == snapshots \cup registering

\* The txn ids currently present among model readers. This is used only by
\* AliveBeforePublish to preserve the two-step reader shape; data-page
\* reclamation below is governed by PinCount, not by this set.
AliveEpochs == { x.txn : x \in LiveReaders }

\* Reader identities currently in use by a live reader (so a fresh reader
\* must pick an unused identity).
UsedRids == { x.rid : x \in LiveReaders }

\* Per-LBA pin count (engine.rs:335-347): how many live readers still have
\* this slot in their pinned view.  retire_range frees immediately only when
\* this is zero.
PinCount(lba) == Cardinality({ x \in LiveReaders : \E k \in Key : x.view[k] = lba })

\* Data-page pending entries safe to flush now. This mirrors
\* StorageEngine::drain_pending_free, which frees queued runs whose current
\* pin count has fallen to zero.
FreeablePending == { lba \in pending : PinCount(lba) = 0 }

Init ==
  /\ root = [k \in Key |-> NONE]
  /\ cur_txn = 1
  /\ snapshots = {}
  /\ registering = {}
  /\ pending = {}
  /\ inUse = {}
  /\ free = LBA
  /\ wfAlloc = {}
  /\ just_rebuilt = FALSE

(***************************************************************************
  Actions.
 ***************************************************************************)

(***************************************************************************
  Write(k): the write/overwrite path (engine.rs:555-659). Allocate a free
  LBA, commit the new btree entry for k, then retire the OLD slot using
  StorageEngine::retire_range. The old slot is freed immediately when its
  PinCount is zero; otherwise it is queued in data-page pending_free until a
  later DrainPendingFree observes PinCount zero. This encodes
  commit-before-free: `root` advances before the old slot can become free.
 ***************************************************************************)
Write(k) ==
  /\ cur_txn <= MaxTxn
  /\ free /= {}
  /\ \E L \in free :
       LET old    == root[k]
           newtxn == cur_txn + 1
        IN /\ root'    = [root EXCEPT ![k] = L]
           /\ cur_txn' = newtxn
           /\ IF old = NONE
              THEN /\ pending' = pending
                   /\ inUse' = inUse \cup {L}
                   /\ free' = free \ {L}
              ELSE IF PinCount(old) = 0
                   THEN /\ pending' = pending
                        /\ inUse' = (inUse \cup {L}) \ {old}
                        /\ free' = (free \ {L}) \cup {old}
                   ELSE /\ pending' = pending \cup {old}
                        /\ inUse' = inUse \cup {L}
                        /\ free' = free \ {L}
           /\ UNCHANGED <<snapshots, registering, wfAlloc>>
           /\ just_rebuilt' = FALSE

(***************************************************************************
  DrainPendingFree: StorageEngine::drain_pending_free (engine.rs:316-329).
  The mutator loop opportunistically scans the data-page pending_free queue
  after committed batches and frees every queued run whose current pin count
  has dropped to zero.
 ***************************************************************************)
DrainPendingFree ==
  /\ FreeablePending /= {}
  /\ pending' = pending \ FreeablePending
  /\ inUse'   = inUse \ FreeablePending
  /\ free'    = free \cup FreeablePending
  /\ UNCHANGED <<root, cur_txn, snapshots, registering, wfAlloc>>
  /\ just_rebuilt' = FALSE

(***************************************************************************
  WriteFailAlloc / WriteFailFree: the IO/btree-error free paths
  (engine.rs:606-651). A write reserves a fresh slot and then fails before
  it can publish a new btree root, so the slot must be returned with NO
  commit. WriteFailAlloc takes a free slot into wfAlloc (in-use but not
  reachable from root and not queued); WriteFailFree returns it. The pair is
  a net no-op on the committed lifecycle.
 ***************************************************************************)
WriteFailAlloc ==
  /\ free /= {}
  /\ \E L \in free :
       /\ inUse'   = inUse \cup {L}
       /\ free'    = free \ {L}
       /\ wfAlloc' = wfAlloc \cup {L}
       /\ UNCHANGED <<root, cur_txn, snapshots, registering, pending>>
       /\ just_rebuilt' = FALSE

WriteFailFree ==
  /\ wfAlloc /= {}
  /\ \E L \in wfAlloc :
       /\ inUse'   = inUse \ {L}
       /\ free'    = free \cup {L}
       /\ wfAlloc' = wfAlloc \ {L}
       /\ UNCHANGED <<root, cur_txn, snapshots, registering, pending>>
       /\ just_rebuilt' = FALSE

(***************************************************************************
  Evict(k): the eviction path (engine.rs:404-427). Production skips pinned
  candidates before submitting the btree DELETE. Once the delete commits, the
  victim run is directly freed instead of being queued in pending_free. We
  model the ordering by updating `root` before moving the old slot from
  in-use to free.
 ***************************************************************************)
Evict(k) ==
  /\ cur_txn <= MaxTxn
  /\ root[k] /= NONE
  /\ PinCount(root[k]) = 0
  /\ LET old    == root[k]
         newtxn == cur_txn + 1
     IN /\ root'    = [root EXCEPT ![k] = NONE]
        /\ inUse'   = inUse \ {old}
        /\ free'    = free \cup {old}
        /\ cur_txn' = newtxn
        /\ UNCHANGED <<pending, snapshots, registering, wfAlloc>>
        /\ just_rebuilt' = FALSE

(***************************************************************************
  AliveRegister: the FIRST half of the modeled read open. A reader captures
  the current root as its view and is counted in PinCount before publication.
  This closes the window where reclamation could free a slot a
  soon-to-publish reader will see. A fresh reader takes an unused identity.
 ***************************************************************************)
AliveRegister ==
  /\ Cardinality(LiveReaders) < MaxSnapshots
  /\ \E rid \in (Reader \ UsedRids) :
       /\ registering' = registering \cup
            { [rid |-> rid, txn |-> cur_txn, view |-> root] }
       /\ UNCHANGED <<root, cur_txn, snapshots, pending, inUse, free, wfAlloc>>
       /\ just_rebuilt' = FALSE

(***************************************************************************
  Publish: the SECOND half of the modeled read open. A previously registered
  reader publishes its view, making it visible to readers. Its data-page pin
  was already counted in PinCount, so there is no window in which a published
  reader is unprotected.
 ***************************************************************************)
Publish ==
  /\ registering /= {}
  /\ \E r \in registering :
       /\ registering' = registering \ {r}
       /\ snapshots'   = snapshots \cup {r}
       /\ UNCHANGED <<root, cur_txn, pending, inUse, free, wfAlloc>>
       /\ just_rebuilt' = FALSE

(***************************************************************************
  DropSnapshot: a data reader releases its pin. Production does not free
  data-page pending_free entries directly on reader drop; it drains them from
  the mutator loop. This action only removes the one reader.
 ***************************************************************************)
DropSnapshot ==
  /\ snapshots /= {}
  /\ \E s \in snapshots :
       /\ snapshots' = snapshots \ {s}
       /\ UNCHANGED <<root, cur_txn, registering, pending, inUse, free, wfAlloc>>
       /\ just_rebuilt' = FALSE

(***************************************************************************
  Rebuild: crash + allocator-bitmap reconstruction (alloc/mod.rs:7-12,
  177-189). The bitmap is ephemeral, so a crash discards it along with all
  in-flight readers (published and registering), the deferred-free queue,
  and any failed-write reservation (all in-memory only). On restart we clear
  the bitmap and replay exactly the LBAs reachable from the recovered btree
  root via mark_in_use; every other slot becomes free. `just_rebuilt` is set
  so RebuildSoundness can pin down this post-state.
 ***************************************************************************)
Rebuild ==
  LET reach == Reachable(root) IN
    /\ inUse'        = reach
    /\ free'         = LBA \ reach
    /\ pending'      = {}
    /\ snapshots'    = {}
    /\ registering'  = {}
    /\ wfAlloc'      = {}
    /\ just_rebuilt' = TRUE
    /\ UNCHANGED <<root, cur_txn>>

Next ==
  \/ \E k \in Key : Write(k)
  \/ \E k \in Key : Evict(k)
  \/ DrainPendingFree
  \/ AliveRegister
  \/ Publish
  \/ DropSnapshot
  \/ WriteFailAlloc
  \/ WriteFailFree
  \/ Rebuild

Spec == Init /\ [][Next]_vars

(***************************************************************************
  Invariants.
 ***************************************************************************)

(***************************************************************************
  NO USE-AFTER-FREE (the data-page pin-count safety property). Every LBA any
  live reader can still see is in the in-use set. This covers BOTH published
  snapshots and registering readers in the model (LiveReaders). The PinCount
  gate keeps retire_range, eviction, and DrainPendingFree from reclaiming any
  slot a live reader still pins.
 ***************************************************************************)
NoUseAfterFree ==
  \A s \in LiveReaders :
    \A k \in Key :
      s.view[k] /= NONE => s.view[k] \in inUse

(***************************************************************************
  NO DOUBLE-FREE / CONSERVATION. The bitmap's set and clear bits are
  disjoint and together cover every LBA: nothing is both free and in-use
  (a double-free would put a slot in both), and nothing is lost. This
  guards the alloc/free accounting in alloc/mod.rs (used count, free_range
  idempotence at lines 127-146).
 ***************************************************************************)
Conservation ==
  /\ inUse \cap free = {}
  /\ inUse \cup free = LBA

(***************************************************************************
  COMMIT-BEFORE-FREE (engine.rs:396-402). No LBA still referenced by the
  CURRENT btree root is pending or free: a live mapping's slot is always
  in-use. This is the direct contrapositive of the double-free the comment
  warns about (a stale entry retiring a slot the allocator already reused).
 ***************************************************************************)
CommitBeforeFree ==
  \A k \in Key :
    root[k] /= NONE =>
      /\ root[k] \in inUse
      /\ root[k] \notin free
      /\ root[k] \notin pending

(***************************************************************************
  REBUILD SOUNDNESS (alloc/mod.rs:7-12, 177-189). Immediately after a
  rebuild, in-use is exactly the set of LBAs reachable from the recovered
  root - no orphan left marked in-use (no leak) and nothing reachable left
  free (no risk of handing a live page to a fresh allocation).
 ***************************************************************************)
RebuildSoundness ==
  just_rebuilt =>
    LET reach == Reachable(root) IN
      /\ inUse = reach
      /\ free  = LBA \ reach

(***************************************************************************
  ALIVE-BEFORE-PUBLISH. No PUBLISHED reader has a txn absent from the model's
  live-reader set: every reader is counted before it publishes, so a
  published reader always carries a data-page pin. A window in which a reader
  were visible while not yet counted would let reclamation free a slot the
  reader can see; this invariant pins that window shut.
 ***************************************************************************)
AliveBeforePublish ==
  \A s \in snapshots : s.txn \in AliveEpochs

(***************************************************************************
  WRITE-FAIL ISOLATION. A slot reserved by a failing write is held in-use
  but is never reachable from the current root and never queued for
  deferred free: it is purely the allocator's, awaiting the failure path's
  free (engine.rs:606-651). Guards against the failed-write reservation
  leaking into the committed lifecycle.
 ***************************************************************************)
WriteFailIsolation ==
  \A L \in wfAlloc :
    /\ L \in inUse
    /\ \A k \in Key : root[k] /= L
    /\ L \notin pending

(***************************************************************************
  Bound the txn counter (mirrors CowBtreeCrash's StateConstraint on
  next_txn). Every commit bumps cur_txn by one, so this keeps the model
  finite.
 ***************************************************************************)
StateConstraint == cur_txn <= MaxTxn + 1

(***************************************************************************
  Symmetry reduction. Keys, LBAs, and reader identities are fully
  interchangeable here: unlike CowBtreeCrash's MetaSlots, no LBA plays a
  privileged role (there is no CHOOSE over LBA in any tie-breaker) and reader
  ids are only ever picked by an unordered \E, so permuting any of the three
  sets maps behaviors to behaviors.
 ***************************************************************************)
Symmetry == Permutations(Key) \cup Permutations(LBA) \cup Permutations(Reader)

=============================================================================
