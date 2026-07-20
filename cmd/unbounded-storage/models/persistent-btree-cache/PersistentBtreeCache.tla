-------------------- MODULE PersistentBtreeCache --------------------
(***************************************************************************
  Persistent internal-node cache and metadata reclamation model.

  The production B+tree publishes an immutable (root LBA, cache root) pair.
  A path-copy commit rewrites one branch and the root, while preserving the
  untouched branch's disk LBA and Arc cache identity. Before publishing the
  candidate root, every reachable internal page is read and checksum
  validated from disk. A validation failure leaves the published snapshot
  unchanged. Old reader snapshots may retain the previous pair, so retired
  metadata cannot be reused until every older snapshot and in-progress
  commit drops it.

  Recovery publishes a snapshot only after decoding every reachable page, so
  every reachable internal has a cache node. This bounded model has two
  internal levels: a root and its left/right internal branches. Leaves and
  values are abstracted as the branch's generation because terminal-leaf
  checks remain covered by CowBtreeCrash.
 ************************************************************************ ***)

EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
  Meta, CacheNode, Reader, Side, MaxTxn, NONE,
  OldRoot, OldLeft, OldRight,
  OldCacheRoot, OldCacheLeft, OldCacheRight

ASSUME Side = {"left", "right"}
ASSUME MaxTxn \in Nat
ASSUME OldRoot \in Meta /\ OldLeft \in Meta /\ OldRight \in Meta
ASSUME Cardinality({OldRoot, OldLeft, OldRight}) = 3
ASSUME OldCacheRoot \in CacheNode
ASSUME OldCacheLeft \in CacheNode
ASSUME OldCacheRight \in CacheNode
ASSUME Cardinality({OldCacheRoot, OldCacheLeft, OldCacheRight}) = 3

DRoot(gen, left, right) ==
  [kind |-> "root", gen |-> gen, side |-> NONE,
   left |-> left, right |-> right]
DBranch(gen, side) ==
  [kind |-> "branch", gen |-> gen, side |-> side,
   left |-> NONE, right |-> NONE]
CRoot(gen, lba, left, right) ==
  [kind |-> "root", gen |-> gen, lba |-> lba,
   side |-> NONE, left |-> left, right |-> right]
CBranch(gen, lba, side) ==
  [kind |-> "branch", gen |-> gen, lba |-> lba,
   side |-> side, left |-> NONE, right |-> NONE]

SnapshotType == [txn : 1..(MaxTxn + 1), root : Meta, cache : CacheNode]
DiskNodeType ==
  [kind : {"root", "branch"}, gen : 1..(MaxTxn + 1),
   side : Side \cup {NONE}, left : Meta \cup {NONE}, right : Meta \cup {NONE}]
CacheNodeType ==
  [kind : {"root", "branch"}, gen : 1..(MaxTxn + 1), lba : Meta,
   side : Side \cup {NONE}, left : CacheNode \cup {NONE},
   right : CacheNode \cup {NONE}]
PendingType == [page : Meta, retire : 2..(MaxTxn + 1)]
AttemptType ==
  [parent : SnapshotType, side : Side, txn : 2..(MaxTxn + 1),
   root : Meta, branch : Meta, cacheRoot : CacheNode,
   cacheBranch : CacheNode, oldRoot : Meta, oldBranch : Meta]
FailureType ==
  [kind : {"write", "validation"}, published : SnapshotType,
   candidate : SnapshotType, root : Meta, branch : Meta]

VARIABLES
  disk, valid, cacheHeap, published, readers, attempt,
  pending, inUse, free, failureWitness, publicationWitness

vars == <<disk, valid, cacheHeap, published, readers, attempt,
          pending, inUse, free, failureWitness, publicationWitness>>

InitialDisk ==
  [m \in Meta |->
    IF m = OldRoot THEN DRoot(1, OldLeft, OldRight)
    ELSE IF m = OldLeft THEN DBranch(1, "left")
    ELSE IF m = OldRight THEN DBranch(1, "right")
    ELSE NONE]

InitialCache ==
  [c \in CacheNode |->
    IF c = OldCacheRoot
      THEN CRoot(1, OldRoot, OldCacheLeft, OldCacheRight)
    ELSE IF c = OldCacheLeft THEN CBranch(1, OldLeft, "left")
    ELSE IF c = OldCacheRight THEN CBranch(1, OldRight, "right")
    ELSE NONE]

Init ==
  /\ disk = InitialDisk
  /\ valid = {OldRoot, OldLeft, OldRight}
  /\ cacheHeap = InitialCache
  /\ published = [txn |-> 1, root |-> OldRoot, cache |-> OldCacheRoot]
  /\ readers = [r \in Reader |-> NONE]
  /\ attempt = NONE
  /\ pending = {}
  /\ inUse = {OldRoot, OldLeft, OldRight}
  /\ free = Meta \ {OldRoot, OldLeft, OldRight}
  /\ failureWitness = NONE
  /\ publicationWitness = NONE

