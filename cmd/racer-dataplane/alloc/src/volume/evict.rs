//! Slot eviction and reopening of partly filled slots.
//!
//! Eviction is incremental: it walks the slot's record map entries a batch
//! at a time, removes the index entries that still point at the slot,
//! yields between batches, and only then frees the slot. It returns the
//! removed entries so callers can drop their own bookkeeping.

use std::ops::Bound;

use uring_runtime::{Budget, Operation, Scope, drivers::yield_now, reactor::Reactor};

use super::format::{Key, Tier, Value};
use super::page::{record_key, record_key_parts};
use super::store::{EVICT_BATCH, IndexOp, RecOp, Reservation, Volume};
use crate::{Charge, Error};

/// A reservation (or why there is none) and the entries evicted for it.
pub type Allocated = (crate::Result<Reservation>, Vec<(Key, Value)>);

/// Partly filled slots [`Volume::allocate_evicting`] may reopen per call.
const REOPENS: usize = 2;

impl<C: Charge> Volume<C> {
    /// Evict one data slot with the CLOCK policy and remove its entries from
    /// the index. Returns the removed entries, or `None` when no slot can be
    /// evicted.
    ///
    /// A slot can be evicted when it is not open, not leased, its reference
    /// bit is clear, and it is a cache slot or a storage slot whose
    /// `retain_until` bound is below `now`. When no such slot exists, an idle
    /// open cache slot is closed and evicted. The slot is free at once, even
    /// for storage: an older on-disk tree that still points into it is caught
    /// by the record header check.
    ///
    /// The work is done [`EVICT_BATCH`] records at a time with a yield
    /// between batches. A slot whose record map is not fully readable is
    /// quarantined instead of freed.
    pub fn evict_one<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        now: u64,
        scope: &'a S,
    ) -> Operation<'a, Option<Vec<(Key, Value)>>, S::Error>
    where
        S::Error: From<Error>,
    {
        Box::pin(async move { self.evict(reactor, scope, now, None).await })
    }

    pub(super) async fn evict<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
        now: u64,
        tier: Option<Tier>,
    ) -> std::result::Result<Option<Vec<(Key, Value)>>, S::Error>
    where
        S::Error: From<Error>,
    {
        let Some((slot, incarnation)) = self.shared.state().slots.evict_candidate(now, tier) else {
            return Ok(None);
        };
        match self.evict_slot(reactor, scope, slot, incarnation).await {
            Ok(removed) => Ok(Some(removed)),
            Err(error) => {
                let _ = self.shared.state().slots.cancel_eviction(slot);
                Err(error)
            }
        }
    }

    async fn evict_slot<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
        slot: u32,
        incarnation: u32,
    ) -> std::result::Result<Vec<(Key, Value)>, S::Error>
    where
        S::Error: From<Error>,
    {
        let high = if u64::from(slot) + 1 < self.slot_count {
            Bound::Excluded(record_key(slot + 1, 0))
        } else {
            Bound::Unbounded
        };
        let mut cursor = Bound::Included(record_key(slot, 0));
        let mut removed = Vec::new();
        let mut entries = Vec::new();
        loop {
            let batch = self
                .scan_records(reactor, scope, cursor, high, EVICT_BATCH)
                .await?;
            let Some(last) = batch.last().map(|(k, _)| *k) else {
                break;
            };
            for (rk, desc) in batch {
                entries.push(rk);
                if desc.incarnation != incarnation {
                    continue;
                }
                let (_, unit) = record_key_parts(&rk);
                let value = self.resolve(reactor, scope, &desc.key).await?;
                if let Some(value) = value
                    && value.slot == slot
                    && value.unit == unit
                    && value.generation == desc.generation
                {
                    self.shared
                        .state()
                        .set(desc.key, IndexOp::Delete, Some(Some(value)));
                    removed.push((desc.key, value));
                }
            }
            cursor = Bound::Excluded(last);
            yield_now().await;
        }
        let mut state = self.shared.state();
        for rk in entries {
            state.active.records.insert(rk, RecOp::Delete);
        }
        let low = record_key(slot, 0);
        let lost = state.lost.iter().any(|(kind, l, h)| {
            *kind == super::page::TreeKind::Records
                && h.is_none_or(|h| h > low)
                && match high {
                    Bound::Excluded(end) => *l < end,
                    _ => true,
                }
        });
        if lost {
            // Some records of the slot are unaccounted for: an index entry may
            // still point into it. Never reuse it.
            state.slots.quarantine(slot);
        } else {
            state.slots.finish_eviction(slot).map_err(S::Error::from)?;
        }
        state.mutations += 1;
        Ok(removed)
    }

    /// Load the occupancy of a closed, partly filled slot of `tier` and
    /// `length`'s class so allocation can reuse its free records. Returns
    /// whether a slot was made ready.
    pub(super) async fn reopen<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
        tier: Tier,
        length: u64,
    ) -> std::result::Result<bool, S::Error>
    where
        S::Error: From<Error>,
    {
        let Some((slot, incarnation)) = self.shared.state().slots.partial_candidate(tier, length)
        else {
            return Ok(false);
        };
        let units = super::format::class_units(
            super::format::class_for(length).ok_or(Error::InvalidConfiguration)?,
        )
        .ok_or(Error::InvalidConfiguration)?;
        let records: Vec<u32> = self
            .slot_records(reactor, scope, slot)
            .await?
            .into_iter()
            .filter(|(_, desc)| desc.incarnation == incarnation)
            .map(|(rk, _)| u32::from(record_key_parts(&rk).1) / units)
            .collect();
        Ok(self
            .shared
            .state()
            .slots
            .install_bitmap(slot, incarnation, &records))
    }

    /// [`Volume::allocate`], reopening partly filled slots and then evicting
    /// up to `max_evictions` slots while it returns `Busy`. When the storage
    /// slot quota blocks a storage allocation, only expired storage slots are
    /// evicted. Returns the reservation and the entries removed by eviction
    /// (also when allocation still fails, they are gone). Does not evict when
    /// the dirty limit is what blocks allocation.
    pub fn allocate_evicting<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        tier: Tier,
        length: u64,
        now: u64,
        max_evictions: usize,
        scope: &'a S,
    ) -> Operation<'a, Allocated, S::Error>
    where
        S::Error: From<Error>,
    {
        Box::pin(async move {
            let mut evicted = Vec::new();
            let mut evictions = 0;
            let mut reopens = 0;
            loop {
                match self.allocate(tier, length) {
                    Err(Error::Busy) if !self.needs_commit() => {}
                    result => return Ok((result, evicted)),
                }
                if reopens < REOPENS && self.reopen(reactor, scope, tier, length).await? {
                    reopens += 1;
                    continue;
                }
                if evictions >= max_evictions {
                    return Ok((Err(Error::Busy), evicted));
                }
                evictions += 1;
                let only = self
                    .shared
                    .state()
                    .slots
                    .storage_quota_blocks(tier, length)
                    .then_some(Tier::Storage);
                match self.evict(reactor, scope, now, only).await? {
                    Some(removed) => evicted.extend(removed),
                    None => {
                        // Loose retention bounds may hide expired slots.
                        if self.refresh_retain(reactor, 4, scope).await? == 0 {
                            return Ok((Err(Error::Busy), evicted));
                        }
                    }
                }
            }
        })
    }

    /// Storage data slots whose `retain_until` bound is at least `now`: the
    /// only slots that can hold entries protected at `now`. O(slots), no I/O.
    pub fn retained_storage_slots(&self, now: u64) -> Vec<u32> {
        self.shared.state().slots.retained_storage(now)
    }

    /// Current index entries whose record lies in `slot`, found through the
    /// record map (no index scan).
    pub fn slot_entries<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        slot: u32,
        scope: &'a S,
    ) -> Operation<'a, Vec<(Key, Value)>, S::Error>
    where
        S::Error: From<Error>,
    {
        Box::pin(async move {
            let mut out = Vec::new();
            for (rk, desc) in self.slot_records(reactor, scope, slot).await? {
                let (_, unit) = record_key_parts(&rk);
                if let Some(value) = self.resolve(reactor, scope, &desc.key).await?
                    && value.slot == slot
                    && value.unit == unit
                    && value.generation == desc.generation
                {
                    out.push((desc.key, value));
                }
            }
            Ok(out)
        })
    }

    /// Evict until at least `free_slots` slots are free or nothing more can
    /// be evicted, at most `max_evictions` slots. Run it ahead of allocation
    /// (for example from a timer) so foreground writes rarely evict. Returns
    /// the removed entries.
    pub fn reserve_free<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        free_slots: u64,
        now: u64,
        max_evictions: usize,
        scope: &'a S,
    ) -> Operation<'a, Vec<(Key, Value)>, S::Error>
    where
        S::Error: From<Error>,
    {
        Box::pin(async move {
            let mut evicted = Vec::new();
            for _ in 0..max_evictions {
                if self.shared.state().slots.free_slots() >= free_slots {
                    break;
                }
                match self.evict(reactor, scope, now, None).await? {
                    Some(removed) => evicted.extend(removed),
                    None => break,
                }
            }
            Ok(evicted)
        })
    }
}
