------------------------- MODULE CowBtreeCrash -------------------------
(***************************************************************************
  CoW B+tree crash-consistency model for unbounded-storage.

  Specifies the on-disk metadata layer under the no-WAL / no-fsync regime:
  dual meta-page toggle, per-btree-page checksum, per-data-page checksum,
  leaf-scan recovery.

  DATA PAGES ARE MODELLED SEPARATELY FROM LEAF PAGES.  A leaf entry does
  not carry the value inline; it carries a reference to a data-page LBA
  plus the checksum of the bytes that entry expects there (LeafEntry ->
  (lba, data_checksum) in src/storage/btree, engine.rs:672).  A read
  follows meta -> leaf -> data LBA and only hits if the data page still
  present at that LBA carries bytes matching the entry's checksum.

  This separation is what makes the model faithful to the DURABLE-WRITE /
  EPHEMERAL-CACHE feature.  A durable request blocks the read until the
  data page AND the btree commit are on stable media; an ephemeral
  (best-effort) request lets the data page live only in the device's
  volatile cache.  Crucially, an ephemeral insert's meta commit can still
  become durable by being batched with a durable insert (the mutator ORs
  `durable` across a batch, mutator.rs / engine.rs process_batch), so the
  INDEX (leaf + meta) can outlive the DATA page it points at across a
  crash.  When that happens the reader must MISS, not return stale or
  foreign bytes; the per-data-page checksum is the only thing standing
  between "durable index -> lost volatile data page" and a wrong-bytes
  answer.

  The best-effort persistence of every write is already captured by the
  existing inflight / FlushOne / Crash nondeterminism: a write sits in the
  volatile inflight set until the device opportunistically flushes it, and
  a crash resolves each still-inflight write independently as clean, torn,
  or dropped.  That nondeterminism ranges over BOTH durability regimes at
  once - a "durable" data page is simply one the schedule flushed before
  the crash, an "ephemeral" one is a write the crash dropped - so the
  single safety invariant below is proven for every durability outcome
  without a separate durable/ephemeral flag.  The positive direction (a
  write acknowledged as durable is recoverable) is covered empirically by
  the DST invariant invariant_durable_survives_crash in
  tests/storage/tests.rs.

  The single invariant we defend:

      If the cache reports a hit for key K and returns bytes B, then B is
      bytes that some prior successful Fault committed for K.

  Only leaf B+tree pages are modelled at the index level: internal pages
  share the same per-page checksum argument and add no states the
  invariant can distinguish.
 ***************************************************************************)

EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
  Key,        \* set of keys
  Data,       \* set of data values (each value is its own checksum proxy:
              \* distinct values have distinct checksums, so comparing
              \* values models comparing the stored xxh3 checksum)
  LBA,        \* set of logical block addresses
  MetaSlots,  \* the two fixed LBAs that hold meta pages; |MetaSlots| = 2
  DataSlots,  \* LBAs that hold data pages, disjoint from MetaSlots. Leaf
              \* pages live in the remaining LBAs (LeafSlots below).
  MaxTxn,     \* bound on next_txn to keep TLC finite
  NONE        \* opaque "absent" sentinel; declared as a TLC model value
              \* so it compares as inequal to any record, string, or other
              \* value without raising a typing error.

ASSUME MetaSlots \subseteq LBA
ASSUME DataSlots \subseteq LBA
ASSUME MetaSlots \cap DataSlots = {}
ASSUME MaxTxn \in Nat

\* Leaf pages occupy whatever LBAs are neither meta nor data slots.
LeafSlots == LBA \ (MetaSlots \cup DataSlots)

MetaPage(t, r, ok)  == [kind |-> "meta", txn |-> t, root |-> r, ok |-> ok]
LeafPage(t, e, ok)  == [kind |-> "leaf", txn |-> t, entries |-> e, ok |-> ok]
DataPage(t, v, ok)  == [kind |-> "data", txn |-> t, val |-> v, ok |-> ok]

\* A leaf entry: a pointer to the data-page LBA plus the checksum (value)
\* the entry expects to find there.
LeafRef(l, v) == [lba |-> l, val |-> v]

EmptyEntries == [k \in Key |-> NONE]