LiveSnapshots ==
  {published}
    \cup {readers[r] : r \in {q \in Reader : readers[q] /= NONE}}
    \cup (IF attempt = NONE THEN {} ELSE {attempt.parent})

BranchLba(s, side) == IF side = "left" THEN disk[s.root].left ELSE disk[s.root].right
BranchCache(s, side) ==
  IF side = "left" THEN cacheHeap[s.cache].left ELSE cacheHeap[s.cache].right

NodeMatches(c, m) ==
  /\ cacheHeap[c] /= NONE
  /\ disk[m] /= NONE
  /\ cacheHeap[c].lba = m
  /\ cacheHeap[c].kind = disk[m].kind
  /\ cacheHeap[c].gen = disk[m].gen

SnapshotCoherent(s) ==
  /\ NodeMatches(s.cache, s.root)
  /\ disk[s.root].kind = "root"
  /\ cacheHeap[s.cache].kind = "root"
  /\ \A side \in Side :
       LET m == BranchLba(s, side)
           c == BranchCache(s, side)
        IN /\ NodeMatches(c, m)
           /\ disk[m].kind = "branch"
           /\ disk[m].side = side
           /\ cacheHeap[c].side = side

Reachable(s) == {s.root} \cup {BranchLba(s, side) : side \in Side}

Candidate ==
  [txn |-> attempt.txn, root |-> attempt.root, cache |-> attempt.cacheRoot]

CandidateValid == Reachable(Candidate) \subseteq valid

SafeToFree(p) == \A s \in LiveSnapshots : s.txn >= p.retire

BeginCommit(side) ==
  /\ attempt = NONE
  /\ published.txn < MaxTxn
  /\ Cardinality(free) >= 2
  /\ \E newRoot, newBranch \in free :
       \E cacheRoot, cacheBranch \in CacheNode :
         /\ newRoot /= newBranch
         /\ cacheRoot /= cacheBranch
         /\ cacheHeap[cacheRoot] = NONE
         /\ cacheHeap[cacheBranch] = NONE
         /\ LET txn == published.txn + 1
                oldBranch == BranchLba(published, side)
                untouchedSide == IF side = "left" THEN "right" ELSE "left"
                untouchedLba == BranchLba(published, untouchedSide)
                untouchedCache == BranchCache(published, untouchedSide)
                leftLba == IF side = "left" THEN newBranch ELSE untouchedLba
                rightLba == IF side = "right" THEN newBranch ELSE untouchedLba
                leftCache == IF side = "left" THEN cacheBranch ELSE untouchedCache
                rightCache == IF side = "right" THEN cacheBranch ELSE untouchedCache
            IN /\ disk' = [disk EXCEPT
                    ![newBranch] = DBranch(txn, side),
                    ![newRoot] = DRoot(txn, leftLba, rightLba)]
               /\ cacheHeap' = [cacheHeap EXCEPT
                    ![cacheBranch] = CBranch(txn, newBranch, side),
                    ![cacheRoot] = CRoot(txn, newRoot, leftCache, rightCache)]
               /\ attempt' =
                    [parent |-> published, side |-> side, txn |-> txn,
                     root |-> newRoot, branch |-> newBranch,
                     cacheRoot |-> cacheRoot, cacheBranch |-> cacheBranch,
                     oldRoot |-> published.root, oldBranch |-> oldBranch]
                /\ inUse' = inUse \cup {newRoot, newBranch}
                /\ free' = free \ {newRoot, newBranch}
                /\ valid' = valid \cup {newRoot, newBranch}
                /\ UNCHANGED <<published, readers, pending>>
                /\ failureWitness' = NONE
                /\ publicationWitness' = NONE

Publish ==
  /\ attempt /= NONE
  /\ CandidateValid
  /\ published' =
        [txn |-> attempt.txn, root |-> attempt.root, cache |-> attempt.cacheRoot]
  /\ pending' = pending \cup
       {[page |-> attempt.oldRoot, retire |-> attempt.txn],
        [page |-> attempt.oldBranch, retire |-> attempt.txn]}
  /\ attempt' = NONE
  /\ UNCHANGED <<disk, valid, cacheHeap, readers, inUse, free>>
  /\ failureWitness' = NONE
  /\ publicationWitness' = published'

FailCommit(kind) ==
  /\ attempt /= NONE
  /\ (kind = "write" \/ (kind = "validation" /\ ~CandidateValid))
  /\ inUse' = inUse \ {attempt.root, attempt.branch}
  /\ free' = free \cup {attempt.root, attempt.branch}
  /\ failureWitness' =
       [kind |-> kind, published |-> published,
        candidate |-> Candidate,
        root |-> attempt.root, branch |-> attempt.branch]
  /\ attempt' = NONE
  /\ UNCHANGED <<disk, valid, cacheHeap, published, readers, pending>>
  /\ publicationWitness' = NONE

