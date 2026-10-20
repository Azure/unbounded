//! Mount, format, and scrub.
//!
//! A healthy mount reads both superblocks, the slot table (O(slots)), the
//! node-slot bitmaps, and the retired log of the newest valid superblock. It
//! does not read the index or the record map: lookups fault their pages in
//! on demand. See the "Mount" section of `designs/racer-extent-volume.md`.

use std::future::Future;
use std::pin::Pin;

use uring_runtime::{Budget, Operation, Scope, reactor::Reactor};

use super::btree::{self, Seek, TreeRoot};
use super::format::{
    Key, MAX_SLOTS, META_SLOT, MIN_SLOTS, NodeAddr, SLOT_BYTES, SUPERBLOCK_A_UNIT,
    SUPERBLOCK_B_UNIT, UNIT_BYTES, Value, bitmap_unit, unit_offset,
};
use super::page::{
    BITMAP_WORDS, Retired, SLOT_NODE, SlotSummary, TreeKind, decode_bitmap, decode_log, slot_key,
    slot_key_slot,
};
use super::pager::Priority;
use super::store::{
    Durable, GENERATION_SKIP, IO_CONCURRENCY, IndexOp, MountReport, ScrubReport, Volume,
    VolumeOptions, bounded,
};
use super::superblock::Superblock;
use super::table::{SlotKind, Slots};
use crate::{Charge, Error, Slab};

/// A root set whose allocation metadata was read.
struct Loaded {
    superblock: Superblock,
    unit: u16,
    summaries: Vec<(u32, SlotSummary)>,
    lost: Vec<(Key, Option<Key>)>,
    log: Vec<Retired>,
    log_pages: Vec<NodeAddr>,
    log_ok: bool,
    /// Per node slot: its bitmap if valid, and whether one was expected.
    bitmaps: Vec<(u32, Option<(Box<[u64; BITMAP_WORDS]>, u8, u64)>, bool)>,
}

impl Loaded {
    fn healthy(&self) -> bool {
        self.lost.is_empty() && self.log_ok
    }
}

