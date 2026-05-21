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
//! For each [`apply_batch`] call:
//!
//! 1. Apply the mutations to the in-memory `BTreeMap`.
//! 2. Bulk-write a fresh tree under freshly-allocated LBAs.
//! 3. Write the new meta page into the *inactive* slot.
//! 4. `ArcSwap::store` the new [`RootSnapshot`], which causes
//!    the old snapshot to be freed once the last in-flight
//!    lookup releases its [`arc_swap::Guard`]. The old
//!    snapshot's [`Drop`] hands every LBA back to the allocator.
//!
//! If step 2 or 3 fails the new pages are unwound back to the
//! allocator and the old snapshot remains the source of truth.

use std::cell::{Cell, RefCell};
use std::collections::BTreeMap;
use std::sync::Arc;

use arc_swap::ArcSwap;

use crate::storage::alloc::Allocator;
use crate::storage::blockdev::BlockDevice;
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
/// shared via `ArcSwap`; their `Drop` returns the LBAs they own
/// (everything reachable from `root_lba`, minus meta slots) to
/// the allocator.
pub struct RootSnapshot {
    pub root_lba: Lba,
    pub txn_id: u64,
    pages: Vec<Lba>,
    allocator: Arc<Allocator>,
}

impl RootSnapshot {
    fn new(root_lba: Lba, txn_id: u64, pages: Vec<Lba>, allocator: Arc<Allocator>) -> Arc<Self> {
        Arc::new(Self {
            root_lba,
            txn_id,
            pages,
            allocator,
        })
    }
}

impl Drop for RootSnapshot {
    fn drop(&mut self) {
        cow::free_all(&self.allocator, &self.pages);
    }
}

struct MutatorState {
    entries: BTreeMap<PageKey, LeafEntry>,
    next_txn_id: u64,
}

pub struct BTreeIndex<B: BlockDevice> {
    device: Arc<B>,
    allocator: Arc<Allocator>,
    root: ArcSwap<RootSnapshot>,
    mutator: RefCell<MutatorState>,
    active_meta: Cell<meta::MetaSlot>,
}

impl<B: BlockDevice> BTreeIndex<B> {
    /// Open the index on `device`. The allocator is updated in
    /// place: meta slots are pinned, and any pages reachable from
    /// the recovered root are marked in-use so subsequent
    /// allocations do not collide with the live tree.
    pub async fn open(device: Arc<B>, allocator: Arc<Allocator>) -> Result<Self, Error> {
        // Reserve the meta slots regardless of disk state.
        let _ = allocator.mark_in_use(page::META_SLOT_A);
        let _ = allocator.mark_in_use(page::META_SLOT_B);

        let loaded = meta::load_meta(&*device).await?;
        if let Some(state) = loaded {
            // Seed allocator HWM from persisted meta so future
            // commits keep monotonicity even if tree walk visits a
            // subset of the live frontier.
            allocator.observe_high_water(state.hwm);
            if let Some(idx) = Self::open_from_meta(&device, &allocator, state).await? {
                return Ok(idx);
            }
            // Meta said something but the tree underneath is
            // gone; fall through to rebuild.
        }

        // Try LBA-scan rebuild.
        if let Some(rebuilt) =
            rebuild::scan_for_leaves(&*device, loaded.as_ref().map(|s| s.hwm)).await?
        {
            return Self::bootstrap_from_entries(
                device,
                allocator,
                rebuilt.txn_id + 1,
                rebuilt.entries,
            )
            .await;
        }

        // Fresh disk: install an empty tree at txn 1.
        Self::bootstrap_from_entries(device, allocator, 1, BTreeMap::new()).await
    }

    async fn open_from_meta(
        device: &Arc<B>,
        allocator: &Arc<Allocator>,
        state: meta::MetaState,
    ) -> Result<Option<Self>, Error> {
        let pages = cow::collect_pages(&**device, state.root_lba).await?;
        if pages.is_empty() {
            // Root was unreadable / corrupt.
            return Ok(None);
        }
        for &lba in &pages {
            let _ = allocator.mark_in_use(lba);
        }

        let mut entries: BTreeMap<PageKey, LeafEntry> = BTreeMap::new();
        cow::for_each_leaf(&**device, state.root_lba, |k, v, _| {
            entries.insert(k, v);
        })
        .await?;

        let snapshot = RootSnapshot::new(state.root_lba, state.txn_id, pages, allocator.clone());

        Ok(Some(Self {
            device: device.clone(),
            allocator: allocator.clone(),
            root: ArcSwap::new(snapshot),
            mutator: RefCell::new(MutatorState {
                entries,
                next_txn_id: state.txn_id + 1,
            }),
            active_meta: Cell::new(state.active),
        }))
    }