VARIABLES
  truth,        \* Key -> SUBSET Data.  History of every value ever
                \* committed by a successful Fault for each key.  The
                \* invariant checks "Hit(d) for k => d \in truth[k]". This
                \* flags returning bytes never written, returning bytes
                \* committed under a different key, and returning bytes
                \* synthesized from a torn page; it intentionally tolerates
                \* recovery falling back to a still-durable older version of
                \* K under the no-fsync persistence model, and it tolerates
                \* an ephemeral data page being lost (that is a miss, not a
                \* wrong-bytes answer).
  durable,      \* LBA -> Page \cup {NONE}.  What is on the platter and
                \* survives a crash.  NONE means the slot has never been
                \* written.
  inflight,     \* Set of [lba |-> LBA, page |-> Page].  Writes issued by
                \* a Fault but not yet promised durable (they live in the
                \* device's volatile cache); a crash decides each one's
                \* fate independently.  Both index and data writes pass
                \* through here.
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

IsValidData(p) ==
  IF p = NONE THEN FALSE ELSE p.kind = "data" /\ p.ok = TRUE

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
  ELSE IF p.kind = "leaf"
       THEN LeafPage(p.txn, p.entries, FALSE)
       ELSE DataPage(p.txn, p.val, FALSE)

(***************************************************************************
  Actions.
 ***************************************************************************)

(***************************************************************************
  Fault(k, d): a successful backend fault commits key k with value d.
  Writes a fresh DATA page carrying d, a new leaf page whose entry for k
  points at that data LBA and records d as its checksum, plus a meta-page
  swap pointing at the new leaf.  All three writes queue into inflight
  (the volatile cache) with no ordering constraint between them.

  A fresh data LBA is chosen for the new value.  Reusing a data slot that
  still backs another live leaf entry is permitted (DataSlots \ Inflight
  may pick a slot holding an older durable data page); this is the
  LBA-reuse case the checksum must defend, and modelling it as an
  unconstrained adversary is a sound over-approximation of the refcounted
  allocator (EngineReclamation covers the allocator itself).

  The meta slot chosen is the one with the older durable txn so the other
  slot remains a valid fallback across the swap.
 ***************************************************************************)
Fault(k, d) ==
  /\ next_txn <= MaxTxn
  /\ \E L_leaf \in LeafSlots \ InflightLBAs :
       \E L_data \in DataSlots \ InflightLBAs :
         LET L_meta == OlderMetaSlot IN
         /\ L_meta \notin InflightLBAs
         /\ LET new_data    == DataPage(next_txn, d, TRUE)
                new_entries == [CurrentEntries EXCEPT ![k] = LeafRef(L_data, d)]
                new_leaf    == LeafPage(next_txn, new_entries, TRUE)
                new_meta    == MetaPage(next_txn, L_leaf, TRUE)
            IN /\ inflight' = inflight \cup
                              { [lba |-> L_data, page |-> new_data],
                                [lba |-> L_leaf, page |-> new_leaf],
                                [lba |-> L_meta, page |-> new_meta] }
               /\ truth' = [truth EXCEPT ![k] = @ \cup {d}]
               /\ next_txn' = next_txn + 1
               /\ UNCHANGED <<durable, loaded_root>>

(***************************************************************************
  FlushOne: the device opportunistically commits one pending write. This
  is the point at which a volatile-cache write becomes durable; a durable
  request is one whose data page and commit have all reached here before
  any crash, an ephemeral one may still be pending when the crash hits.
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
  distinct, at most one inflight write targets any given LBA.  This is
  where an ephemeral data page is dropped while its (already-flushed)
  durable leaf and meta survive.
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
  bad subtrees and lost data pages are handled lazily by Read.
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
  some prior successful Fault and therefore its checksum value lives in
  truth[k].  A rebuilt entry may still point at a data page the crash
  dropped; Read turns that into a miss via the checksum.
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
  Miss.  Follows meta -> leaf -> data LBA.  A missing / torn / out-of-range
  subtree is a silent miss; so is a data page whose checksum does not match
  the leaf entry (a torn data page, a dropped ephemeral data page, or an
  LBA since reused for a different value).
 ***************************************************************************)
ReadResult(k) ==
  IF loaded_root = NONE
  THEN [kind |-> "miss"]
  ELSE LET leaf == durable[loaded_root] IN
       IF ~IsValidLeaf(leaf)
       THEN [kind |-> "miss"]
       ELSE LET ref == leaf.entries[k] IN
            IF ref = NONE
            THEN [kind |-> "miss"]
            ELSE LET dp == durable[ref.lba] IN
                 IF IsValidData(dp) /\ dp.val = ref.val
                 THEN [kind |-> "hit", data |-> dp.val]
                 ELSE [kind |-> "miss"]

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
  unsound.  LeafSlots and DataSlots are chosen only via existential
  quantification, so permuting them maps behaviours to behaviours.
 ***************************************************************************)
Symmetry ==
  Permutations(Key) \cup Permutations(Data)
    \cup Permutations(LeafSlots) \cup Permutations(DataSlots)

=============================================================================
