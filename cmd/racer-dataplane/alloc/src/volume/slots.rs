//! In-memory slot table for the extent volume.
//!
//! The table is pure and single threaded. It tracks which slots are free, node
//! slots, or data slots, which units inside them are in use, the index keys
//! stored in each data slot, leases, CLOCK reference bits, and space whose
//! release waits for the reuse rule.
//!
//! # Reuse rule
//!
//! Space that a durable superblock may still reference must not be reused.
//! The table keeps an epoch counter. [`Slots::epoch`] is the tag for anything
//! freed now: it is the epoch of the next commit attempt, the first attempt
//! whose tree no longer references the freed address. [`Slots::begin_commit`]
//! returns that epoch and advances the counter, so frees made while the
//! attempt runs are tagged with the following epoch.
//!
//! [`Slots::commit_succeeded`] releases every pending address tagged at or
//! before the previous successful commit, then remembers this commit. After
//! that, neither on-disk superblock (the one just written and the one before
//! it) references released space. [`Slots::commit_failed`] changes nothing:
//! the pending space stays pending until two later attempts succeed.
//!
//! Tree nodes and storage records use the rule. Cache records are released at
//! once, because a stale cache entry fails its generation check.
//!
//! # Memory
//!
//! Each slot costs a 24 byte entry plus a 4 byte lease count. Slots with more
//! than one record (and node slots) also own a free bitmap and a list of
//! `(record, key)` pairs (26 bytes per record). Whole-slot records keep their
//! single key in a shared side table (24 bytes).

use std::cell::Cell;
use std::collections::BTreeSet;
use std::fmt;
use std::rc::Rc;

use super::format::{
    CLASSES, Key, MAX_SLOTS, META_FIRST_NODE_UNIT, META_SLOT, MIN_SLOTS, NodeAddr, SLOT_UNITS,
    Tier, UNIT_BYTES, Value, WHOLE_SLOT_CLASS, class_records, class_units,
};
use crate::{Error, Result};

const NONE: u32 = u32::MAX;
const CLASS_COUNT: usize = CLASSES.len();

const KIND_FREE: u8 = 0;
const KIND_NODE: u8 = 1;
const KIND_DATA: u8 = 2;

const OPEN: u8 = 1;
const REFERENCED: u8 = 2;
const EVICTING: u8 = 4;
const LISTED: u8 = 8;
const WHOLE_COMMITTED: u8 = 16;

/// Allocation limits.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Limits {
    /// Most data slots in the storage tier.
    pub storage_slots: u64,

    /// Most bytes held by records of classes below the whole slot, both tiers.
    /// Counts every unit in use, including units waiting for the reuse rule.
    pub small_bytes: u64,
}

impl Limits {
    /// No limits.
    pub const UNLIMITED: Self = Self {
        storage_slots: u64::MAX,
        small_bytes: u64::MAX,
    };
}

/// Occupancy summary.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct Usage {
    /// Slots in the volume, including the meta slot.
    pub slot_count: u64,

    /// Free slots.
    pub free_slots: u64,

    /// Node slots, not counting the meta slot.
    pub node_slots: u64,

    /// Cache data slots.
    pub cache_slots: u64,

    /// Storage data slots.
    pub storage_slots: u64,

    /// Bytes in use by records of classes below the whole slot.
    pub small_bytes: u64,

    /// Node and storage units waiting for the reuse rule.
    pub pending_units: u64,

    /// Node units in use, including pending ones and the meta slot's.
    pub node_units: u64,

    /// Records holding an index entry.
    pub records: u64,
}

/// Kind of one slot.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SlotKind {
    /// Not in use.
    Free,

    /// Holds tree nodes (the meta slot is always a node slot).
    Node,

    /// Holds records of one tier and class.
    Data {
        /// Tier of every record.
        tier: Tier,

        /// Class index of every record.
        class: u8,
    },
}

/// State of one slot, for tests and diagnostics.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SlotState {
    /// Kind.
    pub kind: SlotKind,

    /// Records (or node units) in use: reserved, indexed, or pending.
    pub used: u32,

    /// Records holding an index entry.
    pub indexed: u32,

    /// Open for allocation.
    pub open: bool,

    /// CLOCK reference bit.
    pub referenced: bool,

    /// Returned by [`Slots::evict_candidate`] and not yet finished.
    pub evicting: bool,

    /// Live leases.
    pub leases: u32,

    /// Largest `retain_until` of the slot's records.
    pub retain_until: u64,
}

