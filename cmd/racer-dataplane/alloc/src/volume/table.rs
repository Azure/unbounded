//! In-memory slot table: resident per-slot summaries and allocation state.
//!
//! The table keeps a small summary per slot (24 bytes), a 520 byte bitmap per
//! node slot, and a bitmap for each data slot that is open for allocation.
//! It does not know which keys live in a data slot; the record map tree does.
//! Bitmaps of other data slots are not kept: a closed data slot is reopened
//! only after [`Slots::install_bitmap`] supplies its occupancy, which the
//! volume reads from the record map.
//!
//! Changes that must reach the persisted slot table (a slot taken, freed,
//! quarantined, or its retention raised) are queued as [`SlotOp`]s and drained
//! by the commit with [`Slots::take_ops`]. Record counts are not queued: the
//! commit derives them from the record map changes it applies.
//!
//! The table never releases space a durable root set may still reference;
//! the volume tells it when space is reusable.

use std::cell::Cell;
use std::collections::{BTreeMap, BTreeSet};
use std::fmt;
use std::rc::Rc;

use super::format::{
    CLASSES, MAX_SLOTS, META_SLOT, MIN_SLOTS, NodeAddr, SLOT_UNITS, Tier, UNIT_BYTES,
    WHOLE_SLOT_CLASS, class_records, class_units, first_page_unit,
};
use super::page::{BITMAP_WORDS, SLOT_DATA, SLOT_NODE, SLOT_QUARANTINED, SlotSummary};
use crate::{Error, Result};

const NONE: u32 = u32::MAX;
const CLASS_COUNT: usize = CLASSES.len();

const KIND_FREE: u8 = 0;

const OPEN: u8 = 1;
const REFERENCED: u8 = 2;
const EVICTING: u8 = 4;
const LISTED: u8 = 8;
const RETAIN_STALE: u8 = 16;
const COPY: u8 = 32;

/// Node pages a commit takes per run: 64 pages, 256 KiB.
pub const RUN_PAGES: usize = 64;

/// Allocation limits.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Limits {
    /// Most data slots in the storage tier.
    pub storage_slots: u64,

    /// Most bytes held by records of classes below the whole slot, both tiers.
    /// Counts every record in use, including records waiting for reuse.
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

    /// Quarantined slots: never allocated or evicted.
    pub quarantined_slots: u64,

    /// Bytes in use by records of classes below the whole slot.
    pub small_bytes: u64,

    /// Metadata pages and storage records waiting for the reuse rule.
    pub pending_units: u64,

    /// Node units in use, including pending ones and the meta slot's.
    pub node_units: u64,

    /// Records in use, excluding reservations: published, or waiting for the
    /// reuse rule.
    pub records: u64,
}

/// Kind of one slot.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SlotKind {
    /// Not in use.
    Free,

    /// Holds metadata pages (the meta slot is always a node slot).
    Node,

    /// Holds records of one tier and class.
    Data {
        /// Tier of every record.
        tier: Tier,

        /// Class index of every record.
        class: u8,
    },

    /// Its allocation metadata was unreadable. Never allocated or evicted.
    Quarantined,
}

/// State of one slot, for tests and diagnostics.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SlotState {
    /// Kind.
    pub kind: SlotKind,

    /// Records (or node units) in use: reserved, published, or pending.
    pub used: u32,

    /// Records reserved and not published.
    pub reserved: u32,

    /// Open for allocation.
    pub open: bool,

    /// CLOCK reference bit.
    pub referenced: bool,

    /// Being evicted.
    pub evicting: bool,

    /// Live leases.
    pub leases: u32,

    /// Upper bound of the records' `retain_until`.
    pub retain_until: u64,

    /// Incarnation: changes every time the slot is taken.
    pub incarnation: u32,
}

/// RAII pin on one slot. Eviction skips leased slots.
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

    /// Incarnation of the slot.
    pub incarnation: u32,

    /// Lease on the slot, held until the record is published or aborted.
    pub lease: SlotLease,
}

/// Persisted identity of a slot, as queued for the slot table.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Ident {
    /// Free: no entry.
    Free,

    /// Node slot with a new incarnation.
    Node(u32),

    /// Data slot with a new incarnation.
    Data {
        /// Tier.
        tier: Tier,

        /// Class.
        class: u8,

        /// Incarnation.
        incarnation: u32,
    },

    /// Quarantined.
    Quarantined(u32),
}

/// Queued change to a slot's persisted summary.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct SlotOp {
    /// New identity. Resets the record count.
    pub ident: Option<Ident>,

    /// Raise `retain_until` to at least this.
    pub retain_max: Option<u64>,

    /// Set `retain_until` to this (after a recompute), then apply
    /// `retain_max`.
    pub retain_set: Option<u64>,
}

impl SlotOp {
    /// Fold a later op into this one.
    pub fn merge(&mut self, later: &SlotOp) {
        if later.ident.is_some() {
            *self = *later;
            return;
        }
        if let Some(set) = later.retain_set {
            self.retain_set = Some(set);
            self.retain_max = later.retain_max;
        } else if let Some(max) = later.retain_max {
            self.retain_max = Some(self.retain_max.map_or(max, |m| m.max(max)));
        }
    }

