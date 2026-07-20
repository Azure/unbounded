// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! CoW B+tree index over a [`BlockDevice`].
//!
//! Each per-disk [`crate::storage::engine::StorageEngine`] owns
//! one of these. It maps [`PageKey`] -> [`LeafEntry`] (the LBA,
//! data checksum, and byte length of a cached 2 MiB page) and
//! survives torn writes via a dual meta-page commit protocol.
//!
//! ## Concurrency model
//!
//! The index is *not* `Sync`. All commits go through a single
//! mutator task that holds the only `RefCell` borrow on
//! [`MutatorState`]; lookups go through
//! [`arc_swap::ArcSwap`] over [`RootSnapshot`] and are wait-free
//! against commits.
//!
//! ## Commit protocol
//!
//! For each [`BTreeIndex::apply_batch`] call:
//!
//! 1. Coalesce the mutation list into a sorted `(key, op)` vector.
//! 2. [`cow::apply_path_copy`] rewrites only the spine pages on
//!    the touched paths and shares every untouched subtree with
//!    the previous root. The result reports both the freshly
//!    allocated pages (owned by the new snapshot) and the
//!    retired pages (owned by some earlier snapshot up until
//!    this commit).
//! 3. Write the new meta page into the *inactive* slot, then update
//!    the committed in-memory mirror.
//! 4. Record the retired pages and this txn as alive, then
//!    [`arc_swap::ArcSwap::store`] the new [`RootSnapshot`].
//!    Once the previous snapshot's last [`arc_swap::Guard`]
//!    drops, its `Drop` impl recomputes the
//!    minimum-still-alive txn and frees every retired-page
//!    bundle that no live snapshot can still need.
//!
//! If step 2 or 3 fails the new pages are unwound back to the
//! allocator and the old snapshot remains the source of truth;
//! the retired-page accounting is not touched in that case.

use std::cell::{Cell, RefCell};
use std::collections::{BTreeMap, BTreeSet};
use std::rc::Rc;
use std::sync::Arc;

use arc_swap::ArcSwap;

use crate::storage::alloc::Allocator;
use crate::storage::blockdev::{BlockDevice, ScratchPool};
use crate::storage::types::{Error, Lba, PageKey};

mod cow;
mod meta;
mod page;
mod rebuild;

#[cfg(test)]
mod tests;

pub use page::LeafEntry;

/// A mutation in a commit batch. The mutator applies inserts
/// (overwriting) and deletes (no-op if absent) in input order.
#[derive(Clone, Debug)]
pub enum Mutation {
    Insert { key: PageKey, value: LeafEntry },
    Delete { key: PageKey },
}

/// Immutable view of one transaction id's tree. Snapshots are
/// shared via `ArcSwap`. The on-disk pages a snapshot reaches
/// are *not* listed here: every page is either still reachable
/// from some later snapshot (in which case it stays live) or it
/// was retired by a later commit (in which case that commit
/// recorded it in [`PendingFree`] under its own `txn_id`). The
/// snapshot's `Drop` simply removes itself from the alive set
/// and flushes any retired bundles that are now safe to free.
pub struct RootSnapshot {
    pub root_lba: Lba,
    pub txn_id: u64,
    tracker: Rc<RefCell<AliveTracker>>,
    pending: Rc<RefCell<PendingFree>>,
    allocator: Arc<Allocator>,
    internal_cache: Arc<cow::InternalNodeCache>,
}

impl RootSnapshot {
    fn new(
        root_lba: Lba,
        txn_id: u64,
        tracker: Rc<RefCell<AliveTracker>>,
        pending: Rc<RefCell<PendingFree>>,
        allocator: Arc<Allocator>,
        internal_cache: Arc<cow::InternalNodeCache>,
    ) -> Arc<Self> {
        Arc::new(Self {
            root_lba,
            txn_id,
            tracker,
            pending,
            allocator,
            internal_cache,
        })
    }

    fn lookup_cache_bytes(&self) -> usize {
        self.internal_cache.bytes()
    }
}

impl Drop for RootSnapshot {
    fn drop(&mut self) {
        // Pull our txn out of the alive set and recompute the
        // minimum-still-alive txn. Any retired-page bundle whose
        // retire_t <= new_min is now safe to free: every snapshot
        // that could possibly have referenced those pages has
        // been dropped.
        let new_min = {
            let mut tracker = self.tracker.borrow_mut();
            tracker.alive.remove(&self.txn_id);
            tracker.min_alive()
        };
        self.pending
            .borrow_mut()
            .flush_up_to(new_min, &self.allocator);
    }
}