/// RAII pin on one slot. Eviction skips leased slots.
///
/// The lease owns a reference to the shared counts, so it is `'static` and can
/// guard I/O.
pub struct SlotLease {
    counts: Rc<[Cell<u32>]>,
    slot: u32,
}

impl SlotLease {
    fn new(counts: &Rc<[Cell<u32>]>, slot: u32) -> Self {
        let count = &counts[slot as usize];
        count.set(count.get() + 1);
        Self {
            counts: Rc::clone(counts),
            slot,
        }
    }

    /// Leased slot.
    pub fn slot(&self) -> u32 {
        self.slot
    }
}

impl Clone for SlotLease {
    fn clone(&self) -> Self {
        Self::new(&self.counts, self.slot)
    }
}

impl Drop for SlotLease {
    fn drop(&mut self) {
        let count = &self.counts[self.slot as usize];
        count.set(count.get() - 1);
    }
}

impl fmt::Debug for SlotLease {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("SlotLease")
            .field("slot", &self.slot)
            .finish()
    }
}

/// A reserved record from [`Slots::allocate`].
#[derive(Debug)]
pub struct Allocation {
    /// Data slot.
    pub slot: u32,

    /// First unit of the record.
    pub unit: u16,

    /// Class index.
    pub class: u8,

    /// Lease on the slot, held until the record is indexed or aborted.
    pub lease: SlotLease,
}

/// A slot chosen for eviction.
#[derive(Debug, Eq, PartialEq)]
pub struct Eviction {
    /// Slot to evict.
    pub slot: u32,

    /// Index keys stored in the slot. The caller removes them from the tree.
    pub keys: Vec<Key>,
}

/// Per-slot entry. Kept at 24 bytes.
#[derive(Clone, Copy)]
struct Slot {
    retain_until: u64,
    // Index into `records` (node and multi-record slots) or `whole` (indexed
    // whole-slot records), or NONE.
    aux: u32,
    used: u16,
    kind: u8,
    tier: u8,
    class: u8,
    flags: u8,
}

impl Slot {
    const FREE: Self = Self {
        retain_until: 0,
        aux: NONE,
        used: 0,
        kind: KIND_FREE,
        tier: 0,
        class: 0,
        flags: 0,
    };

    fn has_bitmap(&self) -> bool {
        self.kind == KIND_NODE || (self.kind == KIND_DATA && self.class != WHOLE_SLOT_CLASS)
    }

    fn tier(&self) -> Tier {
        Tier::from_byte(self.tier).unwrap_or(Tier::Cache)
    }

    fn capacity(&self) -> u32 {
        if self.kind == KIND_NODE {
            SLOT_UNITS
        } else {
            class_records(self.class).unwrap_or(0)
        }
    }
}

/// Bitmap and keys of a node or multi-record slot.
#[derive(Default)]
struct Records {
    bits: Box<[u64]>,
    keys: Vec<(u16, Key)>,
}

impl Records {
    fn get(&self, record: u32) -> bool {
        self.bits[(record / 64) as usize] & (1 << (record % 64)) != 0
    }

    fn set(&mut self, record: u32, on: bool) {
        let word = &mut self.bits[(record / 64) as usize];
        if on {
            *word |= 1 << (record % 64);
        } else {
            *word &= !(1 << (record % 64));
        }
    }

    fn first_clear(&self, from: u32, count: u32) -> Option<u32> {
        let mut record = from;
        while record < count {
            let word = self.bits[(record / 64) as usize] | ((1u64 << (record % 64)) - 1);
            if word != u64::MAX {
                let found = (record / 64) * 64 + word.trailing_ones();
                return (found < count).then_some(found);
            }
            record = (record / 64 + 1) * 64;
        }
        None
    }

    fn key_position(&self, record: u32) -> Option<usize> {
        self.keys.iter().position(|(r, _)| u32::from(*r) == record)
    }
}

#[derive(Clone, Copy)]
struct PendingUnit {
    epoch: u64,
    slot: u32,
    unit: u16,
}

