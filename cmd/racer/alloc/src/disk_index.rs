//! Payload-agnostic disk index over interned page slots.
//!
//! [`DiskIndex`] owns everything a storage worker knows about records on
//! disk: stored entries, per-cache bytes, hard holds, in-flight publications,
//! and segment heat. It performs no I/O and reads no clock: time is passed in
//! as [`Monotonic`] nanoseconds, so production and the DST Model drive the
//! same code. Pages are identified by [`PageSlot`] (a per-worker arena index
//! interned by the caller) and caches by [`CacheIdx`] (at most
//! [`MAX_CACHES`]) fenced by a generation in [`CacheKey`].
//!
//! # Invariants enforced by types
//!
//! - A [`RecordLocation`] can only come from [`DiskIndex::append`] (or a
//!   validated [`DiskIndex::restore`]); its fields are private.
//! - Publishing requires a [`PageTicket`] from [`DiskIndex::reserve`] and the
//!   [`Written`] token from [`DiskIndex::write_complete`]; neither can be
//!   constructed or cloned outside this module.
//! - Segment generations come only from [`Segments`], which increments them
//!   monotonically on recycle.
//!
//! # Costs
//!
//! Every per-segment question used by eviction is O(1): `held` (stored
//! entries with a live holder) and `writes` (appends not yet completed) are
//! counters kept incrementally. [`DiskIndex::holds_unused`] is O(1) too. Hold
//! expiry is an explicit bounded sweep ([`DiskIndex::expire_holds`]) on the
//! monotonic clock only; a hold counts until swept. Eviction is one scored
//! segment clock ([`DiskIndex::reclaim`]) that ages heat as it visits.
//! Removing a cache or a segment walks only that cache's or segment's
//! intrusive list. An entry is at most 56 bytes: 48 for the record, segment
//! and cache lists, plus 8 for the publish-order (age) list that
//! [`AgeCursor`] walks.
//!
//! # Callers
//!
//! The racer storage worker (`src/store/pages.rs`) interns `PageId -> PageSlot`
//! and cache IDs with [`crate::intern`], keeps `PageId` in records and
//! checkpoints, and calls [`DiskIndex::reserve`], [`DiskIndex::append`],
//! [`DiskIndex::write_complete`], and [`DiskIndex::publish`] around each write.
//! Each object-version hold maps to one [`HoldId`] per reservation, so a stale
//! release cannot drop a newer holder. Live hold expiry uses [`Monotonic`]
//! deadlines only; restart recomputes them from persisted wall times.

use crate::{Error, Extent, Generation, Result, SegmentId, SegmentLease, SegmentState, Segments};
use std::collections::{BTreeMap, BTreeSet};

/// Maximum caches a disk index tracks at once: the bound on live [`CacheIdx`] values.
pub const MAX_CACHES: usize = 4096;

const NIL: u32 = u32::MAX;
const STORED: u8 = 1;
const INFLIGHT: u8 = 2;

/// Per-worker interned page identity.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct PageSlot(pub u32);

/// Node-assigned cache index, below [`MAX_CACHES`].
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct CacheIdx(pub u16);

/// A cache index fenced by the generation it was added under.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct CacheKey {
    idx: CacheIdx,

    generation: u32,
}

impl CacheKey {
    /// The cache index.
    pub fn idx(self) -> CacheIdx {
        self.idx
    }
}

/// Monotonic time in nanoseconds from an arbitrary origin.
#[derive(Clone, Copy, Debug, Default, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct Monotonic(pub u64);

/// Where a record lives. Only produced by [`DiskIndex::append`] or a
/// validated [`DiskIndex::restore`].
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RecordLocation {
    segment: SegmentId,

    generation: Generation,

    extent: Extent,
}

impl RecordLocation {
    /// Segment holding the record.
    pub fn segment(&self) -> SegmentId {
        self.segment
    }

    /// Segment generation the record was written in.
    pub fn generation(&self) -> Generation {
        self.generation
    }

    /// Byte range of the record.
    pub fn extent(&self) -> Extent {
        self.extent
    }
}

/// A stored entry as seen by callers.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct EntryView {
    /// Owning cache.
    pub cache: CacheIdx,

    /// Record location.
    pub location: RecordLocation,

    /// Live holds covering the slot.
    pub holders: u16,
}

/// An entry removed from the index.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Removed {
    /// Owning cache.
    pub cache: CacheIdx,

    /// Location the record occupied.
    pub location: RecordLocation,
}

/// Exclusive right to publish one slot. Not `Clone`; return it with
/// [`DiskIndex::cancel`] if the write is abandoned.
#[must_use = "cancel or publish the ticket"]
#[derive(Debug)]
pub struct PageTicket {
    slot: PageSlot,

    cache: CacheKey,
}

impl PageTicket {
    /// The reserved slot.
    pub fn slot(&self) -> PageSlot {
        self.slot
    }
}

/// An append whose write is in flight. Keeps its segment from eviction.
#[must_use = "complete or fail the write"]
#[derive(Debug)]
pub struct Writing {
    ticket: PageTicket,

    location: RecordLocation,
}

impl Writing {
    /// Where the record is being written.
    pub fn location(&self) -> RecordLocation {
        self.location
    }

    /// The reserved slot.
    pub fn slot(&self) -> PageSlot {
        self.ticket.slot
    }
}

/// Proof that a record's write completed.
#[must_use = "publish the written record"]
#[derive(Debug)]
pub struct Written {
    ticket: PageTicket,

    location: RecordLocation,
}

/// Identity of one hold reservation. Identities increase with every hold;
/// the default is older than any issued hold.
#[derive(Clone, Copy, Debug, Default, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct HoldId(u64);

/// Number of independent [`AgeCursor`]s per index.
pub const AGE_CURSORS: usize = 4;

/// One of [`AGE_CURSORS`] resumable walks over stored entries in publish
/// order (oldest first). The index repairs a cursor when the entry it last
/// returned is removed, so a walk never skips an entry that was stored when
/// the walk passed its position; a replaced entry moves to the tail and is
/// visited again.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct AgeCursor(pub u8);

/// Bounds for one [`DiskIndex::reclaim`] call.
#[derive(Clone, Copy, Debug)]
pub struct ReclaimBudget {
    /// Free segments to restore.
    pub free_reserve: usize,

    /// Segments the clock may visit (also capped at 64).
    pub max_visits: usize,

    /// Entries that may be removed.
    pub max_entries: usize,
}

/// One slot's record. Kept at 56 bytes; see the size test.
#[derive(Clone, Copy, Debug)]
struct Entry {
    generation: u64,

    offset: u64,

    length: u32,

    segment: u32,

    seg_prev: u32,

    seg_next: u32,

    cache_prev: u32,

    cache_next: u32,

    age_prev: u32,

    age_next: u32,

    holders: u16,

    cache: u16,

    flags: u8,
}

impl Entry {
    const EMPTY: Self = Self {
        generation: 0,
        offset: 0,
        length: 0,
        segment: NIL,
        seg_prev: NIL,
        seg_next: NIL,
        cache_prev: NIL,
        cache_next: NIL,
        age_prev: NIL,
        age_next: NIL,
        holders: 0,
        cache: 0,
        flags: 0,
    };

    fn stored(&self) -> bool {
        self.flags & STORED != 0
    }

    fn inflight(&self) -> bool {
        self.flags & INFLIGHT != 0
    }

    fn location(&self) -> RecordLocation {
        RecordLocation {
            segment: SegmentId(u64::from(self.segment)),
            generation: Generation(self.generation),
            extent: Extent::new(self.offset, self.length as usize).expect("validated extent"),
        }
    }

    /// (counts toward unused holds, held stored segment, occupies capacity)
    fn contribution(&self) -> (bool, Option<u32>, bool) {
        let held = self.holders != 0;
        (
            held && self.flags == 0,
            (held && self.stored()).then_some(self.segment),
            self.flags != 0,
        )
    }
}

#[derive(Clone, Copy, Debug, Default)]
struct SegmentMeta {
    head: u32,

    entries: u32,

    bytes: u64,

    held: u32,

    writes: u32,

    heat: u32,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum CacheState {
    Absent,
    Live,
    Draining,
}

#[derive(Clone, Copy, Debug)]
struct CacheMeta {
    generation: u32,

    state: CacheState,

    head: u32,

    entries: u32,

    bytes: u64,

    inflight: u32,
}

impl CacheMeta {
    const ABSENT: Self = Self {
        generation: 0,
        state: CacheState::Absent,
        head: NIL,
        entries: 0,
        bytes: 0,
        inflight: 0,
    };
}

#[derive(Clone, Debug)]
struct Hold {
    until: Monotonic,

    slots: Vec<PageSlot>,
}

#[derive(Clone, Copy)]
enum List {
    Segment,
    Cache,
}

/// See the module docs.
#[derive(Debug)]
pub struct DiskIndex {
    entries: Vec<Entry>,

    capacity: usize,

    occupied: usize,

    segments: Vec<SegmentMeta>,

    caches: Vec<CacheMeta>,

    holds: BTreeMap<HoldId, Hold>,

    expiry: BTreeSet<(Monotonic, HoldId)>,

    next_hold: u64,

    holds_unused: usize,

    hand: usize,

    age_head: u32,

    age_tail: u32,

    /// Last entry returned per cursor; `NIL` restarts at the oldest entry.
    cursors: [u32; AGE_CURSORS],
}

impl DiskIndex {
    /// An empty index over `slots` page slots holding at most `capacity`
    /// stored or in-flight entries, across `segments` segments.
    pub fn new(slots: u32, capacity: usize, segments: usize) -> Result<Self> {
        if slots == NIL || segments >= NIL as usize {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self {
            entries: vec![Entry::EMPTY; slots as usize],
            capacity,
            occupied: 0,
            segments: vec![
                SegmentMeta {
                    head: NIL,
                    ..SegmentMeta::default()
                };
                segments
            ],
            caches: Vec::new(),
            holds: BTreeMap::new(),
            expiry: BTreeSet::new(),
            next_hold: 0,
            holds_unused: 0,
            hand: 0,
            age_head: NIL,
            age_tail: NIL,
            cursors: [NIL; AGE_CURSORS],
        })
    }