    /// Apply to a persisted summary (or none) given the record count change
    /// of the summary's resulting incarnation.
    pub fn apply(&self, old: Option<SlotSummary>) -> Option<SlotSummary> {
        let mut summary = match self.ident {
            Some(Ident::Free) => return None,
            Some(Ident::Node(incarnation)) => SlotSummary {
                kind: SLOT_NODE,
                incarnation,
                ..SlotSummary::default()
            },
            Some(Ident::Data {
                tier,
                class,
                incarnation,
            }) => SlotSummary {
                kind: SLOT_DATA,
                tier: tier as u8,
                class,
                incarnation,
                ..SlotSummary::default()
            },
            Some(Ident::Quarantined(incarnation)) => SlotSummary {
                kind: SLOT_QUARANTINED,
                incarnation,
                ..SlotSummary::default()
            },
            None => old?,
        };
        if let Some(set) = self.retain_set {
            summary.retain_until = set;
        }
        if let Some(max) = self.retain_max {
            summary.retain_until = summary.retain_until.max(max);
        }
        Some(summary)
    }
}

/// Per-slot entry. Kept at 24 bytes.
#[derive(Clone, Copy)]
struct Slot {
    retain_until: u64,
    // Index into `bitmaps`, or NONE.
    aux: u32,
    incarnation: u32,
    used: u16,
    reserved: u16,
    kind: u8,
    tier: u8,
    class: u8,
    flags: u8,
}

const _: () = assert!(std::mem::size_of::<Slot>() == 24);

impl Slot {
    const FREE: Self = Self {
        retain_until: 0,
        aux: NONE,
        incarnation: 0,
        used: 0,
        reserved: 0,
        kind: KIND_FREE,
        tier: 0,
        class: 0,
        flags: 0,
    };

    fn tier(&self) -> Tier {
        Tier::from_byte(self.tier).unwrap_or(Tier::Cache)
    }

    fn capacity(&self) -> u32 {
        if self.kind == SLOT_NODE {
            SLOT_UNITS
        } else {
            class_records(self.class).unwrap_or(0)
        }
    }

    fn is_data(&self) -> bool {
        self.kind == SLOT_DATA
    }
}

/// A node slot's occupancy and its last written copy.
pub struct Bitmap {
    words: Box<[u64; BITMAP_WORDS]>,
    seq: u64,
}

fn get_bit(words: &[u64; BITMAP_WORDS], bit: u32) -> bool {
    words[(bit / 64) as usize] & (1 << (bit % 64)) != 0
}

fn set_bit(words: &mut [u64; BITMAP_WORDS], bit: u32, on: bool) {
    let word = &mut words[(bit / 64) as usize];
    if on {
        *word |= 1 << (bit % 64);
    } else {
        *word &= !(1 << (bit % 64));
    }
}