/// In-memory slot table. See the module docs.
pub struct Slots {
    slots: Vec<Slot>,
    leases: Rc<[Cell<u32>]>,
    records: Vec<Records>,
    records_free: Vec<u32>,
    whole: Vec<Key>,
    whole_free: Vec<u32>,
    // Stack of free slots. May hold stale entries; `LISTED` marks slots on it.
    free: Vec<u32>,
    free_count: u64,
    partial: Vec<BTreeSet<u32>>,
    open: Vec<u32>,
    node_partial: BTreeSet<u32>,
    node_open: u32,
    pending: Vec<PendingUnit>,
    epoch: u64,
    last_success: u64,
    hand: u32,
    limits: Limits,
    node_slots: u64,
    cache_slots: u64,
    storage_slots: u64,
    small_bytes: u64,
}

fn tier_class(tier: Tier, class: u8) -> usize {
    tier as usize * CLASS_COUNT + usize::from(class)
}

fn record_bytes(class: u8) -> u64 {
    if class == WHOLE_SLOT_CLASS {
        0
    } else {
        u64::from(class_units(class).unwrap_or(0)) * UNIT_BYTES as u64
    }
}

impl Slots {
    /// Empty table: the meta slot holds the superblocks, every other slot is
    /// free. Fails with `InvalidConfiguration` outside `MIN_SLOTS..=MAX_SLOTS`.
    pub fn new(slot_count: u64, limits: Limits) -> Result<Self> {
        if !(MIN_SLOTS..=MAX_SLOTS).contains(&slot_count) {
            return Err(Error::InvalidConfiguration);
        }
        let count = slot_count as usize;
        let mut slots = vec![Slot::FREE; count];
        let free: Vec<u32> = (1..slot_count as u32).rev().collect();
        for &slot in &free {
            slots[slot as usize].flags = LISTED;
        }
        let mut table = Self {
            slots,
            leases: (0..count).map(|_| Cell::new(0)).collect(),
            records: Vec::new(),
            records_free: Vec::new(),
            whole: Vec::new(),
            whole_free: Vec::new(),
            free,
            free_count: slot_count - 1,
            partial: (0..2 * CLASS_COUNT).map(|_| BTreeSet::new()).collect(),
            open: vec![NONE; 2 * CLASS_COUNT],
            node_partial: BTreeSet::new(),
            node_open: NONE,
            pending: Vec::new(),
            epoch: 1,
            last_success: 0,
            hand: 1,
            limits,
            node_slots: 0,
            cache_slots: 0,
            storage_slots: 0,
            small_bytes: 0,
        };
        let aux = table.new_records(SLOT_UNITS);
        let meta = &mut table.slots[META_SLOT as usize];
        meta.kind = KIND_NODE;
        meta.aux = aux;
        meta.used = META_FIRST_NODE_UNIT;
        for unit in 0..u32::from(META_FIRST_NODE_UNIT) {
            table.records[aux as usize].set(unit, true);
        }
        Ok(table)
    }

    /// Rebuild the table at mount from the mounted tree's node addresses and
    /// entries. Returns the table and the entries that were dropped.
    ///
    /// Nodes win over data, storage over cache, and then the first entry seen.
    /// An entry is dropped when it is malformed or out of range, lands on a
    /// node slot, lands on a slot already used by another tier or class, or
    /// overlaps an earlier entry. Invalid or duplicate node addresses are
    /// ignored. Limits are not enforced while rebuilding. The result has no
    /// open slots and no reference bits set.
    pub fn rebuild(
        slot_count: u64,
        limits: Limits,
        nodes: impl IntoIterator<Item = NodeAddr>,
        entries: impl IntoIterator<Item = (Key, Value)>,
    ) -> Result<(Self, Vec<(Key, Value)>)> {
        let mut table = Self::new(slot_count, limits)?;
        for addr in nodes {
            table.mark_node(addr);
        }
        let mut entries: Vec<(Key, Value)> = entries.into_iter().collect();
        // Stable: storage first, then cache, each in the order given.
        entries.sort_by_key(|(_, value)| value.tier != Tier::Storage);
        let mut dropped = Vec::new();
        for (key, value) in entries {
            if !table.place(key, &value) {
                dropped.push((key, value));
            }
        }
        for slot in 0..slot_count as u32 {
            table.refresh(slot);
        }
        Ok((table, dropped))
    }