/// Set of `txn_id`s for which a [`RootSnapshot`] is still alive
/// (held either by `ArcSwap` or by at least one outstanding
/// `Guard`). A txn is added in [`BTreeIndex::apply_batch`]
/// immediately before publishing the new snapshot and removed in
/// [`RootSnapshot::drop`] when the last reference goes away.
#[derive(Default)]
struct AliveTracker {
    alive: BTreeSet<u64>,
}

impl AliveTracker {
    fn min_alive(&self) -> u64 {
        // `u64::MAX` is the "no live snapshots" sentinel: every
        // pending bundle is then safe to flush. Real `txn_id`s
        // start at 1 and never reach `u64::MAX` in practice.
        self.alive.iter().next().copied().unwrap_or(u64::MAX)
    }
}

/// Deferred-free queue keyed by the `txn_id` of the commit that
/// retired the pages. Bundles are inserted by
/// [`BTreeIndex::apply_batch`] and drained by
/// [`RootSnapshot::drop`] once
/// [`AliveTracker::min_alive`] advances past `retire_t`.
#[derive(Default)]
struct PendingFree {
    by_retire_t: BTreeMap<u64, Vec<Lba>>,
}

impl PendingFree {
    fn push(&mut self, retire_t: u64, pages: Vec<Lba>) {
        if pages.is_empty() {
            return;
        }
        self.by_retire_t.entry(retire_t).or_default().extend(pages);
    }

    fn flush_up_to(&mut self, min_alive: u64, allocator: &Allocator) {
        // Free every bundle whose retire_t is no longer
        // reachable by any live snapshot. `min_alive == u64::MAX`
        // (no live snapshots) collapses to "free everything".
        loop {
            let next = match self.by_retire_t.keys().next().copied() {
                Some(t) => t,
                None => return,
            };
            if next > min_alive {
                return;
            }
            let pages = self.by_retire_t.remove(&next).unwrap_or_default();
            cow::free_all(allocator, &pages);
        }
    }
}

struct MutatorState {
    entries: BTreeMap<PageKey, LeafEntry>,
    next_txn_id: u64,
}

pub struct BTreeIndex<B: BlockDevice> {
    device: Arc<B>,
    allocator: Arc<Allocator>,
    scratch: Rc<ScratchPool>,
    root: ArcSwap<RootSnapshot>,
    mutator: RefCell<MutatorState>,
    active_meta: Cell<meta::MetaSlot>,
    alive: Rc<RefCell<AliveTracker>>,
    pending: Rc<RefCell<PendingFree>>,
}

impl<B: BlockDevice> BTreeIndex<B> {
    /// Open the index on `device`. The allocator is updated in
    /// place: meta slots are pinned, and any pages reachable from
    /// the recovered root are marked in-use so subsequent
    /// allocations do not collide with the live tree.
    ///
    /// `scratch` is a [`ScratchPool`] sized for at least two
    /// concurrent registered-buffer pages (meta load needs two);
    /// the engine constructs and shares it across the lifetime of
    /// the index.
    pub async fn open(
        device: Arc<B>,
        allocator: Arc<Allocator>,
        scratch: Rc<ScratchPool>,
        btree_page_bytes: usize,
        skip_recovery_scan_if_no_meta: bool,
        force_format: bool,
    ) -> Result<Self, Error> {
        // Reserve the meta slots regardless of disk state.
        let _ = allocator.mark_in_use(page::META_SLOT_A);
        let _ = allocator.mark_in_use(page::META_SLOT_B);

        if force_format {
            return Self::bootstrap_from_entries(
                device,
                allocator,
                scratch,
                btree_page_bytes,
                1,
                BTreeMap::new(),
            )
            .await;
        }

        let loaded = meta::load_meta(&*device, &scratch).await?;
        if let Some(state) = loaded {
            // Seed allocator HWM from persisted meta so future
            // commits keep monotonicity even if tree walk visits a
            // subset of the live frontier.
            allocator.observe_high_water(state.hwm);
            if let Some(idx) =
                Self::open_from_meta(&device, &allocator, &scratch, btree_page_bytes, state).await?
            {
                return Ok(idx);
            }
            // Meta said something but the tree underneath is
            // gone; fall through to rebuild.
        } else if skip_recovery_scan_if_no_meta {
            // Fresh disk path used by storage tooling: skip the
            // full-capacity LBA-order leaf scan and treat the
            // device as empty. The scan is the design's degraded
            // recovery path for the case where the meta slots are
            // corrupted but data leaves are intact; on a wiped
            // disk it is pure overhead (and on a multi-TB disk it
            // can take many minutes).
            return Self::bootstrap_from_entries(
                device,
                allocator,
                scratch,
                btree_page_bytes,
                1,
                BTreeMap::new(),
            )
            .await;
        }

        // Try LBA-scan rebuild.
        if let Some(rebuilt) =
            rebuild::scan_for_leaves(&*device, &scratch, loaded.as_ref().map(|s| s.hwm)).await?
        {
            return Self::bootstrap_from_entries(
                device,
                allocator,
                scratch,
                btree_page_bytes,
                rebuilt.txn_id + 1,
                rebuilt.entries,
            )
            .await;
        }

        // Fresh disk: install an empty tree at txn 1.
        Self::bootstrap_from_entries(
            device,
            allocator,
            scratch,
            btree_page_bytes,
            1,
            BTreeMap::new(),
        )
        .await
    }