fn first_clear(words: &[u64; BITMAP_WORDS], from: u32, count: u32) -> Option<u32> {
    let mut bit = from;
    while bit < count {
        let word = words[(bit / 64) as usize] | ((1u64 << (bit % 64)) - 1);
        if word != u64::MAX {
            let found = (bit / 64) * 64 + word.trailing_ones();
            return (found < count).then_some(found);
        }
        bit = (bit / 64 + 1) * 64;
    }
    None
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

/// In-memory slot table. See the module docs.
pub struct Slots {
    slots: Vec<Slot>,
    leases: Rc<[Cell<u32>]>,
    bitmaps: Vec<Option<Bitmap>>,
    bitmaps_free: Vec<u32>,
    // Stack of free slots. May hold stale entries; `LISTED` marks slots on it.
    free: Vec<u32>,
    free_count: u64,
    // Closed data slots with room, per tier and class.
    partial: Vec<BTreeSet<u32>>,
    // Closed data slots with room whose bitmap is loaded, per tier and class.
    ready: Vec<Vec<u32>>,
    open: Vec<u32>,
    node_open: u32,
    node_partial: BTreeSet<u32>,
    hand: u32,
    limits: Limits,
    node_slots: u64,
    cache_slots: u64,
    storage_slots: u64,
    quarantined: u64,
    small_bytes: u64,
    pending: u64,
    next_incarnation: u32,
    ops: BTreeMap<u32, SlotOp>,
    dirty_nodes: BTreeSet<u32>,
    // Fenwick tree of per-slot used bytes.
    used: Vec<u64>,
}

impl Slots {
    /// Table of a new volume: the meta slot holds the superblocks and its
    /// bitmap copies; every other slot is free. Fails with
    /// `InvalidConfiguration` outside `MIN_SLOTS..=MAX_SLOTS`.
    pub fn new(slot_count: u64, limits: Limits) -> Result<Self> {
        let mut table = Self::empty(slot_count, limits)?;
        table.init_node(META_SLOT, 1);
        table.next_incarnation = 2;
        table.ops.insert(
            META_SLOT,
            SlotOp {
                ident: Some(Ident::Node(1)),
                ..SlotOp::default()
            },
        );
        table.rebuild_free();
        table.sync_used(META_SLOT);
        table.refresh(META_SLOT);
        Ok(table)
    }

    fn empty(slot_count: u64, limits: Limits) -> Result<Self> {
        if !(MIN_SLOTS..=MAX_SLOTS).contains(&slot_count) {
            return Err(Error::InvalidConfiguration);
        }
        let count = slot_count as usize;
        Ok(Self {
            slots: vec![Slot::FREE; count],
            leases: (0..count).map(|_| Cell::new(0)).collect(),
            bitmaps: Vec::new(),
            bitmaps_free: Vec::new(),
            free: Vec::new(),
            free_count: 0,
            partial: (0..2 * CLASS_COUNT).map(|_| BTreeSet::new()).collect(),
            ready: (0..2 * CLASS_COUNT).map(|_| Vec::new()).collect(),
            open: vec![NONE; 2 * CLASS_COUNT],
            node_open: NONE,
            node_partial: BTreeSet::new(),
            hand: 1,
            limits,
            node_slots: 0,
            cache_slots: 0,
            storage_slots: 0,
            quarantined: 0,
            small_bytes: 0,
            pending: 0,
            next_incarnation: 1,
            ops: BTreeMap::new(),
            dirty_nodes: BTreeSet::new(),
            used: vec![0; count],
        })
    }

    /// Table of a mounted volume from its persisted summaries. Node slots
    /// start with every unit in use until [`Slots::set_node_bitmap`] supplies
    /// their bitmap; slots whose bitmap never arrives stay full. Summaries
    /// out of range are ignored.
    pub fn mount(
        slot_count: u64,
        limits: Limits,
        summaries: impl IntoIterator<Item = (u32, SlotSummary)>,
        next_incarnation: u32,
    ) -> Result<Self> {
        let mut table = Self::empty(slot_count, limits)?;
        table.next_incarnation = next_incarnation.max(1);
        for (slot, summary) in summaries {
            if u64::from(slot) >= slot_count {
                continue;
            }
            match summary.kind {
                SLOT_NODE => {
                    table.init_node(slot, summary.incarnation);
                    let aux = table.slots[slot as usize].aux;
                    let bitmap = table.bitmaps[aux as usize].as_mut().expect("node bitmap");
                    bitmap.words.fill(u64::MAX);
                    let tail = SLOT_UNITS % 64;
                    bitmap.words[BITMAP_WORDS - 1] = (1u64 << tail) - 1;
                    bitmap.seq = summary.bitmap_seq;
                    let entry = &mut table.slots[slot as usize];
                    entry.used = SLOT_UNITS as u16;
                    if summary.copy == 1 {
                        entry.flags |= COPY;
                    }
                }
                SLOT_DATA => {
                    let tier = Tier::from_byte(summary.tier).unwrap_or(Tier::Cache);
                    table.init_data(slot, tier, summary.class, summary.incarnation);
                    let entry = &mut table.slots[slot as usize];
                    entry.used = summary.used as u16;
                    entry.retain_until = summary.retain_until;
                    table.small_bytes += u64::from(summary.used) * record_bytes(summary.class);
                }
                _ => table.quarantine_now(slot, summary.incarnation),
            }
            table.next_incarnation = table.next_incarnation.max(summary.incarnation + 1);
        }
        if table.slots[META_SLOT as usize].kind == KIND_FREE {
            // A slot table without the meta slot is not usable; keep it full.
            table.init_node(META_SLOT, 0);
            table.slots[META_SLOT as usize].used = SLOT_UNITS as u16;
        }
        table.rebuild_free();
        for slot in 0..slot_count as u32 {
            table.sync_used(slot);
            table.refresh(slot);
        }
        Ok(table)
    }

    fn rebuild_free(&mut self) {
        self.free.clear();
        self.free_count = 0;
        for slot in (1..self.slots.len() as u32).rev() {
            if self.slots[slot as usize].kind == KIND_FREE {
                self.slots[slot as usize].flags |= LISTED;
                self.free.push(slot);
                self.free_count += 1;
            }
        }
    }

    /// Supply a node slot's persisted bitmap at mount.
    pub fn set_node_bitmap(&mut self, slot: u32, words: &[u64; BITMAP_WORDS]) {
        let entry = self.slots[slot as usize];
        if entry.kind != SLOT_NODE {
            return;
        }
        let bitmap = self.bitmaps[entry.aux as usize]
            .as_mut()
            .expect("node bitmap");
        bitmap.words.copy_from_slice(words);
        let first = u32::from(first_page_unit(slot));
        for bit in 0..first {
            set_bit(&mut bitmap.words, bit, true);
        }
        let used: u32 = bitmap.words.iter().map(|w| w.count_ones()).sum();
        self.slots[slot as usize].used = used as u16;
        self.sync_used(slot);
        self.refresh(slot);
    }

    /// Mark one unit of a node slot used, for a mount that rebuilds a bitmap
    /// from the trees. `clear_first` empties the bitmap before the first call.
    pub fn mark_node_unit(&mut self, addr: NodeAddr, clear_first: bool) {
        let slot = addr.slot();
        let Some(entry) = self.slots.get(slot as usize).copied() else {
            return;
        };
        if entry.kind != SLOT_NODE {
            return;
        }
        let bitmap = self.bitmaps[entry.aux as usize]
            .as_mut()
            .expect("node bitmap");
        if clear_first {
            bitmap.words.fill(0);
            for bit in 0..u32::from(first_page_unit(slot)) {
                set_bit(&mut bitmap.words, bit, true);
            }
        }
        if u32::from(addr.unit()) < SLOT_UNITS {
            set_bit(&mut bitmap.words, addr.unit().into(), true);
        }
        let used: u32 = bitmap.words.iter().map(|w| w.count_ones()).sum();
        self.slots[slot as usize].used = used as u16;
        self.dirty_nodes.insert(slot);
        self.sync_used(slot);
        self.refresh(slot);
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

    /// Next incarnation to hand out.
    pub fn next_incarnation(&self) -> u32 {
        self.next_incarnation
    }

    /// Count of space waiting for the reuse rule (for usage).
    pub fn set_pending(&mut self, pending: u64) {
        self.pending = pending;
    }

    /// Take the queued slot table changes.
    pub fn take_ops(&mut self) -> BTreeMap<u32, SlotOp> {
        std::mem::take(&mut self.ops)
    }

    /// Put back changes taken by a failed commit, under newer ones.
    pub fn restore_ops(&mut self, mut ops: BTreeMap<u32, SlotOp>) {
        for (slot, later) in std::mem::take(&mut self.ops) {
            ops.entry(slot).or_default().merge(&later);
        }
        self.ops = ops;
    }

    /// Whether slot table changes are queued.
    pub fn has_ops(&self) -> bool {
        !self.ops.is_empty()
    }

    fn queue(&mut self, slot: u32, op: SlotOp) {
        self.ops.entry(slot).or_default().merge(&op);
    }

    fn take_incarnation(&mut self) -> u32 {
        let incarnation = self.next_incarnation;
        self.next_incarnation = self.next_incarnation.checked_add(1).expect("incarnations");
        incarnation
    }

    /// Persisted state of a node slot: bitmap copy bit and seq.
    pub fn node_copy(&self, slot: u32) -> Option<(u8, u64, u32)> {
        let entry = self.slots.get(slot as usize)?;
        if entry.kind != SLOT_NODE {
            return None;
        }
        let bitmap = self.bitmaps[entry.aux as usize].as_ref()?;
        Some((
            u8::from(entry.flags & COPY != 0),
            bitmap.seq,
            entry.incarnation,
        ))
    }

    /// Bitmap of a node slot.
    pub fn node_words(&self, slot: u32) -> Option<&[u64; BITMAP_WORDS]> {
        let entry = self.slots.get(slot as usize)?;
        if entry.kind != SLOT_NODE {
            return None;
        }
        self.bitmaps[entry.aux as usize].as_ref().map(|b| &*b.words)
    }

    /// Node slots whose bitmap changed since the last [`Slots::take_dirty_nodes`].
    pub fn take_dirty_nodes(&mut self) -> BTreeSet<u32> {
        std::mem::take(&mut self.dirty_nodes)
    }

    /// Mark node slots dirty again (after a failed commit).
    pub fn mark_dirty_nodes(&mut self, slots: impl IntoIterator<Item = u32>) {
        self.dirty_nodes.extend(slots);
    }

    /// Record that a commit wrote copy `copy` of a node slot's bitmap at `seq`.
    pub fn set_node_copy(&mut self, slot: u32, copy: u8, seq: u64) {
        let entry = &mut self.slots[slot as usize];
        if entry.kind != SLOT_NODE {
            return;
        }
        if copy == 1 {
            entry.flags |= COPY;
        } else {
            entry.flags &= !COPY;
        }
        let aux = entry.aux;
        if let Some(bitmap) = self.bitmaps[aux as usize].as_mut() {
            bitmap.seq = seq;
        }
    }

    /// Take up to `count` free node units, preferring long runs in the open
    /// node slot. Claims free slots as node slots when needed. Returns fewer
    /// when space runs out.
    pub fn alloc_pages(&mut self, count: usize) -> Vec<NodeAddr> {
        let mut out = Vec::with_capacity(count);
        while out.len() < count {
            let slot = if self.node_open != NONE {
                self.node_open
            } else if self.slots[META_SLOT as usize].kind == SLOT_NODE
                && u32::from(self.slots[META_SLOT as usize].used) < SLOT_UNITS
                && self.node_partial.contains(&META_SLOT)
            {
                META_SLOT
            } else if let Some(&slot) = self.node_partial.iter().find(|&&s| s != META_SLOT) {
                slot
            } else if let Some(slot) = self.claim_free() {
                let incarnation = self.take_incarnation();
                self.init_node(slot, incarnation);
                self.queue(
                    slot,
                    SlotOp {
                        ident: Some(Ident::Node(incarnation)),
                        ..SlotOp::default()
                    },
                );
                self.sync_used(slot);
                slot
            } else {
                break;
            };
            self.node_open = slot;
            let entry = self.slots[slot as usize];
            let bitmap = self.bitmaps[entry.aux as usize]
                .as_mut()
                .expect("node bitmap");
            let first = u32::from(first_page_unit(slot));
            let mut taken = 0;
            let mut unit = first;
            while out.len() < count {
                let Some(found) = first_clear(&bitmap.words, unit, SLOT_UNITS) else {
                    break;
                };
                set_bit(&mut bitmap.words, found, true);
                out.push(NodeAddr::new(slot, found as u16));
                taken += 1;
                unit = found + 1;
            }
            if taken == 0 {
                // Full: close it.
                self.node_partial.remove(&slot);
                self.node_open = NONE;
                continue;
            }
            self.slots[slot as usize].used += taken as u16;
            self.dirty_nodes.insert(slot);
            self.sync_used(slot);
            self.refresh(slot);
        }
        out
    }

    /// Make a node unit free at once (for units no durable root set
    /// references). Frees the slot when it holds nothing.
    pub fn free_page(&mut self, addr: NodeAddr) -> bool {
        let slot = addr.slot();
        let Some(entry) = self.slots.get(slot as usize).copied() else {
            return false;
        };
        if entry.kind != SLOT_NODE
            || addr.unit() < first_page_unit(slot)
            || u32::from(addr.unit()) >= SLOT_UNITS
        {
            return false;
        }
        let bitmap = self.bitmaps[entry.aux as usize]
            .as_mut()
            .expect("node bitmap");
        if !get_bit(&bitmap.words, addr.unit().into()) {
            return false;
        }
        set_bit(&mut bitmap.words, addr.unit().into(), false);
        self.slots[slot as usize].used -= 1;
        self.dirty_nodes.insert(slot);
        self.sync_used(slot);
        self.refresh(slot);
        true
    }

    /// Whether a node unit is marked used.
    pub fn page_used(&self, addr: NodeAddr) -> bool {
        let Some(entry) = self.slots.get(addr.slot() as usize) else {
            return false;
        };
        entry.kind == SLOT_NODE
            && u32::from(addr.unit()) < SLOT_UNITS
            && get_bit(
                &self.bitmaps[entry.aux as usize]
                    .as_ref()
                    .expect("node bitmap")
                    .words,
                addr.unit().into(),
            )
    }

    /// Reserve a record for `length` bytes in `tier`: in the open slot of the
    /// tier and class, then a partly filled slot whose bitmap is loaded, then
    /// a free slot.
    ///
    /// Fails with `InvalidConfiguration` when the length has no class, and
    /// `Busy` when there is no space, the storage quota is reached, or the
    /// small-record byte cap would be exceeded.
    pub fn allocate(&mut self, tier: Tier, length: u64) -> Result<Allocation> {
        let class = super::format::class_for(length).ok_or(Error::InvalidConfiguration)?;
        let bytes = record_bytes(class);
        if bytes != 0 && self.small_bytes.saturating_add(bytes) > self.limits.small_bytes {
            return Err(Error::Busy);
        }
        let index = tier_class(tier, class);
        let slot = loop {
            if self.open[index] != NONE {
                break self.open[index];
            }
            if let Some(slot) = self.ready[index].pop() {
                let entry = self.slots[slot as usize];
                if entry.is_data()
                    && entry.aux != NONE
                    && entry.flags & (OPEN | EVICTING) == 0
                    && u32::from(entry.used) < entry.capacity()
                {
                    self.set_open(index, slot);
                }
                continue;
            }
            if tier == Tier::Storage && self.storage_slots >= self.limits.storage_slots {
                return Err(Error::Busy);
            }
            let slot = self.claim_free().ok_or(Error::Busy)?;
            let incarnation = self.take_incarnation();
            self.init_data(slot, tier, class, incarnation);
            self.queue(
                slot,
                SlotOp {
                    ident: Some(Ident::Data {
                        tier,
                        class,
                        incarnation,
                    }),
                    ..SlotOp::default()
                },
            );
            if class != WHOLE_SLOT_CLASS {
                self.set_open(index, slot);
            }
            break slot;
        };
        let entry = self.slots[slot as usize];
        let record = if entry.aux != NONE {
            let bitmap = self.bitmaps[entry.aux as usize].as_mut().expect("bitmap");
            let record = first_clear(&bitmap.words, 0, entry.capacity())
                .expect("open slot has a clear record");
            set_bit(&mut bitmap.words, record, true);
            record
        } else {
            0
        };
        let entry = &mut self.slots[slot as usize];
        entry.used += 1;
        entry.reserved += 1;
        self.sync_used(slot);
        self.small_bytes += bytes;
        self.refresh(slot);
        let units = class_units(class).expect("known class");
        let incarnation = self.slots[slot as usize].incarnation;
        Ok(Allocation {
            slot,
            unit: (record * units) as u16,
            class,
            incarnation,
            lease: SlotLease::new(&self.leases, slot),
        })
    }

    /// Whether [`Slots::allocate`] for `tier` and `length` would fail only
    /// because the storage slot quota is reached.
    pub fn storage_quota_blocks(&self, tier: Tier, length: u64) -> bool {
        let Some(class) = super::format::class_for(length) else {
            return false;
        };
        let index = tier_class(tier, class);
        tier == Tier::Storage
            && self.open[index] == NONE
            && self.ready[index].is_empty()
            && self.storage_slots >= self.limits.storage_slots
    }

    /// A closed, partly filled data slot of `tier` and `length`'s class whose
    /// bitmap is not loaded, with its incarnation. The volume loads its
    /// bitmap from the record map and calls [`Slots::install_bitmap`].
    pub fn partial_candidate(&self, tier: Tier, length: u64) -> Option<(u32, u32)> {
        let class = super::format::class_for(length)?;
        if class == WHOLE_SLOT_CLASS {
            return None;
        }
        let index = tier_class(tier, class);
        if self.open[index] != NONE || !self.ready[index].is_empty() {
            return None;
        }
        self.partial[index].iter().find_map(|&slot| {
            let entry = self.slots[slot as usize];
            (entry.aux == NONE && entry.flags & (OPEN | EVICTING) == 0)
                .then_some((slot, entry.incarnation))
        })
    }

    /// Supply the occupancy of a closed data slot read from the record map.
    /// Ignored if the slot changed since `incarnation`, or if the bitmap does
    /// not account for every used record (it then stays closed).
    pub fn install_bitmap(&mut self, slot: u32, incarnation: u32, records: &[u32]) -> bool {
        let Some(entry) = self.slots.get(slot as usize).copied() else {
            return false;
        };
        if !entry.is_data()
            || entry.incarnation != incarnation
            || entry.aux != NONE
            || entry.flags & (OPEN | EVICTING) != 0
            || entry.class == WHOLE_SLOT_CLASS
            || records.len() != usize::from(entry.used)
        {
            return false;
        }
        let mut words = Box::new([0u64; BITMAP_WORDS]);
        for &record in records {
            if record >= entry.capacity() || get_bit(&words, record) {
                return false;
            }
            set_bit(&mut words, record, true);
        }
        let aux = self.new_bitmap(words);
        self.slots[slot as usize].aux = aux;
        self.ready[tier_class(entry.tier(), entry.class)].push(slot);
        true
    }

    /// A reservation was published: its record now holds an index entry.
    pub fn published(&mut self, allocation: Allocation) {
        let entry = &mut self.slots[allocation.slot as usize];
        if entry.incarnation == allocation.incarnation && entry.reserved > 0 {
            entry.reserved -= 1;
        }
    }

    /// Release a reservation that was never published. Frees it at once.
    pub fn abort(&mut self, allocation: Allocation) {
        let slot = allocation.slot;
        let entry = self.slots[slot as usize];
        if entry.incarnation != allocation.incarnation || entry.reserved == 0 {
            return;
        }
        self.slots[slot as usize].reserved -= 1;
        self.release_unit(slot, allocation.unit);
    }

    /// Free a published record of incarnation `incarnation` at once (its
    /// record map entry is gone, or will be removed by the commit in progress
    /// before anything can reference the reuse). Returns false when the slot
    /// changed since.
    pub fn release(&mut self, slot: u32, unit: u16, incarnation: u32) -> bool {
        let Some(entry) = self.slots.get(slot as usize).copied() else {
            return false;
        };
        if !entry.is_data() || entry.incarnation != incarnation || entry.used <= entry.reserved {
            return false;
        }
        self.release_unit(slot, unit);
        true
    }

    /// Pin a slot. Panics if `slot` is out of range.
    pub fn lease(&self, slot: u32) -> SlotLease {
        SlotLease::new(&self.leases, slot)
    }

    /// Set the CLOCK reference bit of a data slot (on reads).
    pub fn touch(&mut self, slot: u32) {
        if let Some(entry) = self.slots.get_mut(slot as usize)
            && entry.is_data()
        {
            entry.flags |= REFERENCED;
        }
    }

    /// Incarnation of a data slot.
    pub fn incarnation(&self, slot: u32) -> Option<u32> {
        self.slots
            .get(slot as usize)
            .filter(|e| e.is_data())
            .map(|e| e.incarnation)
    }

    /// Raise a data slot's `retain_until` to at least `until`.
    pub fn retain(&mut self, slot: u32, until: u64) {
        let Some(entry) = self.slots.get_mut(slot as usize).filter(|e| e.is_data()) else {
            return;
        };
        if until > entry.retain_until {
            entry.retain_until = until;
            self.queue(
                slot,
                SlotOp {
                    retain_max: Some(until),
                    ..SlotOp::default()
                },
            );
        }
    }

    /// Note that a record of a data slot lowered its `retain_until`: the
    /// slot's bound may be loose until [`Slots::set_retain`] recomputes it.
    pub fn retain_lowered(&mut self, slot: u32) {
        if let Some(entry) = self.slots.get_mut(slot as usize).filter(|e| e.is_data()) {
            entry.flags |= RETAIN_STALE;
        }
    }

    /// Storage slots whose `retain_until` bound may be loose, up to `limit`.
    pub fn stale_retain(&self, limit: usize) -> Vec<(u32, u32)> {
        self.slots
            .iter()
            .enumerate()
            .filter(|(_, e)| e.is_data() && e.flags & RETAIN_STALE != 0)
            .take(limit)
            .map(|(slot, e)| (slot as u32, e.incarnation))
            .collect()
    }

    /// Set a data slot's `retain_until` after a recompute that started when
    /// it was `seen`. Skipped when the bound rose since (a concurrent raise).
    pub fn set_retain(&mut self, slot: u32, incarnation: u32, seen: u64, until: u64) {
        let Some(entry) = self.slots.get_mut(slot as usize).filter(|e| e.is_data()) else {
            return;
        };
        if entry.incarnation != incarnation || entry.retain_until != seen {
            return;
        }
        entry.flags &= !RETAIN_STALE;
        entry.retain_until = until;
        self.queue(
            slot,
            SlotOp {
                retain_set: Some(until),
                ..SlotOp::default()
            },
        );
    }

    /// Sweep the CLOCK hand for a data slot to evict and mark it evicting.
    ///
    /// A candidate is a data slot that is not open, not leased, not already
    /// being evicted, has a clear reference bit, and is either cache tier or
    /// storage tier with `retain_until < now`. When none is found, an idle
    /// open cache slot is closed and chosen. `tier` limits the candidates.
    /// Returns the slot and its incarnation.
    pub fn evict_candidate(&mut self, now: u64, tier: Option<Tier>) -> Option<(u32, u32)> {
        let count = self.slots.len() as u32;
        for _ in 0..2 * count {
            let slot = self.hand;
            self.hand = if self.hand + 1 >= count {
                1
            } else {
                self.hand + 1
            };
            let entry = &mut self.slots[slot as usize];
            if !entry.is_data()
                || entry.flags & (OPEN | EVICTING) != 0
                || tier.is_some_and(|tier| entry.tier() != tier)
            {
                continue;
            }
            if entry.flags & REFERENCED != 0 {
                entry.flags &= !REFERENCED;
                continue;
            }
            if self.leases[slot as usize].get() != 0
                || entry.reserved != 0
                || (entry.tier() == Tier::Storage && entry.retain_until >= now)
            {
                continue;
            }
            return Some(self.begin_eviction(slot));
        }
        if tier == Some(Tier::Storage) {
            return None;
        }
        let slot = (1..count).find(|&slot| {
            let entry = self.slots[slot as usize];
            entry.is_data()
                && entry.tier() == Tier::Cache
                && entry.flags & OPEN != 0
                && entry.flags & EVICTING == 0
                && entry.reserved == 0
                && self.leases[slot as usize].get() == 0
        })?;
        let entry = self.slots[slot as usize];
        self.set_open(tier_class(entry.tier(), entry.class), NONE);
        Some(self.begin_eviction(slot))
    }

    fn begin_eviction(&mut self, slot: u32) -> (u32, u32) {
        let entry = &mut self.slots[slot as usize];
        entry.flags |= EVICTING;
        entry.flags &= !REFERENCED;
        let entry = *entry;
        let index = tier_class(entry.tier(), entry.class);
        self.partial[index].remove(&slot);
        self.drop_bitmap(slot);
        (slot, entry.incarnation)
    }

    /// Free a slot after eviction removed its index entries and record map
    /// entries. Its storage records waiting for the reuse rule are freed too.
    pub fn finish_eviction(&mut self, slot: u32) -> Result<()> {
        self.evicting(slot)?;
        let entry = self.slots[slot as usize];
        self.small_bytes -= u64::from(entry.used) * record_bytes(entry.class);
        self.make_free(slot);
        Ok(())
    }

    /// Give back a slot whose eviction stopped.
    pub fn cancel_eviction(&mut self, slot: u32) -> Result<()> {
        self.evicting(slot)?;
        self.slots[slot as usize].flags &= !EVICTING;
        self.refresh(slot);
        Ok(())
    }

    /// Quarantine a slot: never allocated or evicted again, persisted.
    pub fn quarantine(&mut self, slot: u32) {
        let Some(entry) = self.slots.get(slot as usize).copied() else {
            return;
        };
        if slot == META_SLOT || entry.kind == SLOT_QUARANTINED {
            return;
        }
        if entry.kind == KIND_FREE {
            self.claim_specific(slot);
        } else {
            self.small_bytes -= u64::from(entry.used) * record_bytes(entry.class);
            self.make_free(slot);
            self.claim_specific(slot);
        }
        let incarnation = self.take_incarnation();
        self.quarantine_now(slot, incarnation);
        self.queue(
            slot,
            SlotOp {
                ident: Some(Ident::Quarantined(incarnation)),
                ..SlotOp::default()
            },
        );
    }

    fn quarantine_now(&mut self, slot: u32, incarnation: u32) {
        let entry = &mut self.slots[slot as usize];
        *entry = Slot {
            kind: SLOT_QUARANTINED,
            incarnation,
            used: 0,
            ..Slot::FREE
        };
        self.quarantined += 1;
        self.sync_used(slot);
    }

    fn claim_specific(&mut self, slot: u32) {
        if self.slots[slot as usize].kind == KIND_FREE {
            self.free_count -= 1;
        }
    }

    /// State of one slot, or `None` when out of range.
    pub fn slot_state(&self, slot: u32) -> Option<SlotState> {
        let entry = self.slots.get(slot as usize)?;
        let kind = match entry.kind {
            SLOT_NODE => SlotKind::Node,
            SLOT_DATA => SlotKind::Data {
                tier: entry.tier(),
                class: entry.class,
            },
            SLOT_QUARANTINED => SlotKind::Quarantined,
            _ => SlotKind::Free,
        };
        Some(SlotState {
            kind,
            used: entry.used.into(),
            reserved: entry.reserved.into(),
            open: entry.flags & OPEN != 0 || (entry.kind == SLOT_NODE && self.node_open == slot),
            referenced: entry.flags & REFERENCED != 0,
            evicting: entry.flags & EVICTING != 0,
            leases: self.leases[slot as usize].get(),
            retain_until: entry.retain_until,
            incarnation: entry.incarnation,
        })
    }

    /// Occupancy summary. O(slots).
    pub fn usage(&self) -> Usage {
        let mut node_units = 0;
        let mut records = 0;
        for entry in &self.slots {
            if entry.kind == SLOT_NODE {
                node_units += u64::from(entry.used);
            } else if entry.is_data() {
                records += u64::from(entry.used - entry.reserved);
            }
        }
        Usage {
            slot_count: self.slot_count(),
            free_slots: self.free_count,
            node_slots: self.node_slots.saturating_sub(1),
            cache_slots: self.cache_slots,
            storage_slots: self.storage_slots,
            quarantined_slots: self.quarantined,
            small_bytes: self.small_bytes,
            pending_units: self.pending,
            node_units,
            records,
        }
    }

    /// Free slots.
    pub fn free_slots(&self) -> u64 {
        self.free_count
    }

    fn evicting(&self, slot: u32) -> Result<()> {
        match self.slots.get(slot as usize) {
            Some(entry) if entry.flags & EVICTING != 0 => Ok(()),
            Some(_) => Err(Error::Stale),
            None => Err(Error::InvalidConfiguration),
        }
    }

    // Free one record of a data slot and refresh the slot.
    fn release_unit(&mut self, slot: u32, unit: u16) {
        let entry = self.slots[slot as usize];
        if entry.aux != NONE {
            let record = u32::from(unit) / class_units(entry.class).expect("known class");
            if record < entry.capacity() {
                let bitmap = self.bitmaps[entry.aux as usize].as_mut().expect("bitmap");
                set_bit(&mut bitmap.words, record, false);
            }
        }
        self.small_bytes -= record_bytes(entry.class);
        self.slots[slot as usize].used -= 1;
        self.sync_used(slot);
        self.refresh(slot);
    }

    fn set_open(&mut self, index: usize, slot: u32) {
        let previous = self.open[index];
        self.open[index] = slot;
        if previous != NONE && previous != slot {
            self.slots[previous as usize].flags &= !OPEN;
            // A closed slot keeps no bitmap.
            self.drop_bitmap(previous);
            self.refresh(previous);
        }
        if slot != NONE {
            self.slots[slot as usize].flags |= OPEN;
            self.partial[index].remove(&slot);
            if self.slots[slot as usize].aux == NONE
                && self.slots[slot as usize].class != WHOLE_SLOT_CLASS
            {
                let aux = self.new_bitmap(Box::new([0; BITMAP_WORDS]));
                self.slots[slot as usize].aux = aux;
            }
        }
    }

    fn drop_bitmap(&mut self, slot: u32) {
        let entry = &mut self.slots[slot as usize];
        if entry.is_data() && entry.aux != NONE {
            let aux = entry.aux;
            entry.aux = NONE;
            self.bitmaps[aux as usize] = None;
            self.bitmaps_free.push(aux);
        }
    }

    // Recompute partial sets, open state, and freeing after a change.
    fn refresh(&mut self, slot: u32) {
        let entry = self.slots[slot as usize];
        match entry.kind {
            SLOT_NODE => {
                if slot != META_SLOT && entry.used == first_page_unit(slot) as u16 {
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
            SLOT_DATA => {
                if entry.flags & EVICTING != 0 {
                    return;
                }
                let index = tier_class(entry.tier(), entry.class);
                if entry.used == 0 {
                    self.make_free(slot);
                } else if u32::from(entry.used) < entry.capacity() {
                    if entry.flags & OPEN == 0 {
                        self.partial[index].insert(slot);
                    }
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

    fn new_bitmap(&mut self, words: Box<[u64; BITMAP_WORDS]>) -> u32 {
        let bitmap = Some(Bitmap { words, seq: 0 });
        if let Some(index) = self.bitmaps_free.pop() {
            self.bitmaps[index as usize] = bitmap;
            index
        } else {
            self.bitmaps.push(bitmap);
            (self.bitmaps.len() - 1) as u32
        }
    }

    fn init_node(&mut self, slot: u32, incarnation: u32) {
        let mut words = Box::new([0u64; BITMAP_WORDS]);
        let first = u32::from(first_page_unit(slot));
        for bit in 0..first {
            set_bit(&mut words, bit, true);
        }
        let aux = self.new_bitmap(words);
        let entry = &mut self.slots[slot as usize];
        *entry = Slot {
            kind: SLOT_NODE,
            aux,
            incarnation,
            used: first as u16,
            ..Slot::FREE
        };
        self.node_slots += 1;
        self.dirty_nodes.insert(slot);
    }

    fn init_data(&mut self, slot: u32, tier: Tier, class: u8, incarnation: u32) {
        let entry = &mut self.slots[slot as usize];
        *entry = Slot {
            kind: SLOT_DATA,
            tier: tier as u8,
            class,
            incarnation,
            ..Slot::FREE
        };
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
            SLOT_NODE => {
                self.node_slots -= 1;
                self.node_partial.remove(&slot);
                self.dirty_nodes.remove(&slot);
                if self.node_open == slot {
                    self.node_open = NONE;
                }
                if entry.aux != NONE {
                    self.bitmaps[entry.aux as usize] = None;
                    self.bitmaps_free.push(entry.aux);
                }
            }
            SLOT_DATA => {
                match entry.tier() {
                    Tier::Cache => self.cache_slots -= 1,
                    Tier::Storage => self.storage_slots -= 1,
                }
                let index = tier_class(entry.tier(), entry.class);
                self.partial[index].remove(&slot);
                if self.open[index] == slot {
                    self.open[index] = NONE;
                }
                self.drop_bitmap(slot);
            }
            SLOT_QUARANTINED => self.quarantined -= 1,
            _ => return,
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
        self.queue(
            slot,
            SlotOp {
                ident: Some(Ident::Free),
                ..SlotOp::default()
            },
        );
        self.sync_used(slot);
    }

    // Bytes in use in one slot.
    fn slot_used_bytes(&self, slot: u32) -> u64 {
        let entry = &self.slots[slot as usize];
        let units = match entry.kind {
            SLOT_NODE => 1,
            SLOT_DATA => class_units(entry.class).unwrap_or(0),
            SLOT_QUARANTINED => return super::format::SLOT_BYTES,
            _ => 0,
        };
        u64::from(entry.used) * u64::from(units) * UNIT_BYTES as u64
    }

    fn used_prefix(&self, end: u32) -> u64 {
        let mut index = (end as usize).min(self.used.len());
        let mut sum = 0;
        while index > 0 {
            sum += self.used[index - 1];
            index &= index - 1;
        }
        sum
    }

    fn sync_used(&mut self, slot: u32) {
        let new = self.slot_used_bytes(slot);
        let old = self.used_prefix(slot + 1) - self.used_prefix(slot);
        if new == old {
            return;
        }
        let mut index = slot as usize + 1;
        while index <= self.used.len() {
            self.used[index - 1] = self.used[index - 1].wrapping_add(new.wrapping_sub(old));
            index += index & index.wrapping_neg();
        }
    }

    /// Bytes in use by node units and records in slots `range`. O(log slots).
    pub fn used_bytes(&self, range: std::ops::Range<u32>) -> u64 {
        if range.start >= range.end {
            return 0;
        }
        self.used_prefix(range.end) - self.used_prefix(range.start)
    }
}

#[cfg(test)]
#[path = "table_tests.rs"]
mod tests;