    /// Reserve nodes referenced only by the older superblock's tree. They are
    /// released by the reuse rule with tag `epoch`. Invalid addresses and
    /// addresses already in use are skipped. Returns how many were reserved.
    pub fn mark_pending_nodes(
        &mut self,
        addrs: impl IntoIterator<Item = NodeAddr>,
        epoch: u64,
    ) -> usize {
        let mut marked = 0;
        for addr in addrs {
            if self.mark_node(addr) {
                self.pending.push(PendingUnit {
                    epoch,
                    slot: addr.slot(),
                    unit: addr.unit(),
                });
                self.refresh(addr.slot());
                marked += 1;
            }
        }
        marked
    }

    /// Slots in the volume.
    pub fn slot_count(&self) -> u64 {
        self.slots.len() as u64
    }

    /// Current limits.
    pub fn limits(&self) -> Limits {
        self.limits
    }

    /// Replace the limits. Space already in use is kept.
    pub fn set_limits(&mut self, limits: Limits) {
        self.limits = limits;
    }

    /// Epoch tag for space freed now: the next commit attempt's epoch.
    pub fn epoch(&self) -> u64 {
        self.epoch
    }

    /// Start a commit attempt. Returns its epoch and advances the counter.
    pub fn begin_commit(&mut self) -> u64 {
        let epoch = self.epoch;
        self.epoch += 1;
        epoch
    }

    /// A commit attempt became durable. Releases pending space tagged at or
    /// before the previous successful commit, then records `epoch` as the
    /// latest successful commit.
    pub fn commit_succeeded(&mut self, epoch: u64) {
        let limit = self.last_success;
        let mut index = 0;
        while index < self.pending.len() {
            let pending = self.pending[index];
            if pending.epoch <= limit {
                self.pending.swap_remove(index);
                self.release_unit(pending.slot, pending.unit);
            } else {
                index += 1;
            }
        }
        self.last_success = self.last_success.max(epoch);
    }

    /// A commit attempt failed. Nothing is released.
    pub fn commit_failed(&mut self, _epoch: u64) {}

    /// Take a free node unit: the meta slot first, then open or partly filled
    /// node slots, then a free slot. `Busy` when there is no space.
    pub fn alloc_node(&mut self) -> Result<NodeAddr> {
        let slot = if self.slots[META_SLOT as usize].used < SLOT_UNITS as u16 {
            META_SLOT
        } else if self.node_open != NONE {
            self.node_open
        } else if let Some(&slot) = self.node_partial.first() {
            self.node_open = slot;
            slot
        } else {
            let slot = self.claim_free().ok_or(Error::Busy)?;
            self.init_node(slot);
            self.node_open = slot;
            slot
        };
        let entry = self.slots[slot as usize];
        let records = &mut self.records[entry.aux as usize];
        let unit = records
            .first_clear(0, SLOT_UNITS)
            .expect("node slot with free units has a clear bit");
        records.set(unit, true);
        self.slots[slot as usize].used += 1;
        self.refresh(slot);
        Ok(NodeAddr::new(slot, unit as u16))
    }

    /// Free a node unit. It is released by the reuse rule with tag `epoch`
    /// (normally [`Slots::epoch`]).
    pub fn free_node(&mut self, addr: NodeAddr, epoch: u64) -> Result<()> {
        if !addr.valid_for(self.slot_count()) {
            return Err(Error::InvalidConfiguration);
        }
        let entry = self.slots[addr.slot() as usize];
        if entry.kind != KIND_NODE || !self.records[entry.aux as usize].get(addr.unit().into()) {
            return Err(Error::Stale);
        }
        self.pending.push(PendingUnit {
            epoch,
            slot: addr.slot(),
            unit: addr.unit(),
        });
        Ok(())
    }