impl<C: Charge> Volume<C> {
    /// Mount the volume on `slab`, or format it.
    ///
    /// The slab must use `SLOT_BYTES` segments and hold `MIN_SLOTS` to
    /// `MAX_SLOTS` of them; otherwise this fails with `InvalidConfiguration`.
    /// Its direct I/O alignment must divide 4096, or this fails with
    /// `Unsupported`. The slab is bound with [`Slab::open_volume`].
    ///
    /// Both superblocks are read. The newest valid one with this geometry is
    /// mounted if its slot table and retired log are readable; otherwise the
    /// other one is. A node slot whose bitmap copies are both invalid gets its
    /// bitmap rebuilt by walking the inner pages of the trees. A superblock
    /// read error fails the mount rather than formatting over it.
    ///
    /// Mount writes nothing unless it formats, and reads no index or record
    /// map leaves.
    pub fn mount<'a, S: Scope, B: Budget>(
        reactor: &'a Reactor<S, B>,
        slab: std::rc::Rc<Slab<C>>,
        options: VolumeOptions<C>,
        scope: &'a S,
    ) -> Operation<'a, (std::rc::Rc<Self>, MountReport), S::Error>
    where
        S::Error: From<Error>,
    {
        Box::pin(async move {
            if slab.segment_bytes() != SLOT_BYTES {
                return Err(Error::InvalidConfiguration.into());
            }
            let slot_count = slab.capacity_bytes() / SLOT_BYTES;
            if !(MIN_SLOTS..=MAX_SLOTS).contains(&slot_count) {
                return Err(Error::InvalidConfiguration.into());
            }
            let alignment = slab.open_volume(slot_count)?;
            if !(UNIT_BYTES as u64).is_multiple_of(alignment.offset())
                || !UNIT_BYTES.is_multiple_of(alignment.length())
                || alignment.memory() > UNIT_BYTES
            {
                return Err(Error::Unsupported.into());
            }
            let volume = Self::new_unmounted(slab, alignment, slot_count, &options)?;
            let report = volume.load(reactor, scope, &options).await?;
            Ok((std::rc::Rc::new(volume), report))
        })
    }

    async fn load<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
        options: &VolumeOptions<C>,
    ) -> std::result::Result<MountReport, S::Error>
    where
        S::Error: From<Error>,
    {
        let reads_before = self.pager.stats().reads;
        let mut report = MountReport::default();
        let mut candidates = Vec::new();
        for unit in [SUPERBLOCK_A_UNIT, SUPERBLOCK_B_UNIT] {
            let bytes = self.read_unit(reactor, unit_offset(0, unit), scope).await?;
            report.pages_read += 1;
            match Superblock::decode(&bytes).filter(|sb| {
                sb.has_current_geometry() && (MIN_SLOTS..=self.slot_count).contains(&sb.slot_count)
            }) {
                Some(sb) => candidates.push((sb, unit)),
                None => report.invalid_superblocks += 1,
            }
        }
        // Newest first; A wins a tie.
        candidates.sort_by_key(|(sb, _)| std::cmp::Reverse(sb.seq));
        if candidates.len() == 2 && candidates[0].0.volume_id != candidates[1].0.volume_id {
            candidates.pop();
            report.invalid_superblocks += 1;
        }
        if candidates.is_empty() {
            if !options.format {
                return Err(Error::Corrupt.into());
            }
            self.format(reactor, scope).await?;
            report.formatted = true;
            report.seq = 1;
            report.superblock_unit = SUPERBLOCK_A_UNIT;
            return Ok(report);
        }
        let highest_seq = candidates[0].0.seq;
        self.volume_id.set(candidates[0].0.volume_id);
        let mut chosen = None;
        let mut first = None;
        for (index, (superblock, unit)) in candidates.iter().enumerate() {
            if superblock.volume_id != self.volume_id.get() {
                continue;
            }
            let loaded = self
                .load_root_set(reactor, scope, superblock, *unit)
                .await?;
            if loaded.healthy() {
                report.fell_back = index > 0;
                chosen = Some(loaded);
                break;
            }
            if first.is_none() {
                first = Some(loaded);
            }
        }
        let loaded = match chosen {
            Some(loaded) => loaded,
            None => {
                report.root_lost = true;
                first.expect("one candidate was loaded")
            }
        };
        report.seq = loaded.superblock.seq;
        report.superblock_unit = loaded.unit;
        report.lost = loaded.lost.clone();
        report.entries = loaded.superblock.trees[TreeKind::Index.index()].count;
        report.pending_nodes = loaded.log.iter().filter(|r| !r.record).count() as u64;
        report.pending_records = loaded.log.iter().filter(|r| r.record).count() as u64;
        self.install(reactor, scope, options, loaded, highest_seq, &mut report)
            .await?;
        report.pages_read += self.pager.stats().reads - reads_before;
        report.invalid_copies = self.shared.state().invalid_copies;
        report.dirty = report.root_lost || report.rebuilt_bitmaps != 0 || !report.lost.is_empty();
        Ok(report)
    }

    /// Read one root set's slot table, retired log, and node bitmaps.
    async fn load_root_set<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
        superblock: &Superblock,
        unit: u16,
    ) -> std::result::Result<Loaded, S::Error>
    where
        S::Error: From<Error>,
    {
        let io = self.io(reactor, scope);
        let seq = superblock.seq;
        // The slot table, leaf by leaf.
        let mut summaries = Vec::new();
        let mut lost = Vec::new();
        let tree = superblock.trees[TreeKind::Slots.index()];
        let mut at = Key::MIN;
        loop {
            let high =
                match btree::seek(&io, &tree, TreeKind::Slots, seq, &at, Priority::Scan).await? {
                    Seek::Empty => None,
                    Seek::Lost(low, high) => {
                        lost.push((low, high));
                        high
                    }
                    Seek::Leaf(page, high) => {
                        for i in 0..page.count() {
                            let key = page.key(i);
                            if key >= at
                                && let Some(summary) = SlotSummary::decode(page.value(i))
                                && u64::from(slot_key_slot(&key)) < self.slot_count
                            {
                                summaries.push((slot_key_slot(&key), summary));
                            }
                        }
                        high
                    }
                };
            match high {
                Some(high) => at = high,
                None => break,
            }
        }
        // The retired log.
        let mut log = Vec::new();
        let mut log_pages = Vec::new();
        let mut log_ok = true;
        let mut next = superblock.log_head;
        while !next.is_none() && log_ok {
            if !next.valid_for(self.slot_count) || log_pages.contains(&next) {
                log_ok = false;
                break;
            }
            let bytes = self.read_unit(reactor, next.offset(), scope).await?;
            match decode_log(
                &bytes[..],
                &superblock.volume_id,
                seq,
                next,
                self.slot_count,
            ) {
                Some((entries, following)) => {
                    log.extend(entries);
                    log_pages.push(next);
                    next = following;
                }
                None => log_ok = false,
            }
        }
        if log.len() as u64 != superblock.log_entries {
            log_ok = false;
        }
        // Node bitmaps: the newest valid copy no newer than the superblock.
        let mut nodes: std::collections::BTreeMap<u32, (u8, u64)> = summaries
            .iter()
            .filter(|(_, s)| s.kind == SLOT_NODE)
            .map(|(slot, s)| (*slot, (s.copy, s.bitmap_seq)))
            .collect();
        nodes.entry(META_SLOT).or_insert((0, 0));
        let volume_id = superblock.volume_id;
        let results = bounded(
            nodes.into_iter().map(|(slot, (copy, expected))| {
                Box::pin(async move {
                    if expected == 0 {
                        // Never written (a fresh format): rebuilt, not lost.
                        return Ok((slot, None, false));
                    }
                    // Only the copy the slot table names, written by the
                    // commit it names: an older valid copy may be stale.
                    let addr = NodeAddr::new(slot, bitmap_unit(slot, copy));
                    let bytes = self.read_unit(reactor, addr.offset(), scope).await?;
                    let found = decode_bitmap(&bytes[..], &volume_id, seq, addr, slot)
                        .filter(|(_, found)| *found == expected)
                        .map(|(words, found)| (words, copy, found));
                    Ok((slot, found, true))
                })
                    as Pin<Box<dyn Future<Output = std::result::Result<_, S::Error>> + '_>>
            }),
            IO_CONCURRENCY,
        )
        .await;
        let mut bitmaps = Vec::new();
        for result in results {
            bitmaps.push(result?);
        }
        Ok(Loaded {
            superblock: *superblock,
            unit,
            summaries,
            lost,
            log,
            log_pages,
            log_ok,
            bitmaps,
        })
    }

    /// Build the in-memory state from a loaded root set.
    async fn install<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
        options: &VolumeOptions<C>,
        loaded: Loaded,
        highest_seq: u64,
        report: &mut MountReport,
    ) -> std::result::Result<(), S::Error>
    where
        S::Error: From<Error>,
    {
        let Loaded {
            superblock,
            unit,
            summaries,
            lost,
            log,
            log_pages,
            log_ok,
            bitmaps,
        } = loaded;
        let mut slots = Slots::mount(
            self.slot_count,
            options.limits,
            summaries,
            superblock.next_incarnation,
        )?;
        let mut missing = Vec::new();
        let mut damaged = 0;
        for (slot, bitmap, expected) in bitmaps {
            match bitmap {
                Some((words, copy, seq)) => slots.set_node_bitmap(slot, &words, copy, seq),
                None => {
                    missing.push(slot);
                    damaged += u64::from(expected);
                }
            }
        }
        // Slots whose summary is unreadable are never reused.
        for (low, high) in &lost {
            let start = slot_key_slot(low).max(1);
            let end = high.map_or(self.slot_count as u32, |h| {
                let slot = slot_key_slot(&h);
                if *low < h && h == slot_key(slot) {
                    slot
                } else {
                    slot + 1
                }
            });
            for slot in start..end.min(self.slot_count as u32) {
                if matches!(slots.slot_state(slot).map(|s| s.kind), Some(SlotKind::Free)) {
                    slots.quarantine(slot);
                }
            }
        }
        if !missing.is_empty() && log_ok {
            // Rebuild the missing bitmaps from every page the root set
            // references. A lost inner page hides pages: keep those slots full.
            let io = self.io(reactor, scope);
            let mut referenced: Vec<NodeAddr> = log_pages.clone();
            referenced.extend(log.iter().filter(|r| !r.record).map(|r| r.addr));
            let mut walk_lost = false;
            for kind in TreeKind::ALL {
                let lost = btree::walk_inner(
                    &io,
                    &superblock.trees[kind.index()],
                    kind,
                    superblock.seq,
                    &mut |addr, _| referenced.push(addr),
                )
                .await?;
                walk_lost |= !lost.is_empty();
            }
            if !walk_lost {
                for slot in &missing {
                    slots.mark_node_unit(NodeAddr::new(*slot, u16::MAX), true);
                }
                for addr in referenced {
                    if missing.contains(&addr.slot()) {
                        slots.mark_node_unit(addr, false);
                    }
                }
                report.rebuilt_bitmaps = damaged;
            }
        }
        let pending = log.len() as u64;
        slots.set_pending(pending);
        let mut state = self.shared.state();
        state.slots = slots;
        state.durable = Durable {
            seq: superblock.seq,
            trees: superblock.trees,
        };
        state.epoch += 1;
        state.log_real = !log.is_empty();
        state.log = log;
        state.log_pages = log_pages;
        state.next_generation = superblock.next_generation.saturating_add(GENERATION_SKIP);
        // Always dirty: the first commit persists the generation skip.
        state.mutations = 1;
        drop(state);
        let mut commit = self.commit.borrow_mut();
        commit.seq = highest_seq;
        commit.durable_unit = unit;
        Ok(())
    }

    async fn format<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
    ) -> std::result::Result<(), S::Error>
    where
        S::Error: From<Error>,
    {
        let mut superblock = Superblock::empty(self.volume_id.get(), 1, self.slot_count);
        superblock.next_incarnation = self.shared.state().slots.next_incarnation();
        self.write_superblock(reactor, scope, SUPERBLOCK_A_UNIT, &superblock)
            .await?;
        self.slab.sync(reactor, scope).await?;
        let mut state = self.shared.state();
        state.durable = Durable {
            seq: 1,
            trees: [TreeRoot::EMPTY; 3],
        };
        drop(state);
        let mut commit = self.commit.borrow_mut();
        commit.seq = 1;
        commit.durable_unit = SUPERBLOCK_A_UNIT;
        Ok(())
    }

    /// Walk every index and record map page with bounded memory, one leaf at
    /// a time. Reports lost ranges (and remembers them for
    /// [`Volume::lost_ranges`]), and removes index entries that are malformed
    /// or do not lie in a data slot of their tier and class. A record map
    /// range that is lost quarantines its slots: their records can no longer
    /// be accounted for.
    pub fn scrub<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        scope: &'a S,
    ) -> Operation<'a, ScrubReport, S::Error>
    where
        S::Error: From<Error>,
    {
        Box::pin(async move {
            let reads_before = self.pager.stats().reads;
            let copies_before = self.shared.state().invalid_copies;
            let start = self.shared.state().durable;
            let mut restarted = false;
            let mut report = ScrubReport::default();
            for kind in [TreeKind::Index, TreeKind::Records] {
                let mut at = Key::MIN;
                loop {
                    let durable = self.shared.state().durable;
                    let io = self.io(reactor, scope);
                    let seek = btree::seek(
                        &io,
                        &durable.trees[kind.index()],
                        kind,
                        durable.seq,
                        &at,
                        Priority::Scan,
                    )
                    .await?;
                    if self.shared.state().durable.seq != durable.seq {
                        // A commit published new roots; continue on them.
                        restarted = true;
                        continue;
                    }
                    let high = match seek {
                        Seek::Empty => None,
                        Seek::Lost(low, high) => {
                            self.shared.state().note_lost(kind, low, high);
                            match kind {
                                TreeKind::Index => report.lost.push((low, high)),
                                _ => report.lost_records.push((low, high)),
                            }
                            high
                        }
                        Seek::Leaf(page, high) => {
                            if kind == TreeKind::Index {
                                for i in 0..page.count() {
                                    let key = page.key(i);
                                    if key < at {
                                        continue;
                                    }
                                    report.entries += 1;
                                    let value = Value::decode(
                                        page.value(i).try_into().expect("value bytes"),
                                    );
                                    self.check_entry(key, value, &mut report);
                                }
                            }
                            high
                        }
                    };
                    match high {
                        Some(high) => at = high,
                        None => break,
                    }
                }
            }
            {
                let mut state = self.shared.state();
                for (low, high) in report.lost_records.clone() {
                    let (start, _) = super::page::record_key_parts(&low);
                    let end = high.map_or(self.slot_count as u32 - 1, |h| {
                        super::page::record_key_parts(&h).0
                    });
                    for slot in start.max(1)..=end.min(self.slot_count as u32 - 1) {
                        if matches!(
                            state.slots.slot_state(slot).map(|s| s.kind),
                            Some(SlotKind::Data { .. })
                        ) {
                            state.slots.quarantine(slot);
                        }
                    }
                }
            }
            {
                // Entries of lost leaves were never subtracted from the
                // durable count; correct it when the walk saw one root set.
                let mut state = self.shared.state();
                if !restarted && state.durable.seq == start.seq {
                    let visited = report.entries as i64;
                    state.count_fix = visited - start.trees[TreeKind::Index.index()].count as i64;
                }
            }
            report.pages_read = self.pager.stats().reads - reads_before;
            report.invalid_copies = self.shared.state().invalid_copies - copies_before;
            Ok(report)
        })
    }

    /// Drop an index entry found by scrub that cannot be right.
    fn check_entry(&self, key: Key, value: Option<Value>, report: &mut ScrubReport) {
        let mut state = self.shared.state();
        if state.overlay_get(&key).is_some() {
            return;
        }
        let ok = value.is_some_and(|v| {
            u64::from(v.slot) < self.slot_count
                && state.slots.slot_state(v.slot).is_some_and(|s| {
                    s.kind
                        == SlotKind::Data {
                            tier: v.tier,
                            class: v.class,
                        }
                        || s.kind == SlotKind::Quarantined
                })
        });
        if !ok && let Some(value) = value {
            state.set(key, IndexOp::Delete, Some(Some(value)));
            report.dropped.push((key, value));
        }
    }
}