CorruptUntouchedBranch ==
  /\ attempt /= NONE
  /\ LET other == IF attempt.side = "left" THEN "right" ELSE "left"
         page == BranchLba(attempt.parent, other)
      IN /\ page \in valid
         /\ valid' = valid \ {page}
  /\ UNCHANGED <<disk, cacheHeap, published, readers, attempt,
                  pending, inUse, free, failureWitness>>
  /\ publicationWitness' = NONE

Acquire(r) ==
  /\ readers[r] = NONE
  /\ readers' = [readers EXCEPT ![r] = published]
  /\ UNCHANGED <<disk, valid, cacheHeap, published, attempt, pending, inUse, free>>
  /\ failureWitness' = NONE
  /\ publicationWitness' = NONE

Release(r) ==
  /\ readers[r] /= NONE
  /\ readers' = [readers EXCEPT ![r] = NONE]
  /\ UNCHANGED <<disk, valid, cacheHeap, published, attempt, pending, inUse, free>>
  /\ failureWitness' = NONE
  /\ publicationWitness' = NONE

Reclaim ==
  /\ \E p \in pending :
       /\ SafeToFree(p)
       /\ pending' = pending \ {p}
       /\ inUse' = inUse \ {p.page}
       /\ free' = free \cup {p.page}
       /\ UNCHANGED <<disk, valid, cacheHeap, published, readers, attempt>>
       /\ failureWitness' = NONE
       /\ publicationWitness' = NONE

Next ==
  \/ \E side \in Side : BeginCommit(side)
  \/ Publish
  \/ \E kind \in {"write", "validation"} : FailCommit(kind)
  \/ CorruptUntouchedBranch
  \/ \E r \in Reader : Acquire(r)
  \/ \E r \in Reader : Release(r)
  \/ Reclaim

Spec == Init /\ [][Next]_vars

TypeOK ==
  /\ disk \in [Meta -> DiskNodeType \cup {NONE}]
  /\ valid \subseteq Meta
  /\ cacheHeap \in [CacheNode -> CacheNodeType \cup {NONE}]
  /\ published \in SnapshotType
  /\ readers \in [Reader -> SnapshotType \cup {NONE}]
  /\ attempt \in AttemptType \cup {NONE}
  /\ pending \subseteq PendingType
  /\ inUse \subseteq Meta
  /\ free \subseteq Meta
  /\ failureWitness \in FailureType \cup {NONE}
  /\ publicationWitness \in SnapshotType \cup {NONE}

SnapshotRootCacheCoherent == \A s \in LiveSnapshots : SnapshotCoherent(s)

ReachableInternalsCached ==
  \A s \in LiveSnapshots :
    /\ cacheHeap[s.cache] /= NONE
    /\ \A side \in Side : BranchCache(s, side) /= NONE

CachedLookupMatchesSnapshotView ==
  \A s \in LiveSnapshots : \A side \in Side :
    LET c == BranchCache(s, side)
        m == BranchLba(s, side)
     IN /\ cacheHeap[c].lba = m
        /\ cacheHeap[c].gen = disk[m].gen

LiveMetadataNeverFree ==
  \A s \in LiveSnapshots : Reachable(s) \subseteq inUse

NoRetiredPageReachableFromNewRoot ==
  \A p \in pending : p.page \notin Reachable(published)

FreshNodesMatchEncodedPages ==
  attempt /= NONE =>
    /\ NodeMatches(attempt.cacheRoot, attempt.root)
    /\ NodeMatches(attempt.cacheBranch, attempt.branch)

UntouchedSubtreeShared ==
  attempt /= NONE =>
    LET other == IF attempt.side = "left" THEN "right" ELSE "left"
        proposed == [txn |-> attempt.txn, root |-> attempt.root,
                     cache |-> attempt.cacheRoot]
     IN /\ BranchLba(proposed, other) = BranchLba(attempt.parent, other)
        /\ BranchCache(proposed, other) = BranchCache(attempt.parent, other)

FailedCommitIsolation ==
  failureWitness /= NONE =>
    /\ published = failureWitness.published
    /\ failureWitness.root \in free
    /\ failureWitness.branch \in free
    /\ failureWitness.root \notin inUse
    /\ failureWitness.branch \notin inUse

PublishedCandidateWasValidated ==
  publicationWitness /= NONE => Reachable(publicationWitness) \subseteq valid

ValidationFailurePreservesPublished ==
  failureWitness /= NONE /\ failureWitness.kind = "validation" =>
    /\ ~(Reachable(failureWitness.candidate) \subseteq valid)
    /\ published = failureWitness.published

CacheNodesImmutable ==
  [][\A c \in CacheNode :
       cacheHeap[c] /= NONE => cacheHeap'[c] = cacheHeap[c]]_vars

StateConstraint == published.txn <= MaxTxn

=============================================================================
