------------------------- MODULE CowBtreeCrash -------------------------
(***************************************************************************
  CoW B+tree crash-consistency model for unbounded-storage.

  Specifies the on-disk metadata layer under the no-WAL / no-fsync regime:
  dual meta-page toggle, per-btree-page checksum, per-data-page checksum,
  leaf-scan recovery.

  The single invariant we defend:

      If the cache reports a hit for key K and returns bytes B, then B is
      bytes that some prior successful Fault committed for K.

  Only leaf B+tree pages are modeled: internal pages share the same
  per-page checksum argument and add no states the invariant can
  distinguish.
 ***************************************************************************)

EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
  Key,        \* set of keys
  Data,       \* set of data values
  LBA,        \* set of logical block addresses
  MetaSlots,  \* the two fixed LBAs that hold meta pages; |MetaSlots| = 2
  MaxTxn,     \* bound on next_txn to keep TLC finite
  NONE        \* opaque "absent" sentinel; declared as a TLC model value
              \* so it compares as inequal to any record, string, or other
              \* value without raising a typing error.

ASSUME MetaSlots \subseteq LBA
ASSUME MaxTxn \in Nat

LeafSlots == LBA \ MetaSlots

MetaPage(t, r, ok)  == [kind |-> "meta", txn |-> t, root |-> r, ok |-> ok]
LeafPage(t, e, ok)  == [kind |-> "leaf", txn |-> t, entries |-> e, ok |-> ok]

EmptyEntries == [k \in Key |-> NONE]

VARIABLES
  truth,        \* Key -> SUBSET Data.  History of every value ever
                \* committed by a successful Fault for each key.  The
                \* invariant checks "Hit(d) for k => d \in truth[k]". This
                \* flags returning bytes never written, returning bytes
                \* committed under a different key, and returning bytes
                \* synthesized from a torn page; it intentionally tolerates
                \* recovery falling back to a still-durable older version of
                \* K under the no-fsync persistence model.
  durable,      \* LBA -> Page \cup {NONE}.  What is on the platter and
                \* survives a crash.  NONE means the slot has never been
                \* written.
  inflight,     \* Set of [lba |-> LBA, page |-> Page].  Writes issued by
                \* a Fault but not yet promised durable; a crash decides
                \* each one's fate independently.
  next_txn,     \* Monotonic counter for meta-page txn ids.
  loaded_root   \* LBA \cup {NONE}.  The root the recovery procedure picked
                \* at last boot; NONE between Crash and Recover.

vars == <<truth, durable, inflight, next_txn, loaded_root>>

Init ==
  /\ truth = [k \in Key |-> {}]
  /\ durable = [l \in LBA |-> NONE]
  /\ inflight = {}
  /\ next_txn = 1
  /\ loaded_root = NONE

(***************************************************************************
  Helpers.
 ***************************************************************************)

IsValidLeaf(p) ==
  IF p = NONE THEN FALSE ELSE p.kind = "leaf" /\ p.ok = TRUE

IsValidMeta(p) ==
  IF p = NONE THEN FALSE ELSE p.kind = "meta" /\ p.ok = TRUE

CurrentEntries ==
  IF loaded_root = NONE
  THEN EmptyEntries
  ELSE IF IsValidLeaf(durable[loaded_root])
       THEN durable[loaded_root].entries
       ELSE EmptyEntries

InflightLBAs == { w.lba : w \in inflight }

\* Score used to identify the "older" meta slot during a Fault.  Invalid
\* slots score 0 so they are always chosen first.
MetaScore(l) == IF IsValidMeta(durable[l]) THEN durable[l].txn ELSE 0

OlderMetaSlot ==
  CHOOSE l \in MetaSlots :
    \A l2 \in MetaSlots : MetaScore(l) <= MetaScore(l2)

\* A torn write lands at its target LBA but with ok = FALSE; recovery and
\* reads must treat it as if the page did not exist.
TornPage(p) ==
  IF p.kind = "meta"
  THEN MetaPage(p.txn, p.root, FALSE)
  ELSE LeafPage(p.txn, p.entries, FALSE)

(***************************************************************************
  Actions.
 ***************************************************************************)

(***************************************************************************
  Fault(k, d): a successful backend fault commits key k with value d.
  Builds a new leaf page reflecting the post-state, plus a meta-page swap
  pointing at it, and queues both into inflight with no ordering
  constraint between them.

  The meta slot chosen is the one with the older durable txn so the other
  slot remains a valid fallback across the swap.
 ***************************************************************************)
Fault(k, d) ==
  /\ next_txn <= MaxTxn
  /\ \E L_leaf \in LeafSlots \ InflightLBAs :
       LET L_meta == OlderMetaSlot IN
       /\ L_meta \notin InflightLBAs
       /\ LET new_entries == [CurrentEntries EXCEPT ![k] = d]
              new_leaf    == LeafPage(next_txn, new_entries, TRUE)
              new_meta    == MetaPage(next_txn, L_leaf, TRUE)
          IN /\ inflight' = inflight \cup
                            { [lba |-> L_leaf, page |-> new_leaf],
                              [lba |-> L_meta, page |-> new_meta] }
             /\ truth' = [truth EXCEPT ![k] = @ \cup {d}]
             /\ next_txn' = next_txn + 1
             /\ UNCHANGED <<durable, loaded_root>>