    /// Stored plus in-flight entries.
    pub fn len(&self) -> usize {
        self.occupied
    }

    /// Whether nothing is stored or in flight.
    pub fn is_empty(&self) -> bool {
        self.occupied == 0
    }

    /// Maximum stored plus in-flight entries.
    pub fn capacity(&self) -> usize {
        self.capacity
    }

    fn entry(&self, slot: PageSlot) -> Result<&Entry> {
        self.entries
            .get(slot.0 as usize)
            .ok_or(Error::InvalidConfiguration)
    }

    /// Apply `change` to a slot and keep the hold and capacity counters exact.
    fn update<T>(&mut self, slot: PageSlot, change: impl FnOnce(&mut Self) -> T) -> T {
        let before = self.entries[slot.0 as usize].contribution();
        let result = change(self);
        let after = self.entries[slot.0 as usize].contribution();
        match (before.0, after.0) {
            (false, true) => self.holds_unused += 1,
            (true, false) => self.holds_unused -= 1,
            _ => {}
        }
        if before.1 != after.1 {
            if let Some(segment) = before.1 {
                self.segments[segment as usize].held -= 1;
            }
            if let Some(segment) = after.1 {
                self.segments[segment as usize].held += 1;
            }
        }
        match (before.2, after.2) {
            (false, true) => self.occupied += 1,
            (true, false) => self.occupied -= 1,
            _ => {}
        }
        result
    }

    fn links(&mut self, slot: u32, list: List) -> (&mut u32, &mut u32) {
        let entry = &mut self.entries[slot as usize];
        match list {
            List::Segment => (&mut entry.seg_prev, &mut entry.seg_next),
            List::Cache => (&mut entry.cache_prev, &mut entry.cache_next),
        }
    }

    fn head(&mut self, slot: u32, list: List) -> &mut u32 {
        let entry = self.entries[slot as usize];
        match list {
            List::Segment => &mut self.segments[entry.segment as usize].head,
            List::Cache => &mut self.caches[usize::from(entry.cache)].head,
        }
    }

    fn link(&mut self, slot: u32, list: List) {
        let head = *self.head(slot, list);
        let (prev, next) = self.links(slot, list);
        *prev = NIL;
        *next = head;
        if head != NIL {
            *self.links(head, list).0 = slot;
        }
        *self.head(slot, list) = slot;
    }

    fn unlink(&mut self, slot: u32, list: List) {
        let (prev, next) = {
            let (prev, next) = self.links(slot, list);
            (*prev, *next)
        };
        if prev == NIL {
            *self.head(slot, list) = next;
        } else {
            *self.links(prev, list).1 = next;
        }
        if next != NIL {
            *self.links(next, list).0 = prev;
        }
    }

    fn store(&mut self, slot: PageSlot, cache: CacheIdx, location: RecordLocation) {
        self.update(slot, |this| {
            let entry = &mut this.entries[slot.0 as usize];
            debug_assert!(!entry.stored());
            entry.generation = location.generation.0;
            entry.offset = location.extent.offset();
            entry.length = location.extent.length() as u32;
            entry.segment = location.segment.0 as u32;
            entry.cache = cache.0;
            entry.flags |= STORED;
            let length = u64::from(entry.length);
            let segment = &mut this.segments[location.segment.0 as usize];
            segment.entries += 1;
            segment.bytes += length;
            let meta = &mut this.caches[usize::from(cache.0)];
            meta.entries += 1;
            meta.bytes += length;
            this.link(slot.0, List::Segment);
            this.link(slot.0, List::Cache);
            this.age_push(slot.0);
        });
    }

    fn unstore(&mut self, slot: PageSlot) -> Removed {
        self.update(slot, |this| {
            this.unlink(slot.0, List::Segment);
            this.unlink(slot.0, List::Cache);
            this.age_unlink(slot.0);
            let entry = &mut this.entries[slot.0 as usize];
            debug_assert!(entry.stored());
            let removed = Removed {
                cache: CacheIdx(entry.cache),
                location: entry.location(),
            };
            entry.flags &= !STORED;
            let length = u64::from(entry.length);
            let segment = &mut this.segments[entry.segment as usize];
            segment.entries -= 1;
            segment.bytes -= length;
            let meta = &mut this.caches[usize::from(entry.cache)];
            meta.entries -= 1;
            meta.bytes -= length;
            entry.segment = NIL;
            this.settle_cache(removed.cache);
            removed
        })
    }

    fn age_push(&mut self, slot: u32) {
        let tail = self.age_tail;
        let entry = &mut self.entries[slot as usize];
        entry.age_prev = tail;
        entry.age_next = NIL;
        if tail == NIL {
            self.age_head = slot;
        } else {
            self.entries[tail as usize].age_next = slot;
        }
        self.age_tail = slot;
    }

    fn age_unlink(&mut self, slot: u32) {
        let Entry {
            age_prev: prev,
            age_next: next,
            ..
        } = self.entries[slot as usize];
        if prev == NIL {
            self.age_head = next;
        } else {
            self.entries[prev as usize].age_next = next;
        }
        if next == NIL {
            self.age_tail = prev;
        } else {
            self.entries[next as usize].age_prev = prev;
        }
        // A cursor parked on this entry resumes at its successor.
        for cursor in &mut self.cursors {
            if *cursor == slot {
                *cursor = prev;
            }
        }
    }

    // ---- publish order ----

    /// Restart `cursor` at the oldest stored entry.
    pub fn age_reset(&mut self, cursor: AgeCursor) {
        if let Some(position) = self.cursors.get_mut(usize::from(cursor.0)) {
            *position = NIL;
        }
    }

    /// Stored entries after `cursor`'s position, oldest first, skipping
    /// caches being removed. Does not move the cursor; callers bound the walk
    /// with [`Iterator::take`] and then call [`Self::age_seek`].
    pub fn age_entries(
        &self,
        cursor: AgeCursor,
    ) -> impl Iterator<Item = (PageSlot, EntryView)> + '_ {
        let mut next = match self.cursors.get(usize::from(cursor.0)) {
            Some(&NIL) | None => self.age_head,
            Some(&slot) => self.entries[slot as usize].age_next,
        };
        std::iter::from_fn(move || {
            while next != NIL {
                let slot = PageSlot(next);
                next = self.entries[next as usize].age_next;
                if let Some(view) = self.get(slot) {
                    return Some((slot, view));
                }
            }
            None
        })
    }

    /// Park `cursor` on stored `slot`, normally the last entry a walk
    /// returned.
    pub fn age_seek(&mut self, cursor: AgeCursor, slot: PageSlot) -> Result<()> {
        if !self.entry(slot)?.stored() {
            return Err(Error::Stale);
        }
        let position = self
            .cursors
            .get_mut(usize::from(cursor.0))
            .ok_or(Error::InvalidConfiguration)?;
        *position = slot.0;
        Ok(())
    }

    // ---- caches ----

    /// Start accepting publications for `idx`. Fails with `Busy` while a
    /// previous cache at this index is still draining.
    pub fn add_cache(&mut self, idx: CacheIdx) -> Result<CacheKey> {
        let index = usize::from(idx.0);
        if index >= MAX_CACHES {
            return Err(Error::InvalidConfiguration);
        }
        if self.caches.len() <= index {
            self.caches.resize(index + 1, CacheMeta::ABSENT);
        }
        let meta = &mut self.caches[index];
        if meta.state != CacheState::Absent {
            return Err(Error::Busy);
        }
        meta.state = CacheState::Live;
        Ok(CacheKey {
            idx,
            generation: meta.generation,
        })
    }

    fn live(&self, key: CacheKey) -> bool {
        self.caches
            .get(usize::from(key.idx.0))
            .is_some_and(|meta| meta.state == CacheState::Live && meta.generation == key.generation)
    }

    fn settle_cache(&mut self, idx: CacheIdx) {
        let meta = &mut self.caches[usize::from(idx.0)];
        if meta.state == CacheState::Draining && meta.entries == 0 && meta.inflight == 0 {
            meta.state = CacheState::Absent;
            meta.generation = meta.generation.wrapping_add(1);
        }
    }

    /// Stop serving `key` and remove up to `budget` of its entries. Returns
    /// `true` once the cache is gone and its index can be reused; in-flight
    /// publications must be published (rejected) or cancelled first.
    pub fn remove_cache(
        &mut self,
        key: CacheKey,
        budget: usize,
        mut removed: impl FnMut(PageSlot, Removed),
    ) -> Result<bool> {
        let index = usize::from(key.idx.0);
        let meta = self.caches.get_mut(index).ok_or(Error::Stale)?;
        if meta.generation != key.generation || meta.state == CacheState::Absent {
            return Err(Error::Stale);
        }
        meta.state = CacheState::Draining;
        for _ in 0..budget {
            let head = self.caches[index].head;
            if head == NIL {
                break;
            }
            let entry = self.unstore(PageSlot(head));
            removed(PageSlot(head), entry);
        }
        self.settle_cache(key.idx);
        Ok(self.caches[index].state == CacheState::Absent)
    }

    /// Bytes stored for a live or draining cache.
    pub fn cache_bytes(&self, idx: CacheIdx) -> u64 {
        self.caches
            .get(usize::from(idx.0))
            .map_or(0, |meta| meta.bytes)
    }

    // ---- lookup ----