    /// Reserve a record for `length` bytes in `tier`. Reuses the open slot of
    /// the tier and class, then any partly filled one, then a free slot.
    ///
    /// Fails with `InvalidConfiguration` when the length has no class, and
    /// `Busy` when there is no free slot, the storage quota is reached, or the
    /// small-record byte cap would be exceeded.
    pub fn allocate(&mut self, tier: Tier, length: u64) -> Result<Allocation> {
        let class = super::format::class_for(length).ok_or(Error::InvalidConfiguration)?;
        let bytes = record_bytes(class);
        if bytes != 0 && self.small_bytes.saturating_add(bytes) > self.limits.small_bytes {
            return Err(Error::Busy);
        }
        let index = tier_class(tier, class);
        let slot = if self.open[index] != NONE {
            self.open[index]
        } else if let Some(&slot) = self.partial[index].first() {
            self.set_open(index, slot);
            slot
        } else {
            if tier == Tier::Storage && self.storage_slots >= self.limits.storage_slots {
                return Err(Error::Busy);
            }
            let slot = self.claim_free().ok_or(Error::Busy)?;
            self.init_data(slot, tier, class);
            if class != WHOLE_SLOT_CLASS {
                self.set_open(index, slot);
            }
            slot
        };
        let entry = self.slots[slot as usize];
        let record = if entry.has_bitmap() {
            let records = &mut self.records[entry.aux as usize];
            let record = records
                .first_clear(0, entry.capacity())
                .expect("open slot has a clear record");
            records.set(record, true);
            record
        } else {
            0
        };
        self.slots[slot as usize].used += 1;
        self.small_bytes += bytes;
        self.refresh(slot);
        let units = class_units(class).expect("known class");
        Ok(Allocation {
            slot,
            unit: (record * units) as u16,
            class,
            lease: SlotLease::new(&self.leases, slot),
        })
    }

    /// Record that the reserved unit now holds an index entry for `key`, and
    /// raise the slot's `retain_until`. Call after the tree insert.
    pub fn commit_record(
        &mut self,
        slot: u32,
        unit: u16,
        key: Key,
        retain_until: u64,
    ) -> Result<()> {
        let record = self.record_of(slot, unit)?;
        let entry = &mut self.slots[slot as usize];
        if entry.has_bitmap() {
            let records = &mut self.records[entry.aux as usize];
            if !records.get(record) || records.key_position(record).is_some() {
                return Err(Error::Stale);
            }
            records.keys.push((record as u16, key));
        } else {
            if entry.used == 0 || entry.flags & WHOLE_COMMITTED != 0 {
                return Err(Error::Stale);
            }
            entry.aux = if let Some(index) = self.whole_free.pop() {
                self.whole[index as usize] = key;
                index
            } else {
                self.whole.push(key);
                (self.whole.len() - 1) as u32
            };
            entry.flags |= WHOLE_COMMITTED;
        }
        let entry = &mut self.slots[slot as usize];
        entry.retain_until = entry.retain_until.max(retain_until);
        Ok(())
    }

    /// Release a reserved record that was never indexed. Frees it at once.
    pub fn abort(&mut self, allocation: Allocation) -> Result<()> {
        let record = self.record_of(allocation.slot, allocation.unit)?;
        let entry = self.slots[allocation.slot as usize];
        let reserved = if entry.has_bitmap() {
            let records = &self.records[entry.aux as usize];
            records.get(record) && records.key_position(record).is_none()
        } else {
            entry.used == 1 && entry.flags & WHOLE_COMMITTED == 0
        };
        if !reserved {
            return Err(Error::Stale);
        }
        self.release_unit(allocation.slot, allocation.unit);
        Ok(())
    }

    /// The index entry stored at `unit` was removed or replaced. A cache
    /// record is freed at once. A storage record is released by the reuse rule
    /// with tag `epoch` (normally [`Slots::epoch`]).
    pub fn release_record(&mut self, slot: u32, unit: u16, epoch: u64) -> Result<()> {
        let record = self.record_of(slot, unit)?;
        let entry = &mut self.slots[slot as usize];
        if entry.has_bitmap() {
            let records = &mut self.records[entry.aux as usize];
            let position = records.key_position(record).ok_or(Error::Stale)?;
            records.keys.swap_remove(position);
        } else {
            if entry.flags & WHOLE_COMMITTED == 0 {
                return Err(Error::Stale);
            }
            entry.flags &= !WHOLE_COMMITTED;
            self.whole_free.push(entry.aux);
            entry.aux = NONE;
        }
        if self.slots[slot as usize].tier() == Tier::Storage {
            self.pending.push(PendingUnit { epoch, slot, unit });
        } else {
            self.release_unit(slot, unit);
        }
        Ok(())
    }

    /// Pin a slot. Panics if `slot` is out of range.
    pub fn lease(&self, slot: u32) -> SlotLease {
        SlotLease::new(&self.leases, slot)
    }

    /// Set the CLOCK reference bit of a data slot (on reads).
    pub fn touch(&mut self, slot: u32) {
        if let Some(entry) = self.slots.get_mut(slot as usize)
            && entry.kind == KIND_DATA
        {
            entry.flags |= REFERENCED;
        }
    }