    async fn open_from_meta(
        device: &Arc<B>,
        allocator: &Arc<Allocator>,
        scratch: &Rc<ScratchPool>,
        btree_page_bytes: usize,
        state: meta::MetaState,
    ) -> Result<Option<Self>, Error> {
        // A recovered snapshot is accepted only when every reachable page can
        // be decoded. This makes `None` cache children unambiguously leaves;
        // an unreadable or mixed-depth internal subtree cannot disappear from
        // validation and path-copy routing.
        let internal_cache =
            match cow::build_internal_cache(&**device, scratch, state.root_lba).await {
                Ok(cache) => cache,
                Err(_) => return Ok(None),
            };

        // `collect_pages` doubles as the "is the recovered root
        // structurally readable?" check: an empty list means
        // every page failed to read and we should fall through to
        // the LBA-scan rebuild path. We also use it to mark every
        // structural page in-use so the allocator bitmap matches
        // disk; we deliberately don't carry the list onto the
        // snapshot - retired pages are tracked per-commit via
        // path-copy, not per-snapshot.
        let pages = cow::collect_pages(&**device, scratch, state.root_lba).await?;
        if pages.is_empty() {
            return Ok(None);
        }
        for &lba in &pages {
            let _ = allocator.mark_in_use(lba);
        }

        let mut entries: BTreeMap<PageKey, LeafEntry> = BTreeMap::new();
        cow::for_each_leaf(&**device, scratch, state.root_lba, |k, v, _| {
            // Mark the entire contiguous data-page run referenced
            // by this leaf entry as in use. The entry covers
            // `byte_len / btree_page_bytes` device pages starting
            // at `v.lba`; without this, a reopened engine would
            // hand out an LBA inside the run via
            // `allocator.alloc()` / `alloc_contig()` and the next
            // write would overwrite live data. The structural
            // btree pages above are already marked via
            // `collect_pages`.
            let n = leaf_run_pages(v.byte_len, btree_page_bytes);
            let _ = allocator.mark_range_in_use(v.lba, n);
            entries.insert(k, v);
        })
        .await?;

        let alive = Rc::new(RefCell::new(AliveTracker::default()));
        let pending = Rc::new(RefCell::new(PendingFree::default()));
        alive.borrow_mut().alive.insert(state.txn_id);

        let snapshot = RootSnapshot::new(
            state.root_lba,
            state.txn_id,
            alive.clone(),
            pending.clone(),
            allocator.clone(),
            internal_cache,
        );

        Ok(Some(Self {
            device: device.clone(),
            allocator: allocator.clone(),
            scratch: scratch.clone(),
            root: ArcSwap::new(snapshot),
            mutator: RefCell::new(MutatorState {
                entries,
                next_txn_id: state.txn_id + 1,
            }),
            active_meta: Cell::new(state.active),
            alive,
            pending,
        }))
    }