    /// The stored entry for `slot`, unless its cache is being removed.
    pub fn get(&self, slot: PageSlot) -> Option<EntryView> {
        let entry = self.entry(slot).ok()?;
        if !entry.stored() || self.caches[usize::from(entry.cache)].state != CacheState::Live {
            return None;
        }
        Some(EntryView {
            cache: CacheIdx(entry.cache),
            location: entry.location(),
            holders: entry.holders,
        })
    }

    /// Whether a publication for `slot` is in flight.
    pub fn is_inflight(&self, slot: PageSlot) -> bool {
        self.entry(slot).is_ok_and(Entry::inflight)
    }

    /// Whether `slot` is stored (even in a draining cache), in flight, or held,
    /// so its interned identity must be kept.
    pub fn references(&self, slot: PageSlot) -> bool {
        self.entry(slot)
            .is_ok_and(|entry| entry.stored() || entry.inflight() || entry.holders != 0)
    }

    /// Every stored entry, in slot order (for checkpoints).
    pub fn iter(&self) -> impl Iterator<Item = (PageSlot, EntryView)> + '_ {
        (0..self.entries.len() as u32)
            .filter_map(|slot| Some((PageSlot(slot), self.get(PageSlot(slot))?)))
    }

    /// Record a read hit, raising its segment's retention score.
    pub fn touch(&mut self, slot: PageSlot) {
        if let Ok(&entry) = self.entry(slot)
            && entry.stored()
        {
            let segment = &mut self.segments[entry.segment as usize];
            segment.heat = segment.heat.saturating_add(1);
        }
    }

    // ---- publication ----

    fn check_reserve(&self, slot: PageSlot, cache: CacheKey, freed: usize) -> Result<()> {
        let entry = self.entry(slot)?;
        if !self.live(cache) {
            return Err(Error::Stale);
        }
        if entry.inflight() {
            return Err(Error::Busy);
        }
        if !entry.stored() && self.occupied - freed >= self.capacity {
            return Err(Error::Busy);
        }
        Ok(())
    }

    fn reserve_unchecked(&mut self, slot: PageSlot, cache: CacheKey) -> PageTicket {
        self.update(slot, |this| this.entries[slot.0 as usize].flags |= INFLIGHT);
        self.caches[usize::from(cache.idx.0)].inflight += 1;
        PageTicket { slot, cache }
    }

    /// Reserve the right to publish `slot` for `cache`. A stored slot may be
    /// re-published (replacing its record) without extra capacity.
    pub fn reserve(&mut self, slot: PageSlot, cache: CacheKey) -> Result<PageTicket> {
        self.check_reserve(slot, cache, 0)?;
        Ok(self.reserve_unchecked(slot, cache))
    }

    /// Transactional queue replacement: validate that `slot` can be reserved
    /// once `victim` is gone and that `victim` is removable, and only then
    /// remove `victim` and reserve. On error nothing changes.
    pub fn reserve_replacing(
        &mut self,
        slot: PageSlot,
        cache: CacheKey,
        victim: PageSlot,
    ) -> Result<(PageTicket, Removed)> {
        let entry = self.entry(victim)?;
        if victim == slot || !entry.stored() || entry.inflight() || entry.holders != 0 {
            return Err(Error::Busy);
        }
        self.check_reserve(slot, cache, 1)?;
        let removed = self.unstore(victim);
        Ok((self.reserve_unchecked(slot, cache), removed))
    }

    fn release_ticket(&mut self, ticket: PageTicket) {
        let PageTicket { slot, cache } = ticket;
        self.update(slot, |this| {
            this.entries[slot.0 as usize].flags &= !INFLIGHT
        });
        self.caches[usize::from(cache.idx.0)].inflight -= 1;
        self.settle_cache(cache.idx);
    }

    /// Abandon a reservation.
    pub fn cancel(&mut self, ticket: PageTicket) {
        self.release_ticket(ticket);
    }

    /// Reserve disk space for `ticket`'s record. The returned lease must stay
    /// alive until the write completes; the segment is not evicted meanwhile.
    pub fn append(
        &mut self,
        segments: &Segments,
        ticket: PageTicket,
        length: usize,
    ) -> std::result::Result<(SegmentLease, Writing), (PageTicket, Error)> {
        let (lease, extent) = match segments.append(length) {
            Ok(appended) => appended,
            Err(error) => return Err((ticket, error)),
        };
        let index = lease.id().0 as usize;
        if index >= self.segments.len() {
            return Err((ticket, Error::InvalidConfiguration));
        }
        self.segments[index].writes += 1;
        let location = RecordLocation {
            segment: lease.id(),
            generation: lease.generation(),
            extent,
        };
        Ok((lease, Writing { ticket, location }))
    }

    fn end_write(&mut self, location: RecordLocation) {
        self.segments[location.segment.0 as usize].writes -= 1;
    }

    /// The write landed; publication may proceed.
    pub fn write_complete(&mut self, writing: Writing) -> Written {
        self.end_write(writing.location);
        Written {
            ticket: writing.ticket,
            location: writing.location,
        }
    }

    /// The write failed; the reservation is released.
    pub fn write_failed(&mut self, writing: Writing) {
        self.end_write(writing.location);
        self.release_ticket(writing.ticket);
    }

    /// Make a written record visible, replacing any previous record of the
    /// slot. A cache removed since reservation rejects with `Stale`.
    pub fn publish(&mut self, written: Written) -> Result<Option<Removed>> {
        let Written { ticket, location } = written;
        let slot = ticket.slot;
        let cache = ticket.cache;
        let live = self.live(cache);
        self.release_ticket(ticket);
        if !live {
            return Err(Error::Stale);
        }
        let previous = self.entries[slot.0 as usize]
            .stored()
            .then(|| self.unstore(slot));
        self.store(slot, cache.idx, location);
        Ok(previous)
    }

    /// Remove `slot`'s stored record.
    pub fn remove(&mut self, slot: PageSlot) -> Option<Removed> {
        self.entry(slot).ok()?.stored().then(|| self.unstore(slot))
    }

    /// Remove `slot` only if it still points at `location`.
    pub fn remove_if(&mut self, slot: PageSlot, location: &RecordLocation) -> Option<Removed> {
        let entry = self.entry(slot).ok()?;
        (entry.stored() && entry.location() == *location).then(|| self.unstore(slot))
    }

    /// Re-insert a checkpointed record after validating it against `segments`.
    pub fn restore(
        &mut self,
        segments: &Segments,
        slot: PageSlot,
        cache: CacheKey,
        segment: SegmentId,
        generation: Generation,
        extent: Extent,
    ) -> Result<()> {
        let entry = self.entry(slot)?;
        if !self.live(cache) {
            return Err(Error::Stale);
        }
        if entry.flags != 0 || self.occupied >= self.capacity {
            return Err(Error::Busy);
        }
        if segment.0 as usize >= self.segments.len() {
            return Err(Error::Corrupt);
        }
        segments.validate(segment, generation, &extent)?;
        self.store(
            slot,
            cache.idx,
            RecordLocation {
                segment,
                generation,
                extent,
            },
        );
        Ok(())
    }

    /// Segments in the table.
    pub fn segment_count(&self) -> usize {
        self.segments.len()
    }

    /// Grow the segment table to at least `count` segments.
    pub fn ensure_segments(&mut self, count: usize) -> Result<()> {
        if count >= NIL as usize {
            return Err(Error::InvalidConfiguration);
        }
        if self.segments.len() < count {
            self.segments.resize(
                count,
                SegmentMeta {
                    head: NIL,
                    ..SegmentMeta::default()
                },
            );
        }
        Ok(())
    }

    /// Store a record at a location the caller has already validated (a
    /// completed catalog write or a checked checkpoint entry), replacing any
    /// record of `slot`. The segment table grows to cover `segment`. Unlike
    /// [`Self::restore`], `capacity` is not checked: the caller admits pages.
    pub fn place(
        &mut self,
        slot: PageSlot,
        cache: CacheKey,
        segment: SegmentId,
        generation: Generation,
        extent: Extent,
    ) -> Result<Option<Removed>> {
        self.entry(slot)?;
        if !self.live(cache) {
            return Err(Error::Stale);
        }
        let index = usize::try_from(segment.0).map_err(|_| Error::Corrupt)?;
        if u32::try_from(extent.length()).is_err() {
            return Err(Error::Corrupt);
        }
        self.ensure_segments(index.checked_add(1).ok_or(Error::Corrupt)?)
            .map_err(|_| Error::Corrupt)?;
        let previous = self.entries[slot.0 as usize]
            .stored()
            .then(|| self.unstore(slot));
        self.store(
            slot,
            cache.idx,
            RecordLocation {
                segment,
                generation,
                extent,
            },
        );
        Ok(previous)
    }

    // ---- holds ----

    /// Hold `slots` until `until`. Held stored records keep their segments
    /// from eviction; held slots not stored or in flight count as unused.
    pub fn hold(&mut self, slots: &[PageSlot], until: Monotonic, now: Monotonic) -> Result<HoldId> {
        if until <= now {
            return Err(Error::InvalidConfiguration);
        }
        let mut slots = slots.to_vec();
        slots.sort_unstable();
        slots.dedup();
        for slot in &slots {
            if self.entry(*slot)?.holders == u16::MAX {
                return Err(Error::Busy);
            }
        }
        for slot in &slots {
            self.update(*slot, |this| this.entries[slot.0 as usize].holders += 1);
        }
        self.next_hold += 1;
        let id = HoldId(self.next_hold);
        self.expiry.insert((until, id));
        self.holds.insert(id, Hold { until, slots });
        Ok(id)
    }

    /// Move a hold's deadline.
    pub fn extend_hold(&mut self, id: HoldId, until: Monotonic, now: Monotonic) -> Result<()> {
        if until <= now {
            return Err(Error::InvalidConfiguration);
        }
        let hold = self.holds.get_mut(&id).ok_or(Error::Stale)?;
        self.expiry.remove(&(hold.until, id));
        hold.until = until;
        self.expiry.insert((until, id));
        Ok(())
    }

    fn drop_slots(&mut self, slots: &[PageSlot]) {
        for slot in slots {
            self.update(*slot, |this| this.entries[slot.0 as usize].holders -= 1);
        }
    }

    /// Release a whole hold.
    pub fn release_hold(&mut self, id: HoldId) -> Result<()> {
        let hold = self.holds.remove(&id).ok_or(Error::Stale)?;
        self.expiry.remove(&(hold.until, id));
        self.drop_slots(&hold.slots);
        Ok(())
    }

    /// Release some slots of a hold; slots it does not cover are ignored.
    /// An emptied hold is removed.
    pub fn release_hold_slots(&mut self, id: HoldId, slots: &[PageSlot]) -> Result<()> {
        let hold = self.holds.get_mut(&id).ok_or(Error::Stale)?;
        // `hold.slots` is sorted and deduplicated; sort the request too so
        // membership is a binary search instead of a linear scan per slot.
        let sorted;
        let slots = if slots.is_sorted() {
            slots
        } else {
            sorted = {
                let mut copy = slots.to_vec();
                copy.sort_unstable();
                copy
            };
            &sorted
        };
        let mut released = Vec::new();
        if slots.len() < hold.slots.len() / 8 {
            // Few slots: locate each one instead of scanning the whole hold.
            for slot in slots {
                if let Ok(at) = hold.slots.binary_search(slot) {
                    released.push(hold.slots.remove(at));
                }
            }
        } else {
            hold.slots.retain(|slot| {
                let keep = slots.binary_search(slot).is_err();
                if !keep {
                    released.push(*slot);
                }
                keep
            });
        }
        if hold.slots.is_empty() {
            let until = hold.until;
            self.holds.remove(&id);
            self.expiry.remove(&(until, id));
        }
        self.drop_slots(&released);
        Ok(())
    }

    /// Release up to `budget` holds whose deadline is at or before `now`.
    pub fn expire_holds(&mut self, now: Monotonic, budget: usize) -> usize {
        self.expire_holds_with(now, budget, |_, _| {})
    }

    /// [`Self::expire_holds`], reporting each released hold and its slots.
    pub fn expire_holds_with(
        &mut self,
        now: Monotonic,
        budget: usize,
        mut expired_hold: impl FnMut(HoldId, &[PageSlot]),
    ) -> usize {
        let mut expired = 0;
        while expired < budget {
            let Some(&(until, id)) = self.expiry.first() else {
                break;
            };
            if until > now {
                break;
            }
            let hold = self.holds.remove(&id).expect("expiry tracks live holds");
            self.expiry.remove(&(until, id));
            self.drop_slots(&hold.slots);
            expired_hold(id, &hold.slots);
            expired += 1;
        }
        expired
    }

    /// Earliest unswept hold deadline.
    pub fn next_hold_expiry(&self) -> Option<Monotonic> {
        self.expiry.first().map(|(until, _)| *until)
    }

    /// Held slots that are neither stored nor in flight.
    pub fn holds_unused(&self) -> usize {
        self.holds_unused
    }

    /// Whether any hold covers `slot`.
    pub fn is_held(&self, slot: PageSlot) -> bool {
        self.entry(slot).is_ok_and(|entry| entry.holders != 0)
    }

    /// The most recently issued hold identity, live or not.
    pub fn last_hold_id(&self) -> HoldId {
        HoldId(self.next_hold)
    }

    /// Issue only identities newer than `after` from now on, so identities
    /// stay increasing across a rebuilt index.
    pub fn continue_hold_ids(&mut self, after: HoldId) {
        self.next_hold = self.next_hold.max(after.0);
    }

    /// Live holds.
    pub fn hold_count(&self) -> usize {
        self.holds.len()
    }

    /// Whether `slot` is held but neither stored nor in flight, so it counts
    /// toward [`Self::holds_unused`].
    pub fn is_unused_held(&self, slot: PageSlot) -> bool {
        self.entry(slot)
            .is_ok_and(|entry| entry.holders != 0 && entry.flags == 0)
    }

    /// Whether a held record is stored in `segment`.
    pub fn segment_held(&self, segment: SegmentId) -> bool {
        self.segments
            .get(segment.0 as usize)
            .is_some_and(|meta| meta.held != 0)
    }

    /// Slots a live hold covers.
    pub fn hold_slots(&self, id: HoldId) -> Option<&[PageSlot]> {
        self.holds.get(&id).map(|hold| hold.slots.as_slice())
    }

    /// A hold's deadline.
    pub fn hold_deadline(&self, id: HoldId) -> Option<Monotonic> {
        self.holds.get(&id).map(|hold| hold.until)
    }

    // ---- segments and eviction ----

    /// Whether no in-flight write targets `segment`.
    pub fn can_reclaim(&self, segment: SegmentId) -> bool {
        self.segments
            .get(segment.0 as usize)
            .is_some_and(|meta| meta.writes == 0)
    }

    /// Whether `segment` may be evicted: no write in flight and no held record.
    pub fn can_evict(&self, segment: SegmentId) -> bool {
        self.segments
            .get(segment.0 as usize)
            .is_some_and(|meta| meta.writes == 0 && meta.held == 0)
    }

    /// Stored entries in `segment`, most recently stored first. Callers bound
    /// the walk with [`Iterator::take`].
    pub fn segment_entries(
        &self,
        segment: SegmentId,
    ) -> impl Iterator<Item = (PageSlot, EntryView)> + '_ {
        let mut cursor = self
            .segments
            .get(segment.0 as usize)
            .map_or(NIL, |meta| meta.head);
        std::iter::from_fn(move || {
            if cursor == NIL {
                return None;
            }
            let slot = PageSlot(cursor);
            let entry = &self.entries[cursor as usize];
            cursor = entry.seg_next;
            Some((
                slot,
                EntryView {
                    cache: CacheIdx(entry.cache),
                    location: entry.location(),
                    holders: entry.holders,
                },
            ))
        })
    }

    /// Read heat of `segment` (see [`Self::touch`]).
    pub fn segment_heat(&self, segment: SegmentId) -> u32 {
        self.segments
            .get(segment.0 as usize)
            .map_or(0, |meta| meta.heat)
    }

    /// Stored records and bytes in `segment`.
    pub fn segment_usage(&self, segment: SegmentId) -> (u32, u64) {
        self.segments
            .get(segment.0 as usize)
            .map_or((0, 0), |meta| (meta.entries, meta.bytes))
    }

    /// Run the scored segment clock until `free_reserve` segments are free or
    /// the budget is spent. Evicting segments finish first, then sealed ones
    /// with the lowest heat; visiting a segment halves its heat. Removed
    /// entries are reported to `removed`. Returns `Busy` if the reserve is not
    /// met yet (for example, leases still draining).
    pub fn reclaim(
        &mut self,
        segments: &Segments,
        budget: ReclaimBudget,
        removed: impl FnMut(PageSlot, Removed),
    ) -> Result<()> {
        self.reclaim_scored(segments, budget, |_, _, heat| u64::from(heat), removed)
    }

    /// [`Self::reclaim`] with a caller-supplied retention score. `score` sees
    /// the index, the visited segment, and its heat before this visit halves
    /// it; sealed candidates with the lowest score are evicted first. Callers
    /// use this to fold cache fairness into victim choice.
    pub fn reclaim_scored(
        &mut self,
        segments: &Segments,
        budget: ReclaimBudget,
        mut score: impl FnMut(&Self, SegmentId, u32) -> u64,
        mut removed: impl FnMut(PageSlot, Removed),
    ) -> Result<()> {
        if budget.free_reserve == 0 {
            return Ok(());
        }
        let count = segments.count().min(self.segments.len());
        if count == 0 {
            return Err(Error::Unavailable);
        }
        let target = budget.free_reserve.min(count);
        if segments.free_count() >= target && segments.evicting_count() == 0 {
            return Ok(());
        }
        let mut candidates = Vec::new();
        for visit in 0..count.min(budget.max_visits).min(64) {
            let index = self.hand % count;
            self.hand = (index + 1) % count;
            let id = SegmentId(index as u64);
            let state = segments.state(id)?;
            let heat = self.segments[index].heat;
            self.segments[index].heat /= 2;
            let evicting = state == SegmentState::Evicting;
            if !(evicting || state == SegmentState::Sealed) || !self.can_reclaim(id) {
                continue;
            }
            if !evicting && !self.can_evict(id) {
                continue;
            }
            let value = if evicting { 0 } else { score(self, id, heat) };
            candidates.push((!evicting, value, visit, id));
        }
        candidates.sort_unstable_by_key(|(sealed, heat, visit, _)| (*sealed, *heat, *visit));
        let mut entries_left = budget.max_entries;
        for (sealed, _, _, id) in candidates {
            if sealed
                && (segments.free_count() + segments.evicting_count() >= target
                    || !self.can_evict(id))
            {
                continue;
            }
            let index = id.0 as usize;
            if sealed && entries_left == 0 && self.segments[index].entries != 0 {
                continue;
            }
            segments.begin_evict(id)?;
            while entries_left != 0 {
                let head = self.segments[index].head;
                if head == NIL {
                    break;
                }
                let entry = self.unstore(PageSlot(head));
                removed(PageSlot(head), entry);
                entries_left -= 1;
            }
            if self.segments[index].entries == 0 {
                match segments.recycle(id) {
                    Ok(()) | Err(Error::Busy) => {}
                    Err(error) => return Err(error),
                }
                self.segments[index].heat = 0;
            }
        }
        if segments.free_count() >= target {
            Ok(())
        } else {
            Err(Error::Busy)
        }
    }

    /// Recompute every counter from scratch and compare (test support).
    #[cfg(test)]
    pub(crate) fn check_invariants(&self) {
        let mut occupied = 0;
        let mut unused = 0;
        let mut segments = vec![(0u32, 0u64, 0u32); self.segments.len()];
        let mut caches = vec![(0u32, 0u64); self.caches.len()];
        let mut holders = vec![0u16; self.entries.len()];
        for hold in self.holds.values() {
            for slot in &hold.slots {
                holders[slot.0 as usize] += 1;
            }
        }
        for (slot, entry) in self.entries.iter().enumerate() {
            assert_eq!(entry.holders, holders[slot]);
            occupied += usize::from(entry.flags != 0);
            unused += usize::from(entry.holders != 0 && entry.flags == 0);
            if entry.stored() {
                let s = &mut segments[entry.segment as usize];
                s.0 += 1;
                s.1 += u64::from(entry.length);
                s.2 += u32::from(entry.holders != 0);
                let c = &mut caches[usize::from(entry.cache)];
                c.0 += 1;
                c.1 += u64::from(entry.length);
            }
        }
        assert_eq!(occupied, self.occupied);
        assert_eq!(unused, self.holds_unused);
        for (index, meta) in self.segments.iter().enumerate() {
            assert_eq!((meta.entries, meta.bytes, meta.held), segments[index]);
            let mut walked = 0;
            let mut cursor = meta.head;
            while cursor != NIL {
                assert_eq!(self.entries[cursor as usize].segment as usize, index);
                walked += 1;
                cursor = self.entries[cursor as usize].seg_next;
            }
            assert_eq!(walked, meta.entries);
        }
        for (index, meta) in self.caches.iter().enumerate() {
            assert_eq!((meta.entries, meta.bytes), caches[index]);
            let mut walked = 0;
            let mut cursor = meta.head;
            while cursor != NIL {
                assert_eq!(usize::from(self.entries[cursor as usize].cache), index);
                walked += 1;
                cursor = self.entries[cursor as usize].cache_next;
            }
            assert_eq!(walked, meta.entries);
        }
        assert_eq!(self.expiry.len(), self.holds.len());
        let stored = self.entries.iter().filter(|entry| entry.stored()).count();
        let (mut walked, mut prev, mut cursor) = (0, NIL, self.age_head);
        while cursor != NIL {
            let entry = &self.entries[cursor as usize];
            assert!(entry.stored());
            assert_eq!(entry.age_prev, prev);
            walked += 1;
            prev = cursor;
            cursor = entry.age_next;
        }
        assert_eq!((walked, prev), (stored, self.age_tail));
        for cursor in self.cursors {
            assert!(cursor == NIL || self.entries[cursor as usize].stored());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::Alignment;
    use std::collections::HashMap;

    const SEGMENT: u64 = 4096;

    fn segments(count: usize) -> Segments {
        let segments = Segments::new(SEGMENT);
        segments
            .configure(
                SEGMENT * count as u64,
                count,
                Alignment::new(512, 512, 512).unwrap(),
            )
            .unwrap();
        segments
    }

    fn write(
        index: &mut DiskIndex,
        segments: &Segments,
        slot: u32,
        cache: CacheKey,
        length: usize,
    ) -> Result<Option<Removed>> {
        let ticket = index.reserve(PageSlot(slot), cache)?;
        let (lease, writing) =
            index
                .append(segments, ticket, length)
                .map_err(|(ticket, error)| {
                    index.cancel(ticket);
                    error
                })?;
        drop(lease);
        let written = index.write_complete(writing);
        index.publish(written)
    }

    fn walk(index: &mut DiskIndex, cursor: AgeCursor, limit: usize) -> Vec<u32> {
        let slots: Vec<u32> = index
            .age_entries(cursor)
            .take(limit)
            .map(|(slot, _)| slot.0)
            .collect();
        if let Some(&last) = slots.last() {
            index.age_seek(cursor, PageSlot(last)).unwrap();
        }
        slots
    }

    #[test]
    fn age_cursors_resume_across_removal_and_replacement_without_skips() {
        let segments = segments(8);
        let mut index = DiskIndex::new(16, 16, 8).unwrap();
        let cache = index.add_cache(CacheIdx(0)).unwrap();
        for slot in [5, 1, 9, 3] {
            write(&mut index, &segments, slot, cache, 512).unwrap();
        }
        let (a, b) = (AgeCursor(0), AgeCursor(1));
        assert_eq!(walk(&mut index, a, 2), [5, 1]);
        assert_eq!(walk(&mut index, b, 1), [5]);
        // Removing the parked entry resumes at its successor.
        index.remove(PageSlot(1)).unwrap();
        // Replacing an entry moves it to the tail; it is visited again.
        write(&mut index, &segments, 5, cache, 512).unwrap();
        index.check_invariants();
        assert_eq!(walk(&mut index, a, 8), [9, 3, 5]);
        assert_eq!(walk(&mut index, b, 8), [9, 3, 5]);
        // New publications are visited by a finished walk.
        write(&mut index, &segments, 7, cache, 512).unwrap();
        assert_eq!(walk(&mut index, a, 8), [7]);
        // Removing the head and the parked tail restarts or resumes correctly.
        index.remove(PageSlot(7)).unwrap();
        index.remove(PageSlot(9)).unwrap();
        index.check_invariants();
        assert_eq!(walk(&mut index, a, 8), Vec::<u32>::new());
        index.age_reset(b);
        assert_eq!(walk(&mut index, b, 8), [3, 5]);
        assert_eq!(index.age_seek(a, PageSlot(9)), Err(Error::Stale));
        assert_eq!(
            index.age_seek(AgeCursor(AGE_CURSORS as u8), PageSlot(3)),
            Err(Error::InvalidConfiguration)
        );
    }

    #[test]
    fn place_grows_segments_replaces_and_rejects_stale_caches() {
        let mut index = DiskIndex::new(4, 1, 1).unwrap();
        let cache = index.add_cache(CacheIdx(0)).unwrap();
        let extent = Extent::new(0, 512).unwrap();
        assert_eq!(
            index.place(PageSlot(0), cache, SegmentId(3), Generation(1), extent),
            Ok(None)
        );
        // Capacity belongs to the caller; a second slot still places.
        index
            .place(PageSlot(1), cache, SegmentId(0), Generation(1), extent)
            .unwrap();
        let removed = index
            .place(PageSlot(0), cache, SegmentId(2), Generation(4), extent)
            .unwrap()
            .unwrap();
        assert_eq!(removed.location.segment(), SegmentId(3));
        assert_eq!(index.segment_usage(SegmentId(3)), (0, 0));
        assert_eq!(index.segment_usage(SegmentId(2)), (1, 512));
        assert_eq!(index.len(), 2);
        index.check_invariants();
        assert!(index.remove_cache(cache, 8, |_, _| {}).unwrap());
        assert_eq!(
            index.place(PageSlot(0), cache, SegmentId(0), Generation(1), extent),
            Err(Error::Stale)
        );
        assert_eq!(
            index.place(PageSlot(9), cache, SegmentId(0), Generation(1), extent),
            Err(Error::InvalidConfiguration)
        );
    }

    #[test]
    fn entry_fits_target_size() {
        assert!(
            std::mem::size_of::<Entry>() <= 56,
            "{}",
            std::mem::size_of::<Entry>()
        );
    }

    #[test]
    fn rejects_bad_configuration() {
        assert!(DiskIndex::new(u32::MAX, 1, 1).is_err());
        let mut index = DiskIndex::new(1, 1, 1).unwrap();
        assert_eq!(
            index.add_cache(CacheIdx(4096)),
            Err(Error::InvalidConfiguration)
        );
        let cache = index.add_cache(CacheIdx(0)).unwrap();
        assert_eq!(index.add_cache(CacheIdx(0)), Err(Error::Busy));
        assert!(index.reserve(PageSlot(1), cache).is_err());
        assert!(index.get(PageSlot(9)).is_none());
    }

    #[test]
    fn publishes_replaces_and_removes() {
        let segments = segments(4);
        let mut index = DiskIndex::new(8, 8, 4).unwrap();
        let a = index.add_cache(CacheIdx(1)).unwrap();
        let b = index.add_cache(CacheIdx(2)).unwrap();
        assert_eq!(write(&mut index, &segments, 0, a, 512), Ok(None));
        let first = index.get(PageSlot(0)).unwrap();
        assert_eq!(first.cache, CacheIdx(1));
        assert_eq!(first.location.extent().length(), 512);
        assert_eq!(index.cache_bytes(CacheIdx(1)), 512);
        let replaced = write(&mut index, &segments, 0, b, 1024).unwrap().unwrap();
        assert_eq!(replaced.location, first.location);
        assert_eq!(index.cache_bytes(CacheIdx(1)), 0);
        assert_eq!(index.cache_bytes(CacheIdx(2)), 1024);
        assert_eq!(index.len(), 1);
        let location = index.get(PageSlot(0)).unwrap().location;
        assert!(index.remove_if(PageSlot(0), &first.location).is_none());
        assert!(index.remove_if(PageSlot(0), &location).is_some());
        assert!(index.remove(PageSlot(0)).is_none());
        assert!(index.is_empty());
        index.check_invariants();
    }

    #[test]
    fn capacity_and_duplicate_reservations() {
        let segments = segments(4);
        let mut index = DiskIndex::new(8, 2, 4).unwrap();
        let cache = index.add_cache(CacheIdx(0)).unwrap();
        write(&mut index, &segments, 0, cache, 512).unwrap();
        let ticket = index.reserve(PageSlot(1), cache).unwrap();
        assert_eq!(index.reserve(PageSlot(1), cache).unwrap_err(), Error::Busy);
        assert_eq!(index.reserve(PageSlot(2), cache).unwrap_err(), Error::Busy);
        // Re-publishing a stored slot needs no new capacity.
        let again = index.reserve(PageSlot(0), cache).unwrap();
        index.cancel(again);
        index.cancel(ticket);
        assert_eq!(index.len(), 1);
        index.check_invariants();
    }

    #[test]
    fn replacement_is_transactional() {
        let segments = segments(4);
        let mut index = DiskIndex::new(8, 1, 4).unwrap();
        let cache = index.add_cache(CacheIdx(0)).unwrap();
        write(&mut index, &segments, 0, cache, 512).unwrap();
        let hold = index
            .hold(&[PageSlot(0)], Monotonic(10), Monotonic(0))
            .unwrap();
        assert_eq!(
            index
                .reserve_replacing(PageSlot(1), cache, PageSlot(0))
                .unwrap_err(),
            Error::Busy
        );
        assert!(index.get(PageSlot(0)).is_some(), "held victim untouched");
        index.release_hold(hold).unwrap();
        let stale = CacheKey {
            idx: CacheIdx(0),
            generation: 7,
        };
        assert_eq!(
            index
                .reserve_replacing(PageSlot(1), stale, PageSlot(0))
                .unwrap_err(),
            Error::Stale
        );
        assert!(
            index.get(PageSlot(0)).is_some(),
            "invalid reservation keeps victim"
        );
        assert!(
            index
                .reserve_replacing(PageSlot(1), cache, PageSlot(1))
                .is_err()
        );
        let (ticket, removed) = index
            .reserve_replacing(PageSlot(1), cache, PageSlot(0))
            .unwrap();
        assert_eq!(removed.cache, CacheIdx(0));
        assert!(index.get(PageSlot(0)).is_none());
        assert!(index.is_inflight(PageSlot(1)));
        index.cancel(ticket);
        index.check_invariants();
    }

    #[test]
    fn cache_generations_fence_reuse() {
        let segments = segments(4);
        let mut index = DiskIndex::new(8, 8, 4).unwrap();
        let old = index.add_cache(CacheIdx(3)).unwrap();
        write(&mut index, &segments, 0, old, 512).unwrap();
        write(&mut index, &segments, 1, old, 512).unwrap();
        let ticket = index.reserve(PageSlot(2), old).unwrap();
        let (lease, writing) = index.append(&segments, ticket, 512).unwrap();
        let mut removed = Vec::new();
        assert_eq!(
            index.remove_cache(old, 1, |slot, _| removed.push(slot)),
            Ok(false)
        );
        assert_eq!(removed.len(), 1);
        assert!(
            index.get(PageSlot(0)).is_none() && index.get(PageSlot(1)).is_none(),
            "draining hides entries"
        );
        assert_eq!(index.add_cache(CacheIdx(3)), Err(Error::Busy));
        assert_eq!(
            index.remove_cache(old, 8, |slot, _| removed.push(slot)),
            Ok(false),
            "in-flight publication"
        );
        let written = index.write_complete(writing);
        drop(lease);
        assert_eq!(index.publish(written), Err(Error::Stale));
        assert_eq!(index.cache_bytes(CacheIdx(3)), 0);
        let new = index.add_cache(CacheIdx(3)).unwrap();
        assert_ne!(new, old);
        assert_eq!(index.reserve(PageSlot(4), old).unwrap_err(), Error::Stale);
        assert_eq!(index.remove_cache(old, 1, |_, _| {}), Err(Error::Stale));
        write(&mut index, &segments, 4, new, 512).unwrap();
        index.check_invariants();
    }

    #[test]
    fn holds_track_unused_and_block_eviction() {
        let segments = segments(4);
        let mut index = DiskIndex::new(8, 8, 4).unwrap();
        let cache = index.add_cache(CacheIdx(0)).unwrap();
        assert_eq!(
            index.hold(&[PageSlot(0)], Monotonic(5), Monotonic(5)),
            Err(Error::InvalidConfiguration)
        );
        let hold = index
            .hold(
                &[PageSlot(0), PageSlot(1), PageSlot(1), PageSlot(2)],
                Monotonic(100),
                Monotonic(0),
            )
            .unwrap();
        assert_eq!(index.holds_unused(), 3);
        let ticket = index.reserve(PageSlot(0), cache).unwrap();
        assert_eq!(index.holds_unused(), 2, "in flight is not unused");
        let (lease, writing) = index.append(&segments, ticket, 512).unwrap();
        let segment = writing.location().segment();
        assert!(!index.can_evict(segment) && !index.can_reclaim(segment));
        let written = index.write_complete(writing);
        drop(lease);
        index.publish(written).unwrap();
        assert!(!index.can_evict(segment), "held record");
        assert!(index.can_reclaim(segment));
        assert_eq!(index.get(PageSlot(0)).unwrap().holders, 1);
        index
            .release_hold_slots(hold, &[PageSlot(0), PageSlot(7)])
            .unwrap();
        assert!(index.can_evict(segment));
        assert_eq!(index.holds_unused(), 2);
        index
            .extend_hold(hold, Monotonic(200), Monotonic(50))
            .unwrap();
        assert_eq!(
            index.extend_hold(hold, Monotonic(50), Monotonic(50)),
            Err(Error::InvalidConfiguration)
        );
        assert_eq!(index.next_hold_expiry(), Some(Monotonic(200)));
        assert_eq!(index.expire_holds(Monotonic(199), 8), 0);
        assert_eq!(index.expire_holds(Monotonic(200), 8), 1);
        assert_eq!(index.holds_unused(), 0);
        assert_eq!(index.release_hold(hold), Err(Error::Stale));
        index.check_invariants();
    }

    #[test]
    fn release_hold_slots_batches_unsorted_and_sparse_requests() {
        let mut index = DiskIndex::new(64, 64, 4).unwrap();
        let all: Vec<PageSlot> = (0..32).map(PageSlot).collect();
        let hold = index.hold(&all, Monotonic(100), Monotonic(0)).unwrap();
        assert_eq!(index.holds_unused(), 32);
        // Sparse request (binary search path), unsorted, with duplicates and
        // slots the hold does not cover.
        index
            .release_hold_slots(hold, &[PageSlot(9), PageSlot(2), PageSlot(9), PageSlot(40)])
            .unwrap();
        assert_eq!(index.holds_unused(), 30);
        assert!(!index.is_held(PageSlot(2)));
        assert!(!index.is_held(PageSlot(9)));
        assert!(index.is_held(PageSlot(3)));
        let left = index.hold_slots(hold).unwrap();
        assert_eq!(left.len(), 30);
        assert!(left.is_sorted());
        // Dense unsorted request (retain path) leaves one slot.
        let mut rest: Vec<PageSlot> = left.iter().copied().filter(|s| s.0 != 31).collect();
        rest.reverse();
        index.release_hold_slots(hold, &rest).unwrap();
        assert_eq!(index.hold_slots(hold), Some(&[PageSlot(31)][..]));
        assert_eq!(index.holds_unused(), 1);
        // Emptying the hold removes it and its expiry entry.
        index.release_hold_slots(hold, &[PageSlot(31)]).unwrap();
        assert_eq!(index.hold_slots(hold), None);
        assert_eq!(index.next_hold_expiry(), None);
        assert_eq!(index.holds_unused(), 0);
        assert_eq!(
            index.release_hold_slots(hold, &[PageSlot(31)]),
            Err(Error::Stale)
        );
        index.check_invariants();
    }

    #[test]
    fn hold_queries_and_reported_expiry() {
        let segments = segments(2);
        let mut index = DiskIndex::new(8, 8, 2).unwrap();
        let cache = index.add_cache(CacheIdx(0)).unwrap();
        assert_eq!(index.last_hold_id(), HoldId::default());
        let first = index
            .hold(&[PageSlot(0), PageSlot(1)], Monotonic(10), Monotonic(0))
            .unwrap();
        assert!(first > HoldId::default());
        assert_eq!(index.last_hold_id(), first);
        assert!(index.is_unused_held(PageSlot(0)) && !index.is_unused_held(PageSlot(2)));
        write(&mut index, &segments, 0, cache, 512).unwrap();
        let segment = index.get(PageSlot(0)).unwrap().location.segment();
        assert!(!index.is_unused_held(PageSlot(0)) && index.segment_held(segment));
        assert_eq!(index.segment_heat(segment), 0);
        index.touch(PageSlot(0));
        index.touch(PageSlot(0));
        assert_eq!(index.segment_heat(segment), 2);
        let second = index
            .hold(&[PageSlot(3)], Monotonic(20), Monotonic(0))
            .unwrap();
        assert!(second > first && index.hold_count() == 2);
        assert_eq!(index.hold_slots(second), Some(&[PageSlot(3)][..]));
        let mut expired = Vec::new();
        assert_eq!(
            index.expire_holds_with(Monotonic(15), 8, |id, slots| {
                expired.push((id, slots.to_vec()));
            }),
            1
        );
        assert_eq!(expired, vec![(first, vec![PageSlot(0), PageSlot(1)])]);
        assert!(!index.segment_held(segment) && index.hold_count() == 1);
        assert_eq!(index.last_hold_id(), second);
        index.continue_hold_ids(HoldId(100));
        index.continue_hold_ids(HoldId(50));
        let third = index
            .hold(&[PageSlot(4)], Monotonic(30), Monotonic(0))
            .unwrap();
        assert_eq!(third, HoldId(101));
        index.check_invariants();
    }

    #[test]
    fn expiry_is_bounded() {
        let mut index = DiskIndex::new(8, 8, 1).unwrap();
        for at in 1..=5 {
            index
                .hold(&[PageSlot(at)], Monotonic(at.into()), Monotonic(0))
                .unwrap();
        }
        assert_eq!(index.expire_holds(Monotonic(10), 2), 2);
        assert_eq!(index.next_hold_expiry(), Some(Monotonic(3)));
        assert_eq!(index.expire_holds(Monotonic(10), 10), 3);
        index.check_invariants();
    }

    #[test]
    fn failed_write_releases_reservation() {
        let segments = segments(2);
        let mut index = DiskIndex::new(8, 8, 2).unwrap();
        let cache = index.add_cache(CacheIdx(0)).unwrap();
        let ticket = index.reserve(PageSlot(0), cache).unwrap();
        let Err((ticket, _)) = index.append(&segments, ticket, 100) else {
            panic!("unaligned length");
        };
        let (lease, writing) = index.append(&segments, ticket, 512).unwrap();
        index.write_failed(writing);
        drop(lease);
        assert!(index.is_empty() && !index.is_inflight(PageSlot(0)));
        index.check_invariants();
    }

    #[test]
    fn clock_evicts_coldest_unheld_segment() {
        let segments = segments(4);
        let mut index = DiskIndex::new(16, 16, 4).unwrap();
        let cache = index.add_cache(CacheIdx(0)).unwrap();
        // Fill three segments with two 2 KiB records each; the fourth stays open.
        for slot in 0..7 {
            write(&mut index, &segments, slot, cache, 2048).unwrap();
        }
        let seg = |index: &DiskIndex, slot| index.get(PageSlot(slot)).unwrap().location.segment();
        let hot = seg(&index, 0);
        let held = seg(&index, 2);
        let cold = seg(&index, 4);
        assert!(hot != held && held != cold);
        for _ in 0..10 {
            index.touch(PageSlot(0));
        }
        index
            .hold(&[PageSlot(2)], Monotonic(10), Monotonic(0))
            .unwrap();
        assert_eq!(segments.free_count(), 0);
        let mut removed = Vec::new();
        let budget = ReclaimBudget {
            free_reserve: 1,
            max_visits: 64,
            max_entries: 64,
        };
        index
            .reclaim(&segments, budget, |slot, _| removed.push(slot))
            .unwrap();
        removed.sort();
        assert_eq!(removed, [PageSlot(4), PageSlot(5)]);
        assert_eq!(segments.state(cold).unwrap(), SegmentState::Free);
        assert_eq!(index.segment_usage(cold), (0, 0));
        assert!(index.get(PageSlot(0)).is_some() && index.get(PageSlot(2)).is_some());
        // Already satisfied.
        index
            .reclaim(&segments, budget, |_, _| panic!("nothing to evict"))
            .unwrap();
        let none = ReclaimBudget {
            free_reserve: 0,
            ..budget
        };
        index
            .reclaim(&segments, none, |_, _| panic!("no reserve"))
            .unwrap();
        index.check_invariants();
    }

    #[test]
    fn scored_reclaim_follows_caller_score_and_walks_segments() {
        let segments = segments(4);
        let mut index = DiskIndex::new(16, 16, 4).unwrap();
        let fair = index.add_cache(CacheIdx(0)).unwrap();
        let over = index.add_cache(CacheIdx(1)).unwrap();
        // Segment of slots 0,1 belongs to `over`; 2..5 to `fair`.
        for slot in 0..7 {
            let cache = if slot < 2 { over } else { fair };
            write(&mut index, &segments, slot, cache, 2048).unwrap();
        }
        let seg = |index: &DiskIndex, slot| index.get(PageSlot(slot)).unwrap().location.segment();
        let target = seg(&index, 0);
        // The over-share segment is the hottest; heat alone would spare it.
        for _ in 0..10 {
            index.touch(PageSlot(0));
        }
        let mut walked: Vec<_> = index
            .segment_entries(target)
            .map(|(slot, view)| (slot, view.cache))
            .collect();
        walked.sort();
        assert_eq!(
            walked,
            [(PageSlot(0), CacheIdx(1)), (PageSlot(1), CacheIdx(1))]
        );
        assert_eq!(index.segment_entries(target).take(1).count(), 1);
        assert_eq!(index.segment_entries(SegmentId(99)).count(), 0);
        let budget = ReclaimBudget {
            free_reserve: 1,
            max_visits: 64,
            max_entries: 64,
        };
        let mut scored = Vec::new();
        let mut removed = Vec::new();
        index
            .reclaim_scored(
                &segments,
                budget,
                |index, segment, heat| {
                    scored.push((segment, heat));
                    let over_share = index
                        .segment_entries(segment)
                        .any(|(_, view)| view.cache == CacheIdx(1));
                    if over_share { 0 } else { 1 + u64::from(heat) }
                },
                |slot, _| removed.push(slot),
            )
            .unwrap();
        removed.sort();
        assert_eq!(removed, [PageSlot(0), PageSlot(1)]);
        assert!(scored.contains(&(target, 10)), "score sees pre-visit heat");
        assert_eq!(segments.state(target).unwrap(), SegmentState::Free);
        index.check_invariants();
    }

    #[test]
    fn eviction_waits_for_leases_and_budget() {
        let segments = segments(2);
        let mut index = DiskIndex::new(16, 16, 2).unwrap();
        let cache = index.add_cache(CacheIdx(0)).unwrap();
        for slot in 0..2 {
            write(&mut index, &segments, slot, cache, 2048).unwrap();
        }
        // Seal segment 0 by rolling over into segment 1, keeping a read lease.
        write(&mut index, &segments, 2, cache, 2048).unwrap();
        let sealed = index.get(PageSlot(0)).unwrap().location;
        let lease = segments
            .lease(sealed.segment(), sealed.generation())
            .unwrap();
        let tight = ReclaimBudget {
            free_reserve: 1,
            max_visits: 64,
            max_entries: 1,
        };
        assert_eq!(index.reclaim(&segments, tight, |_, _| {}), Err(Error::Busy));
        assert_eq!(
            segments.state(sealed.segment()).unwrap(),
            SegmentState::Evicting
        );
        assert_eq!(
            index.reclaim(&segments, tight, |_, _| {}),
            Err(Error::Busy),
            "lease drains"
        );
        drop(lease);
        index.reclaim(&segments, tight, |_, _| {}).unwrap();
        assert_eq!(
            segments.state(sealed.segment()).unwrap(),
            SegmentState::Free
        );
        index.check_invariants();
    }

    #[test]
    fn restore_validates_against_segments() {
        let segments = segments(2);
        let mut index = DiskIndex::new(8, 8, 2).unwrap();
        let cache = index.add_cache(CacheIdx(0)).unwrap();
        write(&mut index, &segments, 0, cache, 512).unwrap();
        let location = index.remove(PageSlot(0)).unwrap().location;
        let (id, generation, extent) =
            (location.segment(), location.generation(), location.extent());
        assert!(
            index
                .restore(
                    &segments,
                    PageSlot(1),
                    cache,
                    id,
                    Generation(generation.0 + 1),
                    extent
                )
                .is_err()
        );
        assert_eq!(
            index.restore(
                &segments,
                PageSlot(1),
                cache,
                SegmentId(9),
                generation,
                extent
            ),
            Err(Error::Corrupt)
        );
        index
            .restore(&segments, PageSlot(1), cache, id, generation, extent)
            .unwrap();
        assert_eq!(index.get(PageSlot(1)).unwrap().location, location);
        assert_eq!(
            index.restore(&segments, PageSlot(1), cache, id, generation, extent),
            Err(Error::Busy)
        );
        assert_eq!(index.iter().count(), 1);
        index.check_invariants();
    }

    /// Small deterministic PRNG (xorshift64*).
    struct Rng(u64);

    impl Rng {
        fn next(&mut self) -> u64 {
            self.0 ^= self.0 >> 12;
            self.0 ^= self.0 << 25;
            self.0 ^= self.0 >> 27;
            self.0.wrapping_mul(0x2545_F491_4F6C_DD1D)
        }

        fn below(&mut self, n: usize) -> usize {
            (self.next() % n as u64) as usize
        }
    }

    /// Reference model kept with plain maps.
    #[derive(Default)]
    struct Reference {
        stored: HashMap<u32, (u16, RecordLocation)>,
        inflight: HashMap<u32, u16>,
        holds: BTreeMap<HoldId, (Monotonic, BTreeSet<u32>)>,
        writes: HashMap<u64, u32>,
        live: HashMap<u16, CacheKey>,
        draining: HashMap<u16, CacheKey>,
    }

    impl Reference {
        fn held(&self, slot: u32) -> u16 {
            self.holds
                .values()
                .filter(|(_, slots)| slots.contains(&slot))
                .count() as u16
        }

        fn check(&self, index: &DiskIndex, slots: u32, segments: u64) {
            index.check_invariants();
            for slot in 0..slots {
                let expected = self
                    .stored
                    .get(&slot)
                    .filter(|(cache, _)| self.live.contains_key(cache))
                    .map(|(cache, location)| EntryView {
                        cache: CacheIdx(*cache),
                        location: *location,
                        holders: self.held(slot),
                    });
                assert_eq!(index.get(PageSlot(slot)), expected, "slot {slot}");
                assert_eq!(
                    index.is_inflight(PageSlot(slot)),
                    self.inflight.contains_key(&slot)
                );
            }
            let unused = (0..slots)
                .filter(|slot| {
                    self.held(*slot) != 0
                        && !self.stored.contains_key(slot)
                        && !self.inflight.contains_key(slot)
                })
                .count();
            assert_eq!(index.holds_unused(), unused);
            for segment in 0..segments {
                let held = self.stored.iter().any(|(slot, (_, location))| {
                    location.segment().0 == segment && self.held(*slot) != 0
                });
                let writing = self.writes.get(&segment).copied().unwrap_or(0) != 0;
                assert_eq!(
                    index.can_evict(SegmentId(segment)),
                    !held && !writing,
                    "segment {segment}"
                );
            }
            for cache in 0..4u16 {
                let bytes: u64 = self
                    .stored
                    .values()
                    .filter(|(c, _)| *c == cache)
                    .map(|(_, location)| location.extent().length() as u64)
                    .sum();
                assert_eq!(index.cache_bytes(CacheIdx(cache)), bytes);
            }
            let occupied = self
                .stored
                .keys()
                .chain(self.inflight.keys())
                .collect::<BTreeSet<_>>()
                .len();
            assert_eq!(index.len(), occupied);
        }
    }

    fn run_model(seed: u64, steps: usize, seen: &mut [usize; 6]) {
        const SLOTS: u32 = 24;
        const CAPACITY: usize = 16;
        const SEGMENTS: usize = 6;
        let segments = segments(SEGMENTS);
        let mut index = DiskIndex::new(SLOTS, CAPACITY, SEGMENTS).unwrap();
        let mut model = Reference::default();
        let mut rng = Rng(seed | 1);
        let mut now = Monotonic(0);
        let mut tickets: Vec<PageTicket> = Vec::new();
        let mut writing: Vec<(SegmentLease, Writing)> = Vec::new();
        for cache in 0..3u16 {
            model
                .live
                .insert(cache, index.add_cache(CacheIdx(cache)).unwrap());
        }
        for _ in 0..steps {
            match rng.below(14) {
                0 | 1 => {
                    let slot = rng.below(SLOTS as usize) as u32;
                    let cache = rng.below(4) as u16;
                    let key = model
                        .live
                        .get(&cache)
                        .or(model.draining.get(&cache))
                        .copied()
                        .unwrap_or(CacheKey {
                            idx: CacheIdx(cache),
                            generation: 99,
                        });
                    let occupied = model
                        .stored
                        .keys()
                        .chain(model.inflight.keys())
                        .collect::<BTreeSet<_>>()
                        .len();
                    let expected = if !model.live.contains_key(&cache) || model.live[&cache] != key
                    {
                        Err(Error::Stale)
                    } else if model.inflight.contains_key(&slot)
                        || (!model.stored.contains_key(&slot) && occupied >= CAPACITY)
                    {
                        Err(Error::Busy)
                    } else {
                        Ok(())
                    };
                    match index.reserve(PageSlot(slot), key) {
                        Ok(ticket) => {
                            assert_eq!(expected, Ok(()));
                            model.inflight.insert(slot, cache);
                            tickets.push(ticket);
                        }
                        Err(error) => assert_eq!(expected, Err(error)),
                    }
                }
                2 if !tickets.is_empty() => {
                    let ticket = tickets.swap_remove(rng.below(tickets.len()));
                    let length = 512 * (1 + rng.below(4));
                    match index.append(&segments, ticket, length) {
                        Ok((lease, w)) => {
                            *model.writes.entry(w.location().segment().0).or_default() += 1;
                            writing.push((lease, w));
                        }
                        Err((ticket, _)) => tickets.push(ticket),
                    }
                }
                3 | 4 if !writing.is_empty() => {
                    let (lease, w) = writing.swap_remove(rng.below(writing.len()));
                    let location = w.location();
                    let slot = w.ticket.slot.0;
                    *model.writes.get_mut(&location.segment().0).unwrap() -= 1;
                    let cache = model.inflight.remove(&slot).unwrap();
                    let written = index.write_complete(w);
                    drop(lease);
                    match index.publish(written) {
                        Ok(previous) => {
                            seen[0] += 1;
                            let old = model.stored.insert(slot, (cache, location));
                            assert_eq!(previous.map(|r| (r.cache.0, r.location)), old);
                        }
                        Err(error) => {
                            seen[5] += 1;
                            assert_eq!(error, Error::Stale);
                            assert!(!model.live.contains_key(&cache));
                        }
                    }
                }
                5 if !writing.is_empty() => {
                    let (lease, w) = writing.swap_remove(rng.below(writing.len()));
                    *model.writes.get_mut(&w.location().segment().0).unwrap() -= 1;
                    model.inflight.remove(&w.ticket.slot.0);
                    index.write_failed(w);
                    drop(lease);
                }
                6 if !tickets.is_empty() => {
                    let ticket = tickets.swap_remove(rng.below(tickets.len()));
                    model.inflight.remove(&ticket.slot.0);
                    index.cancel(ticket);
                }
                7 => {
                    let slot = rng.below(SLOTS as usize) as u32;
                    let removed = index.remove(PageSlot(slot));
                    let expected = model.stored.remove(&slot);
                    assert_eq!(removed.map(|r| (r.cache.0, r.location)), expected);
                }
                8 => {
                    let slots: Vec<PageSlot> = (0..1 + rng.below(4))
                        .map(|_| PageSlot(rng.below(SLOTS as usize) as u32))
                        .collect();
                    let until = Monotonic(now.0 + 1 + rng.below(50) as u64);
                    let id = index.hold(&slots, until, now).unwrap();
                    model
                        .holds
                        .insert(id, (until, slots.iter().map(|s| s.0).collect()));
                }
                9 if !model.holds.is_empty() => {
                    let id = *model
                        .holds
                        .keys()
                        .nth(rng.below(model.holds.len()))
                        .unwrap();
                    if rng.below(2) == 0 {
                        index.release_hold(id).unwrap();
                        model.holds.remove(&id);
                    } else {
                        let slot = rng.below(SLOTS as usize) as u32;
                        index.release_hold_slots(id, &[PageSlot(slot)]).unwrap();
                        let slots = &mut model.holds.get_mut(&id).unwrap().1;
                        slots.remove(&slot);
                        if slots.is_empty() {
                            model.holds.remove(&id);
                        }
                    }
                }
                10 => {
                    now = Monotonic(now.0 + rng.below(20) as u64);
                    let budget = 1 + rng.below(3);
                    let expired = index.expire_holds(now, budget);
                    let due: Vec<_> = model
                        .holds
                        .iter()
                        .filter(|(_, (until, _))| *until <= now)
                        .map(|(id, (until, _))| (*until, *id))
                        .collect::<BTreeSet<_>>()
                        .into_iter()
                        .take(budget)
                        .collect();
                    assert_eq!(expired, due.len());
                    seen[1] += expired;
                    for (_, id) in due {
                        model.holds.remove(&id);
                    }
                }
                11 => {
                    index.touch(PageSlot(rng.below(SLOTS as usize) as u32));
                    let budget = ReclaimBudget {
                        free_reserve: 1 + rng.below(3),
                        max_visits: 1 + rng.below(8),
                        max_entries: rng.below(6),
                    };
                    let mut removed = Vec::new();
                    let _ = index.reclaim(&segments, budget, |slot, r| removed.push((slot, r)));
                    seen[2] += removed.len();
                    for (slot, r) in removed {
                        assert_eq!(model.held(slot.0), 0, "held entry evicted");
                        let expected = model.stored.remove(&slot.0).unwrap();
                        assert_eq!((r.cache.0, r.location), expected);
                    }
                }
                12 => {
                    let cache = rng.below(4) as u16;
                    if let Some(key) = model
                        .live
                        .remove(&cache)
                        .or_else(|| model.draining.remove(&cache))
                    {
                        let mut removed = Vec::new();
                        let done = index
                            .remove_cache(key, rng.below(4), |slot, r| removed.push((slot, r)))
                            .unwrap();
                        for (slot, r) in removed {
                            assert_eq!(r.cache.0, cache);
                            assert_eq!(
                                model.stored.remove(&slot.0).map(|(_, l)| l),
                                Some(r.location)
                            );
                        }
                        let remaining = model.stored.values().any(|(c, _)| *c == cache)
                            || model.inflight.values().any(|c| *c == cache);
                        assert_eq!(done, !remaining);
                        seen[3] += usize::from(done);
                        if !done {
                            model.draining.insert(cache, key);
                        }
                    } else {
                        let key = index.add_cache(CacheIdx(cache)).unwrap();
                        model.live.insert(cache, key);
                    }
                }
                13 => {
                    let slot = rng.below(SLOTS as usize) as u32;
                    let victim = rng.below(SLOTS as usize) as u32;
                    if let Some(&key) = model.live.get(&0)
                        && let Ok((ticket, removed)) =
                            index.reserve_replacing(PageSlot(slot), key, PageSlot(victim))
                    {
                        seen[4] += 1;
                        assert_eq!(model.held(victim), 0);
                        assert!(!model.inflight.contains_key(&slot));
                        let expected = model.stored.remove(&victim).unwrap();
                        assert_eq!((removed.cache.0, removed.location), expected);
                        model.inflight.insert(slot, 0);
                        tickets.push(ticket);
                    }
                }
                _ => {}
            }
            // Draining caches settle once their last entry or publication is gone.
            model.draining.retain(|cache, _| {
                model.stored.values().any(|(c, _)| c == cache)
                    || model.inflight.values().any(|c| c == cache)
            });
            model.check(&index, SLOTS, SEGMENTS as u64);
        }
        for ticket in tickets {
            index.cancel(ticket);
        }
        for (lease, w) in writing {
            index.write_failed(w);
            drop(lease);
        }
        index.check_invariants();
    }

    #[test]
    fn randomized_against_reference() {
        let steps = if cfg!(miri) { 200 } else { 3000 };
        let seeds = if cfg!(miri) { 2 } else { 64 };
        let mut seen = [0; 6];
        for seed in 0..seeds {
            run_model(0x9E37_79B9_7F4A_7C15 ^ seed, steps, &mut seen);
        }
        // Published, expired, evicted, caches removed, replaced, stale publications.
        assert!(cfg!(miri) || seen.iter().all(|n| *n > 0), "{seen:?}");
    }
}