    async fn bootstrap_from_entries(
        device: Arc<B>,
        allocator: Arc<Allocator>,
        txn_id: u64,
        entries: BTreeMap<PageKey, LeafEntry>,
    ) -> Result<Self, Error> {
        let sorted: Vec<(PageKey, LeafEntry)> = entries.iter().map(|(k, v)| (*k, *v)).collect();
        let (root_lba, pages) = cow::build_tree(&*device, &allocator, txn_id, &sorted).await?;

        // Write meta into slot A by default (active=B means we
        // wrote to A; on next commit we'll write to B).
        let active = meta::write_inactive(
            &*device,
            meta::MetaSlot::B,
            txn_id,
            root_lba,
            allocator.high_water(),
        )
        .await?;

        let snapshot = RootSnapshot::new(root_lba, txn_id, pages, allocator.clone());

        Ok(Self {
            device,
            allocator,
            root: ArcSwap::new(snapshot),
            mutator: RefCell::new(MutatorState {
                entries,
                next_txn_id: txn_id + 1,
            }),
            active_meta: Cell::new(active),
        })
    }

    /// Look up `key`. Disk errors and structural corruption
    /// surface as `Ok(None)` per the design's silent-miss policy.
    pub async fn lookup(&self, key: &PageKey) -> Result<Option<LeafEntry>, Error> {
        let snap = self.root.load();
        cow::lookup(&*self.device, snap.root_lba, key).await
    }

    /// Apply a batch of mutations atomically. Must be called by
    /// at most one task at a time (the engine's mutator).
    pub async fn apply_batch(&self, mutations: Vec<Mutation>) -> Result<(), Error> {
        // Build the next-state map without touching the live
        // mirror yet. If build_tree or the meta-page write fails
        // we leave the in-memory state exactly as it was so the
        // caller's recovery (freeing the new data LBA, etc.)
        // matches what is on disk.
        let (sorted, txn_id, next_entries) = {
            let state = self.mutator.borrow();
            let mut next = state.entries.clone();
            for m in mutations {
                match m {
                    Mutation::Insert { key, value } => {
                        next.insert(key, value);
                    }
                    Mutation::Delete { key } => {
                        next.remove(&key);
                    }
                }
            }
            let sorted: Vec<(PageKey, LeafEntry)> = next.iter().map(|(k, v)| (*k, *v)).collect();
            let txn_id = state.next_txn_id;
            (sorted, txn_id, next)
        };

        let (root_lba, pages) =
            cow::build_tree(&*self.device, &self.allocator, txn_id, &sorted).await?;

        let active = self.active_meta.get();
        // build_tree above has already marked the new pages in
        // use, so high_water() reflects them.
        let new_active = match meta::write_inactive(
            &*self.device,
            active,
            txn_id,
            root_lba,
            self.allocator.high_water(),
        )
        .await
        {
            Ok(s) => s,
            Err(e) => {
                cow::free_all(&self.allocator, &pages);
                return Err(e);
            }
        };

        // Commit point: from here on, in-memory mirror, txn
        // counter, active meta slot, and the published snapshot
        // all advance together.
        {
            let mut state = self.mutator.borrow_mut();
            state.entries = next_entries;
            state.next_txn_id = txn_id + 1;
        }
        let snapshot = RootSnapshot::new(root_lba, txn_id, pages, self.allocator.clone());
        self.active_meta.set(new_active);
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

    /// Number of live entries in the in-memory mirror. Exposed
    /// for diagnostics and tests; lookups never use this.
    pub fn live_entries(&self) -> usize {
        self.mutator.borrow().entries.len()
    }

    /// Largest LBA ever allocated on the underlying allocator. Exposed for diagnostics and tests.
    pub fn allocator_high_water(&self) -> u64 {
        self.allocator.high_water()
    }
}