    async fn bootstrap_from_entries(
        device: Arc<B>,
        allocator: Arc<Allocator>,
        scratch: Rc<ScratchPool>,
        btree_page_bytes: usize,
        txn_id: u64,
        entries: BTreeMap<PageKey, LeafEntry>,
    ) -> Result<Self, Error> {
        // Reserve every data-page run referenced by the seeded
        // entries before `build_tree` allocates the new structural
        // pages. The rebuild path lands here with entries pulled
        // from on-disk leaves; their LBAs are live but not yet in
        // the bitmap. Missing this lets `build_tree` (or any later
        // user write) hand out an LBA inside an existing run and
        // overwrite the data the entry points at.
        for v in entries.values() {
            let n = leaf_run_pages(v.byte_len, btree_page_bytes);
            let _ = allocator.mark_range_in_use(v.lba, n);
        }

        let sorted: Vec<(PageKey, LeafEntry)> = entries.iter().map(|(k, v)| (*k, *v)).collect();
        // Bulk-load the initial tree. The returned LBA list is not
        // tracked on the snapshot; those pages stay live until a
        // future path-copy commit retires them.
        let (root_lba, _bootstrap_pages) =
            cow::build_tree(&*device, &scratch, &allocator, txn_id, &sorted).await?;

        // Write meta into slot A by default (active=B means we
        // wrote to A; on next commit we'll write to B).
        let active = meta::write_inactive(
            &*device,
            &scratch,
            meta::MetaSlot::B,
            txn_id,
            root_lba,
            allocator.high_water(),
        )
        .await?;

        let alive = Rc::new(RefCell::new(AliveTracker::default()));
        let pending = Rc::new(RefCell::new(PendingFree::default()));
        alive.borrow_mut().alive.insert(txn_id);

        let internal_cache = cow::build_internal_cache(&*device, &scratch, root_lba).await?;

        let snapshot = RootSnapshot::new(
            root_lba,
            txn_id,
            alive.clone(),
            pending.clone(),
            allocator.clone(),
            internal_cache,
        );

        Ok(Self {
            device,
            allocator,
            scratch,
            root: ArcSwap::new(snapshot),
            mutator: RefCell::new(MutatorState {
                entries,
                next_txn_id: txn_id + 1,
            }),
            active_meta: Cell::new(active),
            alive,
            pending,
        })
    }

    /// Look up `key` by using the current snapshot's internal-node
    /// cache, then reading the terminal leaf from disk.
    pub async fn lookup(&self, key: &PageKey) -> Result<Option<LeafEntry>, Error> {
        let snap = self.root.load();
        cow::lookup(
            &*self.device,
            &self.scratch,
            snap.root_lba,
            &snap.internal_cache,
            key,
        )
        .await
    }

    /// Look up the mutator's committed in-memory mirror. The single
    /// mutator uses this authoritative view for prior-value and stale
    /// eviction checks. Client reads deliberately use [`Self::lookup`]
    /// so the terminal on-disk leaf remains checksum-validated.
    pub fn lookup_committed_mirror(&self, key: &PageKey) -> Option<LeafEntry> {
        self.mutator.borrow().entries.get(key).copied()
    }