(***************************************************************************
  FlushOne: the device opportunistically commits one pending write.
 ***************************************************************************)
FlushOne ==
  /\ inflight /= {}
  /\ \E w \in inflight :
       /\ durable' = [durable EXCEPT ![w.lba] = w.page]
       /\ inflight' = inflight \ {w}
  /\ UNCHANGED <<truth, next_txn, loaded_root>>

(***************************************************************************
  Crash: the NVMe adversary resolves every inflight write independently as
  clean, torn (ok = FALSE), or dropped.  Since Fault keeps inflight LBAs
  distinct, at most one inflight write targets any given LBA.
 ***************************************************************************)
Crash ==
  /\ inflight /= {}
  /\ \E dispose \in [inflight -> {"clean", "torn", "drop"}] :
       durable' =
         [l \in LBA |->
            LET applied == { w \in inflight :
                               w.lba = l /\ dispose[w] /= "drop" }
            IN IF applied = {}
               THEN durable[l]
               ELSE LET w == CHOOSE x \in applied : TRUE IN
                    IF dispose[w] = "clean" THEN w.page ELSE TornPage(w.page)]
  /\ inflight' = {}
  /\ loaded_root' = NONE
  /\ UNCHANGED <<truth, next_txn>>

(***************************************************************************
  Recover: the dual-meta loader.  Picks the meta slot with the largest txn
  whose page passes its self-checksum.  No subtree repair happens here;
  bad subtrees are handled lazily by Read.
 ***************************************************************************)
Recover ==
  /\ loaded_root = NONE
  /\ LET valid == { l \in MetaSlots : IsValidMeta(durable[l]) } IN
       /\ valid /= {}
       /\ LET best == CHOOSE l \in valid :
                        \A l2 \in valid : durable[l].txn >= durable[l2].txn
          IN loaded_root' = durable[best].root
  /\ UNCHANGED <<truth, durable, inflight, next_txn>>

(***************************************************************************
  Rebuild: LBA-order leaf scan, used when no meta slot is recoverable
  (both torn or never written).  Picks the leaf with the largest txn whose
  page passes its self-checksum and adopts it as the loaded root.

  The code in src/storage/btree/rebuild.rs merges all leaves sharing the
  highest txn into a fresh tree at txn+1.  Under the single-leaf
  abstraction there is only one leaf per txn cohort, so adopting it
  directly is equivalent: every entry in a valid leaf was placed there by
  some prior successful Fault and therefore lives in truth[k].
 ***************************************************************************)
Rebuild ==
  /\ loaded_root = NONE
  /\ \A l \in MetaSlots : ~IsValidMeta(durable[l])
  /\ LET valid == { l \in LeafSlots : IsValidLeaf(durable[l]) } IN
       /\ valid /= {}
       /\ LET best == CHOOSE l \in valid :
                        \A l2 \in valid : durable[l].txn >= durable[l2].txn
          IN loaded_root' = best
  /\ UNCHANGED <<truth, durable, inflight, next_txn>>

(***************************************************************************
  Read(k): pure function over durable and loaded_root.  Returns Hit(d) or
  Miss.  Missing / torn / out-of-range subtrees are silent misses.
 ***************************************************************************)
ReadResult(k) ==
  IF loaded_root = NONE
  THEN [kind |-> "miss"]
  ELSE LET leaf == durable[loaded_root] IN
       IF ~IsValidLeaf(leaf)
       THEN [kind |-> "miss"]
       ELSE IF leaf.entries[k] = NONE
            THEN [kind |-> "miss"]
            ELSE [kind |-> "hit", data |-> leaf.entries[k]]

(***************************************************************************
  The invariant.
 ***************************************************************************)
NeverReturnsWrongBytes ==
  \A k \in Key :
    LET r == ReadResult(k) IN
      r.kind = "hit" => r.data \in truth[k]

Next ==
  \/ \E k \in Key, d \in Data : Fault(k, d)
  \/ FlushOne
  \/ Crash
  \/ Recover
  \/ Rebuild

Spec == Init /\ [][Next]_vars

StateConstraint == next_txn <= MaxTxn + 1

(***************************************************************************
  Symmetry reduction.  TLC may collapse states that differ only by a
  permutation of any of these sets.  MetaSlots is intentionally NOT in
  the symmetry group: OlderMetaSlot and Recover both use CHOOSE over
  MetaSlots, and a non-symmetric tie-breaker would make the reduction
  unsound.
 ***************************************************************************)
Symmetry ==
  Permutations(Key) \cup Permutations(Data) \cup Permutations(LeafSlots)

=============================================================================