    /// Raise a data slot's `retain_until` to at least `until`.
    pub fn retain(&mut self, slot: u32, until: u64) {
        if let Some(entry) = self.data_slot_mut(slot) {
            entry.retain_until = entry.retain_until.max(until);
        }
    }

    /// Set a data slot's `retain_until`, for example after the caller
    /// recomputes it from the tree.
    pub fn set_retain(&mut self, slot: u32, until: u64) {
        if let Some(entry) = self.data_slot_mut(slot) {
            entry.retain_until = until;
        }
    }

    /// Sweep the CLOCK hand for a slot to evict.
    ///
    /// A candidate is a data slot that is not open, not leased, not already
    /// being evicted, has a clear reference bit, and is either cache tier or
    /// storage tier with `retain_until < now`. Reference bits are cleared as
    /// the hand passes. The sweep visits at most two full rotations. The
    /// chosen slot is not used for allocation until it is finished or
    /// canceled.
    pub fn evict_candidate(&mut self, now: u64) -> Option<Eviction> {
        let count = self.slots.len() as u32;
        for _ in 0..2 * count {
            let slot = self.hand;
            self.hand = if self.hand + 1 >= count {
                1
            } else {
                self.hand + 1
            };
            let entry = &mut self.slots[slot as usize];
            if entry.kind != KIND_DATA || entry.flags & (OPEN | EVICTING) != 0 {
                continue;
            }
            if entry.flags & REFERENCED != 0 {
                entry.flags &= !REFERENCED;
                continue;
            }
            if self.leases[slot as usize].get() != 0
                || (entry.tier() == Tier::Storage && entry.retain_until >= now)
            {
                continue;
            }
            entry.flags |= EVICTING;
            let entry = *entry;
            self.partial[tier_class(entry.tier(), entry.class)].remove(&slot);
            return Some(Eviction {
                slot,
                keys: self.keys(slot),
            });
        }
        None
    }

    /// Free a slot returned by [`Slots::evict_candidate`] after its keys were
    /// removed from the tree. Do not call `release_record` for those keys.
    ///
    /// Storage slots are freed at once too, along with any of their units
    /// waiting for the reuse rule. Expired storage data is no longer
    /// protected: the index removal makes it unreachable, and a stale entry in
    /// an older superblock fails its generation check.
    pub fn finish_eviction(&mut self, slot: u32) -> Result<()> {
        self.evicting(slot)?;
        self.pending.retain(|pending| pending.slot != slot);
        let entry = self.slots[slot as usize];
        self.small_bytes -= u64::from(entry.used) * record_bytes(entry.class);
        self.make_free(slot);
        Ok(())
    }

    /// Give back a slot returned by [`Slots::evict_candidate`] unchanged.
    pub fn cancel_eviction(&mut self, slot: u32) -> Result<()> {
        self.evicting(slot)?;
        self.slots[slot as usize].flags &= !EVICTING;
        self.refresh(slot);
        Ok(())
    }

    /// Index keys stored in a slot.
    pub fn keys(&self, slot: u32) -> Vec<Key> {
        let Some(entry) = self.slots.get(slot as usize) else {
            return Vec::new();
        };
        if entry.kind != KIND_DATA {
            Vec::new()
        } else if entry.has_bitmap() {
            self.records[entry.aux as usize]
                .keys
                .iter()
                .map(|(_, key)| *key)
                .collect()
        } else if entry.flags & WHOLE_COMMITTED != 0 {
            vec![self.whole[entry.aux as usize]]
        } else {
            Vec::new()
        }
    }

    /// State of one slot, or `None` when out of range.
    pub fn slot_state(&self, slot: u32) -> Option<SlotState> {
        let entry = self.slots.get(slot as usize)?;
        let kind = match entry.kind {
            KIND_NODE => SlotKind::Node,
            KIND_DATA => SlotKind::Data {
                tier: entry.tier(),
                class: entry.class,
            },
            _ => SlotKind::Free,
        };
        let indexed = match entry.kind {
            KIND_DATA if entry.has_bitmap() => self.records[entry.aux as usize].keys.len() as u32,
            KIND_DATA => u32::from(entry.flags & WHOLE_COMMITTED != 0),
            _ => 0,
        };
        Some(SlotState {
            kind,
            used: entry.used.into(),
            indexed,
            open: entry.flags & OPEN != 0 || (entry.kind == KIND_NODE && self.node_open == slot),
            referenced: entry.flags & REFERENCED != 0,
            evicting: entry.flags & EVICTING != 0,
            leases: self.leases[slot as usize].get(),
            retain_until: entry.retain_until,
        })
    }