    /// Apply a batch of mutations atomically. Must be called by
    /// at most one task at a time (the engine's mutator).
    pub async fn apply_batch(&self, mutations: Vec<Mutation>) -> Result<(), Error> {
        // Coalesce the input mutations into a sorted (key, op)
        // list with at most one entry per key. The live mirror is
        // not touched until the inactive meta write makes the new
        // root durable.
        let (sorted_ops, txn_id) = {
            let state = self.mutator.borrow();
            let mut ops: BTreeMap<PageKey, Option<LeafEntry>> = BTreeMap::new();
            for m in mutations {
                match m {
                    Mutation::Insert { key, value } => {
                        ops.insert(key, Some(value));
                    }
                    Mutation::Delete { key } => {
                        ops.insert(key, None);
                    }
                }
            }
            let sorted_ops: Vec<(PageKey, Option<LeafEntry>)> = ops.into_iter().collect();
            (sorted_ops, state.next_txn_id)
        };

        if sorted_ops.is_empty() {
            // No-op commit: skip the path-copy, meta write, and
            // snapshot publish. The current state remains durable
            // at the previously published txn.
            return Ok(());
        }

        // Path-copy consumes the operations, so retain only the bounded
        // batch needed to update the mirror after the durable commit.
        let mirror_ops = sorted_ops.clone();
        let parent = self.root.load_full();
        let result = cow::apply_path_copy(
            &*self.device,
            &self.scratch,
            &self.allocator,
            parent.root_lba,
            &parent.internal_cache,
            txn_id,
            sorted_ops,
        )
        .await?;

        // The path-copy cache is authoritative for routing, but disk remains
        // the recovery source of truth. Validate every internal page reachable
        // from the candidate root before replacing the fallback meta slot.
        // Validate against result.internal_cache so the checked topology is
        // exactly the immutable snapshot that will be published.
        if let Err(e) = cow::validate_internal_cache(
            &*self.device,
            &self.scratch,
            result.new_root,
            &result.internal_cache,
        )
        .await
        {
            cow::free_all(&self.allocator, &result.new_pages);
            return Err(e);
        }

        let active = self.active_meta.get();
        // apply_path_copy above has already marked the new pages
        // in use, so high_water() reflects them.
        let new_active = match meta::write_inactive(
            &*self.device,
            &self.scratch,
            active,
            txn_id,
            result.new_root,
            self.allocator.high_water(),
        )
        .await
        {
            Ok(s) => s,
            Err(e) => {
                // Roll back the path-copy: free every newly
                // allocated page. Retired pages stay live because
                // the still-published parent snapshot still
                // reaches them.
                cow::free_all(&self.allocator, &result.new_pages);
                return Err(e);
            }
        };

        // Commit point: from here on, in-memory mirror, txn
        // counter, active meta slot, retired-page accounting, and
        // the published snapshot all advance together.
        {
            let mut state = self.mutator.borrow_mut();
            for (key, value) in mirror_ops {
                match value {
                    Some(value) => {
                        state.entries.insert(key, value);
                    }
                    None => {
                        state.entries.remove(&key);
                    }
                }
            }
            state.next_txn_id = txn_id + 1;
        }
        self.active_meta.set(new_active);

        // Queue the retired pages under this commit's `txn_id`.
        // They become safe to free once every snapshot with
        // `txn < txn_id` has been dropped - see
        // [`RootSnapshot::drop`].
        self.pending.borrow_mut().push(txn_id, result.retired_pages);
        // Register this txn as alive *before* publishing the new
        // snapshot. If we published first, an immediate Drop of
        // the previous snapshot could observe an alive set that
        // doesn't yet include `txn_id`, compute the wrong
        // `min_alive`, and free pages we still need.
        self.alive.borrow_mut().alive.insert(txn_id);

        let snapshot = RootSnapshot::new(
            result.new_root,
            txn_id,
            self.alive.clone(),
            self.pending.clone(),
            self.allocator.clone(),
            result.internal_cache,
        );
        self.root.store(snapshot);
        Ok(())
    }

    pub fn current_root(&self) -> Lba {
        self.root.load().root_lba
    }

    pub fn current_txn(&self) -> u64 {
        self.root.load().txn_id
    }

    pub fn active_meta_slot(&self) -> u8 {
        match self.active_meta.get() {
            meta::MetaSlot::A => 0,
            meta::MetaSlot::B => 1,
        }
    }

    /// Number of live entries in the in-memory mirror. Exposed for
    /// diagnostics and tests.
    pub fn live_entries(&self) -> usize {
        self.mutator.borrow().entries.len()
    }

    /// Heap footprint of the immutable lookup cache owned by the
    /// currently published snapshot.
    pub fn lookup_cache_bytes(&self) -> usize {
        self.root.load().lookup_cache_bytes()
    }

    /// Largest LBA ever allocated on the underlying allocator. Exposed for diagnostics and tests.
    pub fn allocator_high_water(&self) -> u64 {
        self.allocator.high_water()
    }
}

/// Number of device pages spanned by a leaf entry whose payload is
/// `byte_len` bytes wide over a `btree_page_bytes`-page device.
/// Mirrors [`crate::storage::engine::StorageEngine::n_pages`]: the
/// write path constructs entries with `byte_len` a multiple of
/// `btree_page_bytes`, so this division is exact. Returns 0 if
/// `btree_page_bytes` is zero (defensive; the engine enforces a
/// positive value on construction).
fn leaf_run_pages(byte_len: u32, btree_page_bytes: usize) -> u64 {
    if btree_page_bytes == 0 {
        return 0;
    }
    (byte_len as usize / btree_page_bytes) as u64
}