    /// Occupancy summary.
    pub fn usage(&self) -> Usage {
        let mut node_units = 0;
        let mut records = 0;
        for (slot, entry) in self.slots.iter().enumerate() {
            if entry.kind == KIND_NODE {
                node_units += u64::from(entry.used);
            } else if entry.kind == KIND_DATA {
                records += u64::from(self.slot_state(slot as u32).map_or(0, |s| s.indexed));
            }
        }
        Usage {
            slot_count: self.slot_count(),
            free_slots: self.free_count,
            node_slots: self.node_slots,
            cache_slots: self.cache_slots,
            storage_slots: self.storage_slots,
            small_bytes: self.small_bytes,
            pending_units: self.pending.len() as u64,
            node_units,
            records,
        }
    }

    fn data_slot_mut(&mut self, slot: u32) -> Option<&mut Slot> {
        self.slots
            .get_mut(slot as usize)
            .filter(|entry| entry.kind == KIND_DATA)
    }

    fn evicting(&self, slot: u32) -> Result<()> {
        match self.slots.get(slot as usize) {
            Some(entry) if entry.flags & EVICTING != 0 => Ok(()),
            Some(_) => Err(Error::Stale),
            None => Err(Error::InvalidConfiguration),
        }
    }

    // Record index of `unit` in a data slot.
    fn record_of(&self, slot: u32, unit: u16) -> Result<u32> {
        let entry = self
            .slots
            .get(slot as usize)
            .ok_or(Error::InvalidConfiguration)?;
        if entry.kind != KIND_DATA {
            return Err(Error::Stale);
        }
        let units = class_units(entry.class).expect("known class");
        let unit = u32::from(unit);
        if unit % units != 0 || unit / units >= entry.capacity() {
            return Err(Error::InvalidConfiguration);
        }
        Ok(unit / units)
    }

    // Free one unit (node) or record (data) and refresh the slot.
    fn release_unit(&mut self, slot: u32, unit: u16) {
        let entry = self.slots[slot as usize];
        if entry.has_bitmap() {
            let record = if entry.kind == KIND_NODE {
                u32::from(unit)
            } else {
                u32::from(unit) / class_units(entry.class).expect("known class")
            };
            self.records[entry.aux as usize].set(record, false);
        }
        if entry.kind == KIND_DATA {
            self.small_bytes -= record_bytes(entry.class);
        }
        self.slots[slot as usize].used -= 1;
        self.refresh(slot);
    }

    // Mark a node unit used. Claims the slot if free. Returns false if the
    // address is invalid or already in use.
    fn mark_node(&mut self, addr: NodeAddr) -> bool {
        if !addr.valid_for(self.slot_count()) {
            return false;
        }
        let slot = addr.slot();
        match self.slots[slot as usize].kind {
            KIND_FREE => {
                self.free_count -= 1;
                self.init_node(slot);
            }
            KIND_NODE => {}
            _ => return false,
        }
        let entry = self.slots[slot as usize];
        let records = &mut self.records[entry.aux as usize];
        if records.get(addr.unit().into()) {
            return false;
        }
        records.set(addr.unit().into(), true);
        self.slots[slot as usize].used += 1;
        true
    }

    // Place a rebuilt entry. Returns false when it must be dropped.
    fn place(&mut self, key: Key, value: &Value) -> bool {
        if !value.is_well_formed() || u64::from(value.slot) >= self.slot_count() {
            return false;
        }
        let slot = value.slot;
        let entry = self.slots[slot as usize];
        match entry.kind {
            KIND_FREE => {
                self.free_count -= 1;
                self.init_data(slot, value.tier, value.class);
            }
            KIND_DATA if entry.tier() == value.tier && entry.class == value.class => {}
            _ => return false,
        }
        let entry = self.slots[slot as usize];
        let record = u32::from(value.unit) / class_units(value.class).expect("known class");
        if entry.has_bitmap() {
            if self.records[entry.aux as usize].get(record) {
                return false;
            }
            self.records[entry.aux as usize].set(record, true);
        } else if entry.used != 0 {
            return false;
        }
        self.slots[slot as usize].used += 1;
        self.small_bytes += record_bytes(value.class);
        self.commit_record(slot, value.unit, key, value.retain_until)
            .expect("record was just reserved");
        true
    }

    fn set_open(&mut self, index: usize, slot: u32) {
        let previous = self.open[index];
        if previous != NONE {
            self.slots[previous as usize].flags &= !OPEN;
        }
        self.open[index] = slot;
        if slot != NONE {
            self.slots[slot as usize].flags |= OPEN;
        }
    }

    // Recompute partial sets, open state, and freeing after a change.
    fn refresh(&mut self, slot: u32) {
        let entry = self.slots[slot as usize];
        match entry.kind {
            KIND_NODE => {
                if slot == META_SLOT {
                    return;
                }
                if entry.used == 0 {
                    self.make_free(slot);
                } else if u32::from(entry.used) < SLOT_UNITS {
                    self.node_partial.insert(slot);
                } else {
                    self.node_partial.remove(&slot);
                    if self.node_open == slot {
                        self.node_open = NONE;
                    }
                }
            }
            KIND_DATA => {
                if entry.flags & EVICTING != 0 {
                    return;
                }
                let index = tier_class(entry.tier(), entry.class);
                if entry.used == 0 {
                    self.make_free(slot);
                } else if u32::from(entry.used) < entry.capacity() {
                    self.partial[index].insert(slot);
                } else {
                    self.partial[index].remove(&slot);
                    if self.open[index] == slot {
                        self.set_open(index, NONE);
                    }
                }
            }
            _ => {}
        }
    }

    fn new_records(&mut self, count: u32) -> u32 {
        let records = Records {
            bits: vec![0; count.div_ceil(64) as usize].into_boxed_slice(),
            keys: Vec::new(),
        };
        if let Some(index) = self.records_free.pop() {
            self.records[index as usize] = records;
            index
        } else {
            self.records.push(records);
            (self.records.len() - 1) as u32
        }
    }

    fn init_node(&mut self, slot: u32) {
        let aux = self.new_records(SLOT_UNITS);
        let entry = &mut self.slots[slot as usize];
        entry.kind = KIND_NODE;
        entry.aux = aux;
        self.node_slots += 1;
    }

    fn init_data(&mut self, slot: u32, tier: Tier, class: u8) {
        let aux = if class == WHOLE_SLOT_CLASS {
            NONE
        } else {
            self.new_records(class_records(class).expect("known class"))
        };
        let entry = &mut self.slots[slot as usize];
        entry.kind = KIND_DATA;
        entry.tier = tier as u8;
        entry.class = class;
        entry.aux = aux;
        match tier {
            Tier::Cache => self.cache_slots += 1,
            Tier::Storage => self.storage_slots += 1,
        }
    }

    fn claim_free(&mut self) -> Option<u32> {
        while let Some(slot) = self.free.pop() {
            let entry = &mut self.slots[slot as usize];
            entry.flags &= !LISTED;
            if entry.kind == KIND_FREE {
                self.free_count -= 1;
                return Some(slot);
            }
        }
        None
    }

    fn make_free(&mut self, slot: u32) {
        let entry = self.slots[slot as usize];
        match entry.kind {
            KIND_NODE => {
                self.node_slots -= 1;
                self.node_partial.remove(&slot);
                if self.node_open == slot {
                    self.node_open = NONE;
                }
            }
            KIND_DATA => {
                match entry.tier() {
                    Tier::Cache => self.cache_slots -= 1,
                    Tier::Storage => self.storage_slots -= 1,
                }
                let index = tier_class(entry.tier(), entry.class);
                self.partial[index].remove(&slot);
                if self.open[index] == slot {
                    self.set_open(index, NONE);
                }
            }
            _ => return,
        }
        if entry.has_bitmap() {
            self.records[entry.aux as usize] = Records::default();
            self.records_free.push(entry.aux);
        } else if entry.flags & WHOLE_COMMITTED != 0 {
            self.whole_free.push(entry.aux);
        }
        let listed = entry.flags & LISTED;
        self.slots[slot as usize] = Slot {
            flags: LISTED,
            ..Slot::FREE
        };
        if listed == 0 {
            self.free.push(slot);
        }
        self.free_count += 1;
    }
}

#[cfg(test)]
#[path = "slots_tests.rs"]
mod tests;
